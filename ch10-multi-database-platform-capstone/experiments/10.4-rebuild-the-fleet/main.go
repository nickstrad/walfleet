// Experiment 10.4 implements Rebuild Ladder rung R7 (Fleet): it proves that
// the S3 store plus the control-plane catalog is a complete saved state of a
// multi-database fleet, and that every published restore point is internally
// consistent.
//
// Eight writer node processes own equal shares of the fleet (default 500
// databases; -databases 2000 for the outline's full-scale run), each keeping
// at most -capacity databases open (default 32). Every database is claimed and
// driven store-durable through recipe steps 1-4, then a schema rollout (recipe
// step 5, CREATE INDEX events_by_account) is applied to half of each writer's
// share and stopped. Meanwhile a separate checker process lists the store and
// rebuilds every published restore point in memory against the recipe step
// hashes. After quiescing (closing every writer, where sqlite3WalClose at
// src/wal.c:2507-2571 leaves 0 frames for TRUNCATE) and deleting every local
// file, 8 rebuild workers claim and restore the entire fleet from SeaweedFS S3
// alone, verifying E1 byte equality, catalog schema versions, exact double-read
// object counts, and standing descriptor and memory bounds throughout.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.4
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/procstat"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const stepsBeforeRollout = recipe.IndexStep - 1

var (
	databases = flag.Int("databases", 500, "databases in the fleet (the outline asks for 2000; see the notes for that run)")
	writers   = flag.Int("writers", 8, "writer node processes; each owns an equal share of the fleet")
	workers   = flag.Int("checker-workers", 4, "databases the checker inspects at once")
	pause     = flag.Duration("checker-pause", time.Second, "pause between the checker's passes while the writers run")
	role      = flag.String("role", "", "internal: run as a service node or as the checker")
	process   node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-4", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 32, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: unused here")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.4" runs both.
// part labels the claims made while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, writers, databases, rebuild, checkerPasses, checkerFailures, problems, infoRun, infoTimings, infoWriters *results.Table
	}
	settingSeq int
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.writers = set.Table("writers", "writer", "databases", "rolled_out")
	tables.databases = set.Table("databases", "database", "writer", "acknowledged_version", "catalog_schema_version", "last_shipped", "quiesce_wal_frames", "rebuilt_user_version", "rebuilt_state", "byte_identical")
	tables.rebuild = set.Table("rebuild", "databases", "workers", "local_files_left", "objects_read", "listings", "fence_objects_written")
	tables.checkerPasses = set.Table("checker_passes", "pass", "databases", "restore_points", "failed")
	tables.checkerFailures = set.Table("checker_failures", "phase", "line")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.infoRun = set.Table("info_run", "name", "value")
	tables.infoTimings = set.Table("info_timings", "seq", "phase", "ms")
	tables.infoWriters = set.Table("info_writers", "writer", "reply", "summary")
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

var timingSeq int

func timing(phase string, elapsed time.Duration) {
	timingSeq++
	tables.infoTimings.Row(timingSeq, phase, elapsed.Milliseconds())
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type fleet struct {
	env           *lab.Env
	ids           []string
	shares        [][]string
	writers       []*node.Process
	mu            sync.Mutex
	acks          map[string]int
	rebuildCounts *lab.CountingStore
}

func (f *fleet) send(p *node.Process, command string) node.Reply {
	reply, err := p.Send(command)
	if err != nil {
		fatal("send "+command, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.env.Tracker.Note(p.Name, reply)
	if strings.HasPrefix(reply.Line, "ok write ") {
		id, ack, err := node.ParseAck(reply.Line)
		if err != nil {
			fatal("parse acknowledgement", err)
		}
		f.acks[id] = max(f.acks[id], ack.UserVersion)
	} else if !strings.HasPrefix(reply.Line, "ok ") {
		f.env.Tracker.Problems = append(f.env.Tracker.Problems, p.Name+": "+reply.Line)
	}
	return reply
}

func (f *fleet) eachWriter(work func(w int, p *node.Process, share []string)) {
	var wg sync.WaitGroup
	for w, p := range f.writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(w, p, f.shares[w])
		}()
	}
	wg.Wait()
}

func (f *fleet) writeStream() {
	f.eachWriter(func(_ int, p *node.Process, share []string) {
		for start := 0; start < len(share); start += process.Capacity {
			batch := share[start:min(start+process.Capacity, len(share))]
			for _, id := range batch {
				f.send(p, "claim "+id)
			}
			for range stepsBeforeRollout {
				for _, id := range batch {
					f.send(p, fmt.Sprintf("write %s %s", id, node.StoreDurable))
				}
			}
		}
	})
}

func (f *fleet) rollout() {
	f.eachWriter(func(_ int, p *node.Process, share []string) {
		for _, id := range share[:len(share)/2] {
			f.send(p, fmt.Sprintf("write %s %s", id, node.StoreDurable))
		}
	})
}

func (f *fleet) runChecker(checker *node.Process, stop <-chan struct{}) (passes, points int, failed []string) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(*pause):
		}
		reply, err := checker.Send("pass")
		if err != nil {
			fatal("checker", err)
		}
		pass, err := lab.ParsePass(reply)
		if err != nil {
			fatal("checker pass", err)
		}
		f.mu.Lock()
		f.env.Tracker.Note("checker", node.Reply{Line: reply.Line, Bounds: reply.Bounds})
		f.mu.Unlock()
		passes++
		points += pass.RestorePoints
		failed = append(failed, pass.FailedLines...)
	}
}

type recorded struct {
	sha256        string
	schemaVersion int
	lastShipped   int64
	frames        int
}

func (f *fleet) quiesce(cat *catalog.Catalog) map[string]recorded {
	for _, p := range f.writers {
		reply, err := f.env.Stop(p)
		if err != nil {
			fatal("close "+p.Name, err)
		}
		tables.infoWriters.Row(p.Name, reply.Line, reply.Summary)
	}
	records, err := cat.All(f.env.Ctx)
	if err != nil {
		fatal("catalog", err)
	}
	out := map[string]recorded{}
	frames := 0
	for _, record := range records {
		quiesced, err := lab.TruncateAndHash(f.env.Ctx, f.env.DatabasePath(record.Owner, record.ID))
		if err != nil {
			fatal("quiesce "+record.ID, err)
		}
		frames += quiesced.Checkpoint.LogFrames
		out[record.ID] = recorded{sha256: quiesced.SHA256, schemaVersion: record.SchemaVersion, lastShipped: record.LastShipped, frames: quiesced.Checkpoint.LogFrames}
	}
	check(len(out) == len(f.ids) && frames == 0, "every database was at rest when quiesced: the writers' close left no WAL frame for the TRUNCATE checkpoint")
	return out
}

func (f *fleet) rebuild(cat *catalog.Catalog, before map[string]recorded) {
	for w := range f.writers {
		if err := os.RemoveAll(f.env.NodeDir(fmt.Sprintf("w%d", w+1))); err != nil {
			fatal("delete local files", err)
		}
	}
	left, err := filepath.Glob(filepath.Join(f.env.Work, "w*", "*"))
	if err != nil {
		fatal("glob", err)
	}
	check(len(left) == 0, "the new node starts with empty disks")
	counting := &lab.CountingStore{Store: f.env.Store}
	f.rebuildCounts = counting
	workload := recipe.Default()
	assertion, err := bounds.Start(bounds.Bounds{OpenDatabases: *writers, ConnectionsPerDatabase: node.ConnectionsPerDatabase, CacheBytes: bounds.CacheBytes(node.PlatformSettings().CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors})
	if err != nil {
		fatal("bounds", err)
	}
	rebuilders := make([]*node.Node, *writers)
	for w := range rebuilders {
		rebuilders[w], err = node.Start(node.Config{
			Name: fmt.Sprintf("fresh-%d", w+1), Dir: filepath.Join(f.env.Work, "fresh", fmt.Sprintf("%d", w+1)), Catalog: cat, Store: counting, Capacity: 1,
			File:   workload.SQLiteOptions().File,
			Create: func(ctx context.Context, path string) error { return fmt.Errorf("a rebuild never creates a database") },
		})
		if err != nil {
			fatal("start the new node", err)
		}
	}
	start := time.Now()
	paths := make(map[string]string, len(f.ids))
	var mu sync.Mutex
	var overBounds []string
	var wg sync.WaitGroup
	for w, share := range f.shares {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, id := range share {
				if _, err := rebuilders[w].Claim(f.env.Ctx, id); err != nil {
					fatal("rebuild "+id, err)
				}
				mu.Lock()
				paths[id] = f.env.DatabasePath(filepath.Join("fresh", fmt.Sprintf("%d", w+1)), id)
				if i%50 == 0 {
					if _, err := assertion.Check(); err != nil {
						overBounds = append(overBounds, err.Error())
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, rebuilder := range rebuilders {
		if err := rebuilder.Close(); err != nil {
			fatal("close the new node", err)
		}
	}
	if _, err := assertion.Check(); err != nil {
		overBounds = append(overBounds, err.Error())
	}
	gets, bytes, puts, lists := counting.Counts()
	timing("rebuild", elapsed)
	tables.infoRun.Row("rebuild standing assertion", assertion.Summary())
	tables.infoRun.Row("rebuild bytes read", bytes)
	tables.infoRun.Row("rebuild bytes read per database", bytes/int64(len(f.ids)))
	tables.rebuild.Row(len(f.ids), *writers, len(left), gets, lists, puts)
	equal, steps, schemaAgrees := 0, map[int]int{}, 0
	for i, id := range f.ids {
		image, err := os.ReadFile(paths[id])
		if err != nil {
			fatal("read the rebuilt "+id, err)
		}
		state, err := f.env.MatchImage(image)
		if err != nil {
			fatal("match", err)
		}
		sha, err := f.env.AtRestSHA256(filepath.Join("fresh", filepath.Base(filepath.Dir(paths[id]))), id)
		if err != nil {
			fatal("at rest", err)
		}
		if sha == before[id].sha256 {
			equal++
		}
		if state.RecipeState && state.UserVersion == before[id].schemaVersion && state.UserVersion == f.acks[id] {
			schemaAgrees++
		}
		steps[state.UserVersion]++
		tables.databases.Row(id, fmt.Sprintf("w%d", i%*writers+1), f.acks[id], before[id].schemaVersion, before[id].lastShipped, before[id].frames, state.UserVersion, state.Match, sha == before[id].sha256)
	}
	check(equal == len(f.ids), fmt.Sprintf("%d of %d rebuilt databases are byte-identical to the quiesced originals", equal, len(f.ids)))
	check(schemaAgrees == len(f.ids), "every rebuilt database is a recipe state whose schema version (user_version) is the one the catalog recorded and the last one acknowledged")
	check(steps[recipe.IndexStep]+steps[stepsBeforeRollout] == len(f.ids) && steps[recipe.IndexStep] > 0 && steps[stepsBeforeRollout] > 0, fmt.Sprintf("the rollout stopped halfway: databases are on the old schema (step %d) or the new one (step %d with %s), nothing else", stepsBeforeRollout, recipe.IndexStep, strings.TrimPrefix(recipe.IndexSQL, "CREATE ")))
	for _, line := range overBounds {
		problem("rebuild over bounds", line)
	}
	check(len(overBounds) == 0, "the rebuilding process stayed within the standing bounds")
}

var problemSeq int

func problem(kind, line string) {
	problemSeq++
	tables.problems.Row(problemSeq, kind, line)
}

func main() {
	flag.Parse()
	ctx := context.Background()
	switch *role {
	case "node":
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	case "checker":
		store, err := lab.Connect(ctx, process)
		if err == nil {
			err = lab.ServeChecker(ctx, store, *workers, os.Stdin, os.Stdout)
		}
		if err != nil {
			fatal("checker", err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *databases < 2**writers || *writers < 1 || process.Capacity < 1 {
		fatal("flags", fmt.Errorf("need -writers >= 1, -capacity >= 1 and at least two databases per writer"))
	}
	// Guard against hitting the process descriptor soft limit before launching
	// writers; each open database needs up to 3*3 descriptors plus 32 reserve.
	soft, _, err := procstat.FileLimit()
	if err != nil {
		fatal("file limit", err)
	}
	perNode := bounds.DescriptorsPerConnection*node.ConnectionsPerDatabase*process.Capacity + node.ReserveDescriptors
	if uint64(perNode) > soft {
		fatal("descriptor limit", fmt.Errorf("-capacity %d needs up to %d descriptors per writer, the soft limit is %d", process.Capacity, perNode, soft))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.4", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	start := time.Now()
	f := &fleet{env: env, ids: lab.Ids("db", *databases), acks: map[string]int{}}
	if err := env.Reset(f.ids); err != nil {
		fatal("reset", err)
	}
	// Open the tables after env.Reset, which empties env.Work.
	set = results.MustOpen(env.Work)
	openTables()
	tables.infoRun.Row("descriptor soft limit", soft)
	tables.infoRun.Row("descriptor bound of each writer", perNode)
	f.shares = make([][]string, *writers)
	for i, id := range f.ids {
		f.shares[i%*writers] = append(f.shares[i%*writers], id)
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", env.ShownWork())
	setting("databases", *databases)
	setting("writer_processes", *writers)
	setting("open_databases_per_writer", process.Capacity)
	setting("checker_processes", 1)
	setting("checker_workers", *workers)
	setting("checker_pause", *pause)
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	for w, share := range f.shares {
		tables.writers.Row(fmt.Sprintf("w%d", w+1), len(share), len(share)/2)
	}
	for w := range *writers {
		p, err := env.Start(env.Config(fmt.Sprintf("w%d", w+1), ""))
		if err != nil {
			fatal("start writer", err)
		}
		f.writers = append(f.writers, p)
	}
	checkerArgs := []string{"-role", "checker", "-bucket", process.Bucket, "-prefix", process.Prefix, "-checker-workers", fmt.Sprint(*workers)}
	checker, err := node.StartProcess(ctx, "checker", checkerArgs...)
	if err != nil {
		fatal("start checker", err)
	}
	stop := make(chan struct{})
	type checked struct {
		passes, points int
		failed         []string
	}
	done := make(chan checked)
	go func() {
		passes, points, failed := f.runChecker(checker, stop)
		done <- checked{passes, points, failed}
	}()
	part = "A"
	setting("steps_before_rollout", stepsBeforeRollout)
	setting("rollout_step", recipe.IndexStep)
	setting("rollout_sql", recipe.IndexSQL)
	phase := time.Now()
	f.writeStream()
	timing("write stream", time.Since(phase))
	phase = time.Now()
	f.rollout()
	timing("rollout", time.Since(phase))
	close(stop)
	phase = time.Now()
	concurrent := <-done
	timing("waiting for the last concurrent checker pass", time.Since(phase))
	tables.infoRun.Row("concurrent checker passes", concurrent.passes)
	tables.infoRun.Row("restore points the concurrent checker rebuilt", concurrent.points)
	for _, line := range concurrent.failed {
		tables.checkerFailures.Row("concurrent", line)
	}
	check(concurrent.passes > 0 && len(concurrent.failed) == 0, "every restore point the checker found while the writers ran rebuilt to a recipe state")
	part = "B"
	phase = time.Now()
	before := f.quiesce(env.Catalog)
	timing("quiesce", time.Since(phase))
	part = "C"
	f.rebuild(env.Catalog, before)
	part = "D"
	phase = time.Now()
	reply, err := checker.Send("pass")
	if err != nil {
		fatal("checker", err)
	}
	timing("final checker pass", time.Since(phase))
	env.Tracker.Note("checker", node.Reply{Line: reply.Line, Bounds: reply.Bounds})
	final, err := lab.ParsePass(reply)
	if err != nil {
		fatal("checker pass", err)
	}
	tables.checkerPasses.Row("final", final.Databases, final.RestorePoints, final.Failed)
	tables.infoRun.Row("final checker pass bytes read (the whole store)", final.BytesRead)
	for _, line := range final.FailedLines {
		tables.checkerFailures.Row("final", line)
	}
	check(final.Databases == len(f.ids) && final.Failed == 0, "after the rebuild every published restore point of every database rebuilds to a recipe state")
	// node.Claim reads every existing slot twice: once in adapter.Refresh to
	// check the lineage and once in walship.LoadCatalog to plan and restore.
	gets, _, _, _ := f.rebuildCounts.Counts()
	check(gets == 2*int64(final.RestorePoints-len(f.ids)), fmt.Sprintf("the rebuild read each of the %d slots it found exactly twice (once to check the lineage, once to restore)", final.RestorePoints-len(f.ids)))
	closing, err := env.Stop(checker)
	if err != nil {
		fatal("close checker", err)
	}
	tables.infoRun.Row("checker standing assertion", closing.Summary)
	part = "E"
	for _, line := range env.Tracker.Problems {
		problem("problem", line)
	}
	for _, line := range env.Tracker.OverBounds {
		problem("over bounds", line)
	}
	check(len(env.Tracker.Problems) == 0, "no node reported a problem and every command was acknowledged")
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every writer and checker command the descriptors and memory stayed within the standing bounds")
	tables.infoRun.Row("replies that carried a bounds check", env.Tracker.Replies)
	timing("total", time.Since(start))
	set.Finish("REBUILD THE FLEET")
}

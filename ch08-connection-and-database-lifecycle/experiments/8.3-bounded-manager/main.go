// Experiment 8.3 demonstrates Rebuild Ladder rung R5 (at-rest state): serving
// 1,000 WAL databases through an LRU-bounded connection manager capped at K
// open connections versus an unbounded run (Part A), and six concrete cases
// where a closed WAL database is not a single checkpointed file at rest or
// changes while closed (Part B).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 8.3. Each stream of Part A
// runs in a child process that reports its counts to the parent as one
// RESULT line of JSON; only the parent writes tables.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/procstat"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	uncountedPerConnection = 256 * 1024
	memoryConstant         = 32 << 20
	readQuery              = "SELECT count(*), coalesce(sum(balance), 0), (SELECT user_version FROM pragma_user_version) FROM accounts"
)

var (
	databases     = flag.Int("dbs", 1000, "database files")
	capacity      = flag.Int("k", 64, "connections the bounded manager keeps open")
	operations    = flag.Int("ops", 4000, "operations in the seeded stream")
	seed          = flag.Uint64("seed", 1, "seed of the operation stream")
	maxStep       = flag.Int("max-step", 4, "last recipe step a write may apply")
	writePercent  = flag.Int("write-percent", 20, "share of operations that apply the next recipe step")
	cacheSize     = flag.Int("cache-size", -256, "PRAGMA cache_size of every managed connection")
	sampleEvery   = flag.Int("sample-every", 10, "operations between descriptor samples")
	memoryEvery   = flag.Int("memory-every", 100, "operations between process-memory samples")
	hotPercent    = flag.Int("hot-percent", 50, "share of operations that go to the hot 5% of databases")
	modeFlag      = flag.String("mode", "", "internal: run one stream in this process (bounded or unbounded)")
	runDirFlag    = flag.String("dir", "", "internal: directory of the stream's database files")
	childRoleFlag = flag.String("role", "", "internal: child role for part B")
)

// Every measurement goes to a CSV table in <work dir>/results/. part labels
// the claims written while a part runs; caseID labels the part B observations.
var (
	set    *results.Set
	part   string
	caseID string
	tables struct {
		setup, streams, uncleanEvictions, observations, problems, infoStreams, infoLimits *results.Table
	}
	setupSeq, observationSeq, problemSeq int
)

func openTables() {
	tables.setup = set.Table("setup", "seq", "name", "value")
	tables.streams = set.Table("streams", "mode", "capacity", "operations", "reads", "writes", "distinct_dbs",
		"hits", "misses", "evictions", "evicted_at_rest", "evicted_not_at_rest", "verified_reopens", "refused_reopens", "first_opens", "peak_open",
		"fd_samples", "peak_fds", "fd_mismatches", "fds_after_close", "read_mismatches", "files_exact", "files_masked", "files_wrong",
		"warm_reads", "cold_reads", "memory_budget")
	tables.uncleanEvictions = set.Table("unclean_evictions", "mode", "status")
	tables.observations = set.Table("observations", "seq", "case_id", "moment", "file", "at_rest", "bytes", "sha256", "recipe_step", "exact", "note")
	tables.problems = set.Table("problems", "seq", "observation", "problem", "why")
	tables.infoStreams = set.Table("info_streams", "mode", "peak_rss", "warm_ns", "cold_ns", "elapsed_ms")
	tables.infoLimits = set.Table("info_limits", "soft", "hard")
}

func setup(name string, value any) {
	setupSeq++
	tables.setup.Row(setupSeq, name, value)
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

var stepHashes []recipe.FileHashes

// matchStep compares a closed database file against the recipe's cached step
// hashes (computed once per process so matching 1,000 files stays sub-second)
// and accepts change-counter-only differences when header bytes 24-27 match
// bytes 92-95 (multiple steps applied on one WAL connection).
func matchStep(ctx context.Context, path string) (recipe.Match, error) {
	if stepHashes == nil {
		hashes, err := theRecipe().StepHashes(ctx)
		if err != nil {
			return recipe.Match{Step: -1}, err
		}
		stepHashes = hashes
	}
	if err := recipe.CheckAtRest(path); err != nil {
		return recipe.Match{Step: -1}, err
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return recipe.Match{Step: -1}, err
	}
	match := recipe.MatchHashes(stepHashes, recipe.HashImage(image))
	if match.Found() && !match.Exact && !recipe.ChangeCountersAgree(image) {
		return recipe.Match{Step: -1}, nil
	}
	return match, nil
}

func theRecipe() recipe.Recipe { return recipe.Default().WithSteps(*maxStep) }

func openOptions() sqlitex.Options {
	options := theRecipe().SQLiteOptions()
	options.Conn.CacheSize = *cacheSize
	return options
}

func openManaged(ctx context.Context, path string) (*sqlitex.DB, error) {
	return sqlitex.Open(ctx, path, openOptions())
}

func cacheBudgetBytes() int64 {
	if *cacheSize < 0 {
		return int64(-*cacheSize) * 1024
	}
	return int64(*cacheSize) * 4096
}

func dbID(i int) string { return fmt.Sprintf("db-%04d", i) }

func pathFor(dir string) func(string) (string, error) {
	return func(id string) (string, error) {
		var n int
		if _, err := fmt.Sscanf(id, "db-%04d", &n); err != nil || dbID(n) != id || n < 0 || n >= *databases {
			return "", fmt.Errorf("unknown database id %q", id)
		}
		return filepath.Join(dir, id+".db"), nil
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func populate(master, dir string, n int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	for i := range n {
		if err := copyFile(master, filepath.Join(dir, dbID(i)+".db")); err != nil {
			fatal("copy", err)
		}
	}
}

type operation struct {
	db    int
	write bool
}

func stream() []operation {
	rng := rand.New(rand.NewPCG(*seed, 83))
	hot := max(1, *databases/20)
	steps := make([]int, *databases)
	ops := make([]operation, *operations)
	for i := range ops {
		db := rng.IntN(*databases)
		if rng.IntN(100) < *hotPercent {
			db = rng.IntN(hot)
		}
		write := rng.IntN(100) < *writePercent && steps[db] < *maxStep
		if write {
			steps[db]++
		}
		ops[i] = operation{db: db, write: write}
	}
	return ops
}

func descriptorCount() int {
	n, err := procstat.DescriptorCount()
	if err != nil {
		fatal("descriptors", err)
	}
	return n
}

func settledRSS() int64 {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := procstat.ReadMemory()
	if err != nil {
		fatal("memory", err)
	}
	return m.RSS
}

// streamResult is what a stream child reports to its parent.
type streamResult struct {
	Mode             string
	Capacity         int
	Reads, Writes    int
	Stats            connmgr.Stats
	EvictedAtRest    int
	EvictedNotAtRest int
	NotAtRest        []string
	FDSamples        int
	PeakFDs          int
	FDMismatches     int
	FDsAfterClose    int
	ReadMismatches   int
	Exact            int
	Masked           int
	Wrong            int
	WarmReads        int
	ColdReads        int
	PeakRSS          int64
	Budget           int64
	WarmNs, ColdNs   int64
	ElapsedMs        int64
}

func runStream(ctx context.Context, mode, dir string) {
	limit := *capacity
	if mode == "unbounded" {
		limit = *databases
	}
	var evictedAtRest, evictedNotAtRest int
	var notAtRestReasons []string
	manager, err := connmgr.New(connmgr.Options{
		Capacity: limit,
		PathFor:  pathFor(dir),
		Open:     openManaged,
		OnEvict: func(e connmgr.Eviction) {
			if e.CloseErr != nil || e.StatusErr != nil {
				fatal("evict "+e.ID, errors.Join(e.CloseErr, e.StatusErr))
			}
			if e.Status.AtRest {
				evictedAtRest++
			} else {
				evictedNotAtRest++
				notAtRestReasons = append(notAtRestReasons, e.ID+": "+e.Status.String())
			}
		},
	})
	if err != nil {
		fatal("manager", err)
	}
	r := theRecipe()
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	model := make([]int, *databases)
	baseFDs := descriptorCount()
	baseRSS := settledRSS()
	maxFDs, maxRSS, fdSamples, fdMismatches, readMismatches, writes, reads := 0, int64(0), 0, 0, 0, 0, 0
	var warm, cold time.Duration
	var warmCount, coldCount int
	// Each sample runs between operations, where every open WAL connection
	// holds exactly three descriptors (.db, -wal, -shm).
	sampleFDs := func() {
		fds := descriptorCount() - baseFDs
		fdSamples++
		maxFDs = max(maxFDs, fds)
		if fds != 3*manager.Stats().Open {
			fdMismatches++
		}
	}
	sampleRSS := func() {
		m, err := procstat.ReadMemory()
		if err != nil {
			fatal("memory", err)
		}
		maxRSS = max(maxRSS, m.RSS-baseRSS)
	}
	started := time.Now()
	for i, op := range stream() {
		id := dbID(op.db)
		missesBefore := manager.Stats().Misses
		began := time.Now()
		h, err := manager.Acquire(ctx, id)
		if err != nil {
			fatal("acquire "+id, err)
		}
		if op.write {
			if err := recipe.ApplyStep(ctx, h.DB, plan[model[op.db]+1]); err != nil {
				fatal("write "+id, err)
			}
			model[op.db]++
			writes++
		} else {
			var rows, balance int64
			var version int
			if err := h.DB.QueryRowContext(ctx, readQuery).Scan(&rows, &balance, &version); err != nil {
				fatal("read "+id, err)
			}
			if version != model[op.db] {
				readMismatches++
			}
			reads++
			if manager.Stats().Misses > missesBefore {
				cold += time.Since(began)
				coldCount++
			} else {
				warm += time.Since(began)
				warmCount++
			}
		}
		h.Release()
		if (i+1)%*sampleEvery == 0 {
			sampleFDs()
		}
		if (i+1)%*memoryEvery == 0 {
			sampleRSS()
		}
	}
	sampleFDs()
	sampleRSS()
	elapsed := time.Since(started)
	stats := manager.Stats()
	if err := manager.Close(); err != nil {
		fatal("close manager", err)
	}
	afterClose := descriptorCount() - baseFDs
	exact, masked, wrong := 0, 0, 0
	for i := range *databases {
		match, err := matchStep(ctx, filepath.Join(dir, dbID(i)+".db"))
		if err != nil {
			fatal("match "+dbID(i), err)
		}
		switch {
		case match.Step != model[i]:
			wrong++
		case match.Exact:
			exact++
		default:
			masked++
		}
	}
	budget := int64(limit)*(2*cacheBudgetBytes()+uncountedPerConnection) + memoryConstant
	result := streamResult{
		Mode: mode, Capacity: limit, Reads: reads, Writes: writes, Stats: stats,
		EvictedAtRest: evictedAtRest, EvictedNotAtRest: evictedNotAtRest, NotAtRest: notAtRestReasons,
		FDSamples: fdSamples, PeakFDs: maxFDs, FDMismatches: fdMismatches, FDsAfterClose: afterClose, ReadMismatches: readMismatches,
		Exact: exact, Masked: masked, Wrong: wrong, WarmReads: warmCount, ColdReads: coldCount,
		PeakRSS: maxRSS, Budget: budget, WarmNs: nanos(warm, warmCount), ColdNs: nanos(cold, coldCount), ElapsedMs: elapsed.Milliseconds(),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		fatal("json", err)
	}
	fmt.Printf("RESULT %s\n", encoded)
}

func nanos(total time.Duration, n int) int64 {
	if n == 0 {
		return 0
	}
	return int64(total) / int64(n)
}

func runChild(ctx context.Context, mode, dir string) streamResult {
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	args := []string{"-mode", mode, "-dir", dir}
	flag.Visit(func(f *flag.Flag) {
		if f.Name != "mode" && f.Name != "dir" && f.Name != "role" {
			args = append(args, "-"+f.Name, f.Value.String())
		}
	})
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fatal("pipe", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start "+mode, err)
	}
	var result streamResult
	found := false
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if encoded, ok := strings.CutPrefix(line, "RESULT "); ok {
			if err := json.Unmarshal([]byte(encoded), &result); err != nil {
				fatal("parse "+line, err)
			}
			found = true
			continue
		}
		fmt.Println(line)
	}
	if err := scanner.Err(); err != nil {
		fatal("read child", err)
	}
	if err := cmd.Wait(); err != nil {
		check(false, fmt.Sprintf("the %s run exited with %v", mode, err))
	}
	if !found {
		fatal(mode, errors.New("child printed no RESULT line"))
	}
	return result
}

// writeStream writes one stream's row and checks its claims.
func writeStream(r streamResult) {
	stats := r.Stats
	tables.streams.Row(r.Mode, r.Capacity, *operations, r.Reads, r.Writes, stats.DistinctDatabases,
		stats.Hits, stats.Misses, r.EvictedAtRest+r.EvictedNotAtRest, r.EvictedAtRest, r.EvictedNotAtRest, stats.VerifiedReopens, stats.RefusedReopens, stats.UnverifiedOpens, stats.MaxOpen,
		r.FDSamples, r.PeakFDs, r.FDMismatches, r.FDsAfterClose, r.ReadMismatches, r.Exact, r.Masked, r.Wrong,
		r.WarmReads, r.ColdReads, r.Budget)
	for _, reason := range r.NotAtRest {
		tables.uncleanEvictions.Row(r.Mode, reason)
	}
	tables.infoStreams.Row(r.Mode, r.PeakRSS, r.WarmNs, r.ColdNs, r.ElapsedMs)
	mode, limit := r.Mode, r.Capacity
	check(r.FDMismatches == 0 && r.PeakFDs == 3*stats.MaxOpen && r.FDsAfterClose == 0, fmt.Sprintf("%s: at every sample the process held exactly 3 descriptors per open connection, at most %d; none after Close", mode, 3*stats.MaxOpen))
	check(stats.MaxOpen <= limit, fmt.Sprintf("%s: never more than %d connections open", mode, limit))
	check(r.EvictedNotAtRest == 0 && stats.RefusedReopens == 0 && stats.VerifiedReopens == stats.Misses-stats.UnverifiedOpens, fmt.Sprintf("%s: every eviction left the file at rest, and every reopen matched the sha256 recorded at its eviction", mode))
	check(r.ReadMismatches == 0 && r.Wrong == 0, fmt.Sprintf("%s: every read saw the model's step, and every final file equals the recipe state for its step", mode))
	if mode == "bounded" {
		check(r.PeakRSS <= r.Budget, fmt.Sprintf("bounded: peak process-memory growth stayed within K x (2 x cache budget + %d KiB) + %d MiB", uncountedPerConnection/1024, memoryConstant>>20))
	}
}

func partA(ctx context.Context, work, master string) {
	part = "A"
	soft, hard, err := procstat.FileLimit()
	if err != nil {
		fatal("limit", err)
	}
	tables.infoLimits.Row(soft, hard)
	// Verify RLIMIT_NOFILE up front: K above the process descriptor limit is a
	// configuration error (budget K <= (soft limit - reserve) / 3 in WAL mode),
	// and the unbounded contrast needs roughly 3 * dbs descriptors.
	if uint64(3**databases+64) > soft {
		fatal("descriptor limit", fmt.Errorf("soft limit %d is below the %d descriptors the unbounded run may need; lower -dbs", soft, 3**databases+64))
	}
	setup("databases", *databases)
	setup("K (bounded capacity)", *capacity)
	setup("operations", *operations)
	setup("seed", *seed)
	setup("hot databases", max(1, *databases/20))
	setup("% of operations to hot", *hotPercent)
	setup("% writes", *writePercent)
	setup("every database", "a copy of recipe step 0")
	setup("step 0 sha256", fileHash(master))
	setup("a write applies", fmt.Sprintf("the next recipe step of that database, up to step %d", *maxStep))
	setup("a read runs", readQuery)
	setup("connections", fmt.Sprintf("recipe settings with cache_size=%d", *cacheSize))
	setup("descriptor sample", fmt.Sprintf("every %d operations", *sampleEvery))
	setup("memory sample", fmt.Sprintf("every %d operations", *memoryEvery))
	bounded := runChild(ctx, "bounded", filepath.Join(work, "bounded"))
	unbounded := runChild(ctx, "unbounded", filepath.Join(work, "unbounded"))
	writeStream(bounded)
	writeStream(unbounded)
	check(unbounded.PeakFDs > 3**capacity, fmt.Sprintf("the unbounded run's peak descriptors exceed the bounded run's limit 3 x K = %d", 3**capacity))
	check(unbounded.PeakRSS > bounded.PeakRSS, "the unbounded run's peak process-memory growth exceeds the bounded run's (SHAPE)")
}

func fileHash(path string) string {
	status, err := connmgr.CheckAtRest(path)
	if err != nil || !status.AtRest {
		fatal("hash "+path, fmt.Errorf("%v %v", status, err))
	}
	return status.SHA256
}

// observe writes one observations row for a file at a moment of part B and
// returns its seq. Every argument after file may be nil.
func observe(moment, file string, atRest, bytes, sha256, step, exact, note any) int {
	observationSeq++
	tables.observations.Row(observationSeq, caseID, moment, file, atRest, bytes, sha256, step, exact, note)
	return observationSeq
}

// observeStatus records what CheckAtRest reported, one problems row per problem.
func observeStatus(moment, file string, status connmgr.Status) {
	if status.AtRest {
		observe(moment, file, true, status.Size, status.SHA256, nil, nil, nil)
		return
	}
	seq := observe(moment, file, false, nil, nil, nil, nil, nil)
	for _, problem := range status.Problems {
		what, why, _ := strings.Cut(problem, ": ")
		problemSeq++
		tables.problems.Row(problemSeq, seq, what, why)
	}
}

func expectStep(ctx context.Context, path string, want int, claim string) {
	match, err := matchStep(ctx, path)
	if err != nil {
		fatal("match "+path, err)
	}
	var step any
	if match.Found() {
		step = match.Step
	}
	observe("recipe step held by the file", filepath.Base(path), true, nil, nil, step, match.Exact, nil)
	check(match.Step == want, claim)
}

type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines *bufio.Scanner
}

func startChild(ctx context.Context, role, path string) *child {
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.CommandContext(ctx, self, "-role", role, "-dir", path, "-max-step", fmt.Sprint(*maxStep), "-cache-size", fmt.Sprint(*cacheSize))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("stdin", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("stdout", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start child", err)
	}
	c := &child{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	if !c.lines.Scan() || c.lines.Text() != "ready" {
		fatal("child "+role, fmt.Errorf("did not report ready: %q %v", c.lines.Text(), c.lines.Err()))
	}
	return c
}

func childMain(ctx context.Context, role, path string) {
	runtime.LockOSThread()
	db, err := openManaged(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	switch role {
	case "reader":
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
			fatal("child read", err)
		}
	case "writer":
		if _, err := theRecipe().ApplyNext(ctx, db); err != nil {
			fatal("child write", err)
		}
	default:
		fatal("role", fmt.Errorf("unknown role %q", role))
	}
	fmt.Println("ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fatal("child stdin", err)
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func newManager(dir string) *connmgr.Manager {
	m, err := connmgr.New(connmgr.Options{Capacity: 4, PathFor: pathFor(dir), Open: openManaged})
	if err != nil {
		fatal("manager", err)
	}
	return m
}

func writeNext(ctx context.Context, m *connmgr.Manager, id string) *connmgr.Handle {
	h, err := m.Acquire(ctx, id)
	if err != nil {
		fatal("acquire "+id, err)
	}
	if _, err := theRecipe().ApplyNext(ctx, h.DB); err != nil {
		fatal("write "+id, err)
	}
	return h
}

func evict(m *connmgr.Manager, id string) connmgr.Status {
	e, err := m.Evict(id)
	if err != nil {
		fatal("evict "+id, err)
	}
	if e.CloseErr != nil || e.StatusErr != nil {
		fatal("evict "+id, errors.Join(e.CloseErr, e.StatusErr))
	}
	observeStatus("eviction by the manager", id+".db", e.Status)
	return e.Status
}

func atRest(path string) connmgr.Status {
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		fatal("check "+path, err)
	}
	return status
}

func partB(ctx context.Context, work, master string) {
	part = "B"
	dir := filepath.Join(work, "B")
	populate(master, dir, 6)
	m := newManager(dir)
	path := func(i int) string { return filepath.Join(dir, dbID(i)+".db") }

	// B1: sqlite3WalClose (src/wal.c:2507-2571) cannot take an EXCLUSIVE lock
	// while another process holds the file open, so it skips the checkpoint
	// and leaves -wal and -shm on disk.
	caseID = "B1"
	writeNext(ctx, m, dbID(0)).Release()
	reader := startChild(ctx, "reader", path(0))
	status := evict(m, dbID(0))
	check(!status.AtRest && len(status.Problems) == 2, "B1: the manager's close could not checkpoint or delete the WAL (another connection holds a lock), so the file is reported not at rest")
	if err := reader.stdin.Close(); err != nil {
		fatal("close child stdin", err)
	}
	if err := reader.cmd.Wait(); err != nil {
		fatal("reader exit", err)
	}
	observeStatus("after the other process closed", dbID(0)+".db", atRest(path(0)))
	expectStep(ctx, path(0), 1, "B1: the last connection to close (the other process) checkpointed and deleted the WAL: at rest, holding step 1")

	// B2: (*sql.DB).Close closes only idle pool connections and returns nil
	// while *sql.Rows holds a connection checked out; connmgr detects
	// OpenConnections > 0 and skips CheckAtRest so it does not drop the live
	// connection's POSIX locks.
	caseID = "B2"
	h := writeNext(ctx, m, dbID(1))
	rows, err := h.DB.QueryContext(ctx, "SELECT id FROM accounts ORDER BY id")
	if err != nil {
		fatal("query", err)
	}
	if !rows.Next() {
		fatal("rows", rows.Err())
	}
	h.Release()
	status = evict(m, dbID(1))
	check(!status.AtRest, "B2: database/sql cannot close a connection that a statement still uses, so DB.Close returned and the file is not at rest")
	if err := rows.Close(); err != nil {
		fatal("rows close", err)
	}
	observeStatus("after rows.Close", dbID(1)+".db", atRest(path(1)))
	expectStep(ctx, path(1), 1, "B2: closing the rows let the connection close: at rest, holding step 1")

	caseID = "B3"
	writer := startChild(ctx, "writer", path(2))
	if err := writer.cmd.Process.Kill(); err != nil {
		fatal("kill", err)
	}
	if err := writer.cmd.Wait(); err == nil {
		fatal("writer", errors.New("killed child exited cleanly"))
	}
	status = atRest(path(2))
	observeStatus("after the kill", dbID(2)+".db", status)
	check(!status.AtRest && len(status.Problems) == 2, "B3: a killed writer leaves a -wal with its committed frames and a -shm")
	naive := filepath.Join(dir, "naive-copy.db")
	if err := copyFile(path(2), naive); err != nil {
		fatal("copy", err)
	}
	expectStep(ctx, naive, 0, "B3: a copy of the database file alone is a valid database WITHOUT the committed step 1 (it is still in the -wal)")
	opened, err := openManaged(ctx, path(2))
	if err != nil {
		fatal("reopen", err)
	}
	if err := opened.Close(); err != nil {
		fatal("close", err)
	}
	observeStatus("after one clean open and close", dbID(2)+".db", atRest(path(2)))
	expectStep(ctx, path(2), 1, "B3: one clean open and close recovers the WAL, checkpoints it and deletes it: at rest, holding step 1")

	// B4: SQLITE_FCNTL_PERSIST_WAL causes sqlite3WalClose (src/wal.c:2533-2551)
	// to checkpoint into .db but leave -wal and -shm (walIndexClose with
	// isDelete=0, src/wal.c:1619-1630).
	caseID = "B4"
	h = writeNext(ctx, m, dbID(3))
	conn, err := h.DB.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	if _, err := sqlitex.PersistWAL(ctx, conn, "main", 1); err != nil {
		fatal("persist wal", err)
	}
	if err := conn.Close(); err != nil {
		fatal("conn close", err)
	}
	h.Release()
	status = evict(m, dbID(3))
	problems := strings.Join(status.Problems, ";")
	check(!status.AtRest && strings.Contains(problems, "-wal holds") && strings.Contains(problems, "-shm exists"), "B4: with PERSIST_WAL the close checkpoints but keeps the -wal (with its bytes) and the -shm, so the manager refuses to call it at rest")
	persisted := filepath.Join(dir, "persist-wal-copy.db")
	if err := copyFile(path(3), persisted); err != nil {
		fatal("copy", err)
	}
	expectStep(ctx, persisted, 1, "B4: the refusal is conservative here: the close did checkpoint, so the database file alone already holds step 1; a rule based on which files exist cannot see that")

	// B5: sqlite3PagerClose (src/pager.c:4222-4279) checks databaseIsUnmoved
	// (src/pager.c:4188-4205 -> fileHasMoved, src/os_unix.c:1623-1632) and
	// omits the checkpoint buffer when the file's inode no longer matches its
	// path, leaving step 1 in the old -wal while moved.db looks at rest.
	caseID = "B5"
	writeNext(ctx, m, dbID(4)).Release()
	moved := filepath.Join(dir, "moved.db")
	if err := os.Rename(path(4), moved); err != nil {
		fatal("rename", err)
	}
	status = evict(m, dbID(4))
	var walBytes int64
	if info, err := os.Stat(path(4) + "-wal"); err == nil {
		walBytes = info.Size()
		observe("left at the old name", dbID(4)+".db-wal", nil, walBytes, nil, nil, nil, nil)
	} else {
		observe("left at the old name", dbID(4)+".db-wal", nil, nil, nil, nil, nil, "no -wal")
	}
	check(!status.AtRest && len(status.Problems) > 0 && status.Problems[0] == "database file missing", "B5: the manager reports the database file missing at its path")
	check(walBytes > 0, "B5: a -wal with content stays behind at the old name")
	expectStep(ctx, moved, 0, "B5: the moved file looks at rest but holds step 0: close saw the file had moved, skipped the checkpoint, and step 1 stayed in the -wal under the old name")

	caseID = "B6"
	writeNext(ctx, m, dbID(5)).Release()
	recorded := evict(m, dbID(5))
	outside, err := openManaged(ctx, path(5))
	if err != nil {
		fatal("outside open", err)
	}
	if _, err := theRecipe().ApplyNext(ctx, outside); err != nil {
		fatal("outside write", err)
	}
	if err := outside.Close(); err != nil {
		fatal("outside close", err)
	}
	_, err = m.Acquire(ctx, dbID(5))
	var changed *connmgr.ChangedWhileClosedError
	if errors.As(err, &changed) {
		observe("reopen refused: changed while closed", dbID(5)+".db", changed.Found.AtRest, changed.Found.Size, changed.Found.SHA256, nil, nil, err.Error())
	} else {
		observe("reopen", dbID(5)+".db", nil, nil, nil, nil, nil, fmt.Sprint(err))
	}
	check(recorded.AtRest && errors.As(err, &changed) && changed.Found.AtRest && changed.Found.SHA256 != recorded.SHA256, "B6: a write made outside the manager while the file was closed changes its sha256, and the manager refuses to reopen it")
	if err := m.Close(); err != nil {
		fatal("close manager", err)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *childRoleFlag != "" {
		childMain(ctx, *childRoleFlag, *runDirFlag)
		return
	}
	if *modeFlag != "" {
		runStream(ctx, *modeFlag, *runDirFlag)
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *databases < 20 || *capacity < 1 || *capacity >= *databases || *operations < 1 || *maxStep < 1 || *maxStep > recipe.DefaultSteps || *sampleEvery < 1 || *memoryEvery < 1 || *writePercent < 0 || *writePercent > 100 || *hotPercent < 0 || *hotPercent > 100 || *cacheSize == 0 {
		fatal("flags", fmt.Errorf("need -dbs >= 20, 1 <= -k < -dbs, -ops >= 1, 1 <= -max-step <= %d, positive sample intervals, percentages 0..100, -cache-size != 0", recipe.DefaultSteps))
	}
	work := sqlitex.MustWorkDir("8.3")
	set = results.MustOpen(work)
	openTables()
	master := filepath.Join(work, "master.db")
	if err := theRecipe().Build(ctx, master, 0); err != nil {
		fatal("build master", err)
	}
	populate(master, filepath.Join(work, "bounded"), *databases)
	populate(master, filepath.Join(work, "unbounded"), *databases)
	partA(ctx, work, master)
	partB(ctx, work, master)
	set.Finish("BOUNDED MANAGER")
}

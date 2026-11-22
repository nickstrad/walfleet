// Experiment 10.1 tests the single-writer ownership invariant in two parts.
//
// Part A runs the finite-state ownership model (service/model) exhaustively up
// to a bounded depth plus seeded random walks, checking that slot exclusivity
// in the store combined with the writer's epoch/append rule prevents any fenced
// epoch from getting a commit accepted. It also runs two broken mutants
// (store-accepts-every-put and resume-from-global-end) as self-tests to show
// each half of the fencing rule is necessary and that the checker catches
// violations.
//
// Part B drives two real node child processes (a and b) sharing a WAL catalog
// and a SeaweedFS S3 store through a 15-step stale-owner scenario: a stale
// listing during takeover (fault stale-list:/log/:1), an active stale owner
// fenced on ship, a killed stale owner restarting with its persisted .owner
// belief file and getting fenced on open, and the contrast between
// store-durable acknowledgements (kept) and an unshipped local-durable
// acknowledgement (lost on takeover). Every command step is cross-checked
// against the model and against the standing descriptor and memory bounds.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.1
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/model"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

var (
	depth      = flag.Int("depth", 16, "exhaustive search depth of the model")
	seed       = flag.Uint64("seed", 1, "seed of the scheduler's action order and random walks")
	walks      = flag.Int("walks", 2000, "seeded random walks past the exhaustive depth")
	walkLength = flag.Int("walk-length", 60, "steps per random walk")
	mutant     = flag.String("mutant", string(model.Correct), "model under test: correct, store-accepts-every-put, resume-from-global-end")
	modelOnly  = flag.Bool("model-only", false, "run part A only (no store needed)")
	role       = flag.String("role", "", "internal: run as a service node")
	process    node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10a", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-1", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 4, "databases a node keeps open")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.1" runs both.
// part labels the claims made while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, modelRuns, modelTraces, steps, modelSteps, closes, problems, slots, catalog, acks, infoRun, infoBounds *results.Table
	}
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.modelRuns = set.Table("model_runs", "seq", "model", "role", "depth", "seed", "interleavings", "distinct_states", "violating", "walks", "walk_steps", "walk_violating", "violation")
	tables.modelTraces = set.Table("model_traces", "model", "step_no", "action", "outcome")
	tables.steps = set.Table("steps", "step", "node", "command", "reply", "descriptors", "model_agrees")
	tables.modelSteps = set.Table("model_steps", "step", "database", "seq", "action", "outcome")
	tables.closes = set.Table("closes", "node", "reply", "summary")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.slots = set.Table("slots", "database", "source", "first", "last", "epoch", "kind")
	tables.catalog = set.Table("catalog", "database", "owner", "epoch", "last_shipped", "schema_version", "location", "restored_position", "restored_state", "file_equals_store")
	tables.acks = set.Table("acks", "seq", "database", "mode", "epoch", "user_version", "position", "in_store")
	tables.infoRun = set.Table("info_run", "name", "value")
	tables.infoBounds = set.Table("info_bounds", "step", "node", "bounds")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

var settingSeq, modelSeq int

func partA() {
	part = "A"
	setting("model_nodes", model.Nodes)
	setting("model_max_epoch", model.MaxEpoch)
	setting("model_max_local_position", model.MaxLocal)
	setting("model_seed", *seed)
	under, err := model.ParseMutant(*mutant)
	if err != nil {
		fatal("-mutant", err)
	}
	report := model.Explore(under, *depth, *seed, *walks, *walkLength)
	recordModel(report, "under test")
	check(report.Violating == 0 && report.WalkViolating == 0 && report.Shortest == nil, fmt.Sprintf("model %s: no interleaving up to depth %d and no random walk lets a fenced epoch get a commit accepted", report.Mutant, *depth))
	// Self-tests: disable each half of the fencing rule in turn so the
	// experiment proves the checker can fail when either guarantee is missing.
	for _, m := range []model.Mutant{model.StoreAcceptsEveryPut, model.ResumeFromGlobalEnd} {
		if m == under {
			continue
		}
		self := model.Explore(m, *depth, *seed, 0, 0)
		recordModel(self, "self-test")
		check(self.Violating > 0 && self.Shortest != nil, fmt.Sprintf("self-test: with %s the checker finds the violation", m))
	}
}

// recordModel writes one exploration and its shortest violating interleaving,
// whose lines model.Explore formats as "%2d. %-28s %s" (step, action, outcome).
func recordModel(report model.Report, role string) {
	modelSeq++
	var violation any
	if report.Shortest != nil {
		violation = report.Violation
	}
	tables.modelRuns.Row(modelSeq, report.Mutant, role, report.Depth, *seed, report.Interleavings, report.States, report.Violating, report.Walks, report.WalkSteps, report.WalkViolating, violation)
	for i, line := range report.Shortest {
		_, rest, _ := strings.Cut(line, ". ")
		tables.modelTraces.Row(report.Mutant, i+1, strings.TrimSpace(rest[:min(28, len(rest))]), strings.TrimSpace(rest[min(28, len(rest)):]))
	}
}

type step struct {
	node, command string
	model         map[string]string
}

// scenario scripts two real nodes alongside the model actions for each step:
// steps 1-7 exercise stale-list retry, active stale-owner fencing, and loss of
// an unshipped local-durable commit on db-0001; steps 8-15 exercise SIGKILL and
// restart of a stale owner holding a persisted belief file on db-0002 while
// keeping ownership of db-0001.
var scenario = []step{
	{"a", "claim db-0001", map[string]string{"db-0001": "A claim, A fence"}},
	{"a", "write db-0001 store-durable", map[string]string{"db-0001": "A write, A ship"}},
	{"a", "write db-0001 local-durable", map[string]string{"db-0001": "A write"}},
	{"b", "claim db-0001", map[string]string{"db-0001": "B claim, B fence-with-stale-list, B fence"}},
	{"a", "write db-0001 store-durable", map[string]string{"db-0001": "A write, A ship"}},
	{"a", "write db-0001 local-durable", map[string]string{"db-0001": "A write"}},
	{"b", "write db-0001 store-durable", map[string]string{"db-0001": "B write, B ship"}},
	{"b", "claim db-0002", map[string]string{"db-0002": "B claim, B fence"}},
	{"b", "write db-0002 store-durable", map[string]string{"db-0002": "B write, B ship"}},
	{"a", "claim db-0002", map[string]string{"db-0002": "A claim, A fence"}},
	{"b", "kill", map[string]string{"db-0001": "B crash", "db-0002": "B crash"}},
	{"b", "restart", map[string]string{"db-0001": "B restart", "db-0002": "B restart"}},
	{"b", "write db-0002 store-durable", map[string]string{"db-0002": "B write, B ship"}},
	{"b", "write db-0001 store-durable", map[string]string{"db-0001": "B write, B ship"}},
	{"a", "write db-0002 store-durable", map[string]string{"db-0002": "A write, A ship"}},
}

var databases = []string{"db-0001", "db-0002"}

type ack struct {
	database           string
	mode               node.AckMode
	epoch, userVersion int
	position           int64
}

func lastNumber(text string) int64 {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r < '0' || r > '9' })
	var n int64 = -1
	if len(fields) > 0 {
		fmt.Sscan(fields[len(fields)-1], &n)
	}
	return n
}

func agrees(reply string, outcomes []string) bool {
	last := outcomes[len(outcomes)-1]
	if strings.HasPrefix(reply, "refused ") || !strings.HasPrefix(last, "ok") {
		return strings.HasPrefix(reply, "refused ") && strings.HasPrefix(last, "refused")
	}
	fences := 0
	for _, o := range outcomes {
		if strings.Contains(o, "fence at") || strings.HasSuffix(o, ", retry") {
			fences++
		}
	}
	if at := strings.Index(reply, "fence at position"); at >= 0 {
		return reply[at:] == fmt.Sprintf("fence at position %d, attempts %d", lastNumber(last), fences)
	}
	if strings.Contains(reply, "stored through position") {
		return lastNumber(reply) == lastNumber(last)
	}
	return true
}

func startNode(ctx context.Context, work, name, fault string) *node.Process {
	config := process
	config.Name, config.Work, config.Fault = name, work, fault
	p, err := node.StartProcess(ctx, name, config.Args()...)
	if err != nil {
		fatal("start node "+name, err)
	}
	return p
}

func partB(ctx context.Context, store *s3store.Store, work string) {
	part = "B"
	if _, err := store.Wipe(ctx); err != nil {
		fatal("wipe the store prefix", err)
	}
	shown := work
	if root, err := sqlitex.RepoRoot(); err == nil {
		if relative, err := filepath.Rel(root, work); err == nil {
			shown = relative
		}
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, shown)
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", shown)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("catalog", err)
	}
	defer cat.Close()
	for _, id := range databases {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("register", err)
		}
	}
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	setting("databases", strings.Join(databases, " "))
	for _, pragma := range strings.Fields(node.PlatformSettings().String()) {
		name, value, _ := strings.Cut(pragma, "=")
		setting(name, value)
	}
	setting("open_databases_per_node", process.Capacity)
	setting("fault_on_node_b", "stale-list:/log/:1")
	nodes := map[string]*node.Process{"a": startNode(ctx, work, "a", ""), "b": startNode(ctx, work, "b", "stale-list:/log/:1")}
	states := map[string]model.State{}
	for _, id := range databases {
		states[id] = model.Initial()
	}
	var acks []ack
	var problems, overBounds []string
	commands, agreed, sampled := 0, 0, 0
	for i, s := range scenario {
		var reply node.Reply
		var descriptors any
		switch s.command {
		case "kill":
			if err := nodes[s.node].Kill(); err != nil {
				fatal("kill", err)
			}
		case "restart":
			nodes[s.node] = startNode(ctx, work, s.node, "")
		default:
			if reply, err = nodes[s.node].Send(s.command); err != nil {
				fatal("send", err)
			}
			tables.infoBounds.Row(i+1, s.node, reply.Bounds)
			var n int
			if _, err := fmt.Sscanf(reply.Bounds, "bounds descriptors=%d", &n); err == nil {
				descriptors = n
			}
			sampled++
			problems = append(problems, reply.Problems...)
			if !reply.WithinBounds() {
				overBounds = append(overBounds, reply.Bounds)
			}
		}
		var modelAgrees any
		for _, id := range databases {
			if _, ok := s.model[id]; !ok {
				continue
			}
			actions, err := model.ParseScript(s.model[id])
			if err != nil {
				fatal("script", err)
			}
			var outcomes []string
			for j, a := range actions {
				next, outcome, enabled := model.Step(states[id], model.Correct, a)
				if !enabled {
					outcome = "refused: not enabled"
				} else {
					states[id] = next
				}
				outcomes = append(outcomes, outcome)
				tables.modelSteps.Row(i+1, id, j+1, a.String(), outcome)
			}
			if reply.Line != "" {
				commands++
				ok := agrees(reply.Line, outcomes)
				modelAgrees = ok
				if ok {
					agreed++
				}
			}
		}
		var line any
		if reply.Line != "" {
			line = reply.Line
		}
		tables.steps.Row(i+1, s.node, s.command, line, descriptors, modelAgrees)
		var a ack
		if n, _ := fmt.Sscanf(reply.Line, "ok write %s %s acknowledged at epoch %d, user_version %d,", &a.database, &a.mode, &a.epoch, &a.userVersion); n == 4 {
			a.mode = node.AckMode(strings.TrimSuffix(string(a.mode), ":"))
			if a.mode == node.StoreDurable {
				a.position = lastNumber(reply.Line)
			}
			acks = append(acks, a)
		} else if strings.HasPrefix(reply.Line, "ok write ") {
			fatal("parse acknowledgement", fmt.Errorf("%q", reply.Line))
		}
	}
	check(commands == agreed, fmt.Sprintf("the real processes and the model agree at every command step (%d of %d)", agreed, commands))
	for _, name := range []string{"a", "b"} {
		reply, err := nodes[name].Close()
		if err != nil {
			fatal("close node "+name, err)
		}
		tables.closes.Row(name, reply.Line, reply.Summary)
		sampled++
		problems = append(problems, reply.Problems...)
		if !reply.WithinBounds() {
			overBounds = append(overBounds, reply.Bounds)
		}
	}
	recordProblems(problems, overBounds)
	check(len(problems) == 0, "no node reported a problem (a failed final ship, an eviction that did not leave the shipped state at rest, an I/O error)")
	check(len(overBounds) == 0, fmt.Sprintf("after each of the %d commands the node's descriptors and memory stayed within the standing bounds", sampled))
	verify(ctx, store, cat, work, states, acks)
}

func recordProblems(problems, overBounds []string) {
	seq := 0
	for _, kind := range []struct {
		name  string
		lines []string
	}{{"problem", problems}, {"over bounds", overBounds}} {
		for _, line := range kind.lines {
			seq++
			tables.problems.Row(seq, kind.name, line)
		}
	}
}

func verify(ctx context.Context, store *s3store.Store, cat *catalog.Catalog, work string, states map[string]model.State, acks []ack) {
	ackSeq := 0
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	part = "C"
	for _, id := range databases {
		chain, restorable, err := fence.Audit(ctx, store, id)
		if err != nil {
			fatal("audit "+id, err)
		}
		var slots []string
		for _, slot := range chain {
			slots = append(slots, fmt.Sprintf("positions %d-%d epoch %d %s", slot.First, slot.Last, slot.Epoch, slot.Kind))
			tables.slots.Row(id, "store", slot.First, slot.Last, slot.Epoch, slot.Kind)
		}
		for _, line := range states[id].ChainLines() {
			var first, last, epoch int64
			var kind string
			if _, err := fmt.Sscanf(line, "positions %d-%d epoch %d %s", &first, &last, &epoch, &kind); err != nil {
				fatal("model chain", err)
			}
			tables.slots.Row(id, "model", first, last, epoch, kind)
		}
		// Require len(restorable.Damaged) == 0 in addition to RestoreAt(last):
		// a damaged segment before a newer snapshot would not affect restoring
		// the latest position, but would break point-in-time auditability.
		check(strings.Join(slots, "; ") == strings.Join(states[id].ChainLines(), "; ") && len(restorable.Damaged) == 0, id+": the store holds exactly the slots the model predicts: one lineage, epochs never decreasing, no damaged object")
		restoreAt := func(position int64) []byte {
			image, err := fence.RestoreAt(restorable, position)
			if err != nil {
				fatal(fmt.Sprintf("restore %s at %d", id, position), err)
			}
			return image
		}
		last := chain[len(chain)-1]
		image := restoreAt(last.Last)
		match := recipe.MatchHashes(hashes, recipe.HashImage(image))
		record, err := cat.Get(ctx, id)
		if err != nil {
			fatal("catalog "+id, err)
		}
		check(record.Epoch == last.Epoch && record.LastShipped == last.Last && record.SchemaVersion == match.Step && match.Found() && (match.Exact || recipe.ChangeCountersAgree(image)),
			id+": the catalog names the newest epoch and the last stored position, and the restored state is a recipe state")
		status, err := connmgr.CheckAtRest(filepath.Join(work, record.Owner, id+".db"))
		if err != nil {
			fatal("at rest", err)
		}
		tables.catalog.Row(id, record.Owner, record.Epoch, record.LastShipped, record.SchemaVersion, record.Location, last.Last, match, status.AtRest && status.SHA256 == walship.SHA256Hex(image))
		check(status.AtRest && status.SHA256 == walship.SHA256Hex(image), id+": the owner's closed local file is byte-identical (E1) to the state rebuilt from the store")
		for _, a := range acks {
			if a.database != id {
				continue
			}
			found := false
			for _, slot := range chain {
				for p := slot.First; p <= slot.Last && int(slot.Epoch) == a.epoch; p++ {
					if binary.BigEndian.Uint32(restoreAt(p)[60:64]) == uint32(a.userVersion) && (a.mode == node.LocalDurable || p == a.position) {
						found = true
					}
				}
			}
			ackSeq++
			var position any
			if a.mode == node.StoreDurable {
				position = a.position
			}
			tables.acks.Row(ackSeq, id, a.mode, a.epoch, a.userVersion, position, found)
			if a.mode == node.StoreDurable {
				check(found, fmt.Sprintf("%s: the store-durable acknowledgement of user_version %d is stored at position %d by epoch %d", id, a.userVersion, a.position, a.epoch))
			} else {
				check(!found, fmt.Sprintf("%s: the local-durable acknowledgement of user_version %d by epoch %d was never shipped, and the takeover lost it, as that mode allows", id, a.userVersion, a.epoch))
			}
		}
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	// Dispatch child-node role before printing the pin line so the child's
	// stdout begins cleanly with "ready" for node.StartProcess.
	if *role == "node" {
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	var store *s3store.Store
	if !*modelOnly {
		var err error
		if store, err = s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), process.Bucket, process.Prefix); err != nil {
			fmt.Printf("STORE UNREACHABLE: %v\n", err)
			os.Exit(1)
		}
	}
	work := sqlitex.MustWorkDir("10.1")
	set = results.MustOpen(work)
	openTables()
	partA()
	if !*modelOnly {
		partB(ctx, store, work)
	}
	set.Finish("ONE WRITER")
}

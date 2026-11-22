// Experiment 10.3 tests the migration safety invariant: an interrupted
// relocation cannot silently create two authoritative copies.
//
// Across 32 fault cases (coordinator stops after each of the 7 steps, source
// and destination SIGKILLs, store faults on the final segment and fence puts,
// and rolling back while the destination stays down), node a starts owning
// db-0001 with two store-durable commits and one unshipped local-durable commit.
// At every step boundary a client probes both nodes' status and sends writes to
// both the catalog owner and the other node. After each fault, a fresh
// relocate.Coordinator recovers from the catalog's relocations row and the S3
// store by Resume or Rollback. The experiment verifies one owner, at most one
// writable node at every boundary, E1 equality at the fence point, preservation
// of every acknowledged write (including local-durable writes drained by step
// 3), and standing resource bounds. A final self-test runs the skip-fence
// mutant (no seal and no store fence) to show two writable nodes appear and
// the lineage audit catches it.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.3
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/relocate"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

const id = "db-0001"

var (
	mutant  = flag.String("mutant", "", "skip-fence: relocate without sealing the source and without the store fence")
	only    = flag.Int("case", 0, "run only this case number (0 runs every case)")
	role    = flag.String("role", "", "internal: run as a service node")
	process node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-3", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 2, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: unused here")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.3" runs both.
// part labels the claims made while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, steps, cases, boundaries, slots, failedInvariants, problems, infoRun *results.Table
	}
	settingSeq int
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.steps = set.Table("relocation_steps", "step", "name")
	tables.cases = set.Table("cases", append([]string{"case_no"}, caseColumns...)...)
	tables.boundaries = set.Table("boundaries", "case_no", "seq", "label", "owner", "epoch", "store_end", "store_end_epoch", "store_end_kind", "node_a", "node_b")
	tables.slots = set.Table("slots", "case_no", "first", "last", "epoch", "kind")
	tables.failedInvariants = set.Table("failed_invariants", "case_no", "invariant")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.infoRun = set.Table("info_run", "name", "value")
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type faultCase struct {
	crashAfter relocate.Step
	kill       string
	killAfter  relocate.Step
	faultNode  string
	fault      string
	rollback   bool
	staysDown  bool
}

const finalSegment, fenceSlot = "log/000000000004", "log/000000000006"

// interrupts reports whether the injected fault is expected to stop Run before
// step 7 finishes. Killing destination b after step 6 (Activate) does not
// interrupt Run because step 7 (RetireSource) talks only to source a.
func (c faultCase) interrupts() bool {
	return !(c.kill == "b" && c.killAfter == relocate.Activate)
}

// describe returns c's injected fault, the node it hits, the step after which
// it hits (0 for a store fault), and the recovery strategy.
func (c faultCase) describe() (fault, faultNode string, after int, recovery string) {
	switch {
	case c.crashAfter != 0:
		fault, after = "coordinator crash", int(c.crashAfter)
	case c.kill != "":
		fault, faultNode, after = "SIGKILL", c.kill, int(c.killAfter)
	case c.fault != "":
		fault, faultNode = "store "+c.fault, c.faultNode
	default:
		fault = "no fault"
	}
	recovery = "resume"
	if c.rollback {
		recovery = "roll back"
	}
	if c.staysDown {
		recovery += fmt.Sprintf(" while node %s stays down", c.kill)
	}
	return fault, faultNode, after, recovery
}

func cases() []faultCase {
	var list []faultCase
	for _, step := range relocate.Steps() {
		list = append(list, faultCase{crashAfter: step})
	}
	for _, step := range relocate.Steps()[:relocate.Activate] {
		list = append(list, faultCase{crashAfter: step, rollback: true})
	}
	for _, k := range []struct {
		name  string
		after relocate.Step
	}{{"a", relocate.FenceEpoch}, {"a", relocate.SealSource}, {"a", relocate.ShipFinal}, {"b", relocate.Rebuild}, {"b", relocate.Activate}, {"a", relocate.Activate}} {
		list = append(list, faultCase{kill: k.name, killAfter: k.after})
	}
	list = append(list, faultCase{kill: "a", killAfter: relocate.SealSource, rollback: true}, faultCase{kill: "b", killAfter: relocate.Rebuild, rollback: true})
	for _, f := range []string{"fail-before-put", "fail-after-put", "crash-before-put", "crash-after-put"} {
		list = append(list, faultCase{faultNode: "a", fault: f + ":" + finalSegment + ":1"}, faultCase{faultNode: "b", fault: f + ":" + fenceSlot + ":1"})
	}
	list = append(list, faultCase{faultNode: "a", fault: "fail-before-put:" + finalSegment + ":1", rollback: true}, faultCase{faultNode: "b", fault: "crash-before-put:" + fenceSlot + ":1", rollback: true})
	list = append(list, faultCase{kill: "b", killAfter: relocate.Rebuild, rollback: true, staysDown: true})
	return list
}

type run struct {
	env          *lab.Env
	number       int
	skipFence    bool
	nodes        map[string]*node.Process
	acks         []node.Ack
	refused      int
	mostWritable int
	boundaries   int
	row          map[string]any
}

func (r *run) remote(name string) node.Remote {
	return node.Remote{
		Process: func() *node.Process { return r.nodes[name] },
		OnReply: func(_ string, reply node.Reply) { r.env.Tracker.Note(name, reply) },
	}
}

func (r *run) start(name, fault string) {
	p, err := r.env.Start(r.env.Config(name, fault))
	if err != nil {
		fatal("start node "+name, err)
	}
	r.nodes[name] = p
}

func (r *run) kill(name string) {
	if p := r.nodes[name]; p != nil {
		p.Signal()
		p.Reap()
	}
	r.nodes[name] = nil
}

func (r *run) alive(name string) bool {
	if r.nodes[name] == nil {
		return false
	}
	if _, err := r.remote(name).Status(id); err != nil {
		r.kill(name)
		return false
	}
	return true
}

func (r *run) write(name string, mode node.AckMode) {
	if !r.alive(name) {
		r.refused++
		return
	}
	reply, err := r.nodes[name].Send(fmt.Sprintf("write %s %s", id, mode))
	if err != nil {
		r.kill(name)
		r.refused++
		return
	}
	r.env.Tracker.Note(name, reply)
	if strings.HasPrefix(reply.Line, "refused ") {
		r.refused++
		return
	}
	_, ack, err := node.ParseAck(reply.Line)
	if err != nil {
		fatal("parse acknowledgement", err)
	}
	r.acks = append(r.acks, ack)
}

func (r *run) boundary(label string) {
	writable := 0
	var states []string
	for _, name := range []string{"a", "b"} {
		state := "down"
		if r.alive(name) {
			status, err := r.remote(name).Status(id)
			if err != nil {
				fatal("status", err)
			}
			state = status
			if strings.HasSuffix(status, "writable") {
				writable++
			}
		}
		states = append(states, state)
	}
	r.mostWritable = max(r.mostWritable, writable)
	record, err := r.env.Catalog.Get(r.env.Ctx, id)
	if err != nil {
		fatal("catalog", err)
	}
	var storeEnd, endEpoch, endKind any = "not one lineage", nil, nil
	if chain, err := fence.Chain(r.env.Ctx, r.env.Store, id); err == nil && len(chain) > 0 {
		end := chain[len(chain)-1]
		storeEnd, endEpoch, endKind = end.Last, end.Epoch, end.Kind
	}
	r.boundaries++
	tables.boundaries.Row(r.number, r.boundaries, label, record.Owner, record.Epoch, storeEnd, endEpoch, endKind, states[0], states[1])
	other := "a"
	if record.Owner == "a" {
		other = "b"
	}
	r.write(record.Owner, node.StoreDurable)
	r.write(other, node.LocalDurable)
}

func short(digest string) string {
	if len(digest) < 12 {
		return digest
	}
	return digest[:12]
}

func (r *run) coordinator() *relocate.Coordinator {
	return &relocate.Coordinator{Catalog: r.env.Catalog, Store: r.env.Store, Node: func(name string) relocate.Participant { return r.remote(name) }, SkipFence: r.skipFence}
}

func describe(err error) string {
	var stepErr *relocate.StepError
	switch {
	case errors.Is(err, relocate.ErrCrashed):
		return "the coordinator stopped"
	case errors.As(err, &stepErr):
		var refused *node.RefusedError
		if errors.As(err, &refused) {
			return fmt.Sprintf("step %d failed: the node refused", int(stepErr.Step))
		}
		return fmt.Sprintf("step %d failed: the node did not answer", int(stepErr.Step))
	}
	return "error: " + err.Error()
}

// caseColumns are the columns of the cases table after case_no.
var caseColumns = []string{"fault", "fault_node", "after_step", "recovery", "run", "restarted", "stayed_down", "discard_error", "recovery_error", "outcome", "relocation_state", "relocation_step", "fence_point", "fence_point_sha256", "fence", "owner", "epoch", "audit_error", "newest_slot_epoch", "acknowledged", "store_durable", "refused", "final_user_version", "final_state", "most_writable", "holds"}

// runCase runs one fault case and writes its row of the cases table; the
// columns a case does not reach stay empty.
func runCase(env *lab.Env, number int, c faultCase, skipFence bool) (bool, int) {
	if err := env.Reset([]string{id}); err != nil {
		fatal("reset", err)
	}
	r := &run{env: env, number: number, nodes: map[string]*node.Process{}, skipFence: skipFence, row: map[string]any{}}
	fault, faultNode, after, strategy := c.describe()
	r.row["fault"], r.row["recovery"] = fault, strategy
	if faultNode != "" {
		r.row["fault_node"] = faultNode
	}
	if after != 0 {
		r.row["after_step"] = after
	}
	defer func() {
		r.row["most_writable"] = r.mostWritable
		values := []any{number}
		for _, column := range caseColumns {
			values = append(values, r.row[column])
		}
		tables.cases.Row(values...)
	}()
	// Kill any remaining node process if recovery fails before verify stops them
	// cleanly, so no child holds open files under _work/10.3/ across cases.
	defer func() {
		for name, p := range r.nodes {
			if p != nil {
				r.kill(name)
			}
		}
	}()
	for _, name := range []string{"a", "b"} {
		fault := ""
		if c.faultNode == name {
			fault = c.fault
		}
		r.start(name, fault)
	}
	if _, err := r.remote("a").Claim(env.Ctx, id); err != nil {
		fatal("claim", err)
	}
	r.write("a", node.StoreDurable)
	r.write("a", node.StoreDurable)
	r.write("a", node.LocalDurable)
	coordinator := r.coordinator()
	coordinator.CrashAfter = c.crashAfter
	coordinator.AfterStep = func(step relocate.Step) {
		r.boundary(fmt.Sprintf("after step %d", int(step)))
		if c.kill != "" && step == c.killAfter {
			r.kill(c.kill)
		}
	}
	result, err := coordinator.Run(env.Ctx, id, "a", "b")
	interrupted := err != nil
	if err != nil {
		r.row["run"] = describe(err)
	} else {
		r.row["run"] = fmt.Sprintf("finished without interruption (%s)", result.Outcome)
	}
	var restarted []string
	for _, name := range []string{"a", "b"} {
		if c.staysDown && name == c.kill {
			r.kill(name)
			r.row["stayed_down"] = name
			continue
		}
		if !r.alive(name) {
			restarted = append(restarted, name)
			r.start(name, "")
		}
	}
	if len(restarted) > 0 {
		r.row["restarted"] = strings.Join(restarted, " ")
	}
	recovery := r.coordinator()
	if interrupted && c.rollback {
		result, err = recovery.Rollback(env.Ctx, id)
	} else if interrupted {
		result, err = recovery.Resume(env.Ctx, id)
	}
	if err != nil && !errors.Is(err, relocate.ErrFenceInStore) {
		r.row["recovery_error"] = err.Error()
		return false, r.mostWritable
	}
	if result.DiscardErr != nil {
		r.row["discard_error"] = result.DiscardErr.Error()
	}
	if c.staysDown {
		r.start(c.kill, "")
	}
	relocation, err := env.Catalog.GetRelocation(env.Ctx, id)
	if err != nil {
		fatal("relocation record", err)
	}
	r.row["outcome"], r.row["relocation_state"], r.row["relocation_step"] = result.Outcome, relocation.State, relocation.Step
	r.row["fence_point"], r.row["fence_point_sha256"], r.row["fence"] = result.Position, short(result.SHA256), result.Fence
	r.boundary("after recovery")
	return r.verify(c, interrupted, result, relocation), r.mostWritable
}

func (r *run) verify(c faultCase, interrupted bool, result relocate.Result, relocation catalog.Relocation) bool {
	env := r.env
	for name, p := range r.nodes {
		if _, err := env.Stop(p); err != nil {
			fatal("stop node "+name, err)
		}
		r.nodes[name] = nil
	}
	record, err := env.Catalog.Get(env.Ctx, id)
	if err != nil {
		fatal("catalog", err)
	}
	r.row["owner"], r.row["epoch"] = record.Owner, record.Epoch
	chain, restorable, err := fence.Audit(env.Ctx, env.Store, id)
	if err != nil {
		r.row["audit_error"] = err.Error()
		return false
	}
	for _, slot := range chain {
		tables.slots.Row(r.number, slot.First, slot.Last, slot.Epoch, slot.Kind)
	}
	last := chain[len(chain)-1]
	restore := func(position int64) []byte {
		image, err := fence.RestoreAt(restorable, position)
		if err != nil {
			fatal(fmt.Sprintf("restore at %d", position), err)
		}
		return image
	}
	final := restore(last.Last)
	finalState, err := env.MatchImage(final)
	if err != nil {
		fatal("match", err)
	}
	owner := "b"
	wantState := catalog.RelocationDone
	if result.Outcome == relocate.RolledBack {
		owner, wantState = "a", catalog.RelocationRolledBack
	}
	fencePosition, fenceSHA := relocation.Position, relocation.SHA256
	if result.Outcome == relocate.RolledBack {
		fencePosition, fenceSHA = result.Position, result.SHA256
	}
	if fencePosition < 1 || result.Fence != fencePosition+1 {
		tables.failedInvariants.Row(r.number, fmt.Sprintf("the recovery names fence point %d and fence %d", fencePosition, result.Fence))
		return false
	}
	atFence, fenced := restore(fencePosition), restore(result.Fence)
	storeDurable := 0
	acksInLineage := true
	for _, ack := range r.acks {
		if ack.Mode != node.StoreDurable {
			continue
		}
		storeDurable++
		found := false
		for _, slot := range chain {
			if slot.Epoch == ack.Epoch && slot.First <= ack.Position && ack.Position <= slot.Last && int(binary.BigEndian.Uint32(restore(ack.Position)[60:64])) == ack.UserVersion {
				found = true
			}
		}
		acksInLineage = acksInLineage && found
	}
	status, err := env.AtRestSHA256(owner, id)
	if err != nil {
		fatal("owner file", err)
	}
	r.row["newest_slot_epoch"], r.row["acknowledged"], r.row["store_durable"], r.row["refused"] = last.Epoch, len(r.acks), storeDurable, r.refused
	r.row["final_user_version"], r.row["final_state"] = finalState.UserVersion, finalState.Match
	parts := []struct {
		ok   bool
		what string
	}{
		{interrupted == c.interrupts(), "the fault interrupted the relocation, or (a destination killed after activation) did not"},
		{relocation.State == wantState && record.Owner == owner && record.Epoch == last.Epoch, "one owner: the catalog, the newest slot and the relocation record agree"},
		{r.mostWritable <= 1, "at most one node accepted writes at every step boundary"},
		{walship.SHA256Hex(atFence) == fenceSHA && walship.SHA256Hex(fenced) == fenceSHA, "the rebuilt copy equals the source at the fenced commit"},
		{acksInLineage && finalState.RecipeState && finalState.UserVersion == len(r.acks), "every acknowledged write is in the one lineage and no refused write is"},
		{status == walship.SHA256Hex(final), "the owner's closed file equals the store's last state"},
		{!c.staysDown || (result.Outcome == relocate.RolledBack && result.DiscardErr != nil), "a rollback while the destination is down finishes without the destination"},
	}
	ok := true
	for _, part := range parts {
		if !part.ok {
			tables.failedInvariants.Row(r.number, part.what)
			ok = false
		}
	}
	r.row["holds"] = ok
	return ok
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *mutant != "" && *mutant != "skip-fence" {
		fatal("-mutant", fmt.Errorf("unknown mutant %q (want skip-fence)", *mutant))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.3", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	set = results.MustOpen(env.Work)
	openTables()
	// env.Reset empties env.Work before every case, so the nodes work in a
	// subdirectory and the tables in env.Work/results survive.
	env.Work = filepath.Join(env.Work, "nodes")
	env.Process.Work = env.Work
	if err := os.MkdirAll(env.Work, 0o755); err != nil {
		fatal("work directory", err)
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", env.ShownWork())
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	if *mutant != "" {
		setting("mutant", *mutant)
	}
	for _, pragma := range strings.Fields(node.PlatformSettings().String()) {
		name, value, _ := strings.Cut(pragma, "=")
		setting(name, value)
	}
	for _, step := range relocate.Steps() {
		_, name, _ := strings.Cut(step.String(), " (")
		tables.steps.Row(int(step), strings.TrimSuffix(name, ")"))
	}
	part = "A"
	for i, c := range cases() {
		if *only != 0 && *only != i+1 {
			continue
		}
		ok, _ := runCase(env, i+1, c, *mutant == "skip-fence")
		check(ok, fmt.Sprintf("case %d: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present", i+1))
	}
	if *mutant == "" && *only == 0 {
		part = "B"
		_, writable := runCase(env, 0, faultCase{}, true)
		check(writable >= 2, "self-test: without the fence the boundary check finds two writable nodes")
	}
	part = "C"
	seq := 0
	for _, kind := range []struct {
		name  string
		lines []string
	}{{"problem", env.Tracker.Problems}, {"over bounds", env.Tracker.OverBounds}} {
		for _, line := range kind.lines {
			seq++
			tables.problems.Row(seq, kind.name, line)
		}
	}
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every node process the descriptors and memory stayed within the standing bounds")
	check(len(env.Tracker.Problems) == 0, "no node reported a problem")
	tables.infoRun.Row("node replies that carried a bounds check", env.Tracker.Replies)
	set.Finish("INTERRUPTED RELOCATION")
}

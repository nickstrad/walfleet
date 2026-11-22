// Experiment 10.2 tests two capstone invariants under process kills: every
// database can be reopened after process failure, and no acknowledged durable
// write is lost under its stated durability mode.
//
// One node owns 4 databases while keeping at most 2 open (forcing LRU eviction
// on every round-robin write). Each run sends 12 writes in either local-durable
// mode (synchronous=FULL; walFrames syncs -wal before COMMIT returns at
// src/wal.c:4195-4213; recovery restarts on the local directory) or
// store-durable mode (acknowledged after the segment put; recovery deletes the
// node's entire directory and rebuilds every database from SeaweedFS S3).
//
// Part A (Linux only) uses strace to kill the node on entry to evenly spaced
// fsync/fdatasync calls and verifies walIndexRecover (src/wal.c:1390-1614)
// recovers every database cleanly with recovered user_version >= acknowledged.
// Part B (portable) kills interactive runs at seeded random delays inside the
// measured write-loop window (redrawing if a kill lands after the last reply).
// Part C runs the ack-before-put mutant as a self-test to prove the checker
// catches an unshipped store-durable acknowledgement.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.2
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive"

var (
	databases    = flag.Int("databases", 4, "databases the node owns")
	writes       = flag.Int("writes", 12, "writes in each killed run, round robin over the databases")
	syscallKills = flag.Int("syscall-kills", 4, "kill points at fsync per mode (Linux, strace)")
	randomKills  = flag.Int("random-kills", 2, "kills at a random time per mode")
	seed         = flag.Uint64("seed", 1, "seed of the random kill times")
	mutant       = flag.String("mutant", "", "ack-before-put: the killed node acknowledges store-durable writes before the segment put")
	portable     = flag.Bool("portable", false, "skip the strace kill points even on Linux")
	role         = flag.String("role", "", "internal: run as a service node")
	process      node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-2", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 2, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: the mutant node")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.2" runs both.
// part labels the rows and claims written while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, parts, uninterrupted, fsyncKills, randomKills, recovery, violations, selfTest, problems, infoRun, infoWriteLoops, infoRandomKills *results.Table
	}
	settingSeq int
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.parts = set.Table("parts", "part", "name", "ran", "note")
	tables.uninterrupted = set.Table("uninterrupted", "part", "mode", "how", "commands", "acknowledged", "command_thread_fsyncs", "exit_code")
	tables.fsyncKills = set.Table("fsync_kills", "mode", "kill_no", "at_fsync", "of_fsyncs", "file", "killed", "during_command_no", "during_command", "acknowledged", "reopened")
	tables.randomKills = set.Table("random_kills", "mode", "kill_no", "in_flight", "violations")
	tables.recovery = set.Table("recovery", "part", "mode", "kill_no", "database", "acknowledged_version", "reopened", "integrity", "recovered_version", "recovered_state")
	tables.violations = set.Table("violations", "part", "mode", "kill_no", "line")
	tables.selfTest = set.Table("self_test", "reply", "acknowledged")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.infoRun = set.Table("info_run", "name", "value")
	tables.infoWriteLoops = set.Table("info_write_loops", "mode", "writes", "elapsed_ms", "acknowledged")
	tables.infoRandomKills = set.Table("info_random_kills", "mode", "kill_no", "draw", "delay_ms", "acknowledged", "reopened", "recovered_versions", "acknowledged_versions")
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

type experiment struct {
	env *lab.Env
	ids []string
}

type outcome struct {
	acked      map[string]int
	acks       int
	replies    int
	recovered  map[string]lab.Recovered
	integrity  map[string]string
	reopened   int
	violations []string
}

func (x *experiment) script(mode node.AckMode) []string {
	commands := make([]string, *writes)
	for i := range commands {
		commands[i] = fmt.Sprintf("write %s %s", x.ids[i%len(x.ids)], mode)
	}
	return commands
}

func (x *experiment) setup() {
	if err := x.env.Reset(x.ids); err != nil {
		fatal("reset", err)
	}
	p, err := x.env.Start(x.env.Config("a", ""))
	if err != nil {
		fatal("start node a", err)
	}
	for _, id := range x.ids {
		reply, err := x.env.Send(p, "claim "+id)
		if err != nil || !strings.HasPrefix(reply.Line, "ok claim ") {
			fatal("claim "+id, fmt.Errorf("%q %v", reply.Line, err))
		}
	}
	if _, err := x.env.Stop(p); err != nil {
		fatal("stop node a", err)
	}
}

func (x *experiment) killedConfig() node.ProcessConfig {
	config := x.env.Config("a", "")
	config.AckBeforePut = *mutant == "ack-before-put"
	return config
}

func (x *experiment) record(o *outcome, replies []string) {
	o.replies = len(replies)
	for _, line := range replies {
		if !strings.HasPrefix(line, "ok write ") {
			continue
		}
		id, ack, err := node.ParseAck(line)
		if err != nil {
			fatal("parse acknowledgement", err)
		}
		o.acked[id] = max(o.acked[id], ack.UserVersion)
		o.acks++
	}
}

func newOutcome() *outcome {
	return &outcome{acked: map[string]int{}, recovered: map[string]lab.Recovered{}, integrity: map[string]string{}}
}

func (x *experiment) traced(mode node.AckMode, kill *trace.KillAt) (*trace.Result, *outcome) {
	x.setup()
	command, err := trace.SelfCommand(x.killedConfig().Args()...)
	if err != nil {
		fatal("self command", err)
	}
	stdin := strings.Join(x.script(mode), "\n") + "\n"
	result, err := trace.Run(x.env.Ctx, trace.Options{Command: command, Stdin: strings.NewReader(stdin), Syscalls: []string{"fsync", "fdatasync"}, KillAt: kill})
	if err != nil {
		fatal("strace", err)
	}
	o := newOutcome()
	x.record(o, x.env.Tracker.NoteOutput("a", result.Stdout))
	return result, o
}

func (x *experiment) randomKill(mode node.AckMode, delay time.Duration) (*outcome, time.Duration) {
	x.setup()
	p, err := x.env.Start(x.killedConfig())
	if err != nil {
		fatal("start node a", err)
	}
	// Time only the write loop (after setup and node startup) so random kill
	// delays fall inside the active write window rather than after the last reply.
	writesStarted := time.Now()
	timer := time.AfterFunc(delay, func() { p.Signal() })
	var replies []string
	for _, command := range x.script(mode) {
		reply, err := x.env.Send(p, command)
		if err != nil {
			break
		}
		replies = append(replies, reply.Line)
	}
	writeElapsed := time.Since(writesStarted)
	timer.Stop()
	p.Signal()
	p.Reap()
	o := newOutcome()
	x.record(o, replies)
	return o, writeElapsed
}

func (x *experiment) recover(mode node.AckMode, o *outcome) {
	if mode == node.StoreDurable {
		if err := os.RemoveAll(x.env.NodeDir("a")); err != nil {
			fatal("delete the local files", err)
		}
	}
	p, err := x.env.Start(x.env.Config("a", ""))
	if err != nil {
		fatal("restart node a", err)
	}
	remote := node.Remote{Process: func() *node.Process { return p }, OnReply: func(_ string, reply node.Reply) { x.env.Tracker.Note("a", reply) }}
	for _, id := range x.ids {
		if mode == node.StoreDurable {
			if _, err := remote.Claim(x.env.Ctx, id); err != nil {
				o.violations = append(o.violations, fmt.Sprintf("%s: no rebuild from the store: %v", id, err))
				continue
			}
		}
		inspection, err := remote.Inspect(x.env.Ctx, id)
		if err != nil {
			o.violations = append(o.violations, fmt.Sprintf("%s does not reopen: %v", id, err))
			continue
		}
		o.reopened++
		o.integrity[id] = inspection.Integrity
	}
	if _, err := x.env.Stop(p); err != nil {
		fatal("stop node a", err)
	}
	for _, id := range x.ids {
		if _, ok := o.integrity[id]; !ok {
			continue
		}
		recovered, err := x.env.MatchFile(x.env.DatabasePath("a", id))
		if err != nil {
			fatal("match "+id, err)
		}
		o.recovered[id] = recovered
		switch {
		case o.integrity[id] != "ok":
			o.violations = append(o.violations, fmt.Sprintf("%s integrity %s", id, o.integrity[id]))
		case !recovered.RecipeState:
			o.violations = append(o.violations, fmt.Sprintf("%s is not a recipe state: %s", id, recovered.Match))
		case recovered.UserVersion < o.acked[id]:
			o.violations = append(o.violations, fmt.Sprintf("%s recovered step %d, acknowledged step %d", id, recovered.UserVersion, o.acked[id]))
		}
	}
}

func (x *experiment) versions(o *outcome) (string, string) {
	var recovered, acked []string
	for _, id := range x.ids {
		recovered = append(recovered, fmt.Sprint(o.recovered[id].UserVersion))
		acked = append(acked, fmt.Sprint(o.acked[id]))
	}
	return strings.Join(recovered, " "), strings.Join(acked, " ")
}

// recordRecovery writes what each database held after the recovery of one
// killed run, and the violations the recovery found.
func (x *experiment) recordRecovery(mode node.AckMode, kill int, o *outcome) {
	for _, id := range x.ids {
		integrity, reopened := o.integrity[id]
		var state, version any
		if recovered, ok := o.recovered[id]; ok {
			state, version = recovered.Match, recovered.UserVersion
		}
		tables.recovery.Row(part, mode, kill, id, o.acked[id], reopened, integrity, version, state)
	}
	x.recordViolations(mode, kill, o)
}

func (x *experiment) recordViolations(mode node.AckMode, kill int, o *outcome) {
	for _, v := range o.violations {
		tables.violations.Row(part, mode, kill, v)
	}
}

func killedFile(result *trace.Result) string {
	for _, call := range result.Log.Calls {
		if call.Unfinished && (call.Name == "fsync" || call.Name == "fdatasync") {
			return filepath.Base(call.Path)
		}
	}
	return "no file: nothing was killed"
}

func fsyncsByThread(result *trace.Result) (int, int, int) {
	counts := map[int]int{}
	for _, call := range result.Log.Calls {
		if call.Name == "fsync" || call.Name == "fdatasync" {
			counts[call.PID]++
		}
	}
	best, most, total := 0, 0, 0
	for pid, n := range counts {
		total += n
		if n > most {
			best, most = pid, n
		}
	}
	return best, most, total - most
}

func (x *experiment) syscallKills(mode node.AckMode) {
	uninterrupted, base := x.traced(mode, nil)
	_, syncs, elsewhere := fsyncsByThread(uninterrupted)
	tables.uninterrupted.Row(part, mode, "strace, not killed", base.replies, base.acks, syncs, uninterrupted.ExitCode)
	tables.infoRun.Row(fmt.Sprintf("%s: fsync calls on other threads", mode), elsewhere)
	check(uninterrupted.ExitCode == 0 && base.acks == *writes && elsewhere == 0, fmt.Sprintf("%s: the uninterrupted run acknowledges all %d writes and every fsync runs on the command thread", mode, *writes))
	violations, killed := 0, 0
	for i := range *syscallKills {
		when := 1 + i*(syncs-1)/max(*syscallKills-1, 1)
		result, o := x.traced(mode, &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: when})
		if result.Killed == "SIGKILL" {
			killed++
		}
		x.recover(mode, o)
		var duringNo, during any
		if o.replies < *writes {
			duringNo, during = o.replies+1, x.script(mode)[o.replies]
		}
		tables.fsyncKills.Row(mode, i+1, when, syncs, killedFile(result), result.Killed, duringNo, during, o.acks, o.reopened)
		x.recordRecovery(mode, i+1, o)
		violations += len(o.violations)
	}
	check(killed == *syscallKills, fmt.Sprintf("%s: strace killed the node at each of the %d fsync kill points", mode, *syscallKills))
	check(violations == 0, fmt.Sprintf("%s: after each kill at an fsync every database reopens (integrity ok, a recipe state) and holds at least every write acknowledged in this mode", mode))
}

const drawsPerRandomKill = 5

func (x *experiment) randomTimeKills(mode node.AckMode, rng *rand.Rand) {
	full, elapsed := x.randomKill(mode, time.Hour)
	tables.uninterrupted.Row(part, mode, "interactive, not killed", full.replies, full.acks, nil, nil)
	tables.infoWriteLoops.Row(mode, *writes, elapsed.Milliseconds(), full.acks)
	violations, inFlight := 0, 0
	for i := range *randomKills {
		killViolations, landed := 0, false
		for draw := 1; draw <= drawsPerRandomKill; draw++ {
			delay := time.Duration(rng.Int64N(int64(elapsed)))
			o, _ := x.randomKill(mode, delay)
			x.recover(mode, o)
			recovered, acked := x.versions(o)
			tables.infoRandomKills.Row(mode, i+1, draw, delay.Milliseconds(), o.acks, o.reopened, recovered, acked)
			x.recordViolations(mode, i+1, o)
			killViolations += len(o.violations)
			// Every draw is recovered and checked; redraw up to drawsPerRandomKill
			// if the timer fired after all writes had already replied.
			if o.replies < *writes {
				landed = true
				break
			}
		}
		tables.randomKills.Row(mode, i+1, landed, killViolations)
		violations += killViolations
		if landed {
			inFlight++
		}
	}
	check(full.acks == *writes, fmt.Sprintf("%s: an interactive run that is not killed acknowledges all %d writes", mode, *writes))
	check(inFlight == *randomKills, fmt.Sprintf("%s: each of the %d random kills landed while writes were in flight (a kill after the last reply is recovered and checked too, then redrawn, at most %d draws per kill)", mode, *randomKills, drawsPerRandomKill))
	check(violations == 0, fmt.Sprintf("%s: after every kill at a random time every database reopens (integrity ok, a recipe state) and holds at least every acknowledged write", mode))
}

func (x *experiment) selfTest() {
	part = "C"
	tables.parts.Row(part, "self-test: a node that acknowledges store-durable writes before the segment put", true, nil)
	x.setup()
	config := x.env.Config("a", "")
	config.AckBeforePut = true
	p, err := x.env.Start(config)
	if err != nil {
		fatal("start the mutant node", err)
	}
	reply, err := x.env.Send(p, fmt.Sprintf("write %s %s", x.ids[0], node.StoreDurable))
	if err != nil {
		fatal("mutant write", err)
	}
	p.Signal()
	p.Reap()
	o := newOutcome()
	x.record(o, []string{reply.Line})
	x.recover(node.StoreDurable, o)
	tables.selfTest.Row(reply.Line, o.acks)
	x.recordRecovery(node.StoreDurable, 1, o)
	check(o.acks == 1 && len(o.violations) == 1, "self-test: the checker finds the store-durable acknowledgement the store never received")
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		// Pin the command loop to one OS thread so strace's per-thread syscall
		// counter sees every SQLite fsync on a single deterministic thread.
		runtime.LockOSThread()
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *mutant != "" && *mutant != "ack-before-put" {
		fatal("-mutant", fmt.Errorf("unknown mutant %q (want ack-before-put)", *mutant))
	}
	if *databases < 1 || *writes < 1 || *syscallKills < 1 || *randomKills < 0 {
		fatal("flags", fmt.Errorf("-databases, -writes and -syscall-kills must be positive, -random-kills not negative"))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.2", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	set = results.MustOpen(env.Work)
	openTables()
	// env.Reset empties env.Work before every run, so the node works in a
	// subdirectory and the tables in env.Work/results survive.
	env.Work = filepath.Join(env.Work, "node")
	env.Process.Work = env.Work
	if err := os.MkdirAll(env.Work, 0o755); err != nil {
		fatal("work directory", err)
	}
	x := &experiment{env: env, ids: lab.Ids("db", *databases)}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", env.ShownWork())
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	setting("databases", *databases)
	setting("open_databases", process.Capacity)
	setting("writes_per_run", *writes)
	setting("fsync_kill_points_per_mode", *syscallKills)
	setting("random_kills_per_mode", *randomKills)
	setting("random_seed", *seed)
	if *mutant != "" {
		setting("mutant", *mutant)
	}
	for _, pragma := range strings.Fields(node.PlatformSettings().String()) {
		name, value, _ := strings.Cut(pragma, "=")
		setting(name, value)
	}
	modes := []node.AckMode{node.LocalDurable, node.StoreDurable}
	part = "A"
	if trace.Supported && !*portable {
		tables.parts.Row(part, "kills at a chosen fsync (strace)", true, nil)
		for _, mode := range modes {
			x.syscallKills(mode)
		}
	} else {
		notice := trace.LinuxOnlyMessage("killing the node at a chosen fsync with strace", packagePath)
		fmt.Println(notice)
		tables.parts.Row(part, "kills at a chosen fsync (strace)", false, notice)
	}
	part = "B"
	tables.parts.Row(part, "kills at a random time (portable)", true, nil)
	rng := rand.New(rand.NewPCG(*seed, 102))
	for _, mode := range modes {
		x.randomTimeKills(mode, rng)
	}
	x.selfTest()
	part = "D"
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
	check(len(env.Tracker.Problems) == 0, "no node reported a problem")
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every node process the descriptors and memory stayed within the standing bounds")
	tables.infoRun.Row("node replies that carried a bounds check", env.Tracker.Replies)
	set.Finish("ACKNOWLEDGED WRITES SURVIVE")
}

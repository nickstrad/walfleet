// Experiment 9.2 separates process-crash recovery from power-loss durability
// in WAL mode across synchronous=OFF, NORMAL, and FULL (plus a NORMAL run with
// a concurrent reader that stops the PASSIVE checkpoint short of the end).
// Part B (portable) compares process-crash recovery against a file-level
// power-cut model derived from SQLite's sync rules (src/pager.c:3658-3704,
// src/wal.h:25-26, src/wal.c:2199-2395, 4038-4266) and cross-checked by strace
// on Linux. Part A (Linux only) kills the writer child via strace --inject on
// entry to every sync and six spread pwrite64 calls to verify crash recovery.
//
// It prints only its verdict (and LINUX-ONLY notices where strace is missing).
// The measurements go to CSV tables that report.sql presents:
// go run ./cmd/course run 9.2
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
	"github.com/nickstrad/walfleet/internal/walfile"
)

const (
	childMode              = "child-writer"
	baseStep               = 3
	lastStep               = 11
	checkpointAfter        = 7
	pwriteKills            = 6
	readerFromStep         = 5
	beforeCheckpointSuffix = ".before-checkpoint"
	packagePath            = "ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss"
)

var levels = []string{"OFF", "NORMAL", "FULL"}

type variant struct {
	level      string
	readerFrom int
}

func (v variant) String() string {
	if v.readerFrom == 0 {
		return "synchronous=" + v.level
	}
	return fmt.Sprintf("synchronous=%s with a second connection holding a read transaction from step %d", v.level, v.readerFrom)
}

func (v variant) dirName() string {
	if v.readerFrom == 0 {
		return strings.ToLower(v.level)
	}
	return fmt.Sprintf("%s-reader-%d", strings.ToLower(v.level), v.readerFrom)
}

var modelVariants = []variant{{"OFF", 0}, {"NORMAL", 0}, {"FULL", 0}, {"NORMAL", readerFromStep}}

// runChild executes the writer workload on one pinned OS thread (so strace's
// syscall counter sees its I/O calls in deterministic order), commits recipe
// steps 4-11 with one PASSIVE checkpoint after step 7, and exits with os.Exit(0)
// without closing SQLite so the files remain exactly as a crashed process leaves them.
func runChild(args []string) error {
	flags := flag.NewFlagSet(childMode, flag.ContinueOnError)
	db := flags.String("db", "", "")
	level := flags.String("sync", "", "")
	readerFrom := flags.Int("reader-from", 0, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	options := r.SQLiteOptions()
	options.Conn.Synchronous = *level
	options.Conn.WALAutocheckpoint = 0
	handle, err := sqlitex.Open(ctx, *db, options)
	if err != nil {
		return err
	}
	var reader *sql.Conn
	for k := baseStep + 1; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, handle, k); err != nil {
			return err
		}
		wal, err := walfile.ParseFile(*db + "-wal")
		if err != nil {
			return err
		}
		fmt.Printf("commit %d end=%d generation=%d\n", k, walfile.HeaderSize+wal.LastCommitFrame*wal.FrameSize(), wal.Header.Salt1)
		fmt.Printf("ack %d\n", k)
		if k == *readerFrom {
			if reader, err = openReader(ctx, *db, options); err != nil {
				return err
			}
			fmt.Printf("reader %d\n", k)
		}
		if k == checkpointAfter {
			if reader != nil {
				if err := copyFile(*db, *db+beforeCheckpointSuffix, -1); err != nil {
					return err
				}
			}
			result, err := sqlitex.Checkpoint(ctx, handle, "", sqlitex.CheckpointPassive)
			if err != nil {
				return err
			}
			fmt.Printf("checkpoint %d %s\n", k, result)
		}
	}
	if reader != nil {
		fmt.Println("reader still open")
	}
	os.Exit(0)
	return nil
}

func openReader(ctx context.Context, path string, options sqlitex.Options) (*sql.Conn, error) {
	handle, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return nil, err
	}
	conn, err := handle.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	var tables int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		return nil, err
	}
	return conn, nil
}

type writerRun struct {
	lastAck        int
	commitEnd      map[int]int64
	generation     map[int]uint32
	checkpoint     string
	checkpointBusy int
	checkpointed   int
	logFrames      int
	readerFrom     int
	lastEvent      string
}

func parseRun(stdout string) (writerRun, error) {
	run := writerRun{lastAck: baseStep, commitEnd: map[int]int64{}, generation: map[int]uint32{}}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		var k int
		var end int64
		var generation uint32
		switch {
		case strings.HasPrefix(line, "ack "):
			if _, err := fmt.Sscanf(line, "ack %d", &k); err != nil || k != run.lastAck+1 {
				return run, fmt.Errorf("bad ack line %q after %d", line, run.lastAck)
			}
			run.lastAck, run.lastEvent = k, "commit"
		case strings.HasPrefix(line, "commit "):
			if _, err := fmt.Sscanf(line, "commit %d end=%d generation=%d", &k, &end, &generation); err != nil {
				return run, fmt.Errorf("bad commit line %q", line)
			}
			run.commitEnd[k], run.generation[k] = end, generation
		case strings.HasPrefix(line, "checkpoint "):
			if _, err := fmt.Sscanf(line, "checkpoint %d busy=%d log=%d checkpointed=%d", &k, &run.checkpointBusy, &run.logFrames, &run.checkpointed); err != nil || k != checkpointAfter {
				return run, fmt.Errorf("bad checkpoint line %q", line)
			}
			run.checkpoint, run.lastEvent = line, "checkpoint"
		case strings.HasPrefix(line, "reader "):
			if line == "reader still open" {
				continue
			}
			if _, err := fmt.Sscanf(line, "reader %d", &run.readerFrom); err != nil {
				return run, fmt.Errorf("bad reader line %q", line)
			}
		default:
			return run, fmt.Errorf("unexpected child output %q", line)
		}
	}
	return run, scanner.Err()
}

type syncRule struct {
	cut              int64
	hasCut           bool
	outside          string
	databaseUnsynced bool
}

// ruleCut computes which WAL bytes were synced and whether the database file
// holds unsynced checkpoint writes, directly from SQLite's sync-flag rules:
// sqlite3PagerSetFlags (src/pager.c:3658-3704) + WAL_SYNC_FLAGS/CKPT_SYNC_FLAGS
// (src/wal.h:25-26), walFrames (src/wal.c:4121-4124, 4196-4213), and
// walCheckpoint (src/wal.c:2277 syncs the WAL before backfilling; 2328-2335
// syncs the database file only when mxSafeFrame == mxFrame).
func ruleCut(level string, run writerRun) syncRule {
	partialCheckpoint := run.checkpointed > 0 && run.checkpointed < run.logFrames
	switch level {
	case "OFF":
		// At OFF, syncFlags and walSyncFlags are both 0; a checkpoint writes
		// the database file without syncing it, putting power loss outside a
		// WAL-truncation model.
		if run.checkpoint != "" {
			return syncRule{cut: 0, hasCut: true, outside: "the checkpoint wrote the database file and nothing was ever synced", databaseUnsynced: true}
		}
		return syncRule{cut: 0, hasCut: true}
	case "NORMAL":
		// At NORMAL, CKPT_SYNC_FLAGS is non-zero (header + checkpoint syncs)
		// while WAL_SYNC_FLAGS is 0 (no per-commit sync). If a partial
		// checkpoint stopped WAL restart, the last WAL sync is wal.c:2277 at
		// the checkpointed end; if the WAL restarted, it is the 32-byte header
		// sync at wal.c:4121-4124.
		if run.lastEvent == "checkpoint" {
			return syncRule{databaseUnsynced: partialCheckpoint}
		}
		if run.checkpointed > 0 && run.generation[run.lastAck] == run.generation[checkpointAfter] {
			return syncRule{cut: run.commitEnd[checkpointAfter], hasCut: true, databaseUnsynced: partialCheckpoint}
		}
		return syncRule{cut: walfile.HeaderSize, hasCut: true}
	}
	return syncRule{databaseUnsynced: partialCheckpoint}
}

type lab struct {
	traced bool
	ctx    context.Context
	lab    *procs.Lab
	r      recipe.Recipe
	dir    string
	base   string
	set    *results.Set
	cuts   int
	tables struct {
		runs, commits, syncs, rules, powerCuts, killPlans, kills, skipped *results.Table
	}
}

func (x *lab) openTables() {
	x.tables.runs = x.set.Table("runs", "seq", "variant", "synchronous", "reader_from", "first_acknowledged", "last_acknowledged", "checkpoint_after", "checkpoint_busy", "checkpoint_log", "checkpointed", "same_generation_after_checkpoint", "crash_integrity", "crash_recovered_step", "crash_exact_bytes")
	x.tables.commits = x.set.Table("commits", "variant", "step", "wal_end", "wal_generation")
	x.tables.syncs = x.set.Table("syncs", "variant", "file", "syncs", "writes_after_last_sync", "lowest_unsynced_offset")
	x.tables.rules = x.set.Table("sync_rules", "variant", "wal_synced_up_to", "database_file_unsynced", "power_model_not_applicable")
	x.tables.powerCuts = x.set.Table("power_cuts", "seq", "variant", "database_file", "wal_kept_bytes", "wal_bytes", "lost_commits", "integrity", "recovered_step", "exact_bytes")
	x.tables.killPlans = x.set.Table("kill_plans", "synchronous", "sync_calls", "pwrite64_calls", "pwrite64_kills")
	x.tables.kills = x.set.Table("kills", "synchronous", "kill_no", "syscall", "call_no", "killed_call", "signal", "acknowledged", "recovered_step", "exact_bytes", "integrity")
	x.tables.skipped = x.set.Table("skipped", "part", "variant", "what")
}

// linuxOnly prints the LINUX-ONLY notice and records it in the skipped table.
func (x *lab) linuxOnly(part string, variant any, what string) {
	fmt.Println(trace.LinuxOnlyMessage(what, packagePath))
	x.tables.skipped.Row(part, variant, what)
}

func copyFile(from, to string, limit int64) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	var reader io.Reader = in
	if limit >= 0 {
		reader = io.LimitReader(in, limit)
	}
	_, copyErr := io.Copy(out, reader)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func (x *lab) recover(path string) (string, recipe.Match) {
	db, err := x.r.Open(x.ctx, path)
	if err != nil {
		x.lab.Fatal("reopen", err)
	}
	var integrity string
	err = db.QueryRowContext(x.ctx, "PRAGMA integrity_check").Scan(&integrity)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("integrity_check", err)
	}
	match, err := x.r.MatchStep(x.ctx, path)
	if err != nil {
		x.lab.Fatal("match", err)
	}
	return integrity, match
}

func (x *lab) runWriter(dir string, v variant, kill *trace.KillAt) (string, *trace.Result) {
	db := filepath.Join(dir, "r.db")
	if err := copyFile(x.base, db, -1); err != nil {
		x.lab.Fatal("copy base", err)
	}
	args := []string{childMode, "-db", db, "-sync", v.level, "-reader-from", fmt.Sprint(v.readerFrom)}
	if !x.traced {
		self, err := os.Executable()
		if err != nil {
			x.lab.Fatal("executable", err)
		}
		out, err := exec.Command(self, args...).Output()
		if err != nil {
			x.lab.Fatal("writer", err)
		}
		return string(out), nil
	}
	command, err := trace.SelfCommand(args...)
	if err != nil {
		x.lab.Fatal("command", err)
	}
	result, err := trace.Run(x.ctx, trace.Options{Command: command, KillAt: kill})
	if err != nil {
		x.lab.Fatal("strace", err)
	}
	if kill == nil && result.ExitCode != 0 {
		x.lab.Fatal("writer", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr))
	}
	return result.Stdout, result
}

func (x *lab) model(seq int, v variant) *trace.Result {
	level := v.level
	dir := filepath.Join(x.dir, "model-"+v.dirName())
	stdout, result := x.runWriter(dir, v, nil)
	run, err := parseRun(stdout)
	if err != nil {
		x.lab.Fatal("parse", err)
	}
	db := filepath.Join(dir, "r.db")
	sameGeneration := run.generation[run.lastAck] == run.generation[checkpointAfter]
	if v.readerFrom != 0 {
		x.set.Check("B", run.readerFrom == v.readerFrom && run.checkpointed > 0 && run.checkpointed < run.logFrames && sameGeneration,
			fmt.Sprintf("%s: the reader stopped the checkpoint short of the end and the WAL did not restart", v))
	}
	generation := 0
	for k := baseStep + 1; k <= run.lastAck; k++ {
		if k == baseStep+1 || run.generation[k] != run.generation[k-1] {
			generation++
		}
		x.tables.commits.Row(v.dirName(), k, run.commitEnd[k], generation)
	}
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(db+suffix, filepath.Join(dir, "crash", "r.db"+suffix), -1); err != nil {
			x.lab.Fatal("copy", err)
		}
	}
	integrity, match := x.recover(filepath.Join(dir, "crash", "r.db"))
	var readerFrom any
	if v.readerFrom != 0 {
		readerFrom = v.readerFrom
	}
	x.tables.runs.Row(seq, v.dirName(), level, readerFrom, baseStep+1, run.lastAck, checkpointAfter, run.checkpointBusy, run.logFrames, run.checkpointed, sameGeneration, integrity, match.Step, match.Exact)
	x.set.Check("B", integrity == "ok" && match.Step == run.lastAck, fmt.Sprintf("%s: a process crash keeps every acknowledged step (%d)", v, run.lastAck))

	rule := ruleCut(level, run)
	cut, hasCut, outside := rule.cut, rule.hasCut, rule.outside
	var syncedUpTo, notApplicable any
	if hasCut {
		syncedUpTo = cut
	}
	if outside != "" {
		notApplicable = outside
	}
	x.tables.rules.Row(v.dirName(), syncedUpTo, rule.databaseUnsynced, notApplicable)
	if result != nil {
		real, err := filepath.EvalSymlinks(db)
		if err != nil {
			x.lab.Fatal("path", err)
		}
		walSyncs := trace.SyncsOf(result.Log.Calls, real+"-wal")
		dbSyncs := trace.SyncsOf(result.Log.Calls, real)
		x.syncRow(v, "-wal", walSyncs)
		x.syncRow(v, "database file", dbSyncs)
		x.set.Check("B", walSyncs.HasUnsynced == hasCut && (!hasCut || walSyncs.FirstUnsynced == cut) && dbSyncs.HasUnsynced == rule.databaseUnsynced,
			fmt.Sprintf("%s: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced", v))
	} else {
		x.linuxOnly("B", v.dirName(), "the strace confirmation of the synced bytes (the model below uses the rule read from wal.c and pager.c)")
	}
	if outside != "" {
		x.set.Check("B", level == "OFF", "only synchronous=OFF leaves database-file writes that were never synced")
		return result
	}
	walData, err := os.ReadFile(db + "-wal")
	if err != nil {
		x.lab.Fatal("read wal", err)
	}
	keep := int64(len(walData))
	if hasCut {
		keep = cut
	}
	lost := 0
	for k := baseStep + 1; k <= run.lastAck; k++ {
		if run.generation[k] == run.generation[run.lastAck] && run.commitEnd[k] > keep {
			lost++
		}
	}
	power := filepath.Join(dir, "power", "r.db")
	if err := errors.Join(copyFile(db, power, -1), copyFile(db+"-wal", power+"-wal", keep)); err != nil {
		x.lab.Fatal("copy", err)
	}
	integrity, match = x.recover(power)
	x.cuts++
	x.tables.powerCuts.Row(x.cuts, v.dirName(), "as written", keep, len(walData), lost, integrity, match.Step, match.Exact)
	x.set.Check("B", integrity == "ok" && match.Step == run.lastAck-lost, fmt.Sprintf("%s: the model recovers step %d, the last acknowledged step minus the %d commit(s) written after the last sync", v, run.lastAck-lost, lost))
	if rule.databaseUnsynced {
		before := filepath.Join(dir, "power-before-checkpoint", "r.db")
		if err := errors.Join(copyFile(db+beforeCheckpointSuffix, before, -1), copyFile(db+"-wal", before+"-wal", keep)); err != nil {
			x.lab.Fatal("copy", err)
		}
		beforeIntegrity, beforeMatch := x.recover(before)
		x.cuts++
		x.tables.powerCuts.Row(x.cuts, v.dirName(), "as before the checkpoint", keep, len(walData), lost, beforeIntegrity, beforeMatch.Step, beforeMatch.Exact)
		x.set.Check("B", beforeIntegrity == "ok" && beforeMatch.Step == match.Step,
			fmt.Sprintf("%s: with or without the checkpoint's unsynced database writes the model recovers step %d, because the WAL frames they came from were synced before the checkpoint wrote them", v, match.Step))
	}
	if level == "NORMAL" {
		x.set.Check("B", lost > 0 && match.Step < run.lastAck && match.Step == checkpointAfter, fmt.Sprintf("%s: the acknowledged commits after the checkpoint at step %d are lost in the model, yet the database is consistent", v, checkpointAfter))
	}
	if level == "FULL" {
		x.set.Check("B", lost == 0 && match.Step == run.lastAck, "FULL: no acknowledged commit is lost in the model")
	}
	return result
}

func (x *lab) syncRow(v variant, file string, r trace.SyncRecord) {
	var lowest any
	if r.HasUnsynced {
		lowest = r.FirstUnsynced
	}
	x.tables.syncs.Row(v.dirName(), file, r.Syncs, len(r.Unsynced), lowest)
}

// writerThread identifies the OS thread (tid) that executed the writer's
// pwrite64 calls so killSweep counts syscalls on the LockOSThread-pinned writer.
func writerThread(result *trace.Result) int {
	counts := map[int]int{}
	for _, call := range result.Log.Calls {
		if call.Name == "pwrite64" {
			counts[call.PID]++
		}
	}
	best, most := 0, 0
	for pid, n := range counts {
		if n > most {
			best, most = pid, n
		}
	}
	return best
}

// killSweep kills a fresh writer child via strace --inject=SIGKILL on entry to
// every sync and six evenly spaced pwrite64 calls, then reopens the database to
// verify walIndexRecover (src/wal.c:1390-1614) always yields an exact recipe
// step equal to lastAck or lastAck+1 (the commit whose frames were already in
// the kernel page cache when its fsync was interrupted).
func (x *lab) killSweep(level string, uninterrupted *trace.Result) {
	tid := writerThread(uninterrupted)
	syncs, writes := 0, 0
	for _, call := range uninterrupted.Log.Calls {
		if call.PID != tid {
			continue
		}
		switch call.Name {
		case "fsync", "fdatasync":
			syncs++
		case "pwrite64":
			writes++
		}
	}
	x.tables.killPlans.Row(level, syncs, writes, pwriteKills)
	var points []trace.KillAt
	for i := 1; i <= syncs; i++ {
		points = append(points, trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: i})
	}
	for j := 0; j < pwriteKills; j++ {
		points = append(points, trace.KillAt{Syscalls: []string{"pwrite64"}, When: 1 + j*(writes-1)/(pwriteKills-1)})
	}
	for i, point := range points {
		dir := filepath.Join(x.dir, fmt.Sprintf("kill-%s-%02d", strings.ToLower(level), i))
		stdout, result := x.runWriter(dir, variant{level, 0}, &point)
		run, err := parseRun(stdout)
		if err != nil {
			x.lab.Fatal("parse", err)
		}
		killed := "nothing (the run finished)"
		for _, call := range trace.RelativeTo(dir, result.Log.Calls) {
			if call.Unfinished {
				killed = call.String()
			}
		}
		integrity, match := x.recover(filepath.Join(dir, "r.db"))
		x.tables.kills.Row(level, i+1, point.Syscalls[0], point.When, killed, result.Killed, run.lastAck, match.Step, match.Exact, integrity)
		x.set.Check("A", result.Killed == "SIGKILL" && integrity == "ok" && match.Found() && match.Step >= run.lastAck && match.Step <= run.lastAck+1,
			fmt.Sprintf("%s kill %d: integrity ok, recovered step %d is the last acknowledged step or the one being committed", level, i+1, match.Step))
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == childMode {
		if err := runChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	portable := flag.Bool("portable", false, "run the path used where strace is missing, even on Linux")
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	x := &lab{traced: trace.Supported && !*portable, ctx: ctx, lab: procs.NewLab(os.Stdout), r: recipe.Default(), dir: dir, base: filepath.Join(dir, "base", "r.db"), set: results.MustOpen(dir)}
	x.openTables()
	if err := os.MkdirAll(filepath.Dir(x.base), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(ctx, x.base, baseStep); err != nil {
		x.lab.Fatal("build", err)
	}
	// Part B: what a process crash keeps, and what the simplest power cut keeps.
	traces := map[variant]*trace.Result{}
	for i, v := range modelVariants {
		traces[v] = x.model(i+1, v)
	}
	// Part A: kill the writer at chosen syscalls, reopen, identify the state.
	if !x.traced {
		x.linuxOnly("A", nil, "killing the writer at a chosen syscall with strace")
	} else {
		for _, level := range levels {
			x.killSweep(level, traces[variant{level, 0}])
		}
	}
	x.set.Finish("CRASH VERSUS POWER LOSS")
}

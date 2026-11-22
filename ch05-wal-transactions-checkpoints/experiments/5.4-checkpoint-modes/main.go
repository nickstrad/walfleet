// Experiment 5.4 (checkpoint-modes) runs PASSIVE, FULL, RESTART, and TRUNCATE
// checkpoints against three reader states (no reader, reader behind, reader
// current), records (busy, log, checkpointed), WAL size, and salt changes
// right after the checkpoint and after the next write (elapsed times appear on
// INFO lines only), and contrasts TRUNCATE run by a third connection against
// TRUNCATE run by the writer itself (src/wal.c:2199-2395, 3875-3915,
// 4088-4098, 4301-4432).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.4
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	templateStep       = 5
	firstCommit        = 6
	secondCommit       = 7
	nextWrite          = 8
	checkpointerBusyMs = 300
)

type readerPosition int

const (
	noReader readerPosition = iota
	readerBehind
	readerCurrent
)

func (p readerPosition) String() string {
	switch p {
	case readerBehind:
		return "reader behind"
	case readerCurrent:
		return "reader current"
	}
	return "no reader"
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.4" runs both.
// part labels the claims: A is the mode and reader matrix, B the writer's own TRUNCATE.
var (
	set    *results.Set
	part   string
	tables struct {
		cases, nextWrites, ownTruncate, infoElapsed *results.Table
	}
)

func openTables() {
	tables.cases = set.Table("cases", "seq", "mode", "reader", "frames_before", "reader_frame", "busy", "log_frames", "checkpointed", "wal_bytes", "mx_frame", "n_backfill", "file_salt1", "shm_salt1")
	tables.nextWrites = set.Table("next_writes", "seq", "mode", "reader", "restarted", "file_salt1", "checkpoint_seq", "wal_bytes", "mx_frame", "valid_frames", "frames_in_file")
	tables.ownTruncate = set.Table("own_truncate", "seq", "moment", "busy", "log_frames", "checkpointed", "wal_bytes", "mx_frame", "shm_salt1", "file_salt1", "checkpoint_seq_before", "checkpoint_seq_after")
	tables.infoElapsed = set.Table("info_elapsed", "seq", "mode", "reader", "elapsed_ms")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func must[T any](value T, err error) func(string) T {
	return func(what string) T {
		if err != nil {
			fatal(what, err)
		}
		return value
	}
}

type sample struct {
	walSize       int64
	walFrames     int
	walValid      int
	walSalt1      uint32
	walSalt2      uint32
	walSequence   uint32
	indexMaxFrame uint32
	indexBackfill uint32
	indexSalt1    uint32
}

func take(ctx context.Context, path, scratch string) sample {
	state := must(walfile.ReadLiveState(ctx, path, scratch))("live state")
	if !state.WALPresent || !state.IndexPresent {
		fatal("live state", fmt.Errorf("WAL present %v, wal-index present %v", state.WALPresent, state.IndexPresent))
	}
	s := sample{walSize: state.WALSize}
	if state.WAL != nil {
		s.walFrames = len(state.WAL.Frames)
		s.walValid = state.WAL.ValidFrames
		s.walSalt1 = state.WAL.Header.Salt1
		s.walSalt2 = state.WAL.Header.Salt2
		s.walSequence = state.WAL.Header.CheckpointSequence
	}
	h := state.Index.Header()
	s.indexMaxFrame = h.MaxFrame
	s.indexBackfill = state.Index.Checkpoint.Backfill
	s.indexSalt1 = h.Salt1
	return s
}

type outcome struct {
	mode        string
	reader      readerPosition
	before      sample
	result      sqlitex.CheckpointResult
	elapsed     time.Duration
	after       sample
	next        sample
	readerFrame uint32
}

func open(ctx context.Context, path string, immediate bool, busy time.Duration) *sqlitex.DB {
	options := recipe.Default().SQLiteOptions()
	options.ImmediateTx = immediate
	options.Conn.WALAutocheckpoint = 0
	options.Conn.BusyTimeout = busy
	return must(sqlitex.Open(ctx, path, options))("open")
}

func beginReader(ctx context.Context, path string) (*sqlitex.DB, *sql.Tx) {
	db := open(ctx, path, false, 5*time.Second)
	tx := must(db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}))("begin read")
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		fatal("read", err)
	}
	return db, tx
}

// runCase executes one (mode, readerPosition) cell on a fresh copy of the
// step-5 template: writer commits steps 6 and 7, a third connection with a
// 300 ms busy_timeout runs the checkpoint, then the reader closes and writer
// commits step 8 to reveal whether the WAL restarted and how its salts changed.
func runCase(ctx context.Context, template, dir, mode string, reader readerPosition) outcome {
	name := strings.ToLower(mode) + "-" + strings.ReplaceAll(reader.String(), " ", "-")
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := os.WriteFile(path, must(os.ReadFile(template))("read template"), 0o644); err != nil {
		fatal("copy template", err)
	}
	r := recipe.Default()
	writer := open(ctx, path, true, 5*time.Second)
	var readerDB *sqlitex.DB
	var readerTx *sql.Tx
	o := outcome{mode: mode, reader: reader}
	if err := r.ApplyStep(ctx, writer, firstCommit); err != nil {
		fatal("step", err)
	}
	if reader == readerBehind {
		readerDB, readerTx = beginReader(ctx, path)
		o.readerFrame = take(ctx, path, root).indexMaxFrame
	}
	if err := r.ApplyStep(ctx, writer, secondCommit); err != nil {
		fatal("step", err)
	}
	if reader == readerCurrent {
		readerDB, readerTx = beginReader(ctx, path)
	}
	o.before = take(ctx, path, root)
	checkpointer := open(ctx, path, true, checkpointerBusyMs*time.Millisecond)
	start := time.Now()
	o.result = must(sqlitex.Checkpoint(ctx, checkpointer, "", mode))("checkpoint " + mode)
	o.elapsed = time.Since(start)
	o.after = take(ctx, path, root)
	if readerTx != nil {
		if err := readerTx.Rollback(); err != nil {
			fatal("end read", err)
		}
		if err := readerDB.Close(); err != nil {
			fatal("close reader", err)
		}
	}
	if err := checkpointer.Close(); err != nil {
		fatal("close checkpointer", err)
	}
	if err := r.ApplyStep(ctx, writer, nextWrite); err != nil {
		fatal("step", err)
	}
	o.next = take(ctx, path, root)
	var integrity string
	if err := writer.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		fatal("integrity_check", fmt.Errorf("%s %v", integrity, err))
	}
	if err := writer.Close(); err != nil {
		fatal("close writer", err)
	}
	return o
}

func delta(before, after uint32) string {
	switch after {
	case before:
		return "same"
	case before + 1:
		return "+1"
	}
	return "new value"
}

func saltDelta(before, after sample) string {
	if after.walSize < walfile.HeaderSize {
		return "(no header)"
	}
	return delta(before.walSalt1, after.walSalt1)
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.4")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	template := filepath.Join(dir, "template", "r.db")
	if err := os.MkdirAll(filepath.Dir(template), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.Default().Build(ctx, template, templateStep); err != nil {
		fatal("build", err)
	}
	part = "A"
	set = results.MustOpen(dir)
	openTables()
	modes := []string{sqlitex.CheckpointPassive, sqlitex.CheckpointFull, sqlitex.CheckpointRestart, sqlitex.CheckpointTruncate}
	readers := []readerPosition{noReader, readerBehind, readerCurrent}
	var outcomes []outcome
	for _, mode := range modes {
		for _, reader := range readers {
			outcomes = append(outcomes, runCase(ctx, template, dir, mode, reader))
		}
	}
	for i, o := range outcomes {
		var readerFrame any
		if o.reader == readerBehind {
			readerFrame = o.readerFrame
		}
		tables.cases.Row(i+1, o.mode, o.reader, o.before.indexMaxFrame, readerFrame, o.result.Busy, o.result.LogFrames, o.result.Checkpointed,
			o.after.walSize, o.after.indexMaxFrame, o.after.indexBackfill, saltDelta(o.before, o.after), delta(o.before.indexSalt1, o.after.indexSalt1))
		tables.nextWrites.Row(i+1, o.mode, o.reader, o.next.indexMaxFrame < o.before.indexMaxFrame, saltDelta(o.before, o.next),
			delta(o.before.walSequence, o.next.walSequence), o.next.walSize, o.next.indexMaxFrame, o.next.walValid, o.next.walFrames)
		tables.infoElapsed.Row(i+1, o.mode, o.reader, o.elapsed.Milliseconds())
	}
	n := outcomes[0].before.indexMaxFrame
	behind := outcomes[1].readerFrame
	byKey := map[string]outcome{}
	for _, o := range outcomes {
		byKey[o.mode+"/"+o.reader.String()] = o
	}
	res := func(mode string, reader readerPosition) sqlitex.CheckpointResult {
		return byKey[mode+"/"+reader.String()].result
	}
	full := sqlitex.CheckpointResult{LogFrames: int(n), Checkpointed: int(n)}
	partial := sqlitex.CheckpointResult{LogFrames: int(n), Checkpointed: int(behind)}
	partialBusy := partial
	partialBusy.Busy = true
	fullBusy := full
	fullBusy.Busy = true
	check(res("PASSIVE", noReader) == full && res("FULL", noReader) == full && res("RESTART", noReader) == full,
		"with no reader, PASSIVE, FULL and RESTART all copy every frame and report busy=0")
	check(res("TRUNCATE", noReader) == sqlitex.CheckpointResult{} && byKey["TRUNCATE/no reader"].after.walSize == 0,
		"TRUNCATE with no reader reports 0/0/0 and leaves a zero-length -wal")
	check(res("PASSIVE", readerBehind) == partial, "PASSIVE with a reader behind copies up to the reader's frame and still reports busy=0")
	check(res("FULL", readerBehind) == partialBusy && res("RESTART", readerBehind) == partialBusy && res("TRUNCATE", readerBehind) == partialBusy,
		"FULL, RESTART and TRUNCATE with a reader behind copy the same frames, then report busy=1")
	check(res("PASSIVE", readerCurrent) == full && res("FULL", readerCurrent) == full,
		"a reader whose snapshot is the end of the WAL does not limit PASSIVE or FULL")
	check(res("RESTART", readerCurrent) == fullBusy && res("TRUNCATE", readerCurrent) == fullBusy,
		"RESTART and TRUNCATE copy everything but report busy=1 while any reader is still using the WAL")
	truncated := byKey["TRUNCATE/no reader"]
	check(truncated.after.indexSalt1 == truncated.before.indexSalt1+1 && truncated.after.indexMaxFrame == 0,
		"TRUNCATE restarts the wal-index at once: shm salt-1 + 1, mxFrame 0")
	sameHeader := true
	for _, mode := range []string{"PASSIVE", "FULL", "RESTART"} {
		o := byKey[mode+"/no reader"]
		sameHeader = sameHeader && o.after.walSalt1 == o.before.walSalt1 && o.after.indexSalt1 == o.before.indexSalt1 && o.after.walSize == o.before.walSize
	}
	check(sameHeader, "PASSIVE, FULL and RESTART change neither salt nor the WAL size: the restart happens at the next write")
	restartedOK := true
	for _, o := range outcomes {
		restarted := o.next.indexMaxFrame < o.before.indexMaxFrame
		wantRestart := o.result.Checkpointed == int(n) || (o.mode == "TRUNCATE" && !o.result.Busy)
		restartedOK = restartedOK && restarted == wantRestart
		if !restarted {
			restartedOK = restartedOK && o.next.walSalt1 == o.before.walSalt1 && o.next.walSequence == o.before.walSequence
		}
	}
	check(restartedOK, "the next write restarts the WAL exactly when the checkpoint had copied every frame (or TRUNCATE emptied it); otherwise it appends with the same salts")
	writerRestartOK := true
	for _, o := range outcomes {
		if o.next.indexMaxFrame < o.before.indexMaxFrame && o.mode != "TRUNCATE" || o.mode == "TRUNCATE" && o.reader == readerCurrent {
			writerRestartOK = writerRestartOK && o.next.walSalt1 == o.before.walSalt1+1 && o.next.walSequence == o.before.walSequence+1
		}
	}
	check(writerRestartOK, "when the writer itself restarts the WAL at its next write, the new header has salt-1 + 1 and checkpoint sequence + 1")
	// Check whether the writer's next commit restarted the WAL: walFrames calls
	// walRestartLog (src/wal.c:3879-3896), which resets mxFrame to 0 whenever
	// nBackfill == mxFrame and no reader holds locks 1..4. When another
	// connection already ran TRUNCATE, the writer's own pWal->nCkpt is still 0,
	// so walFrames generates fresh random salts instead of salt-1 + 1
	// (src/wal.c:4095-4096).
	check(truncated.next.walSalt1 != truncated.before.walSalt1 && truncated.next.walSalt1 != truncated.before.walSalt1+1 && truncated.next.walSequence == truncated.before.walSequence,
		"after another connection's TRUNCATE, the writer's next WAL header has new random salts and the writer's own checkpoint sequence (0): the sequence is per connection")
	ownTruncate(ctx, template, dir)
	set.Finish("CHECKPOINT MODES")
}

// ownTruncate repeats TRUNCATE/no-reader on the writer connection itself: its
// walRestartHdr call increments pWal->nCkpt to 1 (src/wal.c:2156), so its next
// commit preserves salt-1 + 1 and writes checkpoint sequence 1 instead of
// re-randomizing salts.
func ownTruncate(ctx context.Context, template, dir string) {
	part = "B"
	root := filepath.Join(dir, "truncate-by-writer")
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := os.WriteFile(path, must(os.ReadFile(template))("read template"), 0o644); err != nil {
		fatal("copy template", err)
	}
	r := recipe.Default()
	writer := open(ctx, path, true, 5*time.Second)
	for _, k := range []int{firstCommit, secondCommit} {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
	}
	before := take(ctx, path, root)
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointTruncate))("checkpoint")
	after := take(ctx, path, root)
	if err := r.ApplyStep(ctx, writer, nextWrite); err != nil {
		fatal("step", err)
	}
	next := take(ctx, path, root)
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
	tables.ownTruncate.Row(1, "right after wal_checkpoint(TRUNCATE)", result.Busy, result.LogFrames, result.Checkpointed, after.walSize, after.indexMaxFrame,
		delta(before.indexSalt1, after.indexSalt1), nil, nil, nil)
	tables.ownTruncate.Row(2, fmt.Sprintf("after the next write (step %d)", nextWrite), nil, nil, nil, next.walSize, next.indexMaxFrame,
		nil, saltDelta(before, next), before.walSequence, next.walSequence)
	check(next.walSalt1 == before.walSalt1+1 && next.walSequence == before.walSequence+1,
		"when the connection that ran TRUNCATE writes next, the new WAL header has salt-1 + 1 and checkpoint sequence + 1")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}

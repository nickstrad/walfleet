// Experiment 5.3 (reader-pins-the-wal) demonstrates both ways an open read
// transaction stops checkpoints and forces unbounded WAL growth: part A holds
// a read mark in slot 1..4 so nBackfill stops at the reader's mark and the WAL
// cannot restart; part B begins on a fully checkpointed WAL so the reader holds
// read lock 0 (WAL_READ_LOCK(0)), letting the WAL restart once while blocking
// all subsequent backfill (nBackfill stays 0).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.3
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	baseStep           = 5
	readerStartsAfter  = 6
	lastLoopStep       = 30
	nextWriteStep      = 31
	autocheckpointSize = 200
	countQuery         = "SELECT count(*) FROM events"
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.3" runs both.
// part labels the rows written while a part runs; the first sample of a part is
// the baseline of its salt-1 column, because the salts themselves are random.
var (
	set             *results.Set
	part            string
	seq             int
	baseSalt        uint32
	haveBase        bool
	sampleTable     *results.Table
	checkpointTable *results.Table
)

func openTables() {
	sampleTable = set.Table("samples", "seq", "part", "moment", "step", "wal_bytes", "mx_frame", "n_backfill", "read_marks", "reader_rows", "writer_rows", "checkpoint_seq", "salt1_advance", "valid_frames", "frames_in_file")
	checkpointTable = set.Table("checkpoints", "seq", "part", "moment", "busy", "log_frames", "checkpointed")
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
	walSize    int64
	maxFrame   uint32
	backfill   uint32
	marks      [walfile.ReadMarkCount]uint32
	salt1      uint32
	sequence   uint32
	validCount int
	frames     int
}

func take(ctx context.Context, path, scratch string) sample {
	state := must(walfile.ReadLiveState(ctx, path, scratch))("live state")
	if !state.WALPresent || !state.IndexPresent || state.WAL == nil {
		fatal("live state", fmt.Errorf("WAL present %v, wal-index present %v, problem %q", state.WALPresent, state.IndexPresent, state.WALProblem))
	}
	h := state.Index.Header()
	return sample{
		walSize:    state.WALSize,
		maxFrame:   h.MaxFrame,
		backfill:   state.Index.Checkpoint.Backfill,
		marks:      state.Index.Checkpoint.ReadMarks,
		salt1:      state.WAL.Header.Salt1,
		sequence:   state.WAL.Header.CheckpointSequence,
		validCount: state.WAL.ValidFrames,
		frames:     len(state.WAL.Frames),
	}
}

func marksText(marks [walfile.ReadMarkCount]uint32) string {
	parts := make([]string, len(marks))
	for i, m := range marks {
		parts[i] = walfile.ReadMarkText(m)
	}
	return strings.Join(parts, ",")
}

func count(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	var n int
	if err := q.QueryRowContext(ctx, countQuery).Scan(&n); err != nil {
		fatal("count", err)
	}
	return n
}

func hasMark(marks [walfile.ReadMarkCount]uint32, mark uint32) bool {
	for i := 1; i < len(marks); i++ {
		if marks[i] == mark {
			return true
		}
	}
	return false
}

func openWriter(ctx context.Context, path string) *sqlitex.DB {
	options := recipe.Default().SQLiteOptions()
	options.Conn.WALAutocheckpoint = autocheckpointSize
	return must(sqlitex.Open(ctx, path, options))("open writer")
}

// openReader starts a deferred read-only transaction and executes countQuery so
// SQLite actually calls walTryBeginRead (src/wal.c:3020-3272) and acquires a
// read lock; BEGIN DEFERRED alone takes no lock until the first statement.
func openReader(ctx context.Context, path string) (*sqlitex.DB, *sql.Tx, int) {
	options := recipe.Default().SQLiteOptions()
	options.ImmediateTx = false
	options.Conn.WALAutocheckpoint = 0
	reader := must(sqlitex.Open(ctx, path, options))("open reader")
	tx := must(reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}))("begin read")
	return reader, tx, count(ctx, tx)
}

// row records one sample of the live WAL and wal-index. step is the recipe
// step just committed (nil when none); a reader or writer count of -1 is not
// taken at that moment.
func row(moment string, step any, s sample, readerRows, writerRows int) {
	if !haveBase {
		baseSalt, haveBase = s.salt1, true
	}
	seq++
	sampleTable.Row(seq, part, moment, step, s.walSize, s.maxFrame, s.backfill, marksText(s.marks), rowsValue(readerRows), rowsValue(writerRows), s.sequence, s.salt1-baseSalt, s.validCount, s.frames)
}

func rowsValue(rows int) any {
	if rows < 0 {
		return nil
	}
	return rows
}

func checkpointRow(moment string, result sqlitex.CheckpointResult) {
	seq++
	checkpointTable.Row(seq, part, moment, result.Busy, result.LogFrames, result.Checkpointed)
}

func prepare(ctx context.Context, dir, name string) (string, string) {
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := recipe.Default().Build(ctx, path, baseStep); err != nil {
		fatal("build", err)
	}
	return path, root
}

func partA(ctx context.Context, dir string) {
	part, haveBase = "A", false
	r := recipe.Default().WithSteps(nextWriteStep)
	path, scratch := prepare(ctx, dir, "a-pinned")
	writer := openWriter(ctx, path)
	if err := r.ApplyStep(ctx, writer, readerStartsAfter); err != nil {
		fatal("step", err)
	}
	// Do not query writer before openReader: a SELECT on writer would run
	// walTryBeginRead (src/wal.c:3182-3205) and write aReadMark[1] = 22 before
	// the reader opens. Sampling after each writer SELECT inside the loop keeps
	// every row's read marks aligned with that step's state.
	start := take(ctx, path, scratch)
	row(fmt.Sprintf("after step %d", readerStartsAfter), readerStartsAfter, start, -1, -1)
	reader, tx, readerRows := openReader(ctx, path)
	writerRows := count(ctx, writer)
	pinned := take(ctx, path, scratch)
	row("reader began", nil, pinned, readerRows, writerRows)
	snapshot := pinned.maxFrame
	check(snapshot == start.maxFrame && !hasMark(start.marks, snapshot) && hasMark(pinned.marks, snapshot) && pinned.backfill < snapshot,
		"the reader's snapshot is a read mark equal to mxFrame when it began, and that WAL content was not yet checkpointed")
	samples := []sample{pinned}
	readerStable, marksHeld, backfillBounded := true, true, true
	firstAutocheckpointed := -1
	for k := readerStartsAfter + 1; k <= lastLoopStep; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
		writerRows := count(ctx, writer)
		s := take(ctx, path, scratch)
		rows := count(ctx, tx)
		row(fmt.Sprintf("step %d committed", k), k, s, rows, writerRows)
		readerStable = readerStable && rows == readerRows
		marksHeld = marksHeld && hasMark(s.marks, snapshot)
		backfillBounded = backfillBounded && s.backfill <= snapshot
		if firstAutocheckpointed < 0 && s.backfill == snapshot {
			firstAutocheckpointed = k
		}
		samples = append(samples, s)
	}
	growing, neverRestarted := true, true
	for i := 1; i < len(samples); i++ {
		growing = growing && samples[i].walSize > samples[i-1].walSize
		neverRestarted = neverRestarted && samples[i].maxFrame > samples[i-1].maxFrame && samples[i].salt1 == pinned.salt1
	}
	last := samples[len(samples)-1]
	check(readerStable, "the reader's row count never changed while the writer committed")
	check(growing && neverRestarted, "the WAL grew on every commit and never restarted while the reader stayed open")
	check(marksHeld && backfillBounded && firstAutocheckpointed > 0 && last.backfill == snapshot,
		"checkpoints ran (autocheckpoint) but nBackfill stopped at the reader's mark and stayed there")

	// Ending the read transaction releases its shared lock on WAL_READ_LOCK(1)
	// but leaves its mark number written in aReadMark[1] until a checkpoint or
	// later reader updates that slot (src/wal.c:2241-2242, 3196).
	if err := tx.Rollback(); err != nil {
		fatal("end read", err)
	}
	if err := reader.Close(); err != nil {
		fatal("close reader", err)
	}
	writerRows = count(ctx, writer)
	ended := take(ctx, path, scratch)
	row("reader ended", nil, ended, -1, writerRows)
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	writerRows = count(ctx, writer)
	afterCheckpoint := take(ctx, path, scratch)
	row("PASSIVE checkpoint", nil, afterCheckpoint, -1, writerRows)
	checkpointRow("PASSIVE checkpoint", result)
	check(!result.Busy && afterCheckpoint.backfill == afterCheckpoint.maxFrame && afterCheckpoint.walSize == last.walSize,
		"with the reader gone one checkpoint brings nBackfill to mxFrame; the WAL file keeps its size")
	if err := r.ApplyStep(ctx, writer, nextWriteStep); err != nil {
		fatal("step", err)
	}
	writerRows = count(ctx, writer)
	restarted := take(ctx, path, scratch)
	row(fmt.Sprintf("step %d committed", nextWriteStep), nextWriteStep, restarted, -1, writerRows)
	check(restarted.maxFrame < last.maxFrame && restarted.validCount == int(restarted.maxFrame) && restarted.salt1 == pinned.salt1+1 && restarted.sequence == pinned.sequence+1 && restarted.walSize == last.walSize,
		"the next write restarted the WAL at frame 1 (salt-1 + 1, checkpoint sequence + 1); the file is reused, not shrunk")
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
}

func partB(ctx context.Context, dir string) {
	part, haveBase = "B", false
	r := recipe.Default()
	path, scratch := prepare(ctx, dir, "b-mark-zero")
	writer := openWriter(ctx, path)
	if err := r.ApplyStep(ctx, writer, readerStartsAfter); err != nil {
		fatal("step", err)
	}
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	before := take(ctx, path, scratch)
	row("checkpointed", readerStartsAfter, before, -1, -1)
	checkpointRow(fmt.Sprintf("PASSIVE checkpoint after step %d", readerStartsAfter), result)
	// With nBackfill == mxFrame, walTryBeginRead takes WAL_READ_LOCK(0) shared
	// instead of setting a mark in slots 1..4 (src/wal.c:3134-3163). The writer
	// can restart the WAL because walRestartLog only locks slots 1..4
	// (src/wal.c:3885), but walCheckpoint cannot backfill a single page because
	// it must take WAL_READ_LOCK(0) exclusively first (src/wal.c:2260).
	reader, tx, readerRows := openReader(ctx, path)
	writerRows := count(ctx, writer)
	began := take(ctx, path, scratch)
	row("reader began", nil, began, readerRows, writerRows)
	check(began.backfill == began.maxFrame && began.marks == before.marks, "the WAL was fully checkpointed, so the reader took no read mark in 1..4 (it holds read lock 0 and ignores the WAL)")
	readerStable, backfillZero := true, true
	var samples []sample
	for k := readerStartsAfter + 1; k <= lastLoopStep; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
		writerRows := count(ctx, writer)
		s := take(ctx, path, scratch)
		rows := count(ctx, tx)
		row(fmt.Sprintf("step %d committed", k), k, s, rows, writerRows)
		readerStable = readerStable && rows == readerRows
		backfillZero = backfillZero && s.backfill == 0
		samples = append(samples, s)
	}
	first, last := samples[0], samples[len(samples)-1]
	check(first.salt1 == before.salt1+1 && first.maxFrame < before.maxFrame, "a reader on read lock 0 does not stop the writer from restarting the WAL")
	check(readerStable, "the reader's row count never changed")
	check(backfillZero && last.maxFrame >= autocheckpointSize, "but no checkpoint can copy a single frame while it is open: nBackfill stays 0 although autocheckpoint ran on every commit past the threshold")
	if err := tx.Rollback(); err != nil {
		fatal("end read", err)
	}
	if err := reader.Close(); err != nil {
		fatal("close reader", err)
	}
	result = must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	writerRows = count(ctx, writer)
	after := take(ctx, path, scratch)
	row("reader ended + PASSIVE", nil, after, -1, writerRows)
	checkpointRow("PASSIVE checkpoint after the reader ended", result)
	check(after.backfill == after.maxFrame, "once the reader ends, a checkpoint copies everything")
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.3")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	partA(ctx, dir)
	partB(ctx, dir)
	set.Finish("READER PINS THE WAL")
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

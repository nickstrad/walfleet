// Experiment 5.2 (wal-replay, rung R3) proves that a base database file plus
// the committed frames of its WAL equals the complete database state. It
// replays copies taken at a quiet moment, mid-transaction with spilled pages,
// and after a rollback in plain Go (no SQLite), and proves E1 byte equality
// against SQLite's own TRUNCATE checkpoint and recovery (both without -shm and
// with a stale -shm from another database).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.2
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	quietSteps        = 8
	spillBaseStep     = 8
	spillCommitted    = 10
	spillCacheSize    = 10
	spillExtraUpdate  = "UPDATE documents SET title = upper(title)"
	databaseName      = "r.db"
	replayOutputName  = "replayed.db"
	recoveryDirectory = "sqlite-recovery"
	quietTotalFrames  = 155
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.2" runs both.
// part labels the rows written while a part runs; seq orders rows across tables.
var (
	set    *results.Set
	part   string
	seq    int
	tables struct {
		captures, replays, sqliteRuns, comparisons, recipeMatches, walStates, staleShm *results.Table
	}
)

func openTables() {
	tables.captures = set.Table("captures", "seq", "part", "capture", "db_bytes", "wal_bytes")
	tables.replays = set.Table("replays", "seq", "part", "capture", "frames_in_file", "valid_frames", "transactions", "last_commit_frame", "database_pages", "applied_frames", "distinct_pages", "skipped_beyond_size", "base_bytes", "output_bytes", "ignored_frames", "ignored", "partial_bytes", "wal_ignored_reason")
	tables.sqliteRuns = set.Table("sqlite_runs", "seq", "part", "run", "shm", "user_version", "busy", "log_frames", "checkpointed")
	tables.comparisons = set.Table("comparisons", "seq", "part", "against", "level", "equal", "sha256", "user_version", "integrity_check", "unchanged_by_open")
	tables.recipeMatches = set.Table("recipe_matches", "seq", "part", "subject", "step", "exact")
	tables.walStates = set.Table("wal_states", "seq", "part", "moment", "wal_bytes", "frames", "valid_frames", "last_commit_frame", "mx_frame")
	tables.staleShm = set.Table("stale_shm", "mx_frame", "n_page")
}

func nextSeq() int {
	seq++
	return seq
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

func mkdir(path string) string {
	if err := os.MkdirAll(path, 0o755); err != nil {
		fatal("mkdir", err)
	}
	return path
}

func liveCopy(ctx context.Context, path, directory, capture string) {
	mkdir(directory)
	results := must(walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
		{From: path, To: filepath.Join(directory, databaseName)},
		{From: path + "-wal", To: filepath.Join(directory, databaseName+"-wal")},
	}))("live copy")
	for _, r := range results {
		if !r.Present {
			fatal("live copy", fmt.Errorf("%s missing", filepath.Base(r.From)))
		}
	}
	tables.captures.Row(nextSeq(), part, capture, results[0].Bytes, results[1].Bytes)
}

func replayCopy(directory, capture string) (string, walfile.ReplayReport) {
	output := filepath.Join(directory, replayOutputName)
	report := must(walfile.ReplayFiles(filepath.Join(directory, databaseName), filepath.Join(directory, databaseName+"-wal"), output))("replay")
	ignored := make([]string, len(report.Ignored))
	for i, g := range report.Ignored {
		ignored[i] = g.String()
	}
	tables.replays.Row(nextSeq(), part, capture, report.FramesInFile, report.ValidFrames, report.Transactions, report.LastCommitFrame, report.DatabasePages,
		report.AppliedFrames, report.DistinctPages, report.SkippedBeyondSize, report.BaseBytes, report.OutputBytes, report.IgnoredFrameCount(), strings.Join(ignored, "; "),
		report.PartialFrameBytes, report.WALIgnoredReason)
	return output, report
}

// sqliteRecoveryOfCopy copies the captured base and -wal (and optionally a
// stale -shm from another database) into a fresh directory, lets SQLite open
// and recover it, checkpoints with TRUNCATE, and returns the resulting path.
func sqliteRecoveryOfCopy(ctx context.Context, directory, name, staleIndex string) string {
	target := mkdir(filepath.Join(directory, name))
	path := filepath.Join(target, databaseName)
	for _, suffix := range []string{"", "-wal"} {
		data := must(os.ReadFile(filepath.Join(directory, databaseName+suffix)))("read copy")
		if err := os.WriteFile(path+suffix, data, 0o644); err != nil {
			fatal("write", err)
		}
	}
	if staleIndex != "" {
		if err := os.WriteFile(path+"-shm", must(os.ReadFile(staleIndex))("read stale -shm"), 0o644); err != nil {
			fatal("write", err)
		}
	}
	db := must(recipe.Default().Open(ctx, path))("open copy with SQLite")
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("user_version", err)
	}
	result := must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate))("checkpoint copy")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	shm := "none"
	if staleIndex != "" {
		shm = "stale, from part A"
	}
	tables.sqliteRuns.Row(nextSeq(), part, "opened a copy, then TRUNCATE checkpoint", shm, version, result.Busy, result.LogFrames, result.Checkpointed)
	return path
}

func userVersion(ctx context.Context, path string) (int, string) {
	db := must(recipe.Default().Open(ctx, path))("open " + filepath.Base(path))
	var version int
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("user_version", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return version, integrity
}

func compareAndInspect(ctx context.Context, label, ours, theirs string, wantStep int) {
	result := must(equal.CompareFiles(ours, theirs, 4096))("compare")
	check(result.Equal, fmt.Sprintf("sha256 of our replay equals sha256 of %s", label))
	before := must(equal.FileSHA256(ours))("hash")
	version, integrity := userVersion(ctx, ours)
	after := must(equal.FileSHA256(ours))("hash")
	tables.comparisons.Row(nextSeq(), part, label, result.Level, result.Equal, result.SHA256A, version, integrity, before == after)
	check(version == wantStep && integrity == "ok" && before == after, fmt.Sprintf("the replay is a valid database at recipe step %d", wantStep))
}

func onlyUncommittedIgnored(report walfile.ReplayReport) bool {
	return len(report.Ignored) == 1 && report.Ignored[0].FirstFrame == report.LastCommitFrame+1 &&
		report.Ignored[0].LastFrame == report.FramesInFile && report.ValidFrames == report.FramesInFile && report.PartialFrameBytes == 0
}

func partA(ctx context.Context, dir string) {
	part = "A"
	r := recipe.Default()
	path := filepath.Join(mkdir(filepath.Join(dir, "a-quiet")), databaseName)
	if err := r.Build(ctx, path, 0); err != nil {
		fatal("build", err)
	}
	options := r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db := must(sqlitex.Open(ctx, path, options))("open")
	for k := 1; k <= quietSteps; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	copyDir := filepath.Join(dir, "a-quiet", "capture")
	liveCopy(ctx, path, copyDir, "quiet moment")
	shmDir := mkdir(filepath.Join(dir, "a-quiet", "shm-from-a"))
	shm := must(walfile.CopyLiveFiles(ctx, []walfile.CopyPair{{From: path + "-shm", To: filepath.Join(shmDir, databaseName+"-shm")}}))("copy -shm")
	if !shm[0].Present {
		fatal("copy -shm", fmt.Errorf("missing"))
	}
	hashes := must(r.StepHashes(ctx))("step hashes")
	baseMatch := recipe.MatchHashes(hashes, must(recipe.HashFile(filepath.Join(copyDir, databaseName)))("hash base"))
	tables.recipeMatches.Row(nextSeq(), part, "captured database file alone", baseMatch.Step, baseMatch.Exact)
	check(baseMatch.Found() && baseMatch.Exact && baseMatch.Step == 0, "the captured database file alone is byte for byte recipe step 0: every change committed since is only in the -wal")
	ours, report := replayCopy(copyDir, "quiet moment")
	check(report.IgnoredFrameCount() == 0 && report.Transactions == quietSteps, "at a quiet moment every frame is committed and nothing is ignored")
	result := must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate))("checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	tables.sqliteRuns.Row(nextSeq(), part, "TRUNCATE checkpoint of the original, then close", nil, nil, result.Busy, result.LogFrames, result.Checkpointed)
	compareAndInspect(ctx, "SQLite's own checkpoint of the original", ours, path, quietSteps)
	match := must(r.MatchStep(ctx, ours))("match replay")
	tables.recipeMatches.Row(nextSeq(), part, "our replay", match.Step, match.Exact)
	check(match.Found() && match.Step == quietSteps, fmt.Sprintf("the replay is the recipe's state after step %d (exact, or equal except the WAL change counter of one long-lived connection)", quietSteps))
}

func partBC(ctx context.Context, dir string) {
	part = "B"
	r := recipe.Default()
	root := mkdir(filepath.Join(dir, "b-mid-transaction"))
	path := filepath.Join(root, databaseName)
	if err := r.Build(ctx, path, spillBaseStep); err != nil {
		fatal("build", err)
	}
	// Set cache_size=10 so the open transaction runs out of page-cache slots
	// and spills dirty frames into -wal before COMMIT (src/wal.c:4140-4173).
	// Updating documents once dirties each page at most once, so no spilled
	// frame is overwritten in place and every spilled frame passes salt and
	// checksum validation—exercising stopping at LastCommitFrame on valid
	// uncommitted frames.
	options := r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	options.Conn.CacheSize = spillCacheSize
	db := must(sqlitex.Open(ctx, path, options))("open")
	for k := spillBaseStep + 1; k <= spillCommitted; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	committedOnly := must(walfile.ReadLiveState(ctx, path, root))("live state")
	tables.walStates.Row(nextSeq(), part, "after the commits", committedOnly.WALSize, len(committedOnly.WAL.Frames), committedOnly.WAL.ValidFrames, committedOnly.WAL.LastCommitFrame, committedOnly.Index.Header().MaxFrame)
	tx := must(db.BeginTx(ctx, nil))("begin")
	if _, err := tx.ExecContext(ctx, spillExtraUpdate); err != nil {
		fatal("update", err)
	}
	captureDir := filepath.Join(root, "capture-open-transaction")
	liveCopy(ctx, path, captureDir, "open transaction")
	captured := must(walfile.ParseFile(filepath.Join(captureDir, databaseName+"-wal")))("parse")
	spilled := len(captured.Frames) - captured.LastCommitFrame
	tables.walStates.Row(nextSeq(), part, "captured while the transaction is open", captured.Size, len(captured.Frames), captured.ValidFrames, captured.LastCommitFrame, nil)
	check(captured.LastCommitFrame == committedOnly.WAL.LastCommitFrame && spilled > 0, "the open transaction spilled frames into the -wal before committing, after the last commit frame")
	check(captured.ValidFrames == len(captured.Frames) && captured.ValidFrames > captured.LastCommitFrame,
		"every spilled frame passes the checksum and salt checks: the frames after the last commit frame are valid, only uncommitted")
	ours, report := replayCopy(captureDir, "open transaction")
	check(report.IgnoredFrameCount() == spilled && report.LastCommitFrame == committedOnly.WAL.LastCommitFrame &&
		report.AppliedFrames+report.SkippedBeyondSize == report.LastCommitFrame && onlyUncommittedIgnored(report),
		"the replayer stops at the last commit frame: it applies frames up to it and ignores every valid spilled frame after it because no commit frame follows")
	recovered := sqliteRecoveryOfCopy(ctx, captureDir, recoveryDirectory, "")
	compareAndInspect(ctx, "SQLite's recovery and checkpoint of the same captured files", ours, recovered, spillCommitted)
	// Pass a -shm taken from part A (different database, different salts,
	// mxFrame 155). The first connection opening a WAL database takes the
	// dead-man-switch lock, truncates -shm, and rebuilds it from -wal
	// (src/os_unix.c:4850-4915, unixLockSharedMemory; src/wal.c:1390-1614).
	staleIndex := filepath.Join(dir, "a-quiet", "shm-from-a", databaseName+"-shm")
	staleState := must(walfile.ParseIndexFile(staleIndex))("parse stale -shm")
	tables.staleShm.Row(staleState.Header().MaxFrame, staleState.Header().Pages)
	withStale := sqliteRecoveryOfCopy(ctx, captureDir, recoveryDirectory+"-stale-shm", staleIndex)
	compareAndInspect(ctx, "SQLite's checkpoint of the same files opened with a stale -shm", ours, withStale, spillCommitted)
	check(staleState.Header().MaxFrame == quietTotalFrames, "the -shm handed to SQLite described another WAL entirely, and SQLite rebuilt it from the -wal instead of trusting it")

	part = "C"
	if err := tx.Rollback(); err != nil {
		fatal("rollback", err)
	}
	afterRollback := must(walfile.ReadLiveState(ctx, path, root))("live state")
	tables.walStates.Row(nextSeq(), part, "after ROLLBACK", afterRollback.WALSize, len(afterRollback.WAL.Frames), afterRollback.WAL.ValidFrames, afterRollback.WAL.LastCommitFrame, afterRollback.Index.Header().MaxFrame)
	check(afterRollback.WALSize == captured.Size && int(afterRollback.Index.Header().MaxFrame) == captured.LastCommitFrame,
		"a rollback leaves the spilled frames in the -wal file; only the wal-index forgets them")
	rollbackDir := filepath.Join(root, "capture-after-rollback")
	liveCopy(ctx, path, rollbackDir, "after rollback")
	oursAfter, reportAfter := replayCopy(rollbackDir, "after rollback")
	check(reportAfter.IgnoredFrameCount() > 0 && reportAfter.LastCommitFrame == captured.LastCommitFrame && onlyUncommittedIgnored(reportAfter),
		"the replayer ignores the rolled-back frames because no commit frame follows them")
	result := must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate))("checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	tables.sqliteRuns.Row(nextSeq(), part, "TRUNCATE checkpoint of the original, then close", nil, nil, result.Busy, result.LogFrames, result.Checkpointed)
	compareAndInspect(ctx, "SQLite's own checkpoint of the original after the rollback", oursAfter, path, spillCommitted)
	same := must(equal.CompareFiles(ours, oursAfter, 4096))("compare")
	tables.comparisons.Row(nextSeq(), part, "replay of the open-transaction capture vs replay of the after-rollback capture", same.Level, same.Equal, same.SHA256A, nil, nil, nil)
	check(same.Equal, "both captures rebuild the same committed state")
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.2")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	partA(ctx, dir)
	partBC(ctx, dir)
	set.Finish("WAL REPLAY")
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

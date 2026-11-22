// Experiment 5.1 (wal-inspector) parses a live SQLite WAL and its -shm
// wal-index in plain Go, verifies the running checksum chain and commit frames
// against PRAGMA wal_checkpoint(NOOP) and SQLite's showwal utility, and proves
// that repeated runs and different synchronous settings produce the exact same
// frame sequence, differing only in random salts and dependent checksums.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.1
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	firstStep = 1
	lastStep  = 5
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.1" runs both.
var (
	set    *results.Set
	tables struct {
		walHeader, frames, commits, noop, walIndex, showwal, regions, sequences *results.Table
		infoSalts, infoRegionBytes                                              *results.Table
	}
)

func openTables() {
	tables.walHeader = set.Table("wal_header", "magic", "checksum_order", "version", "page_size", "checkpoint_seq", "wal_bytes", "header_bytes", "frame_header_bytes", "frames", "valid_frames", "partial_bytes", "stop_reason")
	tables.frames = set.Table("frames", "frame_no", "page", "commit_size", "sha256_16")
	tables.commits = set.Table("commits", "commit_no", "recipe_step", "first_frame", "last_frame", "frames", "commit_size", "page_count")
	tables.noop = set.Table("noop_checkpoint", "busy", "log_frames", "checkpointed")
	tables.walIndex = set.Table("wal_index", "bytes", "byte_order", "headers_agree", "checksum_ok", "mx_frame", "n_page", "page_size", "n_backfill", "salts_equal_wal", "page_array_matches", "frames")
	tables.showwal = set.Table("showwal", "frames_listed", "disagreements", "problems")
	tables.regions = set.Table("regions", "seq", "region", "differs")
	tables.sequences = set.Table("sequences", "part", "run", "synchronous", "wal_bytes", "frames", "commits", "same_sequence", "first_difference")
	tables.infoSalts = set.Table("info_salts", "run", "salt1", "salt2", "header_checksum")
	tables.infoRegionBytes = set.Table("info_region_bytes", "seq", "region", "differing_bytes")
}

func check(part string, ok bool, claim string) {
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

type run struct {
	name        string
	synchronous string
	wal         *walfile.File
	walBytes    []byte
	index       *walfile.Index
	noop        sqlitex.CheckpointResult
	pageCounts  []int
	databaseDir string
}

// buildRun creates a step-0 database, applies recipe steps 1..5 on one
// connection with autocheckpoint disabled, copies the live database, -wal, and
// -shm from a child process before closing, and parses both sidecar files.
func buildRun(ctx context.Context, dir, name, synchronous string) run {
	r := recipe.Default()
	runDir := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(runDir, "copy"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(runDir, "r.db")
	if err := r.Build(ctx, path, firstStep-1); err != nil {
		fatal("build step 0", err)
	}
	options := r.SQLiteOptions()
	// Disable autocheckpoint so every frame of steps 1..5 remains in the WAL
	// until the child copies it.
	options.Conn.WALAutocheckpoint = 0
	options.Conn.Synchronous = synchronous
	db := must(sqlitex.Open(ctx, path, options))("open")
	result := run{name: name, synchronous: synchronous, databaseDir: runDir}
	for k := firstStep; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal(fmt.Sprintf("step %d", k), err)
		}
		var pages int
		if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
			fatal("page_count", err)
		}
		result.pageCounts = append(result.pageCounts, pages)
	}
	// Copy while the connection is still open because closing the last
	// connection triggers a close checkpoint and deletes -wal and -shm
	// (src/wal.c:2528-2565, sqlite3WalClose). Copying in a child avoids
	// dropping this process's POSIX locks on -shm (src/os_unix.c:4703-4750).
	copies := must(walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(path, filepath.Join(runDir, "copy"), "r.db")))("live copy")
	for _, c := range copies {
		if !c.Present {
			fatal("live copy", fmt.Errorf("%s was missing while the connection was open", filepath.Base(c.From)))
		}
	}
	// NOOP takes no locks and reports (busy, mxFrame, nBackfill) from the
	// wal-index (src/wal.c:4341, 4399-4403).
	result.noop = must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointNoop))("NOOP checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	result.walBytes = must(os.ReadFile(filepath.Join(runDir, "copy", "r.db-wal")))("read WAL copy")
	result.wal = must(walfile.Parse(result.walBytes))("parse WAL")
	result.index = must(walfile.ParseIndexFile(filepath.Join(runDir, "copy", "r.db-shm")))("parse wal-index")
	return result
}

func shortHash(h string) string { return h[:16] }

func partA(ctx context.Context, dir string) run {
	first := buildRun(ctx, dir, "run-1", sqlitex.SyncFull)
	wal := first.wal
	tables.walHeader.Row(wal.Header.Magic, wal.Header.ChecksumByteOrder(), wal.Header.Version, wal.Header.PageSize, wal.Header.CheckpointSequence,
		wal.Size, walfile.HeaderSize, walfile.FrameHeaderSize, len(wal.Frames), wal.ValidFrames, wal.PartialFrameBytes, wal.StopReason())
	tables.infoSalts.Row("run 1", wal.Header.Salt1, wal.Header.Salt2, wal.Header.Checksum)
	check("A", wal.Size == walfile.HeaderSize+int64(len(wal.Frames))*int64(walfile.FrameHeaderSize+wal.PageSize()) && wal.PartialFrameBytes == 0,
		"the WAL is a 32-byte header plus whole frames of a 24-byte header and one 4096-byte page")
	for _, frame := range wal.Frames {
		tables.frames.Row(frame.Number, frame.Page, frame.CommitSize, shortHash(frame.PageSHA256))
	}
	saltsMatch := true
	for _, frame := range wal.Frames {
		saltsMatch = saltsMatch && frame.SaltsMatchHeader
	}
	check("A", wal.ValidFrames == len(wal.Frames) && saltsMatch, "every frame carries the header's salts and continues the running checksum chain")
	commits := wal.Commits()
	sizesMatch := len(commits) == len(first.pageCounts)
	for i, c := range commits {
		pageCount := -1
		if i < len(first.pageCounts) {
			pageCount = first.pageCounts[i]
		}
		tables.commits.Row(i+1, firstStep+i, c.FirstFrame, c.LastFrame, c.Frames(), c.Pages, pageCount)
		sizesMatch = sizesMatch && int(c.Pages) == pageCount
	}
	check("A", len(commits) == lastStep-firstStep+1, "five recipe steps left exactly five commit frames")
	check("A", sizesMatch, "each commit frame's size field equals the database's page count after that transaction")
	tables.noop.Row(first.noop.Busy, first.noop.LogFrames, first.noop.Checkpointed)
	check("A", first.noop.LogFrames == len(wal.Frames) && first.noop.Checkpointed == 0, "the frame count equals the log column of wal_checkpoint(NOOP); nothing was checkpointed")
	describeIndex(first)
	crossCheckShowwal(first)
	return first
}

func describeIndex(r run) {
	h := r.index.Header()
	wal := r.wal
	saltsEqual := h.Salt1 == wal.Header.Salt1 && h.Salt2 == wal.Header.Salt2
	pages, err := r.index.FramePages(h.MaxFrame)
	mappingMatches := err == nil && len(pages) == len(wal.Frames)
	for i := 0; mappingMatches && i < len(pages); i++ {
		mappingMatches = pages[i] == wal.Frames[i].Page
	}
	tables.walIndex.Row(r.index.Size, r.index.ByteOrderName(), r.index.HeadersAgree(), h.ChecksumOK, h.MaxFrame, h.Pages, h.PageSize(), r.index.Checkpoint.Backfill, saltsEqual, mappingMatches, len(pages))
	check("A", r.index.HeadersAgree() && h.ChecksumOK && int(h.MaxFrame) == wal.LastCommitFrame && h.Pages == wal.DatabasePages() &&
		saltsEqual && mappingMatches,
		"the -shm header names the last commit frame (mxFrame), the database size and the WAL's salts, and its page array maps every frame to its page")
}

func crossCheckShowwal(r run) {
	tool, err := sqlitex.LocateTool("showwal")
	if err != nil {
		fmt.Printf("NO-SHOWWAL: %v; cross-check skipped\n", err)
		return
	}
	out, err := exec.Command(tool, filepath.Join(r.databaseDir, "copy", "r.db-wal")).Output()
	if err != nil {
		fatal("showwal", err)
	}
	listing := must(walfile.ParseShowwal(string(out)))("parse showwal output")
	problems := r.wal.CompareWithShowwal(listing)
	tables.showwal.Row(len(listing.Frames), len(problems), strings.Join(problems, "; "))
	check("A", len(problems) == 0, "showwal and walfile agree on every frame's page, commit size, salts and stored checksums, and on which checksums verify")
}

func sequenceRow(part, name, synchronous string, other run, equal bool, at int) {
	tables.sequences.Row(part, name, synchronous, len(other.walBytes), len(other.wal.Frames), len(other.wal.Commits()), equal, at)
}

func partB(ctx context.Context, dir string, first run) {
	second := buildRun(ctx, dir, "run-2", sqlitex.SyncFull)
	tables.infoSalts.Row("run 2", second.wal.Header.Salt1, second.wal.Header.Salt2, second.wal.Header.Checksum)
	comparison := walfile.CompareBytes(first.walBytes, second.walBytes, first.wal.PageSize())
	for r := walfile.RegionHeaderMagic; r <= walfile.RegionBeyondShorterFile; r++ {
		tables.regions.Row(int(r), r.String(), comparison.ByRegion[r] > 0)
		tables.infoRegionBytes.Row(int(r), r.String(), comparison.ByRegion[r])
	}
	check("B", !comparison.Identical() && comparison.OnlyIn(walfile.RegionHeaderSalt, walfile.RegionHeaderChecksum, walfile.RegionFrameSalt, walfile.RegionFrameChecksum),
		"the two WALs differ, and only inside the salt and checksum fields")
	equal, at := walfile.EqualTuples(first.wal.Tuples(true), second.wal.Tuples(true))
	sequenceRow("B", "run 2", sqlitex.SyncFull, second, equal, at)
	check("B", equal, "the two runs wrote the same pages, with the same images, in the same order, with the same commit sizes")

	for _, level := range []string{sqlitex.SyncNormal, sqlitex.SyncOff} {
		other := buildRun(ctx, dir, "sync-"+strings.ToLower(level), level)
		equal, at := walfile.EqualTuples(first.wal.Tuples(true), other.wal.Tuples(true))
		sequenceRow("C", "sync-"+strings.ToLower(level), level, other, equal, at)
		check("C", equal, fmt.Sprintf("synchronous=%s changes when SQLite syncs, not which frames it writes", level))
	}
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	first := partA(ctx, dir)
	partB(ctx, dir, first)
	set.Finish("WAL INSPECTOR")
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

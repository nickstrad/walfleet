// Experiment 9.3 demonstrates why PRAGMA integrity_check and PRAGMA quick_check
// verify B-tree structure and index consistency rather than data content. It
// flips one byte in each of four distinct regions of a step-12 recipe database
// (a B-tree page header cell count, an unindexed text column, an indexed
// integer column, and a freelist leaf page) and compares integrity_check
// (src/btree.c:11144-11295, src/pragma.c:1850-1857, 2084-2090), quick_check,
// the E3 logical hash, and a Chapter 2 per-page SHA-256 manifest.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 9.3
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	step          = 12
	eventsTable   = "events"
	accountColumn = 1
	noteColumn    = 4
)

type site struct {
	name   string
	offset int64
	mask   byte
	detail string
}

type verdicts struct {
	integrity, quick []string
	logical          int
	differing        []int
	caught           [4]bool
}

// findSites uses internal/dbfile to locate four deterministic single-byte
// corruption targets in f: a leaf page header's cell count, one ASCII letter
// inside unindexed events.note, the 1-byte integer value of indexed
// events.account_id, and the midpoint of a freelist leaf page.
func findSites(f *dbfile.File) ([]site, error) {
	census, err := f.Census()
	if err != nil {
		return nil, err
	}
	pageSize := int64(f.PageSize())
	var leaves []int
	for _, info := range census.Pages {
		if info.Tree == eventsTable && info.Role == dbfile.RoleTableLeaf {
			leaves = append(leaves, info.Number)
		}
	}
	if len(leaves) < 2 || len(census.FreelistLeaves) == 0 {
		return nil, fmt.Errorf("need two %s leaf pages and a freelist leaf, found %d and %d", eventsTable, len(leaves), len(census.FreelistLeaves))
	}
	headerPage := leaves[0]
	page, err := f.BTreePage(leaves[1])
	if err != nil {
		return nil, err
	}
	cell, err := page.Cell(0)
	if err != nil {
		return nil, err
	}
	if cell.Overflows() {
		return nil, fmt.Errorf("first cell of page %d overflows", page.Number)
	}
	localStart := int64(page.Number-1)*pageSize + int64(cell.Offset+dbfile.VarintLength(cell.PayloadSize)+dbfile.VarintLength(uint64(cell.Rowid)))
	accountStart, accountLength, _, err := dbfile.RecordColumn(cell.Local, accountColumn)
	if err != nil || accountLength != 1 {
		return nil, fmt.Errorf("account_id column: length %d, %v", accountLength, err)
	}
	noteStart, noteLength, _, err := dbfile.RecordColumn(cell.Local, noteColumn)
	if err != nil || noteLength == 0 {
		return nil, fmt.Errorf("note column: %v", err)
	}
	letter := -1
	for i := noteStart; i < noteStart+noteLength; i++ {
		if c := cell.Local[i]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			letter = i
			break
		}
	}
	if letter < 0 {
		return nil, errors.New("the note has no letter to change")
	}
	freelist := census.FreelistLeaves[0]
	return []site{
		{"b-tree page header (cell count)", int64(headerPage-1)*pageSize + 4, 0x01, fmt.Sprintf("page %d (events leaf), header byte 4", headerPage)},
		{"unindexed column (events.note)", localStart + int64(letter), 0x20, fmt.Sprintf("page %d, row %d, letter %q -> %q", page.Number, cell.Rowid, cell.Local[letter], cell.Local[letter]^0x20)},
		{"indexed column (events.account_id)", localStart + int64(accountStart), 0x01, fmt.Sprintf("page %d, row %d, account_id %d -> %d", page.Number, cell.Rowid, cell.Local[accountStart], cell.Local[accountStart]^0x01)},
		{"freelist leaf page", int64(freelist-1)*pageSize + pageSize/2, 0xff, fmt.Sprintf("page %d, byte %d", freelist, pageSize/2)},
	}, nil
}

func corruptCopy(original []byte, s site, path string) error {
	image := append([]byte(nil), original...)
	image[s.offset] ^= s.mask
	return os.WriteFile(path, image, 0o644)
}

// check runs PRAGMA integrity_check, PRAGMA quick_check, E3 logical hashing,
// and capture.VerifyImage against path, confirming SQLite's read-only checks do
// not modify the file bytes.
func check(ctx context.Context, path string, original equal.Manifest, originalLogical equal.Logical) (verdicts, error) {
	before, err := equal.FileSHA256(path)
	if err != nil {
		return verdicts{}, err
	}
	inspection, err := procs.InspectDatabase(ctx, path, sqlitex.JournalWAL, "PRAGMA quick_check")
	if err != nil {
		return verdicts{}, err
	}
	logical, err := equal.LogicalHashFile(ctx, path)
	if err != nil {
		return verdicts{}, err
	}
	after, err := equal.FileSHA256(path)
	if err != nil || after != before {
		return verdicts{}, fmt.Errorf("checking %s changed the file (%v)", path, err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return verdicts{}, err
	}
	differing, err := capture.VerifyImage(original, image)
	if err != nil {
		return verdicts{}, err
	}
	logicalDiff := equal.CompareLogical(originalLogical, logical)
	v := verdicts{
		integrity: procs.IntegrityProblems(inspection.Integrity),
		quick:     procs.IntegrityProblems(inspection.Values[0]),
		logical:   len(logicalDiff.Differences),
		differing: differing,
	}
	v.caught = [4]bool{inspection.Integrity != "ok", inspection.Values[0] != "ok", !logicalDiff.Equal, len(differing) > 0}
	return v, nil
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set := results.MustOpen(dir)
	originalTable := set.Table("original", "recipe_step", "pages", "manifest_sha256")
	sitesTable := set.Table("sites", "seq", "site", "location", "file_offset", "xor_mask")
	verdictsTable := set.Table("verdicts", "seq", "file", "site",
		"caught_integrity", "caught_quick", "caught_e3", "caught_manifest",
		"expected_integrity", "expected_quick", "expected_e3", "expected_manifest",
		"e3_differences", "differing_pages")
	problemsTable := set.Table("problems", "seq", "file", "pragma", "problem_no", "problem")
	record := func(seq int, file, site string, v verdicts, want [4]bool) {
		verdictsTable.Row(seq, file, site, v.caught[0], v.caught[1], v.caught[2], v.caught[3], want[0], want[1], want[2], want[3], v.logical, joinInts(v.differing))
		for i, problem := range v.integrity {
			problemsTable.Row(seq, file, "integrity_check", i+1, problem)
		}
		for i, problem := range v.quick {
			problemsTable.Row(seq, file, "quick_check", i+1, problem)
		}
	}

	r := recipe.Default()
	db := filepath.Join(dir, "original.db")
	if err := r.Build(ctx, db, step); err != nil {
		fatal("build", err)
	}
	original, err := os.ReadFile(db)
	if err != nil {
		fatal("read", err)
	}
	f, err := dbfile.Open(db)
	if err != nil {
		fatal("open", err)
	}
	manifest, err := capture.BuildManifest(f)
	if err != nil {
		fatal("manifest", err)
	}
	sites, err := findSites(f)
	if closeErr := f.Close(); err != nil || closeErr != nil {
		fatal("sites", errors.Join(err, closeErr))
	}
	logical, err := equal.LogicalHashFile(ctx, db)
	if err != nil {
		fatal("logical hash", err)
	}
	baseline, err := check(ctx, db, manifest, logical)
	if err != nil {
		fatal("baseline", err)
	}
	originalTable.Row(step, manifest.PageCount(), manifest.SHA256()[:16])
	record(0, "original.db", "none (the undamaged file)", baseline, [4]bool{})
	set.Check("A", baseline.caught == [4]bool{}, "every check passes on the undamaged file")
	want := map[string][4]bool{
		sites[0].name: {true, true, true, true},
		sites[1].name: {false, false, true, true},
		sites[2].name: {true, false, true, true},
		sites[3].name: {false, false, false, true},
	}
	for i, s := range sites {
		name := fmt.Sprintf("site-%d.db", i+1)
		path := filepath.Join(dir, name)
		if err := corruptCopy(original, s, path); err != nil {
			fatal("corrupt", err)
		}
		v, err := check(ctx, path, manifest, logical)
		if err != nil {
			fatal(s.name, err)
		}
		sitesTable.Row(i+1, s.name, s.detail, s.offset, s.mask)
		record(i+1, name, s.name, v, want[s.name])
		set.Check("B", v.caught == want[s.name], fmt.Sprintf("%s: caught by integrity_check %t, quick_check %t, E3 %t, manifest %t", s.name, want[s.name][0], want[s.name][1], want[s.name][2], want[s.name][3]))
	}
	set.Finish("INTEGRITY CHECK LIMITS")
}

func joinInts(values []int) string {
	text := make([]string, len(values))
	for i, v := range values {
		text[i] = fmt.Sprint(v)
	}
	return strings.Join(text, " ")
}

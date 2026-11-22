// Experiment 3.1 measures how many pages and WAL frames each single-row INSERT
// modifies across five workloads: ascending appends through two depth changes
// (Part A), a middle split into a full leaf (Part B), a table with two secondary
// indexes (Part C), rollback-journal mode (Part D), and four boundary workloads
// showing where "most inserts change one page" stops holding (Part E).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 3.1
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize      = 4096
	blobBytes     = 1000
	rowidStep     = 2
	walHeaderSize = 32
	walFrameHead  = 24
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 3.1" runs both.
// part and workloadName label the rows written while a part runs.
var (
	set          *results.Set
	part         string
	workloadName any
	layoutSeq    int
	workloadSeq  int
	tables       struct {
		inserts, steps, pageStates, predictions, unchangedFrames, workloads *results.Table
	}
)

func openTables() {
	tables.inserts = set.Table("inserts", "part", "workload", "journal_mode", "insert_no", "rowid", "pages_changed", "changed_pages", "changed_roles", "page1_changed", "pages_added", "page_count", "wal_frames", "wal_pages", "depth", "root_page", "integrity")
	tables.steps = set.Table("steps", "part", "label", "insert_no")
	tables.pageStates = set.Table("page_states", "seq", "part", "label", "insert_no", "page", "state", "kind", "cells", "keys", "content_start", "unused", "children", "note")
	tables.predictions = set.Table("predictions", "seq", "name", "value", "how")
	tables.unchangedFrames = set.Table("unchanged_frames", "part", "insert_no", "page", "sha256_before", "sha256_after")
	tables.workloads = set.Table("workloads", "seq", "workload", "table_name", "row_bytes", "leaves", "min_rows_per_leaf", "max_rows_per_leaf", "final_depth")
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, " ")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func rowBlob(rowid int64) []byte {
	return sizedBlob(rowid, blobBytes)
}

func sizedBlob(rowid int64, size int) []byte {
	label := fmt.Sprintf("row %06d ", rowid)
	return bytes.Repeat([]byte(label), size/len(label)+1)[:size]
}

type lab struct {
	path     string
	mode     string
	db       *sqlitex.DB
	conn     *sql.Conn
	file     *dbfile.File
	manifest equal.Manifest
	image    []byte
	census   *dbfile.Census
}

type step struct {
	number    int
	rowid     int64
	changes   []equal.PageChange
	walPages  []int
	before    *dbfile.File
	after     *dbfile.File
	depth     int
	rootPage  int
	integrity string
}

func openLab(ctx context.Context, path, mode string, statements ...string) *lab {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.File.JournalMode = mode
	// SyncOff skips fsync across ~3,000 single-row commits without altering any
	// file byte or WAL frame sequence; WALAutocheckpoint=0 keeps each commit's
	// frames in -wal until snapshot() reads them and runs TRUNCATE.
	options.Conn.Synchronous = sqlitex.SyncOff
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open "+filepath.Base(path), err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	l := &lab{path: path, mode: mode, db: db, conn: conn}
	for _, s := range statements {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	l.file, l.manifest = l.snapshot(ctx)
	return l
}

func (l *lab) close() {
	if err := l.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := l.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (l *lab) snapshot(ctx context.Context) (*dbfile.File, equal.Manifest) {
	if l.mode == "wal" {
		result, err := sqlitex.Checkpoint(ctx, l.conn, "main", "TRUNCATE")
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	var count int
	if err := l.conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&count); err != nil {
		fatal("page_count", err)
	}
	image := make([]byte, 0, count*pageSize)
	// Read raw pages through sqlite_dbpage instead of os.Open: opening and
	// closing the database file in the same process that holds a SQLite
	// connection drops that process's POSIX locks (see Chapter 6).
	rows, err := l.conn.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	expected := int64(1)
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage", err)
		}
		if pgno != expected || len(data) != pageSize {
			fatal("sqlite_dbpage", fmt.Errorf("page %d (%d bytes), want page %d", pgno, len(data), expected))
		}
		expected++
		image = append(image, data...)
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if err := rows.Close(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if int(expected-1) != count {
		fatal("sqlite_dbpage", fmt.Errorf("%d pages, page_count says %d", expected-1, count))
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse image", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	m := equal.Manifest{PageSize: pageSize, Entries: make([]equal.ManifestEntry, count)}
	for n := 1; n <= count; n++ {
		page := image[(n-1)*pageSize : n*pageSize]
		sum := ""
		// Reuse the previous manifest's SHA-256 when a page's bytes are unchanged;
		// verifyManifest() checks the final manifest against capture.BuildManifest.
		if l.image != nil && n*pageSize <= len(l.image) && bytes.Equal(page, l.image[(n-1)*pageSize:n*pageSize]) {
			sum = l.manifest.Entries[n-1].SHA256
		} else {
			sum = equal.SHA256Hex(page)
		}
		m.Entries[n-1] = equal.ManifestEntry{Page: n, Role: census.Page(n).Role.String(), SHA256: sum}
	}
	l.image = image
	l.census = census
	return f, m
}

func (l *lab) verifyManifest() bool {
	full, err := capture.BuildManifest(l.file)
	if err != nil {
		fatal("manifest", err)
	}
	return full.Text() == l.manifest.Text()
}

// readWALPages parses the 32-byte WAL header and each 24-byte frame header to
// list the page numbers written by the most recent commit. Reading -wal with
// os.ReadFile is safe while SQLite is open because WAL POSIX locks live on -shm.
func readWALPages(path string) ([]int, error) {
	data, err := os.ReadFile(path + "-wal")
	if err != nil {
		return nil, err
	}
	if len(data) < walHeaderSize {
		return nil, fmt.Errorf("-wal holds %d bytes, less than its header", len(data))
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil, fmt.Errorf("-wal magic %#x", magic)
	}
	frameSize := walFrameHead + pageSize
	if (len(data)-walHeaderSize)%frameSize != 0 {
		return nil, fmt.Errorf("-wal size %d is not a header plus whole frames", len(data))
	}
	var pages []int
	for off := walHeaderSize; off < len(data); off += frameSize {
		pages = append(pages, int(binary.BigEndian.Uint32(data[off:off+4])))
	}
	return pages, nil
}

func (l *lab) integrity(ctx context.Context) string {
	var result string
	if err := l.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		fatal("integrity_check", err)
	}
	return result
}

func (l *lab) rootOf(table string) int {
	schema, err := l.file.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name == table {
			return int(e.RootPage)
		}
	}
	fatal("schema", fmt.Errorf("no table %q", table))
	return 0
}

func (l *lab) depth(root int) int {
	deepest := 0
	for n := 1; n <= l.census.PageCount; n++ {
		info := l.census.Page(n)
		if info.Role != dbfile.RoleTableInterior && info.Role != dbfile.RoleTableLeaf && info.Role != dbfile.RoleIndexInterior && info.Role != dbfile.RoleIndexLeaf {
			continue
		}
		d, page := 1, n
		for page != root && page != 0 {
			page = l.census.Page(page).Parent
			d++
		}
		if page == root {
			deepest = max(deepest, d)
		}
	}
	return deepest
}

func (l *lab) exec(ctx context.Context, number int, rowid int64, table, statement string, args ...any) step {
	before := l.file
	if _, err := l.conn.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
	s := step{number: number, rowid: rowid, before: before}
	if l.mode == "wal" {
		pages, err := readWALPages(l.path)
		if err != nil {
			fatal("read -wal", err)
		}
		s.walPages = pages
	}
	file, manifest := l.snapshot(ctx)
	s.changes = equal.ChangedPages(l.manifest, manifest)
	s.after = file
	l.file, l.manifest = file, manifest
	s.rootPage = l.rootOf(table)
	s.depth = l.depth(s.rootPage)
	s.integrity = l.integrity(ctx)
	var walFrames, walPages any
	if l.mode == "wal" {
		walFrames, walPages = len(s.walPages), joinInts(s.walPages)
	}
	tables.inserts.Row(part, workloadName, l.mode, number, rowid, len(s.changes), joinInts(s.pages()),
		strings.Trim(equal.FormatPageChanges(s.changes), "[]"), len(s.changes) > 0 && s.changes[0].Page == 1,
		s.after.PageCount()-s.before.PageCount(), s.after.PageCount(), walFrames, walPages, s.depth, s.rootPage, s.integrity)
	return s
}

func (s step) pages() []int { return equal.PageNumbers(s.changes) }

// leafHoldsRowid verifies that page is a user table leaf (not page 1, whose
// B-tree is sqlite_schema) containing a cell with the given rowid.
func leafHoldsRowid(f *dbfile.File, page int, rowid int64) bool {
	if page == 1 {
		return false
	}
	p, err := f.BTreePage(page)
	if err != nil || p.Type != dbfile.TableLeaf {
		return false
	}
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	for _, c := range cells {
		if c.HasRowid && c.Rowid == rowid {
			return true
		}
	}
	return false
}

func sameInts(a, b []int) bool {
	x := append([]int(nil), a...)
	y := append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func subtract(a, b []int) []int {
	in := map[int]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []int
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func keysOf(p *dbfile.BTreePage) string {
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	if len(cells) == 0 {
		return "none"
	}
	key := func(c dbfile.Cell) string {
		if c.HasRowid {
			return fmt.Sprint(c.Rowid)
		}
		return "?"
	}
	if len(cells) <= 6 {
		var keys []string
		for _, c := range cells {
			keys = append(keys, key(c))
		}
		return strings.Join(keys, ",")
	}
	return key(cells[0]) + ".." + key(cells[len(cells)-1])
}

// childrenOf lists the child pages of an interior page, shortened when there are more than eight.
func childrenOf(p *dbfile.BTreePage) any {
	if p.Type.IsLeaf() {
		return nil
	}
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	var children []string
	for _, c := range cells {
		children = append(children, fmt.Sprint(c.LeftChild))
	}
	children = append(children, fmt.Sprint(p.RightChild))
	if len(children) > 8 {
		children = append(children[:3], "...", children[len(children)-1])
	}
	return strings.Join(children, " ")
}

func mustPage(f *dbfile.File, n int) *dbfile.BTreePage {
	p, err := f.BTreePage(n)
	if err != nil {
		fatal("page", err)
	}
	return p
}

// pageStateRow writes one page_states row: what page n of f holds.
func pageStateRow(label string, insert int, state string, f *dbfile.File, n int) {
	layoutSeq++
	var kind, cells, keys, contentStart, unused, children, note any
	if f == nil || n > f.PageCount() {
		note = "not in the file"
	} else if p, err := f.BTreePage(n); err != nil {
		note = "not a b-tree page"
	} else {
		kind, cells, keys, contentStart, unused, children = p.Type, p.CellCount, keysOf(p), p.CellContentStart, p.Unused(), childrenOf(p)
		if n == 1 {
			keys = nil
			note = fmt.Sprintf("sqlite_schema; header says %d pages", f.Header.DatabaseSize)
		}
	}
	tables.pageStates.Row(layoutSeq, part, label, insert, n, state, kind, cells, keys, contentStart, unused, children, note)
}

// recordStep names one insert in the steps table so the report can point at it.
func recordStep(label string, s step) {
	tables.steps.Row(part, label, s.number)
}

// recordLayout writes every changed page of a step, before and after, to page_states.
func recordLayout(label string, s step) {
	for _, c := range s.changes {
		pageStateRow(label, s.number, "before", s.before, c.Page)
		pageStateRow(label, s.number, "after", s.after, c.Page)
	}
}

type arithmetic struct {
	payload      int
	cellSmall    int
	cellLarge    int
	rowsPerLeaf  int
	dividersFit  int
	firstSplit   int
	deeperInsert int
}

// predict computes leaf capacity, first split, and root interior capacity from
// SQLite's cell format (btreeInt.h:149-228) before any row is inserted, and
// writes each step of the arithmetic to the predictions table.
func predict() arithmetic {
	usable := pageSize
	serialBlob := uint64(12 + 2*blobBytes)
	header := 1 + 1 + dbfile.VarintLength(serialBlob)
	headerVarint := dbfile.VarintLength(uint64(header))
	if headerVarint != 1 {
		fatal("arithmetic", errors.New("record header longer than 127 bytes"))
	}
	payload := header + blobBytes
	maxLocal := dbfile.MaxLocalPayload(usable, dbfile.TableLeaf)
	payloadVarint := dbfile.VarintLength(uint64(payload))
	cell := func(rowid uint64) int {
		return payloadVarint + dbfile.VarintLength(rowid) + payload
	}
	small, large := cell(127), cell(128)
	space := usable - 8
	perLeaf := space / (large + 2)
	if space/(small+2) != perLeaf {
		fatal("arithmetic", errors.New("rows per leaf depends on the rowid size"))
	}
	interiorSpace := usable - 12
	used, fit := 0, 0
	for leaf := 1; ; leaf++ {
		key := uint64(rowidStep * perLeaf * leaf)
		size := 4 + dbfile.VarintLength(key) + 2
		if used+size > interiorSpace {
			break
		}
		used += size
		fit++
	}
	small1 := 0
	for leaf := 1; leaf <= fit; leaf++ {
		if rowidStep*perLeaf*leaf <= 127 {
			small1++
		}
	}
	deeper := perLeaf*(fit+1) + 1
	for i, p := range []struct {
		name  string
		value int
		how   string
	}{
		{"record_header_bytes", header, fmt.Sprintf("1 size varint + 1 type (NULL: the id is the rowid) + %d type %d (blob of %d bytes)", dbfile.VarintLength(serialBlob), serialBlob, blobBytes)},
		{"payload_bytes", payload, fmt.Sprintf("%d header + %d body", header, blobBytes)},
		{"max_local_payload", maxLocal, fmt.Sprintf("table-leaf limit at usable size %d; the payload is below it: no overflow", usable)},
		{"cell_bytes_rowid_to_127", small + 2, fmt.Sprintf("%d payload-size varint + 1 rowid varint + %d payload + 2 cell pointer", payloadVarint, payload)},
		{"cell_bytes_rowid_from_128", large + 2, "the rowid varint is 2 bytes for rowids 128..16383"},
		{"leaf_space_bytes", space, fmt.Sprintf("%d - 8 (leaf header)", usable)},
		{"rows_per_leaf", perLeaf, fmt.Sprintf("floor(%d / %d): %d x %d = %d fits, %d x %d = %d does not", space, large+2, perLeaf, large+2, perLeaf*(large+2), perLeaf+1, small+2, (perLeaf+1)*(small+2))},
		{"first_split_insert", perLeaf + 1, fmt.Sprintf("inserts 1..%d fill the root leaf; rowid %d does not fit", perLeaf, rowidStep*(perLeaf+1))},
		{"split_every_inserts", perLeaf, fmt.Sprintf("a new leaf fills after %d appends: splits at inserts #%d, #%d, #%d, ...", perLeaf, perLeaf+1, 2*perLeaf+1, 3*perLeaf+1)},
		{"root_space_bytes", interiorSpace, fmt.Sprintf("%d - 12 (interior header)", usable)},
		{"dividers_fit_in_root", fit, fmt.Sprintf("%d dividers of 7 bytes (keys <= 127) + %d of 8 bytes = %d bytes; no room for one more", small1, fit-small1, used)},
		{"third_level_insert", deeper, fmt.Sprintf("the split that brings divider %d (rowid %d): the root overflows", fit+1, rowidStep*deeper)},
	} {
		tables.predictions.Row(i+1, p.name, p.value, p.how)
	}
	return arithmetic{payload: payload, cellSmall: small, cellLarge: large, rowsPerLeaf: perLeaf, dividersFit: fit, firstSplit: perLeaf + 1, deeperInsert: deeper}
}

// partA appends one row per transaction into an empty table, with a page
// manifest after every insert (WAL, a checkpoint before each manifest).
func partA(ctx context.Context, dir string) {
	part, workloadName = "A", nil
	a := predict()
	l := openLab(ctx, filepath.Join(dir, "append.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	root := l.rootOf("t")
	total := a.deeperInsert + 2*a.rowsPerLeaf
	interesting := map[int]bool{a.firstSplit: true, a.firstSplit + a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf + 1: true}
	kept := map[int]step{}
	histogram := map[int]int{}
	var splits []int
	ordinaryOnePage, ordinaryLeafOnly := true, true
	walMatches, integrityOK, rootConstant := true, true, true
	firstDeeper := map[int]step{}
	prevDepth, inserts := 1, 0
	for n := 1; n <= total; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		inserts++
		histogram[len(s.changes)]++
		if s.integrity != "ok" {
			integrityOK = false
		}
		if s.rootPage != root {
			rootConstant = false
		}
		if !sameInts(s.walPages, s.pages()) {
			walMatches = false
		}
		if s.after.PageCount() > s.before.PageCount() {
			splits = append(splits, s.number)
		} else {
			if len(s.changes) != 1 {
				ordinaryOnePage = false
			} else if !leafHoldsRowid(s.after, s.changes[0].Page, s.rowid) {
				ordinaryLeafOnly = false
			}
		}
		if s.depth > prevDepth {
			if _, seen := firstDeeper[s.depth]; !seen {
				firstDeeper[s.depth] = s
			}
		}
		prevDepth = s.depth
		if interesting[n] {
			kept[n] = s
		}
	}
	check(l.verifyManifest(), "the last manifest equals capture.BuildManifest of the same image (unchanged pages reuse the previous sha256 only to save time)")

	predictedSplits := true
	for i, n := range splits {
		if n != a.firstSplit+i*a.rowsPerLeaf {
			predictedSplits = false
		}
	}
	check(len(splits) > 0 && splits[0] == a.firstSplit, fmt.Sprintf("the first split happens at insert #%d (rowid %d), as the arithmetic predicts", a.firstSplit, rowidStep*a.firstSplit))
	check(predictedSplits, fmt.Sprintf("every split happens exactly where predicted: insert #%d m + 1, every %d inserts", a.rowsPerLeaf, a.rowsPerLeaf))
	check(ordinaryOnePage, "every insert that does not split changes exactly one page")
	check(ordinaryLeafOnly, "that one page is the table leaf that now holds the new rowid, never page 1 (page 1 is not written)")
	check(histogram[1] > inserts/2, fmt.Sprintf("most inserts change one page (%d of %d)", histogram[1], inserts))

	recordStep("first split, depth 1 -> 2", firstDeeper[2])
	recordLayout("first split, depth 1 -> 2", firstDeeper[2])
	second := kept[a.firstSplit+a.rowsPerLeaf]
	recordStep("first append split at depth 2", second)
	recordLayout("first append split at depth 2", second)
	recordStep("next append split", kept[a.firstSplit+2*a.rowsPerLeaf])
	ordinary := kept[a.firstSplit+2*a.rowsPerLeaf+1]
	recordStep("an ordinary insert", ordinary)
	recordLayout("an ordinary insert", ordinary)
	deeper, ok := firstDeeper[3]
	check(ok && deeper.number == a.deeperInsert, fmt.Sprintf("the tree gains its third level at insert #%d (rowid %d), as the arithmetic predicts", a.deeperInsert, rowidStep*a.deeperInsert))
	if ok {
		recordStep("depth 2 -> 3", deeper)
		recordLayout("depth 2 -> 3", deeper)
	}
	check(firstDeeper[2].number == a.firstSplit && len(firstDeeper[2].changes) == 4, "the first split is also the first depth change: the root, a new page holding the root's old cells, a new leaf, and page 1")
	check(len(second.changes) == 3, "an append split at depth 2 changes three pages: page 1 (database size), the parent (one new divider) and the new leaf; the full leaf is not rewritten")
	check(rootConstant, fmt.Sprintf("the root stays page %d through both depth changes and always equals sqlite_schema.rootpage", root))
	check(integrityOK, fmt.Sprintf("integrity_check = ok after every one of the %d inserts", inserts))
	check(walMatches, "for every insert the WAL frames of its commit name exactly the pages the manifest shows changed: pages changed = frames shipped")
}

// partB inserts one row into the middle of a full leaf.
func partB(ctx context.Context, dir string) {
	part, workloadName = "B", nil
	l := openLab(ctx, filepath.Join(dir, "middle.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	var last step
	for n := 1; n <= 20; n++ {
		rowid := int64(rowidStep * n)
		last = l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	}
	root := mustPage(l.file, l.rootOf("t"))
	pageStateRow("before the middle insert", last.number, "layout", l.file, root.Number)
	cells, err := root.Cells()
	if err != nil {
		fatal("cells", err)
	}
	for _, c := range cells {
		pageStateRow("before the middle insert", last.number, "layout", l.file, int(c.LeftChild))
	}
	pageStateRow("before the middle insert", last.number, "layout", l.file, int(root.RightChild))
	// Insert rowid 11 into full leaf 4 (keys 10..16), forcing balance_nonroot
	// (btree.c:8204-9027) to redistribute cells across sibling leaves.
	rowid := int64(11)
	s := l.exec(ctx, 21, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	recordStep("middle split", s)
	recordLayout("middle split", s)
	leaves := 0
	for _, c := range s.changes {
		if c.Page == 1 {
			continue
		}
		if p, err := s.after.BTreePage(c.Page); err == nil && p.Type == dbfile.TableLeaf {
			leaves++
		}
	}
	check(leaves >= 2, fmt.Sprintf("a split in the middle rewrites the full leaf and its siblings (%d leaf pages changed), not just one new page", leaves))
	// balance_nonroot calls sqlite3PagerWrite on every reused sibling page before
	// rewriting it (btree.c:8245-9027), so a sibling whose rebuilt cells match its
	// old bytes still enters the WAL as a frame even though its SHA-256 is unchanged.
	extra := subtract(s.walPages, s.pages())
	check(len(subtract(s.pages(), s.walPages)) == 0, "every changed page is in the commit's WAL frames")
	unchanged := true
	for _, n := range extra {
		before, err := s.before.Page(n)
		if err != nil {
			fatal("page before", err)
		}
		after, err := s.after.Page(n)
		if err != nil {
			fatal("page after", err)
		}
		if !bytes.Equal(before, after) {
			unchanged = false
		}
		tables.unchangedFrames.Row(part, s.number, n, equal.SHA256Hex(before)[:16], equal.SHA256Hex(after)[:16])
	}
	check(len(extra) > 0 && unchanged, "here the WAL ships more frames than pages changed: balance_nonroot rewrote a sibling leaf with the same cells it already held, and a written page is a shipped frame even when its bytes are unchanged")
	check(s.integrity == "ok", "integrity_check = ok after the middle split")
	walker := dbfile.TreeWalker{File: s.after, OnPage: func(p *dbfile.BTreePage, parent, depth int) error {
		if p.Type.IsLeaf() {
			pageStateRow("leaves in key order after the middle split", s.number, "layout", s.after, p.Number)
		}
		return nil
	}}
	if err := walker.Walk(l.rootOf("t")); err != nil {
		fatal("walk", err)
	}
}

// partC runs the same kind of insert into a table with two secondary indexes.
func partC(ctx context.Context, dir string) {
	part, workloadName = "C", nil
	l := openLab(ctx, filepath.Join(dir, "indexed.db"), "wal",
		"CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT, score INTEGER, v BLOB)",
		"CREATE INDEX t_email ON t(email)",
		"CREATE INDEX t_score ON t(score)")
	defer l.close()
	histogram := map[int]int{}
	const inserts = 200
	ok := true
	for n := 1; n <= inserts; n++ {
		rowid := int64(n)
		email := fmt.Sprintf("user%03d@example.com", (n*37)%inserts)
		score := (n * 53) % 97
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, email, score, v) VALUES(?, ?, ?, ?)", rowid, email, score, rowBlob(rowid)[:200])
		histogram[len(s.changes)]++
		if !sameInts(s.walPages, s.pages()) || s.integrity != "ok" {
			ok = false
		}
		if n == 50 {
			recordStep("a common insert", s)
		}
	}
	var counts []int
	for k := range histogram {
		counts = append(counts, k)
	}
	sort.Ints(counts)
	most, mostCount := 0, 0
	for _, k := range counts {
		if histogram[k] > mostCount {
			most, mostCount = k, histogram[k]
		}
	}
	check(most == 3, "with two indexes the common insert changes three pages: the table leaf and one leaf in each index")
	check(ok, "WAL frames equal the changed pages and integrity_check = ok for all 200 inserts")
}

// partD repeats the appends of part A in rollback mode (journal_mode=DELETE).
func partD(ctx context.Context, dir string) {
	part, workloadName = "D", nil
	l := openLab(ctx, filepath.Join(dir, "rollback.db"), "delete", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	ok, ordinaryTwo, counterOnly := true, true, true
	counterBytes := []equal.ByteRange{{Start: 24, End: 28}, {Start: 92, End: 96}}
	for n := 1; n <= 12; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		hasPage1 := len(s.changes) > 0 && s.changes[0].Page == 1
		if !hasPage1 || s.integrity != "ok" {
			ok = false
		}
		if s.after.PageCount() == s.before.PageCount() {
			if len(s.changes) != 2 || !leafHoldsRowid(s.after, s.changes[1].Page, rowid) {
				ordinaryTwo = false
			}
			before, err := s.before.Page(1)
			if err != nil {
				fatal("page 1 before", err)
			}
			after, err := s.after.Page(1)
			if err != nil {
				fatal("page 1 after", err)
			}
			r, err := equal.CompareImagesIgnoring(before, after, pageSize, counterBytes)
			if err != nil {
				fatal("compare page 1", err)
			}
			if !r.Equal || len(r.IgnoredDiffering) == 0 {
				counterOnly = false
			}
		}
	}
	check(ok, "in rollback mode every commit also rewrites page 1")
	check(ordinaryTwo, "an insert that adds no page changes exactly two pages: page 1 and the table leaf that holds the new rowid")
	check(counterOnly, "on those inserts page 1 differs only in the change counter (bytes 24-27) and version-valid-for (bytes 92-95)")
}

type workload struct {
	name   string
	table  string
	create string
	keys   []int64
	size   int
}

type workloadResult struct {
	inserts        int
	onePage        int
	addedPages     int
	rebalanced     int
	minChanged     int
	maxChanged     int
	walCovers      bool
	integrityOK    bool
	splitsAtDepth2 []int
}

func shuffledKeys(n int, seed uint64) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i + 1)
	}
	rng := rand.New(rand.NewPCG(seed, 0))
	rng.Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

func ascendingKeys(n int) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i + 1)
	}
	return keys
}

func runWorkload(ctx context.Context, dir string, w workload) workloadResult {
	workloadName = w.name
	l := openLab(ctx, filepath.Join(dir, w.table+".db"), "wal", w.create)
	defer l.close()
	r := workloadResult{minChanged: -1, walCovers: true, integrityOK: true}
	insert := fmt.Sprintf("INSERT INTO %s(k, v) VALUES(?, ?)", w.table)
	prevDepth := 1
	for i, key := range w.keys {
		s := l.exec(ctx, i+1, key, w.table, insert, key, sizedBlob(key, w.size))
		changed := len(s.changes)
		r.inserts++
		switch {
		case s.after.PageCount() > s.before.PageCount():
			r.addedPages++
			if prevDepth == 2 && s.depth == 2 {
				r.splitsAtDepth2 = append(r.splitsAtDepth2, changed)
			}
		case changed == 1:
			r.onePage++
		default:
			r.rebalanced++
		}
		if r.minChanged < 0 || changed < r.minChanged {
			r.minChanged = changed
		}
		r.maxChanged = max(r.maxChanged, changed)
		if len(subtract(s.pages(), s.walPages)) > 0 {
			r.walCovers = false
		}
		if s.integrity != "ok" {
			r.integrityOK = false
		}
		prevDepth = s.depth
	}
	var leaves []int
	for n := 2; n <= l.census.PageCount; n++ {
		role := l.census.Page(n).Role
		if role == dbfile.RoleTableLeaf || role == dbfile.RoleIndexLeaf {
			leaves = append(leaves, mustPage(l.file, n).CellCount)
		}
	}
	sort.Ints(leaves)
	if len(leaves) > 0 {
		workloadSeq++
		tables.workloads.Row(workloadSeq, w.name, w.table, w.size, len(leaves), leaves[0], leaves[len(leaves)-1], prevDepth)
	}
	return r
}

// partE shows where "most inserts change one page" stops holding.
func partE(ctx context.Context, dir string) {
	part, workloadName = "E", nil
	// Exercise four boundary workloads: random keys with few vs many rows per
	// leaf, overflowing payloads (fillInCell allocates overflow pages at
	// btree.c:7212), and WITHOUT ROWID (an index B-tree, excluded from
	// balance_quick by the pPage->intKeyLeaf guard at btree.c:9185-9189).
	workloads := []workload{
		{"random keys, 1000-byte rows (4 per leaf)", "random_large", "CREATE TABLE random_large(k INTEGER PRIMARY KEY, v BLOB)", shuffledKeys(250, 11), 1000},
		{"random keys, 40-byte rows (about 85 per leaf)", "random_small", "CREATE TABLE random_small(k INTEGER PRIMARY KEY, v BLOB)", shuffledKeys(600, 11), 40},
		{"ascending keys, 5000-byte rows (each overflows)", "overflow", "CREATE TABLE overflow(k INTEGER PRIMARY KEY, v BLOB)", ascendingKeys(25), 5000},
		{"ascending keys, 400-byte rows, WITHOUT ROWID", "without_rowid", "CREATE TABLE without_rowid(k INTEGER PRIMARY KEY, v BLOB) WITHOUT ROWID", ascendingKeys(30), 400},
	}
	results := make([]workloadResult, len(workloads))
	for i, w := range workloads {
		results[i] = runWorkload(ctx, dir, w)
	}
	workloadName = nil
	large, small, overflow, withoutRowid := results[0], results[1], results[2], results[3]
	check(large.onePage*2 < large.inserts && large.rebalanced > 0, fmt.Sprintf("random keys with 4 rows per leaf: only %d of %d inserts change one page; %d split and %d more change several pages without adding one (balance_nonroot moves cells between siblings)", large.onePage, large.inserts, large.addedPages, large.rebalanced))
	check(small.onePage*2 > small.inserts, fmt.Sprintf("random keys with small rows: most inserts change one page again (%d of %d): the rule depends on how many rows fit in a leaf, not on the key order alone", small.onePage, small.inserts))
	check(overflow.onePage == 0 && overflow.minChanged >= 3, fmt.Sprintf("a row larger than the leaf's local limit also writes its overflow pages and page 1 (the file grows): no insert changes one page, each changes %d..%d", overflow.minChanged, overflow.maxChanged))
	allWide := len(withoutRowid.splitsAtDepth2) > 0
	for _, n := range withoutRowid.splitsAtDepth2 {
		if n <= 3 {
			allWide = false
		}
	}
	check(allWide, "a WITHOUT ROWID table is an index b-tree, and balance_quick serves table leaves only: its append splits at depth 2 go through balance_nonroot and change more than three pages")
	covers, integrity := true, true
	for _, r := range results {
		covers = covers && r.walCovers
		integrity = integrity && r.integrityOK
	}
	check(covers, "in all four, every changed page is in its commit's WAL frames")
	check(integrity, "integrity_check = ok after every insert of all four")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	set.Finish("PAGES PER INSERT")
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

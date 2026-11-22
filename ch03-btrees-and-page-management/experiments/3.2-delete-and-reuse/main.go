// Experiment 3.2 tracks page freeing, freelist reuse, file growth, VACUUM,
// auto_vacuum modes, and deleted-byte residue across secure_delete=OFF and
// secure_delete=ON in both the database file at rest and the -wal file.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 3.2
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize    = 4096
	blobBytes   = 1000
	initialRows = 400
	deleteFirst = 99
	deleteLast  = 302
	reuseRows   = 100
	growBatches = 30
	rowsPerTx   = 4
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 3.2" runs both.
var (
	set    *results.Set
	tables struct {
		settings, steps, deleteStep, reuseStep, growBatches, residue,
		largeTransaction, autoVacuum, walResidue *results.Table
	}
)

func openTables() {
	tables.settings = set.Table("settings", "name", "value")
	tables.steps = set.Table("steps", "secure_delete", "step", "what", "file_bytes", "pages", "freelist_pages", "trunk_pages", "freelist_leaves", "table_leaf_pages")
	tables.deleteStep = set.Table("delete_step", "secure_delete", "rows_deleted", "held_on_leaf_pages", "freelist_pages", "former_leaves_freed", "wal_frames")
	tables.reuseStep = set.Table("reuse_step", "secure_delete", "new_rows", "new_leaf_pages", "new_leaf_count", "taken_from_freelist")
	tables.growBatches = set.Table("grow_batches", "secure_delete", "batch", "freelist_before", "freelist_after", "pages_before", "pages_after")
	tables.residue = set.Table("residue", "secure_delete", "step", "markers_searched", "rows_sought", "rows_found", "pages_with_markers", "roles")
	tables.largeTransaction = set.Table("large_transaction", "rows_inserted", "deleted_rows", "freelist_before", "page_count_before", "freelist_after", "page_count_after")
	tables.autoVacuum = set.Table("auto_vacuum", "mode", "pages_created", "pages_after_delete", "freelist_after_delete", "pages_after_incremental_vacuum")
	tables.walResidue = set.Table("wal_residue", "seq", "location", "rows_readable", "rows_total")
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func marker(prefix string, id int64) []byte {
	return []byte(fmt.Sprintf("%s-%06d", prefix, id))
}

func rowBlob(prefix string, id int64) []byte {
	unit := append(marker(prefix, id), ' ')
	return bytes.Repeat(unit, blobBytes/len(unit)+1)[:blobBytes]
}

type fileState struct {
	size      int64
	pages     int
	freelist  int
	trunks    []int
	leaves    []int
	census    *dbfile.Census
	image     []byte
	tableLeaf map[int][]int64
}

// readState inspects a closed, at-rest database file directly from disk so no
// open SQLite connection holds POSIX locks in this process.
func readState(path string) fileState {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	s := fileState{size: int64(len(image)), pages: f.PageCount(), freelist: census.FreelistLength(), trunks: census.FreelistTrunks, leaves: census.FreelistLeaves, census: census, image: image, tableLeaf: map[int][]int64{}}
	schema, err := f.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name != "t" {
			continue
		}
		err := f.WalkTable(int(e.RootPage), func(r dbfile.Row) error {
			s.tableLeaf[r.Page] = append(s.tableLeaf[r.Page], r.Rowid)
			return nil
		})
		if err != nil {
			fatal("walk", err)
		}
	}
	return s
}

func (s fileState) freePages() map[int]bool {
	out := map[int]bool{}
	for _, p := range s.trunks {
		out[p] = true
	}
	for _, p := range s.leaves {
		out[p] = true
	}
	return out
}

func compactPages(xs []int) string {
	if len(xs) == 0 {
		return "none"
	}
	sorted := append([]int(nil), xs...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if start == prev {
			parts = append(parts, fmt.Sprint(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, x := range sorted[1:] {
		if x == prev+1 {
			prev = x
			continue
		}
		flush()
		start, prev = x, x
	}
	flush()
	return strings.Join(parts, ",")
}

type finding struct {
	rowsFound  int
	rowsSought int
	byRole     map[string]int
	pages      map[int]bool
}

type markerRange struct {
	prefix      string
	first, last int64
}

var firstDeletion = []markerRange{{"SECRET", deleteFirst, deleteLast}}

var secondDeletion = []markerRange{{"NEW", 1001, 1000 + reuseRows}, {"GROW", 2001, 2000 + growBatches*rowsPerTx}}

func searchDeleted(s fileState) finding {
	return searchMarkers(s, firstDeletion)
}

func searchMarkers(s fileState, ranges []markerRange) finding {
	f := finding{byRole: map[string]int{}, pages: map[int]bool{}}
	for _, r := range ranges {
		for id := r.first; id <= r.last; id++ {
			f.rowsSought++
			if f.searchOne(s, marker(r.prefix, id)) {
				f.rowsFound++
			}
		}
	}
	return f
}

func (f finding) searchOne(s fileState, needle []byte) bool {
	found := false
	for offset := 0; ; {
		i := bytes.Index(s.image[offset:], needle)
		if i < 0 {
			return found
		}
		at := offset + i
		page := at/pageSize + 1
		if !f.pages[page] {
			f.pages[page] = true
			f.byRole[s.census.Page(page).Role.String()]++
		}
		found = true
		offset = at + len(needle)
	}
}

// roles lists the page roles that hold markers, such as "freelist-leaf=49 table-leaf=2".
func (f finding) roles() any {
	var roles []string
	for role, n := range f.byRole {
		roles = append(roles, fmt.Sprintf("%s=%d", role, n))
	}
	if len(roles) == 0 {
		return nil
	}
	sort.Strings(roles)
	return strings.Join(roles, " ")
}

func spaced(xs []int) any {
	if len(xs) == 0 {
		return nil
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, " ")
}

func open(ctx context.Context, path string, secureDelete bool) *sqlitex.DB {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = secureDelete
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	return db
}

func session(ctx context.Context, path string, secureDelete bool, work func(db *sqlitex.DB)) {
	db := open(ctx, path, secureDelete)
	work(db)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	if integrity != "ok" {
		fatal("integrity_check", fmt.Errorf("%s", integrity))
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func execAll(ctx context.Context, db *sqlitex.DB, statement string, rows [][]any) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, args := range rows {
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
}

func pragmaInt(ctx context.Context, db *sqlitex.DB, name string) int {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		fatal(name, err)
	}
	return v
}

func insertRows(prefix string, first, count int64) [][]any {
	var rows [][]any
	for id := first; id < first+count; id++ {
		rows = append(rows, []any{id, rowBlob(prefix, id)})
	}
	return rows
}

type outcome struct {
	afterDelete       finding
	afterReuse        finding
	afterSecondDelete finding
	afterVacuum       finding
	sizes             []int64
	deleteFrames      int
}

// walFrames computes the frame count from the -wal file length (32-byte header
// plus 24-byte frame header and 4096-byte page image per frame) before close
// checkpoints and deletes the WAL.
func walFrames(path string) int {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		fatal("stat -wal", err)
	}
	if info.Size() < 32 || (info.Size()-32)%(24+pageSize) != 0 {
		fatal("-wal size", fmt.Errorf("%d bytes", info.Size()))
	}
	return int((info.Size() - 32) / (24 + pageSize))
}

func recordStep(label string, step int, what string, s fileState) {
	tables.steps.Row(label, step, what, s.size, s.pages, s.freelist, spaced(s.trunks), len(s.leaves), len(s.tableLeaf))
}

func recordResidue(label string, step int, markers string, f finding) {
	tables.residue.Row(label, step, markers, f.rowsSought, f.rowsFound, len(f.pages), f.roles())
}

func run(ctx context.Context, dir string, secureDelete bool) outcome {
	label := "OFF"
	if secureDelete {
		label = "ON"
	}
	path := filepath.Join(dir, "secure-delete-"+strings.ToLower(label), "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	part := "A"
	if secureDelete {
		part = "B"
	}
	var o outcome
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		execAll(ctx, db, insert, insertRows("SECRET", 1, initialRows))
	})
	base := readState(path)
	o.sizes = append(o.sizes, base.size)
	recordStep(label, 1, "insert the rows", base)
	var held []int
	for page, ids := range base.tableLeaf {
		for _, id := range ids {
			if id >= deleteFirst && id <= deleteLast {
				held = append(held, page)
				break
			}
		}
	}

	// Under secure_delete=OFF, freePage2 (btree.c:6836-6969) calls
	// sqlite3PagerDontWrite (pager.c:6329-6339) for freed freelist leaves, so
	// they are not written to the WAL and retain their old bytes; freeSpace
	// (btree.c:1918-2014) likewise skips zeroing freeblocks on live leaves.
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN ? AND ?", deleteFirst, deleteLast); err != nil {
			fatal("delete", err)
		}
		o.deleteFrames = walFrames(path)
	})
	deleted := readState(path)
	o.sizes = append(o.sizes, deleted.size)
	recordStep(label, 2, "delete rows", deleted)
	free := deleted.freePages()
	var freedFromHeld []int
	for _, p := range held {
		if free[p] {
			freedFromHeld = append(freedFromHeld, p)
		}
	}
	tables.deleteStep.Row(label, deleteLast-deleteFirst+1, compactPages(held), compactPages(append(append([]int(nil), deleted.trunks...), deleted.leaves...)), len(freedFromHeld), o.deleteFrames)
	set.Check(part, deleted.size == base.size, "the file does not shrink on delete")
	set.Check(part, deleted.freelist > 0 && len(freedFromHeld) > 0, "pages emptied by the delete moved to the freelist")
	o.afterDelete = searchDeleted(deleted)
	recordResidue(label, 2, "SECRET", o.afterDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		execAll(ctx, db, insert, insertRows("NEW", 1001, reuseRows))
	})
	reused := readState(path)
	o.sizes = append(o.sizes, reused.size)
	var newPages, fromFreelist []int
	for page, ids := range reused.tableLeaf {
		for _, id := range ids {
			if id >= 1001 {
				newPages = append(newPages, page)
				if free[page] {
					fromFreelist = append(fromFreelist, page)
				}
				break
			}
		}
	}
	recordStep(label, 3, "insert new rows", reused)
	tables.reuseStep.Row(label, reuseRows, compactPages(newPages), len(newPages), len(fromFreelist))
	set.Check(part, reused.size == base.size, "the new rows fit in the file: its size is unchanged")
	set.Check(part, len(newPages) > 0 && len(fromFreelist) == len(newPages), "every leaf page of the new rows is a former freelist page")
	o.afterReuse = searchDeleted(reused)
	recordResidue(label, 3, "SECRET", o.afterReuse)

	grewBeforeEmpty := false
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		next := int64(2001)
		for batch := 1; batch <= growBatches; batch++ {
			freeBefore := pragmaInt(ctx, db, "freelist_count")
			pagesBefore := pragmaInt(ctx, db, "page_count")
			execAll(ctx, db, insert, insertRows("GROW", next, rowsPerTx))
			next += rowsPerTx
			freeAfter := pragmaInt(ctx, db, "freelist_count")
			pagesAfter := pragmaInt(ctx, db, "page_count")
			if pagesAfter > pagesBefore && freeAfter > 0 {
				grewBeforeEmpty = true
			}
			tables.growBatches.Row(label, batch, freeBefore, freeAfter, pagesBefore, pagesAfter)
		}
	})
	grown := readState(path)
	o.sizes = append(o.sizes, grown.size)
	recordStep(label, 4, "grow with more inserts", grown)
	set.Check(part, !grewBeforeEmpty, "page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist")
	set.Check(part, grown.size > base.size && grown.freelist == 0, "once the freelist is used up, the file grows")

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id >= 1001"); err != nil {
			fatal("delete", err)
		}
	})
	again := readState(path)
	o.sizes = append(o.sizes, again.size)
	recordStep(label, 5, "delete the new rows", again)
	set.Check(part, again.size == grown.size, "the file still does not shrink")
	o.afterSecondDelete = searchMarkers(again, secondDeletion)
	recordResidue(label, 5, "NEW, GROW", o.afterSecondDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
			fatal("vacuum", err)
		}
	})
	vacuumed := readState(path)
	o.sizes = append(o.sizes, vacuumed.size)
	o.afterVacuum = searchMarkers(vacuumed, append(append([]markerRange(nil), firstDeletion...), secondDeletion...))
	recordStep(label, 6, "VACUUM", vacuumed)
	recordResidue(label, 6, "SECRET, NEW, GROW", o.afterVacuum)
	set.Check(part, vacuumed.freelist == 0 && vacuumed.pages <= again.pages-again.freelist, fmt.Sprintf("VACUUM is what returns space: %d pages -> %d, at least the %d freelist pages are gone", again.pages, vacuumed.pages, again.freelist))
	set.Check(part, o.afterVacuum.rowsFound == 0, "after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file")
	return o
}

func freshPath(dir, name string) string {
	path := filepath.Join(dir, name, "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	return path
}

func execOne(ctx context.Context, db *sqlitex.DB, statement string, args ...any) {
	if _, err := db.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
}

// largeTransaction verifies that allocateBtreePage (btree.c:6514-6822) pops
// freelist pages before extending the file on a per-allocation basis, so one
// transaction can drain the freelist and grow page_count in the same commit.
func largeTransaction(ctx context.Context, dir string) {
	path := freshPath(dir, "large-transaction")
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	var freeBefore, freeAfter, pagesBefore, pagesAfter int
	session(ctx, path, true, func(db *sqlitex.DB) {
		execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		execAll(ctx, db, insert, insertRows("SECRET", 1, 40))
		execOne(ctx, db, "DELETE FROM t WHERE id <= 8")
		freeBefore, pagesBefore = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
		execAll(ctx, db, insert, insertRows("NEW", 101, 32))
		freeAfter, pagesAfter = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
	})
	tables.largeTransaction.Row(32, 8, freeBefore, pagesBefore, freeAfter, pagesAfter)
	set.Check("D", freeBefore > 0 && freeAfter == 0 && pagesAfter > pagesBefore, "a transaction that needs more pages than the freelist holds takes the freelist pages first, then grows the file in the same transaction: so the rule is per page allocation, not per transaction")
}

// autoVacuumModes contrasts NONE (default, never shrinks without VACUUM), FULL
// (relocates pages and truncates during the DELETE's own commit), and
// INCREMENTAL (keeps freed pages on the freelist until PRAGMA incremental_vacuum).
func autoVacuumModes(ctx context.Context, dir string) {
	type result struct{ created, afterDelete, freeAfterDelete, afterIncremental int64 }
	outcomes := map[string]result{}
	for _, mode := range []string{sqlitex.AutoVacuumNone, sqlitex.AutoVacuumFull, sqlitex.AutoVacuumIncremental} {
		path := freshPath(dir, "auto-vacuum-"+strings.ToLower(mode))
		options := sqlitex.DefaultOptions()
		options.SingleConnection = true
		options.File.AutoVacuum = mode
		pages := func() int64 {
			if err := recipe.CheckAtRest(path); err != nil {
				fatal("at rest", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				fatal("stat", err)
			}
			return info.Size() / pageSize
		}
		withDB := func(work func(db *sqlitex.DB)) {
			db, err := sqlitex.Open(ctx, path, options)
			if err != nil {
				fatal("open", err)
			}
			work(db)
			if err := db.Close(); err != nil {
				fatal("close", err)
			}
		}
		var r result
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows("SECRET", 1, 200))
		})
		r.created = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "DELETE FROM t WHERE id <= 150")
			r.freeAfterDelete = int64(pragmaInt(ctx, db, "freelist_count"))
		})
		r.afterDelete = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "PRAGMA incremental_vacuum")
		})
		r.afterIncremental = pages()
		outcomes[mode] = r
		tables.autoVacuum.Row(mode, r.created, r.afterDelete, r.freeAfterDelete, r.afterIncremental)
	}
	none, full, incremental := outcomes[sqlitex.AutoVacuumNone], outcomes[sqlitex.AutoVacuumFull], outcomes[sqlitex.AutoVacuumIncremental]
	set.Check("E", none.afterDelete == none.created && none.afterIncremental == none.created && none.freeAfterDelete > 0, "auto_vacuum=NONE (SQLite's default and the recipe's): the file never shrinks on its own; PRAGMA incremental_vacuum does nothing")
	set.Check("E", full.afterDelete < full.created && full.freeAfterDelete == 0, "auto_vacuum=FULL: the DELETE's own commit moves pages and truncates the file, leaving no freelist")
	set.Check("E", incremental.afterDelete == incremental.created && incremental.freeAfterDelete > 0 && incremental.afterIncremental < incremental.afterDelete, "auto_vacuum=INCREMENTAL: freed pages wait on the freelist until PRAGMA incremental_vacuum truncates the file")
}

func countMarkers(data []byte, r markerRange) int {
	found := 0
	for id := r.first; id <= r.last; id++ {
		if bytes.Contains(data, marker(r.prefix, id)) {
			found++
		}
	}
	return found
}

// walResidue shows that secure_delete=ON zeroes pages in the database file but
// does not erase earlier INSERT frames already sitting in -wal until the WAL
// is reset (for example by wal_checkpoint(TRUNCATE)).
func walResidue(ctx context.Context, dir string) {
	path := freshPath(dir, "wal-residue")
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	secrets := markerRange{"SECRET", 5001, 5040}
	inWAL := func() int {
		data, err := os.ReadFile(path + "-wal")
		if err != nil {
			fatal("read -wal", err)
		}
		return countMarkers(data, secrets)
	}
	checkpoint := func(mode string) {
		result, err := sqlitex.Checkpoint(ctx, db, "main", mode)
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows(secrets.prefix, secrets.first, secrets.last-secrets.first+1))
	execOne(ctx, db, "DELETE FROM t")
	afterDelete := inWAL()
	checkpoint("PASSIVE")
	afterPassive := inWAL()
	checkpoint("TRUNCATE")
	afterTruncate := inWAL()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	atRest := countMarkers(image, secrets)
	total := int(secrets.last - secrets.first + 1)
	tables.walResidue.Row(1, "-wal after the DELETE", afterDelete, total)
	tables.walResidue.Row(2, "-wal after wal_checkpoint(PASSIVE)", afterPassive, total)
	tables.walResidue.Row(3, "-wal after wal_checkpoint(TRUNCATE)", afterTruncate, total)
	tables.walResidue.Row(4, "database file at rest", atRest, total)
	set.Check("F", afterDelete == total && afterPassive == total, "secure_delete zeroes the database pages, but the -wal still holds the frames the INSERT wrote: every deleted row stays readable there, even after a PASSIVE checkpoint copied the zeroed pages back")
	set.Check("F", afterTruncate == 0 && atRest == 0, "only when the WAL is reset (here by wal_checkpoint(TRUNCATE)) are the deleted rows gone from the files on disk")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.2")
	set = results.MustOpen(dir)
	openTables()
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	for _, setting := range [][2]any{{"page_size", pageSize}, {"blob_bytes", blobBytes}, {"initial_rows", initialRows}, {"delete_first", deleteFirst}, {"delete_last", deleteLast}, {"reuse_rows", reuseRows}, {"grow_batches", growBatches}, {"rows_per_tx", rowsPerTx}} {
		tables.settings.Row(setting[0], setting[1])
	}
	off := run(ctx, dir, false)
	on := run(ctx, dir, true)
	set.Check("C", on.deleteFrames > off.deleteFrames, "secure_delete costs WAL frames: the zeroed pages are written, while without it freed leaf pages are not written at all")
	set.Check("C", off.afterDelete.rowsFound > 0, "without secure_delete the deleted payload bytes are still in the file")
	set.Check("C", off.afterDelete.byRole["freelist-leaf"] > 0, "without secure_delete freelist leaf pages keep their old content")
	set.Check("C", off.afterSecondDelete.rowsFound > 0, "the second delete leaves its rows readable too without secure_delete")
	set.Check("C", on.afterDelete.rowsFound == 0 && on.afterReuse.rowsFound == 0 && on.afterSecondDelete.rowsFound == 0, "with secure_delete the deleted payload bytes are nowhere in the database file at rest")
	set.Check("C", fmt.Sprint(off.sizes) == fmt.Sprint(on.sizes), "secure_delete changes the bytes, not the layout: file sizes are the same at every step")
	largeTransaction(ctx, dir)
	autoVacuumModes(ctx, dir)
	walResidue(ctx, dir)
	set.Finish("DELETE AND REUSE")
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

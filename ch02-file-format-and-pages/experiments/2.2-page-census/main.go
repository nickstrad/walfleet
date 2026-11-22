// Experiment 2.2 runs internal/dbfile's complete page census on the closed R0
// database, validates every B-tree and overflow page against SQLite's dbstat
// virtual table and every table's rows against SELECT, exercises the local
// payload boundary, multi-trunk freelists, auto-vacuum pointer-map pages, and
// the 1 GiB lock-byte page, and tests nine damaged files against integrity_check.
//
// Run from the repo root:
//
//	go run ./ch02-file-format-and-pages/experiments/2.2-page-census
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"

	_ "modernc.org/sqlite"
)

var (
	set       *results.Set
	files     *results.Table
	roles     *results.Table
	freelists *results.Table
	constants *results.Table
	special   *results.Table
)

func openTables(dir string) {
	set = results.MustOpen(dir)
	files = set.Table("files", "part", "scenario", "page_size", "usable_size", "page_count", "page_count_source", "pragma_page_count", "freelist_count", "pragma_freelist_count", "btree_pages_compared", "not_in_dbstat")
	roles = set.Table("roles", "scenario", "seq", "role", "pages")
	freelists = set.Table("freelist_pages", "scenario", "seq", "page", "kind", "leaves_listed")
	constants = set.Table("constants", "part", "name", "value", "note")
	special = set.Table("special_pages", "scenario", "role", "page")
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

func openSQLite(path string, readOnly bool) *sql.DB {
	query := url.Values{}
	if readOnly {
		query.Set("mode", "ro")
	}
	uri := must(sqlitex.FileURI(path, query))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	return db
}

func queryInt(ctx context.Context, db *sql.DB, query string) int {
	var v int
	if err := db.QueryRowContext(ctx, query).Scan(&v); err != nil {
		fatal(query, err)
	}
	return v
}

func closeAtRest(db *sql.DB, path string) {
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

type statRow struct {
	Tree    string
	Type    string
	Cells   int
	Payload int
	Unused  int
	MaxPay  int
}

func readDBStat(ctx context.Context, db *sql.DB) map[int]statRow {
	rows, err := db.QueryContext(ctx, "SELECT pageno, name, pagetype, ncell, payload, unused, mx_payload FROM dbstat")
	if err != nil {
		fatal("dbstat", err)
	}
	defer rows.Close()
	out := map[int]statRow{}
	for rows.Next() {
		var n int
		var r statRow
		if err := rows.Scan(&n, &r.Tree, &r.Type, &r.Cells, &r.Payload, &r.Unused, &r.MaxPay); err != nil {
			fatal("dbstat row", err)
		}
		out[n] = r
	}
	if err := rows.Err(); err != nil {
		fatal("dbstat rows", err)
	}
	return out
}

func dbstatType(r dbfile.Role) string {
	switch r {
	case dbfile.RoleTableInterior, dbfile.RoleIndexInterior:
		return "internal"
	case dbfile.RoleTableLeaf, dbfile.RoleIndexLeaf:
		return "leaf"
	case dbfile.RoleOverflow:
		return "overflow"
	}
	return ""
}

type comparison struct {
	compared   int
	mismatches []string
	notInStat  int
}

func compareWithDBStat(c *dbfile.Census, stat map[int]statRow, pages *results.Table) comparison {
	var result comparison
	for _, p := range c.Pages {
		kind := dbstatType(p.Role)
		theirs, listed := stat[p.Number]
		if pages != nil {
			if listed {
				pages.Row(p.Number, p.Role, p.Tree, kind, p.CellCount, p.Payload, p.Unused, p.MaxPayload, theirs.Tree, theirs.Type, theirs.Cells, theirs.Payload, theirs.Unused, theirs.MaxPay)
			} else {
				pages.Row(p.Number, p.Role, p.Tree, kind, p.CellCount, p.Payload, p.Unused, p.MaxPayload, nil, nil, nil, nil, nil, nil)
			}
		}
		if kind == "" {
			if listed {
				result.mismatches = append(result.mismatches, fmt.Sprintf("page %d (%s) is in dbstat", p.Number, p.Role))
			}
			result.notInStat++
			continue
		}
		ours := statRow{p.Tree, kind, p.CellCount, p.Payload, p.Unused, int(p.MaxPayload)}
		result.compared++
		switch {
		case !listed:
			result.mismatches = append(result.mismatches, fmt.Sprintf("page %d missing from dbstat", p.Number))
		case ours != theirs:
			result.mismatches = append(result.mismatches, fmt.Sprintf("page %d: ours %+v dbstat %+v", p.Number, ours, theirs))
		}
	}
	if result.compared != len(stat) {
		result.mismatches = append(result.mismatches, fmt.Sprintf("dbstat lists %d pages, the census has %d b-tree and overflow pages", len(stat), result.compared))
	}
	return result
}

// recordRoles writes how many pages of each role the census found and returns the total.
func recordRoles(scenario string, c *dbfile.Census) int {
	counts := c.RoleCounts()
	total := 0
	for i, r := range dbfile.AllRoles {
		total += counts[r]
		roles.Row(scenario, i+1, r, counts[r])
	}
	return total
}

// recordFreelist writes the freelist pages in the order the file lists them.
func recordFreelist(scenario string, c *dbfile.Census) {
	seq := 0
	for _, trunk := range c.FreelistTrunks {
		seq++
		freelists.Row(scenario, seq, trunk, "trunk", c.Page(trunk).CellCount)
	}
	for _, leaf := range c.FreelistLeaves {
		seq++
		freelists.Row(scenario, seq, leaf, "leaf", nil)
	}
}

// census loads the closed database file into memory and runs dbfile.Census so
// no os.File descriptor stays open when later checks open SQLite on the same path.
func census(path string) (*dbfile.File, *dbfile.Census) {
	image := must(os.ReadFile(path))("read " + filepath.Base(path))
	f := must(dbfile.FromBytes(image))("parse " + filepath.Base(path))
	c := must(f.Census())("census " + filepath.Base(path))
	return f, c
}

// treeNamed looks up a B-tree entry in c by its schema name.
func treeNamed(c *dbfile.Census, name string) dbfile.Tree {
	for _, t := range c.Trees {
		if t.Name == name {
			return t
		}
	}
	fatal("tree "+name, errors.New("not in the schema"))
	return dbfile.Tree{}
}

func canonicalWrite(h hash.Hash, class string, text string) {
	fmt.Fprintf(h, "%s:%d:%s|", class, len(text), text)
}

func ourValue(h hash.Hash, v dbfile.Value) {
	switch v.Class {
	case dbfile.Null:
		canonicalWrite(h, "null", "")
	case dbfile.Integer:
		canonicalWrite(h, "integer", fmt.Sprint(v.Int))
	case dbfile.Real:
		canonicalWrite(h, "real", fmt.Sprintf("%016x", math.Float64bits(v.Float)))
	case dbfile.Text:
		canonicalWrite(h, "text", hex.EncodeToString(v.Bytes))
	default:
		canonicalWrite(h, "blob", hex.EncodeToString(v.Bytes))
	}
}

func sqliteValue(h hash.Hash, class string, value any) {
	switch class {
	case "null":
		canonicalWrite(h, "null", "")
	case "integer":
		canonicalWrite(h, "integer", fmt.Sprint(value.(int64)))
	case "real":
		canonicalWrite(h, "real", fmt.Sprintf("%016x", math.Float64bits(value.(float64))))
	case "text":
		canonicalWrite(h, "text", hex.EncodeToString([]byte(value.(string))))
	default:
		canonicalWrite(h, "blob", hex.EncodeToString(value.([]byte)))
	}
}

type tableHash struct {
	rows int
	sum  string
}

// ourTableHash walks a table B-tree directly from raw bytes and hashes its
// canonical rows; column 0 NULL is replaced by the rowid because SQLite stores
// an INTEGER PRIMARY KEY column as NULL in the record and keeps its value in
// the cell's rowid varint.
func ourTableHash(f *dbfile.File, root int) tableHash {
	h := sha256.New()
	rows := 0
	err := f.WalkTable(root, func(r dbfile.Row) error {
		rows++
		fmt.Fprintf(h, "row %d|", r.Rowid)
		for i, v := range r.Values {
			if i == 0 && v.Class == dbfile.Null {
				v = dbfile.Value{Class: dbfile.Integer, Int: r.Rowid}
			}
			ourValue(h, v)
		}
		h.Write([]byte("\n"))
		return nil
	})
	if err != nil {
		fatal("walk table", err)
	}
	return tableHash{rows, hex.EncodeToString(h.Sum(nil))}
}

func sqliteTableHash(ctx context.Context, db *sql.DB, table string, columns []string) tableHash {
	var selects []string
	for _, c := range columns {
		selects = append(selects, fmt.Sprintf("typeof(%s), %s", c, c))
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT rowid, %s FROM %s ORDER BY rowid", strings.Join(selects, ", "), table))
	if err != nil {
		fatal("select "+table, err)
	}
	defer rows.Close()
	h := sha256.New()
	count := 0
	for rows.Next() {
		var rowid int64
		cells := make([]any, 2*len(columns))
		targets := []any{&rowid}
		for i := range cells {
			targets = append(targets, &cells[i])
		}
		if err := rows.Scan(targets...); err != nil {
			fatal("scan", err)
		}
		count++
		fmt.Fprintf(h, "row %d|", rowid)
		for i := range columns {
			sqliteValue(h, cells[2*i].(string), cells[2*i+1])
		}
		h.Write([]byte("\n"))
	}
	if err := rows.Err(); err != nil {
		fatal("rows", err)
	}
	return tableHash{count, hex.EncodeToString(h.Sum(nil))}
}

var recipeColumns = map[string][]string{
	"accounts":  {"id", "name", "balance"},
	"documents": {"id", "account_id", "title", "body"},
	"events":    {"id", "account_id", "kind", "amount", "note"},
}

func partA(ctx context.Context, dir string) {
	r := recipe.Default()
	path := filepath.Join(dir, "r0.db")
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build", err)
	}
	f, c := census(path)
	schema := set.Table("schema_entries", "entry", "type", "name", "tbl_name", "rootpage", "sql")
	for _, e := range c.Schema {
		schema.Row(e.Rowid, e.Type, e.Name, e.TableName, e.RootPage, e.SQL)
	}
	trees := set.Table("trees", "name", "type", "root_page", "btree_pages", "overflow_pages", "cells")
	for _, t := range c.SortedTrees() {
		trees.Row(t.Name, t.Type, t.RootPage, t.Pages, t.Overflow, t.Cells)
	}
	recordFreelist("r0", c)
	total := recordRoles("r0", c)
	set.Check("A", total == c.PageCount, fmt.Sprintf("the census sums to page_count %d", c.PageCount))
	set.Check("A", c.Complete() == nil, "every page has exactly one role: none counted twice (the census fails on a second claim), none missed")

	db := openSQLite(path, false)
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	stat := readDBStat(ctx, db)
	pages := set.Table("pages", "page", "role", "tree", "kind", "ncell", "payload", "unused", "mx_payload", "db_tree", "db_type", "db_ncell", "db_payload", "db_unused", "db_mx_payload")
	cmp := compareWithDBStat(c, stat, pages)
	files.Row("A", "r0", c.PageSize, c.UsableSize, c.PageCount, f.PageCountSource(), pageCount, c.FreelistLength(), freelist, cmp.compared, cmp.notInStat)
	set.Check("A", pageCount == c.PageCount, fmt.Sprintf("PRAGMA page_count = %d", pageCount))
	set.Check("A", freelist == c.FreelistLength(), fmt.Sprintf("freelist length %d = PRAGMA freelist_count %d", c.FreelistLength(), freelist))
	set.Check("A", len(cmp.mismatches) == 0, fmt.Sprintf("all %d b-tree and overflow pages match dbstat in tree, type, ncell, payload, unused and mx_payload; the %d other pages are not in dbstat", cmp.compared, cmp.notInStat))

	chainRows := set.Table("overflow_chains", "row_id", "body_bytes", "leaf_page", "overflow_pages", "chain_length")
	var overflowPages int
	err := f.WalkTable(treeNamed(c, "documents").RootPage, func(row dbfile.Row) error {
		if len(row.OverflowPages) == 0 {
			return nil
		}
		overflowPages += len(row.OverflowPages)
		chainRows.Row(row.Rowid, len(row.Values[3].Bytes), row.Page, strings.Trim(fmt.Sprint(row.OverflowPages), "[]"), len(row.OverflowPages))
		return nil
	})
	if err != nil {
		fatal("documents", err)
	}
	set.Check("A", overflowPages == c.RoleCounts()[dbfile.RoleOverflow], "the chains followed from the documents rows hold every overflow page in the file")

	hashes := set.Table("row_hashes", "table_name", "our_rows", "sqlite_rows", "our_sha256", "sqlite_sha256")
	for _, e := range c.Schema {
		columns, isRecipe := recipeColumns[e.Name]
		if !isRecipe {
			continue
		}
		ours := ourTableHash(f, e.RootPage)
		theirs := sqliteTableHash(ctx, db, e.Name, columns)
		hashes.Row(e.Name, ours.rows, theirs.rows, ours.sum, theirs.sum)
		set.Check("A", ours == theirs, fmt.Sprintf("table %s: same rows and values, read from the bytes and through SQLite", e.Name))
	}
	closeAtRest(db, path)
}

func newDB(ctx context.Context, path string, edit func(*sqlitex.Options), statements ...string) {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	if edit != nil {
		edit(&options)
	}
	db := must(sqlitex.Open(ctx, path, options))("open " + filepath.Base(path))
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func checkAgainstSQLite(ctx context.Context, path, part string, f *dbfile.File, c *dbfile.Census, label string) {
	db := openSQLite(path, false)
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	cmp := compareWithDBStat(c, readDBStat(ctx, db), nil)
	closeAtRest(db, path)
	files.Row(part, strings.TrimSuffix(filepath.Base(path), ".db"), c.PageSize, c.UsableSize, c.PageCount, f.PageCountSource(), pageCount, c.FreelistLength(), freelist, cmp.compared, cmp.notInStat)
	set.Check(part, pageCount == c.PageCount && freelist == c.FreelistLength() && c.Complete() == nil && len(cmp.mismatches) == 0,
		fmt.Sprintf("%s: census complete, page_count %d and freelist_count %d agree, %d pages match dbstat", label, pageCount, freelist, cmp.compared))
}

func partB(ctx context.Context, dir string) {
	maxLocal, minLocal := dbfile.MaxLocalPayload(4096, dbfile.TableLeaf), dbfile.MinLocalPayload(4096)
	constants.Row("B", "table leaf maxLocal", maxLocal, "usable-35, usable size 4096")
	constants.Row("B", "table leaf minLocal", minLocal, "(usable-12)*32/255-23")
	constants.Row("B", "index maxLocal", dbfile.MaxLocalPayload(4096, dbfile.IndexLeaf), "(usable-12)*64/255-23")
	path := filepath.Join(dir, "limit.db")
	newDB(ctx, path, nil,
		"CREATE TABLE b(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO b VALUES(1, zeroblob(4057))",
		"INSERT INTO b VALUES(2, zeroblob(4058))",
		"INSERT INTO b VALUES(3, zeroblob(8149))")
	f, c := census(path)
	// Record each row's parsed local/overflow split from WalkTable rather than
	// checking standalone arithmetic constants.
	type split struct {
		payload    uint64
		local      int
		overflow   int
		lastFilled int
	}
	splits := map[int64]split{}
	splitRows := set.Table("payload_splits", "row_id", "blob_bytes", "payload_bytes", "local_bytes", "overflow_pages", "last_overflow_bytes")
	err := f.WalkTable(2, func(row dbfile.Row) error {
		page := must(f.BTreePage(row.Page))("page")
		for i := range page.CellCount {
			cell := must(page.Cell(i))("cell")
			if cell.Rowid != row.Rowid {
				continue
			}
			chain := must(f.OverflowChain(row.Page, cell))("chain")
			s := split{payload: cell.PayloadSize, local: cell.LocalSize, overflow: len(chain)}
			if len(chain) > 0 {
				s.lastFilled = chain[len(chain)-1].Payload
			}
			splits[row.Rowid] = s
			splitRows.Row(row.Rowid, len(row.Values[1].Bytes), cell.PayloadSize, cell.LocalSize, len(chain), s.lastFilled)
		}
		return nil
	})
	if err != nil {
		fatal("walk", err)
	}
	set.Check("B", len(splits) == 3, "limit.db holds the three rows 1, 2 and 3")
	set.Check("B", splits[1] == split{4061, 4061, 0, 0}, "row 1: payload 4061 (= maxLocal) stays entirely on the leaf")
	set.Check("B", splits[2] == split{4062, minLocal, 1, 4062 - minLocal}, fmt.Sprintf("row 2: payload 4062 keeps only minLocal %d bytes on the leaf and spills %d to one overflow page", minLocal, 4062-minLocal))
	set.Check("B", splits[3] == split{8153, 4061, 1, 4092}, "row 3: payload 8153 keeps 4061 on the leaf so its overflow exactly fills one page (4092 bytes)")
	checkAgainstSQLite(ctx, path, "B", f, c, "local limit")
}

func partC(ctx context.Context, dir string) {
	path := filepath.Join(dir, "freelist.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 1024 },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 600) INSERT INTO big SELECT i, zeroblob(1000) FROM n ORDER BY i",
		"DELETE FROM big")
	f, c := census(path)
	constants.Row("C", "leaf numbers per trunk", c.UsableSize/4-2, "usable/4-2 at page size 1024")
	recordFreelist("freelist", c)
	recordRoles("freelist", c)
	set.Check("C", len(c.FreelistTrunks) >= 2, "the freelist spans more than one trunk page")
	checkAgainstSQLite(ctx, path, "C", f, c, "long freelist")
}

func partD(ctx context.Context, dir string) {
	path := filepath.Join(dir, "autovacuum.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 1024; o.File.AutoVacuum = sqlitex.AutoVacuumFull },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 400) INSERT INTO big SELECT i, zeroblob(900) FROM n ORDER BY i")
	f, c := census(path)
	var maps []int
	for _, p := range c.Pages {
		if p.Role == dbfile.RolePointerMap {
			maps = append(maps, p.Number)
		}
	}
	constants.Row("D", "header byte 52 (largest root page)", f.Header.LargestRootPage, "not 0, so auto-vacuum is on: "+f.Header.AutoVacuumName())
	constants.Row("D", "pointer-map spacing", c.UsableSize/5+1, "usable/5+1 pages, from page 2")
	for _, n := range maps {
		special.Row("autovacuum", dbfile.RolePointerMap, n)
	}
	recordRoles("autovacuum", c)
	set.Check("D", slices.Equal(maps, []int{2, 2 + c.UsableSize/5 + 1}), "pointer-map pages are pages 2 and 2 + usable/5 + 1 in this 408-page file")
	checkAgainstSQLite(ctx, path, "D", f, c, "auto_vacuum=FULL")
}

func partE(ctx context.Context, dir string) {
	// Construct a > 1 GiB sparse file at page size 65536 so the 1 GiB
	// lock-byte page (1073741824/65536 + 1 = 16385) lies inside the database
	// without allocating 1 GiB of disk blocks; page 3 lists all remaining pages
	// except 16385 as freelist leaves so SQLite's integrity_check passes.
	path := filepath.Join(dir, "large.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 65536; o.File.JournalMode = sqlitex.JournalDelete },
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')")
	const pageSize = 65536
	lock := dbfile.LockByteOffset/pageSize + 1
	pages := lock + 1
	image := must(os.ReadFile(path))("read")
	var leaves []int
	for n := 4; n <= pages; n++ {
		if n != lock {
			leaves = append(leaves, n)
		}
	}
	binary.BigEndian.PutUint32(image[28:], uint32(pages))
	binary.BigEndian.PutUint32(image[32:], 3)
	binary.BigEndian.PutUint32(image[36:], uint32(1+len(leaves)))
	trunk := make([]byte, pageSize)
	binary.BigEndian.PutUint32(trunk[4:], uint32(len(leaves)))
	for i, leaf := range leaves {
		binary.BigEndian.PutUint32(trunk[8+4*i:], uint32(leaf))
	}
	out := must(os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0))("open for edit")
	_, err := out.Write(image[:2*pageSize])
	if err == nil {
		_, err = out.WriteAt(trunk, 2*pageSize)
	}
	if err == nil {
		err = out.Truncate(int64(pages) * pageSize)
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		fatal("write sparse file", err)
	}
	constants.Row("E", "in-header size (pages)", pages, "page 3 is a freelist trunk listing pages 4.."+fmt.Sprint(pages)+" except the lock-byte page")
	constants.Row("E", "file size (bytes)", int64(pages)*pageSize, "1 GiB is 1073741824; everything after page 3 is a hole")
	constants.Row("E", "lock-byte page", lock, "1073741824/65536 + 1")
	fmt.Printf("INFO: blocks on disk: %s\n", diskUsage(path))
	// A > 1 GiB sparse file is too large to load with os.ReadFile, so open it
	// directly with dbfile.Open and close the descriptor before SQLite opens it.
	f := must(dbfile.Open(path))("parse " + filepath.Base(path))
	c, err := f.Census()
	source := f.PageCountSource()
	if closeErr := f.Close(); closeErr != nil {
		fatal("close parsed file", closeErr)
	}
	if err != nil {
		fatal("census "+filepath.Base(path), err)
	}
	recordRoles("large", c)
	special.Row("large", dbfile.RoleLockByte, lock)
	set.Check("E", c.Page(lock).Role == dbfile.RoleLockByte && c.Complete() == nil, fmt.Sprintf("page %d is counted as the lock-byte page, not as lost", lock))
	db := openSQLite(path, true)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	closeAtRest(db, path)
	files.Row("E", "large", c.PageSize, c.UsableSize, c.PageCount, source, pageCount, c.FreelistLength(), freelist, nil, nil)
	constants.Row("E", "SQLite integrity_check", integrity, "on the edited file, read-only")
	set.Check("E", integrity == "ok" && pageCount == c.PageCount && freelist == c.FreelistLength(), "SQLite accepts the file: integrity_check ok, and page_count and freelist_count equal the census")
	if err := os.Remove(path); err != nil {
		fatal("remove large file", err)
	}
}

type corruption struct {
	name string
	edit func(image []byte)
}

func partF(ctx context.Context, dir string) {
	// Each case damages one structure in a 6-page rollback-mode copy; rollback
	// mode and mode=ro keep SQLite from altering the damaged file on open.
	base := filepath.Join(dir, "reject-base.db")
	newDB(ctx, base, func(o *sqlitex.Options) { o.File.JournalMode = sqlitex.JournalDelete },
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO t VALUES(1, zeroblob(10000))",
		"INSERT INTO t VALUES(2, 'two')",
		"INSERT INTO t VALUES(3, zeroblob(10000))",
		"DELETE FROM t WHERE id = 3")
	good := must(os.ReadFile(base))("read")
	_, c := census(base)
	basePages := set.Table("reject_base_pages", "page", "role")
	for _, p := range c.Pages {
		basePages.Row(p.Number, p.Role)
	}
	page2 := 4096
	cellPointer := page2 + 8
	firstCell := int(binary.BigEndian.Uint16(good[cellPointer:]))
	overflowOfRow1 := 0
	for _, p := range c.Pages {
		if p.Role == dbfile.RoleOverflow && overflowOfRow1 == 0 {
			overflowOfRow1 = p.Number
		}
	}
	cases := []corruption{
		{"magic: byte 0 changed", func(b []byte) { b[0] = 'X' }},
		{"truncated: last page (a freelist leaf) cut in half", nil},
		{"page 2 type byte 0x0d -> 0x07", func(b []byte) { b[page2] = 0x07 }},
		{"page 2 cell count 2 -> 3000", func(b []byte) { binary.BigEndian.PutUint16(b[page2+3:], 3000) }},
		{"page 2 first cell pointer -> 9", func(b []byte) { binary.BigEndian.PutUint16(b[cellPointer:], 9) }},
		{"row 1 payload size varint -> 0x7f", func(b []byte) { b[page2+firstCell] = 0x7f }},
		{fmt.Sprintf("overflow page %d points back to itself", overflowOfRow1), func(b []byte) {
			binary.BigEndian.PutUint32(b[(overflowOfRow1-1)*4096:], uint32(overflowOfRow1))
		}},
		{"freelist trunk (header 32) -> page 2, a table page", func(b []byte) { binary.BigEndian.PutUint32(b[32:], 2) }},
		{"freelist count (header 36) + 1", func(b []byte) { b[39]++ }},
	}
	rejections := set.Table("rejections", "case_no", "damage", "ours", "sqlite")
	for i, k := range cases {
		image := append([]byte(nil), good...)
		if k.edit == nil {
			image = image[:len(image)-2048]
		} else {
			k.edit(image)
		}
		path := filepath.Join(dir, fmt.Sprintf("reject-%d.db", i+1))
		if err := os.WriteFile(path, image, 0o644); err != nil {
			fatal("write", err)
		}
		ours := parseAll(image)
		theirs := sqliteVerdict(ctx, path)
		rejections.Row(fmt.Sprintf("F%d", i+1), k.name, ours, theirs)
		set.Check("F", ours != "accepted" && !strings.HasPrefix(ours, "PANIC"), fmt.Sprintf("F%d: our parser returns an error, without panicking", i+1))
		if k.edit == nil {
			set.Check("F", strings.HasSuffix(theirs, ": ok"), "F2: SQLite rounds a partial last page up and reads the missing bytes as zeros, so it reports ok; our parser is stricter and refuses the file")
		}
	}
}

func parseAll(image []byte) (verdict string) {
	defer func() {
		if r := recover(); r != nil {
			verdict = fmt.Sprintf("PANIC %v", r)
		}
	}()
	f, err := dbfile.FromBytes(image)
	if err != nil {
		return "error: " + err.Error()
	}
	c, err := f.Census()
	if err == nil {
		err = c.Complete()
	}
	if err == nil {
		for _, e := range c.Schema {
			if e.Type == "table" && e.RootPage > 0 {
				if err = f.WalkTable(e.RootPage, func(dbfile.Row) error { return nil }); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return "accepted"
}

func sqliteVerdict(ctx context.Context, path string) string {
	db := openSQLite(path, true)
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "error: " + err.Error()
		}
		lines = append(lines, strings.Split(line, "\n")...)
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	if len(lines) > 4 {
		lines = append(lines[:4], fmt.Sprintf("(+%d more lines)", len(lines)-4))
	}
	return "integrity_check: " + strings.Join(lines, " / ")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("2.2")
	openTables(dir)
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	partF(ctx, dir)
	set.Finish("CENSUS")
}

var errNoUsage = errors.New("disk usage unknown on this system")

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

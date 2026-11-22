// Experiment 3.3 implements Rebuild Ladder rung R2 (canonical logical capture
// and replay) and tests E1, E2, and E3 equality across different build
// histories, a sqlite3_backup copy, dbhash blind spots, and VACUUM INTO's
// canonical-form boundaries.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 3.3
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize    = 4096
	shuffleSeed = 3
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 3.3" runs both.
var (
	set    *results.Set
	tables struct {
		builds, r0Tables, r0Facts, capture, captureLines, compare, headerValues,
		canonicalHeader, pairs, limits *results.Table
	}
)

func openTables() {
	tables.builds = set.Table("builds", "seq", "name", "how", "bytes", "file_sha256", "logical_sha256", "dump_sha256", "dbhash", "canonical_sha256")
	tables.r0Tables = set.Table("r0_tables", "table_name", "rows")
	tables.r0Facts = set.Table("r0_facts", "name", "value")
	tables.capture = set.Table("capture", "path", "bytes", "lines", "sha256")
	tables.captureLines = set.Table("capture_lines", "line_no", "text")
	tables.compare = set.Table("compare", "seq", "part", "subject", "what", "level", "equal", "differing_pages", "pages_total", "size_a", "size_b", "header_bytes_differing", "differences")
	tables.headerValues = set.Table("header_values", "part", "subject", "field", "value_a", "value_b")
	tables.canonicalHeader = set.Table("canonical_header", "bytes", "byte_18", "byte_19", "change_counter", "schema_cookie")
	tables.pairs = set.Table("pairs", "seq", "pair", "ours_equal", "dbhash_equal", "first_difference")
	tables.limits = set.Table("limits", "seq", "case_name", "dbhash_sources_equal", "captures_equal")
}

var compareSeq int

// recordFiles writes an E1 or E2 comparison; level is "E1" or "E2".
func recordFiles(part, subject, what, level string, r equal.Result) {
	var header any
	if len(r.IgnoredDiffering) > 0 {
		parts := make([]string, len(r.IgnoredDiffering))
		for i, b := range r.IgnoredDiffering {
			parts[i] = fmt.Sprint(b)
		}
		header = strings.Join(parts, " ")
	}
	compareSeq++
	tables.compare.Row(compareSeq, part, subject, what, level, r.Equal, r.DifferingPages, r.SizeA/int64(r.PageSize), r.SizeA, r.SizeB, header, nil)
}

// recordLogical writes an E3 comparison.
func recordLogical(part, subject, what string, r equal.LogicalResult) {
	compareSeq++
	tables.compare.Row(compareSeq, part, subject, what, "E3", r.Equal, nil, nil, nil, nil, nil, strings.Join(r.Differences, "; "))
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
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

type row struct {
	table  equal.TableInfo
	values []any
}

type content struct {
	schema      []string
	rows        []row
	userVersion int64
}

func readContent(ctx context.Context, path string) content {
	db := must(equal.OpenExisting(ctx, path))("open " + filepath.Base(path))
	defer db.Close()
	var c content
	rows := must(db.QueryContext(ctx, "SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY rowid"))("schema")
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			fatal("schema", err)
		}
		c.schema = append(c.schema, s)
	}
	if err := rows.Err(); err != nil {
		fatal("schema", err)
	}
	if err := rows.Close(); err != nil {
		fatal("schema", err)
	}
	err := equal.WalkLogical(ctx, db, equal.LogicalVisitor{Row: func(t equal.TableInfo, values []any) error {
		if strings.HasPrefix(t.Name, "sqlite_") {
			return fmt.Errorf("table %s: this lab copies user tables only", t.Name)
		}
		c.rows = append(c.rows, row{table: t, values: values})
		return nil
	}})
	if err != nil {
		fatal("read rows", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&c.userVersion); err != nil {
		fatal("user_version", err)
	}
	return c
}

func insertSQL(t equal.TableInfo) string {
	width := len(t.Columns)
	if !t.WithoutRowid {
		width++
	}
	marks := make([]string, width)
	for i := range marks {
		marks[i] = "?"
	}
	return insertStatement(t, marks)
}

func rowidName(name string) string {
	switch strings.ToLower(name) {
	case "rowid", "_rowid_", "oid":
		return name
	}
	return equal.QuoteIdentifier(name)
}

// insertStatement builds an INSERT statement directly from the table's column
// list and value expressions (either "?" placeholders or SQL literals), avoiding
// string replacement that would corrupt identifiers containing "?".
func insertStatement(t equal.TableInfo, values []string) string {
	var names []string
	if !t.WithoutRowid {
		names = append(names, rowidName(t.RowidColumn))
	}
	for _, c := range t.Columns {
		names = append(names, equal.QuoteIdentifier(c))
	}
	return fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", equal.QuoteIdentifier(t.Name), strings.Join(names, ", "), strings.Join(values, ", "))
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func execOrDie(ctx context.Context, e execer, query string, args ...any) {
	if _, err := e.ExecContext(ctx, query, args...); err != nil {
		fatal(query, err)
	}
}

func openBuild(ctx context.Context, path, mode string) *sqlitex.DB {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	options := recipe.Default().WithJournalMode(mode).SQLiteOptions()
	options.Conn.Synchronous = sqlitex.SyncOff
	return must(sqlitex.Open(ctx, path, options))("open " + filepath.Base(path))
}

func buildOneTransaction(ctx context.Context, path, mode string, c content) {
	db := openBuild(ctx, path, mode)
	tx := must(db.BeginTx(ctx, nil))("begin")
	for _, s := range c.schema {
		execOrDie(ctx, tx, s)
	}
	for _, r := range c.rows {
		execOrDie(ctx, tx, insertSQL(r.table), r.values...)
	}
	execOrDie(ctx, tx, fmt.Sprintf("PRAGMA user_version = %d", c.userVersion))
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func buildPerStatement(ctx context.Context, path, mode string, c content, order []int) {
	db := openBuild(ctx, path, mode)
	for _, s := range c.schema {
		execOrDie(ctx, db, s)
	}
	for _, i := range order {
		r := c.rows[i]
		execOrDie(ctx, db, insertSQL(r.table), r.values...)
	}
	execOrDie(ctx, db, fmt.Sprintf("PRAGMA user_version = %d", c.userVersion))
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

// sqlLiteral renders a scanned SQLite storage-class value as a SQL literal
// that round-trips with the exact same storage class. Embedded NUL bytes in
// TEXT are split with ||char(0)|| because SQLite's parser stops at raw NULs.
func sqlLiteral(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		switch {
		case math.IsInf(v, 1):
			return "1e999"
		case math.IsInf(v, -1):
			return "-1e999"
		}
		text := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eEn") {
			text += ".0"
		}
		return text
	case string:
		parts := strings.Split(v, "\x00")
		for i, part := range parts {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "''") + "'"
		}
		return strings.Join(parts, "||char(0)||")
	case []byte:
		return "X'" + strings.ToUpper(hex.EncodeToString(v)) + "'"
	}
	fatal("literal", fmt.Errorf("unsupported value %T", value))
	return ""
}

// dump emits rung R2's deterministic SQL capture using equal.WalkLogical.
// CREATE statements follow main.sqlite_schema rowid order (creation order) so
// a replayed database matches its source's VACUUM INTO bytes (vacuum.c:143-427
// copies tables in sqlite_schema order).
func dump(ctx context.Context, path string) string {
	db := must(equal.OpenExisting(ctx, path))("open " + filepath.Base(path))
	defer db.Close()
	conn := must(db.Conn(ctx))("conn")
	defer conn.Close()
	execOrDie(ctx, conn, "BEGIN")
	defer conn.ExecContext(ctx, "COMMIT")
	var tables, indexes, others []string
	var body strings.Builder
	var sequence []string
	pragmas := map[string]string{}
	schema := must(conn.QueryContext(ctx, "SELECT type, name, sql FROM main.sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid"))("schema")
	for schema.Next() {
		var kind, name, statement string
		if err := schema.Scan(&kind, &name, &statement); err != nil {
			fatal("schema", err)
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		switch kind {
		case "table":
			tables = append(tables, statement+";")
		case "index":
			indexes = append(indexes, statement+";")
		default:
			others = append(others, statement+";")
		}
	}
	if err := schema.Err(); err != nil {
		fatal("schema", err)
	}
	if err := schema.Close(); err != nil {
		fatal("schema", err)
	}
	err := equal.WalkLogical(ctx, conn, equal.LogicalVisitor{
		Row: func(t equal.TableInfo, values []any) error {
			literals := make([]string, len(values))
			for i, v := range values {
				literals[i] = sqlLiteral(v)
			}
			line := insertStatement(t, literals) + ";\n"
			switch {
			case t.Name == "sqlite_sequence":
				sequence = append(sequence, line)
			case strings.HasPrefix(t.Name, "sqlite_"):
				return fmt.Errorf("table %s: this capture handles sqlite_sequence only", t.Name)
			default:
				body.WriteString(line)
			}
			return nil
		},
		Pragma: func(name, value string) error {
			pragmas[name] = value
			return nil
		},
	})
	if err != nil {
		fatal("dump", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "PRAGMA page_size=%s;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\n", pragmas["page_size"], strings.ToUpper(pragmas["auto_vacuum"]), pragmas["encoding"])
	if pragmas["journal_mode"] == "wal" {
		out.WriteString("PRAGMA journal_mode=WAL;\n")
	}
	out.WriteString("BEGIN;\n")
	for _, s := range tables {
		out.WriteString(s + "\n")
	}
	out.WriteString(body.String())
	if len(sequence) > 0 {
		out.WriteString("DELETE FROM sqlite_sequence;\n")
		for _, s := range sequence {
			out.WriteString(s)
		}
	}
	for _, s := range append(indexes, others...) {
		out.WriteString(s + "\n")
	}
	fmt.Fprintf(&out, "PRAGMA user_version=%s;\nPRAGMA application_id=%s;\nCOMMIT;\n", pragmas["user_version"], pragmas["application_id"])
	return out.String()
}

func replay(ctx context.Context, script, path string) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	uri := must(sqlitex.FileURI(path, url.Values{}))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, script); err != nil {
		fatal("replay", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func vacuumInto(ctx context.Context, source, target string) {
	if err := recipe.RemoveDatabase(target); err != nil {
		fatal("remove", err)
	}
	db := must(equal.OpenExisting(ctx, source))("open " + filepath.Base(source))
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		fatal("vacuum into", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func backupCopy(ctx context.Context, source, target string) {
	if err := recipe.RemoveDatabase(target); err != nil {
		fatal("remove", err)
	}
	db := must(equal.OpenExisting(ctx, source))("open " + filepath.Base(source))
	conn := must(db.Conn(ctx))("conn")
	if _, err := sqlitex.Backup(ctx, conn, target, -1, nil); err != nil {
		fatal("backup", err)
	}
	conn.Close()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

type build struct {
	name      string
	how       string
	path      string
	sha       string
	logical   equal.Logical
	dumpSHA   string
	dbhash    string
	canonical string
	canonSHA  string
}

func atRest(path string) {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func locateDBHash() string {
	tool, err := equal.LocateDBHash()
	if err != nil {
		fmt.Printf("NO-DBHASH: %v; the dbhash column is skipped (build it with: go run ./cmd/course build-tools)\n", err)
		return ""
	}
	fmt.Printf("INFO: dbhash %s\n", displayPath(tool))
	return tool
}

func dbhashOf(ctx context.Context, tool, path string) string {
	if tool == "" {
		return ""
	}
	h := must(equal.RunDBHash(ctx, tool, path))("dbhash " + filepath.Base(path))
	atRest(path)
	return h
}

func distinct(values []string) int {
	seen := map[string]bool{}
	for _, v := range values {
		seen[v] = true
	}
	return len(seen)
}

func partA(ctx context.Context, dir, tool string) content {
	r0 := filepath.Join(dir, "builds", "r0.db")
	if err := os.MkdirAll(filepath.Dir(r0), 0o755); err != nil {
		fatal("mkdir", err)
	}
	r := recipe.Default()
	if err := r.Build(ctx, r0, r.Steps); err != nil {
		fatal("build R0", err)
	}
	c := readContent(ctx, r0)
	perTable := map[string]int{}
	for _, row := range c.rows {
		perTable[row.table.Name]++
	}
	var tableNames []string
	for name := range perTable {
		tableNames = append(tableNames, name)
	}
	sort.Strings(tableNames)
	for _, name := range tableNames {
		tables.r0Tables.Row(name, perTable[name])
	}
	builds := []*build{{name: "r0", how: "the recipe: 31 transactions with updates and deletes, index created at step 5"}}
	builds[0].path = r0
	oneTx := filepath.Join(dir, "builds", "one-transaction.db")
	buildOneTransaction(ctx, oneTx, "wal", c)
	builds = append(builds, &build{name: "one-transaction", how: "schema and every row in one transaction, tables in order, ids ascending", path: oneTx})
	perStatement := filepath.Join(dir, "builds", "per-statement.db")
	inOrder := make([]int, len(c.rows))
	for i := range inOrder {
		inOrder[i] = i
	}
	buildPerStatement(ctx, perStatement, "wal", c, inOrder)
	builds = append(builds, &build{name: "per-statement", how: "the same order, one transaction per statement", path: perStatement})
	shuffled := filepath.Join(dir, "builds", "shuffled.db")
	order := append([]int(nil), inOrder...)
	rng := rand.New(rand.NewPCG(shuffleSeed, 0))
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	buildPerStatement(ctx, shuffled, "wal", c, order)
	builds = append(builds, &build{name: "shuffled", how: fmt.Sprintf("one transaction per row, rows of all tables in a shuffled order (PCG seed %d)", shuffleSeed), path: shuffled})

	captureDir := filepath.Join(dir, "capture")
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	script := dump(ctx, r0)
	dumpPath := filepath.Join(captureDir, "r0.sql")
	if err := os.WriteFile(dumpPath, []byte(script), 0o644); err != nil {
		fatal("write dump", err)
	}
	lines := strings.Split(strings.TrimSuffix(script, "\n"), "\n")
	tables.capture.Row("capture/r0.sql", len(script), len(lines), equal.SHA256Hex([]byte(script)))
	for i, line := range lines {
		if i < 9 || i >= len(lines)-4 {
			if len(line) > 140 {
				line = line[:140] + "..."
			}
			tables.captureLines.Row(i+1, line)
		}
	}
	replayed := filepath.Join(dir, "builds", "replayed.db")
	replay(ctx, string(must(os.ReadFile(dumpPath))("read dump")), replayed)
	builds = append(builds, &build{name: "replayed", how: "the capture file replayed into an empty file (indexes created after the rows)", path: replayed})
	backup := filepath.Join(dir, "builds", "backup.db")
	backupCopy(ctx, r0, backup)
	builds = append(builds, &build{name: "backup", how: "sqlite3_backup of R0 (a page copy, for the E2 row)", path: backup})

	canonicalDir := filepath.Join(dir, "canonical")
	if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	for _, b := range builds {
		atRest(b.path)
		b.sha = must(equal.FileSHA256(b.path))("hash")
		b.logical = must(equal.LogicalHashFile(ctx, b.path))("logical hash " + b.name)
		atRest(b.path)
		b.dumpSHA = equal.SHA256Hex([]byte(dump(ctx, b.path)))
		b.dbhash = dbhashOf(ctx, tool, b.path)
		b.canonical = filepath.Join(canonicalDir, b.name+".db")
		vacuumInto(ctx, b.path, b.canonical)
		atRest(b.path)
		atRest(b.canonical)
		b.canonSHA = must(equal.FileSHA256(b.canonical))("hash")
		if again := must(equal.FileSHA256(b.path))("hash"); again != b.sha {
			fatal("hash", fmt.Errorf("%s changed while it was hashed", b.name))
		}
	}
	for i, b := range builds {
		info := must(os.Stat(b.path))("stat")
		tables.builds.Row(i+1, b.name, b.how, info.Size(), b.sha, b.logical.SHA256, b.dumpSHA, orNil(b.dbhash), b.canonSHA)
	}
	r0Logical := builds[0].logical
	tables.r0Facts.Row("schema_rows", r0Logical.SchemaRows)
	for _, p := range r0Logical.Pragmas {
		tables.r0Facts.Row(p.Name, p.Value)
	}
	tables.r0Facts.Row("integrity_check", r0Logical.Integrity)
	tables.r0Facts.Row("foreign_key_check_rows", len(r0Logical.ForeignKeyCheck))
	var shas, logicals, dumps, hashes, canon []string
	for _, b := range builds {
		e1 := must(equal.CompareFiles(r0, b.path, pageSize))("E1")
		e2 := must(equal.ComparePageFiles(r0, b.path, pageSize))("E2")
		e3 := equal.CompareLogical(builds[0].logical, b.logical)
		recordFiles("A", b.name, "build vs r0", "E1", e1)
		recordFiles("A", b.name, "build vs r0", "E2", e2)
		recordLogical("A", b.name, "build vs r0", e3)
		if b.name != "backup" {
			shas = append(shas, b.sha)
		}
		logicals = append(logicals, b.logical.SHA256)
		dumps = append(dumps, b.dumpSHA)
		hashes = append(hashes, b.dbhash)
		canon = append(canon, b.canonSHA)
		if b.name == "backup" {
			set.Check("A", !e1.Equal && e2.Equal, fmt.Sprintf("the backup copy differs from R0 in bytes (%s) but is equal at E2: only declared header bytes differ", e2))
		}
	}
	set.Check("A", builds[1].sha == builds[2].sha, "in WAL mode one transaction and one transaction per statement give byte-identical files: commit boundaries alone leave no trace when rows are only appended")
	set.Check("A", builds[3].sha != builds[1].sha && builds[0].sha != builds[1].sha && builds[0].sha != builds[3].sha, "insert order (shuffled) and history (R0's updates and deletes) each give a different file")
	set.Check("A", distinct(logicals) == 1, "every build has the same logical hash")
	allE3 := true
	for _, b := range builds {
		if !equal.CompareLogical(builds[0].logical, b.logical).Equal {
			allE3 = false
		}
	}
	set.Check("A", allE3, "every build is equal to R0 at E3 (logical hash, integrity_check ok, foreign_key_check unchanged)")
	set.Check("A", distinct(dumps) == 1, "the ordered logical capture is itself identical for every build: it depends on content and the schema's creation order, not on insert order, batching or updates")
	set.Check("A", distinct(canon[:5]) == 1, "after VACUUM INTO every build, the replayed copy included, has the same sha256 (E1 between canonical forms)")
	// sqlite3_backup_step sets the destination schema cookie to old+1
	// (backup.c:448; 0+1=1 on a fresh file), and sqlite3RunVacuum copies the
	// source schema cookie +1 (vacuum.c:143-427), leaving byte 43 differing.
	backupCanon := builds[5].canonical
	ce1 := must(equal.CompareFiles(builds[0].canonical, backupCanon, pageSize))("E1")
	ce2 := must(equal.ComparePageFiles(builds[0].canonical, backupCanon, pageSize))("E2")
	recordFiles("A", "backup", "canonical form vs canonical form of r0", "E1", ce1)
	recordFiles("A", "backup", "canonical form vs canonical form of r0", "E2", ce2)
	recordCookies("A", "backup", builds[0].canonical, backupCanon)
	set.Check("A", !ce1.Equal && ce2.Equal && fmt.Sprint(ce2.IgnoredDiffering) == "[43]", "except the backup copy: the backup API sets the copy's schema cookie to (old destination cookie + 1) = 1 (backup.c:448), VACUUM INTO carries the cookie + 1, so its canonical form differs at byte 43 only")
	if tool != "" {
		set.Check("A", distinct(hashes) == 1, "dbhash from the pinned tree also calls every build equal: it agrees with our logical hash on equality")
	}
	recordCanonicalHeader(builds[0].canonical)
	ok := true
	for _, b := range builds {
		l := must(equal.LogicalHashFile(ctx, b.canonical))("logical hash")
		if l.SHA256 == b.logical.SHA256 {
			ok = false
		}
		withoutMode := sameExceptJournalMode(b.logical, l)
		if !withoutMode {
			ok = false
		}
	}
	set.Check("A", ok, "VACUUM INTO keeps every row, the schema and user_version/application_id, but writes a rollback-mode file: its logical hash differs from the source only in journal_mode")
	return c
}

func rollbackPair(ctx context.Context, dir string, c content) {
	one := filepath.Join(dir, "builds", "rollback-one-transaction.db")
	per := filepath.Join(dir, "builds", "rollback-per-statement.db")
	buildOneTransaction(ctx, one, "delete", c)
	order := make([]int, len(c.rows))
	for i := range order {
		order[i] = i
	}
	buildPerStatement(ctx, per, "delete", c, order)
	atRest(one)
	atRest(per)
	e1 := must(equal.CompareFiles(one, per, pageSize))("E1")
	e2 := must(equal.ComparePageFiles(one, per, pageSize))("E2")
	ia, ib := must(os.ReadFile(one))("read"), must(os.ReadFile(per))("read")
	recordFiles("A2", "rollback-one-transaction", "vs rollback-per-statement", "E1", e1)
	recordFiles("A2", "rollback-one-transaction", "vs rollback-per-statement", "E2", e2)
	tables.headerValues.Row("A2", "rollback-one-transaction", "change_counter", be32(ia[24:28]), be32(ib[24:28]))
	set.Check("A2", !e1.Equal && e2.Equal, "in rollback mode the batching shows, but only in the change counter (bytes 24-27 and 92-95 count commits): E2 equal")
}

func sameExceptJournalMode(a, b equal.Logical) bool {
	diff := equal.CompareLogical(a, b)
	return len(diff.Differences) == 1 && strings.HasPrefix(diff.Differences[0], "pragma journal_mode")
}

func recordCanonicalHeader(path string) {
	image := must(os.ReadFile(path))("read")
	tables.canonicalHeader.Row(len(image), image[18], image[19], be32(image[24:28]), be32(image[40:44]))
}

func recordCookies(part, subject, a, b string) {
	ia, ib := must(os.ReadFile(a))("read"), must(os.ReadFile(b))("read")
	tables.headerValues.Row(part, subject, "schema_cookie", be32(ia[40:44]), be32(ib[40:44]))
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func newDB(ctx context.Context, path string, statements ...string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	db := must(sqlitex.Open(ctx, path, options))("open")
	for _, s := range statements {
		execOrDie(ctx, db, s)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	atRest(path)
	return path
}

type pair struct {
	name   string
	a, b   []string
	ours   string
	dbhash bool
}

// partB exercises six logical-difference pairs where weaker tools fail,
// including dbhash's unframed value concatenation and missing table boundaries
// in tool/dbhash.c:281-360 and tool/dbhash.c:379-491.
func partB(ctx context.Context, dir, tool string) {
	pairs := []pair{
		{"implicit rowids (1,2 vs 5,9)", []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(1, 'x'), (2, 'y')"}, []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(5, 'x'), (9, 'y')"}, "table t differs", true},
		{"sqlite_sequence (1 vs 2)", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')", "INSERT INTO t(v) VALUES('y')", "DELETE FROM t WHERE id = 2"}, "table sqlite_sequence differs", true},
		{"user_version (1 vs 2)", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 1"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 2"}, "pragma user_version 1 vs 2", true},
		{"integer 1 vs real 1.0", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1)"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1.0)"}, "table t differs", false},
		{"value boundary ('a3','b' vs 'a','3b')", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a3', 'b')"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a', '3b')"}, "table t differs", true},
		{"row moved between tables", []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x'), (2, 'y')", "INSERT INTO b(rowid, v) VALUES(1, 'z')"}, []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x')", "INSERT INTO b(rowid, v) VALUES(1, 'y'), (2, 'z')"}, "table a differs", true},
	}
	type claim struct {
		ok   bool
		text string
	}
	var claims []claim
	for i, p := range pairs {
		a := newDB(ctx, filepath.Join(dir, "pairs", fmt.Sprintf("%d-a.db", i+1)), p.a...)
		b := newDB(ctx, filepath.Join(dir, "pairs", fmt.Sprintf("%d-b.db", i+1)), p.b...)
		result, _, _, err := equal.CompareLogicalFiles(ctx, a, b)
		if err != nil {
			fatal("compare", err)
		}
		var dbhashEqual any
		if tool != "" {
			ha, hb := dbhashOf(ctx, tool, a), dbhashOf(ctx, tool, b)
			dbhashEqual = ha == hb
			claims = append(claims, claim{(ha == hb) == p.dbhash, fmt.Sprintf("%s: dbhash says %s", p.name, sameWord(ha == hb))})
		}
		first := ""
		if len(result.Differences) > 0 {
			first = result.Differences[0]
		}
		tables.pairs.Row(i+1, p.name, result.Equal, dbhashEqual, orNil(first))
		claims = append(claims, claim{!result.Equal && strings.Contains(first, p.ours), fmt.Sprintf("%s: our logical hash tells the two apart", p.name)})
	}
	for _, c := range claims {
		set.Check("B", c.ok, c.text)
	}
}

func sameWord(equalValues bool) string {
	if equalValues {
		return "equal"
	}
	return "differ"
}

// partC tests the three boundaries of VACUUM INTO as a canonical form
// (vacuum.c:143-427): table creation order, prior schema-cookie increments,
// and index creation timing.
func partC(ctx context.Context, dir, tool string) {
	cases := []struct {
		name string
		a, b []string
	}{
		{"tables created in another order", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')", "INSERT INTO b VALUES(1, 'y')"},
			[]string{"CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')", "INSERT INTO b VALUES(1, 'y')"}},
		{"a table created and dropped in the history", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')"},
			[]string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE scratch(x)", "DROP TABLE scratch", "INSERT INTO a VALUES(1, 'x')"}},
		{"an index created before vs after the rows", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE INDEX a_v ON a(v)", "INSERT INTO a VALUES(1, 'x'), (2, 'y')"},
			[]string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x'), (2, 'y')", "CREATE INDEX a_v ON a(v)"}},
	}
	for i, c := range cases {
		a := newDB(ctx, filepath.Join(dir, "limits", fmt.Sprintf("%d-a.db", i+1)), c.a...)
		b := newDB(ctx, filepath.Join(dir, "limits", fmt.Sprintf("%d-b.db", i+1)), c.b...)
		e3, _, _, err := equal.CompareLogicalFiles(ctx, a, b)
		if err != nil {
			fatal("compare", err)
		}
		ca, cb := strings.TrimSuffix(a, ".db")+"-vacuum.db", strings.TrimSuffix(b, ".db")+"-vacuum.db"
		vacuumInto(ctx, a, ca)
		vacuumInto(ctx, b, cb)
		e1 := must(equal.CompareFiles(ca, cb, pageSize))("E1")
		e2 := must(equal.ComparePageFiles(ca, cb, pageSize))("E2")
		recordLogical("C", c.name, "sources", e3)
		recordFiles("C", c.name, "VACUUM INTO outputs", "E1", e1)
		recordFiles("C", c.name, "VACUUM INTO outputs", "E2", e2)
		recordCookies("C", c.name, ca, cb)
		var dbhashEqual, capturesEqual any
		if tool != "" {
			dbhashEqual = dbhashOf(ctx, tool, a) == dbhashOf(ctx, tool, b)
		}
		switch i {
		case 0:
			set.Check("C", e3.Equal && !e1.Equal && !e2.Equal, "same content, tables created in another order: logically equal, but VACUUM INTO copies tables in sqlite_schema order, so the canonical files differ beyond the header")
			scriptA, scriptB := dump(ctx, a), dump(ctx, b)
			rb := strings.TrimSuffix(b, ".db") + "-replayed.db"
			replay(ctx, scriptB, rb)
			crb := strings.TrimSuffix(rb, ".db") + "-vacuum.db"
			vacuumInto(ctx, rb, crb)
			replayE1 := must(equal.CompareFiles(cb, crb, pageSize))("E1")
			capturesEqual = scriptA == scriptB
			recordFiles("C", c.name, "replay of the second source's capture, canonicalized, vs the second source's canonical form", "E1", replayE1)
			set.Check("C", scriptA != scriptB && replayE1.Equal, "the capture writes CREATE TABLE in sqlite_schema order, so it records the creation order too: the replay of a source canonicalizes to the source's own canonical bytes")
		case 1:
			set.Check("C", e3.Equal && !e1.Equal && e2.Equal, "a schema change that left no trace in the content still raises the schema cookie, and VACUUM INTO carries the cookie (+1): the canonical files differ only at bytes 40-43")
		case 2:
			set.Check("C", e3.Equal && e1.Equal, "an index built before or after the rows canonicalizes to the same bytes: VACUUM INTO rebuilds every index")
		}
		tables.limits.Row(i+1, c.name, dbhashEqual, capturesEqual)
	}
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.3")
	set = results.MustOpen(dir)
	openTables()
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	tool := locateDBHash()
	c := partA(ctx, dir, tool)
	rollbackPair(ctx, dir, c)
	partB(ctx, dir, tool)
	partC(ctx, dir, tool)
	set.Finish("SAME ROWS")
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

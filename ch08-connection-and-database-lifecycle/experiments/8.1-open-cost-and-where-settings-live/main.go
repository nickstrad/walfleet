// Experiment 8.1 measures what SQLite reads at open versus at the first
// statement for 1-, 100-, and 1,000-table schemas (Part A), and determines
// empirically which course settings persist in the database file header versus
// per connection across a plain copy and VACUUM INTO (Part B).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 8.1
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const (
	packagePath    = "ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live"
	pageSize       = 4096
	firstStatement = "SELECT count(*) FROM t0001"
	markPrefix     = "MARK "
)

var (
	tableCounts = []int{1, 100, 1000}
	phases      = []string{"open", "first statement", "close"}
)

// Every measurement goes to a CSV table in <work dir>/results/. part labels
// the claims written while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		schemas, events, schemaPages, settings, placements, files, special, skipped, infoTiming *results.Table
	}
)

func openTables() {
	tables.schemas = set.Table("schemas", "tables", "file_pages", "schema_used_after_open", "schema_used_after_first")
	tables.events = set.Table("io_events", "tables", "phase", "seq", "op", "file", "byte_offset", "nbytes", "returned")
	tables.schemaPages = set.Table("schema_pages", "tables", "page", "role")
	tables.settings = set.Table("settings", "seq", "setting", "pragma", "value_set", "on_original", "fresh_default", "on_copy", "on_vacuum_into", "lives_in")
	tables.placements = set.Table("placements", "seq", "name", "pragma", "home", "header_bytes")
	tables.files = set.Table("files", "seq", "file", "at_rest", "bytes", "sha256")
	tables.special = set.Table("special_cases", "seq", "case_name", "observed", "value")
	tables.skipped = set.Table("skipped", "part", "what")
	tables.infoTiming = set.Table("info_first_statement", "tables", "elapsed_us")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

// plainConnection opens a raw modernc.org/sqlite connection without applying
// any PRAGMA settings, because running any PRAGMA would itself trigger lazy
// schema loading (sqlite3ReadSchema, src/prepare.c:470-484).
func plainConnection(ctx context.Context, path string) (*sql.DB, *sql.Conn) {
	uri, err := sqlitex.FileURI(path, nil)
	if err != nil {
		fatal("uri", err)
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		fatal("sql.Open", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("open "+path, err)
	}
	return db, conn
}

func closePlain(db *sql.DB, conn *sql.Conn) {
	if err := conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := db.Close(); err != nil {
		fatal("close db", err)
	}
}

func schemaPath(dir string, tables int) string {
	return filepath.Join(dir, fmt.Sprintf("schema-%d.db", tables))
}

func buildSchema(ctx context.Context, path string, tables int) {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for i := 1; i <= tables; i++ {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("CREATE TABLE t%04d(id INTEGER PRIMARY KEY, name TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT)", i)); err != nil {
			fatal("create", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	status, err := connmgr.CheckAtRest(path)
	if err != nil || !status.AtRest {
		fatal("at rest "+path, fmt.Errorf("%v %v", status, err))
	}
}

// childOpen runs inside strace and writes explicit MARK lines to stdout so the
// parent can partition syscalls cleanly into open, first-statement, and close
// phases. LockOSThread keeps the goroutine on one OS thread for strace order.
func childOpen(path string) {
	runtime.LockOSThread()
	ctx := context.Background()
	fmt.Println(markPrefix + "open")
	db, conn := plainConnection(ctx, path)
	fmt.Println(markPrefix + "first statement")
	var count int
	if err := conn.QueryRowContext(ctx, firstStatement).Scan(&count); err != nil {
		fatal("first statement", err)
	}
	fmt.Println(markPrefix + "close")
	closePlain(db, conn)
	fmt.Println(markPrefix + "done")
}

type phaseIO struct {
	calls      []trace.Syscall
	events     []string
	opens      []string
	readsByDB  []trace.Syscall
	otherReads map[string]int
}

func splitPhases(log *trace.StraceLog, dir string) (map[string]*phaseIO, error) {
	result := map[string]*phaseIO{}
	current := ""
	under := trace.UnderDir(dir)
	for _, call := range log.Calls {
		if call.Name == "write" && call.FD == 1 && strings.HasPrefix(call.Data, markPrefix) {
			current = strings.TrimSuffix(strings.TrimPrefix(call.Data, markPrefix), "\n")
			if current != "done" {
				result[current] = &phaseIO{otherReads: map[string]int{}}
			}
			continue
		}
		phase, ok := result[current]
		if !ok || current == "done" || !under(call) || call.Result < 0 {
			continue
		}
		base := filepath.Base(call.Path)
		switch call.Name {
		case "open", "openat":
			phase.calls = append(phase.calls, call)
			phase.opens = append(phase.opens, base)
			phase.events = append(phase.events, "open "+base)
		case "pread64", "read":
			phase.calls = append(phase.calls, call)
			if strings.HasSuffix(base, ".db") {
				phase.readsByDB = append(phase.readsByDB, call)
				if call.Offset%pageSize == 0 && call.Length == pageSize {
					phase.events = append(phase.events, fmt.Sprintf("read page %d", call.Offset/pageSize+1))
				} else {
					phase.events = append(phase.events, fmt.Sprintf("read %d bytes at %d", call.Length, call.Offset))
				}
			} else {
				phase.otherReads[base]++
			}
		}
	}
	for _, name := range phases {
		if result[name] == nil {
			return nil, fmt.Errorf("trace has no %q marker", name)
		}
	}
	return result, nil
}

// writeEvents writes one io_events row per open or read of a file under the
// work directory, in trace order.
func writeEvents(tableCount int, byPhase map[string]*phaseIO) {
	for _, name := range phases {
		for i, call := range byPhase[name].calls {
			if call.Name == "open" || call.Name == "openat" {
				tables.events.Row(tableCount, name, i+1, "open", filepath.Base(call.Path), nil, nil, nil)
			} else {
				tables.events.Row(tableCount, name, i+1, "read", filepath.Base(call.Path), call.Offset, call.Length, call.Result)
			}
		}
	}
}

// pagesOf converts aligned, whole 4,096-byte reads into 1-based page numbers
// and reports whether every read in the slice was a complete aligned page read.
func pagesOf(reads []trace.Syscall) ([]int, bool) {
	var pages []int
	whole := true
	for _, r := range reads {
		if r.Offset%pageSize != 0 || r.Length != pageSize || r.Result != pageSize {
			whole = false
			continue
		}
		pages = append(pages, int(r.Offset/pageSize)+1)
	}
	return pages, whole
}

// schemaTreePages returns the pages of the sqlite_schema b-tree (from dbstat)
// followed by t0001's root page, and writes them to schema_pages.
func schemaTreePages(ctx context.Context, path string, tableCount int) []int {
	db, conn := plainConnection(ctx, path)
	defer closePlain(db, conn)
	rows, err := conn.QueryContext(ctx, "SELECT pageno FROM dbstat WHERE name = 'sqlite_schema' ORDER BY pageno")
	if err != nil {
		fatal("dbstat", err)
	}
	defer rows.Close()
	var pages []int
	for rows.Next() {
		var page int
		if err := rows.Scan(&page); err != nil {
			fatal("dbstat scan", err)
		}
		pages = append(pages, page)
		tables.schemaPages.Row(tableCount, page, "sqlite_schema")
	}
	if err := rows.Err(); err != nil {
		fatal("dbstat rows", err)
	}
	var root int
	if err := conn.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_schema WHERE name = 't0001'").Scan(&root); err != nil {
		fatal("rootpage", err)
	}
	tables.schemaPages.Row(tableCount, root, "t0001 root")
	return append(pages, root)
}

func partA(ctx context.Context, work string) {
	part = "A"
	if !trace.Supported {
		trace.PrintLinuxOnly("strace read counts per phase", packagePath)
		tables.skipped.Row("A", "strace read counts per phase (needs Linux)")
	}
	for _, tableCount := range tableCounts {
		path := schemaPath(work, tableCount)
		buildSchema(ctx, path, tableCount)
		info, err := os.Stat(path)
		if err != nil {
			fatal("stat", err)
		}
		atOpen, afterFirst := measureSchemaMemory(ctx, path, tableCount)
		tables.schemas.Row(tableCount, info.Size()/pageSize, atOpen, afterFirst)
		wantPages := schemaTreePages(ctx, path, tableCount)
		if !trace.Supported {
			continue
		}
		command, err := trace.SelfCommand("child-open", path)
		if err != nil {
			fatal("self", err)
		}
		result, err := trace.Run(ctx, trace.Options{Command: command, Syscalls: []string{"open", "openat", "pread64", "read", "write", "close"}})
		if err != nil {
			fatal("strace", err)
		}
		if result.ExitCode != 0 {
			fatal("child", fmt.Errorf("exit %d: %s%s", result.ExitCode, result.Stdout, result.Stderr))
		}
		byPhase, err := splitPhases(result.Log, work)
		if err != nil {
			fatal("phases", err)
		}
		writeEvents(tableCount, byPhase)
		// sqlite3BtreeOpen (src/btree.c:2532-2825) reads only the 100-byte file
		// header via sqlite3PagerReadFileheader; no schema or WAL files are
		// touched at open.
		open := byPhase["open"]
		check(len(open.opens) == 1 && open.opens[0] == filepath.Base(path) && len(open.readsByDB) == 1 && open.readsByDB[0].Offset == 0 && open.readsByDB[0].Length == 100 && open.readsByDB[0].Result == 100 && len(open.otherReads) == 0,
			fmt.Sprintf("%d table(s): open opens only the database file and reads only its 100-byte header", tableCount))
		first := byPhase["first statement"]
		firstPages, firstWhole := pagesOf(first.readsByDB)
		check(firstWhole && slices.Equal(sortedUnique(firstPages), sortedUnique(wantPages)),
			fmt.Sprintf("%d table(s): the first statement reads exactly the sqlite_schema b-tree pages (%d, from dbstat) plus t0001's root page", tableCount, len(wantPages)-1))
		// In WAL mode, lockBtree (src/btree.c:3282-3459) reads page 1, sees
		// header byte 19 == 2, opens -wal and -shm, and drops page 1 so
		// btreeBeginTrans (src/btree.c:3598-3804) re-reads page 1 through WAL.
		walOpen := slices.Index(first.events, "open "+filepath.Base(path)+"-wal")
		pageOneReads := 0
		for _, event := range first.events {
			if event == "read page 1" {
				pageOneReads++
			}
		}
		check(pageOneReads == 2 && len(first.events) > 0 && first.events[0] == "read page 1" && walOpen > 0 && slices.Index(first.events[walOpen:], "read page 1") > 0,
			fmt.Sprintf("%d table(s): page 1 is read twice: once before the -wal is opened (header byte 19 = 2 sends lockBtree to open the WAL) and once after", tableCount))
		check(len(first.otherReads) == 0, fmt.Sprintf("%d table(s): nothing is read from the -wal or -shm (the -wal is empty; the -shm is mapped, not read)", tableCount))
		check(slices.Contains(first.opens, filepath.Base(path)+"-wal") && slices.Contains(first.opens, filepath.Base(path)+"-shm"),
			fmt.Sprintf("%d table(s): the -wal and -shm files are opened at the first statement, not at open", tableCount))
	}
}

func sortedUnique(pages []int) []int {
	out := slices.Clone(pages)
	slices.Sort(out)
	return slices.Compact(out)
}

var schemaUsed = map[int][2]int{}

func measureSchemaMemory(ctx context.Context, path string, tableCount int) (int, int) {
	db, conn := plainConnection(ctx, path)
	defer closePlain(db, conn)
	read := func() int {
		status, err := sqlitex.ReadDBStatus(ctx, conn, sqlitex.DBStatusSchemaUsed, false)
		if err != nil {
			fatal("SCHEMA_USED", err)
		}
		return status.Current
	}
	atOpen := read()
	started := time.Now()
	var count int
	if err := conn.QueryRowContext(ctx, firstStatement).Scan(&count); err != nil {
		fatal("first statement", err)
	}
	elapsed := time.Since(started)
	afterFirst := read()
	schemaUsed[tableCount] = [2]int{atOpen, afterFirst}
	tables.infoTiming.Row(tableCount, elapsed.Microseconds())
	return atOpen, afterFirst
}

func checkSchemaMemory() {
	growing := true
	for i, tableCount := range tableCounts {
		if schemaUsed[tableCount][0] != 0 {
			growing = false
		}
		if i > 0 && schemaUsed[tableCount][1] <= schemaUsed[tableCounts[i-1]][1] {
			growing = false
		}
	}
	check(growing, "SCHEMA_USED is 0 after open for every schema, and after the first statement it grows with the table count (1 < 100 < 1000)")
}

func readPragma(ctx context.Context, conn *sql.Conn, pragma string) string {
	var value any
	if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil {
		fatal("PRAGMA "+pragma, err)
	}
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return strings.ToLower(fmt.Sprint(value))
}

func readPragmas(ctx context.Context, path string, pragmas []string) map[string]string {
	db, conn := plainConnection(ctx, path)
	defer closePlain(db, conn)
	values := map[string]string{}
	for _, pragma := range pragmas {
		values[pragma] = readPragma(ctx, conn, pragma)
	}
	return values
}

func copyFile(from, to string) {
	in, err := os.Open(from)
	if err != nil {
		fatal("copy", err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		fatal("copy", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		fatal("copy", err)
	}
	if err := out.Close(); err != nil {
		fatal("copy", err)
	}
}

type settingCase struct {
	name   string
	pragma string
	set    string
}

func partB(ctx context.Context, work string) {
	part = "B"
	dir := filepath.Join(work, "B")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	original := filepath.Join(dir, "original.db")
	options := sqlitex.Options{
		File:             sqlitex.FileSettings{PageSize: 8192, AutoVacuum: sqlitex.AutoVacuumIncremental, Encoding: "UTF-16le", ApplicationID: sqlitex.CourseApplicationID, UserVersion: 7, JournalMode: sqlitex.JournalWAL},
		Conn:             sqlitex.ConnSettings{Synchronous: sqlitex.SyncNormal, ForeignKeys: true, SecureDelete: true, CacheSize: -8000, WALAutocheckpoint: 200, BusyTimeout: 3 * time.Second},
		SingleConnection: true,
	}
	cases := []settingCase{
		{"page_size", "page_size", "8192"},
		{"journal_mode=wal", "journal_mode", "wal"},
		{"auto_vacuum", "auto_vacuum", "2"},
		{"encoding", "encoding", "utf-16le"},
		{"user_version", "user_version", "7"},
		{"application_id", "application_id", strconv.Itoa(int(sqlitex.CourseApplicationID))},
		{"synchronous", "synchronous", "1"},
		{"foreign_keys", "foreign_keys", "1"},
		{"cache_size", "cache_size", "-8000"},
		{"busy_timeout", "busy_timeout", "3000"},
		{"wal_autocheckpoint", "wal_autocheckpoint", "200"},
		{"secure_delete", "secure_delete", "1"},
	}
	db, err := sqlitex.Open(ctx, original, options)
	if err != nil {
		fatal("open original", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1, 'one')"); err != nil {
		fatal("create", err)
	}
	onOriginal := map[string]string{}
	for _, c := range cases {
		onOriginal[c.name] = readPragma(ctx, conn, c.pragma)
	}
	if err := conn.Close(); err != nil {
		fatal("close", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	status, err := connmgr.CheckAtRest(original)
	if err != nil {
		fatal("at rest", err)
	}
	tables.files.Row(1, "original.db", status.AtRest, status.Size, status.SHA256)
	check(status.AtRest, "the original is at rest after its last connection closed")
	copyPath := filepath.Join(dir, "copy.db")
	copyFile(original, copyPath)
	copied, err := connmgr.CheckAtRest(copyPath)
	if err != nil {
		fatal("hash copy", err)
	}
	tables.files.Row(2, "copy.db", copied.AtRest, copied.Size, copied.SHA256)
	check(copied.AtRest && copied.SHA256 == status.SHA256, "a plain copy of the closed file has the same sha256 (rung R5: capture = the file at rest, rebuild = copy)")
	vacuumPath := filepath.Join(dir, "vacuum-into.db")
	vdb, vconn := plainConnection(ctx, original)
	if _, err := vconn.ExecContext(ctx, "VACUUM INTO ?", vacuumPath); err != nil {
		fatal("vacuum into", err)
	}
	closePlain(vdb, vconn)
	freshPath := filepath.Join(dir, "fresh.db")
	pragmas := make([]string, len(cases))
	for i, c := range cases {
		pragmas[i] = c.pragma
	}
	defaults := readPragmas(ctx, freshPath, pragmas)
	onCopy := readPragmas(ctx, copyPath, pragmas)
	onVacuum := readPragmas(ctx, vacuumPath, pragmas)
	allAsDeclared := true
	inFileKept := true
	for i, c := range cases {
		home := sqlitex.SettingPerConnection
		if onCopy[c.pragma] == c.set {
			home = sqlitex.SettingInFile
		}
		placement, ok := sqlitex.SettingPlacementFor(c.name)
		if !ok {
			fatal("placement", fmt.Errorf("no declared placement for %s", c.name))
		}
		if placement.Home != home || onOriginal[c.name] != c.set || defaults[c.pragma] == c.set {
			allAsDeclared = false
		}
		if home == sqlitex.SettingInFile && c.name != "journal_mode=wal" && onVacuum[c.pragma] != onOriginal[c.name] {
			inFileKept = false
		}
		tables.settings.Row(i+1, c.name, c.pragma, c.set, onOriginal[c.name], defaults[c.pragma], onCopy[c.pragma], onVacuum[c.pragma], string(home))
	}
	for i, p := range sqlitex.SettingPlacements() {
		var headerBytes any
		if p.HeaderBytes != "" {
			headerBytes = p.HeaderBytes
		}
		tables.placements.Row(i+1, p.Name, p.Pragma, string(p.Home), headerBytes)
	}
	check(allAsDeclared, "every value set differs from the fresh default, and every setting that survived the copy is exactly a setting sqlitex.SettingPlacements declares as living in the file")
	vacuumHeader, err := os.ReadFile(vacuumPath)
	if err != nil || len(vacuumHeader) < 100 {
		fatal("read VACUUM INTO header", fmt.Errorf("%d bytes, %v", len(vacuumHeader), err))
	}
	tables.special.Row(1, "VACUUM INTO output", "header bytes 18-19", fmt.Sprintf("%d %d", vacuumHeader[18], vacuumHeader[19]))
	check(inFileKept && onVacuum["journal_mode"] == sqlitex.JournalDelete && vacuumHeader[18] == 1 && vacuumHeader[19] == 1, "VACUUM INTO keeps every file setting except journal_mode: its output is a rollback-mode file (header bytes 18-19 = 1)")

	persistPath := filepath.Join(dir, "persist.db")
	pdb, pconn := plainConnection(ctx, persistPath)
	if mode := readPragma(ctx, pconn, "journal_mode=persist"); mode != sqlitex.JournalPersist {
		fatal("persist", fmt.Errorf("got %s", mode))
	}
	if _, err := pconn.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		fatal("create", err)
	}
	closePlain(pdb, pconn)
	persistReopened := readPragmas(ctx, persistPath, []string{"journal_mode"})["journal_mode"]
	tables.special.Row(2, "journal_mode=persist set on a new file, reopened", "journal_mode", persistReopened)
	cachePath := filepath.Join(dir, "default-cache-size.db")
	cdb, cconn := plainConnection(ctx, cachePath)
	if _, err := cconn.ExecContext(ctx, "PRAGMA default_cache_size=777; CREATE TABLE t(x)"); err != nil {
		fatal("default_cache_size", err)
	}
	closePlain(cdb, cconn)
	cacheCopy := filepath.Join(dir, "default-cache-size-copy.db")
	copyFile(cachePath, cacheCopy)
	reopened := readPragmas(ctx, cacheCopy, []string{"default_cache_size", "cache_size"})
	header, err := os.ReadFile(cacheCopy)
	if err != nil || len(header) < 100 {
		fatal("read header", fmt.Errorf("%d bytes, %v", len(header), err))
	}
	// sqlite3InitOne (src/prepare.c:323-333) reads header bytes 48-51
	// (BTREE_DEFAULT_CACHE_SIZE) when a connection has not set cache_size yet,
	// so the deprecated PRAGMA default_cache_size persists in the file header.
	stored := int32(uint32(header[48])<<24 | uint32(header[49])<<16 | uint32(header[50])<<8 | uint32(header[51]))
	const cacheCase = "PRAGMA default_cache_size=777, then a copy opened"
	tables.special.Row(3, cacheCase, "default_cache_size", reopened["default_cache_size"])
	tables.special.Row(4, cacheCase, "cache_size", reopened["cache_size"])
	tables.special.Row(5, cacheCase, "header bytes 48-51", stored)
	check(persistReopened == sqlitex.JournalDelete, "a rollback journal mode (persist) does not survive a reopen: it is per connection; only wal is stored in the file")
	check(reopened["cache_size"] == "777" && stored == 777, "the deprecated PRAGMA default_cache_size is stored in header bytes 48-51 and becomes the starting cache_size of every new connection to the file")
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child-open" {
		childOpen(os.Args[2])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("8.1")
	set = results.MustOpen(work)
	openTables()
	partA(ctx, work)
	checkSchemaMemory()
	partB(ctx, work)
	set.Finish("OPEN COST AND WHERE SETTINGS LIVE")
}

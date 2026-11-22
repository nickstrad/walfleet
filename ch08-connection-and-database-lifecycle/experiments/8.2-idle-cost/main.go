// Experiment 8.2 measures the per-connection file descriptor and memory costs
// of idle SQLite connections: descriptor transitions per state in DELETE and
// WAL mode (Part A), SQLITE_DBSTATUS_* counters versus process RSS across N
// idle WAL connections (Part B), PRAGMA shrink_memory across the shared page
// cache group (Part C), and descriptor sharing and parking with two connections
// to one WAL file in the same process (Part D).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 8.2
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/procstat"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

// uncountedBudget is the per-connection upper margin for memory that
// CACHE_USED, SCHEMA_USED, and STMT_USED do not report (such as the 48 KiB
// lookaside buffer at src/global.c:218-224, driver state, and allocator
// overhead) for this experiment's 3-table recipe workload.
const uncountedBudget = 256 * 1024

const (
	packagePath = "ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost"
	readQuery   = "SELECT count(*), sum(length(body)) FROM documents"
	stmtQuery   = "SELECT name, balance FROM accounts WHERE id = ?"
)

var (
	connections = flag.Int("n", 200, "idle WAL connections opened in part B, one database file each")
	recipeStep  = flag.Int("step", 3, "recipe step every database file in part B is built to")
)

// Every measurement goes to a CSV table in <work dir>/results/. part labels
// the rows and claims written while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		setup, fdStates, fleet, fleetTargets, connections, skipped, infoCache, infoMemory, infoLimits *results.Table
	}
	setupSeq int
)

func openTables() {
	tables.setup = set.Table("setup", "seq", "name", "value")
	tables.fdStates = set.Table("fd_states", "part", "journal_mode", "seq", "state", "descriptors", "expected", "targets", "side_files")
	tables.fleet = set.Table("fleet", "seq", "stage", "descriptors", "files_not_at_rest")
	tables.fleetTargets = set.Table("fleet_targets", "target")
	tables.connections = set.Table("connections", "conn", "schema_used", "stmt_used", "schema_used_after_shrink", "stmt_used_after_shrink")
	tables.skipped = set.Table("skipped", "part", "what")
	tables.infoCache = set.Table("info_cache", "conn", "cache_used", "after_shrink_on_conn_0", "after_shrink_on_all")
	tables.infoMemory = set.Table("info_memory", "seq", "stage", "rss_bytes")
	tables.infoLimits = set.Table("info_limits", "soft", "hard")
}

func setup(name string, value any) {
	setupSeq++
	tables.setup.Row(setupSeq, name, value)
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func descriptorCount() int {
	n, err := procstat.DescriptorCount()
	if err != nil {
		fatal("count descriptors", err)
	}
	return n
}

// openNames returns the descriptor targets under dir, sorted and
// space-separated, or nil where descriptors name no files.
func openNames(dir string) any {
	if !procstat.TargetsSupported {
		return nil
	}
	under, err := procstat.DescriptorsUnder(dir)
	if err != nil {
		fatal("list descriptors", err)
	}
	targets := procstat.RelativeTargets(dir, under)
	slices.Sort(targets)
	return strings.Join(targets, " ")
}

func sideFiles(path string) string {
	var present []string
	for _, suffix := range recipe.SideFileSuffixes {
		if _, err := os.Stat(path + suffix); err == nil {
			present = append(present, filepath.Base(path+suffix))
		}
	}
	return strings.Join(present, " ")
}

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
		fatal("open connection", err)
	}
	return db, conn
}

func exec(ctx context.Context, conn *sql.Conn, statements ...string) {
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
}

func createTable(ctx context.Context, path, journalMode string) {
	options := sqlitex.DefaultOptions()
	options.File.JournalMode = journalMode
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("create "+path, err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		fatal("create table", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func partA(ctx context.Context, work string) {
	part = "A"
	if !procstat.TargetsSupported {
		what := "descriptor file names (from " + procstat.DescriptorDir + ", which names no files on " + runtime.GOOS + ")"
		trace.PrintLinuxOnly(what, packagePath)
		tables.skipped.Row("A", what)
	}
	want := map[string][]int{
		sqlitex.JournalDelete: {1, 1, 2, 1, 0},
		sqlitex.JournalWAL:    {1, 3, 3, 3, 0},
	}
	for _, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		dir := filepath.Join(work, "A-"+mode)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir", err)
		}
		path := filepath.Join(dir, "t.db")
		createTable(ctx, path, mode)
		base := descriptorCount()
		var got []int
		show := func(label string) {
			delta := descriptorCount() - base
			got = append(got, delta)
			tables.fdStates.Row("A", mode, len(got), label, delta, want[mode][len(got)-1], openNames(dir), sideFiles(path))
		}
		db, conn := plainConnection(ctx, path)
		show("opened, no statement yet")
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil {
			fatal("read", err)
		}
		show("idle after a read")
		exec(ctx, conn, "BEGIN IMMEDIATE", "INSERT INTO t(v) VALUES('x')")
		show("inside a write transaction")
		exec(ctx, conn, "COMMIT")
		show("idle after the commit")
		if err := conn.Close(); err != nil {
			fatal("conn close", err)
		}
		if err := db.Close(); err != nil {
			fatal("db close", err)
		}
		show("after close")
		check(fmt.Sprint(got) == fmt.Sprint(want[mode]), fmt.Sprintf("journal_mode=%s: descriptors per state are %v", mode, want[mode]))
	}
}

type idleConnection struct {
	db   *sqlitex.DB
	conn *sql.Conn
	stmt *sql.Stmt
}

type usage struct {
	cache, schema, stmt int64
}

func (u usage) total() int64 { return u.cache + u.schema + u.stmt }

func readUsage(ctx context.Context, conn *sql.Conn) usage {
	read := func(op sqlitex.DBStatusOp, name string) int64 {
		status, err := sqlitex.ReadDBStatus(ctx, conn, op, false)
		if err != nil {
			fatal(name, err)
		}
		return int64(status.Current)
	}
	return usage{
		cache:  read(sqlitex.DBStatusCacheUsed, "CACHE_USED"),
		schema: read(sqlitex.DBStatusSchemaUsed, "SCHEMA_USED"),
		stmt:   read(sqlitex.DBStatusStmtUsed, "STMT_USED"),
	}
}

func readAll(ctx context.Context, idle []idleConnection) []usage {
	all := make([]usage, len(idle))
	for i, c := range idle {
		all[i] = readUsage(ctx, c.conn)
	}
	return all
}

func sum(all []usage) usage {
	var s usage
	for _, u := range all {
		s.cache += u.cache
		s.schema += u.schema
		s.stmt += u.stmt
	}
	return s
}

func minMax(all []usage, field func(usage) int64) (int64, int64) {
	lo, hi := field(all[0]), field(all[0])
	for _, u := range all[1:] {
		lo, hi = min(lo, field(u)), max(hi, field(u))
	}
	return lo, hi
}

func settledMemory() procstat.Memory {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := procstat.ReadMemory()
	if err != nil {
		fatal("read memory", err)
	}
	return m
}

func partB(ctx context.Context, work string) {
	part = "B"
	n := *connections
	dir := filepath.Join(work, "B")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	source := filepath.Join(dir, "source.db")
	r := recipe.Default()
	if err := r.Build(ctx, source, *recipeStep); err != nil {
		fatal("build recipe", err)
	}
	image, err := os.ReadFile(source)
	if err != nil {
		fatal("read source", err)
	}
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("db-%04d.db", i))
		if err := os.WriteFile(paths[i], image, 0o644); err != nil {
			fatal("copy", err)
		}
	}
	setup("connections", n)
	setup("every file", fmt.Sprintf("a copy of recipe step %d", *recipeStep))
	setup("file bytes", len(image))
	setup("file pages", len(image)/4096)
	for _, setting := range strings.Fields(sqlitex.DefaultConnSettings().String()) {
		name, value, _ := strings.Cut(setting, "=")
		setup("sqlitex default "+name, value)
	}
	setup("read per connection", readQuery)
	setup("prepared statement kept open", stmtQuery)
	baseFDs := descriptorCount()
	baseMemory := settledMemory()
	idle := make([]idleConnection, n)
	for i, path := range paths {
		options := sqlitex.DefaultOptions()
		options.SingleConnection = true
		db, err := sqlitex.Open(ctx, path, options)
		if err != nil {
			fatal("open", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		var rows, bytes int64
		if err := conn.QueryRowContext(ctx, readQuery).Scan(&rows, &bytes); err != nil {
			fatal("scan", err)
		}
		stmt, err := conn.PrepareContext(ctx, stmtQuery)
		if err != nil {
			fatal("prepare", err)
		}
		idle[i] = idleConnection{db: db, conn: conn, stmt: stmt}
	}
	fds := descriptorCount() - baseFDs
	tables.fleet.Row(1, fmt.Sprintf("all %d open and idle", n), fds, nil)
	check(fds == 3*n, fmt.Sprintf("%d idle WAL connections hold exactly 3 x %d = %d descriptors", n, n, 3*n))
	if procstat.TargetsSupported {
		under, err := procstat.DescriptorsUnder(dir)
		if err != nil {
			fatal("list", err)
		}
		targets := procstat.RelativeTargets(dir, under)
		slices.Sort(targets)
		for _, target := range targets {
			tables.fleetTargets.Row(target)
		}
	} else {
		tables.skipped.Row("B", "descriptor file names")
	}
	before := readAll(ctx, idle)
	total := sum(before)
	afterOpen := settledMemory()
	growth := afterOpen.RSS - baseMemory.RSS
	schemaLo, schemaHi := minMax(before, func(u usage) int64 { return u.schema })
	stmtLo, stmtHi := minMax(before, func(u usage) int64 { return u.stmt })
	cacheLo, _ := minMax(before, func(u usage) int64 { return u.cache })
	tables.infoMemory.Row(1, "before opening", baseMemory.RSS)
	tables.infoMemory.Row(2, "all open and idle", afterOpen.RSS)
	check(schemaLo == schemaHi && stmtLo == stmtHi, "every connection reports the same SCHEMA_USED and the same STMT_USED (same schema, same statement)")
	uncounted := growth - total.total()
	check(growth >= total.total(), "process memory grew by at least the sum of CACHE_USED + SCHEMA_USED + STMT_USED")
	check(uncounted <= int64(n)*uncountedBudget, fmt.Sprintf("process memory grew by at most that sum + %d KiB per connection (the stated margin for memory the counters do not report)", uncountedBudget/1024))

	// With SQLITE_ENABLE_MEMORY_MANAGEMENT enabled in this driver, all
	// connections share one pcache1 group; PRAGMA shrink_memory
	// (src/pragma.c:2439-2442 -> sqlite3_db_release_memory src/main.c:897-915
	// -> pcache1Shrink src/pcache1.c:839-851) recycles unpinned pages across
	// every connection in that group at once.
	part = "C"
	if _, err := idle[0].conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
		fatal("shrink", err)
	}
	afterOne := readAll(ctx, idle)
	droppedEverywhere := true
	for i := range afterOne {
		if afterOne[i].cache >= before[i].cache {
			droppedEverywhere = false
		}
	}
	check(droppedEverywhere, fmt.Sprintf("shrink_memory on connection 0 alone lowered CACHE_USED on all %d connections (this driver keeps every page cache in one shared group)", n))
	for _, c := range idle[1:] {
		if _, err := c.conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
			fatal("shrink", err)
		}
	}
	after := readAll(ctx, idle)
	shrunk := sum(after)
	afterShrink := settledMemory()
	tables.infoMemory.Row(3, "after shrink_memory on every connection", afterShrink.RSS)
	for i := range idle {
		tables.connections.Row(i, before[i].schema, before[i].stmt, after[i].schema, after[i].stmt)
		tables.infoCache.Row(i, before[i].cache, afterOne[i].cache, after[i].cache)
	}
	check(shrunk.schema == total.schema && shrunk.stmt == total.stmt, "shrink_memory leaves SCHEMA_USED and STMT_USED unchanged on every connection")
	check(sum(afterOne).cache == shrunk.cache, "running shrink_memory on the other connections afterwards released nothing more")
	_, floorHi := minMax(after, func(u usage) int64 { return u.cache })
	check(floorHi*8 < cacheLo, "after shrink every connection's CACHE_USED is under 1/8 of what it was")

	for _, c := range idle {
		if err := c.stmt.Close(); err != nil {
			fatal("stmt close", err)
		}
		if err := c.conn.Close(); err != nil {
			fatal("conn close", err)
		}
		if err := c.db.Close(); err != nil {
			fatal("db close", err)
		}
	}
	left := descriptorCount() - baseFDs
	notAtRest := 0
	for _, path := range paths {
		if err := recipe.CheckAtRest(path); err != nil {
			notAtRest++
		}
	}
	tables.fleet.Row(2, fmt.Sprintf("all %d closed", n), left, notAtRest)
	check(left == 0 && notAtRest == 0, fmt.Sprintf("after closing all %d: 0 descriptors left and every file at rest (no -wal, -shm, -journal)", n))
}

// partD opens two connections to one WAL database in the same process to show
// both shared -shm descriptors (one unixShmNode per inode) and descriptor
// parking in unixClose -> setPendingFd (src/os_unix.c:2341-2371, 2101-2109)
// until closePendingFds (src/os_unix.c:1471-1482) runs when locks reach zero.
func partD(ctx context.Context, work string) {
	part = "D"
	want := []int{3, 5, 4, 0}
	dir := filepath.Join(work, "D")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(dir, "t.db")
	createTable(ctx, path, sqlitex.JournalWAL)
	base := descriptorCount()
	var got []int
	show := func(label string) {
		delta := descriptorCount() - base
		got = append(got, delta)
		tables.fdStates.Row("D", sqlitex.JournalWAL, len(got), label, delta, want[len(got)-1], openNames(dir), sideFiles(path))
	}
	read := func(conn *sql.Conn) {
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil {
			fatal("read", err)
		}
	}
	db1, conn1 := plainConnection(ctx, path)
	read(conn1)
	show("connection 1 open, idle after a read")
	db2, conn2 := plainConnection(ctx, path)
	read(conn2)
	show("connection 2 open too, idle after a read")
	if err := conn2.Close(); err != nil {
		fatal("close", err)
	}
	if err := db2.Close(); err != nil {
		fatal("close", err)
	}
	show("connection 2 closed")
	if err := conn1.Close(); err != nil {
		fatal("close", err)
	}
	if err := db1.Close(); err != nil {
		fatal("close", err)
	}
	show("connection 1 closed")
	check(fmt.Sprint(got) == fmt.Sprint(want), "two connections share one -shm descriptor (5, not 6); the closed connection's database descriptor stays open (4, not 3) until the last connection to the file closes")
}

func main() {
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	if *connections < 20 || *recipeStep < 1 || *recipeStep > recipe.DefaultSteps {
		fatal("flags", fmt.Errorf("need -n >= 20 and 1 <= -step <= %d", recipe.DefaultSteps))
	}
	work := sqlitex.MustWorkDir("8.2")
	set = results.MustOpen(work)
	openTables()
	soft, hard, err := procstat.FileLimit()
	if err != nil {
		fatal("file limit", err)
	}
	setup("descriptors counted from", procstat.DescriptorDir)
	setup("process memory from", procstat.MemorySource)
	tables.infoLimits.Row(soft, hard)
	partA(ctx, work)
	partB(ctx, work)
	partD(ctx, work)
	set.Finish("IDLE COST")
}

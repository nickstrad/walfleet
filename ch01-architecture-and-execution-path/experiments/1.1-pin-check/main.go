// Experiment 1.1 verifies that both SQLite builds used by the course—the
// modernc.org/sqlite Go driver and the pinned sqlite3 C CLI—run the pinned
// SQLite 3.53.4 check-in and share the required compile-time options.
//
// It prints only its verdict; the measurements go to CSV tables that
// report.sql presents:
//
//	go run ./cmd/course run 1.1
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/sqlite"
)

// requiredOptions lists the compile options later labs depend on: sqlite_dbpage
// and dbstat virtual tables for page inspection, and upstream's FULL sync
// default in WAL mode (DEFAULT_WAL_SYNCHRONOUS=2).
var requiredOptions = []string{
	"ENABLE_DBPAGE_VTAB",
	"ENABLE_DBSTAT_VTAB",
	"DEFAULT_WAL_SYNCHRONOUS=2",
}

// fileFormatAndIOOptions lists compile options that govern on-disk format,
// cache sizing, mmap limits, or sync defaults; both builds must agree on all
// of them.
var fileFormatAndIOOptions = []string{
	"DEFAULT_AUTOVACUUM",
	"DEFAULT_CACHE_SIZE",
	"DEFAULT_FILE_FORMAT",
	"DEFAULT_JOURNAL_SIZE_LIMIT",
	"DEFAULT_MMAP_SIZE",
	"DEFAULT_PAGE_SIZE",
	"DEFAULT_PCACHE_INITSZ",
	"DEFAULT_SECTOR_SIZE",
	"DEFAULT_SYNCHRONOUS",
	"DEFAULT_WAL_AUTOCHECKPOINT",
	"DEFAULT_WAL_SYNCHRONOUS",
	"DIRECT_OVERFLOW_READ",
	"MAX_MMAP_SIZE",
	"MAX_PAGE_SIZE",
	"TEMP_STORE",
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func optionName(option string) string {
	name, _, _ := strings.Cut(option, "=")
	return name
}

func findOption(options []string, name string) (string, bool) {
	for _, option := range options {
		if optionName(option) == name {
			return option, true
		}
	}
	return "", false
}

func driverOptions(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var options []string
	for rows.Next() {
		var option string
		if err := rows.Scan(&option); err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, rows.Err()
}

// moduleVersion reads the linked Go module version from the binary's build
// info rather than go.mod so a replace directive or upgrade is caught at run
// time.
func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(no build info)"
	}
	for _, dep := range info.Deps {
		if dep.Path == path {
			if dep.Replace != nil {
				return dep.Version + " replaced by " + dep.Replace.Path + " " + dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "(not linked)"
}

func main() {
	ctx := context.Background()
	pin := sqlite.Current()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		fatal("open :memory:", err)
	}
	// Pin :memory: to one connection so every query shares one database.
	db.SetMaxOpenConns(1)
	defer db.Close()
	// Every course experiment prints sqlite_source_id() first and exits
	// non-zero if the linked build does not match sqlite/PIN.
	sqlitex.MustPinCheck(ctx, db)

	set := results.MustOpen(sqlitex.MustWorkDir("1.1"))
	pinTable := set.Table("pin", "field", "value")
	pinTable.Row("release", pin.Release)
	pinTable.Row("tag", pin.Tag)
	pinTable.Row("git", pin.GitCommit)
	pinTable.Row("fossil", pin.FossilID)
	pinTable.Row("source_id", pin.SourceID)
	versions := set.Table("versions", "seq", "item", "found", "pinned")
	versionSeq := 0
	version := func(item, found, pinned string) {
		versionSeq++
		versions.Row(versionSeq, item, found, pinned)
	}

	driverVersion := moduleVersion(pin.DriverModule)
	libcVersion := moduleVersion(pin.LibcModule)
	version("driver module "+pin.DriverModule, driverVersion, pin.DriverVersion)
	version("libc module "+pin.LibcModule, libcVersion, pin.LibcVersion)
	set.Check("A", driverVersion == pin.DriverVersion, fmt.Sprintf("the linked driver is %s %s", pin.DriverModule, pin.DriverVersion))
	set.Check("A", libcVersion == pin.LibcVersion, fmt.Sprintf("the linked libc is %s %s", pin.LibcModule, pin.LibcVersion))

	var driverRelease, driverSourceID string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version(), sqlite_source_id()").Scan(&driverRelease, &driverSourceID); err != nil {
		fatal("sqlite_version", err)
	}
	version("driver sqlite_version()", driverRelease, pin.Release)
	version("driver sqlite_source_id()", driverSourceID, pin.SourceID)
	set.Check("A", driverRelease == pin.Release, "the driver reports release "+pin.Release)

	options, err := driverOptions(ctx, db)
	if err != nil {
		fatal("compile_options", err)
	}
	driverTable := set.Table("driver_options", "seq", "option")
	for i, option := range options {
		driverTable.Row(i+1, option)
	}

	// Smoke-test both page-inspection virtual tables on a 2-page database
	// (page 1 for sqlite_schema, page 2 for table t).
	var dbpagePages, dbstatRows int
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1, 'one')"); err != nil {
		fatal("create", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_dbpage").Scan(&dbpagePages); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM dbstat").Scan(&dbstatRows); err != nil {
		fatal("dbstat", err)
	}
	vtabs := set.Table("vtab_rows", "build", "vtab", "rows")
	vtabs.Row("driver", "sqlite_dbpage", dbpagePages)
	vtabs.Row("driver", "dbstat", dbstatRows)

	required := set.Table("required_options", "option", "in_driver", "in_cli")
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fmt.Printf("sqlite3 CLI: %v\n", err)
		for _, option := range requiredOptions {
			required.Row(option, slices.Contains(options, option), nil)
		}
		set.Check("B", false, "the pinned sqlite3 CLI is available")
		set.Finish("PIN CHECK")
		return
	}
	fmt.Println(cli.PinLine())
	out, err := cli.Exec(ctx, ":memory:", "SELECT sqlite_version();\nPRAGMA compile_options;\n")
	if err != nil {
		fatal("CLI compile_options", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	cliVersion, cliOptions := lines[0], lines[1:]
	version("CLI sqlite_version()", cliVersion, pin.Release)
	version("CLI sqlite_source_id()", cli.SourceID, pin.SourceID)
	for _, option := range requiredOptions {
		required.Row(option, slices.Contains(options, option), slices.Contains(cliOptions, option))
	}

	// The CLI build varies between machines, so everything that depends on its
	// path or its full option list goes in info tables.
	infoCLI := set.Table("info_cli", "path", "origin")
	infoCLI.Row(displayPath(cli.Path), cli.Origin)
	var onlyDriver, onlyCLI []string
	for _, option := range options {
		if !slices.Contains(cliOptions, option) {
			onlyDriver = append(onlyDriver, option)
		}
	}
	for _, option := range cliOptions {
		if !slices.Contains(options, option) {
			onlyCLI = append(onlyCLI, option)
		}
	}
	infoOnly := set.Table("info_option_differences", "side", "options")
	infoOnly.Row("only in the driver", strings.Join(onlyDriver, " "))
	infoOnly.Row("only in this CLI", strings.Join(onlyCLI, " "))

	for _, option := range requiredOptions {
		set.Check("A", slices.Contains(options, option), "the driver is compiled with "+option)
	}
	set.Check("A", dbpagePages == 2 && dbstatRows == 2, "sqlite_dbpage and dbstat answer queries in the driver (2 pages: schema and table t)")
	set.Check("B", cliVersion == pin.Release, "the CLI reports release "+pin.Release)
	for _, option := range requiredOptions {
		set.Check("B", slices.Contains(cliOptions, option), "the CLI is compiled with "+option)
	}

	formatTable := set.Table("format_options", "name", "driver", "cli", "same")
	same := true
	for _, name := range fileFormatAndIOOptions {
		driverValue, inDriver := findOption(options, name)
		cliValue, inCLI := findOption(cliOptions, name)
		if !inDriver {
			driverValue = "(absent)"
		}
		if !inCLI {
			cliValue = "(absent)"
		}
		formatTable.Row(name, driverValue, cliValue, driverValue == cliValue)
		same = same && driverValue == cliValue
	}
	set.Check("B", same, "the driver and the CLI agree on every file-format and I/O compile option listed")
	set.Finish("PIN CHECK")
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

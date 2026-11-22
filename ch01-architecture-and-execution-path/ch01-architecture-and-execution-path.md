# Chapter 1 — Architecture and execution path

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `prepare.c`, `vdbeapi.c`, `vdbeaux.c`, `os.c` |
| Linked C sources (in the pinned tree, not copied) | `vdbe.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree, the C tools (`sqlite3`, `showwal`, `dbhash`), `strace`, and the DuckDB CLI that presents the results.
> - **Run the experiments** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 1.2`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 1.1
>   go run ./cmd/course run 1.2
>   go run ./cmd/course run 1.3
>   ```
>   In the container, scratch databases and tables are written under `/work/1.1/`, `/work/1.2/`, and `/work/1.3/` (the `lab-work` volume); run directly, under `_work/1.1/`, `_work/1.2/`, and `_work/1.3/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert`). Run this way it prints only its verdict and leaves its tables under `_work/1.2/results/`; it needs the C CLI on the host (`go run ./cmd/course fetch-source`, then `go run ./cmd/course build-tools`), and on macOS it skips the parts marked **Linux only** (in this chapter, the `strace`, twin check, `ANALYZE` limit, and `-vfstrace` sections of Experiment 1.2).
> - **Measured platform:** The stored evidence in this chapter, including the syscall and VFS traces, was captured in the Debian `lab` container on `linux/arm64` (Go 1.26). The CLI's path and its optional compile options, and the sync syscall the CLI uses, are run-dependent and not part of the stored evidence.

---

## 1. Model

SQLite is an embedded library, not a separate database server process. A Go program opens a database file directly inside its own address space and drives SQLite through function calls. Every SQL statement crosses six internal layers before bytes reach the operating system's filesystem.

```text
  Go program (database/sql + modernc.org/sqlite)
        |
        |  sqlite3_prepare_v2 / sqlite3_step / sqlite3_finalize
        v
  +---------------------------------------------------------------+
  | 1. Interface       C API entry points, connection mutexes     |
  +---------------------------------------------------------------+
        | (prepare)                      | (step)
        v                                |
  +--------------------------------+     |
  | 2. SQL Compiler                |     |
  |    Tokenizer -> Parser ->      |     |
  |    Query Planner -> Code Gen   |     |
  +--------------------------------+     |
        | emits bytecode                 |
        v                                v
  +---------------------------------------------------------------+
  | 3. VDBE (Virtual Database Engine)                             |
  |    Register-based bytecode interpreter (opcodes, cursors)     |
  +---------------------------------------------------------------+
        | sqlite3BtreeBeginTrans / sqlite3BtreeInsert / Commit
        v
  +---------------------------------------------------------------+
  | 4. B-tree                                                     |
  |    Tables and indexes laid out as trees of fixed-size pages   |
  +---------------------------------------------------------------+
        | sqlite3PagerSharedLock / Write / CommitPhaseOne/Two
        v
  +---------------------------------------------------------------+
  | 5. Pager + Page Cache  <--- WAL (-wal, -shm) or -journal      |
  |    Fixed-size page cache, atomic commits, crash recovery      |
  +---------------------------------------------------------------+
        | sqlite3OsRead / Write / Sync / Lock
        v
  +---------------------------------------------------------------+
  | 6. VFS (Virtual File System)                                  |
  |    OS abstraction (os_unix.c: pread64, pwrite64, fsync, fcntl)|
  +---------------------------------------------------------------+
        |
        v
  Operating System Kernel & Filesystem (.db, .db-wal, .db-shm, .db-journal)
```

Each layer owns a distinct job:

1. **Interface.** The public C functions (`sqlite3_prepare_v2`, `sqlite3_step`, `sqlite3_finalize`). They acquire the connection's mutex, manage prepared statements, and translate internal status codes into return values.
2. **SQL Compiler.** The tokenizer splits SQL text into tokens; the parser checks grammar; the query planner chooses table and index scan order; the code generator emits a **bytecode** program. This course names the compiler and does not read its C source. A platform managing many database files cannot observe or replace the compiler, and no state-capture method depends on it.
3. **VDBE (Virtual Database Engine).** A virtual machine that runs compiled bytecode one instruction (an **opcode**) at a time over numbered value slots called **registers**. A **prepared statement** (`sqlite3_stmt`) is a `Vdbe` struct holding its program, registers, and **cursors** (handles pointing at rows inside table or index trees).
4. **B-tree.** Organizes every table and index as a balanced tree of fixed-size blocks called **pages** (4,096 bytes each by default, numbered starting at 1). Page 1 begins with a 100-byte database header holding persistent file settings and counters, including the **schema cookie** (incremented whenever tables or indexes change) and the **file change counter** (header bytes 24–27, paired with **version-valid-for** at bytes 92–95). Each table row is stored as a **cell** on a **leaf page** and keyed by a 64-bit signed integer called a **rowid**; deleting or shrinking cells leaves unused holes called **freeblocks** inside the page. When a row is larger than one page can hold locally, its trailing bytes spill onto linked **overflow pages**. Emptied pages move onto a **freelist**—a chain of **freelist trunk pages** pointing to **freelist leaf pages**—for later reuse.
5. **Pager and Page Cache.** Reads pages into memory, tracks modified (**dirty**) pages, coordinates locks between connections, and makes transactions **atomic** (all-or-nothing) through one of two logging modes configured by `PRAGMA` (SQLite's command for reading and changing file or connection settings):
   - **Rollback journal (`journal_mode=delete`).** Before changing a page in `.db`, the pager copies the page's original bytes into a sibling `-journal` file and flushes it to durable storage. If the process dies mid-commit, the next opener reads `-journal` and restores the original pages. Deleting `-journal` is the commit point.
   - **Write-ahead log (`journal_mode=wal`).** Instead of overwriting `.db` during a commit, the pager appends each modified page to a sibling `-wal` file as a **frame** (a 24-byte frame header followed by the 4,096-byte page image) and indexes those frames in a shared-memory file (`-shm`). A transaction commits when its final frame records the database's new page count. Later, a **checkpoint** copies committed frames from `-wal` back into `.db`.
6. **VFS (Virtual File System).** A table of function pointers (`xRead`, `xWrite`, `xSync`, `xLock`, `xShmLock`, and so on) between the pager and the operating system. On Unix, the default VFS (`os_unix.c`) translates VFS calls into kernel syscalls: `pread64` (positioned read), `pwrite64` (positioned write), `fsync` or `fdatasync` (flush dirty buffers to durable storage), and `fcntl` (**POSIX advisory locks** on byte ranges of a file).

A platform holding thousands of SQLite files touches only two boundaries in that diagram: **connections** above the pager (SQL statements, transactions, per-connection settings) and **files** below the pager (the `.db` file and its **sidecar files**: `-wal`, `-shm`, and `-journal`).

**WAL mode as the platform default.** Platform databases in this course run in WAL mode (`journal_mode=wal`) because WAL lets readers run concurrently alongside one writer and records committed changes as an append-only stream of page frames. This chapter includes rollback-mode (`journal_mode=delete`) contrasts in two places on purpose: Experiment 1.2 traces the same `INSERT` in both modes to show commits move from overwriting `.db` to appending frames to `-wal`, and Experiment 1.3 runs rollback-mode contrasts beside its canonical WAL database to isolate how each mode handles the header change counter, deleted space, and rolled-back work.

**The Rebuild Ladder (rung R0).** Every chapter advances one operational goal: capture a database's durable state deterministically and prove that a rebuilt copy equals the original. This chapter builds rung **R0 (Recipe)**—a seeded, deterministic workload (`internal/recipe`) whose closed database file has one known SHA-256 hash after every numbered step *k*, proven at equality level **E1 (bytes)** (verbatim file SHA-256). Later chapters verify snapshots, WAL replays, crash recoveries, and fleet rebuilds against those exact hashes.

---

## 2. Question

When a Go program commits one `INSERT`, which layer does what between `sqlite3_prepare_v2` and the bytes on disk, and which of those layers can a platform see from outside the library?

Before tracing that `INSERT`, the course verifies the exact SQLite builds it runs. Every experiment in this chapter imports two shared packages introduced by the foundation chunk (linked rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens databases through `modernc.org/sqlite` with explicit file and connection settings ([`settings.go`](../internal/sqlitex/settings.go), [`open.go`](../internal/sqlitex/open.go)). New databases apply settings in `layout -> journal_mode -> identity` order, and opening an existing database refuses to switch it into or out of WAL mode. The package also verifies `sqlite_source_id()` and linked `modernc.org/sqlite` / `modernc.org/libc` module versions against `sqlite/PIN` (`MustPinCheck`, `MustPinCheckMemory` in [`pin.go`](../internal/sqlitex/pin.go)), manages `_work/N.M/` scratch directories (`MustWorkDir`, `WorkBase` in [`workdir.go`](../internal/sqlitex/workdir.go)), and locates and runs the pinned C `sqlite3` CLI (`LocateCLI`, `OpenScript` in [`cli.go`](../internal/sqlitex/cli.go)).
- [`internal/trace`](../internal/trace/): runs a child command under Linux `strace` and parses its syscalls ([`strace.go`](../internal/trace/strace.go), [`strace_linux.go`](../internal/trace/strace_linux.go)), merges CLI `-vfstrace` output with `strace` into one ordered stream (`RunCLIMerged` in [`merge.go`](../internal/trace/merge.go), [`vfs.go`](../internal/trace/vfs.go)), and prints the Docker fallback notice on non-Linux hosts (`Supported`, `PrintLinuxOnly` in [`linuxonly.go`](../internal/trace/linuxonly.go)).

### Experiment 1.1 — Pin check (`1.1-pin-check`)

- **Claim:** Both SQLite builds used by the course—the `modernc.org/sqlite` Go driver and the `sqlite3` C CLI—are SQLite 3.53.4 at the pinned Fossil check-in, include the required page-inspection virtual tables (`sqlite_dbpage`, which exposes raw pages, and `dbstat`, which reports per-page B-tree statistics) and upstream's WAL sync default (`DEFAULT_WAL_SYNCHRONOUS=2`, meaning `FULL`), and agree on all 15 listed file-format and I/O compile options.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 1.1
  ```

<!-- include: experiments/1.1-pin-check/main.go -->
```go
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
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/1.1-pin-check/report.sql -->
```sql
-- Walkthrough of experiment 1.1. Run it with:
--   go run ./cmd/course run 1.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   pin                 one row per field of sqlite/PIN
--   versions            one row per version the experiment compared with the pin
--   driver_options      one row per compile option of the Go driver (PRAGMA compile_options)
--   vtab_rows           rows that sqlite_dbpage and dbstat return in the driver
--   required_options    one row per compile option that later labs depend on
--   format_options      one row per file-format or I/O option: driver value, CLI value
--   checks              every claim the experiment asserted (the runner lists it last)
-- info_cli and info_option_differences hold the CLI path and the build-specific
-- option differences; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  1.1  Do the Go driver and the sqlite3 CLI run the pinned SQLite?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The course uses two SQLite builds: the modernc.org/sqlite Go driver and a'
.print 'sqlite3 command-line shell. Both must be the pinned check-in and agree on'
.print 'the options that change the file format.'

.print
.print '── A. The pin and the driver ───────────────────────────────────────────'
.print
.print 'A1. The pin (sqlite/PIN) that every experiment checks first:'

SELECT field, value FROM pin ORDER BY field;

.print
.print 'A2. What is linked or reported, against what the pin says. Each row'
.print 'should show the same text in both columns (source ids are cut after 12 hex digits; A1 has them in full):'

SELECT item,
       regexp_replace(found, '([0-9a-f]{12})[0-9a-f]{52}', '\1...')  AS found,
       regexp_replace(pinned, '([0-9a-f]{12})[0-9a-f]{52}', '\1...') AS pinned,
       found = pinned AS match
FROM versions ORDER BY seq;

.print
.print 'A3. The compile options of the driver. The three that later labs need'
.print 'are sqlite_dbpage and dbstat (page inspection) and the FULL sync default in WAL mode:'

SELECT o.option, o.option IN (SELECT option FROM driver_options) AS present
FROM required_options o
ORDER BY o.option;

.print
.print 'A4. The full list of driver options (the last seq is the count):'

SELECT seq, option FROM driver_options ORDER BY seq;

.print
.print 'A5. Both page-inspection virtual tables answer in the driver. The probe'
.print 'database has 2 pages: page 1 for sqlite_schema and page 2 for table t.'

SELECT vtab, rows FROM vtab_rows ORDER BY vtab;

.print
.print '── B. The CLI against the driver ───────────────────────────────────────'
.print
.print 'B1. The CLI has the same required options (in_cli should be true too):'

SELECT option, in_driver, in_cli FROM required_options ORDER BY option;

.print
.print 'B2. The options that govern the file format, cache, mmap and sync defaults'
.print 'must be identical in both builds:'

SELECT name, driver, cli, CASE WHEN same THEN 'same' ELSE 'DIFFERENT' END AS verdict
FROM format_options ORDER BY name;
```

`course run` also prints the run-dependent values of this machine, which are not part of the stored evidence: the path of the CLI it used and the compile options that only one of the two builds has.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/1.1-pin-check/`](evidence/1.1-pin-check/), and `go run ./cmd/course check evidence ch01` compares a fresh run with them byte for byte.

<!-- include: evidence/1.1-pin-check.txt -->
```text

════════════════════════════════════════════════════════════════════════
  1.1  Do the Go driver and the sqlite3 CLI run the pinned SQLite?
════════════════════════════════════════════════════════════════════════

The course uses two SQLite builds: the modernc.org/sqlite Go driver and a
sqlite3 command-line shell. Both must be the pinned check-in and agree on
the options that change the file format.

── A. The pin and the driver ───────────────────────────────────────────

A1. The pin (sqlite/PIN) that every experiment checks first:
┌───────────┬──────────────────────────────────────────────────────────────────────────────────────┐
│   field   │                                        value                                         │
├───────────┼──────────────────────────────────────────────────────────────────────────────────────┤
│ fossil    │ bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc                     │
│ git       │ b09c88c14082339b66c7b7158d609a771e64ca69                                             │
│ release   │ 3.53.4                                                                               │
│ source_id │ 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc │
│ tag       │ version-3.53.4                                                                       │
└───────────┴──────────────────────────────────────────────────────────────────────────────────────┘

A2. What is linked or reported, against what the pin says. Each row
should show the same text in both columns (source ids are cut after 12 hex digits; A1 has them in full):
┌──────────────────────────────────┬─────────────────────────────────────┬─────────────────────────────────────┬───────┐
│               item               │                found                │               pinned                │ match │
├──────────────────────────────────┼─────────────────────────────────────┼─────────────────────────────────────┼───────┤
│ driver module modernc.org/sqlite │ v1.60.1                             │ v1.60.1                             │ true  │
│ libc module modernc.org/libc     │ v1.77.1                             │ v1.77.1                             │ true  │
│ driver sqlite_version()          │ 3.53.4                              │ 3.53.4                              │ true  │
│ driver sqlite_source_id()        │ 2026-07-24 19:02:57 bf7c7f300318... │ 2026-07-24 19:02:57 bf7c7f300318... │ true  │
│ CLI sqlite_version()             │ 3.53.4                              │ 3.53.4                              │ true  │
│ CLI sqlite_source_id()           │ 2026-07-24 19:02:57 bf7c7f300318... │ 2026-07-24 19:02:57 bf7c7f300318... │ true  │
└──────────────────────────────────┴─────────────────────────────────────┴─────────────────────────────────────┴───────┘

A3. The compile options of the driver. The three that later labs need
are sqlite_dbpage and dbstat (page inspection) and the FULL sync default in WAL mode:
┌───────────────────────────┬─────────┐
│          option           │ present │
├───────────────────────────┼─────────┤
│ DEFAULT_WAL_SYNCHRONOUS=2 │ true    │
│ ENABLE_DBPAGE_VTAB        │ true    │
│ ENABLE_DBSTAT_VTAB        │ true    │
└───────────────────────────┴─────────┘

A4. The full list of driver options (the last seq is the count):
┌─────┬─────────────────────────────────┐
│ seq │             option              │
├─────┼─────────────────────────────────┤
│ 1   │ ATOMIC_INTRINSICS=1             │
│ 2   │ COMPILER=gcc-12.2.0             │
│ 3   │ DEFAULT_AUTOVACUUM              │
│ 4   │ DEFAULT_CACHE_SIZE=-2000        │
│ 5   │ DEFAULT_FILE_FORMAT=4           │
│ 6   │ DEFAULT_JOURNAL_SIZE_LIMIT=-1   │
│ 7   │ DEFAULT_MEMSTATUS=0             │
│ 8   │ DEFAULT_MMAP_SIZE=0             │
│ 9   │ DEFAULT_PAGE_SIZE=4096          │
│ 10  │ DEFAULT_PCACHE_INITSZ=20        │
│ 11  │ DEFAULT_RECURSIVE_TRIGGERS      │
│ 12  │ DEFAULT_SECTOR_SIZE=4096        │
│ 13  │ DEFAULT_SYNCHRONOUS=2           │
│ 14  │ DEFAULT_WAL_AUTOCHECKPOINT=1000 │
│ 15  │ DEFAULT_WAL_SYNCHRONOUS=2       │
│ 16  │ DEFAULT_WORKER_THREADS=0        │
│ 17  │ DIRECT_OVERFLOW_READ            │
│ 18  │ DISABLE_INTRINSIC               │
│ 19  │ ENABLE_COLUMN_METADATA          │
│ 20  │ ENABLE_DBPAGE_VTAB              │
│ 21  │ ENABLE_DBSTAT_VTAB              │
│ 22  │ ENABLE_FTS5                     │
│ 23  │ ENABLE_GEOPOLY                  │
│ 24  │ ENABLE_MATH_FUNCTIONS           │
│ 25  │ ENABLE_MEMORY_MANAGEMENT        │
│ 26  │ ENABLE_OFFSET_SQL_FUNC          │
│ 27  │ ENABLE_PREUPDATE_HOOK           │
│ 28  │ ENABLE_RBU                      │
│ 29  │ ENABLE_RTREE                    │
│ 30  │ ENABLE_SESSION                  │
│ 31  │ ENABLE_SNAPSHOT                 │
│ 32  │ ENABLE_STAT4                    │
│ 33  │ ENABLE_UNLOCK_NOTIFY            │
│ 34  │ LIKE_DOESNT_MATCH_BLOBS         │
│ 35  │ MALLOC_SOFT_LIMIT=1024          │
│ 36  │ MAX_ATTACHED=10                 │
│ 37  │ MAX_COLUMN=2000                 │
│ 38  │ MAX_COMPOUND_SELECT=500         │
│ 39  │ MAX_DEFAULT_PAGE_SIZE=8192      │
│ 40  │ MAX_EXPR_DEPTH=1000             │
│ 41  │ MAX_FUNCTION_ARG=1000           │
│ 42  │ MAX_LENGTH=1000000000           │
│ 43  │ MAX_LIKE_PATTERN_LENGTH=50000   │
│ 44  │ MAX_MMAP_SIZE=0x7fff0000        │
│ 45  │ MAX_PAGE_COUNT=0xfffffffe       │
│ 46  │ MAX_PAGE_SIZE=65536             │
│ 47  │ MAX_SQL_LENGTH=1000000000       │
│ 48  │ MAX_TRIGGER_DEPTH=1000          │
│ 49  │ MAX_VARIABLE_NUMBER=32766       │
│ 50  │ MAX_VDBE_OP=250000000           │
│ 51  │ MAX_WORKER_THREADS=8            │
│ 52  │ MUTEX_PTHREADS                  │
│ 53  │ SOUNDEX                         │
│ 54  │ SYSTEM_MALLOC                   │
│ 55  │ TEMP_STORE=1                    │
│ 56  │ THREADSAFE=1                    │
└─────┴─────────────────────────────────┘

A5. Both page-inspection virtual tables answer in the driver. The probe
database has 2 pages: page 1 for sqlite_schema and page 2 for table t.
┌───────────────┬──────┐
│     vtab      │ rows │
├───────────────┼──────┤
│ dbstat        │ 2    │
│ sqlite_dbpage │ 2    │
└───────────────┴──────┘

── B. The CLI against the driver ───────────────────────────────────────

B1. The CLI has the same required options (in_cli should be true too):
┌───────────────────────────┬───────────┬────────┐
│          option           │ in_driver │ in_cli │
├───────────────────────────┼───────────┼────────┤
│ DEFAULT_WAL_SYNCHRONOUS=2 │ true      │ true   │
│ ENABLE_DBPAGE_VTAB        │ true      │ true   │
│ ENABLE_DBSTAT_VTAB        │ true      │ true   │
└───────────────────────────┴───────────┴────────┘

B2. The options that govern the file format, cache, mmap and sync defaults
must be identical in both builds:
┌────────────────────────────┬─────────────────────────────────┬─────────────────────────────────┬─────────┐
│            name            │             driver              │               cli               │ verdict │
├────────────────────────────┼─────────────────────────────────┼─────────────────────────────────┼─────────┤
│ DEFAULT_AUTOVACUUM         │ DEFAULT_AUTOVACUUM              │ DEFAULT_AUTOVACUUM              │ same    │
│ DEFAULT_CACHE_SIZE         │ DEFAULT_CACHE_SIZE=-2000        │ DEFAULT_CACHE_SIZE=-2000        │ same    │
│ DEFAULT_FILE_FORMAT        │ DEFAULT_FILE_FORMAT=4           │ DEFAULT_FILE_FORMAT=4           │ same    │
│ DEFAULT_JOURNAL_SIZE_LIMIT │ DEFAULT_JOURNAL_SIZE_LIMIT=-1   │ DEFAULT_JOURNAL_SIZE_LIMIT=-1   │ same    │
│ DEFAULT_MMAP_SIZE          │ DEFAULT_MMAP_SIZE=0             │ DEFAULT_MMAP_SIZE=0             │ same    │
│ DEFAULT_PAGE_SIZE          │ DEFAULT_PAGE_SIZE=4096          │ DEFAULT_PAGE_SIZE=4096          │ same    │
│ DEFAULT_PCACHE_INITSZ      │ DEFAULT_PCACHE_INITSZ=20        │ DEFAULT_PCACHE_INITSZ=20        │ same    │
│ DEFAULT_SECTOR_SIZE        │ DEFAULT_SECTOR_SIZE=4096        │ DEFAULT_SECTOR_SIZE=4096        │ same    │
│ DEFAULT_SYNCHRONOUS        │ DEFAULT_SYNCHRONOUS=2           │ DEFAULT_SYNCHRONOUS=2           │ same    │
│ DEFAULT_WAL_AUTOCHECKPOINT │ DEFAULT_WAL_AUTOCHECKPOINT=1000 │ DEFAULT_WAL_AUTOCHECKPOINT=1000 │ same    │
│ DEFAULT_WAL_SYNCHRONOUS    │ DEFAULT_WAL_SYNCHRONOUS=2       │ DEFAULT_WAL_SYNCHRONOUS=2       │ same    │
│ DIRECT_OVERFLOW_READ       │ DIRECT_OVERFLOW_READ            │ DIRECT_OVERFLOW_READ            │ same    │
│ MAX_MMAP_SIZE              │ MAX_MMAP_SIZE=0x7fff0000        │ MAX_MMAP_SIZE=0x7fff0000        │ same    │
│ MAX_PAGE_SIZE              │ MAX_PAGE_SIZE=65536             │ MAX_PAGE_SIZE=65536             │ same    │
│ TEMP_STORE                 │ TEMP_STORE=1                    │ TEMP_STORE=1                    │ same    │
└────────────────────────────┴─────────────────────────────────┴─────────────────────────────────┴─────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the linked driver is modernc.org/sqlite v1.60.1
A  ✓  the linked libc is modernc.org/libc v1.77.1
A  ✓  the driver reports release 3.53.4
A  ✓  the driver is compiled with ENABLE_DBPAGE_VTAB
A  ✓  the driver is compiled with ENABLE_DBSTAT_VTAB
A  ✓  the driver is compiled with DEFAULT_WAL_SYNCHRONOUS=2
A  ✓  sqlite_dbpage and dbstat answer queries in the driver (2 pages: schema and table t)
B  ✓  the CLI reports release 3.53.4
B  ✓  the CLI is compiled with ENABLE_DBPAGE_VTAB
B  ✓  the CLI is compiled with ENABLE_DBSTAT_VTAB
B  ✓  the CLI is compiled with DEFAULT_WAL_SYNCHRONOUS=2
B  ✓  the driver and the CLI agree on every file-format and I/O compile option listed

12 of 12 claims hold

Experiment verdict: PIN CHECK OK
```

**Reading the evidence:**

- **A1–A2 (pin and linked modules).** The driver's `sqlite_source_id()`, read from an in-memory database opened through `database/sql`, is verified against `sqlite/PIN` (A1 shows the pin in full). The module rows of A2 read Go's compiled-in `runtime/debug.ReadBuildInfo()` rather than `go.mod`, proving the binary linked `modernc.org/sqlite v1.60.1` and `modernc.org/libc v1.77.1`, and the `sqlite_version()` rows confirm `3.53.4`.
- **A3–A4 (driver compile options).** `PRAGMA compile_options` reports 56 options in the Go driver (A4). A3 verifies the three options later labs depend on: `ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB`, and `DEFAULT_WAL_SYNCHRONOUS=2`.
- **A5 (virtual table smoke test).** After creating table `t` and inserting one row, both `sqlite_dbpage` and `dbstat` return 2 rows: page 1 (`sqlite_schema`) and page 2 (table `t`).
- **A2 and B1 (C CLI check).** `sqlitex.LocateCLI` finds the pinned C `sqlite3` binary (its path is run-dependent, printed by `course run` and not stored, because it depends on whether the host uses a system binary or the built tools). Its source ID and version (A2) and three required options (B1) match the pin.
- **B2 (file-format and I/O parity).** Both builds agree on all 15 options controlling page size (`4096`), default cache size (`-2000`, meaning 2,000 KiB), mmap default (`0`), and sync defaults (`2` = `FULL`).
- **Optional feature differences (run-dependent, printed by `course run`).** The two builds differ on optional features not in that 15-option table. The Go driver enables `ENABLE_STAT4` and `ENABLE_SNAPSHOT` (compiled into the translated C even though the Go API does not expose `sqlite3_snapshot_*`), while this CLI enables `ENABLE_BYTECODE_VTAB`, `ENABLE_STMTVTAB`, and `ENABLE_EXPLAIN_COMMENTS` (so `bytecode()` and `sqlite_stmt` exist only in the CLI). Most optional flags do not touch storage, but `ENABLE_STAT4` changes on-disk tables whenever `ANALYZE` (SQLite's query-planner statistics command) runs—a boundary Experiment 1.2 measures directly.

---

## 3. Call path

This section follows one autocommit statement—`INSERT INTO t(id, v) VALUES(2, 'two')`—through the pinned SQLite `version-3.53.4` C source. Four files are copied whole into this chapter directory (`prepare.c`, `vdbeapi.c`, `vdbeaux.c`, `os.c`); `vdbe.c` is linked in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`) rather than copied.

### 3.1 Prepare: compiling SQL into a VDBE program

Preparation compiles SQL text into bytecode without writing to the database file. It reads the database only if the connection has not yet loaded `sqlite_schema`.

1. **`sqlite3_prepare_v2`** — [`src/prepare.c:937-954`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L937-L954) (local copy: [`prepare.c`](prepare.c)). Forwards straight to `sqlite3LockAndPrepare` with `SQLITE_PREPARE_SAVESQL`:
   ```c
   rc = sqlite3LockAndPrepare(db,zSql,nBytes,SQLITE_PREPARE_SAVESQL,0,
                              ppStmt,pzTail);
   ```
   Saving the SQL text on the statement lets `sqlite3_step` recompile it automatically if another connection changes the schema before execution.
2. **`sqlite3LockAndPrepare`** — [`src/prepare.c:836-875`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L836-L875) (local copy: [`prepare.c`](prepare.c)). Enters the connection mutex (`sqlite3_mutex_enter(db->mutex)` at line 855) and every attached database's B-tree mutex (`sqlite3BtreeEnterAll` at line 856), then calls `sqlite3Prepare` in a retry loop (lines 857–866). On `SQLITE_SCHEMA`, it resets the connection's cached schema (`sqlite3ResetOneSchema(db, -1)` at line 866) and retries once.
3. **`sqlite3Prepare`** — [`src/prepare.c:682-835`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L682-L835) (local copy: [`prepare.c`](prepare.c)). Initializes a `Parse` context, checks that no shared-cache connection holds a schema lock (`sqlite3BtreeSchemaLocked` at line 752), and runs the tokenizer and parser via `sqlite3RunParser` (line 779 or 786). Grammar actions emit opcodes into a `Vdbe` object. Line 795 stores the original SQL text via `sqlite3VdbeSetSql`. On failure line 803 checks `schemaIsValid`; on success line 818 returns the `Vdbe` pointer as `sqlite3_stmt *`.

### 3.2 Step: running bytecode and crossing into the B-tree

Execution starts only when the caller steps the prepared statement.

4. **`sqlite3_step`** — [`src/vdbeapi.c:919-966`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeapi.c#L919-L966) (local copy: [`vdbeapi.c`](vdbeapi.c)). Takes the connection mutex (line 929) and runs `sqlite3Step` inside a schema-retry loop:
   ```c
   while( (rc = sqlite3Step(v))==SQLITE_SCHEMA
          && cnt++ < SQLITE_MAX_SCHEMA_RETRY ){
     int savedPc = v->pc;
     rc = sqlite3Reprepare(v);
   ```
   If another connection altered the schema since `sqlite3_prepare_v2`, `sqlite3_step` calls `sqlite3Reprepare` (line 933), resets the statement (line 954), and runs it again up to `SQLITE_MAX_SCHEMA_RETRY` (50, [`src/vdbeInt.h:26`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeInt.h#L26)) times. The caller never sees `SQLITE_SCHEMA` unless recompilation fails.
5. **`sqlite3Step`** — [`src/vdbeapi.c:777-912`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeapi.c#L777-L912) (local copy: [`vdbeapi.c`](vdbeapi.c)). Moves a `VDBE_READY_STATE` statement to `VDBE_RUN_STATE` (line 825), increments `db->nVdbeActive` and (for non-read-only statements) `db->nVdbeWrite` (lines 821–822), and sets program counter `p->pc = 0` (line 824). If the statement is already in `VDBE_HALT_STATE`, line 852 auto-resets it first. It then dispatches to `sqlite3VdbeList` for `EXPLAIN` statements (line 864) or runs the bytecode interpreter `sqlite3VdbeExec` (line 869). When an autocommit statement ends with `SQLITE_DONE`, line 886 calls `doWalCallbacks` (which runs automatic WAL checkpoints when the frame threshold is reached).
6. **`OP_Transaction`** — [`src/vdbe.c:4102-4201`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbe.c#L4102-L4201) (in the pinned tree). Starts a read (`P2=0`) or write (`P2!=0`) transaction on database `P1` via `sqlite3BtreeBeginTrans` (line 4128). On `SQLITE_BUSY`, it saves the program counter and returns so the caller can retry. When `P5` is non-zero, it compares the file's schema cookie against the compiled cookie (`P3`); a mismatch sets `"database schema has changed"` (line 4174) and expires the statement with `SQLITE_SCHEMA` (lines 4191–4192), driving the retry loop in `sqlite3_step`.
7. **`OP_Insert`** — [`src/vdbe.c:5748-5834`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbe.c#L5748-L5834) (in the pinned tree). Passes the encoded record built by `OP_MakeRecord` (register `P2`) and the rowid (register `P3`) into the B-tree through cursor `P1` via `sqlite3BtreeInsert` (line 5816). When flags in `P5` request it, lines 5802–5803 update the connection's change count and `last_insert_rowid`. `OP_Insert` writes nothing to disk directly; the B-tree calls `sqlite3PagerWrite` to mark the target page writable. In rollback mode, that first `sqlite3PagerWrite` creates `-journal` and saves the page's original image; in WAL mode, no file write happens until commit.

### 3.3 Halt and commit: crossing into the pager and VFS

8. **`sqlite3VdbeHalt`** — [`src/vdbeaux.c:3315-3517`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L3315-L3517) (local copy: [`vdbeaux.c`](vdbeaux.c)). Runs when a statement hits `OP_Halt` (or `OP_AutoCommit` for an explicit `COMMIT`). Line 3339 closes all cursors. If the connection is in autocommit mode and this is the last active writer (`db->autoCommit && db->nVdbeWrite==(p->readOnly==0)`, lines 3402–3403), it checks deferred foreign keys and calls `vdbeCommit` (line 3421); if `vdbeCommit` fails, line 3429 calls `sqlite3RollbackAll`. Otherwise (inside an explicit transaction or while another writer statement is open), line 3464 closes only the statement's sub-transaction via `sqlite3VdbeCloseStatement`.
9. **`vdbeCommit`** — [`src/vdbeaux.c:2919-3175`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L2919-L3175) (local copy: [`vdbeaux.c`](vdbeaux.c)). Loops over every attached database with an active write transaction, acquires an `EXCLUSIVE` pager lock (`sqlite3PagerExclusiveLock` at line 2973), and inspects `aMJNeeded` (lines 2954–2961) to count how many need a multi-file super-journal (`DELETE`, `PERSIST`, and `TRUNCATE` need one when two or more databases are written; `WAL` sets `0`). With one database (or in WAL mode), it runs two-phase commit: `sqlite3BtreeCommitPhaseOne` (line 3005) writes and syncs dirty pages; `sqlite3BtreeCommitPhaseTwo` (line 3020) deletes the rollback journal or releases WAL locks. Later, `sqlite3_finalize` ([`src/vdbeapi.c:105-124`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeapi.c#L105-L124), local copy [`vdbeapi.c`](vdbeapi.c)) resets and frees the `Vdbe`.
10. **The VFS wrappers** — [`src/os.c:88-102`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L88-L102) (local copy: [`os.c`](os.c)), specifically `sqlite3OsRead` ([`src/os.c:88-91`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L88-L91)), `sqlite3OsWrite` ([`src/os.c:92-95`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L92-L95)), `sqlite3OsTruncate` (lines 96–98), and `sqlite3OsSync` ([`src/os.c:99-102`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L99-L102)). Everything above the VFS calls these one-line wrappers to dispatch through the open file's method table (`pMethods`):
    ```c
    int sqlite3OsRead(sqlite3_file *id, void *pBuf, int amt, i64 offset){
      DO_OS_MALLOC_TEST(id);
      return id->pMethods->xRead(id, pBuf, amt, offset);
    }
    int sqlite3OsWrite(sqlite3_file *id, const void *pBuf, int amt, i64 offset){
      DO_OS_MALLOC_TEST(id);
      return id->pMethods->xWrite(id, pBuf, amt, offset);
    }
    int sqlite3OsTruncate(sqlite3_file *id, i64 size){
      return id->pMethods->xTruncate(id, size);
    }
    int sqlite3OsSync(sqlite3_file *id, int flags){
      DO_OS_MALLOC_TEST(id);
      return flags ? id->pMethods->xSync(id, flags) : SQLITE_OK;
    }
    ```
    In `sqlite3OsSync` (lines 99–102), `flags == 0` (as under `synchronous=OFF`) returns `SQLITE_OK` without calling `xSync`. Below this table, `os_unix.c` issues `pread64`, `pwrite64`, `ftruncate`, and `fsync`/`fdatasync`.

### Version notes

Behavior differences across SQLite releases relevant to this chapter (sources: [`docs/knowledge/source-inventory-ch01-04.md`](../docs/knowledge/source-inventory-ch01-04.md), `changes.html`, and the pinned source):

- **`sqlite3VdbeSerialPut()` removed (2022-04-02).** Record serialization was inlined into `OP_MakeRecord`, noted at [`src/vdbeaux.c:3856`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L3856) (local copy: [`vdbeaux.c`](vdbeaux.c)).
- **`bytecode` and `tables_used` virtual tables (3.32.0, `changes.html`).** Enabled in the C CLI (`ENABLE_BYTECODE_VTAB`) and absent in the Go driver.
- **`sqlite3_stmt_explain()` (3.43.0, `changes.html`).** Switches a prepared statement between normal execution and `EXPLAIN`.
- **`sqlite3_stmt_scanstatus_v2()` in `EXPLAIN QUERY PLAN` (3.41.0, `changes.html`).** Requires `SQLITE_ENABLE_STMT_SCANSTATUS`, which neither build enables.
- **3.53.4 baseline fixes.** Release 3.53.4 includes `SQLITE_CHECKPOINT_NOOP` (3.51.0) and the WAL-reset corruption fix (3.51.3 / 3.53.0).

### Experiment 1.2 — One INSERT through the layers (`1.2-one-insert`)

- **Claim:**
  - One committed `INSERT` crosses every layer, and each crossing is observable from outside: opcodes (`EXPLAIN`), changed pages (`sqlite_dbpage`), journal or WAL writes (pager), VFS calls (`-vfstrace`), and kernel syscalls (`strace`).
  - In rollback mode, each `pwrite64` on `t.db` writes one whole changed page at offset `(page - 1) * 4096`.
  - In WAL mode, the commit makes zero writes to `t.db` and writes only the 32-byte WAL header at offset 0 of the new `t.db-wal`, followed by a 24-byte frame header and 4,096-byte page image per changed page.
  - For SQL statements whose behavior does not depend on compile options differing between the two builds, `modernc.org/sqlite` and the C CLI issue the same writes, syncs, reads, locks, opens, unlinks, and closes in the same order and leave byte-identical files, differing only in extra `fcntl(F_SETFD)` calls issued by `modernc.org/libc`'s musl-derived `open()`.
  - Where compile options do differ (`ENABLE_STAT4` on `ANALYZE`), the two builds diverge and write different files.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Platform:** **Linux only** for `strace`, the twin check, its `ANALYZE` limit, and the `-vfstrace` merged trace; `course run` runs it in the Linux `lab` container, so it runs in full on any host. On macOS, running the experiment package directly (`go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert`) does not require the C CLI: it prints a `LINUX-ONLY:` line, records `EXPLAIN` and the changed page hashes with their expected-page checks (`[1 2]` in rollback mode, `[2]` in WAL mode), and exits 0.
- **Why kept:** this is the one place the course earns the right to use a translated driver. Everything after relies on it.
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 1.2
  ```

<!-- include: experiments/1.2-one-insert/main.go -->
```go
// Experiment 1.2 traces one committed INSERT through every SQLite layer:
// VDBE bytecode (EXPLAIN), B-tree pages (sqlite_dbpage), pager and VFS calls
// (C CLI -vfstrace), and kernel syscalls (strace). It runs in both rollback
// (DELETE) and WAL modes, performs the twin check comparing the translated Go
// driver against the C CLI, and demonstrates the twin check's compile-option
// limit with ANALYZE (ENABLE_STAT4).
//
// It prints only its verdict; the measurements go to CSV tables that
// report.sql presents (Linux for strace; on macOS the runner uses Docker):
//
//	go run ./cmd/course run 1.2
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch01-architecture-and-execution-path/experiments/1.2-one-insert"

// On-disk geometry and SQL shared by the rollback and WAL runs: 4096-byte
// pages, 32-byte WAL header, and 24-byte WAL frame header (src/wal.c:21-60).
const (
	pageSize       = 4096
	walHeaderSize  = 32
	walFrameHeader = 24
	createSQL      = "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one')"
	insertSQL      = "INSERT INTO t(id, v) VALUES(2, 'two')"
	dbName         = "t.db"
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables. part labels the claims of the part
// that is running: A the EXPLAIN listing, B rollback mode, C WAL mode, D the
// ANALYZE limit of the twin check.
var (
	set    *results.Set
	part   string
	tables struct {
		explain, runs, pageChanges, goSyscalls, cliSyscalls, walFrames, fileHashes, phases, phaseEvents, analyze *results.Table
		infoRun, infoSync, infoRandomness                                                                        *results.Table
	}
)

func openTables() {
	tables.explain = set.Table("explain_ops", "addr", "opcode", "p1", "p2", "p3", "p4", "p5")
	tables.runs = set.Table("runs", "mode", "base_pages", "base_sha256")
	tables.pageChanges = set.Table("page_changes", "mode", "page", "before_sha256", "after_sha256")
	tables.goSyscalls = set.Table("go_syscalls", "mode", "seq", "call", "norm")
	tables.cliSyscalls = set.Table("cli_syscalls", "mode", "seq", "norm")
	tables.walFrames = set.Table("wal_frames", "mode", "frame_no", "page", "commit_size")
	tables.fileHashes = set.Table("file_hashes", "mode", "what", "sha256")
	tables.phases = set.Table("phases", "mode", "phase_no", "statement", "opcode", "call_path")
	tables.phaseEvents = set.Table("phase_events", "mode", "phase_no", "seq", "kind", "nested", "text")
	tables.analyze = set.Table("analyze_runs", "stage", "build", "sha256", "stat4_compiled", "stat_tables")
	tables.infoRun = set.Table("info_run", "key", "value")
	tables.infoSync = set.Table("info_sync", "mode", "go_driver", "cli", "cli_path")
	tables.infoRandomness = set.Table("info_randomness", "mode", "phase_no", "vfs_call")
}

func check(ok bool, claim string) { set.Check(part, ok, claim) }

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func options(mode string) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = mode
	o.SingleConnection = true
	return o
}

// cliPrelude translates sqlitex.Options into PRAGMA statements so the C CLI
// runs under the exact same connection settings as the Go driver.
func cliPrelude(o sqlitex.Options) string {
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	c := o.Conn
	return fmt.Sprintf("PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		o.File.JournalMode, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete), c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

func markerPath(dir, name string) string {
	return filepath.Join(dir, "marker-"+name)
}

// createMarkers creates empty files that both the Go child and the CLI (.read)
// open immediately before and after a statement. Because CLI stdout is
// buffered, opening a named file puts an unbuffered syscall bracket in strace
// at the exact statement boundary.
func createMarkers(dir string, names ...string) {
	for _, name := range names {
		if err := os.WriteFile(markerPath(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

func touchMarker(dir, name string) {
	if _, err := os.ReadFile(markerPath(dir, name)); err != nil {
		fatal("marker", err)
	}
}

func createBase(ctx context.Context, path, mode string) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("create base", err)
	}
	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		fatal("create base", err)
	}
	if err := db.Close(); err != nil {
		fatal("close base", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("base at rest", err)
	}
}

// pageHashes reads every page through the sqlite_dbpage virtual table and
// returns its SHA-256 hex digest in page-number order.
func pageHashes(ctx context.Context, path, mode string) []string {
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("open for sqlite_dbpage", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var pgno int
		var data []byte
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage scan", err)
		}
		if pgno != len(hashes)+1 {
			fatal("sqlite_dbpage", fmt.Errorf("page %d out of order", pgno))
		}
		sum := sha256.Sum256(data)
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	return hashes
}

func changedPages(before, after []string) []int {
	var changed []int
	for i := range max(len(before), len(after)) {
		if i >= len(before) || i >= len(after) || before[i] != after[i] {
			changed = append(changed, i+1)
		}
	}
	return changed
}

func short(hash string) string { return hash[:16] }

// printExplain queries SQLite's bytecode listing for insertSQL, which
// sqlite3Step produces via sqlite3VdbeList (src/vdbeapi.c:863-865).
func printExplain(ctx context.Context, path string) {
	db, err := sqlitex.Open(ctx, path, options(sqlitex.JournalDelete))
	if err != nil {
		fatal("open for EXPLAIN", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+insertSQL)
	if err != nil {
		fatal("EXPLAIN", err)
	}
	defer rows.Close()
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			fatal("EXPLAIN scan", err)
		}
		var p4Value any
		if p4.Valid && p4.String != "" {
			p4Value = p4.String
		}
		tables.explain.Row(addr, opcode, p1, p2, p3, p4Value, p5)
	}
	if err := rows.Err(); err != nil {
		fatal("EXPLAIN", err)
	}
}

type walFrame struct {
	Index      int
	Page       uint32
	CommitSize uint32
}

// readWALFrames decodes the page number (bytes 0..3) and commit-size field
// (bytes 4..7) from every 24-byte frame header after the 32-byte WAL header.
func readWALFrames(path string) ([]walFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var frames []walFrame
	for offset := walHeaderSize; offset+walFrameHeader+pageSize <= len(data); offset += walFrameHeader + pageSize {
		frames = append(frames, walFrame{
			Index:      len(frames) + 1,
			Page:       binary.BigEndian.Uint32(data[offset:]),
			CommitSize: binary.BigEndian.Uint32(data[offset+4:]),
		})
	}
	return frames, nil
}

// childInsert is the re-exec'd child entry point traced by strace. It locks
// its goroutine to one OS thread so strace sees its syscalls in order, brackets
// the INSERT with marker files, and reads the -wal frames before db.Close
// checkpoints and deletes the WAL.
func childInsert(dir, mode string) {
	runtime.LockOSThread()
	ctx := context.Background()
	path := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("child open", err)
	}
	touchMarker(dir, "1")
	if _, err := db.ExecContext(ctx, insertSQL); err != nil {
		fatal("child insert", err)
	}
	touchMarker(dir, "2")
	if mode == sqlitex.JournalWAL {
		// Read the -wal before closing, since closing the last WAL connection
		// checkpoints frames into t.db and deletes t.db-wal. Reading the -wal
		// (not t.db) avoids touching the POSIX locks held on t.db and t.db-shm.
		frames, err := readWALFrames(path + "-wal")
		if err != nil {
			fatal("child read wal", err)
		}
		for _, frame := range frames {
			fmt.Printf("frame %d page=%d commit_size=%d\n", frame.Index, frame.Page, frame.CommitSize)
		}
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func parseChildFrames(stdout string) []walFrame {
	var frames []walFrame
	for _, line := range strings.Split(stdout, "\n") {
		var frame walFrame
		if _, err := fmt.Sscanf(line, "frame %d page=%d commit_size=%d", &frame.Index, &frame.Page, &frame.CommitSize); err == nil {
			frames = append(frames, frame)
		}
	}
	return frames
}

func isMarkerOpen(call trace.Syscall, dir string) (string, bool) {
	if call.Name != "open" && call.Name != "openat" {
		return "", false
	}
	prefix := markerPath(dir, "")
	if !strings.HasPrefix(call.Path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(call.Path, prefix), true
}

func isMarkerFile(call trace.Syscall, dir string) bool {
	return strings.HasPrefix(call.Path, markerPath(dir, ""))
}

var fileSyscalls = []string{"open", "openat", "close", "pread64", "pwrite64", "read", "write", "fsync", "fdatasync", "ftruncate", "unlink", "unlinkat", "fcntl", "mmap"}

func isDatabaseIO(call trace.Syscall, dir string) bool {
	if !slices.Contains(fileSyscalls, call.Name) || isMarkerFile(call, dir) {
		return false
	}
	return call.Path == dir || strings.HasPrefix(call.Path, dir+string(filepath.Separator))
}

// segmentCalls extracts all database-directory file syscalls strictly between
// the open of marker from and the open of marker to.
func segmentCalls(calls []trace.Syscall, dir, from, to string) ([]trace.Syscall, error) {
	start, end := -1, -1
	for i, call := range calls {
		name, ok := isMarkerOpen(call, dir)
		if !ok {
			continue
		}
		if name == from && start < 0 {
			start = i
		}
		if name == to && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("markers %s..%s not found in the trace", from, to)
	}
	var out []trace.Syscall
	for _, call := range calls[start+1 : end] {
		if isDatabaseIO(call, dir) {
			out = append(out, call)
		}
	}
	return trace.RelativeTo(dir, out), nil
}

func syncName(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

// normalized canonicalizes syscall spellings that differ across libc or
// configure builds (open vs openat, unlink vs unlinkat, fsync vs fdatasync)
// while preserving errno on failed calls, short-transfer byte counts, and
// read/write lengths so the twin check compares the actual file operations.
func normalized(call trace.Syscall) string {
	text := normalizedOperation(call)
	switch {
	case call.Errno != "":
		text += " = " + call.Errno
	case transfersBytes(call) && call.Length >= 0 && call.Result != call.Length:
		text += fmt.Sprintf(" = %d", call.Result)
	}
	return text
}

func transfersBytes(call trace.Syscall) bool {
	switch call.Name {
	case "pwrite64", "pread64", "write", "read":
		return true
	}
	return false
}

func normalizedOperation(call trace.Syscall) string {
	switch {
	case call.Name == "open" || call.Name == "openat":
		return "open " + call.Path
	case syncName(call.Name):
		return "sync " + call.Path
	case call.Name == "unlink" || call.Name == "unlinkat":
		return "unlink " + call.Path
	case call.Name == "pwrite64" || call.Name == "pread64":
		return fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
	case call.Name == "write" || call.Name == "read":
		return fmt.Sprintf("%s %s len=%d", call.Name, call.Path, call.Length)
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case call.Name == "mmap":
		return fmt.Sprintf("mmap %s off=%d len=%d", call.Path, call.Offset, call.Length)
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, call.Flags)
	}
	return call.Name + " " + call.Path
}

func normalizedAll(calls []trace.Syscall, keep func(trace.Syscall) bool) []string {
	var out []string
	for _, call := range calls {
		if keep == nil || keep(call) {
			out = append(out, normalized(call))
		}
	}
	return out
}

// isCloexecFcntl matches the extra fcntl(fd, F_SETFD, FD_CLOEXEC) call that
// modernc's musl-derived libc issues after every O_CLOEXEC open, whereas
// SQLite's C build relies on O_CLOEXEC directly.
func isCloexecFcntl(call trace.Syscall) bool {
	return call.Name == "fcntl" && call.Lock == nil && strings.HasPrefix(call.Flags, "F_SETFD")
}

func isWrite(call trace.Syscall) bool {
	return call.Name == "pwrite64" || call.Name == "write" || call.Name == "ftruncate"
}

func isSync(call trace.Syscall) bool {
	return syncName(call.Name)
}

func syncCallNames(calls []trace.Syscall) string {
	var names []string
	for _, call := range calls {
		if isSync(call) && !slices.Contains(names, call.Name) {
			names = append(names, call.Name)
		}
	}
	return strings.Join(names, ",")
}

type phase struct {
	no        int
	from, to  string
	statement string
	opcode    string
	path      string
}

// phases maps the three split statements (BEGIN IMMEDIATE, INSERT, COMMIT) to
// the opcodes and C call paths they isolate inside one autocommit INSERT.
func phases(mode string) []phase {
	commitPath := "sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne/PhaseTwo -> sqlite3PagerCommitPhaseOne/PhaseTwo"
	insertPath := "sqlite3BtreeInsert -> sqlite3PagerWrite"
	if mode == sqlitex.JournalWAL {
		commitPath = "sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne -> sqlite3PagerCommitPhaseOne -> pagerWalFrames -> sqlite3WalFrames"
	}
	return []phase{
		{1, "1", "2", "BEGIN IMMEDIATE", "OP_Transaction", "sqlite3BtreeBeginTrans -> sqlite3PagerSharedLock, sqlite3PagerBegin"},
		{2, "2", "3", insertSQL, "OP_Insert", insertPath},
		{3, "3", "4", "COMMIT", "OP_AutoCommit (OP_Halt in an autocommit INSERT)", commitPath},
	}
}

func splitScript(dir string, o sqlitex.Options) string {
	return cliPrelude(o) +
		".read '" + markerPath(dir, "1") + "'\nBEGIN IMMEDIATE;\n" +
		".read '" + markerPath(dir, "2") + "'\n" + insertSQL + ";\n" +
		".read '" + markerPath(dir, "3") + "'\nCOMMIT;\n" +
		".read '" + markerPath(dir, "4") + "'\n"
}

func autocommitScript(dir string, o sqlitex.Options) string {
	return cliPrelude(o) +
		".read '" + markerPath(dir, "1") + "'\n" + insertSQL + ";\n" +
		".read '" + markerPath(dir, "2") + "'\n"
}

type eventSegment struct {
	events []trace.Event
	calls  []trace.Syscall
}

func mergedSegment(merged *trace.Merged, dir, from, to string) (eventSegment, error) {
	start, end := -1, -1
	for i, event := range merged.Events {
		if event.Kind != trace.EventSyscall {
			continue
		}
		name, ok := isMarkerOpen(*event.Syscall, dir)
		if !ok {
			continue
		}
		if name == from && start < 0 {
			start = i
		}
		if name == to && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return eventSegment{}, fmt.Errorf("markers %s..%s not found in the merged trace", from, to)
	}
	var segment eventSegment
	for _, event := range merged.Events[start+1 : end] {
		switch event.Kind {
		case trace.EventVFS:
			segment.events = append(segment.events, event)
		case trace.EventSyscall:
			if isDatabaseIO(*event.Syscall, dir) {
				segment.events = append(segment.events, event)
				segment.calls = append(segment.calls, *event.Syscall)
			}
		}
	}
	segment.calls = trace.RelativeTo(dir, segment.calls)
	return segment, nil
}

func vfsLine(call trace.VFSCall, dir string) string {
	calls := trace.RelativeVFS(dir, []trace.VFSCall{call})
	return strings.TrimPrefix(strings.TrimSpace(calls[0].Raw), "trace.")
}

func runMode(ctx context.Context, base, mode string, cli sqlitex.CLI, haveCLI bool) {
	dir := filepath.Join(base, mode)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(dir, dbName)
	createMarkers(dir, "1", "2", "3", "4")
	part = map[string]string{sqlitex.JournalDelete: "B", sqlitex.JournalWAL: "C"}[mode]

	createBase(ctx, path, mode)
	before := pageHashes(ctx, path, mode)
	baseHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	tables.runs.Row(mode, len(before), short(baseHash))
	tables.fileHashes.Row(mode, "base", short(baseHash))

	// On non-Linux hosts, strace is unavailable; still run the INSERT in
	// process so the reader sees which pages changed.
	if !trace.Supported {
		db, err := sqlitex.Open(ctx, path, options(mode))
		if err != nil {
			fatal("open", err)
		}
		if _, err := db.ExecContext(ctx, insertSQL); err != nil {
			fatal("insert", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		if err := recipe.CheckAtRest(path); err != nil {
			fatal("at rest", err)
		}
		after := pageHashes(ctx, path, mode)
		changed := changedPages(before, after)
		recordChanged(mode, before, after, changed)
		checkChangedPages(mode, changed)
		return
	}

	command, err := trace.SelfCommand("child-insert", dir, mode)
	if err != nil {
		fatal("self command", err)
	}
	goRun, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace Go child", err)
	}
	if goRun.ExitCode != 0 {
		fatal("Go child", fmt.Errorf("exit %d: %s", goRun.ExitCode, goRun.Stderr))
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("Go child at rest", err)
	}
	after := pageHashes(ctx, path, mode)
	goFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	changed := changedPages(before, after)
	recordChanged(mode, before, after, changed)
	checkChangedPages(mode, changed)

	tables.fileHashes.Row(mode, "go_insert", short(goFileHash))
	goCalls, err := segmentCalls(goRun.Log.Calls, dir, "1", "2")
	if err != nil {
		fatal("Go segment", err)
	}
	for i, call := range goCalls {
		tables.goSyscalls.Row(mode, i+1, printed(call), normalized(call))
	}

	dbWrites := 0
	offsetsMatch := true
	written := map[int]int{}
	for _, call := range goCalls {
		if call.Name == "pwrite64" && call.Path == dbName {
			dbWrites++
			page := int(call.Offset/pageSize) + 1
			written[page]++
			if call.Offset%pageSize != 0 || call.Length != pageSize || !slices.Contains(changed, page) {
				offsetsMatch = false
			}
		}
	}
	if mode == sqlitex.JournalDelete {
		// In rollback mode, pager_write_pagelist writes each modified page
		// directly to t.db at offset (page - 1) * pageSize.
		everyChangedWrittenOnce := len(written) == len(changed)
		for _, page := range changed {
			if written[page] != 1 {
				everyChangedWrittenOnce = false
			}
		}
		check(dbWrites > 0 && offsetsMatch && everyChangedWrittenOnce, fmt.Sprintf("rollback mode: each pwrite64 on %s is one whole page at offset (page-1)*4096, and the pages written are exactly the changed pages %v", dbName, changed))
	} else {
		// In WAL mode, pagerWalFrames appends each modified page as a frame in
		// t.db-wal and leaves t.db untouched until a checkpoint runs.
		check(dbWrites == 0, fmt.Sprintf("WAL mode: the commit writes nothing to %s itself (database-file pwrite64 calls during the INSERT: %d)", dbName, dbWrites))
		frames := parseChildFrames(goRun.Stdout)
		var framePages []int
		for _, frame := range frames {
			tables.walFrames.Row(mode, frame.Index, frame.Page, frame.CommitSize)
			framePages = append(framePages, int(frame.Page))
		}
		var walWrites []string
		for _, call := range goCalls {
			if call.Name == "pwrite64" && call.Path == dbName+"-wal" {
				walWrites = append(walWrites, fmt.Sprintf("%d+%d", call.Offset, call.Length))
			}
		}
		walWritesMatch := slices.Equal(walWrites, expectedFreshWALWrites(frames))
		lastCommit := len(frames) > 0 && frames[len(frames)-1].CommitSize > 0
		check(slices.Equal(framePages, changed) && walWritesMatch && lastCommit, fmt.Sprintf("WAL mode: the commit writes the 32-byte WAL header at offset 0 of the new %s-wal, then one frame per changed page %v (frame header 24 bytes, then the page at offset 32 + (frame-1)*4120 + 24), and nothing else; the last frame carries the commit size", dbName, changed))
	}

	if !haveCLI {
		return
	}

	// Twin check: rebuild the base and run the exact same autocommit INSERT in
	// the C CLI under strace, then compare writes, syncs, other syscalls, and
	// final file bytes.
	createBase(ctx, path, mode)
	cliCommand := cli.Command(ctx, sqlitex.CLIRun{}).Args
	cliCommand[0] = cli.Path
	cliRun, err := trace.Run(ctx, trace.Options{Command: cliCommand, Dir: dir, Stdin: strings.NewReader(sqlitex.OpenScript(path, autocommitScript(dir, options(mode))))})
	if err != nil {
		fatal("strace CLI", err)
	}
	if cliRun.ExitCode != 0 {
		fatal("CLI", fmt.Errorf("exit %d: %s", cliRun.ExitCode, cliRun.Stderr))
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("CLI at rest", err)
	}
	cliFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	cliCalls, err := segmentCalls(cliRun.Log.Calls, dir, "1", "2")
	if err != nil {
		fatal("CLI segment", err)
	}
	tables.fileHashes.Row(mode, "cli_insert", short(cliFileHash))
	for i, line := range normalizedAll(cliCalls, nil) {
		tables.cliSyscalls.Row(mode, i+1, line)
	}
	tables.infoSync.Row(mode, syncCallNames(goCalls), syncCallNames(cliCalls), displayPath(cli.Path))
	writesSame := slices.Equal(normalizedAll(goCalls, isWrite), normalizedAll(cliCalls, isWrite))
	syncsSame := slices.Equal(normalizedAll(goCalls, isSync), normalizedAll(cliCalls, isSync))
	goSyncs := normalizedAll(goCalls, isSync)
	check(writesSame && syncsSame && len(goSyncs) > 0, "twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order")
	notCloexec := func(call trace.Syscall) bool { return !isCloexecFcntl(call) }
	allSame := slices.Equal(normalizedAll(goCalls, notCloexec), normalizedAll(cliCalls, notCloexec))
	check(allSame, "apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)")
	check(goFileHash == cliFileHash && goFileHash != baseHash, fmt.Sprintf("the database file after the INSERT is byte-identical from both builds (sha256 %s)", short(goFileHash)))

	// Measured call path: split the INSERT into BEGIN IMMEDIATE, INSERT, and
	// COMMIT under CLI -vfstrace + strace so each VFS call and syscall is
	// attributed to the opcode that caused it, then verify the combined
	// syscalls equal the autocommit INSERT's.
	createBase(ctx, path, mode)
	merged, splitRun, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: dir, DB: path, SQL: splitScript(dir, options(mode))}, trace.Options{})
	if err != nil {
		fatal("CLI -vfstrace under strace", err)
	}
	if splitRun.ExitCode != 0 {
		fatal("CLI -vfstrace", fmt.Errorf("exit %d: %s", splitRun.ExitCode, splitRun.Stderr))
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("CLI -vfstrace at rest", err)
	}
	splitFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	tables.fileHashes.Row(mode, "cli_split", short(splitFileHash))
	var concatenated []trace.Syscall
	for _, p := range phases(mode) {
		segment, err := mergedSegment(merged, dir, p.from, p.to)
		if err != nil {
			fatal("merged segment", err)
		}
		concatenated = append(concatenated, segment.calls...)
		tables.phases.Row(mode, p.no, p.statement, p.opcode, p.path)
		seq := 0
		for _, event := range segment.events {
			switch event.Kind {
			case trace.EventVFS:
				if event.VFS.Method == "xRandomness" {
					// Different CLI builds seed SQLite's PRNG at different
					// points, so keep xRandomness on an INFO line.
					tables.infoRandomness.Row(mode, p.no, vfsLine(*event.VFS, dir))
					continue
				}
				seq++
				tables.phaseEvents.Row(mode, p.no, seq, "VFS", false, vfsLine(*event.VFS, dir))
			case trace.EventSyscall:
				relative := trace.RelativeTo(dir, []trace.Syscall{*event.Syscall})[0]
				seq++
				tables.phaseEvents.Row(mode, p.no, seq, "SYS", event.Enclosed >= 0, normalized(relative))
			}
		}
	}
	check(slices.Equal(normalizedAll(concatenated, nil), normalizedAll(cliCalls, nil)), "the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)")
	check(splitFileHash == cliFileHash, "BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT")
}

// expectedChangedPages returns the 1-based pages that the INSERT modifies:
// pages 1 and 2 in rollback mode (header change counter plus table leaf), and
// page 2 alone in WAL mode (page 1 is not dirtied).
func expectedChangedPages(mode string) []int {
	if mode == sqlitex.JournalWAL {
		return []int{2}
	}
	return []int{1, 2}
}

// checkChangedPages verifies that sqlite_dbpage observed exactly the pages
// expected for mode (runs on both Linux and non-Linux hosts).
func checkChangedPages(mode string, changed []int) {
	want := expectedChangedPages(mode)
	reason := "page 1 (the change counter in the header) and page 2 (table t's leaf)"
	if mode == sqlitex.JournalWAL {
		reason = "only page 2 (table t's leaf); page 1 is not written, so its change counter stays"
	}
	check(slices.Equal(changed, want), fmt.Sprintf("the INSERT changes %s: %v", reason, want))
}

// expectedFreshWALWrites returns the complete sequence of "offset+length"
// pwrite64 operations for a freshly created WAL file: the 32-byte WAL header
// at offset 0 followed by each frame's 24-byte header and 4096-byte page image.
func expectedFreshWALWrites(frames []walFrame) []string {
	if len(frames) == 0 {
		return nil
	}
	want := []string{fmt.Sprintf("0+%d", walHeaderSize)}
	for _, frame := range frames {
		offset := walHeaderSize + (frame.Index-1)*(walFrameHeader+pageSize)
		want = append(want, fmt.Sprintf("%d+%d", offset, walFrameHeader), fmt.Sprintf("%d+%d", offset+walFrameHeader, pageSize))
	}
	return want
}

func recordChanged(mode string, before, after []string, changed []int) {
	for _, page := range changed {
		var b, a any
		if page <= len(before) {
			b = short(before[page-1])
		}
		if page <= len(after) {
			a = short(after[page-1])
		}
		tables.pageChanges.Row(mode, page, b, a)
	}
}

// printed renders a syscall as strace shows it, with open/openat and
// unlink/unlinkat spelled alike: which one libc calls depends on the platform.
func printed(call trace.Syscall) string {
	text := call.String()
	text = strings.Replace(text, "openat ", "open ", 1)
	return strings.Replace(text, "unlinkat ", "unlink ", 1)
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "child-insert" {
		childInsert(os.Args[2], os.Args[3])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("1.2")
	base, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	set = results.MustOpen(work)
	openTables()
	tables.infoRun.Row("work dir", displayPath(base))
	var cli sqlitex.CLI
	haveCLI := false
	if trace.Supported {
		located, cliErr := sqlitex.LocateCLI(ctx)
		cli, haveCLI = located, cliErr == nil
		if haveCLI {
			fmt.Println(cli.PinLine())
		} else {
			fmt.Printf("sqlite3 CLI: unavailable: %v\n", cliErr)
			part = "setup"
			check(false, "the pinned sqlite3 CLI is available")
		}
	}

	part = "A"
	explainPath := filepath.Join(base, "explain.db")
	createBase(ctx, explainPath, sqlitex.JournalDelete)
	printExplain(ctx, explainPath)

	if !trace.Supported {
		trace.PrintLinuxOnly("the strace of the Go driver and of the CLI, the twin check, its ANALYZE limit and the measured call path", packagePath)
	}
	for _, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		runMode(ctx, base, mode, cli, haveCLI)
	}
	if haveCLI {
		twinLimitAnalyze(ctx, base, cli)
	}

	set.Finish("ONE INSERT")
}

const (
	analyzeSchemaSQL = "CREATE TABLE s(id INTEGER PRIMARY KEY, k INTEGER NOT NULL); CREATE INDEX s_by_k ON s(k); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) INSERT INTO s(id, k) SELECT i, i % 7 FROM n"
	stat4OptionSQL   = "SELECT sqlite_compileoption_used('ENABLE_STAT4')"
)

// statTables returns the sorted names of any sqlite_stat* tables created by
// ANALYZE in the database at path.
func statTables(ctx context.Context, path string) []string {
	db, err := sqlitex.Open(ctx, path, options(sqlitex.JournalWAL))
	if err != nil {
		fatal("open for sqlite_schema", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE name LIKE 'sqlite_stat%' ORDER BY name")
	if err != nil {
		fatal("sqlite_schema", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fatal("sqlite_schema scan", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_schema", err)
	}
	return names
}

// twinLimitAnalyze demonstrates a boundary of the twin check: starting from
// byte-identical copies of an indexed table, ANALYZE writes sqlite_stat4 in
// the ENABLE_STAT4-enabled Go driver but only sqlite_stat1 in the CLI, so the
// resulting database files diverge.
func twinLimitAnalyze(ctx context.Context, base string, cli sqlitex.CLI) {
	part = "D"
	dir := filepath.Join(base, "analyze")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	goPath := filepath.Join(dir, "go.db")
	cliPath := filepath.Join(dir, "cli.db")
	o := options(sqlitex.JournalWAL)
	for _, path := range []string{goPath, cliPath} {
		if err := recipe.RemoveDatabase(path); err != nil {
			fatal("remove", err)
		}
		db, err := sqlitex.Open(ctx, path, o)
		if err != nil {
			fatal("create analyze base", err)
		}
		if _, err := db.ExecContext(ctx, analyzeSchemaSQL); err != nil {
			fatal("create analyze base", err)
		}
		if err := db.Close(); err != nil {
			fatal("close analyze base", err)
		}
	}
	baseGo, err := recipe.FileSHA256(goPath)
	if err != nil {
		fatal("hash", err)
	}
	baseCLI, err := recipe.FileSHA256(cliPath)
	if err != nil {
		fatal("hash", err)
	}
	tables.analyze.Row("base", "go.db", short(baseGo), nil, nil)
	tables.analyze.Row("base", "cli.db", short(baseCLI), nil, nil)

	db, err := sqlitex.Open(ctx, goPath, o)
	if err != nil {
		fatal("open", err)
	}
	var goStat4 int
	if err := db.QueryRowContext(ctx, stat4OptionSQL).Scan(&goStat4); err != nil {
		fatal("compile option", err)
	}
	if _, err := db.ExecContext(ctx, "ANALYZE"); err != nil {
		fatal("ANALYZE", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	out, err := cli.Exec(ctx, cliPath, cliPrelude(o)+stat4OptionSQL+";\nANALYZE;\n")
	if err != nil {
		fatal("CLI ANALYZE", err)
	}
	lines := strings.Fields(out)
	cliStat4 := -1
	if len(lines) > 0 {
		fmt.Sscan(lines[len(lines)-1], &cliStat4)
	}
	for _, path := range []string{goPath, cliPath} {
		if err := recipe.CheckAtRest(path); err != nil {
			fatal("analyze at rest", err)
		}
	}
	goHash, err := recipe.FileSHA256(goPath)
	if err != nil {
		fatal("hash", err)
	}
	cliHash, err := recipe.FileSHA256(cliPath)
	if err != nil {
		fatal("hash", err)
	}
	goTables, cliTables := statTables(ctx, goPath), statTables(ctx, cliPath)
	tables.analyze.Row("after ANALYZE", "go.db", short(goHash), goStat4, strings.Join(goTables, " "))
	tables.analyze.Row("after ANALYZE", "cli.db", short(cliHash), cliStat4, strings.Join(cliTables, " "))
	check(baseGo == baseCLI, "both copies start byte-identical")
	if goStat4 == 1 && cliStat4 == 0 {
		check(goHash != cliHash && slices.Contains(goTables, "sqlite_stat4") && !slices.Contains(cliTables, "sqlite_stat4"),
			"twin check limit: the driver is compiled with ENABLE_STAT4 and this CLI is not, so the same ANALYZE writes a sqlite_stat4 table only in the driver and the two files differ")
	} else {
		check(goStat4 == cliStat4 && goHash == cliHash, "with the same ENABLE_STAT4 setting the same ANALYZE leaves byte-identical files")
	}
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
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/1.2-one-insert/report.sql -->
```sql
-- Walkthrough of experiment 1.2. Run it with:
--   go run ./cmd/course run 1.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   explain_ops      one row per VDBE opcode of EXPLAIN INSERT
--   runs             one row per journal mode: the base database before the INSERT
--   page_changes     one row per page the INSERT changed, by mode (sha256 prefix before and after)
--   go_syscalls      one row per file syscall of the Go driver's INSERT, in order
--   cli_syscalls     one row per file syscall of the C CLI's INSERT, in order (normalized)
--   wal_frames       one row per frame the WAL mode commit appended
--   file_hashes      one row per database file whose bytes were hashed after a run
--   phases           one row per split statement (BEGIN IMMEDIATE, INSERT, COMMIT) of the CLI run
--   phase_events     one row per VFS call or syscall inside a phase, in order
--   analyze_runs     one row per database file of the ANALYZE limit
--   checks           every claim the experiment asserted (the runner lists it last)
-- info_run, info_sync and info_randomness hold values that depend on the machine
-- or the CLI build; info.sql shows them.

CREATE TEMP VIEW calls AS
SELECT 'go' AS build, mode, seq, norm FROM go_syscalls
UNION ALL
SELECT 'cli', mode, seq, norm FROM cli_syscalls;

CREATE TEMP VIEW twin_categories AS
SELECT 1 AS ord, 'writes (file, offset, length, order)' AS category, * FROM calls
WHERE split_part(norm, ' ', 1) IN ('pwrite64', 'write', 'ftruncate')
UNION ALL
SELECT 2, 'syncs (file, order)', * FROM calls WHERE split_part(norm, ' ', 1) = 'sync'
UNION ALL
SELECT 3, 'every call except fcntl F_SETFD', * FROM calls WHERE norm NOT LIKE 'fcntl % F_SETFD%'
UNION ALL
SELECT 4, 'fcntl F_SETFD only', * FROM calls WHERE norm LIKE 'fcntl % F_SETFD%';

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  1.2  What does one committed INSERT do, layer by layer?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'One INSERT of the row (2, "two") into table t(id, v), which already holds'
.print 'one row (2 pages: page 1 for sqlite_schema, page 2 for t), followed down the'
.print 'layers: VDBE bytecode, the B-tree pages it changes, the file syscalls of the'
.print 'Go driver (modernc, traced with strace), the same INSERT in the C sqlite3 CLI'
.print '(the twin check), and the CLI with -vfstrace, which names the VFS call behind'
.print 'each syscall. Parts B and C do this in rollback-journal and in WAL mode;'
.print 'part D shows where the twin check stops being valid.'

.print
.print '── A. The bytecode ─────────────────────────────────────────────────────'
.print
.print 'A1. EXPLAIN of the INSERT. OpenWrite opens the table b-tree (root page 2);'
.print 'MakeRecord builds the row; Insert (p5=49) puts it in the leaf; Transaction'
.print 'at addr 14 is where the write lock is taken.'

SELECT addr, opcode, p1, p2, p3, p4, p5 FROM explain_ops ORDER BY addr;

.print
.print '── B. Rollback journal (journal_mode=delete) ───────────────────────────'
.print
.print 'B1. The base database, and the pages the INSERT changed (sha256 prefix of'
.print 'each page, read through sqlite_dbpage before and after).'
.print 'Page 1 changes too: the change counter in its header.'

SELECT r.mode, r.base_pages AS pages_before, r.base_sha256 AS file_sha256,
       p.page, p.before_sha256, p.after_sha256
FROM runs r JOIN page_changes p USING (mode)
WHERE mode = 'delete'
ORDER BY p.page;

.print
.print 'B2. The file syscalls the Go driver made for the INSERT, in order'
.print '(open/openat and unlink/unlinkat are written open and unlink; "= fd" stands'
.print 'for the descriptor number, which varies):'

SELECT seq, call FROM go_syscalls WHERE mode = 'delete' ORDER BY seq;

.print
.print 'B3. Where the bytes land. Each pwrite64 on t.db is one whole page at'
.print 'offset (page - 1) * 4096; the pages written are the pages that changed:'

SELECT call, CAST(regexp_extract(norm, 'off=(\d+)', 1) AS INTEGER) AS file_offset,
       CAST(regexp_extract(norm, 'off=(\d+)', 1) AS INTEGER) // 4096 + 1 AS page,
       CAST(regexp_extract(norm, 'len=(\d+)', 1) AS INTEGER) AS length
FROM go_syscalls
WHERE mode = 'delete' AND split_part(norm, ' ', 1) = 'pwrite64' AND split_part(norm, ' ', 2) = 't.db'
ORDER BY seq;

.print
.print 'B4. The C CLI runs the same INSERT under strace. Its syscalls, normalized'
.print '(open/openat as open, fsync/fdatasync as sync); read and write lengths are'
.print 'kept, so a short transfer would show:'

SELECT seq, norm FROM cli_syscalls WHERE mode = 'delete' ORDER BY seq;

.print
.print 'B5. The twin check: the Go driver and the CLI compared category by category.'
.print 'Writes and syncs must be identical, and so must every call apart from the'
.print 'fcntl(F_SETFD) that the libc of modernc adds after each O_CLOEXEC open:'

SELECT category,
       count(*) FILTER (build = 'go')  AS go_calls,
       count(*) FILTER (build = 'cli') AS cli_calls,
       CASE WHEN ord = 4 THEN NULL
            ELSE list(norm ORDER BY seq) FILTER (build = 'go') = list(norm ORDER BY seq) FILTER (build = 'cli')
       END AS identical
FROM twin_categories
WHERE mode = 'delete'
GROUP BY ord, category
ORDER BY ord;

.print
.print 'The F_SETFD calls only the Go driver makes:'

SELECT seq, call FROM go_syscalls WHERE mode = 'delete' AND call LIKE 'fcntl % F_SETFD%' ORDER BY seq;

.print
.print 'B6. The database file after each run, as a hash. The Go driver and the CLI'
.print 'must leave the same bytes, and the INSERT split into three statements must'
.print 'leave the same bytes as the autocommit INSERT:'

SELECT what, sha256 FROM file_hashes WHERE mode = 'delete'
ORDER BY CASE what WHEN 'base' THEN 1 WHEN 'go_insert' THEN 2 WHEN 'cli_insert' THEN 3 ELSE 4 END;

.print
.print 'B7. The measured call path. The CLI runs BEGIN IMMEDIATE; INSERT; COMMIT with'
.print 'a marker file opened between the statements, so each VFS call and syscall is'
.print 'assigned to the statement that caused it. SYS lines indented under a VFS line'
.print 'are the syscalls that VFS call made. xRandomness calls are left out (info.sql).'

SELECT p.phase_no AS phase, p.statement, p.opcode
FROM phases p WHERE mode = 'delete' ORDER BY p.phase_no;

.print
.print 'The C functions behind each statement, outermost first:'

SELECT phase_no AS phase, unnest(range(1, len(steps) + 1)) AS n, unnest(steps) AS c_function
FROM (SELECT phase_no, string_split(call_path, ' -> ') AS steps FROM phases WHERE mode = 'delete')
ORDER BY phase, n;

.print
.print 'The calls of each statement, in order:'

SELECT phase_no AS phase, kind, repeat('    ', CAST(nested AS INTEGER)) || text AS call
FROM phase_events WHERE mode = 'delete' ORDER BY phase_no, seq;

.print
.print 'The three phases together make exactly the syscalls of the autocommit INSERT:'

SELECT (SELECT list(text ORDER BY phase_no, seq) FROM phase_events WHERE mode = 'delete' AND kind = 'SYS')
     = (SELECT list(norm ORDER BY seq) FROM cli_syscalls WHERE mode = 'delete') AS same_syscalls;

.print
.print '── C. Write-ahead log (journal_mode=wal, the default) ──────────────────'
.print
.print 'C1. The base database, and the pages the INSERT changed (sha256 prefix of'
.print 'each page, read through sqlite_dbpage before and after).'
.print 'Page 1 is not written, so its change counter stays.'

SELECT r.mode, r.base_pages AS pages_before, r.base_sha256 AS file_sha256,
       p.page, p.before_sha256, p.after_sha256
FROM runs r JOIN page_changes p USING (mode)
WHERE mode = 'wal'
ORDER BY p.page;

.print
.print 'C2. The file syscalls the Go driver made for the INSERT, in order'
.print '(open/openat and unlink/unlinkat are written open and unlink; "= fd" stands'
.print 'for the descriptor number, which varies):'

SELECT seq, call FROM go_syscalls WHERE mode = 'wal' ORDER BY seq;

.print
.print 'C3. Where the bytes land. The commit writes nothing to t.db itself; it'
.print 'appends one frame per changed page to t.db-wal after a 32-byte WAL header.'
.print 'The frame table is read from the -wal file by the traced child; offsets are'
.print '32 + (frame - 1) * 4120 for the 24-byte frame header, then +24 for the page:'

SELECT (SELECT count(*) FROM go_syscalls
        WHERE mode = 'wal' AND split_part(norm, ' ', 1) = 'pwrite64' AND split_part(norm, ' ', 2) = 't.db')
         AS pwrites_on_t_db;

SELECT frame_no AS frame, page, commit_size,
       32 + (frame_no - 1) * 4120      AS header_at,
       32 + (frame_no - 1) * 4120 + 24 AS page_image_at
FROM wal_frames WHERE mode = 'wal' ORDER BY frame_no;

.print
.print 'C4. The C CLI runs the same INSERT under strace. Its syscalls, normalized'
.print '(open/openat as open, fsync/fdatasync as sync); read and write lengths are'
.print 'kept, so a short transfer would show:'

SELECT seq, norm FROM cli_syscalls WHERE mode = 'wal' ORDER BY seq;

.print
.print 'C5. The twin check: the Go driver and the CLI compared category by category.'
.print 'Writes and syncs must be identical, and so must every call apart from the'
.print 'fcntl(F_SETFD) that the libc of modernc adds after each O_CLOEXEC open:'

SELECT category,
       count(*) FILTER (build = 'go')  AS go_calls,
       count(*) FILTER (build = 'cli') AS cli_calls,
       CASE WHEN ord = 4 THEN NULL
            ELSE list(norm ORDER BY seq) FILTER (build = 'go') = list(norm ORDER BY seq) FILTER (build = 'cli')
       END AS identical
FROM twin_categories
WHERE mode = 'wal'
GROUP BY ord, category
ORDER BY ord;

.print
.print 'The F_SETFD calls only the Go driver makes:'

SELECT seq, call FROM go_syscalls WHERE mode = 'wal' AND call LIKE 'fcntl % F_SETFD%' ORDER BY seq;

.print
.print 'C6. The database file after each run, as a hash. The Go driver and the CLI'
.print 'must leave the same bytes, and the INSERT split into three statements must'
.print 'leave the same bytes as the autocommit INSERT:'

SELECT what, sha256 FROM file_hashes WHERE mode = 'wal'
ORDER BY CASE what WHEN 'base' THEN 1 WHEN 'go_insert' THEN 2 WHEN 'cli_insert' THEN 3 ELSE 4 END;

.print
.print 'C7. The measured call path. The CLI runs BEGIN IMMEDIATE; INSERT; COMMIT with'
.print 'a marker file opened between the statements, so each VFS call and syscall is'
.print 'assigned to the statement that caused it. SYS lines indented under a VFS line'
.print 'are the syscalls that VFS call made. xRandomness calls are left out (info.sql).'

SELECT p.phase_no AS phase, p.statement, p.opcode
FROM phases p WHERE mode = 'wal' ORDER BY p.phase_no;

.print
.print 'The C functions behind each statement, outermost first:'

SELECT phase_no AS phase, unnest(range(1, len(steps) + 1)) AS n, unnest(steps) AS c_function
FROM (SELECT phase_no, string_split(call_path, ' -> ') AS steps FROM phases WHERE mode = 'wal')
ORDER BY phase, n;

.print
.print 'The calls of each statement, in order:'

SELECT phase_no AS phase, kind, repeat('    ', CAST(nested AS INTEGER)) || text AS call
FROM phase_events WHERE mode = 'wal' ORDER BY phase_no, seq;

.print
.print 'The three phases together make exactly the syscalls of the autocommit INSERT:'

SELECT (SELECT list(text ORDER BY phase_no, seq) FROM phase_events WHERE mode = 'wal' AND kind = 'SYS')
     = (SELECT list(norm ORDER BY seq) FROM cli_syscalls WHERE mode = 'wal') AS same_syscalls;

.print
.print '── D. A limit of the twin check ────────────────────────────────────────'
.print
.print 'The twin check assumes both builds write the same bytes, which holds only'
.print 'if their compile options agree. The driver is compiled with ENABLE_STAT4'
.print 'and this CLI is not. Two copies of a 100-row indexed table start byte for'
.print 'byte identical; the Go driver and the CLI each run ANALYZE on one copy:'

SELECT stage, build AS file, sha256, stat4_compiled, stat_tables
FROM analyze_runs
ORDER BY stage = 'base' DESC, build;
```

`course run` also prints the run-dependent values, which are not part of the stored evidence: the work directory, the sync syscall each build used (`fsync` or `fdatasync`), and the VFS calls where the CLI first asks for randomness.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/1.2-one-insert/`](evidence/1.2-one-insert/), and `go run ./cmd/course check evidence ch01` compares a fresh run with them byte for byte.

<!-- include: evidence/1.2-one-insert.txt -->
```text

════════════════════════════════════════════════════════════════════════
  1.2  What does one committed INSERT do, layer by layer?
════════════════════════════════════════════════════════════════════════

One INSERT of the row (2, "two") into table t(id, v), which already holds
one row (2 pages: page 1 for sqlite_schema, page 2 for t), followed down the
layers: VDBE bytecode, the B-tree pages it changes, the file syscalls of the
Go driver (modernc, traced with strace), the same INSERT in the C sqlite3 CLI
(the twin check), and the CLI with -vfstrace, which names the VFS call behind
each syscall. Parts B and C do this in rollback-journal and in WAL mode;
part D shows where the twin check stops being valid.

── A. The bytecode ─────────────────────────────────────────────────────

A1. EXPLAIN of the INSERT. OpenWrite opens the table b-tree (root page 2);
MakeRecord builds the row; Insert (p5=49) puts it in the leaf; Transaction
at addr 14 is where the write lock is taken.
┌──────┬─────────────┬──────┬────┬────┬──────┬────┐
│ addr │   opcode    │  p1  │ p2 │ p3 │  p4  │ p5 │
├──────┼─────────────┼──────┼────┼────┼──────┼────┤
│ 0    │ Init        │ 0    │ 14 │ 0  │      │ 0  │
│ 1    │ OpenWrite   │ 0    │ 2  │ 0  │ 2    │ 0  │
│ 2    │ SoftNull    │ 2    │ 0  │ 0  │      │ 0  │
│ 3    │ String8     │ 0    │ 3  │ 0  │ two  │ 0  │
│ 4    │ Integer     │ 2    │ 1  │ 0  │      │ 0  │
│ 5    │ NotNull     │ 1    │ 7  │ 0  │      │ 0  │
│ 6    │ NewRowid    │ 0    │ 1  │ 0  │      │ 0  │
│ 7    │ MustBeInt   │ 1    │ 0  │ 0  │      │ 0  │
│ 8    │ HaltIfNull  │ 1299 │ 2  │ 3  │ t.v  │ 1  │
│ 9    │ NotExists   │ 0    │ 11 │ 1  │      │ 0  │
│ 10   │ Halt        │ 1555 │ 2  │ 0  │ t.id │ 2  │
│ 11   │ MakeRecord  │ 2    │ 2  │ 4  │ DB   │ 0  │
│ 12   │ Insert      │ 0    │ 4  │ 1  │ t    │ 49 │
│ 13   │ Halt        │ 0    │ 0  │ 0  │      │ 0  │
│ 14   │ Transaction │ 0    │ 1  │ 1  │ 0    │ 1  │
│ 15   │ Goto        │ 0    │ 1  │ 0  │      │ 0  │
└──────┴─────────────┴──────┴────┴────┴──────┴────┘

── B. Rollback journal (journal_mode=delete) ───────────────────────────

B1. The base database, and the pages the INSERT changed (sha256 prefix of
each page, read through sqlite_dbpage before and after).
Page 1 changes too: the change counter in its header.
┌────────┬──────────────┬──────────────────┬──────┬──────────────────┬──────────────────┐
│  mode  │ pages_before │   file_sha256    │ page │  before_sha256   │   after_sha256   │
├────────┼──────────────┼──────────────────┼──────┼──────────────────┼──────────────────┤
│ delete │ 2            │ 0aec0e95a3f65e0c │ 1    │ bb133eb73b58bce5 │ 0ba56047337c7717 │
│ delete │ 2            │ 0aec0e95a3f65e0c │ 2    │ 5d68aacbd116afe8 │ b21207c95d70a25b │
└────────┴──────────────┴──────────────────┴──────┴──────────────────┴──────────────────┘

B2. The file syscalls the Go driver made for the INSERT, in order
(open/openat and unlink/unlinkat are written open and unlink; "= fd" stands
for the descriptor number, which varies):
┌─────┬────────────────────────────────────────────────────────────────────────┐
│ seq │                                  call                                  │
├─────┼────────────────────────────────────────────────────────────────────────┤
│ 1   │ fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0                  │
│ 2   │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0                │
│ 3   │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0                  │
│ 4   │ pread64 t.db off=24 len=16 = 16                                        │
│ 5   │ fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1 = 0                  │
│ 6   │ pread64 t.db off=4096 len=4096 = 4096                                  │
│ 7   │ open t.db-journal O_RDWR|O_CREAT|O_LARGEFILE|O_NOFOLLOW|O_CLOEXEC = fd │
│ 8   │ fcntl t.db-journal F_SETFD = 0                                         │
│ 9   │ pwrite64 t.db-journal off=0 len=512 = 512                              │
│ 10  │ pwrite64 t.db-journal off=512 len=4 = 4                                │
│ 11  │ pwrite64 t.db-journal off=516 len=4096 = 4096                          │
│ 12  │ pwrite64 t.db-journal off=4612 len=4 = 4                               │
│ 13  │ fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1 = 0                  │
│ 14  │ fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 = 0                │
│ 15  │ pwrite64 t.db-journal off=4616 len=4 = 4                               │
│ 16  │ pwrite64 t.db-journal off=4620 len=4096 = 4096                         │
│ 17  │ pwrite64 t.db-journal off=8716 len=4 = 4                               │
│ 18  │ pread64 t.db-journal off=9216 len=8 = 0                                │
│ 19  │ fsync t.db-journal = 0                                                 │
│ 20  │ open . O_RDONLY|O_LARGEFILE|O_CLOEXEC = fd                             │
│ 21  │ fcntl . F_SETFD = 0                                                    │
│ 22  │ fsync . = 0                                                            │
│ 23  │ close . = 0                                                            │
│ 24  │ pwrite64 t.db-journal off=0 len=12 = 12                                │
│ 25  │ fsync t.db-journal = 0                                                 │
│ 26  │ pwrite64 t.db off=0 len=4096 = 4096                                    │
│ 27  │ pwrite64 t.db off=4096 len=4096 = 4096                                 │
│ 28  │ fsync t.db = 0                                                         │
│ 29  │ close t.db-journal = 0                                                 │
│ 30  │ unlink t.db-journal = 0                                                │
│ 31  │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0                │
│ 32  │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2 = 0                  │
│ 33  │ fcntl t.db F_SETLK F_UNLCK start=0 len=0 = 0                           │
└─────┴────────────────────────────────────────────────────────────────────────┘

B3. Where the bytes land. Each pwrite64 on t.db is one whole page at
offset (page - 1) * 4096; the pages written are the pages that changed:
┌────────────────────────────────────────┬─────────────┬──────┬────────┐
│                  call                  │ file_offset │ page │ length │
├────────────────────────────────────────┼─────────────┼──────┼────────┤
│ pwrite64 t.db off=0 len=4096 = 4096    │ 0           │ 1    │ 4096   │
│ pwrite64 t.db off=4096 len=4096 = 4096 │ 4096        │ 2    │ 4096   │
└────────────────────────────────────────┴─────────────┴──────┴────────┘

B4. The C CLI runs the same INSERT under strace. Its syscalls, normalized
(open/openat as open, fsync/fdatasync as sync); read and write lengths are
kept, so a short transfer would show:
┌─────┬─────────────────────────────────────────────────────┐
│ seq │                        norm                         │
├─────┼─────────────────────────────────────────────────────┤
│ 1   │ fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1   │
│ 2   │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 │
│ 3   │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1   │
│ 4   │ pread64 t.db off=24 len=16                          │
│ 5   │ fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1   │
│ 6   │ pread64 t.db off=4096 len=4096                      │
│ 7   │ open t.db-journal                                   │
│ 8   │ pwrite64 t.db-journal off=0 len=512                 │
│ 9   │ pwrite64 t.db-journal off=512 len=4                 │
│ 10  │ pwrite64 t.db-journal off=516 len=4096              │
│ 11  │ pwrite64 t.db-journal off=4612 len=4                │
│ 12  │ fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1   │
│ 13  │ fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 │
│ 14  │ pwrite64 t.db-journal off=4616 len=4                │
│ 15  │ pwrite64 t.db-journal off=4620 len=4096             │
│ 16  │ pwrite64 t.db-journal off=8716 len=4                │
│ 17  │ pread64 t.db-journal off=9216 len=8 = 0             │
│ 18  │ sync t.db-journal                                   │
│ 19  │ open .                                              │
│ 20  │ sync .                                              │
│ 21  │ close .                                             │
│ 22  │ pwrite64 t.db-journal off=0 len=12                  │
│ 23  │ sync t.db-journal                                   │
│ 24  │ pwrite64 t.db off=0 len=4096                        │
│ 25  │ pwrite64 t.db off=4096 len=4096                     │
│ 26  │ sync t.db                                           │
│ 27  │ close t.db-journal                                  │
│ 28  │ unlink t.db-journal                                 │
│ 29  │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 │
│ 30  │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2   │
│ 31  │ fcntl t.db F_SETLK F_UNLCK start=0 len=0            │
└─────┴─────────────────────────────────────────────────────┘

B5. The twin check: the Go driver and the CLI compared category by category.
Writes and syncs must be identical, and so must every call apart from the
fcntl(F_SETFD) that the libc of modernc adds after each O_CLOEXEC open:
┌──────────────────────────────────────┬──────────┬───────────┬───────────┐
│               category               │ go_calls │ cli_calls │ identical │
├──────────────────────────────────────┼──────────┼───────────┼───────────┤
│ writes (file, offset, length, order) │ 10       │ 10        │ true      │
│ syncs (file, order)                  │ 4        │ 4         │ true      │
│ every call except fcntl F_SETFD      │ 31       │ 31        │ true      │
│ fcntl F_SETFD only                   │ 2        │ 0         │           │
└──────────────────────────────────────┴──────────┴───────────┴───────────┘

The F_SETFD calls only the Go driver makes:
┌─────┬────────────────────────────────┐
│ seq │              call              │
├─────┼────────────────────────────────┤
│ 8   │ fcntl t.db-journal F_SETFD = 0 │
│ 21  │ fcntl . F_SETFD = 0            │
└─────┴────────────────────────────────┘

B6. The database file after each run, as a hash. The Go driver and the CLI
must leave the same bytes, and the INSERT split into three statements must
leave the same bytes as the autocommit INSERT:
┌────────────┬──────────────────┐
│    what    │      sha256      │
├────────────┼──────────────────┤
│ base       │ 0aec0e95a3f65e0c │
│ go_insert  │ 47bae5ed0c8207f2 │
│ cli_insert │ 47bae5ed0c8207f2 │
│ cli_split  │ 47bae5ed0c8207f2 │
└────────────┴──────────────────┘

B7. The measured call path. The CLI runs BEGIN IMMEDIATE; INSERT; COMMIT with
a marker file opened between the statements, so each VFS call and syscall is
assigned to the statement that caused it. SYS lines indented under a VFS line
are the syscalls that VFS call made. xRandomness calls are left out (info.sql).
┌───────┬───────────────────────────────────────┬─────────────────────────────────────────────────┐
│ phase │               statement               │                     opcode                      │
├───────┼───────────────────────────────────────┼─────────────────────────────────────────────────┤
│ 1     │ BEGIN IMMEDIATE                       │ OP_Transaction                                  │
│ 2     │ INSERT INTO t(id, v) VALUES(2, 'two') │ OP_Insert                                       │
│ 3     │ COMMIT                                │ OP_AutoCommit (OP_Halt in an autocommit INSERT) │
└───────┴───────────────────────────────────────┴─────────────────────────────────────────────────┘

The C functions behind each statement, outermost first:
┌───────┬───┬───────────────────────────────────────────┐
│ phase │ n │                c_function                 │
├───────┼───┼───────────────────────────────────────────┤
│ 1     │ 1 │ sqlite3BtreeBeginTrans                    │
│ 1     │ 2 │ sqlite3PagerSharedLock, sqlite3PagerBegin │
│ 2     │ 1 │ sqlite3BtreeInsert                        │
│ 2     │ 2 │ sqlite3PagerWrite                         │
│ 3     │ 1 │ sqlite3VdbeHalt                           │
│ 3     │ 2 │ vdbeCommit                                │
│ 3     │ 3 │ sqlite3BtreeCommitPhaseOne/PhaseTwo       │
│ 3     │ 4 │ sqlite3PagerCommitPhaseOne/PhaseTwo       │
└───────┴───┴───────────────────────────────────────────┘

The calls of each statement, in order:
┌───────┬──────┬──────────────────────────────────────────────────────────────┐
│ phase │ kind │                             call                             │
├───────┼──────┼──────────────────────────────────────────────────────────────┤
│ 1     │ VFS  │ xLock(t.db,SHARED) -> SQLITE_OK                              │
│ 1     │ SYS  │     fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1        │
│ 1     │ SYS  │     fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510      │
│ 1     │ SYS  │     fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1        │
│ 1     │ VFS  │ xAccess("t.db-journal",0) -> SQLITE_OK, out=0                │
│ 1     │ VFS  │ xRead(t.db,n=16,ofst=24) -> SQLITE_OK                        │
│ 1     │ SYS  │     pread64 t.db off=24 len=16                               │
│ 1     │ VFS  │ xAccess("t.db-wal",0) -> SQLITE_OK, out=0                    │
│ 1     │ VFS  │ xFileSize(t.db) -> SQLITE_OK, size=8192                      │
│ 1     │ VFS  │ xLock(t.db,RESERVED) -> SQLITE_OK                            │
│ 1     │ SYS  │     fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1        │
│ 2     │ VFS  │ xRead(t.db,n=4096,ofst=4096) -> SQLITE_OK                    │
│ 2     │ SYS  │     pread64 t.db off=4096 len=4096                           │
│ 2     │ VFS  │ xFileControl(t.db,HAS_MOVED) -> SQLITE_OK, 0                 │
│ 2     │ SYS  │ open t.db-journal                                            │
│ 2     │ VFS  │ xOpen(t.db-journal,flags=0x806) -> SQLITE_OK                 │
│ 2     │ VFS  │ xDeviceCharacteristics(t.db) -> 0x00009000                   │
│ 2     │ VFS  │ xWrite(t.db-journal,n=512,ofst=0) -> SQLITE_OK               │
│ 2     │ SYS  │     pwrite64 t.db-journal off=0 len=512                      │
│ 2     │ VFS  │ xWrite(t.db-journal,n=4,ofst=512) -> SQLITE_OK               │
│ 2     │ SYS  │     pwrite64 t.db-journal off=512 len=4                      │
│ 2     │ VFS  │ xWrite(t.db-journal,n=4096,ofst=516) -> SQLITE_OK            │
│ 2     │ SYS  │     pwrite64 t.db-journal off=516 len=4096                   │
│ 2     │ VFS  │ xWrite(t.db-journal,n=4,ofst=4612) -> SQLITE_OK              │
│ 2     │ SYS  │     pwrite64 t.db-journal off=4612 len=4                     │
│ 3     │ VFS  │ xLock(t.db,EXCLUSIVE) -> SQLITE_OK                           │
│ 3     │ SYS  │     fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1        │
│ 3     │ SYS  │     fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510      │
│ 3     │ VFS  │ xWrite(t.db-journal,n=4,ofst=4616) -> SQLITE_OK              │
│ 3     │ SYS  │     pwrite64 t.db-journal off=4616 len=4                     │
│ 3     │ VFS  │ xWrite(t.db-journal,n=4096,ofst=4620) -> SQLITE_OK           │
│ 3     │ SYS  │     pwrite64 t.db-journal off=4620 len=4096                  │
│ 3     │ VFS  │ xWrite(t.db-journal,n=4,ofst=8716) -> SQLITE_OK              │
│ 3     │ SYS  │     pwrite64 t.db-journal off=8716 len=4                     │
│ 3     │ VFS  │ xDeviceCharacteristics(t.db) -> 0x00009000                   │
│ 3     │ VFS  │ xRead(t.db-journal,n=8,ofst=9216) -> SQLITE_IOERR_SHORT_READ │
│ 3     │ SYS  │     pread64 t.db-journal off=9216 len=8 = 0                  │
│ 3     │ VFS  │ xSync(t.db-journal,FULL) -> 0                                │
│ 3     │ SYS  │     sync t.db-journal                                        │
│ 3     │ SYS  │     open .                                                   │
│ 3     │ SYS  │     sync .                                                   │
│ 3     │ SYS  │     close .                                                  │
│ 3     │ VFS  │ xWrite(t.db-journal,n=12,ofst=0) -> SQLITE_OK                │
│ 3     │ SYS  │     pwrite64 t.db-journal off=0 len=12                       │
│ 3     │ VFS  │ xSync(t.db-journal,FULL) -> 0                                │
│ 3     │ SYS  │     sync t.db-journal                                        │
│ 3     │ VFS  │ xWrite(t.db,n=4096,ofst=0) -> SQLITE_OK                      │
│ 3     │ SYS  │     pwrite64 t.db off=0 len=4096                             │
│ 3     │ VFS  │ xWrite(t.db,n=4096,ofst=4096) -> SQLITE_OK                   │
│ 3     │ SYS  │     pwrite64 t.db off=4096 len=4096                          │
│ 3     │ VFS  │ xFileControl(t.db,SYNC) -> SQLITE_NOTFOUND                   │
│ 3     │ VFS  │ xSync(t.db,FULL) -> 0                                        │
│ 3     │ SYS  │     sync t.db                                                │
│ 3     │ VFS  │ xClose(t.db-journal) -> SQLITE_OK                            │
│ 3     │ SYS  │     close t.db-journal                                       │
│ 3     │ VFS  │ xDelete("t.db-journal",0) -> SQLITE_OK                       │
│ 3     │ SYS  │     unlink t.db-journal                                      │
│ 3     │ VFS  │ xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND        │
│ 3     │ VFS  │ xUnlock(t.db,SHARED) -> SQLITE_OK                            │
│ 3     │ SYS  │     fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510      │
│ 3     │ SYS  │     fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2        │
│ 3     │ VFS  │ xDeviceCharacteristics(t.db) -> 0x00009000                   │
│ 3     │ VFS  │ xUnlock(t.db,NONE) -> SQLITE_OK                              │
│ 3     │ SYS  │     fcntl t.db F_SETLK F_UNLCK start=0 len=0                 │
└───────┴──────┴──────────────────────────────────────────────────────────────┘

The three phases together make exactly the syscalls of the autocommit INSERT:
┌───────────────┐
│ same_syscalls │
├───────────────┤
│ true          │
└───────────────┘

── C. Write-ahead log (journal_mode=wal, the default) ──────────────────

C1. The base database, and the pages the INSERT changed (sha256 prefix of
each page, read through sqlite_dbpage before and after).
Page 1 is not written, so its change counter stays.
┌──────┬──────────────┬──────────────────┬──────┬──────────────────┬──────────────────┐
│ mode │ pages_before │   file_sha256    │ page │  before_sha256   │   after_sha256   │
├──────┼──────────────┼──────────────────┼──────┼──────────────────┼──────────────────┤
│ wal  │ 2            │ 5de4f3aefd26bcea │ 2    │ 5d68aacbd116afe8 │ b21207c95d70a25b │
└──────┴──────────────┴──────────────────┴──────┴──────────────────┴──────────────────┘

C2. The file syscalls the Go driver made for the INSERT, in order
(open/openat and unlink/unlinkat are written open and unlink; "= fd" stands
for the descriptor number, which varies):
┌─────┬────────────────────────────────────────────────────┐
│ seq │                        call                        │
├─────┼────────────────────────────────────────────────────┤
│ 1   │ fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1 = 0 │
│ 2   │ fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 = 0 │
│ 3   │ pread64 t.db off=4096 len=4096 = 4096              │
│ 4   │ fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1 = 0 │
│ 5   │ fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 = 0 │
│ 6   │ fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0 │
│ 7   │ fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1 = 0 │
│ 8   │ pwrite64 t.db-wal off=0 len=32 = 32                │
│ 9   │ fsync t.db-wal = 0                                 │
│ 10  │ open . O_RDONLY|O_LARGEFILE|O_CLOEXEC = fd         │
│ 11  │ fcntl . F_SETFD = 0                                │
│ 12  │ fsync . = 0                                        │
│ 13  │ close . = 0                                        │
│ 14  │ pwrite64 t.db-wal off=32 len=24 = 24               │
│ 15  │ pwrite64 t.db-wal off=56 len=4096 = 4096           │
│ 16  │ fsync t.db-wal = 0                                 │
│ 17  │ fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 = 0 │
│ 18  │ fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0 │
└─────┴────────────────────────────────────────────────────┘

C3. Where the bytes land. The commit writes nothing to t.db itself; it
appends one frame per changed page to t.db-wal after a 32-byte WAL header.
The frame table is read from the -wal file by the traced child; offsets are
32 + (frame - 1) * 4120 for the 24-byte frame header, then +24 for the page:
┌─────────────────┐
│ pwrites_on_t_db │
├─────────────────┤
│ 0               │
└─────────────────┘
┌───────┬──────┬─────────────┬───────────┬───────────────┐
│ frame │ page │ commit_size │ header_at │ page_image_at │
├───────┼──────┼─────────────┼───────────┼───────────────┤
│ 1     │ 2    │ 2           │ 32        │ 56            │
└───────┴──────┴─────────────┴───────────┴───────────────┘

C4. The C CLI runs the same INSERT under strace. Its syscalls, normalized
(open/openat as open, fsync/fdatasync as sync); read and write lengths are
kept, so a short transfer would show:
┌─────┬────────────────────────────────────────────────┐
│ seq │                      norm                      │
├─────┼────────────────────────────────────────────────┤
│ 1   │ fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1 │
│ 2   │ fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 │
│ 3   │ pread64 t.db off=4096 len=4096                 │
│ 4   │ fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1 │
│ 5   │ fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 │
│ 6   │ fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 │
│ 7   │ fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1 │
│ 8   │ pwrite64 t.db-wal off=0 len=32                 │
│ 9   │ sync t.db-wal                                  │
│ 10  │ open .                                         │
│ 11  │ sync .                                         │
│ 12  │ close .                                        │
│ 13  │ pwrite64 t.db-wal off=32 len=24                │
│ 14  │ pwrite64 t.db-wal off=56 len=4096              │
│ 15  │ sync t.db-wal                                  │
│ 16  │ fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 │
│ 17  │ fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 │
└─────┴────────────────────────────────────────────────┘

C5. The twin check: the Go driver and the CLI compared category by category.
Writes and syncs must be identical, and so must every call apart from the
fcntl(F_SETFD) that the libc of modernc adds after each O_CLOEXEC open:
┌──────────────────────────────────────┬──────────┬───────────┬───────────┐
│               category               │ go_calls │ cli_calls │ identical │
├──────────────────────────────────────┼──────────┼───────────┼───────────┤
│ writes (file, offset, length, order) │ 3        │ 3         │ true      │
│ syncs (file, order)                  │ 3        │ 3         │ true      │
│ every call except fcntl F_SETFD      │ 17       │ 17        │ true      │
│ fcntl F_SETFD only                   │ 1        │ 0         │           │
└──────────────────────────────────────┴──────────┴───────────┴───────────┘

The F_SETFD calls only the Go driver makes:
┌─────┬─────────────────────┐
│ seq │        call         │
├─────┼─────────────────────┤
│ 11  │ fcntl . F_SETFD = 0 │
└─────┴─────────────────────┘

C6. The database file after each run, as a hash. The Go driver and the CLI
must leave the same bytes, and the INSERT split into three statements must
leave the same bytes as the autocommit INSERT:
┌────────────┬──────────────────┐
│    what    │      sha256      │
├────────────┼──────────────────┤
│ base       │ 5de4f3aefd26bcea │
│ go_insert  │ 1afce8154bf3124c │
│ cli_insert │ 1afce8154bf3124c │
│ cli_split  │ 1afce8154bf3124c │
└────────────┴──────────────────┘

C7. The measured call path. The CLI runs BEGIN IMMEDIATE; INSERT; COMMIT with
a marker file opened between the statements, so each VFS call and syscall is
assigned to the statement that caused it. SYS lines indented under a VFS line
are the syscalls that VFS call made. xRandomness calls are left out (info.sql).
┌───────┬───────────────────────────────────────┬─────────────────────────────────────────────────┐
│ phase │               statement               │                     opcode                      │
├───────┼───────────────────────────────────────┼─────────────────────────────────────────────────┤
│ 1     │ BEGIN IMMEDIATE                       │ OP_Transaction                                  │
│ 2     │ INSERT INTO t(id, v) VALUES(2, 'two') │ OP_Insert                                       │
│ 3     │ COMMIT                                │ OP_AutoCommit (OP_Halt in an autocommit INSERT) │
└───────┴───────────────────────────────────────┴─────────────────────────────────────────────────┘

The C functions behind each statement, outermost first:
┌───────┬───┬───────────────────────────────────────────┐
│ phase │ n │                c_function                 │
├───────┼───┼───────────────────────────────────────────┤
│ 1     │ 1 │ sqlite3BtreeBeginTrans                    │
│ 1     │ 2 │ sqlite3PagerSharedLock, sqlite3PagerBegin │
│ 2     │ 1 │ sqlite3BtreeInsert                        │
│ 2     │ 2 │ sqlite3PagerWrite                         │
│ 3     │ 1 │ sqlite3VdbeHalt                           │
│ 3     │ 2 │ vdbeCommit                                │
│ 3     │ 3 │ sqlite3BtreeCommitPhaseOne                │
│ 3     │ 4 │ sqlite3PagerCommitPhaseOne                │
│ 3     │ 5 │ pagerWalFrames                            │
│ 3     │ 6 │ sqlite3WalFrames                          │
└───────┴───┴───────────────────────────────────────────┘

The calls of each statement, in order:
┌───────┬──────┬────────────────────────────────────────────────────────────────┐
│ phase │ kind │                              call                              │
├───────┼──────┼────────────────────────────────────────────────────────────────┤
│ 1     │ VFS  │ xShmBarrier(t.db)                                              │
│ 1     │ VFS  │ xShmLock(t.db,ofst=3(READ0),n=1,LOCK|SHARED) -> SQLITE_OK      │
│ 1     │ SYS  │     fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1             │
│ 1     │ VFS  │ xShmBarrier(t.db)                                              │
│ 1     │ VFS  │ xFileSize(t.db) -> SQLITE_OK, size=8192                        │
│ 1     │ VFS  │ xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK   │
│ 1     │ SYS  │     fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1             │
│ 2     │ VFS  │ xRead(t.db,n=4096,ofst=4096) -> SQLITE_OK                      │
│ 2     │ SYS  │     pread64 t.db off=4096 len=4096                             │
│ 3     │ VFS  │ xShmLock(t.db,ofst=3(READ0),n=1,UNLOCK|SHARED) -> SQLITE_OK    │
│ 3     │ SYS  │     fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1             │
│ 3     │ VFS  │ xShmLock(t.db,ofst=4(READ1),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK   │
│ 3     │ SYS  │     fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1             │
│ 3     │ VFS  │ xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK │
│ 3     │ SYS  │     fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1             │
│ 3     │ VFS  │ xShmLock(t.db,ofst=4(READ1),n=1,LOCK|SHARED) -> SQLITE_OK      │
│ 3     │ SYS  │     fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1             │
│ 3     │ VFS  │ xShmBarrier(t.db)                                              │
│ 3     │ VFS  │ xWrite(t.db-wal,n=32,ofst=0) -> SQLITE_OK                      │
│ 3     │ SYS  │     pwrite64 t.db-wal off=0 len=32                             │
│ 3     │ VFS  │ xSync(t.db-wal,FULL) -> 0                                      │
│ 3     │ SYS  │     sync t.db-wal                                              │
│ 3     │ SYS  │     open .                                                     │
│ 3     │ SYS  │     sync .                                                     │
│ 3     │ SYS  │     close .                                                    │
│ 3     │ VFS  │ xWrite(t.db-wal,n=24,ofst=32) -> SQLITE_OK                     │
│ 3     │ SYS  │     pwrite64 t.db-wal off=32 len=24                            │
│ 3     │ VFS  │ xWrite(t.db-wal,n=4096,ofst=56) -> SQLITE_OK                   │
│ 3     │ SYS  │     pwrite64 t.db-wal off=56 len=4096                          │
│ 3     │ VFS  │ xSync(t.db-wal,FULL) -> 0                                      │
│ 3     │ SYS  │     sync t.db-wal                                              │
│ 3     │ VFS  │ xShmBarrier(t.db)                                              │
│ 3     │ VFS  │ xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK │
│ 3     │ SYS  │     fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1             │
│ 3     │ VFS  │ xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND          │
│ 3     │ VFS  │ xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|SHARED) -> SQLITE_OK    │
│ 3     │ SYS  │     fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1             │
└───────┴──────┴────────────────────────────────────────────────────────────────┘

The three phases together make exactly the syscalls of the autocommit INSERT:
┌───────────────┐
│ same_syscalls │
├───────────────┤
│ true          │
└───────────────┘

── D. A limit of the twin check ────────────────────────────────────────

The twin check assumes both builds write the same bytes, which holds only
if their compile options agree. The driver is compiled with ENABLE_STAT4
and this CLI is not. Two copies of a 100-row indexed table start byte for
byte identical; the Go driver and the CLI each run ANALYZE on one copy:
┌───────────────┬────────┬──────────────────┬────────────────┬───────────────────────────┐
│     stage     │  file  │      sha256      │ stat4_compiled │        stat_tables        │
├───────────────┼────────┼──────────────────┼────────────────┼───────────────────────────┤
│ base          │ cli.db │ cd808b1713871cd6 │                │                           │
│ base          │ go.db  │ cd808b1713871cd6 │                │                           │
│ after ANALYZE │ cli.db │ 490d8405dea7402c │ 0              │ sqlite_stat1              │
│ after ANALYZE │ go.db  │ 2f6ce76122bda45c │ 1              │ sqlite_stat1 sqlite_stat4 │
└───────────────┴────────┴──────────────────┴────────────────┴───────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

B  ✓  the INSERT changes page 1 (the change counter in the header) and page 2 (table t's leaf): [1 2]
B  ✓  rollback mode: each pwrite64 on t.db is one whole page at offset (page-1)*4096, and the pages written are exactly the changed pages [1 2]
B  ✓  twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order
B  ✓  apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)
B  ✓  the database file after the INSERT is byte-identical from both builds (sha256 47bae5ed0c8207f2)
B  ✓  the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)
B  ✓  BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT
C  ✓  the INSERT changes only page 2 (table t's leaf); page 1 is not written, so its change counter stays: [2]
C  ✓  WAL mode: the commit writes nothing to t.db itself (database-file pwrite64 calls during the INSERT: 0)
C  ✓  WAL mode: the commit writes the 32-byte WAL header at offset 0 of the new t.db-wal, then one frame per changed page [2] (frame header 24 bytes, then the page at offset 32 + (frame-1)*4120 + 24), and nothing else; the last frame carries the commit size
C  ✓  twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order
C  ✓  apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)
C  ✓  the database file after the INSERT is byte-identical from both builds (sha256 1afce8154bf3124c)
C  ✓  the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)
C  ✓  BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT
D  ✓  both copies start byte-identical
D  ✓  twin check limit: the driver is compiled with ENABLE_STAT4 and this CLI is not, so the same ANALYZE writes a sqlite_stat4 table only in the driver and the two files differ

17 of 17 claims hold

Experiment verdict: ONE INSERT OK
```

**Reading the evidence:**

1. **The `EXPLAIN` bytecode listing (A1).** Each row is one VDBE instruction: `addr` (instruction index), `opcode`, and operands `p1`–`p5`. Execution starts at `0 Init 0 14`, jumps to `14 Transaction 0 1 1 0 1` (write transaction `p2=1` on `p1=0` `main`, checking schema cookie `p3=1` because `p5=1`), and jumps to `1` via `15 Goto 0 1`:
   - `1 OpenWrite 0 2 0 2` opens write cursor `0` on root page `2` (table `t`, 2 columns).
   - `2 SoftNull 2`, `3 String8 0 3 0 two`, and `4 Integer 2 1` load `r2=NULL` (the `INTEGER PRIMARY KEY` column lives in the rowid, not inside the record payload), `r3='two'`, and rowid `r1=2`.
   - `5 NotNull 1 7` skips `6 NewRowid` because rowid `2` is explicit.
   - `7 MustBeInt 1` checks the rowid type; `8 HaltIfNull 1299 2 3 t.v 1` enforces `v TEXT NOT NULL`; `9 NotExists 0 11 1` checks that rowid `2` is unused (else `10 Halt 1555` raises a primary-key constraint error).
   - `11 MakeRecord 2 2 4 DB` packs `r2..r3` into a binary SQLite record in `r4`.
   - `12 Insert 0 4 1 t 49` inserts `r4` with rowid `r1` through cursor `0`. Flag `p5=49` is `1 + 16 + 32` (`OPFLAG_NCHANGE | OPFLAG_USESEEKRESULT | OPFLAG_LASTROWID`): reuse the seek position from `NotExists`, increment the change count, and update `last_insert_rowid`.
   - `13 Halt` enters `sqlite3VdbeHalt`, which commits the autocommit transaction.

2. **Rollback mode (`journal_mode=delete`, section B).**
   - **Changed pages (B1):** `[1 2]`. Page 2 changed because row 2 was inserted into table `t`'s leaf page. Page 1 changed because a rollback-mode commit increments the 4-byte file change counter (bytes 24–27) and version-valid-for field (bytes 92–95).
   - **Database writes (B2, B3):** `pwrite64 t.db off=0 len=4096` equals `(page 1 - 1) * 4096`, and `off=4096 len=4096` equals `(page 2 - 1) * 4096`. Every write to `t.db` is one whole page, matching `[1 2]` exactly.
   - **Syncs (B2):** 4 syncs in order: `fsync t.db-journal`, `fsync .` (makes the new journal directory entry durable), `fsync t.db-journal` (after writing the 12-byte journal header with the page count), and `fsync t.db`.
   - **Errors and short reads (B2 and B4):** `normalized` keeps errno on failed calls and byte counts on short transfers, so SQLite's probe past end-of-file prints `pread64 t.db-journal off=9216 len=8 = 0` in both builds.
   - **Twin check (B5–B6):** 33 Go syscalls vs 31 CLI syscalls. Writes (10), syncs (4), all 31 non-`F_SETFD` calls, and the closed at-rest database file (`47bae5ed0c8207f2`, distinct from base `0aec0e95a3f65e0c`) are identical. The sole difference (B5, listed below its table) is two `fcntl(..., F_SETFD)` calls from `modernc.org/libc v1.77.1`: its musl-derived `open()` calls `fcntl(fd, F_SETFD, FD_CLOEXEC)` after `O_CLOEXEC`, whereas `os_unix.c`'s `robust_open` ([`src/os_unix.c:867-869`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L867-L869)) skips `F_SETFD` when `O_CLOEXEC` is defined. `./configure`-built CLIs also call `fdatasync` where the Go driver and system CLI call `fsync`, and `openat` vs `open` varies by libc; the experiment normalizes those pairs and records the raw sync syscall in a run-dependent `info` table that `course run` prints.

3. **WAL mode (`journal_mode=wal`, the platform default, section C).**
   - **Changed pages (C1):** `[2]` only. Page 1 is untouched during this `INSERT`, so its change counter does not move.
   - **WAL writes vs database writes (C2–C3):** Zero `pwrite64` calls hit `t.db` (C3). Because closing the base database (`5de4f3aefd26bcea`) checkpointed and removed its WAL, this `INSERT` creates a fresh WAL: it writes the 32-byte WAL header (`t.db-wal off=0 len=32`), syncs `t.db-wal` and `.`, appends frame 1's 24-byte header (`off=32 len=24`) and page 2's 4,096-byte image (`off=56 len=4096`), and syncs `t.db-wal`. Reading `t.db-wal` before `db.Close()` confirms frame 1 holds page 2 with commit size 2 (C3's frame table), and a check verifies the exact write list with nothing extra.
   - **Syncs (C2):** 3 syncs total (`t.db-wal`, `.`, `t.db-wal`). The first two pay for creating the fresh WAL header and directory entry; only the third is the per-commit sync at `synchronous=FULL`.
   - **Twin check (C5–C6):** 18 Go syscalls vs 17 CLI syscalls (the single extra call is `fcntl . F_SETFD`, listed in C5). Writes (3), syncs (3), all 17 other syscalls, and the closed at-rest database file (`1afce8154bf3124c`) are identical.
   - **Marker files and `xRandomness`:** Both processes bracket statements by opening empty marker files (`marker-1`, `marker-2`, and so on) because CLI stdout is buffered; an `open`/`openat` syscall marks the exact statement boundary in `strace`. Some CLI builds call `VFS xRandomness` here on their first PRNG use (to seed a journal nonce or WAL salts); the experiment records that call in a run-dependent `info` table (printed by `course run`, not stored), so the stored evidence matches across both the installed CLI and the `build-tools` CLI.

4. **A limit of the twin check: `ANALYZE` and `ENABLE_STAT4` (section D).** Starting from two byte-identical copies (`cd808b1713871cd6`) of a 100-row table `s` with index `s_by_k`, `ANALYZE` in the Go driver (`ENABLE_STAT4 = 1`) creates `[sqlite_stat1 sqlite_stat4]` (`2f6ce76122bda45c`), while the C CLI (`ENABLE_STAT4 = 0`) creates only `[sqlite_stat1]` (`490d8405dea7402c`). Twin parity holds only for statements whose behavior does not depend on compile options differing between the two builds.

---

## 4. Timeline

The third run per mode in Experiment 1.2 splits the autocommit `INSERT` into `BEGIN IMMEDIATE;`, `INSERT INTO t(id, v) VALUES(2, 'two');`, and `COMMIT;` under the C CLI's `-vfstrace` plus `strace`. Sections B6–B7 and C6–C7 of `evidence/1.2-one-insert.txt` prove that the three phases concatenated execute the exact same syscalls and leave the exact same database bytes as the single autocommit `INSERT`. That ties each VFS call and kernel syscall by measurement to the opcode that caused it: `BEGIN IMMEDIATE` isolates `OP_Transaction`, `INSERT` isolates `OP_Insert`, and `COMMIT` isolates `OP_AutoCommit` → `sqlite3VdbeHalt` → `vdbeCommit` (the same path `OP_Halt` takes in an autocommit `INSERT`).

### 4.1 Rollback mode (`journal_mode=delete`)

POSIX byte-range locks in rollback mode sit on `t.db` at the 1 GiB lock-byte region: byte `1073741824` (`0x40000000`, `PENDING`), byte `1073741825` (`RESERVED`), and bytes `1073741826..1073742335` (`len=510`, `SHARED`).

| Phase (opcode) | Layer path | VFS call (`-vfstrace`) | Kernel syscall(s) (`strace`) |
| --- | --- | --- | --- |
| `BEGIN IMMEDIATE` (`OP_Transaction`) | `sqlite3BtreeBeginTrans` → `sqlite3PagerSharedLock` | `xLock(t.db,SHARED)` | `fcntl t.db F_RDLCK 1073741824 len=1`, `F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=1` |
| 〃 | Check for hot journal and verify cache against page 1 bytes 24–39 | `xAccess("t.db-journal")=0`, `xRead(t.db,n=16,ofst=24)`, `xAccess("t.db-wal")=0`, `xFileSize(t.db)=8192` | `pread64 t.db off=24 len=16` |
| 〃 | `sqlite3PagerBegin` (acquire writer intent) | `xLock(t.db,RESERVED)` | `fcntl t.db F_WRLCK 1073741825 len=1` |
| `INSERT` (`OP_Insert`) | `sqlite3BtreeInsert` → read page 2 → `sqlite3PagerWrite` | `xRead(t.db,n=4096,ofst=4096)` | `pread64 t.db off=4096 len=4096` |
| 〃 | `pager_open_journal` creates journal and writes 512-byte header | `xFileControl(HAS_MOVED)`, `xOpen(t.db-journal)`, `xDeviceCharacteristics`, `xWrite(t.db-journal,n=512,ofst=0)` | `open t.db-journal`, `pwrite64 t.db-journal off=0 len=512` |
| 〃 | Journal page 2's original image (4-byte page number + 4,096-byte page + 4-byte checksum) | `xWrite` ×3 (`ofst=512, 516, 4612`) | `pwrite64 t.db-journal off=512 len=4`, `off=516 len=4096`, `off=4612 len=4` |
| `COMMIT` (`OP_AutoCommit` / `OP_Halt`) | `sqlite3VdbeHalt` → `vdbeCommit` → `sqlite3PagerExclusiveLock` | `xLock(t.db,EXCLUSIVE)` | `fcntl t.db F_WRLCK 1073741824 len=1`, `F_WRLCK 1073741826 len=510` |
| 〃 | `sqlite3PagerCommitPhaseOne` → `pager_incr_changecounter` journals page 1 | `xWrite` ×3 (`ofst=4616, 4620, 8716`) | `pwrite64 t.db-journal off=4616 len=4`, `off=4620 len=4096`, `off=8716 len=4` |
| 〃 | `syncJournal`: probe for stale next header, sync journal records and directory | `xRead(t.db-journal,n=8,ofst=9216)` → `SHORT_READ`, `xSync(t.db-journal,FULL)` | `pread64 t.db-journal off=9216 len=8 = 0`, `sync t.db-journal`, `open .`, `sync .`, `close .` |
| 〃 | `syncJournal`: write page count into journal header and sync header | `xWrite(t.db-journal,n=12,ofst=0)`, `xSync(t.db-journal,FULL)` | `pwrite64 t.db-journal off=0 len=12`, `sync t.db-journal` |
| 〃 | `pager_write_pagelist` writes modified pages 1 and 2 to `t.db`, then `sqlite3PagerSync` | `xWrite(t.db,4096,0)`, `xWrite(t.db,4096,4096)`, `xFileControl(SYNC)`, `xSync(t.db,FULL)` | `pwrite64 t.db off=0 len=4096`, `off=4096 len=4096`, `sync t.db` |
| 〃 | `sqlite3BtreeCommitPhaseTwo` → `pager_end_transaction` (**commit point**) | `xClose(t.db-journal)`, `xDelete("t.db-journal")` | `close t.db-journal`, `unlink t.db-journal` |
| 〃 | Drop exclusive lock back to `SHARED`, then `NONE` | `xFileControl(COMMIT_PHASETWO)`, `xUnlock(SHARED)`, `xUnlock(NONE)` | `fcntl t.db F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=2`, `F_UNLCK 0 len=0` |

In rollback mode, `unlink t.db-journal` is the commit point: before it, a crash leaves a synced journal that rolls both pages back on next open; after it, the new pages in `t.db` stand.

### 4.2 WAL mode (`journal_mode=wal`, the platform default)

POSIX byte-range locks in WAL mode sit on `t.db-shm`: byte `120` (`WRITE`), `121` (`CKPT`), `122` (`RECOVER`), and `123 + i` (`READ i`, five reader slots). `xShmBarrier` is a memory barrier ordering reads and writes to the shared-memory `-shm` mapping; `xFileControl` passes control notifications (such as `SYNC` or `COMMIT_PHASETWO`) from the pager to the VFS.

| Phase (opcode) | Layer path | VFS call (`-vfstrace`) | Kernel syscall(s) (`strace`) |
| --- | --- | --- | --- |
| `BEGIN IMMEDIATE` (`OP_Transaction`) | `sqlite3BtreeBeginTrans` → `walTryBeginRead` | `xShmBarrier`, `xShmLock(READ0, LOCK\|SHARED)`, `xShmBarrier`, `xFileSize(t.db)` | `fcntl t.db-shm F_RDLCK start=123 len=1` |
| 〃 | `sqlite3WalBeginWriteTransaction` | `xShmLock(WRITE, LOCK\|EXCLUSIVE)` | `fcntl t.db-shm F_WRLCK start=120 len=1` |
| `INSERT` (`OP_Insert`) | `sqlite3BtreeInsert` → `sqlite3PagerWrite` (no journal in WAL mode) | `xRead(t.db,n=4096,ofst=4096)` | `pread64 t.db off=4096 len=4096` |
| `COMMIT` (`OP_AutoCommit` / `OP_Halt`) | `vdbeCommit` → `pagerWalFrames` → `sqlite3WalFrames` → `walRestartLog` moves reader mark from `READ0` to `READ1` | `xShmLock(READ0, UNLOCK\|SHARED)`, `xShmLock(READ1, LOCK\|EXCLUSIVE)`, `xShmLock(READ1, UNLOCK\|EXCLUSIVE)`, `xShmLock(READ1, LOCK\|SHARED)`, `xShmBarrier` | `fcntl t.db-shm F_UNLCK 123`, `F_WRLCK 124`, `F_UNLCK 124`, `F_RDLCK 124` |
| 〃 | Write and sync the 32-byte WAL header (first write of a new WAL) and sync directory `.` | `xWrite(t.db-wal,n=32,ofst=0)`, `xSync(t.db-wal,FULL)` | `pwrite64 t.db-wal off=0 len=32`, `sync t.db-wal`, `open .`, `sync .`, `close .` |
| 〃 | Append frame 1 (24-byte header + page 2 image) and sync (**commit point**) | `xWrite(t.db-wal,n=24,ofst=32)`, `xWrite(t.db-wal,n=4096,ofst=56)`, `xSync(t.db-wal,FULL)` | `pwrite64 t.db-wal off=32 len=24`, `off=56 len=4096`, `sync t.db-wal` |
| 〃 | Publish to wal-index and release `WRITE` and `READ1` locks | `xShmBarrier`, `xShmLock(WRITE, UNLOCK\|EXCLUSIVE)`, `xFileControl(COMMIT_PHASETWO)`, `xShmLock(READ1, UNLOCK\|SHARED)` | `fcntl t.db-shm F_UNLCK start=120 len=1`, `F_UNLCK start=124 len=1` |

Two structural differences stand out: `OP_Insert` performs zero writes in WAL mode (only reading page 2 into the cache), and `COMMIT` never writes to `t.db` at all. All durability moves onto `t.db-wal`.

---

## 5. Lab

Every later chapter verifies snapshots, WAL replays, crash recoveries, or fleet rebuilds against a known ground truth: a realistic workload whose closed database file has one exact SHA-256 hash after every numbered step *k*. That is **Rebuild Ladder rung R0 (Recipe)**.

This chapter introduces the shared package `internal/recipe`, inlined in full below. Its unit tests live in [`internal/recipe/recipe_test.go`](../internal/recipe/recipe_test.go) and [`internal/recipe/repair_test.go`](../internal/recipe/repair_test.go) and run with `go test ./internal/recipe/`.

### 5.1 The `internal/recipe` package

Three files split generation, execution, and verification:

- **Plan generation (`recipe.go`).** `Recipe` carries `{Seed, Steps, JournalMode}` (`Default()` uses seed `1`, `30` steps, and `wal`). `Plan()` returns a slice of `Step` values, each holding parameterized `Statement{SQL, Args}` entries and `StepKind` tags summarized by `StepCounts`.
  - **Step 0** creates three tables (`accounts`, `documents`, `events`) with explicit `id INTEGER PRIMARY KEY` columns and sets `PRAGMA user_version = 0`.
  - **Steps 1–30** each seed a fresh `math/rand/v2` PCG generator from `(seed, k)` so step *k* depends only on the seed and its index, never on total step count. Each step runs inserts across all three tables, balance updates, occasional document body replacements, periodic deletes (every 3rd step for `events`, every 4th step for `documents`), creates index `events_by_account` at step `5`, and ends with `PRAGMA user_version = k` inside the same transaction.
  - **Overflow pages.** Document bodies range from `MinBodyBytes = 2000` to `MaxBodyBytes = 16000` bytes. On a 4,096-byte page, a table leaf row spills onto overflow pages above 4,061 bytes of payload, so the workload produces both inline rows and multi-page overflow chains.
  - **Explicit settings.** `SQLiteOptions()` sets `secure_delete=ON` and `SingleConnection=true` on top of `sqlitex.DefaultOptions()` (`page_size=4096`, `auto_vacuum=NONE`, `encoding=UTF-8`, `application_id=1397836120`, `synchronous=FULL`, `foreign_keys=ON`, `cache_size=-2000`, `wal_autocheckpoint=1000`, `busy_timeout=5000ms`).
- **Session execution (`apply.go`).** `Build` and `BuildEach` open and close a fresh connection for every step (`ApplyInSession`) and verify `CheckAtRest` (no `-wal`, `-shm`, or `-journal` remaining). Section 6 proves why: in WAL mode, [`pagerWalFrames` (`src/pager.c:3264`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3264) calls [`pager_write_changecounter` (`src/pager.c:3130-3143`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3130-L3143) only when a commit dirties page 1 (`pList->pgno == 1`), writing `dbFileVers + 1` where `dbFileVers` is refreshed only when page 1 is read from disk ([`src/pager.c:3109-3110`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3109-L3110)). Because every recipe step executes `PRAGMA user_version = k` (dirtying page 1), one session per step advances the counter deterministically by one per step.
- **Hashes, cache, and lookup (`hashes.go`).** `FileHashes` stores both the exact file SHA-256 and a change-counter-masked SHA-256 (zeroing header bytes 24–27 and 92–95). `StepHashes` caches the per-step list under `_work/recipe-cache/` in format v2 (`FormatVersion = 2`) with a trailing SHA-256 checksum over the cache key, SQLite options, every planned statement and bound argument, and all step hashes. Before trusting a cached file, `StepHashes` verifies that checksum and rebuilds step 0 in a temporary directory to compare entry 0; any tampered, corrupt, or stale cache is rebuilt atomically. `MatchStep` returns a `Match{Step, Exact}` naming which step an at-rest database equals and rejects any masked-only match whose bytes 24–27 and 92–95 disagree (`ChangeCountersAgree`).

#### `internal/recipe/recipe.go`

<!-- include: ../internal/recipe/recipe.go -->
```go
// Package recipe generates and applies a seeded, deterministic SQLite workload.
//
// Every step k is one transaction ending in PRAGMA user_version = k. Running
// the same recipe produces byte-identical database files at rest, giving every
// chapter a known ground-truth SHA-256 hash after each step (Rebuild Ladder
// rung R0).
package recipe

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Default recipe parameters and bounds.
const (
	// DefaultSeed is the PRNG seed used by Default.
	DefaultSeed uint64 = 1
	// DefaultSteps is the number of workload steps after schema step 0.
	DefaultSteps = 30
	// IndexStep is the step that creates the secondary index on events.
	IndexStep = 5
	// MaxSteps caps the step count accepted by Validate.
	MaxSteps = 10000
)

// FormatVersion versions the on-disk step-hash cache. Bump it whenever the
// generated SQL, parameters, or SQLite options change.
const FormatVersion = 2

// Recipe identifies a deterministic workload by PRNG seed, step count, and
// journal mode ("wal" or "delete").
type Recipe struct {
	Seed        uint64
	Steps       int
	JournalMode string
}

// Default returns the standard course recipe: seed 1, 30 workload steps, and
// WAL mode (the platform default).
func Default() Recipe {
	return Recipe{Seed: DefaultSeed, Steps: DefaultSteps, JournalMode: sqlitex.JournalWAL}
}

// WithSteps returns a copy of r with Steps replaced by steps.
func (r Recipe) WithSteps(steps int) Recipe {
	r.Steps = steps
	return r
}

// WithJournalMode returns a copy of r with JournalMode replaced by mode.
func (r Recipe) WithJournalMode(mode string) Recipe {
	r.JournalMode = mode
	return r
}

// Validate reports an error if r's step count is out of range or its journal
// mode is neither WAL nor DELETE.
func (r Recipe) Validate() error {
	if r.Steps < 0 || r.Steps > MaxSteps {
		return fmt.Errorf("recipe: steps %d out of range 0..%d", r.Steps, MaxSteps)
	}
	switch r.JournalMode {
	case sqlitex.JournalWAL, sqlitex.JournalDelete:
		return nil
	}
	return fmt.Errorf("recipe: journal mode %q not supported (want %q or %q)", r.JournalMode, sqlitex.JournalWAL, sqlitex.JournalDelete)
}

// String formats r as a compact description for logs and evidence output.
func (r Recipe) String() string {
	return fmt.Sprintf("seed=%d steps=%d journal_mode=%s", r.Seed, r.Steps, r.JournalMode)
}

// SQLiteOptions returns the explicit file and connection settings used when
// building or opening a recipe database.
//
// SecureDelete is always enabled (strictly ON) so freed space inside B-tree
// pages and on freed freelist pages is zeroed rather than keeping stale bytes
// from earlier histories. SingleConnection pins the pool to one SQLite
// connection so pool churn cannot open extra connections or shift the WAL
// change counter.
func (r Recipe) SQLiteOptions() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.File.JournalMode = r.JournalMode
	options.Conn.SecureDelete = true
	options.SingleConnection = true
	return options
}

// Statement is one parameterized SQL statement and its bound arguments.
type Statement struct {
	SQL  string
	Args []any
}

// StepKind classifies a statement inside a Step for summary counts and tests.
type StepKind int

// Statement kinds recorded alongside each generated Statement.
const (
	KindSchema StepKind = iota
	KindInsert
	KindUpdate
	KindDelete
	KindIndex
	KindUserVersion
)

// Step is one numbered transaction in a recipe plan. Number 0 creates the
// schema; numbers 1..N run workload statements.
type Step struct {
	Number     int
	Statements []Statement
	Kinds      []StepKind
}

// StepCounts summarizes how many statements of each kind appear in a Step.
type StepCounts struct {
	Schema  int
	Inserts int
	Updates int
	Deletes int
	Indexes int
}

// Counts tallies the non-metadata statements in s by StepKind.
func (s Step) Counts() StepCounts {
	var counts StepCounts
	for _, kind := range s.Kinds {
		switch kind {
		case KindSchema:
			counts.Schema++
		case KindInsert:
			counts.Inserts++
		case KindUpdate:
			counts.Updates++
		case KindDelete:
			counts.Deletes++
		case KindIndex:
			counts.Indexes++
		}
	}
	return counts
}

// Summary formats s's statement counts as a single human-readable line.
func (s Step) Summary() string {
	counts := s.Counts()
	return fmt.Sprintf("step %d: schema=%d inserts=%d updates=%d deletes=%d indexes=%d", s.Number, counts.Schema, counts.Inserts, counts.Updates, counts.Deletes, counts.Indexes)
}

func (s *Step) add(kind StepKind, sql string, args ...any) {
	s.Statements = append(s.Statements, Statement{SQL: sql, Args: args})
	s.Kinds = append(s.Kinds, kind)
}

// SchemaSQL holds the three CREATE TABLE statements executed in step 0. Every
// table uses an explicit INTEGER PRIMARY KEY so rowids are supplied by Go
// rather than chosen by SQLite.
var SchemaSQL = []string{
	"CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT NOT NULL, balance INTEGER NOT NULL)",
	"CREATE TABLE documents(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, title TEXT NOT NULL, body BLOB NOT NULL)",
	"CREATE TABLE events(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, kind TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT NOT NULL)",
}

// IndexSQL is the secondary index created during step IndexStep.
const IndexSQL = "CREATE INDEX events_by_account ON events(account_id, id)"

var eventKinds = []string{"deposit", "withdraw", "transfer", "fee"}

// Bounds on generated document body sizes in bytes. On a 4096-byte page a
// table leaf row spills to overflow pages above 4061 bytes of payload, so this
// range exercises both inline rows and multi-page overflow chains.
const (
	MinBodyBytes = 2000
	MaxBodyBytes = 16000
)

type generator struct {
	seed          uint64
	nextAccountID int64
	nextDocID     int64
	nextEventID   int64
	accounts      []int64
	documents     []int64
	events        []int64
}

func newGenerator(seed uint64) *generator {
	return &generator{seed: seed, nextAccountID: 1, nextDocID: 1, nextEventID: 1}
}

// Plan generates steps 0 through r.Steps in order. Each step depends only on
// r.Seed and its step index, never on r.Steps.
func (r Recipe) Plan() ([]Step, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	g := newGenerator(r.Seed)
	steps := make([]Step, 0, r.Steps+1)
	for k := 0; k <= r.Steps; k++ {
		steps = append(steps, g.step(k))
	}
	return steps, nil
}

// Step generates and returns step k alone by walking the live-id state from
// step 0 through step k.
func (r Recipe) Step(k int) (Step, error) {
	if err := r.Validate(); err != nil {
		return Step{}, err
	}
	if k < 0 || k > r.Steps {
		return Step{}, fmt.Errorf("recipe: step %d out of range 0..%d", k, r.Steps)
	}
	plan, err := r.WithSteps(k).Plan()
	if err != nil {
		return Step{}, err
	}
	return plan[k], nil
}

func userVersionSQL(k int) string {
	return fmt.Sprintf("PRAGMA user_version = %d", k)
}

func (g *generator) step(k int) Step {
	s := Step{Number: k}
	if k == 0 {
		for _, sql := range SchemaSQL {
			s.add(KindSchema, sql)
		}
		s.add(KindUserVersion, userVersionSQL(0))
		return s
	}
	// Seed each step from (seed, k) so every value comes from Go's deterministic
	// PRNG rather than SQL random() or wall-clock functions.
	rng := rand.New(rand.NewPCG(g.seed, uint64(k)))

	for range 4 + rng.IntN(5) {
		id := g.nextAccountID
		g.nextAccountID++
		g.accounts = append(g.accounts, id)
		s.add(KindInsert, "INSERT INTO accounts(id, name, balance) VALUES(?, ?, ?)", id, randomWord(rng, 6, 18), rng.Int64N(100000))
	}
	for range 2 + rng.IntN(4) {
		id := g.nextDocID
		g.nextDocID++
		g.documents = append(g.documents, id)
		s.add(KindInsert, "INSERT INTO documents(id, account_id, title, body) VALUES(?, ?, ?, ?)", id, pick(rng, g.accounts), randomWord(rng, 8, 40), randomBody(rng))
	}
	for range 30 + rng.IntN(31) {
		id := g.nextEventID
		g.nextEventID++
		g.events = append(g.events, id)
		s.add(KindInsert, "INSERT INTO events(id, account_id, kind, amount, note) VALUES(?, ?, ?, ?, ?)", id, pick(rng, g.accounts), eventKinds[rng.IntN(len(eventKinds))], rng.Int64N(20000)-10000, randomWord(rng, 10, 60))
	}

	for range 2 + rng.IntN(4) {
		s.add(KindUpdate, "UPDATE accounts SET balance = ? WHERE id = ?", rng.Int64N(100000), pick(rng, g.accounts))
	}
	if rng.IntN(2) == 0 {
		s.add(KindUpdate, "UPDATE documents SET body = ? WHERE id = ?", randomBody(rng), pick(rng, g.documents))
	}

	// Periodic deletes exercise freeblocks and freelist pages (and overflow
	// chain release when a document is removed).
	if k%3 == 0 {
		for range 4 + rng.IntN(8) {
			var id int64
			g.events, id = removeRandom(rng, g.events)
			s.add(KindDelete, "DELETE FROM events WHERE id = ?", id)
		}
	}
	if k%4 == 0 && len(g.documents) > 2 {
		var id int64
		g.documents, id = removeRandom(rng, g.documents)
		s.add(KindDelete, "DELETE FROM documents WHERE id = ?", id)
	}

	if k == IndexStep {
		s.add(KindIndex, IndexSQL)
	}
	// Record the step number in header bytes 60..63 inside the same transaction
	// so CurrentStep always agrees with the committed rows.
	s.add(KindUserVersion, userVersionSQL(k))
	return s
}

func pick(rng *rand.Rand, ids []int64) int64 {
	return ids[rng.IntN(len(ids))]
}

func removeRandom(rng *rand.Rand, ids []int64) ([]int64, int64) {
	i := rng.IntN(len(ids))
	id := ids[i]
	return append(ids[:i:i], ids[i+1:]...), id
}

const letters = "abcdefghijklmnopqrstuvwxyz"

func randomWord(rng *rand.Rand, minLength, maxLength int) string {
	n := minLength + rng.IntN(maxLength-minLength+1)
	var b strings.Builder
	b.Grow(n)
	for range n {
		b.WriteByte(letters[rng.IntN(len(letters))])
	}
	return b.String()
}

func randomBody(rng *rand.Rand) []byte {
	n := MinBodyBytes + rng.IntN(MaxBodyBytes-MinBodyBytes+1)
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(rng.Uint32())
	}
	return body
}
```

#### `internal/recipe/apply.go`

<!-- include: ../internal/recipe/apply.go -->
```go
package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Execer is the minimal subset of *sql.Tx, *sql.Conn, and *sql.DB needed to
// execute a Step's statements.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// TxBeginner is the subset of *sql.DB and *sql.Conn needed to wrap a Step in
// one transaction.
type TxBeginner interface {
	BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error)
}

// ExecIn runs every statement in s in order on ex without starting or
// committing a transaction. Callers use it when combining steps into a single
// transaction.
func (s Step) ExecIn(ctx context.Context, ex Execer) error {
	for i, statement := range s.Statements {
		if _, err := ex.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			return fmt.Errorf("recipe step %d statement %d (%s): %w", s.Number, i, statement.SQL, err)
		}
	}
	return nil
}

// ApplyStep runs s as a single transaction on db, rolling back if any
// statement fails. With sqlitex's default _txlock=immediate, BeginTx acquires
// the write lock up front.
func ApplyStep(ctx context.Context, db TxBeginner, s Step) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recipe step %d begin: %w", s.Number, err)
	}
	if err := s.ExecIn(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recipe step %d commit: %w", s.Number, err)
	}
	return nil
}

// ApplyStep generates step k of r and commits it as one transaction on db.
func (r Recipe) ApplyStep(ctx context.Context, db TxBeginner, k int) error {
	s, err := r.Step(k)
	if err != nil {
		return err
	}
	return ApplyStep(ctx, db, s)
}

// Open validates r and opens path with r's explicit SQLite options.
func (r Recipe) Open(ctx context.Context, path string) (*sqlitex.DB, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return sqlitex.Open(ctx, path, r.SQLiteOptions())
}

// SideFileSuffixes lists the SQLite sidecar files that must be removed with a
// database or absent when the database is at rest.
var SideFileSuffixes = []string{"-wal", "-shm", "-journal"}

// RemoveDatabase deletes path and any SQLite sidecar files (-wal, -shm,
// -journal), ignoring files that do not exist.
func RemoveDatabase(path string) error {
	for _, suffix := range append([]string{""}, SideFileSuffixes...) {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// CheckAtRest verifies that path exists and has no -wal, -shm, or -journal
// sidecar files alongside it. A closed WAL database leaves no sidecars once
// the last connection checkpoints and removes them on close.
func CheckAtRest(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	for _, suffix := range SideFileSuffixes {
		if _, err := os.Stat(path + suffix); err == nil {
			return fmt.Errorf("recipe: %s is not at rest: %s exists", path, path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ApplyInSession opens path, commits s in one transaction, closes the
// connection, and verifies the database is at rest.
//
// Because every recipe step dirties page 1 (PRAGMA user_version = k), opening
// a fresh connection per step makes the WAL file change counter (header bytes
// 24..27 and 92..95) advance by one per step: pagerWalFrames (src/pager.c:3264)
// calls pager_write_changecounter only when pList->pgno == 1, writing
// dbFileVers+1 (src/pager.c:3135), and dbFileVers is refreshed only when page 1
// is read from disk (src/pager.c:3109-3110).
func (r Recipe) ApplyInSession(ctx context.Context, path string, s Step) error {
	db, err := r.Open(ctx, path)
	if err != nil {
		return err
	}
	if err := ApplyStep(ctx, db, s); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	return CheckAtRest(path)
}

// Build creates a fresh database at path and applies steps 0..upTo using one
// connection session per step.
func (r Recipe) Build(ctx context.Context, path string, upTo int) error {
	return r.BuildEach(ctx, path, upTo, nil)
}

// BuildEach creates a fresh database at path, applies steps 0..upTo in
// separate sessions, and calls afterStep(k) after each step's file is closed
// and at rest.
func (r Recipe) BuildEach(ctx context.Context, path string, upTo int, afterStep func(k int) error) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if upTo < 0 || upTo > r.Steps {
		return fmt.Errorf("recipe: build up to step %d out of range 0..%d", upTo, r.Steps)
	}
	plan, err := r.WithSteps(upTo).Plan()
	if err != nil {
		return err
	}
	if err := RemoveDatabase(path); err != nil {
		return err
	}
	for _, s := range plan {
		if err := r.ApplyInSession(ctx, path, s); err != nil {
			return err
		}
		if afterStep != nil {
			if err := afterStep(s.Number); err != nil {
				return err
			}
		}
	}
	return nil
}

// CurrentStep reports the last recipe step committed in q, or -1 if none of
// the three recipe tables exist yet. Because step 0 sets user_version = 0,
// checking sqlite_schema distinguishes an empty database from step 0.
func CurrentStep(ctx context.Context, q sqlitex.Querier) (int, error) {
	var tables int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('accounts', 'documents', 'events')").Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 0 {
		return -1, nil
	}
	if tables != len(SchemaSQL) {
		return 0, fmt.Errorf("recipe: database has %d of the %d recipe tables", tables, len(SchemaSQL))
	}
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// ApplyNext reads CurrentStep from db and commits the next step (current + 1),
// returning the step number just applied.
func (r Recipe) ApplyNext(ctx context.Context, db *sqlitex.DB) (int, error) {
	current, err := CurrentStep(ctx, db)
	if err != nil {
		return 0, err
	}
	next := current + 1
	if next > r.Steps {
		return current, fmt.Errorf("recipe: already at the last step %d", current)
	}
	return next, r.ApplyStep(ctx, db, next)
}
```

#### `internal/recipe/hashes.go`

<!-- include: ../internal/recipe/hashes.go -->
```go
package recipe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/sqlite"
)

// FileSHA256 returns the lowercase hex SHA-256 digest of the file at path.
// Never call it on a file currently held open by a SQLite connection in the
// same process: closing the extra file descriptor drops the process's POSIX
// locks on that file.
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ChangeCounterRanges lists the half-open byte offsets in page 1's 100-byte
// header updated by pager_write_changecounter (src/pager.c:3130-3143):
// bytes 24..27 (file change counter) and 92..95 (version-valid-for number).
var ChangeCounterRanges = [][2]int{{24, 28}, {92, 96}}

// MaskChangeCounter returns a copy of image with the change-counter header
// bytes zeroed so two WAL databases built with different connection session
// counts can still be compared byte for byte.
func MaskChangeCounter(image []byte) []byte {
	masked := append([]byte(nil), image...)
	for _, span := range ChangeCounterRanges {
		for i := span[0]; i < span[1] && i < len(masked); i++ {
			masked[i] = 0
		}
	}
	return masked
}

// FileHashes holds the SHA-256 digests of a database file both verbatim and
// with ChangeCounterRanges zeroed.
type FileHashes struct {
	Exact  string
	Masked string
}

// HashFile reads path from disk and returns its exact and masked SHA-256
// hashes. The database must already be closed.
func HashFile(path string) (FileHashes, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return FileHashes{}, err
	}
	return HashImage(image), nil
}

// HashImage returns the exact and change-counter-masked SHA-256 hashes of a
// database file image in memory.
func HashImage(image []byte) FileHashes {
	exact := sha256.Sum256(image)
	masked := sha256.Sum256(MaskChangeCounter(image))
	return FileHashes{Exact: hex.EncodeToString(exact[:]), Masked: hex.EncodeToString(masked[:])}
}

// ComputeStepHashes builds r inside dir from step 0 through r.Steps and
// returns the at-rest FileHashes after every step.
func (r Recipe) ComputeStepHashes(ctx context.Context, dir string) ([]FileHashes, error) {
	path := filepath.Join(dir, "recipe-step-hashes.db")
	defer RemoveDatabase(path)
	var hashes []FileHashes
	err := r.BuildEach(ctx, path, r.Steps, func(k int) error {
		h, err := HashFile(path)
		if err != nil {
			return err
		}
		hashes = append(hashes, h)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hashes, nil
}

func (r Recipe) cacheKey() string {
	// Include FormatVersion and the pinned Fossil check-in so a generator or
	// SQLite upgrade never reuses stale hashes.
	return fmt.Sprintf("v%d-%s-seed%d-%s", FormatVersion, sqlite.Current().FossilID[:16], r.Seed, r.JournalMode)
}

// CacheDir returns the directory under the course work base where computed
// per-step hash lists are cached across experiment runs.
func CacheDir() (string, error) {
	base, err := sqlitex.WorkBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "recipe-cache"), nil
}

// CachePath returns the cache file path for r's seed, mode, format version,
// and pinned SQLite check-in.
func (r Recipe) CachePath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, r.cacheKey()+".txt"), nil
}

// StepHashes returns the per-step hashes for steps 0..r.Steps, reusing the
// cached file on disk only when it has at least r.Steps+1 entries, its trailing
// SHA-256 checksum matches r's key, SQLite options, plan, and hashes, and a
// fresh step-0 rebuild matches entry 0. Otherwise it rebuilds and replaces the
// cache atomically.
func (r Recipe) StepHashes(ctx context.Context) ([]FileHashes, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	cachePath, err := r.CachePath()
	if err != nil {
		return nil, err
	}
	if cached, err := r.readVerifiedCache(ctx, cachePath); err == nil && len(cached) > r.Steps {
		return cached[:r.Steps+1], nil
	}
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	buildDir, err := os.MkdirTemp(dir, "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(buildDir)
	hashes, err := r.ComputeStepHashes(ctx, buildDir)
	if err != nil {
		return nil, err
	}
	if err := r.writeHashCache(cachePath, hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

// ExpectedHash returns the exact at-rest SHA-256 digest for step k of r.
func (r Recipe) ExpectedHash(ctx context.Context, k int) (string, error) {
	if k < 0 || k > r.Steps {
		return "", fmt.Errorf("recipe: step %d out of range 0..%d", k, r.Steps)
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		return "", err
	}
	return hashes[k].Exact, nil
}

// Match identifies which recipe step a database file equals. Step is -1 when
// no step matches; Exact is true for a verbatim SHA-256 match and false when
// only the change-counter-masked hash matched.
type Match struct {
	Step  int
	Exact bool
}

// Found reports whether m matched a step of the recipe.
func (m Match) Found() bool {
	return m.Step >= 0
}

// String describes m for experiment output.
func (m Match) String() string {
	switch {
	case !m.Found():
		return "no recipe step"
	case m.Exact:
		return fmt.Sprintf("step %d (exact bytes)", m.Step)
	default:
		return fmt.Sprintf("step %d (equal except the change counter at bytes 24-27 and 92-95)", m.Step)
	}
}

// MatchHashes searches hashes for h, preferring an exact byte match before
// falling back to a change-counter-masked match.
func MatchHashes(hashes []FileHashes, h FileHashes) Match {
	for k, candidate := range hashes {
		if candidate.Exact == h.Exact {
			return Match{Step: k, Exact: true}
		}
	}
	for k, candidate := range hashes {
		if candidate.Masked == h.Masked {
			return Match{Step: k}
		}
	}
	return Match{Step: -1}
}

// MatchStep checks that path is at rest, hashes it, and reports which step of
// r it equals. A masked-only match is accepted only when the two header change
// counters agree (ChangeCountersAgree).
func (r Recipe) MatchStep(ctx context.Context, path string) (Match, error) {
	if err := CheckAtRest(path); err != nil {
		return Match{Step: -1}, err
	}
	h, err := HashFile(path)
	if err != nil {
		return Match{Step: -1}, err
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		return Match{Step: -1}, err
	}
	match := MatchHashes(hashes, h)
	if match.Found() && !match.Exact {
		agree, err := fileCountersAgree(path)
		if err != nil {
			return Match{Step: -1}, err
		}
		if !agree {
			return Match{Step: -1}, nil
		}
	}
	return match, nil
}

func fileCountersAgree(path string) (bool, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return ChangeCountersAgree(image), nil
}

// ChangeCountersAgree reports whether image is at least 96 bytes long and its
// file change counter (bytes 24..27) equals its version-valid-for number
// (bytes 92..95), which SQLite always updates together in
// pager_write_changecounter (src/pager.c:3130-3143).
func ChangeCountersAgree(image []byte) bool {
	first, second := ChangeCounterRanges[0], ChangeCounterRanges[1]
	if len(image) < second[1] {
		return false
	}
	return bytes.Equal(image[first[0]:first[1]], image[second[0]:second[1]])
}

func isSHA256Hex(text string) bool {
	_, err := hex.DecodeString(text)
	return err == nil && len(text) == sha256.Size*2
}

const cacheChecksumPrefix = "checksum "

// readHashCache parses a v2 step-hash cache file ("k exact masked" lines
// followed by a final "checksum <sha256>" line) without verifying the digest.
func readHashCache(path string) ([]FileHashes, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	var hashes []FileHashes
	var body strings.Builder
	checksum := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if checksum != "" {
			return nil, "", fmt.Errorf("recipe cache %s: line %q after the checksum", path, line)
		}
		if rest, ok := strings.CutPrefix(line, cacheChecksumPrefix); ok {
			if !isSHA256Hex(rest) {
				return nil, "", fmt.Errorf("recipe cache %s: bad checksum line %q", path, line)
			}
			checksum = rest
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, "", fmt.Errorf("recipe cache %s: malformed line %q", path, line)
		}
		k, err := strconv.Atoi(fields[0])
		if err != nil || k != len(hashes) {
			return nil, "", fmt.Errorf("recipe cache %s: line %q out of order", path, line)
		}
		if !isSHA256Hex(fields[1]) || !isSHA256Hex(fields[2]) {
			return nil, "", fmt.Errorf("recipe cache %s: bad hash on line %q", path, line)
		}
		hashes = append(hashes, FileHashes{Exact: fields[1], Masked: fields[2]})
		body.WriteString(line)
		body.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	if checksum == "" {
		return nil, "", fmt.Errorf("recipe cache %s: no checksum line", path)
	}
	return hashes, checksum, nil
}

// cacheChecksum hashes the cache key, SQLite options, every planned statement
// and bound argument, and all cached step hashes so any drift invalidates the
// on-disk cache.
func (r Recipe) cacheChecksum(hashes []FileHashes) (string, error) {
	if len(hashes) == 0 {
		return "", errors.New("recipe cache: no hashes")
	}
	plan, err := r.WithSteps(len(hashes) - 1).Plan()
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\n%s\n%s\n", r.cacheKey(), r.SQLiteOptions().File, r.SQLiteOptions().Conn)
	for _, s := range plan {
		for _, statement := range s.Statements {
			fmt.Fprintf(sum, "%d %q %#v\n", s.Number, statement.SQL, statement.Args)
		}
	}
	for k, h := range hashes {
		fmt.Fprintf(sum, "%d %s %s\n", k, h.Exact, h.Masked)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// readVerifiedCache parses path, checks its checksum against r, and rebuilds
// step 0 in a temporary directory to confirm entry 0 matches the live SQLite
// build and sqlitex open path.
func (r Recipe) readVerifiedCache(ctx context.Context, path string) ([]FileHashes, error) {
	hashes, checksum, err := readHashCache(path)
	if err != nil {
		return nil, err
	}
	want, err := r.cacheChecksum(hashes)
	if err != nil {
		return nil, err
	}
	if checksum != want {
		return nil, fmt.Errorf("recipe cache %s: checksum mismatch", path)
	}
	fresh, err := r.freshStepZeroHash(ctx, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if fresh != hashes[0] {
		return nil, fmt.Errorf("recipe cache %s: step 0 rebuilds to %s, the cache says %s", path, fresh.Exact, hashes[0].Exact)
	}
	return hashes, nil
}

// freshStepZeroHash builds step 0 in an isolated temporary directory under dir
// and returns its exact and masked hashes.
func (r Recipe) freshStepZeroHash(ctx context.Context, dir string) (FileHashes, error) {
	buildDir, err := os.MkdirTemp(dir, "verify-")
	if err != nil {
		return FileHashes{}, err
	}
	defer os.RemoveAll(buildDir)
	path := filepath.Join(buildDir, "step-0.db")
	if err := r.Build(ctx, path, 0); err != nil {
		return FileHashes{}, err
	}
	return HashFile(path)
}

func (r Recipe) writeHashCache(path string, hashes []FileHashes) error {
	checksum, err := r.cacheChecksum(hashes)
	if err != nil {
		return err
	}
	var b strings.Builder
	for k, h := range hashes {
		fmt.Fprintf(&b, "%d %s %s\n", k, h.Exact, h.Masked)
	}
	b.WriteString(cacheChecksumPrefix + checksum + "\n")
	// Write to a sibling temp file and rename so concurrent readers never see
	// a partially written cache file.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	if _, err := temp.WriteString(b.String()); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		os.Remove(temp.Name())
		return err
	}
	return nil
}
```

### 5.2 Experiment 1.3 — Recipe rebuild, rung R0 (`1.3-recipe-rebuild`)

- **Claim:** A database built by a fixed, seeded workload with explicit settings is reproducible to the exact byte (**E1**), so the recipe `(seed, step count, journal mode)` is a complete saved state with a unique SHA-256 hash after every step *k*. Breaking the determinism rules breaks byte equality in specific, measurable ways (Section 6).
- **Capture / Rebuild / Verified at:** Capture = recipe `(seed=1, steps=30, journal_mode=wal)`. Rebuild = run the recipe into an empty file. Proven equal at **E1 (bytes)**.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Platform:** Portable (runs natively on Linux and macOS without `strace`).
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 1.3
  ```

<!-- include: experiments/1.3-recipe-rebuild/main.go -->
```go
// Experiment 1.3 proves Rebuild Ladder rung R0: running the seeded recipe
// twice in different directories under different names produces byte-identical
// database files at every step. It then tests seven sources of variation (plus
// secure_delete=FAST vs ON and WAL commits that never touch page 1) to show
// which determinism rules are required and where SQLite's behavior differs
// between WAL and rollback modes.
//
// It prints only its verdict; the measurements go to CSV tables that
// report.sql presents (portable; no strace needed):
//
//	go run ./cmd/course run 1.3
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize     = 4096
	variantBase  = 6
	maxRowid     = "9223372036854775807"
	secondsTries = 3
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables. part labels the claims of the part
// that is running: A the rebuild, B broken variants 1-5, C sessions, D the
// rolled-back savepoint.
var (
	set        *results.Set
	part       string
	comparison int
	tables     struct {
		recipe, plan, runs, stepHashes, lookup, comparisons, unusedPages *results.Table
		infoRun, infoHashes                                              *results.Table
	}
)

func openTables() {
	tables.recipe = set.Table("recipe", "key", "value")
	tables.plan = set.Table("plan", "step", "schema", "inserts", "updates", "deletes", "indexes")
	tables.runs = set.Table("runs", "mode", "run", "path", "pages", "sha256")
	tables.stepHashes = set.Table("step_hashes", "mode", "step", "run1_sha256", "run2_sha256", "cached_sha256")
	tables.lookup = set.Table("lookup", "built_to_step", "matched_step", "exact")
	tables.comparisons = set.Table("comparisons", "seq", "id", "what", "equal", "size_a", "size_b", "differing_pages", "page1_bytes")
	tables.unusedPages = set.Table("unused_pages", "id", "page", "bytes_differing", "bytes_outside_unused", "kind")
	tables.infoRun = set.Table("info_run", "key", "value")
	tables.infoHashes = set.Table("info_hashes", "name", "run", "sha256")
}

func check(ok bool, claim string) { set.Check(part, ok, claim) }

// record writes one file comparison; id is the number of the variant in report.sql.
func record(id, what string, d fileDiff) {
	comparison++
	tables.comparisons.Row(comparison, id, what, d.Equal, d.SizeA, d.SizeB, joinInts(d.DifferingPages), joinInts(d.Page1Bytes))
}

// recordUnused writes where the differing bytes of a comparison sit.
func recordUnused(id string, report unusedReport) {
	for _, page := range report.Pages {
		tables.unusedPages.Row(id, page.Page, page.Count, page.InUse, page.Kind)
	}
}

func joinInts(values []int) string {
	text := make([]string, len(values))
	for i, v := range values {
		text[i] = fmt.Sprint(v)
	}
	return strings.Join(text, " ")
}

func infoHash(name, run, sha string) { tables.infoHashes.Row(name, run, sha) }

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type fileDiff struct {
	Equal          bool
	SizeA, SizeB   int
	DifferingPages []int
	Page1Bytes     []int
}

// compareFiles compares two closed database files page by page (1-based) and
// records the differing byte offsets inside page 1 when page 1 differs.
func compareFiles(a, b string) fileDiff {
	dataA, err := os.ReadFile(a)
	if err != nil {
		fatal("read", err)
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		fatal("read", err)
	}
	return diffImages(dataA, dataB)
}

func pageSlice(data []byte, p int) []byte {
	start := p * pageSize
	if start >= len(data) {
		return nil
	}
	return data[start:min((p+1)*pageSize, len(data))]
}

// diffImages compares two in-memory database images page by page (rounding up
// so partial trailing pages are compared) and records differing byte offsets
// inside page 1.
func diffImages(dataA, dataB []byte) fileDiff {
	d := fileDiff{Equal: bytes.Equal(dataA, dataB), SizeA: len(dataA), SizeB: len(dataB)}
	pages := (max(len(dataA), len(dataB)) + pageSize - 1) / pageSize
	for p := range pages {
		pageA, pageB := pageSlice(dataA, p), pageSlice(dataB, p)
		if bytes.Equal(pageA, pageB) {
			continue
		}
		d.DifferingPages = append(d.DifferingPages, p+1)
		if p == 0 {
			for i := range min(len(pageA), len(pageB)) {
				if pageA[i] != pageB[i] {
					d.Page1Bytes = append(d.Page1Bytes, i)
				}
			}
		}
	}
	return d
}

// onlyChangeCounter reports whether two equal-sized files differ exclusively
// inside header bytes 24..27 (file change counter) and 92..95
// (version-valid-for), written by pager_write_changecounter (src/pager.c:3130-3143).
func onlyChangeCounter(d fileDiff) bool {
	if d.Equal || d.SizeA != d.SizeB || !slices.Equal(d.DifferingPages, []int{1}) {
		return false
	}
	return onlyCounterBytes(d.Page1Bytes)
}

// onlyCounterBytes reports whether every page-1 byte offset in offsets falls
// inside recipe.ChangeCounterRanges (bytes 24..27 or 92..95).
func onlyCounterBytes(offsets []int) bool {
	for _, offset := range offsets {
		inCounter := false
		for _, span := range recipe.ChangeCounterRanges {
			if offset >= span[0] && offset < span[1] {
				inCounter = true
			}
		}
		if !inCounter {
			return false
		}
	}
	return true
}

func hashOf(path string) recipe.FileHashes {
	h, err := recipe.HashFile(path)
	if err != nil {
		fatal("hash", err)
	}
	return h
}

func pages(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		fatal("stat", err)
	}
	return int(info.Size() / pageSize)
}

func rebuild(ctx context.Context, r recipe.Recipe, path string) []recipe.FileHashes {
	var hashes []recipe.FileHashes
	err := r.BuildEach(ctx, path, r.Steps, func(k int) error {
		hashes = append(hashes, hashOf(path))
		return nil
	})
	if err != nil {
		fatal("build "+path, err)
	}
	return hashes
}

func applyStatementsInSession(ctx context.Context, r recipe.Recipe, path string, pragmas []string, statements ...string) {
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("open", err)
	}
	// Pin a single *sql.Conn so any PRAGMAs apply to the exact connection that
	// runs the transaction.
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	for _, pragma := range pragmas {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			fatal(pragma, err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := conn.Close(); err != nil {
		fatal("conn close", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func buildBase(ctx context.Context, r recipe.Recipe, path string) {
	if err := r.Build(ctx, path, variantBase); err != nil {
		fatal("base", err)
	}
}

type variant struct {
	name      string
	rule      string
	pragmas   []string
	statement []string
}

func runTwice(ctx context.Context, r recipe.Recipe, dir string, v variant) fileDiff {
	a := filepath.Join(dir, v.name+"-run1.db")
	b := filepath.Join(dir, v.name+"-run2.db")
	for _, path := range []string{a, b} {
		buildBase(ctx, r, path)
		applyStatementsInSession(ctx, r, path, v.pragmas, v.statement...)
	}
	d := compareFiles(a, b)
	infoHash(v.name, "run 1", hashOf(a).Exact)
	infoHash(v.name, "run 2", hashOf(b).Exact)
	return d
}

// timestampVariant demonstrates both sides of current_timestamp's one-second
// resolution: two runs inside the same wall-clock second produce identical
// files, while a third run after the second advances differs.
func timestampVariant(ctx context.Context, r recipe.Recipe, dir string) (sameSecond, nextSecond fileDiff) {
	statement := "INSERT INTO events(id, account_id, kind, amount, note) VALUES(900002, 1, 'fee', 0, current_timestamp)"
	a := filepath.Join(dir, "timestamp-run1.db")
	b := filepath.Join(dir, "timestamp-run2.db")
	c := filepath.Join(dir, "timestamp-run3.db")
	for attempt := range secondsTries {
		for _, path := range []string{a, b, c} {
			buildBase(ctx, r, path)
		}
		waitForNextSecond()
		started := time.Now().Unix()
		applyStatementsInSession(ctx, r, a, nil, statement)
		applyStatementsInSession(ctx, r, b, nil, statement)
		if time.Now().Unix() != started {
			tables.infoRun.Row(fmt.Sprintf("timestamp attempt %d", attempt+1), "crossed a second boundary; retried")
			continue
		}
		waitForNextSecond()
		applyStatementsInSession(ctx, r, c, nil, statement)
		infoHash("timestamp", "run 1", hashOf(a).Exact)
		infoHash("timestamp", "run 2", hashOf(b).Exact)
		infoHash("timestamp", "run 3", hashOf(c).Exact)
		return compareFiles(a, b), compareFiles(a, c)
	}
	fatal("timestamp variant", fmt.Errorf("could not run two inserts inside one wall-clock second in %d attempts", secondsTries))
	return fileDiff{}, fileDiff{}
}

func waitForNextSecond() {
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now) + 5*time.Millisecond)
}

// mergedWithNext selects steps 2+3 and 4+5 to merge into single transactions:
// step 3 deletes rows written in step 2, exposing stale unzeroed bytes when
// secure_delete=OFF.
var mergedWithNext = []int{2, 4}

func batchVariant(ctx context.Context, r recipe.Recipe, dir, secureDelete string) (fileDiff, unusedReport) {
	plan, err := r.WithSteps(variantBase).Plan()
	if err != nil {
		fatal("plan", err)
	}
	options := r.SQLiteOptions()
	label := fmt.Sprintf("%s-secure-delete-%s", r.JournalMode, strings.ToLower(secureDelete))
	a := filepath.Join(dir, "batches-"+label+"-one-per-step.db")
	b := filepath.Join(dir, "batches-"+label+"-merged.db")
	for _, path := range []string{a, b} {
		if err := recipe.RemoveDatabase(path); err != nil {
			fatal("remove", err)
		}
		db, err := sqlitex.Open(ctx, path, options)
		if err != nil {
			fatal("open", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA secure_delete="+secureDelete); err != nil {
			fatal("secure_delete", err)
		}
		merge := path == b
		for i := 0; i < len(plan); i++ {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				fatal("begin", err)
			}
			if err := plan[i].ExecIn(ctx, tx); err != nil {
				fatal("step", err)
			}
			if merge && slices.Contains(mergedWithNext, i) {
				i++
				if err := plan[i].ExecIn(ctx, tx); err != nil {
					fatal("step", err)
				}
			}
			if err := tx.Commit(); err != nil {
				fatal("commit", err)
			}
		}
		if err := conn.Close(); err != nil {
			fatal("conn close", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		if err := recipe.CheckAtRest(path); err != nil {
			fatal("at rest", err)
		}
	}
	infoHash("batches "+label, "one per step", hashOf(a).Exact)
	infoHash("batches "+label, "merged", hashOf(b).Exact)
	d := compareFiles(a, b)
	return d, classifyDifferences(a, b, d)
}

func be16(data []byte, offset int) int {
	return int(data[offset])<<8 | int(data[offset+1])
}

func be32(data []byte, offset int) int {
	return int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
}

type freelist struct {
	trunkLeafCounts map[int]int
	leaves          map[int]bool
}

// readFreelist validates the database header's page size and freelist count,
// walks SQLite's freelist trunk chain from header offset 32, and separates
// trunk pages (with their leaf counts) from leaf pages while rejecting cycles,
// out-of-range page numbers, and count mismatches.
func readFreelist(data []byte) (freelist, error) {
	fl := freelist{trunkLeafCounts: map[int]int{}, leaves: map[int]bool{}}
	if len(data) < pageSize || len(data)%pageSize != 0 {
		return fl, fmt.Errorf("file size %d is not a whole number of %d-byte pages", len(data), pageSize)
	}
	if headerPageSize := be16(data, 16); headerPageSize != pageSize {
		return fl, fmt.Errorf("header page size %d, want %d", headerPageSize, pageSize)
	}
	pageCount := len(data) / pageSize
	maxLeaves := (pageSize - 8) / 4
	for trunk := be32(data, 32); trunk != 0; {
		if trunk < 1 || trunk > pageCount {
			return fl, fmt.Errorf("freelist trunk page %d outside 1..%d", trunk, pageCount)
		}
		if _, seen := fl.trunkLeafCounts[trunk]; seen || fl.leaves[trunk] {
			return fl, fmt.Errorf("freelist page %d listed twice", trunk)
		}
		base := (trunk - 1) * pageSize
		leafCount := be32(data, base+4)
		if leafCount > maxLeaves {
			return fl, fmt.Errorf("freelist trunk page %d claims %d leaves, at most %d fit", trunk, leafCount, maxLeaves)
		}
		fl.trunkLeafCounts[trunk] = leafCount
		for i := range leafCount {
			leaf := be32(data, base+8+4*i)
			if leaf < 1 || leaf > pageCount {
				return fl, fmt.Errorf("freelist leaf page %d outside 1..%d", leaf, pageCount)
			}
			if _, seen := fl.trunkLeafCounts[leaf]; seen || fl.leaves[leaf] {
				return fl, fmt.Errorf("freelist page %d listed twice", leaf)
			}
			fl.leaves[leaf] = true
		}
		trunk = be32(data, base)
	}
	if total := be32(data, 36); total != len(fl.trunkLeafCounts)+len(fl.leaves) {
		return fl, fmt.Errorf("header counts %d freelist pages, the chain holds %d", total, len(fl.trunkLeafCounts)+len(fl.leaves))
	}
	return fl, nil
}

func (fl freelist) contains(page int) bool {
	_, trunk := fl.trunkLeafCounts[page]
	return trunk || fl.leaves[page]
}

func isBtreePageType(t byte) bool {
	return t == 0x02 || t == 0x05 || t == 0x0a || t == 0x0d
}

// unusedBytes marks every byte offset on page that holds no live data or
// structural metadata: all bytes on a freelist leaf, bytes 8+4*leafCount..end
// on a freelist trunk, or on a B-tree page (type 0x02/0x05/0x0a/0x0d) the
// unallocated gap between the cell-pointer array and the cell content area plus
// the bodies (after the 4-byte header) of every freeblock. Non-B-tree pages
// (such as overflow pages) have no bytes marked unused.
func unusedBytes(data []byte, page int, fl freelist) ([]bool, string, error) {
	unused := make([]bool, pageSize)
	if fl.leaves[page] {
		for i := range unused {
			unused[i] = true
		}
		return unused, "freelist leaf page", nil
	}
	if leafCount, ok := fl.trunkLeafCounts[page]; ok {
		for i := 8 + 4*leafCount; i < pageSize; i++ {
			unused[i] = true
		}
		return unused, "freelist trunk page, bytes after the leaf list", nil
	}
	content := data[(page-1)*pageSize : page*pageSize]
	header := 0
	if page == 1 {
		header = 100
	}
	if !isBtreePageType(content[header]) {
		return unused, fmt.Sprintf("not a b-tree or freelist page (byte %d is 0x%02x)", header, content[header]), nil
	}
	headerSize := 8
	if content[header] == 0x02 || content[header] == 0x05 {
		headerSize = 12
	}
	cellPointersEnd := header + headerSize + 2*be16(content, header+3)
	contentStart := be16(content, header+5)
	if contentStart == 0 {
		contentStart = 65536
	}
	contentStart = min(contentStart, pageSize)
	if cellPointersEnd > contentStart {
		return nil, "", fmt.Errorf("page %d: cell pointers end at %d, after the content area starts at %d", page, cellPointersEnd, contentStart)
	}
	for i := cellPointersEnd; i < contentStart; i++ {
		unused[i] = true
	}
	previousEnd := contentStart
	for block := be16(content, header+1); block != 0; block = be16(content, block) {
		if block < previousEnd || block+4 > pageSize {
			return nil, "", fmt.Errorf("page %d: freeblock at %d is out of order or outside the content area", page, block)
		}
		size := be16(content, block+2)
		if size < 4 || block+size > pageSize {
			return nil, "", fmt.Errorf("page %d: freeblock at %d has size %d", page, block, size)
		}
		for i := block + 4; i < block+size; i++ {
			unused[i] = true
		}
		previousEnd = block + size
	}
	return unused, "b-tree page, unallocated space and freeblocks", nil
}

// differingPage describes one page whose bytes differ: how many bytes, what
// kind of page it is in the two files, and how many of the bytes are outside
// unused space.
type differingPage struct {
	Page, Count, InUse int
	Kind               string
}

type unusedReport struct {
	OnlyUnused    bool
	Pages         []differingPage
	LeafPages     []int
	FreelistPages []int
}

func classifyDifferences(a, b string, d fileDiff) unusedReport {
	dataA, err := os.ReadFile(a)
	if err != nil {
		fatal("read", err)
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		fatal("read", err)
	}
	return classifyImages(dataA, dataB, d)
}

// classifyImages checks every differing page in d against both images'
// freelist and B-tree layouts to determine whether all differing bytes lie in
// unused space. Any format error sets OnlyUnused to false.
func classifyImages(dataA, dataB []byte, d fileDiff) unusedReport {
	report := unusedReport{OnlyUnused: len(dataA) == len(dataB) && len(d.DifferingPages) > 0}
	if len(dataA) != len(dataB) {
		return report
	}
	freeA, errA := readFreelist(dataA)
	freeB, errB := readFreelist(dataB)
	if errA != nil || errB != nil {
		report.OnlyUnused = false
		report.Pages = append(report.Pages, differingPage{Kind: fmt.Sprintf("freelist unreadable: %v / %v", errA, errB)})
		return report
	}
	for _, page := range d.DifferingPages {
		unusedA, kindA, errA := unusedBytes(dataA, page, freeA)
		unusedB, kindB, errB := unusedBytes(dataB, page, freeB)
		if errA != nil || errB != nil {
			report.OnlyUnused = false
			report.Pages = append(report.Pages, differingPage{Page: page, Kind: fmt.Sprintf("unparseable (%v / %v)", errA, errB)})
			continue
		}
		base := (page - 1) * pageSize
		count, inUse := 0, 0
		for i := range pageSize {
			if dataA[base+i] != dataB[base+i] {
				count++
				if !unusedA[i] || !unusedB[i] {
					inUse++
					report.OnlyUnused = false
				}
			}
		}
		if freeA.leaves[page] && freeB.leaves[page] {
			report.LeafPages = append(report.LeafPages, page)
		}
		if freeA.contains(page) && freeB.contains(page) {
			report.FreelistPages = append(report.FreelistPages, page)
		}
		kind := kindA
		if kindB != kindA {
			kind = kindA + " / " + kindB
		}
		report.Pages = append(report.Pages, differingPage{Page: page, Count: count, InUse: inUse, Kind: kind})
	}
	return report
}

// sessionVariant compares building steps 0..variantBase with one connection
// session per step against building the exact same steps on one long-lived
// connection. Because every recipe step dirties page 1 (PRAGMA user_version),
// WAL mode differs in the header change counter alone: dbFileVers is refreshed
// on page-1 read, not on WAL commit (src/pager.c:3109-3110, 3264).
func sessionVariant(ctx context.Context, r recipe.Recipe, dir string) fileDiff {
	canonical := filepath.Join(dir, "sessions-"+r.JournalMode+"-one-per-step.db")
	if err := r.Build(ctx, canonical, variantBase); err != nil {
		fatal("build", err)
	}
	longLived := filepath.Join(dir, "sessions-"+r.JournalMode+"-one-for-all.db")
	if err := recipe.RemoveDatabase(longLived); err != nil {
		fatal("remove", err)
	}
	db, err := r.Open(ctx, longLived)
	if err != nil {
		fatal("open", err)
	}
	for k := 0; k <= variantBase; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return compareFiles(canonical, longLived)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("1.3")
	dir, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	set = results.MustOpen(work)
	openTables()
	tables.infoRun.Row("work dir", displayPath(dir))

	r := recipe.Default()
	tables.recipe.Row("recipe", r.String())
	tables.recipe.Row("index_step", recipe.IndexStep)
	tables.recipe.Row("file_settings", r.SQLiteOptions().File)
	tables.recipe.Row("conn_settings", r.SQLiteOptions().Conn)
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	for _, s := range plan {
		counts := s.Counts()
		tables.plan.Row(s.Number, counts.Schema, counts.Inserts, counts.Updates, counts.Deletes, counts.Indexes)
	}

	part = "A"
	if err := os.MkdirAll(filepath.Join(dir, "run-1"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "run-2", "elsewhere"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	first := filepath.Join(dir, "run-1", "recipe.db")
	second := filepath.Join(dir, "run-2", "elsewhere", "another-name.db")
	started := time.Now()
	firstHashes := rebuild(ctx, r, first)
	tables.infoRun.Row("one build", fmt.Sprintf("%d steps took %s", r.Steps, time.Since(started).Round(time.Millisecond)))
	secondHashes := rebuild(ctx, r, second)
	tables.runs.Row(r.JournalMode, 1, "run-1/recipe.db", pages(first), hashOf(first).Exact)
	tables.runs.Row(r.JournalMode, 2, "run-2/elsewhere/another-name.db", pages(second), hashOf(second).Exact)
	check(hashOf(first) == hashOf(second), "two builds of the recipe in different directories under different names are byte-identical")
	check(slices.Equal(firstHashes, secondHashes), fmt.Sprintf("the per-step hash lists of the two runs are identical (%d states)", len(firstHashes)))
	distinct := map[string]bool{}
	for _, h := range firstHashes {
		distinct[h.Exact] = true
	}
	check(len(distinct) == len(firstHashes), "every step leaves a different file, so a hash names exactly one step")
	cached, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	for k, h := range firstHashes {
		tables.stepHashes.Row(r.JournalMode, k, h.Exact, secondHashes[k].Exact, cached[k].Exact)
	}
	check(slices.Equal(cached, firstHashes), "recipe.StepHashes (the cached list later chapters use) equals the list just built")
	middle := filepath.Join(dir, "run-1", "to-step-12.db")
	if err := r.Build(ctx, middle, 12); err != nil {
		fatal("build", err)
	}
	match, err := r.MatchStep(ctx, middle)
	if err != nil {
		fatal("match", err)
	}
	tables.lookup.Row(12, match.Step, match.Exact)
	check(match == recipe.Match{Step: 12, Exact: true}, "recipe.MatchStep names the step a database equals")

	deleteRecipe := r.WithJournalMode(sqlitex.JournalDelete)
	deleteA := filepath.Join(dir, "run-1", "recipe-delete.db")
	deleteB := filepath.Join(dir, "run-2", "recipe-delete.db")
	deleteHashesA := rebuild(ctx, deleteRecipe, deleteA)
	deleteHashesB := rebuild(ctx, deleteRecipe, deleteB)
	tables.runs.Row(deleteRecipe.JournalMode, 1, "run-1/recipe-delete.db", pages(deleteA), hashOf(deleteA).Exact)
	tables.runs.Row(deleteRecipe.JournalMode, 2, "run-2/recipe-delete.db", pages(deleteB), hashOf(deleteB).Exact)
	for k := range deleteHashesA {
		tables.stepHashes.Row(deleteRecipe.JournalMode, k, deleteHashesA[k].Exact, deleteHashesB[k].Exact, nil)
	}
	check(slices.Equal(deleteHashesA, deleteHashesB), "the rollback-mode recipe also rebuilds byte for byte at every step")
	record("0", "WAL build vs rollback build of the same recipe", compareFiles(first, deleteA))

	part = "B"
	randomDiff := runTwice(ctx, r, dir, variant{
		name:      "random",
		rule:      "no SQL random()",
		statement: []string{"INSERT INTO events(id, account_id, kind, amount, note) VALUES(900001, 1, 'fee', random(), 'random')"},
	})
	record("1", "SQL random() in a value", randomDiff)
	check(!randomDiff.Equal, "SQL random() makes two runs differ")

	sameSecond, nextSecond := timestampVariant(ctx, r, dir)
	record("2a", "current_timestamp, two runs inside one wall-clock second", sameSecond)
	record("2b", "current_timestamp, third run one second later", nextSecond)
	check(sameSecond.Equal && !nextSecond.Equal, "current_timestamp makes runs differ once the wall clock's second changes (its resolution is one second, so two fast runs can still match)")

	unordered := "INSERT INTO archive(account_id, amount) SELECT account_id, amount FROM events WHERE amount > 0"
	ordered := unordered + " ORDER BY id"
	createArchive := "CREATE TABLE archive(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, amount INTEGER NOT NULL)"
	unorderedSame := runTwice(ctx, r, dir, variant{name: "unordered", statement: []string{createArchive, unordered}})
	record("3a", "INSERT ... SELECT without ORDER BY, same plan twice", unorderedSame)
	unorderedPlan := runTwiceWithPlanChange(ctx, r, dir, "unordered-plan", createArchive, unordered)
	record("3b", "same statement, run 2 with reverse_unordered_selects=ON", unorderedPlan)
	orderedPlan := runTwiceWithPlanChange(ctx, r, dir, "ordered-plan", createArchive, ordered)
	record("3c", "with ORDER BY id, run 2 with reverse_unordered_selects=ON", orderedPlan)
	check(unorderedSame.Equal, "FINDING: an unordered INSERT ... SELECT run twice on the same build with the same plan gives identical files")
	check(!unorderedPlan.Equal && orderedPlan.Equal, "an unordered INSERT ... SELECT changes the file when the scan order changes; ORDER BY removes the dependence")

	batchesWAL, _ := batchVariant(ctx, r, dir, "ON")
	record("4a", "WAL, recipe settings (secure_delete=ON)", batchesWAL)
	batchesWALOff, unusedWALOff := batchVariant(ctx, r, dir, "OFF")
	record("4b", "WAL, secure_delete=OFF", batchesWALOff)
	recordUnused("4b", unusedWALOff)
	batchesDelete, _ := batchVariant(ctx, deleteRecipe, dir, "ON")
	record("4c", "rollback, secure_delete=ON", batchesDelete)
	check(batchesWAL.Equal, "FINDING: in WAL mode on one connection with secure_delete=ON, different transaction batches give identical files")
	check(!batchesWALOff.Equal && unusedWALOff.OnlyUnused, "with secure_delete=OFF different batches differ, and only in bytes no cell uses (stale content left in unallocated space, freeblocks or freelist pages)")
	check(onlyChangeCounter(batchesDelete), "in rollback mode different batches differ, and with secure_delete=ON only in the change counter (it counts commits)")
	fastVsOn, fastVsOnUnused := secureDeleteFastVariant(ctx, r, dir)
	record("4d", "WAL, one transaction per step: secure_delete=FAST vs ON", fastVsOn)
	recordUnused("4d", fastVsOnUnused)
	check(!fastVsOn.Equal && fastVsOnUnused.OnlyUnused && slices.Equal(fastVsOnUnused.FreelistPages, fastVsOn.DifferingPages), "secure_delete=FAST is not ON: on the same history FAST leaves old bytes in the unused space of freelist pages that ON zeroes")

	rowidDiff := runTwice(ctx, r, dir, variant{
		name: "rowid",
		statement: []string{
			"CREATE TABLE tickets(id INTEGER PRIMARY KEY, label TEXT NOT NULL)",
			"INSERT INTO tickets(id, label) VALUES(" + maxRowid + ", 'max')",
			"INSERT INTO tickets(label) VALUES('next')",
		},
	})
	record("5", "insert without an id after the maximum rowid", rowidDiff)
	check(!rowidDiff.Equal, "rowid assignment past the maximum rowid makes two runs differ")

	part = "C"
	walSessions := sessionVariant(ctx, r, dir)
	deleteSessions := sessionVariant(ctx, deleteRecipe, dir)
	walSessionsNoPage1 := sessionsWithoutPage1(ctx, r, dir)
	record("6a", "WAL, one connection per step vs one for all steps", walSessions)
	record("6b", "rollback, the same comparison", deleteSessions)
	check(onlyChangeCounter(walSessions), "FINDING: in WAL mode the number of connection sessions changes the file, and only the change counter (bytes 24-27 and 92-95)")
	check(deleteSessions.Equal, "in rollback mode the number of connection sessions does not change the file")
	record("6c", fmt.Sprintf("WAL, %d commits that never write page 1: one connection each vs one for all", page1UntouchedCommits), walSessionsNoPage1)
	check(walSessionsNoPage1.Equal, "in WAL mode only a commit that writes page 1 writes the change counter: sessions whose commits never touch page 1 leave the file identical")

	part = "D"
	walAborted, walAbortedUnused := abortedSavepointVariant(ctx, r, dir)
	deleteAborted, deleteAbortedUnused := abortedSavepointVariant(ctx, deleteRecipe, dir)
	record("7a", "WAL, secure_delete=ON", walAborted)
	recordUnused("7a", walAbortedUnused)
	record("7b", "rollback, secure_delete=ON", deleteAborted)
	recordUnused("7b", deleteAbortedUnused)
	walRest := slices.DeleteFunc(slices.Clone(walAborted.DifferingPages), func(page int) bool { return page == 1 })
	check(!walAborted.Equal && onlyCounterBytes(walAborted.Page1Bytes) && len(walRest) > 0 && slices.Equal(walRest, walAbortedUnused.LeafPages), "WAL mode: a rolled-back SAVEPOINT changes the committed file even with secure_delete=ON: reused freelist leaf pages keep the rolled-back bytes, and page 1's change counter moves")
	check(!deleteAborted.Equal && deleteAbortedUnused.OnlyUnused && slices.Equal(deleteAbortedUnused.LeafPages, deleteAborted.DifferingPages), "rollback mode: a rolled-back SAVEPOINT leaves the rolled-back bytes in reused freelist leaf pages (secure_delete=ON does not clear them)")

	set.Finish("RECIPE REBUILD")
}

const touchAccountSQL = "UPDATE accounts SET balance = balance + 1 WHERE id = 1"

// secureDeleteFastVariant compares the one-transaction-per-step WAL build
// under PRAGMA secure_delete=FAST against secure_delete=ON. FAST zeroes freed
// cell space inside B-tree pages (BTS_OVERWRITE) but does not zero freed
// freelist pages (src/btree.c:6864-6874).
func secureDeleteFastVariant(ctx context.Context, r recipe.Recipe, dir string) (fileDiff, unusedReport) {
	batchVariant(ctx, r, dir, "FAST")
	on := filepath.Join(dir, "batches-"+r.JournalMode+"-secure-delete-on-one-per-step.db")
	fast := filepath.Join(dir, "batches-"+r.JournalMode+"-secure-delete-fast-one-per-step.db")
	d := compareFiles(on, fast)
	return d, classifyDifferences(on, fast, d)
}

// sessionsWithoutPage1 runs three commits that update accounts without
// dirtying page 1, comparing one connection per commit against one connection
// for all three. Because pagerWalFrames updates the change counter only when
// pList->pgno == 1 (src/pager.c:3264), both WAL files remain byte-identical.
func sessionsWithoutPage1(ctx context.Context, r recipe.Recipe, dir string) fileDiff {
	perSession := filepath.Join(dir, "page1-untouched-"+r.JournalMode+"-one-per-commit.db")
	oneSession := filepath.Join(dir, "page1-untouched-"+r.JournalMode+"-one-for-all.db")
	for _, path := range []string{perSession, oneSession} {
		buildBase(ctx, r, path)
	}
	for range page1UntouchedCommits {
		applyStatementsInSession(ctx, r, perSession, nil, touchAccountSQL)
	}
	db, err := r.Open(ctx, oneSession)
	if err != nil {
		fatal("open", err)
	}
	for range page1UntouchedCommits {
		if _, err := db.ExecContext(ctx, touchAccountSQL); err != nil {
			fatal("update", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(oneSession); err != nil {
		fatal("at rest", err)
	}
	infoHash("page1-untouched", "one per commit", hashOf(perSession).Exact)
	infoHash("page1-untouched", "one for all", hashOf(oneSession).Exact)
	return compareFiles(perSession, oneSession)
}

const (
	page1UntouchedCommits = 3
	abortedDocuments      = 10
	abortedDocumentBytes  = 12000
	committedEventSQL     = "INSERT INTO events(id, account_id, kind, amount, note) VALUES(900003, 1, 'fee', 42, 'committed')"
)

// abortedSavepointVariant compares a transaction that commits one INSERT
// against one that first inserts and rolls back large documents inside a
// SAVEPOINT. Reused freelist leaves are fetched with PAGER_GET_NOCONTENT
// (src/btree.c:6740), so ROLLBACK TO cannot restore bytes that were never
// journaled even under secure_delete=ON.
func abortedSavepointVariant(ctx context.Context, r recipe.Recipe, dir string) (fileDiff, unusedReport) {
	clean := filepath.Join(dir, "savepoint-"+r.JournalMode+"-never-run.db")
	aborted := filepath.Join(dir, "savepoint-"+r.JournalMode+"-rolled-back.db")
	for _, path := range []string{clean, aborted} {
		buildBase(ctx, r, path)
	}
	applyStatementsInSession(ctx, r, clean, nil, committedEventSQL)
	insertDocuments := fmt.Sprintf("WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < %d) INSERT INTO documents(id, account_id, title, body) SELECT 900000 + i, 1, 'aborted', unhex(replace(hex(zeroblob(%d)), '00', 'AB')) FROM n", abortedDocuments-1, abortedDocumentBytes)
	applyStatementsInSession(ctx, r, aborted, nil, "SAVEPOINT aborted", insertDocuments, "ROLLBACK TO aborted", "RELEASE aborted", committedEventSQL)
	infoHash("savepoint "+r.JournalMode, "never run", hashOf(clean).Exact)
	infoHash("savepoint "+r.JournalMode, "rolled back", hashOf(aborted).Exact)
	d := compareFiles(clean, aborted)
	return d, classifyDifferences(clean, aborted, d)
}

func runTwiceWithPlanChange(ctx context.Context, r recipe.Recipe, dir, name string, statements ...string) fileDiff {
	a := filepath.Join(dir, name+"-run1.db")
	b := filepath.Join(dir, name+"-run2.db")
	buildBase(ctx, r, a)
	applyStatementsInSession(ctx, r, a, nil, statements...)
	buildBase(ctx, r, b)
	applyStatementsInSession(ctx, r, b, []string{"PRAGMA reverse_unordered_selects=ON"}, statements...)
	return compareFiles(a, b)
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
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/1.3-recipe-rebuild/report.sql -->
```sql
-- Walkthrough of experiment 1.3. Run it with:
--   go run ./cmd/course run 1.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   recipe        the recipe and its settings, one row per key
--   plan          one row per step: how many statements of each kind it runs
--   runs          one row per rebuild of the recipe, by journal mode
--   step_hashes   one row per step and mode: sha256 of the closed file after the step
--   lookup        the step that a database built to step 12 is matched to
--   comparisons   one row per pair of files compared (id is the variant number below)
--   unused_pages  one row per differing page of the comparisons that look at the bytes
--   checks        every claim the experiment asserted (the runner lists it last)
-- info_run and info_hashes hold timings, paths and the hashes of runs that differ;
-- info.sql shows them.

CREATE TEMP VIEW diffs AS
SELECT seq, id, what, equal,
       CASE WHEN equal THEN 'identical' ELSE 'differ' END AS result,
       TRY_CAST(split_part(differing_pages, ' ', 1) AS INTEGER) AS first_page,
       CASE WHEN differing_pages IS NULL THEN 0 ELSE len(string_split(differing_pages, ' ')) END AS pages_differ,
       differing_pages, page1_bytes,
       CASE WHEN size_a = size_b THEN CAST(size_a // 4096 AS VARCHAR)
            ELSE (size_a // 4096) || ' vs ' || (size_b // 4096) END AS file_pages
FROM comparisons;

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  1.3  Does rebuilding the recipe give the same bytes, and what breaks it?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The seeded recipe builds a database in 30 steps from a fixed seed. Part A'
.print 'builds it twice, in different directories under different names, and'
.print 'compares the files after every step. Parts B to D then add one source of'
.print 'variation at a time to the recipe at step 6 and compare two runs: random()'
.print 'and the clock, scan order, transaction batches, secure_delete, the rowid'
.print 'past the maximum, connection sessions and a rolled-back SAVEPOINT.'
.print
.print 'The recipe and its settings:'

SELECT key, value FROM recipe WHERE key IN ('recipe', 'index_step') ORDER BY key;

SELECT key AS settings, unnest(range(1, len(parts) + 1)) AS n, unnest(parts) AS setting
FROM (SELECT key, string_split(value, ' ') AS parts FROM recipe WHERE key LIKE '%settings')
ORDER BY key, n;

.print
.print 'What each step does (statements by kind; step 0 creates the three tables):'

SELECT step, schema, inserts, updates, deletes, indexes FROM plan ORDER BY step;

.print
.print '── A. Rebuild the recipe twice ─────────────────────────────────────────'
.print
.print 'A1. Run 1 and run 2 build the recipe in different directories under'
.print 'different names. Per mode, the two files must have the same hash:'

SELECT mode, run, path, pages, sha256 FROM runs ORDER BY mode DESC, run;

.print
.print 'A2. The hash of the closed file after every step. Each step must leave a'
.print 'different file (a hash names one step), run 2 must repeat run 1 at every step'
.print 'and, in WAL mode, the cached list (recipe.StepHashes) must match as well.'
.print 'The second table lists the first 16 hex digits of every state; the CSV has all 64:'

SELECT mode, count(*) AS states, count(DISTINCT run1_sha256) AS distinct_hashes,
       bool_and(run1_sha256 = run2_sha256) AS run2_same,
       bool_and(run1_sha256 = cached_sha256) AS cached_same
FROM step_hashes GROUP BY mode ORDER BY mode DESC;

SELECT w.step, left(w.run1_sha256, 16) AS wal_sha256_prefix, left(d.run1_sha256, 16) AS delete_sha256_prefix
FROM step_hashes w JOIN step_hashes d USING (step)
WHERE w.mode = 'wal' AND d.mode = 'delete'
ORDER BY w.step;

.print
.print 'A3. A database built to step 12 is found again from its bytes alone:'

SELECT built_to_step, matched_step, exact AS exact_bytes FROM lookup;

.print
.print 'A4. The two journal modes build different files from the same recipe, but'
.print 'only page 1 differs. Bytes 18 and 19 are the file-format write and read'
.print 'versions (2 for WAL, 1 for rollback); 27 and 95 are the low bytes of the'
.print 'change counter and of version-valid-for:'

SELECT id, result, first_page, pages_differ, page1_bytes, file_pages, what FROM diffs WHERE id = '0' ORDER BY seq;

.print
.print '── B. One extra transaction after step 6 ──────────────────────────────'
.print
.print 'Each variant builds the recipe to step 6, runs one more transaction, and'
.print 'does it twice into fresh files. Rules 1 to 5 are the determinism rules'
.print 'these variants test.'
.print
.print 'B1. SQL random() (1) and the clock (2). current_timestamp has a one-second'
.print 'resolution, so two runs inside one second match and a run one second later'
.print 'does not:'

SELECT id, result, what FROM diffs WHERE id IN ('1', '2a', '2b') ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, file_pages FROM diffs WHERE id IN ('1', '2a', '2b') ORDER BY seq;

.print
.print 'B2. INSERT ... SELECT (3) takes its rowids from scan order. The same plan'
.print 'twice gives the same file; a different scan order changes the file, and'
.print 'ORDER BY id removes the dependence:'

SELECT id, result, what FROM diffs WHERE id LIKE '3%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, file_pages FROM diffs WHERE id LIKE '3%' ORDER BY seq;

.print
.print 'B3. Transaction batches (4). Steps 0-6 on one connection, with steps 2+3 and'
.print '4+5 each committed as one transaction instead of two. 4d compares two'
.print 'one-transaction-per-step builds that differ only in secure_delete.'

SELECT id, result, what FROM diffs WHERE id LIKE '4%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, page1_bytes, file_pages FROM diffs WHERE id LIKE '4%' ORDER BY seq;

.print
.print 'B4. Where the bytes differ in 4b (secure_delete=OFF: stale content left in'
.print 'unused space) and 4d (FAST leaves old bytes in freelist pages that ON zeroes).'
.print 'bytes_outside_unused counts differing bytes that sit in live data:'

SELECT id, page, bytes_differing, bytes_outside_unused, kind FROM unused_pages WHERE id LIKE '4%' ORDER BY id, page;

.print
.print 'B5. A rowid past the maximum (5): SQLite picks a random unused rowid:'

SELECT id, result, first_page, pages_differ, differing_pages, what FROM diffs WHERE id = '5' ORDER BY seq;

.print
.print '── C. Connection sessions ──────────────────────────────────────────────'
.print
.print 'C1. Steps 0-6 with one connection per step against one connection for all'
.print 'steps (6a, 6b). In WAL mode the change counter differs, and only it; in'
.print 'rollback mode nothing differs. 6c: three commits that never write page 1'
.print 'leave the files identical, because only a commit that writes page 1 writes'
.print 'the change counter.'

SELECT id, result, what FROM diffs WHERE id LIKE '6%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, page1_bytes, file_pages FROM diffs WHERE id LIKE '6%' ORDER BY seq;

.print
.print '── D. Rolled-back work inside a committed transaction ──────────────────'
.print
.print 'D1. Run 2 first inserts 10 documents of 12000 bytes inside a SAVEPOINT and'
.print 'rolls back to it, then commits the same INSERT as run 1. Freelist pages that'
.print 'the committed insert reuses keep the rolled-back bytes, even with'
.print 'secure_delete=ON:'

SELECT id, result, what FROM diffs WHERE id LIKE '7%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, page1_bytes, file_pages FROM diffs WHERE id LIKE '7%' ORDER BY seq;

SELECT id, page, bytes_differing, bytes_outside_unused, kind FROM unused_pages WHERE id LIKE '7%' ORDER BY id, page;
```

`course run` also prints the run-dependent values, which are not part of the stored evidence: the work directory, how long one build took, and the sha256 of the variant files whose bytes differ between runs (`random()`, the clock, the rowid).

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/1.3-recipe-rebuild/`](evidence/1.3-recipe-rebuild/), and `go run ./cmd/course check evidence ch01` compares a fresh run with them byte for byte.

<!-- include: evidence/1.3-recipe-rebuild.txt -->
```text

════════════════════════════════════════════════════════════════════════
  1.3  Does rebuilding the recipe give the same bytes, and what breaks it?
════════════════════════════════════════════════════════════════════════

The seeded recipe builds a database in 30 steps from a fixed seed. Part A
builds it twice, in different directories under different names, and
compares the files after every step. Parts B to D then add one source of
variation at a time to the recipe at step 6 and compare two runs: random()
and the clock, scan order, transaction batches, secure_delete, the rowid
past the maximum, connection sessions and a rolled-back SAVEPOINT.

The recipe and its settings:
┌────────────┬──────────────────────────────────┐
│    key     │              value               │
├────────────┼──────────────────────────────────┤
│ index_step │ 5                                │
│ recipe     │ seed=1 steps=30 journal_mode=wal │
└────────────┴──────────────────────────────────┘
┌───────────────┬───┬───────────────────────────┐
│   settings    │ n │          setting          │
├───────────────┼───┼───────────────────────────┤
│ conn_settings │ 1 │ synchronous=FULL          │
│ conn_settings │ 2 │ foreign_keys=ON           │
│ conn_settings │ 3 │ secure_delete=ON          │
│ conn_settings │ 4 │ cache_size=-2000          │
│ conn_settings │ 5 │ wal_autocheckpoint=1000   │
│ conn_settings │ 6 │ busy_timeout=5000ms       │
│ file_settings │ 1 │ page_size=4096            │
│ file_settings │ 2 │ auto_vacuum=NONE          │
│ file_settings │ 3 │ encoding=UTF-8            │
│ file_settings │ 4 │ application_id=1397836120 │
│ file_settings │ 5 │ user_version=0            │
│ file_settings │ 6 │ journal_mode=wal          │
└───────────────┴───┴───────────────────────────┘

What each step does (statements by kind; step 0 creates the three tables):
┌──────┬────────┬─────────┬─────────┬─────────┬─────────┐
│ step │ schema │ inserts │ updates │ deletes │ indexes │
├──────┼────────┼─────────┼─────────┼─────────┼─────────┤
│ 0    │ 3      │ 0       │ 0       │ 0       │ 0       │
│ 1    │ 0      │ 65      │ 6       │ 0       │ 0       │
│ 2    │ 0      │ 65      │ 3       │ 0       │ 0       │
│ 3    │ 0      │ 53      │ 5       │ 5       │ 0       │
│ 4    │ 0      │ 48      │ 6       │ 1       │ 0       │
│ 5    │ 0      │ 39      │ 5       │ 0       │ 1       │
│ 6    │ 0      │ 52      │ 3       │ 10      │ 0       │
│ 7    │ 0      │ 46      │ 4       │ 0       │ 0       │
│ 8    │ 0      │ 60      │ 2       │ 1       │ 0       │
│ 9    │ 0      │ 53      │ 4       │ 5       │ 0       │
│ 10   │ 0      │ 57      │ 5       │ 0       │ 0       │
│ 11   │ 0      │ 50      │ 4       │ 0       │ 0       │
│ 12   │ 0      │ 42      │ 5       │ 11      │ 0       │
│ 13   │ 0      │ 50      │ 5       │ 0       │ 0       │
│ 14   │ 0      │ 58      │ 4       │ 0       │ 0       │
│ 15   │ 0      │ 49      │ 3       │ 9       │ 0       │
│ 16   │ 0      │ 48      │ 5       │ 1       │ 0       │
│ 17   │ 0      │ 67      │ 5       │ 0       │ 0       │
│ 18   │ 0      │ 44      │ 6       │ 7       │ 0       │
│ 19   │ 0      │ 54      │ 3       │ 0       │ 0       │
│ 20   │ 0      │ 40      │ 3       │ 1       │ 0       │
│ 21   │ 0      │ 45      │ 6       │ 5       │ 0       │
│ 22   │ 0      │ 48      │ 5       │ 0       │ 0       │
│ 23   │ 0      │ 50      │ 3       │ 0       │ 0       │
│ 24   │ 0      │ 39      │ 4       │ 7       │ 0       │
│ 25   │ 0      │ 49      │ 5       │ 0       │ 0       │
│ 26   │ 0      │ 56      │ 3       │ 0       │ 0       │
│ 27   │ 0      │ 39      │ 4       │ 9       │ 0       │
│ 28   │ 0      │ 61      │ 5       │ 1       │ 0       │
│ 29   │ 0      │ 44      │ 4       │ 0       │ 0       │
│ 30   │ 0      │ 67      │ 4       │ 9       │ 0       │
└──────┴────────┴─────────┴─────────┴─────────┴─────────┘

── A. Rebuild the recipe twice ─────────────────────────────────────────

A1. Run 1 and run 2 build the recipe in different directories under
different names. Per mode, the two files must have the same hash:
┌────────┬─────┬─────────────────────────────────┬───────┬──────────────────────────────────────────────────────────────────┐
│  mode  │ run │              path               │ pages │                              sha256                              │
├────────┼─────┼─────────────────────────────────┼───────┼──────────────────────────────────────────────────────────────────┤
│ wal    │ 1   │ run-1/recipe.db                 │ 270   │ 24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74 │
│ wal    │ 2   │ run-2/elsewhere/another-name.db │ 270   │ 24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74 │
│ delete │ 1   │ run-1/recipe-delete.db          │ 270   │ 0b5b4111fb9946325ad7ffa859b68b1394b04f824a274b0d770697bd16b71d57 │
│ delete │ 2   │ run-2/recipe-delete.db          │ 270   │ 0b5b4111fb9946325ad7ffa859b68b1394b04f824a274b0d770697bd16b71d57 │
└────────┴─────┴─────────────────────────────────┴───────┴──────────────────────────────────────────────────────────────────┘

A2. The hash of the closed file after every step. Each step must leave a
different file (a hash names one step), run 2 must repeat run 1 at every step
and, in WAL mode, the cached list (recipe.StepHashes) must match as well.
The second table lists the first 16 hex digits of every state; the CSV has all 64:
┌────────┬────────┬─────────────────┬───────────┬─────────────┐
│  mode  │ states │ distinct_hashes │ run2_same │ cached_same │
├────────┼────────┼─────────────────┼───────────┼─────────────┤
│ wal    │ 31     │ 31              │ true      │ true        │
│ delete │ 31     │ 31              │ true      │             │
└────────┴────────┴─────────────────┴───────────┴─────────────┘
┌──────┬───────────────────┬──────────────────────┐
│ step │ wal_sha256_prefix │ delete_sha256_prefix │
├──────┼───────────────────┼──────────────────────┤
│ 0    │ 7470585af706eb3d  │ e551bbfca2c05cd9     │
│ 1    │ 6d8cfc77d5f514b2  │ 6207fb74284d25e8     │
│ 2    │ 69490a2dc9c1980f  │ b8a0bd0796442099     │
│ 3    │ 329eb15614fee435  │ 0ef90bd0e64af1e3     │
│ 4    │ e6aa5f4e37fde0be  │ b4a4adf14dbe4826     │
│ 5    │ 4078a0bbc3758ed2  │ 704dd8125826662d     │
│ 6    │ 2d7996f5c596c99e  │ 575d9182d7fb0469     │
│ 7    │ 7d8ae978254856c0  │ c4574af084896634     │
│ 8    │ f27921ff12ecd673  │ 20bf39d8a49cf840     │
│ 9    │ ca22a3f016ee31e4  │ ecf8f6d400de9601     │
│ 10   │ 044c77909ad64db8  │ ade615114d37b8dc     │
│ 11   │ 6ced94850b7f17c8  │ 67f0fa2bb53de1ba     │
│ 12   │ 649990133c619869  │ 239d48653ebbd393     │
│ 13   │ cf469938e5222774  │ ce74350fd7e57b7f     │
│ 14   │ e7f84f8b8cd28ab5  │ 08ed8fb6eb9befea     │
│ 15   │ 52519800be453b84  │ 8fe1be42a8e27b29     │
│ 16   │ a672af8472ba30db  │ fc65fbb4f7617732     │
│ 17   │ 4a1f971701e2b5cd  │ ea538ee4b2f31442     │
│ 18   │ 055a4f7a088f1c76  │ 443f6584e34022e7     │
│ 19   │ e57cb4d4d450456a  │ d14c21fe37761265     │
│ 20   │ e769f5c3b1663de2  │ 95258618e9e31028     │
│ 21   │ a965f7823f589a7b  │ bf6c17cf88f35e54     │
│ 22   │ 3b125736931aa27c  │ 88534c0275b2ff71     │
│ 23   │ 92a22043ecd9caaf  │ 6fed2ab1918e3b14     │
│ 24   │ 46cb974cc3fe3c9e  │ 9acc88a8e8868779     │
│ 25   │ d0996ac06958dd0c  │ ac55f82d5457478d     │
│ 26   │ 13bd55ed7dc6a435  │ 9d9bf656076f8c1b     │
│ 27   │ f1eba6ade98188aa  │ e53e8aeb3871210d     │
│ 28   │ c7df2bec238b874e  │ ee008d3d9d9ddccd     │
│ 29   │ d1c0ab40c8901553  │ 752f34c38f1c7f76     │
│ 30   │ 24aa158fdf713808  │ 0b5b4111fb994632     │
└──────┴───────────────────┴──────────────────────┘

A3. A database built to step 12 is found again from its bytes alone:
┌───────────────┬──────────────┬─────────────┐
│ built_to_step │ matched_step │ exact_bytes │
├───────────────┼──────────────┼─────────────┤
│ 12            │ 12           │ true        │
└───────────────┴──────────────┴─────────────┘

A4. The two journal modes build different files from the same recipe, but
only page 1 differs. Bytes 18 and 19 are the file-format write and read
versions (2 for WAL, 1 for rollback); 27 and 95 are the low bytes of the
change counter and of version-valid-for:
┌────┬────────┬────────────┬──────────────┬─────────────┬────────────┬────────────────────────────────────────────────┐
│ id │ result │ first_page │ pages_differ │ page1_bytes │ file_pages │                      what                      │
├────┼────────┼────────────┼──────────────┼─────────────┼────────────┼────────────────────────────────────────────────┤
│ 0  │ differ │ 1          │ 1            │ 18 19 27 95 │ 270        │ WAL build vs rollback build of the same recipe │
└────┴────────┴────────────┴──────────────┴─────────────┴────────────┴────────────────────────────────────────────────┘

── B. One extra transaction after step 6 ──────────────────────────────

Each variant builds the recipe to step 6, runs one more transaction, and
does it twice into fresh files. Rules 1 to 5 are the determinism rules
these variants test.

B1. SQL random() (1) and the clock (2). current_timestamp has a one-second
resolution, so two runs inside one second match and a run one second later
does not:
┌────┬───────────┬──────────────────────────────────────────────────────────┐
│ id │  result   │                           what                           │
├────┼───────────┼──────────────────────────────────────────────────────────┤
│ 1  │ differ    │ SQL random() in a value                                  │
│ 2a │ identical │ current_timestamp, two runs inside one wall-clock second │
│ 2b │ differ    │ current_timestamp, third run one second later            │
└────┴───────────┴──────────────────────────────────────────────────────────┘
┌────┬────────────┬──────────────┬─────────────────┬────────────┐
│ id │ first_page │ pages_differ │ differing_pages │ file_pages │
├────┼────────────┼──────────────┼─────────────────┼────────────┤
│ 1  │ 59         │ 1            │ 59              │ 60         │
│ 2a │            │ 0            │                 │ 60         │
│ 2b │ 59         │ 1            │ 59              │ 60         │
└────┴────────────┴──────────────┴─────────────────┴────────────┘

B2. INSERT ... SELECT (3) takes its rowids from scan order. The same plan
twice gives the same file; a different scan order changes the file, and
ORDER BY id removes the dependence:
┌────┬───────────┬───────────────────────────────────────────────────────────┐
│ id │  result   │                           what                            │
├────┼───────────┼───────────────────────────────────────────────────────────┤
│ 3a │ identical │ INSERT ... SELECT without ORDER BY, same plan twice       │
│ 3b │ differ    │ same statement, run 2 with reverse_unordered_selects=ON   │
│ 3c │ identical │ with ORDER BY id, run 2 with reverse_unordered_selects=ON │
└────┴───────────┴───────────────────────────────────────────────────────────┘
┌────┬────────────┬──────────────┬─────────────────┬────────────┐
│ id │ first_page │ pages_differ │ differing_pages │ file_pages │
├────┼────────────┼──────────────┼─────────────────┼────────────┤
│ 3a │            │ 0            │                 │ 60         │
│ 3b │ 29         │ 1            │ 29              │ 60         │
│ 3c │            │ 0            │                 │ 60         │
└────┴────────────┴──────────────┴─────────────────┴────────────┘

B3. Transaction batches (4). Steps 0-6 on one connection, with steps 2+3 and
4+5 each committed as one transaction instead of two. 4d compares two
one-transaction-per-step builds that differ only in secure_delete.
┌────┬───────────┬─────────────────────────────────────────────────────────┐
│ id │  result   │                          what                           │
├────┼───────────┼─────────────────────────────────────────────────────────┤
│ 4a │ identical │ WAL, recipe settings (secure_delete=ON)                 │
│ 4b │ differ    │ WAL, secure_delete=OFF                                  │
│ 4c │ differ    │ rollback, secure_delete=ON                              │
│ 4d │ differ    │ WAL, one transaction per step: secure_delete=FAST vs ON │
└────┴───────────┴─────────────────────────────────────────────────────────┘
┌────┬────────────┬──────────────┬─────────────────┬─────────────┬────────────┐
│ id │ first_page │ pages_differ │ differing_pages │ page1_bytes │ file_pages │
├────┼────────────┼──────────────┼─────────────────┼─────────────┼────────────┤
│ 4a │            │ 0            │                 │             │ 60         │
│ 4b │ 24         │ 2            │ 24 28           │             │ 60         │
│ 4c │ 1          │ 1            │ 1               │ 27 95       │ 60         │
│ 4d │ 29         │ 3            │ 29 36 37        │             │ 60         │
└────┴────────────┴──────────────┴─────────────────┴─────────────┴────────────┘

B4. Where the bytes differ in 4b (secure_delete=OFF: stale content left in
unused space) and 4d (FAST leaves old bytes in freelist pages that ON zeroes).
bytes_outside_unused counts differing bytes that sit in live data:
┌────┬──────┬─────────────────┬──────────────────────┬────────────────────────────────────────────────┐
│ id │ page │ bytes_differing │ bytes_outside_unused │                      kind                      │
├────┼──────┼─────────────────┼──────────────────────┼────────────────────────────────────────────────┤
│ 4b │ 24   │ 357             │ 0                    │ b-tree page, unallocated space and freeblocks  │
│ 4b │ 28   │ 1207            │ 0                    │ b-tree page, unallocated space and freeblocks  │
│ 4d │ 29   │ 4079            │ 0                    │ freelist leaf page                             │
│ 4d │ 36   │ 4067            │ 0                    │ freelist trunk page, bytes after the leaf list │
│ 4d │ 37   │ 4078            │ 0                    │ freelist leaf page                             │
└────┴──────┴─────────────────┴──────────────────────┴────────────────────────────────────────────────┘

B5. A rowid past the maximum (5): SQLite picks a random unused rowid:
┌────┬────────┬────────────┬──────────────┬─────────────────┬──────────────────────────────────────────────┐
│ id │ result │ first_page │ pages_differ │ differing_pages │                     what                     │
├────┼────────┼────────────┼──────────────┼─────────────────┼──────────────────────────────────────────────┤
│ 5  │ differ │ 29         │ 1            │ 29              │ insert without an id after the maximum rowid │
└────┴────────┴────────────┴──────────────┴─────────────────┴──────────────────────────────────────────────┘

── C. Connection sessions ──────────────────────────────────────────────

C1. Steps 0-6 with one connection per step against one connection for all
steps (6a, 6b). In WAL mode the change counter differs, and only it; in
rollback mode nothing differs. 6c: three commits that never write page 1
leave the files identical, because only a commit that writes page 1 writes
the change counter.
┌────┬───────────┬────────────────────────────────────────────────────────────────────────────┐
│ id │  result   │                                    what                                    │
├────┼───────────┼────────────────────────────────────────────────────────────────────────────┤
│ 6a │ differ    │ WAL, one connection per step vs one for all steps                          │
│ 6b │ identical │ rollback, the same comparison                                              │
│ 6c │ identical │ WAL, 3 commits that never write page 1: one connection each vs one for all │
└────┴───────────┴────────────────────────────────────────────────────────────────────────────┘
┌────┬────────────┬──────────────┬─────────────────┬─────────────┬────────────┐
│ id │ first_page │ pages_differ │ differing_pages │ page1_bytes │ file_pages │
├────┼────────────┼──────────────┼─────────────────┼─────────────┼────────────┤
│ 6a │ 1          │ 1            │ 1               │ 27 95       │ 60         │
│ 6b │            │ 0            │                 │             │ 60         │
│ 6c │            │ 0            │                 │             │ 60         │
└────┴────────────┴──────────────┴─────────────────┴─────────────┴────────────┘

── D. Rolled-back work inside a committed transaction ──────────────────

D1. Run 2 first inserts 10 documents of 12000 bytes inside a SAVEPOINT and
rolls back to it, then commits the same INSERT as run 1. Freelist pages that
the committed insert reuses keep the rolled-back bytes, even with
secure_delete=ON:
┌────┬────────┬────────────────────────────┐
│ id │ result │            what            │
├────┼────────┼────────────────────────────┤
│ 7a │ differ │ WAL, secure_delete=ON      │
│ 7b │ differ │ rollback, secure_delete=ON │
└────┴────────┴────────────────────────────┘
┌────┬────────────┬──────────────┬─────────────────┬─────────────┬────────────┐
│ id │ first_page │ pages_differ │ differing_pages │ page1_bytes │ file_pages │
├────┼────────────┼──────────────┼─────────────────┼─────────────┼────────────┤
│ 7a │ 1          │ 3            │ 1 29 37         │ 27 95       │ 60         │
│ 7b │ 29         │ 2            │ 29 37           │             │ 60         │
└────┴────────────┴──────────────┴─────────────────┴─────────────┴────────────┘
┌────┬──────┬─────────────────┬──────────────────────┬───────────────────────────────────────────────┐
│ id │ page │ bytes_differing │ bytes_outside_unused │                     kind                      │
├────┼──────┼─────────────────┼──────────────────────┼───────────────────────────────────────────────┤
│ 7a │ 1    │ 2               │ 2                    │ b-tree page, unallocated space and freeblocks │
│ 7a │ 29   │ 4092            │ 0                    │ freelist leaf page                            │
│ 7a │ 37   │ 4093            │ 0                    │ freelist leaf page                            │
│ 7b │ 29   │ 4092            │ 0                    │ freelist leaf page                            │
│ 7b │ 37   │ 4093            │ 0                    │ freelist leaf page                            │
└────┴──────┴─────────────────┴──────────────────────┴───────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  two builds of the recipe in different directories under different names are byte-identical
A  ✓  the per-step hash lists of the two runs are identical (31 states)
A  ✓  every step leaves a different file, so a hash names exactly one step
A  ✓  recipe.StepHashes (the cached list later chapters use) equals the list just built
A  ✓  recipe.MatchStep names the step a database equals
A  ✓  the rollback-mode recipe also rebuilds byte for byte at every step
B  ✓  SQL random() makes two runs differ
B  ✓  current_timestamp makes runs differ once the wall clock's second changes (its resolution is one second, so two fast runs can still match)
B  ✓  FINDING: an unordered INSERT ... SELECT run twice on the same build with the same plan gives identical files
B  ✓  an unordered INSERT ... SELECT changes the file when the scan order changes; ORDER BY removes the dependence
B  ✓  FINDING: in WAL mode on one connection with secure_delete=ON, different transaction batches give identical files
B  ✓  with secure_delete=OFF different batches differ, and only in bytes no cell uses (stale content left in unallocated space, freeblocks or freelist pages)
B  ✓  in rollback mode different batches differ, and with secure_delete=ON only in the change counter (it counts commits)
B  ✓  secure_delete=FAST is not ON: on the same history FAST leaves old bytes in the unused space of freelist pages that ON zeroes
B  ✓  rowid assignment past the maximum rowid makes two runs differ
C  ✓  FINDING: in WAL mode the number of connection sessions changes the file, and only the change counter (bytes 24-27 and 92-95)
C  ✓  in rollback mode the number of connection sessions does not change the file
C  ✓  in WAL mode only a commit that writes page 1 writes the change counter: sessions whose commits never touch page 1 leave the file identical
D  ✓  WAL mode: a rolled-back SAVEPOINT changes the committed file even with secure_delete=ON: reused freelist leaf pages keep the rolled-back bytes, and page 1's change counter moves
D  ✓  rollback mode: a rolled-back SAVEPOINT leaves the rolled-back bytes in reused freelist leaf pages (secure_delete=ON does not clear them)

20 of 20 claims hold

Experiment verdict: RECIPE REBUILD OK
```

**Reading the R0 evidence (opening tables and section A):**

- **Opening tables (recipe plan).** Show the persistent file settings, per-connection settings, and all 31 step summaries (step 0 with 3 `CREATE TABLE` statements; steps 1–30 with inserts, updates, periodic deletes, and the single `CREATE INDEX` at step 5).
- **A1 (two independent builds).** Building the 30-step WAL recipe into `run-1/recipe.db` and `run-2/elsewhere/another-name.db` takes under a second (about `87ms` in one lab run; the time is run-dependent, printed by `course run` and not stored) and produces `270` pages (`1,105,920` bytes) with identical SHA-256 `24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74` (step 0: `7470585af706eb3de499f126d74bdc98396e1e9853ee11643e2e85f61ef0cdb9`; A2 shows the first 16 hex digits of each step, and the stored table has all 64). Directory path and file name do not enter the database bytes.
- **A2–A3 (per-step hashes and lookup).** All 31 per-step hashes match identically between run 1 and run 2, all 31 are distinct, and `recipe.StepHashes` matches the fresh build. A separate database built to step 12 is identified by `recipe.MatchStep` as step `12` with exact bytes.
- **A1 and A4 (WAL vs rollback).** The same recipe in rollback mode (`journal_mode=delete`) also rebuilds byte for byte at every step (`0b5b4111fb9946325ad7ffa859b68b1394b04f824a274b0d770697bd16b71d57`). Comparing the closed WAL file against the closed rollback file shows only 4 differing bytes in the entire `1,105,920`-byte database, all on page 1: bytes `18` and `19` (file format write/read versions: `2` for WAL vs `1` for rollback) and bytes `27` and `95` (the low byte of the 4-byte file change counter and version-valid-for field).

---

## 6. Evidence and failure cases

The second half of Experiment 1.3 (sections B–D of `evidence/1.3-recipe-rebuild.txt`) tests seven sources of physical variation against a 6-step WAL base database, plus two boundary checks (`secure_delete=FAST` vs `ON`, and WAL sessions whose commits never write page 1). Every test starts from fresh files and reports the first differing page (and differing page-1 byte offsets when page 1 changes). Three checks uncovered subtler behavior than a first guess predicts; the checks list marks those `FINDING:`.

1. **SQL `random()` in a value (B1, variant 1).** Inserting `random()` into `events.amount` makes run 1 and run 2 differ on page `59` (`1` page). SQLite's internal PRNG is seeded from the OS on first use, so any SQL `random()` or `randomblob()` breaks byte reproducibility.
2. **`current_timestamp` and time zones (B1, variants 2a–2b).** `current_timestamp` has one-second resolution. Two runs inside the same wall-clock second produce `identical` files (2a); a third run after the second advances differs on page `59` (2b). Fast tests can mask a clock dependency unless they span a second boundary. Environment-dependent modifiers sit in the same class: even on a fixed timestamp literal, SQLite's `'localtime'` modifier produces different bytes on page `59` under `TZ=UTC` versus `TZ=America/New_York` (`DET-08e`).
3. **Unordered `INSERT ... SELECT` (B2).**
   - Running `INSERT INTO archive(account_id, amount) SELECT account_id, amount FROM events WHERE amount > 0` twice on the same build with the same schema yields `identical` files (`FINDING`, 3a), because the planner picks the same scan order both times and assigns rowids in that order.
   - Setting `PRAGMA reverse_unordered_selects=ON` on run 2 reverses the scan order without changing query semantics, and the two files differ on page `29` (3b). Adding an index, running `ANALYZE`, or upgrading SQLite can flip an unordered scan the same way.
   - Adding `ORDER BY id` makes both files `identical` again even under `reverse_unordered_selects=ON` (3c).
4. **Transaction batching and `secure_delete` modes (B3–B4).** Steps 0–6 run on one connection, comparing one transaction per step against merging steps `2+3` and `4+5` into single transactions (step 3 deletes rows inserted in step 2):
   - **WAL mode, `secure_delete=ON` (recipe settings):** `identical` (`FINDING`, 4a). One WAL connection does not increment the header change counter per commit, and `secure_delete=ON` zeroes freed bytes inside B-tree pages and on freed freelist pages.
   - **WAL mode, `secure_delete=OFF`:** `differ`, first differing page `24`, `2` pages differ (`24 28`) (4b in B3 and B4): `357` bytes on page `24` and `1,207` bytes on page `28`, every byte inside unused space on B-tree leaf pages (the unallocated gap between the cell-pointer array and the cell content area, or freeblocks—not freelist pages). Without `secure_delete`, deleted cell payloads leave history-dependent stale bytes.
   - **Rollback mode, `secure_delete=ON`:** `differ`, first differing page `1`, `1` page differs, page 1 bytes `27 95` (4c), because rollback mode increments the change counter on every commit.
   - **WAL mode, `secure_delete=FAST` vs `secure_delete=ON` (4d in B3 and B4):** `secure_delete` must be strictly `ON`, not `FAST`. On the exact same one-transaction-per-step history, `FAST` differs from `ON` on 3 pages `[29 36 37]`: page `29` (`4,079` bytes, freelist leaf), page `36` (`4,067` bytes, freelist trunk bytes after the leaf list), and page `37` (`4,078` bytes, freelist leaf)—all in unused space. In [`freePage2` (`src/btree.c:6864-6874`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6864-L6874), SQLite zeroes freed pages only when `BTS_SECURE_DELETE` (`ON`) is set; [`sqlite3BtreeSecureDelete` (`src/btree.c:3181-3194`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3181-L3194) maps `FAST` to `BTS_OVERWRITE` only, which zeroes freed cell space inside B-tree pages while leaving old bytes on freed freelist pages. At full 30-step scale, `FAST` also makes 1-transaction vs 31-transaction batchings diverge again (`DET-05d`: `2` pages differ, first page `29`).
5. **Omitting a rowid after the maximum 64-bit rowid `9223372036854775807` (B5).** Once a table holds rowid `9223372036854775807` (`2^63 - 1`), SQLite cannot increment past it and picks a random unused rowid for the next insert without an explicit id. The two runs differ on page `29`.
6. **Connection sessions, page-1 writes, cache eviction, and pool churn in WAL mode (C1).**
   - **Steps 0–6 (every step dirties page 1 via `PRAGMA user_version = k`):** one connection per step vs one connection for all 7 steps gives `differ`, first differing page `1`, `1` page differs, page 1 bytes `27 95` in WAL mode (`FINDING`, 6a), and `identical` in rollback mode (6b).
   - **Commits that never write page 1 (6c):** starting from step 6, 3 commits of `UPDATE accounts SET balance = balance + 1 WHERE id = 1` with one connection per commit vs one connection for all three leave `identical` files even in WAL mode. [`pagerWalFrames` (`src/pager.c:3264`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3264) guards the counter write with `if( pList->pgno==1 ) pager_write_changecounter(pList)`. When page 1 is dirty, [`pager_write_changecounter` (`src/pager.c:3130-3143`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3130-L3143) writes `dbFileVers + 1` (line 3135), and `dbFileVers` is refreshed only when page 1 is read through `readDbPage` ([`src/pager.c:3109-3110`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3109-L3110))—either on a new connection or when page 1 is evicted from cache and reloaded.
   - **Cache eviction and pool churn:** because page-1 eviction reloads `dbFileVers`, `PRAGMA cache_size=0` shifts only the WAL change counter deterministically from step 0 onward (`DET-02`: `walZero=cs=0(exact=false,masked=true)`, while all nine non-zero `cache_size` values tested—`1`, `2`, `5`, `10`, `50`, `-10`, `-100`, `-2000`, `-64000`—matched verbatim in WAL mode, and all ten matched in rollback mode). Likewise, leaving `SingleConnection=false` lets a concurrent read open a second pooled `database/sql` connection with its own `dbFileVers`, shifting page 1 bytes `27` and `95` (`DET-08d`, `masked=true`). Both `cache_size` and `SingleConnection=true` are part of the determinism contract.
7. **Rolled-back `SAVEPOINT` inside a committed transaction (D1).** A **savepoint** is a named nested rollback point inside a larger transaction. Starting from step 6, one transaction commits one `INSERT` into `events`; run 2 first inserts `10` documents of `12,000` bytes (`0xAB`) inside `SAVEPOINT aborted`, executes `ROLLBACK TO aborted` and `RELEASE aborted`, and commits the same `INSERT`:
   - **WAL mode, `secure_delete=ON` (7a):** `differ`, first differing page `1`, `3` pages differ (`1 29 37`), page 1 bytes `27 95`. Pages `29` (`4,092` bytes) and `37` (`4,093` bytes) are freelist leaf pages retaining bytes from the aborted inserts; page 1's change counter also moved because the aborted allocation dirtied page 1's freelist count.
   - **Rollback mode, `secure_delete=ON` (7b):** `differ`, first differing page `29`, `2` pages differ (`29 37`), leaving the same `4,092` and `4,093` bytes on those two freelist leaf pages.
   - **Why:** when SQLite allocates a freelist leaf page, [`allocateBtreePage` (`src/btree.c:6740`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6740) fetches it with `PAGER_GET_NOCONTENT`, skipping journaling of its prior content. `ROLLBACK TO` returns the page to the freelist but cannot restore bytes it never saved, and `secure_delete=ON` does not zero those rolled-back leaves. By contrast, rolling back an entire transaction (a failed `ApplyStep` hitting a primary-key conflict) leaves the closed database file byte-identical (`API-05`).

### Determinism rules verified by Experiment 1.3

| Source of variation | Rule enforced in `internal/recipe` and `internal/sqlitex` | Evidence |
| --- | --- | --- |
| SQLite build & compile options | Pin driver and CLI to one Fossil check-in; verify `sqlite_source_id()`, linked modules, and storage-affecting compile options (e.g. `ENABLE_STAT4` if `ANALYZE` runs) | Exp 1.1; Exp 1.2 (`ANALYZE` limit, section D); header bytes 96–99 |
| Persistent & per-connection PRAGMAs (including `cache_size`) | Set `page_size`, `auto_vacuum`, `encoding`, `application_id`, `journal_mode` at creation; set `synchronous`, `foreign_keys`, `secure_delete`, `cache_size` (non-zero), `wal_autocheckpoint`, `busy_timeout` on every open | Exp 1.3 opening tables, A4; `DET-02` (`cache_size=0` shifts WAL counter) |
| Randomness | Generate values in Go from a seeded `math/rand/v2` PCG; ban SQL `random()` and `randomblob()` | Exp 1.3 Variant 1 (page 59 differs) |
| Wall clock & time zone | Pass timestamps as bound parameters; ban `current_timestamp`, `date('now')`, and environment-dependent modifiers such as `'localtime'` | Exp 1.3 Variant 2 (page 59 differs after 1 s); `DET-08e` (`'localtime'` varies with `TZ`) |
| Query-plan row order | Always add `ORDER BY` to `INSERT ... SELECT` | Exp 1.3 Variant 3 (page 29 differs under reversed scan; identical with `ORDER BY id`) |
| Stale deleted bytes | Set `PRAGMA secure_delete=ON` (strictly `ON`, not `OFF` or `FAST`) whenever different histories or batchings must match | Exp 1.3 Variant 4 (pages 24, 28 differ when `OFF`; pages 29, 36, 37 differ in `FAST` vs `ON`); `DET-05d` |
| Rowid selection | Declare explicit `INTEGER PRIMARY KEY` and supply every id | Exp 1.3 Variant 5 (page 29 differs past `9223372036854775807`) |
| WAL connection sessions & pool churn | Set `SingleConnection=true` and fix session boundaries for commits that write page 1, or compare with bytes 24–27 and 92–95 masked and agreeing (`MatchStep`) | Exp 1.3 Variant 6 (page 1 bytes 27, 95 differ when page 1 is written; page-1-untouched sessions identical); `DET-08d` |
| Aborted work inside a committed transaction | Ban rolled-back `SAVEPOINT`s inside recipe transactions | Exp 1.3 Variant 7 (reused freelist leaves 29, 37 keep rolled-back bytes in both modes, plus page 1 counter in WAL) |

### What attacked this chapter

<!-- adversarial-results -->

Four adversarial programs (`48` attacks total, driven by [`run-all.sh`](../agent_work/ch01/adversarial/run-all.sh)) attacked this chapter's claims and shared `internal/recipe` package against the pinned SQLite 3.53.4 build:

- [`pin-falsify`](../agent_work/ch01/adversarial/pin-falsify/main.go) (`PIN-01`–`PIN-08`) attacked pin enforcement across Experiments 1.1–1.3 by mutating `sqlite/PIN` (`fossil_id` and `driver_version`), spoofing `SQLITE3_BIN` with wrong source IDs, stripped `ENABLE_DBPAGE_VTAB`, and drifted `DEFAULT_PAGE_SIZE`, and checking raw `modernc.org/sqlite` runtime defaults.
- [`twin-io`](../agent_work/ch01/adversarial/twin-io/main.go) (`TWIN-01`–`TWIN-12`) attacked Experiment 1.2's Go-vs-CLI twin check across steady-state WAL commits (`FULL` and `NORMAL`), 10 KiB overflow inserts, overflow deletes, explicit rollbacks, `CREATE INDEX`, `wal_checkpoint(TRUNCATE)`, `VACUUM`, `ANALYZE`, `PRAGMA mmap_size`, and a must-fail `FULL`-vs-`NORMAL` mismatch.
- [`recipe-determinism`](../agent_work/ch01/adversarial/recipe-determinism/main.go) (`DET-01`–`DET-08e`) attacked Experiment 1.3's R0 reproducibility across 50 concurrent 30-step builds (varied working directories, spaces, Unicode, symlinks, CPU and disk load), 10 `cache_size` values, 11 other per-connection pragmas, `locking_mode=EXCLUSIVE`, `truncate`/`persist` rollback journals, a full 30-step Go-vs-CLI rebuild, extreme WAL batchings (1 transaction, ~1,700 autocommit transactions, `wal_autocheckpoint=1`, `secure_delete=FAST`), page-1-untouched WAL sessions, a concurrent reader pinning checkpoints, rolled-back `SAVEPOINT`s, `ANALYZE`, `database/sql` pool churn, and `TZ` with `'localtime'`.
- [`recipe-api-abuse`](../agent_work/ch01/adversarial/recipe-api-abuse/main.go) (`API-01`–`API-09`) attacked `internal/recipe` with single-bit file tampering, disagreeing header change counters, live WAL and orphan-journal files, malformed and tampered step-hash caches, failed mid-recipe steps, mismatched `sqlitex.Open` settings, partial schemas, and invalid `Recipe` values.

**Run 1** ([`results-run1.txt`](../agent_work/ch01/adversarial/results-run1.txt)) produced **37 HELD, 11 BROKEN**. Three broken attacks exposed code defects in `internal/recipe` that were fixed in the repair round:

- `API-02`: `MatchStep` accepted a masked-only match when header bytes 24–27 and 92–95 disagreed; fixed by `ChangeCountersAgree` (`TestMatchStepRejectsDisagreeingCounters`).
- `API-04b`: `StepHashes` trusted a syntactically valid tampered cache; fixed by cache v2's trailing checksum and fresh step-0 rebuild check (`TestStepHashesRejectsTamperedAndStaleCaches`).
- `API-09`: `Step(0)` and `Build(..., 0)` bypassed `Recipe.Validate()`; fixed to validate first (`TestInvalidRecipeIsRejectedByStepAndBuild`).

The other eight broken attacks in Run 1 narrowed overly broad claims, now demonstrated in Experiments 1.2 and 1.3 or stated in Section 6: `TWIN-10` and `DET-08c` (`ANALYZE` diverges on `ENABLE_STAT4`), `DET-05d` and `DET-08a` (`secure_delete=FAST` leaves unzeroed freelist pages and re-exposes batching divergence), `DET-06` (WAL change counter advances only on commits that dirty page 1), `DET-08b` (rolled-back `SAVEPOINT`s leave bytes on reused freelist leaves), `DET-08d` (`database/sql` pool churn shifts the WAL counter when `SingleConnection=false`), and `DET-08e` (`'localtime'` depends on `TZ`).

**Run 2** ([`results-run2.txt`](../agent_work/ch01/adversarial/results-run2.txt), after the repair round and foundation merge) produced **37 HELD, 11 BROKEN**: `API-02`, `API-04b`, and `API-09` became `HELD`; the eight narrowed-claim attacks still expected the original broad claims; and three new `BROKEN` lines appeared from the foundation's updated `MustPinCheck` and new-file pragma order:

- `PIN-04` and `DET-04` had stale expectations: `MustPinCheck` now rejects a mismatched `driver_version` on stderr before Experiment 1.1's own check runs, and `DET-04`'s CLI script still applied `application_id` before `journal_mode`.
- `DET-02` exposed a real new fact: with the updated pragma order, `cache_size=0` in WAL mode shifts the change counter deterministically from step 0 onward (`masked=true`), while all nine non-zero `cache_size` values match verbatim.

After aligning those stale and narrowed-claim attack expectations to match the verified rules, the final run ([`results.txt`](../agent_work/ch01/adversarial/results.txt)) passed **48 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every operational rule below follows directly from this chapter's experiments:

1. **Verify the exact SQLite build and compile options at service startup (Experiment 1.1).** Two binaries reporting version `3.53.4` can carry different compile options (`ENABLE_STAT4`, `DEFAULT_WAL_SYNCHRONOUS`, `ENABLE_DBPAGE_VTAB`). Check `sqlite_source_id()` and required `PRAGMA compile_options` on startup rather than trusting a release number alone.
2. **Design the platform around SQLite's two external boundaries: connections and files (Section 3, Experiment 1.2).** The compiler and VDBE live inside the library and run per connection (`sqlite3_prepare_v2` compiles statements on one handle; `sqlite3_step` auto-reprepares them on schema changes). A multi-database service cannot observe or replace those layers; it governs per-connection settings above the pager and owns files and kernel I/O below the pager.
3. **Treat `.db` plus `-wal` as the live database unit, not `.db` alone (Experiment 1.2).** In WAL mode, a committed `INSERT` makes zero `pwrite64` calls on `t.db`; the new page lives only in `t.db-wal` until a checkpoint copies it back. Copying `.db` alone while a WAL connection is open silently drops committed writes.
4. **Account for fresh-WAL creation syncs when opening and closing databases (Experiment 1.2).** Under `synchronous=FULL`, the first commit on a freshly opened WAL database pays 3 syncs (`t.db-wal` header, directory `.`, and `t.db-wal` commit frame), whereas rollback mode pays 4 syncs on every commit (`t.db-journal`, `.`, `t.db-journal`, `t.db`). Opening and closing connections on every write deletes and recreates `-wal`, paying directory syncs repeatedly.
5. **The translated Go driver (`modernc.org/sqlite`) matches the C build's storage I/O within shared compile options (Experiment 1.2, `TWIN-01`–`TWIN-12`).** Across both rollback and WAL modes, the Go driver and the C CLI execute the same writes, syncs, locks, reads, unlinks, and closes and leave byte-identical files for statements independent of differing compile options. The only syscall difference—extra `fcntl(F_SETFD)` calls from `modernc.org/libc`—touches no disk bytes or POSIX locks. Where compile options differ (`ENABLE_STAT4` on `ANALYZE`), the two builds write different tables and different files.
6. **Byte-level reproducibility requires `secure_delete=ON` and no rolled-back `SAVEPOINT`s (Experiment 1.3 Variants 4 and 7, `DET-05d`, `DET-08a`, `DET-08b`).** Both `secure_delete=OFF` and `secure_delete=FAST` leave history-dependent stale bytes in unused space (B-tree leaves and freelist pages), and rolled-back `SAVEPOINT`s leave bytes in reused freelist leaf pages even when `secure_delete=ON`.
7. **Standardize session boundaries, `SingleConnection=true`, and non-zero `cache_size`—or mask the 8 counter bytes with agreement checked (Experiment 1.3 Variant 6, `DET-02`, `DET-08d`, `API-02`).** In WAL mode, bytes 24–27 and 92–95 advance only when a commit dirties page 1 after page 1 was read from storage (new connection, pool churn under `SingleConnection=false`, or cache eviction under `cache_size=0`). Any byte-level verifier must either fix those boundaries or mask bytes 24–27 and 92–95 after confirming the two fields agree.

| Operational rule | Supporting evidence |
| --- | --- |
| Check `sqlite_source_id()` and required `PRAGMA compile_options` on startup | Experiment 1.1; Experiment 1.2 (`ANALYZE` / `ENABLE_STAT4`); `PIN-01`–`PIN-07` |
| Control per-connection settings above the pager and files below it | Section 3; Experiment 1.2 |
| Pair `.db` and `-wal` while any WAL connection is open | Experiment 1.2 (0 `pwrite64` on `t.db` in WAL mode) |
| Avoid open/close churn that recreates `-wal` and pays directory syncs | Experiment 1.2 (3 syncs on fresh WAL vs 1 steady-state sync in `TWIN-01`) |
| Rely on `modernc.org/sqlite` I/O parity only within shared compile options | Experiment 1.2 twin check & `ANALYZE` limit; `TWIN-01`–`TWIN-12` |
| Use `secure_delete=ON` (not `OFF` or `FAST`) and ban rolled-back `SAVEPOINT`s for E1 rebuilds | Experiment 1.3 Variants 4 and 7; `DET-05d`, `DET-08a`, `DET-08b` |
| Pin `SingleConnection=true` and non-zero `cache_size`, or verify masked hashes with agreeing counters | Experiment 1.3 Variant 6; `DET-02`, `DET-08d`, `API-02` |

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In `prepare.c` (`sqlite3_prepare_v2`) and `vdbeapi.c` (`sqlite3_step`), what happens if another connection alters a table's schema between `Prepare` and `Step`, and which flag makes that recovery possible?
2. In Experiment 1.2, how many `pwrite64` calls hit `t.db` during the committed `INSERT` in rollback mode versus WAL mode, and where do the new row's bytes go in each mode?
3. Why did the Go driver issue more syscalls than the C CLI in Experiment 1.2's twin check (33 vs 31 in rollback mode, 18 vs 17 in WAL mode), why does that difference not affect the database file or locking, and what statement still makes the two builds diverge?
4. Experiment 1.3 ran the same 7 recipe steps (0–6) in WAL mode once with a fresh connection per step and once on a single long-lived connection. Which exact bytes differed, why did the same comparison produce `identical` files in rollback mode, and when do multiple WAL sessions still leave `identical` files?
5. In Experiment 1.3 variant 4 (merging steps 2+3 and 4+5 into single transactions in WAL mode), why did `secure_delete=ON` produce `identical` files while `secure_delete=OFF` differed on pages 24 and 28, and why is `secure_delete=FAST` not equivalent to `ON`?

# Chapter 8 — Connection and database lifecycle

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `main.c`, `prepare.c`, `status.c` |
| Linked C sources (in the pinned tree, not copied) | `src/btree.c`, `src/global.c`, `src/os_unix.c`, `src/pager.c`, `src/pcache1.c`, `src/pragma.c`, `src/wal.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree (for browsing the linked C files), the C tools (none of Chapter 8's experiments invokes the C `sqlite3` CLI), `strace` (Experiment 8.1's per-phase syscall breakdown), and the DuckDB CLI that presents the results.
> - **Run an experiment** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 8.1`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 8.1
>   go run ./cmd/course run 8.2
>   go run ./cmd/course run 8.3
>   ```
>   Optional flags: Experiment 8.2 accepts `-n` (default `200`, minimum `20`) and `-step` (default `3`); Experiment 8.3 accepts `-dbs` (`1000`), `-k` (`64`), `-ops` (`4000`), `-seed` (`1`), `-max-step` (`4`), `-write-percent` (`20`), `-hot-percent` (`50`), `-cache-size` (`-256`), `-sample-every` (`10`), and `-memory-every` (`100`). `course run` cannot pass them; run a flag variant directly in the container, for example `docker compose run --rm lab go run ./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager -k 32`. In the container, scratch databases and tables are written under `/work/8.1/` through `/work/8.3/` (the `lab-work` volume); run directly, under `_work/8.1/` through `_work/8.3/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager`). Run this way it prints only its verdict and leaves its tables under `_work/8.3/results/`.
> - **Linux-only parts and macOS:** Experiment 8.1's `strace` phase breakdown and Experiment 8.2's descriptor target filenames (`/proc/self/fd` symlinks) are **Linux only**; `course run` runs them in the Linux `lab` container. Run directly on macOS, Experiment 8.1 still measures `SCHEMA_USED` and runs all of Part B, Experiment 8.2 still counts descriptors from `/dev/fd` and reads memory from `ps -o rss= -p <pid>`, and Experiment 8.3 runs natively.
> - **Measurement environment:** The stored evidence in this chapter, including the `strace` traces and descriptor target names, was captured in the Debian `lab` container on `linux/arm64` (Go 1.26). The checked values are identical to the earlier `linux/amd64` capture. Timings, process memory and raw `CACHE_USED` bytes differ between runs: `course run` prints them, and they are not part of the stored evidence. The `256 KiB` per-connection uncounted-memory margin in Experiment 8.2 was measured on the earlier `linux/amd64` host. macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and cross-vetted only; nothing was run on macOS.

---

## 1. Model

An SQLite **connection** is a `sqlite3` struct allocated inside your process when you open a database file. A connection is not a lightweight handle into a shared server; it owns its own in-memory copies of everything needed to compile and run SQL against that file:

- **File descriptors** (integer handles returned by the OS `open`/`openat` syscalls): in rollback `DELETE` mode, one descriptor for the `.db` file when idle and a second for `.db-journal` during a write transaction; in **WAL** (write-ahead log) mode, one for `.db` immediately on open, plus `.db-wal` (the redo log of appended **frames**, each carrying a 24-byte header and one 4,096-byte **page** image) and `.db-shm` (the memory-mapped **wal-index** lookup table) opened at the first statement.
- **Schema cache:** the parsed in-memory `Table`, `Index`, `Trigger`, and foreign-key structures rebuilt by scanning `sqlite_schema`. It starts empty at open and is populated lazily on the first statement.
- **Page cache:** fixed-size page buffers holding recently read or modified database pages. Each connection configures its own `cache_size` limit, though `modernc.org/sqlite` is built with `SQLITE_ENABLE_MEMORY_MANAGEMENT`, which places every connection's cache into one process-wide `pcache1` group (shown in Experiment 8.2 Part C).
- **Prepared statements:** compiled bytecode programs (`Vdbe` structs) kept alive until finalized.
- **Lookaside buffer:** a per-connection fixed-size scratch slab (`1,200` bytes × `40` slots = `48 KiB` by default) used for small short-lived allocations without calling `malloc`.
- **Connection-scoped settings:** runtime flags such as `synchronous`, `foreign_keys`, `busy_timeout`, `wal_autocheckpoint`, `secure_delete`, and `cache_size` that live only inside the connection struct and reset to SQLite defaults on every new open.

None of the schema cache, prepared statements, lookaside buffer, or connection settings is shared across separate connections to different database files. When two connections in the same process open the **same** WAL file, they share one `-shm` descriptor (`unixShmNode`), and closing one connection while the other still holds a **POSIX advisory lock** (an `fcntl` byte-range lock on the `.db` file) forces `os_unix.c` to **park** the closed connection's `.db` descriptor in a `UnixUnusedFd` list rather than calling `close()`—because on Unix, closing any file descriptor to an inode releases every POSIX lock the process holds on that inode.

To serve thousands of SQLite files inside one process without exhausting **RLIMIT_NOFILE** (the OS open-descriptor limit) or **RSS** (resident set size, the RAM mapped into the process), a platform puts a bounded **LRU** (least-recently-used) connection manager (`internal/connmgr`) in front of the files. At most `K` databases stay open at once. When a request arrives for a closed database and `K` are already open, the manager evicts the least recently used idle connection, waits for SQLite's close to **checkpoint** (copy committed WAL frames back into `.db` and call **fsync** to flush dirty OS buffers to durable storage) and unlink `-wal` and `-shm`, verifies the file is **at rest**, and records its SHA-256 hash.

```text
  Application requests (IDs db-0000 .. db-0999)
                         |
                         v
  +==================================================================+
  | Bounded LRU Connection Manager (internal/connmgr, capacity K)    |
  |  - Hit:  return open *sqlitex.DB, move to front of LRU           |
  |  - Miss: if open == K, evict LRU idle -> Close -> CheckAtRest    |
  |          -> record SHA-256; verify target's recorded SHA-256     |
  |          before Open                                             |
  +==================================================================+
          |                                           |
     up to K open                              N - K closed (at rest)
          v                                           v
  +-------------------------------+         +------------------------+
  | Per-connection state (x K)    |         | Single file on disk    |
  |  - Schema cache (lazy load)   |         |  db-0412.db            |
  |  - Prepared statements        |         |  (no -wal bytes,       |
  |  - Lookaside buffer (48 KiB)  |         |   no -shm,             |
  |  - Connection PRAGMAs         |         |   no hot -journal,     |
  |  - Page cache (shared group)  |         |   no external lock)    |
  |  - FDs: .db, -wal, -shm (3)   |         |  SHA-256 recorded at   |
  +-------------------------------+         |  eviction              |
          |                                 +------------------------+
          v
  VFS (os_unix.c) -> Linux/Unix kernel (pread64, fcntl, mmap, fsync)
```

**WAL mode as the platform default.** Platform databases in this course default to `journal_mode=wal`. Every experiment in this chapter evaluates WAL mode (with rollback `DELETE` mode used only as a descriptor-count contrast in Experiment 8.2 Part A and a settings contrast in Experiment 8.1 Part B), and no exception to WAL-as-default is recommended. Two measured costs and conditions govern WAL databases at scale:

1. **Three descriptors per active database instead of one.** An idle WAL connection holds `.db`, `-wal`, and `-shm` (opened lazily on the first statement, Experiment 8.2 A–B), so a manager capped at `K` open WAL databases uses `3K` database descriptors (Experiment 8.3 A).
2. **Closed-file at-rest state is conditional, not automatic.** Closing a WAL connection leaves a single, fully checkpointed `.db` file only when every one of seven conditions holds: (1) the closing connection is the last connection in every process, (2) no `*sql.Rows`, `*sql.Tx`, or `*sql.Conn` keeps the underlying connection checked out in Go's `database/sql` pool, (3) the process was not killed before closing, (4) `SQLITE_FCNTL_PERSIST_WAL` is off, (5) the file was not moved or renamed while open, (6) `SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE` is off, and (7) the close-time checkpoint succeeds. Experiment 8.3 Part B demonstrates the first five directly; the last two are read from the pinned source. In addition, `VACUUM INTO` always writes a rollback-mode file (header bytes 18–19 = `1`, Experiment 8.1 B), so a rebuild using `VACUUM INTO` must switch the output back to WAL.

**Where this chapter sits on the Rebuild Ladder (rung R5).** Earlier chapters built **R0 (Recipe)**, **R1 (Pages)**, **R2 (Canonical form)**, and **R3 (Redo log)** (with **R4 (Live snapshot)** in Chapter 6). Chapter 8 adds **R5 (At-rest state)**: capture = the single `.db` file left after the last connection closes cleanly and passes `connmgr.CheckAtRest`; rebuild = a plain byte copy of that file; proven equal at **E1 bytes** (SHA-256 recorded at close and verified before reopen). R5 is the cheapest correct capture in the course, and Section 5 and Section 6 establish the exact conditions under which it is valid.

---

## 2. Question

What does it cost to open, hold open, and close an SQLite database, what happens when a Go service must open and query 1,000 database files, and what exact state is a WAL database in after its last connection closes?

This chapter reuses three shared packages from earlier chunks (linked here, not inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens databases with explicit file and connection settings, checks `sqlite_source_id()` against `sqlite/PIN`, wraps `sqlite3_db_status` and `SQLITE_FCNTL_PERSIST_WAL`, and manages `_work/N.M/` directories. This chapter adds [`internal/sqlitex/placement.go`](../internal/sqlitex/placement.go) (`SettingPlacements()`, `RollbackJournalModes()`, `SettingPlacementFor()`), the machine-readable table of which settings live in the file header versus per connection.
- [`internal/trace`](../internal/trace/): runs a child process under Linux `strace` and parses its syscall log (used in Experiment 8.1 Part A).
- [`internal/recipe`](../internal/recipe/): builds the deterministic R0 workload (`accounts`, `documents`, `events`, `events_by_account`), applies numbered steps, and supplies cached step hashes (`StepHashes`, `MatchHashes`, `ChangeCountersAgree`).

This chapter introduces two new shared packages—`internal/procstat` (open-descriptor counting, Linux target resolution, RSS memory reading, and `RLIMIT_NOFILE`) and `internal/connmgr` (bounded LRU connection manager and `CheckAtRest`)—both inlined in full in Section 5.

---

## 3. Call path

Every span below is verified in [`SPANS.txt`](SPANS.txt) against SQLite `version-3.53.4` in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`). Three C files are copied whole into this chapter directory and checked by [`SOURCES.sha256`](SOURCES.sha256): [`main.c`](main.c), [`prepare.c`](prepare.c), and [`status.c`](status.c). Seven other files (`src/btree.c`, `src/global.c`, `src/os_unix.c`, `src/pager.c`, `src/pcache1.c`, `src/pragma.c`, `src/wal.c`) are linked at the pinned tag.

### 3.1 Open: 100 bytes and an empty schema

1. **`sqlite3_open` and `sqlite3_open_v2` → `openDatabase`** — [`src/main.c:3722-3728`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L3722-L3728), [`src/main.c:3729-3736`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L3729-L3736), and [`src/main.c:3360-3716`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L3360-L3716) (local copy: [`main.c`](main.c)). Note that the outline's single span `3360–3736` spans all three functions: `openDatabase` ends at line 3716, followed by the two one-line wrappers. `openDatabase` parses the URI and flags (`sqlite3ParseUri`), opens the main B-tree (`sqlite3BtreeOpen`), allocates empty `Schema` structs for `main` and `temp`, registers built-in functions and automatic extensions, allocates the lookaside buffer (`setupLookaside`, [`src/main.c:772-878`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L772-L878), default `1200, 40` at [`src/global.c:218-224`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/global.c#L218-L224) = `48 KiB`), and installs the default `wal_autocheckpoint` of `1000`. As lines 3642–3645 state directly:
   ```c
   /* Register all built-in functions, but do not attempt to read the
   ** database schema yet. This is delayed until the first time the database
   ** is accessed.
   */
   ```
2. **`sqlite3BtreeOpen`** — [`src/btree.c:2532-2825`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L2532-L2825) (in the pinned tree). Opens the pager (`sqlite3PagerOpen`, which opens only the `.db` file descriptor), installs the busy handler, and calls `sqlite3PagerReadFileheader` to read exactly **100 bytes at offset 0** so SQLite learns the page size and reserved space. No lock is taken, no schema page is read, and neither `-wal` nor `-shm` is opened.

### 3.2 First statement: lazy schema load, double page-1 read, and `default_cache_size`

3. **`sqlite3ReadSchema` → `sqlite3Init` → `sqlite3InitOne` → `sqlite3InitCallback`** — [`src/prepare.c:470-484`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L470-L484), [`438-464`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L438-L464), [`199-427`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L199-L427), and [`96-189`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L96-L189) (local copy: [`prepare.c`](prepare.c)). When the parser compiles the first statement and resolves a table name, it calls `sqlite3ReadSchema`, which calls `sqlite3Init` (loading `main` first, then any attached databases). `sqlite3InitOne` starts a read transaction, reads the header metadata slots (schema cookie, file format, text encoding, default cache size), and runs `SELECT*FROM"main".sqlite_schema ORDER BY rowid` (line 366), invoking `sqlite3InitCallback` on every row to re-parse each `CREATE` statement into in-memory `Table` and `Index` objects. If a later prepare fails, `schemaIsValid` ([`src/prepare.c:492-533`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L492-L533)) compares each database's schema cookie against the loaded schema and resets stale ones.
4. **Why page 1 is read twice in WAL mode (`lockBtree` and `btreeBeginTrans`)** — [`src/btree.c:3282-3459`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3282-L3459) and [`src/btree.c:3598-3804`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3598-L3804) (in the pinned tree). This is a refinement to the outline: on the first read transaction of a WAL file, `lockBtree` reads page 1, sees header byte 19 = `2` (WAL read version), opens `-wal` and `-shm` via `sqlite3PagerOpenWal`, and releases page 1 without keeping it (`src/btree.c:3333-3352`), because a newer version of page 1 might exist in the WAL. Its caller `btreeBeginTrans` loops `while( pBt->pPage1==0 && SQLITE_OK==(rc = lockBtree(pBt)) );` (line 3711) and reads page 1 a second time through the now-open WAL layer.
5. **The `cache_size` exception (`BTREE_DEFAULT_CACHE_SIZE`)** — [`src/prepare.c:323-333`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L323-L333) (local copy: [`prepare.c`](prepare.c)). This is a second correction to the outline: although `PRAGMA cache_size` is per-connection, the deprecated `PRAGMA default_cache_size` writes a 4-byte integer into database header bytes 48–51. Because `modernc.org/sqlite` is not built with `SQLITE_OMIT_DEPRECATED`, `sqlite3InitOne` seeds every new connection's starting `cache_size` from that header field:
   ```c
   if( pDb->pSchema->cache_size==0 ){
   #ifndef SQLITE_OMIT_DEPRECATED
     size = sqlite3AbsInt32(meta[BTREE_DEFAULT_CACHE_SIZE-1]);
     if( size==0 ){ size = SQLITE_DEFAULT_CACHE_SIZE; }
     pDb->pSchema->cache_size = size;
   #else
     pDb->pSchema->cache_size = SQLITE_DEFAULT_CACHE_SIZE;
   #endif
     sqlite3BtreeSetCacheSize(pDb->pBt, pDb->pSchema->cache_size);
   }
   ```
   Notice `sqlite3AbsInt32`: a negative KiB value written via `default_cache_size` is converted to a positive page count on reopen (confirmed by adversarial test `OPN-04b`). A platform must therefore set `PRAGMA cache_size` explicitly on every open instead of trusting SQLite's default.

### 3.3 Close, WAL checkpointing, memory counters, and descriptor parking

6. **Close and zombie handles** — [`src/main.c:1266-1486`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L1266-L1486) (`sqlite3Close`, [`sqlite3_close`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L1363) at 1363, [`sqlite3_close_v2`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L1364) at 1364, and [`sqlite3LeaveMutexAndCloseZombie`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L1375-L1486), local copy: [`main.c`](main.c)). `sqlite3_close` returns `SQLITE_BUSY` if unfinalized statements or unfinished backups remain; `sqlite3_close_v2` (which `modernc.org/sqlite` calls in `conn.Close`) instead marks the handle `SQLITE_STATE_ZOMBIE`, deferring teardown until the last statement finishes. When `database/sql` closes a `*sql.DB` while a `*sql.Rows` is still open (Experiment 8.3 B2), the deferral happens one layer higher: `(*sql.DB).Close` closes only idle connections in its pool and does not call the driver's `Close` until `rows.Close()` finalizes the statement and returns the connection. Once teardown runs in `sqlite3LeaveMutexAndCloseZombie`, it closes every B-tree, freeing schemas and lookaside.
7. **Pager close and WAL teardown** — [`src/pager.c:4222-4279`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4222-L4279) (`sqlite3PagerClose`), [`src/pager.c:4188-4205`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4188-L4205) (`databaseIsUnmoved`), [`src/os_unix.c:1623-1632`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1623-L1632) (`fileHasMoved`), [`src/wal.c:2507-2571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2507-L2571) (`sqlite3WalClose`), and [`src/wal.c:1619-1630`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L1619-L1630) (`walIndexClose`, all in the pinned tree). `sqlite3PagerClose` passes a checkpoint page buffer to `sqlite3WalClose` only when `SQLITE_NoCkptOnClose` is off and `databaseIsUnmoved` confirms the path still stats to the open descriptor's inode (`fileHasMoved`). Inside `sqlite3WalClose`, SQLite tries to take an `EXCLUSIVE` lock on the database file—which succeeds only if no other connection anywhere holds a lock—runs a `PASSIVE` checkpoint, and if that returns `SQLITE_OK`, deletes `-wal` and unlinks `-shm` (`walIndexClose` with `isDelete=1`) unless `SQLITE_FCNTL_PERSIST_WAL` is set (in which case `-wal` and `-shm` stay on disk, and `-wal` is truncated only if `journal_size_limit >= 0`).
8. **Per-connection memory counters and `PRAGMA shrink_memory`** — [`src/status.c:203-421`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/status.c#L203-L421) (`sqlite3_db_status64`, local copy: [`status.c`](status.c)) and [`src/pager.c:6881-6887`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6881-L6887) (`sqlite3PagerMemUsed`). `CACHE_USED` sums `sqlite3PagerMemUsed` (page cache buffers plus the `Pager` struct, which embeds the file paths); `SCHEMA_USED` and `STMT_USED` set `db->pnBytesFreed` and dry-run the schema and statement destructors to count bytes without freeing anything; `LOOKASIDE_USED` counts slots, not bytes, and none of the three byte counters includes the 48 KiB lookaside slab. `PRAGMA shrink_memory` ([`src/pragma.c:2439-2442`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pragma.c#L2439-L2442)) calls `sqlite3_db_release_memory` ([`src/main.c:897-915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L897-L915)) → `sqlite3PagerShrink` → `pcache1Shrink` ([`src/pcache1.c:839-851`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L839-L851)) → `pcache1EnforceMaxPage` ([`src/pcache1.c:621-637`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L621-L637)). Because `modernc.org/sqlite` compiles with `SQLITE_ENABLE_MEMORY_MANAGEMENT`, every connection shares one `pcache1` group; temporarily setting the group's `nMaxPage` to `0` evicts unpinned pages from **every** connection in the process.
9. **Descriptor parking on Unix** — [`src/os_unix.c:246-250`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L246-L250) (`UnixUnusedFd`), [`2341-2371`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L2341-L2371) (`unixClose`), [`2101-2109`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L2101-L2109) (`setPendingFd`), [`1471-1482`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1471-L1482) (`closePendingFds`), and [`6369-6414`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6369-L6414) (`findReusableFd`). When `unixClose` closes a connection to an inode whose lock count (`pInode->nLock`) is still nonzero because another connection in the same process holds a lock, it parks the file descriptor in `pUnused` via `setPendingFd` instead of calling `close()`. A later `unixOpen` on the same file with compatible flags can reuse that parked descriptor (`findReusableFd`), and `closePendingFds` finally closes all parked descriptors once `pInode->nLock` reaches `0`.

### 3.4 Experiment 8.1 — `open-cost-and-where-settings-live`

- **Claim:** Opening a database opens only the `.db` file and reads only its 100-byte header (`SCHEMA_USED = 0`). The schema is read and parsed at the first statement, at an I/O and memory cost that grows with table count, and in WAL mode page 1 is read twice (once before `-wal`/`-shm` open and once after). Across a plain copy, the six persistent file settings (`page_size`, `journal_mode=wal`, `auto_vacuum`, `encoding`, `user_version`, `application_id`) plus the deprecated `default_cache_size` survive in the header, while all per-connection settings (`synchronous`, `foreign_keys`, `cache_size`, `busy_timeout`, `wal_autocheckpoint`, `secure_delete`, and rollback journal modes) reset to SQLite defaults; `VACUUM INTO` keeps every file setting except `journal_mode`, outputting a rollback-mode file with header bytes 18–19 = `1`.
- **Evidence form:** `EXACT` (syscall counts, page sets, `SCHEMA_USED` bytes, setting values, header bytes, SHA-256).
- **Why kept:** The second half produces the exact list of settings a platform must store outside the database file to remake a database that behaves the same; forgotten per-connection settings are a recurring production failure, and here the table is measured and written as machine-readable tables (B2, B4) verified against [`internal/sqlitex/placement.go`](../internal/sqlitex/placement.go).
- **Linux only (Part A syscall traces):** Part A's `strace` read counts require Linux; on macOS, `SCHEMA_USED` and all of Part B run natively, and `course run` runs the full experiment in the Linux `lab` container.

<!-- include: experiments/8.1-open-cost-and-where-settings-live/main.go -->
```go
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
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/8.1-open-cost-and-where-settings-live/report.sql -->
```sql
-- Walkthrough of experiment 8.1. Run it with:
--   go run ./cmd/course run 8.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   schemas          part A: one database per row (1, 100 or 1000 empty tables)
--   io_events        part A: one open or read of a file in the work directory, per phase, in trace order
--   schema_pages     part A: one page of the sqlite_schema b-tree (from dbstat), or t0001's root page
--   files            part B: the original file after its last close, and its plain copy
--   settings         part B: one setting, as set, as a fresh connection sees it, on the copy, on VACUUM INTO
--   placements       part B: sqlitex.SettingPlacements, the declared home of every course setting
--   special_cases    part B: the journal_mode and default_cache_size cases, one observed value per row
--   skipped          parts skipped on this platform (empty on Linux)
--   checks           every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  8.1  What does opening a database cost, and where do settings live?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Part A builds three WAL databases, at rest, with 1, 100 and 1000 tables'
.print 'of 4 columns, all empty. A child process opens each one with a plain'
.print 'driver connection (no PRAGMA), runs SELECT count(*) FROM t0001 and closes'
.print 'it under strace; the child marks where each phase starts.'
.print
.print 'Part B sets twelve settings on one file with sqlitex.Open, closes it,'
.print 'and reopens the file, a plain copy and a VACUUM INTO copy without'
.print 'applying any setting.'
.print
.print 'Skipped on this platform:'

SELECT part, what FROM skipped
UNION ALL
SELECT '-', 'nothing' WHERE NOT EXISTS (FROM skipped)
ORDER BY part;

.print
.print '── A. What open reads, and what the first statement reads ──────────────'
.print
.print 'A1. Schema memory is 0 after open: the schema is not read until the'
.print '    first statement. Then it grows with the number of tables.'

SELECT s.tables,
       s.file_pages,
       count(p.page) FILTER (p.role = 'sqlite_schema') AS sqlite_schema_pages,
       s.schema_used_after_open,
       s.schema_used_after_first,
       s.schema_used_after_first // s.tables         AS bytes_per_table
FROM schemas s LEFT JOIN schema_pages p USING (tables)
GROUP BY ALL
ORDER BY s.tables;

.print
.print 'A2. Per phase: open opens only the .db file and reads its 100-byte header'
.print '    (the one partial read); the first statement opens -wal and -shm and'
.print '    reads whole pages of the .db file; close reads nothing. Nothing is'
.print '    read from -wal or -shm.'

WITH e AS (
    SELECT *, file LIKE '%.db' AS is_db,
           op = 'read' AND file LIKE '%.db' AND byte_offset % 4096 = 0 AND nbytes = 4096 AS whole_page
    FROM io_events
),
phases AS (
    SELECT tables, phase, ord
    FROM schemas,
         (VALUES ('open', 1), ('first statement', 2), ('close', 3)) v(phase, ord)
)
SELECT p.tables, p.phase,
       count(e.seq)                                              AS events,
       coalesce(string_agg(CASE WHEN e.is_db THEN '.db' ELSE regexp_extract(e.file, '-[a-z]+$') END,
                           ' ' ORDER BY e.seq) FILTER (e.op = 'open'), '') AS files_opened,
       count(*) FILTER (e.op = 'read' AND e.is_db AND NOT e.whole_page) AS partial_db_reads,
       count(*) FILTER (e.whole_page)                            AS page_reads,
       count(DISTINCT e.byte_offset) FILTER (e.whole_page)       AS distinct_pages,
       count(*) FILTER (e.op = 'read' AND NOT e.is_db)           AS other_file_reads
FROM phases p LEFT JOIN e USING (tables, phase)
GROUP BY p.tables, p.phase, p.ord
ORDER BY p.tables, p.ord;

.print
.print 'A3. The events in order, side by side. Page 1 is read, the header says'
.print '    WAL (byte 19 = 2), so -wal and -shm are opened and page 1 is read'
.print '    again, now through the WAL. Then the sqlite_schema pages follow.'

WITH e AS (
    SELECT tables, phase, seq,
           CASE WHEN op = 'open' THEN 'open ' || file
                WHEN byte_offset % 4096 = 0 AND nbytes = 4096 AND file LIKE '%.db'
                     THEN 'read page ' || (byte_offset // 4096 + 1)
                ELSE 'read ' || nbytes || ' bytes at ' || byte_offset
                     || CASE WHEN file LIKE '%.db' THEN '' ELSE ' of ' || file END
           END AS event
    FROM io_events
    WHERE phase IN ('open', 'first statement')
),
shown AS (
    SELECT phase, seq::VARCHAR AS step, tables, event FROM e WHERE seq <= 9
    UNION ALL
    SELECT phase, 'more', tables, '... ' || count(*) || ' more' FROM e WHERE seq > 9 GROUP BY phase, tables
)
SELECT phase, step,
       coalesce(max(event) FILTER (tables = 1), '')    AS "1 table",
       coalesce(max(event) FILTER (tables = 100), '')  AS "100 tables",
       coalesce(max(event) FILTER (tables = 1000), '') AS "1000 tables"
FROM shown
GROUP BY phase, step
ORDER BY phase = 'first statement', step = 'more', try_cast(step AS INTEGER);

.print
.print 'A4. The pages the first statement reads are exactly the pages listed in'
.print '    schema_pages: the sqlite_schema b-tree (from dbstat) plus the root page'
.print '    of t0001. Page 1 is the sqlite_schema root and the page read twice.'

WITH r AS (
    SELECT tables, byte_offset // 4096 + 1 AS page
    FROM io_events
    WHERE phase = 'first statement' AND op = 'read' AND file LIKE '%.db'
      AND byte_offset % 4096 = 0 AND nbytes = 4096
),
read_pages AS (SELECT DISTINCT tables, page FROM r)
SELECT s.tables,
       (SELECT count(*) FROM schema_pages p WHERE p.tables = s.tables AND role = 'sqlite_schema') AS schema_pages,
       (SELECT page FROM schema_pages p WHERE p.tables = s.tables AND role = 't0001 root')       AS t0001_root,
       (SELECT count(*) FROM read_pages x WHERE x.tables = s.tables)                             AS pages_read,
       (SELECT count(*) FROM r WHERE r.tables = s.tables AND page = 1)                           AS page_1_reads,
       (SELECT count(*) FROM read_pages x WHERE x.tables = s.tables
          AND x.page NOT IN (SELECT page FROM schema_pages p WHERE p.tables = s.tables))         AS read_not_listed,
       (SELECT count(*) FROM schema_pages p WHERE p.tables = s.tables
          AND p.page NOT IN (SELECT page FROM read_pages x WHERE x.tables = s.tables))           AS listed_not_read
FROM schemas s
ORDER BY s.tables;

.print
.print 'A5. The page numbers, as ranges. Page 2 is the root of t0001, the first'
.print '    table created; every other page belongs to sqlite_schema, whose pages'
.print '    are spread through the file because they were allocated between tables.'

WITH p AS (
    SELECT DISTINCT tables, byte_offset // 4096 + 1 AS page
    FROM io_events
    WHERE phase = 'first statement' AND op = 'read' AND file LIKE '%.db'
      AND byte_offset % 4096 = 0 AND nbytes = 4096
),
g AS (SELECT tables, page, page - row_number() OVER (PARTITION BY tables ORDER BY page) AS grp FROM p),
r AS (SELECT tables, min(page) AS lo, max(page) AS hi FROM g GROUP BY tables, grp),
t AS (
    SELECT tables, lo,
           CASE WHEN lo = hi THEN lo::VARCHAR ELSE lo || '-' || hi END AS txt,
           row_number() OVER (PARTITION BY tables ORDER BY lo) AS rn
    FROM r
)
SELECT tables, string_agg(txt, ',' ORDER BY lo) AS pages_read_by_the_first_statement
FROM t
GROUP BY tables, (rn - 1) // 14
ORDER BY tables, min(lo);

.print
.print '── B. Which settings live in the file, and which in the connection ─────'
.print
.print 'B1. After its last connection closed the original is one file at rest,'
.print '    and a plain copy of it has the same sha256 (rung R5: capture = the'
.print '    file at rest, rebuild = copy).'

SELECT file, at_rest, bytes, sha256 FROM files ORDER BY seq;

.print
.print 'B2. Each setting as set on the original, on a fresh file (the default),'
.print '    and on the copy and the VACUUM INTO output, each opened with no'
.print '    settings applied. A setting the copy kept lives in the file; the'
.print '    declared column is what sqlitex.SettingPlacements says. VACUUM INTO'
.print '    keeps every file setting except journal_mode=wal.'

SELECT s.setting,
       s.on_original,
       s.fresh_default  AS "default",
       s.on_copy        AS copy,
       s.on_vacuum_into AS vacuum_into,
       s.lives_in,
       p.home           AS declared,
       p.header_bytes
FROM settings s LEFT JOIN placements p ON p.name = s.setting
ORDER BY s.seq;

.print
.print 'B3. The two journal_mode cases and the deprecated default_cache_size.'
.print '    A rollback mode (persist) reverts to delete on reopen; only wal is'
.print '    stored in the file (header bytes 18-19 = 2), and the VACUUM INTO output'
.print '    is a rollback-mode file (bytes 18-19 = 1).'
.print '    default_cache_size is stored in header bytes 48-51 and becomes the'
.print '    starting cache_size of every new connection.'

SELECT case_name, observed, value FROM special_cases ORDER BY seq;

.print
.print 'B4. The settings a platform must store outside the file to rebuild a'
.print '    database that behaves the same are the connection rows of this list'
.print '    (sqlitex.SettingPlacements, which B2 checks against):'

SELECT name, pragma, home, header_bytes FROM placements ORDER BY seq;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 8.1
```

Add `-i` (`go run ./cmd/course run -i 8.1`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

`course run` also prints the run-dependent values, which are not part of the stored evidence: the time of the first statement, per schema.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/8.1-open-cost-and-where-settings-live/`](evidence/8.1-open-cost-and-where-settings-live/), and `go run ./cmd/course check evidence ch08` compares a fresh run with them byte for byte.

<!-- include: evidence/8.1-open-cost-and-where-settings-live.txt -->
```text

════════════════════════════════════════════════════════════════════════
  8.1  What does opening a database cost, and where do settings live?
════════════════════════════════════════════════════════════════════════

Part A builds three WAL databases, at rest, with 1, 100 and 1000 tables
of 4 columns, all empty. A child process opens each one with a plain
driver connection (no PRAGMA), runs SELECT count(*) FROM t0001 and closes
it under strace; the child marks where each phase starts.

Part B sets twelve settings on one file with sqlitex.Open, closes it,
and reopens the file, a plain copy and a VACUUM INTO copy without
applying any setting.

Skipped on this platform:
┌──────┬─────────┐
│ part │  what   │
├──────┼─────────┤
│ -    │ nothing │
└──────┴─────────┘

── A. What open reads, and what the first statement reads ──────────────

A1. Schema memory is 0 after open: the schema is not read until the
    first statement. Then it grows with the number of tables.
┌────────┬────────────┬─────────────────────┬────────────────────────┬─────────────────────────┬─────────────────┐
│ tables │ file_pages │ sqlite_schema_pages │ schema_used_after_open │ schema_used_after_first │ bytes_per_table │
├────────┼────────────┼─────────────────────┼────────────────────────┼─────────────────────────┼─────────────────┤
│ 1      │ 2          │ 1                   │ 0                      │ 827                     │ 827             │
│ 100    │ 105        │ 5                   │ 0                      │ 26601                   │ 266             │
│ 1000   │ 1036       │ 36                  │ 0                      │ 251601                  │ 251             │
└────────┴────────────┴─────────────────────┴────────────────────────┴─────────────────────────┴─────────────────┘

A2. Per phase: open opens only the .db file and reads its 100-byte header
    (the one partial read); the first statement opens -wal and -shm and
    reads whole pages of the .db file; close reads nothing. Nothing is
    read from -wal or -shm.
┌────────┬─────────────────┬────────┬──────────────┬──────────────────┬────────────┬────────────────┬──────────────────┐
│ tables │      phase      │ events │ files_opened │ partial_db_reads │ page_reads │ distinct_pages │ other_file_reads │
├────────┼─────────────────┼────────┼──────────────┼──────────────────┼────────────┼────────────────┼──────────────────┤
│ 1      │ open            │ 2      │ .db          │ 1                │ 0          │ 0              │ 0                │
│ 1      │ first statement │ 5      │ -wal -shm    │ 0                │ 3          │ 2              │ 0                │
│ 1      │ close           │ 0      │              │ 0                │ 0          │ 0              │ 0                │
│ 100    │ open            │ 2      │ .db          │ 1                │ 0          │ 0              │ 0                │
│ 100    │ first statement │ 9      │ -wal -shm    │ 0                │ 7          │ 6              │ 0                │
│ 100    │ close           │ 0      │              │ 0                │ 0          │ 0              │ 0                │
│ 1000   │ open            │ 2      │ .db          │ 1                │ 0          │ 0              │ 0                │
│ 1000   │ first statement │ 40     │ -wal -shm    │ 0                │ 38         │ 37             │ 0                │
│ 1000   │ close           │ 0      │              │ 0                │ 0          │ 0              │ 0                │
└────────┴─────────────────┴────────┴──────────────┴──────────────────┴────────────┴────────────────┴──────────────────┘

A3. The events in order, side by side. Page 1 is read, the header says
    WAL (byte 19 = 2), so -wal and -shm are opened and page 1 is read
    again, now through the WAL. Then the sqlite_schema pages follow.
┌─────────────────┬──────┬──────────────────────┬────────────────────────┬─────────────────────────┐
│      phase      │ step │       1 table        │       100 tables       │       1000 tables       │
├─────────────────┼──────┼──────────────────────┼────────────────────────┼─────────────────────────┤
│ open            │ 1    │ open schema-1.db     │ open schema-100.db     │ open schema-1000.db     │
│ open            │ 2    │ read 100 bytes at 0  │ read 100 bytes at 0    │ read 100 bytes at 0     │
│ first statement │ 1    │ read page 1          │ read page 1            │ read page 1             │
│ first statement │ 2    │ open schema-1.db-wal │ open schema-100.db-wal │ open schema-1000.db-wal │
│ first statement │ 3    │ open schema-1.db-shm │ open schema-100.db-shm │ open schema-1000.db-shm │
│ first statement │ 4    │ read page 1          │ read page 1            │ read page 1             │
│ first statement │ 5    │ read page 2          │ read page 34           │ read page 34            │
│ first statement │ 6    │                      │ read page 36           │ read page 36            │
│ first statement │ 7    │                      │ read page 69           │ read page 69            │
│ first statement │ 8    │                      │ read page 102          │ read page 102           │
│ first statement │ 9    │                      │ read page 2            │ read page 132           │
│ first statement │ more │                      │                        │ ... 31 more             │
└─────────────────┴──────┴──────────────────────┴────────────────────────┴─────────────────────────┘

A4. The pages the first statement reads are exactly the pages listed in
    schema_pages: the sqlite_schema b-tree (from dbstat) plus the root page
    of t0001. Page 1 is the sqlite_schema root and the page read twice.
┌────────┬──────────────┬────────────┬────────────┬──────────────┬─────────────────┬─────────────────┐
│ tables │ schema_pages │ t0001_root │ pages_read │ page_1_reads │ read_not_listed │ listed_not_read │
├────────┼──────────────┼────────────┼────────────┼──────────────┼─────────────────┼─────────────────┤
│ 1      │ 1            │ 2          │ 2          │ 2            │ 0               │ 0               │
│ 100    │ 5            │ 2          │ 6          │ 2            │ 0               │ 0               │
│ 1000   │ 36           │ 2          │ 37         │ 2            │ 0               │ 0               │
└────────┴──────────────┴────────────┴────────────┴──────────────┴─────────────────┴─────────────────┘

A5. The page numbers, as ranges. Page 2 is the root of t0001, the first
    table created; every other page belongs to sqlite_schema, whose pages
    are spread through the file because they were allocated between tables.
┌────────┬─────────────────────────────────────────────────────────┐
│ tables │            pages_read_by_the_first_statement            │
├────────┼─────────────────────────────────────────────────────────┤
│ 1      │ 1-2                                                     │
│ 100    │ 1-2,34,36,69,102                                        │
│ 1000   │ 1-2,34,36,69,102,132,162,192,222,252,282,312,342,372    │
│ 1000   │ 402,432,462,492,522,552,582,612,642,672,702,732,762,792 │
│ 1000   │ 822,852,882,912,942,972,1002,1032                       │
└────────┴─────────────────────────────────────────────────────────┘

── B. Which settings live in the file, and which in the connection ─────

B1. After its last connection closed the original is one file at rest,
    and a plain copy of it has the same sha256 (rung R5: capture = the
    file at rest, rebuild = copy).
┌─────────────┬─────────┬───────┬──────────────────────────────────────────────────────────────────┐
│    file     │ at_rest │ bytes │                              sha256                              │
├─────────────┼─────────┼───────┼──────────────────────────────────────────────────────────────────┤
│ original.db │ true    │ 24576 │ 6d566c958f17426cc0f7114d46ad61f044cac8a0952ba4c3c88ee26fad022172 │
│ copy.db     │ true    │ 24576 │ 6d566c958f17426cc0f7114d46ad61f044cac8a0952ba4c3c88ee26fad022172 │
└─────────────┴─────────┴───────┴──────────────────────────────────────────────────────────────────┘

B2. Each setting as set on the original, on a fresh file (the default),
    and on the copy and the VACUUM INTO output, each opened with no
    settings applied. A setting the copy kept lives in the file; the
    declared column is what sqlitex.SettingPlacements says. VACUUM INTO
    keeps every file setting except journal_mode=wal.
┌────────────────────┬─────────────┬─────────┬────────────┬─────────────┬────────────┬────────────┬──────────────┐
│      setting       │ on_original │ default │    copy    │ vacuum_into │  lives_in  │  declared  │ header_bytes │
├────────────────────┼─────────────┼─────────┼────────────┼─────────────┼────────────┼────────────┼──────────────┤
│ page_size          │ 8192        │ 4096    │ 8192       │ 8192        │ file       │ file       │ 16-17        │
│ journal_mode=wal   │ wal         │ delete  │ wal        │ delete      │ file       │ file       │ 18-19        │
│ auto_vacuum        │ 2           │ 0       │ 2          │ 2           │ file       │ file       │ 52-55,64-67  │
│ encoding           │ utf-16le    │ utf-8   │ utf-16le   │ utf-16le    │ file       │ file       │ 56-59        │
│ user_version       │ 7           │ 0       │ 7          │ 7           │ file       │ file       │ 60-63        │
│ application_id     │ 1397836120  │ 0       │ 1397836120 │ 1397836120  │ file       │ file       │ 68-71        │
│ synchronous        │ 1           │ 2       │ 2          │ 2           │ connection │ connection │              │
│ foreign_keys       │ 1           │ 0       │ 0          │ 0           │ connection │ connection │              │
│ cache_size         │ -8000       │ -2000   │ -2000      │ -2000       │ connection │ connection │              │
│ busy_timeout       │ 3000        │ 0       │ 0          │ 0           │ connection │ connection │              │
│ wal_autocheckpoint │ 200         │ 1000    │ 1000       │ 1000        │ connection │ connection │              │
│ secure_delete      │ 1           │ 0       │ 0          │ 0           │ connection │ connection │              │
└────────────────────┴─────────────┴─────────┴────────────┴─────────────┴────────────┴────────────┴──────────────┘

B3. The two journal_mode cases and the deprecated default_cache_size.
    A rollback mode (persist) reverts to delete on reopen; only wal is
    stored in the file (header bytes 18-19 = 2), and the VACUUM INTO output
    is a rollback-mode file (bytes 18-19 = 1).
    default_cache_size is stored in header bytes 48-51 and becomes the
    starting cache_size of every new connection.
┌───────────────────────────────────────────────────┬────────────────────┬────────┐
│                     case_name                     │      observed      │ value  │
├───────────────────────────────────────────────────┼────────────────────┼────────┤
│ VACUUM INTO output                                │ header bytes 18-19 │ 1 1    │
│ journal_mode=persist set on a new file, reopened  │ journal_mode       │ delete │
│ PRAGMA default_cache_size=777, then a copy opened │ default_cache_size │ 777    │
│ PRAGMA default_cache_size=777, then a copy opened │ cache_size         │ 777    │
│ PRAGMA default_cache_size=777, then a copy opened │ header bytes 48-51 │ 777    │
└───────────────────────────────────────────────────┴────────────────────┴────────┘

B4. The settings a platform must store outside the file to rebuild a
    database that behaves the same are the connection rows of this list
    (sqlitex.SettingPlacements, which B2 checks against):
┌─────────────────────────────────────────────────┬────────────────────┬────────────┬──────────────┐
│                      name                       │       pragma       │    home    │ header_bytes │
├─────────────────────────────────────────────────┼────────────────────┼────────────┼──────────────┤
│ page_size                                       │ page_size          │ file       │ 16-17        │
│ journal_mode=wal                                │ journal_mode       │ file       │ 18-19        │
│ auto_vacuum                                     │ auto_vacuum        │ file       │ 52-55,64-67  │
│ encoding                                        │ encoding           │ file       │ 56-59        │
│ user_version                                    │ user_version       │ file       │ 60-63        │
│ application_id                                  │ application_id     │ file       │ 68-71        │
│ default_cache_size                              │ default_cache_size │ file       │ 48-51        │
│ journal_mode=delete|truncate|persist|memory|off │ journal_mode       │ connection │              │
│ synchronous                                     │ synchronous        │ connection │              │
│ foreign_keys                                    │ foreign_keys       │ connection │              │
│ cache_size                                      │ cache_size         │ connection │              │
│ busy_timeout                                    │ busy_timeout       │ connection │              │
│ wal_autocheckpoint                              │ wal_autocheckpoint │ connection │              │
│ secure_delete                                   │ secure_delete      │ connection │              │
└─────────────────────────────────────────────────┴────────────────────┴────────────┴──────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  1 table(s): open opens only the database file and reads only its 100-byte header
A  ✓  1 table(s): the first statement reads exactly the sqlite_schema b-tree pages (1, from dbstat) plus t0001's root page
A  ✓  1 table(s): page 1 is read twice: once before the -wal is opened (header byte 19 = 2 sends lockBtree to open the WAL) and once after
A  ✓  1 table(s): nothing is read from the -wal or -shm (the -wal is empty; the -shm is mapped, not read)
A  ✓  1 table(s): the -wal and -shm files are opened at the first statement, not at open
A  ✓  100 table(s): open opens only the database file and reads only its 100-byte header
A  ✓  100 table(s): the first statement reads exactly the sqlite_schema b-tree pages (5, from dbstat) plus t0001's root page
A  ✓  100 table(s): page 1 is read twice: once before the -wal is opened (header byte 19 = 2 sends lockBtree to open the WAL) and once after
A  ✓  100 table(s): nothing is read from the -wal or -shm (the -wal is empty; the -shm is mapped, not read)
A  ✓  100 table(s): the -wal and -shm files are opened at the first statement, not at open
A  ✓  1000 table(s): open opens only the database file and reads only its 100-byte header
A  ✓  1000 table(s): the first statement reads exactly the sqlite_schema b-tree pages (36, from dbstat) plus t0001's root page
A  ✓  1000 table(s): page 1 is read twice: once before the -wal is opened (header byte 19 = 2 sends lockBtree to open the WAL) and once after
A  ✓  1000 table(s): nothing is read from the -wal or -shm (the -wal is empty; the -shm is mapped, not read)
A  ✓  1000 table(s): the -wal and -shm files are opened at the first statement, not at open
A  ✓  SCHEMA_USED is 0 after open for every schema, and after the first statement it grows with the table count (1 < 100 < 1000)
B  ✓  the original is at rest after its last connection closed
B  ✓  a plain copy of the closed file has the same sha256 (rung R5: capture = the file at rest, rebuild = copy)
B  ✓  every value set differs from the fresh default, and every setting that survived the copy is exactly a setting sqlitex.SettingPlacements declares as living in the file
B  ✓  VACUUM INTO keeps every file setting except journal_mode: its output is a rollback-mode file (header bytes 18-19 = 1)
B  ✓  a rollback journal mode (persist) does not survive a reopen: it is per connection; only wal is stored in the file
B  ✓  the deprecated PRAGMA default_cache_size is stored in header bytes 48-51 and becomes the starting cache_size of every new connection to the file

22 of 22 claims hold

Experiment verdict: OPEN COST AND WHERE SETTINGS LIVE OK
```

#### Reading the Experiment 8.1 evidence

- **Pin check (the experiment's own output, shown when it is run directly):** Confirms `modernc.org/sqlite v1.60.1` reports the exact pinned Fossil check-in `bf7c7f30…` (`3.53.4`).
- **A1–A5 (Part A — open versus first statement across 1, 100, and 1,000 empty 4-column tables):**

| Tables | File size (pages) | `open` phase events | `first statement` `.db` whole-page reads (distinct) | `sqlite_schema` pages (`dbstat`) | `SCHEMA_USED` at open → after 1st stmt | Bytes / table |
| --- | --- | --- | --- | --- | --- | --- |
| `1` | `2` | `2` (`open .db`, `read 100 bytes at 0`) | `3` (`2`: `1-2`) | `1` | `0` → `827` | `827` |
| `100` | `105` | `2` (`open .db`, `read 100 bytes at 0`) | `7` (`6`: `1-2,34,36,69,102`) | `5` | `0` → `26601` | `266` |
| `1000` | `1036` | `2` (`open .db`, `read 100 bytes at 0`) | `38` (`37`: `1-2,34,36,69,102,…,1032`) | `36` | `0` → `251601` | `251` |

  - At `open`, every database—whether `2` pages or `1,036` pages—issues exactly two events: `open schema-N.db` and `read 100 bytes at 0`, with `SCHEMA_USED` `0` bytes. Neither `-wal` nor `-shm` is opened yet.
  - At `first statement` (`SELECT count(*) FROM t0001`), the first four events for every size are identical: `read page 1`, `open schema-N.db-wal`, `open schema-N.db-shm`, `read page 1`. That is `lockBtree` reading page 1, seeing header byte 19 = `2`, opening the WAL, and `btreeBeginTrans` re-reading page 1 (Section 3.2). After that second page-1 read, SQLite reads every remaining `sqlite_schema` B-tree page (`1`, `5`, and `36` pages, spaced ~30 pages apart because the tables were created in one transaction with each table's root page allocated between schema pages) plus `t0001`'s root page (`page 2`). Zero reads hit `-wal` or `-shm` because the file started at rest (`-wal` is empty and `-shm` is `mmap`'d; adversarial test `OPN-03b` shows that when a killed writer leaves frames in `-wal`, the first statement reads `-wal` during recovery while `open` still touches only the 100-byte `.db` header).
  - `SCHEMA_USED` grows linearly with schema size (`827` → `26,601` → `251,601` bytes, roughly `251` bytes per 4-column table at 1,000 tables). Run-dependent wall-clock times (printed by `course run`, not stored) rose from about `61µs` to `226µs` to `1.8ms` in the lab run (`598µs`, `1.536ms`, and `7.891ms` in the earlier capture; timing is informational, never a pass condition).
  - At `close`, `0` opens or reads occur (unlinking `-shm` and `-wal` is not in the traced open/read set).
- **B1–B2 (Part B — file settings versus connection settings):**
  - `original.db` is created with every course setting changed from SQLite's default, closed cleanly (at rest, `24576` bytes, sha256 `6d566c95…`), and copied byte for byte (`copy.db` has the identical SHA-256—R5 in one line).
  - Opening `copy.db` with a plain driver connection (no PRAGMAs applied) separates the 12 settings cleanly:
    - **Six file settings survive the copy:** `page_size` (`8192`), `journal_mode=wal` (`wal`), `auto_vacuum` (`2`), `encoding` (`utf-16le`), `user_version` (`7`), `application_id` (`1397836120`, ASCII `SQIX`).
    - **Six connection settings revert to defaults:** `synchronous` (`1` → `2`, `FULL`), `foreign_keys` (`1` → `0`), `cache_size` (`-8000` → `-2000`), `busy_timeout` (`3000` → `0`), `wal_autocheckpoint` (`200` → `1000`), `secure_delete` (`1` → `0`).
  - `VACUUM INTO` (`vacuum-into.db`) preserves every file setting (`page_size=8192`, `auto_vacuum=2`, `encoding=utf-16le`, `user_version=7`, `application_id=1397836120`) **except** `journal_mode`, which reads `delete`—and the experiment reads `vacuum-into.db`'s header directly to verify bytes 18–19 = `1` (rollback format).
- **B3–B4 (rollback journal mode, `default_cache_size`, and the placement list):**
  - Setting `journal_mode=persist` on a new file, closing, and reopening reads back `delete`: none of the five rollback modes persists in the file header (only `wal` flips bytes 18–19 to `2`).
  - Setting `PRAGMA default_cache_size=777`, closing, copying, and opening with a plain connection reads `default_cache_size` `777`, `cache_size` `777`, and raw header bytes 48–51 = `777` (confirming `src/prepare.c:323-333`).
  - B4 lists all 14 declared entries from `sqlitex.SettingPlacements()`, and B2 links each of the 12 observed rows to its declared placement, ready for the capstone's catalog in Chapter 10.

---

## 4. Timeline

Tracing one connection from open to close shows exactly when each file descriptor and each memory allocation appears and disappears (combining Experiments 8.1 A and 8.2 A–D, measured on Linux):

| Lifecycle point | `DELETE` FDs | `WAL` FDs | Open files (`WAL`) | Side files on disk (`WAL`) | Connection memory state |
| --- | --- | --- | --- | --- | --- |
| 1. `sqlite3_open_v2` | `1` | `1` | `.db` | none | `SCHEMA_USED = 0`; lookaside (`48 KiB`) allocated; 100-byte header read; no lock |
| 2. First statement | `1` | `3` | `.db`, `-wal`, `-shm` | `-wal`, `-shm` | Page 1 read twice; `sqlite_schema` parsed (`SCHEMA_USED` jumps to `1,347` B for recipe, `251,601` B for 1,000 tables); `-shm` mmapped |
| 3. Idle (after scan + 1 prepared stmt) | `1` | `3` | `.db`, `-wal`, `-shm` | `-wal`, `-shm` | `CACHE_USED` (run-dependent: about `52,835` B in the lab run) + `SCHEMA_USED` (`1,347` B) + `STMT_USED` (`5,008` B) + `~168 KiB` uncounted RSS (for 3-table schema) |
| 4. `PRAGMA shrink_memory` (conn 0) | `1` | `3` | `.db`, `-wal`, `-shm` | `-wal`, `-shm` | `CACHE_USED` drops about `52,835` → `4,963` B (lab run) on **all** connections (shared `pcache1` group); `SCHEMA_USED` and `STMT_USED` unchanged |
| 5. Inside write transaction | `2` (`.db`, `-journal`) | `3` | `.db`, `-wal`, `-shm` | `-wal`, `-shm` | Dirty pages pinned in cache (resist `shrink_memory` until `COMMIT`/`ROLLBACK`) |
| 6. After `COMMIT` | `1` | `3` | `.db`, `-wal`, `-shm` | `-wal`, `-shm` | `-journal` unlinked in `DELETE`; `-wal` holds committed frames in `WAL` |
| 7. Second conn in same process (WAL) | — | `5` (not `6`) | `2× .db`, `2× -wal`, `1× -shm` | `-wal`, `-shm` | `-shm` descriptor shared per inode (`unixShmNode`) |
| 8. Second conn closes (first stays open) | — | `4` (not `3`) | `2× .db`, `1× -wal`, `1× -shm` | `-wal`, `-shm` | Closed conn's `.db` FD parked in `UnixUnusedFd` (`setPendingFd`) so conn 1's POSIX lock survives |
| 9. Last connection closes cleanly | `0` | `0` | none | none | `PASSIVE` checkpoint under `EXCLUSIVE` lock, `fsync`, unlink `-shm` & `-wal`, `closePendingFds`; file at rest |

### 4.1 Experiment 8.2 — `idle-cost`

- **Claim:** An idle connection holds `1` file descriptor in `DELETE` mode (`2` during a write transaction) and `3` in `WAL` mode (`1` before its first statement). Across `N >= 20` idle WAL connections to 3-table recipe databases, descriptors equal exactly `3N`, per-connection memory breaks down into `CACHE_USED`, `SCHEMA_USED`, and `STMT_USED`, and process RSS grows by at least their sum and at most their sum plus `256 KiB` per connection (this `256 KiB` margin is specific to 8.2's 3-table, 1-statement workload and grows with schema size—see `IDL-03a` in Section 6). Running `PRAGMA shrink_memory` on connection 0 alone drops `CACHE_USED` under 1/8 on every connection in the process while leaving `SCHEMA_USED` and `STMT_USED` intact. Opening a second connection to the same WAL file in one process uses `5` descriptors (shared `-shm`), and closing it parks its `.db` descriptor (`4` held) until the first connection closes (`0` held).
- **Evidence form:** `EXACT` for descriptor counts, `SCHEMA_USED`, and `STMT_USED`; `BOUND` for process RSS growth and post-shrink `CACHE_USED`.
- **Linux only (descriptor target names and memory-margin measurement):** Listing open filenames (`t.db t.db-shm t.db-wal`, and `200` each of `db`, `-wal` and `-shm` targets in B2) requires Linux `/proc/self/fd`; run directly on macOS, descriptor counts (`/dev/fd`) and RSS (`ps`) still run, and `course run` runs the full experiment in the Linux `lab` container. The `256 KiB` per-connection margin was measured on Linux only.

<!-- include: experiments/8.2-idle-cost/main.go -->
```go
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
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/8.2-idle-cost/report.sql -->
```sql
-- Walkthrough of experiment 8.2. Run it with:
--   go run ./cmd/course run 8.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   setup            one setting of the experiment (name, value)
--   fd_states        parts A and D: one connection state; descriptors above the baseline, predicted and named
--   fleet            part B: the descriptors held with all N connections open, then after closing all
--   fleet_targets    part B: one descriptor held with all N connections open (its file, relative to the directory)
--   connections      parts B and C: one idle connection; SCHEMA_USED and STMT_USED before and after shrink_memory
--   skipped          parts skipped on this platform (empty on Linux)
--   checks           every claim the experiment asserted (the runner lists it last)
-- info.sql presents info_cache (CACHE_USED per connection), info_memory
-- (process memory) and info_limits (RLIMIT_NOFILE): they vary between machines.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  8.2  What does an idle connection cost in descriptors and memory?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Descriptors are counted in this process before and after each step;'
.print 'the numbers below are the difference. Settings of the experiment:'

SELECT name, value FROM setup ORDER BY seq;

.print
.print 'Skipped on this platform:'

SELECT part, what FROM skipped
UNION ALL
SELECT '-', 'nothing' WHERE NOT EXISTS (FROM skipped)
ORDER BY part;

.print
.print '── A. Descriptors held by one connection, per state ────────────────────'
.print
.print 'A1. One plain connection to t.db, through five states: descriptors held'
.print '    above the baseline, the files they name, and the side files on disk.'
.print '    In delete mode the -journal exists, and holds a descriptor, only inside'
.print '    the write transaction. In WAL mode the first read opens -wal and -shm'
.print '    and the connection keeps all three open while idle; close removes'
.print '    both files.'

SELECT journal_mode, state, descriptors, expected, targets,
       coalesce(nullif(side_files, ''), 'none') AS side_files
FROM fd_states
WHERE part = 'A'
ORDER BY journal_mode, seq;

.print
.print '── B. Many idle WAL connections, one database file each ────────────────'
.print
.print 'B1. Every idle WAL connection holds exactly three descriptors, and'
.print '    closing them all releases every one and leaves each file at rest.'

SELECT stage,
       descriptors,
       round(descriptors / (SELECT value FROM setup WHERE name = 'connections')::INTEGER, 2) AS per_connection,
       files_not_at_rest
FROM fleet
ORDER BY seq;

.print
.print 'B2. What those descriptors point at: one .db, one -wal and one -shm per'
.print '    file.'

SELECT CASE WHEN target LIKE '%-wal' THEN '-wal'
            WHEN target LIKE '%-shm' THEN '-shm'
            ELSE 'db' END AS kind,
       count(*)              AS descriptors,
       min(target)           AS first,
       max(target)           AS last
FROM fleet_targets
GROUP BY kind
ORDER BY kind = 'db' DESC, kind DESC;

.print
.print 'B3. The memory each connection reports for its schema and its prepared'
.print '    statement is the same on every connection. CACHE_USED (page cache)'
.print '    also counts the Pager allocation, whose size depends on the length of'
.print '    the path, so it varies between machines: info.sql shows it with the'
.print '    process memory.'

SELECT count(*)                                     AS connections,
       min(schema_used) || '..' || max(schema_used) AS schema_used,
       min(stmt_used) || '..' || max(stmt_used)     AS stmt_used,
       sum(schema_used)                             AS schema_used_sum,
       sum(stmt_used)                               AS stmt_used_sum
FROM connections;

.print
.print '── C. PRAGMA shrink_memory ─────────────────────────────────────────────'
.print
.print 'C1. shrink_memory on connection 0 alone lowered CACHE_USED on every'
.print '    connection (this driver keeps every page cache in one shared group;'
.print '    the numbers are in info.sql). It frees cache pages only: schema and'
.print '    statement memory are unchanged on every connection.'

SELECT count(*) FILTER (schema_used_after_shrink = schema_used) AS schema_used_unchanged,
       count(*) FILTER (stmt_used_after_shrink = stmt_used)     AS stmt_used_unchanged,
       count(*)                                                 AS connections
FROM connections;

.print
.print '── D. Two connections to one WAL file in one process ───────────────────'
.print
.print 'D1. The second connection adds two descriptors, not three: both share'
.print '    one -shm descriptor. When it closes, its database descriptor stays'
.print '    open (4, not 3) until the last connection to the file closes.'

SELECT state, descriptors AS fds, expected, targets,
       coalesce(nullif(side_files, ''), 'none') AS side_files
FROM fd_states
WHERE part = 'D'
ORDER BY seq;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 8.2
```

Add `-i` (`go run ./cmd/course run -i 8.2`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

`course run` also prints the run-dependent values, which are not part of the stored evidence: `CACHE_USED` of one connection before and after `shrink_memory`, process memory (RSS) at three moments of Part B, and `RLIMIT_NOFILE` as the Go process sees it.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/8.2-idle-cost/`](evidence/8.2-idle-cost/), and `go run ./cmd/course check evidence ch08` compares a fresh run with them byte for byte. The run uses the defaults `-n 200 -step 3`.

<!-- include: evidence/8.2-idle-cost.txt -->
```text

════════════════════════════════════════════════════════════════════════
  8.2  What does an idle connection cost in descriptors and memory?
════════════════════════════════════════════════════════════════════════

Descriptors are counted in this process before and after each step;
the numbers below are the difference. Settings of the experiment:
┌────────────────────────────────────┬───────────────────────────────────────────────────┐
│                name                │                       value                       │
├────────────────────────────────────┼───────────────────────────────────────────────────┤
│ descriptors counted from           │ /proc/self/fd                                     │
│ process memory from                │ /proc/self/status                                 │
│ connections                        │ 200                                               │
│ every file                         │ a copy of recipe step 3                           │
│ file bytes                         │ 159744                                            │
│ file pages                         │ 39                                                │
│ sqlitex default synchronous        │ FULL                                              │
│ sqlitex default foreign_keys       │ ON                                                │
│ sqlitex default secure_delete      │ OFF                                               │
│ sqlitex default cache_size         │ -2000                                             │
│ sqlitex default wal_autocheckpoint │ 1000                                              │
│ sqlitex default busy_timeout       │ 5000ms                                            │
│ read per connection                │ SELECT count(*), sum(length(body)) FROM documents │
│ prepared statement kept open       │ SELECT name, balance FROM accounts WHERE id = ?   │
└────────────────────────────────────┴───────────────────────────────────────────────────┘

Skipped on this platform:
┌──────┬─────────┐
│ part │  what   │
├──────┼─────────┤
│ -    │ nothing │
└──────┴─────────┘

── A. Descriptors held by one connection, per state ────────────────────

A1. One plain connection to t.db, through five states: descriptors held
    above the baseline, the files they name, and the side files on disk.
    In delete mode the -journal exists, and holds a descriptor, only inside
    the write transaction. In WAL mode the first read opens -wal and -shm
    and the connection keeps all three open while idle; close removes
    both files.
┌──────────────┬────────────────────────────┬─────────────┬──────────┬────────────────────────┬───────────────────┐
│ journal_mode │           state            │ descriptors │ expected │        targets         │    side_files     │
├──────────────┼────────────────────────────┼─────────────┼──────────┼────────────────────────┼───────────────────┤
│ delete       │ opened, no statement yet   │ 1           │ 1        │ t.db                   │ none              │
│ delete       │ idle after a read          │ 1           │ 1        │ t.db                   │ none              │
│ delete       │ inside a write transaction │ 2           │ 2        │ t.db t.db-journal      │ t.db-journal      │
│ delete       │ idle after the commit      │ 1           │ 1        │ t.db                   │ none              │
│ delete       │ after close                │ 0           │ 0        │                        │ none              │
│ wal          │ opened, no statement yet   │ 1           │ 1        │ t.db                   │ none              │
│ wal          │ idle after a read          │ 3           │ 3        │ t.db t.db-shm t.db-wal │ t.db-wal t.db-shm │
│ wal          │ inside a write transaction │ 3           │ 3        │ t.db t.db-shm t.db-wal │ t.db-wal t.db-shm │
│ wal          │ idle after the commit      │ 3           │ 3        │ t.db t.db-shm t.db-wal │ t.db-wal t.db-shm │
│ wal          │ after close                │ 0           │ 0        │                        │ none              │
└──────────────┴────────────────────────────┴─────────────┴──────────┴────────────────────────┴───────────────────┘

── B. Many idle WAL connections, one database file each ────────────────

B1. Every idle WAL connection holds exactly three descriptors, and
    closing them all releases every one and leaves each file at rest.
┌───────────────────────┬─────────────┬────────────────┬───────────────────┐
│         stage         │ descriptors │ per_connection │ files_not_at_rest │
├───────────────────────┼─────────────┼────────────────┼───────────────────┤
│ all 200 open and idle │ 600         │ 3.0            │                   │
│ all 200 closed        │ 0           │ 0.0            │ 0                 │
└───────────────────────┴─────────────┴────────────────┴───────────────────┘

B2. What those descriptors point at: one .db, one -wal and one -shm per
    file.
┌──────┬─────────────┬────────────────┬────────────────┐
│ kind │ descriptors │     first      │      last      │
├──────┼─────────────┼────────────────┼────────────────┤
│ db   │ 200         │ db-0000.db     │ db-0199.db     │
│ -wal │ 200         │ db-0000.db-wal │ db-0199.db-wal │
│ -shm │ 200         │ db-0000.db-shm │ db-0199.db-shm │
└──────┴─────────────┴────────────────┴────────────────┘

B3. The memory each connection reports for its schema and its prepared
    statement is the same on every connection. CACHE_USED (page cache)
    also counts the Pager allocation, whose size depends on the length of
    the path, so it varies between machines: info.sql shows it with the
    process memory.
┌─────────────┬─────────────┬────────────┬─────────────────┬───────────────┐
│ connections │ schema_used │ stmt_used  │ schema_used_sum │ stmt_used_sum │
├─────────────┼─────────────┼────────────┼─────────────────┼───────────────┤
│ 200         │ 1347..1347  │ 5008..5008 │ 269400          │ 1001600       │
└─────────────┴─────────────┴────────────┴─────────────────┴───────────────┘

── C. PRAGMA shrink_memory ─────────────────────────────────────────────

C1. shrink_memory on connection 0 alone lowered CACHE_USED on every
    connection (this driver keeps every page cache in one shared group;
    the numbers are in info.sql). It frees cache pages only: schema and
    statement memory are unchanged on every connection.
┌───────────────────────┬─────────────────────┬─────────────┐
│ schema_used_unchanged │ stmt_used_unchanged │ connections │
├───────────────────────┼─────────────────────┼─────────────┤
│ 200                   │ 200                 │ 200         │
└───────────────────────┴─────────────────────┴─────────────┘

── D. Two connections to one WAL file in one process ───────────────────

D1. The second connection adds two descriptors, not three: both share
    one -shm descriptor. When it closes, its database descriptor stays
    open (4, not 3) until the last connection to the file closes.
┌──────────────────────────────────────────┬─────┬──────────┬──────────────────────────────────────┬───────────────────┐
│                  state                   │ fds │ expected │               targets                │    side_files     │
├──────────────────────────────────────────┼─────┼──────────┼──────────────────────────────────────┼───────────────────┤
│ connection 1 open, idle after a read     │ 3   │ 3        │ t.db t.db-shm t.db-wal               │ t.db-wal t.db-shm │
│ connection 2 open too, idle after a read │ 5   │ 5        │ t.db t.db t.db-shm t.db-wal t.db-wal │ t.db-wal t.db-shm │
│ connection 2 closed                      │ 4   │ 4        │ t.db t.db t.db-shm t.db-wal          │ t.db-wal t.db-shm │
│ connection 1 closed                      │ 0   │ 0        │                                      │ none              │
└──────────────────────────────────────────┴─────┴──────────┴──────────────────────────────────────┴───────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  journal_mode=delete: descriptors per state are [1 1 2 1 0]
A  ✓  journal_mode=wal: descriptors per state are [1 3 3 3 0]
B  ✓  200 idle WAL connections hold exactly 3 x 200 = 600 descriptors
B  ✓  every connection reports the same SCHEMA_USED and the same STMT_USED (same schema, same statement)
B  ✓  process memory grew by at least the sum of CACHE_USED + SCHEMA_USED + STMT_USED
B  ✓  process memory grew by at most that sum + 256 KiB per connection (the stated margin for memory the counters do not report)
C  ✓  shrink_memory on connection 0 alone lowered CACHE_USED on all 200 connections (this driver keeps every page cache in one shared group)
C  ✓  shrink_memory leaves SCHEMA_USED and STMT_USED unchanged on every connection
C  ✓  running shrink_memory on the other connections afterwards released nothing more
C  ✓  after shrink every connection's CACHE_USED is under 1/8 of what it was
C  ✓  after closing all 200: 0 descriptors left and every file at rest (no -wal, -shm, -journal)
D  ✓  two connections share one -shm descriptor (5, not 6); the closed connection's database descriptor stays open (4, not 3) until the last connection to the file closes

12 of 12 claims hold

Experiment verdict: IDLE COST OK
```

#### Reading the Experiment 8.2 evidence

- **Settings table (pin and environment):** Pin verified (the experiment's own output); descriptors read from `/proc/self/fd`, memory from `/proc/self/status`; Go raised `RLIMIT_NOFILE` to `soft 1048576, hard 1048576` in the earlier capture (a run-dependent value printed by `course run`).
- **A1 (Part A — descriptors per state):**
  - `journal_mode=delete`: `1` (`t.db`) at open, `1` idle after a read, `2` (`t.db t.db-journal`) inside a write transaction, `1` after `COMMIT`, `0` after close.
  - `journal_mode=wal`: `1` (`t.db`) at open (no side files yet, matching Experiment 8.1's lazy WAL open), then `3` (`t.db t.db-shm t.db-wal`) after the first read and throughout writes and commits, dropping to `0` with side files `none` after close.
- **B1–B3 (Part B — 200 idle WAL connections, each to a 39-page, 159,744-byte recipe step-3 file):**
  - Exactly `600` descriptors (`3.0` per connection): `db` `200`, `-wal` `200`, `-shm` `200`.
  - Every connection reports identical `SCHEMA_USED` (`1,347` bytes) and `STMT_USED` (`5,008` bytes), and `CACHE_USED` of about `52,835` bytes in the lab run (`52,961` in the earlier capture; run-dependent, printed by `course run`, because `Pager` embeds the file path length).
  - **Outline correction (uncounted RSS overhead):** In the earlier capture (RSS and `CACHE_USED` are run-dependent values that `course run` prints), the sum of all three counters across 200 connections is `11.3 MiB` (`10.1 MiB` cache + `0.3 MiB` schema + `1.0 MiB` statements), yet process RSS grew from `9.8 MiB` to `54.0 MiB`—a **`44.2 MiB` growth, `3.91×` the summed counters**. The uncounted portion is `32.9 MiB` (`168 KiB` per connection). Known contributors are the `48 KiB` per-connection lookaside buffer (`src/global.c:218-224`, excluded from all three byte counters), `modernc.org/sqlite` + `modernc.org/libc` per-connection Go/C state, and allocator size-class rounding (4.3 KiB page allocations landing in 8 KiB slots, measured in Chapter 4). Because a fixed per-process component dominates at tiny `n` (`280 KiB/conn` at `-n 5`), Experiment 8.2 requires `-n >= 20`. And because adversarial test `IDL-03a` measured `319–321 KiB/conn` uncounted with 1,000-table schemas, treat `256 KiB` as the bound for this 3-table workload—not a universal constant.
- **C1 (Part C — `PRAGMA shrink_memory`):**
  - **Outline correction (process-wide shrink):** Running `PRAGMA shrink_memory` on connection 0 **alone** dropped connection 0's `CACHE_USED` from about `52,835` to `4,963` bytes in the lab run (`52,961` to `5,089` in the earlier capture) *and* dropped connection 1's (and all 199 other connections') `CACHE_USED` by the same amount (`src/pcache1.c:839-851` shrinks the shared `pcache1` group). Running `PRAGMA shrink_memory` on connections 1..199 afterward released `0` additional bytes.
  - Cache memory fell from `10.1 MiB` to `1.0 MiB` (the floor of about `4,963` bytes per connection is the `Pager` struct plus one page buffer, well below 1/8 of `52,835`), while `SCHEMA_USED` (`0.3 MiB`) and `STMT_USED` (`1.0 MiB`) stayed unchanged. Process RSS dropped only partly (`54.0` → `47.2 MiB` for `9.1 MiB` of freed cache in the earlier capture) because Go/libc allocators retain freed pages for reuse. Closing all 200 connections returned descriptors to `0` and left all 200 files at rest.
- **D1 (Part D — two connections to one WAL file in one process):**
  - Connection 1 open after a read: `3` (`t.db t.db-shm t.db-wal`).
  - Connection 2 open after a read: `5` (`t.db t.db t.db-shm t.db-wal t.db-wal`)—only one `-shm` descriptor is held because `os_unix.c` shares a single `unixShmNode` per inode.
  - Connection 2 closed: `4` (`t.db t.db t.db-shm t.db-wal`)—its `-wal` closed, but its `.db` descriptor stayed parked in `UnixUnusedFd` (`src/os_unix.c:2101-2109`) because connection 1 holds a `SHARED` lock on `t.db` for the lifetime of WAL mode.
  - Connection 1 closed: `0`, side files `none` (`closePendingFds`, `src/os_unix.c:1471-1482`).

---

## 5. Lab

To serve 1,000 databases inside fixed descriptor and memory budgets and prove rung R5 (at-rest state), this chapter introduces two shared packages—`internal/procstat` and `internal/connmgr`—and exercises them in Experiment 8.3. Both packages are inlined in full once here; Chapter 10 imports them directly.

### 5.1 Shared package `internal/procstat`

`internal/procstat` provides portable, standard-library-only measurement of a process's open file descriptors, RSS memory, and `RLIMIT_NOFILE`:

- `DescriptorCount()` and `Descriptors()` open `DescriptorDir` (`/proc/self/fd` on Linux, `/dev/fd` elsewhere) and exclude the directory's own `dir.Fd()` so two consecutive counts are equal and opening *N* files adds exactly *N*.
- `DescriptorsUnder(dir)` and `RelativeTargets(dir, ds)` normalize `dir` with `filepath.Abs` and `filepath.EvalSymlinks` (ensuring relative paths and `"/"` match Linux's absolute `/proc/self/fd` symlink targets without matching a sibling directory like `dir-sibling`). Off Linux, `TargetsSupported` is `false` and `DescriptorsUnder` returns `ErrTargetsUnsupported`.
- `ReadMemory()` parses `/proc/self/status` (`VmRSS`, `RssAnon`, `RssFile`, `RssShmem`) on Linux and `ps -o rss= -p <pid>` elsewhere. Both parsers use `kibToBytes`, rejecting non-numeric, negative, duplicate, or `int64`-overflowing KiB values (above `math.MaxInt64/1024`).
- `FileLimit()` calls `syscall.Getrlimit(syscall.RLIMIT_NOFILE, ...)`.

<!-- include: ../internal/procstat/procstat.go -->
```go
// Package procstat reads the current process's open file descriptors, resident
// memory, and descriptor limit without cgo. On Linux it reads /proc/self/fd
// and /proc/self/status; on other Unix systems it counts /dev/fd and invokes
// ps -o rss=.
package procstat

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Descriptor represents one open file descriptor of the current process.
// Target holds the resolved file path on Linux (/proc/self/fd readlink) and is
// empty on platforms where TargetsSupported is false.
type Descriptor struct {
	FD     int
	Target string
}

// Memory holds the current process's resident memory figures in bytes.
// Unavailable breakdown fields (Anon, File, Shmem on non-Linux systems) are -1.
type Memory struct {
	RSS   int64
	Anon  int64
	File  int64
	Shmem int64
}

// HasBreakdown reports whether the anonymous, file-backed, and shared-memory
// RSS breakdown fields are all populated.
func (m Memory) HasBreakdown() bool {
	return m.Anon >= 0 && m.File >= 0 && m.Shmem >= 0
}

// DescriptorCount returns the number of open file descriptors in the current
// process, excluding the directory descriptor opened while counting them.
func DescriptorCount() (int, error) {
	fds, err := descriptorNumbers()
	if err != nil {
		return 0, err
	}
	return len(fds), nil
}

func descriptorNumbers() ([]int, error) {
	dir, err := os.Open(DescriptorDir)
	if err != nil {
		return nil, fmt.Errorf("procstat: list descriptors: %w", err)
	}
	defer dir.Close()
	// Exclude the directory handle itself so repeated calls return the same
	// count and opening N files increases the count by exactly N.
	own := int(dir.Fd())
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("procstat: list descriptors: %w", err)
	}
	fds := make([]int, 0, len(names))
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			return nil, fmt.Errorf("procstat: unexpected entry %q in %s", name, DescriptorDir)
		}
		if fd != own {
			fds = append(fds, fd)
		}
	}
	sort.Ints(fds)
	return fds, nil
}

// Descriptors returns every open file descriptor in ascending numeric order,
// resolving symlink targets from /proc/self/fd when TargetsSupported is true.
func Descriptors() ([]Descriptor, error) {
	fds, err := descriptorNumbers()
	if err != nil {
		return nil, err
	}
	descriptors := make([]Descriptor, 0, len(fds))
	for _, fd := range fds {
		target := ""
		if TargetsSupported {
			target, err = os.Readlink(filepath.Join(DescriptorDir, strconv.Itoa(fd)))
			if errors.Is(err, os.ErrNotExist) {
				// Skip descriptors closed concurrently between listing and readlink.
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("procstat: read descriptor %d: %w", fd, err)
			}
		}
		descriptors = append(descriptors, Descriptor{FD: fd, Target: target})
	}
	return descriptors, nil
}

// DescriptorsUnder returns all open descriptors whose resolved target lies
// inside dir. It returns ErrTargetsUnsupported on platforms where descriptor
// targets cannot be read.
func DescriptorsUnder(dir string) ([]Descriptor, error) {
	if !TargetsSupported {
		return nil, ErrTargetsUnsupported
	}
	prefix, err := directoryPrefix(dir)
	if err != nil {
		return nil, err
	}
	all, err := Descriptors()
	if err != nil {
		return nil, err
	}
	var under []Descriptor
	for _, d := range all {
		if strings.HasPrefix(d.Target, prefix) {
			under = append(under, d)
		}
	}
	return under, nil
}

// directoryPrefix normalizes dir to an absolute, symlink-resolved path ending
// in a single separator so it matches /proc/self/fd targets and excludes
// sibling directories sharing the same name prefix.
func directoryPrefix(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("procstat: resolve %s: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if strings.HasSuffix(abs, string(filepath.Separator)) {
		return abs, nil
	}
	return abs + string(filepath.Separator), nil
}

// RelativeTargets strips dir's resolved prefix from each descriptor target and
// returns the resulting paths sorted lexically.
func RelativeTargets(dir string, descriptors []Descriptor) []string {
	prefix, err := directoryPrefix(dir)
	if err != nil {
		prefix = filepath.Clean(dir) + string(filepath.Separator)
	}
	targets := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		targets = append(targets, strings.TrimPrefix(d.Target, prefix))
	}
	sort.Strings(targets)
	return targets
}

// ErrTargetsUnsupported is returned by DescriptorsUnder on operating systems
// whose descriptor directory does not expose target paths as symlinks.
var ErrTargetsUnsupported = errors.New("procstat: descriptor targets are only readable from /proc/self/fd on Linux")

// FileLimit returns the process's current soft and hard RLIMIT_NOFILE bounds.
func FileLimit() (soft, hard uint64, err error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, 0, fmt.Errorf("procstat: getrlimit: %w", err)
	}
	return uint64(limit.Cur), uint64(limit.Max), nil
}

func parseKiB(value string) (int64, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 || len(fields) > 2 || (len(fields) == 2 && fields[1] != "kB") {
		return 0, fmt.Errorf("procstat: malformed size %q", value)
	}
	return kibToBytes(fields[0])
}

// kibToBytes converts a non-negative KiB count to bytes, rejecting values that
// would overflow int64 when multiplied by 1024.
func kibToBytes(text string) (int64, error) {
	kib, err := strconv.ParseInt(text, 10, 64)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("procstat: malformed size %q", text)
	}
	if kib > math.MaxInt64/1024 {
		return 0, fmt.Errorf("procstat: size %s KiB does not fit in int64 bytes", text)
	}
	return kib * 1024, nil
}

// ParseStatus parses Linux /proc/self/status text and returns resident memory
// figures in bytes. VmRSS is required; RssAnon, RssFile, and RssShmem remain
// -1 if absent. Duplicate fields, unknown units, and overflowing sizes fail.
func ParseStatus(text string) (Memory, error) {
	m := Memory{RSS: -1, Anon: -1, File: -1, Shmem: -1}
	fields := map[string]*int64{"VmRSS": &m.RSS, "RssAnon": &m.Anon, "RssFile": &m.File, "RssShmem": &m.Shmem}
	for _, line := range strings.Split(text, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		target, wanted := fields[name]
		if !wanted {
			continue
		}
		if *target != -1 {
			return Memory{}, fmt.Errorf("procstat: %s appears twice", name)
		}
		bytes, err := parseKiB(value)
		if err != nil {
			return Memory{}, fmt.Errorf("procstat: %s: %w", name, err)
		}
		*target = bytes
	}
	if m.RSS < 0 {
		return Memory{}, errors.New("procstat: no VmRSS line")
	}
	return m, nil
}

// ParsePSRSS parses the single KiB integer printed by "ps -o rss= -p <pid>"
// into a Memory value with RSS populated and breakdown fields set to -1.
func ParsePSRSS(output string) (Memory, error) {
	text := strings.TrimSpace(output)
	if text == "" || strings.ContainsAny(text, " \t\n") {
		return Memory{}, fmt.Errorf("procstat: malformed ps rss output %q", output)
	}
	bytes, err := kibToBytes(text)
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: ps rss output %q: %w", output, err)
	}
	return Memory{RSS: bytes, Anon: -1, File: -1, Shmem: -1}, nil
}
```

<!-- include: ../internal/procstat/procstat_linux.go -->
```go
//go:build linux

package procstat

import (
	"fmt"
	"os"
)

// DescriptorDir is the directory listing open file descriptors for the current
// process on Linux.
const DescriptorDir = "/proc/self/fd"

// TargetsSupported reports whether entries under DescriptorDir are symlinks
// whose file targets can be read with os.Readlink.
const TargetsSupported = true

// MemorySource names the kernel interface ReadMemory reads on Linux.
const MemorySource = "/proc/self/status"

// ReadMemory reads VmRSS and its Anon, File, and Shmem breakdown from
// /proc/self/status.
func ReadMemory() (Memory, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: %w", err)
	}
	return ParseStatus(string(data))
}
```

<!-- include: ../internal/procstat/procstat_other.go -->
```go
//go:build !linux

package procstat

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// DescriptorDir is the directory listing open file descriptors on non-Linux
// Unix platforms (such as macOS fdesc).
const DescriptorDir = "/dev/fd"

// TargetsSupported is false off Linux because /dev/fd entries are not symlinks
// pointing to file paths.
const TargetsSupported = false

// MemorySource names the external command ReadMemory runs off Linux.
const MemorySource = "ps -o rss= -p <pid>"

// ReadMemory queries the current process's resident set size via ps.
func ReadMemory() (Memory, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: ps: %w", err)
	}
	return ParsePSRSS(string(out))
}
```

### 5.2 Shared package `internal/connmgr`

`internal/connmgr` implements a bounded LRU connection manager and the strict R5 at-rest verifier `CheckAtRest`:

- **`CheckAtRest(path)` (`atrest.go`):** Refuses to call a file at rest if: (1) `.db` is missing, (2) `-wal` exists with size `> 0` (a 0-byte `-wal` carries no frames), (3) a hot rollback `-journal` exists (nonzero size and nonzero first byte, matching `hasHotJournal`), (4) `-shm` exists at any size, or (5) another process holds a POSIX lock on SQLite's lock region (`0x40000000`, length `512` bytes: `PENDING`, `RESERVED`, and the 510-byte `SHARED` range), probed via `fcntl(F_GETLK)` on the same file descriptor that hashes `.db`. Because `CheckAtRest` opens and closes `path` (which drops the calling process's own POSIX locks on Unix), it must only run when this process has no open connection to `path`.
- **`Manager` (`connmgr.go`):** Keeps at most `Capacity` open `*sqlitex.DB` entries in an LRU list protected by one mutex.
  - `Acquire(ctx, id)` returns an existing open entry immediately (moving it to the front of the LRU and bumping `Hits`). On a miss, it checks `ctx.Err()` **before** evicting any idle database, evicts the least recently used entry with `refs == 0` if at capacity (or blocks until `Handle.Release()` or `ctx.Done()`), runs `verifyBeforeOpen` if an at-rest record exists for `id` (returning `*ChangedWhileClosedError` on any mismatch or non-at-rest state, or counting `StatusFailures` on an I/O error), and opens the file. `Misses` and `VerifiedReopens`/`UnverifiedOpens` increment only after `Open` succeeds, preserving `Misses == VerifiedReopens + UnverifiedOpens`.
  - `evictLocked` calls `db.Close()`, then checks both `CloseErr != nil` and `db.Stats().OpenConnections > 0`. If either is true—for example, when an unclosed `*sql.Rows` kept a connection checked out in `database/sql`—it skips `CheckAtRest` (avoiding the POSIX lock-drop hazard!), marks the eviction not at rest, and drops any stored hash record. Only when `OpenConnections == 0` does it call `CheckAtRest(path)` and save the SHA-256 record.
  - `OnEvict` hooks run after releasing `m.mu`, so hooks may safely call `Stats()` or `Record()`. `Forget(id)` clears a stored record for a deliberate external change such as a restore.

<!-- include: ../internal/connmgr/atrest.go -->
```go
package connmgr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const (
	// SQLite places its Unix POSIX advisory locks starting at PENDING_BYTE
	// (0x40000000): PENDING (1 byte), RESERVED (1 byte), and SHARED (510 bytes)
	// in src/os.h:160-166 and src/os_unix.c:1866-2095.
	sqliteLockRegionStart  = 0x40000000
	sqliteLockRegionLength = 2 + 510
)

// Status describes whether a database file is at rest (a complete, unheld
// single file safe to copy) along with its SHA-256 and byte size when AtRest
// is true, or the reasons it is not at rest in Problems.
type Status struct {
	Path     string
	AtRest   bool
	Problems []string
	SHA256   string
	Size     int64
}

// String formats Status as either an at-rest summary (size and sha256) or a
// semicolon-separated list of problems.
func (s Status) String() string {
	if s.AtRest {
		return fmt.Sprintf("at rest, %d bytes, sha256 %s", s.Size, s.SHA256)
	}
	return "not at rest: " + strings.Join(s.Problems, "; ")
}

// CheckAtRest verifies that path is a single complete database file safe to
// copy: the file exists, no non-empty -wal remains, no hot rollback -journal
// exists, no -shm exists, and no other process holds a POSIX lock on SQLite's
// lock byte range. When all checks pass, it hashes the file on the same
// descriptor and returns AtRest=true with SHA256 and Size populated.
//
// Hazard: CheckAtRest opens and closes path. On Unix, closing any descriptor
// to an inode releases every POSIX lock the current process holds on that
// inode, so callers must invoke CheckAtRest only when this process has no open
// SQLite connection to path.
func CheckAtRest(path string) (Status, error) {
	status := Status{Path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		status.Problems = append(status.Problems, "database file missing")
		return status, nil
	} else if err != nil {
		return status, err
	}
	// A zero-byte -wal (e.g. left by wal_checkpoint(TRUNCATE)) holds no frames;
	// any positive size may hold committed frames not yet checkpointed.
	wal, err := os.Stat(path + "-wal")
	switch {
	case err == nil && wal.Size() > 0:
		status.Problems = append(status.Problems, fmt.Sprintf("-wal holds %d bytes: the database file alone may lack committed transactions", wal.Size()))
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return status, err
	}
	hot, journalSize, err := hotJournal(path + "-journal")
	if err != nil {
		return status, err
	}
	if hot {
		status.Problems = append(status.Problems, fmt.Sprintf("hot -journal (%d bytes): the next open rolls back an interrupted transaction", journalSize))
	}
	// In normal WAL mode, sqlite3WalClose (src/wal.c:2507-2571) unlinks -shm
	// via walIndexClose (src/wal.c:1619-1630) when the last connection closes
	// cleanly unless PERSIST_WAL is set.
	shm, err := os.Stat(path + "-shm")
	switch {
	case err == nil:
		status.Problems = append(status.Problems, fmt.Sprintf("-shm exists (%d bytes): a connection is open or did not close cleanly", shm.Size()))
	case !errors.Is(err, os.ErrNotExist):
		return status, err
	}
	if len(status.Problems) > 0 {
		return status, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return status, err
	}
	defer file.Close()
	// Side-file checks alone miss an exclusive-mode WAL connection (which keeps
	// its wal-index in heap memory without creating -shm) and an active
	// rollback-mode reader in another process; both hold locks on the db file.
	locked, err := lockedByAnotherProcess(file)
	if err != nil {
		return status, err
	}
	if locked {
		status.Problems = append(status.Problems, "another process holds a SQLite lock on the database file: it has the database open")
		return status, nil
	}
	sum, size, err := fileSHA256(file)
	if err != nil {
		return status, err
	}
	status.AtRest, status.SHA256, status.Size = true, sum, size
	return status, nil
}

// lockedByAnotherProcess uses fcntl(F_GETLK) to test whether a write lock over
// SQLite's PENDING/RESERVED/SHARED byte range would conflict with a lock held
// by another process. POSIX F_GETLK never reports locks held by the calling
// process itself.
func lockedByAnotherProcess(file *os.File) (bool, error) {
	probe := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: 0,
		Start:  sqliteLockRegionStart,
		Len:    sqliteLockRegionLength,
	}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &probe); err != nil {
		return false, fmt.Errorf("connmgr: probe locks on %s: %w", file.Name(), err)
	}
	return probe.Type != syscall.F_UNLCK, nil
}

func fileSHA256(file *os.File) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// hotJournal mirrors SQLite's hot-journal header check (hasHotJournal,
// src/pager.c:5180-5271): a journal file that exists, has nonzero length, and
// starts with a nonzero byte (not zeroed by PERSIST mode) represents an
// interrupted write transaction.
func hotJournal(path string) (bool, int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, 0, err
	}
	if info.Size() == 0 {
		return false, 0, nil
	}
	first := make([]byte, 1)
	if _, err := file.ReadAt(first, 0); err != nil {
		return false, info.Size(), err
	}
	return first[0] != 0, info.Size(), nil
}
```

<!-- include: ../internal/connmgr/connmgr.go -->
```go
// Package connmgr provides a bounded LRU connection manager for services that
// hold many SQLite database files on one machine. At most Capacity databases
// stay open at once; on eviction each closed database is verified at rest and
// its SHA-256 recorded so unexpected changes while closed are caught on reopen.
package connmgr

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

var (
	// ErrClosed is returned when Acquire or Close is called on a closed Manager.
	ErrClosed = errors.New("connmgr: manager is closed")
	// ErrInUse is returned by Evict or Close when at least one Handle has not been released.
	ErrInUse = errors.New("connmgr: database is in use")
	// ErrNotOpen is returned by Evict when the requested database ID is not currently open.
	ErrNotOpen = errors.New("connmgr: database is not open")
)

// ChangedWhileClosedError is returned by Acquire when a previously evicted
// database is no longer at rest or its size or SHA-256 differs from the
// Status recorded when the manager closed it.
type ChangedWhileClosedError struct {
	ID       string
	Path     string
	Recorded Status
	Found    Status
}

// Error formats the mismatch between the recorded at-rest digest and what
// CheckAtRest observed before reopening.
func (e *ChangedWhileClosedError) Error() string {
	if !e.Found.AtRest {
		return fmt.Sprintf("connmgr: %s changed while closed: recorded at rest with sha256 %s, now %s", e.ID, e.Recorded.SHA256, e.Found)
	}
	return fmt.Sprintf("connmgr: %s changed while closed: recorded sha256 %s, file now has sha256 %s", e.ID, e.Recorded.SHA256, e.Found.SHA256)
}

// Cause identifies why a database connection was evicted from a Manager.
type Cause string

const (
	// CauseCapacity means an idle database was closed to free a slot for Acquire.
	CauseCapacity Cause = "capacity"
	// CauseExplicit means the caller invoked Manager.Evict.
	CauseExplicit Cause = "explicit"
	// CauseShutdown means Manager.Close closed every remaining database.
	CauseShutdown Cause = "shutdown"
)

// Eviction reports the outcome of closing one managed database and checking
// whether it landed at rest.
type Eviction struct {
	ID        string
	Path      string
	Cause     Cause
	CloseErr  error
	Status    Status
	StatusErr error
}

// Options configures a Manager. Capacity (>= 1), PathFor, and Open are required.
type Options struct {
	Capacity int
	PathFor  func(id string) (string, error)
	Open     func(ctx context.Context, path string) (*sqlitex.DB, error)
	OnEvict  func(Eviction)
}

// Stats holds cumulative counters and current occupancy for a Manager.
// Misses always equals VerifiedReopens + UnverifiedOpens.
type Stats struct {
	Open              int
	MaxOpen           int
	Hits              int
	Misses            int
	Evictions         int
	EvictedAtRest     int
	EvictedNotAtRest  int
	VerifiedReopens   int
	UnverifiedOpens   int
	RefusedReopens    int
	CapacityWaits     int
	OpenFailures      int
	CloseFailures     int
	StatusFailures    int
	DistinctDatabases int
}

type entry struct {
	id   string
	path string
	db   *sqlitex.DB
	refs int
	elem *list.Element
}

// Manager keeps at most Options.Capacity SQLite databases open at once,
// evicting the least recently used idle database when a new ID is acquired.
// One mutex serializes open, close, and hash checks.
type Manager struct {
	options  Options
	mu       sync.Mutex
	open     map[string]*entry
	lru      *list.List
	records  map[string]Status
	seen     map[string]bool
	stats    Stats
	released chan struct{}
	closed   bool
}

// Handle is a lease on one open database managed by a Manager. Callers must
// close all *sql.Rows, *sql.Tx, and *sql.Conn derived from DB before calling
// Release so a subsequent eviction can actually close the SQLite connection.
type Handle struct {
	ID       string
	Path     string
	DB       *sqlitex.DB
	manager  *Manager
	entry    *entry
	released bool
}

// New validates options and returns a ready Manager with zero open databases.
func New(options Options) (*Manager, error) {
	switch {
	case options.Capacity < 1:
		return nil, fmt.Errorf("connmgr: capacity %d, need at least 1", options.Capacity)
	case options.PathFor == nil:
		return nil, errors.New("connmgr: PathFor is required")
	case options.Open == nil:
		return nil, errors.New("connmgr: Open is required")
	}
	return &Manager{
		options:  options,
		open:     make(map[string]*entry),
		lru:      list.New(),
		records:  make(map[string]Status),
		seen:     make(map[string]bool),
		released: make(chan struct{}),
	}, nil
}

// Acquire returns a Handle for id, reusing an already-open connection when
// present. On a miss it evicts the least recently used idle database if at
// capacity (or waits until a Handle is released or ctx expires), verifies any
// recorded at-rest hash, and opens the database.
func (m *Manager) Acquire(ctx context.Context, id string) (*Handle, error) {
	var evictions []Eviction
	// Invoke OnEvict after releasing m.mu so hooks may call Manager methods.
	defer func() { m.report(evictions) }()
	m.mu.Lock()
	waited := false
	for {
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		if e, ok := m.open[id]; ok {
			e.refs++
			m.lru.MoveToFront(e.elem)
			m.stats.Hits++
			m.mu.Unlock()
			return m.handle(e), nil
		}
		// Reject a cancelled or expired context before evicting a healthy idle
		// database that would not be replaced.
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if len(m.open) < m.options.Capacity {
			break
		}
		if victim := m.leastRecentlyUsedIdle(); victim != nil {
			evictions = append(evictions, m.evictLocked(victim, CauseCapacity))
			continue
		}
		if !waited {
			m.stats.CapacityWaits++
			waited = true
		}
		wake := m.released
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	path, err := m.options.PathFor(id)
	if err != nil {
		m.stats.OpenFailures++
		return nil, fmt.Errorf("connmgr: path for %s: %w", id, err)
	}
	verified, err := m.verifyBeforeOpen(id, path)
	if err != nil {
		return nil, err
	}
	db, err := m.options.Open(ctx, path)
	if err != nil {
		m.stats.OpenFailures++
		return nil, fmt.Errorf("connmgr: open %s: %w", id, err)
	}
	// Update miss and open-kind counters only after Open succeeds so
	// Misses == VerifiedReopens + UnverifiedOpens holds across Open failures.
	m.stats.Misses++
	if verified {
		m.stats.VerifiedReopens++
	} else {
		m.stats.UnverifiedOpens++
	}
	if !m.seen[id] {
		m.seen[id] = true
		m.stats.DistinctDatabases++
	}
	e := &entry{id: id, path: path, db: db, refs: 1}
	e.elem = m.lru.PushFront(e)
	m.open[id] = e
	m.stats.Open = len(m.open)
	m.stats.MaxOpen = max(m.stats.MaxOpen, m.stats.Open)
	return m.handle(e), nil
}

func (m *Manager) handle(e *entry) *Handle {
	return &Handle{ID: e.id, Path: e.path, DB: e.db, manager: m, entry: e}
}

func (m *Manager) verifyBeforeOpen(id, path string) (bool, error) {
	recorded, ok := m.records[id]
	if !ok {
		return false, nil
	}
	found, err := CheckAtRest(path)
	if err != nil {
		m.stats.StatusFailures++
		return false, fmt.Errorf("connmgr: check %s before reopening: %w", id, err)
	}
	if !found.AtRest || found.SHA256 != recorded.SHA256 || found.Size != recorded.Size {
		m.stats.RefusedReopens++
		return false, &ChangedWhileClosedError{ID: id, Path: path, Recorded: recorded, Found: found}
	}
	return true, nil
}

// Release returns the Handle to the Manager, allowing the database to be
// evicted once its reference count reaches zero and waking any waiting Acquire
// calls. Calling Release more than once is a no-op.
func (h *Handle) Release() {
	m := h.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	h.entry.refs--
	close(m.released)
	m.released = make(chan struct{})
}

func (m *Manager) leastRecentlyUsedIdle() *entry {
	for elem := m.lru.Back(); elem != nil; elem = elem.Prev() {
		if e := elem.Value.(*entry); e.refs == 0 {
			return e
		}
	}
	return nil
}

func (m *Manager) evictLocked(e *entry, cause Cause) Eviction {
	m.lru.Remove(e.elem)
	delete(m.open, e.id)
	m.stats.Open = len(m.open)
	m.stats.Evictions++
	eviction := Eviction{ID: e.id, Path: e.path, Cause: cause}
	if err := e.db.Close(); err != nil {
		eviction.CloseErr = err
		m.stats.CloseFailures++
	}
	// Skip CheckAtRest whenever Close failed or database/sql still holds a
	// checked-out connection (unclosed *sql.Rows, *sql.Tx, or *sql.Conn):
	// (*sql.DB).Close closes only idle connections and returns nil, while
	// CheckAtRest's open+close would drop the live connection's POSIX locks.
	switch stillOpen := e.db.Stats().OpenConnections; {
	case eviction.CloseErr != nil:
		eviction.Status = Status{Path: e.path, Problems: []string{fmt.Sprintf("close failed: %v", eviction.CloseErr)}}
	case stillOpen > 0:
		eviction.Status = Status{Path: e.path, Problems: []string{fmt.Sprintf("%d connection(s) still open in this process: a statement, transaction or *sql.Conn was not closed, so database/sql closes the connection only when it is returned", stillOpen)}}
	default:
		eviction.Status, eviction.StatusErr = CheckAtRest(e.path)
	}
	switch {
	case eviction.StatusErr != nil:
		m.stats.StatusFailures++
		delete(m.records, e.id)
	case eviction.Status.AtRest:
		m.stats.EvictedAtRest++
		m.records[e.id] = eviction.Status
	default:
		m.stats.EvictedNotAtRest++
		delete(m.records, e.id)
	}
	return eviction
}

func (m *Manager) report(evictions []Eviction) {
	if m.options.OnEvict == nil {
		return
	}
	for _, eviction := range evictions {
		m.options.OnEvict(eviction)
	}
}

// Evict closes the idle open database identified by id, checks whether it is
// at rest, records its digest if so, and runs OnEvict. It returns ErrNotOpen
// if id is not open or ErrInUse if a Handle is still held.
func (m *Manager) Evict(id string) (Eviction, error) {
	m.mu.Lock()
	e, ok := m.open[id]
	if !ok {
		m.mu.Unlock()
		return Eviction{}, ErrNotOpen
	}
	if e.refs > 0 {
		m.mu.Unlock()
		return Eviction{}, ErrInUse
	}
	eviction := m.evictLocked(e, CauseExplicit)
	m.mu.Unlock()
	m.report([]Eviction{eviction})
	return eviction, nil
}

// Close evicts every open database and marks the Manager closed. It returns
// ErrInUse without closing anything if any Handle is still outstanding, and
// ErrClosed if called a second time.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	var busy []string
	for elem := m.lru.Front(); elem != nil; elem = elem.Next() {
		if e := elem.Value.(*entry); e.refs > 0 {
			busy = append(busy, e.id)
		}
	}
	if len(busy) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("%w: %d handle(s) not released: %v", ErrInUse, len(busy), busy)
	}
	var evictions []Eviction
	for m.lru.Len() > 0 {
		evictions = append(evictions, m.evictLocked(m.lru.Back().Value.(*entry), CauseShutdown))
	}
	m.closed = true
	close(m.released)
	m.mu.Unlock()
	m.report(evictions)
	var errs []error
	for _, eviction := range evictions {
		if eviction.CloseErr != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", eviction.ID, eviction.CloseErr))
		}
		if eviction.StatusErr != nil {
			errs = append(errs, fmt.Errorf("check %s: %w", eviction.ID, eviction.StatusErr))
		}
	}
	return errors.Join(errs...)
}

// Stats returns a snapshot of the Manager's current counters.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// Record returns the at-rest Status recorded when id was last evicted cleanly.
func (m *Manager) Record(id string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, ok := m.records[id]
	return status, ok
}

// OpenIDs returns the IDs of currently open databases from most recently used
// to least recently used.
func (m *Manager) OpenIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, m.lru.Len())
	for elem := m.lru.Front(); elem != nil; elem = elem.Next() {
		ids = append(ids, elem.Value.(*entry).id)
	}
	return ids
}

// Forget drops the stored at-rest record for id so a deliberate external change
// (such as a restore) can be opened without triggering ChangedWhileClosedError.
func (m *Manager) Forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.records[id]
	delete(m.records, id)
	return ok
}
```

### 5.3 Experiment 8.3 — `bounded-manager` (rung R5)

- **Claim:** A bounded LRU manager capped at `K` open connections serves 1,000 WAL databases using at most `3K` descriptors (`3` per open connection at every sample, `0` after `Close`) and bounded process memory (`<= K * (2 * cache budget + 256 KiB) + 32 MiB`), verifying every reopen against the SHA-256 recorded at eviction and producing exact recipe states for all 1,000 files. Conversely, a closed WAL database is **not** a single checkpointed file at rest when another process still holds it open (B1), when an unclosed `*sql.Rows` keeps a `database/sql` connection checked out (B2), when a writer is killed before closing (B3), when `SQLITE_FCNTL_PERSIST_WAL` is enabled (B4), or when the file is renamed while open (B5); and an outside write while closed is caught on reopen by its changed SHA-256 (B6).
- **Evidence form:** `BOUND` for memory, `EXACT` for descriptor counts, eviction/reopen counters, SHA-256 hashes, recipe matches, and Part B outcomes, and `SHAPE` for bounded-vs-unbounded RSS contrast and warm-vs-cold latency.
- **Portability:** Fully portable across Linux and macOS (re-executes itself for child processes; no `strace` or `/proc/self/fd` target names required). Captured evidence below was measured on Linux.

<!-- include: experiments/8.3-bounded-manager/main.go -->
```go
// Experiment 8.3 demonstrates Rebuild Ladder rung R5 (at-rest state): serving
// 1,000 WAL databases through an LRU-bounded connection manager capped at K
// open connections versus an unbounded run (Part A), and six concrete cases
// where a closed WAL database is not a single checkpointed file at rest or
// changes while closed (Part B).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 8.3. Each stream of Part A
// runs in a child process that reports its counts to the parent as one
// RESULT line of JSON; only the parent writes tables.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/procstat"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	uncountedPerConnection = 256 * 1024
	memoryConstant         = 32 << 20
	readQuery              = "SELECT count(*), coalesce(sum(balance), 0), (SELECT user_version FROM pragma_user_version) FROM accounts"
)

var (
	databases     = flag.Int("dbs", 1000, "database files")
	capacity      = flag.Int("k", 64, "connections the bounded manager keeps open")
	operations    = flag.Int("ops", 4000, "operations in the seeded stream")
	seed          = flag.Uint64("seed", 1, "seed of the operation stream")
	maxStep       = flag.Int("max-step", 4, "last recipe step a write may apply")
	writePercent  = flag.Int("write-percent", 20, "share of operations that apply the next recipe step")
	cacheSize     = flag.Int("cache-size", -256, "PRAGMA cache_size of every managed connection")
	sampleEvery   = flag.Int("sample-every", 10, "operations between descriptor samples")
	memoryEvery   = flag.Int("memory-every", 100, "operations between process-memory samples")
	hotPercent    = flag.Int("hot-percent", 50, "share of operations that go to the hot 5% of databases")
	modeFlag      = flag.String("mode", "", "internal: run one stream in this process (bounded or unbounded)")
	runDirFlag    = flag.String("dir", "", "internal: directory of the stream's database files")
	childRoleFlag = flag.String("role", "", "internal: child role for part B")
)

// Every measurement goes to a CSV table in <work dir>/results/. part labels
// the claims written while a part runs; caseID labels the part B observations.
var (
	set    *results.Set
	part   string
	caseID string
	tables struct {
		setup, streams, uncleanEvictions, observations, problems, infoStreams, infoLimits *results.Table
	}
	setupSeq, observationSeq, problemSeq int
)

func openTables() {
	tables.setup = set.Table("setup", "seq", "name", "value")
	tables.streams = set.Table("streams", "mode", "capacity", "operations", "reads", "writes", "distinct_dbs",
		"hits", "misses", "evictions", "evicted_at_rest", "evicted_not_at_rest", "verified_reopens", "refused_reopens", "first_opens", "peak_open",
		"fd_samples", "peak_fds", "fd_mismatches", "fds_after_close", "read_mismatches", "files_exact", "files_masked", "files_wrong",
		"warm_reads", "cold_reads", "memory_budget")
	tables.uncleanEvictions = set.Table("unclean_evictions", "mode", "status")
	tables.observations = set.Table("observations", "seq", "case_id", "moment", "file", "at_rest", "bytes", "sha256", "recipe_step", "exact", "note")
	tables.problems = set.Table("problems", "seq", "observation", "problem", "why")
	tables.infoStreams = set.Table("info_streams", "mode", "peak_rss", "warm_ns", "cold_ns", "elapsed_ms")
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

var stepHashes []recipe.FileHashes

// matchStep compares a closed database file against the recipe's cached step
// hashes (computed once per process so matching 1,000 files stays sub-second)
// and accepts change-counter-only differences when header bytes 24-27 match
// bytes 92-95 (multiple steps applied on one WAL connection).
func matchStep(ctx context.Context, path string) (recipe.Match, error) {
	if stepHashes == nil {
		hashes, err := theRecipe().StepHashes(ctx)
		if err != nil {
			return recipe.Match{Step: -1}, err
		}
		stepHashes = hashes
	}
	if err := recipe.CheckAtRest(path); err != nil {
		return recipe.Match{Step: -1}, err
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return recipe.Match{Step: -1}, err
	}
	match := recipe.MatchHashes(stepHashes, recipe.HashImage(image))
	if match.Found() && !match.Exact && !recipe.ChangeCountersAgree(image) {
		return recipe.Match{Step: -1}, nil
	}
	return match, nil
}

func theRecipe() recipe.Recipe { return recipe.Default().WithSteps(*maxStep) }

func openOptions() sqlitex.Options {
	options := theRecipe().SQLiteOptions()
	options.Conn.CacheSize = *cacheSize
	return options
}

func openManaged(ctx context.Context, path string) (*sqlitex.DB, error) {
	return sqlitex.Open(ctx, path, openOptions())
}

func cacheBudgetBytes() int64 {
	if *cacheSize < 0 {
		return int64(-*cacheSize) * 1024
	}
	return int64(*cacheSize) * 4096
}

func dbID(i int) string { return fmt.Sprintf("db-%04d", i) }

func pathFor(dir string) func(string) (string, error) {
	return func(id string) (string, error) {
		var n int
		if _, err := fmt.Sscanf(id, "db-%04d", &n); err != nil || dbID(n) != id || n < 0 || n >= *databases {
			return "", fmt.Errorf("unknown database id %q", id)
		}
		return filepath.Join(dir, id+".db"), nil
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func populate(master, dir string, n int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	for i := range n {
		if err := copyFile(master, filepath.Join(dir, dbID(i)+".db")); err != nil {
			fatal("copy", err)
		}
	}
}

type operation struct {
	db    int
	write bool
}

func stream() []operation {
	rng := rand.New(rand.NewPCG(*seed, 83))
	hot := max(1, *databases/20)
	steps := make([]int, *databases)
	ops := make([]operation, *operations)
	for i := range ops {
		db := rng.IntN(*databases)
		if rng.IntN(100) < *hotPercent {
			db = rng.IntN(hot)
		}
		write := rng.IntN(100) < *writePercent && steps[db] < *maxStep
		if write {
			steps[db]++
		}
		ops[i] = operation{db: db, write: write}
	}
	return ops
}

func descriptorCount() int {
	n, err := procstat.DescriptorCount()
	if err != nil {
		fatal("descriptors", err)
	}
	return n
}

func settledRSS() int64 {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := procstat.ReadMemory()
	if err != nil {
		fatal("memory", err)
	}
	return m.RSS
}

// streamResult is what a stream child reports to its parent.
type streamResult struct {
	Mode             string
	Capacity         int
	Reads, Writes    int
	Stats            connmgr.Stats
	EvictedAtRest    int
	EvictedNotAtRest int
	NotAtRest        []string
	FDSamples        int
	PeakFDs          int
	FDMismatches     int
	FDsAfterClose    int
	ReadMismatches   int
	Exact            int
	Masked           int
	Wrong            int
	WarmReads        int
	ColdReads        int
	PeakRSS          int64
	Budget           int64
	WarmNs, ColdNs   int64
	ElapsedMs        int64
}

func runStream(ctx context.Context, mode, dir string) {
	limit := *capacity
	if mode == "unbounded" {
		limit = *databases
	}
	var evictedAtRest, evictedNotAtRest int
	var notAtRestReasons []string
	manager, err := connmgr.New(connmgr.Options{
		Capacity: limit,
		PathFor:  pathFor(dir),
		Open:     openManaged,
		OnEvict: func(e connmgr.Eviction) {
			if e.CloseErr != nil || e.StatusErr != nil {
				fatal("evict "+e.ID, errors.Join(e.CloseErr, e.StatusErr))
			}
			if e.Status.AtRest {
				evictedAtRest++
			} else {
				evictedNotAtRest++
				notAtRestReasons = append(notAtRestReasons, e.ID+": "+e.Status.String())
			}
		},
	})
	if err != nil {
		fatal("manager", err)
	}
	r := theRecipe()
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	model := make([]int, *databases)
	baseFDs := descriptorCount()
	baseRSS := settledRSS()
	maxFDs, maxRSS, fdSamples, fdMismatches, readMismatches, writes, reads := 0, int64(0), 0, 0, 0, 0, 0
	var warm, cold time.Duration
	var warmCount, coldCount int
	// Each sample runs between operations, where every open WAL connection
	// holds exactly three descriptors (.db, -wal, -shm).
	sampleFDs := func() {
		fds := descriptorCount() - baseFDs
		fdSamples++
		maxFDs = max(maxFDs, fds)
		if fds != 3*manager.Stats().Open {
			fdMismatches++
		}
	}
	sampleRSS := func() {
		m, err := procstat.ReadMemory()
		if err != nil {
			fatal("memory", err)
		}
		maxRSS = max(maxRSS, m.RSS-baseRSS)
	}
	started := time.Now()
	for i, op := range stream() {
		id := dbID(op.db)
		missesBefore := manager.Stats().Misses
		began := time.Now()
		h, err := manager.Acquire(ctx, id)
		if err != nil {
			fatal("acquire "+id, err)
		}
		if op.write {
			if err := recipe.ApplyStep(ctx, h.DB, plan[model[op.db]+1]); err != nil {
				fatal("write "+id, err)
			}
			model[op.db]++
			writes++
		} else {
			var rows, balance int64
			var version int
			if err := h.DB.QueryRowContext(ctx, readQuery).Scan(&rows, &balance, &version); err != nil {
				fatal("read "+id, err)
			}
			if version != model[op.db] {
				readMismatches++
			}
			reads++
			if manager.Stats().Misses > missesBefore {
				cold += time.Since(began)
				coldCount++
			} else {
				warm += time.Since(began)
				warmCount++
			}
		}
		h.Release()
		if (i+1)%*sampleEvery == 0 {
			sampleFDs()
		}
		if (i+1)%*memoryEvery == 0 {
			sampleRSS()
		}
	}
	sampleFDs()
	sampleRSS()
	elapsed := time.Since(started)
	stats := manager.Stats()
	if err := manager.Close(); err != nil {
		fatal("close manager", err)
	}
	afterClose := descriptorCount() - baseFDs
	exact, masked, wrong := 0, 0, 0
	for i := range *databases {
		match, err := matchStep(ctx, filepath.Join(dir, dbID(i)+".db"))
		if err != nil {
			fatal("match "+dbID(i), err)
		}
		switch {
		case match.Step != model[i]:
			wrong++
		case match.Exact:
			exact++
		default:
			masked++
		}
	}
	budget := int64(limit)*(2*cacheBudgetBytes()+uncountedPerConnection) + memoryConstant
	result := streamResult{
		Mode: mode, Capacity: limit, Reads: reads, Writes: writes, Stats: stats,
		EvictedAtRest: evictedAtRest, EvictedNotAtRest: evictedNotAtRest, NotAtRest: notAtRestReasons,
		FDSamples: fdSamples, PeakFDs: maxFDs, FDMismatches: fdMismatches, FDsAfterClose: afterClose, ReadMismatches: readMismatches,
		Exact: exact, Masked: masked, Wrong: wrong, WarmReads: warmCount, ColdReads: coldCount,
		PeakRSS: maxRSS, Budget: budget, WarmNs: nanos(warm, warmCount), ColdNs: nanos(cold, coldCount), ElapsedMs: elapsed.Milliseconds(),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		fatal("json", err)
	}
	fmt.Printf("RESULT %s\n", encoded)
}

func nanos(total time.Duration, n int) int64 {
	if n == 0 {
		return 0
	}
	return int64(total) / int64(n)
}

func runChild(ctx context.Context, mode, dir string) streamResult {
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	args := []string{"-mode", mode, "-dir", dir}
	flag.Visit(func(f *flag.Flag) {
		if f.Name != "mode" && f.Name != "dir" && f.Name != "role" {
			args = append(args, "-"+f.Name, f.Value.String())
		}
	})
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fatal("pipe", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start "+mode, err)
	}
	var result streamResult
	found := false
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if encoded, ok := strings.CutPrefix(line, "RESULT "); ok {
			if err := json.Unmarshal([]byte(encoded), &result); err != nil {
				fatal("parse "+line, err)
			}
			found = true
			continue
		}
		fmt.Println(line)
	}
	if err := scanner.Err(); err != nil {
		fatal("read child", err)
	}
	if err := cmd.Wait(); err != nil {
		check(false, fmt.Sprintf("the %s run exited with %v", mode, err))
	}
	if !found {
		fatal(mode, errors.New("child printed no RESULT line"))
	}
	return result
}

// writeStream writes one stream's row and checks its claims.
func writeStream(r streamResult) {
	stats := r.Stats
	tables.streams.Row(r.Mode, r.Capacity, *operations, r.Reads, r.Writes, stats.DistinctDatabases,
		stats.Hits, stats.Misses, r.EvictedAtRest+r.EvictedNotAtRest, r.EvictedAtRest, r.EvictedNotAtRest, stats.VerifiedReopens, stats.RefusedReopens, stats.UnverifiedOpens, stats.MaxOpen,
		r.FDSamples, r.PeakFDs, r.FDMismatches, r.FDsAfterClose, r.ReadMismatches, r.Exact, r.Masked, r.Wrong,
		r.WarmReads, r.ColdReads, r.Budget)
	for _, reason := range r.NotAtRest {
		tables.uncleanEvictions.Row(r.Mode, reason)
	}
	tables.infoStreams.Row(r.Mode, r.PeakRSS, r.WarmNs, r.ColdNs, r.ElapsedMs)
	mode, limit := r.Mode, r.Capacity
	check(r.FDMismatches == 0 && r.PeakFDs == 3*stats.MaxOpen && r.FDsAfterClose == 0, fmt.Sprintf("%s: at every sample the process held exactly 3 descriptors per open connection, at most %d; none after Close", mode, 3*stats.MaxOpen))
	check(stats.MaxOpen <= limit, fmt.Sprintf("%s: never more than %d connections open", mode, limit))
	check(r.EvictedNotAtRest == 0 && stats.RefusedReopens == 0 && stats.VerifiedReopens == stats.Misses-stats.UnverifiedOpens, fmt.Sprintf("%s: every eviction left the file at rest, and every reopen matched the sha256 recorded at its eviction", mode))
	check(r.ReadMismatches == 0 && r.Wrong == 0, fmt.Sprintf("%s: every read saw the model's step, and every final file equals the recipe state for its step", mode))
	if mode == "bounded" {
		check(r.PeakRSS <= r.Budget, fmt.Sprintf("bounded: peak process-memory growth stayed within K x (2 x cache budget + %d KiB) + %d MiB", uncountedPerConnection/1024, memoryConstant>>20))
	}
}

func partA(ctx context.Context, work, master string) {
	part = "A"
	soft, hard, err := procstat.FileLimit()
	if err != nil {
		fatal("limit", err)
	}
	tables.infoLimits.Row(soft, hard)
	// Verify RLIMIT_NOFILE up front: K above the process descriptor limit is a
	// configuration error (budget K <= (soft limit - reserve) / 3 in WAL mode),
	// and the unbounded contrast needs roughly 3 * dbs descriptors.
	if uint64(3**databases+64) > soft {
		fatal("descriptor limit", fmt.Errorf("soft limit %d is below the %d descriptors the unbounded run may need; lower -dbs", soft, 3**databases+64))
	}
	setup("databases", *databases)
	setup("K (bounded capacity)", *capacity)
	setup("operations", *operations)
	setup("seed", *seed)
	setup("hot databases", max(1, *databases/20))
	setup("% of operations to hot", *hotPercent)
	setup("% writes", *writePercent)
	setup("every database", "a copy of recipe step 0")
	setup("step 0 sha256", fileHash(master))
	setup("a write applies", fmt.Sprintf("the next recipe step of that database, up to step %d", *maxStep))
	setup("a read runs", readQuery)
	setup("connections", fmt.Sprintf("recipe settings with cache_size=%d", *cacheSize))
	setup("descriptor sample", fmt.Sprintf("every %d operations", *sampleEvery))
	setup("memory sample", fmt.Sprintf("every %d operations", *memoryEvery))
	bounded := runChild(ctx, "bounded", filepath.Join(work, "bounded"))
	unbounded := runChild(ctx, "unbounded", filepath.Join(work, "unbounded"))
	writeStream(bounded)
	writeStream(unbounded)
	check(unbounded.PeakFDs > 3**capacity, fmt.Sprintf("the unbounded run's peak descriptors exceed the bounded run's limit 3 x K = %d", 3**capacity))
	check(unbounded.PeakRSS > bounded.PeakRSS, "the unbounded run's peak process-memory growth exceeds the bounded run's (SHAPE)")
}

func fileHash(path string) string {
	status, err := connmgr.CheckAtRest(path)
	if err != nil || !status.AtRest {
		fatal("hash "+path, fmt.Errorf("%v %v", status, err))
	}
	return status.SHA256
}

// observe writes one observations row for a file at a moment of part B and
// returns its seq. Every argument after file may be nil.
func observe(moment, file string, atRest, bytes, sha256, step, exact, note any) int {
	observationSeq++
	tables.observations.Row(observationSeq, caseID, moment, file, atRest, bytes, sha256, step, exact, note)
	return observationSeq
}

// observeStatus records what CheckAtRest reported, one problems row per problem.
func observeStatus(moment, file string, status connmgr.Status) {
	if status.AtRest {
		observe(moment, file, true, status.Size, status.SHA256, nil, nil, nil)
		return
	}
	seq := observe(moment, file, false, nil, nil, nil, nil, nil)
	for _, problem := range status.Problems {
		what, why, _ := strings.Cut(problem, ": ")
		problemSeq++
		tables.problems.Row(problemSeq, seq, what, why)
	}
}

func expectStep(ctx context.Context, path string, want int, claim string) {
	match, err := matchStep(ctx, path)
	if err != nil {
		fatal("match "+path, err)
	}
	var step any
	if match.Found() {
		step = match.Step
	}
	observe("recipe step held by the file", filepath.Base(path), true, nil, nil, step, match.Exact, nil)
	check(match.Step == want, claim)
}

type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines *bufio.Scanner
}

func startChild(ctx context.Context, role, path string) *child {
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.CommandContext(ctx, self, "-role", role, "-dir", path, "-max-step", fmt.Sprint(*maxStep), "-cache-size", fmt.Sprint(*cacheSize))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("stdin", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("stdout", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start child", err)
	}
	c := &child{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	if !c.lines.Scan() || c.lines.Text() != "ready" {
		fatal("child "+role, fmt.Errorf("did not report ready: %q %v", c.lines.Text(), c.lines.Err()))
	}
	return c
}

func childMain(ctx context.Context, role, path string) {
	runtime.LockOSThread()
	db, err := openManaged(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	switch role {
	case "reader":
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
			fatal("child read", err)
		}
	case "writer":
		if _, err := theRecipe().ApplyNext(ctx, db); err != nil {
			fatal("child write", err)
		}
	default:
		fatal("role", fmt.Errorf("unknown role %q", role))
	}
	fmt.Println("ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fatal("child stdin", err)
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func newManager(dir string) *connmgr.Manager {
	m, err := connmgr.New(connmgr.Options{Capacity: 4, PathFor: pathFor(dir), Open: openManaged})
	if err != nil {
		fatal("manager", err)
	}
	return m
}

func writeNext(ctx context.Context, m *connmgr.Manager, id string) *connmgr.Handle {
	h, err := m.Acquire(ctx, id)
	if err != nil {
		fatal("acquire "+id, err)
	}
	if _, err := theRecipe().ApplyNext(ctx, h.DB); err != nil {
		fatal("write "+id, err)
	}
	return h
}

func evict(m *connmgr.Manager, id string) connmgr.Status {
	e, err := m.Evict(id)
	if err != nil {
		fatal("evict "+id, err)
	}
	if e.CloseErr != nil || e.StatusErr != nil {
		fatal("evict "+id, errors.Join(e.CloseErr, e.StatusErr))
	}
	observeStatus("eviction by the manager", id+".db", e.Status)
	return e.Status
}

func atRest(path string) connmgr.Status {
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		fatal("check "+path, err)
	}
	return status
}

func partB(ctx context.Context, work, master string) {
	part = "B"
	dir := filepath.Join(work, "B")
	populate(master, dir, 6)
	m := newManager(dir)
	path := func(i int) string { return filepath.Join(dir, dbID(i)+".db") }

	// B1: sqlite3WalClose (src/wal.c:2507-2571) cannot take an EXCLUSIVE lock
	// while another process holds the file open, so it skips the checkpoint
	// and leaves -wal and -shm on disk.
	caseID = "B1"
	writeNext(ctx, m, dbID(0)).Release()
	reader := startChild(ctx, "reader", path(0))
	status := evict(m, dbID(0))
	check(!status.AtRest && len(status.Problems) == 2, "B1: the manager's close could not checkpoint or delete the WAL (another connection holds a lock), so the file is reported not at rest")
	if err := reader.stdin.Close(); err != nil {
		fatal("close child stdin", err)
	}
	if err := reader.cmd.Wait(); err != nil {
		fatal("reader exit", err)
	}
	observeStatus("after the other process closed", dbID(0)+".db", atRest(path(0)))
	expectStep(ctx, path(0), 1, "B1: the last connection to close (the other process) checkpointed and deleted the WAL: at rest, holding step 1")

	// B2: (*sql.DB).Close closes only idle pool connections and returns nil
	// while *sql.Rows holds a connection checked out; connmgr detects
	// OpenConnections > 0 and skips CheckAtRest so it does not drop the live
	// connection's POSIX locks.
	caseID = "B2"
	h := writeNext(ctx, m, dbID(1))
	rows, err := h.DB.QueryContext(ctx, "SELECT id FROM accounts ORDER BY id")
	if err != nil {
		fatal("query", err)
	}
	if !rows.Next() {
		fatal("rows", rows.Err())
	}
	h.Release()
	status = evict(m, dbID(1))
	check(!status.AtRest, "B2: database/sql cannot close a connection that a statement still uses, so DB.Close returned and the file is not at rest")
	if err := rows.Close(); err != nil {
		fatal("rows close", err)
	}
	observeStatus("after rows.Close", dbID(1)+".db", atRest(path(1)))
	expectStep(ctx, path(1), 1, "B2: closing the rows let the connection close: at rest, holding step 1")

	caseID = "B3"
	writer := startChild(ctx, "writer", path(2))
	if err := writer.cmd.Process.Kill(); err != nil {
		fatal("kill", err)
	}
	if err := writer.cmd.Wait(); err == nil {
		fatal("writer", errors.New("killed child exited cleanly"))
	}
	status = atRest(path(2))
	observeStatus("after the kill", dbID(2)+".db", status)
	check(!status.AtRest && len(status.Problems) == 2, "B3: a killed writer leaves a -wal with its committed frames and a -shm")
	naive := filepath.Join(dir, "naive-copy.db")
	if err := copyFile(path(2), naive); err != nil {
		fatal("copy", err)
	}
	expectStep(ctx, naive, 0, "B3: a copy of the database file alone is a valid database WITHOUT the committed step 1 (it is still in the -wal)")
	opened, err := openManaged(ctx, path(2))
	if err != nil {
		fatal("reopen", err)
	}
	if err := opened.Close(); err != nil {
		fatal("close", err)
	}
	observeStatus("after one clean open and close", dbID(2)+".db", atRest(path(2)))
	expectStep(ctx, path(2), 1, "B3: one clean open and close recovers the WAL, checkpoints it and deletes it: at rest, holding step 1")

	// B4: SQLITE_FCNTL_PERSIST_WAL causes sqlite3WalClose (src/wal.c:2533-2551)
	// to checkpoint into .db but leave -wal and -shm (walIndexClose with
	// isDelete=0, src/wal.c:1619-1630).
	caseID = "B4"
	h = writeNext(ctx, m, dbID(3))
	conn, err := h.DB.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	if _, err := sqlitex.PersistWAL(ctx, conn, "main", 1); err != nil {
		fatal("persist wal", err)
	}
	if err := conn.Close(); err != nil {
		fatal("conn close", err)
	}
	h.Release()
	status = evict(m, dbID(3))
	problems := strings.Join(status.Problems, ";")
	check(!status.AtRest && strings.Contains(problems, "-wal holds") && strings.Contains(problems, "-shm exists"), "B4: with PERSIST_WAL the close checkpoints but keeps the -wal (with its bytes) and the -shm, so the manager refuses to call it at rest")
	persisted := filepath.Join(dir, "persist-wal-copy.db")
	if err := copyFile(path(3), persisted); err != nil {
		fatal("copy", err)
	}
	expectStep(ctx, persisted, 1, "B4: the refusal is conservative here: the close did checkpoint, so the database file alone already holds step 1; a rule based on which files exist cannot see that")

	// B5: sqlite3PagerClose (src/pager.c:4222-4279) checks databaseIsUnmoved
	// (src/pager.c:4188-4205 -> fileHasMoved, src/os_unix.c:1623-1632) and
	// omits the checkpoint buffer when the file's inode no longer matches its
	// path, leaving step 1 in the old -wal while moved.db looks at rest.
	caseID = "B5"
	writeNext(ctx, m, dbID(4)).Release()
	moved := filepath.Join(dir, "moved.db")
	if err := os.Rename(path(4), moved); err != nil {
		fatal("rename", err)
	}
	status = evict(m, dbID(4))
	var walBytes int64
	if info, err := os.Stat(path(4) + "-wal"); err == nil {
		walBytes = info.Size()
		observe("left at the old name", dbID(4)+".db-wal", nil, walBytes, nil, nil, nil, nil)
	} else {
		observe("left at the old name", dbID(4)+".db-wal", nil, nil, nil, nil, nil, "no -wal")
	}
	check(!status.AtRest && len(status.Problems) > 0 && status.Problems[0] == "database file missing", "B5: the manager reports the database file missing at its path")
	check(walBytes > 0, "B5: a -wal with content stays behind at the old name")
	expectStep(ctx, moved, 0, "B5: the moved file looks at rest but holds step 0: close saw the file had moved, skipped the checkpoint, and step 1 stayed in the -wal under the old name")

	caseID = "B6"
	writeNext(ctx, m, dbID(5)).Release()
	recorded := evict(m, dbID(5))
	outside, err := openManaged(ctx, path(5))
	if err != nil {
		fatal("outside open", err)
	}
	if _, err := theRecipe().ApplyNext(ctx, outside); err != nil {
		fatal("outside write", err)
	}
	if err := outside.Close(); err != nil {
		fatal("outside close", err)
	}
	_, err = m.Acquire(ctx, dbID(5))
	var changed *connmgr.ChangedWhileClosedError
	if errors.As(err, &changed) {
		observe("reopen refused: changed while closed", dbID(5)+".db", changed.Found.AtRest, changed.Found.Size, changed.Found.SHA256, nil, nil, err.Error())
	} else {
		observe("reopen", dbID(5)+".db", nil, nil, nil, nil, nil, fmt.Sprint(err))
	}
	check(recorded.AtRest && errors.As(err, &changed) && changed.Found.AtRest && changed.Found.SHA256 != recorded.SHA256, "B6: a write made outside the manager while the file was closed changes its sha256, and the manager refuses to reopen it")
	if err := m.Close(); err != nil {
		fatal("close manager", err)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *childRoleFlag != "" {
		childMain(ctx, *childRoleFlag, *runDirFlag)
		return
	}
	if *modeFlag != "" {
		runStream(ctx, *modeFlag, *runDirFlag)
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *databases < 20 || *capacity < 1 || *capacity >= *databases || *operations < 1 || *maxStep < 1 || *maxStep > recipe.DefaultSteps || *sampleEvery < 1 || *memoryEvery < 1 || *writePercent < 0 || *writePercent > 100 || *hotPercent < 0 || *hotPercent > 100 || *cacheSize == 0 {
		fatal("flags", fmt.Errorf("need -dbs >= 20, 1 <= -k < -dbs, -ops >= 1, 1 <= -max-step <= %d, positive sample intervals, percentages 0..100, -cache-size != 0", recipe.DefaultSteps))
	}
	work := sqlitex.MustWorkDir("8.3")
	set = results.MustOpen(work)
	openTables()
	master := filepath.Join(work, "master.db")
	if err := theRecipe().Build(ctx, master, 0); err != nil {
		fatal("build master", err)
	}
	populate(master, filepath.Join(work, "bounded"), *databases)
	populate(master, filepath.Join(work, "unbounded"), *databases)
	partA(ctx, work, master)
	partB(ctx, work, master)
	set.Finish("BOUNDED MANAGER")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/8.3-bounded-manager/report.sql -->
```sql
-- Walkthrough of experiment 8.3. Run it with:
--   go run ./cmd/course run 8.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   setup               one setting of the experiment (name, value)
--   streams             part A: one run of the operation stream (bounded or unbounded), counted by its child process
--   unclean_evictions   part A: one eviction that did not leave its file at rest (empty when every check holds)
--   observations        part B: one file at one moment of a case: its at-rest status, size, sha256, or recipe step
--   problems            part B: one problem the at-rest check reported for an observation, and why it matters
--   checks              every claim the experiment asserted (the runner lists it last)
-- info.sql presents info_streams (peak memory, read latency, run time) and
-- info_limits (RLIMIT_NOFILE): they vary between runs and machines.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  8.3  Can a bounded connection manager serve many databases at rest?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Part A serves a seeded stream of reads and writes over many WAL'
.print 'databases twice: through an LRU manager that keeps at most K connections'
.print 'open, and through one that never evicts. Each run happens in its own'
.print 'child process, on its own copies of the files, so descriptor and memory'
.print 'baselines are clean. Part B shows six cases where a closed WAL database'
.print 'is not one file at rest, or changes while closed. The settings:'

SELECT name, value FROM setup WHERE name <> 'a read runs' ORDER BY seq;

.mode list
.headers off
SELECT 'A read runs: ' || value FROM setup WHERE name = 'a read runs' ORDER BY seq;
.headers on
.mode box

.print
.print '── A. A bounded manager against an unbounded one ───────────────────────'
.print
.print 'A1. The same stream in both runs. The bounded manager opens a database'
.print '    again each time it misses; the unbounded one opens each database once.'

SELECT mode, capacity, operations, reads, writes, distinct_dbs, hits, misses, peak_open
FROM streams
ORDER BY capacity;

.print
.print 'A2. Every eviction closed the connection and found the file at rest; the'
.print '    manager recorded its sha256 and checked it at the next open. Every'
.print '    open is either a first open or a verified reopen, and none was refused.'
.print '    (unclean_evictions lists any eviction that was not at rest.)'

SELECT mode, evictions, evicted_at_rest AS at_rest, evicted_not_at_rest AS not_at_rest,
       misses AS opens, first_opens, verified_reopens AS verified, refused_reopens AS refused,
       misses = first_opens + verified_reopens AS opens_add_up
FROM streams
ORDER BY capacity;

.print
.print 'A3. Descriptors, sampled between operations: always exactly 3 per open'
.print '    connection (.db, -wal, -shm), so the peak is 3 x peak_open, and 0'
.print '    after Close. The bounded run never holds more than 3 x K.'

SELECT mode, fd_samples, peak_fds, 3 * peak_open AS three_x_peak_open, fd_mismatches, fds_after_close
FROM streams
ORDER BY capacity;

.print
.print 'A4. Correctness. Every read saw the step the model expects, and every'
.print '    file, once closed, equals the recipe state of its step: exactly, or'
.print '    except the change counter (several steps applied on one connection).'

SELECT mode, read_mismatches, files_exact, files_masked AS equal_except_counter, files_wrong
FROM streams
ORDER BY capacity;

.print
.print 'A5. The cost the bound saves and the price it pays. Peak descriptors'
.print '    against the bounded limit 3 x K; reads served by an open connection'
.print '    (warm) or by an open, verify and read (cold). The memory budget is'
.print '    capacity x (2 x cache budget + 256 KiB) + 32 MiB; the measured peak'
.print '    memory and the read latencies vary between machines: see info.sql.'

SELECT mode,
       peak_fds,
       3 * (SELECT value FROM setup WHERE name = 'K (bounded capacity)')::INTEGER AS three_x_k,
       warm_reads,
       cold_reads,
       round(memory_budget / 1048576, 1) AS memory_budget_mib
FROM streams
ORDER BY capacity;

.print
.print '── B. When a closed WAL database is NOT a single file at rest ───────────'
.print
.print 'Six databases, each a copy of step 0, served by a manager with K = 4.'
.print 'Each case writes step 1 and then closes the connection in a way that'
.print 'goes wrong. A row is the at-rest check of a file at a moment, with the'
.print 'problems it found indented under it, or the recipe step the file holds.'

CREATE TEMP VIEW b_rows AS
SELECT o.case_id, o.seq, 0 AS sub, o.moment, o.file,
       CASE WHEN o.recipe_step IS NOT NULL
                 THEN 'step ' || o.recipe_step || CASE WHEN o.exact THEN ' (exact bytes)'
                                                      ELSE ' (equal except the change counter)' END
            WHEN o.at_rest AND o.sha256 IS NOT NULL
                 THEN 'at rest, ' || o.bytes || ' bytes, sha256 ' || left(o.sha256, 12) || '...'
            WHEN o.at_rest IS NULL AND o.bytes IS NOT NULL
                 THEN o.bytes || ' bytes'
            WHEN NOT o.at_rest THEN 'not at rest:'
            ELSE coalesce(o.note, '')
       END AS result
FROM observations o
UNION ALL
SELECT o.case_id, o.seq, p.seq, '', '', '  ' || p.problem
FROM problems p JOIN observations o ON o.seq = p.observation;

.print
.print 'B1. Another process still has the database open when the manager closes'
.print '    its connection. The close cannot take the lock it needs to checkpoint,'
.print '    so -wal and -shm stay. The last connection to close (the other'
.print '    process) checkpoints and deletes them.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B1' ORDER BY seq, sub;

.print
.print 'B2. A statement is left open (rows not closed) when the handle is'
.print '    released. database/sql cannot close a connection a statement still'
.print '    uses, so DB.Close returns and the connection stays open until rows.Close.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B2' ORDER BY seq, sub;

.print
.print 'B3. The process that wrote is killed before it closes. Its committed'
.print '    step 1 is only in the -wal: a copy of the database file alone is a'
.print '    valid database that holds step 0. One clean open and close recovers'
.print '    the WAL, checkpoints it and deletes it.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B3' ORDER BY seq, sub;

.print
.print 'B4. SQLITE_FCNTL_PERSIST_WAL is set on the connection. The close'
.print '    checkpoints but keeps -wal (with its bytes) and -shm, so the manager'
.print '    refuses to call the file at rest. The refusal is conservative here: a'
.print '    copy of the database file alone already holds step 1, but a rule based'
.print '    on which files exist cannot see that.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B4' ORDER BY seq, sub;

.print
.print 'B5. The file is renamed while its connection is open. Close sees that'
.print '    the file has moved and skips the checkpoint: step 1 stays in a -wal'
.print '    under the old name, and moved.db looks at rest but holds step 0.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B5' ORDER BY seq, sub;

.print
.print 'B6. The file changes while it is closed: another program applies the'
.print '    next step. Its sha256 no longer matches the one recorded at eviction,'
.print '    so the manager refuses to reopen it.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B6' ORDER BY seq, sub;

.print
.print 'The error the reopen returned:'
.mode list
.headers off
SELECT '  ' || regexp_replace(note, '([0-9a-f]{12})[0-9a-f]{52}', '\1...', 'g') FROM observations WHERE case_id = 'B6' AND note IS NOT NULL ORDER BY seq;
.headers on
.mode box

.print
.print 'B7. What each problem means:'
.mode list
.headers off
SELECT '  ' || problem || CASE WHEN why IS NULL THEN '' ELSE ': ' || why END
FROM (SELECT problem, why, min(seq) AS first FROM problems GROUP BY problem, why)
ORDER BY first;
.headers on
.mode box

.print
.print 'B8. The sha256 values above, in full. Every file that holds step 1 at'
.print '    rest has the same bytes; the second value is db-0005.db after the'
.print '    write made outside the manager.'

SELECT sha256, string_agg(DISTINCT case_id, ' ' ORDER BY case_id) AS cases, count(*) AS observations
FROM observations
WHERE sha256 IS NOT NULL
GROUP BY sha256
ORDER BY min(seq);
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 8.3
```

Add `-i` (`go run ./cmd/course run -i 8.3`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

`course run` also prints the run-dependent values, which are not part of the stored evidence: for each stream run, the peak process-memory growth, the mean warm and cold read latency and the run time, and `RLIMIT_NOFILE` as the Go process sees it.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/8.3-bounded-manager/`](evidence/8.3-bounded-manager/), and `go run ./cmd/course check evidence ch08` compares a fresh run with them byte for byte. The run uses the defaults `-dbs 1000 -k 64 -ops 4000`.

<!-- include: evidence/8.3-bounded-manager.txt -->
```text

════════════════════════════════════════════════════════════════════════
  8.3  Can a bounded connection manager serve many databases at rest?
════════════════════════════════════════════════════════════════════════

Part A serves a seeded stream of reads and writes over many WAL
databases twice: through an LRU manager that keeps at most K connections
open, and through one that never evicts. Each run happens in its own
child process, on its own copies of the files, so descriptor and memory
baselines are clean. Part B shows six cases where a closed WAL database
is not one file at rest, or changes while closed. The settings:
┌────────────────────────┬──────────────────────────────────────────────────────────────────┐
│          name          │                              value                               │
├────────────────────────┼──────────────────────────────────────────────────────────────────┤
│ databases              │ 1000                                                             │
│ K (bounded capacity)   │ 64                                                               │
│ operations             │ 4000                                                             │
│ seed                   │ 1                                                                │
│ hot databases          │ 50                                                               │
│ % of operations to hot │ 50                                                               │
│ % writes               │ 20                                                               │
│ every database         │ a copy of recipe step 0                                          │
│ step 0 sha256          │ 7470585af706eb3de499f126d74bdc98396e1e9853ee11643e2e85f61ef0cdb9 │
│ a write applies        │ the next recipe step of that database, up to step 4              │
│ connections            │ recipe settings with cache_size=-256                             │
│ descriptor sample      │ every 10 operations                                              │
│ memory sample          │ every 100 operations                                             │
└────────────────────────┴──────────────────────────────────────────────────────────────────┘
A read runs: SELECT count(*), coalesce(sum(balance), 0), (SELECT user_version FROM pragma_user_version) FROM accounts

── A. A bounded manager against an unbounded one ───────────────────────

A1. The same stream in both runs. The bounded manager opens a database
    again each time it misses; the unbounded one opens each database once.
┌───────────┬──────────┬────────────┬───────┬────────┬──────────────┬──────┬────────┬───────────┐
│   mode    │ capacity │ operations │ reads │ writes │ distinct_dbs │ hits │ misses │ peak_open │
├───────────┼──────────┼────────────┼───────┼────────┼──────────────┼──────┼────────┼───────────┤
│ bounded   │ 64       │ 4000       │ 3415  │ 585    │ 866          │ 1198 │ 2802   │ 64        │
│ unbounded │ 1000     │ 4000       │ 3415  │ 585    │ 866          │ 3134 │ 866    │ 866       │
└───────────┴──────────┴────────────┴───────┴────────┴──────────────┴──────┴────────┴───────────┘

A2. Every eviction closed the connection and found the file at rest; the
    manager recorded its sha256 and checked it at the next open. Every
    open is either a first open or a verified reopen, and none was refused.
    (unclean_evictions lists any eviction that was not at rest.)
┌───────────┬───────────┬─────────┬─────────────┬───────┬─────────────┬──────────┬─────────┬──────────────┐
│   mode    │ evictions │ at_rest │ not_at_rest │ opens │ first_opens │ verified │ refused │ opens_add_up │
├───────────┼───────────┼─────────┼─────────────┼───────┼─────────────┼──────────┼─────────┼──────────────┤
│ bounded   │ 2802      │ 2802    │ 0           │ 2802  │ 866         │ 1936     │ 0       │ true         │
│ unbounded │ 866       │ 866     │ 0           │ 866   │ 866         │ 0        │ 0       │ true         │
└───────────┴───────────┴─────────┴─────────────┴───────┴─────────────┴──────────┴─────────┴──────────────┘

A3. Descriptors, sampled between operations: always exactly 3 per open
    connection (.db, -wal, -shm), so the peak is 3 x peak_open, and 0
    after Close. The bounded run never holds more than 3 x K.
┌───────────┬────────────┬──────────┬───────────────────┬───────────────┬─────────────────┐
│   mode    │ fd_samples │ peak_fds │ three_x_peak_open │ fd_mismatches │ fds_after_close │
├───────────┼────────────┼──────────┼───────────────────┼───────────────┼─────────────────┤
│ bounded   │ 401        │ 192      │ 192               │ 0             │ 0               │
│ unbounded │ 401        │ 2598     │ 2598              │ 0             │ 0               │
└───────────┴────────────┴──────────┴───────────────────┴───────────────┴─────────────────┘

A4. Correctness. Every read saw the step the model expects, and every
    file, once closed, equals the recipe state of its step: exactly, or
    except the change counter (several steps applied on one connection).
┌───────────┬─────────────────┬─────────────┬──────────────────────┬─────────────┐
│   mode    │ read_mismatches │ files_exact │ equal_except_counter │ files_wrong │
├───────────┼─────────────────┼─────────────┼──────────────────────┼─────────────┤
│ bounded   │ 0               │ 970         │ 30                   │ 0           │
│ unbounded │ 0               │ 880         │ 120                  │ 0           │
└───────────┴─────────────────┴─────────────┴──────────────────────┴─────────────┘

A5. The cost the bound saves and the price it pays. Peak descriptors
    against the bounded limit 3 x K; reads served by an open connection
    (warm) or by an open, verify and read (cold). The memory budget is
    capacity x (2 x cache budget + 256 KiB) + 32 MiB; the measured peak
    memory and the read latencies vary between machines: see info.sql.
┌───────────┬──────────┬───────────┬────────────┬────────────┬───────────────────┐
│   mode    │ peak_fds │ three_x_k │ warm_reads │ cold_reads │ memory_budget_mib │
├───────────┼──────────┼───────────┼────────────┼────────────┼───────────────────┤
│ bounded   │ 192      │ 192       │ 1077       │ 2338       │ 80.0              │
│ unbounded │ 2598     │ 192       │ 2725       │ 690        │ 782.0             │
└───────────┴──────────┴───────────┴────────────┴────────────┴───────────────────┘

── B. When a closed WAL database is NOT a single file at rest ───────────

Six databases, each a copy of step 0, served by a manager with K = 4.
Each case writes step 1 and then closes the connection in a way that
goes wrong. A row is the at-rest check of a file at a moment, with the
problems it found indented under it, or the recipe step the file holds.

B1. Another process still has the database open when the manager closes
    its connection. The close cannot take the lock it needs to checkpoint,
    so -wal and -shm stay. The last connection to close (the other
    process) checkpoints and deletes them.
┌────────────────────────────────┬────────────┬──────────────────────────────────────────────┐
│             moment             │    file    │                    result                    │
├────────────────────────────────┼────────────┼──────────────────────────────────────────────┤
│ eviction by the manager        │ db-0000.db │ not at rest:                                 │
│                                │            │   -wal holds 90672 bytes                     │
│                                │            │   -shm exists (32768 bytes)                  │
│ after the other process closed │ db-0000.db │ at rest, 90112 bytes, sha256 6d8cfc77d5f5... │
│ recipe step held by the file   │ db-0000.db │ step 1 (exact bytes)                         │
└────────────────────────────────┴────────────┴──────────────────────────────────────────────┘

B2. A statement is left open (rows not closed) when the handle is
    released. database/sql cannot close a connection a statement still
    uses, so DB.Close returns and the connection stays open until rows.Close.
┌──────────────────────────────┬────────────┬──────────────────────────────────────────────┐
│            moment            │    file    │                    result                    │
├──────────────────────────────┼────────────┼──────────────────────────────────────────────┤
│ eviction by the manager      │ db-0001.db │ not at rest:                                 │
│                              │            │   1 connection(s) still open in this process │
│ after rows.Close             │ db-0001.db │ at rest, 90112 bytes, sha256 6d8cfc77d5f5... │
│ recipe step held by the file │ db-0001.db │ step 1 (exact bytes)                         │
└──────────────────────────────┴────────────┴──────────────────────────────────────────────┘

B3. The process that wrote is killed before it closes. Its committed
    step 1 is only in the -wal: a copy of the database file alone is a
    valid database that holds step 0. One clean open and close recovers
    the WAL, checkpoints it and deletes it.
┌────────────────────────────────┬───────────────┬──────────────────────────────────────────────┐
│             moment             │     file      │                    result                    │
├────────────────────────────────┼───────────────┼──────────────────────────────────────────────┤
│ after the kill                 │ db-0002.db    │ not at rest:                                 │
│                                │               │   -wal holds 90672 bytes                     │
│                                │               │   -shm exists (32768 bytes)                  │
│ recipe step held by the file   │ naive-copy.db │ step 0 (exact bytes)                         │
│ after one clean open and close │ db-0002.db    │ at rest, 90112 bytes, sha256 6d8cfc77d5f5... │
│ recipe step held by the file   │ db-0002.db    │ step 1 (exact bytes)                         │
└────────────────────────────────┴───────────────┴──────────────────────────────────────────────┘

B4. SQLITE_FCNTL_PERSIST_WAL is set on the connection. The close
    checkpoints but keeps -wal (with its bytes) and -shm, so the manager
    refuses to call the file at rest. The refusal is conservative here: a
    copy of the database file alone already holds step 1, but a rule based
    on which files exist cannot see that.
┌──────────────────────────────┬─────────────────────┬─────────────────────────────┐
│            moment            │        file         │           result            │
├──────────────────────────────┼─────────────────────┼─────────────────────────────┤
│ eviction by the manager      │ db-0003.db          │ not at rest:                │
│                              │                     │   -wal holds 90672 bytes    │
│                              │                     │   -shm exists (32768 bytes) │
│ recipe step held by the file │ persist-wal-copy.db │ step 1 (exact bytes)        │
└──────────────────────────────┴─────────────────────┴─────────────────────────────┘

B5. The file is renamed while its connection is open. Close sees that
    the file has moved and skips the checkpoint: step 1 stays in a -wal
    under the old name, and moved.db looks at rest but holds step 0.
┌──────────────────────────────┬────────────────┬─────────────────────────┐
│            moment            │      file      │         result          │
├──────────────────────────────┼────────────────┼─────────────────────────┤
│ eviction by the manager      │ db-0004.db     │ not at rest:            │
│                              │                │   database file missing │
│ left at the old name         │ db-0004.db-wal │ 90672 bytes             │
│ recipe step held by the file │ moved.db       │ step 0 (exact bytes)    │
└──────────────────────────────┴────────────────┴─────────────────────────┘

B6. The file changes while it is closed: another program applies the
    next step. Its sha256 no longer matches the one recorded at eviction,
    so the manager refuses to reopen it.
┌──────────────────────────────────────┬────────────┬───────────────────────────────────────────────┐
│                moment                │    file    │                    result                     │
├──────────────────────────────────────┼────────────┼───────────────────────────────────────────────┤
│ eviction by the manager              │ db-0005.db │ at rest, 90112 bytes, sha256 6d8cfc77d5f5...  │
│ reopen refused: changed while closed │ db-0005.db │ at rest, 118784 bytes, sha256 69490a2dc9c1... │
└──────────────────────────────────────┴────────────┴───────────────────────────────────────────────┘

The error the reopen returned:
  connmgr: db-0005 changed while closed: recorded sha256 6d8cfc77d5f5..., file now has sha256 69490a2dc9c1...

B7. What each problem means:
  -wal holds 90672 bytes: the database file alone may lack committed transactions
  -shm exists (32768 bytes): a connection is open or did not close cleanly
  1 connection(s) still open in this process: a statement, transaction or *sql.Conn was not closed, so database/sql closes the connection only when it is returned
  database file missing

B8. The sha256 values above, in full. Every file that holds step 1 at
    rest has the same bytes; the second value is db-0005.db after the
    write made outside the manager.
┌──────────────────────────────────────────────────────────────────┬─────────────┬──────────────┐
│                              sha256                              │    cases    │ observations │
├──────────────────────────────────────────────────────────────────┼─────────────┼──────────────┤
│ 6d8cfc77d5f514b26155ee7c31633198a55633b68b761ba297e4031b7e318924 │ B1 B2 B3 B6 │ 4            │
│ 69490a2dc9c1980f70afed4f5830799677937b011bde64c3110f823273389a06 │ B6          │ 1            │
└──────────────────────────────────────────────────────────────────┴─────────────┴──────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  bounded: at every sample the process held exactly 3 descriptors per open connection, at most 192; none after Close
A  ✓  bounded: never more than 64 connections open
A  ✓  bounded: every eviction left the file at rest, and every reopen matched the sha256 recorded at its eviction
A  ✓  bounded: every read saw the model's step, and every final file equals the recipe state for its step
A  ✓  bounded: peak process-memory growth stayed within K x (2 x cache budget + 256 KiB) + 32 MiB
A  ✓  unbounded: at every sample the process held exactly 3 descriptors per open connection, at most 2598; none after Close
A  ✓  unbounded: never more than 1000 connections open
A  ✓  unbounded: every eviction left the file at rest, and every reopen matched the sha256 recorded at its eviction
A  ✓  unbounded: every read saw the model's step, and every final file equals the recipe state for its step
A  ✓  the unbounded run's peak descriptors exceed the bounded run's limit 3 x K = 192
A  ✓  the unbounded run's peak process-memory growth exceeds the bounded run's (SHAPE)
B  ✓  B1: the manager's close could not checkpoint or delete the WAL (another connection holds a lock), so the file is reported not at rest
B  ✓  B1: the last connection to close (the other process) checkpointed and deleted the WAL: at rest, holding step 1
B  ✓  B2: database/sql cannot close a connection that a statement still uses, so DB.Close returned and the file is not at rest
B  ✓  B2: closing the rows let the connection close: at rest, holding step 1
B  ✓  B3: a killed writer leaves a -wal with its committed frames and a -shm
B  ✓  B3: a copy of the database file alone is a valid database WITHOUT the committed step 1 (it is still in the -wal)
B  ✓  B3: one clean open and close recovers the WAL, checkpoints it and deletes it: at rest, holding step 1
B  ✓  B4: with PERSIST_WAL the close checkpoints but keeps the -wal (with its bytes) and the -shm, so the manager refuses to call it at rest
B  ✓  B4: the refusal is conservative here: the close did checkpoint, so the database file alone already holds step 1; a rule based on which files exist cannot see that
B  ✓  B5: the manager reports the database file missing at its path
B  ✓  B5: a -wal with content stays behind at the old name
B  ✓  B5: the moved file looks at rest but holds step 0: close saw the file had moved, skipped the checkpoint, and step 1 stayed in the -wal under the old name
B  ✓  B6: a write made outside the manager while the file was closed changes its sha256, and the manager refuses to reopen it

24 of 24 claims hold

Experiment verdict: BOUNDED MANAGER OK
```

#### Reading the Experiment 8.3 evidence

- **Settings table (setup):** One recipe step-0 database (`master.db`, SHA-256 `7470585a…cdb9`) is copied 1,000 times into `bounded/` and 1,000 times into `unbounded/` (the R5 rebuild). The seeded stream (`seed 1`, `4,000` operations: `3,415` reads, `585` writes, `50%` targeting the hot `50` databases, `cache_size=-256`) runs inside two separate child processes so descriptor and RSS baselines start clean.
- **A1–A5 (Part A — bounded `K = 64` versus unbounded `K = 1000`):**

| Metric | Bounded (`K = 64`) | Unbounded (`K = 1000`) |
| --- | --- | --- |
| Distinct databases touched | `866` | `866` |
| Hits / Misses (opens) | `1,198` / `2,802` (`866` first + `1,936` verified reopens) | `3,134` / `866` (`866` first + `0` reopens) |
| Evictions at rest / not at rest | `2,802` / `0` | `866` / `0` (all at shutdown) |
| Refused reopens | `0` | `0` |
| Peak open connections | `64` | `866` |
| Peak descriptors above baseline | `192` (`401` samples, `0` != `3×open`, `0` after `Close`) | `2,598` (`401` samples, `0` != `3×open`, `0` after `Close`) |
| Peak RSS growth (run-dependent, printed by `course run`; earlier capture) | `22.7 MiB` (budget `80.0 MiB`; about `20 MiB` in the lab run) | `211.4 MiB` (about `211 MiB` in the lab run) |
| Mean read latency warm / cold (run-dependent, SHAPE; earlier capture) | `112µs` / `3.565ms` (`17.251s` total; about `18µs` / `455µs` in the lab run) | `144µs` / `1.757ms` (`13.082s` total) |
| Final files matching recipe model | `970` exact + `30` change-counter-only, `0` wrong | `880` exact + `120` change-counter-only, `0` wrong |

  - Every one of the `401` descriptor samples in both runs held **exact** `3 × open` descriptors—stronger than an inequality with a slack constant (`192` peak for `K = 64` vs `2,598` peak for unbounded).
  - Bounded peak memory growth was `22.7 MiB` in the earlier capture (about `20 MiB` in the lab run) against the budget `64 × (2 × 256 KiB cache + 256 KiB) + 32.0 MiB = 80.0 MiB`, nearly `10×` lower than the unbounded run's `211.4 MiB` (about `211 MiB` in the lab run).
  - Cold reads in the bounded run averaged `3.565ms` versus `112µs` warm in the earlier capture (`~32×` slower, SHAPE; about `455µs` versus `18µs`, `~25×`, in the lab run), because a miss at capacity pays for evicting an idle database (checkpoint + `fsync` at `synchronous=FULL` + SHA-256 hash) before hashing and opening the requested database.
  - All `1,000` databases in both runs ended at their exact expected recipe step (`0` wrong). The `30` (bounded) and `120` (unbounded) files marked `equal except the change counter` had two or more recipe steps applied on the same open WAL connection (which increments page 1's change counter once per connection that writes page 1, accepted only when header bytes 24–27 equal 92–95). Notice why `bounded` has fewer (`30` vs `120`): frequent evictions closed connections between writes.
- **B1–B8 (Part B — six cases where a closed WAL database is not a single file at rest, or changes while closed):**
  - Every step-1 commit produces a `90,672`-byte `-wal` (`32`-byte header + `22` frames × `4,120` bytes) and checkpoints into a `90,112`-byte (`22`-page) `.db` file with SHA-256 `6d8cfc77…18924`.
  - **B1 (another process holds the database open):** The manager's `Evict` cannot take `EXCLUSIVE` in `sqlite3WalClose`, leaving `-wal` (`90,672` bytes) and `-shm` (`32,768` bytes). The manager flags both problems and drops the record. Once the child reader closes (now the last connection), its close checkpoints and deletes the side files, leaving `db-0000.db` at rest with exact step-1 bytes.
  - **B2 (unclosed `*sql.Rows` when the handle is released):** `(*sql.DB).Close()` returns `nil` while `rows` still checks out the single connection. `connmgr` sees `db.Stats().OpenConnections == 1`, skips `CheckAtRest` (so it does not drop the live connection's POSIX locks), and reports `1 connection(s) still open in this process`. Closing `rows` returns the connection to the closed pool, triggering driver `Close` and leaving `db-0001.db` at rest at step 1.
  - **B3 (writer killed with `SIGKILL` before closing):** After `Process.Kill()`, `-wal` (`90,672` bytes) and `-shm` (`32,768` bytes) remain. A naive copy of `db-0002.db` alone (`naive-copy.db`) opens cleanly and equals **step 0 (exact bytes)**—silently losing the committed step 1 that still lives in `-wal`. One clean open and close recovers, checkpoints, and deletes `-wal`/`-shm`, producing exact step-1 bytes.
  - **B4 (`SQLITE_FCNTL_PERSIST_WAL` enabled):** `sqlite3WalClose` (`src/wal.c:2533-2551`) checkpoints step 1 into `db-0003.db` (`persist-wal-copy.db` already equals step 1), yet keeps `-wal` (`90,672` bytes, not truncated because `journal_size_limit < 0`) and `-shm` (`32,768` bytes). `CheckAtRest` conservatively refuses to declare it at rest because a file-presence check cannot distinguish a checkpointed persisted WAL from an uncheckpointed one.
  - **B5 (file renamed while its connection is open):** After `os.Rename(db-0004.db, moved.db)`, eviction reports `database file missing` at the old path, and `-wal` (`90,672` bytes) stays behind at the old name. Inside `sqlite3PagerClose` (`src/pager.c:4222-4279`), `databaseIsUnmoved` fails (`fileHasMoved`, `src/os_unix.c:1623-1632`), so SQLite skips the checkpoint. The hazard is severe: **`moved.db` passes every at-rest check yet holds step 0 (exact bytes)**—committed step 1 was left behind in the orphaned `-wal`.
  - **B6 (file modified while closed):** An outside connection applies step 2 while `db-0005` is evicted (`6d8cfc77…` → `69490a2d…`). The manager's `Acquire` detects the SHA-256 change before opening and refuses with `*ChangedWhileClosedError`.

---

## 6. Evidence and failure cases

### 6.1 When a closed WAL database is not at rest—and what `CheckAtRest` can and cannot see

Experiment 8.3 Part B and the pinned source establish **seven** ways a WAL database fails the simple assumption "closed = single fully checkpointed file":

| # | Condition | Source / Experiment | What lands on disk | Caught by `connmgr`? |
| --- | --- | --- | --- | --- |
| 1 | Another process still holds the database open | `src/wal.c:2526-2528`; Exp 8.3 B1, `ATR-01a/b` | `-wal`/`-shm` remain, or `EXCLUSIVE` lock on `.db` | Yes (side-file check or `F_GETLK` lock probe) |
| 2 | Unclosed `*sql.Rows` / `*sql.Tx` / `*sql.Conn` in Go | Go `database/sql`; Exp 8.3 B2, `ATR-02` | `(*sql.DB).Close()` returns `nil`; connection stays open | Yes (`OpenConnections > 0` skips `CheckAtRest`) |
| 3 | Writer process killed (`SIGKILL`) before close | Exp 8.3 B3, `MGR-01` | Committed frames stay in `-wal`; `.db` alone is stale | Yes (`-wal` and `-shm` present) |
| 4 | `SQLITE_FCNTL_PERSIST_WAL` enabled | `src/wal.c:2533-2551`; Exp 8.3 B4 | Checkpointed `.db`, but `-wal` and `-shm` stay | Yes (conservative refusal) |
| 5 | File renamed or unlinked while open | `src/pager.c:4188-4205,4232-4236`; `src/os_unix.c:1623-1632`; Exp 8.3 B5 | Old `-wal` keeps commits; moved `.db` looks at rest and is stale | Caught at old path (`missing`); **invisible at new path** |
| 6 | `SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE` set | `src/pager.c:4232` (source only; driver does not expose `sqlite3_db_config`) | Close skips checkpoint; `-wal` stays | Would be caught by non-empty `-wal` |
| 7 | Close-time checkpoint returns an error (e.g., I/O error) | `src/wal.c:2533-2537` (source only) | `rc != SQLITE_OK` skips delete; `-wal` and `-shm` stay | Would be caught by `-wal` and `-shm` |

**What `CheckAtRest` can and cannot detect.** Side-file checks (`-wal`, `-shm`, hot `-journal`) alone are **not** enough: an open WAL connection using `PRAGMA locking_mode=EXCLUSIVE` keeps its wal-index in heap memory and creates **no** `-shm` file (`ATR-01b`), and an active rollback-mode reader creates no sidecar file at all. Adding the `fcntl(F_GETLK)` probe on SQLite's lock range (`0x40000000..+512`) catches both because both hold POSIX locks on `.db`.

One class of external connection remains **invisible** to any file or lock check: a connection in another process that currently holds **no lock**—specifically an idle rollback-mode connection, or a connection that called `sqlite3_open_v2` without yet running a statement (`ATR-01a` case 3). And `F_GETLK` never reports locks held by the calling process itself, so `CheckAtRest` must never run while this process has another `*sql.DB` open to the same file (that would also trigger the POSIX lock-drop hazard). "At rest" therefore proves **"the `.db` file is complete right now and nobody holds a lock on it right now"**—not "no other process has an idle file descriptor." Ownership (ensuring only the platform's manager opens the file) is a platform invariant, not something SQLite's files can enforce alone.

### 6.2 Version notes

From the pinned-tree research inventory ([`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md) §8.8) and the copied C source:

- **SQLite 3.49.0:** added `SQLITE_DBCONFIG_ENABLE_ATTACH_CREATE` and `SQLITE_DBCONFIG_ENABLE_ATTACH_WRITE` to let an application prevent `ATTACH` from creating new files or writing to attached files.
- **SQLite 3.49.1:** hardened `SQLITE_DBCONFIG_LOOKASIDE`.
- **SQLite 3.53.0:** added `SQLITE_PREPARE_FROM_DDL` (used by virtual tables).
- **Compile-time options in this driver (`modernc.org/sqlite v1.60.1`):** `SQLITE_OMIT_DEPRECATED` is **not** set, so `PRAGMA default_cache_size` persists in header bytes 48–51 (`src/prepare.c:324`). `SQLITE_ENABLE_MEMORY_MANAGEMENT` **is** set, placing every connection's page cache in one shared `pcache1` group so `PRAGMA shrink_memory` shrinks all connections in the process at once (a C build without that option gives each connection its own group and shrinks only the calling connection, read from `src/pcache1.c:839-851`, not measured here).

### What attacked this chapter

<!-- adversarial-results -->

Four adversarial programs (`29` attacks total, driven by [`run-all.sh`](../agent_work/ch08/adversarial/run-all.sh)) attacked Chapter 8's claims and shared packages against SQLite 3.53.4:

- [`open-settings-edge`](../agent_work/ch08/adversarial/open-settings-edge/main.go) (`OPN-01`–`OPN-06`): missing files with `mode=rw`/`ro` (`SQLITE_CANTOPEN`, code `14`, no file created) vs `mode=rwc` (0-byte file at open, `8,192` bytes after DDL); non-database files (random 4 KiB, ASCII text, truncated 50-byte file — plain open succeeds, `SQLITE_NOTADB` code `26` arrives on the first statement, while `sqlitex.Open` fails immediately; 0-byte file queries cleanly); `strace` contrast in rollback `DELETE` mode (page 1 read once, no `-wal`/`-shm`) and on a killed-writer dirty WAL (`open` still reads only 100 bytes, first statement reads `-wal` 3 times during recovery); all five rollback journal modes reverting to `delete` and `WAL -> delete` resetting header bytes 18–19 to `1,1`; negative `PRAGMA default_cache_size=-500` reopening as positive `cache_size=500` (`src/prepare.c:325`); `VACUUM INTO` preserving `default_cache_size=777`; auditing 8.1's `settings-json`; and pin-mismatch enforcement across Experiments 8.1, 8.2, and 8.3.
- [`idle-cost-limits`](../agent_work/ch08/adversarial/idle-cost-limits/main.go) (`IDL-01`–`IDL-06b`): exhausting `RLIMIT_NOFILE=1024` across 1,000 WAL files (failed at `339/1000`, `1023/1024` FDs, `SQLITE_CANTOPEN` code `14`); running 8.2 at `-n 1000` (`3,000` exact FDs, `165 KiB/conn` uncounted); 50 WAL connections with 1,000-table schemas (`SCHEMA_USED = 251,601 B`) and 5 prepared statements (`STMT_USED = 28,240 B`) plus `PRAGMA shrink_memory` across them; `PRAGMA shrink_memory` while dirty pages are pinned in `BEGIN IMMEDIATE` (`166,188 -> 153,132 B`) versus after `ROLLBACK` (`5,164 B`); same-file WAL sequential open/close reuse (`findReusableFd` caps 10 cycles at `5` open / `4` parked) and 5 simultaneous connections (`11 -> 7 -> 0`); rollback-mode lock-dependent descriptor parking (`1 -> 2 -> 1` when idle vs `1 -> 2 -> 2 -> 1 -> 0` when holding a `SHARED` lock); and `procstat` overflow and path-resolution edge cases.
- [`manager-kills-and-limits`](../agent_work/ch08/adversarial/manager-kills-and-limits/main.go) (`MGR-01`–`MGR-03b`): 5 rounds of `SIGKILL` against a running bounded manager (60 DBs, `K = 12`, `249` acknowledged writes — `0` integrity failures, `0` step violations, `0` files left not at rest after recovery); Experiment 8.3 at `K = 1` (`peak open = 1`, `peak FDs = 3`); 8 concurrent goroutines × 20 ops at `Capacity = 1` with a re-entrant `OnEvict` hook (`0` errors, `0` deadlocks); `Capacity = 200` under `RLIMIT_NOFILE = 128` (`EMFILE` at DB 40, `126` FDs before and after, already-open DB servable, manual `Evict` unblocks); and Experiment 8.3's preflight descriptor-limit check under `RLIMIT_NOFILE = 128`.
- [`atrest-and-connmgr-edge`](../agent_work/ch08/adversarial/atrest-and-connmgr-edge/main.go) (`ATR-01a`–`ATR-05`): cross-process open readers (active writer + WAL, active reader + 0-byte WAL, opened-only with no statement); cross-process `PRAGMA locking_mode=EXCLUSIVE` WAL reader (no `-shm`) and active rollback reader; rollback-mode eviction with open `*sql.Rows` testing POSIX lock preservation against a second process; `connmgr.Stats()` accounting under `Open` failures; `Acquire` with a pre-cancelled context at capacity; and `CheckAtRest` on a directory, 32-byte header-only `-wal`, orphaned `-wal` without `-shm`, and 0-byte/cold/hot `-journal`.

**Run 1** ([`results-run1.txt`](../agent_work/ch08/adversarial/results-run1.txt)) produced **20 HELD, 9 BROKEN**. Seven broken attacks exposed real bugs that were fixed in code (alongside the code-review corrections, including removing the false claim that Experiment 8.3 B2 is `sqlite3_close_v2`'s zombie path, requiring `-n >= 20` in 8.2, reading `vacuum-into.db` header bytes 18–19 directly in 8.1, checking `whole` and `Result == 100` in 8.1's `strace` checks, and strengthening 8.3 B4/B5 checks):

- `OPN-05`: Experiment 8.1 left every `observed[].placement` struct empty in `settings-json`. Fixed by populating `Placement: placement` and regenerating `evidence/8.1-open-cost-and-where-settings-live.txt`.
- `IDL-06a`: `procstat.ParseStatus` and `ParsePSRSS` silently overflowed `int64` on `2^53` and `2^54` KiB inputs, returning `0` or negative bytes with `err == nil`. Fixed via `kibToBytes` rejecting values above `math.MaxInt64/1024`.
- `IDL-06b`: `procstat.DescriptorsUnder` and `RelativeTargets` returned zero matches for relative directories and `"/"` (producing `"//"`). Fixed by resolving `filepath.Abs` + `filepath.EvalSymlinks` in `directoryPrefix`.
- `ATR-01b`: A cross-process WAL connection using `PRAGMA locking_mode=EXCLUSIVE` created no `-shm`, refuting the draft claim that every open WAL connection keeps `-shm` and causing `CheckAtRest` to return `AtRest=true`. Fixed by adding `lockedByAnotherProcess` (`fcntl(F_GETLK)` on SQLite's lock bytes `0x40000000..+512`), which now catches both WAL exclusive-mode and active rollback-mode readers.
- `ATR-02`: Evicting a rollback-mode database while `*sql.Rows` was open falsely reported `AtRest=true` **and** `CheckAtRest`'s `os.Open` + `Close` stripped the open cursor's POSIX `SHARED` lock, allowing a second process to write. Fixed by checking `e.db.Stats().OpenConnections > 0` (and `CloseErr != nil`) in `evictLocked` and skipping `CheckAtRest`.
- `ATR-03`: `verifyBeforeOpen` incremented `UnverifiedOpens`/`VerifiedReopens` before `Open` ran, breaking `Misses == VerifiedReopens + UnverifiedOpens` whenever `Open` failed, and counted `CheckAtRest` I/O errors as `RefusedReopens` instead of `StatusFailures`. Fixed by incrementing open counters only after `Open` succeeds.
- `ATR-04`: `Acquire` with a pre-cancelled context at capacity evicted a healthy idle database before returning `context.Canceled`. Fixed by checking `ctx.Err()` right after the cache-hit check and before eviction.

Two broken attacks from Run 1 remained broken in **Run 2** ([`results-run2.txt`](../agent_work/ch08/adversarial/results-run2.txt): **27 HELD, 2 BROKEN**) and narrowed the chapter's claims rather than changing code:

- `IDL-03a` measured `319–321 KiB/conn` of uncounted RSS across 50 WAL connections with 1,000-table schemas (`21.3 MiB` counter sum vs `36.9 MiB` RSS growth), exceeding Experiment 8.2's `256 KiB/conn` margin. Sections 4 and 7 now state plainly that `256 KiB/conn` is specific to 8.2's 3-table workload and that uncounted overhead grows with schema size.
- `MGR-03a` showed that setting `Capacity = 200` under `RLIMIT_NOFILE = 128` fails at database 40 with `SQLITE_CANTOPEN` (`14`) without automatically evicting an idle entry (because `modernc.org/sqlite` does not surface `EMFILE` distinctly from other `CANTOPEN` errors), while leaking zero descriptors (`126` before and after), keeping open databases servable, and unblocking on manual `Evict`. Section 7 states the sizing rule `K <= (soft limit - reserve) / 3` in WAL mode and treats exceeding the OS descriptor limit as a configuration error caught at startup (as Experiment 8.3's preflight does, `MGR-03b`).

After updating `IDL-03a` and `MGR-03a` to assert those narrowed, documented contracts, the final run ([`results.txt`](../agent_work/ch08/adversarial/results.txt)) passed **29 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every capacity number and operational rule below comes directly from this chapter's experiments:

1. **Size `K` against `RLIMIT_NOFILE` using `3` descriptors per WAL database (Experiments 8.2 A, B, D; 8.3 A; `IDL-01`, `MGR-03a/b`).** An idle WAL connection holds `3` descriptors (`.db`, `-wal`, `-shm`) versus `1` in rollback `DELETE` mode (`2` during a write). At `RLIMIT_NOFILE = 1024`, an unbounded process ran out of descriptors at `339` WAL databases (`IDL-01`). Budget `K <= (soft limit - reserve) / 3` in WAL mode and validate it at startup (Experiment 8.3 preflight). Keep one connection per managed database (`SingleConnection: true`): multiple connections to the same file in one process add `2` descriptors each (`.db` + `-wal`, sharing `-shm`) and park closed `.db` descriptors in `UnixUnusedFd` until the last connection closes (Experiment 8.2 D, `IDL-05a`).
2. **Budget memory from measured process RSS for your own schema, not from `sqlite3_db_status` alone (Experiments 8.1 A; 8.2 B, C; 8.3 A; `IDL-03a/b`).** Per connection, `SCHEMA_USED` scales at roughly `251` bytes per 4-column table (`251,601` bytes for 1,000 tables in 8.1 A; `1,347` bytes for the 3-table recipe in 8.2 B), `STMT_USED` took `5,008` bytes for one prepared statement, and `CACHE_USED` scales with `cache_size` (with ~2× allocator overhead from Chapter 4). On top of those counters, uncounted RSS (`48 KiB` lookaside, driver/libc state, allocator rounding) added `168 KiB` per connection for 8.2's 3-table schema in the earlier capture (`44.2 MiB` growth vs `11.3 MiB` counter sum, `3.91×`; RSS is run-dependent) and `319–321 KiB` per connection for 1,000-table schemas (`IDL-03a`). Capping `K = 64` with `cache_size = -256` cut peak RSS growth across 1,000 databases from `211.4 MiB` to `22.7 MiB` in the earlier capture (about `211 MiB` to `20 MiB` in the lab run; Experiment 8.3 A). If you call `PRAGMA shrink_memory` with `modernc.org/sqlite`, remember it clears unpinned pages from **every** connection in the process (about `52,835` → `4,963` bytes across all 200 connections in the lab run of 8.2 C) while leaving `SCHEMA_USED`, `STMT_USED`, and dirty write-transaction pages (`IDL-04`) untouched.
3. **Price cold opens by schema size and eviction `fsync` cost (Experiments 8.1 A; 8.3 A).** Opening a file costs only one 100-byte read, but the first statement reads page 1 twice, opens `-wal` and `-shm`, and scans every `sqlite_schema` page (`1` page for 1 table, `5` for 100, `36` for 1,000). Under a full LRU manager, a cold read also pays for evicting an idle database (checkpoint + `fsync` + SHA-256) and hashing the target before opening—averaging `3.565ms` cold versus `112µs` warm in the earlier capture of Experiment 8.3 A (about `455µs` versus `18µs` in the lab run; run-dependent, `SHAPE`). Keep `K` large enough to hold the hot working set (`K = 64` held 8.3's `50` hot databases).
4. **Never copy, move, or declare a WAL file at rest until every handle is closed and `CheckAtRest` passes (Experiments 8.1 B; 8.3 A, B1–B6; `ATR-01a/b`, `ATR-02`).** Before copying a database file for rung R5 capture or relocation: (a) ensure all `*sql.Rows`, `*sql.Tx`, and `*sql.Conn` are closed (`OpenConnections == 0` after `db.Close()`, B2), (b) run `CheckAtRest` only when this process holds no open connection to the file, (c) require zero `-wal` bytes, no `-shm`, no hot `-journal`, and no cross-process lock (`F_GETLK`), (d) never rename or move a database while a connection is open (B5 leaves committed data in the old `-wal` while the moved file looks at rest), and (e) record the SHA-256 on eviction and verify it before reopening (B6).
5. **Store per-connection settings in the platform catalog and reapply them on every open (Experiment 8.1 B; `OPN-04a/b/c`).** A copied database carries only `page_size`, `journal_mode=wal`, `auto_vacuum`, `encoding`, `user_version`, `application_id`, and (if set) the deprecated `default_cache_size`. The platform catalog must store and reapply `synchronous`, `foreign_keys`, `cache_size` (explicitly, to override any foreign `default_cache_size`), `busy_timeout`, `wal_autocheckpoint`, and `secure_delete` on every connection—recorded in machine-readable form by [`internal/sqlitex/placement.go`](../internal/sqlitex/placement.go) and Experiment 8.1's placement tables (B2, B4). Whenever a rebuild uses `VACUUM INTO`, also reapply `PRAGMA journal_mode=wal`, because `VACUUM INTO` outputs a rollback-mode file.
6. **Do not use `ATTACH` to span tenant databases (from the SQLite documentation and [`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md), not demonstrated here).** `SQLITE_LIMIT_ATTACHED` defaults to `10` attached databases per connection, and per the SQLite documentation a transaction across attached WAL-mode databases is atomic per file only—not across files. A multi-database platform holding thousands of files should isolate each database to its own connection and never rely on cross-database `ATTACH` transactions.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 8.1 Part A, why does `sqlite3_open_v2` perform only a single 100-byte read regardless of whether the database has `1` or `1,000` tables, and why does the first statement in WAL mode read page 1 **twice** (once before opening `-wal` and `-shm` and once after)?
2. In Experiment 8.1 Part B, `PRAGMA cache_size` resets to `-2000` on a copied file, yet a new connection to `default-cache-size-copy.db` started with `cache_size = 777`. Which database header bytes and which lines of `prepare.c` cause this, and why must a platform also reapply `journal_mode=wal` after rebuilding a database with `VACUUM INTO`?
3. In Experiment 8.2, `200` idle WAL connections reported `CACHE_USED + SCHEMA_USED + STMT_USED = 11.3 MiB`, yet process RSS grew by `44.2 MiB` (`3.91×`) in the earlier capture (RSS is a run-dependent value that `course run` prints). What accounts for the uncounted memory, why did `PRAGMA shrink_memory` on connection 0 alone shrink `CACHE_USED` on all `200` connections, and why did closing connection 2 in Part D drop descriptors from `5` to `4` instead of `3`?
4. In Experiment 8.3 Part B, compare **B2** (unclosed `*sql.Rows`), **B3** (killed writer), and **B5** (file renamed while open). In each case, what happens to committed step 1, why did `connmgr` skip `CheckAtRest` in B2, and why is B5 uniquely dangerous for an at-rest check?
5. Why was `connmgr.CheckAtRest`'s original side-file check (`-wal`, `-shm`, hot `-journal`) insufficient to detect every active external reader, what did the `fcntl(F_GETLK)` lock probe add, and which external connections still remain invisible to `CheckAtRest`?

# Chapter 4 — Pager and page cache

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `pager.c`, `pcache.c`, `pcache1.c`, `status.c` |
| Linked C sources (in the pinned tree, not copied) | `btree.c`, `os_unix.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree, the C tools (`sqlite3`, `showwal`, `dbhash`; Experiment 4.1's twin check uses the C `sqlite3` CLI), `strace`, and the DuckDB CLI that presents the results.
> - **Run the experiments** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 4.1`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 4.1
>   go run ./cmd/course run 4.2
>   ```
>   In the container, scratch databases and tables are written under `/work/4.1/` and `/work/4.2/` (the `lab-work` volume); run directly, under `_work/4.1/` and `_work/4.2/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget`). Run this way it prints only its verdict and leaves its tables under `_work/4.2/results/`; Experiment 4.1's twin check then needs the C CLI on the host (`go run ./cmd/course fetch-source`, then `go run ./cmd/course build-tools`). `course run` cannot pass experiment flags, so flag variants such as Experiment 4.1's `-step` are run directly in the container: `docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal -step 3`.
> - **Linux only notes and the Mac route:**
>   - **Experiment 4.1** uses Linux `strace` (including `--inject=...:signal=SIGKILL`) for Part A's traced children and Parts D, D2, E, and F (**Linux only**). `course run` runs it in full in the Linux `lab` container. On macOS, running the experiment package directly executes Part A's portable header checks, Part B, Part C (decoding the live rollback journal with `internal/journalfile`), prints a `LINUX-ONLY:` notice, and commits the step in-process at `FULL`, `NORMAL`, and `OFF`.
>   - **Experiment 4.2** is portable. On Linux it reads `/proc/self/status` for total resident memory (`VmRSS`) and its breakdown into anonymous (`RssAnon`), file-backed (`RssFile`), and shared (`RssShmem`) memory. Run directly on macOS it falls back to `ps -o rss= -p <pid>`, reporting total RSS only; the anonymous-versus-file-backed check in Part E is **Linux only** and is skipped there.
>   - **Measured platform:** The stored evidence in this chapter was captured in the Debian `lab` container on `linux/arm64` (Go 1.26), whose `./configure`-built `sqlite3` CLI uses `fdatasync`. The sync syscall name, process memory (RSS) and raw `CACHE_USED` bytes are run-dependent: `course run` prints them, and they are not part of the stored evidence.

---

## 1. Model

The **pager** sits between SQLite's B-tree layer and the **VFS** (Virtual File System, the thin layer that forwards file operations to the operating system). The B-tree thinks in fixed-size **pages** (4,096 bytes by default). The pager owns three jobs:

1. **Atomic commit and crash recovery.** A single SQL transaction modifies multiple pages. If the process dies halfway through writing them, the file on disk holds a mix of old and new pages (a **torn** database). In rollback-journal mode, the pager protects the database by writing the original **page images** into a sidecar **rollback journal** (`<name>-journal`) before modifying the database file, then removing or invalidating that journal as the single **commit point**.
2. **Page caching.** Reading a page from disk on every B-tree lookup is slow, and modifying pages in place before a transaction commits would corrupt the file on rollback. The pager holds recently used pages and modified (**dirty**) pages in an in-memory **page cache** (`PCache`, backed by the default allocator and LRU manager `PCache1`).
3. **File locking.** The pager acquires and releases locks on the database file to coordinate readers and writers across processes. Chapter 6 examines the lock byte ranges and concurrency rules; this chapter follows the locks only where they gate pager state transitions.

```text
  B-tree layer (btree.c)
       |  sqlite3PagerGet / sqlite3PagerWrite / CommitPhaseOne / CommitPhaseTwo
       v
+-------------------------------------------------------------------------+
| Pager (pager.c)                                                         |
|                                                                         |
|  State machine (Pager.eState):                                          |
|    OPEN ---> READER ---> WRITER_LOCKED ---> WRITER_CACHEMOD             |
|      ^         ^                                  |  syncJournal()      |
|      |         |  pager_end_transaction()         v                     |
|      |         +------ WRITER_FINISHED <--- WRITER_DBMOD                |
|      |                    (commit point)       (writes to .db)          |
|      +--- ERROR (on unrecoverable I/O failure)                          |
|                                                                         |
|  +-----------------------------+    +--------------------------------+  |
|  | Page cache (pcache.c)       |    | Rollback journal logic         |  |
|  |  - dirty list (sorted at    |    |  - pInJournal bitmap           |  |
|  |    commit)                  |    |  - writeJournalHdr / syncJournal| |
|  |  - pluggable backend        |    |  - pager_playback (hot journal)|  |
|  |    (pcache1.c: PGroup LRU)  |    |                                |  |
|  +--------------+--------------+    +---------------+----------------+  |
+-----------------|-----------------------------------|-------------------+
                  | readDbPage / pager_write_pagelist | write / sync / unlink
                  | (or getPageMMap -> sqlite3OsFetch)|
                  v                                   v
            +-----------+                       +------------------+
            |  t.db     |                       |  t.db-journal    |
            |  (pages)  |                       |  [header sector] |
            +-----------+                       |  [pgno|page|sum] |
                                                +------------------+
                  ^                                   ^
                  +------------ VFS (os_unix.c) ------+
                   pread64, pwrite64, fsync/fdatasync, unlink, fcntl, mmap
```

Key terms used in this chapter:

- **Rollback journal (`-journal`).** A companion file holding copies of pages as they existed *before* the current transaction began. Each journal segment starts on a **sector size** boundary (512 bytes on this Linux ext4 system) with a 28-byte header padded to one sector, followed by records of `[4-byte page number][page image][4-byte checksum]`.
- **Nonce.** A 32-bit random initializer written into each journal header (`cksumInit`, generated via `sqlite3_randomness`) and folded into every record's checksum so stale records left over from an older transaction cannot pass validation.
- **Hot journal versus cold journal.** A **hot journal** is a `-journal` left behind by a dead writer that SQLite must replay to restore the database on the next open. SQLite considers a journal hot when it exists, no process holds a `RESERVED` lock on the database, the database has more than 0 pages, and the journal's first byte is non-zero. A **cold journal** is a `-journal` whose size is 0 or whose first byte is `0x00`; SQLite ignores it.
- **`fsync` / `fdatasync`.** Operating-system calls that flush modified file data (and, for `fsync`, metadata) from the kernel buffer cache to durable storage. A **directory fsync** (`fsync` on `.`) flushes the directory entry so a newly created filename survives power loss.
- **Dirty page and spill.** A **dirty page** is a page modified in the page cache and not yet written to the database file. If a transaction dirties more pages than the cache can hold, the cache triggers a **spill** (`pagerStress`): it syncs the journal early, starts a new journal segment, and writes unreferenced dirty pages to the database file before `COMMIT`.
- **Memory-mapped I/O (`mmap`).** Setting `PRAGMA mmap_size` asks the operating system to map the main database file into the process's address space. For pages served from the main `.db` file (that is, pages with no uncheckpointed frame in `-wal`), reads fetch pointers straight from the kernel's page cache instead of copying pages into SQLite's heap cache.
- **RSS (`VmRSS`, `RssAnon`, `RssFile`, `RssShmem`) and PSS (`Pss_File`).** Resident Set Size is the physical memory currently mapped into a process. On Linux, `/proc/self/status` splits `VmRSS` into **anonymous** heap memory (`RssAnon`), **file-backed** mapped pages (`RssFile`), and shared memory (`RssShmem`). When multiple mappings point to the same file, `RssFile` counts each mapping's resident page-table entries separately, while Proportional Set Size (`Pss_File` in `/proc/self/smaps_rollup`) divides shared pages across the mappings that share them.

**WAL mode as the platform default.** Platform databases in this course run in WAL (Write-Ahead Logging) mode, covered in Chapter 5. Chapter 4 evaluates that default and keeps one planned exception:

- **Planned exception (Experiment 4.1).** Chapter 4 is the rollback-journal chapter by subject, so Experiment 4.1 Parts B–F run in rollback mode (`journal_mode=delete`, plus `truncate` and `persist` in Part F). Rollback mode still reaches a WAL-default platform in two places that Part A measures directly: every newly created or imported database starts in `journal_mode=delete` until switched, and **the switch to WAL itself is a rollback-mode commit** (even `sqlitex.Open` on a brand-new WAL database creates, writes, syncs, and unlinks a `-journal` file).
- **WAL default kept (Experiment 4.2).** Every database in Experiment 4.2 runs in WAL mode (`sqlitex.DefaultOptions()`). The page cache operates identically in both modes, and WAL mode adds one measurable cost per distinct open database file: a 32 KiB `-shm` memory mapping visible in file-backed RSS (2 MiB across 64 distinct files).

**Relation to the Rebuild Ladder.** Chapter 4 adds no new rung to the Rebuild Ladder. Instead, it establishes two foundations that later rungs depend on: the rule that a rollback-mode database is its `.db` file *plus* its hot `-journal` (and why post-crash verification in rollback mode must compare at equality level E3 or canonical E1), and the per-connection memory model that sizes the bounded connection manager in Chapter 8 (rung R5) and the multi-database fleet in Chapter 10 (rung R7).

---

## 2. Question

What makes a multi-page rollback-journal commit all-or-nothing when the writer process dies at any write, sync, or unlink—and how much memory does each open database connection actually hold in this Go driver?

This chapter reuses four shared packages introduced in earlier chunks (linked rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite with explicit file and connection settings, checks the pinned `sqlite_source_id()`, manages `_work/N.M/` directories, locates the pinned `sqlite3` CLI, and wraps `sqlite3_db_status` (`ReadDBStatus`).
- [`internal/trace`](../internal/trace/): runs child processes under Linux `strace`, parses syscalls, and injects `SIGKILL` on entry to a chosen syscall ordinal (`KillAt`).
- [`internal/recipe`](../internal/recipe/): builds the deterministic 30-step workload (`accounts`, `documents`, `events`, `events_by_account`), applies single steps, and matches file SHA-256 hashes against known states.
- [`internal/dbfile`](../internal/dbfile/): parses database files in plain Go and computes the page census (`Census().FreelistLeaves`), used here to identify pre-step freelist leaves.

Chapter 4 introduces one new shared package, `internal/journalfile`—a read-only rollback-journal inspector with no SQLite dependency—which is inlined in full and walked through in Section 5.1.

---

## 3. Call path

All source citations below are at tag `version-3.53.4`. Four files are copied whole into this chapter directory: [`pager.c`](pager.c), [`pcache.c`](pcache.c), [`pcache1.c`](pcache1.c), and [`status.c`](status.c) (verified by `SOURCES.sha256` against the pinned tree `$SQLITE_SRC` or `_src/sqlite-3.53.4`). `btree.c` (copied in Chapter 2) and `os_unix.c` (copied in Chapter 7) are linked in the pinned tree.

### 3.1 Pager state machine and locks

1. **State machine and constants** — [`src/pager.c:135-350`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L135-L350) (`eState` comment) and [`src/pager.c:351-357`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L351-L357) (`PAGER_OPEN`…`PAGER_ERROR`, local copy: [`pager.c`](pager.c)). Defines the seven pager states (`0` through `6`) and the invariants held in each:
   - `PAGER_OPEN` (`0`): file is open, no lock held, no transaction active.
   - `PAGER_READER` (`1`): `SHARED` lock held; pages may be read, none modified. Entered via `sqlite3PagerSharedLock`.
   - `PAGER_WRITER_LOCKED` (`2`): `RESERVED` lock held (in rollback mode) or WAL write lock held; journal not yet opened, no pages modified. Entered via `sqlite3PagerBegin`.
   - `PAGER_WRITER_CACHEMOD` (`3`): rollback journal is open and its first header is written; dirty pages exist in cache, but the database file itself has not been modified. Entered via `pager_open_journal`.
   - `PAGER_WRITER_DBMOD` (`4`): journal has been synced (`syncJournal`), and the pager is now permitted to write dirty pages to the database file.
   - `PAGER_WRITER_FINISHED` (`5`): all dirty pages have been written and synced to the database file (`sqlite3PagerCommitPhaseOne`), but the journal still exists.
   - `PAGER_ERROR` (`6`): an unrecoverable I/O error occurred.
2. **Lock-state tracking** — [`src/pager.c:360-406`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L360-L406) (`eLock` comment, local copy: [`pager.c`](pager.c)). `Pager.eLock` mirrors the lock held on the database file (`NO_LOCK`, `SHARED`, `RESERVED`, `EXCLUSIVE`, or `UNKNOWN_LOCK` after a failed unlock so the next reader rechecks for a hot journal).

### 3.2 Beginning a write transaction and building the journal

3. **`sqlite3PagerBegin`** — [`src/pager.c:5968-6032`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5968-L6032) (local copy: [`pager.c`](pager.c)). Transitions `PAGER_READER -> PAGER_WRITER_LOCKED` (line 6018). In rollback mode it acquires a `RESERVED` lock (line 6002, or `EXCLUSIVE` immediately if requested); in WAL mode it acquires the WAL write lock (line 5995). It records `pPager->dbOrigSize`, the page count before the transaction begins.
4. **`pager_write` and `pager_open_journal`** — [`src/pager.c:6094-6173`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6094-L6173) and [`src/pager.c:5877-5949`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5877-L5949) (local copy: [`pager.c`](pager.c)). On the first page modification of a write transaction, `pager_write` calls `pager_open_journal` (line 6121), which allocates the `pInJournal` bitmap, opens `<db>-journal`, calls `writeJournalHdr` (line 5935), and sets `eState = PAGER_WRITER_CACHEMOD` (line 5945). `pager_write` then marks the page dirty (`sqlite3PcacheMakeDirty`, line 6128). If the page number is within `dbOrigSize` and not yet marked in `pInJournal` (lines 6135–6143), it calls `pagerAddPageToRollbackJournal`. Pages appended past `dbOrigSize` are **not** journaled (lines 6144–6147); they are only marked `PGHDR_NEED_SYNC` because rollback removes them by truncating the file back to `dbOrigSize`.
5. **`writeJournalHdr` and `journalHdrOffset`** — [`src/pager.c:1449-1571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1449-L1571) and [`src/pager.c:1366-1376`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1366-L1376) (local copy: [`pager.c`](pager.c)). Every journal header begins on a sector boundary (`journalHdrOffset`). `writeJournalHdr` formats the 28-byte header (8-byte magic `aJournalMagic`, 4-byte record count, 4-byte random nonce `cksumInit` from `sqlite3_randomness`, 4-byte `dbOrigSize`, 4-byte `sectorSize`, 4-byte `pageSize`), pads it with zeros to a full sector, and writes it:
   ```c
   if( pPager->noSync || (pPager->journalMode==PAGER_JOURNALMODE_MEMORY)
    || (sqlite3OsDeviceCharacteristics(pPager->fd)&SQLITE_IOCAP_SAFE_APPEND)
   ){
     memcpy(zHeader, aJournalMagic, sizeof(aJournalMagic));
     put32bits(&zHeader[sizeof(aJournalMagic)], 0xffffffff);
   }else{
     memset(zHeader, 0, sizeof(aJournalMagic)+4);
   }
   ```
   That branch (lines 1495–1502) is why a rollback journal written under `synchronous=FULL` or `NORMAL` starts out **cold**: its first 12 bytes (magic and record count) are written as zeros until `syncJournal` runs—either at `COMMIT` (when the transaction fits in the page cache) or earlier during a mid-transaction spill via `pagerStress`. Under `synchronous=OFF` (`noSync`), SQLite writes the magic and `0xffffffff` ("infer the record count from file size") immediately, so the journal is hot as soon as the header hits the file.
6. **`pagerAddPageToRollbackJournal` and `pager_cksum`** — [`src/pager.c:6037-6085`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6037-L6085) and [`src/pager.c:2251-2259`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2251-L2259) (local copy: [`pager.c`](pager.c)). Appends one record to the journal in three separate `sqlite3OsWrite` calls: the 4-byte big-endian page number, the `pageSize`-byte original page image, and the 4-byte checksum (lines 6062–6067), advancing `journalOff` by `pageSize + 8` and setting the page's bit in `pInJournal`. `pager_cksum` starts from `cksumInit` and sums every 200th byte stepping down from `pageSize - 200` while `i > 0` (20 sampled bytes on a 4,096-byte page: offsets 3896, 3696, …, 96). Because `pager_cksum` samples only 20 bytes per 4,096-byte page, a single-byte corruption at an unsampled offset is not detected by the journal checksum.

### 3.3 Two-phase commit: `syncJournal`, database writes, and the commit point

7. **`sqlite3PagerCommitPhaseOne`** — [`src/pager.c:6511-6730`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6511-L6730) (local copy: [`pager.c`](pager.c)). In rollback mode, Phase One brings the database file up to the new state while keeping the journal intact: it updates the change counter on page 1 (journaling page 1 if not already journaled, lines 6620–6636), writes a super-journal name if multiple databases participate (line 6644), calls `syncJournal` (line 6658), writes every dirty page with `pager_write_pagelist` (line 6666/6696), extends an undersized file if `dbSize > dbFileSize` (lines 6704–6715), calls `sqlite3PagerSync` (line 6719), and sets `eState = PAGER_WRITER_FINISHED` (line 6727). Phase One only *extends* an undersized file; shrinking a database file happens later, in `pager_end_transaction`.
8. **`syncJournal`** — [`src/pager.c:4332-4441`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4332-L4441) (local copy: [`pager.c`](pager.c)). Takes the `EXCLUSIVE` lock, probes 8 bytes at the next sector boundary and zeroes byte 0 there if a stale `PERSIST`-mode header follows (lines 4380–4385), then executes the exact sync sequence that distinguishes `FULL` from `NORMAL`:
   ```c
   if( pPager->fullSync && 0==(iDc&SQLITE_IOCAP_SEQUENTIAL) ){
     rc = sqlite3OsSync(pPager->jfd, pPager->syncFlags);
     if( rc!=SQLITE_OK ) return rc;
   }
   rc = sqlite3OsWrite(
       pPager->jfd, zHeader, sizeof(zHeader), pPager->journalHdr
   );
   if( rc!=SQLITE_OK ) return rc;
   ...
   rc = sqlite3OsSync(pPager->jfd, pPager->syncFlags|
     (pPager->syncFlags==SQLITE_SYNC_FULL?SQLITE_SYNC_DATAONLY:0)
   );
   ```
   In `FULL` mode (`fullSync` true, lines 4401–4420), SQLite syncs the journal *first* (flushing the records and, on the first sync of a new journal file, triggering the VFS directory sync in `unixSync`), writes the 12-byte `zHeader` (magic + `nRec`) at offset 0—making the journal hot—and then syncs the journal a second time. In `NORMAL` mode (`fullSync` false), SQLite skips the first sync, writes the 12-byte magic + `nRec` immediately, and performs one journal sync (plus the directory sync). In `OFF` mode (`noSync`), `syncJournal` issues no writes and no syncs. At line 4438 it sets `eState = PAGER_WRITER_DBMOD`. When called mid-transaction by `pagerStress` (`newHdr=1`), `syncJournal` also opens a new header segment on the next sector boundary (`lines 4423-4427`), so a spilling transaction runs this sync cycle multiple times.
9. **`pager_write_pagelist` and `sqlite3PagerSync`** — [`src/pager.c:4475-4556`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4475-L4556) and [`src/pager.c:6446-6456`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6446-L6456) (local copy: [`pager.c`](pager.c)). `sqlite3PcacheDirtyList` ([`src/pcache.c:818-824`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L818-L824), local copy: [`pcache.c`](pcache.c)) merge-sorts dirty pages by ascending page number, and `pager_write_pagelist` writes each page to `(pgno - 1) * pageSize`, counting each write in `aStat[PAGER_STAT_WRITE]`. `sqlite3PagerSync` sends `SQLITE_FCNTL_SYNC` and syncs the database file unless `noSync`.
10. **`sqlite3PagerCommitPhaseTwo`, `pager_end_transaction`, and `zeroJournalHdr`** — [`src/pager.c:6748-6786`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6748-L6786), [`src/pager.c:2052-2180`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2052-L2180), and [`src/pager.c:1399-1432`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1399-L1432) (local copy: [`pager.c`](pager.c)). Phase Two calls `pager_end_transaction`, which executes the **commit point** according to `journal_mode` (lines 2086–2122):
    - `DELETE`: closes and deletes `-journal` (`sqlite3OsDelete`, line 2120).
    - `TRUNCATE`: truncates `-journal` to 0 bytes (`sqlite3OsTruncate`, line 2090), followed by `sqlite3OsSync` on the journal when `fullSync` is set (lines 2091–2098).
    - `PERSIST`: calls `zeroJournalHdr` (line 2104), overwriting the first 28 bytes of `-journal` with zeros and syncing the journal unless `noSync`.
    After finalizing the journal—and while still holding the `EXCLUSIVE` lock—`pager_end_transaction` truncates the database file if the transaction shrank it (`bCommit && dbFileSize > dbSize`, lines 2155–2164). Because that `ftruncate` runs *after* the journal is gone, a process crash right on the truncation leaves a committed database whose logical content is post-step, with trailing slack pages beyond the header's page count. Finally, `pager_end_transaction` destroys `pInJournal`, marks cached pages clean, drops the lock back to `SHARED`, and sets `eState = PAGER_READER`.

### 3.4 Hot-journal recovery and the `PAGER_GET_NOCONTENT` exception

11. **`sqlite3PagerSharedLock`, `hasHotJournal`, and `pagerSyncHotJournal`** — [`src/pager.c:5300-5507`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5300-L5507), [`src/pager.c:5180-5271`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5180-L5271), and [`src/pager.c:4094-4103`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4094-L4103) (local copy: [`pager.c`](pager.c)). On the first read or write (`OPEN -> READER`), `sqlite3PagerSharedLock` takes a `SHARED` lock and calls `hasHotJournal`. `hasHotJournal` checks four conditions in order: (a) `<db>-journal` exists (`sqlite3OsAccess`, line 5196), (b) no process holds `RESERVED` (`sqlite3OsCheckReservedLock`, line 5209), (c) the database has more than 0 pages (if `nPage == 0`, it deletes the stale journal instead, lines 5224–5230), and (d) the first byte of `-journal` is non-zero (`sqlite3OsRead` of 1 byte at offset 0, lines 5243–5251). When `hasHotJournal` reports true, `sqlite3PagerSharedLock` jumps straight to an `EXCLUSIVE` lock (line 5354, bypassing `RESERVED` so other readers cannot see the torn database), syncs the hot journal (`pagerSyncHotJournal`, line 5400), and runs `pager_playback` (line 5402).
12. **`pager_playback`, `readJournalHdr`, and `pager_playback_one_page`** — [`src/pager.c:2864-3052`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2864-L3052), [`src/pager.c:1590-1693`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1590-L1693), and [`src/pager.c:2298-2500`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2298-L2500) (local copy: [`pager.c`](pager.c)). `pager_playback` first checks for a super-journal footer (`readSuperJournal`, [`src/pager.c:1303-1349`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1303-L1349)); if a super-journal is named and does not exist on disk, the multi-database commit already succeeded and rollback skips playback completely (lines 2891–2897). Otherwise it loops over headers via `readJournalHdr`. If the remaining file is shorter than one full sector (`journalOff + JOURNAL_HDR_SZ(pPager) > journalSize`, lines 1607–1610), or if the magic or power-of-two geometry fails, `readJournalHdr` returns `SQLITE_DONE` before truncating anything. On a valid first header, `pager_playback` truncates the database back to `dbOrigSize` (`pager_truncate`, line 2952) and calls `pager_playback_one_page` for each record. `pager_playback_one_page` stops on page `0` or `PAGER_SJ_PGNO`, skips pages larger than `dbOrigSize`, stops on a checksum mismatch, and otherwise writes the original page image back to the database file. Finally, `pager_playback` syncs the restored database and calls `pager_end_transaction` (which deletes the journal in `DELETE` mode).
13. **`getPageNormal` and `PAGER_GET_NOCONTENT`** — [`src/pager.c:5581-5683`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5581-L5683) (local copy: [`pager.c`](pager.c)) and `allocateBtreePage` at [`src/btree.c:6740`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6740) (in the pinned tree and Chapter 2's copy). When the B-tree pops a leaf page off the freelist, its old content is unused, so `allocateBtreePage` passes `PAGER_GET_NOCONTENT` to the pager. Inside `getPageNormal` (lines 5645–5661), SQLite zeroes the in-memory page instead of reading disk *and* sets the page's bit in `pInJournal`:
    ```c
    if( noContent ){
      sqlite3BeginBenignMalloc();
      if( pgno<=pPager->dbOrigSize ){
        TESTONLY( rc = ) sqlite3BitvecSet(pPager->pInJournal, pgno);
      }
    ...
    }
    ```
    When `pager_write` later marks that page dirty, `pInJournal` already has its bit set, so **no journal record is written for a reused freelist leaf**. If a crash happens after the database write reaches that page, hot-journal rollback restores the freelist trunk page (putting the page back on the freelist) but leaves the new bytes sitting inside that unused freelist leaf. Experiment 4.1 demonstrates this directly.

### 3.5 Page cache, spills, unified LRU, mmap, and `sqlite3_db_status`

14. **`PCache`, `numberOfCachePages`, dirty list, and fetch/stress** — [`src/pcache.c:41-54`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L41-L54) (`struct PCache`), [`src/pcache.c:195-259`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L195-L259) (`pcacheManageDirtyList`), [`src/pcache.c:277-292`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L277-L292) (`numberOfCachePages`), [`src/pcache.c:403-432`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L403-L432) (`sqlite3PcacheFetch`), [`src/pcache.c:445-490`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L445-L490) (`sqlite3PcacheFetchStress`), [`src/pcache.c:593-607`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L593-L607) (`sqlite3PcacheMakeDirty`), and [`src/pager.c:4655-4727`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4655-L4727) (`pagerStress`, local copies: [`pcache.c`](pcache.c), [`pager.c`](pager.c)). `numberOfCachePages` translates `PRAGMA cache_size`: a positive `N` means `N` pages; a negative `N` means `(-1024 * N) / (szPage + szExtra)` pages (KiB budget). When a cache fills with dirty pages above `szSpill`, `sqlite3PcacheFetchStress` invokes `pagerStress`, which increments `PAGER_STAT_SPILL`, calls `syncJournal(pPager, 1)` (syncing the journal and starting a new header segment for subsequent writes), and writes an unreferenced dirty page out to the database file early.
15. **`pcache1` structures, unified `PGroup`, `shrink_memory`, and the `N - 1` rule** — [`src/pcache1.c:117-127`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L117-L127) (`struct PgHdr1`), [`src/pcache1.c:158-165`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L158-L165) (`struct PGroup`), [`src/pcache1.c:175-203`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L175-L203) (`struct PCache1`), [`src/pcache1.c:209-211`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L209-L211) (`struct PgFreeslot`), [`src/pcache1.c:520-526`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L520-L526) (`pcache1UnderMemoryPressure`), [`src/pcache1.c:621-637`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L621-L637) (`pcache1EnforceMaxPage`), and [`src/pcache1.c:874-941`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L874-L941) (`pcache1FetchStage2`, local copy: [`pcache1.c`](pcache1.c)). Three details in `pcache1.c` govern the numbers Experiment 4.2 and the adversarial checks measure:
    - **Unified LRU under `ENABLE_MEMORY_MANAGEMENT` (`pcache1Init`, [`src/pcache1.c:717-724`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L717-L724); `pcache1Cachesize`, [`src/pcache1.c:814-832`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L814-L832); `pcache1FetchStage2`, `src/pcache1.c:899-916`).** When compiled with `SQLITE_ENABLE_MEMORY_MANAGEMENT`—which `modernc.org/sqlite v1.60.1` enables—`pcache1.separateCache` is `0`, so every open connection in the process shares one global `PGroup` and one LRU list. Each connection's `cache_size` adds its share `nMax` to `pGroup->nMaxPage`. When a cache needs a page and reaches its own limit, `pcache1FetchStage2` recycles `pGroup->lru.pLruPrev`, **whichever connection owns it**:
      ```c
      if( pCache->bPurgeable
       && !pGroup->lru.pLruPrev->isAnchor
       && ((pCache->nPage+1>=pCache->nMax) || pcache1UnderMemoryPressure(pCache))
      ){
        PCache1 *pOther;
        pPage = pGroup->lru.pLruPrev;
        pcache1RemoveFromHash(pPage, 0);
        pcache1PinPage(pPage);
        pOther = pPage->pCache;
      ```
      An active connection can therefore hold more pages than its own `cache_size` by recycling pages from idle connections. On a single first-pass sweep across `N` open connections, the process-wide sum equals `N × (nMax - 1)`. Under connection-pool churn or a second round-robin sweep across evicted connections (where `nPage < nMax`), allocations climb to the true group cap `pGroup->nMaxPage = N × nMax`, where `pcache1Unpin` ([`src/pcache1.c:1096`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L1096)) trims any excess. Without `SQLITE_ENABLE_MEMORY_MANAGEMENT` (such as the C `sqlite3` CLI here), `separateCache` is `1`, giving each connection its own private `PGroup` and enforcing `cache_size` per connection.
    - **Process-wide `PRAGMA shrink_memory` (`pcache1Shrink`, [`src/pcache1.c:839-851`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L839-L851)).** `pcache1Shrink` temporarily sets `pGroup->nMaxPage = 0`, calls `pcache1EnforceMaxPage` (`src/pcache1.c:621-637`), and restores `nMaxPage`. Under `SQLITE_ENABLE_MEMORY_MANAGEMENT`, `pGroup` is process-wide—so running `PRAGMA shrink_memory` on *any* single connection (even an idle one holding 0 pages) evicts every other connection's unpinned clean pages while leaving cursor-pinned and uncommitted dirty pages intact.
    - **The `N - 1` rule for `N >= 10`.** The recycle condition at line 902 is `pCache->nPage + 1 >= pCache->nMax`. Once `nPage` reaches `nMax - 1`, subsequent misses recycle an existing page instead of allocating a new one: `cache_size=10` holds `9` pages, `100` holds `99`, `200` holds `199`, and `cache_size=-2000` (`nMax = 2048000 / (4096 + szExtra) = 483`) holds `482` pages. For tiny caches (`N <= 2`), the cache rests at `N` (`0 -> 0`, `1 -> 1`, `2 -> 2`) and temporarily holds more than `N` while a statement runs (`4` pages pinned by an open scan cursor cannot be recycled). Huge `cache_size` values allocate lazily, holding only the pages actually read.
16. **Memory-mapped reads (`mmap`)** — [`src/pager.c:3579-3590`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3579-L3590) (`pagerFixMaplimit`) and [`src/pager.c:5687-5752`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5687-L5752) (`getPageMMap`, local copy: [`pager.c`](pager.c)). Setting `PRAGMA mmap_size > 0` switches the pager's fetch method to `getPageMMap` and passes the limit to the VFS (`SQLITE_FCNTL_MMAP_SIZE`). In WAL mode, `getPageMMap` checks `sqlite3WalFindFrame` first (lines 5719–5726): if the requested page has a frame in `-wal` (`iFrame != 0`), `getPageMMap` falls back to `getPageNormal` and copies the page into `pcache1`. Only when `iFrame == 0`—a page read from the main `.db` file, such as a checkpointed database—does `sqlite3OsFetch` (`unixFetch` -> `unixMapfile` in `src/os_unix.c`, linked in the pinned tree) return a pointer straight into the `mmap` region and wrap it in a lightweight `pagerAcquireMapPage` handle outside `pcache1`.
17. **Per-connection counters** — [`src/status.c:203-421`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/status.c#L203-L421) (`sqlite3_db_status64`, local copy: [`status.c`](status.c)), [`src/pager.c:6881-6887`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6881-L6887) (`sqlite3PagerMemUsed`), and [`src/pager.c:6928-6946`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6928-L6946) (`sqlite3PagerCacheStat`). `SQLITE_DBSTATUS_CACHE_USED` sums `sqlite3PagerMemUsed` across attached databases:
    ```c
    int sqlite3PagerMemUsed(Pager *pPager){
      int perPageSize = pPager->pageSize + pPager->nExtra
        + (int)(sizeof(PgHdr) + 5*sizeof(void*));
      return perPageSize*sqlite3PcachePagecount(pPager->pPCache)
               + sqlite3MallocSize(pPager)
               + pPager->pageSize;
    }
    ```
    Because `pPager` stores the database, journal, and WAL filenames inside its single heap allocation, `sqlite3MallocSize(pPager)`—and therefore raw `CACHE_USED` bytes—varies with the database's path length. `SQLITE_DBSTATUS_CACHE_HIT`, `CACHE_MISS`, `CACHE_WRITE`, and `CACHE_SPILL` read `aStat[]` through `sqlite3PagerCacheStat`.

### Version notes

Behavior differences across SQLite releases relevant to the pager and page cache (sources: [`docs/knowledge/source-inventory-ch01-04.md`](../docs/knowledge/source-inventory-ch01-04.md), `changes.html`, `pgszchng2016.html`, and the copied source):

- **Default `cache_size = -2000` (3.12.0).** Release 3.12.0 changed the default page size from `1024` to `4096` and the default `cache_size` from `2000` pages to `-2000` (2,000 KiB = 2,048,000 bytes; `numberOfCachePages`, `src/pcache.c:277-292`).
- **`sqlite3_db_status` counters added over time (`changes.html`).** `CACHE_HIT` and `CACHE_MISS` were added in 3.7.9, `CACHE_WRITE` in 3.7.12, `CACHE_SPILL` in 3.23.0, and `TEMPBUF_SPILL` in 3.51.0 (present at `src/status.c:390`).
- **Hard heap limit (3.31.0).** `sqlite3_hard_heap_limit64` and `PRAGMA hard_heap_limit` were added in 3.31.0 (`changes.html`).
- **Journal header page-size field (pre-3.5.8).** Versions prior to 3.5.8 left the journal header's page-size field unset; `readJournalHdr` (`src/pager.c:1650`) still accommodates that legacy format.

---

### Experiment 4.1 — Atomic commit and hot journal (`4.1-atomic-commit-and-hot-journal`)

- **Claim:**
  1. New databases start in rollback-journal mode (`journal_mode=delete`, header bytes 18–19 = `1,1`), and switching a new database to WAL—including inside the platform opener `sqlitex.Open`—commits page 1 through a rollback `-journal`.
  2. In rollback mode, for a transaction that fits in the page cache (`spill=0`), SQLite writes all original page images to `-journal` and syncs the journal before any write reaches the database file: 4 syncs per commit at `synchronous=FULL` (`journal-sync`, `dir-sync`, 12-byte `journal-count`, `journal-sync`, database writes, `db-sync`, `journal-unlink`), 3 syncs at `NORMAL` (12-byte `journal-count` before the single `journal-sync` and `dir-sync`), and 0 syncs at `OFF`. (If a transaction spills via `pagerStress`, journal segments and database writes interleave with additional syncs while keeping per-page journal-before-database ordering.) The pinned C `sqlite3` CLI produces the exact same writes, syncs, and unlink in the same order.
  3. Killing the writer on entry to every write, sync, and unlink across `FULL`, `NORMAL`, and `OFF` always leaves a database that SQLite restores to the pre-step or post-step state—never a third state—with the flip occurring on the journal finalization syscall (`unlink` in `DELETE`, `ftruncate` in `TRUNCATE`, 28-byte header zero in `PERSIST`). At the byte level, hot-journal rollback restores exact pre-step bytes whenever the pre-step file has no freelist (Part D2, step 1), but leaves new bytes inside reused freelist leaves when the transaction pops pages off the freelist via `PAGER_GET_NOCONTENT` (Part D, step 3, page 22); in every such case, `integrity_check` is `ok` and the `VACUUM INTO` canonical hash equals pre-step's. (For a shrinking transaction, `ftruncate` on the database file runs *after* journal finalization in `pager_end_transaction`, leaving logical post-step content with trailing slack pages if killed there.)
  4. Deleting a hot journal or copying a crashed database without its hot journal either yields an unreadable malformed database or silently commits a transaction whose `COMMIT` never returned (Part E).
- **Why kept:** A WAL-mode platform still meets rollback mode. Every new database starts in it, the switch to WAL itself commits through a `-journal`, and a file moved without its `-journal` is a corrupt file. One full-depth experiment covers all of that; a Go journal replayer is not built because the platform never replays rollback journals itself.
- **Evidence form:** `EXACT` (every stored table matches byte for byte on repeated runs; the run-dependent `info` tables carry the sync syscall name, random per-run journal nonces, and runtimes, and are not stored).
- **Linux only (parts A traced children, D, D2, E, F):** `course run` runs them in the Linux `lab` container.
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 4.1
  ```
  Optional flags: `-step 3` (traced and killed recipe step), `-exact-step 1` (freelist-free step; `0` skips Part D2), `-workers 4` (parallel kill workers). `course run` cannot pass them; run a flag variant directly in the container, for example `docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal -exact-step 0`.

<!-- include: experiments/4.1-atomic-commit-and-hot-journal/main.go -->
```go
// Experiment 4.1 traces a multi-page rollback-journal commit through the
// kernel, decodes the live journal with internal/journalfile, kills the writer
// on entry to every write, sync, and unlink across synchronous=FULL/NORMAL/OFF,
// cross-checks the syscall stream against the pinned C CLI, demonstrates what
// happens when a hot journal is lost, and locates the exact commit point in
// DELETE, TRUNCATE, and PERSIST modes.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 4.1 (the strace parts need
// Linux; the lab container has it).
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/journalfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal"

const (
	dbName      = "t.db"
	pageSize    = 4096
	markerBegin = "marker-begin"
	markerEnd   = "marker-end"
)

var (
	stepFlag  = flag.Int("step", 3, "recipe step whose commit is traced and killed (the pre-step state is the step before it)")
	exactFlag = flag.Int("exact-step", 1, "second recipe step, killed at every point too, whose pre-step state has no freelist pages (0 skips it)")
	workers   = flag.Int("workers", 4, "kill runs executed in parallel")
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 4.1" runs both.
var (
	set    *results.Set
	tables struct {
		parts, headerModes, creations, creationCalls, steps, states, journals, journalRecords, liveRollback *results.Table
		commits, commitCalls, twins, twinDiffs, kills, crashes, reopens, commitPoints, info, infoKills      *results.Table
	}
	creationSeq, journalSeq, commitSeq, crashSeq, reopenSeq, infoSeq int
)

var journalColumns = []string{"journal_bytes", "first_byte", "hot", "header", "record_count", "initial_pages", "sector_size", "page_size", "records", "record_pages", "checksum_failures", "trailing_bytes"}

func openTables() {
	tables.parts = set.Table("parts", "part", "what", "status")
	tables.headerModes = set.Table("header_modes", "seq", "stage", "journal_mode", "byte_18", "byte_19")
	tables.creations = set.Table("creations", "seq", "opener", "statement", "journal_calls", "wal_writes")
	tables.creationCalls = set.Table("creation_calls", "creation_seq", "n", "call")
	tables.steps = set.Table("steps", "step", "pre_step", "recipe", "schema", "inserts", "updates", "deletes", "indexes")
	tables.states = set.Table("states", "step", "state", "recipe_step", "pages", "sha256_prefix", "freelist_leaves")
	tables.journals = set.Table("journals", append([]string{"seq", "part", "label"}, journalColumns...)...)
	tables.journalRecords = set.Table("journal_records", "seq", "file_offset", "page", "role", "checksum", "same_as_pre_step")
	tables.liveRollback = set.Table("live_rollback", "file_after", "journal_left")
	tables.commits = set.Table("commits", "seq", "part", "step", "journal_mode", "synchronous", "sequence", "journaled_pages", "record_writes", "db_page_writes", "syncs", "sync_targets")
	tables.commitCalls = set.Table("commit_calls", "commit_seq", "n", "kind", "call")
	tables.twins = set.Table("twins", "synchronous", "go_calls", "cli_calls", "identical", "cli_leaves_post_step")
	tables.twinDiffs = set.Table("twin_diffs", "synchronous", "n", "go_call", "cli_call")
	tables.kills = set.Table("kills", append(append([]string{"part", "step", "journal_mode", "synchronous", "kill_no", "syscall", "kind", "landed_ok", "landed", "db_file", "db_pages", "differ_pre", "differ_post"}, journalColumns...),
		"playback", "playback_pages", "truncate_to", "outcome", "outcome_name", "outcome_pages", "recipe_match", "integrity", "journal_after", "covered")...)
	tables.crashes = set.Table("crashes", "seq", "crash", "killed_on", "db_write_no", "db_writes", "db_file", "db_pages", "differ_pre", "differ_post")
	tables.reopens = set.Table("reopens", "seq", "scenario", "open_error", "integrity", "user_version", "accounts", "documents", "events", "outcome_name", "outcome_pages")
	tables.commitPoints = set.Table("commit_points", "seq", "journal_mode", "commit_point", "killed_on_entry", "next_kind", "killed_on_next")
	tables.info = set.Table("info", "seq", "name", "value")
	tables.infoKills = set.Table("info_kills", "part", "step", "journal_mode", "synchronous", "kill_no", "journal_nonce", "run_ms")
}

func info(name string, value any) {
	infoSeq++
	tables.info.Row(infoSeq, name, value)
}

// init pins the main goroutine to one OS thread so strace's per-thread syscall
// counter observes the child writer's SQLite syscalls in a deterministic order.
func init() {
	runtime.LockOSThread()
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func rollbackRecipe() recipe.Recipe {
	return recipe.Default().WithJournalMode(sqlitex.JournalDelete)
}

func stepOptions(journalMode, synchronous string) sqlitex.Options {
	options := rollbackRecipe().SQLiteOptions()
	options.File.JournalMode = journalMode
	options.Conn.Synchronous = synchronous
	return options
}

// touch opens and reads an empty marker file so strace records an unambiguous
// open/openat boundary around the traced transaction without touching SQLite.
func touch(dir, name string) {
	if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
		fatal("marker", err)
	}
}

func childStep(dir, journalMode, synchronous string, k int) {
	ctx := context.Background()
	step, err := rollbackRecipe().Step(k)
	if err != nil {
		fatal("child step", err)
	}
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(journalMode, synchronous))
	if err != nil {
		fatal("child open", err)
	}
	touch(dir, markerBegin)
	if err := recipe.ApplyStep(ctx, db, step); err != nil {
		fatal("child apply", err)
	}
	touch(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func copyFile(from, to string) {
	data, err := os.ReadFile(from)
	if err != nil {
		fatal("copy", err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		fatal("copy", err)
	}
}

func mkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
}

func prepareDir(dir, pre string) {
	mkdir(dir)
	if err := recipe.RemoveDatabase(filepath.Join(dir, dbName)); err != nil {
		fatal("remove", err)
	}
	copyFile(pre, filepath.Join(dir, dbName))
	for _, name := range []string{markerBegin, markerEnd} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

type states struct {
	step, preStep  int
	prePath        string
	pre, post      []byte
	prePages       int
	postPages      int
	hashes         []recipe.FileHashes
	preFreeLeaves  []uint32
	preCanon       string
	postCanon      string
	stepStatements recipe.Step
}

func newStates(ctx context.Context, dir string, r recipe.Recipe, k int, hashes []recipe.FileHashes) *states {
	mkdir(dir)
	s := &states{step: k, preStep: k - 1, hashes: hashes, prePath: filepath.Join(dir, "pre.db")}
	var err error
	s.stepStatements, err = r.Step(k)
	if err != nil {
		fatal("step", err)
	}
	if err := r.Build(ctx, s.prePath, k-1); err != nil {
		fatal("build pre-step", err)
	}
	postPath := filepath.Join(dir, "post.db")
	if err := r.Build(ctx, postPath, k); err != nil {
		fatal("build post-step", err)
	}
	if s.pre, err = os.ReadFile(s.prePath); err != nil {
		fatal("read pre", err)
	}
	if s.post, err = os.ReadFile(postPath); err != nil {
		fatal("read post", err)
	}
	s.prePages, s.postPages = len(s.pre)/pageSize, len(s.post)/pageSize
	// Collect pre-step freelist leaves: when the B-tree reuses a freelist page,
	// it fetches with PAGER_GET_NOCONTENT (src/btree.c:6740), which marks the
	// page in pInJournal without writing a journal record (src/pager.c:5645-5656).
	// A hot-journal rollback therefore cannot restore those unused bytes.
	file, err := dbfile.FromBytes(s.pre)
	if err != nil {
		fatal("parse pre", err)
	}
	census, err := file.Census()
	if err != nil {
		fatal("census pre", err)
	}
	for _, leaf := range census.FreelistLeaves {
		s.preFreeLeaves = append(s.preFreeLeaves, uint32(leaf))
	}
	slices.Sort(s.preFreeLeaves)
	// Canonical hashes (VACUUM INTO) provide rung R2 logical-to-byte equality
	// when a rolled-back file differs only inside reused freelist leaves.
	s.preCanon = canonicalHashOfFile(ctx, s.prePath)
	s.postCanon = canonicalHashOfFile(ctx, postPath)
	return s
}

// imageColumns are the db_file, db_pages, differ_pre and differ_post columns
// for a database file found on disk after a kill.
func (s *states) imageColumns(image []byte) []any {
	state := "torn"
	switch {
	case bytes.Equal(image, s.pre):
		state = "pre-step"
	case bytes.Equal(image, s.post):
		state = "post-step"
	}
	return []any{state, len(image) / pageSize, len(differingPages(image, s.pre)), len(differingPages(image, s.post))}
}

func (s *states) matchFile(path string) recipe.Match {
	h, err := recipe.HashFile(path)
	if err != nil {
		fatal("hash", err)
	}
	return recipe.MatchHashes(s.hashes, h)
}

type outcomeKind int

const (
	outcomePre outcomeKind = iota
	outcomePreFreelist
	outcomePost
	outcomeThird
	outcomeUnreadable
)

type outcome struct {
	kind     outcomeKind
	pages    []uint32
	match    recipe.Match
	canonPre bool
}

func (s *states) outcomeOf(path, canon string) outcome {
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read reopened", err)
	}
	o := outcome{match: s.matchFile(path), canonPre: canon != "" && canon == s.preCanon}
	switch {
	case canon == "":
		o.kind = outcomeUnreadable
	case bytes.Equal(image, s.pre):
		o.kind = outcomePre
	case bytes.Equal(image, s.post):
		o.kind = outcomePost
	default:
		o.pages = differingPages(image, s.pre)
		o.kind = outcomeThird
		// A rolled-back database whose file size and VACUUM INTO canonical hash
		// equal pre-step, and whose only differing pages are pre-step freelist
		// leaves, is logically identical to pre-step (PAGER_GET_NOCONTENT effect).
		if len(image) == len(s.pre) && o.canonPre && subset(o.pages, s.preFreeLeaves) {
			o.kind = outcomePreFreelist
		}
	}
	return o
}

func (o outcome) before() bool {
	return o.kind == outcomePre || o.kind == outcomePreFreelist
}

func (o outcome) letter() string {
	switch o.kind {
	case outcomePre:
		return "B"
	case outcomePreFreelist:
		return "b"
	case outcomePost:
		return "A"
	}
	return "?"
}

func (o outcome) name() string {
	switch o.kind {
	case outcomePre:
		return "pre-step exact"
	case outcomePreFreelist:
		return "pre-step except freelist leaves"
	case outcomePost:
		return "post-step exact"
	case outcomeUnreadable:
		return "unreadable"
	}
	return "third state"
}

// recipeMatch names the recipe state a third-state file equals, if any.
func (o outcome) recipeMatch() any {
	if o.kind != outcomeThird {
		return nil
	}
	return o.match.String()
}

func (o outcome) shortName() string {
	switch o.kind {
	case outcomePre:
		return "before (pre-step)"
	case outcomePreFreelist:
		return "before (pre-step, logical)"
	case outcomePost:
		return "after (post-step)"
	}
	return "neither"
}

func subset(pages, set []uint32) bool {
	for _, p := range pages {
		if !slices.Contains(set, p) {
			return false
		}
	}
	return true
}

func pageList(pages []uint32) string {
	parts := make([]string, len(pages))
	for i, p := range pages {
		parts[i] = strconv.Itoa(int(p))
	}
	return strings.Join(parts, " ")
}

func pageAt(image []byte, offset int) []byte {
	if offset >= len(image) {
		return nil
	}
	return image[offset:min(offset+pageSize, len(image))]
}

func differingPages(a, b []byte) []uint32 {
	var pages []uint32
	for i := 0; i < max(len(a), len(b)); i += pageSize {
		if !bytes.Equal(pageAt(a, i), pageAt(b, i)) {
			pages = append(pages, uint32(i/pageSize+1))
		}
	}
	return pages
}

func canonicalHashOfFile(ctx context.Context, path string) string {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open for VACUUM INTO", err)
	}
	defer db.Close()
	hash, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		fatal("VACUUM INTO", err)
	}
	return hash
}

func canonicalHash(ctx context.Context, db *sql.DB, path string) (string, error) {
	target := path + ".canonical"
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return "", err
	}
	defer os.Remove(target)
	return recipe.FileSHA256(target)
}

type op struct {
	call    trace.Syscall
	kind    string
	text    string
	ordinal int
}

func syncName(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

func openName(name string) bool {
	return name == "open" || name == "openat"
}

func unlinkName(name string) bool {
	return name == "unlink" || name == "unlinkat"
}

func killable(name string) bool {
	return name == "pwrite64" || name == "ftruncate" || syncName(name) || unlinkName(name)
}

func describe(call trace.Syscall) string {
	switch {
	case call.Name == "pwrite64" || call.Name == "pread64":
		text := fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
		if call.Result >= 0 && call.Result != call.Length && !call.Unfinished {
			text += fmt.Sprintf(" = %d", call.Result)
		}
		return text
	case syncName(call.Name):
		return "sync " + call.Path
	case unlinkName(call.Name):
		return "unlink " + call.Path
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case openName(call.Name):
		return "open " + call.Path
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, strings.SplitN(call.Flags, ",", 2)[0])
	}
	return call.Name + " " + call.Path
}

// classify maps each traced syscall in the commit window to its pager role:
// 512-byte header write (writeJournalHdr, src/pager.c:1449-1571), 12-byte
// magic+count update (syncJournal, src/pager.c:4377-4410), 28-byte header zero
// (zeroJournalHdr, src/pager.c:1399-1432), record writes, db page writes,
// syncs (including the directory sync on a newly created journal), and journal
// finalization.
func classify(call trace.Syscall) string {
	journal := dbName + "-journal"
	switch {
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0 && call.Length == 12:
		return "journal-count"
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0 && call.Length == 28:
		return "journal-zero-header"
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0:
		return "journal-header"
	case call.Name == "pwrite64" && call.Path == journal:
		return "journal-record"
	case call.Name == "pwrite64" && call.Path == dbName:
		return "db-page"
	case syncName(call.Name) && call.Path == journal:
		return "journal-sync"
	case syncName(call.Name) && call.Path == ".":
		return "dir-sync"
	case syncName(call.Name) && call.Path == dbName:
		return "db-sync"
	case unlinkName(call.Name) && call.Path == journal:
		return "journal-unlink"
	case call.Name == "ftruncate" && call.Path == journal:
		return "journal-truncate"
	case call.Name == "ftruncate" && call.Path == dbName:
		return "db-truncate"
	case openName(call.Name) && call.Path == markerEnd:
		return "step-returned"
	}
	return "other"
}

type window struct {
	tid     int
	ops     []op
	targets []op
}

// extractWindow isolates the writer thread's syscalls between marker-begin and
// marker-end while counting each syscall name's 1-based ordinal from process
// start, which strace's inject=...:when=ordinal flag requires.
func extractWindow(log *trace.StraceLog, dir string) (window, error) {
	w := window{tid: -1}
	beginPath := filepath.Join(dir, markerBegin)
	endPath := filepath.Join(dir, markerEnd)
	for _, call := range log.Calls {
		if openName(call.Name) && call.Path == beginPath {
			w.tid = call.PID
			break
		}
	}
	if w.tid < 0 {
		return w, errors.New("begin marker not found in the trace")
	}
	ordinals := map[string]int{}
	inside, ended := false, false
	for _, call := range log.Calls {
		if call.PID != w.tid || ended {
			continue
		}
		ordinals[call.Name]++
		if openName(call.Name) && call.Path == beginPath && !inside {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if openName(call.Name) && call.Path == endPath {
			relative := trace.RelativeTo(dir, []trace.Syscall{call})[0]
			w.targets = append(w.targets, op{call: relative, kind: classify(relative), text: describe(relative), ordinal: ordinals[call.Name]})
			ended = true
			continue
		}
		if !trace.UnderDir(dir)(call) || strings.HasPrefix(filepath.Base(call.Path), "marker-") {
			continue
		}
		relative := trace.RelativeTo(dir, []trace.Syscall{call})[0]
		o := op{call: relative, kind: classify(relative), text: describe(relative), ordinal: ordinals[call.Name]}
		w.ops = append(w.ops, o)
		if killable(call.Name) {
			w.targets = append(w.targets, o)
		}
	}
	if !ended {
		return w, errors.New("end marker not found in the trace")
	}
	return w, nil
}

func collapse(ops []op) string {
	var parts []string
	last, count := "", 0
	flush := func() {
		if last == "" {
			return
		}
		if count > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", last, count))
		} else {
			parts = append(parts, last)
		}
	}
	for _, o := range ops {
		if o.kind == "other" || o.kind == "step-returned" {
			continue
		}
		if o.kind == last {
			count++
			continue
		}
		flush()
		last, count = o.kind, 1
	}
	flush()
	return strings.Join(parts, ", ")
}

// recordTriplesWellFormed verifies that every journal-record write arrives as a
// (4, pageSize, 4) triple: page number, original page image, and checksum
// (pagerAddPageToRollbackJournal, src/pager.c:6062-6067).
func recordTriplesWellFormed(ops []op) bool {
	var lengths []int64
	for _, o := range ops {
		if o.kind == "journal-record" {
			lengths = append(lengths, int64(o.call.Length))
		}
	}
	if len(lengths)%3 != 0 {
		return false
	}
	for i := 0; i < len(lengths); i += 3 {
		if lengths[i] != 4 || lengths[i+1] != pageSize || lengths[i+2] != 4 {
			return false
		}
	}
	return true
}

func countKind(ops []op, kind string) int {
	n := 0
	for _, o := range ops {
		if o.kind == kind {
			n++
		}
	}
	return n
}

// expectedSequence builds the canonical commit syscall sequence for a given
// journal mode and synchronous level: FULL syncs the journal before and after
// writing magic+count (syncJournal, src/pager.c:4401-4420), NORMAL writes
// magic+count before its single journal sync, and OFF emits no syncs; TRUNCATE
// and PERSIST add a final journal sync at FULL (pager_end_transaction,
// src/pager.c:2086-2106).
func expectedSequence(journalMode, synchronous string, records, dbPages int) string {
	r := fmt.Sprintf("journal-record x%d", records)
	d := fmt.Sprintf("db-page x%d", dbPages)
	finish := map[string]string{
		sqlitex.JournalDelete:   "journal-unlink",
		sqlitex.JournalTruncate: "journal-truncate, journal-sync",
		sqlitex.JournalPersist:  "journal-zero-header, journal-sync",
	}[journalMode]
	switch synchronous {
	case sqlitex.SyncFull:
		return "journal-header, " + r + ", journal-sync, dir-sync, journal-count, journal-sync, " + d + ", db-sync, " + finish
	case sqlitex.SyncNormal:
		return "journal-header, " + r + ", journal-count, journal-sync, dir-sync, " + d + ", db-sync, " + finish
	}
	return "journal-header, " + r + ", " + d + ", " + finish
}

func syncTargets(ops []op) []string {
	var out []string
	for _, o := range ops {
		if syncName(o.call.Name) {
			out = append(out, o.call.Path)
		}
	}
	return out
}

func journalInfo(path string) (*journalfile.Journal, bool) {
	j, err := journalfile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		fatal("read journal", err)
	}
	return j, true
}

// journalRow returns the values of journalColumns for a -journal file, or
// NULLs when there is none.
func journalRow(j *journalfile.Journal, present bool) []any {
	row := make([]any, len(journalColumns))
	if !present {
		return row
	}
	row[0] = j.Size
	if j.Size == 0 {
		row[2] = false
		return row
	}
	row[1] = j.FirstByte
	row[2] = j.LooksHot()
	if len(j.Segments) == 0 {
		return row
	}
	h := j.Segments[0].Header
	row[3] = h.State.String()
	if h.State == journalfile.HeaderZeroed {
		return row
	}
	row[4] = h.RecordCount
	if h.State == journalfile.HeaderSynced || h.State == journalfile.HeaderNoSync || h.State == journalfile.HeaderNotYetSynced {
		row[5], row[6], row[7] = h.InitialPages, h.SectorSize, h.PageSize
	}
	row[8], row[9], row[10], row[11] = j.RecordsPresent(), pageList(recordPages(j)), j.ChecksumFailures(), j.TrailingByte
	return row
}

func writeJournal(part, label string, j *journalfile.Journal, present bool) {
	journalSeq++
	tables.journals.Row(append([]any{journalSeq, part, label}, journalRow(j, present)...)...)
}

func recordPages(j *journalfile.Journal) []uint32 {
	var pages []uint32
	for _, s := range j.Segments {
		for _, r := range s.Records {
			pages = append(pages, r.Page)
		}
	}
	return pages
}

// playbackRow returns the playback, playback_pages and truncate_to columns:
// what SQLite does with the -journal when it reopens the database.
func playbackRow(j *journalfile.Journal, present bool) []any {
	if !present {
		return []any{"no journal", nil, nil}
	}
	p := j.Playback()
	if !p.Hot {
		return []any{"ignored (not hot)", nil, nil}
	}
	if len(p.Pages) == 0 {
		return []any{"hot, restores no page: " + p.StopReason, nil, nil}
	}
	return []any{"rolled back", pageList(p.Pages), p.TruncateTo}
}

type killResult struct {
	target        op
	landed        string
	landedOK      bool
	dbBeforeImage []byte
	journal       *journalfile.Journal
	journalThere  bool
	integrity     string
	after         outcome
	journalAfter  string
	nonce         uint32
	duration      time.Duration
}

func killRun(ctx context.Context, dir string, s *states, journalMode, synchronous string, target op) *trace.Result {
	prepareDir(dir, s.prePath)
	command, err := trace.SelfCommand("child-step", dir, journalMode, synchronous, strconv.Itoa(s.step))
	if err != nil {
		fatal("self command", err)
	}
	run, err := trace.Run(ctx, trace.Options{Command: command, KillAt: &trace.KillAt{Syscalls: []string{target.call.Name}, When: target.ordinal}})
	if err != nil {
		fatal("strace kill run", err)
	}
	return run
}

// runKill runs one child process killed by SIGKILL on entry to target, inspects
// the resulting database and -journal files in plain Go before SQLite sees
// them, then reopens with sqlitex.Open to trigger sqlite3PagerSharedLock ->
// hasHotJournal -> pager_playback (src/pager.c:5300-5405) and classifies the
// recovered database.
func runKill(ctx context.Context, dir string, s *states, journalMode, synchronous string, target op) killResult {
	result := killResult{target: target}
	start := time.Now()
	run := killRun(ctx, dir, s, journalMode, synchronous, target)
	result.duration = time.Since(start)
	result.landed, result.landedOK = landedOn(run, dir, target)
	path := filepath.Join(dir, dbName)
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read killed db", err)
	}
	result.dbBeforeImage = image
	result.journal, result.journalThere = journalInfo(path + "-journal")
	if result.journalThere && len(result.journal.Segments) > 0 {
		result.nonce = result.journal.Segments[0].Header.Nonce
	}
	var canon string
	result.integrity, canon = reopen(ctx, path)
	result.after = s.outcomeOf(path, canon)
	after, there := journalInfo(path + "-journal")
	switch {
	case !there:
		result.journalAfter = "no -journal"
	case after.LooksHot():
		result.journalAfter = fmt.Sprintf("-journal still hot (%d bytes)", after.Size)
	default:
		result.journalAfter = fmt.Sprintf("cold -journal left (%d bytes, first byte 0x%02x)", after.Size, after.FirstByte)
	}
	return result
}

// writeKill writes one row of the kills table and its info_kills row.
// covered is nil for kills after the commit point, where it is not checked.
func writeKill(part string, s *states, journalMode, synchronous string, killNo int, r killResult, covered any) {
	var landed any
	if !r.landedOK {
		landed = r.landed
	}
	row := []any{part, s.step, journalMode, synchronous, killNo, r.target.text, r.target.kind, r.landedOK, landed}
	row = append(row, s.imageColumns(r.dbBeforeImage)...)
	row = append(row, journalRow(r.journal, r.journalThere)...)
	row = append(row, playbackRow(r.journal, r.journalThere)...)
	row = append(row, r.after.letter(), r.after.name(), pageList(r.after.pages), r.after.recipeMatch(), r.integrity, r.journalAfter, covered)
	tables.kills.Row(row...)
	var nonce any
	if r.journalThere && len(r.journal.Segments) > 0 {
		nonce = r.nonce
	}
	tables.infoKills.Row(part, s.step, journalMode, synchronous, killNo, nonce, r.duration.Milliseconds())
}

func reopen(ctx context.Context, path string) (string, string) {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		return "open failed: " + strings.ReplaceAll(err.Error(), path, filepath.Base(path)), ""
	}
	defer db.Close()
	integrity := integrityCheck(ctx, db.DB)
	canon, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		return integrity + "; VACUUM INTO failed: " + err.Error(), ""
	}
	return integrity, canon
}

func integrityCheck(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "integrity_check failed: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "integrity_check failed: " + err.Error()
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "integrity_check failed: " + err.Error()
	}
	return strings.Join(lines, " | ")
}

func runKills(ctx context.Context, base string, s *states, journalMode, synchronous string, targets []op) []killResult {
	results := make([]killResult, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(1, *workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = runKill(ctx, filepath.Join(base, fmt.Sprintf("kill-%02d", i+1)), s, journalMode, synchronous, targets[i])
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func baseline(ctx context.Context, dir string, s *states, journalMode, synchronous string) (window, []byte) {
	prepareDir(dir, s.prePath)
	command, err := trace.SelfCommand("child-step", dir, journalMode, synchronous, strconv.Itoa(s.step))
	if err != nil {
		fatal("self command", err)
	}
	run, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace baseline", err)
	}
	if run.ExitCode != 0 {
		fatal("baseline child", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
	}
	w, err := extractWindow(run.Log, dir)
	if err != nil {
		fatal("baseline window", err)
	}
	image, err := os.ReadFile(filepath.Join(dir, dbName))
	if err != nil {
		fatal("read baseline db", err)
	}
	return w, image
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	}
	fatal("literal", fmt.Errorf("unsupported argument type %T", v))
	return ""
}

func renderStatement(st recipe.Statement) string {
	var b strings.Builder
	arg := 0
	for _, r := range st.SQL {
		if r == '?' {
			b.WriteString(sqlLiteral(st.Args[arg]))
			arg++
			continue
		}
		b.WriteRune(r)
	}
	if arg != len(st.Args) {
		fatal("render", fmt.Errorf("%d placeholders for %d arguments in %q", arg, len(st.Args), st.SQL))
	}
	return b.String()
}

func cliScript(dir, synchronous string, step recipe.Step) string {
	o := stepOptions(sqlitex.JournalDelete, synchronous)
	var b strings.Builder
	fmt.Fprintf(&b, "PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=ON;\nPRAGMA secure_delete=ON;\nPRAGMA cache_size=%d;\nPRAGMA busy_timeout=%d;\n", sqlitex.JournalDelete, synchronous, o.Conn.CacheSize, o.Conn.BusyTimeout.Milliseconds())
	fmt.Fprintf(&b, ".read '%s'\nBEGIN IMMEDIATE;\n", filepath.Join(dir, markerBegin))
	for _, st := range step.Statements {
		b.WriteString(renderStatement(st))
		b.WriteString(";\n")
	}
	fmt.Fprintf(&b, "COMMIT;\n.read '%s'\n", filepath.Join(dir, markerEnd))
	return b.String()
}

// twinCheck replays the same recipe step as literal SQL through the pinned C
// sqlite3 CLI under strace and checks that its writes, syncs, and unlinks match
// the translated Go driver in file, offset, length, and order.
func twinCheck(ctx context.Context, cli sqlitex.CLI, dir string, s *states, synchronous string, goWindow window) {
	prepareDir(dir, s.prePath)
	command := cli.Command(ctx, sqlitex.CLIRun{}).Args
	command[0] = cli.Path
	path := filepath.Join(dir, dbName)
	run, err := trace.Run(ctx, trace.Options{Command: command, Dir: dir, Stdin: strings.NewReader(sqlitex.OpenScript(path, cliScript(dir, synchronous, s.stepStatements)))})
	if err != nil {
		fatal("strace CLI", err)
	}
	if run.ExitCode != 0 {
		fatal("CLI", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
	}
	cliWindow, err := extractWindow(run.Log, dir)
	if err != nil {
		fatal("CLI window", err)
	}
	goOps, cliOps := writesAndSyncs(goWindow.ops), writesAndSyncs(cliWindow.ops)
	same := slices.Equal(goOps, cliOps)
	for i := range max(len(goOps), len(cliOps)) {
		var g, c any
		if i < len(goOps) {
			g = goOps[i]
		}
		if i < len(cliOps) {
			c = cliOps[i]
		}
		if g != c {
			tables.twinDiffs.Row(synchronous, i+1, g, c)
		}
	}
	info(fmt.Sprintf("sync syscall, C CLI, step %d delete %s", s.step, synchronous), syncSyscallNames(cliWindow.ops))
	match := s.matchFile(path)
	postStep := match.Step == s.step && match.Exact
	tables.twins.Row(synchronous, len(goOps), len(cliOps), same, postStep)
	set.Check("D", same && postStep, fmt.Sprintf("synchronous=%s: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes", synchronous))
}

func writesAndSyncs(ops []op) []string {
	var out []string
	for _, o := range ops {
		if killable(o.call.Name) {
			out = append(out, o.text)
		}
	}
	return out
}

func syncSyscallNames(ops []op) string {
	var names []string
	for _, o := range ops {
		if syncName(o.call.Name) && !slices.Contains(names, o.call.Name) {
			names = append(names, o.call.Name)
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ",")
}

// checkBaseline writes the traced commit to the commits and commit_calls
// tables and checks its shape.
func checkBaseline(part string, w window, image []byte, s *states, journalMode, synchronous string) {
	sequence := collapse(w.ops)
	records, dbPages := countKind(w.ops, "journal-record"), countKind(w.ops, "db-page")
	syncs := syncTargets(w.ops)
	commitSeq++
	tables.commits.Row(commitSeq, part, s.step, journalMode, synchronous, sequence, records/3, records, dbPages, len(syncs), strings.Join(syncs, " "))
	for i, o := range w.ops {
		tables.commitCalls.Row(commitSeq, i+1, o.kind, o.text)
	}
	info(fmt.Sprintf("sync syscall, Go driver, step %d %s %s", s.step, journalMode, synchronous), syncSyscallNames(w.ops))
	label := fmt.Sprintf("step %d, journal_mode=%s, synchronous=%s", s.step, journalMode, synchronous)
	set.Check(part, sequence == expectedSequence(journalMode, synchronous, records, dbPages), label+": order is "+expectedSequence(journalMode, synchronous, records, dbPages))
	if journalMode == sqlitex.JournalDelete {
		wantSyncs := map[string]int{sqlitex.SyncFull: 4, sqlitex.SyncNormal: 3, sqlitex.SyncOff: 0}[synchronous]
		set.Check(part, len(syncs) == wantSyncs, fmt.Sprintf("%s: %d syncs per commit (journal, directory and database syncs counted)", label, wantSyncs))
	}
	set.Check(part, records > 0 && recordTriplesWellFormed(w.ops), label+": each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)")
	set.Check(part, bytes.Equal(image, s.post), label+": the traced commit leaves exactly the post-step bytes")
}

// runLevel traces one commit of s at one synchronous level, then kills it at
// every write, sync and unlink. The twin check against the C CLI runs in part D.
func runLevel(ctx context.Context, part, base string, s *states, synchronous string, cli sqlitex.CLI, haveCLI bool) []op {
	levelDir := filepath.Join(base, strings.ToLower(synchronous))
	w, image := baseline(ctx, filepath.Join(levelDir, "baseline"), s, sqlitex.JournalDelete, synchronous)
	checkBaseline(part, w, image, s, sqlitex.JournalDelete, synchronous)
	if haveCLI && part == "D" {
		twinCheck(ctx, cli, filepath.Join(levelDir, "twin"), s, synchronous, w)
	}

	started := time.Now()
	results := runKills(ctx, levelDir, s, sqlitex.JournalDelete, synchronous, w.targets)
	info(fmt.Sprintf("step %d %s: %d kill runs with %d workers, seconds", s.step, synchronous, len(results), max(1, *workers)), time.Since(started).Round(time.Millisecond).Seconds())
	unlinkIndex := -1
	for i, r := range results {
		if r.target.kind == "journal-unlink" {
			unlinkIndex = i
		}
	}
	allLanded, neverThird, integrityOK, coveredOK := true, true, true, true
	flipOK := unlinkIndex >= 0
	exactOnly := true
	for i, r := range results {
		if !r.landedOK {
			allLanded = false
		}
		if r.integrity != "ok" {
			integrityOK = false
		}
		switch r.after.kind {
		case outcomePre, outcomePost:
		case outcomePreFreelist:
			exactOnly = false
		default:
			neverThird, exactOnly = false, false
		}
		if (i <= unlinkIndex && !r.after.before()) || (i > unlinkIndex && r.after.kind != outcomePost) {
			flipOK = false
		}
		var isCovered any
		if i <= unlinkIndex {
			ok := covered(r, s)
			coveredOK = coveredOK && ok
			isCovered = ok
		}
		writeKill(part, s, sqlitex.JournalDelete, synchronous, i+1, r, isCovered)
	}
	label := fmt.Sprintf("step %d, synchronous=%s", s.step, synchronous)
	set.Check(part, allLanded, label+": every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)")
	set.Check(part, neverThird, label+": every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)")
	if len(s.preFreeLeaves) == 0 {
		set.Check(part, exactOnly, label+": the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly")
	}
	set.Check(part, flipOK, label+": every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)")
	set.Check(part, integrityOK, label+": integrity_check is ok after every reopen")
	set.Check(part, coveredOK, label+": for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves")
	return w.targets
}

// covered verifies that whenever a pre-commit crash leaves modified bytes in
// the database file, the journal is hot and accounts for every changed page:
// appended pages (> prePages) are removed by truncation (src/pager.c:2952),
// existing modified pages are listed in Playback().Pages, and reused freelist
// leaves were fetched with PAGER_GET_NOCONTENT (src/pager.c:5645-5656).
func covered(r killResult, s *states) bool {
	changed := differingPages(r.dbBeforeImage, s.pre)
	if len(changed) == 0 {
		return true
	}
	if !r.journalThere {
		return false
	}
	p := r.journal.Playback()
	if !p.Hot {
		return false
	}
	for _, page := range changed {
		switch {
		case int(page) > s.prePages:
			if p.TruncateTo != uint32(s.prePages) {
				return false
			}
		case slices.Contains(p.Pages, page), slices.Contains(s.preFreeLeaves, page):
		default:
			return false
		}
	}
	return true
}

func journalOpsIn(calls []trace.Syscall) []string {
	var ops []string
	for _, call := range calls {
		if !strings.HasSuffix(call.Path, "-journal") {
			continue
		}
		if openName(call.Name) || unlinkName(call.Name) || syncName(call.Name) || call.Name == "pwrite64" {
			ops = append(ops, describe(call))
		}
	}
	return ops
}

func walWritesIn(calls []trace.Syscall) int {
	n := 0
	for _, call := range calls {
		if strings.HasSuffix(call.Path, "-wal") && call.Name == "pwrite64" {
			n++
		}
	}
	return n
}

func segmentsByMarkers(log *trace.StraceLog, dir, prefix string, count int) [][]trace.Syscall {
	segments := make([][]trace.Syscall, count)
	current := -1
	for _, call := range log.Calls {
		if openName(call.Name) && strings.HasPrefix(call.Path, filepath.Join(dir, prefix)) {
			n, err := strconv.Atoi(strings.TrimPrefix(call.Path, filepath.Join(dir, prefix)))
			if err == nil {
				current = n - 1
			}
			continue
		}
		if current >= 0 && current < count && trace.UnderDir(dir)(call) {
			segments[current] = append(segments[current], trace.RelativeTo(dir, []trace.Syscall{call})[0])
		}
	}
	return segments
}

// partA shows where a WAL-default platform still meets rollback mode: new
// databases default to journal_mode=delete (header bytes 18-19 = 1,1), and
// switching a new file to WAL—including inside sqlitex.Open—writes page 1
// through a rollback -journal before WAL mode takes effect.
func partA(ctx context.Context, base string) {
	dir := filepath.Join(base, "a")
	mkdir(dir)
	path := filepath.Join(dir, "plain.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fatal("open plain", err)
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		fatal("journal_mode", err)
	}
	tables.headerModes.Row(1, "new file, driver defaults, before any write", mode, nil, nil)
	set.Check("A", mode == sqlitex.JournalDelete, "a new database starts in rollback-journal mode (journal_mode=delete) until something switches it")
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		fatal("create", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	header := readHeaderBytes(path)
	tables.headerModes.Row(2, "after its first CREATE TABLE", nil, header[18], header[19])
	set.Check("A", header[18] == 1 && header[19] == 1, "the file records rollback mode in header bytes 18 and 19 (value 1)")
	db, err = sql.Open("sqlite", path)
	if err != nil {
		fatal("reopen plain", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=wal").Scan(&mode); err != nil {
		fatal("switch", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	header = readHeaderBytes(path)
	db, err = sql.Open("sqlite", path)
	if err != nil {
		fatal("reopen plain", err)
	}
	var again string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&again); err != nil {
		fatal("journal_mode", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	tables.headerModes.Row(3, "after PRAGMA journal_mode=wal, read by a fresh connection", again, header[18], header[19])
	set.Check("A", header[18] == 2 && header[19] == 2 && again == sqlitex.JournalWAL, "switching to WAL rewrites bytes 18 and 19 to 2, and the mode persists in the file")

	if !trace.Supported {
		return
	}
	cases := []struct {
		role   string
		labels []string
	}{
		{"plain", []string{"CREATE TABLE (driver defaults, first write)"}},
		{"switch", []string{"PRAGMA journal_mode=wal on the new empty file", "CREATE TABLE after the switch"}},
		{"platform", []string{"sqlitex.Open of a new file with journal_mode=wal", "CREATE TABLE after it"}},
	}
	journalBySegment := map[string][][]string{}
	walBySegment := map[string][]int{}
	for _, c := range cases {
		target := filepath.Join(dir, c.role+".db")
		if err := recipe.RemoveDatabase(target); err != nil {
			fatal("remove", err)
		}
		for i := 1; i <= len(c.labels)+1; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-marker-%d", c.role, i)), nil, 0o644); err != nil {
				fatal("marker", err)
			}
		}
		command, err := trace.SelfCommand("child-create", c.role, target)
		if err != nil {
			fatal("self command", err)
		}
		run, err := trace.Run(ctx, trace.Options{Command: command})
		if err != nil {
			fatal("strace create", err)
		}
		if run.ExitCode != 0 {
			fatal("create child", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
		}
		segments := segmentsByMarkers(run.Log, dir, c.role+"-marker-", len(c.labels))
		for i, label := range c.labels {
			ops := journalOpsIn(segments[i])
			wal := walWritesIn(segments[i])
			journalBySegment[c.role] = append(journalBySegment[c.role], ops)
			walBySegment[c.role] = append(walBySegment[c.role], wal)
			creationSeq++
			tables.creations.Row(creationSeq, c.role, label, len(ops), wal)
			for n, o := range ops {
				tables.creationCalls.Row(creationSeq, n+1, o)
			}
		}
	}
	usesJournal := func(ops []string) bool {
		hasPrefix := func(prefix string) bool {
			return slices.ContainsFunc(ops, func(o string) bool { return strings.HasPrefix(o, prefix) })
		}
		return len(ops) >= 2 && strings.HasPrefix(ops[0], "open ") && strings.HasPrefix(ops[len(ops)-1], "unlink ") && hasPrefix("pwrite64 ") && hasPrefix("sync ")
	}
	set.Check("A", usesJournal(journalBySegment["plain"][0]), "a file created with driver defaults commits its first transaction through a -journal (opened, written, synced, unlinked)")
	set.Check("A", usesJournal(journalBySegment["switch"][0]) && walBySegment["switch"][0] == 0, "the switch to WAL is itself a rollback-mode commit: it writes page 1 through a -journal")
	set.Check("A", len(journalBySegment["switch"][1]) == 0 && walBySegment["switch"][1] > 0, "after the switch, commits go to the -wal and no -journal is opened")
	set.Check("A", usesJournal(journalBySegment["platform"][0]) && len(journalBySegment["platform"][1]) == 0, "a WAL database created by the platform opener (sqlitex.Open) is also born through one -journal commit, then never uses one")
}

func readHeaderBytes(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read header", err)
	}
	if len(data) < 100 {
		fatal("read header", fmt.Errorf("file has %d bytes", len(data)))
	}
	return data[:100]
}

func childCreate(role, path string) {
	ctx := context.Background()
	dir := filepath.Dir(path)
	mark := func(i int) { touch(dir, fmt.Sprintf("%s-marker-%d", role, i)) }
	exec := func(db *sql.DB, statement string) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
	switch role {
	case "plain", "switch":
		db, err := sql.Open("sqlite", path)
		if err != nil {
			fatal("plain open", err)
		}
		db.SetMaxOpenConns(1)
		mark(1)
		if role == "switch" {
			exec(db, "PRAGMA journal_mode=wal")
			mark(2)
			exec(db, "CREATE TABLE t(x)")
			mark(3)
		} else {
			exec(db, "CREATE TABLE t(x)")
			mark(2)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	case "platform":
		mark(1)
		db, err := sqlitex.Open(ctx, path, sqlitex.DefaultOptions())
		if err != nil {
			fatal("platform open", err)
		}
		mark(2)
		exec(db.DB, "CREATE TABLE t(x)")
		mark(3)
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	default:
		fatal("child-create", fmt.Errorf("unknown role %q", role))
	}
}

// partC inspects the rollback journal of an open, uncommitted transaction at
// synchronous=FULL using internal/journalfile. Reading -journal while the
// connection is open is safe because SQLite locks only the main database file.
func partC(ctx context.Context, base string, s *states) {
	dir := filepath.Join(base, "c")
	prepareDir(dir, s.prePath)
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	if err := s.stepStatements.ExecIn(ctx, tx); err != nil {
		fatal("exec", err)
	}
	j, present := journalInfo(filepath.Join(dir, dbName+"-journal"))
	if !present {
		fatal("journal", errors.New("no -journal during the transaction"))
	}
	writeJournal("C", fmt.Sprintf("step %d executed, COMMIT not yet issued", s.step), j, present)
	allOriginal := j.RecordsPresent() > 0
	seq := 0
	for _, segment := range j.Segments {
		for _, r := range segment.Records {
			same := bytes.Equal(pageAt(s.pre, int(r.Page-1)*pageSize), r.Image)
			allOriginal = allOriginal && same && r.ChecksumOK()
			role := "b-tree or overflow page"
			if r.Page == 1 {
				role = "header and schema root"
			}
			checksum := "ok"
			if !r.ChecksumOK() {
				checksum = "BAD"
			}
			seq++
			tables.journalRecords.Row(seq, r.Offset, r.Page, role, checksum, same)
		}
	}
	set.Check("C", allOriginal, "every journal record holds the page's original (pre-step) image with a valid checksum")
	set.Check("C", j.Segments[0].Header.State == journalfile.HeaderNotYetSynced && !j.LooksHot(), "before COMMIT (synchronous=FULL) the header's magic and record count are still zero, so the journal is not hot yet")
	set.Check("C", j.Segments[0].Header.InitialPages == uint32(s.prePages), fmt.Sprintf("the header records the database size before the transaction (%d pages)", s.prePages))
	if err := tx.Rollback(); err != nil {
		fatal("rollback", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	match := s.matchFile(filepath.Join(dir, dbName))
	atRest := recipe.CheckAtRest(filepath.Join(dir, dbName)) == nil
	tables.liveRollback.Row(map[bool]string{true: "pre-step exact bytes", false: "NOT the pre-step bytes"}[match.Step == s.preStep && match.Exact], !atRest)
	set.Check("C", match.Step == s.preStep && match.Exact, "a ROLLBACK by the live connection restores the pre-step bytes exactly (the reused freelist pages were never written to the file)")
	set.Check("C", atRest, "no -journal remains after ROLLBACK and close")
}

// reopenRow opens path, runs integrity_check and counts the rows, and writes
// one row of the reopens table. It returns the row's report columns, so two
// reopens can be compared, and the outcome.
func reopenRow(ctx context.Context, scenario, path string, s *states, classify bool) (string, outcome) {
	var report []any
	var o outcome
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		report = []any{"open failed: " + strings.ReplaceAll(err.Error(), path, filepath.Base(path)), nil, nil, nil, nil, nil}
		if classify {
			o = s.outcomeOf(path, "")
		}
	} else {
		report = append([]any{nil, integrityCheck(ctx, db.DB)}, tableCounts(ctx, db.DB)...)
		canon := ""
		if classify {
			if canon, err = canonicalHash(ctx, db.DB, path); err != nil {
				canon = ""
			}
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		if classify {
			o = s.outcomeOf(path, canon)
		}
	}
	var name, pages any
	if classify {
		name, pages = o.name(), pageList(o.pages)
	}
	reopenSeq++
	tables.reopens.Row(append(append([]any{reopenSeq, scenario}, report...), name, pages)...)
	return fmt.Sprint(report...), o
}

// tableCounts returns user_version and the row counts of the recipe tables;
// a value that cannot be read is replaced by its error.
func tableCounts(ctx context.Context, db *sql.DB) []any {
	var values []any
	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		values = append(values, "error: "+err.Error())
	} else {
		values = append(values, userVersion)
	}
	for _, table := range []string{"accounts", "documents", "events"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			values = append(values, "error: "+err.Error())
			continue
		}
		values = append(values, n)
	}
	return values
}

func recreateKilled(ctx context.Context, dir string, s *states, target op) {
	run := killRun(ctx, dir, s, sqlitex.JournalDelete, sqlitex.SyncFull, target)
	if landed, ok := landedOn(run, dir, target); !ok {
		fatal("kill", fmt.Errorf("child was not killed on %q (ordinal %d): killed=%q exit %d landed %q", target.text, target.ordinal, run.Killed, run.ExitCode, landed))
	}
}

func writeCrash(crash string, target op, writeNo any, writes int, s *states, dir string) {
	image, err := os.ReadFile(filepath.Join(dir, dbName))
	if err != nil {
		fatal("read", err)
	}
	crashSeq++
	tables.crashes.Row(append([]any{crashSeq, crash, target.text, writeNo, writes}, s.imageColumns(image)...)...)
}

// landedOn confirms strace killed the child with SIGKILL on entry to the exact
// intended syscall by matching both formatted syscall text and per-PID ordinal
// (strace counts --inject when= per thread). Tracking ordinals per PID also
// handles logs where a killed pwrite64 appears unfinished on a second thread.
func landedOn(run *trace.Result, dir string, target op) (string, bool) {
	landed, onTarget := "", false
	ordinals := map[int]int{}
	for _, call := range run.Log.Calls {
		if call.Name != target.call.Name {
			continue
		}
		ordinals[call.PID]++
		if !call.Unfinished || !trace.UnderDir(dir)(call) {
			continue
		}
		text := describe(trace.RelativeTo(dir, []trace.Syscall{call})[0])
		if text == target.text && ordinals[call.PID] == target.ordinal {
			landed, onTarget = text, true
		} else if !onTarget {
			landed = text
		}
	}
	return landed, run.Killed == "SIGKILL" && onTarget
}

// partE demonstrates three failure cases of losing a hot journal: deleting it
// mid-write, copying the database file alone versus together with -journal, and
// deleting it after all database writes completed (silently committing a
// transaction whose COMMIT never returned).
func partE(ctx context.Context, base string, s *states, fullTargets []op) {
	reopenRow(ctx, fmt.Sprintf("pre-step file (step %d)", s.preStep), s.prePath, s, false)
	reopenRow(ctx, fmt.Sprintf("post-step file (step %d)", s.step), filepath.Join(filepath.Dir(s.prePath), "post.db"), s, false)
	var dbWrites []op
	var dbSync op
	for _, t := range fullTargets {
		switch t.kind {
		case "db-page":
			dbWrites = append(dbWrites, t)
		case "db-sync":
			dbSync = t
		}
	}
	if len(dbWrites) < 2 || dbSync.kind == "" {
		fatal("failure cases", errors.New("the synchronous=FULL commit has too few database writes"))
	}
	middle := dbWrites[len(dbWrites)/2]

	deleted := filepath.Join(base, "e-deleted")
	recreateKilled(ctx, deleted, s, middle)
	writeCrash("mid-write", middle, len(dbWrites)/2+1, len(dbWrites), s, deleted)
	j, present := journalInfo(filepath.Join(deleted, dbName+"-journal"))
	writeJournal("E", "mid-write crash", j, present)

	copied := filepath.Join(base, "e-copied")
	original := filepath.Join(base, "e-with-journal")
	mkdir(copied)
	mkdir(original)
	copyFile(filepath.Join(deleted, dbName), filepath.Join(copied, dbName))
	copyFile(filepath.Join(deleted, dbName), filepath.Join(original, dbName))
	copyFile(filepath.Join(deleted, dbName+"-journal"), filepath.Join(original, dbName+"-journal"))

	if err := os.Remove(filepath.Join(deleted, dbName+"-journal")); err != nil {
		fatal("delete journal", err)
	}
	report, deletedOutcome := reopenRow(ctx, "1. -journal deleted", filepath.Join(deleted, dbName), s, true)
	set.Check("E", deletedOutcome.kind == outcomeUnreadable || deletedOutcome.kind == outcomeThird, "deleting the hot journal before reopening leaves a damaged database that is neither the pre-step nor the post-step state")

	copyReport, copyOutcome := reopenRow(ctx, "2. copied without -journal", filepath.Join(copied, dbName), s, true)
	set.Check("E", copyOutcome.kind == deletedOutcome.kind && copyReport == report && !copyOutcome.before(), "a copy made without the -journal is the same damaged database")
	_, originalOutcome := reopenRow(ctx, "2. copied with -journal", filepath.Join(original, dbName), s, true)
	set.Check("E", originalOutcome.before(), "the same crashed file copied together with its -journal rolls back to pre-step")

	late := filepath.Join(base, "e-late")
	recreateKilled(ctx, late, s, dbSync)
	writeCrash("before db sync", dbSync, nil, len(dbWrites), s, late)
	if err := os.Remove(filepath.Join(late, dbName+"-journal")); err != nil {
		fatal("delete journal", err)
	}
	_, lateOutcome := reopenRow(ctx, "3. late crash, -journal deleted", filepath.Join(late, dbName), s, true)
	set.Check("E", lateOutcome.kind == outcomePost, "deleting the journal after the last database write turns a commit that never returned into a committed one (post-step)")
}

// partF locates the exact commit-point syscall for DELETE (unlink), TRUNCATE
// (ftruncate to 0), and PERSIST (28-byte header zero write) by killing on entry
// to that syscall and on the syscall immediately after it
// (pager_end_transaction, src/pager.c:2086-2122).
func partF(ctx context.Context, base string, s *states) {
	for seq, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalTruncate, sqlitex.JournalPersist} {
		dir := filepath.Join(base, "f-"+mode)
		w, image := baseline(ctx, filepath.Join(dir, "baseline"), s, mode, sqlitex.SyncFull)
		checkBaseline("F", w, image, s, mode, sqlitex.SyncFull)
		left, there := journalInfo(filepath.Join(dir, "baseline", dbName+"-journal"))
		writeJournal("F", mode, left, there)
		commitKind := map[string]string{sqlitex.JournalDelete: "journal-unlink", sqlitex.JournalTruncate: "journal-truncate", sqlitex.JournalPersist: "journal-zero-header"}[mode]
		index := -1
		for i, t := range w.targets {
			if t.kind == commitKind && i+1 < len(w.targets) {
				index = i
				break
			}
		}
		if index < 0 {
			set.Check("F", false, mode+": commit point found in the trace")
			continue
		}
		commitPoint, next := w.targets[index], w.targets[index+1]
		at := runKill(ctx, filepath.Join(dir, "at"), s, mode, sqlitex.SyncFull, commitPoint)
		after := runKill(ctx, filepath.Join(dir, "after"), s, mode, sqlitex.SyncFull, next)
		writeKill("F", s, mode, sqlitex.SyncFull, index+1, at, nil)
		writeKill("F", s, mode, sqlitex.SyncFull, index+2, after, nil)
		tables.commitPoints.Row(seq+1, mode, commitPoint.text, at.after.shortName(), next.kind, after.after.shortName())
		set.Check("F", at.landedOK && after.landedOK && at.after.before() && after.after.kind == outcomePost, mode+": a kill on entry to the commit point gives pre-step, the next kill gives post-step")
		if mode != sqlitex.JournalDelete {
			set.Check("F", there && !left.LooksHot(), mode+": the journal file stays beside the database after a clean commit, and it is not hot")
		}
	}
}

func portableLevels(ctx context.Context, base string, s *states) {
	for _, level := range []string{sqlitex.SyncFull, sqlitex.SyncNormal, sqlitex.SyncOff} {
		dir := filepath.Join(base, "portable-"+strings.ToLower(level))
		prepareDir(dir, s.prePath)
		db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(sqlitex.JournalDelete, level))
		if err != nil {
			fatal("open", err)
		}
		if err := recipe.ApplyStep(ctx, db, s.stepStatements); err != nil {
			fatal("apply", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		match := s.matchFile(filepath.Join(dir, dbName))
		set.Check("D", match.Step == s.step && match.Exact, fmt.Sprintf("step %d, synchronous=%s: the step committed in process leaves the post-step bytes", s.step, level))
	}
}

func describeStates(part string, r recipe.Recipe, s *states) {
	c := s.stepStatements.Counts()
	tables.steps.Row(s.step, s.preStep, r.String(), c.Schema, c.Inserts, c.Updates, c.Deletes, c.Indexes)
	tables.states.Row(s.step, "pre-step", s.preStep, s.prePages, s.hashes[s.preStep].Exact[:16], pageList(s.preFreeLeaves))
	tables.states.Row(s.step, "post-step", s.step, s.postPages, s.hashes[s.step].Exact[:16], nil)
	preMatch, postMatch := s.matchFile(s.prePath), s.matchFile(filepath.Join(filepath.Dir(s.prePath), "post.db"))
	set.Check(part, preMatch.Step == s.preStep && preMatch.Exact && postMatch.Step == s.step && postMatch.Exact, fmt.Sprintf("the pre-step and post-step files equal the recipe's hashes for steps %d and %d", s.preStep, s.step))
}

func writeParts() {
	status := "ran"
	if !trace.Supported {
		status = "skipped: needs strace (Linux)"
	}
	tables.parts.Row("A", "journal_mode and header bytes 18-19 of a new file", "ran")
	tables.parts.Row("A", "syscalls of the first commits of a new file", status)
	tables.parts.Row("B", "the recipe step: pre-step and post-step files", "ran")
	tables.parts.Row("C", "the journal of an open transaction", "ran")
	if !trace.Supported {
		tables.parts.Row("D", "the step committed in process at each synchronous level", "ran")
	}
	tables.parts.Row("D", "the commit traced and killed at every write and sync", status)
	if *exactFlag != 0 {
		tables.parts.Row("D2", "the same for a step whose pre-step file has no freelist", status)
	}
	tables.parts.Row("E", "losing the hot journal", status)
	tables.parts.Row("F", "the commit point in DELETE, TRUNCATE and PERSIST modes", status)
}

func main() {
	if len(os.Args) == 6 && os.Args[1] == "child-step" {
		k, err := strconv.Atoi(os.Args[5])
		if err != nil {
			fatal("child step", err)
		}
		childStep(os.Args[2], os.Args[3], os.Args[4], k)
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "child-create" {
		childCreate(os.Args[2], os.Args[3])
		return
	}
	flag.Parse()
	ctx := context.Background()
	started := time.Now()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("4.1")
	base, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(base))
	set = results.MustOpen(base)
	openTables()
	writeParts()
	var cli sqlitex.CLI
	haveCLI := false
	if trace.Supported {
		located, cliErr := sqlitex.LocateCLI(ctx)
		cli, haveCLI = located, cliErr == nil
		if haveCLI {
			fmt.Println(cli.PinLine())
		} else {
			fmt.Printf("sqlite3 CLI: unavailable: %v\n", cliErr)
			set.Check("D", false, "the pinned sqlite3 CLI is available")
		}
	}

	partA(ctx, base)

	r := rollbackRecipe()
	for _, k := range []int{*stepFlag, *exactFlag} {
		if k != 0 && (k < 1 || k > r.Steps) {
			fatal("step", fmt.Errorf("steps must be 1..%d, got %d", r.Steps, k))
		}
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	main := newStates(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *stepFlag)), r, *stepFlag, hashes)
	describeStates("B", r, main)

	partC(ctx, base, main)

	if !trace.Supported {
		trace.PrintLinuxOnly("the strace of the commit, the kill at every write and sync, the failure cases and the commit-point table", packagePath)
		portableLevels(ctx, base, main)
		finish(started)
		return
	}

	levels := []string{sqlitex.SyncFull, sqlitex.SyncNormal, sqlitex.SyncOff}
	var fullTargets []op
	for _, level := range levels {
		targets := runLevel(ctx, "D", filepath.Join(base, fmt.Sprintf("step-%d", *stepFlag)), main, level, cli, haveCLI)
		if level == sqlitex.SyncFull {
			fullTargets = targets
		}
	}

	if *exactFlag != 0 {
		exact := newStates(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *exactFlag)), r, *exactFlag, hashes)
		describeStates("D2", r, exact)
		for _, level := range levels {
			runLevel(ctx, "D2", filepath.Join(base, fmt.Sprintf("step-%d", *exactFlag)), exact, level, cli, false)
		}
	}

	partE(ctx, base, main, fullTargets)
	partF(ctx, base, main)
	finish(started)
}

func finish(started time.Time) {
	info("runtime, seconds", time.Since(started).Round(100*time.Millisecond).Seconds())
	set.Finish("ATOMIC COMMIT")
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

<!-- include: experiments/4.1-atomic-commit-and-hot-journal/report.sql -->
```sql
-- Walkthrough of experiment 4.1. Run it with:
--   go run ./cmd/course run 4.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   parts            what ran and what was skipped (parts B to F need strace, so Linux)
--   header_modes     part A: journal_mode and header bytes 18-19 of a new file, by stage
--   creations        part A: one traced statement of a new file, with its -journal and -wal writes
--   creation_calls   part A: one -journal syscall of such a statement
--   steps            the recipe steps whose commit is traced (3, and 1 for part D2)
--   states           the pre-step and post-step file of each traced step
--   journals         a -journal decoded in Go: part C (open transaction), E (crash), F (after commit)
--   journal_records  part C: one page record of the open transaction's -journal
--   live_rollback    part C: the file after ROLLBACK and close
--   commits          one traced commit: its syscall sequence, page writes and syncs
--   commit_calls     one syscall of a traced commit, on the database directory
--   twins            part D: the C CLI's writes and syncs against the Go driver's
--   twin_diffs       part D: a position where they differ (empty when they agree)
--   kills            one child killed on entry to one syscall: the files it left, and the reopen
--   crashes          part E: the two crashes whose -journal is then lost
--   reopens          part E: one database opened after a crash, or the pre/post-step file
--   commit_points    part F: the commit-point syscall of each journal mode
--   checks           every claim the experiment asserted (the runner lists it last)
-- Run-dependent values (sync syscall names, timings, journal nonces) are in
-- info and info_kills, shown by info.sql.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  4.1  Atomic commit in rollback-journal mode, and the hot journal'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A rollback-journal commit of recipe step 3 (seed 1) is traced with strace,'
.print 'then rerun and killed with SIGKILL on entry to each of its writes, syncs'
.print 'and unlinks, at synchronous=FULL, NORMAL and OFF. After each kill Go reads'
.print 'the database and -journal as bytes, then SQLite reopens the database.'
.print
.print 'What ran:'

SELECT part, what, status FROM parts ORDER BY part, what;

.print
.print '── A. Where a WAL-default platform meets rollback mode ─────────────────'
.print
.print 'A1. A new file starts in rollback mode (header bytes 18 and 19 = 1) until'
.print '    PRAGMA journal_mode=wal rewrites them to 2.'

SELECT stage, journal_mode, byte_18, byte_19 FROM header_modes ORDER BY seq;

.print
.print 'A2. Each statement traced in a fresh process. The switch to WAL, and the'
.print '    first commit of a WAL file opened with sqlitex.Open, still go through'
.print '    a -journal; the statements after them write only to the -wal.'

SELECT c.opener, c.statement,
       string_agg(split_part(k.call, ' ', 1), ' ' ORDER BY k.n) AS journal_syscalls,
       c.wal_writes
FROM creations c LEFT JOIN creation_calls k ON k.creation_seq = c.seq
GROUP BY c.seq, c.opener, c.statement, c.wal_writes
ORDER BY c.seq;

.print
.print 'A3. The -journal syscalls of each of those rollback-mode commits:'

SELECT c.statement, k.n, k.call
FROM creations c JOIN creation_calls k ON k.creation_seq = c.seq
ORDER BY c.seq, k.n;

.print
.print '── B. The recipe step ──────────────────────────────────────────────────'
.print
.print 'B1. Step 3 of the recipe, the transaction this walkthrough commits:'

SELECT step, recipe, schema, inserts, updates, deletes, indexes FROM steps WHERE step = 3;

.print
.print 'B2. The file before (after recipe step 2) and after it. Page 22 is a'
.print '    freelist leaf in the pre-step file; remember it for part D.'

SELECT state, recipe_step, pages, sha256_prefix, freelist_leaves
FROM states WHERE step = 3 ORDER BY state DESC;

.print
.print '── C. The journal of an open transaction, read before COMMIT ──────────'
.print
.print 'C1. Every statement of step 3 has run; COMMIT is not issued. The header'
.print '    is written but magic and record count are still zero, so the journal'
.print '    is not hot. It records the pre-transaction size: 29 pages.'

SELECT journal_bytes AS bytes, printf('0x%02x', first_byte) AS first_byte, hot, header,
       record_count AS count, initial_pages AS init_pages, sector_size AS sector, page_size AS page, records,
       checksum_failures AS bad_sums, trailing_bytes AS partial
FROM journals WHERE part = 'C';

.print
.print 'C2. Each record is a page number, the original image of the page and a checksum.'
.print '    Page 22, the reused freelist leaf, is not among them: SQLite fetches it'
.print '    with PAGER_GET_NOCONTENT and never journals it.'

SELECT file_offset, page, role, checksum, same_as_pre_step FROM journal_records ORDER BY seq;

.print
.print 'C3. ROLLBACK by the live connection, then close:'

SELECT file_after, journal_left FROM live_rollback;

.print
.print '── D. Step 3: one commit traced, then killed at every write and sync ──'
.print
.print 'D1. The synchronous=FULL commit: syscalls of the writer thread on the database'
.print '    directory, between the start of the step and its return. The journal'
.print '    records come first, then two journal syncs around the record count,'
.print '    then the database pages, one database sync, and the unlink.'

SELECT n, kind, call
FROM commit_calls
WHERE commit_seq = (SELECT seq FROM commits WHERE part = 'D' AND synchronous = 'FULL')
ORDER BY n;

.print
.print 'D2. The same commit at NORMAL and OFF is identical up to the last journal'
.print '    record. From there on: NORMAL writes the record count before a single'
.print '    journal sync; OFF syncs nothing.'

SELECT c.synchronous, k.n, k.kind, k.call
FROM commits c JOIN commit_calls k ON k.commit_seq = c.seq
WHERE c.part = 'D' AND c.synchronous IN ('NORMAL', 'OFF')
  AND k.n > (SELECT max(n) FROM commit_calls WHERE commit_seq = c.seq AND kind = 'journal-record')
ORDER BY c.seq, k.n;

.print
.print 'D3. The three sequences side by side (other syscalls left out, repeats folded):'

WITH s AS (
  SELECT synchronous, unnest(string_split(sequence, ', ')) AS what,
         generate_subscripts(string_split(sequence, ', '), 1) AS i
  FROM commits WHERE part = 'D')
SELECT i,
       max(what) FILTER (synchronous = 'FULL')   AS "FULL",
       max(what) FILTER (synchronous = 'NORMAL') AS "NORMAL",
       max(what) FILTER (synchronous = 'OFF')    AS "OFF"
FROM s GROUP BY i ORDER BY i;

SELECT synchronous, journaled_pages AS pages, record_writes, db_page_writes AS db_writes, syncs, sync_targets
FROM commits WHERE part = 'D' ORDER BY seq;

.print
.print 'D4. The pinned C sqlite3 CLI runs the same step as literal SQL. Its writes,'
.print '    syncs and unlink equal those of the Go driver in file, offset, length, order.'

SELECT synchronous, go_calls, cli_calls, identical, cli_leaves_post_step
FROM twins ORDER BY go_calls DESC;

.print
.print 'D5. Kill points: every pwrite64, sync and unlink of the commit, then the'
.print '    first syscall after the step returned.'

SELECT synchronous, count(*) AS kill_points
FROM kills WHERE part = 'D' GROUP BY synchronous ORDER BY kill_points DESC;

.print
.print 'D6. synchronous=FULL: what each kill left on disk. Until the record count is'
.print '    written and synced (kill 26) the journal is not hot and the database'
.print '    file is untouched. From kill 28 on, the database is torn mid-write.'
.print '    chg_pre and chg_post count the pages of the database file that differ'
.print '    from the pre-step and post-step files.'

SELECT kill_no AS kill, syscall, differ_pre AS chg_pre, differ_post AS chg_post,
       journal_bytes AS bytes, hot, header, records AS recs, trailing_bytes AS partial
FROM kills WHERE part = 'D' AND synchronous = 'FULL' ORDER BY kill_no;

.print
.print 'D7. synchronous=FULL: what SQLite did on reopen. A hot journal is played'
.print '    back and the file truncated to its 29 original pages; the result is the'
.print '    pre-step state (B), or pre-step except page 22 (b). After the unlink,'
.print '    the reopened file is the post-step state (A).'

SELECT kill_no AS kill, playback, playback_pages, truncate_to AS trunc,
       outcome, outcome_pages AS differs_in, integrity,
       regexp_replace(journal_after, 'cold -journal left \((\d+) bytes, first byte (0x..)\)', 'cold, \1 bytes, \2') AS journal_after
FROM kills WHERE part = 'D' AND synchronous = 'FULL' ORDER BY kill_no;

.print
.print 'b means the reopened file equals pre-step except in reused freelist leaves'
.print '(page 22): the rollback cannot restore their unused bytes because they were'
.print 'never journaled, and VACUUM INTO of the file equals that of the pre-step file.'
.print
.print 'D8. synchronous=NORMAL, every kill: the record count is written before the'
.print '    single journal sync, so the journal is hot from kill 24 on.'

SELECT kill_no AS kill, syscall, differ_pre AS chg_pre, differ_post AS chg_post,
       journal_bytes AS bytes, hot, header, outcome
FROM kills WHERE part = 'D' AND synchronous = 'NORMAL' ORDER BY kill_no;

.print
.print 'D9. synchronous=OFF, every kill: the header is written at once with its magic'
.print '    and record count 0xffffffff (no-sync: read to the end of the file), so'
.print '    the journal is hot from the first record on.'

SELECT kill_no AS kill, syscall, differ_pre AS chg_pre, differ_post AS chg_post,
       journal_bytes AS bytes, hot, header, printf('0x%x', record_count) AS count, outcome
FROM kills WHERE part = 'D' AND synchronous = 'OFF' ORDER BY kill_no;

.print
.print 'D10. The outcome of every kill, in commit order. B = pre-step exact bytes,'
.print '     b = pre-step except reused freelist leaves, A = post-step exact bytes,'
.print '     ? = anything else. The last B or b is the kill on entry to the unlink.'

SELECT synchronous,
       string_agg(outcome, '' ORDER BY kill_no) AS outcome_per_kill_point
FROM kills WHERE part = 'D' GROUP BY synchronous ORDER BY count(*) DESC;

SELECT synchronous,
       count(*) FILTER (outcome = 'B') AS "B pre exact",
       count(*) FILTER (outcome = 'b') AS "b pre logical",
       count(*) FILTER (outcome = 'A') AS "A post exact",
       count(*) FILTER (outcome = '?') AS "? other",
       count(*) FILTER (NOT landed_ok) AS missed_kills,
       count(*) FILTER (integrity <> 'ok') AS integrity_bad,
       count(*) FILTER (covered = false) AS not_covered
FROM kills WHERE part = 'D' GROUP BY synchronous ORDER BY count(*) DESC;

.print
.print '── D2. A step whose pre-step file has no freelist: step 1 ─────────────'
.print
.print 'Step 1 starts from the file after step 0, which has no freelist, so there'
.print 'is no b: every reopen gives the pre-step or post-step bytes exactly.'

SELECT step, recipe, schema, inserts, updates, deletes, indexes FROM steps WHERE step = 1;

SELECT state, recipe_step, pages, sha256_prefix, freelist_leaves
FROM states WHERE step = 1 ORDER BY state DESC;

SELECT synchronous, journaled_pages AS pages, record_writes, db_page_writes AS db_writes, syncs, sync_targets
FROM commits WHERE part = 'D2' ORDER BY seq;

WITH s AS (
  SELECT synchronous, unnest(string_split(sequence, ', ')) AS what,
         generate_subscripts(string_split(sequence, ', '), 1) AS i
  FROM commits WHERE part = 'D2')
SELECT i,
       max(what) FILTER (synchronous = 'FULL')   AS "FULL",
       max(what) FILTER (synchronous = 'NORMAL') AS "NORMAL",
       max(what) FILTER (synchronous = 'OFF')    AS "OFF"
FROM s GROUP BY i ORDER BY i;

SELECT synchronous,
       string_agg(outcome, '' ORDER BY kill_no) AS outcome_per_kill_point
FROM kills WHERE part = 'D2' GROUP BY synchronous ORDER BY count(*) DESC;

SELECT synchronous,
       count(*) FILTER (outcome = 'B') AS "B pre exact",
       count(*) FILTER (outcome = 'b') AS "b pre logical",
       count(*) FILTER (outcome = 'A') AS "A post exact",
       count(*) FILTER (outcome = '?') AS "? other",
       count(*) FILTER (NOT landed_ok) AS missed_kills,
       count(*) FILTER (integrity <> 'ok') AS integrity_bad
FROM kills WHERE part = 'D2' GROUP BY synchronous ORDER BY count(*) DESC;

.print
.print '── E. Losing the hot journal (step 3, synchronous=FULL) ───────────────'
.print
.print 'E1. Two crashes: in the middle of the database writes, and on entry to the'
.print '    database sync, when every page is written but the journal is still hot.'

SELECT crash, killed_on, db_write_no AS write_no, db_writes AS writes, db_file,
       db_pages AS pages, differ_pre AS chg_pre, differ_post AS chg_post
FROM crashes ORDER BY seq;

.print
.print 'The hot journal beside the mid-write crash:'

SELECT journal_bytes, printf('0x%02x', first_byte) AS first_byte, hot, header, record_count, initial_pages, records, record_pages
FROM journals WHERE part = 'E';

.print
.print 'E2. What each file holds when opened. Without its journal the torn file is'
.print '    unreadable, and a copy of the file alone is the same damage. Copied with'
.print '    its journal, it rolls back. Deleting the journal after the last database'
.print '    write commits a transaction whose COMMIT never returned.'

SELECT scenario, integrity, user_version, accounts, documents, events
FROM reopens ORDER BY seq;

SELECT scenario, outcome_name AS state, outcome_pages AS differs_in
FROM reopens WHERE outcome_name IS NOT NULL ORDER BY seq;

SELECT scenario, open_error FROM reopens WHERE open_error IS NOT NULL ORDER BY seq;

.print
.print '── F. The commit point in DELETE, TRUNCATE and PERSIST modes ──────────'
.print
.print 'F1. The same commit (step 3, synchronous=FULL) in each mode. Only the end'
.print '    differs: TRUNCATE and PERSIST finish the journal and sync it again.'

WITH s AS (
  SELECT journal_mode, unnest(string_split(sequence, ', ')) AS what,
         generate_subscripts(string_split(sequence, ', '), 1) AS i
  FROM commits WHERE part = 'F')
SELECT i,
       max(what) FILTER (journal_mode = 'delete')   AS "delete",
       max(what) FILTER (journal_mode = 'truncate') AS "truncate",
       max(what) FILTER (journal_mode = 'persist')  AS "persist"
FROM s GROUP BY i ORDER BY i;

SELECT journal_mode, journaled_pages AS pages, record_writes, db_page_writes AS db_writes, syncs, sync_targets
FROM commits WHERE part = 'F' ORDER BY seq;

.print
.print 'F2. A kill on entry to the commit-point syscall gives the pre-step state; a'
.print '    kill on the syscall after it gives the post-step state.'

SELECT journal_mode, commit_point, killed_on_entry, next_kind, killed_on_next
FROM commit_points ORDER BY seq;

.print
.print 'F3. The -journal left beside the database after a clean commit. TRUNCATE'
.print '    leaves 0 bytes; PERSIST leaves the whole journal with a zeroed header.'
.print '    Neither is hot.'

SELECT label AS journal_mode,
       coalesce(journal_bytes::VARCHAR, 'no -journal') AS journal_bytes,
       printf('0x%02x', first_byte) AS first_byte, hot, header
FROM journals WHERE part = 'F' ORDER BY seq;
```

`course run` also prints the run-dependent values, which are not part of the stored evidence: the sync syscall each program used, the timings, and the random nonce and run time of each journal the kills left behind.

#### Captured evidence (`4.1-atomic-commit-and-hot-journal.txt`)

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/4.1-atomic-commit-and-hot-journal/`](evidence/4.1-atomic-commit-and-hot-journal/), and `go run ./cmd/course check evidence ch04` compares a fresh run with them byte for byte.

<!-- include: evidence/4.1-atomic-commit-and-hot-journal.txt -->
```text

════════════════════════════════════════════════════════════════════════
  4.1  Atomic commit in rollback-journal mode, and the hot journal
════════════════════════════════════════════════════════════════════════

A rollback-journal commit of recipe step 3 (seed 1) is traced with strace,
then rerun and killed with SIGKILL on entry to each of its writes, syncs
and unlinks, at synchronous=FULL, NORMAL and OFF. After each kill Go reads
the database and -journal as bytes, then SQLite reopens the database.

What ran:
┌──────┬─────────────────────────────────────────────────────────┬────────┐
│ part │                          what                           │ status │
├──────┼─────────────────────────────────────────────────────────┼────────┤
│ A    │ journal_mode and header bytes 18-19 of a new file       │ ran    │
│ A    │ syscalls of the first commits of a new file             │ ran    │
│ B    │ the recipe step: pre-step and post-step files           │ ran    │
│ C    │ the journal of an open transaction                      │ ran    │
│ D    │ the commit traced and killed at every write and sync    │ ran    │
│ D2   │ the same for a step whose pre-step file has no freelist │ ran    │
│ E    │ losing the hot journal                                  │ ran    │
│ F    │ the commit point in DELETE, TRUNCATE and PERSIST modes  │ ran    │
└──────┴─────────────────────────────────────────────────────────┴────────┘

── A. Where a WAL-default platform meets rollback mode ─────────────────

A1. A new file starts in rollback mode (header bytes 18 and 19 = 1) until
    PRAGMA journal_mode=wal rewrites them to 2.
┌───────────────────────────────────────────────────────────┬──────────────┬─────────┬─────────┐
│                           stage                           │ journal_mode │ byte_18 │ byte_19 │
├───────────────────────────────────────────────────────────┼──────────────┼─────────┼─────────┤
│ new file, driver defaults, before any write               │ delete       │         │         │
│ after its first CREATE TABLE                              │              │ 1       │ 1       │
│ after PRAGMA journal_mode=wal, read by a fresh connection │ wal          │ 2       │ 2       │
└───────────────────────────────────────────────────────────┴──────────────┴─────────┴─────────┘

A2. Each statement traced in a fresh process. The switch to WAL, and the
    first commit of a WAL file opened with sqlitex.Open, still go through
    a -journal; the statements after them write only to the -wal.
┌──────────┬──────────────────────────────────────────────────┬─────────────────────────────────────────┬────────────┐
│  opener  │                    statement                     │            journal_syscalls             │ wal_writes │
├──────────┼──────────────────────────────────────────────────┼─────────────────────────────────────────┼────────────┤
│ plain    │ CREATE TABLE (driver defaults, first write)      │ open pwrite64 sync pwrite64 sync unlink │ 0          │
│ switch   │ PRAGMA journal_mode=wal on the new empty file    │ open pwrite64 sync pwrite64 sync unlink │ 0          │
│ switch   │ CREATE TABLE after the switch                    │                                         │ 5          │
│ platform │ sqlitex.Open of a new file with journal_mode=wal │ open pwrite64 sync pwrite64 sync unlink │ 5          │
│ platform │ CREATE TABLE after it                            │                                         │ 4          │
└──────────┴──────────────────────────────────────────────────┴─────────────────────────────────────────┴────────────┘

A3. The -journal syscalls of each of those rollback-mode commits:
┌──────────────────────────────────────────────────┬───┬────────────────────────────────────────────┐
│                    statement                     │ n │                    call                    │
├──────────────────────────────────────────────────┼───┼────────────────────────────────────────────┤
│ CREATE TABLE (driver defaults, first write)      │ 1 │ open plain.db-journal                      │
│ CREATE TABLE (driver defaults, first write)      │ 2 │ pwrite64 plain.db-journal off=0 len=512    │
│ CREATE TABLE (driver defaults, first write)      │ 3 │ sync plain.db-journal                      │
│ CREATE TABLE (driver defaults, first write)      │ 4 │ pwrite64 plain.db-journal off=0 len=12     │
│ CREATE TABLE (driver defaults, first write)      │ 5 │ sync plain.db-journal                      │
│ CREATE TABLE (driver defaults, first write)      │ 6 │ unlink plain.db-journal                    │
│ PRAGMA journal_mode=wal on the new empty file    │ 1 │ open switch.db-journal                     │
│ PRAGMA journal_mode=wal on the new empty file    │ 2 │ pwrite64 switch.db-journal off=0 len=512   │
│ PRAGMA journal_mode=wal on the new empty file    │ 3 │ sync switch.db-journal                     │
│ PRAGMA journal_mode=wal on the new empty file    │ 4 │ pwrite64 switch.db-journal off=0 len=12    │
│ PRAGMA journal_mode=wal on the new empty file    │ 5 │ sync switch.db-journal                     │
│ PRAGMA journal_mode=wal on the new empty file    │ 6 │ unlink switch.db-journal                   │
│ sqlitex.Open of a new file with journal_mode=wal │ 1 │ open platform.db-journal                   │
│ sqlitex.Open of a new file with journal_mode=wal │ 2 │ pwrite64 platform.db-journal off=0 len=512 │
│ sqlitex.Open of a new file with journal_mode=wal │ 3 │ sync platform.db-journal                   │
│ sqlitex.Open of a new file with journal_mode=wal │ 4 │ pwrite64 platform.db-journal off=0 len=12  │
│ sqlitex.Open of a new file with journal_mode=wal │ 5 │ sync platform.db-journal                   │
│ sqlitex.Open of a new file with journal_mode=wal │ 6 │ unlink platform.db-journal                 │
└──────────────────────────────────────────────────┴───┴────────────────────────────────────────────┘

── B. The recipe step ──────────────────────────────────────────────────

B1. Step 3 of the recipe, the transaction this walkthrough commits:
┌──────┬─────────────────────────────────────┬────────┬─────────┬─────────┬─────────┬─────────┐
│ step │               recipe                │ schema │ inserts │ updates │ deletes │ indexes │
├──────┼─────────────────────────────────────┼────────┼─────────┼─────────┼─────────┼─────────┤
│ 3    │ seed=1 steps=30 journal_mode=delete │ 0      │ 53      │ 5       │ 5       │ 0       │
└──────┴─────────────────────────────────────┴────────┴─────────┴─────────┴─────────┴─────────┘

B2. The file before (after recipe step 2) and after it. Page 22 is a
    freelist leaf in the pre-step file; remember it for part D.
┌───────────┬─────────────┬───────┬──────────────────┬─────────────────┐
│   state   │ recipe_step │ pages │  sha256_prefix   │ freelist_leaves │
├───────────┼─────────────┼───────┼──────────────────┼─────────────────┤
│ pre-step  │ 2           │ 29    │ b8a0bd0796442099 │ 22              │
│ post-step │ 3           │ 39    │ 0ef90bd0e64af1e3 │                 │
└───────────┴─────────────┴───────┴──────────────────┴─────────────────┘

── C. The journal of an open transaction, read before COMMIT ──────────

C1. Every statement of step 3 has run; COMMIT is not issued. The header
    is written but magic and record count are still zero, so the journal
    is not hot. It records the pre-transaction size: 29 pages.
┌───────┬────────────┬───────┬────────────────┬───────┬────────────┬────────┬──────┬─────────┬──────────┬─────────┐
│ bytes │ first_byte │  hot  │     header     │ count │ init_pages │ sector │ page │ records │ bad_sums │ partial │
├───────┼────────────┼───────┼────────────────┼───────┼────────────┼────────┼──────┼─────────┼──────────┼─────────┤
│ 29240 │ 0x00       │ false │ not-yet-synced │ 0     │ 29         │ 512    │ 4096 │ 7       │ 0        │ 0       │
└───────┴────────────┴───────┴────────────────┴───────┴────────────┴────────┴──────┴─────────┴──────────┴─────────┘

C2. Each record is a page number, the original image of the page and a checksum.
    Page 22, the reused freelist leaf, is not among them: SQLite fetches it
    with PAGER_GET_NOCONTENT and never journals it.
┌─────────────┬──────┬─────────────────────────┬──────────┬──────────────────┐
│ file_offset │ page │          role           │ checksum │ same_as_pre_step │
├─────────────┼──────┼─────────────────────────┼──────────┼──────────────────┤
│ 512         │ 2    │ b-tree or overflow page │ ok       │ true             │
│ 4616        │ 1    │ header and schema root  │ ok       │ true             │
│ 8720        │ 20   │ b-tree or overflow page │ ok       │ true             │
│ 12824       │ 3    │ b-tree or overflow page │ ok       │ true             │
│ 16928       │ 27   │ b-tree or overflow page │ ok       │ true             │
│ 21032       │ 4    │ b-tree or overflow page │ ok       │ true             │
│ 25136       │ 26   │ b-tree or overflow page │ ok       │ true             │
└─────────────┴──────┴─────────────────────────┴──────────┴──────────────────┘

C3. ROLLBACK by the live connection, then close:
┌──────────────────────┬──────────────┐
│      file_after      │ journal_left │
├──────────────────────┼──────────────┤
│ pre-step exact bytes │ false        │
└──────────────────────┴──────────────┘

── D. Step 3: one commit traced, then killed at every write and sync ──

D1. The synchronous=FULL commit: syscalls of the writer thread on the database
    directory, between the start of the step and its return. The journal
    records come first, then two journal syncs around the record count,
    then the database pages, one database sync, and the unlink.
┌────┬────────────────┬─────────────────────────────────────────────────────┐
│ n  │      kind      │                        call                         │
├────┼────────────────┼─────────────────────────────────────────────────────┤
│ 1  │ other          │ fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1   │
│ 2  │ other          │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 │
│ 3  │ other          │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1   │
│ 4  │ other          │ pread64 t.db off=24 len=16                          │
│ 5  │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1   │
│ 6  │ other          │ pread64 t.db off=4096 len=4096                      │
│ 7  │ other          │ open t.db-journal                                   │
│ 8  │ other          │ fcntl t.db-journal F_SETFD                          │
│ 9  │ journal-header │ pwrite64 t.db-journal off=0 len=512                 │
│ 10 │ journal-record │ pwrite64 t.db-journal off=512 len=4                 │
│ 11 │ journal-record │ pwrite64 t.db-journal off=516 len=4096              │
│ 12 │ journal-record │ pwrite64 t.db-journal off=4612 len=4                │
│ 13 │ other          │ pread64 t.db off=8192 len=4096                      │
│ 14 │ other          │ pread64 t.db off=98304 len=4096                     │
│ 15 │ journal-record │ pwrite64 t.db-journal off=4616 len=4                │
│ 16 │ journal-record │ pwrite64 t.db-journal off=4620 len=4096             │
│ 17 │ journal-record │ pwrite64 t.db-journal off=8716 len=4                │
│ 18 │ other          │ pread64 t.db off=77824 len=4096                     │
│ 19 │ journal-record │ pwrite64 t.db-journal off=8720 len=4                │
│ 20 │ journal-record │ pwrite64 t.db-journal off=8724 len=4096             │
│ 21 │ journal-record │ pwrite64 t.db-journal off=12820 len=4               │
│ 22 │ journal-record │ pwrite64 t.db-journal off=12824 len=4               │
│ 23 │ journal-record │ pwrite64 t.db-journal off=12828 len=4096            │
│ 24 │ journal-record │ pwrite64 t.db-journal off=16924 len=4               │
│ 25 │ other          │ pread64 t.db off=12288 len=4096                     │
│ 26 │ other          │ pread64 t.db off=106496 len=4096                    │
│ 27 │ journal-record │ pwrite64 t.db-journal off=16928 len=4               │
│ 28 │ journal-record │ pwrite64 t.db-journal off=16932 len=4096            │
│ 29 │ journal-record │ pwrite64 t.db-journal off=21028 len=4               │
│ 30 │ journal-record │ pwrite64 t.db-journal off=21032 len=4               │
│ 31 │ journal-record │ pwrite64 t.db-journal off=21036 len=4096            │
│ 32 │ journal-record │ pwrite64 t.db-journal off=25132 len=4               │
│ 33 │ other          │ pread64 t.db off=102400 len=4096                    │
│ 34 │ journal-record │ pwrite64 t.db-journal off=25136 len=4               │
│ 35 │ journal-record │ pwrite64 t.db-journal off=25140 len=4096            │
│ 36 │ journal-record │ pwrite64 t.db-journal off=29236 len=4               │
│ 37 │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1   │
│ 38 │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 │
│ 39 │ other          │ pread64 t.db-journal off=29696 len=8 = 0            │
│ 40 │ journal-sync   │ sync t.db-journal                                   │
│ 41 │ other          │ open .                                              │
│ 42 │ other          │ fcntl . F_SETFD                                     │
│ 43 │ dir-sync       │ sync .                                              │
│ 44 │ other          │ close .                                             │
│ 45 │ journal-count  │ pwrite64 t.db-journal off=0 len=12                  │
│ 46 │ journal-sync   │ sync t.db-journal                                   │
│ 47 │ db-page        │ pwrite64 t.db off=0 len=4096                        │
│ 48 │ db-page        │ pwrite64 t.db off=4096 len=4096                     │
│ 49 │ db-page        │ pwrite64 t.db off=8192 len=4096                     │
│ 50 │ db-page        │ pwrite64 t.db off=12288 len=4096                    │
│ 51 │ db-page        │ pwrite64 t.db off=77824 len=4096                    │
│ 52 │ db-page        │ pwrite64 t.db off=86016 len=4096                    │
│ 53 │ db-page        │ pwrite64 t.db off=102400 len=4096                   │
│ 54 │ db-page        │ pwrite64 t.db off=106496 len=4096                   │
│ 55 │ db-page        │ pwrite64 t.db off=118784 len=4096                   │
│ 56 │ db-page        │ pwrite64 t.db off=122880 len=4096                   │
│ 57 │ db-page        │ pwrite64 t.db off=126976 len=4096                   │
│ 58 │ db-page        │ pwrite64 t.db off=131072 len=4096                   │
│ 59 │ db-page        │ pwrite64 t.db off=135168 len=4096                   │
│ 60 │ db-page        │ pwrite64 t.db off=139264 len=4096                   │
│ 61 │ db-page        │ pwrite64 t.db off=143360 len=4096                   │
│ 62 │ db-page        │ pwrite64 t.db off=147456 len=4096                   │
│ 63 │ db-page        │ pwrite64 t.db off=151552 len=4096                   │
│ 64 │ db-page        │ pwrite64 t.db off=155648 len=4096                   │
│ 65 │ db-sync        │ sync t.db                                           │
│ 66 │ other          │ close t.db-journal                                  │
│ 67 │ journal-unlink │ unlink t.db-journal                                 │
│ 68 │ other          │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 │
│ 69 │ other          │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2   │
│ 70 │ other          │ fcntl t.db F_SETLK F_UNLCK start=0 len=0            │
└────┴────────────────┴─────────────────────────────────────────────────────┘

D2. The same commit at NORMAL and OFF is identical up to the last journal
    record. From there on: NORMAL writes the record count before a single
    journal sync; OFF syncs nothing.
┌─────────────┬────┬────────────────┬─────────────────────────────────────────────────────┐
│ synchronous │ n  │      kind      │                        call                         │
├─────────────┼────┼────────────────┼─────────────────────────────────────────────────────┤
│ NORMAL      │ 37 │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1   │
│ NORMAL      │ 38 │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 │
│ NORMAL      │ 39 │ other          │ pread64 t.db-journal off=29696 len=8 = 0            │
│ NORMAL      │ 40 │ journal-count  │ pwrite64 t.db-journal off=0 len=12                  │
│ NORMAL      │ 41 │ journal-sync   │ sync t.db-journal                                   │
│ NORMAL      │ 42 │ other          │ open .                                              │
│ NORMAL      │ 43 │ other          │ fcntl . F_SETFD                                     │
│ NORMAL      │ 44 │ dir-sync       │ sync .                                              │
│ NORMAL      │ 45 │ other          │ close .                                             │
│ NORMAL      │ 46 │ db-page        │ pwrite64 t.db off=0 len=4096                        │
│ NORMAL      │ 47 │ db-page        │ pwrite64 t.db off=4096 len=4096                     │
│ NORMAL      │ 48 │ db-page        │ pwrite64 t.db off=8192 len=4096                     │
│ NORMAL      │ 49 │ db-page        │ pwrite64 t.db off=12288 len=4096                    │
│ NORMAL      │ 50 │ db-page        │ pwrite64 t.db off=77824 len=4096                    │
│ NORMAL      │ 51 │ db-page        │ pwrite64 t.db off=86016 len=4096                    │
│ NORMAL      │ 52 │ db-page        │ pwrite64 t.db off=102400 len=4096                   │
│ NORMAL      │ 53 │ db-page        │ pwrite64 t.db off=106496 len=4096                   │
│ NORMAL      │ 54 │ db-page        │ pwrite64 t.db off=118784 len=4096                   │
│ NORMAL      │ 55 │ db-page        │ pwrite64 t.db off=122880 len=4096                   │
│ NORMAL      │ 56 │ db-page        │ pwrite64 t.db off=126976 len=4096                   │
│ NORMAL      │ 57 │ db-page        │ pwrite64 t.db off=131072 len=4096                   │
│ NORMAL      │ 58 │ db-page        │ pwrite64 t.db off=135168 len=4096                   │
│ NORMAL      │ 59 │ db-page        │ pwrite64 t.db off=139264 len=4096                   │
│ NORMAL      │ 60 │ db-page        │ pwrite64 t.db off=143360 len=4096                   │
│ NORMAL      │ 61 │ db-page        │ pwrite64 t.db off=147456 len=4096                   │
│ NORMAL      │ 62 │ db-page        │ pwrite64 t.db off=151552 len=4096                   │
│ NORMAL      │ 63 │ db-page        │ pwrite64 t.db off=155648 len=4096                   │
│ NORMAL      │ 64 │ db-sync        │ sync t.db                                           │
│ NORMAL      │ 65 │ other          │ close t.db-journal                                  │
│ NORMAL      │ 66 │ journal-unlink │ unlink t.db-journal                                 │
│ NORMAL      │ 67 │ other          │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 │
│ NORMAL      │ 68 │ other          │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2   │
│ NORMAL      │ 69 │ other          │ fcntl t.db F_SETLK F_UNLCK start=0 len=0            │
│ OFF         │ 37 │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1   │
│ OFF         │ 38 │ other          │ fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 │
│ OFF         │ 39 │ db-page        │ pwrite64 t.db off=0 len=4096                        │
│ OFF         │ 40 │ db-page        │ pwrite64 t.db off=4096 len=4096                     │
│ OFF         │ 41 │ db-page        │ pwrite64 t.db off=8192 len=4096                     │
│ OFF         │ 42 │ db-page        │ pwrite64 t.db off=12288 len=4096                    │
│ OFF         │ 43 │ db-page        │ pwrite64 t.db off=77824 len=4096                    │
│ OFF         │ 44 │ db-page        │ pwrite64 t.db off=86016 len=4096                    │
│ OFF         │ 45 │ db-page        │ pwrite64 t.db off=102400 len=4096                   │
│ OFF         │ 46 │ db-page        │ pwrite64 t.db off=106496 len=4096                   │
│ OFF         │ 47 │ db-page        │ pwrite64 t.db off=118784 len=4096                   │
│ OFF         │ 48 │ db-page        │ pwrite64 t.db off=122880 len=4096                   │
│ OFF         │ 49 │ db-page        │ pwrite64 t.db off=126976 len=4096                   │
│ OFF         │ 50 │ db-page        │ pwrite64 t.db off=131072 len=4096                   │
│ OFF         │ 51 │ db-page        │ pwrite64 t.db off=135168 len=4096                   │
│ OFF         │ 52 │ db-page        │ pwrite64 t.db off=139264 len=4096                   │
│ OFF         │ 53 │ db-page        │ pwrite64 t.db off=143360 len=4096                   │
│ OFF         │ 54 │ db-page        │ pwrite64 t.db off=147456 len=4096                   │
│ OFF         │ 55 │ db-page        │ pwrite64 t.db off=151552 len=4096                   │
│ OFF         │ 56 │ db-page        │ pwrite64 t.db off=155648 len=4096                   │
│ OFF         │ 57 │ other          │ close t.db-journal                                  │
│ OFF         │ 58 │ journal-unlink │ unlink t.db-journal                                 │
│ OFF         │ 59 │ other          │ fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 │
│ OFF         │ 60 │ other          │ fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2   │
│ OFF         │ 61 │ other          │ fcntl t.db F_SETLK F_UNLCK start=0 len=0            │
└─────────────┴────┴────────────────┴─────────────────────────────────────────────────────┘

D3. The three sequences side by side (other syscalls left out, repeats folded):
┌───┬────────────────────┬────────────────────┬────────────────────┐
│ i │        FULL        │       NORMAL       │        OFF         │
├───┼────────────────────┼────────────────────┼────────────────────┤
│ 1 │ journal-header     │ journal-header     │ journal-header     │
│ 2 │ journal-record x21 │ journal-record x21 │ journal-record x21 │
│ 3 │ journal-sync       │ journal-count      │ db-page x18        │
│ 4 │ dir-sync           │ journal-sync       │ journal-unlink     │
│ 5 │ journal-count      │ dir-sync           │                    │
│ 6 │ journal-sync       │ db-page x18        │                    │
│ 7 │ db-page x18        │ db-sync            │                    │
│ 8 │ db-sync            │ journal-unlink     │                    │
│ 9 │ journal-unlink     │                    │                    │
└───┴────────────────────┴────────────────────┴────────────────────┘
┌─────────────┬───────┬───────────────┬───────────┬───────┬──────────────────────────────────┐
│ synchronous │ pages │ record_writes │ db_writes │ syncs │           sync_targets           │
├─────────────┼───────┼───────────────┼───────────┼───────┼──────────────────────────────────┤
│ FULL        │ 7     │ 21            │ 18        │ 4     │ t.db-journal . t.db-journal t.db │
│ NORMAL      │ 7     │ 21            │ 18        │ 3     │ t.db-journal . t.db              │
│ OFF         │ 7     │ 21            │ 18        │ 0     │                                  │
└─────────────┴───────┴───────────────┴───────────┴───────┴──────────────────────────────────┘

D4. The pinned C sqlite3 CLI runs the same step as literal SQL. Its writes,
    syncs and unlink equal those of the Go driver in file, offset, length, order.
┌─────────────┬──────────┬───────────┬───────────┬──────────────────────┐
│ synchronous │ go_calls │ cli_calls │ identical │ cli_leaves_post_step │
├─────────────┼──────────┼───────────┼───────────┼──────────────────────┤
│ FULL        │ 46       │ 46        │ true      │ true                 │
│ NORMAL      │ 45       │ 45        │ true      │ true                 │
│ OFF         │ 41       │ 41        │ true      │ true                 │
└─────────────┴──────────┴───────────┴───────────┴──────────────────────┘

D5. Kill points: every pwrite64, sync and unlink of the commit, then the
    first syscall after the step returned.
┌─────────────┬─────────────┐
│ synchronous │ kill_points │
├─────────────┼─────────────┤
│ FULL        │ 47          │
│ NORMAL      │ 46          │
│ OFF         │ 42          │
└─────────────┴─────────────┘

D6. synchronous=FULL: what each kill left on disk. Until the record count is
    written and synced (kill 26) the journal is not hot and the database
    file is untouched. From kill 28 on, the database is torn mid-write.
    chg_pre and chg_post count the pages of the database file that differ
    from the pre-step and post-step files.
┌──────┬──────────────────────────────────────────┬─────────┬──────────┬───────┬───────┬────────────────┬──────┬─────────┐
│ kill │                 syscall                  │ chg_pre │ chg_post │ bytes │  hot  │     header     │ recs │ partial │
├──────┼──────────────────────────────────────────┼─────────┼──────────┼───────┼───────┼────────────────┼──────┼─────────┤
│ 1    │ pwrite64 t.db-journal off=0 len=512      │ 0       │ 18       │ 0     │ false │                │      │         │
│ 2    │ pwrite64 t.db-journal off=512 len=4      │ 0       │ 18       │ 512   │ false │ not-yet-synced │ 0    │ 0       │
│ 3    │ pwrite64 t.db-journal off=516 len=4096   │ 0       │ 18       │ 516   │ false │ not-yet-synced │ 0    │ 4       │
│ 4    │ pwrite64 t.db-journal off=4612 len=4     │ 0       │ 18       │ 4612  │ false │ not-yet-synced │ 0    │ 4100    │
│ 5    │ pwrite64 t.db-journal off=4616 len=4     │ 0       │ 18       │ 4616  │ false │ not-yet-synced │ 1    │ 0       │
│ 6    │ pwrite64 t.db-journal off=4620 len=4096  │ 0       │ 18       │ 4620  │ false │ not-yet-synced │ 1    │ 4       │
│ 7    │ pwrite64 t.db-journal off=8716 len=4     │ 0       │ 18       │ 8716  │ false │ not-yet-synced │ 1    │ 4100    │
│ 8    │ pwrite64 t.db-journal off=8720 len=4     │ 0       │ 18       │ 8720  │ false │ not-yet-synced │ 2    │ 0       │
│ 9    │ pwrite64 t.db-journal off=8724 len=4096  │ 0       │ 18       │ 8724  │ false │ not-yet-synced │ 2    │ 4       │
│ 10   │ pwrite64 t.db-journal off=12820 len=4    │ 0       │ 18       │ 12820 │ false │ not-yet-synced │ 2    │ 4100    │
│ 11   │ pwrite64 t.db-journal off=12824 len=4    │ 0       │ 18       │ 12824 │ false │ not-yet-synced │ 3    │ 0       │
│ 12   │ pwrite64 t.db-journal off=12828 len=4096 │ 0       │ 18       │ 12828 │ false │ not-yet-synced │ 3    │ 4       │
│ 13   │ pwrite64 t.db-journal off=16924 len=4    │ 0       │ 18       │ 16924 │ false │ not-yet-synced │ 3    │ 4100    │
│ 14   │ pwrite64 t.db-journal off=16928 len=4    │ 0       │ 18       │ 16928 │ false │ not-yet-synced │ 4    │ 0       │
│ 15   │ pwrite64 t.db-journal off=16932 len=4096 │ 0       │ 18       │ 16932 │ false │ not-yet-synced │ 4    │ 4       │
│ 16   │ pwrite64 t.db-journal off=21028 len=4    │ 0       │ 18       │ 21028 │ false │ not-yet-synced │ 4    │ 4100    │
│ 17   │ pwrite64 t.db-journal off=21032 len=4    │ 0       │ 18       │ 21032 │ false │ not-yet-synced │ 5    │ 0       │
│ 18   │ pwrite64 t.db-journal off=21036 len=4096 │ 0       │ 18       │ 21036 │ false │ not-yet-synced │ 5    │ 4       │
│ 19   │ pwrite64 t.db-journal off=25132 len=4    │ 0       │ 18       │ 25132 │ false │ not-yet-synced │ 5    │ 4100    │
│ 20   │ pwrite64 t.db-journal off=25136 len=4    │ 0       │ 18       │ 25136 │ false │ not-yet-synced │ 6    │ 0       │
│ 21   │ pwrite64 t.db-journal off=25140 len=4096 │ 0       │ 18       │ 25140 │ false │ not-yet-synced │ 6    │ 4       │
│ 22   │ pwrite64 t.db-journal off=29236 len=4    │ 0       │ 18       │ 29236 │ false │ not-yet-synced │ 6    │ 4100    │
│ 23   │ sync t.db-journal                        │ 0       │ 18       │ 29240 │ false │ not-yet-synced │ 7    │ 0       │
│ 24   │ sync .                                   │ 0       │ 18       │ 29240 │ false │ not-yet-synced │ 7    │ 0       │
│ 25   │ pwrite64 t.db-journal off=0 len=12       │ 0       │ 18       │ 29240 │ false │ not-yet-synced │ 7    │ 0       │
│ 26   │ sync t.db-journal                        │ 0       │ 18       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 27   │ pwrite64 t.db off=0 len=4096             │ 0       │ 18       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 28   │ pwrite64 t.db off=4096 len=4096          │ 1       │ 17       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 29   │ pwrite64 t.db off=8192 len=4096          │ 2       │ 16       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 30   │ pwrite64 t.db off=12288 len=4096         │ 3       │ 15       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 31   │ pwrite64 t.db off=77824 len=4096         │ 4       │ 14       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 32   │ pwrite64 t.db off=86016 len=4096         │ 5       │ 13       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 33   │ pwrite64 t.db off=102400 len=4096        │ 6       │ 12       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 34   │ pwrite64 t.db off=106496 len=4096        │ 7       │ 11       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 35   │ pwrite64 t.db off=118784 len=4096        │ 8       │ 10       │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 36   │ pwrite64 t.db off=122880 len=4096        │ 9       │ 9        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 37   │ pwrite64 t.db off=126976 len=4096        │ 10      │ 8        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 38   │ pwrite64 t.db off=131072 len=4096        │ 11      │ 7        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 39   │ pwrite64 t.db off=135168 len=4096        │ 12      │ 6        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 40   │ pwrite64 t.db off=139264 len=4096        │ 13      │ 5        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 41   │ pwrite64 t.db off=143360 len=4096        │ 14      │ 4        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 42   │ pwrite64 t.db off=147456 len=4096        │ 15      │ 3        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 43   │ pwrite64 t.db off=151552 len=4096        │ 16      │ 2        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 44   │ pwrite64 t.db off=155648 len=4096        │ 17      │ 1        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 45   │ sync t.db                                │ 18      │ 0        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 46   │ unlink t.db-journal                      │ 18      │ 0        │ 29240 │ true  │ synced         │ 7    │ 0       │
│ 47   │ open marker-end                          │ 18      │ 0        │       │       │                │      │         │
└──────┴──────────────────────────────────────────┴─────────┴──────────┴───────┴───────┴────────────────┴──────┴─────────┘

D7. synchronous=FULL: what SQLite did on reopen. A hot journal is played
    back and the file truncated to its 29 original pages; the result is the
    pre-step state (B), or pre-step except page 22 (b). After the unlink,
    the reopened file is the post-step state (A).
┌──────┬───────────────────┬──────────────────┬───────┬─────────┬────────────┬───────────┬─────────────────────────┐
│ kill │     playback      │  playback_pages  │ trunc │ outcome │ differs_in │ integrity │      journal_after      │
├──────┼───────────────────┼──────────────────┼───────┼─────────┼────────────┼───────────┼─────────────────────────┤
│ 1    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 0 bytes, 0x00     │
│ 2    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 512 bytes, 0x00   │
│ 3    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 516 bytes, 0x00   │
│ 4    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 4612 bytes, 0x00  │
│ 5    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 4616 bytes, 0x00  │
│ 6    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 4620 bytes, 0x00  │
│ 7    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 8716 bytes, 0x00  │
│ 8    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 8720 bytes, 0x00  │
│ 9    │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 8724 bytes, 0x00  │
│ 10   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 12820 bytes, 0x00 │
│ 11   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 12824 bytes, 0x00 │
│ 12   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 12828 bytes, 0x00 │
│ 13   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 16924 bytes, 0x00 │
│ 14   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 16928 bytes, 0x00 │
│ 15   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 16932 bytes, 0x00 │
│ 16   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 21028 bytes, 0x00 │
│ 17   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 21032 bytes, 0x00 │
│ 18   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 21036 bytes, 0x00 │
│ 19   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 25132 bytes, 0x00 │
│ 20   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 25136 bytes, 0x00 │
│ 21   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 25140 bytes, 0x00 │
│ 22   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 29236 bytes, 0x00 │
│ 23   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 29240 bytes, 0x00 │
│ 24   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 29240 bytes, 0x00 │
│ 25   │ ignored (not hot) │                  │       │ B       │            │ ok        │ cold, 29240 bytes, 0x00 │
│ 26   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 27   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 28   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 29   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 30   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 31   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 32   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ B       │            │ ok        │ no -journal             │
│ 33   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 34   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 35   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 36   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 37   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 38   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 39   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 40   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 41   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 42   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 43   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 44   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 45   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 46   │ rolled back       │ 2 1 20 3 27 4 26 │ 29    │ b       │ 22         │ ok        │ no -journal             │
│ 47   │ no journal        │                  │       │ A       │            │ ok        │ no -journal             │
└──────┴───────────────────┴──────────────────┴───────┴─────────┴────────────┴───────────┴─────────────────────────┘

b means the reopened file equals pre-step except in reused freelist leaves
(page 22): the rollback cannot restore their unused bytes because they were
never journaled, and VACUUM INTO of the file equals that of the pre-step file.

D8. synchronous=NORMAL, every kill: the record count is written before the
    single journal sync, so the journal is hot from kill 24 on.
┌──────┬──────────────────────────────────────────┬─────────┬──────────┬───────┬───────┬────────────────┬─────────┐
│ kill │                 syscall                  │ chg_pre │ chg_post │ bytes │  hot  │     header     │ outcome │
├──────┼──────────────────────────────────────────┼─────────┼──────────┼───────┼───────┼────────────────┼─────────┤
│ 1    │ pwrite64 t.db-journal off=0 len=512      │ 0       │ 18       │ 0     │ false │                │ B       │
│ 2    │ pwrite64 t.db-journal off=512 len=4      │ 0       │ 18       │ 512   │ false │ not-yet-synced │ B       │
│ 3    │ pwrite64 t.db-journal off=516 len=4096   │ 0       │ 18       │ 516   │ false │ not-yet-synced │ B       │
│ 4    │ pwrite64 t.db-journal off=4612 len=4     │ 0       │ 18       │ 4612  │ false │ not-yet-synced │ B       │
│ 5    │ pwrite64 t.db-journal off=4616 len=4     │ 0       │ 18       │ 4616  │ false │ not-yet-synced │ B       │
│ 6    │ pwrite64 t.db-journal off=4620 len=4096  │ 0       │ 18       │ 4620  │ false │ not-yet-synced │ B       │
│ 7    │ pwrite64 t.db-journal off=8716 len=4     │ 0       │ 18       │ 8716  │ false │ not-yet-synced │ B       │
│ 8    │ pwrite64 t.db-journal off=8720 len=4     │ 0       │ 18       │ 8720  │ false │ not-yet-synced │ B       │
│ 9    │ pwrite64 t.db-journal off=8724 len=4096  │ 0       │ 18       │ 8724  │ false │ not-yet-synced │ B       │
│ 10   │ pwrite64 t.db-journal off=12820 len=4    │ 0       │ 18       │ 12820 │ false │ not-yet-synced │ B       │
│ 11   │ pwrite64 t.db-journal off=12824 len=4    │ 0       │ 18       │ 12824 │ false │ not-yet-synced │ B       │
│ 12   │ pwrite64 t.db-journal off=12828 len=4096 │ 0       │ 18       │ 12828 │ false │ not-yet-synced │ B       │
│ 13   │ pwrite64 t.db-journal off=16924 len=4    │ 0       │ 18       │ 16924 │ false │ not-yet-synced │ B       │
│ 14   │ pwrite64 t.db-journal off=16928 len=4    │ 0       │ 18       │ 16928 │ false │ not-yet-synced │ B       │
│ 15   │ pwrite64 t.db-journal off=16932 len=4096 │ 0       │ 18       │ 16932 │ false │ not-yet-synced │ B       │
│ 16   │ pwrite64 t.db-journal off=21028 len=4    │ 0       │ 18       │ 21028 │ false │ not-yet-synced │ B       │
│ 17   │ pwrite64 t.db-journal off=21032 len=4    │ 0       │ 18       │ 21032 │ false │ not-yet-synced │ B       │
│ 18   │ pwrite64 t.db-journal off=21036 len=4096 │ 0       │ 18       │ 21036 │ false │ not-yet-synced │ B       │
│ 19   │ pwrite64 t.db-journal off=25132 len=4    │ 0       │ 18       │ 25132 │ false │ not-yet-synced │ B       │
│ 20   │ pwrite64 t.db-journal off=25136 len=4    │ 0       │ 18       │ 25136 │ false │ not-yet-synced │ B       │
│ 21   │ pwrite64 t.db-journal off=25140 len=4096 │ 0       │ 18       │ 25140 │ false │ not-yet-synced │ B       │
│ 22   │ pwrite64 t.db-journal off=29236 len=4    │ 0       │ 18       │ 29236 │ false │ not-yet-synced │ B       │
│ 23   │ pwrite64 t.db-journal off=0 len=12       │ 0       │ 18       │ 29240 │ false │ not-yet-synced │ B       │
│ 24   │ sync t.db-journal                        │ 0       │ 18       │ 29240 │ true  │ synced         │ B       │
│ 25   │ sync .                                   │ 0       │ 18       │ 29240 │ true  │ synced         │ B       │
│ 26   │ pwrite64 t.db off=0 len=4096             │ 0       │ 18       │ 29240 │ true  │ synced         │ B       │
│ 27   │ pwrite64 t.db off=4096 len=4096          │ 1       │ 17       │ 29240 │ true  │ synced         │ B       │
│ 28   │ pwrite64 t.db off=8192 len=4096          │ 2       │ 16       │ 29240 │ true  │ synced         │ B       │
│ 29   │ pwrite64 t.db off=12288 len=4096         │ 3       │ 15       │ 29240 │ true  │ synced         │ B       │
│ 30   │ pwrite64 t.db off=77824 len=4096         │ 4       │ 14       │ 29240 │ true  │ synced         │ B       │
│ 31   │ pwrite64 t.db off=86016 len=4096         │ 5       │ 13       │ 29240 │ true  │ synced         │ B       │
│ 32   │ pwrite64 t.db off=102400 len=4096        │ 6       │ 12       │ 29240 │ true  │ synced         │ b       │
│ 33   │ pwrite64 t.db off=106496 len=4096        │ 7       │ 11       │ 29240 │ true  │ synced         │ b       │
│ 34   │ pwrite64 t.db off=118784 len=4096        │ 8       │ 10       │ 29240 │ true  │ synced         │ b       │
│ 35   │ pwrite64 t.db off=122880 len=4096        │ 9       │ 9        │ 29240 │ true  │ synced         │ b       │
│ 36   │ pwrite64 t.db off=126976 len=4096        │ 10      │ 8        │ 29240 │ true  │ synced         │ b       │
│ 37   │ pwrite64 t.db off=131072 len=4096        │ 11      │ 7        │ 29240 │ true  │ synced         │ b       │
│ 38   │ pwrite64 t.db off=135168 len=4096        │ 12      │ 6        │ 29240 │ true  │ synced         │ b       │
│ 39   │ pwrite64 t.db off=139264 len=4096        │ 13      │ 5        │ 29240 │ true  │ synced         │ b       │
│ 40   │ pwrite64 t.db off=143360 len=4096        │ 14      │ 4        │ 29240 │ true  │ synced         │ b       │
│ 41   │ pwrite64 t.db off=147456 len=4096        │ 15      │ 3        │ 29240 │ true  │ synced         │ b       │
│ 42   │ pwrite64 t.db off=151552 len=4096        │ 16      │ 2        │ 29240 │ true  │ synced         │ b       │
│ 43   │ pwrite64 t.db off=155648 len=4096        │ 17      │ 1        │ 29240 │ true  │ synced         │ b       │
│ 44   │ sync t.db                                │ 18      │ 0        │ 29240 │ true  │ synced         │ b       │
│ 45   │ unlink t.db-journal                      │ 18      │ 0        │ 29240 │ true  │ synced         │ b       │
│ 46   │ open marker-end                          │ 18      │ 0        │       │       │                │ A       │
└──────┴──────────────────────────────────────────┴─────────┴──────────┴───────┴───────┴────────────────┴─────────┘

D9. synchronous=OFF, every kill: the header is written at once with its magic
    and record count 0xffffffff (no-sync: read to the end of the file), so
    the journal is hot from the first record on.
┌──────┬──────────────────────────────────────────┬─────────┬──────────┬───────┬───────┬─────────┬────────────┬─────────┐
│ kill │                 syscall                  │ chg_pre │ chg_post │ bytes │  hot  │ header  │   count    │ outcome │
├──────┼──────────────────────────────────────────┼─────────┼──────────┼───────┼───────┼─────────┼────────────┼─────────┤
│ 1    │ pwrite64 t.db-journal off=0 len=512      │ 0       │ 18       │ 0     │ false │         │            │ B       │
│ 2    │ pwrite64 t.db-journal off=512 len=4      │ 0       │ 18       │ 512   │ true  │ no-sync │ 0xffffffff │ B       │
│ 3    │ pwrite64 t.db-journal off=516 len=4096   │ 0       │ 18       │ 516   │ true  │ no-sync │ 0xffffffff │ B       │
│ 4    │ pwrite64 t.db-journal off=4612 len=4     │ 0       │ 18       │ 4612  │ true  │ no-sync │ 0xffffffff │ B       │
│ 5    │ pwrite64 t.db-journal off=4616 len=4     │ 0       │ 18       │ 4616  │ true  │ no-sync │ 0xffffffff │ B       │
│ 6    │ pwrite64 t.db-journal off=4620 len=4096  │ 0       │ 18       │ 4620  │ true  │ no-sync │ 0xffffffff │ B       │
│ 7    │ pwrite64 t.db-journal off=8716 len=4     │ 0       │ 18       │ 8716  │ true  │ no-sync │ 0xffffffff │ B       │
│ 8    │ pwrite64 t.db-journal off=8720 len=4     │ 0       │ 18       │ 8720  │ true  │ no-sync │ 0xffffffff │ B       │
│ 9    │ pwrite64 t.db-journal off=8724 len=4096  │ 0       │ 18       │ 8724  │ true  │ no-sync │ 0xffffffff │ B       │
│ 10   │ pwrite64 t.db-journal off=12820 len=4    │ 0       │ 18       │ 12820 │ true  │ no-sync │ 0xffffffff │ B       │
│ 11   │ pwrite64 t.db-journal off=12824 len=4    │ 0       │ 18       │ 12824 │ true  │ no-sync │ 0xffffffff │ B       │
│ 12   │ pwrite64 t.db-journal off=12828 len=4096 │ 0       │ 18       │ 12828 │ true  │ no-sync │ 0xffffffff │ B       │
│ 13   │ pwrite64 t.db-journal off=16924 len=4    │ 0       │ 18       │ 16924 │ true  │ no-sync │ 0xffffffff │ B       │
│ 14   │ pwrite64 t.db-journal off=16928 len=4    │ 0       │ 18       │ 16928 │ true  │ no-sync │ 0xffffffff │ B       │
│ 15   │ pwrite64 t.db-journal off=16932 len=4096 │ 0       │ 18       │ 16932 │ true  │ no-sync │ 0xffffffff │ B       │
│ 16   │ pwrite64 t.db-journal off=21028 len=4    │ 0       │ 18       │ 21028 │ true  │ no-sync │ 0xffffffff │ B       │
│ 17   │ pwrite64 t.db-journal off=21032 len=4    │ 0       │ 18       │ 21032 │ true  │ no-sync │ 0xffffffff │ B       │
│ 18   │ pwrite64 t.db-journal off=21036 len=4096 │ 0       │ 18       │ 21036 │ true  │ no-sync │ 0xffffffff │ B       │
│ 19   │ pwrite64 t.db-journal off=25132 len=4    │ 0       │ 18       │ 25132 │ true  │ no-sync │ 0xffffffff │ B       │
│ 20   │ pwrite64 t.db-journal off=25136 len=4    │ 0       │ 18       │ 25136 │ true  │ no-sync │ 0xffffffff │ B       │
│ 21   │ pwrite64 t.db-journal off=25140 len=4096 │ 0       │ 18       │ 25140 │ true  │ no-sync │ 0xffffffff │ B       │
│ 22   │ pwrite64 t.db-journal off=29236 len=4    │ 0       │ 18       │ 29236 │ true  │ no-sync │ 0xffffffff │ B       │
│ 23   │ pwrite64 t.db off=0 len=4096             │ 0       │ 18       │ 29240 │ true  │ no-sync │ 0xffffffff │ B       │
│ 24   │ pwrite64 t.db off=4096 len=4096          │ 1       │ 17       │ 29240 │ true  │ no-sync │ 0xffffffff │ B       │
│ 25   │ pwrite64 t.db off=8192 len=4096          │ 2       │ 16       │ 29240 │ true  │ no-sync │ 0xffffffff │ B       │
│ 26   │ pwrite64 t.db off=12288 len=4096         │ 3       │ 15       │ 29240 │ true  │ no-sync │ 0xffffffff │ B       │
│ 27   │ pwrite64 t.db off=77824 len=4096         │ 4       │ 14       │ 29240 │ true  │ no-sync │ 0xffffffff │ B       │
│ 28   │ pwrite64 t.db off=86016 len=4096         │ 5       │ 13       │ 29240 │ true  │ no-sync │ 0xffffffff │ B       │
│ 29   │ pwrite64 t.db off=102400 len=4096        │ 6       │ 12       │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 30   │ pwrite64 t.db off=106496 len=4096        │ 7       │ 11       │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 31   │ pwrite64 t.db off=118784 len=4096        │ 8       │ 10       │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 32   │ pwrite64 t.db off=122880 len=4096        │ 9       │ 9        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 33   │ pwrite64 t.db off=126976 len=4096        │ 10      │ 8        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 34   │ pwrite64 t.db off=131072 len=4096        │ 11      │ 7        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 35   │ pwrite64 t.db off=135168 len=4096        │ 12      │ 6        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 36   │ pwrite64 t.db off=139264 len=4096        │ 13      │ 5        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 37   │ pwrite64 t.db off=143360 len=4096        │ 14      │ 4        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 38   │ pwrite64 t.db off=147456 len=4096        │ 15      │ 3        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 39   │ pwrite64 t.db off=151552 len=4096        │ 16      │ 2        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 40   │ pwrite64 t.db off=155648 len=4096        │ 17      │ 1        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 41   │ unlink t.db-journal                      │ 18      │ 0        │ 29240 │ true  │ no-sync │ 0xffffffff │ b       │
│ 42   │ open marker-end                          │ 18      │ 0        │       │       │         │            │ A       │
└──────┴──────────────────────────────────────────┴─────────┴──────────┴───────┴───────┴─────────┴────────────┴─────────┘

D10. The outcome of every kill, in commit order. B = pre-step exact bytes,
     b = pre-step except reused freelist leaves, A = post-step exact bytes,
     ? = anything else. The last B or b is the kill on entry to the unlink.
┌─────────────┬─────────────────────────────────────────────────┐
│ synchronous │             outcome_per_kill_point              │
├─────────────┼─────────────────────────────────────────────────┤
│ FULL        │ BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA │
│ NORMAL      │ BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA  │
│ OFF         │ BBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbA      │
└─────────────┴─────────────────────────────────────────────────┘
┌─────────────┬─────────────┬───────────────┬──────────────┬─────────┬──────────────┬───────────────┬─────────────┐
│ synchronous │ B pre exact │ b pre logical │ A post exact │ ? other │ missed_kills │ integrity_bad │ not_covered │
├─────────────┼─────────────┼───────────────┼──────────────┼─────────┼──────────────┼───────────────┼─────────────┤
│ FULL        │ 32          │ 14            │ 1            │ 0       │ 0            │ 0             │ 0           │
│ NORMAL      │ 31          │ 14            │ 1            │ 0       │ 0            │ 0             │ 0           │
│ OFF         │ 28          │ 13            │ 1            │ 0       │ 0            │ 0             │ 0           │
└─────────────┴─────────────┴───────────────┴──────────────┴─────────┴──────────────┴───────────────┴─────────────┘

── D2. A step whose pre-step file has no freelist: step 1 ─────────────

Step 1 starts from the file after step 0, which has no freelist, so there
is no b: every reopen gives the pre-step or post-step bytes exactly.
┌──────┬─────────────────────────────────────┬────────┬─────────┬─────────┬─────────┬─────────┐
│ step │               recipe                │ schema │ inserts │ updates │ deletes │ indexes │
├──────┼─────────────────────────────────────┼────────┼─────────┼─────────┼─────────┼─────────┤
│ 1    │ seed=1 steps=30 journal_mode=delete │ 0      │ 65      │ 6       │ 0       │ 0       │
└──────┴─────────────────────────────────────┴────────┴─────────┴─────────┴─────────┴─────────┘
┌───────────┬─────────────┬───────┬──────────────────┬─────────────────┐
│   state   │ recipe_step │ pages │  sha256_prefix   │ freelist_leaves │
├───────────┼─────────────┼───────┼──────────────────┼─────────────────┤
│ pre-step  │ 0           │ 4     │ e551bbfca2c05cd9 │                 │
│ post-step │ 1           │ 22    │ 6207fb74284d25e8 │                 │
└───────────┴─────────────┴───────┴──────────────────┴─────────────────┘
┌─────────────┬───────┬───────────────┬───────────┬───────┬──────────────────────────────────┐
│ synchronous │ pages │ record_writes │ db_writes │ syncs │           sync_targets           │
├─────────────┼───────┼───────────────┼───────────┼───────┼──────────────────────────────────┤
│ FULL        │ 4     │ 12            │ 22        │ 4     │ t.db-journal . t.db-journal t.db │
│ NORMAL      │ 4     │ 12            │ 22        │ 3     │ t.db-journal . t.db              │
│ OFF         │ 4     │ 12            │ 22        │ 0     │                                  │
└─────────────┴───────┴───────────────┴───────────┴───────┴──────────────────────────────────┘
┌───┬────────────────────┬────────────────────┬────────────────────┐
│ i │        FULL        │       NORMAL       │        OFF         │
├───┼────────────────────┼────────────────────┼────────────────────┤
│ 1 │ journal-header     │ journal-header     │ journal-header     │
│ 2 │ journal-record x12 │ journal-record x12 │ journal-record x12 │
│ 3 │ journal-sync       │ journal-count      │ db-page x22        │
│ 4 │ dir-sync           │ journal-sync       │ journal-unlink     │
│ 5 │ journal-count      │ dir-sync           │                    │
│ 6 │ journal-sync       │ db-page x22        │                    │
│ 7 │ db-page x22        │ db-sync            │                    │
│ 8 │ db-sync            │ journal-unlink     │                    │
│ 9 │ journal-unlink     │                    │                    │
└───┴────────────────────┴────────────────────┴────────────────────┘
┌─────────────┬────────────────────────────────────────────┐
│ synchronous │           outcome_per_kill_point           │
├─────────────┼────────────────────────────────────────────┤
│ FULL        │ BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA │
│ NORMAL      │ BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA  │
│ OFF         │ BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA      │
└─────────────┴────────────────────────────────────────────┘
┌─────────────┬─────────────┬───────────────┬──────────────┬─────────┬──────────────┬───────────────┐
│ synchronous │ B pre exact │ b pre logical │ A post exact │ ? other │ missed_kills │ integrity_bad │
├─────────────┼─────────────┼───────────────┼──────────────┼─────────┼──────────────┼───────────────┤
│ FULL        │ 41          │ 0             │ 1            │ 0       │ 0            │ 0             │
│ NORMAL      │ 40          │ 0             │ 1            │ 0       │ 0            │ 0             │
│ OFF         │ 36          │ 0             │ 1            │ 0       │ 0            │ 0             │
└─────────────┴─────────────┴───────────────┴──────────────┴─────────┴──────────────┴───────────────┘

── E. Losing the hot journal (step 3, synchronous=FULL) ───────────────

E1. Two crashes: in the middle of the database writes, and on entry to the
    database sync, when every page is written but the journal is still hot.
┌────────────────┬───────────────────────────────────┬──────────┬────────┬───────────┬───────┬─────────┬──────────┐
│     crash      │             killed_on             │ write_no │ writes │  db_file  │ pages │ chg_pre │ chg_post │
├────────────────┼───────────────────────────────────┼──────────┼────────┼───────────┼───────┼─────────┼──────────┤
│ mid-write      │ pwrite64 t.db off=122880 len=4096 │ 10       │ 18     │ torn      │ 30    │ 9       │ 9        │
│ before db sync │ sync t.db                         │          │ 18     │ post-step │ 39    │ 18      │ 0        │
└────────────────┴───────────────────────────────────┴──────────┴────────┴───────────┴───────┴─────────┴──────────┘

The hot journal beside the mid-write crash:
┌───────────────┬────────────┬──────┬────────┬──────────────┬───────────────┬─────────┬──────────────────┐
│ journal_bytes │ first_byte │ hot  │ header │ record_count │ initial_pages │ records │   record_pages   │
├───────────────┼────────────┼──────┼────────┼──────────────┼───────────────┼─────────┼──────────────────┤
│ 29240         │ 0xd9       │ true │ synced │ 7            │ 29            │ 7       │ 2 1 20 3 27 4 26 │
└───────────────┴────────────┴──────┴────────┴──────────────┴───────────────┴─────────┴──────────────────┘

E2. What each file holds when opened. Without its journal the torn file is
    unreadable, and a copy of the file alone is the same damage. Copied with
    its journal, it rolls back. Deleting the journal after the last database
    write commits a transaction whose COMMIT never returned.
┌─────────────────────────────────┬───────────┬──────────────┬──────────┬───────────┬────────┐
│            scenario             │ integrity │ user_version │ accounts │ documents │ events │
├─────────────────────────────────┼───────────┼──────────────┼──────────┼───────────┼────────┤
│ pre-step file (step 2)          │ ok        │ 2            │ 15       │ 7         │ 108    │
│ post-step file (step 3)         │ ok        │ 3            │ 21       │ 10        │ 147    │
│ 1. -journal deleted             │           │              │          │           │        │
│ 2. copied without -journal      │           │              │          │           │        │
│ 2. copied with -journal         │ ok        │ 2            │ 15       │ 7         │ 108    │
│ 3. late crash, -journal deleted │ ok        │ 3            │ 21       │ 10        │ 147    │
└─────────────────────────────────┴───────────┴──────────────┴──────────┴───────────┴────────┘
┌─────────────────────────────────┬─────────────────────────────────┬────────────┐
│            scenario             │              state              │ differs_in │
├─────────────────────────────────┼─────────────────────────────────┼────────────┤
│ 1. -journal deleted             │ unreadable                      │            │
│ 2. copied without -journal      │ unreadable                      │            │
│ 2. copied with -journal         │ pre-step except freelist leaves │ 22         │
│ 3. late crash, -journal deleted │ post-step exact                 │            │
└─────────────────────────────────┴─────────────────────────────────┴────────────┘
┌────────────────────────────┬──────────────────────────────────────────────────────────────────────────────────┐
│          scenario          │                                    open_error                                    │
├────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────┤
│ 1. -journal deleted        │ open failed: open t.db: PRAGMA page_count: database disk image is malformed (11) │
│ 2. copied without -journal │ open failed: open t.db: PRAGMA page_count: database disk image is malformed (11) │
└────────────────────────────┴──────────────────────────────────────────────────────────────────────────────────┘

── F. The commit point in DELETE, TRUNCATE and PERSIST modes ──────────

F1. The same commit (step 3, synchronous=FULL) in each mode. Only the end
    differs: TRUNCATE and PERSIST finish the journal and sync it again.
┌────┬────────────────────┬────────────────────┬─────────────────────┐
│ i  │       delete       │      truncate      │       persist       │
├────┼────────────────────┼────────────────────┼─────────────────────┤
│ 1  │ journal-header     │ journal-header     │ journal-header      │
│ 2  │ journal-record x21 │ journal-record x21 │ journal-record x21  │
│ 3  │ journal-sync       │ journal-sync       │ journal-sync        │
│ 4  │ dir-sync           │ dir-sync           │ dir-sync            │
│ 5  │ journal-count      │ journal-count      │ journal-count       │
│ 6  │ journal-sync       │ journal-sync       │ journal-sync        │
│ 7  │ db-page x18        │ db-page x18        │ db-page x18         │
│ 8  │ db-sync            │ db-sync            │ db-sync             │
│ 9  │ journal-unlink     │ journal-truncate   │ journal-zero-header │
│ 10 │                    │ journal-sync       │ journal-sync        │
└────┴────────────────────┴────────────────────┴─────────────────────┘
┌──────────────┬───────┬───────────────┬───────────┬───────┬───────────────────────────────────────────────┐
│ journal_mode │ pages │ record_writes │ db_writes │ syncs │                 sync_targets                  │
├──────────────┼───────┼───────────────┼───────────┼───────┼───────────────────────────────────────────────┤
│ delete       │ 7     │ 21            │ 18        │ 4     │ t.db-journal . t.db-journal t.db              │
│ truncate     │ 7     │ 21            │ 18        │ 5     │ t.db-journal . t.db-journal t.db t.db-journal │
│ persist      │ 7     │ 21            │ 18        │ 5     │ t.db-journal . t.db-journal t.db t.db-journal │
└──────────────┴───────┴───────────────┴───────────┴───────┴───────────────────────────────────────────────┘

F2. A kill on entry to the commit-point syscall gives the pre-step state; a
    kill on the syscall after it gives the post-step state.
┌──────────────┬────────────────────────────────────┬────────────────────────────┬───────────────┬───────────────────┐
│ journal_mode │            commit_point            │      killed_on_entry       │   next_kind   │  killed_on_next   │
├──────────────┼────────────────────────────────────┼────────────────────────────┼───────────────┼───────────────────┤
│ delete       │ unlink t.db-journal                │ before (pre-step, logical) │ step-returned │ after (post-step) │
│ truncate     │ ftruncate t.db-journal len=0       │ before (pre-step, logical) │ journal-sync  │ after (post-step) │
│ persist      │ pwrite64 t.db-journal off=0 len=28 │ before (pre-step, logical) │ journal-sync  │ after (post-step) │
└──────────────┴────────────────────────────────────┴────────────────────────────┴───────────────┴───────────────────┘

F3. The -journal left beside the database after a clean commit. TRUNCATE
    leaves 0 bytes; PERSIST leaves the whole journal with a zeroed header.
    Neither is hot.
┌──────────────┬───────────────┬────────────┬───────┬────────┐
│ journal_mode │ journal_bytes │ first_byte │  hot  │ header │
├──────────────┼───────────────┼────────────┼───────┼────────┤
│ delete       │ no -journal   │            │       │        │
│ truncate     │ 0             │            │ false │        │
│ persist      │ 29240         │ 0x00       │ false │ zeroed │
└──────────────┴───────────────┴────────────┴───────┴────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  a new database starts in rollback-journal mode (journal_mode=delete) until something switches it
A  ✓  the file records rollback mode in header bytes 18 and 19 (value 1)
A  ✓  switching to WAL rewrites bytes 18 and 19 to 2, and the mode persists in the file
A  ✓  a file created with driver defaults commits its first transaction through a -journal (opened, written, synced, unlinked)
A  ✓  the switch to WAL is itself a rollback-mode commit: it writes page 1 through a -journal
A  ✓  after the switch, commits go to the -wal and no -journal is opened
A  ✓  a WAL database created by the platform opener (sqlitex.Open) is also born through one -journal commit, then never uses one
B  ✓  the pre-step and post-step files equal the recipe's hashes for steps 2 and 3
C  ✓  every journal record holds the page's original (pre-step) image with a valid checksum
C  ✓  before COMMIT (synchronous=FULL) the header's magic and record count are still zero, so the journal is not hot yet
C  ✓  the header records the database size before the transaction (29 pages)
C  ✓  a ROLLBACK by the live connection restores the pre-step bytes exactly (the reused freelist pages were never written to the file)
C  ✓  no -journal remains after ROLLBACK and close
D  ✓  step 3, journal_mode=delete, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink
D  ✓  step 3, journal_mode=delete, synchronous=FULL: 4 syncs per commit (journal, directory and database syncs counted)
D  ✓  step 3, journal_mode=delete, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
D  ✓  step 3, journal_mode=delete, synchronous=FULL: the traced commit leaves exactly the post-step bytes
D  ✓  synchronous=FULL: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes
D  ✓  step 3, synchronous=FULL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
D  ✓  step 3, synchronous=FULL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
D  ✓  step 3, synchronous=FULL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
D  ✓  step 3, synchronous=FULL: integrity_check is ok after every reopen
D  ✓  step 3, synchronous=FULL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves
D  ✓  step 3, journal_mode=delete, synchronous=NORMAL: order is journal-header, journal-record x21, journal-count, journal-sync, dir-sync, db-page x18, db-sync, journal-unlink
D  ✓  step 3, journal_mode=delete, synchronous=NORMAL: 3 syncs per commit (journal, directory and database syncs counted)
D  ✓  step 3, journal_mode=delete, synchronous=NORMAL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
D  ✓  step 3, journal_mode=delete, synchronous=NORMAL: the traced commit leaves exactly the post-step bytes
D  ✓  synchronous=NORMAL: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes
D  ✓  step 3, synchronous=NORMAL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
D  ✓  step 3, synchronous=NORMAL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
D  ✓  step 3, synchronous=NORMAL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
D  ✓  step 3, synchronous=NORMAL: integrity_check is ok after every reopen
D  ✓  step 3, synchronous=NORMAL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves
D  ✓  step 3, journal_mode=delete, synchronous=OFF: order is journal-header, journal-record x21, db-page x18, journal-unlink
D  ✓  step 3, journal_mode=delete, synchronous=OFF: 0 syncs per commit (journal, directory and database syncs counted)
D  ✓  step 3, journal_mode=delete, synchronous=OFF: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
D  ✓  step 3, journal_mode=delete, synchronous=OFF: the traced commit leaves exactly the post-step bytes
D  ✓  synchronous=OFF: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes
D  ✓  step 3, synchronous=OFF: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
D  ✓  step 3, synchronous=OFF: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
D  ✓  step 3, synchronous=OFF: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
D  ✓  step 3, synchronous=OFF: integrity_check is ok after every reopen
D  ✓  step 3, synchronous=OFF: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves
D2  ✓  the pre-step and post-step files equal the recipe's hashes for steps 0 and 1
D2  ✓  step 1, journal_mode=delete, synchronous=FULL: order is journal-header, journal-record x12, journal-sync, dir-sync, journal-count, journal-sync, db-page x22, db-sync, journal-unlink
D2  ✓  step 1, journal_mode=delete, synchronous=FULL: 4 syncs per commit (journal, directory and database syncs counted)
D2  ✓  step 1, journal_mode=delete, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
D2  ✓  step 1, journal_mode=delete, synchronous=FULL: the traced commit leaves exactly the post-step bytes
D2  ✓  step 1, synchronous=FULL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
D2  ✓  step 1, synchronous=FULL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
D2  ✓  step 1, synchronous=FULL: the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly
D2  ✓  step 1, synchronous=FULL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
D2  ✓  step 1, synchronous=FULL: integrity_check is ok after every reopen
D2  ✓  step 1, synchronous=FULL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves
D2  ✓  step 1, journal_mode=delete, synchronous=NORMAL: order is journal-header, journal-record x12, journal-count, journal-sync, dir-sync, db-page x22, db-sync, journal-unlink
D2  ✓  step 1, journal_mode=delete, synchronous=NORMAL: 3 syncs per commit (journal, directory and database syncs counted)
D2  ✓  step 1, journal_mode=delete, synchronous=NORMAL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
D2  ✓  step 1, journal_mode=delete, synchronous=NORMAL: the traced commit leaves exactly the post-step bytes
D2  ✓  step 1, synchronous=NORMAL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
D2  ✓  step 1, synchronous=NORMAL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
D2  ✓  step 1, synchronous=NORMAL: the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly
D2  ✓  step 1, synchronous=NORMAL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
D2  ✓  step 1, synchronous=NORMAL: integrity_check is ok after every reopen
D2  ✓  step 1, synchronous=NORMAL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves
D2  ✓  step 1, journal_mode=delete, synchronous=OFF: order is journal-header, journal-record x12, db-page x22, journal-unlink
D2  ✓  step 1, journal_mode=delete, synchronous=OFF: 0 syncs per commit (journal, directory and database syncs counted)
D2  ✓  step 1, journal_mode=delete, synchronous=OFF: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
D2  ✓  step 1, journal_mode=delete, synchronous=OFF: the traced commit leaves exactly the post-step bytes
D2  ✓  step 1, synchronous=OFF: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
D2  ✓  step 1, synchronous=OFF: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
D2  ✓  step 1, synchronous=OFF: the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly
D2  ✓  step 1, synchronous=OFF: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
D2  ✓  step 1, synchronous=OFF: integrity_check is ok after every reopen
D2  ✓  step 1, synchronous=OFF: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves
E  ✓  deleting the hot journal before reopening leaves a damaged database that is neither the pre-step nor the post-step state
E  ✓  a copy made without the -journal is the same damaged database
E  ✓  the same crashed file copied together with its -journal rolls back to pre-step
E  ✓  deleting the journal after the last database write turns a commit that never returned into a committed one (post-step)
F  ✓  step 3, journal_mode=delete, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink
F  ✓  step 3, journal_mode=delete, synchronous=FULL: 4 syncs per commit (journal, directory and database syncs counted)
F  ✓  step 3, journal_mode=delete, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
F  ✓  step 3, journal_mode=delete, synchronous=FULL: the traced commit leaves exactly the post-step bytes
F  ✓  delete: a kill on entry to the commit point gives pre-step, the next kill gives post-step
F  ✓  step 3, journal_mode=truncate, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-truncate, journal-sync
F  ✓  step 3, journal_mode=truncate, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
F  ✓  step 3, journal_mode=truncate, synchronous=FULL: the traced commit leaves exactly the post-step bytes
F  ✓  truncate: a kill on entry to the commit point gives pre-step, the next kill gives post-step
F  ✓  truncate: the journal file stays beside the database after a clean commit, and it is not hot
F  ✓  step 3, journal_mode=persist, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-zero-header, journal-sync
F  ✓  step 3, journal_mode=persist, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
F  ✓  step 3, journal_mode=persist, synchronous=FULL: the traced commit leaves exactly the post-step bytes
F  ✓  persist: a kill on entry to the commit point gives pre-step, the next kill gives post-step
F  ✓  persist: the journal file stays beside the database after a clean commit, and it is not hot

93 of 93 claims hold

Experiment verdict: ATOMIC COMMIT OK
```

#### Reading the evidence (Parts A–D2)

Parts E and F are analyzed in Section 6 alongside the hot-journal failure cases. Parts A through D2 establish:

- **A1–A3 (Part A: where a WAL-default platform meets rollback mode).** Opening a new path with plain `sql.Open("sqlite", path)` reports `journal_mode=delete`, and its first `CREATE TABLE` writes `1,1` into header bytes 18–19. Running `PRAGMA journal_mode=wal` rewrites bytes 18–19 to `2,2` so a fresh connection reports `journal_mode=wal`. The three traced children show that not only does a driver-default `CREATE TABLE` go through `plain.db-journal`, but `PRAGMA journal_mode=wal` on an empty file (`switch.db-journal`) and `sqlitex.Open` with `journal_mode=wal` (`platform.db-journal`) each run the exact same six `-journal` syscalls (`open`, `pwrite64 off=0 len=512`, `sync`, `pwrite64 off=0 len=12`, `sync`, `unlink`). The journal holds zero page records because page 1 is past the 0-page initial file size (`pager_write`, `src/pager.c:6139-6147`). Only *after* page 1 records format version `2` do commits move to `-wal` (no `-journal` syscalls; `5` WAL writes for `sqlitex.Open`'s `application_id`/`user_version` commit and `4` for the subsequent `CREATE TABLE`).
- **B1–B2 (Part B: the recipe step).** Step 3 of the rollback-mode recipe (`seed=1`) executes 53 inserts, 5 updates, and 5 deletes, growing the database from 29 pages (`b8a0bd0796442099`, with freelist leaf `22`) to 39 pages (`0ef90bd0e64af1e3`).
- **C1–C3 (Part C: the live journal before `COMMIT`, portable).** After executing every statement of step 3 without committing (the transaction fits in `cache_size=-2000`, so `spill=0`), `internal/journalfile` reads `t.db-journal` (29,240 bytes = 512-byte header sector + 7 × 4,104-byte records). Its first byte is `0x00` (not hot, header `not-yet-synced`), initial size is `29` pages, sector size is `512` (the unix VFS advertises `SQLITE_IOCAP_POWERSAFE_OVERWRITE`, so the pager uses 512-byte sectors), and all 7 records (pages `2 1 20 3 27 4 26`) pass their checksums and match the exact pre-step pages byte for byte. Page `22` (the reused freelist leaf) is absent from the journal. Issuing `ROLLBACK` on the live connection restores `pre-step exact bytes` because page 22 was modified only in memory and never written to disk.
- **D1 and D3–D7, D10 (Part D: step 3 at `synchronous=FULL`).** The baseline trace (D1) records 70 syscalls (examined line by line in Section 4): 7 journaled pages (21 record writes, verified as `(4, 4096, 4)` triples), 18 database page writes, and 4 syncs (`t.db-journal . t.db-journal t.db`). The twin check confirms the pinned C `sqlite3` CLI issues the exact same 46 writes, syncs, and unlink in the same order and produces identical post-step bytes. Killing on entry to each of the 47 kill points yields:
  - **Kills 1–25** (through entry to `journal-count` at offset 0 len 12): database untouched; journal first byte is `0x00` (cold); SQLite ignores the journal on reopen, leaving pre-step exact bytes (`B`) and a harmless cold `-journal` on disk.
  - **Kill 26** (entry to second `journal-sync`): `journal-count` has written magic `0xd9...` and record count `7`, turning the journal hot; database is still untouched; SQLite rolls back the 7 pages and deletes `-journal`, leaving pre-step exact bytes (`B`).
  - **Kills 27–32** (database writes of pages 1, 2, 3, 4, 20, and entry to page 22 at offset `86016`): database is torn (1 to 5 pages differ from pre-step); hot journal restores all 7 original pages; pre-step exact bytes (`B`).
  - **Kills 33–46** (after page 22 is written through entry to `unlink t.db-journal`): database is torn (or post-step bytes at kills 45–46); hot journal restores the 7 original pages and truncates to 29 pages; reopened database equals pre-step except page `22` (`b`), integrity `ok`, and `VACUUM INTO` matches pre-step's canonical hash.
  - **Kill 47** (entry to `open marker-end`, immediately after `unlink`): no journal remains; reopened database equals post-step exact bytes (`A`).
- **D2–D4, D8–D10 (Part D: `NORMAL`, `OFF`, and the summary strip).**
  - `NORMAL` (46 kill points, 3 syncs `t.db-journal . t.db`, 45 twin-check ops identical to C CLI): moves `journal-count` ahead of the single journal sync, so the journal becomes hot at kill 24 (entry to `journal-sync`) rather than kill 26. Outcomes: 31 pre-step exact, 14 pre-step except page 22, 1 post-step exact.
  - `OFF` (42 kill points, 0 syncs, 41 twin-check ops identical to C CLI): writes magic + `0xffffffff` in the initial 512-byte header, so the journal is hot from kill 2 onward. Even with zero syncs, every process kill reopens to pre-step (28 exact, 13 except page 22) or post-step (1 exact) because completed writes survive a process crash in the OS kernel buffer cache.
  - The D10 summary shows the single flip from `B`/`b` to `A` right after `unlink`:
    - `FULL`: 32 `B`, 14 `b`, 1 `A`
    - `NORMAL`: 31 `B`, 14 `b`, 1 `A`
    - `OFF`: 28 `B`, 13 `b`, 1 `A`
- **Section D2 (Part D2: step 1 with no freelist).** Pre-step (after step 0) has 4 pages and no freelist leaves; step 1 grows it to 22 pages (4 journaled pages, 22 database writes—18 of them appended past page 4 and undone by truncation). Across all 42 (`FULL`), 41 (`NORMAL`), and 37 (`OFF`) kill points, every reopened database matches pre-step or post-step **to the exact byte** (`41 B + 1 A`, `40 B + 1 A`, `36 B + 1 A`, zero `b` outcomes).

---

## 4. Timeline

One non-spilling rollback-mode commit of recipe step 3 (`synchronous=FULL`, `journal_mode=delete`, Experiment 4.1 Part D, D1) crosses every pager state in 70 traced syscalls on the database directory:

| Syscall # | Syscalls in the trace | Pager action | Pager state / lock |
| --- | --- | --- | --- |
| 1–3 | `fcntl F_RDLCK 1073741824 len=1`, `F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824` | Acquire `SHARED` lock (through the `PENDING` byte) | `OPEN -> READER` |
| 4 | `pread64 t.db off=24 len=16` | Read header change counter to check cache validity | `READER` |
| 5 | `fcntl F_WRLCK 1073741825 len=1` | Acquire `RESERVED` lock (`BEGIN IMMEDIATE`) | `-> WRITER_LOCKED` |
| 6 | `pread64 t.db off=4096 len=4096` | Read page 2 before modifying it | `WRITER_LOCKED` |
| 7–9 | `open t.db-journal`, `fcntl F_SETFD` (modernc), `pwrite64 off=0 len=512` | Create journal; write header with zeroed magic and count (`writeJournalHdr`) | `-> WRITER_CACHEMOD` |
| 10–36 | 21 × `pwrite64 t.db-journal` (4 B / 4,096 B / 4 B per page) interleaved with `pread64 t.db` | Journal original images of pages `2, 1, 20, 3, 27, 4, 26` (`pagerAddPageToRollbackJournal`) | `WRITER_CACHEMOD` |
| 37–38 | `fcntl F_WRLCK 1073741824 len=1`, `F_WRLCK 1073741826 len=510` | Upgrade `RESERVED -> PENDING -> EXCLUSIVE` for commit | `WRITER_CACHEMOD` |
| 39 | `pread64 t.db-journal off=29696 len=8 = 0` | Probe next sector boundary for any stale header (`syncJournal`) | `WRITER_CACHEMOD` |
| 40 | `fsync t.db-journal` | **Journal sync 1 (`FULL` only):** flush all 7 page records | `WRITER_CACHEMOD` |
| 41–44 | `open .`, `fcntl F_SETFD`, `fsync .`, `close .` | **Directory sync:** make the new `t.db-journal` directory entry durable | `WRITER_CACHEMOD` |
| 45 | `pwrite64 t.db-journal off=0 len=12` | Write 8-byte magic + 4-byte count (`7`): **journal becomes hot** | `WRITER_CACHEMOD` |
| 46 | `fsync t.db-journal` | **Journal sync 2:** make the hot header durable | `-> WRITER_DBMOD` |
| 47–64 | 18 × `pwrite64 t.db` at ascending offsets `0` … `155648` | Write dirty pages `1–4, 20, 22, 26–39` in sorted order (`pager_write_pagelist`) | `WRITER_DBMOD` |
| 65 | `fsync t.db` | **Database sync:** flush all 18 written pages to disk (`sqlite3PagerSync`) | `-> WRITER_FINISHED` |
| 66–67 | `close t.db-journal`, `unlink t.db-journal` | **Commit point (`DELETE` mode):** remove the hot journal (`pager_end_transaction`) | `-> READER` |
| 68–70 | `fcntl F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=2`, `F_UNLCK 0 len=0` | Downgrade to `SHARED`, then release all locks | `-> OPEN` |

How other settings and workloads alter this timeline:

- **`synchronous=NORMAL` (69 calls, 3 syncs; Experiment 4.1 Part D).** Drops call #40 (the first journal sync). Call #45 (`journal-count`, 12 bytes at offset 0) runs right after the stale-header probe, followed by the single `fsync t.db-journal` and `fsync .`.
- **`synchronous=OFF` (61 calls, 0 syncs; Experiment 4.1 Part D).** Drops all four syncs and the stale-header probe. Call #9 writes the magic and `0xffffffff` immediately, so call #45 disappears too.
- **`journal_mode=truncate` and `persist` at `FULL` (5 syncs; Experiment 4.1 Part F).** Replace calls #66–67 (`close`, `unlink`) with either `ftruncate t.db-journal len=0` (`TRUNCATE`) or `pwrite64 t.db-journal off=0 len=28` (`PERSIST`), followed by a fifth sync (`fsync t.db-journal`) so the journal inactivation reaches disk (`pager_end_transaction`, `src/pager.c:2090-2105`).
- **Spilling transactions (`pagerStress`, `src/pager.c:4655-4727`; `JRN-04`, `RCM-03a`).** When dirty pages exceed `cache_size`, `syncJournal` runs before `COMMIT`—writing the `0xd9` magic and record count, syncing the journal, opening a new sector-aligned header segment, and writing dirty pages to `t.db` early. Journal records and database writes then interleave across segments and sync counts rise above 4 (`FULL`) / 3 (`NORMAL`).
- **Shrinking commits (`pager_end_transaction`, `src/pager.c:2155-2164`; `RCM-06`).** When a transaction shrinks the database (for example, `DELETE` under `auto_vacuum=FULL`), `ftruncate t.db` runs *after* call #67 (`unlink t.db-journal`) and before locks are released at #68–70.

Crash outcome by timeline window for a non-spilling, non-shrinking commit (`FULL`, `DELETE`):

- **Before #45 (`journal-count`):** `-journal` is cold (first byte `0x00`), `t.db` is untouched -> reopen ignores the journal -> **pre-step exact**.
- **From #45 through #67 (before `unlink` completes):** `-journal` is hot (`0xd9...`, count `7`), `t.db` is untouched, torn, or post-step -> reopen replays the 7 pages and truncates to 29 pages -> **pre-step** (exact before page 22 is written at #52; pre-step except freelist leaf 22 from #52 through #67).
- **After #67 (`unlink` completed):** no `-journal` exists, `t.db` holds all 18 new pages -> reopen sees **post-step exact**.

---

## 5. Lab

### 5.1 Shared package: `internal/journalfile`

Experiment 4.1 needs to inspect `-journal` files *before* SQLite reopens them—decoding header states, checking record checksums, and predicting what `pager_playback` will restore—without linking SQLite into the reader. `internal/journalfile` is a read-only Go parser that never modifies any file.

<!-- include: ../internal/journalfile/journalfile.go -->
```go
// Package journalfile implements a read-only inspector for SQLite rollback
// journal (-journal) files, using plain Go with no SQLite dependency.
//
// It decodes journal headers, page records, checksums, and super-journal
// trailers, and predicts which pages SQLite would restore during hot-journal
// playback. It never writes to a database or replays a journal itself.
package journalfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"slices"
)

// Magic is the 8-byte signature that identifies a live rollback journal header
// (aJournalMagic in src/pager.c:757).
var Magic = [8]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// Journal format sizes and bounds matching src/pager.c:1449-1693.
const (
	// HeaderFieldsSize is the byte length of the six big-endian fields at the
	// start of a journal header (8-byte magic + five 4-byte integers).
	HeaderFieldsSize = 28
	// NoSyncRecordCount (0xffffffff) in a header's record-count field tells
	// SQLite to infer the record count from the file size (src/pager.c:1499).
	NoSyncRecordCount = 0xffffffff
	// RecordOverhead is the 8 bytes wrapping each journaled page image:
	// 4-byte page number before the image and 4-byte checksum after it
	// (pagerAddPageToRollbackJournal, src/pager.c:6062-6067).
	RecordOverhead     = 8
	pendingByte        = 0x40000000
	minSectorSize      = 32
	maxSectorSize      = 0x10000
	minPageSize        = 512
	maxPageSize        = 65536
	superJournalFooter = 16
	// maxSuperJournalLen matches unix MAX_PATHNAME (512, src/os_unix.c:180);
	// readSuperJournal rejects lengths >= 1 + mxPathname (src/pager.c:1315,2891).
	maxSuperJournalLen = 512
)

// HeaderState classifies the 28-byte header at the start of a journal segment.
type HeaderState int

// Rollback journal header states recognized by parseHeader.
const (
	// HeaderSynced indicates a header carrying Magic and an explicit record
	// count, written by syncJournal (src/pager.c:4377-4410).
	HeaderSynced HeaderState = iota
	// HeaderNoSync indicates a header carrying Magic and NoSyncRecordCount,
	// written directly by writeJournalHdr when synchronous=OFF,
	// journal_mode=MEMORY, or SAFE_APPEND is active (src/pager.c:1495-1499).
	HeaderNoSync
	// HeaderNotYetSynced indicates a header whose first 12 bytes (magic and
	// record count) are still zero while the remaining fields are populated,
	// written by writeJournalHdr before syncJournal runs (src/pager.c:1500-1502).
	HeaderNotYetSynced
	// HeaderZeroed indicates all 28 header bytes are zero, as left after commit
	// in PERSIST mode by zeroJournalHdr (src/pager.c:1399-1432).
	HeaderZeroed
	// HeaderInvalid indicates bytes that are neither valid magic, nor zeroed,
	// nor a valid sector/page-size geometry (readJournalHdr, src/pager.c:1650-1672).
	HeaderInvalid
	// HeaderMissing indicates fewer than HeaderFieldsSize bytes or fewer than
	// one full first sector (readJournalHdr returns SQLITE_DONE before reading
	// the magic or truncating the database, src/pager.c:1607-1610).
	HeaderMissing
)

// String returns a short human-readable name for the header state.
func (s HeaderState) String() string {
	switch s {
	case HeaderSynced:
		return "synced"
	case HeaderNoSync:
		return "no-sync"
	case HeaderNotYetSynced:
		return "not-yet-synced"
	case HeaderZeroed:
		return "zeroed"
	case HeaderInvalid:
		return "invalid"
	case HeaderMissing:
		return "missing"
	}
	return fmt.Sprintf("HeaderState(%d)", int(s))
}

// Header holds the decoded fields of one sector-aligned rollback journal header
// (writeJournalHdr and readJournalHdr, src/pager.c:1449-1693).
type Header struct {
	Offset       int64
	State        HeaderState
	MagicOK      bool
	RecordCount  uint32
	Nonce        uint32
	InitialPages uint32
	SectorSize   uint32
	PageSize     uint32
}

// CountsRecords reports whether the header carries a valid magic and an
// explicit record count rather than the NoSyncRecordCount sentinel.
func (h Header) CountsRecords() bool {
	return h.MagicOK && h.RecordCount != NoSyncRecordCount
}

// Record holds one journaled page record: the 4-byte page number, the original
// page image, the stored 4-byte checksum, and the checksum recomputed from the
// segment's nonce and page bytes (src/pager.c:6037-6085).
type Record struct {
	Offset           int64
	Page             uint32
	StoredChecksum   uint32
	ComputedChecksum uint32
	Image            []byte
}

// ChecksumOK reports whether the record's stored checksum matches the checksum
// computed by Checksum.
func (r Record) ChecksumOK() bool {
	return r.StoredChecksum == r.ComputedChecksum
}

// Segment pairs one journal header with the complete page records that follow
// it. A transaction that spills dirty pages mid-transaction writes multiple
// segments, each starting on a sector boundary (syncJournal, src/pager.c:4423-4427).
type Segment struct {
	Header  Header
	Records []Record
}

// Journal is the decoded structure of a rollback journal file.
type Journal struct {
	Size         int64
	FirstByte    byte
	Segments     []Segment
	SuperJournal string
	TrailingByte int64
}

// PlaybackOutcome predicts what SQLite's pager_playback (src/pager.c:2864-3052)
// will do if it reopens the database beside this journal.
type PlaybackOutcome struct {
	Hot          bool
	TruncateTo   uint32
	Pages        []uint32
	SkippedPages []uint32
	StopReason   string
}

// ReadFile reads path from disk and parses it as a rollback journal.
func ReadFile(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes raw rollback-journal bytes into segments, records, and any
// trailing super-journal name without modifying the input slice.
func Parse(data []byte) (*Journal, error) {
	j := &Journal{Size: int64(len(data))}
	if len(data) > 0 {
		j.FirstByte = data[0]
	}
	end := int64(len(data))
	// Check for a multi-database super-journal footer at EOF first so its bytes
	// are excluded from record parsing (pager_playback, src/pager.c:2891).
	if name, start, ok := superJournalName(data); ok {
		j.SuperJournal = name
		end = start
	}
	if end < HeaderFieldsSize {
		if end > 0 {
			j.Segments = append(j.Segments, Segment{Header: Header{State: HeaderMissing}})
			j.TrailingByte = end
		}
		return j, nil
	}
	first := parseHeader(data, 0)
	if first.State == HeaderZeroed || first.State == HeaderInvalid {
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	// Geometry (sector size and page size) comes only from the first header;
	// SQLite stops playback if either is out of range or not a power of two
	// (readJournalHdr, src/pager.c:1650-1672).
	if !validGeometry(first.SectorSize, first.PageSize) {
		first.State = HeaderInvalid
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	sector := int64(first.SectorSize)
	// readJournalHdr returns SQLITE_DONE if journalOff+sectorSize > journalSize
	// (src/pager.c:1607-1610), before reading the magic or truncating the
	// database; treat a sub-sector file as missing so TrailingByte stays >= 0.
	if end < sector {
		first.State = HeaderMissing
		j.Segments = append(j.Segments, Segment{Header: first})
		j.TrailingByte = end
		return j, nil
	}
	pageSize := int64(first.PageSize)
	recordSize := pageSize + RecordOverhead
	offset := int64(0)
	header := first
	for {
		segment := Segment{Header: header}
		// Records begin one sector after the header (writeJournalHdr pads the
		// header out to a full sector, src/pager.c:1563-1568).
		position := offset + sector
		limit := int64(-1)
		if header.CountsRecords() {
			limit = int64(header.RecordCount)
		}
		for (limit < 0 || int64(len(segment.Records)) < limit) && position+recordSize <= end {
			segment.Records = append(segment.Records, parseRecord(data, position, pageSize, header.Nonce))
			position += recordSize
		}
		j.Segments = append(j.Segments, segment)
		if limit < 0 || int64(len(segment.Records)) < limit {
			j.TrailingByte = end - position
			break
		}
		// A subsequent segment header starts on the next sector boundary
		// (journalHdrOffset, src/pager.c:1366-1376).
		next := roundUp(position, sector)
		if next+sector > end {
			j.TrailingByte = end - position
			break
		}
		header = parseHeader(data, next)
		if header.State != HeaderSynced && header.State != HeaderNotYetSynced && header.State != HeaderNoSync {
			j.TrailingByte = end - position
			break
		}
		header.SectorSize = first.SectorSize
		header.PageSize = first.PageSize
		offset = next
	}
	return j, nil
}

func parseHeader(data []byte, offset int64) Header {
	h := Header{Offset: offset}
	fields := data[offset : offset+HeaderFieldsSize]
	h.MagicOK = bytes.Equal(fields[:8], Magic[:])
	h.RecordCount = binary.BigEndian.Uint32(fields[8:12])
	h.Nonce = binary.BigEndian.Uint32(fields[12:16])
	h.InitialPages = binary.BigEndian.Uint32(fields[16:20])
	h.SectorSize = binary.BigEndian.Uint32(fields[20:24])
	h.PageSize = binary.BigEndian.Uint32(fields[24:28])
	switch {
	case allZero(fields):
		h.State = HeaderZeroed
	case h.MagicOK && h.RecordCount == NoSyncRecordCount:
		h.State = HeaderNoSync
	case h.MagicOK:
		h.State = HeaderSynced
	case allZero(fields[:12]):
		h.State = HeaderNotYetSynced
	default:
		h.State = HeaderInvalid
	}
	return h
}

func parseRecord(data []byte, offset, pageSize int64, nonce uint32) Record {
	image := data[offset+4 : offset+4+pageSize]
	return Record{
		Offset:           offset,
		Page:             binary.BigEndian.Uint32(data[offset : offset+4]),
		StoredChecksum:   binary.BigEndian.Uint32(data[offset+4+pageSize : offset+8+pageSize]),
		ComputedChecksum: Checksum(nonce, image),
		Image:            image,
	}
}

// Checksum computes SQLite's rollback-journal page checksum: starting from
// nonce, it adds every 200th byte stepping down from len(image)-200 while the
// index is strictly positive (pager_cksum, src/pager.c:2251-2259).
func Checksum(nonce uint32, image []byte) uint32 {
	sum := nonce
	for i := len(image) - 200; i > 0; i -= 200 {
		sum += uint32(image[i])
	}
	return sum
}

// SuperJournalPage returns the reserved page number (PAGER_SJ_PGNO) that marks
// the start of a super-journal name record in the journal (the lock-byte page
// at byte offset 0x40000000, which can never be a real database page).
func SuperJournalPage(pageSize uint32) uint32 {
	if pageSize == 0 {
		return 0
	}
	return pendingByte/pageSize + 1
}

// superJournalName detects the optional multi-database super-journal footer
// written by writeSuperJournal (src/pager.c:1703-1737) and validated by
// readSuperJournal (src/pager.c:1303-1349): [4-byte PAGER_SJ_PGNO][name]
// [4-byte len][4-byte sum][8-byte Magic]. Names starting with NUL or longer
// than maxSuperJournalLen are ignored, and the name stops at its first NUL.
func superJournalName(data []byte) (string, int64, bool) {
	size := int64(len(data))
	if size < superJournalFooter+4 || !bytes.Equal(data[size-8:], Magic[:]) {
		return "", 0, false
	}
	length := int64(binary.BigEndian.Uint32(data[size-16 : size-12]))
	stored := binary.BigEndian.Uint32(data[size-12 : size-8])
	nameStart := size - superJournalFooter - length
	if length <= 0 || length > maxSuperJournalLen || nameStart < 4 {
		return "", 0, false
	}
	name := data[nameStart : size-superJournalFooter]
	if superJournalChecksum(name, cCharIsSigned) != stored || name[0] == 0 {
		return "", 0, false
	}
	if nul := bytes.IndexByte(name, 0); nul >= 0 {
		name = name[:nul]
	}
	return string(name), nameStart - 4, true
}

// cCharIsSigned mirrors the target platform's C char signedness because
// writeSuperJournal and readSuperJournal sum char elements directly
// (src/pager.c:1735-1737, 1333-1335). C char is unsigned on Linux ARM/ARM64/
// PPC64/RISC-V/s390x and signed on amd64 and macOS.
var cCharIsSigned = !(runtime.GOOS == "linux" && slices.Contains([]string{"arm", "arm64", "ppc64", "ppc64le", "riscv64", "s390x"}, runtime.GOARCH))

// superJournalChecksum sums name bytes as signed or unsigned C chars to match
// writeSuperJournal (src/pager.c:1735-1737) and readSuperJournal (1333-1335).
func superJournalChecksum(name []byte, signedChars bool) uint32 {
	var sum uint32
	for _, b := range name {
		if signedChars {
			sum += uint32(int8(b))
		} else {
			sum += uint32(b)
		}
	}
	return sum
}

func validGeometry(sector, pageSize uint32) bool {
	powerOfTwo := func(v uint32) bool { return v != 0 && v&(v-1) == 0 }
	return sector >= minSectorSize && sector <= maxSectorSize && powerOfTwo(sector) &&
		pageSize >= minPageSize && pageSize <= maxPageSize && powerOfTwo(pageSize)
}

func roundUp(offset, unit int64) int64 {
	if offset == 0 {
		return 0
	}
	return ((offset-1)/unit + 1) * unit
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// LooksHot reports the byte-level half of hasHotJournal (src/pager.c:5244-5251):
// a journal is hot only if it is non-empty and its first byte is non-zero.
// The lock and database-size checks in hasHotJournal depend on external state.
func (j *Journal) LooksHot() bool {
	return j.Size > 0 && j.FirstByte != 0
}

// RecordsPresent returns the total count of complete page records across all
// parsed segments.
func (j *Journal) RecordsPresent() int {
	n := 0
	for _, s := range j.Segments {
		n += len(s.Records)
	}
	return n
}

// ChecksumFailures returns the number of complete records whose stored checksum
// disagrees with Checksum.
func (j *Journal) ChecksumFailures() int {
	n := 0
	for _, s := range j.Segments {
		for _, r := range s.Records {
			if !r.ChecksumOK() {
				n++
			}
		}
	}
	return n
}

// Playback simulates the read loop of pager_playback (src/pager.c:2864-3052)
// and pager_playback_one_page (src/pager.c:2298-2500) to predict which pages
// SQLite will restore and what page count it will truncate the database to.
// When the journal names a super-journal, pager_playback skips rollback unless
// that super-journal file still exists on disk (src/pager.c:2891-2897); because
// Playback sees only the journal bytes, it refuses to predict an unconditional
// rollback and names the super-journal in StopReason.
func (j *Journal) Playback() PlaybackOutcome {
	outcome := PlaybackOutcome{Hot: j.LooksHot()}
	if outcome.Hot && j.SuperJournal != "" {
		outcome.StopReason = fmt.Sprintf("names super-journal %q: nothing is played back unless that file exists (see PlaybackIfSuperJournalExists)", j.SuperJournal)
		return outcome
	}
	return j.playback(outcome)
}

// PlaybackIfSuperJournalExists returns the playback prediction assuming any
// named super-journal file exists on disk (the branch of pager_playback that
// proceeds past src/pager.c:2895).
func (j *Journal) PlaybackIfSuperJournalExists() PlaybackOutcome {
	return j.playback(PlaybackOutcome{Hot: j.LooksHot()})
}

// playback runs the shared segment-and-record simulation used by Playback and
// PlaybackIfSuperJournalExists.
func (j *Journal) playback(outcome PlaybackOutcome) PlaybackOutcome {
	if !outcome.Hot {
		outcome.StopReason = "not hot: empty file or first byte is zero"
		return outcome
	}
	if len(j.Segments) == 0 {
		outcome.StopReason = "no header"
		return outcome
	}
	first := j.Segments[0].Header
	// readJournalHdr returns SQLITE_DONE when the magic bytes do not match.
	if first.State != HeaderSynced && first.State != HeaderNoSync {
		outcome.StopReason = "first header is " + first.State.String()
		return outcome
	}
	// pager_playback truncates the database to the first header's dbOrigSize
	// before restoring pages (src/pager.c:2952).
	outcome.TruncateTo = first.InitialPages
	superPage := SuperJournalPage(first.PageSize)
	for _, segment := range j.Segments {
		h := segment.Header
		if !h.MagicOK {
			outcome.StopReason = fmt.Sprintf("header at offset %d has no magic", h.Offset)
			return outcome
		}
		count := len(segment.Records)
		if h.RecordCount != NoSyncRecordCount {
			if int64(h.RecordCount) > int64(count) {
				outcome.StopReason = fmt.Sprintf("header at offset %d counts %d records but only %d are complete", h.Offset, h.RecordCount, count)
				for _, r := range segment.Records {
					if stop := outcome.take(r, first.InitialPages, superPage); stop != "" {
						outcome.StopReason = stop
						return outcome
					}
				}
				return outcome
			}
			count = int(h.RecordCount)
		}
		for _, r := range segment.Records[:count] {
			if stop := outcome.take(r, first.InitialPages, superPage); stop != "" {
				outcome.StopReason = stop
				return outcome
			}
		}
	}
	outcome.StopReason = "end of journal"
	return outcome
}

// take applies pager_playback_one_page's per-record checks in SQLite's order
// (src/pager.c:2298-2500): page 0 or superPage stops playback, pages beyond
// initialPages are skipped, and a bad checksum stops playback.
func (o *PlaybackOutcome) take(r Record, initialPages, superPage uint32) string {
	if r.Page == 0 || r.Page == superPage {
		return fmt.Sprintf("record at offset %d has page number %d", r.Offset, r.Page)
	}
	if r.Page > initialPages {
		o.SkippedPages = append(o.SkippedPages, r.Page)
		return ""
	}
	if !r.ChecksumOK() {
		return fmt.Sprintf("record at offset %d (page %d) fails its checksum", r.Offset, r.Page)
	}
	o.Pages = append(o.Pages, r.Page)
	return ""
}
```

Walk-through of `internal/journalfile`:

- **Constants and `HeaderState` (`Magic`, `HeaderFieldsSize`, `NoSyncRecordCount`, `RecordOverhead`, `maxSuperJournalLen`).** Encodes the 8-byte signature `aJournalMagic` (`d9 d5 05 f9 20 a1 63 d7`), the 28-byte header field block, the `0xffffffff` no-sync sentinel, the 8-byte per-record wrapper (4-byte page number + 4-byte checksum), and Unix's 512-byte maximum super-journal pathname length (`MAX_PATHNAME`, `src/os_unix.c:180`). `parseHeader` classifies the first 28 bytes into six states: `HeaderZeroed` (all 28 bytes zero, `PERSIST` after commit), `HeaderNoSync` (magic + `0xffffffff`), `HeaderSynced` (magic + explicit count), `HeaderNotYetSynced` (first 12 bytes zero, rest populated—cold journal before `syncJournal`), `HeaderInvalid`, and `HeaderMissing` (fewer than 28 bytes, or fewer than one full first sector—matching `readJournalHdr`, `src/pager.c:1607-1610`).
- **Segment and super-journal parsing (`Parse`, `validGeometry`, `roundUp`, `superJournalName`, `superJournalChecksum`).** Checks first for a multi-database super-journal footer at EOF (`[PAGER_SJ_PGNO][name][4-byte len][4-byte sum][Magic]`) and trims it off the record scan. Matching `readSuperJournal` (`src/pager.c:1303-1349`) and `writeSuperJournal` (`src/pager.c:1709-1737`), `superJournalChecksum` sums the name bytes using the target platform's C `char` signedness (`cCharIsSigned`: signed on `linux/amd64` and `darwin/*`; unsigned on `linux/arm`, `arm64`, `ppc64`, `ppc64le`, `riscv64`, and `s390x`), ignores names starting with NUL (`name[0] == 0`) or longer than 512 bytes, and ends the name at its first NUL byte. `Parse` then reads sector size and page size from header 0, rejecting non-power-of-two or out-of-range values (`src/pager.c:1650-1672`). If the remaining file is shorter than `first.SectorSize`, `Parse` keeps the decoded fields, marks the header `HeaderMissing`, and sets `TrailingByte = end` so `TrailingByte` is never negative. Otherwise it walks sector-aligned segments: records begin one sector after each header; a counted header owns `RecordCount` records and the next header begins at `roundUp(position, sector)` (`journalHdrOffset`, `src/pager.c:1366-1376`), while a `no-sync` or `not-yet-synced` header reads every complete record up to EOF, recording any incomplete tail in `TrailingByte`.
- **Checksum (`Checksum`).** Re-implements `pager_cksum` (`src/pager.c:2251-2259`): starts from `nonce` and sums `image[i]` for `i = len(image) - 200; i > 0; i -= 200`. Byte 0 is intentionally not included.
- **Hotness and playback prediction (`LooksHot`, `Playback`, `PlaybackIfSuperJournalExists`, `take`).** `LooksHot()` checks `Size > 0 && FirstByte != 0`—the byte-level half of `hasHotJournal` (`src/pager.c:5244-5251`). Because `pager_playback` skips rollback whenever a named super-journal file does not exist on disk (`src/pager.c:2891-2897`), `Playback()` refuses to predict an unconditional rollback when `SuperJournal != ""` (returning `Hot: true`, empty `Pages`, `TruncateTo: 0`, and a `StopReason` naming the super-journal); `PlaybackIfSuperJournalExists()` returns the rollback prediction for the case where that file exists. Both share `playback` and `take`, which apply `pager_playback_one_page`'s exact stop/skip rules (`src/pager.c:2298-2500`): stop on page `0` or `SuperJournalPage(pageSize)` (`262145` for 4,096-byte pages), skip pages `> InitialPages`, stop on the first bad checksum, and report `TruncateTo = first.InitialPages`.

Unit tests and a live test against an open SQLite transaction live in [`internal/journalfile/journalfile_test.go`](../internal/journalfile/journalfile_test.go) and run with:

```text
go test ./internal/journalfile/
```

---

### 5.2 Experiment 4.2 — Cache budget (`4.2-cache-budget`)

- **Claim:**
  1. One full scan reports exact per-page memory overhead (`4,352` bytes = 4,096-byte page + 256 bytes of headers in `sqlite3PagerMemUsed`, `src/pager.c:6881-6887`), a cache of `N` pages holds `N - 1` for `N >= 10` (`pcache1FetchStage2`, `src/pcache1.c:902`), a file larger than `cache_size` misses every page again on a second scan, and a cache larger than the file serves the second scan with zero new misses (Part A).
  2. In `modernc.org/sqlite v1.60.1`, `SQLITE_ENABLE_MEMORY_MANAGEMENT=1` is enabled, so `cache_size` is **not** a per-connection cap; every open connection contributes its share (`nMax = 483`, holding `482` pages on a single pass at `cache_size=-2000`) to one process-wide LRU `PGroup` (`pcache1Init`, `src/pcache1.c:718-719`). An active connection takes pages from idle connections (holding up to all 1,004 pages of its file while idle connections drop to 0). On a first sweep across `N` open connections, the sum equals `N × 482`; under pool churn (Part D) or repeated sweeps, the group stays capped at `pGroup->nMaxPage = open × (full + 1)`.
  3. Total cached pages and process RSS grow linearly with open connections—whether those connections open `N` distinct files (Part B) or the same single file (Part C, proving connections do not share a cache). With this driver's `modernc.org/memory` power-of-two allocator, RSS growth is roughly `2x` summed `CACHE_USED` (each ~4.3 KiB page allocation occupies an 8 KiB slot).
  4. Visiting `N` files through an LRU pool capped at `K` open connections holds cached pages (within `K × (full + 1)`) and process RSS flat past `K` (Part D).
  5. On a checkpointed database (no pending frames in `-wal`), enabling `PRAGMA mmap_size` to cover the file drops `CACHE_USED` near zero (from `482` to `2` pages) while total RSS still grows by the file's size—shifted from anonymous heap (`RssAnon`) to file-backed mapped pages (`RssFile`, Part E).
- **Evidence form:** `BOUND` and `SHAPE` for process RSS and pool bounds, with `EXACT` page counts and hit/miss/spill counters in every stored table. Process RSS and raw `CACHE_USED` bytes are run-dependent: they are in `info` tables, printed by `course run`, and not stored.
- **Portable (Linux-only split in Part E):** Runs on Linux and macOS; the `RssFile`/`RssAnon` split in Part E requires `/proc/self/status` (**Linux only**; `course run` runs it in the Linux `lab` container).
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 4.2
  ```
  Optional flags: `-n 64` (unbounded connection count), `-k 8` (pool cap), `-cache-size -2000` (must represent a budget of at least `1 MiB`; positive page counts are verified in pages rather than bytes), `-rows 1000` (must be at least `1.2 × max(200, budget pages)` so the database exceeds both the 200-page calibration cache and the requested budget). `course run` cannot pass them; run a flag variant directly in the container, for example `docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -k 4`.

<!-- include: experiments/4.2-cache-budget/main.go -->
```go
// Experiment 4.2 measures SQLite's page-cache memory budget and process RSS
// across five scenarios in WAL mode: one connection scanning twice at two
// cache_size settings (Part A), N open connections to N files (Part B), N open
// connections to one file (Part C), N files visited through a K-connection LRU
// pool (Part D), and memory-mapped reads via PRAGMA mmap_size (Part E).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 4.2 (portable; the anonymous vs
// file-backed RSS split in Part E uses /proc/self/status on Linux).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize  = 4096
	bodyBytes = 3500
	scanSQL   = "SELECT count(*), sum(length(body)) FROM t"
)

var (
	connectionsN  = flag.Int("n", 64, "connections opened in the unbounded runs")
	boundK        = flag.Int("k", 8, "open connections allowed by the bounded LRU pool")
	cacheSizeFlag = flag.Int("cache-size", -2000, "PRAGMA cache_size for every connection (negative = KiB, positive = pages)")
	rowsFlag      = flag.Int("rows", 1000, "rows per database file; one row of 3500 bytes fills one 4096-byte leaf page")
)

type processMemory struct {
	RSS   int64
	Anon  int64
	File  int64
	Shmem int64
}

type connStat struct {
	CacheUsed int
	Hits      int
	Misses    int
	Spills    int
}

type checkpoint struct {
	Label   string
	Open    int
	Visited int
	Conns   []connStat
	Memory  processMemory
}

var perPage = 1

// cachedPages derives the cached page count from SQLITE_DBSTATUS_CACHE_USED
// (sqlite3PagerMemUsed, src/pager.c:6881-6887). Subtracting the single
// page-sized buffer and dividing by perPage discards the Pager struct's
// path-length-dependent allocation (as long as it stays below perPage), making
// page counts identical across host and container paths.
func cachedPages(cacheUsed int) int {
	return max(0, (cacheUsed-pageSize)/perPage)
}

func (c checkpoint) pagesTotal() int {
	total := 0
	for _, s := range c.Conns {
		total += cachedPages(s.CacheUsed)
	}
	return total
}

func (c checkpoint) pagesMinMax() (int, int) {
	lo, hi := c.cacheMinMax()
	return cachedPages(lo), cachedPages(hi)
}

func (c checkpoint) cacheTotal() int {
	total := 0
	for _, s := range c.Conns {
		total += s.CacheUsed
	}
	return total
}

func (c checkpoint) cacheMinMax() (int, int) {
	if len(c.Conns) == 0 {
		return 0, 0
	}
	lo, hi := c.Conns[0].CacheUsed, c.Conns[0].CacheUsed
	for _, s := range c.Conns {
		lo, hi = min(lo, s.CacheUsed), max(hi, s.CacheUsed)
	}
	return lo, hi
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func options(cacheSize int) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.SingleConnection = true
	o.Conn.CacheSize = cacheSize
	return o
}

// budgetBytes converts PRAGMA cache_size to bytes matching numberOfCachePages
// (src/pcache.c:277-292): negative values are KiB, positive values are pages.
func budgetBytes(cacheSize int) int {
	if cacheSize < 0 {
		return -1024 * cacheSize
	}
	return cacheSize * pageSize
}

// Flag floors ensuring calibration and RSS checks stay meaningful: Part A
// calibrates bytes per page from cache_size=200 minus cache_size=100, and RSS
// checks require at least 1 MiB of cache per connection so page-cache memory
// dominates fixed per-connection overhead and allocator noise.
const (
	calibrationPages = 200
	minBudgetBytes   = 1 << 20
)

// minimumRows returns the smallest row count (1.2x the larger of the 200-page
// calibration cache and the requested cache budget) so the database exceeds
// both caches and misses on a full scan.
func minimumRows(cacheSize int) int {
	return max(calibrationPages, budgetBytes(cacheSize)/pageSize) * 6 / 5
}

// withinBudget checks Part A's budget claim: positive PRAGMA cache_size counts
// pages directly (numberOfCachePages, src/pcache.c:277-292), while negative
// cache_size is a KiB byte budget that already accounts for pageSize + szExtra.
func withinBudget(cacheUsed, cacheSize int) bool {
	if cacheSize > 0 {
		return cachedPages(cacheUsed) <= cacheSize
	}
	return cacheUsed <= budgetBytes(cacheSize)*105/100
}

func fileName(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("f-%03d.db", i))
}

// buildSource creates one deterministic WAL-mode database with rows of 3500
// PRNG bytes each so every row fills a single 4096-byte leaf page, giving a
// predictable multi-megabyte table that exceeds the default -2000 KiB cache.
func buildSource(ctx context.Context, path string, rows int) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, options(-2000))
	if err != nil {
		fatal("create source", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create table", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	rng := rand.New(rand.NewPCG(4, 2))
	body := make([]byte, bodyBytes)
	for id := 1; id <= rows; id++ {
		for i := range body {
			body[i] = byte(rng.Uint32())
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, body) VALUES(?, ?)", id, body); err != nil {
			fatal("insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

type handle struct {
	db   *sqlitex.DB
	conn *sql.Conn
}

func openHandle(ctx context.Context, path string, cacheSize int) handle {
	db, err := sqlitex.Open(ctx, path, options(cacheSize))
	if err != nil {
		fatal("open", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	return handle{db: db, conn: conn}
}

func (h handle) close() {
	if err := h.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := h.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (h handle) scan(ctx context.Context) {
	var count, total int64
	if err := h.conn.QueryRowContext(ctx, scanSQL).Scan(&count, &total); err != nil {
		fatal("scan", err)
	}
}

func (h handle) stat(ctx context.Context) connStat {
	read := func(op sqlitex.DBStatusOp) int {
		s, err := sqlitex.ReadDBStatus(ctx, h.conn, op, false)
		if err != nil {
			fatal("db_status", err)
		}
		return s.Current
	}
	return connStat{CacheUsed: read(sqlitex.DBStatusCacheUsed), Hits: read(sqlitex.DBStatusCacheHit), Misses: read(sqlitex.DBStatusCacheMiss), Spills: read(sqlitex.DBStatusCacheSpill)}
}

// measure forces a Go GC and returns unused Go spans to the OS before sampling
// process memory so RSS deltas reflect SQLite's page cache and file mappings.
func measure(label string, open, visited int, stats []connStat) {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := readProcessMemory()
	if err != nil {
		fatal("process memory", err)
	}
	line, err := json.Marshal(checkpoint{Label: label, Open: open, Visited: visited, Conns: stats, Memory: m})
	if err != nil {
		fatal("json", err)
	}
	fmt.Println(string(line))
}

func reportPoints(n int) []int {
	points := []int{1}
	for p := 2; p < n; p *= 2 {
		points = append(points, p)
	}
	if n > 1 {
		points = append(points, n)
	}
	return points
}

func childUnbounded(ctx context.Context, dir string, n, cacheSize int, oneFile bool) {
	measure("start", 0, 0, nil)
	var handles []handle
	points := reportPoints(n)
	for i := range n {
		path := fileName(dir, i)
		if oneFile {
			path = fileName(dir, 0)
		}
		h := openHandle(ctx, path, cacheSize)
		h.scan(ctx)
		handles = append(handles, h)
		if slices.Contains(points, i+1) {
			stats := make([]connStat, len(handles))
			for j, other := range handles {
				stats[j] = other.stat(ctx)
			}
			measure(fmt.Sprintf("after %d", i+1), len(handles), i+1, stats)
		}
	}
	for _, h := range handles {
		h.close()
	}
	measure("all closed", 0, n, nil)
}

func childBounded(ctx context.Context, dir string, n, k, cacheSize int) {
	measure("start", 0, 0, nil)
	var pool []handle
	points := reportPoints(n)
	for i := range n {
		if len(pool) == k {
			pool[0].close()
			pool = pool[1:]
		}
		h := openHandle(ctx, fileName(dir, i), cacheSize)
		h.scan(ctx)
		pool = append(pool, h)
		if slices.Contains(points, i+1) || i+1 == k {
			stats := make([]connStat, len(pool))
			for j, other := range pool {
				stats[j] = other.stat(ctx)
			}
			measure(fmt.Sprintf("after %d", i+1), len(pool), i+1, stats)
		}
	}
	for _, h := range pool {
		h.close()
	}
}

func childScans(ctx context.Context, dir string, cacheSize int) {
	h := openHandle(ctx, fileName(dir, 0), cacheSize)
	defer h.close()
	var stats []connStat
	for range 2 {
		h.scan(ctx)
		stats = append(stats, h.stat(ctx))
	}
	measure("scans", 1, 1, stats)
}

func childMmap(ctx context.Context, dir string, cacheSize int, mmapSize int64) {
	measure("start", 0, 0, nil)
	plain := openHandle(ctx, fileName(dir, 0), cacheSize)
	plain.scan(ctx)
	measure("plain", 1, 1, []connStat{plain.stat(ctx)})
	mapped := openHandle(ctx, fileName(dir, 1), cacheSize)
	var got int64
	if err := mapped.conn.QueryRowContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", mmapSize)).Scan(&got); err != nil {
		fatal("mmap_size", err)
	}
	if got != mmapSize {
		fatal("mmap_size", fmt.Errorf("PRAGMA mmap_size returned %d, want %d", got, mmapSize))
	}
	mapped.scan(ctx)
	measure("mapped", 2, 2, []connStat{plain.stat(ctx), mapped.stat(ctx)})
	mapped.close()
	plain.close()
}

func childMain(args []string) {
	ctx := context.Background()
	atoi := func(s string) int {
		v, err := strconv.Atoi(s)
		if err != nil {
			fatal("child args", err)
		}
		return v
	}
	switch args[0] {
	case "files":
		childUnbounded(ctx, args[1], atoi(args[2]), atoi(args[3]), false)
	case "one-file":
		childUnbounded(ctx, args[1], atoi(args[2]), atoi(args[3]), true)
	case "bounded":
		childBounded(ctx, args[1], atoi(args[2]), atoi(args[3]), atoi(args[4]))
	case "scans":
		childScans(ctx, args[1], atoi(args[2]))
	case "mmap":
		childMmap(ctx, args[1], atoi(args[2]), int64(atoi(args[3])))
	default:
		fatal("child", fmt.Errorf("unknown role %q", args[0]))
	}
}

// runChild executes one measurement scenario in a fresh child process so every
// scenario starts from a clean RSS baseline and a fresh pcache1 PGroup.
func runChild(args ...string) []checkpoint {
	exe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.Command(exe, append([]string{"child"}, args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal("child "+args[0], err)
	}
	var points []checkpoint
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var c checkpoint
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			fatal("child output", fmt.Errorf("%w: %q", err, line))
		}
		points = append(points, c)
	}
	return points
}

func find(points []checkpoint, label string) checkpoint {
	for _, p := range points {
		if p.Label == label {
			return p
		}
	}
	fatal("checkpoint", fmt.Errorf("no checkpoint %q", label))
	return checkpoint{}
}

func sumsMatch(points []checkpoint, perConnection int) bool {
	for _, p := range points {
		if p.pagesTotal() != p.Open*perConnection {
			return false
		}
	}
	return true
}

// pagesMatch compares path-length-independent cached-page totals across every
// checkpoint (raw CACHE_USED bytes vary with filename length via
// sqlite3MallocSize(pPager), src/pager.c:6881-6887).
func pagesMatch(a, b []checkpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Label != b[i].Label || a[i].pagesTotal() != b[i].pagesTotal() {
			return false
		}
	}
	return true
}

func growth(points []checkpoint, label string) int64 {
	return find(points, label).Memory.RSS - points[0].Memory.RSS
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 4.2" runs
// both. Raw CACHE_USED bytes include the Pager allocation, which holds the
// file names, and RSS varies by machine: both go to info tables.
var (
	set    *results.Set
	tables struct {
		parts, settings, calibration, scans, checkpoints, connections, info, infoMemory, infoCacheUsed *results.Table
	}
	settingSeq, infoSeq int
)

func openTables() {
	tables.parts = set.Table("parts", "part", "what", "status")
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.calibration = set.Table("calibration", "cache_size", "cached_pages")
	tables.scans = set.Table("scans", "cache_size", "budget_bytes", "scan", "cached_pages", "hits", "misses", "spills")
	tables.checkpoints = set.Table("checkpoints", "scenario", "seq", "checkpoint", "open", "visited")
	tables.connections = set.Table("connections", "scenario", "seq", "checkpoint", "conn", "cached_pages", "hits", "misses", "spills")
	tables.info = set.Table("info", "seq", "name", "value")
	tables.infoMemory = set.Table("info_memory", "scenario", "seq", "checkpoint", "rss", "rss_anon", "rss_file", "rss_shmem", "cache_used_sum")
	tables.infoCacheUsed = set.Table("info_cache_used", "scenario", "seq", "checkpoint", "conn", "cache_used")
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

func info(name string, value any) {
	infoSeq++
	tables.info.Row(infoSeq, name, value)
}

// writePoints writes the checkpoints of one child run: the open connections
// and their cache counters, and the process memory as info.
func writePoints(scenario string, points []checkpoint) {
	for i, p := range points {
		tables.checkpoints.Row(scenario, i+1, p.Label, p.Open, p.Visited)
		for j, c := range p.Conns {
			tables.connections.Row(scenario, i+1, p.Label, j+1, cachedPages(c.CacheUsed), c.Hits, c.Misses, c.Spills)
			tables.infoCacheUsed.Row(scenario, i+1, p.Label, j+1, c.CacheUsed)
		}
		m := p.Memory
		tables.infoMemory.Row(scenario, i+1, p.Label, m.RSS, known(m.Anon), known(m.File), known(m.Shmem), p.cacheTotal())
	}
}

// known returns nil for a memory figure the platform does not report (-1).
func known(b int64) any {
	if b < 0 {
		return nil
	}
	return b
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "child" {
		childMain(os.Args[2:])
		return
	}
	flag.Parse()
	ctx := context.Background()
	started := time.Now()
	sqlitex.MustPinCheckMemory(ctx)
	if *connectionsN < 2 || *boundK < 1 || *boundK >= *connectionsN || *cacheSizeFlag == 0 {
		fatal("flags", errors.New("need n >= 2, 1 <= k < n, cache-size != 0"))
	}
	if minRows := minimumRows(*cacheSizeFlag); *rowsFlag < minRows {
		fatal("flags", fmt.Errorf("-rows %d is too small for -cache-size %d: need rows >= %d so the file is larger than both the cache_size=200 calibration cache and the requested budget", *rowsFlag, *cacheSizeFlag, minRows))
	}
	if budgetBytes(*cacheSizeFlag) < minBudgetBytes {
		fatal("flags", fmt.Errorf("-cache-size %d is a budget of %d bytes; need at least %d so that cache memory dominates per-connection memory in the RSS checks", *cacheSizeFlag, budgetBytes(*cacheSizeFlag), minBudgetBytes))
	}
	work := sqlitex.MustWorkDir("4.2")
	set = results.MustOpen(work)
	openTables()
	n, k, cacheSize := *connectionsN, *boundK, *cacheSizeFlag
	budget := budgetBytes(cacheSize)
	source := fileName(work, 0)
	buildSource(ctx, source, *rowsFlag)
	stat, err := os.Stat(source)
	if err != nil {
		fatal("stat", err)
	}
	for i := 1; i < n; i++ {
		data, err := os.ReadFile(source)
		if err != nil {
			fatal("read source", err)
		}
		if err := os.WriteFile(fileName(work, i), data, 0o644); err != nil {
			fatal("copy", err)
		}
	}
	var compileMM int
	db, err := sqlitex.OpenMemory(ctx)
	if err != nil {
		fatal("open memory", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT sqlite_compileoption_used('ENABLE_MEMORY_MANAGEMENT')").Scan(&compileMM); err != nil {
		fatal("compile option", err)
	}
	db.Close()
	setting("database files (copies of one WAL-mode file)", n)
	setting("rows in table t", *rowsFlag)
	setting("bytes per row (body BLOB)", bodyBytes)
	setting("pages per file", stat.Size()/pageSize)
	setting("bytes per file", stat.Size())
	setting("cache_size of every connection", cacheSize)
	setting("cache budget per connection, bytes", budget)
	setting("scan", scanSQL)
	setting("N: connections kept open (parts B, C)", n)
	setting("K: connections in the bounded pool (part D)", k)
	setting("driver compiled with ENABLE_MEMORY_MANAGEMENT", compileMM)
	info("process memory read from", memorySource)

	// Measure exact bytes per cached page from sqlite3PagerMemUsed
	// (src/pager.c:6881-6887) by comparing two full caches 100 pages apart.
	// pcache1FetchStage2 recycles at nPage+1 >= nMax (src/pcache1.c:902), so
	// cache_size=100 holds 99 pages and cache_size=200 holds 199 pages; their
	// difference is still exactly 100 pages and cancels the fixed Pager overhead.
	small := runChild("scans", work, "100")[0].Conns[0].CacheUsed
	large := runChild("scans", work, "200")[0].Conns[0].CacheUsed
	if large <= small || (large-small)%100 != 0 {
		fatal("per-page size", fmt.Errorf("CACHE_USED at cache_size=200 minus at 100 is %d, not a multiple of 100", large-small))
	}
	perPage = (large - small) / 100
	setting("CACHE_USED bytes per cached page", perPage)
	setting("of which page header bytes", perPage-pageSize)
	// The fixed part is the Pager allocation, which holds the file names, plus one page buffer.
	info("fixed part of CACHE_USED per connection, bytes", small-cachedPages(small)*perPage)
	tables.calibration.Row(100, cachedPages(small))
	tables.calibration.Row(200, cachedPages(large))
	for _, size := range []int{cacheSize, -2 * int(stat.Size()) / 1024} {
		scans := runChild("scans", work, strconv.Itoa(size))[0]
		for i, s := range scans.Conns {
			tables.scans.Row(size, budgetBytes(size), i+1, cachedPages(s.CacheUsed), s.Hits, s.Misses, s.Spills)
			info(fmt.Sprintf("part A, cache_size=%d, CACHE_USED after scan %d, bytes", size, i+1), s.CacheUsed)
		}
		first, second := scans.Conns[0], scans.Conns[1]
		budgetClaim := fmt.Sprintf("cache_size=%d: CACHE_USED stays within the budget plus 5%% (%d pages x %d bytes against %d bytes)", size, cachedPages(first.CacheUsed), perPage, budgetBytes(size))
		if size > 0 {
			budgetClaim = fmt.Sprintf("cache_size=%d: the cache holds at most %d pages (it holds %d)", size, size, cachedPages(first.CacheUsed))
		}
		set.Check("A", withinBudget(first.CacheUsed, size), budgetClaim)
		if size == cacheSize {
			set.Check("A", second.Misses-first.Misses >= int(stat.Size()/pageSize)*9/10, "a file larger than the cache misses again on the second scan (the LRU evicted the pages the scan needs next)")
		} else {
			set.Check("A", second.Misses == first.Misses && second.Hits > first.Hits, "a cache larger than the file serves the second scan from memory: no new misses")
		}
	}

	full := cachedPages(runChild("scans", work, strconv.Itoa(cacheSize))[0].Conns[0].CacheUsed)
	nLabel, halfLabel := fmt.Sprintf("after %d", n), fmt.Sprintf("after %d", reportPoints(n)[len(reportPoints(n))-2])

	files := runChild("files", work, strconv.Itoa(n), strconv.Itoa(cacheSize))
	writePoints("B", files)
	lo, hi := find(files, nLabel).pagesMinMax()
	set.Check("B", sumsMatch(files, full), fmt.Sprintf("the cached pages of all open connections add up to exactly (open connections) x %d, the pages one connection holds when it fills its budget", full))
	// With ENABLE_MEMORY_MANAGEMENT, pcache1Init sets separateCache=0
	// (src/pcache1.c:718-719), putting every open connection in one process-wide
	// PGroup LRU. Active scanners recycle idle connections' pages
	// (pcache1FetchStage2, src/pcache1.c:899-912) while the group total equals
	// open * perConnBudget (pcache1Cachesize, src/pcache1.c:814-832).
	if compileMM == 1 {
		set.Check("B", hi > full && lo < full, fmt.Sprintf("in this driver build cache_size is not a per-connection cap: the connection that scanned last holds %d pages (its own budget is %d) while the idle connections were left with %d", hi, full, lo))
	} else {
		set.Check("B", hi <= full, "every connection stays within its own budget")
	}
	rssN, rssHalf, cacheN := growth(files, nLabel), growth(files, halfLabel), int64(find(files, nLabel).cacheTotal())
	info(fmt.Sprintf("part B, RSS growth / summed CACHE_USED at %s", nLabel), fmt.Sprintf("%.2f", float64(rssN)/float64(cacheN)))
	set.Check("B", rssN >= cacheN*9/10, "process memory grows by at least the summed CACHE_USED of the open connections")
	half := find(files, halfLabel).Open
	perN, perHalf := float64(rssN)/float64(n), float64(rssHalf)/float64(half)
	info(fmt.Sprintf("part B, RSS growth per open connection at %d, bytes", n), int64(perN))
	info(fmt.Sprintf("part B, RSS growth per open connection at %d, bytes", half), int64(perHalf))
	set.Check("B", perN >= perHalf*0.8 && perN <= perHalf*1.25, fmt.Sprintf("process memory grows linearly with open connections: growth per open connection at %d is within -20%%/+25%% of that at %d", n, half))

	oneFile := runChild("one-file", work, strconv.Itoa(n), strconv.Itoa(cacheSize))
	writePoints("C", oneFile)
	set.Check("C", sumsMatch(oneFile, full) && pagesMatch(oneFile, files), "connections to one file do not share a cache: the totals equal the N-files run at every checkpoint")
	oneN := growth(oneFile, nLabel)
	set.Check("C", oneN >= cacheN*9/10, "process memory grows by the summed CACHE_USED here too")

	bounded := runChild("bounded", work, strconv.Itoa(n), strconv.Itoa(k), strconv.Itoa(cacheSize))
	writePoints("D", bounded)
	kLabel := fmt.Sprintf("after %d", k)
	atK, atN := growth(bounded, kLabel), growth(bounded, nLabel)
	set.Check("D", find(bounded, nLabel).Open == k && find(bounded, nLabel).pagesTotal() <= k*(full+1), fmt.Sprintf("with the pool, %d connections are open at the end and their cached pages stay within %d x (%d + 1), the sum of their cache sizes in pages", k, k, full))
	set.Check("D", atN-atK <= atK/4, fmt.Sprintf("with the pool, process memory stays flat after %d connections: growth from %s to %s is at most a quarter of the growth up to %s", k, kLabel, nLabel, kLabel))
	set.Check("D", float64(atN) <= float64(rssN)*float64(k)/float64(n)*1.5, fmt.Sprintf("the pooled run ends with about %d/%d of the unbounded run's memory growth (at most 1.5 x that fraction)", k, n))

	// PRAGMA mmap_size activates getPageMMap (pagerFixMaplimit,
	// src/pager.c:3579-3590; getPageMMap, src/pager.c:5687-5752), which serves
	// pages directly from the kernel's mapped file cache via sqlite3OsFetch and
	// pagerAcquireMapPage outside pcache1, so CACHE_USED stays near zero while
	// RssFile grows by the file size.
	mmapSize := 2 * stat.Size()
	setting("part E: mmap_size of connection 2 (twice the file)", mmapSize)
	mapped := runChild("mmap", work, strconv.Itoa(cacheSize), strconv.FormatInt(mmapSize, 10))
	writePoints("E", mapped)
	plain, withMap := find(mapped, "plain"), find(mapped, "mapped")
	mappedConn := withMap.Conns[1]
	set.Check("E", mappedConn.CacheUsed*20 < plain.Conns[0].CacheUsed, "with mmap covering the file, the scan leaves CACHE_USED near zero (under 5% of the unmapped connection's)")
	rssDelta := withMap.Memory.RSS - plain.Memory.RSS
	info("part E, RSS added by the mapped scan, bytes", rssDelta)
	set.Check("E", rssDelta >= stat.Size()*8/10, "the mapped scan still adds about the file's size to process RSS: the memory moved from SQLite's heap to mapped file pages")
	tables.parts.Row("A", "one connection, the same scan twice", "ran")
	tables.parts.Row("B", fmt.Sprintf("%d connections to %d files, all kept open", n, n), "ran")
	tables.parts.Row("C", fmt.Sprintf("%d connections to one file, all kept open", n), "ran")
	tables.parts.Row("D", fmt.Sprintf("%d files through a pool of at most %d open connections", n, k), "ran")
	tables.parts.Row("E", "one connection without and one with mmap", "ran")
	if withMap.Memory.File >= 0 {
		fileDelta, anonDelta := withMap.Memory.File-plain.Memory.File, withMap.Memory.Anon-plain.Memory.Anon
		info("part E, file-backed RSS added, bytes", fileDelta)
		info("part E, anonymous RSS added, bytes", anonDelta)
		set.Check("E", fileDelta >= stat.Size()*8/10 && anonDelta < stat.Size()/4, "the added memory is file-backed (RssFile), not anonymous heap")
		tables.parts.Row("E", "split of the added RSS into file-backed and anonymous", "ran")
	} else {
		fmt.Println("LINUX-ONLY: the split of RSS into anonymous and file-backed memory (/proc/self/status) skipped on " + runtime.GOOS + "; run it with: docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget")
		tables.parts.Row("E", "split of the added RSS into file-backed and anonymous", "skipped: needs /proc/self/status (Linux)")
	}

	info("runtime, seconds", time.Since(started).Round(100*time.Millisecond).Seconds())
	set.Finish("CACHE BUDGET")
}
```

<!-- include: experiments/4.2-cache-budget/memory_linux.go -->
```go
//go:build linux

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

const memorySource = "/proc/self/status"

// readProcessMemory parses /proc/self/status on Linux to report total resident
// memory (VmRSS) together with its anonymous, file-backed, and shared-memory
// breakdown so Part E can verify that mmap shifts pages from RssAnon to RssFile.
func readProcessMemory() (processMemory, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return processMemory{}, err
	}
	defer f.Close()
	m := processMemory{RSS: -1, Anon: -1, File: -1, Shmem: -1}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			m.RSS = kib * 1024
		case "RssAnon:":
			m.Anon = kib * 1024
		case "RssFile:":
			m.File = kib * 1024
		case "RssShmem:":
			m.Shmem = kib * 1024
		}
	}
	return m, scanner.Err()
}
```

<!-- include: experiments/4.2-cache-budget/memory_other.go -->
```go
//go:build !linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const memorySource = "ps -o rss= -p <pid>"

// readProcessMemory falls back to ps on non-Linux systems (such as macOS),
// reporting total RSS in bytes and leaving the anonymous/file-backed split
// unavailable (-1).
func readProcessMemory() (processMemory, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return processMemory{}, err
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return processMemory{}, err
	}
	return processMemory{RSS: kib * 1024, Anon: -1, File: -1, Shmem: -1}, nil
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/4.2-cache-budget/report.sql -->
```sql
-- Walkthrough of experiment 4.2. Run it with:
--   go run ./cmd/course run 4.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   parts        what ran and what was skipped
--   settings     one setting or derived constant of the run, by name
--   calibration  part A: the pages a full cache holds at cache_size=100 and 200
--   scans        part A: one connection after its first or second full scan
--   checkpoints  parts B to E: one measurement point of a child process run
--   connections  parts B to E: one open connection at one checkpoint
--   checks       every claim the experiment asserted (the runner lists it last)
-- Raw CACHE_USED bytes (they include the Pager allocation, which holds the file
-- names) and process memory (RSS) differ between runs and machines. They are in
-- info, info_memory and info_cache_used, shown by info.sql; the memory claims
-- below are checked against them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  4.2  What a page cache budget costs, per connection and per process'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Copies of one WAL-mode file are scanned by connections that keep their'
.print 'page cache. Each scenario runs in a fresh child process, which reads'
.print 'SQLITE_DBSTATUS_CACHE_USED, CACHE_HIT, CACHE_MISS and CACHE_SPILL of every'
.print 'open connection and its own RSS at each checkpoint.'

SELECT name, value FROM settings ORDER BY seq;

.print
.print 'ENABLE_MEMORY_MANAGEMENT = 1 means every page cache of the process is in'
.print 'one LRU group (pcache1.c:718-719). Cached pages are CACHE_USED minus one'
.print 'page buffer, divided by the bytes per cached page: the 4096-byte page plus'
.print 'its headers (sqlite3PagerMemUsed, pager.c:6881).'

SELECT part, what, status FROM parts ORDER BY part, what;

.print
.print '── A. One connection, the same scan twice ─────────────────────────────'
.print
.print 'A1. The bytes per cached page come from two full caches 100 pages apart.'
.print '    A cache recycles a page when it is one page short of full'
.print '    (pcache1.c:902), so cache_size=N holds N-1 pages.'

SELECT cache_size, cached_pages FROM calibration ORDER BY cache_size;

.print
.print 'A2. At the default budget (2048000 bytes) the cache holds 482 pages of the'
.print '    1004-page file, and the second scan misses every page again. A budget of'
.print '    twice the file holds it all, and the second scan only hits.'

SELECT cache_size, budget_bytes, scan, cached_pages, hits, misses, spills
FROM scans ORDER BY cache_size DESC, scan;

.print
.print '── B. N connections to N files, all kept open ─────────────────────────'
.print
.print 'B1. Every connection opens its file, scans it and stays open. The cached'
.print '    pages of all open connections add up to 482 per connection, but they'
.print '    are not spread evenly: the one LRU group lets the connection that'
.print '    scanned last take pages from the idle ones.'

SELECT p.checkpoint, p.open,
       coalesce(sum(c.cached_pages), 0)                 AS cached_pages_sum,
       coalesce(sum(c.cached_pages), 0) // nullif(p.open, 0) AS per_open_conn,
       min(c.cached_pages) || '..' || max(c.cached_pages) AS per_conn_min_max
FROM checkpoints p LEFT JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'B' AND p.checkpoint <> 'start'
GROUP BY p.seq, p.checkpoint, p.open ORDER BY p.seq;

.print
.print 'B2. At the last checkpoint, the first connection and the last one:'

SELECT conn, cached_pages, hits, misses, spills
FROM connections
WHERE scenario = 'B' AND seq = (SELECT max(seq) FROM connections WHERE scenario = 'B')
  AND conn IN (1, (SELECT max(conn) FROM connections WHERE scenario = 'B'))
ORDER BY conn;

.print
.print 'B3. Cached pages per connection at the last checkpoint:'

SELECT cached_pages, count(*) AS connections, min(conn) AS first_conn, max(conn) AS last_conn
FROM connections
WHERE scenario = 'B' AND seq = (SELECT max(seq) FROM connections WHERE scenario = 'B')
GROUP BY cached_pages ORDER BY cached_pages;

.print
.print '── C. N connections to one file, all kept open ────────────────────────'
.print
.print 'C1. Connections to the same file do not share pages: every checkpoint'
.print '    has the same cached pages as in part B.'

SELECT p.checkpoint, p.open,
       coalesce(sum(c.cached_pages), 0) AS cached_pages_sum,
       min(c.cached_pages) || '..' || max(c.cached_pages) AS per_conn_min_max,
       (SELECT coalesce(sum(b.cached_pages), 0) FROM connections b
        WHERE b.scenario = 'B' AND b.checkpoint = p.checkpoint) AS part_b_sum
FROM checkpoints p LEFT JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'C' AND p.checkpoint <> 'start'
GROUP BY p.seq, p.checkpoint, p.open ORDER BY p.seq;

.print
.print '── D. N files visited through a pool of at most K open connections ────'
.print
.print 'D1. The pool closes its least recently used connection before it opens'
.print '    the next. After K files the cached pages stop growing.'

SELECT p.checkpoint, p.open, p.visited,
       coalesce(sum(c.cached_pages), 0) AS cached_pages_sum,
       min(c.cached_pages) || '..' || max(c.cached_pages) AS per_conn_min_max
FROM checkpoints p LEFT JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'D' AND p.checkpoint <> 'start'
GROUP BY p.seq, p.checkpoint, p.open, p.visited ORDER BY p.seq;

.print
.print '── E. mmap ─────────────────────────────────────────────────────────────'
.print
.print 'E1. Connection 1 scans f-000 without mmap. Connection 2 sets PRAGMA'
.print '    mmap_size to twice the file and scans f-001: its pages come from the'
.print '    mapped file, outside the page cache, so it caches almost nothing.'

SELECT p.checkpoint, c.conn, c.cached_pages, c.hits, c.misses, c.spills
FROM checkpoints p JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'E'
ORDER BY p.seq, c.conn;

.print
.print 'The memory claims (RSS grows with the summed CACHE_USED, linearly with open'
.print 'connections, stays flat with the pool, and moves to file-backed pages with'
.print 'mmap) are in the checks below; the RSS figures are in info.sql.'
```

`course run` also prints the run-dependent values, which are not part of the stored evidence: the raw `CACHE_USED` bytes and the process memory (RSS, split into anonymous, file-backed and shared) at every checkpoint, and the values derived from them.

#### Captured evidence (`4.2-cache-budget.txt`)

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/4.2-cache-budget/`](evidence/4.2-cache-budget/), and `go run ./cmd/course check evidence ch04` compares a fresh run with them byte for byte.

<!-- include: evidence/4.2-cache-budget.txt -->
```text

════════════════════════════════════════════════════════════════════════
  4.2  What a page cache budget costs, per connection and per process
════════════════════════════════════════════════════════════════════════

Copies of one WAL-mode file are scanned by connections that keep their
page cache. Each scenario runs in a fresh child process, which reads
SQLITE_DBSTATUS_CACHE_USED, CACHE_HIT, CACHE_MISS and CACHE_SPILL of every
open connection and its own RSS at each checkpoint.
┌────────────────────────────────────────────────────┬───────────────────────────────────────────┐
│                        name                        │                   value                   │
├────────────────────────────────────────────────────┼───────────────────────────────────────────┤
│ database files (copies of one WAL-mode file)       │ 64                                        │
│ rows in table t                                    │ 1000                                      │
│ bytes per row (body BLOB)                          │ 3500                                      │
│ pages per file                                     │ 1004                                      │
│ bytes per file                                     │ 4112384                                   │
│ cache_size of every connection                     │ -2000                                     │
│ cache budget per connection, bytes                 │ 2048000                                   │
│ scan                                               │ SELECT count(*), sum(length(body)) FROM t │
│ N: connections kept open (parts B, C)              │ 64                                        │
│ K: connections in the bounded pool (part D)        │ 8                                         │
│ driver compiled with ENABLE_MEMORY_MANAGEMENT      │ 1                                         │
│ CACHE_USED bytes per cached page                   │ 4352                                      │
│ of which page header bytes                         │ 256                                       │
│ part E: mmap_size of connection 2 (twice the file) │ 8224768                                   │
└────────────────────────────────────────────────────┴───────────────────────────────────────────┘

ENABLE_MEMORY_MANAGEMENT = 1 means every page cache of the process is in
one LRU group (pcache1.c:718-719). Cached pages are CACHE_USED minus one
page buffer, divided by the bytes per cached page: the 4096-byte page plus
its headers (sqlite3PagerMemUsed, pager.c:6881).
┌──────┬───────────────────────────────────────────────────────┬────────┐
│ part │                         what                          │ status │
├──────┼───────────────────────────────────────────────────────┼────────┤
│ A    │ one connection, the same scan twice                   │ ran    │
│ B    │ 64 connections to 64 files, all kept open             │ ran    │
│ C    │ 64 connections to one file, all kept open             │ ran    │
│ D    │ 64 files through a pool of at most 8 open connections │ ran    │
│ E    │ one connection without and one with mmap              │ ran    │
│ E    │ split of the added RSS into file-backed and anonymous │ ran    │
└──────┴───────────────────────────────────────────────────────┴────────┘

── A. One connection, the same scan twice ─────────────────────────────

A1. The bytes per cached page come from two full caches 100 pages apart.
    A cache recycles a page when it is one page short of full
    (pcache1.c:902), so cache_size=N holds N-1 pages.
┌────────────┬──────────────┐
│ cache_size │ cached_pages │
├────────────┼──────────────┤
│ 100        │ 99           │
│ 200        │ 199          │
└────────────┴──────────────┘

A2. At the default budget (2048000 bytes) the cache holds 482 pages of the
    1004-page file, and the second scan misses every page again. A budget of
    twice the file holds it all, and the second scan only hits.
┌────────────┬──────────────┬──────┬──────────────┬──────┬────────┬────────┐
│ cache_size │ budget_bytes │ scan │ cached_pages │ hits │ misses │ spills │
├────────────┼──────────────┼──────┼──────────────┼──────┼────────┼────────┤
│ -2000      │ 2048000      │ 1    │ 482          │ 7    │ 1005   │ 0      │
│ -2000      │ 2048000      │ 2    │ 482          │ 9    │ 2007   │ 0      │
│ -8032      │ 8224768      │ 1    │ 1004         │ 7    │ 1005   │ 0      │
│ -8032      │ 8224768      │ 2    │ 1004         │ 1011 │ 1005   │ 0      │
└────────────┴──────────────┴──────┴──────────────┴──────┴────────┴────────┘

── B. N connections to N files, all kept open ─────────────────────────

B1. Every connection opens its file, scans it and stays open. The cached
    pages of all open connections add up to 482 per connection, but they
    are not spread evenly: the one LRU group lets the connection that
    scanned last take pages from the idle ones.
┌────────────┬──────┬──────────────────┬───────────────┬──────────────────┐
│ checkpoint │ open │ cached_pages_sum │ per_open_conn │ per_conn_min_max │
├────────────┼──────┼──────────────────┼───────────────┼──────────────────┤
│ after 1    │ 1    │ 482              │ 482           │ 482..482         │
│ after 2    │ 2    │ 964              │ 482           │ 0..964           │
│ after 4    │ 4    │ 1928             │ 482           │ 0..1004          │
│ after 8    │ 8    │ 3856             │ 482           │ 0..1004          │
│ after 16   │ 16   │ 7712             │ 482           │ 0..1004          │
│ after 32   │ 32   │ 15424            │ 482           │ 0..1004          │
│ after 64   │ 64   │ 30848            │ 482           │ 0..1004          │
│ all closed │ 0    │ 0                │               │                  │
└────────────┴──────┴──────────────────┴───────────────┴──────────────────┘

B2. At the last checkpoint, the first connection and the last one:
┌──────┬──────────────┬──────┬────────┬────────┐
│ conn │ cached_pages │ hits │ misses │ spills │
├──────┼──────────────┼──────┼────────┼────────┤
│ 1    │ 0            │ 7    │ 1005   │ 0      │
│ 64   │ 1004         │ 7    │ 1005   │ 0      │
└──────┴──────────────┴──────┴────────┴────────┘

B3. Cached pages per connection at the last checkpoint:
┌──────────────┬─────────────┬────────────┬───────────┐
│ cached_pages │ connections │ first_conn │ last_conn │
├──────────────┼─────────────┼────────────┼───────────┤
│ 0            │ 33          │ 1          │ 33        │
│ 728          │ 1           │ 34         │ 34        │
│ 1004         │ 30          │ 35         │ 64        │
└──────────────┴─────────────┴────────────┴───────────┘

── C. N connections to one file, all kept open ────────────────────────

C1. Connections to the same file do not share pages: every checkpoint
    has the same cached pages as in part B.
┌────────────┬──────┬──────────────────┬──────────────────┬────────────┐
│ checkpoint │ open │ cached_pages_sum │ per_conn_min_max │ part_b_sum │
├────────────┼──────┼──────────────────┼──────────────────┼────────────┤
│ after 1    │ 1    │ 482              │ 482..482         │ 482        │
│ after 2    │ 2    │ 964              │ 0..964           │ 964        │
│ after 4    │ 4    │ 1928             │ 0..1004          │ 1928       │
│ after 8    │ 8    │ 3856             │ 0..1004          │ 3856       │
│ after 16   │ 16   │ 7712             │ 0..1004          │ 7712       │
│ after 32   │ 32   │ 15424            │ 0..1004          │ 15424      │
│ after 64   │ 64   │ 30848            │ 0..1004          │ 30848      │
│ all closed │ 0    │ 0                │                  │ 0          │
└────────────┴──────┴──────────────────┴──────────────────┴────────────┘

── D. N files visited through a pool of at most K open connections ────

D1. The pool closes its least recently used connection before it opens
    the next. After K files the cached pages stop growing.
┌────────────┬──────┬─────────┬──────────────────┬──────────────────┐
│ checkpoint │ open │ visited │ cached_pages_sum │ per_conn_min_max │
├────────────┼──────┼─────────┼──────────────────┼──────────────────┤
│ after 1    │ 1    │ 1       │ 482              │ 482..482         │
│ after 2    │ 2    │ 2       │ 964              │ 0..964           │
│ after 4    │ 4    │ 4       │ 1928             │ 0..1004          │
│ after 8    │ 8    │ 8       │ 3856             │ 0..1004          │
│ after 16   │ 8    │ 16      │ 3863             │ 0..1004          │
│ after 32   │ 8    │ 32      │ 3863             │ 0..1004          │
│ after 64   │ 8    │ 64      │ 3863             │ 0..1004          │
└────────────┴──────┴─────────┴──────────────────┴──────────────────┘

── E. mmap ─────────────────────────────────────────────────────────────

E1. Connection 1 scans f-000 without mmap. Connection 2 sets PRAGMA
    mmap_size to twice the file and scans f-001: its pages come from the
    mapped file, outside the page cache, so it caches almost nothing.
┌────────────┬──────┬──────────────┬──────┬────────┬────────┐
│ checkpoint │ conn │ cached_pages │ hits │ misses │ spills │
├────────────┼──────┼──────────────┼──────┼────────┼────────┤
│ plain      │ 1    │ 482          │ 7    │ 1005   │ 0      │
│ mapped     │ 1    │ 482          │ 7    │ 1005   │ 0      │
│ mapped     │ 2    │ 2            │ 7    │ 3      │ 0      │
└────────────┴──────┴──────────────┴──────┴────────┴────────┘

The memory claims (RSS grows with the summed CACHE_USED, linearly with open
connections, stays flat with the pool, and moves to file-backed pages with
mmap) are in the checks below; the RSS figures are in info.sql.

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  cache_size=-2000: CACHE_USED stays within the budget plus 5% (482 pages x 4352 bytes against 2048000 bytes)
A  ✓  a file larger than the cache misses again on the second scan (the LRU evicted the pages the scan needs next)
A  ✓  cache_size=-8032: CACHE_USED stays within the budget plus 5% (1004 pages x 4352 bytes against 8224768 bytes)
A  ✓  a cache larger than the file serves the second scan from memory: no new misses
B  ✓  the cached pages of all open connections add up to exactly (open connections) x 482, the pages one connection holds when it fills its budget
B  ✓  in this driver build cache_size is not a per-connection cap: the connection that scanned last holds 1004 pages (its own budget is 482) while the idle connections were left with 0
B  ✓  process memory grows by at least the summed CACHE_USED of the open connections
B  ✓  process memory grows linearly with open connections: growth per open connection at 64 is within -20%/+25% of that at 32
C  ✓  connections to one file do not share a cache: the totals equal the N-files run at every checkpoint
C  ✓  process memory grows by the summed CACHE_USED here too
D  ✓  with the pool, 8 connections are open at the end and their cached pages stay within 8 x (482 + 1), the sum of their cache sizes in pages
D  ✓  with the pool, process memory stays flat after 8 connections: growth from after 8 to after 64 is at most a quarter of the growth up to after 8
D  ✓  the pooled run ends with about 8/64 of the unbounded run's memory growth (at most 1.5 x that fraction)
E  ✓  with mmap covering the file, the scan leaves CACHE_USED near zero (under 5% of the unmapped connection's)
E  ✓  the mapped scan still adds about the file's size to process RSS: the memory moved from SQLite's heap to mapped file pages
E  ✓  the added memory is file-backed (RssFile), not anonymous heap

16 of 16 claims hold

Experiment verdict: CACHE BUDGET OK
```

#### Reading the evidence

- **Opening tables (Setup).** Builds one WAL-mode database (`1,000` rows × `3,500` bytes = `1,004` pages = `4,112,384` bytes, ~3.9 MiB) and copies it 64 times. Each connection opens with `sqlitex.DefaultOptions()` (`journal_mode=wal`, `synchronous=FULL`) and `cache_size=-2000` (`2,048,000`-byte budget). The settings table shows `driver compiled with ENABLE_MEMORY_MANAGEMENT` = `1`, confirming the unified `PGroup` mode (`src/pcache1.c:718-719`).
- **A1–A2 (Part A: single connection, page accounting, and LRU behavior).**
  - Subtracting `CACHE_USED` at `cache_size=100` from `cache_size=200` and dividing by `100` measures `4,352` bytes per cached page (`4,096` page bytes + `256` bytes of `PgHdr`/extra/pointers from `sqlite3PagerMemUsed`, `src/pager.c:6881-6887`). The fixed overhead (a run-dependent value printed by `course run`: about `4,951` bytes in one lab run) is the `Pager` struct (whose size includes the database, journal, and WAL paths, which is why raw `CACHE_USED` bytes are run-dependent `info` values while derived page counts are `EXACT`) plus one 4,096-byte page buffer.
  - A1 shows the `N - 1` recycle rule: `cache_size=100` holds `99` pages, `cache_size=200` holds `199` pages (`pCache->nPage + 1 >= pCache->nMax`, `src/pcache1.c:902`).
  - At `cache_size=-2000`, scan 1 fills `482` pages (`hit=7 miss=1005 spill=0`; `CACHE_USED` is run-dependent, about `2,102,615` bytes in one lab run). Scan 2 ends at `hit=9 miss=2007`: because the 1,004-page table exceeds the 482-page cache, the LRU evicts every leaf page before the second scan reaches it again.
  - At `cache_size=-8032` (8,224,768 bytes, twice the file), scan 1 caches all `1,004` pages (`hit=7 miss=1005`), and scan 2 records `hit=1011 miss=1005`—zero new misses.
- **B1–B3 (Part B: 64 open connections to 64 files).**
  - At every checkpoint (`1, 2, 4, 8, 16, 32, 64` open connections), `cached pages (sum)` equals `open × 482` (`482`, `964`, `1,928`, `3,856`, `7,712`, `15,424`, `30,848`).
  - In the `min..max` column, after connection 2 scans, `min..max` is `0..964`; from connection 4 onward it is `0..1004`. B2 confirms connection 1 holds `0` cached pages while connection 64 holds `1004`. Because `ENABLE_MEMORY_MANAGEMENT` puts every connection in one `PGroup`, each scanning connection steals the LRU pages of idle connections until it holds the entire 1,004-page file.
  - In the run-dependent RSS figures that `course run` prints, at 64 connections summed `CACHE_USED` is about `134,567,360` bytes (`128.3 MiB`), while RSS growth is about `255.1 MiB` (`RSS growth / summed CACHE_USED = 1.99`) in one lab run, about `4.0 MiB` per connection at both 32 and 64. That `~2x` factor is a property of `modernc.org/memory` v1.12.1, whose power-of-two allocator places each ~4.3 KiB page allocation into an 8 KiB slot; a C build using glibc `malloc` would show much smaller allocator overhead. File-backed RSS rose by about `2.0 MiB` (from `7.1` to `9.1 MiB`), largely the 64 × 32 KiB (`2.0 MiB`) WAL `-shm` mappings.
  - Closing all 64 connections drops the cached pages to `0` and RSS from about `261.8 MiB` to `33.5 MiB` (the remaining `26.7 MiB` growth is retained Go/driver allocator arenas).
- **C1 (Part C: 64 open connections to one file).** Every `cached_pages_sum` row equals Part B's (`30,848` at 64), and the run-dependent RSS growth matches (about `250.2 MiB` in one lab run). Opening 64 connections to one database does not share a page cache; each connection adds its own `cache_size` share to the process total. File-backed RSS stayed at about `7.1 MiB` instead of rising by `2.0 MiB` because all 64 connections share one `-shm` mapping.
- **D1 (Part D: 64 files visited through an 8-connection LRU pool).** Up to 8 connections, cached pages climb to `3,856` (`8 × 482`) and RSS growth reaches about `35.7 MiB` (RSS is run-dependent). Across connections 9 through 64, closing the least recently used connection before opening the next keeps open connections at `8`, cached pages flat at `3,863 <= 8 × (482 + 1) = 3,864` (the true `PGroup.nMaxPage` cap: closing an evicted 0-page connection leaves `pGroup->nMaxPage = 7 × 483 = 3,381`, and the newly opened scanner adds `482`, totaling `3,863`), and RSS growth flat at about `36.1 MiB`—`14%` of Part B's `255.1 MiB` in the same run (matching `8/64 = 12.5%` within the `1.5x` bound).
- **E1 (Part E: `mmap`).** Connection 1 (no `mmap`) scans `f-000` and holds `482` cached pages (`hit=7 miss=1005`; run-dependent `CACHE_USED` about `2,102,615` bytes and RSS about `18.2 MiB` in one lab run). Connection 2 sets `PRAGMA mmap_size=8224768` (twice the file) and scans `f-001` (which is cleanly closed and checkpointed, so no page sits in `-wal`): it holds only `2` cached pages (`hit=7 miss=3`; `CACHE_USED` about `13,655` bytes). Total RSS still rises by about `+4.1 MiB` (the `3.9 MiB` file size), and `/proc/self/status` shows where it went (run-dependent, printed by `course run`): file-backed RSS about `+4,145,152` bytes and anonymous RSS about `+135,168` bytes. `mmap` does not eliminate memory usage—it moves the read cache for checkpointed `.db` pages from SQLite's private heap into the OS kernel's shared, reclaimable page cache.

---

## 6. Evidence and failure cases

### 6.1 Cold journals versus hot journals

For a transaction that fits in the page cache (`spill=0`), a rollback journal under `synchronous=FULL` or `NORMAL` starts with 12 zero bytes (`writeJournalHdr`, `src/pager.c:1500-1502`) until `syncJournal` writes the 12-byte magic and record count at offset 0 (call #45 in Section 4). Experiment 4.1 Part D shows what happens if the writer dies during that window (kills 1–25 at `FULL`, kills 1–22 at `NORMAL`):

- `t.db` still holds the exact pre-step bytes.
- `t.db-journal` has first byte `0x00` (not hot) and header `not-yet-synced`.
- On reopen, `hasHotJournal` (`src/pager.c:5244-5251`) reads byte 0, sees `0x00`, and ignores the file without deleting it (`cold, 29240 bytes, 0x00` left behind).

A cold `-journal` left beside a database is harmless because no write reached `t.db` before the header was activated. Once a transaction overflows `cache_size` and spills via `pagerStress`, however, `syncJournal` runs *before* `COMMIT`: the journal's first byte flips to `0xd9` (hot), dirty pages reach `t.db` before `COMMIT`, and subsequent writes open new sector-aligned segments (`JRN-04`, `JRN-05a`). A platform check that asks "is this database at rest?" by testing only whether `<db>-journal` exists will misclassify a cold journal; the right test is `journalfile.LooksHot()` (`size > 0 && firstByte != 0`), matching `hasHotJournal` and Chapter 2's `capture.CheckAtRest`.

### 6.2 Why hot-journal rollback is not always byte-exact (`PAGER_GET_NOCONTENT` and post-commit truncation)

Hot-journal recovery always restores a database that is **logically** pre-step or post-step, never a third logical state. At the raw byte level, two SQLite mechanisms can make a crashed file differ from the exact pre-step or post-step SHA-256:

- **Reused freelist leaves (`PAGER_GET_NOCONTENT`, Experiment 4.1 Parts D and D2).** In step 3 (freelist leaf `22`), kills 1–32 at `FULL` reopen to pre-step exact bytes (`B`). Kill 32 lands on entry to `pwrite64 t.db off=86016 len=4096`—the write of page 22 (`21 × 4096 = 86016`). From kill 33 through kill 46 (`unlink t.db-journal`), page 22's new bytes are on disk. Because `allocateBtreePage` (`src/btree.c:6740`) fetched page 22 with `PAGER_GET_NOCONTENT`, `getPageNormal` (`src/pager.c:5645-5656`) marked page 22 in `pInJournal` without writing a journal record for it. When SQLite replays the hot journal on reopen, it restores the freelist trunk page—putting page 22 back on the freelist—but never overwrites page 22's bytes. With `secure_delete=ON`, pre-step page 22 was all zeros, so the leftover bytes make the raw file SHA-256 differ from both pre-step and post-step (`14` kills at `FULL`, `14` at `NORMAL`, `13` at `OFF`). Every one of those files passes `PRAGMA integrity_check`, has the exact pre-step size (`29` pages), differs from pre-step *only* in page 22, and produces the exact pre-step `VACUUM INTO` canonical hash (equality level E3 turned into E1). In Part D2 (step 1, no freelist leaves), with no freelist pages to reuse, every kill across all three `synchronous` levels (`42` at `FULL`, `41` at `NORMAL`, `37` at `OFF`) reopens to the exact pre-step or post-step bytes (`BBBB…BA`).
- **Post-commit truncation of a shrinking file (`pager_end_transaction`, `src/pager.c:2155-2164`; `RCM-06`).** `sqlite3PagerCommitPhaseOne` only extends an undersized file (`src/pager.c:6704-6715`); shrinking a database (for example, `DELETE` under `auto_vacuum=FULL`) calls `ftruncate` in `pager_end_transaction` *after* the journal is unlinked. Killing the writer on entry to that `ftruncate` leaves a committed database whose logical content is post-step and passes `integrity_check`, while the OS file still carries trailing slack pages beyond the header's page count (`155,648` bytes instead of `53,248` bytes in `RCM-06`).

### 6.3 Losing a hot journal (Experiment 4.1 Part E)

Part E kills the `synchronous=FULL` step-3 writer at two points and shows what happens if an operator, backup script, or copy tool loses `-journal` (evidence section E):

| Case | Crash point before action | Action taken | Reopen result |
| --- | --- | --- | --- |
| **E1** | Killed on entry to db write 10 of 18 (`pwrite64 t.db off=122880 len=4096`); database torn (30 pages, 9 differ from pre, 9 differ from post); hot `-journal` present | Delete `t.db-journal`, reopen `t.db` | `open failed: open t.db: PRAGMA page_count: database disk image is malformed (11)` (`unreadable`) |
| **E2a** | Same torn database and hot `-journal` | Copy `t.db` alone to another directory, open copy | `open failed: open t.db: PRAGMA page_count: database disk image is malformed (11)` (`unreadable`) |
| **E2b** | Same torn database and hot `-journal` | Copy `t.db` **and** `t.db-journal` together, open copy | integrity `ok`; `user_version` `2`, accounts `15`, documents `7`, events `108` (pre-step except freelist leaf 22) |
| **E3** | Killed on entry to `sync t.db` (all 18 database writes done, `COMMIT` never returned, hot `-journal` present) | Delete `t.db-journal`, reopen `t.db` | integrity `ok`; `user_version` `3`, accounts `21`, documents `10`, events `147` (`post-step exact`) |

Case E3 is as dangerous as E1: deleting the hot journal after the last database write produces a clean `integrity_check: ok`, turning a transaction that died inside `COMMIT` and never acknowledged to the application into a committed transaction. With the journal kept (kill 45 in Part D), SQLite rolled that exact state back to pre-step.

### 6.4 The commit point across `DELETE`, `TRUNCATE`, and `PERSIST` (Experiment 4.1 Part F)

Part F traces step 3 at `synchronous=FULL` in all three disk-backed rollback modes and kills the writer on entry to the finalization syscall and on the syscall immediately after it (evidence section F):

| `journal_mode` | Syncs at `FULL` | Commit-point syscall | Killed on entry | Killed on next syscall | `-journal` left after clean commit |
| --- | --- | --- | --- | --- | --- |
| `delete` | `4` | `unlink t.db-journal` | pre-step (logical) | post-step (kill on `step-returned`) | no `-journal` |
| `truncate` | `5` | `ftruncate t.db-journal len=0` | pre-step (logical) | post-step (kill on `journal-sync`) | `0` bytes (not hot) |
| `persist` | `5` | `pwrite64 t.db-journal off=0 len=28` | pre-step (logical) | post-step (kill on `journal-sync`) | `29240` bytes, first byte `0x00` (not hot), header `zeroed` |

In all three modes, a kill on entry to the commit point rolls back to pre-step and a kill on the very next syscall opens to post-step. `TRUNCATE` and `PERSIST` avoid directory-entry creation/deletion overhead on every transaction, at the cost of a fifth sync (`fsync t.db-journal`) at `synchronous=FULL` (`pager_end_transaction`, `src/pager.c:2091-2105`) and leaving a cold `-journal` file beside the database. By contrast, `journal_mode=MEMORY` and `journal_mode=OFF` keep no journal on disk and leave a damaged database on a mid-write `SIGKILL` (`RCM-05`).

### 6.5 Cache boundaries and driver-specific behaviors

Five cache behaviors matter when sizing or tuning a Go service using `modernc.org/sqlite`:

- **`PRAGMA shrink_memory` is process-wide under `ENABLE_MEMORY_MANAGEMENT` (`CAC-02a`, `CAC-02b`).** Calling `PRAGMA shrink_memory` on a single idle connection holding `0` pages zeroes the global `PGroup.nMaxPage` temporarily (`pcache1Shrink`, `src/pcache1.c:839-851`) and evicts every other connection's unpinned clean pages (`7,712 -> 0` pages across 16 connections; RSS growth `65.6 -> 13.9 MiB`). Cursor-pinned pages (`4`) and uncommitted dirty pages (`41`) survive until the cursor closes or the transaction finishes.
- **`PRAGMA soft_heap_limit` does not cap `modernc`'s default allocator (`CAC-03`).** Setting `PRAGMA soft_heap_limit=4 MiB` on connection 1 is visible to other connections, but without `SQLITE_ENABLE_MEMSYS` it does not prevent 32 connections at `cache_size=-2000` from growing linearly to `15,424` cached pages (`64.2 MiB` `CACHE_USED`, `128.5 MiB` RSS).
- **`mmap` does not bypass `pcache1` for uncheckpointed WAL pages (`CAC-04`).** Because `getPageMMap` checks `sqlite3WalFindFrame` first (`src/pager.c:5719-5726`), scanning 500 leaf pages sitting in an uncheckpointed `-wal` cached `501` pages even with `mmap_size = 4×` file size (vs `502` unmapped), dropping to `2` pages only after a checkpoint (and `253` pages with half-file `mmap_size`).
- **Shared-file `mmap` accounting on Linux: `RssFile` versus `Pss_File` (`SCL-05`).** When 1,000 connections in one process `mmap` 1,000 distinct 408 KiB files, both `RssFile` (`+432.4 MiB`) and `Pss_File` (`+431.2 MiB`) grow with the distinct files. When 1,000 connections `mmap` the *same* 408 KiB file, `/proc/self/status` `RssFile` sums resident PTEs across all 1,000 VMAs (`+401.6 MiB`), while `/proc/self/smaps_rollup` `Pss_File` reflects the single shared kernel page-cache copy (`+2.3 MiB`).
- **Tiny/huge `cache_size` and the second-pass `N × nMax` ceiling (`CAC-01a`, `SCL-07`).** The `N - 1` rule holds for `N >= 10` (`10 -> 9`, `100 -> 99`, `200 -> 199`, `483 -> 482`). Tiny caches (`N <= 2`) rest at `N` (`0 -> 0`, `1 -> 1`, `2 -> 2`) and hold `4` cursor-pinned pages mid-scan; huge budgets (`±1,000,000`) allocate lazily (`1,004` pages). Under repeated round-robin sweeps across 16 open connections (`cache_size=-2000`, `nMax=483`), pass 1 sums to `16 × 482 = 7,712` and pass 2 reaches and holds `pGroup->nMaxPage = 16 × 483 = 7,728` (`src/pcache1.c:902, 1096`).

### 6.6 Boundaries not exercised live here

Three behaviors discussed in this chapter are read from the pinned source or unit-tested rather than exercised live in Experiments 4.1–4.2:

- **Separate per-connection `PGroup` without `SQLITE_ENABLE_MEMORY_MANAGEMENT`.** The installed C `sqlite3` CLI lacks `ENABLE_MEMORY_MANAGEMENT` (`src/pcache1.c:720-721`), giving each connection its own private LRU. That path is read from `pcache1.c`, not measured across multiple connections.
- **Unsigned-`char` super-journal checksums on ARM64 Linux hardware.** `internal/journalfile` unit-tests the unsigned `char` checksum path (`0x0f98` vs `0x0d98`) by toggling `cCharIsSigned` in `TestSuperJournalChecksumWhereCCharIsUnsigned`; it was not executed on an ARM64 Linux host.
- **Real power loss, torn sector writes, and disk write reordering.** Experiment 4.1 tests process crashes (`SIGKILL`), where completed writes survive in the kernel buffer cache. Power loss and unsynced writes are modeled in Chapter 9.

### What attacked this chapter

<!-- adversarial-results -->

Four adversarial programs (`32` attacks total, driven by [`run-all.sh`](../agent_work/ch04/adversarial/run-all.sh)) attacked this chapter's claims, `internal/journalfile`, and Experiments 4.1 and 4.2 against the pinned SQLite 3.53.4 build:

- [`journal-parser`](../agent_work/ch04/adversarial/journal-parser/main.go) (`JRN-01`–`JRN-05b`, 8 attacks): attacked `internal/journalfile` with sub-sector journals (`28` and `100` bytes < `512`-byte sector) compared against real SQLite reopen, non-ASCII UTF-8 and NUL-leading super-journal footers, a hot journal pointing to an absent super-journal file, live SQLite rollback journals across all 8 valid page sizes (`512..65536`), sampled (`3896`) vs unsampled (`1097`) single-byte flips against `pager_cksum`, a mid-transaction `pagerStress` spill (`cache_size=10`, 55-page `UPDATE`), multi-segment spilled journals under `FULL` and `OFF`, and 5,000 seeded parser/playback fuzz mutations.
- [`rollback-crashes`](../agent_work/ch04/adversarial/rollback-crashes/main.go) (`RCM-01`–`RCM-07`, 9 attacks): attacked Experiment 4.1 by deleting/omitting hot journals across `DELETE`, `TRUNCATE`, and `PERSIST`, killing a spilling `synchronous=OFF` transaction at 6 syscall points and running 30 asynchronous `SIGKILL`s during an 8-step `OFF` loop, tracing and killing spilling `FULL`/`NORMAL` commits across multiple journal segments, killing `sqlitex.Open(DefaultOptions())` on a brand-new WAL database at all 27 write/sync/unlink points (`4` left hot journals, `4` left cold journals, `0` recovery failures on retry), testing a hot journal beside a 0-byte database plus `journal_mode=MEMORY` and `OFF` mid-write kills, killing a shrinking `auto_vacuum=FULL` `DELETE` around `unlink` and `ftruncate`, and testing pin-mismatch enforcement on mutated `sqlite/PIN` and a bogus `SQLITE3_BIN`.
- [`cache-limits`](../agent_work/ch04/adversarial/cache-limits/main.go) (`CAC-01a`–`CAC-05`, 8 attacks): attacked Experiment 4.2 with tiny (`0, 1, 2, 10, -1, -10`) and huge (`±1,000,000`) `PRAGMA cache_size` values, positive `-cache-size 500`, small negative `-cache-size -50` and minimum `-1024`, `PRAGMA shrink_memory` invoked on an idle 0-page connection and while another connection holds pinned cursor pages and 40 dirty uncommitted pages, `PRAGMA soft_heap_limit=4 MiB` across 32 connections, `PRAGMA mmap_size` with 500 uncheckpointed WAL pages vs checkpointed vs half-file mapping, and short (`74` B), max-valid (`480` B < `512` B), and over-`MAX_PATHNAME` (`682` B) paths.
- [`cache-scale`](../agent_work/ch04/adversarial/cache-scale/main.go) (`SCL-01`–`SCL-07`, 7 attacks): scaled Experiment 4.2 to **1,000 open connections** (`cache_size=-256 KiB` on 102-page WAL files to fit 7 GiB RAM) across 1,000 distinct files (`60,000` cached pages, `602.1 MiB` RSS, `3,007` FDs), 1,000 connections to one file (`60,000` pages, `570.3 MiB` RSS, `2,008` FDs), 1,000 distinct files through a `K=16` LRU pool (`984` evictions, `975/976` pages, `14.5 MiB` RSS, `55` FDs), 1,000 heterogeneous-budget connections (`500 × -384 KiB` + `500 × -128 KiB`, verifying immediate eviction on closing the 500 large connections and `>= 75%` RSS drop on closing all 1,000), 1,000 `mmap`-ed connections (1,000 distinct files vs 1 shared file using `Pss_File`), Experiment 4.2 itself at 4× scale (`-n 256 -k 16 -cache-size -1024 -rows 320`), and a second round-robin scan across 16 WAL connections verifying the `N × (nMax - 1)` to `N × nMax` (`7,712 -> 7,728`) transition.

**Run 1** ([`results-run1.txt`](../agent_work/ch04/adversarial/results-run1.txt), first three groups) produced **12 HELD, 12 BROKEN** (and `cache-limits` exited non-zero on `CAC-05`'s over-`MAX_PATHNAME` path). **Run 2** ([`results-run2.txt`](../agent_work/ch04/adversarial/results-run2.txt), after the repair round and adding `cache-scale`) produced **21 HELD, 10 BROKEN**.

**Code defects exposed and fixed in repair:**

- `JRN-01`, `JRN-05b` (and review blocker 1): `internal/journalfile.Parse` previously computed a negative `TrailingByte` (`-412` on a 100-byte journal; `221` negative cases in 5,000 fuzz mutations) and predicted truncation for journals shorter than one sector; fixed to mark sub-sector journals `HeaderMissing` with `TrailingByte = end` (`src/pager.c:1607-1610`).
- `JRN-02a`, `JRN-02b` (and review blocker 2): `superJournalName` previously summed unsigned bytes unconditionally (rejecting non-ASCII UTF-8 names on signed-`char` platforms) and accepted NUL-leading names, while `Playback()` predicted unconditional rollback; fixed to match C `char` signedness (`cCharIsSigned`), reject `name[0] == 0` and lengths `> 512`, and refuse unconditional rollback in `Playback()` (adding `PlaybackIfSuperJournalExists()`).
- `CAC-01b`, `CAC-01c`: Experiment 4.2 previously checked positive `-cache-size` in bytes instead of pages (failing `-cache-size 500`), compared Part D against `K × full × 1.01` instead of the true `PGroup.nMaxPage = K × (full + 1)`, and panicked on `-rows 50` (review finding 3); fixed with `withinBudget`, `K × (full + 1)`, and flag validation requiring `-rows >= 1.2 × max(200, budget pages)` and budget `>= 1 MiB` (cleanly rejecting `-cache-size -50`).

**Claims narrowed or corrected in this chapter (rather than changing SQLite behavior):**

- `JRN-04`, `RCM-03a`: cold pre-`COMMIT` journals, untouched `.db` before `COMMIT`, and the `4 / 3 / 0` sync counts apply only to non-spilling transactions (`spill=0`); spilling via `pagerStress` activates the journal early, interleaves segments with `.db` writes, and multiplies syncs (`20` at `FULL`, `11` at `NORMAL`).
- `RCM-06`: `sqlite3PagerCommitPhaseOne` only extends undersized files (`src/pager.c:6704-6715`); shrinking commits truncate in `pager_end_transaction` *after* `unlink` (`src/pager.c:2155-2164`), leaving logical post-step content with trailing slack pages if killed on `ftruncate`.
- `CAC-01a`: `N -> N - 1` applies to `N >= 10`; tiny caches (`N <= 2`) rest at `N` and hold `4` cursor-pinned pages mid-scan.
- `CAC-02a`: under `SQLITE_ENABLE_MEMORY_MANAGEMENT`, `PRAGMA shrink_memory` on any connection empties every connection's unpinned clean pages (`pcache1Shrink`, `src/pcache1.c:839-851`).
- `CAC-04`: `mmap` bypasses `pcache1` only for pages with no frame in `-wal` (`getPageMMap`, `src/pager.c:5719-5726`).
- `SCL-07`: `open × (nMax - 1)` holds on a first sweep; repeated sweeps and pool churn reach `pGroup->nMaxPage = open × nMax` (`src/pcache1.c:902, 1096`).

Two attack-side bugs were also fixed before the final run (`CAC-05` generated a ~1,550-byte path exceeding Unix `MAX_PATHNAME = 512`, and `SCL-05` originally checked `RssFile` instead of `Pss_File` for shared-file mappings). After aligning the stale attack expectations to the narrowed rules, the final run ([`results-run3.txt`](../agent_work/ch04/adversarial/results-run3.txt) and [`results.txt`](../agent_work/ch04/adversarial/results.txt)) passed **32 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every operational rule below is backed directly by this chapter's experiments and verified source spans:

1. **A hot `-journal` is part of the database; copy both or refuse (Experiment 4.1 Parts D and E, `RCM-01`).** Copying a crashed rollback-mode `.db` file without its hot `-journal` produced `database disk image is malformed (11)` (E2a), while copying both together recovered cleanly to pre-step (E2b). Backup, move, and capture tools must either copy `<db>` and `<db>-journal` together or refuse any file with a hot journal (as `capture.CheckAtRest` does in Chapter 2).
2. **Never delete a `-journal` file to "fix" a database (Experiment 4.1 Part E1, E3).** Removing a hot journal mid-write corrupts the database (`malformed (11)`). Removing it after the last database write silently turns an unacknowledged crashed commit into a committed transaction (`user_version=3` instead of `2`).
3. **Test for a *hot* journal, not merely for the `-journal` filename (Experiment 4.1 Parts D and F).** Cold journals (first byte `0x00`) remain after pre-sync crashes in `DELETE` mode and after every clean commit in `TRUNCATE` (0 bytes) and `PERSIST` (28 zeroed header bytes) modes. Use `size > 0 && firstByte != 0` (`journalfile.LooksHot`) to test whether a database is at rest.
4. **Rollback mode costs at least 3 (`NORMAL`) or 4 (`FULL`) syncs per commit and writes every modified page twice (Experiment 4.1 Part D, `RCM-03a`).** Step 3 (non-spilling) wrote 7 pages to `-journal`, synced twice (plus a directory sync at `FULL`), wrote 18 pages to `t.db`, and synced `t.db`; transactions that spill past `cache_size` multiply those syncs (`20` at `FULL`, `11` at `NORMAL` in `RCM-03a`). Chapter 1 showed WAL mode needed 1 sync per commit after the first; Chapter 7 measures the full I/O matrix across modes.
5. **Every newly created WAL database passes through one rollback-journal commit, and imported files start in rollback mode (Experiment 4.1 Part A, `RCM-04`).** Even `sqlitex.Open` with `journal_mode=wal` opens, syncs twice, and unlinks a `-journal` when initializing a brand-new file; a crash mid-open can leave a hot `-journal` that a retry cleans up. A platform opener must explicitly switch new files to WAL and refuse to silently flip existing files whose mode disagrees with policy.
6. **Verify rollback-mode crash recovery at E3 (or canonical E1), not raw E1 (Experiment 4.1 Parts D and D2, `RCM-06`).** Because `PAGER_GET_NOCONTENT` skips journaling reused freelist leaves (`14` of `47` kills in step 3) and shrinking commits truncate *after* journal unlink (`pager_end_transaction`, `src/pager.c:2155-2164`), a valid rollback or post-commit recovery can leave unused bytes in freelist leaves or trailing slack pages. Compare recovered rollback-mode files with `VACUUM INTO` canonical SHA-256 or logical equality (E3), not raw file SHA-256 (E1).
7. **Size a multi-database Go service by its open-connection cap, not its file count (Experiment 4.2 Parts B, C, D, `SCL-01`–`SCL-03`).** Page-cache memory scales linearly with open connections—even when multiple connections open the same file (`134.6 MB` `CACHE_USED`, `~250–255 MiB` RSS for 64 connections at `cache_size=-2000`; confirmed to 1,000 connections in `SCL-01` and `SCL-02`). An 8-connection LRU pool capped memory flat at about `36.1 MiB` across 64 files (RSS is run-dependent) (and a 16-connection pool held `14.5 MiB` across 1,000 files at `-256 KiB` in `SCL-03`).
8. **Account for `ENABLE_MEMORY_MANAGEMENT` pooling, process-wide `shrink_memory`, and the driver's `~2x` RSS multiplier (Experiment 4.2 Parts B and D, `CAC-02a`, `SCL-07`).** With `modernc.org/sqlite`, `cache_size` adds to one process-wide `PGroup` budget capped at `open × nMax` pages (`open × 483` at `-2000`), so active connections automatically reuse idle connections' pages and closing an idle connection frees its fixed overhead (~5 KiB `CACHE_USED`, 32 KiB `-shm` mapping, Go/driver structs) while shrinking the group cap. Never call `PRAGMA shrink_memory` expecting it to affect only one connection—in this build it evicts every connection's unpinned clean pages across the whole process. Budget roughly `2 × summed CACHE_USED` for process RSS (`~4.0 MiB` per connection at `-2000`) because `modernc.org/memory` rounds ~4.3 KiB page allocations into 8 KiB slots.
9. **Use `PRAGMA mmap_size` to move read-cache memory for checkpointed `.db` pages into the kernel page cache (Experiment 4.2 Part E, `CAC-04`, `SCL-05`).** Covering the checkpointed 3.9 MiB file with `mmap_size` cut `CACHE_USED` from `482` pages (`2.1 MB`) to `2` pages (about `13.7 KB`; raw `CACHE_USED` bytes are run-dependent) while shifting the `4.0 MiB` resident footprint into file-backed RSS (`RssFile`), which the OS shares across processes and reclaims under memory pressure. Pages still sitting in an uncheckpointed `-wal` fall back to `pcache1` (`CAC-04`), and on Linux use `Pss_File` (`/proc/self/smaps_rollup`) rather than `RssFile` when measuring shared-file mappings across many connections in one process (`SCL-05`).

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 4.1 Part D (`synchronous=FULL`), why did killing the writer during kills 1–25 leave a `-journal` file on disk that SQLite ignored and did *not* delete on reopen, whereas kills 26–46 triggered rollback and deleted `-journal`—and what condition on the transaction's size kept the journal cold until `COMMIT`?
2. In Experiment 4.1 Part D (step 3), 14 kill points at `synchronous=FULL` reopened to a database whose raw SHA-256 matched neither pre-step nor post-step, yet Part D2 (step 1) reopened to exact pre-step or post-step bytes at every kill point. What exact SQLite mechanism caused the difference, which page differed in step 3, and how did the experiment verify the database was still logically pre-step?
3. Compare what happened in Experiment 4.1 Part E when `t.db-journal` was deleted after a crash at database write 10 of 18 (E1) versus after a crash on entry to `sync t.db` (E3). Why is E3 dangerous even though `PRAGMA integrity_check` returns `ok`?
4. In Experiment 4.2 Part B (`64` connections, `cache_size=-2000`), connection 1 held `0` cached pages and connection 64 held `1,004` cached pages—more than double its `482`-page budget—while the sum across all 64 connections was `30,848` (`64 × 482`). Which compile option and `pcache1.c` mechanism caused this, what is the true group ceiling under pool churn or repeated sweeps, and why did RSS grow by about `255 MiB` (a run-dependent value printed by `course run`: `255.1 MiB` in one lab run) when summed `CACHE_USED` was only `128.3 MiB`?
5. In Experiment 4.2 Part E, setting `PRAGMA mmap_size=8224768` cut `CACHE_USED` from about `2,102,615` bytes (`482` pages) to about `13,655` bytes (`2` pages) in one lab run (raw `CACHE_USED` bytes and RSS are run-dependent values printed by `course run`), yet process RSS still grew by about `4.1 MiB`. What happened to the memory, how did `/proc/self/status` prove it, and when does `getPageMMap` fall back to `pcache1` in WAL mode?

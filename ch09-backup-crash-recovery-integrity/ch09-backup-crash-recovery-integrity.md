# Chapter 9 — Backup, crash recovery, and integrity

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `backup.c`, `vacuum.c` |
| Linked C sources (in the pinned tree, not copied) | `src/btree.c`, `src/pager.c`, `src/pragma.c`, `src/wal.c`, `src/wal.h` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree (for inspecting the linked C files), the C tools (none of Chapter 9's four experiments invokes the C `sqlite3` CLI; only the adversarial script `capture-falsify` does, to reproduce CLI `.backup` lock contention), `strace` (Experiment 9.2's syscall cross-check and kill sweep), and the DuckDB CLI that presents the results.
> - **Run an experiment** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 9.4`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 9.1
>   go run ./cmd/course run 9.2
>   go run ./cmd/course run 9.3
>   go run ./cmd/course run 9.4
>   ```
>   Experiment 9.2 also accepts `-portable` to run the non-`strace` path on Linux. `course run` cannot pass experiment flags, so run that variant directly in the container: `docker compose run --rm lab go run ./ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss -portable`. In the container, scratch files and tables are written under `/work/9.1/` through `/work/9.4/` (the `lab-work` volume); run directly, under `_work/9.1/` through `_work/9.4/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch09-backup-crash-recovery-integrity/experiments/9.4-point-in-time-rebuild`). Run this way it prints only its verdict and leaves its tables under `_work/9.4/results/`.
> - **Linux-only parts and macOS:** Experiments 9.1, 9.3, and 9.4, plus `internal/walship` and Experiment 9.2's power-cut model (Part B's rule calculation and recovery), are portable. Experiment 9.2's `strace` confirmation of synced bytes (Part B) and syscall kill sweep (Part A) are **Linux only**; `course run` runs them in the Linux `lab` container. Run directly on macOS, Experiment 9.2 runs the process-crash test and the rule-based power-cut model natively and prints `LINUX-ONLY:` notices for the `strace` parts.
> - **Measurement environment:** The stored evidence in this chapter, including the `strace` counts and kill sweeps, was captured in the Debian `lab` container on `linux/arm64` (Go 1.26). The checked values are identical to the earlier `linux/amd64` capture. macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and cross-vetted only; nothing was run on macOS.

---

## 1. Model

Running a platform that holds thousands of SQLite databases requires answering three operational questions with precision: how to save a live database without stopping writers, what commits survive a crash versus a power cut, and how to prove a restored database holds the right data.

### Three families of capture

An SQLite database in **WAL** (write-ahead log) mode consists of fixed-size **pages** (4,096 bytes in the course recipe) stored in the main `.db` file, plus committed **frames** (each a 24-byte header and one full page image) appended to the `-wal` file and indexed in shared memory via `-shm` (the **wal-index**). Every way to capture a database's state belongs to one of three families, each reaching a specific level of the course's **equality ladder** (`internal/equal`):

1. **Physical page captures** copy raw pages directly:
   - A **closed-file copy** (Chapter 8, rung R5) copies `.db` after the last connection closes cleanly. It reaches **E1 bytes** (exact SHA-256 match). On a live WAL database, copying `.db` alone without `-wal` misses uncommitted-to-`.db` WAL frames and yields a stale (or, under a concurrent checkpoint, torn) file.
   - **Reading every page through `sqlite_dbpage` inside one read transaction** (Chapter 6, rung R4) asks SQLite's pager to resolve each page `1..N` from the snapshot pinned by that transaction. Writing those pages out in order reaches **E1 bytes** without blocking writers.
   - The **online backup API** (`sqlite3_backup_init` / `sqlite3_backup_step` / `sqlite3_backup_finish`) copies pages into a destination database connection, either all at once (`nPage = -1`) or in fixed-size steps. Because SQLite rewrites page-1 header fields on the destination—specifically bytes `24–27` (change counter), `40–43` (schema cookie), and `92–99` (version-valid-for and SQLite version)—a backup reaches **E2 pages** (identical pages outside those declared header ranges), not E1 bytes.
2. **Logical captures** rebuild a new database from the SQL schema and row values:
   - **`VACUUM INTO`** opens a read transaction on the source, creates a fresh destination database, executes `INSERT INTO … SELECT * FROM …` for every table, increments the schema cookie, and commits. Because it repacks every B-tree and always outputs a rollback-mode file (header bytes `18–19 = 1`), its output is neither E1 nor E2 equal to a WAL source, and under the course's **E3 logical** hash (which includes `PRAGMA journal_mode`) it matches **E3 except `journal_mode`**, and reaches **E1 between canonical forms** (`VACUUM INTO` of both files).
3. **Log captures** save a base image plus committed WAL frames and replay them:
   - **Copying `.db` then `-wal` under a held read transaction** prevents any checkpoint from overwriting pages older than the reader's snapshot; replaying `-wal` onto `.db` with `internal/walfile` reaches **E1 bytes** for the last commit present in `-wal` when it was copied.
   - **Continuous WAL shipping (`internal/walship`, rung R6)** turns that one-shot technique into a stream: one base **snapshot** (taken through `sqlite_dbpage`) followed by numbered, hash-chained **segments** of raw WAL frames copied before the shipper runs its own `PASSIVE` **checkpoint** (copying committed WAL frames back into `.db`, called **backfilling**). Restoring replays a snapshot plus segments up to any chosen commit point and proves **E1 bytes** against the state at that commit.

### Two families of failure

Do not treat a process crash and a power cut as the same failure:

1. **Process crash (`SIGKILL`, panic, OOM kill):** the user-space process dies instantly, but the operating system kernel stays up. Every `pwrite64` or `write` syscall that already returned sits intact in the kernel's **page cache** (OS memory buffers waiting to be written to disk). When the next connection opens the database, SQLite scans `-wal` (`walIndexRecover`), stops at the first incomplete or invalid frame, and keeps every committed transaction whose commit frame reached the kernel—regardless of `PRAGMA synchronous`.
2. **Power loss (or kernel crash):** volatile RAM disappears, including the OS page cache. Only bytes flushed to durable storage by **`fsync`** or **`fdatasync`** are guaranteed to survive. In WAL mode, `PRAGMA synchronous` controls which operations call `fsync`:
   - `OFF`: zero syncs on either `-wal` or `.db`—even a checkpoint writes `.db` without syncing it.
   - `NORMAL`: syncs `-wal` when writing a new WAL header and during checkpoints, and syncs `.db` when a checkpoint backfills every frame; **zero syncs on normal commits**. A power cut can roll the database back to the last checkpoint while leaving it internally consistent.
   - `FULL` (SQLite's default): adds one `-wal` `fsync` after every commit frame. Every acknowledged commit has already been synced before `COMMIT` returns.

### Verification: structure versus content

`PRAGMA integrity_check` walks B-tree page headers, cell bounds, key order, freelist pointers, and table-versus-index cross-checks. It proves the database file is structurally valid and self-consistent. It does **not** prove the bytes inside your rows are unchanged—a flipped byte in an unindexed column or a freelist leaf passes `integrity_check` with `ok`. Proving restored content requires cryptographic hashes: either a per-page SHA-256 **manifest** (`internal/capture`) or the full-image **state hash** recorded per commit in `internal/walship`.

```text
  CAPTURE FAMILIES                            FAILURE BOUNDARIES
  ----------------                            ------------------
  1. Physical pages                           +-----------------------------+
     - closed-file copy (R5)        -> E1     | Process (Go + SQLite)       |
     - sqlite_dbpage in read txn    -> E1     |   commits -> pwrite64       |
     - backup API (restarts on ext) -> E2     +--------------+--------------+
                                                             |  <-- Process crash (SIGKILL):
  2. Logical content                                         |      completed writes survive
     - VACUUM INTO (rollback out)   -> E3*                   v      in kernel page cache
       (*except journal_mode; E1 canonical)   +-----------------------------+
                                              | OS Kernel Page Cache        |
  3. Log + replay (R6: internal/walship)      |   .db and -wal dirty pages  |
     - Snapshot (sqlite_dbpage)               +--------------+--------------+
     - Held read mark (pin conn)                             |  <-- Power cut:
     - Read -wal -> hash-chained segments                    |      only fsync'd bytes survive
     - Shipper-owned PASSIVE checkpoint                      v      (NORMAL loses tail since ckpt)
     - Restore any commit k         -> E1     +-----------------------------+
                                              | Durable Storage (ext4/disk) |
                                              +-----------------------------+
```

**WAL mode as the platform default (ruling 10).** Every experiment in Chapter 9 runs in WAL mode (`journal_mode=wal`). The table below summarizes how WAL behaves across this chapter's measurements:

| Operational property | WAL behavior | Supporting evidence |
| --- | --- | --- |
| Live capture without blocking writers | Supported by four methods (`sqlite_dbpage`, file+WAL, `VACUUM INTO`, backup API) | Experiment 9.1 Part B |
| Continuous point-in-time capture to every commit | Supported directly because WAL frames are self-contained full page images with commit markers | Experiment 9.4 |
| Process crash recovery | Loses zero acknowledged commits at `OFF`, `NORMAL`, and `FULL` | Experiment 9.2 Parts A and B |
| Power cut at `synchronous=NORMAL` | Consistent database, but loses acknowledged commits written since the last checkpoint/header sync | Experiment 9.2 Part B |
| Power cut at `synchronous=FULL` | Loses zero acknowledged commits; costs one extra `-wal` `fsync` per commit | Experiment 9.2 Part B |
| Plain file copy of live `.db` (no `-wal`) | Valid but stale (missed committed WAL frames; Chapter 6 shows torn copies under checkpoint) | Experiment 9.1 Part B |

**Recorded exception:** `VACUUM INTO` of a WAL database always creates a rollback-mode output file (header bytes `18–19 = 1`, matching Chapter 3 and Chapter 8). A platform restoring from a canonical `VACUUM INTO` file must switch it back to `PRAGMA journal_mode=wal` after comparing hashes. Nothing in this chapter argues against WAL as the platform default; for durability against power loss, pair WAL with `synchronous=FULL` (local-durable) or `synchronous=NORMAL` with acknowledgement delayed until the WAL segment is durable in object storage (store-durable, built in Chapter 10).

**Where this chapter sits on the Rebuild Ladder (rung R6).** Rung **R6 (Point in time)** combines R1's page images with R3's WAL replay into a continuous capture stream: one base snapshot plus ordered, hash-chained WAL segments shipped across checkpoints and WAL restarts, rebuilding any committed position *k* byte-for-byte equal (**E1 bytes**) to the recipe's at-rest state for step *k*.

---

## 2. Question

Which methods of saving a live SQLite database produce a trustworthy copy and at what equality level, what survives a process crash versus what a power cut can take away at each `synchronous` level, why does `PRAGMA integrity_check` fail to prove content, and how can a continuous WAL shipper rebuild any committed state byte for byte across checkpoints?

This chapter reuses shared packages from earlier chapters (linked here, not inlined), with five small backward-compatible additions made for Chapter 9:

- [`internal/sqlitex`](../internal/sqlitex/): opens databases with explicit settings, checks `sqlite_source_id()`, and wraps `Conn.Raw` APIs. Added in [`raw.go`](../internal/sqlitex/raw.go): `SourceExecer` and `BackupWithSourceExec`, which let Experiment 9.1 commit SQL on the backup's own raw driver connection between steps while `Conn.Raw` holds `*sql.Conn` locked.
- [`internal/procs`](../internal/procs/): multi-process child harness and scripted SQL/copier roles. Added in [`sqlrole.go`](../internal/procs/sqlrole.go): action `recipe-session K`, which opens a fresh `SingleConnection` handle with `_txlock=immediate`, applies recipe step `K`, and closes that handle while the role's held connection keeps the WAL alive—reproducing the canonical recipe's page-1 change counter for exact E1 comparison.
- [`internal/dbfile`](../internal/dbfile/): read-only database file parser. Added in [`record.go`](../internal/dbfile/record.go): `RecordColumn`, returning the exact byte offset, length, and serial type of one column inside a record payload (used by Experiment 9.3 to flip one byte in a chosen column).
- [`internal/capture`](../internal/capture/): rung R1 page-manifest capture. Added in [`capture.go`](../internal/capture/capture.go): `VerifyImage`, hashing every page of a raw image against a manifest without parsing B-tree structures (so it identifies damaged pages even when `BuildManifest`'s census rejects a structurally broken header).
- [`internal/trace`](../internal/trace/): Linux `strace` runner, parser, and `KillAt` syscall injector. Added in [`sync.go`](../internal/trace/sync.go) (portable): `SyncsOf` and `SyncRecord`, summarizing successful `fsync`/`fdatasync` calls and every write (`pwrite64`, `pwrite`, `write`, `writev`, `ftruncate`) issued to a file after its last sync.
- [`internal/recipe`](../internal/recipe/), [`internal/equal`](../internal/equal/), and [`internal/walfile`](../internal/walfile/): deterministic workload, E1/E2/E3 comparison, and WAL parser/replayer.

This chapter introduces one new shared package: **`internal/walship`** (rung R6: object-store interface, `DirStore`, snapshot/segment wire format, WAL checksum-chain verification, catalog/planner/restore, and continuous `Shipper`), inlined in full and walked through in Section 5.

---

## 3. Call path

Every span cited below is listed in [`SPANS.txt`](SPANS.txt) and verified against SQLite `version-3.53.4` in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`). Two C files are copied whole into this chapter directory and verified by [`SOURCES.sha256`](SOURCES.sha256): [`backup.c`](backup.c) and [`vacuum.c`](vacuum.c). Five other files (`src/btree.c`, `src/pager.c`, `src/pragma.c`, `src/wal.c`, `src/wal.h`) are linked at the pinned tag.

### 3.1 Online backup API: stepping, same-connection updates, and restarts

1. **`struct sqlite3_backup` and `isFatalError`** — [`src/backup.c:21-42`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L21-L42) and [`src/backup.c:226-228`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L226-L228) (local copy: [`backup.c`](backup.c)). A backup handle tracks `iNext` (the next 1-based source page to copy), `nRemaining`, `nPagecount`, `iDestSchema` (the destination's pre-backup schema cookie), and a linked-list pointer `pNext` that attaches the backup handle to the source pager. `isFatalError` treats `SQLITE_BUSY` and `SQLITE_LOCKED` as retryable rather than fatal.
2. **`sqlite3_backup_step`, `sqlite3_backup_remaining`, and `sqlite3_backup_pagecount`** — [`src/backup.c:323-591`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L323-L591), [`652-660`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L652-L660), and [`666-674`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L666-L674) (local copy: [`backup.c`](backup.c)). Each call to `sqlite3_backup_step(p, nPage)`:
   - Immediately returns `SQLITE_BUSY` if the source connection is already inside a write transaction (lines `350–351`).
   - Opens a **read transaction** on the source if none is open, and closes it before returning (`bCloseTrans = 1`, lines `361–364` and `573–577`). Thus each step of a stepped backup is its own read transaction, whereas `nPage = -1` copies the entire database in one read transaction.
   - Takes a **write transaction** on the destination (`sqlite3BtreeBeginTrans(pDest, 2, (int*)&p->iDestSchema)`, lines `390–394`), capturing the destination's starting schema cookie in `iDestSchema`.
   - Reads `nSrcPage = sqlite3BtreeLastPage(p->pSrc)` (line `413`), copies up to `nPage` pages starting at `iNext` (lines `415–426`), recomputes `nPagecount` and `nRemaining` from the **current** `nSrcPage`, and attaches the backup handle to the source pager (lines `427–435`):
     ```c
     if( rc==SQLITE_OK ){
       p->nPagecount = nSrcPage;
       p->nRemaining = nSrcPage+1-p->iNext;
       if( p->iNext>(Pgno)nSrcPage ){
         rc = SQLITE_DONE;
       }else if( !p->isAttached ){
         attachBackupObject(p);
       }
     }
     ```
   - Notice why `nRemaining` is not a monotonic progress counter when the source grows: `nRemaining` is `nSrcPage + 1 - iNext`. The number of pages already copied is `nPagecount - nRemaining` (`= iNext - 1`).
   - When `rc == SQLITE_DONE`, line `448` updates the destination's schema cookie to **the destination's own previous cookie plus 1**—not the source's cookie:
     ```c
     if( rc==SQLITE_OK || rc==SQLITE_DONE ){
       rc = sqlite3BtreeUpdateMeta(p->pDest,1,p->iDestSchema+1);
     }
     ```
     For a freshly created destination file (`iDestSchema == 0`), the backup ends with schema cookie `1` regardless of the source's schema cookie. It then sets WAL version bytes if the destination is WAL (lines `454–456`), truncates the destination to match the source size, and commits it.
3. **Same-connection updates (`backupUpdate` / `sqlite3BackupUpdate`) vs external restarts (`sqlite3BackupRestart`)** — [`src/backup.c:688-712`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L688-L712), [`713-715`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L713-L715), and [`728-734`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L728-L734) (local copy: [`backup.c`](backup.c)), together with [`src/pager.c:3225-3282`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3225-L3282) (`pagerWalFrames`) and [`src/pager.c:1783-1787`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1783-L1787) (`pager_reset`, linked).
   - When the backup's **own connection commits**, `pagerWalFrames` (line `3270`, or `pager_write_pagelist` in rollback mode) calls `sqlite3BackupUpdate` for each modified page. If `iPage < p->iNext`, `backupUpdate` copies the updated page straight into the destination and leaves `p->iNext` untouched—no restart.
   - When **another connection commits**, the source pager sees a changed database at the next read transaction and calls `pager_reset` (`src/pager.c:1785`), which calls `sqlite3BackupRestart` and resets `p->iNext = 1`. Every previously copied page must be copied again.
   - A **same-connection write transaction that rolls back** in WAL mode also calls `sqlite3BackupRestart` (via `pagerUndoCallback`, `src/pager.c:3185`), resetting `iNext = 1` (confirmed by adversarial test `CAP-03a`). The no-restart guarantee applies to same-connection **commits**, not rollbacks.
4. **Why CLI `.backup` can return `"database is locked"` under WAL writers** — [`src/wal.c:2507-2571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2507-L2571) (`sqlite3WalClose`, linked). The `sqlite3` CLI's `.backup` command loops `sqlite3_backup_step(p, 100)` with no busy timeout unless the caller sets `.timeout`. When a short-lived WAL writer process closes and is the **last** connection to the database, `sqlite3WalClose` acquires an `EXCLUSIVE` lock on the `.db` file (lines `2520–2529`) to checkpoint and unlink `-wal`/`-shm`. Any `sqlite3_backup_step` that tries to begin a source read transaction (`src/backup.c:361-364`) during that window receives `SQLITE_BUSY` (`Error: database is locked`). Setting a busy timeout (or keeping one persistent connection open so writers are never the last to close) eliminates the failure (proven by adversarial tests `CAP-02a/b/c`).

### 3.2 `VACUUM INTO`

5. **`sqlite3RunVacuum`** — [`src/vacuum.c:143-427`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vacuum.c#L143-L427) (local copy: [`vacuum.c`](vacuum.c)). When `pOut != NULL` (`VACUUM INTO 'file'`), SQLite attaches the output file as a fresh temporary schema (`ATTACH %Q AS vacuum_<random>`, line `226`), configures it with `synchronous=OFF` for the bulk copy, and opens only a **read** transaction on the source (line `270`):
   ```c
   rc = sqlite3BtreeBeginTrans(pMain, pOut==0 ? 2 : 0, 0);
   ```
   A plain `VACUUM` (`pOut == 0`) passes `2` (write transaction), blocking writers; `VACUUM INTO` passes `0` (read transaction), so it runs concurrently even while another connection holds an active write transaction. It replays `sqlite_schema` DDL into the output, runs `INSERT INTO vacuum_db.x SELECT * FROM main.x` for each table, copies the five metadata fields in `aCopy[]` (lines `358–364`: `BTREE_SCHEMA_VERSION` incremented by `1`, plus `DEFAULT_CACHE_SIZE`, `TEXT_ENCODING`, `USER_VERSION`, and `APPLICATION_ID` unchanged), skips `sqlite3BtreeCopyFile` (lines `378–380`), and commits the output. Because the attached output is a newly created database file, its `journal_mode` is rollback (`DELETE`, header bytes `18–19 = 1`), even when the source is in WAL mode. And because `VACUUM INTO` adds `1` to the source's existing schema cookie, running `VACUUM INTO` on a backup-API copy (cookie `1`) versus the original source (cookie `4`) yields canonical files with cookies `2` versus `5`—differing at header byte `43`.

### 3.3 WAL recovery, sync flags, and checkpoint read marks

6. **`walIndexRecover`** — [`src/wal.c:1390-1614`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L1390-L1614) (linked). On the first open after a crash (when `-shm` is absent or invalid), SQLite takes exclusive checkpoint/recovery locks, reads the 32-byte `-wal` header, ignores the whole WAL if magic, page size, or header checksum fails (lines `1447–1467`), and refuses any format version other than `3007000` (line `1472`). It scans frames sequentially with `walDecodeFrame`, stopping at the first frame whose salts, page number, or cumulative checksum fails, and updates `mxFrame` and `nPage` **only at commit frames** (non-zero commit-size field, lines `1517–1519`). Any uncommitted or torn tail is discarded automatically.
7. **`sqlite3PagerSetFlags`, `WAL_SYNC_FLAGS` / `CKPT_SYNC_FLAGS`, `walFrames`, and `walWriteToLog`** — [`src/pager.c:3658-3704`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3658-L3704), [`src/wal.h:25-26`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.h#L25-L26), [`src/wal.c:4038-4266`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4038-L4266), and [`src/wal.c:3938-3960`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3938-L3960) (all linked). Two macros in `wal.h` unpack the pager's `walSyncFlags` bitfield:
   ```c
   #define WAL_SYNC_FLAGS(X)   ((X)&0x03)
   #define CKPT_SYNC_FLAGS(X)  (((X)>>2)&0x03)
   ```
   And `sqlite3PagerSetFlags` packs it (lines `3692–3695`):
   ```c
   pPager->walSyncFlags = (pPager->syncFlags<<2);
   if( pPager->fullSync ){
     pPager->walSyncFlags |= pPager->syncFlags;
   }
   ```
   This four-line assignment produces the entire WAL sync rule:
   - At `OFF`: `syncFlags = 0`, so both `WAL_SYNC_FLAGS` and `CKPT_SYNC_FLAGS` are `0` (no syncs anywhere).
   - At `NORMAL`: `syncFlags = SQLITE_SYNC_NORMAL` and `fullSync = 0`, so `CKPT_SYNC_FLAGS` is non-zero (syncs the 32-byte WAL header when frame 1 is written in `walFrames` lines `4121–4124`, and syncs during checkpoints) while `WAL_SYNC_FLAGS` is `0` (**no sync on commit**, lines `4196–4213`).
   - At `FULL` (and `EXTRA`): `fullSync = 1`, so both bitfields are non-zero, adding a `-wal` sync after every commit frame.
   - Inside `walFrames`, when writing frame 1 of a WAL whose per-connection checkpoint counter `pWal->nCkpt` is `0`, SQLite randomizes **both** `salt-1` and `salt-2` (lines `4095–4096`) rather than incrementing `salt-1`.
8. **`walCheckpoint`, `walRestartHdr`, `walTryBeginRead`, and `walRestartLog`** — [`src/wal.c:2199-2395`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395), [`2152-2166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2152-L2166), [`3020-3272`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3020-L3272), and [`3875-3915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3875-L3915) (all linked).
   - `walCheckpoint` computes `mxSafeFrame` as the lowest active read mark among slots `1–4` (lines `2233–2251`), acquires an **exclusive lock on read-lock slot 0** before backfilling (lines `2259–2260`), **syncs the entire `-wal` file first** (`sqlite3OsSync(pWal->pWalFd, CKPT_SYNC_FLAGS(sync_flags))`, line `2277`), writes each safe page into `.db`, and truncates and **syncs `.db` only when `mxSafeFrame == mxFrame`** (every frame in the WAL was copied, lines `2328–2335`). If a reader stopped the checkpoint early (`mxSafeFrame < mxFrame`), `.db` is written without being synced—safe at `NORMAL` and `FULL` because the backing WAL frames were already synced at line `2277`, so recovery reads those pages from `-wal`.
   - `walTryBeginRead` assigns a reader to **slot 0** (read `.db` only) when `nBackfill == mxFrame`, or to a **slot `1–4`** pinned to its `mxFrame` otherwise.
   - Before writing frame 1 of a commit, `walRestartLog` checks whether `nBackfill > 0` and tries exclusive locks on slots `1–4` (not slot 0). If it gets them, it calls `walRestartHdr` (`nCkpt++`, `mxFrame = 0`, `salt-1++`, new random `salt-2`, `nBackfill = 0`) so new frames overwrite the WAL from frame 1. Because a reader holding slot 0 does **not** block `walRestartLog` (it blocks backfilling at `wal.c:2259-2260`, not restart when `nBackfill == mxFrame`), `internal/walship` lets the WAL restart safely after every shipper-owned full backfill while still guaranteeing no unshipped frame can ever be backfilled or overwritten.

### 3.4 Integrity check and quick check

9. **`sqlite3BtreeIntegrityCheck` and `PRAGMA integrity_check` / `quick_check`** — [`src/btree.c:11144-11295`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L11144-L11295), [`src/pragma.c:1850-1857`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pragma.c#L1850-L1857), and [`src/pragma.c:2084-2090`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pragma.c#L2084-L2090) (all linked). `sqlite3BtreeIntegrityCheck` verifies the freelist chain (`checkList`, lines `11206–11210`), walks every B-tree root (`checkTreePage`: headers, cell boundaries, fragmentation accounting, key order, overflow chains), and flags unreferenced pages (`Page N: never used`). It never inspects the payload bytes of a freelist leaf page or validates non-indexed row values. The SQL-level codegen in `pragma.c` adds table-to-index cross-checks (`!isQuick` at line `2058`, skipped by `PRAGMA quick_check`). Register 7 counts scanned rows (`OP_AddImm 7, 1`, line `1857`), and `integrityCheckResultRow` (lines `2084–2090`) prints that scan counter—not the table's `rowid`—as `N` in `"row N missing from index X"`.

### 3.5 Experiment 9.1 — `capture-comparison`

- **Claim:** On a quiet WAL database, closed-file copy, `sqlite_dbpage` in one read transaction, and file+WAL replay all reach **E1 bytes**; the online backup API reaches **E2 pages** (differing at header bytes `27`, `43`, `95` inside `24–27`, `40–43`, `92–99`) and does **not** reach E1 after `VACUUM INTO` because `backup.c:448` resets the destination schema cookie (`1` vs source `4`, leaving byte `43` different after `VACUUM INTO`); and `VACUUM INTO` reaches **E3 except `journal_mode`** (producing a rollback file) and **E1 between canonical forms**. Under an active writer committing recipe steps: a plain file copy of `.db` without `-wal` yields a valid but **stale** older state; `sqlite_dbpage`, file+WAL under a held read transaction, `VACUUM INTO` (running while the writer holds an uncommitted write transaction), and the backup API all return exact recipe states at their expected levels without blocking the writer. In a stepped backup, same-connection **commits** advance the copied count (`pagecount - remaining`) steadily (`5, 10, 15, 20, 25`) even as `remaining` rises when the database grows; a commit by another connection restarts the backup from page `1` (`copied` drops back to `5`), and a commit by another connection after every step prevents the backup from finishing. (A same-connection write followed by `ROLLBACK` does restart the backup via `src/pager.c:3185`, shown in `CAP-03a`.)
- **Evidence form:** `EXACT` (every stored table must match on every run).

<!-- include: experiments/9.1-capture-comparison/main.go -->
```go
// Experiment 9.1 compares five SQLite capture methods—closed-file copy, online
// backup API, VACUUM INTO, reading every page through sqlite_dbpage in one
// read transaction, and copying the database file plus -wal under a held read
// transaction—first on a quiet database (Part A) and then while a writer child
// commits recipe steps concurrently (Part B). It also steps the backup API
// 5 pages at a time to distinguish same-connection commits (which update in
// place via sqlite3BackupUpdate, src/backup.c:688-715) from other-connection
// commits (which restart from page 1 via sqlite3BackupRestart,
// src/backup.c:728-734).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 9.1
package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
	"github.com/nickstrad/walfleet/internal/walship"
)

const (
	quietStep           = 6
	lastRefStep         = 22
	pageSize            = 4096
	backupPages         = 5
	giveUpAfter         = 8
	schemaCookieLowByte = 43
	methodCopy          = "closed-file copy"
	methodBackup        = "backup API"
	methodVacuum        = "VACUUM INTO"
	methodPages         = "pages in a read transaction"
	methodFile          = "file + WAL under a held transaction"
)

var errGaveUp = errors.New("gave up")

type levels struct{ e1, e2, e3, e3ExceptJournalMode, canonical bool }

var levelColumns = []string{"e1", "e2", "e3", "e3_except_journal_mode", "e1_after_vacuum_into"}

func (l levels) values() []any { return []any{l.e1, l.e2, l.e3, l.e3ExceptJournalMode, l.canonical} }

type run struct {
	ctx      context.Context
	lab      *procs.Lab
	r        recipe.Recipe
	dir      string
	live     string
	writer   *procs.Child
	source   *sqlitex.DB
	conn     *sql.Conn
	next     int
	set      *results.Set
	captures int
	during   string
	tables   struct {
		expected, quiet, header, details, steps, captures, backupSteps, summary *results.Table
	}
}

func (x *run) openTables() {
	x.tables.expected = x.set.Table("expected_levels", append([]string{"method"}, levelColumns...)...)
	x.tables.quiet = x.set.Table("quiet_captures", append([]string{"seq", "method"}, levelColumns...)...)
	x.tables.header = x.set.Table("backup_header", "seq", "comparison", "equal_ignoring_header_bytes", "ignored_header_bytes", "differing_header_bytes")
	x.tables.details = x.set.Table("quiet_details", "seq", "name", "value")
	x.tables.steps = x.set.Table("steps", "step", "during", "child", "action", "outcome")
	x.tables.captures = x.set.Table("writer_captures", append([]string{"seq", "method", "latest_commit", "equals_step", "want_step"}, levelColumns...)...)
	x.tables.backupSteps = x.set.Table("backup_steps", "run", "step", "remaining", "page_count", "copied", "then_committed_by", "committed_step")
	x.tables.summary = x.set.Table("summary", "seq", "method", "strongest_level", "safe_under_writer", "blocks_the_writer")
}

// must runs one action on the writer child and records it in the steps table.
func (x *run) must(action string, args ...string) {
	result := x.lab.Must(x.writer, action, args...)
	x.tables.steps.Row(result.Step, x.during, x.writer.Name, procs.ActionText(action, args), result.Outcome())
}

func (x *run) ref(k int) string { return filepath.Join(x.dir, "ref", fmt.Sprintf("step-%02d.db", k)) }

func (x *run) canonicalRef(k int) string {
	path := filepath.Join(x.dir, "ref", fmt.Sprintf("canonical-%02d.db", k))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	db, err := x.r.Open(x.ctx, x.ref(k))
	if err != nil {
		x.lab.Fatal("open reference", err)
	}
	_, err = db.ExecContext(x.ctx, "VACUUM INTO ?", path)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("canonical reference", err)
	}
	return path
}

func (x *run) buildReferences() {
	build := filepath.Join(x.dir, "ref", "build.db")
	if err := os.MkdirAll(filepath.Dir(build), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	err := x.r.BuildEach(x.ctx, build, lastRefStep, func(k int) error { return copyFile(build, x.ref(k)) })
	if err != nil {
		x.lab.Fatal("references", err)
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func (x *run) compare(capture string, k int, vacuumed bool) levels {
	e1, err := equal.CompareFiles(capture, x.ref(k), pageSize)
	if err != nil {
		x.lab.Fatal("E1", err)
	}
	e2, err := equal.ComparePageFiles(capture, x.ref(k), pageSize)
	if err != nil {
		x.lab.Fatal("E2", err)
	}
	e3, _, _, err := equal.CompareLogicalFiles(x.ctx, capture, x.ref(k))
	if err != nil {
		x.lab.Fatal("E3", err)
	}
	result := levels{e1: e1.Equal, e2: e2.Equal, e3: e3.Equal, e3ExceptJournalMode: e3.Equal || onlyJournalMode(e3.Differences)}
	if !result.e3ExceptJournalMode {
		return result
	}
	canonicalCapture := capture
	if !vacuumed {
		canonicalCapture = x.canonicalCapture(capture)
	}
	canonical, err := equal.CompareFiles(canonicalCapture, x.canonicalRef(k), pageSize)
	if err != nil {
		x.lab.Fatal("canonical", err)
	}
	result.canonical = canonical.Equal
	return result
}

// canonicalCapture runs VACUUM INTO on a scratch copy of capture so the
// original capture file is never altered by opening it, allowing every method's
// canonical-form byte equality to be measured directly.
func (x *run) canonicalCapture(capture string) string {
	path := capture + ".canonical"
	if _, err := os.Stat(path); err == nil {
		return path
	}
	scratch := filepath.Join(x.dir, "canonical-scratch", filepath.Base(capture))
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := copyFile(capture, scratch); err != nil {
		x.lab.Fatal("canonical scratch copy", err)
	}
	db, err := x.r.Open(x.ctx, scratch)
	if err != nil {
		x.lab.Fatal("open capture copy", err)
	}
	_, err = db.ExecContext(x.ctx, "VACUUM INTO ?", path)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("canonical capture", err)
	}
	return path
}

func onlyJournalMode(differences []string) bool {
	for _, d := range differences {
		if !strings.Contains(d, "journal_mode") {
			return false
		}
	}
	return len(differences) > 0
}

func (x *run) stateOf(capture string, vacuumed bool) (int, levels) {
	for k := 0; k <= lastRefStep; k++ {
		if l := x.compare(capture, k, vacuumed); l.e3ExceptJournalMode {
			return k, l
		}
	}
	return -1, levels{}
}

func (x *run) commit() int {
	x.must("recipe-session", strconv.Itoa(x.next))
	x.next++
	return x.next - 1
}

func (x *run) begin() int {
	if _, err := x.conn.ExecContext(x.ctx, "BEGIN"); err != nil {
		x.lab.Fatal("begin", err)
	}
	step, err := recipe.CurrentStep(x.ctx, x.conn)
	if err != nil {
		x.lab.Fatal("current step", err)
	}
	return step
}

func (x *run) end() {
	if _, err := x.conn.ExecContext(x.ctx, "COMMIT"); err != nil {
		x.lab.Fatal("commit", err)
	}
}

func (x *run) readPages(first, last int) []byte {
	rows, err := x.conn.QueryContext(x.ctx, "SELECT data FROM sqlite_dbpage WHERE pgno BETWEEN ? AND ? ORDER BY pgno", first, last)
	if err != nil {
		x.lab.Fatal("sqlite_dbpage", err)
	}
	defer rows.Close()
	var image []byte
	for rows.Next() {
		var page []byte
		if err := rows.Scan(&page); err != nil {
			x.lab.Fatal("scan", err)
		}
		image = append(image, page...)
	}
	if err := rows.Err(); err != nil || len(image) != (last-first+1)*pageSize {
		x.lab.Fatal("sqlite_dbpage", fmt.Errorf("%d bytes for pages %d-%d: %v", len(image), first, last, err))
	}
	return image
}

func (x *run) pageCount() int {
	var pages int
	if err := x.conn.QueryRowContext(x.ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		x.lab.Fatal("page_count", err)
	}
	return pages
}

// copyLive delegates raw OS file copies to a short-lived child process so the
// parent never opens and closes live.db in a process holding SQLite locks.
func (x *run) copyLive(suffix, name string) {
	if _, err := walfile.CopyLiveFiles(x.ctx, []walfile.CopyPair{{From: x.live + suffix, To: filepath.Join(x.dir, name)}}); err != nil {
		x.lab.Fatal("copy", err)
	}
}

func (x *run) path(name string) string { return filepath.Join(x.dir, name) }

func (x *run) quiet() {
	if err := copyFile(x.ref(quietStep), x.live); err != nil {
		x.lab.Fatal("live copy", err)
	}
	if err := copyFile(x.live, x.path("a-copy.db")); err != nil {
		x.lab.Fatal("closed copy", err)
	}
	x.openSource()
	if _, err := sqlitex.Backup(x.ctx, x.conn, x.path("a-backup.db"), -1, nil); err != nil {
		x.lab.Fatal("backup", err)
	}
	if _, err := x.conn.ExecContext(x.ctx, "VACUUM INTO ?", x.path("a-vacuum.db")); err != nil {
		x.lab.Fatal("vacuum into", err)
	}
	x.begin()
	image := x.readPages(1, x.pageCount())
	x.copyLive("", "a-file.db")
	x.copyLive("-wal", "a-file.wal")
	x.end()
	if err := walship.WriteDatabase(x.path("a-pages.db"), image); err != nil {
		x.lab.Fatal("write pages", err)
	}
	if _, err := walfile.ReplayFiles(x.path("a-file.db"), x.path("a-file.wal"), x.path("a-replayed.db")); err != nil {
		x.lab.Fatal("replay", err)
	}
	x.closeSource()
	want := map[string]levels{
		methodCopy:   {true, true, true, true, true},
		methodBackup: backupLevels,
		methodVacuum: {false, false, false, true, true},
		methodPages:  {true, true, true, true, true},
		methodFile:   {true, true, true, true, true},
	}
	for i, c := range []struct{ method, file string }{
		{methodCopy, "a-copy.db"}, {methodBackup, "a-backup.db"}, {methodVacuum, "a-vacuum.db"}, {methodPages, "a-pages.db"}, {methodFile, "a-replayed.db"},
	} {
		got := x.compare(x.path(c.file), quietStep, c.method == methodVacuum)
		x.tables.expected.Row(append([]any{c.method}, want[c.method].values()...)...)
		x.tables.quiet.Row(append([]any{i + 1, c.method}, got.values()...)...)
		x.set.Check("A", got == want[c.method], fmt.Sprintf("%s reaches exactly the expected levels on a quiet database", c.method))
	}
	backup, err := equal.ComparePageFiles(x.path("a-backup.db"), x.ref(quietStep), pageSize)
	if err != nil {
		x.lab.Fatal("E2", err)
	}
	x.headerRow(1, "backup copy vs the source", backup)
	backupCanonical, err := equal.ComparePageFiles(x.canonicalCapture(x.path("a-backup.db")), x.canonicalRef(quietStep), pageSize)
	if err != nil {
		x.lab.Fatal("E2 canonical", err)
	}
	x.headerRow(2, "both after VACUUM INTO", backupCanonical)
	// Read the backup's schema cookie (header bytes 40-43) to show why its
	// canonical form still differs from the source's at byte 43:
	// sqlite3_backup_step sets the destination's cookie to old+1 (src/backup.c:448),
	// and VACUUM INTO carries that offset forward by adding 1 again (src/vacuum.c:359).
	backupCookie, sourceCookie := schemaCookie(x, x.path("a-backup.db")), schemaCookie(x, x.ref(quietStep))
	x.tables.details.Row(1, "schema cookie of the backup", backupCookie)
	x.tables.details.Row(2, "schema cookie of the source", sourceCookie)
	x.set.Check("A", backupCanonical.Equal && slices.Equal(backupCanonical.IgnoredDiffering, []int{schemaCookieLowByte}) && backupCookie != sourceCookie,
		fmt.Sprintf("the backup's VACUUM INTO form differs from the source's only at byte %d, the schema cookie", schemaCookieLowByte))
	_, logical, vacuumed, err := equal.CompareLogicalFiles(x.ctx, x.path("a-vacuum.db"), x.ref(quietStep))
	if err != nil {
		x.lab.Fatal("E3", err)
	}
	x.tables.details.Row(3, "journal_mode of the VACUUM INTO copy", pragma(logical, "journal_mode"))
	x.tables.details.Row(4, "journal_mode of the source", pragma(vacuumed, "journal_mode"))
}

func (x *run) headerRow(seq int, comparison string, r equal.Result) {
	ignored := make([]string, len(r.Ignored))
	for i, b := range r.Ignored {
		ignored[i] = b.String()
	}
	differing := make([]string, len(r.IgnoredDiffering))
	for i, offset := range r.IgnoredDiffering {
		differing[i] = strconv.Itoa(offset)
	}
	x.tables.header.Row(seq, comparison, r.Equal, strings.Join(ignored, " "), strings.Join(differing, " "))
}

func schemaCookie(x *run, path string) uint32 {
	file, err := os.Open(path)
	if err != nil {
		x.lab.Fatal("open", err)
	}
	defer file.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		x.lab.Fatal("read header", err)
	}
	return binary.BigEndian.Uint32(header[40:44])
}

func pragma(l equal.Logical, name string) string {
	for _, p := range l.Pragmas {
		if p.Name == name {
			return p.Value
		}
	}
	return "missing"
}

func (x *run) openSource() {
	options := x.r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(x.ctx, x.live, options)
	if err != nil {
		x.lab.Fatal("open", err)
	}
	conn, err := db.Conn(x.ctx)
	if err != nil {
		x.lab.Fatal("conn", err)
	}
	x.source, x.conn = db, conn
}

func (x *run) closeSource() {
	if err := errors.Join(x.conn.Close(), x.source.Close()); err != nil {
		x.lab.Fatal("close", err)
	}
}

var (
	everyLevel   = levels{true, true, true, true, true}
	backupLevels = levels{false, true, true, true, false}
	vacuumLevels = levels{false, false, false, true, true}
)

func (x *run) report(method, file string, vacuumed bool, want int, wantLevels levels) bool {
	k, l := x.stateOf(x.path(file), vacuumed)
	x.captureRow(method, k, want, l)
	ok := k == want && l == wantLevels
	x.set.Check("B", ok, fmt.Sprintf("under the writer, %s equals recipe step %d at exactly the expected levels", method, want))
	return ok
}

func (x *run) captureRow(method string, k int, want any, l levels) {
	x.captures++
	x.tables.captures.Row(append([]any{x.captures, method, x.next - 1, k, want}, l.values()...)...)
}

func (x *run) underWriter() map[string]bool {
	x.writer = x.lab.SpawnSQL("writer", procs.SQLOptions{Path: x.live, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second})
	x.openSource()
	x.next = quietStep + 1
	safe := map[string]bool{}

	x.during = "plain copy of the live file"
	current := x.commit()
	x.copyLive("", "b-copy.db")
	stale, staleLevels := x.stateOf(x.path("b-copy.db"), false)
	x.captureRow(x.during, stale, nil, staleLevels)
	x.set.Check("B", stale >= 0 && stale < current && staleLevels == everyLevel,
		fmt.Sprintf("the plain copy is byte-identical (E1) to an older recipe step (%d), not to the committed step %d", stale, current))
	safe[methodCopy] = stale == current

	x.during = methodPages
	seen := x.begin()
	pages := x.pageCount()
	image := x.readPages(1, pages/2)
	x.commit()
	image = append(image, x.readPages(pages/2+1, pages)...)
	x.end()
	if err := walship.WriteDatabase(x.path("b-pages.db"), image); err != nil {
		x.lab.Fatal("write", err)
	}
	safe[methodPages] = x.report(methodPages, "b-pages.db", false, seen, everyLevel)

	x.during = methodFile
	seen = x.begin()
	x.copyLive("", "b-file.db")
	afterCopy := x.commit()
	x.copyLive("-wal", "b-file.wal")
	x.end()
	if _, err := walfile.ReplayFiles(x.path("b-file.db"), x.path("b-file.wal"), x.path("b-replayed.db")); err != nil {
		x.lab.Fatal("replay", err)
	}
	safe[methodFile] = x.report(methodFile, "b-replayed.db", false, afterCopy, everyLevel) && afterCopy >= seen

	// Run VACUUM INTO while the writer child holds BEGIN IMMEDIATE with an
	// uncommitted step: sqlite3RunVacuum opens only a read transaction on the
	// source when pOut != NULL (src/vacuum.c:270), so it neither blocks nor
	// sees the uncommitted writes.
	x.during = methodVacuum
	before := x.next - 1
	x.must("begin", "immediate")
	x.must("recipe-exec", strconv.Itoa(x.next))
	if _, err := x.conn.ExecContext(x.ctx, "VACUUM INTO ?", x.path("b-vacuum.db")); err != nil {
		x.lab.Fatal("vacuum into", err)
	}
	x.must("commit")
	x.next++
	safe[methodVacuum] = x.report(methodVacuum, "b-vacuum.db", true, before, vacuumLevels)

	safe[methodBackup] = x.backups()
	x.closeSource()
	x.lab.StopChildren()
	return safe
}

// backups runs two stepped backups (5 pages per step): one interleaved with
// same-connection commits (via SourceExecer) and a single writer-child commit
// to contrast sqlite3BackupUpdate with sqlite3BackupRestart, and a second
// where the writer child commits after every step to show starvation.
func (x *run) backups() bool {
	x.during = methodBackup
	var copied []int
	steps, err := sqlitex.BackupWithSourceExec(x.ctx, x.conn, x.path("b-backup.db"), backupPages, func(step sqlitex.BackupStep, exec sqlitex.SourceExecer) error {
		// Track PageCount-Remaining (= iNext-1) rather than Remaining alone:
		// sqlite3_backup_step recomputes Remaining from the current source size
		// (src/backup.c:428-429), so Remaining rises whenever a commit grows the database.
		copied = append(copied, step.PageCount-step.Remaining)
		var by, committed any
		switch step.Step {
		case 2, 3:
			planned, err := x.r.Step(x.next)
			if err != nil {
				return err
			}
			if err := applyThrough(x.ctx, exec, planned); err != nil {
				return err
			}
			by, committed = "the backup's own connection", x.next
			x.next++
		case 5:
			by, committed = "the writer child", x.commit()
		}
		x.tables.backupSteps.Row(1, step.Step, step.Remaining, step.PageCount, step.PageCount-step.Remaining, by, committed)
		return nil
	})
	if err != nil {
		x.lab.Fatal("backup", err)
	}
	finished := steps[len(steps)-1].Remaining == 0
	x.set.Check("B", copied[0] == backupPages && copied[1] == 2*backupPages && copied[2] == 3*backupPages && copied[3] == 4*backupPages && copied[4] == 5*backupPages,
		fmt.Sprintf("through the backup's own commits the copy keeps advancing %d pages per step (no restart), although the remaining count rose when those commits grew the database", backupPages))
	x.set.Check("B", copied[5] == backupPages, "after the writer child's commit the next step starts again from page 1: the backup restarted")
	var remaining []int
	ok := x.report(methodBackup, "b-backup.db", false, x.next-1, backupLevels) && finished

	x.during = methodBackup + ", a commit after every step"
	_, err = sqlitex.Backup(x.ctx, x.conn, x.path("b-backup-never.db"), backupPages, func(step sqlitex.BackupStep) error {
		remaining = append(remaining, step.Remaining)
		x.tables.backupSteps.Row(2, step.Step, step.Remaining, step.PageCount, step.PageCount-step.Remaining, "the writer child", x.next)
		x.commit()
		if step.Step == giveUpAfter {
			return errGaveUp
		}
		return nil
	})
	x.set.Check("B", errors.Is(err, errGaveUp) && minimum(remaining) > 0, fmt.Sprintf("with a commit by another connection after every step the backup never finished: after %d steps the smallest remaining count was %d", giveUpAfter, minimum(remaining)))
	return ok
}

func applyThrough(ctx context.Context, exec sqlitex.SourceExecer, step recipe.Step) error {
	if _, err := exec.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := step.ExecIn(ctx, exec); err != nil {
		_, rollbackErr := exec.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	_, err := exec.ExecContext(ctx, "COMMIT")
	return err
}

func minimum(values []int) int {
	m := values[0]
	for _, v := range values {
		m = min(m, v)
	}
	return m
}

func main() {
	if procs.Serve(procs.Roles) || walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.1")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil // the steps table records each writer step
	x := &run{ctx: ctx, lab: lab, r: recipe.Default(), dir: dir, live: filepath.Join(dir, "live.db"), set: results.MustOpen(dir)}
	x.openTables()
	x.buildReferences()
	x.quiet()
	safe := x.underWriter()

	rows := []struct{ method, level, blocks string }{
		{methodCopy, "E1", "the database must be closed"},
		{methodBackup, "E2", "no (commits ran between steps)"},
		{methodVacuum, "E3 but journal_mode; E1 canonical", "no (ran while the writer held its write transaction)"},
		{methodPages, "E1", "no (a commit ran inside the read transaction)"},
		{methodFile, "E1 after WAL replay", "no (a commit ran inside the read transaction)"},
	}
	for i, row := range rows {
		x.tables.summary.Row(i+1, row.method, row.level, safe[row.method], row.blocks)
	}
	x.set.Check("C", !safe[methodCopy] && safe[methodBackup] && safe[methodVacuum] && safe[methodPages] && safe[methodFile],
		"under the writer every capture except the plain file copy equals the exact recipe state it should; the plain copy equals an older one")
	x.set.Finish(strings.ToUpper("capture comparison"))
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/9.1-capture-comparison/report.sql -->
```sql
-- Walkthrough of experiment 9.1. Run it with:
--   go run ./cmd/course run 9.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   expected_levels   one row per capture method: the equality levels it should reach
--   quiet_captures    part A: one row per capture of the quiet database, the levels reached
--   backup_header     part A: where the backup API copy's header differs from the source
--   quiet_details     part A: schema cookies and journal modes behind those differences
--   steps             part B: one row per step the writer child ran
--   writer_captures   part B: one row per capture taken while the writer commits
--   backup_steps      part B: one row per sqlite3_backup_step call (run 1 and run 2)
--   summary           one row per method: strongest level, safe under a writer, blocking
--   checks            every claim the experiment asserted (the runner lists it last)
-- Levels: e1 = identical file bytes; e2 = identical pages, ignoring the header
-- bytes SQLite rewrites on every copy (24-27, 40-43, 92-99); e3 = identical
-- logical content (schema, rows, pragmas); e3_except_journal_mode = the same
-- with journal_mode left out; e1_after_vacuum_into = identical bytes once both
-- files are rewritten with VACUUM INTO.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.1  Five ways to capture a live SQLite database'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Five capture methods: a copy of the closed file, the online backup API,'
.print 'VACUUM INTO, reading every page through sqlite_dbpage in one read'
.print 'transaction, and copying the database file and its -wal while a read'
.print 'transaction is held (rebuilt by WAL replay). Each capture is compared with'
.print 'reference copies of the recipe database built to every step (4096-byte pages).'

.print
.print '── A. A quiet database (recipe step 6, no writer) ─────────────────────'
.print
.print 'A1. Each capture against the source. Three methods give the same bytes'
.print '    (E1); the backup API gives the same pages but not the same header;'
.print '    VACUUM INTO rewrites the file, so it matches only when journal_mode is'
.print '    ignored, and byte for byte once the source is vacuumed too.'

SELECT q.method, q.e1, q.e2, q.e3, q.e3_except_journal_mode AS e3_but_jm,
       q.e1_after_vacuum_into AS e1_vacuumed,
       (q.e1, q.e2, q.e3, q.e3_except_journal_mode, q.e1_after_vacuum_into)
         = (e.e1, e.e2, e.e3, e.e3_except_journal_mode, e.e1_after_vacuum_into) AS as_expected
FROM quiet_captures q JOIN expected_levels e USING (method)
ORDER BY q.seq;

.print
.print 'A2. Where the backup copy differs: only in header bytes that E2 ignores.'
.print '    After VACUUM INTO on both sides one byte still differs: byte 43, the'
.print '    low byte of the schema cookie.'

SELECT comparison, equal_ignoring_header_bytes AS e2_equal, ignored_header_bytes, differing_header_bytes
FROM backup_header ORDER BY seq;

.print
.print 'A3. Why. sqlite3_backup_step sets the schema cookie of the copy to the'
.print '    old value plus one (src/backup.c:448), and VACUUM INTO adds one to the'
.print '    cookie it copies (src/vacuum.c:359), so the offset survives. VACUUM INTO'
.print '    also leaves its copy in rollback mode, which is the one E3 difference.'

SELECT name, value FROM quiet_details ORDER BY seq;

.print
.print '── B. The same database while a writer child commits ───────────────────'
.print
.print 'A child process commits recipe steps, one fresh connection per step'
.print '(wal_autocheckpoint=0). The experiment captures between and during its'
.print 'commits:'
.print '  plain copy       a copy of the live database file only (no -wal)'
.print '  pages            every page through sqlite_dbpage in one read transaction,'
.print '                   in two halves; the writer commits between them'
.print '  file + WAL       copy the database file, the writer commits, copy the -wal,'
.print '                   all inside one read transaction; rebuilt by WAL replay'
.print '  VACUUM INTO      run while the writer holds BEGIN IMMEDIATE with a step'
.print '                   not yet committed; the writer commits afterwards'
.print '  backup API       5 pages per step, commits in between (see B3)'
.print
.print 'B1. What the writer did, and during which capture:'

SELECT step, during, action, outcome FROM steps WHERE during NOT LIKE '%every step' ORDER BY step;

.print
.print 'B2. The recipe step each capture equals, beside the newest step the writer'
.print '    had committed when the comparison ran. The plain copy misses commits'
.print '    still in the -wal and equals an older step. Every other method equals'
.print '    the exact step its read transaction or last step saw, at the expected'
.print '    levels (the same as in A1). VACUUM INTO finished while the writer held'
.print '    its write transaction: it reads under a read transaction only'
.print '    (src/vacuum.c:270).'

SELECT w.method, w.latest_commit AS latest, w.equals_step AS equals, w.want_step AS want,
       concat_ws(' ', CASE WHEN w.e1 THEN 'E1' END, CASE WHEN w.e2 THEN 'E2' END,
                 CASE WHEN w.e3 THEN 'E3' END, CASE WHEN w.e3_except_journal_mode THEN 'E3-but-jm' END,
                 CASE WHEN w.e1_after_vacuum_into THEN 'E1-vacuumed' END) AS levels_reached,
       (w.e1, w.e2, w.e3, w.e3_except_journal_mode, w.e1_after_vacuum_into)
         = (e.e1, e.e2, e.e3, e.e3_except_journal_mode, e.e1_after_vacuum_into) AS as_expected
FROM writer_captures w
JOIN expected_levels e ON e.method = CASE WHEN w.want_step IS NULL THEN 'closed-file copy' ELSE w.method END
ORDER BY w.seq;

.print
.print 'B3. The backup API, 5 pages per step (run 1). After steps 2 and 3 the'
.print '    connection being backed up commits: the copy keeps advancing 5 pages a'
.print '    step (sqlite3BackupUpdate, src/backup.c:688-715) although remaining'
.print '    rises as the database grows. After step 5 the writer child commits:'
.print '    the next step starts again from page 1 (sqlite3BackupRestart,'
.print '    src/backup.c:728-734). copied = page_count - remaining.'

SELECT step, remaining, page_count, copied, then_committed_by, committed_step
FROM backup_steps WHERE run = 1 ORDER BY step;

.print
.print 'B4. Run 2: the writer child commits after every step. Every commit'
.print '    restarts the backup, and the database keeps growing, so remaining'
.print '    never reaches 0. The experiment gives up after 8 steps.'

SELECT step, remaining, page_count, then_committed_by, committed_step,
       min(remaining) OVER () AS smallest_remaining
FROM backup_steps WHERE run = 2 ORDER BY step;

.print
.print '── C. Summary ──────────────────────────────────────────────────────────'
.print

SELECT method, strongest_level, safe_under_writer FROM summary ORDER BY seq;

.print
.print 'Whether a method blocks the writer, from what happened in part B:'

SELECT method, blocks_the_writer FROM summary ORDER BY seq;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 9.1
```

Add `-i` (`go run ./cmd/course run -i 9.1`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/9.1-capture-comparison/`](evidence/9.1-capture-comparison/), and `go run ./cmd/course check evidence ch09` compares a fresh run with them byte for byte.

<!-- include: evidence/9.1-capture-comparison.txt -->
```text

════════════════════════════════════════════════════════════════════════
  9.1  Five ways to capture a live SQLite database
════════════════════════════════════════════════════════════════════════

Five capture methods: a copy of the closed file, the online backup API,
VACUUM INTO, reading every page through sqlite_dbpage in one read
transaction, and copying the database file and its -wal while a read
transaction is held (rebuilt by WAL replay). Each capture is compared with
reference copies of the recipe database built to every step (4096-byte pages).

── A. A quiet database (recipe step 6, no writer) ─────────────────────

A1. Each capture against the source. Three methods give the same bytes
    (E1); the backup API gives the same pages but not the same header;
    VACUUM INTO rewrites the file, so it matches only when journal_mode is
    ignored, and byte for byte once the source is vacuumed too.
┌─────────────────────────────────────┬───────┬───────┬───────┬───────────┬─────────────┬─────────────┐
│               method                │  e1   │  e2   │  e3   │ e3_but_jm │ e1_vacuumed │ as_expected │
├─────────────────────────────────────┼───────┼───────┼───────┼───────────┼─────────────┼─────────────┤
│ closed-file copy                    │ true  │ true  │ true  │ true      │ true        │ true        │
│ backup API                          │ false │ true  │ true  │ true      │ false       │ true        │
│ VACUUM INTO                         │ false │ false │ false │ true      │ true        │ true        │
│ pages in a read transaction         │ true  │ true  │ true  │ true      │ true        │ true        │
│ file + WAL under a held transaction │ true  │ true  │ true  │ true      │ true        │ true        │
└─────────────────────────────────────┴───────┴───────┴───────┴───────────┴─────────────┴─────────────┘

A2. Where the backup copy differs: only in header bytes that E2 ignores.
    After VACUUM INTO on both sides one byte still differs: byte 43, the
    low byte of the schema cookie.
┌───────────────────────────┬──────────┬──────────────────────┬────────────────────────┐
│        comparison         │ e2_equal │ ignored_header_bytes │ differing_header_bytes │
├───────────────────────────┼──────────┼──────────────────────┼────────────────────────┤
│ backup copy vs the source │ true     │ 24-27 40-43 92-99    │ 27 43 95               │
│ both after VACUUM INTO    │ true     │ 24-27 40-43 92-99    │ 43                     │
└───────────────────────────┴──────────┴──────────────────────┴────────────────────────┘

A3. Why. sqlite3_backup_step sets the schema cookie of the copy to the
    old value plus one (src/backup.c:448), and VACUUM INTO adds one to the
    cookie it copies (src/vacuum.c:359), so the offset survives. VACUUM INTO
    also leaves its copy in rollback mode, which is the one E3 difference.
┌──────────────────────────────────────┬──────────┐
│                 name                 │  value   │
├──────────────────────────────────────┼──────────┤
│ schema cookie of the backup          │ 1        │
│ schema cookie of the source          │ 4        │
│ journal_mode of the VACUUM INTO copy │ rollback │
│ journal_mode of the source           │ wal      │
└──────────────────────────────────────┴──────────┘

── B. The same database while a writer child commits ───────────────────

A child process commits recipe steps, one fresh connection per step
(wal_autocheckpoint=0). The experiment captures between and during its
commits:
  plain copy       a copy of the live database file only (no -wal)
  pages            every page through sqlite_dbpage in one read transaction,
                   in two halves; the writer commits between them
  file + WAL       copy the database file, the writer commits, copy the -wal,
                   all inside one read transaction; rebuilt by WAL replay
  VACUUM INTO      run while the writer holds BEGIN IMMEDIATE with a step
                   not yet committed; the writer commits afterwards
  backup API       5 pages per step, commits in between (see B3)

B1. What the writer did, and during which capture:
┌──────┬─────────────────────────────────────┬───────────────────┬───────────┐
│ step │               during                │      action       │  outcome  │
├──────┼─────────────────────────────────────┼───────────────────┼───────────┤
│ 1    │ plain copy of the live file         │ recipe-session 7  │ SQLITE_OK │
│ 2    │ pages in a read transaction         │ recipe-session 8  │ SQLITE_OK │
│ 3    │ file + WAL under a held transaction │ recipe-session 9  │ SQLITE_OK │
│ 4    │ VACUUM INTO                         │ begin immediate   │ SQLITE_OK │
│ 5    │ VACUUM INTO                         │ recipe-exec 10    │ SQLITE_OK │
│ 6    │ VACUUM INTO                         │ commit            │ SQLITE_OK │
│ 7    │ backup API                          │ recipe-session 13 │ SQLITE_OK │
└──────┴─────────────────────────────────────┴───────────────────┴───────────┘

B2. The recipe step each capture equals, beside the newest step the writer
    had committed when the comparison ran. The plain copy misses commits
    still in the -wal and equals an older step. Every other method equals
    the exact step its read transaction or last step saw, at the expected
    levels (the same as in A1). VACUUM INTO finished while the writer held
    its write transaction: it reads under a read transaction only
    (src/vacuum.c:270).
┌─────────────────────────────────────┬────────┬────────┬──────┬────────────────────────────────┬─────────────┐
│               method                │ latest │ equals │ want │         levels_reached         │ as_expected │
├─────────────────────────────────────┼────────┼────────┼──────┼────────────────────────────────┼─────────────┤
│ plain copy of the live file         │ 7      │ 6      │      │ E1 E2 E3 E3-but-jm E1-vacuumed │ true        │
│ pages in a read transaction         │ 8      │ 7      │ 7    │ E1 E2 E3 E3-but-jm E1-vacuumed │ true        │
│ file + WAL under a held transaction │ 9      │ 9      │ 9    │ E1 E2 E3 E3-but-jm E1-vacuumed │ true        │
│ VACUUM INTO                         │ 10     │ 9      │ 9    │ E3-but-jm E1-vacuumed          │ true        │
│ backup API                          │ 13     │ 13     │ 13   │ E2 E3 E3-but-jm                │ true        │
└─────────────────────────────────────┴────────┴────────┴──────┴────────────────────────────────┴─────────────┘

B3. The backup API, 5 pages per step (run 1). After steps 2 and 3 the
    connection being backed up commits: the copy keeps advancing 5 pages a
    step (sqlite3BackupUpdate, src/backup.c:688-715) although remaining
    rises as the database grows. After step 5 the writer child commits:
    the next step starts again from page 1 (sqlite3BackupRestart,
    src/backup.c:728-734). copied = page_count - remaining.
┌──────┬───────────┬────────────┬────────┬─────────────────────────────┬────────────────┐
│ step │ remaining │ page_count │ copied │      then_committed_by      │ committed_step │
├──────┼───────────┼────────────┼────────┼─────────────────────────────┼────────────────┤
│ 1    │ 95        │ 100        │ 5      │                             │                │
│ 2    │ 90        │ 100        │ 10     │ the backup's own connection │ 11             │
│ 3    │ 93        │ 108        │ 15     │ the backup's own connection │ 12             │
│ 4    │ 98        │ 118        │ 20     │                             │                │
│ 5    │ 93        │ 118        │ 25     │ the writer child            │ 13             │
│ 6    │ 119       │ 124        │ 5      │                             │                │
│ 7    │ 114       │ 124        │ 10     │                             │                │
│ 8    │ 109       │ 124        │ 15     │                             │                │
│ 9    │ 104       │ 124        │ 20     │                             │                │
│ 10   │ 99        │ 124        │ 25     │                             │                │
│ 11   │ 94        │ 124        │ 30     │                             │                │
│ 12   │ 89        │ 124        │ 35     │                             │                │
│ 13   │ 84        │ 124        │ 40     │                             │                │
│ 14   │ 79        │ 124        │ 45     │                             │                │
│ 15   │ 74        │ 124        │ 50     │                             │                │
│ 16   │ 69        │ 124        │ 55     │                             │                │
│ 17   │ 64        │ 124        │ 60     │                             │                │
│ 18   │ 59        │ 124        │ 65     │                             │                │
│ 19   │ 54        │ 124        │ 70     │                             │                │
│ 20   │ 49        │ 124        │ 75     │                             │                │
│ 21   │ 44        │ 124        │ 80     │                             │                │
│ 22   │ 39        │ 124        │ 85     │                             │                │
│ 23   │ 34        │ 124        │ 90     │                             │                │
│ 24   │ 29        │ 124        │ 95     │                             │                │
│ 25   │ 24        │ 124        │ 100    │                             │                │
│ 26   │ 19        │ 124        │ 105    │                             │                │
│ 27   │ 14        │ 124        │ 110    │                             │                │
│ 28   │ 9         │ 124        │ 115    │                             │                │
│ 29   │ 4         │ 124        │ 120    │                             │                │
│ 30   │ 0         │ 124        │ 124    │                             │                │
└──────┴───────────┴────────────┴────────┴─────────────────────────────┴────────────────┘

B4. Run 2: the writer child commits after every step. Every commit
    restarts the backup, and the database keeps growing, so remaining
    never reaches 0. The experiment gives up after 8 steps.
┌──────┬───────────┬────────────┬───────────────────┬────────────────┬────────────────────┐
│ step │ remaining │ page_count │ then_committed_by │ committed_step │ smallest_remaining │
├──────┼───────────┼────────────┼───────────────────┼────────────────┼────────────────────┤
│ 1    │ 119       │ 124        │ the writer child  │ 14             │ 119                │
│ 2    │ 127       │ 132        │ the writer child  │ 15             │ 119                │
│ 3    │ 139       │ 144        │ the writer child  │ 16             │ 119                │
│ 4    │ 155       │ 160        │ the writer child  │ 17             │ 119                │
│ 5    │ 157       │ 162        │ the writer child  │ 18             │ 119                │
│ 6    │ 165       │ 170        │ the writer child  │ 19             │ 119                │
│ 7    │ 166       │ 171        │ the writer child  │ 20             │ 119                │
│ 8    │ 179       │ 184        │ the writer child  │ 21             │ 119                │
└──────┴───────────┴────────────┴───────────────────┴────────────────┴────────────────────┘

── C. Summary ──────────────────────────────────────────────────────────

┌─────────────────────────────────────┬───────────────────────────────────┬───────────────────┐
│               method                │          strongest_level          │ safe_under_writer │
├─────────────────────────────────────┼───────────────────────────────────┼───────────────────┤
│ closed-file copy                    │ E1                                │ false             │
│ backup API                          │ E2                                │ true              │
│ VACUUM INTO                         │ E3 but journal_mode; E1 canonical │ true              │
│ pages in a read transaction         │ E1                                │ true              │
│ file + WAL under a held transaction │ E1 after WAL replay               │ true              │
└─────────────────────────────────────┴───────────────────────────────────┴───────────────────┘

Whether a method blocks the writer, from what happened in part B:
┌─────────────────────────────────────┬──────────────────────────────────────────────────────┐
│               method                │                  blocks_the_writer                   │
├─────────────────────────────────────┼──────────────────────────────────────────────────────┤
│ closed-file copy                    │ the database must be closed                          │
│ backup API                          │ no (commits ran between steps)                       │
│ VACUUM INTO                         │ no (ran while the writer held its write transaction) │
│ pages in a read transaction         │ no (a commit ran inside the read transaction)        │
│ file + WAL under a held transaction │ no (a commit ran inside the read transaction)        │
└─────────────────────────────────────┴──────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  closed-file copy reaches exactly the expected levels on a quiet database
A  ✓  backup API reaches exactly the expected levels on a quiet database
A  ✓  VACUUM INTO reaches exactly the expected levels on a quiet database
A  ✓  pages in a read transaction reaches exactly the expected levels on a quiet database
A  ✓  file + WAL under a held transaction reaches exactly the expected levels on a quiet database
A  ✓  the backup's VACUUM INTO form differs from the source's only at byte 43, the schema cookie
B  ✓  the plain copy is byte-identical (E1) to an older recipe step (6), not to the committed step 7
B  ✓  under the writer, pages in a read transaction equals recipe step 7 at exactly the expected levels
B  ✓  under the writer, file + WAL under a held transaction equals recipe step 9 at exactly the expected levels
B  ✓  under the writer, VACUUM INTO equals recipe step 9 at exactly the expected levels
B  ✓  through the backup's own commits the copy keeps advancing 5 pages per step (no restart), although the remaining count rose when those commits grew the database
B  ✓  after the writer child's commit the next step starts again from page 1: the backup restarted
B  ✓  under the writer, backup API equals recipe step 13 at exactly the expected levels
B  ✓  with a commit by another connection after every step the backup never finished: after 8 steps the smallest remaining count was 119
C  ✓  under the writer every capture except the plain file copy equals the exact recipe state it should; the plain copy equals an older one

15 of 15 claims hold

Experiment verdict: CAPTURE COMPARISON OK
```

#### Reading the Experiment 9.1 evidence

- **A1–A3 (Part A — quiet database at recipe step 6):**
  - Closed-file copy, `sqlite_dbpage` pages in one read transaction, and file+WAL under a held transaction each show `true` across all five columns (`e1`, `e2`, `e3`, `e3_but_jm` for E3-except-journal_mode, `e1_vacuumed` for E1-after-VACUUM-INTO).
  - **Backup API (A1–A3):** E1 `false`, E2 `true`, E3 `true`, E3-except-journal_mode `true`, **E1-after-VACUUM-INTO `false`**. A2 shows the raw E2 difference is confined to bytes `[27 43 95]` (the low bytes of change counter `24–27`, schema cookie `40–43`, and version-valid-for `92–95`). A2 and A3 explain the outline correction discovered when `E1-after-VACUUM-INTO` was measured directly: after running `VACUUM INTO` on both the backup copy and the source, the two canonical files still differ at **byte 43** because `sqlite3_backup_step` set the backup's schema cookie to `1` (`0 + 1` at `src/backup.c:448`) while the source's cookie was `4`, and `VACUUM INTO` incremented both by `1` (`2` vs `5` at `src/vacuum.c:359`). A backup API capture reaches E2, never E1-after-`VACUUM INTO`.
  - **`VACUUM INTO` (A1, A3):** E1 `false`, E2 `false`, **E3 `false`**, E3-except-journal_mode `true`, E1-after-VACUUM-INTO `true`. This is a second outline correction: because `equal.LogicalPragmas` records `journal_mode`, and `VACUUM INTO` writes a `rollback` file from a `wal` source (A3), it reaches E3 except `journal_mode` and E1 between canonical forms.
- **B1–B2 (Part B — live captures under a concurrent writer child):**
  - **Plain copy of live `.db`:** after the writer commits step `7`, copying `live.db` alone yields a valid database equal at E1 to **recipe step 6**—silently missing step 7, which still lives in `-wal`.
  - **Pages in one read transaction:** the parent opens a read transaction at step `7`, reads pages `1..N/2`, the writer commits step `8`, and the parent reads pages `N/2+1..N`. The assembled file equals **recipe step 7 at E1**.
  - **File + WAL under a held read transaction:** inside a read transaction opened at step `8`, the copier copies `live.db`, the writer commits step `9`, and the copier copies `-wal`. Replaying `-wal` onto the copied `live.db` produces **recipe step 9 at E1**.
  - **`VACUUM INTO` under a held write transaction:** the writer executes `BEGIN IMMEDIATE` and `recipe-exec 10` (holding the write lock with step 10 uncommitted). While that write transaction is open, the parent's `VACUUM INTO` completes without blocking (`VACUUM INTO finished while the writer held its write transaction open`), and after the writer commits step `10`, the output equals **recipe step 9** (the last committed state) at `E3-except-journal_mode` and `E1-after-VACUUM-INTO`.
- **B3–B4 (Part B — stepped backup API, 5 pages per step):**
  - **Same-connection commits vs another connection's commit (B3):** This demonstrates another outline correction. When the backup's own connection commits step `11` after step 2 and step `12` after step 3, the database grows (`100 -> 108 -> 118` pages), so `sqlite3_backup_remaining` **rises** (`95 -> 90 -> 93 -> 98 -> 93`) even though nothing restarted. Tracking `copied = pagecount - remaining` (`= iNext - 1`) shows steady progress: `5, 10, 15, 20, 25`. After step 5, the **writer child** commits step `13`; step 6 immediately drops `copied` back to **`5`** (`remaining 119 of 124`) because `pager_reset` called `sqlite3BackupRestart`. With no further external writes, steps `6..30` finish cleanly (`remaining 0 of 124`), matching **step 13 at E2**.
  - **Starvation under a steady writer (B4):** with the writer child committing steps `14..21` after every 5-page backup step, `remaining` climbs `119, 127, 139, 155, 157, 165, 166, 179` across 8 steps before giving up. A stepped backup can starve indefinitely under continuous external writes.
- **Section C (summary):** All 15 checks pass (verdict `CAPTURE COMPARISON OK`).

---

## 4. Timeline

### 4.1 Stepped backup racing a writer (Experiment 9.1 Part B, 5 pages per step)

| Backup step | Source `pagecount` | Copied (`iNext - 1`) | `remaining` (`pagecount - copied`) | Event immediately after the step |
| --- | --- | --- | --- | --- |
| `1` | `100` | `5` | `95` | — |
| `2` | `100` | `10` | `90` | Backup's own connection commits step `11` (`sqlite3BackupUpdate`; DB grows to `108`) |
| `3` | `108` | `15` | `93` | Backup's own connection commits step `12` (`sqlite3BackupUpdate`; DB grows to `118`) |
| `4` | `118` | `20` | `98` | — (`remaining` rose from `90` to `98` without restarting) |
| `5` | `118` | `25` | `93` | **Writer child** commits step `13` (`pager_reset` -> `sqlite3BackupRestart`) |
| `6` | `124` | `5` | `119` | Restarted at page `1` (`iNext = 1`) |
| `30` | `124` | `124` | `0` | Finished (`SQLITE_DONE`; schema cookie set to `1`; E2 equal to step `13`) |

When the writer child commits after **every** step (`steps 14..21`), `copied` resets to `5` on every iteration while `pagecount` grows from `124` to `184`, pushing `remaining` from `119` to `179` with zero net progress.

### 4.2 Killing a WAL writer at every `fsync` (Experiment 9.2 Part A, steps 4–11, `PASSIVE` checkpoint after step 7)

| `fsync` # (`FULL`) | `fsync` # (`NORMAL`) | Target file | What SQLite is syncing | Last acked (`FULL` / `NORMAL`) | Recovered (`FULL` / `NORMAL`) |
| --- | --- | --- | --- | --- | --- |
| `#1` | `#1` | `r.db-wal` | New 32-byte WAL header (`walFrames`, `wal.c:4121-4124`) | `3` / `3` | `3` / `3` |
| `#2` | `#2` | `.` | Parent directory entry for the new `-wal` file | `3` / `3` | `3` / `3` |
| `#3` | — | `r.db-wal` | Step `4` commit sync (`wal.c:4196-4213`) | `3` / — | **`4`** / — |
| `#4` | — | `r.db-wal` | Step `5` commit sync | `4` / — | **`5`** / — |
| `#5` | — | `r.db-wal` | Step `6` commit sync | `5` / — | **`6`** / — |
| `#6` | — | `r.db-wal` | Step `7` commit sync | `6` / — | **`7`** / — |
| `#7` | `#3` | `r.db-wal` | Checkpoint's pre-backfill WAL sync (`wal.c:2277`) | `7` / `7` | `7` / `7` |
| `#8` | `#4` | `r.db` | Checkpoint's full-backfill DB sync (`wal.c:2333`) | `7` / `7` | `7` / `7` |
| `#9` | `#5` | `r.db-wal` | Restarted WAL's new 32-byte header (first write of step `8`) | `7` / `7` | `7` / `7` |
| `#10` | — | `r.db-wal` | Step `8` commit sync | `7` / — | **`8`** / — |
| `#11` | — | `r.db-wal` | Step `9` commit sync | `8` / — | **`9`** / — |
| `#12` | — | `r.db-wal` | Step `10` commit sync | `9` / — | **`10`** / — |
| `#13` | — | `r.db-wal` | Step `11` commit sync | `10` / — | **`11`** / — |

Notice `FULL` syncs `#3–#6` and `#10–#13`: killing on **entry** to a commit's `fsync` recovers `acknowledged + 1`, because SQLite already finished every `pwrite64` for that transaction (including its commit frame) into the kernel page cache before calling `fsync`.

### 4.3 One `walship` shipping cycle (Experiment 9.4)

```text
  control conn                        pin conn                         Store / -wal
  ------------                        --------                         ------------
  (idle)                              holds BEGIN + read (read mark)   writers append frames
  BEGIN IMMEDIATE  --------------------------------------------------> writers wait on busy_timeout
  read -wal via os.ReadFile (POSIX-lock-safe: no locks on -wal)
  classify(generation, lastCommit, checksum)
    -> WALDiscontinuity: readPages -> PutIfAbsent snapshot (pos+1)
    -> WALUnchanged / WALNewGeneration:
       enumerate CommitPoints -> ApplySegment per commit -> PutIfAbsent segment
                                      COMMIT (drop read mark)
                                      PRAGMA wal_checkpoint(PASSIVE)   copies shipped frames -> .db
                                      BEGIN + SELECT sqlite_schema     re-acquires pin (slot 0 if full)
  [if sinceSnapshot >= SnapshotEvery:
     readPages -> verify SHA256 == s.state -> PutIfAbsent periodic snapshot]
  COMMIT  -----------------------------------------------------------> next writer commit restarts WAL
```

---

## 5. Lab

This section runs the crash-versus-power-loss lab (Experiment 9.2) and then walks through the complete source code of `internal/walship`, the rung R6 continuous capture-and-restore package built on top of Chapters 5 and 6.

### 5.1 Experiment 9.2 — `crash-versus-power-loss`

- **Claim:**
  - **Part A and Part B (process crash):** After a `SIGKILL` or unclosed exit at any point—across `synchronous=OFF`, `NORMAL`, and `FULL`—reopening the WAL database passes `PRAGMA integrity_check` and recovers an exact recipe step equal to `lastAcknowledged` or `lastAcknowledged + 1` (never a corrupt or non-recipe state), because completed `pwrite64` calls survive in the OS page cache.
  - **Part B (power-cut model, portable; cross-checked by `strace` on Linux):** Dropping every WAL byte written after the last `fsync` yields:
    - At `synchronous=FULL`: **zero** acknowledged commits lost (recovers step `11`).
    - At `synchronous=NORMAL`: a consistent database that **loses the 4 acknowledged commits** written after the last checkpoint (recovers step `7`). When the checkpoint copied every frame (`74/74`) and the next commit restarted the WAL, the last WAL sync was the restarted 32-byte header (`cut = 32` of `321,392` bytes). When a concurrent reader from step `5` stopped the checkpoint early (`36/74` frames) so the WAL did **not** restart, the last WAL sync was the checkpoint's own WAL sync (`wal.c:2277`, `cut = 304,912` of `626,272` bytes), and the checkpoint left `15` unsynced writes in `.db` (`wal.c:2328-2335`); both the post-checkpoint `.db` and the pre-checkpoint `.db` recover step `7` because those pages are covered by synced WAL frames.
    - At `synchronous=OFF`: the WAL-truncation model **does not apply**, because the checkpoint wrote `44` unsynced `pwrite64`s to `.db` with `0` syncs on either `.db` or `-wal`, so a power cut can leave any arbitrary mix of old and new database pages.
- **Scope statement:** The power-cut model shows **one** outcome of a power cut—the simplest one, where every synced byte survives and every unsynced byte disappears. It does not simulate torn sector writes, reordered writes, or disks that lie about `fsync`. SQLite's defenses against torn or reordered WAL writes (salts and per-frame cumulative checksums, `src/wal.c:1390-1614`) are read in the source rather than hardware-simulated. This experiment is the whole of the course's power-loss material.
- **Evidence form:** `EXACT` (every stored table must match on every run).
- **Linux only (Part B `strace` cross-check and Part A kill sweep):** The portable rule calculation and recovery run natively on Linux and macOS (try `-portable` on Linux to observe the non-`strace` path). `course run` runs the full `strace` cross-check and kill sweep in the Linux `lab` container; the `-portable` variant is run directly in the container with `docker compose run --rm lab go run ./ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss -portable`.

<!-- include: experiments/9.2-crash-versus-power-loss/main.go -->
```go
// Experiment 9.2 separates process-crash recovery from power-loss durability
// in WAL mode across synchronous=OFF, NORMAL, and FULL (plus a NORMAL run with
// a concurrent reader that stops the PASSIVE checkpoint short of the end).
// Part B (portable) compares process-crash recovery against a file-level
// power-cut model derived from SQLite's sync rules (src/pager.c:3658-3704,
// src/wal.h:25-26, src/wal.c:2199-2395, 4038-4266) and cross-checked by strace
// on Linux. Part A (Linux only) kills the writer child via strace --inject on
// entry to every sync and six spread pwrite64 calls to verify crash recovery.
//
// It prints only its verdict (and LINUX-ONLY notices where strace is missing).
// The measurements go to CSV tables that report.sql presents:
// go run ./cmd/course run 9.2
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
	"github.com/nickstrad/walfleet/internal/walfile"
)

const (
	childMode              = "child-writer"
	baseStep               = 3
	lastStep               = 11
	checkpointAfter        = 7
	pwriteKills            = 6
	readerFromStep         = 5
	beforeCheckpointSuffix = ".before-checkpoint"
	packagePath            = "ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss"
)

var levels = []string{"OFF", "NORMAL", "FULL"}

type variant struct {
	level      string
	readerFrom int
}

func (v variant) String() string {
	if v.readerFrom == 0 {
		return "synchronous=" + v.level
	}
	return fmt.Sprintf("synchronous=%s with a second connection holding a read transaction from step %d", v.level, v.readerFrom)
}

func (v variant) dirName() string {
	if v.readerFrom == 0 {
		return strings.ToLower(v.level)
	}
	return fmt.Sprintf("%s-reader-%d", strings.ToLower(v.level), v.readerFrom)
}

var modelVariants = []variant{{"OFF", 0}, {"NORMAL", 0}, {"FULL", 0}, {"NORMAL", readerFromStep}}

// runChild executes the writer workload on one pinned OS thread (so strace's
// syscall counter sees its I/O calls in deterministic order), commits recipe
// steps 4-11 with one PASSIVE checkpoint after step 7, and exits with os.Exit(0)
// without closing SQLite so the files remain exactly as a crashed process leaves them.
func runChild(args []string) error {
	flags := flag.NewFlagSet(childMode, flag.ContinueOnError)
	db := flags.String("db", "", "")
	level := flags.String("sync", "", "")
	readerFrom := flags.Int("reader-from", 0, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	options := r.SQLiteOptions()
	options.Conn.Synchronous = *level
	options.Conn.WALAutocheckpoint = 0
	handle, err := sqlitex.Open(ctx, *db, options)
	if err != nil {
		return err
	}
	var reader *sql.Conn
	for k := baseStep + 1; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, handle, k); err != nil {
			return err
		}
		wal, err := walfile.ParseFile(*db + "-wal")
		if err != nil {
			return err
		}
		fmt.Printf("commit %d end=%d generation=%d\n", k, walfile.HeaderSize+wal.LastCommitFrame*wal.FrameSize(), wal.Header.Salt1)
		fmt.Printf("ack %d\n", k)
		if k == *readerFrom {
			if reader, err = openReader(ctx, *db, options); err != nil {
				return err
			}
			fmt.Printf("reader %d\n", k)
		}
		if k == checkpointAfter {
			if reader != nil {
				if err := copyFile(*db, *db+beforeCheckpointSuffix, -1); err != nil {
					return err
				}
			}
			result, err := sqlitex.Checkpoint(ctx, handle, "", sqlitex.CheckpointPassive)
			if err != nil {
				return err
			}
			fmt.Printf("checkpoint %d %s\n", k, result)
		}
	}
	if reader != nil {
		fmt.Println("reader still open")
	}
	os.Exit(0)
	return nil
}

func openReader(ctx context.Context, path string, options sqlitex.Options) (*sql.Conn, error) {
	handle, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return nil, err
	}
	conn, err := handle.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	var tables int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		return nil, err
	}
	return conn, nil
}

type writerRun struct {
	lastAck        int
	commitEnd      map[int]int64
	generation     map[int]uint32
	checkpoint     string
	checkpointBusy int
	checkpointed   int
	logFrames      int
	readerFrom     int
	lastEvent      string
}

func parseRun(stdout string) (writerRun, error) {
	run := writerRun{lastAck: baseStep, commitEnd: map[int]int64{}, generation: map[int]uint32{}}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		var k int
		var end int64
		var generation uint32
		switch {
		case strings.HasPrefix(line, "ack "):
			if _, err := fmt.Sscanf(line, "ack %d", &k); err != nil || k != run.lastAck+1 {
				return run, fmt.Errorf("bad ack line %q after %d", line, run.lastAck)
			}
			run.lastAck, run.lastEvent = k, "commit"
		case strings.HasPrefix(line, "commit "):
			if _, err := fmt.Sscanf(line, "commit %d end=%d generation=%d", &k, &end, &generation); err != nil {
				return run, fmt.Errorf("bad commit line %q", line)
			}
			run.commitEnd[k], run.generation[k] = end, generation
		case strings.HasPrefix(line, "checkpoint "):
			if _, err := fmt.Sscanf(line, "checkpoint %d busy=%d log=%d checkpointed=%d", &k, &run.checkpointBusy, &run.logFrames, &run.checkpointed); err != nil || k != checkpointAfter {
				return run, fmt.Errorf("bad checkpoint line %q", line)
			}
			run.checkpoint, run.lastEvent = line, "checkpoint"
		case strings.HasPrefix(line, "reader "):
			if line == "reader still open" {
				continue
			}
			if _, err := fmt.Sscanf(line, "reader %d", &run.readerFrom); err != nil {
				return run, fmt.Errorf("bad reader line %q", line)
			}
		default:
			return run, fmt.Errorf("unexpected child output %q", line)
		}
	}
	return run, scanner.Err()
}

type syncRule struct {
	cut              int64
	hasCut           bool
	outside          string
	databaseUnsynced bool
}

// ruleCut computes which WAL bytes were synced and whether the database file
// holds unsynced checkpoint writes, directly from SQLite's sync-flag rules:
// sqlite3PagerSetFlags (src/pager.c:3658-3704) + WAL_SYNC_FLAGS/CKPT_SYNC_FLAGS
// (src/wal.h:25-26), walFrames (src/wal.c:4121-4124, 4196-4213), and
// walCheckpoint (src/wal.c:2277 syncs the WAL before backfilling; 2328-2335
// syncs the database file only when mxSafeFrame == mxFrame).
func ruleCut(level string, run writerRun) syncRule {
	partialCheckpoint := run.checkpointed > 0 && run.checkpointed < run.logFrames
	switch level {
	case "OFF":
		// At OFF, syncFlags and walSyncFlags are both 0; a checkpoint writes
		// the database file without syncing it, putting power loss outside a
		// WAL-truncation model.
		if run.checkpoint != "" {
			return syncRule{cut: 0, hasCut: true, outside: "the checkpoint wrote the database file and nothing was ever synced", databaseUnsynced: true}
		}
		return syncRule{cut: 0, hasCut: true}
	case "NORMAL":
		// At NORMAL, CKPT_SYNC_FLAGS is non-zero (header + checkpoint syncs)
		// while WAL_SYNC_FLAGS is 0 (no per-commit sync). If a partial
		// checkpoint stopped WAL restart, the last WAL sync is wal.c:2277 at
		// the checkpointed end; if the WAL restarted, it is the 32-byte header
		// sync at wal.c:4121-4124.
		if run.lastEvent == "checkpoint" {
			return syncRule{databaseUnsynced: partialCheckpoint}
		}
		if run.checkpointed > 0 && run.generation[run.lastAck] == run.generation[checkpointAfter] {
			return syncRule{cut: run.commitEnd[checkpointAfter], hasCut: true, databaseUnsynced: partialCheckpoint}
		}
		return syncRule{cut: walfile.HeaderSize, hasCut: true}
	}
	return syncRule{databaseUnsynced: partialCheckpoint}
}

type lab struct {
	traced bool
	ctx    context.Context
	lab    *procs.Lab
	r      recipe.Recipe
	dir    string
	base   string
	set    *results.Set
	cuts   int
	tables struct {
		runs, commits, syncs, rules, powerCuts, killPlans, kills, skipped *results.Table
	}
}

func (x *lab) openTables() {
	x.tables.runs = x.set.Table("runs", "seq", "variant", "synchronous", "reader_from", "first_acknowledged", "last_acknowledged", "checkpoint_after", "checkpoint_busy", "checkpoint_log", "checkpointed", "same_generation_after_checkpoint", "crash_integrity", "crash_recovered_step", "crash_exact_bytes")
	x.tables.commits = x.set.Table("commits", "variant", "step", "wal_end", "wal_generation")
	x.tables.syncs = x.set.Table("syncs", "variant", "file", "syncs", "writes_after_last_sync", "lowest_unsynced_offset")
	x.tables.rules = x.set.Table("sync_rules", "variant", "wal_synced_up_to", "database_file_unsynced", "power_model_not_applicable")
	x.tables.powerCuts = x.set.Table("power_cuts", "seq", "variant", "database_file", "wal_kept_bytes", "wal_bytes", "lost_commits", "integrity", "recovered_step", "exact_bytes")
	x.tables.killPlans = x.set.Table("kill_plans", "synchronous", "sync_calls", "pwrite64_calls", "pwrite64_kills")
	x.tables.kills = x.set.Table("kills", "synchronous", "kill_no", "syscall", "call_no", "killed_call", "signal", "acknowledged", "recovered_step", "exact_bytes", "integrity")
	x.tables.skipped = x.set.Table("skipped", "part", "variant", "what")
}

// linuxOnly prints the LINUX-ONLY notice and records it in the skipped table.
func (x *lab) linuxOnly(part string, variant any, what string) {
	fmt.Println(trace.LinuxOnlyMessage(what, packagePath))
	x.tables.skipped.Row(part, variant, what)
}

func copyFile(from, to string, limit int64) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	var reader io.Reader = in
	if limit >= 0 {
		reader = io.LimitReader(in, limit)
	}
	_, copyErr := io.Copy(out, reader)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func (x *lab) recover(path string) (string, recipe.Match) {
	db, err := x.r.Open(x.ctx, path)
	if err != nil {
		x.lab.Fatal("reopen", err)
	}
	var integrity string
	err = db.QueryRowContext(x.ctx, "PRAGMA integrity_check").Scan(&integrity)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("integrity_check", err)
	}
	match, err := x.r.MatchStep(x.ctx, path)
	if err != nil {
		x.lab.Fatal("match", err)
	}
	return integrity, match
}

func (x *lab) runWriter(dir string, v variant, kill *trace.KillAt) (string, *trace.Result) {
	db := filepath.Join(dir, "r.db")
	if err := copyFile(x.base, db, -1); err != nil {
		x.lab.Fatal("copy base", err)
	}
	args := []string{childMode, "-db", db, "-sync", v.level, "-reader-from", fmt.Sprint(v.readerFrom)}
	if !x.traced {
		self, err := os.Executable()
		if err != nil {
			x.lab.Fatal("executable", err)
		}
		out, err := exec.Command(self, args...).Output()
		if err != nil {
			x.lab.Fatal("writer", err)
		}
		return string(out), nil
	}
	command, err := trace.SelfCommand(args...)
	if err != nil {
		x.lab.Fatal("command", err)
	}
	result, err := trace.Run(x.ctx, trace.Options{Command: command, KillAt: kill})
	if err != nil {
		x.lab.Fatal("strace", err)
	}
	if kill == nil && result.ExitCode != 0 {
		x.lab.Fatal("writer", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr))
	}
	return result.Stdout, result
}

func (x *lab) model(seq int, v variant) *trace.Result {
	level := v.level
	dir := filepath.Join(x.dir, "model-"+v.dirName())
	stdout, result := x.runWriter(dir, v, nil)
	run, err := parseRun(stdout)
	if err != nil {
		x.lab.Fatal("parse", err)
	}
	db := filepath.Join(dir, "r.db")
	sameGeneration := run.generation[run.lastAck] == run.generation[checkpointAfter]
	if v.readerFrom != 0 {
		x.set.Check("B", run.readerFrom == v.readerFrom && run.checkpointed > 0 && run.checkpointed < run.logFrames && sameGeneration,
			fmt.Sprintf("%s: the reader stopped the checkpoint short of the end and the WAL did not restart", v))
	}
	generation := 0
	for k := baseStep + 1; k <= run.lastAck; k++ {
		if k == baseStep+1 || run.generation[k] != run.generation[k-1] {
			generation++
		}
		x.tables.commits.Row(v.dirName(), k, run.commitEnd[k], generation)
	}
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(db+suffix, filepath.Join(dir, "crash", "r.db"+suffix), -1); err != nil {
			x.lab.Fatal("copy", err)
		}
	}
	integrity, match := x.recover(filepath.Join(dir, "crash", "r.db"))
	var readerFrom any
	if v.readerFrom != 0 {
		readerFrom = v.readerFrom
	}
	x.tables.runs.Row(seq, v.dirName(), level, readerFrom, baseStep+1, run.lastAck, checkpointAfter, run.checkpointBusy, run.logFrames, run.checkpointed, sameGeneration, integrity, match.Step, match.Exact)
	x.set.Check("B", integrity == "ok" && match.Step == run.lastAck, fmt.Sprintf("%s: a process crash keeps every acknowledged step (%d)", v, run.lastAck))

	rule := ruleCut(level, run)
	cut, hasCut, outside := rule.cut, rule.hasCut, rule.outside
	var syncedUpTo, notApplicable any
	if hasCut {
		syncedUpTo = cut
	}
	if outside != "" {
		notApplicable = outside
	}
	x.tables.rules.Row(v.dirName(), syncedUpTo, rule.databaseUnsynced, notApplicable)
	if result != nil {
		real, err := filepath.EvalSymlinks(db)
		if err != nil {
			x.lab.Fatal("path", err)
		}
		walSyncs := trace.SyncsOf(result.Log.Calls, real+"-wal")
		dbSyncs := trace.SyncsOf(result.Log.Calls, real)
		x.syncRow(v, "-wal", walSyncs)
		x.syncRow(v, "database file", dbSyncs)
		x.set.Check("B", walSyncs.HasUnsynced == hasCut && (!hasCut || walSyncs.FirstUnsynced == cut) && dbSyncs.HasUnsynced == rule.databaseUnsynced,
			fmt.Sprintf("%s: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced", v))
	} else {
		x.linuxOnly("B", v.dirName(), "the strace confirmation of the synced bytes (the model below uses the rule read from wal.c and pager.c)")
	}
	if outside != "" {
		x.set.Check("B", level == "OFF", "only synchronous=OFF leaves database-file writes that were never synced")
		return result
	}
	walData, err := os.ReadFile(db + "-wal")
	if err != nil {
		x.lab.Fatal("read wal", err)
	}
	keep := int64(len(walData))
	if hasCut {
		keep = cut
	}
	lost := 0
	for k := baseStep + 1; k <= run.lastAck; k++ {
		if run.generation[k] == run.generation[run.lastAck] && run.commitEnd[k] > keep {
			lost++
		}
	}
	power := filepath.Join(dir, "power", "r.db")
	if err := errors.Join(copyFile(db, power, -1), copyFile(db+"-wal", power+"-wal", keep)); err != nil {
		x.lab.Fatal("copy", err)
	}
	integrity, match = x.recover(power)
	x.cuts++
	x.tables.powerCuts.Row(x.cuts, v.dirName(), "as written", keep, len(walData), lost, integrity, match.Step, match.Exact)
	x.set.Check("B", integrity == "ok" && match.Step == run.lastAck-lost, fmt.Sprintf("%s: the model recovers step %d, the last acknowledged step minus the %d commit(s) written after the last sync", v, run.lastAck-lost, lost))
	if rule.databaseUnsynced {
		before := filepath.Join(dir, "power-before-checkpoint", "r.db")
		if err := errors.Join(copyFile(db+beforeCheckpointSuffix, before, -1), copyFile(db+"-wal", before+"-wal", keep)); err != nil {
			x.lab.Fatal("copy", err)
		}
		beforeIntegrity, beforeMatch := x.recover(before)
		x.cuts++
		x.tables.powerCuts.Row(x.cuts, v.dirName(), "as before the checkpoint", keep, len(walData), lost, beforeIntegrity, beforeMatch.Step, beforeMatch.Exact)
		x.set.Check("B", beforeIntegrity == "ok" && beforeMatch.Step == match.Step,
			fmt.Sprintf("%s: with or without the checkpoint's unsynced database writes the model recovers step %d, because the WAL frames they came from were synced before the checkpoint wrote them", v, match.Step))
	}
	if level == "NORMAL" {
		x.set.Check("B", lost > 0 && match.Step < run.lastAck && match.Step == checkpointAfter, fmt.Sprintf("%s: the acknowledged commits after the checkpoint at step %d are lost in the model, yet the database is consistent", v, checkpointAfter))
	}
	if level == "FULL" {
		x.set.Check("B", lost == 0 && match.Step == run.lastAck, "FULL: no acknowledged commit is lost in the model")
	}
	return result
}

func (x *lab) syncRow(v variant, file string, r trace.SyncRecord) {
	var lowest any
	if r.HasUnsynced {
		lowest = r.FirstUnsynced
	}
	x.tables.syncs.Row(v.dirName(), file, r.Syncs, len(r.Unsynced), lowest)
}

// writerThread identifies the OS thread (tid) that executed the writer's
// pwrite64 calls so killSweep counts syscalls on the LockOSThread-pinned writer.
func writerThread(result *trace.Result) int {
	counts := map[int]int{}
	for _, call := range result.Log.Calls {
		if call.Name == "pwrite64" {
			counts[call.PID]++
		}
	}
	best, most := 0, 0
	for pid, n := range counts {
		if n > most {
			best, most = pid, n
		}
	}
	return best
}

// killSweep kills a fresh writer child via strace --inject=SIGKILL on entry to
// every sync and six evenly spaced pwrite64 calls, then reopens the database to
// verify walIndexRecover (src/wal.c:1390-1614) always yields an exact recipe
// step equal to lastAck or lastAck+1 (the commit whose frames were already in
// the kernel page cache when its fsync was interrupted).
func (x *lab) killSweep(level string, uninterrupted *trace.Result) {
	tid := writerThread(uninterrupted)
	syncs, writes := 0, 0
	for _, call := range uninterrupted.Log.Calls {
		if call.PID != tid {
			continue
		}
		switch call.Name {
		case "fsync", "fdatasync":
			syncs++
		case "pwrite64":
			writes++
		}
	}
	x.tables.killPlans.Row(level, syncs, writes, pwriteKills)
	var points []trace.KillAt
	for i := 1; i <= syncs; i++ {
		points = append(points, trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: i})
	}
	for j := 0; j < pwriteKills; j++ {
		points = append(points, trace.KillAt{Syscalls: []string{"pwrite64"}, When: 1 + j*(writes-1)/(pwriteKills-1)})
	}
	for i, point := range points {
		dir := filepath.Join(x.dir, fmt.Sprintf("kill-%s-%02d", strings.ToLower(level), i))
		stdout, result := x.runWriter(dir, variant{level, 0}, &point)
		run, err := parseRun(stdout)
		if err != nil {
			x.lab.Fatal("parse", err)
		}
		killed := "nothing (the run finished)"
		for _, call := range trace.RelativeTo(dir, result.Log.Calls) {
			if call.Unfinished {
				killed = call.String()
			}
		}
		integrity, match := x.recover(filepath.Join(dir, "r.db"))
		x.tables.kills.Row(level, i+1, point.Syscalls[0], point.When, killed, result.Killed, run.lastAck, match.Step, match.Exact, integrity)
		x.set.Check("A", result.Killed == "SIGKILL" && integrity == "ok" && match.Found() && match.Step >= run.lastAck && match.Step <= run.lastAck+1,
			fmt.Sprintf("%s kill %d: integrity ok, recovered step %d is the last acknowledged step or the one being committed", level, i+1, match.Step))
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == childMode {
		if err := runChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	portable := flag.Bool("portable", false, "run the path used where strace is missing, even on Linux")
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	x := &lab{traced: trace.Supported && !*portable, ctx: ctx, lab: procs.NewLab(os.Stdout), r: recipe.Default(), dir: dir, base: filepath.Join(dir, "base", "r.db"), set: results.MustOpen(dir)}
	x.openTables()
	if err := os.MkdirAll(filepath.Dir(x.base), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(ctx, x.base, baseStep); err != nil {
		x.lab.Fatal("build", err)
	}
	// Part B: what a process crash keeps, and what the simplest power cut keeps.
	traces := map[variant]*trace.Result{}
	for i, v := range modelVariants {
		traces[v] = x.model(i+1, v)
	}
	// Part A: kill the writer at chosen syscalls, reopen, identify the state.
	if !x.traced {
		x.linuxOnly("A", nil, "killing the writer at a chosen syscall with strace")
	} else {
		for _, level := range levels {
			x.killSweep(level, traces[variant{level, 0}])
		}
	}
	x.set.Finish("CRASH VERSUS POWER LOSS")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/9.2-crash-versus-power-loss/report.sql -->
```sql
-- Walkthrough of experiment 9.2. Run it with:
--   go run ./cmd/course run 9.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   runs         part B: one row per uninterrupted writer run (one per variant),
--                with what a process crash at its end recovers
--   commits      part B: one row per acknowledged commit of each run: where it
--                ends in the -wal and which WAL generation it is in (1, 2, ...)
--   sync_rules   part B: per variant, the bytes SQLite's sync rules say were synced
--   syncs        part B: per variant and file, what strace recorded (Linux only)
--   power_cuts   part B: one row per power-cut model: files cut back to the last sync
--   kill_plans   part A: per synchronous level, the calls an uninterrupted writer made
--   kills        part A: one row per writer killed at a chosen syscall (Linux only)
--   skipped      one row per LINUX-ONLY notice (empty on Linux)
--   checks       every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.2  Crash versus power loss'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The recipe database is built to step 3. A writer child process commits'
.print 'steps 4-11 on one connection in WAL mode (wal_autocheckpoint=0), runs one'
.print 'PASSIVE checkpoint after step 7, and exits without closing SQLite, so its'
.print 'files stay exactly as a crashed process leaves them. It prints an'
.print 'acknowledgement after each commit returns.'
.print
.print 'A process crash keeps what the kernel already holds. A power cut keeps'
.print 'only what was synced. Part B compares the two for synchronous=OFF, NORMAL'
.print 'and FULL, and for NORMAL with a second connection holding a read'
.print 'transaction from step 5. Part A (Linux only) kills the writer at chosen'
.print 'system calls.'
.print
.print 'Parts skipped on this platform (LINUX-ONLY notices):'

SELECT part, coalesce(variant, '') AS variant, what FROM skipped
UNION ALL
SELECT 'none', '', 'strace ran: nothing was skipped' WHERE NOT EXISTS (SELECT 1 FROM skipped)
ORDER BY part, variant;

.print
.print '── B. What a process crash keeps, and what the simplest power cut keeps'
.print
.print 'B1. The four runs. Each writer acknowledged steps 4-11. The checkpoint'
.print '    copied every frame, so the next commit restarted the WAL, except with'
.print '    the reader: it stopped the checkpoint short of the end, and the'
.print '    commits after it stayed in the same WAL generation.'

SELECT variant, synchronous, reader_from,
       first_acknowledged || '-' || last_acknowledged AS acknowledged,
       'after ' || checkpoint_after || ': busy=' || checkpoint_busy || ', copied ' || checkpointed || ' of ' || checkpoint_log AS checkpoint,
       same_generation_after_checkpoint AS wal_not_restarted
FROM runs ORDER BY seq;

.print
.print 'B2. A process crash: the database file and -wal copied as the kernel holds'
.print '    them, then reopened. Every variant recovers every acknowledged step,'
.print '    even synchronous=OFF. "exact_bytes = false" means equal to the recipe'
.print '    step except the change counter (header bytes 24-27 and 92-95).'

SELECT variant, last_acknowledged, crash_integrity AS integrity_check,
       crash_recovered_step AS recovered_step, crash_exact_bytes AS exact_bytes
FROM runs ORDER BY seq;

.print
.print 'B3. Which -wal bytes were synced. The rule column is read from the SQLite'
.print '    sync rules (src/pager.c:3658-3704, src/wal.h:25-26, src/wal.c); the'
.print '    strace columns are what the writer actually did. OFF never syncs;'
.print '    NORMAL syncs only around the checkpoint (up to byte 32, the header of'
.print '    the restarted WAL, or up to the checkpointed end when the reader kept'
.print '    the WAL from restarting); FULL syncs every commit.'

SELECT r.variant,
       coalesce(CAST(r.wal_synced_up_to AS VARCHAR), 'all of it') AS rule_synced_up_to,
       s.syncs, s.writes_after_last_sync,
       s.lowest_unsynced_offset,
       CASE WHEN s.variant IS NOT NULL
            THEN coalesce(CAST(s.lowest_unsynced_offset AS BIGINT), -1) = coalesce(r.wal_synced_up_to, -1) END AS agree
FROM sync_rules r LEFT JOIN syncs s ON s.variant = r.variant AND s.file = '-wal'
JOIN runs u ON u.variant = r.variant
ORDER BY u.seq;

.print
.print 'B4. The database file. A checkpoint that copies every frame syncs it; one'
.print '    that stops short (the reader) does not, and at OFF nothing is synced.'

SELECT r.variant, r.database_file_unsynced AS rule_unsynced_writes,
       s.syncs, s.writes_after_last_sync, s.lowest_unsynced_offset,
       r.database_file_unsynced = (CAST(s.writes_after_last_sync AS BIGINT) > 0) AS agree
FROM sync_rules r LEFT JOIN syncs s ON s.variant = r.variant AND s.file = 'database file'
JOIN runs u ON u.variant = r.variant
ORDER BY u.seq;

.print
.print 'B5. Where each acknowledged commit ends in the -wal (byte offset, then'
.print '    #WAL generation). A commit ending past the last sync in the newest'
.print '    generation is marked "lost": the power-cut model removes it.'

WITH cut AS (SELECT variant, wal_kept_bytes FROM power_cuts WHERE database_file = 'as written'),
     last_gen AS (SELECT variant, max(wal_generation) AS g FROM commits GROUP BY variant),
     marked AS (
       SELECT c.variant, c.step,
              c.wal_end || ' #' || c.wal_generation
                || CASE WHEN c.wal_generation = l.g AND c.wal_end > cut.wal_kept_bytes THEN ' lost' ELSE '' END AS ends
       FROM commits c JOIN last_gen l USING (variant) LEFT JOIN cut USING (variant))
SELECT step,
       first(ends) FILTER (variant = 'off')             AS "off",
       first(ends) FILTER (variant = 'normal')          AS normal,
       first(ends) FILTER (variant = 'full')            AS "full",
       first(ends) FILTER (variant = 'normal-reader-5') AS "normal-reader-5"
FROM marked GROUP BY step ORDER BY step;

.print
.print 'B6. The power-cut model: the database file as written, the -wal cut back'
.print '    to the last sync, no -shm; then reopened. NORMAL loses the four commits'
.print '    after the checkpoint but stays consistent; FULL loses nothing. With the'
.print '    reader the checkpoint did not sync the database file, so the model also'
.print '    runs with the file as it was before the checkpoint: the same step, since'
.print '    the WAL frames those writes came from were synced first.'

SELECT variant, database_file, wal_kept_bytes || ' of ' || wal_bytes AS wal_bytes_kept,
       lost_commits AS lost, integrity, recovered_step AS recovered, exact_bytes AS exact
FROM power_cuts ORDER BY seq;

.print
.print '    At OFF there is no model: the checkpoint wrote the database file and'
.print '    nothing was ever synced, so a power cut can leave any mix of old and'
.print '    new database pages.'

SELECT variant, power_model_not_applicable AS why_not
FROM sync_rules WHERE power_model_not_applicable IS NOT NULL ORDER BY variant;

.print
.print '── A. Kill the writer at chosen syscalls, reopen, identify the state ──'
.print
.print 'A1. For each synchronous level, a fresh writer runs under strace and is'
.print '    killed with SIGKILL on entry to one call: every fsync/fdatasync the'
.print '    uninterrupted writer made, and 6 of its pwrite64 calls spread evenly.'

SELECT synchronous, sync_calls, pwrite64_calls, pwrite64_kills,
       CAST(sync_calls AS BIGINT) + CAST(pwrite64_kills AS BIGINT) AS kills
FROM kill_plans ORDER BY CAST(sync_calls AS BIGINT);

.print
.print 'A2. Every kill. "recovered" is the recipe step the reopened database'
.print '    equals: the last acknowledged step, or (at FULL, killed in the fsync of'
.print '    a commit) the step being committed, whose frames were already written.'

SELECT synchronous, kill_no, syscall || ' #' || call_no AS kill_at,
       replace(killed_call, ' = ? (process killed)', '') AS killed_in,
       acknowledged, recovered_step AS recovered, exact_bytes AS exact
FROM kills
ORDER BY CASE synchronous WHEN 'OFF' THEN 1 WHEN 'NORMAL' THEN 2 ELSE 3 END, CAST(kill_no AS BIGINT);

.print
.print 'A3. Summary. Every kill was a SIGKILL, every reopened database passed'
.print '    integrity_check, and none lost an acknowledged step.'

SELECT synchronous, count(*) AS kills,
       count(*) FILTER (signal = 'SIGKILL')              AS sigkill,
       count(*) FILTER (integrity = 'ok')                AS integrity_ok,
       count(*) FILTER (recovered_step = acknowledged)     AS recovered_acknowledged,
       count(*) FILTER (CAST(recovered_step AS BIGINT) = CAST(acknowledged AS BIGINT) + 1) AS recovered_one_more
FROM kills GROUP BY synchronous
ORDER BY CASE synchronous WHEN 'OFF' THEN 1 WHEN 'NORMAL' THEN 2 ELSE 3 END;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 9.2
```

Add `-i` (`go run ./cmd/course run -i 9.2`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/9.2-crash-versus-power-loss/`](evidence/9.2-crash-versus-power-loss/), and `go run ./cmd/course check evidence ch09` compares a fresh run with them byte for byte.

<!-- include: evidence/9.2-crash-versus-power-loss.txt -->
```text

════════════════════════════════════════════════════════════════════════
  9.2  Crash versus power loss
════════════════════════════════════════════════════════════════════════

The recipe database is built to step 3. A writer child process commits
steps 4-11 on one connection in WAL mode (wal_autocheckpoint=0), runs one
PASSIVE checkpoint after step 7, and exits without closing SQLite, so its
files stay exactly as a crashed process leaves them. It prints an
acknowledgement after each commit returns.

A process crash keeps what the kernel already holds. A power cut keeps
only what was synced. Part B compares the two for synchronous=OFF, NORMAL
and FULL, and for NORMAL with a second connection holding a read
transaction from step 5. Part A (Linux only) kills the writer at chosen
system calls.

Parts skipped on this platform (LINUX-ONLY notices):
┌──────┬─────────┬─────────────────────────────────┐
│ part │ variant │              what               │
├──────┼─────────┼─────────────────────────────────┤
│ none │         │ strace ran: nothing was skipped │
└──────┴─────────┴─────────────────────────────────┘

── B. What a process crash keeps, and what the simplest power cut keeps

B1. The four runs. Each writer acknowledged steps 4-11. The checkpoint
    copied every frame, so the next commit restarted the WAL, except with
    the reader: it stopped the checkpoint short of the end, and the
    commits after it stayed in the same WAL generation.
┌─────────────────┬─────────────┬─────────────┬──────────────┬──────────────────────────────────┬───────────────────┐
│     variant     │ synchronous │ reader_from │ acknowledged │            checkpoint            │ wal_not_restarted │
├─────────────────┼─────────────┼─────────────┼──────────────┼──────────────────────────────────┼───────────────────┤
│ off             │ OFF         │             │ 4-11         │ after 7: busy=0, copied 74 of 74 │ false             │
│ normal          │ NORMAL      │             │ 4-11         │ after 7: busy=0, copied 74 of 74 │ false             │
│ full            │ FULL        │             │ 4-11         │ after 7: busy=0, copied 74 of 74 │ false             │
│ normal-reader-5 │ NORMAL      │ 5           │ 4-11         │ after 7: busy=0, copied 36 of 74 │ true              │
└─────────────────┴─────────────┴─────────────┴──────────────┴──────────────────────────────────┴───────────────────┘

B2. A process crash: the database file and -wal copied as the kernel holds
    them, then reopened. Every variant recovers every acknowledged step,
    even synchronous=OFF. "exact_bytes = false" means equal to the recipe
    step except the change counter (header bytes 24-27 and 92-95).
┌─────────────────┬───────────────────┬─────────────────┬────────────────┬─────────────┐
│     variant     │ last_acknowledged │ integrity_check │ recovered_step │ exact_bytes │
├─────────────────┼───────────────────┼─────────────────┼────────────────┼─────────────┤
│ off             │ 11                │ ok              │ 11             │ false       │
│ normal          │ 11                │ ok              │ 11             │ false       │
│ full            │ 11                │ ok              │ 11             │ false       │
│ normal-reader-5 │ 11                │ ok              │ 11             │ false       │
└─────────────────┴───────────────────┴─────────────────┴────────────────┴─────────────┘

B3. Which -wal bytes were synced. The rule column is read from the SQLite
    sync rules (src/pager.c:3658-3704, src/wal.h:25-26, src/wal.c); the
    strace columns are what the writer actually did. OFF never syncs;
    NORMAL syncs only around the checkpoint (up to byte 32, the header of
    the restarted WAL, or up to the checkpointed end when the reader kept
    the WAL from restarting); FULL syncs every commit.
┌─────────────────┬───────────────────┬───────┬────────────────────────┬────────────────────────┬───────┐
│     variant     │ rule_synced_up_to │ syncs │ writes_after_last_sync │ lowest_unsynced_offset │ agree │
├─────────────────┼───────────────────┼───────┼────────────────────────┼────────────────────────┼───────┤
│ off             │ 0                 │ 0     │ 306                    │ 0                      │ true  │
│ normal          │ 32                │ 3     │ 156                    │ 32                     │ true  │
│ full            │ all of it         │ 11    │ 0                      │                        │ true  │
│ normal-reader-5 │ 304912            │ 2     │ 156                    │ 304912                 │ true  │
└─────────────────┴───────────────────┴───────┴────────────────────────┴────────────────────────┴───────┘

B4. The database file. A checkpoint that copies every frame syncs it; one
    that stops short (the reader) does not, and at OFF nothing is synced.
┌─────────────────┬──────────────────────┬───────┬────────────────────────┬────────────────────────┬───────┐
│     variant     │ rule_unsynced_writes │ syncs │ writes_after_last_sync │ lowest_unsynced_offset │ agree │
├─────────────────┼──────────────────────┼───────┼────────────────────────┼────────────────────────┼───────┤
│ off             │ true                 │ 0     │ 44                     │ 0                      │ true  │
│ normal          │ false                │ 1     │ 0                      │                        │ true  │
│ full            │ false                │ 1     │ 0                      │                        │ true  │
│ normal-reader-5 │ true                 │ 0     │ 15                     │ 73728                  │ true  │
└─────────────────┴──────────────────────┴───────┴────────────────────────┴────────────────────────┴───────┘

B5. Where each acknowledged commit ends in the -wal (byte offset, then
    #WAL generation). A commit ending past the last sync in the newest
    generation is marked "lost": the power-cut model removes it.
┌──────┬───────────┬────────────────┬───────────┬─────────────────┐
│ step │    off    │     normal     │   full    │ normal-reader-5 │
├──────┼───────────┼────────────────┼───────────┼─────────────────┤
│ 4    │ 103032 #1 │ 103032 #1      │ 103032 #1 │ 103032 #1       │
│ 5    │ 148352 #1 │ 148352 #1      │ 148352 #1 │ 148352 #1       │
│ 6    │ 238992 #1 │ 238992 #1      │ 238992 #1 │ 238992 #1       │
│ 7    │ 304912 #1 │ 304912 #1      │ 304912 #1 │ 304912 #1       │
│ 8    │ 94792 #2  │ 94792 #2 lost  │ 94792 #2  │ 399672 #1 lost  │
│ 9    │ 189552 #2 │ 189552 #2 lost │ 189552 #2 │ 494432 #1 lost  │
│ 10   │ 259592 #2 │ 259592 #2 lost │ 259592 #2 │ 564472 #1 lost  │
│ 11   │ 321392 #2 │ 321392 #2 lost │ 321392 #2 │ 626272 #1 lost  │
└──────┴───────────┴────────────────┴───────────┴─────────────────┘

B6. The power-cut model: the database file as written, the -wal cut back
    to the last sync, no -shm; then reopened. NORMAL loses the four commits
    after the checkpoint but stays consistent; FULL loses nothing. With the
    reader the checkpoint did not sync the database file, so the model also
    runs with the file as it was before the checkpoint: the same step, since
    the WAL frames those writes came from were synced first.
┌─────────────────┬──────────────────────────┬──────────────────┬──────┬───────────┬───────────┬───────┐
│     variant     │      database_file       │  wal_bytes_kept  │ lost │ integrity │ recovered │ exact │
├─────────────────┼──────────────────────────┼──────────────────┼──────┼───────────┼───────────┼───────┤
│ normal          │ as written               │ 32 of 321392     │ 4    │ ok        │ 7         │ false │
│ full            │ as written               │ 321392 of 321392 │ 0    │ ok        │ 11        │ false │
│ normal-reader-5 │ as written               │ 304912 of 626272 │ 4    │ ok        │ 7         │ false │
│ normal-reader-5 │ as before the checkpoint │ 304912 of 626272 │ 4    │ ok        │ 7         │ false │
└─────────────────┴──────────────────────────┴──────────────────┴──────┴───────────┴───────────┴───────┘

    At OFF there is no model: the checkpoint wrote the database file and
    nothing was ever synced, so a power cut can leave any mix of old and
    new database pages.
┌─────────┬────────────────────────────────────────────────────────────────────┐
│ variant │                              why_not                               │
├─────────┼────────────────────────────────────────────────────────────────────┤
│ off     │ the checkpoint wrote the database file and nothing was ever synced │
└─────────┴────────────────────────────────────────────────────────────────────┘

── A. Kill the writer at chosen syscalls, reopen, identify the state ──

A1. For each synchronous level, a fresh writer runs under strace and is
    killed with SIGKILL on entry to one call: every fsync/fdatasync the
    uninterrupted writer made, and 6 of its pwrite64 calls spread evenly.
┌─────────────┬────────────┬────────────────┬────────────────┬───────┐
│ synchronous │ sync_calls │ pwrite64_calls │ pwrite64_kills │ kills │
├─────────────┼────────────┼────────────────┼────────────────┼───────┤
│ OFF         │ 0          │ 357            │ 6              │ 6     │
│ NORMAL      │ 5          │ 357            │ 6              │ 11    │
│ FULL        │ 13         │ 357            │ 6              │ 19    │
└─────────────┴────────────┴────────────────┴────────────────┴───────┘

A2. Every kill. "recovered" is the recipe step the reopened database
    equals: the last acknowledged step, or (at FULL, killed in the fsync of
    a commit) the step being committed, whose frames were already written.
┌─────────────┬─────────┬───────────────┬───────────────────────────────────────┬──────────────┬───────────┬───────┐
│ synchronous │ kill_no │    kill_at    │               killed_in               │ acknowledged │ recovered │ exact │
├─────────────┼─────────┼───────────────┼───────────────────────────────────────┼──────────────┼───────────┼───────┤
│ OFF         │ 1       │ pwrite64 #1   │ pwrite64 r.db-shm off=4095 len=1      │ 3            │ 3         │ true  │
│ OFF         │ 2       │ pwrite64 #72  │ pwrite64 r.db-wal off=127752 len=24   │ 4            │ 4         │ true  │
│ OFF         │ 3       │ pwrite64 #143 │ pwrite64 r.db-wal off=271976 len=4096 │ 6            │ 6         │ false │
│ OFF         │ 4       │ pwrite64 #214 │ pwrite64 r.db-wal off=24752 len=24    │ 7            │ 7         │ false │
│ OFF         │ 5       │ pwrite64 #285 │ pwrite64 r.db-wal off=168976 len=4096 │ 8            │ 8         │ false │
│ OFF         │ 6       │ pwrite64 #357 │ pwrite64 r.db-wal off=317296 len=4096 │ 10           │ 10        │ false │
│ NORMAL      │ 1       │ fsync #1      │ fsync r.db-wal                        │ 3            │ 3         │ true  │
│ NORMAL      │ 2       │ fsync #2      │ fsync .                               │ 3            │ 3         │ true  │
│ NORMAL      │ 3       │ fsync #3      │ fsync r.db-wal                        │ 7            │ 7         │ false │
│ NORMAL      │ 4       │ fsync #4      │ fsync r.db                            │ 7            │ 7         │ false │
│ NORMAL      │ 5       │ fsync #5      │ fsync r.db-wal                        │ 7            │ 7         │ false │
│ NORMAL      │ 6       │ pwrite64 #1   │ pwrite64 r.db-shm off=4095 len=1      │ 3            │ 3         │ true  │
│ NORMAL      │ 7       │ pwrite64 #72  │ pwrite64 r.db-wal off=127752 len=24   │ 4            │ 4         │ true  │
│ NORMAL      │ 8       │ pwrite64 #143 │ pwrite64 r.db-wal off=271976 len=4096 │ 6            │ 6         │ false │
│ NORMAL      │ 9       │ pwrite64 #214 │ pwrite64 r.db-wal off=24752 len=24    │ 7            │ 7         │ false │
│ NORMAL      │ 10      │ pwrite64 #285 │ pwrite64 r.db-wal off=168976 len=4096 │ 8            │ 8         │ false │
│ NORMAL      │ 11      │ pwrite64 #357 │ pwrite64 r.db-wal off=317296 len=4096 │ 10           │ 10        │ false │
│ FULL        │ 1       │ fsync #1      │ fsync r.db-wal                        │ 3            │ 3         │ true  │
│ FULL        │ 2       │ fsync #2      │ fsync .                               │ 3            │ 3         │ true  │
│ FULL        │ 3       │ fsync #3      │ fsync r.db-wal                        │ 3            │ 4         │ true  │
│ FULL        │ 4       │ fsync #4      │ fsync r.db-wal                        │ 4            │ 5         │ false │
│ FULL        │ 5       │ fsync #5      │ fsync r.db-wal                        │ 5            │ 6         │ false │
│ FULL        │ 6       │ fsync #6      │ fsync r.db-wal                        │ 6            │ 7         │ false │
│ FULL        │ 7       │ fsync #7      │ fsync r.db-wal                        │ 7            │ 7         │ false │
│ FULL        │ 8       │ fsync #8      │ fsync r.db                            │ 7            │ 7         │ false │
│ FULL        │ 9       │ fsync #9      │ fsync r.db-wal                        │ 7            │ 7         │ false │
│ FULL        │ 10      │ fsync #10     │ fsync r.db-wal                        │ 7            │ 8         │ false │
│ FULL        │ 11      │ fsync #11     │ fsync r.db-wal                        │ 8            │ 9         │ false │
│ FULL        │ 12      │ fsync #12     │ fsync r.db-wal                        │ 9            │ 10        │ false │
│ FULL        │ 13      │ fsync #13     │ fsync r.db-wal                        │ 10           │ 11        │ false │
│ FULL        │ 14      │ pwrite64 #1   │ pwrite64 r.db-shm off=4095 len=1      │ 3            │ 3         │ true  │
│ FULL        │ 15      │ pwrite64 #72  │ pwrite64 r.db-wal off=127752 len=24   │ 4            │ 4         │ true  │
│ FULL        │ 16      │ pwrite64 #143 │ pwrite64 r.db-wal off=271976 len=4096 │ 6            │ 6         │ false │
│ FULL        │ 17      │ pwrite64 #214 │ pwrite64 r.db-wal off=24752 len=24    │ 7            │ 7         │ false │
│ FULL        │ 18      │ pwrite64 #285 │ pwrite64 r.db-wal off=168976 len=4096 │ 8            │ 8         │ false │
│ FULL        │ 19      │ pwrite64 #357 │ pwrite64 r.db-wal off=317296 len=4096 │ 10           │ 10        │ false │
└─────────────┴─────────┴───────────────┴───────────────────────────────────────┴──────────────┴───────────┴───────┘

A3. Summary. Every kill was a SIGKILL, every reopened database passed
    integrity_check, and none lost an acknowledged step.
┌─────────────┬───────┬─────────┬──────────────┬────────────────────────┬────────────────────┐
│ synchronous │ kills │ sigkill │ integrity_ok │ recovered_acknowledged │ recovered_one_more │
├─────────────┼───────┼─────────┼──────────────┼────────────────────────┼────────────────────┤
│ OFF         │ 6     │ 6       │ 6            │ 6                      │ 0                  │
│ NORMAL      │ 11    │ 11      │ 11           │ 11                     │ 0                  │
│ FULL        │ 19    │ 19      │ 19           │ 11                     │ 8                  │
└─────────────┴───────┴─────────┴──────────────┴────────────────────────┴────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

B  ✓  synchronous=OFF: a process crash keeps every acknowledged step (11)
B  ✓  synchronous=OFF: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced
B  ✓  only synchronous=OFF leaves database-file writes that were never synced
B  ✓  synchronous=NORMAL: a process crash keeps every acknowledged step (11)
B  ✓  synchronous=NORMAL: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced
B  ✓  synchronous=NORMAL: the model recovers step 7, the last acknowledged step minus the 4 commit(s) written after the last sync
B  ✓  synchronous=NORMAL: the acknowledged commits after the checkpoint at step 7 are lost in the model, yet the database is consistent
B  ✓  synchronous=FULL: a process crash keeps every acknowledged step (11)
B  ✓  synchronous=FULL: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced
B  ✓  synchronous=FULL: the model recovers step 11, the last acknowledged step minus the 0 commit(s) written after the last sync
B  ✓  FULL: no acknowledged commit is lost in the model
B  ✓  synchronous=NORMAL with a second connection holding a read transaction from step 5: the reader stopped the checkpoint short of the end and the WAL did not restart
B  ✓  synchronous=NORMAL with a second connection holding a read transaction from step 5: a process crash keeps every acknowledged step (11)
B  ✓  synchronous=NORMAL with a second connection holding a read transaction from step 5: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced
B  ✓  synchronous=NORMAL with a second connection holding a read transaction from step 5: the model recovers step 7, the last acknowledged step minus the 4 commit(s) written after the last sync
B  ✓  synchronous=NORMAL with a second connection holding a read transaction from step 5: with or without the checkpoint's unsynced database writes the model recovers step 7, because the WAL frames they came from were synced before the checkpoint wrote them
B  ✓  synchronous=NORMAL with a second connection holding a read transaction from step 5: the acknowledged commits after the checkpoint at step 7 are lost in the model, yet the database is consistent
A  ✓  OFF kill 1: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  OFF kill 2: integrity ok, recovered step 4 is the last acknowledged step or the one being committed
A  ✓  OFF kill 3: integrity ok, recovered step 6 is the last acknowledged step or the one being committed
A  ✓  OFF kill 4: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  OFF kill 5: integrity ok, recovered step 8 is the last acknowledged step or the one being committed
A  ✓  OFF kill 6: integrity ok, recovered step 10 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 1: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 2: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 3: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 4: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 5: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 6: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 7: integrity ok, recovered step 4 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 8: integrity ok, recovered step 6 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 9: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 10: integrity ok, recovered step 8 is the last acknowledged step or the one being committed
A  ✓  NORMAL kill 11: integrity ok, recovered step 10 is the last acknowledged step or the one being committed
A  ✓  FULL kill 1: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  FULL kill 2: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  FULL kill 3: integrity ok, recovered step 4 is the last acknowledged step or the one being committed
A  ✓  FULL kill 4: integrity ok, recovered step 5 is the last acknowledged step or the one being committed
A  ✓  FULL kill 5: integrity ok, recovered step 6 is the last acknowledged step or the one being committed
A  ✓  FULL kill 6: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  FULL kill 7: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  FULL kill 8: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  FULL kill 9: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  FULL kill 10: integrity ok, recovered step 8 is the last acknowledged step or the one being committed
A  ✓  FULL kill 11: integrity ok, recovered step 9 is the last acknowledged step or the one being committed
A  ✓  FULL kill 12: integrity ok, recovered step 10 is the last acknowledged step or the one being committed
A  ✓  FULL kill 13: integrity ok, recovered step 11 is the last acknowledged step or the one being committed
A  ✓  FULL kill 14: integrity ok, recovered step 3 is the last acknowledged step or the one being committed
A  ✓  FULL kill 15: integrity ok, recovered step 4 is the last acknowledged step or the one being committed
A  ✓  FULL kill 16: integrity ok, recovered step 6 is the last acknowledged step or the one being committed
A  ✓  FULL kill 17: integrity ok, recovered step 7 is the last acknowledged step or the one being committed
A  ✓  FULL kill 18: integrity ok, recovered step 8 is the last acknowledged step or the one being committed
A  ✓  FULL kill 19: integrity ok, recovered step 10 is the last acknowledged step or the one being committed

53 of 53 claims hold

Experiment verdict: CRASH VERSUS POWER LOSS OK
```

#### Reading the Experiment 9.2 evidence

- **Section B, variant `off` (Part B — `synchronous=OFF`):** The writer commits steps `4–11`, checkpoints all `74` frames after step `7`, restarts the WAL for steps `8–11`, and calls `os.Exit(0)` without closing. Copying `.db` and `-wal` as left recovers **step 11** (`integrity_check ok`; the change-counter-only note is because one connection committed multiple steps, Chapter 1 correction 1). `strace` records **`0` syncs** on `-wal` (`306` unsynced writes from offset `0`) and **`0` syncs** on `.db` (`44` unsynced writes from offset `0`), matching `ruleCut`. Because `.db` itself was modified without syncing either `.db` or `-wal`, the power-cut model is marked not applicable. (Note on the wording of the Part B check `"only synchronous=OFF leaves database-file writes that were never synced"`: it was written before the fourth model run was added in the repair round; as B4 shows, a reader-pinned partial checkpoint at `NORMAL` also leaves unsynced `.db` writes, but at `OFF` uniquely the backing WAL frames were *also* never synced, leaving the power-cut outcome undefined.)
- **Variant `normal` (Part B — `synchronous=NORMAL`, full checkpoint):** Process crash again recovers **step 11**. `strace` counts **`3` syncs** on `-wal` (initial header, checkpoint WAL sync, restarted header) with `156` unsynced writes starting at byte `32`, and **`1` sync** on `.db` (checkpoint DB sync, zero writes after). Truncating `-wal` to byte `32` of `321,392` and removing `-shm` recovers **step 7** (`integrity_check ok`): all **4** acknowledged commits after the checkpoint (steps `8–11`) vanish, while the database stays consistent.
- **Variant `full` (Part B — `synchronous=FULL`):** Process crash recovers **step 11**. `strace` counts **`11` syncs** on `-wal` (the `3` from `NORMAL` plus `8` per-commit syncs) and `1` sync on `.db`, with **zero** writes after the last sync of either file. Keeping all `321,392` bytes of `-wal` recovers **step 11** with `0` acknowledged commits lost.
- **Variant `normal-reader-5` (Part B — `synchronous=NORMAL` with a reader from step `5`, repair-round addition):** The reader holds a read mark at step `5`, so the `PASSIVE` checkpoint copies only **`36` of `74` frames** and steps `8–11` stay in the **same WAL generation** (`wal_not_restarted` `true` in B1). `strace` confirms the corrected `NORMAL` rule: `-wal` has **`2` syncs** (initial header + checkpoint's `wal.c:2277` sync), leaving `156` unsynced writes starting at offset **`304,912`** (the exact end of step 7's commit frame, not byte `32`), and `.db` has **`0` syncs** with `15` unsynced writes from offset `73,728` (because `wal.c:2328-2335` skips syncing `.db` on a partial checkpoint). Truncating `-wal` to `304,912` of `626,272` bytes recovers **step 7** (`4` commits lost), and substituting the pre-checkpoint `.db` (dropping all `15` unsynced `.db` writes) also recovers **step 7** (`integrity_check ok`), because every page the partial checkpoint touched was already synced in `-wal` first.
- **A1–A3 (Part A — `strace --inject=SIGKILL` sweep, Linux only):** Across **`36` kill runs** (`6` at `OFF`: `0` syncs + `6` `pwrite64`s; `11` at `NORMAL`: `5` syncs including directory `fsync .` + `6` `pwrite64`s; `19` at `FULL`: `13` syncs including directory `fsync .` + `6` `pwrite64`s), every single kill recovers `integrity_check ok` and an exact recipe step equal to `acknowledged` or `acknowledged + 1`. Look closely at `FULL` kills `#3–#6` and `#10–#13` in A2: killing on entry to a commit's `fsync r.db-wal` recovers the step **being committed** (acknowledged `3`, recovered `4`, up to acknowledged `10`, recovered `11`). All 53 checks pass (verdict `CRASH VERSUS POWER LOSS OK`).

---

### 5.2 Walk-through of `internal/walship` (rung R6)

`internal/walship` is the shared continuous capture-and-restore package introduced by this chapter and used directly by the Chapter 10 capstone service. It is split into five files—`store.go`, `format.go`, `apply.go`, `catalog.go`, and `shipper.go`—inlined in full below.

#### 1. The object-store contract and `DirStore` (`store.go`)

`walship` talks to storage through a four-method `Store` interface (`PutIfAbsent`, `Get`, `List`, `Delete`). Three properties are load-bearing for Chapter 10's S3 adapter (verified against SeaweedFS in Phase 1):

- **`PutIfAbsent` is atomic and exclusive:** readers see either the entire object or nothing, and an existing key fails with `ErrExists` without modifying the stored bytes (in S3, `PutObject` with `If-None-Match: *`).
- **Read-after-write and list-after-write:** once `PutIfAbsent` returns `nil`, `Get` and `List` see the key immediately.
- **No dependence on `List` order:** `LoadCatalog` sorts keys itself, and fixed-width 12-digit decimal keys (`snapshots/000000000002`, `segments/000000000003`) make lexical order match numeric order.

`DirStore` implements `Store` on a local filesystem by writing and `fsync`ing a random temporary file inside `.tmp/`, hard-linking it to the final path with `os.Link` (which fails atomically with `EEXIST` if the key already exists, unlike `os.Rename`), removing the temp entry, and `fsync`ing the target directory.

<!-- include: ../internal/walship/store.go -->
```go
// Package walship implements Rebuild Ladder rung R6: continuous point-in-time
// capture and restore for a live WAL-mode SQLite database. A Shipper captures
// a base snapshot through sqlite_dbpage, holds a read mark so unshipped WAL
// frames cannot be overwritten, copies newly committed frames from the -wal
// file into numbered hash-chained segments in an object Store, and performs
// shipper-owned PASSIVE checkpoints only after shipping. A Catalog and Plan
// verify every object, WAL checksum chain, and state hash before replaying
// frames through internal/walfile to rebuild any commit position byte for byte.
package walship

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Sentinel errors returned by Store implementations.
var (
	// ErrExists reports that PutIfAbsent refused to overwrite an existing key,
	// or that WriteDatabase refused to write over an existing file or sidecar.
	ErrExists = errors.New("object already exists")
	// ErrNotFound reports that Get requested a key absent from the store.
	ErrNotFound = errors.New("object not found")
	// ErrInvalidKey reports that a key or prefix violates ValidateKey's syntax.
	ErrInvalidKey = errors.New("invalid object key")
)

// Store is the minimal object-store contract that walship requires and that
// Chapter 10 implements against S3-compatible storage (SeaweedFS).
//
// Implementations must guarantee:
//   - PutIfAbsent is atomic and exclusive: readers observe either the whole
//     object or none of it, and an existing key fails with ErrExists unchanged.
//   - Read-after-write and list-after-write visibility once PutIfAbsent returns nil.
//   - Delete is idempotent (deleting an absent key returns nil).
//
// List may return keys in any order; walship sorts keys itself.
type Store interface {
	PutIfAbsent(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// keyPattern restricts keys to lowercase ASCII slash-separated segments with
// no leading/trailing slashes, empty segments, or dot-prefixed entries (preventing
// path traversal and hiding DirStore's .tmp staging directory).
var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*$`)

// ValidateKey checks that key is a safe, normalized object-store key.
func ValidateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidateKey(strings.TrimSuffix(prefix, "/"))
}

const dirStoreTemp = ".tmp"

// DirStore implements Store over a local filesystem directory using fsynced
// temporary files and hard links (os.Link) for atomic PutIfAbsent.
type DirStore struct {
	Root string
}

// NewDirStore initializes a DirStore rooted at root, creating the root and its
// hidden .tmp staging directory if needed.
func NewDirStore(root string) (*DirStore, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(absolute, dirStoreTemp), 0o755); err != nil {
		return nil, err
	}
	return &DirStore{Root: absolute}, nil
}

func (s *DirStore) path(key string) string {
	return filepath.Join(s.Root, filepath.FromSlash(key))
}

// PutIfAbsent writes data to an fsynced temp file under .tmp, links it
// atomically to key's path (returning ErrExists if already present), unlinks
// the temp entry, and fsyncs the target directory.
func (s *DirStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	final := s.path(key)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	temp, err := s.writeTemp(data)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	// os.Link fails atomically with EEXIST if final already exists, unlike
	// os.Rename which silently overwrites an existing target on Unix.
	if err := os.Link(temp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, key)
		}
		return err
	}
	return syncDir(filepath.Dir(final))
}

func (s *DirStore) writeTemp(data []byte) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	name := filepath.Join(s.Root, dirStoreTemp, "put-"+hex.EncodeToString(suffix[:]))
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}

// Get reads and returns the complete bytes stored under key, or ErrNotFound.
func (s *DirStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return data, err
}

// List returns all valid keys starting with prefix in sorted lexical order,
// skipping dot-prefixed directories such as .tmp.
func (s *DirStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	var keys []string
	err := filepath.WalkDir(s.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != s.Root && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(s.Root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if strings.HasPrefix(key, prefix) && ValidateKey(key) == nil {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete removes key from the store and fsyncs its parent directory. Deleting
// a key that does not exist returns nil.
func (s *DirStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	err := os.Remove(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.path(key)))
}
```

#### 2. Self-contained object wire format (`format.go`)

Each snapshot and segment is encoded as one immutable object: a magic header line (`walship snapshot v1` or `walship segment v1`), one strict JSON metadata line (decoded with `DisallowUnknownFields()` and rejecting trailing values), and the raw binary body (either the full database image or raw WAL frames `FirstFrame..LastFrame` exactly as SQLite wrote them). Putting metadata and body in one object means a single `PutIfAbsent` publishes both atomically. Every snapshot records its `StateSHA256` (whole-image SHA-256) and `WALPosition`; every segment records `BeforeSHA256`, `AfterSHA256`, `SeedChecksum`, `LastChecksum`, and a `CommitPoint` (position, frame, pages, `user_version`, and state SHA-256) for every commit frame it contains.

<!-- include: ../internal/walship/format.go -->
```go
package walship

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/walfile"
)

// Object key prefixes and wire-format constants. Every snapshot and segment is
// stored as a single self-contained object (magic line, JSON metadata line,
// raw body) so one atomic PutIfAbsent publishes both metadata and payload.
const (
	// SnapshotPrefix is the key prefix for full-database snapshot objects.
	SnapshotPrefix = "snapshots/"
	// SegmentPrefix is the key prefix for WAL frame segment objects.
	SegmentPrefix  = "segments/"
	positionDigits = 12
	snapshotMagic  = "walship snapshot v1"
	segmentMagic   = "walship segment v1"
)

// Reasons recorded in SnapshotMeta.Reason explaining why a snapshot was taken.
const (
	// ReasonInitial marks the base snapshot taken when opening an empty store.
	ReasonInitial = "initial"
	// ReasonPeriodic marks a checkpointed snapshot taken every SnapshotEvery segments.
	ReasonPeriodic = "periodic"
	// ReasonDiscontinuity marks a re-base snapshot taken after an unvouched WAL change.
	ReasonDiscontinuity = "discontinuity"
)

// ErrMalformedObject reports a snapshot or segment object whose magic header,
// JSON metadata, key naming, body length/hash, or frame layout is invalid.
var ErrMalformedObject = errors.New("malformed walship object")

// Generation identifies one WAL generation by the hex encoding of its 32-byte
// WAL header (including its magic, checkpoint sequence, salts, and checksum).
// An empty Header means no WAL file (or a zero-byte WAL) existed.
type Generation struct {
	Header string `json:"header"`
}

// GenerationOf returns the Generation encoded from a 32-byte WAL header slice.
func GenerationOf(header []byte) Generation {
	return Generation{Header: hex.EncodeToString(header)}
}

// None reports whether g represents the absence of a WAL header.
func (g Generation) None() bool { return g.Header == "" }

// Bytes decodes g.Header and verifies that it is exactly walfile.HeaderSize (32) bytes.
func (g Generation) Bytes() ([]byte, error) {
	raw, err := hex.DecodeString(g.Header)
	if err != nil {
		return nil, err
	}
	if len(raw) != walfile.HeaderSize {
		return nil, fmt.Errorf("generation header is %d bytes, want %d", len(raw), walfile.HeaderSize)
	}
	return raw, nil
}

// Salts returns salt-1 and salt-2 from bytes 16-23 of the WAL header, or (0, 0)
// if g is empty or invalid.
func (g Generation) Salts() (uint32, uint32) {
	raw, err := g.Bytes()
	if err != nil {
		return 0, 0
	}
	return binary.BigEndian.Uint32(raw[16:]), binary.BigEndian.Uint32(raw[20:])
}

// String formats g's checkpoint sequence and random salts for diagnostics.
func (g Generation) String() string {
	if g.None() {
		return "no WAL header"
	}
	raw, err := g.Bytes()
	if err != nil {
		return "bad header"
	}
	salt1, salt2 := g.Salts()
	return fmt.Sprintf("checkpoint-seq=%d salt-1=0x%08x salt-2=0x%08x", binary.BigEndian.Uint32(raw[12:]), salt1, salt2)
}

// WALPosition records the exact cursor inside a WAL generation—header, 1-based
// frame number (0 for the header itself), and cumulative checksum at that frame—
// so the next segment can prove WAL continuity.
type WALPosition struct {
	Generation Generation `json:"generation"`
	Frame      int        `json:"frame"`
	Checksum   [2]uint32  `json:"checksum"`
}

// CommitPoint describes one committed transaction inside a SegmentMeta: its
// stream position, 1-based WAL commit frame, committed database size in pages,
// header user_version, and the SHA-256 of the complete checkpointed database
// image at that commit.
type CommitPoint struct {
	Position    int64  `json:"position"`
	Frame       int    `json:"frame"`
	Pages       uint32 `json:"pages"`
	UserVersion uint32 `json:"user_version"`
	StateSHA256 string `json:"state_sha256"`
}

// SnapshotMeta is the JSON metadata header stored ahead of a full raw database
// image in a snapshot object.
type SnapshotMeta struct {
	Position    int64       `json:"position"`
	Reason      string      `json:"reason"`
	PageSize    int         `json:"page_size"`
	Pages       uint32      `json:"pages"`
	UserVersion uint32      `json:"user_version"`
	StateSHA256 string      `json:"state_sha256"`
	WAL         WALPosition `json:"wal"`
	BodyBytes   int64       `json:"body_bytes"`
	BodySHA256  string      `json:"body_sha256"`
}

// SegmentMeta is the JSON metadata header stored ahead of raw WAL frames
// [FirstFrame..LastFrame] in a segment object. BeforeSHA256 and AfterSHA256
// chain consecutive segments (and their base snapshot) by full-image SHA-256.
type SegmentMeta struct {
	FirstPosition int64         `json:"first_position"`
	LastPosition  int64         `json:"last_position"`
	Generation    Generation    `json:"generation"`
	FirstFrame    int           `json:"first_frame"`
	LastFrame     int           `json:"last_frame"`
	SeedChecksum  [2]uint32     `json:"seed_checksum"`
	LastChecksum  [2]uint32     `json:"last_checksum"`
	PageSize      int           `json:"page_size"`
	BeforeSHA256  string        `json:"before_sha256"`
	AfterSHA256   string        `json:"after_sha256"`
	Commits       []CommitPoint `json:"commits"`
	BodyBytes     int64         `json:"body_bytes"`
	BodySHA256    string        `json:"body_sha256"`
}

// End returns the WALPosition at the final frame of m, used to check that the
// following segment continues cleanly.
func (m SegmentMeta) End() WALPosition {
	return WALPosition{Generation: m.Generation, Frame: m.LastFrame, Checksum: m.LastChecksum}
}

// Commit looks up the CommitPoint for stream position within m.
func (m SegmentMeta) Commit(position int64) (CommitPoint, bool) {
	for _, c := range m.Commits {
		if c.Position == position {
			return c, true
		}
	}
	return CommitPoint{}, false
}

// SnapshotKey returns the fixed-width key for the snapshot at position.
func SnapshotKey(position int64) string { return SnapshotPrefix + formatPosition(position) }

// SegmentKey returns the fixed-width key for the segment starting at firstPosition.
func SegmentKey(firstPosition int64) string { return SegmentPrefix + formatPosition(firstPosition) }

func formatPosition(position int64) string {
	return fmt.Sprintf("%0*d", positionDigits, position)
}

// ParseKey splits a snapshot or segment object key into its kind ("snapshot"
// or "segment") and 12-digit decimal position.
func ParseKey(key string) (kind string, position int64, err error) {
	var rest string
	switch {
	case strings.HasPrefix(key, SnapshotPrefix):
		kind, rest = "snapshot", strings.TrimPrefix(key, SnapshotPrefix)
	case strings.HasPrefix(key, SegmentPrefix):
		kind, rest = "segment", strings.TrimPrefix(key, SegmentPrefix)
	default:
		return "", 0, fmt.Errorf("%w: key %q is neither a snapshot nor a segment", ErrMalformedObject, key)
	}
	if len(rest) != positionDigits {
		return "", 0, fmt.Errorf("%w: key %q: position must be %d digits", ErrMalformedObject, key, positionDigits)
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return "", 0, fmt.Errorf("%w: key %q: position must be decimal digits", ErrMalformedObject, key)
		}
	}
	position, err = strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: key %q: %v", ErrMalformedObject, key, err)
	}
	return kind, position, nil
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encodeObject(magic string, meta any, body []byte) ([]byte, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(magic)
	out.WriteByte('\n')
	out.Write(encoded)
	out.WriteByte('\n')
	out.Write(body)
	return out.Bytes(), nil
}

func decodeObject(magic string, data []byte, meta any) ([]byte, error) {
	first := bytes.IndexByte(data, '\n')
	if first < 0 || string(data[:first]) != magic {
		return nil, fmt.Errorf("%w: first line is not %q", ErrMalformedObject, magic)
	}
	rest := data[first+1:]
	second := bytes.IndexByte(rest, '\n')
	if second < 0 {
		return nil, fmt.Errorf("%w: no metadata line", ErrMalformedObject)
	}
	// Reject unknown JSON fields and trailing JSON values so metadata lines
	// cannot smuggle extra state past validation.
	decoder := json.NewDecoder(bytes.NewReader(rest[:second]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(meta); err != nil {
		return nil, fmt.Errorf("%w: metadata: %v", ErrMalformedObject, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: metadata line holds more than one value", ErrMalformedObject)
	}
	return rest[second+1:], nil
}

func checkBody(body []byte, wantBytes int64, wantSHA256 string) error {
	if int64(len(body)) != wantBytes {
		return fmt.Errorf("%w: body is %d bytes, metadata says %d", ErrMalformedObject, len(body), wantBytes)
	}
	if SHA256Hex(body) != wantSHA256 {
		return fmt.Errorf("%w: body sha256 does not match its metadata", ErrMalformedObject)
	}
	return nil
}

// EncodeSnapshot populates BodyBytes and BodySHA256 on meta and serializes the
// snapshot header and raw database image into one object.
func EncodeSnapshot(meta SnapshotMeta, image []byte) ([]byte, error) {
	meta.BodyBytes = int64(len(image))
	meta.BodySHA256 = SHA256Hex(image)
	return encodeObject(snapshotMagic, meta, image)
}

// DecodeSnapshot parses and validates a snapshot object, checking its magic
// line, JSON metadata, body length and SHA-256, power-of-two page size, page
// count, WAL generation header length, and StateSHA256.
func DecodeSnapshot(data []byte) (SnapshotMeta, []byte, error) {
	var meta SnapshotMeta
	body, err := decodeObject(snapshotMagic, data, &meta)
	if err != nil {
		return meta, nil, err
	}
	if err := checkBody(body, meta.BodyBytes, meta.BodySHA256); err != nil {
		return meta, nil, err
	}
	if meta.PageSize < walfile.MinPageSize || meta.PageSize > walfile.MaxPageSize || meta.PageSize&(meta.PageSize-1) != 0 {
		return meta, nil, fmt.Errorf("%w: snapshot page size %d", ErrMalformedObject, meta.PageSize)
	}
	if !meta.WAL.Generation.None() {
		if _, err := meta.WAL.Generation.Bytes(); err != nil {
			return meta, nil, fmt.Errorf("%w: snapshot WAL generation: %v", ErrMalformedObject, err)
		}
	}
	if meta.Pages == 0 || len(body)%meta.PageSize != 0 || uint32(len(body)/meta.PageSize) != meta.Pages {
		return meta, nil, fmt.Errorf("%w: snapshot body of %d bytes is not %d pages of %d bytes", ErrMalformedObject, len(body), meta.Pages, meta.PageSize)
	}
	if SHA256Hex(body) != meta.StateSHA256 {
		return meta, nil, fmt.Errorf("%w: snapshot state hash does not match its image", ErrMalformedObject)
	}
	return meta, body, nil
}

// EncodeSegment populates BodyBytes and BodySHA256 on meta and serializes the
// segment header and raw WAL frame bytes into one object.
func EncodeSegment(meta SegmentMeta, frames []byte) ([]byte, error) {
	meta.BodyBytes = int64(len(frames))
	meta.BodySHA256 = SHA256Hex(frames)
	return encodeObject(segmentMagic, meta, frames)
}

// DecodeSegment parses and validates a segment object's wire format, body hash,
// and metadata invariants (frame count, consecutive commit positions, and final
// commit hash). Call VerifySegmentFrames as well to check WAL header validity,
// commit-frame alignment, and SQLite's cumulative checksum chain.
func DecodeSegment(data []byte) (SegmentMeta, []byte, error) {
	var meta SegmentMeta
	body, err := decodeObject(segmentMagic, data, &meta)
	if err != nil {
		return meta, nil, err
	}
	if err := checkBody(body, meta.BodyBytes, meta.BodySHA256); err != nil {
		return meta, nil, err
	}
	if err := meta.validate(len(body)); err != nil {
		return meta, nil, err
	}
	return meta, body, nil
}

func (m SegmentMeta) validate(bodyBytes int) error {
	if _, err := m.Generation.Bytes(); err != nil {
		return fmt.Errorf("%w: segment generation: %v", ErrMalformedObject, err)
	}
	if m.PageSize < walfile.MinPageSize || m.PageSize > walfile.MaxPageSize {
		return fmt.Errorf("%w: page size %d", ErrMalformedObject, m.PageSize)
	}
	if m.FirstFrame < 1 || m.LastFrame < m.FirstFrame {
		return fmt.Errorf("%w: frames %d-%d", ErrMalformedObject, m.FirstFrame, m.LastFrame)
	}
	frameSize := m.PageSize + walfile.FrameHeaderSize
	if bodyBytes != (m.LastFrame-m.FirstFrame+1)*frameSize {
		return fmt.Errorf("%w: body of %d bytes does not hold frames %d-%d", ErrMalformedObject, bodyBytes, m.FirstFrame, m.LastFrame)
	}
	if len(m.Commits) == 0 || m.LastPosition-m.FirstPosition+1 != int64(len(m.Commits)) {
		return fmt.Errorf("%w: positions %d-%d with %d commit points", ErrMalformedObject, m.FirstPosition, m.LastPosition, len(m.Commits))
	}
	previousFrame := m.FirstFrame - 1
	for i, c := range m.Commits {
		if c.Position != m.FirstPosition+int64(i) {
			return fmt.Errorf("%w: commit point %d has position %d", ErrMalformedObject, i, c.Position)
		}
		if c.Frame <= previousFrame || c.Frame > m.LastFrame || c.Pages == 0 {
			return fmt.Errorf("%w: commit point at position %d has frame %d, pages %d", ErrMalformedObject, c.Position, c.Frame, c.Pages)
		}
		previousFrame = c.Frame
	}
	if previousFrame != m.LastFrame {
		return fmt.Errorf("%w: the last frame %d is not a commit frame", ErrMalformedObject, m.LastFrame)
	}
	if m.Commits[len(m.Commits)-1].StateSHA256 != m.AfterSHA256 {
		return fmt.Errorf("%w: after hash differs from the last commit point", ErrMalformedObject)
	}
	return nil
}

func short(digest string) string {
	if len(digest) > 16 {
		return digest[:16]
	}
	return digest
}

// userVersion reads SQLite's PRAGMA user_version from database header bytes
// 60-63 of page 1.
func userVersion(image []byte) uint32 {
	if len(image) < 64 {
		return 0
	}
	return binary.BigEndian.Uint32(image[60:64])
}
```

#### 3. Frame verification and replay (`apply.go`)

Before any page is ever applied during restore, `VerifySegmentFrames` validates the segment's 32-byte WAL generation header with `walfile.Parse` (magic, format version `3007000`, page size matching `meta.PageSize`, header checksum), verifies every frame's salts and non-zero page number, checks a one-to-one correspondence between non-zero frame commit sizes (frame header bytes `4–7`) and `meta.Commits`, and walks SQLite's cumulative WAL checksum chain from `SeedChecksum` to `LastChecksum`. Because a segment starting at `FirstFrame > 1` originally chained its checksums from an earlier frame rather than from the WAL header, `StandaloneWAL` re-chains frames `FirstFrame..throughFrame` from the header checksum so Chapter 5's `walfile.Replay` can validate and replay the segment cleanly in `ApplySegment`.

<!-- include: ../internal/walship/apply.go -->
```go
package walship

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/nickstrad/walfleet/internal/walfile"
)

// ErrChecksumChain reports that a segment's raw WAL frames do not match its
// generation salts or do not continue SQLite's cumulative WAL checksum chain
// from SeedChecksum to LastChecksum.
var ErrChecksumChain = errors.New("segment frames do not continue the WAL checksum chain")

// VerifySegmentFrames validates a segment before any page is replayed: it parses
// the 32-byte WAL generation header (magic, format version 3007000, page size,
// header checksum), matches every frame's salts and page number, verifies a
// one-to-one match between non-zero frame commit sizes and meta.Commits, and
// walks SQLite's cumulative checksum chain (walDecodeFrame, src/wal.c:1390-1614)
// from meta.SeedChecksum to meta.LastChecksum.
func VerifySegmentFrames(meta SegmentMeta, frames []byte) error {
	header, err := meta.Generation.Bytes()
	if err != nil {
		return err
	}
	parsed, err := walfile.Parse(header)
	if err != nil {
		return fmt.Errorf("%w: segment WAL header: %v", ErrMalformedObject, err)
	}
	if parsed.PageSize() != meta.PageSize {
		return fmt.Errorf("%w: segment WAL header page size %d, metadata page size %d", ErrMalformedObject, parsed.PageSize(), meta.PageSize)
	}
	bigEndian := parsed.Header.BigEndianChecksums()
	frameSize := meta.PageSize + walfile.FrameHeaderSize
	if len(frames) != (meta.LastFrame-meta.FirstFrame+1)*frameSize {
		return fmt.Errorf("%w: %d bytes for frames %d-%d", ErrMalformedObject, len(frames), meta.FirstFrame, meta.LastFrame)
	}
	chain := walfile.Checksum(meta.SeedChecksum)
	commit := 0
	for offset, number := 0, meta.FirstFrame; offset < len(frames); offset, number = offset+frameSize, number+1 {
		frame := frames[offset : offset+frameSize]
		if string(frame[8:16]) != string(header[16:24]) {
			return fmt.Errorf("%w: frame %d carries other salts than its generation", ErrChecksumChain, number)
		}
		if binary.BigEndian.Uint32(frame[0:]) == 0 {
			return fmt.Errorf("%w: frame %d has page number 0", ErrChecksumChain, number)
		}
		listed := commit < len(meta.Commits) && meta.Commits[commit].Frame == number
		// Require every frame with a non-zero commit-size field (frame header
		// bytes 4-7) to match the next listed CommitPoint, and forbid listing a
		// non-commit frame as a restore point.
		if commitSize := binary.BigEndian.Uint32(frame[4:]); commitSize != 0 || listed {
			if !listed || meta.Commits[commit].Pages != commitSize {
				return fmt.Errorf("%w: frame %d has commit size %d, which the segment's commit points do not list", ErrMalformedObject, number, commitSize)
			}
			commit++
		}
		next, err := walfile.ChecksumBytes(bigEndian, frame[:8], chain)
		if err != nil {
			return err
		}
		next, err = walfile.ChecksumBytes(bigEndian, frame[walfile.FrameHeaderSize:], next)
		if err != nil {
			return err
		}
		stored := walfile.Checksum{binary.BigEndian.Uint32(frame[16:]), binary.BigEndian.Uint32(frame[20:])}
		if next != stored {
			return fmt.Errorf("%w: frame %d stores a checksum the chain does not give", ErrChecksumChain, number)
		}
		chain = next
	}
	if commit != len(meta.Commits) {
		return fmt.Errorf("%w: %d commit points, %d commit frames", ErrMalformedObject, len(meta.Commits), commit)
	}
	if chain != walfile.Checksum(meta.LastChecksum) {
		return fmt.Errorf("%w: the chain does not end at the checksum the metadata records", ErrChecksumChain)
	}
	return nil
}

// StandaloneWAL packages frames [meta.FirstFrame..throughFrame] with the
// segment's 32-byte WAL header into a self-contained WAL file whose cumulative
// frame checksums are re-chained starting from the header checksum. Re-chaining
// is required because a segment starting at FirstFrame > 1 originally chained
// from an earlier frame's checksum and would not validate on its own in
// walfile.Parse or walfile.Replay.
func StandaloneWAL(meta SegmentMeta, frames []byte, throughFrame int) ([]byte, error) {
	if throughFrame < meta.FirstFrame || throughFrame > meta.LastFrame {
		return nil, fmt.Errorf("frame %d is outside the segment's frames %d-%d", throughFrame, meta.FirstFrame, meta.LastFrame)
	}
	header, err := meta.Generation.Bytes()
	if err != nil {
		return nil, err
	}
	bigEndian := binary.BigEndian.Uint32(header[0:])&1 == 1
	frameSize := meta.PageSize + walfile.FrameHeaderSize
	count := throughFrame - meta.FirstFrame + 1
	if len(frames) < count*frameSize {
		return nil, fmt.Errorf("%w: segment body too short for frame %d", ErrMalformedObject, throughFrame)
	}
	out := make([]byte, walfile.HeaderSize+count*frameSize)
	copy(out, header)
	copy(out[walfile.HeaderSize:], frames[:count*frameSize])
	chain := walfile.Checksum{binary.BigEndian.Uint32(header[24:]), binary.BigEndian.Uint32(header[28:])}
	for i := 0; i < count; i++ {
		frame := out[walfile.HeaderSize+i*frameSize : walfile.HeaderSize+(i+1)*frameSize]
		next, err := walfile.ChecksumBytes(bigEndian, frame[:8], chain)
		if err != nil {
			return nil, err
		}
		next, err = walfile.ChecksumBytes(bigEndian, frame[walfile.FrameHeaderSize:], next)
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(frame[16:], next[0])
		binary.BigEndian.PutUint32(frame[20:], next[1])
		chain = next
	}
	return out, nil
}

// ApplySegment verifies meta and frames, builds a standalone WAL through
// throughFrame, and replays it onto image using Chapter 5's walfile.Replay.
// It fails unless every frame in [meta.FirstFrame..throughFrame] is valid and
// throughFrame is a commit frame.
func ApplySegment(image []byte, meta SegmentMeta, frames []byte, throughFrame int) ([]byte, walfile.ReplayReport, error) {
	if err := VerifySegmentFrames(meta, frames); err != nil {
		return nil, walfile.ReplayReport{}, err
	}
	wal, err := StandaloneWAL(meta, frames, throughFrame)
	if err != nil {
		return nil, walfile.ReplayReport{}, err
	}
	out, report, err := walfile.Replay(image, wal)
	if err != nil {
		return nil, report, err
	}
	want := throughFrame - meta.FirstFrame + 1
	if report.ValidFrames != want || report.LastCommitFrame != want {
		return nil, report, fmt.Errorf("replay of frames %d-%d found %d valid frames, last commit at %d; want %d and %d", meta.FirstFrame, throughFrame, report.ValidFrames, report.LastCommitFrame, want, want)
	}
	return out, report, nil
}
```

#### 4. Catalog, restore planning, execution, and pruning (`catalog.go`)

`LoadCatalog` fetches every snapshot and segment, verifies key names against metadata positions, and runs `VerifySegmentFrames` on every segment. Any object that fails validation is quarantined in `Catalog.Damaged` instead of aborting the load, so an intact older snapshot can still serve restores even if a later periodic snapshot is corrupted (verified by adversarial test `SHIP-03e`). `Catalog.Plan(target)` picks the newest usable snapshot `<= target` and delegates to `PlanFrom`, which checks every segment's `BeforeSHA256`, WAL generation, frame continuity, and seed checksum **before** `Plan.Execute` touches any page. Because positions exist only at `CommitPoint` entries, restoring to the middle of a transaction is impossible by construction. `Prune(ctx, store, keepFrom)` keeps the newest snapshot `<= keepFrom` and everything after it, returning only the keys actually deleted before any error.

<!-- include: ../internal/walship/catalog.go -->
```go
package walship

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

// Sentinel errors returned while planning, executing, or pruning restores.
var (
	// ErrNoSnapshot reports that no usable snapshot exists at or before the target position.
	ErrNoSnapshot = errors.New("no usable snapshot at or before the target")
	// ErrBrokenChain reports a missing, damaged, or non-contiguous segment or a post-replay state-hash mismatch.
	ErrBrokenChain = errors.New("segment chain is broken")
	// ErrNoCommit reports that a target position does not name any commit point inside its covering segment.
	ErrNoCommit = errors.New("no commit point at that position")
)

// Snapshot is a validated snapshot object loaded from a Store.
type Snapshot struct {
	Key   string
	Meta  SnapshotMeta
	Image []byte
}

// Segment is a validated WAL segment object loaded from a Store.
type Segment struct {
	Key    string
	Meta   SegmentMeta
	Frames []byte
}

// Damaged records an object in a Store that failed key parsing, decoding, body
// hashing, or WAL checksum-chain verification during LoadCatalog.
type Damaged struct {
	Key     string
	Kind    string
	Problem string
}

// Catalog holds all usable snapshots and segments loaded from a Store, sorted
// by position, alongside any objects quarantined as Damaged.
type Catalog struct {
	Snapshots []Snapshot
	Segments  []Segment
	Damaged   []Damaged
}

// LoadCatalog lists, fetches, decodes, and verifies every snapshot and segment
// object in store. Objects that fail key/metadata/body/checksum checks are
// recorded in Catalog.Damaged instead of aborting the load, so undamaged
// snapshots and segments remain usable for planning and diagnostics.
func LoadCatalog(ctx context.Context, store Store) (*Catalog, error) {
	catalog := &Catalog{}
	for _, prefix := range []string{SnapshotPrefix, SegmentPrefix} {
		keys, err := store.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		// Sort keys explicitly; Store.List makes no ordering guarantee.
		sort.Strings(keys)
		for _, key := range keys {
			data, err := store.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			catalog.add(key, data)
		}
	}
	sort.Slice(catalog.Snapshots, func(i, j int) bool { return catalog.Snapshots[i].Meta.Position < catalog.Snapshots[j].Meta.Position })
	sort.Slice(catalog.Segments, func(i, j int) bool {
		return catalog.Segments[i].Meta.FirstPosition < catalog.Segments[j].Meta.FirstPosition
	})
	return catalog, nil
}

func (c *Catalog) add(key string, data []byte) {
	kind, position, err := ParseKey(key)
	if err != nil {
		c.Damaged = append(c.Damaged, Damaged{Key: key, Kind: "unknown", Problem: err.Error()})
		return
	}
	switch kind {
	case "snapshot":
		meta, image, err := DecodeSnapshot(data)
		if err == nil && meta.Position != position {
			err = fmt.Errorf("%w: key names position %d, metadata says %d", ErrMalformedObject, position, meta.Position)
		}
		if err != nil {
			c.Damaged = append(c.Damaged, Damaged{Key: key, Kind: kind, Problem: err.Error()})
			return
		}
		c.Snapshots = append(c.Snapshots, Snapshot{Key: key, Meta: meta, Image: image})
	case "segment":
		meta, frames, err := DecodeSegment(data)
		if err == nil && meta.FirstPosition != position {
			err = fmt.Errorf("%w: key names first position %d, metadata says %d", ErrMalformedObject, position, meta.FirstPosition)
		}
		// Verify the WAL header, commit frames, and checksum chain up front so
		// damaged segments are rejected during LoadCatalog and Plan before any
		// page is applied.
		if err == nil {
			err = VerifySegmentFrames(meta, frames)
		}
		if err != nil {
			c.Damaged = append(c.Damaged, Damaged{Key: key, Kind: kind, Problem: err.Error()})
			return
		}
		c.Segments = append(c.Segments, Segment{Key: key, Meta: meta, Frames: frames})
	}
}

// LastPosition returns the highest usable stream position present in c (across
// both snapshots and segments), or (0, false) if c has no usable objects.
func (c *Catalog) LastPosition() (int64, bool) {
	last, found := int64(0), false
	for _, s := range c.Snapshots {
		if !found || s.Meta.Position > last {
			last, found = s.Meta.Position, true
		}
	}
	for _, s := range c.Segments {
		if !found || s.Meta.LastPosition > last {
			last, found = s.Meta.LastPosition, true
		}
	}
	return last, found
}

func (c *Catalog) damagedAt(kind string, position int64) (Damaged, bool) {
	for _, d := range c.Damaged {
		k, p, err := ParseKey(d.Key)
		if err == nil && k == kind && p == position {
			return d, true
		}
	}
	return Damaged{}, false
}

func (c *Catalog) segmentStartingAt(position int64) (Segment, error) {
	for _, s := range c.Segments {
		if s.Meta.FirstPosition == position {
			return s, nil
		}
	}
	if d, ok := c.damagedAt("segment", position); ok {
		return Segment{}, fmt.Errorf("%w: the segment starting at position %d is damaged: %s", ErrBrokenChain, position, d.Problem)
	}
	return Segment{}, fmt.Errorf("%w: no segment starts at position %d", ErrBrokenChain, position)
}

// Plan is a verified restore plan from one base Snapshot through zero or more
// contiguous Segments up to Target's CommitPoint.
type Plan struct {
	Target   int64
	Snapshot Snapshot
	Segments []Segment
	Commit   CommitPoint
}

// Commits returns the number of shipped commits between p.Snapshot and p.Target.
func (p Plan) Commits() int64 { return p.Target - p.Snapshot.Meta.Position }

// PlanFrom builds a restore Plan starting at the specific snapshot at
// snapshotPosition and advancing through contiguous segments to target. Every
// link's BeforeSHA256, WAL generation, frame number, and seed checksum are
// verified before Plan.Execute applies any page.
func (c *Catalog) PlanFrom(snapshotPosition, target int64) (Plan, error) {
	var snapshot *Snapshot
	for i := range c.Snapshots {
		if c.Snapshots[i].Meta.Position == snapshotPosition {
			snapshot = &c.Snapshots[i]
		}
	}
	if snapshot == nil {
		if d, ok := c.damagedAt("snapshot", snapshotPosition); ok {
			return Plan{}, fmt.Errorf("%w: the snapshot at position %d is damaged: %s", ErrNoSnapshot, snapshotPosition, d.Problem)
		}
		return Plan{}, fmt.Errorf("%w: no snapshot at position %d", ErrNoSnapshot, snapshotPosition)
	}
	if snapshot.Meta.Position > target {
		return Plan{}, fmt.Errorf("%w: the snapshot at position %d is after target %d", ErrNoSnapshot, snapshot.Meta.Position, target)
	}
	plan := Plan{Target: target, Snapshot: *snapshot, Commit: CommitPoint{
		Position: snapshot.Meta.Position, Frame: snapshot.Meta.WAL.Frame, Pages: snapshot.Meta.Pages,
		UserVersion: snapshot.Meta.UserVersion, StateSHA256: snapshot.Meta.StateSHA256,
	}}
	position, state, end := snapshot.Meta.Position, snapshot.Meta.StateSHA256, snapshot.Meta.WAL
	for position < target {
		segment, err := c.segmentStartingAt(position + 1)
		if err != nil {
			return Plan{}, err
		}
		if err := continues(end, state, segment.Meta); err != nil {
			return Plan{}, fmt.Errorf("%w: segment %s: %v", ErrBrokenChain, segment.Key, err)
		}
		plan.Segments = append(plan.Segments, segment)
		if segment.Meta.LastPosition >= target {
			commit, ok := segment.Meta.Commit(target)
			if !ok {
				return Plan{}, fmt.Errorf("%w: %d in segment %s", ErrNoCommit, target, segment.Key)
			}
			plan.Commit = commit
			return plan, nil
		}
		position, state, end = segment.Meta.LastPosition, segment.Meta.AfterSHA256, segment.Meta.End()
	}
	return plan, nil
}

// continues verifies that next chains cleanly from the previous state hash and
// WAL position: within the same generation it must start at Frame+1 with
// SeedChecksum == end.Checksum; in a new generation it must start at frame 1
// seeded from the new 32-byte WAL header's checksum.
func continues(end WALPosition, state string, next SegmentMeta) error {
	if next.BeforeSHA256 != state {
		return fmt.Errorf("its before hash %s is not the previous state %s", short(next.BeforeSHA256), short(state))
	}
	if next.Generation == end.Generation {
		if next.FirstFrame != end.Frame+1 {
			return fmt.Errorf("it starts at frame %d of the same WAL generation, want %d", next.FirstFrame, end.Frame+1)
		}
		if next.SeedChecksum != end.Checksum {
			return fmt.Errorf("its seed checksum does not continue the previous frame's checksum")
		}
		return nil
	}
	if next.FirstFrame != 1 {
		return fmt.Errorf("it starts a new WAL generation at frame %d, want 1", next.FirstFrame)
	}
	header, err := next.Generation.Bytes()
	if err != nil {
		return err
	}
	if seed := [2]uint32{be32(header[24:]), be32(header[28:])}; next.SeedChecksum != seed {
		return fmt.Errorf("it starts a new WAL generation but its seed is not the header checksum")
	}
	return nil
}

// Plan chooses the newest usable snapshot at or before target and returns
// PlanFrom that snapshot to target, minimizing the number of segments replayed.
func (c *Catalog) Plan(target int64) (Plan, error) {
	for i := len(c.Snapshots) - 1; i >= 0; i-- {
		if c.Snapshots[i].Meta.Position <= target {
			return c.PlanFrom(c.Snapshots[i].Meta.Position, target)
		}
	}
	return Plan{}, fmt.Errorf("%w: target %d", ErrNoSnapshot, target)
}

// RestoreReport summarizes the segments and frames replayed by Plan.Execute and
// the final restored database image's SHA-256.
type RestoreReport struct {
	Plan            Plan
	SegmentsApplied int
	FramesApplied   int
	StateSHA256     string
}

// Execute clones p.Snapshot.Image, applies each planned segment in order through
// p.Commit.Frame using ApplySegment, and verifies the whole-image SHA-256 after
// every segment and at the target commit point.
func (p Plan) Execute() ([]byte, RestoreReport, error) {
	image := append([]byte(nil), p.Snapshot.Image...)
	report := RestoreReport{Plan: p}
	for _, segment := range p.Segments {
		through := segment.Meta.LastFrame
		want := segment.Meta.AfterSHA256
		if segment.Meta.LastPosition > p.Target {
			through, want = p.Commit.Frame, p.Commit.StateSHA256
		}
		out, replay, err := ApplySegment(image, segment.Meta, segment.Frames, through)
		if err != nil {
			return nil, report, fmt.Errorf("segment %s: %w", segment.Key, err)
		}
		if got := SHA256Hex(out); got != want {
			return nil, report, fmt.Errorf("%w: after segment %s the state hash is %s, want %s", ErrBrokenChain, segment.Key, short(got), short(want))
		}
		image = out
		report.SegmentsApplied++
		report.FramesApplied += replay.AppliedFrames
	}
	report.StateSHA256 = SHA256Hex(image)
	if report.StateSHA256 != p.Commit.StateSHA256 {
		return nil, report, fmt.Errorf("%w: restored state %s, the commit point says %s", ErrBrokenChain, short(report.StateSHA256), short(p.Commit.StateSHA256))
	}
	return image, report, nil
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// Prune deletes snapshots and segments that are no longer needed to restore any
// position at or after keepFrom. It keeps the newest usable snapshot at or
// before keepFrom and everything after it, returning only the keys actually
// deleted before any error.
func Prune(ctx context.Context, store Store, keepFrom int64) ([]string, error) {
	catalog, err := LoadCatalog(ctx, store)
	if err != nil {
		return nil, err
	}
	base := int64(-1)
	for _, s := range catalog.Snapshots {
		if s.Meta.Position <= keepFrom {
			base = s.Meta.Position
		}
	}
	if base < 0 {
		return nil, fmt.Errorf("%w: nothing to keep restores from at or before %d", ErrNoSnapshot, keepFrom)
	}
	var deleted []string
	for _, s := range catalog.Snapshots {
		if s.Meta.Position < base {
			deleted = append(deleted, s.Key)
		}
	}
	for _, s := range catalog.Segments {
		if s.Meta.LastPosition <= base {
			deleted = append(deleted, s.Key)
		}
	}
	for i, key := range deleted {
		if err := store.Delete(ctx, key); err != nil {
			return deleted[:i], err
		}
	}
	return deleted, nil
}

// WriteDatabase writes a restored database image to path and fsyncs it. It
// refuses to overwrite an existing database file or write beside an existing
// -wal, -shm, or -journal sidecar that SQLite would apply on open.
func WriteDatabase(path string, image []byte) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("%w: %s", ErrExists, candidate)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(image)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

// CopyStore copies every object from from into to using PutIfAbsent in sorted
// key order, returning the number of objects copied.
func CopyStore(ctx context.Context, from, to Store) (int, error) {
	keys, err := from.List(ctx, "")
	if err != nil {
		return 0, err
	}
	sort.Strings(keys)
	for i, key := range keys {
		data, err := from.Get(ctx, key)
		if err != nil {
			return i, err
		}
		if err := to.PutIfAbsent(ctx, key, data); err != nil {
			return i, err
		}
	}
	return len(keys), nil
}
```

#### 5. The continuous shipper (`shipper.go`)

`Shipper` owns two connections to the WAL database (`wal_autocheckpoint=0`): **`control`** and **`pin`**.

- **Between cycles**, `pin` holds an open read transaction (`BEGIN` + `SELECT count(*) FROM sqlite_schema`). By `src/wal.c:2233-2260`, a read mark on slot `1–4` caps any checkpoint at `mxSafeFrame` (which is at or below the last frame the shipper already shipped), while a read mark on slot `0` (acquired when the WAL was fully backfilled) blocks any backfill because backfilling requires an exclusive lock on slot `0` (`wal.c:2259-2260`). A writer may restart the WAL after a full backfill (`walRestartLog`, `wal.c:3875-3915`), and that restart loses nothing because every old frame was already shipped and backfilled.
- **During each `Cycle`**, `control` acquires `BEGIN IMMEDIATE` so no commit can race the shipper while it reads `-wal` via `os.ReadFile` (POSIX-lock-safe because SQLite puts advisory locks on `.db` and `-shm`, never on `-wal`—verified by `TestReadingTheWALInAProcessWithAConnectionKeepsItsLocks`).
- **Discontinuity detection (`classify`):** Because SQLite's `nCkpt` and `salt-1` are per-connection fields (`src/wal.c:2152-2166, 4095-4096`), a changed header is accepted as `WALNewGeneration` **only** when the shipper held its pin continuously (`s.continuous == true`) and its last checkpoint backfilled every shipped frame. On resume (`!s.continuous`), if the WAL header changed—or if there is no WAL now and none at the last shipped position, yet `ReadPages` does not hash to `s.state` (offline commits checkpointed away while down)—`classify` returns `WALDiscontinuity` and `Cycle` re-bases with a full snapshot.
- **Resume safety and mirror verification:** On `Open`, `resume` refuses with `ErrBrokenChain` if any damaged object in the store has a key position past `LastPosition()`, preventing a silent rewind over a corrupted tail. (Note one narrow boundary: if a store contains *only* damaged objects—zero usable snapshots or segments—and `Open` is called with a different `Origin`, `start` takes the fresh-store branch; with the same `Origin`, `PutIfAbsent` still fails closed with `ErrExists`.) On every periodic snapshot (`SnapshotEvery` segments), `snapshot` hashes SQLite's live pages from `sqlite_dbpage` against the shipper's locally replayed `s.state` and aborts with `ErrMirrorDiverged` if they ever disagree.

<!-- include: ../internal/walship/shipper.go -->
```go
package walship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
)

// Sentinel errors returned by Shipper operations.
var (
	// ErrMirrorDiverged reports that a periodic snapshot read from SQLite via
	// sqlite_dbpage does not hash to the shipper's locally replayed image.
	ErrMirrorDiverged = errors.New("the shipper's replayed state differs from SQLite's pages")
	// ErrNotWAL reports that Open was called for a non-WAL database configuration.
	ErrNotWAL = errors.New("walship needs a database in WAL mode")
)

// Config configures a Shipper: the target WAL database path, object Store,
// SQLite settings, periodic snapshot cadence (SnapshotEvery segments; 0
// disables periodic snapshots), and initial stream position (Origin).
type Config struct {
	Database      string
	Store         Store
	Options       sqlitex.Options
	SnapshotEvery int
	Origin        int64
}

// Shipper continuously captures committed WAL frames from a live database into
// a Store while owning checkpoints. It holds two SQLite connections: control
// (which takes BEGIN IMMEDIATE during each Cycle and reads snapshots via
// sqlite_dbpage) and pin (which holds a read transaction between cycles so no
// outside checkpoint can copy or recycle unshipped frames).
type Shipper struct {
	config          Config
	db              *sqlitex.DB
	control         *sql.Conn
	pin             *sql.Conn
	pinned          bool
	pageSize        int
	position        int64
	wal             WALPosition
	image           []byte
	state           string
	sinceSnapshot   int
	continuous      bool
	fullyBackfilled bool
	resumed         bool
	openSnapshot    *SnapshotMeta
	broken          error
}

// WALChange classifies how the on-disk -wal file relates to the shipper's last
// shipped WALPosition at the start of a Cycle.
type WALChange int

// Possible WAL transitions observed by Shipper.classify.
const (
	// WALUnchanged means the -wal keeps the same header and shipped prefix.
	WALUnchanged WALChange = iota
	// WALNewGeneration means the -wal restarted cleanly after the shipper's own
	// full checkpoint while the shipper held its read mark continuously.
	WALNewGeneration
	// WALDiscontinuity means continuity cannot be proven (for example, an
	// outside checkpoint or offline commit), forcing a new snapshot.
	WALDiscontinuity
)

// String returns a human-readable description of c for cycle logs and evidence.
func (c WALChange) String() string {
	switch c {
	case WALUnchanged:
		return "same WAL generation"
	case WALNewGeneration:
		return "new WAL generation after the shipper's full checkpoint"
	case WALDiscontinuity:
		return "discontinuity"
	}
	return fmt.Sprintf("WALChange(%d)", int(c))
}

// CycleReport summarizes what one Shipper.Cycle observed and published.
type CycleReport struct {
	Change       WALChange
	Reason       string
	Segment      *SegmentMeta
	Snapshot     *SnapshotMeta
	Checkpoint   sqlitex.CheckpointResult
	FullBackfill bool
}

// Lines formats r into deterministic, salt-free lines using name to label WAL
// generations in order of appearance.
func (r CycleReport) Lines(name func(Generation) string) []string {
	lines := []string{"wal: " + r.Change.String()}
	if r.Reason != "" {
		lines[0] += ": " + r.Reason
	}
	if r.Segment != nil {
		lines = append(lines, DescribeSegment(*r.Segment, name))
	} else {
		lines = append(lines, "segment: none")
	}
	lines = append(lines, fmt.Sprintf("checkpoint PASSIVE: %s; every shipped frame copied: %t", r.Checkpoint, r.FullBackfill))
	if r.Snapshot != nil {
		lines = append(lines, DescribeSnapshot(*r.Snapshot, name))
	}
	return lines
}

// DescribeSegment formats m as a single summary line without exposing random WAL salts.
func DescribeSegment(m SegmentMeta, name func(Generation) string) string {
	return fmt.Sprintf("segment %s: positions %d-%d, frames %d-%d of %s, user_version %d-%d, before %s after %s",
		SegmentKey(m.FirstPosition), m.FirstPosition, m.LastPosition, m.FirstFrame, m.LastFrame, name(m.Generation),
		m.Commits[0].UserVersion, m.Commits[len(m.Commits)-1].UserVersion, short(m.BeforeSHA256), short(m.AfterSHA256))
}

// DescribeSnapshot formats m as a single summary line without exposing random WAL salts.
func DescribeSnapshot(m SnapshotMeta, name func(Generation) string) string {
	return fmt.Sprintf("snapshot %s (%s): position %d, %d pages, user_version %d, state %s, after frame %d of %s",
		SnapshotKey(m.Position), m.Reason, m.Position, m.Pages, m.UserVersion, short(m.StateSHA256), m.WAL.Frame, name(m.WAL.Generation))
}

// GenerationNames maps random WAL Generation headers to stable sequential
// labels ("WAL generation #1", "#2", ...) so EXACT experiment output does not
// depend on SQLite's random salts.
type GenerationNames struct {
	seen map[Generation]int
}

// Name returns the stable label for gen, assigning the next number on first sight.
func (g *GenerationNames) Name(gen Generation) string {
	if gen.None() {
		return "no WAL"
	}
	if g.seen == nil {
		g.seen = map[Generation]int{}
	}
	if _, ok := g.seen[gen]; !ok {
		g.seen[gen] = len(g.seen) + 1
	}
	return fmt.Sprintf("WAL generation #%d", g.seen[gen])
}

// Open starts a Shipper on config.Database against config.Store. If the store
// is empty, Open takes an initial snapshot at config.Origin under BEGIN
// IMMEDIATE and establishes its read pin; if the store already holds objects,
// Open resumes by replaying to the newest position (refusing damaged objects
// past that position) and marks continuity false until the first Cycle verifies
// the live WAL.
func Open(ctx context.Context, config Config) (*Shipper, error) {
	if config.Store == nil || config.Database == "" {
		return nil, errors.New("walship: Database and Store are required")
	}
	if config.SnapshotEvery < 0 {
		return nil, errors.New("walship: SnapshotEvery must not be negative")
	}
	if !strings.EqualFold(config.Options.File.JournalMode, sqlitex.JournalWAL) {
		return nil, fmt.Errorf("%w: journal mode %q", ErrNotWAL, config.Options.File.JournalMode)
	}
	// The shipper needs two pooled connections (control + pin), explicit
	// transaction control, and automatic checkpoints disabled so only the
	// shipper decides when frames are backfilled.
	options := config.Options
	options.SingleConnection = false
	options.ImmediateTx = false
	options.ReadOnly = false
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, config.Database, options)
	if err != nil {
		return nil, err
	}
	s := &Shipper{config: config, db: db}
	if err := s.start(ctx); err != nil {
		return nil, errors.Join(err, s.closeConnections())
	}
	return s, nil
}

func (s *Shipper) start(ctx context.Context) error {
	var err error
	if s.control, err = s.db.Conn(ctx); err != nil {
		return err
	}
	if s.pin, err = s.db.Conn(ctx); err != nil {
		return err
	}
	if err := s.control.QueryRowContext(ctx, "PRAGMA page_size").Scan(&s.pageSize); err != nil {
		return err
	}
	catalog, err := LoadCatalog(ctx, s.config.Store)
	if err != nil {
		return err
	}
	if _, found := catalog.LastPosition(); found {
		if err := s.resume(catalog); err != nil {
			return err
		}
	} else {
		return s.withWriteLock(ctx, func() error {
			data, file, err := s.readWAL()
			if err != nil {
				return err
			}
			meta, err := s.snapshot(ctx, ReasonInitial, s.config.Origin, data, file)
			if err != nil {
				return err
			}
			s.openSnapshot = meta
			if err := s.takePin(ctx); err != nil {
				return err
			}
			s.continuous = true
			return nil
		})
	}
	return s.takePin(ctx)
}

func (s *Shipper) resume(catalog *Catalog) error {
	last, _ := catalog.LastPosition()
	// Refuse to rewind silently over a damaged snapshot or segment past the
	// last usable position, which would otherwise fork a second history.
	for _, d := range catalog.Damaged {
		if _, position, err := ParseKey(d.Key); err == nil && position > last {
			return fmt.Errorf("walship: resume: %w: the store holds a damaged object %s past the last usable position %d: %s", ErrBrokenChain, d.Key, last, d.Problem)
		}
	}
	plan, err := catalog.Plan(last)
	if err != nil {
		return fmt.Errorf("walship: resume: %w", err)
	}
	image, report, err := plan.Execute()
	if err != nil {
		return fmt.Errorf("walship: resume: %w", err)
	}
	s.image, s.state, s.position = image, report.StateSHA256, last
	s.wal = plan.Snapshot.Meta.WAL
	if n := len(plan.Segments); n > 0 {
		s.wal = plan.Segments[n-1].Meta.End()
	}
	s.sinceSnapshot = len(plan.Segments)
	s.resumed = true
	return nil
}

// Resumed reports whether Open resumed from existing objects in the store
// rather than taking a fresh initial snapshot.
func (s *Shipper) Resumed() bool { return s.resumed }

// OpenSnapshot returns the initial SnapshotMeta taken during Open, or nil if
// the shipper resumed from an existing store.
func (s *Shipper) OpenSnapshot() *SnapshotMeta { return s.openSnapshot }

// Position returns the shipper's latest shipped stream position.
func (s *Shipper) Position() int64 { return s.position }

// StateSHA256 returns the SHA-256 digest of the database image at Position.
func (s *Shipper) StateSHA256() string { return s.state }

// WALPosition returns the WAL generation, frame, and checksum at Position.
func (s *Shipper) WALPosition() WALPosition { return s.wal }

func (s *Shipper) withWriteLock(ctx context.Context, work func() error) error {
	if _, err := s.control.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("walship: take the write lock: %w", err)
	}
	if err := work(); err != nil {
		_, rollbackErr := s.control.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	_, err := s.control.ExecContext(ctx, "COMMIT")
	return err
}

// takePin starts a real read transaction on s.pin so SQLite acquires a WAL read
// mark (walTryBeginRead, src/wal.c:3020-3272): slot >= 1 caps every checkpoint
// at mxSafeFrame (src/wal.c:2233-2251), and slot 0 (when nBackfill == mxFrame)
// blocks any backfill because backfilling needs an exclusive lock on slot 0
// (src/wal.c:2259-2260).
func (s *Shipper) takePin(ctx context.Context) error {
	if _, err := s.pin.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	var tables int
	if err := s.pin.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		_, rollbackErr := s.pin.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	s.pinned = true
	return nil
}

func (s *Shipper) dropPin(ctx context.Context) error {
	if !s.pinned {
		return nil
	}
	s.pinned = false
	_, err := s.pin.ExecContext(ctx, "COMMIT")
	return err
}

// readWAL reads and parses the live -wal file directly. This is POSIX-lock-safe
// inside the owning process because SQLite places advisory locks on the .db and
// -shm files, never on the -wal file.
func (s *Shipper) readWAL() ([]byte, *walfile.File, error) {
	data, err := os.ReadFile(s.config.Database + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	file, err := walfile.Parse(data)
	if errors.Is(err, walfile.ErrWALIgnored) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if file.Size > 0 && file.PageSize() != s.pageSize {
		return nil, nil, fmt.Errorf("WAL page size %d, database page size %d", file.PageSize(), s.pageSize)
	}
	return data, file, nil
}

func generationOf(data []byte, file *walfile.File) Generation {
	if file == nil || file.Size == 0 {
		return Generation{}
	}
	return GenerationOf(data[:walfile.HeaderSize])
}

func chainAt(file *walfile.File, frame int) [2]uint32 {
	if file == nil || file.Size == 0 {
		return [2]uint32{}
	}
	if frame == 0 {
		return file.Header.Checksum
	}
	return file.Frames[frame-1].StoredChecksum
}

func lastCommit(file *walfile.File) int {
	if file == nil {
		return 0
	}
	return file.LastCommitFrame
}

// RowsQuerier is the query interface required by ReadPages (implemented by
// *sql.DB, *sql.Conn, *sql.Tx, and *sqlitex.DB).
type RowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ReadPages reads every page of schema "main" in 1..N order through the
// sqlite_dbpage virtual table, refusing gaps, wrong page sizes, or empty
// results. Called inside a read or write transaction, it yields a consistent
// E1 snapshot image.
func ReadPages(ctx context.Context, q RowsQuerier, pageSize int) ([]byte, error) {
	rows, err := q.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage('main') ORDER BY pgno")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var image []byte
	expected := int64(1)
	for rows.Next() {
		var pgno int64
		var data []byte
		if err := rows.Scan(&pgno, &data); err != nil {
			return nil, err
		}
		if pgno != expected || len(data) != pageSize {
			return nil, fmt.Errorf("sqlite_dbpage returned page %d with %d bytes, want page %d with %d", pgno, len(data), expected, pageSize)
		}
		image = append(image, data...)
		expected++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(image) == 0 {
		return nil, errors.New("sqlite_dbpage returned no pages")
	}
	return image, nil
}

func (s *Shipper) readPages(ctx context.Context) ([]byte, error) {
	return ReadPages(ctx, s.control, s.pageSize)
}

func (s *Shipper) snapshot(ctx context.Context, reason string, position int64, data []byte, file *walfile.File) (*SnapshotMeta, error) {
	image, err := s.readPages(ctx)
	if err != nil {
		return nil, err
	}
	digest := SHA256Hex(image)
	frame := lastCommit(file)
	// Every periodic snapshot cross-checks SQLite's live pages against the
	// shipper's own replayed mirror image; any divergence stops shipping.
	if reason == ReasonPeriodic {
		if digest != s.state {
			return nil, fmt.Errorf("%w: SQLite's pages hash to %s, the replayed state to %s", ErrMirrorDiverged, short(digest), short(s.state))
		}
	}
	meta := SnapshotMeta{
		Position: position, Reason: reason, PageSize: s.pageSize, Pages: uint32(len(image) / s.pageSize),
		UserVersion: userVersion(image), StateSHA256: digest,
		WAL: WALPosition{Generation: generationOf(data, file), Frame: frame, Checksum: chainAt(file, frame)},
	}
	object, err := EncodeSnapshot(meta, image)
	if err != nil {
		return nil, err
	}
	if err := s.config.Store.PutIfAbsent(ctx, SnapshotKey(position), object); err != nil {
		return nil, err
	}
	s.image, s.state, s.position, s.wal = image, digest, position, meta.WAL
	s.sinceSnapshot = 0
	return &meta, nil
}

// Cycle runs one atomic shipping step under BEGIN IMMEDIATE on the control
// connection: read and classify -wal, publish either a discontinuity snapshot
// or one segment covering all newly committed frames, drop the pin to run a
// shipper-owned PASSIVE checkpoint, re-acquire the pin, and take a periodic
// snapshot if SnapshotEvery segments have been shipped since the last one.
// If any error occurs, the shipper marks itself broken until reopened.
func (s *Shipper) Cycle(ctx context.Context) (CycleReport, error) {
	var report CycleReport
	if s.broken != nil {
		return report, fmt.Errorf("walship: an earlier cycle failed; reopen the shipper to resume from the store: %w", s.broken)
	}
	err := s.withWriteLock(ctx, func() error {
		data, file, err := s.readWAL()
		if err != nil {
			return err
		}
		start, change, reason, err := s.classify(ctx, data, file)
		if err != nil {
			return err
		}
		report.Change, report.Reason = change, reason
		if change == WALDiscontinuity {
			meta, err := s.snapshot(ctx, ReasonDiscontinuity, s.position+1, data, file)
			if err != nil {
				return err
			}
			report.Snapshot = meta
		} else {
			s.wal = start
			if lastCommit(file) > s.wal.Frame {
				meta, err := s.ship(ctx, data, file)
				if err != nil {
					return err
				}
				report.Segment = meta
			}
		}
		if err := s.dropPin(ctx); err != nil {
			return err
		}
		result, err := sqlitex.Checkpoint(ctx, s.pin, "", sqlitex.CheckpointPassive)
		if err != nil {
			return err
		}
		report.Checkpoint = result
		s.fullyBackfilled = !result.Busy && result.LogFrames == result.Checkpointed && result.LogFrames == s.wal.Frame
		report.FullBackfill = s.fullyBackfilled
		if err := s.takePin(ctx); err != nil {
			return err
		}
		if report.Snapshot == nil && s.config.SnapshotEvery > 0 && s.sinceSnapshot >= s.config.SnapshotEvery {
			meta, err := s.snapshot(ctx, ReasonPeriodic, s.position, data, file)
			if err != nil {
				return err
			}
			report.Snapshot = meta
		}
		return nil
	})
	if err != nil {
		s.broken = err
		return report, err
	}
	s.continuous = true
	return report, nil
}

// classify decides whether the live -wal continues s.wal. Because SQLite's
// nCkpt and salt-1 are per-connection (src/wal.c:2152-2166, 4095-4096) and
// cannot prove how many restarts occurred, a header change is accepted as
// WALNewGeneration only when the shipper held its pin continuously and its last
// checkpoint backfilled every shipped frame; otherwise it forces a snapshot.
func (s *Shipper) classify(ctx context.Context, data []byte, file *walfile.File) (WALPosition, WALChange, string, error) {
	current := generationOf(data, file)
	if current == s.wal.Generation {
		// When both the last shipped position and the live database have no WAL
		// and the shipper was closed in between, offline commits could have
		// been checkpointed and the -wal deleted; verify the full page hash.
		if current.None() && !s.continuous {
			image, err := s.readPages(ctx)
			if err != nil {
				return s.wal, WALUnchanged, "", err
			}
			if SHA256Hex(image) != s.state {
				return s.wal, WALDiscontinuity, "there is no WAL and the database pages differ from the last shipped state", nil
			}
		}
		if lastCommit(file) < s.wal.Frame {
			return s.wal, WALDiscontinuity, fmt.Sprintf("the WAL holds %d committed frames, %d were already shipped", lastCommit(file), s.wal.Frame), nil
		}
		if s.wal.Frame > 0 && chainAt(file, s.wal.Frame) != s.wal.Checksum {
			return s.wal, WALDiscontinuity, fmt.Sprintf("frame %d no longer carries the shipped checksum", s.wal.Frame), nil
		}
		return s.wal, WALUnchanged, "", nil
	}
	if !s.continuous {
		return s.wal, WALDiscontinuity, "the WAL header changed while the shipper held no read mark", nil
	}
	if !s.wal.Generation.None() && !s.fullyBackfilled {
		return s.wal, WALDiscontinuity, "the WAL header changed although the shipper's last checkpoint did not copy every frame", nil
	}
	return WALPosition{Generation: current, Frame: 0, Checksum: chainAt(file, 0)}, WALNewGeneration, "", nil
}

func (s *Shipper) ship(ctx context.Context, data []byte, file *walfile.File) (*SegmentMeta, error) {
	first, last := s.wal.Frame+1, file.LastCommitFrame
	frameSize := file.FrameSize()
	start := file.Frames[first-1].Offset
	body := append([]byte(nil), data[start:file.Frames[last-1].Offset+int64(frameSize)]...)
	meta := SegmentMeta{
		FirstPosition: s.position + 1, Generation: s.wal.Generation, FirstFrame: first, LastFrame: last,
		SeedChecksum: s.wal.Checksum, LastChecksum: chainAt(file, last), PageSize: s.pageSize, BeforeSHA256: s.state,
	}
	// Enumerate every commit frame first so VerifySegmentFrames inside
	// ApplySegment can cross-check the complete commit list before replay.
	for _, frame := range file.Frames[first-1 : last] {
		if frame.IsCommit() {
			meta.Commits = append(meta.Commits, CommitPoint{
				Position: s.position + int64(len(meta.Commits)) + 1, Frame: frame.Number, Pages: frame.CommitSize,
			})
		}
	}
	image := s.image
	for i := range meta.Commits {
		out, _, err := ApplySegment(s.image, meta, body, meta.Commits[i].Frame)
		if err != nil {
			return nil, err
		}
		image = out
		meta.Commits[i].UserVersion, meta.Commits[i].StateSHA256 = userVersion(out), SHA256Hex(out)
	}
	meta.LastPosition = meta.Commits[len(meta.Commits)-1].Position
	meta.AfterSHA256 = meta.Commits[len(meta.Commits)-1].StateSHA256
	object, err := EncodeSegment(meta, body)
	if err != nil {
		return nil, err
	}
	if err := s.config.Store.PutIfAbsent(ctx, SegmentKey(meta.FirstPosition), object); err != nil {
		return nil, err
	}
	s.image, s.state, s.position, s.wal = image, meta.AfterSHA256, meta.LastPosition, meta.End()
	s.sinceSnapshot++
	return &meta, nil
}

// Close releases the held read pin and closes the shipper's SQLite connections.
func (s *Shipper) Close() error {
	var pinErr error
	if s.pinned {
		pinErr = s.dropPin(context.Background())
	}
	return errors.Join(pinErr, s.closeConnections())
}

func (s *Shipper) closeConnections() error {
	var errs []error
	if s.pin != nil {
		errs = append(errs, s.pin.Close())
	}
	if s.control != nil {
		errs = append(errs, s.control.Close())
	}
	errs = append(errs, s.db.Close())
	return errors.Join(errs...)
}
```

---

## 6. Evidence and failure cases

### 6.1 Experiment 9.3 — `integrity-check-limits`

- **Claim:** `PRAGMA integrity_check` verifies B-tree structure and index-to-table consistency, not row content or unused pages: it catches a damaged B-tree page header and an indexed-column mismatch against its index, but returns `ok` when a byte is flipped inside an unindexed column or a freelist leaf page. `PRAGMA quick_check` skips index checks (`src/pragma.c:2058`) and misses the indexed-column mismatch too. The E3 logical hash catches every data-value change but misses the freelist leaf. Only the Chapter 2 per-page SHA-256 manifest (`capture.VerifyImage`) detects all four sites and names the exact damaged page.
- **Evidence form:** `EXACT` (every stored table must match on every run).
- **Why kept:** "The restore passed `integrity_check`" is the usual proof offered for a rebuilt database, and it is not one. The Rebuild Ladder's hashes are.

<!-- include: experiments/9.3-integrity-check-limits/main.go -->
```go
// Experiment 9.3 demonstrates why PRAGMA integrity_check and PRAGMA quick_check
// verify B-tree structure and index consistency rather than data content. It
// flips one byte in each of four distinct regions of a step-12 recipe database
// (a B-tree page header cell count, an unindexed text column, an indexed
// integer column, and a freelist leaf page) and compares integrity_check
// (src/btree.c:11144-11295, src/pragma.c:1850-1857, 2084-2090), quick_check,
// the E3 logical hash, and a Chapter 2 per-page SHA-256 manifest.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 9.3
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	step          = 12
	eventsTable   = "events"
	accountColumn = 1
	noteColumn    = 4
)

type site struct {
	name   string
	offset int64
	mask   byte
	detail string
}

type verdicts struct {
	integrity, quick []string
	logical          int
	differing        []int
	caught           [4]bool
}

// findSites uses internal/dbfile to locate four deterministic single-byte
// corruption targets in f: a leaf page header's cell count, one ASCII letter
// inside unindexed events.note, the 1-byte integer value of indexed
// events.account_id, and the midpoint of a freelist leaf page.
func findSites(f *dbfile.File) ([]site, error) {
	census, err := f.Census()
	if err != nil {
		return nil, err
	}
	pageSize := int64(f.PageSize())
	var leaves []int
	for _, info := range census.Pages {
		if info.Tree == eventsTable && info.Role == dbfile.RoleTableLeaf {
			leaves = append(leaves, info.Number)
		}
	}
	if len(leaves) < 2 || len(census.FreelistLeaves) == 0 {
		return nil, fmt.Errorf("need two %s leaf pages and a freelist leaf, found %d and %d", eventsTable, len(leaves), len(census.FreelistLeaves))
	}
	headerPage := leaves[0]
	page, err := f.BTreePage(leaves[1])
	if err != nil {
		return nil, err
	}
	cell, err := page.Cell(0)
	if err != nil {
		return nil, err
	}
	if cell.Overflows() {
		return nil, fmt.Errorf("first cell of page %d overflows", page.Number)
	}
	localStart := int64(page.Number-1)*pageSize + int64(cell.Offset+dbfile.VarintLength(cell.PayloadSize)+dbfile.VarintLength(uint64(cell.Rowid)))
	accountStart, accountLength, _, err := dbfile.RecordColumn(cell.Local, accountColumn)
	if err != nil || accountLength != 1 {
		return nil, fmt.Errorf("account_id column: length %d, %v", accountLength, err)
	}
	noteStart, noteLength, _, err := dbfile.RecordColumn(cell.Local, noteColumn)
	if err != nil || noteLength == 0 {
		return nil, fmt.Errorf("note column: %v", err)
	}
	letter := -1
	for i := noteStart; i < noteStart+noteLength; i++ {
		if c := cell.Local[i]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			letter = i
			break
		}
	}
	if letter < 0 {
		return nil, errors.New("the note has no letter to change")
	}
	freelist := census.FreelistLeaves[0]
	return []site{
		{"b-tree page header (cell count)", int64(headerPage-1)*pageSize + 4, 0x01, fmt.Sprintf("page %d (events leaf), header byte 4", headerPage)},
		{"unindexed column (events.note)", localStart + int64(letter), 0x20, fmt.Sprintf("page %d, row %d, letter %q -> %q", page.Number, cell.Rowid, cell.Local[letter], cell.Local[letter]^0x20)},
		{"indexed column (events.account_id)", localStart + int64(accountStart), 0x01, fmt.Sprintf("page %d, row %d, account_id %d -> %d", page.Number, cell.Rowid, cell.Local[accountStart], cell.Local[accountStart]^0x01)},
		{"freelist leaf page", int64(freelist-1)*pageSize + pageSize/2, 0xff, fmt.Sprintf("page %d, byte %d", freelist, pageSize/2)},
	}, nil
}

func corruptCopy(original []byte, s site, path string) error {
	image := append([]byte(nil), original...)
	image[s.offset] ^= s.mask
	return os.WriteFile(path, image, 0o644)
}

// check runs PRAGMA integrity_check, PRAGMA quick_check, E3 logical hashing,
// and capture.VerifyImage against path, confirming SQLite's read-only checks do
// not modify the file bytes.
func check(ctx context.Context, path string, original equal.Manifest, originalLogical equal.Logical) (verdicts, error) {
	before, err := equal.FileSHA256(path)
	if err != nil {
		return verdicts{}, err
	}
	inspection, err := procs.InspectDatabase(ctx, path, sqlitex.JournalWAL, "PRAGMA quick_check")
	if err != nil {
		return verdicts{}, err
	}
	logical, err := equal.LogicalHashFile(ctx, path)
	if err != nil {
		return verdicts{}, err
	}
	after, err := equal.FileSHA256(path)
	if err != nil || after != before {
		return verdicts{}, fmt.Errorf("checking %s changed the file (%v)", path, err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return verdicts{}, err
	}
	differing, err := capture.VerifyImage(original, image)
	if err != nil {
		return verdicts{}, err
	}
	logicalDiff := equal.CompareLogical(originalLogical, logical)
	v := verdicts{
		integrity: procs.IntegrityProblems(inspection.Integrity),
		quick:     procs.IntegrityProblems(inspection.Values[0]),
		logical:   len(logicalDiff.Differences),
		differing: differing,
	}
	v.caught = [4]bool{inspection.Integrity != "ok", inspection.Values[0] != "ok", !logicalDiff.Equal, len(differing) > 0}
	return v, nil
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set := results.MustOpen(dir)
	originalTable := set.Table("original", "recipe_step", "pages", "manifest_sha256")
	sitesTable := set.Table("sites", "seq", "site", "location", "file_offset", "xor_mask")
	verdictsTable := set.Table("verdicts", "seq", "file", "site",
		"caught_integrity", "caught_quick", "caught_e3", "caught_manifest",
		"expected_integrity", "expected_quick", "expected_e3", "expected_manifest",
		"e3_differences", "differing_pages")
	problemsTable := set.Table("problems", "seq", "file", "pragma", "problem_no", "problem")
	record := func(seq int, file, site string, v verdicts, want [4]bool) {
		verdictsTable.Row(seq, file, site, v.caught[0], v.caught[1], v.caught[2], v.caught[3], want[0], want[1], want[2], want[3], v.logical, joinInts(v.differing))
		for i, problem := range v.integrity {
			problemsTable.Row(seq, file, "integrity_check", i+1, problem)
		}
		for i, problem := range v.quick {
			problemsTable.Row(seq, file, "quick_check", i+1, problem)
		}
	}

	r := recipe.Default()
	db := filepath.Join(dir, "original.db")
	if err := r.Build(ctx, db, step); err != nil {
		fatal("build", err)
	}
	original, err := os.ReadFile(db)
	if err != nil {
		fatal("read", err)
	}
	f, err := dbfile.Open(db)
	if err != nil {
		fatal("open", err)
	}
	manifest, err := capture.BuildManifest(f)
	if err != nil {
		fatal("manifest", err)
	}
	sites, err := findSites(f)
	if closeErr := f.Close(); err != nil || closeErr != nil {
		fatal("sites", errors.Join(err, closeErr))
	}
	logical, err := equal.LogicalHashFile(ctx, db)
	if err != nil {
		fatal("logical hash", err)
	}
	baseline, err := check(ctx, db, manifest, logical)
	if err != nil {
		fatal("baseline", err)
	}
	originalTable.Row(step, manifest.PageCount(), manifest.SHA256()[:16])
	record(0, "original.db", "none (the undamaged file)", baseline, [4]bool{})
	set.Check("A", baseline.caught == [4]bool{}, "every check passes on the undamaged file")
	want := map[string][4]bool{
		sites[0].name: {true, true, true, true},
		sites[1].name: {false, false, true, true},
		sites[2].name: {true, false, true, true},
		sites[3].name: {false, false, false, true},
	}
	for i, s := range sites {
		name := fmt.Sprintf("site-%d.db", i+1)
		path := filepath.Join(dir, name)
		if err := corruptCopy(original, s, path); err != nil {
			fatal("corrupt", err)
		}
		v, err := check(ctx, path, manifest, logical)
		if err != nil {
			fatal(s.name, err)
		}
		sitesTable.Row(i+1, s.name, s.detail, s.offset, s.mask)
		record(i+1, name, s.name, v, want[s.name])
		set.Check("B", v.caught == want[s.name], fmt.Sprintf("%s: caught by integrity_check %t, quick_check %t, E3 %t, manifest %t", s.name, want[s.name][0], want[s.name][1], want[s.name][2], want[s.name][3]))
	}
	set.Finish("INTEGRITY CHECK LIMITS")
}

func joinInts(values []int) string {
	text := make([]string, len(values))
	for i, v := range values {
		text[i] = fmt.Sprint(v)
	}
	return strings.Join(text, " ")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/9.3-integrity-check-limits/report.sql -->
```sql
-- Walkthrough of experiment 9.3. Run it with:
--   go run ./cmd/course run 9.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   original   the undamaged recipe database (one row)
--   sites      one row per single-byte flip: where it is and what it changes
--   verdicts   one row per file checked (the original, then one per flip):
--              which of the four checks caught it, and which were expected to
--   problems   one row per problem line PRAGMA integrity_check or quick_check reported
--   checks     every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.3  What does integrity_check not catch?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The experiment builds the recipe database to one step, then makes four'
.print 'copies, each with one byte flipped in a different place. Every file is'
.print 'checked four ways: PRAGMA integrity_check, PRAGMA quick_check, the E3'
.print 'logical hash (every row and schema object), and the per-page SHA-256'
.print 'manifest of Chapter 2 taken from the original. No check changes the file.'

.print
.print '── A. The undamaged file ───────────────────────────────────────────────'
.print
.print 'A1. The original, and the manifest every copy is compared with:'

SELECT recipe_step, pages, manifest_sha256 FROM original ORDER BY recipe_step;

.print
.print 'A2. Every check passes on it (caught = false everywhere, no problem lines):'

SELECT file, caught_integrity, caught_quick, caught_e3, caught_manifest,
       (SELECT count(*) FROM problems p WHERE p.seq = v.seq) AS problem_lines
FROM verdicts v WHERE seq = 0 ORDER BY seq;

.print
.print '── B. One byte flipped ─────────────────────────────────────────────────'
.print
.print 'B1. The four places. The cell count is B-tree structure; the note and'
.print '    account_id are row data, and only account_id has an index; the'
.print '    freelist page belongs to no table at all.'

SELECT seq, site, location, file_offset, printf('0x%02x', xor_mask) AS xor_mask FROM sites ORDER BY seq;

.print
.print 'B2. Which check caught which flip, measured against expected. Only the'
.print '    page manifest catches all four. integrity_check misses the unindexed'
.print '    column and the freelist page; quick_check catches only the header.'

SELECT site,
       caught_integrity AS integrity_check,
       caught_quick     AS quick_check,
       caught_e3        AS e3_logical,
       caught_manifest  AS manifest,
       (caught_integrity, caught_quick, caught_e3, caught_manifest)
         = (expected_integrity, expected_quick, expected_e3, expected_manifest) AS as_expected
FROM verdicts WHERE seq > 0 ORDER BY seq;

.print
.print 'B3. How much each check reported. The freelist flip changes no row, schema'
.print '    object or pragma, so the E3 hash sees no difference; only the page'
.print '    manifest sees it.'

SELECT v.site,
       count(*) FILTER (p.pragma = 'integrity_check') AS integrity_problems,
       count(*) FILTER (p.pragma = 'quick_check')     AS quick_problems,
       v.e3_differences,
       v.differing_pages                              AS manifest_pages_differ
FROM verdicts v LEFT JOIN problems p USING (seq)
WHERE v.seq > 0
GROUP BY v.seq, v.site, v.e3_differences, v.differing_pages
ORDER BY v.seq;

.print
.print 'B4. What the two pragmas said, problem by problem. integrity_check reads'
.print '    every index against its table, so it finds the row missing from'
.print '    events_by_account; quick_check skips that comparison.'

SELECT v.site, p.pragma, p.problem
FROM problems p JOIN verdicts v USING (seq)
ORDER BY p.seq, p.pragma, p.problem_no;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 9.3
```

Add `-i` (`go run ./cmd/course run -i 9.3`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/9.3-integrity-check-limits/`](evidence/9.3-integrity-check-limits/), and `go run ./cmd/course check evidence ch09` compares a fresh run with them byte for byte.

<!-- include: evidence/9.3-integrity-check-limits.txt -->
```text

════════════════════════════════════════════════════════════════════════
  9.3  What does integrity_check not catch?
════════════════════════════════════════════════════════════════════════

The experiment builds the recipe database to one step, then makes four
copies, each with one byte flipped in a different place. Every file is
checked four ways: PRAGMA integrity_check, PRAGMA quick_check, the E3
logical hash (every row and schema object), and the per-page SHA-256
manifest of Chapter 2 taken from the original. No check changes the file.

── A. The undamaged file ───────────────────────────────────────────────

A1. The original, and the manifest every copy is compared with:
┌─────────────┬───────┬──────────────────┐
│ recipe_step │ pages │ manifest_sha256  │
├─────────────┼───────┼──────────────────┤
│ 12          │ 118   │ 5a2143ca7f8bc869 │
└─────────────┴───────┴──────────────────┘

A2. Every check passes on it (caught = false everywhere, no problem lines):
┌─────────────┬──────────────────┬──────────────┬───────────┬─────────────────┬───────────────┐
│    file     │ caught_integrity │ caught_quick │ caught_e3 │ caught_manifest │ problem_lines │
├─────────────┼──────────────────┼──────────────┼───────────┼─────────────────┼───────────────┤
│ original.db │ false            │ false        │ false     │ false           │ 0             │
└─────────────┴──────────────────┴──────────────┴───────────┴─────────────────┴───────────────┘

── B. One byte flipped ─────────────────────────────────────────────────

B1. The four places. The cell count is B-tree structure; the note and
    account_id are row data, and only account_id has an index; the
    freelist page belongs to no table at all.
┌─────┬────────────────────────────────────┬──────────────────────────────────────┬─────────────┬──────────┐
│ seq │                site                │               location               │ file_offset │ xor_mask │
├─────┼────────────────────────────────────┼──────────────────────────────────────┼─────────────┼──────────┤
│ 1   │ b-tree page header (cell count)    │ page 26 (events leaf), header byte 4 │ 102404      │ 0x01     │
│ 2   │ unindexed column (events.note)     │ page 27, row 76, letter 'q' -> 'Q'   │ 110538      │ 0x20     │
│ 3   │ indexed column (events.account_id) │ page 27, row 76, account_id 8 -> 9   │ 110527      │ 0x01     │
│ 4   │ freelist leaf page                 │ page 24, byte 2048                   │ 96256       │ 0xff     │
└─────┴────────────────────────────────────┴──────────────────────────────────────┴─────────────┴──────────┘

B2. Which check caught which flip, measured against expected. Only the
    page manifest catches all four. integrity_check misses the unindexed
    column and the freelist page; quick_check catches only the header.
┌────────────────────────────────────┬─────────────────┬─────────────┬────────────┬──────────┬─────────────┐
│                site                │ integrity_check │ quick_check │ e3_logical │ manifest │ as_expected │
├────────────────────────────────────┼─────────────────┼─────────────┼────────────┼──────────┼─────────────┤
│ b-tree page header (cell count)    │ true            │ true        │ true       │ true     │ true        │
│ unindexed column (events.note)     │ false           │ false       │ true       │ true     │ true        │
│ indexed column (events.account_id) │ true            │ false       │ true       │ true     │ true        │
│ freelist leaf page                 │ false           │ false       │ false      │ true     │ true        │
└────────────────────────────────────┴─────────────────┴─────────────┴────────────┴──────────┴─────────────┘

B3. How much each check reported. The freelist flip changes no row, schema
    object or pragma, so the E3 hash sees no difference; only the page
    manifest sees it.
┌────────────────────────────────────┬────────────────────┬────────────────┬────────────────┬───────────────────────┐
│                site                │ integrity_problems │ quick_problems │ e3_differences │ manifest_pages_differ │
├────────────────────────────────────┼────────────────────┼────────────────┼────────────────┼───────────────────────┤
│ b-tree page header (cell count)    │ 2                  │ 1              │ 2              │ 26                    │
│ unindexed column (events.note)     │ 0                  │ 0              │ 1              │ 27                    │
│ indexed column (events.account_id) │ 1                  │ 0              │ 2              │ 27                    │
│ freelist leaf page                 │ 0                  │ 0              │ 0              │ 24                    │
└────────────────────────────────────┴────────────────────┴────────────────┴────────────────┴───────────────────────┘

B4. What the two pragmas said, problem by problem. integrity_check reads
    every index against its table, so it finds the row missing from
    events_by_account; quick_check skips that comparison.
┌────────────────────────────────────┬─────────────────┬────────────────────────────────────────────────────┐
│                site                │     pragma      │                      problem                       │
├────────────────────────────────────┼─────────────────┼────────────────────────────────────────────────────┤
│ b-tree page header (cell count)    │ integrity_check │ Fragmentation of 46 bytes reported as 0 on page 26 │
│ b-tree page header (cell count)    │ integrity_check │ wrong # of entries in index events_by_account      │
│ b-tree page header (cell count)    │ quick_check     │ Fragmentation of 46 bytes reported as 0 on page 26 │
│ indexed column (events.account_id) │ integrity_check │ row 66 missing from index events_by_account        │
└────────────────────────────────────┴─────────────────┴────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  every check passes on the undamaged file
B  ✓  b-tree page header (cell count): caught by integrity_check true, quick_check true, E3 true, manifest true
B  ✓  unindexed column (events.note): caught by integrity_check false, quick_check false, E3 true, manifest true
B  ✓  indexed column (events.account_id): caught by integrity_check true, quick_check false, E3 true, manifest true
B  ✓  freelist leaf page: caught by integrity_check false, quick_check false, E3 false, manifest true

5 of 5 claims hold

Experiment verdict: INTEGRITY CHECK LIMITS OK
```

#### Reading the Experiment 9.3 evidence

- **A1–A2 (baseline):** The undamaged step-12 recipe database has `118` pages, manifest SHA-256 `5a2143ca7f8bc869`, and passes `integrity_check` and `quick_check` with `ok`.
- **Site 1 in B1–B4 (B-tree page header cell count, page `26`, header byte `4`):** Flipping `0x01` alters the page's cell count, leaving `46` bytes unaccounted for. All four checks catch it (`integrity_check` reports `2` problems, the first `Fragmentation of 46 bytes reported as 0 on page 26`; `quick_check` fails; E3 differs; manifest names page `26`).
- **Site 2 (unindexed column `events.note`, page `27`, row `76`, `'q' -> 'Q'`):** Both `integrity_check` and `quick_check` report **no problem** (caught `false`). The B-tree structure and every index remain valid while stored user data is silently altered. E3 logical hashing catches the changed text (`1` difference), and the manifest pins it to page `27`.
- **Site 3 (indexed column `events.account_id`, page `27`, row `76`, `8 -> 9`):** Because `events_by_account` indexes `account_id`, `integrity_check` catches the table-versus-index disagreement (`1` problem: `row 66 missing from index events_by_account`), while `quick_check` still reports **no problem** because it skips index cross-checks. Notice `"row 66"` for rowid `76`: `pragma.c:1850-1857` and `2084-2090` print the table-scan row counter, not the `rowid`. Both E3 (`2` differences) and the manifest (page `27`) catch it.
- **Site 4 (freelist leaf page `24`, byte `2048`):** `integrity_check`, `quick_check`, and E3 logical hashing all report **no problem / no difference** because a freelist leaf holds no live rows and `sqlite3BtreeIntegrityCheck` never inspects its body. Only the page manifest catches it (page `24`). All 5 checks pass (verdict `INTEGRITY CHECK LIMITS OK`).

---

### 6.2 Experiment 9.4 — `point-in-time-rebuild` (rung R6)

- **Claim:** One base snapshot plus ordered WAL segments captured continuously across shipper-owned checkpoints and WAL restarts (`internal/walship`) can rebuild the database as of **any** shipped commit, byte-identical (**E1 bytes**) to the recipe's checkpointed state for that step. Restoring from the newest snapshot before a target never replays more than `SnapshotEvery` (`3`) segments and agrees byte for byte with restoring from older reachable snapshots. An outside `TRUNCATE` checkpoint while the shipper is down causes the reopened shipper to detect the unvouched WAL header change and re-base with a discontinuity snapshot (losing only the intermediate commits folded away while it was stopped, never silently corrupting the chain). Missing, swapped, or byte-flipped segments are rejected during `Catalog.Plan` before any page is applied.
- **Evidence form:** `EXACT` (every stored table must match on every run).

<!-- include: experiments/9.4-point-in-time-rebuild/main.go -->
```go
// Experiment 9.4 demonstrates Rebuild Ladder rung R6 using internal/walship:
// continuous snapshot-plus-WAL-segment capture across shipper-owned checkpoints
// and WAL restarts (Phase 1), discontinuity detection and re-basing after an
// outside TRUNCATE checkpoint while the shipper was stopped (Phase 2), exact E1
// point-in-time restore of every shipped position from both newest and older
// snapshots, and pre-replay rejection of missing, swapped, and corrupted
// segments.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 9.4
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

const (
	baseStep      = 2
	snapshotEvery = 3
)

var shippedBatches = [][]int{{3}, {4, 5}, {6}, {7, 8, 9}, {10}, {11, 12}, {13}, {14}}

type run struct {
	ctx      context.Context
	lab      *procs.Lab
	r        recipe.Recipe
	dir      string
	db       string
	store    *walship.DirStore
	shipper  *walship.Shipper
	writer   *procs.Child
	outsider *procs.Child
	names    walship.GenerationNames
	set      *results.Set
	part     string
	cycles   int
	tampers  int
	tables   struct {
		steps, starts, cycles, snapshots, store, restores, final, tampered *results.Table
	}
}

func (x *run) openTables() {
	x.tables.steps = x.set.Table("steps", "step", "part", "next_cycle", "child", "action", "outcome")
	x.tables.starts = x.set.Table("shipper_starts", "part", "started", "position")
	x.tables.cycles = x.set.Table("cycles", "cycle", "part", "wal", "segment", "first_position", "last_position", "first_frame", "last_frame", "generation", "first_user_version", "last_user_version", "before_sha256", "after_sha256", "checkpoint_busy", "checkpoint_log", "checkpointed", "every_frame_copied")
	x.tables.snapshots = x.set.Table("snapshots", "cycle", "part", "snapshot", "reason", "position", "pages", "user_version", "state_sha256", "after_frame", "generation")
	x.tables.store = x.set.Table("store", "snapshots", "snapshot_positions", "segments", "damaged")
	x.tables.restores = x.set.Table("restores", "position", "snapshot", "segments", "user_version", "sha256", "recipe_sha256", "e1_equal", "older_snapshots", "older_same_bytes", "older_cannot_reach")
	x.tables.final = x.set.Table("final", "file", "position", "integrity_check", "recipe_step", "exact_bytes")
	x.tables.tampered = x.set.Table("tampered", "seq", "damage", "restore_to", "refused_while_planning", "error")
}

func (x *run) generation(g walship.Generation) string {
	return strings.TrimPrefix(x.names.Name(g), "WAL generation ")
}

func (x *run) snapshotRow(cycle any, m walship.SnapshotMeta) {
	x.tables.snapshots.Row(cycle, x.part, walship.SnapshotKey(m.Position), m.Reason, m.Position, m.Pages, m.UserVersion, m.StateSHA256[:16], m.WAL.Frame, x.generation(m.WAL.Generation))
}

func (x *run) openShipper() {
	shipper, err := walship.Open(x.ctx, walship.Config{Database: x.db, Store: x.store, Options: x.r.SQLiteOptions(), SnapshotEvery: snapshotEvery, Origin: baseStep})
	if err != nil {
		x.lab.Fatal("open shipper", err)
	}
	x.shipper = shipper
	if snapshot := shipper.OpenSnapshot(); snapshot != nil {
		x.tables.starts.Row(x.part, "opened on an empty store: initial snapshot", snapshot.Position)
		x.snapshotRow(nil, *snapshot)
	} else {
		x.tables.starts.Row(x.part, "reopened: resumed from the store", shipper.Position())
	}
}

func (x *run) must(child *procs.Child, action string, args ...string) procs.Result {
	result := x.lab.Must(child, action, args...)
	x.tables.steps.Row(result.Step, x.part, x.cycles+1, child.Name, procs.ActionText(action, args), result.Outcome())
	return result
}

func (x *run) commit(steps ...int) {
	for _, k := range steps {
		x.must(x.writer, "recipe-session", strconv.Itoa(k))
	}
}

func (x *run) cycle() walship.CycleReport {
	report, err := x.shipper.Cycle(x.ctx)
	if err != nil {
		x.lab.Fatal("ship cycle", err)
	}
	x.cycles++
	wal := report.Change.String()
	if report.Reason != "" {
		wal += ": " + report.Reason
	}
	cp := report.Checkpoint
	if m := report.Segment; m != nil {
		x.tables.cycles.Row(x.cycles, x.part, wal, walship.SegmentKey(m.FirstPosition), m.FirstPosition, m.LastPosition, m.FirstFrame, m.LastFrame, x.generation(m.Generation),
			m.Commits[0].UserVersion, m.Commits[len(m.Commits)-1].UserVersion, m.BeforeSHA256[:16], m.AfterSHA256[:16], cp.Busy, cp.LogFrames, cp.Checkpointed, report.FullBackfill)
	} else {
		x.tables.cycles.Row(x.cycles, x.part, wal, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, cp.Busy, cp.LogFrames, cp.Checkpointed, report.FullBackfill)
	}
	if report.Snapshot != nil {
		x.snapshotRow(x.cycles, *report.Snapshot)
	}
	return report
}

func (x *run) closeShipper() {
	if err := x.shipper.Close(); err != nil {
		x.lab.Fatal("close shipper", err)
	}
}

// restoreEveryPosition rebuilds every stream position from baseStep to last,
// checks E1 equality against recipe.StepHashes, verifies that every older
// snapshot either reproduces identical bytes or refuses across a discontinuity
// gap with ErrBrokenChain, and checks that newest-snapshot restores never
// replay more than snapshotEvery segments.
func (x *run) restoreEveryPosition(catalog *walship.Catalog, hashes []recipe.FileHashes) []int {
	last, _ := catalog.LastPosition()
	var steps []int
	worst := 0
	for position := int64(baseStep); position <= last; position++ {
		plan, err := catalog.Plan(position)
		if err != nil {
			x.lab.Fatal(fmt.Sprintf("plan %d", position), err)
		}
		image, report, err := plan.Execute()
		if err != nil {
			x.lab.Fatal(fmt.Sprintf("restore %d", position), err)
		}
		step := int(plan.Commit.UserVersion)
		steps = append(steps, step)
		worst = max(worst, len(plan.Segments))
		same, refused := 0, 0
		for _, snapshot := range catalog.Snapshots {
			if snapshot.Meta.Position >= plan.Snapshot.Meta.Position {
				continue
			}
			older, err := catalog.PlanFrom(snapshot.Meta.Position, position)
			if errors.Is(err, walship.ErrBrokenChain) {
				refused++
				continue
			}
			if err != nil {
				x.lab.Fatal("plan from an older snapshot", err)
			}
			olderImage, _, err := older.Execute()
			if err != nil {
				x.lab.Fatal("restore from an older snapshot", err)
			}
			if walship.SHA256Hex(olderImage) == report.StateSHA256 {
				same++
			}
		}
		digest := walship.SHA256Hex(image)
		equal := step < len(hashes) && digest == hashes[step].Exact
		older := countOlder(catalog, plan.Snapshot.Meta.Position)
		x.tables.restores.Row(position, plan.Snapshot.Meta.Position, len(plan.Segments), step, digest[:16], hashes[step].Exact[:16], equal, older, same, refused)
		x.set.Check(x.part, equal && same+refused == older, fmt.Sprintf("position %d restores byte-identical to recipe step %d from every snapshot that reaches it", position, step))
	}
	x.set.Check(x.part, worst <= snapshotEvery, fmt.Sprintf("a restore from the newest snapshot never replays more than %d segments (worst here: %d)", snapshotEvery, worst))
	return steps
}

func countOlder(catalog *walship.Catalog, position int64) int {
	n := 0
	for _, snapshot := range catalog.Snapshots {
		if snapshot.Meta.Position < position {
			n++
		}
	}
	return n
}

// tamper copies the finished store, applies one fault (missing, swapped, or
// byte-flipped segment), and verifies Catalog.Plan fails with ErrBrokenChain
// before any page is replayed.
func (x *run) tamper(label string, target int64, damage func(walship.Store) error) {
	copied, err := walship.NewDirStore(filepath.Join(x.dir, "tampered", label))
	if err != nil {
		x.lab.Fatal("tampered store", err)
	}
	if _, err := walship.CopyStore(x.ctx, x.store, copied); err != nil {
		x.lab.Fatal("copy store", err)
	}
	if err := damage(copied); err != nil {
		x.lab.Fatal("damage "+label, err)
	}
	catalog, err := walship.LoadCatalog(x.ctx, copied)
	if err != nil {
		x.lab.Fatal("catalog", err)
	}
	_, err = catalog.Plan(target)
	refused := errors.Is(err, walship.ErrBrokenChain)
	var text any
	if err != nil {
		text = err.Error()
	}
	x.tampers++
	x.tables.tampered.Row(x.tampers, label, target, refused, text)
	x.set.Check(x.part, refused, fmt.Sprintf("%s: refused while planning, before any page is applied", label))
}

func swap(ctx context.Context, store walship.Store, a, b string) error {
	first, err := store.Get(ctx, a)
	if err != nil {
		return err
	}
	second, err := store.Get(ctx, b)
	if err != nil {
		return err
	}
	if err := errors.Join(store.Delete(ctx, a), store.Delete(ctx, b)); err != nil {
		return err
	}
	return errors.Join(store.PutIfAbsent(ctx, a, second), store.PutIfAbsent(ctx, b, first))
}

func flipByte(ctx context.Context, store walship.Store, key string) error {
	data, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	data[len(data)-100] ^= 0x40
	if err := store.Delete(ctx, key); err != nil {
		return err
	}
	return store.PutIfAbsent(ctx, key, data)
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil // the steps table records each child step
	x := &run{ctx: ctx, lab: lab, r: recipe.Default(), dir: dir, db: filepath.Join(dir, "live", "r.db"), set: results.MustOpen(dir), part: "A"}
	x.openTables()
	if err := os.MkdirAll(filepath.Dir(x.db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(ctx, x.db, baseStep); err != nil {
		lab.Fatal("build", err)
	}
	store, err := walship.NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		lab.Fatal("store", err)
	}
	x.store = store
	options := procs.SQLOptions{Path: x.db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}
	x.writer = lab.SpawnSQL("writer", options)
	x.outsider = lab.SpawnSQL("outsider", options)
	x.openShipper()

	// Part A: the writer commits steps 3-14 in batches; the shipper runs one cycle after each batch.
	generations := map[walship.Generation]bool{}
	for _, batch := range shippedBatches {
		x.commit(batch...)
		report := x.cycle()
		last := batch[len(batch)-1]
		segment := report.Segment
		x.set.Check("A", segment != nil && segment.LastPosition == int64(last) && len(segment.Commits) == len(batch) && segment.Commits[len(batch)-1].UserVersion == uint32(last) && report.FullBackfill,
			fmt.Sprintf("steps %v shipped as one segment of %d commit(s), then the shipper's checkpoint copied every frame", batch, len(batch)))
		if segment != nil {
			generations[segment.Generation] = true
		}
	}
	x.set.Check("A", len(generations) == len(shippedBatches), fmt.Sprintf("every segment came from a new WAL generation: the WAL restarted after each shipper checkpoint (%d generations)", len(generations)))

	// Part B: the shipper stops; the writer commits 15 and 16; the outsider runs
	// a TRUNCATE checkpoint; the writer commits 17; the shipper restarts.
	x.part = "B"
	x.closeShipper()
	x.commit(15, 16)
	truncate := x.must(x.outsider, "checkpoint", sqlitex.CheckpointTruncate)
	x.set.Check("B", truncate.Value == "busy=0 log=0 checkpointed=0", "the outsider's TRUNCATE checkpoint copied steps 15-16 into the database file and emptied the WAL")
	x.commit(17)
	x.openShipper()
	report := x.cycle()
	x.set.Check("B", report.Change == walship.WALDiscontinuity && report.Snapshot != nil && report.Snapshot.Reason == walship.ReasonDiscontinuity && report.Snapshot.UserVersion == 17 && report.Segment == nil,
		"the restarted shipper sees a WAL header it did not ship, cannot vouch for what happened in between, and takes a new snapshot (it holds step 17)")
	x.commit(18)
	x.cycle()
	x.commit(19, 20)
	x.cycle()
	x.closeShipper()
	lab.StopChildren()

	// Part C: restore every position from the store and compare with the recipe.
	x.part = "C"
	catalog, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		lab.Fatal("catalog", err)
	}
	x.tables.store.Row(len(catalog.Snapshots), joinPositions(snapshotPositions(catalog)), len(catalog.Segments), len(catalog.Damaged))
	hashes, err := x.r.StepHashes(ctx)
	if err != nil {
		lab.Fatal("recipe hashes", err)
	}
	steps := x.restoreEveryPosition(catalog, hashes)
	var missing []int
	for k := baseStep; k <= 20; k++ {
		if !slices.Contains(steps, k) {
			missing = append(missing, k)
		}
	}
	x.set.Check("C", slices.Equal(missing, []int{15, 16}), "only steps 15 and 16 have no restore point: they were committed and checkpointed away while the shipper was down")

	last, _ := catalog.LastPosition()
	plan, err := catalog.Plan(last)
	if err != nil {
		lab.Fatal("plan", err)
	}
	image, _, err := plan.Execute()
	if err != nil {
		lab.Fatal("restore", err)
	}
	restored := filepath.Join(dir, "restored.db")
	if err := walship.WriteDatabase(restored, image); err != nil {
		lab.Fatal("write", err)
	}
	restoredMatch, err := x.r.MatchStep(ctx, restored)
	if err != nil {
		lab.Fatal("match", err)
	}
	inspection, err := procs.InspectDatabase(ctx, restored, sqlitex.JournalWAL)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	liveMatch, err := x.r.MatchStep(ctx, x.db)
	if err != nil {
		lab.Fatal("match live", err)
	}
	x.tables.final.Row("restored.db", last, procs.IntegritySummary(inspection.Integrity), restoredMatch.Step, restoredMatch.Exact)
	x.tables.final.Row("the live database after every process closed", nil, nil, liveMatch.Step, liveMatch.Exact)
	x.set.Check("C", inspection.Integrity == "ok" && restoredMatch.Exact && restoredMatch.Step == 20 && liveMatch.Exact && liveMatch.Step == 20, "the newest restore point, written to a file, opens, passes integrity_check and equals the live database's final state (step 20)")

	// Part D: damaged stores, each a copy of the store above.
	x.part = "D"
	x.tamper("segment 7 deleted", 9, func(s walship.Store) error { return s.Delete(ctx, walship.SegmentKey(7)) })
	x.tamper("segments 10 and 11 swapped", 11, func(s walship.Store) error { return swap(ctx, s, walship.SegmentKey(10), walship.SegmentKey(11)) })
	x.tamper("one byte of segment 13 flipped", 13, func(s walship.Store) error { return flipByte(ctx, s, walship.SegmentKey(13)) })
	x.set.Finish("POINT IN TIME")
}

func joinPositions(positions []int64) string {
	text := make([]string, len(positions))
	for i, p := range positions {
		text[i] = fmt.Sprint(p)
	}
	return strings.Join(text, " ")
}

func snapshotPositions(catalog *walship.Catalog) []int64 {
	var positions []int64
	for _, s := range catalog.Snapshots {
		positions = append(positions, s.Meta.Position)
	}
	return positions
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/9.4-point-in-time-rebuild/report.sql -->
```sql
-- Walkthrough of experiment 9.4. Run it with:
--   go run ./cmd/course run 9.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps            one row per step a child process ran (writer or outsider);
--                    next_cycle is the shipper cycle that ran after it
--   shipper_starts   one row per time the shipper was opened
--   cycles           one row per shipper cycle: what it saw in the WAL, the segment
--                    it shipped (if any) and its PASSIVE checkpoint
--   snapshots        one row per snapshot the shipper stored (cycle is empty for
--                    the one taken when the shipper opened)
--   store            the store as the restore reads it (one row)
--   restores         one row per stream position restored from the store
--   final            the newest restore written to a file, and the live database
--   tampered         one row per damaged copy of the store
--   checks           every claim the experiment asserted (the runner lists it last)
-- Hashes are the first 16 hex digits of a SHA-256. WAL generations are numbered
-- in the order they appear, because their salts are random.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.4  Point-in-time rebuild from a snapshot and WAL segments'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The recipe database is built to step 2. Two child processes, writer and'
.print 'outsider, each hold a connection (wal_autocheckpoint=0); the writer commits'
.print 'every step on a fresh connection. A shipper (internal/walship) copies each'
.print 'new run of committed WAL frames to a store as one segment, then runs a'
.print 'PASSIVE checkpoint, and stores a snapshot every 3 segments. A stream'
.print 'position counts the commits the shipper has shipped.'

.print
.print '── A. Phase 1: shipping steps 3-14 ─────────────────────────────────────'
.print
.print 'A1. The shipper opens on an empty store and takes the initial snapshot.'

SELECT snapshot, reason, position, pages, user_version, state_sha256,
       CASE WHEN generation = 'no WAL' THEN 'none (no WAL yet)'
            ELSE after_frame || ' of ' || generation END AS after_wal_frame
FROM snapshots WHERE cycle IS NULL ORDER BY position;

.print
.print 'A2. The writer commits a batch of steps, then the shipper runs one cycle.'
.print '    Each batch becomes one segment. The checkpoint copies every frame, so'
.print '    the next commit restarts the WAL: every segment is a new generation'
.print '    and starts at frame 1.'

SELECT c.cycle,
       (SELECT string_agg(split_part(s.action, ' ', 2), ' ' ORDER BY s.step)
        FROM steps s WHERE s.next_cycle = c.cycle AND s.child = 'writer') AS steps_committed,
       c.segment,
       c.first_position || '-' || c.last_position         AS positions,
       c.first_frame || '-' || c.last_frame               AS frames,
       c.generation,
       c.first_user_version || '-' || c.last_user_version AS user_version,
       'copied ' || c.checkpointed || ' of ' || c.checkpoint_log AS checkpoint
FROM cycles c WHERE c.part = 'A' ORDER BY c.cycle;

.print
.print 'A3. After every third segment the shipper also stores a snapshot of the'
.print '    state that segment leaves:'

SELECT cycle, snapshot, reason, position, pages, user_version, state_sha256,
       after_frame || ' of ' || generation AS after_wal_frame
FROM snapshots WHERE part = 'A' AND cycle IS NOT NULL ORDER BY cycle;

.print
.print '── B. Phase 2: the shipper is down while another process checkpoints ──'
.print
.print 'B1. With the shipper closed, the writer commits 15 and 16, the outsider'
.print '    runs a TRUNCATE checkpoint (it copies them into the database file and'
.print '    empties the WAL), and the writer commits 17.'

SELECT step, child, action, outcome FROM steps WHERE part = 'B' AND next_cycle = 9 ORDER BY step;

.print
.print 'B2. The shipper reopens from the store, which ends at position 14:'

SELECT started, position FROM shipper_starts WHERE part = 'B' ORDER BY position;

.print
.print 'B3. Its first cycle finds a WAL header it never shipped. It cannot know'
.print '    what was committed in between, so it ships no segment and stores a'
.print '    new snapshot instead. That snapshot holds step 17 at position 15.'

SELECT cycle, wal, coalesce(segment, 'none') AS segment,
       'copied ' || checkpointed || ' of ' || checkpoint_log AS checkpoint
FROM cycles WHERE part = 'B' AND segment IS NULL ORDER BY cycle;

SELECT cycle, snapshot, reason, position, pages, user_version, state_sha256,
       after_frame || ' of ' || generation AS after_wal_frame
FROM snapshots WHERE part = 'B' ORDER BY cycle;

.print
.print 'B4. Shipping resumes from the new snapshot: steps 18-20 become positions'
.print '    16-18. From here on the position and the recipe step differ by 2.'

SELECT c.cycle,
       (SELECT string_agg(split_part(s.action, ' ', 2), ' ' ORDER BY s.step)
        FROM steps s WHERE s.next_cycle = c.cycle AND s.child = 'writer') AS steps_committed,
       c.segment,
       c.first_position || '-' || c.last_position         AS positions,
       c.first_frame || '-' || c.last_frame               AS frames,
       c.generation,
       c.first_user_version || '-' || c.last_user_version AS user_version,
       'copied ' || c.checkpointed || ' of ' || c.checkpoint_log AS checkpoint
FROM cycles c WHERE c.part = 'B' AND c.segment IS NOT NULL ORDER BY c.cycle;

.print
.print 'B5. Each segment names the database state it starts from and the state'
.print '    it leaves. Every segment starts from the state the store held just'
.print '    before it: the previous segment, or the snapshot after the gap.'

WITH states AS (
    SELECT cycle, segment AS object, after_sha256 AS sha256 FROM cycles WHERE segment IS NOT NULL
    UNION ALL
    SELECT coalesce(cycle, 0), snapshot, state_sha256 FROM snapshots
)
SELECT c.cycle, c.segment, c.before_sha256, c.after_sha256,
       (SELECT object FROM states s WHERE s.cycle < c.cycle
        ORDER BY s.cycle DESC, s.object LIMIT 1)       AS starts_from,
       c.before_sha256 = (SELECT sha256 FROM states s WHERE s.cycle < c.cycle
                          ORDER BY s.cycle DESC, s.object LIMIT 1) AS links
FROM cycles c WHERE c.segment IS NOT NULL ORDER BY c.cycle;

.print
.print '── C. Restore every position ───────────────────────────────────────────'
.print
.print 'C1. What the store holds (every object passed its checks):'

SELECT snapshots, snapshot_positions, segments, damaged FROM store ORDER BY snapshots;

.print
.print 'C2. Each position restored from the newest snapshot at or before it, then'
.print '    compared with the recipe built to the same step. Every restore is'
.print '    byte-identical (E1), and none replays more than 3 segments.'

SELECT position, snapshot, segments, user_version, sha256, recipe_sha256, e1_equal
FROM restores ORDER BY position;

.print
.print 'C3. The same positions restored from every older snapshot. Before the'
.print '    discontinuity the older snapshots give the same bytes; after it no'
.print '    segment chain leads from them to position 15 or later, and the plan'
.print '    refuses.'

SELECT position, snapshot AS newest_snapshot, older_snapshots, older_same_bytes, older_cannot_reach
FROM restores ORDER BY position;

.print
.print 'C4. Recipe steps with no restore point: the two that were committed and'
.print '    checkpointed away while the shipper was down.'

SELECT step AS recipe_step
FROM range(2, 21) t(step)
WHERE step NOT IN (SELECT user_version FROM restores)
ORDER BY step;

.print
.print 'C5. The newest position written out as a file, beside the live database'
.print '    once every process has closed it. Both are recipe step 20, byte for byte.'

SELECT file, position, integrity_check, recipe_step, exact_bytes FROM final ORDER BY position NULLS LAST;

.print
.print '── D. Damaged stores ───────────────────────────────────────────────────'
.print
.print 'D1. Three copies of the store, each damaged one way. Every restore is'
.print '    refused while planning, before any page is applied.'

SELECT seq, damage, restore_to, refused_while_planning FROM tampered ORDER BY seq;

.print
.print 'D2. Why each was refused. Every error is walship.ErrBrokenChain; each'
.print '    further line is the cause of the line above it.'

WITH parts AS (SELECT seq, damage, string_split(error, ': ') AS p FROM tampered)
SELECT CASE WHEN i = 1 THEN damage ELSE '' END AS damage,
       repeat('  ', i - 1) || p[i]           AS error
FROM parts, range(1, 10) r(i)
WHERE i <= len(p)
ORDER BY seq, i;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 9.4
```

Add `-i` (`go run ./cmd/course run -i 9.4`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/9.4-point-in-time-rebuild/`](evidence/9.4-point-in-time-rebuild/), and `go run ./cmd/course check evidence ch09` compares a fresh run with them byte for byte.

<!-- include: evidence/9.4-point-in-time-rebuild.txt -->
```text

════════════════════════════════════════════════════════════════════════
  9.4  Point-in-time rebuild from a snapshot and WAL segments
════════════════════════════════════════════════════════════════════════

The recipe database is built to step 2. Two child processes, writer and
outsider, each hold a connection (wal_autocheckpoint=0); the writer commits
every step on a fresh connection. A shipper (internal/walship) copies each
new run of committed WAL frames to a store as one segment, then runs a
PASSIVE checkpoint, and stores a snapshot every 3 segments. A stream
position counts the commits the shipper has shipped.

── A. Phase 1: shipping steps 3-14 ─────────────────────────────────────

A1. The shipper opens on an empty store and takes the initial snapshot.
┌────────────────────────┬─────────┬──────────┬───────┬──────────────┬──────────────────┬───────────────────┐
│        snapshot        │ reason  │ position │ pages │ user_version │   state_sha256   │  after_wal_frame  │
├────────────────────────┼─────────┼──────────┼───────┼──────────────┼──────────────────┼───────────────────┤
│ snapshots/000000000002 │ initial │ 2        │ 29    │ 2            │ 69490a2dc9c1980f │ none (no WAL yet) │
└────────────────────────┴─────────┴──────────┴───────┴──────────────┴──────────────────┴───────────────────┘

A2. The writer commits a batch of steps, then the shipper runs one cycle.
    Each batch becomes one segment. The checkpoint copies every frame, so
    the next commit restarts the WAL: every segment is a new generation
    and starts at frame 1.
┌───────┬─────────────────┬───────────────────────┬───────────┬────────┬────────────┬──────────────┬─────────────────┐
│ cycle │ steps_committed │        segment        │ positions │ frames │ generation │ user_version │   checkpoint    │
├───────┼─────────────────┼───────────────────────┼───────────┼────────┼────────────┼──────────────┼─────────────────┤
│ 1     │ 3               │ segments/000000000003 │ 3-3       │ 1-18   │ #1         │ 3-3          │ copied 18 of 18 │
│ 2     │ 4 5             │ segments/000000000004 │ 4-5       │ 1-36   │ #2         │ 4-5          │ copied 36 of 36 │
│ 3     │ 6               │ segments/000000000006 │ 6-6       │ 1-22   │ #3         │ 6-6          │ copied 22 of 22 │
│ 4     │ 7 8 9           │ segments/000000000007 │ 7-9       │ 1-62   │ #4         │ 7-9          │ copied 62 of 62 │
│ 5     │ 10              │ segments/000000000010 │ 10-10     │ 1-17   │ #5         │ 10-10        │ copied 17 of 17 │
│ 6     │ 11 12           │ segments/000000000011 │ 11-12     │ 1-44   │ #6         │ 11-12        │ copied 44 of 44 │
│ 7     │ 13              │ segments/000000000013 │ 13-13     │ 1-22   │ #7         │ 13-13        │ copied 22 of 22 │
│ 8     │ 14              │ segments/000000000014 │ 14-14     │ 1-18   │ #8         │ 14-14        │ copied 18 of 18 │
└───────┴─────────────────┴───────────────────────┴───────────┴────────┴────────────┴──────────────┴─────────────────┘

A3. After every third segment the shipper also stores a snapshot of the
    state that segment leaves:
┌───────┬────────────────────────┬──────────┬──────────┬───────┬──────────────┬──────────────────┬─────────────────┐
│ cycle │        snapshot        │  reason  │ position │ pages │ user_version │   state_sha256   │ after_wal_frame │
├───────┼────────────────────────┼──────────┼──────────┼───────┼──────────────┼──────────────────┼─────────────────┤
│ 3     │ snapshots/000000000006 │ periodic │ 6        │ 60    │ 6            │ 2d7996f5c596c99e │ 22 of #3        │
│ 6     │ snapshots/000000000012 │ periodic │ 12       │ 118   │ 12           │ 649990133c619869 │ 44 of #6        │
└───────┴────────────────────────┴──────────┴──────────┴───────┴──────────────┴──────────────────┴─────────────────┘

── B. Phase 2: the shipper is down while another process checkpoints ──

B1. With the shipper closed, the writer commits 15 and 16, the outsider
    runs a TRUNCATE checkpoint (it copies them into the database file and
    empties the WAL), and the writer commits 17.
┌──────┬──────────┬─────────────────────┬───────────────────────────────────────┐
│ step │  child   │       action        │                outcome                │
├──────┼──────────┼─────────────────────┼───────────────────────────────────────┤
│ 13   │ writer   │ recipe-session 15   │ SQLITE_OK                             │
│ 14   │ writer   │ recipe-session 16   │ SQLITE_OK                             │
│ 15   │ outsider │ checkpoint TRUNCATE │ SQLITE_OK busy=0 log=0 checkpointed=0 │
│ 16   │ writer   │ recipe-session 17   │ SQLITE_OK                             │
└──────┴──────────┴─────────────────────┴───────────────────────────────────────┘

B2. The shipper reopens from the store, which ends at position 14:
┌──────────────────────────────────┬──────────┐
│             started              │ position │
├──────────────────────────────────┼──────────┤
│ reopened: resumed from the store │ 14       │
└──────────────────────────────────┴──────────┘

B3. Its first cycle finds a WAL header it never shipped. It cannot know
    what was committed in between, so it ships no segment and stores a
    new snapshot instead. That snapshot holds step 17 at position 15.
┌───────┬───────────────────────────────────────────────────────────────────────────┬─────────┬─────────────────┐
│ cycle │                                    wal                                    │ segment │   checkpoint    │
├───────┼───────────────────────────────────────────────────────────────────────────┼─────────┼─────────────────┤
│ 9     │ discontinuity: the WAL header changed while the shipper held no read mark │ none    │ copied 13 of 13 │
└───────┴───────────────────────────────────────────────────────────────────────────┴─────────┴─────────────────┘
┌───────┬────────────────────────┬───────────────┬──────────┬───────┬──────────────┬──────────────────┬─────────────────┐
│ cycle │        snapshot        │    reason     │ position │ pages │ user_version │   state_sha256   │ after_wal_frame │
├───────┼────────────────────────┼───────────────┼──────────┼───────┼──────────────┼──────────────────┼─────────────────┤
│ 9     │ snapshots/000000000015 │ discontinuity │ 15       │ 162   │ 17           │ 4a1f971701e2b5cd │ 13 of #9        │
└───────┴────────────────────────┴───────────────┴──────────┴───────┴──────────────┴──────────────────┴─────────────────┘

B4. Shipping resumes from the new snapshot: steps 18-20 become positions
    16-18. From here on the position and the recipe step differ by 2.
┌───────┬─────────────────┬───────────────────────┬───────────┬────────┬────────────┬──────────────┬─────────────────┐
│ cycle │ steps_committed │        segment        │ positions │ frames │ generation │ user_version │   checkpoint    │
├───────┼─────────────────┼───────────────────────┼───────────┼────────┼────────────┼──────────────┼─────────────────┤
│ 10    │ 18              │ segments/000000000016 │ 16-16     │ 1-25   │ #10        │ 18-18        │ copied 25 of 25 │
│ 11    │ 19 20           │ segments/000000000017 │ 17-18     │ 1-36   │ #11        │ 19-20        │ copied 36 of 36 │
└───────┴─────────────────┴───────────────────────┴───────────┴────────┴────────────┴──────────────┴─────────────────┘

B5. Each segment names the database state it starts from and the state
    it leaves. Every segment starts from the state the store held just
    before it: the previous segment, or the snapshot after the gap.
┌───────┬───────────────────────┬──────────────────┬──────────────────┬────────────────────────┬───────┐
│ cycle │        segment        │  before_sha256   │   after_sha256   │      starts_from       │ links │
├───────┼───────────────────────┼──────────────────┼──────────────────┼────────────────────────┼───────┤
│ 1     │ segments/000000000003 │ 69490a2dc9c1980f │ 329eb15614fee435 │ snapshots/000000000002 │ true  │
│ 2     │ segments/000000000004 │ 329eb15614fee435 │ 4078a0bbc3758ed2 │ segments/000000000003  │ true  │
│ 3     │ segments/000000000006 │ 4078a0bbc3758ed2 │ 2d7996f5c596c99e │ segments/000000000004  │ true  │
│ 4     │ segments/000000000007 │ 2d7996f5c596c99e │ ca22a3f016ee31e4 │ segments/000000000006  │ true  │
│ 5     │ segments/000000000010 │ ca22a3f016ee31e4 │ 044c77909ad64db8 │ segments/000000000007  │ true  │
│ 6     │ segments/000000000011 │ 044c77909ad64db8 │ 649990133c619869 │ segments/000000000010  │ true  │
│ 7     │ segments/000000000013 │ 649990133c619869 │ cf469938e5222774 │ segments/000000000011  │ true  │
│ 8     │ segments/000000000014 │ cf469938e5222774 │ e7f84f8b8cd28ab5 │ segments/000000000013  │ true  │
│ 10    │ segments/000000000016 │ 4a1f971701e2b5cd │ 055a4f7a088f1c76 │ snapshots/000000000015 │ true  │
│ 11    │ segments/000000000017 │ 055a4f7a088f1c76 │ e769f5c3b1663de2 │ segments/000000000016  │ true  │
└───────┴───────────────────────┴──────────────────┴──────────────────┴────────────────────────┴───────┘

── C. Restore every position ───────────────────────────────────────────

C1. What the store holds (every object passed its checks):
┌───────────┬────────────────────┬──────────┬─────────┐
│ snapshots │ snapshot_positions │ segments │ damaged │
├───────────┼────────────────────┼──────────┼─────────┤
│ 4         │ 2 6 12 15          │ 10       │ 0       │
└───────────┴────────────────────┴──────────┴─────────┘

C2. Each position restored from the newest snapshot at or before it, then
    compared with the recipe built to the same step. Every restore is
    byte-identical (E1), and none replays more than 3 segments.
┌──────────┬──────────┬──────────┬──────────────┬──────────────────┬──────────────────┬──────────┐
│ position │ snapshot │ segments │ user_version │      sha256      │  recipe_sha256   │ e1_equal │
├──────────┼──────────┼──────────┼──────────────┼──────────────────┼──────────────────┼──────────┤
│ 2        │ 2        │ 0        │ 2            │ 69490a2dc9c1980f │ 69490a2dc9c1980f │ true     │
│ 3        │ 2        │ 1        │ 3            │ 329eb15614fee435 │ 329eb15614fee435 │ true     │
│ 4        │ 2        │ 2        │ 4            │ e6aa5f4e37fde0be │ e6aa5f4e37fde0be │ true     │
│ 5        │ 2        │ 2        │ 5            │ 4078a0bbc3758ed2 │ 4078a0bbc3758ed2 │ true     │
│ 6        │ 6        │ 0        │ 6            │ 2d7996f5c596c99e │ 2d7996f5c596c99e │ true     │
│ 7        │ 6        │ 1        │ 7            │ 7d8ae978254856c0 │ 7d8ae978254856c0 │ true     │
│ 8        │ 6        │ 1        │ 8            │ f27921ff12ecd673 │ f27921ff12ecd673 │ true     │
│ 9        │ 6        │ 1        │ 9            │ ca22a3f016ee31e4 │ ca22a3f016ee31e4 │ true     │
│ 10       │ 6        │ 2        │ 10           │ 044c77909ad64db8 │ 044c77909ad64db8 │ true     │
│ 11       │ 6        │ 3        │ 11           │ 6ced94850b7f17c8 │ 6ced94850b7f17c8 │ true     │
│ 12       │ 12       │ 0        │ 12           │ 649990133c619869 │ 649990133c619869 │ true     │
│ 13       │ 12       │ 1        │ 13           │ cf469938e5222774 │ cf469938e5222774 │ true     │
│ 14       │ 12       │ 2        │ 14           │ e7f84f8b8cd28ab5 │ e7f84f8b8cd28ab5 │ true     │
│ 15       │ 15       │ 0        │ 17           │ 4a1f971701e2b5cd │ 4a1f971701e2b5cd │ true     │
│ 16       │ 15       │ 1        │ 18           │ 055a4f7a088f1c76 │ 055a4f7a088f1c76 │ true     │
│ 17       │ 15       │ 2        │ 19           │ e57cb4d4d450456a │ e57cb4d4d450456a │ true     │
│ 18       │ 15       │ 2        │ 20           │ e769f5c3b1663de2 │ e769f5c3b1663de2 │ true     │
└──────────┴──────────┴──────────┴──────────────┴──────────────────┴──────────────────┴──────────┘

C3. The same positions restored from every older snapshot. Before the
    discontinuity the older snapshots give the same bytes; after it no
    segment chain leads from them to position 15 or later, and the plan
    refuses.
┌──────────┬─────────────────┬─────────────────┬──────────────────┬────────────────────┐
│ position │ newest_snapshot │ older_snapshots │ older_same_bytes │ older_cannot_reach │
├──────────┼─────────────────┼─────────────────┼──────────────────┼────────────────────┤
│ 2        │ 2               │ 0               │ 0                │ 0                  │
│ 3        │ 2               │ 0               │ 0                │ 0                  │
│ 4        │ 2               │ 0               │ 0                │ 0                  │
│ 5        │ 2               │ 0               │ 0                │ 0                  │
│ 6        │ 6               │ 1               │ 1                │ 0                  │
│ 7        │ 6               │ 1               │ 1                │ 0                  │
│ 8        │ 6               │ 1               │ 1                │ 0                  │
│ 9        │ 6               │ 1               │ 1                │ 0                  │
│ 10       │ 6               │ 1               │ 1                │ 0                  │
│ 11       │ 6               │ 1               │ 1                │ 0                  │
│ 12       │ 12              │ 2               │ 2                │ 0                  │
│ 13       │ 12              │ 2               │ 2                │ 0                  │
│ 14       │ 12              │ 2               │ 2                │ 0                  │
│ 15       │ 15              │ 3               │ 0                │ 3                  │
│ 16       │ 15              │ 3               │ 0                │ 3                  │
│ 17       │ 15              │ 3               │ 0                │ 3                  │
│ 18       │ 15              │ 3               │ 0                │ 3                  │
└──────────┴─────────────────┴─────────────────┴──────────────────┴────────────────────┘

C4. Recipe steps with no restore point: the two that were committed and
    checkpointed away while the shipper was down.
┌─────────────┐
│ recipe_step │
├─────────────┤
│ 15          │
│ 16          │
└─────────────┘

C5. The newest position written out as a file, beside the live database
    once every process has closed it. Both are recipe step 20, byte for byte.
┌──────────────────────────────────────────────┬──────────┬─────────────────┬─────────────┬─────────────┐
│                     file                     │ position │ integrity_check │ recipe_step │ exact_bytes │
├──────────────────────────────────────────────┼──────────┼─────────────────┼─────────────┼─────────────┤
│ restored.db                                  │ 18       │ ok              │ 20          │ true        │
│ the live database after every process closed │          │                 │ 20          │ true        │
└──────────────────────────────────────────────┴──────────┴─────────────────┴─────────────┴─────────────┘

── D. Damaged stores ───────────────────────────────────────────────────

D1. Three copies of the store, each damaged one way. Every restore is
    refused while planning, before any page is applied.
┌─────┬────────────────────────────────┬────────────┬────────────────────────┐
│ seq │             damage             │ restore_to │ refused_while_planning │
├─────┼────────────────────────────────┼────────────┼────────────────────────┤
│ 1   │ segment 7 deleted              │ 9          │ true                   │
│ 2   │ segments 10 and 11 swapped     │ 11         │ true                   │
│ 3   │ one byte of segment 13 flipped │ 13         │ true                   │
└─────┴────────────────────────────────┴────────────┴────────────────────────┘

D2. Why each was refused. Every error is walship.ErrBrokenChain; each
    further line is the cause of the line above it.
┌────────────────────────────────┬─────────────────────────────────────────────────────┐
│             damage             │                        error                        │
├────────────────────────────────┼─────────────────────────────────────────────────────┤
│ segment 7 deleted              │ segment chain is broken                             │
│                                │   no segment starts at position 7                   │
│ segments 10 and 11 swapped     │ segment chain is broken                             │
│                                │   the segment starting at position 10 is damaged    │
│                                │     malformed walship object                        │
│                                │       key names first position 10, metadata says 11 │
│ one byte of segment 13 flipped │ segment chain is broken                             │
│                                │   the segment starting at position 13 is damaged    │
│                                │     malformed walship object                        │
│                                │       body sha256 does not match its metadata       │
└────────────────────────────────┴─────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  steps [3] shipped as one segment of 1 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [4 5] shipped as one segment of 2 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [6] shipped as one segment of 1 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [7 8 9] shipped as one segment of 3 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [10] shipped as one segment of 1 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [11 12] shipped as one segment of 2 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [13] shipped as one segment of 1 commit(s), then the shipper's checkpoint copied every frame
A  ✓  steps [14] shipped as one segment of 1 commit(s), then the shipper's checkpoint copied every frame
A  ✓  every segment came from a new WAL generation: the WAL restarted after each shipper checkpoint (8 generations)
B  ✓  the outsider's TRUNCATE checkpoint copied steps 15-16 into the database file and emptied the WAL
B  ✓  the restarted shipper sees a WAL header it did not ship, cannot vouch for what happened in between, and takes a new snapshot (it holds step 17)
C  ✓  position 2 restores byte-identical to recipe step 2 from every snapshot that reaches it
C  ✓  position 3 restores byte-identical to recipe step 3 from every snapshot that reaches it
C  ✓  position 4 restores byte-identical to recipe step 4 from every snapshot that reaches it
C  ✓  position 5 restores byte-identical to recipe step 5 from every snapshot that reaches it
C  ✓  position 6 restores byte-identical to recipe step 6 from every snapshot that reaches it
C  ✓  position 7 restores byte-identical to recipe step 7 from every snapshot that reaches it
C  ✓  position 8 restores byte-identical to recipe step 8 from every snapshot that reaches it
C  ✓  position 9 restores byte-identical to recipe step 9 from every snapshot that reaches it
C  ✓  position 10 restores byte-identical to recipe step 10 from every snapshot that reaches it
C  ✓  position 11 restores byte-identical to recipe step 11 from every snapshot that reaches it
C  ✓  position 12 restores byte-identical to recipe step 12 from every snapshot that reaches it
C  ✓  position 13 restores byte-identical to recipe step 13 from every snapshot that reaches it
C  ✓  position 14 restores byte-identical to recipe step 14 from every snapshot that reaches it
C  ✓  position 15 restores byte-identical to recipe step 17 from every snapshot that reaches it
C  ✓  position 16 restores byte-identical to recipe step 18 from every snapshot that reaches it
C  ✓  position 17 restores byte-identical to recipe step 19 from every snapshot that reaches it
C  ✓  position 18 restores byte-identical to recipe step 20 from every snapshot that reaches it
C  ✓  a restore from the newest snapshot never replays more than 3 segments (worst here: 3)
C  ✓  only steps 15 and 16 have no restore point: they were committed and checkpointed away while the shipper was down
C  ✓  the newest restore point, written to a file, opens, passes integrity_check and equals the live database's final state (step 20)
D  ✓  segment 7 deleted: refused while planning, before any page is applied
D  ✓  segments 10 and 11 swapped: refused while planning, before any page is applied
D  ✓  one byte of segment 13 flipped: refused while planning, before any page is applied

34 of 34 claims hold

Experiment verdict: POINT IN TIME OK
```

#### Reading the Experiment 9.4 evidence

- **A1 (initial snapshot):** Opening the shipper against an empty store at recipe step `2` captures `snapshots/000000000002` (reason `initial`; `29` pages, `user_version` `2`, state `69490a2dc9c1980f`, after WAL frame `none (no WAL yet)`).
- **A2–A3 (Phase 1 — continuous shipping across 8 checkpoints and 8 WAL generations):** The writer commits steps `3–14` in batches `{3}`, `{4,5}`, `{6}`, `{7,8,9}`, `{10}`, `{11,12}`, `{13}`, `{14}`. After every batch, `Cycle` finds a new WAL generation after the shipper's full checkpoint, publishes one segment chained by `before`/`after` state hashes (for example, segment `segments/000000000004` covers positions `4–5`, frames `1–36` of WAL generation `#2`, before `329eb15614fee435`, after `4078a0bbc3758ed2`; B5 lists the chain), runs a `PASSIVE` checkpoint that copies every shipped frame (`copied 36 of 36`), and writes periodic snapshots every `3` segments: `snapshots/000000000006` (`60` pages, state `2d7996f5c596c99e`) and `snapshots/000000000012` (`118` pages, state `649990133c619869`). All 8 segments come from 8 distinct WAL generations because the writer restarted the WAL after each full backfill.
- **B1–B5 (Phase 2 — shipper stopped + outside `TRUNCATE` checkpoint):** While the shipper is closed, the writer commits steps `15` and `16`, `outsider` runs `checkpoint TRUNCATE` (`busy=0 log=0 checkpointed=0`), folding steps `15–16` into `.db` and emptying `-wal`, and the writer commits step `17`. When the shipper reopens (resumed from the store at position `14`), `Cycle` sees WAL generation `#9` without a held read mark (`discontinuity: the WAL header changed while the shipper held no read mark`), ships no segment, checkpoints its `13` frames, and publishes `snapshots/000000000015` (reason `discontinuity`; `162` pages, `user_version` `17`, state `4a1f971701e2b5cd`). Steps `18` and `19–20` then ship normally as segments `16` (WAL generation `#10`, `25` frames) and `17–18` (WAL generation `#11`, `36` frames).
- **C1–C5 (point-in-time restore across all 17 positions):** The store holds `4` snapshots (`2 6 12 15`), `10` segments, and `0` damaged objects. Every position `2..18` restores byte-identical (**E1 equal `true`**) to `recipe.StepHashes` for its `user_version`. Every older snapshot before the discontinuity restores identical bytes for positions `2..14`, and all `3` pre-discontinuity snapshots (`2`, `6`, `12`) refuse to cross the gap to positions `15..18` (`older_cannot_reach` `3` in C3). Worst-case replay from the newest snapshot is `3` segments (`== snapshotEvery`). Only steps `15` and `16` lack restore points (C4). Writing position `18` to `restored.db` yields `integrity_check` `ok` and exact E1 bytes equal to the closed live database (step `20`, `e769f5c3b1663de2`).
- **D1–D2 (damaged stores refused at plan time):** Deleting segment `7` (`no segment starts at position 7`), swapping segments `10` and `11` (`key names first position 10, metadata says 11`), and flipping one byte of segment `13` (`body sha256 does not match its metadata`) are each rejected with `ErrBrokenChain` during `Catalog.Plan`, before any page is replayed. All 34 checks pass (verdict `POINT IN TIME OK`).

---

### 6.3 Version notes

From the pinned-tree research inventory ([`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md) §9.9) and [`OUTLINE.md`](../OUTLINE.md):

- **SQLite 3.40.0:** `VACUUM INTO` honors `PRAGMA synchronous` on the output file.
- **SQLite 3.41.0:** Deserialized databases report an empty filename `""`.
- **SQLite 3.44.0+:** `PRAGMA integrity_check` also invokes virtual table `xIntegrity` methods (such as FTS5 and R-Tree).
- **SQLite 3.47.0 / 3.50.0 / 3.50.1 / 3.53.0:** `sqlite3_rsync` was added in 3.47.0 for consistent live database replication; 3.50.0 dropped its WAL-only requirement, 3.50.1 fixed a last-page off-by-one bug, and 3.53.0 added `-p`/`--port`.
- **SQLite 3.51.0:** Added `SQLITE_CHECKPOINT_NOOP`.
- **SQLite 3.51.3 / 3.53.0:** Included the WAL-reset corruption fix already present in our pinned `3.53.4`.
- **SQLite 3.53.0:** `VACUUM INTO 'file:x?reserve=N'` sets reserve bytes on the output file (`src/vacuum.c:250-257`).

---

### What attacked this chapter

<!-- adversarial-results -->

Four adversarial programs (`40` attacks total, driven by [`run-all.sh`](../agent_work/ch09/adversarial/run-all.sh)) attacked Chapter 9's experiments and `internal/walship` against SQLite `3.53.4`:

- [`capture-falsify`](../agent_work/ch09/adversarial/capture-falsify/main.go) (`CAP-01`–`CAP-07`): concurrent stress of all four live capture methods under an active writer and `PASSIVE` checkpoints (`32/32` exact recipe matches); reproducing CLI `.backup` `"Error: database is locked"` under fresh-process WAL writers (`CAP-02a`: `9/40` to `11/40` locked without `.timeout`), isolating `sqlite3WalClose`'s last-connection `EXCLUSIVE` lock (`CAP-02b`: `0/40` locked when one idle connection stays open), and verifying `.timeout 5000` mitigation (`CAP-02c`); testing same-connection write + `ROLLBACK` (`CAP-03a`: restarts backup via `src/pager.c:3185`) vs empty `BEGIN IMMEDIATE; COMMIT` (`CAP-03b`: no restart); concurrent `VACUUM INTO` overlapping writer commits (`CAP-04`: `5/5` exact steps, `0` writer errors); `file + WAL` under a held read transaction torn mid-copy by a `PASSIVE` checkpoint (`CAP-05`: E1 rebuild of step `10`); must-fail torn captures (`CAP-06a`: split `sqlite_dbpage` across two read transactions -> `40` integrity problems; `CAP-06b`: file+WAL across a `TRUNCATE` checkpoint without a read transaction -> `7` integrity problems); and `sqlitex.BackupWithSourceExec` error boundaries (`CAP-07`).
- [`crash-power-falsify`](../agent_work/ch09/adversarial/crash-power-falsify/main.go) (`CRASH-01`–`CRASH-07`): `1,200` real `SIGKILL` iterations at seeded random delays (`400` each at `OFF`, `NORMAL`, `FULL`; `1200/1200` recovered in `[lastAck, lastAck+1]` with `integrity_check ok`, `0` failures); `22/22` `strace`-injected `SIGKILL`s on `.db` backfill `pwrite64` and `ftruncate` calls inside `PASSIVE` and `TRUNCATE` checkpoints (`CRASH-02`); `10/10` single-kills plus a consecutive double-kill inside `walIndexRecover` (`CRASH-03`: always recovered step `11`); `15/15` mid-header and mid-frame WAL cuts matching `walfile.Replay` byte for byte (`CRASH-04`); `NORMAL` power cut keeping a synced generation-2 header over stale generation-1 frames (`CRASH-05`: salt mismatch stopped recovery at frame `1`, recovering step `7`); `NORMAL` commits after a reader-pinned partial `PASSIVE` checkpoint (`CRASH-06`); and `trace.SyncsOf` edge cases (`CRASH-07`).
- [`integrity-sweep`](../agent_work/ch09/adversarial/integrity-sweep/main.go) (`INT-01`–`INT-04`): a seeded sweep of `500` uniform random 1-bit flips across the `483,328`-byte step-12 database (`INT-01`: `integrity_check` caught `8/500` = `1.6%`, `quick_check` caught `2/500` = `0.4%`, E3 caught `445/500` = `89.0%`, and the page manifest caught `500/500` = `100.0%` naming the exact page every time); targeted flips in an overflow page payload (`INT-02a`, missed by `integrity_check`/`quick_check`), unallocated table-leaf free space (`INT-02b`), freelist trunk unused tail (`INT-02c`), and reserved database header bytes (`INT-02d`, all three caught **only** by the page manifest); a coordinated `account_id` `54 -> 55` flip in **both** the table leaf (page `66`) and index leaf (page `89`) passing `integrity_check` with `ok` while caught by E3 and the manifest (`INT-03`); and boundary validation of `dbfile.RecordColumn` and `capture.VerifyImage` (`INT-04`).
- [`walship-falsify`](../agent_work/ch09/adversarial/walship-falsify/main.go) (`SHIP-01a`–`SHIP-07`): `strace`-injected `SIGKILL`s during `walship.Open` (`4/4`) and `walship.Cycle` (`8/8`, covering segment puts, `PASSIVE` checkpoint writes/syncs, and periodic snapshot puts) all resuming cleanly with `0` `.tmp` leaks and E1 restore; outside checkpoints behind a live shipper pinned on slot `0` (`SHIP-02a`) and slot `>= 1` (`SHIP-02b`), plus an outside `RESTART` checkpoint while down (`SHIP-02c`); six store-tampering attacks (`SHIP-03a`–`SHIP-03f`: raw body flip, recomputed body hash caught by WAL checksum chain, forged checksum chain caught by `AfterSHA256` during `Execute`, forged non-commit `CommitPoint.Frame`, damaged periodic snapshot falling back to snapshot `2`, and damaged latest segment on resume); multi-statement transaction atomicity plus spilled `ROLLBACK` and crashed uncommitted spills (`SHIP-04`); shrinking database via `DELETE` + plain `VACUUM` (`483,328 -> 20,480` bytes, `SHIP-05`); page sizes `1024` and `8192` plus `ErrNotWAL` (`SHIP-06`); and 8-way concurrent `DirStore.PutIfAbsent` races (`50/50` rounds, `0` `.tmp` leaks) plus `WriteDatabase` sidecar refusals (`SHIP-07`).

**Run 1** ([`results-run1.txt`](../agent_work/ch09/adversarial/results-run1.txt)) produced **`35 HELD, 5 BROKEN`** (`CAP-02a`, `CAP-03a`, `CRASH-06`, `SHIP-03d`, `SHIP-03f`). Alongside the independent code review ([`REVIEW-1.md`](../agent_work/ch09/REVIEW-1.md)), the repair round ([`REPAIR-REPORT.md`](../agent_work/ch09/REPAIR-REPORT.md)) fixed three real defects exposed by the attacks and seven code-review findings:

- **`CRASH-06` (fixed):** Experiment 9.2's original portable `NORMAL` rule always cut at byte `32`, which failed when a concurrent reader prevented a `PASSIVE` checkpoint from backfilling every frame and prevented WAL restart. `ruleCut` was fixed to cut at the checkpoint's WAL end (`304,912`) when the WAL stayed in the same generation and to predict unsynced `.db` writes on partial checkpoints (`src/wal.c:2277, 2328-2335`); Experiment 9.2 gained its fourth model run demonstrating this directly.
- **`SHIP-03d` & Review Finding 3 (fixed):** `VerifySegmentFrames` did not validate the 32-byte WAL header or match frame commit sizes one-to-one against `meta.Commits`, allowing a forged non-commit `CommitPoint.Frame` to pass `LoadCatalog` and `Plan` and fail only in `Execute`. Fixed so both malformations are quarantined as `Damaged` before any page is applied.
- **`SHIP-03f` (fixed):** `walship.Open` silently rewound past a corrupted latest segment (`segments/000000000008`) to position `7`. Fixed so `resume` fails closed with `ErrBrokenChain` whenever any damaged object has a key position past `LastPosition()`.
- **Review Findings 1, 2, 4, 5, 6, 7 (fixed):** `Shipper.classify` now hashes live pages via `ReadPages` when resuming with no WAL so offline commits whose `-wal` was deleted trigger a discontinuity snapshot instead of silent corruption (Finding 1); Experiment 9.1 Part B now checks exact equality levels on every capture, checks the stale plain copy explicitly, and runs `VACUUM INTO` while the writer holds an uncommitted write transaction (Finding 2); a tautological WAL-movement check in `snapshot` was removed (Finding 4); measuring `E1-after-VACUUM-INTO` directly for every capture exposed that the backup API's canonical form differs from the source's at byte `43` (Finding 5); `Prune` returns only keys deleted before an error (Finding 6); and `DecodeSnapshot` rejects impossible page sizes/counts while `walship.Open` matches `"wal"` case-insensitively (Finding 7).

**Run 2** ([`results-run2.txt`](../agent_work/ch09/adversarial/results-run2.txt)) passed `SHIP-03d` and `SHIP-03f` (**`37 HELD, 3 BROKEN`**). The remaining three lines narrowed claims or reflected an attack script that still hardcoded the old rule:

- **`CAP-02a`** (narrowed/confirmed): CLI `.backup` without `.timeout` hit `Error: database is locked` (`5–11` of `40` attempts across runs) against fresh closing WAL writers, while `CAP-02b` (`0/40` with one persistent connection) and `CAP-02c` (`.timeout 5000` waits and succeeds) proved `sqlite3WalClose`'s last-connection `EXCLUSIVE` lock is the cause. Section 3.1 states this explicitly.
- **`CAP-03a`** (narrowed): same-connection write + `ROLLBACK` reset `copied` to `[5 10 5 10]` via `src/pager.c:3185`, while `CAP-03b` confirmed an empty `BEGIN IMMEDIATE; COMMIT` does not restart. Section 3.1 and Experiment 9.1 state plainly that same-connection **commits**—not rollbacks—avoid restarting a backup.
- **`CRASH-06`** (aligned to the fixed rule): Run 2 still hardcoded `ruleCut = 32` in the attack script; after aligning the expectations in `CAP-02a`, `CAP-03a`, and `CRASH-06` to the verified behavior, the final run ([`results.txt`](../agent_work/ch09/adversarial/results.txt) and [`results-run3.txt`](../agent_work/ch09/adversarial/results-run3.txt)) passed **`40 HELD, 0 BROKEN`**.

---

## 7. Platform implications

Every rule below follows directly from this chapter's experiments and adversarial runs:

1. **Choose the capture method by the equality level and writer concurrency you need (Experiment 9.1; `CAP-01`–`CAP-06b`).**
   - Need **E1 bytes** on a live database without blocking writers: read every page through `sqlite_dbpage` in one read transaction, or copy `.db` then `-wal` under a held read transaction and replay.
   - Need a compact canonical copy without blocking writers: use `VACUUM INTO` (`E3` except `journal_mode`, E1 between canonical forms), and switch the restored file back to `PRAGMA journal_mode=wal`.
   - Avoid stepped `sqlite3_backup_step` on busy multi-writer databases: every external commit restarts it from page `1` (starving indefinitely in Experiment 9.1), it reaches only **E2** (not E1 even after `VACUUM INTO`, because of byte `43`'s schema cookie), and without a busy timeout it fails with `SQLITE_BUSY` if a closing last connection triggers `sqlite3WalClose` (`CAP-02a/c`).
   - Never copy a live WAL `.db` file alone: Experiment 9.1 showed it produces a valid but silently **stale** database (`step 6` while the writer was at `step 7`).
2. **State durability in terms of `fsync`, never "committed" (Experiment 9.2; `CRASH-01`–`CRASH-06`).** A process kill lost zero acknowledged commits at `OFF`, `NORMAL`, and `FULL` (`36` syscall kills in 9.2 plus `1,200` random `SIGKILL`s in `CRASH-01`, `22` mid-checkpoint kills in `CRASH-02`, and `10` mid-recovery kills in `CRASH-03`). Power loss is governed strictly by `fsync`: at `NORMAL` the power-cut model lost all `4` acknowledged commits written since the last checkpoint (`321,360` WAL bytes in the restarted run); at `FULL` it lost `0`; at `OFF` the database file itself received `44` unsynced checkpoint writes, leaving the outcome outside any file-level guarantee.
3. **Treat the acknowledgement rule as a fleet-wide platform decision (Experiment 9.2; Experiment 7.2).** A single host losing power drops the unsynced tail of **every** database on that host at once. At `synchronous=NORMAL`, every database loses all commits since its last checkpoint; at `synchronous=FULL`, every commit pays one additional `-wal` `fsync` (`11` vs `3` WAL syncs across `8` commits in Experiment 9.2). A platform therefore chooses explicitly between **local-durable** acknowledgement (acknowledge after `COMMIT` at `WAL + synchronous=FULL`) and **store-durable** acknowledgement (acknowledge only after `walship` puts the segment into durable object storage)—the two durability modes tested in Chapter 10.
4. **Never accept `PRAGMA integrity_check` as proof of restored content (Experiment 9.3; `INT-01`–`INT-03`).** Across `500` random 1-bit flips in `INT-01`, `integrity_check` caught only `1.6%` (`8/500`) and `quick_check` caught `0.4%` (`2/500`), missing unindexed column payloads (`events.note`), overflow payloads (`INT-02a`), freelist pages, unallocated page space, and even coordinated table+index flips (`INT-03`). Prove captures and restores with cryptographic hashes—either a per-page manifest (`capture.VerifyImage`, `100%` detection in `INT-01`) or `walship`'s per-commit whole-image `StateSHA256` chain.
5. **Continuous point-in-time capture (rung R6) requires three SQLite invariants and one store invariant (Experiment 9.4; `internal/walship`; `SHIP-01a`–`SHIP-07`).**
   - Disable `wal_autocheckpoint` (`= 0`) on every writer connection so only the shipper checkpoints.
   - Hold the shipper's read mark between cycles (`pin` connection) and run shipper-owned `PASSIVE` checkpoints only after shipping.
   - Read `-wal` directly via `os.ReadFile` (lock-safe inside the owning process; never stray-open/close `.db` or `-shm`).
   - Back `walship.Store` with atomic, exclusive `PutIfAbsent`, read-after-write and list-after-write visibility, and idempotent `Delete`, never relying on `List` sort order.
6. **Guard against unowned checkpoints and bound restore cost (Experiment 9.4 Phase 2; `SHIP-02c`, `SHIP-03e`).** Any checkpoint run while the shipper is stopped folds intermediate WAL frames into `.db`, permanently removing those individual commit points (`steps 15` and `16` in Experiment 9.4). The shipper detects the unvouched WAL change (or changed page hash when no WAL exists) on resume and re-bases with a discontinuity snapshot so restore chains never silently skip commits. Taking a periodic snapshot every `m` segments bounds restore replay to at most `m` segments (`3` in Experiment 9.4) and lets `Catalog.Plan` fall back to an older snapshot if a periodic snapshot is damaged (`SHIP-03e`).
7. **Budget for the shipper's write-lock window and add ownership fencing above `walship` (`internal/walship/shipper.go`).** Each `Shipper.Cycle` holds `BEGIN IMMEDIATE` while reading `-wal`, putting one object to the store, and running one `PASSIVE` checkpoint; concurrent writers wait in `busy_timeout` for that duration. Because `walship` assumes a single shipper per database, preventing two nodes from shipping the same database concurrently requires an external epoch/fencing control plane—the subject of Chapter 10.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 9.1 Part B, why did `sqlite3_backup_remaining` **rise** from `90` to `98` across two same-connection commits even though the backup did not restart, why did the writer child's commit drop `copied` back to `5`, and why does a same-connection `ROLLBACK` (`CAP-03a`) behave differently from a same-connection `COMMIT`?
2. In Experiment 9.1 Part A, why does a backup-API copy reach **E2** (bytes `27 43 95`) yet show **E1-after-VACUUM-INTO `false`**, and why does `VACUUM INTO` of a WAL database show E3 `false` yet E3-except-journal_mode `true` and E1-after-VACUUM-INTO `true`?
3. In Experiment 9.2, why does killing a `synchronous=FULL` writer on entry to `fsync #3` recover **step 4** when only step `3` was acknowledged, and why did adding a concurrent reader from step `5` change the `synchronous=NORMAL` power-cut offset from byte `32` to byte `304,912` while leaving `15` unsynced writes in `.db` that did not break recovery?
4. In Experiment 9.3, why did `PRAGMA integrity_check` return `ok` on the flipped `events.note` byte and freelist leaf page, why did `PRAGMA quick_check` also return `ok` on the flipped `events.account_id` byte, and why did `integrity_check` report **`row 66`** when the mutated row had `rowid = 76`?
5. In `internal/walship` and Experiment 9.4, why can the shipper safely read `-wal` with `os.ReadFile` inside the same process that holds SQLite connections, why can a read mark on slot `0` allow the WAL to restart after every cycle without losing unshipped frames, and why must `Shipper.classify` hash live pages when resuming with no WAL?

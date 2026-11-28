# Chapter 5 — WAL, transactions, and checkpoints

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `wal.c`, `wal.h` |
| Linked C sources (in the pinned tree, not copied) | `pager.c`, `main.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to invoke this repository's experiment runner (`cmd/course`) from the host. Build the `lab` container image (`Dockerfile.lab`) once from the repository root:
>   ```text
>   make docker-build
>   ```
>   The image bundles the pinned SQLite `3.53.4` source tree, the compiled SQLite C tools (`sqlite3`, `showwal`—SQLite's WAL dump utility—and `dbhash`; Experiment 5.1 cross-checks its Go parser against `showwal`), `strace`, and the DuckDB CLI that renders each experiment's CSV results as a walkthrough.
> - **Run an experiment** from the repository root. Each command starts the `lab` container, runs the experiment inside it, and prints the rendered DuckDB walkthrough; add `-i` (`make run EXP=5.1 ARGS='-i'`) to stay in the DuckDB shell afterwards with all tables loaded:
>   ```text
>   make run EXP=5.1
>   make run EXP=5.2
>   make run EXP=5.3
>   make run EXP=5.4
>   ```
>   Inside the container, scratch databases and result tables are written under `/work/5.1/` through `/work/5.4/` (the Docker `lab-work` volume). When run directly on the host, they go under `_work/5.1/` through `_work/5.4/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** Each experiment is an ordinary Go program (`make run-native EXP=5.1`). Run directly on the host, it prints only its verdict and leaves its CSV tables under `_work/5.1/results/`. Experiment 5.1's `showwal` cross-check then needs the C tools on the host (`make fetch-source` to clone and verify the pinned SQLite source tree, then `make build-tools` to compile the C tools); without `showwal`, Experiment 5.1 still runs, prints a `NO-SHOWWAL:` line, and skips that cross-check.
> - **Cross-platform notes and macOS:** Nothing in Chapter 5 is Linux-only (no `strace`, no `/proc`). All four experiments copy live files by re-executing themselves as a child process and parse bytes in plain Go, so they also run directly on macOS.
> - **Measured platform:** The stored evidence in this chapter was captured in the Debian `lab` container on `linux/arm64` (Go 1.26). WAL salts, the checksums derived from them, and checkpoint timings vary across runs: `course run` prints them in run-dependent `info` tables, and they are excluded from the stored evidence.

---

## 1. Model

In **write-ahead log (WAL)** mode (`PRAGMA journal_mode=WAL`), a write transaction does not overwrite pages in the main database file (`.db`). Instead, SQLite appends each modified database **page** (4,096 bytes by default) to a sibling redo log file named `<db>-wal`. Each entry in `-wal` is a **frame**: a 24-byte frame header followed by one complete **page image**.

Every frame carries two 32-bit **salt** values copied from the 32-byte WAL header and continues a two-word **running checksum** seeded from that header. A transaction commits when its final frame records the database's new size in pages (a non-zero **commit size** field); earlier frames in that transaction store `0`. Frames written before a commit—either because a large transaction spilled dirty pages out of the page cache (Chapter 4) or because the transaction rolled back—stay in `-wal` with no following commit frame and are ignored during recovery.

Scanning the entire `-wal` file on every page read would be slow. SQLite therefore maintains a shared-memory lookup table called the **wal-index**, mapped on Unix onto a second sibling file named `<db>-shm`. The `-shm` file is transient and uses the host's native byte order: if it is missing or damaged, SQLite rebuilds it by scanning `-wal`.

```text
Base database (.db)          Write-ahead log (.db-wal)            Wal-index (.db-shm, native endian)
+----------------------+     +------------------------------+     +-----------------------------------+
| Page 1 (base image)  |     | [0..31] 32-byte WAL header   |     | [0..47]   WalIndexHdr copy 1      |
| Page 2               |     |   magic, version 3007000,    |     | [48..95]  WalIndexHdr copy 2      |
| ...                  |     |   pageSize, ckptSeq,         |     |   mxFrame, nPage, aSalt, aCksum   |
| Page M               |     |   salt-1, salt-2, checksum   |     | [96..135] WalCkptInfo             |
+----------------------+     +------------------------------+     |   nBackfill, aReadMark[0..4],     |
           ^                 | Frame 1 (offset 32):         |     |   aLock[0..7] (bytes 120..127),   |
           |                 |   [0..23] pgno, commit=0,    |     |   DMS byte 128, nBackfillAttempt  |
           |  checkpoint     |           salts, checksum    |     +-----------------------------------+
           |  (backfill      |   [24..]  page image (4096 B)|     | Block 0 (32 KiB):                 |
           |   frames        +------------------------------+     |   [136..16383] 4062 page numbers  |
           |   nBackfill+1   | ...                          |     |   [16384..32767] 8192-slot hash   |
           |   ..mxSafe)     +------------------------------+     +-----------------------------------+
           |                 | Frame k: pgno, commit=N (>0) |     | Block 1+ (32 KiB each):           |
           +-----------------|   <- commit boundary         |     |   4096 page numbers + hash table  |
                             +------------------------------+     +-----------------------------------+

Reader snapshot:
  - If nBackfill == mxFrame: takes WAL_READ_LOCK(0) shared -> ignores -wal, reads .db only.
  - Otherwise: picks/sets aReadMark[i] <= mxFrame (i in 1..4), holds WAL_READ_LOCK(i) shared ->
    for each page P, takes newest frame <= its mxFrame in -shm, else reads P from .db.
Writer (one at a time, WAL_WRITE_LOCK):
  - Appends frames after mxFrame, or restarts at frame 1 if nBackfill == mxFrame and locks 1..4 are free.
Checkpoint (one at a time, WAL_CKPT_LOCK):
  - Copies frames nBackfill+1..mxSafeFrame to .db, where mxSafeFrame <= every locked aReadMark[1..4],
    after taking WAL_READ_LOCK(0) exclusively so no .db-only reader sees overwritten pages.
```

Five mechanisms in that diagram govern everything that follows:

1. **Snapshot isolation via `mxFrame` and read marks.** When a read transaction starts, SQLite records the current `mxFrame`—the frame number of the last committed transaction. That integer is the reader's **snapshot**: when looking up page *P*, the reader accepts only frames `<= mxFrame` and ignores every frame appended after it began. A **checkpoint** copies committed WAL frames back into `.db` (called **backfilling**). To keep a checkpoint from overwriting a `.db` page the reader still needs, the reader holds a shared **POSIX advisory lock** (an `fcntl` byte-range lock on `-shm` bytes 120–127) on one of five reader slots `0..4`. Slots `1..4` store frame numbers called **read marks** (`aReadMark[1..4]`); slot `0` is reserved for readers that began when the WAL was already completely backfilled and therefore read only `.db`.
2. **One writer, concurrent readers.** Only one writer may hold `WAL_WRITE_LOCK` at a time. Readers do not block writers from appending frames, and writers do not block readers from reading older snapshots.
3. **Checkpoints copy frames without necessarily shrinking `-wal`.** A checkpoint advances `nBackfill` up to the lowest locked read mark (`mxSafeFrame`), calls `fsync` on `-wal` before writing `.db`, copies the newest frame for each page in `nBackfill+1..mxSafeFrame` into `.db`, and calls `fsync` on `.db` once the entire WAL has been copied. Leaving `-wal` at its current file size is normal; only a `TRUNCATE` checkpoint, `PRAGMA journal_size_limit`, or closing the last connection shrinks or removes `-wal`.
4. **WAL restart happens at the next write.** Copying all frames (`nBackfill == mxFrame`) does not empty or reset `-wal` in `PASSIVE`, `FULL`, or `RESTART` mode. When the next writer commits and no reader holds locks `1..4`, the writer performs a **WAL restart**: it writes a fresh 32-byte WAL header with new salts and overwrites the file from frame 1. Older-generation frames sitting past the new ones fail the salt check and are ignored.
5. **The `-shm` file is disposable; `-wal` is not.** While any connection is open, committed changes may exist only in `-wal`. By contrast, `-shm` is pure derived state: deleting it or replacing it with a stale `-shm` from another database causes the first opener to truncate and rebuild it from `-wal`.

**WAL mode as the platform default.** Platform databases in this book run in WAL mode (`journal_mode=wal`), and every experiment in this chapter uses WAL mode without departing into rollback mode. Nothing measured here argues against WAL as the default, but WAL shifts concrete responsibilities onto the platform: owning checkpoints (`wal_autocheckpoint=0` on application connections), bounding read-transaction lifetimes, treating `-wal` as part of the database while open, and never shipping `-shm`. A WAL platform must also monitor WAL size and checkpoint progress, detect WAL restarts by any change in `(salt-1, salt-2)`, and keep databases on local storage because WAL requires shared memory (`src/wal.c:130-133`, a source-level constraint not tested here). Section 7 ties each obligation to its experiment.

**The Rebuild Ladder (rung R3).** Chapters 1–3 built **R0 (Recipe)**, **R1 (Pages)**, and **R2 (Canonical form)**. This chapter builds **R3 (Redo log)**: capture the base `.db` file plus its `-wal` file, then rebuild in plain Go (`internal/walfile`, introduced in Section 5.1) by applying committed frames in order, stopping at `LastCommitFrame`, and truncating to that commit's database size. Experiment 5.2 proves that the rebuilt file matches SQLite's own `wal_checkpoint(TRUNCATE)` at equality level **E1 bytes** (identical file SHA-256, introduced in Chapter 2).

---

## 2. Question

In WAL mode, where exactly does a committed transaction live, what keeps a reader's snapshot stable, and what must a checkpoint do before the WAL can be reused?

Four experiments answer that question against live files: Experiment 5.1 traces five commits into a WAL and `-shm`; Experiment 5.2 replays a base file plus WAL in plain Go at a quiet moment, mid-transaction with spilled pages, and after a rollback; Experiment 5.3 holds one reader open while a writer commits 24 steps with autocheckpoint enabled; and Experiment 5.4 runs all four checkpoint modes against three reader states.

The experiments reuse three shared Go packages introduced in Chapters 1 and 2 (linked rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/) (Chapter 1): opens SQLite with explicit file and connection settings, checks the pinned `sqlite_source_id()`, manages `_work/N.M/` scratch directories, runs `PRAGMA wal_checkpoint`, and locates pinned C tools (`showwal`).
- [`internal/recipe`](../internal/recipe/) (Chapter 1): builds the deterministic R0 database (`accounts`, `documents`, `events`, `events_by_account`), applies numbered steps, and matches rebuilt files against the cached per-step SHA-256 table.
- [`internal/equal`](../internal/equal/) (Chapter 2): compares rebuilt database files at equality level **E1 bytes** (`CompareFiles`, `FileSHA256`).

This chapter introduces one new shared package, `internal/walfile` (WAL parser, region diff, frame replayer, `-shm` wal-index decoder, `showwal` cross-check, and child-process live file copier), inlined in full in Section 5.

---

## 3. Call path

This section walks the pinned SQLite `version-3.53.4` C source for WAL formatting, checksums, read marks, frame writes, sync flags, checkpoints, and recovery. Two C source files are copied whole into this chapter's directory (`wal.c`, `wal.h`, checksummed in `SOURCES.sha256` against the pinned tree); `pager.c` and `main.c` are linked in the pinned SQLite source tree (`$SQLITE_SRC` inside the `lab` container or `_src/sqlite-3.53.4` on the host).

### 3.1 WAL format, checksums, and recovery

1. **In-source WAL specification** — [`src/wal.c:16-249`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L16-L249) (local copy: [`wal.c`](wal.c)), covering the file format, [`READER ALGORITHM` (`src/wal.c:100-126`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L100-L126), and [`WAL-INDEX FORMAT` (`src/wal.c:127-249`)](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L127-L249), alongside the header size and magic constants at [`src/wal.c:477-491`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L477-L491):
   - A WAL starts with a 32-byte header (`WAL_HDRSIZE = 32`) of eight big-endian 32-bit unsigned integers: magic (`0x377f0682` for little-endian checksum words or `0x377f0683` for big-endian checksum words), format version (`3007000`), database page size, checkpoint sequence number, `salt-1`, `salt-2`, `checksum-1`, and `checksum-2`.
   - Every frame starts with a 24-byte header (`WAL_FRAME_HDRSIZE = 24`) of six big-endian 32-bit unsigned integers: page number, commit database size in pages (`0` for non-commit frames, `> 0` on the last frame of a committed transaction), `salt-1`, `salt-2`, `checksum-1`, and `checksum-2`, followed immediately by one database page of `pageSize` bytes.
   - `src/wal.c:130-133` notes that the wal-index lives in shared memory (an `mmap`-ed `-shm` file on Unix), which is why WAL mode requires local shared memory and does not work across network filesystems.
2. **`walChecksumBytes`** — [`src/wal.c:856-912`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L856-L912) (local copy: [`wal.c`](wal.c)). Computes SQLite's cumulative Fletcher-like checksum over pairs of 32-bit words (`nByte` must be a multiple of 8):
   ```c
   s1 += *aData++ + s2;
   s2 += *aData++ + s1;
   ```
   When bit 0 of the WAL magic disagrees with host endianness (`!nativeCksum`), lines 877–882 byte-swap each 32-bit word before adding. The resulting `s1` and `s2` are always stored big-endian in the WAL file.
3. **`walEncodeFrame` and `walDecodeFrame`** — [`src/wal.c:969-993`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L969-L993) and [`src/wal.c:1000-1053`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L1000-L1053) (local copy: [`wal.c`](wal.c)). `walEncodeFrame` writes the page number and commit size, copies both salts from `pWal->hdr.aSalt`, extends the running checksum over the first 8 bytes of the frame header and the page image, and writes the new checksum (while `pWal->iReCksum != 0`, salts and checksums are zeroed until `walRewriteChecksums` recomputes them at commit). `walDecodeFrame` validates in strict order: salts must match the WAL header (line 1015), page number must be non-zero (line 1022), and the running checksum must match bytes 16–23:
   ```c
   nativeCksum = (pWal->hdr.bigEndCksum==SQLITE_BIGENDIAN);
   walChecksumBytes(nativeCksum, aFrame, 8, aCksum, aCksum);
   walChecksumBytes(nativeCksum, aData, pWal->szPage, aCksum, aCksum);
   ```
   **Source-comment discrepancy:** the comment immediately above those lines (`src/wal.c:1032-1035`) claims the checksum covers "the first 16 bytes of this frame-header". The code at line 1038 (and `walEncodeFrame` at line 985) actually passes `8` bytes (`pgno` and `commit-size`), agreeing with the top format comment (`src/wal.c:65-68`). Our Go parser follows the code.
4. **`walIndexRecover`** — [`src/wal.c:1390-1614`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L1390-L1614) (local copy: [`wal.c`](wal.c)). Rebuilds `-shm` from `-wal` under exclusive locks. If the WAL header has a bad magic, an invalid page size, or a bad header checksum (lines 1447–1467), SQLite jumps to `finished` and treats the WAL as having zero valid frames. Only after the header checksum verifies does line 1472 check `version != WAL_MAX_VERSION` (`3007000`), returning `SQLITE_CANTOPEN_BKPT`. Line 1489 computes `iLastFrame = (nSize - WAL_HDRSIZE) / szFrame`, silently ignoring any trailing partial-frame bytes. The scan loop (lines 1502–1525) stops at the first frame rejected by `walDecodeFrame`, appends every valid frame to the wal-index, and advances `mxFrame` and `nPage` only when `nTruncate != 0` (a commit frame). At the end, it publishes the wal-index header, sets `nBackfill = 0`, and initializes `aReadMark[1] = mxFrame`.

### 3.2 Wal-index structures, read marks, and read/write locking

5. **Lock slots, `WalIndexHdr`, and `WalCkptInfo`** — [`src/wal.c:294-401`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L294-L401) (`WalIndexHdr` at [`src/wal.c:321-333`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L321-L333), `WalCkptInfo` at [`src/wal.c:394-401`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L394-L401), local copy: [`wal.c`](wal.c)). Defines the eight POSIX lock slots on `-shm` bytes 120–127: `0 = WAL_WRITE_LOCK`, `1 = WAL_CKPT_LOCK`, `2 = WAL_RECOVER_LOCK`, and `3+i = WAL_READ_LOCK(i)` for `i = 0..4` (`WAL_NREADER = 5`). Byte 128 is the dead-man-switch lock (`UNIX_SHM_DMS`): the first connection to open the database takes it exclusively, truncates `-shm`, and runs `walIndexRecover` so a stale `-shm` file on disk is never trusted. The 136-byte header region holds two 48-byte `WalIndexHdr` copies (bytes 0–47 and 48–95) followed by the 40-byte `WalCkptInfo` (bytes 96–135: `nBackfill`, `aReadMark[0..4]`, 8 lock bytes, `nBackfillAttempted`, `notUsed0`). Block 0 stores 4,062 page numbers (bytes 136–16,383) and an 8,192-slot `uint16` hash table (bytes 16,384–32,767); each subsequent 32 KiB block stores 4,096 page numbers plus an 8,192-slot hash table.
6. **`walIndexWriteHdr`** — [`src/wal.c:942-954`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L942-L954) (local copy: [`wal.c`](wal.c)). Sets `isInit = 1` and `iVersion = 3007000`, checksums the first 40 bytes in native byte order, writes copy 2 (`aHdr[1]`), executes a memory barrier (`walShmBarrier`), and then writes copy 1 (`aHdr[0]`). Readers read copy 1, issue a barrier, read copy 2, and compare all 48 bytes with `memcmp` (`walIndexTryHdr`) to detect torn header writes.
7. **`walTryBeginRead`** — [`src/wal.c:3020-3272`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3020-L3272) (local copy: [`wal.c`](wal.c)). Starts a read transaction and locks its snapshot:
   - **Database-only path (`WAL_READ_LOCK(0)`, lines 3134–3167):** When `nBackfill == pWal->hdr.mxFrame` (every WAL frame has been copied into `.db`), the reader acquires a shared lock on `WAL_READ_LOCK(0)`, re-checks that `WalIndexHdr` did not change, sets `pWal->readLock = 0`, and ignores the WAL completely. It sets no mark in `aReadMark[1..4]`.
   - **WAL-reading path (`WAL_READ_LOCK(1..4)`, lines 3174–3224):** Otherwise, the reader scans `aReadMark[1..4]` for the largest mark `<= mxFrame`. If no slot already equals `mxFrame`, it tries to take an exclusive lock on a free slot `i` in `1..4`, writes `aReadMark[i] = mxFrame`, and releases the exclusive lock. It then takes a shared lock on `WAL_READ_LOCK(mxI)` and verifies that neither the mark nor `WalIndexHdr` changed while locking.
   - **Stale marks stay in `-shm`:** Ending a read transaction releases the shared lock on `WAL_READ_LOCK(i)` without clearing `aReadMark[i]`. The old number stays in `-shm` until a later checkpoint or reader overwrites that slot.
8. **`sqlite3WalBeginWriteTransaction`** — [`src/wal.c:3699-3746`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3699-L3746) (local copy: [`wal.c`](wal.c)). Acquires `WAL_WRITE_LOCK` exclusively. If `WalIndexHdr` in `-shm` changed since this connection started its read transaction (line 3735), another writer committed first, so it releases `WAL_WRITE_LOCK` and returns `SQLITE_BUSY_SNAPSHOT`.

### 3.3 Writing frames, restarting the WAL, and sync flags

9. **`pagerWalFrames` and `pagerOpenWalIfPresent`** — [`src/pager.c:3225-3282`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3225-L3282) and [`src/pager.c:3385-3413`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3385-L3413) (in the pinned tree). `pagerOpenWalIfPresent` switches the pager into WAL mode whenever a `-wal` file sits beside the database (deleting an empty `-wal` beside an empty database). `pagerWalFrames` hands the pager's dirty-page list to `sqlite3WalFrames` with `walSyncFlags` and updates page 1's change counter only when page 1 is in that dirty list (as Chapter 2 proved).
10. **`walRestartLog` and `walRestartHdr`** — [`src/wal.c:3875-3915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3875-L3915) and [`src/wal.c:2152-2166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2152-L2166) (local copy: [`wal.c`](wal.c)). Called at the start of `walFrames`: if the writer holds `WAL_READ_LOCK(0)` (meaning `nBackfill == mxFrame`) and `nBackfill > 0`, `walRestartLog` tries to take exclusive locks on `WAL_READ_LOCK(1..4)` (line 3885)—notably *not* `WAL_READ_LOCK(0)`. If no reader holds slots `1..4`, it calls `walRestartHdr`, which increments the connection's `pWal->nCkpt`, resets `mxFrame = 0` and `nBackfill = 0`, increments `aSalt[0]` (`salt-1`) by 1, replaces `aSalt[1]` (`salt-2`) with a random value, publishes the new wal-index header, and resets `aReadMark[1] = 0` and `aReadMark[2..4] = 0xffffffff` (`READMARK_NOT_USED`, marking those slots empty).
11. **`walFrames`** — [`src/wal.c:4038-4266`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4038-L4266) (local copy: [`wal.c`](wal.c)). Calls `walRestartLog` (line 4079). When `iFrame == 0` (a fresh or restarted WAL), it writes the 32-byte WAL header and syncs it with `CKPT_SYNC_FLAGS` (unless `synchronous=OFF` or `SQLITE_IOCAP_SEQUENTIAL`, a VFS flag indicating sequential-write guarantees, is set):
    ```c
    sqlite3Put4byte(&aWalHdr[0], (WAL_MAGIC | SQLITE_BIGENDIAN));
    sqlite3Put4byte(&aWalHdr[4], WAL_MAX_VERSION);
    sqlite3Put4byte(&aWalHdr[8], szPage);
    sqlite3Put4byte(&aWalHdr[12], pWal->nCkpt);
    if( pWal->nCkpt==0 ) sqlite3_randomness(8, pWal->hdr.aSalt);
    memcpy(&aWalHdr[16], pWal->hdr.aSalt, 8);
    ```
    Lines 4095–4096 reveal a per-connection subtlety: `pWal->nCkpt` is **per connection**, updated only when that connection runs `walIndexRecover` (line 1456) or `walRestartHdr` (line 2156). If connection C runs `wal_checkpoint(TRUNCATE)` (calling `walRestartHdr` on C's handle and resetting `mxFrame = 0`), and then connection W—whose own `pWal->nCkpt` is still `0`—writes the next WAL header, line 4096 re-randomizes both salts and writes `checkpoint-seq = 0`. Only when the same connection that ran `walRestartHdr` writes the new header does `salt-1` advance by `+1` and `checkpoint-seq` advance by `+1`. Experiment 5.4 demonstrates both cases.

    Next, `walFrames` writes each dirty page (lines 4140–4173). If a page was already spilled to `-wal` earlier in the same open transaction, lines 4147–4163 overwrite its page data in place and record `pWal->iReCksum` so `walRewriteChecksums` (line 4177) recomputes all checksums at commit. On commit (`isCommit != 0`), the last frame gets `nTruncate` = database page count (line 4167). If `WAL_SYNC_FLAGS(sync_flags) != 0`, lines 4195–4214 sync `-wal` (padding to a sector boundary only when `padToSectorBoundary` is set). If this is the first commit after a restart and `journal_size_limit >= 0`, lines 4220–4227 call `walLimitSize` ([`src/wal.c:2401-2413`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2401-L2413)). Finally, lines 4235–4261 append frames to the wal-index and call `walIndexWriteHdr`, making the commit visible to new readers.
12. **WAL sync flags (`WAL_SYNC_FLAGS` and `CKPT_SYNC_FLAGS`)** — [`src/wal.h:25-26`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.h#L25-L26) (local copy: [`wal.h`](wal.h)) and [`src/pager.c:3658-3704`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3658-L3704) (`sqlite3PagerSetFlags`, in the pinned tree):
    ```c
    /* src/wal.h:25-26 */
    #define WAL_SYNC_FLAGS(X)   ((X)&0x03)
    #define CKPT_SYNC_FLAGS(X)  (((X)>>2)&0x03)

    /* src/pager.c:3692-3695 */
    pPager->walSyncFlags = (pPager->syncFlags<<2);
    if( pPager->fullSync ){
      pPager->walSyncFlags |= pPager->syncFlags;
    }
    ```
    The low two bits (`WAL_SYNC_FLAGS`) control commit syncs; bits 2–3 (`CKPT_SYNC_FLAGS`) control checkpoint (and fresh WAL header) syncs. Under `PRAGMA synchronous=NORMAL`, `pPager->fullSync` is `0`, so the low two bits stay `0`: SQLite syncs at checkpoints and not on steady-state commits. Under `FULL` or `EXTRA`, `fullSync` is `1`, so SQLite syncs `-wal` on every commit as well. **Source-comment bug:** the struct comment at [`src/pager.c:607-617`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L607-L617) describes those bitfields backwards (claiming the low two bits are for checkpoints and bits `0x04`/`0x08` are for commits). Always cite `wal.h:25-26` and `sqlite3PagerSetFlags`.

The sequence below contrasts both `TRUNCATE` paths from step 11—when a separate connection C runs `TRUNCATE` versus when writer W runs `TRUNCATE` itself—showing why per-connection `pWal->nCkpt` changes the next WAL header's salts and sequence number:

<!-- draw-visual: diagrams/ch05-wal-transactions-checkpoints-truncate-salts.mmd -->
```text
  ┌────────┐     ┌─────────────┐     ┌──────────┐
  │ Conn C │     │ -shm / -wal │     │ Writer W │
  └────┬───┘     └──────┬──────┘     └─────┬────┘
       │                │                  │
 ┌───────────────────────────────────────────────┐
 │ Case 1: Conn C runs TRUNCATE (both nCkpt = 0) │
 └───────────────────────────────────────────────┘
       │                │                  │
       │ 1. TRUNCATE    │                  │
       ├───────────────►│                  │
       │                │                  │
┌─────────────┐         │                  │
│ C.nCkpt = 1 │         │                  │
└─────────────┘         │                  │
       │                │                  │
       │    ┌───────────────────────┐      │
       │    │ shm salt1+1, -wal 0 B │      │
       │    └───────────────────────┘      │
       │                │                  │
       │                │ 2. write iFrame=0│
       │                │◄─────────────────┤
       │                │                  │
       │                │        ┌────────────────────┐
       │                │        │ W.nCkpt is still 0 │
       │                │        └────────────────────┘
       │                │                  │
       │                │ 3. random, seq=0 │
       │                │◄─────────────────┤
       │                │                  │
  ┌──────────────────────────────────────────────┐
  │ Case 2: Writer W runs TRUNCATE (W.nCkpt = 0) │
  └──────────────────────────────────────────────┘
       │                │                  │
       │                │ 4. TRUNCATE      │
       │                │◄─────────────────┤
       │                │                  │
       │                │           ┌─────────────┐
       │                │           │ W.nCkpt = 1 │
       │                │           └─────────────┘
       │                │                  │
       │                │ 5. write iFrame=0│
       │                │◄─────────────────┤
       │                │                  │
       │                │ 6. salt1+1, seq=1│
       │                │◄─────────────────┤
       │                │                  │
```

### 3.4 Checkpoints, autocheckpoint, and last close

13. **`sqlite3WalCheckpoint` and `walBusyLock`** — [`src/wal.c:4301-4432`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4301-L4432) and [`src/wal.c:2107-2125`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2107-L2125) (local copy: [`wal.c`](wal.c)). `SQLITE_CHECKPOINT_NOOP` takes no locks and only reads `mxFrame` and `nBackfill` from `-shm` (lines 4341, 4399–4403). All other modes first try `WAL_CKPT_LOCK` without invoking the busy handler (line 4342): if another checkpoint is already running, the call returns `SQLITE_BUSY` immediately with `log=-1, checkpointed=-1`. `FULL`, `RESTART`, and `TRUNCATE` then request `WAL_WRITE_LOCK` through `walBusyLock` (line 4358); if a writer holds it past the busy timeout, they fall back to `PASSIVE` backfill (`eMode2 = SQLITE_CHECKPOINT_PASSIVE`, lines 4361–4365) and still report `SQLITE_BUSY` at line 4431.
14. **`walCheckpoint`** — [`src/wal.c:2199-2395`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395) (local copy: [`wal.c`](wal.c)). Runs in three phases:
    - **Compute `mxSafeFrame` (lines 2233–2251):** Starts at `mxFrame` and loops over slots `1..4`. For each slot whose mark `y < mxSafeFrame`, it tries `walBusyLock(WAL_READ_LOCK(i))` (with `xBusy = 0` in `PASSIVE` mode). If unlocked, it updates slot `1` to `mxSafeFrame` and clears slots `2..4` to `READMARK_NOT_USED`; if locked by a reader, it lowers `mxSafeFrame = y` and disables further busy waits (`xBusy = 0`).
    - **Backfill pages under `WAL_READ_LOCK(0)` (lines 2254–2344):** Before copying a single page, line 2260 must take `WAL_READ_LOCK(0)` exclusively. If any database-only reader holds `WAL_READ_LOCK(0)`, no frames are copied at all (`nBackfill` stays `0`). Otherwise, it verifies the WAL salts did not wrap, sets `nBackfillAttempted = mxSafeFrame`, syncs `-wal` with `CKPT_SYNC_FLAGS` (line 2277), checks the corruption bound `nSize + 65536 + mxFrame*szPage < mxPage*szPage` (line 2288), copies the newest frame for each page `<= mxPage` in `nBackfill+1..mxSafeFrame` into `.db` (lines 2304–2323), truncates `.db` to `nPage * szPage` and syncs `.db` if all frames were copied (lines 2328–2335), and stores `nBackfill = mxSafeFrame` (line 2337).
    - **Mode tail (lines 2358–2389):** For `FULL` and above, if `nBackfill < mxFrame`, it returns `SQLITE_BUSY` (line 2362). For `RESTART` and `TRUNCATE`, once `nBackfill == mxFrame`, line 2367 calls `walBusyLock` for exclusive `WAL_READ_LOCK(1..4)` so no reader still uses the WAL. `RESTART` then releases those locks without touching the WAL header or size—leaving the restart itself to the next writer. Only `TRUNCATE` calls `walRestartHdr` and `sqlite3OsTruncate(pWal->pWalFd, 0)` (lines 2383–2384).
15. **`sqlite3WalDefaultHook` and `sqlite3WalClose`** — [`src/main.c:2484-2496`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L2484-L2496) (in the pinned tree) and [`src/wal.c:2507-2571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2507-L2571) (local copy: [`wal.c`](wal.c)). By default, every connection installs `sqlite3WalDefaultHook` with a threshold of 1,000 frames: after any commit where `nFrame >= N`, that committing connection runs `sqlite3_wal_checkpoint` (`PASSIVE`). On close, `sqlite3WalClose` tries to obtain an `EXCLUSIVE` lock on `.db` (line 2529). If it succeeds—meaning this is the last open connection—it runs a `PASSIVE` checkpoint (line 2534) and deletes `-wal` and `-shm` (lines 2559–2564), unless `SQLITE_FCNTL_PERSIST_WAL` (a file-control option that keeps `-wal` on disk after the last close) is set, in which case it truncates `-wal` to 0 when `journal_size_limit >= 0`.

Putting steps 13 and 14 together, the flow below shows how `NOOP`, `PASSIVE`, `FULL`, `RESTART`, and `TRUNCATE` branch across locks, backfill, and truncation:

<!-- draw-visual: diagrams/ch05-wal-transactions-checkpoints-checkpoint-flow.mmd -->
```text
┌─────────────────────────┐
│      wal_checkpoint     │
└────────────┬────────────┘
             │
             ├────────────────────────────────────┐
             │                                    │
           NOOP                               PASSIVE+
             │                                    │
             ▼                                    ▼
┌─────────────────────────┐             ┌───────────────────┐
│ Read mxFrame, nBackfill │      PASSIVE┤ Try WAL_CKPT_LOCK │
└─────────────────────────┘      │      └─────────┬─────────┘
                                 │                │
             ┌───────────────────┘              FULL+
             ▼                                    ▼
┌─────────────────────────┐             ┌───────────────────┐
│Scan marks 1..4 -> mxSafe│◄ok─or─busy──┤Wait WAL_WRITE_LOCK│
└────────────┬────────────┘             └───────────────────┘
             ▼
┌─────────────────────────┐
│   Excl WAL_READ_LOCK 0  │
└────────────┬────────────┘
             ▼
┌─────────────────────────┐
│ Sync -wal, backfill .db │
└────────────┬────────────┘
             │
         RESTART+
             ▼
┌─────────────────────────┐
│ Wait WAL_READ_LOCK 1..4 │
└────────────┬────────────┘
             │
         TRUNCATE
             ▼
┌─────────────────────────┐
│ walRestartHdr, -wal = 0 │
└─────────────────────────┘
```

Two experimental SQLite branches often mentioned online—**WAL2** and **`BEGIN CONCURRENT`**—are not present in SQLite 3.53.4.

### Version notes

Behavior differences across SQLite releases relevant to WAL mode (from this repository's C-source inventory [`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md), SQLite's release notes `changes.html`, WAL documentation `wal.html`, and the copied source):

- **`SQLITE_CHECKPOINT_NOOP` (added in 3.51.0, `changes.html`).** `PRAGMA wal_checkpoint(NOOP)` reports `(busy, log, checkpointed)` without taking `WAL_CKPT_LOCK` or copying frames; older releases do not recognize `NOOP`.
- **WAL-reset database corruption fix (3.51.3 and 3.53.0, `wal.html#walresetbug`).** Fixed a race that could corrupt a database across a WAL reset. Our 3.53.4 pin includes the fix; older 3.x builds bundled in other drivers may not.
- **Savepoint rollback with spilled WAL frames (3.50.2, `changes.html`).** Eliminated checksum-less frames left after rolling back a savepoint that spilled pages.
- **64 KiB page size in `WalIndexHdr.szPage` (3.7.1, `src/wal.c:318-319`).** Value `1` in the 16-bit `szPage` field represents 65,536 bytes.

### Experiment 5.1 — WAL inspector (`5.1-wal-inspector`)

- **Claim:** A WAL file consists of a 32-byte header followed by whole frames (each a 24-byte header plus one page image) linked by salts and a running checksum. A transaction commits on the frame carrying a non-zero commit size equal to `PRAGMA page_count`, and the frame count matches `PRAGMA wal_checkpoint(NOOP)`'s `log` column, `-shm`'s `mxFrame`, and `showwal`'s listing. Repeated runs—and different `synchronous` levels (`FULL`, `NORMAL`, `OFF`)—write the exact same `(page, page sha256, commit size)` frame sequence, differing only in salt and checksum bytes.
- **Evidence form:** `EXACT` (every stored CSV table must match byte for byte on every run; run-dependent `info` tables are excluded).
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the DuckDB walkthrough; add `-i` to stay in the DuckDB shell afterwards with all tables loaded):
  ```text
  make run EXP=5.1
  ```

<!-- include: experiments/5.1-wal-inspector/main.go -->
```go
// Experiment 5.1 (wal-inspector) parses a live SQLite WAL and its -shm
// wal-index in plain Go, verifies the running checksum chain and commit frames
// against PRAGMA wal_checkpoint(NOOP) and SQLite's showwal utility, and proves
// that repeated runs and different synchronous settings produce the exact same
// frame sequence, differing only in random salts and dependent checksums.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.1
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	firstStep = 1
	lastStep  = 5
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.1" runs both.
var (
	set    *results.Set
	tables struct {
		walHeader, frames, commits, noop, walIndex, showwal, regions, sequences *results.Table
		infoSalts, infoRegionBytes                                              *results.Table
	}
)

func openTables() {
	tables.walHeader = set.Table("wal_header", "magic", "checksum_order", "version", "page_size", "checkpoint_seq", "wal_bytes", "header_bytes", "frame_header_bytes", "frames", "valid_frames", "partial_bytes", "stop_reason")
	tables.frames = set.Table("frames", "frame_no", "page", "commit_size", "sha256_16")
	tables.commits = set.Table("commits", "commit_no", "recipe_step", "first_frame", "last_frame", "frames", "commit_size", "page_count")
	tables.noop = set.Table("noop_checkpoint", "busy", "log_frames", "checkpointed")
	tables.walIndex = set.Table("wal_index", "bytes", "byte_order", "headers_agree", "checksum_ok", "mx_frame", "n_page", "page_size", "n_backfill", "salts_equal_wal", "page_array_matches", "frames")
	tables.showwal = set.Table("showwal", "frames_listed", "disagreements", "problems")
	tables.regions = set.Table("regions", "seq", "region", "differs")
	tables.sequences = set.Table("sequences", "part", "run", "synchronous", "wal_bytes", "frames", "commits", "same_sequence", "first_difference")
	tables.infoSalts = set.Table("info_salts", "run", "salt1", "salt2", "header_checksum")
	tables.infoRegionBytes = set.Table("info_region_bytes", "seq", "region", "differing_bytes")
}

func check(part string, ok bool, claim string) {
	set.Check(part, ok, claim)
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

type run struct {
	name        string
	synchronous string
	wal         *walfile.File
	walBytes    []byte
	index       *walfile.Index
	noop        sqlitex.CheckpointResult
	pageCounts  []int
	databaseDir string
}

// buildRun creates a step-0 database, applies recipe steps 1..5 on one
// connection with autocheckpoint disabled, copies the live database, -wal, and
// -shm from a child process before closing, and parses both sidecar files.
func buildRun(ctx context.Context, dir, name, synchronous string) run {
	r := recipe.Default()
	runDir := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(runDir, "copy"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(runDir, "r.db")
	if err := r.Build(ctx, path, firstStep-1); err != nil {
		fatal("build step 0", err)
	}
	options := r.SQLiteOptions()
	// Disable autocheckpoint so every frame of steps 1..5 remains in the WAL
	// until the child copies it.
	options.Conn.WALAutocheckpoint = 0
	options.Conn.Synchronous = synchronous
	db := must(sqlitex.Open(ctx, path, options))("open")
	result := run{name: name, synchronous: synchronous, databaseDir: runDir}
	for k := firstStep; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal(fmt.Sprintf("step %d", k), err)
		}
		var pages int
		if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
			fatal("page_count", err)
		}
		result.pageCounts = append(result.pageCounts, pages)
	}
	// Copy while the connection is still open because closing the last
	// connection triggers a close checkpoint and deletes -wal and -shm
	// (src/wal.c:2528-2565, sqlite3WalClose). Copying in a child avoids
	// dropping this process's POSIX locks on -shm (src/os_unix.c:4703-4750).
	copies := must(walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(path, filepath.Join(runDir, "copy"), "r.db")))("live copy")
	for _, c := range copies {
		if !c.Present {
			fatal("live copy", fmt.Errorf("%s was missing while the connection was open", filepath.Base(c.From)))
		}
	}
	// NOOP takes no locks and reports (busy, mxFrame, nBackfill) from the
	// wal-index (src/wal.c:4341, 4399-4403).
	result.noop = must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointNoop))("NOOP checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	result.walBytes = must(os.ReadFile(filepath.Join(runDir, "copy", "r.db-wal")))("read WAL copy")
	result.wal = must(walfile.Parse(result.walBytes))("parse WAL")
	result.index = must(walfile.ParseIndexFile(filepath.Join(runDir, "copy", "r.db-shm")))("parse wal-index")
	return result
}

func shortHash(h string) string { return h[:16] }

func partA(ctx context.Context, dir string) run {
	first := buildRun(ctx, dir, "run-1", sqlitex.SyncFull)
	wal := first.wal
	tables.walHeader.Row(wal.Header.Magic, wal.Header.ChecksumByteOrder(), wal.Header.Version, wal.Header.PageSize, wal.Header.CheckpointSequence,
		wal.Size, walfile.HeaderSize, walfile.FrameHeaderSize, len(wal.Frames), wal.ValidFrames, wal.PartialFrameBytes, wal.StopReason())
	tables.infoSalts.Row("run 1", wal.Header.Salt1, wal.Header.Salt2, wal.Header.Checksum)
	check("A", wal.Size == walfile.HeaderSize+int64(len(wal.Frames))*int64(walfile.FrameHeaderSize+wal.PageSize()) && wal.PartialFrameBytes == 0,
		"the WAL is a 32-byte header plus whole frames of a 24-byte header and one 4096-byte page")
	for _, frame := range wal.Frames {
		tables.frames.Row(frame.Number, frame.Page, frame.CommitSize, shortHash(frame.PageSHA256))
	}
	saltsMatch := true
	for _, frame := range wal.Frames {
		saltsMatch = saltsMatch && frame.SaltsMatchHeader
	}
	check("A", wal.ValidFrames == len(wal.Frames) && saltsMatch, "every frame carries the header's salts and continues the running checksum chain")
	commits := wal.Commits()
	sizesMatch := len(commits) == len(first.pageCounts)
	for i, c := range commits {
		pageCount := -1
		if i < len(first.pageCounts) {
			pageCount = first.pageCounts[i]
		}
		tables.commits.Row(i+1, firstStep+i, c.FirstFrame, c.LastFrame, c.Frames(), c.Pages, pageCount)
		sizesMatch = sizesMatch && int(c.Pages) == pageCount
	}
	check("A", len(commits) == lastStep-firstStep+1, "five recipe steps left exactly five commit frames")
	check("A", sizesMatch, "each commit frame's size field equals the database's page count after that transaction")
	tables.noop.Row(first.noop.Busy, first.noop.LogFrames, first.noop.Checkpointed)
	check("A", first.noop.LogFrames == len(wal.Frames) && first.noop.Checkpointed == 0, "the frame count equals the log column of wal_checkpoint(NOOP); nothing was checkpointed")
	describeIndex(first)
	crossCheckShowwal(first)
	return first
}

func describeIndex(r run) {
	h := r.index.Header()
	wal := r.wal
	saltsEqual := h.Salt1 == wal.Header.Salt1 && h.Salt2 == wal.Header.Salt2
	pages, err := r.index.FramePages(h.MaxFrame)
	mappingMatches := err == nil && len(pages) == len(wal.Frames)
	for i := 0; mappingMatches && i < len(pages); i++ {
		mappingMatches = pages[i] == wal.Frames[i].Page
	}
	tables.walIndex.Row(r.index.Size, r.index.ByteOrderName(), r.index.HeadersAgree(), h.ChecksumOK, h.MaxFrame, h.Pages, h.PageSize(), r.index.Checkpoint.Backfill, saltsEqual, mappingMatches, len(pages))
	check("A", r.index.HeadersAgree() && h.ChecksumOK && int(h.MaxFrame) == wal.LastCommitFrame && h.Pages == wal.DatabasePages() &&
		saltsEqual && mappingMatches,
		"the -shm header names the last commit frame (mxFrame), the database size and the WAL's salts, and its page array maps every frame to its page")
}

func crossCheckShowwal(r run) {
	tool, err := sqlitex.LocateTool("showwal")
	if err != nil {
		fmt.Printf("NO-SHOWWAL: %v; cross-check skipped\n", err)
		return
	}
	out, err := exec.Command(tool, filepath.Join(r.databaseDir, "copy", "r.db-wal")).Output()
	if err != nil {
		fatal("showwal", err)
	}
	listing := must(walfile.ParseShowwal(string(out)))("parse showwal output")
	problems := r.wal.CompareWithShowwal(listing)
	tables.showwal.Row(len(listing.Frames), len(problems), strings.Join(problems, "; "))
	check("A", len(problems) == 0, "showwal and walfile agree on every frame's page, commit size, salts and stored checksums, and on which checksums verify")
}

func sequenceRow(part, name, synchronous string, other run, equal bool, at int) {
	tables.sequences.Row(part, name, synchronous, len(other.walBytes), len(other.wal.Frames), len(other.wal.Commits()), equal, at)
}

func partB(ctx context.Context, dir string, first run) {
	second := buildRun(ctx, dir, "run-2", sqlitex.SyncFull)
	tables.infoSalts.Row("run 2", second.wal.Header.Salt1, second.wal.Header.Salt2, second.wal.Header.Checksum)
	comparison := walfile.CompareBytes(first.walBytes, second.walBytes, first.wal.PageSize())
	for r := walfile.RegionHeaderMagic; r <= walfile.RegionBeyondShorterFile; r++ {
		tables.regions.Row(int(r), r.String(), comparison.ByRegion[r] > 0)
		tables.infoRegionBytes.Row(int(r), r.String(), comparison.ByRegion[r])
	}
	check("B", !comparison.Identical() && comparison.OnlyIn(walfile.RegionHeaderSalt, walfile.RegionHeaderChecksum, walfile.RegionFrameSalt, walfile.RegionFrameChecksum),
		"the two WALs differ, and only inside the salt and checksum fields")
	equal, at := walfile.EqualTuples(first.wal.Tuples(true), second.wal.Tuples(true))
	sequenceRow("B", "run 2", sqlitex.SyncFull, second, equal, at)
	check("B", equal, "the two runs wrote the same pages, with the same images, in the same order, with the same commit sizes")

	for _, level := range []string{sqlitex.SyncNormal, sqlitex.SyncOff} {
		other := buildRun(ctx, dir, "sync-"+strings.ToLower(level), level)
		equal, at := walfile.EqualTuples(first.wal.Tuples(true), other.wal.Tuples(true))
		sequenceRow("C", "sync-"+strings.ToLower(level), level, other, equal, at)
		check("C", equal, fmt.Sprintf("synchronous=%s changes when SQLite syncs, not which frames it writes", level))
	}
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	first := partA(ctx, dir)
	partB(ctx, dir, first)
	set.Finish("WAL INSPECTOR")
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

<!-- include: experiments/5.1-wal-inspector/report.sql -->
```sql
-- Walkthrough of experiment 5.1. Run it with:
--   go run ./cmd/course run 5.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   wal_header       one row: the WAL header and size of run 1 (recipe steps 1-5)
--   frames           one row per frame of run 1: page, commit size, first 16 hex digits of the page sha256
--   commits          one row per commit frame (one per recipe step)
--   noop_checkpoint  one row: PRAGMA wal_checkpoint(NOOP) on the open connection
--   wal_index        one row: the -shm copy (wal-index) of run 1
--   showwal          one row: the cross-check against SQLite's showwal utility (absent when skipped)
--   regions          one row per region of the WAL file: do run 1 and run 2 differ there
--   sequences        one row per further run: is its frame sequence equal to run 1
--   checks           every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.1  What is inside a live WAL, and what differs between two of them?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Recipe steps 1-5 are committed on one connection with wal_autocheckpoint=0,'
.print 'synchronous=FULL. While that connection is still open, a child process'
.print 'copies the database, the -wal and the -shm. Plain Go parses both sidecar'
.print 'files and checks them against PRAGMA wal_checkpoint(NOOP) and showwal.'
.print 'Base: recipe step 0, built and closed before the five steps.'

.print
.print '── A. One WAL holding recipe steps 1-5 ─────────────────────────────────'
.print
.print 'A1. The header and the size. The file is a 32-byte header plus whole frames'
.print '    of a 24-byte frame header and one page; the arithmetic must match.'

SELECT printf('0x%08x', magic) AS magic, checksum_order, version, page_size, checkpoint_seq, wal_bytes,
       header_bytes + frames * (frame_header_bytes + page_size) AS computed_bytes,
       partial_bytes
FROM wal_header;

.print
.print 'A2. The checksum chain: every frame carries the header salts and continues'
.print '    the running checksum, so all frames verify.'

SELECT frames, valid_frames, stop_reason FROM wal_header;

.print
.print 'A3. The frames in order, with the commit they belong to. Only the last frame'
.print '    of a transaction has a non-zero commit size, and it equals the database'
.print '    size in pages at that point. Later transactions rewrite early pages'
.print '    such as 1 and 2; an identical sha256 means the same page image.'

SELECT f.frame_no, f.page, f.commit_size, f.sha256_16,
       (SELECT min(c.commit_no) FROM commits c WHERE c.last_frame >= f.frame_no) AS commit_no,
       CASE WHEN f.commit_size > 0 THEN '<- commit' ELSE '' END AS marker
FROM frames f
ORDER BY f.frame_no;

.print
.print 'A4. The transactions. The commit size equals PRAGMA page_count after the step.'

SELECT *, commit_size = page_count AS sizes_match FROM commits ORDER BY commit_no;

.print
.print 'A5. PRAGMA wal_checkpoint(NOOP) on the open connection takes no locks and'
.print '    reports the wal-index counts: log is the frame count of the WAL, and nothing'
.print '    is checkpointed.'

SELECT n.busy, n.log_frames, n.checkpointed, (SELECT count(*) FROM frames) AS frames_in_wal
FROM noop_checkpoint n;

.print
.print 'A6. The wal-index (the -shm copy): both header copies agree, the checksum'
.print '    is good, mxFrame is the last commit frame, nPage the database size, and'
.print '    its page-number array maps every frame to the page the WAL holds.'

SELECT bytes, byte_order, headers_agree, checksum_ok, page_size
FROM wal_index;

.print
.print '    What the header says, against the WAL itself:'

SELECT w.mx_frame, w.n_page, w.n_backfill, w.salts_equal_wal,
       w.page_array_matches, w.frames AS array_frames
FROM wal_index w;

.print
.print 'A7. showwal (built from the pinned tree) against the Go parser: they agree on'
.print '    every frame page, commit size, salts and checksum, so disagreements is 0.'

SELECT frames_listed, disagreements, problems FROM showwal;

.print
.print '── B. The same workload again: what differs between two WALs ──────────'
.print
.print 'B1. Where run 1 and run 2 differ, by region of the file. Only the salts'
.print '    and the checksums that depend on them: the page data is the same.'
.print '    (The salts themselves are random; see the info report.)'

SELECT region, CASE WHEN differs THEN 'differs' ELSE 'same' END AS result
FROM regions
ORDER BY seq;

.print
.print 'B2. The two runs wrote the same frames: the sequence of (page, page sha256,'
.print '    commit size) is equal.'

SELECT run, synchronous, wal_bytes, frames, commits, same_sequence, first_difference
FROM sequences WHERE part = 'B';

.print
.print '── C. Other synchronous levels write the same frames ──────────────────'
.print
.print 'C1. synchronous changes when SQLite syncs, not which frames it writes.'
.print '    first_difference is the first frame that differs; 0 means none.'

SELECT run, synchronous, wal_bytes, frames, commits, same_sequence, first_difference
FROM sequences WHERE part = 'C' ORDER BY run;
```

`course run` also prints the run-dependent values excluded from the stored evidence: the random salts, the checksums derived from them, and how many bytes differ between run 1 and run 2.

Captured evidence: the walkthrough rendered by the `lab` container. Its backing CSV tables live in [`evidence/5.1-wal-inspector/`](evidence/5.1-wal-inspector/), and `make check ARGS='evidence ch05'` compares a fresh run against them byte for byte.

<!-- include: evidence/5.1-wal-inspector.txt -->
```text

════════════════════════════════════════════════════════════════════════
  5.1  What is inside a live WAL, and what differs between two of them?
════════════════════════════════════════════════════════════════════════

Recipe steps 1-5 are committed on one connection with wal_autocheckpoint=0,
synchronous=FULL. While that connection is still open, a child process
copies the database, the -wal and the -shm. Plain Go parses both sidecar
files and checks them against PRAGMA wal_checkpoint(NOOP) and showwal.
Base: recipe step 0, built and closed before the five steps.

── A. One WAL holding recipe steps 1-5 ─────────────────────────────────

A1. The header and the size. The file is a 32-byte header plus whole frames
    of a 24-byte frame header and one page; the arithmetic must match.
┌────────────┬────────────────┬─────────┬───────────┬────────────────┬───────────┬────────────────┬───────────────┐
│   magic    │ checksum_order │ version │ page_size │ checkpoint_seq │ wal_bytes │ computed_bytes │ partial_bytes │
├────────────┼────────────────┼─────────┼───────────┼────────────────┼───────────┼────────────────┼───────────────┤
│ 0x377f0682 │ little-endian  │ 3007000 │ 4096      │ 0              │ 387312    │ 387312         │ 0             │
└────────────┴────────────────┴─────────┴───────────┴────────────────┴───────────┴────────────────┴───────────────┘

A2. The checksum chain: every frame carries the header salts and continues
    the running checksum, so all frames verify.
┌────────┬──────────────┬─────────────┐
│ frames │ valid_frames │ stop_reason │
├────────┼──────────────┼─────────────┤
│ 94     │ 94           │ valid       │
└────────┴──────────────┴─────────────┘

A3. The frames in order, with the commit they belong to. Only the last frame
    of a transaction has a non-zero commit size, and it equals the database
    size in pages at that point. Later transactions rewrite early pages
    such as 1 and 2; an identical sha256 means the same page image.
┌──────────┬──────┬─────────────┬──────────────────┬───────────┬───────────┐
│ frame_no │ page │ commit_size │    sha256_16     │ commit_no │  marker   │
├──────────┼──────┼─────────────┼──────────────────┼───────────┼───────────┤
│ 1        │ 1    │ 0           │ 59ffcca063706e7c │ 1         │           │
│ 2        │ 2    │ 0           │ 1d3a999cc98c2840 │ 1         │           │
│ 3        │ 3    │ 0           │ 976f28147cdb33d9 │ 1         │           │
│ 4        │ 4    │ 0           │ bc8e1a0a073d87cc │ 1         │           │
│ 5        │ 5    │ 0           │ 7aad2880eb458697 │ 1         │           │
│ 6        │ 6    │ 0           │ 7e7afe23ae214297 │ 1         │           │
│ 7        │ 7    │ 0           │ a534c6fdb6abf78b │ 1         │           │
│ 8        │ 8    │ 0           │ 603d2dd28b5b9466 │ 1         │           │
│ 9        │ 9    │ 0           │ ad7facb2586fc6e9 │ 1         │           │
│ 10       │ 10   │ 0           │ 5bb69638130cf638 │ 1         │           │
│ 11       │ 11   │ 0           │ 249ae8d5abec517a │ 1         │           │
│ 12       │ 12   │ 0           │ 7f5502748230a361 │ 1         │           │
│ 13       │ 13   │ 0           │ 12a4b4e75b3e86d3 │ 1         │           │
│ 14       │ 14   │ 0           │ 34962cb641e4deba │ 1         │           │
│ 15       │ 15   │ 0           │ 4d3175e4b2c34d53 │ 1         │           │
│ 16       │ 16   │ 0           │ a0572382d4875333 │ 1         │           │
│ 17       │ 17   │ 0           │ 15b724903840c0ad │ 1         │           │
│ 18       │ 18   │ 0           │ 383820231ab24d46 │ 1         │           │
│ 19       │ 19   │ 0           │ a70e2bb302c4b820 │ 1         │           │
│ 20       │ 20   │ 0           │ 88dec991912d57cd │ 1         │           │
│ 21       │ 21   │ 0           │ 8ecf5f8db76665de │ 1         │           │
│ 22       │ 22   │ 22          │ 5957d75016820319 │ 1         │ <- commit │
│ 23       │ 1    │ 0           │ bd8534c764397e90 │ 2         │           │
│ 24       │ 2    │ 0           │ a2bf415c8897e0ae │ 2         │           │
│ 25       │ 3    │ 0           │ acdc5e1702a86558 │ 2         │           │
│ 26       │ 4    │ 0           │ e41a92c0d1702744 │ 2         │           │
│ 27       │ 8    │ 0           │ fba5521b93297c4b │ 2         │           │
│ 28       │ 9    │ 0           │ ca9a4af9e802b51d │ 2         │           │
│ 29       │ 15   │ 0           │ 4d3175e4b2c34d53 │ 2         │           │
│ 30       │ 19   │ 0           │ e3bb3b6b00cacac6 │ 2         │           │
│ 31       │ 20   │ 0           │ d2e5bb020e2f27de │ 2         │           │
│ 32       │ 21   │ 0           │ 9f00211d84c42594 │ 2         │           │
│ 33       │ 22   │ 0           │ ad7facb2586fc6e9 │ 2         │           │
│ 34       │ 23   │ 0           │ 59134759e86d787f │ 2         │           │
│ 35       │ 24   │ 0           │ deb86ca0005d14f7 │ 2         │           │
│ 36       │ 25   │ 0           │ 732423d9158389d0 │ 2         │           │
│ 37       │ 26   │ 0           │ f5bebfb50ea0bb49 │ 2         │           │
│ 38       │ 27   │ 0           │ 933d83c368aa19b3 │ 2         │           │
│ 39       │ 28   │ 0           │ 44f3ca488e79cc19 │ 2         │           │
│ 40       │ 29   │ 29          │ 862623cb8534c4f3 │ 2         │ <- commit │
│ 41       │ 1    │ 0           │ 565a6b55fcd12fa2 │ 3         │           │
│ 42       │ 2    │ 0           │ 9e832dbc8e6c0016 │ 3         │           │
│ 43       │ 3    │ 0           │ cfa94ac9e81bf655 │ 3         │           │
│ 44       │ 4    │ 0           │ 5e46c8dea368b648 │ 3         │           │
│ 45       │ 20   │ 0           │ 3b65dbee9941c10a │ 3         │           │
│ 46       │ 22   │ 0           │ e245c6247cbcc8c2 │ 3         │           │
│ 47       │ 26   │ 0           │ f296abd436098c82 │ 3         │           │
│ 48       │ 27   │ 0           │ 8270bcf463d86ed8 │ 3         │           │
│ 49       │ 30   │ 0           │ 2d8423154a553dbc │ 3         │           │
│ 50       │ 31   │ 0           │ f3da9777ca90fcf5 │ 3         │           │
│ 51       │ 32   │ 0           │ ec7d9f3bb6c90249 │ 3         │           │
│ 52       │ 33   │ 0           │ 4552b82b526e55ab │ 3         │           │
│ 53       │ 34   │ 0           │ ce92b5493316efc5 │ 3         │           │
│ 54       │ 35   │ 0           │ 5f88ea1f5fec7bf1 │ 3         │           │
│ 55       │ 36   │ 0           │ 9398967028b09df2 │ 3         │           │
│ 56       │ 37   │ 0           │ 6f229b1b93dd19df │ 3         │           │
│ 57       │ 38   │ 0           │ 10a2d292bc654bde │ 3         │           │
│ 58       │ 39   │ 39          │ 677a49036ee0b9f0 │ 3         │ <- commit │
│ 59       │ 1    │ 0           │ 7050f12d532e2ba0 │ 4         │           │
│ 60       │ 2    │ 0           │ aa08648bd4375304 │ 4         │           │
│ 61       │ 3    │ 0           │ 120e8112163f5fc3 │ 4         │           │
│ 62       │ 19   │ 0           │ e3bb3b6b00cacac6 │ 4         │           │
│ 63       │ 21   │ 0           │ deb86ca0005d14f7 │ 4         │           │
│ 64       │ 24   │ 0           │ ad7facb2586fc6e9 │ 4         │           │
│ 65       │ 28   │ 0           │ ad7facb2586fc6e9 │ 4         │           │
│ 66       │ 29   │ 0           │ ad7facb2586fc6e9 │ 4         │           │
│ 67       │ 35   │ 0           │ 2749f192057baac9 │ 4         │           │
│ 68       │ 36   │ 0           │ ad7facb2586fc6e9 │ 4         │           │
│ 69       │ 37   │ 0           │ ad7facb2586fc6e9 │ 4         │           │
│ 70       │ 38   │ 0           │ c2fae8bcd1c019e2 │ 4         │           │
│ 71       │ 39   │ 0           │ 15146137ff3a1e9d │ 4         │           │
│ 72       │ 40   │ 0           │ 91be9b798474f1b3 │ 4         │           │
│ 73       │ 41   │ 0           │ de1b5cde831492f3 │ 4         │           │
│ 74       │ 42   │ 0           │ 3a30707ad61b3366 │ 4         │           │
│ 75       │ 43   │ 0           │ b221d22756aae42e │ 4         │           │
│ 76       │ 44   │ 0           │ 887b2f0e56135974 │ 4         │           │
│ 77       │ 45   │ 0           │ 5c63bd72305ee430 │ 4         │           │
│ 78       │ 46   │ 0           │ c8bee6ed976fd2a7 │ 4         │           │
│ 79       │ 47   │ 0           │ ef1c66b7fa90afff │ 4         │           │
│ 80       │ 48   │ 0           │ d06e873c2057fd6e │ 4         │           │
│ 81       │ 49   │ 0           │ 216aec892a242448 │ 4         │           │
│ 82       │ 50   │ 0           │ aeec3729f9e1bb5c │ 4         │           │
│ 83       │ 51   │ 51          │ 0c62745060da6ad7 │ 4         │ <- commit │
│ 84       │ 1    │ 0           │ 5dccc56df1a35487 │ 5         │           │
│ 85       │ 2    │ 0           │ cc7b92e235c499b1 │ 5         │           │
│ 86       │ 3    │ 0           │ f895c8931cdbd22e │ 5         │           │
│ 87       │ 24   │ 0           │ 111b43f641f5f7fb │ 5         │           │
│ 88       │ 28   │ 0           │ 2cb8d341b022297d │ 5         │           │
│ 89       │ 29   │ 0           │ afc954957f9089e6 │ 5         │           │
│ 90       │ 35   │ 0           │ e325e2d37e43aa15 │ 5         │           │
│ 91       │ 36   │ 0           │ 12109790903aad13 │ 5         │           │
│ 92       │ 37   │ 0           │ 5ea79dd1cb030de3 │ 5         │           │
│ 93       │ 39   │ 0           │ caea83ecb054ea08 │ 5         │           │
│ 94       │ 50   │ 51          │ 3e3710812142b9df │ 5         │ <- commit │
└──────────┴──────┴─────────────┴──────────────────┴───────────┴───────────┘

A4. The transactions. The commit size equals PRAGMA page_count after the step.
┌───────────┬─────────────┬─────────────┬────────────┬────────┬─────────────┬────────────┬─────────────┐
│ commit_no │ recipe_step │ first_frame │ last_frame │ frames │ commit_size │ page_count │ sizes_match │
├───────────┼─────────────┼─────────────┼────────────┼────────┼─────────────┼────────────┼─────────────┤
│ 1         │ 1           │ 1           │ 22         │ 22     │ 22          │ 22         │ true        │
│ 2         │ 2           │ 23          │ 40         │ 18     │ 29          │ 29         │ true        │
│ 3         │ 3           │ 41          │ 58         │ 18     │ 39          │ 39         │ true        │
│ 4         │ 4           │ 59          │ 83         │ 25     │ 51          │ 51         │ true        │
│ 5         │ 5           │ 84          │ 94         │ 11     │ 51          │ 51         │ true        │
└───────────┴─────────────┴─────────────┴────────────┴────────┴─────────────┴────────────┴─────────────┘

A5. PRAGMA wal_checkpoint(NOOP) on the open connection takes no locks and
    reports the wal-index counts: log is the frame count of the WAL, and nothing
    is checkpointed.
┌───────┬────────────┬──────────────┬───────────────┐
│ busy  │ log_frames │ checkpointed │ frames_in_wal │
├───────┼────────────┼──────────────┼───────────────┤
│ false │ 94         │ 0            │ 94            │
└───────┴────────────┴──────────────┴───────────────┘

A6. The wal-index (the -shm copy): both header copies agree, the checksum
    is good, mxFrame is the last commit frame, nPage the database size, and
    its page-number array maps every frame to the page the WAL holds.
┌───────┬───────────────┬───────────────┬─────────────┬───────────┐
│ bytes │  byte_order   │ headers_agree │ checksum_ok │ page_size │
├───────┼───────────────┼───────────────┼─────────────┼───────────┤
│ 32768 │ little-endian │ true          │ true        │ 4096      │
└───────┴───────────────┴───────────────┴─────────────┴───────────┘

    What the header says, against the WAL itself:
┌──────────┬────────┬────────────┬─────────────────┬────────────────────┬──────────────┐
│ mx_frame │ n_page │ n_backfill │ salts_equal_wal │ page_array_matches │ array_frames │
├──────────┼────────┼────────────┼─────────────────┼────────────────────┼──────────────┤
│ 94       │ 51     │ 0          │ true            │ true               │ 94           │
└──────────┴────────┴────────────┴─────────────────┴────────────────────┴──────────────┘

A7. showwal (built from the pinned tree) against the Go parser: they agree on
    every frame page, commit size, salts and checksum, so disagreements is 0.
┌───────────────┬───────────────┬──────────┐
│ frames_listed │ disagreements │ problems │
├───────────────┼───────────────┼──────────┤
│ 94            │ 0             │          │
└───────────────┴───────────────┴──────────┘

── B. The same workload again: what differs between two WALs ──────────

B1. Where run 1 and run 2 differ, by region of the file. Only the salts
    and the checksums that depend on them: the page data is the same.
    (The salts themselves are random; see the info report.)
┌──────────────────────────────────────────┬─────────┐
│                  region                  │ result  │
├──────────────────────────────────────────┼─────────┤
│ header magic                             │ same    │
│ header version                           │ same    │
│ header page size                         │ same    │
│ header checkpoint sequence               │ same    │
│ header salts                             │ differs │
│ header checksum                          │ differs │
│ frame page number                        │ same    │
│ frame commit size                        │ same    │
│ frame salts                              │ differs │
│ frame checksums                          │ differs │
│ page data                                │ same    │
│ bytes beyond the end of the shorter file │ same    │
└──────────────────────────────────────────┴─────────┘

B2. The two runs wrote the same frames: the sequence of (page, page sha256,
    commit size) is equal.
┌───────┬─────────────┬───────────┬────────┬─────────┬───────────────┬──────────────────┐
│  run  │ synchronous │ wal_bytes │ frames │ commits │ same_sequence │ first_difference │
├───────┼─────────────┼───────────┼────────┼─────────┼───────────────┼──────────────────┤
│ run 2 │ FULL        │ 387312    │ 94     │ 5       │ true          │ 0                │
└───────┴─────────────┴───────────┴────────┴─────────┴───────────────┴──────────────────┘

── C. Other synchronous levels write the same frames ──────────────────

C1. synchronous changes when SQLite syncs, not which frames it writes.
    first_difference is the first frame that differs; 0 means none.
┌─────────────┬─────────────┬───────────┬────────┬─────────┬───────────────┬──────────────────┐
│     run     │ synchronous │ wal_bytes │ frames │ commits │ same_sequence │ first_difference │
├─────────────┼─────────────┼───────────┼────────┼─────────┼───────────────┼──────────────────┤
│ sync-normal │ NORMAL      │ 387312    │ 94     │ 5       │ true          │ 0                │
│ sync-off    │ OFF         │ 387312    │ 94     │ 5       │ true          │ 0                │
└─────────────┴─────────────┴───────────┴────────┴─────────┴───────────────┴──────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the WAL is a 32-byte header plus whole frames of a 24-byte header and one 4096-byte page
A  ✓  every frame carries the header's salts and continues the running checksum chain
A  ✓  five recipe steps left exactly five commit frames
A  ✓  each commit frame's size field equals the database's page count after that transaction
A  ✓  the frame count equals the log column of wal_checkpoint(NOOP); nothing was checkpointed
A  ✓  the -shm header names the last commit frame (mxFrame), the database size and the WAL's salts, and its page array maps every frame to its page
A  ✓  showwal and walfile agree on every frame's page, commit size, salts and stored checksums, and on which checksums verify
B  ✓  the two WALs differ, and only inside the salt and checksum fields
B  ✓  the two runs wrote the same pages, with the same images, in the same order, with the same commit sizes
C  ✓  synchronous=NORMAL changes when SQLite syncs, not which frames it writes
C  ✓  synchronous=OFF changes when SQLite syncs, not which frames it writes

11 of 11 claims hold

Experiment verdict: WAL INSPECTOR OK
```

**Reading the evidence:**

- **Header and file size (A1):** `magic=0x377f0682` selects little-endian 32-bit word reads for the checksum (`WAL_MAGIC | SQLITE_BIGENDIAN` on this little-endian host), with `version=3007000`, `page-size=4096`, and `checkpoint-seq=0`. Because the random salts (`0x802a9331` and `0x94ead34f` in one lab run) and the header checksum change on every run, they go to a run-dependent `info` table printed by `course run` rather than the stored evidence. The file is `387,312` bytes, matching `32 + 94 * (24 + 4096)` with `0` trailing bytes.
- **Frames and commits (A2–A4):** All 94 frames verify (`94` valid frames of `94`, stop reason `valid`). Five frames carry a non-zero commit size (`<- commit`): frames 22, 40, 58, 83, and 94, corresponding to recipe steps 1–5 (22, 18, 18, 25, and 11 frames). Each commit starts with page 1 because every recipe step writes `PRAGMA user_version`, and each commit size (`22, 29, 39, 51, 51`) matches `PRAGMA page_count` read right after that step.
- **Three independent oracles (A5–A7):** `PRAGMA wal_checkpoint(NOOP)` reports busy `false`, log `94`, and checkpointed `0`. Our plain-Go `-shm` decoder reads `32,768` bytes in little-endian order with matching header copies, a valid header checksum, `mxFrame=94`, `nPage=51`, `nBackfill=0`, salts matching the WAL header, and a 94-entry page-number array identical to the WAL frames. Finally, SQLite's pinned C `showwal` lists 94 frames with `0` disagreements.
- **Part B — Repeat run (B1–B2):** Run 2 produces the same `387,312`-byte file. The region breakdown shows `differs` only on `header salts`, `header checksum`, `frame salts`, and `frame checksums` (the differing byte count is run-dependent and printed by `course run`: `1,519` in one lab run—`8 + 8 + 752 + 751`, where one checksum byte happened to collide). Every page number, commit size, and page-data byte is `same`, and the `(page, page sha256, commit size)` tuple sequences are equal.
- **Part C — `synchronous` levels (C1):** `NORMAL` and `OFF` both write 94 frames and 5 commits with tuple sequences identical to `FULL`: `synchronous` controls when SQLite calls `fsync`, never which frames it writes.

---

## 4. Timeline

Four timelines trace the life of a WAL: frame append offsets per commit, read marks and backfill under a held reader, WAL restart on the next write, and uncommitted frames left behind by a rollback.

### 4.1 Frame layout across five commits (Experiment 5.1)

Frame *k* (1-based) begins at byte offset `32 + (k - 1) * (24 + pageSize)` (`32 + (k - 1) * 4120` for 4,096-byte pages), and its page image sits 24 bytes later:

| Recipe step | Frames | Frame count | First frame offset | Commit frame offset | Commit size (pages) |
| --- | --- | --- | --- | --- | --- |
| Step 1 | `1–22` | `22` | `32` | `86,552` | `22` |
| Step 2 | `23–40` | `18` | `90,672` | `160,712` | `29` |
| Step 3 | `41–58` | `18` | `164,832` | `234,872` | `39` |
| Step 4 | `59–83` | `25` | `238,992` | `337,872` | `51` |
| Step 5 | `84–94` | `11` | `341,992` | `383,192` | `51` |

### 4.2 Read marks, backfill, and WAL restart under a held reader (Experiment 5.3)

Experiment 5.3 commits steps 7–30 with `wal_autocheckpoint=200` while one reader stays open in two situations:

```text
Part A (reader begins while nBackfill=0 < mxFrame=22):
  after step 6     WAL=90,672 B    mx=22   nBackfill=0    marks=0,0,unused,...
  reader began     WAL=90,672 B    mx=22   nBackfill=0    marks=0,22,unused,...  <- reader sets mark 1 = 22
  steps 7..14      WAL -> 762,232  mx=185  nBackfill=0    marks=0,22,185,...     <- below autocheckpoint 200
  step 15          WAL=861,112 B   mx=209  nBackfill=22   marks=0,22,209,...     <- autocheckpoint hits mark 22
  steps 16..30     WAL -> 2,286,632 mx=555 nBackfill=22   marks=0,22,555,...     <- 533 frames pinned; reader=251 rows
  reader ended     WAL=2,286,632 B mx=555  nBackfill=22   marks=0,22,555,...     <- marks unchanged in -shm
  PASSIVE ckpt     WAL=2,286,632 B mx=555  nBackfill=555  marks=0,555,555,...    <- full backfill; file unshrunk
  step 31          WAL=2,286,632 B mx=18   nBackfill=0    marks=0,18,unused,...  <- restart: salt-1 +1, ckpt-seq 0->1

Part B (reader begins after PASSIVE made nBackfill=22 == mxFrame=22):
  checkpointed     WAL=90,672 B    mx=22   nBackfill=22   marks=0,22,unused,...
  reader began     WAL=90,672 B    mx=22   nBackfill=22   marks=0,22,unused,...  <- takes WAL_READ_LOCK(0), no new mark
  step 7           WAL=90,672 B    mx=16   nBackfill=0    marks=0,16,unused,...  <- WAL restarts (salt-1 +1)!
  steps 8..30      WAL -> 2,195,992 mx=533 nBackfill=0    marks=0,533,unused,... <- WAL_READ_LOCK(0) blocks ALL backfill
  reader + PASSIVE WAL=2,195,992 B mx=533  nBackfill=533  marks=0,533,unused,... <- full backfill once reader closes
```

Part B's lock interaction is easy to misread in prose alone: the reader holds only `WAL_READ_LOCK(0)`, so the writer's restart succeeds on exclusive locks `1..4`, yet every autocheckpoint past frame 200 fails to take exclusive lock `0` and leaves `nBackfill` at `0`:

<!-- draw-visual: diagrams/ch05-wal-transactions-checkpoints-reader-pinning.mmd -->
```text
   ┌────────┐     ┌────────────┐     ┌────────┐
   │ Reader │     │ -shm locks │     │ Writer │
   └────┬───┘     └──────┬─────┘     └────┬───┘
        │                │                │
   ┌───────────────────────────────────────────┐
   │ WAL backfilled: nBackfill == mxFrame (22) │
   └───────────────────────────────────────────┘
        │                │                │
        │ 1. lock 0 (sh) │                │
        ├───────────────►│                │
        │                │                │
┌────────────────┐       │                │
│ reads .db only │       │                │
└────────────────┘       │                │
        │                │                │
        │                │ 2. excl 1..4   │
        │                │◄───────────────┤
        │                │                │
        │                │ 3. granted     │
        │                ├┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈►│
        │                │                │
        │                │      ┌────────────────────┐
        │                │      │ restart at frame 1 │
        │                │      └────────────────────┘
        │                │                │
        │                │ 4. commit >200 │
        │                │◄───────────────┤
        │                │                │
        │                │ 5. excl lock 0 │
        │                │◄───────────────┤
        │                │                │
        │                │ 6. busy: R on 0│
        │                ├┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈×│
        │                │                │
        │                │      ┌───────────────────┐
        │                │      │ nBackfill stays 0 │
        │                │      └───────────────────┘
        │                │                │
```

### 4.3 Spilled frames and rollback (Experiment 5.2 parts B and C)

When a write transaction spills dirty pages out of a small page cache (`cache_size=10`, covered in Chapter 4), frames land in `-wal` before `COMMIT`, while `WalIndexHdr.mxFrame` in `-shm` stays unchanged until commit:

- After committing steps 9–10: `-wal` is `168,952` bytes (`41` frames, `mxFrame = 41`).
- After running `UPDATE documents SET title = upper(title)` without committing: `-wal` grows to `247,232` bytes (`60` frames, `19` valid uncommitted frames after frame `41`).
- After `ROLLBACK`: `-wal` stays `247,232` bytes (`60` frames, byte-for-byte unchanged), while `-shm` keeps `mxFrame = 41`. Only a subsequent write or checkpoint overwrites or truncates those trailing frames.

### 4.4 Experiment 5.3 — Reader pins the WAL (`5.3-reader-pins-the-wal`)

- **Claim:** An open read transaction stops checkpoints and forces unbounded WAL growth in both possible starting states. In Part A (when the WAL is not fully backfilled), the reader pins a read mark in `aReadMark[1..4]` equal to `mxFrame` when it began: `nBackfill` stops at that mark, the WAL never restarts, and the reader's row count stays constant until it closes. Its stale mark remains in `-shm` until a `PASSIVE` checkpoint finishes backfilling, after which the next write restarts at frame 1 without shrinking the file. In Part B (when `nBackfill == mxFrame`), the reader takes `WAL_READ_LOCK(0)` with no mark in `1..4`, letting the next write restart the WAL once while blocking every subsequent backfill (`nBackfill` stays `0`) until the reader closes.
- **Evidence form:** `EXACT` throughout (scripted two-connection order with child-process sampling and no sleeps, so every stored CSV table matches byte for byte on every run).
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the DuckDB walkthrough; add `-i` to stay in the DuckDB shell afterwards with all tables loaded):
  ```text
  make run EXP=5.3
  ```

<!-- include: experiments/5.3-reader-pins-the-wal/main.go -->
```go
// Experiment 5.3 (reader-pins-the-wal) demonstrates both ways an open read
// transaction stops checkpoints and forces unbounded WAL growth: part A holds
// a read mark in slot 1..4 so nBackfill stops at the reader's mark and the WAL
// cannot restart; part B begins on a fully checkpointed WAL so the reader holds
// read lock 0 (WAL_READ_LOCK(0)), letting the WAL restart once while blocking
// all subsequent backfill (nBackfill stays 0).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.3
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	baseStep           = 5
	readerStartsAfter  = 6
	lastLoopStep       = 30
	nextWriteStep      = 31
	autocheckpointSize = 200
	countQuery         = "SELECT count(*) FROM events"
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.3" runs both.
// part labels the rows written while a part runs; the first sample of a part is
// the baseline of its salt-1 column, because the salts themselves are random.
var (
	set             *results.Set
	part            string
	seq             int
	baseSalt        uint32
	haveBase        bool
	sampleTable     *results.Table
	checkpointTable *results.Table
)

func openTables() {
	sampleTable = set.Table("samples", "seq", "part", "moment", "step", "wal_bytes", "mx_frame", "n_backfill", "read_marks", "reader_rows", "writer_rows", "checkpoint_seq", "salt1_advance", "valid_frames", "frames_in_file")
	checkpointTable = set.Table("checkpoints", "seq", "part", "moment", "busy", "log_frames", "checkpointed")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
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

type sample struct {
	walSize    int64
	maxFrame   uint32
	backfill   uint32
	marks      [walfile.ReadMarkCount]uint32
	salt1      uint32
	sequence   uint32
	validCount int
	frames     int
}

func take(ctx context.Context, path, scratch string) sample {
	state := must(walfile.ReadLiveState(ctx, path, scratch))("live state")
	if !state.WALPresent || !state.IndexPresent || state.WAL == nil {
		fatal("live state", fmt.Errorf("WAL present %v, wal-index present %v, problem %q", state.WALPresent, state.IndexPresent, state.WALProblem))
	}
	h := state.Index.Header()
	return sample{
		walSize:    state.WALSize,
		maxFrame:   h.MaxFrame,
		backfill:   state.Index.Checkpoint.Backfill,
		marks:      state.Index.Checkpoint.ReadMarks,
		salt1:      state.WAL.Header.Salt1,
		sequence:   state.WAL.Header.CheckpointSequence,
		validCount: state.WAL.ValidFrames,
		frames:     len(state.WAL.Frames),
	}
}

func marksText(marks [walfile.ReadMarkCount]uint32) string {
	parts := make([]string, len(marks))
	for i, m := range marks {
		parts[i] = walfile.ReadMarkText(m)
	}
	return strings.Join(parts, ",")
}

func count(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	var n int
	if err := q.QueryRowContext(ctx, countQuery).Scan(&n); err != nil {
		fatal("count", err)
	}
	return n
}

func hasMark(marks [walfile.ReadMarkCount]uint32, mark uint32) bool {
	for i := 1; i < len(marks); i++ {
		if marks[i] == mark {
			return true
		}
	}
	return false
}

func openWriter(ctx context.Context, path string) *sqlitex.DB {
	options := recipe.Default().SQLiteOptions()
	options.Conn.WALAutocheckpoint = autocheckpointSize
	return must(sqlitex.Open(ctx, path, options))("open writer")
}

// openReader starts a deferred read-only transaction and executes countQuery so
// SQLite actually calls walTryBeginRead (src/wal.c:3020-3272) and acquires a
// read lock; BEGIN DEFERRED alone takes no lock until the first statement.
func openReader(ctx context.Context, path string) (*sqlitex.DB, *sql.Tx, int) {
	options := recipe.Default().SQLiteOptions()
	options.ImmediateTx = false
	options.Conn.WALAutocheckpoint = 0
	reader := must(sqlitex.Open(ctx, path, options))("open reader")
	tx := must(reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}))("begin read")
	return reader, tx, count(ctx, tx)
}

// row records one sample of the live WAL and wal-index. step is the recipe
// step just committed (nil when none); a reader or writer count of -1 is not
// taken at that moment.
func row(moment string, step any, s sample, readerRows, writerRows int) {
	if !haveBase {
		baseSalt, haveBase = s.salt1, true
	}
	seq++
	sampleTable.Row(seq, part, moment, step, s.walSize, s.maxFrame, s.backfill, marksText(s.marks), rowsValue(readerRows), rowsValue(writerRows), s.sequence, s.salt1-baseSalt, s.validCount, s.frames)
}

func rowsValue(rows int) any {
	if rows < 0 {
		return nil
	}
	return rows
}

func checkpointRow(moment string, result sqlitex.CheckpointResult) {
	seq++
	checkpointTable.Row(seq, part, moment, result.Busy, result.LogFrames, result.Checkpointed)
}

func prepare(ctx context.Context, dir, name string) (string, string) {
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := recipe.Default().Build(ctx, path, baseStep); err != nil {
		fatal("build", err)
	}
	return path, root
}

func partA(ctx context.Context, dir string) {
	part, haveBase = "A", false
	r := recipe.Default().WithSteps(nextWriteStep)
	path, scratch := prepare(ctx, dir, "a-pinned")
	writer := openWriter(ctx, path)
	if err := r.ApplyStep(ctx, writer, readerStartsAfter); err != nil {
		fatal("step", err)
	}
	// Do not query writer before openReader: a SELECT on writer would run
	// walTryBeginRead (src/wal.c:3182-3205) and write aReadMark[1] = 22 before
	// the reader opens. Sampling after each writer SELECT inside the loop keeps
	// every row's read marks aligned with that step's state.
	start := take(ctx, path, scratch)
	row(fmt.Sprintf("after step %d", readerStartsAfter), readerStartsAfter, start, -1, -1)
	reader, tx, readerRows := openReader(ctx, path)
	writerRows := count(ctx, writer)
	pinned := take(ctx, path, scratch)
	row("reader began", nil, pinned, readerRows, writerRows)
	snapshot := pinned.maxFrame
	check(snapshot == start.maxFrame && !hasMark(start.marks, snapshot) && hasMark(pinned.marks, snapshot) && pinned.backfill < snapshot,
		"the reader's snapshot is a read mark equal to mxFrame when it began, and that WAL content was not yet checkpointed")
	samples := []sample{pinned}
	readerStable, marksHeld, backfillBounded := true, true, true
	firstAutocheckpointed := -1
	for k := readerStartsAfter + 1; k <= lastLoopStep; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
		writerRows := count(ctx, writer)
		s := take(ctx, path, scratch)
		rows := count(ctx, tx)
		row(fmt.Sprintf("step %d committed", k), k, s, rows, writerRows)
		readerStable = readerStable && rows == readerRows
		marksHeld = marksHeld && hasMark(s.marks, snapshot)
		backfillBounded = backfillBounded && s.backfill <= snapshot
		if firstAutocheckpointed < 0 && s.backfill == snapshot {
			firstAutocheckpointed = k
		}
		samples = append(samples, s)
	}
	growing, neverRestarted := true, true
	for i := 1; i < len(samples); i++ {
		growing = growing && samples[i].walSize > samples[i-1].walSize
		neverRestarted = neverRestarted && samples[i].maxFrame > samples[i-1].maxFrame && samples[i].salt1 == pinned.salt1
	}
	last := samples[len(samples)-1]
	check(readerStable, "the reader's row count never changed while the writer committed")
	check(growing && neverRestarted, "the WAL grew on every commit and never restarted while the reader stayed open")
	check(marksHeld && backfillBounded && firstAutocheckpointed > 0 && last.backfill == snapshot,
		"checkpoints ran (autocheckpoint) but nBackfill stopped at the reader's mark and stayed there")

	// Ending the read transaction releases its shared lock on WAL_READ_LOCK(1)
	// but leaves its mark number written in aReadMark[1] until a checkpoint or
	// later reader updates that slot (src/wal.c:2241-2242, 3196).
	if err := tx.Rollback(); err != nil {
		fatal("end read", err)
	}
	if err := reader.Close(); err != nil {
		fatal("close reader", err)
	}
	writerRows = count(ctx, writer)
	ended := take(ctx, path, scratch)
	row("reader ended", nil, ended, -1, writerRows)
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	writerRows = count(ctx, writer)
	afterCheckpoint := take(ctx, path, scratch)
	row("PASSIVE checkpoint", nil, afterCheckpoint, -1, writerRows)
	checkpointRow("PASSIVE checkpoint", result)
	check(!result.Busy && afterCheckpoint.backfill == afterCheckpoint.maxFrame && afterCheckpoint.walSize == last.walSize,
		"with the reader gone one checkpoint brings nBackfill to mxFrame; the WAL file keeps its size")
	if err := r.ApplyStep(ctx, writer, nextWriteStep); err != nil {
		fatal("step", err)
	}
	writerRows = count(ctx, writer)
	restarted := take(ctx, path, scratch)
	row(fmt.Sprintf("step %d committed", nextWriteStep), nextWriteStep, restarted, -1, writerRows)
	check(restarted.maxFrame < last.maxFrame && restarted.validCount == int(restarted.maxFrame) && restarted.salt1 == pinned.salt1+1 && restarted.sequence == pinned.sequence+1 && restarted.walSize == last.walSize,
		"the next write restarted the WAL at frame 1 (salt-1 + 1, checkpoint sequence + 1); the file is reused, not shrunk")
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
}

func partB(ctx context.Context, dir string) {
	part, haveBase = "B", false
	r := recipe.Default()
	path, scratch := prepare(ctx, dir, "b-mark-zero")
	writer := openWriter(ctx, path)
	if err := r.ApplyStep(ctx, writer, readerStartsAfter); err != nil {
		fatal("step", err)
	}
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	before := take(ctx, path, scratch)
	row("checkpointed", readerStartsAfter, before, -1, -1)
	checkpointRow(fmt.Sprintf("PASSIVE checkpoint after step %d", readerStartsAfter), result)
	// With nBackfill == mxFrame, walTryBeginRead takes WAL_READ_LOCK(0) shared
	// instead of setting a mark in slots 1..4 (src/wal.c:3134-3163). The writer
	// can restart the WAL because walRestartLog only locks slots 1..4
	// (src/wal.c:3885), but walCheckpoint cannot backfill a single page because
	// it must take WAL_READ_LOCK(0) exclusively first (src/wal.c:2260).
	reader, tx, readerRows := openReader(ctx, path)
	writerRows := count(ctx, writer)
	began := take(ctx, path, scratch)
	row("reader began", nil, began, readerRows, writerRows)
	check(began.backfill == began.maxFrame && began.marks == before.marks, "the WAL was fully checkpointed, so the reader took no read mark in 1..4 (it holds read lock 0 and ignores the WAL)")
	readerStable, backfillZero := true, true
	var samples []sample
	for k := readerStartsAfter + 1; k <= lastLoopStep; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
		writerRows := count(ctx, writer)
		s := take(ctx, path, scratch)
		rows := count(ctx, tx)
		row(fmt.Sprintf("step %d committed", k), k, s, rows, writerRows)
		readerStable = readerStable && rows == readerRows
		backfillZero = backfillZero && s.backfill == 0
		samples = append(samples, s)
	}
	first, last := samples[0], samples[len(samples)-1]
	check(first.salt1 == before.salt1+1 && first.maxFrame < before.maxFrame, "a reader on read lock 0 does not stop the writer from restarting the WAL")
	check(readerStable, "the reader's row count never changed")
	check(backfillZero && last.maxFrame >= autocheckpointSize, "but no checkpoint can copy a single frame while it is open: nBackfill stays 0 although autocheckpoint ran on every commit past the threshold")
	if err := tx.Rollback(); err != nil {
		fatal("end read", err)
	}
	if err := reader.Close(); err != nil {
		fatal("close reader", err)
	}
	result = must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	writerRows = count(ctx, writer)
	after := take(ctx, path, scratch)
	row("reader ended + PASSIVE", nil, after, -1, writerRows)
	checkpointRow("PASSIVE checkpoint after the reader ended", result)
	check(after.backfill == after.maxFrame, "once the reader ends, a checkpoint copies everything")
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.3")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	partA(ctx, dir)
	partB(ctx, dir)
	set.Finish("READER PINS THE WAL")
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

<!-- include: experiments/5.3-reader-pins-the-wal/report.sql -->
```sql
-- Walkthrough of experiment 5.3. Run it with:
--   go run ./cmd/course run 5.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   samples      one row per look at the live -wal and its wal-index, parts A and B
--   checkpoints  one row per wal_checkpoint(PASSIVE) the experiment ran
--   checks       every claim the experiment asserted (the runner lists it last)
-- In samples, reader_rows and writer_rows are blank where that connection was
-- not queried; salt1_advance is salt-1 minus the salt-1 of the first sample of
-- the part (the salts themselves are random).

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.3  How does an open reader stop checkpoints and grow the WAL?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A writer commits recipe steps with wal_autocheckpoint=200 while a reader'
.print 'holds a read transaction open. After every commit the experiment reads'
.print 'the live -wal and the wal-index (-shm) from a child process and counts'
.print 'the rows the reader and the writer see. The read marks column is the five'
.print 'wal-index read marks [0..4]; "unused" is a slot nobody holds.'

.print
.print '── A. A reader that holds a read mark in slot 1 ───────────────────────'
.print
.print 'The reader begins after step 6 (mxFrame 22, nothing checkpointed yet).'
.print
.print 'A1. The reader takes read mark 22 in slot 1 and sees 251 rows for the'
.print '    whole run; the writer moves on to 1177 rows. The WAL grows on every'
.print '    commit and never restarts. Autocheckpoint runs, but n_backfill stops'
.print '    at the reader mark (22) and stays there.'

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks, reader_rows, writer_rows
FROM samples WHERE part = 'A' ORDER BY seq;

.print
.print 'A2. Summary of the loop (steps 7-30):'

WITH s AS (SELECT * FROM samples WHERE part = 'A'),
     mark AS (SELECT mx_frame AS m FROM s WHERE moment = 'reader began'),
     loop AS (SELECT * FROM s WHERE step BETWEEN 7 AND 30)
SELECT (SELECT m FROM mark)                                           AS reader_mark,
       (SELECT min(step) FROM loop WHERE n_backfill = (SELECT m FROM mark)) AS mark_reached_at_step,
       max(mx_frame)                                                  AS final_mx_frame,
       max(mx_frame) - max(n_backfill)                                AS frames_not_checkpointed,
       count(DISTINCT reader_rows)                                    AS reader_row_counts,
       min(salt1_advance) || '..' || max(salt1_advance)               AS salt1_advance
FROM loop;

.print
.print 'A3. The reader ends. Its read mark stays written in slot 1 until a'
.print '    checkpoint, so n_backfill is still 22 (row "reader ended"). One PASSIVE'
.print '    checkpoint now brings n_backfill to mxFrame; the WAL file keeps its size.'

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks
FROM samples WHERE part = 'A' AND moment IN ('reader ended', 'PASSIVE checkpoint') ORDER BY seq;

SELECT moment, busy, log_frames, checkpointed FROM checkpoints WHERE part = 'A' ORDER BY seq;

.print
.print 'A4. The next write (step 31) restarts the WAL at frame 1: salt-1 advances'
.print '    by 1 and the checkpoint sequence by 1. The file is reused, not shrunk;'
.print '    only valid_frames of the frames_in_file belong to the new generation.'

SELECT moment, wal_bytes, mx_frame, checkpoint_seq, salt1_advance,
       valid_frames, frames_in_file
FROM samples WHERE part = 'A' AND moment IN ('reader began', 'PASSIVE checkpoint', 'step 31 committed')
ORDER BY seq;

.print
.print '── B. A reader on read lock 0 ─────────────────────────────────────────'
.print
.print 'A PASSIVE checkpoint after step 6 copies every frame, so the reader that'
.print 'begins next finds n_backfill = mxFrame and takes read lock 0: it reads the'
.print 'database file and ignores the WAL.'
.print
.print 'B1. The checkpoint, and the reader that follows it. The read marks do not'
.print '    change: the reader took no mark in slots 1..4.'

SELECT moment, busy, log_frames, checkpointed FROM checkpoints WHERE part = 'B' ORDER BY seq;

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks, reader_rows, writer_rows
FROM samples WHERE part = 'B' AND moment IN ('checkpointed', 'reader began') ORDER BY seq;

.print
.print 'B2. The writer is free to restart the WAL (salt-1 advances by 1 and mxFrame'
.print '    falls back to 16 on step 7), but no checkpoint can copy one frame while'
.print '    the reader is open: n_backfill stays 0 although autocheckpoint ran on'
.print '    every commit past 200 frames.'

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks, reader_rows, writer_rows
FROM samples WHERE part = 'B' AND step > 6 ORDER BY seq;

.print
.print 'B3. Summary of the loop (steps 7-30):'

SELECT min(CASE WHEN step = 7 THEN salt1_advance END)  AS salt1_advance_at_step_7,
       min(CASE WHEN step = 7 THEN mx_frame END)       AS mx_frame_at_step_7,
       max(mx_frame)                                   AS final_mx_frame,
       max(n_backfill)                                 AS final_n_backfill,
       count(DISTINCT reader_rows)                     AS reader_row_counts
FROM samples WHERE part = 'B' AND step BETWEEN 7 AND 30;

.print
.print 'B4. The reader ends and one PASSIVE checkpoint copies everything.'

SELECT moment, busy, log_frames, checkpointed FROM checkpoints WHERE part = 'B' AND moment LIKE '%reader ended' ORDER BY seq;

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks
FROM samples WHERE part = 'B' AND moment = 'reader ended + PASSIVE' ORDER BY seq;
```

Captured evidence: the walkthrough rendered by the `lab` container. Its backing CSV tables live in [`evidence/5.3-reader-pins-the-wal/`](evidence/5.3-reader-pins-the-wal/), and `make check ARGS='evidence ch05'` compares a fresh run against them byte for byte.

<!-- include: evidence/5.3-reader-pins-the-wal.txt -->
```text

════════════════════════════════════════════════════════════════════════
  5.3  How does an open reader stop checkpoints and grow the WAL?
════════════════════════════════════════════════════════════════════════

A writer commits recipe steps with wal_autocheckpoint=200 while a reader
holds a read transaction open. After every commit the experiment reads
the live -wal and the wal-index (-shm) from a child process and counts
the rows the reader and the writer see. The read marks column is the five
wal-index read marks [0..4]; "unused" is a slot nobody holds.

── A. A reader that holds a read mark in slot 1 ───────────────────────

The reader begins after step 6 (mxFrame 22, nothing checkpointed yet).

A1. The reader takes read mark 22 in slot 1 and sees 251 rows for the
    whole run; the writer moves on to 1177 rows. The WAL grows on every
    commit and never restarts. Autocheckpoint runs, but n_backfill stops
    at the reader mark (22) and stays there.
┌────────────────────┬───────────┬──────────┬────────────┬───────────────────────────┬─────────────┬─────────────┐
│       moment       │ wal_bytes │ mx_frame │ n_backfill │        read_marks         │ reader_rows │ writer_rows │
├────────────────────┼───────────┼──────────┼────────────┼───────────────────────────┼─────────────┼─────────────┤
│ after step 6       │ 90672     │ 22       │ 0          │ 0,0,unused,unused,unused  │             │             │
│ reader began       │ 90672     │ 22       │ 0          │ 0,22,unused,unused,unused │ 251         │ 251         │
│ step 7 committed   │ 156592    │ 38       │ 0          │ 0,22,38,unused,unused     │ 251         │ 287         │
│ step 8 committed   │ 251352    │ 61       │ 0          │ 0,22,61,unused,unused     │ 251         │ 336         │
│ step 9 committed   │ 346112    │ 84       │ 0          │ 0,22,84,unused,unused     │ 251         │ 372         │
│ step 10 committed  │ 416152    │ 101      │ 0          │ 0,22,101,unused,unused    │ 251         │ 418         │
│ step 11 committed  │ 477952    │ 116      │ 0          │ 0,22,116,unused,unused    │ 251         │ 458         │
│ step 12 committed  │ 597432    │ 145      │ 0          │ 0,22,145,unused,unused    │ 251         │ 482         │
│ step 13 committed  │ 688072    │ 167      │ 0          │ 0,22,167,unused,unused    │ 251         │ 524         │
│ step 14 committed  │ 762232    │ 185      │ 0          │ 0,22,185,unused,unused    │ 251         │ 570         │
│ step 15 committed  │ 861112    │ 209      │ 22         │ 0,22,209,unused,unused    │ 251         │ 600         │
│ step 16 committed  │ 992952    │ 241      │ 22         │ 0,22,241,unused,unused    │ 251         │ 635         │
│ step 17 committed  │ 1046512   │ 254      │ 22         │ 0,22,254,unused,unused    │ 251         │ 695         │
│ step 18 committed  │ 1149512   │ 279      │ 22         │ 0,22,279,unused,unused    │ 251         │ 725         │
│ step 19 committed  │ 1190712   │ 289      │ 22         │ 0,22,289,unused,unused    │ 251         │ 770         │
│ step 20 committed  │ 1297832   │ 315      │ 22         │ 0,22,315,unused,unused    │ 251         │ 802         │
│ step 21 committed  │ 1417312   │ 344      │ 22         │ 0,22,344,unused,unused    │ 251         │ 831         │
│ step 22 committed  │ 1503832   │ 365      │ 22         │ 0,22,365,unused,unused    │ 251         │ 871         │
│ step 23 committed  │ 1586232   │ 385      │ 22         │ 0,22,385,unused,unused    │ 251         │ 915         │
│ step 24 committed  │ 1705712   │ 414      │ 22         │ 0,22,414,unused,unused    │ 251         │ 942         │
│ step 25 committed  │ 1788112   │ 434      │ 22         │ 0,22,434,unused,unused    │ 251         │ 980         │
│ step 26 committed  │ 1899352   │ 461      │ 22         │ 0,22,461,unused,unused    │ 251         │ 1024        │
│ step 27 committed  │ 2006472   │ 487      │ 22         │ 0,22,487,unused,unused    │ 251         │ 1047        │
│ step 28 committed  │ 2088872   │ 507      │ 22         │ 0,22,507,unused,unused    │ 251         │ 1095        │
│ step 29 committed  │ 2163032   │ 525      │ 22         │ 0,22,525,unused,unused    │ 251         │ 1130        │
│ step 30 committed  │ 2286632   │ 555      │ 22         │ 0,22,555,unused,unused    │ 251         │ 1177        │
│ reader ended       │ 2286632   │ 555      │ 22         │ 0,22,555,unused,unused    │             │ 1177        │
│ PASSIVE checkpoint │ 2286632   │ 555      │ 555        │ 0,555,555,unused,unused   │             │ 1177        │
│ step 31 committed  │ 2286632   │ 18       │ 0          │ 0,18,unused,unused,unused │             │ 1214        │
└────────────────────┴───────────┴──────────┴────────────┴───────────────────────────┴─────────────┴─────────────┘

A2. Summary of the loop (steps 7-30):
┌─────────────┬──────────────────────┬────────────────┬─────────────────────────┬───────────────────┬───────────────┐
│ reader_mark │ mark_reached_at_step │ final_mx_frame │ frames_not_checkpointed │ reader_row_counts │ salt1_advance │
├─────────────┼──────────────────────┼────────────────┼─────────────────────────┼───────────────────┼───────────────┤
│ 22          │ 15                   │ 555            │ 533                     │ 1                 │ 0..0          │
└─────────────┴──────────────────────┴────────────────┴─────────────────────────┴───────────────────┴───────────────┘

A3. The reader ends. Its read mark stays written in slot 1 until a
    checkpoint, so n_backfill is still 22 (row "reader ended"). One PASSIVE
    checkpoint now brings n_backfill to mxFrame; the WAL file keeps its size.
┌────────────────────┬───────────┬──────────┬────────────┬─────────────────────────┐
│       moment       │ wal_bytes │ mx_frame │ n_backfill │       read_marks        │
├────────────────────┼───────────┼──────────┼────────────┼─────────────────────────┤
│ reader ended       │ 2286632   │ 555      │ 22         │ 0,22,555,unused,unused  │
│ PASSIVE checkpoint │ 2286632   │ 555      │ 555        │ 0,555,555,unused,unused │
└────────────────────┴───────────┴──────────┴────────────┴─────────────────────────┘
┌────────────────────┬───────┬────────────┬──────────────┐
│       moment       │ busy  │ log_frames │ checkpointed │
├────────────────────┼───────┼────────────┼──────────────┤
│ PASSIVE checkpoint │ false │ 555        │ 555          │
└────────────────────┴───────┴────────────┴──────────────┘

A4. The next write (step 31) restarts the WAL at frame 1: salt-1 advances
    by 1 and the checkpoint sequence by 1. The file is reused, not shrunk;
    only valid_frames of the frames_in_file belong to the new generation.
┌────────────────────┬───────────┬──────────┬────────────────┬───────────────┬──────────────┬────────────────┐
│       moment       │ wal_bytes │ mx_frame │ checkpoint_seq │ salt1_advance │ valid_frames │ frames_in_file │
├────────────────────┼───────────┼──────────┼────────────────┼───────────────┼──────────────┼────────────────┤
│ reader began       │ 90672     │ 22       │ 0              │ 0             │ 22           │ 22             │
│ PASSIVE checkpoint │ 2286632   │ 555      │ 0              │ 0             │ 555          │ 555            │
│ step 31 committed  │ 2286632   │ 18       │ 1              │ 1             │ 18           │ 555            │
└────────────────────┴───────────┴──────────┴────────────────┴───────────────┴──────────────┴────────────────┘

── B. A reader on read lock 0 ─────────────────────────────────────────

A PASSIVE checkpoint after step 6 copies every frame, so the reader that
begins next finds n_backfill = mxFrame and takes read lock 0: it reads the
database file and ignores the WAL.

B1. The checkpoint, and the reader that follows it. The read marks do not
    change: the reader took no mark in slots 1..4.
┌───────────────────────────────────────────┬───────┬────────────┬──────────────┐
│                  moment                   │ busy  │ log_frames │ checkpointed │
├───────────────────────────────────────────┼───────┼────────────┼──────────────┤
│ PASSIVE checkpoint after step 6           │ false │ 22         │ 22           │
│ PASSIVE checkpoint after the reader ended │ false │ 533        │ 533          │
└───────────────────────────────────────────┴───────┴────────────┴──────────────┘
┌──────────────┬───────────┬──────────┬────────────┬───────────────────────────┬─────────────┬─────────────┐
│    moment    │ wal_bytes │ mx_frame │ n_backfill │        read_marks         │ reader_rows │ writer_rows │
├──────────────┼───────────┼──────────┼────────────┼───────────────────────────┼─────────────┼─────────────┤
│ checkpointed │ 90672     │ 22       │ 22         │ 0,22,unused,unused,unused │             │             │
│ reader began │ 90672     │ 22       │ 22         │ 0,22,unused,unused,unused │ 251         │ 251         │
└──────────────┴───────────┴──────────┴────────────┴───────────────────────────┴─────────────┴─────────────┘

B2. The writer is free to restart the WAL (salt-1 advances by 1 and mxFrame
    falls back to 16 on step 7), but no checkpoint can copy one frame while
    the reader is open: n_backfill stays 0 although autocheckpoint ran on
    every commit past 200 frames.
┌───────────────────┬───────────┬──────────┬────────────┬────────────────────────────┬─────────────┬─────────────┐
│      moment       │ wal_bytes │ mx_frame │ n_backfill │         read_marks         │ reader_rows │ writer_rows │
├───────────────────┼───────────┼──────────┼────────────┼────────────────────────────┼─────────────┼─────────────┤
│ step 7 committed  │ 90672     │ 16       │ 0          │ 0,16,unused,unused,unused  │ 251         │ 287         │
│ step 8 committed  │ 160712    │ 39       │ 0          │ 0,39,unused,unused,unused  │ 251         │ 336         │
│ step 9 committed  │ 255472    │ 62       │ 0          │ 0,62,unused,unused,unused  │ 251         │ 372         │
│ step 10 committed │ 325512    │ 79       │ 0          │ 0,79,unused,unused,unused  │ 251         │ 418         │
│ step 11 committed │ 387312    │ 94       │ 0          │ 0,94,unused,unused,unused  │ 251         │ 458         │
│ step 12 committed │ 506792    │ 123      │ 0          │ 0,123,unused,unused,unused │ 251         │ 482         │
│ step 13 committed │ 597432    │ 145      │ 0          │ 0,145,unused,unused,unused │ 251         │ 524         │
│ step 14 committed │ 671592    │ 163      │ 0          │ 0,163,unused,unused,unused │ 251         │ 570         │
│ step 15 committed │ 770472    │ 187      │ 0          │ 0,187,unused,unused,unused │ 251         │ 600         │
│ step 16 committed │ 902312    │ 219      │ 0          │ 0,219,unused,unused,unused │ 251         │ 635         │
│ step 17 committed │ 955872    │ 232      │ 0          │ 0,232,unused,unused,unused │ 251         │ 695         │
│ step 18 committed │ 1058872   │ 257      │ 0          │ 0,257,unused,unused,unused │ 251         │ 725         │
│ step 19 committed │ 1100072   │ 267      │ 0          │ 0,267,unused,unused,unused │ 251         │ 770         │
│ step 20 committed │ 1207192   │ 293      │ 0          │ 0,293,unused,unused,unused │ 251         │ 802         │
│ step 21 committed │ 1326672   │ 322      │ 0          │ 0,322,unused,unused,unused │ 251         │ 831         │
│ step 22 committed │ 1413192   │ 343      │ 0          │ 0,343,unused,unused,unused │ 251         │ 871         │
│ step 23 committed │ 1495592   │ 363      │ 0          │ 0,363,unused,unused,unused │ 251         │ 915         │
│ step 24 committed │ 1615072   │ 392      │ 0          │ 0,392,unused,unused,unused │ 251         │ 942         │
│ step 25 committed │ 1697472   │ 412      │ 0          │ 0,412,unused,unused,unused │ 251         │ 980         │
│ step 26 committed │ 1808712   │ 439      │ 0          │ 0,439,unused,unused,unused │ 251         │ 1024        │
│ step 27 committed │ 1915832   │ 465      │ 0          │ 0,465,unused,unused,unused │ 251         │ 1047        │
│ step 28 committed │ 1998232   │ 485      │ 0          │ 0,485,unused,unused,unused │ 251         │ 1095        │
│ step 29 committed │ 2072392   │ 503      │ 0          │ 0,503,unused,unused,unused │ 251         │ 1130        │
│ step 30 committed │ 2195992   │ 533      │ 0          │ 0,533,unused,unused,unused │ 251         │ 1177        │
└───────────────────┴───────────┴──────────┴────────────┴────────────────────────────┴─────────────┴─────────────┘

B3. Summary of the loop (steps 7-30):
┌─────────────────────────┬────────────────────┬────────────────┬──────────────────┬───────────────────┐
│ salt1_advance_at_step_7 │ mx_frame_at_step_7 │ final_mx_frame │ final_n_backfill │ reader_row_counts │
├─────────────────────────┼────────────────────┼────────────────┼──────────────────┼───────────────────┤
│ 1                       │ 16                 │ 533            │ 0                │ 1                 │
└─────────────────────────┴────────────────────┴────────────────┴──────────────────┴───────────────────┘

B4. The reader ends and one PASSIVE checkpoint copies everything.
┌───────────────────────────────────────────┬───────┬────────────┬──────────────┐
│                  moment                   │ busy  │ log_frames │ checkpointed │
├───────────────────────────────────────────┼───────┼────────────┼──────────────┤
│ PASSIVE checkpoint after the reader ended │ false │ 533        │ 533          │
└───────────────────────────────────────────┴───────┴────────────┴──────────────┘
┌────────────────────────┬───────────┬──────────┬────────────┬────────────────────────────┐
│         moment         │ wal_bytes │ mx_frame │ n_backfill │         read_marks         │
├────────────────────────┼───────────┼──────────┼────────────┼────────────────────────────┤
│ reader ended + PASSIVE │ 2195992   │ 533      │ 533        │ 0,533,unused,unused,unused │
└────────────────────────┴───────────┴──────────┴────────────┴────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the reader's snapshot is a read mark equal to mxFrame when it began, and that WAL content was not yet checkpointed
A  ✓  the reader's row count never changed while the writer committed
A  ✓  the WAL grew on every commit and never restarted while the reader stayed open
A  ✓  checkpoints ran (autocheckpoint) but nBackfill stopped at the reader's mark and stayed there
A  ✓  with the reader gone one checkpoint brings nBackfill to mxFrame; the WAL file keeps its size
A  ✓  the next write restarted the WAL at frame 1 (salt-1 + 1, checkpoint sequence + 1); the file is reused, not shrunk
B  ✓  the WAL was fully checkpointed, so the reader took no read mark in 1..4 (it holds read lock 0 and ignores the WAL)
B  ✓  a reader on read lock 0 does not stop the writer from restarting the WAL
B  ✓  the reader's row count never changed
B  ✓  but no checkpoint can copy a single frame while it is open: nBackfill stays 0 although autocheckpoint ran on every commit past the threshold
B  ✓  once the reader ends, a checkpoint copies everything

11 of 11 claims hold

Experiment verdict: READER PINS THE WAL OK
```

**Reading the evidence:**

- **Part A — Read mark in slot 1 (A1–A4):**
  - `after step 6` shows `WAL bytes = 90672`, `mxFrame = 22`, `nBackfill = 0`, and `read marks = 0,0,unused,unused,unused` (the writer did not run a `SELECT` before `openReader`, so slot 1 is still `0`).
  - `reader began` updates slot 1 to `22` (`0,22,unused,unused,unused`) and reads `251` rows.
  - Across steps 7–30, the writer's own `SELECT count(*)` after each commit updates slot 2 (`38, 61, ..., 555`), while slot 1 stays pinned at `22` and the reader sees `251` rows as the writer climbs to `1177`.
  - At step 15 (`mxFrame = 209 >= 200`), autocheckpoint fires for the first time and advances `nBackfill` from `0` to `22`. On steps 16 through 30, autocheckpoint fires again on every commit and cannot move `nBackfill` past `22`; `533` of `555` frames stay uncheckpointed and the WAL grows to `2,286,632` bytes.
  - `reader ended` prints the exact same `-shm` row (`0,22,555,unused,unused`) as `step 30 committed`, confirming that read marks outlive their readers—only the POSIX lock on `-shm` tells SQLite the slot is free.
  - One `PASSIVE` checkpoint then returns busy `false`, log `555`, checkpointed `555` (A3), advancing `nBackfill` and mark 1 to `555` while leaving the file at `2,286,632` bytes.
  - `step 31 committed` restarts the WAL at frame 1 (`mxFrame = 18`, `nBackfill = 0`, `checkpoint-seq 0 -> 1`, `salt-1 + 1`, `18` valid frames of `555` in the file, with the remaining `537` frames belonging to the old generation), still at `2,286,632` bytes.
- **Part B — Database-only reader on `WAL_READ_LOCK(0)` (B1–B4):**
  - After step 6, `PASSIVE` brings `nBackfill = 22 == mxFrame`. When the reader begins, `read marks` stay `0,22,unused,unused,unused`: the reader took `WAL_READ_LOCK(0)` and reads only `.db`.
  - On step 7, the writer immediately restarts the WAL (`salt-1 + 1`, `mxFrame = 16`, `WAL bytes = 90672`) because `walRestartLog` needs only exclusive locks `1..4`.
  - Yet from step 7 through step 30 (`mxFrame = 533`, `2,195,992` bytes), `nBackfill` stays `0` on every row even though autocheckpoint ran on every commit past frame 200 (`walCheckpoint` cannot take `WAL_READ_LOCK(0)` exclusively). Once the reader closes, one `PASSIVE` checkpoint copies all `533` frames.

---

## 5. Lab

This section inlines the `internal/walfile` package introduced by this chapter and then proves rung **R3 (Redo log)** in Experiment 5.2.

### 5.1 The `internal/walfile` package

`internal/walfile` imports only the Go standard library (its live integration test imports `internal/sqlitex`) and is organized into eight implementation files:

- **`errors.go`** — typed sentinel errors matching SQLite's recovery and checkpoint failure classes (`ErrWALIgnored`, `ErrUnsupportedVersion`, `ErrBaseNotDatabase`, `ErrBaseSize`, `ErrPageSizeMismatch`, `ErrCommitSizeTooLarge`, `ErrOutputExists`, `ErrShortIndex`, `ErrIndexVersion`, `ErrFrameNotIndexed`) plus `HeaderError`.

<!-- include: ../internal/walfile/errors.go -->
```go
package walfile

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by WAL parsing, replay, and wal-index decoding.
var (
	// ErrWALIgnored marks a WAL header that SQLite silently treats as having
	// zero valid frames during recovery (src/wal.c:1447-1467, walIndexRecover).
	ErrWALIgnored = errors.New("SQLite ignores this WAL file")
	// ErrUnsupportedVersion marks a WAL header whose format version is not
	// 3007000; SQLite returns SQLITE_CANTOPEN (src/wal.c:1472-1474).
	ErrUnsupportedVersion = errors.New("unsupported WAL format version")
	// ErrBaseNotDatabase reports a non-empty replay base missing the 100-byte
	// "SQLite format 3\x00" database header or carrying an invalid page-size
	// field in bytes 16..17 (must be a power of two in 512..65536).
	ErrBaseNotDatabase = errors.New("base is not an SQLite database file")
	// ErrBaseSize reports a base database whose byte length is not a whole
	// multiple of the page size (the WAL page size, or the base's own page
	// size when the WAL is empty).
	ErrBaseSize = errors.New("base size is not a whole number of pages")
	// ErrPageSizeMismatch reports that the base database header and WAL header
	// disagree on page size (src/wal.c:4392, SQLITE_CORRUPT).
	ErrPageSizeMismatch = errors.New("base and WAL page sizes differ")
	// ErrCommitSizeTooLarge reports a commit size larger than base + 65536 +
	// frames*pageSize, matching SQLite's corruption guard in walCheckpoint
	// (src/wal.c:2288-2293).
	ErrCommitSizeTooLarge = errors.New("commit size is larger than the base plus every frame could fill")
	// ErrOutputExists reports that the replay target path or a sibling -wal,
	// -journal, or -shm file already exists and could corrupt a later open.
	ErrOutputExists = errors.New("output file or one of its side files already exists")
	// ErrShortIndex reports a -shm file smaller than the 136-byte wal-index
	// header region (src/wal.c:474, WALINDEX_HDR_SIZE).
	ErrShortIndex = errors.New("wal-index is shorter than its header")
	// ErrIndexVersion reports a non-zero -shm header whose first word is not
	// 3007000 in either little-endian or big-endian byte order.
	ErrIndexVersion = errors.New("wal-index header version is not 3007000 in either byte order")
	// ErrFrameNotIndexed reports a frame number outside the mapped -shm blocks
	// or a lookup against an uninitialized or torn wal-index.
	ErrFrameNotIndexed = errors.New("frame is outside the wal-index")
)

// HeaderError pairs a sentinel error (ErrWALIgnored or ErrUnsupportedVersion)
// with the specific header field that failed validation.
type HeaderError struct {
	Kind   error
	Reason string
}

// Error formats the sentinel error followed by the specific header reason.
func (e *HeaderError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Reason)
}

// Unwrap returns Kind so errors.Is matches ErrWALIgnored or ErrUnsupportedVersion.
func (e *HeaderError) Unwrap() error { return e.Kind }

func ignored(format string, args ...any) error {
	return &HeaderError{Kind: ErrWALIgnored, Reason: fmt.Sprintf(format, args...)}
}
```

- **`checksum.go`** — implements `walChecksumBytes` (`src/wal.c:856-912`), reading 32-bit word pairs in little-endian or big-endian order according to bit 0 of the WAL magic while storing output checksums big-endian.

<!-- include: ../internal/walfile/checksum.go -->
```go
package walfile

import (
	"encoding/binary"
	"fmt"
)

// Checksum holds SQLite's two-word WAL cumulative checksum (s1, s2).
type Checksum [2]uint32

// String formats both checksum words as 8-digit lowercase hex.
func (c Checksum) String() string {
	return fmt.Sprintf("%08x %08x", c[0], c[1])
}

// ChecksumBytes extends seed over data using SQLite's WAL Fletcher-like
// checksum (src/wal.c:856-912, walChecksumBytes). Input length must be a
// positive multiple of 8 bytes because the loop consumes 32-bit word pairs.
func ChecksumBytes(bigEndianWords bool, data []byte, seed Checksum) (Checksum, error) {
	if len(data) == 0 || len(data)%8 != 0 {
		return seed, fmt.Errorf("checksum input of %d bytes is not a positive multiple of 8", len(data))
	}
	return sumWordPairs(bigEndianWords, data, seed), nil
}

// sumWordPairs implements the inner loop of walChecksumBytes (src/wal.c:877-911).
// Each pair of 32-bit words is read in the byte order selected by the WAL magic
// bit (big-endian for 0x377f0683, little-endian for 0x377f0682), while the
// resulting s1 and s2 values are always stored big-endian on disk.
func sumWordPairs(bigEndianWords bool, data []byte, seed Checksum) Checksum {
	var order binary.ByteOrder = binary.LittleEndian
	if bigEndianWords {
		order = binary.BigEndian
	}
	s1, s2 := seed[0], seed[1]
	for i := 0; i+8 <= len(data); i += 8 {
		s1 += order.Uint32(data[i:]) + s2
		s2 += order.Uint32(data[i+4:]) + s1
	}
	return Checksum{s1, s2}
}
```

- **`wal.go`** — parses a raw WAL file (`Parse`, `ParseFile`) in SQLite's exact validation order (`walIndexRecover` + `walDecodeFrame`), lists commits (`Commits`), exposes page images (`PageImage`), and projects frames to salt-independent `Tuple` slices (`Tuples`, `EqualTuples`).

<!-- include: ../internal/walfile/wal.go -->
```go
// Package walfile parses SQLite write-ahead log (-wal) and wal-index (-shm)
// files in plain Go without linking SQLite, replays committed WAL frames onto a
// base database file, compares WAL files by structural region or frame tuples,
// cross-checks against SQLite's showwal utility, and copies live database files
// through a child process so POSIX advisory locks are never dropped.
package walfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
)

// On-disk WAL format constants (src/wal.c:34-57, 477-491).
const (
	// HeaderSize is the fixed 32-byte WAL file header length (WAL_HDRSIZE).
	HeaderSize = 32
	// FrameHeaderSize is the fixed 24-byte header preceding each page image
	// (WAL_FRAME_HDRSIZE).
	FrameHeaderSize = 24
	// MagicLittleEndian selects little-endian 32-bit word reads for checksums
	// (WAL_MAGIC, src/wal.c:491).
	MagicLittleEndian = 0x377f0682
	// MagicBigEndian selects big-endian 32-bit word reads for checksums
	// (WAL_MAGIC | 1, src/wal.c:482-491).
	MagicBigEndian = 0x377f0683
	// FormatVersion is the only WAL file format version SQLite 3.53.4 accepts
	// (WAL_MAX_VERSION, src/wal.c:1472).
	FormatVersion = 3007000
	// MinPageSize is the smallest valid SQLite page size in bytes.
	MinPageSize = 512
	// MaxPageSize is the largest valid SQLite page size in bytes.
	MaxPageSize = 65536
)

// Header holds the eight big-endian 32-bit fields of the 32-byte WAL header
// (src/wal.c:34-44).
type Header struct {
	Magic              uint32
	Version            uint32
	PageSize           uint32
	CheckpointSequence uint32
	Salt1              uint32
	Salt2              uint32
	Checksum           Checksum
}

// BigEndianChecksums reports whether checksums in this WAL treat 32-bit input
// words as big-endian (indicated by bit 0 of Magic, src/wal.c:70-72, 1454).
func (h Header) BigEndianChecksums() bool { return h.Magic&1 == 1 }

// ChecksumByteOrder names the word byte order used by the checksum algorithm.
func (h Header) ChecksumByteOrder() string {
	if h.BigEndianChecksums() {
		return "big-endian"
	}
	return "little-endian"
}

// String formats all WAL header fields in a single readable line.
func (h Header) String() string {
	return fmt.Sprintf("magic=0x%08x (%s checksums) version=%d page-size=%d checkpoint-seq=%d salt-1=0x%08x salt-2=0x%08x checksum=%s",
		h.Magic, h.ChecksumByteOrder(), h.Version, h.PageSize, h.CheckpointSequence, h.Salt1, h.Salt2, h.Checksum)
}

func decodeHeader(data []byte) Header {
	be := binary.BigEndian
	return Header{
		Magic:              be.Uint32(data[0:]),
		Version:            be.Uint32(data[4:]),
		PageSize:           be.Uint32(data[8:]),
		CheckpointSequence: be.Uint32(data[12:]),
		Salt1:              be.Uint32(data[16:]),
		Salt2:              be.Uint32(data[20:]),
		Checksum:           Checksum{be.Uint32(data[24:]), be.Uint32(data[28:])},
	}
}

// FrameProblem classifies why a WAL frame failed validation or was stopped
// behind an earlier invalid frame (src/wal.c:1000-1053, walDecodeFrame).
type FrameProblem int

// Frame validation outcomes in the exact check order SQLite uses.
const (
	FrameValid FrameProblem = iota
	FrameSaltMismatch
	FramePageZero
	FrameChecksumMismatch
	FrameAfterInvalid
)

// String returns a human-readable explanation of p.
func (p FrameProblem) String() string {
	switch p {
	case FrameValid:
		return "valid"
	case FrameSaltMismatch:
		return "salt mismatch (frame from an earlier WAL generation, or rewritten in place by an open transaction)"
	case FramePageZero:
		return "page number 0"
	case FrameChecksumMismatch:
		return "checksum mismatch (running checksum chain broken)"
	case FrameAfterInvalid:
		return "follows an invalid frame"
	}
	return fmt.Sprintf("FrameProblem(%d)", int(p))
}

// Frame represents one whole frame slot in a WAL file: its 1-based slot number,
// file offset, decoded 24-byte header fields, page SHA-256, and validation state.
type Frame struct {
	Number           int
	Offset           int64
	Page             uint32
	CommitSize       uint32
	Salt1            uint32
	Salt2            uint32
	StoredChecksum   Checksum
	PageSHA256       string
	SaltsMatchHeader bool
	Problem          FrameProblem
}

// Valid reports whether f passed salt, page-number, and cumulative-checksum
// validation as part of the unbroken valid prefix of the WAL.
func (f Frame) Valid() bool { return f.Problem == FrameValid }

// IsCommit reports whether f marks a transaction commit (non-zero database size
// in pages, src/wal.c:52-53, 1516).
func (f Frame) IsCommit() bool { return f.CommitSize != 0 }

// Tuple is the run-independent projection of a WAL frame: page number, SHA-256
// of the page image, and commit size. Comparing tuples ignores random salts and
// checksums so two runs of a deterministic workload can be compared directly.
type Tuple struct {
	Page       uint32
	PageSHA256 string
	CommitSize uint32
}

// String formats a Tuple as key=value fields.
func (t Tuple) String() string {
	return fmt.Sprintf("page=%d commit-size=%d sha256=%s", t.Page, t.CommitSize, t.PageSHA256)
}

// Commit describes one committed transaction inside the valid prefix of a WAL:
// its inclusive 1-based frame range and the resulting database size in pages.
type Commit struct {
	FirstFrame int
	LastFrame  int
	Pages      uint32
}

// Frames returns the number of WAL frames belonging to c.
func (c Commit) Frames() int { return c.LastFrame - c.FirstFrame + 1 }

// File is a parsed WAL file holding the header, every whole frame slot present
// in the file, the lengths of the valid and committed prefixes, and any
// trailing bytes that do not form a complete frame.
type File struct {
	Size              int64
	Header            Header
	ComputedChecksum  Checksum
	Frames            []Frame
	ValidFrames       int
	LastCommitFrame   int
	PartialFrameBytes int64
	data              []byte
}

// PageSize returns the database page size recorded in the WAL header.
func (f *File) PageSize() int { return int(f.Header.PageSize) }

// FrameSize returns the total byte size of one frame (24-byte header + page).
func (f *File) FrameSize() int { return int(f.Header.PageSize) + FrameHeaderSize }

// StopReason returns the FrameProblem of the first invalid frame, or FrameValid
// when every whole frame slot in the WAL verified.
func (f *File) StopReason() FrameProblem {
	if f.ValidFrames < len(f.Frames) {
		return f.Frames[f.ValidFrames].Problem
	}
	return FrameValid
}

// CommittedFrames returns the valid prefix up to and including the last commit
// frame (empty if no commit frame exists).
func (f *File) CommittedFrames() []Frame { return f.Frames[:f.LastCommitFrame] }

// UncommittedValidFrames returns valid frames after LastCommitFrame that have
// no subsequent commit frame (for example, spilled frames from an open or
// rolled-back transaction whose checksums were never invalidated).
func (f *File) UncommittedValidFrames() []Frame {
	return f.Frames[f.LastCommitFrame:f.ValidFrames]
}

// Commits groups CommittedFrames into one Commit entry per commit frame.
func (f *File) Commits() []Commit {
	var commits []Commit
	first := 1
	for _, frame := range f.Frames[:f.LastCommitFrame] {
		if frame.IsCommit() {
			commits = append(commits, Commit{FirstFrame: first, LastFrame: frame.Number, Pages: frame.CommitSize})
			first = frame.Number + 1
		}
	}
	return commits
}

// DatabasePages returns the committed database size in pages from the last
// commit frame, or 0 if the WAL contains no committed transaction.
func (f *File) DatabasePages() uint32 {
	if f.LastCommitFrame == 0 {
		return 0
	}
	return f.Frames[f.LastCommitFrame-1].CommitSize
}

// PageImage returns the raw page bytes stored in 1-based frameNumber.
func (f *File) PageImage(frameNumber int) ([]byte, error) {
	if frameNumber < 1 || frameNumber > len(f.Frames) {
		return nil, fmt.Errorf("frame %d is not in this WAL (%d frames)", frameNumber, len(f.Frames))
	}
	start := f.Frames[frameNumber-1].Offset + FrameHeaderSize
	return f.data[start : start+int64(f.PageSize())], nil
}

// Tuples projects the WAL frames (or only the valid prefix when validOnly is
// true) to their salt-independent Tuple sequence.
func (f *File) Tuples(validOnly bool) []Tuple {
	frames := f.Frames
	if validOnly {
		frames = f.Frames[:f.ValidFrames]
	}
	tuples := make([]Tuple, 0, len(frames))
	for _, frame := range frames {
		tuples = append(tuples, Tuple{Page: frame.Page, PageSHA256: frame.PageSHA256, CommitSize: frame.CommitSize})
	}
	return tuples
}

// ParseFile reads path from disk and decodes it with Parse.
func ParseFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a raw WAL byte slice following SQLite's recovery rules in
// walIndexRecover (src/wal.c:1390-1614) and walDecodeFrame (src/wal.c:1000-1053).
// A 0-byte WAL is valid and empty (for example right after TRUNCATE); a bad
// header magic, page size, or checksum returns *HeaderError wrapping
// ErrWALIgnored; a version other than 3007000 returns ErrUnsupportedVersion.
func Parse(data []byte) (*File, error) {
	file := &File{Size: int64(len(data)), data: data}
	if len(data) == 0 {
		return file, nil
	}
	if len(data) < HeaderSize {
		return nil, ignored("file is %d bytes, shorter than the %d-byte WAL header", len(data), HeaderSize)
	}
	header := decodeHeader(data)
	file.Header = header
	// SQLite ignores the entire WAL when magic, page size, or header checksum
	// fails (src/wal.c:1447-1467, jumping to finished with zero frames).
	if header.Magic&0xfffffffe != MagicLittleEndian {
		return nil, ignored("magic 0x%08x is neither 0x%08x nor 0x%08x", header.Magic, MagicLittleEndian, MagicBigEndian)
	}
	pageSize := header.PageSize
	if pageSize < MinPageSize || pageSize > MaxPageSize || pageSize&(pageSize-1) != 0 {
		return nil, ignored("page size %d is not a power of two between %d and %d", pageSize, MinPageSize, MaxPageSize)
	}
	file.ComputedChecksum = sumWordPairs(header.BigEndianChecksums(), data[:HeaderSize-8], Checksum{})
	if file.ComputedChecksum != header.Checksum {
		return nil, ignored("header checksum %s does not match the computed %s", header.Checksum, file.ComputedChecksum)
	}
	// Version is checked after the header checksum, matching src/wal.c:1460-1475.
	if header.Version != FormatVersion {
		return nil, &HeaderError{Kind: ErrUnsupportedVersion, Reason: fmt.Sprintf("version %d, want %d", header.Version, FormatVersion)}
	}
	// SQLite processes only whole frame slots and ignores trailing partial bytes
	// (src/wal.c:1489).
	frameSize := int64(pageSize) + FrameHeaderSize
	slots := (int64(len(data)) - HeaderSize) / frameSize
	file.PartialFrameBytes = (int64(len(data)) - HeaderSize) % frameSize
	file.Frames = make([]Frame, 0, slots)
	chain := file.ComputedChecksum
	broken := false
	bigEndian := header.BigEndianChecksums()
	be := binary.BigEndian
	for i := int64(0); i < slots; i++ {
		offset := HeaderSize + i*frameSize
		frameHeader := data[offset : offset+FrameHeaderSize]
		page := data[offset+FrameHeaderSize : offset+frameSize]
		digest := sha256.Sum256(page)
		frame := Frame{
			Number:         int(i) + 1,
			Offset:         offset,
			Page:           be.Uint32(frameHeader[0:]),
			CommitSize:     be.Uint32(frameHeader[4:]),
			Salt1:          be.Uint32(frameHeader[8:]),
			Salt2:          be.Uint32(frameHeader[12:]),
			StoredChecksum: Checksum{be.Uint32(frameHeader[16:]), be.Uint32(frameHeader[20:])},
			PageSHA256:     hex.EncodeToString(digest[:]),
		}
		frame.SaltsMatchHeader = bytes.Equal(frameHeader[8:16], data[16:24])
		// Validate in walDecodeFrame's exact order (src/wal.c:1015-1045):
		// salts, then page != 0, then running checksum over header bytes 0..7
		// (not 16, despite the stale comment at src/wal.c:1033) plus the page.
		// Once any frame fails, every subsequent frame is ignored (src/wal.c:1511).
		switch {
		case broken:
			frame.Problem = FrameAfterInvalid
		case !frame.SaltsMatchHeader:
			frame.Problem = FrameSaltMismatch
		case frame.Page == 0:
			frame.Problem = FramePageZero
		default:
			next := sumWordPairs(bigEndian, frameHeader[:8], chain)
			next = sumWordPairs(bigEndian, page, next)
			if next != frame.StoredChecksum {
				frame.Problem = FrameChecksumMismatch
			} else {
				chain = next
			}
		}
		if frame.Problem != FrameValid {
			broken = true
		} else {
			file.ValidFrames = frame.Number
			if frame.IsCommit() {
				file.LastCommitFrame = frame.Number
			}
		}
		file.Frames = append(file.Frames, frame)
	}
	return file, nil
}

// EqualTuples reports whether a and b have identical Tuple elements in order.
// On mismatch it returns false and the 1-based frame number of the first
// difference; on equality it returns true and 0.
func EqualTuples(a, b []Tuple) (bool, int) {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false, i + 1
		}
	}
	if len(a) != len(b) {
		return false, n + 1
	}
	return true, 0
}
```

- **`diff.go`** — maps every byte offset in a WAL file to its structural `Region` (`RegionAt`) and summarizes byte differences between two WAL files (`CompareBytes`, `OnlyIn`).

<!-- include: ../internal/walfile/diff.go -->
```go
package walfile

import (
	"fmt"
	"strings"
)

// Region classifies a byte offset inside a WAL file into its structural field
// (WAL header field, frame header field, page image, or trailing length delta).
type Region int

// Structural regions of a WAL file (src/wal.c:34-57).
const (
	RegionHeaderMagic Region = iota
	RegionHeaderVersion
	RegionHeaderPageSize
	RegionHeaderCheckpointSequence
	RegionHeaderSalt
	RegionHeaderChecksum
	RegionFramePageNumber
	RegionFrameCommitSize
	RegionFrameSalt
	RegionFrameChecksum
	RegionPageData
	RegionBeyondShorterFile
	regionCount
)

// String returns a human-readable label for r.
func (r Region) String() string {
	switch r {
	case RegionHeaderMagic:
		return "header magic"
	case RegionHeaderVersion:
		return "header version"
	case RegionHeaderPageSize:
		return "header page size"
	case RegionHeaderCheckpointSequence:
		return "header checkpoint sequence"
	case RegionHeaderSalt:
		return "header salts"
	case RegionHeaderChecksum:
		return "header checksum"
	case RegionFramePageNumber:
		return "frame page number"
	case RegionFrameCommitSize:
		return "frame commit size"
	case RegionFrameSalt:
		return "frame salts"
	case RegionFrameChecksum:
		return "frame checksums"
	case RegionPageData:
		return "page data"
	case RegionBeyondShorterFile:
		return "bytes beyond the end of the shorter file"
	}
	return fmt.Sprintf("Region(%d)", int(r))
}

// RegionAt maps a byte offset in a WAL with the given pageSize to its Region.
func RegionAt(offset int64, pageSize int) Region {
	if offset < HeaderSize {
		switch {
		case offset < 4:
			return RegionHeaderMagic
		case offset < 8:
			return RegionHeaderVersion
		case offset < 12:
			return RegionHeaderPageSize
		case offset < 16:
			return RegionHeaderCheckpointSequence
		case offset < 24:
			return RegionHeaderSalt
		}
		return RegionHeaderChecksum
	}
	if pageSize <= 0 {
		return RegionPageData
	}
	within := (offset - HeaderSize) % int64(pageSize+FrameHeaderSize)
	switch {
	case within < 4:
		return RegionFramePageNumber
	case within < 8:
		return RegionFrameCommitSize
	case within < 16:
		return RegionFrameSalt
	case within < FrameHeaderSize:
		return RegionFrameChecksum
	}
	return RegionPageData
}

// ByteComparison summarizes every byte difference between two WAL files broken
// down by Region.
type ByteComparison struct {
	SizeA          int64
	SizeB          int64
	DifferingBytes int64
	ByRegion       [regionCount]int64
}

// Identical reports whether the two WAL byte slices have equal length and zero
// differing bytes.
func (c ByteComparison) Identical() bool {
	return c.SizeA == c.SizeB && c.DifferingBytes == 0
}

// OnlyIn reports whether the two WAL files have the same size and every
// differing byte falls within the listed regions (used to verify that two
// deterministic runs differ only in random salts and dependent checksums).
func (c ByteComparison) OnlyIn(regions ...Region) bool {
	allowed := map[Region]bool{}
	for _, r := range regions {
		allowed[r] = true
	}
	for r := Region(0); r < regionCount; r++ {
		if c.ByRegion[r] > 0 && !allowed[r] {
			return false
		}
	}
	return c.SizeA == c.SizeB
}

// String formats the comparison summary with per-region differing byte counts.
func (c ByteComparison) String() string {
	if c.Identical() {
		return fmt.Sprintf("byte-identical (%d bytes)", c.SizeA)
	}
	var parts []string
	for r := Region(0); r < regionCount; r++ {
		if c.ByRegion[r] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", r, c.ByRegion[r]))
		}
	}
	text := fmt.Sprintf("%d differing bytes", c.DifferingBytes)
	if c.SizeA != c.SizeB {
		text += fmt.Sprintf(", sizes %d and %d", c.SizeA, c.SizeB)
	}
	if len(parts) > 0 {
		text += ": " + strings.Join(parts, ", ")
	}
	return text
}

// CompareBytes compares two raw WAL byte slices byte by byte and attributes
// each difference to its WAL Region.
func CompareBytes(a, b []byte, pageSize int) ByteComparison {
	result := ByteComparison{SizeA: int64(len(a)), SizeB: int64(len(b))}
	shorter := min(len(a), len(b))
	longer := max(len(a), len(b))
	for i := 0; i < longer; i++ {
		if i >= shorter {
			result.DifferingBytes++
			result.ByRegion[RegionBeyondShorterFile]++
			continue
		}
		if a[i] == b[i] {
			continue
		}
		result.DifferingBytes++
		result.ByRegion[RegionAt(int64(i), pageSize)]++
	}
	return result
}
```

- **`replay.go`** — implements rung R3 (`Replay`, `ReplayFiles`). It validates the base header and page-size alignment even on zero-commit WALs, enforces `walCheckpoint`'s commit-size bound (`base + 65536 + frames*pageSize`), applies committed frames in order up to `LastCommitFrame`, skips pages past the final commit size, and truncates or zero-extends to `DatabasePages * pageSize`. It also reports every ignored frame group with its reason, refuses existing outputs or sidecar files (`ErrOutputExists`), and writes atomically via `outPath.replay-tmp` (`O_EXCL` + `fsync` + `rename`, leaving any pre-existing `.replay-tmp` untouched on `os.ErrExist`).

<!-- include: ../internal/walfile/replay.go -->
```go
package walfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var databaseMagic = []byte("SQLite format 3\x00")

// IgnoredFrames summarizes a contiguous range of WAL frames that Replay did not
// apply, along with the reason they were skipped.
type IgnoredFrames struct {
	FirstFrame int
	LastFrame  int
	Reason     string
}

// Count returns the number of frames in g.
func (g IgnoredFrames) Count() int { return g.LastFrame - g.FirstFrame + 1 }

// String formats g as either a single frame or an inclusive frame range.
func (g IgnoredFrames) String() string {
	if g.FirstFrame == g.LastFrame {
		return fmt.Sprintf("frame %d: %s", g.FirstFrame, g.Reason)
	}
	return fmt.Sprintf("frames %d-%d (%d): %s", g.FirstFrame, g.LastFrame, g.Count(), g.Reason)
}

// ReplayReport records every count and ignored-frame group produced by Replay
// or ReplayFiles.
type ReplayReport struct {
	WALIgnoredReason  string
	PageSize          int
	FramesInFile      int
	PartialFrameBytes int64
	ValidFrames       int
	AppliedFrames     int
	Transactions      int
	LastCommitFrame   int
	DatabasePages     uint32
	DistinctPages     int
	SkippedBeyondSize int
	BaseBytes         int64
	OutputBytes       int64
	Ignored           []IgnoredFrames
}

// IgnoredFrameCount returns the total number of whole frames not applied.
func (r ReplayReport) IgnoredFrameCount() int {
	total := 0
	for _, g := range r.Ignored {
		total += g.Count()
	}
	return total
}

// Lines formats r into human-readable summary lines suitable for experiment output.
func (r ReplayReport) Lines() []string {
	if r.WALIgnoredReason != "" {
		return []string{
			fmt.Sprintf("WAL ignored as a whole (%s); output = base (%d bytes)", r.WALIgnoredReason, r.BaseBytes),
		}
	}
	lines := []string{
		fmt.Sprintf("frames in file %d, valid %d, committed transactions %d, last commit frame %d (database size %d pages)",
			r.FramesInFile, r.ValidFrames, r.Transactions, r.LastCommitFrame, r.DatabasePages),
		fmt.Sprintf("applied %d frames (%d distinct pages, %d skipped beyond the committed size); base %d bytes -> output %d bytes",
			r.AppliedFrames, r.DistinctPages, r.SkippedBeyondSize, r.BaseBytes, r.OutputBytes),
	}
	if len(r.Ignored) == 0 && r.PartialFrameBytes == 0 {
		lines = append(lines, "ignored: none")
	}
	for _, g := range r.Ignored {
		lines = append(lines, "ignored "+g.String())
	}
	if r.PartialFrameBytes > 0 {
		lines = append(lines, fmt.Sprintf("ignored %d trailing bytes: a partial frame", r.PartialFrameBytes))
	}
	return lines
}

func basePageSize(base []byte) (int, error) {
	if len(base) == 0 {
		return 0, nil
	}
	if len(base) < 100 || !bytes.Equal(base[:16], databaseMagic) {
		return 0, fmt.Errorf("%w: %d bytes without the \"SQLite format 3\" header", ErrBaseNotDatabase, len(base))
	}
	// SQLite stores page size 65,536 as big-endian 1 in database header bytes 16..17.
	size := int(base[16])<<8 | int(base[17])
	if size == 1 {
		size = 65536
	}
	if size < MinPageSize || size > MaxPageSize || size&(size-1) != 0 {
		return 0, fmt.Errorf("%w: header page size field %d", ErrBaseNotDatabase, size)
	}
	return size, nil
}

func ignoredGroups(file *File) []IgnoredFrames {
	var groups []IgnoredFrames
	if file.ValidFrames > file.LastCommitFrame {
		groups = append(groups, IgnoredFrames{
			FirstFrame: file.LastCommitFrame + 1,
			LastFrame:  file.ValidFrames,
			Reason:     "valid but uncommitted: no commit frame follows",
		})
	}
	if file.ValidFrames < len(file.Frames) {
		stop := file.Frames[file.ValidFrames]
		groups = append(groups, IgnoredFrames{FirstFrame: stop.Number, LastFrame: stop.Number, Reason: stop.Problem.String()})
		if stop.Number < len(file.Frames) {
			rest := IgnoredFrames{FirstFrame: stop.Number + 1, LastFrame: len(file.Frames)}
			older := 0
			for _, frame := range file.Frames[stop.Number:] {
				if !frame.SaltsMatchHeader {
					older++
				}
			}
			rest.Reason = fmt.Sprintf("%s (%d of them carry other salts)", FrameAfterInvalid, older)
			groups = append(groups, rest)
		}
	}
	return groups
}

// Replay applies all committed WAL frames onto base in plain Go, mirroring
// SQLite's checkpoint backfill in walCheckpoint (src/wal.c:2282-2335).
// A non-empty base must carry the 100-byte SQLite header with a power-of-two
// page size in 512..65536 (ErrBaseNotDatabase); page-size mismatch
// (ErrPageSizeMismatch) and non-page-multiple base length (ErrBaseSize) are
// validated even when the WAL has no commit frame (an empty WAL checks base
// length against the base's own page size). Frames are applied in order up to
// LastCommitFrame so later versions of a page overwrite earlier ones; frames
// targeting pages past the final commit size are skipped (src/wal.c:2312), and
// the result is truncated or zero-extended to the final commit size
// (src/wal.c:2328-2331).
func Replay(base, wal []byte) ([]byte, ReplayReport, error) {
	report := ReplayReport{BaseBytes: int64(len(base))}
	pageSize, err := basePageSize(base)
	if err != nil {
		return nil, report, err
	}
	file, err := Parse(wal)
	if errors.Is(err, ErrWALIgnored) {
		report.WALIgnoredReason = err.Error()
		report.OutputBytes = int64(len(base))
		return bytes.Clone(base), report, nil
	}
	if err != nil {
		return nil, report, err
	}
	report.PageSize = file.PageSize()
	report.FramesInFile = len(file.Frames)
	report.PartialFrameBytes = file.PartialFrameBytes
	report.ValidFrames = file.ValidFrames
	report.LastCommitFrame = file.LastCommitFrame
	report.Transactions = len(file.Commits())
	report.DatabasePages = file.DatabasePages()
	report.Ignored = ignoredGroups(file)
	// Validate base page size and whole-page alignment before returning early
	// on zero commits so header-only, uncommitted, and empty WALs still reject
	// mismatched or truncated base files.
	walPageSize := file.PageSize()
	if len(base) > 0 && walPageSize != 0 && pageSize != walPageSize {
		return nil, report, fmt.Errorf("%w: base %d, WAL %d", ErrPageSizeMismatch, pageSize, walPageSize)
	}
	if sizeUnit := max(walPageSize, pageSize); sizeUnit != 0 && len(base)%sizeUnit != 0 {
		return nil, report, fmt.Errorf("%w: %d bytes, page size %d", ErrBaseSize, len(base), sizeUnit)
	}
	if file.LastCommitFrame == 0 {
		report.OutputBytes = int64(len(base))
		return bytes.Clone(base), report, nil
	}
	// Reject impossible commit sizes using walCheckpoint's exact bound
	// (src/wal.c:2288-2293: base + 65536 for a possible pending-byte page +
	// frames*pageSize). This also prevents a corrupt header from triggering a
	// huge allocation.
	finalSize := int64(report.DatabasePages) * int64(walPageSize)
	if limit := int64(len(base)) + 65536 + int64(file.LastCommitFrame)*int64(walPageSize); finalSize > limit {
		return nil, report, fmt.Errorf("%w: commit size %d pages needs %d bytes, more than base %d + 65536 + %d frames x %d", ErrCommitSizeTooLarge, report.DatabasePages, finalSize, len(base), file.LastCommitFrame, walPageSize)
	}
	out := make([]byte, max(int64(len(base)), finalSize))
	copy(out, base)
	distinct := map[uint32]bool{}
	for _, frame := range file.CommittedFrames() {
		if frame.Page > report.DatabasePages {
			report.SkippedBeyondSize++
			continue
		}
		image, err := file.PageImage(frame.Number)
		if err != nil {
			return nil, report, err
		}
		copy(out[int64(frame.Page-1)*int64(walPageSize):], image)
		distinct[frame.Page] = true
		report.AppliedFrames++
	}
	report.DistinctPages = len(distinct)
	out = out[:finalSize]
	report.OutputBytes = finalSize
	return out, report, nil
}

// ReplayFiles replays walPath onto basePath and atomically writes the rebuilt
// database to outPath via a synced temporary file (outPath + ".replay-tmp",
// opened with O_EXCL) and rename. A missing walPath is treated as zero frames.
// Existing outPath or sibling -wal, -journal, or -shm files are refused with
// ErrOutputExists so SQLite cannot later apply stale sidecar files. If
// outPath + ".replay-tmp" already exists, ReplayFiles returns an os.ErrExist
// error and leaves that pre-existing file untouched; the temporary file is
// removed on error only when this call created it.
func ReplayFiles(basePath, walPath, outPath string) (ReplayReport, error) {
	for _, path := range []string{outPath, outPath + "-wal", outPath + "-journal", outPath + "-shm"} {
		if _, err := os.Lstat(path); err == nil {
			return ReplayReport{}, fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(path))
		}
	}
	base, err := os.ReadFile(basePath)
	if err != nil {
		return ReplayReport{}, err
	}
	wal, err := os.ReadFile(walPath)
	if errors.Is(err, os.ErrNotExist) {
		wal = nil
	} else if err != nil {
		return ReplayReport{}, err
	}
	out, report, err := Replay(base, wal)
	if err != nil {
		return report, err
	}
	temp := outPath + ".replay-tmp"
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return report, err
	}
	if err := writeSynced(f, out); err != nil {
		os.Remove(temp)
		return report, err
	}
	if err := os.Rename(temp, outPath); err != nil {
		os.Remove(temp)
		return report, err
	}
	return report, nil
}

func writeSynced(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
```

- **`shm.go`** — decodes the native-endian `-shm` wal-index (`ParseIndex`, `ParseIndexFile`). It detects host byte order from version word `3007000`, catches torn headers where copy 1 is zeroed and copy 2 is populated (the midpoint of `walIndexWriteHdr`), and compares both raw 48-byte header copies including the `unused` padding field (`HeadersAgree`). It also decodes `WalCkptInfo` (`nBackfill`, `aReadMark[0..4]`, `nBackfillAttempted`) and maps frame numbers across block boundaries (`4062` entries in block 0, `4096` per later 32 KiB block) via `FramePage` and `FramePages`.

<!-- include: ../internal/walfile/shm.go -->
```go
package walfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// On-disk wal-index (-shm) layout constants (src/wal.c:159-178, 311-474;
// src/os_unix.c:4602-4603).
const (
	// IndexHeaderSize is the byte size of one WalIndexHdr struct (src/wal.c:321-333).
	IndexHeaderSize = 48
	// IndexHeaderCopies is the number of consecutive WalIndexHdr copies at the
	// start of -shm; readers compare both copies to detect torn writes (src/wal.c:942-954).
	IndexHeaderCopies = 2
	// CheckpointInfoOffset is the byte offset of WalCkptInfo immediately after
	// the two WalIndexHdr copies (96, src/wal.c:450).
	CheckpointInfoOffset = IndexHeaderSize * IndexHeaderCopies
	// CheckpointInfoSize is the byte size of WalCkptInfo (40, src/wal.c:394-400).
	CheckpointInfoSize = 40
	// IndexHeaderRegionSize is the 136-byte header region (WALINDEX_HDR_SIZE,
	// src/wal.c:474) preceding frame 1's page-number slot in block 0.
	IndexHeaderRegionSize = CheckpointInfoOffset + CheckpointInfoSize
	// ReadMarkCount is the number of reader lock/mark slots (WAL_NREADER = 5,
	// src/wal.c:299). Slot 0 is always 0 (database-only readers); slots 1..4
	// hold WAL frame marks.
	ReadMarkCount = 5
	// ReadMarkNotUsed is the sentinel value 0xffffffff for an unoccupied read
	// mark slot (READMARK_NOT_USED, src/wal.c:401).
	ReadMarkNotUsed = 0xffffffff
	// IndexBlockSize is the 32 KiB size of each wal-index shared-memory block.
	IndexBlockSize = 32768
	// FramesPerIndexBlock is HASHTABLE_NPAGE (4096): page-number slots in every
	// index block after block 0 (src/wal.c:166).
	FramesPerIndexBlock = 4096
	// FramesInFirstIndexBlock is HASHTABLE_NPAGE_ONE (4062): page-number slots
	// in block 0 after subtracting the 136-byte header region (src/wal.c:167).
	FramesInFirstIndexBlock = FramesPerIndexBlock - IndexHeaderRegionSize/4
	// LockBytesOffset is byte offset 120 (WALINDEX_LOCK_OFFSET / UNIX_SHM_BASE),
	// where the unix VFS places fcntl byte-range locks (src/wal.c:458, 473;
	// src/os_unix.c:4602).
	LockBytesOffset = 120
	// LockByteCount is the 8 lock bytes covering WRITE, CKPT, RECOVER, and
	// READ(0..4) (src/wal.c:294-299, 458-460).
	LockByteCount = 8
	// DeadManSwitchOffset is byte offset 128 (UNIX_SHM_DMS, src/os_unix.c:4603),
	// locked by the first opener to detect whether -shm must be truncated and
	// recovered.
	DeadManSwitchOffset = LockBytesOffset + LockByteCount
	// IndexFormatVersion is WALINDEX_MAX_VERSION (3007000, src/wal.c:948).
	IndexFormatVersion  = 3007000
	indexChecksumLength = 40
)

// IndexHeader holds one decoded 48-byte WalIndexHdr copy from the -shm file
// (src/wal.c:321-333). Multi-byte fields use the host's native byte order
// except Salt1 and Salt2, which are copied as raw big-endian bytes from the WAL
// header (src/wal.c:144-146, 1457).
type IndexHeader struct {
	Version           uint32
	Change            uint32
	IsInit            uint8
	BigEndianChecksum uint8
	PageSizeField     uint16
	MaxFrame          uint32
	Pages             uint32
	FrameChecksum     Checksum
	Salt1             uint32
	Salt2             uint32
	Checksum          Checksum
	ChecksumOK        bool
}

// PageSize decodes PageSizeField using SQLite's walPagesize formula
// (src/wal.c:2131-2133), mapping stored value 1 to 65,536 bytes.
func (h IndexHeader) PageSize() int {
	return int(h.PageSizeField&0xfe00) + int(h.PageSizeField&0x0001)<<16
}

// String formats the key fields of h on one line.
func (h IndexHeader) String() string {
	return fmt.Sprintf("version=%d change=%d is-init=%d page-size=%d mxFrame=%d nPage=%d salt-1=0x%08x salt-2=0x%08x checksum-ok=%v",
		h.Version, h.Change, h.IsInit, h.PageSize(), h.MaxFrame, h.Pages, h.Salt1, h.Salt2, h.ChecksumOK)
}

// CheckpointInfo holds the decoded WalCkptInfo fields at -shm bytes 96..135
// (src/wal.c:394-401): nBackfill, the five read-mark slots, and
// nBackfillAttempted. Lock bytes 120..127 are never read or written by SQLite.
type CheckpointInfo struct {
	Backfill          uint32
	ReadMarks         [ReadMarkCount]uint32
	BackfillAttempted uint32
}

// ReadMarkText formats a single read-mark slot, printing ReadMarkNotUsed
// (0xffffffff) as "unused".
func ReadMarkText(mark uint32) string {
	if mark == ReadMarkNotUsed {
		return "unused"
	}
	return fmt.Sprintf("%d", mark)
}

// ReadMarksText formats all five read-mark slots as "[0]=... [4]=...".
func (c CheckpointInfo) ReadMarksText() string {
	parts := make([]string, len(c.ReadMarks))
	for i, mark := range c.ReadMarks {
		parts[i] = fmt.Sprintf("[%d]=%s", i, ReadMarkText(mark))
	}
	return strings.Join(parts, " ")
}

// String formats nBackfill, nBackfillAttempted, and all read marks on one line.
func (c CheckpointInfo) String() string {
	return fmt.Sprintf("nBackfill=%d nBackfillAttempted=%d read-marks %s", c.Backfill, c.BackfillAttempted, c.ReadMarksText())
}

// Index is a decoded wal-index (-shm) snapshot containing both WalIndexHdr
// copies, CheckpointInfo, detected native byte order, and the backing bytes for
// page-number array lookups.
type Index struct {
	Size        int
	Initialized bool
	BigEndian   bool
	Headers     [IndexHeaderCopies]IndexHeader
	Checkpoint  CheckpointInfo
	order       binary.ByteOrder
	data        []byte
}

// Header returns copy 1 of WalIndexHdr (written second after the memory barrier
// in walIndexWriteHdr, src/wal.c:951-953).
func (x *Index) Header() IndexHeader { return x.Headers[0] }

// HeadersAgree reports whether both raw 48-byte WalIndexHdr copies in -shm are
// byte-for-byte identical, including the 4-byte unused field at bytes 4..7 and
// 52..55 (matching walIndexTryHdr's memcmp at src/wal.c:2619).
func (x *Index) HeadersAgree() bool {
	return bytes.Equal(x.data[:IndexHeaderSize], x.data[IndexHeaderSize:IndexHeaderCopies*IndexHeaderSize])
}

// ByteOrderName returns "big-endian" or "little-endian" for the detected host
// byte order of the -shm file.
func (x *Index) ByteOrderName() string {
	if x.BigEndian {
		return "big-endian"
	}
	return "little-endian"
}

// ParseIndexFile reads a -shm file from disk and decodes it with ParseIndex.
func ParseIndexFile(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIndex(data)
}

// ParseIndex decodes a raw wal-index (-shm) byte slice. Because -shm uses the
// host's native byte order (src/wal.c:144-146), the byte order is detected from
// the version word (3007000). When both 48-byte header copies are all zero,
// ParseIndex returns Initialized=false with a nil error (a freshly truncated or
// zeroed -shm before recovery). When copy 1 (bytes 0..47) is all zero and copy
// 2 (bytes 48..95) carries version 3007000 in either byte order—the midpoint of
// walIndexWriteHdr (src/wal.c:942-954)—ParseIndex decodes both headers and
// Checkpoint using copy 2's byte order and returns a nil error with
// Initialized=false and HeadersAgree()==false (FramePage refuses lookups with
// ErrFrameNotIndexed). If copy 1 is zero and copy 2 starts with any other
// non-zero word, ParseIndex returns ErrIndexVersion.
func ParseIndex(data []byte) (*Index, error) {
	if len(data) < IndexHeaderRegionSize {
		return nil, fmt.Errorf("%w: %d bytes, need %d", ErrShortIndex, len(data), IndexHeaderRegionSize)
	}
	index := &Index{Size: len(data), data: data}
	switch {
	case binary.LittleEndian.Uint32(data) == IndexFormatVersion:
		index.order = binary.LittleEndian
	case binary.BigEndian.Uint32(data) == IndexFormatVersion:
		index.order = binary.BigEndian
		index.BigEndian = true
	case allZero(data[:IndexHeaderSize]):
		// walIndexWriteHdr writes copy 2 before the barrier and copy 1 after
		// (src/wal.c:951-953), so a zero copy 1 beside a populated copy 2 is a
		// torn header rather than an uninitialized file.
		second := data[IndexHeaderSize:]
		switch {
		case allZero(second[:IndexHeaderSize]):
			index.order = binary.LittleEndian
			return index, nil
		case binary.LittleEndian.Uint32(second) == IndexFormatVersion:
			index.order = binary.LittleEndian
		case binary.BigEndian.Uint32(second) == IndexFormatVersion:
			index.order = binary.BigEndian
			index.BigEndian = true
		default:
			return nil, fmt.Errorf("%w: first header zero, second header word %08x", ErrIndexVersion, binary.BigEndian.Uint32(second))
		}
		index.decodeHeaderRegion()
		return index, nil
	default:
		return nil, fmt.Errorf("%w: first word %08x", ErrIndexVersion, binary.BigEndian.Uint32(data))
	}
	index.Initialized = true
	index.decodeHeaderRegion()
	return index, nil
}

func (x *Index) decodeHeaderRegion() {
	data := x.data
	for i := range x.Headers {
		x.Headers[i] = decodeIndexHeader(data[i*IndexHeaderSize:(i+1)*IndexHeaderSize], x.order, x.BigEndian)
	}
	info := data[CheckpointInfoOffset:]
	x.Checkpoint.Backfill = x.order.Uint32(info[0:])
	for i := range x.Checkpoint.ReadMarks {
		x.Checkpoint.ReadMarks[i] = x.order.Uint32(info[4+4*i:])
	}
	x.Checkpoint.BackfillAttempted = x.order.Uint32(info[32:])
}

func decodeIndexHeader(raw []byte, order binary.ByteOrder, bigEndian bool) IndexHeader {
	h := IndexHeader{
		Version:           order.Uint32(raw[0:]),
		Change:            order.Uint32(raw[8:]),
		IsInit:            raw[12],
		BigEndianChecksum: raw[13],
		PageSizeField:     order.Uint16(raw[14:]),
		MaxFrame:          order.Uint32(raw[16:]),
		Pages:             order.Uint32(raw[20:]),
		FrameChecksum:     Checksum{order.Uint32(raw[24:]), order.Uint32(raw[28:])},
		// Salts are copied verbatim from the big-endian WAL header (src/wal.c:1457).
		Salt1:    binary.BigEndian.Uint32(raw[32:]),
		Salt2:    binary.BigEndian.Uint32(raw[36:]),
		Checksum: Checksum{order.Uint32(raw[40:]), order.Uint32(raw[44:])},
	}
	// walIndexWriteHdr checksums bytes 0..39 in native host byte order (src/wal.c:949).
	h.ChecksumOK = sumWordPairs(bigEndian, raw[:indexChecksumLength], Checksum{}) == h.Checksum
	return h
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// indexSlot maps a 1-based WAL frame number to its 32 KiB index block and byte
// offset within that block's page-number array (src/wal.c:159-177).
func indexSlot(frame uint32) (block int, entryOffset int) {
	if frame <= FramesInFirstIndexBlock {
		return 0, IndexHeaderRegionSize + int(frame-1)*4
	}
	rest := frame - FramesInFirstIndexBlock - 1
	return 1 + int(rest/FramesPerIndexBlock), int(rest%FramesPerIndexBlock) * 4
}

// FramePage returns the database page number recorded in the wal-index
// page-mapping array for 1-based frame.
func (x *Index) FramePage(frame uint32) (uint32, error) {
	if !x.Initialized {
		return 0, fmt.Errorf("%w: the wal-index is not initialized", ErrFrameNotIndexed)
	}
	if frame == 0 {
		return 0, fmt.Errorf("%w: frame numbers start at 1", ErrFrameNotIndexed)
	}
	block, offset := indexSlot(frame)
	position := block*IndexBlockSize + offset
	if position+4 > len(x.data) {
		return 0, fmt.Errorf("%w: frame %d maps to byte %d of a %d-byte wal-index", ErrFrameNotIndexed, frame, position, len(x.data))
	}
	return x.order.Uint32(x.data[position:]), nil
}

// FramePages returns the page-mapping array entries for frames 1..maxFrame.
func (x *Index) FramePages(maxFrame uint32) ([]uint32, error) {
	var pages []uint32
	for frame := uint32(1); frame <= maxFrame; frame++ {
		page, err := x.FramePage(frame)
		if err != nil {
			return pages, err
		}
		pages = append(pages, page)
	}
	return pages, nil
}
```

- **`showwal.go`** — parses output from SQLite's pinned `showwal` tool (`ParseShowwal`) and cross-checks page size, frame counts, per-frame fields, and checksum agreement in both directions (`CompareWithShowwal`).

<!-- include: ../internal/walfile/showwal.go -->
```go
package walfile

import (
	"bufio"
	"fmt"
	"strings"
)

// ShowwalFrame holds one frame line decoded from SQLite's in-tree showwal tool
// (tool/showwal.c:236).
type ShowwalFrame struct {
	Number           int
	Page             uint32
	CommitSize       uint32
	Salt1            uint32
	Salt2            uint32
	StoredChecksum   Checksum
	ChecksumAgrees   bool
	ExpectedChecksum Checksum
}

// ShowwalListing holds the parsed header summary and frame lines printed by
// running `showwal <file>`.
type ShowwalListing struct {
	PageSize        int
	AvailableFrames int
	Frames          []ShowwalFrame
}

// ParseShowwal parses stdout from SQLite's pinned showwal utility.
// showwal checks the running checksum chain and prints " should be ..." on
// mismatches, but does not validate salts (tool/showwal.c:236, 567).
func ParseShowwal(text string) (ShowwalListing, error) {
	var listing ShowwalListing
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "Pagesize: "):
			if _, err := fmt.Sscanf(line, "Pagesize: %d", &listing.PageSize); err != nil {
				return listing, fmt.Errorf("showwal line %q: %w", line, err)
			}
		case strings.HasPrefix(line, "Available pages: 1.."):
			if _, err := fmt.Sscanf(line, "Available pages: 1..%d", &listing.AvailableFrames); err != nil {
				return listing, fmt.Errorf("showwal line %q: %w", line, err)
			}
		case strings.HasPrefix(line, "Frame ") && !strings.Contains(line, "(offsets"):
			frame, err := parseShowwalFrame(line)
			if err != nil {
				return listing, err
			}
			listing.Frames = append(listing.Frames, frame)
		}
	}
	if err := scanner.Err(); err != nil {
		return listing, err
	}
	if listing.PageSize == 0 {
		return listing, fmt.Errorf("showwal output has no Pagesize line")
	}
	return listing, nil
}

func parseShowwalFrame(line string) (ShowwalFrame, error) {
	var frame ShowwalFrame
	main, expected, hasExpected := strings.Cut(line, " should be ")
	_, err := fmt.Sscanf(main, "Frame %d: %d %d 0x%x,%x 0x%x,%x",
		&frame.Number, &frame.Page, &frame.CommitSize, &frame.Salt1, &frame.Salt2,
		&frame.StoredChecksum[0], &frame.StoredChecksum[1])
	if err != nil {
		return frame, fmt.Errorf("showwal line %q: %w", line, err)
	}
	frame.ChecksumAgrees = !hasExpected
	frame.ExpectedChecksum = frame.StoredChecksum
	if hasExpected {
		if _, err := fmt.Sscanf(expected, "0x%x,%x", &frame.ExpectedChecksum[0], &frame.ExpectedChecksum[1]); err != nil {
			return frame, fmt.Errorf("showwal line %q: %w", line, err)
		}
	}
	return frame, nil
}

// CompareWithShowwal cross-checks f against a ShowwalListing from SQLite's
// in-tree showwal tool, returning one description per discrepancy. It checks
// checksum agreement in both directions: every frame walfile accepts as valid
// must also verify in showwal, and if walfile rejects the first invalid frame
// with FrameChecksumMismatch while showwal reports the checksum agrees, that
// disagreement is reported as well.
func (f *File) CompareWithShowwal(listing ShowwalListing) []string {
	var problems []string
	if listing.PageSize != f.PageSize() {
		problems = append(problems, fmt.Sprintf("page size: showwal %d, walfile %d", listing.PageSize, f.PageSize()))
	}
	if listing.AvailableFrames != len(f.Frames) || len(listing.Frames) != len(f.Frames) {
		problems = append(problems, fmt.Sprintf("frame count: showwal %d (%d listed), walfile %d", listing.AvailableFrames, len(listing.Frames), len(f.Frames)))
	}
	for i := 0; i < min(len(listing.Frames), len(f.Frames)); i++ {
		s, w := listing.Frames[i], f.Frames[i]
		if s.Number != w.Number || s.Page != w.Page || s.CommitSize != w.CommitSize || s.Salt1 != w.Salt1 || s.Salt2 != w.Salt2 || s.StoredChecksum != w.StoredChecksum {
			problems = append(problems, fmt.Sprintf("frame %d: showwal page=%d commit=%d salts=%08x,%08x checksum=%s; walfile page=%d commit=%d salts=%08x,%08x checksum=%s",
				w.Number, s.Page, s.CommitSize, s.Salt1, s.Salt2, s.StoredChecksum, w.Page, w.CommitSize, w.Salt1, w.Salt2, w.StoredChecksum))
		}
		if i < f.ValidFrames && !s.ChecksumAgrees {
			problems = append(problems, fmt.Sprintf("frame %d: walfile says valid, showwal computes checksum %s", w.Number, s.ExpectedChecksum))
		}
		if i == f.ValidFrames && w.Problem == FrameChecksumMismatch && s.ChecksumAgrees {
			problems = append(problems, fmt.Sprintf("frame %d: walfile says checksum mismatch, showwal says the checksum agrees", w.Number))
		}
	}
	return problems
}
```

- **`livecopy.go`** — safely copies open `.db`, `-wal`, and `-shm` files by re-executing the current binary in `LiveCopyChildMode` (`ServeLiveCopyChild`, `CopyLiveFiles`, `ReadLiveState`). Because Unix SQLite stores WAL locks as POSIX `fcntl` byte-range locks on `-shm` (`src/os_unix.c:4703-4750`), calling `os.Open` and `Close` on `.db` or `-shm` inside the parent process would drop every lock that process holds on that file. Delegating the copy to a short-lived child process keeps the parent's locks intact, and `ReadLiveState` removes its temporary copies on every return path.

<!-- include: ../internal/walfile/livecopy.go -->
```go
package walfile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// LiveCopyChildMode is the first CLI argument used when CopyLiveFiles re-execs
// the current binary as a child process to copy open SQLite files safely.
const LiveCopyChildMode = "walfile-live-copy-child"

// CopyPair names one source path and destination path for CopyLiveFiles.
type CopyPair struct {
	From string
	To   string
}

// CopyResult reports whether a CopyPair's source existed and how many bytes
// were copied.
type CopyResult struct {
	From    string
	To      string
	Present bool
	Bytes   int64
}

// ServeLiveCopyChild handles the child-process file-copy mode and exits the
// process when args[1] == LiveCopyChildMode. Call it at the start of main or
// TestMain in any binary that invokes CopyLiveFiles or ReadLiveState.
//
// Copying in a child process avoids a POSIX lock hazard: closing any file
// descriptor to the database or -shm inside the process that holds an SQLite
// connection releases all POSIX fcntl locks that process holds on that inode
// (src/os_unix.c:1189-1278, 4703-4750).
func ServeLiveCopyChild(args []string) bool {
	if len(args) < 2 || args[1] != LiveCopyChildMode {
		return false
	}
	rest := args[2:]
	if len(rest)%2 != 0 {
		fmt.Fprintln(os.Stderr, "live copy child: arguments must be pairs of source and destination")
		os.Exit(2)
	}
	out := bufio.NewWriter(os.Stdout)
	for i := 0; i < len(rest); i += 2 {
		size, err := copyOne(rest[i], rest[i+1])
		switch {
		case isMissingSource(err):
			// Remove any stale copy at the destination so callers never read
			// leftover bytes from an earlier sample.
			if removeErr := os.Remove(rest[i+1]); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "live copy child: %v\n", removeErr)
				os.Exit(1)
			}
			fmt.Fprintf(out, "missing\n")
		case err != nil:
			fmt.Fprintf(os.Stderr, "live copy child: %v\n", err)
			os.Exit(1)
		default:
			fmt.Fprintf(out, "copied %d\n", size)
		}
	}
	if err := out.Flush(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
	return true
}

func copyOne(from, to string) (int64, error) {
	source, err := os.Open(from)
	if err != nil {
		return 0, &sourceError{err}
	}
	defer source.Close()
	destination, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	size, err := io.Copy(destination, source)
	if closeErr := destination.Close(); err == nil {
		err = closeErr
	}
	return size, err
}

// CopyLiveFiles spawns the current executable in LiveCopyChildMode to copy each
// source file to its destination without opening those files in the parent
// process that holds live SQLite connections. Passing an empty pairs slice
// returns nil, nil without spawning a child process.
func CopyLiveFiles(ctx context.Context, pairs []CopyPair) ([]CopyResult, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{LiveCopyChildMode}
	for _, p := range pairs {
		args = append(args, p.From, p.To)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("live copy child: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(pairs) {
		return nil, fmt.Errorf("live copy child: %d result lines for %d files", len(lines), len(pairs))
	}
	results := make([]CopyResult, len(pairs))
	for i, line := range lines {
		results[i] = CopyResult{From: pairs[i].From, To: pairs[i].To}
		if line == "missing" {
			continue
		}
		size, ok := strings.CutPrefix(line, "copied ")
		if !ok {
			return nil, fmt.Errorf("live copy child: unexpected line %q", line)
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("live copy child: %w", err)
		}
		results[i].Present = true
		results[i].Bytes = n
	}
	return results, nil
}

// DatabaseFileSet returns CopyPairs for a database file and its -wal and -shm
// sidecar files copied into directory under name.
func DatabaseFileSet(database, directory, name string) []CopyPair {
	var pairs []CopyPair
	for _, suffix := range []string{"", "-wal", "-shm"} {
		pairs = append(pairs, CopyPair{From: database + suffix, To: filepath.Join(directory, name+suffix)})
	}
	return pairs
}

type sourceError struct{ err error }

func (e *sourceError) Error() string { return "source: " + e.err.Error() }

func (e *sourceError) Unwrap() error { return e.err }

func isMissingSource(err error) bool {
	var source *sourceError
	return errors.As(err, &source) && errors.Is(err, os.ErrNotExist)
}

// LiveState holds a parsed snapshot of a live database's -wal and -shm files
// sampled through CopyLiveFiles.
type LiveState struct {
	WALPresent   bool
	WALSize      int64
	WAL          *File
	WALProblem   string
	IndexPresent bool
	Index        *Index
}

// ReadLiveState copies database's -wal and -shm into scratchDirectory via a
// child process, parses both, and removes both temporary copies on every return
// path, including when parsing -shm fails.
func ReadLiveState(ctx context.Context, database, scratchDirectory string) (LiveState, error) {
	walCopy := filepath.Join(scratchDirectory, "live-state-copy-wal")
	indexCopy := filepath.Join(scratchDirectory, "live-state-copy-shm")
	results, err := CopyLiveFiles(ctx, []CopyPair{{database + "-wal", walCopy}, {database + "-shm", indexCopy}})
	if err != nil {
		return LiveState{}, err
	}
	defer os.Remove(walCopy)
	defer os.Remove(indexCopy)
	var state LiveState
	if results[0].Present {
		state.WALPresent = true
		state.WALSize = results[0].Bytes
		wal, err := ParseFile(walCopy)
		if err != nil {
			state.WALProblem = err.Error()
		}
		state.WAL = wal
	}
	if results[1].Present {
		state.IndexPresent = true
		index, err := ParseIndexFile(indexCopy)
		if err != nil {
			return state, fmt.Errorf("wal-index copy: %w", err)
		}
		state.Index = index
	}
	for _, path := range []string{walCopy, indexCopy} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return state, err
		}
	}
	return state, nil
}
```

### 5.2 Experiment 5.2 — WAL replay, rung R3 (`5.2-wal-replay`)

- **Claim:** A base database file plus the committed frames of its WAL holds the complete database state. Applying committed frames in order in plain Go (`internal/walfile`), stopping at `LastCommitFrame`, and truncating to that commit's database size produces a file equal at **E1 bytes** to SQLite's own `wal_checkpoint(TRUNCATE)`—at a quiet moment (Part A), mid-transaction with valid spilled uncommitted frames at the tail (Part B), and after a `ROLLBACK` that leaves those frames in `-wal` (Part C). Furthermore, `-shm` is disposable: SQLite recovers the exact same bytes whether `-shm` is absent or replaced by a stale `-shm` copied from another database. *(Scope boundary: raw `base + WAL` files alone cannot detect a base from an earlier WAL generation because the `.db` header stores no WAL salt or generation; that check requires capture metadata recorded at capture time in Chapter 9's `internal/walship`.)*
- **Evidence form:** `EXACT` (every stored CSV table must match byte for byte on every run; run-dependent `info` tables are excluded).
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the DuckDB walkthrough; add `-i` to stay in the DuckDB shell afterwards with all tables loaded):
  ```text
  make run EXP=5.2
  ```

<!-- include: experiments/5.2-wal-replay/main.go -->
```go
// Experiment 5.2 (wal-replay, rung R3) proves that a base database file plus
// the committed frames of its WAL equals the complete database state. It
// replays copies taken at a quiet moment, mid-transaction with spilled pages,
// and after a rollback in plain Go (no SQLite), and proves E1 byte equality
// against SQLite's own TRUNCATE checkpoint and recovery (both without -shm and
// with a stale -shm from another database).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.2
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	quietSteps        = 8
	spillBaseStep     = 8
	spillCommitted    = 10
	spillCacheSize    = 10
	spillExtraUpdate  = "UPDATE documents SET title = upper(title)"
	databaseName      = "r.db"
	replayOutputName  = "replayed.db"
	recoveryDirectory = "sqlite-recovery"
	quietTotalFrames  = 155
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.2" runs both.
// part labels the rows written while a part runs; seq orders rows across tables.
var (
	set    *results.Set
	part   string
	seq    int
	tables struct {
		captures, replays, sqliteRuns, comparisons, recipeMatches, walStates, staleShm *results.Table
	}
)

func openTables() {
	tables.captures = set.Table("captures", "seq", "part", "capture", "db_bytes", "wal_bytes")
	tables.replays = set.Table("replays", "seq", "part", "capture", "frames_in_file", "valid_frames", "transactions", "last_commit_frame", "database_pages", "applied_frames", "distinct_pages", "skipped_beyond_size", "base_bytes", "output_bytes", "ignored_frames", "ignored", "partial_bytes", "wal_ignored_reason")
	tables.sqliteRuns = set.Table("sqlite_runs", "seq", "part", "run", "shm", "user_version", "busy", "log_frames", "checkpointed")
	tables.comparisons = set.Table("comparisons", "seq", "part", "against", "level", "equal", "sha256", "user_version", "integrity_check", "unchanged_by_open")
	tables.recipeMatches = set.Table("recipe_matches", "seq", "part", "subject", "step", "exact")
	tables.walStates = set.Table("wal_states", "seq", "part", "moment", "wal_bytes", "frames", "valid_frames", "last_commit_frame", "mx_frame")
	tables.staleShm = set.Table("stale_shm", "mx_frame", "n_page")
}

func nextSeq() int {
	seq++
	return seq
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
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

func mkdir(path string) string {
	if err := os.MkdirAll(path, 0o755); err != nil {
		fatal("mkdir", err)
	}
	return path
}

func liveCopy(ctx context.Context, path, directory, capture string) {
	mkdir(directory)
	results := must(walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
		{From: path, To: filepath.Join(directory, databaseName)},
		{From: path + "-wal", To: filepath.Join(directory, databaseName+"-wal")},
	}))("live copy")
	for _, r := range results {
		if !r.Present {
			fatal("live copy", fmt.Errorf("%s missing", filepath.Base(r.From)))
		}
	}
	tables.captures.Row(nextSeq(), part, capture, results[0].Bytes, results[1].Bytes)
}

func replayCopy(directory, capture string) (string, walfile.ReplayReport) {
	output := filepath.Join(directory, replayOutputName)
	report := must(walfile.ReplayFiles(filepath.Join(directory, databaseName), filepath.Join(directory, databaseName+"-wal"), output))("replay")
	ignored := make([]string, len(report.Ignored))
	for i, g := range report.Ignored {
		ignored[i] = g.String()
	}
	tables.replays.Row(nextSeq(), part, capture, report.FramesInFile, report.ValidFrames, report.Transactions, report.LastCommitFrame, report.DatabasePages,
		report.AppliedFrames, report.DistinctPages, report.SkippedBeyondSize, report.BaseBytes, report.OutputBytes, report.IgnoredFrameCount(), strings.Join(ignored, "; "),
		report.PartialFrameBytes, report.WALIgnoredReason)
	return output, report
}

// sqliteRecoveryOfCopy copies the captured base and -wal (and optionally a
// stale -shm from another database) into a fresh directory, lets SQLite open
// and recover it, checkpoints with TRUNCATE, and returns the resulting path.
func sqliteRecoveryOfCopy(ctx context.Context, directory, name, staleIndex string) string {
	target := mkdir(filepath.Join(directory, name))
	path := filepath.Join(target, databaseName)
	for _, suffix := range []string{"", "-wal"} {
		data := must(os.ReadFile(filepath.Join(directory, databaseName+suffix)))("read copy")
		if err := os.WriteFile(path+suffix, data, 0o644); err != nil {
			fatal("write", err)
		}
	}
	if staleIndex != "" {
		if err := os.WriteFile(path+"-shm", must(os.ReadFile(staleIndex))("read stale -shm"), 0o644); err != nil {
			fatal("write", err)
		}
	}
	db := must(recipe.Default().Open(ctx, path))("open copy with SQLite")
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("user_version", err)
	}
	result := must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate))("checkpoint copy")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	shm := "none"
	if staleIndex != "" {
		shm = "stale, from part A"
	}
	tables.sqliteRuns.Row(nextSeq(), part, "opened a copy, then TRUNCATE checkpoint", shm, version, result.Busy, result.LogFrames, result.Checkpointed)
	return path
}

func userVersion(ctx context.Context, path string) (int, string) {
	db := must(recipe.Default().Open(ctx, path))("open " + filepath.Base(path))
	var version int
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("user_version", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return version, integrity
}

func compareAndInspect(ctx context.Context, label, ours, theirs string, wantStep int) {
	result := must(equal.CompareFiles(ours, theirs, 4096))("compare")
	check(result.Equal, fmt.Sprintf("sha256 of our replay equals sha256 of %s", label))
	before := must(equal.FileSHA256(ours))("hash")
	version, integrity := userVersion(ctx, ours)
	after := must(equal.FileSHA256(ours))("hash")
	tables.comparisons.Row(nextSeq(), part, label, result.Level, result.Equal, result.SHA256A, version, integrity, before == after)
	check(version == wantStep && integrity == "ok" && before == after, fmt.Sprintf("the replay is a valid database at recipe step %d", wantStep))
}

func onlyUncommittedIgnored(report walfile.ReplayReport) bool {
	return len(report.Ignored) == 1 && report.Ignored[0].FirstFrame == report.LastCommitFrame+1 &&
		report.Ignored[0].LastFrame == report.FramesInFile && report.ValidFrames == report.FramesInFile && report.PartialFrameBytes == 0
}

func partA(ctx context.Context, dir string) {
	part = "A"
	r := recipe.Default()
	path := filepath.Join(mkdir(filepath.Join(dir, "a-quiet")), databaseName)
	if err := r.Build(ctx, path, 0); err != nil {
		fatal("build", err)
	}
	options := r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db := must(sqlitex.Open(ctx, path, options))("open")
	for k := 1; k <= quietSteps; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	copyDir := filepath.Join(dir, "a-quiet", "capture")
	liveCopy(ctx, path, copyDir, "quiet moment")
	shmDir := mkdir(filepath.Join(dir, "a-quiet", "shm-from-a"))
	shm := must(walfile.CopyLiveFiles(ctx, []walfile.CopyPair{{From: path + "-shm", To: filepath.Join(shmDir, databaseName+"-shm")}}))("copy -shm")
	if !shm[0].Present {
		fatal("copy -shm", fmt.Errorf("missing"))
	}
	hashes := must(r.StepHashes(ctx))("step hashes")
	baseMatch := recipe.MatchHashes(hashes, must(recipe.HashFile(filepath.Join(copyDir, databaseName)))("hash base"))
	tables.recipeMatches.Row(nextSeq(), part, "captured database file alone", baseMatch.Step, baseMatch.Exact)
	check(baseMatch.Found() && baseMatch.Exact && baseMatch.Step == 0, "the captured database file alone is byte for byte recipe step 0: every change committed since is only in the -wal")
	ours, report := replayCopy(copyDir, "quiet moment")
	check(report.IgnoredFrameCount() == 0 && report.Transactions == quietSteps, "at a quiet moment every frame is committed and nothing is ignored")
	result := must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate))("checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	tables.sqliteRuns.Row(nextSeq(), part, "TRUNCATE checkpoint of the original, then close", nil, nil, result.Busy, result.LogFrames, result.Checkpointed)
	compareAndInspect(ctx, "SQLite's own checkpoint of the original", ours, path, quietSteps)
	match := must(r.MatchStep(ctx, ours))("match replay")
	tables.recipeMatches.Row(nextSeq(), part, "our replay", match.Step, match.Exact)
	check(match.Found() && match.Step == quietSteps, fmt.Sprintf("the replay is the recipe's state after step %d (exact, or equal except the WAL change counter of one long-lived connection)", quietSteps))
}

func partBC(ctx context.Context, dir string) {
	part = "B"
	r := recipe.Default()
	root := mkdir(filepath.Join(dir, "b-mid-transaction"))
	path := filepath.Join(root, databaseName)
	if err := r.Build(ctx, path, spillBaseStep); err != nil {
		fatal("build", err)
	}
	// Set cache_size=10 so the open transaction runs out of page-cache slots
	// and spills dirty frames into -wal before COMMIT (src/wal.c:4140-4173).
	// Updating documents once dirties each page at most once, so no spilled
	// frame is overwritten in place and every spilled frame passes salt and
	// checksum validation—exercising stopping at LastCommitFrame on valid
	// uncommitted frames.
	options := r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	options.Conn.CacheSize = spillCacheSize
	db := must(sqlitex.Open(ctx, path, options))("open")
	for k := spillBaseStep + 1; k <= spillCommitted; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	committedOnly := must(walfile.ReadLiveState(ctx, path, root))("live state")
	tables.walStates.Row(nextSeq(), part, "after the commits", committedOnly.WALSize, len(committedOnly.WAL.Frames), committedOnly.WAL.ValidFrames, committedOnly.WAL.LastCommitFrame, committedOnly.Index.Header().MaxFrame)
	tx := must(db.BeginTx(ctx, nil))("begin")
	if _, err := tx.ExecContext(ctx, spillExtraUpdate); err != nil {
		fatal("update", err)
	}
	captureDir := filepath.Join(root, "capture-open-transaction")
	liveCopy(ctx, path, captureDir, "open transaction")
	captured := must(walfile.ParseFile(filepath.Join(captureDir, databaseName+"-wal")))("parse")
	spilled := len(captured.Frames) - captured.LastCommitFrame
	tables.walStates.Row(nextSeq(), part, "captured while the transaction is open", captured.Size, len(captured.Frames), captured.ValidFrames, captured.LastCommitFrame, nil)
	check(captured.LastCommitFrame == committedOnly.WAL.LastCommitFrame && spilled > 0, "the open transaction spilled frames into the -wal before committing, after the last commit frame")
	check(captured.ValidFrames == len(captured.Frames) && captured.ValidFrames > captured.LastCommitFrame,
		"every spilled frame passes the checksum and salt checks: the frames after the last commit frame are valid, only uncommitted")
	ours, report := replayCopy(captureDir, "open transaction")
	check(report.IgnoredFrameCount() == spilled && report.LastCommitFrame == committedOnly.WAL.LastCommitFrame &&
		report.AppliedFrames+report.SkippedBeyondSize == report.LastCommitFrame && onlyUncommittedIgnored(report),
		"the replayer stops at the last commit frame: it applies frames up to it and ignores every valid spilled frame after it because no commit frame follows")
	recovered := sqliteRecoveryOfCopy(ctx, captureDir, recoveryDirectory, "")
	compareAndInspect(ctx, "SQLite's recovery and checkpoint of the same captured files", ours, recovered, spillCommitted)
	// Pass a -shm taken from part A (different database, different salts,
	// mxFrame 155). The first connection opening a WAL database takes the
	// dead-man-switch lock, truncates -shm, and rebuilds it from -wal
	// (src/os_unix.c:4850-4915, unixLockSharedMemory; src/wal.c:1390-1614).
	staleIndex := filepath.Join(dir, "a-quiet", "shm-from-a", databaseName+"-shm")
	staleState := must(walfile.ParseIndexFile(staleIndex))("parse stale -shm")
	tables.staleShm.Row(staleState.Header().MaxFrame, staleState.Header().Pages)
	withStale := sqliteRecoveryOfCopy(ctx, captureDir, recoveryDirectory+"-stale-shm", staleIndex)
	compareAndInspect(ctx, "SQLite's checkpoint of the same files opened with a stale -shm", ours, withStale, spillCommitted)
	check(staleState.Header().MaxFrame == quietTotalFrames, "the -shm handed to SQLite described another WAL entirely, and SQLite rebuilt it from the -wal instead of trusting it")

	part = "C"
	if err := tx.Rollback(); err != nil {
		fatal("rollback", err)
	}
	afterRollback := must(walfile.ReadLiveState(ctx, path, root))("live state")
	tables.walStates.Row(nextSeq(), part, "after ROLLBACK", afterRollback.WALSize, len(afterRollback.WAL.Frames), afterRollback.WAL.ValidFrames, afterRollback.WAL.LastCommitFrame, afterRollback.Index.Header().MaxFrame)
	check(afterRollback.WALSize == captured.Size && int(afterRollback.Index.Header().MaxFrame) == captured.LastCommitFrame,
		"a rollback leaves the spilled frames in the -wal file; only the wal-index forgets them")
	rollbackDir := filepath.Join(root, "capture-after-rollback")
	liveCopy(ctx, path, rollbackDir, "after rollback")
	oursAfter, reportAfter := replayCopy(rollbackDir, "after rollback")
	check(reportAfter.IgnoredFrameCount() > 0 && reportAfter.LastCommitFrame == captured.LastCommitFrame && onlyUncommittedIgnored(reportAfter),
		"the replayer ignores the rolled-back frames because no commit frame follows them")
	result := must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate))("checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	tables.sqliteRuns.Row(nextSeq(), part, "TRUNCATE checkpoint of the original, then close", nil, nil, result.Busy, result.LogFrames, result.Checkpointed)
	compareAndInspect(ctx, "SQLite's own checkpoint of the original after the rollback", oursAfter, path, spillCommitted)
	same := must(equal.CompareFiles(ours, oursAfter, 4096))("compare")
	tables.comparisons.Row(nextSeq(), part, "replay of the open-transaction capture vs replay of the after-rollback capture", same.Level, same.Equal, same.SHA256A, nil, nil, nil)
	check(same.Equal, "both captures rebuild the same committed state")
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.2")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	partA(ctx, dir)
	partBC(ctx, dir)
	set.Finish("WAL REPLAY")
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

<!-- include: experiments/5.2-wal-replay/report.sql -->
```sql
-- Walkthrough of experiment 5.2. Run it with:
--   go run ./cmd/course run 5.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   captures        one row per live copy a child process took (database and -wal sizes)
--   recipe_matches  one row per file looked up among the recipe step hashes
--   replays         one row per replay of a captured -wal in plain Go (no SQLite)
--   sqlite_runs     one row per time SQLite opened or checkpointed files
--   comparisons     one row per byte comparison, with the replay opened by SQLite
--   wal_states      one row per look at the live -wal of part B and C
--   stale_shm       one row: the -shm of part A that part B hands to SQLite
--   checks          every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.2  Is a base database plus the committed WAL frames the whole state?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A plain Go replayer applies the frames of a WAL to a base database file up'
.print 'to the last commit frame, with no SQLite. Its output is compared byte for'
.print 'byte (sha256) with what SQLite itself produces from the same files.'
.print '(The tables show the first 16 hex digits of each sha256; the CSV keeps all.)'
.print 'Every capture is taken by a child process while the writer connection is'
.print 'still open, so the files are exactly what a crash would leave.'

.print
.print '── A. A quiet moment ──────────────────────────────────────────────────'
.print
.print 'Steps 1-8 are committed on one connection (wal_autocheckpoint=0); nothing'
.print 'is in flight.'
.print
.print 'A1. The copy: a small database file and a large -wal.'

SELECT capture, db_bytes, wal_bytes FROM captures WHERE part = 'A' ORDER BY seq;

.print
.print 'A2. The database file alone is byte for byte recipe step 0 (exact = true):'
.print '    every change committed since lives only in the -wal.'

SELECT subject, step, exact FROM recipe_matches WHERE part = 'A' AND subject = 'captured database file alone';

.print
.print 'A3. The replay: all frames are committed, nothing is ignored, and the output'
.print '    is the size of the database at the last commit.'

SELECT frames_in_file, valid_frames, transactions, last_commit_frame, database_pages,
       applied_frames, distinct_pages
FROM replays WHERE part = 'A' ORDER BY seq;

SELECT skipped_beyond_size, base_bytes, output_bytes, ignored_frames, partial_bytes
FROM replays WHERE part = 'A' ORDER BY seq;

.print
.print 'A4. SQLite on the original: a TRUNCATE checkpoint, then close. Then the'
.print '    replay against that file, and the replay opened by SQLite.'

SELECT run, busy, log_frames, checkpointed FROM sqlite_runs WHERE part = 'A' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'A' ORDER BY seq;

SELECT user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'A' ORDER BY seq;

.print
.print 'A5. The recipe lookup of the replay: step 8, equal except the change counter'
.print '    (bytes 24-27 and 92-95) that one long-lived connection leaves behind;'
.print '    exact = false means that masked match.'

SELECT subject, step, exact FROM recipe_matches WHERE part = 'A' AND subject = 'our replay';

.print
.print '── B. Mid-transaction ─────────────────────────────────────────────────'
.print
.print 'Steps 9-10 are committed. Then UPDATE documents SET title = upper(title)'
.print 'is left open with cache_size=10, so dirty pages spill into the -wal before'
.print 'any commit.'
.print
.print 'B1. The -wal after the commits, and as captured while the transaction is'
.print '    open. The frames after the last commit frame are spilled: valid'
.print '    (checksums and salts pass) but uncommitted.'

SELECT moment, wal_bytes, frames, valid_frames, last_commit_frame,
       frames - last_commit_frame AS spilled_frames
FROM wal_states WHERE part = 'B' ORDER BY seq;

SELECT capture, db_bytes, wal_bytes FROM captures WHERE part = 'B' ORDER BY seq;

.print
.print 'B2. The replay applies frames up to the last commit frame and ignores every'
.print '    valid spilled frame after it, because no commit frame follows.'

SELECT frames_in_file, valid_frames, transactions, last_commit_frame, database_pages,
       applied_frames, distinct_pages
FROM replays WHERE part = 'B' ORDER BY seq;

SELECT skipped_beyond_size, base_bytes, output_bytes, ignored_frames, partial_bytes
FROM replays WHERE part = 'B' ORDER BY seq;

SELECT capture, coalesce(ignored, 'none') AS ignored FROM replays WHERE part = 'B' ORDER BY seq;

.print
.print 'B3. SQLite opens a copy of the same captured files (no -shm), recovers,'
.print '    and checkpoints with TRUNCATE. Our replay equals the result byte for'
.print '    byte, and SQLite opens the replay as recipe step 10.'

SELECT run, shm, user_version, busy, log_frames, checkpointed
FROM sqlite_runs WHERE part = 'B' AND shm = 'none' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'B' AND against LIKE '%recovery%' ORDER BY seq;

SELECT user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'B' AND against LIKE '%recovery%' ORDER BY seq;

.print
.print 'B4. The same again, but the copy also gets a stale -shm taken from the part'
.print '    A database: another file, other salts, mxFrame 155. SQLite does not trust'
.print '    it; it rebuilds the wal-index from the -wal. The result is the same.'

SELECT s.mx_frame AS stale_mx_frame, s.n_page AS stale_n_page,
       (SELECT frames_in_file FROM replays WHERE part = 'A') AS part_a_frames
FROM stale_shm s;

SELECT run, shm, user_version, busy, log_frames, checkpointed
FROM sqlite_runs WHERE part = 'B' AND shm <> 'none' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'B' AND against LIKE '%stale%' ORDER BY seq;

SELECT user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'B' AND against LIKE '%stale%' ORDER BY seq;

.print
.print '── C. After a rollback ────────────────────────────────────────────────'
.print
.print 'The open transaction rolls back. Its frames stay in the -wal file; only the'
.print 'wal-index forgets them (mx_frame is back at the last commit frame).'
.print
.print 'C1. The -wal after ROLLBACK against the one captured in part B: the size is'
.print '    unchanged.'

SELECT moment, wal_bytes, frames, valid_frames, last_commit_frame, mx_frame
FROM wal_states WHERE part IN ('B', 'C') AND moment <> 'after the commits' ORDER BY seq;

.print
.print 'C2. A new capture after the rollback, and its replay: the same frames are'
.print '    ignored, because no commit frame follows them.'

SELECT capture, db_bytes, wal_bytes FROM captures WHERE part = 'C' ORDER BY seq;

SELECT frames_in_file, valid_frames, transactions, last_commit_frame, database_pages,
       applied_frames, distinct_pages
FROM replays WHERE part = 'C' ORDER BY seq;

SELECT skipped_beyond_size, base_bytes, output_bytes, ignored_frames, partial_bytes
FROM replays WHERE part = 'C' ORDER BY seq;

SELECT capture, coalesce(ignored, 'none') AS ignored FROM replays WHERE part = 'C' ORDER BY seq;

.print
.print 'C3. SQLite on the original: TRUNCATE checkpoint, then close. Our replay'
.print '    against it, and the two replays (open transaction, after rollback)'
.print '    against each other.'

SELECT run, busy, log_frames, checkpointed FROM sqlite_runs WHERE part = 'C' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'C' ORDER BY seq;

SELECT against, user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'C' AND user_version IS NOT NULL ORDER BY seq;
```

Captured evidence: the walkthrough rendered by the `lab` container. Its backing CSV tables live in [`evidence/5.2-wal-replay/`](evidence/5.2-wal-replay/), and `make check ARGS='evidence ch05'` compares a fresh run against them byte for byte.

<!-- include: evidence/5.2-wal-replay.txt -->
```text

════════════════════════════════════════════════════════════════════════
  5.2  Is a base database plus the committed WAL frames the whole state?
════════════════════════════════════════════════════════════════════════

A plain Go replayer applies the frames of a WAL to a base database file up
to the last commit frame, with no SQLite. Its output is compared byte for
byte (sha256) with what SQLite itself produces from the same files.
(The tables show the first 16 hex digits of each sha256; the CSV keeps all.)
Every capture is taken by a child process while the writer connection is
still open, so the files are exactly what a crash would leave.

── A. A quiet moment ──────────────────────────────────────────────────

Steps 1-8 are committed on one connection (wal_autocheckpoint=0); nothing
is in flight.

A1. The copy: a small database file and a large -wal.
┌──────────────┬──────────┬───────────┐
│   capture    │ db_bytes │ wal_bytes │
├──────────────┼──────────┼───────────┤
│ quiet moment │ 16384    │ 638632    │
└──────────────┴──────────┴───────────┘

A2. The database file alone is byte for byte recipe step 0 (exact = true):
    every change committed since lives only in the -wal.
┌──────────────────────────────┬──────┬───────┐
│           subject            │ step │ exact │
├──────────────────────────────┼──────┼───────┤
│ captured database file alone │ 0    │ true  │
└──────────────────────────────┴──────┴───────┘

A3. The replay: all frames are committed, nothing is ignored, and the output
    is the size of the database at the last commit.
┌────────────────┬──────────────┬──────────────┬───────────────────┬────────────────┬────────────────┬────────────────┐
│ frames_in_file │ valid_frames │ transactions │ last_commit_frame │ database_pages │ applied_frames │ distinct_pages │
├────────────────┼──────────────┼──────────────┼───────────────────┼────────────────┼────────────────┼────────────────┤
│ 155            │ 155          │ 8            │ 155               │ 79             │ 155            │ 79             │
└────────────────┴──────────────┴──────────────┴───────────────────┴────────────────┴────────────────┴────────────────┘
┌─────────────────────┬────────────┬──────────────┬────────────────┬───────────────┐
│ skipped_beyond_size │ base_bytes │ output_bytes │ ignored_frames │ partial_bytes │
├─────────────────────┼────────────┼──────────────┼────────────────┼───────────────┤
│ 0                   │ 16384      │ 323584       │ 0              │ 0             │
└─────────────────────┴────────────┴──────────────┴────────────────┴───────────────┘

A4. SQLite on the original: a TRUNCATE checkpoint, then close. Then the
    replay against that file, and the replay opened by SQLite.
┌─────────────────────────────────────────────────┬───────┬────────────┬──────────────┐
│                       run                       │ busy  │ log_frames │ checkpointed │
├─────────────────────────────────────────────────┼───────┼────────────┼──────────────┤
│ TRUNCATE checkpoint of the original, then close │ false │ 0          │ 0            │
└─────────────────────────────────────────────────┴───────┴────────────┴──────────────┘
┌─────────────────────────────────────────┬──────────┬───────┬──────────────────┐
│                 against                 │  level   │ equal │    sha256_16     │
├─────────────────────────────────────────┼──────────┼───────┼──────────────────┤
│ SQLite's own checkpoint of the original │ E1 bytes │ true  │ 00eac4cab1747d71 │
└─────────────────────────────────────────┴──────────┴───────┴──────────────────┘
┌──────────────┬─────────────────┬───────────────────┐
│ user_version │ integrity_check │ unchanged_by_open │
├──────────────┼─────────────────┼───────────────────┤
│ 8            │ ok              │ true              │
└──────────────┴─────────────────┴───────────────────┘

A5. The recipe lookup of the replay: step 8, equal except the change counter
    (bytes 24-27 and 92-95) that one long-lived connection leaves behind;
    exact = false means that masked match.
┌────────────┬──────┬───────┐
│  subject   │ step │ exact │
├────────────┼──────┼───────┤
│ our replay │ 8    │ false │
└────────────┴──────┴───────┘

── B. Mid-transaction ─────────────────────────────────────────────────

Steps 9-10 are committed. Then UPDATE documents SET title = upper(title)
is left open with cache_size=10, so dirty pages spill into the -wal before
any commit.

B1. The -wal after the commits, and as captured while the transaction is
    open. The frames after the last commit frame are spilled: valid
    (checksums and salts pass) but uncommitted.
┌────────────────────────────────────────┬───────────┬────────┬──────────────┬───────────────────┬────────────────┐
│                 moment                 │ wal_bytes │ frames │ valid_frames │ last_commit_frame │ spilled_frames │
├────────────────────────────────────────┼───────────┼────────┼──────────────┼───────────────────┼────────────────┤
│ after the commits                      │ 168952    │ 41     │ 41           │ 41                │ 0              │
│ captured while the transaction is open │ 247232    │ 60     │ 60           │ 41                │ 19             │
└────────────────────────────────────────┴───────────┴────────┴──────────────┴───────────────────┴────────────────┘
┌──────────────────┬──────────┬───────────┐
│     capture      │ db_bytes │ wal_bytes │
├──────────────────┼──────────┼───────────┤
│ open transaction │ 323584   │ 247232    │
└──────────────────┴──────────┴───────────┘

B2. The replay applies frames up to the last commit frame and ignores every
    valid spilled frame after it, because no commit frame follows.
┌────────────────┬──────────────┬──────────────┬───────────────────┬────────────────┬────────────────┬────────────────┐
│ frames_in_file │ valid_frames │ transactions │ last_commit_frame │ database_pages │ applied_frames │ distinct_pages │
├────────────────┼──────────────┼──────────────┼───────────────────┼────────────────┼────────────────┼────────────────┤
│ 60             │ 60           │ 2            │ 41                │ 100            │ 41             │ 34             │
└────────────────┴──────────────┴──────────────┴───────────────────┴────────────────┴────────────────┴────────────────┘
┌─────────────────────┬────────────┬──────────────┬────────────────┬───────────────┐
│ skipped_beyond_size │ base_bytes │ output_bytes │ ignored_frames │ partial_bytes │
├─────────────────────┼────────────┼──────────────┼────────────────┼───────────────┤
│ 0                   │ 323584     │ 409600       │ 19             │ 0             │
└─────────────────────┴────────────┴──────────────┴────────────────┴───────────────┘
┌──────────────────┬───────────────────────────────────────────────────────────────────┐
│     capture      │                              ignored                              │
├──────────────────┼───────────────────────────────────────────────────────────────────┤
│ open transaction │ frames 42-60 (19): valid but uncommitted: no commit frame follows │
└──────────────────┴───────────────────────────────────────────────────────────────────┘

B3. SQLite opens a copy of the same captured files (no -shm), recovers,
    and checkpoints with TRUNCATE. Our replay equals the result byte for
    byte, and SQLite opens the replay as recipe step 10.
┌─────────────────────────────────────────┬──────┬──────────────┬───────┬────────────┬──────────────┐
│                   run                   │ shm  │ user_version │ busy  │ log_frames │ checkpointed │
├─────────────────────────────────────────┼──────┼──────────────┼───────┼────────────┼──────────────┤
│ opened a copy, then TRUNCATE checkpoint │ none │ 10           │ false │ 0          │ 0            │
└─────────────────────────────────────────┴──────┴──────────────┴───────┴────────────┴──────────────┘
┌─────────────────────────────────────────────────────────────┬──────────┬───────┬──────────────────┐
│                           against                           │  level   │ equal │    sha256_16     │
├─────────────────────────────────────────────────────────────┼──────────┼───────┼──────────────────┤
│ SQLite's recovery and checkpoint of the same captured files │ E1 bytes │ true  │ fb68a52159415857 │
└─────────────────────────────────────────────────────────────┴──────────┴───────┴──────────────────┘
┌──────────────┬─────────────────┬───────────────────┐
│ user_version │ integrity_check │ unchanged_by_open │
├──────────────┼─────────────────┼───────────────────┤
│ 10           │ ok              │ true              │
└──────────────┴─────────────────┴───────────────────┘

B4. The same again, but the copy also gets a stale -shm taken from the part
    A database: another file, other salts, mxFrame 155. SQLite does not trust
    it; it rebuilds the wal-index from the -wal. The result is the same.
┌────────────────┬──────────────┬───────────────┐
│ stale_mx_frame │ stale_n_page │ part_a_frames │
├────────────────┼──────────────┼───────────────┤
│ 155            │ 79           │ 155           │
└────────────────┴──────────────┴───────────────┘
┌─────────────────────────────────────────┬────────────────────┬──────────────┬───────┬────────────┬──────────────┐
│                   run                   │        shm         │ user_version │ busy  │ log_frames │ checkpointed │
├─────────────────────────────────────────┼────────────────────┼──────────────┼───────┼────────────┼──────────────┤
│ opened a copy, then TRUNCATE checkpoint │ stale, from part A │ 10           │ false │ 0          │ 0            │
└─────────────────────────────────────────┴────────────────────┴──────────────┴───────┴────────────┴──────────────┘
┌────────────────────────────────────────────────────────────────┬──────────┬───────┬──────────────────┐
│                            against                             │  level   │ equal │    sha256_16     │
├────────────────────────────────────────────────────────────────┼──────────┼───────┼──────────────────┤
│ SQLite's checkpoint of the same files opened with a stale -shm │ E1 bytes │ true  │ fb68a52159415857 │
└────────────────────────────────────────────────────────────────┴──────────┴───────┴──────────────────┘
┌──────────────┬─────────────────┬───────────────────┐
│ user_version │ integrity_check │ unchanged_by_open │
├──────────────┼─────────────────┼───────────────────┤
│ 10           │ ok              │ true              │
└──────────────┴─────────────────┴───────────────────┘

── C. After a rollback ────────────────────────────────────────────────

The open transaction rolls back. Its frames stay in the -wal file; only the
wal-index forgets them (mx_frame is back at the last commit frame).

C1. The -wal after ROLLBACK against the one captured in part B: the size is
    unchanged.
┌────────────────────────────────────────┬───────────┬────────┬──────────────┬───────────────────┬──────────┐
│                 moment                 │ wal_bytes │ frames │ valid_frames │ last_commit_frame │ mx_frame │
├────────────────────────────────────────┼───────────┼────────┼──────────────┼───────────────────┼──────────┤
│ captured while the transaction is open │ 247232    │ 60     │ 60           │ 41                │          │
│ after ROLLBACK                         │ 247232    │ 60     │ 60           │ 41                │ 41       │
└────────────────────────────────────────┴───────────┴────────┴──────────────┴───────────────────┴──────────┘

C2. A new capture after the rollback, and its replay: the same frames are
    ignored, because no commit frame follows them.
┌────────────────┬──────────┬───────────┐
│    capture     │ db_bytes │ wal_bytes │
├────────────────┼──────────┼───────────┤
│ after rollback │ 323584   │ 247232    │
└────────────────┴──────────┴───────────┘
┌────────────────┬──────────────┬──────────────┬───────────────────┬────────────────┬────────────────┬────────────────┐
│ frames_in_file │ valid_frames │ transactions │ last_commit_frame │ database_pages │ applied_frames │ distinct_pages │
├────────────────┼──────────────┼──────────────┼───────────────────┼────────────────┼────────────────┼────────────────┤
│ 60             │ 60           │ 2            │ 41                │ 100            │ 41             │ 34             │
└────────────────┴──────────────┴──────────────┴───────────────────┴────────────────┴────────────────┴────────────────┘
┌─────────────────────┬────────────┬──────────────┬────────────────┬───────────────┐
│ skipped_beyond_size │ base_bytes │ output_bytes │ ignored_frames │ partial_bytes │
├─────────────────────┼────────────┼──────────────┼────────────────┼───────────────┤
│ 0                   │ 323584     │ 409600       │ 19             │ 0             │
└─────────────────────┴────────────┴──────────────┴────────────────┴───────────────┘
┌────────────────┬───────────────────────────────────────────────────────────────────┐
│    capture     │                              ignored                              │
├────────────────┼───────────────────────────────────────────────────────────────────┤
│ after rollback │ frames 42-60 (19): valid but uncommitted: no commit frame follows │
└────────────────┴───────────────────────────────────────────────────────────────────┘

C3. SQLite on the original: TRUNCATE checkpoint, then close. Our replay
    against it, and the two replays (open transaction, after rollback)
    against each other.
┌─────────────────────────────────────────────────┬───────┬────────────┬──────────────┐
│                       run                       │ busy  │ log_frames │ checkpointed │
├─────────────────────────────────────────────────┼───────┼────────────┼──────────────┤
│ TRUNCATE checkpoint of the original, then close │ false │ 0          │ 0            │
└─────────────────────────────────────────────────┴───────┴────────────┴──────────────┘
┌────────────────────────────────────────────────────────────────────────────────┬──────────┬───────┬──────────────────┐
│                                    against                                     │  level   │ equal │    sha256_16     │
├────────────────────────────────────────────────────────────────────────────────┼──────────┼───────┼──────────────────┤
│ SQLite's own checkpoint of the original after the rollback                     │ E1 bytes │ true  │ fb68a52159415857 │
│ replay of the open-transaction capture vs replay of the after-rollback capture │ E1 bytes │ true  │ fb68a52159415857 │
└────────────────────────────────────────────────────────────────────────────────┴──────────┴───────┴──────────────────┘
┌────────────────────────────────────────────────────────────┬──────────────┬─────────────────┬───────────────────┐
│                          against                           │ user_version │ integrity_check │ unchanged_by_open │
├────────────────────────────────────────────────────────────┼──────────────┼─────────────────┼───────────────────┤
│ SQLite's own checkpoint of the original after the rollback │ 10           │ ok              │ true              │
└────────────────────────────────────────────────────────────┴──────────────┴─────────────────┴───────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the captured database file alone is byte for byte recipe step 0: every change committed since is only in the -wal
A  ✓  at a quiet moment every frame is committed and nothing is ignored
A  ✓  sha256 of our replay equals sha256 of SQLite's own checkpoint of the original
A  ✓  the replay is a valid database at recipe step 8
A  ✓  the replay is the recipe's state after step 8 (exact, or equal except the WAL change counter of one long-lived connection)
B  ✓  the open transaction spilled frames into the -wal before committing, after the last commit frame
B  ✓  every spilled frame passes the checksum and salt checks: the frames after the last commit frame are valid, only uncommitted
B  ✓  the replayer stops at the last commit frame: it applies frames up to it and ignores every valid spilled frame after it because no commit frame follows
B  ✓  sha256 of our replay equals sha256 of SQLite's recovery and checkpoint of the same captured files
B  ✓  the replay is a valid database at recipe step 10
B  ✓  sha256 of our replay equals sha256 of SQLite's checkpoint of the same files opened with a stale -shm
B  ✓  the replay is a valid database at recipe step 10
B  ✓  the -shm handed to SQLite described another WAL entirely, and SQLite rebuilt it from the -wal instead of trusting it
C  ✓  a rollback leaves the spilled frames in the -wal file; only the wal-index forgets them
C  ✓  the replayer ignores the rolled-back frames because no commit frame follows them
C  ✓  sha256 of our replay equals sha256 of SQLite's own checkpoint of the original after the rollback
C  ✓  the replay is a valid database at recipe step 10
C  ✓  both captures rebuild the same committed state

18 of 18 claims hold

Experiment verdict: WAL REPLAY OK
```

**Reading the evidence:**

- **Part A — Quiet moment (A1–A5):** After 8 commits with `wal_autocheckpoint=0`, the child copies `r.db` (`16,384` bytes) and `r.db-wal` (`638,632` bytes). Hashing `r.db` alone matches recipe step `0` with exact bytes (A2): every change from those 8 committed transactions lives exclusively in `-wal`. `walfile.ReplayFiles` applies all `155` frames (`79` distinct pages, `8` transactions, `0` ignored), growing the output from `16,384` to `323,584` bytes (`79 * 4096`). SQLite then runs `wal_checkpoint(TRUNCATE)` on the original and closes; both files share SHA-256 `00eac4cab1747d717e2cebf666b7c63ed0504b8f83e8a2f15d02155130989af6`. Opening the replay with SQLite returns `user_version` `8` and `integrity_check` `ok`, leaving the file unchanged. Recipe lookup matches step 8 under the WAL change-counter mask (the one-connection header difference proved in Chapter 2).
- **Part B — Mid-transaction spill and disposable `-shm` (B1–B4):** Starting from canonical step 8 (`323,584` bytes) with `cache_size=10`, the writer commits steps 9–10 (`41` frames, `168,952` bytes) and runs `UPDATE documents SET title = upper(title)` without committing. Because that single update dirties each page at most once, `19` pages spill into `-wal` (`247,232` bytes, `60` frames total) with no in-place overwrites: all `60` frames pass salt and checksum checks (`60` valid frames, last commit frame `41`). `walfile.ReplayFiles` applies frames `1–41` (`34` distinct pages, output `409,600` bytes = `100` pages) and reports ignored `frames 42-60 (19): valid but uncommitted: no commit frame follows`. Two separate SQLite recoveries of the same base + `-wal` copy—first with **no `-shm`**, then with a **stale `-shm` from Part A** (`mxFrame` `155`, `nPage` `79`, different salts)—both recover `user_version` `10` and checkpoint to the exact same SHA-256 `fb68a521594158576391416edfaf14c310865c3eb929cb1e127d2a6f1bf215d7`.
- **Part C — After rollback (C1–C3):** Calling `tx.Rollback()` leaves `-wal` at `247,232` bytes (`60` frames, unchanged) while `-shm` keeps `mxFrame 41`. Replaying the post-rollback capture produces the same report (ignored `frames 42-60 (19): valid but uncommitted: no commit frame follows`) and the same SHA-256 `fb68a521…`, matching both SQLite's `TRUNCATE` checkpoint after rollback and Part B's replay byte for byte.

---

## 6. Evidence and failure cases

### 6.1 Experiment 5.4 — Checkpoint modes (`5.4-checkpoint-modes`)

**Why this experiment matters:** The R6 WAL shipper in Chapter 9 must own checkpoints and notice whenever anything else restarts the WAL. The salt change in the WAL header is how it notices.

- **Claim:** `PASSIVE`, `FULL`, `RESTART`, and `TRUNCATE` differ in which locks they wait for, what `(busy, log, checkpointed)` triple they return, and what they leave on disk:
  - `PASSIVE` never sets `busy=1` for readers and copies up to the oldest pinned read mark (`22` of `38` for a reader behind; `38` of `38` for a current reader or no reader).
  - `FULL` copies the same frames as `PASSIVE`, returning `busy=1` when any frame could not be copied (`reader behind`) and `busy=0` when all frames are copied (`no reader`, `reader current`).
  - `RESTART` does not restart the WAL itself; after copying all frames, it waits for read locks `1..4` so the next writer can restart, returning `busy=1` even for a `reader current` (whose snapshot is already at frame 38).
  - `TRUNCATE` behaves like `RESTART` and, when no reader blocks it, immediately restarts the wal-index (`shm salt-1 + 1`, `mxFrame = 0`), truncates `-wal` to `0` bytes, and returns `0/0/0`.
  - `PASSIVE`, `FULL`, and `RESTART` change neither the WAL size nor its salts; the WAL restarts at the **next write** whenever every frame was backfilled (or `TRUNCATE` emptied it). When the writer itself restarts the WAL (or writes after its own `TRUNCATE`), the new WAL header has `salt-1 + 1` and `checkpoint-seq + 1`; when another connection ran `TRUNCATE`, the writer's per-connection `pWal->nCkpt` is still `0`, so its next WAL header carries **fresh random salts** and `checkpoint-seq = 0`.
- **Evidence form:** `EXACT` (elapsed times go to a run-dependent `info` table printed by `course run`, excluded from stored evidence and not pass conditions).
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the DuckDB walkthrough; add `-i` to stay in the DuckDB shell afterwards with all tables loaded):
  ```text
  make run EXP=5.4
  ```

<!-- include: experiments/5.4-checkpoint-modes/main.go -->
```go
// Experiment 5.4 (checkpoint-modes) runs PASSIVE, FULL, RESTART, and TRUNCATE
// checkpoints against three reader states (no reader, reader behind, reader
// current), records (busy, log, checkpointed), WAL size, and salt changes
// right after the checkpoint and after the next write (elapsed times appear on
// INFO lines only), and contrasts TRUNCATE run by a third connection against
// TRUNCATE run by the writer itself (src/wal.c:2199-2395, 3875-3915,
// 4088-4098, 4301-4432).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 5.4
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	templateStep       = 5
	firstCommit        = 6
	secondCommit       = 7
	nextWrite          = 8
	checkpointerBusyMs = 300
)

type readerPosition int

const (
	noReader readerPosition = iota
	readerBehind
	readerCurrent
)

func (p readerPosition) String() string {
	switch p {
	case readerBehind:
		return "reader behind"
	case readerCurrent:
		return "reader current"
	}
	return "no reader"
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 5.4" runs both.
// part labels the claims: A is the mode and reader matrix, B the writer's own TRUNCATE.
var (
	set    *results.Set
	part   string
	tables struct {
		cases, nextWrites, ownTruncate, infoElapsed *results.Table
	}
)

func openTables() {
	tables.cases = set.Table("cases", "seq", "mode", "reader", "frames_before", "reader_frame", "busy", "log_frames", "checkpointed", "wal_bytes", "mx_frame", "n_backfill", "file_salt1", "shm_salt1")
	tables.nextWrites = set.Table("next_writes", "seq", "mode", "reader", "restarted", "file_salt1", "checkpoint_seq", "wal_bytes", "mx_frame", "valid_frames", "frames_in_file")
	tables.ownTruncate = set.Table("own_truncate", "seq", "moment", "busy", "log_frames", "checkpointed", "wal_bytes", "mx_frame", "shm_salt1", "file_salt1", "checkpoint_seq_before", "checkpoint_seq_after")
	tables.infoElapsed = set.Table("info_elapsed", "seq", "mode", "reader", "elapsed_ms")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
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

type sample struct {
	walSize       int64
	walFrames     int
	walValid      int
	walSalt1      uint32
	walSalt2      uint32
	walSequence   uint32
	indexMaxFrame uint32
	indexBackfill uint32
	indexSalt1    uint32
}

func take(ctx context.Context, path, scratch string) sample {
	state := must(walfile.ReadLiveState(ctx, path, scratch))("live state")
	if !state.WALPresent || !state.IndexPresent {
		fatal("live state", fmt.Errorf("WAL present %v, wal-index present %v", state.WALPresent, state.IndexPresent))
	}
	s := sample{walSize: state.WALSize}
	if state.WAL != nil {
		s.walFrames = len(state.WAL.Frames)
		s.walValid = state.WAL.ValidFrames
		s.walSalt1 = state.WAL.Header.Salt1
		s.walSalt2 = state.WAL.Header.Salt2
		s.walSequence = state.WAL.Header.CheckpointSequence
	}
	h := state.Index.Header()
	s.indexMaxFrame = h.MaxFrame
	s.indexBackfill = state.Index.Checkpoint.Backfill
	s.indexSalt1 = h.Salt1
	return s
}

type outcome struct {
	mode        string
	reader      readerPosition
	before      sample
	result      sqlitex.CheckpointResult
	elapsed     time.Duration
	after       sample
	next        sample
	readerFrame uint32
}

func open(ctx context.Context, path string, immediate bool, busy time.Duration) *sqlitex.DB {
	options := recipe.Default().SQLiteOptions()
	options.ImmediateTx = immediate
	options.Conn.WALAutocheckpoint = 0
	options.Conn.BusyTimeout = busy
	return must(sqlitex.Open(ctx, path, options))("open")
}

func beginReader(ctx context.Context, path string) (*sqlitex.DB, *sql.Tx) {
	db := open(ctx, path, false, 5*time.Second)
	tx := must(db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}))("begin read")
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		fatal("read", err)
	}
	return db, tx
}

// runCase executes one (mode, readerPosition) cell on a fresh copy of the
// step-5 template: writer commits steps 6 and 7, a third connection with a
// 300 ms busy_timeout runs the checkpoint, then the reader closes and writer
// commits step 8 to reveal whether the WAL restarted and how its salts changed.
func runCase(ctx context.Context, template, dir, mode string, reader readerPosition) outcome {
	name := strings.ToLower(mode) + "-" + strings.ReplaceAll(reader.String(), " ", "-")
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := os.WriteFile(path, must(os.ReadFile(template))("read template"), 0o644); err != nil {
		fatal("copy template", err)
	}
	r := recipe.Default()
	writer := open(ctx, path, true, 5*time.Second)
	var readerDB *sqlitex.DB
	var readerTx *sql.Tx
	o := outcome{mode: mode, reader: reader}
	if err := r.ApplyStep(ctx, writer, firstCommit); err != nil {
		fatal("step", err)
	}
	if reader == readerBehind {
		readerDB, readerTx = beginReader(ctx, path)
		o.readerFrame = take(ctx, path, root).indexMaxFrame
	}
	if err := r.ApplyStep(ctx, writer, secondCommit); err != nil {
		fatal("step", err)
	}
	if reader == readerCurrent {
		readerDB, readerTx = beginReader(ctx, path)
	}
	o.before = take(ctx, path, root)
	checkpointer := open(ctx, path, true, checkpointerBusyMs*time.Millisecond)
	start := time.Now()
	o.result = must(sqlitex.Checkpoint(ctx, checkpointer, "", mode))("checkpoint " + mode)
	o.elapsed = time.Since(start)
	o.after = take(ctx, path, root)
	if readerTx != nil {
		if err := readerTx.Rollback(); err != nil {
			fatal("end read", err)
		}
		if err := readerDB.Close(); err != nil {
			fatal("close reader", err)
		}
	}
	if err := checkpointer.Close(); err != nil {
		fatal("close checkpointer", err)
	}
	if err := r.ApplyStep(ctx, writer, nextWrite); err != nil {
		fatal("step", err)
	}
	o.next = take(ctx, path, root)
	var integrity string
	if err := writer.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		fatal("integrity_check", fmt.Errorf("%s %v", integrity, err))
	}
	if err := writer.Close(); err != nil {
		fatal("close writer", err)
	}
	return o
}

func delta(before, after uint32) string {
	switch after {
	case before:
		return "same"
	case before + 1:
		return "+1"
	}
	return "new value"
}

func saltDelta(before, after sample) string {
	if after.walSize < walfile.HeaderSize {
		return "(no header)"
	}
	return delta(before.walSalt1, after.walSalt1)
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.4")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	template := filepath.Join(dir, "template", "r.db")
	if err := os.MkdirAll(filepath.Dir(template), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.Default().Build(ctx, template, templateStep); err != nil {
		fatal("build", err)
	}
	part = "A"
	set = results.MustOpen(dir)
	openTables()
	modes := []string{sqlitex.CheckpointPassive, sqlitex.CheckpointFull, sqlitex.CheckpointRestart, sqlitex.CheckpointTruncate}
	readers := []readerPosition{noReader, readerBehind, readerCurrent}
	var outcomes []outcome
	for _, mode := range modes {
		for _, reader := range readers {
			outcomes = append(outcomes, runCase(ctx, template, dir, mode, reader))
		}
	}
	for i, o := range outcomes {
		var readerFrame any
		if o.reader == readerBehind {
			readerFrame = o.readerFrame
		}
		tables.cases.Row(i+1, o.mode, o.reader, o.before.indexMaxFrame, readerFrame, o.result.Busy, o.result.LogFrames, o.result.Checkpointed,
			o.after.walSize, o.after.indexMaxFrame, o.after.indexBackfill, saltDelta(o.before, o.after), delta(o.before.indexSalt1, o.after.indexSalt1))
		tables.nextWrites.Row(i+1, o.mode, o.reader, o.next.indexMaxFrame < o.before.indexMaxFrame, saltDelta(o.before, o.next),
			delta(o.before.walSequence, o.next.walSequence), o.next.walSize, o.next.indexMaxFrame, o.next.walValid, o.next.walFrames)
		tables.infoElapsed.Row(i+1, o.mode, o.reader, o.elapsed.Milliseconds())
	}
	n := outcomes[0].before.indexMaxFrame
	behind := outcomes[1].readerFrame
	byKey := map[string]outcome{}
	for _, o := range outcomes {
		byKey[o.mode+"/"+o.reader.String()] = o
	}
	res := func(mode string, reader readerPosition) sqlitex.CheckpointResult {
		return byKey[mode+"/"+reader.String()].result
	}
	full := sqlitex.CheckpointResult{LogFrames: int(n), Checkpointed: int(n)}
	partial := sqlitex.CheckpointResult{LogFrames: int(n), Checkpointed: int(behind)}
	partialBusy := partial
	partialBusy.Busy = true
	fullBusy := full
	fullBusy.Busy = true
	check(res("PASSIVE", noReader) == full && res("FULL", noReader) == full && res("RESTART", noReader) == full,
		"with no reader, PASSIVE, FULL and RESTART all copy every frame and report busy=0")
	check(res("TRUNCATE", noReader) == sqlitex.CheckpointResult{} && byKey["TRUNCATE/no reader"].after.walSize == 0,
		"TRUNCATE with no reader reports 0/0/0 and leaves a zero-length -wal")
	check(res("PASSIVE", readerBehind) == partial, "PASSIVE with a reader behind copies up to the reader's frame and still reports busy=0")
	check(res("FULL", readerBehind) == partialBusy && res("RESTART", readerBehind) == partialBusy && res("TRUNCATE", readerBehind) == partialBusy,
		"FULL, RESTART and TRUNCATE with a reader behind copy the same frames, then report busy=1")
	check(res("PASSIVE", readerCurrent) == full && res("FULL", readerCurrent) == full,
		"a reader whose snapshot is the end of the WAL does not limit PASSIVE or FULL")
	check(res("RESTART", readerCurrent) == fullBusy && res("TRUNCATE", readerCurrent) == fullBusy,
		"RESTART and TRUNCATE copy everything but report busy=1 while any reader is still using the WAL")
	truncated := byKey["TRUNCATE/no reader"]
	check(truncated.after.indexSalt1 == truncated.before.indexSalt1+1 && truncated.after.indexMaxFrame == 0,
		"TRUNCATE restarts the wal-index at once: shm salt-1 + 1, mxFrame 0")
	sameHeader := true
	for _, mode := range []string{"PASSIVE", "FULL", "RESTART"} {
		o := byKey[mode+"/no reader"]
		sameHeader = sameHeader && o.after.walSalt1 == o.before.walSalt1 && o.after.indexSalt1 == o.before.indexSalt1 && o.after.walSize == o.before.walSize
	}
	check(sameHeader, "PASSIVE, FULL and RESTART change neither salt nor the WAL size: the restart happens at the next write")
	restartedOK := true
	for _, o := range outcomes {
		restarted := o.next.indexMaxFrame < o.before.indexMaxFrame
		wantRestart := o.result.Checkpointed == int(n) || (o.mode == "TRUNCATE" && !o.result.Busy)
		restartedOK = restartedOK && restarted == wantRestart
		if !restarted {
			restartedOK = restartedOK && o.next.walSalt1 == o.before.walSalt1 && o.next.walSequence == o.before.walSequence
		}
	}
	check(restartedOK, "the next write restarts the WAL exactly when the checkpoint had copied every frame (or TRUNCATE emptied it); otherwise it appends with the same salts")
	writerRestartOK := true
	for _, o := range outcomes {
		if o.next.indexMaxFrame < o.before.indexMaxFrame && o.mode != "TRUNCATE" || o.mode == "TRUNCATE" && o.reader == readerCurrent {
			writerRestartOK = writerRestartOK && o.next.walSalt1 == o.before.walSalt1+1 && o.next.walSequence == o.before.walSequence+1
		}
	}
	check(writerRestartOK, "when the writer itself restarts the WAL at its next write, the new header has salt-1 + 1 and checkpoint sequence + 1")
	// Check whether the writer's next commit restarted the WAL: walFrames calls
	// walRestartLog (src/wal.c:3879-3896), which resets mxFrame to 0 whenever
	// nBackfill == mxFrame and no reader holds locks 1..4. When another
	// connection already ran TRUNCATE, the writer's own pWal->nCkpt is still 0,
	// so walFrames generates fresh random salts instead of salt-1 + 1
	// (src/wal.c:4095-4096).
	check(truncated.next.walSalt1 != truncated.before.walSalt1 && truncated.next.walSalt1 != truncated.before.walSalt1+1 && truncated.next.walSequence == truncated.before.walSequence,
		"after another connection's TRUNCATE, the writer's next WAL header has new random salts and the writer's own checkpoint sequence (0): the sequence is per connection")
	ownTruncate(ctx, template, dir)
	set.Finish("CHECKPOINT MODES")
}

// ownTruncate repeats TRUNCATE/no-reader on the writer connection itself: its
// walRestartHdr call increments pWal->nCkpt to 1 (src/wal.c:2156), so its next
// commit preserves salt-1 + 1 and writes checkpoint sequence 1 instead of
// re-randomizing salts.
func ownTruncate(ctx context.Context, template, dir string) {
	part = "B"
	root := filepath.Join(dir, "truncate-by-writer")
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := os.WriteFile(path, must(os.ReadFile(template))("read template"), 0o644); err != nil {
		fatal("copy template", err)
	}
	r := recipe.Default()
	writer := open(ctx, path, true, 5*time.Second)
	for _, k := range []int{firstCommit, secondCommit} {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
	}
	before := take(ctx, path, root)
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointTruncate))("checkpoint")
	after := take(ctx, path, root)
	if err := r.ApplyStep(ctx, writer, nextWrite); err != nil {
		fatal("step", err)
	}
	next := take(ctx, path, root)
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
	tables.ownTruncate.Row(1, "right after wal_checkpoint(TRUNCATE)", result.Busy, result.LogFrames, result.Checkpointed, after.walSize, after.indexMaxFrame,
		delta(before.indexSalt1, after.indexSalt1), nil, nil, nil)
	tables.ownTruncate.Row(2, fmt.Sprintf("after the next write (step %d)", nextWrite), nil, nil, nil, next.walSize, next.indexMaxFrame,
		nil, saltDelta(before, next), before.walSequence, next.walSequence)
	check(next.walSalt1 == before.walSalt1+1 && next.walSequence == before.walSequence+1,
		"when the connection that ran TRUNCATE writes next, the new WAL header has salt-1 + 1 and checkpoint sequence + 1")
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

<!-- include: experiments/5.4-checkpoint-modes/report.sql -->
```sql
-- Walkthrough of experiment 5.4. Run it with:
--   go run ./cmd/course run 5.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   cases         one row per (mode, reader) case: the checkpoint result and the state right after it
--   next_writes   the same cases after the reader ended and the writer committed step 8
--   own_truncate  two rows: the writer runs TRUNCATE itself, then commits step 8
--   checks        every claim the experiment asserted (the runner lists it last)
-- file_salt1, shm_salt1 and checkpoint_seq are changes against the state before
-- the checkpoint: same, +1, new value, or (no header) for an empty -wal.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.4  What do PASSIVE, FULL, RESTART and TRUNCATE do with readers around?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Each of the 12 cases starts from a fresh copy of recipe step 5 (a closed'
.print 'file). A writer commits step 6 and step 7 with wal_autocheckpoint=0.'
.print '"reader behind" begins its read transaction between the two commits,'
.print '"reader current" after both. A third connection (busy_timeout=300ms) runs'
.print 'the checkpoint; then the reader ends and the writer commits step 8.'
.print 'The WAL file header is read from a child process copy, the wal-index from'
.print 'the -shm.'

.print
.print '── A. Four modes against three reader states ──────────────────────────'
.print
.print 'A1. Frames before every checkpoint, and the snapshot of the reader behind:'

SELECT min(frames_before)         AS frames_before_every_checkpoint,
       count(DISTINCT frames_before) AS distinct_values,
       min(reader_frame)             AS reader_behind_snapshot_frame
FROM cases;

.print
.print 'A2. The result (busy, log, checkpointed) and the state right after it.'
.print '    Without a reader every mode copies all 38 frames; a reader behind caps'
.print '    the copy at its frame (22); busy=1 reports a checkpoint that could not'
.print '    finish its mode. TRUNCATE with no reader empties the -wal and restarts'
.print '    the wal-index at once (shm salt-1 +1, mxFrame 0); the other modes leave'
.print '    salts and size alone, so the restart waits for the next write.'

SELECT mode, reader, busy, log_frames AS log, checkpointed, wal_bytes, mx_frame, n_backfill, file_salt1, shm_salt1
FROM cases ORDER BY seq;

.print
.print 'A3. After the reader ended and the writer committed step 8. The WAL'
.print '    restarted exactly when the checkpoint had copied every frame (or TRUNCATE'
.print '    emptied it): salt-1 and the checkpoint sequence advance by 1 and the file'
.print '    is reused. Otherwise the writer appends with the same salts (valid frames'
.print '    = frames in file). After the TRUNCATE of another connection the writer starts'
.print '    with new random salts and its own checkpoint sequence, which is per'
.print '    connection (row TRUNCATE / no reader).'

SELECT mode, reader, restarted, file_salt1, checkpoint_seq, wal_bytes, mx_frame,
       valid_frames || ' / ' || frames_in_file AS valid_frames_of_file
FROM next_writes ORDER BY seq;

.print
.print '── B. The writer runs TRUNCATE itself ─────────────────────────────────'
.print
.print 'B1. The same TRUNCATE with no reader, but on the writer connection. Its'
.print '    own checkpoint bumps its counter, so its next commit keeps salt-1 +1'
.print '    and writes checkpoint sequence 1 instead of new random salts.'

SELECT moment, busy, log_frames AS log, checkpointed, wal_bytes, mx_frame, shm_salt1, file_salt1,
       checkpoint_seq_before || ' -> ' || checkpoint_seq_after AS checkpoint_seq
FROM own_truncate ORDER BY seq;
```

`course run` also prints the run-dependent values excluded from the stored evidence: how long each checkpoint took (the ones near 300 ms waited out the checkpointer's `busy_timeout`).

Captured evidence: the walkthrough rendered by the `lab` container. Its backing CSV tables live in [`evidence/5.4-checkpoint-modes/`](evidence/5.4-checkpoint-modes/), and `make check ARGS='evidence ch05'` compares a fresh run against them byte for byte.

<!-- include: evidence/5.4-checkpoint-modes.txt -->
```text

════════════════════════════════════════════════════════════════════════
  5.4  What do PASSIVE, FULL, RESTART and TRUNCATE do with readers around?
════════════════════════════════════════════════════════════════════════

Each of the 12 cases starts from a fresh copy of recipe step 5 (a closed
file). A writer commits step 6 and step 7 with wal_autocheckpoint=0.
"reader behind" begins its read transaction between the two commits,
"reader current" after both. A third connection (busy_timeout=300ms) runs
the checkpoint; then the reader ends and the writer commits step 8.
The WAL file header is read from a child process copy, the wal-index from
the -shm.

── A. Four modes against three reader states ──────────────────────────

A1. Frames before every checkpoint, and the snapshot of the reader behind:
┌────────────────────────────────┬─────────────────┬──────────────────────────────┐
│ frames_before_every_checkpoint │ distinct_values │ reader_behind_snapshot_frame │
├────────────────────────────────┼─────────────────┼──────────────────────────────┤
│ 38                             │ 1               │ 22                           │
└────────────────────────────────┴─────────────────┴──────────────────────────────┘

A2. The result (busy, log, checkpointed) and the state right after it.
    Without a reader every mode copies all 38 frames; a reader behind caps
    the copy at its frame (22); busy=1 reports a checkpoint that could not
    finish its mode. TRUNCATE with no reader empties the -wal and restarts
    the wal-index at once (shm salt-1 +1, mxFrame 0); the other modes leave
    salts and size alone, so the restart waits for the next write.
┌──────────┬────────────────┬───────┬─────┬──────────────┬───────────┬──────────┬────────────┬─────────────┬───────────┐
│   mode   │     reader     │ busy  │ log │ checkpointed │ wal_bytes │ mx_frame │ n_backfill │ file_salt1  │ shm_salt1 │
├──────────┼────────────────┼───────┼─────┼──────────────┼───────────┼──────────┼────────────┼─────────────┼───────────┤
│ PASSIVE  │ no reader      │ false │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
│ PASSIVE  │ reader behind  │ false │ 38  │ 22           │ 156592    │ 38       │ 22         │ same        │ same      │
│ PASSIVE  │ reader current │ false │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
│ FULL     │ no reader      │ false │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
│ FULL     │ reader behind  │ true  │ 38  │ 22           │ 156592    │ 38       │ 22         │ same        │ same      │
│ FULL     │ reader current │ false │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
│ RESTART  │ no reader      │ false │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
│ RESTART  │ reader behind  │ true  │ 38  │ 22           │ 156592    │ 38       │ 22         │ same        │ same      │
│ RESTART  │ reader current │ true  │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
│ TRUNCATE │ no reader      │ false │ 0   │ 0            │ 0         │ 0        │ 0          │ (no header) │ +1        │
│ TRUNCATE │ reader behind  │ true  │ 38  │ 22           │ 156592    │ 38       │ 22         │ same        │ same      │
│ TRUNCATE │ reader current │ true  │ 38  │ 38           │ 156592    │ 38       │ 38         │ same        │ same      │
└──────────┴────────────────┴───────┴─────┴──────────────┴───────────┴──────────┴────────────┴─────────────┴───────────┘

A3. After the reader ended and the writer committed step 8. The WAL
    restarted exactly when the checkpoint had copied every frame (or TRUNCATE
    emptied it): salt-1 and the checkpoint sequence advance by 1 and the file
    is reused. Otherwise the writer appends with the same salts (valid frames
    = frames in file). After the TRUNCATE of another connection the writer starts
    with new random salts and its own checkpoint sequence, which is per
    connection (row TRUNCATE / no reader).
┌──────────┬────────────────┬───────────┬────────────┬────────────────┬───────────┬──────────┬──────────────────────┐
│   mode   │     reader     │ restarted │ file_salt1 │ checkpoint_seq │ wal_bytes │ mx_frame │ valid_frames_of_file │
├──────────┼────────────────┼───────────┼────────────┼────────────────┼───────────┼──────────┼──────────────────────┤
│ PASSIVE  │ no reader      │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
│ PASSIVE  │ reader behind  │ false     │ same       │ same           │ 251352    │ 61       │ 61 / 61              │
│ PASSIVE  │ reader current │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
│ FULL     │ no reader      │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
│ FULL     │ reader behind  │ false     │ same       │ same           │ 251352    │ 61       │ 61 / 61              │
│ FULL     │ reader current │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
│ RESTART  │ no reader      │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
│ RESTART  │ reader behind  │ false     │ same       │ same           │ 251352    │ 61       │ 61 / 61              │
│ RESTART  │ reader current │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
│ TRUNCATE │ no reader      │ true      │ new value  │ same           │ 94792     │ 23       │ 23 / 23              │
│ TRUNCATE │ reader behind  │ false     │ same       │ same           │ 251352    │ 61       │ 61 / 61              │
│ TRUNCATE │ reader current │ true      │ +1         │ +1             │ 156592    │ 23       │ 23 / 38              │
└──────────┴────────────────┴───────────┴────────────┴────────────────┴───────────┴──────────┴──────────────────────┘

── B. The writer runs TRUNCATE itself ─────────────────────────────────

B1. The same TRUNCATE with no reader, but on the writer connection. Its
    own checkpoint bumps its counter, so its next commit keeps salt-1 +1
    and writes checkpoint sequence 1 instead of new random salts.
┌──────────────────────────────────────┬───────┬─────┬──────────────┬───────────┬──────────┬───────────┬────────────┬────────────────┐
│                moment                │ busy  │ log │ checkpointed │ wal_bytes │ mx_frame │ shm_salt1 │ file_salt1 │ checkpoint_seq │
├──────────────────────────────────────┼───────┼─────┼──────────────┼───────────┼──────────┼───────────┼────────────┼────────────────┤
│ right after wal_checkpoint(TRUNCATE) │ false │ 0   │ 0            │ 0         │ 0        │ +1        │            │                │
│ after the next write (step 8)        │       │     │              │ 94792     │ 23       │           │ +1         │ 0 -> 1         │
└──────────────────────────────────────┴───────┴─────┴──────────────┴───────────┴──────────┴───────────┴────────────┴────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  with no reader, PASSIVE, FULL and RESTART all copy every frame and report busy=0
A  ✓  TRUNCATE with no reader reports 0/0/0 and leaves a zero-length -wal
A  ✓  PASSIVE with a reader behind copies up to the reader's frame and still reports busy=0
A  ✓  FULL, RESTART and TRUNCATE with a reader behind copy the same frames, then report busy=1
A  ✓  a reader whose snapshot is the end of the WAL does not limit PASSIVE or FULL
A  ✓  RESTART and TRUNCATE copy everything but report busy=1 while any reader is still using the WAL
A  ✓  TRUNCATE restarts the wal-index at once: shm salt-1 + 1, mxFrame 0
A  ✓  PASSIVE, FULL and RESTART change neither salt nor the WAL size: the restart happens at the next write
A  ✓  the next write restarts the WAL exactly when the checkpoint had copied every frame (or TRUNCATE emptied it); otherwise it appends with the same salts
A  ✓  when the writer itself restarts the WAL at its next write, the new header has salt-1 + 1 and checkpoint sequence + 1
A  ✓  after another connection's TRUNCATE, the writer's next WAL header has new random salts and the writer's own checkpoint sequence (0): the sequence is per connection
B  ✓  when the connection that ran TRUNCATE writes next, the new WAL header has salt-1 + 1 and checkpoint sequence + 1

12 of 12 claims hold

Experiment verdict: CHECKPOINT MODES OK
```

**Reading the evidence:**

- **First table — right after the checkpoint (A1–A2):** Before every case, the WAL holds `38` frames (`156,592` bytes); `"reader behind"` holds frame `22` (between steps 6 and 7), and `"reader current"` holds frame `38`.
  - With `no reader`, `PASSIVE`, `FULL`, and `RESTART` all return busy `false`, log `38`, checkpointed `38`, leave `WAL bytes = 156592`, and leave both `file salt-1` and `shm salt-1` `same`. Only `TRUNCATE` returns busy `false`, log `0`, checkpointed `0`, shrinks `-wal` to `0` bytes (`(no header)`), and advances `shm salt-1` by `+1`.
  - With `reader behind`, all four modes copy the same `22` frames (`nBackfill = 22`). `PASSIVE` returns busy `false`; `FULL`, `RESTART`, and `TRUNCATE` wait out the `300ms` busy timeout (run-dependent elapsed times printed by `course run`, excluded from stored evidence: about `340ms`, `346ms`, `338ms` in one lab run) and return busy `true`.
  - With `reader current`, `PASSIVE` and `FULL` copy all `38` frames and return busy `false` because copying overwrites no page the reader needs. `RESTART` and `TRUNCATE` copy all `38` frames too, then wait (run-dependent elapsed times: about `336ms` and `345ms` in one lab run) for the reader to leave `WAL_READ_LOCK(1)` before returning busy `true`.
- **Second table — after the reader ends and the writer commits step 8 (A3):**
  - Every `reader behind` case (`nBackfill = 22 < 38`) does **not** restart: step 8 appends `23` frames to frame `38`, growing `-wal` to `251,352` bytes (`61 / 61` valid frames) with `file salt-1 = same` and `checkpoint-seq = same`.
  - Every `no reader` and `reader current` case for `PASSIVE`, `FULL`, and `RESTART`—plus `TRUNCATE / reader current`, where `TRUNCATE` had copied all `38` frames before returning busy `true`—restarts at frame 1 when the writer commits step 8: `WAL bytes = 156592` (file reused, not shrunk), `mxFrame = 23`, `23 / 38` valid frames (frames 24–38 are old-generation frames), `file salt-1 = +1`, and `checkpoint-seq = +1`.
  - **`TRUNCATE / no reader` (that row of A3) exposes the per-connection sequence rule:** checkpointer connection C had already called `walRestartHdr` and truncated `-wal` to `0` bytes. When writer W—whose own `pWal->nCkpt` is still `0`—writes the new WAL header, `-wal` grows to `94,792` bytes (`32 + 23 * 4120`), `file salt-1` is a **`new value`** (fresh random bytes, not `before + 1`), and `checkpoint-seq` stays `same` (`0`).
- **Same-connection `TRUNCATE` contrast (B1):** When the writer itself runs `TRUNCATE` (advancing its own `pWal->nCkpt` from `0` to `1`) and then commits step 8, the new WAL header has `file salt-1 = +1` and `checkpoint-seq = +1 (0 -> 1)`. Because a platform cannot assume which connection ran `TRUNCATE`, WAL generation detection must treat **any change in `(salt-1, salt-2)`** as a new generation, never `salt-1 + 1` or `checkpoint-seq`.

### 6.2 Failure cases and replay boundaries

The experiments, unit tests (`make test PACKAGE=./internal/walfile`, including 3,000 mutated inputs, 200 random inputs, and `FuzzParseAndReplay` in `internal/walfile/walfile_test.go`), and standalone stress tests establish six concrete failure boundaries (parenthetical codes such as `REP-01`, `REP-02`, `REP-03`, `CHK-04`, and `CHK-05` name stress tests listed under "What attacked this chapter" below):

1. **Applying frames past `LastCommitFrame` corrupts the database.** Rolled-back and in-flight spilled frames stay in `-wal` (Experiment 5.2 B/C) and can still carry valid checksums. Applying all frames instead of stopping at `LastCommitFrame` leaks rolled-back bytes and fails `PRAGMA integrity_check` (`REP-03` below).
2. **A stale-generation base cannot be caught from raw `base + WAL` bytes alone.** Neither `.db` nor `-wal` records which WAL generation `.db` has absorbed. Replaying a generation-2 WAL onto a generation-0 base succeeds with `err == nil` and produces a corrupt database (`REP-02` below). Safe replay requires capture metadata pairing base SHA-256 and WAL salts, built in Chapter 9 (`internal/walship`).
3. **Damaged WAL frames truncate recovery cleanly at the last valid commit.** Flipping any bit in a WAL frame breaks the running checksum chain at that frame, causing both `walfile.Replay` and SQLite recovery to stop at the previous commit frame and produce identical bytes (`REP-01` below).
4. **Torn `-shm` headers and `unused` padding bytes.** Because `walIndexWriteHdr` writes copy 2 before the memory barrier and copy 1 after (`src/wal.c:951-953`), a zeroed copy 1 beside a populated copy 2 is a torn write (`Initialized=false`, `HeadersAgree()=false`, `FramePage` returns `ErrFrameNotIndexed`), and `HeadersAgree()` compares all 48 raw bytes including the 4-byte `unused` field (`src/wal.c:323`).
5. **Zero-commit WALs still validate the base file, and foreign `.replay-tmp` files are never overwritten or deleted.** `walfile.Replay` checks that a non-empty base has a valid power-of-two page size in `512..65536`, matches the WAL page size, and is a whole multiple of that page size even when `LastCommitFrame == 0`. `ReplayFiles` opens `.replay-tmp` with `O_EXCL` and leaves any pre-existing file untouched on `os.ErrExist`.
6. **Concurrent checkpointers fail immediately; read-only connections cannot backfill.** A second checkpointer hitting `WAL_CKPT_LOCK` ignores `busy_timeout` and immediately returns `busy=1, log=-1, checkpointed=-1` (`src/wal.c:4342`, `CHK-04`). A `mode=ro` connection calling `PRAGMA wal_checkpoint(PASSIVE)` on an un-backfilled WAL fails with `disk I/O error (778)` and leaves `nBackfill = 0`; it returns `busy=0` only when zero frames remain to write (`CHK-05`).

### What attacked this chapter

<!-- adversarial-results -->

Four standalone Go stress-test programs in this repository (`28` attacks total, driven by [`run-all.sh`](../agent_work/ch05/adversarial/run-all.sh)) tried to break this chapter's claims, `internal/walfile`, and Experiments 5.1–5.4 against the pinned SQLite 3.53.4 build (`HELD` means the code or claim stood up to the test; `BROKEN` means the test exposed a bug or an overly broad claim):

- [`wal-inspector-edge`](../agent_work/ch05/adversarial/wal-inspector-edge/main.go) (`INS-01`–`INS-07`, including `INS-05a`/`INS-05b`) tested Experiment 5.1 and `internal/walfile` across all 8 valid page sizes (`512..65536`), restarted WALs with older-generation tails, all 4 `synchronous` levels (`OFF`, `NORMAL`, `FULL`, `EXTRA`), big-endian magic `0x377f0683` against `showwal`, synthetic bad headers/versions/page-0/partial-frame tails, a real 512-byte-page WAL crossing the 4,062/4,063 `-shm` block boundary (`4,531` frames, `65,536`-byte `-shm`), a torn `-shm` header (copy 1 zeroed, copy 2 populated), 5,000 seeded parser/replayer mutations, and pin enforcement across Experiments 5.1–5.4.
- [`wal-replay-falsify`](../agent_work/ch05/adversarial/wal-replay-falsify/main.go) (`REP-01`–`REP-07`, including `REP-07a`/`REP-07b`) tested Experiment 5.2 and `walfile.Replay`/`ReplayFiles` with single-bit flips in the last and middle commits, replaying a generation-2 WAL onto a generation-0 base, naive replay past `LastCommitFrame`, `auto_vacuum=FULL` database shrinkage (`93` pages to `8`, `85` skipped), `cache_size=5` in-place spills with `SAVEPOINT`/`ROLLBACK TO`/`RELEASE`, page sizes `512` and `65536` including from an empty base, 0-commit header-only WAL base validation, and pre-existing `.replay-tmp` files.
- [`reader-pinning`](../agent_work/ch05/adversarial/reader-pinning/main.go) (`PIN-01a`, `PIN-01b`, `PIN-02`–`PIN-05`) tested Experiment 5.3 with 3 readers closing oldest-first, 5 readers at 5 distinct commits exceeding the 4 read-mark slots `aReadMark[1..4]` (confirming reader 5 shares slot 4 conservatively without error), a half-read `*sql.Rows` cursor with no explicit `BEGIN`, shrinking a ~1 MB WAL via `PRAGMA journal_size_limit` (`65536` and `0` shrink on the first commit after restart), `TRUNCATE`, and last close, a `WAL_READ_LOCK(0)` reader against all 4 checkpoint modes, and stale `-shm` marks immediately after a reader closes.
- [`checkpoint-races`](../agent_work/ch05/adversarial/checkpoint-races/main.go) (`CHK-01a`, `CHK-01b`, `CHK-02`–`CHK-05`) tested Experiment 5.4 with a writer holding `BEGIN IMMEDIATE` (`WAL_WRITE_LOCK`) against all 4 modes, 40 concurrent commits racing all 4 modes, a rogue second connection triggering autocheckpoint or `TRUNCATE`, cross-connection vs same-connection `TRUNCATE` salt/sequence behavior, two concurrent checkpointers contending on `WAL_CKPT_LOCK`, lock-free `NOOP` under active writer+checkpointer+reader locks, multi-connection close cleanup vs `Replay` E1, and `mode=ro` checkpoints before vs after backfill.

**Run 1** ([`results-run1.txt`](../agent_work/ch05/adversarial/results-run1.txt)) produced **21 HELD, 7 BROKEN**. Three failures exposed real defects in `internal/walfile` that were fixed:

- `INS-05b`: `ParseIndex` previously returned `HeadersAgree() == true` when copy 1 was zeroed and copy 2 was populated; fixed to decode copy 2's byte order and return `Initialized=false, HeadersAgree()==false` (`TestParseIndexTornHeader`).
- `REP-07a`: `Replay` previously returned early on `LastCommitFrame == 0` before checking `ErrPageSizeMismatch` and `ErrBaseSize` (and accepted invalid base page-size bytes); fixed to validate the base before the zero-commit return (`TestReplayValidatesBaseWithoutCommits`).
- `REP-07b`: `ReplayFiles` previously called `os.Remove` on a pre-existing `.replay-tmp` after `O_EXCL` failed; fixed to leave foreign temp files untouched (`TestReplayFilesKeepsForeignTempFile`).

**Run 2** ([`results-run2.txt`](../agent_work/ch05/adversarial/results-run2.txt)) passed **24 HELD, 4 BROKEN**. The four remaining failures reflected overly broad claims or test checks that needed narrowing to match measured SQLite behavior:

- `REP-02` confirmed that raw `walfile.Replay(gen0Base, gen2WAL)` cannot detect a stale-generation base because the `.db` header stores no WAL generation; the result fails `PRAGMA integrity_check` (`database disk image is malformed (11)`). Sections 5.2 and 6.2 state this boundary explicitly: safe replay requires capture metadata (Chapter 9).
- `REP-05` flagged `savepoint_ghost=true` by scanning raw output bytes for `"SAVEPOINT_SHOULD_VANISH"`, which also appeared in SQLite's own `TRUNCATE`-checkpointed file (`87` raw matches in unused page space on both sides, E1 byte-identical) with `0` SQL rows matching and `integrity_check = ok`. The test was updated to assert SQL row absence, integrity, and E1 parity.
- `CHK-03` assumed `salt-1` always increments by `+1` on restart, even across a cross-connection `TRUNCATE`. Instead, it observed `shm salt-1 + 1` followed by a fresh random `salt-1` and `ckpt_seq 0->0` when the other writer created the next header (`src/wal.c:4095-4096`). Sections 3.3 and 6.1 state the per-connection rule, and `CHK-03` was updated to test both cross-connection and same-connection `TRUNCATE`.
- `CHK-05` called `mode=ro` `PASSIVE` only after a `RESTART` checkpointer had already backfilled all 38 frames, misreading `checkpointed=38` as a read-only backfill. A dedicated probe ([`readonly-checkpoint`](../agent_work/ch05/repair-probes/readonly-checkpoint/main.go)) confirmed `mode=ro` `PASSIVE` fails with `disk I/O error (778)` and `nBackfill = 0` on an un-backfilled WAL and succeeds without writing only after backfill completes; `CHK-05` was updated to test both before and after backfill.

After aligning those four assertions to the verified SQLite rules, **Run 3** ([`results-run3.txt`](../agent_work/ch05/adversarial/results-run3.txt) and [`results.txt`](../agent_work/ch05/adversarial/results.txt)) passed **28 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every operational rule below follows directly from this chapter's experiments and verified source spans:

1. **One stuck reader grows the WAL without bound—even without a read mark (Experiment 5.3 A & B, `PIN-01a/b`, `PIN-02`).** A reader pinning mark 1 (`22`) prevented `533` of `555` frames from being checkpointed (`2,286,632` bytes after 24 commits); a reader opened on a fully checkpointed WAL held `WAL_READ_LOCK(0)` and kept `nBackfill = 0` for all `533` frames (`2,195,992` bytes). Even a single unclosed `*sql.Rows` cursor pins the WAL (`PIN-02`). Meanwhile autocheckpoint keeps firing on every commit past its threshold, syncing the WAL and accomplishing nothing. Bound read-transaction and cursor lifetimes on every database.
2. **The platform must own checkpoints; set `wal_autocheckpoint=0` on every application connection (Experiment 5.4, `src/main.c:2484-2496`, `CHK-02`).** Default autocheckpoint runs a `PASSIVE` checkpoint inside whichever connection commits past 1,000 frames, and any complete checkpoint lets the next write restart the WAL (Experiment 5.4). A WAL shipper that leaves autocheckpoint on—or lets another connection checkpoint—loses frames when generations wrap behind its back.
3. **Treat `-wal` as part of the database while any connection is open (Experiment 5.2 A).** Across 8 committed transactions with `wal_autocheckpoint=0`, `.db` stays byte-for-byte recipe step 0 (`16,384` bytes) while all committed state sits in the `638,632`-byte `-wal`. Capturing a live WAL database means capturing base + WAL together with generation metadata (Chapter 9) or checkpointing first.
4. **Never back up, ship, or open+close `-shm` in the serving process (Experiment 5.2 B, `internal/walfile/livecopy.go`, `src/os_unix.c:4703-4750`).** SQLite rebuilds `-shm` identically whether it is absent or replaced by a stale `-shm` from another database (`fb68a521…`). Because Unix SQLite stores WAL locks as POSIX `fcntl` locks on `-shm`, opening and closing `.db` or `-shm` inside a process holding SQLite connections drops those locks; copy live files only from a child process or after all connections close.
5. **Monitor three signals per database (Experiments 5.1, 5.3, 5.4):** `-wal` size in bytes; `log` and `checkpointed` from checkpoints or lock-free `PRAGMA wal_checkpoint(NOOP)` (`log - checkpointed` is the backlog pinned by readers); and `busy` from `FULL`/`RESTART`/`TRUNCATE` (`1` means a reader or writer blocked completion).
6. **Expect `-wal` not to shrink after `PASSIVE`, `FULL`, or `RESTART` (Experiments 5.3 A and 5.4, `PIN-03`).** After a full backfill, SQLite reuses the existing `-wal` file from frame 1 (`2,286,632` bytes in 5.3 A; `156,592` bytes in 5.4). To reclaim disk space after a WAL spike, run `wal_checkpoint(TRUNCATE)`, set `PRAGMA journal_size_limit` (which takes effect on the first commit after restart), or close the last connection.
7. **Detect WAL restarts by any change in `(salt-1, salt-2)` (Experiment 5.4, `src/wal.c:4095-4096`, `CHK-03`).** Do not assume `salt-1` or `checkpoint-seq` increments by 1: after another connection runs `TRUNCATE`, the next writer's header carries fresh random salts and `checkpoint-seq = 0`. Old-generation frames stay behind new frames in a reused WAL (`18 / 555` in 5.3 A, `23 / 38` in 5.4) and are excluded by the salt check.
8. **Stop shipping and replaying at `LastCommitFrame` (Experiment 5.2 B/C, `REP-01`, `REP-03`).** Rolled-back and in-flight spilled frames stay in `-wal` until overwritten and can carry valid checksums. Only frames up to the last valid commit frame are committed state.
9. **Keep WAL databases on local filesystems (`src/wal.c:130-133`).** WAL requires shared memory (`-shm`) and local POSIX locks; do not place WAL files on NFS or other network filesystems.

Summary of what running in WAL mode obliges a platform to own:

| Platform obligation in WAL mode | Supporting evidence |
| --- | --- |
| Run checkpoints from the platform (`wal_autocheckpoint=0` on app connections) | Experiment 5.4 (restart after any full backfill); `src/main.c:2484-2496`; `CHK-02` |
| Bound read-transaction and cursor lifetimes per database | Experiment 5.3 A & B; `PIN-01a/b`, `PIN-02`, `PIN-04` |
| Treat `-wal` as part of the database while open; pair base + WAL with metadata or checkpoint first | Experiment 5.2 A; `REP-02` |
| Never capture or ship `-shm`; never open+close `.db`/`-shm` inside a serving process | Experiment 5.2 B (missing and stale `-shm`); `src/os_unix.c:4703-4750` |
| Watch WAL size and `(busy, log, checkpointed)` (`NOOP` for cheap backlog checks) | Experiments 5.1, 5.3, 5.4; `CHK-04`, `CHK-05` |
| Detect WAL generation changes by comparing `(salt-1, salt-2)` | Experiment 5.4 (`TRUNCATE` cross-connection vs same-connection); `src/wal.c:4095-4096` |
| Stop replay/shipping at `LastCommitFrame` | Experiment 5.2 B & C; `REP-01`, `REP-03` |
| Keep databases on local storage with shared memory | `src/wal.c:130-133` (source-level constraint, not tested here) |

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 5.1, which exact byte regions differ between two WAL files produced by identical deterministic runs (and across `synchronous=FULL`, `NORMAL`, and `OFF`), which bytes of each 24-byte frame header does `walDecodeFrame` (`src/wal.c:1000-1053`) actually include in its running checksum, and how does that disagree with its own source comment?
2. In Experiment 5.2, what happens to the 19 spilled frames (`42–60`) in `-wal` after `ROLLBACK`, why does SQLite recover the exact same SHA-256 (`fb68a521…`) whether `-shm` is missing or replaced with Part A's stale `-shm` (`mxFrame=155`), and why can `walfile.Replay(base, wal)` not detect a base file from an earlier WAL generation?
3. In Experiment 5.3, compare how a reader pins the WAL in Part A (`nBackfill < mxFrame`) versus Part B (`nBackfill == mxFrame`). Why did the writer restart the WAL on step 7 in Part B while `nBackfill` stayed `0` through step 30, and why did `aReadMark[1]` still hold `22` right after the reader closed in Part A?
4. In Experiment 5.4, why did `RESTART` and `TRUNCATE` return `busy=1` against `"reader current"` while `PASSIVE` and `FULL` returned `busy=0`, and why did the writer's next commit after another connection's `TRUNCATE` write fresh random salts and `checkpoint-seq = 0` whereas `TRUNCATE` run by the writer itself produced `salt-1 + 1` and `checkpoint-seq = 1`?
5. In WAL mode, which operations call `fsync` under `PRAGMA synchronous=NORMAL` versus `PRAGMA synchronous=FULL` (`src/wal.h:25-26`, `src/pager.c:3658-3704`), what is wrong with the comment at `src/pager.c:607-617`, and why must a WAL-shipping platform set `wal_autocheckpoint=0` on every application connection?

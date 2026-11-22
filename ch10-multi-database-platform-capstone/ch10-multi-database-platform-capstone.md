# Chapter 10 — Multi-database platform capstone

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Object store | `SeaweedFS 4.48` (S3-compatible, via `compose.yaml`) |
| Copied C source in this directory (`SOURCES.sha256`) | `wal.c` |
| Linked C source (in the pinned tree, not copied) | `src/os_unix.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree (with the linked `src/os_unix.c`), the C tools (none of Chapter 10's four experiments invokes the C `sqlite3` CLI), `strace` (Experiment 10.2 Part A), and the DuckDB CLI that presents the results.
> - **Start SeaweedFS** (the local S3-compatible object store, `SeaweedFS 4.48`) from the repository root before running any S3-backed experiment (`10.1-one-writer -model-only` runs Part A without a store):
>   ```text
>   docker compose up -d seaweedfs
>   ```
>   Every S3-backed experiment exits `1` with `STORE UNREACHABLE:` if the endpoint does not answer within `10` seconds. The `lab` container reaches it at `COURSE_S3_ENDPOINT=http://seaweedfs:8333`.
> - **Run the experiments** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 10.4`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 10.1
>   go run ./cmd/course run 10.2
>   go run ./cmd/course run 10.3
>   go run ./cmd/course run 10.4
>   ```
>   Experiment 10.4 defaults to `500` databases (`-databases 500 -writers 8 -capacity 32`). `course run` cannot pass experiment flags, so run flag variants directly in the container; the full `2,000`-database run is:
>   ```text
>   docker compose run --rm lab go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet -databases 2000 -prefix exp-10-4-2000
>   ```
>   In the container, scratch files and tables go under `/work/10.1/` through `/work/10.4/` (the `lab-work` volume); run directly, under `_work/10.1/` through `_work/10.4/`, which are git-ignored. Both are wiped at the start of each run. Each experiment uses its own S3 key prefix (`exp-10-1/` in bucket `course-ch10a`; `exp-10-2/`, `exp-10-3/`, `exp-10-4/` in bucket `course-ch10b`).
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet`). Run this way it prints only its verdict and leaves its tables under `_work/10.4/results/`.
> - **Linux-only parts and macOS:** Experiments 10.1, 10.3, and 10.4 are portable (S3 over HTTP, `SIGKILL`, and descriptor/RSS sampling through `internal/procstat`). **Linux only:** Experiment 10.2 Part A kills the node process on entry to a chosen `fsync`/`fdatasync` using `strace`; `course run` runs it in the Linux `lab` container (which has `strace` and `SYS_PTRACE`). Run directly on macOS—or on Linux with `-portable`—Part A prints a `LINUX-ONLY:` notice while Parts B and C run natively and exit `0`.
> - **Measurement environment:** The stored evidence in this chapter, including the `strace` kill sweeps and the SeaweedFS runs, was captured in the Debian `lab` container on `linux/arm64` (Go 1.26, SeaweedFS 4.48). The adversarial suites were run separately, on a `linux/amd64` host (4 shared cores, 7.7 GiB RAM, descriptor soft limit `1,048,576`). Timings, byte totals and memory readings are run-dependent; `course run` prints them. macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and cross-vetted only; nothing was run on macOS.

---

## 1. Model

Chapters 1 through 9 showed how SQLite stores pages, commits transactions, writes a **WAL** (write-ahead log), locks files across processes on one host, bounds open connections, and captures a live database continuously into snapshots and WAL segments (`internal/walship`). None of those mechanisms stops two machines from writing the same database at the same time, tracks which node owns which file, moves a database cleanly from one node to another, or rebuilds an entire fleet onto empty disks. Those responsibilities belong to the platform built around SQLite.

### Three layers of guarantee

A multi-database platform fails when it blurs which component owns which guarantee. This capstone keeps three layers strictly separate:

1. **SQLite's local guarantees (per file, on one machine):**
   - **Atomic commit and local durability:** in WAL mode with `PRAGMA synchronous=FULL`, `walFrames` (`src/wal.c:4195-4213`) syncs the `-wal` file before `COMMIT` returns.
   - **Crash recovery:** after a process kill, the next open runs `walIndexRecover` (`src/wal.c:1390-1614`), verifies the WAL header and running frame checksums, stops at the first invalid frame, and keeps every transaction whose commit frame reached the kernel page cache.
   - **Snapshot isolation and single-writer exclusion on one host:** POSIX advisory byte-range locks (`fcntl`) keyed by local device and inode (`unixFileId` and `unixInodeInfo`, `src/os_unix.c:1189-1342`) keep the shipper's read mark and `BEGIN IMMEDIATE` cycles mutually consistent with the writer on the same host. Nothing in SQLite's locking reaches across machines.
   - **Clean close at rest:** when the last connection to a WAL database closes, `sqlite3WalClose` (`src/wal.c:2507-2571`) takes an `EXCLUSIVE` lock, runs a `PASSIVE` checkpoint, and deletes `-wal` and `-shm`, leaving a single at-rest `.db` file.

2. **The coordination the Go service adds (`ch10-multi-database-platform-capstone/service/`):**
   - **Control-plane catalog (`service/catalog`):** one WAL-mode SQLite database shared by all nodes on the host. Its `databases` table maps each database ID to `owner`, monotonically increasing `epoch`, `location`, `last_shipped` stream position, `schema_version` (`user_version`), and per-connection SQLite settings from Chapter 8. Its `relocations` table stores the coordinator's sole durable state for an in-flight or finished move.
   - **Fenced slot chain (`service/fence`):** every database's capture stream lives under fixed-width keys `dbs/<id>/log/<12-digit position>`. Each object begins with a single-line envelope (`fenced-slot v1 epoch=<e> kind=<snapshot|segment> first=<p> last=<q>`) followed by the unchanged Chapter 9 `walship` snapshot or segment bytes. Every new epoch opens with a full **fence snapshot**. A writer (`fence.Adapter`) lists the chain, refuses to proceed (`FencedError`) if any slot carries an epoch higher than its own, and appends only at `end + 1` of the chain it validated.
   - **Explicit acknowledgement modes (`service/node`):** every write chooses either **local-durable** (acknowledged after local commit at `synchronous=FULL`) or **store-durable** (acknowledged only after `walship` puts the segment into the object store).
   - **Crash-surviving local sidecars (`service/node`):** beside `<id>.db` (plus `-wal` and `-shm` while open), each node writes four small sidecar files atomically via write-temp-and-rename: `<id>.owner` (`epoch N\n`), `<id>.atrest` (64-hex SHA-256 after a clean close), `<id>.sealed` (`epoch N\n` while draining a relocation), and `<id>.staged` (the destination's rebuilt image before activation).
   - **Seven-step relocation (`service/relocate`):** catalog epoch bump, source seal, final drain to at-rest, destination rebuild into `.staged`, hash verification, store fence and activation, and source retirement—plus crash-safe `Resume` and `Rollback` decided from the catalog row and whether the destination's fence landed in the store.
   - **Standing resource bounds (`service/bounds`):** every node process checks open descriptors and RSS growth against Chapter 8's per-connection bounds after every command.

3. **What the object store provides (as measured on SeaweedFS 4.48, single server, on this machine):**
   - **Atomic put-if-absent (`If-None-Match: *`):** probed under `2` and `8` concurrent writers (`agent_work/probes/seaweedfs-s3/RESULT.md`), with strictly one winner and HTTP `412 PreconditionFailed` (`walship.ErrExists`) for every loser. Every slot refusal in Experiments 10.1 and 10.3 relies on this.
   - **Read-after-write and list-after-write visibility:** measured consistent on single-server SeaweedFS; Experiment 10.3's `Resume` relies on reading back fences whose replies were lost (`fail-after-put` and `crash-after-put`).
   - **Atomic single PUT:** objects are either absent or complete (`partial-put` in `service/faultstore` is a stress test for readers, not behavior observed from SeaweedFS).
   - **Not provided by S3 / SeaweedFS:** epoch comparison (an S3 server cannot parse an epoch inside an object and compare it against another object—it enforces only write-once key creation), sorted listing order (the service sorts keys itself), or multi-key transactions.
   - **Not measured and not claimed:** AWS S3 behavior, volume durability of SeaweedFS under disk/power loss, multi-server SeaweedFS clusters, or network partitions.

### Component diagram

```text
  client ──► node (router: id -> connmgr handle; ownership; ack mode; seal)
               │  Local files per database in <work>/<node>/:
               │    <id>.db (+ -wal, -shm while open; 3 SQLite conns = 7 FDs)
               │    <id>.owner   ("epoch N\n" belief surviving crashes)
               │    <id>.atrest  (64-hex sha256 after clean eviction/close)
               │    <id>.sealed  ("epoch N\n" refuses writes during relocation)
               │    <id>.staged  (destination's rebuilt image before activate)
               │
               ├─► connmgr (Chapter 8): <= K open writer connections, LRU eviction
               ├─► per open database: walship.Shipper (control + pin connections)
               │      └─► fence.Adapter (epoch e; dbs/<id>/log/<12-digit pos>)
               │             └─► faultstore (optional) ─► s3store ─► SeaweedFS S3
               └─► catalog (shared WAL SQLite DB on the host):
                      databases(id, owner, epoch, location, last_shipped,
                                schema_version, settings)
                      relocations(id, source, destination, from_epoch, to_epoch,
                                  step, position, sha256, state)

  relocation coordinator ──► catalog (steps 1, 5, progress) + source/dest nodes (2-4, 6, 7)
  checker (10.4)         ──► SeaweedFS S3 only (lists dbs/, rebuilds every restore point)
```

### Durability model

Every write names one of two modes:

| Mode | When acknowledged | Survives process kill? | Survives deleting all local files (store rebuild)? | Survives takeover (`claim`)? | Survives relocation (`relocate`)? | Power cut (from Chapter 9.2) |
| --- | --- | --- | --- | --- | --- | --- |
| **`local-durable`** | After local SQLite `COMMIT` at `synchronous=FULL` (`ErrNotLocallyDurable` otherwise) | Yes (restart on local files; Experiment 10.2) | No (unshipped commits are only on local disk) | **No** — unshipped commits are lost (Experiment 10.1 step 3–4) | **Yes** — step 3 drains local commits before fencing (Experiment 10.3) | Zero acknowledged commits lost if the disk honors `fsync` (Chapter 9.2) |
| **`store-durable`** | Only after `walship` puts the segment into S3 (`If-None-Match: *`) | Yes (Experiment 10.2) | **Yes** — rebuilt from S3 alone (Experiments 10.2, 10.4) | **Yes** (Experiment 10.1) | **Yes** (Experiment 10.3) | Survives host loss as long as the store kept the accepted object |

**WAL mode as the platform default (ruling 10).** WAL is not merely the default here—it is **required** by the design. `internal/walship` reads committed frames directly from `-wal` while holding a read mark on the wal-index (`-shm`), which rollback-journal mode has no equivalent for; and the shared control-plane `catalog.db` runs in WAL mode so multiple node processes can read it concurrently while one writes. Chapter 10 measures two operational costs of WAL at fleet scale:

1. **Sync amplification per store-durable write:** in Experiment 10.2, `12` local-durable writes triggered `77` `fsync`/`fdatasync` calls and `12` store-durable writes triggered `85` (each write pays the WAL commit sync at `synchronous=FULL`, the shipper's checkpoint syncs, and the catalog update sync). That sync load makes the `500`- and `2,000`-database write streams in Experiment 10.4 I/O-bound.
2. **Last-close WAL deletion:** `sqlite3WalClose` deletes `-wal` and `-shm` on clean close. That gives relocation step 3 and Experiment 10.4's quiesce a single at-rest file (`WAL frames found: 0`), and `walship.AdoptAtRest` prevents clean reopens from uploading a full snapshot. Conversely, if a relocation drain's final segment PUT fails before reaching the store, the eviction's close still checkpoints the unshipped frames into `.db`, so the retried drain finds no WAL and must upload a full **discontinuity snapshot** instead of a segment (Experiment 10.3 cases 22 and 26).

No experiment in Chapter 10 runs in rollback mode, and there is no exception to WAL as the platform default.

**Where this chapter sits on the Rebuild Ladder (rung R7).** Rung **R7 (Fleet)** closes the course's through-line: R6's continuous capture (`internal/walship`) applied to every database owned by a bounded multi-process service, combined with catalog ownership and store-backed fencing, verified by wiping every local file and rebuilding the entire fleet from the object store alone to **E1 bytes** (`500/500` by default; `2,000/2,000` at full outline scale).

**Design context (prior art).** Production WAL shippers such as Litestream ([how it works](https://litestream.io/how-it-works/)) use the same SQLite mechanism proven in Chapters 5 and 9—holding a read transaction to prevent uncontrolled checkpoints while reading WAL frames and triggering checkpoints manually, shipping ordered TXID-indexed LTX files ([superfly/ltx](https://github.com/superfly/ltx))—while LiteFS ([ARCHITECTURE.md](https://github.com/superfly/litefs/blob/main/docs/ARCHITECTURE.md)) intercepts filesystem calls via FUSE and tracks a rolling XOR-of-CRC64 checksum over pages (see [`docs/knowledge/determinism-and-rebuild.md`](../docs/knowledge/determinism-and-rebuild.md); Litestream's internal WAL-restart detection is not verified against its source here). This capstone stays in plain Go on local disk with no FUSE layer, using `walship` segments wrapped in write-once fenced S3 slots.

---

## 2. Question

Can a small Go service own thousands of SQLite database files—bounded in memory and file descriptors, with at most one authorized writer per database, and every database rebuildable byte for byte from saved state—and keep all six capstone invariants when processes die, object-store calls fail, and relocations are interrupted?

### The six capstone invariants as checkable properties

| # | Capstone invariant | Checkable property | Checked by |
| --- | --- | --- | --- |
| 1 | **At most one authorized writer owns a database at a time** | (a) In store order: once an epoch's object is accepted, no older epoch's object is accepted; no two epochs are accepted for overlapping positions; no accepted slot is overwritten. (b) At every relocation step boundary, at most `1` node reports `writable`. | Experiment 10.1 (`9,595,507,962` interleavings to depth `16`, `0` violations; two broken mutants fail; real processes agree `13/13`). Experiment 10.3 (`most writable nodes at any boundary 1` in `32/32` fault cases; `skip-fence` self-test finds `2`). |
| 2 | **No acknowledged durable write is lost under the stated durability model** | For every database, recovered `user_version >=` last `user_version` acknowledged in its mode. Local-durable: restart on local files. Store-durable: delete all local files and rebuild from S3. Relocation: every ack (either mode) is in the final lineage. Takeover: store-durable kept; unshipped local-durable may be lost. | Experiment 10.2 (`8` `strace` fsync kills + `4` in-flight random kills, `0` violations; `ack-before-put` self-test caught). Experiment 10.3 (`final user_version == acknowledged count` in `32/32` cases). Experiment 10.1 (takeover). |
| 3 | **Every published snapshot is internally consistent** | Every published restore point (every position of every database in the store) rebuilds to a valid recipe state (SHA-256 matches the recipe's step file, exact or differing only at change counter bytes `24–27 == 92–95`). | Experiment 10.4 (concurrent checker passes while writers run + final pass: `3,248` restore points at `500` DBs and `13,000` at `2,000` DBs, `0` failures). Experiment 10.3 (every store-durable ack slot restores to its `user_version`). |
| 4 | **A database can be reopened after process failure** | After `SIGKILL` at any chosen fsync or random time, every database opens cleanly, `PRAGMA integrity_check = ok`, and the file matches a recipe state. | Experiment 10.2 (`reopened 4 of 4` after every kill). Experiment 10.1 step 14 (reopen after killing node `b` with an open WAL). Experiment 10.3 (killed nodes restart and finish). |
| 5 | **File descriptors and memory remain bounded** | After every command of every node process: descriptors above startup baseline `<= 3 * 3 * K + 32` and RSS growth `<= 3K * (2 * cache + 256 KiB) + 32 MiB` (Chapter 8's per-connection bounds for `3` connections per open database). | Standing assertion inside every experiment: 10.1 (`15` commands), 10.2 (`308` replies), 10.3 (`1,726` replies), 10.4 (`2,760` replies at `500` DBs; `11,016` at `2,000` DBs; plus rebuild workers and checker). Reply counts after 10.1 are run-dependent; these are from one lab run of each. |
| 6 | **An interrupted migration cannot silently create two authoritative copies** | Under a crash or store fault at every relocation step, after `Resume` or `Rollback`: catalog owner, newest store slot epoch, and `relocations` record agree; `<= 1` writable node at every boundary; destination image equals drained source state at the fence point (SHA-256); every acknowledged write is present and no refused write is; owner's closed file equals the store's last state. | Experiment 10.3 (`32 of 32` fault cases hold every invariant; `skip-fence` self-test fails as designed). |

### Reused shared packages and new code in this chapter

Chapter 10 imports seven shared packages from earlier chapters (linked here, not inlined): [`internal/sqlitex`](../internal/sqlitex/), [`internal/recipe`](../internal/recipe/), [`internal/connmgr`](../internal/connmgr/), [`internal/procstat`](../internal/procstat/), [`internal/rangefs`](../internal/rangefs/), [`internal/trace`](../internal/trace/), and [`internal/walship`](../internal/walship/).

Because `internal/connmgr/connmgr.go` is inlined byte for byte in Chapter 8 and `internal/walship/shipper.go` is inlined byte for byte in Chapter 9, neither file was modified. Instead, Chapter 10 adds one new file to `internal/walship`—[`internal/walship/atrest.go`](../internal/walship/atrest.go) (`AdoptAtRest`)—and introduces nine capstone packages under `ch10-multi-database-platform-capstone/service/` (`s3store`, `faultstore`, `fence`, `catalog`, `bounds`, `model`, `node`, `relocate`, `lab`). Every non-test `.go` file introduced by this chapter is inlined once in Section 5.

---

## 3. Call path

Every span cited below is listed in [`SPANS.txt`](SPANS.txt) and verified against SQLite `version-3.53.4` in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`). [`wal.c`](wal.c) is copied whole into this chapter directory and verified by [`SOURCES.sha256`](SOURCES.sha256); `src/os_unix.c` is linked at the pinned tag.

### 3.1 Shipper read marks, checkpoints, and WAL restarts

1. **`walTryBeginRead`** — [`src/wal.c:3020-3272`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3020-L3272) (local copy: [`wal.c`](wal.c)). When `walship` opens its pin connection and starts a read transaction, SQLite picks read-lock **slot `0`** if the WAL is fully backfilled (`nBackfill == mxFrame`, meaning readers read `.db` only) or a **slot `1–4`** pinned to a frame mark `<= mxFrame` otherwise.
2. **`walCheckpoint`** — [`src/wal.c:2199-2395`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395) (local copy: [`wal.c`](wal.c)). Before copying frames back into `.db`, `walCheckpoint` lowers `mxSafeFrame` to the oldest held read mark across slots `1–4` (lines `2233–2251`), then takes an exclusive lock on **slot `0`** (lines `2259–2260`), verifies the WAL salts did not change (`memcmp`, line `2272`), syncs `-wal` (`sqlite3OsSync`, line `2277`), and copies pages up to `mxSafeFrame` (lines `2304–2321`). Only when `mxSafeFrame == mxFrame` does it truncate and sync `.db` (lines `2328–2334`):
   ```c
   mxSafeFrame = pWal->hdr.mxFrame;
   mxPage = pWal->hdr.nPage;
   for(i=1; i<WAL_NREADER; i++){
     u32 y = AtomicLoad(pInfo->aReadMark+i); SEH_INJECT_FAULT;
     if( mxSafeFrame>y ){
       rc = walBusyLock(pWal, xBusy, pBusyArg, WAL_READ_LOCK(i), 1);
       if( rc==SQLITE_OK ){
         ...
       }else if( rc==SQLITE_BUSY ){
         mxSafeFrame = y;
         xBusy = 0;
       }
     }
   }
   ```
   Every platform writer runs with `wal_autocheckpoint=0` (`node.PlatformSettings()`), so only the shipper checkpoints, and its held read mark guarantees no checkpoint can copy unshipped frames past what the shipper already put in the store.
3. **`walRestartLog` and `walRestartHdr`** — [`src/wal.c:3875-3915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3875-L3915) and [`src/wal.c:2152-2166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2152-L2166) (local copy: [`wal.c`](wal.c)). A writer restarts the WAL at frame `1` only when every frame has been backfilled and it can lock read slots `1–4` exclusively. `walRestartHdr` increments `nCkpt`, zeroes `mxFrame` and `nBackfill`, increments `salt-1` by `1`, and writes a fresh random `salt-2`:
   ```c
   pWal->nCkpt++;
   pWal->hdr.mxFrame = 0;
   sqlite3Put4byte((u8*)&aSalt[0], 1 + sqlite3Get4byte((u8*)&aSalt[0]));
   memcpy(&pWal->hdr.aSalt[1], &salt1, 4);
   walIndexWriteHdr(pWal);
   AtomicStore(&pInfo->nBackfill, 0);
   ```
   That salt change is how `walship` distinguishes a normal WAL restart after its own full checkpoint from an unauthorized outside checkpoint (discontinuity), and why WAL files are never compared byte for byte.

### 3.2 Clean close at rest and `AdoptAtRest`

4. **`sqlite3WalClose`** — [`src/wal.c:2507-2571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2507-L2571) (local copy: [`wal.c`](wal.c)). When a node evicts or drains a database, `connmgr` closes the writer connection first and `Node.evicted` runs a final `walship.Cycle` and closes the shipper's two connections. Because the shipper's close is the **last** connection to the database, `sqlite3WalClose` acquires `SQLITE_LOCK_EXCLUSIVE` on `.db` (line `2529`), runs a `PASSIVE` checkpoint (lines `2534–2536`), and—because `PERSIST_WAL` is not set—sets `isDelete = 1` (line `2546`) and unlinks `-shm` and `-wal` (lines `2559–2565`):
   ```c
   if( zBuf!=0
    && SQLITE_OK==(rc = sqlite3OsLock(pWal->pDbFd, SQLITE_LOCK_EXCLUSIVE))
   ){
     rc = sqlite3WalCheckpoint(pWal, db,
         SQLITE_CHECKPOINT_PASSIVE, 0, 0, sync_flags, nBuf, zBuf, 0, 0
     );
     if( rc==SQLITE_OK ){
       ...
       if( bPersist!=1 ){
         isDelete = 1;
       }
     }
   }
   ```
   Three capstone behaviors come directly from these lines:
   - Relocation step 3 (`Drain`) and Experiment 10.4's quiesce leave a single at-rest `.db` file with `WAL frames found: 0`.
   - If an external long reader stays open during relocation, `sqlite3WalClose` cannot obtain `SQLITE_LOCK_EXCLUSIVE`, `-wal`/`-shm` stay on disk, and `Drain` refuses cleanly rather than handing off an uncheckpointed file (verified by adversarial attack `REL-03a`).
   - On reopen after a clean eviction, `-wal` is gone while `walship`'s saved `WALPosition` still names the old WAL generation. Without `AdoptAtRest`, `walship.Cycle` would treat the missing WAL as a discontinuity and upload a full snapshot on every eviction reopen. `AdoptAtRest` (`internal/walship/atrest.go`) checks under `BEGIN IMMEDIATE` that `-wal` has zero committed frames and that every page through `sqlite_dbpage` hashes to the last shipped state, resetting `WALPosition` so the next commit ships as a cheap `WALNewGeneration` segment at frame `1`.

### 3.3 Local-durable commits and post-crash recovery

5. **`walFrames`** — [`src/wal.c:4038-4266`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4038-L4266) (local copy: [`wal.c`](wal.c)). Appends a transaction's dirty pages to `-wal`. A brand-new WAL writes and syncs its 32-byte header first (lines `4108–4124`). When `isCommit` is true and `WAL_SYNC_FLAGS(sync_flags) != 0` (`PRAGMA synchronous=FULL`, required by `Node.Write` for `LocalDurable`), lines `4195–4214` call `sqlite3OsSync` on `-wal` (padding to a sector boundary if the VFS requires it) before `COMMIT` returns:
   ```c
   if( isCommit && WAL_SYNC_FLAGS(sync_flags)!=0 ){
     ...
     if( bSync ){
       assert( rc==SQLITE_OK );
       rc = sqlite3OsSync(w.pFd, WAL_SYNC_FLAGS(sync_flags));
     }
   }
   ```
   Those syncs—plus directory syncs after `-wal` creation and `catalog.db-wal` syncs after shipping—are the exact syscalls Experiment 10.2 Part A kills with `strace`.
6. **`walIndexRecover`** — [`src/wal.c:1390-1614`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L1390-L1614) (local copy: [`wal.c`](wal.c)). When a restarted node reopens a database after `SIGKILL`, `walIndexRecover` takes exclusive locks (line `1407`), validates the 32-byte WAL header magic and checksum (lines `1445–1466`), walks every frame verifying its running checksum via `walDecodeFrame` (line `1510`), stops at the first invalid or incomplete frame, and advances `mxFrame` only at commit frames carrying a non-zero database size (lines `1516–1518`). Every committed transaction whose commit frame reached the OS kernel survives, and any partial write is discarded (`reopened 4 of 4` in Experiment 10.2 and `8/8` mid-`pwrite64` kills in adversarial attack `KILL-05`).

### 3.4 Why POSIX locks cannot provide cross-machine ownership

7. **POSIX advisory locking, `unixFileId`, `unixInodeInfo`, `unixFileLock`, and `unixLock`** — [`src/os_unix.c:1189-1342`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1189-L1342), [`1282-1296`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1282-L1296), [`1323-1342`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1323-L1342), [`1802-1833`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1802-L1833), and [`1866-2095`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1866-L2095) (linked from the pinned tree; also see [SQLite Over a Network](https://www.sqlite.org/useovernet.html) and [How To Corrupt An SQLite Database File §2](https://www.sqlite.org/howtocorrupt.html)).
   - `unixFileId` identifies an open file solely by its local kernel's `(dev, ino)` pair.
   - `unixInodeInfo` keeps process-wide lock counters (`nShared`, `nLock`, `eFileLock`), deferred-close file descriptors (`pUnused`), and the single shared-memory node (`pShmNode`) per inode—which is why one open service database with `3` SQLite connections uses `7` file descriptors (`3` `.db` + `3` `-wal` + `1` shared `-shm`), not `9`.
   - `unixLock` drives `SHARED -> RESERVED -> PENDING -> EXCLUSIVE` transitions through `unixFileLock` -> `osSetPosixAdvisoryLock` (`fcntl`), whose lock table lives inside one local kernel and vanishes when the process exits.
   - Because another machine has its own kernel, inode table, and lock table—and network filesystem lock implementations cannot be trusted by SQLite—cross-machine single-writer ownership cannot come from SQLite. It must live above SQLite in the catalog epoch and the object store's write-once slots.

---

## 4. Timeline

Chapter 10 moves ownership in two ways: an abrupt **takeover** (`Node.Claim`, Experiment 10.1) and a cooperative seven-step **relocation** (`relocate.Coordinator`, Experiment 10.3). Keeping their timelines side by side shows why they differ on local-durable acknowledgements and why bumping the catalog epoch alone is never a fence.

### 4.1 Takeover (`claim`) timeline

From Experiment 10.1 Part B steps `2–7` on `db-0001` (node `b`'s store wrapper injects `stale-list:/log/:1`):

| # | Event | Node `a` | Node `b` | Catalog (`owner, epoch`) | Store slots (`dbs/db-0001/log/`) |
| --- | --- | --- | --- | --- | --- |
| 0 | After step 2 (`store-durable`) | owner `e1`, local step `1`, shipped `2` | — | `a, 1` | `1` (`e1` snap, step `0`), `2` (`e1` seg, step `1`) |
| 1 | Step 3: `a` writes `local-durable` | local step `2` (acked), shipped `2` | — | `a, 1` | unchanged |
| 2 | Step 4a: `b` calls `catalog.Claim` | unchanged (does not read catalog) | claiming `e2` | `b, 2` | unchanged |
| 3 | Step 4b: `b` lists (stale fault hides slot `2`) | unchanged | sees end `= 1`, restores step `0` | `b, 2` | unchanged |
| 4 | Step 4c: `b` puts fence at slot `2` | unchanged | `412 PreconditionFailed`; reads holder `e1 < e2` -> retry (`attempts = 2`) | `b, 2` | unchanged |
| 5 | Step 4d: `b` re-lists (fresh) -> end `2`, restores step `1`, puts fence at slot `3` | unchanged | **accepted**; writes `db-0001.db` = step `1`, writes `.owner` (`epoch 2`), updates catalog | `b, 2` (`last_shipped 3`) | `+ 3` (`e2` snap, step `1`) |
| 6 | Step 5: `a` writes `store-durable` (commits step `3` locally, tries to ship `3–4` at slot `3`) | `412`; reads holder `e2 > e1` -> **`FencedError`**, deletes `.owner`, evicts; write **not** acknowledged | owner `e2` | `b, 2` | unchanged |
| 7 | Step 7: `b` writes `store-durable` | fenced (refuses step 6 at once) | ships step `2` at slot `4` | `b, 2` (`last_shipped 4`) | `+ 4` (`e2` seg, step `2`) |

Two consequences follow directly from this timeline:

1. **Ownership is lost when the new owner's fence lands in the store (event 5), not when the catalog bumps (event 2).** Any segment `a` shipped between events 2 and 5 would simply land before `b`'s fence in the same unbroken lineage, causing `b` to retry at the new end.
2. **A takeover loses `a`'s unshipped `local-durable` acknowledgement (step `2` from event 1)** as well as `a`'s unacknowledged local commit (step `3` from event 6). Store-durable acknowledgements (step `1`, slot `2`) survive.

### 4.2 Seven-step relocation timeline

From Experiment 10.3 case `7` (every step boundary `1–6` reached; step `7` with `b` alive verified by `TestRelocationMovesOwnershipAndKeepsEveryWrite` and every case's `after recovery` boundary). Before step 1: `a` owns `db-0001` at epoch `1`; store holds slots `1` (`e1` snap, step `0`), `2` (`e1` seg, step `1`), `3` (`e1` seg, step `2`); `a`'s local file also holds unshipped `local-durable` step `3`.

| After step | Step name (`relocate.Step`) | Catalog (`owner, epoch`) | Node `a` (source) | Node `b` (destination) | Store end | Client writes at this boundary |
| --- | --- | --- | --- | --- | --- | --- |
| 0 (before) | — | `a, 1` | `owner at epoch 1, writable`; local step `3` unshipped | `not owner` | `3` (`e1` segment) | — |
| 1 | `FenceEpoch` (catalog txn: owner `:= b`, epoch `:= 2`, `relocations` row step `1`) | `b, 2` | `owner at epoch 1, writable` (does not consult catalog) | `not owner` | `3` (`e1` segment) | To `b` (`store-durable`): refused (`not the owner`). To `a` (`local-durable`): **accepted** (step `4`)! |
| 2 | `SealSource` (`a` writes `db-0001.sealed`) | `b, 2` | `owner at epoch 1, sealed` | `not owner` | `3` (`e1` segment) | Both refused (`b` not owner; `a` sealed). |
| 3 | `ShipFinal` (`a` drains, ships steps `3–4`, closes at rest -> `P=5`, `H=d53fc07b1293…`) | `b, 2` | `owner at epoch 1, sealed`; `.db` at rest `== H` | `not owner` | `5` (`e1` segment `4-5`) | Both refused. |
| 4 | `Rebuild` (`b` stages restore of `5` into `db-0001.staged`) | `b, 2` | `owner at epoch 1, sealed` | `not owner`; `.staged` written | `5` (`e1` segment) | Both refused. |
| 5 | `Verify` (coordinator checks staged `P == 5` and `sha256 == H`) | `b, 2` | `owner at epoch 1, sealed` | `not owner`; `.staged` verified | `5` (`e1` segment) | Both refused. |
| 6 | `Activate` (`b` puts `e2` fence snapshot `= H` at slot `6`, installs `.db` and `.owner`, updates catalog) | `b, 2` (`last_shipped 6`) | `owner at epoch 1, sealed` (and fenced at slot `6` even if unsealed) | **`owner at epoch 2, writable`** | `6` (`e2` snapshot `= H`) | To `b` (`store-durable`): **accepted** (step `5`, slot `7`). To `a`: refused (`sealed`). |
| 7 | `RetireSource` (`a` deletes `.db`, `.owner`, `.sealed`, `.atrest`, `.staged`) | `b, 2` | `not owner` (no local files) | `owner at epoch 2, writable` | `7` (`e2` segment) | To `b`: accepted (verified in unit test and `after recovery`). To `a`: refused (`not the owner`). |

Notice two corrections over a naive six-step design:

- **Bumping the epoch in step 1 does not stop `a`.** At boundary 1, `a` is still `writable` and accepts a `local-durable` write (step `4`). That is safe—and never creates two writable nodes—because `b` is still `not owner`. Step 2 (`SealSource`) is the cooperative pause that stops new writes on `a` so step 3 (`ShipFinal`) can drain both unshipped local-durable commits (steps `3` and `4`) into the store before step 6 (`Activate`) binds any uncooperative source with the store fence.
- **If the final ship fails after `a`'s close (or `a` is killed before draining), the fence-point position shifts from `5` to `4` while the state hash stays identical.** Closing `a` checkpoints steps `3` and `4` into `.db` and deletes `-wal`, so the retried drain uploads one **discontinuity snapshot** (`4-4 e1 snapshot`) instead of segment `4-5` (`4-5 e1 segment`). Both produce the exact same drained SHA-256 (`d53fc07b1293…`), and `b`'s fence lands at `P + 1` (`5` instead of `6`).

### 4.3 Interrupted relocation: resume and rollback decision tree

Because the coordinator's only durable state is the catalog's `relocations` row, a fresh coordinator recovering from any crash or fault lists the database's slot chain from S3 and branches on the newest epoch present in the store:

```text
  Read catalog relocations row (from_epoch -> to_epoch, position P, sha256 H)
  List store chain -> newest_epoch

  ├── newest_epoch > to_epoch
  │     ├── Rollback AND catalog says owner == source at newest_epoch?
  │     │     └── Prior rollback stopped after source.Claim: finish rolled-back,
  │     │         best-effort destination Discard (Result.DiscardErr)
  │     └── Otherwise (another node took over)
  │           └── Mark relocations row superseded, return ErrSupersededBy
  │
  ├── newest_epoch == to_epoch (destination's fence already landed in S3)
  │     ├── Resume   ──► adopt(P, H): destination Activate (adopt + repair catalog),
  │     │                source Retire, mark done (RolledForward)
  │     └── Rollback ──► cannot undo a landed fence: call Resume + return ErrFenceInStore
  │
  └── newest_epoch < to_epoch (no destination fence in S3 yet)
        ├── Resume   ──► replay idempotent steps 2..7 (Seal, Drain, Stage, Verify,
        │                Activate, Retire), mark done (RolledForward)
        └── Rollback ──► source Seal -> source Drain (keeps local-durable acks)
                         -> source Claim (epoch = to_epoch + 1, fence at end + 1)
                         -> mark rolled-back in catalog
                         -> best-effort destination Discard (Result.DiscardErr)
```

Marking `rolled-back` **before** calling `Destination.Discard` is critical (exposed by adversarial attack `REL-04`): once the source's higher-epoch fence is in S3, a dead destination's `.staged` file can never activate, so an unreachable destination must not leave `relocations.state = running` wedged in the control plane.

---

## 5. Lab

This section builds the capstone service piece by piece—every non-test Go file introduced by Chapter 10 is inlined in full below—and then runs Experiment 10.1 to prove the single-writer fencing invariant on both an exhaustive finite-state model and two real service processes on SeaweedFS.

### 5.1 Adopting an at-rest database on reopen (`internal/walship/atrest.go`)

When `connmgr` evicts a database and closes the shipper, `sqlite3WalClose` checkpoints and unlinks `-wal`. Without `AdoptAtRest`, reopening that database would trigger `WALDiscontinuity` and upload a full snapshot on every eviction reopen. `AdoptAtRest` verifies under `BEGIN IMMEDIATE` that `-wal` contains no committed frames and that every page through `sqlite_dbpage` hashes to the last shipped state, allowing the next commit to ship as a small `WALNewGeneration` segment at frame `1`.

<!-- include: ../internal/walship/atrest.go -->
```go
package walship

import (
	"context"
	"fmt"
)

// AdoptAtRest resumes a closed, fully checkpointed database without uploading a
// discontinuity snapshot. When the last connection closes, sqlite3WalClose
// (src/wal.c:2507-2571) checkpoints and deletes the -wal file; on reopen the
// shipper's saved WALPosition still names the deleted WAL generation, so a
// plain Cycle would see a WAL header mismatch with no held read mark and upload
// a full snapshot.
//
// Under BEGIN IMMEDIATE on the control connection, AdoptAtRest checks that the
// -wal holds no committed frames and that every page read through sqlite_dbpage
// hashes to the shipper's last shipped state. When both hold, it clears the
// saved WALPosition and marks the shipper continuous and fully backfilled: the
// pin taken in Open sits on read slot 0 while no WAL exists, which blocks any
// backfill (src/wal.c:2259-2260), and the next commit starts a fresh WAL
// generation at frame 1 that Plan accepts.
func (s *Shipper) AdoptAtRest(ctx context.Context) (bool, error) {
	if s.broken != nil {
		return false, fmt.Errorf("walship: an earlier cycle failed; reopen the shipper to resume from the store: %w", s.broken)
	}
	adopted := false
	err := s.withWriteLock(ctx, func() error {
		_, file, err := s.readWAL()
		// Any committed frame in -wal means the database is not at rest; leave
		// the shipper unchanged so the next Cycle handles it normally.
		if err != nil || lastCommit(file) > 0 {
			return err
		}
		image, err := s.readPages(ctx)
		if err != nil {
			return err
		}
		// Refuse to adopt if the file bytes diverge from the last shipped state
		// (for example, an offline write while closed).
		if SHA256Hex(image) != s.state {
			return nil
		}
		s.wal = WALPosition{}
		s.continuous, s.fullyBackfilled, adopted = true, true, true
		return nil
	})
	if err != nil {
		s.broken = err
	}
	return adopted, err
}
```

### 5.2 Object store client and batch wipe (`service/s3store`)

`s3store` adapts SeaweedFS S3 (via `aws-sdk-go-v2` and `internal/rangefs`) to `walship.Store`. It maps HTTP `412 PreconditionFailed` on `If-None-Match: "*"` to `walship.ErrExists`, `NoSuchKey`/`NotFound` to `walship.ErrNotFound`, and unreachable endpoints to `ErrUnreachable`. `WipeInBatches` uses S3 multi-object `DeleteObjects` (`1,000` keys per batch) so Experiments 10.2–10.4 can reset prefixes holding thousands of slots quickly.

<!-- include: service/s3store/s3store.go -->
```go
// Package s3store implements walship.Store on an S3-compatible object store
// (SeaweedFS from compose.yaml) using aws-sdk-go-v2.
//
// It owns the thin translation between walship keys and S3 requests under one
// bucket and key prefix. It relies only on three S3 behaviors probed against
// SeaweedFS: atomic conditional put via If-None-Match: "*", read-after-write
// visibility, and list-after-write visibility. It does not rely on listing
// order (callers sort keys themselves) or on If-Match.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/walship"
)

// ErrUnreachable reports that the configured S3 endpoint did not answer the
// bucket check within Connect's deadline.
var ErrUnreachable = errors.New("the S3 store is unreachable")

// Store is a walship.Store backed by one S3 bucket and normalized key prefix.
type Store struct {
	Client   *s3.Client
	Bucket   string
	Prefix   string
	Endpoint string
}

// Connect builds a path-style S3 client for config, ensures bucket exists
// within a 10-second probe deadline, and returns a Store scoped to prefix.
func Connect(ctx context.Context, config rangefs.S3Config, bucket, prefix string) (*Store, error) {
	if err := walship.ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
		return nil, err
	}
	client := rangefs.NewS3Client(config)
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rangefs.EnsureBucket(probe, client, bucket); err != nil {
		return nil, fmt.Errorf("%w at %s (start it with: docker compose up -d seaweedfs): %v", ErrUnreachable, config.Endpoint, err)
	}
	return &Store{Client: client, Bucket: bucket, Prefix: strings.TrimSuffix(prefix, "/") + "/", Endpoint: config.Endpoint}, nil
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// PutIfAbsent creates key only if it does not already exist in the bucket by
// sending PutObject with If-None-Match: "*". A 412 PreconditionFailed response
// is mapped to walship.ErrExists.
//
// If the SDK retries a PUT after a transient error and the first attempt
// succeeded, the retry receives PreconditionFailed for the caller's own bytes;
// fence.Adapter handles that by reading the existing slot and comparing bytes.
func (s *Store) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := walship.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(s.Prefix + key),
		Body:        bytes.NewReader(data),
		IfNoneMatch: aws.String("*"),
	})
	if errorCode(err) == "PreconditionFailed" {
		return fmt.Errorf("%w: %s", walship.ErrExists, key)
	}
	return err
}

// Get fetches key and returns its bytes, mapping NoSuchKey or NotFound to
// walship.ErrNotFound and rejecting a truncated body shorter than Content-Length.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	if err := walship.ValidateKey(key); err != nil {
		return nil, err
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.Prefix + key)})
	switch code := errorCode(err); {
	case code == "NoSuchKey" || code == "NotFound":
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	case err != nil:
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	if out.ContentLength != nil && int64(len(data)) != *out.ContentLength {
		return nil, fmt.Errorf("get %s: read %d bytes, Content-Length %d", key, len(data), *out.ContentLength)
	}
	return data, nil
}

// List returns every key under prefix with the store's own prefix stripped.
// Keys are returned in whatever order the server lists them; walship and fence
// sort keys themselves.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if err := walship.ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, err
		}
	}
	pages := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(s.Prefix + prefix)})
	var keys []string
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			key, ok := strings.CutPrefix(aws.ToString(object.Key), s.Prefix)
			if !ok {
				return nil, fmt.Errorf("list %s: key %q is outside the prefix %s", prefix, aws.ToString(object.Key), s.Prefix)
			}
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// Delete removes key from the bucket. S3 DeleteObject is idempotent, so
// deleting a missing key returns nil.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := walship.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.Prefix + key)})
	return err
}

// Wipe deletes every object under s.Prefix and returns how many objects were
// deleted. Experiments call it at startup so each run begins from an empty prefix.
func (s *Store) Wipe(ctx context.Context) (int, error) {
	keys, err := s.List(ctx, "")
	if err != nil {
		return 0, err
	}
	for i, key := range keys {
		if err := s.Delete(ctx, key); err != nil {
			return i, err
		}
	}
	return len(keys), nil
}
```

<!-- include: service/s3store/batch.go -->
```go
package s3store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// deleteBatch is S3's maximum object count per DeleteObjects request.
const deleteBatch = 1000

// WipeInBatches lists every object under s.Prefix, removes them in S3
// DeleteObjects batches of up to 1,000 keys, and re-lists to confirm the prefix
// is empty before returning the deleted object count.
func (s *Store) WipeInBatches(ctx context.Context) (int, error) {
	keys, err := s.List(ctx, "")
	if err != nil {
		return 0, err
	}
	deleted := 0
	for start := 0; start < len(keys); start += deleteBatch {
		batch := keys[start:min(start+deleteBatch, len(keys))]
		objects := make([]types.ObjectIdentifier, len(batch))
		for i, key := range batch {
			objects[i] = types.ObjectIdentifier{Key: aws.String(s.Prefix + key)}
		}
		out, err := s.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.Bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
		if err != nil {
			return deleted, err
		}
		if len(out.Errors) > 0 {
			return deleted, fmt.Errorf("s3store: delete %s: %s", aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
		}
		deleted += len(batch)
	}
	remaining, err := s.List(ctx, "")
	if err != nil {
		return deleted, err
	}
	if len(remaining) != 0 {
		return deleted, fmt.Errorf("s3store: %d objects remain under %s after the wipe", len(remaining), s.Prefix)
	}
	return deleted, nil
}
```

### 5.3 Fault-injection wrapper (`service/faultstore`)

Rather than replacing SeaweedFS with an in-memory mock, `faultstore` wraps the real `s3store.Store` and fires one-shot rules (`fault:match:nth`) on puts and non-empty listings: `fail-before-put`, `fail-after-put`, `partial-put`, `crash-before-put`, `crash-after-put`, and `stale-list`.

<!-- include: service/faultstore/faultstore.go -->
```go
// Package faultstore wraps any walship.Store to inject one-shot faults around
// puts and listings while keeping the underlying real store (SeaweedFS S3 in
// experiments, DirStore in unit tests) in the loop.
//
// Each Rule matches calls whose key or list prefix contains Match and fires
// once on the Nth matching call (1-based). Put faults simulate dropped,
// ambiguous, partial, or crashing uploads; stale-list simulates delayed list
// visibility by hiding the lexicographically last key once.
package faultstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/internal/walship"
)

// ErrInjected is returned whenever a fault rule fires on PutIfAbsent.
var ErrInjected = errors.New("injected store fault")

// Fault names a failure mode that Store can inject on a matching call.
type Fault string

// Supported Fault values.
const (
	// FailBeforePut returns ErrInjected without calling the underlying store.
	FailBeforePut Fault = "fail-before-put"
	// FailAfterPut stores the full object and then returns ErrInjected,
	// modeling an ambiguous upload whose reply was lost.
	FailAfterPut Fault = "fail-after-put"
	// PartialPut stores only the first half of the bytes and returns
	// ErrInjected, stressing readers against non-atomic uploads.
	PartialPut Fault = "partial-put"
	// CrashBeforePut calls Store.Crash before calling the underlying store.
	CrashBeforePut Fault = "crash-before-put"
	// CrashAfterPut calls Store.Crash after the underlying PutIfAbsent succeeds.
	CrashAfterPut Fault = "crash-after-put"
	// StaleList omits the lexicographically last key from one non-empty List.
	StaleList Fault = "stale-list"
)

// Rule specifies one fault to fire on the Nth (1-based) call whose key or list
// prefix contains Match.
type Rule struct {
	Fault Fault
	Match string
	Nth   int
}

// String formats r in the "fault:match:nth" syntax accepted by ParseRule.
func (r Rule) String() string { return fmt.Sprintf("%s:%s:%d", r.Fault, r.Match, r.Nth) }

// ParseRule parses a "fault:match:nth" specification, requiring exactly three
// colon-separated fields, a known Fault name, and a positive integer nth.
func ParseRule(text string) (Rule, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 3 {
		return Rule{}, fmt.Errorf("fault rule %q: want fault:match:nth", text)
	}
	nth, err := strconv.Atoi(parts[2])
	if err != nil || nth < 1 {
		return Rule{}, fmt.Errorf("fault rule %q: nth must be a positive integer", text)
	}
	rule := Rule{Fault: Fault(parts[0]), Match: parts[1], Nth: nth}
	switch rule.Fault {
	case FailBeforePut, FailAfterPut, PartialPut, CrashBeforePut, CrashAfterPut, StaleList:
		return rule, nil
	}
	return Rule{}, fmt.Errorf("fault rule %q: unknown fault %q", text, parts[0])
}

// Store is a walship.Store wrapper that forwards operations to Base and fires
// matching Rules once. Crash defaults to os.Exit(3) and can be replaced in tests.
type Store struct {
	Base  walship.Store
	Rules []Rule
	Crash func()
	mu    sync.Mutex
	seen  map[int]int
	fired []string
}

// New wraps base with rules and installs the default os.Exit(3) Crash hook.
func New(base walship.Store, rules ...Rule) *Store {
	return &Store{Base: base, Rules: rules, Crash: func() { os.Exit(3) }, seen: map[int]int{}}
}

func (s *Store) match(isList bool, key string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rule := range s.Rules {
		if (rule.Fault == StaleList) != isList || !strings.Contains(key, rule.Match) {
			continue
		}
		s.seen[i]++
		if s.seen[i] == rule.Nth {
			s.fired = append(s.fired, fmt.Sprintf("%s on %s", rule.Fault, key))
			return rule.Fault, true
		}
	}
	return "", false
}

// Fired returns a copy of the log of rules that have fired so far.
func (s *Store) Fired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.fired...)
}

// PutIfAbsent forwards to Base unless a put fault rule matches key on its Nth
// occurrence, in which case it executes that fault mode and returns ErrInjected.
func (s *Store) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	fault, ok := s.match(false, key)
	if !ok {
		return s.Base.PutIfAbsent(ctx, key, data)
	}
	switch fault {
	case FailBeforePut:
		return fmt.Errorf("%w: %s before put of %s", ErrInjected, fault, key)
	case CrashBeforePut:
		s.Crash()
		return fmt.Errorf("%w: %s before put of %s", ErrInjected, fault, key)
	case PartialPut:
		if err := s.Base.PutIfAbsent(ctx, key, data[:len(data)/2]); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s stored %d of %d bytes of %s", ErrInjected, fault, len(data)/2, len(data), key)
	}
	if err := s.Base.PutIfAbsent(ctx, key, data); err != nil {
		return err
	}
	if fault == CrashAfterPut {
		s.Crash()
	}
	return fmt.Errorf("%w: %s after put of %s", ErrInjected, fault, key)
}

// Get forwards directly to Base.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) { return s.Base.Get(ctx, key) }

// Delete forwards directly to Base.
func (s *Store) Delete(ctx context.Context, key string) error { return s.Base.Delete(ctx, key) }

// List forwards to Base and, if a StaleList rule fires on a non-empty result,
// sorts the keys and drops the last (newest fixed-width) key once.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.Base.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	// An empty listing has nothing to hide, so do not consume a StaleList
	// rule's Nth count or record it as fired.
	if len(keys) == 0 {
		return keys, nil
	}
	if fault, ok := s.match(true, prefix); ok && fault == StaleList {
		sort.Strings(keys)
		keys = keys[:len(keys)-1]
	}
	return keys, nil
}
```

### 5.4 Store-backed fencing and fleet inspection (`service/fence`)

`fence` implements the single-writer slot chain (`dbs/<id>/log/<12-digit position>`). `Adapter` translates `walship`'s `snapshots/<p>` and `segments/<p>` keys to fenced slots, caches **only** slot metadata in RAM (never object bytes—fixed after adversarial attack `CAT-05b`), validates envelopes before uploading, and distinguishes retried identical puts (`nil`) from higher-epoch fencing (`FencedError`) and stale-listing contention (`walship.ErrExists`). `Inspect` reads one database's chain in read-only mode, stops at the first damaged slot or lineage break, and rebuilds every valid position `1..last` in memory so callers know the exact `LastGood` restore point.

<!-- include: service/fence/fence.go -->
```go
// Package fence owns the store-backed fencing scheme for single-writer
// ownership across nodes.
//
// Each database's stream is stored in fixed-width position slots under
// dbs/<id>/log/<12-digit position>. Every slot object begins with a one-line
// envelope naming the writer's epoch, object kind (snapshot or segment), and
// covered position range, followed by the unchanged walship object bytes.
//
// Fencing has two halves because an S3 store cannot read an epoch inside an
// object and compare it against another object:
//   - Store half (slot exclusivity): PutIfAbsent (If-None-Match: "*") ensures
//     each position key can be written at most once and never overwritten.
//   - Writer half (Adapter): a writer lists the chain, stops with FencedError
//     if any slot holds an epoch higher than its own, and appends only at
//     end+1 of the chain it validated.
//
// Both halves are necessary. Without slot exclusivity, a stale listing lets a
// writer overwrite an accepted slot. Without the writer rule, a restarted
// stale owner (such as raw walship.Open resuming at the store's global end)
// could append into a free slot after the new owner's fence and have
// If-None-Match accept it.
package fence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/internal/walship"
)

// Envelope constants identifying slot object kinds and the wire header tag.
const (
	// KindSnapshot marks a slot carrying a full database snapshot (and every
	// new epoch's opening fence object).
	KindSnapshot = "snapshot"
	// KindSegment marks a slot carrying a walship WAL segment.
	KindSegment = "segment"
	envelopeTag = "fenced-slot v1"
)

// Sentinel errors returned by slot decoding, lineage checks, and Adapter calls.
var (
	// ErrFenced is matched by FencedError via errors.Is when a newer epoch
	// occupies the next slot or appears anywhere in the chain.
	ErrFenced = errors.New("fenced")
	// ErrDamaged reports a malformed slot key, envelope line, or embedded
	// walship object, or a slot whose metadata changed after it was listed.
	ErrDamaged = errors.New("damaged slot")
	// ErrBadChain reports a slot sequence that breaks the single-lineage rules
	// checked by CheckLineage.
	ErrBadChain = errors.New("slot chain is not one lineage")
	// ErrOutOfOrder reports a PutIfAbsent whose first position is not end+1 of
	// the chain the adapter knows.
	ErrOutOfOrder = errors.New("put is not at the next position of this writer's chain")
)

// FencedError records that a writer with Epoch was stopped because a newer
// epoch (Holder) already owns Position in Database's slot chain.
type FencedError struct {
	Database string
	Epoch    uint64
	Holder   uint64
	Position int64
}

// Error formats e as a deterministic message suitable for logs and node replies.
func (e *FencedError) Error() string {
	return fmt.Sprintf("fenced: %s position %d is held by epoch %d, this writer has epoch %d", e.Database, e.Position, e.Holder, e.Epoch)
}

// Is allows callers to test for any FencedError with errors.Is(err, ErrFenced).
func (e *FencedError) Is(target error) bool { return target == ErrFenced }

// Slot is one decoded entry in a database's fenced log. Object holds the
// embedded walship snapshot or segment payload when loaded; cached Adapter
// entries clear Object and raw so open databases do not hold shipped bytes in RAM.
type Slot struct {
	Key    string
	Epoch  uint64
	Kind   string
	First  int64
	Last   int64
	Object []byte
	raw    []byte
}

func (s Slot) withoutBytes() Slot {
	s.Object, s.raw = nil, nil
	return s
}

// SlotPrefix returns the object-store key prefix under which database's slots live.
func SlotPrefix(database string) string { return "dbs/" + database + "/log/" }

// SlotKey returns the fixed-width key for position in database's slot chain.
// Twelve zero-padded digits keep lexicographic order identical to numeric order.
func SlotKey(database string, position int64) string {
	return fmt.Sprintf("%s%012d", SlotPrefix(database), position)
}

// Encode prepends the single-line fenced-slot v1 envelope to object.
func Encode(epoch uint64, kind string, first, last int64, object []byte) []byte {
	header := fmt.Sprintf("%s epoch=%d kind=%s first=%d last=%d\n", envelopeTag, epoch, kind, first, last)
	return append([]byte(header), object...)
}

// Decode parses and strictly validates a slot object against key. It requires
// the envelope line to re-encode identically (rejecting padding or extra
// fields), epoch >= 1, first matching key's position, last >= first, a valid
// database segment in key, and a decodable embedded walship snapshot or segment
// whose positions match the envelope.
func Decode(key string, data []byte) (Slot, error) {
	line, object, found := bytes.Cut(data, []byte("\n"))
	slot := Slot{Key: key, Object: object, raw: data}
	if !found {
		return slot, fmt.Errorf("%w: %s has no envelope line", ErrDamaged, key)
	}
	_, err := fmt.Sscanf(string(line), envelopeTag+" epoch=%d kind=%s first=%d last=%d", &slot.Epoch, &slot.Kind, &slot.First, &slot.Last)
	// Round-trip comparison rejects zero-padded integers, extra whitespace, or
	// trailing fields that fmt.Sscanf alone would silently ignore.
	if err != nil || string(Encode(slot.Epoch, slot.Kind, slot.First, slot.Last, nil)) != string(line)+"\n" {
		return slot, fmt.Errorf("%w: %s envelope %q", ErrDamaged, key, line)
	}
	if slot.Epoch == 0 || slot.First < 1 || slot.Last < slot.First || keyDatabase(key) == "" || key != SlotKey(keyDatabase(key), slot.First) {
		return slot, fmt.Errorf("%w: %s envelope %q does not fit its key", ErrDamaged, key, line)
	}
	switch slot.Kind {
	case KindSnapshot:
		meta, _, err := walship.DecodeSnapshot(object)
		if err != nil || meta.Position != slot.First || slot.Last != slot.First {
			return slot, fmt.Errorf("%w: %s snapshot object does not match its envelope: %v", ErrDamaged, key, err)
		}
	case KindSegment:
		meta, _, err := walship.DecodeSegment(object)
		if err != nil || meta.FirstPosition != slot.First || meta.LastPosition != slot.Last {
			return slot, fmt.Errorf("%w: %s segment object does not match its envelope: %v", ErrDamaged, key, err)
		}
	default:
		return slot, fmt.Errorf("%w: %s kind %q", ErrDamaged, key, slot.Kind)
	}
	return slot, nil
}

func keyDatabase(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "dbs" || parts[2] != "log" || parts[1] == "" || walship.ValidateKey("dbs/"+parts[1]) != nil {
		return ""
	}
	return parts[1]
}

// Chain lists, fetches, decodes, and lineage-checks every slot for database in
// base, sorted by key (and therefore by position).
func Chain(ctx context.Context, base walship.Store, database string) ([]Slot, error) {
	keys, err := base.List(ctx, SlotPrefix(database))
	if err != nil {
		return nil, err
	}
	// Sort explicitly because S3 listings are not guaranteed to arrive in byte order.
	sort.Strings(keys)
	var chain []Slot
	for _, key := range keys {
		data, err := base.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		slot, err := Decode(key, data)
		if err != nil {
			return nil, err
		}
		chain = append(chain, slot)
	}
	return chain, CheckLineage(chain)
}

// CheckLineage verifies that chain forms a single unbroken lineage: if
// non-empty, it begins with a snapshot at position 1, every subsequent slot
// begins at previous.Last+1, epochs never decrease, and every epoch increase
// begins with a fence snapshot.
func CheckLineage(chain []Slot) error {
	for i, slot := range chain {
		if i == 0 {
			if slot.First != 1 || slot.Kind != KindSnapshot {
				return fmt.Errorf("%w: the chain starts with a %s at position %d, want a snapshot at 1", ErrBadChain, slot.Kind, slot.First)
			}
			continue
		}
		previous := chain[i-1]
		switch {
		case slot.First != previous.Last+1:
			return fmt.Errorf("%w: position %d follows %d", ErrBadChain, slot.First, previous.Last)
		case slot.Epoch < previous.Epoch:
			return fmt.Errorf("%w: epoch %d at position %d after epoch %d", ErrBadChain, slot.Epoch, slot.First, previous.Epoch)
		case slot.Epoch > previous.Epoch && slot.Kind != KindSnapshot:
			return fmt.Errorf("%w: epoch %d starts at position %d with a %s, not a fence snapshot", ErrBadChain, slot.Epoch, slot.First, slot.Kind)
		}
	}
	return nil
}

// Adapter presents one database's fenced slot chain as a walship.Store for a
// writer operating at Epoch. It translates between walship's snapshots/<p> and
// segments/<p> keys and the store's dbs/<id>/log/<p> slots, enforcing the
// writer half of the fencing rule.
type Adapter struct {
	Base     walship.Store
	Database string
	Epoch    uint64
	mu       sync.Mutex
	slots    map[int64]Slot
	end      int64
	listed   bool
}

// New constructs an Adapter for database at epoch, rejecting invalid key
// segments and epoch 0 (which is not an ownership epoch).
func New(base walship.Store, database string, epoch uint64) (*Adapter, error) {
	if err := walship.ValidateKey("dbs/" + database); err != nil || strings.Contains(database, "/") {
		return nil, fmt.Errorf("fence: database id %q is not a valid key segment", database)
	}
	if epoch == 0 {
		return nil, errors.New("fence: epoch 0 is not an ownership epoch")
	}
	return &Adapter{Base: base, Database: database, Epoch: epoch, slots: map[int64]Slot{}}, nil
}

// Refresh reloads the database's slot chain from Base, fails with FencedError
// if any slot holds an epoch higher than a.Epoch, and returns the chain's last
// position.
func (a *Adapter) Refresh(ctx context.Context) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshLocked(ctx)
}

func (a *Adapter) refreshLocked(ctx context.Context) (int64, error) {
	chain, err := Chain(ctx, a.Base, a.Database)
	if err != nil {
		return 0, err
	}
	// Build the new metadata map in locals and replace a.slots only after the
	// entire chain passes the epoch check; otherwise a fenced Refresh would
	// leave a truncated pre-fence view cached as listed.
	slots, end := map[int64]Slot{}, int64(0)
	for _, slot := range chain {
		if slot.Epoch > a.Epoch {
			return 0, &FencedError{Database: a.Database, Epoch: a.Epoch, Holder: slot.Epoch, Position: slot.First}
		}
		// Keep metadata only so open databases do not retain shipped bytes in RAM.
		slots[slot.First] = slot.withoutBytes()
		end = slot.Last
	}
	a.slots, a.end, a.listed = slots, end, true
	return a.end, nil
}

// List returns walship-style snapshot and segment keys matching prefix from the
// adapter's cached listing, refreshing once from Base if not yet listed.
// Listing only once per Refresh lets a claim compute both its restore image and
// its fence position from a single consistent listing.
func (a *Adapter) List(ctx context.Context, prefix string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.listed {
		if _, err := a.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	var keys []string
	for _, slot := range a.slots {
		key := walship.SnapshotKey(slot.First)
		if slot.Kind == KindSegment {
			key = walship.SegmentKey(slot.First)
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Get fetches and decodes the slot for walship key from Base on demand,
// returning its embedded walship object bytes. If the slot was previously
// listed and its epoch or range no longer match, Get returns ErrDamaged.
func (a *Adapter) Get(ctx context.Context, key string) ([]byte, error) {
	kind, position, err := walship.ParseKey(key)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	known, ok := a.slots[position]
	a.mu.Unlock()
	if ok && known.Kind != kind {
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	}
	data, err := a.Base.Get(ctx, SlotKey(a.Database, position))
	if err != nil {
		return nil, err
	}
	slot, err := Decode(SlotKey(a.Database, position), data)
	if err != nil {
		return nil, err
	}
	if ok && (slot.Epoch != known.Epoch || slot.Last != known.Last) {
		return nil, fmt.Errorf("%w: %s changed after it was listed", ErrDamaged, slot.Key)
	}
	if slot.Kind != kind {
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	}
	return slot.Object, nil
}

// PutIfAbsent wraps data in a fenced-slot envelope for a.Epoch and writes it
// conditionally at end+1 of this adapter's known chain.
//
// It validates the envelope before sending anything so a malformed object never
// becomes an immutable slot. When Base reports ErrExists, PutIfAbsent reads the
// occupying slot to distinguish three cases: identical bytes (an SDK retry of
// this writer's own put, treated as success), a higher epoch (FencedError), or
// a lower/equal epoch from a stale listing (walship.ErrExists so the caller
// re-lists and retries).
func (a *Adapter) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	kind, first, err := walship.ParseKey(key)
	if err != nil {
		return err
	}
	last := first
	if kind == KindSegment {
		meta, _, err := walship.DecodeSegment(data)
		if err != nil {
			return err
		}
		last = meta.LastPosition
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.listed {
		if _, err := a.refreshLocked(ctx); err != nil {
			return err
		}
	}
	// Writer half of the fencing rule: append only directly after the last
	// position this adapter validated.
	if first != a.end+1 {
		return fmt.Errorf("%w: %s at position %d, the chain this writer knows ends at %d", ErrOutOfOrder, a.Database, first, a.end)
	}
	slotKey := SlotKey(a.Database, first)
	envelope := Encode(a.Epoch, kind, first, last, data)
	// Decode before uploading so malformed objects are rejected locally rather
	// than written into an immutable slot.
	slot, err := Decode(slotKey, envelope)
	if err != nil {
		return err
	}
	if err := a.Base.PutIfAbsent(ctx, slotKey, envelope); err != nil {
		if !errors.Is(err, walship.ErrExists) {
			return err
		}
		stored, getErr := a.Base.Get(ctx, slotKey)
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		holder, decodeErr := Decode(slotKey, stored)
		switch {
		case decodeErr != nil:
			return errors.Join(err, decodeErr)
		case bytes.Equal(stored, envelope):
			// Identical bytes mean an earlier attempt of this same put already
			// landed (for example, before an SDK retry).
		case holder.Epoch > a.Epoch:
			return &FencedError{Database: a.Database, Epoch: a.Epoch, Holder: holder.Epoch, Position: first}
		default:
			return fmt.Errorf("%w: %s position %d is held by epoch %d", walship.ErrExists, a.Database, first, holder.Epoch)
		}
	}
	a.slots[first], a.end = slot.withoutBytes(), last
	return nil
}

// Audit loads and lineage-checks database's full slot chain and returns both
// the slots and the walship.Catalog built over them using a read-only adapter
// at epoch MaxUint64 (which is never fenced and never writes).
func Audit(ctx context.Context, base walship.Store, database string) ([]Slot, *walship.Catalog, error) {
	chain, err := Chain(ctx, base, database)
	if err != nil {
		return nil, nil, err
	}
	auditor, err := New(base, database, math.MaxUint64)
	if err != nil {
		return nil, nil, err
	}
	restorable, err := walship.LoadCatalog(ctx, auditor)
	return chain, restorable, err
}

// RestoreAt plans and executes a walship restore of restorable to position,
// returning the rebuilt database file bytes.
func RestoreAt(restorable *walship.Catalog, position int64) ([]byte, error) {
	plan, err := restorable.Plan(position)
	if err != nil {
		return nil, err
	}
	image, _, err := plan.Execute()
	return image, err
}

// Delete always refuses because fenced log slots are write-once and never
// deleted by a writer.
func (a *Adapter) Delete(ctx context.Context, key string) error {
	return fmt.Errorf("fence: %s: slots are never deleted by a writer", key)
}
```

<!-- include: service/fence/inspect.go -->
```go
package fence

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/walship"
)

// Inspection summarizes a read-only audit of one database's slot chain in the
// store, counting valid restore points and naming LastGood when a gap, damaged
// object, or rejected state stops recovery early.
type Inspection struct {
	Database      string
	Slots         int
	BytesRead     int64
	RestorePoints int
	LastGood      int64
	Problem       string
}

// String formats i as either a healthy slot/restore-point summary or a
// diagnostic naming the last good restore point and the first failure past it.
func (i Inspection) String() string {
	if i.Problem == "" {
		return fmt.Sprintf("%s: %d slots, %d restore points, all rebuild", i.Database, i.Slots, i.RestorePoints)
	}
	return fmt.Sprintf("%s: unrecoverable past position %d (last good restore point %d): %s", i.Database, i.LastGood, i.LastGood, i.Problem)
}

// prefixStore exposes the decoded healthy prefix of one database's slots in
// memory so walship.LoadCatalog can plan and execute restores without touching
// damaged trailing slots or re-reading objects from S3.
type prefixStore struct {
	objects map[string][]byte
}

// PutIfAbsent always refuses because prefixStore is read-only during inspection.
func (p *prefixStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	return fmt.Errorf("fence: inspection is read-only")
}

// Get returns the validated slot payload stored under key, or walship.ErrNotFound.
func (p *prefixStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, ok := p.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	}
	return data, nil
}

// List returns the sorted walship keys matching prefix from the healthy prefix.
func (p *prefixStore) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for key := range p.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete always refuses because prefixStore is read-only during inspection.
func (p *prefixStore) Delete(ctx context.Context, key string) error {
	return fmt.Errorf("fence: inspection is read-only")
}

// Inspect lists and reads database's slots from base, keeps the longest valid
// prefix that decodes and passes CheckLineage, rebuilds every position 1..last
// in memory, and calls visit on each rebuilt image. Any gap, corrupt object,
// rebuild error, or visit rejection stops progress and is recorded in
// Inspection.Problem while LastGood names the highest valid restore point.
func Inspect(ctx context.Context, base walship.Store, database string, visit func(position int64, image []byte) error) (Inspection, error) {
	inspection := Inspection{Database: database}
	keys, err := base.List(ctx, SlotPrefix(database))
	if err != nil {
		return inspection, err
	}
	sort.Strings(keys)
	inspection.Slots = len(keys)
	prefix := &prefixStore{objects: map[string][]byte{}}
	var good []Slot
	for _, key := range keys {
		data, err := base.Get(ctx, key)
		if err != nil {
			return inspection, err
		}
		inspection.BytesRead += int64(len(data))
		slot, err := Decode(key, data)
		if err == nil {
			// Check lineage incrementally so a gap or bad epoch stops the valid
			// prefix right before the broken slot while still letting the loop
			// below verify every earlier restore point.
			err = CheckLineage(append(good[:len(good):len(good)], slot))
		}
		if err != nil {
			inspection.Problem = err.Error()
			break
		}
		good = append(good, slot)
		walshipKey := walship.SnapshotKey(slot.First)
		if slot.Kind == KindSegment {
			walshipKey = walship.SegmentKey(slot.First)
		}
		prefix.objects[walshipKey] = slot.Object
	}
	if inspection.Problem == "" && len(keys) == 0 {
		inspection.Problem = "the store holds no slot"
	}
	restorable, err := walship.LoadCatalog(ctx, prefix)
	if err != nil {
		return inspection, err
	}
	var last int64
	if len(good) > 0 {
		last = good[len(good)-1].Last
	}
	for position := int64(1); position <= last; position++ {
		image, err := RestoreAt(restorable, position)
		if err == nil && visit != nil {
			err = visit(position, image)
		}
		if err != nil {
			if inspection.Problem == "" {
				inspection.Problem = fmt.Sprintf("position %d does not rebuild: %v", position, err)
			} else {
				inspection.Problem = fmt.Sprintf("position %d does not rebuild: %v; then %s", position, err, inspection.Problem)
			}
			break
		}
		inspection.RestorePoints++
		inspection.LastGood = position
	}
	return inspection, nil
}
```

### 5.5 Control-plane catalog (`service/catalog`)

`catalog` stores the `databases` table (ownership, epoch, location, last shipped position, `schema_version`, validated per-connection settings JSON), bulk helpers (`RegisterAll`, `All`), and the `relocations` table (`BeginRelocation`, `GetRelocation`, `AdvanceRelocation`, `FinishRelocation`) in one WAL-mode SQLite file shared by all node processes on the host.

<!-- include: service/catalog/catalog.go -->
```go
// Package catalog owns the platform's control-plane metadata in a single
// WAL-mode SQLite database shared by all nodes on the host.
//
// Its databases table maps each database id to its current owner node,
// monotonically increasing ownership epoch, local file location, last shipped
// stream position, schema version (the recipe's user_version), and
// per-connection SQLite settings (from Chapter 8). Its relocations table
// (relocation.go) stores the single durable record of an in-flight or finished
// relocation so a fresh coordinator can resume or roll back after a crash. A
// Claim increments the epoch atomically in one SQLite write transaction so
// concurrent claims receive distinct epochs. Because SQLite locks work only
// within one host (src/os_unix.c:1189-1342), a multi-host platform would
// replace this file with a networked control-plane service exposing the same
// operations.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Sentinel errors returned by Catalog lookups and epoch-checked updates.
var (
	// ErrUnknown reports that no row exists for the requested database id.
	ErrUnknown = errors.New("catalog: unknown database")
	// ErrStaleEpoch reports that RecordShipped targeted an epoch that is no
	// longer current in the catalog row.
	ErrStaleEpoch = errors.New("catalog: the epoch is no longer current")
)

const schema = `CREATE TABLE IF NOT EXISTS databases (
	id TEXT PRIMARY KEY,
	owner TEXT NOT NULL DEFAULT '',
	epoch INTEGER NOT NULL DEFAULT 0,
	location TEXT NOT NULL DEFAULT '',
	last_shipped INTEGER NOT NULL DEFAULT 0,
	schema_version INTEGER NOT NULL DEFAULT 0,
	settings TEXT NOT NULL
) STRICT`

// Record is one database's control-plane row in the catalog.
type Record struct {
	ID            string
	Owner         string
	Epoch         uint64
	Location      string
	LastShipped   int64
	SchemaVersion int
	Settings      sqlitex.ConnSettings
}

// Catalog wraps the shared SQLite handle that stores the databases table.
type Catalog struct {
	DB *sqlitex.DB
}

// Options returns the SQLite settings used for the catalog database: WAL mode,
// synchronous=FULL, 5-second busy timeout (from sqlitex.DefaultOptions), a
// single pooled connection, and a 256 KiB page cache.
func Options() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.CacheSize = -256
	return options
}

// Open opens or creates the catalog SQLite database at path and ensures the
// databases table exists.
func Open(ctx context.Context, path string) (*Catalog, error) {
	db, err := sqlitex.Open(ctx, path, Options())
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Catalog{DB: db}, nil
}

// Close closes the underlying catalog database connection.
func (c *Catalog) Close() error { return c.DB.Close() }

// Register inserts a database row with validated per-connection settings if id
// is not already present; existing rows are left unchanged.
func (c *Catalog) Register(ctx context.Context, id string, settings sqlitex.ConnSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = c.DB.ExecContext(ctx, "INSERT INTO databases (id, settings) VALUES (?, ?) ON CONFLICT (id) DO NOTHING", id, string(encoded))
	return err
}

// Get loads the catalog Record for id, strictly decoding and validating its
// stored per-connection settings JSON.
func (c *Catalog) Get(ctx context.Context, id string) (Record, error) {
	record := Record{ID: id}
	var settings string
	err := c.DB.QueryRowContext(ctx, "SELECT owner, epoch, location, last_shipped, schema_version, settings FROM databases WHERE id = ?", id).
		Scan(&record.Owner, &record.Epoch, &record.Location, &record.LastShipped, &record.SchemaVersion, &settings)
	if errors.Is(err, sql.ErrNoRows) {
		return record, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(strings.NewReader(settings))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record.Settings); err != nil {
		return record, fmt.Errorf("catalog: settings of %s: %w", id, err)
	}
	return record, record.Settings.Validate()
}

// Claim assigns ownership of id to node, increments the epoch atomically,
// clears location until the new owner fences the store, and returns the new
// epoch. Claim is a takeover: it does not contact the previous owner.
func (c *Catalog) Claim(ctx context.Context, id, node string) (uint64, error) {
	var epoch uint64
	err := c.DB.QueryRowContext(ctx, "UPDATE databases SET owner = ?, epoch = epoch + 1, location = '' WHERE id = ? RETURNING epoch", node, id).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	return epoch, err
}

// RecordShipped updates last_shipped, schema_version, and location only if the
// catalog row still holds epoch; otherwise it returns ErrStaleEpoch. Callers
// ignore ErrStaleEpoch after a store-accepted put because the store, not the
// catalog, is the authority on whether a write was accepted into the lineage.
func (c *Catalog) RecordShipped(ctx context.Context, id string, epoch uint64, position int64, schemaVersion int, location string) error {
	result, err := c.DB.ExecContext(ctx, "UPDATE databases SET last_shipped = ?, schema_version = ?, location = ? WHERE id = ? AND epoch = ?",
		position, schemaVersion, location, id, epoch)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s epoch %d", ErrStaleEpoch, id, epoch)
	}
	return nil
}
```

<!-- include: service/catalog/bulk.go -->
```go
package catalog

import (
	"context"
	"encoding/json"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// RegisterAll validates settings once and inserts every id into databases in a
// single SQLite transaction, leaving any existing rows unchanged.
func (c *Catalog) RegisterAll(ctx context.Context, ids []string, settings sqlitex.ConnSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO databases (id, settings) VALUES (?, ?) ON CONFLICT (id) DO NOTHING", id, string(encoded)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// All returns every registered database Record ordered by id, validating each
// row's stored settings JSON through Get.
func (c *Catalog) All(ctx context.Context) ([]Record, error) {
	rows, err := c.DB.QueryContext(ctx, "SELECT id FROM databases ORDER BY id")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(ids))
	for _, id := range ids {
		record, err := c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}
```

<!-- include: service/catalog/relocation.go -->
```go
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

// Relocation lifecycle states stored in the relocations table.
const (
	// RelocationRunning marks a relocation that is still in progress and blocks
	// a second concurrent relocation of the same database.
	RelocationRunning = "running"
	// RelocationDone marks a relocation whose destination fenced and activated
	// and whose source retired cleanly.
	RelocationDone = "done"
	// RelocationRolledBack marks a relocation cancelled before the destination
	// fenced the store, returning ownership to the source at a higher epoch.
	RelocationRolledBack = "rolled-back"
	// RelocationSuperseded marks a relocation overtaken by a newer epoch in the
	// store (for example, an explicit takeover by another node).
	RelocationSuperseded = "superseded"
)

// Sentinel errors returned by relocation state-machine transitions in the catalog.
var (
	// ErrNotTheOwner reports that BeginRelocation's source is not the database's
	// current catalog owner (or that the database has never been claimed).
	ErrNotTheOwner = errors.New("catalog: the database is not owned by that node")
	// ErrRelocationRunning reports that another relocation row for the database
	// is already in the running state.
	ErrRelocationRunning = errors.New("catalog: a relocation of the database is already running")
	// ErrNoRelocation reports that no matching relocation row exists (or that
	// FinishRelocation targeted a row that is no longer running).
	ErrNoRelocation = errors.New("catalog: no running relocation of the database")
	// ErrRelocationStepGoes reports that AdvanceRelocation tried to jump more
	// than one step ahead of the recorded step, or targeted a non-running row.
	ErrRelocationStepGoes = errors.New("catalog: a relocation can only record a step at most one past its last")
	sha256Hex             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const relocationSchema = `CREATE TABLE IF NOT EXISTS relocations (
	id TEXT PRIMARY KEY,
	source TEXT NOT NULL,
	destination TEXT NOT NULL,
	from_epoch INTEGER NOT NULL,
	to_epoch INTEGER NOT NULL,
	step INTEGER NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	sha256 TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL
) STRICT`

// Relocation is the coordinator's sole durable record of a database move from
// Source (FromEpoch) to Destination (ToEpoch), tracking the last completed
// step, the drained fence-point Position and SHA256, and State.
type Relocation struct {
	ID          string
	Source      string
	Destination string
	FromEpoch   uint64
	ToEpoch     uint64
	Step        int
	Position    int64
	SHA256      string
	State       string
}

// String formats r as a single-line summary for error messages and logs.
func (r Relocation) String() string {
	return fmt.Sprintf("relocation of %s from %s (epoch %d) to %s (epoch %d): step %d, position %d, state %s",
		r.ID, r.Source, r.FromEpoch, r.Destination, r.ToEpoch, r.Step, r.Position, r.State)
}

func (c *Catalog) ensureRelocations(ctx context.Context) error {
	_, err := c.DB.ExecContext(ctx, relocationSchema)
	return err
}

// BeginRelocation executes relocation step 1 atomically in one SQLite
// transaction: it verifies source currently owns id with no running relocation,
// reassigns databases.owner to destination while bumping epoch by 1 and
// clearing location, and writes the running relocations row at step 1.
func (c *Catalog) BeginRelocation(ctx context.Context, id, source, destination string) (Relocation, error) {
	if source == destination || source == "" || destination == "" {
		return Relocation{}, fmt.Errorf("catalog: relocate %s from %q to %q", id, source, destination)
	}
	if err := c.ensureRelocations(ctx); err != nil {
		return Relocation{}, err
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return Relocation{}, err
	}
	defer tx.Rollback()
	var owner string
	var epoch uint64
	err = tx.QueryRowContext(ctx, "SELECT owner, epoch FROM databases WHERE id = ?", id).Scan(&owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Relocation{}, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if err != nil {
		return Relocation{}, err
	}
	if owner != source || epoch == 0 {
		return Relocation{}, fmt.Errorf("%w: %s is owned by %q at epoch %d, not by %q", ErrNotTheOwner, id, owner, epoch, source)
	}
	var running int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM relocations WHERE id = ? AND state = ?", id, RelocationRunning).Scan(&running); err != nil {
		return Relocation{}, err
	}
	if running != 0 {
		return Relocation{}, fmt.Errorf("%w: %s", ErrRelocationRunning, id)
	}
	// Updating databases and inserting the relocations row in the same
	// transaction guarantees a crash cannot bump the epoch without leaving a
	// relocation record for Resume or Rollback to recover from.
	if _, err := tx.ExecContext(ctx, "UPDATE databases SET owner = ?, epoch = epoch + 1, location = '' WHERE id = ?", destination, id); err != nil {
		return Relocation{}, err
	}
	r := Relocation{ID: id, Source: source, Destination: destination, FromEpoch: epoch, ToEpoch: epoch + 1, Step: 1, State: RelocationRunning}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO relocations (id, source, destination, from_epoch, to_epoch, step, position, sha256, state) VALUES (?, ?, ?, ?, ?, 1, 0, '', ?)",
		id, source, destination, r.FromEpoch, r.ToEpoch, RelocationRunning); err != nil {
		return Relocation{}, err
	}
	return r, tx.Commit()
}

// GetRelocation returns the stored Relocation row for id, or ErrNoRelocation if
// none exists.
func (c *Catalog) GetRelocation(ctx context.Context, id string) (Relocation, error) {
	if err := c.ensureRelocations(ctx); err != nil {
		return Relocation{}, err
	}
	r := Relocation{ID: id}
	err := c.DB.QueryRowContext(ctx, "SELECT source, destination, from_epoch, to_epoch, step, position, sha256, state FROM relocations WHERE id = ?", id).
		Scan(&r.Source, &r.Destination, &r.FromEpoch, &r.ToEpoch, &r.Step, &r.Position, &r.SHA256, &r.State)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: %s", ErrNoRelocation, id)
	}
	return r, err
}

// AdvanceRelocation records progress on a running relocation. It permits
// re-recording an earlier step idempotently during Resume (keeping max(step, ?))
// and forbids skipping ahead more than one step past the last recorded step.
// Non-zero position and non-empty 64-hex digest overwrite the fence-point fields.
func (c *Catalog) AdvanceRelocation(ctx context.Context, id string, step int, position int64, digest string) error {
	if position < 0 || (digest != "" && !sha256Hex.MatchString(digest)) {
		return fmt.Errorf("catalog: relocation of %s: position %d, sha256 %q", id, position, digest)
	}
	if err := c.ensureRelocations(ctx); err != nil {
		return err
	}
	// Allowing step <= recorded step keeps replayed steps idempotent on Resume
	// while preventing out-of-order jumps past step+1.
	result, err := c.DB.ExecContext(ctx, `UPDATE relocations SET step = max(step, ?),
		position = CASE WHEN ? > 0 THEN ? ELSE position END,
		sha256 = CASE WHEN ? <> '' THEN ? ELSE sha256 END
		WHERE id = ? AND state = ? AND ? BETWEEN 1 AND step + 1`,
		step, position, position, digest, digest, id, RelocationRunning, step)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s to step %d", ErrRelocationStepGoes, id, step)
	}
	return nil
}

// FinishRelocation transitions a running relocation row to one of the three
// terminal states (RelocationDone, RelocationRolledBack, RelocationSuperseded).
func (c *Catalog) FinishRelocation(ctx context.Context, id, state string) error {
	if state != RelocationDone && state != RelocationRolledBack && state != RelocationSuperseded {
		return fmt.Errorf("catalog: %q is not a final relocation state", state)
	}
	if err := c.ensureRelocations(ctx); err != nil {
		return err
	}
	result, err := c.DB.ExecContext(ctx, "UPDATE relocations SET state = ? WHERE id = ? AND state = ?", state, id, RelocationRunning)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s", ErrNoRelocation, id)
	}
	return nil
}
```

### 5.6 Standing resource bounds (`service/bounds`)

`bounds` turns Chapter 8's descriptor and memory formulas into a standing assertion checked after every command of every node process: descriptor growth `<= 3 * 3 * K + 32` and RSS growth `<= 3K * (2 * cache + 256 KiB) + 32 MiB`.

<!-- include: service/bounds/bounds.go -->
```go
// Package bounds owns the standing resource-bound assertion run inside every
// capstone node process after every command.
//
// It reuses the per-connection descriptor and memory formulas established in
// Chapter 8 (Experiment 8.3). A service-held database keeps 3 connections open
// (the writer plus walship's control and pin connections), which actually open
// 7 descriptors per database (3 .db + 3 -wal + 1 process-shared -shm through
// unixInodeInfo, src/os_unix.c:1323-1342); DescriptorLimit uses 3 descriptors
// per connection as a conservative upper bound.
package bounds

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/nickstrad/walfleet/internal/procstat"
)

// Per-connection upper-bound constants carried forward from Chapters 4 and 8.
const (
	// DescriptorsPerConnection is the per-connection WAL descriptor upper bound
	// measured in Experiment 8.2.
	DescriptorsPerConnection = 3
	// UncountedPerConnection is the per-connection schema/statement/overhead
	// allowance from Experiment 8.2.
	UncountedPerConnection = 256 * 1024
	// MemoryConstant is the fixed Go-runtime and process-baseline margin from
	// Experiment 8.3.
	MemoryConstant = 32 << 20
)

// ErrExceeded is returned by Assertion.Check when either open descriptors or
// RSS growth above the startup baseline exceeds Bounds.
var ErrExceeded = errors.New("bounds exceeded")

// Bounds holds the capacity parameters used to derive descriptor and RSS limits.
type Bounds struct {
	OpenDatabases          int
	ConnectionsPerDatabase int
	CacheBytes             int64
	ReserveDescriptors     int
}

// DescriptorLimit returns the maximum descriptor growth above baseline:
// DescriptorsPerConnection * ConnectionsPerDatabase * OpenDatabases + ReserveDescriptors.
func (b Bounds) DescriptorLimit() int {
	return DescriptorsPerConnection*b.ConnectionsPerDatabase*b.OpenDatabases + b.ReserveDescriptors
}

// MemoryLimit returns the maximum RSS growth above baseline in bytes:
// totalConnections * (2*CacheBytes + UncountedPerConnection) + MemoryConstant,
// where the factor of 2 covers SQLite's page-cache allocator overhead (Chapter 4).
func (b Bounds) MemoryLimit() int64 {
	connections := int64(b.OpenDatabases * b.ConnectionsPerDatabase)
	return connections*(2*b.CacheBytes+UncountedPerConnection) + MemoryConstant
}

// CacheBytes converts SQLite's PRAGMA cache_size value to bytes: negative
// values specify kibibytes, non-negative values specify page counts.
func CacheBytes(cacheSize int, pageSize int) int64 {
	if cacheSize < 0 {
		return int64(-cacheSize) * 1024
	}
	return int64(cacheSize) * int64(pageSize)
}

// Sample holds one measurement of open descriptors and RSS growth above baseline.
type Sample struct {
	Descriptors  int
	MemoryGrowth int64
}

// Assertion tracks a process's descriptor and RSS baseline, peak samples, and
// check count against Bounds.
type Assertion struct {
	Bounds  Bounds
	baseFDs int
	baseRSS int64
	Peak    Sample
	Checks  int
}

// Start records the current descriptor count and GC-settled RSS as the
// baseline for subsequent Check calls.
func Start(b Bounds) (*Assertion, error) {
	fds, err := procstat.DescriptorCount()
	if err != nil {
		return nil, err
	}
	rss, err := settledRSS()
	if err != nil {
		return nil, err
	}
	return &Assertion{Bounds: b, baseFDs: fds, baseRSS: rss}, nil
}

func settledRSS() (int64, error) {
	runtime.GC()
	debug.FreeOSMemory()
	memory, err := procstat.ReadMemory()
	return memory.RSS, err
}

// Check samples current descriptors and RSS relative to the baseline, updates
// Peak and Checks, and returns ErrExceeded if either limit is crossed.
func (a *Assertion) Check() (Sample, error) {
	fds, err := procstat.DescriptorCount()
	if err != nil {
		return Sample{}, err
	}
	memory, err := procstat.ReadMemory()
	if err != nil {
		return Sample{}, err
	}
	sample := Sample{Descriptors: fds - a.baseFDs, MemoryGrowth: memory.RSS - a.baseRSS}
	a.Checks++
	a.Peak.Descriptors = max(a.Peak.Descriptors, sample.Descriptors)
	a.Peak.MemoryGrowth = max(a.Peak.MemoryGrowth, sample.MemoryGrowth)
	if sample.Descriptors > a.Bounds.DescriptorLimit() || sample.MemoryGrowth > a.Bounds.MemoryLimit() {
		return sample, fmt.Errorf("%w: %d descriptors above the baseline (limit %d), memory growth %d bytes (limit %d)",
			ErrExceeded, sample.Descriptors, a.Bounds.DescriptorLimit(), sample.MemoryGrowth, a.Bounds.MemoryLimit())
	}
	return sample, nil
}

// Summary formats a stable one-line description of a's check count and limits
// for experiment output.
func (a *Assertion) Summary() string {
	return fmt.Sprintf("standing assertion: %d checks, descriptors above baseline <= %d (%d x %d connections x %d open databases + %d reserve), memory growth <= %d MiB",
		a.Checks, a.Bounds.DescriptorLimit(), DescriptorsPerConnection, a.Bounds.ConnectionsPerDatabase, a.Bounds.OpenDatabases, a.Bounds.ReserveDescriptors, a.Bounds.MemoryLimit()>>20)
}
```

### 5.7 Finite-state ownership model (`service/model`)

`model` encodes the exact fencing protocol—two nodes, catalog epoch counter up to `3`, write-once store slots, local commits up to stream position `5`, volatile runtime state versus persisted disk state (`Belief` and `Local`), stale listings, crashes, and restarts—plus two broken mutants (`store-accepts-every-put` and `resume-from-global-end`). `Explore` runs memoized exhaustive depth-first counting of all interleavings up to `depth`, breadth-first search for the shortest counterexample, and seeded random walks past the exhaustive bound.

<!-- include: service/model/model.go -->
```go
// Package model owns the finite-state ownership model checked by Experiment 10.1.
//
// It models two nodes, one database, the catalog's monotonic epoch counter,
// and an object store of position slots written by put-if-absent. Each node
// has volatile runtime state (role, epoch, shipped position) and persisted
// disk state (ownership belief and local commit position) that survives Crash.
//
// Explore checks three safety invariants across every interleaving up to a
// bounded depth and across seeded random walks: no two epochs are accepted for
// overlapping positions, no older epoch is accepted after a newer epoch was
// accepted, and no accepted slot is overwritten. Two Mutants disable each half
// of the fencing rule in turn (StoreAcceptsEveryPut removes slot exclusivity;
// ResumeFromGlobalEnd removes the writer's epoch check on restart, matching
// unfenced walship.Open) to verify that the checker catches violations.
package model

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// Mutant selects either the real fencing protocol (Correct) or one of two
// intentionally broken variants used as checker self-tests.
type Mutant string

// Supported Mutant variants.
const (
	// Correct models both halves of the fencing rule: write-once slots plus
	// writers that append only after their own last object and stop on newer epochs.
	Correct Mutant = "correct"
	// StoreAcceptsEveryPut removes put-if-absent exclusivity so puts overwrite
	// occupied slots, breaking the store half of fencing.
	StoreAcceptsEveryPut Mutant = "store-accepts-every-put"
	// ResumeFromGlobalEnd lets a restarted node resume at the store's global end
	// regardless of epoch (as unfenced walship.Open does), breaking the writer
	// half of fencing.
	ResumeFromGlobalEnd Mutant = "resume-from-global-end"
)

// ParseMutant validates name against the three known Mutant values.
func ParseMutant(name string) (Mutant, error) {
	switch m := Mutant(name); m {
	case Correct, StoreAcceptsEveryPut, ResumeFromGlobalEnd:
		return m, nil
	}
	return "", fmt.Errorf("model: unknown mutant %q (want %s, %s or %s)", name, Correct, StoreAcceptsEveryPut, ResumeFromGlobalEnd)
}

// State-space bounds that keep exhaustive exploration finite and fast.
const (
	// Nodes is the number of competing service nodes (A and B).
	Nodes = 2
	// MaxLocal caps a node's Local stream position across all epochs and fences
	// (a fence sets Local to the fence position, so this is a stream-position
	// cap rather than a per-node commit count).
	MaxLocal = 5
	// MaxEpoch is the highest catalog epoch that Claim may allocate.
	MaxEpoch    = 3
	slotCount   = MaxLocal + MaxEpoch + 2
	maxAccepted = 16
)

// Kind identifies one node transition in the ownership state machine.
type Kind uint8

// Action kinds modeled per node.
const (
	// Claim increments the catalog epoch and moves an idle node to Claiming.
	Claim Kind = iota
	// Fence lists the full slot chain and attempts a fence snapshot put at end+1.
	Fence
	// FenceStale lists the chain without its newest slot (modeling delayed list
	// visibility) before attempting the fence snapshot put.
	FenceStale
	// Write advances the owning node's local commit position (a local-durable commit).
	Write
	// Ship puts positions Shipped+1..Local into the store (store-durable commits).
	Ship
	// Crash drops the node's volatile state while keeping Belief and Local on disk.
	Crash
	// Restart brings a crashed node back up and lists the full slot chain.
	Restart
	// RestartStale brings a crashed node back up against a stale listing.
	RestartStale
	kindCount
)

var kindNames = [kindCount]string{"claim", "fence", "fence-with-stale-list", "write", "ship", "crash", "restart", "restart-with-stale-list"}

// String returns the canonical hyphenated name of k.
func (k Kind) String() string { return kindNames[k] }

// Action pairs a Node index (0 for A, 1 for B) with a transition Kind.
type Action struct {
	Node uint8
	Kind Kind
}

// String formats a as "<letter> <kind>", for example "A claim".
func (a Action) String() string { return fmt.Sprintf("%c %s", 'A'+a.Node, a.Kind) }

// Role is a node's volatile ownership lifecycle state while Up is true.
type Role uint8

// Possible Role values for an up node.
const (
	// Idle means the node does not currently claim or own the database.
	Idle Role = iota
	// Claiming means the node bumped the catalog epoch and has not fenced yet.
	Claiming
	// Owner means the node believes it holds the database at Epoch.
	Owner
	// Fenced means the node observed a higher epoch in the store and stepped down.
	Fenced
	// Stuck means a Ship collided with a lower or equal epoch (not fenced, cannot advance).
	Stuck
)

// Object is one slot entry stored in State.Slots and recorded in State.Accepted.
type Object struct {
	Epoch    uint8
	First    uint8
	Last     uint8
	Snapshot bool
}

// NodeState holds one node's volatile state (Up, Role, Epoch, Shipped) and
// persisted disk state (Belief, Local).
type NodeState struct {
	Up      bool
	Role    Role
	Epoch   uint8
	Belief  uint8
	Local   uint8
	Shipped uint8
}

// State is the complete value-comparable state of the model: catalog epoch,
// store slots, both nodes, and the ordered history of accepted puts. Keeping
// Accepted inside State makes memoization on (State, depth) sound for history-
// dependent invariants.
type State struct {
	CatalogEpoch uint8
	Slots        [slotCount]Object
	Nodes        [Nodes]NodeState
	Accepted     [maxAccepted]Object
	NAccepted    uint8
}

// Initial returns the starting State with both nodes up and idle and an empty store.
func Initial() State {
	var s State
	for i := range s.Nodes {
		s.Nodes[i].Up = true
	}
	return s
}

func (s *State) chain(stale bool) []Object {
	var objects []Object
	for p := 1; p < slotCount && s.Slots[p].Epoch != 0; p = int(s.Slots[p].Last) + 1 {
		objects = append(objects, s.Slots[p])
		if s.Slots[p].Last < s.Slots[p].First {
			break
		}
	}
	if stale && len(objects) > 0 {
		objects = objects[:len(objects)-1]
	}
	return objects
}

func end(objects []Object) uint8 {
	if len(objects) == 0 {
		return 0
	}
	return objects[len(objects)-1].Last
}

func newerThan(objects []Object, epoch uint8) bool {
	for _, o := range objects {
		if o.Epoch > epoch {
			return true
		}
	}
	return false
}

func (s *State) put(mutant Mutant, o Object) (accepted, inBounds bool) {
	if int(o.Last) >= slotCount || s.NAccepted == maxAccepted {
		return false, false
	}
	if s.Slots[o.First].Epoch != 0 && mutant != StoreAcceptsEveryPut {
		return false, true
	}
	s.Slots[o.First] = o
	s.Accepted[s.NAccepted] = o
	s.NAccepted++
	return true, true
}

// Step applies action a to state s under mutant. It returns the next State, a
// short outcome description, and whether a was enabled in s.
func Step(s State, mutant Mutant, a Action) (State, string, bool) {
	n := &s.Nodes[a.Node]
	switch a.Kind {
	case Claim:
		if !n.Up || n.Role == Claiming || n.Role == Owner || s.CatalogEpoch == MaxEpoch {
			return s, "", false
		}
		s.CatalogEpoch++
		n.Epoch, n.Role = s.CatalogEpoch, Claiming
		return s, fmt.Sprintf("ok: catalog epoch %d", n.Epoch), true
	case Fence, FenceStale:
		if !n.Up || n.Role != Claiming || (a.Kind == FenceStale && s.Slots[1].Epoch == 0) {
			return s, "", false
		}
		view := s.chain(a.Kind == FenceStale)
		if newerThan(view, n.Epoch) {
			n.Role = Idle
			return s, "refused: a newer epoch is in the store", true
		}
		at := end(view) + 1
		accepted, inBounds := s.put(mutant, Object{Epoch: n.Epoch, First: at, Last: at, Snapshot: true})
		switch {
		case !inBounds:
			return s, "", false
		case accepted:
			// Belief is persisted only after the fence snapshot lands in the store,
			// matching Node.Claim writing <id>.owner after Adapter.PutIfAbsent.
			n.Role, n.Belief, n.Local, n.Shipped = Owner, n.Epoch, at, at
			return s, fmt.Sprintf("ok: fence at %d", at), true
		case s.Slots[at].Epoch > n.Epoch:
			n.Role = Idle
			return s, fmt.Sprintf("refused: position %d holds epoch %d", at, s.Slots[at].Epoch), true
		}
		// A lower or equal epoch occupies the slot (stale listing or concurrent
		// ship before the fence); remain Claiming so the node re-lists and retries.
		return s, fmt.Sprintf("refused: position %d holds epoch %d, retry", at, s.Slots[at].Epoch), true
	case Write:
		if !n.Up || n.Role != Owner || n.Local >= MaxLocal {
			return s, "", false
		}
		n.Local++
		return s, fmt.Sprintf("ok: local commit %d", n.Local), true
	case Ship:
		if !n.Up || n.Role != Owner || n.Local <= n.Shipped {
			return s, "", false
		}
		first := n.Shipped + 1
		accepted, inBounds := s.put(mutant, Object{Epoch: n.Epoch, First: first, Last: n.Local})
		switch {
		case !inBounds:
			return s, "", false
		case accepted:
			n.Shipped = n.Local
			return s, fmt.Sprintf("ok: positions %d-%d accepted", first, n.Local), true
		case s.Slots[first].Epoch > n.Epoch:
			n.Role, n.Belief = Fenced, 0
			return s, fmt.Sprintf("refused: position %d holds epoch %d", first, s.Slots[first].Epoch), true
		}
		n.Role = Stuck
		return s, fmt.Sprintf("refused: position %d holds epoch %d", first, s.Slots[first].Epoch), true
	case Crash:
		if !n.Up {
			return s, "", false
		}
		// Crash wipes volatile state; Belief and Local remain on disk.
		n.Up, n.Role, n.Epoch, n.Shipped = false, Idle, 0, 0
		return s, "ok: volatile state lost", true
	case Restart, RestartStale:
		if n.Up || (a.Kind == RestartStale && s.Slots[1].Epoch == 0) {
			return s, "", false
		}
		n.Up = true
		if n.Belief == 0 {
			return s, "ok: no ownership belief", true
		}
		n.Epoch, n.Role = n.Belief, Owner
		view := s.chain(a.Kind == RestartStale)
		if mutant == ResumeFromGlobalEnd {
			// Models unfenced walship.Open: resume at the store's global end and
			// treat the local file as a discontinuity snapshot at the next slot.
			n.Shipped = end(view)
			n.Local = n.Shipped + 1
			return s, fmt.Sprintf("ok: resumed at the store's end %d", n.Shipped), true
		}
		if newerThan(view, n.Epoch) {
			n.Role, n.Belief = Fenced, 0
			return s, "ok: restarted, then refused: a newer epoch is in the store", true
		}
		n.Shipped = end(view)
		if n.Local < n.Shipped {
			n.Local = n.Shipped
		}
		return s, fmt.Sprintf("ok: resumed own chain at %d", n.Shipped), true
	}
	return s, "", false
}

func overlaps(a, b Object) bool { return a.First <= b.Last && b.First <= a.Last }

// Violation checks all three safety invariants over s.Accepted and returns a
// description of the first violation found, or "" if s is valid.
func (s *State) Violation() string {
	highest := uint8(0)
	for i := 0; i < int(s.NAccepted); i++ {
		a := s.Accepted[i]
		for j := 0; j < i; j++ {
			if b := s.Accepted[j]; b.Epoch != a.Epoch && overlaps(a, b) {
				return fmt.Sprintf("epochs %d and %d were both accepted for positions %d-%d and %d-%d", b.Epoch, a.Epoch, b.First, b.Last, a.First, a.Last)
			}
		}
		if a.Epoch < highest {
			return fmt.Sprintf("epoch %d was accepted at positions %d-%d after epoch %d had been accepted", a.Epoch, a.First, a.Last, highest)
		}
		highest = max(highest, a.Epoch)
		if s.Slots[a.First] != a {
			return fmt.Sprintf("the accepted object of epoch %d at position %d was overwritten", a.Epoch, a.First)
		}
	}
	return ""
}

func actions() []Action {
	var all []Action
	for node := uint8(0); node < Nodes; node++ {
		for kind := Kind(0); kind < kindCount; kind++ {
			all = append(all, Action{Node: node, Kind: kind})
		}
	}
	return all
}

// Report summarizes an Explore run: exhaustive interleaving and state counts up
// to Depth, the shortest violating trace if any, and random-walk totals.
type Report struct {
	Mutant        Mutant
	Depth         int
	Interleavings uint64
	Violating     uint64
	States        int
	Shortest      []string
	Violation     string
	Walks         int
	WalkSteps     int
	WalkViolating int
}

type memoKey struct {
	state State
	depth int
}

type counts struct{ paths, violating uint64 }

// Explore exhaustively counts every action sequence of length <= depth (via
// memoized DFS), finds the shortest violating trace and distinct-state count
// (via BFS), and runs walks seeded random walks of up to walkLength steps
// beyond the exhaustive depth.
func Explore(mutant Mutant, depth int, seed uint64, walks, walkLength int) Report {
	order := actions()
	rand.New(rand.NewPCG(seed, 1001)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	report := Report{Mutant: mutant, Depth: depth}
	memo := map[memoKey]counts{}
	var count func(State, int) counts
	count = func(s State, d int) counts {
		if s.Violation() != "" {
			return counts{1, 1}
		}
		if d == 0 {
			return counts{1, 0}
		}
		key := memoKey{s, d}
		if c, ok := memo[key]; ok {
			return c
		}
		var total counts
		for _, a := range order {
			if next, _, ok := Step(s, mutant, a); ok {
				c := count(next, d-1)
				total.paths += c.paths
				total.violating += c.violating
			}
		}
		if total.paths == 0 {
			total = counts{1, 0}
		}
		memo[key] = total
		return total
	}
	c := count(Initial(), depth)
	report.Interleavings, report.Violating = c.paths, c.violating
	report.States, report.Shortest, report.Violation = breadthFirst(mutant, depth, order)
	rng := rand.New(rand.NewPCG(seed, 2002))
	for w := 0; w < walks; w++ {
		s := Initial()
		for step := 0; step < walkLength; step++ {
			var enabled []State
			for _, a := range order {
				if next, _, ok := Step(s, mutant, a); ok {
					enabled = append(enabled, next)
				}
			}
			if len(enabled) == 0 {
				break
			}
			s = enabled[rng.IntN(len(enabled))]
			report.WalkSteps++
			if s.Violation() != "" {
				report.WalkViolating++
				break
			}
		}
		report.Walks++
	}
	return report
}

type visit struct {
	parent State
	action Action
	depth  int
}

func breadthFirst(mutant Mutant, depth int, order []Action) (int, []string, string) {
	start := Initial()
	seen := map[State]visit{start: {depth: 0}}
	frontier := []State{start}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []State
		for _, s := range frontier {
			for _, a := range order {
				t, _, ok := Step(s, mutant, a)
				if !ok {
					continue
				}
				if _, done := seen[t]; done {
					continue
				}
				seen[t] = visit{parent: s, action: a, depth: d + 1}
				if v := t.Violation(); v != "" {
					return len(seen), trace(mutant, seen, t), v
				}
				next = append(next, t)
			}
		}
		frontier = next
	}
	return len(seen), nil, ""
}

func trace(mutant Mutant, seen map[State]visit, last State) []string {
	var path []Action
	for s := last; seen[s].depth > 0; s = seen[s].parent {
		path = append([]Action{seen[s].action}, path...)
	}
	lines, _ := Replay(mutant, path)
	return lines
}

// Replay executes script from Initial under mutant and returns one formatted
// trace line per action together with the resulting State.
func Replay(mutant Mutant, script []Action) ([]string, State) {
	s := Initial()
	var lines []string
	for i, a := range script {
		next, outcome, ok := Step(s, mutant, a)
		if !ok {
			outcome = "refused: not enabled (" + roleName(s.Nodes[a.Node]) + ")"
		} else {
			s = next
		}
		lines = append(lines, fmt.Sprintf("%2d. %-28s %s", i+1, a.String(), outcome))
	}
	return lines, s
}

func roleName(n NodeState) string {
	if !n.Up {
		return "down"
	}
	return [...]string{"idle", "claiming", "owner", "fenced", "stuck"}[n.Role]
}

// ChainLines formats the store's contiguous slot chain in the same line format
// Experiment 10.1 uses when comparing the model against real SeaweedFS slots.
func (s State) ChainLines() []string {
	var lines []string
	for _, o := range s.chain(false) {
		kind := "segment"
		if o.Snapshot {
			kind = "snapshot"
		}
		lines = append(lines, fmt.Sprintf("positions %d-%d epoch %d %s", o.First, o.Last, o.Epoch, kind))
	}
	return lines
}

// ParseScript parses a comma-separated list of "<node> <kind>" actions
// (for example "A claim, A fence") into a slice of Actions.
func ParseScript(text string) ([]Action, error) {
	var script []Action
	for _, item := range strings.Split(text, ",") {
		fields := strings.Fields(item)
		if len(fields) != 2 || len(fields[0]) != 1 || fields[0][0] < 'A' || fields[0][0] >= 'A'+Nodes {
			return nil, fmt.Errorf("model: bad action %q", item)
		}
		kind := Kind(0)
		for kind < kindCount && kindNames[kind] != fields[1] {
			kind++
		}
		if kind == kindCount {
			return nil, fmt.Errorf("model: unknown action kind %q", fields[1])
		}
		script = append(script, Action{Node: fields[0][0] - 'A', Kind: kind})
	}
	return script, nil
}
```

### 5.8 Service node runtime, relocation commands, and remote driver (`service/node`)

`node` ties together `connmgr`, `walship.Shipper`, `fence.Adapter`, `catalog.Catalog`, the four on-disk sidecars (`.owner`, `.atrest`, `.sealed`, `.staged`), takeover `Claim`, explicit `LocalDurable`/`StoreDurable` writes, all relocation participant operations (`Seal`, `Drain`, `Stage`, `Activate`, `Retire`, `Discard`, `Status`, `Inspect`), the stdin/stdout line-protocol server (`Serve`, `handleRelocation`), child-process lifecycle (`Process`, `Signal`/`Reap`), and strict typed client (`Remote`, `ParseAck`).

<!-- include: service/node/node.go -->
```go
// Package node owns a single service node's runtime: database routing, bounded
// LRU connection management (internal/connmgr), continuous WAL capture per open
// database (internal/walship over fence.Adapter), local ownership and at-rest
// state files, takeover claims, relocation participant operations (Seal, Drain,
// Stage, Activate, Retire, Discard), and the two explicit acknowledgement modes
// (LocalDurable and StoreDurable). It also provides the line-protocol server
// (Serve), child-process driver (Process), and typed remote client (Remote)
// used by multi-process experiments.
//
// Per-database crash-surviving state on disk lives in four sidecar files in
// Config.Dir:
//   - <id>.owner holds "epoch N\n" so a restarted node retains its pre-crash
//     ownership belief until the store proves a newer epoch fenced it.
//   - <id>.atrest holds the 64-hex sha256 recorded after a clean eviction or
//     close so offline tampering between runs is rejected before shipping.
//   - <id>.sealed holds "epoch N\n" while a relocation is draining this source
//     so Write refuses new commits until Activate, Retire, or Claim clears it.
//   - <id>.staged holds a destination node's rebuilt database image between
//     Stage (relocation step 4) and Activate, Discard, Retire, or Claim.
//
// Each open database holds 3 SQLite connections (the writer plus walship's
// control and pin connections) and opens 7 file descriptors (3 .db, 3 -wal,
// and 1 process-shared -shm via unixInodeInfo, src/os_unix.c:1323-1342).
package node

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

// AckMode selects when Node.Write acknowledges a committed transaction.
type AckMode string

// Acknowledgement modes and claim constants.
const (
	// LocalDurable acknowledges immediately after the local SQLite commit at
	// synchronous=FULL; unshipped commits can be lost if another node takes over.
	LocalDurable AckMode = "local-durable"
	// StoreDurable acknowledges only after the shipper puts the segment holding
	// the commit into the object store.
	StoreDurable AckMode = "store-durable"
	// ReasonFence is the walship snapshot reason recorded on a takeover fence.
	ReasonFence   = "fence"
	claimAttempts = 3
)

// Sentinel errors returned by Node ownership, durability, and reopen checks.
var (
	// ErrNotOwner reports that this node has no ownership belief for the database.
	ErrNotOwner = errors.New("this node does not own the database")
	// ErrNotLocallyDurable reports a LocalDurable write against a database whose
	// catalog settings do not specify synchronous=FULL.
	ErrNotLocallyDurable = errors.New("local-durable acknowledgement needs synchronous=FULL")
	// ErrMissingLocalFile reports that an owned database's local file is absent.
	ErrMissingLocalFile = errors.New("the local database file is missing; claim the database again to restore it")
	// ErrChangedWhileShut reports that a database closed cleanly no longer
	// matches its recorded at-rest sha256 (or still has WAL sidecars).
	ErrChangedWhileShut = errors.New("the local database file changed while it was closed")
	idPattern           = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Config configures a Node: its identity, local working directory, shared
// catalog, object store, open-database LRU capacity, persistent SQLite file
// settings, and initializer for a brand-new database (empty store chain).
// AckBeforePut is an intentional bug mutant used by Experiment 10.2's self-test
// to skip shipping on StoreDurable writes and prove the checker detects lost
// store-durable acknowledgements.
type Config struct {
	Name         string
	Dir          string
	Catalog      *catalog.Catalog
	Store        walship.Store
	Capacity     int
	File         sqlitex.FileSettings
	Create       func(ctx context.Context, path string) error
	AckBeforePut bool
}

// Ack describes an acknowledged write: the durability mode, the owning epoch,
// the last shipped stream position (0 for LocalDurable), and the database's
// user_version after the write.
type Ack struct {
	Mode        AckMode
	Epoch       uint64
	Position    int64
	UserVersion int
}

// ClaimResult summarizes a completed Node.Claim: the new catalog epoch, the
// stream position restored from the store (0 when newly created), the position
// of the accepted fence snapshot, and how many listing/put attempts were needed.
type ClaimResult struct {
	Epoch        uint64
	RestoredFrom int64
	Fence        int64
	Attempts     int
}

type capture struct {
	shipper *walship.Shipper
	epoch   uint64
	broken  bool
}

// Node serves databases it owns on one host, bounding open handles through
// internal/connmgr and serializing public calls with a single mutex.
type Node struct {
	config   Config
	mu       sync.Mutex
	manager  *connmgr.Manager
	owned    map[string]uint64
	fenced   map[string]error
	open     map[string]*capture
	atRest   map[string]string
	problems []string
}

// PlatformSettings returns the per-connection SQLite settings registered in
// the catalog for platform databases: WAL mode's default recipe settings with
// wal_autocheckpoint=0 (so only walship checkpoints) and a 256 KiB cache.
func PlatformSettings() sqlitex.ConnSettings {
	settings := recipe.Default().SQLiteOptions().Conn
	settings.WALAutocheckpoint = 0
	settings.CacheSize = -256
	return settings
}

// Start initializes a Node in config.Dir, strictly loading any persisted
// *.owner belief files (rejecting epoch 0) and *.atrest digests from prior runs.
func Start(config Config) (*Node, error) {
	if config.Name == "" || config.Catalog == nil || config.Store == nil || config.Create == nil {
		return nil, errors.New("node: Name, Catalog, Store and Create are required")
	}
	if err := os.MkdirAll(config.Dir, 0o755); err != nil {
		return nil, err
	}
	n := &Node{config: config, owned: map[string]uint64{}, fenced: map[string]error{}, open: map[string]*capture{}, atRest: map[string]string{}}
	beliefs, err := filepath.Glob(filepath.Join(config.Dir, "*.owner"))
	if err != nil {
		return nil, err
	}
	for _, belief := range beliefs {
		var epoch uint64
		data, err := os.ReadFile(belief)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Sscanf(string(data), "epoch %d\n", &epoch); err != nil || epoch == 0 || fmt.Sprintf("epoch %d\n", epoch) != string(data) {
			return nil, fmt.Errorf("node: belief file %s: %q", belief, data)
		}
		n.owned[strings.TrimSuffix(filepath.Base(belief), ".owner")] = epoch
	}
	records, err := filepath.Glob(filepath.Join(config.Dir, "*.atrest"))
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		data, err := os.ReadFile(record)
		if err != nil {
			return nil, err
		}
		digest := strings.TrimSuffix(string(data), "\n")
		if !sha256Pattern.MatchString(digest) || digest+"\n" != string(data) {
			return nil, fmt.Errorf("node: at-rest record %s: %q", record, data)
		}
		n.atRest[strings.TrimSuffix(filepath.Base(record), ".atrest")] = digest
	}
	n.manager, err = connmgr.New(connmgr.Options{Capacity: config.Capacity, PathFor: n.path, Open: n.openDatabase, OnEvict: n.evicted})
	return n, err
}

func (n *Node) path(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("node: invalid database id %q", id)
	}
	return filepath.Join(n.config.Dir, id+".db"), nil
}

func (n *Node) options(settings sqlitex.ConnSettings) sqlitex.Options {
	return sqlitex.Options{File: n.config.File, Conn: settings, SingleConnection: true, ImmediateTx: true}
}

func (n *Node) openDatabase(ctx context.Context, path string) (*sqlitex.DB, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	epoch, ok := n.owned[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotOwner, id)
	}
	// Refuse a missing local file explicitly: otherwise SQLite would create an
	// empty database and walship would publish it as a discontinuity snapshot.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMissingLocalFile, err)
	}
	if recorded, ok := n.atRest[id]; ok {
		status, err := connmgr.CheckAtRest(path)
		if err != nil {
			return nil, err
		}
		if !status.AtRest || status.SHA256 != recorded {
			return nil, fmt.Errorf("%w: %s: %s", ErrChangedWhileShut, id, status)
		}
		// Remove the on-disk at-rest digest once the reopen check passes, before
		// opening the shipper: if the node crashes while the database is open,
		// the next restart sees no .atrest file and treats it as post-crash
		// (shipping any unshipped local commits instead of rejecting the file).
		if err := n.removeFile(id, ".atrest"); err != nil {
			return nil, err
		}
	}
	record, err := n.config.Catalog.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	adapter, err := fence.New(n.config.Store, id, epoch)
	if err != nil {
		return nil, err
	}
	options := n.options(record.Settings)
	// Opening walship lists the fenced slot chain through adapter, which fences
	// a restarted stale owner before any local write or upload can happen.
	shipper, err := walship.Open(ctx, walship.Config{Database: path, Store: adapter, Options: options})
	if err != nil {
		return nil, err
	}
	if _, err := shipper.AdoptAtRest(ctx); err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
	// Ship any commits left unshipped on disk (for example, local-durable writes
	// committed before a process crash) before admitting new requests.
	report, err := shipper.Cycle(ctx)
	if err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
	if shipped(report) {
		var version int
		err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		if err == nil {
			err = n.recordShipped(ctx, id, epoch, shipper.Position(), version)
		}
		if err != nil {
			return nil, errors.Join(err, db.Close(), shipper.Close())
		}
	}
	n.open[id] = &capture{shipper: shipper, epoch: epoch}
	return db, nil
}

func shipped(report walship.CycleReport) bool {
	return report.Segment != nil || report.Snapshot != nil
}

func (n *Node) recordShipped(ctx context.Context, id string, epoch uint64, position int64, userVersion int) error {
	err := n.config.Catalog.RecordShipped(ctx, id, epoch, position, userVersion, n.config.Name+":"+id+".db")
	// Ignore ErrStaleEpoch: the store already accepted this put into the one
	// lineage, and another node bumping the catalog epoch before fencing does
	// not invalidate an already-accepted slot.
	if errors.Is(err, catalog.ErrStaleEpoch) {
		return nil
	}
	return err
}

func (n *Node) removeFile(id, suffix string) error {
	err := os.Remove(filepath.Join(n.config.Dir, id+suffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (n *Node) writeFile(id, suffix, content string) error {
	final := filepath.Join(n.config.Dir, id+suffix)
	temp := final + ".tmp"
	if err := os.WriteFile(temp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, final)
}

func (n *Node) evicted(eviction connmgr.Eviction) {
	c := n.open[eviction.ID]
	delete(n.open, eviction.ID)
	delete(n.atRest, eviction.ID)
	if c == nil {
		return
	}
	_, fenced := n.fenced[eviction.ID]
	var shipErr error
	var report walship.CycleReport
	if !fenced && !c.broken {
		report, shipErr = c.shipper.Cycle(context.Background())
	}
	// Closing the shipper closes the last SQLite connections to the database,
	// so sqlite3WalClose (src/wal.c:2507-2571) checkpoints and deletes -wal/-shm.
	closeErr := c.shipper.Close()
	if errors.Is(shipErr, fence.ErrFenced) {
		n.markFenced(eviction.ID, shipErr)
		fenced = true
	}
	if err := errors.Join(eviction.CloseErr, shipErr, closeErr); err != nil && !fenced {
		n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", eviction.ID, err))
		return
	}
	if fenced || c.broken {
		return
	}
	// Verify at-rest state here rather than relying on connmgr's own check:
	// connmgr ran while the shipper still held -wal/-shm (returning early
	// without opening the file, avoiding src/os_unix.c:1189-1342 lock loss).
	// Now that all connections are closed, prove local bytes == last shipped state.
	status, err := connmgr.CheckAtRest(eviction.Path)
	switch {
	case err != nil:
		n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", eviction.ID, err))
	case !status.AtRest || status.SHA256 != c.shipper.StateSHA256():
		n.problems = append(n.problems, fmt.Sprintf("evict %s: the closed file is not the last shipped state: %s", eviction.ID, status))
	default:
		n.atRest[eviction.ID] = status.SHA256
		if err := n.writeFile(eviction.ID, ".atrest", status.SHA256+"\n"); err != nil {
			n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", eviction.ID, err))
		}
	}
	if shipped(report) {
		version, err := userVersionOf(eviction.Path)
		if err == nil {
			err = n.recordShipped(context.Background(), eviction.ID, c.epoch, c.shipper.Position(), version)
		}
		if err != nil {
			n.problems = append(n.problems, fmt.Sprintf("evict %s: record shipped: %v", eviction.ID, err))
		}
	}
}

func userVersionOf(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, err
	}
	// Bytes 60..63 of the SQLite database header store the big-endian user_version.
	return int(binary.BigEndian.Uint32(header[60:64])), nil
}

func (n *Node) markFenced(id string, err error) {
	n.fenced[id] = err
	delete(n.owned, id)
	if removeErr := n.removeFile(id, ".owner"); removeErr != nil {
		n.problems = append(n.problems, fmt.Sprintf("forget ownership of %s: %v", id, removeErr))
	}
}

func (n *Node) evictIfOpen(id string) {
	if _, err := n.manager.Evict(id); err != nil && !errors.Is(err, connmgr.ErrNotOpen) {
		n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", id, err))
	}
}

func (n *Node) writeBelief(id string, epoch uint64) error {
	return n.writeFile(id, ".owner", fmt.Sprintf("epoch %d\n", epoch))
}

func pageSizeOf(image []byte) (int, error) {
	if len(image) < 100 {
		return 0, fmt.Errorf("node: image of %d bytes", len(image))
	}
	// Header bytes 16..17 store page size in big-endian; value 1 encodes 65536.
	size := int(binary.BigEndian.Uint16(image[16:18]))
	if size == 1 {
		size = 65536
	}
	if size < 512 || size&(size-1) != 0 || len(image)%size != 0 {
		return 0, fmt.Errorf("node: image of %d bytes has page size %d", len(image), size)
	}
	return size, nil
}

// Claim takes ownership of id for this node: it clears any local belief,
// at-rest digest, seal, and staged file, bumps the catalog epoch, restores the
// store's latest state (or creates an empty database when the chain is empty),
// writes a fence snapshot at end+1 with put-if-absent (retrying on ErrExists
// from a stale listing), writes the local database and <id>.owner belief file,
// and updates the catalog.
func (n *Node) Claim(ctx context.Context, id string) (ClaimResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return ClaimResult{}, err
	}
	n.evictIfOpen(id)
	delete(n.fenced, id)
	delete(n.owned, id)
	delete(n.atRest, id)
	// Remove both sidecars on disk before claiming so a failed re-claim cannot
	// resurrect the old epoch on restart, and an older at-rest digest cannot
	// reject a reclaimed database that another node advanced.
	if err := errors.Join(n.removeFile(id, ".owner"), n.removeFile(id, ".atrest")); err != nil {
		return ClaimResult{}, err
	}
	if err := errors.Join(n.removeFile(id, ".sealed"), recipe.RemoveDatabase(n.sidecar(id, ".staged"))); err != nil {
		return ClaimResult{}, err
	}
	epoch, err := n.config.Catalog.Claim(ctx, id, n.config.Name)
	if err != nil {
		return ClaimResult{}, err
	}
	adapter, err := fence.New(n.config.Store, id, epoch)
	if err != nil {
		return ClaimResult{}, err
	}
	for attempt := 1; attempt <= claimAttempts; attempt++ {
		end, err := adapter.Refresh(ctx)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		// imageAt reads through adapter's already-cached listing so the restored
		// image and the fence position end+1 come from the same listing.
		image, err := n.imageAt(ctx, adapter, path, end)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		pageSize, err := pageSizeOf(image)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, fmt.Errorf("claim %s: %w", id, err)
		}
		meta := walship.SnapshotMeta{
			Position: end + 1, Reason: ReasonFence, PageSize: pageSize, Pages: uint32(len(image) / pageSize),
			UserVersion: binary.BigEndian.Uint32(image[60:64]), StateSHA256: walship.SHA256Hex(image),
		}
		object, err := walship.EncodeSnapshot(meta, image)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		err = adapter.PutIfAbsent(ctx, walship.SnapshotKey(end+1), object)
		if errors.Is(err, walship.ErrExists) {
			// A lower or equal epoch occupies end+1 (stale listing or a last
			// pre-fence ship by the old owner); re-list and try again.
			continue
		}
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		if err := recipe.RemoveDatabase(path); err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		if err := walship.WriteDatabase(path, image); err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		if err := n.writeBelief(id, epoch); err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		n.owned[id] = epoch
		result := ClaimResult{Epoch: epoch, RestoredFrom: end, Fence: end + 1, Attempts: attempt}
		return result, n.recordShipped(ctx, id, epoch, end+1, int(meta.UserVersion))
	}
	return ClaimResult{Epoch: epoch, Attempts: claimAttempts}, fmt.Errorf("node: claim %s: the next position stayed taken for %d attempts", id, claimAttempts)
}

func (n *Node) imageAt(ctx context.Context, adapter *fence.Adapter, path string, end int64) ([]byte, error) {
	if end > 0 {
		chain, err := walship.LoadCatalog(ctx, adapter)
		if err != nil {
			return nil, err
		}
		plan, err := chain.Plan(end)
		if err != nil {
			return nil, err
		}
		image, _, err := plan.Execute()
		return image, err
	}
	scratch := path + "-create"
	if err := recipe.RemoveDatabase(scratch); err != nil {
		return nil, err
	}
	if err := n.config.Create(ctx, scratch); err != nil {
		return nil, err
	}
	if err := recipe.CheckAtRest(scratch); err != nil {
		return nil, err
	}
	image, err := os.ReadFile(scratch)
	return image, errors.Join(err, recipe.RemoveDatabase(scratch))
}

func (n *Node) acquire(ctx context.Context, id string) (*connmgr.Handle, uint64, error) {
	if err := n.fenced[id]; err != nil {
		return nil, 0, err
	}
	epoch, ok := n.owned[id]
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotOwner, id)
	}
	h, err := n.manager.Acquire(ctx, id)
	if errors.Is(err, fence.ErrFenced) {
		n.markFenced(id, err)
	}
	return h, epoch, err
}

// Write runs apply against database id under mode, first refusing any database
// sealed for relocation (ErrSealed). LocalDurable requires synchronous=FULL in
// the catalog settings and acknowledges after the local commit. StoreDurable
// acknowledges only after shipping the new WAL frames to the store (unless the
// AckBeforePut test mutant is enabled); if shipping fails (including
// FencedError), the write is committed locally on this node's orphaned file but
// not acknowledged, and the database is evicted so the next use reopens from
// the store.
func (n *Node) Write(ctx context.Context, id string, mode AckMode, apply func(context.Context, *sqlitex.DB) error) (Ack, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if mode != LocalDurable && mode != StoreDurable {
		return Ack{}, fmt.Errorf("node: unknown acknowledgement mode %q", mode)
	}
	if err := n.refuseSealed(id); err != nil {
		return Ack{}, err
	}
	h, epoch, err := n.acquire(ctx, id)
	if err != nil {
		return Ack{}, err
	}
	if mode == LocalDurable && !strings.EqualFold(h.DB.Options.Conn.Synchronous, sqlitex.SyncFull) {
		h.Release()
		return Ack{}, fmt.Errorf("%w: %s runs synchronous=%s", ErrNotLocallyDurable, id, h.DB.Options.Conn.Synchronous)
	}
	ack := Ack{Mode: mode, Epoch: epoch}
	err = apply(ctx, h.DB)
	if err == nil {
		err = h.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ack.UserVersion)
	}
	if err == nil && mode == StoreDurable && !n.config.AckBeforePut {
		ack.Position, err = n.shipLocked(ctx, id, ack.UserVersion)
	}
	h.Release()
	n.afterShip(id, err)
	if err != nil {
		return Ack{}, err
	}
	return ack, nil
}

func (n *Node) shipLocked(ctx context.Context, id string, userVersion int) (int64, error) {
	c := n.open[id]
	if c == nil {
		return 0, fmt.Errorf("node: %s has no open shipper", id)
	}
	if _, err := c.shipper.Cycle(ctx); err != nil {
		c.broken = true
		return 0, err
	}
	position := c.shipper.Position()
	return position, n.recordShipped(ctx, id, c.epoch, position, userVersion)
}

func (n *Node) afterShip(id string, err error) {
	if errors.Is(err, fence.ErrFenced) {
		n.markFenced(id, err)
	}
	if c := n.open[id]; c != nil && c.broken {
		n.evictIfOpen(id)
	}
}

// Ship runs one walship Cycle for database id, updates the catalog, and returns
// the latest shipped stream position.
func (n *Node) Ship(ctx context.Context, id string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	h, _, err := n.acquire(ctx, id)
	if err != nil {
		return 0, err
	}
	var version int
	err = h.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	var position int64
	if err == nil {
		position, err = n.shipLocked(ctx, id, version)
	}
	h.Release()
	n.afterShip(id, err)
	return position, err
}

// Read acquires database id (opening and fencing-checking it if not open) and
// runs read against its SQLite connection.
func (n *Node) Read(ctx context.Context, id string, read func(context.Context, *sqlitex.DB) error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	h, _, err := n.acquire(ctx, id)
	if err != nil {
		return err
	}
	defer h.Release()
	return read(ctx, h.DB)
}

// Owned reports whether this node currently believes it owns id, and if so at
// what epoch.
func (n *Node) Owned(id string) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	epoch, ok := n.owned[id]
	return epoch, ok
}

// Stats returns the underlying connmgr statistics (open count, evictions, etc.).
func (n *Node) Stats() connmgr.Stats { return n.manager.Stats() }

// TakeProblems returns and clears any background problems recorded during
// evictions or ownership cleanup (such as a failed final ship or at-rest check).
func (n *Node) TakeProblems() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	problems := n.problems
	n.problems = nil
	return problems
}

// Close closes every open database (shipping and verifying at-rest state via
// evicted) and returns any close errors joined with recorded problems.
func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	err := n.manager.Close()
	if len(n.problems) > 0 {
		err = errors.Join(err, errors.New(strings.Join(n.problems, "; ")))
	}
	return err
}
```

<!-- include: service/node/relocate.go -->
```go
package node

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

// Sentinel errors returned by relocation participant operations on a Node.
var (
	// ErrSealed reports that Write targeted a database sealed for relocation.
	ErrSealed = errors.New("the database is sealed for relocation")
	// ErrNotSealed reports that Drain or Retire was called on an owned database
	// that has not been sealed yet.
	ErrNotSealed = errors.New("the database is not sealed for relocation")
	// ErrMovedAfterDrain reports that Activate found the store chain no longer
	// ends at the drained position recorded before activation.
	ErrMovedAfterDrain = errors.New("the store moved past the drained position")
	// ErrStagedMismatch reports that the staged (or adopted) database image
	// does not hash to the drained sha256 recorded at step 3.
	ErrStagedMismatch = errors.New("the staged image is not the drained state")
)

// Drained reports the stream Position and at-rest file SHA256 left by Drain
// after shipping the source's final commits and closing the database cleanly.
type Drained struct {
	Position int64
	SHA256   string
}

// Staged reports the stream Position and SHA256 of the image rebuilt into
// <id>.staged on the destination node during relocation step 4.
type Staged struct {
	Position int64
	SHA256   string
}

// Activated reports the store Fence position written (or adopted) by Activate,
// with Adopted true when the destination's fence snapshot was already in the store.
type Activated struct {
	Fence   int64
	Adopted bool
}

// Inspection holds the PRAGMA user_version and PRAGMA integrity_check results
// returned by Node.Inspect.
type Inspection struct {
	UserVersion int
	Integrity   string
}

func (n *Node) sidecar(id, suffix string) string {
	return filepath.Join(n.config.Dir, id+suffix)
}

func (n *Node) isSealed(id string) bool {
	_, err := os.Stat(n.sidecar(id, ".sealed"))
	return err == nil
}

func (n *Node) refuseSealed(id string) error {
	if n.isSealed(id) {
		return fmt.Errorf("%w: %s", ErrSealed, id)
	}
	return nil
}

// Seal performs relocation step 2 on the source node: it writes <id>.sealed
// atomically so subsequent Write calls return ErrSealed before draining.
// Sealing is a cooperative courtesy; the store fence in Activate is what binds
// an uncooperative source.
func (n *Node) Seal(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return err
	}
	if err := n.fenced[id]; err != nil {
		return err
	}
	epoch, ok := n.owned[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOwner, id)
	}
	return n.writeFile(id, ".sealed", fmt.Sprintf("epoch %d\n", epoch))
}

// Drain performs relocation step 3 on a sealed source node: it acquires id
// (reopening and shipping any unshipped local commits if needed) and evicts it
// so evicted runs a final walship cycle, closes the last SQLite connection
// (triggering sqlite3WalClose at src/wal.c:2507-2571 to checkpoint and delete
// -wal/-shm), and proves the closed file's sha256 equals the shipped state.
// Any eviction problem is returned as an error rather than left in n.problems.
func (n *Node) Drain(ctx context.Context, id string) (Drained, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return Drained{}, err
	}
	if !n.isSealed(id) {
		return Drained{}, fmt.Errorf("%w: %s", ErrNotSealed, id)
	}
	h, _, err := n.acquire(ctx, id)
	if err != nil {
		return Drained{}, err
	}
	c := n.open[id]
	h.Release()
	if c == nil {
		return Drained{}, fmt.Errorf("node: %s has no open shipper", id)
	}
	// Capture problems emitted by this eviction so Drain fails explicitly if
	// either the at-rest verification, .atrest write, or catalog update failed.
	before := len(n.problems)
	n.evictIfOpen(id)
	problems := append([]string(nil), n.problems[before:]...)
	n.problems = n.problems[:before]
	if err := n.fenced[id]; err != nil {
		return Drained{}, err
	}
	digest, ok := n.atRest[id]
	if !ok {
		return Drained{}, fmt.Errorf("node: drain %s: the final ship did not leave the shipped state at rest: %s", id, strings.Join(problems, "; "))
	}
	if len(problems) > 0 {
		return Drained{}, fmt.Errorf("node: drain %s: the final ship left the state at rest but reported: %s", id, strings.Join(problems, "; "))
	}
	return Drained{Position: c.shipper.Position(), SHA256: digest}, nil
}

// Stage performs relocation step 4 on the destination node: it lists the slot
// chain through a fence.Adapter at epoch (failing with FencedError if a newer
// epoch is already in the store), rebuilds the database image at the chain's
// end, and writes it to <id>.staged without installing it as live.
func (n *Node) Stage(ctx context.Context, id string, epoch uint64) (Staged, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return Staged{}, err
	}
	adapter, err := fence.New(n.config.Store, id, epoch)
	if err != nil {
		return Staged{}, err
	}
	end, err := adapter.Refresh(ctx)
	if err != nil {
		return Staged{}, err
	}
	if end == 0 {
		return Staged{}, fmt.Errorf("node: stage %s: the store holds nothing to rebuild", id)
	}
	image, err := n.imageAt(ctx, adapter, path, end)
	if err != nil {
		return Staged{}, err
	}
	staged := n.sidecar(id, ".staged")
	if err := recipe.RemoveDatabase(staged); err != nil {
		return Staged{}, err
	}
	if err := walship.WriteDatabase(staged, image); err != nil {
		return Staged{}, err
	}
	return Staged{Position: end, SHA256: walship.SHA256Hex(image)}, nil
}

func fenceSnapshot(position int64, image []byte) ([]byte, uint32, error) {
	pageSize, err := pageSizeOf(image)
	if err != nil {
		return nil, 0, err
	}
	meta := walship.SnapshotMeta{
		Position: position, Reason: ReasonFence, PageSize: pageSize, Pages: uint32(len(image) / pageSize),
		UserVersion: binary.BigEndian.Uint32(image[60:64]), StateSHA256: walship.SHA256Hex(image),
	}
	object, err := walship.EncodeSnapshot(meta, image)
	return object, meta.UserVersion, err
}

// Activate performs relocation step 6 on the destination node. It verifies no
// newer epoch exists in the store, puts the fence snapshot at position+1 with
// put-if-absent (unless fenceStore is false for the skip-fence mutant, or the
// fence already landed on an earlier attempt and is adopted), installs the
// verified image as <id>.db, writes <id>.owner, clears temporary sidecars, and
// records the new location and position in the catalog.
//
// When called again on a node that already holds epoch and whose fence is
// already in the store, Activate repairs the catalog row if the store still
// ends at position+1 (recovering from a prior catalog error or crash after
// writeBelief) without overwriting later writes if the chain has moved on.
func (n *Node) Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (Activated, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return Activated{}, err
	}
	if position < 1 || !sha256Pattern.MatchString(digest) {
		return Activated{}, fmt.Errorf("node: activate %s at position %d with sha256 %q", id, position, digest)
	}
	chain, err := fence.Chain(ctx, n.config.Store, id)
	if err != nil {
		return Activated{}, err
	}
	var image []byte
	var fenceSlot *fence.Slot
	result := Activated{Fence: position + 1}
	for _, slot := range chain {
		if slot.Epoch > epoch {
			return Activated{}, &fence.FencedError{Database: id, Epoch: epoch, Holder: slot.Epoch, Position: slot.First}
		}
	}
	for i, slot := range chain {
		if slot.Epoch == epoch && fenceSlot == nil {
			if slot.First != position+1 || slot.Kind != fence.KindSnapshot {
				return Activated{}, fmt.Errorf("node: activate %s: epoch %d starts at position %d with a %s, not with the fence at %d", id, epoch, slot.First, slot.Kind, position+1)
			}
			fenceSlot = &chain[i]
		}
	}
	if fenceSlot != nil {
		if owned, ok := n.owned[id]; ok && owned == epoch {
			result.Adopted = true
			// If the node already shipped writes past the fence, leave the
			// catalog alone so a retried Activate never rewinds last_shipped.
			if chainEnd(chain) != position+1 {
				return result, nil
			}
			meta, _, err := walship.DecodeSnapshot(fenceSlot.Object)
			if err != nil {
				return Activated{}, err
			}
			return result, n.recordShipped(ctx, id, epoch, position+1, int(meta.UserVersion))
		}
		// Adopt an already-landed fence (for example after fail-after-put or
		// crash-after-put) by decoding the image straight from the store slot.
		if end := chainEnd(chain); end != position+1 {
			return Activated{}, fmt.Errorf("node: activate %s: epoch %d wrote through position %d after its fence, and this node does not hold it", id, epoch, end)
		}
		if _, image, err = walship.DecodeSnapshot(fenceSlot.Object); err != nil {
			return Activated{}, err
		}
		result.Adopted = true
	}
	if image == nil {
		if end := chainEnd(chain); end != position {
			return Activated{}, fmt.Errorf("%w: %s: the store ends at position %d, the drain ended at %d", ErrMovedAfterDrain, id, end, position)
		}
		if image, err = os.ReadFile(n.sidecar(id, ".staged")); err != nil {
			return Activated{}, err
		}
	}
	if walship.SHA256Hex(image) != digest {
		return Activated{}, fmt.Errorf("%w: %s: %s, drained %s", ErrStagedMismatch, id, walship.SHA256Hex(image), digest)
	}
	object, userVersion, err := fenceSnapshot(position+1, image)
	if err != nil {
		return Activated{}, err
	}
	shipped := position + 1
	switch {
	case result.Adopted:
	case fenceStore:
		adapter, err := fence.New(n.config.Store, id, epoch)
		if err != nil {
			return Activated{}, err
		}
		if err := adapter.PutIfAbsent(ctx, walship.SnapshotKey(position+1), object); err != nil {
			return Activated{}, err
		}
	default:
		result.Fence, shipped = 0, position
	}
	n.evictIfOpen(id)
	if err := recipe.RemoveDatabase(path); err != nil {
		return result, err
	}
	if err := walship.WriteDatabase(path, image); err != nil {
		return result, err
	}
	if err := errors.Join(recipe.RemoveDatabase(n.sidecar(id, ".staged")), n.removeFile(id, ".atrest"), n.removeFile(id, ".sealed")); err != nil {
		return result, err
	}
	if err := n.writeBelief(id, epoch); err != nil {
		return result, err
	}
	delete(n.fenced, id)
	delete(n.atRest, id)
	n.owned[id] = epoch
	return result, n.recordShipped(ctx, id, epoch, shipped, int(userVersion))
}

func chainEnd(chain []fence.Slot) int64 {
	if len(chain) == 0 {
		return 0
	}
	return chain[len(chain)-1].Last
}

// Retire performs relocation step 7 on the source node: after verifying an
// owned database is sealed, it evicts any open handle, drops local ownership
// state, and deletes the database file and all four per-database sidecars.
func (n *Node) Retire(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return err
	}
	if _, owned := n.owned[id]; owned && !n.isSealed(id) {
		return fmt.Errorf("%w: %s is owned by this node and still writable", ErrNotSealed, id)
	}
	n.evictIfOpen(id)
	delete(n.owned, id)
	delete(n.fenced, id)
	delete(n.atRest, id)
	return errors.Join(recipe.RemoveDatabase(path), recipe.RemoveDatabase(n.sidecar(id, ".staged")),
		n.removeFile(id, ".owner"), n.removeFile(id, ".atrest"), n.removeFile(id, ".sealed"))
}

// Discard removes any leftover <id>.staged image on a destination node when a
// relocation is rolled back before activation.
func (n *Node) Discard(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return err
	}
	return recipe.RemoveDatabase(n.sidecar(id, ".staged"))
}

// Status reports this node's local view of id ("fenced", "not owner",
// "owner at epoch E, sealed", or "owner at epoch E, writable") for relocation
// boundary assertions.
func (n *Node) Status(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return "invalid id"
	}
	if n.fenced[id] != nil {
		return "fenced"
	}
	epoch, ok := n.owned[id]
	switch {
	case !ok:
		return "not owner"
	case n.isSealed(id):
		return fmt.Sprintf("owner at epoch %d, sealed", epoch)
	}
	return fmt.Sprintf("owner at epoch %d, writable", epoch)
}

// Inspect opens database id through Read and returns its PRAGMA integrity_check
// and PRAGMA user_version results (used after crash recovery in Experiment 10.2).
func (n *Node) Inspect(ctx context.Context, id string) (Inspection, error) {
	var inspection Inspection
	err := n.Read(ctx, id, func(ctx context.Context, db *sqlitex.DB) error {
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&inspection.Integrity); err != nil {
			return err
		}
		return db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&inspection.UserVersion)
	})
	return inspection, err
}
```

<!-- include: service/node/serve.go -->
```go
package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Explain formats err into stable, reader-facing text for line-protocol
// replies: FencedError's message verbatim, "not the owner" for ErrNotOwner, or
// "error: <err>" otherwise.
func Explain(err error) string {
	var fenced *fence.FencedError
	switch {
	case errors.As(err, &fenced):
		return fenced.Error()
	case errors.Is(err, ErrNotOwner):
		return "not the owner"
	}
	return "error: " + err.Error()
}

// Serve runs the node's stdin/stdout line protocol, dispatching core commands
// (claim, write, ship, close) and relocation/inspection commands (via
// handleRelocation in commands.go). It writes "ready", then for each command
// line emits any recorded "problem …" lines, one "ok …" or "refused …" reply,
// and one "bounds …" line checked against assertion. A bare "close" command
// also writes assertion.Summary() and returns nil.
func Serve(ctx context.Context, n *Node, assertion *bounds.Assertion, apply func(context.Context, *sqlitex.DB) error, in io.Reader, out io.Writer) error {
	lines := bufio.NewScanner(in)
	fmt.Fprintln(out, "ready")
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) == 0 {
			continue
		}
		reply := n.handle(ctx, fields, apply)
		for _, problem := range n.TakeProblems() {
			fmt.Fprintln(out, "problem "+problem)
		}
		fmt.Fprintln(out, reply)
		sample, err := assertion.Check()
		status := "ok"
		if err != nil {
			status = "EXCEEDED: " + err.Error()
		}
		fmt.Fprintf(out, "bounds descriptors=%d memory=%d %s\n", sample.Descriptors, sample.MemoryGrowth, status)
		// Exit only on a bare "close"; "close" with extra arguments is handled
		// as an unknown command without closing n.
		if fields[0] == "close" && len(fields) == 1 {
			fmt.Fprintln(out, assertion.Summary())
			return nil
		}
	}
	return lines.Err()
}

func (n *Node) handle(ctx context.Context, fields []string, apply func(context.Context, *sqlitex.DB) error) string {
	command := strings.Join(fields, " ")
	switch {
	case fields[0] == "claim" && len(fields) == 2:
		result, err := n.Claim(ctx, fields[1])
		if err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		return fmt.Sprintf("ok %s: epoch %d, restored position %d, fence at position %d, attempts %d", command, result.Epoch, result.RestoredFrom, result.Fence, result.Attempts)
	case fields[0] == "write" && len(fields) == 3:
		ack, err := n.Write(ctx, fields[1], AckMode(fields[2]), apply)
		if err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		if ack.Mode == LocalDurable {
			return fmt.Sprintf("ok %s: acknowledged at epoch %d, user_version %d, committed locally (not yet in the store)", command, ack.Epoch, ack.UserVersion)
		}
		return fmt.Sprintf("ok %s: acknowledged at epoch %d, user_version %d, stored through position %d", command, ack.Epoch, ack.UserVersion, ack.Position)
	case fields[0] == "ship" && len(fields) == 2:
		position, err := n.Ship(ctx, fields[1])
		if err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		return fmt.Sprintf("ok %s: stored through position %d", command, position)
	case fields[0] == "close" && len(fields) == 1:
		if err := n.Close(); err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		return "ok close"
	}
	return n.handleRelocation(ctx, command, fields)
}
```

<!-- include: service/node/commands.go -->
```go
package node

import (
	"context"
	"fmt"
	"strconv"
)

// handleRelocation dispatches the relocation participant commands (seal, drain,
// stage, activate [without-fence], retire, discard) plus status and check for
// Serve's line protocol. The optional "without-fence" argument on activate
// supports Experiment 10.3's skip-fence self-test mutant.
func (n *Node) handleRelocation(ctx context.Context, command string, fields []string) string {
	refused := func(err error) string { return fmt.Sprintf("refused %s: %s", command, Explain(err)) }
	switch {
	case fields[0] == "seal" && len(fields) == 2:
		if err := n.Seal(ctx, fields[1]); err != nil {
			return refused(err)
		}
		return "ok " + command
	case fields[0] == "drain" && len(fields) == 2:
		drained, err := n.Drain(ctx, fields[1])
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: position %d, sha256 %s", command, drained.Position, drained.SHA256)
	case fields[0] == "stage" && len(fields) == 3:
		epoch, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return refused(err)
		}
		staged, err := n.Stage(ctx, fields[1], epoch)
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: position %d, sha256 %s", command, staged.Position, staged.SHA256)
	case fields[0] == "activate" && (len(fields) == 5 || len(fields) == 6 && fields[5] == "without-fence"):
		epoch, epochErr := strconv.ParseUint(fields[2], 10, 64)
		position, positionErr := strconv.ParseInt(fields[3], 10, 64)
		if epochErr != nil || positionErr != nil {
			return refused(fmt.Errorf("activate needs an epoch and a position"))
		}
		activated, err := n.Activate(ctx, fields[1], epoch, position, fields[4], len(fields) == 5)
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: fence at position %d, adopted %t", command, activated.Fence, activated.Adopted)
	case fields[0] == "retire" && len(fields) == 2:
		if err := n.Retire(ctx, fields[1]); err != nil {
			return refused(err)
		}
		return "ok " + command
	case fields[0] == "discard" && len(fields) == 2:
		if err := n.Discard(ctx, fields[1]); err != nil {
			return refused(err)
		}
		return "ok " + command
	case fields[0] == "status" && len(fields) == 2:
		return fmt.Sprintf("ok %s: %s", command, n.Status(fields[1]))
	case fields[0] == "check" && len(fields) == 2:
		inspection, err := n.Inspect(ctx, fields[1])
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: user_version %d, integrity %s", command, inspection.UserVersion, inspection.Integrity)
	}
	return fmt.Sprintf("refused %s: unknown command", command)
}
```

<!-- include: service/node/process.go -->
```go
package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

// Standing-bound constants used by RunProcess for each node child process.
const (
	// ConnectionsPerDatabase is 3: the connmgr writer connection plus walship's
	// control and pin connections.
	ConnectionsPerDatabase = 3
	// ReserveDescriptors covers non-database descriptors (pipes, catalog, S3
	// HTTP sockets, and runtime overhead).
	ReserveDescriptors = 32
)

// Process drives a node child process over its stdin/stdout line protocol.
type Process struct {
	Name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines *bufio.Scanner
}

// Reply holds the lines returned by a node Process for one command.
type Reply struct {
	Line     string
	Problems []string
	Bounds   string
	Summary  string
}

// WithinBounds reports whether the node's bounds check after the command passed.
func (r Reply) WithinBounds() bool { return strings.HasSuffix(r.Bounds, " ok") }

// StartProcess re-executes the current binary with args, waits for its "ready"
// line, and returns a handle for sending commands.
func StartProcess(ctx context.Context, name string, args ...string) (*Process, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{Name: name, cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	if !p.lines.Scan() || p.lines.Text() != "ready" {
		return nil, errors.Join(fmt.Errorf("node %s: no ready line, got %q", name, p.lines.Text()), p.lines.Err(), cmd.Process.Kill(), cmd.Wait())
	}
	return p, nil
}

// Send writes one command line to p and reads lines until the trailing "bounds"
// line completes the reply.
func (p *Process) Send(command string) (Reply, error) {
	if _, err := fmt.Fprintln(p.stdin, command); err != nil {
		return Reply{}, err
	}
	var reply Reply
	for p.lines.Scan() {
		line := p.lines.Text()
		switch {
		case strings.HasPrefix(line, "bounds "):
			reply.Bounds = line
			return reply, nil
		case strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "refused "):
			reply.Line = line
		default:
			reply.Problems = append(reply.Problems, line)
		}
	}
	return reply, fmt.Errorf("node %s: output ended after %q: %v", p.Name, command, p.lines.Err())
}

// Kill terminates the child process with SIGKILL and reaps it.
func (p *Process) Kill() error {
	if err := p.cmd.Process.Kill(); err != nil {
		return err
	}
	if err := p.cmd.Wait(); err == nil {
		return fmt.Errorf("node %s: exited cleanly although it was killed", p.Name)
	}
	return nil
}

// Close sends "close", captures the trailing standing-assertion summary line,
// closes stdin, and waits for the child process to exit cleanly.
func (p *Process) Close() (Reply, error) {
	reply, err := p.Send("close")
	if err != nil {
		return reply, err
	}
	if p.lines.Scan() {
		reply.Summary = p.lines.Text()
	}
	return reply, errors.Join(p.stdin.Close(), p.cmd.Wait())
}

// ProcessConfig carries the command-line parameters for a node child process,
// including the optional AckBeforePut mutant flag for Experiment 10.2.
type ProcessConfig struct {
	Name         string
	Work         string
	Fault        string
	Bucket       string
	Prefix       string
	Capacity     int
	AckBeforePut bool
}

// Args renders c into the flag slice expected by an experiment's "-role node"
// entry point. It appends "-ack-before-put" only when AckBeforePut is true so
// experiments without that flag (such as 10.1) receive unchanged arguments.
func (c ProcessConfig) Args() []string {
	args := []string{"-role", "node", "-name", c.Name, "-work", c.Work, "-fault", c.Fault, "-bucket", c.Bucket, "-prefix", c.Prefix, "-capacity", fmt.Sprint(c.Capacity)}
	if c.AckBeforePut {
		args = append(args, "-ack-before-put")
	}
	return args
}

// RunProcess is the entry point for a node child process: it connects to
// SeaweedFS S3 (optionally wrapped in faultstore), opens the shared catalog,
// starts a Node with the course recipe workload and standing bounds assertion,
// and serves commands on stdin/stdout.
func RunProcess(ctx context.Context, c ProcessConfig) error {
	s3, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), c.Bucket, c.Prefix)
	if err != nil {
		return err
	}
	var store walship.Store = s3
	if c.Fault != "" {
		rule, err := faultstore.ParseRule(c.Fault)
		if err != nil {
			return err
		}
		store = faultstore.New(s3, rule)
	}
	cat, err := catalog.Open(ctx, filepath.Join(c.Work, "catalog.db"))
	if err != nil {
		return err
	}
	defer cat.Close()
	workload := recipe.Default()
	n, err := Start(Config{
		Name: c.Name, Dir: filepath.Join(c.Work, c.Name), Catalog: cat, Store: store, Capacity: c.Capacity,
		File:         workload.SQLiteOptions().File,
		Create:       func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
		AckBeforePut: c.AckBeforePut,
	})
	if err != nil {
		return err
	}
	assertion, err := bounds.Start(bounds.Bounds{
		OpenDatabases: c.Capacity, ConnectionsPerDatabase: ConnectionsPerDatabase,
		CacheBytes: bounds.CacheBytes(PlatformSettings().CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: ReserveDescriptors,
	})
	if err != nil {
		return err
	}
	apply := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}
	return Serve(ctx, n, assertion, apply, os.Stdin, os.Stdout)
}
```

<!-- include: service/node/kill.go -->
```go
package node

// Signal delivers SIGKILL to the child process without waiting for it to exit.
// Splitting Signal from Reap lets Experiment 10.2's timer goroutine kill a
// node mid-command while the main goroutine stops sending and reaps it cleanly.
func (p *Process) Signal() error { return p.cmd.Process.Kill() }

// Reap closes stdin and waits for a signaled child process to exit so no
// zombie process remains.
func (p *Process) Reap() error {
	_ = p.stdin.Close()
	return p.cmd.Wait()
}
```

<!-- include: service/node/remote.go -->
```go
package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRefused is matched by RefusedError via errors.Is when a node child process
// returns a "refused …" line-protocol reply.
var ErrRefused = errors.New("the node refused the command")

// RefusedError wraps a node child process's "refused …" reply line so callers
// can distinguish an explicit node refusal from a process crash or broken pipe.
type RefusedError struct {
	Line string
}

// Error returns the verbatim "refused …" line emitted by the node process.
func (e *RefusedError) Error() string { return e.Line }

// Is allows callers to match any RefusedError with errors.Is(err, ErrRefused).
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// Remote invokes relocation, claim, status, and inspect operations over a live
// node child Process, implementing relocate.Participant with strict reply
// parsing and optional per-reply bounds tracking through OnReply.
type Remote struct {
	Process func() *Process
	OnReply func(command string, reply Reply)
}

func (r Remote) call(command string) (string, error) {
	p := r.Process()
	if p == nil {
		return "", fmt.Errorf("node process for %q is not running", command)
	}
	reply, err := p.Send(command)
	if err != nil {
		return "", err
	}
	if r.OnReply != nil {
		r.OnReply(command, reply)
	}
	if strings.HasPrefix(reply.Line, "refused "+command+": ") {
		return "", &RefusedError{Line: reply.Line}
	}
	if reply.Line == "ok "+command {
		return "", nil
	}
	detail, found := strings.CutPrefix(reply.Line, "ok "+command+": ")
	if !found {
		return "", fmt.Errorf("node reply %q does not answer %q", reply.Line, command)
	}
	return detail, nil
}

// scanExactly parses detail with fmt.Sscanf and re-formats the scanned values
// to reject extra tokens, padding, or truncated replies that Sscanf alone accepts.
func scanExactly(detail, format string, values ...any) error {
	if _, err := fmt.Sscanf(detail, format, values...); err != nil {
		return fmt.Errorf("node reply %q: %w", detail, err)
	}
	pointed := make([]any, len(values))
	for i, v := range values {
		switch p := v.(type) {
		case *int64:
			pointed[i] = *p
		case *uint64:
			pointed[i] = *p
		case *int:
			pointed[i] = *p
		case *string:
			pointed[i] = *p
		case *bool:
			pointed[i] = *p
		default:
			return fmt.Errorf("scanExactly: unsupported %T", v)
		}
	}
	if fmt.Sprintf(format, pointed...) != detail {
		return fmt.Errorf("node reply %q does not have the form %q", detail, format)
	}
	return nil
}

// Seal sends "seal <id>" to the child process.
func (r Remote) Seal(ctx context.Context, id string) error {
	detail, err := r.call("seal " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to seal", detail)
	}
	return err
}

// Drain sends "drain <id>" to the child process and parses the drained position and sha256.
func (r Remote) Drain(ctx context.Context, id string) (Drained, error) {
	var d Drained
	detail, err := r.call("drain " + id)
	if err == nil {
		err = scanExactly(detail, "position %d, sha256 %s", &d.Position, &d.SHA256)
	}
	return d, err
}

// Stage sends "stage <id> <epoch>" to the child process and parses the staged position and sha256.
func (r Remote) Stage(ctx context.Context, id string, epoch uint64) (Staged, error) {
	var s Staged
	detail, err := r.call(fmt.Sprintf("stage %s %d", id, epoch))
	if err == nil {
		err = scanExactly(detail, "position %d, sha256 %s", &s.Position, &s.SHA256)
	}
	return s, err
}

// Activate sends "activate <id> <epoch> <position> <digest>" (plus
// "without-fence" when fenceStore is false) and parses the resulting fence slot
// and adoption flag.
func (r Remote) Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (Activated, error) {
	var a Activated
	command := fmt.Sprintf("activate %s %d %d %s", id, epoch, position, digest)
	if !fenceStore {
		command += " without-fence"
	}
	detail, err := r.call(command)
	if err == nil {
		err = scanExactly(detail, "fence at position %d, adopted %t", &a.Fence, &a.Adopted)
	}
	return a, err
}

// Retire sends "retire <id>" to the child process.
func (r Remote) Retire(ctx context.Context, id string) error {
	detail, err := r.call("retire " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to retire", detail)
	}
	return err
}

// Discard sends "discard <id>" to the child process.
func (r Remote) Discard(ctx context.Context, id string) error {
	detail, err := r.call("discard " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to discard", detail)
	}
	return err
}

// Claim sends "claim <id>" to the child process and parses the ClaimResult.
func (r Remote) Claim(ctx context.Context, id string) (ClaimResult, error) {
	var c ClaimResult
	detail, err := r.call("claim " + id)
	if err == nil {
		err = scanExactly(detail, "epoch %d, restored position %d, fence at position %d, attempts %d", &c.Epoch, &c.RestoredFrom, &c.Fence, &c.Attempts)
	}
	return c, err
}

// Status sends "status <id>" to the child process and returns the status string.
func (r Remote) Status(id string) (string, error) {
	return r.call("status " + id)
}

// Inspect sends "check <id>" to the child process and parses user_version and integrity_check.
func (r Remote) Inspect(ctx context.Context, id string) (Inspection, error) {
	var i Inspection
	detail, err := r.call("check " + id)
	if err == nil {
		err = scanExactly(detail, "user_version %d, integrity %s", &i.UserVersion, &i.Integrity)
	}
	return i, err
}

// ParseAck strictly parses an "ok write <id> <mode>: …" reply line into the
// database id and Ack, rejecting malformed fields, padding, or trailing text.
func ParseAck(line string) (string, Ack, error) {
	rest, found := strings.CutPrefix(line, "ok write ")
	head, detail, cut := strings.Cut(rest, ": ")
	fields := strings.Fields(head)
	if !found || !cut || len(fields) != 2 || head != fields[0]+" "+fields[1] {
		return "", Ack{}, fmt.Errorf("not an acknowledgement: %q", line)
	}
	ack := Ack{Mode: AckMode(fields[1])}
	var err error
	switch ack.Mode {
	case LocalDurable:
		err = scanExactly(detail, "acknowledged at epoch %d, user_version %d, committed locally (not yet in the store)", &ack.Epoch, &ack.UserVersion)
	case StoreDurable:
		err = scanExactly(detail, "acknowledged at epoch %d, user_version %d, stored through position %d", &ack.Epoch, &ack.UserVersion, &ack.Position)
	default:
		err = fmt.Errorf("unknown acknowledgement mode in %q", line)
	}
	return fields[0], ack, err
}
```

### 5.9 Relocation coordinator (`service/relocate`)

`relocate` drives the seven numbered steps (`Run`) and implements crash-safe `Resume` and `Rollback` from the catalog's `relocations` row and the store's slot chain.

<!-- include: service/relocate/relocate.go -->
```go
// Package relocate implements the seven-step coordinator that moves a database
// from a source node to a destination node without allowing two writable owners
// at once.
//
// Bumping the catalog epoch alone (step 1) does not stop the source node, which
// does not consult the catalog before writing; step 2 (SealSource) asks the
// cooperative source to refuse writes before step 3 (ShipFinal) drains local
// commits into the store, and step 6 (Activate) puts the destination's fence
// snapshot in the store to bind any uncooperative source.
//
// The coordinator holds no durable state of its own outside the catalog's
// relocations row. After a coordinator crash, node kill, or store fault, a
// fresh Coordinator reads that row and inspects the store's slot chain: if the
// destination's fence is already in the store, recovery can only roll forward
// (adopting the fence); otherwise it can either Resume forward or Rollback by
// draining and re-claiming on the source at a higher epoch.
package relocate

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/walship"
)

// Step numbers the seven phases of a relocation so progress can be persisted in
// the catalog and faults can be injected after each step.
type Step int

// Ordered relocation steps from catalog epoch bump through source retirement.
const (
	// FenceEpoch bumps the catalog epoch to destination and inserts the running
	// relocation row in one SQLite transaction.
	FenceEpoch Step = iota + 1
	// SealSource writes <id>.sealed on the source so later writes are refused.
	SealSource
	// ShipFinal drains unshipped commits from the source, closes it at rest,
	// and records the drained position and sha256 in the catalog.
	ShipFinal
	// Rebuild lists the store at the new epoch on the destination and restores
	// the chain end into <id>.staged.
	Rebuild
	// Verify checks that the staged position and sha256 equal the drained state.
	Verify
	// Activate puts the destination's fence snapshot in the store, installs the
	// local database and .owner file, and updates the catalog.
	Activate
	// RetireSource evicts and deletes the database file and sidecars on the source.
	RetireSource
)

var stepNames = map[Step]string{
	FenceEpoch:   "fence the source: bump the epoch in the catalog",
	SealSource:   "seal the source",
	ShipFinal:    "ship the final segment and close the source at rest",
	Rebuild:      "rebuild at the destination from the store",
	Verify:       "verify the rebuilt hash against the source's",
	Activate:     "activate: put the destination's fence in the store",
	RetireSource: "retire the source",
}

// String formats s as "<number> (<description>)".
func (s Step) String() string {
	if name, ok := stepNames[s]; ok {
		return fmt.Sprintf("%d (%s)", int(s), name)
	}
	return fmt.Sprintf("Step(%d)", int(s))
}

// Steps returns all seven relocation steps in execution order.
func Steps() []Step {
	return []Step{FenceEpoch, SealSource, ShipFinal, Rebuild, Verify, Activate, RetireSource}
}

// Sentinel errors returned by Coordinator.Run, Resume, and Rollback.
var (
	// ErrCrashed is returned when the Coordinator.CrashAfter injection hook fires.
	ErrCrashed = errors.New("relocate: the coordinator stopped (injected crash)")
	// ErrVerifyFailed reports that the destination's staged position or sha256
	// did not match the source's drained state at step 5.
	ErrVerifyFailed = errors.New("relocate: the rebuilt database is not the source's state at the fence point")
	// ErrNotRunning reports that the relocation row is already in a terminal state.
	ErrNotRunning = errors.New("relocate: the relocation is not running")
	// ErrFenceInStore is joined with the roll-forward result when Rollback
	// discovers the destination's fence snapshot already landed in the store.
	ErrFenceInStore = errors.New("relocate: the destination's fence is in the store; the relocation can only roll forward")
	// ErrSupersededBy reports that a newer epoch than the relocation's ToEpoch
	// (owned by another node) already holds a slot in the store.
	ErrSupersededBy = errors.New("relocate: a newer epoch than the relocation's is in the store")
	// ErrMissingRecord reports that the store holds the destination's fence
	// snapshot but the catalog row lacks a matching drained position and sha256.
	ErrMissingRecord = errors.New("relocate: the catalog has no drained position for this relocation")
)

// Participant is the set of operations the coordinator invokes on a source or
// destination node, satisfied by both an in-process *node.Node and node.Remote.
type Participant interface {
	Seal(ctx context.Context, id string) error
	Drain(ctx context.Context, id string) (node.Drained, error)
	Stage(ctx context.Context, id string, epoch uint64) (node.Staged, error)
	Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (node.Activated, error)
	Retire(ctx context.Context, id string) error
	Discard(ctx context.Context, id string) error
	Claim(ctx context.Context, id string) (node.ClaimResult, error)
}

// Coordinator drives a database relocation against Catalog, Store, and
// Participant handles. CrashAfter, SkipFence (the Experiment 10.3 mutant), and
// AfterStep are test and experiment hooks.
type Coordinator struct {
	Catalog    *catalog.Catalog
	Store      walship.Store
	Node       func(name string) Participant
	CrashAfter Step
	SkipFence  bool
	AfterStep  func(step Step)
}

// Outcome names how a Run, Resume, or Rollback call finished.
type Outcome string

// Terminal outcomes returned in Result.Outcome.
const (
	// Done means Run completed all seven steps without interruption.
	Done Outcome = "done"
	// RolledBack means Rollback returned ownership to the source at a new epoch.
	RolledBack Outcome = "rolled back"
	// RolledForward means Resume (or Rollback after a landed fence) finished
	// moving ownership to the destination.
	RolledForward Outcome = "rolled forward"
	// Superseded means another node's higher epoch already took over in the store.
	Superseded Outcome = "superseded"
	// NothingToDo means the relocation row was already finished.
	NothingToDo Outcome = "nothing to do"
)

// Result summarizes the outcome, drained fence-point Position and SHA256,
// accepted Fence position, resulting Epoch, and any best-effort destination
// DiscardErr left when rolling back while the destination is unreachable.
type Result struct {
	Outcome    Outcome
	Position   int64
	SHA256     string
	Fence      int64
	Epoch      uint64
	DiscardErr error
}

// StepError tags a failure returned during a specific relocation Step.
type StepError struct {
	Step Step
	Err  error
}

// Error formats e with the failing step number and description.
func (e *StepError) Error() string { return fmt.Sprintf("relocate: step %s: %v", e.Step, e.Err) }

// Unwrap exposes the underlying error for errors.Is and errors.As.
func (e *StepError) Unwrap() error { return e.Err }

func (c *Coordinator) after(step Step) {
	if c.AfterStep != nil {
		c.AfterStep(step)
	}
}

// Run starts a new relocation of id from source to destination (step 1 in the
// catalog) and executes steps 2 through 7 in order.
func (c *Coordinator) Run(ctx context.Context, id, source, destination string) (Result, error) {
	r, err := c.Catalog.BeginRelocation(ctx, id, source, destination)
	if err != nil {
		return Result{}, &StepError{Step: FenceEpoch, Err: err}
	}
	if c.CrashAfter == FenceEpoch {
		return Result{}, ErrCrashed
	}
	c.after(FenceEpoch)
	return c.forward(ctx, r)
}

func (c *Coordinator) forward(ctx context.Context, r catalog.Relocation) (Result, error) {
	source, destination := c.Node(r.Source), c.Node(r.Destination)
	var staged node.Staged
	result := Result{Epoch: r.ToEpoch}
	for _, step := range Steps()[1:] {
		var position int64
		var digest string
		var err error
		switch step {
		case SealSource:
			// SkipFence is the Experiment 10.3 mutant: skipping seal, drain,
			// store fence, and retire proves the boundary check catches dual writers.
			if !c.SkipFence {
				err = source.Seal(ctx, r.ID)
			}
		case ShipFinal:
			var drained node.Drained
			if c.SkipFence {
				drained, err = c.storeEnd(ctx, r.ID)
			} else {
				drained, err = source.Drain(ctx, r.ID)
			}
			position, digest = drained.Position, drained.SHA256
			r.Position, r.SHA256 = position, digest
		case Rebuild:
			staged, err = destination.Stage(ctx, r.ID, r.ToEpoch)
		case Verify:
			if staged.Position != r.Position || staged.SHA256 != r.SHA256 {
				err = fmt.Errorf("%w: staged position %d sha256 %s, drained position %d sha256 %s", ErrVerifyFailed, staged.Position, staged.SHA256, r.Position, r.SHA256)
			}
		case Activate:
			var activated node.Activated
			activated, err = destination.Activate(ctx, r.ID, r.ToEpoch, r.Position, r.SHA256, !c.SkipFence)
			result.Fence = activated.Fence
		case RetireSource:
			if !c.SkipFence {
				err = source.Retire(ctx, r.ID)
			}
		}
		if err != nil {
			return result, &StepError{Step: step, Err: err}
		}
		if c.CrashAfter == step {
			return result, ErrCrashed
		}
		if err := c.Catalog.AdvanceRelocation(ctx, r.ID, int(step), position, digest); err != nil {
			return result, &StepError{Step: step, Err: err}
		}
		c.after(step)
	}
	result.Outcome, result.Position, result.SHA256 = Done, r.Position, r.SHA256
	return result, c.Catalog.FinishRelocation(ctx, r.ID, catalog.RelocationDone)
}

func (c *Coordinator) storeEnd(ctx context.Context, id string) (node.Drained, error) {
	_, restorable, err := fence.Audit(ctx, c.Store, id)
	if err != nil {
		return node.Drained{}, err
	}
	last, found := restorable.LastPosition()
	if !found {
		return node.Drained{}, fmt.Errorf("relocate: the store holds nothing for %s", id)
	}
	image, err := fence.RestoreAt(restorable, last)
	return node.Drained{Position: last, SHA256: walship.SHA256Hex(image)}, err
}

func newestEpoch(chain []fence.Slot) (uint64, int64) {
	var epoch uint64
	var first int64
	for _, slot := range chain {
		if slot.Epoch > epoch {
			epoch, first = slot.Epoch, slot.First
		}
	}
	return epoch, first
}

func (c *Coordinator) running(ctx context.Context, id string) (catalog.Relocation, uint64, int64, error) {
	r, err := c.Catalog.GetRelocation(ctx, id)
	if err != nil {
		return r, 0, 0, err
	}
	if r.State != catalog.RelocationRunning {
		return r, 0, 0, fmt.Errorf("%w: %s", ErrNotRunning, r)
	}
	chain, err := fence.Chain(ctx, c.Store, id)
	if err != nil {
		return r, 0, 0, err
	}
	epoch, first := newestEpoch(chain)
	return r, epoch, first, nil
}

// Resume finishes an interrupted relocation using only the catalog's
// relocations row and the store's slot chain. If a higher epoch already holds a
// slot, it marks the row superseded; if ToEpoch's fence is already in the store,
// it adopts the fence and retires the source; otherwise it replays idempotent
// steps 2 through 7 to completion.
func (c *Coordinator) Resume(ctx context.Context, id string) (Result, error) {
	r, newest, first, err := c.running(ctx, id)
	if errors.Is(err, ErrNotRunning) {
		return Result{Outcome: NothingToDo, Epoch: r.ToEpoch, Position: r.Position, SHA256: r.SHA256}, nil
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	case newest > r.ToEpoch:
		return Result{Outcome: Superseded}, errors.Join(fmt.Errorf("%w: epoch %d, relocation epoch %d", ErrSupersededBy, newest, r.ToEpoch), c.Catalog.FinishRelocation(ctx, id, catalog.RelocationSuperseded))
	case newest == r.ToEpoch:
		return c.adopt(ctx, r, first)
	}
	result, err := c.forward(ctx, r)
	if err == nil {
		result.Outcome = RolledForward
	}
	return result, err
}

func (c *Coordinator) adopt(ctx context.Context, r catalog.Relocation, fencePosition int64) (Result, error) {
	if r.Position == 0 || r.SHA256 == "" || r.Position+1 != fencePosition {
		return Result{}, fmt.Errorf("%w: %s, fence at %d", ErrMissingRecord, r, fencePosition)
	}
	activated, err := c.Node(r.Destination).Activate(ctx, r.ID, r.ToEpoch, r.Position, r.SHA256, true)
	if err != nil {
		return Result{}, &StepError{Step: Activate, Err: err}
	}
	if err := c.Catalog.AdvanceRelocation(ctx, r.ID, int(Activate), 0, ""); err != nil {
		return Result{}, &StepError{Step: Activate, Err: err}
	}
	if err := c.Node(r.Source).Retire(ctx, r.ID); err != nil {
		return Result{}, &StepError{Step: RetireSource, Err: err}
	}
	if err := c.Catalog.AdvanceRelocation(ctx, r.ID, int(RetireSource), 0, ""); err != nil {
		return Result{}, &StepError{Step: RetireSource, Err: err}
	}
	result := Result{Outcome: RolledForward, Position: r.Position, SHA256: r.SHA256, Fence: activated.Fence, Epoch: r.ToEpoch}
	return result, c.Catalog.FinishRelocation(ctx, r.ID, catalog.RelocationDone)
}

// Rollback cancels an interrupted relocation before the destination's fence
// lands in the store: it seals and drains the source (preserving every local
// acknowledgement), lets the source Claim a higher epoch and fence the store
// against any zombie destination, marks the relocation rolled-back in the
// catalog, and then asks the destination to Discard its staged copy best-effort
// (recording any unreachable error in Result.DiscardErr without wedging the
// relocation). If the destination's fence is already in the store, Rollback
// rolls forward instead and returns ErrFenceInStore.
func (c *Coordinator) Rollback(ctx context.Context, id string) (Result, error) {
	r, newest, _, err := c.running(ctx, id)
	if errors.Is(err, ErrNotRunning) {
		return Result{Outcome: NothingToDo}, nil
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	// A prior Rollback may have completed source.Claim before crashing prior to
	// FinishRelocation; recognize the source's own higher epoch and finish as
	// rolled-back rather than reporting Superseded.
	case newest > r.ToEpoch && c.reclaimedBySource(ctx, r, newest):
		result := Result{Outcome: RolledBack, Epoch: newest}
		if err := c.Catalog.FinishRelocation(ctx, id, catalog.RelocationRolledBack); err != nil {
			return result, err
		}
		result.DiscardErr = c.Node(r.Destination).Discard(ctx, id)
		return result, nil
	case newest > r.ToEpoch:
		return Result{Outcome: Superseded}, errors.Join(fmt.Errorf("%w: epoch %d, relocation epoch %d", ErrSupersededBy, newest, r.ToEpoch), c.Catalog.FinishRelocation(ctx, id, catalog.RelocationSuperseded))
	case newest == r.ToEpoch:
		result, err := c.Resume(ctx, id)
		return result, errors.Join(ErrFenceInStore, err)
	}
	source := c.Node(r.Source)
	if err := source.Seal(ctx, id); err != nil {
		return Result{}, &StepError{Step: SealSource, Err: err}
	}
	drained, err := source.Drain(ctx, id)
	if err != nil {
		return Result{}, &StepError{Step: ShipFinal, Err: err}
	}
	claimed, err := source.Claim(ctx, id)
	if err != nil {
		return Result{}, fmt.Errorf("relocate: roll back %s: the source's claim: %w", id, err)
	}
	result := Result{Outcome: RolledBack, Position: drained.Position, SHA256: drained.SHA256, Fence: claimed.Fence, Epoch: claimed.Epoch}
	// Finish the catalog record before contacting the destination: once the
	// source's higher-epoch fence is in the store, a leftover .staged file on a
	// down destination cannot activate and must not keep relocations.state running.
	if err := c.Catalog.FinishRelocation(ctx, id, catalog.RelocationRolledBack); err != nil {
		return result, err
	}
	result.DiscardErr = c.Node(r.Destination).Discard(ctx, id)
	return result, nil
}

func (c *Coordinator) reclaimedBySource(ctx context.Context, r catalog.Relocation, newest uint64) bool {
	record, err := c.Catalog.Get(ctx, r.ID)
	return err == nil && record.Owner == r.Source && record.Epoch == newest
}
```

### 5.10 Shared experiment harness (`service/lab`)

`lab` provides the reset, process-management, reply/bounds tracking, recipe-image matching, concurrent fleet inspection (`InspectFleet`, `ServeChecker`, `ParsePass`), `CountingStore`, and `TruncateAndHash` helpers shared by Experiments 10.2, 10.3, and 10.4.

<!-- include: service/lab/lab.go -->
```go
// Package lab owns the shared experiment harness used by Experiments 10.2,
// 10.3, and 10.4: resetting scratch work directories and S3 prefixes, managing
// node child processes, tracking problem and standing-bounds lines across
// replies, matching recovered images against recipe step hashes, running the
// concurrent fleet checker subprocess, counting store operations, and
// quiescing closed databases with a TRUNCATE checkpoint.
package lab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Env bundles one experiment's scratch work directory, S3 store, shared
// catalog, template node ProcessConfig, precomputed recipe step hashes, and
// reply Tracker.
type Env struct {
	Ctx     context.Context
	Work    string
	Store   *s3store.Store
	Catalog *catalog.Catalog
	Process node.ProcessConfig
	Hashes  []recipe.FileHashes
	Tracker Tracker
}

// Connect connects to the SeaweedFS S3 store configured in the environment
// using process.Bucket and process.Prefix.
func Connect(ctx context.Context, process node.ProcessConfig) (*s3store.Store, error) {
	return s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), process.Bucket, process.Prefix)
}

// New prepares an Env for experiment name under _work/<name>/ and precomputes
// the recipe step hashes used to verify recovered and rebuilt databases.
func New(ctx context.Context, name string, store *s3store.Store, process node.ProcessConfig) (*Env, error) {
	work, err := sqlitex.WorkDir(name)
	if err != nil {
		return nil, err
	}
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		return nil, err
	}
	process.Work = work
	return &Env{Ctx: ctx, Work: work, Store: store, Process: process, Hashes: hashes}, nil
}

// ShownWork returns e.Work relative to the repository root when possible so
// INFO lines never print machine-specific absolute paths.
func (e *Env) ShownWork() string {
	if root, err := sqlitex.RepoRoot(); err == nil {
		if relative, err := filepath.Rel(root, e.Work); err == nil {
			return relative
		}
	}
	return e.Work
}

// Reset closes any open catalog, wipes e.Work and the experiment's S3 prefix,
// opens a fresh catalog, and registers ids with node.PlatformSettings().
func (e *Env) Reset(ids []string) error {
	if e.Catalog != nil {
		if err := e.Catalog.Close(); err != nil {
			return err
		}
		e.Catalog = nil
	}
	entries, err := os.ReadDir(e.Work)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(e.Work, entry.Name())); err != nil {
			return err
		}
	}
	if _, err := e.Store.WipeInBatches(e.Ctx); err != nil {
		return err
	}
	cat, err := catalog.Open(e.Ctx, filepath.Join(e.Work, "catalog.db"))
	if err != nil {
		return err
	}
	e.Catalog = cat
	return cat.RegisterAll(e.Ctx, ids, node.PlatformSettings())
}

// Close closes the open catalog handle, if any.
func (e *Env) Close() error {
	if e.Catalog == nil {
		return nil
	}
	err := e.Catalog.Close()
	e.Catalog = nil
	return err
}

// Config returns a copy of e.Process specialized for node name and optional
// faultstore rule string.
func (e *Env) Config(name, fault string) node.ProcessConfig {
	config := e.Process
	config.Name, config.Fault = name, fault
	return config
}

// Start launches a child node process with config and waits for its "ready" line.
func (e *Env) Start(config node.ProcessConfig) (*node.Process, error) {
	return node.StartProcess(e.Ctx, config.Name, config.Args()...)
}

// NodeDir returns the local directory owned by node name under e.Work.
func (e *Env) NodeDir(name string) string { return filepath.Join(e.Work, name) }

// DatabasePath returns the local path to database id inside nodeName's directory.
func (e *Env) DatabasePath(nodeName, id string) string {
	return filepath.Join(e.Work, nodeName, id+".db")
}

// Send dispatches command to node process p and records its reply in e.Tracker.
func (e *Env) Send(p *node.Process, command string) (node.Reply, error) {
	reply, err := p.Send(command)
	if err == nil {
		e.Tracker.Note(p.Name, reply)
	}
	return reply, err
}

// Stop cleanly closes node process p, records its closing reply in e.Tracker,
// and appends a problem if the close reply did not start with "ok ".
func (e *Env) Stop(p *node.Process) (node.Reply, error) {
	reply, err := p.Close()
	if err == nil {
		e.Tracker.Note(p.Name, reply)
		if !strings.HasPrefix(reply.Line, "ok ") {
			e.Tracker.Problems = append(e.Tracker.Problems, p.Name+": "+reply.Line)
		}
	}
	return reply, err
}

// Ids generates count zero-padded database ids ("<prefix>-0001" .. "<prefix>-NNNN").
func Ids(prefix string, count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%04d", prefix, i+1)
	}
	return ids
}

// Tracker aggregates bounds checks, background node problems, and out-of-bounds
// lines observed across all child processes in an experiment run.
type Tracker struct {
	Replies    int
	Problems   []string
	OverBounds []string
}

// Note records one interactive command reply from node name.
func (t *Tracker) Note(name string, reply node.Reply) {
	t.Replies++
	for _, problem := range reply.Problems {
		t.Problems = append(t.Problems, name+": "+problem)
	}
	if !reply.WithinBounds() {
		t.OverBounds = append(t.OverBounds, name+": "+reply.Bounds)
	}
}

// NoteOutput parses the captured stdout of a non-interactive (for example,
// strace-killed) node process, discarding any trailing incomplete line left by
// SIGKILL, recording bounds and problem lines, and returning all complete
// "ok …"/"refused …" replies.
func (t *Tracker) NoteOutput(name, stdout string) []string {
	var replies []string
	lines := strings.Split(stdout, "\n")
	// A killed process may die mid-line; ignore a trailing line with no newline
	// so partial stdout never turns into a false problem or acknowledgement.
	if len(lines) > 0 && !strings.HasSuffix(stdout, "\n") {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		switch {
		case line == "" || line == "ready" || strings.HasPrefix(line, "standing assertion: "):
		case strings.HasPrefix(line, "bounds "):
			t.Replies++
			if !strings.HasSuffix(line, " ok") {
				t.OverBounds = append(t.OverBounds, name+": "+line)
			}
		case strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "refused "):
			replies = append(replies, line)
		default:
			t.Problems = append(t.Problems, name+": "+line)
		}
	}
	return replies
}

// Recovered summarizes matching a closed database image against the recipe's
// known step hashes.
type Recovered struct {
	UserVersion int
	Match       recipe.Match
	RecipeState bool
}

// MatchFile verifies that path is at rest (no -wal/-shm sidecars), reads its
// bytes, and matches them against e.Hashes.
func (e *Env) MatchFile(path string) (Recovered, error) {
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		return Recovered{}, err
	}
	if !status.AtRest {
		return Recovered{}, fmt.Errorf("lab: %s is not at rest: %s", path, status)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return Recovered{}, err
	}
	return e.MatchImage(image)
}

// MatchImage matches raw database bytes image against e.Hashes.
func (e *Env) MatchImage(image []byte) (Recovered, error) {
	return MatchImage(e.Hashes, image)
}

// AtRestSHA256 verifies that database id on nodeName is at rest and returns its
// SHA-256 hex digest.
func (e *Env) AtRestSHA256(nodeName, id string) (string, error) {
	status, err := connmgr.CheckAtRest(e.DatabasePath(nodeName, id))
	if err != nil {
		return "", err
	}
	if !status.AtRest {
		return "", fmt.Errorf("lab: %s on node %s is not at rest: %s", id, nodeName, status)
	}
	return status.SHA256, nil
}
```

<!-- include: service/lab/fleet.go -->
```go
package lab

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

// MatchImage hashes raw SQLite database bytes image against hashes, decodes
// user_version from page-1 header bytes 60..63, and reports RecipeState true
// only when the matched step equals user_version and either the bytes match
// exactly or only the change counter differs with bytes 24..27 == 92..95.
func MatchImage(hashes []recipe.FileHashes, image []byte) (Recovered, error) {
	if len(image) < 100 {
		return Recovered{}, errors.New("lab: an image shorter than the database header")
	}
	match := recipe.MatchHashes(hashes, recipe.HashImage(image))
	// Bytes 60..63 of the SQLite header store PRAGMA user_version in big-endian.
	version := int(image[60])<<24 | int(image[61])<<16 | int(image[62])<<8 | int(image[63])
	ok := match.Found() && match.Step == version && (match.Exact || recipe.ChangeCountersAgree(image))
	return Recovered{UserVersion: version, Match: match, RecipeState: ok}, nil
}

// DatabasesIn lists every key under "dbs/" in store, validates that every key
// has the fenced slot layout "dbs/<id>/log/<pos>", and returns the sorted list
// of distinct database ids.
func DatabasesIn(ctx context.Context, store walship.Store) ([]string, error) {
	keys, err := store.List(ctx, "dbs/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, key := range keys {
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[0] != "dbs" || parts[2] != "log" {
			return nil, fmt.Errorf("lab: unexpected key %q in the store", key)
		}
		seen[parts[1]] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// FleetReport summarizes one full-store inspection pass across all databases.
type FleetReport struct {
	Databases     int
	RestorePoints int
	BytesRead     int64
	Failed        []fence.Inspection
}

// InspectFleet discovers every database in store and runs fence.Inspect in
// parallel across workers goroutines, rebuilding every published restore point
// in memory and verifying that each rebuilt image is a valid recipe state.
func InspectFleet(ctx context.Context, store walship.Store, hashes []recipe.FileHashes, workers int) (FleetReport, error) {
	ids, err := DatabasesIn(ctx, store)
	if err != nil {
		return FleetReport{}, err
	}
	report := FleetReport{Databases: len(ids)}
	inspections := make([]fence.Inspection, len(ids))
	errs := make([]error, len(ids))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(ids) {
					return
				}
				inspections[i], errs[i] = fence.Inspect(ctx, store, ids[i], func(position int64, image []byte) error {
					state, err := MatchImage(hashes, image)
					if err == nil && !state.RecipeState {
						err = fmt.Errorf("the rebuilt state is not a recipe state (user_version %d, %s)", state.UserVersion, state.Match)
					}
					return err
				})
			}
		}()
	}
	wg.Wait()
	for i, inspection := range inspections {
		if errs[i] != nil {
			return report, fmt.Errorf("inspect %s: %w", ids[i], errs[i])
		}
		report.RestorePoints += inspection.RestorePoints
		report.BytesRead += inspection.BytesRead
		if inspection.Problem != "" {
			report.Failed = append(report.Failed, inspection)
		}
	}
	return report, nil
}

// ServeChecker runs Experiment 10.4's standalone checker subprocess over the
// same line protocol as node.Serve ("pass" and "close"), enforcing standing
// bounds with OpenDatabases=0 (since the checker never opens SQLite files).
func ServeChecker(ctx context.Context, store walship.Store, workers int, in io.Reader, out io.Writer) error {
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		return err
	}
	assertion, err := bounds.Start(bounds.Bounds{ConnectionsPerDatabase: node.ConnectionsPerDatabase, CacheBytes: bounds.CacheBytes(node.PlatformSettings().CacheSize, recipe.Default().SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors})
	if err != nil {
		return err
	}
	lines := bufio.NewScanner(in)
	fmt.Fprintln(out, "ready")
	for lines.Scan() {
		command := strings.TrimSpace(lines.Text())
		reply := "refused " + command + ": unknown command"
		switch command {
		case "pass":
			report, err := InspectFleet(ctx, store, hashes, workers)
			if err != nil {
				reply = "refused pass: error: " + err.Error()
				break
			}
			for _, failed := range report.Failed {
				fmt.Fprintln(out, "failed "+failed.String())
			}
			reply = fmt.Sprintf("ok pass: databases %d, restore points %d, failed %d, bytes read %d", report.Databases, report.RestorePoints, len(report.Failed), report.BytesRead)
		case "close":
			reply = "ok close"
		}
		fmt.Fprintln(out, reply)
		sample, err := assertion.Check()
		status := "ok"
		if err != nil {
			status = "EXCEEDED: " + err.Error()
		}
		fmt.Fprintf(out, "bounds descriptors=%d memory=%d %s\n", sample.Descriptors, sample.MemoryGrowth, status)
		if command == "close" {
			fmt.Fprintln(out, assertion.Summary())
			return nil
		}
	}
	return lines.Err()
}

// CheckerPass holds the parsed counts and failure descriptions from one "pass"
// reply emitted by ServeChecker.
type CheckerPass struct {
	Databases     int
	RestorePoints int
	Failed        int
	BytesRead     int64
	FailedLines   []string
}

// ParsePass strictly parses a ServeChecker "pass" reply and verifies that the
// number of preceding "failed …" lines matches the reported failure count.
func ParsePass(reply node.Reply) (CheckerPass, error) {
	var pass CheckerPass
	detail, found := strings.CutPrefix(reply.Line, "ok pass: ")
	if !found {
		return pass, fmt.Errorf("checker reply %q", reply.Line)
	}
	if err := scan(detail, "databases %d, restore points %d, failed %d, bytes read %d", &pass.Databases, &pass.RestorePoints, &pass.Failed, &pass.BytesRead); err != nil {
		return pass, err
	}
	for _, line := range reply.Problems {
		if !strings.HasPrefix(line, "failed ") {
			return pass, fmt.Errorf("checker line %q", line)
		}
		pass.FailedLines = append(pass.FailedLines, strings.TrimPrefix(line, "failed "))
	}
	if len(pass.FailedLines) != pass.Failed {
		return pass, fmt.Errorf("checker reported %d failures and listed %d", pass.Failed, len(pass.FailedLines))
	}
	return pass, nil
}

func scan(detail, format string, values ...any) error {
	if _, err := fmt.Sscanf(detail, format, values...); err != nil {
		return fmt.Errorf("reply %q: %w", detail, err)
	}
	shown := make([]any, len(values))
	for i, v := range values {
		switch p := v.(type) {
		case *int:
			shown[i] = *p
		case *int64:
			shown[i] = *p
		default:
			return fmt.Errorf("scan: unsupported %T", v)
		}
	}
	if fmt.Sprintf(format, shown...) != detail {
		return fmt.Errorf("reply %q does not have the form %q", detail, format)
	}
	return nil
}

// CountingStore wraps a walship.Store and atomically counts Get, bytes read,
// PutIfAbsent, and List calls across concurrent rebuild workers.
type CountingStore struct {
	walship.Store
	gets  atomic.Int64
	bytes atomic.Int64
	puts  atomic.Int64
	lists atomic.Int64
}

// Get forwards to Store.Get and increments the object and byte counters on success.
func (c *CountingStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := c.Store.Get(ctx, key)
	if err == nil {
		c.gets.Add(1)
		c.bytes.Add(int64(len(data)))
	}
	return data, err
}

// PutIfAbsent increments the put counter and forwards to Store.PutIfAbsent.
func (c *CountingStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	return c.Store.PutIfAbsent(ctx, key, data)
}

// List increments the listing counter and forwards to Store.List.
func (c *CountingStore) List(ctx context.Context, prefix string) ([]string, error) {
	c.lists.Add(1)
	return c.Store.List(ctx, prefix)
}

// Counts returns the total successful gets, bytes read, puts, and listings so far.
func (c *CountingStore) Counts() (gets, bytes, puts, lists int64) {
	return c.gets.Load(), c.bytes.Load(), c.puts.Load(), c.lists.Load()
}

// Quiesced holds the at-rest SHA256 and PRAGMA wal_checkpoint(TRUNCATE) result
// recorded by TruncateAndHash.
type Quiesced struct {
	SHA256     string
	Checkpoint sqlitex.CheckpointResult
}

// TruncateAndHash opens path with platform settings, runs
// PRAGMA wal_checkpoint(TRUNCATE), closes the handle, and returns the at-rest
// SHA-256 digest. After a clean writer shutdown sqlite3WalClose
// (src/wal.c:2507-2571) has already checkpointed and removed -wal, so
// Checkpoint.LogFrames is expected to be 0.
func TruncateAndHash(ctx context.Context, path string) (Quiesced, error) {
	options := recipe.Default().SQLiteOptions()
	options.Conn = node.PlatformSettings()
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return Quiesced{}, err
	}
	result, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate)
	if err = errors.Join(err, db.Close()); err != nil {
		return Quiesced{}, err
	}
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		return Quiesced{}, err
	}
	if !status.AtRest {
		return Quiesced{}, fmt.Errorf("lab: %s is not at rest after the checkpoint: %s", path, status)
	}
	return Quiesced{SHA256: status.SHA256, Checkpoint: result}, nil
}
```

### 5.11 Experiment 10.1 — `one-writer`

- **Claim (invariant 1 — at most one authorized writer per database):** With write-once position slots (`If-None-Match: *`) in the store **and** writers that append only after their own last object and stop if a newer epoch is listed, once a newer epoch's object is accepted no older epoch gets an object accepted, no two epochs are accepted for overlapping positions, and no accepted slot is overwritten—across every interleaving of two nodes (`claim`, `fence`, `fence-with-stale-list`, `write`, `ship`, `crash`, `restart`, `restart-with-stale-list`) up to depth `16` (within bounds: `2` nodes, `<= 3` epochs, local commits up to stream position `5`) and `2,000` seeded random walks of `60` steps. Removing either half of the fencing rule (`store-accepts-every-put` or `resume-from-global-end`) causes the model checker to find concrete counterexamples. Two real service processes against SeaweedFS S3 agree with the model at every command step (`13/13`), including stale-list takeover retry, fencing an active stale owner, fencing a `SIGKILL`ed and restarted stale owner holding its `.owner` belief file, keeping store-durable acknowledgements, and losing an unshipped local-durable acknowledgement on takeover.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).

<!-- include: experiments/10.1-one-writer/main.go -->
```go
// Experiment 10.1 tests the single-writer ownership invariant in two parts.
//
// Part A runs the finite-state ownership model (service/model) exhaustively up
// to a bounded depth plus seeded random walks, checking that slot exclusivity
// in the store combined with the writer's epoch/append rule prevents any fenced
// epoch from getting a commit accepted. It also runs two broken mutants
// (store-accepts-every-put and resume-from-global-end) as self-tests to show
// each half of the fencing rule is necessary and that the checker catches
// violations.
//
// Part B drives two real node child processes (a and b) sharing a WAL catalog
// and a SeaweedFS S3 store through a 15-step stale-owner scenario: a stale
// listing during takeover (fault stale-list:/log/:1), an active stale owner
// fenced on ship, a killed stale owner restarting with its persisted .owner
// belief file and getting fenced on open, and the contrast between
// store-durable acknowledgements (kept) and an unshipped local-durable
// acknowledgement (lost on takeover). Every command step is cross-checked
// against the model and against the standing descriptor and memory bounds.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.1
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/model"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

var (
	depth      = flag.Int("depth", 16, "exhaustive search depth of the model")
	seed       = flag.Uint64("seed", 1, "seed of the scheduler's action order and random walks")
	walks      = flag.Int("walks", 2000, "seeded random walks past the exhaustive depth")
	walkLength = flag.Int("walk-length", 60, "steps per random walk")
	mutant     = flag.String("mutant", string(model.Correct), "model under test: correct, store-accepts-every-put, resume-from-global-end")
	modelOnly  = flag.Bool("model-only", false, "run part A only (no store needed)")
	role       = flag.String("role", "", "internal: run as a service node")
	process    node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10a", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-1", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 4, "databases a node keeps open")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.1" runs both.
// part labels the claims made while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, modelRuns, modelTraces, steps, modelSteps, closes, problems, slots, catalog, acks, infoRun, infoBounds *results.Table
	}
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.modelRuns = set.Table("model_runs", "seq", "model", "role", "depth", "seed", "interleavings", "distinct_states", "violating", "walks", "walk_steps", "walk_violating", "violation")
	tables.modelTraces = set.Table("model_traces", "model", "step_no", "action", "outcome")
	tables.steps = set.Table("steps", "step", "node", "command", "reply", "descriptors", "model_agrees")
	tables.modelSteps = set.Table("model_steps", "step", "database", "seq", "action", "outcome")
	tables.closes = set.Table("closes", "node", "reply", "summary")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.slots = set.Table("slots", "database", "source", "first", "last", "epoch", "kind")
	tables.catalog = set.Table("catalog", "database", "owner", "epoch", "last_shipped", "schema_version", "location", "restored_position", "restored_state", "file_equals_store")
	tables.acks = set.Table("acks", "seq", "database", "mode", "epoch", "user_version", "position", "in_store")
	tables.infoRun = set.Table("info_run", "name", "value")
	tables.infoBounds = set.Table("info_bounds", "step", "node", "bounds")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

var settingSeq, modelSeq int

func partA() {
	part = "A"
	setting("model_nodes", model.Nodes)
	setting("model_max_epoch", model.MaxEpoch)
	setting("model_max_local_position", model.MaxLocal)
	setting("model_seed", *seed)
	under, err := model.ParseMutant(*mutant)
	if err != nil {
		fatal("-mutant", err)
	}
	report := model.Explore(under, *depth, *seed, *walks, *walkLength)
	recordModel(report, "under test")
	check(report.Violating == 0 && report.WalkViolating == 0 && report.Shortest == nil, fmt.Sprintf("model %s: no interleaving up to depth %d and no random walk lets a fenced epoch get a commit accepted", report.Mutant, *depth))
	// Self-tests: disable each half of the fencing rule in turn so the
	// experiment proves the checker can fail when either guarantee is missing.
	for _, m := range []model.Mutant{model.StoreAcceptsEveryPut, model.ResumeFromGlobalEnd} {
		if m == under {
			continue
		}
		self := model.Explore(m, *depth, *seed, 0, 0)
		recordModel(self, "self-test")
		check(self.Violating > 0 && self.Shortest != nil, fmt.Sprintf("self-test: with %s the checker finds the violation", m))
	}
}

// recordModel writes one exploration and its shortest violating interleaving,
// whose lines model.Explore formats as "%2d. %-28s %s" (step, action, outcome).
func recordModel(report model.Report, role string) {
	modelSeq++
	var violation any
	if report.Shortest != nil {
		violation = report.Violation
	}
	tables.modelRuns.Row(modelSeq, report.Mutant, role, report.Depth, *seed, report.Interleavings, report.States, report.Violating, report.Walks, report.WalkSteps, report.WalkViolating, violation)
	for i, line := range report.Shortest {
		_, rest, _ := strings.Cut(line, ". ")
		tables.modelTraces.Row(report.Mutant, i+1, strings.TrimSpace(rest[:min(28, len(rest))]), strings.TrimSpace(rest[min(28, len(rest)):]))
	}
}

type step struct {
	node, command string
	model         map[string]string
}

// scenario scripts two real nodes alongside the model actions for each step:
// steps 1-7 exercise stale-list retry, active stale-owner fencing, and loss of
// an unshipped local-durable commit on db-0001; steps 8-15 exercise SIGKILL and
// restart of a stale owner holding a persisted belief file on db-0002 while
// keeping ownership of db-0001.
var scenario = []step{
	{"a", "claim db-0001", map[string]string{"db-0001": "A claim, A fence"}},
	{"a", "write db-0001 store-durable", map[string]string{"db-0001": "A write, A ship"}},
	{"a", "write db-0001 local-durable", map[string]string{"db-0001": "A write"}},
	{"b", "claim db-0001", map[string]string{"db-0001": "B claim, B fence-with-stale-list, B fence"}},
	{"a", "write db-0001 store-durable", map[string]string{"db-0001": "A write, A ship"}},
	{"a", "write db-0001 local-durable", map[string]string{"db-0001": "A write"}},
	{"b", "write db-0001 store-durable", map[string]string{"db-0001": "B write, B ship"}},
	{"b", "claim db-0002", map[string]string{"db-0002": "B claim, B fence"}},
	{"b", "write db-0002 store-durable", map[string]string{"db-0002": "B write, B ship"}},
	{"a", "claim db-0002", map[string]string{"db-0002": "A claim, A fence"}},
	{"b", "kill", map[string]string{"db-0001": "B crash", "db-0002": "B crash"}},
	{"b", "restart", map[string]string{"db-0001": "B restart", "db-0002": "B restart"}},
	{"b", "write db-0002 store-durable", map[string]string{"db-0002": "B write, B ship"}},
	{"b", "write db-0001 store-durable", map[string]string{"db-0001": "B write, B ship"}},
	{"a", "write db-0002 store-durable", map[string]string{"db-0002": "A write, A ship"}},
}

var databases = []string{"db-0001", "db-0002"}

type ack struct {
	database           string
	mode               node.AckMode
	epoch, userVersion int
	position           int64
}

func lastNumber(text string) int64 {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r < '0' || r > '9' })
	var n int64 = -1
	if len(fields) > 0 {
		fmt.Sscan(fields[len(fields)-1], &n)
	}
	return n
}

func agrees(reply string, outcomes []string) bool {
	last := outcomes[len(outcomes)-1]
	if strings.HasPrefix(reply, "refused ") || !strings.HasPrefix(last, "ok") {
		return strings.HasPrefix(reply, "refused ") && strings.HasPrefix(last, "refused")
	}
	fences := 0
	for _, o := range outcomes {
		if strings.Contains(o, "fence at") || strings.HasSuffix(o, ", retry") {
			fences++
		}
	}
	if at := strings.Index(reply, "fence at position"); at >= 0 {
		return reply[at:] == fmt.Sprintf("fence at position %d, attempts %d", lastNumber(last), fences)
	}
	if strings.Contains(reply, "stored through position") {
		return lastNumber(reply) == lastNumber(last)
	}
	return true
}

func startNode(ctx context.Context, work, name, fault string) *node.Process {
	config := process
	config.Name, config.Work, config.Fault = name, work, fault
	p, err := node.StartProcess(ctx, name, config.Args()...)
	if err != nil {
		fatal("start node "+name, err)
	}
	return p
}

func partB(ctx context.Context, store *s3store.Store, work string) {
	part = "B"
	if _, err := store.Wipe(ctx); err != nil {
		fatal("wipe the store prefix", err)
	}
	shown := work
	if root, err := sqlitex.RepoRoot(); err == nil {
		if relative, err := filepath.Rel(root, work); err == nil {
			shown = relative
		}
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, shown)
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", shown)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("catalog", err)
	}
	defer cat.Close()
	for _, id := range databases {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("register", err)
		}
	}
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	setting("databases", strings.Join(databases, " "))
	for _, pragma := range strings.Fields(node.PlatformSettings().String()) {
		name, value, _ := strings.Cut(pragma, "=")
		setting(name, value)
	}
	setting("open_databases_per_node", process.Capacity)
	setting("fault_on_node_b", "stale-list:/log/:1")
	nodes := map[string]*node.Process{"a": startNode(ctx, work, "a", ""), "b": startNode(ctx, work, "b", "stale-list:/log/:1")}
	states := map[string]model.State{}
	for _, id := range databases {
		states[id] = model.Initial()
	}
	var acks []ack
	var problems, overBounds []string
	commands, agreed, sampled := 0, 0, 0
	for i, s := range scenario {
		var reply node.Reply
		var descriptors any
		switch s.command {
		case "kill":
			if err := nodes[s.node].Kill(); err != nil {
				fatal("kill", err)
			}
		case "restart":
			nodes[s.node] = startNode(ctx, work, s.node, "")
		default:
			if reply, err = nodes[s.node].Send(s.command); err != nil {
				fatal("send", err)
			}
			tables.infoBounds.Row(i+1, s.node, reply.Bounds)
			var n int
			if _, err := fmt.Sscanf(reply.Bounds, "bounds descriptors=%d", &n); err == nil {
				descriptors = n
			}
			sampled++
			problems = append(problems, reply.Problems...)
			if !reply.WithinBounds() {
				overBounds = append(overBounds, reply.Bounds)
			}
		}
		var modelAgrees any
		for _, id := range databases {
			if _, ok := s.model[id]; !ok {
				continue
			}
			actions, err := model.ParseScript(s.model[id])
			if err != nil {
				fatal("script", err)
			}
			var outcomes []string
			for j, a := range actions {
				next, outcome, enabled := model.Step(states[id], model.Correct, a)
				if !enabled {
					outcome = "refused: not enabled"
				} else {
					states[id] = next
				}
				outcomes = append(outcomes, outcome)
				tables.modelSteps.Row(i+1, id, j+1, a.String(), outcome)
			}
			if reply.Line != "" {
				commands++
				ok := agrees(reply.Line, outcomes)
				modelAgrees = ok
				if ok {
					agreed++
				}
			}
		}
		var line any
		if reply.Line != "" {
			line = reply.Line
		}
		tables.steps.Row(i+1, s.node, s.command, line, descriptors, modelAgrees)
		var a ack
		if n, _ := fmt.Sscanf(reply.Line, "ok write %s %s acknowledged at epoch %d, user_version %d,", &a.database, &a.mode, &a.epoch, &a.userVersion); n == 4 {
			a.mode = node.AckMode(strings.TrimSuffix(string(a.mode), ":"))
			if a.mode == node.StoreDurable {
				a.position = lastNumber(reply.Line)
			}
			acks = append(acks, a)
		} else if strings.HasPrefix(reply.Line, "ok write ") {
			fatal("parse acknowledgement", fmt.Errorf("%q", reply.Line))
		}
	}
	check(commands == agreed, fmt.Sprintf("the real processes and the model agree at every command step (%d of %d)", agreed, commands))
	for _, name := range []string{"a", "b"} {
		reply, err := nodes[name].Close()
		if err != nil {
			fatal("close node "+name, err)
		}
		tables.closes.Row(name, reply.Line, reply.Summary)
		sampled++
		problems = append(problems, reply.Problems...)
		if !reply.WithinBounds() {
			overBounds = append(overBounds, reply.Bounds)
		}
	}
	recordProblems(problems, overBounds)
	check(len(problems) == 0, "no node reported a problem (a failed final ship, an eviction that did not leave the shipped state at rest, an I/O error)")
	check(len(overBounds) == 0, fmt.Sprintf("after each of the %d commands the node's descriptors and memory stayed within the standing bounds", sampled))
	verify(ctx, store, cat, work, states, acks)
}

func recordProblems(problems, overBounds []string) {
	seq := 0
	for _, kind := range []struct {
		name  string
		lines []string
	}{{"problem", problems}, {"over bounds", overBounds}} {
		for _, line := range kind.lines {
			seq++
			tables.problems.Row(seq, kind.name, line)
		}
	}
}

func verify(ctx context.Context, store *s3store.Store, cat *catalog.Catalog, work string, states map[string]model.State, acks []ack) {
	ackSeq := 0
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	part = "C"
	for _, id := range databases {
		chain, restorable, err := fence.Audit(ctx, store, id)
		if err != nil {
			fatal("audit "+id, err)
		}
		var slots []string
		for _, slot := range chain {
			slots = append(slots, fmt.Sprintf("positions %d-%d epoch %d %s", slot.First, slot.Last, slot.Epoch, slot.Kind))
			tables.slots.Row(id, "store", slot.First, slot.Last, slot.Epoch, slot.Kind)
		}
		for _, line := range states[id].ChainLines() {
			var first, last, epoch int64
			var kind string
			if _, err := fmt.Sscanf(line, "positions %d-%d epoch %d %s", &first, &last, &epoch, &kind); err != nil {
				fatal("model chain", err)
			}
			tables.slots.Row(id, "model", first, last, epoch, kind)
		}
		// Require len(restorable.Damaged) == 0 in addition to RestoreAt(last):
		// a damaged segment before a newer snapshot would not affect restoring
		// the latest position, but would break point-in-time auditability.
		check(strings.Join(slots, "; ") == strings.Join(states[id].ChainLines(), "; ") && len(restorable.Damaged) == 0, id+": the store holds exactly the slots the model predicts: one lineage, epochs never decreasing, no damaged object")
		restoreAt := func(position int64) []byte {
			image, err := fence.RestoreAt(restorable, position)
			if err != nil {
				fatal(fmt.Sprintf("restore %s at %d", id, position), err)
			}
			return image
		}
		last := chain[len(chain)-1]
		image := restoreAt(last.Last)
		match := recipe.MatchHashes(hashes, recipe.HashImage(image))
		record, err := cat.Get(ctx, id)
		if err != nil {
			fatal("catalog "+id, err)
		}
		check(record.Epoch == last.Epoch && record.LastShipped == last.Last && record.SchemaVersion == match.Step && match.Found() && (match.Exact || recipe.ChangeCountersAgree(image)),
			id+": the catalog names the newest epoch and the last stored position, and the restored state is a recipe state")
		status, err := connmgr.CheckAtRest(filepath.Join(work, record.Owner, id+".db"))
		if err != nil {
			fatal("at rest", err)
		}
		tables.catalog.Row(id, record.Owner, record.Epoch, record.LastShipped, record.SchemaVersion, record.Location, last.Last, match, status.AtRest && status.SHA256 == walship.SHA256Hex(image))
		check(status.AtRest && status.SHA256 == walship.SHA256Hex(image), id+": the owner's closed local file is byte-identical (E1) to the state rebuilt from the store")
		for _, a := range acks {
			if a.database != id {
				continue
			}
			found := false
			for _, slot := range chain {
				for p := slot.First; p <= slot.Last && int(slot.Epoch) == a.epoch; p++ {
					if binary.BigEndian.Uint32(restoreAt(p)[60:64]) == uint32(a.userVersion) && (a.mode == node.LocalDurable || p == a.position) {
						found = true
					}
				}
			}
			ackSeq++
			var position any
			if a.mode == node.StoreDurable {
				position = a.position
			}
			tables.acks.Row(ackSeq, id, a.mode, a.epoch, a.userVersion, position, found)
			if a.mode == node.StoreDurable {
				check(found, fmt.Sprintf("%s: the store-durable acknowledgement of user_version %d is stored at position %d by epoch %d", id, a.userVersion, a.position, a.epoch))
			} else {
				check(!found, fmt.Sprintf("%s: the local-durable acknowledgement of user_version %d by epoch %d was never shipped, and the takeover lost it, as that mode allows", id, a.userVersion, a.epoch))
			}
		}
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	// Dispatch child-node role before printing the pin line so the child's
	// stdout begins cleanly with "ready" for node.StartProcess.
	if *role == "node" {
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	var store *s3store.Store
	if !*modelOnly {
		var err error
		if store, err = s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), process.Bucket, process.Prefix); err != nil {
			fmt.Printf("STORE UNREACHABLE: %v\n", err)
			os.Exit(1)
		}
	}
	work := sqlitex.MustWorkDir("10.1")
	set = results.MustOpen(work)
	openTables()
	partA()
	if !*modelOnly {
		partB(ctx, store, work)
	}
	set.Finish("ONE WRITER")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/10.1-one-writer/report.sql -->
```sql
-- Walkthrough of experiment 10.1. Run it with:
--   go run ./cmd/course run 10.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings       one setting of the model or of the service run (name, value)
--   model_runs     part A: one exploration of the ownership model (the model under test or a mutant)
--   model_traces   part A: one step of the shortest violating interleaving a mutant has
--   steps          part B: one step of the stale-owner scenario and the node's reply
--   model_steps    part B: one model action the scenario step maps to, and its outcome
--   closes         part B: one node's reply when it was closed, with its standing-bounds summary
--   problems       part B: one problem or over-bounds line a node reported (none on a good run)
--   slots          part C: one slot of a database's chain, read back from S3 or predicted by the model
--   catalog        part C: one database's catalog row and the restore of its last position
--   acks           part C: one acknowledged write and whether the store holds it
--   checks         every claim the experiment asserted (the runner lists it last)
-- info_run and info_bounds hold the store endpoint, the work directory and each
-- reply's memory sample; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.1  Can a fenced owner ever get a commit accepted?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Part A checks the ownership rule on a model: every interleaving up to a'
.print 'bounded depth, plus seeded random walks. Part B runs the same rule on two'
.print 'real node processes that share a catalog and a SeaweedFS S3 store. Part C'
.print 'reads the store back after part B and compares it with the model.'

.print
.print '── A. The ownership model, every interleaving up to a bounded depth ───'
.print
.print 'The model: nodes A and B, one database, the epoch counter of the catalog, a'
.print 'store of position slots with put-if-absent. Actions per node: claim, fence,'
.print 'fence-with-stale-list, write, ship, crash, restart, restart-with-stale-list.'
.print 'The bound on local positions counts positions across all epochs and fences,'
.print 'not commits per node.'

SELECT name, value FROM settings WHERE name LIKE 'model_%' ORDER BY seq;

.print
.print 'Invariants after every step: no two epochs accepted for overlapping'
.print 'positions; no epoch accepted after a newer one was; no accepted object'
.print 'overwritten.'
.print
.print 'A1. The correct model has no violating interleaving and no violating walk.'
.print '    Each mutant disables one half of the fencing rule, and the checker finds'
.print '    the violation: the checker can fail.'

SELECT model, depth, interleavings, distinct_states AS states, violating,
       walks, walk_steps, walk_violating
FROM model_runs ORDER BY seq;

.print
.print 'A2. The shortest violating interleaving of each mutant. With'
.print '    store-accepts-every-put, the stale listing of A lets its fence overwrite'
.print '    the accepted object of epoch 1. With resume-from-global-end, B restarts'
.print '    at the end of the store and ships behind the fence of epoch 2.'

SELECT r.model, r.violation, count(*) AS steps
FROM model_runs r JOIN model_traces t USING (model)
GROUP BY r.seq, r.model, r.violation ORDER BY r.seq;

SELECT t.model, t.step_no, t.action, t.outcome
FROM model_traces t JOIN model_runs r USING (model)
ORDER BY r.seq, t.step_no;

.print
.print '── B. The stale-owner scenario on two service processes ───────────────'
.print
.print 'Nodes a and b share the catalog and the store. The store wrapper of node b'
.print 'hides the newest slot from its first listing (fault_on_node_b). Every'
.print 'database is opened with the SQLite settings listed after the store.'

SELECT name, value FROM settings WHERE name NOT LIKE 'model_%' ORDER BY seq;

.print
.print 'B1. Fifteen steps and the replies. Steps 1-7 run on db-0001: b takes over'
.print '    while a still runs, and a is fenced on its next write (steps 5 and 6).'
.print '    Steps 8-15 run on db-0002: b is killed and restarts with its .owner'
.print '    belief file, and is fenced again (step 13) while it keeps db-0001.'

SELECT step, node, command, split_part(reply, ' ', 1) AS result
FROM steps ORDER BY step;

.print
.print 'B2. What each reply says after the command. A claim names the new epoch'
.print '    and how many fence attempts it took; a write names the epoch and'
.print '    user_version it was acknowledged at, and the store position for a'
.print '    store-durable write.'

SELECT step, split_part(reply, ' ', 1) AS result, substr(reply, strpos(reply, ': ') + 2) AS detail
FROM steps WHERE reply IS NOT NULL ORDER BY step;

.print
.print 'B3. The model actions each step maps to, and their outcomes. In step 4 the'
.print '    stale listing makes the first fence of B retry, as it did for node b'
.print '    (attempts 2).'

SELECT step, database, seq, action, outcome FROM model_steps ORDER BY step, database, seq;

.print
.print 'B4. The model agrees with every command step. The descriptors column is the'
.print '    open descriptors of the node above its baseline after the command: 7 per'
.print '    open database (3 connections), 14 for two, 0 when it holds none open.'

SELECT step, node, command, descriptors, model_agrees FROM steps
WHERE reply IS NOT NULL ORDER BY step;

SELECT count(*) AS command_steps,
       count(*) FILTER (model_agrees) AS model_agrees
FROM steps WHERE reply IS NOT NULL;

.print
.print 'B5. Closing each node. The standing assertion checked descriptors and memory'
.print '    after every command against the bounds for at most 4 open databases.'

SELECT node, reply,
       regexp_extract(summary, 'standing assertion: (\d+) checks', 1)::INT AS bound_checks,
       replace(replace(regexp_extract(summary, 'descriptors above baseline <= (\d+ \([^)]*\))', 1),
                       ' (', ' = '), ')', '')                              AS max_descriptors,
       regexp_extract(summary, 'memory growth <= (.*)$', 1)                AS max_memory_growth
FROM closes ORDER BY node;

.print
.print 'B6. Problems and over-bounds lines the nodes reported. Both counts are 0;'
.print '    any line would be listed in the table problems.'

SELECT count(*) FILTER (kind = 'problem')     AS problems,
       count(*) FILTER (kind = 'over bounds') AS over_bounds
FROM problems;

.print
.print '── C. The store after the run, read back from S3 ───────────────────────'
.print
.print 'C1. The slots in the store and the slots the model predicts after the same'
.print '    steps. They are the same: one lineage per database, epochs never'
.print '    decreasing, each epoch opening with a fence snapshot.'

SELECT s.database, s.first, s.last, s.epoch, s.kind AS store_kind, m.kind AS model_kind,
       m.epoch = s.epoch AND m.last = s.last AND m.kind = s.kind AS same
FROM slots s FULL JOIN slots m
  ON m.source = 'model' AND s.database = m.database AND s.first = m.first
WHERE s.source = 'store'
ORDER BY s.database, s.first;

.print
.print 'C2. The catalog names the newest epoch and the last stored position. The'
.print '    restore of that position is a recipe state, and the closed local file of'
.print '    the owner is byte-identical (E1) to it.'

SELECT database, owner, epoch, last_shipped, schema_version, location FROM catalog ORDER BY database;

SELECT database, restored_position, restored_state, file_equals_store FROM catalog ORDER BY database;

.print
.print 'C3. Every acknowledgement. Each store-durable one is in the store at the'
.print '    position its reply named. The local-durable acknowledgement of'
.print '    db-0001 user_version 2 was never shipped, and the takeover lost it, as'
.print '    that mode allows.'

SELECT database, mode, epoch, user_version, position, in_store FROM acks ORDER BY seq;
```

Run from the repository root, with SeaweedFS running via `docker compose up -d seaweedfs` (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 10.1
```

Add `-i` (`go run ./cmd/course run -i 10.1`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries. To run Part A without SeaweedFS, or to see the main check fail and exit `1`, run a flag variant directly in the container: `docker compose run --rm lab go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer -model-only` (or `-mutant store-accepts-every-put` / `-mutant resume-from-global-end`).

`course run` also prints the run-dependent values, which are not part of the stored evidence: where the store and the work directory were, and the bounds sample each node sent after a command, with its memory reading.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/10.1-one-writer/`](evidence/10.1-one-writer/), and `go run ./cmd/course check evidence ch10` compares a fresh run with them byte for byte.

<!-- include: evidence/10.1-one-writer.txt -->
```text

════════════════════════════════════════════════════════════════════════
  10.1  Can a fenced owner ever get a commit accepted?
════════════════════════════════════════════════════════════════════════

Part A checks the ownership rule on a model: every interleaving up to a
bounded depth, plus seeded random walks. Part B runs the same rule on two
real node processes that share a catalog and a SeaweedFS S3 store. Part C
reads the store back after part B and compares it with the model.

── A. The ownership model, every interleaving up to a bounded depth ───

The model: nodes A and B, one database, the epoch counter of the catalog, a
store of position slots with put-if-absent. Actions per node: claim, fence,
fence-with-stale-list, write, ship, crash, restart, restart-with-stale-list.
The bound on local positions counts positions across all epochs and fences,
not commits per node.
┌──────────────────────────┬───────┐
│           name           │ value │
├──────────────────────────┼───────┤
│ model_nodes              │ 2     │
│ model_max_epoch          │ 3     │
│ model_max_local_position │ 5     │
│ model_seed               │ 1     │
└──────────────────────────┴───────┘

Invariants after every step: no two epochs accepted for overlapping
positions; no epoch accepted after a newer one was; no accepted object
overwritten.

A1. The correct model has no violating interleaving and no violating walk.
    Each mutant disables one half of the fencing rule, and the checker finds
    the violation: the checker can fail.
┌─────────────────────────┬───────┬───────────────┬────────┬───────────┬───────┬────────────┬────────────────┐
│          model          │ depth │ interleavings │ states │ violating │ walks │ walk_steps │ walk_violating │
├─────────────────────────┼───────┼───────────────┼────────┼───────────┼───────┼────────────┼────────────────┤
│ correct                 │ 16    │ 9595507962    │ 44134  │ 0         │ 2000  │ 120000     │ 0              │
│ store-accepts-every-put │ 16    │ 3536900716    │ 38     │ 451413240 │ 0     │ 0          │ 0              │
│ resume-from-global-end  │ 16    │ 11458358774   │ 360    │ 170698606 │ 0     │ 0          │ 0              │
└─────────────────────────┴───────┴───────────────┴────────┴───────────┴───────┴────────────┴────────────────┘

A2. The shortest violating interleaving of each mutant. With
    store-accepts-every-put, the stale listing of A lets its fence overwrite
    the accepted object of epoch 1. With resume-from-global-end, B restarts
    at the end of the store and ships behind the fence of epoch 2.
┌─────────────────────────┬───────────────────────────────────────────────────────────────────────┬───────┐
│          model          │                               violation                               │ steps │
├─────────────────────────┼───────────────────────────────────────────────────────────────────────┼───────┤
│ store-accepts-every-put │ the accepted object of epoch 1 at position 1 was overwritten          │ 4     │
│ resume-from-global-end  │ epoch 1 was accepted at positions 3-3 after epoch 2 had been accepted │ 7     │
└─────────────────────────┴───────────────────────────────────────────────────────────────────────┴───────┘
┌─────────────────────────┬─────────┬─────────────────────────┬──────────────────────────────────┐
│          model          │ step_no │         action          │             outcome              │
├─────────────────────────┼─────────┼─────────────────────────┼──────────────────────────────────┤
│ store-accepts-every-put │ 1       │ B claim                 │ ok: catalog epoch 1              │
│ store-accepts-every-put │ 2       │ B fence                 │ ok: fence at 1                   │
│ store-accepts-every-put │ 3       │ A claim                 │ ok: catalog epoch 2              │
│ store-accepts-every-put │ 4       │ A fence-with-stale-list │ ok: fence at 1                   │
│ resume-from-global-end  │ 1       │ B claim                 │ ok: catalog epoch 1              │
│ resume-from-global-end  │ 2       │ B fence                 │ ok: fence at 1                   │
│ resume-from-global-end  │ 3       │ A claim                 │ ok: catalog epoch 2              │
│ resume-from-global-end  │ 4       │ A fence                 │ ok: fence at 2                   │
│ resume-from-global-end  │ 5       │ B crash                 │ ok: volatile state lost          │
│ resume-from-global-end  │ 6       │ B restart               │ ok: resumed at the store's end 2 │
│ resume-from-global-end  │ 7       │ B ship                  │ ok: positions 3-3 accepted       │
└─────────────────────────┴─────────┴─────────────────────────┴──────────────────────────────────┘

── B. The stale-owner scenario on two service processes ───────────────

Nodes a and b share the catalog and the store. The store wrapper of node b
hides the newest slot from its first listing (fault_on_node_b). Every
database is opened with the SQLite settings listed after the store.
┌─────────────────────────┬────────────────────┐
│          name           │       value        │
├─────────────────────────┼────────────────────┤
│ bucket                  │ course-ch10a       │
│ prefix                  │ exp-10-1/          │
│ databases               │ db-0001 db-0002    │
│ synchronous             │ FULL               │
│ foreign_keys            │ ON                 │
│ secure_delete           │ ON                 │
│ cache_size              │ -256               │
│ wal_autocheckpoint      │ 0                  │
│ busy_timeout            │ 5000ms             │
│ open_databases_per_node │ 4                  │
│ fault_on_node_b         │ stale-list:/log/:1 │
└─────────────────────────┴────────────────────┘

B1. Fifteen steps and the replies. Steps 1-7 run on db-0001: b takes over
    while a still runs, and a is fenced on its next write (steps 5 and 6).
    Steps 8-15 run on db-0002: b is killed and restarts with its .owner
    belief file, and is fenced again (step 13) while it keeps db-0001.
┌──────┬──────┬─────────────────────────────┬─────────┐
│ step │ node │           command           │ result  │
├──────┼──────┼─────────────────────────────┼─────────┤
│ 1    │ a    │ claim db-0001               │ ok      │
│ 2    │ a    │ write db-0001 store-durable │ ok      │
│ 3    │ a    │ write db-0001 local-durable │ ok      │
│ 4    │ b    │ claim db-0001               │ ok      │
│ 5    │ a    │ write db-0001 store-durable │ refused │
│ 6    │ a    │ write db-0001 local-durable │ refused │
│ 7    │ b    │ write db-0001 store-durable │ ok      │
│ 8    │ b    │ claim db-0002               │ ok      │
│ 9    │ b    │ write db-0002 store-durable │ ok      │
│ 10   │ a    │ claim db-0002               │ ok      │
│ 11   │ b    │ kill                        │         │
│ 12   │ b    │ restart                     │         │
│ 13   │ b    │ write db-0002 store-durable │ refused │
│ 14   │ b    │ write db-0001 store-durable │ ok      │
│ 15   │ a    │ write db-0002 store-durable │ ok      │
└──────┴──────┴─────────────────────────────┴─────────┘

B2. What each reply says after the command. A claim names the new epoch
    and how many fence attempts it took; a write names the epoch and
    user_version it was acknowledged at, and the store position for a
    store-durable write.
┌──────┬─────────┬───────────────────────────────────────────────────────────────────────────────────┐
│ step │ result  │                                      detail                                       │
├──────┼─────────┼───────────────────────────────────────────────────────────────────────────────────┤
│ 1    │ ok      │ epoch 1, restored position 0, fence at position 1, attempts 1                     │
│ 2    │ ok      │ acknowledged at epoch 1, user_version 1, stored through position 2                │
│ 3    │ ok      │ acknowledged at epoch 1, user_version 2, committed locally (not yet in the store) │
│ 4    │ ok      │ epoch 2, restored position 2, fence at position 3, attempts 2                     │
│ 5    │ refused │ fenced: db-0001 position 3 is held by epoch 2, this writer has epoch 1            │
│ 6    │ refused │ fenced: db-0001 position 3 is held by epoch 2, this writer has epoch 1            │
│ 7    │ ok      │ acknowledged at epoch 2, user_version 2, stored through position 4                │
│ 8    │ ok      │ epoch 1, restored position 0, fence at position 1, attempts 1                     │
│ 9    │ ok      │ acknowledged at epoch 1, user_version 1, stored through position 2                │
│ 10   │ ok      │ epoch 2, restored position 2, fence at position 3, attempts 1                     │
│ 13   │ refused │ fenced: db-0002 position 3 is held by epoch 2, this writer has epoch 1            │
│ 14   │ ok      │ acknowledged at epoch 2, user_version 3, stored through position 5                │
│ 15   │ ok      │ acknowledged at epoch 2, user_version 2, stored through position 4                │
└──────┴─────────┴───────────────────────────────────────────────────────────────────────────────────┘

B3. The model actions each step maps to, and their outcomes. In step 4 the
    stale listing makes the first fence of B retry, as it did for node b
    (attempts 2).
┌──────┬──────────┬─────┬─────────────────────────┬────────────────────────────────────────────────────────────┐
│ step │ database │ seq │         action          │                          outcome                           │
├──────┼──────────┼─────┼─────────────────────────┼────────────────────────────────────────────────────────────┤
│ 1    │ db-0001  │ 1   │ A claim                 │ ok: catalog epoch 1                                        │
│ 1    │ db-0001  │ 2   │ A fence                 │ ok: fence at 1                                             │
│ 2    │ db-0001  │ 1   │ A write                 │ ok: local commit 2                                         │
│ 2    │ db-0001  │ 2   │ A ship                  │ ok: positions 2-2 accepted                                 │
│ 3    │ db-0001  │ 1   │ A write                 │ ok: local commit 3                                         │
│ 4    │ db-0001  │ 1   │ B claim                 │ ok: catalog epoch 2                                        │
│ 4    │ db-0001  │ 2   │ B fence-with-stale-list │ refused: position 2 holds epoch 1, retry                   │
│ 4    │ db-0001  │ 3   │ B fence                 │ ok: fence at 3                                             │
│ 5    │ db-0001  │ 1   │ A write                 │ ok: local commit 4                                         │
│ 5    │ db-0001  │ 2   │ A ship                  │ refused: position 3 holds epoch 2                          │
│ 6    │ db-0001  │ 1   │ A write                 │ refused: not enabled                                       │
│ 7    │ db-0001  │ 1   │ B write                 │ ok: local commit 4                                         │
│ 7    │ db-0001  │ 2   │ B ship                  │ ok: positions 4-4 accepted                                 │
│ 8    │ db-0002  │ 1   │ B claim                 │ ok: catalog epoch 1                                        │
│ 8    │ db-0002  │ 2   │ B fence                 │ ok: fence at 1                                             │
│ 9    │ db-0002  │ 1   │ B write                 │ ok: local commit 2                                         │
│ 9    │ db-0002  │ 2   │ B ship                  │ ok: positions 2-2 accepted                                 │
│ 10   │ db-0002  │ 1   │ A claim                 │ ok: catalog epoch 2                                        │
│ 10   │ db-0002  │ 2   │ A fence                 │ ok: fence at 3                                             │
│ 11   │ db-0001  │ 1   │ B crash                 │ ok: volatile state lost                                    │
│ 11   │ db-0002  │ 1   │ B crash                 │ ok: volatile state lost                                    │
│ 12   │ db-0001  │ 1   │ B restart               │ ok: resumed own chain at 4                                 │
│ 12   │ db-0002  │ 1   │ B restart               │ ok: restarted, then refused: a newer epoch is in the store │
│ 13   │ db-0002  │ 1   │ B write                 │ refused: not enabled                                       │
│ 13   │ db-0002  │ 2   │ B ship                  │ refused: not enabled                                       │
│ 14   │ db-0001  │ 1   │ B write                 │ ok: local commit 5                                         │
│ 14   │ db-0001  │ 2   │ B ship                  │ ok: positions 5-5 accepted                                 │
│ 15   │ db-0002  │ 1   │ A write                 │ ok: local commit 4                                         │
│ 15   │ db-0002  │ 2   │ A ship                  │ ok: positions 4-4 accepted                                 │
└──────┴──────────┴─────┴─────────────────────────┴────────────────────────────────────────────────────────────┘

B4. The model agrees with every command step. The descriptors column is the
    open descriptors of the node above its baseline after the command: 7 per
    open database (3 connections), 14 for two, 0 when it holds none open.
┌──────┬──────┬─────────────────────────────┬─────────────┬──────────────┐
│ step │ node │           command           │ descriptors │ model_agrees │
├──────┼──────┼─────────────────────────────┼─────────────┼──────────────┤
│ 1    │ a    │ claim db-0001               │ 0           │ true         │
│ 2    │ a    │ write db-0001 store-durable │ 7           │ true         │
│ 3    │ a    │ write db-0001 local-durable │ 7           │ true         │
│ 4    │ b    │ claim db-0001               │ 0           │ true         │
│ 5    │ a    │ write db-0001 store-durable │ 0           │ true         │
│ 6    │ a    │ write db-0001 local-durable │ 0           │ true         │
│ 7    │ b    │ write db-0001 store-durable │ 7           │ true         │
│ 8    │ b    │ claim db-0002               │ 7           │ true         │
│ 9    │ b    │ write db-0002 store-durable │ 14          │ true         │
│ 10   │ a    │ claim db-0002               │ 0           │ true         │
│ 13   │ b    │ write db-0002 store-durable │ 0           │ true         │
│ 14   │ b    │ write db-0001 store-durable │ 7           │ true         │
│ 15   │ a    │ write db-0002 store-durable │ 7           │ true         │
└──────┴──────┴─────────────────────────────┴─────────────┴──────────────┘
┌───────────────┬──────────────┐
│ command_steps │ model_agrees │
├───────────────┼──────────────┤
│ 13            │ 13           │
└───────────────┴──────────────┘

B5. Closing each node. The standing assertion checked descriptors and memory
    after every command against the bounds for at most 4 open databases.
┌──────┬──────────┬──────────────┬────────────────────────────────────────────────────────┬───────────────────┐
│ node │  reply   │ bound_checks │                    max_descriptors                     │ max_memory_growth │
├──────┼──────────┼──────────────┼────────────────────────────────────────────────────────┼───────────────────┤
│ a    │ ok close │ 8            │ 68 = 3 x 3 connections x 4 open databases + 32 reserve │ 41 MiB            │
│ b    │ ok close │ 3            │ 68 = 3 x 3 connections x 4 open databases + 32 reserve │ 41 MiB            │
└──────┴──────────┴──────────────┴────────────────────────────────────────────────────────┴───────────────────┘

B6. Problems and over-bounds lines the nodes reported. Both counts are 0;
    any line would be listed in the table problems.
┌──────────┬─────────────┐
│ problems │ over_bounds │
├──────────┼─────────────┤
│ 0        │ 0           │
└──────────┴─────────────┘

── C. The store after the run, read back from S3 ───────────────────────

C1. The slots in the store and the slots the model predicts after the same
    steps. They are the same: one lineage per database, epochs never
    decreasing, each epoch opening with a fence snapshot.
┌──────────┬───────┬──────┬───────┬────────────┬────────────┬──────┐
│ database │ first │ last │ epoch │ store_kind │ model_kind │ same │
├──────────┼───────┼──────┼───────┼────────────┼────────────┼──────┤
│ db-0001  │ 1     │ 1    │ 1     │ snapshot   │ snapshot   │ true │
│ db-0001  │ 2     │ 2    │ 1     │ segment    │ segment    │ true │
│ db-0001  │ 3     │ 3    │ 2     │ snapshot   │ snapshot   │ true │
│ db-0001  │ 4     │ 4    │ 2     │ segment    │ segment    │ true │
│ db-0001  │ 5     │ 5    │ 2     │ segment    │ segment    │ true │
│ db-0002  │ 1     │ 1    │ 1     │ snapshot   │ snapshot   │ true │
│ db-0002  │ 2     │ 2    │ 1     │ segment    │ segment    │ true │
│ db-0002  │ 3     │ 3    │ 2     │ snapshot   │ snapshot   │ true │
│ db-0002  │ 4     │ 4    │ 2     │ segment    │ segment    │ true │
└──────────┴───────┴──────┴───────┴────────────┴────────────┴──────┘

C2. The catalog names the newest epoch and the last stored position. The
    restore of that position is a recipe state, and the closed local file of
    the owner is byte-identical (E1) to it.
┌──────────┬───────┬───────┬──────────────┬────────────────┬──────────────┐
│ database │ owner │ epoch │ last_shipped │ schema_version │   location   │
├──────────┼───────┼───────┼──────────────┼────────────────┼──────────────┤
│ db-0001  │ b     │ 2     │ 5            │ 3              │ b:db-0001.db │
│ db-0002  │ a     │ 2     │ 4            │ 2              │ a:db-0002.db │
└──────────┴───────┴───────┴──────────────┴────────────────┴──────────────┘
┌──────────┬───────────────────┬──────────────────────┬───────────────────┐
│ database │ restored_position │    restored_state    │ file_equals_store │
├──────────┼───────────────────┼──────────────────────┼───────────────────┤
│ db-0001  │ 5                 │ step 3 (exact bytes) │ true              │
│ db-0002  │ 4                 │ step 2 (exact bytes) │ true              │
└──────────┴───────────────────┴──────────────────────┴───────────────────┘

C3. Every acknowledgement. Each store-durable one is in the store at the
    position its reply named. The local-durable acknowledgement of
    db-0001 user_version 2 was never shipped, and the takeover lost it, as
    that mode allows.
┌──────────┬───────────────┬───────┬──────────────┬──────────┬──────────┐
│ database │     mode      │ epoch │ user_version │ position │ in_store │
├──────────┼───────────────┼───────┼──────────────┼──────────┼──────────┤
│ db-0001  │ store-durable │ 1     │ 1            │ 2        │ true     │
│ db-0001  │ local-durable │ 1     │ 2            │          │ false    │
│ db-0001  │ store-durable │ 2     │ 2            │ 4        │ true     │
│ db-0001  │ store-durable │ 2     │ 3            │ 5        │ true     │
│ db-0002  │ store-durable │ 1     │ 1            │ 2        │ true     │
│ db-0002  │ store-durable │ 2     │ 2            │ 4        │ true     │
└──────────┴───────────────┴───────┴──────────────┴──────────┴──────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  model correct: no interleaving up to depth 16 and no random walk lets a fenced epoch get a commit accepted
A  ✓  self-test: with store-accepts-every-put the checker finds the violation
A  ✓  self-test: with resume-from-global-end the checker finds the violation
B  ✓  the real processes and the model agree at every command step (13 of 13)
B  ✓  no node reported a problem (a failed final ship, an eviction that did not leave the shipped state at rest, an I/O error)
B  ✓  after each of the 15 commands the node's descriptors and memory stayed within the standing bounds
C  ✓  db-0001: the store holds exactly the slots the model predicts: one lineage, epochs never decreasing, no damaged object
C  ✓  db-0001: the catalog names the newest epoch and the last stored position, and the restored state is a recipe state
C  ✓  db-0001: the owner's closed local file is byte-identical (E1) to the state rebuilt from the store
C  ✓  db-0001: the store-durable acknowledgement of user_version 1 is stored at position 2 by epoch 1
C  ✓  db-0001: the local-durable acknowledgement of user_version 2 by epoch 1 was never shipped, and the takeover lost it, as that mode allows
C  ✓  db-0001: the store-durable acknowledgement of user_version 2 is stored at position 4 by epoch 2
C  ✓  db-0001: the store-durable acknowledgement of user_version 3 is stored at position 5 by epoch 2
C  ✓  db-0002: the store holds exactly the slots the model predicts: one lineage, epochs never decreasing, no damaged object
C  ✓  db-0002: the catalog names the newest epoch and the last stored position, and the restored state is a recipe state
C  ✓  db-0002: the owner's closed local file is byte-identical (E1) to the state rebuilt from the store
C  ✓  db-0002: the store-durable acknowledgement of user_version 1 is stored at position 2 by epoch 1
C  ✓  db-0002: the store-durable acknowledgement of user_version 2 is stored at position 4 by epoch 2

18 of 18 claims hold

Experiment verdict: ONE WRITER OK
```

#### Reading the Experiment 10.1 evidence

- **Pin check** (the experiment's own output, shown when it is run directly; not part of the walkthrough): confirms `SQLite 3.53.4` check-in `bf7c7f30…59bcc`.
- **Section A settings and A1 (Part A — correct protocol):** Within the stated bounds (`2` nodes, `<= 3` epochs, local commits stopping at stream position `5` across all epochs and fences), `model.Explore` explores **`9,595,507,962` interleavings** across **`44,134` distinct states** up to depth `16` plus **`2,000` random walks (`120,000` steps)** and finds **`0` invariant violations**.
- **A1–A2, `store-accepts-every-put` (self-test 1 — removing store slot exclusivity):** With `store-accepts-every-put`, **`451,413,240` of `3,536,900,716` interleavings** violate an invariant. The shortest counterexample is `4` steps: `B` claims epoch `1` and fences at `1`; `A` claims epoch `2`, reads a stale listing, and overwrites `B`'s accepted fence at position `1`.
- **A1–A2, `resume-from-global-end` (self-test 2 — removing the writer epoch check on restart):** With `resume-from-global-end` (matching unfenced `walship.Open`), **`170,698,606` of `11,458,358,774` interleavings** violate an invariant. The shortest counterexample is `7` steps: `B` fences at `1`, `A` fences at `2`, `B` crashes, restarts at global end `2`, and ships epoch `1` into free slot `3` after epoch `2` was already accepted—proving `If-None-Match: *` alone is insufficient without `fence.Adapter`.
- **Section B settings (Part B setup):** Connects to SeaweedFS (`course-ch10a`, prefix `exp-10-1/`), registers `db-0001` and `db-0002` with `synchronous=FULL`, `foreign_keys=ON`, `secure_delete=ON`, `cache_size=-256`, `wal_autocheckpoint=0`, `busy_timeout=5000ms`, capacity `4` open databases per node, and installs `stale-list:/log/:1` on node `b`.
- **B1–B3, steps 1–7 (`db-0001` stale-list retry and active stale-owner fencing):** `a` claims `db-0001` (epoch `1`, fence at `1`), commits step `1` store-durable (slot `2`), and commits step `2` local-durable (not in store). At step `4`, `b` claims epoch `2`: its first listing misses slot `2`, its fence attempt at slot `2` hits `e1` and retries, and attempt `2` restores position `2` (step `1`) and puts its fence at slot `3` (`attempts 2`). At step `5`, stale `a` commits step `3` locally and tries to ship at slot `3`, receives `fenced: db-0001 position 3 is held by epoch 2, this writer has epoch 1`, drops its belief, and refuses step `6` immediately. Node `b` then advances `db-0001` to position `4` (`user_version 2`).
- **B1–B3, steps 8–15 (`db-0002` crashed and restarted stale owner):** `b` owns `db-0002` (epoch `1`, slot `2`), `a` takes over (epoch `2`, fence at `3`), and `b` is `SIGKILL`ed (step `11`) and restarted (step `12`) with its persisted `.owner` belief files intact. At step `13`, `b`'s write to `db-0002` opens the shipper through `fence.Adapter`, lists slot `3` (`epoch 2`), and refuses before touching the local file. At step `14`, `b` reopens `db-0001` (whose `-wal`/`-shm` survived `SIGKILL`), recovers cleanly, and writes slot `5` (`user_version 3`). At step `15`, `a` writes `db-0002` slot `4`.
- **B4–B6 (agreement and standing bounds):** Real processes and model agree **`13 of 13`** command steps; `0` node problems; descriptors stayed at `0`, `7`, or `14` above baseline (exactly `7` per open database, well under the `68` limit for `K = 4`) and memory growth stayed within the `41 MiB` bound across all `15` commands (B5; the memory readings themselves are run-dependent and printed by `course run`).
- **C1–C3 (store, catalog, E1, and durability checks):** For both `db-0001` (`5` slots) and `db-0002` (`4` slots), the S3 chain matches the model slot for slot with `0` damaged objects, the catalog matches the newest epoch/position/`user_version`, the owner's closed file is **E1 byte-identical** to `RestoreAt(last)`, every `store-durable` acknowledgement is stored at its exact epoch and position, and `db-0001`'s unshipped `local-durable` acknowledgement (epoch `1`, `user_version` `2`) reports `in_store` `false`, lost by the takeover as that mode allows.

**What Experiment 10.1 does and does not show.** Part A proves safety inside its finite bounds (`2` nodes, `3` epochs, stream position `<= 5`, depth `16`; adversarial attack `MOD-04` extends this to an independent `3`-node, `5`-epoch, multi-slot-stale model). Part B scripts one sequential interleaving on two real processes; truly concurrent S3 `PutIfAbsent` and `catalog.Claim` races are tested in the SeaweedFS probe and adversarial attack `FENCE-02`.

---

## 6. Evidence and failure cases

Experiments 10.2, 10.3, and 10.4 attack the service with process kills, interrupted relocations, store faults, and full-fleet destruction.

### 6.1 Experiment 10.2 — `acknowledged-writes-survive`

- **Claim (invariants 2 and 4 — durability under stated mode and reopen after process failure):** One node owns `4` databases while keeping at most `2` open (forcing LRU eviction on every write). After the node is killed—on entry to evenly spaced `fsync`/`fdatasync` calls via `strace` (**Linux only**, Part A) or by `SIGKILL` at seeded random delays inside the active write loop (portable, Part B)—every database reopens with `PRAGMA integrity_check = ok` as a valid recipe state and holds at least every acknowledged write (`recovered user_version >= acknowledged user_version`): on local files for `local-durable`, and after deleting the node's entire directory and rebuilding from SeaweedFS S3 for `store-durable`. A mutant node that acknowledges `store-durable` writes before putting the segment (`-mutant ack-before-put`, Part C) is caught as a violation.
- **Evidence form:** `EXACT` (every stored table must match on every run, including the check requiring every random kill to land while writes are in flight; Part B's timings and random kill points are in the run-dependent `info` tables, which are excluded).
- **Linux only (Part A):** Killing on entry to `fsync`/`fdatasync` requires Linux `strace`. Run directly on macOS (or with `-portable` on Linux), Part A prints `LINUX-ONLY: killing the node at a chosen fsync with strace skipped on …` while Parts B and C run natively. `course run` runs Part A in the Linux `lab` container.

<!-- include: experiments/10.2-acknowledged-writes-survive/main.go -->
```go
// Experiment 10.2 tests two capstone invariants under process kills: every
// database can be reopened after process failure, and no acknowledged durable
// write is lost under its stated durability mode.
//
// One node owns 4 databases while keeping at most 2 open (forcing LRU eviction
// on every round-robin write). Each run sends 12 writes in either local-durable
// mode (synchronous=FULL; walFrames syncs -wal before COMMIT returns at
// src/wal.c:4195-4213; recovery restarts on the local directory) or
// store-durable mode (acknowledged after the segment put; recovery deletes the
// node's entire directory and rebuilds every database from SeaweedFS S3).
//
// Part A (Linux only) uses strace to kill the node on entry to evenly spaced
// fsync/fdatasync calls and verifies walIndexRecover (src/wal.c:1390-1614)
// recovers every database cleanly with recovered user_version >= acknowledged.
// Part B (portable) kills interactive runs at seeded random delays inside the
// measured write-loop window (redrawing if a kill lands after the last reply).
// Part C runs the ack-before-put mutant as a self-test to prove the checker
// catches an unshipped store-durable acknowledgement.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.2
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive"

var (
	databases    = flag.Int("databases", 4, "databases the node owns")
	writes       = flag.Int("writes", 12, "writes in each killed run, round robin over the databases")
	syscallKills = flag.Int("syscall-kills", 4, "kill points at fsync per mode (Linux, strace)")
	randomKills  = flag.Int("random-kills", 2, "kills at a random time per mode")
	seed         = flag.Uint64("seed", 1, "seed of the random kill times")
	mutant       = flag.String("mutant", "", "ack-before-put: the killed node acknowledges store-durable writes before the segment put")
	portable     = flag.Bool("portable", false, "skip the strace kill points even on Linux")
	role         = flag.String("role", "", "internal: run as a service node")
	process      node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-2", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 2, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: the mutant node")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.2" runs both.
// part labels the rows and claims written while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, parts, uninterrupted, fsyncKills, randomKills, recovery, violations, selfTest, problems, infoRun, infoWriteLoops, infoRandomKills *results.Table
	}
	settingSeq int
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.parts = set.Table("parts", "part", "name", "ran", "note")
	tables.uninterrupted = set.Table("uninterrupted", "part", "mode", "how", "commands", "acknowledged", "command_thread_fsyncs", "exit_code")
	tables.fsyncKills = set.Table("fsync_kills", "mode", "kill_no", "at_fsync", "of_fsyncs", "file", "killed", "during_command_no", "during_command", "acknowledged", "reopened")
	tables.randomKills = set.Table("random_kills", "mode", "kill_no", "in_flight", "violations")
	tables.recovery = set.Table("recovery", "part", "mode", "kill_no", "database", "acknowledged_version", "reopened", "integrity", "recovered_version", "recovered_state")
	tables.violations = set.Table("violations", "part", "mode", "kill_no", "line")
	tables.selfTest = set.Table("self_test", "reply", "acknowledged")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.infoRun = set.Table("info_run", "name", "value")
	tables.infoWriteLoops = set.Table("info_write_loops", "mode", "writes", "elapsed_ms", "acknowledged")
	tables.infoRandomKills = set.Table("info_random_kills", "mode", "kill_no", "draw", "delay_ms", "acknowledged", "reopened", "recovered_versions", "acknowledged_versions")
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type experiment struct {
	env *lab.Env
	ids []string
}

type outcome struct {
	acked      map[string]int
	acks       int
	replies    int
	recovered  map[string]lab.Recovered
	integrity  map[string]string
	reopened   int
	violations []string
}

func (x *experiment) script(mode node.AckMode) []string {
	commands := make([]string, *writes)
	for i := range commands {
		commands[i] = fmt.Sprintf("write %s %s", x.ids[i%len(x.ids)], mode)
	}
	return commands
}

func (x *experiment) setup() {
	if err := x.env.Reset(x.ids); err != nil {
		fatal("reset", err)
	}
	p, err := x.env.Start(x.env.Config("a", ""))
	if err != nil {
		fatal("start node a", err)
	}
	for _, id := range x.ids {
		reply, err := x.env.Send(p, "claim "+id)
		if err != nil || !strings.HasPrefix(reply.Line, "ok claim ") {
			fatal("claim "+id, fmt.Errorf("%q %v", reply.Line, err))
		}
	}
	if _, err := x.env.Stop(p); err != nil {
		fatal("stop node a", err)
	}
}

func (x *experiment) killedConfig() node.ProcessConfig {
	config := x.env.Config("a", "")
	config.AckBeforePut = *mutant == "ack-before-put"
	return config
}

func (x *experiment) record(o *outcome, replies []string) {
	o.replies = len(replies)
	for _, line := range replies {
		if !strings.HasPrefix(line, "ok write ") {
			continue
		}
		id, ack, err := node.ParseAck(line)
		if err != nil {
			fatal("parse acknowledgement", err)
		}
		o.acked[id] = max(o.acked[id], ack.UserVersion)
		o.acks++
	}
}

func newOutcome() *outcome {
	return &outcome{acked: map[string]int{}, recovered: map[string]lab.Recovered{}, integrity: map[string]string{}}
}

func (x *experiment) traced(mode node.AckMode, kill *trace.KillAt) (*trace.Result, *outcome) {
	x.setup()
	command, err := trace.SelfCommand(x.killedConfig().Args()...)
	if err != nil {
		fatal("self command", err)
	}
	stdin := strings.Join(x.script(mode), "\n") + "\n"
	result, err := trace.Run(x.env.Ctx, trace.Options{Command: command, Stdin: strings.NewReader(stdin), Syscalls: []string{"fsync", "fdatasync"}, KillAt: kill})
	if err != nil {
		fatal("strace", err)
	}
	o := newOutcome()
	x.record(o, x.env.Tracker.NoteOutput("a", result.Stdout))
	return result, o
}

func (x *experiment) randomKill(mode node.AckMode, delay time.Duration) (*outcome, time.Duration) {
	x.setup()
	p, err := x.env.Start(x.killedConfig())
	if err != nil {
		fatal("start node a", err)
	}
	// Time only the write loop (after setup and node startup) so random kill
	// delays fall inside the active write window rather than after the last reply.
	writesStarted := time.Now()
	timer := time.AfterFunc(delay, func() { p.Signal() })
	var replies []string
	for _, command := range x.script(mode) {
		reply, err := x.env.Send(p, command)
		if err != nil {
			break
		}
		replies = append(replies, reply.Line)
	}
	writeElapsed := time.Since(writesStarted)
	timer.Stop()
	p.Signal()
	p.Reap()
	o := newOutcome()
	x.record(o, replies)
	return o, writeElapsed
}

func (x *experiment) recover(mode node.AckMode, o *outcome) {
	if mode == node.StoreDurable {
		if err := os.RemoveAll(x.env.NodeDir("a")); err != nil {
			fatal("delete the local files", err)
		}
	}
	p, err := x.env.Start(x.env.Config("a", ""))
	if err != nil {
		fatal("restart node a", err)
	}
	remote := node.Remote{Process: func() *node.Process { return p }, OnReply: func(_ string, reply node.Reply) { x.env.Tracker.Note("a", reply) }}
	for _, id := range x.ids {
		if mode == node.StoreDurable {
			if _, err := remote.Claim(x.env.Ctx, id); err != nil {
				o.violations = append(o.violations, fmt.Sprintf("%s: no rebuild from the store: %v", id, err))
				continue
			}
		}
		inspection, err := remote.Inspect(x.env.Ctx, id)
		if err != nil {
			o.violations = append(o.violations, fmt.Sprintf("%s does not reopen: %v", id, err))
			continue
		}
		o.reopened++
		o.integrity[id] = inspection.Integrity
	}
	if _, err := x.env.Stop(p); err != nil {
		fatal("stop node a", err)
	}
	for _, id := range x.ids {
		if _, ok := o.integrity[id]; !ok {
			continue
		}
		recovered, err := x.env.MatchFile(x.env.DatabasePath("a", id))
		if err != nil {
			fatal("match "+id, err)
		}
		o.recovered[id] = recovered
		switch {
		case o.integrity[id] != "ok":
			o.violations = append(o.violations, fmt.Sprintf("%s integrity %s", id, o.integrity[id]))
		case !recovered.RecipeState:
			o.violations = append(o.violations, fmt.Sprintf("%s is not a recipe state: %s", id, recovered.Match))
		case recovered.UserVersion < o.acked[id]:
			o.violations = append(o.violations, fmt.Sprintf("%s recovered step %d, acknowledged step %d", id, recovered.UserVersion, o.acked[id]))
		}
	}
}

func (x *experiment) versions(o *outcome) (string, string) {
	var recovered, acked []string
	for _, id := range x.ids {
		recovered = append(recovered, fmt.Sprint(o.recovered[id].UserVersion))
		acked = append(acked, fmt.Sprint(o.acked[id]))
	}
	return strings.Join(recovered, " "), strings.Join(acked, " ")
}

// recordRecovery writes what each database held after the recovery of one
// killed run, and the violations the recovery found.
func (x *experiment) recordRecovery(mode node.AckMode, kill int, o *outcome) {
	for _, id := range x.ids {
		integrity, reopened := o.integrity[id]
		var state, version any
		if recovered, ok := o.recovered[id]; ok {
			state, version = recovered.Match, recovered.UserVersion
		}
		tables.recovery.Row(part, mode, kill, id, o.acked[id], reopened, integrity, version, state)
	}
	x.recordViolations(mode, kill, o)
}

func (x *experiment) recordViolations(mode node.AckMode, kill int, o *outcome) {
	for _, v := range o.violations {
		tables.violations.Row(part, mode, kill, v)
	}
}

func killedFile(result *trace.Result) string {
	for _, call := range result.Log.Calls {
		if call.Unfinished && (call.Name == "fsync" || call.Name == "fdatasync") {
			return filepath.Base(call.Path)
		}
	}
	return "no file: nothing was killed"
}

func fsyncsByThread(result *trace.Result) (int, int, int) {
	counts := map[int]int{}
	for _, call := range result.Log.Calls {
		if call.Name == "fsync" || call.Name == "fdatasync" {
			counts[call.PID]++
		}
	}
	best, most, total := 0, 0, 0
	for pid, n := range counts {
		total += n
		if n > most {
			best, most = pid, n
		}
	}
	return best, most, total - most
}

func (x *experiment) syscallKills(mode node.AckMode) {
	uninterrupted, base := x.traced(mode, nil)
	_, syncs, elsewhere := fsyncsByThread(uninterrupted)
	tables.uninterrupted.Row(part, mode, "strace, not killed", base.replies, base.acks, syncs, uninterrupted.ExitCode)
	tables.infoRun.Row(fmt.Sprintf("%s: fsync calls on other threads", mode), elsewhere)
	check(uninterrupted.ExitCode == 0 && base.acks == *writes && elsewhere == 0, fmt.Sprintf("%s: the uninterrupted run acknowledges all %d writes and every fsync runs on the command thread", mode, *writes))
	violations, killed := 0, 0
	for i := range *syscallKills {
		when := 1 + i*(syncs-1)/max(*syscallKills-1, 1)
		result, o := x.traced(mode, &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: when})
		if result.Killed == "SIGKILL" {
			killed++
		}
		x.recover(mode, o)
		var duringNo, during any
		if o.replies < *writes {
			duringNo, during = o.replies+1, x.script(mode)[o.replies]
		}
		tables.fsyncKills.Row(mode, i+1, when, syncs, killedFile(result), result.Killed, duringNo, during, o.acks, o.reopened)
		x.recordRecovery(mode, i+1, o)
		violations += len(o.violations)
	}
	check(killed == *syscallKills, fmt.Sprintf("%s: strace killed the node at each of the %d fsync kill points", mode, *syscallKills))
	check(violations == 0, fmt.Sprintf("%s: after each kill at an fsync every database reopens (integrity ok, a recipe state) and holds at least every write acknowledged in this mode", mode))
}

const drawsPerRandomKill = 5

func (x *experiment) randomTimeKills(mode node.AckMode, rng *rand.Rand) {
	full, elapsed := x.randomKill(mode, time.Hour)
	tables.uninterrupted.Row(part, mode, "interactive, not killed", full.replies, full.acks, nil, nil)
	tables.infoWriteLoops.Row(mode, *writes, elapsed.Milliseconds(), full.acks)
	violations, inFlight := 0, 0
	for i := range *randomKills {
		killViolations, landed := 0, false
		for draw := 1; draw <= drawsPerRandomKill; draw++ {
			delay := time.Duration(rng.Int64N(int64(elapsed)))
			o, _ := x.randomKill(mode, delay)
			x.recover(mode, o)
			recovered, acked := x.versions(o)
			tables.infoRandomKills.Row(mode, i+1, draw, delay.Milliseconds(), o.acks, o.reopened, recovered, acked)
			x.recordViolations(mode, i+1, o)
			killViolations += len(o.violations)
			// Every draw is recovered and checked; redraw up to drawsPerRandomKill
			// if the timer fired after all writes had already replied.
			if o.replies < *writes {
				landed = true
				break
			}
		}
		tables.randomKills.Row(mode, i+1, landed, killViolations)
		violations += killViolations
		if landed {
			inFlight++
		}
	}
	check(full.acks == *writes, fmt.Sprintf("%s: an interactive run that is not killed acknowledges all %d writes", mode, *writes))
	check(inFlight == *randomKills, fmt.Sprintf("%s: each of the %d random kills landed while writes were in flight (a kill after the last reply is recovered and checked too, then redrawn, at most %d draws per kill)", mode, *randomKills, drawsPerRandomKill))
	check(violations == 0, fmt.Sprintf("%s: after every kill at a random time every database reopens (integrity ok, a recipe state) and holds at least every acknowledged write", mode))
}

func (x *experiment) selfTest() {
	part = "C"
	tables.parts.Row(part, "self-test: a node that acknowledges store-durable writes before the segment put", true, nil)
	x.setup()
	config := x.env.Config("a", "")
	config.AckBeforePut = true
	p, err := x.env.Start(config)
	if err != nil {
		fatal("start the mutant node", err)
	}
	reply, err := x.env.Send(p, fmt.Sprintf("write %s %s", x.ids[0], node.StoreDurable))
	if err != nil {
		fatal("mutant write", err)
	}
	p.Signal()
	p.Reap()
	o := newOutcome()
	x.record(o, []string{reply.Line})
	x.recover(node.StoreDurable, o)
	tables.selfTest.Row(reply.Line, o.acks)
	x.recordRecovery(node.StoreDurable, 1, o)
	check(o.acks == 1 && len(o.violations) == 1, "self-test: the checker finds the store-durable acknowledgement the store never received")
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		// Pin the command loop to one OS thread so strace's per-thread syscall
		// counter sees every SQLite fsync on a single deterministic thread.
		runtime.LockOSThread()
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *mutant != "" && *mutant != "ack-before-put" {
		fatal("-mutant", fmt.Errorf("unknown mutant %q (want ack-before-put)", *mutant))
	}
	if *databases < 1 || *writes < 1 || *syscallKills < 1 || *randomKills < 0 {
		fatal("flags", fmt.Errorf("-databases, -writes and -syscall-kills must be positive, -random-kills not negative"))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.2", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	set = results.MustOpen(env.Work)
	openTables()
	// env.Reset empties env.Work before every run, so the node works in a
	// subdirectory and the tables in env.Work/results survive.
	env.Work = filepath.Join(env.Work, "node")
	env.Process.Work = env.Work
	if err := os.MkdirAll(env.Work, 0o755); err != nil {
		fatal("work directory", err)
	}
	x := &experiment{env: env, ids: lab.Ids("db", *databases)}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", env.ShownWork())
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	setting("databases", *databases)
	setting("open_databases", process.Capacity)
	setting("writes_per_run", *writes)
	setting("fsync_kill_points_per_mode", *syscallKills)
	setting("random_kills_per_mode", *randomKills)
	setting("random_seed", *seed)
	if *mutant != "" {
		setting("mutant", *mutant)
	}
	for _, pragma := range strings.Fields(node.PlatformSettings().String()) {
		name, value, _ := strings.Cut(pragma, "=")
		setting(name, value)
	}
	modes := []node.AckMode{node.LocalDurable, node.StoreDurable}
	part = "A"
	if trace.Supported && !*portable {
		tables.parts.Row(part, "kills at a chosen fsync (strace)", true, nil)
		for _, mode := range modes {
			x.syscallKills(mode)
		}
	} else {
		notice := trace.LinuxOnlyMessage("killing the node at a chosen fsync with strace", packagePath)
		fmt.Println(notice)
		tables.parts.Row(part, "kills at a chosen fsync (strace)", false, notice)
	}
	part = "B"
	tables.parts.Row(part, "kills at a random time (portable)", true, nil)
	rng := rand.New(rand.NewPCG(*seed, 102))
	for _, mode := range modes {
		x.randomTimeKills(mode, rng)
	}
	x.selfTest()
	part = "D"
	seq := 0
	for _, kind := range []struct {
		name  string
		lines []string
	}{{"problem", env.Tracker.Problems}, {"over bounds", env.Tracker.OverBounds}} {
		for _, line := range kind.lines {
			seq++
			tables.problems.Row(seq, kind.name, line)
		}
	}
	check(len(env.Tracker.Problems) == 0, "no node reported a problem")
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every node process the descriptors and memory stayed within the standing bounds")
	tables.infoRun.Row("node replies that carried a bounds check", env.Tracker.Replies)
	set.Finish("ACKNOWLEDGED WRITES SURVIVE")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/10.2-acknowledged-writes-survive/report.sql -->
```sql
-- Walkthrough of experiment 10.2. Run it with:
--   go run ./cmd/course run 10.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings        one setting of the run (name, value)
--   parts           one part of the experiment, whether it ran, and why not
--   uninterrupted   one run that was not killed: the baseline of parts A and B
--   fsync_kills     part A: one run killed by strace on entry to a chosen fsync
--   random_kills    part B: one kill at a random time, after its redraws
--   recovery        parts A and C: one database after the recovery of one killed run
--   violations      one database a recovery found broken or missing an acknowledged write
--   self_test       part C: the reply of the mutant node
--   problems        one problem or over-bounds line a node reported (none on a good run)
--   checks          every claim the experiment asserted (the runner lists it last)
-- info_run, info_write_loops and info_random_kills hold what depends on timing:
-- the random kill delays and what each random kill left. info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.2  Does every acknowledged write survive a killed process?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'One node owns 4 databases and keeps at most 2 open, so every round-robin'
.print 'write evicts one. Each killed run sends 12 writes in one mode:'
.print
.print '  local-durable   acknowledged after the local commit at synchronous=FULL.'
.print '                  Recovery restarts the node on its own files.'
.print '  store-durable   acknowledged after the segment put to the S3 store.'
.print '                  Recovery deletes the directory of the node first and'
.print '                  rebuilds every database from the store alone.'
.print
.print 'After each kill, every database must reopen with integrity ok, be a recipe'
.print 'state, and hold at least the last user_version acknowledged for it.'

SELECT name, value FROM settings ORDER BY seq;

SELECT part, name, ran, note FROM parts ORDER BY part;

.print
.print '── A. Kills at a chosen fsync (strace) ─────────────────────────────────'
.print
.print 'A1. The uninterrupted run counts the fsync and fdatasync calls of the node.'
.print '    All of them run on its command thread. The process exits at the end of'
.print '    its input without closing, so no close-time sync is counted.'

SELECT mode, how, commands, acknowledged, command_thread_fsyncs, exit_code
FROM uninterrupted WHERE part = 'A' ORDER BY mode;

.print
.print 'A2. Four kill points per mode, evenly spaced over those calls: the first,'
.print '    the last, and two between. The file is the one the killed fsync was'
.print '    syncing; "a" is the directory of the node. during_command is the command'
.print '    the node was running (empty if the kill came after the last reply).'

SELECT mode, kill_no AS kill, at_fsync || ' of ' || of_fsyncs AS fsync, file,
       during_command_no || ': ' || during_command AS during_command, acknowledged, reopened
FROM fsync_kills ORDER BY mode, kill_no;

SELECT killed, count(*) AS kills FROM fsync_kills GROUP BY killed ORDER BY killed;

.print
.print 'A3. What each database held after the recovery. recovered is never below'
.print '    acknowledged; it can be above, when the kill hit after the commit but'
.print '    before the reply.'

SELECT mode, kill_no,
       string_agg(recovered_version::VARCHAR, ' ' ORDER BY database)    AS recovered,
       string_agg(acknowledged_version::VARCHAR, ' ' ORDER BY database) AS acknowledged,
       count(*) FILTER (integrity = 'ok')                               AS integrity_ok,
       count(*) FILTER (recovered_version >= acknowledged_version)      AS holds_acks
FROM recovery WHERE part = 'A' GROUP BY mode, kill_no ORDER BY mode, kill_no;

.print
.print 'A4. Every recovered database matched a recipe step:'

SELECT mode, recovered_state, count(*) AS databases
FROM recovery WHERE part = 'A' GROUP BY mode, recovered_state ORDER BY mode, recovered_state;

.print
.print '── B. Kills at a random time (portable) ───────────────────────────────'
.print
.print 'B1. One interactive run per mode is not killed and times its write loop.'
.print '    Each random kill then waits a random part of that time. A kill that'
.print '    lands after the last reply is recovered and checked too, then redrawn,'
.print '    at most 5 draws per kill. The delays and what each kill left depend on'
.print '    timing; they are in info_random_kills.'

SELECT mode, how, commands, acknowledged FROM uninterrupted WHERE part = 'B' ORDER BY mode;

SELECT mode, kill_no, in_flight, violations FROM random_kills ORDER BY mode, kill_no;

.print
.print '── C. Self-test: acknowledge before the segment put ────────────────────'
.print
.print 'C1. A mutant node acknowledges one store-durable write before it puts the'
.print '    segment, and is killed. Its reply says stored through position 0.'

SELECT reply, acknowledged FROM self_test ORDER BY reply;

.print
.print 'C2. The rebuild from the store finds the acknowledged write missing, and'
.print '    the checker reports it: the check can fail.'

SELECT database, acknowledged_version, recovered_version, recovered_state
FROM recovery WHERE part = 'C' ORDER BY database;

SELECT part, mode, kill_no, line FROM violations ORDER BY part, mode, kill_no, line;

.print
.print '── D. Every node process ──────────────────────────────────────────────'
.print
.print 'D1. Problems and over-bounds lines the nodes reported, across all runs.'
.print '    Both counts are 0; any line would be listed in the table problems.'

SELECT count(*) FILTER (kind = 'problem')     AS problems,
       count(*) FILTER (kind = 'over bounds') AS over_bounds
FROM problems;
```

Run from the repository root, with SeaweedFS running (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 10.2
```

Add `-i` (`go run ./cmd/course run -i 10.2`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries. To watch Parts A and B fail with `2 check(s) FAILED` and exit `1`, run the mutant directly in the container: `docker compose run --rm lab go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive -mutant ack-before-put`.

`course run` also prints the run-dependent values, which are not part of the stored evidence: where the store and the work directory were, how long the write loop of each unkilled interactive run took, and each random kill draw with its delay and the `user_version`s acknowledged and recovered per database.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/10.2-acknowledged-writes-survive/`](evidence/10.2-acknowledged-writes-survive/), and `go run ./cmd/course check evidence ch10` compares a fresh run with them byte for byte.

<!-- include: evidence/10.2-acknowledged-writes-survive.txt -->
```text

════════════════════════════════════════════════════════════════════════
  10.2  Does every acknowledged write survive a killed process?
════════════════════════════════════════════════════════════════════════

One node owns 4 databases and keeps at most 2 open, so every round-robin
write evicts one. Each killed run sends 12 writes in one mode:

  local-durable   acknowledged after the local commit at synchronous=FULL.
                  Recovery restarts the node on its own files.
  store-durable   acknowledged after the segment put to the S3 store.
                  Recovery deletes the directory of the node first and
                  rebuilds every database from the store alone.

After each kill, every database must reopen with integrity ok, be a recipe
state, and hold at least the last user_version acknowledged for it.
┌────────────────────────────┬──────────────┐
│            name            │    value     │
├────────────────────────────┼──────────────┤
│ bucket                     │ course-ch10b │
│ prefix                     │ exp-10-2/    │
│ databases                  │ 4            │
│ open_databases             │ 2            │
│ writes_per_run             │ 12           │
│ fsync_kill_points_per_mode │ 4            │
│ random_kills_per_mode      │ 2            │
│ random_seed                │ 1            │
│ synchronous                │ FULL         │
│ foreign_keys               │ ON           │
│ secure_delete              │ ON           │
│ cache_size                 │ -256         │
│ wal_autocheckpoint         │ 0            │
│ busy_timeout               │ 5000ms       │
└────────────────────────────┴──────────────┘
┌──────┬─────────────────────────────────────────────────────────────────────────────────┬──────┬──────┐
│ part │                                      name                                       │ ran  │ note │
├──────┼─────────────────────────────────────────────────────────────────────────────────┼──────┼──────┤
│ A    │ kills at a chosen fsync (strace)                                                │ true │      │
│ B    │ kills at a random time (portable)                                               │ true │      │
│ C    │ self-test: a node that acknowledges store-durable writes before the segment put │ true │      │
└──────┴─────────────────────────────────────────────────────────────────────────────────┴──────┴──────┘

── A. Kills at a chosen fsync (strace) ─────────────────────────────────

A1. The uninterrupted run counts the fsync and fdatasync calls of the node.
    All of them run on its command thread. The process exits at the end of
    its input without closing, so no close-time sync is counted.
┌───────────────┬────────────────────┬──────────┬──────────────┬───────────────────────┬───────────┐
│     mode      │        how         │ commands │ acknowledged │ command_thread_fsyncs │ exit_code │
├───────────────┼────────────────────┼──────────┼──────────────┼───────────────────────┼───────────┤
│ local-durable │ strace, not killed │ 12       │ 12           │ 77                    │ 0         │
│ store-durable │ strace, not killed │ 12       │ 12           │ 85                    │ 0         │
└───────────────┴────────────────────┴──────────┴──────────────┴───────────────────────┴───────────┘

A2. Four kill points per mode, evenly spaced over those calls: the first,
    the last, and two between. The file is the one the killed fsync was
    syncing; "a" is the directory of the node. during_command is the command
    the node was running (empty if the kill came after the last reply).
┌───────────────┬──────┬──────────┬────────────────┬─────────────────────────────────┬──────────────┬──────────┐
│     mode      │ kill │  fsync   │      file      │         during_command          │ acknowledged │ reopened │
├───────────────┼──────┼──────────┼────────────────┼─────────────────────────────────┼──────────────┼──────────┤
│ local-durable │ 1    │ 1 of 77  │ db-0001.db-wal │ 1: write db-0001 local-durable  │ 0            │ 4        │
│ local-durable │ 2    │ 26 of 77 │ db-0001.db-wal │ 5: write db-0001 local-durable  │ 4            │ 4        │
│ local-durable │ 3    │ 51 of 77 │ a              │ 9: write db-0001 local-durable  │ 8            │ 4        │
│ local-durable │ 4    │ 77 of 77 │ db-0004.db-wal │ 12: write db-0004 local-durable │ 11           │ 4        │
│ store-durable │ 1    │ 1 of 85  │ db-0001.db-wal │ 1: write db-0001 store-durable  │ 0            │ 4        │
│ store-durable │ 2    │ 29 of 85 │ catalog.db-wal │ 4: write db-0004 store-durable  │ 3            │ 4        │
│ store-durable │ 3    │ 57 of 85 │ catalog.db-wal │ 8: write db-0004 store-durable  │ 7            │ 4        │
│ store-durable │ 4    │ 85 of 85 │ catalog.db-wal │ 12: write db-0004 store-durable │ 11           │ 4        │
└───────────────┴──────┴──────────┴────────────────┴─────────────────────────────────┴──────────────┴──────────┘
┌─────────┬───────┐
│ killed  │ kills │
├─────────┼───────┤
│ SIGKILL │ 8     │
└─────────┴───────┘

A3. What each database held after the recovery. recovered is never below
    acknowledged; it can be above, when the kill hit after the commit but
    before the reply.
┌───────────────┬─────────┬───────────┬──────────────┬──────────────┬────────────┐
│     mode      │ kill_no │ recovered │ acknowledged │ integrity_ok │ holds_acks │
├───────────────┼─────────┼───────────┼──────────────┼──────────────┼────────────┤
│ local-durable │ 1       │ 0 0 0 0   │ 0 0 0 0      │ 4            │ 4          │
│ local-durable │ 2       │ 1 1 1 1   │ 1 1 1 1      │ 4            │ 4          │
│ local-durable │ 3       │ 2 2 2 2   │ 2 2 2 2      │ 4            │ 4          │
│ local-durable │ 4       │ 3 3 3 3   │ 3 3 3 2      │ 4            │ 4          │
│ store-durable │ 1       │ 0 0 0 0   │ 0 0 0 0      │ 4            │ 4          │
│ store-durable │ 2       │ 1 1 1 1   │ 1 1 1 0      │ 4            │ 4          │
│ store-durable │ 3       │ 2 2 2 2   │ 2 2 2 1      │ 4            │ 4          │
│ store-durable │ 4       │ 3 3 3 3   │ 3 3 3 2      │ 4            │ 4          │
└───────────────┴─────────┴───────────┴──────────────┴──────────────┴────────────┘

A4. Every recovered database matched a recipe step:
┌───────────────┬──────────────────────┬───────────┐
│     mode      │   recovered_state    │ databases │
├───────────────┼──────────────────────┼───────────┤
│ local-durable │ step 0 (exact bytes) │ 4         │
│ local-durable │ step 1 (exact bytes) │ 4         │
│ local-durable │ step 2 (exact bytes) │ 4         │
│ local-durable │ step 3 (exact bytes) │ 4         │
│ store-durable │ step 0 (exact bytes) │ 4         │
│ store-durable │ step 1 (exact bytes) │ 4         │
│ store-durable │ step 2 (exact bytes) │ 4         │
│ store-durable │ step 3 (exact bytes) │ 4         │
└───────────────┴──────────────────────┴───────────┘

── B. Kills at a random time (portable) ───────────────────────────────

B1. One interactive run per mode is not killed and times its write loop.
    Each random kill then waits a random part of that time. A kill that
    lands after the last reply is recovered and checked too, then redrawn,
    at most 5 draws per kill. The delays and what each kill left depend on
    timing; they are in info_random_kills.
┌───────────────┬─────────────────────────┬──────────┬──────────────┐
│     mode      │           how           │ commands │ acknowledged │
├───────────────┼─────────────────────────┼──────────┼──────────────┤
│ local-durable │ interactive, not killed │ 12       │ 12           │
│ store-durable │ interactive, not killed │ 12       │ 12           │
└───────────────┴─────────────────────────┴──────────┴──────────────┘
┌───────────────┬─────────┬───────────┬────────────┐
│     mode      │ kill_no │ in_flight │ violations │
├───────────────┼─────────┼───────────┼────────────┤
│ local-durable │ 1       │ true      │ 0          │
│ local-durable │ 2       │ true      │ 0          │
│ store-durable │ 1       │ true      │ 0          │
│ store-durable │ 2       │ true      │ 0          │
└───────────────┴─────────┴───────────┴────────────┘

── C. Self-test: acknowledge before the segment put ────────────────────

C1. A mutant node acknowledges one store-durable write before it puts the
    segment, and is killed. Its reply says stored through position 0.
┌────────────────────────────────────────────────────────────────────────────────────────────────────┬──────────────┐
│                                               reply                                                │ acknowledged │
├────────────────────────────────────────────────────────────────────────────────────────────────────┼──────────────┤
│ ok write db-0001 store-durable: acknowledged at epoch 1, user_version 1, stored through position 0 │ 1            │
└────────────────────────────────────────────────────────────────────────────────────────────────────┴──────────────┘

C2. The rebuild from the store finds the acknowledged write missing, and
    the checker reports it: the check can fail.
┌──────────┬──────────────────────┬───────────────────┬──────────────────────┐
│ database │ acknowledged_version │ recovered_version │   recovered_state    │
├──────────┼──────────────────────┼───────────────────┼──────────────────────┤
│ db-0001  │ 1                    │ 0                 │ step 0 (exact bytes) │
│ db-0002  │ 0                    │ 0                 │ step 0 (exact bytes) │
│ db-0003  │ 0                    │ 0                 │ step 0 (exact bytes) │
│ db-0004  │ 0                    │ 0                 │ step 0 (exact bytes) │
└──────────┴──────────────────────┴───────────────────┴──────────────────────┘
┌──────┬───────────────┬─────────┬───────────────────────────────────────────────┐
│ part │     mode      │ kill_no │                     line                      │
├──────┼───────────────┼─────────┼───────────────────────────────────────────────┤
│ C    │ store-durable │ 1       │ db-0001 recovered step 0, acknowledged step 1 │
└──────┴───────────────┴─────────┴───────────────────────────────────────────────┘

── D. Every node process ──────────────────────────────────────────────

D1. Problems and over-bounds lines the nodes reported, across all runs.
    Both counts are 0; any line would be listed in the table problems.
┌──────────┬─────────────┐
│ problems │ over_bounds │
├──────────┼─────────────┤
│ 0        │ 0           │
└──────────┴─────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  local-durable: the uninterrupted run acknowledges all 12 writes and every fsync runs on the command thread
A  ✓  local-durable: strace killed the node at each of the 4 fsync kill points
A  ✓  local-durable: after each kill at an fsync every database reopens (integrity ok, a recipe state) and holds at least every write acknowledged in this mode
A  ✓  store-durable: the uninterrupted run acknowledges all 12 writes and every fsync runs on the command thread
A  ✓  store-durable: strace killed the node at each of the 4 fsync kill points
A  ✓  store-durable: after each kill at an fsync every database reopens (integrity ok, a recipe state) and holds at least every write acknowledged in this mode
B  ✓  local-durable: an interactive run that is not killed acknowledges all 12 writes
B  ✓  local-durable: each of the 2 random kills landed while writes were in flight (a kill after the last reply is recovered and checked too, then redrawn, at most 5 draws per kill)
B  ✓  local-durable: after every kill at a random time every database reopens (integrity ok, a recipe state) and holds at least every acknowledged write
B  ✓  store-durable: an interactive run that is not killed acknowledges all 12 writes
B  ✓  store-durable: each of the 2 random kills landed while writes were in flight (a kill after the last reply is recovered and checked too, then redrawn, at most 5 draws per kill)
B  ✓  store-durable: after every kill at a random time every database reopens (integrity ok, a recipe state) and holds at least every acknowledged write
C  ✓  self-test: the checker finds the store-durable acknowledgement the store never received
D  ✓  no node reported a problem
D  ✓  after every command of every node process the descriptors and memory stayed within the standing bounds

15 of 15 claims hold

Experiment verdict: ACKNOWLEDGED WRITES SURVIVE OK
```

#### Reading the Experiment 10.2 evidence

- **Opening settings:** Pin check passes (the experiment's own output, shown when it is run directly); `1` node owns `4` databases with capacity `2` (so every round-robin write evicts one open database and triggers `Node.evicted`'s final ship and at-rest check); `local-durable` restarts on the node directory `a` (`/work/10.2/node/a/` in the container, `_work/10.2/node/a/` when run directly), while `store-durable` deletes that directory completely and rebuilds every database with `claim`.
- **A1–A4, `local-durable` (Part A — `local-durable` fsync kills):** An uninterrupted `12`-write run executes **`77` `fsync`/`fdatasync` calls**, all `77` on the `runtime.LockOSThread()` command thread (`0` on other threads). `strace` kills the node at evenly spaced indices `1`, `26`, `51`, and `77`:
  - `fsync 1` (`db-0001.db-wal`, command `1`): `0` acked, recovered `0 0 0 0`.
  - `fsync 26` (`db-0001.db-wal`, command `5`): `4` acked, recovered `1 1 1 1`.
  - `fsync 51` (directory `a`—SQLite's directory fsync after creating a `-wal` file—command `9`): `8` acked, recovered `2 2 2 2`.
  - `fsync 77` (`db-0004.db-wal`, command `12`): `11` acked (`3 3 3 2`), recovered **`3 3 3 3`**—the twelfth commit's WAL frames had already been written to the kernel before `walFrames` entered its final `fsync`, so `walIndexRecover` kept it (`recovered >= acknowledged`).
- **A1–A4, `store-durable` (Part A — `store-durable` fsync kills):** An uninterrupted `12`-write run executes **`85` `fsync`/`fdatasync` calls** (`8` more than `local-durable` because every write checkpoints immediately after shipping), all on the command thread. Kills land at `1` (`db-0001.db-wal`), `29`, `57`, and `85`—all three later kills land on **`catalog.db-wal`**, because updating the control-plane catalog is the final durable step after the S3 segment PUT already succeeded. Consequently, at `fsync 29`, `57`, and `85`, `db-0004`'s segment is already in SeaweedFS before the node replies, so store-only recovery rebuilds `1 1 1 1` vs acked `1 1 1 0`, `2 2 2 2` vs `2 2 2 1`, and `3 3 3 3` vs `3 3 3 2`. Every database reopens (`4` of `4`, integrity ok, valid recipe state) with `0` violations. Notice that store-only recovery costs one new epoch and one fence snapshot per database.
- **B1 (Part B — portable random-time kills):** Measures only the interactive write-loop duration (run-dependent: about `102ms` for `local-durable` and `112ms` for `store-durable` in one lab run), draws random delays inside that window, and redraws (up to `5` draws) if a kill ever lands after the last reply while still recovering and checking every draw. All `4` kills landed in flight (`in_flight` `true`) with `0` violations, so every database reopened and `recovered >= acknowledged` held in every run. Which draw landed in flight and how many writes each kill had acknowledged are run-dependent: in one lab run all `4` kills landed on draw `1` (`7–11` of `12` writes acknowledged). `course run` prints every draw.
- **C1–C2 (Part C — `ack-before-put` self-test):** The mutant node acknowledges `db-0001` `store-durable` with `stored through position 0` (nothing put to S3), is killed, and store-only recovery reports the violation `db-0001 recovered step 0, acknowledged step 1`, proving the checker catches broken store-durable implementations.
- **D1:** `0` node problems and `0` over-bounds lines; standing bounds held across every node reply (the reply count is run-dependent: **`308`** in one lab run); verdict `ACKNOWLEDGED WRITES SURVIVE OK`.

**What Experiment 10.2 does not show:** Power loss (Chapter 9.2's model applies: at `synchronous=FULL`, local-durable loses zero acknowledged commits if the disk honors `fsync`), kills during `Claim` or mid-`pwrite64` (covered by adversarial attacks `KILL-02a`–`KILL-05` below), or concurrent commands inside one node.

---

### 6.2 Experiment 10.3 — `interrupted-relocation`

- **Claim (invariant 6 — migration safety, plus invariants 1, 2, 4, and 5):** Across `32` fault cases injecting coordinator crashes, node `SIGKILL`s, store faults (`fail-before-put`, `fail-after-put`, `crash-before-put`, `crash-after-put`) on both the final segment (`log/000000000004`) and the destination fence (`log/000000000006`), and a rollback while the destination stays down—with a client probing status and writing at every step boundary—a fresh coordinator recovering via `Resume` or `Rollback` always leaves one owner (catalog, newest store slot, and `relocations` row agree), at most `1` writable node at every boundary, destination SHA-256 equal to the drained source state at the fence point, every acknowledged write (both `store-durable` and `local-durable`) in the one lineage and no refused write present, and the owner's closed file equal to the store's last state. Running the same relocation without the seal and store fence (`skip-fence` self-test) leaves `2` nodes writable at once and breaks the store lineage.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).

<!-- include: experiments/10.3-interrupted-relocation/main.go -->
```go
// Experiment 10.3 tests the migration safety invariant: an interrupted
// relocation cannot silently create two authoritative copies.
//
// Across 32 fault cases (coordinator stops after each of the 7 steps, source
// and destination SIGKILLs, store faults on the final segment and fence puts,
// and rolling back while the destination stays down), node a starts owning
// db-0001 with two store-durable commits and one unshipped local-durable commit.
// At every step boundary a client probes both nodes' status and sends writes to
// both the catalog owner and the other node. After each fault, a fresh
// relocate.Coordinator recovers from the catalog's relocations row and the S3
// store by Resume or Rollback. The experiment verifies one owner, at most one
// writable node at every boundary, E1 equality at the fence point, preservation
// of every acknowledged write (including local-durable writes drained by step
// 3), and standing resource bounds. A final self-test runs the skip-fence
// mutant (no seal and no store fence) to show two writable nodes appear and
// the lineage audit catches it.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.3
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/relocate"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

const id = "db-0001"

var (
	mutant  = flag.String("mutant", "", "skip-fence: relocate without sealing the source and without the store fence")
	only    = flag.Int("case", 0, "run only this case number (0 runs every case)")
	role    = flag.String("role", "", "internal: run as a service node")
	process node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-3", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 2, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: unused here")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.3" runs both.
// part labels the claims made while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, steps, cases, boundaries, slots, failedInvariants, problems, infoRun *results.Table
	}
	settingSeq int
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.steps = set.Table("relocation_steps", "step", "name")
	tables.cases = set.Table("cases", append([]string{"case_no"}, caseColumns...)...)
	tables.boundaries = set.Table("boundaries", "case_no", "seq", "label", "owner", "epoch", "store_end", "store_end_epoch", "store_end_kind", "node_a", "node_b")
	tables.slots = set.Table("slots", "case_no", "first", "last", "epoch", "kind")
	tables.failedInvariants = set.Table("failed_invariants", "case_no", "invariant")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.infoRun = set.Table("info_run", "name", "value")
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type faultCase struct {
	crashAfter relocate.Step
	kill       string
	killAfter  relocate.Step
	faultNode  string
	fault      string
	rollback   bool
	staysDown  bool
}

const finalSegment, fenceSlot = "log/000000000004", "log/000000000006"

// interrupts reports whether the injected fault is expected to stop Run before
// step 7 finishes. Killing destination b after step 6 (Activate) does not
// interrupt Run because step 7 (RetireSource) talks only to source a.
func (c faultCase) interrupts() bool {
	return !(c.kill == "b" && c.killAfter == relocate.Activate)
}

// describe returns c's injected fault, the node it hits, the step after which
// it hits (0 for a store fault), and the recovery strategy.
func (c faultCase) describe() (fault, faultNode string, after int, recovery string) {
	switch {
	case c.crashAfter != 0:
		fault, after = "coordinator crash", int(c.crashAfter)
	case c.kill != "":
		fault, faultNode, after = "SIGKILL", c.kill, int(c.killAfter)
	case c.fault != "":
		fault, faultNode = "store "+c.fault, c.faultNode
	default:
		fault = "no fault"
	}
	recovery = "resume"
	if c.rollback {
		recovery = "roll back"
	}
	if c.staysDown {
		recovery += fmt.Sprintf(" while node %s stays down", c.kill)
	}
	return fault, faultNode, after, recovery
}

func cases() []faultCase {
	var list []faultCase
	for _, step := range relocate.Steps() {
		list = append(list, faultCase{crashAfter: step})
	}
	for _, step := range relocate.Steps()[:relocate.Activate] {
		list = append(list, faultCase{crashAfter: step, rollback: true})
	}
	for _, k := range []struct {
		name  string
		after relocate.Step
	}{{"a", relocate.FenceEpoch}, {"a", relocate.SealSource}, {"a", relocate.ShipFinal}, {"b", relocate.Rebuild}, {"b", relocate.Activate}, {"a", relocate.Activate}} {
		list = append(list, faultCase{kill: k.name, killAfter: k.after})
	}
	list = append(list, faultCase{kill: "a", killAfter: relocate.SealSource, rollback: true}, faultCase{kill: "b", killAfter: relocate.Rebuild, rollback: true})
	for _, f := range []string{"fail-before-put", "fail-after-put", "crash-before-put", "crash-after-put"} {
		list = append(list, faultCase{faultNode: "a", fault: f + ":" + finalSegment + ":1"}, faultCase{faultNode: "b", fault: f + ":" + fenceSlot + ":1"})
	}
	list = append(list, faultCase{faultNode: "a", fault: "fail-before-put:" + finalSegment + ":1", rollback: true}, faultCase{faultNode: "b", fault: "crash-before-put:" + fenceSlot + ":1", rollback: true})
	list = append(list, faultCase{kill: "b", killAfter: relocate.Rebuild, rollback: true, staysDown: true})
	return list
}

type run struct {
	env          *lab.Env
	number       int
	skipFence    bool
	nodes        map[string]*node.Process
	acks         []node.Ack
	refused      int
	mostWritable int
	boundaries   int
	row          map[string]any
}

func (r *run) remote(name string) node.Remote {
	return node.Remote{
		Process: func() *node.Process { return r.nodes[name] },
		OnReply: func(_ string, reply node.Reply) { r.env.Tracker.Note(name, reply) },
	}
}

func (r *run) start(name, fault string) {
	p, err := r.env.Start(r.env.Config(name, fault))
	if err != nil {
		fatal("start node "+name, err)
	}
	r.nodes[name] = p
}

func (r *run) kill(name string) {
	if p := r.nodes[name]; p != nil {
		p.Signal()
		p.Reap()
	}
	r.nodes[name] = nil
}

func (r *run) alive(name string) bool {
	if r.nodes[name] == nil {
		return false
	}
	if _, err := r.remote(name).Status(id); err != nil {
		r.kill(name)
		return false
	}
	return true
}

func (r *run) write(name string, mode node.AckMode) {
	if !r.alive(name) {
		r.refused++
		return
	}
	reply, err := r.nodes[name].Send(fmt.Sprintf("write %s %s", id, mode))
	if err != nil {
		r.kill(name)
		r.refused++
		return
	}
	r.env.Tracker.Note(name, reply)
	if strings.HasPrefix(reply.Line, "refused ") {
		r.refused++
		return
	}
	_, ack, err := node.ParseAck(reply.Line)
	if err != nil {
		fatal("parse acknowledgement", err)
	}
	r.acks = append(r.acks, ack)
}

func (r *run) boundary(label string) {
	writable := 0
	var states []string
	for _, name := range []string{"a", "b"} {
		state := "down"
		if r.alive(name) {
			status, err := r.remote(name).Status(id)
			if err != nil {
				fatal("status", err)
			}
			state = status
			if strings.HasSuffix(status, "writable") {
				writable++
			}
		}
		states = append(states, state)
	}
	r.mostWritable = max(r.mostWritable, writable)
	record, err := r.env.Catalog.Get(r.env.Ctx, id)
	if err != nil {
		fatal("catalog", err)
	}
	var storeEnd, endEpoch, endKind any = "not one lineage", nil, nil
	if chain, err := fence.Chain(r.env.Ctx, r.env.Store, id); err == nil && len(chain) > 0 {
		end := chain[len(chain)-1]
		storeEnd, endEpoch, endKind = end.Last, end.Epoch, end.Kind
	}
	r.boundaries++
	tables.boundaries.Row(r.number, r.boundaries, label, record.Owner, record.Epoch, storeEnd, endEpoch, endKind, states[0], states[1])
	other := "a"
	if record.Owner == "a" {
		other = "b"
	}
	r.write(record.Owner, node.StoreDurable)
	r.write(other, node.LocalDurable)
}

func short(digest string) string {
	if len(digest) < 12 {
		return digest
	}
	return digest[:12]
}

func (r *run) coordinator() *relocate.Coordinator {
	return &relocate.Coordinator{Catalog: r.env.Catalog, Store: r.env.Store, Node: func(name string) relocate.Participant { return r.remote(name) }, SkipFence: r.skipFence}
}

func describe(err error) string {
	var stepErr *relocate.StepError
	switch {
	case errors.Is(err, relocate.ErrCrashed):
		return "the coordinator stopped"
	case errors.As(err, &stepErr):
		var refused *node.RefusedError
		if errors.As(err, &refused) {
			return fmt.Sprintf("step %d failed: the node refused", int(stepErr.Step))
		}
		return fmt.Sprintf("step %d failed: the node did not answer", int(stepErr.Step))
	}
	return "error: " + err.Error()
}

// caseColumns are the columns of the cases table after case_no.
var caseColumns = []string{"fault", "fault_node", "after_step", "recovery", "run", "restarted", "stayed_down", "discard_error", "recovery_error", "outcome", "relocation_state", "relocation_step", "fence_point", "fence_point_sha256", "fence", "owner", "epoch", "audit_error", "newest_slot_epoch", "acknowledged", "store_durable", "refused", "final_user_version", "final_state", "most_writable", "holds"}

// runCase runs one fault case and writes its row of the cases table; the
// columns a case does not reach stay empty.
func runCase(env *lab.Env, number int, c faultCase, skipFence bool) (bool, int) {
	if err := env.Reset([]string{id}); err != nil {
		fatal("reset", err)
	}
	r := &run{env: env, number: number, nodes: map[string]*node.Process{}, skipFence: skipFence, row: map[string]any{}}
	fault, faultNode, after, strategy := c.describe()
	r.row["fault"], r.row["recovery"] = fault, strategy
	if faultNode != "" {
		r.row["fault_node"] = faultNode
	}
	if after != 0 {
		r.row["after_step"] = after
	}
	defer func() {
		r.row["most_writable"] = r.mostWritable
		values := []any{number}
		for _, column := range caseColumns {
			values = append(values, r.row[column])
		}
		tables.cases.Row(values...)
	}()
	// Kill any remaining node process if recovery fails before verify stops them
	// cleanly, so no child holds open files under _work/10.3/ across cases.
	defer func() {
		for name, p := range r.nodes {
			if p != nil {
				r.kill(name)
			}
		}
	}()
	for _, name := range []string{"a", "b"} {
		fault := ""
		if c.faultNode == name {
			fault = c.fault
		}
		r.start(name, fault)
	}
	if _, err := r.remote("a").Claim(env.Ctx, id); err != nil {
		fatal("claim", err)
	}
	r.write("a", node.StoreDurable)
	r.write("a", node.StoreDurable)
	r.write("a", node.LocalDurable)
	coordinator := r.coordinator()
	coordinator.CrashAfter = c.crashAfter
	coordinator.AfterStep = func(step relocate.Step) {
		r.boundary(fmt.Sprintf("after step %d", int(step)))
		if c.kill != "" && step == c.killAfter {
			r.kill(c.kill)
		}
	}
	result, err := coordinator.Run(env.Ctx, id, "a", "b")
	interrupted := err != nil
	if err != nil {
		r.row["run"] = describe(err)
	} else {
		r.row["run"] = fmt.Sprintf("finished without interruption (%s)", result.Outcome)
	}
	var restarted []string
	for _, name := range []string{"a", "b"} {
		if c.staysDown && name == c.kill {
			r.kill(name)
			r.row["stayed_down"] = name
			continue
		}
		if !r.alive(name) {
			restarted = append(restarted, name)
			r.start(name, "")
		}
	}
	if len(restarted) > 0 {
		r.row["restarted"] = strings.Join(restarted, " ")
	}
	recovery := r.coordinator()
	if interrupted && c.rollback {
		result, err = recovery.Rollback(env.Ctx, id)
	} else if interrupted {
		result, err = recovery.Resume(env.Ctx, id)
	}
	if err != nil && !errors.Is(err, relocate.ErrFenceInStore) {
		r.row["recovery_error"] = err.Error()
		return false, r.mostWritable
	}
	if result.DiscardErr != nil {
		r.row["discard_error"] = result.DiscardErr.Error()
	}
	if c.staysDown {
		r.start(c.kill, "")
	}
	relocation, err := env.Catalog.GetRelocation(env.Ctx, id)
	if err != nil {
		fatal("relocation record", err)
	}
	r.row["outcome"], r.row["relocation_state"], r.row["relocation_step"] = result.Outcome, relocation.State, relocation.Step
	r.row["fence_point"], r.row["fence_point_sha256"], r.row["fence"] = result.Position, short(result.SHA256), result.Fence
	r.boundary("after recovery")
	return r.verify(c, interrupted, result, relocation), r.mostWritable
}

func (r *run) verify(c faultCase, interrupted bool, result relocate.Result, relocation catalog.Relocation) bool {
	env := r.env
	for name, p := range r.nodes {
		if _, err := env.Stop(p); err != nil {
			fatal("stop node "+name, err)
		}
		r.nodes[name] = nil
	}
	record, err := env.Catalog.Get(env.Ctx, id)
	if err != nil {
		fatal("catalog", err)
	}
	r.row["owner"], r.row["epoch"] = record.Owner, record.Epoch
	chain, restorable, err := fence.Audit(env.Ctx, env.Store, id)
	if err != nil {
		r.row["audit_error"] = err.Error()
		return false
	}
	for _, slot := range chain {
		tables.slots.Row(r.number, slot.First, slot.Last, slot.Epoch, slot.Kind)
	}
	last := chain[len(chain)-1]
	restore := func(position int64) []byte {
		image, err := fence.RestoreAt(restorable, position)
		if err != nil {
			fatal(fmt.Sprintf("restore at %d", position), err)
		}
		return image
	}
	final := restore(last.Last)
	finalState, err := env.MatchImage(final)
	if err != nil {
		fatal("match", err)
	}
	owner := "b"
	wantState := catalog.RelocationDone
	if result.Outcome == relocate.RolledBack {
		owner, wantState = "a", catalog.RelocationRolledBack
	}
	fencePosition, fenceSHA := relocation.Position, relocation.SHA256
	if result.Outcome == relocate.RolledBack {
		fencePosition, fenceSHA = result.Position, result.SHA256
	}
	if fencePosition < 1 || result.Fence != fencePosition+1 {
		tables.failedInvariants.Row(r.number, fmt.Sprintf("the recovery names fence point %d and fence %d", fencePosition, result.Fence))
		return false
	}
	atFence, fenced := restore(fencePosition), restore(result.Fence)
	storeDurable := 0
	acksInLineage := true
	for _, ack := range r.acks {
		if ack.Mode != node.StoreDurable {
			continue
		}
		storeDurable++
		found := false
		for _, slot := range chain {
			if slot.Epoch == ack.Epoch && slot.First <= ack.Position && ack.Position <= slot.Last && int(binary.BigEndian.Uint32(restore(ack.Position)[60:64])) == ack.UserVersion {
				found = true
			}
		}
		acksInLineage = acksInLineage && found
	}
	status, err := env.AtRestSHA256(owner, id)
	if err != nil {
		fatal("owner file", err)
	}
	r.row["newest_slot_epoch"], r.row["acknowledged"], r.row["store_durable"], r.row["refused"] = last.Epoch, len(r.acks), storeDurable, r.refused
	r.row["final_user_version"], r.row["final_state"] = finalState.UserVersion, finalState.Match
	parts := []struct {
		ok   bool
		what string
	}{
		{interrupted == c.interrupts(), "the fault interrupted the relocation, or (a destination killed after activation) did not"},
		{relocation.State == wantState && record.Owner == owner && record.Epoch == last.Epoch, "one owner: the catalog, the newest slot and the relocation record agree"},
		{r.mostWritable <= 1, "at most one node accepted writes at every step boundary"},
		{walship.SHA256Hex(atFence) == fenceSHA && walship.SHA256Hex(fenced) == fenceSHA, "the rebuilt copy equals the source at the fenced commit"},
		{acksInLineage && finalState.RecipeState && finalState.UserVersion == len(r.acks), "every acknowledged write is in the one lineage and no refused write is"},
		{status == walship.SHA256Hex(final), "the owner's closed file equals the store's last state"},
		{!c.staysDown || (result.Outcome == relocate.RolledBack && result.DiscardErr != nil), "a rollback while the destination is down finishes without the destination"},
	}
	ok := true
	for _, part := range parts {
		if !part.ok {
			tables.failedInvariants.Row(r.number, part.what)
			ok = false
		}
	}
	r.row["holds"] = ok
	return ok
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *mutant != "" && *mutant != "skip-fence" {
		fatal("-mutant", fmt.Errorf("unknown mutant %q (want skip-fence)", *mutant))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.3", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	set = results.MustOpen(env.Work)
	openTables()
	// env.Reset empties env.Work before every case, so the nodes work in a
	// subdirectory and the tables in env.Work/results survive.
	env.Work = filepath.Join(env.Work, "nodes")
	env.Process.Work = env.Work
	if err := os.MkdirAll(env.Work, 0o755); err != nil {
		fatal("work directory", err)
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", env.ShownWork())
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	if *mutant != "" {
		setting("mutant", *mutant)
	}
	for _, pragma := range strings.Fields(node.PlatformSettings().String()) {
		name, value, _ := strings.Cut(pragma, "=")
		setting(name, value)
	}
	for _, step := range relocate.Steps() {
		_, name, _ := strings.Cut(step.String(), " (")
		tables.steps.Row(int(step), strings.TrimSuffix(name, ")"))
	}
	part = "A"
	for i, c := range cases() {
		if *only != 0 && *only != i+1 {
			continue
		}
		ok, _ := runCase(env, i+1, c, *mutant == "skip-fence")
		check(ok, fmt.Sprintf("case %d: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present", i+1))
	}
	if *mutant == "" && *only == 0 {
		part = "B"
		_, writable := runCase(env, 0, faultCase{}, true)
		check(writable >= 2, "self-test: without the fence the boundary check finds two writable nodes")
	}
	part = "C"
	seq := 0
	for _, kind := range []struct {
		name  string
		lines []string
	}{{"problem", env.Tracker.Problems}, {"over bounds", env.Tracker.OverBounds}} {
		for _, line := range kind.lines {
			seq++
			tables.problems.Row(seq, kind.name, line)
		}
	}
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every node process the descriptors and memory stayed within the standing bounds")
	check(len(env.Tracker.Problems) == 0, "no node reported a problem")
	tables.infoRun.Row("node replies that carried a bounds check", env.Tracker.Replies)
	set.Finish("INTERRUPTED RELOCATION")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/10.3-interrupted-relocation/report.sql -->
```sql
-- Walkthrough of experiment 10.3. Run it with:
--   go run ./cmd/course run 10.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings            one setting of the run (name, value)
--   relocation_steps    one of the seven steps of a relocation
--   cases               one fault case: the fault, the recovery and the final state (case 0 is the self-test)
--   boundaries          one step boundary of one case: the catalog, the end of the store, both nodes
--   slots               one slot of the store chain after one case
--   failed_invariants   one invariant a case broke (none on a good run)
--   problems            one problem or over-bounds line a node reported (none on a good run)
--   checks              every claim the experiment asserted (the runner lists it last)
-- info_run holds the store endpoint, the work directory and the number of node
-- replies that carried a bounds check; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.3  Can an interrupted relocation leave two authoritative copies?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Node a (the source) moves db-0001 to node b (the destination). Both share'
.print 'the catalog and the S3 store. In each case, a claims db-0001 and writes'
.print 'steps 1-2 store-durable and step 3 local-durable. At every step boundary'
.print 'the client writes store-durable to the node the catalog names and'
.print 'local-durable to the other. One fault per case; then a fresh coordinator'
.print 'resumes or rolls back from the relocations row and the store.'

SELECT name, value FROM settings ORDER BY seq;

.print
.print 'The seven steps of a relocation:'

SELECT step, name FROM relocation_steps ORDER BY step;

.print
.print '── A. Thirty-two fault cases ───────────────────────────────────────────'
.print
.print 'A1. The faults. Cases 1-13 stop the coordinator after a step; 14-21 kill'
.print '    a node after a step; 22-31 make one store put fail or crash the node'
.print '    around it (log/000000000004 is the final segment of a, 000000000006'
.print '    the fence of b); 32 rolls back while the destination stays down.'

SELECT case_no AS "case", fault, fault_node AS node, after_step AS after, recovery
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A2. How each run stopped and how the recovery ended. Killing b after step'
.print '    6 (case 18) does not interrupt the run: step 7 talks only to a. A'
.print '    roll back after step 6 rolls forward instead (case 13): the fence of b'
.print '    is already in the store.'

SELECT case_no AS "case", run, restarted, outcome, relocation_state AS state,
       relocation_step AS step
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A3. Case 32: node b stays down during the roll back. The roll back still'
.print '    finishes; b keeps a staged copy it could not be told to discard, and'
.print '    is restarted after the recovery.'

SELECT case_no AS "case", stayed_down, discard_error
FROM cases WHERE stayed_down IS NOT NULL OR discard_error IS NOT NULL ORDER BY case_no;

.print
.print 'A4. The fence point is the last position of the source; the fence is the'
.print '    snapshot the new owner puts right after it. The rebuilt copy equals the'
.print '    source there (the same sha256). A roll back fences a again, at epoch 3.'

SELECT case_no AS "case", fence_point, fence_point_sha256 AS sha256_prefix, fence,
       owner, epoch, newest_slot_epoch AS slot_epoch
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A5. Writes. Every acknowledged write is in the one lineage and no refused'
.print '    write is: the final user_version equals the number acknowledged. The'
.print '    final state is that recipe step; its bytes differ only in the change'
.print '    counter (bytes 24-27 and 92-95).'

SELECT case_no AS "case", acknowledged, store_durable, refused,
       final_user_version AS user_version,
       regexp_extract(final_state, '^step \d+')                               AS recipe,
       CASE WHEN final_state LIKE '%exact%' THEN 'exact' ELSE 'counter differs' END AS vs_recipe_file,
       most_writable, holds
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A6. The step boundaries of four cases. In case 7 the coordinator stops'
.print '    after the last step; in case 16, a is killed after step 3; in case 18,'
.print '    b is killed after step 6; in case 32, b stays down. Before step 6 only'
.print '    a is writable; from step 6 on only b is. store_end is the last slot.'

SELECT case_no AS "case", label, owner, epoch,
       CASE WHEN store_end_epoch IS NULL THEN store_end
            ELSE store_end || ' e' || store_end_epoch || ' ' || store_end_kind END AS store_end,
       node_a, node_b
FROM boundaries WHERE case_no IN (7, 16, 18, 32) ORDER BY case_no, seq;

.print
.print 'A7. Every boundary of every case: how many nodes accepted writes. In the'
.print '    fault cases never more than one, and none while a is sealed and b is'
.print '    not yet active. The self-test (part B) reaches two.'

SELECT CASE WHEN case_no = 0 THEN 'self-test' ELSE 'fault cases' END AS cases,
       (node_a LIKE '%writable')::INT + (node_b LIKE '%writable')::INT AS writable_nodes,
       count(*) AS boundaries
FROM boundaries GROUP BY ALL ORDER BY cases, writable_nodes;

.print
.print 'A8. The store after each case: one lineage, epochs never decreasing, and'
.print '    each new epoch opens with a fence snapshot. Each slot is shown as'
.print '    positions, epoch, and snap (snapshot) or seg (segment).'

SELECT case_no AS "case",
       string_agg(first || '-' || last || ' e' || epoch || ' ' ||
                  CASE kind WHEN 'snapshot' THEN 'snap' WHEN 'segment' THEN 'seg' ELSE kind END,
                  '; ' ORDER BY first) AS store
FROM slots WHERE case_no > 0 GROUP BY case_no ORDER BY case_no;

.print
.print '    In some cases the final segment never reached the store, and the close'
.print '    of the source had already checkpointed its frames into the .db file. The'
.print '    retried drain found no WAL and shipped a discontinuity snapshot at'
.print '    position 4 instead of a segment, in these cases:'

SELECT string_agg(case_no::VARCHAR, ' ' ORDER BY case_no) AS cases, first AS position, epoch, kind
FROM slots WHERE case_no > 0 AND kind = 'snapshot' AND epoch = 1 AND first > 1
GROUP BY first, epoch, kind ORDER BY first;

.print
.print 'A9. The cases that hold every invariant, and the invariants broken (none):'

SELECT count(*) FILTER (holds) || ' of ' || count(*) AS cases_holding_every_invariant,
       (SELECT count(*) FROM failed_invariants) AS broken_invariants
FROM cases WHERE case_no > 0;

.print
.print '── B. Self-test: the same relocation without the seal and the fence ────'
.print
.print 'B1. The skip-fence mutant does not seal a and does not put the fence of'
.print '    b. From step 6 on, both nodes are writable, and the store is no longer'
.print '    one lineage: epoch 2 starts with a segment, not a fence snapshot.'

SELECT run, outcome, relocation_state AS state, fence_point, fence, most_writable
FROM cases WHERE case_no = 0 ORDER BY case_no;

SELECT label, owner, epoch,
       CASE WHEN store_end_epoch IS NULL THEN store_end
            ELSE store_end || ' e' || store_end_epoch || ' ' || store_end_kind END AS store_end,
       node_a, node_b
FROM boundaries WHERE case_no = 0 ORDER BY seq;

SELECT audit_error FROM cases WHERE case_no = 0 ORDER BY case_no;

.print
.print '── C. Every node process ───────────────────────────────────────────────'
.print
.print 'C1. Problems and over-bounds lines the nodes reported, across all cases.'
.print '    Both counts are 0; any line would be listed in the table problems.'

SELECT count(*) FILTER (kind = 'problem')     AS problems,
       count(*) FILTER (kind = 'over bounds') AS over_bounds
FROM problems;
```

Run from the repository root, with SeaweedFS running (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 10.3
```

Add `-i` (`go run ./cmd/course run -i 10.3`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries. To run one case (`-case N`), or `-mutant skip-fence -case 1` to see the invariant check fail and exit `1`, run the experiment directly in the container: `docker compose run --rm lab go run ./ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation -case N`.

`course run` also prints the run-dependent values, which are not part of the stored evidence: where the store and the work directory were (node directories under `nodes/`), and how many node replies carried a bounds check.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/10.3-interrupted-relocation/`](evidence/10.3-interrupted-relocation/), and `go run ./cmd/course check evidence ch10` compares a fresh run with them byte for byte.

<!-- include: evidence/10.3-interrupted-relocation.txt -->
```text

════════════════════════════════════════════════════════════════════════
  10.3  Can an interrupted relocation leave two authoritative copies?
════════════════════════════════════════════════════════════════════════

Node a (the source) moves db-0001 to node b (the destination). Both share
the catalog and the S3 store. In each case, a claims db-0001 and writes
steps 1-2 store-durable and step 3 local-durable. At every step boundary
the client writes store-durable to the node the catalog names and
local-durable to the other. One fault per case; then a fresh coordinator
resumes or rolls back from the relocations row and the store.
┌────────────────────┬──────────────┐
│        name        │    value     │
├────────────────────┼──────────────┤
│ bucket             │ course-ch10b │
│ prefix             │ exp-10-3/    │
│ synchronous        │ FULL         │
│ foreign_keys       │ ON           │
│ secure_delete      │ ON           │
│ cache_size         │ -256         │
│ wal_autocheckpoint │ 0            │
│ busy_timeout       │ 5000ms       │
└────────────────────┴──────────────┘

The seven steps of a relocation:
┌──────┬─────────────────────────────────────────────────────┐
│ step │                        name                         │
├──────┼─────────────────────────────────────────────────────┤
│ 1    │ fence the source: bump the epoch in the catalog     │
│ 2    │ seal the source                                     │
│ 3    │ ship the final segment and close the source at rest │
│ 4    │ rebuild at the destination from the store           │
│ 5    │ verify the rebuilt hash against the source's        │
│ 6    │ activate: put the destination's fence in the store  │
│ 7    │ retire the source                                   │
└──────┴─────────────────────────────────────────────────────┘

── A. Thirty-two fault cases ───────────────────────────────────────────

A1. The faults. Cases 1-13 stop the coordinator after a step; 14-21 kill
    a node after a step; 22-31 make one store put fail or crash the node
    around it (log/000000000004 is the final segment of a, 000000000006
    the fence of b); 32 rolls back while the destination stays down.
┌──────┬───────────────────────────────────────────┬──────┬───────┬───────────────────────────────────┐
│ case │                   fault                   │ node │ after │             recovery              │
├──────┼───────────────────────────────────────────┼──────┼───────┼───────────────────────────────────┤
│ 1    │ coordinator crash                         │      │ 1     │ resume                            │
│ 2    │ coordinator crash                         │      │ 2     │ resume                            │
│ 3    │ coordinator crash                         │      │ 3     │ resume                            │
│ 4    │ coordinator crash                         │      │ 4     │ resume                            │
│ 5    │ coordinator crash                         │      │ 5     │ resume                            │
│ 6    │ coordinator crash                         │      │ 6     │ resume                            │
│ 7    │ coordinator crash                         │      │ 7     │ resume                            │
│ 8    │ coordinator crash                         │      │ 1     │ roll back                         │
│ 9    │ coordinator crash                         │      │ 2     │ roll back                         │
│ 10   │ coordinator crash                         │      │ 3     │ roll back                         │
│ 11   │ coordinator crash                         │      │ 4     │ roll back                         │
│ 12   │ coordinator crash                         │      │ 5     │ roll back                         │
│ 13   │ coordinator crash                         │      │ 6     │ roll back                         │
│ 14   │ SIGKILL                                   │ a    │ 1     │ resume                            │
│ 15   │ SIGKILL                                   │ a    │ 2     │ resume                            │
│ 16   │ SIGKILL                                   │ a    │ 3     │ resume                            │
│ 17   │ SIGKILL                                   │ b    │ 4     │ resume                            │
│ 18   │ SIGKILL                                   │ b    │ 6     │ resume                            │
│ 19   │ SIGKILL                                   │ a    │ 6     │ resume                            │
│ 20   │ SIGKILL                                   │ a    │ 2     │ roll back                         │
│ 21   │ SIGKILL                                   │ b    │ 4     │ roll back                         │
│ 22   │ store fail-before-put:log/000000000004:1  │ a    │       │ resume                            │
│ 23   │ store fail-before-put:log/000000000006:1  │ b    │       │ resume                            │
│ 24   │ store fail-after-put:log/000000000004:1   │ a    │       │ resume                            │
│ 25   │ store fail-after-put:log/000000000006:1   │ b    │       │ resume                            │
│ 26   │ store crash-before-put:log/000000000004:1 │ a    │       │ resume                            │
│ 27   │ store crash-before-put:log/000000000006:1 │ b    │       │ resume                            │
│ 28   │ store crash-after-put:log/000000000004:1  │ a    │       │ resume                            │
│ 29   │ store crash-after-put:log/000000000006:1  │ b    │       │ resume                            │
│ 30   │ store fail-before-put:log/000000000004:1  │ a    │       │ roll back                         │
│ 31   │ store crash-before-put:log/000000000006:1 │ b    │       │ roll back                         │
│ 32   │ SIGKILL                                   │ b    │ 4     │ roll back while node b stays down │
└──────┴───────────────────────────────────────────┴──────┴───────┴───────────────────────────────────┘

A2. How each run stopped and how the recovery ended. Killing b after step
    6 (case 18) does not interrupt the run: step 7 talks only to a. A
    roll back after step 6 rolls forward instead (case 13): the fence of b
    is already in the store.
┌──────┬────────────────────────────────────────┬───────────┬────────────────┬─────────────┬──────┐
│ case │                  run                   │ restarted │    outcome     │    state    │ step │
├──────┼────────────────────────────────────────┼───────────┼────────────────┼─────────────┼──────┤
│ 1    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 2    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 3    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 4    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 5    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 6    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 7    │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 8    │ the coordinator stopped                │           │ rolled back    │ rolled-back │ 1    │
│ 9    │ the coordinator stopped                │           │ rolled back    │ rolled-back │ 1    │
│ 10   │ the coordinator stopped                │           │ rolled back    │ rolled-back │ 2    │
│ 11   │ the coordinator stopped                │           │ rolled back    │ rolled-back │ 3    │
│ 12   │ the coordinator stopped                │           │ rolled back    │ rolled-back │ 4    │
│ 13   │ the coordinator stopped                │           │ rolled forward │ done        │ 7    │
│ 14   │ step 2 failed: the node did not answer │ a         │ rolled forward │ done        │ 7    │
│ 15   │ step 3 failed: the node did not answer │ a         │ rolled forward │ done        │ 7    │
│ 16   │ step 7 failed: the node did not answer │ a         │ rolled forward │ done        │ 7    │
│ 17   │ step 6 failed: the node did not answer │ b         │ rolled forward │ done        │ 7    │
│ 18   │ finished without interruption (done)   │ b         │ done           │ done        │ 7    │
│ 19   │ step 7 failed: the node did not answer │ a         │ rolled forward │ done        │ 7    │
│ 20   │ step 3 failed: the node did not answer │ a         │ rolled back    │ rolled-back │ 2    │
│ 21   │ step 6 failed: the node did not answer │ b         │ rolled back    │ rolled-back │ 5    │
│ 22   │ step 3 failed: the node refused        │           │ rolled forward │ done        │ 7    │
│ 23   │ step 6 failed: the node refused        │           │ rolled forward │ done        │ 7    │
│ 24   │ step 3 failed: the node refused        │           │ rolled forward │ done        │ 7    │
│ 25   │ step 6 failed: the node refused        │           │ rolled forward │ done        │ 7    │
│ 26   │ step 3 failed: the node did not answer │ a         │ rolled forward │ done        │ 7    │
│ 27   │ step 6 failed: the node did not answer │ b         │ rolled forward │ done        │ 7    │
│ 28   │ step 3 failed: the node did not answer │ a         │ rolled forward │ done        │ 7    │
│ 29   │ step 6 failed: the node did not answer │ b         │ rolled forward │ done        │ 7    │
│ 30   │ step 3 failed: the node refused        │           │ rolled back    │ rolled-back │ 2    │
│ 31   │ step 6 failed: the node did not answer │ b         │ rolled back    │ rolled-back │ 5    │
│ 32   │ step 6 failed: the node did not answer │           │ rolled back    │ rolled-back │ 5    │
└──────┴────────────────────────────────────────┴───────────┴────────────────┴─────────────┴──────┘

A3. Case 32: node b stays down during the roll back. The roll back still
    finishes; b keeps a staged copy it could not be told to discard, and
    is restarted after the recovery.
┌──────┬─────────────┬───────────────────────────────────────────────────┐
│ case │ stayed_down │                   discard_error                   │
├──────┼─────────────┼───────────────────────────────────────────────────┤
│ 32   │ b           │ node process for "discard db-0001" is not running │
└──────┴─────────────┴───────────────────────────────────────────────────┘

A4. The fence point is the last position of the source; the fence is the
    snapshot the new owner puts right after it. The rebuilt copy equals the
    source there (the same sha256). A roll back fences a again, at epoch 3.
┌──────┬─────────────┬───────────────┬───────┬───────┬───────┬────────────┐
│ case │ fence_point │ sha256_prefix │ fence │ owner │ epoch │ slot_epoch │
├──────┼─────────────┼───────────────┼───────┼───────┼───────┼────────────┤
│ 1    │ 4           │ c75549790ef1  │ 5     │ b     │ 2     │ 2          │
│ 2    │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 3    │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 4    │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 5    │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 6    │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 7    │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 8    │ 4           │ c75549790ef1  │ 5     │ a     │ 3     │ 3          │
│ 9    │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
│ 10   │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
│ 11   │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
│ 12   │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
│ 13   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 14   │ 4           │ d53fc07b1293  │ 5     │ b     │ 2     │ 2          │
│ 15   │ 4           │ d53fc07b1293  │ 5     │ b     │ 2     │ 2          │
│ 16   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 17   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 18   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 19   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 20   │ 4           │ d53fc07b1293  │ 5     │ a     │ 3     │ 3          │
│ 21   │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
│ 22   │ 4           │ d53fc07b1293  │ 5     │ b     │ 2     │ 2          │
│ 23   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 24   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 25   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 26   │ 4           │ d53fc07b1293  │ 5     │ b     │ 2     │ 2          │
│ 27   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 28   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 29   │ 5           │ d53fc07b1293  │ 6     │ b     │ 2     │ 2          │
│ 30   │ 4           │ d53fc07b1293  │ 5     │ a     │ 3     │ 3          │
│ 31   │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
│ 32   │ 5           │ d53fc07b1293  │ 6     │ a     │ 3     │ 3          │
└──────┴─────────────┴───────────────┴───────┴───────┴───────┴────────────┘

A5. Writes. Every acknowledged write is in the one lineage and no refused
    write is: the final user_version equals the number acknowledged. The
    final state is that recipe step; its bytes differ only in the change
    counter (bytes 24-27 and 92-95).
┌──────┬──────────────┬───────────────┬─────────┬──────────────┬────────┬─────────────────┬───────────────┬───────┐
│ case │ acknowledged │ store_durable │ refused │ user_version │ recipe │ vs_recipe_file  │ most_writable │ holds │
├──────┼──────────────┼───────────────┼─────────┼──────────────┼────────┼─────────────────┼───────────────┼───────┤
│ 1    │ 4            │ 3             │ 1       │ 4            │ step 4 │ counter differs │ 1             │ true  │
│ 2    │ 5            │ 3             │ 2       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 3    │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 4    │ 5            │ 3             │ 6       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 5    │ 5            │ 3             │ 8       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 6    │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 7    │ 6            │ 4             │ 11      │ 6            │ step 6 │ counter differs │ 1             │ true  │
│ 8    │ 4            │ 3             │ 1       │ 4            │ step 4 │ counter differs │ 1             │ true  │
│ 9    │ 5            │ 3             │ 2       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 10   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 11   │ 5            │ 3             │ 6       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 12   │ 5            │ 3             │ 8       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 13   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 14   │ 5            │ 3             │ 2       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 15   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 16   │ 6            │ 4             │ 11      │ 6            │ step 6 │ counter differs │ 1             │ true  │
│ 17   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 18   │ 6            │ 4             │ 13      │ 6            │ step 6 │ counter differs │ 1             │ true  │
│ 19   │ 6            │ 4             │ 11      │ 6            │ step 6 │ counter differs │ 1             │ true  │
│ 20   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 21   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 22   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 23   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 24   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 25   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 26   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 27   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 28   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 29   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 30   │ 5            │ 3             │ 4       │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 31   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
│ 32   │ 5            │ 3             │ 10      │ 5            │ step 5 │ counter differs │ 1             │ true  │
└──────┴──────────────┴───────────────┴─────────┴──────────────┴────────┴─────────────────┴───────────────┴───────┘

A6. The step boundaries of four cases. In case 7 the coordinator stops
    after the last step; in case 16, a is killed after step 3; in case 18,
    b is killed after step 6; in case 32, b stays down. Before step 6 only
    a is writable; from step 6 on only b is. store_end is the last slot.
┌──────┬────────────────┬───────┬───────┬───────────────┬────────────────────────────┬────────────────────────────┐
│ case │     label      │ owner │ epoch │   store_end   │           node_a           │           node_b           │
├──────┼────────────────┼───────┼───────┼───────────────┼────────────────────────────┼────────────────────────────┤
│ 7    │ after step 1   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, writable │ not owner                  │
│ 7    │ after step 2   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 7    │ after step 3   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 7    │ after step 4   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 7    │ after step 5   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 7    │ after step 6   │ b     │ 2     │ 6 e2 snapshot │ owner at epoch 1, sealed   │ owner at epoch 2, writable │
│ 7    │ after recovery │ b     │ 2     │ 7 e2 segment  │ not owner                  │ owner at epoch 2, writable │
│ 16   │ after step 1   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, writable │ not owner                  │
│ 16   │ after step 2   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 16   │ after step 3   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 16   │ after step 4   │ b     │ 2     │ 5 e1 segment  │ down                       │ not owner                  │
│ 16   │ after step 5   │ b     │ 2     │ 5 e1 segment  │ down                       │ not owner                  │
│ 16   │ after step 6   │ b     │ 2     │ 6 e2 snapshot │ down                       │ owner at epoch 2, writable │
│ 16   │ after recovery │ b     │ 2     │ 7 e2 segment  │ not owner                  │ owner at epoch 2, writable │
│ 18   │ after step 1   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, writable │ not owner                  │
│ 18   │ after step 2   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 18   │ after step 3   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 18   │ after step 4   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 18   │ after step 5   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 18   │ after step 6   │ b     │ 2     │ 6 e2 snapshot │ owner at epoch 1, sealed   │ owner at epoch 2, writable │
│ 18   │ after step 7   │ b     │ 2     │ 7 e2 segment  │ not owner                  │ down                       │
│ 18   │ after recovery │ b     │ 2     │ 7 e2 segment  │ not owner                  │ owner at epoch 2, writable │
│ 32   │ after step 1   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, writable │ not owner                  │
│ 32   │ after step 2   │ b     │ 2     │ 3 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 32   │ after step 3   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 32   │ after step 4   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ not owner                  │
│ 32   │ after step 5   │ b     │ 2     │ 5 e1 segment  │ owner at epoch 1, sealed   │ down                       │
│ 32   │ after recovery │ a     │ 3     │ 6 e3 snapshot │ owner at epoch 3, writable │ not owner                  │
└──────┴────────────────┴───────┴───────┴───────────────┴────────────────────────────┴────────────────────────────┘

A7. Every boundary of every case: how many nodes accepted writes. In the
    fault cases never more than one, and none while a is sealed and b is
    not yet active. The self-test (part B) reaches two.
┌─────────────┬────────────────┬────────────┐
│    cases    │ writable_nodes │ boundaries │
├─────────────┼────────────────┼────────────┤
│ fault cases │ 0              │ 76         │
│ fault cases │ 1              │ 66         │
│ self-test   │ 1              │ 5          │
│ self-test   │ 2              │ 3          │
└─────────────┴────────────────┴────────────┘

A8. The store after each case: one lineage, epochs never decreasing, and
    each new epoch opens with a fence snapshot. Each slot is shown as
    positions, epoch, and snap (snapshot) or seg (segment).
┌──────┬──────────────────────────────────────────────────────────────────────────────────────┐
│ case │                                        store                                         │
├──────┼──────────────────────────────────────────────────────────────────────────────────────┤
│ 1    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 seg; 5-5 e2 snap; 6-6 e2 seg             │
│ 2    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 3    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 4    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 5    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 6    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 7    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg; 8-8 e2 seg │
│ 8    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 seg; 5-5 e3 snap; 6-6 e3 seg             │
│ 9    │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
│ 10   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
│ 11   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
│ 12   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
│ 13   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 14   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 snap; 5-5 e2 snap; 6-6 e2 seg            │
│ 15   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 snap; 5-5 e2 snap; 6-6 e2 seg            │
│ 16   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg; 8-8 e2 seg │
│ 17   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 18   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg; 8-8 e2 seg │
│ 19   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg; 8-8 e2 seg │
│ 20   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 snap; 5-5 e3 snap; 6-6 e3 seg            │
│ 21   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
│ 22   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 snap; 5-5 e2 snap; 6-6 e2 seg            │
│ 23   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 24   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 25   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 26   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 snap; 5-5 e2 snap; 6-6 e2 seg            │
│ 27   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 28   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 29   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e2 snap; 7-7 e2 seg             │
│ 30   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-4 e1 snap; 5-5 e3 snap; 6-6 e3 seg            │
│ 31   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
│ 32   │ 1-1 e1 snap; 2-2 e1 seg; 3-3 e1 seg; 4-5 e1 seg; 6-6 e3 snap; 7-7 e3 seg             │
└──────┴──────────────────────────────────────────────────────────────────────────────────────┘

    In some cases the final segment never reached the store, and the close
    of the source had already checkpointed its frames into the .db file. The
    retried drain found no WAL and shipped a discontinuity snapshot at
    position 4 instead of a segment, in these cases:
┌───────────────────┬──────────┬───────┬──────────┐
│       cases       │ position │ epoch │   kind   │
├───────────────────┼──────────┼───────┼──────────┤
│ 14 15 20 22 26 30 │ 4        │ 1     │ snapshot │
└───────────────────┴──────────┴───────┴──────────┘

A9. The cases that hold every invariant, and the invariants broken (none):
┌───────────────────────────────┬───────────────────┐
│ cases_holding_every_invariant │ broken_invariants │
├───────────────────────────────┼───────────────────┤
│ 32 of 32                      │ 0                 │
└───────────────────────────────┴───────────────────┘

── B. Self-test: the same relocation without the seal and the fence ────

B1. The skip-fence mutant does not seal a and does not put the fence of
    b. From step 6 on, both nodes are writable, and the store is no longer
    one lineage: epoch 2 starts with a segment, not a fence snapshot.
┌──────────────────────────────────────┬─────────┬───────┬─────────────┬───────┬───────────────┐
│                 run                  │ outcome │ state │ fence_point │ fence │ most_writable │
├──────────────────────────────────────┼─────────┼───────┼─────────────┼───────┼───────────────┤
│ finished without interruption (done) │ done    │ done  │ 3           │ 0     │ 2             │
└──────────────────────────────────────┴─────────┴───────┴─────────────┴───────┴───────────────┘
┌────────────────┬───────┬───────┬─────────────────┬────────────────────────────┬────────────────────────────┐
│     label      │ owner │ epoch │    store_end    │           node_a           │           node_b           │
├────────────────┼───────┼───────┼─────────────────┼────────────────────────────┼────────────────────────────┤
│ after step 1   │ b     │ 2     │ 3 e1 segment    │ owner at epoch 1, writable │ not owner                  │
│ after step 2   │ b     │ 2     │ 3 e1 segment    │ owner at epoch 1, writable │ not owner                  │
│ after step 3   │ b     │ 2     │ 3 e1 segment    │ owner at epoch 1, writable │ not owner                  │
│ after step 4   │ b     │ 2     │ 3 e1 segment    │ owner at epoch 1, writable │ not owner                  │
│ after step 5   │ b     │ 2     │ 3 e1 segment    │ owner at epoch 1, writable │ not owner                  │
│ after step 6   │ b     │ 2     │ 3 e1 segment    │ owner at epoch 1, writable │ owner at epoch 2, writable │
│ after step 7   │ b     │ 2     │ not one lineage │ owner at epoch 1, writable │ owner at epoch 2, writable │
│ after recovery │ b     │ 2     │ not one lineage │ owner at epoch 1, writable │ owner at epoch 2, writable │
└────────────────┴───────┴───────┴─────────────────┴────────────────────────────┴────────────────────────────┘
┌──────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                           audit_error                                            │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ slot chain is not one lineage: epoch 2 starts at position 4 with a segment, not a fence snapshot │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘

── C. Every node process ───────────────────────────────────────────────

C1. Problems and over-bounds lines the nodes reported, across all cases.
    Both counts are 0; any line would be listed in the table problems.
┌──────────┬─────────────┐
│ problems │ over_bounds │
├──────────┼─────────────┤
│ 0        │ 0           │
└──────────┴─────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  case 1: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 2: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 3: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 4: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 5: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 6: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 7: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 8: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 9: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 10: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 11: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 12: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 13: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 14: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 15: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 16: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 17: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 18: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 19: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 20: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 21: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 22: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 23: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 24: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 25: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 26: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 27: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 28: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 29: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 30: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 31: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
A  ✓  case 32: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present
B  ✓  self-test: without the fence the boundary check finds two writable nodes
C  ✓  after every command of every node process the descriptors and memory stayed within the standing bounds
C  ✓  no node reported a problem

35 of 35 claims hold

Experiment verdict: INTERRUPTED RELOCATION OK
```

#### Reading the Experiment 10.3 evidence

- **Opening settings and step list (setup and protocol):** Lists the seven numbered relocation steps and the per-case starting state: `a` claims `db-0001` (fence at `1`), commits steps `1–2` `store-durable` (positions `2`, `3`) and step `3` `local-durable` (unshipped). At every reached boundary, the client sends one `store-durable` write to the catalog owner and one `local-durable` write to the other node.
- **Section A, cases `1–7` (coordinator crash after each step, `Resume`):**
  - **Case `1`** stops immediately after `FenceEpoch` (before `AfterStep(1)` runs), so the client's boundary-1 local write never happens; `Resume` drains step `3` into slot `4` (sha256 `c75549790ef1` in A4), fences at `5`, and ends at `user_version` `4` (`4` acknowledged, `1` refused, A5).
  - **Cases `2–6`** reach boundary `1`, where `a` accepts the client's `local-durable` step `4` (A6, after step 1: catalog owner `b` at epoch `2`; `a` `owner at epoch 1, writable`; `b` `not owner`); step `2` seals `a`; step `3` drains steps `3` and `4` into segment `4-5` (sha256 `d53fc07b1293`); `Resume` rolls forward to fence slot `6` (`user_version` `5`, `5` acknowledged).
  - **Case `7`** reaches boundary `6` (A6: `a` `owner at epoch 1, sealed`; `b` `owner at epoch 2, writable`), where `b` accepts a client write at slot `7` before the coordinator crashes after step `7`; `Resume` adopts and finishes cleanly at `user_version` `6` (`6` acknowledged, `11` refused).
- **Cases `8–13` (coordinator crash after steps `1–6`, `Rollback`):**
  - **Cases `8–12`** crash before the destination's fence reaches S3: `Rollback` drains `a`, lets `a` re-claim at **epoch `3`** (fence at `5` in case `8`, `6` in cases `9–12`), marks `rolled-back`, discards `b`'s `.staged`, and preserves every acknowledged write (`user_version` `4` in case `8`, `5` in cases `9–12`).
  - **Case `13`** crashes after step `6` (`Activate`): the destination's epoch-2 fence is already at slot `6` in S3, so `Rollback` refuses to undo it, **rolls forward** (A2: state `done`, step `7`), and returns `ErrFenceInStore`.
- **Cases `14–21` (node `SIGKILL`s, `Resume` and `Rollback`):**
  - **Cases `14`, `15`, and `20`** kill source `a` before step `3` completes. When `a` restarts, `openDatabase` recovers its WAL and ships steps `3` and `4` as a **discontinuity snapshot** (A8: position `4`, epoch `1`, `snapshot`), leaving fence point `4` with sha256 `d53fc07b1293` (A4)—the exact same bytes as segment `4-5`, at one position lower.
  - **Case `18`** kills destination `b` after step `6`: `Run` still finishes without interruption (`done`) because step `7` (`RetireSource`) talks only to `a`; restarting `b` reloads its `.owner` belief file cleanly (final `user_version` `6`).
- **Cases `22–31` (store faults on final segment `log/000000000004` and fence `log/000000000006`):**
  - **Cases `22`, `26`, and `30`** (`fail-before-put` and `crash-before-put` on `a`'s final segment): `a`'s close already checkpointed steps `3–4` into `.db`, so recovery ships them as a discontinuity snapshot at position `4`, epoch `1` (sha256 `d53fc07b1293`), fencing at `5`.
  - **Cases `24` and `28`** (`fail-after-put` and `crash-after-put` on `a`'s final segment): segment `4-5` already landed in S3; recovery sees it and puts the fence at `6`.
  - **Cases `25` and `29`** (`fail-after-put` and `crash-after-put` on `b`'s fence): `b`'s fence at slot `6` already landed although `b` got an error or died; `Resume` reads it from S3 and **adopts** it with no second fence.
  - **Case `31`** (`crash-before-put` on `b`'s fence, `Rollback`): `b` died before putting slot `6`; `a` re-claims at epoch `3`, placing its fence at slot `6`.
- **A3 and case `32` rows (rollback while destination `b` stays down):** Kills `b` after step `4` and keeps `b` stopped through `Rollback`. `Rollback` drains `a`, claims epoch `3` (fence at `6`), marks `rolled-back` in the catalog, and records the discard error `node process for "discard db-0001" is not running` (A3) without failing. When `b` restarts afterward, its leftover `.staged` cannot activate and `b` reports `not owner`. All **`32 of 32`** fault cases hold every invariant (A9).
- **B1 and C1 (`skip-fence` self-test and standing bounds):** Without the seal and store fence, from step `6` onward both nodes report writable (`a` `owner at epoch 1, writable`; `b` `owner at epoch 2, writable`), and `fence.Audit` catches `epoch 2 starts at position 4 with a segment, not a fence snapshot`. Standing bounds held across every node reply (the reply count is run-dependent; **`1,726`** in one lab run) with `0` node problems.

---

### 6.3 Experiment 10.4 — `rebuild-the-fleet` (rung R7)

- **Claim (invariant 3 — every published snapshot is internally consistent, plus rung R7 fleet rebuild and invariant 5):** SeaweedFS S3 plus the control-plane catalog is a complete saved state of a multi-database fleet. After `8` writer processes (each owning an equal share and keeping at most `32` databases open) claim every database, drive recipe steps `1–4` `store-durable`, and apply a schema rollout (recipe step `5`, `CREATE INDEX events_by_account ON events(account_id, id)`) to half of each writer's share and stop—while an independent checker subprocess continuously lists S3 and rebuilds every published restore point in memory—quiescing (`TRUNCATE` checkpoint finds `0` WAL frames), deleting **every** local file under the writers' directories, and rebuilding the entire fleet from S3 alone onto empty disks produces `100%` **E1 byte-identical** databases (`500/500` by default; `2,000/2,000` at full outline scale), each at the exact `schema_version` (`user_version`) recorded in the catalog (old step `4` or new step `5`), with `0` restore-point rebuild failures during or after the run, and every existing slot read **exactly twice** during the rebuild (`once to check the lineage, once to restore`).
- **Evidence form:** `EXACT` for hashes, object/listing/fence counts, restore-point counts, and standing bounds (every stored table must match on every run); `SHAPE` for elapsed wall times and byte totals, which are in the run-dependent `info` tables and excluded (byte totals vary by ~100 bytes run to run because WAL checksums and salts are serialized as variable-width decimal integers inside JSON metadata).

<!-- include: experiments/10.4-rebuild-the-fleet/main.go -->
```go
// Experiment 10.4 implements Rebuild Ladder rung R7 (Fleet): it proves that
// the S3 store plus the control-plane catalog is a complete saved state of a
// multi-database fleet, and that every published restore point is internally
// consistent.
//
// Eight writer node processes own equal shares of the fleet (default 500
// databases; -databases 2000 for the outline's full-scale run), each keeping
// at most -capacity databases open (default 32). Every database is claimed and
// driven store-durable through recipe steps 1-4, then a schema rollout (recipe
// step 5, CREATE INDEX events_by_account) is applied to half of each writer's
// share and stopped. Meanwhile a separate checker process lists the store and
// rebuilds every published restore point in memory against the recipe step
// hashes. After quiescing (closing every writer, where sqlite3WalClose at
// src/wal.c:2507-2571 leaves 0 frames for TRUNCATE) and deleting every local
// file, 8 rebuild workers claim and restore the entire fleet from SeaweedFS S3
// alone, verifying E1 byte equality, catalog schema versions, exact double-read
// object counts, and standing descriptor and memory bounds throughout.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 10.4
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/procstat"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const stepsBeforeRollout = recipe.IndexStep - 1

var (
	databases = flag.Int("databases", 500, "databases in the fleet (the outline asks for 2000; see the notes for that run)")
	writers   = flag.Int("writers", 8, "writer node processes; each owns an equal share of the fleet")
	workers   = flag.Int("checker-workers", 4, "databases the checker inspects at once")
	pause     = flag.Duration("checker-pause", time.Second, "pause between the checker's passes while the writers run")
	role      = flag.String("role", "", "internal: run as a service node or as the checker")
	process   node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-4", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 32, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: unused here")
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 10.4" runs both.
// part labels the claims made while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		settings, writers, databases, rebuild, checkerPasses, checkerFailures, problems, infoRun, infoTimings, infoWriters *results.Table
	}
	settingSeq int
)

func openTables() {
	tables.settings = set.Table("settings", "seq", "name", "value")
	tables.writers = set.Table("writers", "writer", "databases", "rolled_out")
	tables.databases = set.Table("databases", "database", "writer", "acknowledged_version", "catalog_schema_version", "last_shipped", "quiesce_wal_frames", "rebuilt_user_version", "rebuilt_state", "byte_identical")
	tables.rebuild = set.Table("rebuild", "databases", "workers", "local_files_left", "objects_read", "listings", "fence_objects_written")
	tables.checkerPasses = set.Table("checker_passes", "pass", "databases", "restore_points", "failed")
	tables.checkerFailures = set.Table("checker_failures", "phase", "line")
	tables.problems = set.Table("problems", "seq", "kind", "line")
	tables.infoRun = set.Table("info_run", "name", "value")
	tables.infoTimings = set.Table("info_timings", "seq", "phase", "ms")
	tables.infoWriters = set.Table("info_writers", "writer", "reply", "summary")
}

func setting(name string, value any) {
	settingSeq++
	tables.settings.Row(settingSeq, name, value)
}

var timingSeq int

func timing(phase string, elapsed time.Duration) {
	timingSeq++
	tables.infoTimings.Row(timingSeq, phase, elapsed.Milliseconds())
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type fleet struct {
	env           *lab.Env
	ids           []string
	shares        [][]string
	writers       []*node.Process
	mu            sync.Mutex
	acks          map[string]int
	rebuildCounts *lab.CountingStore
}

func (f *fleet) send(p *node.Process, command string) node.Reply {
	reply, err := p.Send(command)
	if err != nil {
		fatal("send "+command, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.env.Tracker.Note(p.Name, reply)
	if strings.HasPrefix(reply.Line, "ok write ") {
		id, ack, err := node.ParseAck(reply.Line)
		if err != nil {
			fatal("parse acknowledgement", err)
		}
		f.acks[id] = max(f.acks[id], ack.UserVersion)
	} else if !strings.HasPrefix(reply.Line, "ok ") {
		f.env.Tracker.Problems = append(f.env.Tracker.Problems, p.Name+": "+reply.Line)
	}
	return reply
}

func (f *fleet) eachWriter(work func(w int, p *node.Process, share []string)) {
	var wg sync.WaitGroup
	for w, p := range f.writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(w, p, f.shares[w])
		}()
	}
	wg.Wait()
}

func (f *fleet) writeStream() {
	f.eachWriter(func(_ int, p *node.Process, share []string) {
		for start := 0; start < len(share); start += process.Capacity {
			batch := share[start:min(start+process.Capacity, len(share))]
			for _, id := range batch {
				f.send(p, "claim "+id)
			}
			for range stepsBeforeRollout {
				for _, id := range batch {
					f.send(p, fmt.Sprintf("write %s %s", id, node.StoreDurable))
				}
			}
		}
	})
}

func (f *fleet) rollout() {
	f.eachWriter(func(_ int, p *node.Process, share []string) {
		for _, id := range share[:len(share)/2] {
			f.send(p, fmt.Sprintf("write %s %s", id, node.StoreDurable))
		}
	})
}

func (f *fleet) runChecker(checker *node.Process, stop <-chan struct{}) (passes, points int, failed []string) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(*pause):
		}
		reply, err := checker.Send("pass")
		if err != nil {
			fatal("checker", err)
		}
		pass, err := lab.ParsePass(reply)
		if err != nil {
			fatal("checker pass", err)
		}
		f.mu.Lock()
		f.env.Tracker.Note("checker", node.Reply{Line: reply.Line, Bounds: reply.Bounds})
		f.mu.Unlock()
		passes++
		points += pass.RestorePoints
		failed = append(failed, pass.FailedLines...)
	}
}

type recorded struct {
	sha256        string
	schemaVersion int
	lastShipped   int64
	frames        int
}

func (f *fleet) quiesce(cat *catalog.Catalog) map[string]recorded {
	for _, p := range f.writers {
		reply, err := f.env.Stop(p)
		if err != nil {
			fatal("close "+p.Name, err)
		}
		tables.infoWriters.Row(p.Name, reply.Line, reply.Summary)
	}
	records, err := cat.All(f.env.Ctx)
	if err != nil {
		fatal("catalog", err)
	}
	out := map[string]recorded{}
	frames := 0
	for _, record := range records {
		quiesced, err := lab.TruncateAndHash(f.env.Ctx, f.env.DatabasePath(record.Owner, record.ID))
		if err != nil {
			fatal("quiesce "+record.ID, err)
		}
		frames += quiesced.Checkpoint.LogFrames
		out[record.ID] = recorded{sha256: quiesced.SHA256, schemaVersion: record.SchemaVersion, lastShipped: record.LastShipped, frames: quiesced.Checkpoint.LogFrames}
	}
	check(len(out) == len(f.ids) && frames == 0, "every database was at rest when quiesced: the writers' close left no WAL frame for the TRUNCATE checkpoint")
	return out
}

func (f *fleet) rebuild(cat *catalog.Catalog, before map[string]recorded) {
	for w := range f.writers {
		if err := os.RemoveAll(f.env.NodeDir(fmt.Sprintf("w%d", w+1))); err != nil {
			fatal("delete local files", err)
		}
	}
	left, err := filepath.Glob(filepath.Join(f.env.Work, "w*", "*"))
	if err != nil {
		fatal("glob", err)
	}
	check(len(left) == 0, "the new node starts with empty disks")
	counting := &lab.CountingStore{Store: f.env.Store}
	f.rebuildCounts = counting
	workload := recipe.Default()
	assertion, err := bounds.Start(bounds.Bounds{OpenDatabases: *writers, ConnectionsPerDatabase: node.ConnectionsPerDatabase, CacheBytes: bounds.CacheBytes(node.PlatformSettings().CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors})
	if err != nil {
		fatal("bounds", err)
	}
	rebuilders := make([]*node.Node, *writers)
	for w := range rebuilders {
		rebuilders[w], err = node.Start(node.Config{
			Name: fmt.Sprintf("fresh-%d", w+1), Dir: filepath.Join(f.env.Work, "fresh", fmt.Sprintf("%d", w+1)), Catalog: cat, Store: counting, Capacity: 1,
			File:   workload.SQLiteOptions().File,
			Create: func(ctx context.Context, path string) error { return fmt.Errorf("a rebuild never creates a database") },
		})
		if err != nil {
			fatal("start the new node", err)
		}
	}
	start := time.Now()
	paths := make(map[string]string, len(f.ids))
	var mu sync.Mutex
	var overBounds []string
	var wg sync.WaitGroup
	for w, share := range f.shares {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, id := range share {
				if _, err := rebuilders[w].Claim(f.env.Ctx, id); err != nil {
					fatal("rebuild "+id, err)
				}
				mu.Lock()
				paths[id] = f.env.DatabasePath(filepath.Join("fresh", fmt.Sprintf("%d", w+1)), id)
				if i%50 == 0 {
					if _, err := assertion.Check(); err != nil {
						overBounds = append(overBounds, err.Error())
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, rebuilder := range rebuilders {
		if err := rebuilder.Close(); err != nil {
			fatal("close the new node", err)
		}
	}
	if _, err := assertion.Check(); err != nil {
		overBounds = append(overBounds, err.Error())
	}
	gets, bytes, puts, lists := counting.Counts()
	timing("rebuild", elapsed)
	tables.infoRun.Row("rebuild standing assertion", assertion.Summary())
	tables.infoRun.Row("rebuild bytes read", bytes)
	tables.infoRun.Row("rebuild bytes read per database", bytes/int64(len(f.ids)))
	tables.rebuild.Row(len(f.ids), *writers, len(left), gets, lists, puts)
	equal, steps, schemaAgrees := 0, map[int]int{}, 0
	for i, id := range f.ids {
		image, err := os.ReadFile(paths[id])
		if err != nil {
			fatal("read the rebuilt "+id, err)
		}
		state, err := f.env.MatchImage(image)
		if err != nil {
			fatal("match", err)
		}
		sha, err := f.env.AtRestSHA256(filepath.Join("fresh", filepath.Base(filepath.Dir(paths[id]))), id)
		if err != nil {
			fatal("at rest", err)
		}
		if sha == before[id].sha256 {
			equal++
		}
		if state.RecipeState && state.UserVersion == before[id].schemaVersion && state.UserVersion == f.acks[id] {
			schemaAgrees++
		}
		steps[state.UserVersion]++
		tables.databases.Row(id, fmt.Sprintf("w%d", i%*writers+1), f.acks[id], before[id].schemaVersion, before[id].lastShipped, before[id].frames, state.UserVersion, state.Match, sha == before[id].sha256)
	}
	check(equal == len(f.ids), fmt.Sprintf("%d of %d rebuilt databases are byte-identical to the quiesced originals", equal, len(f.ids)))
	check(schemaAgrees == len(f.ids), "every rebuilt database is a recipe state whose schema version (user_version) is the one the catalog recorded and the last one acknowledged")
	check(steps[recipe.IndexStep]+steps[stepsBeforeRollout] == len(f.ids) && steps[recipe.IndexStep] > 0 && steps[stepsBeforeRollout] > 0, fmt.Sprintf("the rollout stopped halfway: databases are on the old schema (step %d) or the new one (step %d with %s), nothing else", stepsBeforeRollout, recipe.IndexStep, strings.TrimPrefix(recipe.IndexSQL, "CREATE ")))
	for _, line := range overBounds {
		problem("rebuild over bounds", line)
	}
	check(len(overBounds) == 0, "the rebuilding process stayed within the standing bounds")
}

var problemSeq int

func problem(kind, line string) {
	problemSeq++
	tables.problems.Row(problemSeq, kind, line)
}

func main() {
	flag.Parse()
	ctx := context.Background()
	switch *role {
	case "node":
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	case "checker":
		store, err := lab.Connect(ctx, process)
		if err == nil {
			err = lab.ServeChecker(ctx, store, *workers, os.Stdin, os.Stdout)
		}
		if err != nil {
			fatal("checker", err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *databases < 2**writers || *writers < 1 || process.Capacity < 1 {
		fatal("flags", fmt.Errorf("need -writers >= 1, -capacity >= 1 and at least two databases per writer"))
	}
	// Guard against hitting the process descriptor soft limit before launching
	// writers; each open database needs up to 3*3 descriptors plus 32 reserve.
	soft, _, err := procstat.FileLimit()
	if err != nil {
		fatal("file limit", err)
	}
	perNode := bounds.DescriptorsPerConnection*node.ConnectionsPerDatabase*process.Capacity + node.ReserveDescriptors
	if uint64(perNode) > soft {
		fatal("descriptor limit", fmt.Errorf("-capacity %d needs up to %d descriptors per writer, the soft limit is %d", process.Capacity, perNode, soft))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.4", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	start := time.Now()
	f := &fleet{env: env, ids: lab.Ids("db", *databases), acks: map[string]int{}}
	if err := env.Reset(f.ids); err != nil {
		fatal("reset", err)
	}
	// Open the tables after env.Reset, which empties env.Work.
	set = results.MustOpen(env.Work)
	openTables()
	tables.infoRun.Row("descriptor soft limit", soft)
	tables.infoRun.Row("descriptor bound of each writer", perNode)
	f.shares = make([][]string, *writers)
	for i, id := range f.ids {
		f.shares[i%*writers] = append(f.shares[i%*writers], id)
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	tables.infoRun.Row("store endpoint", store.Endpoint)
	tables.infoRun.Row("work directory", env.ShownWork())
	setting("databases", *databases)
	setting("writer_processes", *writers)
	setting("open_databases_per_writer", process.Capacity)
	setting("checker_processes", 1)
	setting("checker_workers", *workers)
	setting("checker_pause", *pause)
	setting("bucket", store.Bucket)
	setting("prefix", store.Prefix)
	for w, share := range f.shares {
		tables.writers.Row(fmt.Sprintf("w%d", w+1), len(share), len(share)/2)
	}
	for w := range *writers {
		p, err := env.Start(env.Config(fmt.Sprintf("w%d", w+1), ""))
		if err != nil {
			fatal("start writer", err)
		}
		f.writers = append(f.writers, p)
	}
	checkerArgs := []string{"-role", "checker", "-bucket", process.Bucket, "-prefix", process.Prefix, "-checker-workers", fmt.Sprint(*workers)}
	checker, err := node.StartProcess(ctx, "checker", checkerArgs...)
	if err != nil {
		fatal("start checker", err)
	}
	stop := make(chan struct{})
	type checked struct {
		passes, points int
		failed         []string
	}
	done := make(chan checked)
	go func() {
		passes, points, failed := f.runChecker(checker, stop)
		done <- checked{passes, points, failed}
	}()
	part = "A"
	setting("steps_before_rollout", stepsBeforeRollout)
	setting("rollout_step", recipe.IndexStep)
	setting("rollout_sql", recipe.IndexSQL)
	phase := time.Now()
	f.writeStream()
	timing("write stream", time.Since(phase))
	phase = time.Now()
	f.rollout()
	timing("rollout", time.Since(phase))
	close(stop)
	phase = time.Now()
	concurrent := <-done
	timing("waiting for the last concurrent checker pass", time.Since(phase))
	tables.infoRun.Row("concurrent checker passes", concurrent.passes)
	tables.infoRun.Row("restore points the concurrent checker rebuilt", concurrent.points)
	for _, line := range concurrent.failed {
		tables.checkerFailures.Row("concurrent", line)
	}
	check(concurrent.passes > 0 && len(concurrent.failed) == 0, "every restore point the checker found while the writers ran rebuilt to a recipe state")
	part = "B"
	phase = time.Now()
	before := f.quiesce(env.Catalog)
	timing("quiesce", time.Since(phase))
	part = "C"
	f.rebuild(env.Catalog, before)
	part = "D"
	phase = time.Now()
	reply, err := checker.Send("pass")
	if err != nil {
		fatal("checker", err)
	}
	timing("final checker pass", time.Since(phase))
	env.Tracker.Note("checker", node.Reply{Line: reply.Line, Bounds: reply.Bounds})
	final, err := lab.ParsePass(reply)
	if err != nil {
		fatal("checker pass", err)
	}
	tables.checkerPasses.Row("final", final.Databases, final.RestorePoints, final.Failed)
	tables.infoRun.Row("final checker pass bytes read (the whole store)", final.BytesRead)
	for _, line := range final.FailedLines {
		tables.checkerFailures.Row("final", line)
	}
	check(final.Databases == len(f.ids) && final.Failed == 0, "after the rebuild every published restore point of every database rebuilds to a recipe state")
	// node.Claim reads every existing slot twice: once in adapter.Refresh to
	// check the lineage and once in walship.LoadCatalog to plan and restore.
	gets, _, _, _ := f.rebuildCounts.Counts()
	check(gets == 2*int64(final.RestorePoints-len(f.ids)), fmt.Sprintf("the rebuild read each of the %d slots it found exactly twice (once to check the lineage, once to restore)", final.RestorePoints-len(f.ids)))
	closing, err := env.Stop(checker)
	if err != nil {
		fatal("close checker", err)
	}
	tables.infoRun.Row("checker standing assertion", closing.Summary)
	part = "E"
	for _, line := range env.Tracker.Problems {
		problem("problem", line)
	}
	for _, line := range env.Tracker.OverBounds {
		problem("over bounds", line)
	}
	check(len(env.Tracker.Problems) == 0, "no node reported a problem and every command was acknowledged")
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every writer and checker command the descriptors and memory stayed within the standing bounds")
	tables.infoRun.Row("replies that carried a bounds check", env.Tracker.Replies)
	timing("total", time.Since(start))
	set.Finish("REBUILD THE FLEET")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/10.4-rebuild-the-fleet/report.sql -->
```sql
-- Walkthrough of experiment 10.4. Run it with:
--   go run ./cmd/course run 10.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings           one setting of the run (name, value)
--   writers            one writer process: how many databases it owns and how many it rolled out
--   databases          one database: what was acknowledged, quiesced and rebuilt
--   rebuild            the rebuild from the store: objects read and written
--   checker_passes     the final pass of the checker over every restore point in the store
--   checker_failures   one restore point a checker pass could not rebuild (none on a good run)
--   problems           one problem or over-bounds line (none on a good run)
--   checks             every claim the experiment asserted (the runner lists it last)
-- info_run, info_timings and info_writers hold timings, byte counts, the passes
-- of the concurrent checker and the standing-bounds summaries; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.4  Can the whole fleet be rebuilt from the store and the catalog?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Writer processes own equal shares of the fleet and keep a bounded number'
.print 'of databases open. A separate checker process lists the store and rebuilds'
.print 'every published restore point in memory while the writers run. Then every'
.print 'writer closes, every local file is deleted, and fresh nodes rebuild the'
.print 'whole fleet from the S3 store and the catalog alone.'

SELECT name, value FROM settings ORDER BY seq;

.print
.print '── A. Write stream and a schema rollout stopped halfway ────────────────'
.print
.print 'A1. Every database is claimed and given recipe steps 1-4, each write'
.print '    acknowledged store-durable. Then recipe step 5 (the CREATE INDEX in'
.print '    rollout_sql) is applied to the first half of the share of each writer,'
.print '    and the rollout stops.'

SELECT writer, databases, rolled_out FROM writers ORDER BY writer;

SELECT sum(databases) AS databases, sum(rolled_out) AS rolled_out,
       min(databases) || '-' || max(databases) AS per_writer
FROM writers;

.print
.print 'A2. Every restore point the checker found while the writers ran rebuilt to'
.print '    a recipe state: no checker failure. How many passes it made depends on'
.print '    timing; info_run has the count.'

SELECT count(*) FILTER (phase = 'concurrent') AS concurrent_failures FROM checker_failures;

.print
.print '── B. Quiesce ──────────────────────────────────────────────────────────'
.print
.print 'B1. Every writer closes. Each database is recorded (sha256, catalog row)'
.print '    after a TRUNCATE checkpoint. The close of the writer already left no'
.print '    WAL frame to checkpoint: the last close deletes -wal and -shm.'

SELECT count(*) AS databases_recorded, sum(quiesce_wal_frames) AS wal_frames_found
FROM databases;

.print
.print '── C. Rebuild from the store alone ─────────────────────────────────────'
.print
.print 'C1. Every local file is deleted. One fresh node per worker claims its share'
.print '    of the fleet and restores it from the store. A claim reads every slot'
.print '    twice (once to check the lineage, once to restore) and writes one fence.'

SELECT databases, workers, local_files_left, listings, objects_read, fence_objects_written
FROM rebuild;

.print
.print 'C2. The rebuilt databases: byte-identical (E1) to the sha256 recorded at'
.print '    quiesce, and at the recipe step the catalog recorded and the writer last'
.print '    acknowledged. Half are on the old schema (step 4), half on the new one'
.print '    (step 5); nothing else. Each matches its recipe step file except the'
.print '    change counter (bytes 24-27 and 92-95).'

SELECT rebuilt_user_version AS recipe_step,
       CASE WHEN rebuilt_state LIKE '%exact%' THEN 'exact' ELSE 'counter differs' END AS vs_recipe_file,
       count(*) AS databases,
       count(*) FILTER (byte_identical)                                    AS byte_identical,
       count(*) FILTER (rebuilt_user_version = catalog_schema_version
                        AND rebuilt_user_version = acknowledged_version)  AS schema_agrees
FROM databases GROUP BY ALL ORDER BY recipe_step, vs_recipe_file;

.print
.print 'C3. The stream positions each database reached in the store (last_shipped in'
.print '    the catalog), by schema:'

SELECT rebuilt_user_version AS recipe_step, last_shipped, count(*) AS databases
FROM databases GROUP BY ALL ORDER BY recipe_step, last_shipped;

.print
.print 'C4. A few databases, for a closer look:'

SELECT database, writer, acknowledged_version AS acked, catalog_schema_version AS catalog,
       last_shipped, rebuilt_user_version AS rebuilt,
       CASE WHEN rebuilt_state LIKE '%exact%' THEN 'exact' ELSE 'counter differs' END AS vs_recipe_file,
       byte_identical
FROM databases WHERE database IN ('db-0001', 'db-0002', 'db-0500') ORDER BY database;

.print
.print '── D. The final checker pass ───────────────────────────────────────────'
.print
.print 'D1. After the rebuild, the checker rebuilds every published restore point'
.print '    of every database. The slots the rebuild found are those restore points'
.print '    minus the fences it wrote, and it read each of them exactly twice.'

SELECT c.databases, c.restore_points, c.failed,
       c.restore_points - r.fence_objects_written      AS slots_found,
       r.objects_read,
       r.objects_read = 2 * (c.restore_points - r.fence_objects_written) AS each_read_twice
FROM checker_passes c, rebuild r WHERE c.pass = 'final' ORDER BY c.pass;

.print
.print '── E. Every process ────────────────────────────────────────────────────'
.print
.print 'E1. Problems and over-bounds lines of the writers, the checker and the'
.print '    rebuilding process. All counts are 0; any line would be listed in the'
.print '    table problems.'

SELECT count(*) FILTER (kind = 'problem')             AS problems,
       count(*) FILTER (kind = 'over bounds')         AS over_bounds,
       count(*) FILTER (kind = 'rebuild over bounds') AS rebuild_over_bounds
FROM problems;
```

Run from the repository root, with SeaweedFS running (this starts the `lab` container, runs the experiment in it, and prints the walkthrough; `500` databases by default, about `9` seconds in the lab container):

```text
go run ./cmd/course run 10.4
```

Add `-i` (`go run ./cmd/course run -i 10.4`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries. For the outline's `2,000`-database fleet, run the experiment directly in the container: `docker compose run --rm lab go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet -databases 2000 -prefix exp-10-4-2000` (about `40` seconds in the lab container).

`course run` also prints the run-dependent values, which are not part of the stored evidence: where the store and the work directory were, the descriptor limit and bounds, byte counts, the passes of the concurrent checker, the standing-bounds summary of each writer when it closed, and how long each phase took.

Captured evidence (`500` databases, default): the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/10.4-rebuild-the-fleet/`](evidence/10.4-rebuild-the-fleet/), and `go run ./cmd/course check evidence ch10` compares a fresh run with them byte for byte.

<!-- include: evidence/10.4-rebuild-the-fleet.txt -->
```text

════════════════════════════════════════════════════════════════════════
  10.4  Can the whole fleet be rebuilt from the store and the catalog?
════════════════════════════════════════════════════════════════════════

Writer processes own equal shares of the fleet and keep a bounded number
of databases open. A separate checker process lists the store and rebuilds
every published restore point in memory while the writers run. Then every
writer closes, every local file is deleted, and fresh nodes rebuild the
whole fleet from the S3 store and the catalog alone.
┌───────────────────────────┬──────────────────────────────────────────────────────────┐
│           name            │                          value                           │
├───────────────────────────┼──────────────────────────────────────────────────────────┤
│ databases                 │ 500                                                      │
│ writer_processes          │ 8                                                        │
│ open_databases_per_writer │ 32                                                       │
│ checker_processes         │ 1                                                        │
│ checker_workers           │ 4                                                        │
│ checker_pause             │ 1s                                                       │
│ bucket                    │ course-ch10b                                             │
│ prefix                    │ exp-10-4/                                                │
│ steps_before_rollout      │ 4                                                        │
│ rollout_step              │ 5                                                        │
│ rollout_sql               │ CREATE INDEX events_by_account ON events(account_id, id) │
└───────────────────────────┴──────────────────────────────────────────────────────────┘

── A. Write stream and a schema rollout stopped halfway ────────────────

A1. Every database is claimed and given recipe steps 1-4, each write
    acknowledged store-durable. Then recipe step 5 (the CREATE INDEX in
    rollout_sql) is applied to the first half of the share of each writer,
    and the rollout stops.
┌────────┬───────────┬────────────┐
│ writer │ databases │ rolled_out │
├────────┼───────────┼────────────┤
│ w1     │ 63        │ 31         │
│ w2     │ 63        │ 31         │
│ w3     │ 63        │ 31         │
│ w4     │ 63        │ 31         │
│ w5     │ 62        │ 31         │
│ w6     │ 62        │ 31         │
│ w7     │ 62        │ 31         │
│ w8     │ 62        │ 31         │
└────────┴───────────┴────────────┘
┌───────────┬────────────┬────────────┐
│ databases │ rolled_out │ per_writer │
├───────────┼────────────┼────────────┤
│ 500       │ 248        │ 62-63      │
└───────────┴────────────┴────────────┘

A2. Every restore point the checker found while the writers ran rebuilt to
    a recipe state: no checker failure. How many passes it made depends on
    timing; info_run has the count.
┌─────────────────────┐
│ concurrent_failures │
├─────────────────────┤
│ 0                   │
└─────────────────────┘

── B. Quiesce ──────────────────────────────────────────────────────────

B1. Every writer closes. Each database is recorded (sha256, catalog row)
    after a TRUNCATE checkpoint. The close of the writer already left no
    WAL frame to checkpoint: the last close deletes -wal and -shm.
┌────────────────────┬──────────────────┐
│ databases_recorded │ wal_frames_found │
├────────────────────┼──────────────────┤
│ 500                │ 0                │
└────────────────────┴──────────────────┘

── C. Rebuild from the store alone ─────────────────────────────────────

C1. Every local file is deleted. One fresh node per worker claims its share
    of the fleet and restores it from the store. A claim reads every slot
    twice (once to check the lineage, once to restore) and writes one fence.
┌───────────┬─────────┬──────────────────┬──────────┬──────────────┬───────────────────────┐
│ databases │ workers │ local_files_left │ listings │ objects_read │ fence_objects_written │
├───────────┼─────────┼──────────────────┼──────────┼──────────────┼───────────────────────┤
│ 500       │ 8       │ 0                │ 500      │ 5496         │ 500                   │
└───────────┴─────────┴──────────────────┴──────────┴──────────────┴───────────────────────┘

C2. The rebuilt databases: byte-identical (E1) to the sha256 recorded at
    quiesce, and at the recipe step the catalog recorded and the writer last
    acknowledged. Half are on the old schema (step 4), half on the new one
    (step 5); nothing else. Each matches its recipe step file except the
    change counter (bytes 24-27 and 92-95).
┌─────────────┬─────────────────┬───────────┬────────────────┬───────────────┐
│ recipe_step │ vs_recipe_file  │ databases │ byte_identical │ schema_agrees │
├─────────────┼─────────────────┼───────────┼────────────────┼───────────────┤
│ 4           │ counter differs │ 252       │ 252            │ 252           │
│ 5           │ counter differs │ 248       │ 248            │ 248           │
└─────────────┴─────────────────┴───────────┴────────────────┴───────────────┘

C3. The stream positions each database reached in the store (last_shipped in
    the catalog), by schema:
┌─────────────┬──────────────┬───────────┐
│ recipe_step │ last_shipped │ databases │
├─────────────┼──────────────┼───────────┤
│ 4           │ 5            │ 252       │
│ 5           │ 6            │ 248       │
└─────────────┴──────────────┴───────────┘

C4. A few databases, for a closer look:
┌──────────┬────────┬───────┬─────────┬──────────────┬─────────┬─────────────────┬────────────────┐
│ database │ writer │ acked │ catalog │ last_shipped │ rebuilt │ vs_recipe_file  │ byte_identical │
├──────────┼────────┼───────┼─────────┼──────────────┼─────────┼─────────────────┼────────────────┤
│ db-0001  │ w1     │ 5     │ 5       │ 6            │ 5       │ counter differs │ true           │
│ db-0002  │ w2     │ 5     │ 5       │ 6            │ 5       │ counter differs │ true           │
│ db-0500  │ w4     │ 4     │ 4       │ 5            │ 4       │ counter differs │ true           │
└──────────┴────────┴───────┴─────────┴──────────────┴─────────┴─────────────────┴────────────────┘

── D. The final checker pass ───────────────────────────────────────────

D1. After the rebuild, the checker rebuilds every published restore point
    of every database. The slots the rebuild found are those restore points
    minus the fences it wrote, and it read each of them exactly twice.
┌───────────┬────────────────┬────────┬─────────────┬──────────────┬─────────────────┐
│ databases │ restore_points │ failed │ slots_found │ objects_read │ each_read_twice │
├───────────┼────────────────┼────────┼─────────────┼──────────────┼─────────────────┤
│ 500       │ 3248           │ 0      │ 2748        │ 5496         │ true            │
└───────────┴────────────────┴────────┴─────────────┴──────────────┴─────────────────┘

── E. Every process ────────────────────────────────────────────────────

E1. Problems and over-bounds lines of the writers, the checker and the
    rebuilding process. All counts are 0; any line would be listed in the
    table problems.
┌──────────┬─────────────┬─────────────────────┐
│ problems │ over_bounds │ rebuild_over_bounds │
├──────────┼─────────────┼─────────────────────┤
│ 0        │ 0           │ 0                   │
└──────────┴─────────────┴─────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  every restore point the checker found while the writers ran rebuilt to a recipe state
B  ✓  every database was at rest when quiesced: the writers' close left no WAL frame for the TRUNCATE checkpoint
C  ✓  the new node starts with empty disks
C  ✓  500 of 500 rebuilt databases are byte-identical to the quiesced originals
C  ✓  every rebuilt database is a recipe state whose schema version (user_version) is the one the catalog recorded and the last one acknowledged
C  ✓  the rollout stopped halfway: databases are on the old schema (step 4) or the new one (step 5 with INDEX events_by_account ON events(account_id, id)), nothing else
C  ✓  the rebuilding process stayed within the standing bounds
D  ✓  after the rebuild every published restore point of every database rebuilds to a recipe state
D  ✓  the rebuild read each of the 2748 slots it found exactly twice (once to check the lineage, once to restore)
E  ✓  no node reported a problem and every command was acknowledged
E  ✓  after every command of every writer and checker command the descriptors and memory stayed within the standing bounds

11 of 11 claims hold

Experiment verdict: REBUILD THE FLEET OK
```

Captured evidence (`2,000` databases, full outline size): the walkthrough of a lab-container run with `-databases 2000`, rendered in the same format. No tool checks this file.

<!-- include: evidence/10.4-rebuild-the-fleet-2000.txt -->
```text

════════════════════════════════════════════════════════════════════════
  10.4  Can the whole fleet be rebuilt from the store and the catalog?
════════════════════════════════════════════════════════════════════════

Writer processes own equal shares of the fleet and keep a bounded number
of databases open. A separate checker process lists the store and rebuilds
every published restore point in memory while the writers run. Then every
writer closes, every local file is deleted, and fresh nodes rebuild the
whole fleet from the S3 store and the catalog alone.
┌───────────────────────────┬──────────────────────────────────────────────────────────┐
│           name            │                          value                           │
├───────────────────────────┼──────────────────────────────────────────────────────────┤
│ databases                 │ 2000                                                     │
│ writer_processes          │ 8                                                        │
│ open_databases_per_writer │ 32                                                       │
│ checker_processes         │ 1                                                        │
│ checker_workers           │ 4                                                        │
│ checker_pause             │ 1s                                                       │
│ bucket                    │ course-ch10b                                             │
│ prefix                    │ exp-10-4-2000/                                           │
│ steps_before_rollout      │ 4                                                        │
│ rollout_step              │ 5                                                        │
│ rollout_sql               │ CREATE INDEX events_by_account ON events(account_id, id) │
└───────────────────────────┴──────────────────────────────────────────────────────────┘

── A. Write stream and a schema rollout stopped halfway ────────────────

A1. Every database is claimed and given recipe steps 1-4, each write
    acknowledged store-durable. Then recipe step 5 (the CREATE INDEX in
    rollout_sql) is applied to the first half of the share of each writer,
    and the rollout stops.
┌────────┬───────────┬────────────┐
│ writer │ databases │ rolled_out │
├────────┼───────────┼────────────┤
│ w1     │ 250       │ 125        │
│ w2     │ 250       │ 125        │
│ w3     │ 250       │ 125        │
│ w4     │ 250       │ 125        │
│ w5     │ 250       │ 125        │
│ w6     │ 250       │ 125        │
│ w7     │ 250       │ 125        │
│ w8     │ 250       │ 125        │
└────────┴───────────┴────────────┘
┌───────────┬────────────┬────────────┐
│ databases │ rolled_out │ per_writer │
├───────────┼────────────┼────────────┤
│ 2000      │ 1000       │ 250-250    │
└───────────┴────────────┴────────────┘

A2. Every restore point the checker found while the writers ran rebuilt to
    a recipe state: no checker failure. How many passes it made depends on
    timing; info_run has the count.
┌─────────────────────┐
│ concurrent_failures │
├─────────────────────┤
│ 0                   │
└─────────────────────┘

── B. Quiesce ──────────────────────────────────────────────────────────

B1. Every writer closes. Each database is recorded (sha256, catalog row)
    after a TRUNCATE checkpoint. The close of the writer already left no
    WAL frame to checkpoint: the last close deletes -wal and -shm.
┌────────────────────┬──────────────────┐
│ databases_recorded │ wal_frames_found │
├────────────────────┼──────────────────┤
│ 2000               │ 0                │
└────────────────────┴──────────────────┘

── C. Rebuild from the store alone ─────────────────────────────────────

C1. Every local file is deleted. One fresh node per worker claims its share
    of the fleet and restores it from the store. A claim reads every slot
    twice (once to check the lineage, once to restore) and writes one fence.
┌───────────┬─────────┬──────────────────┬──────────┬──────────────┬───────────────────────┐
│ databases │ workers │ local_files_left │ listings │ objects_read │ fence_objects_written │
├───────────┼─────────┼──────────────────┼──────────┼──────────────┼───────────────────────┤
│ 2000      │ 8       │ 0                │ 2000     │ 22000        │ 2000                  │
└───────────┴─────────┴──────────────────┴──────────┴──────────────┴───────────────────────┘

C2. The rebuilt databases: byte-identical (E1) to the sha256 recorded at
    quiesce, and at the recipe step the catalog recorded and the writer last
    acknowledged. Half are on the old schema (step 4), half on the new one
    (step 5); nothing else. Each matches its recipe step file except the
    change counter (bytes 24-27 and 92-95).
┌─────────────┬─────────────────┬───────────┬────────────────┬───────────────┐
│ recipe_step │ vs_recipe_file  │ databases │ byte_identical │ schema_agrees │
├─────────────┼─────────────────┼───────────┼────────────────┼───────────────┤
│ 4           │ counter differs │ 1000      │ 1000           │ 1000          │
│ 5           │ counter differs │ 1000      │ 1000           │ 1000          │
└─────────────┴─────────────────┴───────────┴────────────────┴───────────────┘

C3. The stream positions each database reached in the store (last_shipped in
    the catalog), by schema:
┌─────────────┬──────────────┬───────────┐
│ recipe_step │ last_shipped │ databases │
├─────────────┼──────────────┼───────────┤
│ 4           │ 5            │ 1000      │
│ 5           │ 6            │ 1000      │
└─────────────┴──────────────┴───────────┘

C4. A few databases, for a closer look:
┌──────────┬────────┬───────┬─────────┬──────────────┬─────────┬─────────────────┬────────────────┐
│ database │ writer │ acked │ catalog │ last_shipped │ rebuilt │ vs_recipe_file  │ byte_identical │
├──────────┼────────┼───────┼─────────┼──────────────┼─────────┼─────────────────┼────────────────┤
│ db-0001  │ w1     │ 5     │ 5       │ 6            │ 5       │ counter differs │ true           │
│ db-0002  │ w2     │ 5     │ 5       │ 6            │ 5       │ counter differs │ true           │
│ db-0500  │ w4     │ 5     │ 5       │ 6            │ 5       │ counter differs │ true           │
└──────────┴────────┴───────┴─────────┴──────────────┴─────────┴─────────────────┴────────────────┘

── D. The final checker pass ───────────────────────────────────────────

D1. After the rebuild, the checker rebuilds every published restore point
    of every database. The slots the rebuild found are those restore points
    minus the fences it wrote, and it read each of them exactly twice.
┌───────────┬────────────────┬────────┬─────────────┬──────────────┬─────────────────┐
│ databases │ restore_points │ failed │ slots_found │ objects_read │ each_read_twice │
├───────────┼────────────────┼────────┼─────────────┼──────────────┼─────────────────┤
│ 2000      │ 13000          │ 0      │ 11000       │ 22000        │ true            │
└───────────┴────────────────┴────────┴─────────────┴──────────────┴─────────────────┘

── E. Every process ────────────────────────────────────────────────────

E1. Problems and over-bounds lines of the writers, the checker and the
    rebuilding process. All counts are 0; any line would be listed in the
    table problems.
┌──────────┬─────────────┬─────────────────────┐
│ problems │ over_bounds │ rebuild_over_bounds │
├──────────┼─────────────┼─────────────────────┤
│ 0        │ 0           │ 0                   │
└──────────┴─────────────┴─────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  every restore point the checker found while the writers ran rebuilt to a recipe state
B  ✓  every database was at rest when quiesced: the writers' close left no WAL frame for the TRUNCATE checkpoint
C  ✓  the new node starts with empty disks
C  ✓  2000 of 2000 rebuilt databases are byte-identical to the quiesced originals
C  ✓  every rebuilt database is a recipe state whose schema version (user_version) is the one the catalog recorded and the last one acknowledged
C  ✓  the rollout stopped halfway: databases are on the old schema (step 4) or the new one (step 5 with INDEX events_by_account ON events(account_id, id)), nothing else
C  ✓  the rebuilding process stayed within the standing bounds
D  ✓  after the rebuild every published restore point of every database rebuilds to a recipe state
D  ✓  the rebuild read each of the 11000 slots it found exactly twice (once to check the lineage, once to restore)
E  ✓  no node reported a problem and every command was acknowledged
E  ✓  after every command of every writer and checker command the descriptors and memory stayed within the standing bounds

11 of 11 claims hold

Experiment verdict: REBUILD THE FLEET OK
```

#### Reading the Experiment 10.4 evidence

- **500-database default run (`evidence/10.4-rebuild-the-fleet.txt`):**
  - **Opening settings and A1:** Pin check passes (the experiment's own output, shown when it is run directly); the descriptor soft limit (`1,048,576` in one lab run; run-dependent, printed by `course run`) easily exceeds each writer's bound of `320` (`3 * 3 * 32 + 32`). Eight writers (`w1`–`w8`) own `62–63` databases each (`4 * 63 + 4 * 62 = 500`) with at most `32` open at once.
  - **A1–A2 (write stream, halfway schema rollout, concurrent checker):** Write stream takes about `3.8s` in one lab run (timings are run-dependent); schema rollout applies recipe step `5` (`CREATE INDEX events_by_account ON events(account_id, id)`) to **`248 of 500`** databases (`4 * floor(63/2) + 4 * floor(62/2) = 4*31 + 4*31 = 248`) in about `0.7s`. Meanwhile the checker subprocess rebuilt restore points in memory with `0` failures; how many passes it made depends on timing (`2` passes and `3,354` restore points in one lab run).
  - **B1 and C1 (quiesce and wipe):** All `8` writers close cleanly within their `320`-descriptor / `104 MiB` bounds (`342–347` checks each in one lab run; the per-writer summaries are run-dependent and printed by `course run`). Opening every file for `PRAGMA wal_checkpoint(TRUNCATE)` finds **`0` WAL frames** because `sqlite3WalClose` already checkpointed and removed `-wal` on writer close. Deleting `w1`–`w8` leaves **`0` files** on disk.
  - **C1–C4 (empty-disk rebuild):** Eight in-process rebuild workers (`fresh-1`–`fresh-8`) claim and restore all `500` databases in about `1.6s` (one lab run) within the rebuilding process's `104`-descriptor / `50 MiB` bound (`K = 8`). The `CountingStore` records **`5,496` objects read in `500` listings and `500` fence objects written** (the byte count is run-dependent: about `384,547,250` bytes, `769,094` per database, in one lab run). All **`500 of 500`** rebuilt databases are **E1 byte-identical** to the quiesced originals: **`252` at recipe step `4`** and **`248` at recipe step `5`**, matching both the catalog's `schema_version` and the last acknowledged step.
  - **D1 and E1 (final checker pass and double-read accounting):** The final pass inspects all `500` databases, reading the store's entire contents (about **`296,918,625` bytes** in one lab run), and rebuilds **`3,248` restore points** (`252 * 6 + 248 * 7`: initial claim fence + `4` segments + optional rollout segment + rebuild fence) with **`0` failures**. Excluding the `500` newly written rebuild fences leaves `2,748` pre-rebuild slots, and `5,496 == 2 * 2,748`: `node.Claim` read every slot **exactly twice**—once in `adapter.Refresh` to validate the lineage, and once in `walship.LoadCatalog` to plan and restore. Across all replies (writers + checker; **`2,760`** with total runtime about `8.7s` in one lab run), `0` problems and `0` bounds violations occurred.
- **2,000-database outline-scale run (`evidence/10.4-rebuild-the-fleet-2000.txt`):**
  - Each of the `8` writers owns exactly `250` databases (at most `32` open); schema rollout stops halfway at **`1,000 of 2,000`**.
  - The concurrent checker rebuilds restore points with `0` failures while the writers run (run-dependent: `6` passes and `32,359` points in one lab run, with an about `17.2s` write stream + `2.7s` rollout).
  - Quiesce records all `2,000` files (`0` WAL frames found), wipes local disk (`0` files left), and rebuilds all **`2,000 of 2,000`** databases **E1 byte-identical** (`1,000` at step `4`, `1,000` at step `5`) in about `5.6s` (one lab run), reading **`22,000` objects** (`2 * 11,000` slots; about `1,538,925,482` bytes, about `769,462` per database, in that run) in `2,000` listings and writing `2,000` fence objects.
  - The final checker pass reads about `1,188,042,741` bytes in that run and verifies **`13,000` restore points** (`1,000 * 6 + 1,000 * 7`) with **`0` failures**. Standing bounds held across all replies (`11,016` in that run, total runtime about `39s`).

---

### 6.4 Version notes

From [`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md) and [`OUTLINE.md`](../OUTLINE.md):

- **SQLite 3.49.0:** Added `SQLITE_DBCONFIG_ENABLE_ATTACH_CREATE` and `SQLITE_DBCONFIG_ENABLE_ATTACH_WRITE` to prevent SQL statements from attaching or creating extra database files outside a platform's connection manager.
- **SQLite 3.50.0 (fixed in 3.50.1/3.50.2):** Added `sqlite3_setlk_timeout()` (when compiled with `SQLITE_ENABLE_SETLK_TIMEOUT`) for blocking POSIX locks instead of busy-handler polling; 3.50.2 also eliminated checksum-less WAL frames after a savepoint rollback with spilled pages.
- **SQLite 3.51.0 (fixed in 3.51.2):** Added `SQLITE_CHECKPOINT_NOOP` and hardened POSIX lock handling against accidental `close()` lock drops via `unixIsSharingShmNode` in `unixLock` (`src/os_unix.c:2034-2041`; 3.51.2 fixed a deadlock in that check).
- **SQLite 3.51.3 / 3.53.0:** Fixed the WAL-reset database corruption bug; our pinned `3.53.4` includes the fix, whereas older embedded builds in some language drivers may not.
- **Cross-version byte hashes:** SQLite stores the writer's library version in header bytes `96–99` (`3053004` at our pin). E1 byte-identical rebuild checks therefore hold within one SQLite version; across a SQLite library upgrade, compare rebuilt files at **E2 pages** or **E3 logical** (`internal/equal`).

---

### What attacked this chapter

<!-- adversarial-results -->

Chapter 10 was built and attacked in two chunks—the service core plus Experiment 10.1 (`ch10a`, `27` attacks across four scripts driven by [`agent_work/ch10a/adversarial/run-all.sh`](../agent_work/ch10a/adversarial/run-all.sh)) and relocation plus Experiments 10.2–10.4 (`ch10b`, `25` attacks across three scripts driven by [`agent_work/ch10b/adversarial/run-all.sh`](../agent_work/ch10b/adversarial/run-all.sh))—for **`52` adversarial attacks** total on real SeaweedFS S3 and SQLite `3.53.4`.

#### Chunk 10a (`service/` core, `walship.AdoptAtRest`, and Experiment 10.1)

- [`model-and-mustfail/main.go`](../agent_work/ch10a/adversarial/model-and-mustfail/main.go) (`MOD-01`–`MOD-05b`): verified Experiment 10.1 exits non-zero on both model mutants (`MOD-01`), proved removing `If-None-Match: *` on live SeaweedFS causes a split-brain overwrite caught by `fence.Audit` (`MOD-02`), proved raw `walship.Open` without `fence.Adapter` lets a restarted stale owner clobber newer history despite `PutIfAbsent` (`MOD-03`), checked an independent `3`-node / `5`-epoch / multi-slot-stale state machine (`1,453` BFS states to depth `6` + `4,000,000` random-walk steps, `0` violations vs `496/500` on an overwrite control, `MOD-04`), confirmed seed-independence across `5` seeds (`MOD-05a`), and probed `model.MaxLocal` and `ParseMutant` (`MOD-05b`).
- [`fencing-and-crashes/main.go`](../agent_work/ch10a/adversarial/fencing-and-crashes/main.go) (`FENCE-01a`–`FENCE-05`): `SIGKILL`ed and restarted stale owners with persisted `.owner` files (`FENCE-01a`), unshipped `local-durable` commits (`FENCE-01b`), `stale-list` hiding the new owner's fence (`FENCE-01c`), and `crash-after-put` between fence PUT and `.owner` write (`FENCE-01d`); raced `8` concurrent S3 `PutIfAbsent` writers × `20` rounds and `8` concurrent `catalog.Claim` handles × `25` rounds (`0` S3 violations, `200/200` unique epochs, `0` busy errors, `FENCE-02`); raced `16` rounds of concurrent `node.Claim` vs `node.Claim` (`FENCE-03a`) plus a deterministic `Claim` race before `RecordShipped` (`FENCE-03b`); exhausted `claimAttempts = 3` against an active writer (`FENCE-04`); and injected `fail-before-put`, `fail-after-put`, `partial-put`, and 2-slot `stale-list` on SeaweedFS (`FENCE-05`).
- [`catalog-and-lifecycle/main.go`](../agent_work/ch10a/adversarial/catalog-and-lifecycle/main.go) (`CAT-01`–`CAT-05b`): checked catalog synchronization after eviction/close (`CAT-01`) and post-crash reopen (`CAT-02`), failed re-`Claim` belief cleanup (`CAT-03a`), malformed `epoch 0` belief rejection (`CAT-03b`), offline file tampering across eviction (`CAT-04a`) and clean close + restart (`CAT-04b`), standing bounds rotating `600` databases (`K = 16`, `DirStore`) plus `50` on SeaweedFS (`K = 4`, `CAT-05a`), and RSS growth across `180` × `256 KiB` updates on one small open database (`CAT-05b`).
- [`atrest-and-fence-edges/main.go`](../agent_work/ch10a/adversarial/atrest-and-fence-edges/main.go) (`ATREST-01`–`ATREST-03`, `FENCE-EDGE-01`): `AdoptAtRest` after `DELETE` + `VACUUM` shrink (`ATREST-01`), rolled-back spilled WAL / 10-byte stub WAL / external `TRUNCATE` on slot `0` (`ATREST-02`), refusal on 1-bit header byte `80` flip (`integrity_check = ok`) / unshipped WAL / canceled context (`ATREST-03`), and `fence.Adapter` edge errors plus `STORE UNREACHABLE:` exit `1` (`FENCE-EDGE-01`).

**10a Run 1** ([`agent_work/ch10a/adversarial/results-run1.txt`](../agent_work/ch10a/adversarial/results-run1.txt)) found **`18 HELD, 9 BROKEN`**, exposing real defects fixed in [`agent_work/ch10a/REPAIR-REPORT.md`](../agent_work/ch10a/REPAIR-REPORT.md) alongside twelve code-review findings:

- **`CAT-05b` (fixed):** `fence.Adapter` cached raw object bytes of every slot in RAM, pushing RSS growth to `37.9 MiB` on one `276 KiB` database (exceeding the `34 MiB` bound). Fixed so `Adapter` caches metadata only and `Get` fetches from the store; peak RSS growth dropped to `2 MiB`.
- **`CAT-04b` (fixed):** At-rest digests lived only in memory, so offline tampering between clean `Close()` and `Start()` was silently published to S3 as a discontinuity snapshot. Fixed by persisting `<id>.atrest` on clean close and deleting it only after the reopen check passes.
- **`CAT-01` & `CAT-02` (fixed):** Ships performed during `evicted()` and `openDatabase()` did not call `Catalog.RecordShipped`, leaving `last_shipped` and `schema_version` stale. Fixed so every ship path updates the catalog.
- **`FENCE-03a/b`, `CAT-03a`, `CAT-03b`, and review findings R1/R3/R5/R6/R8/R9/R10/R11/R12 (fixed):** `Claim` now ignores `catalog.ErrStaleEpoch` after its fence lands in S3 and deletes `.owner` and `.atrest` on disk when clearing ownership; `Start` rejects `epoch 0` belief files; `Adapter.Refresh` commits its cached map only after the whole chain passes fencing; `Adapter.PutIfAbsent` validates envelopes before uploading; `Decode` rejects empty database segments in slot keys; `model.ParseMutant` rejects unknown `-mutant` flags; `faultstore` skips empty listings on `stale-list`.
- **`MOD-05b` (claim narrowed):** Run 1 flagged that `10.1`'s printed banner described `MaxLocal = 5` as `"5 local commits per node"`, whereas `MaxLocal` caps a node's `Local` stream position across all epochs and fences (so a node fencing at `6` cannot commit). The banner and chapter text were narrowed to state the true bound; after aligning `MOD-05b`'s expectation in Run 3, `ch10a` finished **`27 HELD, 0 BROKEN`** ([`agent_work/ch10a/adversarial/results.txt`](../agent_work/ch10a/adversarial/results.txt)).

#### Chunk 10b (relocation, fleet inspection, and Experiments 10.2–10.4)

- [`kills-and-durability/main.go`](../agent_work/ch10b/adversarial/kills-and-durability/main.go) (`KILL-01`–`KILL-05`): verified `10.2` catches `-mutant ack-before-put`, rejects `-mutant bogus`, and passes `-portable` (`KILL-01`); injected `crash-before-put` / `crash-after-put` during LRU eviction (`KILL-02a`), initial and recovery `Claim` fence snapshots (`KILL-03a`), and `openDatabase` discontinuity snapshots (`KILL-03b`); ran `strace` `SIGKILL`s at `8` fsyncs across LRU eviction (`KILL-02b`), `3` fsyncs inside `Claim` (`KILL-03c`), all `6` `catalog.db*` fsyncs × both recovery modes (`12` runs, `KILL-04`), and `8` mid-`pwrite64` points across both modes (`KILL-05`). All `8` attacks held on Run 1.
- [`relocation-faults/main.go`](../agent_work/ch10b/adversarial/relocation-faults/main.go) (`REL-01`–`REL-05c`): verified `10.3 -mutant skip-fence` fails and `-mutant bogus` exits `1` (`REL-01`); injected `partial-put` on final segment and fence slots (`REL-02a`); combined `fail-after-put` fence with `stale-list` on `Rollback` (`REL-02b`, fenced at `a.Drain` and cleanly adopted by `Resume`); relocated a database held open by a long reader (`REL-03a`: `Drain` refused cleanly because `-wal` held `90,672` bytes and `-shm` stayed open, `0` writable nodes; `REL-03b`: closing the reader let `Resume` finish cleanly with `user_version = 3` and E1 match); rolled back while destination `b` stayed down (`REL-04`); deleted `a`'s `.sealed` file after `b` activated to verify S3 fencing still stops an uncooperative source (`REL-05a`); ran a third-node `c.Claim` takeover mid-relocation (`REL-05b`); and injected `SQLITE_BUSY` on `catalog.db` during `b.Activate`'s `recordShipped` (`REL-05c`).
- [`fleet-corruption-and-fencing/main.go`](../agent_work/ch10b/adversarial/fleet-corruption-and-fencing/main.go) (`FLEET-01a`–`FLEET-04`): deleted slot `3` (`FLEET-01a`), bit-flipped slot `3`'s envelope and deep WAL-frame payload (`FLEET-01b`), deleted opening fence slot `1` (`FLEET-01c`), and shipped a valid non-recipe SQL table (`FLEET-01d`) in a `16`-DB SeaweedFS fleet, confirming `InspectFleet` isolated the exact damaged database and named the exact `LastGood` restore point while all healthy DBs rebuilt E1; injected `PartialPut` plus a stray key under `log/` (`FLEET-02`: never counted as restore points, checker bounds ok); rebuilt `8` DBs while old writers held them open (`FLEET-03a`, `8/8` fenced) and raced `4` rebuild workers against `2` active writers hammering `16` DBs (`FLEET-03b`, `16/16` fenced, `0` lost `store-durable` acks); and verified `10.4` at `32` DBs plus `-capacity 200000` hitting `FATAL descriptor limit` (`FLEET-04`).

**10b Run 1** ([`agent_work/ch10b/adversarial/results-run1.txt`](../agent_work/ch10b/adversarial/results-run1.txt)) found **`23 HELD, 2 BROKEN`** (`REL-04` and `REL-05c`), which together with [`agent_work/ch10b/REVIEW-1.md`](../agent_work/ch10b/REVIEW-1.md) (3 blockers, 2 should-fix, 2 nits) were fixed in [`agent_work/ch10b/REPAIR-REPORT.md`](../agent_work/ch10b/REPAIR-REPORT.md):

- **`REL-04` & Review #1 (fixed):** `Coordinator.Rollback` called `Destination.Discard` before `Catalog.FinishRelocation(..., RelocationRolledBack)`, so rolling back when destination `b` was down left `a` owning epoch `3` in `databases` while `relocations.state` stayed `running`—blocking any new relocation and turning a retry into `superseded`. Fixed by recording `rolled-back` immediately after `source.Claim`, making `Destination.Discard` best-effort (`Result.DiscardErr`), recognizing a retried rollback after `source.Claim` via `reclaimedBySource`, and adding Experiment 10.3 case `32`.
- **`REL-05c` & Review #2 (fixed):** Repeated `Node.Activate` on a node already holding `epoch` returned early without calling `recordShipped`, leaving `location = ""` and stale `last_shipped`/`schema_version` forever if the first activation failed at `catalog.db` or crashed after writing `.owner`. Fixed to re-record the catalog when the store still ends at `position + 1`, while leaving the catalog untouched if the node already shipped writes past the fence.
- **Review #3–#7 (fixed):** Experiment 10.2 Part B now measures only the write loop (excluding `setup()` and `Reap()`), redraws up to `5` times if a random kill misses the write window, and enforces an `EXACT` check that every random kill landed in flight; Experiment 10.3 defers killing remaining child processes on failed recovery and asserts `len(env.Tracker.Problems) == 0`; `Node.Drain` fails if the final eviction reported problems; `user_version` is decoded as full 4-byte big-endian; and Experiment 10.4 prints `each owns 62-63` for uneven splits.

**10b Run 2** ([`agent_work/ch10b/adversarial/results-run2.txt`](../agent_work/ch10b/adversarial/results-run2.txt)) and the final run ([`agent_work/ch10b/adversarial/results.txt`](../agent_work/ch10b/adversarial/results.txt)) passed **`25 HELD, 0 BROKEN`**, and [`agent_work/ch10b/REVIEW-2.md`](../agent_work/ch10b/REVIEW-2.md) passed with `0` new findings.

---

## 7. Platform implications

### Answers to the eight platform design questions

Every answer below is grounded in this chapter's experiments and adversarial tests:

1. **How many connections and file descriptors can remain open?**
   Each open service database holds **`3` SQLite connections** (the writer plus `walship`'s control and pin connections) and opens **`7` file descriptors** (`3` `.db` + `3` `-wal` + `1` process-shared `-shm` via `unixInodeInfo`, `src/os_unix.c:1323-1342`, measured in Experiment 10.1's `0 / 7 / 14` descriptor progression). Bounding open databases per node to `K` via LRU eviction (`internal/connmgr`) caps descriptor growth at **`<= 3 * 3 * K + 32`** (e.g., `320` for `K = 32` in Experiment 10.4, verified against `procstat.FileLimit()` at startup and tested by `FLEET-04`). Closed databases hold `0` descriptors.
2. **How much cache memory is consumed per database?**
   Each connection sets `cache_size = -256` (`256 KiB`), giving a standing upper bound of `3K * (2 * 256 KiB + 256 KiB) + 32 MiB` (`41 MiB` for `K = 4`, `104 MiB` for `K = 32`), which held across every reply in Experiments 10.1–10.4 (`15`, `308`, `1,726`, `2,760`, and `11,016` replies in one lab run of each; the counts after 10.1 are run-dependent) and across `600` rotated databases in `CAT-05a`. Keeping `fence.Adapter`'s slot cache metadata-only (`CAT-05b`) is essential so long segment histories do not leak RAM inside open handles.
3. **What is the write-ownership model?**
   The control-plane catalog names one `owner` and monotonically increasing `epoch` per database; the object store enforces write-once position slots (`If-None-Match: *`); and every writer obeys `fence.Adapter`'s rule ("append only at `end + 1` of your validated chain, and stop with `FencedError` if any slot holds a higher epoch"). Ownership changes only when the new owner's fence snapshot lands in the store—either abruptly by `claim` (Experiment 10.1, losing unshipped `local-durable` writes) or cooperatively by seven-step relocation (Experiment 10.3, draining and keeping them).
4. **What happens when a reader blocks checkpoints?**
   The shipper's own pin deliberately caps `walCheckpoint` (`src/wal.c:2233-2251`) at shipped frames while writers disable autocheckpointing (`wal_autocheckpoint=0`). An external long-lived reader likewise caps `PASSIVE` checkpoints so `-wal` grows while it stays open (Chapter 5.3, Chapter 9.2), and during relocation it blocks `sqlite3WalClose` (`src/wal.c:2529`) from checkpointing and deleting `-wal`/`-shm`—so `Node.Drain` refuses cleanly with `0` writable nodes until the reader closes, after which `Resume` completes without data loss (`REL-03a`, `REL-03b`).
5. **How are consistent snapshots produced?**
   Two ways: `walship` reads pages `1..N` through `sqlite_dbpage` inside its `BEGIN IMMEDIATE` transaction (Chapter 9.4), and fence snapshots during `Claim` or `Activate` are produced by replaying the validated snapshot + segment chain in Go and checking every commit's SHA-256. Experiment 10.4's checker verified `3,248` (`500` DBs) and `13,000` (`2,000` DBs) published restore points with `0` failures.
6. **How is a database moved between machines without two writers?**
   Through the seven-step relocation protocol (`service/relocate`): catalog epoch bump (1), source seal (2), drain to at-rest (3), destination stage (4), hash verify (5), store fence and activate (6), and source retire (7). Step `2` stops a cooperative source; step `6` binds an uncooperative source even if `.sealed` is deleted (`REL-05a`). Experiment 10.3 verified `<= 1` writable node across `32/32` fault cases, while removing steps `2` and `6` (`skip-fence`) immediately produced `2` writable nodes.
7. **Which metadata belongs in a global control plane versus local SQLite files?**
   - **Control plane (`catalog`):** `owner`, `epoch`, `location`, `last_shipped`, `schema_version`, per-connection settings (Chapter 8.1), and the `relocations` row (`step`, `position`, `sha256`, `state`).
   - **Local node disk:** `<id>.db` (carrying persistent file settings: page size, encoding, `application_id`, `user_version`, `journal_mode=wal`) plus crash-surviving sidecars (`.owner`, `.atrest`, `.sealed`, `.staged`).
   - **Object store:** the authoritative slot lineage. A stale catalog `last_shipped` is self-healing on the next ship or repeated `Activate` (`REL-05c`), and `RecordShipped` ignores `ErrStaleEpoch` once the store accepted a put (`FENCE-03b`).
8. **What is the recovery path after an interrupted upload or ownership transfer?**
   - **Interrupted upload:** a failed put marks the shipper broken and evicts the database; the next open lists the store (`fence.Adapter` treats byte-identical retries as success and surfaces `fail-after-put` / `crash-after-put` puts already in S3) and ships any unshipped local commits as a segment or discontinuity snapshot (Experiments 10.2, 10.3 cases `22–31`, `FENCE-05`).
   - **Interrupted transfer:** a fresh coordinator reads `relocations` and lists S3: newest epoch `== to_epoch` rolls forward by adopting the fence (cases `6`, `7`, `13`, `18`, `19`, `25`, `29`); newest `< to_epoch` either replays idempotent steps `2–7` (`Resume`) or drains and re-claims on the source at `to_epoch + 1`, records `rolled-back`, and best-effort discards `.staged` (`Rollback`, cases `8–12`, `20–21`, `30–32`); newest `> to_epoch` finishes `rolled-back` if the source already reclaimed or `superseded` if another node took over (`REL-05b`).

### Operational signals worth exporting

Every signal below is already produced by the capstone packages:

- **Per database:** `owner`, `epoch`, `last_shipped`, and `schema_version` (`catalog`); unshipped local lag (`local_position - last_shipped`, measuring `local-durable` exposure); `walship.CycleReport.Checkpoint` (`busy`, `log`, `checkpointed`) and `FullBackfill` (detecting checkpoint-blocking readers); WAL change kind per cycle (frequent `WALDiscontinuity` means expensive full snapshots); at-rest SHA-256 on clean close; relocation `step` and `state`; node `Status` (`writable`, `sealed`, `fenced`); `fence.Inspect` `LastGood` and failure count; store slot count and total bytes (restore cost).
- **Per node process:** descriptors above baseline vs `DescriptorLimit()`, RSS growth vs `MemoryLimit()`, `connmgr.Stats` (open count, evictions), and background `TakeProblems()` count.

### What this design does not handle

- **Multi-host control plane:** `catalog.db` is one SQLite file on one host; a real multi-machine platform needs a networked consensus-backed control plane exposing the same atomic `Claim` and `BeginRelocation` operations (and managing its own availability).
- **Automatic failover and leases:** ownership moves only when an operator or orchestrator calls `Claim` or `relocate.Run`; there are no heartbeat leases, failure detectors, or automatic retries of stuck `running` relocations.
- **Periodic compaction snapshots and slot pruning:** `walship.SnapshotEvery` is `0` (only fence snapshots reset restore length). Adding compaction snapshots requires putting them at `end + 1` in the writer's epoch (not at the last segment's position, because slots are write-once), and pruning old prefixes requires recording a floor position in the catalog so `CheckLineage` can distinguish a pruned prefix from a deleted slot `1`.
- **Garbage collection:** orphaned local files on a fenced node remain until re-claimed or retired, and historical slots in S3 are never deleted.
- **Non-atomic object stores:** a truncated partial object written into a write-once slot wedges that database past `LastGood` because slots cannot be overwritten (`REL-02a`, `FLEET-02`).
- **Power loss beyond Chapter 9.2's model:** Chapter 10 injects process kills and store faults, not power cuts, torn disk writes, or SeaweedFS storage failures.
- **Ambiguous client replies:** if a node dies after committing or shipping and before printing `ok write …` (as at `catalog.db-wal` in Experiment 10.2), the client sees a broken connection and must use idempotent writes or query the database to learn whether its write landed.
- **Security, quotas, multi-tenant isolation, complex DDL migrations, and SQLite version upgrades:** out of scope; cross-version upgrades invalidate E1 byte hashes at header bytes `96–99`.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. Why can SeaweedFS / S3 conditional writes (`If-None-Match: *`) not enforce single-writer ownership by themselves, and what happens in Experiment 10.1's `resume-from-global-end` mutant when `If-None-Match: *` stays enabled while `fence.Adapter`'s epoch check is removed?
2. Why does a **takeover** (`Node.Claim`, Experiment 10.1) lose the previous owner's unshipped `local-durable` acknowledgements whereas a **relocation** (`relocate.Coordinator`, Experiment 10.3) preserves them—and why did Experiment 10.3 need a separate `SealSource` step after `FenceEpoch`?
3. In Experiment 10.2 Part A, why did `store-durable` kills at `fsync 29`, `57`, and `85` land on `catalog.db-wal` and recover `user_version` **one step ahead** of the client's acknowledged count even after the node directory `_work/10.2/node/a/` was completely deleted?
4. In Experiment 10.3, why did a `fail-before-put` on `a`'s final segment (case `22`) move the fence point from position `5` to position `4` while keeping the exact same drained SHA-256 (`d53fc07b1293…`), and why must `Coordinator.Rollback` record `rolled-back` in the catalog **before** calling `Destination.Discard` (case `32` / `REL-04`)?
5. In Experiment 10.4, why did `PRAGMA wal_checkpoint(TRUNCATE)` find **`WAL frames found: 0`** across all `500` (and `2,000`) databases at quiesce, and why did the empty-disk rebuild read **`5,496` objects** (`22,000` at `2,000` databases)—exactly **twice** the number of pre-rebuild slots?

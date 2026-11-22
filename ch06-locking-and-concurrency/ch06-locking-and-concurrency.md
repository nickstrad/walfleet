# Chapter 6 — Locking and concurrent access

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `os_unix.c`, `os.h` |
| Linked C sources (in the pinned tree, not copied) | `src/wal.c`, `src/pager.c`, `src/btree.c`, `src/build.c`, `src/main.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree, the C tools (`sqlite3`, `showwal`, `dbhash`; Experiments 6.1–6.4 use pure Go child processes, and the `sqlite3` CLI is needed only by the cross-build adversarial scripts under `agent_work/ch06/adversarial/`), `strace`, and the DuckDB CLI that presents the results.
> - **Run an experiment** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 6.1`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 6.1
>   go run ./cmd/course run 6.2
>   go run ./cmd/course run 6.3
>   go run ./cmd/course run 6.4
>   ```
>   Experiment 6.4 also accepts `-hold read` (the default, stored as evidence) or `-hold immediate`. `course run` cannot pass experiment flags, so run the variant directly in the container: `docker compose run --rm lab go run ./ch06-locking-and-concurrency/experiments/6.4-live-snapshot -hold immediate`. In the container, scratch databases and tables are written under `/work/6.1/` through `/work/6.4/` (the `lab-work` volume); run directly, under `_work/6.1/` through `_work/6.4/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch06-locking-and-concurrency/experiments/6.2-busy-rules`). Run this way it prints only its verdict and leaves its tables under `_work/6.2/results/`.
> - **Linux only (Experiment 6.1 `/proc/locks` view) and macOS:** Experiment 6.1's `/proc/locks` kernel lock listing (and its 11 `/proc/locks:` checks) is **Linux only**; `course run` runs it in full in the Linux `lab` container. Run directly on macOS, Experiment 6.1 prints a `LINUX-ONLY:` notice, skips `/proc/locks`, and still runs the portable `fcntl(F_GETLK)` probe and all 32 remaining checks. Experiments 6.2, 6.3, and 6.4 use nothing Linux-specific and also run directly on macOS.
> - **Measurement environment:** The stored evidence in this chapter was captured in the Debian `lab` container on `linux/arm64` (Go 1.26). The checked values are identical to the earlier `linux/amd64` capture; process ids and the work directory differ between runs and are not part of the stored evidence.

---

## 1. Model

On Unix systems, SQLite coordinates concurrent processes using **POSIX advisory locks** — byte-range locks placed on a file through the `fcntl(F_SETLK)` system call. "Advisory" means the kernel tracks lock conflicts only between processes that ask for locks; plain `read` and `write` calls ignore them completely.

Two rules of POSIX advisory locking govern every result in this chapter:

1. **Locks belong to the OS process and the inode, not to a connection or a file descriptor.** An **inode** is the filesystem's internal identity for a file; a **file descriptor** is one process's integer handle to an open file. If a process opens the same file twice, POSIX treats both descriptors as the same lock owner. Worse, closing *any* file descriptor to that inode immediately drops *every* POSIX lock the process holds on that file. SQLite works around this for its own connections by tracking locks per inode in memory (`unixInodeInfo`) and parking closed descriptors until all locks clear, but SQLite cannot intercept an `os.Open` and `Close` made by application code in the same process.
2. **Rollback mode and WAL mode lock different files.**
   - In **rollback-journal mode** (`journal_mode=delete`), all locks live on the main database file (`.db`) at fixed offsets starting at the 1 GiB boundary (`0x40000000`, byte `1073741824`): a 1-byte `PENDING` byte (`1073741824`), a 1-byte `RESERVED` byte (`1073741825`), and a 510-byte `SHARED` range (`1073741826-1073742335`). A connection progresses through five lock states: `NO_LOCK` (0), `SHARED` (1, read lock on the 510-byte range), `RESERVED` (2, write lock on byte `1073741825` — one writer may buffer changes while existing readers keep reading), `PENDING` (3, write lock on byte `1073741824` — blocks new readers while waiting for existing readers to finish), and `EXCLUSIVE` (4, write lock on the 510-byte `SHARED` range — required to write modified pages into `.db`).
   - In **write-ahead log (WAL) mode** (`journal_mode=wal`), every open connection holds a permanent `SHARED` read lock on `.db` (only upgraded to `EXCLUSIVE` when the last connection closes or leaves WAL mode) and a shared read lock on the **dead-man switch (`DMS`)** byte (`128`) of the sibling `-shm` shared-memory index file. All active transaction locks live on bytes `120–127` of `-shm`: byte `120` is `WAL_WRITE_LOCK` (one writer at a time), byte `121` is `WAL_CKPT_LOCK` (one checkpointer at a time), byte `122` is `WAL_RECOVER_LOCK`, and bytes `123–127` are five reader slots `WAL_READ_LOCK(0)` through `WAL_READ_LOCK(4)`.

```text
  Rollback mode (journal_mode=delete)             WAL mode (journal_mode=wal, locking_mode=NORMAL)
  ===================================             ================================================

  Main database file (.db)                        Main database file (.db)
  +-------------------------------------+         +-------------------------------------+
  | byte 1073741824      PENDING   (1B) |         | bytes 1073741826-1073742335 SHARED  |
  | byte 1073741825      RESERVED  (1B) |         |   (held read-locked by every open   |
  | bytes 1073741826..   SHARED  (510B) |         |    WAL connection; EXCLUSIVE only   |
  |       1073742335                    |         |    on last close / mode change)     |
  +-------------------------------------+         +-------------------------------------+
    Reader: READ on SHARED.
    Writer: SHARED -> RESERVED (buffers)          Wal-index file (.db-shm)
            -> PENDING (blocks new                +-------------------------------------+
               readers) -> EXCLUSIVE              | byte 120  WAL_WRITE_LOCK    (1B)    |
               (writes .db, unlinks               | byte 121  WAL_CKPT_LOCK     (1B)    |
                -journal, unlocks).               | byte 122  WAL_RECOVER_LOCK  (1B)    |
                                                  | byte 123  WAL_READ_LOCK(0)  (.db)   |
  Stray os.Open+Close of .db in holder:           | byte 124  WAL_READ_LOCK(1)  (mark1) |
    -> drops RESERVED + SHARED at once.           | byte 125  WAL_READ_LOCK(2)  (mark2) |
                                                  | byte 126  WAL_READ_LOCK(3)  (mark3) |
                                                  | byte 127  WAL_READ_LOCK(4)  (mark4) |
                                                  | byte 128  DMS (dead-man switch, 1B) |
                                                  +-------------------------------------+
                                                    Reader: READ on WAL_READ_LOCK(i) (i in 0..4).
                                                    Writer: WRITE on byte 120 + its read lock.
                                                    Checkpoint: WRITE on byte 121; backfills up
                                                      to lowest locked read mark (needs READ(0)
                                                      exclusively before copying any page).

                                                  Stray os.Open+Close in holder:
                                                    of .db  -> drops .db SHARED only (byte 120 stays).
                                                    of -shm -> drops ALL holder -shm locks at once.
```

Three further terms appear throughout the chapter:

- **Transaction start modes (`BEGIN DEFERRED`, `BEGIN IMMEDIATE`, `BEGIN EXCLUSIVE`).** `BEGIN DEFERRED` (plain `BEGIN`) acquires no lock until the first SQL statement runs; if that statement reads, it starts a read transaction, and only a later write statement tries to upgrade to a write lock. `BEGIN IMMEDIATE` acquires the write lock up front (`WAL_WRITE_LOCK` in WAL mode, `RESERVED` in rollback mode). In WAL mode under the default `PRAGMA locking_mode=NORMAL`, `BEGIN EXCLUSIVE` behaves identically to `BEGIN IMMEDIATE`: it takes `WAL_WRITE_LOCK` and lets concurrent readers continue. Only under `PRAGMA locking_mode=EXCLUSIVE` does a WAL writer take and hold the database file's `EXCLUSIVE` lock, blocking other processes' readers.
- **Busy handler and `busy_timeout`.** When SQLite hits a lock conflict, it returns `SQLITE_BUSY` (extended code `5`) unless a **busy handler** is configured (via `PRAGMA busy_timeout=MS`) *and* SQLite decides retrying can succeed. A retry sleeps in growing intervals and tries the lock again. A deferred transaction that has already read cannot wait on the busy handler: if another writer holds the lock, SQLite returns `SQLITE_BUSY` (`5`) immediately regardless of `busy_timeout`, and once that other writer commits, upgrading returns `SQLITE_BUSY_SNAPSHOT` (`517`) immediately because the reader's **snapshot** (the frozen database view pinned at its first read) is no longer the newest committed state.
- **Torn copy versus live snapshot.** A **page** is SQLite's 4,096-byte storage unit; a **frame** is a page image plus 24-byte header appended to `-wal`; a **checkpoint** copies (backfills) committed WAL frames into `.db`. A **torn copy** is a file read with plain OS calls while SQLite writes into `.db` (during a rollback commit or a WAL checkpoint), mixing pages from before and after the write.

**WAL mode as the platform default.** Platform databases in this course run in WAL mode (`journal_mode=wal`, `locking_mode=NORMAL`). Every experiment in this chapter uses WAL mode, and rollback mode (`journal_mode=delete`) appears only as the contrast listed in the outline (Experiments 6.1 A, 6.2 D, and 6.3 B). Nothing measured here argues against WAL as the platform default; concurrency and live capture both depend on it:

| Question | WAL mode (`locking_mode=NORMAL`) | Rollback mode (`journal_mode=delete`) | Evidence |
| --- | --- | --- | --- |
| Does a reader block a commit? | No | Yes (`SQLITE_BUSY` at `COMMIT`; retained `PENDING` blocks new readers) | Experiment 6.2 A vs D |
| Does a writer block readers? | No | Yes once it reaches `PENDING`/`EXCLUSIVE` | Experiment 6.2 A vs D |
| Where does the writer lock live? | `-shm` byte `120` (`WAL_WRITE_LOCK`) | `.db` byte `1073741825` (`RESERVED`) | Experiment 6.1 A vs B |
| Does a stray close of `.db` break writer exclusion? | No (only a stray close of `-shm` does) | Yes | Experiment 6.1 B vs A |
| Outcome of broken locks | Depends on state: `SQLITE_PROTOCOL` (`15`) after ~10 s on an empty WAL (Part B), or both commits return `SQLITE_OK` and one is silently lost (Parts C and D) | Commit that returned `SQLITE_OK` is silently lost; holder gets `SQLITE_IOERR_DELETE_NOENT` (`5898`) and persists | Experiment 6.1 A–D |
| Safe live capture without blocking the writer? | Yes (Rung R4, both methods) | No (an open reader blocks commits for the whole capture) | Experiments 6.4, 6.2 D |

WAL adds two operational obligations proven here: the `-shm` file carries the active locks and must never be opened and closed by non-SQLite code in a serving process, and because WAL relies on POSIX locks and local shared memory, databases stay on local disk rather than network filesystems.

**The Rebuild Ladder (rung R4 — Live snapshot).** Chapters 1–5 established **R0 (Recipe)**, **R1 (Pages)**, **R2 (Canonical form)**, and **R3 (Redo log)**. This chapter adds **R4 (Live snapshot)**: capturing a consistent database from a live WAL writer without stopping writes. Experiment 6.4 proves two methods: reading every page through `sqlite_dbpage` inside one read transaction (rebuilt equal at **E1 bytes** to the recipe run to that snapshot step and checkpointed), and copying `.db` followed by `-wal` while holding a read transaction and replaying with `internal/walfile` (rebuilt to a consistent recipe state at or after the snapshot, even when a concurrent checkpoint tears `.db` during the copy).

---

## 2. Question

When two processes use one database, who waits, who gets `SQLITE_BUSY`, and when is it safe to copy a database that another process is writing?

Four scripted multi-process experiments answer each part without timing guesswork:

- **Experiment 6.1 (`6.1-locks-belong-to-the-process`)** inspects live kernel locks via `fcntl(F_GETLK)` and `/proc/locks`, triggers a stray `os.Open` + `Close` inside the lock-holding process, and records the exact lock state and commit outcomes in rollback mode (Part A) and three WAL states (Parts B, C, and D).
- **Experiment 6.2 (`6.2-busy-rules`)** scripts reader/writer and writer/writer interleavings in WAL mode (Parts A, B, C1–C3, E) and rollback mode (Part D) to record every extended result code and prove when `busy_timeout` waits versus fails immediately.
- **Experiment 6.3 (`6.3-torn-copy`)** copies a live WAL database without its `-wal` (Part A1) and copies `.db` in two halves across a WAL checkpoint (Part A2) and a rollback commit (Part B).
- **Experiment 6.4 (`6.4-live-snapshot`, rung R4)** captures a live WAL database under active commits and checkpoints using `sqlite_dbpage` in one read transaction (Method 1) and a plain `.db` + `-wal` copy under a held read transaction replayed with `internal/walfile` (Method 2).

The experiments reuse five shared packages from earlier chapters (linked here, not inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens databases with explicit settings and verifies `sqlite_source_id()`. This chunk adds two backward-compatible files to `internal/sqlitex`: [`codes.go`](../internal/sqlitex/codes.go) (constants and `ResultCodeName` for primary and extended codes such as `SQLITE_BUSY_SNAPSHOT = 517`, `SQLITE_PROTOCOL = 15`, and `SQLITE_IOERR_DELETE_NOENT = 5898`, because `modernc.org/sqlite`'s `ErrorCodeString` omits several extended codes) and [`display.go`](../internal/sqlitex/display.go) (`DisplayPath`, which formats repo-relative paths cleanly without leaking host directories).
- [`internal/recipe`](../internal/recipe/): builds the seeded R0 database and matches captured files against its known step hashes (`MatchStep`).
- [`internal/capture`](../internal/capture/): writes and rebuilds R1 page manifests (`Pages`, `Rebuild`).
- [`internal/equal`](../internal/equal/): checks **E1 bytes** equality (`CompareFiles`, `FileSHA256`).
- [`internal/walfile`](../internal/walfile/): replays a base `.db` plus `-wal` into a standalone database file (`ReplayFiles`).
- [`internal/trace`](../internal/trace/): prints the standard `LINUX-ONLY:` banner on non-Linux hosts (`PrintLinuxOnly`).

This chapter introduces one new shared package, `internal/procs`, which drives child processes through numbered step barriers over pipes and inspects POSIX locks. It is inlined in full in Section 5.

---

## 3. Call path

This section traces the pinned SQLite `version-3.53.4` C source for file locks, `-shm` locks, `BEGIN` modes, busy retries, and `SQLITE_BUSY_SNAPSHOT`. Two files are copied whole into this chapter directory and verified by `SOURCES.sha256`: [`os.h`](os.h) and [`os_unix.c`](os_unix.c). Five larger files (`src/wal.c`, `src/pager.c`, `src/btree.c`, `src/build.c`, `src/main.c`) are linked in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`).

### 3.1 Database-file lock levels and POSIX advisory locking

1. **Lock levels and byte offsets** — [`src/os.h:98-166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.h#L98-L166) and [`src/os.h:160-166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.h#L160-L166) (local copy: [`os.h`](os.h)). Defines `NO_LOCK` (0), `SHARED_LOCK` (1), `RESERVED_LOCK` (2), `PENDING_LOCK` (3), and `EXCLUSIVE_LOCK` (4), and places the lock bytes at the 1 GiB boundary:
   ```c
   # define PENDING_BYTE     (0x40000000)
   #define RESERVED_BYTE     (PENDING_BYTE+1)
   #define SHARED_FIRST      (PENDING_BYTE+2)
   #define SHARED_SIZE       510
   ```
   The pager never stores database data in the page containing byte `0x40000000` (`1073741824`) so mandatory locking on Windows and advisory locking on Unix share the same byte ranges.
2. **Why POSIX advisory locks require an in-process inode table** — [`src/os_unix.c:1189-1342`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1189-L1342) and `struct unixInodeInfo` at [`src/os_unix.c:1323-1342`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1323-L1342) (local copy: [`os_unix.c`](os_unix.c)). The block comment opens with `"POSIX advisory locks are broken by design"` and explains both defects: a lock placed through one descriptor overrides locks the same process placed through another, and closing *any* descriptor to the file releases all of that process's locks on that inode. SQLite therefore allocates one `unixInodeInfo` per `(dev, ino)` holding process-wide lock counters (`nShared`, `nLock`, `eFileLock`, `bProcessLock`), a pointer to the shared-memory node (`pShmNode`), and a list of descriptors whose `close()` must wait (`pUnused`). SQLite invokes `fcntl(F_SETLK)` only when the process-wide state changes.
3. **Deferred descriptor close (`setPendingFd` and `closePendingFds`)** — [`src/os_unix.c:2101-2109`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L2101-L2109) and [`src/os_unix.c:1471-1482`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1471-L1482) (local copy: [`os_unix.c`](os_unix.c)). When one SQLite connection closes while another connection in the same process still holds a lock on the same inode, `setPendingFd` moves the closing file descriptor onto `pInode->pUnused` instead of calling `close()`. Once the last lock on that inode clears, `closePendingFds` closes every parked descriptor.
4. **Lock escalation (`unixLock`)** — doc comment and transition chart at [`src/os_unix.c:1842-1865`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1842-L1865), function body at [`src/os_unix.c:1866-2095`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1866-L2095) (local copy: [`os_unix.c`](os_unix.c); note: line `1843` in earlier notes fell inside the doc comment, while the C function itself starts at line `1866`). Allowed upward transitions are `UNLOCKED -> SHARED`, `SHARED -> RESERVED`, `SHARED -> EXCLUSIVE`, `RESERVED -> (PENDING) -> EXCLUSIVE`, and `PENDING -> EXCLUSIVE`:
   - For `SHARED`, SQLite read-locks `PENDING_BYTE`, read-locks all `510` bytes of `SHARED_FIRST..SHARED_FIRST+509`, and unlocks `PENDING_BYTE`.
   - For `RESERVED`, SQLite write-locks `RESERVED_BYTE` (`1073741825`).
   - For `EXCLUSIVE` from `RESERVED`, SQLite first write-locks `PENDING_BYTE` (`1073741824`) so no new readers can start, then tries to write-lock the 510-byte `SHARED` range. If existing readers hold `SHARED`, `EXCLUSIVE` fails with `SQLITE_BUSY` and SQLite **keeps the `PENDING` write lock** until the writer retries or rolls back.
5. **Broken-lock guard (`unixIsSharingShmNode`)** — [`src/os_unix.c:2034-2041`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L2034-L2041) and [`src/os_unix.c:4679-4695`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4679-L4695) (local copy: [`os_unix.c`](os_unix.c)). Added in SQLite 3.51.0: before granting `EXCLUSIVE` on a WAL database (which happens on last close or `PRAGMA journal_mode=DELETE`), `unixLock` calls `unixIsSharingShmNode`, which runs `fcntl(F_GETLK)` for a write lock on the `-shm` dead-man-switch byte (`UNIX_SHM_DMS`, byte `128`):
   ```c
   }else if( unixIsSharingShmNode(pFile) ){
     rc = SQLITE_BUSY;
   }
   ```
   If another process still holds the `DMS` lock on `-shm`, `unixLock` refuses `EXCLUSIVE` with `SQLITE_BUSY` even if the other process's `.db` `SHARED` lock was wiped out by a stray `close(open(db))`.
6. **Journal deletion error (`unixDelete`)** — [`src/os_unix.c:6833-6869`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6833-L6869) (local copy: [`os_unix.c`](os_unix.c)). Calls `unlink`; if `errno == ENOENT` (line `6842`), returns `SQLITE_IOERR_DELETE_NOENT` (`5898`, line `6847`). In Experiment 6.1 A, when two rollback-mode writers commit after a stray close, the second writer deletes the shared `-journal` first and the holder's commit hits line `6847`.

### 3.2 WAL `-shm` locks, restarts, close, and `SQLITE_PROTOCOL`

7. **WAL `-shm` lock bytes** — [`src/os_unix.c:4602-4603`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4602-L4603), [`src/os_unix.c:4703-4786`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4703-L4786) (`unixShmSystemLock`, local copy: [`os_unix.c`](os_unix.c)), and [`src/wal.c:294-299`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L294-L299) (in the pinned tree). `UNIX_SHM_BASE` is `(22 + 8) * 4 = 120` and `UNIX_SHM_DMS` is `128`. Slot `n` maps to `-shm` byte `120 + n`: `0 = WAL_WRITE_LOCK` (`120`), `1 = WAL_CKPT_LOCK` (`121`), `2 = WAL_RECOVER_LOCK` (`122`), and `3 + i = WAL_READ_LOCK(i)` (`123–127`, `WAL_NREADER = 5`). `unixShmSystemLock` issues `fcntl(F_SETLK)` on the `-shm` file descriptor — a separate inode from `.db`.
8. **WAL restart, `WAL_READ_LOCK(0)`, and `SQLITE_PROTOCOL`** — [`src/wal.c:3875-3915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3875-L3915) (`walRestartLog`), [`src/wal.c:2152-2166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2152-L2166) (`walRestartHdr`), [`src/wal.c:2963`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2963) (`WAL_RETRY_PROTOCOL_LIMIT = 100`), [`src/wal.c:2985-2997`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2985-L2997), and [`src/wal.c:3020-3272`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3020-L3272) (`walTryBeginRead`, lines [`3053-3056`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3053-L3056) and [`3261-3265`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3261-L3265), in the pinned tree).
   - At the start of writing frames, `walRestartLog` enters its body **only** when [`src/wal.c:3879`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3879) holds:
     ```c
     if( pWal->readLock==0 ){
     ```
     That condition is true only when the writer's transaction started on an empty or fully backfilled WAL (so it took `WAL_READ_LOCK(0)`). Inside that branch, `walRestartLog` acquires `WAL_READ_LOCK(1..4)` exclusively — notably *not* `WAL_READ_LOCK(0)` — calls `walRestartHdr` if `nBackfill > 0`, unlocks `WAL_READ_LOCK(0)`, and calls `walTryBeginRead` with `useWal=1` to claim a WAL read slot.
   - If another writer committed after a stray close of `-shm`, `walTryBeginRead` with `useWal=1` retains the holder's stale cached header; every retry sees the shared header mismatch (`memcmp` at lines `3261-3265`) and loops. After `100` retries (`WAL_RETRY_PROTOCOL_LIMIT`, sleeps growing up to ~10 s total), lines `3053-3056` return `SQLITE_PROTOCOL` (`15`), which the source comment (lines `2992-2997`) attributes to `"some other process has gone rogue"`.
   - If the writer's transaction started when uncheckpointed frames already sat in `-wal`, it holds `WAL_READ_LOCK(1)` (`readLock != 0`), so line `3879` skips `walRestartLog` completely. After a stray close of `-shm`, the writer never re-reads the shared header and writes its frames right over the other process's frames, returning `SQLITE_OK` (Experiment 6.1 C).
9. **Last-connection close (`sqlite3WalClose`)** — [`src/wal.c:2507-2571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2507-L2571) (in the pinned tree). On close, a connection tries to acquire `EXCLUSIVE` on `.db` (line `2529`). If it succeeds — which happens either when it truly is the last connection, or after another process dropped both its `.db` and `-shm` locks via stray closes — it checkpoints, unlinks `-shm` (line `2559`), and unlinks `-wal` (line `2563`). If a displaced holder still has `-wal` open, its later `COMMIT` writes frames into the unlinked inode and returns `SQLITE_OK` (Experiment 6.1 D).
10. **Reader-limited checkpoint skipping and `WAL_READ_LOCK(0)` backfill lock** — [`src/wal.c:2199-2395`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395) (`walCheckpoint`), [`src/wal.c:2254-2260`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2254-L2260), [`src/wal.c:2312-2314`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2312-L2314), and [`src/wal.c:1764-1792`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L1764-L1792) (`walIteratorNext`, in the pinned tree). Two subtle rules matter for live captures:
    - Before backfilling a single page, lines `2254-2260` must take `WAL_READ_LOCK(0)` exclusively. A reader on `WAL_READ_LOCK(0)` therefore blocks all checkpoint backfills (keeping `.db` untouched for its snapshot) even though `walRestartLog` can restart the WAL under it.
    - `walIteratorNext` yields each page in the WAL **once**, at its newest frame number. At lines `2312-2314`, `walCheckpoint` copies that page only if its newest frame is `<= mxSafeFrame` (the oldest reader's mark). If a later commit rewrote page 1 past `mxSafeFrame`, page 1 is skipped altogether — so `nBackfill` can advance to `61` while page 1 in `.db` remains at an older state (Experiment 6.4).

### 3.3 `BEGIN` modes, busy retries, and `SQLITE_BUSY_SNAPSHOT`

11. **`BEGIN` code generation (`sqlite3BeginTransaction`)** — [`src/build.c:5245-5274`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/build.c#L5245-L5274) (in the pinned tree). `BEGIN DEFERRED` emits no `OP_Transaction` opcode, so SQLite takes no lock until the first statement executes. `BEGIN IMMEDIATE` emits `OP_Transaction` with `p2 = 1` (write lock), and `BEGIN EXCLUSIVE` emits `p2 = 2`.
12. **Pager write-lock acquisition (`sqlite3PagerBegin`)** — [`src/pager.c:5968-6032`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5968-L6032) (in the pinned tree). In WAL mode, only when `pPager->exclusiveMode` (`PRAGMA locking_mode=EXCLUSIVE`) is set do lines [`5982-5988`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5982-L5988) acquire the `.db` `EXCLUSIVE` lock; otherwise both `IMMEDIATE` and `EXCLUSIVE` proceed straight to `sqlite3WalBeginWriteTransaction` (line `5995`), without calling the busy handler inside the pager. In rollback mode, `sqlite3PagerBegin` takes `RESERVED_LOCK` (line `6002`), and upgrades via `pager_wait_on_lock(pPager, EXCLUSIVE_LOCK)` only if `exFlag` (`BEGIN EXCLUSIVE`, line `6003`) is set.
13. **Where the busy handler is — and is not — called (`btreeBeginTrans` and `sqliteDefaultBusyCallback`)** — [`src/btree.c:3598-3804`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3598-L3804) (`btreeBeginTrans`), [`src/main.c:1730-1792`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L1730-L1792) (`sqliteDefaultBusyCallback`), and [`src/main.c:1782-1792`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/main.c#L1782-L1792) (`sqlite3InvokeBusyHandler`, in the pinned tree). When `btreeBeginTrans` starts a write transaction, its retry loop at [`src/btree.c:3740-3741`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3740-L3741) is:
    ```c
    }while( (rc&0xFF)==SQLITE_BUSY && pBt->inTransaction==TRANS_NONE &&
            btreeInvokeBusyHandler(pBt) );
    ```
    The `pBt->inTransaction==TRANS_NONE` guard is decisive: if a connection has *not* opened a transaction yet (`BEGIN IMMEDIATE`, or `BEGIN DEFERRED` whose first statement is a write), `btreeBeginTrans` calls `sqlite3InvokeBusyHandler` → `sqliteDefaultBusyCallback`, which sleeps `1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100` ms per retry up to `busy_timeout`. If the connection is *already* inside a read transaction (`TRANS_READ`), `inTransaction != TRANS_NONE`, so the loop exits immediately with `SQLITE_BUSY` (`5`) without sleeping a single millisecond — even with `busy_timeout=120000`.
14. **Stale snapshot detection (`sqlite3WalBeginWriteTransaction`)** — [`src/wal.c:3699-3746`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3699-L3746) (in the pinned tree). Acquires `WAL_WRITE_LOCK` exclusively (line `3724`). If another connection committed since this connection's read transaction began, lines [`3734-3737`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3734-L3737) compare the cached and shared `WalIndexHdr`:
    ```c
    if( memcmp(&pWal->hdr, (void *)walIndexHdr(pWal), sizeof(WalIndexHdr))!=0 ){
      rc = SQLITE_BUSY_SNAPSHOT;
    }
    ```
    It then releases `WAL_WRITE_LOCK` and returns `SQLITE_BUSY_SNAPSHOT` (`517`). Because `inTransaction != TRANS_NONE`, `btreeBeginTrans` does not retry; the application must `ROLLBACK` and start a new transaction.

### Version notes

Behavior differences across SQLite releases relevant to locking (sources: [`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md), `changes.html`, and the copied source):

- **`sqlite3_setlk_timeout()` (added in 3.50.0; fixed in 3.50.1 and 3.50.2).** Enables kernel-level blocking locks (`F_SETLKW` with timeout) only when SQLite is compiled with `SQLITE_ENABLE_SETLK_TIMEOUT`. That compile option is **off** in our pinned 3.53.4 build, which uses non-blocking POSIX `F_SETLK` plus the user-space busy handler. Linux Open File Description locks (`F_OFD_*`) are not used anywhere in `os_unix.c`.
- **Broken-lock resistance via `unixIsSharingShmNode` (added in 3.51.0; deadlock fix in 3.51.2; [`src/os_unix.c:2034-2041`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L2034-L2041)).** Prevents a second connection from taking `EXCLUSIVE` on a WAL database file (to switch out of WAL mode or delete `-wal`/`-shm` on close) if another process still holds the `-shm` `DMS` lock after losing its `.db` locks to a stray `close(open(db))`. Older 3.x releases lack this guard.

### Experiment 6.1 — Locks belong to the process (`6.1-locks-belong-to-the-process`)

- **Claim:** SQLite's locks are POSIX byte-range locks owned by the process on a specific file inode — `.db` in rollback mode (`RESERVED` byte `1073741825`, `SHARED` bytes `1073741826-1073742335`) and `-shm` in WAL mode (`WAL_WRITE_LOCK` byte `120`, `WAL_READ_LOCK(i)` bytes `123-127`, `DMS` byte `128`). Closing any file descriptor to a file inside the holding process releases all of that process's locks on *that* file (so in WAL mode, a stray close of `.db` leaves writer exclusion intact, whereas a stray close of `-shm` breaks it). A second writer refused before the stray close is admitted after it while the first writer still believes its transaction is intact, and committing both loses a transaction: in rollback mode (Part A) the contender's `SQLITE_OK` commit is silently lost while the holder gets `SQLITE_IOERR_DELETE_NOENT` (`5898`) and persists; in WAL mode the outcome depends on state — `SQLITE_PROTOCOL` (`15`) after ~10 s on an empty WAL (Part B), or both commits returning `SQLITE_OK` with the contender's commit silently lost when uncheckpointed frames already exist in `-wal` (Part C) or when the contender closes and exits first (Part D).
- **Why kept:** This is the hazard a Go platform is most likely to hit by accident (hashing, stat-by-open, or copying a file it also has open).
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Linux only (partial):** The `/proc/locks` listing and its 11 `/proc/locks:` checks run on Linux only; the `fcntl(F_GETLK)` probe and all 32 behavioral checks run on both Linux and macOS. `course run` runs the full Linux version in the `lab` container.
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 6.1
  ```

<!-- include: experiments/6.1-locks-belong-to-the-process/main.go -->
```go
// Command 6.1-locks-belong-to-the-process demonstrates that SQLite's POSIX
// advisory locks are byte-range locks owned by the OS process on a specific
// inode—the database file in rollback mode (src/os.h:160-166) and the -shm
// file in WAL mode (src/os_unix.c:4602-4603, src/wal.c:294-299)—and that
// closing any file descriptor to that inode inside the holding process drops
// every lock it holds on that file (src/os_unix.c:1189-1255).
//
// Part A contrasts rollback mode; Parts B, C, and D show all three WAL
// outcomes after a stray close of -shm (SQLITE_PROTOCOL on an empty WAL,
// silent overwrite on an active WAL, and silent loss when the contender exits
// first and deletes -wal/-shm).
//
// It prints only its verdict. The steps, lock states and outcomes go to CSV
// tables that report.sql presents: go run ./cmd/course run 6.1
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process"

const rowsQuery = "SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '') FROM (SELECT who FROM lab ORDER BY id)"

// Every step, lock probe and outcome goes to a CSV table in <work dir>/results/;
// part labels the rows written while a part runs.
var (
	set     *results.Set
	part    string
	viewSeq int
	tables  struct {
		steps, lockViews, lockProbes, procLocks, finalState, files, infoRun, infoProcesses *results.Table
	}
)

func openTables() {
	tables.steps = set.Table("steps", "part", "step", "process", "action", "result", "code", "value")
	tables.lockViews = set.Table("lock_views", "seq", "part", "after_step", "file", "proc_locks_listed", "proc_locks")
	tables.lockProbes = set.Table("lock_probes", "part", "after_step", "file", "lock", "first_byte", "last_byte", "state", "holder")
	tables.procLocks = set.Table("proc_locks", "part", "after_step", "file", "process", "class", "type", "bytes", "lock")
	tables.finalState = set.Table("final_state", "part", "integrity", "contents")
	tables.files = set.Table("files", "seq", "part", "moment", "file", "state")
	tables.infoRun = set.Table("info_run", "work_dir", "goos")
	tables.infoProcesses = set.Table("info_processes", "part", "process", "pid")
}

// recorder is a procs.Lab that writes each step, lock view and claim to the
// tables instead of printing them.
type recorder struct{ *procs.Lab }

func newRecorder() *recorder {
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil
	return &recorder{lab}
}

func (l *recorder) Do(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, action, args, l.Lab.Do(child, action, args...))
}

func (l *recorder) Must(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, action, args, l.Lab.Must(child, action, args...))
}

func (l *recorder) step(child *procs.Child, action string, args []string, r procs.Result) procs.Result {
	value := r.Value
	if r.Code == procs.CodeNotSQLite {
		value = r.Error
	}
	tables.steps.Row(part, r.Step, child.Name, procs.ActionText(action, args), r.CodeName(), r.Code, value)
	return r
}

func (l *recorder) Check(ok bool, claim string) { set.Check(part, ok, claim) }

// Locks probes ranges on path from the parent (fcntl F_GETLK) and, on Linux,
// lists the children's /proc/locks entries for path.
func (l *recorder) Locks(file, path string, ranges []procs.LockRange) procs.LockView {
	states, err := procs.ProbeRanges(path, ranges)
	if err != nil {
		l.Fatal("probe locks", err)
	}
	step := l.Script.Step()
	for _, s := range states {
		// F_GETLK names one arbitrary holder of a shared read lock, so only a
		// write lock has a holder worth naming.
		var holder any
		if s.Holder.Held && s.Holder.Write {
			holder = l.PidName(s.Holder.Pid)
		}
		tables.lockProbes.Row(part, step, file, s.Range.Name, s.Range.Start, s.Range.Start+s.Range.Length-1, s.Holder.Kind(), holder)
	}
	view := procs.LockView{Ranges: states}
	if procs.ProcLocksSupported {
		view.Proc = l.procLocks(file, path, ranges, step)
		view.ProcListed = true
	}
	viewSeq++
	tables.lockViews.Row(viewSeq, part, step, file, view.ProcListed, len(view.Proc))
	return view
}

func (l *recorder) procLocks(file, path string, ranges []procs.LockRange, step int) []procs.ProcLock {
	inode, err := procs.FileInode(path)
	if err != nil {
		l.Fatal("inode", err)
	}
	all, err := procs.ReadProcLocks()
	if err != nil {
		l.Fatal(procs.ProcLocksPath, err)
	}
	names := map[int]string{}
	pids := map[int]bool{}
	for _, child := range l.Script.Children() {
		names[child.Pid] = child.Name
		pids[child.Pid] = true
	}
	mine := procs.FilterProcLocks(all, inode, pids)
	sorted := slices.SortedFunc(slices.Values(mine), func(a, b procs.ProcLock) int {
		return cmp.Or(cmp.Compare(names[a.Pid], names[b.Pid]), cmp.Compare(a.Type, b.Type), cmp.Compare(a.Start, b.Start))
	})
	for _, lock := range sorted {
		tables.procLocks.Row(part, step, file, names[lock.Pid], lock.Class, lock.Type, lock.RangeText(), rangeLabel(lock, ranges))
	}
	return mine
}

// rangeLabel names the SQLite lock range a kernel lock covers exactly, or
// "(A+B)" when the kernel merged adjacent ranges into one lock.
func rangeLabel(lock procs.ProcLock, ranges []procs.LockRange) string {
	var covered []string
	for _, r := range ranges {
		if lock.Start == r.Start && !lock.ToEOF && lock.Length() == r.Length {
			return r.Name
		}
		if r.Start <= lock.End && lock.Start <= r.Start+r.Length-1 {
			covered = append(covered, r.Name)
		}
	}
	if len(covered) == 0 {
		return "(no named range)"
	}
	return "(" + strings.Join(covered, "+") + ")"
}

func prepare(ctx context.Context, lab *recorder, dir, journal string) (string, *procs.Child, *procs.Child) {
	return prepareIn(ctx, lab, filepath.Join(dir, journal), procs.SQLOptions{JournalMode: journal})
}

func prepareIn(ctx context.Context, lab *recorder, partDir string, options procs.SQLOptions) (string, *procs.Child, *procs.Child) {
	journal := options.JournalMode
	db := filepath.Join(partDir, "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, journal, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)"); err != nil {
		lab.Fatal("create", err)
	}
	options.Path = db
	holder := lab.SpawnSQL("holder", options)
	contender := lab.SpawnSQL("contender", options)
	tables.infoProcesses.Row(part, holder.Name, holder.Pid)
	tables.infoProcesses.Row(part, contender.Name, contender.Pid)
	return db, holder, contender
}

func kernelHolders(lab *recorder, view procs.LockView, rangeName string, want map[int]string, claim string) {
	if !view.ProcListed {
		return
	}
	lab.Check(maps.Equal(view.ProcHolders(rangeName), want), claim)
}

func kernelNone(lab *recorder, view procs.LockView, claim string) {
	if !view.ProcListed {
		return
	}
	lab.Check(len(view.Proc) == 0, claim)
}

func finalRows(ctx context.Context, lab *recorder, db, journal string) procs.Inspection {
	lab.StopChildren()
	inspection, err := procs.InspectDatabase(ctx, db, journal, rowsQuery)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	tables.finalState.Row(part, inspection.Integrity, inspection.Values[0])
	return inspection
}

// rollbackMode runs Part A: in journal_mode=delete, a stray os.Open+Close of
// the database file drops RESERVED and SHARED, admits a second writer whose
// COMMIT returns SQLITE_OK, and causes the holder's later COMMIT to overwrite
// the page and fail with SQLITE_IOERR_DELETE_NOENT (5898, src/os_unix.c:6842-6847).
func rollbackMode(ctx context.Context, lab *recorder, dir string) {
	part = "A"
	db, holder, contender := prepare(ctx, lab, dir, sqlitex.JournalDelete)
	ranges := procs.DatabaseLockRanges()
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	before := lab.Locks("database file", db, ranges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "RESERVED", holder.Pid) && procs.ReadLocked(before.Ranges, "SHARED") && procs.Unlocked(before.Ranges, "PENDING"),
		"the holder's open write transaction is a write lock on the RESERVED byte and a read lock on the SHARED range of the database file")
	kernelHolders(lab, before, "RESERVED", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: the RESERVED byte is write-locked by the holder alone")
	kernelHolders(lab, before, "SHARED", map[int]string{holder.Pid: "READ"}, "/proc/locks: the SHARED range is read-locked by the holder alone; the contender, with no transaction, holds nothing")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused the write lock: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "db")
	after := lab.Locks("database file", db, ranges)
	lab.Check(procs.Unlocked(after.Ranges), "after the holder opened and closed the database file with os.Open, none of its locks remain")
	kernelNone(lab, after, "/proc/locks: neither process holds any lock on the database file")
	inside := lab.Do(holder, "query", "SELECT count(*) FROM lab")
	lab.Check(inside.OK() && inside.Value == "1", "the holder still sees its uncommitted row: as far as SQLite knows, its write transaction is intact")
	admitted := lab.Do(contender, "begin", "immediate")
	lab.Check(admitted.OK(), "the second process is now admitted: BEGIN IMMEDIATE returns SQLITE_OK while the holder's transaction is still open")
	lab.Must(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')")
	during := lab.Locks("database file", db, ranges)
	lab.Check(procs.WriteLockedBy(during.Ranges, "RESERVED", contender.Pid), "the RESERVED byte is now write-locked by the second process")
	kernelHolders(lab, during, "SHARED", map[int]string{contender.Pid: "READ"}, "/proc/locks: the SHARED range is read-locked by the second process only; the holder, mid-transaction, holds no lock at all")
	contenderCommit := lab.Do(contender, "commit")
	lab.Check(contenderCommit.OK(), "the second process commits: its COMMIT returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.Code == sqlitex.CodeIOErrDeleteNoEnt,
		"the holder's COMMIT writes its pages, then fails to delete the rollback journal the other process already deleted: SQLITE_IOERR_DELETE_NOENT (5898)")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalDelete)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): holder",
		"the file passes integrity_check but holds only the holder's row: the commit that returned SQLITE_OK is lost")
}

// walMode runs Part B: in WAL mode with an empty WAL, writer exclusion lives on
// -shm byte 120 (WAL_WRITE_LOCK). A stray close of the database file drops only
// the holder's SHARED lock and still refuses the contender; a stray close of
// -shm drops all -shm locks, admits the contender, and sends the holder's
// COMMIT (on WAL_READ_LOCK(0)) into walRestartLog -> walTryBeginRead retries
// until SQLITE_PROTOCOL (15, src/wal.c:2963, 3053-3056, 3261-3265, 3875-3915).
func walMode(ctx context.Context, lab *recorder, dir string) {
	part = "B"
	db, holder, contender := prepare(ctx, lab, dir, sqlitex.JournalWAL)
	shm := db + "-shm"
	shmRanges, dbRanges := procs.ShmLockRanges(), procs.DatabaseLockRanges()
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	before := lab.Locks("-shm", shm, shmRanges)
	dbBefore := lab.Locks("database file", db, dbRanges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "WAL_WRITE_LOCK", holder.Pid) && procs.ReadLocked(before.Ranges, "WAL_READ_LOCK(0)") && procs.ReadLocked(before.Ranges, "DMS"),
		"the holder's write transaction is a write lock on -shm byte 120 (WAL_WRITE_LOCK), next to its read lock on byte 123 (WAL_READ_LOCK(0))")
	lab.Check(procs.ReadLocked(dbBefore.Ranges, "SHARED") && procs.Unlocked(dbBefore.Ranges, "PENDING", "RESERVED"),
		"on the database file the SHARED range is read-locked and PENDING and RESERVED are free: WAL mode does not use RESERVED for writers")
	kernelHolders(lab, before, "WAL_WRITE_LOCK", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: -shm byte 120 is write-locked by the holder alone")
	kernelHolders(lab, before, "DMS", map[int]string{holder.Pid: "READ", contender.Pid: "READ"}, "/proc/locks: both processes read-lock the DMS byte 128 of -shm")
	kernelHolders(lab, dbBefore, "SHARED", map[int]string{holder.Pid: "READ", contender.Pid: "READ"}, "/proc/locks: both processes read-lock the SHARED range of the database file")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "db")
	afterDB := lab.Locks("-shm", shm, shmRanges)
	dbAfter := lab.Locks("database file", db, dbRanges)
	lab.Check(procs.WriteLockedBy(afterDB.Ranges, "WAL_WRITE_LOCK", holder.Pid), "a stray close of the database file leaves the -shm write lock in place")
	kernelHolders(lab, dbAfter, "SHARED", map[int]string{contender.Pid: "READ"}, "/proc/locks: on the database file only the contender's SHARED read lock is left")
	kernelHolders(lab, afterDB, "WAL_WRITE_LOCK", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: -shm byte 120 is still write-locked by the holder")
	stillRefused := lab.Do(contender, "begin", "immediate")
	lab.Check(stillRefused.Code == sqlitex.CodeBusy, "so the second process is still refused")
	lab.Must(holder, "stray-close", "-shm")
	afterShm := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.Unlocked(afterShm.Ranges, "WAL_WRITE_LOCK", "WAL_READ_LOCK(0)"), "a stray close of the -shm file releases the holder's -shm locks")
	kernelHolders(lab, afterShm, "DMS", map[int]string{contender.Pid: "READ"}, "/proc/locks: the holder has no -shm lock left; only the contender's DMS read lock remains")
	if afterShm.ProcListed {
		lab.Check(len(afterShm.Proc) == 1, "/proc/locks: that DMS read lock is the only lock on -shm")
	}
	inside := lab.Do(holder, "query", "SELECT count(*) FROM lab")
	lab.Check(inside.OK() && inside.Value == "1", "the holder still sees its uncommitted row")
	admitted := lab.Do(contender, "begin", "immediate")
	lab.Check(admitted.OK(), "the second process is now admitted while the holder's transaction is still open")
	lab.Must(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')")
	contenderCommit := lab.Do(contender, "commit")
	lab.Check(contenderCommit.OK(), "the second process commits: its COMMIT returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.Code == sqlitex.CodeProtocol,
		"the holder's COMMIT finds the wal-index header changed under its write lock, retries, and gives up with SQLITE_PROTOCOL (15)")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): contender",
		"the file passes integrity_check and holds only the second process's row: the holder's transaction is lost, and the holder was told")
}

// walWithFrames runs Part C: when the WAL already holds an uncheckpointed commit,
// the holder starts on WAL_READ_LOCK(1) (byte 124). Because walRestartLog only
// runs when pWal->readLock == 0 (src/wal.c:3879), the holder skips the header
// check after the stray close of -shm, writes its frames over the contender's,
// and both COMMITs return SQLITE_OK while the contender's row silently vanishes.
func walWithFrames(ctx context.Context, lab *recorder, dir string) {
	part = "C"
	db, holder, contender := prepareIn(ctx, lab, filepath.Join(dir, "wal-with-frames"), procs.SQLOptions{JournalMode: sqlitex.JournalWAL})
	shm := db + "-shm"
	shmRanges := procs.ShmLockRanges()
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'seed')")
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (2, 'holder')")
	before := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "WAL_WRITE_LOCK", holder.Pid) && procs.ReadLocked(before.Ranges, "WAL_READ_LOCK(1)") && procs.Unlocked(before.Ranges, "WAL_READ_LOCK(0)"),
		"with frames in the WAL the holder reads through WAL_READ_LOCK(1), not WAL_READ_LOCK(0)")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "-shm")
	after := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.Unlocked(after.Ranges, "WAL_WRITE_LOCK", "WAL_READ_LOCK(1)"), "the stray close of -shm releases the holder's write lock and read lock")
	admitted := []procs.Result{lab.Do(contender, "begin", "immediate"), lab.Do(contender, "exec", "INSERT INTO lab VALUES (3, 'contender')"), lab.Do(contender, "commit")}
	lab.Check(allOK(admitted), "the second process is admitted, writes and commits: every statement returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.OK(),
		"the holder's COMMIT also returns SQLITE_OK: it is not on WAL_READ_LOCK(0), so it never re-reads the wal-index header and writes its frames over the other commit's")
	seen := lab.Do(contender, "query", rowsQuery)
	lab.Check(seen.OK() && seen.Value == "2 row(s): seed,holder",
		"the second process, still open, no longer sees the row it committed")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "2 row(s): seed,holder",
		"the file passes integrity_check and holds the seed and the holder's row: a commit that returned SQLITE_OK is lost and neither process was told")
}

// walContenderExits runs Part D: after stray closes of both db and -shm, the
// contender commits and closes before the holder commits. Seeing no other locks,
// sqlite3WalClose (src/wal.c:2507-2571) closes as the last connection and unlinks
// -wal and -shm; the holder then commits SQLITE_OK into its unlinked -wal fd and
// only the holder's row survives.
func walContenderExits(ctx context.Context, lab *recorder, dir string) {
	part = "D"
	db, holder, contender := prepareIn(ctx, lab, filepath.Join(dir, "wal-contender-exits"), procs.SQLOptions{JournalMode: sqlitex.JournalWAL})
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	lab.Must(holder, "stray-close", "db")
	lab.Must(holder, "stray-close", "-shm")
	admitted := []procs.Result{lab.Do(contender, "begin", "immediate"), lab.Do(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')"), lab.Do(contender, "commit")}
	lab.Check(allOK(admitted), "the second process is admitted, writes and commits: every statement returns SQLITE_OK")
	if err := contender.Stop(); err != nil {
		lab.Fatal("stop contender", err)
	}
	tables.files.Row(1, part, "after the contender exited", filepath.Base(db)+"-wal", presence(db+"-wal"))
	tables.files.Row(2, part, "after the contender exited", filepath.Base(db)+"-shm", presence(db+"-shm"))
	lab.Check(presence(db+"-wal") == "gone" && presence(db+"-shm") == "gone",
		"seeing no other process's locks, the second process closed as the last connection and deleted -wal and -shm")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.OK(), "the holder's COMMIT returns SQLITE_OK")
	tables.files.Row(3, part, "after the holder's commit", filepath.Base(db)+"-wal", presence(db+"-wal"))
	lab.Check(presence(db+"-wal") == "gone", "no -wal file exists under that name: the holder wrote its commit into the deleted file it still had open")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): holder",
		"the file passes integrity_check and holds only the holder's row: the second process's commit is lost and neither process was told")
}

func presence(path string) string {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return "present"
	case errors.Is(err, fs.ErrNotExist):
		return "gone"
	default:
		return "unreadable: " + err.Error()
	}
}

func allOK(results []procs.Result) bool {
	for _, r := range results {
		if !r.OK() {
			return false
		}
	}
	return true
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.1")
	fmt.Printf("INFO: work dir %s, GOOS %s\n", sqlitex.DisplayPath(dir), runtime.GOOS)
	if !procs.ProcLocksSupported {
		trace.PrintLinuxOnly("the /proc/locks listing (the fcntl F_GETLK probe and every behavioural check still run)", packagePath)
	}
	set = results.MustOpen(dir)
	openTables()
	tables.infoRun.Row(sqlitex.DisplayPath(dir), runtime.GOOS)
	lab := newRecorder()
	rollbackMode(ctx, lab, dir)
	walMode(ctx, lab, dir)
	walWithFrames(ctx, lab, dir)
	walContenderExits(ctx, lab, dir)
	set.Finish("LOCKS BELONG TO THE PROCESS")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/6.1-locks-belong-to-the-process/report.sql -->
```sql
-- Walkthrough of experiment 6.1. Run it with:
--   go run ./cmd/course run 6.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps          one statement or action one child process ran, numbered across all parts
--   lock_views     one look at the locks on one file, taken after a step
--   lock_probes    one named lock range in one look, probed by the parent with fcntl F_GETLK
--   proc_locks     one /proc/locks entry of a child for that file in one look (Linux only)
--   final_state    one part: the file after both processes closed
--   files          part D: whether -wal and -shm exist at a moment
--   checks         every claim the experiment asserted (the runner lists it last)
-- info_run and info_processes (work dir, GOOS, pids) are shown by info.sql.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  6.1  Locks belong to the process'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Two child processes, holder and contender, open the same database. The'
.print 'holder starts a write transaction, then opens and closes the database file'
.print 'or -shm with a plain os.Open + Close (a "stray close"). POSIX locks belong'
.print 'to the process and the inode, so that close drops every lock the holder'
.print 'has on that file, while SQLite still believes it holds them.'
.print
.print 'The parent holds no connection. It probes each lock range with fcntl F_GETLK'
.print 'and, on Linux, reads /proc/locks. F_GETLK names a single holder only for a'
.print 'write lock; /proc/locks lists every holder. Where /proc/locks does not'
.print 'exist (not Linux), the experiment prints LINUX-ONLY, skips that listing and'
.print 'its claims, and with_proc_locks below is 0.'

SELECT count(*) AS looks, count(*) FILTER (proc_locks_listed) AS with_proc_locks FROM lock_views ORDER BY looks;

.print
.print 'How each part ended: what COMMIT told each process, and what the file holds.'

SELECT f.part,
       x.scenario,
       (SELECT result FROM steps s WHERE s.part = f.part AND process = 'holder' AND action = 'commit')    AS holder_commit,
       (SELECT result FROM steps s WHERE s.part = f.part AND process = 'contender' AND action = 'commit') AS contender_commit,
       f.integrity,
       f.contents
FROM final_state f
JOIN (VALUES ('A', 'rollback mode'),
             ('B', 'WAL, empty WAL'),
             ('C', 'WAL holding a commit'),
             ('D', 'WAL, contender exits first')) x(part, scenario) USING (part)
ORDER BY f.part;

.print
.print 'In the transcripts below, ROWS_QUERY stands for'
.print "  SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '')"
.print '  FROM (SELECT who FROM lab ORDER BY id)'

.print
.print '── A. Rollback mode (journal_mode=delete) ──────────────────────────────'
.print
.print 'The write lock is a byte-range lock on the database file.'
.print
.print 'A1. The transcript. Step 4 is the stray close; after it the contender is'
.print "    admitted (step 6) and commits (step 8), and the holder's COMMIT fails."

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'A' ORDER BY step;

.print
.print 'A2. The database-file locks seen by F_GETLK after steps 2, 4 and 7. After'
.print "    the stray close (step 4) nothing is locked; then RESERVED is the contender's."

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'A' AND file = 'database file')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'A3. The same looks in /proc/locks, which names every holder. After step 4'
.print '    neither process holds a lock; after step 7 only the contender does.'

SELECT v.after_step, coalesce(p.process, '(none)') AS process, p.type, p.bytes, p.lock
FROM lock_views v LEFT JOIN proc_locks p USING (part, after_step, file)
WHERE v.part = 'A' AND v.proc_locks_listed
ORDER BY v.seq, p.process, p.type;

.print
.print '── B. WAL mode, empty WAL ──────────────────────────────────────────────'
.print
.print 'The write lock is byte 120 of -shm (WAL_WRITE_LOCK). A stray close of the'
.print 'database file (step 13) leaves it in place; a stray close of -shm (step 15)'
.print "drops it. The holder's COMMIT then sees the wal-index header changed and"
.print 'gives up with SQLITE_PROTOCOL: here the holder is told.'
.print
.print 'B1. The transcript.'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'B' ORDER BY step;

.print
.print "B2. -shm locks seen by F_GETLK. Byte 120 is the holder's write lock until"
.print '    the stray close of -shm; byte 128 (DMS) stays read-locked by the contender.'

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'B' AND file = '-shm')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'B3. Database-file locks seen by F_GETLK. RESERVED stays free: WAL mode does'
.print '    not use it for writers.'

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'B' AND file = 'database file')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'B4. /proc/locks for both files. After the stray close of the database file'
.print "    (step 13) the holder's SHARED lock is gone; after the stray close of -shm"
.print "    (step 15) only the contender's DMS read lock is left on -shm."

SELECT v.after_step, v.file, coalesce(p.process, '(none)') AS process, p.type, p.bytes, p.lock
FROM lock_views v LEFT JOIN proc_locks p USING (part, after_step, file)
WHERE v.part = 'B' AND v.proc_locks_listed
ORDER BY v.seq, p.process, p.type, p.bytes;

.print
.print '── C. WAL mode, the WAL already holding a commit ───────────────────────'
.print
.print 'A seed row is committed first and no checkpoint copies it (wal_autocheckpoint=0),'
.print 'so the holder reads through WAL_READ_LOCK(1), byte 124. Only -shm is closed.'
.print "Both COMMITs return SQLITE_OK, and the contender's row is gone (step 30)."
.print
.print 'C1. The transcript.'

SELECT step, process,
       CASE WHEN action LIKE 'query SELECT count(*) || %' THEN 'query ROWS_QUERY' ELSE action END AS action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C' ORDER BY step;

.print
.print 'C2. -shm locks seen by F_GETLK, before and after the stray close of -shm.'

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'C' AND file = '-shm')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'C3. The same looks in /proc/locks.'

SELECT v.after_step, coalesce(p.process, '(none)') AS process, p.type, p.bytes, p.lock
FROM lock_views v LEFT JOIN proc_locks p USING (part, after_step, file)
WHERE v.part = 'C' AND v.proc_locks_listed
ORDER BY v.seq, p.process, p.type, p.bytes;

.print
.print '── D. WAL mode, the contender exits before the holder commits ──────────'
.print
.print 'As in B, the holder closes the database file and -shm. The contender commits,'
.print 'closes its connection and exits. Seeing no other locks, it closes as the last'
.print 'connection and deletes -wal and -shm. The holder then commits into the -wal'
.print 'it still has open, a file that no longer has a name.'
.print
.print 'D1. The transcript.'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'D' ORDER BY step;

.print
.print 'D2. Whether -wal and -shm exist, after the contender exited and after the'
.print "    holder's commit."

SELECT moment, file, state FROM files WHERE part = 'D' ORDER BY seq;
```

`course run` also prints the run-dependent values, which are not part of the stored evidence: the work directory, `GOOS`, and the process ids of the two children in each part.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/6.1-locks-belong-to-the-process/`](evidence/6.1-locks-belong-to-the-process/), and `go run ./cmd/course check evidence ch06` compares a fresh run with them byte for byte.

<!-- include: evidence/6.1-locks-belong-to-the-process.txt -->
```text

════════════════════════════════════════════════════════════════════════
  6.1  Locks belong to the process
════════════════════════════════════════════════════════════════════════

Two child processes, holder and contender, open the same database. The
holder starts a write transaction, then opens and closes the database file
or -shm with a plain os.Open + Close (a "stray close"). POSIX locks belong
to the process and the inode, so that close drops every lock the holder
has on that file, while SQLite still believes it holds them.

The parent holds no connection. It probes each lock range with fcntl F_GETLK
and, on Linux, reads /proc/locks. F_GETLK names a single holder only for a
write lock; /proc/locks lists every holder. Where /proc/locks does not
exist (not Linux), the experiment prints LINUX-ONLY, skips that listing and
its claims, and with_proc_locks below is 0.
┌───────┬─────────────────┐
│ looks │ with_proc_locks │
├───────┼─────────────────┤
│ 10    │ 10              │
└───────┴─────────────────┘

How each part ended: what COMMIT told each process, and what the file holds.
┌──────┬────────────────────────────┬───────────────────────────┬──────────────────┬───────────┬───────────────────────┐
│ part │          scenario          │       holder_commit       │ contender_commit │ integrity │       contents        │
├──────┼────────────────────────────┼───────────────────────────┼──────────────────┼───────────┼───────────────────────┤
│ A    │ rollback mode              │ SQLITE_IOERR_DELETE_NOENT │ SQLITE_OK        │ ok        │ 1 row(s): holder      │
│ B    │ WAL, empty WAL             │ SQLITE_PROTOCOL           │ SQLITE_OK        │ ok        │ 1 row(s): contender   │
│ C    │ WAL holding a commit       │ SQLITE_OK                 │ SQLITE_OK        │ ok        │ 2 row(s): seed,holder │
│ D    │ WAL, contender exits first │ SQLITE_OK                 │ SQLITE_OK        │ ok        │ 1 row(s): holder      │
└──────┴────────────────────────────┴───────────────────────────┴──────────────────┴───────────┴───────────────────────┘

In the transcripts below, ROWS_QUERY stands for
  SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '')
  FROM (SELECT who FROM lab ORDER BY id)

── A. Rollback mode (journal_mode=delete) ──────────────────────────────

The write lock is a byte-range lock on the database file.

A1. The transcript. Step 4 is the stray close; after it the contender is
    admitted (step 6) and commits (step 8), and the holder's COMMIT fails.
┌──────┬───────────┬──────────────────────────────────────────────┬──────────────────────────────────┬───────┐
│ step │  process  │                    action                    │              result              │ value │
├──────┼───────────┼──────────────────────────────────────────────┼──────────────────────────────────┼───────┤
│ 1    │ holder    │ begin immediate                              │ SQLITE_OK                        │       │
│ 2    │ holder    │ exec INSERT INTO lab VALUES (1, 'holder')    │ SQLITE_OK                        │       │
│ 3    │ contender │ begin immediate                              │ SQLITE_BUSY (5)                  │       │
│ 4    │ holder    │ stray-close db                               │ SQLITE_OK                        │       │
│ 5    │ holder    │ query SELECT count(*) FROM lab               │ SQLITE_OK                        │ 1     │
│ 6    │ contender │ begin immediate                              │ SQLITE_OK                        │       │
│ 7    │ contender │ exec INSERT INTO lab VALUES (2, 'contender') │ SQLITE_OK                        │       │
│ 8    │ contender │ commit                                       │ SQLITE_OK                        │       │
│ 9    │ holder    │ commit                                       │ SQLITE_IOERR_DELETE_NOENT (5898) │       │
└──────┴───────────┴──────────────────────────────────────────────┴──────────────────────────────────┴───────┘

A2. The database-file locks seen by F_GETLK after steps 2, 4 and 7. After
    the stray close (step 4) nothing is locked; then RESERVED is the contender's.
┌──────────┬─────────────────────────────┬───────────────┬──────────────┬──────────────────┐
│   lock   │            bytes            │ after step 2  │ after step 4 │   after step 7   │
├──────────┼─────────────────────────────┼───────────────┼──────────────┼──────────────────┤
│ PENDING  │ byte 1073741824             │ -             │ -            │ -                │
│ RESERVED │ byte 1073741825             │ write: holder │ -            │ write: contender │
│ SHARED   │ bytes 1073741826-1073742335 │ read          │ -            │ read             │
└──────────┴─────────────────────────────┴───────────────┴──────────────┴──────────────────┘

A3. The same looks in /proc/locks, which names every holder. After step 4
    neither process holds a lock; after step 7 only the contender does.
┌────────────┬───────────┬───────┬───────────────────────┬──────────┐
│ after_step │  process  │ type  │         bytes         │   lock   │
├────────────┼───────────┼───────┼───────────────────────┼──────────┤
│ 2          │ holder    │ READ  │ 1073741826-1073742335 │ SHARED   │
│ 2          │ holder    │ WRITE │ 1073741825            │ RESERVED │
│ 4          │ (none)    │       │                       │          │
│ 7          │ contender │ READ  │ 1073741826-1073742335 │ SHARED   │
│ 7          │ contender │ WRITE │ 1073741825            │ RESERVED │
└────────────┴───────────┴───────┴───────────────────────┴──────────┘

── B. WAL mode, empty WAL ──────────────────────────────────────────────

The write lock is byte 120 of -shm (WAL_WRITE_LOCK). A stray close of the
database file (step 13) leaves it in place; a stray close of -shm (step 15)
drops it. The holder's COMMIT then sees the wal-index header changed and
gives up with SQLITE_PROTOCOL: here the holder is told.

B1. The transcript.
┌──────┬───────────┬──────────────────────────────────────────────┬──────────────────────┬───────┐
│ step │  process  │                    action                    │        result        │ value │
├──────┼───────────┼──────────────────────────────────────────────┼──────────────────────┼───────┤
│ 10   │ holder    │ begin immediate                              │ SQLITE_OK            │       │
│ 11   │ holder    │ exec INSERT INTO lab VALUES (1, 'holder')    │ SQLITE_OK            │       │
│ 12   │ contender │ begin immediate                              │ SQLITE_BUSY (5)      │       │
│ 13   │ holder    │ stray-close db                               │ SQLITE_OK            │       │
│ 14   │ contender │ begin immediate                              │ SQLITE_BUSY (5)      │       │
│ 15   │ holder    │ stray-close -shm                             │ SQLITE_OK            │       │
│ 16   │ holder    │ query SELECT count(*) FROM lab               │ SQLITE_OK            │ 1     │
│ 17   │ contender │ begin immediate                              │ SQLITE_OK            │       │
│ 18   │ contender │ exec INSERT INTO lab VALUES (2, 'contender') │ SQLITE_OK            │       │
│ 19   │ contender │ commit                                       │ SQLITE_OK            │       │
│ 20   │ holder    │ commit                                       │ SQLITE_PROTOCOL (15) │       │
└──────┴───────────┴──────────────────────────────────────────────┴──────────────────────┴───────┘

B2. -shm locks seen by F_GETLK. Byte 120 is the holder's write lock until
    the stray close of -shm; byte 128 (DMS) stays read-locked by the contender.
┌──────────────────┬──────────┬───────────────┬───────────────┬───────────────┐
│       lock       │  bytes   │ after step 11 │ after step 13 │ after step 15 │
├──────────────────┼──────────┼───────────────┼───────────────┼───────────────┤
│ WAL_WRITE_LOCK   │ byte 120 │ write: holder │ write: holder │ -             │
│ WAL_CKPT_LOCK    │ byte 121 │ -             │ -             │ -             │
│ WAL_RECOVER_LOCK │ byte 122 │ -             │ -             │ -             │
│ WAL_READ_LOCK(0) │ byte 123 │ read          │ read          │ -             │
│ WAL_READ_LOCK(1) │ byte 124 │ -             │ -             │ -             │
│ WAL_READ_LOCK(2) │ byte 125 │ -             │ -             │ -             │
│ WAL_READ_LOCK(3) │ byte 126 │ -             │ -             │ -             │
│ WAL_READ_LOCK(4) │ byte 127 │ -             │ -             │ -             │
│ DMS              │ byte 128 │ read          │ read          │ read          │
└──────────────────┴──────────┴───────────────┴───────────────┴───────────────┘

B3. Database-file locks seen by F_GETLK. RESERVED stays free: WAL mode does
    not use it for writers.
┌──────────┬─────────────────────────────┬───────────────┬───────────────┐
│   lock   │            bytes            │ after step 11 │ after step 13 │
├──────────┼─────────────────────────────┼───────────────┼───────────────┤
│ PENDING  │ byte 1073741824             │ -             │ -             │
│ RESERVED │ byte 1073741825             │ -             │ -             │
│ SHARED   │ bytes 1073741826-1073742335 │ read          │ read          │
└──────────┴─────────────────────────────┴───────────────┴───────────────┘

B4. /proc/locks for both files. After the stray close of the database file
    (step 13) the holder's SHARED lock is gone; after the stray close of -shm
    (step 15) only the contender's DMS read lock is left on -shm.
┌────────────┬───────────────┬───────────┬───────┬───────────────────────┬──────────────────┐
│ after_step │     file      │  process  │ type  │         bytes         │       lock       │
├────────────┼───────────────┼───────────┼───────┼───────────────────────┼──────────────────┤
│ 11         │ -shm          │ contender │ READ  │ 128                   │ DMS              │
│ 11         │ -shm          │ holder    │ READ  │ 123                   │ WAL_READ_LOCK(0) │
│ 11         │ -shm          │ holder    │ READ  │ 128                   │ DMS              │
│ 11         │ -shm          │ holder    │ WRITE │ 120                   │ WAL_WRITE_LOCK   │
│ 11         │ database file │ contender │ READ  │ 1073741826-1073742335 │ SHARED           │
│ 11         │ database file │ holder    │ READ  │ 1073741826-1073742335 │ SHARED           │
│ 13         │ -shm          │ contender │ READ  │ 128                   │ DMS              │
│ 13         │ -shm          │ holder    │ READ  │ 123                   │ WAL_READ_LOCK(0) │
│ 13         │ -shm          │ holder    │ READ  │ 128                   │ DMS              │
│ 13         │ -shm          │ holder    │ WRITE │ 120                   │ WAL_WRITE_LOCK   │
│ 13         │ database file │ contender │ READ  │ 1073741826-1073742335 │ SHARED           │
│ 15         │ -shm          │ contender │ READ  │ 128                   │ DMS              │
└────────────┴───────────────┴───────────┴───────┴───────────────────────┴──────────────────┘

── C. WAL mode, the WAL already holding a commit ───────────────────────

A seed row is committed first and no checkpoint copies it (wal_autocheckpoint=0),
so the holder reads through WAL_READ_LOCK(1), byte 124. Only -shm is closed.
Both COMMITs return SQLITE_OK, and the contender's row is gone (step 30).

C1. The transcript.
┌──────┬───────────┬──────────────────────────────────────────────┬─────────────────┬───────────────────────┐
│ step │  process  │                    action                    │     result      │         value         │
├──────┼───────────┼──────────────────────────────────────────────┼─────────────────┼───────────────────────┤
│ 21   │ holder    │ exec INSERT INTO lab VALUES (1, 'seed')      │ SQLITE_OK       │                       │
│ 22   │ holder    │ begin immediate                              │ SQLITE_OK       │                       │
│ 23   │ holder    │ exec INSERT INTO lab VALUES (2, 'holder')    │ SQLITE_OK       │                       │
│ 24   │ contender │ begin immediate                              │ SQLITE_BUSY (5) │                       │
│ 25   │ holder    │ stray-close -shm                             │ SQLITE_OK       │                       │
│ 26   │ contender │ begin immediate                              │ SQLITE_OK       │                       │
│ 27   │ contender │ exec INSERT INTO lab VALUES (3, 'contender') │ SQLITE_OK       │                       │
│ 28   │ contender │ commit                                       │ SQLITE_OK       │                       │
│ 29   │ holder    │ commit                                       │ SQLITE_OK       │                       │
│ 30   │ contender │ query ROWS_QUERY                             │ SQLITE_OK       │ 2 row(s): seed,holder │
└──────┴───────────┴──────────────────────────────────────────────┴─────────────────┴───────────────────────┘

C2. -shm locks seen by F_GETLK, before and after the stray close of -shm.
┌──────────────────┬──────────┬───────────────┬───────────────┐
│       lock       │  bytes   │ after step 23 │ after step 25 │
├──────────────────┼──────────┼───────────────┼───────────────┤
│ WAL_WRITE_LOCK   │ byte 120 │ write: holder │ -             │
│ WAL_CKPT_LOCK    │ byte 121 │ -             │ -             │
│ WAL_RECOVER_LOCK │ byte 122 │ -             │ -             │
│ WAL_READ_LOCK(0) │ byte 123 │ -             │ -             │
│ WAL_READ_LOCK(1) │ byte 124 │ read          │ -             │
│ WAL_READ_LOCK(2) │ byte 125 │ -             │ -             │
│ WAL_READ_LOCK(3) │ byte 126 │ -             │ -             │
│ WAL_READ_LOCK(4) │ byte 127 │ -             │ -             │
│ DMS              │ byte 128 │ read          │ read          │
└──────────────────┴──────────┴───────────────┴───────────────┘

C3. The same looks in /proc/locks.
┌────────────┬───────────┬───────┬───────┬──────────────────┐
│ after_step │  process  │ type  │ bytes │       lock       │
├────────────┼───────────┼───────┼───────┼──────────────────┤
│ 23         │ contender │ READ  │ 128   │ DMS              │
│ 23         │ holder    │ READ  │ 124   │ WAL_READ_LOCK(1) │
│ 23         │ holder    │ READ  │ 128   │ DMS              │
│ 23         │ holder    │ WRITE │ 120   │ WAL_WRITE_LOCK   │
│ 25         │ contender │ READ  │ 128   │ DMS              │
└────────────┴───────────┴───────┴───────┴──────────────────┘

── D. WAL mode, the contender exits before the holder commits ──────────

As in B, the holder closes the database file and -shm. The contender commits,
closes its connection and exits. Seeing no other locks, it closes as the last
connection and deletes -wal and -shm. The holder then commits into the -wal
it still has open, a file that no longer has a name.

D1. The transcript.
┌──────┬───────────┬──────────────────────────────────────────────┬───────────┬───────┐
│ step │  process  │                    action                    │  result   │ value │
├──────┼───────────┼──────────────────────────────────────────────┼───────────┼───────┤
│ 31   │ holder    │ begin immediate                              │ SQLITE_OK │       │
│ 32   │ holder    │ exec INSERT INTO lab VALUES (1, 'holder')    │ SQLITE_OK │       │
│ 33   │ holder    │ stray-close db                               │ SQLITE_OK │       │
│ 34   │ holder    │ stray-close -shm                             │ SQLITE_OK │       │
│ 35   │ contender │ begin immediate                              │ SQLITE_OK │       │
│ 36   │ contender │ exec INSERT INTO lab VALUES (2, 'contender') │ SQLITE_OK │       │
│ 37   │ contender │ commit                                       │ SQLITE_OK │       │
│ 38   │ holder    │ commit                                       │ SQLITE_OK │       │
└──────┴───────────┴──────────────────────────────────────────────┴───────────┴───────┘

D2. Whether -wal and -shm exist, after the contender exited and after the
    holder's commit.
┌────────────────────────────┬────────────┬───────┐
│           moment           │    file    │ state │
├────────────────────────────┼────────────┼───────┤
│ after the contender exited │ lab.db-wal │ gone  │
│ after the contender exited │ lab.db-shm │ gone  │
│ after the holder's commit  │ lab.db-wal │ gone  │
└────────────────────────────┴────────────┴───────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the holder's open write transaction is a write lock on the RESERVED byte and a read lock on the SHARED range of the database file
A  ✓  /proc/locks: the RESERVED byte is write-locked by the holder alone
A  ✓  /proc/locks: the SHARED range is read-locked by the holder alone; the contender, with no transaction, holds nothing
A  ✓  a second process is refused the write lock: BEGIN IMMEDIATE returns SQLITE_BUSY (5)
A  ✓  after the holder opened and closed the database file with os.Open, none of its locks remain
A  ✓  /proc/locks: neither process holds any lock on the database file
A  ✓  the holder still sees its uncommitted row: as far as SQLite knows, its write transaction is intact
A  ✓  the second process is now admitted: BEGIN IMMEDIATE returns SQLITE_OK while the holder's transaction is still open
A  ✓  the RESERVED byte is now write-locked by the second process
A  ✓  /proc/locks: the SHARED range is read-locked by the second process only; the holder, mid-transaction, holds no lock at all
A  ✓  the second process commits: its COMMIT returns SQLITE_OK
A  ✓  the holder's COMMIT writes its pages, then fails to delete the rollback journal the other process already deleted: SQLITE_IOERR_DELETE_NOENT (5898)
A  ✓  the file passes integrity_check but holds only the holder's row: the commit that returned SQLITE_OK is lost
B  ✓  the holder's write transaction is a write lock on -shm byte 120 (WAL_WRITE_LOCK), next to its read lock on byte 123 (WAL_READ_LOCK(0))
B  ✓  on the database file the SHARED range is read-locked and PENDING and RESERVED are free: WAL mode does not use RESERVED for writers
B  ✓  /proc/locks: -shm byte 120 is write-locked by the holder alone
B  ✓  /proc/locks: both processes read-lock the DMS byte 128 of -shm
B  ✓  /proc/locks: both processes read-lock the SHARED range of the database file
B  ✓  a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)
B  ✓  a stray close of the database file leaves the -shm write lock in place
B  ✓  /proc/locks: on the database file only the contender's SHARED read lock is left
B  ✓  /proc/locks: -shm byte 120 is still write-locked by the holder
B  ✓  so the second process is still refused
B  ✓  a stray close of the -shm file releases the holder's -shm locks
B  ✓  /proc/locks: the holder has no -shm lock left; only the contender's DMS read lock remains
B  ✓  /proc/locks: that DMS read lock is the only lock on -shm
B  ✓  the holder still sees its uncommitted row
B  ✓  the second process is now admitted while the holder's transaction is still open
B  ✓  the second process commits: its COMMIT returns SQLITE_OK
B  ✓  the holder's COMMIT finds the wal-index header changed under its write lock, retries, and gives up with SQLITE_PROTOCOL (15)
B  ✓  the file passes integrity_check and holds only the second process's row: the holder's transaction is lost, and the holder was told
C  ✓  with frames in the WAL the holder reads through WAL_READ_LOCK(1), not WAL_READ_LOCK(0)
C  ✓  a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)
C  ✓  the stray close of -shm releases the holder's write lock and read lock
C  ✓  the second process is admitted, writes and commits: every statement returns SQLITE_OK
C  ✓  the holder's COMMIT also returns SQLITE_OK: it is not on WAL_READ_LOCK(0), so it never re-reads the wal-index header and writes its frames over the other commit's
C  ✓  the second process, still open, no longer sees the row it committed
C  ✓  the file passes integrity_check and holds the seed and the holder's row: a commit that returned SQLITE_OK is lost and neither process was told
D  ✓  the second process is admitted, writes and commits: every statement returns SQLITE_OK
D  ✓  seeing no other process's locks, the second process closed as the last connection and deleted -wal and -shm
D  ✓  the holder's COMMIT returns SQLITE_OK
D  ✓  no -wal file exists under that name: the holder wrote its commit into the deleted file it still had open
D  ✓  the file passes integrity_check and holds only the holder's row: the second process's commit is lost and neither process was told

43 of 43 claims hold

Experiment verdict: LOCKS BELONG TO THE PROCESS OK
```

**Reading the evidence:**

- **Part A — Rollback mode (`journal_mode=delete`, steps 1–9):**
  - After `holder` runs `BEGIN IMMEDIATE` and inserts `(1, 'holder')`, both `F_GETLK` and `/proc/locks` show `holder` holding `WRITE` on `RESERVED` (`1073741825`) and `READ` on `SHARED` (`1073741826-1073742335`); `PENDING` (`1073741824`) is unlocked. `contender`'s `BEGIN IMMEDIATE` fails at step 3 with `SQLITE_BUSY (5)`.
  - At step 4, `holder` executes `stray-close db` (plain `os.Open` + `Close` on `lab.db`). Both `F_GETLK` and `/proc/locks` immediately show **zero** locks on `lab.db`, yet step 5 shows `holder` still counts `1` row — SQLite's in-memory `unixInodeInfo` has no idea the kernel dropped its locks.
  - At steps 6–8, `contender` enters `BEGIN IMMEDIATE`, inserts `(2, 'contender')`, and commits with `SQLITE_OK` (taking `RESERVED` and `SHARED` while `holder` holds nothing).
  - At step 9, `holder` commits. It overwrites the database page with its own cached page containing only `(1, 'holder')`, then tries to unlink `lab.db-journal`, which `contender` already deleted — hitting `unixDelete` (`src/os_unix.c:6847`) and returning `SQLITE_IOERR_DELETE_NOENT (5898)`.
  - After both processes close, `integrity_check` is `ok` and the database holds `1 row(s): holder`. **The commit that returned `SQLITE_OK` vanished silently, and the transaction that returned an I/O error is the one on disk.**
- **Part B — WAL mode on an empty WAL (steps 10–20):**
  - After steps 10–11, `-shm` has `holder` write-locking `WAL_WRITE_LOCK` (`120`) and read-locking `WAL_READ_LOCK(0)` (`123`), while **both** processes read-lock `DMS` (`128`) and `.db`'s `SHARED` range (`1073741826-1073742335`). `PENDING` and `RESERVED` on `.db` stay unlocked. Notice why `/proc/locks` is essential alongside `F_GETLK`: `F_GETLK` reports only `"read-locked"` on `SHARED` both before and after step 13, whereas `/proc/locks` proves both processes held `SHARED` before step 13 and only `contender` holds it after.
  - Step 13 (`holder stray-close db`) strips `holder`'s `.db` `SHARED` lock, yet `-shm` byte `120` stays write-locked by `holder`, so step 14 still refuses `contender` with `SQLITE_BUSY (5)`. POSIX lock release is strictly **per inode**.
  - Step 15 (`holder stray-close -shm`) strips every `-shm` lock held by `holder` (`120`, `123`, `128`), leaving only `contender`'s `DMS` lock. Steps 17–19 admit `contender` and commit `SQLITE_OK`.
  - At step 20, `holder`'s `COMMIT` enters `walRestartLog` because `holder` held `WAL_READ_LOCK(0)` (`src/wal.c:3879`). Its `walTryBeginRead(..., useWal=1)` sees the shared wal-index header changed by `contender`, retries `100` times (~10 s), and returns `SQLITE_PROTOCOL (15)`. The final database holds `1 row(s): contender`.
- **Part C — WAL mode when `-wal` already holds uncheckpointed frames (steps 21–30):**
  - This part corrects the initial assumption that a broken WAL lock always reports `SQLITE_PROTOCOL`. Step 21 commits `'seed'` into `-wal` (`wal_autocheckpoint=0`), so steps 22–23 place `holder` on `WAL_READ_LOCK(1)` (byte `124`), leaving `WAL_READ_LOCK(0)` (`123`) unlocked.
  - After `stray-close -shm` (step 25) and `contender`'s `SQLITE_OK` commit (steps 26–28), `holder`'s `COMMIT` at step 29 skips `walRestartLog` (`readLock == 1 != 0`, `src/wal.c:3879`), writes its frames over `contender`'s frames using its stale `mxFrame`, and **returns `SQLITE_OK` immediately**.
  - At step 30, `contender` — still open — queries the table and sees `2 row(s): seed,holder`. Its own committed row is gone, `integrity_check` is `ok`, and neither process received an error.
- **Part D — WAL mode when the second process exits first (steps 31–38):**
  - Even on an empty WAL (same setup as Part B), if `holder` drops both `db` and `-shm` locks (steps 33–34) and `contender` commits and closes before `holder` commits, `contender` sees no other locks on `.db` or `DMS` and closes as the last connection (`sqlite3WalClose`, `src/wal.c:2507-2571`), deleting `lab.db-wal` and `lab.db-shm`.
  - Step 38's `holder` `COMMIT` returns `SQLITE_OK` while `lab.db-wal` remains `gone` on disk — `holder` wrote its commit into the unlinked `-wal` file descriptor it still held open, and its own close later checkpoints that deleted WAL into `lab.db`. Final state: integrity `ok`, `1 row(s): holder`. Again, a `SQLITE_OK` commit is silently lost with zero errors reported.

---

## 4. Timeline

The two tables below lay out lock transitions across a writer and a reader (or second writer), contrasting WAL mode (default) with rollback mode.

### 4.1 WAL mode (`journal_mode=wal`, `locking_mode=NORMAL`)

Lock bytes: `-shm` `120` (`WAL_WRITE_LOCK`), `121` (`WAL_CKPT_LOCK`), `122` (`WAL_RECOVER_LOCK`), `123–127` (`WAL_READ_LOCK(0..4)`), `128` (`DMS`); `.db` `1073741826-1073742335` (`SHARED`).

| Moment | Writer / holder | Reader or second writer |
| --- | --- | --- |
| Every open WAL connection | `.db` `SHARED` (read) + `-shm` `DMS` byte `128` (read) | Same |
| Read txn on empty / fully backfilled WAL | `-shm` `WAL_READ_LOCK(0)` byte `123` (read); reads `.db` only | Blocks checkpoint backfill (`wal.c:2254-2260`), but does **not** block next writer's `walRestartLog` (`wal.c:3885`) |
| Read txn with uncheckpointed WAL frames | `-shm` `WAL_READ_LOCK(i)` byte `123+i` (read), `aReadMark[i] = mxFrame` | Caps checkpoint `mxSafeFrame` and blocks WAL restart |
| `BEGIN IMMEDIATE` (or `BEGIN EXCLUSIVE` in normal mode) | `-shm` `WAL_WRITE_LOCK` byte `120` (write) + its read lock | Readers start and commit freely; second `BEGIN IMMEDIATE` gets `SQLITE_BUSY (5)` or waits on `busy_timeout` |
| `COMMIT` | Appends frames to `-wal`, publishes `WalIndexHdr`, releases byte `120` | Earlier readers keep seeing their pinned `mxFrame` snapshot |
| Deferred reader attempts first write | — | `SQLITE_BUSY (5)` immediately while writer holds byte `120` (`btree.c:3740-3741`); `SQLITE_BUSY_SNAPSHOT (517)` immediately after writer commits (`wal.c:3734-3737`) |

### 4.2 Rollback mode contrast (`journal_mode=delete`)

Lock bytes on `.db`: `1073741824` (`PENDING`), `1073741825` (`RESERVED`), `1073741826-1073742335` (`SHARED`).

| Moment | Writer | Reader or second writer |
| --- | --- | --- |
| `BEGIN IMMEDIATE` + write | `SHARED` (read) + `RESERVED` byte `1073741825` (write); buffers dirty pages | Existing and new readers still take `SHARED`; second `BEGIN IMMEDIATE` gets `SQLITE_BUSY (5)` |
| `COMMIT` while a reader holds `SHARED` | Takes `PENDING` byte `1073741824` (write), fails `EXCLUSIVE` → returns `SQLITE_BUSY (5)`, **keeps `PENDING`** | Existing reader continues; **new readers fail with `SQLITE_BUSY (5)`** |
| Reader ends, writer retries `COMMIT` | Acquires `EXCLUSIVE` (write on `SHARED` range), writes `.db`, deletes `-journal`, unlocks all | New reads succeed after unlock and see the committed row |

### Experiment 6.2 — Busy rules (`6.2-busy-rules`)

- **Claim:** In WAL mode under default `PRAGMA locking_mode=NORMAL`, readers never block the writer, the writer never blocks readers, and `BEGIN EXCLUSIVE` behaves like `BEGIN IMMEDIATE`. A second writer receives `SQLITE_BUSY (5)` at `BEGIN` under `BEGIN IMMEDIATE` and at its first write under `BEGIN DEFERRED`. A generous `busy_timeout` turns that failure into a wait **only** for transactions that have not read yet (`BEGIN IMMEDIATE`, or `BEGIN DEFERRED` whose first statement writes); a deferred transaction that has already read skips the busy handler (`src/btree.c:3740-3741`) and fails immediately with `SQLITE_BUSY (5)` while the other writer is open and `SQLITE_BUSY_SNAPSHOT (517)` once it commits, succeeding only after `ROLLBACK` and restart. In rollback mode, an open reader blocks the writer's `COMMIT` and the writer retains `PENDING` to block new readers. With `BEGIN IMMEDIATE` and a generous `busy_timeout`, alternating writers commit every transaction under contention and match the deterministic recipe state.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 6.2
  ```

<!-- include: experiments/6.2-busy-rules/main.go -->
```go
// Command 6.2-busy-rules scripts cross-process reader/writer and writer/writer
// interleavings to verify SQLite's exact extended result codes: WAL reader and
// writer concurrency (including BEGIN EXCLUSIVE behaving like BEGIN IMMEDIATE
// under locking_mode=NORMAL), BEGIN DEFERRED vs BEGIN IMMEDIATE contention with
// busy_timeout=0 and 120s, immediate SQLITE_BUSY (5) and SQLITE_BUSY_SNAPSHOT
// (517) on a deferred read-then-write upgrade (src/btree.c:3740-3741,
// src/wal.c:3734-3737), rollback-mode COMMIT contention and retained PENDING
// locks (Part D), and alternating multi-process recipe steps under contention (Part E).
//
// It prints only its verdict. The steps and outcomes go to CSV tables that
// report.sql presents: go run ./cmd/course run 6.2
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	// longTimeout exceeds stepDeadline so any statement that waited out its busy
	// handler instead of returning at once (Part C3) would time out the child loudly.
	longTimeout   = 120 * time.Second
	stepDeadline  = 30 * time.Second
	countQuery    = "SELECT count(*) FROM lab"
	recipeBase    = 5
	recipeWrites  = 10
	insertPattern = "INSERT INTO lab(who) VALUES ('%s')"
)

// Every step and outcome goes to a CSV table in <work dir>/results/; part
// labels the rows written while a part runs.
var (
	set     *results.Set
	part    string
	stepSeq int
	tables  struct {
		steps, settings, recipeResult *results.Table
	}
)

func openTables() {
	tables.steps = set.Table("steps", "seq", "part", "step", "process", "action", "waiting_at_250ms", "result", "code", "value")
	tables.settings = set.Table("settings", "part", "journal_mode", "busy_timeout_ms")
	tables.recipeResult = set.Table("recipe_result", "part", "user_version", "matched_step", "exact")
}

// recorder is a procs.Lab that writes each step and claim to the tables
// instead of printing them.
type recorder struct {
	*procs.Lab
	sent map[*procs.Call]*sentCall
}

// sentCall is a step sent with Begin: who runs it and, once StillWaiting has
// looked, whether it was still unanswered.
type sentCall struct {
	child   *procs.Child
	waiting any
}

func newRecorder() *recorder {
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil
	return &recorder{Lab: lab, sent: map[*procs.Call]*sentCall{}}
}

func (l *recorder) Do(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, procs.ActionText(action, args), nil, l.Lab.Do(child, action, args...))
}

func (l *recorder) Must(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, procs.ActionText(action, args), nil, l.Lab.Must(child, action, args...))
}

// Begin sends a step without waiting for its answer.
func (l *recorder) Begin(child *procs.Child, action string, args ...string) *procs.Call {
	call, err := l.Script.Begin(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	l.sent[call] = &sentCall{child: child}
	return call
}

// StillWaiting reports whether call is still unanswered after procs.StillWaitingWindow.
func (l *recorder) StillWaiting(call *procs.Call) bool {
	answered, err := call.AnsweredWithin(procs.StillWaitingWindow)
	if err != nil {
		l.Fatal("poll", err)
	}
	l.sent[call].waiting = !answered
	return !answered
}

func (l *recorder) Finish(call *procs.Call) procs.Result {
	sent := l.sent[call]
	delete(l.sent, call)
	request := call.Request()
	return l.step(sent.child, procs.ActionText(request.Action, request.Args), sent.waiting, l.Lab.Finish(call))
}

func (l *recorder) step(child *procs.Child, action string, waiting any, r procs.Result) procs.Result {
	value := r.Value
	if r.Code == procs.CodeNotSQLite {
		value = r.Error
	}
	stepSeq++
	tables.steps.Row(stepSeq, part, r.Step, child.Name, action, waiting, r.CodeName(), r.Code, value)
	return r
}

func (l *recorder) Check(ok bool, claim string) { set.Check(part, ok, claim) }

func insert(who string) string { return fmt.Sprintf(insertPattern, who) }

func database(ctx context.Context, lab *recorder, dir, name, journal string) string {
	db := filepath.Join(dir, name, "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, journal, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)", insert("seed")); err != nil {
		lab.Fatal("create", err)
	}
	return db
}

func pair(ctx context.Context, lab *recorder, dir, name, journal string, timeout time.Duration, first, second string) (*procs.Child, *procs.Child) {
	db := database(ctx, lab, dir, name, journal)
	options := procs.SQLOptions{Path: db, JournalMode: journal, BusyTimeout: timeout}
	tables.settings.Row(part, journal, timeout.Milliseconds())
	return lab.SpawnSQL(first, options), lab.SpawnSQL(second, options)
}

// readersAndWriter runs Part A: with busy_timeout=0 in WAL mode, readers and
// writers never block each other, and BEGIN EXCLUSIVE behaves like BEGIN IMMEDIATE.
func readersAndWriter(ctx context.Context, lab *recorder, dir string) {
	part = "A"
	reader, writer := pair(ctx, lab, dir, "a-reader-writer", sqlitex.JournalWAL, 0, "reader", "writer")
	lab.Must(reader, "begin", "deferred")
	snapshot := lab.Must(reader, "query", countQuery)
	commits := []procs.Result{lab.Do(writer, "begin", "immediate"), lab.Do(writer, "exec", insert("writer")), lab.Do(writer, "commit")}
	lab.Check(allOK(commits), "with a read transaction open, the writer begins, writes and commits without waiting")
	again := lab.Must(reader, "query", countQuery)
	lab.Check(again.Value == snapshot.Value, "the reader still sees its snapshot ("+snapshot.Value+" rows) after the commit")
	lab.Must(reader, "commit")
	lab.Must(writer, "begin", "immediate")
	lab.Must(writer, "exec", insert("uncommitted"))
	lab.Must(reader, "begin", "deferred")
	during := lab.Do(reader, "query", countQuery)
	lab.Check(during.OK() && during.Value == "2", "with a write transaction open, a reader starts and reads the last committed state (2 rows) without waiting")
	lab.Must(writer, "commit")
	lab.Must(reader, "commit")
	lab.Must(reader, "begin", "deferred")
	lab.Must(reader, "query", countQuery)
	exclusive := []procs.Result{lab.Do(writer, "begin", "exclusive"), lab.Do(writer, "exec", insert("exclusive"))}
	readDuring := lab.Do(reader, "query", countQuery)
	exclusive = append(exclusive, lab.Do(writer, "commit"))
	lab.Check(allOK(exclusive) && readDuring.OK(), "BEGIN EXCLUSIVE in WAL mode behaves like BEGIN IMMEDIATE: it starts beside an open reader and the reader keeps reading")
	lab.Must(reader, "commit")
	lab.StopChildren()
}

// writersWithoutTimeout runs Part B: with busy_timeout=0, BEGIN DEFERRED defers
// SQLITE_BUSY (5) until the first write statement, whereas BEGIN IMMEDIATE fails
// at BEGIN itself (src/build.c:5245-5274).
func writersWithoutTimeout(ctx context.Context, lab *recorder, dir string) {
	part = "B"
	first, second := pair(ctx, lab, dir, "b-writers-no-timeout", sqlitex.JournalWAL, 0, "writer-1", "writer-2")
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	deferredBegin := lab.Do(second, "begin", "deferred")
	deferredRead := lab.Do(second, "query", countQuery)
	deferredWrite := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(deferredBegin.OK() && deferredRead.OK() && deferredWrite.Code == sqlitex.CodeBusy,
		"BEGIN DEFERRED and a read succeed; the first write statement returns SQLITE_BUSY (5)")
	lab.Must(second, "rollback")
	immediate := lab.Do(second, "begin", "immediate")
	lab.Check(immediate.Code == sqlitex.CodeBusy, "BEGIN IMMEDIATE returns SQLITE_BUSY (5) at the BEGIN itself")
	lab.Must(first, "commit")
	lab.StopChildren()
}

// writersWithTimeout runs Part C: with a 120s busy_timeout, transactions that
// have not read yet (C1 BEGIN IMMEDIATE, C2 BEGIN DEFERRED writing first) wait
// on the busy handler (src/main.c:1730-1792), while a deferred transaction that
// already read (C3) skips the busy handler (src/btree.c:3740-3741) and gets
// SQLITE_BUSY (5) then SQLITE_BUSY_SNAPSHOT (517, src/wal.c:3734-3737) at once.
func writersWithTimeout(ctx context.Context, lab *recorder, dir string) {
	part = "C"
	first, second := pair(ctx, lab, dir, "c-writers-timeout", sqlitex.JournalWAL, longTimeout, "writer-1", "writer-2")

	part = "C1"
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	waiting := lab.Begin(second, "begin", "immediate")
	blocked := lab.StillWaiting(waiting)
	lab.Must(first, "commit")
	admitted := lab.Finish(waiting)
	lab.Check(blocked && admitted.OK(), "BEGIN IMMEDIATE did not return while the other writer held the lock, and returned SQLITE_OK after it committed")
	lab.Must(second, "exec", insert("writer-2"))
	lab.Must(second, "commit")

	part = "C2"
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	lab.Must(second, "begin", "deferred")
	pending := lab.Begin(second, "exec", insert("writer-2"))
	blocked = lab.StillWaiting(pending)
	lab.Must(first, "commit")
	written := lab.Finish(pending)
	lab.Check(blocked && written.OK(), "the first statement of a deferred transaction waits like BEGIN IMMEDIATE and then succeeds")
	lab.Must(second, "commit")

	part = "C3"
	lab.Must(second, "begin", "deferred")
	lab.Must(second, "query", countQuery)
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	upgrade := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(upgrade.Code == sqlitex.CodeBusy, "its first write returns SQLITE_BUSY (5) at once although the busy timeout is 120000ms")
	lab.Must(first, "commit")
	stale := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(stale.Code == sqlitex.CodeBusySnapshot, "after the other writer committed, the same write returns SQLITE_BUSY_SNAPSHOT (517) at once: the snapshot it read is no longer the newest")
	lab.Must(second, "rollback")
	retry := []procs.Result{lab.Do(second, "begin", "immediate"), lab.Do(second, "exec", insert("writer-2")), lab.Do(second, "commit")}
	lab.Check(allOK(retry), "the same write succeeds once the transaction is restarted")
	lab.StopChildren()
}

// rollbackContrast runs Part D: in rollback mode a reader's SHARED lock blocks
// the writer's COMMIT (EXCLUSIVE), and the failed COMMIT retains PENDING so new
// readers are blocked until the writer retries (src/os_unix.c:1898-1905).
func rollbackContrast(ctx context.Context, lab *recorder, dir string) {
	part = "D"
	reader, writer := pair(ctx, lab, dir, "d-rollback", sqlitex.JournalDelete, 0, "reader", "writer")
	lab.Must(reader, "begin", "deferred")
	lab.Must(reader, "query", countQuery)
	writes := []procs.Result{lab.Do(writer, "begin", "immediate"), lab.Do(writer, "exec", insert("writer"))}
	lab.Check(allOK(writes), "the writer takes RESERVED and writes beside the reader's SHARED lock")
	blockedCommit := lab.Do(writer, "commit")
	lab.Check(blockedCommit.Code == sqlitex.CodeBusy, "the writer's COMMIT returns SQLITE_BUSY (5): it needs EXCLUSIVE and the reader holds SHARED")
	lab.Must(reader, "commit")
	newReader := lab.Do(reader, "query", countQuery)
	lab.Check(newReader.Code == sqlitex.CodeBusy, "a new read now returns SQLITE_BUSY (5): the writer kept PENDING after its failed COMMIT")
	retried := lab.Do(writer, "commit")
	lab.Check(retried.OK(), "the writer's second COMMIT succeeds once no reader is left")
	after := lab.Do(reader, "query", countQuery)
	lab.Check(after.OK() && after.Value == "2", "and the reader then reads the committed row")
	lab.StopChildren()
}

// recipeUnderContention runs Part E: two processes alternate recipe steps 6-15
// using BEGIN IMMEDIATE and a 120s busy_timeout; each waiter sends BEGIN IMMEDIATE
// while the holder still owns WAL_WRITE_LOCK so contention is verified by StillWaiting.
func recipeUnderContention(ctx context.Context, lab *recorder, dir string) {
	part = "E"
	r := recipe.Default()
	db := filepath.Join(dir, "e-recipe", "r.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(db); err != nil {
		lab.Fatal("remove", err)
	}
	if err := r.Build(ctx, db, recipeBase); err != nil {
		lab.Fatal("build", err)
	}
	options := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
	tables.settings.Row(part, options.JournalMode, longTimeout.Milliseconds())
	writers := []*procs.Child{lab.SpawnSQL("writer-1", options), lab.SpawnSQL("writer-2", options)}
	allWaited, allCommitted := true, true
	for k := recipeBase + 1; k < recipeBase+recipeWrites; k += 2 {
		holder, waiter := writers[0], writers[1]
		lab.Must(holder, "begin", "immediate")
		lab.Must(holder, "recipe-exec", strconv.Itoa(k))
		call := lab.Begin(waiter, "begin", "immediate")
		allWaited = lab.StillWaiting(call) && allWaited
		allCommitted = lab.Do(holder, "commit").OK() && allCommitted
		allCommitted = lab.Finish(call).OK() && allCommitted
		allCommitted = lab.Do(waiter, "recipe-exec", strconv.Itoa(k+1)).OK() && allCommitted
		allCommitted = lab.Do(waiter, "commit").OK() && allCommitted
		writers[0], writers[1] = writers[1], writers[0]
	}
	last := lab.Must(writers[0], "user-version")
	lab.Check(allWaited && allCommitted, "every waiting BEGIN IMMEDIATE was still waiting while the other writer held the lock, and every transaction committed")
	lab.StopChildren()
	match, err := r.MatchStep(ctx, db)
	if err != nil {
		lab.Fatal("match", err)
	}
	var matched any
	if match.Found() {
		matched = match.Step
	}
	tables.recipeResult.Row(part, last.Value, matched, match.Exact)
	lab.Check(last.Value == strconv.Itoa(recipeBase+recipeWrites) && match.Found() && match.Step == recipeBase+recipeWrites,
		fmt.Sprintf("the final database equals the recipe's state after step %d", recipeBase+recipeWrites))
}

func allOK(results []procs.Result) bool {
	for _, r := range results {
		if !r.OK() {
			return false
		}
	}
	return true
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	lab := newRecorder()
	lab.Script.Timeout = stepDeadline
	readersAndWriter(ctx, lab, dir)
	writersWithoutTimeout(ctx, lab, dir)
	writersWithTimeout(ctx, lab, dir)
	rollbackContrast(ctx, lab, dir)
	recipeUnderContention(ctx, lab, dir)
	set.Finish("BUSY RULES")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/6.2-busy-rules/report.sql -->
```sql
-- Walkthrough of experiment 6.2. Run it with:
--   go run ./cmd/course run 6.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps          one statement or action one child process ran, in the order the answers came back
--   settings       one part: journal_mode and busy_timeout of both connections
--   recipe_result  part E: the user_version the last writer saw and the recipe step the closed file equals
--   checks         every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  6.2  The busy rules'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Two child processes, each with one connection to the same database, run'
.print 'scripted steps in turn. Every step is numbered in the order it was sent.'
.print 'A step can also be sent without waiting for its answer: the parent then'
.print 'checks that no answer came within 250ms (at_250ms below), runs other steps,'
.print 'and collects the answer later, so its row appears after theirs.'
.print
.print 'Every step must answer within 30s. A statement that waited out a busy'
.print 'timeout of 120000ms would fail the run instead of returning late.'
.print
.print 'The settings of each part (the same on both connections):'

SELECT part, journal_mode, busy_timeout_ms FROM settings ORDER BY part;

.print
.print '── A. WAL mode: a reader and a writer, busy_timeout 0 ─────────────────'
.print
.print 'With busy_timeout 0 any wait would show up as SQLITE_BUSY. None does: the'
.print 'writer commits beside an open reader (steps 3-5), the reader keeps its'
.print 'snapshot (step 6 still counts 1 row), a new reader reads the last committed'
.print 'state while a write is open (step 11), and BEGIN EXCLUSIVE (step 16) starts'
.print 'beside an open reader, exactly like BEGIN IMMEDIATE.'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'A' ORDER BY seq;

.print
.print '── B. WAL mode: two writers, busy_timeout 0 ───────────────────────────'
.print
.print 'BEGIN DEFERRED and a read succeed; the first write returns SQLITE_BUSY'
.print '(step 25). BEGIN IMMEDIATE returns SQLITE_BUSY at the BEGIN itself (step 27).'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'B' ORDER BY seq;

.print
.print '── C. WAL mode: two writers, busy_timeout 120000ms ────────────────────'
.print
.print 'C1. BEGIN IMMEDIATE waits for the lock. Step 31 is sent while writer-1'
.print '    holds the lock, has no answer after 250ms, and returns SQLITE_OK once'
.print '    writer-1 commits (step 32).'

SELECT step, process, action,
       CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END AS at_250ms,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C1' ORDER BY seq;

.print
.print 'C2. A deferred transaction that has not read yet also waits, at its first'
.print '    write (step 38), and then succeeds.'

SELECT step, process, action,
       CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END AS at_250ms,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C2' ORDER BY seq;

.print
.print 'C3. A deferred transaction that has already read (step 42) cannot wait: its'
.print '    first write returns SQLITE_BUSY at once (step 45), despite the 120000ms'
.print '    timeout. After the other writer commits, the same write returns'
.print '    SQLITE_BUSY_SNAPSHOT (step 47): the snapshot it read is no longer the'
.print '    newest. Only a restarted transaction succeeds (steps 49-51).'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C3' ORDER BY seq;

.print
.print '── D. Rollback mode (journal_mode=delete), busy_timeout 0 ─────────────'
.print
.print "The contrast. The writer takes RESERVED and writes beside the reader's SHARED"
.print 'lock, but its COMMIT needs EXCLUSIVE and returns SQLITE_BUSY (step 56). The'
.print 'failed COMMIT keeps PENDING, so a new read also returns SQLITE_BUSY (step 58)'
.print "until the writer's second COMMIT succeeds (step 59)."

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'D' ORDER BY seq;

.print
.print '── E. Recipe steps 6-15 under contention ──────────────────────────────'
.print
.print 'The database is the recipe built to step 5. Two writers apply steps 6-15'
.print 'in turn, each in its own BEGIN IMMEDIATE transaction, busy_timeout 120000ms.'
.print 'Each BEGIN IMMEDIATE is sent while the other writer still holds the lock.'
.print
.print 'E1. One row per recipe step: who applied it, whether its BEGIN IMMEDIATE'
.print '    was still waiting at 250ms (the first writer of a pair began alone),'
.print '    and how its BEGIN and COMMIT ended.'

WITH e AS (SELECT * FROM steps WHERE part = 'E'),
     x AS (SELECT seq, process, CAST(split_part(action, ' ', 2) AS INTEGER) AS recipe_step
           FROM e WHERE action LIKE 'recipe-exec %')
SELECT x.recipe_step,
       x.process,
       (SELECT CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END
        FROM e WHERE e.process = x.process AND e.action = 'begin immediate' AND e.seq < x.seq
        ORDER BY e.seq DESC LIMIT 1) AS begin_at_250ms,
       (SELECT result FROM e WHERE e.process = x.process AND e.action = 'begin immediate' AND e.seq < x.seq
        ORDER BY e.seq DESC LIMIT 1) AS begin_result,
       (SELECT e.result FROM e WHERE e.seq = x.seq) AS exec_result,
       (SELECT result FROM e WHERE e.process = x.process AND e.action = 'commit' AND e.seq > x.seq
        ORDER BY e.seq LIMIT 1) AS commit_result
FROM x ORDER BY x.recipe_step;

.print
.print 'E2. The full transcript.'

SELECT step, process, action,
       CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END AS at_250ms,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'E' ORDER BY seq;

.print
.print 'E3. The user_version the last writer saw, and the recipe step the closed'
.print '    file equals (exact = false: equal except the change counter at bytes'
.print '    24-27 and 92-95).'

SELECT user_version, matched_step, exact FROM recipe_result ORDER BY part;
```

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/6.2-busy-rules/`](evidence/6.2-busy-rules/), and `go run ./cmd/course check evidence ch06` compares a fresh run with them byte for byte.

<!-- include: evidence/6.2-busy-rules.txt -->
```text

════════════════════════════════════════════════════════════════════════
  6.2  The busy rules
════════════════════════════════════════════════════════════════════════

Two child processes, each with one connection to the same database, run
scripted steps in turn. Every step is numbered in the order it was sent.
A step can also be sent without waiting for its answer: the parent then
checks that no answer came within 250ms (at_250ms below), runs other steps,
and collects the answer later, so its row appears after theirs.

Every step must answer within 30s. A statement that waited out a busy
timeout of 120000ms would fail the run instead of returning late.

The settings of each part (the same on both connections):
┌──────┬──────────────┬─────────────────┐
│ part │ journal_mode │ busy_timeout_ms │
├──────┼──────────────┼─────────────────┤
│ A    │ wal          │ 0               │
│ B    │ wal          │ 0               │
│ C    │ wal          │ 120000          │
│ D    │ delete       │ 0               │
│ E    │ wal          │ 120000          │
└──────┴──────────────┴─────────────────┘

── A. WAL mode: a reader and a writer, busy_timeout 0 ─────────────────

With busy_timeout 0 any wait would show up as SQLITE_BUSY. None does: the
writer commits beside an open reader (steps 3-5), the reader keeps its
snapshot (step 6 still counts 1 row), a new reader reads the last committed
state while a write is open (step 11), and BEGIN EXCLUSIVE (step 16) starts
beside an open reader, exactly like BEGIN IMMEDIATE.
┌──────┬─────────┬──────────────────────────────────────────────────┬───────────┬───────┐
│ step │ process │                      action                      │  result   │ value │
├──────┼─────────┼──────────────────────────────────────────────────┼───────────┼───────┤
│ 1    │ reader  │ begin deferred                                   │ SQLITE_OK │       │
│ 2    │ reader  │ query SELECT count(*) FROM lab                   │ SQLITE_OK │ 1     │
│ 3    │ writer  │ begin immediate                                  │ SQLITE_OK │       │
│ 4    │ writer  │ exec INSERT INTO lab(who) VALUES ('writer')      │ SQLITE_OK │       │
│ 5    │ writer  │ commit                                           │ SQLITE_OK │       │
│ 6    │ reader  │ query SELECT count(*) FROM lab                   │ SQLITE_OK │ 1     │
│ 7    │ reader  │ commit                                           │ SQLITE_OK │       │
│ 8    │ writer  │ begin immediate                                  │ SQLITE_OK │       │
│ 9    │ writer  │ exec INSERT INTO lab(who) VALUES ('uncommitted') │ SQLITE_OK │       │
│ 10   │ reader  │ begin deferred                                   │ SQLITE_OK │       │
│ 11   │ reader  │ query SELECT count(*) FROM lab                   │ SQLITE_OK │ 2     │
│ 12   │ writer  │ commit                                           │ SQLITE_OK │       │
│ 13   │ reader  │ commit                                           │ SQLITE_OK │       │
│ 14   │ reader  │ begin deferred                                   │ SQLITE_OK │       │
│ 15   │ reader  │ query SELECT count(*) FROM lab                   │ SQLITE_OK │ 3     │
│ 16   │ writer  │ begin exclusive                                  │ SQLITE_OK │       │
│ 17   │ writer  │ exec INSERT INTO lab(who) VALUES ('exclusive')   │ SQLITE_OK │       │
│ 18   │ reader  │ query SELECT count(*) FROM lab                   │ SQLITE_OK │ 3     │
│ 19   │ writer  │ commit                                           │ SQLITE_OK │       │
│ 20   │ reader  │ commit                                           │ SQLITE_OK │       │
└──────┴─────────┴──────────────────────────────────────────────────┴───────────┴───────┘

── B. WAL mode: two writers, busy_timeout 0 ───────────────────────────

BEGIN DEFERRED and a read succeed; the first write returns SQLITE_BUSY
(step 25). BEGIN IMMEDIATE returns SQLITE_BUSY at the BEGIN itself (step 27).
┌──────┬──────────┬───────────────────────────────────────────────┬─────────────────┬───────┐
│ step │ process  │                    action                     │     result      │ value │
├──────┼──────────┼───────────────────────────────────────────────┼─────────────────┼───────┤
│ 21   │ writer-1 │ begin immediate                               │ SQLITE_OK       │       │
│ 22   │ writer-1 │ exec INSERT INTO lab(who) VALUES ('writer-1') │ SQLITE_OK       │       │
│ 23   │ writer-2 │ begin deferred                                │ SQLITE_OK       │       │
│ 24   │ writer-2 │ query SELECT count(*) FROM lab                │ SQLITE_OK       │ 1     │
│ 25   │ writer-2 │ exec INSERT INTO lab(who) VALUES ('writer-2') │ SQLITE_BUSY (5) │       │
│ 26   │ writer-2 │ rollback                                      │ SQLITE_OK       │       │
│ 27   │ writer-2 │ begin immediate                               │ SQLITE_BUSY (5) │       │
│ 28   │ writer-1 │ commit                                        │ SQLITE_OK       │       │
└──────┴──────────┴───────────────────────────────────────────────┴─────────────────┴───────┘

── C. WAL mode: two writers, busy_timeout 120000ms ────────────────────

C1. BEGIN IMMEDIATE waits for the lock. Step 31 is sent while writer-1
    holds the lock, has no answer after 250ms, and returns SQLITE_OK once
    writer-1 commits (step 32).
┌──────┬──────────┬───────────────────────────────────────────────┬───────────┬───────────┬───────┐
│ step │ process  │                    action                     │ at_250ms  │  result   │ value │
├──────┼──────────┼───────────────────────────────────────────────┼───────────┼───────────┼───────┤
│ 29   │ writer-1 │ begin immediate                               │           │ SQLITE_OK │       │
│ 30   │ writer-1 │ exec INSERT INTO lab(who) VALUES ('writer-1') │           │ SQLITE_OK │       │
│ 32   │ writer-1 │ commit                                        │           │ SQLITE_OK │       │
│ 31   │ writer-2 │ begin immediate                               │ no answer │ SQLITE_OK │       │
│ 33   │ writer-2 │ exec INSERT INTO lab(who) VALUES ('writer-2') │           │ SQLITE_OK │       │
│ 34   │ writer-2 │ commit                                        │           │ SQLITE_OK │       │
└──────┴──────────┴───────────────────────────────────────────────┴───────────┴───────────┴───────┘

C2. A deferred transaction that has not read yet also waits, at its first
    write (step 38), and then succeeds.
┌──────┬──────────┬───────────────────────────────────────────────┬───────────┬───────────┬───────┐
│ step │ process  │                    action                     │ at_250ms  │  result   │ value │
├──────┼──────────┼───────────────────────────────────────────────┼───────────┼───────────┼───────┤
│ 35   │ writer-1 │ begin immediate                               │           │ SQLITE_OK │       │
│ 36   │ writer-1 │ exec INSERT INTO lab(who) VALUES ('writer-1') │           │ SQLITE_OK │       │
│ 37   │ writer-2 │ begin deferred                                │           │ SQLITE_OK │       │
│ 39   │ writer-1 │ commit                                        │           │ SQLITE_OK │       │
│ 38   │ writer-2 │ exec INSERT INTO lab(who) VALUES ('writer-2') │ no answer │ SQLITE_OK │       │
│ 40   │ writer-2 │ commit                                        │           │ SQLITE_OK │       │
└──────┴──────────┴───────────────────────────────────────────────┴───────────┴───────────┴───────┘

C3. A deferred transaction that has already read (step 42) cannot wait: its
    first write returns SQLITE_BUSY at once (step 45), despite the 120000ms
    timeout. After the other writer commits, the same write returns
    SQLITE_BUSY_SNAPSHOT (step 47): the snapshot it read is no longer the
    newest. Only a restarted transaction succeeds (steps 49-51).
┌──────┬──────────┬───────────────────────────────────────────────┬────────────────────────────┬───────┐
│ step │ process  │                    action                     │           result           │ value │
├──────┼──────────┼───────────────────────────────────────────────┼────────────────────────────┼───────┤
│ 41   │ writer-2 │ begin deferred                                │ SQLITE_OK                  │       │
│ 42   │ writer-2 │ query SELECT count(*) FROM lab                │ SQLITE_OK                  │ 5     │
│ 43   │ writer-1 │ begin immediate                               │ SQLITE_OK                  │       │
│ 44   │ writer-1 │ exec INSERT INTO lab(who) VALUES ('writer-1') │ SQLITE_OK                  │       │
│ 45   │ writer-2 │ exec INSERT INTO lab(who) VALUES ('writer-2') │ SQLITE_BUSY (5)            │       │
│ 46   │ writer-1 │ commit                                        │ SQLITE_OK                  │       │
│ 47   │ writer-2 │ exec INSERT INTO lab(who) VALUES ('writer-2') │ SQLITE_BUSY_SNAPSHOT (517) │       │
│ 48   │ writer-2 │ rollback                                      │ SQLITE_OK                  │       │
│ 49   │ writer-2 │ begin immediate                               │ SQLITE_OK                  │       │
│ 50   │ writer-2 │ exec INSERT INTO lab(who) VALUES ('writer-2') │ SQLITE_OK                  │       │
│ 51   │ writer-2 │ commit                                        │ SQLITE_OK                  │       │
└──────┴──────────┴───────────────────────────────────────────────┴────────────────────────────┴───────┘

── D. Rollback mode (journal_mode=delete), busy_timeout 0 ─────────────

The contrast. The writer takes RESERVED and writes beside the reader's SHARED
lock, but its COMMIT needs EXCLUSIVE and returns SQLITE_BUSY (step 56). The
failed COMMIT keeps PENDING, so a new read also returns SQLITE_BUSY (step 58)
until the writer's second COMMIT succeeds (step 59).
┌──────┬─────────┬─────────────────────────────────────────────┬─────────────────┬───────┐
│ step │ process │                   action                    │     result      │ value │
├──────┼─────────┼─────────────────────────────────────────────┼─────────────────┼───────┤
│ 52   │ reader  │ begin deferred                              │ SQLITE_OK       │       │
│ 53   │ reader  │ query SELECT count(*) FROM lab              │ SQLITE_OK       │ 1     │
│ 54   │ writer  │ begin immediate                             │ SQLITE_OK       │       │
│ 55   │ writer  │ exec INSERT INTO lab(who) VALUES ('writer') │ SQLITE_OK       │       │
│ 56   │ writer  │ commit                                      │ SQLITE_BUSY (5) │       │
│ 57   │ reader  │ commit                                      │ SQLITE_OK       │       │
│ 58   │ reader  │ query SELECT count(*) FROM lab              │ SQLITE_BUSY (5) │       │
│ 59   │ writer  │ commit                                      │ SQLITE_OK       │       │
│ 60   │ reader  │ query SELECT count(*) FROM lab              │ SQLITE_OK       │ 2     │
└──────┴─────────┴─────────────────────────────────────────────┴─────────────────┴───────┘

── E. Recipe steps 6-15 under contention ──────────────────────────────

The database is the recipe built to step 5. Two writers apply steps 6-15
in turn, each in its own BEGIN IMMEDIATE transaction, busy_timeout 120000ms.
Each BEGIN IMMEDIATE is sent while the other writer still holds the lock.

E1. One row per recipe step: who applied it, whether its BEGIN IMMEDIATE
    was still waiting at 250ms (the first writer of a pair began alone),
    and how its BEGIN and COMMIT ended.
┌─────────────┬──────────┬────────────────┬──────────────┬─────────────┬───────────────┐
│ recipe_step │ process  │ begin_at_250ms │ begin_result │ exec_result │ commit_result │
├─────────────┼──────────┼────────────────┼──────────────┼─────────────┼───────────────┤
│ 6           │ writer-1 │                │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 7           │ writer-2 │ no answer      │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 8           │ writer-2 │                │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 9           │ writer-1 │ no answer      │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 10          │ writer-1 │                │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 11          │ writer-2 │ no answer      │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 12          │ writer-2 │                │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 13          │ writer-1 │ no answer      │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 14          │ writer-1 │                │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
│ 15          │ writer-2 │ no answer      │ SQLITE_OK    │ SQLITE_OK   │ SQLITE_OK     │
└─────────────┴──────────┴────────────────┴──────────────┴─────────────┴───────────────┘

E2. The full transcript.
┌──────┬──────────┬─────────────────┬───────────┬───────────┬───────┐
│ step │ process  │     action      │ at_250ms  │  result   │ value │
├──────┼──────────┼─────────────────┼───────────┼───────────┼───────┤
│ 61   │ writer-1 │ begin immediate │           │ SQLITE_OK │       │
│ 62   │ writer-1 │ recipe-exec 6   │           │ SQLITE_OK │       │
│ 64   │ writer-1 │ commit          │           │ SQLITE_OK │       │
│ 63   │ writer-2 │ begin immediate │ no answer │ SQLITE_OK │       │
│ 65   │ writer-2 │ recipe-exec 7   │           │ SQLITE_OK │       │
│ 66   │ writer-2 │ commit          │           │ SQLITE_OK │       │
│ 67   │ writer-2 │ begin immediate │           │ SQLITE_OK │       │
│ 68   │ writer-2 │ recipe-exec 8   │           │ SQLITE_OK │       │
│ 70   │ writer-2 │ commit          │           │ SQLITE_OK │       │
│ 69   │ writer-1 │ begin immediate │ no answer │ SQLITE_OK │       │
│ 71   │ writer-1 │ recipe-exec 9   │           │ SQLITE_OK │       │
│ 72   │ writer-1 │ commit          │           │ SQLITE_OK │       │
│ 73   │ writer-1 │ begin immediate │           │ SQLITE_OK │       │
│ 74   │ writer-1 │ recipe-exec 10  │           │ SQLITE_OK │       │
│ 76   │ writer-1 │ commit          │           │ SQLITE_OK │       │
│ 75   │ writer-2 │ begin immediate │ no answer │ SQLITE_OK │       │
│ 77   │ writer-2 │ recipe-exec 11  │           │ SQLITE_OK │       │
│ 78   │ writer-2 │ commit          │           │ SQLITE_OK │       │
│ 79   │ writer-2 │ begin immediate │           │ SQLITE_OK │       │
│ 80   │ writer-2 │ recipe-exec 12  │           │ SQLITE_OK │       │
│ 82   │ writer-2 │ commit          │           │ SQLITE_OK │       │
│ 81   │ writer-1 │ begin immediate │ no answer │ SQLITE_OK │       │
│ 83   │ writer-1 │ recipe-exec 13  │           │ SQLITE_OK │       │
│ 84   │ writer-1 │ commit          │           │ SQLITE_OK │       │
│ 85   │ writer-1 │ begin immediate │           │ SQLITE_OK │       │
│ 86   │ writer-1 │ recipe-exec 14  │           │ SQLITE_OK │       │
│ 88   │ writer-1 │ commit          │           │ SQLITE_OK │       │
│ 87   │ writer-2 │ begin immediate │ no answer │ SQLITE_OK │       │
│ 89   │ writer-2 │ recipe-exec 15  │           │ SQLITE_OK │       │
│ 90   │ writer-2 │ commit          │           │ SQLITE_OK │       │
│ 91   │ writer-2 │ user-version    │           │ SQLITE_OK │ 15    │
└──────┴──────────┴─────────────────┴───────────┴───────────┴───────┘

E3. The user_version the last writer saw, and the recipe step the closed
    file equals (exact = false: equal except the change counter at bytes
    24-27 and 92-95).
┌──────────────┬──────────────┬───────┐
│ user_version │ matched_step │ exact │
├──────────────┼──────────────┼───────┤
│ 15           │ 15           │ false │
└──────────────┴──────────────┴───────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  with a read transaction open, the writer begins, writes and commits without waiting
A  ✓  the reader still sees its snapshot (1 rows) after the commit
A  ✓  with a write transaction open, a reader starts and reads the last committed state (2 rows) without waiting
A  ✓  BEGIN EXCLUSIVE in WAL mode behaves like BEGIN IMMEDIATE: it starts beside an open reader and the reader keeps reading
B  ✓  BEGIN DEFERRED and a read succeed; the first write statement returns SQLITE_BUSY (5)
B  ✓  BEGIN IMMEDIATE returns SQLITE_BUSY (5) at the BEGIN itself
C1  ✓  BEGIN IMMEDIATE did not return while the other writer held the lock, and returned SQLITE_OK after it committed
C2  ✓  the first statement of a deferred transaction waits like BEGIN IMMEDIATE and then succeeds
C3  ✓  its first write returns SQLITE_BUSY (5) at once although the busy timeout is 120000ms
C3  ✓  after the other writer committed, the same write returns SQLITE_BUSY_SNAPSHOT (517) at once: the snapshot it read is no longer the newest
C3  ✓  the same write succeeds once the transaction is restarted
D  ✓  the writer takes RESERVED and writes beside the reader's SHARED lock
D  ✓  the writer's COMMIT returns SQLITE_BUSY (5): it needs EXCLUSIVE and the reader holds SHARED
D  ✓  a new read now returns SQLITE_BUSY (5): the writer kept PENDING after its failed COMMIT
D  ✓  the writer's second COMMIT succeeds once no reader is left
D  ✓  and the reader then reads the committed row
E  ✓  every waiting BEGIN IMMEDIATE was still waiting while the other writer held the lock, and every transaction committed
E  ✓  the final database equals the recipe's state after step 15

18 of 18 claims hold

Experiment verdict: BUSY RULES OK
```

**Reading the evidence:**

The script sets a `30s` per-step deadline (`stepDeadline`) against a `120,000ms` `busy_timeout` (`longTimeout`). If any statement meant to fail immediately had entered the busy-handler sleep loop for `120s`, the child would time out at `30s` and fail the run loudly.

| Part | Setup | Statement | Result |
| --- | --- | --- | --- |
| A (WAL, `0ms`) | Reader open (`count=1`) | Writer `BEGIN IMMEDIATE` / `INSERT` / `COMMIT` | `SQLITE_OK` / `SQLITE_OK` / `SQLITE_OK`; reader still sees `1` |
| A (WAL, `0ms`) | Writer holds open write txn | Reader `BEGIN DEFERRED` + `count` | `SQLITE_OK 2` (last committed state) |
| A (WAL, `0ms`) | Reader open (`count=3`) | Writer `BEGIN EXCLUSIVE` / `INSERT` / `COMMIT`, reader queries during txn | All `SQLITE_OK` (`EXCLUSIVE` = `IMMEDIATE` in WAL) |
| B (WAL, `0ms`) | `writer-1` holds write txn | `writer-2` `BEGIN DEFERRED` / `count` / `INSERT` | `OK` / `OK 1` / `SQLITE_BUSY (5)` at first write |
| B (WAL, `0ms`) | `writer-1` holds write txn | `writer-2` `BEGIN IMMEDIATE` | `SQLITE_BUSY (5)` at `BEGIN` itself |
| C1 (WAL, `120s`) | `writer-1` holds write txn | `writer-2` `BEGIN IMMEDIATE` | `no answer` at 250ms, then `SQLITE_OK` after `writer-1` commits |
| C2 (WAL, `120s`) | `writer-1` holds write txn | `writer-2` `BEGIN DEFERRED`, then `INSERT` (no prior read) | `no answer` at 250ms, then `SQLITE_OK` after `writer-1` commits |
| C3 (WAL, `120s`) | `writer-2` `BEGIN DEFERRED` + `count=5`; `writer-1` holds write txn | `writer-2` `INSERT` | `SQLITE_BUSY (5)` at once (skips `120s` timeout) |
| C3 (WAL, `120s`) | `writer-1` commits | `writer-2` same `INSERT` | `SQLITE_BUSY_SNAPSHOT (517)` at once |
| C3 (WAL, `120s`) | `writer-2` `ROLLBACK` | `BEGIN IMMEDIATE` / `INSERT` / `COMMIT` | All `SQLITE_OK` |
| D (delete, `0ms`) | Reader open (`SHARED`) | Writer `BEGIN IMMEDIATE` / `INSERT` / `COMMIT` | `OK` / `OK` / `SQLITE_BUSY (5)` |
| D (delete, `0ms`) | Reader ends, tries new `count` | Reader `count` (autocommit) | `SQLITE_BUSY (5)` (writer kept `PENDING`) |
| D (delete, `0ms`) | — | Writer `COMMIT`, then reader `count` | `SQLITE_OK`, then `SQLITE_OK 2` |
| E (WAL, `120s`) | Two writers alternating recipe steps 6–15 | Each waiter sends `BEGIN IMMEDIATE` while holder owns lock | All 5 waiting steps report `no answer` at 250ms, then `SQLITE_OK`; closed file equals recipe step `15` |

- **What `no answer` at 250ms proves (and its limit):** `modernc.org/sqlite` does not expose a Go callback for `sqlite3_busy_handler`, so the experiment cannot print an in-process busy-handler call count. Instead, `Lab.StillWaiting` waits `250ms` (`StillWaitingWindow`) via `Call.AnsweredWithin` without killing the child. A non-waiting failure (`busy_timeout=0`) answers over the local pipe in well under `250ms` and is recorded as `answered` (`internal/procs` prints `.. already answered`) (verified by `TestStillWaitingSeesImmediateBusy` and adversarial test `BUSY-09`, whose `strace` of Experiment 6.2 recorded `87` `EAGAIN` return values on `-shm` byte `120`).
- **Part E match (E3):** `user_version` `15`, matched step `15`, exact `false`: the closed file equals recipe step 15 except the change counter at bytes 24-27 and 92-95. Because two long-lived connections applied steps 6–15 instead of opening a fresh connection per step, the file's change-counter bytes differ from the per-step reference build while every other byte of every page is identical (the Chapter 1 and Chapter 2 rule).

---

## 5. Lab

Every multi-process lab in this chapter (and later chapters) uses `internal/procs` to eliminate timing races. Section 5.1 walks through and inlines `internal/procs` in full. Section 5.2 runs Experiment 6.3 to prove why plain OS copies of a live database fail.

### 5.1 Shared package `internal/procs`

Rather than compiling helper binaries or coordinating processes with `time.Sleep`, `internal/procs` re-executes the experiment binary itself with `os.Args[1] == "procs-child" <role> <args...>`. The parent attaches two OS pipes on `exec.Cmd.ExtraFiles`, which Unix maps to file descriptors `3` (requests in) and `4` (replies out), leaving `stdout` and `stderr` free for diagnostics. Every request and reply is one JSON line tagged with a global step number.

- **Protocol (`protocol.go`):** Defines `Request`, `Result`, `Actor`, `Role`, `Serve` (called first in `main` and `TestMain`), and `RunChild`. `ResultFor` extracts SQLite's extended result code via `sqlitex.ErrorCode`; non-SQLite errors record `CodeNotSQLite = -1`.

<!-- include: ../internal/procs/protocol.go -->
```go
// Package procs coordinates scripted multi-process SQLite experiments over
// step barriers. An experiment binary re-executes itself in named child roles
// ("sql", "copier", or custom roles), exchanging JSON request/reply lines over
// inherited pipes (file descriptors 3 and 4) so lock ownership and interleavings
// are deterministic rather than timing-dependent.
package procs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// ChildMarker is the os.Args[1] sentinel that switches a re-executed binary
// into child-role mode inside Serve.
const ChildMarker = "procs-child"

// Control actions exchanged between parent and child outside role-defined actions.
const (
	// ActionReady is the step-0 reply a child emits once its role has started, carrying its PID.
	ActionReady = "ready"
	// ActionExit asks a child to close its Actor, reply with the close result, and exit.
	ActionExit = "exit"
)

// Result code sentinels stored in Result.Code.
const (
	// CodeOK indicates a command succeeded (matching SQLITE_OK).
	CodeOK = 0
	// CodeNotSQLite marks an error that did not originate from SQLite (no SQLite result code).
	CodeNotSQLite = -1
)

// Inherited pipe file descriptors attached via exec.Cmd.ExtraFiles in StartCommand.
// Go maps ExtraFiles[0] to fd 3 and ExtraFiles[1] to fd 4, keeping stdout/stderr free for diagnostics.
const (
	requestFD = 3
	replyFD   = 4
)

// Request is one numbered step sent from the parent to a child over fd 3.
type Request struct {
	Step   int      `json:"step"`
	Action string   `json:"action"`
	Args   []string `json:"args,omitempty"`
}

// Result is the child's reply on fd 4 for a Request, recording either a value
// on success or a SQLite extended result code (or CodeNotSQLite) on failure.
type Result struct {
	Step   int    `json:"step"`
	Action string `json:"action"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
	Value  string `json:"value,omitempty"`
}

// OK reports whether the step succeeded with CodeOK and no error string.
func (r Result) OK() bool { return r.Code == CodeOK && r.Error == "" }

// CodeName returns the SQLite macro name for r.Code, or "ERROR" for non-SQLite errors.
func (r Result) CodeName() string {
	if r.Code == CodeNotSQLite {
		return "ERROR"
	}
	return sqlitex.ResultCodeName(r.Code)
}

// Outcome formats r as a single transcript token: "SQLITE_OK [value]",
// "<SQLITE_NAME> (<code>)", or "ERROR <text>".
func (r Result) Outcome() string {
	if r.OK() {
		if r.Value == "" {
			return sqlitex.ResultCodeName(CodeOK)
		}
		return sqlitex.ResultCodeName(CodeOK) + " " + r.Value
	}
	if r.Code == CodeNotSQLite {
		return "ERROR " + r.Error
	}
	return fmt.Sprintf("%s (%d)", r.CodeName(), r.Code)
}

// Handler executes one named action inside a child process and returns an
// optional single-line text value or an error.
type Handler func(ctx context.Context, args []string) (string, error)

// Actor holds the action handlers and optional cleanup hook for a started role.
type Actor struct {
	Actions map[string]Handler
	Close   func() error
}

// Role constructs an Actor from role-specific command-line arguments when a
// child process starts.
type Role func(ctx context.Context, args []string) (*Actor, error)

// ResultFor converts a Handler's (value, err) return into a Result, extracting
// SQLite's extended result code when err wraps a modernc.org/sqlite error.
func ResultFor(request Request, value string, err error) Result {
	result := Result{Step: request.Step, Action: request.Action, Value: value}
	if err == nil {
		return result
	}
	result.Error = err.Error()
	if code, ok := sqlitex.ErrorCode(err); ok {
		result.Code = code
	} else {
		result.Code = CodeNotSQLite
	}
	return result
}

// ErrUnknownRole is returned when RunChild is asked to start an unregistered role name.
var ErrUnknownRole = errors.New("procs: unknown role")

// Serve checks whether the current process was re-executed as a procs child
// (os.Args[1] == ChildMarker). If so, it runs the requested role over fd 3 and
// fd 4 and terminates the process; otherwise it returns false so main or
// TestMain can continue as the parent.
func Serve(roles map[string]Role) bool {
	if len(os.Args) < 3 || os.Args[1] != ChildMarker {
		return false
	}
	requests := os.NewFile(requestFD, "procs-requests")
	replies := os.NewFile(replyFD, "procs-replies")
	if requests == nil || replies == nil {
		fmt.Fprintln(os.Stderr, "procs: child started without its request and reply pipes")
		os.Exit(3)
	}
	err := RunChild(context.Background(), roles, os.Args[2], os.Args[3:], requests, replies)
	if err != nil {
		fmt.Fprintf(os.Stderr, "procs: child %s: %v\n", os.Args[2], err)
		os.Exit(2)
	}
	os.Exit(0)
	return true
}

// RunChild starts roleName, sends a step-0 ready reply containing the child's
// PID, and processes newline-delimited JSON requests until ActionExit arrives.
// Closing requests before ActionExit is treated as an error so a child detects
// a crashed parent instead of exiting silently.
func RunChild(ctx context.Context, roles map[string]Role, roleName string, args []string, requests io.Reader, replies io.Writer) error {
	role, ok := roles[roleName]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownRole, roleName)
	}
	encoder := json.NewEncoder(replies)
	actor, err := role(ctx, args)
	if err != nil {
		return fmt.Errorf("start role: %w", err)
	}
	if actor == nil {
		return errors.New("role returned no actor")
	}
	ready := Result{Step: 0, Action: ActionReady, Value: strconv.Itoa(os.Getpid())}
	if err := encoder.Encode(ready); err != nil {
		closeActor(actor)
		return err
	}
	scanner := bufio.NewScanner(requests)
	// Allow large SQL arguments or values without hitting bufio.Scanner's 64 KiB default cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var request Request
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			closeActor(actor)
			return fmt.Errorf("malformed request %q: %w", scanner.Text(), err)
		}
		if request.Action == ActionExit {
			closeErr := closeActor(actor)
			result := ResultFor(request, "", closeErr)
			if err := encoder.Encode(result); err != nil {
				return err
			}
			return closeErr
		}
		handler, ok := actor.Actions[request.Action]
		var result Result
		if !ok {
			result = ResultFor(request, "", fmt.Errorf("role %s has no action %q", roleName, request.Action))
		} else {
			value, err := handler(ctx, request.Args)
			result = ResultFor(request, value, err)
		}
		if err := encoder.Encode(result); err != nil {
			closeActor(actor)
			return err
		}
	}
	closeErr := closeActor(actor)
	if err := scanner.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return errors.New("request pipe closed before an exit request")
}

func closeActor(actor *Actor) error {
	if actor.Close == nil {
		return nil
	}
	return actor.Close()
}
```

- **Parent driver (`parent.go`):** Spawns children (`StartChild`, `StartCommand`), verifies the step-0 `ready` PID, enforces at most one outstanding `Call` per child (`ErrBusyChild`), provides `Call.AnsweredWithin` for non-destructive blocking checks, kills and reaps children on timeout or broken exit handshakes, and retains the last 8 KiB of child stderr/stdout in `tailBuffer`.

<!-- include: ../internal/procs/parent.go -->
```go
package procs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout is the per-step deadline applied to newly started children.
const DefaultTimeout = 30 * time.Second

// Sentinel errors returned by Child and Call methods so protocol failures are loud and typed.
var (
	// ErrChildDied reports that a child's reply pipe closed or its request pipe broke before answering.
	ErrChildDied = errors.New("procs: child exited")
	// ErrTimeout reports that a child failed to answer or exit within its deadline and was killed.
	ErrTimeout = errors.New("procs: child did not answer in time")
	// ErrBusyChild reports starting or stopping a child while a previous Call is still unanswered.
	ErrBusyChild = errors.New("procs: child still has a request outstanding")
	// ErrStepMismatch reports a child reply whose step number or action does not match the request.
	ErrStepMismatch = errors.New("procs: reply does not match the request")
	// ErrStopped reports sending a request to a child that has already been stopped or killed.
	ErrStopped = errors.New("procs: child already stopped")
)

// Child represents a running child process driven by the parent over fd 3/4 pipes.
type Child struct {
	Name    string
	Role    string
	Pid     int
	Timeout time.Duration

	cmd      *exec.Cmd
	requests *os.File
	replies  chan Result
	exited   chan struct{}
	killed   chan struct{}
	killOnce sync.Once
	output   *tailBuffer

	mu          sync.Mutex
	outstanding *Call
	waitErr     error
	readErr     error
	stopped     bool
}

// Call tracks one in-flight request sent to a Child so callers can poll or wait
// for its reply while driving another child process.
type Call struct {
	child   *Child
	request Request
	result  *Result
	err     error
	done    bool
}

// StartChild re-executes the current binary in the named child role.
func StartChild(name, role string, args ...string) (*Child, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return StartCommand(name, role, self, args...)
}

// StartCommand launches executable as a procs child, wires request/reply pipes
// on ExtraFiles (fd 3 and fd 4), and waits for the child's step-0 ready reply
// verifying that the reported PID matches the spawned process.
func StartCommand(name, role, executable string, args ...string) (*Child, error) {
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	replyRead, replyWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		return nil, err
	}
	output := &tailBuffer{limit: 8192}
	cmd := exec.Command(executable, append([]string{ChildMarker, role}, args...)...)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.ExtraFiles = []*os.File{requestRead, replyWrite}
	if err := cmd.Start(); err != nil {
		requestRead.Close()
		requestWrite.Close()
		replyRead.Close()
		replyWrite.Close()
		return nil, fmt.Errorf("start child %s: %w", name, err)
	}
	// Close the child ends in the parent so EOF fires immediately if the child exits.
	requestRead.Close()
	replyWrite.Close()
	child := &Child{
		Name:     name,
		Role:     role,
		Timeout:  DefaultTimeout,
		cmd:      cmd,
		requests: requestWrite,
		replies:  make(chan Result, 1),
		exited:   make(chan struct{}),
		killed:   make(chan struct{}),
		output:   output,
	}
	go child.readReplies(replyRead)
	go func() {
		err := cmd.Wait()
		child.mu.Lock()
		child.waitErr = err
		child.mu.Unlock()
		close(child.exited)
	}()
	ready, err := child.await(Request{Step: 0, Action: ActionReady}, child.Timeout)
	if err != nil {
		child.Kill()
		return nil, err
	}
	if !ready.OK() {
		child.Kill()
		return nil, fmt.Errorf("child %s: bad ready message: %s", name, ready.Outcome())
	}
	pid, err := strconv.Atoi(ready.Value)
	if err != nil || pid != cmd.Process.Pid {
		child.Kill()
		return nil, fmt.Errorf("child %s: ready message names pid %q, the process is %d", name, ready.Value, cmd.Process.Pid)
	}
	child.Pid = pid
	return child, nil
}

func (c *Child) readReplies(r io.ReadCloser) {
	defer r.Close()
	defer close(c.replies)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var result Result
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
			c.mu.Lock()
			c.readErr = fmt.Errorf("malformed reply %q: %w", scanner.Text(), err)
			c.mu.Unlock()
			return
		}
		// Exit promptly if Kill was called while nobody is draining c.replies.
		select {
		case c.replies <- result:
		case <-c.killed:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
	}
}

func (c *Child) await(request Request, timeout time.Duration) (Result, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result, ok := <-c.replies:
		if !ok {
			return Result{}, c.deathError(request)
		}
		if result.Step != request.Step || result.Action != request.Action {
			return Result{}, fmt.Errorf("%w: child %s answered step %d %q to step %d %q", ErrStepMismatch, c.Name, result.Step, result.Action, request.Step, request.Action)
		}
		return result, nil
	case <-timer.C:
		c.Kill()
		return Result{}, fmt.Errorf("%w: child %s, step %d %q, after %v; output: %s", ErrTimeout, c.Name, request.Step, request.Action, timeout, c.output.String())
	}
}

func (c *Child) deathError(request Request) error {
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status := "still running"
	select {
	case <-c.exited:
		status = "exited cleanly"
		if c.waitErr != nil {
			status = c.waitErr.Error()
		}
	default:
	}
	detail := ""
	if c.readErr != nil {
		detail = "; reply pipe: " + c.readErr.Error()
	}
	return fmt.Errorf("%w: child %s (%s) during step %d %q: %s%s; output: %s", ErrChildDied, c.Name, c.Role, request.Step, request.Action, status, detail, c.output.String())
}

// Start sends one numbered request to c without waiting for the reply.
// Step numbers must start at 1 (0 is reserved for ActionReady, -1 for ActionExit),
// and at most one request may be outstanding per child at a time.
func (c *Child) Start(step int, action string, args ...string) (*Call, error) {
	if step <= 0 {
		return nil, fmt.Errorf("procs: step numbers start at 1, got %d", step)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, fmt.Errorf("%w: %s", ErrStopped, c.Name)
	}
	if c.outstanding != nil && !c.outstanding.done {
		return nil, fmt.Errorf("%w: %s has step %d %q", ErrBusyChild, c.Name, c.outstanding.request.Step, c.outstanding.request.Action)
	}
	request := Request{Step: step, Action: action, Args: args}
	line, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := c.requests.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("%w: child %s: write step %d: %v; output: %s", ErrChildDied, c.Name, step, err, c.output.String())
	}
	call := &Call{child: c, request: request}
	c.outstanding = call
	return call, nil
}

// Do sends one numbered request to c and waits up to c.Timeout for its reply.
func (c *Child) Do(step int, action string, args ...string) (Result, error) {
	call, err := c.Start(step, action, args...)
	if err != nil {
		return Result{}, err
	}
	return call.Wait()
}

// Request returns the Request that started this Call.
func (call *Call) Request() Request { return call.request }

// Wait blocks up to the child's Timeout for this Call's reply.
func (call *Call) Wait() (Result, error) {
	return call.WaitFor(call.child.Timeout)
}

// WaitFor blocks up to timeout for this Call's reply, killing the child on
// timeout and caching the outcome so repeated calls return the same Result.
func (call *Call) WaitFor(timeout time.Duration) (Result, error) {
	if call.done {
		if call.result == nil {
			return Result{}, call.err
		}
		return *call.result, call.err
	}
	result, err := call.child.await(call.request, timeout)
	call.finish(result, err)
	return result, err
}

// Answered checks without blocking whether the child has already replied to call.
func (call *Call) Answered() (bool, error) {
	if call.done {
		return true, call.err
	}
	select {
	case result, ok := <-call.child.replies:
		return call.accept(result, ok)
	default:
		return false, nil
	}
}

// AnsweredWithin waits up to d for the child to reply to call. Unlike WaitFor,
// expiry returns (false, nil) without killing the child, allowing callers such
// as Lab.StillWaiting to verify that a blocked SQL statement is still waiting.
func (call *Call) AnsweredWithin(d time.Duration) (bool, error) {
	if d <= 0 {
		return call.Answered()
	}
	if call.done {
		return true, call.err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case result, ok := <-call.child.replies:
		return call.accept(result, ok)
	case <-timer.C:
		return false, nil
	}
}

func (call *Call) accept(result Result, ok bool) (bool, error) {
	if !ok {
		err := call.child.deathError(call.request)
		call.finish(Result{}, err)
		return true, err
	}
	if result.Step != call.request.Step || result.Action != call.request.Action {
		err := fmt.Errorf("%w: child %s answered step %d %q to step %d %q", ErrStepMismatch, call.child.Name, result.Step, result.Action, call.request.Step, call.request.Action)
		call.finish(Result{}, err)
		return true, err
	}
	call.finish(result, nil)
	return true, nil
}

func (call *Call) finish(result Result, err error) {
	call.child.mu.Lock()
	defer call.child.mu.Unlock()
	call.done = true
	call.err = err
	if err == nil {
		call.result = &result
	}
}

// Stop sends ActionExit so the child closes its Actor (including any SQLite
// connection, which checkpoints and cleans up WAL files on last close), waits
// for the child process to exit, and reports any close or exit error. If a
// request is still outstanding or the exit handshake fails, Stop kills the child.
func (c *Child) Stop() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	var pending *Call
	if c.outstanding != nil && !c.outstanding.done {
		pending = c.outstanding
	}
	c.mu.Unlock()
	if pending != nil {
		c.Kill()
		return fmt.Errorf("%w: %s stopped with step %d %q unanswered", ErrBusyChild, c.Name, pending.request.Step, pending.request.Action)
	}
	request := Request{Step: -1, Action: ActionExit}
	line, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := c.requests.Write(append(line, '\n')); err != nil {
		c.Kill()
		return fmt.Errorf("%w: child %s: %v; output: %s", ErrChildDied, c.Name, err, c.output.String())
	}
	result, err := c.await(request, c.Timeout)
	c.requests.Close()
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	if err != nil {
		c.Kill()
		return err
	}
	select {
	case <-c.exited:
	case <-time.After(c.Timeout):
		c.Kill()
		return fmt.Errorf("%w: child %s answered exit but did not exit", ErrTimeout, c.Name)
	}
	c.mu.Lock()
	waitErr := c.waitErr
	c.mu.Unlock()
	if !result.OK() {
		return fmt.Errorf("child %s: close failed: %s", c.Name, result.Outcome())
	}
	if waitErr != nil {
		return fmt.Errorf("child %s: %v; output: %s", c.Name, waitErr, c.output.String())
	}
	return nil
}

// Kill forcefully terminates the child process, unblocks the reply reader,
// closes the request pipe, and waits briefly for the process to be reaped.
func (c *Child) Kill() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.killOnce.Do(func() { close(c.killed) })
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	c.requests.Close()
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
}

// Output returns the last 8 KiB of combined stdout and stderr emitted by c.
func (c *Child) Output() string { return c.output.String() }

// tailBuffer retains the most recent limit bytes written to it so diagnostic
// errors include the child's trailing stderr without unbounded memory growth.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
	lost  int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if extra := len(b.data) - b.limit; extra > 0 {
		b.lost += extra
		b.data = append([]byte(nil), b.data[extra:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.data))
	if text == "" {
		return "(none)"
	}
	if b.lost > 0 {
		return fmt.Sprintf("[%d earlier bytes dropped] %s", b.lost, text)
	}
	return text
}
```

- **Transcript and lab harness (`script.go`, `lab.go`):** `Script` assigns global step numbers across all children and prints aligned transcript lines. `Lab` adds `Must`, `Check`, `Begin`/`StillWaiting`/`Finish`, `Locks` (returning `LockView` with `ProcHolders`), and `Report`.

<!-- include: ../internal/procs/script.go -->
```go
package procs

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Script tracks a set of named Child processes, assigns global step numbers
// across them, and prints a deterministic transcript line as each step finishes.
type Script struct {
	Out     io.Writer
	Timeout time.Duration

	step     int
	children []*Child
}

// NewScript returns a Script writing transcript lines to out with DefaultTimeout.
func NewScript(out io.Writer) *Script {
	return &Script{Out: out, Timeout: DefaultTimeout}
}

// Spawn starts a child in role under a unique logical name and registers it on s.
func (s *Script) Spawn(name, role string, args ...string) (*Child, error) {
	for _, existing := range s.children {
		if existing.Name == name {
			return nil, fmt.Errorf("procs: a child named %q already exists", name)
		}
	}
	child, err := StartChild(name, role, args...)
	if err != nil {
		return nil, err
	}
	child.Timeout = s.Timeout
	s.children = append(s.children, child)
	return child, nil
}

// Children returns a shallow copy of the currently registered children.
func (s *Script) Children() []*Child { return append([]*Child(nil), s.children...) }

// NameOfPid maps a child OS PID back to its logical name in s so lock probes
// and /proc/locks listings print stable names instead of ephemeral PIDs.
func (s *Script) NameOfPid(pid int) (string, bool) {
	for _, child := range s.children {
		if child.Pid == pid {
			return child.Name, true
		}
	}
	return "", false
}

// Step returns the most recently assigned global step number.
func (s *Script) Step() int { return s.step }

// Do sends action to child, waits for its reply, and prints the transcript line.
func (s *Script) Do(child *Child, action string, args ...string) (Result, error) {
	call, err := s.Begin(child, action, args...)
	if err != nil {
		return Result{}, err
	}
	return s.Finish(call)
}

// Begin increments the global step counter and sends action to child without
// waiting for the reply.
func (s *Script) Begin(child *Child, action string, args ...string) (*Call, error) {
	s.step++
	call, err := child.Start(s.step, action, args...)
	if err != nil {
		return nil, err
	}
	return call, nil
}

// Finish waits for a Call started by Begin and prints its transcript line.
func (s *Script) Finish(call *Call) (Result, error) {
	result, err := call.Wait()
	if err != nil {
		return result, err
	}
	s.print(call, result)
	return result, nil
}

func (s *Script) print(call *Call, result Result) {
	if s.Out == nil {
		return
	}
	request := call.Request()
	fmt.Fprintf(s.Out, "step %2d  %-10s %-36s -> %s\n", request.Step, call.child.Name, ActionText(request.Action, request.Args), result.Outcome())
}

// ActionText formats an action name and its arguments for transcript output.
func ActionText(action string, args []string) string {
	if len(args) == 0 {
		return action
	}
	return action + " " + strings.Join(args, " ")
}

// Close stops every child in reverse spawn order so later connections close
// before earlier ones, joining any close errors.
func (s *Script) Close() error {
	var errs []error
	for i := len(s.children) - 1; i >= 0; i-- {
		if err := s.children[i].Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	s.children = nil
	return errors.Join(errs...)
}

// Kill forcefully terminates every child tracked by s.
func (s *Script) Kill() {
	for _, child := range s.children {
		child.Kill()
	}
	s.children = nil
}
```

<!-- include: ../internal/procs/lab.go -->
```go
package procs

import (
	"fmt"
	"io"
	"os"
	"time"
)

// StillWaitingWindow is how long Lab.StillWaiting waits for a reply before
// concluding that a child is genuinely blocked in SQLite's busy-retry loop.
// 250ms is far longer than a local pipe round-trip (so an immediate SQLITE_BUSY
// arrives inside the window and fails the check) and far shorter than the
// 120s busy_timeout used in contention labs.
const StillWaitingWindow = 250 * time.Millisecond

// Lab wraps Script with experiment ergonomics: fatal-on-infrastructure-error
// helpers, claim assertions, lock inspection, and final pass/fail reporting.
type Lab struct {
	Out      io.Writer
	Script   *Script
	failures []string
	exit     func(int)
}

// NewLab creates a Lab that writes transcripts and check results to out and
// exits the process via os.Exit on Fatal or a failing Report.
func NewLab(out io.Writer) *Lab {
	return &Lab{Out: out, Script: NewScript(out), exit: os.Exit}
}

// Fatal kills all children, prints an error line, and exits with status 1.
func (l *Lab) Fatal(what string, err error) {
	l.Script.Kill()
	fmt.Fprintf(l.Out, "FATAL %s: %v\n", what, err)
	l.exit(1)
}

// Check prints one "check ok:" or "check FAILED:" line and records any failed
// claim so Report exits non-zero at the end of the experiment.
func (l *Lab) Check(ok bool, claim string) bool {
	status := "ok"
	if !ok {
		status = "FAILED"
		l.failures = append(l.failures, claim)
	}
	fmt.Fprintf(l.Out, "check %s: %s\n", status, claim)
	return ok
}

// Failures returns a copy of all claim strings that failed Check so far.
func (l *Lab) Failures() []string { return append([]string(nil), l.failures...) }

// Spawn starts a child in role under name, aborting via Fatal on error.
func (l *Lab) Spawn(name, role string, args ...string) *Child {
	child, err := l.Script.Spawn(name, role, args...)
	if err != nil {
		l.Fatal("spawn "+name, err)
	}
	return child
}

// SpawnSQL starts a RoleSQL child configured with options.
func (l *Lab) SpawnSQL(name string, options SQLOptions) *Child {
	return l.Spawn(name, RoleSQL, options.Args()...)
}

// Do runs action on child and returns its Result (whether SQLITE_OK or an error
// code); transport or child-death errors are fatal.
func (l *Lab) Do(child *Child, action string, args ...string) Result {
	result, err := l.Script.Do(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	return result
}

// Must runs action on child and aborts via Fatal unless the result is SQLITE_OK.
func (l *Lab) Must(child *Child, action string, args ...string) Result {
	result := l.Do(child, action, args...)
	if !result.OK() {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), fmt.Errorf("%s %s: %s %s", child.Name, ActionText(action, args), result.Outcome(), result.Error))
	}
	return result
}

// Begin sends action to child without waiting for the reply and logs that the
// step is in flight.
func (l *Lab) Begin(child *Child, action string, args ...string) *Call {
	call, err := l.Script.Begin(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	request := call.Request()
	fmt.Fprintf(l.Out, "step %2d  %-10s %-36s .. sent, not waiting for the answer\n", request.Step, child.Name, ActionText(request.Action, request.Args))
	return call
}

// StillWaiting waits StillWaitingWindow for call to answer, logs whether it
// stayed unanswered, and returns true if the child is still blocked.
func (l *Lab) StillWaiting(call *Call) bool {
	answered, err := call.AnsweredWithin(StillWaitingWindow)
	if err != nil {
		l.Fatal("poll", err)
	}
	request := call.Request()
	state := fmt.Sprintf("no answer after %v", StillWaitingWindow)
	if answered {
		state = "already answered"
	}
	fmt.Fprintf(l.Out, "step %2d  %-10s %-36s .. %s\n", request.Step, call.child.Name, ActionText(request.Action, request.Args), state)
	return !answered
}

// Finish waits for an in-flight Call started with Begin and prints its outcome.
func (l *Lab) Finish(call *Call) Result {
	result, err := l.Script.Finish(call)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", call.Request().Step), err)
	}
	return result
}

// StopChildren cleanly stops every running child, aborting on any close error.
func (l *Lab) StopChildren() {
	if err := l.Script.Close(); err != nil {
		l.Fatal("stop children", err)
	}
}

// PidName returns the logical child name for pid, or "another process" if unknown.
func (l *Lab) PidName(pid int) string {
	if name, ok := l.Script.NameOfPid(pid); ok {
		return name
	}
	return "another process"
}

// LockView combines the portable fcntl(F_GETLK) probe results with Linux's
// per-child /proc/locks entries so callers can assert both views.
type LockView struct {
	Ranges     []RangeState
	Proc       []ProcLock
	ProcListed bool
}

// ProcHolders returns a map of PID -> lock type ("READ" or "WRITE") for every
// /proc/locks entry in v.Proc that covers the entire named range (including
// kernel-merged ranges and locks extending to EOF).
func (v LockView) ProcHolders(name string) map[int]string {
	holders := map[int]string{}
	for _, s := range v.Ranges {
		if s.Range.Name != name {
			continue
		}
		last := s.Range.Start + s.Range.Length - 1
		for _, lock := range v.Proc {
			if lock.Start <= s.Range.Start && (lock.ToEOF || lock.End >= last) {
				holders[lock.Pid] = lock.Type
			}
		}
	}
	return holders
}

// Locks probes ranges on path from the parent process (which holds no SQLite
// connection, so opening and closing path here drops no SQLite locks) and, on
// Linux, also lists and returns the children's /proc/locks entries for path.
func (l *Lab) Locks(label, path string, ranges []LockRange) LockView {
	states, err := ProbeRanges(path, ranges)
	if err != nil {
		l.Fatal("probe locks", err)
	}
	fmt.Fprintf(l.Out, "  %s locks (fcntl F_GETLK from the parent):\n", label)
	for _, line := range FormatRangeStates(states, l.PidName) {
		fmt.Fprintf(l.Out, "    %s\n", line)
	}
	view := LockView{Ranges: states}
	if ProcLocksSupported {
		view.Proc = l.procLocks(label, path, ranges)
		view.ProcListed = true
	}
	return view
}

func (l *Lab) procLocks(label, path string, ranges []LockRange) []ProcLock {
	inode, err := FileInode(path)
	if err != nil {
		l.Fatal("inode", err)
	}
	all, err := ReadProcLocks()
	if err != nil {
		l.Fatal(ProcLocksPath, err)
	}
	names := map[int]string{}
	pids := map[int]bool{}
	for _, child := range l.Script.Children() {
		names[child.Pid] = child.Name
		pids[child.Pid] = true
	}
	mine := FilterProcLocks(all, inode, pids)
	lines := DescribeProcLocks(mine, names, ranges)
	if len(lines) == 0 {
		fmt.Fprintf(l.Out, "  %s locks in %s: none\n", label, ProcLocksPath)
		return mine
	}
	fmt.Fprintf(l.Out, "  %s locks in %s:\n", label, ProcLocksPath)
	for _, line := range lines {
		fmt.Fprintf(l.Out, "    %s\n", line)
	}
	return mine
}

// StateOf looks up the probed LockHolder for the named range inside states.
func StateOf(states []RangeState, name string) (LockHolder, bool) {
	for _, s := range states {
		if s.Range.Name == name {
			return s.Holder, true
		}
	}
	return LockHolder{}, false
}

// WriteLockedBy reports whether name is exclusively write-locked by pid.
func WriteLockedBy(states []RangeState, name string, pid int) bool {
	holder, ok := StateOf(states, name)
	return ok && holder.Held && holder.Write && holder.Pid == pid
}

// ReadLocked reports whether name is held under a shared read lock.
func ReadLocked(states []RangeState, name string) bool {
	holder, ok := StateOf(states, name)
	return ok && holder.Held && !holder.Write
}

// Unlocked reports whether all named ranges (or every range in states if names
// is empty) have no conflicting lock.
func Unlocked(states []RangeState, names ...string) bool {
	for _, s := range states {
		if len(names) > 0 && !contains(names, s.Range.Name) {
			continue
		}
		if s.Holder.Held {
			return false
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Report prints the final summary banner and exits with status 1 if any Check failed.
func (l *Lab) Report(title string) {
	fmt.Fprintln(l.Out)
	if len(l.failures) > 0 {
		fmt.Fprintf(l.Out, "%s FAILED: %d claim(s) did not hold\n", title, len(l.failures))
		for _, f := range l.failures {
			fmt.Fprintf(l.Out, "  - %s\n", f)
		}
		l.exit(1)
		return
	}
	fmt.Fprintf(l.Out, "%s OK\n", title)
}
```

- **Child roles (`sqlrole.go`):** Provides two built-in roles. `SQLRole` opens the database with recipe settings and pins one `*sql.Conn` for the child's life so explicit `BEGIN`/`COMMIT`/`ROLLBACK`, `checkpoint`, `recipe-exec`, `recipe-step`, `stray-close`, and `dbpage-write` (`SELECT data FROM sqlite_dbpage WHERE pgno = ?`) all run on the same SQLite connection. `CopierRole` holds no SQLite connections and executes plain whole-file (`copy`) and byte-range (`copy-range`) reads on live files without disturbing any SQLite process's POSIX locks.

<!-- include: ../internal/procs/sqlrole.go -->
```go
package procs

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Built-in child role names registered in Roles.
const (
	// RoleSQL identifies the role holding one SQLite connection across scripted steps.
	RoleSQL = "sql"
	// RoleCopier identifies the role that copies live files with plain OS reads, holding no SQLite connection.
	RoleCopier = "copier"
)

// Roles is the default role registry passed to Serve by multi-process experiments.
var Roles = map[string]Role{
	RoleSQL:    SQLRole,
	RoleCopier: CopierRole,
}

// SQLOptions configures a RoleSQL child process's SQLite connection and output paths.
type SQLOptions struct {
	Path              string
	JournalMode       string
	BusyTimeout       time.Duration
	WALAutocheckpoint int
	PagesOut          string
}

// Args encodes o into command-line flags accepted by SQLRole.
func (o SQLOptions) Args() []string {
	args := []string{"-db", o.Path, "-journal", o.JournalMode, "-timeout-ms", strconv.FormatInt(o.BusyTimeout.Milliseconds(), 10), "-autocheckpoint", strconv.Itoa(o.WALAutocheckpoint)}
	if o.PagesOut != "" {
		args = append(args, "-pages-out", o.PagesOut)
	}
	return args
}

func parseSQLOptions(args []string) (SQLOptions, error) {
	flags := flag.NewFlagSet(RoleSQL, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options SQLOptions
	var timeoutMS int64
	flags.StringVar(&options.Path, "db", "", "")
	flags.StringVar(&options.JournalMode, "journal", sqlitex.JournalWAL, "")
	flags.Int64Var(&timeoutMS, "timeout-ms", 0, "")
	flags.IntVar(&options.WALAutocheckpoint, "autocheckpoint", 0, "")
	flags.StringVar(&options.PagesOut, "pages-out", "", "")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected role arguments %q", flags.Args())
	}
	if options.Path == "" {
		return options, errors.New("-db is required")
	}
	options.BusyTimeout = time.Duration(timeoutMS) * time.Millisecond
	return options, nil
}

type sqlSession struct {
	options    SQLOptions
	recipe     recipe.Recipe
	sqlOptions sqlitex.Options
	db         *sqlitex.DB
	conn       *sql.Conn
}

// SQLRole opens the target database with explicit recipe settings and pins one
// *sql.Conn for the child's entire lifetime so explicit BEGIN, COMMIT,
// ROLLBACK, and sqlite_dbpage reads all execute on the same SQLite connection.
func SQLRole(ctx context.Context, args []string) (*Actor, error) {
	options, err := parseSQLOptions(args)
	if err != nil {
		return nil, err
	}
	r := recipe.Default().WithSteps(recipe.MaxSteps)
	if options.JournalMode != "" {
		r = r.WithJournalMode(options.JournalMode)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	sqlOptions := r.SQLiteOptions()
	sqlOptions.Conn.BusyTimeout = options.BusyTimeout
	sqlOptions.Conn.WALAutocheckpoint = options.WALAutocheckpoint
	// Leave _txlock deferred so the script controls exact BEGIN DEFERRED / IMMEDIATE / EXCLUSIVE SQL.
	sqlOptions.ImmediateTx = false
	db, err := sqlitex.Open(ctx, options.Path, sqlOptions)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &sqlSession{options: options, recipe: r, sqlOptions: sqlOptions, db: db, conn: conn}
	return &Actor{
		Actions: map[string]Handler{
			"exec":           s.exec,
			"query":          s.query,
			"begin":          s.begin,
			"commit":         s.simple("COMMIT"),
			"rollback":       s.simple("ROLLBACK"),
			"timeout":        s.timeout,
			"checkpoint":     s.checkpoint,
			"recipe-exec":    s.recipeExec,
			"recipe-step":    s.recipeStep,
			"recipe-session": s.recipeSession,
			"user-version":   s.userVersion,
			"stray-close":    s.strayClose,
			"dbpage-write":   s.dbpageWrite,
		},
		Close: s.close,
	}, nil
}

func (s *sqlSession) close() error {
	connErr := s.conn.Close()
	dbErr := s.db.Close()
	return errors.Join(connErr, dbErr)
}

func joinedSQL(args []string) (string, error) {
	statement := strings.TrimSpace(strings.Join(args, " "))
	if statement == "" {
		return "", errors.New("no SQL given")
	}
	return statement, nil
}

func (s *sqlSession) exec(ctx context.Context, args []string) (string, error) {
	statement, err := joinedSQL(args)
	if err != nil {
		return "", err
	}
	_, err = s.conn.ExecContext(ctx, statement)
	return "", err
}

func (s *sqlSession) query(ctx context.Context, args []string) (string, error) {
	statement, err := joinedSQL(args)
	if err != nil {
		return "", err
	}
	var value sql.NullString
	if err := s.conn.QueryRowContext(ctx, statement).Scan(&value); err != nil {
		return "", err
	}
	if !value.Valid {
		return "NULL", nil
	}
	return value.String, nil
}

func (s *sqlSession) begin(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("begin takes one mode: deferred, immediate or exclusive")
	}
	mode := strings.ToUpper(args[0])
	switch mode {
	case "DEFERRED", "IMMEDIATE", "EXCLUSIVE":
	default:
		return "", fmt.Errorf("unknown transaction mode %q", args[0])
	}
	_, err := s.conn.ExecContext(ctx, "BEGIN "+mode)
	return "", err
}

func (s *sqlSession) simple(statement string) Handler {
	return func(ctx context.Context, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("%s takes no arguments", statement)
		}
		_, err := s.conn.ExecContext(ctx, statement)
		return "", err
	}
}

func oneInt(args []string, what string) (int, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("want one %s", what)
	}
	value, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", what, args[0], err)
	}
	return value, nil
}

func (s *sqlSession) timeout(ctx context.Context, args []string) (string, error) {
	ms, err := oneInt(args, "busy timeout in milliseconds")
	if err != nil {
		return "", err
	}
	if ms < 0 {
		return "", fmt.Errorf("busy timeout %d is negative", ms)
	}
	_, err = s.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", ms))
	return "", err
}

func (s *sqlSession) checkpoint(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("checkpoint takes one mode")
	}
	result, err := sqlitex.Checkpoint(ctx, s.conn, "", args[0])
	if err != nil {
		return "", err
	}
	return result.String(), nil
}

func (s *sqlSession) recipeExec(ctx context.Context, args []string) (string, error) {
	k, err := oneInt(args, "recipe step")
	if err != nil {
		return "", err
	}
	step, err := s.recipe.Step(k)
	if err != nil {
		return "", err
	}
	return "", step.ExecIn(ctx, s.conn)
}

func (s *sqlSession) recipeStep(ctx context.Context, args []string) (string, error) {
	k, err := oneInt(args, "recipe step")
	if err != nil {
		return "", err
	}
	step, err := s.recipe.Step(k)
	if err != nil {
		return "", err
	}
	if _, err := s.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return "", err
	}
	if err := step.ExecIn(ctx, s.conn); err != nil {
		_, rollbackErr := s.conn.ExecContext(ctx, "ROLLBACK")
		return "", errors.Join(err, rollbackErr)
	}
	if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, rollbackErr := s.conn.ExecContext(ctx, "ROLLBACK")
		return "", errors.Join(err, rollbackErr)
	}
	return "", nil
}

// recipeSession opens a fresh SingleConnection handle with _txlock=immediate,
// applies recipe step k, and closes that handle while the role's own s.conn
// stays open. Opening a fresh connection per step reproduces the canonical
// recipe's page-1 change counter (enabling exact E1 comparison against
// recipe.StepHashes), while keeping s.conn open prevents sqlite3WalClose
// (src/wal.c:2507-2571) from checkpointing and deleting the WAL on close.
func (s *sqlSession) recipeSession(ctx context.Context, args []string) (string, error) {
	k, err := oneInt(args, "recipe step")
	if err != nil {
		return "", err
	}
	options := s.sqlOptions
	options.SingleConnection = true
	options.ImmediateTx = true
	db, err := sqlitex.Open(ctx, s.options.Path, options)
	if err != nil {
		return "", err
	}
	applyErr := s.recipe.ApplyStep(ctx, db, k)
	return "", errors.Join(applyErr, db.Close())
}

func (s *sqlSession) userVersion(ctx context.Context, args []string) (string, error) {
	if len(args) != 0 {
		return "", errors.New("user-version takes no arguments")
	}
	k, err := recipe.CurrentStep(ctx, s.conn)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(k), nil
}

// strayClose intentionally triggers the POSIX advisory-lock hazard documented in
// src/os_unix.c:1189-1255: opening and closing any file descriptor to an inode
// drops every POSIX lock this process holds on that inode, even though SQLite's
// own connection and unixInodeInfo remain open and unaware.
func (s *sqlSession) strayClose(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("stray-close takes one file: db, -wal, -shm or -journal")
	}
	path, err := sideFile(s.options.Path, args[0])
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	return "", file.Close()
}

func sideFile(dbPath, which string) (string, error) {
	switch which {
	case "db":
		return dbPath, nil
	case "-wal", "-shm", "-journal":
		return dbPath + which, nil
	}
	return "", fmt.Errorf("unknown file %q: want db, -wal, -shm or -journal", which)
}

// dbpageWrite reads pages [first..last] through the sqlite_dbpage virtual table
// on s.conn (so they come from this connection's active read snapshot) and writes
// them at their page offsets into s.options.PagesOut.
func (s *sqlSession) dbpageWrite(ctx context.Context, args []string) (string, error) {
	if len(args) != 2 {
		return "", errors.New("dbpage-write takes a first and a last page number")
	}
	if s.options.PagesOut == "" {
		return "", errors.New("the role was started without -pages-out")
	}
	first, err := strconv.Atoi(args[0])
	if err != nil {
		return "", err
	}
	last, err := strconv.Atoi(args[1])
	if err != nil {
		return "", err
	}
	if first < 1 || last < first {
		return "", fmt.Errorf("page range %d-%d is empty or starts before page 1", first, last)
	}
	file, err := os.OpenFile(s.options.PagesOut, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	writer := bufio.NewWriter(file)
	pageSize := 0
	fail := func(err error) (string, error) {
		file.Close()
		return "", err
	}
	for n := first; n <= last; n++ {
		var page []byte
		if err := s.conn.QueryRowContext(ctx, "SELECT data FROM sqlite_dbpage WHERE pgno = ?", n).Scan(&page); err != nil {
			return fail(fmt.Errorf("page %d: %w", n, err))
		}
		if pageSize == 0 {
			pageSize = len(page)
			if _, err := file.Seek(int64(first-1)*int64(pageSize), io.SeekStart); err != nil {
				return fail(err)
			}
		}
		if len(page) != pageSize || pageSize == 0 {
			return fail(fmt.Errorf("page %d has %d bytes, earlier pages %d", n, len(page), pageSize))
		}
		if _, err := writer.Write(page); err != nil {
			return fail(err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d pages", last-first+1), nil
}

type copierSession struct {
	db  string
	out string
}

// CopierRole starts a child process with no SQLite connections that copies
// whole files ("copy") or byte ranges ("copy-range") of a live database and its
// sidecars into -out. Running in a separate process guarantees its os.Open and
// Close calls never disturb any SQLite process's POSIX locks.
func CopierRole(ctx context.Context, args []string) (*Actor, error) {
	flags := flag.NewFlagSet(RoleCopier, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var s copierSession
	flags.StringVar(&s.db, "db", "", "")
	flags.StringVar(&s.out, "out", "", "")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if s.db == "" || s.out == "" || flags.NArg() != 0 {
		return nil, errors.New("copier needs -db and -out and nothing else")
	}
	return &Actor{Actions: map[string]Handler{
		"copy":       s.copyWhole,
		"copy-range": s.copyRange,
	}}, nil
}

func (s copierSession) target(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("copy target %q must be a plain file name", name)
	}
	return filepath.Join(s.out, name), nil
}

func (s copierSession) copyWhole(ctx context.Context, args []string) (string, error) {
	if len(args) != 2 {
		return "", errors.New("copy takes a source (db, -wal, -shm, -journal) and a target name")
	}
	source, err := sideFile(s.db, args[0])
	if err != nil {
		return "", err
	}
	target, err := s.target(args[1])
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d bytes", len(data)), nil
}

func (s copierSession) copyRange(ctx context.Context, args []string) (string, error) {
	if len(args) != 4 {
		return "", errors.New("copy-range takes a source, a target name, a start offset and an end offset or 'end'")
	}
	source, err := sideFile(s.db, args[0])
	if err != nil {
		return "", err
	}
	target, err := s.target(args[1])
	if err != nil {
		return "", err
	}
	start, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || start < 0 {
		return "", fmt.Errorf("bad start offset %q", args[2])
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	end := info.Size()
	if args[3] != "end" {
		end, err = strconv.ParseInt(args[3], 10, 64)
		if err != nil || end < start {
			return "", fmt.Errorf("bad end offset %q", args[3])
		}
	}
	if end > info.Size() {
		end = info.Size()
	}
	if start > end {
		start = end
	}
	buffer, err := readRange(in, start, end)
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := out.WriteAt(buffer, start); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("bytes %d-%d", start, start+int64(len(buffer))), nil
}

// readRange reads [start, end) from in and slices to the actual byte count so a
// live file that shrank after Stat is not zero-padded on EOF.
func readRange(in io.ReaderAt, start, end int64) ([]byte, error) {
	buffer := make([]byte, end-start)
	n, err := in.ReadAt(buffer, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buffer[:n], nil
}
```

- **Database setup and safe inspection (`database.go`):** `CreateDatabase` builds a fresh closed database at rest; `InspectDatabase` verifies no live sidecars exist, runs `PRAGMA integrity_check` and arbitrary queries, and turns SQLite errors (such as `SQLITE_CORRUPT` or `SQLITE_NOTADB`) into printable text so torn copies can be described cleanly.

<!-- include: ../internal/procs/database.go -->
```go
package procs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

func labOptions(journalMode string) (sqlitex.Options, error) {
	r := recipe.Default().WithJournalMode(journalMode)
	if err := r.Validate(); err != nil {
		return sqlitex.Options{}, err
	}
	options := r.SQLiteOptions()
	options.ImmediateTx = false
	return options, nil
}

// CreateDatabase removes any existing database and sidecars at path, creates a
// fresh database configured with journalMode and recipe settings, executes
// statements, closes the connection, and verifies the file is at rest.
func CreateDatabase(ctx context.Context, path, journalMode string, statements ...string) error {
	if err := recipe.RemoveDatabase(path); err != nil {
		return err
	}
	options, err := labOptions(journalMode)
	if err != nil {
		return err
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	return recipe.CheckAtRest(path)
}

// Inspection holds the result of opening a closed database file, running
// PRAGMA integrity_check, and evaluating caller-supplied queries.
type Inspection struct {
	OpenError string
	Integrity string
	Values    []string
}

func errorText(err error) string {
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("error %s (%d)", sqlitex.ResultCodeName(code), code)
	}
	return "error " + err.Error()
}

// InspectDatabase verifies path has no live sidecar files, opens it with
// journalMode, runs PRAGMA integrity_check and each query, and closes it.
// SQLite errors (such as SQLITE_CORRUPT or SQLITE_NOTADB on a torn copy) are
// captured as text in Inspection rather than returned as errors.
func InspectDatabase(ctx context.Context, path, journalMode string, queries ...string) (Inspection, error) {
	if err := recipe.CheckAtRest(path); err != nil {
		return Inspection{}, err
	}
	options, err := labOptions(journalMode)
	if err != nil {
		return Inspection{}, err
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		if _, isSQLite := sqlitex.ErrorCode(err); !isSQLite {
			return Inspection{}, err
		}
		inspection := Inspection{OpenError: errorText(err), Integrity: "not run"}
		for range queries {
			inspection.Values = append(inspection.Values, "not run")
		}
		return inspection, recipe.CheckAtRest(path)
	}
	inspection := Inspection{Integrity: integrityText(ctx, db)}
	for _, query := range queries {
		var value string
		if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
			value = errorText(err)
		}
		inspection.Values = append(inspection.Values, value)
	}
	if err := db.Close(); err != nil {
		return Inspection{}, err
	}
	return inspection, recipe.CheckAtRest(path)
}

func integrityText(ctx context.Context, db *sqlitex.DB) string {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return errorText(err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return errorText(err)
		}
		lines = append(lines, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return errorText(err)
	}
	return strings.Join(lines, "; ")
}

// IntegrityProblems splits the joined output of PRAGMA integrity_check into
// individual diagnostic lines, skipping SQLite's "*** in database ..." header.
// It returns nil when text is "ok".
func IntegrityProblems(text string) []string {
	if text == "ok" {
		return nil
	}
	var problems []string
	for _, part := range strings.Split(text, "; ") {
		for _, line := range strings.Split(part, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "*** in database") {
				problems = append(problems, line)
			}
		}
	}
	return problems
}

// IntegritySummary condenses PRAGMA integrity_check output to either "ok" or
// "<N> problem(s), the first: <line>".
func IntegritySummary(text string) string {
	problems := IntegrityProblems(text)
	if text == "ok" {
		return "ok"
	}
	if len(problems) == 0 {
		return text
	}
	return fmt.Sprintf("%d problem(s), the first: %s", len(problems), problems[0])
}
```

- **Lock observation (`locks.go`, `lockprobe.go`, `lockprobe_unix.go`, `lockprobe_other.go`, `proclocks.go`, `proclocks_linux.go`, `proclocks_other.go`):** Encodes SQLite's exact lock byte offsets from `src/os.h:160-166`, `src/os_unix.c:4602-4603`, and `src/wal.c:294-299`. `ProbeLock` uses portable `fcntl(F_GETLK)` from the parent process (which holds no SQLite connection, so closing its probe fd drops no SQLite locks). On Linux, `ReadProcLocks` parses `/proc/locks` — including host variants `<none>:0`, `MSNFS`, `UNKNOWN`, and `NONE` from Linux's `fs/locks.c` — and `FilterProcLocks` matches entries by inode and PID so container overlay filesystems match cleanly.

<!-- include: ../internal/procs/locks.go -->
```go
package procs

import (
	"fmt"
	"sort"
	"strings"
)

// POSIX byte-range lock offsets defined by SQLite at version-3.53.4:
// database file lock bytes in src/os.h:160-166 and -shm lock bytes in
// src/os_unix.c:4602-4603 and src/wal.c:294-299.
const (
	// PendingByte is the 1 GiB boundary byte locked exclusively during PENDING (0x40000000).
	PendingByte int64 = 0x40000000
	// ReservedByte is the byte locked exclusively during RESERVED (0x40000001).
	ReservedByte = PendingByte + 1
	// SharedFirst is the first byte of the 510-byte SHARED/EXCLUSIVE range (0x40000002).
	SharedFirst = PendingByte + 2
	// SharedSize is the length of the SHARED lock range (510 bytes, src/os.h:166).
	SharedSize int64 = 510
	// ShmLockBase is the first WAL lock byte in -shm: (22+8)*4 = 120 (src/os_unix.c:4602).
	ShmLockBase int64 = 120
	// ShmLockCount is SQLITE_SHM_NLOCK (8 slots: WRITE, CKPT, RECOVER, READ 0..4).
	ShmLockCount = 8
	// ShmDeadManSwitch is byte 128 of -shm, held shared by every WAL connection (src/os_unix.c:4603).
	ShmDeadManSwitch = ShmLockBase + ShmLockCount
)

// LockRange names one POSIX byte range that SQLite locks on a database or -shm file.
type LockRange struct {
	Name   string
	Start  int64
	Length int64
}

// Text formats r as "byte N" for single-byte locks or "bytes A-B" for multi-byte ranges.
func (r LockRange) Text() string {
	if r.Length == 1 {
		return fmt.Sprintf("byte %d", r.Start)
	}
	return fmt.Sprintf("bytes %d-%d", r.Start, r.Start+r.Length-1)
}

// DatabaseLockRanges returns the three POSIX lock byte ranges on the main
// database file (PENDING, RESERVED, SHARED; src/os.h:160-166).
func DatabaseLockRanges() []LockRange {
	return []LockRange{
		{Name: "PENDING", Start: PendingByte, Length: 1},
		{Name: "RESERVED", Start: ReservedByte, Length: 1},
		{Name: "SHARED", Start: SharedFirst, Length: SharedSize},
	}
}

// ShmLockRanges returns the nine POSIX lock bytes on the WAL -shm file:
// WAL_WRITE_LOCK (120), WAL_CKPT_LOCK (121), WAL_RECOVER_LOCK (122),
// WAL_READ_LOCK(0..4) (123-127), and DMS (128) (src/wal.c:294-299, src/os_unix.c:4602-4603).
func ShmLockRanges() []LockRange {
	ranges := []LockRange{
		{Name: "WAL_WRITE_LOCK", Start: ShmLockBase, Length: 1},
		{Name: "WAL_CKPT_LOCK", Start: ShmLockBase + 1, Length: 1},
		{Name: "WAL_RECOVER_LOCK", Start: ShmLockBase + 2, Length: 1},
	}
	for i := 0; i < 5; i++ {
		ranges = append(ranges, LockRange{Name: fmt.Sprintf("WAL_READ_LOCK(%d)", i), Start: ShmLockBase + 3 + int64(i), Length: 1})
	}
	return append(ranges, LockRange{Name: "DMS", Start: ShmDeadManSwitch, Length: 1})
}

// RangeState pairs a named LockRange with the LockHolder returned by fcntl(F_GETLK).
type RangeState struct {
	Range  LockRange
	Holder LockHolder
}

// ProbeRanges calls ProbeLock for each range in ranges on path.
func ProbeRanges(path string, ranges []LockRange) ([]RangeState, error) {
	states := make([]RangeState, 0, len(ranges))
	for _, r := range ranges {
		holder, err := ProbeLock(path, r.Start, r.Length)
		if err != nil {
			return nil, err
		}
		states = append(states, RangeState{Range: r, Holder: holder})
	}
	return states, nil
}

// Text formats the probed lock state. Because fcntl(F_GETLK) reports only one
// arbitrary holder when multiple processes share a read lock, only write locks
// (which have a unique holder) print the holder's logical name.
func (s RangeState) Text(names func(pid int) string) string {
	switch {
	case !s.Holder.Held:
		return "unlocked"
	case s.Holder.Write:
		return "write-locked by " + names(s.Holder.Pid)
	default:
		return "read-locked"
	}
}

// FormatRangeStates aligns each RangeState into a readable table row.
func FormatRangeStates(states []RangeState, names func(pid int) string) []string {
	lines := make([]string, len(states))
	for i, s := range states {
		lines[i] = fmt.Sprintf("%-18s %-16s %s", s.Range.Name, s.Range.Text(), s.Text(names))
	}
	return lines
}

// DescribeProcLocks formats filtered /proc/locks entries with child names and
// SQLite range labels, sorted lexicographically for deterministic evidence.
// A kernel lock merged across adjacent named bytes is labelled "(A+B)".
func DescribeProcLocks(locks []ProcLock, names map[int]string, ranges []LockRange) []string {
	lines := make([]string, 0, len(locks))
	for _, lock := range locks {
		label := ""
		for _, r := range ranges {
			if lock.Start == r.Start && !lock.ToEOF && lock.Length() == r.Length {
				label = r.Name
			}
		}
		if label == "" {
			label = "(" + rangeNames(lock, ranges) + ")"
		}
		lines = append(lines, fmt.Sprintf("%-10s %s %-5s %-21s %s", names[lock.Pid], lock.Class, lock.Type, lock.RangeText(), label))
	}
	sort.Strings(lines)
	return lines
}

func rangeNames(lock ProcLock, ranges []LockRange) string {
	var covered []string
	for _, r := range ranges {
		if r.Start <= lock.End && lock.Start <= r.Start+r.Length-1 {
			covered = append(covered, r.Name)
		}
	}
	if len(covered) == 0 {
		return "no named range"
	}
	return strings.Join(covered, "+")
}
```

<!-- include: ../internal/procs/lockprobe.go -->
```go
package procs

import "fmt"

// LockHolder describes the conflicting POSIX advisory lock reported by
// fcntl(F_GETLK) for a probed byte range.
type LockHolder struct {
	Held   bool
	Write  bool
	Pid    int
	Start  int64
	Length int64
}

// Kind returns "none", "read", or "write".
func (h LockHolder) Kind() string {
	switch {
	case !h.Held:
		return "none"
	case h.Write:
		return "write"
	default:
		return "read"
	}
}

// Describe returns a human-readable lock summary naming the conflicting holder via names.
func (h LockHolder) Describe(names func(pid int) string) string {
	if !h.Held {
		return "unlocked"
	}
	return fmt.Sprintf("%s lock held by %s", h.Kind(), names(h.Pid))
}
```

<!-- include: ../internal/procs/lockprobe_unix.go -->
```go
//go:build linux || darwin

package procs

import (
	"fmt"
	"os"
	"syscall"
)

// LockProbeSupported reports that fcntl(F_GETLK) lock probing is available on this OS.
const LockProbeSupported = true

// ProbeLock asks the kernel via fcntl(F_GETLK) whether another process holds a
// POSIX advisory lock conflicting with a write lock on [start, start+length) of
// path (the same mechanism SQLite uses in unixIsSharingShmNode, src/os_unix.c:4679-4695).
// Must be called from a process that holds no SQLite connection to path, because
// closing the temporary file descriptor releases all of the caller's POSIX locks
// on that inode (src/os_unix.c:1246-1248).
func ProbeLock(path string, start, length int64) (LockHolder, error) {
	if start < 0 || length <= 0 {
		return LockHolder{}, fmt.Errorf("procs: probe range start %d length %d is not a byte range", start, length)
	}
	// F_WRLCK probes require a writable descriptor on POSIX systems.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return LockHolder{}, err
	}
	defer file.Close()
	// Probing with F_WRLCK conflicts with both F_RDLCK and F_WRLCK held by other processes.
	probe := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: start, Len: length}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &probe); err != nil {
		return LockHolder{}, fmt.Errorf("fcntl(F_GETLK) on %s: %w", path, err)
	}
	switch probe.Type {
	case syscall.F_UNLCK:
		return LockHolder{}, nil
	case syscall.F_RDLCK, syscall.F_WRLCK:
		return LockHolder{Held: true, Write: probe.Type == syscall.F_WRLCK, Pid: int(probe.Pid), Start: probe.Start, Length: probe.Len}, nil
	}
	return LockHolder{}, fmt.Errorf("fcntl(F_GETLK) on %s returned lock type %d", path, probe.Type)
}
```

<!-- include: ../internal/procs/lockprobe_other.go -->
```go
//go:build !linux && !darwin

package procs

import (
	"errors"
	"runtime"
)

// LockProbeSupported is false on platforms without a Linux/Darwin fcntl(F_GETLK) implementation.
const LockProbeSupported = false

// ProbeLock returns an unsupported-platform error on non-Linux, non-Darwin builds.
func ProbeLock(path string, start, length int64) (LockHolder, error) {
	return LockHolder{}, errors.New("procs: fcntl(F_GETLK) probing is implemented for linux and darwin, not " + runtime.GOOS)
}
```

<!-- include: ../internal/procs/proclocks.go -->
```go
package procs

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ProcLocksPath is the Linux procfs file listing every active kernel file lock.
const ProcLocksPath = "/proc/locks"

// NoInodeField is the device:inode token Linux's fs/locks.c (lock_get_status)
// prints for lock entries whose inode pointer is nil.
const NoInodeField = "<none>:0"

// ErrProcLocksFormat is returned when a line of /proc/locks does not match the
// Linux kernel's lock_get_status format.
var ErrProcLocksFormat = errors.New("procs: malformed /proc/locks line")

// ProcLock represents one parsed line from Linux's /proc/locks.
type ProcLock struct {
	ID       int64
	Blocked  bool
	Class    string
	Mode     string
	Type     string
	Pid      int
	DevMajor uint32
	DevMinor uint32
	Inode    uint64
	Start    int64
	End      int64
	ToEOF    bool
}

// Length returns the inclusive byte length of l, or math.MaxInt64 when l.ToEOF is true.
func (l ProcLock) Length() int64 {
	if l.ToEOF {
		return math.MaxInt64
	}
	return l.End - l.Start + 1
}

// RangeText formats l's inclusive byte range as "N", "A-B", or "A-EOF".
func (l ProcLock) RangeText() string {
	if l.ToEOF {
		return fmt.Sprintf("%d-EOF", l.Start)
	}
	if l.Start == l.End {
		return strconv.FormatInt(l.Start, 10)
	}
	return fmt.Sprintf("%d-%d", l.Start, l.End)
}

// ParseProcLocks parses all non-empty lines of /proc/locks text, returning an
// error wrapping ErrProcLocksFormat with the 1-based line number on malformed input.
func ParseProcLocks(text string) ([]ProcLock, error) {
	var locks []ProcLock
	scanner := bufio.NewScanner(strings.NewReader(text))
	number := 0
	for scanner.Scan() {
		number++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		lock, err := ParseProcLockLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number, err)
		}
		locks = append(locks, lock)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return locks, nil
}

// ParseProcLockLine parses one line formatted by Linux fs/locks.c
// (lock_get_status): "ID: [->] CLASS MODE TYPE PID MAJ:MIN:INODE START END|EOF".
// It accepts host-wide kernel variants (<none>:0, MSNFS, UNKNOWN, NONE) so
// unrelated locks on a reader's machine do not abort experiments.
func ParseProcLockLine(line string) (ProcLock, error) {
	bad := func(why string) (ProcLock, error) {
		return ProcLock{}, fmt.Errorf("%w (%s): %q", ErrProcLocksFormat, why, line)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.HasSuffix(fields[0], ":") {
		return bad("no id")
	}
	var lock ProcLock
	id, err := strconv.ParseInt(strings.TrimSuffix(fields[0], ":"), 10, 64)
	if err != nil || id <= 0 {
		return bad("id")
	}
	lock.ID = id
	rest := fields[1:]
	if len(rest) > 0 && rest[0] == "->" {
		lock.Blocked = true
		rest = rest[1:]
	}
	if len(rest) != 7 {
		return bad(fmt.Sprintf("want 7 fields after the id, got %d", len(rest)))
	}
	switch rest[0] {
	case "POSIX", "OFDLCK", "FLOCK", "ACCESS", "LEASE", "DELEG", "UNKNOWN":
		lock.Class = rest[0]
	default:
		return bad("class")
	}
	switch rest[1] {
	case "ADVISORY", "MANDATORY", "ACTIVE", "BREAKING", "BREAKER", "*NOINODE*", "MSNFS", "UNKNOWN":
		lock.Mode = rest[1]
	default:
		return bad("mode")
	}
	switch rest[2] {
	case "READ", "WRITE", "UNLCK", "RW", "NONE":
		lock.Type = rest[2]
	default:
		return bad("type")
	}
	pid, err := strconv.Atoi(rest[3])
	if err != nil {
		return bad("pid")
	}
	lock.Pid = pid
	if rest[4] == NoInodeField {
		return parseLockRange(lock, rest[5], rest[6], bad)
	}
	device := strings.Split(rest[4], ":")
	if len(device) != 3 {
		return bad("device and inode")
	}
	// Major and minor device numbers are printed in hexadecimal; the inode is decimal.
	major, err := strconv.ParseUint(device[0], 16, 32)
	if err != nil {
		return bad("device major")
	}
	minor, err := strconv.ParseUint(device[1], 16, 32)
	if err != nil {
		return bad("device minor")
	}
	inode, err := strconv.ParseUint(device[2], 10, 64)
	if err != nil {
		return bad("inode")
	}
	lock.DevMajor, lock.DevMinor, lock.Inode = uint32(major), uint32(minor), inode
	return parseLockRange(lock, rest[5], rest[6], bad)
}

func parseLockRange(lock ProcLock, startField, endField string, bad func(string) (ProcLock, error)) (ProcLock, error) {
	start, err := strconv.ParseInt(startField, 10, 64)
	if err != nil || start < 0 {
		return bad("start")
	}
	lock.Start = start
	if endField == "EOF" {
		lock.ToEOF = true
		lock.End = math.MaxInt64
		return lock, nil
	}
	end, err := strconv.ParseInt(endField, 10, 64)
	if err != nil || end < start {
		return bad("end")
	}
	lock.End = end
	return lock, nil
}

// FilterProcLocks selects entries matching inode and belonging to a PID in pids.
// It matches by inode and PID rather than device major:minor so container
// overlay filesystems with synthetic device numbers still match cleanly.
func FilterProcLocks(locks []ProcLock, inode uint64, pids map[int]bool) []ProcLock {
	var out []ProcLock
	for _, lock := range locks {
		if lock.Inode == inode && pids[lock.Pid] {
			out = append(out, lock)
		}
	}
	return out
}
```

<!-- include: ../internal/procs/proclocks_linux.go -->
```go
package procs

import (
	"fmt"
	"os"
	"syscall"
)

// ProcLocksSupported reports that Linux /proc/locks inspection is available.
const ProcLocksSupported = true

// ReadProcLocks reads and parses all active kernel lock entries from /proc/locks.
func ReadProcLocks() ([]ProcLock, error) {
	data, err := os.ReadFile(ProcLocksPath)
	if err != nil {
		return nil, err
	}
	return ParseProcLocks(string(data))
}

// FileInode returns the filesystem inode number of path via stat(2).
func FileInode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("procs: no inode for %s", path)
	}
	return stat.Ino, nil
}
```

<!-- include: ../internal/procs/proclocks_other.go -->
```go
//go:build !linux

package procs

import (
	"errors"
	"runtime"
)

// ProcLocksSupported is false on non-Linux operating systems where /proc/locks does not exist.
const ProcLocksSupported = false

// ErrNoProcLocks is returned by ReadProcLocks and FileInode on non-Linux builds.
var ErrNoProcLocks = errors.New("procs: /proc/locks exists only on Linux, not on " + runtime.GOOS)

// ReadProcLocks returns ErrNoProcLocks on non-Linux builds.
func ReadProcLocks() ([]ProcLock, error) { return nil, ErrNoProcLocks }

// FileInode returns ErrNoProcLocks on non-Linux builds.
func FileInode(path string) (uint64, error) { return 0, ErrNoProcLocks }
```

### 5.2 Experiment 6.3 — Torn copy (`6.3-torn-copy`)

- **Claim:** Copying a database file with plain OS reads while SQLite writes to it is not safe: a copy straddling a WAL checkpoint or a rollback commit produces a torn file that fails `PRAGMA integrity_check` or matches no recipe state. In WAL mode, copying only the main `.db` file between commits opens cleanly and passes `integrity_check`, but silently loses every uncheckpointed commit.
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 6.3
  ```

<!-- include: experiments/6.3-torn-copy/main.go -->
```go
// Command 6.3-torn-copy demonstrates why plain file copies of a live SQLite
// database are unsafe: in WAL mode, copying only the main .db file silently
// drops uncheckpointed commits (Part A1), and copying .db across an active
// checkpoint produces a torn, corrupt file (Part A2); in rollback mode, copying
// .db across a live commit tears the copy the same way (Part B).
//
// It prints only its verdict. The steps and what each copy holds go to CSV
// tables that report.sql presents: go run ./cmd/course run 6.3
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	baseStep       = 5
	checkpointedAt = 8
	lastCommitted  = 10
	pageSize       = 4096
)

var describeQueries = []string{
	"PRAGMA user_version",
	"SELECT (SELECT count(*) FROM accounts) || ' accounts, ' || (SELECT count(*) FROM events) || ' events, ' || (SELECT count(*) FROM documents) || ' documents'",
}

// Every step and every examined copy goes to a CSV table in <work dir>/results/;
// part labels the rows written while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		steps, copies, integrityProblems, infoRun *results.Table
	}
)

func openTables() {
	tables.steps = set.Table("steps", "part", "step", "process", "action", "result", "code", "value")
	tables.copies = set.Table("copies", "part", "copy", "bytes", "pages", "open_error", "integrity", "user_version", "contents", "matched_step", "exact")
	tables.integrityProblems = set.Table("integrity_problems", "part", "copy", "n", "problem")
	tables.infoRun = set.Table("info_run", "work_dir", "db_bytes_at_copy")
}

// recorder is a procs.Lab that writes each step and claim to the tables
// instead of printing them.
type recorder struct{ *procs.Lab }

func newRecorder() *recorder {
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil
	return &recorder{lab}
}

func (l *recorder) Must(child *procs.Child, action string, args ...string) procs.Result {
	r := l.Lab.Must(child, action, args...)
	tables.steps.Row(part, r.Step, child.Name, procs.ActionText(action, args), r.CodeName(), r.Code, r.Value)
	return r
}

func (l *recorder) Check(ok bool, claim string) { set.Check(part, ok, claim) }

type copyReport struct {
	name       string
	bytes      int64
	match      recipe.Match
	inspection procs.Inspection
}

// examine verifies that inspecting path does not modify it, matches path against
// the recipe's known states, and records its size, integrity_check result, and counts.
func examine(ctx context.Context, lab *recorder, r recipe.Recipe, path, journal string) copyReport {
	info, err := os.Stat(path)
	if err != nil {
		lab.Fatal("stat", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		lab.Fatal("match", err)
	}
	before, err := recipe.HashFile(path)
	if err != nil {
		lab.Fatal("hash", err)
	}
	inspection, err := procs.InspectDatabase(ctx, path, journal, describeQueries...)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	after, err := recipe.HashFile(path)
	if err != nil {
		lab.Fatal("hash", err)
	}
	if before != after {
		lab.Fatal("inspect", fmt.Errorf("opening %s changed it", filepath.Base(path)))
	}
	report := copyReport{name: filepath.Base(path), bytes: info.Size(), match: match, inspection: inspection}
	problems := procs.IntegrityProblems(inspection.Integrity)
	integrity := inspection.Integrity
	if len(problems) > 0 {
		integrity = fmt.Sprintf("%d problem(s)", len(problems))
	}
	var openError, matched any
	if inspection.OpenError != "" {
		openError = inspection.OpenError
	}
	if match.Found() {
		matched = match.Step
	}
	tables.copies.Row(part, report.name, report.bytes, report.bytes/pageSize, openError, integrity, inspection.Values[0], inspection.Values[1], matched, match.Exact)
	for i, problem := range problems {
		tables.integrityProblems.Row(part, report.name, i+1, problem)
	}
	return report
}

func prepare(ctx context.Context, lab *recorder, r recipe.Recipe, dir, name, journal string) (string, string) {
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	db := filepath.Join(root, "r.db")
	if err := r.Build(ctx, db, baseStep); err != nil {
		lab.Fatal("build", err)
	}
	return db, root
}

// walMode runs Part A: commits steps 6-10 with a PASSIVE checkpoint after step 8,
// inspects a main-file-only copy (A1: valid step 8, commits 9-10 silently lost),
// and splits a copy across a second checkpoint (A2: torn and corrupt).
func walMode(ctx context.Context, lab *recorder, dir string) {
	part = "A"
	r := recipe.Default()
	db, root := prepare(ctx, lab, r, dir, "wal", sqlitex.JournalWAL)
	writer := lab.SpawnSQL("writer", procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL})
	copier := lab.Spawn("copier", procs.RoleCopier, "-db", db, "-out", root)
	for k := baseStep + 1; k <= checkpointedAt; k++ {
		lab.Must(writer, "recipe-step", strconv.Itoa(k))
	}
	lab.Must(writer, "checkpoint", sqlitex.CheckpointPassive)
	for k := checkpointedAt + 1; k <= lastCommitted; k++ {
		lab.Must(writer, "recipe-step", strconv.Itoa(k))
	}
	sizes := lab.Must(copier, "copy", "db", "main-only.db")
	lab.Must(copier, "copy", "-wal", "wal-at-copy")
	part = "A1"
	mainOnly := examine(ctx, lab, r, filepath.Join(root, "main-only.db"), sqlitex.JournalWAL)
	lab.Check(mainOnly.inspection.OpenError == "" && mainOnly.inspection.Integrity == "ok" && mainOnly.inspection.Values[0] == strconv.Itoa(checkpointedAt),
		"the copy of the database file without its -wal opens cleanly and passes integrity_check")
	lab.Check(mainOnly.match.Found() && mainOnly.match.Step == checkpointedAt,
		fmt.Sprintf("but it equals recipe step %d, the last checkpoint, not step %d, the last commit: commits %d-%d are silently missing", checkpointedAt, lastCommitted, checkpointedAt+1, lastCommitted))
	tables.infoRun.Row(sqlitex.DisplayPath(dir), sizes.Value)

	part = "A2"
	half := lab.Must(writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size")
	lab.Must(copier, "copy-range", "db", "torn.db", "0", half.Value)
	result := lab.Must(writer, "checkpoint", sqlitex.CheckpointPassive)
	lab.Check(result.Value == "busy=0 log=40 checkpointed=40", "the checkpoint copied every WAL frame (steps 9-10) into the database file")
	lab.Must(copier, "copy-range", "db", "torn.db", half.Value, "end")
	torn := examine(ctx, lab, r, filepath.Join(root, "torn.db"), sqlitex.JournalWAL)
	lab.Check(torn.inspection.Integrity != "ok" || !torn.match.Found(), "the torn copy fails integrity_check or equals no recipe state")
	lab.StopChildren()
}

// rollbackMode runs Part B: copies the first half of a rollback-mode database,
// commits recipe step 6, copies the second half, and verifies the torn copy fails.
func rollbackMode(ctx context.Context, lab *recorder, dir string) {
	part = "B"
	r := recipe.Default().WithJournalMode(sqlitex.JournalDelete)
	db, root := prepare(ctx, lab, r, dir, "rollback", sqlitex.JournalDelete)
	writer := lab.SpawnSQL("writer", procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete})
	copier := lab.Spawn("copier", procs.RoleCopier, "-db", db, "-out", root)
	half := lab.Must(writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size")
	lab.Must(copier, "copy-range", "db", "torn.db", "0", half.Value)
	lab.Must(writer, "recipe-step", strconv.Itoa(baseStep+1))
	lab.Must(copier, "copy-range", "db", "torn.db", half.Value, "end")
	lab.StopChildren()
	torn := examine(ctx, lab, r, filepath.Join(root, "torn.db"), sqlitex.JournalDelete)
	lab.Check(torn.inspection.Integrity != "ok" || !torn.match.Found(), "the torn copy fails integrity_check or equals no recipe state")
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	lab := newRecorder()
	walMode(ctx, lab, dir)
	rollbackMode(ctx, lab, dir)
	set.Finish("TORN COPY")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/6.3-torn-copy/report.sql -->
```sql
-- Walkthrough of experiment 6.3. Run it with:
--   go run ./cmd/course run 6.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps                one action of the writer or the copier child process
--   copies               one copied database file, examined after the copy
--   integrity_problems   one line of integrity_check output for a copy that fails it
--   checks               every claim the experiment asserted (the runner lists it last)
-- info_run (work dir, database file size at copy time) is shown by info.sql.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  6.3  A plain copy of a live database'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A writer process changes a database built from the recipe to step 5. A'
.print 'copier process copies its files with plain reads, the way cp would, with'
.print 'no SQLite lock. Each copy is then opened read-only, checked with'
.print 'integrity_check, and compared with every known recipe state.'
.print
.print 'What each copy turned out to be:'

SELECT part, copy, pages, coalesce(open_error, '') AS open_error, integrity, user_version,
       coalesce('step ' || matched_step, 'no recipe step') AS equals
FROM copies ORDER BY part;

.print
.print 'In the transcripts below, HALF_QUERY stands for'
.print '  SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size'

.print
.print '── A. WAL mode (wal_autocheckpoint=0) ─────────────────────────────────'
.print
.print 'The writer commits recipe steps 6-8, runs a PASSIVE checkpoint, then commits'
.print 'steps 9 and 10, which stay in -wal. The copier then copies the database'
.print 'file and -wal.'

SELECT step, process,
       CASE WHEN action LIKE 'query SELECT (page_count / 2)%' THEN 'query HALF_QUERY' ELSE action END AS action,
       result, value
FROM steps WHERE part = 'A' ORDER BY step;

.print
.print 'A1. The database file alone (main-only.db), without its -wal. It opens'
.print '    cleanly and passes integrity_check, but it is recipe step 8, the last'
.print '    checkpoint, not step 10, the last commit: commits 9-10 are silently'
.print '    missing. Equal except the change counter at bytes 24-27 and 92-95'
.print '    (exact = false).'

SELECT copy, bytes, integrity, user_version, contents, matched_step, exact
FROM copies WHERE part = 'A1' ORDER BY copy;

.print
.print 'A2. The database file copied in two halves while a checkpoint writes into it'
.print '    between them. The checkpoint (step 11) copies all 40 WAL frames of steps'
.print '    9-10, so the second half comes from a newer database than the first.'

SELECT step, process,
       CASE WHEN action LIKE 'query SELECT (page_count / 2)%' THEN 'query HALF_QUERY' ELSE action END AS action,
       result, value
FROM steps WHERE part = 'A2' ORDER BY step;

.print
.print 'The torn copy fails integrity_check and equals no recipe state.'

SELECT copy, bytes, pages, integrity, user_version, contents,
       coalesce('step ' || matched_step, 'no recipe step') AS equals
FROM copies WHERE part = 'A2' ORDER BY copy;

.print
.print 'Its integrity_check problems, the first 3 and the count:'

SELECT n, problem FROM integrity_problems WHERE part = 'A2' AND n <= 3
UNION ALL
SELECT NULL, '... ' || count(*) || ' problem(s) in all' FROM integrity_problems WHERE part = 'A2'
ORDER BY n NULLS LAST;

.print
.print '── B. Rollback mode (journal_mode=delete) ─────────────────────────────'
.print
.print 'The contrast: the database file copied in two halves while the commit of'
.print 'recipe step 6 (transcript step 15) writes into it between them.'

SELECT step, process,
       CASE WHEN action LIKE 'query SELECT (page_count / 2)%' THEN 'query HALF_QUERY' ELSE action END AS action,
       result, value
FROM steps WHERE part = 'B' ORDER BY step;

.print
.print 'This torn copy also fails integrity_check and equals no recipe state.'

SELECT copy, bytes, pages, integrity, user_version, contents,
       coalesce('step ' || matched_step, 'no recipe step') AS equals
FROM copies WHERE part = 'B' ORDER BY copy;

.print
.print 'Its integrity_check problems:'

SELECT n, problem FROM integrity_problems WHERE part = 'B' ORDER BY n;
```

`course run` also prints the run-dependent values, which are not part of the stored evidence: the work directory, and the size of the database file when the copier copied it.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/6.3-torn-copy/`](evidence/6.3-torn-copy/), and `go run ./cmd/course check evidence ch06` compares a fresh run with them byte for byte.

<!-- include: evidence/6.3-torn-copy.txt -->
```text

════════════════════════════════════════════════════════════════════════
  6.3  A plain copy of a live database
════════════════════════════════════════════════════════════════════════

A writer process changes a database built from the recipe to step 5. A
copier process copies its files with plain reads, the way cp would, with
no SQLite lock. Each copy is then opened read-only, checked with
integrity_check, and compared with every known recipe state.

What each copy turned out to be:
┌──────┬──────────────┬───────┬────────────┬───────────────┬──────────────┬────────────────┐
│ part │     copy     │ pages │ open_error │   integrity   │ user_version │     equals     │
├──────┼──────────────┼───────┼────────────┼───────────────┼──────────────┼────────────────┤
│ A1   │ main-only.db │ 79    │            │ ok            │ 8            │ step 8         │
│ A2   │ torn.db      │ 100   │            │ 25 problem(s) │ 8            │ no recipe step │
│ B    │ torn.db      │ 60    │            │ 6 problem(s)  │ 5            │ no recipe step │
└──────┴──────────────┴───────┴────────────┴───────────────┴──────────────┴────────────────┘

In the transcripts below, HALF_QUERY stands for
  SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size

── A. WAL mode (wal_autocheckpoint=0) ─────────────────────────────────

The writer commits recipe steps 6-8, runs a PASSIVE checkpoint, then commits
steps 9 and 10, which stay in -wal. The copier then copies the database
file and -wal.
┌──────┬─────────┬───────────────────────┬───────────┬───────────────────────────────┐
│ step │ process │        action         │  result   │             value             │
├──────┼─────────┼───────────────────────┼───────────┼───────────────────────────────┤
│ 1    │ writer  │ recipe-step 6         │ SQLITE_OK │                               │
│ 2    │ writer  │ recipe-step 7         │ SQLITE_OK │                               │
│ 3    │ writer  │ recipe-step 8         │ SQLITE_OK │                               │
│ 4    │ writer  │ checkpoint PASSIVE    │ SQLITE_OK │ busy=0 log=61 checkpointed=61 │
│ 5    │ writer  │ recipe-step 9         │ SQLITE_OK │                               │
│ 6    │ writer  │ recipe-step 10        │ SQLITE_OK │                               │
│ 7    │ copier  │ copy db main-only.db  │ SQLITE_OK │ 323584 bytes                  │
│ 8    │ copier  │ copy -wal wal-at-copy │ SQLITE_OK │ 251352 bytes                  │
└──────┴─────────┴───────────────────────┴───────────┴───────────────────────────────┘

A1. The database file alone (main-only.db), without its -wal. It opens
    cleanly and passes integrity_check, but it is recipe step 8, the last
    checkpoint, not step 10, the last commit: commits 9-10 are silently
    missing. Equal except the change counter at bytes 24-27 and 92-95
    (exact = false).
┌──────────────┬────────┬───────────┬──────────────┬───────────────────────────────────────┬──────────────┬───────┐
│     copy     │ bytes  │ integrity │ user_version │               contents                │ matched_step │ exact │
├──────────────┼────────┼───────────┼──────────────┼───────────────────────────────────────┼──────────────┼───────┤
│ main-only.db │ 323584 │ ok        │ 8            │ 50 accounts, 336 events, 25 documents │ 8            │ false │
└──────────────┴────────┴───────────┴──────────────┴───────────────────────────────────────┴──────────────┴───────┘

A2. The database file copied in two halves while a checkpoint writes into it
    between them. The checkpoint (step 11) copies all 40 WAL frames of steps
    9-10, so the second half comes from a newer database than the first.
┌──────┬─────────┬──────────────────────────────────┬───────────┬───────────────────────────────┐
│ step │ process │              action              │  result   │             value             │
├──────┼─────────┼──────────────────────────────────┼───────────┼───────────────────────────────┤
│ 9    │ writer  │ query HALF_QUERY                 │ SQLITE_OK │ 204800                        │
│ 10   │ copier  │ copy-range db torn.db 0 204800   │ SQLITE_OK │ bytes 0-204800                │
│ 11   │ writer  │ checkpoint PASSIVE               │ SQLITE_OK │ busy=0 log=40 checkpointed=40 │
│ 12   │ copier  │ copy-range db torn.db 204800 end │ SQLITE_OK │ bytes 204800-409600           │
└──────┴─────────┴──────────────────────────────────┴───────────┴───────────────────────────────┘

The torn copy fails integrity_check and equals no recipe state.
┌─────────┬────────┬───────┬───────────────┬──────────────┬───────────────────────────────────────┬────────────────┐
│  copy   │ bytes  │ pages │   integrity   │ user_version │               contents                │     equals     │
├─────────┼────────┼───────┼───────────────┼──────────────┼───────────────────────────────────────┼────────────────┤
│ torn.db │ 409600 │ 100   │ 25 problem(s) │ 8            │ 50 accounts, 336 events, 25 documents │ no recipe step │
└─────────┴────────┴───────┴───────────────┴──────────────┴───────────────────────────────────────┴────────────────┘

Its integrity_check problems, the first 3 and the count:
┌───┬───────────────────────────────────────────────┐
│ n │                    problem                    │
├───┼───────────────────────────────────────────────┤
│ 1 │ wrong # of entries in index events_by_account │
│ 2 │ row 335 missing from index events_by_account  │
│ 3 │ row 336 missing from index events_by_account  │
│   │ ... 25 problem(s) in all                      │
└───┴───────────────────────────────────────────────┘

── B. Rollback mode (journal_mode=delete) ─────────────────────────────

The contrast: the database file copied in two halves while the commit of
recipe step 6 (transcript step 15) writes into it between them.
┌──────┬─────────┬──────────────────────────────────┬───────────┬─────────────────────┐
│ step │ process │              action              │  result   │        value        │
├──────┼─────────┼──────────────────────────────────┼───────────┼─────────────────────┤
│ 13   │ writer  │ query HALF_QUERY                 │ SQLITE_OK │ 102400              │
│ 14   │ copier  │ copy-range db torn.db 0 102400   │ SQLITE_OK │ bytes 0-102400      │
│ 15   │ writer  │ recipe-step 6                    │ SQLITE_OK │                     │
│ 16   │ copier  │ copy-range db torn.db 102400 end │ SQLITE_OK │ bytes 102400-245760 │
└──────┴─────────┴──────────────────────────────────┴───────────┴─────────────────────┘

This torn copy also fails integrity_check and equals no recipe state.
┌─────────┬────────┬───────┬──────────────┬──────────────┬───────────────────────────────────────┬────────────────┐
│  copy   │ bytes  │ pages │  integrity   │ user_version │               contents                │     equals     │
├─────────┼────────┼───────┼──────────────┼──────────────┼───────────────────────────────────────┼────────────────┤
│ torn.db │ 245760 │ 60    │ 6 problem(s) │ 5            │ 31 accounts, 251 events, 15 documents │ no recipe step │
└─────────┴────────┴───────┴──────────────┴──────────────┴───────────────────────────────────────┴────────────────┘

Its integrity_check problems:
┌───┬──────────────────────────────────────────────────────────────────┐
│ n │                             problem                              │
├───┼──────────────────────────────────────────────────────────────────┤
│ 1 │ Freelist: freelist leaf count too big on page 35                 │
│ 2 │ Freelist: invalid page number 52                                 │
│ 3 │ Tree 3 page 24 cell 0: overflow list length is 1 but should be 3 │
│ 4 │ Page 29: never used                                              │
│ 5 │ Page 37: never used                                              │
│ 6 │ wrong # of entries in index events_by_account                    │
└───┴──────────────────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A1  ✓  the copy of the database file without its -wal opens cleanly and passes integrity_check
A1  ✓  but it equals recipe step 8, the last checkpoint, not step 10, the last commit: commits 9-10 are silently missing
A2  ✓  the checkpoint copied every WAL frame (steps 9-10) into the database file
A2  ✓  the torn copy fails integrity_check or equals no recipe state
B  ✓  the torn copy fails integrity_check or equals no recipe state

5 of 5 claims hold

Experiment verdict: TORN COPY OK
```

**Reading the evidence:**

- **Part A1 — Main `.db` file copied alone in WAL mode (steps 1–8):** The writer builds from recipe step `5`, commits steps `6–8`, checkpoints all `61` frames (`busy=0 log=61 checkpointed=61`), and commits steps `9–10` (which sit in the `251,352`-byte `-wal`). The copier reads `r.db` (`323,584` bytes, `79` pages) into `main-only.db`. Opening `main-only.db` succeeds with integrity `ok`, `user_version` `8`, `50 accounts, 336 events, 25 documents`, matching recipe step `8`. **Commits 9 and 10 vanished with no error and a passing `integrity_check`.**
- **Part A2 — WAL `.db` file copied across a checkpoint (steps 9–12):** The copier reads the first half (`bytes 0-204800`, 50 pages) of `r.db`. While it pauses, the writer checkpoints steps `9–10` (`busy=0 log=40 checkpointed=40`), growing `r.db` to `100` pages (`409,600` bytes). The copier reads `bytes 204800-409600`. The resulting `torn.db` mixes step-8 pages in its first half with step-10 pages in its second half: `integrity_check` reports `25 problem(s)` (index `events_by_account` missing rows 335, 336, …), `user_version` still reads `8`, and `r.MatchStep` reports `no recipe step`.
- **Part B — Rollback contrast (steps 13–16):** On a step-5 `journal_mode=delete` database (`50` pages), the copier reads `bytes 0-102400` (`25` pages), the writer commits step `6` (growing the file to `60` pages, `245,760` bytes), and the copier reads `bytes 102400-245760`. `torn.db` fails `integrity_check` with `6 problem(s)` (corrupt freelist trunk on page 35, invalid page 52, truncated overflow chain on page 24) and matches `no recipe step`.
- Note on inspection order and safety: in Part A the parent inspects the separate copy files `main-only.db` and `torn.db` while the writer and copier children are still running (then calls `StopChildren`), whereas in Part B it stops the children first. Because the copies are distinct files with distinct inodes, opening them touches no POSIX lock on `r.db`, and `examine` verifies each copy's SHA-256 is unchanged before and after inspection.
- Note on "not safe" versus "always corrupt": because the interleaving is scripted, Experiment 6.3 demonstrates one guaranteed failure per mode rather than a failure rate. A lucky uncoordinated copy can pass `integrity_check` (Part A1 is already a valid stale copy, and adversarial test `TORN-02b` below shows a torn split copy can even pass `integrity_check` while breaking transaction atomicity). Passing once proves luck, not safety.

---

## 6. Evidence and failure cases

A live capture works only when it cooperates with SQLite's WAL snapshot mechanism rather than reading around it.

### 6.1 Experiment 6.4 — Live snapshot (`6.4-live-snapshot`, Rebuild Ladder rung R4)

- **Claim:** Inside a single held read transaction in WAL mode (`BEGIN DEFERRED` followed by a read), reading every page through `sqlite_dbpage` produces a consistent database that passes `integrity_check` and matches (at equality level **E1 bytes**) the recipe run on one connection to that exact snapshot step and checkpointed — even while a writer commits new steps and runs `PASSIVE` checkpoints between chunks. Second, copying `.db` (even torn mid-copy by a checkpoint) and then copying `-wal` while holding a read transaction rebuilds via `internal/walfile` to a consistent recipe state at or after the snapshot (here, the last commit before `-wal` was copied).
- **Evidence form:** `EXACT` (every stored table must match on every run; the run-dependent `info` tables are excluded).
- **Run command** (from the repository root; this starts the `lab` container, runs the experiment in it, and prints the walkthrough; add `-i` to stay in the DuckDB shell afterwards with the tables loaded):
  ```text
  go run ./cmd/course run 6.4
  ```
  (To repeat the experiment holding `BEGIN IMMEDIATE` instead of a read transaction, run it directly in the container with `-hold immediate`: `docker compose run --rm lab go run ./ch06-locking-and-concurrency/experiments/6.4-live-snapshot -hold immediate`.)

<!-- include: experiments/6.4-live-snapshot/main.go -->
```go
// Command 6.4-live-snapshot implements Rebuild Ladder rung R4 (Live snapshot):
// capturing a consistent database from a live WAL writer without stopping writes.
//
// Method 1 reads every page through sqlite_dbpage inside one held read
// transaction while a writer commits and runs PASSIVE checkpoints between chunks,
// proving E1 byte equality against the recipe run to the snapshot step.
// Method 2 copies the database file (torn mid-copy by a PASSIVE checkpoint) and
// then copies -wal while holding a read transaction, proving Chapter 5's WAL
// replayer repairs the torn base to the last commit before the -wal copy.
// The -hold flag selects read (default, BEGIN DEFERRED + read) or immediate
// (BEGIN IMMEDIATE, blocking concurrent commits while checkpoints still run).
//
// It prints only its verdict. The steps and the checked files go to CSV
// tables that report.sql presents: go run ./cmd/course run 6.4
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
)

const (
	baseStep      = 5
	snapshotStep  = 8
	chunks        = 4
	holdRead      = "read"
	holdImmediate = "immediate"
)

// Every step and every checked file goes to a CSV table in <work dir>/results/;
// part labels the rows written while a part runs.
var (
	set         *results.Set
	part        string
	databaseSeq int
	tables      struct {
		settings, steps, pageCapture, databases, comparisons, replay *results.Table
	}
)

func openTables() {
	tables.settings = set.Table("settings", "hold", "base_step", "snapshot_step", "chunks")
	tables.steps = set.Table("steps", "part", "step", "process", "action", "result", "code", "value")
	tables.pageCapture = set.Table("page_capture", "pages", "manifest_sha256")
	tables.databases = set.Table("databases", "seq", "name", "sha256", "integrity", "user_version", "matched_step", "exact")
	tables.comparisons = set.Table("comparisons", "name", "level", "equal", "result")
	tables.replay = set.Table("replay", "wal_ignored_reason", "frames_in_file", "valid_frames", "transactions", "last_commit_frame", "database_pages",
		"applied_frames", "distinct_pages", "skipped_beyond_size", "base_bytes", "output_bytes", "ignored")
}

// recorder is a procs.Lab that writes each step and claim to the tables
// instead of printing them.
type recorder struct{ *procs.Lab }

func newRecorder() *recorder {
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil
	return &recorder{lab}
}

func (l *recorder) Must(child *procs.Child, action string, args ...string) procs.Result {
	r := l.Lab.Must(child, action, args...)
	tables.steps.Row(part, r.Step, child.Name, procs.ActionText(action, args), r.CodeName(), r.Code, r.Value)
	return r
}

func (l *recorder) Check(ok bool, claim string) { set.Check(part, ok, claim) }

type run struct {
	ctx      context.Context
	lab      *recorder
	r        recipe.Recipe
	hold     string
	dir      string
	db       string
	writer   *procs.Child
	capturer *procs.Child
	copier   *procs.Child
	next     int
}

// betweenReads advances the live database between capture chunks: under -hold
// read it commits the next recipe step and runs a PASSIVE checkpoint; under
// -hold immediate it can only checkpoint because the capture holds WAL_WRITE_LOCK.
func (x *run) betweenReads() int {
	if x.hold == holdRead {
		x.lab.Must(x.writer, "recipe-step", strconv.Itoa(x.next))
		x.next++
	}
	before := x.backfill()
	x.lab.Must(x.writer, "checkpoint", sqlitex.CheckpointPassive)
	return x.backfill() - before
}

func (x *run) backfill() int {
	result := x.lab.Must(x.writer, "checkpoint", sqlitex.CheckpointNoop)
	var busy, logFrames, checkpointed int
	if _, err := fmt.Sscanf(result.Value, "busy=%d log=%d checkpointed=%d", &busy, &logFrames, &checkpointed); err != nil {
		x.lab.Fatal("backfill", fmt.Errorf("%q: %w", result.Value, err))
	}
	return checkpointed
}

// beginCapture starts the capture transaction and reads PRAGMA user_version so
// the read snapshot (and its WAL read mark) is actually acquired before copying.
func (x *run) beginCapture() int {
	if x.hold == holdRead {
		x.lab.Must(x.capturer, "begin", "deferred")
	} else {
		x.lab.Must(x.capturer, "begin", "immediate")
	}
	seen := x.lab.Must(x.capturer, "user-version")
	step, err := strconv.Atoi(seen.Value)
	if err != nil {
		x.lab.Fatal("user_version", err)
	}
	return step
}

func (x *run) endCapture(seen int) {
	still := x.lab.Must(x.capturer, "user-version")
	x.lab.Must(x.capturer, "commit")
	x.lab.Check(still.Value == strconv.Itoa(seen), fmt.Sprintf("the capture's transaction saw step %d from its first read to its last", seen))
}

func (x *run) lastCommitted() int { return x.next - 1 }

func (x *run) pageCapture(pages int) int {
	size := (pages + chunks - 1) / chunks
	firstBackfill := -1
	for first := 1; first <= pages; first += size {
		last := min(first+size-1, pages)
		x.lab.Must(x.capturer, "dbpage-write", strconv.Itoa(first), strconv.Itoa(last))
		if last < pages {
			copied := x.betweenReads()
			if firstBackfill < 0 {
				firstBackfill = copied
			}
		}
	}
	return firstBackfill
}

func (x *run) fileAndWALCopy() bool {
	half := x.lab.Must(x.writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size")
	x.lab.Must(x.copier, "copy-range", "db", "copy.db", "0", half.Value)
	copied := x.betweenReads()
	x.lab.Must(x.copier, "copy-range", "db", "copy.db", half.Value, "end")
	if x.hold == holdRead {
		x.lab.Must(x.writer, "recipe-step", strconv.Itoa(x.next))
		x.next++
	}
	x.lab.Must(x.copier, "copy", "-wal", "copy.wal")
	return copied > 0
}

// reference builds the exact ground-truth database for step by applying steps
// 6..step on a single long-lived connection (matching the writer child's change
// counter history) and running a TRUNCATE checkpoint so Method 1 can be checked at E1.
func (x *run) reference(step int) string {
	path := filepath.Join(x.dir, "reference", "r.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(x.ctx, path, baseStep); err != nil {
		x.lab.Fatal("build reference", err)
	}
	options := x.r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(x.ctx, path, options)
	if err != nil {
		x.lab.Fatal("open reference", err)
	}
	for k := baseStep + 1; k <= step; k++ {
		if err := x.r.ApplyStep(x.ctx, db, k); err != nil {
			db.Close()
			x.lab.Fatal("reference step", err)
		}
	}
	if _, err := sqlitex.Checkpoint(x.ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
		db.Close()
		x.lab.Fatal("reference checkpoint", err)
	}
	if err := db.Close(); err != nil {
		x.lab.Fatal("close reference", err)
	}
	return path
}

func (x *run) verify(label, path string) (recipe.Match, procs.Inspection) {
	match, err := x.r.MatchStep(x.ctx, path)
	if err != nil {
		x.lab.Fatal("match", err)
	}
	sum, err := equal.FileSHA256(path)
	if err != nil {
		x.lab.Fatal("hash", err)
	}
	inspection, err := procs.InspectDatabase(x.ctx, path, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		x.lab.Fatal("inspect", err)
	}
	after, err := equal.FileSHA256(path)
	if err != nil || after != sum {
		x.lab.Fatal("inspect", fmt.Errorf("opening %s changed it (%v)", label, err))
	}
	var matched any
	if match.Found() {
		matched = match.Step
	}
	databaseSeq++
	tables.databases.Row(databaseSeq, label, sum, procs.IntegritySummary(inspection.Integrity), inspection.Values[0], matched, match.Exact)
	return match, inspection
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	hold := flag.String("hold", holdRead, "transaction held by the capture connection: read (BEGIN, then a read) or immediate (BEGIN IMMEDIATE)")
	flag.Parse()
	if *hold != holdRead && *hold != holdImmediate {
		fmt.Printf("FATAL -hold must be %s or %s, got %q\n", holdRead, holdImmediate, *hold)
		os.Exit(2)
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	lab := newRecorder()
	x := &run{ctx: ctx, lab: lab, r: recipe.Default(), hold: *hold, dir: dir}
	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	x.db = filepath.Join(live, "r.db")
	if err := x.r.Build(ctx, x.db, baseStep); err != nil {
		lab.Fatal("build", err)
	}
	snapshotFile := filepath.Join(dir, "snapshot.db")
	tables.settings.Row(x.hold, baseStep, snapshotStep, chunks)
	part = "A"
	x.writer = lab.SpawnSQL("writer", procs.SQLOptions{Path: x.db, JournalMode: sqlitex.JournalWAL})
	x.capturer = lab.SpawnSQL("capture", procs.SQLOptions{Path: x.db, JournalMode: sqlitex.JournalWAL, PagesOut: snapshotFile})
	x.copier = lab.Spawn("copier", procs.RoleCopier, "-db", x.db, "-out", dir)
	for k := baseStep + 1; k <= snapshotStep; k++ {
		lab.Must(x.writer, "recipe-step", strconv.Itoa(k))
	}
	x.next = snapshotStep + 1

	part = "B"
	seen := x.beginCapture()
	lab.Check(seen == snapshotStep, fmt.Sprintf("the capture's transaction sees recipe step %d", snapshotStep))
	pageCount := lab.Must(x.capturer, "query", "PRAGMA page_count")
	pages, err := strconv.Atoi(pageCount.Value)
	if err != nil || pages < chunks {
		lab.Fatal("page count", fmt.Errorf("%q", pageCount.Value))
	}
	firstBackfill := x.pageCapture(pages)
	x.endCapture(seen)
	lab.Check(firstBackfill > 0, "the first checkpoint during the page capture copied WAL frames into the database file under the open transaction")

	part = "C"
	secondSnapshot := x.beginCapture()
	lab.Check(secondSnapshot == x.lastCommitted(), fmt.Sprintf("the second transaction sees step %d, the last commit", x.lastCommitted()))
	baseTorn := x.fileAndWALCopy()
	x.endCapture(secondSnapshot)
	if x.hold == holdRead {
		lab.Check(baseTorn, "the checkpoint between the two halves wrote WAL frames into the database file while it was being copied")
	}
	writerSees := lab.Must(x.writer, "user-version")
	if x.hold == holdRead {
		lab.Check(writerSees.Value == strconv.Itoa(x.lastCommitted()) && x.lastCommitted() > snapshotStep,
			fmt.Sprintf("meanwhile the writer committed steps %d-%d", snapshotStep+1, x.lastCommitted()))
	} else {
		lab.Check(writerSees.Value == strconv.Itoa(snapshotStep), "under BEGIN IMMEDIATE the writer committed nothing; it only checkpointed")
	}
	lab.StopChildren()

	part = "D"
	captureDir := filepath.Join(dir, "capture")
	manifest, err := capture.Pages(snapshotFile, captureDir)
	if err != nil {
		lab.Fatal("capture", err)
	}
	rebuilt := filepath.Join(dir, "rebuilt.db")
	if _, err := capture.Rebuild(captureDir, rebuilt); err != nil {
		lab.Fatal("rebuild", err)
	}
	tables.pageCapture.Row(manifest.PageCount(), manifest.SHA256())
	pageMatch, pageInspection := x.verify("rebuilt from the page capture", rebuilt)
	reference := x.reference(snapshotStep)
	x.verify(fmt.Sprintf("reference (recipe to step %d on one connection, then checkpoint)", snapshotStep), reference)
	comparison, err := equal.CompareFiles(rebuilt, reference, 4096)
	if err != nil {
		lab.Fatal("compare", err)
	}
	tables.comparisons.Row("rebuilt vs reference", comparison.Level, comparison.Equal, comparison.String())
	lab.Check(pageInspection.Integrity == "ok" && pageMatch.Found() && pageMatch.Step == snapshotStep && comparison.Equal,
		fmt.Sprintf("the page capture passes integrity_check and its bytes equal the recipe run to exactly step %d and checkpointed (E1)", snapshotStep))

	replayed := filepath.Join(dir, "replayed.db")
	report, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed)
	if err != nil {
		lab.Fatal("replay", err)
	}
	var ignored []string
	for _, line := range report.Lines() {
		if strings.HasPrefix(line, "ignored") {
			ignored = append(ignored, line)
		}
	}
	tables.replay.Row(report.WALIgnoredReason, report.FramesInFile, report.ValidFrames, report.Transactions, report.LastCommitFrame, report.DatabasePages,
		report.AppliedFrames, report.DistinctPages, report.SkippedBeyondSize, report.BaseBytes, report.OutputBytes, strings.Join(ignored, "; "))
	copyMatch, copyInspection := x.verify("rebuilt from the file + -wal copy", replayed)
	lab.Check(copyInspection.Integrity == "ok" && copyMatch.Found() && copyMatch.Step >= secondSnapshot && copyMatch.Step == x.lastCommitted(),
		fmt.Sprintf("the file + -wal copy rebuilds to a recipe state at or after its transaction's snapshot (step %d): step %d, the last commit before the -wal was copied", secondSnapshot, x.lastCommitted()))
	baseMatch, baseInspection := x.verify("the database-file copy on its own", filepath.Join(dir, "copy.db"))
	if x.hold == holdRead {
		lab.Check(baseInspection.Integrity != "ok" || !baseMatch.Found(), "on its own the database-file copy is torn (it fails integrity_check or equals no recipe state); the -wal frames repair it")
	}
	set.Finish("LIVE SNAPSHOT")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/6.4-live-snapshot/report.sql -->
```sql
-- Walkthrough of experiment 6.4. Run it with:
--   go run ./cmd/course run 6.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings       the run: the transaction the capture holds (-hold), recipe steps, chunks
--   steps          one action of the writer, capture or copier child process
--   page_capture   method 1: the captured pages and their manifest
--   comparisons    method 1: the E1 byte comparison of the rebuilt file with the reference
--   replay         method 2: the WAL replay of the copied -wal over the copied database file
--   databases      one database file checked after every process closed
--   checks         every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  6.4  A consistent snapshot of a live database (rung R4)'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A writer process keeps committing recipe steps to a WAL database and'
.print 'running PASSIVE checkpoints (wal_autocheckpoint=0) while a capture process'
.print 'takes a snapshot without stopping it, in two ways. hold is the transaction'
.print 'the capture holds: read (BEGIN, then a read) or immediate (BEGIN IMMEDIATE,'
.print 'under which the writer cannot commit and only checkpoints).'

SELECT hold, base_step, snapshot_step, chunks FROM settings ORDER BY hold;

.print
.print '── A. The live database ───────────────────────────────────────────────'
.print
.print 'The recipe is built to step 5; the writer commits steps 6-8.'

SELECT step, process, action, result, value FROM steps WHERE part = 'A' ORDER BY step;

.print
.print '── B. Method 1: every page through sqlite_dbpage in one transaction ───'
.print
.print 'The capture begins a transaction, reads user_version (step 5: 8), and'
.print 'writes the pages out in 4 chunks. Between chunks the writer commits the next'
.print 'recipe step and runs a PASSIVE checkpoint, with a NOOP checkpoint before and'
.print 'after to read how many frames are backfilled. The first PASSIVE checkpoint'
.print 'copies frames into the database file under the open transaction (checkpointed'
.print 'goes from 0 to 61, steps 9-11).'
.print 'The capture still sees step 8 at the end (step 23).'

SELECT step, process, action, result, value FROM steps WHERE part = 'B' ORDER BY step;

.print
.print '── C. Method 2: plain copies of the database file and its -wal ────────'
.print
.print 'In a new transaction (it sees step 11), the copier copies the database file'
.print 'in two halves; between them the writer commits step 12 and a PASSIVE'
.print 'checkpoint writes frames into the file (checkpointed 61 -> 116, step 31).'
.print 'The writer commits step 13, then the copier copies -wal.'
.print 'HALF_QUERY stands for'
.print '  SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size'

SELECT step, process,
       CASE WHEN action LIKE 'query SELECT (page_count / 2)%' THEN 'query HALF_QUERY' ELSE action END AS action,
       result, value
FROM steps WHERE part = 'C' ORDER BY step;

.print
.print '── D. Checks after every process closed ───────────────────────────────'
.print
.print 'D1. Method 1. The captured pages are rebuilt into a database file and'
.print '    compared byte for byte (E1) with a reference: the recipe run to step 8'
.print '    on one connection, then checkpointed.'

SELECT pages, manifest_sha256[:16] AS manifest_sha256 FROM page_capture ORDER BY pages;

SELECT name, level, equal FROM comparisons ORDER BY name;

.print
.print 'D2. Method 2. The copied -wal is replayed over the copied database file'
.print '    with the WAL replayer of Chapter 5.'

SELECT * FROM (UNPIVOT (SELECT * REPLACE (coalesce(wal_ignored_reason, 'none') AS wal_ignored_reason)
                        FROM replay)
               ON COLUMNS(*)::VARCHAR INTO NAME field VALUE value)
ORDER BY CASE field WHEN 'wal_ignored_reason' THEN 1 WHEN 'frames_in_file' THEN 2 WHEN 'valid_frames' THEN 3
                    WHEN 'transactions' THEN 4 WHEN 'last_commit_frame' THEN 5 WHEN 'database_pages' THEN 6
                    WHEN 'applied_frames' THEN 7 WHEN 'distinct_pages' THEN 8 WHEN 'skipped_beyond_size' THEN 9
                    WHEN 'base_bytes' THEN 10 WHEN 'output_bytes' THEN 11 ELSE 12 END;

.print
.print 'D3. Every checked file. The page capture equals the reference byte for byte'
.print '    (the same sha256) and is recipe step 8. The file + -wal copy is step 13,'
.print '    the last commit before -wal was copied. The database-file copy alone is'
.print '    torn: the -wal frames repair it. exact = false: equal to the recipe step'
.print '    except the change counter at bytes 24-27 and 92-95.'

SELECT split_part(name, ' (', 1) AS name, sha256[:16] AS sha256, user_version, matched_step, exact FROM databases ORDER BY seq;

SELECT split_part(name, ' (', 1) AS name, integrity FROM databases ORDER BY seq;
```

Captured evidence: the walkthrough as the `lab` container rendered it. The walkthrough is of the default `-hold read`. The tables behind it are stored in [`evidence/6.4-live-snapshot/`](evidence/6.4-live-snapshot/), and `go run ./cmd/course check evidence ch06` compares a fresh run with them byte for byte.

<!-- include: evidence/6.4-live-snapshot.txt -->
```text

════════════════════════════════════════════════════════════════════════
  6.4  A consistent snapshot of a live database (rung R4)
════════════════════════════════════════════════════════════════════════

A writer process keeps committing recipe steps to a WAL database and
running PASSIVE checkpoints (wal_autocheckpoint=0) while a capture process
takes a snapshot without stopping it, in two ways. hold is the transaction
the capture holds: read (BEGIN, then a read) or immediate (BEGIN IMMEDIATE,
under which the writer cannot commit and only checkpoints).
┌──────┬───────────┬───────────────┬────────┐
│ hold │ base_step │ snapshot_step │ chunks │
├──────┼───────────┼───────────────┼────────┤
│ read │ 5         │ 8             │ 4      │
└──────┴───────────┴───────────────┴────────┘

── A. The live database ───────────────────────────────────────────────

The recipe is built to step 5; the writer commits steps 6-8.
┌──────┬─────────┬───────────────┬───────────┬───────┐
│ step │ process │    action     │  result   │ value │
├──────┼─────────┼───────────────┼───────────┼───────┤
│ 1    │ writer  │ recipe-step 6 │ SQLITE_OK │       │
│ 2    │ writer  │ recipe-step 7 │ SQLITE_OK │       │
│ 3    │ writer  │ recipe-step 8 │ SQLITE_OK │       │
└──────┴─────────┴───────────────┴───────────┴───────┘

── B. Method 1: every page through sqlite_dbpage in one transaction ───

The capture begins a transaction, reads user_version (step 5: 8), and
writes the pages out in 4 chunks. Between chunks the writer commits the next
recipe step and runs a PASSIVE checkpoint, with a NOOP checkpoint before and
after to read how many frames are backfilled. The first PASSIVE checkpoint
copies frames into the database file under the open transaction (checkpointed
goes from 0 to 61, steps 9-11).
The capture still sees step 8 at the end (step 23).
┌──────┬─────────┬─────────────────────────┬───────────┬────────────────────────────────┐
│ step │ process │         action          │  result   │             value              │
├──────┼─────────┼─────────────────────────┼───────────┼────────────────────────────────┤
│ 4    │ capture │ begin deferred          │ SQLITE_OK │                                │
│ 5    │ capture │ user-version            │ SQLITE_OK │ 8                              │
│ 6    │ capture │ query PRAGMA page_count │ SQLITE_OK │ 79                             │
│ 7    │ capture │ dbpage-write 1 20       │ SQLITE_OK │ 20 pages                       │
│ 8    │ writer  │ recipe-step 9           │ SQLITE_OK │                                │
│ 9    │ writer  │ checkpoint NOOP         │ SQLITE_OK │ busy=0 log=84 checkpointed=0   │
│ 10   │ writer  │ checkpoint PASSIVE      │ SQLITE_OK │ busy=0 log=84 checkpointed=61  │
│ 11   │ writer  │ checkpoint NOOP         │ SQLITE_OK │ busy=0 log=84 checkpointed=61  │
│ 12   │ capture │ dbpage-write 21 40      │ SQLITE_OK │ 20 pages                       │
│ 13   │ writer  │ recipe-step 10          │ SQLITE_OK │                                │
│ 14   │ writer  │ checkpoint NOOP         │ SQLITE_OK │ busy=0 log=101 checkpointed=61 │
│ 15   │ writer  │ checkpoint PASSIVE      │ SQLITE_OK │ busy=0 log=101 checkpointed=61 │
│ 16   │ writer  │ checkpoint NOOP         │ SQLITE_OK │ busy=0 log=101 checkpointed=61 │
│ 17   │ capture │ dbpage-write 41 60      │ SQLITE_OK │ 20 pages                       │
│ 18   │ writer  │ recipe-step 11          │ SQLITE_OK │                                │
│ 19   │ writer  │ checkpoint NOOP         │ SQLITE_OK │ busy=0 log=116 checkpointed=61 │
│ 20   │ writer  │ checkpoint PASSIVE      │ SQLITE_OK │ busy=0 log=116 checkpointed=61 │
│ 21   │ writer  │ checkpoint NOOP         │ SQLITE_OK │ busy=0 log=116 checkpointed=61 │
│ 22   │ capture │ dbpage-write 61 79      │ SQLITE_OK │ 19 pages                       │
│ 23   │ capture │ user-version            │ SQLITE_OK │ 8                              │
│ 24   │ capture │ commit                  │ SQLITE_OK │                                │
└──────┴─────────┴─────────────────────────┴───────────┴────────────────────────────────┘

── C. Method 2: plain copies of the database file and its -wal ────────

In a new transaction (it sees step 11), the copier copies the database file
in two halves; between them the writer commits step 12 and a PASSIVE
checkpoint writes frames into the file (checkpointed 61 -> 116, step 31).
The writer commits step 13, then the copier copies -wal.
HALF_QUERY stands for
  SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size
┌──────┬─────────┬──────────────────────────────────┬───────────┬─────────────────────────────────┐
│ step │ process │              action              │  result   │              value              │
├──────┼─────────┼──────────────────────────────────┼───────────┼─────────────────────────────────┤
│ 25   │ capture │ begin deferred                   │ SQLITE_OK │                                 │
│ 26   │ capture │ user-version                     │ SQLITE_OK │ 11                              │
│ 27   │ writer  │ query HALF_QUERY                 │ SQLITE_OK │ 221184                          │
│ 28   │ copier  │ copy-range db copy.db 0 221184   │ SQLITE_OK │ bytes 0-221184                  │
│ 29   │ writer  │ recipe-step 12                   │ SQLITE_OK │                                 │
│ 30   │ writer  │ checkpoint NOOP                  │ SQLITE_OK │ busy=0 log=145 checkpointed=61  │
│ 31   │ writer  │ checkpoint PASSIVE               │ SQLITE_OK │ busy=0 log=145 checkpointed=116 │
│ 32   │ writer  │ checkpoint NOOP                  │ SQLITE_OK │ busy=0 log=145 checkpointed=116 │
│ 33   │ copier  │ copy-range db copy.db 221184 end │ SQLITE_OK │ bytes 221184-438272             │
│ 34   │ writer  │ recipe-step 13                   │ SQLITE_OK │                                 │
│ 35   │ copier  │ copy -wal copy.wal               │ SQLITE_OK │ 688072 bytes                    │
│ 36   │ capture │ user-version                     │ SQLITE_OK │ 11                              │
│ 37   │ capture │ commit                           │ SQLITE_OK │                                 │
│ 38   │ writer  │ user-version                     │ SQLITE_OK │ 13                              │
└──────┴─────────┴──────────────────────────────────┴───────────┴─────────────────────────────────┘

── D. Checks after every process closed ───────────────────────────────

D1. Method 1. The captured pages are rebuilt into a database file and
    compared byte for byte (E1) with a reference: the recipe run to step 8
    on one connection, then checkpointed.
┌───────┬──────────────────┐
│ pages │ manifest_sha256  │
├───────┼──────────────────┤
│ 79    │ fdacd7ac2cf9defb │
└───────┴──────────────────┘
┌──────────────────────┬──────────┬───────┐
│         name         │  level   │ equal │
├──────────────────────┼──────────┼───────┤
│ rebuilt vs reference │ E1 bytes │ true  │
└──────────────────────┴──────────┴───────┘

D2. Method 2. The copied -wal is replayed over the copied database file
    with the WAL replayer of Chapter 5.
┌─────────────────────┬───────────────┐
│        field        │     value     │
├─────────────────────┼───────────────┤
│ wal_ignored_reason  │ none          │
│ frames_in_file      │ 167           │
│ valid_frames        │ 167           │
│ transactions        │ 8             │
│ last_commit_frame   │ 167           │
│ database_pages      │ 124           │
│ applied_frames      │ 167           │
│ distinct_pages      │ 92            │
│ skipped_beyond_size │ 0             │
│ base_bytes          │ 438272        │
│ output_bytes        │ 507904        │
│ ignored             │ ignored: none │
└─────────────────────┴───────────────┘

D3. Every checked file. The page capture equals the reference byte for byte
    (the same sha256) and is recipe step 8. The file + -wal copy is step 13,
    the last commit before -wal was copied. The database-file copy alone is
    torn: the -wal frames repair it. exact = false: equal to the recipe step
    except the change counter at bytes 24-27 and 92-95.
┌───────────────────────────────────┬──────────────────┬──────────────┬──────────────┬───────┐
│               name                │      sha256      │ user_version │ matched_step │ exact │
├───────────────────────────────────┼──────────────────┼──────────────┼──────────────┼───────┤
│ rebuilt from the page capture     │ 1558477d01bc85e9 │ 8            │ 8            │ false │
│ reference                         │ 1558477d01bc85e9 │ 8            │ 8            │ false │
│ rebuilt from the file + -wal copy │ fbe88f844fab4985 │ 13           │ 13           │ false │
│ the database-file copy on its own │ 04c300a81ff19200 │ 5            │              │ false │
└───────────────────────────────────┴──────────────────┴──────────────┴──────────────┴───────┘
┌───────────────────────────────────┬────────────────────────────────────────────────────────────────────────────┐
│               name                │                                 integrity                                  │
├───────────────────────────────────┼────────────────────────────────────────────────────────────────────────────┤
│ rebuilt from the page capture     │ ok                                                                         │
│ reference                         │ ok                                                                         │
│ rebuilt from the file + -wal copy │ ok                                                                         │
│ the database-file copy on its own │ 10 problem(s), the first: Freelist: freelist leaf count too big on page 35 │
└───────────────────────────────────┴────────────────────────────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

B  ✓  the capture's transaction sees recipe step 8
B  ✓  the capture's transaction saw step 8 from its first read to its last
B  ✓  the first checkpoint during the page capture copied WAL frames into the database file under the open transaction
C  ✓  the second transaction sees step 11, the last commit
C  ✓  the capture's transaction saw step 11 from its first read to its last
C  ✓  the checkpoint between the two halves wrote WAL frames into the database file while it was being copied
C  ✓  meanwhile the writer committed steps 9-13
D  ✓  the page capture passes integrity_check and its bytes equal the recipe run to exactly step 8 and checkpointed (E1)
D  ✓  the file + -wal copy rebuilds to a recipe state at or after its transaction's snapshot (step 11): step 13, the last commit before the -wal was copied
D  ✓  on its own the database-file copy is torn (it fails integrity_check or equals no recipe state); the -wal frames repair it

10 of 10 claims hold

Experiment verdict: LIVE SNAPSHOT OK
```

**Reading the evidence:**

- **Method 1 — Page capture via `sqlite_dbpage` inside one read transaction (steps 4–24):**
  - After the writer commits steps `6–8` (`wal_autocheckpoint=0`), `capture` runs `BEGIN DEFERRED` and `user-version` (`8`). That first read acquires `WAL_READ_LOCK(1)` and pins frame `61` (step 8's commit frame). `PRAGMA page_count` reports `79` pages.
  - `capture` reads pages in four chunks (`1..20`, `21..40`, `41..60`, `61..79`) via `SELECT data FROM sqlite_dbpage WHERE pgno = ?`. Between chunks, the writer commits steps `9`, `10`, and `11` (`log` grows `84` → `101` → `116`) and runs `PASSIVE` checkpoints.
  - Look closely at steps 9–21: the first `PASSIVE` checkpoint advances `checkpointed` from `0` to `61` **while the capture is between page 20 and page 21**, writing into `.db` underneath the open capture. The next two `PASSIVE` checkpoints stay pinned at `checkpointed=61` because `capture` holds its read mark at frame `61`. At step 23, `capture` still sees `user-version = 8`.
  - After all processes close, the 79 captured pages (`manifest sha256 fdacd7ac2cf9defb`) are rebuilt through Chapter 2's `capture.Rebuild` into `rebuilt.db` (sha256 `1558477d01bc85e9…`, integrity `ok`, `user_version` `8`; D1 and D3). Comparing against the reference database built by applying steps `6–8` on one connection and running `wal_checkpoint(TRUNCATE)` yields **E1 bytes: equal** (`sha256 1558477d01bc85e9ec8dfa29e63f245bc427ee31a6da515ffc3f75a892579529`). (Using one connection for steps `6–8` matches the writer child's change-counter history so the proof holds at E1 bytes, not just the masked step match.)
- **Method 2 — Plain `.db` + `-wal` copy under a held read transaction (steps 25–38 and D2–D3):**
  - `capture` opens a second read transaction (`user-version = 11`, pinning frame `116`). The copier copies the first half of `r.db` (`bytes 0-221184`). Between the two halves, the writer commits step `12` and runs a `PASSIVE` checkpoint that advances `checkpointed` from `61` to `116` — writing into `r.db` **while `copy.db` is half copied**. The copier copies the rest (`bytes 221184-438272`), the writer commits step `13`, and the copier copies the `-wal` (`688,072` bytes, `167` frames).
  - Inspecting `copy.db` on its own (the last row of the two D3 tables) shows a severe surprise: **integrity `10 problem(s)`**, **`user_version` `5`**, and **no recipe step**. Why is `copy.db`'s page 1 still step `5` even though checkpoints reported `checkpointed=61` (step 8) and `checkpointed=116` (step 11)? Because `walIteratorNext` (`src/wal.c:1764-1792`) yields each page in `-wal` **once, at its newest frame**, and `walCheckpoint` (`src/wal.c:2312-2314`) skips any page whose newest frame exceeds `mxSafeFrame`. Page 1 was modified by step 9 (frame > 61) and step 12 (frame > 116), so neither checkpoint copied page 1 into `.db` while other pages *were* copied. **A checkpoint's `checkpointed` (`nBackfill`) count does not mean `.db` alone holds that state:** between complete checkpoints, `.db` is a mixture of states that only `-wal` makes whole.
  - Because `capture` kept the WAL from restarting, all `167` frames (steps `6–13`, `92` distinct pages) remained in `copy.wal`. Replaying `copy.db` + `copy.wal` with `walfile.ReplayFiles` overwrites every torn page in `copy.db` and extends the file from `438,272` bytes to `507,904` bytes (`124` pages), matching **recipe step 13** (integrity `ok`, `user_version` `13`, sha256 `fbe88f844fab4985`) — the last commit before `-wal` was copied.
- **What `-hold immediate` shows:** Running `6.4-live-snapshot -hold immediate` holds `BEGIN IMMEDIATE` (`WAL_WRITE_LOCK`) during both captures. Writers cannot commit (`SQLITE_BUSY`), yet another connection's `PASSIVE` checkpoints **still run** (copying `61` frames into `.db` during Method 1, and tearing `.db` alone in adversarial test `SNAP-03b`). Holding `BEGIN IMMEDIATE` does not make a bare `.db` copy safe against concurrent checkpoints; a file + WAL copy stays valid only because `-wal` is copied after `.db` and replayed whole.
- **A subtle boundary on `WAL_READ_LOCK(0)` (discovered by adversarial test `SNAP-04`):** A read transaction that starts on an already fully checkpointed WAL (`nBackfill == mxFrame`) takes `WAL_READ_LOCK(0)` (byte `123`) rather than a mark in `1..4`. When the next commit runs, `walRestartLog` (`src/wal.c:3885`) locks only `WAL_READ_LOCK(1..4)` and therefore **restarts the WAL under the open reader** (`salt-1 + 1`, `nBackfill = 0`). That is safe for the reader itself — it reads only `.db`, and `walCheckpoint` (`src/wal.c:2254-2260`) cannot backfill a single page while `WAL_READ_LOCK(0)` is held — so Method 1 still rebuilds E1-identically. However, any WAL-shipping design (Chapter 9, Rung R6) that relies on a held read mark to stop WAL restarts must acquire that read mark while uncheckpointed frames exist in `-wal`.

### 6.2 Failure cases summary

The experiments and adversarial tests establish five concrete failure modes a platform must prevent:

1. **Stray `os.Open` + `Close` inside a serving process (Experiment 6.1, `LOCK-01`–`LOCK-06`).** Drops all of the process's POSIX locks on that inode. In rollback mode (Part A) and in two of three WAL states (Parts C and D), a commit reported as `SQLITE_OK` is silently lost while `integrity_check` still reports `ok`.
2. **Deferred read-then-write upgrades under contention (Experiment 6.2 C3, `BUSY-06`).** Even with `busy_timeout=120000`, `btreeBeginTrans` (`src/btree.c:3740-3741`) never calls the busy handler once `inTransaction != TRANS_NONE`, failing immediately with `SQLITE_BUSY (5)` and then `SQLITE_BUSY_SNAPSHOT (517)`.
3. **Plain file copies and `integrity_check` blind spots (Experiment 6.3, `TORN-02a`, `TORN-02b`).** Copying `.db` without `-wal` silently drops commits (6.3 A1); copying across writes tears the file (6.3 A2, B); and a torn split-half copy across an atomic two-table transfer can **pass `PRAGMA integrity_check=ok`** while violating transaction atomicity (`500 + 600 = 1100 != 1000`, `TORN-02b`).
4. **Splitting a `sqlite_dbpage` capture across two read transactions (`SNAP-01a`, `SNAP-01b`).** Produces either a corrupt file (`28` integrity problems in `SNAP-01a`) or a file with constant `page_count` and `integrity_check=ok` that silently breaks atomicity (`500 + 600 = 1100`, `SNAP-01b`).
5. **Holding `BEGIN DEFERRED` without an initial read (`SNAP-05`).** Because `BEGIN DEFERRED` emits no `OP_Transaction` (`src/build.c:5245-5274`), it pins no WAL read mark. A concurrent `checkpoint(RESTART)` + commit succeeds (`busy=0`) and breaks a subsequent `.db` + `-wal` replay (`SQLITE_CORRUPT (11)`).

### What attacked this chapter

<!-- adversarial-results -->

Five adversarial Go programs (`49` attacks total, driven by [`run-all.sh`](../agent_work/ch06/adversarial/run-all.sh)) attacked this chapter's claims and shared code against the pinned SQLite `3.53.4` build:

- [`locks-process`](../agent_work/ch06/adversarial/locks-process/main.go) (`LOCK-01`–`LOCK-06`, 12 attacks): stray close of `-wal`, SQLite 3.51+ `unixIsSharingShmNode` guarding `PRAGMA journal_mode=delete` after a stray close of `.db`, stray close of `-shm` releasing a reader's `WAL_READ_LOCK`, two connections inside one process (verifying 0 extra `F_SETLK` calls, deferred fd parking via `setPendingFd`, intra-process exclusion, and whole-process lock loss on a stray `.db` close), cross-build interop with the C `sqlite3` CLI as both contender and holder in rollback and WAL modes, multi-page indexed rollback stray-close writes, and active-WAL stray-close of `-shm`.
- [`busy-contention`](../agent_work/ch06/adversarial/busy-contention/main.go) (`BUSY-01a`–`BUSY-09`, 12 attacks): three queued WAL writers plus a zero-timeout reader across four processes, three writers alternating recipe steps `6–17`, three-process rollback `PENDING` lock blocking a third-process reader while an existing reader continues, C `sqlite3` CLI busy waits and immediate `SQLITE_BUSY_SNAPSHOT`, `100` full scripted re-runs of every Experiment 6.2 cell (`100/100` exact in A, B, C1, C2, C3, and D), `BEGIN EXCLUSIVE` lock probing under `locking_mode=NORMAL` vs `locking_mode=EXCLUSIVE`, rollback-mode deferred upgrade skipping `busy_timeout`, genuine busy-handler sleep and expiration at `100ms` (`102ms`), negative control for `517` when a concurrent writer rolls back, and falsifying `Lab.StillWaiting` plus `strace` counting `EAGAIN` on `-shm` byte `120`.
- [`torn-copy-attacks`](../agent_work/ch06/adversarial/torn-copy-attacks/main.go) (`TORN-01a`–`TORN-05`, 8 attacks): unscripted 4 KiB concurrent copies in rollback (`5/5` corrupt/no-match) and WAL with `wal_autocheckpoint=10` (`3/3` corrupt/no-match), lucky split copy passing `integrity_check=ok` stale, split copy across an atomic two-table transfer passing `integrity_check=ok` with broken sum `1100`, WAL main-file-only copy under default `wal_autocheckpoint=1000`, naive `.db` + `-wal` copy without a held transaction racing `TRUNCATE` and `RESTART`, and verifier corruption detection.
- [`live-snapshot-attacks`](../agent_work/ch06/adversarial/live-snapshot-attacks/main.go) (`SNAP-01a`–`SNAP-06`, 9 attacks): `sqlite_dbpage` capture split across two read transactions (both growing and constant `page_count`), a third independent process running `PASSIVE`, `FULL`, `RESTART`, and `TRUNCATE` checkpoints mid-copy, `6.4-live-snapshot -hold immediate` and a third-process `PASSIVE` checkpoint tearing `.db` under `BEGIN IMMEDIATE`, a slow capture reader starting on a fully checkpointed WAL (`wal_autocheckpoint=20`, steps `6–18`), holding `BEGIN DEFERRED` with no initial `SELECT`, and Method 2 ignoring `48` spilled uncommitted tail frames after a rollback.
- [`procs-and-codes`](../agent_work/ch06/adversarial/procs-and-codes/main.go) (`PROCS-01`–`PROCS-06`, 8 attacks): every `internal/procs` loud-failure sentinel (`ErrChildDied`, `ErrTimeout` + reap, `ErrBusyChild`, duplicate name, `ErrStopped`), live host `/proc/locks` and kernel line variants (`OFDLCK`, `->`, `FLOCK`, `LEASE`, `EOF`, `<none>:0`, `MSNFS`, `UNKNOWN`), merged and `0-EOF` lock formatting, `F_GETLK` self-process blind spot and invalid ranges, `sqlitex.ResultCodeName` and `DisplayPath` symlink escape checks, pin-mismatch refusal across Experiments 6.1–6.4, and `InspectDatabase`/`CopierRole` safety boundaries.

**Run 1** ([`results-run1.txt`](../agent_work/ch06/adversarial/results-run1.txt)) produced **43 HELD, 6 BROKEN** (`LOCK-04b`, `LOCK-06`, `BUSY-05b`, `BUSY-09`, `SNAP-04`, `PROCS-02c`). Two broken attacks exposed real defects that were fixed in the repair round (alongside code-review fixes adding `/proc/locks` assertions via `LockView.ProcHolders` in 6.1, slicing short reads in `copierSession.copyRange`, and cleaning up `Child.Stop` / `readReplies`):

- `BUSY-09` (with Review 1 Finding 1): `Lab.StillWaiting` previously used a non-blocking 0 ns poll (`Answered`), so it returned `true` even for a `busy_timeout=0` child that failed immediately, and Experiment 6.2 Part E regenerated recipe steps before `BEGIN IMMEDIATE`, leaving some rounds uncontended (`24` `EAGAIN`s total). Fixed by adding `Call.AnsweredWithin(250ms)` and sending `begin immediate` as the waiting call in Part E; after repair, `still_waiting_on_timeout0=false` and `strace` counted `82`–`87` `EAGAIN`s on `-shm` byte `120`.
- `PROCS-02c` (with Review 1 Finding 4): `ParseProcLockLine` rejected valid Linux `fs/locks.c` host entries (`<none>:0`, `MSNFS`, `UNKNOWN`, `NONE`), which could abort Experiment 6.1 on a reader's host. Fixed to accept those variants while still rejecting malformed lines (`parsed=3/3`).

**Run 2** ([`results-run2.txt`](../agent_work/ch06/adversarial/results-run2.txt)) produced **45 HELD, 4 BROKEN**. All four remaining `BROKEN` lines reflected claims that needed correction or narrowing (or a wrong expectation in the attack itself):

- `LOCK-06`: proved that a stray close of `-shm` when `-wal` already holds uncheckpointed frames (`WAL_READ_LOCK(1)`) skips `walRestartLog` (`src/wal.c:3879`), returning `SQLITE_OK` to both writers and silently overwriting the contender's commit. Experiment 6.1 added Part C, and Section 3.2 and Experiment 6.1 now restrict the `SQLITE_PROTOCOL` outcome to `WAL_READ_LOCK(0)` only.
- `LOCK-04b`: proved that when the second process closes and exits before the holder commits (as the C `sqlite3` CLI always does after a batch), `sqlite3WalClose` (`src/wal.c:2507-2571`) deletes `-wal` and `-shm` and the holder's `COMMIT` returns `SQLITE_OK` into its unlinked `-wal`, silently losing the CLI's commit. Experiment 6.1 added Part D to reproduce this cleanly with Go children, and the chapter reports all three WAL outcomes.
- `BUSY-05b`: proved that setting `PRAGMA locking_mode=EXCLUSIVE` in WAL mode takes the database file's `EXCLUSIVE` lock (`src/pager.c:5982-5988`) and blocks a second process's reader at open (`SQLITE_BUSY (5)`). Sections 1, 4, and 6.2 narrow WAL non-blocking reader/writer concurrency to the default `locking_mode=NORMAL`.
- `SNAP-04`: the attack expected `salt-1 + 1`, but step 6 (`22` frames `>= wal_autocheckpoint=20`) triggered a full checkpoint before the capture opened, placing the capture on `WAL_READ_LOCK(0)`. Step 7 restarted the WAL under it (`salt-1 + 1`, `nBackfill = 0`), and step 19 restarted it again (`salt-1 + 2`), confirmed by [`snap04-restart`](../agent_work/ch06/probes/snap04-restart/main.go). Section 6.1 records this `WAL_READ_LOCK(0)` rule and its consequence for Rung R6.

After updating those four attack expectations to match the verified rules, the final run ([`results-run3.txt`](../agent_work/ch06/adversarial/results-run3.txt) and [`results.txt`](../agent_work/ch06/adversarial/results.txt)) passed **49 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every operational rule below is supported directly by this chapter's experiments and verified source spans:

1. **One writer per database is SQLite's rule; the platform decides who (Experiment 6.2 B, C, E; `BUSY-01a/b`).** All writers on one database serialize on a single lock byte (`-shm` byte `120` in WAL mode). With `BEGIN IMMEDIATE` and a generous `busy_timeout`, competing writers queue and commit cleanly without losing transactions (Experiment 6.2 E: all 10 alternating recipe steps committed and matched step `15`).
2. **Use `BEGIN IMMEDIATE` for every write transaction (Experiment 6.2 B, C1, C3; `BUSY-06`).** It is the only start mode whose lock conflict is always handled by `busy_timeout`, and when `busy_timeout` expires it fails at `BEGIN` before doing application work. Starting a write with `BEGIN DEFERRED` and reading first bypasses `busy_timeout` (`src/btree.c:3740-3741`) and fails immediately with `SQLITE_BUSY (5)` or `SQLITE_BUSY_SNAPSHOT (517)`.
3. **Never touch a database file or its `-shm` with ordinary file calls inside a serving process (Experiment 6.1 A–D; `LOCK-02b`, `LOCK-03d`, `LOCK-04a/b`, `LOCK-06`).** A single `os.Open` + `Close` drops every POSIX lock that process holds on that inode. The result is a silently lost `SQLITE_OK` commit in rollback mode (6.1 A) and in two of three WAL states (6.1 C and D), or a `SQLITE_PROTOCOL (15)` failure after ~10 s (6.1 B). Hashing, copying, log shipping, backup, and any file-opening inspection must run either in a separate process (`internal/procs` `CopierRole` pattern) or after the last connection to that file has closed.
4. **SQLite's locks work between processes on one machine and nowhere else (Experiment 6.1 `/proc/locks` and `F_GETLK`; `src/os_unix.c:1189-1342`).** Locking relies on local kernel POSIX advisory locks on one inode and a local shared-memory `-shm` mapping. A platform that moves or serves databases across machines must coordinate single-writer ownership at the platform layer (Chapter 10), never by sharing a database file over a network filesystem.
5. **Every live capture must go through SQLite's locking, never around it (Experiments 6.3 and 6.4; `TORN-01a`–`TORN-04b`, `SNAP-01a`–`SNAP-06`).** Plain reads of a live `.db` file produce torn copies (6.3 A2, B; 6.4's `copy.db` alone) or valid-looking stale databases that silently drop commits (6.3 A1). Use one of the two R4 live captures: read all pages via `sqlite_dbpage` inside a single read transaction (E1-exact to the snapshot), or copy `.db` then `-wal` while holding an active read transaction (after at least one `SELECT`, never bare `BEGIN DEFERRED`) and replay with `internal/walfile`. Remember that holding `BEGIN IMMEDIATE` stops commits but does not stop another connection's `PASSIVE` checkpoints from writing into `.db` (`SNAP-03b`).
6. **Bound live-capture durations, and know which read slot a reader pins (Experiment 6.4; `SNAP-04`; Chapter 5 Experiment 5.3).** While a capture holds its read transaction, checkpoints stop at its mark (frame `61` throughout Experiment 6.4 Method 1), so `-wal` grows for as long as the capture lasts. Conversely, if a reader starts on an already fully checkpointed WAL (`WAL_READ_LOCK(0)`), it blocks all checkpoint backfill (`src/wal.c:2254-2260`) but does *not* stop the next write from restarting the WAL (`src/wal.c:3885`) — a constraint Chapter 9's WAL shipper must honor when pinning WAL generations.
7. **Alert on `SQLITE_PROTOCOL (15)` and `SQLITE_IOERR_DELETE_NOENT (5898)` in production logs, but do not treat their absence as proof that locks are intact (Experiment 6.1 A–D).** On local storage both codes indicate broken POSIX locks (6.1 A and B), yet 6.1 C and D lose committed transactions while every statement returns `SQLITE_OK`.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 6.1 Part B, why did `holder stray-close db` leave `contender` still blocked with `SQLITE_BUSY (5)`, whereas `holder stray-close -shm` admitted `contender` immediately? Compare all three WAL commit outcomes after a stray close of `-shm` (Parts B, C, and D) and name the C guard in `walRestartLog` (`src/wal.c:3879`) that separates Part B from Part C.
2. In Experiment 6.2 Part C3, why did `writer-2`'s `INSERT` return `SQLITE_BUSY (5)` immediately — and then `SQLITE_BUSY_SNAPSHOT (517)` immediately after `writer-1` committed — even though `busy_timeout` was `120,000ms`, whereas Parts C1 and C2 waited? Cite the exact loop condition in `src/btree.c:3740-3741`.
3. In Experiment 6.2 Part D (rollback mode), why did `reader`'s new `SELECT count(*) FROM lab` at step 58 return `SQLITE_BUSY (5)` immediately after `reader` finished its own transaction at step 57, and what happens under WAL mode with `PRAGMA locking_mode=NORMAL` versus `PRAGMA locking_mode=EXCLUSIVE`?
4. In Experiment 6.4 Method 2, `PASSIVE` checkpoints reported `checkpointed=61` and then `checkpointed=116`, yet `copy.db` on its own reported `user_version 5` and `10` integrity problems. Explain why `nBackfill` advanced while page 1 remained at step 5 (`src/wal.c:1764-1792`, `src/wal.c:2312-2314`), and why replaying `copy.db` + `copy.wal` still produced a valid recipe step `13`.
5. Why does holding `BEGIN DEFERRED` with no initial `SELECT` fail to protect a `.db` + `-wal` copy (`SNAP-05`), why does holding `BEGIN IMMEDIATE` still not prevent another connection's `PASSIVE` checkpoint from tearing `.db` (`SNAP-03b`), and why can a WAL still restart under an open read transaction that started on a fully checkpointed WAL (`SNAP-04`)?

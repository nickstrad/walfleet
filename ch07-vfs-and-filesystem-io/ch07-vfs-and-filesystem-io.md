# Chapter 7 — VFS and filesystem I/O

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `os_unix.c`, `os.c`, `os.h`, `vfstrace.c` (copied from `ext/misc/vfstrace.c`) |
| Linked C sources (in the pinned tree, not copied) | `src/sqlite.h.in`, `src/sqliteInt.h`, `src/sqliteLimit.h`, `src/pager.c`, `src/wal.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to start the runner from the host. Build the `lab` image once, from the repository root:
>   ```text
>   docker compose build lab
>   ```
>   The image already contains the pinned SQLite `3.53.4` source tree, the C tools (`sqlite3`, `showwal`, `dbhash`; Experiments 7.1 and 7.3 invoke the pinned `sqlite3` CLI), `strace` (Experiments 7.1 and 7.2), and the DuckDB CLI that presents the results.
> - **Start SeaweedFS** (optional, required only for the `-s3` route of Experiment 7.4):
>   ```text
>   docker compose up -d seaweedfs
>   ```
> - **Run an experiment** from the repository root. Each command starts the `lab` container, runs the experiment in it, and prints its walkthrough; add `-i` (`go run ./cmd/course run -i 7.1`) to stay in DuckDB afterwards with the tables loaded:
>   ```text
>   go run ./cmd/course run 7.1
>   go run ./cmd/course run 7.2
>   go run ./cmd/course run 7.3
>   go run ./cmd/course run 7.4
>   ```
>   `go run ./cmd/course run 7.4` runs the default HTTP route. `course run` cannot pass experiment flags, so the `-s3` route is run directly in the container (where `http://seaweedfs:8333` is preconfigured): `docker compose run --rm lab go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3`. In the container, scratch databases and tables are written under `/work/7.1/` through `/work/7.4/` (the `lab-work` volume); run directly on the host, under `_work/7.1/` through `_work/7.4/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** an experiment is still an ordinary Go program (`go run ./ch07-vfs-and-filesystem-io/experiments/7.3-go-vfs-shim`). Run this way it prints only its verdict and leaves its tables under `_work/7.3/results/`; Experiments 7.1 and 7.3 then need the pinned CLI on the host (`go run ./cmd/course fetch-source`, then `go run ./cmd/course build-tools`).
> - **Linux-only experiments on macOS:** Experiments 7.1 (merged syscall stream) and 7.2 (strace I/O counting) are **Linux only** because they use `strace`; `course run` runs them in full in the Linux `lab` container. Run directly on macOS, Experiment 7.1 records the CLI `-vfstrace` call list without syscalls and Experiment 7.2 runs its 12 workloads uncounted. Experiments 7.3 and 7.4 do not use `strace` and run natively on Linux and macOS.
> - **Measurement environment:** The stored evidence in this chapter was captured in the Debian `lab` container on `linux/arm64` (Go 1.26; its `./configure` CLI build calls `fdatasync`). The checked values are identical to the earlier `linux/amd64` capture. macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and cross-vetted only; `internal/govfs`'s startup self-check and unit tests were not run on macOS.

---

## 1. Model

SQLite's storage engine—the **pager** (which manages fixed-size 4,096-byte **pages** and atomic commits) and the **WAL** (write-ahead log) module (which appends committed pages as **frames** to a log file instead of overwriting the main database)—never invokes an operating system call directly. Every byte read, byte written, durability flush (**fsync**), file lock, directory lookup, and shared-memory mapping crosses one interface: the **VFS** (Virtual File System).

The VFS consists of two C structures:

1. **`sqlite3_vfs`** (one per registered filesystem backend): creates, opens, deletes, and checks files (`xOpen`, `xDelete`, `xAccess`, `xFullPathname`), supplies random bytes (`xRandomness`), sleeps (`xSleep`), and reads the clock (`xCurrentTime`, `xCurrentTimeInt64`). On Linux and macOS, the default registered VFS is `"unix"`, implemented in `os_unix.c`.
2. **`sqlite3_file` and `sqlite3_io_methods`** (one `sqlite3_file` per open file handle, pointing to a `sqlite3_io_methods` table of function pointers): performs operations on an open file. SQLite versions this method table from 1 to 3:
   - **Version 1 (12 methods):** `xClose`, `xRead`, `xWrite`, `xTruncate`, `xSync`, `xFileSize`, `xLock`, `xUnlock`, `xCheckReservedLock`, `xFileControl`, `xSectorSize`, `xDeviceCharacteristics`. Sufficient for rollback-journal databases (`-journal`) and immutable read-only databases.
   - **Version 2 (+4 methods):** adds `xShmMap`, `xShmLock`, `xShmBarrier`, `xShmUnmap` for the **WAL-index** shared-memory file (`-shm`). Without version 2, SQLite cannot run WAL mode.
   - **Version 3 (+2 methods):** adds `xFetch` and `xUnfetch` for **memory-mapped I/O** (`PRAGMA mmap_size`), which maps database pages into process memory with `mmap`.

Three VFS patterns appear in this chapter:

- **The stock OS VFS (`unix` in `os_unix.c`).** Translates each VFS method into Unix **syscalls** (kernel calls): `open`/`openat` (returning a **file descriptor**, an integer handle to an open file), `pread64` and `pwrite64` (reads and writes at explicit byte offsets), `fsync` or `fdatasync` (flushing dirty buffers from the **OS page cache** to durable storage), `fcntl` (acquiring or releasing **POSIX advisory locks** on byte ranges of a file), `ftruncate`, `stat`, `unlink`, `mmap`, and `close`.
- **A forwarding VFS (a shim).** Wraps an underlying VFS, intercepts calls to log them or inject errors, and forwards the actual I/O to the wrapped VFS. Each open file places a small shim header directly ahead of the wrapped VFS's `sqlite3_file` bytes. SQLite's own `ext/misc/vfstrace.c` (built into the `sqlite3` CLI as `-vfstrace`) and this chapter's pure-Go package `internal/govfs` both use this layout.
- **A standalone read-only VFS.** Replaces local disk files with another byte source. `modernc.org/sqlite/vfs` exposes any Go `io/fs.FS` to SQLite as a version-1 read-only VFS. Backed by this chapter's `internal/rangefs`, each page miss becomes one HTTP or S3 **range request** (`Range: bytes=start-end`).

```text
  SQLite Core (pager.c, wal.c)
        |
        |  sqlite3OsOpen / Read / Write / Sync / Lock / ShmLock (os.c)
        v
  +=======================================================================+
  | VFS Boundary (sqlite3_vfs + sqlite3_io_methods)                       |
  +=======================================================================+
        |                           |                           |
        | Route A: stock or shim    | Route B: Go shim          | Route C: remote snapshot
        v                           v                           v
  +--------------------+    +--------------------+    +------------------------+
  | vfstrace.c (C CLI) |    | internal/govfs     |    | modernc.org/sqlite/vfs |
  | logs calls -> unix |    | logs & faults      |    | (io_methods v1, RO)    |
  +--------------------+    +--------------------+    +------------------------+
        |                           |                           |
        v                           v                           v
  +----------------------------------------------+    +------------------------+
  | os_unix.c ("unix" VFS, io_methods v3)        |    | internal/rangefs       |
  | pread64, pwrite64, fsync/fdatasync, fcntl,   |    | HEAD/HeadObject (stat) |
  | mmap, ftruncate, stat, unlink, dir fsync     |    | GET/GetObject (Range)  |
  +----------------------------------------------+    +------------------------+
        |                                                       |
        v                                                       v
  Linux / Unix Kernel & Local Disk (.db, -journal, -wal, -shm)  HTTP Server / SeaweedFS S3
```

**WAL mode as the platform default.** Platform databases in this course default to `journal_mode=wal`, where commits append frames to a **sidecar** `-wal` file (sibling files next to `.db`: `-journal`, `-wal`, and `-shm`) until a **checkpoint** copies committed pages back into the main `.db` file. A database is **at rest** when every committed page is inside `.db` and no uncheckpointed `-wal` or live rollback `-journal` remains. This chapter evaluates the WAL default and keeps it: Experiment 7.2 measures how WAL reduces syncs and bytes per commit compared with rollback `DELETE` mode, and Experiment 7.3 confirms our pure-Go forwarding VFS supports WAL in full (`iVersion = 3`). Rollback mode (`journal_mode=delete`) appears here only as the comparison baseline in Experiments 7.2 and 7.3, and briefly inside Experiment 7.1 when SQLite switches an empty database into WAL mode. Two WAL-specific VFS constraints apply:

1. **A writable custom VFS must provide `io_methods` version 2 (`xShm*`).** With version 1, SQLite does not return an error when asked for `PRAGMA journal_mode=wal`; it silently replies `delete` (verified by `TestIOVersionOneCannotUseWAL` in `internal/govfs/govfs_test.go`, and turned into an error by `sqlitex.Open` because it checks the returned mode).
2. **Reading a WAL-format snapshot through a read-only version-1 VFS requires `immutable=1` and a fully checkpointed file.** Without `immutable=1`, SQLite reads the WAL format version (`2`) from database header bytes 18–19, demands shared memory, and fails with `SQLITE_CANTOPEN` (code `14`). With `immutable=1`, SQLite never checks for a `-wal` file, so any uncheckpointed commits left in `-wal` are silently ignored (Experiment 7.4).

**Where this chapter sits on the Rebuild Ladder.** Chapter 7 adds no new rung of its own. It builds two custom VFS packages (`internal/govfs` and `internal/rangefs`), supplies the per-commit I/O cost model used by Chapters 9 and 10, provides in-process deterministic fault injection, and establishes the operational rules for querying at-rest snapshots (rung R5) directly from object storage.

---

## 2. Question

What exactly does SQLite ask the operating system to do for one commit, in what order, what does each choice of journal mode and `synchronous` cost, and what contract must a custom VFS honor when a platform intercepts I/O or queries snapshots from object storage?

This chapter reuses three shared packages from earlier chunks (linked here, not inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite through `modernc.org/sqlite` with explicit settings (including `Options.VFS` to select a registered VFS via `vfs=<name>`), checks `sqlite_source_id()` against `sqlite/PIN`, formats repository-relative scratch paths (`DisplayPath`), and locates the pinned `sqlite3` CLI.
- [`internal/trace`](../internal/trace/): runs commands under Linux `strace`, parses `-vfstrace` output, merges both into one ordered stream (`RunCLIMerged`), captures paths across `stat`/`access` syscall families, and normalizes syscall aliases (`CanonicalName`: `open`/`openat` → `open`, `fsync`/`fdatasync` → `sync`, `unlink`/`unlinkat` → `unlink`, and `stat` variants → `stat`) so one evidence file holds across builds that call `fsync` versus `fdatasync`.
- [`internal/recipe`](../internal/recipe/): provides the deterministic seeded workload (Rebuild Ladder rung R0) used in Experiments 7.3 and 7.4.

This chapter introduces two new shared packages—`internal/govfs` and `internal/rangefs`—both inlined in full in Section 5.

---

## 3. Call path

Every VFS call travels through four layers of C source code in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`). Four files are copied whole into `ch07-vfs-and-filesystem-io/` and verified by `SOURCES.sha256`: `os_unix.c`, `os.c`, `os.h`, and `vfstrace.c`. Five larger files (`src/sqlite.h.in`, `src/sqliteInt.h`, `src/sqliteLimit.h`, `src/pager.c`, `src/wal.c`) are linked at the pinned tag rather than copied.

### 3.1 The public VFS structs and core wrappers

- **Open flags, sync flags, and structs** ([`src/sqlite.h.in:584-878`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L584-L878), [`src/sqlite.h.in:1513-1550`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L1513-L1550)):
  - File-role bits tell `xOpen` which kind of file SQLite is opening: `SQLITE_OPEN_MAIN_DB` (`0x100`), `SQLITE_OPEN_MAIN_JOURNAL` (`0x800`), `SQLITE_OPEN_WAL` (`0x80000`), along with read/write/create bits (`0x2`, `0x4`).
  - Sync flags ([`src/sqlite.h.in:731-733`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L731-L733)) define `SQLITE_SYNC_NORMAL` (`0x00002`), `SQLITE_SYNC_FULL` (`0x00003`), and `SQLITE_SYNC_DATAONLY` (`0x00010`). Notice that `0x03` contains bit `0x02`—which causes `vfstrace`'s sync-label bug in Section 3.4.
  - `struct sqlite3_file` ([`src/sqlite.h.in:747-749`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L747-L749)) contains only one field: `const struct sqlite3_io_methods *pMethods`. Every VFS allocates `szOsFile` bytes and stores its own state after that pointer.
  - `struct sqlite3_io_methods` ([`src/sqlite.h.in:854-878`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L854-L878)) lists the 18 per-file method pointers across versions 1–3; `struct sqlite3_vfs` ([`src/sqlite.h.in:1513-1550`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L1513-L1550)) lists `iVersion`, `szOsFile`, `mxPathname`, `pNext`, `zName`, `pAppData`, and the filesystem methods.
- **Core wrappers and VFS registry** (copied as [`os.c`](os.c)):
  - `sqlite3OsClose` ([`src/os.c:82-87`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L82-L87)), `sqlite3OsRead` ([`src/os.c:88-91`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L88-L91)), `sqlite3OsWrite` ([`src/os.c:92-95`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L92-L95)), `sqlite3OsTruncate` ([`src/os.c:96-98`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L96-L98)), `sqlite3OsDelete` ([`src/os.c:233-237`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L233-L237)), and `sqlite3OsAccess` ([`src/os.c:238-246`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L238-L246)) are thin one-line forwards to the method pointers.
  - `sqlite3OsSync` ([`src/os.c:99-102`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L99-L102)) skips `xSync` completely whenever `flags` is zero (which is how `PRAGMA synchronous=OFF` avoids every sync):
    ```c
    int sqlite3OsSync(sqlite3_file *id, int flags){
      DO_OS_MALLOC_TEST(id);
      return flags ? id->pMethods->xSync(id, flags) : SQLITE_OK;
    }
    ```
  - `sqlite3OsOpen` ([`src/os.c:215-232`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L215-L232)) masks the open flags with `0x1087f7f` so connection-only flags (such as mutex or shared-cache bits) never reach a VFS, and asserts `rc == SQLITE_OK || pFile->pMethods == 0`.
  - `sqlite3_vfs_find` ([`src/os.c:362-381`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L362-L381)) walks the global linked list of registered VFS structs under the main static mutex; passing `NULL` returns the head (default) VFS. `sqlite3_vfs_register` ([`src/os.c:408-431`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L408-L431)) unlinks the struct if already present and inserts it at the head (when `makeDflt` is nonzero) or second; SQLite stores the `zName` pointer without copying it, so the caller must keep `zName` alive for as long as the VFS stays registered.

### 3.2 How `os_unix.c` implements each method

All functions below are in copied [`os_unix.c`](os_unix.c), with lock constants in copied [`os.h`](os.h):

- **Opening files and arming directory syncs:** `unixOpen` ([`src/os_unix.c:6538-6826`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6538-L6826)) calls `robust_open` ([`src/os_unix.c:834-872`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L834-L872)), which retries `open()` on `EINTR`, always sets `O_CLOEXEC`, and refuses file descriptors 0–2 so a database never collides with stdin, stdout, or stderr. When `unixOpen` opens a new rollback journal, super-journal, or WAL with `SQLITE_OPEN_CREATE` (`isNewJrnl` at lines 6569–6572), it sets `UNIXFILE_DIRSYNC` in `ctrlFlags` (line 6777):
  ```c
  int isNewJrnl = (isCreate && (
        eType==SQLITE_OPEN_SUPER_JOURNAL
     || eType==SQLITE_OPEN_MAIN_JOURNAL
     || eType==SQLITE_OPEN_WAL
  ));
  ```
- **Positioned reads and writes:** `unixRead` ([`src/os_unix.c:3512-3579`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3512-L3579)) serves bytes from the file's memory map with `memcpy` whenever `PRAGMA mmap_size > 0` covers the requested range ([`src/os_unix.c:3533-3548`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3533-L3548); the pager also fetches mapped pages directly through `xFetch` in `getPageMMap`, [`src/pager.c:5687`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5687)). With memory-mapped I/O off—SQLite's default (`SQLITE_DEFAULT_MMAP_SIZE 0`, [`src/sqliteInt.h:1113-1115`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqliteInt.h#L1113-L1115))—every `unixRead` calls `seekAndRead` ([`src/os_unix.c:3463-3505`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3463-L3505)), which loops `pread`/`pread64` until all requested bytes arrive or EOF is reached. On a **short read** (fewer bytes than requested), `unixRead` zero-fills the unread tail of the buffer and returns `SQLITE_IOERR_SHORT_READ` (lines 3575–3577); SQLite expects this behavior when reading the 100-byte header of a new empty file or probing past the end of a journal. `unixWrite` ([`src/os_unix.c:3643-3721`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3643-L3721)) loops `seekAndWriteFd` ([`src/os_unix.c:3588-3624`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3588-L3624)), which issues `pwrite`/`pwrite64` in chunks of at most 128 KiB and maps `ENOSPC` to `SQLITE_FULL` and other write errors to `SQLITE_IOERR_WRITE` (lines 3710–3717).
- **Syncing files and directories:** `unixSync` ([`src/os_unix.c:3911-3956`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3911-L3956)) decodes `isDataOnly` and `isFullsync` (`(flags & 0x0F) == SQLITE_SYNC_FULL`), calls `full_fsync` ([`src/os_unix.c:3778-3849`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3778-L3849)), and then checks `UNIXFILE_DIRSYNC` ([`src/os_unix.c:3941-3953`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3941-L3953)):
  ```c
  if( pFile->ctrlFlags & UNIXFILE_DIRSYNC ){
    int dirfd;
    OSTRACE(("DIRSYNC %s (have_fullfsync=%d fullsync=%d)\n", pFile->zPath,
            HAVE_FULLFSYNC, isFullsync));
    rc = osOpenDirectory(pFile->zPath, &dirfd);
    if( rc==SQLITE_OK ){
      full_fsync(dirfd, 0, 0);
      robust_close(pFile, dirfd, __LINE__);
    }else{
      assert( rc==SQLITE_CANTOPEN );
      rc = SQLITE_OK;
    }
    pFile->ctrlFlags &= ~UNIXFILE_DIRSYNC;
  }
  ```
  `openDirectory` ([`src/os_unix.c:3874-3894`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3874-L3894)) opens the parent directory read-only, `full_fsync` flushes the directory entry so the new journal or WAL file's existence survives power loss, and `UNIXFILE_DIRSYNC` is cleared so the directory sync runs once per `xOpen`. Inside `full_fsync`, Linux calls `fdatasync` when compiled with `HAVE_FDATASYNC` (a `./configure` build) and `fsync` otherwise ([`src/os_unix.c:3738-3740`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3738-L3740) defines `#define fdatasync fsync`); macOS issues `fcntl(fd, F_FULLFSYNC, 0)` when `isFullsync` is set (`PRAGMA fullfsync=ON`) and falls back to `fsync`.
- **Truncate, delete, access, file controls, and device characteristics:**
  - `unixTruncate` ([`src/os_unix.c:3961-4006`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3961-L4006)) calls `ftruncate`.
  - `unixDelete` ([`src/os_unix.c:6833-6869`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6833-L6869)) calls `unlink`, returning `SQLITE_IOERR_DELETE_NOENT` if the file is missing, and syncs the directory when `dirSync` is nonzero (`synchronous=EXTRA`).
  - `unixAccess` ([`src/os_unix.c:6881-6903`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6881-L6903)) uses `stat`—not the `access()` syscall—for `SQLITE_ACCESS_EXISTS` (lines 6895–6898), and treats a zero-byte regular file as absent (`!S_ISREG(buf.st_mode) || buf.st_size > 0`). Only `SQLITE_ACCESS_READWRITE` calls `access()`.
  - `unixFileControl` ([`src/os_unix.c:4141-4341`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4141-L4341)) handles controls such as `SIZE_HINT`, `PERSIST_WAL`, `HAS_MOVED` (which runs `stat` to detect an unlinked or replaced file), `VFSNAME`, and `MMAP_SIZE`, returning `SQLITE_NOTFOUND` (`12`) for controls meant only for other VFS shims (`SYNC`, `COMMIT_PHASETWO`, `CKPT_START`, `CKPT_DONE`, `PRAGMA`, `PDB`, `BUSYHANDLER`).
  - `setDeviceCharacteristics` ([`src/os_unix.c:4353-4375`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4353-L4375)) sets sector size to 4,096 bytes and advertises `SQLITE_IOCAP_POWERSAFE_OVERWRITE | SQLITE_IOCAP_SUBPAGE_READ` (`0x9000`).
- **Locks and WAL shared memory:**
  - `unixLock` ([`src/os_unix.c:1866-2095`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1866-L2095)) maps SQLite's five lock levels (`NO_LOCK`..`EXCLUSIVE_LOCK`, [`src/os.h:98-102`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.h#L98-L102)) onto POSIX byte-range `fcntl(F_SETLK)` locks on the main database file at `PENDING_BYTE` (`0x40000000` = `1073741824`), `RESERVED_BYTE` (`+1`), and `SHARED_FIRST` (`+2`, length `SHARED_SIZE = 510`, [`src/os.h:160-166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.h#L160-L166)).
  - `unixOpenSharedMemory` ([`src/os_unix.c:4952-5085`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4952-L5085)) opens `<db>-shm` and probes byte `128` (`UNIX_SHM_DMS`, [`src/os_unix.c:4603`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4603), the **dead-man switch**) to check whether this connection is the first opener (which truncates `-shm` and triggers WAL-index recovery).
  - `unixShmMap` ([`src/os_unix.c:5106-5239`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L5106-L5239)) grows `-shm` in 32 KiB regions by writing one byte at the end of each 4 KiB page (`4095`, `8191`, …, `32767`) and then calls `mmap`.
  - `unixShmLock` ([`src/os_unix.c:5284-5476`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L5284-L5476)) places `fcntl` locks on bytes `120..127` (`UNIX_SHM_BASE = 120`, [`src/os_unix.c:4602`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4602), plus slot `0..7`: `WRITE=0`, `CKPT=1`, `RECOVER=2`, `READ0..READ4=3..7`).
  - `unixShmUnmap` ([`src/os_unix.c:5503-5546`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L5503-L5546)) unmaps the regions and, when the last connection closes with `deleteFlag=1`, unlinks `-shm` and closes its descriptor.

### 3.3 How commits and checkpoints drive `xWrite` and `xSync`

- **Sync flags (`sqlite3PagerSetFlags`, [`src/pager.c:3685-3698`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3685-L3698)):** Unless `synchronous=OFF`, SQLite sets `syncFlags` to `SQLITE_SYNC_NORMAL` (`0x02`)—switching to `SQLITE_SYNC_FULL` (`0x03`) only when `PRAGMA fullfsync=ON` is set. For WAL mode, it packs checkpoint and header sync flags into `walSyncFlags = syncFlags << 2` (active at both `NORMAL` and `FULL`) and adds commit sync flags into the low bits only at `FULL` (and `EXTRA`).
- **Rollback-journal commit (`journal_mode=delete`, `persist`, `truncate`):** `sqlite3PagerCommitPhaseOne` ([`src/pager.c:6511-6730`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6511-L6730)) calls `syncJournal` ([`src/pager.c:4332-4441`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4332-L4441)). At `synchronous=NORMAL`, `syncJournal` updates the 12-byte journal header and calls `xSync` on `-journal` once (which also triggers the one-time directory sync inside `unixSync`). At `synchronous=FULL`, `syncJournal` calls `xSync` on `-journal`, writes the 12-byte header count, and calls `xSync` on `-journal` a second time. Next, the pager writes every dirty page to the main `.db` file and calls `xSync` on `.db`. Finally, `pager_end_transaction` ([`src/pager.c:2052-2180`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2052-L2180)) finalizes `-journal`:
  - In `DELETE` mode it closes and unlinks `-journal` (`xDelete`, passing `dirSync=1` only at `synchronous=EXTRA`, which adds a second directory sync for `5` sync syscalls per commit).
  - In `PERSIST` mode it zeros the journal header and syncs `-journal` again (`zeroJournalHdr`, [`src/pager.c:1399-1415`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1399-L1415)).
  - In `TRUNCATE` mode it calls `ftruncate(0)` and—at `FULL`—syncs `-journal` again ([`src/pager.c:2086-2098`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2086-L2098)).
  - Neither `PERSIST` nor `TRUNCATE` eliminates the directory sync on Unix: `pager_unlock` ([`src/pager.c:1891-1895`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1891-L1895)) closes the journal whenever the lock drops unless the VFS advertises `SQLITE_IOCAP_UNDELETABLE_WHEN_OPEN` (which `unix` does not), so the next write transaction reopens `-journal` with `SQLITE_OPEN_CREATE` and `unixOpen` re-arms `UNIXFILE_DIRSYNC`.
- **WAL commit and checkpoint (`journal_mode=wal`):** `walFrames` ([`src/wal.c:4038-4266`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4038-L4266)) checks whether the WAL is fully checkpointed and can be reused from frame 0 via `walRestartLog` ([`src/wal.c:3875-3915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3875-L3915)), which generates a new **salt** (a pair of random 32-bit values in the 32-byte WAL header identifying one WAL generation so stale tail frames from an earlier cycle are ignored). When starting at frame 0 (a brand-new WAL or right after a restart), `walFrames` writes the 32-byte WAL header and syncs it (`syncHeader`, [`src/wal.c:4114-4124`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4114-L4124)) using checkpoint sync flags—active at both `NORMAL` and `FULL` (off only at `OFF`). Then it appends each dirty page as a 24-byte frame header plus a 4,096-byte page image. At `synchronous=FULL`, it calls `xSync` on `-wal` at commit; at `synchronous=NORMAL`, commit sync flags are zero, so ordinary commits issue zero syncs. When a checkpoint runs (`walCheckpoint`, [`src/wal.c:2199-2395`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395)), SQLite syncs `-wal` (line 2277), copies committed pages to `.db`, truncates `.db` if needed, and syncs `.db` (line 2333)—two syncs at both `NORMAL` and `FULL`.

### 3.4 The shim pattern in `ext/misc/vfstrace.c`

Copied as [`vfstrace.c`](vfstrace.c), SQLite's tracing shim demonstrates how any forwarding VFS works:

- `vfstrace_register` ([`ext/misc/vfstrace.c:1138-1199`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L1138-L1199)) looks up the underlying VFS with `sqlite3_vfs_find`, allocates a new `sqlite3_vfs` with `szOsFile = pRoot->szOsFile + sizeof(vfstrace_file)`, wires in wrapper functions, and registers it.
- `vfstraceOpen` ([`ext/misc/vfstrace.c:876-930`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L876-L930)) points `p->pReal` just past its own header (`(sqlite3_file *)&p[1]`), calls `pRoot->xOpen` **before** printing the `xOpen(...)` log line, and builds a `sqlite3_io_methods` table matching `pReal->pMethods->iVersion` (filling `xShm*` and `xFetch`/`xUnfetch` only when the underlying file provides them).
- `vfstraceWrite` ([`ext/misc/vfstrace.c:410-425`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L410-L425)) prints `xWrite(...)` *before* calling the real method and ` -> rc` *after* it returns. That ordering lets `strace` in Experiment 7.1 bracket the exact syscalls executed inside each VFS call (while `xOpen`'s syscalls appear immediately *before* its printed `xOpen` line).
- `vfstraceSync` ([`ext/misc/vfstrace.c:445-465`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L445-L465)) formats the sync flags with a bitmask bug at lines 453–454:
  ```c
  if( flags & SQLITE_SYNC_FULL )        strappend(zBuf, &i, "|FULL");
  else if( flags & SQLITE_SYNC_NORMAL ) strappend(zBuf, &i, "|NORMAL");
  ```
  Because `SQLITE_SYNC_FULL` is `0x03` and `SQLITE_SYNC_NORMAL` is `0x02`, `0x02 & 0x03` is nonzero (`0x02`), so `vfstrace` prints `FULL` even when SQLite passed `SQLITE_SYNC_NORMAL`. Experiment 7.3 exposes the real flag value `0x2` directly from Go.

### 3.5 Version notes

Behavior differences across SQLite releases relevant to the VFS layer (sources: [`docs/knowledge/source-inventory-ch05-09.md`](../docs/knowledge/source-inventory-ch05-09.md) §7.9 and the pinned source):

- **Device characteristics `0x9000` (3.47.1 / 3.48.0):** Added `SQLITE_IOCAP_SUBPAGE_READ` (`0x8000`). On Linux and macOS, `os_unix.c` therefore reports `0x9000` (`SQLITE_IOCAP_POWERSAFE_OVERWRITE | SQLITE_IOCAP_SUBPAGE_READ`), whereas older releases reported `0x1000`.
- **`vfstrace.c` location:** At `3.53.4`, `vfstrace.c` lives at `ext/misc/vfstrace.c` and is compiled into the `sqlite3` CLI (`-vfstrace`). Older references cite `src/test_vfstrace.c` (and the file's own header comment still mentions that old name).
- **In-memory VFS:** `ext/misc/memvfs.c` does not exist at `3.53.4`; the built-in in-memory VFS is `"memdb"` in `src/memdb.c`.
- **Browser VFS (3.53.0):** Added the browser `opfs-wl` VFS, which is not used on Linux or macOS.

---

### Experiment 7.1 — VFS calls mapped to syscalls (`7.1-vfs-calls-to-syscalls`)

- **Claim:** Every byte SQLite reads or writes passes through a VFS method, and each VFS call maps to a specific `os_unix.c` function and specific kernel syscalls: at SQLite's default `PRAGMA mmap_size=0`, every `xRead` executes one `pread64` and every `xWrite` executes one `pwrite64` at the exact same file, offset, and length; no `pread64`, `pwrite64`, `read`, `write`, or `writev` on database, journal, WAL, or shm files occurs outside `xRead`, `xWrite`, or `xShmMap`; every `xSync` executes one `fsync`/`fdatasync` of its own file; the parent directory is synced once inside the first `xSync` of a newly created `-journal` or `-wal`; and `-wal`/`-shm` are created on first use and unlinked on last close.
- **Evidence form:** `EXACT` (every stored table must match identically on every run; the run-dependent `info` tables are excluded).
- **Platform:** **Linux only** for the merged syscall stream (`strace`). Requires the pinned `sqlite3` CLI (`go run ./cmd/course build-tools`). Run directly on macOS, the program records the CLI `-vfstrace` call list without syscalls; `course run` runs the full Linux experiment in the `lab` container.

<!-- include: experiments/7.1-vfs-calls-to-syscalls/main.go -->
```go
// Experiment 7.1 runs the pinned sqlite3 C CLI with -vfstrace under Linux
// strace to merge VFS calls and kernel syscalls into a single ordered stream.
// At SQLite's default mmap_size=0, it verifies that every xRead/xWrite/xSync
// maps to the expected pread64, pwrite64, or fsync/fdatasync syscall, that no
// byte-moving syscall on database/sidecar files escapes the VFS, that directory
// syncs happen inside the first xSync of a new journal or WAL, and that
// -wal/-shm are created at first use and removed at last close.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.1 (the strace part needs
// Linux, which the lab container provides).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls"

const dbName = "t.db"

var statements = []struct {
	label string
	sql   string
}{
	{"set WAL mode", "PRAGMA journal_mode=WAL;"},
	{"set synchronous FULL", "PRAGMA synchronous=FULL;"},
	{"create a table", "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL);"},
	{"commit one insert", "INSERT INTO t(id, v) VALUES(1, 'one');"},
	{"checkpoint", "PRAGMA wal_checkpoint(TRUNCATE);"},
	{"close", ""},
}

var unixFunction = map[string]string{
	"xOpen":                  "unixOpen",
	"xDelete":                "unixDelete",
	"xAccess":                "unixAccess",
	"xFullPathname":          "unixFullPathname",
	"xRandomness":            "unixRandomness",
	"xSleep":                 "unixSleep",
	"xCurrentTime":           "unixCurrentTime",
	"xCurrentTimeInt64":      "unixCurrentTimeInt64",
	"xClose":                 "unixClose",
	"xRead":                  "unixRead",
	"xWrite":                 "unixWrite",
	"xTruncate":              "unixTruncate",
	"xSync":                  "unixSync",
	"xFileSize":              "unixFileSize",
	"xLock":                  "unixLock",
	"xUnlock":                "unixUnlock",
	"xCheckReservedLock":     "unixCheckReservedLock",
	"xFileControl":           "unixFileControl",
	"xSectorSize":            "unixSectorSize",
	"xDeviceCharacteristics": "unixDeviceCharacteristics",
	"xShmMap":                "unixShmMap",
	"xShmLock":               "unixShmLock",
	"xShmBarrier":            "unixShmBarrier",
	"xShmUnmap":              "unixShmUnmap",
	"xFetch":                 "unixFetch",
	"xUnfetch":               "unixUnfetch",
}

// tracedSyscalls extends trace.DefaultSyscalls with stat and access families so
// xAccess (os_unix.c:6881-6903), xFileSize, and xFullPathname show their kernel
// calls. fchown is omitted because unixOpen only calls it when running as root.
var tracedSyscalls = append(append([]string(nil), trace.DefaultSyscalls...), "fstat", "newfstatat", "statx", "stat", "lstat", "access", "faccessat", "faccessat2")

// byteMovingSyscalls lists every traced canonical syscall that transfers file
// bytes, used to verify no database/journal/WAL/shm byte I/O escapes the VFS.
var byteMovingSyscalls = []string{"pread64", "pwrite64", "read", "write", "writev"}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.1" runs both.
var (
	set    *results.Set
	stream *results.Table
)

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func script() string {
	var b strings.Builder
	for i, s := range statements {
		fmt.Fprintf(&b, ".print STEP %d %s\n", i+1, s.label)
		if s.sql != "" {
			b.WriteString(s.sql + "\n")
		}
	}
	return b.String()
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.1")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	cli := sqlitex.MustLocateCLI(ctx)
	set = results.MustOpen(dir)
	steps := set.Table("statements", "step", "label", "sql")
	for i, s := range statements {
		sql := any(s.sql)
		if s.sql == "" {
			sql = "(end of input: the CLI closes the database)"
		}
		steps.Row(i+1, s.label, sql)
	}
	stream = set.Table("stream", "seq", "step", "kind", "nested", "charged_to", "call", "file", "text")
	tracing := set.Table("tracing", "syscalls")
	request := trace.CLITrace{Dir: dir, DB: filepath.Join(dir, dbName), SQL: script()}

	if !trace.Supported {
		tracing.Row(trace.LinuxOnlyMessage("strace of the CLI (VFS calls merged with syscalls)", packagePath))
		portable(ctx, cli, request, dir)
		trace.PrintLinuxOnly("strace of the CLI (VFS calls merged with syscalls)", packagePath)
		set.Finish("VFS CALLS TO SYSCALLS")
		return
	}
	tracing.Row("traced with strace")
	merged, result, err := trace.RunCLIMerged(ctx, cli, request, trace.Options{Syscalls: tracedSyscalls})
	if errors.Is(err, trace.ErrPtraceDenied) {
		fatal("strace", fmt.Errorf("%w (run: docker compose run --rm lab go run ./%s)", err, packagePath))
	}
	if err != nil {
		fatal("strace", err)
	}
	if result.ExitCode != 0 {
		fatal("cli", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr))
	}
	analyze(merged.Relative(dir), dir)
	set.Finish("VFS CALLS TO SYSCALLS")
}

func portable(ctx context.Context, cli sqlitex.CLI, request trace.CLITrace, dir string) {
	vt, err := trace.RunCLIVFSTrace(ctx, cli, request)
	if err != nil {
		fatal("vfstrace", err)
	}
	calls := trace.RelativeVFS(dir, vt.Calls)
	seq := 0
	for _, c := range calls {
		if c.Method == "xRandomness" {
			continue
		}
		seq++
		stream.Row(seq, nil, "VFS", nil, nil, c.Method, c.File, c.String())
	}
	walOpen, walDelete, shmDelete := closeLifecycle(calls)
	set.Check("D", walOpen, "the -wal file is opened through xOpen")
	set.Check("D", walDelete && shmDelete, "at the last close the -wal is deleted (xDelete) and the -shm is unmapped with delFlag=1")
}

// closeLifecycle checks the VFS-only trace (used on non-Linux hosts) for WAL
// open, WAL deletion, and shm unmap with delFlag=1 (vfstrace.c:843).
func closeLifecycle(calls []trace.VFSCall) (walOpen, walDelete, shmDelete bool) {
	for _, c := range calls {
		switch {
		case c.Method == "xOpen" && c.File == dbName+"-wal":
			walOpen = true
		case c.Method == "xDelete" && strings.HasSuffix(c.File, dbName+"-wal"):
			walDelete = true
		case c.Method == "xShmUnmap" && slices.Contains(c.Args, "delFlag=1"):
			shmDelete = true
		}
	}
	return walOpen, walDelete, shmDelete
}

func onDisk(s trace.Syscall) bool {
	return s.Path == "." || s.Path == dbName || strings.HasPrefix(s.Path, dbName+"-")
}

func syscallLine(s trace.Syscall) string {
	return s.Canonical().String()
}

func analyze(m *trace.Merged, dir string) {
	relevant := onDisk
	var kept []int
	for i, e := range m.Events {
		if e.Kind == trace.EventSyscall && !relevant(*e.Syscall) {
			continue
		}
		kept = append(kept, i)
	}

	// charged maps each file syscall to the VFS call it is counted against:
	// the call it ran inside, or, for xOpen, the unenclosed syscalls just
	// before its line, because vfstraceOpen calls the underlying xOpen before
	// printing its trace line (vfstrace.c:890-893).
	charged := map[int]int{}
	for i, e := range m.Events {
		if e.Kind != trace.EventVFS {
			continue
		}
		if e.VFS.Method != "xOpen" {
			for j, s := range m.Events {
				if s.Kind == trace.EventSyscall && s.Enclosed == i && relevant(*s.Syscall) {
					charged[j] = i
				}
			}
			continue
		}
		for j := i - 1; j >= 0; j-- {
			s := m.Events[j]
			if s.Kind == trace.EventVFS {
				break
			}
			if s.Kind == trace.EventSyscall && s.Enclosed < 0 && relevant(*s.Syscall) {
				charged[j] = i
			}
		}
	}
	syscallsOf := func(i int) []trace.Syscall {
		var out []trace.Syscall
		for _, j := range kept {
			if v, ok := charged[j]; ok && v == i {
				out = append(out, m.Events[j].Syscall.Canonical())
			}
		}
		return out
	}

	// xRandomness is left out of the stream table: where SQLite first asks the
	// VFS for random bytes differs between CLI builds. Its position goes to an
	// info table.
	infoRandomness := set.Table("info_randomness", "after_seq", "call")
	seqOf := map[int]int{}
	seq := 0
	for _, i := range kept {
		e := m.Events[i]
		if e.Kind == trace.EventVFS && e.VFS.Method == "xRandomness" {
			infoRandomness.Row(seq, e.VFS.String())
			continue
		}
		seq++
		seqOf[i] = seq
	}
	step := 0
	for _, i := range kept {
		n, ok := seqOf[i]
		if !ok {
			continue
		}
		e := m.Events[i]
		switch e.Kind {
		case trace.EventVFS:
			stream.Row(n, step, "VFS", nil, nil, e.VFS.Method, e.VFS.File, e.VFS.String())
		case trace.EventSyscall:
			s := e.Syscall.Canonical()
			var to any
			if v, ok := charged[i]; ok {
				to = seqOf[v]
			}
			stream.Row(n, step, "SYS", e.Enclosed >= 0, to, s.Name, s.Path, s.String())
		case trace.EventOutput:
			if label, ok := strings.CutPrefix(e.Output, "STEP "); ok {
				fmt.Sscanf(label, "%d", &step)
			}
			stream.Row(n, step, "OUT", nil, nil, nil, nil, e.Output)
		}
	}

	problems := set.Table("problems", "rule", "detail")
	calls := map[string]int{}
	writesOK, readsOK := 0, 0
	var dirSyncs []string
	syncCalls := 0
	var walOpenedWithCreate, shmOpenedInMap, walUnlinkedInDelete, shmUnlinkedInUnmap bool
	syncsPerFile := map[string]int{}
	writeProblems, readProblems, syncProblems := 0, 0, 0
	for i, e := range m.Events {
		if e.Kind != trace.EventVFS {
			continue
		}
		c := e.VFS
		seen := syscallsOf(i)
		calls[c.Method]++
		switch c.Method {
		case "xWrite":
			if len(seen) == 1 && seen[0].Name == "pwrite64" && seen[0].Path == c.File && seen[0].Offset == c.Offset && seen[0].Length == c.Amount && seen[0].Result == c.Amount {
				writesOK++
			} else {
				writeProblems++
				problems.Row("xWrite", fmt.Sprintf("%s encloses %v", c.String(), seen))
			}
		case "xRead":
			if len(seen) == 1 && seen[0].Name == "pread64" && seen[0].Path == c.File && seen[0].Offset == c.Offset && seen[0].Length == c.Amount {
				readsOK++
			} else {
				readProblems++
				problems.Row("xRead", fmt.Sprintf("%s encloses %v", c.String(), seen))
			}
		case "xSync":
			syncCalls++
			own := 0
			for _, s := range seen {
				if s.Name != "sync" {
					continue
				}
				if s.Path == c.File {
					own++
					syncsPerFile[c.File]++
				} else if s.Path == "." {
					dirSyncs = append(dirSyncs, fmt.Sprintf("inside xSync(%s) number %d", c.File, syncsPerFile[c.File]))
				}
			}
			if own != 1 {
				syncProblems++
				problems.Row("xSync", fmt.Sprintf("%s encloses %d syncs of its own file", c.String(), own))
			}
		case "xOpen":
			if c.File == dbName+"-wal" {
				for _, s := range seen {
					if s.Name == "open" && s.Path == dbName+"-wal" && strings.Contains(s.Flags, "O_CREAT") {
						walOpenedWithCreate = true
					}
				}
			}
		case "xShmMap":
			for _, s := range seen {
				if s.Name == "open" && s.Path == dbName+"-shm" && strings.Contains(s.Flags, "O_CREAT") {
					shmOpenedInMap = true
				}
			}
		case "xDelete":
			for _, s := range seen {
				if s.Name == "unlink" && s.Path == dbName+"-wal" {
					walUnlinkedInDelete = true
				}
			}
		case "xShmUnmap":
			for _, s := range seen {
				if s.Name == "unlink" && s.Path == dbName+"-shm" {
					shmUnlinkedInUnmap = true
				}
			}
		}
	}

	outsideIO := 0
	for _, i := range kept {
		e := m.Events[i]
		if e.Kind != trace.EventSyscall {
			continue
		}
		name := trace.CanonicalName(e.Syscall.Name)
		if !slices.Contains(byteMovingSyscalls, name) {
			continue
		}
		if e.Enclosed < 0 {
			outsideIO++
			problems.Row("outside", syscallLine(*e.Syscall)+" (outside any VFS call)")
			continue
		}
		method := m.Events[e.Enclosed].VFS.Method
		if method != "xRead" && method != "xWrite" && method != "xShmMap" {
			outsideIO++
			problems.Row("outside", syscallLine(*e.Syscall)+" inside "+method)
		}
	}

	methods := set.Table("vfs_methods", "method", "unix_function", "calls")
	names := make([]string, 0, len(calls))
	for method := range calls {
		names = append(names, method)
	}
	sort.Strings(names)
	for _, method := range names {
		methods.Row(method, unixFunction[method], calls[method])
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		fatal("read dir", err)
	}
	filesLeft := set.Table("files_left", "name")
	var left []string
	for _, e := range entries {
		if e.Name() == results.DirName {
			continue
		}
		left = append(left, e.Name())
		filesLeft.Row(e.Name())
	}

	set.Check("B", calls["xWrite"] > 0 && writeProblems == 0, fmt.Sprintf("each of the %d xWrite calls made exactly one pwrite64 to the same file, offset and length", writesOK))
	set.Check("B", calls["xRead"] > 0 && readProblems == 0, fmt.Sprintf("each of the %d xRead calls made exactly one pread64 of the same file, offset and length", readsOK))
	set.Check("B", outsideIO == 0, "every pread64/pwrite64/read/write/writev on the database, journal, WAL and shm files happened inside xRead, xWrite or xShmMap")
	set.Check("C", syncCalls > 0 && syncProblems == 0, fmt.Sprintf("each of the %d xSync calls made exactly one fsync/fdatasync of its own file", syncCalls))
	set.Check("C", len(dirSyncs) == 2 && strings.Contains(dirSyncs[0], "-journal) number 1") && strings.Contains(dirSyncs[1], "-wal) number 1"), "the directory is synced twice, inside the first xSync of the new journal and of the new WAL (unixSync's one-time directory sync)")
	set.Check("D", walOpenedWithCreate, "the -wal file is created (O_CREAT) at its first use, by xOpen")
	set.Check("D", shmOpenedInMap, "the -shm file is created inside the first xShmMap")
	set.Check("D", walUnlinkedInDelete && shmUnlinkedInUnmap, "at the last close xDelete unlinks the -wal and xShmUnmap unlinks the -shm")
	set.Check("D", slices.Equal(left, []string{dbName}), "only the database file is left after the CLI exits")
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/7.1-vfs-calls-to-syscalls/report.sql -->
```sql
-- Walkthrough of experiment 7.1. Run it with:
--   go run ./cmd/course run 7.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   statements        one row per step of the CLI session
--   tracing           one row: whether the syscalls were traced (Linux) or skipped
--   stream            one row per line of the merged stream: a VFS call, a file
--                     syscall, or a line of CLI output; charged_to is the seq of
--                     the VFS call a syscall is counted against
--   vfs_methods       one row per VFS method called: its os_unix.c function and calls
--   problems          one row per VFS call or syscall that broke a checked rule
--   files_left        one row per file in the directory after the CLI exits
--   info_randomness   where xRandomness ran (differs between CLI builds; info.sql)
--   checks            every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  7.1  Which syscalls does each VFS call make?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The pinned sqlite3 CLI runs one session with -vfstrace (each VFS call it'
.print 'makes is printed as a line) under strace, which logs the syscalls on the'
.print 'database and its -journal, -wal and -shm files and on the directory. The'
.print 'two logs are merged into one ordered stream. mmap_size is 0, the SQLite'
.print 'default. Traced: openat open close pwrite64 write writev pread64 read'
.print 'fsync fdatasync unlink unlinkat fcntl ftruncate mmap munmap and the stat'
.print 'and access families.'
.print
.print 'The session, one CLI process (.open --normal t.db first):'

SELECT step, label, sql FROM statements ORDER BY step;

.print
.print 'Whether the syscalls were traced (strace needs Linux; elsewhere only the'
.print 'VFS calls are recorded):'

SELECT syscalls FROM tracing ORDER BY syscalls;

.print
.print '── A. The merged stream ────────────────────────────────────────────────'
.print
.print 'VFS = a line of -vfstrace output; indented SYS = a syscall made while'
.print 'that VFS call ran; unindented SYS = a syscall outside any printed VFS call;'
.print 'OUT = CLI output. Syscall names are normalized: open = open/openat,'
.print 'sync = fsync/fdatasync, stat = the stat family, unlink = unlink/unlinkat.'
.print 'xRandomness is left out of the stream: where SQLite first asks the VFS for'
.print 'random bytes differs between CLI builds.'
.print
.print 'A1. The size of the stream:'

SELECT count(*) FILTER (kind = 'VFS')
         + coalesce((SELECT calls FROM vfs_methods WHERE method = 'xRandomness'), 0) AS vfs_calls,
       count(*) FILTER (kind = 'SYS') AS file_syscalls,
       count(*) FILTER (kind = 'OUT') AS output_lines,
       coalesce((SELECT calls FROM vfs_methods WHERE method = 'xRandomness'), 0) AS xrandomness_left_out
FROM stream;

CREATE TEMP VIEW shown AS
SELECT seq, step,
       CASE kind
         WHEN 'SYS' THEN CASE WHEN nested THEN '    SYS ' ELSE 'SYS ' END || text
         ELSE kind || ' ' || text
       END AS event
FROM stream;

.print
.print 'A2. Opening t.db. vfstrace prints xOpen after the real open returns, so the'
.print 'open and stat syscalls of xOpen come just before its line. The 100-byte'
.print 'header read is short: the file is empty.'

SELECT seq, event FROM shown WHERE step = 0 ORDER BY seq;

.print
.print 'A3. STEP 1, journal_mode=WAL. Changing the mode writes page 1 under a'
.print 'rollback journal: the journal is created and synced twice (the first'
.print 'time with a sync of the directory), page 1 is written and synced, and the'
.print 'journal is deleted.'

SELECT seq, event FROM shown WHERE step = 1 ORDER BY seq;

.print
.print 'A4. STEP 2, synchronous=FULL, only tells the VFS about the pragma. STEP 3,'
.print 'CREATE TABLE, is the first use of the WAL: xOpen creates -wal, the first'
.print 'xShmMap creates -shm, extends it one byte per 4 KiB and maps it, and the'
.print 'commit writes the WAL header, syncs it (with the directory), writes two'
.print 'frames (24-byte frame header + 4096-byte page) and syncs again.'

SELECT seq, event FROM shown WHERE step IN (2, 3) ORDER BY seq;

.print
.print 'A5. STEP 4, one INSERT: one frame and one sync of the -wal.'

SELECT seq, event FROM shown WHERE step = 4 ORDER BY seq;

.print
.print 'A6. STEP 5, wal_checkpoint(TRUNCATE): sync the -wal, copy the page of each'
.print 'frame into t.db (pread64 of the -wal, pwrite64 of t.db), truncate and sync t.db,'
.print 'then truncate the -wal to 0 bytes.'

SELECT seq, event FROM shown WHERE step = 5 ORDER BY seq;

.print
.print 'A7. STEP 6, close: the last connection unlinks -shm (xShmUnmap) and -wal'
.print '(xDelete).'

SELECT seq, event FROM shown WHERE step = 6 ORDER BY seq;

.print
.print '── B. Each VFS method and the syscalls behind it ───────────────────────'
.print
.print 'B1. Every VFS method, the os_unix.c function behind it, and the syscalls'
.print 'seen while it ran (for xOpen: the syscalls just before its line). Several'
.print 'methods make no syscall at all.'

SELECT m.method,
       m.calls,
       m.unix_function,
       coalesce((SELECT string_agg(call || '×' || n, ', ' ORDER BY call)
                 FROM (SELECT s.call, count(*) AS n
                       FROM stream v JOIN stream s ON s.charged_to = v.seq
                       WHERE v.kind = 'VFS' AND v.call = m.method AND s.kind = 'SYS'
                       GROUP BY s.call)), 'no file syscall') AS syscalls
FROM vfs_methods m
ORDER BY m.method;

.print
.print 'B2. Every syscall that moves file bytes, by the VFS call it ran inside:'
.print 'reads only in xRead, writes only in xWrite, and the one-byte writes that'
.print 'extend -shm in xShmMap.'

SELECT s.call AS syscall,
       CASE WHEN s.nested THEN v.call ELSE '(outside any VFS call)' END AS inside,
       count(*) AS syscalls
FROM stream s LEFT JOIN stream v ON v.seq = s.charged_to
WHERE s.kind = 'SYS' AND s.call IN ('pread64', 'pwrite64', 'read', 'write', 'writev')
GROUP BY ALL
ORDER BY syscall, inside;

.print
.print 'B3. VFS calls or syscalls that broke one of the rules in the checks below'
.print '(one pwrite64 per xWrite, one pread64 per xRead, one sync of its own file'
.print 'per xSync, no byte I/O outside xRead, xWrite and xShmMap):'

SELECT count(*) AS rule_breaks FROM problems;

.print
.print '── C. Syncs ────────────────────────────────────────────────────────────'
.print
.print 'C1. Each xSync makes exactly one fsync/fdatasync of its own file. The first'
.print 'xSync of a newly created journal or WAL also syncs the directory, so the'
.print 'new directory entry is durable; no VFS call names the directory.'

WITH x AS (
  SELECT seq, step, file, row_number() OVER (PARTITION BY file ORDER BY seq) AS nth
  FROM stream WHERE kind = 'VFS' AND call = 'xSync'
)
SELECT x.seq, x.step, x.file, x.nth AS nth_xsync_of_file,
       count(s.seq) FILTER (s.file = x.file) AS own_file_syncs,
       count(s.seq) FILTER (s.file = '.') AS directory_syncs
FROM x LEFT JOIN stream s ON s.charged_to = x.seq AND s.kind = 'SYS' AND s.call = 'sync'
GROUP BY ALL
ORDER BY x.seq;

.print
.print 'C2. Syncs per file:'

SELECT CASE file WHEN '.' THEN 'directory (.)' ELSE file END AS file, count(*) AS syncs
FROM stream WHERE kind = 'SYS' AND call = 'sync'
GROUP BY file
ORDER BY file = '.', file;

.print
.print '── D. Creating and removing the sidecar files ──────────────────────────'
.print
.print 'D1. Every open and unlink of the -journal, -wal and -shm files, and the VFS'
.print 'call it is counted against: each file is created at its first use and'
.print 'removed by the VFS call that ends that use.'

SELECT s.seq, s.step, s.text AS syscall, v.call AS vfs_call
FROM stream s LEFT JOIN stream v ON v.seq = s.charged_to
WHERE s.kind = 'SYS' AND s.call IN ('open', 'unlink') AND s.file LIKE 't.db-%'
ORDER BY s.seq;

.print
.print 'D2. Files left in the directory after the CLI exits:'

SELECT name FROM files_left ORDER BY name;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 7.1
```

Add `-i` (`go run ./cmd/course run -i 7.1`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

`course run` also prints the run-dependent values, which are not part of the stored evidence: where the CLI build first asked the VFS for random bytes (`xRandomness`).

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/7.1-vfs-calls-to-syscalls/`](evidence/7.1-vfs-calls-to-syscalls/), and `go run ./cmd/course check evidence ch07` compares a fresh run with them byte for byte.

<!-- include: evidence/7.1-vfs-calls-to-syscalls.txt -->
```text

════════════════════════════════════════════════════════════════════════
  7.1  Which syscalls does each VFS call make?
════════════════════════════════════════════════════════════════════════

The pinned sqlite3 CLI runs one session with -vfstrace (each VFS call it
makes is printed as a line) under strace, which logs the syscalls on the
database and its -journal, -wal and -shm files and on the directory. The
two logs are merged into one ordered stream. mmap_size is 0, the SQLite
default. Traced: openat open close pwrite64 write writev pread64 read
fsync fdatasync unlink unlinkat fcntl ftruncate mmap munmap and the stat
and access families.

The session, one CLI process (.open --normal t.db first):
┌──────┬──────────────────────┬──────────────────────────────────────────────────────────┐
│ step │        label         │                           sql                            │
├──────┼──────────────────────┼──────────────────────────────────────────────────────────┤
│ 1    │ set WAL mode         │ PRAGMA journal_mode=WAL;                                 │
│ 2    │ set synchronous FULL │ PRAGMA synchronous=FULL;                                 │
│ 3    │ create a table       │ CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); │
│ 4    │ commit one insert    │ INSERT INTO t(id, v) VALUES(1, 'one');                   │
│ 5    │ checkpoint           │ PRAGMA wal_checkpoint(TRUNCATE);                         │
│ 6    │ close                │ (end of input: the CLI closes the database)              │
└──────┴──────────────────────┴──────────────────────────────────────────────────────────┘

Whether the syscalls were traced (strace needs Linux; elsewhere only the
VFS calls are recorded):
┌────────────────────┐
│      syscalls      │
├────────────────────┤
│ traced with strace │
└────────────────────┘

── A. The merged stream ────────────────────────────────────────────────

VFS = a line of -vfstrace output; indented SYS = a syscall made while
that VFS call ran; unindented SYS = a syscall outside any printed VFS call;
OUT = CLI output. Syscall names are normalized: open = open/openat,
sync = fsync/fdatasync, stat = the stat family, unlink = unlink/unlinkat.
xRandomness is left out of the stream: where SQLite first asks the VFS for
random bytes differs between CLI builds.

A1. The size of the stream:
┌───────────┬───────────────┬──────────────┬──────────────────────┐
│ vfs_calls │ file_syscalls │ output_lines │ xrandomness_left_out │
├───────────┼───────────────┼──────────────┼──────────────────────┤
│ 139       │ 143           │ 8            │ 1                    │
└───────────┴───────────────┴──────────────┴──────────────────────┘

A2. Opening t.db. vfstrace prints xOpen after the real open returns, so the
open and stat syscalls of xOpen come just before its line. The 100-byte
header read is short: the file is empty.
┌─────┬──────────────────────────────────────────────────────────┐
│ seq │                          event                           │
├─────┼──────────────────────────────────────────────────────────┤
│ 1   │ VFS xFullPathname("t.db") -> SQLITE_OK, out="t.db"       │
│ 2   │     SYS stat . = 0                                       │
│ 3   │     SYS stat t.db = -1 ENOENT                            │
│ 4   │ SYS open t.db O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd   │
│ 5   │ SYS stat t.db = 0                                        │
│ 6   │ SYS stat t.db = 0                                        │
│ 7   │ SYS stat t.db = 0                                        │
│ 8   │ VFS xOpen(t.db,flags=0x106) -> SQLITE_OK, outFlags=0x106 │
│ 9   │ VFS xDeviceCharacteristics(t.db) -> 0x00009000           │
│ 10  │ VFS xDeviceCharacteristics(t.db) -> 0x00009000           │
│ 11  │ VFS xFileControl(t.db,MMAP_SIZE,0) -> SQLITE_OK, 0       │
│ 12  │ VFS xDeviceCharacteristics(t.db) -> 0x00009000           │
│ 13  │ VFS xFileControl(t.db,MMAP_SIZE,0) -> SQLITE_OK, 0       │
│ 14  │ VFS xRead(t.db,n=100,ofst=0) -> SQLITE_IOERR_SHORT_READ  │
│ 15  │     SYS pread64 t.db off=0 len=100 = 0                   │
│ 16  │ VFS xFileControl(t.db,BUSYHANDLER) -> SQLITE_NOTFOUND    │
│ 17  │ VFS xFileControl(t.db,MMAP_SIZE,0) -> SQLITE_OK, 0       │
│ 18  │ VFS xFileControl(t.db,PDB) -> SQLITE_NOTFOUND            │
└─────┴──────────────────────────────────────────────────────────┘

A3. STEP 1, journal_mode=WAL. Changing the mode writes page 1 under a
rollback journal: the journal is created and synced twice (the first
time with a sync of the directory), page 1 is written and synced, and the
journal is deleted.
┌─────┬─────────────────────────────────────────────────────────────────────┐
│ seq │                                event                                │
├─────┼─────────────────────────────────────────────────────────────────────┤
│ 19  │ OUT STEP 1 set WAL mode                                             │
│ 20  │ VFS xFileControl(t.db,PRAGMA,[journal_mode,WAL]) -> SQLITE_NOTFOUND │
│ 21  │ VFS xLock(t.db,SHARED) -> SQLITE_OK                                 │
│ 22  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0       │
│ 23  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0     │
│ 24  │     SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0       │
│ 25  │ VFS xAccess("t.db-journal",0) -> SQLITE_OK, out=0                   │
│ 26  │     SYS stat t.db-journal = -1 ENOENT                               │
│ 27  │ VFS xAccess("t.db-wal",0) -> SQLITE_OK, out=0                       │
│ 28  │     SYS stat t.db-wal = -1 ENOENT                                   │
│ 29  │ VFS xFileSize(t.db) -> SQLITE_OK, size=0                            │
│ 30  │     SYS stat t.db = 0                                               │
│ 31  │ VFS xDeviceCharacteristics(t.db) -> 0x00009000                      │
│ 32  │ VFS xUnlock(t.db,NONE) -> SQLITE_OK                                 │
│ 33  │     SYS fcntl t.db F_SETLK F_UNLCK start=0 len=0 = 0                │
│ 34  │ VFS xLock(t.db,SHARED) -> SQLITE_OK                                 │
│ 35  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0       │
│ 36  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0     │
│ 37  │     SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0       │
│ 38  │ VFS xAccess("t.db-journal",0) -> SQLITE_OK, out=0                   │
│ 39  │     SYS stat t.db-journal = -1 ENOENT                               │
│ 40  │ VFS xRead(t.db,n=16,ofst=24) -> SQLITE_IOERR_SHORT_READ             │
│ 41  │     SYS pread64 t.db off=24 len=16 = 0                              │
│ 42  │ VFS xAccess("t.db-wal",0) -> SQLITE_OK, out=0                       │
│ 43  │     SYS stat t.db-wal = -1 ENOENT                                   │
│ 44  │ VFS xFileSize(t.db) -> SQLITE_OK, size=0                            │
│ 45  │     SYS stat t.db = 0                                               │
│ 46  │ VFS xLock(t.db,RESERVED) -> SQLITE_OK                               │
│ 47  │     SYS fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1 = 0       │
│ 48  │ VFS xLock(t.db,EXCLUSIVE) -> SQLITE_OK                              │
│ 49  │     SYS fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1 = 0       │
│ 50  │     SYS fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 = 0     │
│ 51  │ SYS stat t.db = 0                                                   │
│ 52  │ SYS open t.db-journal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd      │
│ 53  │ SYS stat t.db-journal = 0                                           │
│ 54  │ VFS xOpen(t.db-journal,flags=0x806) -> SQLITE_OK                    │
│ 55  │ VFS xDeviceCharacteristics(t.db) -> 0x00009000                      │
│ 56  │ VFS xWrite(t.db-journal,n=512,ofst=0) -> SQLITE_OK                  │
│ 57  │     SYS pwrite64 t.db-journal off=0 len=512 = 512                   │
│ 58  │ OUT wal                                                             │
│ 59  │ VFS xDeviceCharacteristics(t.db) -> 0x00009000                      │
│ 60  │ VFS xRead(t.db-journal,n=8,ofst=512) -> SQLITE_IOERR_SHORT_READ     │
│ 61  │     SYS pread64 t.db-journal off=512 len=8 = 0                      │
│ 62  │ VFS xSync(t.db-journal,FULL) -> 0                                   │
│ 63  │     SYS sync t.db-journal = 0                                       │
│ 64  │     SYS open . O_RDONLY|O_CLOEXEC = fd                              │
│ 65  │     SYS sync . = 0                                                  │
│ 66  │     SYS close . = 0                                                 │
│ 67  │ VFS xWrite(t.db-journal,n=12,ofst=0) -> SQLITE_OK                   │
│ 68  │     SYS pwrite64 t.db-journal off=0 len=12 = 12                     │
│ 69  │ VFS xSync(t.db-journal,FULL) -> 0                                   │
│ 70  │     SYS sync t.db-journal = 0                                       │
│ 71  │ VFS xFileControl(t.db,SIZE_HINT,4096) -> SQLITE_OK                  │
│ 72  │ VFS xWrite(t.db,n=4096,ofst=0) -> SQLITE_OK                         │
│ 73  │     SYS pwrite64 t.db off=0 len=4096 = 4096                         │
│ 74  │ VFS xFileControl(t.db,SYNC) -> SQLITE_NOTFOUND                      │
│ 75  │ VFS xSync(t.db,FULL) -> 0                                           │
│ 76  │     SYS sync t.db = 0                                               │
│ 77  │ VFS xClose(t.db-journal) -> SQLITE_OK                               │
│ 78  │     SYS close t.db-journal = 0                                      │
│ 79  │ VFS xDelete("t.db-journal",0) -> SQLITE_OK                          │
│ 80  │     SYS unlink t.db-journal = 0                                     │
│ 81  │ VFS xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND           │
│ 82  │ VFS xUnlock(t.db,SHARED) -> SQLITE_OK                               │
│ 83  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0     │
│ 84  │     SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2 = 0       │
│ 85  │ VFS xDeviceCharacteristics(t.db) -> 0x00009000                      │
│ 86  │ VFS xUnlock(t.db,NONE) -> SQLITE_OK                                 │
│ 87  │     SYS fcntl t.db F_SETLK F_UNLCK start=0 len=0 = 0                │
└─────┴─────────────────────────────────────────────────────────────────────┘

A4. STEP 2, synchronous=FULL, only tells the VFS about the pragma. STEP 3,
CREATE TABLE, is the first use of the WAL: xOpen creates -wal, the first
xShmMap creates -shm, extends it one byte per 4 KiB and maps it, and the
commit writes the WAL header, syncs it (with the directory), writes two
frames (24-byte frame header + 4096-byte page) and syncs again.
┌─────┬─────────────────────────────────────────────────────────────────────┐
│ seq │                                event                                │
├─────┼─────────────────────────────────────────────────────────────────────┤
│ 88  │ OUT STEP 2 set synchronous FULL                                     │
│ 89  │ VFS xFileControl(t.db,PRAGMA,[synchronous,FULL]) -> SQLITE_NOTFOUND │
│ 90  │ OUT STEP 3 create a table                                           │
│ 91  │ VFS xLock(t.db,SHARED) -> SQLITE_OK                                 │
│ 92  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0       │
│ 93  │     SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0     │
│ 94  │     SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0       │
│ 95  │ VFS xAccess("t.db-journal",0) -> SQLITE_OK, out=0                   │
│ 96  │     SYS stat t.db-journal = -1 ENOENT                               │
│ 97  │ VFS xRead(t.db,n=16,ofst=24) -> SQLITE_OK                           │
│ 98  │     SYS pread64 t.db off=24 len=16 = 16                             │
│ 99  │ VFS xAccess("t.db-wal",0) -> SQLITE_OK, out=0                       │
│ 100 │     SYS stat t.db-wal = -1 ENOENT                                   │
│ 101 │ VFS xFileSize(t.db) -> SQLITE_OK, size=4096                         │
│ 102 │     SYS stat t.db = 0                                               │
│ 103 │ SYS stat t.db = 0                                                   │
│ 104 │ SYS open t.db-wal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd          │
│ 105 │ SYS stat t.db-wal = 0                                               │
│ 106 │ VFS xOpen(t.db-wal,flags=0x80006) -> SQLITE_OK, outFlags=0x80006    │
│ 107 │ VFS xDeviceCharacteristics(t.db) -> 0x00009000                      │
│ 108 │ VFS xFileControl(t.db,MMAP_SIZE,0) -> SQLITE_OK, 0                  │
│ 109 │ VFS xShmMap(t.db,iRegion=0,szRegion=32768,isWrite=0,*) -> SQLITE_OK │
│ 110 │     SYS stat t.db = 0                                               │
│ 111 │     SYS open t.db-shm O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd      │
│ 112 │     SYS stat t.db-shm = 0                                           │
│ 113 │     SYS fcntl t.db-shm F_GETLK F_UNLCK start=128 len=1 = 0          │
│ 114 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=128 len=1 = 0          │
│ 115 │     SYS ftruncate t.db-shm len=3 = 0                                │
│ 116 │     SYS fcntl t.db-shm F_SETLK F_RDLCK start=128 len=1 = 0          │
│ 117 │     SYS stat t.db-shm = 0                                           │
│ 118 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 119 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 = 0          │
│ 120 │ VFS xShmMap(t.db,iRegion=0,szRegion=32768,isWrite=2,*) -> SQLITE_OK │
│ 121 │     SYS stat t.db-shm = 0                                           │
│ 122 │     SYS pwrite64 t.db-shm off=4095 len=1 = 1                        │
│ 123 │     SYS pwrite64 t.db-shm off=8191 len=1 = 1                        │
│ 124 │     SYS pwrite64 t.db-shm off=12287 len=1 = 1                       │
│ 125 │     SYS pwrite64 t.db-shm off=16383 len=1 = 1                       │
│ 126 │     SYS pwrite64 t.db-shm off=20479 len=1 = 1                       │
│ 127 │     SYS pwrite64 t.db-shm off=24575 len=1 = 1                       │
│ 128 │     SYS pwrite64 t.db-shm off=28671 len=1 = 1                       │
│ 129 │     SYS pwrite64 t.db-shm off=32767 len=1 = 1                       │
│ 130 │     SYS mmap t.db-shm off=0 len=32768 = ok                          │
│ 131 │ VFS xShmBarrier(t.db)                                               │
│ 132 │ VFS xShmLock(t.db,ofst=1(CKPT),n=2,LOCK|EXCLUSIVE) -> SQLITE_OK     │
│ 133 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=121 len=2 = 0          │
│ 134 │ VFS xFileSize(t.db-wal) -> SQLITE_OK, size=0                        │
│ 135 │     SYS stat t.db-wal = 0                                           │
│ 136 │ VFS xShmBarrier(t.db)                                               │
│ 137 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 138 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 = 0          │
│ 139 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 140 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0          │
│ 141 │ VFS xShmLock(t.db,ofst=5(READ2),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 142 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=125 len=1 = 0          │
│ 143 │ VFS xShmLock(t.db,ofst=5(READ2),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 144 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=125 len=1 = 0          │
│ 145 │ VFS xShmLock(t.db,ofst=6(READ3),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 146 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=126 len=1 = 0          │
│ 147 │ VFS xShmLock(t.db,ofst=6(READ3),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 148 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=126 len=1 = 0          │
│ 149 │ VFS xShmLock(t.db,ofst=7(READ4),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 150 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=127 len=1 = 0          │
│ 151 │ VFS xShmLock(t.db,ofst=7(READ4),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 152 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=127 len=1 = 0          │
│ 153 │ VFS xShmLock(t.db,ofst=1(CKPT),n=2,UNLOCK|EXCLUSIVE) -> SQLITE_OK   │
│ 154 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=121 len=2 = 0          │
│ 155 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 156 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 = 0          │
│ 157 │ VFS xShmLock(t.db,ofst=3(READ0),n=1,LOCK|SHARED) -> SQLITE_OK       │
│ 158 │     SYS fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1 = 0          │
│ 159 │ VFS xShmBarrier(t.db)                                               │
│ 160 │ VFS xFileSize(t.db) -> SQLITE_OK, size=4096                         │
│ 161 │     SYS stat t.db = 0                                               │
│ 162 │ VFS xRead(t.db,n=4096,ofst=0) -> SQLITE_OK                          │
│ 163 │     SYS pread64 t.db off=0 len=4096 = 4096                          │
│ 164 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 165 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 = 0          │
│ 166 │ VFS xShmLock(t.db,ofst=3(READ0),n=1,UNLOCK|SHARED) -> SQLITE_OK     │
│ 167 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1 = 0          │
│ 168 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK    │
│ 169 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 = 0          │
│ 170 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 171 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0          │
│ 172 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|SHARED) -> SQLITE_OK       │
│ 173 │     SYS fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1 = 0          │
│ 174 │ VFS xShmBarrier(t.db)                                               │
│ 175 │ VFS xWrite(t.db-wal,n=32,ofst=0) -> SQLITE_OK                       │
│ 176 │     SYS pwrite64 t.db-wal off=0 len=32 = 32                         │
│ 177 │ VFS xSync(t.db-wal,FULL) -> 0                                       │
│ 178 │     SYS sync t.db-wal = 0                                           │
│ 179 │     SYS open . O_RDONLY|O_CLOEXEC = fd                              │
│ 180 │     SYS sync . = 0                                                  │
│ 181 │     SYS close . = 0                                                 │
│ 182 │ VFS xWrite(t.db-wal,n=24,ofst=32) -> SQLITE_OK                      │
│ 183 │     SYS pwrite64 t.db-wal off=32 len=24 = 24                        │
│ 184 │ VFS xWrite(t.db-wal,n=4096,ofst=56) -> SQLITE_OK                    │
│ 185 │     SYS pwrite64 t.db-wal off=56 len=4096 = 4096                    │
│ 186 │ VFS xWrite(t.db-wal,n=24,ofst=4152) -> SQLITE_OK                    │
│ 187 │     SYS pwrite64 t.db-wal off=4152 len=24 = 24                      │
│ 188 │ VFS xWrite(t.db-wal,n=4096,ofst=4176) -> SQLITE_OK                  │
│ 189 │     SYS pwrite64 t.db-wal off=4176 len=4096 = 4096                  │
│ 190 │ VFS xSync(t.db-wal,FULL) -> 0                                       │
│ 191 │     SYS sync t.db-wal = 0                                           │
│ 192 │ VFS xShmBarrier(t.db)                                               │
│ 193 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK  │
│ 194 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 = 0          │
│ 195 │ VFS xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND           │
│ 196 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|SHARED) -> SQLITE_OK     │
│ 197 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0          │
└─────┴─────────────────────────────────────────────────────────────────────┘

A5. STEP 4, one INSERT: one frame and one sync of the -wal.
┌─────┬────────────────────────────────────────────────────────────────────┐
│ seq │                               event                                │
├─────┼────────────────────────────────────────────────────────────────────┤
│ 198 │ OUT STEP 4 commit one insert                                       │
│ 199 │ VFS xShmBarrier(t.db)                                              │
│ 200 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK   │
│ 201 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 = 0         │
│ 202 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK │
│ 203 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0         │
│ 204 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|SHARED) -> SQLITE_OK      │
│ 205 │     SYS fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1 = 0         │
│ 206 │ VFS xShmBarrier(t.db)                                              │
│ 207 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK   │
│ 208 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 = 0         │
│ 209 │ VFS xWrite(t.db-wal,n=24,ofst=8272) -> SQLITE_OK                   │
│ 210 │     SYS pwrite64 t.db-wal off=8272 len=24 = 24                     │
│ 211 │ VFS xWrite(t.db-wal,n=4096,ofst=8296) -> SQLITE_OK                 │
│ 212 │     SYS pwrite64 t.db-wal off=8296 len=4096 = 4096                 │
│ 213 │ VFS xSync(t.db-wal,FULL) -> 0                                      │
│ 214 │     SYS sync t.db-wal = 0                                          │
│ 215 │ VFS xShmBarrier(t.db)                                              │
│ 216 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK │
│ 217 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 = 0         │
│ 218 │ VFS xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND          │
│ 219 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|SHARED) -> SQLITE_OK    │
│ 220 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0         │
└─────┴────────────────────────────────────────────────────────────────────┘

A6. STEP 5, wal_checkpoint(TRUNCATE): sync the -wal, copy the page of each
frame into t.db (pread64 of the -wal, pwrite64 of t.db), truncate and sync t.db,
then truncate the -wal to 0 bytes.
┌─────┬────────────────────────────────────────────────────────────────────────────┐
│ seq │                                   event                                    │
├─────┼────────────────────────────────────────────────────────────────────────────┤
│ 221 │ OUT STEP 5 checkpoint                                                      │
│ 222 │ VFS xFileControl(t.db,PRAGMA,[wal_checkpoint,TRUNCATE]) -> SQLITE_NOTFOUND │
│ 223 │ VFS xShmLock(t.db,ofst=1(CKPT),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK            │
│ 224 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=121 len=1 = 0                 │
│ 225 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK           │
│ 226 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 = 0                 │
│ 227 │ VFS xShmBarrier(t.db)                                                      │
│ 228 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK           │
│ 229 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 = 0                 │
│ 230 │ VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK         │
│ 231 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0                 │
│ 232 │ VFS xShmLock(t.db,ofst=3(READ0),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK           │
│ 233 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=123 len=1 = 0                 │
│ 234 │ VFS xSync(t.db-wal,FULL) -> 0                                              │
│ 235 │     SYS sync t.db-wal = 0                                                  │
│ 236 │ VFS xFileControl(t.db,CKPT_START) -> SQLITE_NOTFOUND                       │
│ 237 │ VFS xFileSize(t.db) -> SQLITE_OK, size=4096                                │
│ 238 │     SYS stat t.db = 0                                                      │
│ 239 │ VFS xFileControl(t.db,SIZE_HINT,8192) -> SQLITE_OK                         │
│ 240 │ VFS xRead(t.db-wal,n=4096,ofst=56) -> SQLITE_OK                            │
│ 241 │     SYS pread64 t.db-wal off=56 len=4096 = 4096                            │
│ 242 │ VFS xWrite(t.db,n=4096,ofst=0) -> SQLITE_OK                                │
│ 243 │     SYS pwrite64 t.db off=0 len=4096 = 4096                                │
│ 244 │ VFS xRead(t.db-wal,n=4096,ofst=8296) -> SQLITE_OK                          │
│ 245 │     SYS pread64 t.db-wal off=8296 len=4096 = 4096                          │
│ 246 │ VFS xWrite(t.db,n=4096,ofst=4096) -> SQLITE_OK                             │
│ 247 │     SYS pwrite64 t.db off=4096 len=4096 = 4096                             │
│ 248 │ VFS xFileControl(t.db,CKPT_DONE) -> SQLITE_NOTFOUND                        │
│ 249 │ VFS xTruncate(t.db,8192) -> 0                                              │
│ 250 │     SYS ftruncate t.db len=8192 = 0                                        │
│ 251 │ VFS xSync(t.db,FULL) -> 0                                                  │
│ 252 │     SYS sync t.db = 0                                                      │
│ 253 │ VFS xShmLock(t.db,ofst=3(READ0),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK         │
│ 254 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1 = 0                 │
│ 255 │ VFS xShmLock(t.db,ofst=4(READ1),n=4,LOCK|EXCLUSIVE) -> SQLITE_OK           │
│ 256 │     SYS fcntl t.db-shm F_SETLK F_WRLCK start=124 len=4 = 0                 │
│ 257 │ VFS xShmBarrier(t.db)                                                      │
│ 258 │ VFS xTruncate(t.db-wal,0) -> 0                                             │
│ 259 │     SYS ftruncate t.db-wal len=0 = 0                                       │
│ 260 │ VFS xShmLock(t.db,ofst=4(READ1),n=4,UNLOCK|EXCLUSIVE) -> SQLITE_OK         │
│ 261 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=4 = 0                 │
│ 262 │ VFS xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK         │
│ 263 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 = 0                 │
│ 264 │ VFS xShmLock(t.db,ofst=1(CKPT),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK          │
│ 265 │     SYS fcntl t.db-shm F_SETLK F_UNLCK start=121 len=1 = 0                 │
│ 266 │ OUT 0|0|0                                                                  │
└─────┴────────────────────────────────────────────────────────────────────────────┘

A7. STEP 6, close: the last connection unlinks -shm (xShmUnmap) and -wal
(xDelete).
┌─────┬─────────────────────────────────────────────────────────────────┐
│ seq │                              event                              │
├─────┼─────────────────────────────────────────────────────────────────┤
│ 267 │ OUT STEP 6 close                                                │
│ 268 │ VFS xFileControl(t.db,HAS_MOVED) -> SQLITE_OK, 0                │
│ 269 │     SYS stat t.db = 0                                           │
│ 270 │ VFS xLock(t.db,EXCLUSIVE) -> SQLITE_OK                          │
│ 271 │     SYS fcntl t.db-shm F_GETLK F_UNLCK start=128 len=1 = 0      │
│ 272 │     SYS fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 = 0 │
│ 273 │ VFS xShmBarrier(t.db)                                           │
│ 274 │ VFS xFileControl(t.db,PERSIST_WAL,-1) -> SQLITE_OK, 0           │
│ 275 │ VFS xShmUnmap(t.db,delFlag=1) -> SQLITE_OK                      │
│ 276 │     SYS unlink t.db-shm = 0                                     │
│ 277 │     SYS close t.db-shm = 0                                      │
│ 278 │ VFS xClose(t.db-wal) -> SQLITE_OK                               │
│ 279 │     SYS close t.db-wal = 0                                      │
│ 280 │ VFS xDelete("t.db-wal",0) -> SQLITE_OK                          │
│ 281 │     SYS unlink t.db-wal = 0                                     │
│ 282 │ VFS xDeviceCharacteristics(t.db) -> 0x00009000                  │
│ 283 │ VFS xUnlock(t.db,NONE) -> SQLITE_OK                             │
│ 284 │     SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2 = 0   │
│ 285 │     SYS fcntl t.db F_SETLK F_UNLCK start=0 len=0 = 0            │
│ 286 │ VFS xClose(t.db) -> SQLITE_OK                                   │
│ 287 │     SYS stat t.db = 0                                           │
│ 288 │     SYS stat t.db = 0                                           │
│ 289 │     SYS close t.db = 0                                          │
└─────┴─────────────────────────────────────────────────────────────────┘

── B. Each VFS method and the syscalls behind it ───────────────────────

B1. Every VFS method, the os_unix.c function behind it, and the syscalls
seen while it ran (for xOpen: the syscalls just before its line). Several
methods make no syscall at all.
┌────────────────────────┬───────┬───────────────────────────┬──────────────────────────────────────────────────────────┐
│         method         │ calls │       unix_function       │                         syscalls                         │
├────────────────────────┼───────┼───────────────────────────┼──────────────────────────────────────────────────────────┤
│ xAccess                │ 6     │ unixAccess                │ stat×6                                                   │
│ xClose                 │ 3     │ unixClose                 │ close×3, stat×2                                          │
│ xDelete                │ 2     │ unixDelete                │ unlink×2                                                 │
│ xDeviceCharacteristics │ 9     │ unixDeviceCharacteristics │ no file syscall                                          │
│ xFileControl           │ 19    │ unixFileControl           │ stat×1                                                   │
│ xFileSize              │ 6     │ unixFileSize              │ stat×6                                                   │
│ xFullPathname          │ 1     │ unixFullPathname          │ stat×2                                                   │
│ xLock                  │ 6     │ unixLock                  │ fcntl×14                                                 │
│ xOpen                  │ 3     │ unixOpen                  │ open×3, stat×7                                           │
│ xRandomness            │ 1     │ unixRandomness            │ no file syscall                                          │
│ xRead                  │ 7     │ unixRead                  │ pread64×7                                                │
│ xShmBarrier            │ 11    │ unixShmBarrier            │ no file syscall                                          │
│ xShmLock               │ 36    │ unixShmLock               │ fcntl×36                                                 │
│ xShmMap                │ 2     │ unixShmMap                │ fcntl×3, ftruncate×1, mmap×1, open×1, pwrite64×8, stat×4 │
│ xShmUnmap              │ 1     │ unixShmUnmap              │ close×1, unlink×1                                        │
│ xSync                  │ 8     │ unixSync                  │ close×2, open×2, sync×10                                 │
│ xTruncate              │ 2     │ unixTruncate              │ ftruncate×2                                              │
│ xUnlock                │ 4     │ unixUnlock                │ fcntl×6                                                  │
│ xWrite                 │ 12    │ unixWrite                 │ pwrite64×12                                              │
└────────────────────────┴───────┴───────────────────────────┴──────────────────────────────────────────────────────────┘

B2. Every syscall that moves file bytes, by the VFS call it ran inside:
reads only in xRead, writes only in xWrite, and the one-byte writes that
extend -shm in xShmMap.
┌──────────┬─────────┬──────────┐
│ syscall  │ inside  │ syscalls │
├──────────┼─────────┼──────────┤
│ pread64  │ xRead   │ 7        │
│ pwrite64 │ xShmMap │ 8        │
│ pwrite64 │ xWrite  │ 12       │
└──────────┴─────────┴──────────┘

B3. VFS calls or syscalls that broke one of the rules in the checks below
(one pwrite64 per xWrite, one pread64 per xRead, one sync of its own file
per xSync, no byte I/O outside xRead, xWrite and xShmMap):
┌─────────────┐
│ rule_breaks │
├─────────────┤
│ 0           │
└─────────────┘

── C. Syncs ────────────────────────────────────────────────────────────

C1. Each xSync makes exactly one fsync/fdatasync of its own file. The first
xSync of a newly created journal or WAL also syncs the directory, so the
new directory entry is durable; no VFS call names the directory.
┌─────┬──────┬──────────────┬───────────────────┬────────────────┬─────────────────┐
│ seq │ step │     file     │ nth_xsync_of_file │ own_file_syncs │ directory_syncs │
├─────┼──────┼──────────────┼───────────────────┼────────────────┼─────────────────┤
│ 62  │ 1    │ t.db-journal │ 1                 │ 1              │ 1               │
│ 69  │ 1    │ t.db-journal │ 2                 │ 1              │ 0               │
│ 75  │ 1    │ t.db         │ 1                 │ 1              │ 0               │
│ 177 │ 3    │ t.db-wal     │ 1                 │ 1              │ 1               │
│ 190 │ 3    │ t.db-wal     │ 2                 │ 1              │ 0               │
│ 213 │ 4    │ t.db-wal     │ 3                 │ 1              │ 0               │
│ 234 │ 5    │ t.db-wal     │ 4                 │ 1              │ 0               │
│ 251 │ 5    │ t.db         │ 2                 │ 1              │ 0               │
└─────┴──────┴──────────────┴───────────────────┴────────────────┴─────────────────┘

C2. Syncs per file:
┌───────────────┬───────┐
│     file      │ syncs │
├───────────────┼───────┤
│ t.db          │ 2     │
│ t.db-journal  │ 2     │
│ t.db-wal      │ 4     │
│ directory (.) │ 2     │
└───────────────┴───────┘

── D. Creating and removing the sidecar files ──────────────────────────

D1. Every open and unlink of the -journal, -wal and -shm files, and the VFS
call it is counted against: each file is created at its first use and
removed by the VFS call that ends that use.
┌─────┬──────┬────────────────────────────────────────────────────────────┬───────────┐
│ seq │ step │                          syscall                           │ vfs_call  │
├─────┼──────┼────────────────────────────────────────────────────────────┼───────────┤
│ 52  │ 1    │ open t.db-journal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd │ xOpen     │
│ 80  │ 1    │ unlink t.db-journal = 0                                    │ xDelete   │
│ 104 │ 3    │ open t.db-wal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd     │ xOpen     │
│ 111 │ 3    │ open t.db-shm O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd     │ xShmMap   │
│ 276 │ 6    │ unlink t.db-shm = 0                                        │ xShmUnmap │
│ 281 │ 6    │ unlink t.db-wal = 0                                        │ xDelete   │
└─────┴──────┴────────────────────────────────────────────────────────────┴───────────┘

D2. Files left in the directory after the CLI exits:
┌──────┐
│ name │
├──────┤
│ t.db │
└──────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

B  ✓  each of the 12 xWrite calls made exactly one pwrite64 to the same file, offset and length
B  ✓  each of the 7 xRead calls made exactly one pread64 of the same file, offset and length
B  ✓  every pread64/pwrite64/read/write/writev on the database, journal, WAL and shm files happened inside xRead, xWrite or xShmMap
C  ✓  each of the 8 xSync calls made exactly one fsync/fdatasync of its own file
C  ✓  the directory is synced twice, inside the first xSync of the new journal and of the new WAL (unixSync's one-time directory sync)
D  ✓  the -wal file is created (O_CREAT) at its first use, by xOpen
D  ✓  the -shm file is created inside the first xShmMap
D  ✓  at the last close xDelete unlinks the -wal and xShmUnmap unlinks the -shm
D  ✓  only the database file is left after the CLI exits

9 of 9 claims hold

Experiment verdict: VFS CALLS TO SYSCALLS OK
```

#### Reading the Experiment 7.1 evidence

- **Opening table (pin checks and script).** Both the Go driver and the `sqlite3` CLI verify against SQLite `3.53.4` (`bf7c7f30…59bcc`; the pin lines are the experiment's own output, shown when it is run directly). The CLI opens `t.db` and executes six steps delimited by `.print STEP …` markers (which perform no file I/O): switch to `journal_mode=WAL`, set `synchronous=FULL`, create table `t`, insert one row, run `PRAGMA wal_checkpoint(TRUNCATE)`, and exit to close the database.
- **A1–A2 (opening `t.db`).** Across the session, the experiment records **139 VFS calls** and **143 file syscalls** (A1) on `t.db`, `t.db-journal`, `t.db-wal`, `t.db-shm`, and `.`. Stream rows 4–8 (the `seq` column) show why `xOpen`'s `open` and `stat` syscalls appear *unindented before* `VFS xOpen(t.db,flags=0x106)`: `vfstraceOpen` calls `unixOpen` before printing its line. Flags `0x106` equal `SQLITE_OPEN_MAIN_DB (0x100) | SQLITE_OPEN_CREATE (0x4) | SQLITE_OPEN_READWRITE (0x2)`. `xDeviceCharacteristics` returns `0x00009000` (`POWERSAFE_OVERWRITE | SUBPAGE_READ`), and `xFileControl(t.db,MMAP_SIZE,0)` (rows 10, 12, 16, and 108) confirms memory-mapped I/O is off. Rows 13–14 show `xRead(t.db,n=100,ofst=0)` issuing `pread64 t.db off=0 len=100 = 0` on the empty file and returning `SQLITE_IOERR_SHORT_READ`. The single `xRandomness(44)` call is omitted from the printed stream (A1 counts it as left out, and it remains in the B1 summary table; where it happened is a run-dependent value that `course run` prints) because an installed CLI calls it right after open whereas a `./configure` build calls it right before the first journal header. `fchown` is excluded from `tracedSyscalls` on purpose: `unixOpen` calls `robustFchown` on new sidecar files only when running as root.
- **A3 (STEP 1: `PRAGMA journal_mode=WAL`).** Switching an empty database to WAL mode is itself a rollback-mode transaction that writes page 1 with header bytes 18–19 set to `2`.
  - Rows 21–24 show `xLock(t.db,SHARED)` executing three `fcntl` calls: read-lock `PENDING_BYTE` (`1073741824`, len 1), read-lock the 510-byte shared range (`1073741826`, len 510), and release `PENDING_BYTE`.
  - Rows 25–28 show `xAccess("t.db-journal",0)` and `xAccess("t.db-wal",0)` each mapping to `stat` (`ENOENT`), not `access()`.
  - Rows 46–57 take `RESERVED` (`1073741825`) and `EXCLUSIVE` (`1073741824` and `1073741826..+510`), create `t.db-journal` (`flags=0x806` = `MAIN_JOURNAL | CREATE | READWRITE`), and write its 512-byte header.
  - Rows 62–66 show the hidden directory sync: `VFS xSync(t.db-journal,FULL)` encloses `sync t.db-journal` **plus** `open .`, `sync .`, and `close .` (`unixSync`'s one-time `UNIXFILE_DIRSYNC`). The second journal sync at rows 69–70 (after the 12-byte header update at row 67) syncs only `t.db-journal`, because `UNIXFILE_DIRSYNC` was cleared after the first sync.
  - Rows 72–80 write page 1 (`pwrite64 t.db off=0 len=4096`), sync `t.db`, close `t.db-journal`, and delete it (`xDelete` → `unlink`).
- **A4 (STEP 2 and STEP 3: `synchronous=FULL` and `CREATE TABLE`).** `PRAGMA synchronous=FULL` touches no files (row 89). `CREATE TABLE` is the first WAL write:
  - Rows 103–106 create `t.db-wal` with `O_CREAT` (`flags=0x80006` = `WAL | CREATE | READWRITE`).
  - Rows 109–130 show the first two `xShmMap` calls creating and mapping `t.db-shm`: `open` with `O_CREAT`, dead-man-switch `fcntl` calls on byte `128` (`F_GETLK`, `F_SETLK F_WRLCK`, `ftruncate len=3`, downgrade to `F_RDLCK`), eight 1-byte `pwrite64` calls at offsets `4095`, `8191`, …, `32767` to allocate the 32 KiB region, and `mmap t.db-shm off=0 len=32768`.
  - Rows 132–159 lock `CKPT`/`RECOVER` and `READ1`–`READ4` (`fcntl` on bytes `121..127`) during WAL-index initialization; rows 162–163 read page 1 (`pread64 t.db off=0 len=4096`).
  - Rows 175–181 write the 32-byte WAL header (`pwrite64 t.db-wal off=0 len=32`) and run the first `xSync(t.db-wal)`, which triggers the second directory sync (`open .`, `sync .`, `close .`).
  - Rows 182–191 append two frames (page 1 and page 2: 24-byte header + 4,096-byte page each at offsets `32`/`56` and `4152`/`4176`) and sync `t.db-wal`.
- **A5 (STEP 4: `INSERT INTO t VALUES(1, 'one')`).** Takes `READ1` shared (byte `124`) and `WRITE` exclusive (byte `120`), writes one frame (`n=24 ofst=8272` and `n=4096 ofst=8296`), syncs `t.db-wal`, and unlocks. Zero reads or writes touch `t.db`.
- **A6 (STEP 5: `PRAGMA wal_checkpoint(TRUNCATE)`).** Locks `CKPT` (byte `121`) and `WRITE` (byte `120`), syncs `t.db-wal` (row 234), reads the two latest page images from `t.db-wal` (`off=56` and `off=8296`), writes them to `t.db` (`off=0` and `off=4096`), truncates `t.db` to `8192`, syncs `t.db`, takes exclusive locks on `READ1`–`READ4` (`start=124 len=4`), truncates `t.db-wal` to `0`, unlocks, and prints `0|0|0`.
- **A7 (STEP 6: close).** Checks `HAS_MOVED` (`stat t.db`), takes `EXCLUSIVE` lock on `t.db` (probing `t.db-shm` byte `128` first), checks `PERSIST_WAL` (returns `0`), calls `xShmUnmap(delFlag=1)` (`unlink t.db-shm` then `close`), closes `t.db-wal`, calls `xDelete("t.db-wal", 0)` (`unlink t.db-wal`), unlocks, and closes `t.db`.
- **Sections B–D (summary table, sync counts, and checks).** Every one of the 19 VFS methods maps to its `unix*` function and exact syscall counts: `xWrite` (12 calls → 12 `pwrite64`), `xRead` (7 → 7 `pread64`), `xSync` (8 → 10 `sync`, 2 `open`, 2 `close`), `xShmMap` (2 → `open`, `fcntl`×3, `ftruncate`, `pwrite64`×8, `mmap`, `stat`×4), `xShmLock` (36 → 36 `fcntl`), `xAccess` (6 → 6 `stat`), `xDelete` (2 → 2 `unlink`). No `pread64`, `pwrite64`, `read`, `write`, or `writev` syscall on the database, journal, WAL, or shm files occurred outside `xRead`, `xWrite`, or `xShmMap`. That 1-to-1 `xRead` → `pread64` mapping holds because `PRAGMA mmap_size=0`; with `PRAGMA mmap_size > 0`, reads still cross the VFS (`xFetch` and `unixRead`'s mapped `memcpy`) without issuing `pread64`. All 10 sync syscalls are accounted for (`t.db` 2, `t.db-journal` 2, `t.db-wal` 4, directory `.` 2), only `t.db` remains on disk, and all 9 checks pass.
- **Portable non-Linux path.** On macOS, `portable()` checks `-wal` open/delete and `xShmUnmap(..., delFlag=1)` through `closeLifecycle`, verified against real `-vfstrace` lines in [`7.1-vfs-calls-to-syscalls/main_test.go`](experiments/7.1-vfs-calls-to-syscalls/main_test.go).

---

## 4. Timeline

Combining Experiment 7.1's syscall stream with Experiment 7.3 Part A's exact per-commit VFS call lists gives a side-by-side timeline for one committed single-row `INSERT` (after the table already exists and, in WAL mode, the WAL file is already open):

| Phase | Rollback (`DELETE`), `FULL` | WAL, `FULL` | WAL, `NORMAL` |
| --- | --- | --- | --- |
| **1. Begin & locks** | `xLock(t.db, SHARED)` (`fcntl` bytes `1073741824`, `1073741826..+510`), `xAccess` `-journal` & `-wal` (`stat`), `xRead` 16 B at offset 24 (change counter), `xFileSize`, `xLock(RESERVED)` (`1073741825`), `xFileControl(HAS_MOVED)` | `xShmBarrier`, `xShmLock(READ1)` exclusive→unlock→shared (`fcntl` byte `124`), `xShmBarrier`, `xShmLock(WRITE)` exclusive (`fcntl` byte `120`) | Identical to WAL, `FULL` |
| **2. Journal / WAL write** | `xOpen(t.db-journal, 0x806)` (`open O_CREAT`), write 512 B header + page 1 record (`4+4096+4` B), `xLock(EXCLUSIVE)`, page 2 record (`4+4096+4` B), short-read probe at `9216`, `xSync(t.db-journal)` (**syncs journal + opens/syncs/closes `.`**), rewrite 12 B header at `0`, second `xSync(t.db-journal)` | `xWrite(t.db-wal, 24 B)` frame header + `xWrite(t.db-wal, 4096 B)` page image (`2` `pwrite64` calls) | Identical to WAL, `FULL` |
| **3. Main DB write** | `xWrite(t.db, 4096 B, off=0)` (page 1) + `xWrite(t.db, 4096 B, off=4096)` (page 2), `xFileControl(SYNC)`, `xSync(t.db)` | None | None |
| **4. Commit & end** | `xClose(t.db-journal)`, `xDelete(t.db-journal)` (`unlink`), `xFileControl(COMMIT_PHASETWO)`, `xUnlock(SHARED)` → `xUnlock(NONE)` | `xSync(t.db-wal)` (`1` sync), `xShmBarrier`, `xShmLock(WRITE)` unlock, `xFileControl(COMMIT_PHASETWO)`, `xShmLock(READ1)` unlock | No `xSync`; `xShmBarrier`, `xShmLock(WRITE)` unlock, `xFileControl(COMMIT_PHASETWO)`, `xShmLock(READ1)` unlock |
| **Totals (VFS calls / sync syscalls)** | **32 VFS calls / 4 sync syscalls** (`3` `xSync` + `1` dir sync) | **13 VFS calls / 1 sync syscall** | **12 VFS calls / 0 sync syscalls** |

In rollback `DELETE` mode at `NORMAL`, step 2 skips the first journal `xSync` (writing the 12-byte header and syncing the journal once, which still runs the directory sync), giving **31 VFS calls / 3 sync syscalls** (`2` `xSync` + `1` dir sync).

In WAL mode, two occasional commits break the steady-state pattern above (proven in Experiment 7.2):

1. **The automatic-checkpoint commit** (when the WAL reaches `wal_autocheckpoint`, 1,000 frames by default): after appending the commit's frames, `walCheckpoint` syncs `-wal`, copies pages back to `.db`, truncates `.db` if needed, and syncs `.db`. That adds **+2 sync syscalls** (`wal:1`, `db:1`)—giving `2` syncs at `NORMAL` and `3` at `FULL`.
2. **The WAL-restart commit** (the very next commit after a full checkpoint, when no reader pins old frames): `walRestartLog` resets the WAL to frame 0 with a new salt, writes a fresh 32-byte WAL header, and `syncHeader` syncs `-wal` before appending frames. That adds **+1 sync syscall** (`wal:1`)—giving `1` sync at `NORMAL` and `2` at `FULL`.

**Sidecar file lifecycle across a session:**

```text
  Action                  Rollback (DELETE)                  WAL
  -----------------------------------------------------------------------------------
  open()                  t.db only                          t.db only
  first read              t.db only                          t.db only (until WAL exists)
  every write commit      creates t.db-journal (+dir sync),  first write creates t.db-wal
                          unlinks t.db-journal at commit     (+dir sync) & t.db-shm;
                                                             later commits append to t.db-wal
  last connection close   0 writes, 0 syncs                  PASSIVE checkpoint (copies pages,
                                                             2 syncs), unlinks t.db-shm & t.db-wal
```

---

## 5. Lab

This section runs three experiments and introduces two shared Go packages:

- **5.1 Experiment 7.2** measures exact syscall I/O costs across 12 combinations of journal mode, `synchronous`, and transaction batching.
- **5.2 `internal/govfs`** and **5.3 Experiment 7.3** build and verify a pure-Go forwarding VFS on `modernc.org/sqlite` for in-process call counting and deterministic I/O fault injection.
- **5.4 `internal/rangefs`** and **5.5 Experiment 7.4** serve and query a checkpointed WAL snapshot over HTTP and SeaweedFS S3 through `modernc.org/sqlite/vfs`.

### 5.1 Experiment 7.2 — I/O cost by write pattern (`7.2-io-cost-by-pattern`)

- **Claim:** Inserting the exact same 1,000 rows costs sharply different numbers of writes, bytes, file creates/deletes, and syncs depending on transaction batching, journal mode, and `synchronous`, and every count is exact and derivable from SQLite's commit and checkpoint paths. In WAL mode at `synchronous=NORMAL`, ordinary commits perform 0 syncs, except the commit that triggers an automatic checkpoint (2 syncs) and the immediately following commit that restarts the WAL (1 sync).
- **Evidence form:** `EXACT` (every stored table must match on every run).
- **Why kept:** This is the I/O cost model for a local disk shared by thousands of databases, and the WAL/`NORMAL` per-commit sync behavior is the foundation that Experiment 9.2 (crash versus power loss) builds on. Measuring every commit individually also refutes the common misconception that WAL/`NORMAL` syncs on every commit, while correcting the oversimplified claim that WAL/`NORMAL` never syncs on commit.
- **Platform:** **Linux only** (`strace`). Run directly on macOS, the program runs all 12 workloads uncounted; `course run` runs the full experiment in the Linux `lab` container.

<!-- include: experiments/7.2-io-cost-by-pattern/main.go -->
```go
// Experiment 7.2 measures exact pwrite64 calls, bytes written, fsync/fdatasync
// calls, opens, unlinks, and ftruncates for inserting 1,000 rows across 12
// configurations: journal_mode {delete, wal} x synchronous {NORMAL, FULL} x
// batching {1x1000, 20x50, 1000x1}. Each cell runs a child Go process under
// Linux strace and splits the syscall log per commit using a marker file.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.2 (strace needs Linux, which
// the lab container provides).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const (
	packagePath   = "ch07-vfs-and-filesystem-io/experiments/7.2-io-cost-by-pattern"
	totalRows     = 1000
	valueBytes    = 100
	dbName        = "t.db"
	markerName    = "commit-marker"
	parallelCells = 4
)

type cell struct {
	journal string
	sync    string
	batch   int
}

func (c cell) commits() int {
	return totalRows / c.batch
}

func (c cell) label() string {
	return fmt.Sprintf("%-6s %-6s %4d x %-4d", c.journal, c.sync, c.commits(), c.batch)
}

func (c cell) dirName() string {
	return fmt.Sprintf("%s-%s-%d", c.journal, strings.ToLower(c.sync), c.batch)
}

func cells() []cell {
	var out []cell
	for _, journal := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		for _, sync := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
			for _, batch := range []int{totalRows, 50, 1} {
				out = append(out, cell{journal, sync, batch})
			}
		}
	}
	return out
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.2" runs both.
var set *results.Set

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		child(os.Args[2:])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	base := sqlitex.MustWorkDir("7.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(base))
	set = results.MustOpen(base)
	cellTable := set.Table("cells", "cell", "journal", "sync", "commits", "rows_per_commit")
	for i, c := range cells() {
		cellTable.Row(i+1, c.journal, c.sync, c.commits(), c.batch)
	}
	tracing := set.Table("tracing", "syscalls")
	if !trace.Supported {
		tracing.Row(trace.LinuxOnlyMessage("counting writes, bytes and syncs with strace", packagePath))
		for _, c := range cells() {
			dir := filepath.Join(base, c.dirName())
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fatal("mkdir", err)
			}
			if err := runWorkload(ctx, dir, c, func() {}); err != nil {
				fatal(c.label(), err)
			}
		}
		trace.PrintLinuxOnly("counting writes, bytes and syncs with strace", packagePath)
		set.Finish("IO COST BY PATTERN")
		return
	}
	tracing.Row("traced with strace")
	measured := map[cell]*measurement{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallelCells)
	errs := map[cell]error{}
	for _, c := range cells() {
		dir := filepath.Join(base, c.dirName())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			m, err := measure(ctx, dir, c)
			mu.Lock()
			defer mu.Unlock()
			measured[c] = m
			errs[c] = err
		}()
	}
	wg.Wait()
	for _, c := range cells() {
		if errs[c] != nil {
			fatal(c.label(), errs[c])
		}
	}
	report(measured)
	set.Finish("IO COST BY PATTERN")
}

func options(c cell) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = c.journal
	o.Conn.Synchronous = c.sync
	o.SingleConnection = true
	return o
}

func value(id int) []byte {
	v := make([]byte, valueBytes)
	for i := range v {
		v[i] = byte((id*31 + i*7) % 251)
	}
	return v
}

func runWorkload(ctx context.Context, dir string, c cell, afterCommit func()) error {
	path := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, path, options(c))
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB NOT NULL)"); err != nil {
		db.Close()
		return err
	}
	afterCommit()
	id := 0
	for range c.commits() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			return err
		}
		for range c.batch {
			id++
			if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", id, value(id)); err != nil {
				tx.Rollback()
				db.Close()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			return err
		}
		afterCommit()
	}
	if err := db.Close(); err != nil {
		return err
	}
	afterCommit()
	return nil
}

// child runs one workload cell inside a dedicated straced process, pinning its
// goroutine to one OS thread and opening markerName after CREATE, after every
// commit, and after Close so the parent can segment the strace log per commit.
func child(args []string) {
	runtime.LockOSThread()
	if len(args) != 4 {
		fatal("child", errors.New("usage: child DIR JOURNAL SYNC BATCH"))
	}
	batch, err := strconv.Atoi(args[3])
	if err != nil {
		fatal("child", err)
	}
	dir := args[0]
	marker := filepath.Join(dir, markerName)
	touch := func() {
		if _, err := os.ReadFile(marker); err != nil {
			fatal("marker", err)
		}
	}
	if err := runWorkload(context.Background(), dir, cell{args[1], args[2], batch}, touch); err != nil {
		fatal("child", err)
	}
}

type counts struct {
	writes    int
	bytes     int64
	syncs     map[string]int
	opens     map[string]int
	unlinks   map[string]int
	truncates map[string]int
}

func newCounts() *counts {
	return &counts{syncs: map[string]int{}, opens: map[string]int{}, unlinks: map[string]int{}, truncates: map[string]int{}}
}

func (c *counts) totalSyncs() int {
	n := 0
	for _, v := range c.syncs {
		n += v
	}
	return n
}

func fileRole(path string) string {
	switch path {
	case dbName:
		return "db"
	case dbName + "-journal":
		return "journal"
	case dbName + "-wal":
		return "wal"
	case dbName + "-shm":
		return "shm"
	case ".":
		return "dir"
	}
	return ""
}

func (c *counts) add(s trace.Syscall) {
	role := fileRole(s.Path)
	if role == "" || !s.Succeeded() {
		return
	}
	switch trace.CanonicalName(s.Name) {
	case "pwrite64":
		c.writes++
		if s.Result > 0 {
			c.bytes += s.Result
		}
	case "sync":
		c.syncs[role]++
	case "open":
		c.opens[role]++
	case "unlink":
		c.unlinks[role]++
	case "ftruncate":
		c.truncates[role]++
	}
}

type commitShape struct {
	syncs      string
	dbWrites   bool
	totalSyncs int
}

// measurement holds one cell's counts: all sums the commits, segments has one
// entry per commit and a last one for the close, and dbWrites says whether that
// segment wrote the database file.
type measurement struct {
	all      *counts
	segments []*counts
	dbWrites []bool
	sequence []commitShape
}

// classifyWALCommits verifies every WAL commit fits the plain, checkpoint, or
// post-checkpoint restart shape and that at least one checkpoint+restart pair
// occurred (preventing vacuous passes if wal_autocheckpoint never triggered).
func classifyWALCommits(sequences [][]commitShape, plain, restart, checkpoint string) (ok bool, checkpoints, restarts int) {
	ok = true
	for _, sequence := range sequences {
		for i, s := range sequence {
			switch {
			case !s.dbWrites && s.syncs == plain:
			case s.dbWrites && s.syncs == checkpoint:
				checkpoints++
			case !s.dbWrites && s.syncs == restart && i > 0 && sequence[i-1].dbWrites:
				restarts++
			default:
				ok = false
			}
		}
	}
	return ok && checkpoints > 0 && restarts == checkpoints, checkpoints, restarts
}

func roleList(m map[string]int) string {
	roles := make([]string, 0, len(m))
	for r, n := range m {
		if n > 0 {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = fmt.Sprintf("%s:%d", r, m[r])
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func measure(ctx context.Context, dir string, c cell) (*measurement, error) {
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0o644); err != nil {
		return nil, err
	}
	command, err := trace.SelfCommand("child", dir, c.journal, c.sync, strconv.Itoa(c.batch))
	if err != nil {
		return nil, err
	}
	result, err := trace.Run(ctx, trace.Options{Command: command, Dir: dir, Syscalls: trace.DefaultSyscalls})
	if errors.Is(err, trace.ErrPtraceDenied) {
		return nil, fmt.Errorf("%w (run: docker compose run --rm lab go run ./%s)", err, packagePath)
	}
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("child exit %d: %s%s", result.ExitCode, result.Stdout, result.Stderr)
	}
	calls := trace.RelativeTo(dir, result.Log.Calls)
	var segments [][]trace.Syscall
	var current []trace.Syscall
	started := false
	for _, s := range calls {
		if trace.CanonicalName(s.Name) == "open" && s.Path == markerName {
			if started {
				segments = append(segments, current)
			}
			started = true
			current = nil
			continue
		}
		if started {
			current = append(current, s)
		}
	}
	if len(segments) != c.commits()+1 {
		return nil, fmt.Errorf("found %d marker segments, want %d commits + close", len(segments), c.commits())
	}
	m := &measurement{all: newCounts()}
	for i, seg := range segments {
		target := newCounts()
		dbWrites := false
		for _, s := range seg {
			target.add(s)
			if i < c.commits() {
				m.all.add(s)
			}
			if trace.CanonicalName(s.Name) == "pwrite64" && s.Path == dbName {
				dbWrites = true
			}
		}
		m.segments = append(m.segments, target)
		m.dbWrites = append(m.dbWrites, dbWrites)
		if i < c.commits() {
			m.sequence = append(m.sequence, commitShape{syncs: roleList(target.syncs), dbWrites: dbWrites, totalSyncs: target.totalSyncs()})
		}
	}
	return m, nil
}

// roles and kinds name the per-file columns of the segments table, such as
// syncs_db or unlinks_wal.
var (
	roles = []string{"db", "dir", "journal", "shm", "wal"}
	kinds = []struct {
		name   string
		counts func(*counts) map[string]int
	}{
		{"syncs", func(c *counts) map[string]int { return c.syncs }},
		{"opens", func(c *counts) map[string]int { return c.opens }},
		{"unlinks", func(c *counts) map[string]int { return c.unlinks }},
		{"ftruncates", func(c *counts) map[string]int { return c.truncates }},
	}
)

func report(measured map[cell]*measurement) {
	columns := []string{"cell", "seq", "phase", "pwrites", "bytes", "db_writes"}
	for _, k := range kinds {
		for _, r := range roles {
			columns = append(columns, k.name+"_"+r)
		}
	}
	table := set.Table("segments", columns...)
	for i, c := range cells() {
		m := measured[c]
		for j, seg := range m.segments {
			phase := "commit"
			if j == c.commits() {
				phase = "close"
			}
			row := []any{i + 1, j + 1, phase, seg.writes, seg.bytes, m.dbWrites[j]}
			for _, k := range kinds {
				for _, r := range roles {
					row = append(row, k.counts(seg)[r])
				}
			}
			table.Row(row...)
		}
	}

	expectEvery := func(c cell, want string) bool {
		sequence := measured[c].sequence
		for _, s := range sequence {
			if s != sequence[0] {
				return false
			}
		}
		return len(sequence) > 0 && sequence[0].syncs == want
	}
	for _, sync := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
		want := "db:1 dir:1 journal:1"
		n := 3
		if sync == sqlitex.SyncFull {
			want = "db:1 dir:1 journal:2"
			n = 4
		}
		ok := true
		for _, batch := range []int{totalRows, 50, 1} {
			ok = ok && expectEvery(cell{sqlitex.JournalDelete, sync, batch}, want)
		}
		set.Check("B", ok, fmt.Sprintf("rollback (DELETE) at %s: every commit, at every batch size, makes %d syncs [%s]", sync, n, want))
	}
	for _, sync := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
		plain, restart, checkpoint := walShapes(sync)
		var sequences [][]commitShape
		for _, batch := range []int{totalRows, 50, 1} {
			sequences = append(sequences, measured[cell{sqlitex.JournalWAL, sync, batch}].sequence)
		}
		ok, checkpoints, restarts := classifyWALCommits(sequences, plain, restart, checkpoint)
		set.Check("B", ok, fmt.Sprintf("WAL at %s: a commit makes [%s]; the commit that runs an automatic checkpoint makes [%s] (%d such), and the next commit, which restarts the WAL and writes a new WAL header, makes [%s] (%d such)", sync, plain, checkpoint, checkpoints, restart, restarts))
	}
	one := measured[cell{sqlitex.JournalDelete, sqlitex.SyncFull, 1}].all.totalSyncs()
	all := measured[cell{sqlitex.JournalDelete, sqlitex.SyncFull, totalRows}].all.totalSyncs()
	set.Check("A", one == 1000*all, fmt.Sprintf("rollback FULL: 1000 one-row commits cost %d syncs, one 1000-row commit costs %d (x%d)", one, all, one/max(all, 1)))
	walOne := measured[cell{sqlitex.JournalWAL, sqlitex.SyncNormal, 1}].all
	delOne := measured[cell{sqlitex.JournalDelete, sqlitex.SyncNormal, 1}].all
	set.Check("A", walOne.totalSyncs() < delOne.totalSyncs() && walOne.bytes < delOne.bytes, fmt.Sprintf("1000 one-row commits at NORMAL: WAL %d syncs and %d bytes written, rollback %d syncs and %d bytes", walOne.totalSyncs(), walOne.bytes, delOne.totalSyncs(), delOne.bytes))
}

// walShapes returns the expected per-commit sync patterns in WAL mode:
// plain commits sync nothing at NORMAL (wal:1 at FULL), an automatic checkpoint
// commit syncs both WAL and db (wal.c:2277, 2333), and the immediately following
// restart commit writes and syncs a fresh WAL header (wal.c:3875-3915, 4114-4124).
func walShapes(sync string) (plain, restart, checkpoint string) {
	if sync == sqlitex.SyncFull {
		return "wal:1", "wal:2", "db:1 wal:2"
	}
	return "-", "wal:1", "db:1 wal:1"
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/7.2-io-cost-by-pattern/report.sql -->
```sql
-- Walkthrough of experiment 7.2. Run it with:
--   go run ./cmd/course run 7.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   cells      one row per configuration: journal mode, synchronous, batching
--   tracing    one row: whether the syscalls were traced (Linux) or skipped
--   segments   one row per commit of a cell, and one for its close (phase), with
--              the successful pwrite64 calls, bytes written, whether the
--              database file was written, and the syncs, opens, unlinks and
--              ftruncates per file (columns <kind>_<file>; dir = the directory)
--   checks     every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  7.2  What does a commit cost in writes, bytes and syncs?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Workload: 1000 rows (id INTEGER PRIMARY KEY, v BLOB of 100 bytes) into an'
.print 'empty table, Go driver, one connection. Settings: page_size 4096,'
.print 'cache_size -2000, wal_autocheckpoint 1000, secure_delete OFF; the table is'
.print 'created before measuring. Each cell runs in its own process under strace,'
.print 'which opens a marker file after every commit so the syscall log splits'
.print 'per commit. Only successful syscalls on the database, its -journal, -wal'
.print 'and -shm and the directory count; sync = fsync or fdatasync.'
.print
.print 'The 12 cells: journal mode x synchronous x batching.'

SELECT cell, journal, sync, commits, rows_per_commit FROM cells ORDER BY cell;

.print
.print 'Whether the syscalls were traced (strace needs Linux; elsewhere the'
.print 'workloads run without it and nothing is counted):'

SELECT syscalls FROM tracing ORDER BY syscalls;

-- One file-by-file list, such as "db:1 dir:1 journal:1", or "-" for none.
CREATE TEMP MACRO by_file(db, dir, journal, shm, wal) AS
  coalesce(nullif(concat_ws(' ',
    CASE WHEN db > 0 THEN 'db:' || db END,
    CASE WHEN dir > 0 THEN 'dir:' || dir END,
    CASE WHEN journal > 0 THEN 'journal:' || journal END,
    CASE WHEN shm > 0 THEN 'shm:' || shm END,
    CASE WHEN wal > 0 THEN 'wal:' || wal END), ''), '-');

CREATE TEMP VIEW labelled AS
SELECT s.*, c.journal, c.sync, c.commits || ' x ' || c.rows_per_commit AS batching,
       s.syncs_db + s.syncs_dir + s.syncs_journal + s.syncs_shm + s.syncs_wal AS syncs
FROM segments s JOIN cells c USING (cell);

CREATE TEMP VIEW totals AS
SELECT cell, journal, sync, batching,
       sum(pwrites) AS pwrites, sum(bytes) AS bytes, sum(syncs) AS syncs,
       by_file(sum(syncs_db), sum(syncs_dir), sum(syncs_journal), sum(syncs_shm), sum(syncs_wal)) AS syncs_by_file,
       by_file(sum(opens_db), sum(opens_dir), sum(opens_journal), sum(opens_shm), sum(opens_wal)) AS opens,
       by_file(sum(unlinks_db), sum(unlinks_dir), sum(unlinks_journal), sum(unlinks_shm), sum(unlinks_wal)) AS unlinks,
       by_file(sum(ftruncates_db), sum(ftruncates_dir), sum(ftruncates_journal), sum(ftruncates_shm), sum(ftruncates_wal)) AS ftruncates
FROM labelled
WHERE phase = 'commit'
GROUP BY ALL;

.print
.print '── A. Totals while the rows were written ───────────────────────────────'
.print
.print 'A1. All commits of each cell (the close is not included). Rollback mode'
.print 'pays 3 or 4 syncs per commit, so its syncs grow with the number of'
.print 'commits; WAL at NORMAL syncs only around the automatic checkpoint.'

SELECT journal, sync, batching, pwrites, bytes, syncs, syncs_by_file
FROM totals ORDER BY cell;

.print
.print 'A2. Files opened, unlinked and truncated: rollback mode creates and deletes'
.print 'the journal at every commit and opens the directory to sync it; WAL mode'
.print 'opens nothing while the rows are written and truncates the database once,'
.print 'at the automatic checkpoint.'

SELECT journal, sync, batching, opens, unlinks, ftruncates
FROM totals ORDER BY cell;

.print
.print 'A3. The same 1000 rows in one commit and in 1000 commits:'

SELECT a.journal, a.sync,
       a.syncs AS syncs_1x1000, b.syncs AS syncs_1000x1,
       round(b.syncs / nullif(a.syncs, 0), 1) AS syncs_ratio,
       a.bytes AS bytes_1x1000, b.bytes AS bytes_1000x1,
       round(b.bytes / a.bytes, 1) AS bytes_ratio
FROM totals a JOIN totals b USING (journal, sync)
WHERE a.batching = '1 x 1000' AND b.batching = '1000 x 1'
ORDER BY a.cell;

.print
.print '── B. Syncs per commit ─────────────────────────────────────────────────'
.print
.print 'B1. How many commits had each sync pattern. db_writes = the commit also'
.print 'wrote database pages: always in rollback mode, and in WAL mode only in the'
.print 'commit that ran an automatic checkpoint.'

SELECT journal, sync, batching, count(*) AS commits, syncs,
       by_file(syncs_db, syncs_dir, syncs_journal, syncs_shm, syncs_wal) AS pattern,
       db_writes
FROM labelled
WHERE phase = 'commit'
GROUP BY ALL
ORDER BY min(cell), min(seq);

.print
.print 'B2. In WAL mode, the commits that do not have the plain pattern: the'
.print 'commit that crosses wal_autocheckpoint pages runs a checkpoint (syncs the'
.print '-wal and the database), and the next one restarts the WAL with a new'
.print 'header (one more sync of the -wal).'

SELECT journal, sync, batching, seq AS commit_no, pwrites, bytes, syncs,
       by_file(syncs_db, syncs_dir, syncs_journal, syncs_shm, syncs_wal) AS pattern,
       db_writes
FROM labelled
WHERE phase = 'commit' AND journal = 'wal'
  AND (db_writes OR syncs <> CASE sync WHEN 'FULL' THEN 1 ELSE 0 END)
ORDER BY cell, seq;

.print
.print '── C. At close ─────────────────────────────────────────────────────────'
.print
.print 'C1. Closing the connection after the last commit. Rollback mode has'
.print 'nothing left to do; in WAL mode the last connection checkpoints, syncs,'
.print 'truncates the database and unlinks the -wal and -shm.'

SELECT journal, sync, batching, pwrites, bytes, syncs,
       by_file(syncs_db, syncs_dir, syncs_journal, syncs_shm, syncs_wal) AS syncs_by_file,
       by_file(unlinks_db, unlinks_dir, unlinks_journal, unlinks_shm, unlinks_wal) AS unlinks,
       by_file(ftruncates_db, ftruncates_dir, ftruncates_journal, ftruncates_shm, ftruncates_wal) AS ftruncates
FROM labelled
WHERE phase = 'close'
ORDER BY cell;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 7.2
```

Add `-i` (`go run ./cmd/course run -i 7.2`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/7.2-io-cost-by-pattern/`](evidence/7.2-io-cost-by-pattern/), and `go run ./cmd/course check evidence ch07` compares a fresh run with them byte for byte.

<!-- include: evidence/7.2-io-cost-by-pattern.txt -->
```text

════════════════════════════════════════════════════════════════════════
  7.2  What does a commit cost in writes, bytes and syncs?
════════════════════════════════════════════════════════════════════════

Workload: 1000 rows (id INTEGER PRIMARY KEY, v BLOB of 100 bytes) into an
empty table, Go driver, one connection. Settings: page_size 4096,
cache_size -2000, wal_autocheckpoint 1000, secure_delete OFF; the table is
created before measuring. Each cell runs in its own process under strace,
which opens a marker file after every commit so the syscall log splits
per commit. Only successful syscalls on the database, its -journal, -wal
and -shm and the directory count; sync = fsync or fdatasync.

The 12 cells: journal mode x synchronous x batching.
┌──────┬─────────┬────────┬─────────┬─────────────────┐
│ cell │ journal │  sync  │ commits │ rows_per_commit │
├──────┼─────────┼────────┼─────────┼─────────────────┤
│ 1    │ delete  │ NORMAL │ 1       │ 1000            │
│ 2    │ delete  │ NORMAL │ 20      │ 50              │
│ 3    │ delete  │ NORMAL │ 1000    │ 1               │
│ 4    │ delete  │ FULL   │ 1       │ 1000            │
│ 5    │ delete  │ FULL   │ 20      │ 50              │
│ 6    │ delete  │ FULL   │ 1000    │ 1               │
│ 7    │ wal     │ NORMAL │ 1       │ 1000            │
│ 8    │ wal     │ NORMAL │ 20      │ 50              │
│ 9    │ wal     │ NORMAL │ 1000    │ 1               │
│ 10   │ wal     │ FULL   │ 1       │ 1000            │
│ 11   │ wal     │ FULL   │ 20      │ 50              │
│ 12   │ wal     │ FULL   │ 1000    │ 1               │
└──────┴─────────┴────────┴─────────┴─────────────────┘

Whether the syscalls were traced (strace needs Linux; elsewhere the
workloads run without it and nothing is counted):
┌────────────────────┐
│      syscalls      │
├────────────────────┤
│ traced with strace │
└────────────────────┘

── A. Totals while the rows were written ───────────────────────────────

A1. All commits of each cell (the close is not included). Rollback mode
pays 3 or 4 syncs per commit, so its syncs grow with the number of
commits; WAL at NORMAL syncs only around the automatic checkpoint.
┌─────────┬────────┬──────────┬─────────┬──────────┬───────┬───────────────────────────────┐
│ journal │  sync  │ batching │ pwrites │  bytes   │ syncs │         syncs_by_file         │
├─────────┼────────┼──────────┼─────────┼──────────┼───────┼───────────────────────────────┤
│ delete  │ NORMAL │ 1 x 1000 │ 38      │ 131612   │ 3     │ db:1 dir:1 journal:1          │
│ delete  │ NORMAL │ 20 x 50  │ 304     │ 608968   │ 60    │ db:20 dir:20 journal:20       │
│ delete  │ NORMAL │ 1000 x 1 │ 10028   │ 17038688 │ 3000  │ db:1000 dir:1000 journal:1000 │
│ delete  │ FULL   │ 1 x 1000 │ 38      │ 131612   │ 4     │ db:1 dir:1 journal:2          │
│ delete  │ FULL   │ 20 x 50  │ 304     │ 608968   │ 80    │ db:20 dir:20 journal:40       │
│ delete  │ FULL   │ 1000 x 1 │ 10028   │ 17038688 │ 4000  │ db:1000 dir:1000 journal:2000 │
│ wal     │ NORMAL │ 1 x 1000 │ 60      │ 123600   │ 0     │ -                             │
│ wal     │ NORMAL │ 20 x 50  │ 174     │ 358440   │ 0     │ -                             │
│ wal     │ NORMAL │ 1000 x 1 │ 2139    │ 4461320  │ 3     │ db:1 wal:2                    │
│ wal     │ FULL   │ 1 x 1000 │ 60      │ 123600   │ 1     │ wal:1                         │
│ wal     │ FULL   │ 20 x 50  │ 174     │ 358440   │ 20    │ wal:20                        │
│ wal     │ FULL   │ 1000 x 1 │ 2139    │ 4461320  │ 1003  │ db:1 wal:1002                 │
└─────────┴────────┴──────────┴─────────┴──────────┴───────┴───────────────────────────────┘

A2. Files opened, unlinked and truncated: rollback mode creates and deletes
the journal at every commit and opens the directory to sync it; WAL mode
opens nothing while the rows are written and truncates the database once,
at the automatic checkpoint.
┌─────────┬────────┬──────────┬───────────────────────┬──────────────┬────────────┐
│ journal │  sync  │ batching │         opens         │   unlinks    │ ftruncates │
├─────────┼────────┼──────────┼───────────────────────┼──────────────┼────────────┤
│ delete  │ NORMAL │ 1 x 1000 │ dir:1 journal:1       │ journal:1    │ -          │
│ delete  │ NORMAL │ 20 x 50  │ dir:20 journal:20     │ journal:20   │ -          │
│ delete  │ NORMAL │ 1000 x 1 │ dir:1000 journal:1000 │ journal:1000 │ -          │
│ delete  │ FULL   │ 1 x 1000 │ dir:1 journal:1       │ journal:1    │ -          │
│ delete  │ FULL   │ 20 x 50  │ dir:20 journal:20     │ journal:20   │ -          │
│ delete  │ FULL   │ 1000 x 1 │ dir:1000 journal:1000 │ journal:1000 │ -          │
│ wal     │ NORMAL │ 1 x 1000 │ -                     │ -            │ -          │
│ wal     │ NORMAL │ 20 x 50  │ -                     │ -            │ -          │
│ wal     │ NORMAL │ 1000 x 1 │ -                     │ -            │ db:1       │
│ wal     │ FULL   │ 1 x 1000 │ -                     │ -            │ -          │
│ wal     │ FULL   │ 20 x 50  │ -                     │ -            │ -          │
│ wal     │ FULL   │ 1000 x 1 │ -                     │ -            │ db:1       │
└─────────┴────────┴──────────┴───────────────────────┴──────────────┴────────────┘

A3. The same 1000 rows in one commit and in 1000 commits:
┌─────────┬────────┬──────────────┬──────────────┬─────────────┬──────────────┬──────────────┬─────────────┐
│ journal │  sync  │ syncs_1x1000 │ syncs_1000x1 │ syncs_ratio │ bytes_1x1000 │ bytes_1000x1 │ bytes_ratio │
├─────────┼────────┼──────────────┼──────────────┼─────────────┼──────────────┼──────────────┼─────────────┤
│ delete  │ NORMAL │ 3            │ 3000         │ 1000.0      │ 131612       │ 17038688     │ 129.5       │
│ delete  │ FULL   │ 4            │ 4000         │ 1000.0      │ 131612       │ 17038688     │ 129.5       │
│ wal     │ NORMAL │ 0            │ 3            │             │ 123600       │ 4461320      │ 36.1        │
│ wal     │ FULL   │ 1            │ 1003         │ 1003.0      │ 123600       │ 4461320      │ 36.1        │
└─────────┴────────┴──────────────┴──────────────┴─────────────┴──────────────┴──────────────┴─────────────┘

── B. Syncs per commit ─────────────────────────────────────────────────

B1. How many commits had each sync pattern. db_writes = the commit also
wrote database pages: always in rollback mode, and in WAL mode only in the
commit that ran an automatic checkpoint.
┌─────────┬────────┬──────────┬─────────┬───────┬──────────────────────┬───────────┐
│ journal │  sync  │ batching │ commits │ syncs │       pattern        │ db_writes │
├─────────┼────────┼──────────┼─────────┼───────┼──────────────────────┼───────────┤
│ delete  │ NORMAL │ 1 x 1000 │ 1       │ 3     │ db:1 dir:1 journal:1 │ true      │
│ delete  │ NORMAL │ 20 x 50  │ 20      │ 3     │ db:1 dir:1 journal:1 │ true      │
│ delete  │ NORMAL │ 1000 x 1 │ 1000    │ 3     │ db:1 dir:1 journal:1 │ true      │
│ delete  │ FULL   │ 1 x 1000 │ 1       │ 4     │ db:1 dir:1 journal:2 │ true      │
│ delete  │ FULL   │ 20 x 50  │ 20      │ 4     │ db:1 dir:1 journal:2 │ true      │
│ delete  │ FULL   │ 1000 x 1 │ 1000    │ 4     │ db:1 dir:1 journal:2 │ true      │
│ wal     │ NORMAL │ 1 x 1000 │ 1       │ 0     │ -                    │ false     │
│ wal     │ NORMAL │ 20 x 50  │ 20      │ 0     │ -                    │ false     │
│ wal     │ NORMAL │ 1000 x 1 │ 998     │ 0     │ -                    │ false     │
│ wal     │ NORMAL │ 1000 x 1 │ 1       │ 2     │ db:1 wal:1           │ true      │
│ wal     │ NORMAL │ 1000 x 1 │ 1       │ 1     │ wal:1                │ false     │
│ wal     │ FULL   │ 1 x 1000 │ 1       │ 1     │ wal:1                │ false     │
│ wal     │ FULL   │ 20 x 50  │ 20      │ 1     │ wal:1                │ false     │
│ wal     │ FULL   │ 1000 x 1 │ 998     │ 1     │ wal:1                │ false     │
│ wal     │ FULL   │ 1000 x 1 │ 1       │ 3     │ db:1 wal:2           │ true      │
│ wal     │ FULL   │ 1000 x 1 │ 1       │ 2     │ wal:2                │ false     │
└─────────┴────────┴──────────┴─────────┴───────┴──────────────────────┴───────────┘

B2. In WAL mode, the commits that do not have the plain pattern: the
commit that crosses wal_autocheckpoint pages runs a checkpoint (syncs the
-wal and the database), and the next one restarts the WAL with a new
header (one more sync of the -wal).
┌─────────┬────────┬──────────┬───────────┬─────────┬────────┬───────┬────────────┬───────────┐
│ journal │  sync  │ batching │ commit_no │ pwrites │ bytes  │ syncs │  pattern   │ db_writes │
├─────────┼────────┼──────────┼───────────┼─────────┼────────┼───────┼────────────┼───────────┤
│ wal     │ NORMAL │ 1000 x 1 │ 945       │ 30      │ 118808 │ 2     │ db:1 wal:1 │ true      │
│ wal     │ NORMAL │ 1000 x 1 │ 946       │ 3       │ 4152   │ 1     │ wal:1      │ false     │
│ wal     │ FULL   │ 1000 x 1 │ 945       │ 30      │ 118808 │ 3     │ db:1 wal:2 │ true      │
│ wal     │ FULL   │ 1000 x 1 │ 946       │ 3       │ 4152   │ 2     │ wal:2      │ false     │
└─────────┴────────┴──────────┴───────────┴─────────┴────────┴───────┴────────────┴───────────┘

── C. At close ─────────────────────────────────────────────────────────

C1. Closing the connection after the last commit. Rollback mode has
nothing left to do; in WAL mode the last connection checkpoints, syncs,
truncates the database and unlinks the -wal and -shm.
┌─────────┬────────┬──────────┬─────────┬────────┬───────┬───────────────┬─────────────┬────────────┐
│ journal │  sync  │ batching │ pwrites │ bytes  │ syncs │ syncs_by_file │   unlinks   │ ftruncates │
├─────────┼────────┼──────────┼─────────┼────────┼───────┼───────────────┼─────────────┼────────────┤
│ delete  │ NORMAL │ 1 x 1000 │ 0       │ 0      │ 0     │ -             │ -           │ -          │
│ delete  │ NORMAL │ 20 x 50  │ 0       │ 0      │ 0     │ -             │ -           │ -          │
│ delete  │ NORMAL │ 1000 x 1 │ 0       │ 0      │ 0     │ -             │ -           │ -          │
│ delete  │ FULL   │ 1 x 1000 │ 0       │ 0      │ 0     │ -             │ -           │ -          │
│ delete  │ FULL   │ 20 x 50  │ 0       │ 0      │ 0     │ -             │ -           │ -          │
│ delete  │ FULL   │ 1000 x 1 │ 0       │ 0      │ 0     │ -             │ -           │ -          │
│ wal     │ NORMAL │ 1 x 1000 │ 30      │ 122880 │ 2     │ db:1 wal:1    │ shm:1 wal:1 │ db:1       │
│ wal     │ NORMAL │ 20 x 50  │ 30      │ 122880 │ 2     │ db:1 wal:1    │ shm:1 wal:1 │ db:1       │
│ wal     │ NORMAL │ 1000 x 1 │ 5       │ 20480  │ 2     │ db:1 wal:1    │ shm:1 wal:1 │ db:1       │
│ wal     │ FULL   │ 1 x 1000 │ 30      │ 122880 │ 2     │ db:1 wal:1    │ shm:1 wal:1 │ db:1       │
│ wal     │ FULL   │ 20 x 50  │ 30      │ 122880 │ 2     │ db:1 wal:1    │ shm:1 wal:1 │ db:1       │
│ wal     │ FULL   │ 1000 x 1 │ 5       │ 20480  │ 2     │ db:1 wal:1    │ shm:1 wal:1 │ db:1       │
└─────────┴────────┴──────────┴─────────┴────────┴───────┴───────────────┴─────────────┴────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

B  ✓  rollback (DELETE) at NORMAL: every commit, at every batch size, makes 3 syncs [db:1 dir:1 journal:1]
B  ✓  rollback (DELETE) at FULL: every commit, at every batch size, makes 4 syncs [db:1 dir:1 journal:2]
B  ✓  WAL at NORMAL: a commit makes [-]; the commit that runs an automatic checkpoint makes [db:1 wal:1] (1 such), and the next commit, which restarts the WAL and writes a new WAL header, makes [wal:1] (1 such)
B  ✓  WAL at FULL: a commit makes [wal:1]; the commit that runs an automatic checkpoint makes [db:1 wal:2] (1 such), and the next commit, which restarts the WAL and writes a new WAL header, makes [wal:2] (1 such)
A  ✓  rollback FULL: 1000 one-row commits cost 4000 syncs, one 1000-row commit costs 4 (x1000)
A  ✓  1000 one-row commits at NORMAL: WAL 3 syncs and 4461320 bytes written, rollback 3000 syncs and 17038688 bytes

6 of 6 claims hold

Experiment verdict: IO COST BY PATTERN OK
```

#### Reading the Experiment 7.2 evidence

- **Opening tables (workload and settings).** Each of the 12 cells runs a dedicated Go child process under `strace`, pinned to one OS thread (`runtime.LockOSThread()`), inserting 1,000 rows (`id INTEGER PRIMARY KEY`, 100-byte deterministic `BLOB`) into an already-created table `t` using `page_size=4096`, `cache_size=-2000`, `wal_autocheckpoint=1000`, and `secure_delete=OFF`. The child opens a marker file (`commit-marker`) after `CREATE TABLE`, after every commit, and after `db.Close()`, splitting the syscall trace into one exact segment per commit plus a close segment.
- **A1–A3 (totals during writes, excluding close):**
  - **Batching in rollback `DELETE` mode:** Writing 1,000 rows in `1 x 1000` costs **38 `pwrite64`** (`131,612` bytes), **3 syncs** at `NORMAL` (`db:1 dir:1 journal:1`) or **4 syncs** at `FULL` (`db:1 dir:1 journal:2`), and 1 journal create/unlink. Splitting the same rows into `1000 x 1` costs **10,028 `pwrite64`** (`17,038,688` bytes—129× more bytes), **3,000 syncs** at `NORMAL` or **4,000 syncs** at `FULL` (1,000× more syncs), and 1,000 journal creates, directory opens, and journal unlinks.
  - **Deriving the rollback 1-row cost from the code:** Each 1-row commit modifies 2 pages (page 1 for the **change counter** at header bytes 24–27, plus the active table leaf page). SQLite writes a 512-byte journal header (`1` `pwrite64`), 2 page records of `4 + 4096 + 4 = 4104` bytes written as 3 `pwrite64` calls each (`6` `pwrite64`), the 12-byte journal header update (`1` `pwrite64`), and the 2 updated pages to `t.db` (`2` `pwrite64`): `10` `pwrite64` and `512 + 2×4104 + 12 + 2×4096 = 16,924` bytes per ordinary commit (with occasional **B-tree** leaf splits adding extra pages, totaling `10,028` writes and `17,038,688` bytes across 1,000 commits).
  - **WAL mode versus rollback mode:** At `1000 x 1` under `NORMAL`, WAL executes **2,139 `pwrite64`** (`4,461,320` bytes—nearly 4× fewer bytes than rollback's `17,038,688`) and **3 syncs** total (`db:1 wal:2`) versus rollback's **3,000 syncs**—a 1,000× reduction in syncs—with **0** journal/directory opens and **0** unlinks during the workload. At `1000 x 1` under `FULL`, WAL executes **1,003 syncs** (`db:1 wal:1002`) versus rollback's **4,000 syncs**. Every plain 1-row WAL commit writes 1 frame: 2 `pwrite64` calls (`24 + 4096 = 4,120` bytes).
- **B1–B2 (syncs per commit):**
  - Every rollback `DELETE` commit at `NORMAL` makes `db:1 dir:1 journal:1` (`3` syncs) and at `FULL` makes `db:1 dir:1 journal:2` (`4` syncs), regardless of batch size. These are **sync syscalls**, not VFS `xSync` calls (which are `2` and `3`, because the directory sync happens inside the journal's first `xSync`, [`src/os_unix.c:3941-3953`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3941-L3953)).
  - In WAL mode at `1 x 1000` (30 frames) and `20 x 50` (87 frames), the WAL never reaches `wal_autocheckpoint=1000`, so every commit makes `0` syncs at `NORMAL` and `1` sync (`wal:1`) at `FULL`.
  - In WAL mode at `1000 x 1`, the WAL crosses 1,000 frames partway through the run:
    - **`wal NORMAL 1000 x 1`:** `998` commits make **0 syncs** (`-`); **1 commit** (commit `945`, B2) runs the automatic checkpoint (`walCheckpoint`, [`src/wal.c:2199-2395`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395)) and makes **2 syncs** `db:1 wal:1` (with database writes); and the **1 commit right after it** (commit `946`) restarts the WAL (`walRestartLog`, [`src/wal.c:3875-3915`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L3875-L3915)) and syncs its new 32-byte header ([`src/wal.c:4114-4124`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4114-L4124)), making **1 sync** `wal:1`.
    - **`wal FULL 1000 x 1`:** `998` commits make **1 sync** `wal:1`; the automatic-checkpoint commit makes **3 syncs** `db:1 wal:2` (with database writes) (commit sync + WAL checkpoint sync + DB sync); and the restart commit makes **2 syncs** `wal:2` (header sync + commit sync).
- **C1 (cost at `db.Close()`):**
  - Rollback `DELETE` mode does **0 writes and 0 syncs** at close—the journal was already deleted on the last commit.
  - WAL mode runs a final `PASSIVE` checkpoint when the last connection closes: `30` page writes (`122,880` bytes) for `1 x 1000` and `20 x 50`, and `5` page writes (`20,480` bytes) for `1000 x 1` (since most pages were already checkpointed mid-run), plus **2 syncs** (`db:1 wal:1`), `ftruncate` on `t.db`, and `unlink` of `t.db-shm` and `t.db-wal`.
- **Other rollback settings and unit checks:** Outside the 12-cell matrix, probing `20 x 50` commits on Linux (adversarial test `COST-05`) confirms `delete/EXTRA` costs **5 syncs/commit** (`db:20 dir:40 journal:40`, with the second directory sync inside `unixDelete`), while `persist/NORMAL` (`80` syncs: `db:20 dir:20 journal:40`, `0` unlinks) and `truncate/FULL` (`100` syncs: `db:20 dir:20 journal:60`, `0` unlinks) eliminate `unlink` syscalls but keep the per-commit directory sync on Unix (Section 3.3) plus a final journal sync. Unit tests in [`7.2-io-cost-by-pattern/main_test.go`](experiments/7.2-io-cost-by-pattern/main_test.go) verify that `counts.add` ignores failed syscalls and `classifyWALCommits` requires at least one checkpoint followed by a matching restart.

---

### 5.2 Shared package `internal/govfs`

`internal/govfs` registers a pure-Go forwarding VFS shim on top of `modernc.org/sqlite`'s default `"unix"` VFS with no **cgo** (Go's C foreign-function bridge) and no custom C. It intercepts all 9 filesystem-level VFS methods and all 18 per-file `sqlite3_io_methods` slots (versions 1–3), records every call in memory, and injects single-shot errors on `xRead`, `xWrite`, `xSync`, or `xTruncate`. Its unit tests live in [`internal/govfs/govfs_test.go`](../internal/govfs/govfs_test.go), [`internal/govfs/inject_test.go`](../internal/govfs/inject_test.go), [`internal/govfs/vfsname_test.go`](../internal/govfs/vfsname_test.go), and [`internal/govfs/signature_test.go`](../internal/govfs/signature_test.go) (`go test ./internal/govfs/`).

#### `internal/govfs/cfunc.go`

<!-- include: ../internal/govfs/cfunc.go -->
```go
package govfs

import "unsafe"

// funcAddress returns the static funcval address stored in the data word of an
// interface value holding a top-level Go function. This matches ccgo's
// __ccgo_fp convention used by modernc.org/sqlite/lib to represent C function
// pointers inside sqlite3_vfs and sqlite3_io_methods structs.
func funcAddress(f any) uintptr {
	return (*[2]uintptr)(unsafe.Pointer(&f))[1]
}

// funcAt reinterprets a stored C function pointer (a ccgo funcval address) as a
// Go function value of type F. Every address passed here comes from SQLite's
// own VFS or io_methods tables and is checked at startup and by
// TestSignaturesMatchTranslatedSource.
func funcAt[F any](address uintptr) F {
	return *(*F)(unsafe.Pointer(&address))
}

// at converts a non-Go C-memory address (allocated by modernc.org/libc or
// sqlite3_malloc64, outside the Go garbage collector's heap) into a typed
// pointer *T. Taking the address of the uintptr variable keeps go vet's
// unsafeptr check satisfied while relying on the invariant that no Go heap
// pointer is ever round-tripped through uintptr here.
func at[T any](address uintptr) *T {
	return *(**T)(unsafe.Pointer(&address))
}

// codeAddress reads the first word of a Go funcval (the machine code entry
// point) so runtime.FuncForPC can resolve the target function's name.
func codeAddress(funcAddress uintptr) uintptr {
	return *at[uintptr](funcAddress)
}
```

**Walk-through (`cfunc.go`) — `unsafe`, `ccgo`'s function-pointer convention, and why it is sound at this pin:**

Every `unsafe` pointer conversion in `internal/govfs` lives in this 34-line file (plus two `unsafe.Sizeof` calls in `govfs.go` and one `unsafe.Slice` in `methods.go`'s `prependVFSName`).

- **How `modernc.org/sqlite/lib` represents C function pointers.** `modernc.org/sqlite` is produced by **`ccgo`** (a C-to-Go transpiler), which translates C structs such as `sqlite3_vfs` and `sqlite3_io_methods` into Go structs whose function-pointer fields have type `uintptr`. To store a top-level Go function in a `uintptr` field, `ccgo`'s runtime helper (`__ccgo_fp`) reads the second word (the data word) of an `any` interface holding that function—which in the Go runtime points to the function's static **`funcval`** struct (a small runtime descriptor whose first word is the machine-code entry point). `funcAddress(f any)` performs that exact read, `funcAt[F](address)` reinterprets a stored `uintptr` back into a callable Go function value of type `F`, and `codeAddress(funcAddress)` dereferences the first word of the `funcval` so `runtime.FuncForPC` can resolve the function's symbol name.
- **Why `at[T](address)` is safe here and why `go vet` passes cleanly.** `at[T](address)` converts a `uintptr` into `*T` via `*(**T)(unsafe.Pointer(&address))`. Normally, Go's `unsafeptr` analyzer forbids converting an arbitrary `uintptr` to `unsafe.Pointer` because the Go garbage collector can move or collect Go heap objects if their addresses are hidden in integers. Here, `go vet`'s default `unsafeptr` check does not flag `*(**T)(unsafe.Pointer(&address))`—and the check is **not** disabled—but the actual safety guarantee comes from the **allocation rule**: every `uintptr` dereferenced by `at[T]` points into memory outside the Go garbage collector's heap:
  - SQLite's `sqlite3_file`, `sqlite3_vfs`, and buffer addresses are allocated by `modernc.org/libc`'s `mmap`-backed C allocator (`modernc.org/memory`) or `sqlite3_malloc64`.
  - `internal/govfs` allocates its own `sqlite3_vfs`, `sqlite3_io_methods`, and `zName` strings in C memory (`sqlite3_malloc64` and `libc.CString`) and never frees them while registered.
  - Top-level Go function values live in static read-only program data.
  - No Go heap pointer is ever converted to a `uintptr` and handed to SQLite; instead, `registry` in `govfs.go` maps C addresses back to Go `*VFS` and `*openFile` structs on the Go side.
- **What would break this code.** Three changes could break it: (1) a future Go compiler changing the two-word interface layout or `funcval` first-word layout; (2) passing a closure or Go-heap pointer instead of a top-level function or C-allocated struct; or (3) upgrading `modernc.org/sqlite` to a release where a translated VFS function changes a parameter type or struct field. A parameter-type mismatch is silent memory corruption, not a compile error: changing `ioSyncFunc`'s `flags` parameter from `int32` to `int64` compiles and passes runtime tests on `amd64` without crashing because the register **ABI** (application binary interface, the calling convention for passing arguments) happens to tolerate the width mismatch. Three guards protect against that failure:
  1. **Compile-time typing (`slots.go`):** `newVFSSlot[F]` and `newIOSlot[F]` bind each slot to an explicit function type `F`, so the wrapper and the `funcAt[F]` call site cannot disagree.
  2. **Startup self-checks (`selfcheck.go`):** `Register` verifies the `funcAddress`/`funcAt`/`codeAddress` round-trip (`selfCheckDouble(21) == 42`), checks the default VFS fields, and verifies via `runtime.FuncForPC` that every wrapped slot points to a function in `modernc.org/sqlite/lib` whose name ends with the slot's suffix (`xWrite` → `_unixWrite`, `xLock` → `_unixLock` or `_dotlockLock`).
  3. **Source-signature test (`signature_test.go`):** `TestSignaturesMatchTranslatedSource` parses the **AST** (abstract syntax tree) of the installed `modernc.org/sqlite/lib` Go source files for the current `GOOS`/`GOARCH`, resolves `ccgo` type aliases (`Tsqlite3_int64`, `Ti64` → `int64`), and checks every parameter and return type of all 27 slots against the shim's `F`. That test catches the `int32`→`int64` mutation immediately (verified by adversarial attack `GOVFS-08`) and must be re-run after any driver upgrade.

#### `internal/govfs/slots.go`

<!-- include: ../internal/govfs/slots.go -->
```go
package govfs

import (
	"reflect"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Function types matching ccgo's translated signatures for sqlite3_vfs
// (sqlite.h.in:1513-1550) and sqlite3_io_methods (sqlite.h.in:854-878).
// Every wrapper must match its type alias at compile time, and
// TestSignaturesMatchTranslatedSource verifies each type against the
// translated modernc.org/sqlite/lib source.
type (
	vfsOpenFunc             = func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32
	vfsDeleteFunc           = func(*libc.TLS, uintptr, uintptr, int32) int32
	vfsAccessFunc           = func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	vfsFullPathnameFunc     = func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	vfsRandomnessFunc       = func(*libc.TLS, uintptr, int32, uintptr) int32
	vfsSleepFunc            = func(*libc.TLS, uintptr, int32) int32
	vfsCurrentTimeFunc      = func(*libc.TLS, uintptr, uintptr) int32
	vfsGetLastErrorFunc     = func(*libc.TLS, uintptr, int32, uintptr) int32
	vfsCurrentTimeInt64Func = func(*libc.TLS, uintptr, uintptr) int32

	ioCloseFunc                 = func(*libc.TLS, uintptr) int32
	ioReadFunc                  = func(*libc.TLS, uintptr, uintptr, int32, int64) int32
	ioWriteFunc                 = func(*libc.TLS, uintptr, uintptr, int32, int64) int32
	ioTruncateFunc              = func(*libc.TLS, uintptr, int64) int32
	ioSyncFunc                  = func(*libc.TLS, uintptr, int32) int32
	ioFileSizeFunc              = func(*libc.TLS, uintptr, uintptr) int32
	ioLockFunc                  = func(*libc.TLS, uintptr, int32) int32
	ioUnlockFunc                = func(*libc.TLS, uintptr, int32) int32
	ioCheckReservedLockFunc     = func(*libc.TLS, uintptr, uintptr) int32
	ioFileControlFunc           = func(*libc.TLS, uintptr, int32, uintptr) int32
	ioSectorSizeFunc            = func(*libc.TLS, uintptr) int32
	ioDeviceCharacteristicsFunc = func(*libc.TLS, uintptr) int32
	ioShmMapFunc                = func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32
	ioShmLockFunc               = func(*libc.TLS, uintptr, int32, int32, int32) int32
	ioShmBarrierFunc            = func(*libc.TLS, uintptr)
	ioShmUnmapFunc              = func(*libc.TLS, uintptr, int32) int32
	ioFetchFunc                 = func(*libc.TLS, uintptr, int64, int32, uintptr) int32
	ioUnfetchFunc               = func(*libc.TLS, uintptr, int64, uintptr) int32
)

// Slot describes one intercepted sqlite3_vfs or sqlite3_io_methods method,
// the minimum struct iVersion that defines it, and its expected Go signature.
type Slot struct {
	Method     string
	MinVersion int32
	Signature  reflect.Type
}

type vfsSlot struct {
	Slot
	field   func(*sqlite3.Tsqlite3_vfs) *uintptr
	wrapper uintptr
}

type ioSlot struct {
	Slot
	field   func(*sqlite3.Tsqlite3_io_methods) *uintptr
	wrapper uintptr
}

// newVFSSlot binds a sqlite3_vfs field to a wrapper whose type must equal F at
// compile time, recording F via reflect.TypeFor for the signature test.
func newVFSSlot[F any](method string, minVersion int32, field func(*sqlite3.Tsqlite3_vfs) *uintptr, wrapper F) vfsSlot {
	return vfsSlot{Slot{method, minVersion, reflect.TypeFor[F]()}, field, funcAddress(wrapper)}
}

// newIOSlot binds a sqlite3_io_methods field to a wrapper whose type must equal
// F at compile time, recording F via reflect.TypeFor for the signature test.
func newIOSlot[F any](method string, minVersion int32, field func(*sqlite3.Tsqlite3_io_methods) *uintptr, wrapper F) ioSlot {
	return ioSlot{Slot{method, minVersion, reflect.TypeFor[F]()}, field, funcAddress(wrapper)}
}

// vfsSlots lists the 9 VFS methods wrapped by the shim. Dynamic-library
// (xDl*) and system-call override slots keep the underlying VFS's pointers,
// which ignore their pVfs parameter.
var vfsSlots = []vfsSlot{
	newVFSSlot[vfsOpenFunc]("xOpen", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxOpen }, vfsOpen),
	newVFSSlot[vfsDeleteFunc]("xDelete", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxDelete }, vfsDelete),
	newVFSSlot[vfsAccessFunc]("xAccess", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxAccess }, vfsAccess),
	newVFSSlot[vfsFullPathnameFunc]("xFullPathname", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxFullPathname }, vfsFullPathname),
	newVFSSlot[vfsRandomnessFunc]("xRandomness", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxRandomness }, vfsRandomness),
	newVFSSlot[vfsSleepFunc]("xSleep", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxSleep }, vfsSleep),
	newVFSSlot[vfsCurrentTimeFunc]("xCurrentTime", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxCurrentTime }, vfsCurrentTime),
	newVFSSlot[vfsGetLastErrorFunc]("xGetLastError", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxGetLastError }, vfsGetLastError),
	newVFSSlot[vfsCurrentTimeInt64Func]("xCurrentTimeInt64", 2, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxCurrentTimeInt64 }, vfsCurrentTimeInt64),
}

// ioSlots lists all 18 sqlite3_io_methods entries across versions 1 through 3
// (sqlite.h.in:854-878).
var ioSlots = []ioSlot{
	newIOSlot[ioCloseFunc]("xClose", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxClose }, ioClose),
	newIOSlot[ioReadFunc]("xRead", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxRead }, ioRead),
	newIOSlot[ioWriteFunc]("xWrite", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxWrite }, ioWrite),
	newIOSlot[ioTruncateFunc]("xTruncate", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxTruncate }, ioTruncate),
	newIOSlot[ioSyncFunc]("xSync", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxSync }, ioSync),
	newIOSlot[ioFileSizeFunc]("xFileSize", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxFileSize }, ioFileSize),
	newIOSlot[ioLockFunc]("xLock", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxLock }, ioLock),
	newIOSlot[ioUnlockFunc]("xUnlock", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxUnlock }, ioUnlock),
	newIOSlot[ioCheckReservedLockFunc]("xCheckReservedLock", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxCheckReservedLock }, ioCheckReservedLock),
	newIOSlot[ioFileControlFunc]("xFileControl", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxFileControl }, ioFileControl),
	newIOSlot[ioSectorSizeFunc]("xSectorSize", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxSectorSize }, ioSectorSize),
	newIOSlot[ioDeviceCharacteristicsFunc]("xDeviceCharacteristics", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxDeviceCharacteristics }, ioDeviceCharacteristics),
	newIOSlot[ioShmMapFunc]("xShmMap", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmMap }, ioShmMap),
	newIOSlot[ioShmLockFunc]("xShmLock", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmLock }, ioShmLock),
	newIOSlot[ioShmBarrierFunc]("xShmBarrier", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmBarrier }, ioShmBarrier),
	newIOSlot[ioShmUnmapFunc]("xShmUnmap", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmUnmap }, ioShmUnmap),
	newIOSlot[ioFetchFunc]("xFetch", 3, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxFetch }, ioFetch),
	newIOSlot[ioUnfetchFunc]("xUnfetch", 3, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxUnfetch }, ioUnfetch),
}

// VFSSlots returns copies of the 9 intercepted sqlite3_vfs slot descriptors.
func VFSSlots() []Slot {
	slots := make([]Slot, len(vfsSlots))
	for i, s := range vfsSlots {
		slots[i] = s.Slot
	}
	return slots
}

// IOSlots returns copies of the 18 intercepted sqlite3_io_methods slot descriptors.
func IOSlots() []Slot {
	slots := make([]Slot, len(ioSlots))
	for i, s := range ioSlots {
		slots[i] = s.Slot
	}
	return slots
}
```

**Walk-through (`slots.go`).** Defines the 27 translated function signatures (`9` for `sqlite3_vfs`, `18` for `sqlite3_io_methods` across versions 1–3) and binds each slot to its struct field accessor and wrapper function. The dynamic-library (`xDl*`) and system-call override (`xSetSystemCall`, `xGetSystemCall`, `xNextSystemCall`) pointers are left untouched in the copied VFS struct because `os_unix.c`'s implementations ignore their `pVfs` parameter.

#### `internal/govfs/govfs.go`

<!-- include: ../internal/govfs/govfs.go -->
```go
// Package govfs registers a pure-Go forwarding VFS shim on top of
// modernc.org/sqlite's default VFS (unix on Linux and macOS). It records every
// VFS and sqlite3_io_methods call in-process and supports deterministic fault
// injection on xRead, xWrite, xSync, and xTruncate without cgo or custom C.
//
// Like ext/misc/vfstrace.c (vfstrace_register, lines 1138-1199), each shim
// sqlite3_file prefixes an 8-byte header (its own pMethods pointer) ahead of
// the underlying VFS's sqlite3_file bytes in memory allocated by SQLite.
package govfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// headerSize is the byte size of the leading sqlite3_file header (one pMethods
// pointer on 64-bit platforms) placed in front of the wrapped VFS's file object
// (sqlite.h.in:747-749).
const headerSize = 8

// Options configures a shim VFS registered with Register.
type Options struct {
	IOVersion   int32
	MakeDefault bool
}

// Fault specifies a single deterministic error to inject on the Nth matching
// call to Method (one of xRead, xWrite, xSync, xTruncate), optionally limited
// to files of Kind.
type Fault struct {
	Method string
	Nth    int
	Code   int32
	Kind   FileKind
}

// FaultReport summarizes what happened to an armed Fault when Disarm is called.
type FaultReport struct {
	Armed bool
	Fault Fault
	Seen  int
	Fired bool
	Call  Call
}

// VFS is a registered Go forwarding VFS that records calls and can inject
// single-shot I/O errors.
type VFS struct {
	name      string
	address   uintptr
	ioVersion int32

	mu        sync.Mutex
	recording bool
	seq       int
	calls     []Call
	fault     *Fault
	faultSeen int
	fired     *Call
	err       error
}

type openFile struct {
	vfs         *VFS
	path        string
	name        string
	kind        FileKind
	realMethods uintptr
}

type methodTable struct {
	address uintptr
	version int32
}

// registry maps C-memory sqlite3_vfs and sqlite3_file addresses back to Go
// state so no Go pointer is ever stored inside C-allocated SQLite structs.
var registry = struct {
	sync.RWMutex
	realVFS   uintptr
	byAddress map[uintptr]*VFS
	byName    map[string]*VFS
	files     map[uintptr]*openFile
	tables    map[[2]uintptr]methodTable
}{
	byAddress: map[uintptr]*VFS{},
	byName:    map[string]*VFS{},
	files:     map[uintptr]*openFile{},
	tables:    map[[2]uintptr]methodTable{},
}

// ErrAlreadyRegistered is returned by Register when SQLite already has a VFS
// with the requested name.
var ErrAlreadyRegistered = errors.New("a VFS with this name is already registered")

// Register allocates and registers a forwarding sqlite3_vfs named name using
// sqlite3_vfs_find(NULL) as its underlying VFS (os.c:362-381) and
// sqlite3_vfs_register (os.c:408-431). An Options.IOVersion of 0 defaults to 3.
// Startup self-checks verify the default VFS layout and ccgo's function-pointer
// convention before registering.
func Register(name string, options Options) (*VFS, error) {
	if name == "" {
		return nil, errors.New("govfs: empty VFS name")
	}
	version := options.IOVersion
	if version == 0 {
		version = 3
	}
	if version < 1 || version > 3 {
		return nil, fmt.Errorf("govfs: io_methods version %d not in 1..3", version)
	}
	tls := libc.NewTLS()
	defer tls.Close()

	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.byName[name]; ok {
		return nil, fmt.Errorf("govfs: %q: %w", name, ErrAlreadyRegistered)
	}
	// Allocate zName in C memory and keep it alive for the process lifetime:
	// sqlite3_vfs_register stores the pointer without copying (os.c:408-431).
	cname, err := libc.CString(name)
	if err != nil {
		return nil, err
	}
	// Free C-allocated name and sqlite3_vfs memory on any failure path; this
	// defer runs before tls.Close() in LIFO order.
	var address uintptr
	registered := false
	defer func() {
		if registered {
			return
		}
		libc.Xfree(tls, cname)
		if address != 0 {
			sqlite3.Xsqlite3_free(tls, address)
		}
	}()
	if sqlite3.Xsqlite3_vfs_find(tls, cname) != 0 {
		return nil, fmt.Errorf("govfs: %q: %w", name, ErrAlreadyRegistered)
	}
	if registry.realVFS == 0 {
		real := sqlite3.Xsqlite3_vfs_find(tls, 0)
		if real == 0 {
			return nil, errors.New("govfs: SQLite has no default VFS")
		}
		if err := checkRealVFS(real); err != nil {
			return nil, err
		}
		if err := checkFunctionPointerConvention(); err != nil {
			return nil, err
		}
		registry.realVFS = real
	}
	real := at[sqlite3.Tsqlite3_vfs](registry.realVFS)
	address = sqlite3.Xsqlite3_malloc64(tls, uint64(unsafe.Sizeof(sqlite3.Tsqlite3_vfs{})))
	if address == 0 {
		return nil, errors.New("govfs: out of memory")
	}
	// Copy the underlying VFS struct verbatim, grow szOsFile by headerSize for
	// our leading pMethods pointer, and swap in only the 9 wrapped VFS slots.
	shim := at[sqlite3.Tsqlite3_vfs](address)
	*shim = *real
	shim.FzName = cname
	shim.FpNext = 0
	shim.FszOsFile = real.FszOsFile + headerSize
	for _, slot := range vfsSlots {
		if slot.MinVersion > real.FiVersion || *slot.field(real) == 0 {
			continue
		}
		*slot.field(shim) = slot.wrapper
	}
	v := &VFS{name: name, address: address, ioVersion: version, recording: true}
	registry.byAddress[address] = v
	registry.byName[name] = v
	if rc := sqlite3.Xsqlite3_vfs_register(tls, address, libc.Bool32(options.MakeDefault)); rc != sqlite3.SQLITE_OK {
		delete(registry.byAddress, address)
		delete(registry.byName, name)
		return nil, fmt.Errorf("govfs: sqlite3_vfs_register: %d", rc)
	}
	registered = true
	return v, nil
}

// Lookup returns the registered shim VFS for name, or nil if none exists.
func Lookup(name string) *VFS {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byName[name]
}

// Name returns the registered SQLite VFS name for URI selection (?vfs=<name>).
func (v *VFS) Name() string {
	return v.name
}

// IOVersion returns the maximum sqlite3_io_methods version this shim advertises.
func (v *VFS) IOVersion() int32 {
	return v.ioVersion
}

// SetRecording enables or disables recording of forwarded VFS calls.
func (v *VFS) SetRecording(on bool) {
	v.mu.Lock()
	v.recording = on
	v.mu.Unlock()
}

// Reset clears all recorded calls and resets the sequence counter to zero.
func (v *VFS) Reset() {
	v.mu.Lock()
	v.calls = nil
	v.seq = 0
	v.mu.Unlock()
}

// Calls returns a snapshot copy of all recorded calls since the last Reset or TakeCalls.
func (v *VFS) Calls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Call(nil), v.calls...)
}

// TakeCalls returns all recorded calls and clears the buffer, resetting the
// sequence counter to zero.
func (v *VFS) TakeCalls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := v.calls
	v.calls = nil
	v.seq = 0
	return calls
}

// Arm installs a one-shot fault that will fail the Nth matching call with
// f.Code instead of forwarding to the underlying VFS.
func (v *VFS) Arm(f Fault) error {
	if f.Nth < 1 {
		return fmt.Errorf("govfs: fault Nth must be >= 1, got %d", f.Nth)
	}
	if !faultable[f.Method] {
		return fmt.Errorf("govfs: method %q cannot be failed (faultable: xRead xWrite xSync xTruncate)", f.Method)
	}
	if f.Code == CodeOK {
		return errors.New("govfs: fault code must not be SQLITE_OK")
	}
	v.mu.Lock()
	v.fault = &f
	v.faultSeen = 0
	v.fired = nil
	v.mu.Unlock()
	return nil
}

// Disarm clears any armed fault and returns a FaultReport describing how many
// matching calls were observed and whether the fault fired.
func (v *VFS) Disarm() FaultReport {
	v.mu.Lock()
	defer v.mu.Unlock()
	report := FaultReport{Armed: v.fault != nil, Seen: v.faultSeen}
	if v.fault != nil {
		report.Fault = *v.fault
	}
	if v.fired != nil {
		report.Fired = true
		report.Call = *v.fired
	}
	v.fault = nil
	v.faultSeen = 0
	v.fired = nil
	return report
}

var faultable = map[string]bool{"xRead": true, "xWrite": true, "xSync": true, "xTruncate": true}

// Err returns the first internal self-check error encountered when wrapping a
// file's sqlite3_io_methods table during xOpen, or nil if none occurred.
func (v *VFS) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

func (v *VFS) setError(err error) {
	if v == nil {
		return
	}
	v.mu.Lock()
	if v.err == nil {
		v.err = err
	}
	v.mu.Unlock()
}

// injectFault checks whether c matches the armed fault and, if it is the Nth
// match, completes the injected Call, appends it when recording is enabled, and
// stores it in v.fired under one lock so Disarm never sees a half-filled Call.
func (v *VFS) injectFault(c Call) (int32, bool) {
	if v == nil {
		return 0, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	f := v.fault
	if f == nil || v.fired != nil || f.Method != c.Method || (f.Kind != KindNone && f.Kind != c.Kind) {
		return 0, false
	}
	v.faultSeen++
	if v.faultSeen != f.Nth {
		return 0, false
	}
	c.Result = f.Code
	c.Injected = true
	v.appendLocked(&c)
	v.fired = &c
	return f.Code, true
}

func (v *VFS) record(c Call) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.appendLocked(&c)
}

func (v *VFS) appendLocked(c *Call) {
	if !v.recording {
		return
	}
	v.seq++
	c.Seq = v.seq
	v.calls = append(v.calls, *c)
}

func vfsFor(pVfs uintptr) *VFS {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byAddress[pVfs]
}

func realVFS() *sqlite3.Tsqlite3_vfs {
	return at[sqlite3.Tsqlite3_vfs](registry.realVFS)
}

func fileFor(pFile uintptr) *openFile {
	registry.RLock()
	defer registry.RUnlock()
	return registry.files[pFile]
}

// inner advances past the 8-byte shim sqlite3_file header to the embedded real
// VFS sqlite3_file struct.
func inner(pFile uintptr) uintptr {
	return pFile + headerSize
}

func realMethods(f *openFile) *sqlite3.Tsqlite3_io_methods {
	return at[sqlite3.Tsqlite3_io_methods](f.realMethods)
}

func displayName(path string) string {
	if path == "" {
		return "<temp>"
	}
	return filepath.Base(path)
}

// methodTableFor returns a cached shim sqlite3_io_methods table allocated in
// SQLite memory for the (shim VFS, real method table) pair. Its iVersion is
// capped at min(v.ioVersion, realTable.FiVersion) and null slots stay null,
// matching vfstraceOpen's pattern (vfstrace.c:894-922).
func methodTableFor(v *VFS, real uintptr) (uintptr, error) {
	key := [2]uintptr{v.address, real}
	registry.RLock()
	table, ok := registry.tables[key]
	registry.RUnlock()
	if ok {
		return table.address, nil
	}
	if err := checkRealMethods(real); err != nil {
		return 0, err
	}
	realTable := at[sqlite3.Tsqlite3_io_methods](real)
	version := min(v.ioVersion, realTable.FiVersion)
	tls := libc.NewTLS()
	defer tls.Close()
	address := sqlite3.Xsqlite3_malloc64(tls, uint64(unsafe.Sizeof(sqlite3.Tsqlite3_io_methods{})))
	if address == 0 {
		return 0, errors.New("govfs: out of memory")
	}
	shim := at[sqlite3.Tsqlite3_io_methods](address)
	*shim = sqlite3.Tsqlite3_io_methods{FiVersion: version}
	for _, slot := range ioSlots {
		if slot.MinVersion > version || *slot.field(realTable) == 0 {
			continue
		}
		*slot.field(shim) = slot.wrapper
	}
	registry.Lock()
	defer registry.Unlock()
	if existing, ok := registry.tables[key]; ok {
		// Another goroutine populated the cache first; free our unused table.
		sqlite3.Xsqlite3_free(tls, address)
		return existing.address, nil
	}
	registry.tables[key] = methodTable{address: address, version: version}
	return address, nil
}
```

**Walk-through (`govfs.go`).** `Register` validates the name and `IOVersion` (`0` defaults to `3`; `1..3` allowed), ensures SQLite does not already have a VFS with that name (`ErrAlreadyRegistered`), runs the startup self-checks once against `sqlite3_vfs_find(NULL)`, allocates a C-memory `Tsqlite3_vfs` with `sqlite3_malloc64`, copies the default VFS into it, increases `FszOsFile` by `headerSize` (`8` bytes for our leading `pMethods` pointer), swaps in the 9 wrapped VFS slots, and calls `sqlite3_vfs_register`; a `defer` frees `cname` (`libc.Xfree`) and `address` (`sqlite3_free`) on any failure return (running before `tls.Close()` in LIFO order). `methodTableFor` caches one C-allocated `Tsqlite3_io_methods` table per `(shim VFS, real method table)` pair with `iVersion = min(v.ioVersion, realTable.FiVersion)`, freeing any losing allocation on a concurrent race and leaving null slots null—so registering with `IOVersion: 1` omits `xShm*` and prevents WAL mode. `VFS` also manages call recording (`Calls`, `TakeCalls`, `Reset`, `SetRecording`) and single-shot fault injection (`Arm`, `Disarm`) on `xRead`, `xWrite`, `xSync`, and `xTruncate`; `injectFault` matches the fault, sets `Result` and `Injected`, appends via `appendLocked` only when recording is enabled (so `seq` does not advance when recording is off), and stores `v.fired` under one `v.mu` lock so a concurrent `Disarm` never sees a half-filled `Call`.

#### `internal/govfs/methods.go`

<!-- include: ../internal/govfs/methods.go -->
```go
package govfs

import (
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// codeCantOpen is SQLITE_CANTOPEN (14), returned if a newly opened file's real
// sqlite3_io_methods table fails the startup self-check.
const codeCantOpen int32 = 14

func goString(z uintptr) string {
	if z == 0 {
		return ""
	}
	return libc.GoString(z)
}

// vfsOpen forwards xOpen to the underlying VFS on the embedded file object at
// inner(pFile), then installs a shim sqlite3_io_methods table in pFile's
// leading header if the underlying open populated pMethods (vfstrace.c:876-930).
func vfsOpen(tls *libc.TLS, pVfs uintptr, zName uintptr, pFile uintptr, flags int32, pOutFlags uintptr) int32 {
	v := vfsFor(pVfs)
	path := goString(zName)
	// Clear pMethods before calling the underlying xOpen: sqlite3OsOpen asserts
	// pFile->pMethods == 0 whenever xOpen returns non-OK (os.c:230).
	header := at[sqlite3.Tsqlite3_file](pFile)
	header.FpMethods = 0
	rc := funcAt[vfsOpenFunc](realVFS().FxOpen)(tls, registry.realVFS, zName, inner(pFile), flags, pOutFlags)
	kind := KindFromOpenFlags(flags)
	call := Call{Method: "xOpen", File: displayName(path), Kind: kind, Flags: int64(flags), Result: rc}
	innerMethods := at[sqlite3.Tsqlite3_file](inner(pFile)).FpMethods
	if innerMethods != 0 {
		table, err := methodTableFor(v, innerMethods)
		if err != nil {
			funcAt[ioCloseFunc](at[sqlite3.Tsqlite3_io_methods](innerMethods).FxClose)(tls, inner(pFile))
			v.setError(err)
			call.Result = codeCantOpen
			v.record(call)
			return codeCantOpen
		}
		registry.Lock()
		registry.files[pFile] = &openFile{vfs: v, path: path, name: displayName(path), kind: kind, realMethods: innerMethods}
		registry.Unlock()
		header.FpMethods = table
	}
	v.record(call)
	return rc
}

func vfsDelete(tls *libc.TLS, pVfs uintptr, zName uintptr, syncDir int32) int32 {
	rc := funcAt[vfsDeleteFunc](realVFS().FxDelete)(tls, registry.realVFS, zName, syncDir)
	vfsFor(pVfs).record(Call{Method: "xDelete", File: displayName(goString(zName)), Flags: int64(syncDir), Result: rc})
	return rc
}

func vfsAccess(tls *libc.TLS, pVfs uintptr, zName uintptr, flags int32, pResOut uintptr) int32 {
	rc := funcAt[vfsAccessFunc](realVFS().FxAccess)(tls, registry.realVFS, zName, flags, pResOut)
	call := Call{Method: "xAccess", File: displayName(goString(zName)), Flags: int64(flags), Result: rc}
	if rc == 0 && pResOut != 0 {
		call.Out = int64(*at[int32](pResOut))
	}
	vfsFor(pVfs).record(call)
	return rc
}

func vfsFullPathname(tls *libc.TLS, pVfs uintptr, zName uintptr, nOut int32, zOut uintptr) int32 {
	rc := funcAt[vfsFullPathnameFunc](realVFS().FxFullPathname)(tls, registry.realVFS, zName, nOut, zOut)
	vfsFor(pVfs).record(Call{Method: "xFullPathname", File: displayName(goString(zName)), Result: rc})
	return rc
}

func vfsRandomness(tls *libc.TLS, pVfs uintptr, nByte int32, zOut uintptr) int32 {
	rc := funcAt[vfsRandomnessFunc](realVFS().FxRandomness)(tls, registry.realVFS, nByte, zOut)
	vfsFor(pVfs).record(Call{Method: "xRandomness", Amount: int64(nByte), Result: rc})
	return rc
}

func vfsSleep(tls *libc.TLS, pVfs uintptr, microseconds int32) int32 {
	rc := funcAt[vfsSleepFunc](realVFS().FxSleep)(tls, registry.realVFS, microseconds)
	vfsFor(pVfs).record(Call{Method: "xSleep", Amount: int64(microseconds), Result: rc})
	return rc
}

func vfsCurrentTime(tls *libc.TLS, pVfs uintptr, pOut uintptr) int32 {
	rc := funcAt[vfsCurrentTimeFunc](realVFS().FxCurrentTime)(tls, registry.realVFS, pOut)
	vfsFor(pVfs).record(Call{Method: "xCurrentTime", Result: rc})
	return rc
}

// vfsGetLastError forwards xGetLastError and records the call, matching
// vfstraceGetLastError (vfstrace.c:1091-1101).
func vfsGetLastError(tls *libc.TLS, pVfs uintptr, nByte int32, zOut uintptr) int32 {
	rc := funcAt[vfsGetLastErrorFunc](realVFS().FxGetLastError)(tls, registry.realVFS, nByte, zOut)
	vfsFor(pVfs).record(Call{Method: "xGetLastError", Amount: int64(nByte), Result: rc})
	return rc
}

func vfsCurrentTimeInt64(tls *libc.TLS, pVfs uintptr, pOut uintptr) int32 {
	rc := funcAt[vfsCurrentTimeInt64Func](realVFS().FxCurrentTimeInt64)(tls, registry.realVFS, pOut)
	vfsFor(pVfs).record(Call{Method: "xCurrentTimeInt64", Result: rc})
	return rc
}

func (f *openFile) call(method string) Call {
	return Call{Method: method, File: f.name, Kind: f.kind}
}

func ioClose(tls *libc.TLS, pFile uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xClose")
	call.Result = funcAt[ioCloseFunc](realMethods(f).FxClose)(tls, inner(pFile))
	registry.Lock()
	delete(registry.files, pFile)
	registry.Unlock()
	f.vfs.record(call)
	return call.Result
}

func ioRead(tls *libc.TLS, pFile uintptr, buf uintptr, amount int32, offset int64) int32 {
	f := fileFor(pFile)
	call := f.call("xRead")
	call.Amount, call.Offset = int64(amount), offset
	if code, ok := f.vfs.injectFault(call); ok {
		return code
	}
	call.Result = funcAt[ioReadFunc](realMethods(f).FxRead)(tls, inner(pFile), buf, amount, offset)
	f.vfs.record(call)
	return call.Result
}

func ioWrite(tls *libc.TLS, pFile uintptr, buf uintptr, amount int32, offset int64) int32 {
	f := fileFor(pFile)
	call := f.call("xWrite")
	call.Amount, call.Offset = int64(amount), offset
	if code, ok := f.vfs.injectFault(call); ok {
		return code
	}
	call.Result = funcAt[ioWriteFunc](realMethods(f).FxWrite)(tls, inner(pFile), buf, amount, offset)
	f.vfs.record(call)
	return call.Result
}

func ioTruncate(tls *libc.TLS, pFile uintptr, size int64) int32 {
	f := fileFor(pFile)
	call := f.call("xTruncate")
	call.Amount = size
	if code, ok := f.vfs.injectFault(call); ok {
		return code
	}
	call.Result = funcAt[ioTruncateFunc](realMethods(f).FxTruncate)(tls, inner(pFile), size)
	f.vfs.record(call)
	return call.Result
}

func ioSync(tls *libc.TLS, pFile uintptr, flags int32) int32 {
	f := fileFor(pFile)
	call := f.call("xSync")
	call.Flags = int64(flags)
	if code, ok := f.vfs.injectFault(call); ok {
		return code
	}
	call.Result = funcAt[ioSyncFunc](realMethods(f).FxSync)(tls, inner(pFile), flags)
	f.vfs.record(call)
	return call.Result
}

func ioFileSize(tls *libc.TLS, pFile uintptr, pSize uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xFileSize")
	call.Result = funcAt[ioFileSizeFunc](realMethods(f).FxFileSize)(tls, inner(pFile), pSize)
	if call.Result == 0 && pSize != 0 {
		call.Out = *at[int64](pSize)
	}
	f.vfs.record(call)
	return call.Result
}

func ioLock(tls *libc.TLS, pFile uintptr, level int32) int32 {
	f := fileFor(pFile)
	call := f.call("xLock")
	call.Flags = int64(level)
	call.Result = funcAt[ioLockFunc](realMethods(f).FxLock)(tls, inner(pFile), level)
	f.vfs.record(call)
	return call.Result
}

func ioUnlock(tls *libc.TLS, pFile uintptr, level int32) int32 {
	f := fileFor(pFile)
	call := f.call("xUnlock")
	call.Flags = int64(level)
	call.Result = funcAt[ioUnlockFunc](realMethods(f).FxUnlock)(tls, inner(pFile), level)
	f.vfs.record(call)
	return call.Result
}

func ioCheckReservedLock(tls *libc.TLS, pFile uintptr, pResOut uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xCheckReservedLock")
	call.Result = funcAt[ioCheckReservedLockFunc](realMethods(f).FxCheckReservedLock)(tls, inner(pFile), pResOut)
	if call.Result == 0 && pResOut != 0 {
		call.Out = int64(*at[int32](pResOut))
	}
	f.vfs.record(call)
	return call.Result
}

func ioFileControl(tls *libc.TLS, pFile uintptr, op int32, pArg uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xFileControl")
	call.Flags = int64(op)
	call.Result = funcAt[ioFileControlFunc](realMethods(f).FxFileControl)(tls, inner(pFile), op, pArg)
	if op == sqlite3.SQLITE_FCNTL_VFSNAME && call.Result == CodeOK && pArg != 0 {
		call.Result = prependVFSName(tls, f.vfs.name, pArg)
	}
	f.vfs.record(call)
	return call.Result
}

func ioSectorSize(tls *libc.TLS, pFile uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xSectorSize")
	call.Result = funcAt[ioSectorSizeFunc](realMethods(f).FxSectorSize)(tls, inner(pFile))
	f.vfs.record(call)
	return call.Result
}

func ioDeviceCharacteristics(tls *libc.TLS, pFile uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xDeviceCharacteristics")
	call.Result = funcAt[ioDeviceCharacteristicsFunc](realMethods(f).FxDeviceCharacteristics)(tls, inner(pFile))
	f.vfs.record(call)
	return call.Result
}

func ioShmMap(tls *libc.TLS, pFile uintptr, region int32, size int32, extend int32, pp uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xShmMap")
	call.Offset, call.Amount, call.Flags = int64(region), int64(size), int64(extend)
	call.Result = funcAt[ioShmMapFunc](realMethods(f).FxShmMap)(tls, inner(pFile), region, size, extend, pp)
	f.vfs.record(call)
	return call.Result
}

func ioShmLock(tls *libc.TLS, pFile uintptr, offset int32, n int32, flags int32) int32 {
	f := fileFor(pFile)
	call := f.call("xShmLock")
	call.Offset, call.Amount, call.Flags = int64(offset), int64(n), int64(flags)
	call.Result = funcAt[ioShmLockFunc](realMethods(f).FxShmLock)(tls, inner(pFile), offset, n, flags)
	f.vfs.record(call)
	return call.Result
}

func ioShmBarrier(tls *libc.TLS, pFile uintptr) {
	f := fileFor(pFile)
	funcAt[ioShmBarrierFunc](realMethods(f).FxShmBarrier)(tls, inner(pFile))
	f.vfs.record(f.call("xShmBarrier"))
}

func ioShmUnmap(tls *libc.TLS, pFile uintptr, deleteFlag int32) int32 {
	f := fileFor(pFile)
	call := f.call("xShmUnmap")
	call.Flags = int64(deleteFlag)
	call.Result = funcAt[ioShmUnmapFunc](realMethods(f).FxShmUnmap)(tls, inner(pFile), deleteFlag)
	f.vfs.record(call)
	return call.Result
}

func ioFetch(tls *libc.TLS, pFile uintptr, offset int64, amount int32, pp uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xFetch")
	call.Offset, call.Amount = offset, int64(amount)
	call.Result = funcAt[ioFetchFunc](realMethods(f).FxFetch)(tls, inner(pFile), offset, amount, pp)
	f.vfs.record(call)
	return call.Result
}

func ioUnfetch(tls *libc.TLS, pFile uintptr, offset int64, p uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xUnfetch")
	call.Offset = offset
	call.Result = funcAt[ioUnfetchFunc](realMethods(f).FxUnfetch)(tls, inner(pFile), offset, p)
	f.vfs.record(call)
	return call.Result
}

// prependVFSName implements the shim contract for SQLITE_FCNTL_VFSNAME
// (sqlite.h.in:1008-1018, vfstrace.c:711-716): prepend "<shimName>/" to the
// lower VFS's sqlite3_malloc'd name string, freeing the old buffer.
func prependVFSName(tls *libc.TLS, shimName string, pArg uintptr) int32 {
	slot := at[uintptr](pArg)
	stack := shimName
	if *slot != 0 {
		stack += "/" + libc.GoString(*slot)
	}
	buffer := sqlite3.Xsqlite3_malloc64(tls, uint64(len(stack)+1))
	if buffer == 0 {
		return sqlite3.SQLITE_NOMEM
	}
	copy(unsafe.Slice(at[byte](buffer), len(stack)+1), stack+"\x00")
	sqlite3.Xsqlite3_free(tls, *slot)
	*slot = buffer
	return CodeOK
}
```

**Walk-through (`methods.go`).** Holds the 27 wrapper functions called by SQLite:

- `vfsOpen` clears `header.FpMethods = 0` first (honoring [`src/os.c:230`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L230)'s assertion that a failed open leaves `pMethods == 0`), forwards `xOpen` to the underlying VFS using `inner(pFile)` (`pFile + 8`), verifies and builds the shim `sqlite3_io_methods` table if the underlying open populated `pMethods`, stores the `*openFile` metadata in `registry.files[pFile]`, and writes our shim table pointer into `header.FpMethods`. If a self-check fails, it closes the real file, saves the error for `VFS.Err()`, and returns `SQLITE_CANTOPEN` (`14`).
- `vfsGetLastError` forwards `xGetLastError` and records the call (`Amount = nByte`), matching `vfstraceGetLastError` ([`ext/misc/vfstrace.c:1091-1101`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L1091-L1101)).
- `vfsAccess`, `ioFileSize`, and `ioCheckReservedLock` dereference their output pointers (`pResOut`, `pSize`) only when the underlying call returned `0` and the pointer is non-zero.
- `ioFileControl` honors the `SQLITE_FCNTL_VFSNAME` shim stack contract ([`src/sqlite.h.in:1008-1018`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L1008-L1018), [`ext/misc/vfstrace.c:711-716`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L711-L716)): when `op == SQLITE_FCNTL_VFSNAME` succeeds, `prependVFSName` allocates `"<shimName>/<lower>"` with `sqlite3_malloc64`, frees the lower VFS's string, and stores the new pointer in `*pArg` (tested in [`internal/govfs/vfsname_test.go`](../internal/govfs/vfsname_test.go)).
- `ioRead`, `ioWrite`, `ioTruncate`, and `ioSync` build their `Call` and check `f.vfs.injectFault(call)` before calling the real method. When the N-th matching call arrives, `injectFault` atomically marks `Injected: true`, appends (if recording), stores `v.fired`, and returns the configured error code without calling `os_unix.c`; all other calls forward normally and record their result. `ioClose` forwards `xClose`, removes `pFile` from `registry.files`, and records the call.

#### `internal/govfs/call.go`

<!-- include: ../internal/govfs/call.go -->
```go
package govfs

import (
	"fmt"
	"sort"
	"strings"
)

// FileKind identifies the SQLite file role passed to xOpen in the SQLITE_OPEN_*
// flags (sqlite.h.in:584-622).
type FileKind string

// FileKind constants corresponding to SQLite's file-type open flags.
const (
	KindMainDB       FileKind = "main-db"
	KindMainJournal  FileKind = "main-journal"
	KindWAL          FileKind = "wal"
	KindTempDB       FileKind = "temp-db"
	KindTempJournal  FileKind = "temp-journal"
	KindTransientDB  FileKind = "transient-db"
	KindSubjournal   FileKind = "subjournal"
	KindSuperJournal FileKind = "super-journal"
	KindOther        FileKind = "other"
	KindNone         FileKind = ""
)

// SQLITE_OPEN_* bit flags forwarded through sqlite3OsOpen (sqlite.h.in:584-622,
// masked by 0x1087f7f at os.c:229).
const (
	OpenReadOnly      = 0x00000001
	OpenReadWrite     = 0x00000002
	OpenCreate        = 0x00000004
	OpenDeleteOnClose = 0x00000008
	OpenExclusive     = 0x00000010
	OpenMainDB        = 0x00000100
	OpenTempDB        = 0x00000200
	OpenTransientDB   = 0x00000400
	OpenMainJournal   = 0x00000800
	OpenTempJournal   = 0x00001000
	OpenSubjournal    = 0x00002000
	OpenSuperJournal  = 0x00004000
	OpenWAL           = 0x00080000
)

// SQLITE_SYNC_* flags passed to xSync (sqlite.h.in:731-733). Note that
// SyncFull (0x03) shares bit 0x02 with SyncNormal (0x02).
const (
	SyncNormal   = 0x00002
	SyncFull     = 0x00003
	SyncDataOnly = 0x00010
)

// Primary SQLite result codes used for fault injection and success checks.
const (
	CodeOK    int32 = 0
	CodeIOErr int32 = 10
)

// KindFromOpenFlags decodes the file-role bit from an xOpen flags word.
func KindFromOpenFlags(flags int32) FileKind {
	switch {
	case flags&OpenMainDB != 0:
		return KindMainDB
	case flags&OpenMainJournal != 0:
		return KindMainJournal
	case flags&OpenWAL != 0:
		return KindWAL
	case flags&OpenTempDB != 0:
		return KindTempDB
	case flags&OpenTempJournal != 0:
		return KindTempJournal
	case flags&OpenTransientDB != 0:
		return KindTransientDB
	case flags&OpenSubjournal != 0:
		return KindSubjournal
	case flags&OpenSuperJournal != 0:
		return KindSuperJournal
	}
	return KindOther
}

// Call records one intercepted sqlite3_vfs or sqlite3_io_methods invocation.
type Call struct {
	Seq      int
	Method   string
	File     string
	Kind     FileKind
	Amount   int64
	Offset   int64
	Flags    int64
	Out      int64
	Result   int32
	Injected bool
}

// String formats a recorded Call into the single-line text used in evidence logs.
func (c Call) String() string {
	var args string
	switch c.Method {
	case "xOpen":
		args = fmt.Sprintf("kind=%s flags=0x%x", c.Kind, c.Flags)
	case "xDelete":
		args = fmt.Sprintf("syncDir=%d", c.Flags)
	case "xAccess":
		args = fmt.Sprintf("flags=%d", c.Flags)
	case "xRead", "xWrite":
		args = fmt.Sprintf("n=%d ofst=%d", c.Amount, c.Offset)
	case "xTruncate":
		args = fmt.Sprintf("size=%d", c.Amount)
	case "xSync":
		args = "flags=" + SyncFlagsString(c.Flags)
	case "xLock", "xUnlock":
		args = "level=" + LockLevelString(c.Flags)
	case "xFileControl":
		args = "op=" + FileControlName(c.Flags)
	case "xShmMap":
		args = fmt.Sprintf("region=%d size=%d extend=%d", c.Offset, c.Amount, c.Flags)
	case "xShmLock":
		args = fmt.Sprintf("ofst=%d n=%d flags=%s", c.Offset, c.Amount, ShmLockFlagsString(c.Flags))
	case "xShmUnmap":
		args = fmt.Sprintf("delete=%d", c.Flags)
	case "xFetch":
		args = fmt.Sprintf("ofst=%d n=%d", c.Offset, c.Amount)
	case "xUnfetch":
		args = fmt.Sprintf("ofst=%d", c.Offset)
	case "xRandomness":
		args = fmt.Sprintf("n=%d", c.Amount)
	case "xSleep":
		args = fmt.Sprintf("us=%d", c.Amount)
	}
	var b strings.Builder
	b.WriteString(c.Method)
	if c.File != "" {
		b.WriteString(" " + c.File)
	}
	if args != "" {
		b.WriteString(" " + args)
	}
	switch c.Method {
	case "xShmBarrier":
	case "xDeviceCharacteristics":
		fmt.Fprintf(&b, " -> 0x%x", c.Result)
	default:
		fmt.Fprintf(&b, " -> %d", c.Result)
	}
	switch c.Method {
	case "xAccess", "xCheckReservedLock":
		fmt.Fprintf(&b, " out=%d", c.Out)
	case "xFileSize":
		fmt.Fprintf(&b, " size=%d", c.Out)
	}
	if c.Injected {
		b.WriteString(" (injected)")
	}
	return b.String()
}

// SyncFlagsString formats the actual SQLITE_SYNC_* flags passed to xSync,
// distinguishing SyncNormal (0x2) from SyncFull (0x3) by masking with SyncFull
// as unixSync does (os_unix.c:3916).
func SyncFlagsString(flags int64) string {
	var parts []string
	switch flags & SyncFull {
	case SyncFull:
		parts = append(parts, "FULL")
	case SyncNormal:
		parts = append(parts, "NORMAL")
	}
	if flags&SyncDataOnly != 0 {
		parts = append(parts, "DATAONLY")
	}
	if rest := flags &^ (SyncFull | SyncDataOnly); rest != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", rest))
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

// VFSTraceSyncLabel reproduces ext/misc/vfstrace.c's sync label logic
// (vfstraceSync, vfstrace.c:453-458). Because SQLITE_SYNC_FULL is 0x03 and
// SQLITE_SYNC_NORMAL is 0x02, testing flags&SyncFull != 0 prints "FULL" for
// NORMAL syncs as well.
func VFSTraceSyncLabel(flags int64) string {
	var parts []string
	if flags&SyncFull != 0 {
		parts = append(parts, "FULL")
	} else if flags&SyncNormal != 0 {
		parts = append(parts, "NORMAL")
	}
	if flags&SyncDataOnly != 0 {
		parts = append(parts, "DATAONLY")
	}
	if flags&^(SyncFull|SyncDataOnly) != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", flags))
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

var lockLevels = []string{"NONE", "SHARED", "RESERVED", "PENDING", "EXCLUSIVE"}

// LockLevelString formats a SQLITE_LOCK_* level (0..4) as its symbolic name.
func LockLevelString(level int64) string {
	if level >= 0 && int(level) < len(lockLevels) {
		return lockLevels[level]
	}
	return fmt.Sprintf("%d", level)
}

// SQLITE_SHM_* lock flags passed to xShmLock.
const (
	ShmUnlock    = 1
	ShmLock      = 2
	ShmShared    = 4
	ShmExclusive = 8
)

// ShmLockFlagsString formats a SQLITE_SHM_* flag bitmask as pipe-joined names.
func ShmLockFlagsString(flags int64) string {
	var parts []string
	if flags&ShmUnlock != 0 {
		parts = append(parts, "UNLOCK")
	}
	if flags&ShmLock != 0 {
		parts = append(parts, "LOCK")
	}
	if flags&ShmShared != 0 {
		parts = append(parts, "SHARED")
	}
	if flags&ShmExclusive != 0 {
		parts = append(parts, "EXCLUSIVE")
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

// Counts tallies recorded VFS calls keyed by method name.
type Counts map[string]int

// CountCalls aggregates calls by Method into a Counts map.
func CountCalls(calls []Call) Counts {
	counts := Counts{}
	for _, c := range calls {
		counts[c.Method]++
	}
	return counts
}

// String renders Counts in sorted "method=n" order separated by spaces.
func (c Counts) String() string {
	methods := make([]string, 0, len(c))
	for m := range c {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	parts := make([]string, len(methods))
	for i, m := range methods {
		parts[i] = fmt.Sprintf("%s=%d", m, c[m])
	}
	return strings.Join(parts, " ")
}

// Filter returns a slice of calls for which keep returns true.
func Filter(calls []Call, keep func(Call) bool) []Call {
	var out []Call
	for _, c := range calls {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// Methods returns a Filter predicate matching any call whose Method is in names.
func Methods(names ...string) func(Call) bool {
	return func(c Call) bool {
		for _, n := range names {
			if c.Method == n {
				return true
			}
		}
		return false
	}
}

// SQLITE_FCNTL_* opcode numbers from src/sqlite.h.in (1243-1286).
var fileControlNames = map[int64]string{
	1: "LOCKSTATE", 2: "GET_LOCKPROXYFILE", 3: "SET_LOCKPROXYFILE", 4: "LAST_ERRNO", 5: "SIZE_HINT",
	6: "CHUNK_SIZE", 7: "FILE_POINTER", 8: "SYNC_OMITTED", 9: "WIN32_AV_RETRY", 10: "PERSIST_WAL",
	11: "OVERWRITE", 12: "VFSNAME", 13: "POWERSAFE_OVERWRITE", 14: "PRAGMA", 15: "BUSYHANDLER",
	16: "TEMPFILENAME", 18: "MMAP_SIZE", 19: "TRACE", 20: "HAS_MOVED", 21: "SYNC",
	22: "COMMIT_PHASETWO", 23: "WIN32_SET_HANDLE", 24: "WAL_BLOCK", 25: "ZIPVFS", 26: "RBU",
	27: "VFS_POINTER", 28: "JOURNAL_POINTER", 29: "WIN32_GET_HANDLE", 30: "PDB",
	31: "BEGIN_ATOMIC_WRITE", 32: "COMMIT_ATOMIC_WRITE", 33: "ROLLBACK_ATOMIC_WRITE",
	34: "LOCK_TIMEOUT", 35: "DATA_VERSION", 36: "SIZE_LIMIT", 37: "CKPT_DONE", 38: "RESERVE_BYTES",
	39: "CKPT_START", 40: "EXTERNAL_READER", 41: "CKSM_FILE", 42: "RESET_CACHE", 43: "NULL_IO",
	44: "BLOCK_ON_CONNECT", 45: "FILESTAT",
}

// FileControlName returns the symbolic SQLITE_FCNTL_* name for op, or its
// decimal number if unknown.
func FileControlName(op int64) string {
	if name, ok := fileControlNames[op]; ok {
		return name
	}
	return fmt.Sprintf("%d", op)
}
```

**Walk-through (`call.go`).** Defines `Call`, `FileKind`, `Counts`, `Filter`/`Methods`, and the exact SQLite flag constants from [`src/sqlite.h.in`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L584-L878). Crucially, it provides both `SyncFlagsString`—which decodes the true `SQLITE_SYNC_*` bitmask by testing `flags & SyncFull` (`0x3`) for equality, matching `unixSync` at [`src/os_unix.c:3916`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3916)—and `VFSTraceSyncLabel`, which reproduces [`ext/misc/vfstrace.c:453-458`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L453-L458)'s bitmask test so Experiment 7.3 can compare against CLI `-vfstrace` output while also showing the real flag passed by SQLite.

#### `internal/govfs/selfcheck.go`

<!-- include: ../internal/govfs/selfcheck.go -->
```go
package govfs

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	sqlite3 "modernc.org/sqlite/lib"
)

const translatedPackage = "modernc.org/sqlite/lib."

// Target pairs one intercepted Slot with the fully qualified Go function name
// resolved from the underlying VFS or sqlite3_io_methods table at run time.
type Target struct {
	Slot     Slot
	Function string
}

var checkedTables = struct {
	sync.Mutex
	result  map[uintptr]error
	targets map[uintptr][]Target
}{result: map[uintptr]error{}, targets: map[uintptr][]Target{}}

func functionName(address uintptr) string {
	if address == 0 {
		return ""
	}
	f := runtime.FuncForPC(codeAddress(address))
	if f == nil {
		return ""
	}
	return f.Name()
}

// checkTarget verifies that a stored C function pointer resolves to a function
// in modernc.org/sqlite/lib whose name ends with the slot's suffix (for
// example xWrite -> _unixWrite, xLock -> _unixLock or _dotlockLock).
func checkTarget(slot Slot, address uintptr) (Target, error) {
	name := functionName(address)
	target := Target{Slot: slot, Function: name}
	want := strings.TrimPrefix(slot.Method, "x")
	short, ok := strings.CutPrefix(name, translatedPackage)
	if !ok || !strings.HasSuffix(short, want) {
		return target, fmt.Errorf("slot %s points at %q, want a function of %s ending in %q", slot.Method, name, strings.TrimSuffix(translatedPackage, "."), want)
	}
	return target, nil
}

func realVFSTargets(real uintptr) ([]Target, error) {
	table := at[sqlite3.Tsqlite3_vfs](real)
	var targets []Target
	var problems []string
	for _, slot := range vfsSlots {
		if slot.MinVersion > table.FiVersion {
			continue
		}
		address := *slot.field(table)
		if address == 0 {
			continue
		}
		target, err := checkTarget(slot.Slot, address)
		targets = append(targets, target)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return targets, fmt.Errorf("govfs self-check of the default VFS failed: %s", strings.Join(problems, "; "))
	}
	return targets, nil
}

func checkRealVFS(real uintptr) error {
	table := at[sqlite3.Tsqlite3_vfs](real)
	if table.FiVersion < 1 || table.FszOsFile <= 0 || table.FmxPathname <= 0 {
		return fmt.Errorf("govfs self-check: default VFS looks wrong (iVersion=%d szOsFile=%d mxPathname=%d)", table.FiVersion, table.FszOsFile, table.FmxPathname)
	}
	_, err := realVFSTargets(real)
	return err
}

// checkRealMethods validates a newly encountered real sqlite3_io_methods table
// once and caches both the check result and resolved Targets for signature tests.
func checkRealMethods(real uintptr) error {
	checkedTables.Lock()
	defer checkedTables.Unlock()
	if err, ok := checkedTables.result[real]; ok {
		return err
	}
	table := at[sqlite3.Tsqlite3_io_methods](real)
	var targets []Target
	var problems []string
	if table.FiVersion < 1 || table.FiVersion > 3 {
		problems = append(problems, fmt.Sprintf("io_methods iVersion %d not in 1..3", table.FiVersion))
	}
	for _, slot := range ioSlots {
		if slot.MinVersion > table.FiVersion {
			continue
		}
		address := *slot.field(table)
		if address == 0 {
			continue
		}
		target, err := checkTarget(slot.Slot, address)
		targets = append(targets, target)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	var err error
	if len(problems) > 0 {
		err = fmt.Errorf("govfs self-check of io_methods failed: %s", strings.Join(problems, "; "))
	}
	checkedTables.result[real] = err
	checkedTables.targets[real] = targets
	return err
}

func selfCheckDouble(x int32) int32 {
	return 2 * x
}

// checkFunctionPointerConvention verifies at startup that funcAddress, funcAt,
// and codeAddress round-trip a top-level Go function through a uintptr pointer
// on the current Go toolchain and architecture.
func checkFunctionPointerConvention() error {
	address := funcAddress(selfCheckDouble)
	if got := funcAt[func(int32) int32](address)(21); got != 42 {
		return fmt.Errorf("govfs self-check: a Go func stored as a C function pointer returned %d, want 42", got)
	}
	if name := functionName(address); !strings.HasSuffix(name, ".selfCheckDouble") {
		return fmt.Errorf("govfs self-check: function pointer resolves to %q, want selfCheckDouble", name)
	}
	return nil
}

// ForwardTargets returns every underlying modernc.org/sqlite/lib function
// resolved so far across the default VFS and all checked io_methods tables.
func ForwardTargets() []Target {
	registry.RLock()
	real := registry.realVFS
	registry.RUnlock()
	var targets []Target
	if real != 0 {
		t, _ := realVFSTargets(real)
		targets = append(targets, t...)
	}
	checkedTables.Lock()
	defer checkedTables.Unlock()
	for _, t := range checkedTables.targets {
		targets = append(targets, t...)
	}
	return targets
}
```

**Walk-through (`selfcheck.go`).** Implements the runtime half of the safety guards: `checkFunctionPointerConvention` confirms the `funcval` round-trip, `checkRealVFS` checks the default VFS's fields and target symbols, `checkRealMethods` validates every newly encountered real `sqlite3_io_methods` table once and caches the result, and `ForwardTargets` exposes all resolved symbols for `TestSignaturesMatchTranslatedSource`.

---

### 5.3 Experiment 7.3 — Go VFS shim and fault injection (`7.3-go-vfs-shim`)

- **Claim:**
  - **Part A (in-process VFS parity):** Through `internal/govfs`, the Go driver's VFS calls for one committed `INSERT` match the C CLI's `-vfstrace` call for call—same methods, order, files, offsets, lengths, lock levels, shared-memory slots, file controls, and sync labels—in `delete/NORMAL`, `delete/FULL`, `wal/NORMAL`, and `wal/FULL`, while revealing that SQLite passes `SQLITE_SYNC_NORMAL` (`0x2`) to every `xSync` in all default configurations.
  - **Part B (deterministic I/O fault injection):** Failing the N-th `xWrite` or `xSync` of a multi-statement recipe commit with `SQLITE_IOERR` (`10`), for every N from 1 through the commit's call count, in both WAL and `DELETE` modes at `synchronous=FULL`, makes the transaction fail and leaves the database passing `integrity_check` and byte-identical to the pre-step recipe state (step 3).
- **Evidence form:** `EXACT` (every stored table must match on every run).
- **Platform:** Portable (no `strace`; Part A needs the pinned `sqlite3` CLI, which the `lab` container includes; run directly on the host it needs `go run ./cmd/course build-tools`). Measured in the Debian `lab` container (and earlier on `linux/amd64`); macOS was cross-built and cross-vetted only.

<!-- include: experiments/7.3-go-vfs-shim/main.go -->
```go
// Experiment 7.3 registers the pure-Go forwarding VFS shim (internal/govfs) on
// top of modernc.org/sqlite's default VFS. Part A compares the Go driver's
// per-commit VFS call sequence call-for-call against the C CLI's -vfstrace in
// DELETE and WAL modes at NORMAL and FULL, also recording the real SQLITE_SYNC_*
// flag values. Part B fails every Nth xWrite and xSync of a recipe step with
// SQLITE_IOERR in both journal modes and verifies SQLite rolls back cleanly to
// the exact pre-step database bytes.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.3 (it needs the pinned CLI,
// from go run ./cmd/course build-tools).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/govfs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const (
	vfsName    = "course-govfs"
	dbName     = "t.db"
	setupSQL   = "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one')"
	measureSQL = "INSERT INTO t(id, v) VALUES(2, 'two')"
	faultStep  = 4
)

type config struct {
	journal string
	sync    string
}

func (c config) String() string {
	return c.journal + "/" + c.sync
}

var configs = []config{
	{sqlitex.JournalDelete, sqlitex.SyncNormal},
	{sqlitex.JournalDelete, sqlitex.SyncFull},
	{sqlitex.JournalWAL, sqlitex.SyncNormal},
	{sqlitex.JournalWAL, sqlitex.SyncFull},
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.3" runs both.
var (
	set    *results.Set
	tables struct {
		aCalls, bSteps, bCleanCalls, bTrials *results.Table
	}
)

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	cli := sqlitex.MustLocateCLI(ctx)
	v, err := govfs.Register(vfsName, govfs.Options{})
	if err != nil {
		fatal("register", err)
	}
	set = results.MustOpen(dir)
	set.Table("vfs", "name", "io_version", "forwards_to").Row(v.Name(), v.IOVersion(), "the default VFS (self-check passed)")
	tables.aCalls = set.Table("a_calls", "journal", "sync", "seq", "method", "call", "sync_flags", "vfstrace_label", "go_key", "cli_method", "cli_key")
	tables.bSteps = set.Table("b_steps", "journal", "synchronous", "seed", "base_step", "base_bytes", "step", "summary", "marker_id")
	tables.bCleanCalls = set.Table("b_clean_calls", "journal", "seq", "method", "file", "amount", "call")
	tables.bTrials = set.Table("b_trials", "journal", "method", "n", "fired", "target", "step_error", "step_code", "error_at", "close_error", "user_version", "marker_row", "integrity", "file_state", "held")

	for _, c := range configs {
		partA(ctx, cli, v, dir, c)
	}
	for _, journal := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		partB(ctx, v, dir, journal)
	}
	if err := v.Err(); err != nil {
		set.Check("B", false, "the Go VFS reported no internal error: "+err.Error())
	}
	set.Finish("GO VFS SHIM")
}

func goOptions(c config) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = c.journal
	o.Conn.Synchronous = c.sync
	o.SingleConnection = true
	o.VFS = vfsName
	return o
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func cliPrelude(o sqlitex.Options) string {
	f, c := o.File, o.Conn
	return fmt.Sprintf("PRAGMA page_size=%d;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\nPRAGMA journal_mode=%s;\nPRAGMA application_id=%d;\nPRAGMA user_version=%d;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		f.PageSize, f.AutoVacuum, f.Encoding, f.JournalMode, f.ApplicationID, f.UserVersion, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete), c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

// goKey normalizes a recorded govfs.Call to the same comparison tuple produced
// by cliKey for CLI -vfstrace output, including vfstrace's sync flag label
// (vfstrace.c:453-458).
func goKey(c govfs.Call) string {
	switch c.Method {
	case "xRead", "xWrite":
		return fmt.Sprintf("%s %s n=%d ofst=%d", c.Method, c.File, c.Amount, c.Offset)
	case "xSync":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.VFSTraceSyncLabel(c.Flags))
	case "xLock", "xUnlock":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.LockLevelString(c.Flags))
	case "xFileControl":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.FileControlName(c.Flags))
	case "xShmLock":
		return fmt.Sprintf("%s %s ofst=%d n=%d %s", c.Method, c.File, c.Offset, c.Amount, govfs.ShmLockFlagsString(c.Flags))
	case "xShmMap":
		return fmt.Sprintf("%s %s region=%d size=%d write=%d", c.Method, c.File, c.Offset, c.Amount, c.Flags)
	case "xTruncate":
		return fmt.Sprintf("%s %s %d", c.Method, c.File, c.Amount)
	case "xOpen":
		return fmt.Sprintf("%s %s flags=0x%x", c.Method, c.File, c.Flags)
	case "xDelete", "xAccess":
		return fmt.Sprintf("%s %s %d", c.Method, c.File, c.Flags)
	case "xShmUnmap":
		return fmt.Sprintf("%s %s delete=%d", c.Method, c.File, c.Flags)
	}
	return c.Method + " " + c.File
}

func argValue(arg string) string {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[i+1:]
	}
	return arg
}

func cliKey(c trace.VFSCall) string {
	file := filepath.Base(strings.Trim(c.File, `"`))
	arg := func(i int) string {
		if i < len(c.Args) {
			return argValue(c.Args[i])
		}
		return ""
	}
	switch c.Method {
	case "xRead", "xWrite":
		return fmt.Sprintf("%s %s n=%s ofst=%s", c.Method, file, arg(1), arg(2))
	case "xSync", "xLock", "xUnlock":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xFileControl":
		op := arg(1)
		if i := strings.IndexAny(op, ",["); i >= 0 {
			op = op[:i]
		}
		return fmt.Sprintf("%s %s %s", c.Method, file, op)
	case "xShmLock":
		ofst := arg(1)
		if i := strings.IndexByte(ofst, '('); i >= 0 {
			ofst = ofst[:i]
		}
		return fmt.Sprintf("%s %s ofst=%s n=%s %s", c.Method, file, ofst, arg(2), arg(3))
	case "xShmMap":
		return fmt.Sprintf("%s %s region=%s size=%s write=%s", c.Method, file, arg(1), arg(2), arg(3))
	case "xTruncate":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xOpen":
		return fmt.Sprintf("%s %s flags=%s", c.Method, file, arg(1))
	case "xDelete", "xAccess":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xShmUnmap":
		return fmt.Sprintf("%s %s delete=%s", c.Method, file, arg(1))
	case "xRandomness", "xCurrentTime", "xCurrentTimeInt64", "xSleep":
		return c.Method + " "
	}
	return c.Method + " " + file
}

func runCLI(ctx context.Context, cli sqlitex.CLI, dir string, c config) ([]trace.VFSCall, error) {
	path := filepath.Join(dir, "cli-"+strings.ToLower(c.journal+"-"+c.sync)+".db")
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	script := cliPrelude(goOptions(c)) + setupSQL + ";\n.print MARK-BEGIN\n" + measureSQL + ";\n.print MARK-END\n"
	result, err := cli.Run(ctx, sqlitex.CLIRun{Args: []string{"-vfstrace"}, Stdin: strings.NewReader(sqlitex.OpenScript(path, script)), Dir: dir})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("cli exit %d: %s", result.ExitCode, result.Stderr)
	}
	var calls []trace.VFSCall
	inside := false
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		switch line {
		case "MARK-BEGIN":
			inside = true
			continue
		case "MARK-END":
			inside = false
			continue
		}
		if !inside {
			continue
		}
		call, ok := trace.ParseVFSLine(line)
		if ok {
			calls = append(calls, call)
		}
	}
	return trace.RelativeVFS(dir, calls), nil
}

func runGo(ctx context.Context, v *govfs.VFS, dir string, c config) ([]govfs.Call, error) {
	path := filepath.Join(dir, "go-"+strings.ToLower(c.journal+"-"+c.sync)+".db")
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	db, err := sqlitex.Open(ctx, path, goOptions(c))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, setupSQL); err != nil {
		return nil, err
	}
	v.Reset()
	if _, err := db.ExecContext(ctx, measureSQL); err != nil {
		return nil, err
	}
	calls := v.TakeCalls()
	return calls, nil
}

func renameFiles(calls []govfs.Call, from, to string) []govfs.Call {
	out := make([]govfs.Call, len(calls))
	for i, c := range calls {
		c.File = strings.Replace(c.File, from, to, 1)
		out[i] = c
	}
	return out
}

func partA(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS, dir string, c config) {
	goCalls, err := runGo(ctx, v, dir, c)
	if err != nil {
		fatal("go "+c.String(), err)
	}
	cliCalls, err := runCLI(ctx, cli, dir, c)
	if err != nil {
		fatal("cli "+c.String(), err)
	}
	name := "go-" + strings.ToLower(c.journal+"-"+c.sync) + ".db"
	goCalls = renameFiles(goCalls, name, dbName)
	cliName := "cli-" + strings.ToLower(c.journal+"-"+c.sync) + ".db"
	goKeys := make([]string, len(goCalls))
	for i, call := range goCalls {
		goKeys[i] = goKey(call)
	}
	cliKeys := make([]string, len(cliCalls))
	for i, call := range cliCalls {
		cliKeys[i] = strings.Replace(cliKey(call), cliName, dbName, 1)
	}
	for i := range max(len(goCalls), len(cliCalls)) {
		var method, text, flags, label, goK, cliMethod, cliK any
		if i < len(goCalls) {
			call := goCalls[i]
			method, text, goK = call.Method, call.String(), goKeys[i]
			if call.Method == "xSync" {
				flags = fmt.Sprintf("0x%x(%s)", call.Flags, govfs.SyncFlagsString(call.Flags))
				label = govfs.VFSTraceSyncLabel(call.Flags)
			}
		}
		if i < len(cliCalls) {
			cliMethod, cliK = cliCalls[i].Method, cliKeys[i]
		}
		tables.aCalls.Row(c.journal, c.sync, i+1, method, text, flags, label, goK, cliMethod, cliK)
	}
	same := len(goKeys) == len(cliKeys)
	for i := range min(len(goKeys), len(cliKeys)) {
		if goKeys[i] != cliKeys[i] {
			same = false
			break
		}
	}
	goCounts := govfs.CountCalls(goCalls)
	cliCounts := govfs.Counts{}
	for _, call := range cliCalls {
		cliCounts[call.Method]++
	}
	set.Check("A", same && len(goCalls) > 0 && goCounts.String() == cliCounts.String(), fmt.Sprintf("%s: the Go driver makes the same %d VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI", c, len(goCalls)))
}

type outcome struct {
	stepErr     string
	stepCode    int
	where       string
	closeErr    string
	userVersion int
	rowPresent  bool
	integrity   string
	match       recipe.Match
}

// rolledBack reports whether a faulted trial returned primary SQLITE_IOERR,
// closed cleanly, and left the database byte-identical to step faultStep-1.
func (o outcome) rolledBack() bool {
	return o.stepCode == int(govfs.CodeIOErr) && o.closeErr == "" && !o.rowPresent && o.integrity == "ok" && o.match.Step == faultStep-1 && o.match.Exact && o.userVersion == faultStep-1
}

// committed reports whether an unreached-fault control trial committed cleanly
// and left the database byte-identical to step faultStep.
func (o outcome) committed() bool {
	return o.stepErr == "" && o.closeErr == "" && o.rowPresent && o.match.Step == faultStep && o.match.Exact
}

// trialHeld enforces that faults armed within 1..total actually fired and rolled
// back, while the N=total+1 control did not fire and committed normally.
func trialHeld(n, total int, fired bool, o outcome) bool {
	if n <= total {
		return fired && o.rolledBack()
	}
	return !fired && o.committed()
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

func errorText(err error) string {
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("SQLite error code %d (%s)", code, strings.TrimSpace(err.Error()))
	}
	return err.Error()
}

// applyThroughVFS runs one recipe step inside a single transaction through the
// shim VFS with an optional armed fault, disarms and drains recorded calls, and
// closes the connection before returning so the caller can safely reopen and
// inspect the file.
func applyThroughVFS(ctx context.Context, r recipe.Recipe, path string, step recipe.Step, v *govfs.VFS, fault *govfs.Fault) (stepErr error, where string, report govfs.FaultReport, calls []govfs.Call, closeErr error) {
	o := r.SQLiteOptions()
	o.VFS = vfsName
	db, err := sqlitex.Open(ctx, path, o)
	if err != nil {
		fatal("open through the Go VFS", err)
	}
	v.Reset()
	if fault != nil {
		if err := v.Arm(*fault); err != nil {
			fatal("arm", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		stepErr, where = err, "BEGIN"
	} else {
		for i, s := range step.Statements {
			if _, err := tx.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				stepErr, where = err, fmt.Sprintf("statement %d/%d (%s)", i+1, len(step.Statements), firstWords(s.SQL))
				break
			}
		}
		if stepErr == nil {
			if err := tx.Commit(); err != nil {
				stepErr, where = err, "COMMIT"
			}
		} else {
			tx.Rollback()
		}
	}
	report = v.Disarm()
	calls = v.TakeCalls()
	closeErr = db.Close()
	return stepErr, where, report, calls, closeErr
}

func firstWords(sql string) string {
	if i := strings.IndexByte(sql, '('); i >= 0 {
		sql = sql[:i]
	}
	fields := strings.Fields(sql)
	if len(fields) > 3 {
		fields = fields[:3]
	}
	return strings.Join(fields, " ")
}

func markerEventID(step recipe.Step) int64 {
	for _, s := range step.Statements {
		if strings.HasPrefix(s.SQL, "INSERT INTO events") {
			switch id := s.Args[0].(type) {
			case int:
				return int64(id)
			case int64:
				return id
			}
		}
	}
	return -1
}

func inspect(ctx context.Context, r recipe.Recipe, path string, markerID int64) outcome {
	var o outcome
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("reopen", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&o.userVersion); err != nil {
		fatal("user_version", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE id = ?", markerID).Scan(&n); err != nil {
		fatal("marker row", err)
	}
	o.rowPresent = n > 0
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&o.integrity); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close after inspect", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	o.match = match
	return o
}

func partB(ctx context.Context, v *govfs.VFS, dir, journal string) {
	r := recipe.Default().WithJournalMode(journal).WithSteps(faultStep + 1)
	if _, err := r.StepHashes(ctx); err != nil {
		fatal("step hashes", err)
	}
	base := filepath.Join(dir, "base-"+journal+".db")
	if err := r.Build(ctx, base, faultStep-1); err != nil {
		fatal("build", err)
	}
	step, err := r.Step(faultStep)
	if err != nil {
		fatal("step", err)
	}
	markerID := markerEventID(step)
	baseInfo, err := os.Stat(base)
	if err != nil {
		fatal("stat base", err)
	}
	tables.bSteps.Row(journal, r.SQLiteOptions().Conn.Synchronous, r.Seed, faultStep-1, baseInfo.Size(), faultStep, step.Summary(), markerID)

	clean := filepath.Join(dir, "clean-"+journal+".db")
	if err := copyFile(base, clean); err != nil {
		fatal("copy", err)
	}
	stepErr, _, _, calls, closeErr := applyThroughVFS(ctx, r, clean, step, v, nil)
	if stepErr != nil || closeErr != nil {
		fatal("clean run", fmt.Errorf("%v / %v", stepErr, closeErr))
	}
	cleanOutcome := inspect(ctx, r, clean, markerID)
	for _, call := range calls {
		call.File = strings.Replace(call.File, "clean-"+journal+".db", dbName, 1)
		tables.bCleanCalls.Row(journal, call.Seq, call.Method, call.File, call.Amount, call.String())
	}
	cleanHeld := cleanOutcome.match.Step == faultStep && cleanOutcome.match.Exact && cleanOutcome.rowPresent
	trialRow(journal, "none", 0, false, nil, cleanOutcome, cleanHeld)
	set.Check("B", cleanHeld, fmt.Sprintf("%s: without a fault the step commits and the file equals recipe step %d exactly", journal, faultStep))

	for _, method := range []string{"xWrite", "xSync"} {
		total := len(govfs.Filter(calls, govfs.Methods(method)))
		allRolledBack := true
		var summary []string
		for n := 1; n <= total+1; n++ {
			trial := filepath.Join(dir, fmt.Sprintf("fault-%s-%s-%03d.db", journal, method, n))
			if err := copyFile(base, trial); err != nil {
				fatal("copy", err)
			}
			fault := govfs.Fault{Method: method, Nth: n, Code: govfs.CodeIOErr}
			stepErr, where, report, _, closeErr := applyThroughVFS(ctx, r, trial, step, v, &fault)
			o := inspect(ctx, r, trial, markerID)
			if stepErr != nil {
				o.stepErr, o.where = errorText(stepErr), where
				o.stepCode, _ = sqlitex.PrimaryErrorCode(stepErr)
			}
			if closeErr != nil {
				o.closeErr = errorText(closeErr)
			}
			var target any
			if report.Fired {
				target = strings.Replace(report.Call.String(), filepath.Base(trial), dbName, 1)
			}
			held := trialHeld(n, total, report.Fired, o)
			trialRow(journal, method, n, report.Fired, target, o, held)
			if n <= total {
				if !held {
					allRolledBack = false
					summary = append(summary, fmt.Sprintf("%s #%d", method, n))
				}
			} else {
				set.Check("B", held, fmt.Sprintf("%s: with %s #%d never reached, the step commits normally", journal, method, n))
			}
			if err := recipe.RemoveDatabase(trial); err != nil {
				fatal("remove", err)
			}
		}
		claim := fmt.Sprintf("%s: for each of the %d %s calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step %d)", journal, total, method, faultStep-1)
		if !allRolledBack {
			claim += "; exceptions: " + strings.Join(summary, ", ")
		}
		set.Check("B", allRolledBack && total > 0, claim)
	}
}

// trialRow writes one run of the fault step: method "none" is the run
// without a fault.
func trialRow(journal, method string, n int, fired bool, target any, o outcome, held bool) {
	var stepErr, code, where, closeErr any
	if o.stepErr != "" {
		stepErr, code, where = o.stepErr, o.stepCode, o.where
	}
	if o.closeErr != "" {
		closeErr = o.closeErr
	}
	row := "absent"
	if o.rowPresent {
		row = "present"
	}
	tables.bTrials.Row(journal, method, n, fired, target, stepErr, code, where, closeErr, o.userVersion, row, o.integrity, o.match.String(), held)
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/7.3-go-vfs-shim/report.sql -->
```sql
-- Walkthrough of experiment 7.3. Run it with:
--   go run ./cmd/course run 7.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   vfs             one row: the Go VFS shim that was registered
--   a_calls         part A: one row per VFS call position of the measured commit,
--                   per configuration: the call the Go driver made through the
--                   shim, and the comparison keys of the Go call and of the
--                   CLI -vfstrace line at the same position
--   b_steps         part B: one row per journal mode: the recipe step that is faulted
--   b_clean_calls   part B: one row per VFS call of the step run without a fault
--   b_trials        part B: one row per run of the step: method/n = the call that
--                   was failed (none = no fault), and what reopening found
--   checks          every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  7.3  A Go VFS shim: call counts and fault injection'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A VFS written in Go (internal/govfs) is registered on top of the default'
.print 'VFS of modernc.org/sqlite. It forwards every call, records it, and can'
.print 'fail the N-th call of a method with an error code.'

SELECT name, io_version, forwards_to FROM vfs ORDER BY name;

.print
.print '── A. One committed INSERT: the Go driver vs the C CLI ─────────────────'
.print
.print 'Setup (not measured): CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT'
.print "NULL); INSERT INTO t(id, v) VALUES(1, 'one'). Measured: INSERT INTO t(id,"
.print "v) VALUES(2, 'two'). The Go driver runs it through the shim; the pinned"
.print 'CLI runs the same settings and statements with -vfstrace. Calls are'
.print 'compared by method, file, offset, length, lock level and sync label.'
.print
.print 'A1. Both make the same calls in the same order, in every configuration:'

SELECT journal, sync,
       count(method) AS go_calls,
       count(cli_method) AS cli_calls,
       count(*) FILTER (go_key IS DISTINCT FROM cli_key) AS calls_that_differ,
       min(seq) FILTER (go_key IS DISTINCT FROM cli_key) AS first_difference
FROM a_calls
GROUP BY journal, sync
ORDER BY journal, sync DESC;

.print
.print 'A2. Rollback journal at NORMAL: the journal gets the original pages and is'
.print 'synced once before the database pages are written and synced. SQLite'
.print 'passes SQLITE_SYNC_NORMAL (0x2); vfstrace labels such a call FULL'
.print '(vfstrace.c:453-458), and the comparison uses that label.'

SELECT seq, call, sync_flags, vfstrace_label
FROM a_calls WHERE journal = 'delete' AND sync = 'NORMAL' ORDER BY seq;

.print
.print 'A3. Rollback journal at FULL: one more xSync of the journal, before its'
.print 'header is rewritten with the page count.'

SELECT seq, call, sync_flags, vfstrace_label
FROM a_calls WHERE journal = 'delete' AND sync = 'FULL' ORDER BY seq;

.print
.print 'A4. WAL at NORMAL: two writes to the -wal (frame header, page) and locks in'
.print 'shared memory; no xSync.'

SELECT seq, call, sync_flags, vfstrace_label
FROM a_calls WHERE journal = 'wal' AND sync = 'NORMAL' ORDER BY seq;

.print
.print 'A5. WAL at FULL: the same, plus one xSync of the -wal.'

SELECT seq, call, sync_flags, vfstrace_label
FROM a_calls WHERE journal = 'wal' AND sync = 'FULL' ORDER BY seq;

.print
.print 'A6. VFS calls per commit, by method (Go driver = CLI where A1 shows no'
.print 'difference):'

WITH counts AS (
  SELECT method,
         count(*) FILTER (journal = 'delete' AND sync = 'NORMAL') AS delete_normal,
         count(*) FILTER (journal = 'delete' AND sync = 'FULL') AS delete_full,
         count(*) FILTER (journal = 'wal' AND sync = 'NORMAL') AS wal_normal,
         count(*) FILTER (journal = 'wal' AND sync = 'FULL') AS wal_full
  FROM a_calls WHERE method IS NOT NULL
  GROUP BY method
)
SELECT * FROM counts
UNION ALL
SELECT 'total', sum(delete_normal), sum(delete_full), sum(wal_normal), sum(wal_full) FROM counts
ORDER BY method = 'total', method;

.print
.print '── B. Fail one xWrite or xSync of a commit ─────────────────────────────'
.print
.print 'B1. A recipe database at rest after step 3 gets step 4 through the shim,'
.print 'in one transaction, at synchronous=FULL. The marker row is an events row'
.print 'that step 4 inserts.'

SELECT journal, synchronous, seed, base_step, base_bytes, step, marker_id
FROM b_steps ORDER BY journal DESC;

SELECT journal, summary FROM b_steps ORDER BY journal DESC;

.print
.print 'B2. Step 4 without a fault: how many calls it makes, and what a reopen'
.print 'finds. Rollback mode writes more: the original pages to the journal, then'
.print 'the new pages to the database.'

SELECT c.journal,
       count(*) AS calls,
       count(*) FILTER (c.method = 'xWrite') AS xwrite,
       count(*) FILTER (c.method = 'xSync') AS xsync,
       any_value(t.user_version) AS user_version,
       any_value(t.marker_row) AS marker_row,
       any_value(t.integrity) AS integrity,
       any_value(t.file_state) AS file_state
FROM b_clean_calls c JOIN b_trials t ON t.journal = c.journal AND t.method = 'none'
GROUP BY c.journal
ORDER BY c.journal DESC;

.print
.print 'B3. Its writes, by file:'

SELECT journal, file, count(*) AS xwrite_calls, sum(amount) AS bytes
FROM b_clean_calls WHERE method = 'xWrite'
GROUP BY journal, file
ORDER BY journal DESC, file;

.print
.print 'B4. Its syncs, and where they fall among the calls of the step:'

SELECT journal, seq, call,
       (SELECT count(*) FROM b_clean_calls a WHERE a.journal = c.journal) AS calls_in_step
FROM b_clean_calls c WHERE method = 'xSync'
ORDER BY journal DESC, seq;

.print
.print 'B5. Every N-th xWrite and xSync failed with SQLITE_IOERR, one run each,'
.print 'plus one run with N past the last call. Every failed call makes the step'
.print 'fail with error code 10; reopening finds user_version 3, no marker row,'
.print 'integrity ok and the exact bytes of step 3. Rollback mode writes the'
.print 'journal while the statements run, so the error surfaces at a statement;'
.print 'in WAL mode every write happens at COMMIT.'

CREATE TEMP MACRO n_range(lo, hi) AS
  CASE WHEN lo = hi THEN lo::VARCHAR ELSE lo || '..' || hi END;

SELECT journal, method, n_range(min(n), max(n)) AS n, count(*) AS runs, fired,
       coalesce(error_at, 'committed') AS error_at, step_code AS code
FROM b_trials
WHERE method <> 'none'
GROUP BY ALL
ORDER BY journal DESC, method DESC, min(n);

.print
.print 'What reopening the database found after each of those runs (held = the'
.print 'run did what the checks below require):'

SELECT journal, method, n_range(min(n), max(n)) AS n, count(*) AS runs,
       user_version, marker_row, integrity, file_state, held
FROM b_trials
WHERE method <> 'none'
GROUP BY ALL
ORDER BY journal DESC, method DESC, min(n);

.print
.print 'B6. The error each failed step returned, and the close after it:'

SELECT journal, coalesce(step_error, '(none: the step committed)') AS step_error,
       coalesce(close_error, 'ok') AS close, count(*) AS runs
FROM b_trials
WHERE method <> 'none'
GROUP BY ALL
ORDER BY journal DESC, step_error;

.print
.print 'B7. The call each run failed (none past the end):'

SELECT journal, method, n, coalesce(target, 'never reached (the step has fewer calls)') AS failed_call
FROM b_trials
WHERE method <> 'none'
ORDER BY journal DESC, method DESC, n;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 7.3
```

Add `-i` (`go run ./cmd/course run -i 7.3`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/7.3-go-vfs-shim/`](evidence/7.3-go-vfs-shim/), and `go run ./cmd/course check evidence ch07` compares a fresh run with them byte for byte.

<!-- include: evidence/7.3-go-vfs-shim.txt -->
```text

════════════════════════════════════════════════════════════════════════
  7.3  A Go VFS shim: call counts and fault injection
════════════════════════════════════════════════════════════════════════

A VFS written in Go (internal/govfs) is registered on top of the default
VFS of modernc.org/sqlite. It forwards every call, records it, and can
fail the N-th call of a method with an error code.
┌──────────────┬────────────┬─────────────────────────────────────┐
│     name     │ io_version │             forwards_to             │
├──────────────┼────────────┼─────────────────────────────────────┤
│ course-govfs │ 3          │ the default VFS (self-check passed) │
└──────────────┴────────────┴─────────────────────────────────────┘

── A. One committed INSERT: the Go driver vs the C CLI ─────────────────

Setup (not measured): CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT
NULL); INSERT INTO t(id, v) VALUES(1, 'one'). Measured: INSERT INTO t(id,
v) VALUES(2, 'two'). The Go driver runs it through the shim; the pinned
CLI runs the same settings and statements with -vfstrace. Calls are
compared by method, file, offset, length, lock level and sync label.

A1. Both make the same calls in the same order, in every configuration:
┌─────────┬────────┬──────────┬───────────┬───────────────────┬──────────────────┐
│ journal │  sync  │ go_calls │ cli_calls │ calls_that_differ │ first_difference │
├─────────┼────────┼──────────┼───────────┼───────────────────┼──────────────────┤
│ delete  │ NORMAL │ 31       │ 31        │ 0                 │                  │
│ delete  │ FULL   │ 32       │ 32        │ 0                 │                  │
│ wal     │ NORMAL │ 12       │ 12        │ 0                 │                  │
│ wal     │ FULL   │ 13       │ 13        │ 0                 │                  │
└─────────┴────────┴──────────┴───────────┴───────────────────┴──────────────────┘

A2. Rollback journal at NORMAL: the journal gets the original pages and is
synced once before the database pages are written and synced. SQLite
passes SQLITE_SYNC_NORMAL (0x2); vfstrace labels such a call FULL
(vfstrace.c:453-458), and the comparison uses that label.
┌─────┬───────────────────────────────────────────────────────┬─────────────┬────────────────┐
│ seq │                         call                          │ sync_flags  │ vfstrace_label │
├─────┼───────────────────────────────────────────────────────┼─────────────┼────────────────┤
│ 1   │ xLock t.db level=SHARED -> 0                          │             │                │
│ 2   │ xAccess t.db-journal flags=0 -> 0 out=0               │             │                │
│ 3   │ xRead t.db n=16 ofst=24 -> 0                          │             │                │
│ 4   │ xAccess t.db-wal flags=0 -> 0 out=0                   │             │                │
│ 5   │ xFileSize t.db -> 0 size=8192                         │             │                │
│ 6   │ xLock t.db level=RESERVED -> 0                        │             │                │
│ 7   │ xFileControl t.db op=HAS_MOVED -> 0                   │             │                │
│ 8   │ xOpen t.db-journal kind=main-journal flags=0x806 -> 0 │             │                │
│ 9   │ xDeviceCharacteristics t.db -> 0x9000                 │             │                │
│ 10  │ xWrite t.db-journal n=512 ofst=0 -> 0                 │             │                │
│ 11  │ xWrite t.db-journal n=4 ofst=512 -> 0                 │             │                │
│ 12  │ xWrite t.db-journal n=4096 ofst=516 -> 0              │             │                │
│ 13  │ xWrite t.db-journal n=4 ofst=4612 -> 0                │             │                │
│ 14  │ xLock t.db level=EXCLUSIVE -> 0                       │             │                │
│ 15  │ xWrite t.db-journal n=4 ofst=4616 -> 0                │             │                │
│ 16  │ xWrite t.db-journal n=4096 ofst=4620 -> 0             │             │                │
│ 17  │ xWrite t.db-journal n=4 ofst=8716 -> 0                │             │                │
│ 18  │ xDeviceCharacteristics t.db -> 0x9000                 │             │                │
│ 19  │ xRead t.db-journal n=8 ofst=9216 -> 522               │             │                │
│ 20  │ xWrite t.db-journal n=12 ofst=0 -> 0                  │             │                │
│ 21  │ xSync t.db-journal flags=NORMAL -> 0                  │ 0x2(NORMAL) │ FULL           │
│ 22  │ xWrite t.db n=4096 ofst=0 -> 0                        │             │                │
│ 23  │ xWrite t.db n=4096 ofst=4096 -> 0                     │             │                │
│ 24  │ xFileControl t.db op=SYNC -> 12                       │             │                │
│ 25  │ xSync t.db flags=NORMAL -> 0                          │ 0x2(NORMAL) │ FULL           │
│ 26  │ xClose t.db-journal -> 0                              │             │                │
│ 27  │ xDelete t.db-journal syncDir=0 -> 0                   │             │                │
│ 28  │ xFileControl t.db op=COMMIT_PHASETWO -> 12            │             │                │
│ 29  │ xUnlock t.db level=SHARED -> 0                        │             │                │
│ 30  │ xDeviceCharacteristics t.db -> 0x9000                 │             │                │
│ 31  │ xUnlock t.db level=NONE -> 0                          │             │                │
└─────┴───────────────────────────────────────────────────────┴─────────────┴────────────────┘

A3. Rollback journal at FULL: one more xSync of the journal, before its
header is rewritten with the page count.
┌─────┬───────────────────────────────────────────────────────┬─────────────┬────────────────┐
│ seq │                         call                          │ sync_flags  │ vfstrace_label │
├─────┼───────────────────────────────────────────────────────┼─────────────┼────────────────┤
│ 1   │ xLock t.db level=SHARED -> 0                          │             │                │
│ 2   │ xAccess t.db-journal flags=0 -> 0 out=0               │             │                │
│ 3   │ xRead t.db n=16 ofst=24 -> 0                          │             │                │
│ 4   │ xAccess t.db-wal flags=0 -> 0 out=0                   │             │                │
│ 5   │ xFileSize t.db -> 0 size=8192                         │             │                │
│ 6   │ xLock t.db level=RESERVED -> 0                        │             │                │
│ 7   │ xFileControl t.db op=HAS_MOVED -> 0                   │             │                │
│ 8   │ xOpen t.db-journal kind=main-journal flags=0x806 -> 0 │             │                │
│ 9   │ xDeviceCharacteristics t.db -> 0x9000                 │             │                │
│ 10  │ xWrite t.db-journal n=512 ofst=0 -> 0                 │             │                │
│ 11  │ xWrite t.db-journal n=4 ofst=512 -> 0                 │             │                │
│ 12  │ xWrite t.db-journal n=4096 ofst=516 -> 0              │             │                │
│ 13  │ xWrite t.db-journal n=4 ofst=4612 -> 0                │             │                │
│ 14  │ xLock t.db level=EXCLUSIVE -> 0                       │             │                │
│ 15  │ xWrite t.db-journal n=4 ofst=4616 -> 0                │             │                │
│ 16  │ xWrite t.db-journal n=4096 ofst=4620 -> 0             │             │                │
│ 17  │ xWrite t.db-journal n=4 ofst=8716 -> 0                │             │                │
│ 18  │ xDeviceCharacteristics t.db -> 0x9000                 │             │                │
│ 19  │ xRead t.db-journal n=8 ofst=9216 -> 522               │             │                │
│ 20  │ xSync t.db-journal flags=NORMAL -> 0                  │ 0x2(NORMAL) │ FULL           │
│ 21  │ xWrite t.db-journal n=12 ofst=0 -> 0                  │             │                │
│ 22  │ xSync t.db-journal flags=NORMAL -> 0                  │ 0x2(NORMAL) │ FULL           │
│ 23  │ xWrite t.db n=4096 ofst=0 -> 0                        │             │                │
│ 24  │ xWrite t.db n=4096 ofst=4096 -> 0                     │             │                │
│ 25  │ xFileControl t.db op=SYNC -> 12                       │             │                │
│ 26  │ xSync t.db flags=NORMAL -> 0                          │ 0x2(NORMAL) │ FULL           │
│ 27  │ xClose t.db-journal -> 0                              │             │                │
│ 28  │ xDelete t.db-journal syncDir=0 -> 0                   │             │                │
│ 29  │ xFileControl t.db op=COMMIT_PHASETWO -> 12            │             │                │
│ 30  │ xUnlock t.db level=SHARED -> 0                        │             │                │
│ 31  │ xDeviceCharacteristics t.db -> 0x9000                 │             │                │
│ 32  │ xUnlock t.db level=NONE -> 0                          │             │                │
└─────┴───────────────────────────────────────────────────────┴─────────────┴────────────────┘

A4. WAL at NORMAL: two writes to the -wal (frame header, page) and locks in
shared memory; no xSync.
┌─────┬──────────────────────────────────────────────────────┬────────────┬────────────────┐
│ seq │                         call                         │ sync_flags │ vfstrace_label │
├─────┼──────────────────────────────────────────────────────┼────────────┼────────────────┤
│ 1   │ xShmBarrier t.db                                     │            │                │
│ 2   │ xShmLock t.db ofst=4 n=1 flags=LOCK|EXCLUSIVE -> 0   │            │                │
│ 3   │ xShmLock t.db ofst=4 n=1 flags=UNLOCK|EXCLUSIVE -> 0 │            │                │
│ 4   │ xShmLock t.db ofst=4 n=1 flags=LOCK|SHARED -> 0      │            │                │
│ 5   │ xShmBarrier t.db                                     │            │                │
│ 6   │ xShmLock t.db ofst=0 n=1 flags=LOCK|EXCLUSIVE -> 0   │            │                │
│ 7   │ xWrite t.db-wal n=24 ofst=20632 -> 0                 │            │                │
│ 8   │ xWrite t.db-wal n=4096 ofst=20656 -> 0               │            │                │
│ 9   │ xShmBarrier t.db                                     │            │                │
│ 10  │ xShmLock t.db ofst=0 n=1 flags=UNLOCK|EXCLUSIVE -> 0 │            │                │
│ 11  │ xFileControl t.db op=COMMIT_PHASETWO -> 12           │            │                │
│ 12  │ xShmLock t.db ofst=4 n=1 flags=UNLOCK|SHARED -> 0    │            │                │
└─────┴──────────────────────────────────────────────────────┴────────────┴────────────────┘

A5. WAL at FULL: the same, plus one xSync of the -wal.
┌─────┬──────────────────────────────────────────────────────┬─────────────┬────────────────┐
│ seq │                         call                         │ sync_flags  │ vfstrace_label │
├─────┼──────────────────────────────────────────────────────┼─────────────┼────────────────┤
│ 1   │ xShmBarrier t.db                                     │             │                │
│ 2   │ xShmLock t.db ofst=4 n=1 flags=LOCK|EXCLUSIVE -> 0   │             │                │
│ 3   │ xShmLock t.db ofst=4 n=1 flags=UNLOCK|EXCLUSIVE -> 0 │             │                │
│ 4   │ xShmLock t.db ofst=4 n=1 flags=LOCK|SHARED -> 0      │             │                │
│ 5   │ xShmBarrier t.db                                     │             │                │
│ 6   │ xShmLock t.db ofst=0 n=1 flags=LOCK|EXCLUSIVE -> 0   │             │                │
│ 7   │ xWrite t.db-wal n=24 ofst=20632 -> 0                 │             │                │
│ 8   │ xWrite t.db-wal n=4096 ofst=20656 -> 0               │             │                │
│ 9   │ xSync t.db-wal flags=NORMAL -> 0                     │ 0x2(NORMAL) │ FULL           │
│ 10  │ xShmBarrier t.db                                     │             │                │
│ 11  │ xShmLock t.db ofst=0 n=1 flags=UNLOCK|EXCLUSIVE -> 0 │             │                │
│ 12  │ xFileControl t.db op=COMMIT_PHASETWO -> 12           │             │                │
│ 13  │ xShmLock t.db ofst=4 n=1 flags=UNLOCK|SHARED -> 0    │             │                │
└─────┴──────────────────────────────────────────────────────┴─────────────┴────────────────┘

A6. VFS calls per commit, by method (Go driver = CLI where A1 shows no
difference):
┌────────────────────────┬───────────────┬─────────────┬────────────┬──────────┐
│         method         │ delete_normal │ delete_full │ wal_normal │ wal_full │
├────────────────────────┼───────────────┼─────────────┼────────────┼──────────┤
│ xAccess                │ 2             │ 2           │ 0          │ 0        │
│ xClose                 │ 1             │ 1           │ 0          │ 0        │
│ xDelete                │ 1             │ 1           │ 0          │ 0        │
│ xDeviceCharacteristics │ 3             │ 3           │ 0          │ 0        │
│ xFileControl           │ 3             │ 3           │ 1          │ 1        │
│ xFileSize              │ 1             │ 1           │ 0          │ 0        │
│ xLock                  │ 3             │ 3           │ 0          │ 0        │
│ xOpen                  │ 1             │ 1           │ 0          │ 0        │
│ xRead                  │ 2             │ 2           │ 0          │ 0        │
│ xShmBarrier            │ 0             │ 0           │ 3          │ 3        │
│ xShmLock               │ 0             │ 0           │ 6          │ 6        │
│ xSync                  │ 2             │ 3           │ 0          │ 1        │
│ xUnlock                │ 2             │ 2           │ 0          │ 0        │
│ xWrite                 │ 10            │ 10          │ 2          │ 2        │
│ total                  │ 31            │ 32          │ 12         │ 13       │
└────────────────────────┴───────────────┴─────────────┴────────────┴──────────┘

── B. Fail one xWrite or xSync of a commit ─────────────────────────────

B1. A recipe database at rest after step 3 gets step 4 through the shim,
in one transaction, at synchronous=FULL. The marker row is an events row
that step 4 inserts.
┌─────────┬─────────────┬──────┬───────────┬────────────┬──────┬───────────┐
│ journal │ synchronous │ seed │ base_step │ base_bytes │ step │ marker_id │
├─────────┼─────────────┼──────┼───────────┼────────────┼──────┼───────────┤
│ wal     │ FULL        │ 1    │ 3         │ 159744     │ 4    │ 153       │
│ delete  │ FULL        │ 1    │ 3         │ 159744     │ 4    │ 153       │
└─────────┴─────────────┴──────┴───────────┴────────────┴──────┴───────────┘
┌─────────┬───────────────────────────────────────────────────────────┐
│ journal │                          summary                          │
├─────────┼───────────────────────────────────────────────────────────┤
│ wal     │ step 4: schema=0 inserts=48 updates=6 deletes=1 indexes=0 │
│ delete  │ step 4: schema=0 inserts=48 updates=6 deletes=1 indexes=0 │
└─────────┴───────────────────────────────────────────────────────────┘

B2. Step 4 without a fault: how many calls it makes, and what a reopen
finds. Rollback mode writes more: the original pages to the journal, then
the new pages to the database.
┌─────────┬───────┬────────┬───────┬──────────────┬────────────┬───────────┬──────────────────────┐
│ journal │ calls │ xwrite │ xsync │ user_version │ marker_row │ integrity │      file_state      │
├─────────┼───────┼────────┼───────┼──────────────┼────────────┼───────────┼──────────────────────┤
│ wal     │ 81    │ 51     │ 2     │ 4            │ present    │ ok        │ step 4 (exact bytes) │
│ delete  │ 103   │ 66     │ 3     │ 4            │ present    │ ok        │ step 4 (exact bytes) │
└─────────┴───────┴────────┴───────┴──────────────┴────────────┴───────────┴──────────────────────┘

B3. Its writes, by file:
┌─────────┬──────────────┬──────────────┬────────┐
│ journal │     file     │ xwrite_calls │ bytes  │
├─────────┼──────────────┼──────────────┼────────┤
│ wal     │ t.db-wal     │ 51           │ 103032 │
│ delete  │ t.db         │ 25           │ 102400 │
│ delete  │ t.db-journal │ 41           │ 53876  │
└─────────┴──────────────┴──────────────┴────────┘

B4. Its syncs, and where they fall among the calls of the step:
┌─────────┬─────┬──────────────────────────────────────┬───────────────┐
│ journal │ seq │                 call                 │ calls_in_step │
├─────────┼─────┼──────────────────────────────────────┼───────────────┤
│ wal     │ 26  │ xSync t.db-wal flags=NORMAL -> 0     │ 81            │
│ wal     │ 77  │ xSync t.db-wal flags=NORMAL -> 0     │ 81            │
│ delete  │ 67  │ xSync t.db-journal flags=NORMAL -> 0 │ 103           │
│ delete  │ 69  │ xSync t.db-journal flags=NORMAL -> 0 │ 103           │
│ delete  │ 97  │ xSync t.db flags=NORMAL -> 0         │ 103           │
└─────────┴─────┴──────────────────────────────────────┴───────────────┘

B5. Every N-th xWrite and xSync failed with SQLITE_IOERR, one run each,
plus one run with N past the last call. Every failed call makes the step
fail with error code 10; reopening finds user_version 3, no marker row,
integrity ok and the exact bytes of step 3. Rollback mode writes the
journal while the statements run, so the error surfaces at a statement;
in WAL mode every write happens at COMMIT.
┌─────────┬────────┬────────┬──────┬───────┬─────────────────────────────────────────┬──────┐
│ journal │ method │   n    │ runs │ fired │                error_at                 │ code │
├─────────┼────────┼────────┼──────┼───────┼─────────────────────────────────────────┼──────┤
│ wal     │ xWrite │ 1..51  │ 51   │ true  │ COMMIT                                  │ 10   │
│ wal     │ xWrite │ 52     │ 1    │ false │ committed                               │      │
│ wal     │ xSync  │ 1..2   │ 2    │ true  │ COMMIT                                  │ 10   │
│ wal     │ xSync  │ 3      │ 1    │ false │ committed                               │      │
│ delete  │ xWrite │ 1..4   │ 4    │ true  │ statement 1/56 (INSERT INTO accounts)   │ 10   │
│ delete  │ xWrite │ 5..10  │ 6    │ true  │ statement 5/56 (INSERT INTO documents)  │ 10   │
│ delete  │ xWrite │ 11..13 │ 3    │ true  │ statement 9/56 (INSERT INTO events)     │ 10   │
│ delete  │ xWrite │ 14..25 │ 12   │ true  │ statement 54/56 (UPDATE documents SET)  │ 10   │
│ delete  │ xWrite │ 26..40 │ 15   │ true  │ statement 55/56 (DELETE FROM documents) │ 10   │
│ delete  │ xWrite │ 41..66 │ 26   │ true  │ COMMIT                                  │ 10   │
│ delete  │ xWrite │ 67     │ 1    │ false │ committed                               │      │
│ delete  │ xSync  │ 1..3   │ 3    │ true  │ COMMIT                                  │ 10   │
│ delete  │ xSync  │ 4      │ 1    │ false │ committed                               │      │
└─────────┴────────┴────────┴──────┴───────┴─────────────────────────────────────────┴──────┘

What reopening the database found after each of those runs (held = the
run did what the checks below require):
┌─────────┬────────┬───────┬──────┬──────────────┬────────────┬───────────┬──────────────────────┬──────┐
│ journal │ method │   n   │ runs │ user_version │ marker_row │ integrity │      file_state      │ held │
├─────────┼────────┼───────┼──────┼──────────────┼────────────┼───────────┼──────────────────────┼──────┤
│ wal     │ xWrite │ 1..51 │ 51   │ 3            │ absent     │ ok        │ step 3 (exact bytes) │ true │
│ wal     │ xWrite │ 52    │ 1    │ 4            │ present    │ ok        │ step 4 (exact bytes) │ true │
│ wal     │ xSync  │ 1..2  │ 2    │ 3            │ absent     │ ok        │ step 3 (exact bytes) │ true │
│ wal     │ xSync  │ 3     │ 1    │ 4            │ present    │ ok        │ step 4 (exact bytes) │ true │
│ delete  │ xWrite │ 1..66 │ 66   │ 3            │ absent     │ ok        │ step 3 (exact bytes) │ true │
│ delete  │ xWrite │ 67    │ 1    │ 4            │ present    │ ok        │ step 4 (exact bytes) │ true │
│ delete  │ xSync  │ 1..3  │ 3    │ 3            │ absent     │ ok        │ step 3 (exact bytes) │ true │
│ delete  │ xSync  │ 4     │ 1    │ 4            │ present    │ ok        │ step 4 (exact bytes) │ true │
└─────────┴────────┴───────┴──────┴──────────────┴────────────┴───────────┴──────────────────────┴──────┘

B6. The error each failed step returned, and the close after it:
┌─────────┬────────────────────────────────────────────┬───────┬──────┐
│ journal │                 step_error                 │ close │ runs │
├─────────┼────────────────────────────────────────────┼───────┼──────┤
│ wal     │ (none: the step committed)                 │ ok    │ 2    │
│ wal     │ SQLite error code 10 (disk I/O error (10)) │ ok    │ 53   │
│ delete  │ (none: the step committed)                 │ ok    │ 2    │
│ delete  │ SQLite error code 10 (disk I/O error (10)) │ ok    │ 69   │
└─────────┴────────────────────────────────────────────┴───────┴──────┘

B7. The call each run failed (none past the end):
┌─────────┬────────┬────┬────────────────────────────────────────────────────────┐
│ journal │ method │ n  │                      failed_call                       │
├─────────┼────────┼────┼────────────────────────────────────────────────────────┤
│ wal     │ xWrite │ 1  │ xWrite t.db-wal n=32 ofst=0 -> 10 (injected)           │
│ wal     │ xWrite │ 2  │ xWrite t.db-wal n=24 ofst=32 -> 10 (injected)          │
│ wal     │ xWrite │ 3  │ xWrite t.db-wal n=4096 ofst=56 -> 10 (injected)        │
│ wal     │ xWrite │ 4  │ xWrite t.db-wal n=24 ofst=4152 -> 10 (injected)        │
│ wal     │ xWrite │ 5  │ xWrite t.db-wal n=4096 ofst=4176 -> 10 (injected)      │
│ wal     │ xWrite │ 6  │ xWrite t.db-wal n=24 ofst=8272 -> 10 (injected)        │
│ wal     │ xWrite │ 7  │ xWrite t.db-wal n=4096 ofst=8296 -> 10 (injected)      │
│ wal     │ xWrite │ 8  │ xWrite t.db-wal n=24 ofst=12392 -> 10 (injected)       │
│ wal     │ xWrite │ 9  │ xWrite t.db-wal n=4096 ofst=12416 -> 10 (injected)     │
│ wal     │ xWrite │ 10 │ xWrite t.db-wal n=24 ofst=16512 -> 10 (injected)       │
│ wal     │ xWrite │ 11 │ xWrite t.db-wal n=4096 ofst=16536 -> 10 (injected)     │
│ wal     │ xWrite │ 12 │ xWrite t.db-wal n=24 ofst=20632 -> 10 (injected)       │
│ wal     │ xWrite │ 13 │ xWrite t.db-wal n=4096 ofst=20656 -> 10 (injected)     │
│ wal     │ xWrite │ 14 │ xWrite t.db-wal n=24 ofst=24752 -> 10 (injected)       │
│ wal     │ xWrite │ 15 │ xWrite t.db-wal n=4096 ofst=24776 -> 10 (injected)     │
│ wal     │ xWrite │ 16 │ xWrite t.db-wal n=24 ofst=28872 -> 10 (injected)       │
│ wal     │ xWrite │ 17 │ xWrite t.db-wal n=4096 ofst=28896 -> 10 (injected)     │
│ wal     │ xWrite │ 18 │ xWrite t.db-wal n=24 ofst=32992 -> 10 (injected)       │
│ wal     │ xWrite │ 19 │ xWrite t.db-wal n=4096 ofst=33016 -> 10 (injected)     │
│ wal     │ xWrite │ 20 │ xWrite t.db-wal n=24 ofst=37112 -> 10 (injected)       │
│ wal     │ xWrite │ 21 │ xWrite t.db-wal n=4096 ofst=37136 -> 10 (injected)     │
│ wal     │ xWrite │ 22 │ xWrite t.db-wal n=24 ofst=41232 -> 10 (injected)       │
│ wal     │ xWrite │ 23 │ xWrite t.db-wal n=4096 ofst=41256 -> 10 (injected)     │
│ wal     │ xWrite │ 24 │ xWrite t.db-wal n=24 ofst=45352 -> 10 (injected)       │
│ wal     │ xWrite │ 25 │ xWrite t.db-wal n=4096 ofst=45376 -> 10 (injected)     │
│ wal     │ xWrite │ 26 │ xWrite t.db-wal n=24 ofst=49472 -> 10 (injected)       │
│ wal     │ xWrite │ 27 │ xWrite t.db-wal n=4096 ofst=49496 -> 10 (injected)     │
│ wal     │ xWrite │ 28 │ xWrite t.db-wal n=24 ofst=53592 -> 10 (injected)       │
│ wal     │ xWrite │ 29 │ xWrite t.db-wal n=4096 ofst=53616 -> 10 (injected)     │
│ wal     │ xWrite │ 30 │ xWrite t.db-wal n=24 ofst=57712 -> 10 (injected)       │
│ wal     │ xWrite │ 31 │ xWrite t.db-wal n=4096 ofst=57736 -> 10 (injected)     │
│ wal     │ xWrite │ 32 │ xWrite t.db-wal n=24 ofst=61832 -> 10 (injected)       │
│ wal     │ xWrite │ 33 │ xWrite t.db-wal n=4096 ofst=61856 -> 10 (injected)     │
│ wal     │ xWrite │ 34 │ xWrite t.db-wal n=24 ofst=65952 -> 10 (injected)       │
│ wal     │ xWrite │ 35 │ xWrite t.db-wal n=4096 ofst=65976 -> 10 (injected)     │
│ wal     │ xWrite │ 36 │ xWrite t.db-wal n=24 ofst=70072 -> 10 (injected)       │
│ wal     │ xWrite │ 37 │ xWrite t.db-wal n=4096 ofst=70096 -> 10 (injected)     │
│ wal     │ xWrite │ 38 │ xWrite t.db-wal n=24 ofst=74192 -> 10 (injected)       │
│ wal     │ xWrite │ 39 │ xWrite t.db-wal n=4096 ofst=74216 -> 10 (injected)     │
│ wal     │ xWrite │ 40 │ xWrite t.db-wal n=24 ofst=78312 -> 10 (injected)       │
│ wal     │ xWrite │ 41 │ xWrite t.db-wal n=4096 ofst=78336 -> 10 (injected)     │
│ wal     │ xWrite │ 42 │ xWrite t.db-wal n=24 ofst=82432 -> 10 (injected)       │
│ wal     │ xWrite │ 43 │ xWrite t.db-wal n=4096 ofst=82456 -> 10 (injected)     │
│ wal     │ xWrite │ 44 │ xWrite t.db-wal n=24 ofst=86552 -> 10 (injected)       │
│ wal     │ xWrite │ 45 │ xWrite t.db-wal n=4096 ofst=86576 -> 10 (injected)     │
│ wal     │ xWrite │ 46 │ xWrite t.db-wal n=24 ofst=90672 -> 10 (injected)       │
│ wal     │ xWrite │ 47 │ xWrite t.db-wal n=4096 ofst=90696 -> 10 (injected)     │
│ wal     │ xWrite │ 48 │ xWrite t.db-wal n=24 ofst=94792 -> 10 (injected)       │
│ wal     │ xWrite │ 49 │ xWrite t.db-wal n=4096 ofst=94816 -> 10 (injected)     │
│ wal     │ xWrite │ 50 │ xWrite t.db-wal n=24 ofst=98912 -> 10 (injected)       │
│ wal     │ xWrite │ 51 │ xWrite t.db-wal n=4096 ofst=98936 -> 10 (injected)     │
│ wal     │ xWrite │ 52 │ never reached (the step has fewer calls)               │
│ wal     │ xSync  │ 1  │ xSync t.db-wal flags=NORMAL -> 10 (injected)           │
│ wal     │ xSync  │ 2  │ xSync t.db-wal flags=NORMAL -> 10 (injected)           │
│ wal     │ xSync  │ 3  │ never reached (the step has fewer calls)               │
│ delete  │ xWrite │ 1  │ xWrite t.db-journal n=512 ofst=0 -> 10 (injected)      │
│ delete  │ xWrite │ 2  │ xWrite t.db-journal n=4 ofst=512 -> 10 (injected)      │
│ delete  │ xWrite │ 3  │ xWrite t.db-journal n=4096 ofst=516 -> 10 (injected)   │
│ delete  │ xWrite │ 4  │ xWrite t.db-journal n=4 ofst=4612 -> 10 (injected)     │
│ delete  │ xWrite │ 5  │ xWrite t.db-journal n=4 ofst=4616 -> 10 (injected)     │
│ delete  │ xWrite │ 6  │ xWrite t.db-journal n=4096 ofst=4620 -> 10 (injected)  │
│ delete  │ xWrite │ 7  │ xWrite t.db-journal n=4 ofst=8716 -> 10 (injected)     │
│ delete  │ xWrite │ 8  │ xWrite t.db-journal n=4 ofst=8720 -> 10 (injected)     │
│ delete  │ xWrite │ 9  │ xWrite t.db-journal n=4096 ofst=8724 -> 10 (injected)  │
│ delete  │ xWrite │ 10 │ xWrite t.db-journal n=4 ofst=12820 -> 10 (injected)    │
│ delete  │ xWrite │ 11 │ xWrite t.db-journal n=4 ofst=12824 -> 10 (injected)    │
│ delete  │ xWrite │ 12 │ xWrite t.db-journal n=4096 ofst=12828 -> 10 (injected) │
│ delete  │ xWrite │ 13 │ xWrite t.db-journal n=4 ofst=16924 -> 10 (injected)    │
│ delete  │ xWrite │ 14 │ xWrite t.db-journal n=4 ofst=16928 -> 10 (injected)    │
│ delete  │ xWrite │ 15 │ xWrite t.db-journal n=4096 ofst=16932 -> 10 (injected) │
│ delete  │ xWrite │ 16 │ xWrite t.db-journal n=4 ofst=21028 -> 10 (injected)    │
│ delete  │ xWrite │ 17 │ xWrite t.db-journal n=4 ofst=21032 -> 10 (injected)    │
│ delete  │ xWrite │ 18 │ xWrite t.db-journal n=4096 ofst=21036 -> 10 (injected) │
│ delete  │ xWrite │ 19 │ xWrite t.db-journal n=4 ofst=25132 -> 10 (injected)    │
│ delete  │ xWrite │ 20 │ xWrite t.db-journal n=4 ofst=25136 -> 10 (injected)    │
│ delete  │ xWrite │ 21 │ xWrite t.db-journal n=4096 ofst=25140 -> 10 (injected) │
│ delete  │ xWrite │ 22 │ xWrite t.db-journal n=4 ofst=29236 -> 10 (injected)    │
│ delete  │ xWrite │ 23 │ xWrite t.db-journal n=4 ofst=29240 -> 10 (injected)    │
│ delete  │ xWrite │ 24 │ xWrite t.db-journal n=4096 ofst=29244 -> 10 (injected) │
│ delete  │ xWrite │ 25 │ xWrite t.db-journal n=4 ofst=33340 -> 10 (injected)    │
│ delete  │ xWrite │ 26 │ xWrite t.db-journal n=4 ofst=33344 -> 10 (injected)    │
│ delete  │ xWrite │ 27 │ xWrite t.db-journal n=4096 ofst=33348 -> 10 (injected) │
│ delete  │ xWrite │ 28 │ xWrite t.db-journal n=4 ofst=37444 -> 10 (injected)    │
│ delete  │ xWrite │ 29 │ xWrite t.db-journal n=4 ofst=37448 -> 10 (injected)    │
│ delete  │ xWrite │ 30 │ xWrite t.db-journal n=4096 ofst=37452 -> 10 (injected) │
│ delete  │ xWrite │ 31 │ xWrite t.db-journal n=4 ofst=41548 -> 10 (injected)    │
│ delete  │ xWrite │ 32 │ xWrite t.db-journal n=4 ofst=41552 -> 10 (injected)    │
│ delete  │ xWrite │ 33 │ xWrite t.db-journal n=4096 ofst=41556 -> 10 (injected) │
│ delete  │ xWrite │ 34 │ xWrite t.db-journal n=4 ofst=45652 -> 10 (injected)    │
│ delete  │ xWrite │ 35 │ xWrite t.db-journal n=4 ofst=45656 -> 10 (injected)    │
│ delete  │ xWrite │ 36 │ xWrite t.db-journal n=4096 ofst=45660 -> 10 (injected) │
│ delete  │ xWrite │ 37 │ xWrite t.db-journal n=4 ofst=49756 -> 10 (injected)    │
│ delete  │ xWrite │ 38 │ xWrite t.db-journal n=4 ofst=49760 -> 10 (injected)    │
│ delete  │ xWrite │ 39 │ xWrite t.db-journal n=4096 ofst=49764 -> 10 (injected) │
│ delete  │ xWrite │ 40 │ xWrite t.db-journal n=4 ofst=53860 -> 10 (injected)    │
│ delete  │ xWrite │ 41 │ xWrite t.db-journal n=12 ofst=0 -> 10 (injected)       │
│ delete  │ xWrite │ 42 │ xWrite t.db n=4096 ofst=0 -> 10 (injected)             │
│ delete  │ xWrite │ 43 │ xWrite t.db n=4096 ofst=4096 -> 10 (injected)          │
│ delete  │ xWrite │ 44 │ xWrite t.db n=4096 ofst=8192 -> 10 (injected)          │
│ delete  │ xWrite │ 45 │ xWrite t.db n=4096 ofst=73728 -> 10 (injected)         │
│ delete  │ xWrite │ 46 │ xWrite t.db n=4096 ofst=81920 -> 10 (injected)         │
│ delete  │ xWrite │ 47 │ xWrite t.db n=4096 ofst=94208 -> 10 (injected)         │
│ delete  │ xWrite │ 48 │ xWrite t.db n=4096 ofst=110592 -> 10 (injected)        │
│ delete  │ xWrite │ 49 │ xWrite t.db n=4096 ofst=114688 -> 10 (injected)        │
│ delete  │ xWrite │ 50 │ xWrite t.db n=4096 ofst=139264 -> 10 (injected)        │
│ delete  │ xWrite │ 51 │ xWrite t.db n=4096 ofst=143360 -> 10 (injected)        │
│ delete  │ xWrite │ 52 │ xWrite t.db n=4096 ofst=147456 -> 10 (injected)        │
│ delete  │ xWrite │ 53 │ xWrite t.db n=4096 ofst=151552 -> 10 (injected)        │
│ delete  │ xWrite │ 54 │ xWrite t.db n=4096 ofst=155648 -> 10 (injected)        │
│ delete  │ xWrite │ 55 │ xWrite t.db n=4096 ofst=159744 -> 10 (injected)        │
│ delete  │ xWrite │ 56 │ xWrite t.db n=4096 ofst=163840 -> 10 (injected)        │
│ delete  │ xWrite │ 57 │ xWrite t.db n=4096 ofst=167936 -> 10 (injected)        │
│ delete  │ xWrite │ 58 │ xWrite t.db n=4096 ofst=172032 -> 10 (injected)        │
│ delete  │ xWrite │ 59 │ xWrite t.db n=4096 ofst=176128 -> 10 (injected)        │
│ delete  │ xWrite │ 60 │ xWrite t.db n=4096 ofst=180224 -> 10 (injected)        │
│ delete  │ xWrite │ 61 │ xWrite t.db n=4096 ofst=184320 -> 10 (injected)        │
│ delete  │ xWrite │ 62 │ xWrite t.db n=4096 ofst=188416 -> 10 (injected)        │
│ delete  │ xWrite │ 63 │ xWrite t.db n=4096 ofst=192512 -> 10 (injected)        │
│ delete  │ xWrite │ 64 │ xWrite t.db n=4096 ofst=196608 -> 10 (injected)        │
│ delete  │ xWrite │ 65 │ xWrite t.db n=4096 ofst=200704 -> 10 (injected)        │
│ delete  │ xWrite │ 66 │ xWrite t.db n=4096 ofst=204800 -> 10 (injected)        │
│ delete  │ xWrite │ 67 │ never reached (the step has fewer calls)               │
│ delete  │ xSync  │ 1  │ xSync t.db-journal flags=NORMAL -> 10 (injected)       │
│ delete  │ xSync  │ 2  │ xSync t.db-journal flags=NORMAL -> 10 (injected)       │
│ delete  │ xSync  │ 3  │ xSync t.db flags=NORMAL -> 10 (injected)               │
│ delete  │ xSync  │ 4  │ never reached (the step has fewer calls)               │
└─────────┴────────┴────┴────────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  delete/NORMAL: the Go driver makes the same 31 VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI
A  ✓  delete/FULL: the Go driver makes the same 32 VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI
A  ✓  wal/NORMAL: the Go driver makes the same 12 VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI
A  ✓  wal/FULL: the Go driver makes the same 13 VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI
B  ✓  wal: without a fault the step commits and the file equals recipe step 4 exactly
B  ✓  wal: with xWrite #52 never reached, the step commits normally
B  ✓  wal: for each of the 51 xWrite calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step 3)
B  ✓  wal: with xSync #3 never reached, the step commits normally
B  ✓  wal: for each of the 2 xSync calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step 3)
B  ✓  delete: without a fault the step commits and the file equals recipe step 4 exactly
B  ✓  delete: with xWrite #67 never reached, the step commits normally
B  ✓  delete: for each of the 66 xWrite calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step 3)
B  ✓  delete: with xSync #4 never reached, the step commits normally
B  ✓  delete: for each of the 3 xSync calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step 3)

14 of 14 claims hold

Experiment verdict: GO VFS SHIM OK
```

#### Reading the Experiment 7.3 evidence

- **Opening table (startup).** Both the Go driver and CLI pass the pin check, and `govfs.Register("course-govfs", govfs.Options{})` registers a version-3 forwarding VFS with its startup self-check passing.
- **Section A (Part A: Go driver through `govfs` vs C CLI `-vfstrace`).** After creating table `t` and inserting row 1, the experiment measures a single statement: `INSERT INTO t(id, v) VALUES(2, 'two')`.
  - **`delete/NORMAL` (A2): 31 VFS calls.** Both the Go driver and the C CLI execute the exact same 31 calls in the exact same order: `xLock(SHARED)`, `xAccess` `-journal` and `-wal`, `xRead` 16 B at offset 24 (the file change counter), `xFileSize` (`8192`), `xLock(RESERVED)`, `HAS_MOVED`, `xOpen(t.db-journal, 0x806)`, 512 B journal header write, page 1 record (`4 + 4096 + 4` B), `xLock(EXCLUSIVE)`, page 2 record (`4 + 4096 + 4` B), `xRead(t.db-journal, n=8, ofst=9216) -> 522` (`SQLITE_IOERR_SHORT_READ`, where the pager probes past the journal end), 12 B header rewrite at offset 0, `xSync(t.db-journal)`, 2 database page writes (`off=0` and `off=4096`), `xFileControl(SYNC) -> 12` (`SQLITE_NOTFOUND`), `xSync(t.db)`, `xClose` and `xDelete` of `-journal`, `COMMIT_PHASETWO`, and unlock to `SHARED` then `NONE`.
  - **`delete/FULL` (A3): 32 VFS calls.** Identical to `delete/NORMAL` except one extra `xSync(t.db-journal)` at call 20 *before* the 12-byte header rewrite at call 21, bringing `xSync` from 2 to 3 (matching Experiment 7.2's 3 vs 4 sync syscalls once the one-time directory sync is counted).
  - **`wal/NORMAL` (A4): 12 VFS calls.** Zero `xOpen`, `xAccess`, `xClose`, `xDelete`, `xLock`, `xRead`, or `xSync` calls: only `xShmBarrier` (3), `xShmLock` (6, acquiring `READ1` shared and `WRITE` exclusive and releasing both), `xWrite` (2: 24-byte frame header at `20632` and 4,096-byte page at `20656`), and `xFileControl(COMMIT_PHASETWO)`.
  - **`wal/FULL` (A5): 13 VFS calls.** Identical to `wal/NORMAL` plus one `xSync(t.db-wal)` at call 9.
  - **The real `xSync` flag (the `sync_flags` and `vfstrace_label` columns of A2–A5).** In every single configuration—including `delete/FULL` and `wal/FULL`—SQLite passes `0x2 (NORMAL)` to `xSync`, while `vfstrace` prints `FULL` (because `0x2 & 0x3 != 0`, `vfstrace.c:453`). `synchronous=FULL` controls *how many* syncs SQLite performs, not the flag bit; SQLite passes `SQLITE_SYNC_FULL` (`0x3`) only when `PRAGMA fullfsync=ON`.
  - **Per-commit table (A6).** Summarizes the 4 configurations (`31`, `32`, `12`, `13` total VFS calls). This strengthens Chapter 1's syscall twin check: the translated Go driver matches the C build at the VFS method boundary call for call.
- **Section B (Part B: fault injection across recipe step 4).** Starting from recipe seed 1 built to step 3 (`159,744` bytes at rest), the experiment applies step 4 (`inserts=48 updates=6 deletes=1`, 56 statements, marker row `events id 153`) through `course-govfs` in both WAL and `DELETE` modes at `synchronous=FULL`.
  - **WAL clean run (B2–B4):** Because the step-3 database is at rest with no `-wal`, step 4 creates the WAL and makes **51 `xWrite`** calls to `t.db-wal` (`103,032` bytes: 1 32-byte header + 25 frames of 2 writes each) and **2 `xSync`** calls (`xSync` #1 at call 26 for the WAL header; `xSync` #2 at call 77 for the commit). Without faults, the file equals step 4 exactly.
  - **WAL fault trials (B5–B7):** Failing `xWrite #1` through `xWrite #51` and `xSync #1` through `xSync #2` (**53 fired faults total**) returns `SQLite error code 10 (disk I/O error (10))` at **`COMMIT`** in every trial (nothing is written to `-wal` before `COMMIT` because dirty pages stay in the page cache). Reopening with the default VFS yields `user_version=3`, marker row absent, `integrity ok`, and file state `step 3 (exact bytes)`. Notice `xSync #2` (in B7): even though all 25 frames—including the commit frame—were already written to `t.db-wal`, failing the commit `xSync` aborts before `walFrames` updates the WAL-index header in `-shm`; when the connection closes, SQLite checkpoints only up to the previous committed frame (0) and deletes `-wal`, leaving step 3 untouched. Trials `xWrite #52` and `xSync #3` are never reached and commit cleanly to step 4.
  - **`DELETE` clean run (B2–B4):** **66 `xWrite`** calls (`41` to `t.db-journal` = `53,876` bytes; `25` to `t.db` = `102,400` bytes) and **3 `xSync`** calls (`t.db-journal` at calls 67 and 69, `t.db` at call 97 of 103).
  - **`DELETE` fault trials (B5–B7):** Failing `xWrite #1` through `#66` and `xSync #1` through `#3` (**69 fired faults total**) returns error code `10` and leaves `user_version=3`, marker row absent, `integrity ok`, and file state `step 3 (exact bytes)` in every trial—including `xWrite #42` through `#66` and `xSync #3`, where pages had already been overwritten in `t.db` and SQLite rolled them back from `t.db-journal`. Unlike WAL mode, the first 40 `xWrite` faults surface *mid-transaction* when a statement first dirties a page and writes its pre-image into `t.db-journal`: statement 1 (`INSERT INTO accounts`, 4 trials), statement 5 (`INSERT INTO documents`, 6 trials), statement 9 (`INSERT INTO events`, 3 trials), statement 54 (`UPDATE documents SET`, 12 trials), and statement 55 (`DELETE FROM documents`, 15 trials). Trials `#41`–`#66` and all 3 `xSync` trials surface at `COMMIT`. `db.Close()` returned no error in any trial.
  - **Trial verification and scope:** Every trial is validated by `trialHeld` (unit-tested in [`7.3-go-vfs-shim/main_test.go`](experiments/7.3-go-vfs-shim/main_test.go)), requiring `Fired == (n <= total)`, primary error code `10` (`SQLITE_IOERR`), a clean close, and an exact unmasked byte match (`match.Exact`). Part B tests `xWrite` and `xSync` at `synchronous=FULL`; `xRead` and `xTruncate` faults (supported by `internal/govfs`), `synchronous=NORMAL`, multi-fault commits, and faults during reopen are outside Part B (see `GOVFS-03` and `GOVFS-04` in Section 6).

---

### 5.4 Shared package `internal/rangefs`

`internal/rangefs` adapts any byte-range object store (`HTTPSource` or `S3Source`) into a read-only Go `io/fs.FS` and `fs.StatFS` that logs every metadata lookup and byte-range read. Passed to `modernc.org/sqlite/vfs.New`, it lets SQLite query a remote snapshot page by page with no local copy, no caching, and no read-ahead. Its unit tests live in [`internal/rangefs/rangefs_test.go`](../internal/rangefs/rangefs_test.go) and [`internal/rangefs/edge_test.go`](../internal/rangefs/edge_test.go) (`go test ./internal/rangefs/`).

#### `internal/rangefs/rangefs.go`

<!-- include: ../internal/rangefs/rangefs.go -->
```go
// Package rangefs implements a read-only io/fs.FS and fs.StatFS backed by
// byte-range reads over HTTP or S3, recording every stat and range request.
// Passed to modernc.org/sqlite/vfs.New, it lets SQLite query a checkpointed
// remote database snapshot one page miss at a time with no local copy, no
// caching, and no read-ahead.
package rangefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"
	"time"
)

// Source fetches file metadata and exact byte ranges from a remote store.
// Stat must return an error wrapping fs.ErrNotExist when the object is absent.
type Source interface {
	Stat(ctx context.Context, name string) (int64, error)
	ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error)
}

// RequestKind distinguishes metadata checks (stat) from byte-range reads (range).
type RequestKind string

// RequestKind values recorded by FS.
const (
	KindStat  RequestKind = "stat"
	KindRange RequestKind = "range"
)

// Request logs one remote operation issued through FS. For a failed or short
// range read, Failed is true and Length records only the bytes actually received.
type Request struct {
	Kind   RequestKind
	Name   string
	Offset int64
	Length int64
	Found  bool
	Failed bool
}

// String formats a Request as a single readable line for evidence output.
func (r Request) String() string {
	switch r.Kind {
	case KindStat:
		if r.Found {
			return fmt.Sprintf("stat %s (size %d)", r.Name, r.Length)
		}
		return fmt.Sprintf("stat %s (absent)", r.Name)
	case KindRange:
		if r.Failed {
			return fmt.Sprintf("range %s at %d failed (%d bytes received)", r.Name, r.Offset, r.Length)
		}
		return fmt.Sprintf("range %s bytes %d-%d (%d bytes)", r.Name, r.Offset, r.Offset+r.Length-1, r.Length)
	}
	return string(r.Kind)
}

// Stats aggregates request counts and total bytes read across a slice of Requests.
type Stats struct {
	StatRequests  int
	RangeRequests int
	BytesRead     int64
}

// Requests returns the total number of stat and range requests.
func (s Stats) Requests() int {
	return s.StatRequests + s.RangeRequests
}

// String formats Stats as "N requests (S stat, R range), B bytes".
func (s Stats) String() string {
	return fmt.Sprintf("%d requests (%d stat, %d range), %d bytes", s.Requests(), s.StatRequests, s.RangeRequests, s.BytesRead)
}

// Summarize tallies stat requests, range requests, and bytes read in requests.
func Summarize(requests []Request) Stats {
	var s Stats
	for _, r := range requests {
		switch r.Kind {
		case KindStat:
			s.StatRequests++
		case KindRange:
			s.RangeRequests++
			s.BytesRead += r.Length
		}
	}
	return s
}

// FS adapts a Source into a read-only fs.FS and fs.StatFS that records every
// remote request. Open and Stat each issue one Source.Stat call; each non-empty
// Read or ReadAt on an open file issues one Source.ReadRange call for the exact
// bytes requested, clipped at EOF (a zero-length read issues no request).
type FS struct {
	source Source
	ctx    context.Context

	mu       sync.Mutex
	requests []Request
}

// New returns an FS that performs all Source calls using ctx. A nil ctx
// defaults to context.Background().
func New(ctx context.Context, source Source) *FS {
	if ctx == nil {
		ctx = context.Background()
	}
	return &FS{source: source, ctx: ctx}
}

func (f *FS) log(r Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
}

// Requests returns a snapshot copy of all recorded requests.
func (f *FS) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// TakeRequests returns all recorded requests and clears the log.
func (f *FS) TakeRequests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests
	f.requests = nil
	return r
}

func (f *FS) stat(op, name string) (int64, error) {
	if !fs.ValidPath(name) {
		return 0, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	size, err := f.source.Stat(f.ctx, name)
	if err != nil {
		f.log(Request{Kind: KindStat, Name: name})
		return 0, &fs.PathError{Op: op, Path: name, Err: err}
	}
	f.log(Request{Kind: KindStat, Name: name, Length: size, Found: true})
	return size, nil
}

// Open stats name and returns a read-only seekable file implementing io.ReaderAt.
func (f *FS) Open(name string) (fs.File, error) {
	size, err := f.stat("open", name)
	if err != nil {
		return nil, err
	}
	return &file{fsys: f, name: name, size: size}, nil
}

// Stat implements fs.StatFS, which modernc.org/sqlite/vfs calls to answer
// SQLite's xAccess checks (for example, probing for -journal and -wal files).
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	size, err := f.stat("stat", name)
	if err != nil {
		return nil, err
	}
	return fileInfo{name: path.Base(name), size: size}, nil
}

type file struct {
	fsys   *FS
	name   string
	size   int64
	offset int64
	closed bool
}

func (f *file) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	return fileInfo{name: path.Base(f.name), size: f.size}, nil
}

func (f *file) ReadAt(p []byte, offset int64) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if offset < 0 {
		return 0, fmt.Errorf("rangefs: negative offset %d", offset)
	}
	if offset >= f.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Clip the range at EOF so the store never receives an out-of-bounds
	// Range header, then return io.EOF on a short read as io.ReaderAt requires.
	want := min(int64(len(p)), f.size-offset)
	n, err := f.fsys.source.ReadRange(f.fsys.ctx, f.name, offset, p[:want])
	if err != nil {
		f.fsys.log(Request{Kind: KindRange, Name: f.name, Offset: offset, Length: int64(n), Failed: true})
		return n, err
	}
	if int64(n) != want {
		f.fsys.log(Request{Kind: KindRange, Name: f.name, Offset: offset, Length: int64(n), Failed: true})
		return n, fmt.Errorf("rangefs: %s: short range read %d of %d bytes at %d", f.name, n, want, offset)
	}
	f.fsys.log(Request{Kind: KindRange, Name: f.name, Offset: offset, Length: want})
	if want < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := f.ReadAt(p, f.offset)
	f.offset += int64(n)
	if err == io.EOF && n > 0 {
		return n, nil
	}
	return n, err
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return f.offset, fs.ErrClosed
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.offset + offset
	case io.SeekEnd:
		next = f.size + offset
	default:
		return f.offset, fmt.Errorf("rangefs: bad whence %d", whence)
	}
	if next < 0 {
		return f.offset, errors.New("rangefs: seek before start of file")
	}
	f.offset = next
	return next, nil
}

func (f *file) Close() error {
	f.closed = true
	return nil
}

type fileInfo struct {
	name string
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return 0o444 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }
```

**Walk-through (`rangefs.go`).** Defines the two-method `Source` interface (`Stat` and `ReadRange`). `New(nil, source)` defaults to `context.Background()`. `FS.Open` and `FS.Stat` (implementing `fs.StatFS`, which `modernc.org/sqlite/vfs` calls for SQLite's `xAccess` checks) each perform one `Source.Stat` and log a `KindStat` request (recording `Found: false` when absent). Each open `file` implements `Read`, `ReadAt`, `Seek`, `Stat`, and `Close`, returning `fs.ErrClosed` on `ReadAt`, `Seek`, and `Stat` after `Close`. A zero-length `ReadAt` returns `0, nil` without issuing or logging a request; otherwise `ReadAt` clips the requested length at `f.size - offset` so the store never sees an out-of-bounds range header, invokes `Source.ReadRange` for those exact bytes, and returns `io.EOF` when clipped (matching `io.ReaderAt`'s contract). Failed or short `ReadRange` calls log `Request{Kind: KindRange, Failed: true, Length: int64(n)}` (`range NAME at OFF failed (N bytes received)`) so `Summarize` counts only bytes actually received. `TakeRequests` and `Summarize` drive Experiment 7.4's request accounting.

#### `internal/rangefs/http.go`

<!-- include: ../internal/rangefs/http.go -->
```go
package rangefs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// HTTPSource implements Source against an HTTP server that supports HEAD and
// byte-range GET requests (206 Partial Content).
type HTTPSource struct {
	BaseURL string
	Client  *http.Client
}

func (h HTTPSource) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

func (h HTTPSource) url(name string) string {
	return strings.TrimSuffix(h.BaseURL, "/") + "/" + (&url.URL{Path: name}).EscapedPath()
}

// Stat issues an HTTP HEAD request for name and returns its Content-Length,
// mapping 404 Not Found to fs.ErrNotExist.
func (h HTTPSource) Stat(ctx context.Context, name string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, h.url(name), nil)
	if err != nil {
		return 0, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, fs.ErrNotExist
	default:
		return 0, fmt.Errorf("HEAD %s: %s", name, resp.Status)
	}
	if resp.ContentLength < 0 {
		return 0, fmt.Errorf("HEAD %s: no Content-Length", name)
	}
	return resp.ContentLength, nil
}

// ReadRange issues an HTTP GET with an inclusive Range header for len(p) bytes
// starting at offset, requiring a 206 Partial Content response so a server
// ignoring Range cannot silently send the entire file.
func (h HTTPSource) ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(name), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1))
	resp, err := h.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("GET %s range %d+%d: %s (want 206 Partial Content)", name, offset, len(p), resp.Status)
	}
	return io.ReadFull(resp.Body, p)
}
```

**Walk-through (`http.go`).** Implements `Source` over standard HTTP: `Stat` sends `HEAD` and reads `Content-Length` (mapping `404` to `fs.ErrNotExist`); `ReadRange` sends `GET` with an inclusive `Range: bytes=start-end` header and requires `206 Partial Content` so a server that ignores `Range` cannot silently stream the whole file.

#### `internal/rangefs/s3.go`

<!-- include: ../internal/rangefs/s3.go -->
```go
package rangefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Default connection settings matching the SeaweedFS S3 service in compose.yaml
// and docker/seaweedfs/s3.json.
const (
	DefaultS3Endpoint  = "http://127.0.0.1:18333"
	DefaultS3Region    = "us-east-1"
	DefaultS3AccessKey = "course"
	DefaultS3SecretKey = "course-secret"
)

// S3Config holds endpoint, region, and static credentials for an S3-compatible store.
type S3Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// S3ConfigFromEnv reads COURSE_S3_ENDPOINT, COURSE_S3_REGION,
// COURSE_S3_ACCESS_KEY, and COURSE_S3_SECRET_KEY, falling back to the
// compose.yaml SeaweedFS defaults.
func S3ConfigFromEnv() S3Config {
	return S3Config{
		Endpoint:  envOr("COURSE_S3_ENDPOINT", DefaultS3Endpoint),
		Region:    envOr("COURSE_S3_REGION", DefaultS3Region),
		AccessKey: envOr("COURSE_S3_ACCESS_KEY", DefaultS3AccessKey),
		SecretKey: envOr("COURSE_S3_SECRET_KEY", DefaultS3SecretKey),
	}
}

// NewS3Client builds a path-style S3 client for cfg. Checksum calculation and
// response validation are limited to "when required" so plain range GETs against
// SeaweedFS work without extra checksum headers.
func NewS3Client(cfg S3Config) *s3.Client {
	return s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),

		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// EnsureBucket creates bucket if it does not already exist, ignoring
// BucketAlreadyOwnedByYou and BucketAlreadyExists.
func EnsureBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	switch errorCode(err) {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return nil
	}
	return err
}

// PutObject uploads body to bucket/key.
func PutObject(ctx context.Context, client *s3.Client, bucket, key string, body []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)})
	return err
}

// DeleteObject removes bucket/key from the store.
func DeleteObject(ctx context.Context, client *s3.Client, bucket, key string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	return err
}

// S3Source implements Source against an S3 bucket and optional key prefix using
// HeadObject and ranged GetObject calls.
type S3Source struct {
	Client *s3.Client
	Bucket string
	Prefix string
}

func (s S3Source) key(name string) string {
	if s.Prefix == "" {
		return name
	}
	return strings.TrimSuffix(s.Prefix, "/") + "/" + name
}

// Stat calls HeadObject for name under s.Prefix and maps NotFound/NoSuchKey to
// fs.ErrNotExist.
func (s S3Source) Stat(ctx context.Context, name string) (int64, error) {
	out, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.key(name))})
	if err != nil {
		switch errorCode(err) {
		case "NotFound", "NoSuchKey":
			return 0, fs.ErrNotExist
		}
		return 0, err
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("HeadObject %s: no content length", name)
	}
	return *out.ContentLength, nil
}

// ReadRange calls GetObject with an inclusive byte-range header for len(p)
// bytes starting at offset. It verifies that Content-Range begins with
// "bytes OFF-LAST/", rejecting S3 endpoints that ignore Range and return the
// full object.
func (s S3Source) ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.key(name)),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1)),
	})
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	last := offset + int64(len(p)) - 1
	wantPrefix := fmt.Sprintf("bytes %d-%d/", offset, last)
	if out.ContentRange == nil || !strings.HasPrefix(*out.ContentRange, wantPrefix) {
		got := "none"
		if out.ContentRange != nil {
			got = *out.ContentRange
		}
		return 0, fmt.Errorf("GetObject %s range %d+%d: Content-Range %q (want %q)", name, offset, len(p), got, wantPrefix+"*")
	}
	return io.ReadFull(out.Body, p)
}
```

**Walk-through (`s3.go`).** Implements `Source` against any S3-compatible store (`HeadObject` for `Stat`, mapping `NotFound`/`NoSuchKey` to `fs.ErrNotExist`, and `GetObject` with `Range: bytes=start-end` for `ReadRange`). Just as `HTTPSource.ReadRange` requires `206 Partial Content`, `S3Source.ReadRange` verifies that `Content-Range` starts with `"bytes OFF-LAST/"`, rejecting any endpoint that ignores `Range` and returns the full object from offset 0 (`edge_test.go`). `S3ConfigFromEnv` reads `COURSE_S3_ENDPOINT` (default `http://127.0.0.1:18333`), `COURSE_S3_REGION` (`us-east-1`), `COURSE_S3_ACCESS_KEY` (`course`), and `COURSE_S3_SECRET_KEY` (`course-secret`), matching `compose.yaml` and `docker/seaweedfs/s3.json`. `NewS3Client` enables path-style addressing and sets request/response checksum options to `WhenRequired` so plain ranged `GetObject` calls against SeaweedFS carry no unnecessary checksum work.

---

### 5.5 Experiment 7.4 — Read-only snapshot VFS over HTTP and S3 (`7.4-read-only-snapshot-vfs`)

- **Claim:** A fully checkpointed 30-step WAL recipe snapshot (`1,105,920` bytes, `270` pages) can be queried remotely through `modernc.org/sqlite/vfs` + `internal/rangefs` over HTTP or S3/SeaweedFS with identical results to the local file, paying exactly one range request per page miss (no read-ahead) and zero requests on repeat queries whose working set still fits in the connection's page cache (`immutable=1` keeps cached pages valid, not cached; shrinking `cache_size=4` forces re-fetches of evicted pages). Both `immutable=1` and a fully checkpointed, truly immutable object are mandatory for WAL-format snapshots: omitting `immutable=1` fails with `SQLITE_CANTOPEN` (14), publishing an uncheckpointed database alongside its `-wal` silently serves stale pre-WAL data under `immutable=1`, and overwriting an object key in place behind an open `immutable=1` connection produces `SQLITE_CORRUPT` (`11`).
- **Evidence form:** `EXACT` (every stored table must match on every run). The checked evidence is the HTTP route; the `-s3` walkthrough below differs from it only in the route tables and is not checked by `go run ./cmd/course check evidence ch07`.
- **Platform:** Portable (no `strace`). Default HTTP route runs everywhere with no services. The `-s3` route requires SeaweedFS (`docker compose up -d seaweedfs`) and is run directly in the `lab` container, because `course run` cannot pass experiment flags.

<!-- include: experiments/7.4-read-only-snapshot-vfs/main.go -->
```go
// Experiment 7.4 serves a checkpointed 30-step recipe snapshot over HTTP (by
// default) or SeaweedFS S3 (-s3) and queries it through modernc.org/sqlite/vfs
// backed by internal/rangefs. It counts stat and byte-range requests for a
// point lookup, index range scan, and full scan, shows cache eviction under
// PRAGMA cache_size=4, verifies remote results equal the local file, and
// demonstrates why immutable=1 and a fully checkpointed file are both required
// for WAL-format snapshots.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.4. To read from S3 instead,
// run docker compose up -d seaweedfs and then the package itself with -s3:
//
//	go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"modernc.org/sqlite/vfs"
)

const (
	pageSize     = 4096
	headerBytes  = 100
	snapshotName = "snapshot.db"
	rollbackName = "snapshot-rollback.db"
	liveName     = "live.db"
	s3Bucket     = "course"
	s3Prefix     = "ch07/7.4"
)

type query struct {
	name string
	sql  string
}

var queries = []query{
	{"point lookup", "SELECT id, account_id, kind, amount, note FROM events WHERE id = 372"},
	{"index range scan", "SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id"},
	{"full scan", "SELECT count(*), sum(amount), sum(length(note)) FROM events"},
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.4" runs both.
var (
	set    *results.Set
	tables struct {
		requests, opens *results.Table
	}
	requestSeq int
)

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

// logRequests writes the remote requests of one connection (attempt) of a
// part; phase says what the connection was doing when it made them.
func logRequests(part string, attempt int, phase string, requests []rangefs.Request) {
	for _, req := range requests {
		requestSeq++
		tables.requests.Row(requestSeq, part, attempt, phase, string(req.Kind), req.Name, req.Offset, req.Length, req.Found, req.Failed, req.String())
	}
}

type publisher interface {
	publish(ctx context.Context, name string, body []byte) error
	remove(ctx context.Context, name string) error
	source() rangefs.Source
	describe() (route, server, stat, read string)
}

type httpPublisher struct {
	dir    string
	server *httptest.Server
}

func newHTTPPublisher(dir string) (*httpPublisher, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &httpPublisher{dir: dir, server: httptest.NewServer(http.FileServer(http.Dir(dir)))}, nil
}

func (p *httpPublisher) publish(ctx context.Context, name string, body []byte) error {
	return os.WriteFile(filepath.Join(p.dir, name), body, 0o644)
}

func (p *httpPublisher) remove(ctx context.Context, name string) error {
	err := os.Remove(filepath.Join(p.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (p *httpPublisher) source() rangefs.Source {
	return rangefs.HTTPSource{BaseURL: p.server.URL, Client: p.server.Client()}
}

func (p *httpPublisher) describe() (route, server, stat, read string) {
	return "http", "an in-process HTTP server (net/http FileServer, which answers Range requests)", "HEAD", "GET with a Range header"
}

type s3Publisher struct {
	client *s3.Client
}

func (p *s3Publisher) publish(ctx context.Context, name string, body []byte) error {
	return rangefs.PutObject(ctx, p.client, s3Bucket, s3Prefix+"/"+name, body)
}

func (p *s3Publisher) remove(ctx context.Context, name string) error {
	return rangefs.DeleteObject(ctx, p.client, s3Bucket, s3Prefix+"/"+name)
}

func (p *s3Publisher) source() rangefs.Source {
	return rangefs.S3Source{Client: p.client, Bucket: s3Bucket, Prefix: s3Prefix}
}

func (p *s3Publisher) describe() (route, server, stat, read string) {
	return "s3", fmt.Sprintf("SeaweedFS from compose.yaml, bucket %q, keys %s/<name>", s3Bucket, s3Prefix), "HeadObject", "GetObject with a Range header"
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child-writer" {
		childWriter(os.Args[2])
		return
	}
	useS3 := flag.Bool("s3", false, "read the snapshot from the SeaweedFS S3 service instead of the in-process HTTP server")
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	tables.requests = set.Table("requests", "seq", "part", "attempt", "phase", "kind", "name", "offset", "length", "found", "failed", "request")
	tables.opens = set.Table("opens", "part", "attempt", "file", "params", "result", "error_code")

	var pub publisher
	if *useS3 {
		cfg := rangefs.S3ConfigFromEnv()
		client := rangefs.NewS3Client(cfg)
		timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := rangefs.EnsureBucket(timeout, client, s3Bucket)
		cancel()
		if err != nil {
			fatal("S3 bucket", fmt.Errorf("%w (start the store with: docker compose up -d seaweedfs)", err))
		}
		fmt.Printf("INFO: S3 endpoint %s\n", cfg.Endpoint)
		set.Table("info_s3", "endpoint").Row(cfg.Endpoint)
		pub = &s3Publisher{client: client}
	} else {
		p, err := newHTTPPublisher(filepath.Join(dir, "published"))
		if err != nil {
			fatal("http", err)
		}
		defer p.server.Close()
		pub = p
	}
	route, server, stat, read := pub.describe()
	set.Table("route", "route", "server", "stat_request", "read_request").Row(route, server, stat, read)
	for _, name := range []string{snapshotName, rollbackName, liveName, liveName + "-wal", liveName + "-journal", snapshotName + "-wal", snapshotName + "-journal"} {
		if err := pub.remove(ctx, name); err != nil {
			fatal("remove "+name, err)
		}
	}
	remote := rangefs.New(ctx, pub.source())
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	r := recipe.Default()
	local := filepath.Join(dir, snapshotName)
	snapshot := buildSnapshot(ctx, r, local)
	if err := pub.publish(ctx, snapshotName, snapshot); err != nil {
		fatal("publish", err)
	}

	queryTable := set.Table("queries", "query", "name", "sql", "plan", "cache_pages", "local_rows", "local_digest", "local_first_row", "remote_rows", "remote_digest", "remote_first_row")
	for i, q := range queries {
		runQuery(ctx, queryTable, i+1, q, local, vfsName, remote)
	}
	smallCache(ctx, queries[len(queries)-1], vfsName, remote)
	withoutImmutable(ctx, vfsName, remote, snapshotName, local)
	rollbackFormat(ctx, pub, snapshot, vfsName, remote, local)
	staleRead(ctx, r, dir, pub, vfsName, remote)
	set.Finish("READ-ONLY SNAPSHOT VFS")
}

func buildSnapshot(ctx context.Context, r recipe.Recipe, path string) []byte {
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build snapshot", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fatal("read snapshot", err)
	}
	_, walErr := os.Stat(path + "-wal")
	set.Table("snapshot", "seed", "steps", "journal_mode", "bytes", "pages", "sha256_prefix", "file_state", "header_byte_18", "header_byte_19", "wal_present").
		Row(r.Seed, r.Steps, r.JournalMode, len(body), len(body)/pageSize, shortHash(body), match.String(), body[18], body[19], walErr == nil)
	set.Check("A", body[18] == 2 && body[19] == 2 && os.IsNotExist(walErr) && match.Step == r.Steps && match.Exact, "the snapshot is a WAL-format database with every commit checkpointed into the main file and no -wal left")
	return body
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func remoteDSN(vfsName, name, params string) string {
	dsn := "file:" + name + "?vfs=" + vfsName
	if params != "" {
		dsn += "&" + params
	}
	return dsn
}

func openSingle(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func render(ctx context.Context, db *sql.DB, query string) (int, string, string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, "", "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, "", "", err
	}
	var all strings.Builder
	first := ""
	n := 0
	for rows.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return 0, "", "", err
		}
		parts := make([]string, len(values))
		for i, v := range values {
			switch x := v.(type) {
			case []byte:
				parts[i] = fmt.Sprintf("%q", x)
			default:
				parts[i] = fmt.Sprintf("%v", x)
			}
		}
		line := strings.Join(parts, "|")
		if n == 0 {
			first = line
		}
		all.WriteString(line + "\n")
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, "", "", err
	}
	return n, shortHash([]byte(all.String())), first, nil
}

func localResult(ctx context.Context, path, query string) (int, string, string, []string) {
	db, err := openSingle("file:" + path + "?mode=ro")
	if err != nil {
		fatal("local open", err)
	}
	defer db.Close()
	n, digest, first, err := render(ctx, db, query)
	if err != nil {
		fatal("local query", err)
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		fatal("plan", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			fatal("plan", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		fatal("plan", err)
	}
	return n, digest, first, plan
}

// oddReads returns the range requests that are neither one whole page nor the
// 100-byte header.
func oddReads(requests []rangefs.Request) []string {
	var odd []string
	for _, req := range requests {
		if req.Kind != rangefs.KindRange {
			continue
		}
		header := req.Offset == 0 && req.Length == headerBytes
		page := req.Offset%pageSize == 0 && req.Length == pageSize
		if !header && !page {
			odd = append(odd, req.String())
		}
	}
	return odd
}

func runQuery(ctx context.Context, queryTable *results.Table, n int, q query, local, vfsName string, remote *rangefs.FS) {
	localRows, localDigest, localFirst, plan := localResult(ctx, local, q.sql)

	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("remote open", err)
	}
	var schemaRows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&schemaRows); err != nil {
		fatal("remote schema", err)
	}
	cachePages := mustCachePages(ctx, db)
	openRequests := remote.TakeRequests()
	rows, digest, first, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("remote query", err)
	}
	queryRequests := remote.TakeRequests()
	if _, _, _, err := render(ctx, db, q.sql); err != nil {
		fatal("remote repeat", err)
	}
	repeatRequests := remote.TakeRequests()
	if err := db.Close(); err != nil {
		fatal("remote close", err)
	}
	queryTable.Row(n, q.name, q.sql, strings.Join(plan, "; "), cachePages, localRows, localDigest, localFirst, rows, digest, first)
	logRequests("B", n, "open", openRequests)
	logRequests("B", n, "query", queryRequests)
	logRequests("B", n, "repeat", repeatRequests)
	openStats, queryStats, repeatStats := rangefs.Summarize(openRequests), rangefs.Summarize(queryRequests), rangefs.Summarize(repeatRequests)
	odd, oddOpen := oddReads(queryRequests), oddReads(openRequests)
	set.Check("B", rows > 0 && rows == localRows && digest == localDigest, fmt.Sprintf("%s: the remote result equals the local file's (%d row(s), digest %s)", q.name, rows, digest))
	set.Check("B", len(odd) == 0 && len(oddOpen) == 0 && queryStats.StatRequests == 0 && queryStats.RangeRequests > 0, fmt.Sprintf("%s: every read is one range request for one whole %d-byte page (or the %d-byte header): one request per page miss, no read-ahead", q.name, pageSize, headerBytes))
	pagesHeld := openStats.RangeRequests + queryStats.RangeRequests
	set.Check("B", repeatStats.Requests() == 0 && pagesHeld <= cachePages, fmt.Sprintf("%s: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the %d reads so far fit in the %d-page cache)", q.name, pagesHeld, cachePages))
}

// smallCachePages shrinks the page cache below the 18-page events scan to show
// that immutable=1 skips change-counter invalidation but does not prevent cache
// eviction (pager.c:5033-5056,5436; default cache is -2000 KiB, sqliteLimit.h:161).
const smallCachePages = 4

// mustCachePages returns the connection's PRAGMA cache_size expressed in pages.
func mustCachePages(ctx context.Context, db *sql.DB) int {
	var cacheSize int
	if err := db.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cacheSize); err != nil {
		fatal("cache_size", err)
	}
	if cacheSize < 0 {
		return -cacheSize * 1024 / pageSize
	}
	return cacheSize
}

// smallCache repeats the full events scan with a 4-page cache on one immutable=1
// connection, showing that evicted pages still incur remote range requests.
func smallCache(ctx context.Context, q query, vfsName string, remote *rangefs.FS) {
	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("small-cache open", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size=%d", smallCachePages)); err != nil {
		fatal("small-cache pragma", err)
	}
	cachePages := mustCachePages(ctx, db)
	_, firstDigest, _, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("small-cache query", err)
	}
	remote.TakeRequests()
	_, repeatDigest, _, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("small-cache repeat", err)
	}
	repeat := remote.TakeRequests()
	repeatStats := rangefs.Summarize(repeat)
	set.Table("small_cache", "query", "cache_pages", "same_result").Row(q.name, cachePages, firstDigest == repeatDigest)
	logRequests("C", 1, "repeat", repeat)
	set.Check("C", firstDigest == repeatDigest && repeatStats.RangeRequests > cachePages, fmt.Sprintf("%s: when the query reads more pages than the cache holds, the repeat reads evicted pages again (%d range requests); only pages still in the cache are free", q.name, repeatStats.RangeRequests))
}

func errorLine(err error) string {
	if err == nil {
		return "no error"
	}
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("SQLite error code %d: %s", code, err.Error())
	}
	return err.Error()
}

func tryOpen(ctx context.Context, vfsName string, remote *rangefs.FS, name, params string) (string, error, []rangefs.Request) {
	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, name, params))
	if err != nil {
		return "", err, nil
	}
	defer db.Close()
	var count int
	var version int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count)
	if err == nil {
		err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	}
	requests := remote.TakeRequests()
	if err != nil {
		return "", err, requests
	}
	return fmt.Sprintf("user_version=%d events=%d", version, count), nil, requests
}

func localState(ctx context.Context, path string) string {
	db, err := openSingle("file:" + path)
	if err != nil {
		fatal("local", err)
	}
	defer db.Close()
	var count, version int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil {
		fatal("local count", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("local version", err)
	}
	return fmt.Sprintf("user_version=%d events=%d", version, count)
}

// openRow writes one remote open of part and its requests.
func openRow(part string, attempt int, name, params, state string, err error, requests []rangefs.Request) {
	var code any
	if c, ok := sqlitex.ErrorCode(err); ok {
		code = c
	}
	if params == "" {
		params = "(no parameters)"
	}
	tables.opens.Row(part, attempt, name, params, fallback(state, err), code)
	logRequests(part, attempt, "open", requests)
}

func withoutImmutable(ctx context.Context, vfsName string, remote *rangefs.FS, name, local string) {
	for i, params := range []string{"", "mode=ro", "immutable=1"} {
		label := params
		if label == "" {
			label = "(no parameters)"
		}
		state, err, requests := tryOpen(ctx, vfsName, remote, name, params)
		openRow("D", i+1, name, params, state, err, requests)
		code, _ := sqlitex.ErrorCode(err)
		switch params {
		case "immutable=1":
			set.Check("D", err == nil && state == localState(ctx, local), "with immutable=1 the WAL-format snapshot opens and reads the same state as the local file")
		default:
			set.Check("D", code == 14, fmt.Sprintf("without immutable=1 (%s) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)", label))
		}
	}
}

// rollbackFormat flips database header bytes 18 and 19 (file format write and
// read versions) from 2 (WAL) to 1 (rollback) on a copy of the checkpointed
// snapshot to prove that the header's WAL flag—not the page content—is what
// makes SQLite require shared memory (io_methods version >= 2) when immutable=1
// is omitted.
func rollbackFormat(ctx context.Context, pub publisher, snapshot []byte, vfsName string, remote *rangefs.FS, local string) {
	patched := append([]byte(nil), snapshot...)
	patched[18], patched[19] = 1, 1
	if err := pub.publish(ctx, rollbackName, patched); err != nil {
		fatal("publish", err)
	}
	state, err, requests := tryOpen(ctx, vfsName, remote, rollbackName, "")
	openRow("E", 1, rollbackName, "", state, err, requests)
	set.Check("E", err == nil && state == localState(ctx, local), "a checkpointed snapshot with header bytes 18-19 rewritten to 1 opens without immutable=1 and reads the same state (the WAL flag in the header, not the content, is what demands shared memory)")
	journalChecks := 0
	for _, req := range requests {
		if req.Kind == rangefs.KindStat && strings.HasSuffix(req.Name, "-journal") {
			journalChecks++
		}
	}
	set.Check("E", journalChecks > 0, fmt.Sprintf("without immutable=1 SQLite checks for a hot journal (%d stat request(s) for %s-journal), which immutable=1 skips", journalChecks, rollbackName))
}

func fallback(state string, err error) string {
	if err != nil {
		return errorLine(err)
	}
	return state
}

// childWriter commits the final recipe step in a separate process and holds its
// connection open until stdin closes. Keeping the writer in a child process
// ensures the parent's plain os.ReadFile calls do not close a descriptor to the
// same file inside the writer process and drop its POSIX locks.
func childWriter(path string) {
	ctx := context.Background()
	r := recipe.Default()
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	if err := r.ApplyStep(ctx, db, r.Steps); err != nil {
		fatal("child step", err)
	}
	fmt.Println("READY")
	io.Copy(io.Discard, os.Stdin)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func staleRead(ctx context.Context, r recipe.Recipe, dir string, pub publisher, vfsName string, remote *rangefs.FS) {
	live := filepath.Join(dir, liveName)
	if err := r.Build(ctx, live, r.Steps-1); err != nil {
		fatal("build live", err)
	}
	command, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	child := exec.CommandContext(ctx, command, "child-writer", live)
	stdin, err := child.StdinPipe()
	if err != nil {
		fatal("pipe", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		fatal("pipe", err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fatal("child", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "READY" {
		fatal("child", fmt.Errorf("child said %q: %v", line, err))
	}
	truth := localState(ctx, live)
	dbBytes, err := os.ReadFile(live)
	if err != nil {
		fatal("read live", err)
	}
	walBytes, err := os.ReadFile(live + "-wal")
	if err != nil {
		fatal("read live wal", err)
	}
	liveTable := set.Table("live", "built_to_step", "committed_step", "local_state", "db_bytes", "wal_bytes", "wal_frames", "wal_deleted_after_close")
	liveRow := []any{r.Steps - 1, r.Steps, truth, len(dbBytes), len(walBytes), (len(walBytes) - 32) / (24 + pageSize)}
	if err := pub.publish(ctx, liveName, dbBytes); err != nil {
		fatal("publish", err)
	}
	if err := pub.publish(ctx, liveName+"-wal", walBytes); err != nil {
		fatal("publish", err)
	}
	stale, err, requests := tryOpen(ctx, vfsName, remote, liveName, "immutable=1")
	openRow("F", 1, liveName, "immutable=1", stale, err, requests)
	walRequests := 0
	for _, req := range requests {
		if strings.HasSuffix(req.Name, "-wal") {
			walRequests++
		}
	}
	_, plainErr, plainRequests := tryOpen(ctx, vfsName, remote, liveName, "")
	openRow("F", 2, liveName, "", "", plainErr, plainRequests)
	wantStale := fmt.Sprintf("user_version=%d", r.Steps-1)
	set.Check("F", err == nil && strings.HasPrefix(stale, wantStale+" ") && strings.HasPrefix(truth, fmt.Sprintf("user_version=%d ", r.Steps)) && stale != truth,
		fmt.Sprintf("immutable=1 ignores the -wal: the remote read returns step %d's state while the writer has committed step %d (a stale read, no error)", r.Steps-1, r.Steps))
	set.Check("F", walRequests == 0, "with immutable=1 SQLite never asks for the -wal file (0 requests name it)")
	code, _ := sqlitex.ErrorCode(plainErr)
	set.Check("F", code == 14, "without immutable=1 the live WAL database cannot be opened through the read-only VFS either (SQLITE_CANTOPEN)")

	stdin.Close()
	if err := child.Wait(); err != nil {
		fatal("child exit", err)
	}
	_, walErr := os.Stat(live + "-wal")
	dbBytes, err = os.ReadFile(live)
	if err != nil {
		fatal("read live", err)
	}
	if err := pub.publish(ctx, liveName, dbBytes); err != nil {
		fatal("publish", err)
	}
	if err := pub.remove(ctx, liveName+"-wal"); err != nil {
		fatal("remove", err)
	}
	fresh, err, freshRequests := tryOpen(ctx, vfsName, remote, liveName, "immutable=1")
	openRow("F", 3, liveName, "immutable=1", fresh, err, freshRequests)
	liveTable.Row(append(liveRow, os.IsNotExist(walErr))...)
	set.Check("F", err == nil && fresh == truth && os.IsNotExist(walErr), fmt.Sprintf("once the commit is checkpointed into the main file, the republished snapshot shows step %d", r.Steps))
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/7.4-read-only-snapshot-vfs/report.sql -->
```sql
-- Walkthrough of experiment 7.4. Run it with:
--   go run ./cmd/course run 7.4        (add -i to stay in DuckDB afterwards)
-- The experiment also runs alone against SeaweedFS S3 instead of the
-- in-process HTTP server: docker compose up -d seaweedfs, then
--   go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   route         one row: how the snapshot is served and read
--   snapshot      one row: the published WAL-format snapshot
--   queries       part B: one row per query, its plan, and its result read
--                 from the local file and from the remote snapshot
--   small_cache   part C: one row: the full scan repeated with a 4-page cache
--   opens         parts D-F: one row per remote open and what it read or the error
--   live          part F: one row: the live database whose last commit is in its -wal
--   requests      one row per remote request: part, attempt (query or open number),
--                 phase (open = open and schema load, query, repeat), kind (stat or
--                 range), file, offset and length
--   info_s3       with -s3 only: the S3 endpoint (machine-dependent)
--   checks        every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  7.4  A read-only snapshot VFS over byte-range reads'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A checkpointed recipe database is published as one object and queried'
.print 'through modernc.org/sqlite/vfs backed by internal/rangefs, which turns'
.print 'each open into one stat request and each read into one byte-range'
.print 'request, and records every request.'

SELECT route, server FROM route ORDER BY route;

SELECT stat_request, read_request FROM route ORDER BY route;

CREATE TEMP VIEW request_stats AS
SELECT part, attempt, phase,
       count(*) AS requests,
       count(*) FILTER (kind = 'stat') AS stat,
       count(*) FILTER (kind = 'range') AS range,
       coalesce(sum(length) FILTER (kind = 'range'), 0) AS bytes,
       string_agg(CASE WHEN kind <> 'range' THEN NULL
                       WHEN "offset" = 0 AND length = 100 THEN 'hdr'
                       ELSE ("offset" // 4096 + 1)::VARCHAR END, ' ' ORDER BY seq) AS pages
FROM requests
GROUP BY part, attempt, phase;

.print
.print '── A. The snapshot ─────────────────────────────────────────────────────'
.print
.print 'A1. The recipe database after all its steps, closed by its last'
.print 'connection, which checkpointed every commit into the main file:'

SELECT seed, steps, journal_mode, bytes, pages, sha256_prefix, file_state
FROM snapshot ORDER BY seed;

.print
.print 'A2. Header bytes 18-19 (file format write and read version) still say 2,'
.print 'WAL, although no -wal file is left:'

SELECT header_byte_18, header_byte_19, wal_present FROM snapshot ORDER BY seed;

.print
.print '── B. Three queries over the remote snapshot (immutable=1) ─────────────'
.print
.print 'B1. Each query runs on a new connection: open and schema load, the query'
.print 'with a cold cache, then the same query again.'

SELECT query, name, sql FROM queries ORDER BY query;

SELECT query, name, plan FROM queries ORDER BY query;

.print
.print 'B2. The remote result equals the result from the local file (digest ='
.print 'sha256 prefix of all rows):'

SELECT query, name, local_rows, remote_rows, local_digest, remote_digest
FROM queries ORDER BY query;

SELECT query, local_first_row = remote_first_row AS same_first_row, remote_first_row AS first_row
FROM queries ORDER BY query;

.print
.print 'B3. Opening and loading the schema costs the same three requests every'
.print 'time: a stat, the 100-byte header, and page 1.'

SELECT request, count(*) AS connections
FROM requests WHERE part = 'B' AND phase = 'open'
GROUP BY request
ORDER BY min(seq);

.print
.print 'B4. Every read is one range request for one whole 4096-byte page: one'
.print 'request per page miss, no read-ahead. The repeat on the same connection'
.print 'makes no request: immutable=1 skips the change-counter check, so the'
.print 'cached pages stay valid. Pages are listed in the order they were read.'

SELECT q.query, p.phase, coalesce(s.requests, 0) AS requests, coalesce(s.stat, 0) AS stat,
       coalesce(s.range, 0) AS range, coalesce(s.bytes, 0) AS bytes, s.pages
FROM queries q
CROSS JOIN (VALUES ('open', 1), ('query', 2), ('repeat', 3)) p(phase, ord)
LEFT JOIN request_stats s ON s.part = 'B' AND s.attempt = q.query AND s.phase = p.phase
ORDER BY q.query, p.ord;

.print
.print 'B5. The first query on a new connection, in total, and how many of its'
.print 'page reads the page cache can hold:'

SELECT q.name, sum(s.requests) AS requests, sum(s.bytes) AS bytes,
       sum(s.range) AS pages_read, any_value(q.cache_pages) AS cache_pages
FROM queries q JOIN request_stats s ON s.part = 'B' AND s.attempt = q.query AND s.phase IN ('open', 'query')
GROUP BY q.query, q.name
ORDER BY q.query;

.print
.print '── C. immutable=1 keeps cached pages valid, not cached ─────────────────'
.print
.print 'C1. The full scan again, repeated on one connection with PRAGMA'
.print 'cache_size=4. The scan reads more pages than the cache holds, so the'
.print 'repeat reads the evicted pages again; only pages still cached are free.'

SELECT c.query, c.cache_pages, c.same_result, s.requests, s.stat, s.range, s.bytes
FROM small_cache c JOIN request_stats s ON s.part = 'C' AND s.phase = 'repeat'
ORDER BY c.query;

.print
.print '── D. Why immutable=1: the WAL-format snapshot opened without it ───────'
.print
.print 'D1. Each open counts the events and reads user_version. Without'
.print 'immutable=1, SQLite sees the WAL flag in the header, needs shared memory,'
.print 'which this read-only VFS does not provide, and fails with SQLITE_CANTOPEN.'

SELECT o.params, o.result, s.requests, s.stat, s.range, s.bytes
FROM opens o LEFT JOIN request_stats s ON s.part = o.part AND s.attempt = o.attempt
WHERE o.part = 'D'
ORDER BY o.attempt;

.print
.print 'D2. The requests of each open: without immutable=1 SQLite also looks for'
.print 'a -journal and a -wal.'

SELECT o.params, r.request
FROM requests r JOIN opens o USING (part, attempt)
WHERE r.part = 'D'
ORDER BY r.seq;

.print
.print '── E. The same bytes in rollback format ────────────────────────────────'
.print
.print 'E1. A copy of the snapshot with header bytes 18-19 set to 1 (rollback'
.print 'format) opens without immutable=1 and reads the same state: the WAL flag'
.print 'in the header, not the content, is what demands shared memory.'

SELECT o.file, o.params, o.result, s.requests, s.stat, s.range, s.bytes
FROM opens o LEFT JOIN request_stats s ON s.part = o.part AND s.attempt = o.attempt
WHERE o.part = 'E'
ORDER BY o.attempt;

.print
.print 'E2. Its requests. Without immutable=1 SQLite checks for a hot journal'
.print '(stat of the -journal) whenever it takes a read lock; immutable=1 skips it.'

SELECT seq, request FROM requests WHERE part = 'E' ORDER BY seq;

SELECT count(*) AS journal_stat_requests
FROM requests WHERE part = 'E' AND kind = 'stat' AND name LIKE '%-journal';

.print
.print '── F. Why a fully checkpointed file ────────────────────────────────────'
.print
.print 'F1. A child process builds live.db to step 29, commits step 30 and keeps'
.print 'its connection open, so step 30 is only in the -wal. A local read through'
.print 'SQLite sees it. Both files are published as they are on disk.'

SELECT built_to_step, committed_step, local_state, db_bytes, wal_bytes, wal_frames
FROM live ORDER BY built_to_step;

.print
.print 'F2. The remote reads. 1: immutable=1 ignores the -wal and returns the'
.print 'state of step 29, a stale read with no error. 2: without immutable=1 the'
.print 'open fails. 3: after the writer closes (its last connection checkpoints'
.print 'and deletes the -wal) and the main file is republished, immutable=1 reads'
.print 'step 30.'

SELECT o.attempt AS n, o.params, o.result, s.requests, s.stat, s.range, s.bytes
FROM opens o LEFT JOIN request_stats s ON s.part = o.part AND s.attempt = o.attempt
WHERE o.part = 'F'
ORDER BY o.attempt;

SELECT count(*) FILTER (name LIKE '%-wal') AS requests_naming_the_wal_with_immutable,
       any_value(l.wal_deleted_after_close) AS wal_deleted_after_close
FROM requests r, live l
WHERE r.part = 'F' AND r.attempt = 1
ORDER BY 1;
```

Run from the repository root (HTTP route, always runnable; this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
go run ./cmd/course run 7.4
```

Add `-i` (`go run ./cmd/course run -i 7.4`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Run against SeaweedFS S3 (after `docker compose up -d seaweedfs`) directly in the container; it prints only its verdict and leaves its tables in the container's work directory:

```text
docker compose run --rm lab go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3
```

Captured evidence — HTTP route: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/7.4-read-only-snapshot-vfs/`](evidence/7.4-read-only-snapshot-vfs/), and `go run ./cmd/course check evidence ch07` compares a fresh run with them byte for byte.

<!-- include: evidence/7.4-read-only-snapshot-vfs.txt -->
```text

════════════════════════════════════════════════════════════════════════
  7.4  A read-only snapshot VFS over byte-range reads
════════════════════════════════════════════════════════════════════════

A checkpointed recipe database is published as one object and queried
through modernc.org/sqlite/vfs backed by internal/rangefs, which turns
each open into one stat request and each read into one byte-range
request, and records every request.
┌───────┬───────────────────────────────────────────────────────────────────────────────┐
│ route │                                    server                                     │
├───────┼───────────────────────────────────────────────────────────────────────────────┤
│ http  │ an in-process HTTP server (net/http FileServer, which answers Range requests) │
└───────┴───────────────────────────────────────────────────────────────────────────────┘
┌──────────────┬─────────────────────────┐
│ stat_request │      read_request       │
├──────────────┼─────────────────────────┤
│ HEAD         │ GET with a Range header │
└──────────────┴─────────────────────────┘

── A. The snapshot ─────────────────────────────────────────────────────

A1. The recipe database after all its steps, closed by its last
connection, which checkpointed every commit into the main file:
┌──────┬───────┬──────────────┬─────────┬───────┬──────────────────┬───────────────────────┐
│ seed │ steps │ journal_mode │  bytes  │ pages │  sha256_prefix   │      file_state       │
├──────┼───────┼──────────────┼─────────┼───────┼──────────────────┼───────────────────────┤
│ 1    │ 30    │ wal          │ 1105920 │ 270   │ 24aa158fdf713808 │ step 30 (exact bytes) │
└──────┴───────┴──────────────┴─────────┴───────┴──────────────────┴───────────────────────┘

A2. Header bytes 18-19 (file format write and read version) still say 2,
WAL, although no -wal file is left:
┌────────────────┬────────────────┬─────────────┐
│ header_byte_18 │ header_byte_19 │ wal_present │
├────────────────┼────────────────┼─────────────┤
│ 2              │ 2              │ false       │
└────────────────┴────────────────┴─────────────┘

── B. Three queries over the remote snapshot (immutable=1) ─────────────

B1. Each query runs on a new connection: open and schema load, the query
with a cold cache, then the same query again.
┌───────┬──────────────────┬──────────────────────────────────────────────────────────────────────┐
│ query │       name       │                                 sql                                  │
├───────┼──────────────────┼──────────────────────────────────────────────────────────────────────┤
│ 1     │ point lookup     │ SELECT id, account_id, kind, amount, note FROM events WHERE id = 372 │
│ 2     │ index range scan │ SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id       │
│ 3     │ full scan        │ SELECT count(*), sum(amount), sum(length(note)) FROM events          │
└───────┴──────────────────┴──────────────────────────────────────────────────────────────────────┘
┌───────┬──────────────────┬────────────────────────────────────────────────────────────┐
│ query │       name       │                            plan                            │
├───────┼──────────────────┼────────────────────────────────────────────────────────────┤
│ 1     │ point lookup     │ SEARCH events USING INTEGER PRIMARY KEY (rowid=?)          │
│ 2     │ index range scan │ SEARCH events USING INDEX events_by_account (account_id=?) │
│ 3     │ full scan        │ SCAN events                                                │
└───────┴──────────────────┴────────────────────────────────────────────────────────────┘

B2. The remote result equals the result from the local file (digest =
sha256 prefix of all rows):
┌───────┬──────────────────┬────────────┬─────────────┬──────────────────┬──────────────────┐
│ query │       name       │ local_rows │ remote_rows │   local_digest   │  remote_digest   │
├───────┼──────────────────┼────────────┼─────────────┼──────────────────┼──────────────────┤
│ 1     │ point lookup     │ 1          │ 1           │ 564912d877435e1b │ 564912d877435e1b │
│ 2     │ index range scan │ 29         │ 29          │ d98fa27230232e1c │ d98fa27230232e1c │
│ 3     │ full scan        │ 1          │ 1           │ 6b2a4af3714ab7f4 │ 6b2a4af3714ab7f4 │
└───────┴──────────────────┴────────────┴─────────────┴──────────────────┴──────────────────┘
┌───────┬────────────────┬───────────────────────────────────────┐
│ query │ same_first_row │               first_row               │
├───────┼────────────────┼───────────────────────────────────────┤
│ 1     │ true           │ 372|7|fee|2642|gkusfxyuudbxvuvijtfcnc │
│ 2     │ true           │ 4|2828                                │
│ 3     │ true           │ 1177|-124383|41332                    │
└───────┴────────────────┴───────────────────────────────────────┘

B3. Opening and loading the schema costs the same three requests every
time: a stat, the 100-byte header, and page 1.
┌─────────────────────────────────────────────┬─────────────┐
│                   request                   │ connections │
├─────────────────────────────────────────────┼─────────────┤
│ stat snapshot.db (size 1105920)             │ 3           │
│ range snapshot.db bytes 0-99 (100 bytes)    │ 3           │
│ range snapshot.db bytes 0-4095 (4096 bytes) │ 3           │
└─────────────────────────────────────────────┴─────────────┘

B4. Every read is one range request for one whole 4096-byte page: one
request per page miss, no read-ahead. The repeat on the same connection
makes no request: immutable=1 skips the change-counter check, so the
cached pages stay valid. Pages are listed in the order they were read.
┌───────┬────────┬──────────┬──────┬───────┬───────┬─────────────────────────────────────────────────────────────────┐
│ query │ phase  │ requests │ stat │ range │ bytes │                              pages                              │
├───────┼────────┼──────────┼──────┼───────┼───────┼─────────────────────────────────────────────────────────────────┤
│ 1     │ open   │ 3        │ 1    │ 2     │ 4196  │ hdr 1                                                           │
│ 1     │ query  │ 2        │ 0    │ 2     │ 8192  │ 4 66                                                            │
│ 1     │ repeat │ 0        │ 0    │ 0     │ 0     │                                                                 │
│ 2     │ open   │ 3        │ 1    │ 2     │ 4196  │ hdr 1                                                           │
│ 2     │ query  │ 13       │ 0    │ 13    │ 53248 │ 28 88 4 26 27 39 66 87 108 204 210 219 259                      │
│ 2     │ repeat │ 0        │ 0    │ 0     │ 0     │                                                                 │
│ 3     │ open   │ 3        │ 1    │ 2     │ 4196  │ hdr 1                                                           │
│ 3     │ query  │ 18       │ 0    │ 18    │ 73728 │ 4 26 27 39 59 66 87 108 124 132 157 168 184 204 210 219 243 259 │
│ 3     │ repeat │ 0        │ 0    │ 0     │ 0     │                                                                 │
└───────┴────────┴──────────┴──────┴───────┴───────┴─────────────────────────────────────────────────────────────────┘

B5. The first query on a new connection, in total, and how many of its
page reads the page cache can hold:
┌──────────────────┬──────────┬───────┬────────────┬─────────────┐
│       name       │ requests │ bytes │ pages_read │ cache_pages │
├──────────────────┼──────────┼───────┼────────────┼─────────────┤
│ point lookup     │ 5        │ 12388 │ 4          │ 500         │
│ index range scan │ 16       │ 57444 │ 15         │ 500         │
│ full scan        │ 21       │ 77924 │ 20         │ 500         │
└──────────────────┴──────────┴───────┴────────────┴─────────────┘

── C. immutable=1 keeps cached pages valid, not cached ─────────────────

C1. The full scan again, repeated on one connection with PRAGMA
cache_size=4. The scan reads more pages than the cache holds, so the
repeat reads the evicted pages again; only pages still cached are free.
┌───────────┬─────────────┬─────────────┬──────────┬──────┬───────┬───────┐
│   query   │ cache_pages │ same_result │ requests │ stat │ range │ bytes │
├───────────┼─────────────┼─────────────┼──────────┼──────┼───────┼───────┤
│ full scan │ 4           │ true        │ 17       │ 0    │ 17    │ 69632 │
└───────────┴─────────────┴─────────────┴──────────┴──────┴───────┴───────┘

── D. Why immutable=1: the WAL-format snapshot opened without it ───────

D1. Each open counts the events and reads user_version. Without
immutable=1, SQLite sees the WAL flag in the header, needs shared memory,
which this read-only VFS does not provide, and fails with SQLITE_CANTOPEN.
┌─────────────────┬─────────────────────────────────────────────────────────┬──────────┬──────┬───────┬───────┐
│     params      │                         result                          │ requests │ stat │ range │ bytes │
├─────────────────┼─────────────────────────────────────────────────────────┼──────────┼──────┼───────┼───────┤
│ (no parameters) │ SQLite error code 14: unable to open database file (14) │ 5        │ 3    │ 2     │ 4196  │
│ mode=ro         │ SQLite error code 14: unable to open database file (14) │ 5        │ 3    │ 2     │ 4196  │
│ immutable=1     │ user_version=30 events=1177                             │ 8        │ 1    │ 7     │ 24676 │
└─────────────────┴─────────────────────────────────────────────────────────┴──────────┴──────┴───────┴───────┘

D2. The requests of each open: without immutable=1 SQLite also looks for
a -journal and a -wal.
┌─────────────────┬────────────────────────────────────────────────────┐
│     params      │                      request                       │
├─────────────────┼────────────────────────────────────────────────────┤
│ (no parameters) │ stat snapshot.db (size 1105920)                    │
│ (no parameters) │ range snapshot.db bytes 0-99 (100 bytes)           │
│ (no parameters) │ stat snapshot.db-journal (absent)                  │
│ (no parameters) │ stat snapshot.db-wal (absent)                      │
│ (no parameters) │ range snapshot.db bytes 0-4095 (4096 bytes)        │
│ mode=ro         │ stat snapshot.db (size 1105920)                    │
│ mode=ro         │ range snapshot.db bytes 0-99 (100 bytes)           │
│ mode=ro         │ stat snapshot.db-journal (absent)                  │
│ mode=ro         │ stat snapshot.db-wal (absent)                      │
│ mode=ro         │ range snapshot.db bytes 0-4095 (4096 bytes)        │
│ immutable=1     │ stat snapshot.db (size 1105920)                    │
│ immutable=1     │ range snapshot.db bytes 0-99 (100 bytes)           │
│ immutable=1     │ range snapshot.db bytes 0-4095 (4096 bytes)        │
│ immutable=1     │ range snapshot.db bytes 110592-114687 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 356352-360447 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 360448-364543 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 679936-684031 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 987136-991231 (4096 bytes) │
└─────────────────┴────────────────────────────────────────────────────┘

── E. The same bytes in rollback format ────────────────────────────────

E1. A copy of the snapshot with header bytes 18-19 set to 1 (rollback
format) opens without immutable=1 and reads the same state: the WAL flag
in the header, not the content, is what demands shared memory.
┌──────────────────────┬─────────────────┬─────────────────────────────┬──────────┬──────┬───────┬───────┐
│         file         │     params      │           result            │ requests │ stat │ range │ bytes │
├──────────────────────┼─────────────────┼─────────────────────────────┼──────────┼──────┼───────┼───────┤
│ snapshot-rollback.db │ (no parameters) │ user_version=30 events=1177 │ 16       │ 7    │ 9     │ 24708 │
└──────────────────────┴─────────────────┴─────────────────────────────┴──────────┴──────┴───────┴───────┘

E2. Its requests. Without immutable=1 SQLite checks for a hot journal
(stat of the -journal) whenever it takes a read lock; immutable=1 skips it.
┌─────┬─────────────────────────────────────────────────────────────┐
│ seq │                           request                           │
├─────┼─────────────────────────────────────────────────────────────┤
│ 78  │ stat snapshot-rollback.db (size 1105920)                    │
│ 79  │ range snapshot-rollback.db bytes 0-99 (100 bytes)           │
│ 80  │ stat snapshot-rollback.db-journal (absent)                  │
│ 81  │ stat snapshot-rollback.db-wal (absent)                      │
│ 82  │ range snapshot-rollback.db bytes 0-4095 (4096 bytes)        │
│ 83  │ stat snapshot-rollback.db-journal (absent)                  │
│ 84  │ range snapshot-rollback.db bytes 24-39 (16 bytes)           │
│ 85  │ stat snapshot-rollback.db-wal (absent)                      │
│ 86  │ range snapshot-rollback.db bytes 110592-114687 (4096 bytes) │
│ 87  │ range snapshot-rollback.db bytes 356352-360447 (4096 bytes) │
│ 88  │ range snapshot-rollback.db bytes 360448-364543 (4096 bytes) │
│ 89  │ range snapshot-rollback.db bytes 679936-684031 (4096 bytes) │
│ 90  │ range snapshot-rollback.db bytes 987136-991231 (4096 bytes) │
│ 91  │ stat snapshot-rollback.db-journal (absent)                  │
│ 92  │ range snapshot-rollback.db bytes 24-39 (16 bytes)           │
│ 93  │ stat snapshot-rollback.db-wal (absent)                      │
└─────┴─────────────────────────────────────────────────────────────┘
┌───────────────────────┐
│ journal_stat_requests │
├───────────────────────┤
│ 3                     │
└───────────────────────┘

── F. Why a fully checkpointed file ────────────────────────────────────

F1. A child process builds live.db to step 29, commits step 30 and keeps
its connection open, so step 30 is only in the -wal. A local read through
SQLite sees it. Both files are published as they are on disk.
┌───────────────┬────────────────┬─────────────────────────────┬──────────┬───────────┬────────────┐
│ built_to_step │ committed_step │         local_state         │ db_bytes │ wal_bytes │ wal_frames │
├───────────────┼────────────────┼─────────────────────────────┼──────────┼───────────┼────────────┤
│ 29            │ 30             │ user_version=30 events=1177 │ 1060864  │ 123632    │ 30         │
└───────────────┴────────────────┴─────────────────────────────┴──────────┴───────────┴────────────┘

F2. The remote reads. 1: immutable=1 ignores the -wal and returns the
state of step 29, a stale read with no error. 2: without immutable=1 the
open fails. 3: after the writer closes (its last connection checkpoints
and deletes the -wal) and the main file is republished, immutable=1 reads
step 30.
┌───┬─────────────────┬─────────────────────────────────────────────────────────┬──────────┬──────┬───────┬───────┐
│ n │     params      │                         result                          │ requests │ stat │ range │ bytes │
├───┼─────────────────┼─────────────────────────────────────────────────────────┼──────────┼──────┼───────┼───────┤
│ 1 │ immutable=1     │ user_version=29 events=1130                             │ 8        │ 1    │ 7     │ 24676 │
│ 2 │ (no parameters) │ SQLite error code 14: unable to open database file (14) │ 4        │ 3    │ 1     │ 100   │
│ 3 │ immutable=1     │ user_version=30 events=1177                             │ 8        │ 1    │ 7     │ 24676 │
└───┴─────────────────┴─────────────────────────────────────────────────────────┴──────────┴──────┴───────┴───────┘
┌────────────────────────────────────────┬─────────────────────────┐
│ requests_naming_the_wal_with_immutable │ wal_deleted_after_close │
├────────────────────────────────────────┼─────────────────────────┤
│ 0                                      │ true                    │
└────────────────────────────────────────┴─────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the snapshot is a WAL-format database with every commit checkpointed into the main file and no -wal left
B  ✓  point lookup: the remote result equals the local file's (1 row(s), digest 564912d877435e1b)
B  ✓  point lookup: every read is one range request for one whole 4096-byte page (or the 100-byte header): one request per page miss, no read-ahead
B  ✓  point lookup: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the 4 reads so far fit in the 500-page cache)
B  ✓  index range scan: the remote result equals the local file's (29 row(s), digest d98fa27230232e1c)
B  ✓  index range scan: every read is one range request for one whole 4096-byte page (or the 100-byte header): one request per page miss, no read-ahead
B  ✓  index range scan: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the 15 reads so far fit in the 500-page cache)
B  ✓  full scan: the remote result equals the local file's (1 row(s), digest 6b2a4af3714ab7f4)
B  ✓  full scan: every read is one range request for one whole 4096-byte page (or the 100-byte header): one request per page miss, no read-ahead
B  ✓  full scan: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the 20 reads so far fit in the 500-page cache)
C  ✓  full scan: when the query reads more pages than the cache holds, the repeat reads evicted pages again (17 range requests); only pages still in the cache are free
D  ✓  without immutable=1 ((no parameters)) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)
D  ✓  without immutable=1 (mode=ro) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)
D  ✓  with immutable=1 the WAL-format snapshot opens and reads the same state as the local file
E  ✓  a checkpointed snapshot with header bytes 18-19 rewritten to 1 opens without immutable=1 and reads the same state (the WAL flag in the header, not the content, is what demands shared memory)
E  ✓  without immutable=1 SQLite checks for a hot journal (3 stat request(s) for snapshot-rollback.db-journal), which immutable=1 skips
F  ✓  immutable=1 ignores the -wal: the remote read returns step 29's state while the writer has committed step 30 (a stale read, no error)
F  ✓  with immutable=1 SQLite never asks for the -wal file (0 requests name it)
F  ✓  without immutable=1 the live WAL database cannot be opened through the read-only VFS either (SQLITE_CANTOPEN)
F  ✓  once the commit is checkpointed into the main file, the republished snapshot shows step 30

20 of 20 claims hold

Experiment verdict: READ-ONLY SNAPSHOT VFS OK
```

Captured walkthrough — SeaweedFS S3 route (`-s3`), rendered from the tables of an `-s3` run; it is not checked by `check evidence`:

<!-- include: evidence/7.4-read-only-snapshot-vfs-s3.txt -->
```text

════════════════════════════════════════════════════════════════════════
  7.4  A read-only snapshot VFS over byte-range reads
════════════════════════════════════════════════════════════════════════

A checkpointed recipe database is published as one object and queried
through modernc.org/sqlite/vfs backed by internal/rangefs, which turns
each open into one stat request and each read into one byte-range
request, and records every request.
┌───────┬────────────────────────────────────────────────────────────────────┐
│ route │                               server                               │
├───────┼────────────────────────────────────────────────────────────────────┤
│ s3    │ SeaweedFS from compose.yaml, bucket "course", keys ch07/7.4/<name> │
└───────┴────────────────────────────────────────────────────────────────────┘
┌──────────────┬───────────────────────────────┐
│ stat_request │         read_request          │
├──────────────┼───────────────────────────────┤
│ HeadObject   │ GetObject with a Range header │
└──────────────┴───────────────────────────────┘

── A. The snapshot ─────────────────────────────────────────────────────

A1. The recipe database after all its steps, closed by its last
connection, which checkpointed every commit into the main file:
┌──────┬───────┬──────────────┬─────────┬───────┬──────────────────┬───────────────────────┐
│ seed │ steps │ journal_mode │  bytes  │ pages │  sha256_prefix   │      file_state       │
├──────┼───────┼──────────────┼─────────┼───────┼──────────────────┼───────────────────────┤
│ 1    │ 30    │ wal          │ 1105920 │ 270   │ 24aa158fdf713808 │ step 30 (exact bytes) │
└──────┴───────┴──────────────┴─────────┴───────┴──────────────────┴───────────────────────┘

A2. Header bytes 18-19 (file format write and read version) still say 2,
WAL, although no -wal file is left:
┌────────────────┬────────────────┬─────────────┐
│ header_byte_18 │ header_byte_19 │ wal_present │
├────────────────┼────────────────┼─────────────┤
│ 2              │ 2              │ false       │
└────────────────┴────────────────┴─────────────┘

── B. Three queries over the remote snapshot (immutable=1) ─────────────

B1. Each query runs on a new connection: open and schema load, the query
with a cold cache, then the same query again.
┌───────┬──────────────────┬──────────────────────────────────────────────────────────────────────┐
│ query │       name       │                                 sql                                  │
├───────┼──────────────────┼──────────────────────────────────────────────────────────────────────┤
│ 1     │ point lookup     │ SELECT id, account_id, kind, amount, note FROM events WHERE id = 372 │
│ 2     │ index range scan │ SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id       │
│ 3     │ full scan        │ SELECT count(*), sum(amount), sum(length(note)) FROM events          │
└───────┴──────────────────┴──────────────────────────────────────────────────────────────────────┘
┌───────┬──────────────────┬────────────────────────────────────────────────────────────┐
│ query │       name       │                            plan                            │
├───────┼──────────────────┼────────────────────────────────────────────────────────────┤
│ 1     │ point lookup     │ SEARCH events USING INTEGER PRIMARY KEY (rowid=?)          │
│ 2     │ index range scan │ SEARCH events USING INDEX events_by_account (account_id=?) │
│ 3     │ full scan        │ SCAN events                                                │
└───────┴──────────────────┴────────────────────────────────────────────────────────────┘

B2. The remote result equals the result from the local file (digest =
sha256 prefix of all rows):
┌───────┬──────────────────┬────────────┬─────────────┬──────────────────┬──────────────────┐
│ query │       name       │ local_rows │ remote_rows │   local_digest   │  remote_digest   │
├───────┼──────────────────┼────────────┼─────────────┼──────────────────┼──────────────────┤
│ 1     │ point lookup     │ 1          │ 1           │ 564912d877435e1b │ 564912d877435e1b │
│ 2     │ index range scan │ 29         │ 29          │ d98fa27230232e1c │ d98fa27230232e1c │
│ 3     │ full scan        │ 1          │ 1           │ 6b2a4af3714ab7f4 │ 6b2a4af3714ab7f4 │
└───────┴──────────────────┴────────────┴─────────────┴──────────────────┴──────────────────┘
┌───────┬────────────────┬───────────────────────────────────────┐
│ query │ same_first_row │               first_row               │
├───────┼────────────────┼───────────────────────────────────────┤
│ 1     │ true           │ 372|7|fee|2642|gkusfxyuudbxvuvijtfcnc │
│ 2     │ true           │ 4|2828                                │
│ 3     │ true           │ 1177|-124383|41332                    │
└───────┴────────────────┴───────────────────────────────────────┘

B3. Opening and loading the schema costs the same three requests every
time: a stat, the 100-byte header, and page 1.
┌─────────────────────────────────────────────┬─────────────┐
│                   request                   │ connections │
├─────────────────────────────────────────────┼─────────────┤
│ stat snapshot.db (size 1105920)             │ 3           │
│ range snapshot.db bytes 0-99 (100 bytes)    │ 3           │
│ range snapshot.db bytes 0-4095 (4096 bytes) │ 3           │
└─────────────────────────────────────────────┴─────────────┘

B4. Every read is one range request for one whole 4096-byte page: one
request per page miss, no read-ahead. The repeat on the same connection
makes no request: immutable=1 skips the change-counter check, so the
cached pages stay valid. Pages are listed in the order they were read.
┌───────┬────────┬──────────┬──────┬───────┬───────┬─────────────────────────────────────────────────────────────────┐
│ query │ phase  │ requests │ stat │ range │ bytes │                              pages                              │
├───────┼────────┼──────────┼──────┼───────┼───────┼─────────────────────────────────────────────────────────────────┤
│ 1     │ open   │ 3        │ 1    │ 2     │ 4196  │ hdr 1                                                           │
│ 1     │ query  │ 2        │ 0    │ 2     │ 8192  │ 4 66                                                            │
│ 1     │ repeat │ 0        │ 0    │ 0     │ 0     │                                                                 │
│ 2     │ open   │ 3        │ 1    │ 2     │ 4196  │ hdr 1                                                           │
│ 2     │ query  │ 13       │ 0    │ 13    │ 53248 │ 28 88 4 26 27 39 66 87 108 204 210 219 259                      │
│ 2     │ repeat │ 0        │ 0    │ 0     │ 0     │                                                                 │
│ 3     │ open   │ 3        │ 1    │ 2     │ 4196  │ hdr 1                                                           │
│ 3     │ query  │ 18       │ 0    │ 18    │ 73728 │ 4 26 27 39 59 66 87 108 124 132 157 168 184 204 210 219 243 259 │
│ 3     │ repeat │ 0        │ 0    │ 0     │ 0     │                                                                 │
└───────┴────────┴──────────┴──────┴───────┴───────┴─────────────────────────────────────────────────────────────────┘

B5. The first query on a new connection, in total, and how many of its
page reads the page cache can hold:
┌──────────────────┬──────────┬───────┬────────────┬─────────────┐
│       name       │ requests │ bytes │ pages_read │ cache_pages │
├──────────────────┼──────────┼───────┼────────────┼─────────────┤
│ point lookup     │ 5        │ 12388 │ 4          │ 500         │
│ index range scan │ 16       │ 57444 │ 15         │ 500         │
│ full scan        │ 21       │ 77924 │ 20         │ 500         │
└──────────────────┴──────────┴───────┴────────────┴─────────────┘

── C. immutable=1 keeps cached pages valid, not cached ─────────────────

C1. The full scan again, repeated on one connection with PRAGMA
cache_size=4. The scan reads more pages than the cache holds, so the
repeat reads the evicted pages again; only pages still cached are free.
┌───────────┬─────────────┬─────────────┬──────────┬──────┬───────┬───────┐
│   query   │ cache_pages │ same_result │ requests │ stat │ range │ bytes │
├───────────┼─────────────┼─────────────┼──────────┼──────┼───────┼───────┤
│ full scan │ 4           │ true        │ 17       │ 0    │ 17    │ 69632 │
└───────────┴─────────────┴─────────────┴──────────┴──────┴───────┴───────┘

── D. Why immutable=1: the WAL-format snapshot opened without it ───────

D1. Each open counts the events and reads user_version. Without
immutable=1, SQLite sees the WAL flag in the header, needs shared memory,
which this read-only VFS does not provide, and fails with SQLITE_CANTOPEN.
┌─────────────────┬─────────────────────────────────────────────────────────┬──────────┬──────┬───────┬───────┐
│     params      │                         result                          │ requests │ stat │ range │ bytes │
├─────────────────┼─────────────────────────────────────────────────────────┼──────────┼──────┼───────┼───────┤
│ (no parameters) │ SQLite error code 14: unable to open database file (14) │ 5        │ 3    │ 2     │ 4196  │
│ mode=ro         │ SQLite error code 14: unable to open database file (14) │ 5        │ 3    │ 2     │ 4196  │
│ immutable=1     │ user_version=30 events=1177                             │ 8        │ 1    │ 7     │ 24676 │
└─────────────────┴─────────────────────────────────────────────────────────┴──────────┴──────┴───────┴───────┘

D2. The requests of each open: without immutable=1 SQLite also looks for
a -journal and a -wal.
┌─────────────────┬────────────────────────────────────────────────────┐
│     params      │                      request                       │
├─────────────────┼────────────────────────────────────────────────────┤
│ (no parameters) │ stat snapshot.db (size 1105920)                    │
│ (no parameters) │ range snapshot.db bytes 0-99 (100 bytes)           │
│ (no parameters) │ stat snapshot.db-journal (absent)                  │
│ (no parameters) │ stat snapshot.db-wal (absent)                      │
│ (no parameters) │ range snapshot.db bytes 0-4095 (4096 bytes)        │
│ mode=ro         │ stat snapshot.db (size 1105920)                    │
│ mode=ro         │ range snapshot.db bytes 0-99 (100 bytes)           │
│ mode=ro         │ stat snapshot.db-journal (absent)                  │
│ mode=ro         │ stat snapshot.db-wal (absent)                      │
│ mode=ro         │ range snapshot.db bytes 0-4095 (4096 bytes)        │
│ immutable=1     │ stat snapshot.db (size 1105920)                    │
│ immutable=1     │ range snapshot.db bytes 0-99 (100 bytes)           │
│ immutable=1     │ range snapshot.db bytes 0-4095 (4096 bytes)        │
│ immutable=1     │ range snapshot.db bytes 110592-114687 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 356352-360447 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 360448-364543 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 679936-684031 (4096 bytes) │
│ immutable=1     │ range snapshot.db bytes 987136-991231 (4096 bytes) │
└─────────────────┴────────────────────────────────────────────────────┘

── E. The same bytes in rollback format ────────────────────────────────

E1. A copy of the snapshot with header bytes 18-19 set to 1 (rollback
format) opens without immutable=1 and reads the same state: the WAL flag
in the header, not the content, is what demands shared memory.
┌──────────────────────┬─────────────────┬─────────────────────────────┬──────────┬──────┬───────┬───────┐
│         file         │     params      │           result            │ requests │ stat │ range │ bytes │
├──────────────────────┼─────────────────┼─────────────────────────────┼──────────┼──────┼───────┼───────┤
│ snapshot-rollback.db │ (no parameters) │ user_version=30 events=1177 │ 16       │ 7    │ 9     │ 24708 │
└──────────────────────┴─────────────────┴─────────────────────────────┴──────────┴──────┴───────┴───────┘

E2. Its requests. Without immutable=1 SQLite checks for a hot journal
(stat of the -journal) whenever it takes a read lock; immutable=1 skips it.
┌─────┬─────────────────────────────────────────────────────────────┐
│ seq │                           request                           │
├─────┼─────────────────────────────────────────────────────────────┤
│ 78  │ stat snapshot-rollback.db (size 1105920)                    │
│ 79  │ range snapshot-rollback.db bytes 0-99 (100 bytes)           │
│ 80  │ stat snapshot-rollback.db-journal (absent)                  │
│ 81  │ stat snapshot-rollback.db-wal (absent)                      │
│ 82  │ range snapshot-rollback.db bytes 0-4095 (4096 bytes)        │
│ 83  │ stat snapshot-rollback.db-journal (absent)                  │
│ 84  │ range snapshot-rollback.db bytes 24-39 (16 bytes)           │
│ 85  │ stat snapshot-rollback.db-wal (absent)                      │
│ 86  │ range snapshot-rollback.db bytes 110592-114687 (4096 bytes) │
│ 87  │ range snapshot-rollback.db bytes 356352-360447 (4096 bytes) │
│ 88  │ range snapshot-rollback.db bytes 360448-364543 (4096 bytes) │
│ 89  │ range snapshot-rollback.db bytes 679936-684031 (4096 bytes) │
│ 90  │ range snapshot-rollback.db bytes 987136-991231 (4096 bytes) │
│ 91  │ stat snapshot-rollback.db-journal (absent)                  │
│ 92  │ range snapshot-rollback.db bytes 24-39 (16 bytes)           │
│ 93  │ stat snapshot-rollback.db-wal (absent)                      │
└─────┴─────────────────────────────────────────────────────────────┘
┌───────────────────────┐
│ journal_stat_requests │
├───────────────────────┤
│ 3                     │
└───────────────────────┘

── F. Why a fully checkpointed file ────────────────────────────────────

F1. A child process builds live.db to step 29, commits step 30 and keeps
its connection open, so step 30 is only in the -wal. A local read through
SQLite sees it. Both files are published as they are on disk.
┌───────────────┬────────────────┬─────────────────────────────┬──────────┬───────────┬────────────┐
│ built_to_step │ committed_step │         local_state         │ db_bytes │ wal_bytes │ wal_frames │
├───────────────┼────────────────┼─────────────────────────────┼──────────┼───────────┼────────────┤
│ 29            │ 30             │ user_version=30 events=1177 │ 1060864  │ 123632    │ 30         │
└───────────────┴────────────────┴─────────────────────────────┴──────────┴───────────┴────────────┘

F2. The remote reads. 1: immutable=1 ignores the -wal and returns the
state of step 29, a stale read with no error. 2: without immutable=1 the
open fails. 3: after the writer closes (its last connection checkpoints
and deletes the -wal) and the main file is republished, immutable=1 reads
step 30.
┌───┬─────────────────┬─────────────────────────────────────────────────────────┬──────────┬──────┬───────┬───────┐
│ n │     params      │                         result                          │ requests │ stat │ range │ bytes │
├───┼─────────────────┼─────────────────────────────────────────────────────────┼──────────┼──────┼───────┼───────┤
│ 1 │ immutable=1     │ user_version=29 events=1130                             │ 8        │ 1    │ 7     │ 24676 │
│ 2 │ (no parameters) │ SQLite error code 14: unable to open database file (14) │ 4        │ 3    │ 1     │ 100   │
│ 3 │ immutable=1     │ user_version=30 events=1177                             │ 8        │ 1    │ 7     │ 24676 │
└───┴─────────────────┴─────────────────────────────────────────────────────────┴──────────┴──────┴───────┴───────┘
┌────────────────────────────────────────┬─────────────────────────┐
│ requests_naming_the_wal_with_immutable │ wal_deleted_after_close │
├────────────────────────────────────────┼─────────────────────────┤
│ 0                                      │ true                    │
└────────────────────────────────────────┴─────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the snapshot is a WAL-format database with every commit checkpointed into the main file and no -wal left
B  ✓  point lookup: the remote result equals the local file's (1 row(s), digest 564912d877435e1b)
B  ✓  point lookup: every read is one range request for one whole 4096-byte page (or the 100-byte header): one request per page miss, no read-ahead
B  ✓  point lookup: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the 4 reads so far fit in the 500-page cache)
B  ✓  index range scan: the remote result equals the local file's (29 row(s), digest d98fa27230232e1c)
B  ✓  index range scan: every read is one range request for one whole 4096-byte page (or the 100-byte header): one request per page miss, no read-ahead
B  ✓  index range scan: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the 15 reads so far fit in the 500-page cache)
B  ✓  full scan: the remote result equals the local file's (1 row(s), digest 6b2a4af3714ab7f4)
B  ✓  full scan: every read is one range request for one whole 4096-byte page (or the 100-byte header): one request per page miss, no read-ahead
B  ✓  full scan: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the 20 reads so far fit in the 500-page cache)
C  ✓  full scan: when the query reads more pages than the cache holds, the repeat reads evicted pages again (17 range requests); only pages still in the cache are free
D  ✓  without immutable=1 ((no parameters)) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)
D  ✓  without immutable=1 (mode=ro) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)
D  ✓  with immutable=1 the WAL-format snapshot opens and reads the same state as the local file
E  ✓  a checkpointed snapshot with header bytes 18-19 rewritten to 1 opens without immutable=1 and reads the same state (the WAL flag in the header, not the content, is what demands shared memory)
E  ✓  without immutable=1 SQLite checks for a hot journal (3 stat request(s) for snapshot-rollback.db-journal), which immutable=1 skips
F  ✓  immutable=1 ignores the -wal: the remote read returns step 29's state while the writer has committed step 30 (a stale read, no error)
F  ✓  with immutable=1 SQLite never asks for the -wal file (0 requests name it)
F  ✓  without immutable=1 the live WAL database cannot be opened through the read-only VFS either (SQLITE_CANTOPEN)
F  ✓  once the commit is checkpointed into the main file, the republished snapshot shows step 30

20 of 20 claims hold

Experiment verdict: READ-ONLY SNAPSHOT VFS OK
```

#### Reading the Experiment 7.4 evidence

Section labels below refer to the HTTP evidence (`evidence/7.4-read-only-snapshot-vfs.txt`); the `-s3` walkthrough has the same sections.

- **A1–A2 (the snapshot).** Builds the 30-step WAL recipe (`seed=1`) and closes its last connection, leaving a single file at rest: `1,105,920` bytes (`270` pages), header bytes 18–19 = `2 2` (WAL format), no `-wal` file on disk, matching step 30 byte for byte (`sha256` prefix `24aa158fdf713808`). Before publishing, the experiment deletes all object names it uses so leftover objects cannot affect a run.
- **B1–B5 (three queries on `file:snapshot.db?vfs=<name>&immutable=1`):** Every query runs on a fresh connection, warms only the schema (`SELECT count(*) FROM sqlite_schema`), runs the query cold, and repeats the query on the same connection:

| Query | Query plan | Rows | Open + schema | Cold query (range reads) | Pages read in order | Repeat | First-query total |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Point lookup (`WHERE id = 372`) | `SEARCH events USING INTEGER PRIMARY KEY (rowid=?)` | 1 | 3 req (`1` stat, `2` range), `4,196` B | 2 req, `8,192` B | `4 66` | 0 | **5 req, `12,388` B** |
| Index range scan (`WHERE account_id = 7 ORDER BY id`) | `SEARCH events USING INDEX events_by_account (account_id=?)` | 29 | 3 req (`1` stat, `2` range), `4,196` B | 13 req, `53,248` B | `28 88 4 26 27 39 66 87 108 204 210 219 259` | 0 | **16 req, `57,444` B** |
| Full scan (`count(*), sum(amount), sum(length(note))`) | `SCAN events` | 1 | 3 req (`1` stat, `2` range), `4,196` B | 18 req, `73,728` B | `4 26 27 39 59 66 87 108 124 132 157 168 184 204 210 219 243 259` | 0 | **21 req, `77,924` B** |

  - Open + schema load is always exactly 3 requests: `stat snapshot.db (size 1105920)`, `range bytes 0-99 (100 bytes)` (header), and `range bytes 0-4095 (4096 bytes)` (page 1, `sqlite_schema`).
  - Every cold query reads only the B-tree pages its plan visits: page `4` is `events`'s root page, pages `28` and `88` are `events_by_account` index pages, and all 18 pages of `events` represent just 6.7% of the 270-page database (`documents` and its **overflow pages**—linked pages holding large row payloads—are never fetched). Every read is a single 4,096-byte page request—one round trip per page miss, zero read-ahead, zero stats during the query.
  - Repeating any of these three queries on the same connection makes **0 requests** because `immutable=1` makes the pager act like a temp file ([`src/pager.c:5033-5056`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5033-L5056): no locking, pretend `EXCLUSIVE`), so the change-counter check that normally invalidates the cache ([`src/pager.c:5436`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5436)) never runs, **and** the 4, 15, and 20 reads so far all fit inside SQLite's default `-2000` KiB (`500`-page) cache ([`src/sqliteLimit.h:161`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqliteLimit.h#L161)).
  - Every remote digest matches the local file (`564912d877435e1b`, `d98fa27230232e1c`, `6b2a4af3714ab7f4`). Comparing the two walkthroughs confirms HTTP and S3 produce identical requests, bytes, and digests.
- **C1 (`PRAGMA cache_size=4`: `immutable=1` keeps cached pages valid, not cached).** Repeating the 18-page `events` full scan on one `immutable=1` connection after shrinking the page cache to `4` pages issues **17 range requests (`69,632` bytes)** on the repeat: `immutable=1` prevents change-counter invalidation, but it does not prevent **LRU** (least-recently-used) cache eviction when a query scans more pages than `cache_size` holds. Only pages still resident in the cache are free.
- **D1–D2 (why `immutable=1` is required).** Opening the WAL-format snapshot with no URI parameters or with `mode=ro` fails with `SQLite error code 14: unable to open database file (14)` after 5 requests (`stat snapshot.db`, 100-byte header, `stat snapshot.db-journal (absent)`, `stat snapshot.db-wal (absent)`, page 1). SQLite reads header bytes 18–19 = `2`, tries to enter WAL mode, and aborts because `modernc.org/sqlite/vfs` registers an `io_methods` version-1 table with no `xShm*` methods. Adding `immutable=1` succeeds in 8 requests (`1` stat, `7` range, `24,676` bytes), never probing for `-journal` or `-wal`, and matches the local file (`user_version=30 events=1177`).
- **E1–E2 (flipping bytes 18–19 from `2` to `1`).** Publishing the same bytes with header bytes 18–19 rewritten to `1 1` (`snapshot-rollback.db`) lets SQLite open it with no URI parameters and read `user_version=30 events=1177`. That proves the WAL header flag—not the page content—demands shared memory. It also exposes the hidden cost of omitting `immutable=1`: the two statements take **16 requests** (`7` stat, `9` range, `24,708` bytes) instead of 8 because each read transaction probes `snapshot-rollback.db-journal` (3 times total) and `snapshot-rollback.db-wal` and re-reads the 16-byte change counter (`bytes 24-39`). Rewriting bytes 18–19 also changes the file's SHA-256 (breaking byte-for-byte **E1** equality), so production snapshot serving should keep the original bytes and pass `immutable=1`.
- **F1–F2 (why a fully checkpointed, truly immutable object is required).** A child process builds `live.db` to step 29, commits step 30, and holds its connection open so step 30 stays in `live.db-wal` (`123,632` bytes, `30` frames). Running the writer in a child process avoids the POSIX lock hazard: the parent's `os.ReadFile` calls open and close `live.db` in a separate process without dropping the child's locks. A local SQLite read sees `user_version=30 events=1177`. The parent publishes both `live.db` (`1,060,864` bytes) and `live.db-wal` to the store. Querying `live.db` remotely with `immutable=1` returns **`user_version=29 events=1130`—a silent stale read with no error**—in 8 requests, 0 of which mention `live.db-wal`. Querying without `immutable=1` fails with code `14`. Only after the child closes (checkpointing step 30 into `live.db` and unlinking `-wal`), `live.db` is republished, and a **fresh connection** opens it does the remote query return `user_version=30 events=1177`. Never overwrite a served object in place while an `immutable=1` connection stays open: as [`src/sqlite.h.in:3922-3929`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L3922-L3929) warns, an open connection keeps cached pages from the old snapshot while fetching uncached pages from the new file, failing with `SQLITE_CORRUPT` (`11`) and invalid page numbers (adversarial test `SNAP-04`). Publish each snapshot under a new immutable key—or replace a key only when no connection has it open—and open a new connection to read it.

---

## 6. Evidence and failure cases

### 6.1 What a custom VFS must honor

Every rule below is grounded in the pinned `os_unix.c`/`os.c` spans and verified by Experiments 7.1–7.4:

1. **Offset-addressed reads and writes, short reads, and disk-full errors.** `xRead` and `xWrite` take explicit 64-bit byte offsets (`pread64`/`pwrite64`, [`src/os_unix.c:3463-3721`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3463-L3721)). On a short read, `xRead` must zero-fill the unread tail of the buffer and return `SQLITE_IOERR_SHORT_READ` ([`src/os_unix.c:3575-3577`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3575-L3577)); SQLite depends on this when probing an empty file or the end of a journal (Experiments 7.1 and 7.3). On a short write, `xWrite` must retry or return `SQLITE_FULL` (for `ENOSPC`) or `SQLITE_IOERR_WRITE` ([`src/os_unix.c:3710-3717`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3710-L3717)).
2. **Durability contract of `xSync` and directory syncs.** `xSync` must not return `SQLITE_OK` until file data and size are flushed to durable storage (`full_fsync`, [`src/os_unix.c:3778-3849`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3778-L3849)), and the first `xSync` of a newly created `-journal` or `-wal` must also sync its parent directory ([`src/os_unix.c:3941-3953`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L3941-L3953), [`6569-6572`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6569-L6572), [`6777`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6777); verified in Experiment 7.1). SQLite relies on sync ordering: journal synced before database pages are written, and WAL header synced before frames after a restart ([`src/wal.c:4114-4124`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4114-L4124)). A VFS that fakes `xSync` breaks atomic commit on power loss.
3. **Process crash versus power cut.** `fsync` does not protect against a process crash—the operating system's page cache already preserves every completed `pwrite64` after a `SIGKILL`. That is why a process killed under `synchronous=OFF` or WAL/`NORMAL` still recovers committed transactions on reopen. A **power cut** discards everything in the OS page cache that has not been flushed by `fsync`. In WAL mode at `synchronous=NORMAL`, ordinary commits perform 0 syncs (Experiment 7.2), so a power cut can lose every transaction committed since the last checkpoint or WAL restart while leaving the database structurally consistent; at `synchronous=FULL`, every commit performs 1 WAL sync (`wal:1`), so acknowledged commits survive. Chapter 9 (Experiment 9.2) demonstrates this difference directly.
4. **File locks and WAL shared memory.** `xLock`/`xUnlock`/`xCheckReservedLock` enforce SQLite's five lock levels ([`src/os.h:98-102`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.h#L98-L102), [`160-166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.h#L160-L166); `unixLock`, [`src/os_unix.c:1866-2095`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L1866-L2095)). For WAL mode, the VFS must also provide `io_methods` version ≥ 2 (`xShmMap`, `xShmLock`, `xShmBarrier`, `xShmUnmap`, [`src/os_unix.c:4952-5546`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4952-L5546)) with 32 KiB shared regions, 8 lock slots (`UNIX_SHM_BASE = 120`), and a dead-man switch (`128`) visible to every process opening the database. A storage backend without POSIX-grade shared memory and advisory locks cannot host live multi-process WAL databases.
5. **Open, access, and delete semantics.** `xAccess(SQLITE_ACCESS_EXISTS)` must treat a zero-byte regular file as absent ([`src/os_unix.c:6896-6898`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6896-L6898)). `xDelete` must return `SQLITE_IOERR_DELETE_NOENT` if the target is missing and sync the directory when `dirSync=1` ([`src/os_unix.c:6833-6869`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L6833-L6869)). `xOpen` receives masked file-role flags ([`src/os.c:215-232`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L215-L232)) and must leave `pFile->pMethods == 0` on any error ([`src/os.c:230`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L230)).
6. **Device characteristics.** `os_unix.c` reports `0x9000` (`POWERSAFE_OVERWRITE | SUBPAGE_READ`) and a 4,096-byte sector size ([`src/os_unix.c:4353-4375`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os_unix.c#L4353-L4375)). Advertising flags the storage does not actually provide is dangerous: `SQLITE_IOCAP_SEQUENTIAL` disables journal and WAL-header syncs ([`src/pager.c:4332-4441`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4332-L4441), [`src/wal.c:4114-4124`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L4114-L4124)), and `SQLITE_IOCAP_SAFE_APPEND` skips the journal header rewrite and first journal sync.
7. **Error contract.** Returning a non-OK error code from `xWrite` or `xSync` aborts the active transaction and restores the exact pre-transaction database state (Experiment 7.3 Part B: `122` of `122` fired faults across WAL and `DELETE` rolled back to step 3 byte for byte).

### 6.2 Failure cases demonstrated in this chapter

- **Misreading `vfstrace`'s `FULL` sync label (Section 3.4, Experiment 7.3 Part A).** [`ext/misc/vfstrace.c:453`](https://github.com/sqlite/sqlite/blob/version-3.53.4/ext/misc/vfstrace.c#L453) tests `flags & SQLITE_SYNC_FULL` (`0x03`), which also matches `SQLITE_SYNC_NORMAL` (`0x02`). Experiment 7.3 proves SQLite passes `0x2 (NORMAL)` in all four default configurations (`delete`/`wal` × `NORMAL`/`FULL`); `SQLITE_SYNC_FULL` (`0x03`) is sent only when `PRAGMA fullfsync=ON`.
- **Silent WAL refusal or `SQLITE_CANTOPEN` on a version-1 VFS (Section 1, Experiment 7.4).** A VFS with `io_methods.iVersion = 1` silently rejects `PRAGMA journal_mode=wal` (returning `delete`) on creation and fails with error `14` when opening an existing WAL-format snapshot unless `immutable=1` is passed.
- **Silent stale reads from an uncheckpointed snapshot (Experiment 7.4).** Publishing `live.db` and `live.db-wal` while a writer holds step 30 in `-wal` and querying with `immutable=1` returns step 29 (`user_version=29 events=1130`) with no error and zero requests for `-wal`.
- **Cache eviction under `immutable=1` (Experiment 7.4).** `immutable=1` disables change-counter invalidation ([`src/pager.c:5033-5056`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5033-L5056), [`5436`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5436)), not LRU eviction. Repeating the 18-page `events` full scan with `PRAGMA cache_size=4` issues `17` range requests (`69,632` bytes).
- **Overwriting a served snapshot in place behind an open `immutable=1` connection (`SNAP-04`).** Replacing step 29's object with step 30's bytes under an open `immutable=1` connection mixes cached step-29 pages (including page 1, `user_version=29`) with uncached step-30 pages and fails with `SQLITE_CORRUPT` (`11`, `database disk image is malformed`) and invalid page numbers in `PRAGMA integrity_check` ([`src/sqlite.h.in:3922-3929`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqlite.h.in#L3922-L3929)).
- **Silent ABI mismatch in a translated-library shim (Section 5.2, `GOVFS-08`).** Changing a parameter width (`int32` → `int64`) in a `govfs` slot compiles and passes runtime tests on `amd64` without crashing; only AST comparison against `modernc.org/sqlite/lib` (`TestSignaturesMatchTranslatedSource`) catches it.

### What attacked this chapter

<!-- adversarial-results -->

Four adversarial programs (`34` attacks total, driven by [`run-all.sh`](../agent_work/ch07/adversarial/run-all.sh)) tested this chapter's claims against the pinned SQLite 3.53.4 build:

- [`vfs-syscall-map`](../agent_work/ch07/adversarial/vfs-syscall-map/main.go) (`MAP-01`–`MAP-08`, including `MAP-03a`/`MAP-03b`) attacked Experiment 7.1's VFS-to-syscall claims: Go-vs-CLI syscall parity on 7.1's script, `SIGKILL` WAL recovery, real `xSync` flags vs `-vfstrace` labels with `fullfsync=OFF` and `ON`, `PRAGMA mmap_size` reads via `xFetch`, `xAccess(EXISTS)` on 0-byte vs 512-byte journals, `synchronous=EXTRA` directory syncs, a must-be-able-to-fail `xWrite` offset mutation, and 7.1's non-Linux `portable()` check against real `-vfstrace` output.
- [`io-cost-falsify`](../agent_work/ch07/adversarial/io-cost-falsify/main.go) (`COST-01`–`COST-06`) attacked Experiment 7.2's I/O cost model: recomputing all 12 cells with the C `sqlite3` CLI under `strace`, `synchronous=OFF` across all 6 cells, 20-run determinism checks, `wal_autocheckpoint=0` vs `100`, rollback `delete/EXTRA` + `persist/NORMAL` + `truncate/FULL`, and a pinned reader blocking WAL checkpoints past `wal_autocheckpoint`.
- [`govfs-adversarial`](../agent_work/ch07/adversarial/govfs-adversarial/main.go) (`GOVFS-01`–`GOVFS-09`) attacked `internal/govfs` and Experiment 7.3: a must-be-able-to-fail Go-vs-CLI mismatch check, harder workloads (10 KB overflow inserts, explicit `ROLLBACK`, `wal_checkpoint(TRUNCATE)`), exhaustive `xRead`/`xTruncate` and `synchronous=NORMAL` fault sweeps, `SQLITE_FULL` (`13`) / `SQLITE_IOERR_FSYNC` (`1034`) and same-connection retry, `SQLITE_FCNTL_VFSNAME` shim stack identity, `mmap_size`/temp-table/subjournal/v2/8-goroutine stress, a must-be-able-to-fail `TestSignaturesMatchTranslatedSource` ABI mutation, and `SetRecording(false)` + interoperability with `modernc.org/sqlite/vfs.New`.
- [`snapshot-rangefs`](../agent_work/ch07/adversarial/snapshot-rangefs/main.go) (`SNAP-01`–`SNAP-10`) attacked `internal/rangefs` and Experiment 7.4: write-refusal tests, overflow-blob table scans and `integrity_check`, repeat-query caching under `PRAGMA cache_size=4`, in-place snapshot overwrites behind an open `immutable=1` connection, hostile HTTP and hostile S3 servers (`200 OK` ignoring `Range`, truncated `206`, mid-query `500`, missing `Content-Length`), `rangefs` edge cases (0-byte `ReadAt`, failed `ReadRange` byte accounting, `Seek`/`Stat` after `Close`, `New(nil, ...)`), 512 B and 16 KiB page sizes + truncated snapshots, HTTP-vs-SeaweedFS S3 request-for-request parity, and 8-connection concurrency.

**Run 1** ([`results-run1.txt`](../agent_work/ch07/adversarial/results-run1.txt)) produced **26 HELD, 8 BROKEN**. Four broken attacks exposed real bugs that were fixed in code:

- `MAP-08`: Experiment 7.1's non-Linux `portable()` check matched `"delete=1"` instead of `vfstrace.c:843`'s `"delFlag=1"`, which would have failed unconditionally on macOS. Fixed by extracting `closeLifecycle(calls)` matching `delFlag=1` in `c.Args`, with unit tests in `7.1/main_test.go`.
- `GOVFS-06`: `govfs` forwarded `SQLITE_FCNTL_VFSNAME` unmodified, returning `"unix"` instead of `"<shim>/unix"` per `sqlite.h.in:1008-1018` and `vfstrace.c:711-716`. Fixed by adding `prependVFSName` in `internal/govfs/methods.go` and `internal/govfs/vfsname_test.go`.
- `SNAP-06`: `rangefs.S3Source.ReadRange` did not verify `Content-Range`, allowing a hostile S3 endpoint returning `200 OK` with the full object to serve offset-0 bytes silently. Fixed by adding `Content-Range` prefix verification in `internal/rangefs/s3.go` and unit tests in `internal/rangefs/edge_test.go`.
- `SNAP-07`: `rangefs` logged a bogus `bytes 0--1` request on zero-length `ReadAt`, counted requested bytes on failed/short `ReadRange`, allowed `Seek`/`Stat` after `Close`, and did not default `nil` contexts in `New`. All four were fixed in `internal/rangefs/rangefs.go` (`Request.Failed`, `fs.ErrClosed`, `context.Background()`) and covered by `internal/rangefs/edge_test.go`.

Four broken attacks identified claim-scope boundaries that were narrowed in the chapter prose (and, for `SNAP-03`, demonstrated in Experiment 7.4):

- `MAP-04` showed that with `PRAGMA mmap_size > 0`, data pages arrive via `xFetch` (`18` calls) and `unixRead` copies mapped ranges via `memcpy` with `0` `pread64` syscalls. Section 3.2 and Experiment 7.1 now scope the 1-to-1 `xRead` → `pread64` mapping to SQLite's default `mmap_size=0`.
- `COST-05` refuted the assumption that `PERSIST` and `TRUNCATE` eliminate directory syncs on Unix: both eliminate `unlink`, but `pager_unlock` (`src/pager.c:1891-1895`) still closes `-journal` (Unix lacks `SQLITE_IOCAP_UNDELETABLE_WHEN_OPEN`), re-arming `UNIXFILE_DIRSYNC` on every commit, and journal finalization adds a journal sync (`persist/NORMAL`: `4` syncs/commit; `truncate/FULL`: `5` syncs/commit; `delete/EXTRA`: `5` syncs/commit with `2` dir syncs). Sections 3.3, 5.1, and 7 now state this Unix behavior.
- `SNAP-03` showed that `immutable=1` prevents change-counter invalidation (`src/pager.c:5033-5056,5436`), not LRU cache eviction: repeating the 18-page `events` scan with `PRAGMA cache_size=4` issued `17` range requests (`69,632` bytes). Experiment 7.4 now checks that each query's reads fit in the 500-page default cache and adds a `cache_size=4` section demonstrating the `17`-request repeat directly.
- `SNAP-04` demonstrated `src/sqlite.h.in:3922-3929`'s warning in practice: overwriting a served snapshot in place (step 29 → step 30) while an `immutable=1` connection stays open mixes cached page 1 (`user_version=29`) with step-30 pages and fails with `SQLITE_CORRUPT` (`11`) and invalid page numbers. Sections 5.5, 6.2, and 7 now require publishing each snapshot under a new immutable key and opening a fresh connection to read it.

After the fixes and updating `MAP-04`, `MAP-08`, `COST-05`, `SNAP-03`, and `SNAP-04` to assert the repaired code and narrowed claims, the final run of [`run-all.sh`](../agent_work/ch07/adversarial/run-all.sh) passed **34 HELD, 0 BROKEN** ([`results.txt`](../agent_work/ch07/adversarial/results.txt)).

---

## 7. Platform implications

Every operational rule below is tied directly to the experiment that supports it:

1. **WAL mode cuts syncs and write amplification by orders of magnitude on a shared disk (Experiments 7.1, 7.2, 7.3).** **Write amplification** is the ratio of bytes written to disk versus logical row bytes inserted. For 1,000 single-row commits, rollback `DELETE` mode costs `3,000` syncs and `17,038,688` bytes at `NORMAL` (`4,000` syncs at `FULL`), while WAL mode costs `3` syncs and `4,461,320` bytes at `NORMAL` (`1,003` syncs at `FULL`). On one machine holding thousands of databases, rollback mode turns small commits into a wall of `fsync` and directory-sync calls.
2. **Batching writes is the largest I/O lever under rollback mode and WAL/`FULL` (Experiment 7.2).** Grouping 1,000 rows into 1 transaction instead of 1,000 cuts rollback `FULL` syncs by `1,000×` (`4,000` → `4`) and written bytes by `129×` (`17,038,688` → `131,612`). In WAL `FULL`, batching cuts syncs from `1,003` to `1` and written bytes from `4,461,320` to `123,600`.
3. **Budget for transient sidecar files and directory syncs (Experiments 7.1, 7.2).** Rollback `DELETE` mode creates and unlinks `-journal` and opens/syncs the parent directory on every write transaction (`1,000` journal creates, directory opens, and unlinks per 1,000 commits in Experiment 7.2); switching rollback mode to `PERSIST` or `TRUNCATE` eliminates the `unlink` on Unix but still pays the directory sync on every commit (plus an extra journal sync). WAL mode creates `-wal` (with one directory sync) and `-shm` once at first write and keeps both until the last connection closes. Directory watchers, filesystem file-entry (inode) quotas, and backup scanners must expect these sidecars.
4. **Closing the last WAL connection triggers a real checkpoint; do not open and close per request (Experiments 7.1, 7.2).** Closing the final WAL connection in Experiment 7.2 wrote `30` pages (`122,880` bytes) and issued `2` syncs (`db:1 wal:1`) before unlinking `-shm` and `-wal`, and the next writer must pay the WAL-creation directory sync all over again. A multi-database service should pool open connections (Chapter 8's bounded connection manager) rather than churning them per request.
5. **Keep live WAL databases on local disk; serve immutable snapshots from object storage only after checkpointing, under immutable keys (Experiments 7.1, 7.4).** Live WAL databases depend on POSIX byte-range locks (`fcntl` on `.db` and `-shm`) and shared `mmap` memory that S3 and network filesystems cannot safely provide across machines. For read-only queries against historical snapshots, serving a checkpointed at-rest file (Rebuild Ladder rung R5) through `modernc.org/sqlite/vfs` + `internal/rangefs` with `immutable=1` answers a point lookup in `5` requests (`12,388` bytes out of `1,105,920`) and `0` requests on repeat queries while the working set fits in `cache_size`—at the cost of one network round trip per page miss and re-fetches on cache eviction. Always checkpoint (or close the last connection) before uploading and publish each snapshot under a new immutable key rather than overwriting an object behind an open `immutable=1` connection.
6. **Build a custom VFS only for the three jobs local disk cannot do (Experiments 7.3, 7.4).** A platform has three clear reasons to install a custom VFS: (a) portable in-process VFS observability without `strace` (Experiment 7.3 Part A); (b) deterministic I/O fault injection with the process alive—verifying that the service treats every `SQLITE_IOERR` as a rolled-back commit (Experiment 7.3 Part B, `122/122` exact rollbacks); and (c) on-demand page-level reads of immutable snapshots in HTTP or S3 object storage (Experiment 7.4). For live read-write databases on local disk, the stock `unix` VFS plus WAL shipping (Chapter 9) avoids VFS maintenance risk.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 7.1 and Experiment 7.2, why does a rollback `DELETE`-mode commit at `synchronous=FULL` perform **3** `xSync` VFS calls (Experiment 7.3) yet **4** `fsync`/`fdatasync` syscalls (Experiment 7.2), and why does the extra sync happen only once per journal creation?
2. Experiment 7.2 ran 1,000 single-row commits in WAL mode with `synchronous=NORMAL` and `wal_autocheckpoint=1000`. Exactly how many commits performed 0 syncs, how many performed 2 syncs, and how many performed 1 sync—and which SQLite functions caused those two non-zero sync patterns?
3. Why did the CLI's `-vfstrace` print `FULL` on every `xSync` in Experiment 7.1, whereas `internal/govfs` in Experiment 7.3 Part A recorded `0x2 (NORMAL)` for every `xSync` even when `PRAGMA synchronous=FULL` was set?
4. In Experiment 7.3 Part B (failing every `xWrite` and `xSync` of recipe step 4), why did all 53 WAL faults surface at `COMMIT` while 40 of the 69 `DELETE`-mode faults surfaced during earlier statements—and why did failing WAL `xSync #2` (after every WAL frame was written) still leave exact step-3 bytes after reopen?
5. In Experiment 7.4, what happens when you open a checkpointed WAL-format snapshot through `modernc.org/sqlite/vfs` without `immutable=1`, why does flipping header bytes 18–19 from `2` to `1` let it open (and at what request-count penalty), and what goes wrong if you open an uncheckpointed database with `immutable=1` while its `-wal` is published beside it?

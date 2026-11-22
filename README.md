# SQLite Replication and Backup Strategies

*Deterministic Capture, WAL Shipping, and Fleet Recovery through SQLite Internals*

An experiment-driven book on capturing, shipping, backing up, and deterministically rebuilding fleets of SQLite databases—grounded in how SQLite `3.53.4` stores, locks, caches, commits, and recovers data. Written for engineers building platforms that hold thousands of SQLite database files, each of the ten chapters pairs annotated SQLite C source code with runnable Go experiments inlined directly in the text and checked against stored evidence files.

The published book is at [https://nickstrad.github.io/walfleet/](https://nickstrad.github.io/walfleet/). The GitHub Actions workflow [.github/workflows/pages.yml](.github/workflows/pages.yml) builds and verifies the static site on every push to `main` by running `go run ./cmd/course site`.

## The through-line: the Rebuild Ladder

Every chapter serves one operational goal: capture a database's durable state deterministically, rebuild a working database from that capture, and prove that the rebuild equals the original. That progression is the **Rebuild Ladder** (rungs **R0** through **R7**).

Every proof names one of three equality levels implemented in [internal/equal/](internal/equal/):

- **E1 bytes:** identical SHA-256 hash of the entire database file.
- **E2 pages:** identical fixed-size database **pages** (4,096-byte blocks), ignoring the declared page-1 header fields rewritten by SQLite's online backup API (bytes 24–27, 40–43, and 92–99).
- **E3 logical:** identical schema, ordered rows with explicit rowids and storage classes, `sqlite_sequence`, and persistent pragmas, plus `PRAGMA integrity_check` = `ok`.

Chapters 4 and 7 add no rung of their own; they measure the commit atomicity, page-cache memory, and filesystem flush (**fsync**) costs that the surrounding rungs rely on.

## Chapters and rungs

1. [Chapter 1 — Architecture and execution path](ch01-architecture-and-execution-path/ch01-architecture-and-execution-path.md) (**Rung R0 — Recipe**): Traces one committed `INSERT` across SQLite's bytecode engine, B-tree (balanced tree of fixed-size pages), pager (page cache and transaction manager), **VFS** (Virtual File System, the layer that translates SQLite file calls to OS syscalls), and kernel writes and syncs, and builds a seeded Go workload whose closed database has one exact SHA-256 hash after every step.
2. [Chapter 2 — Database file format and pages](ch02-file-format-and-pages/ch02-file-format-and-pages.md) (**Rung R1 — Pages**): Parses the 100-byte database header, every B-tree page, overflow chain, and freelist page in plain Go without SQLite, then rebuilds the closed file from its page images and manifest to E1 bytes.
3. [Chapter 3 — B-trees and page management](ch03-btrees-and-page-management/ch03-btrees-and-page-management.md) (**Rung R2 — Canonical form**): Measures how many pages an insert or split dirties, proves that freed pages keep stale bytes unless `PRAGMA secure_delete=ON`, and proves that different write histories produce equal E3 logical hashes and E1-identical files after `VACUUM INTO`.
4. [Chapter 4 — Pager and page cache](ch04-pager-and-page-cache/ch04-pager-and-page-cache.md): Proves rollback-journal commit ordering and crash recovery by killing a writer at each write and sync, and bounds per-connection page-cache memory with `PRAGMA cache_size`.
5. [Chapter 5 — WAL, transactions, and checkpoints](ch05-wal-transactions-checkpoints/ch05-wal-transactions-checkpoints.md) (**Rung R3 — Redo log**): Decodes the **WAL** (write-ahead log, the sibling `-wal` redo log of 24-byte headers plus page images called **frames**) and `-shm` index, proves that a held reader pins WAL growth and blocks a **checkpoint** (copying committed frames back into the main database file), and replays committed frames in plain Go to match SQLite's own checkpoint to E1 bytes.
6. [Chapter 6 — Locking and concurrent access](ch06-locking-and-concurrency/ch06-locking-and-concurrency.md) (**Rung R4 — Live snapshot**): Demonstrates **POSIX advisory locks** (process-scoped OS byte-range locks dropped by any stray `close` on the same file), maps every `SQLITE_BUSY` rule across processes, shows naive live file copies tear, and captures a consistent live WAL database to E1 bytes through a single read transaction.
7. [Chapter 7 — VFS and filesystem I/O](ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md): Pairs every VFS method with its OS syscall, counts exact write and `fsync` costs across journal modes and `PRAGMA synchronous` settings, wraps the Go driver in a fault-injecting forwarding VFS, and queries a read-only snapshot over HTTP and S3 byte ranges.
8. [Chapter 8 — Connection and database lifecycle](ch08-connection-and-database-lifecycle/ch08-connection-and-database-lifecycle.md) (**Rung R5 — At-rest state**): Measures lazy schema loading, separates file-persistent settings from per-connection settings, counts file descriptors and memory per idle database, and runs a bounded LRU connection manager that verifies each WAL database at rest after its last connection closes.
9. [Chapter 9 — Backup, crash recovery, and integrity](ch09-backup-crash-recovery-integrity/ch09-backup-crash-recovery-integrity.md) (**Rung R6 — Point in time**): Compares five live capture methods, separates process crashes from power cuts under each `synchronous` level, shows why `PRAGMA integrity_check` misses modified unindexed row bytes, and continuously ships WAL segments across checkpoints to restore any committed step to E1 bytes.
10. [Chapter 10 — Multi-database platform capstone](ch10-multi-database-platform-capstone/ch10-multi-database-platform-capstone.md) (**Rung R7 — Fleet**): Runs a multi-process Go service backed by **SeaweedFS** (an S3-compatible object store) and proves single-writer fencing, survival of acknowledged writes across crashes, safe interrupted relocation, and a full fleet rebuild onto empty disks to E1 bytes.

## Pinned SQLite version

All 34 experiments and all copied C source files are pinned to one SQLite release recorded in [sqlite/PIN](sqlite/PIN):

| Field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver (`go.mod`) | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Local SQLite source tree | the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`) |

Every experiment prints `sqlite_source_id()` on its first line and exits non-zero if the build does not match the pin.

## Running the experiments

Prerequisites:

- **Docker with Docker Compose** ([compose.yaml](compose.yaml)). Every experiment runs in the `lab` container ([Dockerfile.lab](Dockerfile.lab)): Linux, Go 1.26, `strace`, the pinned SQLite `3.53.4` source tree, the pre-built C tools (`sqlite3`, `showwal`, `dbhash`), and the DuckDB CLI `2.0.0` that presents the results. Chapter 10 (and Experiment 7.4's optional `-s3` flag) also uses the local S3-compatible store (`SeaweedFS 4.48`) from the same file.
- **Go `1.26.0` or newer** ([go.mod](go.mod) declares `go 1.26.0`) to start the runner from the host. No C compiler is needed for the Go driver (`modernc.org/sqlite` is pure Go).

Build the container image once, from the repository root:

```text
docker compose build lab
```

Run an experiment by its number:

```text
go run ./cmd/course run 3.1
```

- The command starts a temporary `lab` container with this repository mounted at `/src`, runs the experiment there, and prints a walkthrough of its results: short explanations, each followed by a table.
- An experiment writes its measurements as CSV tables and prints only a verdict line. The walkthrough is the DuckDB script `report.sql` next to the experiment's `main.go`.
- Add `-i` (`go run ./cmd/course run -i 3.1`) to stay in the DuckDB shell afterwards, with every table loaded, and run your own queries.
- Without Go on the host, run the same command in the container: `docker compose run --rm lab go run ./cmd/course run 3.1`.

For Chapter 10 (and Experiment 7.4 with `-s3`), start the local SeaweedFS container first:

```text
docker compose up -d seaweedfs
go run ./cmd/course run 10.1
```

- `docker compose up -d seaweedfs` starts SeaweedFS in the background and publishes its S3 API on host port `18333` (overridable with `COURSE_S3_HOST_PORT`). Inside `lab`, `COURSE_S3_ENDPOINT` is preconfigured to reach it.

### Running an experiment without the container

Every package in this repository compiles and passes `go vet` on Linux and macOS, and an experiment is an ordinary Go program:

```text
go run ./ch03-btrees-and-page-management/experiments/3.1-pages-per-insert
```

- Run this way, the experiment prints its verdict and leaves its tables under `_work/<experiment>/results/` (git-ignored and safe to delete); nothing presents them.
- Experiments that cross-check against the C tools need them on the host: `go run ./cmd/course fetch-source` clones SQLite `version-3.53.4` into `_src/sqlite-3.53.4` and verifies it, and `go run ./cmd/course build-tools` compiles `sqlite3`, `showwal`, and `dbhash` into `_tools/bin/`.
- Experiments that trace syscalls with `strace` (`1.2`, `4.1`, `7.1`, `7.2`, `8.1`, `9.2`, `10.2` Part A) or inspect `/proc` (`4.2` Part E, `6.1` `/proc/locks`, `8.2` descriptor target names) are build-tag gated: on macOS they print a `LINUX-ONLY:` notice and skip the Linux-specific section. The container runs them in full.

## Repository layout

- **Chapter evidence:** Each chapter directory stores, for each experiment, the result tables under `evidence/<experiment>/` (CSV) and the walkthrough as the `lab` container rendered it in `evidence/<experiment>.txt`, for example [ch01-architecture-and-execution-path/evidence/](ch01-architecture-and-execution-path/evidence/) through [ch10-multi-database-platform-capstone/evidence/](ch10-multi-database-platform-capstone/evidence/).
- **Knowledge-check answers:** Each chapter ends with five review questions whose answers live in a separate `answers.md` file inside the chapter directory, for example [ch01-architecture-and-execution-path/answers.md](ch01-architecture-and-execution-path/answers.md) through [ch10-multi-database-platform-capstone/answers.md](ch10-multi-database-platform-capstone/answers.md) (rendered as their own pages on the site).
- **Course outline and specification:** [OUTLINE.md](OUTLINE.md) records the chapter plan, technical decisions, and build rulings; [docs/course-source.md](docs/course-source.md) holds the original course specification.
- **Research reports and chapter findings:** [docs/knowledge/](docs/knowledge/) holds the source inventories, feasibility probes, and per-chapter verified findings.
- **Working and adversarial scripts:** [agent_work/INDEX.md](agent_work/INDEX.md) indexes every probe, build note, and adversarial falsification script under `agent_work/`.
- **Course CLI and checks:** [cmd/course/](cmd/course/) builds the site (`go run ./cmd/course site -check -base /walfleet/`), runs experiments (`run`), synchronizes inlined code (`inline`), and checks copied sources, line spans, formatting, evidence, and paths (`check all`).

# SQLite Replication and Backup Strategies

*Deterministic Capture, WAL Shipping, and Fleet Recovery through SQLite Internals*

This experiment-driven book explains how to capture, ship, back up, and deterministically rebuild fleets of SQLite databases, grounded in how SQLite `3.53.4` stores, locks, caches, commits, and recovers data. It is written for engineers building platforms that hold thousands of SQLite database files. Each of the ten chapters pairs annotated SQLite C source code with runnable Go experiments whose code is inlined in the text and checked against stored CSV and text evidence files.

Read the published book at [https://nickstrad.github.io/walfleet/](https://nickstrad.github.io/walfleet/). On every push to `main`, the GitHub Actions workflow [.github/workflows/pages.yml](.github/workflows/pages.yml) builds and verifies the static site using the repository CLI (`go run ./cmd/course site`).

## The through-line: the Rebuild Ladder

Every chapter serves one operational goal: capture a database's durable state deterministically, rebuild a working database from that capture, and prove that the rebuild equals the original. Across the book, these techniques climb from a single reproducible database file up to a full multi-database fleet—the **Rebuild Ladder** (rungs **R0** through **R7**).

Every proof names one of three equality levels implemented in the Go verification package [internal/equal/](internal/equal/):

- **E1 bytes:** identical SHA-256 hash of the entire database file.
- **E2 pages:** identical fixed-size database **pages** (4,096-byte blocks), ignoring the declared page-1 header fields rewritten by SQLite's online backup API (bytes 24–27, 40–43, and 92–99: the file change counter, schema cookie, and version fields).
- **E3 logical:** identical schema, ordered rows with explicit rowids and storage classes, `sqlite_sequence`, and persistent pragmas, plus `PRAGMA integrity_check` = `ok`.

Chapters 4 and 7 add no rung of their own; they measure the commit atomicity, page-cache memory, and filesystem flush (**fsync**) costs that the surrounding rungs rely on.

## Chapters and rungs

1. [Chapter 1 — Architecture and execution path](ch01-architecture-and-execution-path/ch01-architecture-and-execution-path.md) (**Rung R0 — Recipe**): Traces one committed `INSERT` from SQLite's bytecode engine through the B-tree (balanced tree of fixed-size pages), pager (page cache and transaction manager), and **VFS** (Virtual File System, the layer that translates SQLite file calls to OS syscalls) down to kernel writes and syncs. Builds a seeded Go workload whose closed database has one exact SHA-256 hash after every step.
2. [Chapter 2 — Database file format and pages](ch02-file-format-and-pages/ch02-file-format-and-pages.md) (**Rung R1 — Pages**): Parses the 100-byte database header, every B-tree page, overflow chain, and freelist page in plain Go without SQLite. Rebuilds the closed file from its page images and manifest to E1 bytes.
3. [Chapter 3 — B-trees and page management](ch03-btrees-and-page-management/ch03-btrees-and-page-management.md) (**Rung R2 — Canonical form**): Measures how many pages an insert or split dirties and proves that freed pages keep stale bytes unless `PRAGMA secure_delete=ON`. Proves that different write histories produce equal E3 logical hashes and E1-identical files after `VACUUM INTO`.
4. [Chapter 4 — Pager and page cache](ch04-pager-and-page-cache/ch04-pager-and-page-cache.md): Proves rollback-journal commit ordering and crash recovery by killing a writer at each write and sync. Bounds per-connection page-cache memory with `PRAGMA cache_size`.
5. [Chapter 5 — WAL, transactions, and checkpoints](ch05-wal-transactions-checkpoints/ch05-wal-transactions-checkpoints.md) (**Rung R3 — Redo log**): Decodes the **WAL** (write-ahead log, the sibling `-wal` redo log of 24-byte headers plus page images called **frames**) and `-shm` shared-memory index. Proves that a held reader pins WAL growth and blocks a **checkpoint** (copying committed frames back into the main database file), then replays committed frames in plain Go to match SQLite's own checkpoint to E1 bytes.
6. [Chapter 6 — Locking and concurrent access](ch06-locking-and-concurrency/ch06-locking-and-concurrency.md) (**Rung R4 — Live snapshot**): Demonstrates **POSIX advisory locks** (process-scoped OS byte-range locks dropped by any stray `close` on the same file) and maps every `SQLITE_BUSY` rule across processes. Shows that naive live file copies tear, then captures a consistent live WAL database to E1 bytes through a single read transaction.
7. [Chapter 7 — VFS and filesystem I/O](ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md): Pairs every VFS method with its OS syscall and counts exact write and `fsync` costs across journal modes and `PRAGMA synchronous` settings. Wraps the Go driver in a fault-injecting forwarding VFS and queries a read-only snapshot over HTTP and S3 byte ranges.
8. [Chapter 8 — Connection and database lifecycle](ch08-connection-and-database-lifecycle/ch08-connection-and-database-lifecycle.md) (**Rung R5 — At-rest state**): Measures lazy schema loading, separates file-persistent settings from per-connection settings, and counts file descriptors and memory per idle database. Runs a bounded LRU connection manager that verifies each WAL database at rest after its last connection closes.
9. [Chapter 9 — Backup, crash recovery, and integrity](ch09-backup-crash-recovery-integrity/ch09-backup-crash-recovery-integrity.md) (**Rung R6 — Point in time**): Compares five live capture methods, separates process crashes from power cuts under each `synchronous` level, and shows why `PRAGMA integrity_check` misses modified unindexed row bytes. Continuously ships WAL segments across checkpoints to restore any committed step to E1 bytes.
10. [Chapter 10 — Multi-database platform capstone](ch10-multi-database-platform-capstone/ch10-multi-database-platform-capstone.md) (**Rung R7 — Fleet**): Runs a multi-process Go service backed by **SeaweedFS** (an S3-compatible object store). Proves single-writer fencing, survival of acknowledged writes across crashes, safe interrupted relocation, and a full fleet rebuild onto empty disks to E1 bytes.

## Pinned SQLite version

All 34 experiments and the C source files copied into each chapter directory are pinned to one SQLite release recorded in [sqlite/PIN](sqlite/PIN):

| Field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver (`go.mod`) | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Local SQLite source tree | `$SQLITE_SRC` (pre-installed in the container) or `_src/sqlite-3.53.4` (cloned on the host) |

Every experiment prints `sqlite_source_id()` on its first line and exits non-zero if the SQLite build does not match the pin.

## Running the experiments

Prerequisites:

- **Docker with Docker Compose** ([compose.yaml](compose.yaml)): provides the `lab` container ([Dockerfile.lab](Dockerfile.lab)) where every experiment runs, bundling Linux, Go 1.26, `strace`, the pinned SQLite `3.53.4` source tree, the pre-built SQLite C tools (`sqlite3`, `showwal`, `dbhash`), and the DuckDB CLI `2.0.0` used to render results. Chapter 10 and Experiment 7.4 (when run with its optional `-s3` flag to query a read-only snapshot VFS) also use the local S3-compatible store (`SeaweedFS 4.48`) defined in the same Compose file.
- **Go `1.26.0` or newer** ([go.mod](go.mod) declares `go 1.26.0`): runs the course CLI from the host. No C compiler is needed because the Go driver (`modernc.org/sqlite`) is pure Go.

Build the container image once from the repository root:

```text
make docker-build
```

Run an experiment by its chapter and experiment number (for example, the first experiment in Chapter 3):

```text
make run EXP=3.1
```

- The command starts a temporary `lab` container with this repository mounted at `/src`, runs the experiment there, and prints a walkthrough of its results: short explanations, each followed by a table.
- Each experiment program writes its measurements as CSV tables and prints only a verdict line; the runner then executes the DuckDB script `report.sql` next to `main.go` to render the walkthrough.
- Add `-i` (`make run EXP=3.1 ARGS='-i'`) to stay in the DuckDB shell afterwards, with every table loaded, and run your own queries.
- Without Go on the host, run the same command inside the container: `make docker-run EXP=3.1`.

For Chapter 10 (and Experiment 7.4 with `-s3`), start the local SeaweedFS container first:

```text
make docker-up
make run EXP=10.1
```

`make docker-up` starts SeaweedFS in the background and publishes its S3 API on host port `18333` (overridable with `COURSE_S3_HOST_PORT`). Inside the `lab` container, `COURSE_S3_ENDPOINT` is preconfigured to reach SeaweedFS.

### Running an experiment without the container

Every package in this repository compiles and passes `go vet` on Linux and macOS. Because each experiment is an ordinary Go program, you can run it directly on the host:

```text
make run-native EXP=3.1
```

- Run this way, the experiment prints its verdict and leaves its CSV tables under `_work/<experiment>/results/` (git-ignored and safe to delete) without running the DuckDB walkthrough.
- Experiments that cross-check against SQLite's C tools require those tools on the host. Run `make fetch-source` to clone and verify SQLite `version-3.53.4` into `_src/sqlite-3.53.4`, then run `make build-tools` to compile `sqlite3`, `showwal`, and `dbhash` into `_tools/bin/`.
- Experiments that trace syscalls with `strace` (`1.2`, `4.1`, `7.1`, `7.2`, `8.1`, `9.2`, `10.2` Part A) or inspect `/proc` (`4.2` Part E, `6.1` `/proc/locks`, `8.2` open file-descriptor target names) are gated by Go build tags: on macOS they print a `LINUX-ONLY:` notice and skip the Linux-specific section. The container runs them in full.

## Repository layout

- **Chapter evidence:** Each chapter directory stores every experiment's CSV result tables under `evidence/<experiment>/` and the DuckDB walkthrough rendered by the `lab` container in `evidence/<experiment>.txt`, spanning [ch01-architecture-and-execution-path/evidence/](ch01-architecture-and-execution-path/evidence/) through [ch10-multi-database-platform-capstone/evidence/](ch10-multi-database-platform-capstone/evidence/).
- **Knowledge-check answers:** Each chapter ends with five review questions whose answers live in a separate `answers.md` file inside the chapter directory, from [ch01-architecture-and-execution-path/answers.md](ch01-architecture-and-execution-path/answers.md) through [ch10-multi-database-platform-capstone/answers.md](ch10-multi-database-platform-capstone/answers.md), rendered as their own pages on the static site.
- **Course outline and specification:** [OUTLINE.md](OUTLINE.md) records the chapter plan, technical decisions (the SQLite pin, pure-Go driver, and SeaweedFS store), and project constraints; [docs/course-source.md](docs/course-source.md) holds the original course specification.
- **Research reports and chapter findings:** [docs/knowledge/](docs/knowledge/) holds the SQLite C source inventories (mapping functions and structs to file and line spans), technical feasibility probes, and per-chapter verified findings.
- **Working and adversarial scripts:** [agent_work/INDEX.md](agent_work/INDEX.md) indexes every probe, build note, and adversarial falsification script (edge-case tests that try to break each chapter's claims) under `agent_work/`.
- **Course CLI and checks:** [cmd/course/](cmd/course/) is the repository Go tool that builds and verifies the static site (`make site ARGS='-check -base /walfleet/'`), runs experiments (`run`), synchronizes Go code snippets inlined in the chapter Markdown (`inline`), and checks copied C sources, line spans, Go formatting, saved evidence, and Markdown paths (`check all`).

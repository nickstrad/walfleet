# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments. North Star (Nick, 2026-11-20): understand SQLite
deterministically, well enough to build platforms holding thousands of SQLite files that
can be remade as needed; state capture and deterministic rebuild is the through-line.
Plan: OUTLINE.md (revision 2). What changed from revision 1: OUTLINE-CHANGES.md.

Status: **Phase 2 (build) COMPLETE** (2026-11-21). Chunk 0, chapters 1–10 and the publishing
chunk have landed on `main` and are pushed. The book is published at
https://nickstrad.github.io/walfleet/ (built and deployed by
`.github/workflows/pages.yml`; the first deploy of the full book ran green on 2026-11-21 and the
home page, chapter, answers and outline pages answer 200).

Rulings applied (details: OUTLINE.md section 8):
- Upstream github.com/nickstrad/walfleet (public); every chunk lands on main and is pushed.
- Docker for services; SeaweedFS is the capstone's S3 store. Shared `internal/` packages.
- Cross-platform: must build and run on macOS; Linux-only tooling gated, `lab` Docker service
  as the Mac route; darwin cross-build + vet verify every chunk.
- No second SQLite version (future note: a version-upgrade experiment could be added later).
- Knowledge in two stores kept in sync: the build machine's `kb` store, and this repo (`docs/knowledge/` indexed by
  the vendored `kb/`, location `.kb/`). `agent_work/kb-sync.sh`.
- Go for all code and scripts; working scripts in `agent_work/` (Go or bash), indexed.
- Driver modernc.org/sqlite v1.60.1, no cgo/C. Rollback experiment 4.1 at full depth.
- WAL is the platform default; per-chapter evaluation:
  - Chapter 3: WAL holds. All experiment databases are WAL except 3.1 part D and 3.3 part A2
    (rollback mode shown as the contrast). WAL frames serve as the independent record of which
    pages a write changed.
  - Chapter 4: 4.1 is rollback mode by subject (the planned exception, ruling 9). The platform
    stays WAL; 4.1 shows the two places a WAL platform still meets rollback mode: files created
    or imported without the WAL switch, and the switch commit itself. 4.2 runs in WAL.
  - Chapter 5: WAL holds as the default. What it obliges a platform to own (each tied to an
    experiment in the chapter): run checkpoints itself, bound reader lifetime, treat `-wal` as
    part of the database while any connection is open, never ship `-shm`, watch WAL size,
    detect WAL generations by salt change. Exception from the source (not tested): no WAL on
    network filesystems (shared memory needed).
  - Chapter 6: WAL holds and the evidence favours it for concurrency; rollback mode appears
    only as the contrast (6.1 A, 6.2 D, 6.3 B). Caveat recorded in the chapter:
    `locking_mode=EXCLUSIVE` breaks "the writer never blocks readers".
  - Chapter 7: WAL holds. Care points: a custom VFS must implement the shared-memory methods
    (io_methods version 2) or `journal_mode=wal` silently stays `delete`.
  - Chapter 9: WAL holds; no experiment runs in rollback mode. WAL allows live capture
    without stopping the writer (9.1) and segment shipping (9.4). Recorded conditions: at
    `synchronous=NORMAL` a power cut loses the commits since the last checkpoint (FULL loses
    none); `VACUUM INTO` of a WAL database yields a rollback-mode file.
  - Chapter 8: WAL holds, with measured costs and conditions: 3 descriptors per idle connection
    instead of 1, and a closed WAL database is NOT a single file at rest in five cases (another
    process has it open, a statement left open, a killed writer, persist-WAL, a rename while
    open) — the chapter's `CheckAtRest` is the rule before any copy.
  - Chapter 10: WAL holds and is required by the capstone design, not only its default: WAL
    segment shipping reads committed frames from `-wal`, and the shared catalog database runs
    in WAL so several node processes read it while one writes. No experiment runs in rollback
    mode; no exception.
- VFS viability verdict: **VIABLE** (probe `agent_work/probes/vfs-viability/RESULT.md`, chunk 0): a
  writable Go forwarding VFS registers with modernc in pure Go; read-only `fs.FS` VFS also works.
  Chapter 7 gains experiments 7.3 (Go VFS shim: call counts + fault injection) and 7.4
  (read-only snapshot VFS over S3 range reads). Risk noted: relies on ccgo's function-pointer
  convention; that package needs `go vet -unsafeptr=false`.
- S3 client for the capstone: aws-sdk-go-v2 (probe `agent_work/probes/seaweedfs-s3/RESULT.md`).
- Seats: Opus writes code only. **Neon makes ALL prose — the chapter text and the Go code
  comments** — and reviews Opus's code and writes the adversarial tests. Fable orchestrates,
  runs the adversarial tests, accepts. Astra not used.
- Pipeline (Nick, 2026-11-20 prose-ordering and closing-rewrite notes). Every chapter's FINAL
  step before landing is Neon's unreviewed closing prose; no chapter lands without it.
  - Late ordering (chapters 3, 5, 6, 8, 9, 10a, 10b): Opus build → Neon review + Neon
    adversarial → Opus repair → Neon re-review → **Neon prose once, at the end (chapter text +
    comments on the finished code)** → accept → land. Scripts: `agent_work/orch/stageA.sh`
    then `stageB.sh … late`.
  - Old ordering (chapters 1, 2, 7, and 4 whose prose ran before review): they close with a
    separate **Neon closing rewrite** (`stageB.sh … closing`). Chapters 1 and 2 landed without
    it and get it retroactively as follow-up commits on main.
- Docker fallback (Nick, 2026-11-20): where no generic approach works on both systems, the
  experiment runs in the Docker lab and the chapter presents Linux behaviour first. Uses: (none
  recorded yet; chapter 7's macOS delFlag mismatch got a generic fix in its repair).
- Open questions 7, 12, 13: outline recommendations applied (real source copies checked against
  the pin; five questions per chapter with answers in a separate file, no tutor packaging;
  failure-cases sections link the adversarial scripts).
- Addendum: final publishing chunk (GitHub Pages via Actions,
  https://nickstrad.github.io/walfleet/); self-contained rule (no build-machine
  references in anything a reader sees; sweep before publishing lands).

Done:
- Phase 1 and 1b: research reports, probes, OUTLINE.md revision 2 — a7bb640.
- Phase 2 setup: kb vendored under `kb/` (upstream SHA in `agent_work/kb-vendor.sha`), in-repo
  kb location `.kb/`, orchestration and Neon scripts (`agent_work/orch`, `agent_work/neon`),
  rulings recorded in OUTLINE.md section 8.

Chunk status. Pipeline per chunk: Opus build → Neon review + Neon adversarial → one Opus
repair → Neon re-review → Neon closing prose → acceptance gate → merge and push. Chunks 0, 1, 2,
4 and 7 ran the older order (prose before review) and closed with a Neon closing rewrite.

| Chunk | State | Review | Adversarial | Landed |
| --- | --- | --- | --- | --- |
| 0 foundation | done | Neon FAIL → repair → PASS | 38 held / 0 broken | merged cf58cb8 |
| 1 chapter 1 | done | Neon FAIL → one Opus repair → PASS | 48 held / 0 broken after repair (run 1: 11 broken; claims narrowed) | merged 2dcf743 |
| 2 chapter 2 | done | Neon FAIL → one Opus repair → PASS | 36 held / 0 broken after repair (run 1: 14 broken) | merged 68afd03 |
| 3 chapter 3 | done | Neon NITS → one Opus repair → PASS | 30 held / 0 broken after repair (run 1: 14 broken) | merged 4e1e290 |
| 4 chapter 4 | done | Neon FAIL → one Opus repair → NITS | 32 held / 0 broken after repair (run 1: 12 broken) | merged f500664 |
| 5 chapter 5 | done | Neon FAIL → one Opus repair → PASS | 28 held / 0 broken after repair (run 1: 7 broken) | merged 2629fec |
| 6 chapter 6 | done | Neon FAIL → one Opus repair → PASS | 49 held / 0 broken after repair (run 1: 6 broken) | merged e4af9e0 |
| 7 chapter 7 | done | Neon FAIL → one Opus repair → PASS | 34 held / 0 broken after repair | merged 751e198 |
| 8 chapter 8 | done | Neon FAIL → one Opus repair → PASS | 29 held / 0 broken after repair (run 1: 9 broken) | merged 3f54929 |
| 9 chapter 9 | done | Neon FAIL → one Opus repair → NITS | 40 held / 0 broken after repair (run 1: 5 broken) | merged a64e85e |
| 10a capstone service core | done | Neon FAIL → one Opus repair → PASS | 27 held / 0 broken after repair (run 1: 9 broken) | merged 3b2ef92 |
| 10b chapter 10 and relocation | done | Neon FAIL → one Opus repair → PASS | 25 held / 0 broken after repair (run 1: 2 broken) | merged 11681f8 |
| pub publishing (GitHub Pages site) | done | Neon FAIL → repair → FAIL → arbitration (third family: FAIL stands) → second repair → NITS | 48 held / 0 broken (one attack aligned to the final home page) | merged 6f4292c |

Chapters 1 and 2 also received their retroactive Neon closing rewrites (66fca8e, dd5a9b4, merged).
A separate small fix (`equal.LogicalHash` always ends its read transaction) is merged, with a
Neon review PASS (`agent_work/fixeq/`).

Open chunks: none.

Closing prose record (commit of each chapter's closing Neon pass): chapter 5 — 394203d (single late prose pass); chapter 7 — c645979 (closing rewrite); chapter 3 — e71fa8c (single late prose pass); chapter 4 — a9fff51 (closing rewrite); chapter 6 — d394e64, chapter 8 — dfd1dfd and chapter 9 — b58a1a0 (single late prose passes); 10a — 09037a8 (comments only; no chapter text in that chunk); chapter 1 — 66fca8e and chapter 2 — dd5a9b4 (retroactive closing rewrites, merged on main).

Closing prose for the last two chunks: 10b — 37aa704 (single late prose pass: the whole of
chapter 10 and the comments); publishing chunk — 04a697d (README as the book's home page, comments).

Follow-up (2026-11-22, Nick): experiment results as tables, read in the terminal with DuckDB.
- Every experiment (all 34) writes its measurements as CSV tables (`internal/results`) and prints
  only a verdict. A DuckDB script `report.sql` next to each `main.go` presents the tables as a
  walkthrough. Convention and rules: `docs/knowledge/experiment-reports.md`; exemplar: 3.1.
- Command: `go run ./cmd/course run [-i] [-update] N.M`. It runs in the `lab` container (DuckDB
  CLI 2.0.0 pinned by sha256 in `Dockerfile.lab`); started on the host it re-runs itself with
  `docker compose run --rm lab`. Docker is now the route for running experiments.
- Evidence: `chNN/evidence/N.M-slug/*.csv` (checked byte for byte by `check evidence`) and
  `chNN/evidence/N.M-slug.txt` (the rendered walkthrough, checked when DuckDB 2.0.0 is
  installed). Run-dependent values (timings, memory, pids) are in `info_*` tables, shown by an
  optional `info.sql` and left out of the evidence. All evidence was recaptured in the lab
  container on linux/arm64; the checked values equal the earlier linux/amd64 capture.
- Seats for this follow-up differ from Phase 2: Fable did the shared code and 3.1, Sonnet agents
  converted chapters 1, 2, 3, 5, Opus agents converted 4, 6, 7, 8, 9, 10, and one Opus agent
  aligned the chapter prose. The Phase 2 gate `check prose` (every prose line by the Neon
  author) therefore does not hold for the lines this follow-up changed. The adversarial scripts
  under `agent_work/<chunk>/adversarial/` were not re-run or updated; those that read an
  experiment's stdout need updating before they are used again.
- Two variant evidence files are rendered walkthroughs that no tool checks:
  `ch07…/evidence/7.4-read-only-snapshot-vfs-s3.txt` and
  `ch10…/evidence/10.4-rebuild-the-fleet-2000.txt` (`course run` cannot pass experiment flags).

Next steps: none required. Possible later work: a version-upgrade experiment against a second
SQLite release (dropped from this build by ruling); syntax highlighting on the site; tightening
the link checker's end-tag search inside `script`/`style` bodies (review nit, no effect on the
generated pages).

Open decisions / blockers: none.

Resume or extend: read this file, then OUTLINE.md (section 8 first) and `git log`. Each chunk's
working record (build notes, reviews, repair reports, adversarial suites and their results) is
under `agent_work/<chunk>/`, indexed in `agent_work/INDEX.md`. The checks that gate a change:
`docker compose run --rm lab go run ./cmd/course check all`, `bash agent_work/verify/crossbuild.sh`,
`go run ./cmd/course site -check -base /walfleet/`, and the chunk's
`agent_work/<chunk>/adversarial/run-all.sh`. The orchestration scripts in `agent_work/orch/`
record how the build machine ran the chunks; they hold paths of that machine and are not needed
to read or run the book.

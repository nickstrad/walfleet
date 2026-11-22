---
title: "Experiment results as CSV tables with DuckDB walkthroughs"
summary: "How a walfleet experiment writes its measurements to CSV tables, how report.sql presents them in the terminal, and how the tables became the checked evidence"
tags: [walfleet, experiments, duckdb, evidence, csv]
updated: 2026-11-22
verified: 2026-11-22 — lab container (Debian, linux/arm64), Go 1.26, DuckDB CLI 2.0.0; exemplar is Experiment 3.1
---

# Experiment results as CSV tables with DuckDB walkthroughs

How a `walfleet` experiment writes its measurements to CSV tables, how `report.sql` presents them in the terminal, and how the tables became the checked evidence.

The exemplar is Experiment 3.1: [`main.go`](../../ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/main.go), [`report.sql`](../../ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/report.sql), the stored tables in [`evidence/3.1-pages-per-insert/`](../../ch03-btrees-and-page-management/evidence/3.1-pages-per-insert/) and the rendered walkthrough [`evidence/3.1-pages-per-insert.txt`](../../ch03-btrees-and-page-management/evidence/3.1-pages-per-insert.txt).

## The parts

| Part | Where | What it does |
| --- | --- | --- |
| Tables | `internal/results` | The experiment writes each measurement as a row of a CSV table in `<work dir>/results/`. |
| Walkthrough | `experiments/N.M-slug/report.sql` | A DuckDB script: `.print` lines say what to look at, a query shows it. |
| Run-dependent values | `experiments/N.M-slug/info.sql` (optional) | Same form, for tables whose name starts with `info`. Shown by `course run`, not stored. |
| Runner | `go run ./cmd/course run [-i] [-update] N.M` | Runs the experiment in the `lab` container, loads every CSV as a table, reads the report. |
| Evidence | `chNN/evidence/N.M-slug/*.csv` and `chNN/evidence/N.M-slug.txt` | The stored tables, and the walkthrough as DuckDB rendered it. |
| Check | `go run ./cmd/course check evidence [chNN]` | Runs the experiment again and compares its tables byte for byte, then the rendered walkthrough. |

DuckDB is pinned at 2.0.0 in `Dockerfile.lab`. The runner started on the host re-runs itself with `docker compose run --rm lab`, so the host needs Docker and Go, not DuckDB. `check evidence` on a host without DuckDB 2.0.0 still compares the tables and says that it did not render the report.

## What the experiment prints

Only this goes to stdout:

- the pin line from `sqlitex.MustPinCheck…`;
- `INFO:` lines (machine-local paths and the like);
- skip notices that the checks read, such as `LINUX-ONLY:` and `NO-DBHASH:`;
- `FATAL …` when the experiment cannot continue;
- the verdict, printed by `set.Finish("NAME")`: `NAME OK`, or `NAME FAILED: n claim(s) did not hold` with the failed claims and exit status 1.

Everything else is a table row, or fixed text in `report.sql`.

## The Go side

```go
dir := sqlitex.MustWorkDir("3.1")           // the work directory name is the experiment number
set := results.MustOpen(dir)                // <work dir>/results/
inserts := set.Table("inserts", "part", "insert_no", "pages_changed")
inserts.Row("A", 1, 1)                      // nil is written empty and read as NULL
set.Check("A", ok, "the claim, the same text on every run")
set.Finish("PAGES PER INSERT")              // closes the tables, prints the verdict
```

Rules:

- One row per observation, one fact per column. Column and table names are lower-case SQL identifiers. Give a table a `seq` column when its row order matters, and a `part` column when the experiment has parts.
- Let SQL do the counting. Write the raw rows (every insert, every call, every page) and compute histograms, totals and comparisons in `report.sql`. Keep in Go only what needs the live process: the measurement and the claims.
- A list in one cell is a space-separated string. Long text goes in a `note` or `how` column.
- Every byte of a table must be the same on every run and on every machine. Timings, process ids, addresses, paths and anything else that varies go in a table whose name starts with `info`; the evidence check skips those tables.
- `set.Check` writes the standard `checks` table (`seq`, `part`, `status`, `claim`). The runner lists it at the end of every walkthrough, so `report.sql` does not.
- Only one process opens the `Set`. A child process that the experiment starts reports to its parent as before.
- Keep the stored tables of one experiment under about 1 MB.

## The report

`report.sql` is read after the runner has set `.mode box`, set `.nullvalue ''`, and created one table per CSV. The skeleton:

```sql
.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  N.M  The question the experiment answers'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Two or three sentences: what was done and the settings.'

.print
.print '── A. First part ───────────────────────────────────────────────────────'
.print
.print 'A1. What to look at in the next table.'

SELECT ... ORDER BY ...;
```

Rules:

- Always single-quote the text of `.print`: without quotes DuckDB collapses spaces. For a line that holds an apostrophe, use double quotes instead (`.print "the writer's lock"`): DuckDB 2.0.0 does not turn `''` back into an apostrophe.
- One sentence or two, then one query. Say what the reader should notice, not what the columns are.
- Every query has an `ORDER BY`, and no query reads an `info` table: the rendered text is compared with the stored copy.
- Keep each table at most about 120 characters wide. `box` mode does not truncate. Split a wide table, or show a short column in place of a long one.
- Where the experiment predicts a value, show the prediction beside the measurement.
- Do not add a checks section or print the verdict; the runner adds both.
- Head the file with a comment that lists the tables and what one row of each is.

## Commands

```text
go run ./cmd/course run 3.1            run in the lab container and print the walkthrough
go run ./cmd/course run -i 3.1         the same, then stay in DuckDB with the tables loaded
go run ./cmd/course run -update 3.1    the same, and store the tables and the rendered text as evidence
go run ./cmd/course check evidence ch03 -runs 3
docker compose run --rm lab go run ./cmd/course check evidence ch03 -runs 3
go run ./cmd/course inline ch03-btrees-and-page-management/ch03-btrees-and-page-management.md
```

An experiment without a `report.sql` is still checked the old way: its stdout, minus `INFO:` lines, against `evidence/N.M-slug.txt`.

## What changed for the reader

- The command in the book is `go run ./cmd/course run N.M`. The experiment package can still be run alone with `go run ./chNN-…/experiments/N.M-slug`; it then prints its verdict and leaves the tables in `_work/N.M/results/`.
- The "Captured evidence" block of a chapter is the rendered walkthrough. The numbers come from the stored tables beside it.
- Evidence is captured in the `lab` container. Values that depend on the platform were measured there.

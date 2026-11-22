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

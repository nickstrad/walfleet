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

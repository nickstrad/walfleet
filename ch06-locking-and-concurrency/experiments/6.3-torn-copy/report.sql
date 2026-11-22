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

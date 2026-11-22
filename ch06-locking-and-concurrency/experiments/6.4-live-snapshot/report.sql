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

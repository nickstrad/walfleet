-- Walkthrough of experiment 8.3. Run it with:
--   go run ./cmd/course run 8.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   setup               one setting of the experiment (name, value)
--   streams             part A: one run of the operation stream (bounded or unbounded), counted by its child process
--   unclean_evictions   part A: one eviction that did not leave its file at rest (empty when every check holds)
--   observations        part B: one file at one moment of a case: its at-rest status, size, sha256, or recipe step
--   problems            part B: one problem the at-rest check reported for an observation, and why it matters
--   checks              every claim the experiment asserted (the runner lists it last)
-- info.sql presents info_streams (peak memory, read latency, run time) and
-- info_limits (RLIMIT_NOFILE): they vary between runs and machines.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  8.3  Can a bounded connection manager serve many databases at rest?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Part A serves a seeded stream of reads and writes over many WAL'
.print 'databases twice: through an LRU manager that keeps at most K connections'
.print 'open, and through one that never evicts. Each run happens in its own'
.print 'child process, on its own copies of the files, so descriptor and memory'
.print 'baselines are clean. Part B shows six cases where a closed WAL database'
.print 'is not one file at rest, or changes while closed. The settings:'

SELECT name, value FROM setup WHERE name <> 'a read runs' ORDER BY seq;

.mode list
.headers off
SELECT 'A read runs: ' || value FROM setup WHERE name = 'a read runs' ORDER BY seq;
.headers on
.mode box

.print
.print '── A. A bounded manager against an unbounded one ───────────────────────'
.print
.print 'A1. The same stream in both runs. The bounded manager opens a database'
.print '    again each time it misses; the unbounded one opens each database once.'

SELECT mode, capacity, operations, reads, writes, distinct_dbs, hits, misses, peak_open
FROM streams
ORDER BY capacity;

.print
.print 'A2. Every eviction closed the connection and found the file at rest; the'
.print '    manager recorded its sha256 and checked it at the next open. Every'
.print '    open is either a first open or a verified reopen, and none was refused.'
.print '    (unclean_evictions lists any eviction that was not at rest.)'

SELECT mode, evictions, evicted_at_rest AS at_rest, evicted_not_at_rest AS not_at_rest,
       misses AS opens, first_opens, verified_reopens AS verified, refused_reopens AS refused,
       misses = first_opens + verified_reopens AS opens_add_up
FROM streams
ORDER BY capacity;

.print
.print 'A3. Descriptors, sampled between operations: always exactly 3 per open'
.print '    connection (.db, -wal, -shm), so the peak is 3 x peak_open, and 0'
.print '    after Close. The bounded run never holds more than 3 x K.'

SELECT mode, fd_samples, peak_fds, 3 * peak_open AS three_x_peak_open, fd_mismatches, fds_after_close
FROM streams
ORDER BY capacity;

.print
.print 'A4. Correctness. Every read saw the step the model expects, and every'
.print '    file, once closed, equals the recipe state of its step: exactly, or'
.print '    except the change counter (several steps applied on one connection).'

SELECT mode, read_mismatches, files_exact, files_masked AS equal_except_counter, files_wrong
FROM streams
ORDER BY capacity;

.print
.print 'A5. The cost the bound saves and the price it pays. Peak descriptors'
.print '    against the bounded limit 3 x K; reads served by an open connection'
.print '    (warm) or by an open, verify and read (cold). The memory budget is'
.print '    capacity x (2 x cache budget + 256 KiB) + 32 MiB; the measured peak'
.print '    memory and the read latencies vary between machines: see info.sql.'

SELECT mode,
       peak_fds,
       3 * (SELECT value FROM setup WHERE name = 'K (bounded capacity)')::INTEGER AS three_x_k,
       warm_reads,
       cold_reads,
       round(memory_budget / 1048576, 1) AS memory_budget_mib
FROM streams
ORDER BY capacity;

.print
.print '── B. When a closed WAL database is NOT a single file at rest ───────────'
.print
.print 'Six databases, each a copy of step 0, served by a manager with K = 4.'
.print 'Each case writes step 1 and then closes the connection in a way that'
.print 'goes wrong. A row is the at-rest check of a file at a moment, with the'
.print 'problems it found indented under it, or the recipe step the file holds.'

CREATE TEMP VIEW b_rows AS
SELECT o.case_id, o.seq, 0 AS sub, o.moment, o.file,
       CASE WHEN o.recipe_step IS NOT NULL
                 THEN 'step ' || o.recipe_step || CASE WHEN o.exact THEN ' (exact bytes)'
                                                      ELSE ' (equal except the change counter)' END
            WHEN o.at_rest AND o.sha256 IS NOT NULL
                 THEN 'at rest, ' || o.bytes || ' bytes, sha256 ' || left(o.sha256, 12) || '...'
            WHEN o.at_rest IS NULL AND o.bytes IS NOT NULL
                 THEN o.bytes || ' bytes'
            WHEN NOT o.at_rest THEN 'not at rest:'
            ELSE coalesce(o.note, '')
       END AS result
FROM observations o
UNION ALL
SELECT o.case_id, o.seq, p.seq, '', '', '  ' || p.problem
FROM problems p JOIN observations o ON o.seq = p.observation;

.print
.print 'B1. Another process still has the database open when the manager closes'
.print '    its connection. The close cannot take the lock it needs to checkpoint,'
.print '    so -wal and -shm stay. The last connection to close (the other'
.print '    process) checkpoints and deletes them.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B1' ORDER BY seq, sub;

.print
.print 'B2. A statement is left open (rows not closed) when the handle is'
.print '    released. database/sql cannot close a connection a statement still'
.print '    uses, so DB.Close returns and the connection stays open until rows.Close.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B2' ORDER BY seq, sub;

.print
.print 'B3. The process that wrote is killed before it closes. Its committed'
.print '    step 1 is only in the -wal: a copy of the database file alone is a'
.print '    valid database that holds step 0. One clean open and close recovers'
.print '    the WAL, checkpoints it and deletes it.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B3' ORDER BY seq, sub;

.print
.print 'B4. SQLITE_FCNTL_PERSIST_WAL is set on the connection. The close'
.print '    checkpoints but keeps -wal (with its bytes) and -shm, so the manager'
.print '    refuses to call the file at rest. The refusal is conservative here: a'
.print '    copy of the database file alone already holds step 1, but a rule based'
.print '    on which files exist cannot see that.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B4' ORDER BY seq, sub;

.print
.print 'B5. The file is renamed while its connection is open. Close sees that'
.print '    the file has moved and skips the checkpoint: step 1 stays in a -wal'
.print '    under the old name, and moved.db looks at rest but holds step 0.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B5' ORDER BY seq, sub;

.print
.print 'B6. The file changes while it is closed: another program applies the'
.print '    next step. Its sha256 no longer matches the one recorded at eviction,'
.print '    so the manager refuses to reopen it.'

SELECT moment, file, result FROM b_rows WHERE case_id = 'B6' ORDER BY seq, sub;

.print
.print 'The error the reopen returned:'
.mode list
.headers off
SELECT '  ' || regexp_replace(note, '([0-9a-f]{12})[0-9a-f]{52}', '\1...', 'g') FROM observations WHERE case_id = 'B6' AND note IS NOT NULL ORDER BY seq;
.headers on
.mode box

.print
.print 'B7. What each problem means:'
.mode list
.headers off
SELECT '  ' || problem || CASE WHEN why IS NULL THEN '' ELSE ': ' || why END
FROM (SELECT problem, why, min(seq) AS first FROM problems GROUP BY problem, why)
ORDER BY first;
.headers on
.mode box

.print
.print 'B8. The sha256 values above, in full. Every file that holds step 1 at'
.print '    rest has the same bytes; the second value is db-0005.db after the'
.print '    write made outside the manager.'

SELECT sha256, string_agg(DISTINCT case_id, ' ' ORDER BY case_id) AS cases, count(*) AS observations
FROM observations
WHERE sha256 IS NOT NULL
GROUP BY sha256
ORDER BY min(seq);

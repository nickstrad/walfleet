-- Walkthrough of experiment 8.2. Run it with:
--   go run ./cmd/course run 8.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   setup            one setting of the experiment (name, value)
--   fd_states        parts A and D: one connection state; descriptors above the baseline, predicted and named
--   fleet            part B: the descriptors held with all N connections open, then after closing all
--   fleet_targets    part B: one descriptor held with all N connections open (its file, relative to the directory)
--   connections      parts B and C: one idle connection; SCHEMA_USED and STMT_USED before and after shrink_memory
--   skipped          parts skipped on this platform (empty on Linux)
--   checks           every claim the experiment asserted (the runner lists it last)
-- info.sql presents info_cache (CACHE_USED per connection), info_memory
-- (process memory) and info_limits (RLIMIT_NOFILE): they vary between machines.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  8.2  What does an idle connection cost in descriptors and memory?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Descriptors are counted in this process before and after each step;'
.print 'the numbers below are the difference. Settings of the experiment:'

SELECT name, value FROM setup ORDER BY seq;

.print
.print 'Skipped on this platform:'

SELECT part, what FROM skipped
UNION ALL
SELECT '-', 'nothing' WHERE NOT EXISTS (FROM skipped)
ORDER BY part;

.print
.print '── A. Descriptors held by one connection, per state ────────────────────'
.print
.print 'A1. One plain connection to t.db, through five states: descriptors held'
.print '    above the baseline, the files they name, and the side files on disk.'
.print '    In delete mode the -journal exists, and holds a descriptor, only inside'
.print '    the write transaction. In WAL mode the first read opens -wal and -shm'
.print '    and the connection keeps all three open while idle; close removes'
.print '    both files.'

SELECT journal_mode, state, descriptors, expected, targets,
       coalesce(nullif(side_files, ''), 'none') AS side_files
FROM fd_states
WHERE part = 'A'
ORDER BY journal_mode, seq;

.print
.print '── B. Many idle WAL connections, one database file each ────────────────'
.print
.print 'B1. Every idle WAL connection holds exactly three descriptors, and'
.print '    closing them all releases every one and leaves each file at rest.'

SELECT stage,
       descriptors,
       round(descriptors / (SELECT value FROM setup WHERE name = 'connections')::INTEGER, 2) AS per_connection,
       files_not_at_rest
FROM fleet
ORDER BY seq;

.print
.print 'B2. What those descriptors point at: one .db, one -wal and one -shm per'
.print '    file.'

SELECT CASE WHEN target LIKE '%-wal' THEN '-wal'
            WHEN target LIKE '%-shm' THEN '-shm'
            ELSE 'db' END AS kind,
       count(*)              AS descriptors,
       min(target)           AS first,
       max(target)           AS last
FROM fleet_targets
GROUP BY kind
ORDER BY kind = 'db' DESC, kind DESC;

.print
.print 'B3. The memory each connection reports for its schema and its prepared'
.print '    statement is the same on every connection. CACHE_USED (page cache)'
.print '    also counts the Pager allocation, whose size depends on the length of'
.print '    the path, so it varies between machines: info.sql shows it with the'
.print '    process memory.'

SELECT count(*)                                     AS connections,
       min(schema_used) || '..' || max(schema_used) AS schema_used,
       min(stmt_used) || '..' || max(stmt_used)     AS stmt_used,
       sum(schema_used)                             AS schema_used_sum,
       sum(stmt_used)                               AS stmt_used_sum
FROM connections;

.print
.print '── C. PRAGMA shrink_memory ─────────────────────────────────────────────'
.print
.print 'C1. shrink_memory on connection 0 alone lowered CACHE_USED on every'
.print '    connection (this driver keeps every page cache in one shared group;'
.print '    the numbers are in info.sql). It frees cache pages only: schema and'
.print '    statement memory are unchanged on every connection.'

SELECT count(*) FILTER (schema_used_after_shrink = schema_used) AS schema_used_unchanged,
       count(*) FILTER (stmt_used_after_shrink = stmt_used)     AS stmt_used_unchanged,
       count(*)                                                 AS connections
FROM connections;

.print
.print '── D. Two connections to one WAL file in one process ───────────────────'
.print
.print 'D1. The second connection adds two descriptors, not three: both share'
.print '    one -shm descriptor. When it closes, its database descriptor stays'
.print '    open (4, not 3) until the last connection to the file closes.'

SELECT state, descriptors AS fds, expected, targets,
       coalesce(nullif(side_files, ''), 'none') AS side_files
FROM fd_states
WHERE part = 'D'
ORDER BY seq;

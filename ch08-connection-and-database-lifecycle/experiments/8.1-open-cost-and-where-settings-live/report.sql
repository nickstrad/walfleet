-- Walkthrough of experiment 8.1. Run it with:
--   go run ./cmd/course run 8.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   schemas          part A: one database per row (1, 100 or 1000 empty tables)
--   io_events        part A: one open or read of a file in the work directory, per phase, in trace order
--   schema_pages     part A: one page of the sqlite_schema b-tree (from dbstat), or t0001's root page
--   files            part B: the original file after its last close, and its plain copy
--   settings         part B: one setting, as set, as a fresh connection sees it, on the copy, on VACUUM INTO
--   placements       part B: sqlitex.SettingPlacements, the declared home of every course setting
--   special_cases    part B: the journal_mode and default_cache_size cases, one observed value per row
--   skipped          parts skipped on this platform (empty on Linux)
--   checks           every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  8.1  What does opening a database cost, and where do settings live?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Part A builds three WAL databases, at rest, with 1, 100 and 1000 tables'
.print 'of 4 columns, all empty. A child process opens each one with a plain'
.print 'driver connection (no PRAGMA), runs SELECT count(*) FROM t0001 and closes'
.print 'it under strace; the child marks where each phase starts.'
.print
.print 'Part B sets twelve settings on one file with sqlitex.Open, closes it,'
.print 'and reopens the file, a plain copy and a VACUUM INTO copy without'
.print 'applying any setting.'
.print
.print 'Skipped on this platform:'

SELECT part, what FROM skipped
UNION ALL
SELECT '-', 'nothing' WHERE NOT EXISTS (FROM skipped)
ORDER BY part;

.print
.print '── A. What open reads, and what the first statement reads ──────────────'
.print
.print 'A1. Schema memory is 0 after open: the schema is not read until the'
.print '    first statement. Then it grows with the number of tables.'

SELECT s.tables,
       s.file_pages,
       count(p.page) FILTER (p.role = 'sqlite_schema') AS sqlite_schema_pages,
       s.schema_used_after_open,
       s.schema_used_after_first,
       s.schema_used_after_first // s.tables         AS bytes_per_table
FROM schemas s LEFT JOIN schema_pages p USING (tables)
GROUP BY ALL
ORDER BY s.tables;

.print
.print 'A2. Per phase: open opens only the .db file and reads its 100-byte header'
.print '    (the one partial read); the first statement opens -wal and -shm and'
.print '    reads whole pages of the .db file; close reads nothing. Nothing is'
.print '    read from -wal or -shm.'

WITH e AS (
    SELECT *, file LIKE '%.db' AS is_db,
           op = 'read' AND file LIKE '%.db' AND byte_offset % 4096 = 0 AND nbytes = 4096 AS whole_page
    FROM io_events
),
phases AS (
    SELECT tables, phase, ord
    FROM schemas,
         (VALUES ('open', 1), ('first statement', 2), ('close', 3)) v(phase, ord)
)
SELECT p.tables, p.phase,
       count(e.seq)                                              AS events,
       coalesce(string_agg(CASE WHEN e.is_db THEN '.db' ELSE regexp_extract(e.file, '-[a-z]+$') END,
                           ' ' ORDER BY e.seq) FILTER (e.op = 'open'), '') AS files_opened,
       count(*) FILTER (e.op = 'read' AND e.is_db AND NOT e.whole_page) AS partial_db_reads,
       count(*) FILTER (e.whole_page)                            AS page_reads,
       count(DISTINCT e.byte_offset) FILTER (e.whole_page)       AS distinct_pages,
       count(*) FILTER (e.op = 'read' AND NOT e.is_db)           AS other_file_reads
FROM phases p LEFT JOIN e USING (tables, phase)
GROUP BY p.tables, p.phase, p.ord
ORDER BY p.tables, p.ord;

.print
.print 'A3. The events in order, side by side. Page 1 is read, the header says'
.print '    WAL (byte 19 = 2), so -wal and -shm are opened and page 1 is read'
.print '    again, now through the WAL. Then the sqlite_schema pages follow.'

WITH e AS (
    SELECT tables, phase, seq,
           CASE WHEN op = 'open' THEN 'open ' || file
                WHEN byte_offset % 4096 = 0 AND nbytes = 4096 AND file LIKE '%.db'
                     THEN 'read page ' || (byte_offset // 4096 + 1)
                ELSE 'read ' || nbytes || ' bytes at ' || byte_offset
                     || CASE WHEN file LIKE '%.db' THEN '' ELSE ' of ' || file END
           END AS event
    FROM io_events
    WHERE phase IN ('open', 'first statement')
),
shown AS (
    SELECT phase, seq::VARCHAR AS step, tables, event FROM e WHERE seq <= 9
    UNION ALL
    SELECT phase, 'more', tables, '... ' || count(*) || ' more' FROM e WHERE seq > 9 GROUP BY phase, tables
)
SELECT phase, step,
       coalesce(max(event) FILTER (tables = 1), '')    AS "1 table",
       coalesce(max(event) FILTER (tables = 100), '')  AS "100 tables",
       coalesce(max(event) FILTER (tables = 1000), '') AS "1000 tables"
FROM shown
GROUP BY phase, step
ORDER BY phase = 'first statement', step = 'more', try_cast(step AS INTEGER);

.print
.print 'A4. The pages the first statement reads are exactly the pages listed in'
.print '    schema_pages: the sqlite_schema b-tree (from dbstat) plus the root page'
.print '    of t0001. Page 1 is the sqlite_schema root and the page read twice.'

WITH r AS (
    SELECT tables, byte_offset // 4096 + 1 AS page
    FROM io_events
    WHERE phase = 'first statement' AND op = 'read' AND file LIKE '%.db'
      AND byte_offset % 4096 = 0 AND nbytes = 4096
),
read_pages AS (SELECT DISTINCT tables, page FROM r)
SELECT s.tables,
       (SELECT count(*) FROM schema_pages p WHERE p.tables = s.tables AND role = 'sqlite_schema') AS schema_pages,
       (SELECT page FROM schema_pages p WHERE p.tables = s.tables AND role = 't0001 root')       AS t0001_root,
       (SELECT count(*) FROM read_pages x WHERE x.tables = s.tables)                             AS pages_read,
       (SELECT count(*) FROM r WHERE r.tables = s.tables AND page = 1)                           AS page_1_reads,
       (SELECT count(*) FROM read_pages x WHERE x.tables = s.tables
          AND x.page NOT IN (SELECT page FROM schema_pages p WHERE p.tables = s.tables))         AS read_not_listed,
       (SELECT count(*) FROM schema_pages p WHERE p.tables = s.tables
          AND p.page NOT IN (SELECT page FROM read_pages x WHERE x.tables = s.tables))           AS listed_not_read
FROM schemas s
ORDER BY s.tables;

.print
.print 'A5. The page numbers, as ranges. Page 2 is the root of t0001, the first'
.print '    table created; every other page belongs to sqlite_schema, whose pages'
.print '    are spread through the file because they were allocated between tables.'

WITH p AS (
    SELECT DISTINCT tables, byte_offset // 4096 + 1 AS page
    FROM io_events
    WHERE phase = 'first statement' AND op = 'read' AND file LIKE '%.db'
      AND byte_offset % 4096 = 0 AND nbytes = 4096
),
g AS (SELECT tables, page, page - row_number() OVER (PARTITION BY tables ORDER BY page) AS grp FROM p),
r AS (SELECT tables, min(page) AS lo, max(page) AS hi FROM g GROUP BY tables, grp),
t AS (
    SELECT tables, lo,
           CASE WHEN lo = hi THEN lo::VARCHAR ELSE lo || '-' || hi END AS txt,
           row_number() OVER (PARTITION BY tables ORDER BY lo) AS rn
    FROM r
)
SELECT tables, string_agg(txt, ',' ORDER BY lo) AS pages_read_by_the_first_statement
FROM t
GROUP BY tables, (rn - 1) // 14
ORDER BY tables, min(lo);

.print
.print '── B. Which settings live in the file, and which in the connection ─────'
.print
.print 'B1. After its last connection closed the original is one file at rest,'
.print '    and a plain copy of it has the same sha256 (rung R5: capture = the'
.print '    file at rest, rebuild = copy).'

SELECT file, at_rest, bytes, sha256 FROM files ORDER BY seq;

.print
.print 'B2. Each setting as set on the original, on a fresh file (the default),'
.print '    and on the copy and the VACUUM INTO output, each opened with no'
.print '    settings applied. A setting the copy kept lives in the file; the'
.print '    declared column is what sqlitex.SettingPlacements says. VACUUM INTO'
.print '    keeps every file setting except journal_mode=wal.'

SELECT s.setting,
       s.on_original,
       s.fresh_default  AS "default",
       s.on_copy        AS copy,
       s.on_vacuum_into AS vacuum_into,
       s.lives_in,
       p.home           AS declared,
       p.header_bytes
FROM settings s LEFT JOIN placements p ON p.name = s.setting
ORDER BY s.seq;

.print
.print 'B3. The two journal_mode cases and the deprecated default_cache_size.'
.print '    A rollback mode (persist) reverts to delete on reopen; only wal is'
.print '    stored in the file (header bytes 18-19 = 2), and the VACUUM INTO output'
.print '    is a rollback-mode file (bytes 18-19 = 1).'
.print '    default_cache_size is stored in header bytes 48-51 and becomes the'
.print '    starting cache_size of every new connection.'

SELECT case_name, observed, value FROM special_cases ORDER BY seq;

.print
.print 'B4. The settings a platform must store outside the file to rebuild a'
.print '    database that behaves the same are the connection rows of this list'
.print '    (sqlitex.SettingPlacements, which B2 checks against):'

SELECT name, pragma, home, header_bytes FROM placements ORDER BY seq;

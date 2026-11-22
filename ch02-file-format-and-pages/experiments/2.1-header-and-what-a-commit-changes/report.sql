-- Walkthrough of experiment 2.1. Run it with:
--   go run ./cmd/course run 2.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   r0              part A: one row, the R0 database the header is decoded from
--   r0_header       part A: one row per header field, as decoded by internal/dbfile
--   oracles         part A: one row per field, with SQLite's own report of it (NULL: no oracle)
--   bases           parts B-E: one row per base database a scenario starts from
--   events          parts B-D: one row per commit, close or statement the header is read after
--   header_changes  parts B-D: one row per header field that differs between two copies of page 1
--                   (one row with no field: no header byte changed)
--   size_variants   part E: one row per edited copy of a database, with both page counts
--   page_sizes      part F: one row per page size
--   checks          every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  2.1  What is in the 100-byte header, and what does a commit change?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The first 100 bytes of a database file are the header. The experiment'
.print 'decodes it field by field with internal/dbfile and compares every field'
.print 'with what SQLite itself reports. Then it reads page 1 after commits, in'
.print 'rollback mode and in WAL mode, to see which header bytes move and when.'
.print 'In WAL mode two copies of page 1 exist: "current" is the view of SQLite'
.print '(sqlite_dbpage, WAL included) and "file" is the bytes in the database'
.print 'file, read by a child process.'

.print
.print '── A. Decode the header of the R0 database ─────────────────────────────'
.print
.print 'A1. The R0 database: the recipe, the page count we derive and where it comes from.'

SELECT recipe, sha256 FROM r0;

SELECT page_count, page_count_source, in_header_size_valid FROM r0;

.print
.print 'A2. The 100 bytes, decoded by internal/dbfile.'

SELECT byte_offset, size, field, value FROM r0_header ORDER BY byte_offset;

.print
.print 'A3. Every decoded field against SQLite: PRAGMA through the Go driver and the'
.print '.dbinfo of the pinned sqlite3 CLI. "-" means that oracle does not report the field.'

SELECT field,
       ours,
       coalesce(pragma, '-')                  AS pragma,
       coalesce(CAST(dbinfo AS VARCHAR), '-') AS dbinfo,
       CASE WHEN pragma IS NULL AND dbinfo IS NULL THEN 'n/a'
            ELSE CAST((pragma IS NULL OR pragma = ours)
                  AND (dbinfo IS NULL OR CAST(dbinfo AS VARCHAR) = ours) AS VARCHAR)
       END                                    AS match
FROM oracles
ORDER BY seq;

.print
.print '── B. Rollback mode: what one commit changes in page 1 ─────────────────'
.print
.print 'B1. Steps 28, 29 and 30 on one connection; page 1 is read from the database'
.print 'file after each commit. Each commit advances the file change counter (24-27)'
.print 'and version-valid-for (92-95) by one, and moves the size, freelist and'
.print 'user-version fields.'
.print
.print 'The base database, built to step 27 and closed:'

SELECT name, journal_mode, built_to_step, write_version AS byte18, read_version AS byte19,
       change_counter, in_header_size, file_pages
FROM bases WHERE part = 'B' ORDER BY name;

.print
.print 'The header fields that differ from the page 1 before the commit:'

SELECT e.event,
       c.view,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       coalesce(c.field, 'no header byte changed')               AS field,
       c.before_value AS before,
       c.after_value  AS after,
       c.changed_bytes
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.part = 'B'
ORDER BY c.event_seq, c.byte_offset;

.print
.print '── C. WAL mode (the platform default): what changes in page 1, and when ──'
.print
.print 'The base database, built to step 27 and closed:'

SELECT name, journal_mode, built_to_step, write_version AS byte18, read_version AS byte19,
       change_counter, in_header_size, file_pages
FROM bases WHERE part = 'C' ORDER BY name;

.print
.print 'C1. Steps 28, 29, 30 on one connection; each step writes page 1 (user_version).'
.print 'Look at the "current vs previous" rows: the change counter moves at step 28 and'
.print 'then stays put, although page 1 is rewritten each time. The "file vs base"'
.print 'rows show page 1 of the database file unchanged until the last connection'
.print 'closes and the checkpoint copies the current page 1 into it.'
.print
.print 'The commits and the size of the -wal after each:'

SELECT event, wal_state FROM events WHERE scenario = 'C1' ORDER BY seq;

.print
.print 'The header fields that differ, by view:'

SELECT e.event,
       c.view,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       coalesce(c.field, 'no header byte changed')               AS field,
       c.before_value AS before,
       c.after_value  AS after,
       c.changed_bytes
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C1'
ORDER BY c.event_seq, c.view, c.byte_offset;

.print
.print 'C2. Three commits on one connection that do not touch page 1 (UPDATE one'
.print 'account balance). The -wal grows, the header does not change: not even the'
.print 'change counter, before or after the checkpoint at close.'

SELECT e.event, e.wal_state, c.view, coalesce(c.field, 'no header byte changed') AS field
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C2'
ORDER BY c.event_seq, c.byte_offset;

.print
.print 'C3. Steps 28, 29, 30, each on a new connection, while a second idle connection'
.print 'stays open so that closing a writer does not checkpoint. Now the counter'
.print 'advances at every step: each new connection reads page 1 afresh and writes'
.print 'counter + 1.'

SELECT e.event,
       e.wal_state,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       c.field,
       c.before_value AS before,
       c.after_value  AS after
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C3'
ORDER BY c.event_seq, c.byte_offset;

.print
.print 'C4. Steps 28, 29, 30 on one connection again, with PRAGMA shrink_memory before'
.print 'each step. It drops page 1 from the cache of the connection, and the counter'
.print 'advances once per commit: the constant counter of C1 comes from page 1 staying'
.print 'cached, not from the connection.'

SELECT e.event,
       e.wal_state,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       c.field,
       c.before_value AS before,
       c.after_value  AS after
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C4'
ORDER BY c.event_seq, c.byte_offset;

.print
.print '── D. Bytes 18-19: switching journal mode rewrites the header ──────────'
.print
.print 'The base database is in rollback mode, built to step 3. Each statement runs'
.print 'on its own connection that is then closed, and no -wal file exists. The'
.print 'table shows what the file header differs by afterwards.'

SELECT name, journal_mode, built_to_step, write_version AS byte18, read_version AS byte19,
       change_counter, in_header_size, file_pages
FROM bases WHERE part = 'D' ORDER BY name;

SELECT e.event,
       e.detail,
       c.view,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       c.field,
       c.before_value AS before,
       c.after_value  AS after
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.part = 'D'
ORDER BY c.event_seq, c.byte_offset;

.print
.print '── E. The in-header size (bytes 28-31) is used only when 24-27 equal 92-95 ──'
.print
.print 'Four edited copies of a rollback-mode database, opened read-only. The page'
.print 'count of each according to our parser and to SQLite (PRAGMA page_count).'
.print
.print 'The base database, built to step 3:'

SELECT name, journal_mode, built_to_step, change_counter, in_header_size, file_pages
FROM bases WHERE part = 'E' ORDER BY name;

SELECT variant, description, in_header_size
FROM size_variants ORDER BY variant;

.print
.print 'The page count each of them reports:'

SELECT variant, ours AS "our parser", sqlite AS "SQLite page_count"
FROM size_variants ORDER BY variant;

.print
.print '── F. The page size lives in bytes 16-17; 65536 is stored as 1 ───────────'
.print
.print '65536 does not fit in two bytes. Each row is a new database created with'
.print 'PRAGMA page_size = requested; bytes 16 and 17 are shown in hex.'

SELECT requested, byte16, byte17, stored, decoded, sqlite_reports
FROM page_sizes ORDER BY requested;

-- Walkthrough of experiment 1.1. Run it with:
--   go run ./cmd/course run 1.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   pin                 one row per field of sqlite/PIN
--   versions            one row per version the experiment compared with the pin
--   driver_options      one row per compile option of the Go driver (PRAGMA compile_options)
--   vtab_rows           rows that sqlite_dbpage and dbstat return in the driver
--   required_options    one row per compile option that later labs depend on
--   format_options      one row per file-format or I/O option: driver value, CLI value
--   checks              every claim the experiment asserted (the runner lists it last)
-- info_cli and info_option_differences hold the CLI path and the build-specific
-- option differences; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  1.1  Do the Go driver and the sqlite3 CLI run the pinned SQLite?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The course uses two SQLite builds: the modernc.org/sqlite Go driver and a'
.print 'sqlite3 command-line shell. Both must be the pinned check-in and agree on'
.print 'the options that change the file format.'

.print
.print '── A. The pin and the driver ───────────────────────────────────────────'
.print
.print 'A1. The pin (sqlite/PIN) that every experiment checks first:'

SELECT field, value FROM pin ORDER BY field;

.print
.print 'A2. What is linked or reported, against what the pin says. Each row'
.print 'should show the same text in both columns (source ids are cut after 12 hex digits; A1 has them in full):'

SELECT item,
       regexp_replace(found, '([0-9a-f]{12})[0-9a-f]{52}', '\1...')  AS found,
       regexp_replace(pinned, '([0-9a-f]{12})[0-9a-f]{52}', '\1...') AS pinned,
       found = pinned AS match
FROM versions ORDER BY seq;

.print
.print 'A3. The compile options of the driver. The three that later labs need'
.print 'are sqlite_dbpage and dbstat (page inspection) and the FULL sync default in WAL mode:'

SELECT o.option, o.option IN (SELECT option FROM driver_options) AS present
FROM required_options o
ORDER BY o.option;

.print
.print 'A4. The full list of driver options (the last seq is the count):'

SELECT seq, option FROM driver_options ORDER BY seq;

.print
.print 'A5. Both page-inspection virtual tables answer in the driver. The probe'
.print 'database has 2 pages: page 1 for sqlite_schema and page 2 for table t.'

SELECT vtab, rows FROM vtab_rows ORDER BY vtab;

.print
.print '── B. The CLI against the driver ───────────────────────────────────────'
.print
.print 'B1. The CLI has the same required options (in_cli should be true too):'

SELECT option, in_driver, in_cli FROM required_options ORDER BY option;

.print
.print 'B2. The options that govern the file format, cache, mmap and sync defaults'
.print 'must be identical in both builds:'

SELECT name, driver, cli, CASE WHEN same THEN 'same' ELSE 'DIFFERENT' END AS verdict
FROM format_options ORDER BY name;

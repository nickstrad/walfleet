-- Walkthrough of experiment 10.4. Run it with:
--   go run ./cmd/course run 10.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings           one setting of the run (name, value)
--   writers            one writer process: how many databases it owns and how many it rolled out
--   databases          one database: what was acknowledged, quiesced and rebuilt
--   rebuild            the rebuild from the store: objects read and written
--   checker_passes     the final pass of the checker over every restore point in the store
--   checker_failures   one restore point a checker pass could not rebuild (none on a good run)
--   problems           one problem or over-bounds line (none on a good run)
--   checks             every claim the experiment asserted (the runner lists it last)
-- info_run, info_timings and info_writers hold timings, byte counts, the passes
-- of the concurrent checker and the standing-bounds summaries; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.4  Can the whole fleet be rebuilt from the store and the catalog?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Writer processes own equal shares of the fleet and keep a bounded number'
.print 'of databases open. A separate checker process lists the store and rebuilds'
.print 'every published restore point in memory while the writers run. Then every'
.print 'writer closes, every local file is deleted, and fresh nodes rebuild the'
.print 'whole fleet from the S3 store and the catalog alone.'

SELECT name, value FROM settings ORDER BY seq;

.print
.print '── A. Write stream and a schema rollout stopped halfway ────────────────'
.print
.print 'A1. Every database is claimed and given recipe steps 1-4, each write'
.print '    acknowledged store-durable. Then recipe step 5 (the CREATE INDEX in'
.print '    rollout_sql) is applied to the first half of the share of each writer,'
.print '    and the rollout stops.'

SELECT writer, databases, rolled_out FROM writers ORDER BY writer;

SELECT sum(databases) AS databases, sum(rolled_out) AS rolled_out,
       min(databases) || '-' || max(databases) AS per_writer
FROM writers;

.print
.print 'A2. Every restore point the checker found while the writers ran rebuilt to'
.print '    a recipe state: no checker failure. How many passes it made depends on'
.print '    timing; info_run has the count.'

SELECT count(*) FILTER (phase = 'concurrent') AS concurrent_failures FROM checker_failures;

.print
.print '── B. Quiesce ──────────────────────────────────────────────────────────'
.print
.print 'B1. Every writer closes. Each database is recorded (sha256, catalog row)'
.print '    after a TRUNCATE checkpoint. The close of the writer already left no'
.print '    WAL frame to checkpoint: the last close deletes -wal and -shm.'

SELECT count(*) AS databases_recorded, sum(quiesce_wal_frames) AS wal_frames_found
FROM databases;

.print
.print '── C. Rebuild from the store alone ─────────────────────────────────────'
.print
.print 'C1. Every local file is deleted. One fresh node per worker claims its share'
.print '    of the fleet and restores it from the store. A claim reads every slot'
.print '    twice (once to check the lineage, once to restore) and writes one fence.'

SELECT databases, workers, local_files_left, listings, objects_read, fence_objects_written
FROM rebuild;

.print
.print 'C2. The rebuilt databases: byte-identical (E1) to the sha256 recorded at'
.print '    quiesce, and at the recipe step the catalog recorded and the writer last'
.print '    acknowledged. Half are on the old schema (step 4), half on the new one'
.print '    (step 5); nothing else. Each matches its recipe step file except the'
.print '    change counter (bytes 24-27 and 92-95).'

SELECT rebuilt_user_version AS recipe_step,
       CASE WHEN rebuilt_state LIKE '%exact%' THEN 'exact' ELSE 'counter differs' END AS vs_recipe_file,
       count(*) AS databases,
       count(*) FILTER (byte_identical)                                    AS byte_identical,
       count(*) FILTER (rebuilt_user_version = catalog_schema_version
                        AND rebuilt_user_version = acknowledged_version)  AS schema_agrees
FROM databases GROUP BY ALL ORDER BY recipe_step, vs_recipe_file;

.print
.print 'C3. The stream positions each database reached in the store (last_shipped in'
.print '    the catalog), by schema:'

SELECT rebuilt_user_version AS recipe_step, last_shipped, count(*) AS databases
FROM databases GROUP BY ALL ORDER BY recipe_step, last_shipped;

.print
.print 'C4. A few databases, for a closer look:'

SELECT database, writer, acknowledged_version AS acked, catalog_schema_version AS catalog,
       last_shipped, rebuilt_user_version AS rebuilt,
       CASE WHEN rebuilt_state LIKE '%exact%' THEN 'exact' ELSE 'counter differs' END AS vs_recipe_file,
       byte_identical
FROM databases WHERE database IN ('db-0001', 'db-0002', 'db-0500') ORDER BY database;

.print
.print '── D. The final checker pass ───────────────────────────────────────────'
.print
.print 'D1. After the rebuild, the checker rebuilds every published restore point'
.print '    of every database. The slots the rebuild found are those restore points'
.print '    minus the fences it wrote, and it read each of them exactly twice.'

SELECT c.databases, c.restore_points, c.failed,
       c.restore_points - r.fence_objects_written      AS slots_found,
       r.objects_read,
       r.objects_read = 2 * (c.restore_points - r.fence_objects_written) AS each_read_twice
FROM checker_passes c, rebuild r WHERE c.pass = 'final' ORDER BY c.pass;

.print
.print '── E. Every process ────────────────────────────────────────────────────'
.print
.print 'E1. Problems and over-bounds lines of the writers, the checker and the'
.print '    rebuilding process. All counts are 0; any line would be listed in the'
.print '    table problems.'

SELECT count(*) FILTER (kind = 'problem')             AS problems,
       count(*) FILTER (kind = 'over bounds')         AS over_bounds,
       count(*) FILTER (kind = 'rebuild over bounds') AS rebuild_over_bounds
FROM problems;

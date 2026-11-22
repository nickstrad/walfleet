-- Walkthrough of experiment 1.3. Run it with:
--   go run ./cmd/course run 1.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   recipe        the recipe and its settings, one row per key
--   plan          one row per step: how many statements of each kind it runs
--   runs          one row per rebuild of the recipe, by journal mode
--   step_hashes   one row per step and mode: sha256 of the closed file after the step
--   lookup        the step that a database built to step 12 is matched to
--   comparisons   one row per pair of files compared (id is the variant number below)
--   unused_pages  one row per differing page of the comparisons that look at the bytes
--   checks        every claim the experiment asserted (the runner lists it last)
-- info_run and info_hashes hold timings, paths and the hashes of runs that differ;
-- info.sql shows them.

CREATE TEMP VIEW diffs AS
SELECT seq, id, what, equal,
       CASE WHEN equal THEN 'identical' ELSE 'differ' END AS result,
       TRY_CAST(split_part(differing_pages, ' ', 1) AS INTEGER) AS first_page,
       CASE WHEN differing_pages IS NULL THEN 0 ELSE len(string_split(differing_pages, ' ')) END AS pages_differ,
       differing_pages, page1_bytes,
       CASE WHEN size_a = size_b THEN CAST(size_a // 4096 AS VARCHAR)
            ELSE (size_a // 4096) || ' vs ' || (size_b // 4096) END AS file_pages
FROM comparisons;

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  1.3  Does rebuilding the recipe give the same bytes, and what breaks it?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The seeded recipe builds a database in 30 steps from a fixed seed. Part A'
.print 'builds it twice, in different directories under different names, and'
.print 'compares the files after every step. Parts B to D then add one source of'
.print 'variation at a time to the recipe at step 6 and compare two runs: random()'
.print 'and the clock, scan order, transaction batches, secure_delete, the rowid'
.print 'past the maximum, connection sessions and a rolled-back SAVEPOINT.'
.print
.print 'The recipe and its settings:'

SELECT key, value FROM recipe WHERE key IN ('recipe', 'index_step') ORDER BY key;

SELECT key AS settings, unnest(range(1, len(parts) + 1)) AS n, unnest(parts) AS setting
FROM (SELECT key, string_split(value, ' ') AS parts FROM recipe WHERE key LIKE '%settings')
ORDER BY key, n;

.print
.print 'What each step does (statements by kind; step 0 creates the three tables):'

SELECT step, schema, inserts, updates, deletes, indexes FROM plan ORDER BY step;

.print
.print '── A. Rebuild the recipe twice ─────────────────────────────────────────'
.print
.print 'A1. Run 1 and run 2 build the recipe in different directories under'
.print 'different names. Per mode, the two files must have the same hash:'

SELECT mode, run, path, pages, sha256 FROM runs ORDER BY mode DESC, run;

.print
.print 'A2. The hash of the closed file after every step. Each step must leave a'
.print 'different file (a hash names one step), run 2 must repeat run 1 at every step'
.print 'and, in WAL mode, the cached list (recipe.StepHashes) must match as well.'
.print 'The second table lists the first 16 hex digits of every state; the CSV has all 64:'

SELECT mode, count(*) AS states, count(DISTINCT run1_sha256) AS distinct_hashes,
       bool_and(run1_sha256 = run2_sha256) AS run2_same,
       bool_and(run1_sha256 = cached_sha256) AS cached_same
FROM step_hashes GROUP BY mode ORDER BY mode DESC;

SELECT w.step, left(w.run1_sha256, 16) AS wal_sha256_prefix, left(d.run1_sha256, 16) AS delete_sha256_prefix
FROM step_hashes w JOIN step_hashes d USING (step)
WHERE w.mode = 'wal' AND d.mode = 'delete'
ORDER BY w.step;

.print
.print 'A3. A database built to step 12 is found again from its bytes alone:'

SELECT built_to_step, matched_step, exact AS exact_bytes FROM lookup;

.print
.print 'A4. The two journal modes build different files from the same recipe, but'
.print 'only page 1 differs. Bytes 18 and 19 are the file-format write and read'
.print 'versions (2 for WAL, 1 for rollback); 27 and 95 are the low bytes of the'
.print 'change counter and of version-valid-for:'

SELECT id, result, first_page, pages_differ, page1_bytes, file_pages, what FROM diffs WHERE id = '0' ORDER BY seq;

.print
.print '── B. One extra transaction after step 6 ──────────────────────────────'
.print
.print 'Each variant builds the recipe to step 6, runs one more transaction, and'
.print 'does it twice into fresh files. Rules 1 to 5 are the determinism rules'
.print 'these variants test.'
.print
.print 'B1. SQL random() (1) and the clock (2). current_timestamp has a one-second'
.print 'resolution, so two runs inside one second match and a run one second later'
.print 'does not:'

SELECT id, result, what FROM diffs WHERE id IN ('1', '2a', '2b') ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, file_pages FROM diffs WHERE id IN ('1', '2a', '2b') ORDER BY seq;

.print
.print 'B2. INSERT ... SELECT (3) takes its rowids from scan order. The same plan'
.print 'twice gives the same file; a different scan order changes the file, and'
.print 'ORDER BY id removes the dependence:'

SELECT id, result, what FROM diffs WHERE id LIKE '3%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, file_pages FROM diffs WHERE id LIKE '3%' ORDER BY seq;

.print
.print 'B3. Transaction batches (4). Steps 0-6 on one connection, with steps 2+3 and'
.print '4+5 each committed as one transaction instead of two. 4d compares two'
.print 'one-transaction-per-step builds that differ only in secure_delete.'

SELECT id, result, what FROM diffs WHERE id LIKE '4%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, page1_bytes, file_pages FROM diffs WHERE id LIKE '4%' ORDER BY seq;

.print
.print 'B4. Where the bytes differ in 4b (secure_delete=OFF: stale content left in'
.print 'unused space) and 4d (FAST leaves old bytes in freelist pages that ON zeroes).'
.print 'bytes_outside_unused counts differing bytes that sit in live data:'

SELECT id, page, bytes_differing, bytes_outside_unused, kind FROM unused_pages WHERE id LIKE '4%' ORDER BY id, page;

.print
.print 'B5. A rowid past the maximum (5): SQLite picks a random unused rowid:'

SELECT id, result, first_page, pages_differ, differing_pages, what FROM diffs WHERE id = '5' ORDER BY seq;

.print
.print '── C. Connection sessions ──────────────────────────────────────────────'
.print
.print 'C1. Steps 0-6 with one connection per step against one connection for all'
.print 'steps (6a, 6b). In WAL mode the change counter differs, and only it; in'
.print 'rollback mode nothing differs. 6c: three commits that never write page 1'
.print 'leave the files identical, because only a commit that writes page 1 writes'
.print 'the change counter.'

SELECT id, result, what FROM diffs WHERE id LIKE '6%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, page1_bytes, file_pages FROM diffs WHERE id LIKE '6%' ORDER BY seq;

.print
.print '── D. Rolled-back work inside a committed transaction ──────────────────'
.print
.print 'D1. Run 2 first inserts 10 documents of 12000 bytes inside a SAVEPOINT and'
.print 'rolls back to it, then commits the same INSERT as run 1. Freelist pages that'
.print 'the committed insert reuses keep the rolled-back bytes, even with'
.print 'secure_delete=ON:'

SELECT id, result, what FROM diffs WHERE id LIKE '7%' ORDER BY seq;

SELECT id, first_page, pages_differ, differing_pages, page1_bytes, file_pages FROM diffs WHERE id LIKE '7%' ORDER BY seq;

SELECT id, page, bytes_differing, bytes_outside_unused, kind FROM unused_pages WHERE id LIKE '7%' ORDER BY id, page;

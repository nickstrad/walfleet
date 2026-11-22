-- Walkthrough of experiment 2.3. Run it with:
--   go run ./cmd/course run 2.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   captures      part A: one row per run of the recipe, with its capture directory
--   manifest      part A: one row per page of each run's manifest (page, role, sha256)
--   comparisons   parts A and B: one row per manifest or file comparison, as the library reports it
--   rebuild       part B: one row, the database rebuilt from the capture alone
--   tampered      part C: one row per damaged capture, with the refusal
--   not_at_rest   part D: numbered observations of the two not-at-rest scenarios
--   checks        every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  2.3  Can the pages of a database be captured and rebuilt exactly?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Rebuild Ladder rung R1: internal/capture reads every page of the closed R0'
.print 'database into pages.bin and writes manifest.txt, one line per page with'
.print 'its role and the sha256 of its bytes. The experiment rebuilds a database'
.print 'from the capture alone and checks it is byte-identical, refuses five kinds'
.print 'of damaged captures, and refuses databases that are not at rest.'

.print
.print '── A. Capture: every page of the closed R0 database ────────────────────'
.print
.print 'A1. The recipe is run twice and each result captured. pages.bin holds'
.print 'pages x page_size bytes; manifest.txt starts with the format line, the page'
.print 'size and the page count, then one line per page.'

SELECT run, database, capture_dir, steps, pages, page_size, pages_bin_bytes
FROM captures ORDER BY run;

.print
.print 'A2. The manifest as a whole: its format line and the sha256 of its text.'
.print 'Both runs must give the same manifest.'

SELECT run, manifest_format, manifest_sha256 FROM captures ORDER BY run;

.print
.print 'A3. Run 1 against run 2, as the library compares them, and from the rows:'
.print 'the number of pages whose role or sha256 differ between the runs.'

SELECT name, result, equal, differing_pages
FROM comparisons WHERE part = 'A' ORDER BY name;

SELECT count(*) FILTER (a.role IS DISTINCT FROM b.role OR a.sha256 IS DISTINCT FROM b.sha256) AS differing_pages,
       count(*) AS pages_compared
FROM (SELECT * FROM manifest WHERE run = 1) a
FULL JOIN (SELECT * FROM manifest WHERE run = 2) b USING (page);

.print
.print 'A4. The roles in the manifest (run 1).'

SELECT role, count(*) AS pages FROM manifest WHERE run = 1 GROUP BY role ORDER BY pages DESC, role;

.print
.print 'A5. The manifest of run 1, one line per page.'

SELECT page, role, sha256 FROM manifest WHERE run = 1 ORDER BY page;

.print
.print '── B. Rebuild from the capture alone and prove it equal (E1) ───────────'
.print
.print 'B1. The rebuild: pages written in order, its sha256, what the recipe lookup'
.print 'says it is, and the integrity_check of SQLite.'

SELECT rebuilt, from_capture, pages_written, recipe_lookup, integrity_check FROM rebuild;

.print
.print 'B2. The rebuild against the original, and the manifest after step 29'
.print 'against the manifest after step 30. They differ first at page 1: the header'
.print 'changes on every recipe step.'

SELECT name, equal, first_differing_page, differing_pages
FROM comparisons WHERE part = 'B' ORDER BY name;

.print
.print 'The words of the library for each comparison:'

SELECT result FROM comparisons WHERE part = 'B' ORDER BY name;

.print
.print 'The sha256 of the rebuild, which is also the sha256 of the original:'

SELECT sha256 FROM rebuild;

.print
.print '── C. A damaged capture is refused, and the refusal names the page ─────'
.print
.print 'Each row is a copy of the capture with one thing changed. The rebuild must'
.print 'refuse it with a message that contains the text in wanted, and must not write'
.print 'an output file.'

SELECT case_no, damage, wanted, output_written FROM tampered ORDER BY case_no;

.print
.print 'The refusals:'

SELECT case_no, refusal FROM tampered ORDER BY case_no;

.print
.print '── D. A database that is not at rest is refused ────────────────────────'
.print
.print 'Its file alone is stale or torn. Each scenario starts a child process that'
.print 'leaves the database not at rest and exits with status 3 without closing'
.print 'its connection.'
.print
.print 'D1. WAL mode: the child commits one UPDATE (account 1 gains 1000). The'
.print 'commit lives in the -wal, so the database file alone is stale.'

SELECT CASE WHEN g.i = 0 THEN seq END AS seq,
       CASE WHEN g.i = 0 THEN observation END AS observation,
       substr(value, g.i * 70 + 1, 70) AS value
FROM not_at_rest, generate_series(0, (length(value) - 1) // 70) AS g(i)
WHERE scenario = 'D1'
ORDER BY not_at_rest.seq, g.i;

.print
.print 'D2. Rollback mode: the child sets cache_size=10, updates every document body'
.print 'in one transaction, which spills dirty pages to the file, and exits before'
.print 'COMMIT. The file alone holds part of an uncommitted transaction.'

SELECT CASE WHEN g.i = 0 THEN seq END AS seq,
       CASE WHEN g.i = 0 THEN observation END AS observation,
       substr(value, g.i * 70 + 1, 70) AS value
FROM not_at_rest, generate_series(0, (length(value) - 1) // 70) AS g(i)
WHERE scenario = 'D2'
ORDER BY not_at_rest.seq, g.i;

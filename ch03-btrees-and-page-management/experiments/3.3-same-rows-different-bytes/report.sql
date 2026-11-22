-- Walkthrough of experiment 3.3. Run it with:
--   go run ./cmd/course run 3.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   builds           part A: one row per build of the same rows, with its hashes
--   r0_tables        part A: rows per table of R0, the database of the recipe
--   r0_facts         part A: schema, pragmas and checks of R0, as the logical hash saw them
--   capture          part A: the ordered logical capture (rung R2) of R0, its size and hash
--   capture_lines    part A: the first nine and the last four lines of that capture
--   compare          one row per comparison of two files at one level (E1, E2, E3)
--   header_values    schema cookies and change counters of the two files compared
--   canonical_header part A: the header of the VACUUM INTO copy of R0
--   pairs            part B: one row per pair of databases that differ in content
--   limits           part C: dbhash and capture results for the three cases
--   checks           every claim the experiment asserted (the runner lists it last)

-- The words of one comparison, as a verdict.
CREATE VIEW verdicts AS
SELECT *,
       CASE WHEN level = 'E3' AND equal THEN 'equal'
            WHEN level = 'E3' THEN 'differ: ' || differences
            WHEN equal AND header_bytes_differing IS NOT NULL
                 THEN 'equal except header bytes [' || header_bytes_differing || ']'
            WHEN equal THEN 'equal'
            WHEN size_a <> size_b
                 THEN 'differ (' || differing_pages || ' pages differ, sizes differ)'
            ELSE 'differ (' || differing_pages || ' of ' || pages_total || ' pages)'
       END AS verdict
FROM compare;

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  3.3  The same rows, different bytes: which equality holds?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Three levels of equality between two database files:'
.print '  E1  the file sha256 is the same'
.print '  E2  the pages are the same, ignoring header bytes 24-27, 40-43 and 92-99'
.print '  E3  the logical hash is the same: schema, every row, the persistent pragmas,'
.print '      integrity_check ok and foreign_key_check unchanged'
.print 'dbhash is the SQLite tool, built from the pinned tree: it prints a SHA1 of'
.print 'the schema and every row it reads. It is skipped, and its column is empty,'
.print 'when the tool is not built.'


.print
.print '── A. The same rows built six ways ─────────────────────────────────────'
.print
.print 'A.1 R0 is the database of the recipe: seed 1, 30 steps of inserts, updates and'
.print 'deletes. Its content, as the logical hash saw it:'

SELECT table_name AS item, rows AS value FROM r0_tables
UNION ALL
SELECT name, value FROM r0_facts
ORDER BY item;

.print
.print 'A.2 The ordered logical capture of R0 (rung R2): a SQL script of the schema,'
.print 'the rows in a fixed order and the pragmas. Its size and hash:'

SELECT path, bytes, lines, sha256 FROM capture;

.print
.print 'The first nine and the last four lines of the capture:'
.mode list
.separator ' | '

SELECT line_no::VARCHAR AS line, text FROM capture_lines WHERE line_no <= 9
UNION ALL
SELECT '...', '(' || (SELECT lines - 13 FROM capture) || ' INSERT lines not shown)'
UNION ALL
SELECT line_no::VARCHAR, text FROM capture_lines WHERE line_no > 9
ORDER BY CASE WHEN line = '...' THEN 9.5 ELSE line::DOUBLE END;

.mode box

.print
.print 'A.3 The builds. All of them hold the same rows.'

SELECT seq, name, bytes, how FROM builds ORDER BY seq;

.print
.print 'A.4 Each build against R0 at E1, E2 and E3. The files differ in size because'
.print 'the builds lay the rows out differently. The backup is a page copy: it differs'
.print 'from R0 only in header bytes.'

SELECT subject AS build,
       max(verdict) FILTER (level = 'E1') AS e1,
       max(verdict) FILTER (level = 'E2') AS e2,
       max(verdict) FILTER (level = 'E3') AS e3
FROM verdicts
WHERE part = 'A' AND what = 'build vs r0'
GROUP BY subject
ORDER BY min(seq);

.print
.print 'A.5 The same builds by hash (first 16 hex digits of each). The file'
.print 'sha256 differs; the logical hash, the capture hash and dbhash agree for all of'
.print 'them. After VACUUM INTO, every build but the backup has one sha256.'

SELECT name,
       left(file_sha256, 16)      AS file_sha256,
       left(logical_sha256, 16)   AS logical_sha256,
       left(dump_sha256, 16)      AS dump_sha256,
       left(dbhash, 16)           AS dbhash,
       left(canonical_sha256, 16) AS vacuum_into_sha256
FROM builds ORDER BY seq;

.print
.print 'Distinct values among the builds:'

SELECT count(DISTINCT file_sha256) FILTER (name <> 'backup')       AS file_sha256_without_backup,
       count(DISTINCT logical_sha256)                              AS logical,
       count(DISTINCT dump_sha256)                                 AS dump,
       count(DISTINCT dbhash)                                      AS dbhash,
       count(DISTINCT canonical_sha256) FILTER (name <> 'backup')  AS vacuum_into_without_backup
FROM builds;

.print
.print 'A.6 The backup after VACUUM INTO, against R0 after VACUUM INTO: the files differ,'
.print 'but at E2 only in the schema cookie (byte 43).'

SELECT max(v.verdict) FILTER (v.level = 'E1') AS e1,
       max(v.verdict) FILTER (v.level = 'E2') AS e2,
       any_value(h.value_a)                    AS cookie_r0,
       any_value(h.value_b)                    AS cookie_backup
FROM verdicts v JOIN header_values h USING (part, subject)
WHERE v.part = 'A' AND v.subject = 'backup' AND v.what LIKE 'canonical%';

.print
.print 'A.7 The header of R0 after VACUUM INTO. Bytes 18-19 = 1,1 mean a rollback'
.print 'journal: VACUUM INTO writes a rollback-mode file, so its logical hash differs'
.print 'from the source only in journal_mode.'

SELECT bytes, byte_18, byte_19, change_counter, schema_cookie FROM canonical_header;

.print
.print '── A2. The same two batchings in rollback mode (journal_mode=DELETE) ───'
.print
.print 'A2.1 One transaction against one transaction per statement. The files differ at E1'
.print 'and at E2 only in header bytes: the change counter (bytes 24-27 and 92-95)'
.print 'counts commits.'

SELECT subject, what, max(verdict) FILTER (level = 'E1') AS e1, max(verdict) FILTER (level = 'E2') AS e2
FROM verdicts WHERE part = 'A2' GROUP BY subject, what;

SELECT field, value_a AS one_transaction, value_b AS one_per_statement
FROM header_values WHERE part = 'A2';

.print
.print '── B. Pairs that differ in content where a weaker hash says equal ──────'
.print
.print 'B.1 Six pairs of databases that differ in content. Our logical hash tells every'
.print 'pair apart; dbhash misses all but integer 1 against real 1.0. (sqldiff is not'
.print 'built by build-tools; the research probe found it reports integer 1 and real'
.print '1.0 as equal: docs/knowledge/determinism-and-rebuild.md, Q4.)'

SELECT pair,
       CASE WHEN ours_equal THEN 'equal' ELSE 'differ' END   AS ours,
       CASE WHEN dbhash_equal THEN 'equal' WHEN NOT dbhash_equal THEN 'differ' END AS dbhash,
       first_difference AS our_first_difference
FROM pairs ORDER BY seq;

.print
.print '── C. Where VACUUM INTO stops being a canonical form ───────────────────'
.print
.print 'C.1 Three cases of two databases with the same content. The sources are compared'
.print 'at E3, their VACUUM INTO outputs at E1 and E2.'

SELECT l.case_name,
       max(v.verdict) FILTER (v.what = 'sources')                                   AS sources_e3,
       max(v.verdict) FILTER (v.what = 'VACUUM INTO outputs' AND v.level = 'E1')     AS outputs_e1,
       max(v.verdict) FILTER (v.what = 'VACUUM INTO outputs' AND v.level = 'E2')     AS outputs_e2
FROM limits l JOIN verdicts v ON v.part = 'C' AND v.subject = l.case_name
GROUP BY l.case_name, l.seq ORDER BY l.seq;

.print
.print 'C.2 The schema cookie of each output, and dbhash of the sources.'

SELECT l.case_name, h.value_a AS cookie_a, h.value_b AS cookie_b,
       CASE WHEN l.dbhash_sources_equal THEN 'equal' WHEN NOT l.dbhash_sources_equal THEN 'differ' END AS dbhash_of_sources
FROM limits l JOIN header_values h ON h.part = 'C' AND h.subject = l.case_name
ORDER BY l.seq;
.print
.print 'C.3 For the first case, the capture records the creation order too: the two'
.print 'captures of the two sources differ, and replaying the second capture and'
.print 'canonicalizing it gives the canonical bytes of the second source.'

SELECT l.case_name,
       CASE WHEN l.captures_equal THEN 'equal' ELSE 'differ' END AS captures_of_the_two_sources,
       v.verdict AS replay_vs_canonical_form_e1
FROM limits l JOIN verdicts v ON v.part = 'C' AND v.subject = l.case_name AND v.what LIKE 'replay%'
ORDER BY l.seq;

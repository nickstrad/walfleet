-- Walkthrough of experiment 9.3. Run it with:
--   go run ./cmd/course run 9.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   original   the undamaged recipe database (one row)
--   sites      one row per single-byte flip: where it is and what it changes
--   verdicts   one row per file checked (the original, then one per flip):
--              which of the four checks caught it, and which were expected to
--   problems   one row per problem line PRAGMA integrity_check or quick_check reported
--   checks     every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.3  What does integrity_check not catch?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The experiment builds the recipe database to one step, then makes four'
.print 'copies, each with one byte flipped in a different place. Every file is'
.print 'checked four ways: PRAGMA integrity_check, PRAGMA quick_check, the E3'
.print 'logical hash (every row and schema object), and the per-page SHA-256'
.print 'manifest of Chapter 2 taken from the original. No check changes the file.'

.print
.print '── A. The undamaged file ───────────────────────────────────────────────'
.print
.print 'A1. The original, and the manifest every copy is compared with:'

SELECT recipe_step, pages, manifest_sha256 FROM original ORDER BY recipe_step;

.print
.print 'A2. Every check passes on it (caught = false everywhere, no problem lines):'

SELECT file, caught_integrity, caught_quick, caught_e3, caught_manifest,
       (SELECT count(*) FROM problems p WHERE p.seq = v.seq) AS problem_lines
FROM verdicts v WHERE seq = 0 ORDER BY seq;

.print
.print '── B. One byte flipped ─────────────────────────────────────────────────'
.print
.print 'B1. The four places. The cell count is B-tree structure; the note and'
.print '    account_id are row data, and only account_id has an index; the'
.print '    freelist page belongs to no table at all.'

SELECT seq, site, location, file_offset, printf('0x%02x', xor_mask) AS xor_mask FROM sites ORDER BY seq;

.print
.print 'B2. Which check caught which flip, measured against expected. Only the'
.print '    page manifest catches all four. integrity_check misses the unindexed'
.print '    column and the freelist page; quick_check catches only the header.'

SELECT site,
       caught_integrity AS integrity_check,
       caught_quick     AS quick_check,
       caught_e3        AS e3_logical,
       caught_manifest  AS manifest,
       (caught_integrity, caught_quick, caught_e3, caught_manifest)
         = (expected_integrity, expected_quick, expected_e3, expected_manifest) AS as_expected
FROM verdicts WHERE seq > 0 ORDER BY seq;

.print
.print 'B3. How much each check reported. The freelist flip changes no row, schema'
.print '    object or pragma, so the E3 hash sees no difference; only the page'
.print '    manifest sees it.'

SELECT v.site,
       count(*) FILTER (p.pragma = 'integrity_check') AS integrity_problems,
       count(*) FILTER (p.pragma = 'quick_check')     AS quick_problems,
       v.e3_differences,
       v.differing_pages                              AS manifest_pages_differ
FROM verdicts v LEFT JOIN problems p USING (seq)
WHERE v.seq > 0
GROUP BY v.seq, v.site, v.e3_differences, v.differing_pages
ORDER BY v.seq;

.print
.print 'B4. What the two pragmas said, problem by problem. integrity_check reads'
.print '    every index against its table, so it finds the row missing from'
.print '    events_by_account; quick_check skips that comparison.'

SELECT v.site, p.pragma, p.problem
FROM problems p JOIN verdicts v USING (seq)
ORDER BY p.seq, p.pragma, p.problem_no;

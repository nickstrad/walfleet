-- Walkthrough of experiment 9.1. Run it with:
--   go run ./cmd/course run 9.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   expected_levels   one row per capture method: the equality levels it should reach
--   quiet_captures    part A: one row per capture of the quiet database, the levels reached
--   backup_header     part A: where the backup API copy's header differs from the source
--   quiet_details     part A: schema cookies and journal modes behind those differences
--   steps             part B: one row per step the writer child ran
--   writer_captures   part B: one row per capture taken while the writer commits
--   backup_steps      part B: one row per sqlite3_backup_step call (run 1 and run 2)
--   summary           one row per method: strongest level, safe under a writer, blocking
--   checks            every claim the experiment asserted (the runner lists it last)
-- Levels: e1 = identical file bytes; e2 = identical pages, ignoring the header
-- bytes SQLite rewrites on every copy (24-27, 40-43, 92-99); e3 = identical
-- logical content (schema, rows, pragmas); e3_except_journal_mode = the same
-- with journal_mode left out; e1_after_vacuum_into = identical bytes once both
-- files are rewritten with VACUUM INTO.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.1  Five ways to capture a live SQLite database'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Five capture methods: a copy of the closed file, the online backup API,'
.print 'VACUUM INTO, reading every page through sqlite_dbpage in one read'
.print 'transaction, and copying the database file and its -wal while a read'
.print 'transaction is held (rebuilt by WAL replay). Each capture is compared with'
.print 'reference copies of the recipe database built to every step (4096-byte pages).'

.print
.print '── A. A quiet database (recipe step 6, no writer) ─────────────────────'
.print
.print 'A1. Each capture against the source. Three methods give the same bytes'
.print '    (E1); the backup API gives the same pages but not the same header;'
.print '    VACUUM INTO rewrites the file, so it matches only when journal_mode is'
.print '    ignored, and byte for byte once the source is vacuumed too.'

SELECT q.method, q.e1, q.e2, q.e3, q.e3_except_journal_mode AS e3_but_jm,
       q.e1_after_vacuum_into AS e1_vacuumed,
       (q.e1, q.e2, q.e3, q.e3_except_journal_mode, q.e1_after_vacuum_into)
         = (e.e1, e.e2, e.e3, e.e3_except_journal_mode, e.e1_after_vacuum_into) AS as_expected
FROM quiet_captures q JOIN expected_levels e USING (method)
ORDER BY q.seq;

.print
.print 'A2. Where the backup copy differs: only in header bytes that E2 ignores.'
.print '    After VACUUM INTO on both sides one byte still differs: byte 43, the'
.print '    low byte of the schema cookie.'

SELECT comparison, equal_ignoring_header_bytes AS e2_equal, ignored_header_bytes, differing_header_bytes
FROM backup_header ORDER BY seq;

.print
.print 'A3. Why. sqlite3_backup_step sets the schema cookie of the copy to the'
.print '    old value plus one (src/backup.c:448), and VACUUM INTO adds one to the'
.print '    cookie it copies (src/vacuum.c:359), so the offset survives. VACUUM INTO'
.print '    also leaves its copy in rollback mode, which is the one E3 difference.'

SELECT name, value FROM quiet_details ORDER BY seq;

.print
.print '── B. The same database while a writer child commits ───────────────────'
.print
.print 'A child process commits recipe steps, one fresh connection per step'
.print '(wal_autocheckpoint=0). The experiment captures between and during its'
.print 'commits:'
.print '  plain copy       a copy of the live database file only (no -wal)'
.print '  pages            every page through sqlite_dbpage in one read transaction,'
.print '                   in two halves; the writer commits between them'
.print '  file + WAL       copy the database file, the writer commits, copy the -wal,'
.print '                   all inside one read transaction; rebuilt by WAL replay'
.print '  VACUUM INTO      run while the writer holds BEGIN IMMEDIATE with a step'
.print '                   not yet committed; the writer commits afterwards'
.print '  backup API       5 pages per step, commits in between (see B3)'
.print
.print 'B1. What the writer did, and during which capture:'

SELECT step, during, action, outcome FROM steps WHERE during NOT LIKE '%every step' ORDER BY step;

.print
.print 'B2. The recipe step each capture equals, beside the newest step the writer'
.print '    had committed when the comparison ran. The plain copy misses commits'
.print '    still in the -wal and equals an older step. Every other method equals'
.print '    the exact step its read transaction or last step saw, at the expected'
.print '    levels (the same as in A1). VACUUM INTO finished while the writer held'
.print '    its write transaction: it reads under a read transaction only'
.print '    (src/vacuum.c:270).'

SELECT w.method, w.latest_commit AS latest, w.equals_step AS equals, w.want_step AS want,
       concat_ws(' ', CASE WHEN w.e1 THEN 'E1' END, CASE WHEN w.e2 THEN 'E2' END,
                 CASE WHEN w.e3 THEN 'E3' END, CASE WHEN w.e3_except_journal_mode THEN 'E3-but-jm' END,
                 CASE WHEN w.e1_after_vacuum_into THEN 'E1-vacuumed' END) AS levels_reached,
       (w.e1, w.e2, w.e3, w.e3_except_journal_mode, w.e1_after_vacuum_into)
         = (e.e1, e.e2, e.e3, e.e3_except_journal_mode, e.e1_after_vacuum_into) AS as_expected
FROM writer_captures w
JOIN expected_levels e ON e.method = CASE WHEN w.want_step IS NULL THEN 'closed-file copy' ELSE w.method END
ORDER BY w.seq;

.print
.print 'B3. The backup API, 5 pages per step (run 1). After steps 2 and 3 the'
.print '    connection being backed up commits: the copy keeps advancing 5 pages a'
.print '    step (sqlite3BackupUpdate, src/backup.c:688-715) although remaining'
.print '    rises as the database grows. After step 5 the writer child commits:'
.print '    the next step starts again from page 1 (sqlite3BackupRestart,'
.print '    src/backup.c:728-734). copied = page_count - remaining.'

SELECT step, remaining, page_count, copied, then_committed_by, committed_step
FROM backup_steps WHERE run = 1 ORDER BY step;

.print
.print 'B4. Run 2: the writer child commits after every step. Every commit'
.print '    restarts the backup, and the database keeps growing, so remaining'
.print '    never reaches 0. The experiment gives up after 8 steps.'

SELECT step, remaining, page_count, then_committed_by, committed_step,
       min(remaining) OVER () AS smallest_remaining
FROM backup_steps WHERE run = 2 ORDER BY step;

.print
.print '── C. Summary ──────────────────────────────────────────────────────────'
.print

SELECT method, strongest_level, safe_under_writer FROM summary ORDER BY seq;

.print
.print 'Whether a method blocks the writer, from what happened in part B:'

SELECT method, blocks_the_writer FROM summary ORDER BY seq;

-- Walkthrough of experiment 4.1. Run it with:
--   go run ./cmd/course run 4.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   parts            what ran and what was skipped (parts B to F need strace, so Linux)
--   header_modes     part A: journal_mode and header bytes 18-19 of a new file, by stage
--   creations        part A: one traced statement of a new file, with its -journal and -wal writes
--   creation_calls   part A: one -journal syscall of such a statement
--   steps            the recipe steps whose commit is traced (3, and 1 for part D2)
--   states           the pre-step and post-step file of each traced step
--   journals         a -journal decoded in Go: part C (open transaction), E (crash), F (after commit)
--   journal_records  part C: one page record of the open transaction's -journal
--   live_rollback    part C: the file after ROLLBACK and close
--   commits          one traced commit: its syscall sequence, page writes and syncs
--   commit_calls     one syscall of a traced commit, on the database directory
--   twins            part D: the C CLI's writes and syncs against the Go driver's
--   twin_diffs       part D: a position where they differ (empty when they agree)
--   kills            one child killed on entry to one syscall: the files it left, and the reopen
--   crashes          part E: the two crashes whose -journal is then lost
--   reopens          part E: one database opened after a crash, or the pre/post-step file
--   commit_points    part F: the commit-point syscall of each journal mode
--   checks           every claim the experiment asserted (the runner lists it last)
-- Run-dependent values (sync syscall names, timings, journal nonces) are in
-- info and info_kills, shown by info.sql.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  4.1  Atomic commit in rollback-journal mode, and the hot journal'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A rollback-journal commit of recipe step 3 (seed 1) is traced with strace,'
.print 'then rerun and killed with SIGKILL on entry to each of its writes, syncs'
.print 'and unlinks, at synchronous=FULL, NORMAL and OFF. After each kill Go reads'
.print 'the database and -journal as bytes, then SQLite reopens the database.'
.print
.print 'What ran:'

SELECT part, what, status FROM parts ORDER BY part, what;

.print
.print '── A. Where a WAL-default platform meets rollback mode ─────────────────'
.print
.print 'A1. A new file starts in rollback mode (header bytes 18 and 19 = 1) until'
.print '    PRAGMA journal_mode=wal rewrites them to 2.'

SELECT stage, journal_mode, byte_18, byte_19 FROM header_modes ORDER BY seq;

.print
.print 'A2. Each statement traced in a fresh process. The switch to WAL, and the'
.print '    first commit of a WAL file opened with sqlitex.Open, still go through'
.print '    a -journal; the statements after them write only to the -wal.'

SELECT c.opener, c.statement,
       string_agg(split_part(k.call, ' ', 1), ' ' ORDER BY k.n) AS journal_syscalls,
       c.wal_writes
FROM creations c LEFT JOIN creation_calls k ON k.creation_seq = c.seq
GROUP BY c.seq, c.opener, c.statement, c.wal_writes
ORDER BY c.seq;

.print
.print 'A3. The -journal syscalls of each of those rollback-mode commits:'

SELECT c.statement, k.n, k.call
FROM creations c JOIN creation_calls k ON k.creation_seq = c.seq
ORDER BY c.seq, k.n;

.print
.print '── B. The recipe step ──────────────────────────────────────────────────'
.print
.print 'B1. Step 3 of the recipe, the transaction this walkthrough commits:'

SELECT step, recipe, schema, inserts, updates, deletes, indexes FROM steps WHERE step = 3;

.print
.print 'B2. The file before (after recipe step 2) and after it. Page 22 is a'
.print '    freelist leaf in the pre-step file; remember it for part D.'

SELECT state, recipe_step, pages, sha256_prefix, freelist_leaves
FROM states WHERE step = 3 ORDER BY state DESC;

.print
.print '── C. The journal of an open transaction, read before COMMIT ──────────'
.print
.print 'C1. Every statement of step 3 has run; COMMIT is not issued. The header'
.print '    is written but magic and record count are still zero, so the journal'
.print '    is not hot. It records the pre-transaction size: 29 pages.'

SELECT journal_bytes AS bytes, printf('0x%02x', first_byte) AS first_byte, hot, header,
       record_count AS count, initial_pages AS init_pages, sector_size AS sector, page_size AS page, records,
       checksum_failures AS bad_sums, trailing_bytes AS partial
FROM journals WHERE part = 'C';

.print
.print 'C2. Each record is a page number, the original image of the page and a checksum.'
.print '    Page 22, the reused freelist leaf, is not among them: SQLite fetches it'
.print '    with PAGER_GET_NOCONTENT and never journals it.'

SELECT file_offset, page, role, checksum, same_as_pre_step FROM journal_records ORDER BY seq;

.print
.print 'C3. ROLLBACK by the live connection, then close:'

SELECT file_after, journal_left FROM live_rollback;

.print
.print '── D. Step 3: one commit traced, then killed at every write and sync ──'
.print
.print 'D1. The synchronous=FULL commit: syscalls of the writer thread on the database'
.print '    directory, between the start of the step and its return. The journal'
.print '    records come first, then two journal syncs around the record count,'
.print '    then the database pages, one database sync, and the unlink.'

SELECT n, kind, call
FROM commit_calls
WHERE commit_seq = (SELECT seq FROM commits WHERE part = 'D' AND synchronous = 'FULL')
ORDER BY n;

.print
.print 'D2. The same commit at NORMAL and OFF is identical up to the last journal'
.print '    record. From there on: NORMAL writes the record count before a single'
.print '    journal sync; OFF syncs nothing.'

SELECT c.synchronous, k.n, k.kind, k.call
FROM commits c JOIN commit_calls k ON k.commit_seq = c.seq
WHERE c.part = 'D' AND c.synchronous IN ('NORMAL', 'OFF')
  AND k.n > (SELECT max(n) FROM commit_calls WHERE commit_seq = c.seq AND kind = 'journal-record')
ORDER BY c.seq, k.n;

.print
.print 'D3. The three sequences side by side (other syscalls left out, repeats folded):'

WITH s AS (
  SELECT synchronous, unnest(string_split(sequence, ', ')) AS what,
         generate_subscripts(string_split(sequence, ', '), 1) AS i
  FROM commits WHERE part = 'D')
SELECT i,
       max(what) FILTER (synchronous = 'FULL')   AS "FULL",
       max(what) FILTER (synchronous = 'NORMAL') AS "NORMAL",
       max(what) FILTER (synchronous = 'OFF')    AS "OFF"
FROM s GROUP BY i ORDER BY i;

SELECT synchronous, journaled_pages AS pages, record_writes, db_page_writes AS db_writes, syncs, sync_targets
FROM commits WHERE part = 'D' ORDER BY seq;

.print
.print 'D4. The pinned C sqlite3 CLI runs the same step as literal SQL. Its writes,'
.print '    syncs and unlink equal those of the Go driver in file, offset, length, order.'

SELECT synchronous, go_calls, cli_calls, identical, cli_leaves_post_step
FROM twins ORDER BY go_calls DESC;

.print
.print 'D5. Kill points: every pwrite64, sync and unlink of the commit, then the'
.print '    first syscall after the step returned.'

SELECT synchronous, count(*) AS kill_points
FROM kills WHERE part = 'D' GROUP BY synchronous ORDER BY kill_points DESC;

.print
.print 'D6. synchronous=FULL: what each kill left on disk. Until the record count is'
.print '    written and synced (kill 26) the journal is not hot and the database'
.print '    file is untouched. From kill 28 on, the database is torn mid-write.'
.print '    chg_pre and chg_post count the pages of the database file that differ'
.print '    from the pre-step and post-step files.'

SELECT kill_no AS kill, syscall, differ_pre AS chg_pre, differ_post AS chg_post,
       journal_bytes AS bytes, hot, header, records AS recs, trailing_bytes AS partial
FROM kills WHERE part = 'D' AND synchronous = 'FULL' ORDER BY kill_no;

.print
.print 'D7. synchronous=FULL: what SQLite did on reopen. A hot journal is played'
.print '    back and the file truncated to its 29 original pages; the result is the'
.print '    pre-step state (B), or pre-step except page 22 (b). After the unlink,'
.print '    the reopened file is the post-step state (A).'

SELECT kill_no AS kill, playback, playback_pages, truncate_to AS trunc,
       outcome, outcome_pages AS differs_in, integrity,
       regexp_replace(journal_after, 'cold -journal left \((\d+) bytes, first byte (0x..)\)', 'cold, \1 bytes, \2') AS journal_after
FROM kills WHERE part = 'D' AND synchronous = 'FULL' ORDER BY kill_no;

.print
.print 'b means the reopened file equals pre-step except in reused freelist leaves'
.print '(page 22): the rollback cannot restore their unused bytes because they were'
.print 'never journaled, and VACUUM INTO of the file equals that of the pre-step file.'
.print
.print 'D8. synchronous=NORMAL, every kill: the record count is written before the'
.print '    single journal sync, so the journal is hot from kill 24 on.'

SELECT kill_no AS kill, syscall, differ_pre AS chg_pre, differ_post AS chg_post,
       journal_bytes AS bytes, hot, header, outcome
FROM kills WHERE part = 'D' AND synchronous = 'NORMAL' ORDER BY kill_no;

.print
.print 'D9. synchronous=OFF, every kill: the header is written at once with its magic'
.print '    and record count 0xffffffff (no-sync: read to the end of the file), so'
.print '    the journal is hot from the first record on.'

SELECT kill_no AS kill, syscall, differ_pre AS chg_pre, differ_post AS chg_post,
       journal_bytes AS bytes, hot, header, printf('0x%x', record_count) AS count, outcome
FROM kills WHERE part = 'D' AND synchronous = 'OFF' ORDER BY kill_no;

.print
.print 'D10. The outcome of every kill, in commit order. B = pre-step exact bytes,'
.print '     b = pre-step except reused freelist leaves, A = post-step exact bytes,'
.print '     ? = anything else. The last B or b is the kill on entry to the unlink.'

SELECT synchronous,
       string_agg(outcome, '' ORDER BY kill_no) AS outcome_per_kill_point
FROM kills WHERE part = 'D' GROUP BY synchronous ORDER BY count(*) DESC;

SELECT synchronous,
       count(*) FILTER (outcome = 'B') AS "B pre exact",
       count(*) FILTER (outcome = 'b') AS "b pre logical",
       count(*) FILTER (outcome = 'A') AS "A post exact",
       count(*) FILTER (outcome = '?') AS "? other",
       count(*) FILTER (NOT landed_ok) AS missed_kills,
       count(*) FILTER (integrity <> 'ok') AS integrity_bad,
       count(*) FILTER (covered = false) AS not_covered
FROM kills WHERE part = 'D' GROUP BY synchronous ORDER BY count(*) DESC;

.print
.print '── D2. A step whose pre-step file has no freelist: step 1 ─────────────'
.print
.print 'Step 1 starts from the file after step 0, which has no freelist, so there'
.print 'is no b: every reopen gives the pre-step or post-step bytes exactly.'

SELECT step, recipe, schema, inserts, updates, deletes, indexes FROM steps WHERE step = 1;

SELECT state, recipe_step, pages, sha256_prefix, freelist_leaves
FROM states WHERE step = 1 ORDER BY state DESC;

SELECT synchronous, journaled_pages AS pages, record_writes, db_page_writes AS db_writes, syncs, sync_targets
FROM commits WHERE part = 'D2' ORDER BY seq;

WITH s AS (
  SELECT synchronous, unnest(string_split(sequence, ', ')) AS what,
         generate_subscripts(string_split(sequence, ', '), 1) AS i
  FROM commits WHERE part = 'D2')
SELECT i,
       max(what) FILTER (synchronous = 'FULL')   AS "FULL",
       max(what) FILTER (synchronous = 'NORMAL') AS "NORMAL",
       max(what) FILTER (synchronous = 'OFF')    AS "OFF"
FROM s GROUP BY i ORDER BY i;

SELECT synchronous,
       string_agg(outcome, '' ORDER BY kill_no) AS outcome_per_kill_point
FROM kills WHERE part = 'D2' GROUP BY synchronous ORDER BY count(*) DESC;

SELECT synchronous,
       count(*) FILTER (outcome = 'B') AS "B pre exact",
       count(*) FILTER (outcome = 'b') AS "b pre logical",
       count(*) FILTER (outcome = 'A') AS "A post exact",
       count(*) FILTER (outcome = '?') AS "? other",
       count(*) FILTER (NOT landed_ok) AS missed_kills,
       count(*) FILTER (integrity <> 'ok') AS integrity_bad
FROM kills WHERE part = 'D2' GROUP BY synchronous ORDER BY count(*) DESC;

.print
.print '── E. Losing the hot journal (step 3, synchronous=FULL) ───────────────'
.print
.print 'E1. Two crashes: in the middle of the database writes, and on entry to the'
.print '    database sync, when every page is written but the journal is still hot.'

SELECT crash, killed_on, db_write_no AS write_no, db_writes AS writes, db_file,
       db_pages AS pages, differ_pre AS chg_pre, differ_post AS chg_post
FROM crashes ORDER BY seq;

.print
.print 'The hot journal beside the mid-write crash:'

SELECT journal_bytes, printf('0x%02x', first_byte) AS first_byte, hot, header, record_count, initial_pages, records, record_pages
FROM journals WHERE part = 'E';

.print
.print 'E2. What each file holds when opened. Without its journal the torn file is'
.print '    unreadable, and a copy of the file alone is the same damage. Copied with'
.print '    its journal, it rolls back. Deleting the journal after the last database'
.print '    write commits a transaction whose COMMIT never returned.'

SELECT scenario, integrity, user_version, accounts, documents, events
FROM reopens ORDER BY seq;

SELECT scenario, outcome_name AS state, outcome_pages AS differs_in
FROM reopens WHERE outcome_name IS NOT NULL ORDER BY seq;

SELECT scenario, open_error FROM reopens WHERE open_error IS NOT NULL ORDER BY seq;

.print
.print '── F. The commit point in DELETE, TRUNCATE and PERSIST modes ──────────'
.print
.print 'F1. The same commit (step 3, synchronous=FULL) in each mode. Only the end'
.print '    differs: TRUNCATE and PERSIST finish the journal and sync it again.'

WITH s AS (
  SELECT journal_mode, unnest(string_split(sequence, ', ')) AS what,
         generate_subscripts(string_split(sequence, ', '), 1) AS i
  FROM commits WHERE part = 'F')
SELECT i,
       max(what) FILTER (journal_mode = 'delete')   AS "delete",
       max(what) FILTER (journal_mode = 'truncate') AS "truncate",
       max(what) FILTER (journal_mode = 'persist')  AS "persist"
FROM s GROUP BY i ORDER BY i;

SELECT journal_mode, journaled_pages AS pages, record_writes, db_page_writes AS db_writes, syncs, sync_targets
FROM commits WHERE part = 'F' ORDER BY seq;

.print
.print 'F2. A kill on entry to the commit-point syscall gives the pre-step state; a'
.print '    kill on the syscall after it gives the post-step state.'

SELECT journal_mode, commit_point, killed_on_entry, next_kind, killed_on_next
FROM commit_points ORDER BY seq;

.print
.print 'F3. The -journal left beside the database after a clean commit. TRUNCATE'
.print '    leaves 0 bytes; PERSIST leaves the whole journal with a zeroed header.'
.print '    Neither is hot.'

SELECT label AS journal_mode,
       coalesce(journal_bytes::VARCHAR, 'no -journal') AS journal_bytes,
       printf('0x%02x', first_byte) AS first_byte, hot, header
FROM journals WHERE part = 'F' ORDER BY seq;

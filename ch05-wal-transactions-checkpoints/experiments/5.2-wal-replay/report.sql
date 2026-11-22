-- Walkthrough of experiment 5.2. Run it with:
--   go run ./cmd/course run 5.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   captures        one row per live copy a child process took (database and -wal sizes)
--   recipe_matches  one row per file looked up among the recipe step hashes
--   replays         one row per replay of a captured -wal in plain Go (no SQLite)
--   sqlite_runs     one row per time SQLite opened or checkpointed files
--   comparisons     one row per byte comparison, with the replay opened by SQLite
--   wal_states      one row per look at the live -wal of part B and C
--   stale_shm       one row: the -shm of part A that part B hands to SQLite
--   checks          every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.2  Is a base database plus the committed WAL frames the whole state?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A plain Go replayer applies the frames of a WAL to a base database file up'
.print 'to the last commit frame, with no SQLite. Its output is compared byte for'
.print 'byte (sha256) with what SQLite itself produces from the same files.'
.print '(The tables show the first 16 hex digits of each sha256; the CSV keeps all.)'
.print 'Every capture is taken by a child process while the writer connection is'
.print 'still open, so the files are exactly what a crash would leave.'

.print
.print '── A. A quiet moment ──────────────────────────────────────────────────'
.print
.print 'Steps 1-8 are committed on one connection (wal_autocheckpoint=0); nothing'
.print 'is in flight.'
.print
.print 'A1. The copy: a small database file and a large -wal.'

SELECT capture, db_bytes, wal_bytes FROM captures WHERE part = 'A' ORDER BY seq;

.print
.print 'A2. The database file alone is byte for byte recipe step 0 (exact = true):'
.print '    every change committed since lives only in the -wal.'

SELECT subject, step, exact FROM recipe_matches WHERE part = 'A' AND subject = 'captured database file alone';

.print
.print 'A3. The replay: all frames are committed, nothing is ignored, and the output'
.print '    is the size of the database at the last commit.'

SELECT frames_in_file, valid_frames, transactions, last_commit_frame, database_pages,
       applied_frames, distinct_pages
FROM replays WHERE part = 'A' ORDER BY seq;

SELECT skipped_beyond_size, base_bytes, output_bytes, ignored_frames, partial_bytes
FROM replays WHERE part = 'A' ORDER BY seq;

.print
.print 'A4. SQLite on the original: a TRUNCATE checkpoint, then close. Then the'
.print '    replay against that file, and the replay opened by SQLite.'

SELECT run, busy, log_frames, checkpointed FROM sqlite_runs WHERE part = 'A' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'A' ORDER BY seq;

SELECT user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'A' ORDER BY seq;

.print
.print 'A5. The recipe lookup of the replay: step 8, equal except the change counter'
.print '    (bytes 24-27 and 92-95) that one long-lived connection leaves behind;'
.print '    exact = false means that masked match.'

SELECT subject, step, exact FROM recipe_matches WHERE part = 'A' AND subject = 'our replay';

.print
.print '── B. Mid-transaction ─────────────────────────────────────────────────'
.print
.print 'Steps 9-10 are committed. Then UPDATE documents SET title = upper(title)'
.print 'is left open with cache_size=10, so dirty pages spill into the -wal before'
.print 'any commit.'
.print
.print 'B1. The -wal after the commits, and as captured while the transaction is'
.print '    open. The frames after the last commit frame are spilled: valid'
.print '    (checksums and salts pass) but uncommitted.'

SELECT moment, wal_bytes, frames, valid_frames, last_commit_frame,
       frames - last_commit_frame AS spilled_frames
FROM wal_states WHERE part = 'B' ORDER BY seq;

SELECT capture, db_bytes, wal_bytes FROM captures WHERE part = 'B' ORDER BY seq;

.print
.print 'B2. The replay applies frames up to the last commit frame and ignores every'
.print '    valid spilled frame after it, because no commit frame follows.'

SELECT frames_in_file, valid_frames, transactions, last_commit_frame, database_pages,
       applied_frames, distinct_pages
FROM replays WHERE part = 'B' ORDER BY seq;

SELECT skipped_beyond_size, base_bytes, output_bytes, ignored_frames, partial_bytes
FROM replays WHERE part = 'B' ORDER BY seq;

SELECT capture, coalesce(ignored, 'none') AS ignored FROM replays WHERE part = 'B' ORDER BY seq;

.print
.print 'B3. SQLite opens a copy of the same captured files (no -shm), recovers,'
.print '    and checkpoints with TRUNCATE. Our replay equals the result byte for'
.print '    byte, and SQLite opens the replay as recipe step 10.'

SELECT run, shm, user_version, busy, log_frames, checkpointed
FROM sqlite_runs WHERE part = 'B' AND shm = 'none' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'B' AND against LIKE '%recovery%' ORDER BY seq;

SELECT user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'B' AND against LIKE '%recovery%' ORDER BY seq;

.print
.print 'B4. The same again, but the copy also gets a stale -shm taken from the part'
.print '    A database: another file, other salts, mxFrame 155. SQLite does not trust'
.print '    it; it rebuilds the wal-index from the -wal. The result is the same.'

SELECT s.mx_frame AS stale_mx_frame, s.n_page AS stale_n_page,
       (SELECT frames_in_file FROM replays WHERE part = 'A') AS part_a_frames
FROM stale_shm s;

SELECT run, shm, user_version, busy, log_frames, checkpointed
FROM sqlite_runs WHERE part = 'B' AND shm <> 'none' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'B' AND against LIKE '%stale%' ORDER BY seq;

SELECT user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'B' AND against LIKE '%stale%' ORDER BY seq;

.print
.print '── C. After a rollback ────────────────────────────────────────────────'
.print
.print 'The open transaction rolls back. Its frames stay in the -wal file; only the'
.print 'wal-index forgets them (mx_frame is back at the last commit frame).'
.print
.print 'C1. The -wal after ROLLBACK against the one captured in part B: the size is'
.print '    unchanged.'

SELECT moment, wal_bytes, frames, valid_frames, last_commit_frame, mx_frame
FROM wal_states WHERE part IN ('B', 'C') AND moment <> 'after the commits' ORDER BY seq;

.print
.print 'C2. A new capture after the rollback, and its replay: the same frames are'
.print '    ignored, because no commit frame follows them.'

SELECT capture, db_bytes, wal_bytes FROM captures WHERE part = 'C' ORDER BY seq;

SELECT frames_in_file, valid_frames, transactions, last_commit_frame, database_pages,
       applied_frames, distinct_pages
FROM replays WHERE part = 'C' ORDER BY seq;

SELECT skipped_beyond_size, base_bytes, output_bytes, ignored_frames, partial_bytes
FROM replays WHERE part = 'C' ORDER BY seq;

SELECT capture, coalesce(ignored, 'none') AS ignored FROM replays WHERE part = 'C' ORDER BY seq;

.print
.print 'C3. SQLite on the original: TRUNCATE checkpoint, then close. Our replay'
.print '    against it, and the two replays (open transaction, after rollback)'
.print '    against each other.'

SELECT run, busy, log_frames, checkpointed FROM sqlite_runs WHERE part = 'C' ORDER BY seq;

SELECT against, level, equal, left(sha256, 16) AS sha256_16
FROM comparisons WHERE part = 'C' ORDER BY seq;

SELECT against, user_version, integrity_check, unchanged_by_open
FROM comparisons WHERE part = 'C' AND user_version IS NOT NULL ORDER BY seq;

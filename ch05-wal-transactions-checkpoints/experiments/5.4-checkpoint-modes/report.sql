-- Walkthrough of experiment 5.4. Run it with:
--   go run ./cmd/course run 5.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   cases         one row per (mode, reader) case: the checkpoint result and the state right after it
--   next_writes   the same cases after the reader ended and the writer committed step 8
--   own_truncate  two rows: the writer runs TRUNCATE itself, then commits step 8
--   checks        every claim the experiment asserted (the runner lists it last)
-- file_salt1, shm_salt1 and checkpoint_seq are changes against the state before
-- the checkpoint: same, +1, new value, or (no header) for an empty -wal.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.4  What do PASSIVE, FULL, RESTART and TRUNCATE do with readers around?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Each of the 12 cases starts from a fresh copy of recipe step 5 (a closed'
.print 'file). A writer commits step 6 and step 7 with wal_autocheckpoint=0.'
.print '"reader behind" begins its read transaction between the two commits,'
.print '"reader current" after both. A third connection (busy_timeout=300ms) runs'
.print 'the checkpoint; then the reader ends and the writer commits step 8.'
.print 'The WAL file header is read from a child process copy, the wal-index from'
.print 'the -shm.'

.print
.print '── A. Four modes against three reader states ──────────────────────────'
.print
.print 'A1. Frames before every checkpoint, and the snapshot of the reader behind:'

SELECT min(frames_before)         AS frames_before_every_checkpoint,
       count(DISTINCT frames_before) AS distinct_values,
       min(reader_frame)             AS reader_behind_snapshot_frame
FROM cases;

.print
.print 'A2. The result (busy, log, checkpointed) and the state right after it.'
.print '    Without a reader every mode copies all 38 frames; a reader behind caps'
.print '    the copy at its frame (22); busy=1 reports a checkpoint that could not'
.print '    finish its mode. TRUNCATE with no reader empties the -wal and restarts'
.print '    the wal-index at once (shm salt-1 +1, mxFrame 0); the other modes leave'
.print '    salts and size alone, so the restart waits for the next write.'

SELECT mode, reader, busy, log_frames AS log, checkpointed, wal_bytes, mx_frame, n_backfill, file_salt1, shm_salt1
FROM cases ORDER BY seq;

.print
.print 'A3. After the reader ended and the writer committed step 8. The WAL'
.print '    restarted exactly when the checkpoint had copied every frame (or TRUNCATE'
.print '    emptied it): salt-1 and the checkpoint sequence advance by 1 and the file'
.print '    is reused. Otherwise the writer appends with the same salts (valid frames'
.print '    = frames in file). After the TRUNCATE of another connection the writer starts'
.print '    with new random salts and its own checkpoint sequence, which is per'
.print '    connection (row TRUNCATE / no reader).'

SELECT mode, reader, restarted, file_salt1, checkpoint_seq, wal_bytes, mx_frame,
       valid_frames || ' / ' || frames_in_file AS valid_frames_of_file
FROM next_writes ORDER BY seq;

.print
.print '── B. The writer runs TRUNCATE itself ─────────────────────────────────'
.print
.print 'B1. The same TRUNCATE with no reader, but on the writer connection. Its'
.print '    own checkpoint bumps its counter, so its next commit keeps salt-1 +1'
.print '    and writes checkpoint sequence 1 instead of new random salts.'

SELECT moment, busy, log_frames AS log, checkpointed, wal_bytes, mx_frame, shm_salt1, file_salt1,
       checkpoint_seq_before || ' -> ' || checkpoint_seq_after AS checkpoint_seq
FROM own_truncate ORDER BY seq;

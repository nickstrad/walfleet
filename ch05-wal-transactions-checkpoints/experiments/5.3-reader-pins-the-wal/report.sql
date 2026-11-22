-- Walkthrough of experiment 5.3. Run it with:
--   go run ./cmd/course run 5.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   samples      one row per look at the live -wal and its wal-index, parts A and B
--   checkpoints  one row per wal_checkpoint(PASSIVE) the experiment ran
--   checks       every claim the experiment asserted (the runner lists it last)
-- In samples, reader_rows and writer_rows are blank where that connection was
-- not queried; salt1_advance is salt-1 minus the salt-1 of the first sample of
-- the part (the salts themselves are random).

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.3  How does an open reader stop checkpoints and grow the WAL?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A writer commits recipe steps with wal_autocheckpoint=200 while a reader'
.print 'holds a read transaction open. After every commit the experiment reads'
.print 'the live -wal and the wal-index (-shm) from a child process and counts'
.print 'the rows the reader and the writer see. The read marks column is the five'
.print 'wal-index read marks [0..4]; "unused" is a slot nobody holds.'

.print
.print '── A. A reader that holds a read mark in slot 1 ───────────────────────'
.print
.print 'The reader begins after step 6 (mxFrame 22, nothing checkpointed yet).'
.print
.print 'A1. The reader takes read mark 22 in slot 1 and sees 251 rows for the'
.print '    whole run; the writer moves on to 1177 rows. The WAL grows on every'
.print '    commit and never restarts. Autocheckpoint runs, but n_backfill stops'
.print '    at the reader mark (22) and stays there.'

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks, reader_rows, writer_rows
FROM samples WHERE part = 'A' ORDER BY seq;

.print
.print 'A2. Summary of the loop (steps 7-30):'

WITH s AS (SELECT * FROM samples WHERE part = 'A'),
     mark AS (SELECT mx_frame AS m FROM s WHERE moment = 'reader began'),
     loop AS (SELECT * FROM s WHERE step BETWEEN 7 AND 30)
SELECT (SELECT m FROM mark)                                           AS reader_mark,
       (SELECT min(step) FROM loop WHERE n_backfill = (SELECT m FROM mark)) AS mark_reached_at_step,
       max(mx_frame)                                                  AS final_mx_frame,
       max(mx_frame) - max(n_backfill)                                AS frames_not_checkpointed,
       count(DISTINCT reader_rows)                                    AS reader_row_counts,
       min(salt1_advance) || '..' || max(salt1_advance)               AS salt1_advance
FROM loop;

.print
.print 'A3. The reader ends. Its read mark stays written in slot 1 until a'
.print '    checkpoint, so n_backfill is still 22 (row "reader ended"). One PASSIVE'
.print '    checkpoint now brings n_backfill to mxFrame; the WAL file keeps its size.'

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks
FROM samples WHERE part = 'A' AND moment IN ('reader ended', 'PASSIVE checkpoint') ORDER BY seq;

SELECT moment, busy, log_frames, checkpointed FROM checkpoints WHERE part = 'A' ORDER BY seq;

.print
.print 'A4. The next write (step 31) restarts the WAL at frame 1: salt-1 advances'
.print '    by 1 and the checkpoint sequence by 1. The file is reused, not shrunk;'
.print '    only valid_frames of the frames_in_file belong to the new generation.'

SELECT moment, wal_bytes, mx_frame, checkpoint_seq, salt1_advance,
       valid_frames, frames_in_file
FROM samples WHERE part = 'A' AND moment IN ('reader began', 'PASSIVE checkpoint', 'step 31 committed')
ORDER BY seq;

.print
.print '── B. A reader on read lock 0 ─────────────────────────────────────────'
.print
.print 'A PASSIVE checkpoint after step 6 copies every frame, so the reader that'
.print 'begins next finds n_backfill = mxFrame and takes read lock 0: it reads the'
.print 'database file and ignores the WAL.'
.print
.print 'B1. The checkpoint, and the reader that follows it. The read marks do not'
.print '    change: the reader took no mark in slots 1..4.'

SELECT moment, busy, log_frames, checkpointed FROM checkpoints WHERE part = 'B' ORDER BY seq;

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks, reader_rows, writer_rows
FROM samples WHERE part = 'B' AND moment IN ('checkpointed', 'reader began') ORDER BY seq;

.print
.print 'B2. The writer is free to restart the WAL (salt-1 advances by 1 and mxFrame'
.print '    falls back to 16 on step 7), but no checkpoint can copy one frame while'
.print '    the reader is open: n_backfill stays 0 although autocheckpoint ran on'
.print '    every commit past 200 frames.'

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks, reader_rows, writer_rows
FROM samples WHERE part = 'B' AND step > 6 ORDER BY seq;

.print
.print 'B3. Summary of the loop (steps 7-30):'

SELECT min(CASE WHEN step = 7 THEN salt1_advance END)  AS salt1_advance_at_step_7,
       min(CASE WHEN step = 7 THEN mx_frame END)       AS mx_frame_at_step_7,
       max(mx_frame)                                   AS final_mx_frame,
       max(n_backfill)                                 AS final_n_backfill,
       count(DISTINCT reader_rows)                     AS reader_row_counts
FROM samples WHERE part = 'B' AND step BETWEEN 7 AND 30;

.print
.print 'B4. The reader ends and one PASSIVE checkpoint copies everything.'

SELECT moment, busy, log_frames, checkpointed FROM checkpoints WHERE part = 'B' AND moment LIKE '%reader ended' ORDER BY seq;

SELECT moment, wal_bytes, mx_frame, n_backfill, read_marks
FROM samples WHERE part = 'B' AND moment = 'reader ended + PASSIVE' ORDER BY seq;

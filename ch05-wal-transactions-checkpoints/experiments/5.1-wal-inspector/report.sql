-- Walkthrough of experiment 5.1. Run it with:
--   go run ./cmd/course run 5.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   wal_header       one row: the WAL header and size of run 1 (recipe steps 1-5)
--   frames           one row per frame of run 1: page, commit size, first 16 hex digits of the page sha256
--   commits          one row per commit frame (one per recipe step)
--   noop_checkpoint  one row: PRAGMA wal_checkpoint(NOOP) on the open connection
--   wal_index        one row: the -shm copy (wal-index) of run 1
--   showwal          one row: the cross-check against SQLite's showwal utility (absent when skipped)
--   regions          one row per region of the WAL file: do run 1 and run 2 differ there
--   sequences        one row per further run: is its frame sequence equal to run 1
--   checks           every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  5.1  What is inside a live WAL, and what differs between two of them?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Recipe steps 1-5 are committed on one connection with wal_autocheckpoint=0,'
.print 'synchronous=FULL. While that connection is still open, a child process'
.print 'copies the database, the -wal and the -shm. Plain Go parses both sidecar'
.print 'files and checks them against PRAGMA wal_checkpoint(NOOP) and showwal.'
.print 'Base: recipe step 0, built and closed before the five steps.'

.print
.print '── A. One WAL holding recipe steps 1-5 ─────────────────────────────────'
.print
.print 'A1. The header and the size. The file is a 32-byte header plus whole frames'
.print '    of a 24-byte frame header and one page; the arithmetic must match.'

SELECT printf('0x%08x', magic) AS magic, checksum_order, version, page_size, checkpoint_seq, wal_bytes,
       header_bytes + frames * (frame_header_bytes + page_size) AS computed_bytes,
       partial_bytes
FROM wal_header;

.print
.print 'A2. The checksum chain: every frame carries the header salts and continues'
.print '    the running checksum, so all frames verify.'

SELECT frames, valid_frames, stop_reason FROM wal_header;

.print
.print 'A3. The frames in order, with the commit they belong to. Only the last frame'
.print '    of a transaction has a non-zero commit size, and it equals the database'
.print '    size in pages at that point. Later transactions rewrite early pages'
.print '    such as 1 and 2; an identical sha256 means the same page image.'

SELECT f.frame_no, f.page, f.commit_size, f.sha256_16,
       (SELECT min(c.commit_no) FROM commits c WHERE c.last_frame >= f.frame_no) AS commit_no,
       CASE WHEN f.commit_size > 0 THEN '<- commit' ELSE '' END AS marker
FROM frames f
ORDER BY f.frame_no;

.print
.print 'A4. The transactions. The commit size equals PRAGMA page_count after the step.'

SELECT *, commit_size = page_count AS sizes_match FROM commits ORDER BY commit_no;

.print
.print 'A5. PRAGMA wal_checkpoint(NOOP) on the open connection takes no locks and'
.print '    reports the wal-index counts: log is the frame count of the WAL, and nothing'
.print '    is checkpointed.'

SELECT n.busy, n.log_frames, n.checkpointed, (SELECT count(*) FROM frames) AS frames_in_wal
FROM noop_checkpoint n;

.print
.print 'A6. The wal-index (the -shm copy): both header copies agree, the checksum'
.print '    is good, mxFrame is the last commit frame, nPage the database size, and'
.print '    its page-number array maps every frame to the page the WAL holds.'

SELECT bytes, byte_order, headers_agree, checksum_ok, page_size
FROM wal_index;

.print
.print '    What the header says, against the WAL itself:'

SELECT w.mx_frame, w.n_page, w.n_backfill, w.salts_equal_wal,
       w.page_array_matches, w.frames AS array_frames
FROM wal_index w;

.print
.print 'A7. showwal (built from the pinned tree) against the Go parser: they agree on'
.print '    every frame page, commit size, salts and checksum, so disagreements is 0.'

SELECT frames_listed, disagreements, problems FROM showwal;

.print
.print '── B. The same workload again: what differs between two WALs ──────────'
.print
.print 'B1. Where run 1 and run 2 differ, by region of the file. Only the salts'
.print '    and the checksums that depend on them: the page data is the same.'
.print '    (The salts themselves are random; see the info report.)'

SELECT region, CASE WHEN differs THEN 'differs' ELSE 'same' END AS result
FROM regions
ORDER BY seq;

.print
.print 'B2. The two runs wrote the same frames: the sequence of (page, page sha256,'
.print '    commit size) is equal.'

SELECT run, synchronous, wal_bytes, frames, commits, same_sequence, first_difference
FROM sequences WHERE part = 'B';

.print
.print '── C. Other synchronous levels write the same frames ──────────────────'
.print
.print 'C1. synchronous changes when SQLite syncs, not which frames it writes.'
.print '    first_difference is the first frame that differs; 0 means none.'

SELECT run, synchronous, wal_bytes, frames, commits, same_sequence, first_difference
FROM sequences WHERE part = 'C' ORDER BY run;

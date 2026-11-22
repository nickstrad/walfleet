-- Walkthrough of experiment 9.2. Run it with:
--   go run ./cmd/course run 9.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   runs         part B: one row per uninterrupted writer run (one per variant),
--                with what a process crash at its end recovers
--   commits      part B: one row per acknowledged commit of each run: where it
--                ends in the -wal and which WAL generation it is in (1, 2, ...)
--   sync_rules   part B: per variant, the bytes SQLite's sync rules say were synced
--   syncs        part B: per variant and file, what strace recorded (Linux only)
--   power_cuts   part B: one row per power-cut model: files cut back to the last sync
--   kill_plans   part A: per synchronous level, the calls an uninterrupted writer made
--   kills        part A: one row per writer killed at a chosen syscall (Linux only)
--   skipped      one row per LINUX-ONLY notice (empty on Linux)
--   checks       every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.2  Crash versus power loss'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The recipe database is built to step 3. A writer child process commits'
.print 'steps 4-11 on one connection in WAL mode (wal_autocheckpoint=0), runs one'
.print 'PASSIVE checkpoint after step 7, and exits without closing SQLite, so its'
.print 'files stay exactly as a crashed process leaves them. It prints an'
.print 'acknowledgement after each commit returns.'
.print
.print 'A process crash keeps what the kernel already holds. A power cut keeps'
.print 'only what was synced. Part B compares the two for synchronous=OFF, NORMAL'
.print 'and FULL, and for NORMAL with a second connection holding a read'
.print 'transaction from step 5. Part A (Linux only) kills the writer at chosen'
.print 'system calls.'
.print
.print 'Parts skipped on this platform (LINUX-ONLY notices):'

SELECT part, coalesce(variant, '') AS variant, what FROM skipped
UNION ALL
SELECT 'none', '', 'strace ran: nothing was skipped' WHERE NOT EXISTS (SELECT 1 FROM skipped)
ORDER BY part, variant;

.print
.print '── B. What a process crash keeps, and what the simplest power cut keeps'
.print
.print 'B1. The four runs. Each writer acknowledged steps 4-11. The checkpoint'
.print '    copied every frame, so the next commit restarted the WAL, except with'
.print '    the reader: it stopped the checkpoint short of the end, and the'
.print '    commits after it stayed in the same WAL generation.'

SELECT variant, synchronous, reader_from,
       first_acknowledged || '-' || last_acknowledged AS acknowledged,
       'after ' || checkpoint_after || ': busy=' || checkpoint_busy || ', copied ' || checkpointed || ' of ' || checkpoint_log AS checkpoint,
       same_generation_after_checkpoint AS wal_not_restarted
FROM runs ORDER BY seq;

.print
.print 'B2. A process crash: the database file and -wal copied as the kernel holds'
.print '    them, then reopened. Every variant recovers every acknowledged step,'
.print '    even synchronous=OFF. "exact_bytes = false" means equal to the recipe'
.print '    step except the change counter (header bytes 24-27 and 92-95).'

SELECT variant, last_acknowledged, crash_integrity AS integrity_check,
       crash_recovered_step AS recovered_step, crash_exact_bytes AS exact_bytes
FROM runs ORDER BY seq;

.print
.print 'B3. Which -wal bytes were synced. The rule column is read from the SQLite'
.print '    sync rules (src/pager.c:3658-3704, src/wal.h:25-26, src/wal.c); the'
.print '    strace columns are what the writer actually did. OFF never syncs;'
.print '    NORMAL syncs only around the checkpoint (up to byte 32, the header of'
.print '    the restarted WAL, or up to the checkpointed end when the reader kept'
.print '    the WAL from restarting); FULL syncs every commit.'

SELECT r.variant,
       coalesce(CAST(r.wal_synced_up_to AS VARCHAR), 'all of it') AS rule_synced_up_to,
       s.syncs, s.writes_after_last_sync,
       s.lowest_unsynced_offset,
       CASE WHEN s.variant IS NOT NULL
            THEN coalesce(CAST(s.lowest_unsynced_offset AS BIGINT), -1) = coalesce(r.wal_synced_up_to, -1) END AS agree
FROM sync_rules r LEFT JOIN syncs s ON s.variant = r.variant AND s.file = '-wal'
JOIN runs u ON u.variant = r.variant
ORDER BY u.seq;

.print
.print 'B4. The database file. A checkpoint that copies every frame syncs it; one'
.print '    that stops short (the reader) does not, and at OFF nothing is synced.'

SELECT r.variant, r.database_file_unsynced AS rule_unsynced_writes,
       s.syncs, s.writes_after_last_sync, s.lowest_unsynced_offset,
       r.database_file_unsynced = (CAST(s.writes_after_last_sync AS BIGINT) > 0) AS agree
FROM sync_rules r LEFT JOIN syncs s ON s.variant = r.variant AND s.file = 'database file'
JOIN runs u ON u.variant = r.variant
ORDER BY u.seq;

.print
.print 'B5. Where each acknowledged commit ends in the -wal (byte offset, then'
.print '    #WAL generation). A commit ending past the last sync in the newest'
.print '    generation is marked "lost": the power-cut model removes it.'

WITH cut AS (SELECT variant, wal_kept_bytes FROM power_cuts WHERE database_file = 'as written'),
     last_gen AS (SELECT variant, max(wal_generation) AS g FROM commits GROUP BY variant),
     marked AS (
       SELECT c.variant, c.step,
              c.wal_end || ' #' || c.wal_generation
                || CASE WHEN c.wal_generation = l.g AND c.wal_end > cut.wal_kept_bytes THEN ' lost' ELSE '' END AS ends
       FROM commits c JOIN last_gen l USING (variant) LEFT JOIN cut USING (variant))
SELECT step,
       first(ends) FILTER (variant = 'off')             AS "off",
       first(ends) FILTER (variant = 'normal')          AS normal,
       first(ends) FILTER (variant = 'full')            AS "full",
       first(ends) FILTER (variant = 'normal-reader-5') AS "normal-reader-5"
FROM marked GROUP BY step ORDER BY step;

.print
.print 'B6. The power-cut model: the database file as written, the -wal cut back'
.print '    to the last sync, no -shm; then reopened. NORMAL loses the four commits'
.print '    after the checkpoint but stays consistent; FULL loses nothing. With the'
.print '    reader the checkpoint did not sync the database file, so the model also'
.print '    runs with the file as it was before the checkpoint: the same step, since'
.print '    the WAL frames those writes came from were synced first.'

SELECT variant, database_file, wal_kept_bytes || ' of ' || wal_bytes AS wal_bytes_kept,
       lost_commits AS lost, integrity, recovered_step AS recovered, exact_bytes AS exact
FROM power_cuts ORDER BY seq;

.print
.print '    At OFF there is no model: the checkpoint wrote the database file and'
.print '    nothing was ever synced, so a power cut can leave any mix of old and'
.print '    new database pages.'

SELECT variant, power_model_not_applicable AS why_not
FROM sync_rules WHERE power_model_not_applicable IS NOT NULL ORDER BY variant;

.print
.print '── A. Kill the writer at chosen syscalls, reopen, identify the state ──'
.print
.print 'A1. For each synchronous level, a fresh writer runs under strace and is'
.print '    killed with SIGKILL on entry to one call: every fsync/fdatasync the'
.print '    uninterrupted writer made, and 6 of its pwrite64 calls spread evenly.'

SELECT synchronous, sync_calls, pwrite64_calls, pwrite64_kills,
       CAST(sync_calls AS BIGINT) + CAST(pwrite64_kills AS BIGINT) AS kills
FROM kill_plans ORDER BY CAST(sync_calls AS BIGINT);

.print
.print 'A2. Every kill. "recovered" is the recipe step the reopened database'
.print '    equals: the last acknowledged step, or (at FULL, killed in the fsync of'
.print '    a commit) the step being committed, whose frames were already written.'

SELECT synchronous, kill_no, syscall || ' #' || call_no AS kill_at,
       replace(killed_call, ' = ? (process killed)', '') AS killed_in,
       acknowledged, recovered_step AS recovered, exact_bytes AS exact
FROM kills
ORDER BY CASE synchronous WHEN 'OFF' THEN 1 WHEN 'NORMAL' THEN 2 ELSE 3 END, CAST(kill_no AS BIGINT);

.print
.print 'A3. Summary. Every kill was a SIGKILL, every reopened database passed'
.print '    integrity_check, and none lost an acknowledged step.'

SELECT synchronous, count(*) AS kills,
       count(*) FILTER (signal = 'SIGKILL')              AS sigkill,
       count(*) FILTER (integrity = 'ok')                AS integrity_ok,
       count(*) FILTER (recovered_step = acknowledged)     AS recovered_acknowledged,
       count(*) FILTER (CAST(recovered_step AS BIGINT) = CAST(acknowledged AS BIGINT) + 1) AS recovered_one_more
FROM kills GROUP BY synchronous
ORDER BY CASE synchronous WHEN 'OFF' THEN 1 WHEN 'NORMAL' THEN 2 ELSE 3 END;

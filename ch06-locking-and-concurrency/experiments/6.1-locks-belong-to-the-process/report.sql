-- Walkthrough of experiment 6.1. Run it with:
--   go run ./cmd/course run 6.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps          one statement or action one child process ran, numbered across all parts
--   lock_views     one look at the locks on one file, taken after a step
--   lock_probes    one named lock range in one look, probed by the parent with fcntl F_GETLK
--   proc_locks     one /proc/locks entry of a child for that file in one look (Linux only)
--   final_state    one part: the file after both processes closed
--   files          part D: whether -wal and -shm exist at a moment
--   checks         every claim the experiment asserted (the runner lists it last)
-- info_run and info_processes (work dir, GOOS, pids) are shown by info.sql.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  6.1  Locks belong to the process'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Two child processes, holder and contender, open the same database. The'
.print 'holder starts a write transaction, then opens and closes the database file'
.print 'or -shm with a plain os.Open + Close (a "stray close"). POSIX locks belong'
.print 'to the process and the inode, so that close drops every lock the holder'
.print 'has on that file, while SQLite still believes it holds them.'
.print
.print 'The parent holds no connection. It probes each lock range with fcntl F_GETLK'
.print 'and, on Linux, reads /proc/locks. F_GETLK names a single holder only for a'
.print 'write lock; /proc/locks lists every holder. Where /proc/locks does not'
.print 'exist (not Linux), the experiment prints LINUX-ONLY, skips that listing and'
.print 'its claims, and with_proc_locks below is 0.'

SELECT count(*) AS looks, count(*) FILTER (proc_locks_listed) AS with_proc_locks FROM lock_views ORDER BY looks;

.print
.print 'How each part ended: what COMMIT told each process, and what the file holds.'

SELECT f.part,
       x.scenario,
       (SELECT result FROM steps s WHERE s.part = f.part AND process = 'holder' AND action = 'commit')    AS holder_commit,
       (SELECT result FROM steps s WHERE s.part = f.part AND process = 'contender' AND action = 'commit') AS contender_commit,
       f.integrity,
       f.contents
FROM final_state f
JOIN (VALUES ('A', 'rollback mode'),
             ('B', 'WAL, empty WAL'),
             ('C', 'WAL holding a commit'),
             ('D', 'WAL, contender exits first')) x(part, scenario) USING (part)
ORDER BY f.part;

.print
.print 'In the transcripts below, ROWS_QUERY stands for'
.print "  SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '')"
.print '  FROM (SELECT who FROM lab ORDER BY id)'

.print
.print '── A. Rollback mode (journal_mode=delete) ──────────────────────────────'
.print
.print 'The write lock is a byte-range lock on the database file.'
.print
.print 'A1. The transcript. Step 4 is the stray close; after it the contender is'
.print "    admitted (step 6) and commits (step 8), and the holder's COMMIT fails."

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'A' ORDER BY step;

.print
.print 'A2. The database-file locks seen by F_GETLK after steps 2, 4 and 7. After'
.print "    the stray close (step 4) nothing is locked; then RESERVED is the contender's."

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'A' AND file = 'database file')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'A3. The same looks in /proc/locks, which names every holder. After step 4'
.print '    neither process holds a lock; after step 7 only the contender does.'

SELECT v.after_step, coalesce(p.process, '(none)') AS process, p.type, p.bytes, p.lock
FROM lock_views v LEFT JOIN proc_locks p USING (part, after_step, file)
WHERE v.part = 'A' AND v.proc_locks_listed
ORDER BY v.seq, p.process, p.type;

.print
.print '── B. WAL mode, empty WAL ──────────────────────────────────────────────'
.print
.print 'The write lock is byte 120 of -shm (WAL_WRITE_LOCK). A stray close of the'
.print 'database file (step 13) leaves it in place; a stray close of -shm (step 15)'
.print "drops it. The holder's COMMIT then sees the wal-index header changed and"
.print 'gives up with SQLITE_PROTOCOL: here the holder is told.'
.print
.print 'B1. The transcript.'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'B' ORDER BY step;

.print
.print "B2. -shm locks seen by F_GETLK. Byte 120 is the holder's write lock until"
.print '    the stray close of -shm; byte 128 (DMS) stays read-locked by the contender.'

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'B' AND file = '-shm')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'B3. Database-file locks seen by F_GETLK. RESERVED stays free: WAL mode does'
.print '    not use it for writers.'

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'B' AND file = 'database file')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'B4. /proc/locks for both files. After the stray close of the database file'
.print "    (step 13) the holder's SHARED lock is gone; after the stray close of -shm"
.print "    (step 15) only the contender's DMS read lock is left on -shm."

SELECT v.after_step, v.file, coalesce(p.process, '(none)') AS process, p.type, p.bytes, p.lock
FROM lock_views v LEFT JOIN proc_locks p USING (part, after_step, file)
WHERE v.part = 'B' AND v.proc_locks_listed
ORDER BY v.seq, p.process, p.type, p.bytes;

.print
.print '── C. WAL mode, the WAL already holding a commit ───────────────────────'
.print
.print 'A seed row is committed first and no checkpoint copies it (wal_autocheckpoint=0),'
.print 'so the holder reads through WAL_READ_LOCK(1), byte 124. Only -shm is closed.'
.print "Both COMMITs return SQLITE_OK, and the contender's row is gone (step 30)."
.print
.print 'C1. The transcript.'

SELECT step, process,
       CASE WHEN action LIKE 'query SELECT count(*) || %' THEN 'query ROWS_QUERY' ELSE action END AS action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C' ORDER BY step;

.print
.print 'C2. -shm locks seen by F_GETLK, before and after the stray close of -shm.'

SELECT * EXCLUDE (first_byte) FROM (
  PIVOT (SELECT lock,
                CASE WHEN first_byte = last_byte THEN 'byte ' || first_byte
                     ELSE 'bytes ' || first_byte || '-' || last_byte END AS bytes,
                first_byte,
                'after step ' || after_step AS look,
                CASE state WHEN 'write' THEN 'write: ' || holder WHEN 'read' THEN 'read' ELSE '-' END AS state
         FROM lock_probes WHERE part = 'C' AND file = '-shm')
  ON look USING first(state) GROUP BY lock, bytes, first_byte)
ORDER BY first_byte;

.print
.print 'C3. The same looks in /proc/locks.'

SELECT v.after_step, coalesce(p.process, '(none)') AS process, p.type, p.bytes, p.lock
FROM lock_views v LEFT JOIN proc_locks p USING (part, after_step, file)
WHERE v.part = 'C' AND v.proc_locks_listed
ORDER BY v.seq, p.process, p.type, p.bytes;

.print
.print '── D. WAL mode, the contender exits before the holder commits ──────────'
.print
.print 'As in B, the holder closes the database file and -shm. The contender commits,'
.print 'closes its connection and exits. Seeing no other locks, it closes as the last'
.print 'connection and deletes -wal and -shm. The holder then commits into the -wal'
.print 'it still has open, a file that no longer has a name.'
.print
.print 'D1. The transcript.'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'D' ORDER BY step;

.print
.print 'D2. Whether -wal and -shm exist, after the contender exited and after the'
.print "    holder's commit."

SELECT moment, file, state FROM files WHERE part = 'D' ORDER BY seq;

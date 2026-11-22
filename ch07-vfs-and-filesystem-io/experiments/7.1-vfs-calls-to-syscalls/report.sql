-- Walkthrough of experiment 7.1. Run it with:
--   go run ./cmd/course run 7.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   statements        one row per step of the CLI session
--   tracing           one row: whether the syscalls were traced (Linux) or skipped
--   stream            one row per line of the merged stream: a VFS call, a file
--                     syscall, or a line of CLI output; charged_to is the seq of
--                     the VFS call a syscall is counted against
--   vfs_methods       one row per VFS method called: its os_unix.c function and calls
--   problems          one row per VFS call or syscall that broke a checked rule
--   files_left        one row per file in the directory after the CLI exits
--   info_randomness   where xRandomness ran (differs between CLI builds; info.sql)
--   checks            every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  7.1  Which syscalls does each VFS call make?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The pinned sqlite3 CLI runs one session with -vfstrace (each VFS call it'
.print 'makes is printed as a line) under strace, which logs the syscalls on the'
.print 'database and its -journal, -wal and -shm files and on the directory. The'
.print 'two logs are merged into one ordered stream. mmap_size is 0, the SQLite'
.print 'default. Traced: openat open close pwrite64 write writev pread64 read'
.print 'fsync fdatasync unlink unlinkat fcntl ftruncate mmap munmap and the stat'
.print 'and access families.'
.print
.print 'The session, one CLI process (.open --normal t.db first):'

SELECT step, label, sql FROM statements ORDER BY step;

.print
.print 'Whether the syscalls were traced (strace needs Linux; elsewhere only the'
.print 'VFS calls are recorded):'

SELECT syscalls FROM tracing ORDER BY syscalls;

.print
.print '── A. The merged stream ────────────────────────────────────────────────'
.print
.print 'VFS = a line of -vfstrace output; indented SYS = a syscall made while'
.print 'that VFS call ran; unindented SYS = a syscall outside any printed VFS call;'
.print 'OUT = CLI output. Syscall names are normalized: open = open/openat,'
.print 'sync = fsync/fdatasync, stat = the stat family, unlink = unlink/unlinkat.'
.print 'xRandomness is left out of the stream: where SQLite first asks the VFS for'
.print 'random bytes differs between CLI builds.'
.print
.print 'A1. The size of the stream:'

SELECT count(*) FILTER (kind = 'VFS')
         + coalesce((SELECT calls FROM vfs_methods WHERE method = 'xRandomness'), 0) AS vfs_calls,
       count(*) FILTER (kind = 'SYS') AS file_syscalls,
       count(*) FILTER (kind = 'OUT') AS output_lines,
       coalesce((SELECT calls FROM vfs_methods WHERE method = 'xRandomness'), 0) AS xrandomness_left_out
FROM stream;

CREATE TEMP VIEW shown AS
SELECT seq, step,
       CASE kind
         WHEN 'SYS' THEN CASE WHEN nested THEN '    SYS ' ELSE 'SYS ' END || text
         ELSE kind || ' ' || text
       END AS event
FROM stream;

.print
.print 'A2. Opening t.db. vfstrace prints xOpen after the real open returns, so the'
.print 'open and stat syscalls of xOpen come just before its line. The 100-byte'
.print 'header read is short: the file is empty.'

SELECT seq, event FROM shown WHERE step = 0 ORDER BY seq;

.print
.print 'A3. STEP 1, journal_mode=WAL. Changing the mode writes page 1 under a'
.print 'rollback journal: the journal is created and synced twice (the first'
.print 'time with a sync of the directory), page 1 is written and synced, and the'
.print 'journal is deleted.'

SELECT seq, event FROM shown WHERE step = 1 ORDER BY seq;

.print
.print 'A4. STEP 2, synchronous=FULL, only tells the VFS about the pragma. STEP 3,'
.print 'CREATE TABLE, is the first use of the WAL: xOpen creates -wal, the first'
.print 'xShmMap creates -shm, extends it one byte per 4 KiB and maps it, and the'
.print 'commit writes the WAL header, syncs it (with the directory), writes two'
.print 'frames (24-byte frame header + 4096-byte page) and syncs again.'

SELECT seq, event FROM shown WHERE step IN (2, 3) ORDER BY seq;

.print
.print 'A5. STEP 4, one INSERT: one frame and one sync of the -wal.'

SELECT seq, event FROM shown WHERE step = 4 ORDER BY seq;

.print
.print 'A6. STEP 5, wal_checkpoint(TRUNCATE): sync the -wal, copy the page of each'
.print 'frame into t.db (pread64 of the -wal, pwrite64 of t.db), truncate and sync t.db,'
.print 'then truncate the -wal to 0 bytes.'

SELECT seq, event FROM shown WHERE step = 5 ORDER BY seq;

.print
.print 'A7. STEP 6, close: the last connection unlinks -shm (xShmUnmap) and -wal'
.print '(xDelete).'

SELECT seq, event FROM shown WHERE step = 6 ORDER BY seq;

.print
.print '── B. Each VFS method and the syscalls behind it ───────────────────────'
.print
.print 'B1. Every VFS method, the os_unix.c function behind it, and the syscalls'
.print 'seen while it ran (for xOpen: the syscalls just before its line). Several'
.print 'methods make no syscall at all.'

SELECT m.method,
       m.calls,
       m.unix_function,
       coalesce((SELECT string_agg(call || '×' || n, ', ' ORDER BY call)
                 FROM (SELECT s.call, count(*) AS n
                       FROM stream v JOIN stream s ON s.charged_to = v.seq
                       WHERE v.kind = 'VFS' AND v.call = m.method AND s.kind = 'SYS'
                       GROUP BY s.call)), 'no file syscall') AS syscalls
FROM vfs_methods m
ORDER BY m.method;

.print
.print 'B2. Every syscall that moves file bytes, by the VFS call it ran inside:'
.print 'reads only in xRead, writes only in xWrite, and the one-byte writes that'
.print 'extend -shm in xShmMap.'

SELECT s.call AS syscall,
       CASE WHEN s.nested THEN v.call ELSE '(outside any VFS call)' END AS inside,
       count(*) AS syscalls
FROM stream s LEFT JOIN stream v ON v.seq = s.charged_to
WHERE s.kind = 'SYS' AND s.call IN ('pread64', 'pwrite64', 'read', 'write', 'writev')
GROUP BY ALL
ORDER BY syscall, inside;

.print
.print 'B3. VFS calls or syscalls that broke one of the rules in the checks below'
.print '(one pwrite64 per xWrite, one pread64 per xRead, one sync of its own file'
.print 'per xSync, no byte I/O outside xRead, xWrite and xShmMap):'

SELECT count(*) AS rule_breaks FROM problems;

.print
.print '── C. Syncs ────────────────────────────────────────────────────────────'
.print
.print 'C1. Each xSync makes exactly one fsync/fdatasync of its own file. The first'
.print 'xSync of a newly created journal or WAL also syncs the directory, so the'
.print 'new directory entry is durable; no VFS call names the directory.'

WITH x AS (
  SELECT seq, step, file, row_number() OVER (PARTITION BY file ORDER BY seq) AS nth
  FROM stream WHERE kind = 'VFS' AND call = 'xSync'
)
SELECT x.seq, x.step, x.file, x.nth AS nth_xsync_of_file,
       count(s.seq) FILTER (s.file = x.file) AS own_file_syncs,
       count(s.seq) FILTER (s.file = '.') AS directory_syncs
FROM x LEFT JOIN stream s ON s.charged_to = x.seq AND s.kind = 'SYS' AND s.call = 'sync'
GROUP BY ALL
ORDER BY x.seq;

.print
.print 'C2. Syncs per file:'

SELECT CASE file WHEN '.' THEN 'directory (.)' ELSE file END AS file, count(*) AS syncs
FROM stream WHERE kind = 'SYS' AND call = 'sync'
GROUP BY file
ORDER BY file = '.', file;

.print
.print '── D. Creating and removing the sidecar files ──────────────────────────'
.print
.print 'D1. Every open and unlink of the -journal, -wal and -shm files, and the VFS'
.print 'call it is counted against: each file is created at its first use and'
.print 'removed by the VFS call that ends that use.'

SELECT s.seq, s.step, s.text AS syscall, v.call AS vfs_call
FROM stream s LEFT JOIN stream v ON v.seq = s.charged_to
WHERE s.kind = 'SYS' AND s.call IN ('open', 'unlink') AND s.file LIKE 't.db-%'
ORDER BY s.seq;

.print
.print 'D2. Files left in the directory after the CLI exits:'

SELECT name FROM files_left ORDER BY name;

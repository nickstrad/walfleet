-- Walkthrough of experiment 1.2. Run it with:
--   go run ./cmd/course run 1.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   explain_ops      one row per VDBE opcode of EXPLAIN INSERT
--   runs             one row per journal mode: the base database before the INSERT
--   page_changes     one row per page the INSERT changed, by mode (sha256 prefix before and after)
--   go_syscalls      one row per file syscall of the Go driver's INSERT, in order
--   cli_syscalls     one row per file syscall of the C CLI's INSERT, in order (normalized)
--   wal_frames       one row per frame the WAL mode commit appended
--   file_hashes      one row per database file whose bytes were hashed after a run
--   phases           one row per split statement (BEGIN IMMEDIATE, INSERT, COMMIT) of the CLI run
--   phase_events     one row per VFS call or syscall inside a phase, in order
--   analyze_runs     one row per database file of the ANALYZE limit
--   checks           every claim the experiment asserted (the runner lists it last)
-- info_run, info_sync and info_randomness hold values that depend on the machine
-- or the CLI build; info.sql shows them.

CREATE TEMP VIEW calls AS
SELECT 'go' AS build, mode, seq, norm FROM go_syscalls
UNION ALL
SELECT 'cli', mode, seq, norm FROM cli_syscalls;

CREATE TEMP VIEW twin_categories AS
SELECT 1 AS ord, 'writes (file, offset, length, order)' AS category, * FROM calls
WHERE split_part(norm, ' ', 1) IN ('pwrite64', 'write', 'ftruncate')
UNION ALL
SELECT 2, 'syncs (file, order)', * FROM calls WHERE split_part(norm, ' ', 1) = 'sync'
UNION ALL
SELECT 3, 'every call except fcntl F_SETFD', * FROM calls WHERE norm NOT LIKE 'fcntl % F_SETFD%'
UNION ALL
SELECT 4, 'fcntl F_SETFD only', * FROM calls WHERE norm LIKE 'fcntl % F_SETFD%';

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  1.2  What does one committed INSERT do, layer by layer?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'One INSERT of the row (2, "two") into table t(id, v), which already holds'
.print 'one row (2 pages: page 1 for sqlite_schema, page 2 for t), followed down the'
.print 'layers: VDBE bytecode, the B-tree pages it changes, the file syscalls of the'
.print 'Go driver (modernc, traced with strace), the same INSERT in the C sqlite3 CLI'
.print '(the twin check), and the CLI with -vfstrace, which names the VFS call behind'
.print 'each syscall. Parts B and C do this in rollback-journal and in WAL mode;'
.print 'part D shows where the twin check stops being valid.'

.print
.print '── A. The bytecode ─────────────────────────────────────────────────────'
.print
.print 'A1. EXPLAIN of the INSERT. OpenWrite opens the table b-tree (root page 2);'
.print 'MakeRecord builds the row; Insert (p5=49) puts it in the leaf; Transaction'
.print 'at addr 14 is where the write lock is taken.'

SELECT addr, opcode, p1, p2, p3, p4, p5 FROM explain_ops ORDER BY addr;

.print
.print '── B. Rollback journal (journal_mode=delete) ───────────────────────────'
.print
.print 'B1. The base database, and the pages the INSERT changed (sha256 prefix of'
.print 'each page, read through sqlite_dbpage before and after).'
.print 'Page 1 changes too: the change counter in its header.'

SELECT r.mode, r.base_pages AS pages_before, r.base_sha256 AS file_sha256,
       p.page, p.before_sha256, p.after_sha256
FROM runs r JOIN page_changes p USING (mode)
WHERE mode = 'delete'
ORDER BY p.page;

.print
.print 'B2. The file syscalls the Go driver made for the INSERT, in order'
.print '(open/openat and unlink/unlinkat are written open and unlink; "= fd" stands'
.print 'for the descriptor number, which varies):'

SELECT seq, call FROM go_syscalls WHERE mode = 'delete' ORDER BY seq;

.print
.print 'B3. Where the bytes land. Each pwrite64 on t.db is one whole page at'
.print 'offset (page - 1) * 4096; the pages written are the pages that changed:'

SELECT call, CAST(regexp_extract(norm, 'off=(\d+)', 1) AS INTEGER) AS file_offset,
       CAST(regexp_extract(norm, 'off=(\d+)', 1) AS INTEGER) // 4096 + 1 AS page,
       CAST(regexp_extract(norm, 'len=(\d+)', 1) AS INTEGER) AS length
FROM go_syscalls
WHERE mode = 'delete' AND split_part(norm, ' ', 1) = 'pwrite64' AND split_part(norm, ' ', 2) = 't.db'
ORDER BY seq;

.print
.print 'B4. The C CLI runs the same INSERT under strace. Its syscalls, normalized'
.print '(open/openat as open, fsync/fdatasync as sync); read and write lengths are'
.print 'kept, so a short transfer would show:'

SELECT seq, norm FROM cli_syscalls WHERE mode = 'delete' ORDER BY seq;

.print
.print 'B5. The twin check: the Go driver and the CLI compared category by category.'
.print 'Writes and syncs must be identical, and so must every call apart from the'
.print 'fcntl(F_SETFD) that the libc of modernc adds after each O_CLOEXEC open:'

SELECT category,
       count(*) FILTER (build = 'go')  AS go_calls,
       count(*) FILTER (build = 'cli') AS cli_calls,
       CASE WHEN ord = 4 THEN NULL
            ELSE list(norm ORDER BY seq) FILTER (build = 'go') = list(norm ORDER BY seq) FILTER (build = 'cli')
       END AS identical
FROM twin_categories
WHERE mode = 'delete'
GROUP BY ord, category
ORDER BY ord;

.print
.print 'The F_SETFD calls only the Go driver makes:'

SELECT seq, call FROM go_syscalls WHERE mode = 'delete' AND call LIKE 'fcntl % F_SETFD%' ORDER BY seq;

.print
.print 'B6. The database file after each run, as a hash. The Go driver and the CLI'
.print 'must leave the same bytes, and the INSERT split into three statements must'
.print 'leave the same bytes as the autocommit INSERT:'

SELECT what, sha256 FROM file_hashes WHERE mode = 'delete'
ORDER BY CASE what WHEN 'base' THEN 1 WHEN 'go_insert' THEN 2 WHEN 'cli_insert' THEN 3 ELSE 4 END;

.print
.print 'B7. The measured call path. The CLI runs BEGIN IMMEDIATE; INSERT; COMMIT with'
.print 'a marker file opened between the statements, so each VFS call and syscall is'
.print 'assigned to the statement that caused it. SYS lines indented under a VFS line'
.print 'are the syscalls that VFS call made. xRandomness calls are left out (info.sql).'

SELECT p.phase_no AS phase, p.statement, p.opcode
FROM phases p WHERE mode = 'delete' ORDER BY p.phase_no;

.print
.print 'The C functions behind each statement, outermost first:'

SELECT phase_no AS phase, unnest(range(1, len(steps) + 1)) AS n, unnest(steps) AS c_function
FROM (SELECT phase_no, string_split(call_path, ' -> ') AS steps FROM phases WHERE mode = 'delete')
ORDER BY phase, n;

.print
.print 'The calls of each statement, in order:'

SELECT phase_no AS phase, kind, repeat('    ', CAST(nested AS INTEGER)) || text AS call
FROM phase_events WHERE mode = 'delete' ORDER BY phase_no, seq;

.print
.print 'The three phases together make exactly the syscalls of the autocommit INSERT:'

SELECT (SELECT list(text ORDER BY phase_no, seq) FROM phase_events WHERE mode = 'delete' AND kind = 'SYS')
     = (SELECT list(norm ORDER BY seq) FROM cli_syscalls WHERE mode = 'delete') AS same_syscalls;

.print
.print '── C. Write-ahead log (journal_mode=wal, the default) ──────────────────'
.print
.print 'C1. The base database, and the pages the INSERT changed (sha256 prefix of'
.print 'each page, read through sqlite_dbpage before and after).'
.print 'Page 1 is not written, so its change counter stays.'

SELECT r.mode, r.base_pages AS pages_before, r.base_sha256 AS file_sha256,
       p.page, p.before_sha256, p.after_sha256
FROM runs r JOIN page_changes p USING (mode)
WHERE mode = 'wal'
ORDER BY p.page;

.print
.print 'C2. The file syscalls the Go driver made for the INSERT, in order'
.print '(open/openat and unlink/unlinkat are written open and unlink; "= fd" stands'
.print 'for the descriptor number, which varies):'

SELECT seq, call FROM go_syscalls WHERE mode = 'wal' ORDER BY seq;

.print
.print 'C3. Where the bytes land. The commit writes nothing to t.db itself; it'
.print 'appends one frame per changed page to t.db-wal after a 32-byte WAL header.'
.print 'The frame table is read from the -wal file by the traced child; offsets are'
.print '32 + (frame - 1) * 4120 for the 24-byte frame header, then +24 for the page:'

SELECT (SELECT count(*) FROM go_syscalls
        WHERE mode = 'wal' AND split_part(norm, ' ', 1) = 'pwrite64' AND split_part(norm, ' ', 2) = 't.db')
         AS pwrites_on_t_db;

SELECT frame_no AS frame, page, commit_size,
       32 + (frame_no - 1) * 4120      AS header_at,
       32 + (frame_no - 1) * 4120 + 24 AS page_image_at
FROM wal_frames WHERE mode = 'wal' ORDER BY frame_no;

.print
.print 'C4. The C CLI runs the same INSERT under strace. Its syscalls, normalized'
.print '(open/openat as open, fsync/fdatasync as sync); read and write lengths are'
.print 'kept, so a short transfer would show:'

SELECT seq, norm FROM cli_syscalls WHERE mode = 'wal' ORDER BY seq;

.print
.print 'C5. The twin check: the Go driver and the CLI compared category by category.'
.print 'Writes and syncs must be identical, and so must every call apart from the'
.print 'fcntl(F_SETFD) that the libc of modernc adds after each O_CLOEXEC open:'

SELECT category,
       count(*) FILTER (build = 'go')  AS go_calls,
       count(*) FILTER (build = 'cli') AS cli_calls,
       CASE WHEN ord = 4 THEN NULL
            ELSE list(norm ORDER BY seq) FILTER (build = 'go') = list(norm ORDER BY seq) FILTER (build = 'cli')
       END AS identical
FROM twin_categories
WHERE mode = 'wal'
GROUP BY ord, category
ORDER BY ord;

.print
.print 'The F_SETFD calls only the Go driver makes:'

SELECT seq, call FROM go_syscalls WHERE mode = 'wal' AND call LIKE 'fcntl % F_SETFD%' ORDER BY seq;

.print
.print 'C6. The database file after each run, as a hash. The Go driver and the CLI'
.print 'must leave the same bytes, and the INSERT split into three statements must'
.print 'leave the same bytes as the autocommit INSERT:'

SELECT what, sha256 FROM file_hashes WHERE mode = 'wal'
ORDER BY CASE what WHEN 'base' THEN 1 WHEN 'go_insert' THEN 2 WHEN 'cli_insert' THEN 3 ELSE 4 END;

.print
.print 'C7. The measured call path. The CLI runs BEGIN IMMEDIATE; INSERT; COMMIT with'
.print 'a marker file opened between the statements, so each VFS call and syscall is'
.print 'assigned to the statement that caused it. SYS lines indented under a VFS line'
.print 'are the syscalls that VFS call made. xRandomness calls are left out (info.sql).'

SELECT p.phase_no AS phase, p.statement, p.opcode
FROM phases p WHERE mode = 'wal' ORDER BY p.phase_no;

.print
.print 'The C functions behind each statement, outermost first:'

SELECT phase_no AS phase, unnest(range(1, len(steps) + 1)) AS n, unnest(steps) AS c_function
FROM (SELECT phase_no, string_split(call_path, ' -> ') AS steps FROM phases WHERE mode = 'wal')
ORDER BY phase, n;

.print
.print 'The calls of each statement, in order:'

SELECT phase_no AS phase, kind, repeat('    ', CAST(nested AS INTEGER)) || text AS call
FROM phase_events WHERE mode = 'wal' ORDER BY phase_no, seq;

.print
.print 'The three phases together make exactly the syscalls of the autocommit INSERT:'

SELECT (SELECT list(text ORDER BY phase_no, seq) FROM phase_events WHERE mode = 'wal' AND kind = 'SYS')
     = (SELECT list(norm ORDER BY seq) FROM cli_syscalls WHERE mode = 'wal') AS same_syscalls;

.print
.print '── D. A limit of the twin check ────────────────────────────────────────'
.print
.print 'The twin check assumes both builds write the same bytes, which holds only'
.print 'if their compile options agree. The driver is compiled with ENABLE_STAT4'
.print 'and this CLI is not. Two copies of a 100-row indexed table start byte for'
.print 'byte identical; the Go driver and the CLI each run ANALYZE on one copy:'

SELECT stage, build AS file, sha256, stat4_compiled, stat_tables
FROM analyze_runs
ORDER BY stage = 'base' DESC, build;

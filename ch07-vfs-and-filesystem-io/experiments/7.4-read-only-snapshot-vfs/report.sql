-- Walkthrough of experiment 7.4. Run it with:
--   go run ./cmd/course run 7.4        (add -i to stay in DuckDB afterwards)
-- The experiment also runs alone against SeaweedFS S3 instead of the
-- in-process HTTP server: docker compose up -d seaweedfs, then
--   go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   route         one row: how the snapshot is served and read
--   snapshot      one row: the published WAL-format snapshot
--   queries       part B: one row per query, its plan, and its result read
--                 from the local file and from the remote snapshot
--   small_cache   part C: one row: the full scan repeated with a 4-page cache
--   opens         parts D-F: one row per remote open and what it read or the error
--   live          part F: one row: the live database whose last commit is in its -wal
--   requests      one row per remote request: part, attempt (query or open number),
--                 phase (open = open and schema load, query, repeat), kind (stat or
--                 range), file, offset and length
--   info_s3       with -s3 only: the S3 endpoint (machine-dependent)
--   checks        every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  7.4  A read-only snapshot VFS over byte-range reads'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'A checkpointed recipe database is published as one object and queried'
.print 'through modernc.org/sqlite/vfs backed by internal/rangefs, which turns'
.print 'each open into one stat request and each read into one byte-range'
.print 'request, and records every request.'

SELECT route, server FROM route ORDER BY route;

SELECT stat_request, read_request FROM route ORDER BY route;

CREATE TEMP VIEW request_stats AS
SELECT part, attempt, phase,
       count(*) AS requests,
       count(*) FILTER (kind = 'stat') AS stat,
       count(*) FILTER (kind = 'range') AS range,
       coalesce(sum(length) FILTER (kind = 'range'), 0) AS bytes,
       string_agg(CASE WHEN kind <> 'range' THEN NULL
                       WHEN "offset" = 0 AND length = 100 THEN 'hdr'
                       ELSE ("offset" // 4096 + 1)::VARCHAR END, ' ' ORDER BY seq) AS pages
FROM requests
GROUP BY part, attempt, phase;

.print
.print '── A. The snapshot ─────────────────────────────────────────────────────'
.print
.print 'A1. The recipe database after all its steps, closed by its last'
.print 'connection, which checkpointed every commit into the main file:'

SELECT seed, steps, journal_mode, bytes, pages, sha256_prefix, file_state
FROM snapshot ORDER BY seed;

.print
.print 'A2. Header bytes 18-19 (file format write and read version) still say 2,'
.print 'WAL, although no -wal file is left:'

SELECT header_byte_18, header_byte_19, wal_present FROM snapshot ORDER BY seed;

.print
.print '── B. Three queries over the remote snapshot (immutable=1) ─────────────'
.print
.print 'B1. Each query runs on a new connection: open and schema load, the query'
.print 'with a cold cache, then the same query again.'

SELECT query, name, sql FROM queries ORDER BY query;

SELECT query, name, plan FROM queries ORDER BY query;

.print
.print 'B2. The remote result equals the result from the local file (digest ='
.print 'sha256 prefix of all rows):'

SELECT query, name, local_rows, remote_rows, local_digest, remote_digest
FROM queries ORDER BY query;

SELECT query, local_first_row = remote_first_row AS same_first_row, remote_first_row AS first_row
FROM queries ORDER BY query;

.print
.print 'B3. Opening and loading the schema costs the same three requests every'
.print 'time: a stat, the 100-byte header, and page 1.'

SELECT request, count(*) AS connections
FROM requests WHERE part = 'B' AND phase = 'open'
GROUP BY request
ORDER BY min(seq);

.print
.print 'B4. Every read is one range request for one whole 4096-byte page: one'
.print 'request per page miss, no read-ahead. The repeat on the same connection'
.print 'makes no request: immutable=1 skips the change-counter check, so the'
.print 'cached pages stay valid. Pages are listed in the order they were read.'

SELECT q.query, p.phase, coalesce(s.requests, 0) AS requests, coalesce(s.stat, 0) AS stat,
       coalesce(s.range, 0) AS range, coalesce(s.bytes, 0) AS bytes, s.pages
FROM queries q
CROSS JOIN (VALUES ('open', 1), ('query', 2), ('repeat', 3)) p(phase, ord)
LEFT JOIN request_stats s ON s.part = 'B' AND s.attempt = q.query AND s.phase = p.phase
ORDER BY q.query, p.ord;

.print
.print 'B5. The first query on a new connection, in total, and how many of its'
.print 'page reads the page cache can hold:'

SELECT q.name, sum(s.requests) AS requests, sum(s.bytes) AS bytes,
       sum(s.range) AS pages_read, any_value(q.cache_pages) AS cache_pages
FROM queries q JOIN request_stats s ON s.part = 'B' AND s.attempt = q.query AND s.phase IN ('open', 'query')
GROUP BY q.query, q.name
ORDER BY q.query;

.print
.print '── C. immutable=1 keeps cached pages valid, not cached ─────────────────'
.print
.print 'C1. The full scan again, repeated on one connection with PRAGMA'
.print 'cache_size=4. The scan reads more pages than the cache holds, so the'
.print 'repeat reads the evicted pages again; only pages still cached are free.'

SELECT c.query, c.cache_pages, c.same_result, s.requests, s.stat, s.range, s.bytes
FROM small_cache c JOIN request_stats s ON s.part = 'C' AND s.phase = 'repeat'
ORDER BY c.query;

.print
.print '── D. Why immutable=1: the WAL-format snapshot opened without it ───────'
.print
.print 'D1. Each open counts the events and reads user_version. Without'
.print 'immutable=1, SQLite sees the WAL flag in the header, needs shared memory,'
.print 'which this read-only VFS does not provide, and fails with SQLITE_CANTOPEN.'

SELECT o.params, o.result, s.requests, s.stat, s.range, s.bytes
FROM opens o LEFT JOIN request_stats s ON s.part = o.part AND s.attempt = o.attempt
WHERE o.part = 'D'
ORDER BY o.attempt;

.print
.print 'D2. The requests of each open: without immutable=1 SQLite also looks for'
.print 'a -journal and a -wal.'

SELECT o.params, r.request
FROM requests r JOIN opens o USING (part, attempt)
WHERE r.part = 'D'
ORDER BY r.seq;

.print
.print '── E. The same bytes in rollback format ────────────────────────────────'
.print
.print 'E1. A copy of the snapshot with header bytes 18-19 set to 1 (rollback'
.print 'format) opens without immutable=1 and reads the same state: the WAL flag'
.print 'in the header, not the content, is what demands shared memory.'

SELECT o.file, o.params, o.result, s.requests, s.stat, s.range, s.bytes
FROM opens o LEFT JOIN request_stats s ON s.part = o.part AND s.attempt = o.attempt
WHERE o.part = 'E'
ORDER BY o.attempt;

.print
.print 'E2. Its requests. Without immutable=1 SQLite checks for a hot journal'
.print '(stat of the -journal) whenever it takes a read lock; immutable=1 skips it.'

SELECT seq, request FROM requests WHERE part = 'E' ORDER BY seq;

SELECT count(*) AS journal_stat_requests
FROM requests WHERE part = 'E' AND kind = 'stat' AND name LIKE '%-journal';

.print
.print '── F. Why a fully checkpointed file ────────────────────────────────────'
.print
.print 'F1. A child process builds live.db to step 29, commits step 30 and keeps'
.print 'its connection open, so step 30 is only in the -wal. A local read through'
.print 'SQLite sees it. Both files are published as they are on disk.'

SELECT built_to_step, committed_step, local_state, db_bytes, wal_bytes, wal_frames
FROM live ORDER BY built_to_step;

.print
.print 'F2. The remote reads. 1: immutable=1 ignores the -wal and returns the'
.print 'state of step 29, a stale read with no error. 2: without immutable=1 the'
.print 'open fails. 3: after the writer closes (its last connection checkpoints'
.print 'and deletes the -wal) and the main file is republished, immutable=1 reads'
.print 'step 30.'

SELECT o.attempt AS n, o.params, o.result, s.requests, s.stat, s.range, s.bytes
FROM opens o LEFT JOIN request_stats s ON s.part = o.part AND s.attempt = o.attempt
WHERE o.part = 'F'
ORDER BY o.attempt;

SELECT count(*) FILTER (name LIKE '%-wal') AS requests_naming_the_wal_with_immutable,
       any_value(l.wal_deleted_after_close) AS wal_deleted_after_close
FROM requests r, live l
WHERE r.part = 'F' AND r.attempt = 1
ORDER BY 1;

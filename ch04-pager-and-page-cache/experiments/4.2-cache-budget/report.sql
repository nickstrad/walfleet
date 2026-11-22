-- Walkthrough of experiment 4.2. Run it with:
--   go run ./cmd/course run 4.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   parts        what ran and what was skipped
--   settings     one setting or derived constant of the run, by name
--   calibration  part A: the pages a full cache holds at cache_size=100 and 200
--   scans        part A: one connection after its first or second full scan
--   checkpoints  parts B to E: one measurement point of a child process run
--   connections  parts B to E: one open connection at one checkpoint
--   checks       every claim the experiment asserted (the runner lists it last)
-- Raw CACHE_USED bytes (they include the Pager allocation, which holds the file
-- names) and process memory (RSS) differ between runs and machines. They are in
-- info, info_memory and info_cache_used, shown by info.sql; the memory claims
-- below are checked against them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  4.2  What a page cache budget costs, per connection and per process'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Copies of one WAL-mode file are scanned by connections that keep their'
.print 'page cache. Each scenario runs in a fresh child process, which reads'
.print 'SQLITE_DBSTATUS_CACHE_USED, CACHE_HIT, CACHE_MISS and CACHE_SPILL of every'
.print 'open connection and its own RSS at each checkpoint.'

SELECT name, value FROM settings ORDER BY seq;

.print
.print 'ENABLE_MEMORY_MANAGEMENT = 1 means every page cache of the process is in'
.print 'one LRU group (pcache1.c:718-719). Cached pages are CACHE_USED minus one'
.print 'page buffer, divided by the bytes per cached page: the 4096-byte page plus'
.print 'its headers (sqlite3PagerMemUsed, pager.c:6881).'

SELECT part, what, status FROM parts ORDER BY part, what;

.print
.print '── A. One connection, the same scan twice ─────────────────────────────'
.print
.print 'A1. The bytes per cached page come from two full caches 100 pages apart.'
.print '    A cache recycles a page when it is one page short of full'
.print '    (pcache1.c:902), so cache_size=N holds N-1 pages.'

SELECT cache_size, cached_pages FROM calibration ORDER BY cache_size;

.print
.print 'A2. At the default budget (2048000 bytes) the cache holds 482 pages of the'
.print '    1004-page file, and the second scan misses every page again. A budget of'
.print '    twice the file holds it all, and the second scan only hits.'

SELECT cache_size, budget_bytes, scan, cached_pages, hits, misses, spills
FROM scans ORDER BY cache_size DESC, scan;

.print
.print '── B. N connections to N files, all kept open ─────────────────────────'
.print
.print 'B1. Every connection opens its file, scans it and stays open. The cached'
.print '    pages of all open connections add up to 482 per connection, but they'
.print '    are not spread evenly: the one LRU group lets the connection that'
.print '    scanned last take pages from the idle ones.'

SELECT p.checkpoint, p.open,
       coalesce(sum(c.cached_pages), 0)                 AS cached_pages_sum,
       coalesce(sum(c.cached_pages), 0) // nullif(p.open, 0) AS per_open_conn,
       min(c.cached_pages) || '..' || max(c.cached_pages) AS per_conn_min_max
FROM checkpoints p LEFT JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'B' AND p.checkpoint <> 'start'
GROUP BY p.seq, p.checkpoint, p.open ORDER BY p.seq;

.print
.print 'B2. At the last checkpoint, the first connection and the last one:'

SELECT conn, cached_pages, hits, misses, spills
FROM connections
WHERE scenario = 'B' AND seq = (SELECT max(seq) FROM connections WHERE scenario = 'B')
  AND conn IN (1, (SELECT max(conn) FROM connections WHERE scenario = 'B'))
ORDER BY conn;

.print
.print 'B3. Cached pages per connection at the last checkpoint:'

SELECT cached_pages, count(*) AS connections, min(conn) AS first_conn, max(conn) AS last_conn
FROM connections
WHERE scenario = 'B' AND seq = (SELECT max(seq) FROM connections WHERE scenario = 'B')
GROUP BY cached_pages ORDER BY cached_pages;

.print
.print '── C. N connections to one file, all kept open ────────────────────────'
.print
.print 'C1. Connections to the same file do not share pages: every checkpoint'
.print '    has the same cached pages as in part B.'

SELECT p.checkpoint, p.open,
       coalesce(sum(c.cached_pages), 0) AS cached_pages_sum,
       min(c.cached_pages) || '..' || max(c.cached_pages) AS per_conn_min_max,
       (SELECT coalesce(sum(b.cached_pages), 0) FROM connections b
        WHERE b.scenario = 'B' AND b.checkpoint = p.checkpoint) AS part_b_sum
FROM checkpoints p LEFT JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'C' AND p.checkpoint <> 'start'
GROUP BY p.seq, p.checkpoint, p.open ORDER BY p.seq;

.print
.print '── D. N files visited through a pool of at most K open connections ────'
.print
.print 'D1. The pool closes its least recently used connection before it opens'
.print '    the next. After K files the cached pages stop growing.'

SELECT p.checkpoint, p.open, p.visited,
       coalesce(sum(c.cached_pages), 0) AS cached_pages_sum,
       min(c.cached_pages) || '..' || max(c.cached_pages) AS per_conn_min_max
FROM checkpoints p LEFT JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'D' AND p.checkpoint <> 'start'
GROUP BY p.seq, p.checkpoint, p.open, p.visited ORDER BY p.seq;

.print
.print '── E. mmap ─────────────────────────────────────────────────────────────'
.print
.print 'E1. Connection 1 scans f-000 without mmap. Connection 2 sets PRAGMA'
.print '    mmap_size to twice the file and scans f-001: its pages come from the'
.print '    mapped file, outside the page cache, so it caches almost nothing.'

SELECT p.checkpoint, c.conn, c.cached_pages, c.hits, c.misses, c.spills
FROM checkpoints p JOIN connections c USING (scenario, seq)
WHERE p.scenario = 'E'
ORDER BY p.seq, c.conn;

.print
.print 'The memory claims (RSS grows with the summed CACHE_USED, linearly with open'
.print 'connections, stays flat with the pool, and moves to file-backed pages with'
.print 'mmap) are in the checks below; the RSS figures are in info.sql.'

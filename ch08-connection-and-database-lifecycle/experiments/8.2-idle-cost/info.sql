-- Run-dependent values of experiment 8.2 (not stored as evidence):
--   info_cache    one connection: CACHE_USED before shrink_memory, after it ran on connection 0, after it ran on all
--   info_memory   process memory (RSS) at three moments of part B
--   info_limits   RLIMIT_NOFILE as this Go process sees it

.print
.print '── Info (varies between runs and machines) ─────────────────────────────'
.print
.print 'Descriptor limit (RLIMIT_NOFILE as this Go process sees it):'

SELECT soft, hard FROM info_limits ORDER BY soft;

.print
.print 'CACHE_USED per connection, in bytes: before shrink_memory, after it ran'
.print 'on connection 0 only, and after it ran on every connection.'

SELECT min(cache_used) || '..' || max(cache_used)                         AS before_shrink,
       min(after_shrink_on_conn_0) || '..' || max(after_shrink_on_conn_0) AS after_shrink_on_conn_0,
       min(after_shrink_on_all) || '..' || max(after_shrink_on_all)       AS after_shrink_on_all
FROM info_cache;

.print
.print 'The three counters summed over all connections, in MiB:'

SELECT round(sum(i.cache_used) / 1048576, 1)                          AS cache,
       round(sum(c.schema_used) / 1048576, 1)                         AS schema,
       round(sum(c.stmt_used) / 1048576, 1)                           AS statements,
       round(sum(i.cache_used + c.schema_used + c.stmt_used) / 1048576, 1) AS total,
       round(sum(i.after_shrink_on_all) / 1048576, 1)                 AS cache_after_shrink
FROM info_cache i JOIN connections c USING (conn);

.print
.print 'Process memory, and the growth the three counters do not report (the'
.print 'experiment allows at most 256 KiB per connection):'

WITH m AS (SELECT seq, stage || ', MiB' AS stage, rss_bytes FROM info_memory),
     counted AS (SELECT sum(i.cache_used + c.schema_used + c.stmt_used) AS bytes, count(*) AS n
                 FROM info_cache i JOIN connections c USING (conn)),
     growth AS (SELECT (SELECT rss_bytes FROM m WHERE seq = 2) - (SELECT rss_bytes FROM m WHERE seq = 1) AS bytes)
SELECT what, value FROM (
    SELECT seq AS ord, stage AS what, round(rss_bytes / 1048576, 1) AS value FROM m
    UNION ALL
    SELECT 4, 'growth from opening, MiB', round(growth.bytes / 1048576, 1) FROM growth
    UNION ALL
    SELECT 5, 'growth / summed counters', round(growth.bytes / counted.bytes, 2) FROM growth, counted
    UNION ALL
    SELECT 6, 'not reported, KiB per connection', round((growth.bytes - counted.bytes) / counted.n / 1024, 0) FROM growth, counted
)
ORDER BY ord;

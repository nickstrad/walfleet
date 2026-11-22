-- Run-dependent values of experiment 8.3 (not stored as evidence):
--   info_streams   one stream run: peak process-memory growth, mean warm and cold read latency, run time
--   info_limits    RLIMIT_NOFILE as this Go process sees it

.print
.print '── Info (varies between runs and machines) ─────────────────────────────'
.print
.print 'Descriptor limit (RLIMIT_NOFILE); the unbounded run needs about 3 x the'
.print 'number of databases:'

SELECT soft, hard FROM info_limits ORDER BY soft;

.print
.print 'Peak process-memory growth against the budget, and mean read latency:'
.print 'warm (connection already open) and cold (open + verify + read).'

SELECT i.mode,
       round(i.peak_rss / 1048576, 1)      AS peak_growth_mib,
       round(s.memory_budget / 1048576, 1) AS budget_mib,
       round(i.warm_ns / 1000, 1)          AS warm_read_us,
       round(i.cold_ns / 1000, 1)          AS cold_read_us,
       i.elapsed_ms                        AS stream_ms
FROM info_streams i JOIN streams s USING (mode)
ORDER BY s.capacity;

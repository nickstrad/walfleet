-- Run-dependent values of experiment 4.2: raw CACHE_USED bytes and process
-- memory (RSS) at every checkpoint.

.print
.print 'Derived values of this run:'

SELECT name, value FROM info ORDER BY seq;

.print
.print 'Process memory at each checkpoint, MiB (growth is since the start of that child):'

SELECT scenario, checkpoint,
       round(cache_used_sum / 1048576, 1)                                      AS cache_used,
       round(rss / 1048576, 1)                                                 AS rss,
       round(rss_anon / 1048576, 1)                                            AS anon,
       round(rss_file / 1048576, 1)                                            AS file,
       round(rss_shmem / 1048576, 1)                                           AS shmem,
       round((rss - first(rss) OVER (PARTITION BY scenario ORDER BY seq)) / 1048576, 1) AS growth
FROM info_memory ORDER BY scenario, seq;

.print
.print 'Raw CACHE_USED of each connection in part E, bytes:'

SELECT checkpoint, conn, cache_used FROM info_cache_used WHERE scenario = 'E' ORDER BY seq, conn;

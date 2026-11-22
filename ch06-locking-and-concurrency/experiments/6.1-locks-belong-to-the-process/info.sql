-- Values that differ between runs or machines: the work directory, GOOS and
-- the process ids of the two children in each part.

.print
.print '── info. Work directory and process ids ───────────────────────────────'

SELECT work_dir, goos FROM info_run ORDER BY work_dir;

SELECT part, process, pid FROM info_processes ORDER BY part, pid;

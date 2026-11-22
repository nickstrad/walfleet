-- Values that may differ between machines: the work directory, and the size
-- of the database file when the copier copied it (as the copier reported it).

.print
.print '── info. Work directory ───────────────────────────────────────────────'

SELECT work_dir, db_bytes_at_copy FROM info_run ORDER BY work_dir;

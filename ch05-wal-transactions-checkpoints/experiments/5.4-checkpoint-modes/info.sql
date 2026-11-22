-- Values that differ on every run: how long each checkpoint took. The ones
-- near 300 ms waited out the busy_timeout of the checkpointer.

.print
.print '── info. Elapsed time of each checkpoint ──────────────────────────────'

SELECT mode, reader, elapsed_ms FROM info_elapsed ORDER BY seq;

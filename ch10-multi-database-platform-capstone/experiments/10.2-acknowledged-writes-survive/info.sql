-- Run-dependent values of experiment 10.2: where the store and the work
-- directory were, how long the write loops took, and what each random kill
-- left. The evidence check skips them.

.print
.print '── Info: this run ──────────────────────────────────────────────────────'
.print

SELECT name, value FROM info_run ORDER BY name;

.print
.print 'The write loop of the unkilled interactive run of each mode:'

SELECT mode, writes, elapsed_ms, acknowledged FROM info_write_loops ORDER BY mode;

.print
.print 'Each random kill draw: the delay, and the user_versions per database'
.print '(db-0001 to db-0004) that were acknowledged and recovered:'

SELECT mode, kill_no, draw, delay_ms, acknowledged, reopened,
       acknowledged_versions, recovered_versions
FROM info_random_kills ORDER BY mode, kill_no, draw;

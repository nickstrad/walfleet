-- Run-dependent values of experiment 10.1: where the store and the work
-- directory were, and each reply's bounds sample with its memory reading.
-- The evidence check skips them.

.print
.print '── Info: this run ──────────────────────────────────────────────────────'
.print

SELECT name, value FROM info_run ORDER BY name;

.print
.print 'The bounds sample each node sent after a command (memory varies by run):'

SELECT step, node, bounds FROM info_bounds ORDER BY step;

-- Run-dependent values of experiment 10.3: where the store and the work
-- directory were, and how many node replies carried a bounds check.
-- The evidence check skips them.

.print
.print '── Info: this run ──────────────────────────────────────────────────────'
.print

SELECT name, value FROM info_run ORDER BY name;

-- Run-dependent values of experiment 10.4: where the store and the work
-- directory were, the descriptor limit, byte counts, the passes of the
-- concurrent checker, the standing-bounds summaries and how long each phase
-- took. The evidence check skips them.

.print
.print '── Info: this run ──────────────────────────────────────────────────────'
.print

SELECT name, value FROM info_run ORDER BY name;

.print
.print 'How long each phase took:'

SELECT phase, ms FROM info_timings ORDER BY seq;

.print
.print 'Each writer when it closed, with its standing assertion:'

SELECT writer, reply, summary FROM info_writers ORDER BY writer;

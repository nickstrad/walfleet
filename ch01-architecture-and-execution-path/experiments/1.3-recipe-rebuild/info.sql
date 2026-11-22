-- Run-dependent values of experiment 1.3: the work directory, timings, and the
-- hashes of runs whose bytes differ between runs (random(), the clock, the
-- rowid). The evidence check skips them.

.print
.print '── Info: this run ──────────────────────────────────────────────────────'
.print

SELECT key, value FROM info_run ORDER BY key;

.print
.print 'sha256 of the files of the variants, by run:'

SELECT name, run, sha256 FROM info_hashes ORDER BY name, run;

-- Run-dependent values of experiment 4.1: the sync syscall each program used,
-- timings, and the random nonce of each journal the kills left behind.

.print
.print 'Sync syscalls and timings of this run:'

SELECT name, value FROM info ORDER BY seq;

.print
.print 'Journal nonce and run time of each kill:'

SELECT part, step, journal_mode, synchronous, kill_no, printf('0x%08x', journal_nonce) AS journal_nonce, run_ms
FROM info_kills ORDER BY part, step, journal_mode, synchronous, kill_no;

-- Run-dependent values of experiment 8.1 (not stored as evidence):
--   info_first_statement   part A: time of the first statement, per schema, in this process

.print
.print '── Info (varies between runs and machines) ─────────────────────────────'
.print
.print 'Time of the first statement (schema load included), measured in the'
.print 'parent process outside strace:'

SELECT tables, elapsed_us FROM info_first_statement ORDER BY tables;

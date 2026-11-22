-- Walkthrough of experiment 10.2. Run it with:
--   go run ./cmd/course run 10.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings        one setting of the run (name, value)
--   parts           one part of the experiment, whether it ran, and why not
--   uninterrupted   one run that was not killed: the baseline of parts A and B
--   fsync_kills     part A: one run killed by strace on entry to a chosen fsync
--   random_kills    part B: one kill at a random time, after its redraws
--   recovery        parts A and C: one database after the recovery of one killed run
--   violations      one database a recovery found broken or missing an acknowledged write
--   self_test       part C: the reply of the mutant node
--   problems        one problem or over-bounds line a node reported (none on a good run)
--   checks          every claim the experiment asserted (the runner lists it last)
-- info_run, info_write_loops and info_random_kills hold what depends on timing:
-- the random kill delays and what each random kill left. info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.2  Does every acknowledged write survive a killed process?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'One node owns 4 databases and keeps at most 2 open, so every round-robin'
.print 'write evicts one. Each killed run sends 12 writes in one mode:'
.print
.print '  local-durable   acknowledged after the local commit at synchronous=FULL.'
.print '                  Recovery restarts the node on its own files.'
.print '  store-durable   acknowledged after the segment put to the S3 store.'
.print '                  Recovery deletes the directory of the node first and'
.print '                  rebuilds every database from the store alone.'
.print
.print 'After each kill, every database must reopen with integrity ok, be a recipe'
.print 'state, and hold at least the last user_version acknowledged for it.'

SELECT name, value FROM settings ORDER BY seq;

SELECT part, name, ran, note FROM parts ORDER BY part;

.print
.print '── A. Kills at a chosen fsync (strace) ─────────────────────────────────'
.print
.print 'A1. The uninterrupted run counts the fsync and fdatasync calls of the node.'
.print '    All of them run on its command thread. The process exits at the end of'
.print '    its input without closing, so no close-time sync is counted.'

SELECT mode, how, commands, acknowledged, command_thread_fsyncs, exit_code
FROM uninterrupted WHERE part = 'A' ORDER BY mode;

.print
.print 'A2. Four kill points per mode, evenly spaced over those calls: the first,'
.print '    the last, and two between. The file is the one the killed fsync was'
.print '    syncing; "a" is the directory of the node. during_command is the command'
.print '    the node was running (empty if the kill came after the last reply).'

SELECT mode, kill_no AS kill, at_fsync || ' of ' || of_fsyncs AS fsync, file,
       during_command_no || ': ' || during_command AS during_command, acknowledged, reopened
FROM fsync_kills ORDER BY mode, kill_no;

SELECT killed, count(*) AS kills FROM fsync_kills GROUP BY killed ORDER BY killed;

.print
.print 'A3. What each database held after the recovery. recovered is never below'
.print '    acknowledged; it can be above, when the kill hit after the commit but'
.print '    before the reply.'

SELECT mode, kill_no,
       string_agg(recovered_version::VARCHAR, ' ' ORDER BY database)    AS recovered,
       string_agg(acknowledged_version::VARCHAR, ' ' ORDER BY database) AS acknowledged,
       count(*) FILTER (integrity = 'ok')                               AS integrity_ok,
       count(*) FILTER (recovered_version >= acknowledged_version)      AS holds_acks
FROM recovery WHERE part = 'A' GROUP BY mode, kill_no ORDER BY mode, kill_no;

.print
.print 'A4. Every recovered database matched a recipe step:'

SELECT mode, recovered_state, count(*) AS databases
FROM recovery WHERE part = 'A' GROUP BY mode, recovered_state ORDER BY mode, recovered_state;

.print
.print '── B. Kills at a random time (portable) ───────────────────────────────'
.print
.print 'B1. One interactive run per mode is not killed and times its write loop.'
.print '    Each random kill then waits a random part of that time. A kill that'
.print '    lands after the last reply is recovered and checked too, then redrawn,'
.print '    at most 5 draws per kill. The delays and what each kill left depend on'
.print '    timing; they are in info_random_kills.'

SELECT mode, how, commands, acknowledged FROM uninterrupted WHERE part = 'B' ORDER BY mode;

SELECT mode, kill_no, in_flight, violations FROM random_kills ORDER BY mode, kill_no;

.print
.print '── C. Self-test: acknowledge before the segment put ────────────────────'
.print
.print 'C1. A mutant node acknowledges one store-durable write before it puts the'
.print '    segment, and is killed. Its reply says stored through position 0.'

SELECT reply, acknowledged FROM self_test ORDER BY reply;

.print
.print 'C2. The rebuild from the store finds the acknowledged write missing, and'
.print '    the checker reports it: the check can fail.'

SELECT database, acknowledged_version, recovered_version, recovered_state
FROM recovery WHERE part = 'C' ORDER BY database;

SELECT part, mode, kill_no, line FROM violations ORDER BY part, mode, kill_no, line;

.print
.print '── D. Every node process ──────────────────────────────────────────────'
.print
.print 'D1. Problems and over-bounds lines the nodes reported, across all runs.'
.print '    Both counts are 0; any line would be listed in the table problems.'

SELECT count(*) FILTER (kind = 'problem')     AS problems,
       count(*) FILTER (kind = 'over bounds') AS over_bounds
FROM problems;

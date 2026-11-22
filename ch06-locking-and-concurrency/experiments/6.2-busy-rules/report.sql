-- Walkthrough of experiment 6.2. Run it with:
--   go run ./cmd/course run 6.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps          one statement or action one child process ran, in the order the answers came back
--   settings       one part: journal_mode and busy_timeout of both connections
--   recipe_result  part E: the user_version the last writer saw and the recipe step the closed file equals
--   checks         every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  6.2  The busy rules'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Two child processes, each with one connection to the same database, run'
.print 'scripted steps in turn. Every step is numbered in the order it was sent.'
.print 'A step can also be sent without waiting for its answer: the parent then'
.print 'checks that no answer came within 250ms (at_250ms below), runs other steps,'
.print 'and collects the answer later, so its row appears after theirs.'
.print
.print 'Every step must answer within 30s. A statement that waited out a busy'
.print 'timeout of 120000ms would fail the run instead of returning late.'
.print
.print 'The settings of each part (the same on both connections):'

SELECT part, journal_mode, busy_timeout_ms FROM settings ORDER BY part;

.print
.print '── A. WAL mode: a reader and a writer, busy_timeout 0 ─────────────────'
.print
.print 'With busy_timeout 0 any wait would show up as SQLITE_BUSY. None does: the'
.print 'writer commits beside an open reader (steps 3-5), the reader keeps its'
.print 'snapshot (step 6 still counts 1 row), a new reader reads the last committed'
.print 'state while a write is open (step 11), and BEGIN EXCLUSIVE (step 16) starts'
.print 'beside an open reader, exactly like BEGIN IMMEDIATE.'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'A' ORDER BY seq;

.print
.print '── B. WAL mode: two writers, busy_timeout 0 ───────────────────────────'
.print
.print 'BEGIN DEFERRED and a read succeed; the first write returns SQLITE_BUSY'
.print '(step 25). BEGIN IMMEDIATE returns SQLITE_BUSY at the BEGIN itself (step 27).'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'B' ORDER BY seq;

.print
.print '── C. WAL mode: two writers, busy_timeout 120000ms ────────────────────'
.print
.print 'C1. BEGIN IMMEDIATE waits for the lock. Step 31 is sent while writer-1'
.print '    holds the lock, has no answer after 250ms, and returns SQLITE_OK once'
.print '    writer-1 commits (step 32).'

SELECT step, process, action,
       CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END AS at_250ms,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C1' ORDER BY seq;

.print
.print 'C2. A deferred transaction that has not read yet also waits, at its first'
.print '    write (step 38), and then succeeds.'

SELECT step, process, action,
       CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END AS at_250ms,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C2' ORDER BY seq;

.print
.print 'C3. A deferred transaction that has already read (step 42) cannot wait: its'
.print '    first write returns SQLITE_BUSY at once (step 45), despite the 120000ms'
.print '    timeout. After the other writer commits, the same write returns'
.print '    SQLITE_BUSY_SNAPSHOT (step 47): the snapshot it read is no longer the'
.print '    newest. Only a restarted transaction succeeds (steps 49-51).'

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'C3' ORDER BY seq;

.print
.print '── D. Rollback mode (journal_mode=delete), busy_timeout 0 ─────────────'
.print
.print "The contrast. The writer takes RESERVED and writes beside the reader's SHARED"
.print 'lock, but its COMMIT needs EXCLUSIVE and returns SQLITE_BUSY (step 56). The'
.print 'failed COMMIT keeps PENDING, so a new read also returns SQLITE_BUSY (step 58)'
.print "until the writer's second COMMIT succeeds (step 59)."

SELECT step, process, action,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'D' ORDER BY seq;

.print
.print '── E. Recipe steps 6-15 under contention ──────────────────────────────'
.print
.print 'The database is the recipe built to step 5. Two writers apply steps 6-15'
.print 'in turn, each in its own BEGIN IMMEDIATE transaction, busy_timeout 120000ms.'
.print 'Each BEGIN IMMEDIATE is sent while the other writer still holds the lock.'
.print
.print 'E1. One row per recipe step: who applied it, whether its BEGIN IMMEDIATE'
.print '    was still waiting at 250ms (the first writer of a pair began alone),'
.print '    and how its BEGIN and COMMIT ended.'

WITH e AS (SELECT * FROM steps WHERE part = 'E'),
     x AS (SELECT seq, process, CAST(split_part(action, ' ', 2) AS INTEGER) AS recipe_step
           FROM e WHERE action LIKE 'recipe-exec %')
SELECT x.recipe_step,
       x.process,
       (SELECT CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END
        FROM e WHERE e.process = x.process AND e.action = 'begin immediate' AND e.seq < x.seq
        ORDER BY e.seq DESC LIMIT 1) AS begin_at_250ms,
       (SELECT result FROM e WHERE e.process = x.process AND e.action = 'begin immediate' AND e.seq < x.seq
        ORDER BY e.seq DESC LIMIT 1) AS begin_result,
       (SELECT e.result FROM e WHERE e.seq = x.seq) AS exec_result,
       (SELECT result FROM e WHERE e.process = x.process AND e.action = 'commit' AND e.seq > x.seq
        ORDER BY e.seq LIMIT 1) AS commit_result
FROM x ORDER BY x.recipe_step;

.print
.print 'E2. The full transcript.'

SELECT step, process, action,
       CASE waiting_at_250ms WHEN true THEN 'no answer' WHEN false THEN 'answered' END AS at_250ms,
       result || CASE WHEN code <> 0 THEN ' (' || code || ')' ELSE '' END AS result,
       value
FROM steps WHERE part = 'E' ORDER BY seq;

.print
.print 'E3. The user_version the last writer saw, and the recipe step the closed'
.print '    file equals (exact = false: equal except the change counter at bytes'
.print '    24-27 and 92-95).'

SELECT user_version, matched_step, exact FROM recipe_result ORDER BY part;

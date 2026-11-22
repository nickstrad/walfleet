-- Walkthrough of experiment 10.1. Run it with:
--   go run ./cmd/course run 10.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings       one setting of the model or of the service run (name, value)
--   model_runs     part A: one exploration of the ownership model (the model under test or a mutant)
--   model_traces   part A: one step of the shortest violating interleaving a mutant has
--   steps          part B: one step of the stale-owner scenario and the node's reply
--   model_steps    part B: one model action the scenario step maps to, and its outcome
--   closes         part B: one node's reply when it was closed, with its standing-bounds summary
--   problems       part B: one problem or over-bounds line a node reported (none on a good run)
--   slots          part C: one slot of a database's chain, read back from S3 or predicted by the model
--   catalog        part C: one database's catalog row and the restore of its last position
--   acks           part C: one acknowledged write and whether the store holds it
--   checks         every claim the experiment asserted (the runner lists it last)
-- info_run and info_bounds hold the store endpoint, the work directory and each
-- reply's memory sample; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.1  Can a fenced owner ever get a commit accepted?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Part A checks the ownership rule on a model: every interleaving up to a'
.print 'bounded depth, plus seeded random walks. Part B runs the same rule on two'
.print 'real node processes that share a catalog and a SeaweedFS S3 store. Part C'
.print 'reads the store back after part B and compares it with the model.'

.print
.print '── A. The ownership model, every interleaving up to a bounded depth ───'
.print
.print 'The model: nodes A and B, one database, the epoch counter of the catalog, a'
.print 'store of position slots with put-if-absent. Actions per node: claim, fence,'
.print 'fence-with-stale-list, write, ship, crash, restart, restart-with-stale-list.'
.print 'The bound on local positions counts positions across all epochs and fences,'
.print 'not commits per node.'

SELECT name, value FROM settings WHERE name LIKE 'model_%' ORDER BY seq;

.print
.print 'Invariants after every step: no two epochs accepted for overlapping'
.print 'positions; no epoch accepted after a newer one was; no accepted object'
.print 'overwritten.'
.print
.print 'A1. The correct model has no violating interleaving and no violating walk.'
.print '    Each mutant disables one half of the fencing rule, and the checker finds'
.print '    the violation: the checker can fail.'

SELECT model, depth, interleavings, distinct_states AS states, violating,
       walks, walk_steps, walk_violating
FROM model_runs ORDER BY seq;

.print
.print 'A2. The shortest violating interleaving of each mutant. With'
.print '    store-accepts-every-put, the stale listing of A lets its fence overwrite'
.print '    the accepted object of epoch 1. With resume-from-global-end, B restarts'
.print '    at the end of the store and ships behind the fence of epoch 2.'

SELECT r.model, r.violation, count(*) AS steps
FROM model_runs r JOIN model_traces t USING (model)
GROUP BY r.seq, r.model, r.violation ORDER BY r.seq;

SELECT t.model, t.step_no, t.action, t.outcome
FROM model_traces t JOIN model_runs r USING (model)
ORDER BY r.seq, t.step_no;

.print
.print '── B. The stale-owner scenario on two service processes ───────────────'
.print
.print 'Nodes a and b share the catalog and the store. The store wrapper of node b'
.print 'hides the newest slot from its first listing (fault_on_node_b). Every'
.print 'database is opened with the SQLite settings listed after the store.'

SELECT name, value FROM settings WHERE name NOT LIKE 'model_%' ORDER BY seq;

.print
.print 'B1. Fifteen steps and the replies. Steps 1-7 run on db-0001: b takes over'
.print '    while a still runs, and a is fenced on its next write (steps 5 and 6).'
.print '    Steps 8-15 run on db-0002: b is killed and restarts with its .owner'
.print '    belief file, and is fenced again (step 13) while it keeps db-0001.'

SELECT step, node, command, split_part(reply, ' ', 1) AS result
FROM steps ORDER BY step;

.print
.print 'B2. What each reply says after the command. A claim names the new epoch'
.print '    and how many fence attempts it took; a write names the epoch and'
.print '    user_version it was acknowledged at, and the store position for a'
.print '    store-durable write.'

SELECT step, split_part(reply, ' ', 1) AS result, substr(reply, strpos(reply, ': ') + 2) AS detail
FROM steps WHERE reply IS NOT NULL ORDER BY step;

.print
.print 'B3. The model actions each step maps to, and their outcomes. In step 4 the'
.print '    stale listing makes the first fence of B retry, as it did for node b'
.print '    (attempts 2).'

SELECT step, database, seq, action, outcome FROM model_steps ORDER BY step, database, seq;

.print
.print 'B4. The model agrees with every command step. The descriptors column is the'
.print '    open descriptors of the node above its baseline after the command: 7 per'
.print '    open database (3 connections), 14 for two, 0 when it holds none open.'

SELECT step, node, command, descriptors, model_agrees FROM steps
WHERE reply IS NOT NULL ORDER BY step;

SELECT count(*) AS command_steps,
       count(*) FILTER (model_agrees) AS model_agrees
FROM steps WHERE reply IS NOT NULL;

.print
.print 'B5. Closing each node. The standing assertion checked descriptors and memory'
.print '    after every command against the bounds for at most 4 open databases.'

SELECT node, reply,
       regexp_extract(summary, 'standing assertion: (\d+) checks', 1)::INT AS bound_checks,
       replace(replace(regexp_extract(summary, 'descriptors above baseline <= (\d+ \([^)]*\))', 1),
                       ' (', ' = '), ')', '')                              AS max_descriptors,
       regexp_extract(summary, 'memory growth <= (.*)$', 1)                AS max_memory_growth
FROM closes ORDER BY node;

.print
.print 'B6. Problems and over-bounds lines the nodes reported. Both counts are 0;'
.print '    any line would be listed in the table problems.'

SELECT count(*) FILTER (kind = 'problem')     AS problems,
       count(*) FILTER (kind = 'over bounds') AS over_bounds
FROM problems;

.print
.print '── C. The store after the run, read back from S3 ───────────────────────'
.print
.print 'C1. The slots in the store and the slots the model predicts after the same'
.print '    steps. They are the same: one lineage per database, epochs never'
.print '    decreasing, each epoch opening with a fence snapshot.'

SELECT s.database, s.first, s.last, s.epoch, s.kind AS store_kind, m.kind AS model_kind,
       m.epoch = s.epoch AND m.last = s.last AND m.kind = s.kind AS same
FROM slots s FULL JOIN slots m
  ON m.source = 'model' AND s.database = m.database AND s.first = m.first
WHERE s.source = 'store'
ORDER BY s.database, s.first;

.print
.print 'C2. The catalog names the newest epoch and the last stored position. The'
.print '    restore of that position is a recipe state, and the closed local file of'
.print '    the owner is byte-identical (E1) to it.'

SELECT database, owner, epoch, last_shipped, schema_version, location FROM catalog ORDER BY database;

SELECT database, restored_position, restored_state, file_equals_store FROM catalog ORDER BY database;

.print
.print 'C3. Every acknowledgement. Each store-durable one is in the store at the'
.print '    position its reply named. The local-durable acknowledgement of'
.print '    db-0001 user_version 2 was never shipped, and the takeover lost it, as'
.print '    that mode allows.'

SELECT database, mode, epoch, user_version, position, in_store FROM acks ORDER BY seq;

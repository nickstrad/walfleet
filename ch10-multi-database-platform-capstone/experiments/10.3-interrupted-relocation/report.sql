-- Walkthrough of experiment 10.3. Run it with:
--   go run ./cmd/course run 10.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings            one setting of the run (name, value)
--   relocation_steps    one of the seven steps of a relocation
--   cases               one fault case: the fault, the recovery and the final state (case 0 is the self-test)
--   boundaries          one step boundary of one case: the catalog, the end of the store, both nodes
--   slots               one slot of the store chain after one case
--   failed_invariants   one invariant a case broke (none on a good run)
--   problems            one problem or over-bounds line a node reported (none on a good run)
--   checks              every claim the experiment asserted (the runner lists it last)
-- info_run holds the store endpoint, the work directory and the number of node
-- replies that carried a bounds check; info.sql shows them.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  10.3  Can an interrupted relocation leave two authoritative copies?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Node a (the source) moves db-0001 to node b (the destination). Both share'
.print 'the catalog and the S3 store. In each case, a claims db-0001 and writes'
.print 'steps 1-2 store-durable and step 3 local-durable. At every step boundary'
.print 'the client writes store-durable to the node the catalog names and'
.print 'local-durable to the other. One fault per case; then a fresh coordinator'
.print 'resumes or rolls back from the relocations row and the store.'

SELECT name, value FROM settings ORDER BY seq;

.print
.print 'The seven steps of a relocation:'

SELECT step, name FROM relocation_steps ORDER BY step;

.print
.print '── A. Thirty-two fault cases ───────────────────────────────────────────'
.print
.print 'A1. The faults. Cases 1-13 stop the coordinator after a step; 14-21 kill'
.print '    a node after a step; 22-31 make one store put fail or crash the node'
.print '    around it (log/000000000004 is the final segment of a, 000000000006'
.print '    the fence of b); 32 rolls back while the destination stays down.'

SELECT case_no AS "case", fault, fault_node AS node, after_step AS after, recovery
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A2. How each run stopped and how the recovery ended. Killing b after step'
.print '    6 (case 18) does not interrupt the run: step 7 talks only to a. A'
.print '    roll back after step 6 rolls forward instead (case 13): the fence of b'
.print '    is already in the store.'

SELECT case_no AS "case", run, restarted, outcome, relocation_state AS state,
       relocation_step AS step
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A3. Case 32: node b stays down during the roll back. The roll back still'
.print '    finishes; b keeps a staged copy it could not be told to discard, and'
.print '    is restarted after the recovery.'

SELECT case_no AS "case", stayed_down, discard_error
FROM cases WHERE stayed_down IS NOT NULL OR discard_error IS NOT NULL ORDER BY case_no;

.print
.print 'A4. The fence point is the last position of the source; the fence is the'
.print '    snapshot the new owner puts right after it. The rebuilt copy equals the'
.print '    source there (the same sha256). A roll back fences a again, at epoch 3.'

SELECT case_no AS "case", fence_point, fence_point_sha256 AS sha256_prefix, fence,
       owner, epoch, newest_slot_epoch AS slot_epoch
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A5. Writes. Every acknowledged write is in the one lineage and no refused'
.print '    write is: the final user_version equals the number acknowledged. The'
.print '    final state is that recipe step; its bytes differ only in the change'
.print '    counter (bytes 24-27 and 92-95).'

SELECT case_no AS "case", acknowledged, store_durable, refused,
       final_user_version AS user_version,
       regexp_extract(final_state, '^step \d+')                               AS recipe,
       CASE WHEN final_state LIKE '%exact%' THEN 'exact' ELSE 'counter differs' END AS vs_recipe_file,
       most_writable, holds
FROM cases WHERE case_no > 0 ORDER BY case_no;

.print
.print 'A6. The step boundaries of four cases. In case 7 the coordinator stops'
.print '    after the last step; in case 16, a is killed after step 3; in case 18,'
.print '    b is killed after step 6; in case 32, b stays down. Before step 6 only'
.print '    a is writable; from step 6 on only b is. store_end is the last slot.'

SELECT case_no AS "case", label, owner, epoch,
       CASE WHEN store_end_epoch IS NULL THEN store_end
            ELSE store_end || ' e' || store_end_epoch || ' ' || store_end_kind END AS store_end,
       node_a, node_b
FROM boundaries WHERE case_no IN (7, 16, 18, 32) ORDER BY case_no, seq;

.print
.print 'A7. Every boundary of every case: how many nodes accepted writes. In the'
.print '    fault cases never more than one, and none while a is sealed and b is'
.print '    not yet active. The self-test (part B) reaches two.'

SELECT CASE WHEN case_no = 0 THEN 'self-test' ELSE 'fault cases' END AS cases,
       (node_a LIKE '%writable')::INT + (node_b LIKE '%writable')::INT AS writable_nodes,
       count(*) AS boundaries
FROM boundaries GROUP BY ALL ORDER BY cases, writable_nodes;

.print
.print 'A8. The store after each case: one lineage, epochs never decreasing, and'
.print '    each new epoch opens with a fence snapshot. Each slot is shown as'
.print '    positions, epoch, and snap (snapshot) or seg (segment).'

SELECT case_no AS "case",
       string_agg(first || '-' || last || ' e' || epoch || ' ' ||
                  CASE kind WHEN 'snapshot' THEN 'snap' WHEN 'segment' THEN 'seg' ELSE kind END,
                  '; ' ORDER BY first) AS store
FROM slots WHERE case_no > 0 GROUP BY case_no ORDER BY case_no;

.print
.print '    In some cases the final segment never reached the store, and the close'
.print '    of the source had already checkpointed its frames into the .db file. The'
.print '    retried drain found no WAL and shipped a discontinuity snapshot at'
.print '    position 4 instead of a segment, in these cases:'

SELECT string_agg(case_no::VARCHAR, ' ' ORDER BY case_no) AS cases, first AS position, epoch, kind
FROM slots WHERE case_no > 0 AND kind = 'snapshot' AND epoch = 1 AND first > 1
GROUP BY first, epoch, kind ORDER BY first;

.print
.print 'A9. The cases that hold every invariant, and the invariants broken (none):'

SELECT count(*) FILTER (holds) || ' of ' || count(*) AS cases_holding_every_invariant,
       (SELECT count(*) FROM failed_invariants) AS broken_invariants
FROM cases WHERE case_no > 0;

.print
.print '── B. Self-test: the same relocation without the seal and the fence ────'
.print
.print 'B1. The skip-fence mutant does not seal a and does not put the fence of'
.print '    b. From step 6 on, both nodes are writable, and the store is no longer'
.print '    one lineage: epoch 2 starts with a segment, not a fence snapshot.'

SELECT run, outcome, relocation_state AS state, fence_point, fence, most_writable
FROM cases WHERE case_no = 0 ORDER BY case_no;

SELECT label, owner, epoch,
       CASE WHEN store_end_epoch IS NULL THEN store_end
            ELSE store_end || ' e' || store_end_epoch || ' ' || store_end_kind END AS store_end,
       node_a, node_b
FROM boundaries WHERE case_no = 0 ORDER BY seq;

SELECT audit_error FROM cases WHERE case_no = 0 ORDER BY case_no;

.print
.print '── C. Every node process ───────────────────────────────────────────────'
.print
.print 'C1. Problems and over-bounds lines the nodes reported, across all cases.'
.print '    Both counts are 0; any line would be listed in the table problems.'

SELECT count(*) FILTER (kind = 'problem')     AS problems,
       count(*) FILTER (kind = 'over bounds') AS over_bounds
FROM problems;

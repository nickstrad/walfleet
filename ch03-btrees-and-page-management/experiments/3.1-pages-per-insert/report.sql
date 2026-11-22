-- Walkthrough of experiment 3.1. Run it with:
--   go run ./cmd/course run 3.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   inserts            one row per single-row transaction, all parts
--   steps              the inserts the walkthrough looks at closely, by label
--   page_states        what the pages of those inserts hold, before and after
--   predictions        part A arithmetic, computed before any insert
--   unchanged_frames   part B: WAL frames whose page bytes did not change
--   workloads          part E: the four workloads and the leaves they end with
--   checks             every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  3.1  How many pages does one INSERT change?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Every row below is one single-row transaction. After each commit the'
.print 'experiment reads the frames in -wal, runs a TRUNCATE checkpoint, and'
.print 'compares a sha256 manifest of every page with the manifest before.'
.print
.print 'Settings: page_size=4096, one connection, wal_autocheckpoint=0,'
.print 'synchronous=OFF (skips fsync only; no file byte depends on it).'
.print
.print 'What was measured, by part:'

SELECT part,
       journal_mode,
       count(*)                                         AS inserts,
       min(pages_changed) || '..' || max(pages_changed) AS pages_changed,
       sum(wal_frames)                                  AS wal_frames
FROM inserts
GROUP BY part, journal_mode
ORDER BY part;

.print
.print '── A. Appends into an empty table ─────────────────────────────────────'
.print
.print 'Table t(id INTEGER PRIMARY KEY, v BLOB of 1000 bytes), rowids 2, 4, 6, ...'
.print
.print 'A1. The prediction, from the cell format alone, before any insert:'

SELECT name, value, how FROM predictions ORDER BY seq;

.print
.print 'A2. Prediction against measurement:'

WITH a AS (SELECT * FROM inserts WHERE part = 'A'),
     p AS (SELECT name, value FROM predictions)
SELECT 'first split at insert #' AS what,
       (SELECT value FROM p WHERE name = 'first_split_insert') AS predicted,
       (SELECT min(insert_no) FROM a WHERE pages_added > 0)    AS measured
UNION ALL
SELECT 'inserts between splits',
       (SELECT value FROM p WHERE name = 'split_every_inserts'),
       (SELECT mode(gap) FROM (SELECT insert_no - lag(insert_no) OVER (ORDER BY insert_no) AS gap
                               FROM a WHERE pages_added > 0))
UNION ALL
SELECT 'third level at insert #',
       (SELECT value FROM p WHERE name = 'third_level_insert'),
       (SELECT min(insert_no) FROM a WHERE depth = 3);

.print
.print 'A3. Pages changed per insert. Most inserts change one page: the leaf.'
.print '    Every insert that changes more is a split (it adds pages).'

SELECT pages_changed,
       count(*)                                           AS inserts,
       round(100.0 * count(*) / sum(count(*)) OVER (), 1) AS pct,
       bar(count(*), 0, max(count(*)) OVER (), 30)        AS share,
       count(*) FILTER (pages_added > 0)                  AS splits,
       min(insert_no)                                     AS first_at_insert
FROM inserts
WHERE part = 'A'
GROUP BY pages_changed
ORDER BY pages_changed;

.print
.print 'A4. The inserts worth a close look. Compare changed_pages with wal_pages:'
.print '    the frames of a commit name exactly the pages the manifest shows changed.'

SELECT s.label AS step,
       i.insert_no, i.rowid, i.pages_changed, i.changed_pages, i.wal_pages, i.depth
FROM steps s JOIN inserts i USING (part, insert_no)
WHERE s.part = 'A'
ORDER BY i.insert_no;

.print
.print 'A5. The same inserts page by page, before and after.'
.print
.print 'Insert #5, the first split. The root (page 2) stays the root: it turns'
.print 'into an interior page, its four cells move to new page 3, and new page 4'
.print 'takes the new row. Page 1 changes because the file grew.'

SELECT page, state, kind, cells, keys, unused, children, note
FROM page_states WHERE part = 'A' AND label = 'first split, depth 1 -> 2' ORDER BY seq;

.print
.print 'Insert #9, an append split at depth 2. Three pages: page 1, one more'
.print 'divider in the root, one new leaf. The full leaf is not rewritten.'

SELECT page, state, kind, cells, keys, unused, children, note
FROM page_states WHERE part = 'A' AND label = 'first append split at depth 2' ORDER BY seq;

.print
.print 'Insert #14, an ordinary insert. One page: the leaf that holds the row.'

SELECT page, state, kind, cells, keys, unused, children, note
FROM page_states WHERE part = 'A' AND label = 'an ordinary insert' ORDER BY seq;

.print
.print 'Insert #2053. The root is full (unused = 3), so the tree gains a third'
.print 'level: the root keeps one divider and two new interior pages share the rest.'

SELECT page, state, kind, cells, keys, unused, children, note
FROM page_states WHERE part = 'A' AND label = 'depth 2 -> 3' ORDER BY seq;

.print
.print 'A6. The whole run. The last column counts commits whose WAL frames'
.print '    differ from the changed pages; it must be 0.'

SELECT count(*)                      AS inserts,
       sum(wal_frames)               AS wal_frames,
       sum(wal_frames) * (4096 + 24) AS wal_bytes,
       max(page_count)               AS final_pages,
       max(depth)                    AS final_depth,
       count(*) FILTER (wal_pages IS DISTINCT FROM changed_pages) AS frames_differ
FROM inserts
WHERE part = 'A';

.print
.print '── B. A middle insert into a full leaf ─────────────────────────────────'
.print
.print 'B1. The tree after 20 appends: a root and five full leaves.'

SELECT page, kind, cells, keys, unused, children
FROM page_states WHERE part = 'B' AND label = 'before the middle insert' ORDER BY seq;

.print
.print 'B2. Insert rowid 11. It belongs in page 4, which is full.'

SELECT i.insert_no, i.rowid, i.pages_changed, i.changed_pages, i.wal_pages
FROM steps s JOIN inserts i USING (part, insert_no)
WHERE s.part = 'B';

.print
.print '    Not one new page as in part A: the full leaf and its right sibling'
.print '    are rewritten, and the root gains a divider in the middle.'

SELECT page, state, kind, cells, keys, unused, children, note
FROM page_states WHERE part = 'B' AND label = 'middle split' ORDER BY seq;

.print
.print 'B3. wal_pages has one page more than changed_pages. balance_nonroot rewrote'
.print '    that sibling with the cells it already held: same bytes, still a frame.'

SELECT page, sha256_before, sha256_after, sha256_before = sha256_after AS same_bytes
FROM unchanged_frames WHERE part = 'B';

.print
.print 'B4. The leaves in key order after the split:'

SELECT page, cells, keys, unused
FROM page_states WHERE part = 'B' AND label = 'leaves in key order after the middle split' ORDER BY seq;

.print
.print '── C. The same insert with two secondary indexes ──────────────────────'
.print
.print 'The common insert now changes three pages: the table leaf and one leaf'
.print 'in each index.'

SELECT pages_changed,
       count(*)                                    AS inserts,
       bar(count(*), 0, max(count(*)) OVER (), 30) AS share,
       count(*) FILTER (pages_added > 0)           AS splits
FROM inserts
WHERE part = 'C'
GROUP BY pages_changed
ORDER BY pages_changed;

.print
.print 'One of the common inserts:'

SELECT i.insert_no, i.changed_roles
FROM steps s JOIN inserts i USING (part, insert_no)
WHERE s.part = 'C';

.print
.print '── D. The same appends in rollback mode (journal_mode=DELETE) ─────────'
.print
.print 'There is no WAL. Every commit also rewrites page 1 (the change counter).'

SELECT insert_no, rowid, changed_roles, wal_pages
FROM inserts WHERE part = 'D' AND insert_no <= 2 ORDER BY insert_no;

SELECT pages_changed, page1_changed, count(*) AS inserts,
       first(changed_pages ORDER BY insert_no) AS first_example_pages
FROM inserts
WHERE part = 'D'
GROUP BY pages_changed, page1_changed
ORDER BY pages_changed;

.print
.print '── E. Where "most inserts change one page" stops holding ──────────────'
.print
.print 'Four workloads, one row per transaction, WAL. Each insert is one of:'
.print '  one_page    it changed one page'
.print '  added_page  the file grew (a split, or an overflow page)'
.print '  rebalanced  several pages changed, none added (cells moved to siblings)'

SELECT w.workload,
       count(*)                                                    AS inserts,
       count(*) FILTER (i.pages_added = 0 AND i.pages_changed = 1) AS one_page,
       count(*) FILTER (i.pages_added > 0)                         AS added_page,
       count(*) FILTER (i.pages_added = 0 AND i.pages_changed > 1) AS rebalanced,
       min(i.pages_changed) || '..' || max(i.pages_changed)        AS pages_changed
FROM inserts i
JOIN workloads w USING (workload)
WHERE i.part = 'E'
GROUP BY w.seq, w.workload
ORDER BY w.seq;

.print
.print 'The leaves each workload ends with:'

SELECT workload, row_bytes, leaves,
       min_rows_per_leaf || '..' || max_rows_per_leaf AS rows_per_leaf, final_depth
FROM workloads ORDER BY seq;

.print
.print 'Splits at depth 2, by pages changed. An append split of a rowid table'
.print 'costs 3 pages (part A); most splits here cost more.'

WITH e AS (
    SELECT *, lag(depth) OVER (PARTITION BY workload ORDER BY insert_no) AS depth_before
    FROM inserts WHERE part = 'E'
)
SELECT w.workload, e.pages_changed, count(*) AS splits
FROM e JOIN workloads w USING (workload)
WHERE e.pages_added > 0 AND e.depth_before = 2 AND e.depth = 2
GROUP BY w.seq, w.workload, e.pages_changed
ORDER BY w.seq, e.pages_changed;

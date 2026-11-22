-- Walkthrough of experiment 3.2. Run it with:
--   go run ./cmd/course run 3.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   settings           the constants of the experiment, one row per name
--   steps              the file at rest after each of six steps, per secure_delete setting
--   delete_step        step 2 in detail: the pages that held the rows, the freelist, the WAL frames
--   reuse_step         step 3 in detail: the leaf pages of the new rows and where they came from
--   grow_batches       step 4: one row per transaction of 4 rows, with freelist and page_count around it
--   residue            one row per search of the file for the markers of deleted rows
--   large_transaction  part D: the one transaction that needs more pages than the freelist holds
--   auto_vacuum        part E: one row per auto_vacuum mode
--   wal_residue        part F: deleted rows still readable in the -wal, by moment
--   checks             every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  3.2  Does a DELETE return space or erase bytes?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Table t(id INTEGER PRIMARY KEY, v BLOB). Each blob is 1000 bytes and repeats'
.print 'a marker such as SECRET-000150, so a deleted row can be searched for in the'
.print 'file. Parts A and B run the same six steps with secure_delete=OFF and ON.'
.print 'Every step is its own connection, so the file is at rest and checkpointed'
.print 'when it is read. Settings: journal_mode=wal, synchronous=FULL.'
.print
.print 'The constants:'

SELECT name, value FROM settings ORDER BY name;

.print
.print '── A. secure_delete=OFF (the default) ─────────────────────────────'
.print
.print 'A1. The file after each step. Steps: 1 insert the rows, 2 delete ids 99..302,'
.print '    3 insert 100 new rows (ids 1001..1100), 4 insert 30 transactions of 4 rows,'
.print '    5 delete ids >= 1001, 6 VACUUM. Only VACUUM makes the file smaller.'

SELECT step, what, file_bytes, pages, freelist_pages,
       trunk_pages AS trunk, freelist_leaves, table_leaf_pages
FROM steps WHERE secure_delete = 'OFF' ORDER BY step;

.print
.print 'A2. Step 2: the delete empties leaf pages and moves them to the freelist.'
.print '    Compare held_on_leaf_pages with freelist_pages. wal_frames is what the'
.print '    commit of the DELETE wrote to the -wal.'

SELECT rows_deleted, held_on_leaf_pages, freelist_pages, former_leaves_freed, wal_frames
FROM delete_step WHERE secure_delete = 'OFF';

.print
.print 'A3. Step 3: the new rows go into former freelist pages, so the file does not grow.'

SELECT new_rows, new_leaf_pages, new_leaf_count, taken_from_freelist
FROM reuse_step WHERE secure_delete = 'OFF';

.print
.print 'A4. Step 4: page_count grows only when the freelist is empty. Batch 1, every'
.print '    batch that grew the file, and the batch that emptied the freelist:'

SELECT batch, freelist_before, freelist_after, pages_before, pages_after
FROM grow_batches
WHERE secure_delete = 'OFF'
  AND (batch = 1 OR pages_after <> pages_before
       OR (freelist_before <> freelist_after AND freelist_after = 0))
ORDER BY batch;

.print
.print 'A5. Searching the file at rest for the markers of the deleted rows. roles names'
.print '    the kind of page each marker was found on.'

SELECT step, markers_searched, rows_sought, rows_found, pages_with_markers, roles
FROM residue WHERE secure_delete = 'OFF' ORDER BY step;

.print
.print '── B. secure_delete=ON ─────────────────────────────'
.print
.print 'B1. The file after each step. Steps: 1 insert the rows, 2 delete ids 99..302,'
.print '    3 insert 100 new rows (ids 1001..1100), 4 insert 30 transactions of 4 rows,'
.print '    5 delete ids >= 1001, 6 VACUUM. Only VACUUM makes the file smaller.'

SELECT step, what, file_bytes, pages, freelist_pages,
       trunk_pages AS trunk, freelist_leaves, table_leaf_pages
FROM steps WHERE secure_delete = 'ON' ORDER BY step;

.print
.print 'B2. Step 2: the delete empties leaf pages and moves them to the freelist.'
.print '    Compare held_on_leaf_pages with freelist_pages. wal_frames is what the'
.print '    commit of the DELETE wrote to the -wal.'

SELECT rows_deleted, held_on_leaf_pages, freelist_pages, former_leaves_freed, wal_frames
FROM delete_step WHERE secure_delete = 'ON';

.print
.print 'B3. Step 3: the new rows go into former freelist pages, so the file does not grow.'

SELECT new_rows, new_leaf_pages, new_leaf_count, taken_from_freelist
FROM reuse_step WHERE secure_delete = 'ON';

.print
.print 'B4. Step 4: page_count grows only when the freelist is empty. Batch 1, every'
.print '    batch that grew the file, and the batch that emptied the freelist:'

SELECT batch, freelist_before, freelist_after, pages_before, pages_after
FROM grow_batches
WHERE secure_delete = 'ON'
  AND (batch = 1 OR pages_after <> pages_before
       OR (freelist_before <> freelist_after AND freelist_after = 0))
ORDER BY batch;

.print
.print 'B5. Searching the file at rest for the markers of the deleted rows. roles names'
.print '    the kind of page each marker was found on.'

SELECT step, markers_searched, rows_sought, rows_found, pages_with_markers, roles
FROM residue WHERE secure_delete = 'ON' ORDER BY step;

.print
.print '── C. Comparison ───────────────────────────────────────────────────────'
.print
.print 'C1. secure_delete changes the bytes, not the layout: the file size after each'
.print '    step is the same in both runs.'

SELECT step, what,
       max(file_bytes) FILTER (secure_delete = 'OFF') AS bytes_off,
       max(file_bytes) FILTER (secure_delete = 'ON')  AS bytes_on
FROM steps GROUP BY step, what ORDER BY step;

.print
.print 'C2. What a reader of the file finds, and what the DELETE cost in WAL frames:'

SELECT d.secure_delete,
       max(r.rows_found) FILTER (r.step = 2) AS readable_after_delete,
       max(r.rows_found) FILTER (r.step = 3) AS readable_after_reuse,
       max(r.rows_found) FILTER (r.step = 5) AS readable_after_second_delete,
       any_value(d.wal_frames)               AS delete_wal_frames
FROM delete_step d JOIN residue r USING (secure_delete)
GROUP BY d.secure_delete ORDER BY d.secure_delete;

.print
.print '── D. A transaction that needs more pages than the freelist holds ──────'
.print
.print 'secure_delete=ON, WAL. 40 rows, DELETE of ids 1..8, then one transaction of 32'
.print 'new rows (8 leaves worth). It takes the freelist pages first and then grows'
.print 'the file, all in one transaction.'

SELECT rows_inserted, deleted_rows,
       freelist_before, freelist_after, page_count_before, page_count_after
FROM large_transaction;

.print
.print '── E. auto_vacuum ──────────────────────────────────────────────────────'
.print
.print 'The file shrinks on its own only when the database was created with'
.print 'auto_vacuum FULL or INCREMENTAL. Each mode: 200 rows, DELETE of 150 rows,'
.print 'then PRAGMA incremental_vacuum, each in its own connection.'

SELECT mode, pages_created, pages_after_delete, freelist_after_delete, pages_after_incremental_vacuum
FROM auto_vacuum
ORDER BY CASE mode WHEN 'NONE' THEN 1 WHEN 'FULL' THEN 2 ELSE 3 END;

.print
.print '── F. secure_delete=ON and the -wal file ───────────────────────────────'
.print
.print 'wal_autocheckpoint=0, one connection. 40 rows are inserted, then deleted. How'
.print 'many of the 40 deleted rows can still be read in the -wal, and in the database'
.print 'file at rest?'

SELECT location, rows_readable, rows_total FROM wal_residue ORDER BY seq;

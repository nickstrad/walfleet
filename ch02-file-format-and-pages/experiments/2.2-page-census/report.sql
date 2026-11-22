-- Walkthrough of experiment 2.2. Run it with:
--   go run ./cmd/course run 2.2        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   files             one row per database file the census ran on, with SQLite's page counts
--   roles             one row per scenario and page role: how many pages have that role
--   freelist_pages    one row per freelist page, in the order the file lists them
--   constants         one row per derived or observed value (formula in note)
--   special_pages     one row per pointer-map or lock-byte page
--   schema_entries    part A: one row per entry of the schema table on page 1
--   trees             part A: one row per b-tree walked from a schema root
--   pages             part A: one row per page, our census beside the dbstat row
--   overflow_chains   part A: one row per documents row that has overflow pages
--   row_hashes        part A: one row per recipe table, rows hashed both ways
--   payload_splits    part B: one row per blob around the local-payload limit
--   reject_base_pages part F: one row per page of the undamaged file
--   rejections        part F: one row per damaged copy, with both verdicts
--   checks            every claim the experiment asserted (the runner lists it last)

-- Pages of one scenario by role, in the order of the census, with the total last.
CREATE TEMP MACRO role_counts(s) AS TABLE
    SELECT role, pages FROM (
        SELECT seq, role, pages FROM roles WHERE scenario = s
        UNION ALL
        SELECT 1000, 'total', sum(pages) FROM roles WHERE scenario = s
    ) ORDER BY seq;

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  2.2  Can a parser account for every page of a database file?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'internal/dbfile reads a closed database file and gives every page one'
.print 'role: b-tree interior or leaf, overflow, freelist trunk or leaf, pointer'
.print 'map, or the lock-byte page. The experiment checks the census against'
.print 'SQLite (PRAGMA page_count, freelist_count, the dbstat virtual table, a'
.print 'SELECT of every table), builds files that stress the corners, and tries'
.print 'to break the parser with nine damaged files.'

.print
.print '── A. Census of the R0 database (recipe default, WAL, 30 steps, closed) ──'
.print
.print 'A1. The file as the census and SQLite see it. The page count comes from the'
.print 'in-header size; both page counts and both freelist lengths must agree.'

SELECT page_size, usable_size, page_count, page_count_source,
       pragma_page_count AS sqlite_pages, freelist_count AS free_pages, pragma_freelist_count AS sqlite_free
FROM files WHERE scenario = 'r0';

.print
.print 'A2. The schema table on page 1, decoded by our record reader. The sql column'
.print 'is cut into lines of at most 50 characters.'

SELECT CASE WHEN g.i = 0 THEN s.entry END    AS entry,
       CASE WHEN g.i = 0 THEN s.type END     AS type,
       CASE WHEN g.i = 0 THEN s.name END     AS name,
       CASE WHEN g.i = 0 THEN s.tbl_name END AS tbl_name,
       CASE WHEN g.i = 0 THEN s.rootpage END AS rootpage,
       substr(s.sql, g.i * 50 + 1, 50)       AS sql
FROM schema_entries s, generate_series(0, (length(s.sql) - 1) // 50) AS g(i)
ORDER BY s.entry, g.i;

.print
.print 'A3. The b-trees walked from those roots.'

SELECT name, type, root_page, btree_pages, overflow_pages, cells FROM trees ORDER BY name;

.print
.print 'A4. The freelist from header offset 32: the trunk and the leaves it lists.'

SELECT string_agg(CASE WHEN kind = 'trunk' THEN CAST(page AS VARCHAR) END, ' ' ORDER BY seq) AS trunks,
       string_agg(CASE WHEN kind = 'leaf'  THEN CAST(page AS VARCHAR) END, ' ' ORDER BY seq) AS leaves
FROM freelist_pages WHERE scenario = 'r0';

.print
.print 'A5. Pages by role. Every page has exactly one role, so the roles sum to the'
.print 'page count (270) and none is unaccounted for.'

SELECT role, pages FROM role_counts('r0');

.print
.print 'A6. The page-role census against dbstat, page by page. Every b-tree and'
.print 'overflow page must match on tree, type, cell count, payload, unused bytes and'
.print 'largest payload. The other pages are not b-tree pages, so dbstat does not list them.'

SELECT page, role, tree, ncell, payload, unused, mx_payload AS mx_pay,
       CASE WHEN coalesce(kind, '') = ''
            THEN CASE WHEN db_type IS NULL THEN 'not a b-tree page (not in dbstat)' ELSE 'LISTED BY DBSTAT' END
            WHEN db_type IS NULL THEN 'MISSING FROM DBSTAT'
            WHEN tree = db_tree AND kind = db_type AND ncell = db_ncell AND payload = db_payload
                 AND unused = db_unused AND mx_payload = db_mx_payload THEN 'match'
            ELSE 'DIFFERS'
       END AS dbstat
FROM pages ORDER BY page;

.print
.print 'A7. The verdicts in total.'

SELECT CASE WHEN coalesce(kind, '') = ''
            THEN CASE WHEN db_type IS NULL THEN 'not a b-tree page (not in dbstat)' ELSE 'LISTED BY DBSTAT' END
            WHEN db_type IS NULL THEN 'MISSING FROM DBSTAT'
            WHEN tree = db_tree AND kind = db_type AND ncell = db_ncell AND payload = db_payload
                 AND unused = db_unused AND mx_payload = db_mx_payload THEN 'match'
            ELSE 'DIFFERS'
       END AS dbstat,
       count(*) AS pages
FROM pages GROUP BY ALL ORDER BY dbstat;

.print
.print 'A8. Overflow chains of the documents table: the first five rows that overflow,'
.print 'then the totals over all of them.'

SELECT row_id, body_bytes, leaf_page, overflow_pages AS overflow_chain
FROM overflow_chains ORDER BY row_id LIMIT 5;

SELECT count(*)            AS rows_that_overflow,
       sum(chain_length)   AS overflow_pages_in_all,
       max(chain_length)   AS longest_chain
FROM overflow_chains;

.print
.print 'A9. Every row of every recipe table, hashed from the page bytes by our parser'
.print 'and read through SELECT ... ORDER BY rowid with every value tagged by typeof().'
.print 'The first 16 hex digits of each sha256 are shown.'

SELECT table_name, our_rows, sqlite_rows,
       left(our_sha256, 16) AS our_sha256, left(sqlite_sha256, 16) AS sqlite_sha256,
       our_sha256 = sqlite_sha256 AS same
FROM row_hashes ORDER BY table_name;

.print
.print '── B. A payload exactly at the local limit, and one byte over ───────────'
.print
.print 'A table leaf at usable size 4096 keeps a whole payload when it is at most'
.print 'maxLocal. A larger one keeps only minLocal bytes (or maxLocal, when the'
.print 'overflow would then fill its last page exactly) and spills the rest.'

SELECT name, value, note FROM constants WHERE part = 'B' ORDER BY name;

.print
.print 'B1. Three blobs. The payload is the blob plus a 4-byte record header.'
.print 'Row 1 is exactly maxLocal and stays on the leaf; row 2 is one byte over.'

SELECT row_id, blob_bytes, payload_bytes, local_bytes, overflow_pages, last_overflow_bytes
FROM payload_splits ORDER BY row_id;

.print
.print 'B2. The census of that file against SQLite.'

SELECT scenario, page_count, pragma_page_count AS sqlite_pages,
       freelist_count AS free_pages, pragma_freelist_count AS sqlite_free, btree_pages_compared, not_in_dbstat
FROM files WHERE part = 'B' ORDER BY scenario;

.print
.print '── C. A freelist longer than one trunk page ────────────────────────────'
.print
.print 'Page size 1024, 600 rows of 1000 bytes inserted and then all deleted.'

SELECT name, value, note FROM constants WHERE part = 'C' ORDER BY name;

.print
.print 'C1. The trunks, in the order the file chains them, with how many leaves each lists.'

SELECT seq, page AS trunk_page, leaves_listed
FROM freelist_pages WHERE scenario = 'freelist' AND kind = 'trunk' ORDER BY seq;

.print
.print 'C2. Trunks plus leaves are the free pages, and they agree with SQLite.'

SELECT count(*) FILTER (kind = 'trunk') AS trunks,
       count(*) FILTER (kind = 'leaf')  AS leaves,
       count(*)                         AS free_pages,
       any_value(f.page_count)          AS of_pages,
       any_value(f.pragma_freelist_count) AS pragma_freelist_count
FROM freelist_pages p, files f
WHERE p.scenario = 'freelist' AND f.scenario = 'freelist';

.print
.print 'C3. Pages by role.'

SELECT role, pages FROM role_counts('freelist');

.print
.print '── D. auto_vacuum=FULL adds pointer-map pages ──────────────────────────'
.print
.print 'The course runs auto_vacuum=NONE; the parser still accounts for pointer maps.'
.print 'Page size 1024, 400 rows of 900 bytes, auto_vacuum=FULL.'

SELECT name, value, note FROM constants WHERE part = 'D' ORDER BY name;

.print
.print 'D1. The pointer-map pages found: page 2, then one every spacing pages.'

SELECT scenario, role, page FROM special_pages WHERE scenario = 'autovacuum' ORDER BY page;

.print
.print 'D2. Pages by role.'

SELECT role, pages FROM role_counts('autovacuum');

.print
.print 'D3. The census against SQLite.'

SELECT scenario, page_count, pragma_page_count AS sqlite_pages,
       freelist_count AS free_pages, pragma_freelist_count AS sqlite_free, btree_pages_compared, not_in_dbstat
FROM files WHERE part = 'D' ORDER BY scenario;

.print
.print '── E. A file larger than 1 GiB: the lock-byte page ─────────────────────'
.print
.print 'The page that holds byte offset 1073741824 is never used. The file is built'
.print 'by editing a 2-page database with page size 65536: the in-header size is'
.print 'raised, page 3 becomes a freelist trunk that lists every later page except the'
.print 'lock-byte page, and the rest of the file is a hole (sparse).'

SELECT name, value, note FROM constants WHERE part = 'E' ORDER BY name;

.print
.print 'E1. Pages by role, and the lock-byte page the census found.'

SELECT role, pages FROM role_counts('large');

SELECT role, page FROM special_pages WHERE scenario = 'large' ORDER BY page;

.print
.print 'E2. SQLite accepts the file: its page count and freelist count equal the census.'

SELECT scenario, page_count, pragma_page_count AS sqlite_pages, freelist_count AS free_pages,
       pragma_freelist_count AS sqlite_free
FROM files WHERE part = 'E' ORDER BY scenario;

.print
.print '── F. What the inspector must reject ───────────────────────────────────'
.print
.print 'One damaged structure per file, in a 6-page rollback-mode copy. For each,'
.print 'what our parser says and what PRAGMA integrity_check says. Texts are cut'
.print 'into lines of at most 100 characters.'
.print
.print 'F0. The pages of the undamaged file (table t has root page 2):'

SELECT string_agg(page || '=' || role, ' ' ORDER BY page) AS pages FROM reject_base_pages;

.print
.print 'F1. The nine damaged copies, one structure each.'

SELECT case_no, damage FROM rejections ORDER BY CAST(substr(case_no, 2) AS INTEGER);

.print
.print 'F2. What each parser says about each copy. Our parser must return an error'
.print 'without panicking. SQLite does not always object: for F2 it reads the missing'
.print 'half of the last page as zeros and calls the file ok.'

WITH texts AS (
    SELECT case_no, 1 AS ord, 'ours' AS who, ours AS text FROM rejections
    UNION ALL
    SELECT case_no, 2, 'SQLite', sqlite FROM rejections
)
SELECT CASE WHEN ord = 1 AND g.i = 0 THEN case_no END AS case_no,
       CASE WHEN g.i = 0 THEN who END                 AS who,
       substr(text, g.i * 85 + 1, 85)                 AS verdict
FROM texts, generate_series(0, (length(text) - 1) // 85) AS g(i)
ORDER BY CAST(substr(case_no, 2) AS INTEGER), ord, g.i;

# Chapter 3 — B-trees and page management

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `btree.c`, `btreeInt.h` |
| Linked C sources (in the pinned tree, not copied) | `pager.c`, `backup.c`, `vacuum.c`, `tool/dbhash.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to invoke this repository's experiment runner (`cmd/course`) from the host. Build the `lab` container image (`Dockerfile.lab`) once, from the repository root:
>   ```text
>   make docker-build
>   ```
>   The image bundles the pinned SQLite `3.53.4` source tree, the compiled SQLite C tools (`sqlite3`, `showwal`, and `dbhash` in `/opt/course-tools/bin`; `dbhash` computes a logical SHA-1 digest of a database and backs Experiment 3.3's `dbhash` cross-check column), `strace`, and the DuckDB CLI that renders each experiment's CSV results as a walkthrough.
> - **Run the experiments** from the repository root. Each command starts the `lab` container, runs the experiment inside it, and prints the rendered walkthrough; add `-i` (`make run EXP=3.1 ARGS='-i'`) to stay in the DuckDB shell afterwards with all result tables loaded:
>   ```text
>   make run EXP=3.1
>   make run EXP=3.2
>   make run EXP=3.3
>   ```
>   Inside the container, scratch databases and CSV tables are written under `/work/3.1/`, `/work/3.2/`, and `/work/3.3/` (the Docker `lab-work` volume). When run directly on the host, they go under `_work/3.1/`, `_work/3.2/`, and `_work/3.3/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** Each experiment is an ordinary Go program (`make run-native EXP=3.1`). Run directly on the host, it prints only its verdict and leaves its CSV tables under `_work/3.1/results/`. Nothing in Chapter 3 is Linux-only (no `strace`, no `/proc`, and no required `sqlite3` CLI), so all three experiments run in full on macOS too. Experiment 3.3's `dbhash` column then needs `_tools/bin/dbhash` (`make fetch-source` to fetch the pinned SQLite source tree, then `make build-tools` to compile the C tools, which requires a C compiler such as Xcode Command Line Tools); without it, Experiment 3.3 prints a `NO-DBHASH:` line, skips the `dbhash` checks, and still exits 0.
> - **Measured platform:** The stored evidence in this chapter was captured in the Debian `lab` container on `linux/arm64` (Go 1.26).

---

## 1. Model

Every SQLite table and index is stored as a **B-tree**: a balanced search tree whose nodes are fixed-size **pages** (4,096 bytes by default). A B-tree starts at a single **root page** whose page number is recorded in `sqlite_schema.rootpage` (in SQLite's catalog table on page 1) and never moves even as the tree grows deeper. At height 1, the root page is itself a **leaf page** holding rows or index keys. When the root fills up, SQLite copies its contents to a new child page, turns the root into an **interior page** holding divider keys and child page pointers, and raises the tree's depth by one.

SQLite uses two kinds of B-tree:

- **Table B-tree (`intKey`, B+-tree).** Used by every ordinary (rowid) table. Keys are 64-bit signed integers called **rowids**, and row payloads live **only on leaf pages** (page-type byte `0x0d`). Interior pages (`0x05`) store no row data—only 4-byte child page numbers and integer **divider keys** (each giving the largest rowid in its left child), plus a 4-byte **rightmost child pointer** in the page header. Because interior cells are tiny (7 or 8 bytes for small keys), one 4,096-byte interior page holds hundreds of children.
- **Index B-tree.** Used by every secondary index and by `WITHOUT ROWID` tables. Keys are arbitrary binary records, and both **index-leaf** (`0x0a`) and **index-interior** (`0x02`) pages store full key payloads. Index B-trees do not use SQLite's append-only leaf-splitting shortcut (`balance_quick`), so every split redistributes cells across siblings.

Inside a single B-tree page, two regions grow toward each other from opposite ends:

```text
Single B-tree page (4,096 bytes; page 1 starts at offset 100 after the file header)
+-------------------------------------------------------------------------+
| B-tree page header (8 B on a leaf, 12 B on an interior page)            |
|   [0]    page type (0x0d table-leaf, 0x05 table-interior, 0x0a/0x02)    |
|   [1..2] first freeblock offset (0 if none)                             |
|   [3..4] cell count (nCell)                                             |
|   [5..6] start of cell content area (content-start; 0 means 65,536)     |
|   [7]    fragmented free bytes (sum of 1..3-byte gaps)                  |
|   [8..11] rightmost child page number (interior pages only)             |
+-------------------------------------------------------------------------+
| Cell pointer array: nCell x 2-byte offsets, sorted by key               |
|   grows DOWNWARD  |                                                     |
|                   v                                                     |
|                  ... unallocated space (contiguous gap) ...             |
|                   ^                                                     |
|   grows UPWARD    |                                                     |
| Cell content area: variable-length cells + freeblocks (>= 4 B each)     |
|   (cells need not be in key order; cell pointers keep them sorted)      |
+-------------------------------------------------------------------------+

Tree growth and page recycling
  Root page (fixed page number in sqlite_schema.rootpage)
    |
    +--> interior pages (dividers + child page numbers)
           |
           +--> leaf pages (cells) ---> overflow page chain (if payload > max local)

  Freelist (header offset 32 = first trunk, offset 36 = total free pages):
    page 1[32] -> freelist-trunk -> next trunk -> ...
                    |-- array of 4-byte freelist-leaf page numbers
                    |-- allocateBtreePage pops leaves first, then the trunk
                    +-- file grows only when freelist_count == 0
```

Key terms used in this chapter:

- **Cell and cell pointer.** A **cell** is the serialized on-disk entry for one row (on a table leaf), one divider (on a table interior page), or one key (on an index page). Each cell is addressed by a 2-byte big-endian offset in the **cell pointer array**, which stays sorted by key so binary search works without moving cell bytes.
- **Freeblock and fragment.** Deleting a cell returns its bytes inside the cell content area to a sorted linked list of **freeblocks** (each at least 4 bytes: a 2-byte next-freeblock offset and a 2-byte size). Leftover gaps of 1 to 3 bytes are too small for a freeblock header; they are called **fragments** and summed in page header byte 7. If a new cell fits in total free space but not in any single freeblock or the middle gap, SQLite compacts (**defragments**) the page.
- **Overflow cell and overflow page.** These are two different things. An **overflow cell** (`MemPage.apOvfl`) is an in-memory slot holding a cell that did not fit onto a full B-tree page during an insert, triggering `balance()` to split or redistribute the page. An **overflow page** is an on-disk page holding the tail of a single row whose payload exceeds the page's local limit (`4,061` bytes on a 4,096-byte table leaf), linked in a chain with a 4-byte next-page pointer at the start of each page.
- **Freelist trunk and freelist leaf.** Under `auto_vacuum=NONE` (SQLite's default), deleting rows and emptying a page does not shrink the file. The emptied page joins the **freelist**: a linked list of **freelist-trunk** pages (starting at file header offset 32) where each trunk holds an array of 4-byte page numbers pointing to **freelist-leaf** pages. Later allocations pop pages from the freelist before extending the file.
- **WAL frame and checkpoint.** In Write-Ahead Logging (**WAL**) mode, committing a transaction appends a 24-byte frame header plus a full 4,096-byte page image—one **WAL frame** (`4,120` bytes)—to the sibling `-wal` file for every page SQLite marked dirty. A **checkpoint** (`PRAGMA wal_checkpoint(...)`) copies committed frames back into the main `.db` file; `PASSIVE` copies without blocking writers or resetting `-wal`, while `TRUNCATE` resets `-wal` to zero bytes once no reader needs it.
- **`fsync` and `PRAGMA synchronous`.** `fsync` flushes modified file data and metadata from the OS buffer cache to durable storage. `PRAGMA synchronous=OFF` skips `fsync` calls without changing any bytes written to the database file or the WAL frame sequence.
- **`PRAGMA secure_delete`.** Per-connection setting (default `OFF`). When `OFF`, SQLite skips zeroing deleted cells' freeblocks on live pages and marks freed freelist-leaf pages "don't write", leaving old row bytes intact on disk. When `ON`, SQLite zeroes freed bytes and whole freed pages before writing them. (`FAST` mode zeroes live-page deletions while leaving freelist leaves unwritten.)
- **`PRAGMA auto_vacuum` and `VACUUM` / `VACUUM INTO`.** `auto_vacuum` is a persistent file-header setting chosen before the first table is created: `NONE` (0, default) puts freed pages on the freelist and never shrinks the file; `FULL` (1) maintains **pointer-map** pages and relocates pages to truncate the file at every deleting commit; `INCREMENTAL` (2) keeps freed pages on the freelist until `PRAGMA incremental_vacuum` moves and truncates them. `VACUUM` rebuilds the database into a compact file with zero freelist pages; `VACUUM INTO 'path'` writes that rebuilt copy to a new file.
- **Change counter and schema cookie.** Two 4-byte big-endian fields in the 100-byte database header: the **file change counter** (bytes 24–27, paired with **version-valid-for** at bytes 92–95, which SQLite checks against the change counter before trusting the in-header page count) increments when page 1 is written on a commit; the **schema cookie** (bytes 40–43) increments on every DDL statement (`CREATE`, `DROP`, `ALTER`) so other connections know to reload `sqlite_schema`.
- **Logical hash and canonical form.** A **logical hash** (equality level **E3**) hashes a database's schema, rows (with explicit rowids and storage classes), and persistent pragmas independently of physical page layout. A **canonical form** is a deterministic physical rewrite—here `VACUUM INTO`—that maps logically equivalent databases with the same schema history to byte-identical files so they can be verified at **E1** (file SHA-256).

**WAL mode as the platform default.** Platform databases in this book run in WAL mode (`journal_mode=wal`), so WAL is the default throughout Chapter 3. Every experiment database runs in WAL mode except for two deliberate rollback-mode (`journal_mode=delete`) cases:

- **Experiment 3.1 Part D** runs 12 appends in rollback mode to show that rollback commits rewrite page 1 on every transaction (advancing header bytes 24–27 and 92–95), turning each ordinary 1-page WAL insert into a 2-page rollback write.
- **Experiment 3.3 Part A2** compares one-transaction versus per-statement batching in rollback mode to show where commit boundaries *do* appear in the file (header bytes 24–27 and 92–95, matching at equality level **E2**, which compares every page while ignoring volatile header counters), contrasting with WAL mode where append-only commit boundaries leave no byte difference at all.

One WAL-mode consequence also appears in Experiment 3.3: `VACUUM INTO` always writes a **rollback-mode** file (header bytes 18–19 equal `1, 1`). Any platform that restores or compares canonical files must run E1 comparisons before switching the restored file back to WAL mode.

**The Rebuild Ladder (rung R2).** Across this book, the **Rebuild Ladder** climbs from a single reproducible database file up to full recovery, verifying each rebuild against an explicit equality level. Chapter 1 built rung **R0 (Recipe)**—a deterministic 30-step seeded workload with known per-step file hashes—and Chapter 2 built rung **R1 (Pages)**—per-page raw image capture and SHA-256 manifests verified at equality level **E1 bytes** (file SHA-256). Chapter 3 builds rung **R2 (Canonical form)**: capture a database's logical content as an ordered SQL script (`capture/r0.sql`), replay it into a fresh file, prove the rebuild equal to the original at level **E3 logical**, and prove that `VACUUM INTO` brings both to identical bytes at **E1** (within the schema-history boundaries measured in Experiment 3.3). This chapter also completes this repository's shared comparison package, `internal/equal`, by adding levels **E2 pages** (page-by-page comparison ignoring header bytes 24–27, 40–43, and 92–99) and **E3 logical**.

---

## 2. Question

How many pages does one insert or delete change, where do freed pages go, and why do two databases holding the exact same rows end up with different bytes on disk?

This chapter's experiments reuse four shared Go packages already present in this repository (introduced in Chapters 1 and 2, linked here rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite with explicit file and connection settings, checks the pinned `sqlite_source_id()` against the repository's pinned commit, manages `_work/N.M/` scratch directories, runs `PRAGMA wal_checkpoint`, wraps `sqlite3_backup` (`Backup`), and locates built C tools (`LocateTool`).
- [`internal/recipe`](../internal/recipe/): builds the deterministic 30-step R0 database (three tables—`accounts`, `documents`, `events`—and the secondary index `events_by_account`), checks that a database is at rest (`CheckAtRest`: no non-empty `-wal` file and no hot rollback `-journal`), and removes scratch databases (`RemoveDatabase`).
- [`internal/dbfile`](../internal/dbfile/): parses database files in plain Go (header, B-tree pages, cells, 1-to-9-byte big-endian varint lengths, local payload limits, table walks, and full page census).
- [`internal/capture`](../internal/capture/): builds rung R1 per-page hash manifests (`BuildManifest`), used in Experiment 3.1 to cross-check incremental SHA-256 reuse.

Chapter 2 introduced `internal/equal` with level E1 and page manifests in [`internal/equal/equal.go`](../internal/equal/equal.go) and [`internal/equal/manifest.go`](../internal/equal/manifest.go). Chapter 3 completes `internal/equal` by adding two `Level` constants (`E2Pages`, `E3Logical`) and two `Result` fields (`Ignored`, `IgnoredDiffering`) to `equal.go`, plus four new files—`changes.go`, `pages.go`, `logical.go`, and `dbhash.go`—inlined and walked through in Section 5.1.

---

## 3. Call path

All source citations below are at tag `version-3.53.4`. Two files are copied whole into this chapter directory: [`btree.c`](btree.c) and [`btreeInt.h`](btreeInt.h), verified by `SOURCES.sha256` against the pinned SQLite source tree (`$SQLITE_SRC` inside the `lab` container, or `_src/sqlite-3.53.4` on the host after `make fetch-source`). Four additional files cited for specific functions—`pager.c`, `backup.c`, `vacuum.c`, and `tool/dbhash.c`—are linked in that pinned tree. Every line span below is listed in [`SPANS.txt`](SPANS.txt) (this chapter's cited-span manifest) and verified against the pinned source by `make check ARGS='spans ch03'`.

### 3.1 Cells, freeblocks, and page space

1. **Cell and page layout specification** — [`src/btreeInt.h:149-228`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btreeInt.h#L149-L228) (local copy: [`btreeInt.h`](btreeInt.h)). Lines 149–215 specify how cells grow upward from the end of the page while the sorted cell-pointer array grows downward from the page header. Unused bytes inside the cell content area form freeblocks (4 bytes or larger: a 2-byte next offset and 2-byte size) or fragments (1–3 bytes, summed at page header byte 7). The same comment specifies 1-to-9-byte big-endian varints (variable-length integers) for lengths and rowids, along with overflow pages and freelist trunks. Lines 222–229 define `MX_CELL_SIZE` (`pageSize - 8`) and `MX_CELL` (`(pageSize - 8) / 6`, assuming a 4-byte minimum cell plus 2-byte pointer).

### 3.2 Insert path and splitting (`sqlite3BtreeInsert` to `balance`)

The sequence below traces how an insert either finishes on one leaf or enters the `balance()` dispatch loop:

<!-- draw-visual: diagrams/ch03-btrees-and-page-management-insert-balance.mmd -->
```text
  ┌─────────────┐     ┌─────────┐     ┌───────┐
  │ BtreeInsert │     │ balance │     │ Pager │
  └──────┬──────┘     └────┬────┘     └───┬───┘
         │                 │              │
         │ 1. insertCellFast              │
         ├───────────────────────────────►│
         │                 │              │
       ┌────────────────────────────────────┐
       │ cell fits -> return (1 leaf dirty) │
       └────────────────────────────────────┘
         │                 │              │
         │ 2. overflow cell queued        │
         ├────────────────►│              │
         │           ┌─[loop while overflow or >2/3 free]───────┐
         │           │   ┌─[alt root page]────────────────────┐ │
         │           │   │ │              │                   │ │
         │           │   │ │ 3. balance_deeper (new pChild)   │ │
         │           │   │ ├─────────────►│                   │ │
         │           │   ├┈[rightmost intKeyLeaf append]┈┈┈┈┈┈┤ │
         │           │   │ │              │                   │ │
         │           │   │ │ 4. balance_quick (skip old leaf) │ │
         │           │   │ ├─────────────►│                   │ │
         │           │   ├┈[other page or WITHOUT ROWID]┈┈┈┈┈┈┤ │
         │           │   │ │              │                   │ │
         │           │   │ │ 5. balance_nonroot (siblings)    │ │
         │           │   │ ├─────────────►│                   │ │
         │           │   │ │              │                   │ │
         │           │   └────────────────────────────────────┘ │
         │           │     │              │                     │
         │           │┌─────────────────────────┐               │
         │           ││ next: pChild or pParent │               │
         │           │└─────────────────────────┘               │
         │           │     │              │                     │
         │           └──────────────────────────────────────────┘
         │                 │              │
```

2. **`sqlite3BtreeInsert`** — [`src/btree.c:9409-9710`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9409-L9710) (local copy: [`btree.c`](btree.c)). The spine of every row or index insert:
   - Saves other cursors open on the same B-tree (line 9439).
   - Takes a fast path for same-size replacement: if the key already exists and the new cell has the exact same size, `btreeOverwriteCell` (lines 9505 and 9557) overwrites the payload in place—one page, no rebalancing.
   - Otherwise seeks the target leaf (`sqlite3BtreeTableMoveto`, line 9513; the `BTREE_APPEND` hint skips the seek on sequential appends), serializes the cell with `fillInCell` (line 9601), marks the target leaf writable with `sqlite3PagerWrite` (line 9614), clears and drops any replaced cell (`BTREE_CLEAR_CELL`, line 9622; `dropCell`, line 9647), and inserts the cell with `insertCellFast` (line 9656).
   - Calls `balance()` (line 9683) **only** if `insertCellFast` could not fit the cell onto the page and queued it as an overflow cell (`pPage->nOverflow > 0`). When the cell fits, `sqlite3BtreeInsert` returns having marked only that one leaf page dirty.
3. **`fillInCell`** — [`src/btree.c:7074-7257`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7074-L7257) (local copy: [`btree.c`](btree.c)). Writes the payload-length and key varints plus the local payload into a cell buffer. Whenever payload size exceeds the page's local limit (`4,061` bytes on a 4,096-byte table leaf), `fillInCell` allocates overflow pages one at a time via `allocateBtreePage` (line 7212) and chains them with 4-byte next-page pointers.
4. **`insertCellFast`** — [`src/btree.c:7428-7500`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7428-L7500) (local copy: [`btree.c`](btree.c)). A dedicated copy of `insertCell` called only from `sqlite3BtreeInsert` so the compiler inlines it (comment at lines 7418–7427). If `sz + 2 > pPage->nFree`, it stores the cell in `pPage->apOvfl` for `balance()`. Otherwise it calls `allocateSpace` (line 7472), which searches the freeblock list first-fit (`pageFindSlot`), takes bytes from the middle gap, or compacts the page (`defragmentPage`), then shifts the cell-pointer array and writes the 2-byte pointer.
5. **`balance`** — [`src/btree.c:9130-9259`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9130-L9259) (local copy: [`btree.c`](btree.c)). Loops upward from the modified page toward the root until the current page has no overflow cells and its free space is at most two-thirds of usable size (`pPage->nOverflow==0 && pPage->nFree*3 <= (int)pCur->pBt->usableSize*2`, line 9143). At each iteration it dispatches to one of three routines:
   - Root page with an overflow cell → **`balance_deeper`** (line 9158), then continues the loop on the new child page.
   - Non-root rightmost table leaf appending one cell at the end → **`balance_quick`** (lines 9185–9206):
     ```c
     if( pPage->intKeyLeaf
      && pPage->nOverflow==1
      && pPage->aiOvfl[0]==pPage->nCell
      && pParent->pgno!=1
      && pParent->nCell==iIdx
     ){
       ...
       rc = balance_quick(pParent, pPage, aBalanceQuickSpace);
     }
     ```
     Notice `pPage->intKeyLeaf`: **`balance_quick` runs only on rowid table leaves**. Index B-trees and `WITHOUT ROWID` tables have `intKeyLeaf == 0`, so their append splits always fall through to `balance_nonroot`.
   - Every other overfull or underfull non-root page → **`balance_nonroot`** (line 9228), then steps up to `pParent` (`pCur->iPage--`, line 9249) to check whether inserting or removing dividers made the parent overflow or underflow.
6. **`balance_deeper`** — [`src/btree.c:9049-9094`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9049-L9094) (local copy: [`btree.c`](btree.c)). Grows the tree by one level while keeping the root's page number unchanged:
   ```c
   rc = sqlite3PagerWrite(pRoot->pDbPage);
   if( rc==SQLITE_OK ){
     rc = allocateBtreePage(pBt,&pChild,&pgnoChild,pRoot->pgno,0);
     copyNodeContent(pRoot, pChild, &rc);
   ...
   zeroPage(pRoot, pChild->aData[0] & ~PTF_LEAF);
   put4byte(&pRoot->aData[pRoot->hdrOffset+8], pgnoChild);
   ```
   It allocates a new child page (line 9064), copies all cells and overflow cells from `pRoot` to `pChild` (lines 9065 and 9082–9086), clears `pRoot` as an interior page of the same B-tree kind (`~PTF_LEAF`, line 9089), and installs `pChild` as `pRoot`'s rightmost child (line 9090). The next iteration of `balance()` then splits `pChild`—calling `balance_quick` on a first table-leaf overflow, or `balance_nonroot` when a full interior root splits.
7. **`balance_quick`** — [`src/btree.c:8007-8101`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L8007-L8101) (local copy: [`btree.c`](btree.c)). The sequential-append fast path. It allocates one new leaf page (`allocateBtreePage`, line 8025), places only the single overflow cell on it (`rebuildPage`, line 8045), reads the largest rowid from the old leaf's last cell to build a divider cell (lines 8081–8085), inserts that divider at the end of `pParent` (`insertCell`, line 8089), and points `pParent`'s rightmost child at the new leaf (line 8094). **The full old leaf is never rewritten.** At depth 2, an append split modifies exactly three pages: page 1 (in-header `page_count`), the parent interior page, and the new leaf.
8. **`balance_nonroot` header comment and routine** — [`src/btree.c:8204-8244`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L8204-L8244) (comment) and [`src/btree.c:8245-9027`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L8245-L9027) (function, local copy: [`btree.c`](btree.c)). Redistributes cells across the target page and up to two siblings (one on each side, or two on one side at an edge) so all participating pages end with roughly equal free space, adding or freeing up to two pages and updating at most four dividers in `pParent`. Because `balance_nonroot` marks every reused sibling writable via `sqlite3PagerWrite` before rebuilding or editing it, a sibling whose cells end up unchanged still gets written as a WAL frame (observed in Experiment 3.1 Part B).

### 3.3 Delete, page freeing, and reuse

9. **`sqlite3BtreeDelete`, `BTREE_CLEAR_CELL`, `clearCellOverflow`, `dropCell`, and `freeSpace`** — [`src/btree.c:9841-10041`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9841-L10041), [`src/btree.c:7053-7059`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7053-L7059), [`src/btree.c:6979-7045`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6979-L7045), [`src/btree.c:7267-7309`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7267-L7309), and [`src/btree.c:1918-2014`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1918-L2014) (local copy: [`btree.c`](btree.c)). `sqlite3BtreeDelete` expands `BTREE_CLEAR_CELL` (line 9947), which checks whether the cell overflows and calls `clearCellOverflow` to walk and free its overflow chain with `freePage2`. It then calls `dropCell` (line 9948), which shifts the cell-pointer array and calls `freeSpace` (line 7292) to link the cell's bytes into the freeblock list, merging adjacent freeblocks. With `secure_delete` enabled (`BTS_FAST_SECURE` flags), `freeSpace` zeroes those bytes (lines 1992–1996); with `secure_delete=OFF`, the old bytes stay inside the live leaf's freeblock. If the deleted cell sat on an interior page (an index B-tree), SQLite replaces it with the largest cell from its left subtree's leaf (lines 9979–9981). Finally, `sqlite3BtreeDelete` calls `balance()` only if the page now has more than two-thirds of its usable space free (line 10002, calls at lines 10007 and 10016), letting `balance_nonroot` merge underfull siblings and free emptied pages.
10. **`freePage2` and `sqlite3PagerDontWrite`** — [`src/btree.c:6836-6969`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6836-L6969) (local copy: [`btree.c`](btree.c)) and [`src/pager.c:6329-6339`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6329-L6339) (in the pinned tree). `freePage2` increments the freelist count at page 1 offset 36 (lines 6860–6862). If `BTS_SECURE_DELETE` is set, it marks the freed page writable and zeroes all `pageSize` bytes (lines 6864–6874). If an existing trunk has room (`nLeaf < usableSize/4 - 8`, line 6910), `freePage2` appends the freed page number to that trunk:
    ```c
    rc = sqlite3PagerWrite(pTrunk->pDbPage);
    if( rc==SQLITE_OK ){
      put4byte(&pTrunk->aData[4], nLeaf+1);
      put4byte(&pTrunk->aData[8+nLeaf*4], iPage);
      if( pPage && (pBt->btsFlags & BTS_SECURE_DELETE)==0 ){
        sqlite3PagerDontWrite(pPage->pDbPage);
      }
      rc = btreeSetHasContent(pBt, iPage);
    }
    ```
    With `secure_delete=OFF`, `sqlite3PagerDontWrite` sets `PGHDR_DONT_WRITE`, telling the pager not to write the freed leaf page at commit at all. That single optimization leaves deleted row bytes sitting untouched on disk and cuts the WAL frames written by Experiment 3.2's `DELETE` from `54` frames (`ON`) down to `5` frames (`OFF`). When no trunk exists or the first trunk is full, the freed page becomes the new first trunk (lines 6950–6959), overwriting its first 8 bytes (next trunk pointer and leaf count `0`) while leaving its remaining bytes untouched under `secure_delete=OFF`.
11. **`allocateBtreePage`** — [`src/btree.c:6514-6822`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6514-L6822) (local copy: [`btree.c`](btree.c)). Checks the freelist count at page 1 offset 36 (line 6535). Whenever `nFree > 0`, it decrements the count (line 6572), reads the trunk head at offset 32, and pops either a leaf page from the trunk (picking the one closest to `nearby` when requested, lines 6690–6720) or the trunk itself once its leaf count hits zero. Only when the freelist is empty does it increment `pBt->nPage` (line 6778, skipping the 1 GiB lock-byte page reserved for Unix rollback-mode advisory locks) and write the new in-header database size at page 1 offset 28 (line 6800). Because this check runs per page allocation, a single transaction needing more pages than the freelist holds pops every freelist page first and then extends the file during the same commit (Experiment 3.2).

### 3.4 Invariants, backup, `VACUUM INTO`, and `dbhash`

12. **`checkTreePage`** — [`src/btree.c:10855-11118`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L10855-L11118) (local copy: [`btree.c`](btree.c)). The core of `PRAGMA integrity_check`: parses each page (`btreeInitPage`, line 10908), checks free-space arithmetic (`btreeComputeFreeSpace`, line 10914), verifies every cell pointer lies inside the content area (line 10970) and stays within page bounds, checks key ordering against parent divider bounds (line 10986), confirms all children share the same tree depth (line 11018), walks overflow chains (`checkList`), and uses a min-heap of cell and freeblock extents to confirm no byte is used twice (line 11090) and uncovered bytes match header byte 7 (line 11105).
13. **`sqlite3_backup_step`** — [`src/backup.c:323-591`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L323-L591) (in the pinned tree). Copies pages verbatim from source to destination. At completion, line 448 updates the destination's schema cookie to `iDestSchema + 1` so other connections to the destination reload their schema cache. For an empty destination file (`iDestSchema == 0`), the backup ends with schema cookie `1` regardless of the source's schema cookie.
14. **`sqlite3RunVacuum`** — [`src/vacuum.c:143-427`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vacuum.c#L143-L427) (in the pinned tree). For `VACUUM INTO`, refuses an existing non-empty target file (`"output file already exists"`), attaches the target, copies `page_size` and `auto_vacuum`, and recreates tables in `sqlite_schema` rowid order (creation order, with no `ORDER BY`). It then copies every table's rows via `INSERT INTO ... SELECT * FROM`, recreates indexes and other schema objects, copies persistent header meta values (`BTREE_SCHEMA_VERSION` incremented by `+1`, `BTREE_DEFAULT_CACHE_SIZE`, text encoding, `user_version`, `application_id`), and commits. It does **not** copy WAL mode: the output file always has header bytes 18–19 set to `1, 1` (rollback mode).
15. **`hash_one_query` and `main` in `dbhash`** — [`tool/dbhash.c:281-360`](https://github.com/sqlite/sqlite/blob/version-3.53.4/tool/dbhash.c#L281-L360) and [`tool/dbhash.c:379-491`](https://github.com/sqlite/sqlite/blob/version-3.53.4/tool/dbhash.c#L379-L491) (in the pinned tree). `dbhash` is SQLite's bundled C tool for computing a SHA-1 digest of a database's schema and rows. Its `main` opens each database read-write (lines 433–438, so hot journals can recover, printing an error to stderr while still exiting 0 if open fails at line 440), skips tables whose names start with `sqlite_` (`sqlite_sequence`, `sqlite_stat*`), runs `SELECT * FROM t` without `ORDER BY` or explicit `rowid`, and hashes no file-header pragmas. `hash_one_query` writes a 1-byte type tag (`'0'`..`'4'`) followed by 8 big-endian bytes for numbers or raw bytes for TEXT/BLOB—with **no length prefix** and **no row or table delimiter**. Experiment 3.3 Part B proves all six resulting blind spots.

### 3.5 Version notes

- **`insertCellFast` vs `insertCell`.** At SQLite 3.53.4, `sqlite3BtreeInsert` calls `insertCellFast` (`src/btree.c:7428-7500`, comment at lines 7418–7427), a single-caller copy of `insertCell` split out for inlining; older references name `insertCell` on this path (recorded in this repository's source inventory, `docs/knowledge/source-inventory-ch01-04.md`, section 3.7).
- **`BTREE_CLEAR_CELL` and `clearCellOverflow` vs `clearCell()`.** The older `clearCell()` function does not exist in 3.53.4; `sqlite3BtreeDelete` expands `BTREE_CLEAR_CELL` (`src/btree.c:7053-7059`), which calls `clearCellOverflow` (`src/btree.c:6979-7045`) only when a cell overflows (recorded in `docs/knowledge/source-inventory-ch01-04.md`, section 3.7).
- **Freelist trunk capacity (`usableSize/4 - 8`).** A freelist trunk has room for `usableSize/4 - 2` leaf pointers, but `freePage2` (`src/btree.c:6910-6928`) caps it at `usableSize/4 - 8` so database files written by modern SQLite remain readable by versions older than 3.6.0, which had a bounds-check bug on the last six slots (comment in `btree.c:6914-6928`).

### 3.6 Experiment 3.1 — Pages per insert (`3.1-pages-per-insert`)

- **Claim:** On ascending appends of non-overflowing rows into a rowid table in WAL mode, most inserts change one page (the leaf holding the new rowid, never page 1). An insert that fills a leaf changes several pages: 3 pages on a depth-2 append split via `balance_quick`, 4 on the first split/depth change (`1→2`), 5 on the depth change `2→3`, and 5 pages (`6` WAL frames) on a middle split via `balance_nonroot`. The root keeps its page number equal to `sqlite_schema.rootpage` across depth changes, and every changed page appears in its commit's WAL frames (though `balance_nonroot` can also ship an unchanged sibling page as an extra frame). Adding `k` secondary indexes multiplies the common per-insert page count by `1 + k`, rollback mode adds page 1 to every commit, and "most inserts change one page" stops holding for random-key inserts into few-row leaves, overflowing payloads, or `WITHOUT ROWID` append splits.
- **Evidence form:** `EXACT` (every stored CSV table must match a fresh run byte for byte).
- **Why it matters:** A platform that ships WAL frames pays per changed page, so this experiment measures the exact page and frame cost of each insert pattern.

<!-- include: experiments/3.1-pages-per-insert/main.go -->
```go
// Experiment 3.1 measures how many pages and WAL frames each single-row INSERT
// modifies across five workloads: ascending appends through two depth changes
// (Part A), a middle split into a full leaf (Part B), a table with two secondary
// indexes (Part C), rollback-journal mode (Part D), and four boundary workloads
// showing where "most inserts change one page" stops holding (Part E).
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 3.1
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize      = 4096
	blobBytes     = 1000
	rowidStep     = 2
	walHeaderSize = 32
	walFrameHead  = 24
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 3.1" runs both.
// part and workloadName label the rows written while a part runs.
var (
	set          *results.Set
	part         string
	workloadName any
	layoutSeq    int
	workloadSeq  int
	tables       struct {
		inserts, steps, pageStates, predictions, unchangedFrames, workloads *results.Table
	}
)

func openTables() {
	tables.inserts = set.Table("inserts", "part", "workload", "journal_mode", "insert_no", "rowid", "pages_changed", "changed_pages", "changed_roles", "page1_changed", "pages_added", "page_count", "wal_frames", "wal_pages", "depth", "root_page", "integrity")
	tables.steps = set.Table("steps", "part", "label", "insert_no")
	tables.pageStates = set.Table("page_states", "seq", "part", "label", "insert_no", "page", "state", "kind", "cells", "keys", "content_start", "unused", "children", "note")
	tables.predictions = set.Table("predictions", "seq", "name", "value", "how")
	tables.unchangedFrames = set.Table("unchanged_frames", "part", "insert_no", "page", "sha256_before", "sha256_after")
	tables.workloads = set.Table("workloads", "seq", "workload", "table_name", "row_bytes", "leaves", "min_rows_per_leaf", "max_rows_per_leaf", "final_depth")
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, " ")
}

func check(ok bool, claim string) {
	set.Check(part, ok, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func rowBlob(rowid int64) []byte {
	return sizedBlob(rowid, blobBytes)
}

func sizedBlob(rowid int64, size int) []byte {
	label := fmt.Sprintf("row %06d ", rowid)
	return bytes.Repeat([]byte(label), size/len(label)+1)[:size]
}

type lab struct {
	path     string
	mode     string
	db       *sqlitex.DB
	conn     *sql.Conn
	file     *dbfile.File
	manifest equal.Manifest
	image    []byte
	census   *dbfile.Census
}

type step struct {
	number    int
	rowid     int64
	changes   []equal.PageChange
	walPages  []int
	before    *dbfile.File
	after     *dbfile.File
	depth     int
	rootPage  int
	integrity string
}

func openLab(ctx context.Context, path, mode string, statements ...string) *lab {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.File.JournalMode = mode
	// SyncOff skips fsync across ~3,000 single-row commits without altering any
	// file byte or WAL frame sequence; WALAutocheckpoint=0 keeps each commit's
	// frames in -wal until snapshot() reads them and runs TRUNCATE.
	options.Conn.Synchronous = sqlitex.SyncOff
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open "+filepath.Base(path), err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	l := &lab{path: path, mode: mode, db: db, conn: conn}
	for _, s := range statements {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	l.file, l.manifest = l.snapshot(ctx)
	return l
}

func (l *lab) close() {
	if err := l.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := l.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (l *lab) snapshot(ctx context.Context) (*dbfile.File, equal.Manifest) {
	if l.mode == "wal" {
		result, err := sqlitex.Checkpoint(ctx, l.conn, "main", "TRUNCATE")
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	var count int
	if err := l.conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&count); err != nil {
		fatal("page_count", err)
	}
	image := make([]byte, 0, count*pageSize)
	// Read raw pages through sqlite_dbpage instead of os.Open: opening and
	// closing the database file in the same process that holds a SQLite
	// connection drops that process's POSIX locks (see Chapter 6).
	rows, err := l.conn.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	expected := int64(1)
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage", err)
		}
		if pgno != expected || len(data) != pageSize {
			fatal("sqlite_dbpage", fmt.Errorf("page %d (%d bytes), want page %d", pgno, len(data), expected))
		}
		expected++
		image = append(image, data...)
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if err := rows.Close(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if int(expected-1) != count {
		fatal("sqlite_dbpage", fmt.Errorf("%d pages, page_count says %d", expected-1, count))
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse image", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	m := equal.Manifest{PageSize: pageSize, Entries: make([]equal.ManifestEntry, count)}
	for n := 1; n <= count; n++ {
		page := image[(n-1)*pageSize : n*pageSize]
		sum := ""
		// Reuse the previous manifest's SHA-256 when a page's bytes are unchanged;
		// verifyManifest() checks the final manifest against capture.BuildManifest.
		if l.image != nil && n*pageSize <= len(l.image) && bytes.Equal(page, l.image[(n-1)*pageSize:n*pageSize]) {
			sum = l.manifest.Entries[n-1].SHA256
		} else {
			sum = equal.SHA256Hex(page)
		}
		m.Entries[n-1] = equal.ManifestEntry{Page: n, Role: census.Page(n).Role.String(), SHA256: sum}
	}
	l.image = image
	l.census = census
	return f, m
}

func (l *lab) verifyManifest() bool {
	full, err := capture.BuildManifest(l.file)
	if err != nil {
		fatal("manifest", err)
	}
	return full.Text() == l.manifest.Text()
}

// readWALPages parses the 32-byte WAL header and each 24-byte frame header to
// list the page numbers written by the most recent commit. Reading -wal with
// os.ReadFile is safe while SQLite is open because WAL POSIX locks live on -shm.
func readWALPages(path string) ([]int, error) {
	data, err := os.ReadFile(path + "-wal")
	if err != nil {
		return nil, err
	}
	if len(data) < walHeaderSize {
		return nil, fmt.Errorf("-wal holds %d bytes, less than its header", len(data))
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil, fmt.Errorf("-wal magic %#x", magic)
	}
	frameSize := walFrameHead + pageSize
	if (len(data)-walHeaderSize)%frameSize != 0 {
		return nil, fmt.Errorf("-wal size %d is not a header plus whole frames", len(data))
	}
	var pages []int
	for off := walHeaderSize; off < len(data); off += frameSize {
		pages = append(pages, int(binary.BigEndian.Uint32(data[off:off+4])))
	}
	return pages, nil
}

func (l *lab) integrity(ctx context.Context) string {
	var result string
	if err := l.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		fatal("integrity_check", err)
	}
	return result
}

func (l *lab) rootOf(table string) int {
	schema, err := l.file.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name == table {
			return int(e.RootPage)
		}
	}
	fatal("schema", fmt.Errorf("no table %q", table))
	return 0
}

func (l *lab) depth(root int) int {
	deepest := 0
	for n := 1; n <= l.census.PageCount; n++ {
		info := l.census.Page(n)
		if info.Role != dbfile.RoleTableInterior && info.Role != dbfile.RoleTableLeaf && info.Role != dbfile.RoleIndexInterior && info.Role != dbfile.RoleIndexLeaf {
			continue
		}
		d, page := 1, n
		for page != root && page != 0 {
			page = l.census.Page(page).Parent
			d++
		}
		if page == root {
			deepest = max(deepest, d)
		}
	}
	return deepest
}

func (l *lab) exec(ctx context.Context, number int, rowid int64, table, statement string, args ...any) step {
	before := l.file
	if _, err := l.conn.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
	s := step{number: number, rowid: rowid, before: before}
	if l.mode == "wal" {
		pages, err := readWALPages(l.path)
		if err != nil {
			fatal("read -wal", err)
		}
		s.walPages = pages
	}
	file, manifest := l.snapshot(ctx)
	s.changes = equal.ChangedPages(l.manifest, manifest)
	s.after = file
	l.file, l.manifest = file, manifest
	s.rootPage = l.rootOf(table)
	s.depth = l.depth(s.rootPage)
	s.integrity = l.integrity(ctx)
	var walFrames, walPages any
	if l.mode == "wal" {
		walFrames, walPages = len(s.walPages), joinInts(s.walPages)
	}
	tables.inserts.Row(part, workloadName, l.mode, number, rowid, len(s.changes), joinInts(s.pages()),
		strings.Trim(equal.FormatPageChanges(s.changes), "[]"), len(s.changes) > 0 && s.changes[0].Page == 1,
		s.after.PageCount()-s.before.PageCount(), s.after.PageCount(), walFrames, walPages, s.depth, s.rootPage, s.integrity)
	return s
}

func (s step) pages() []int { return equal.PageNumbers(s.changes) }

// leafHoldsRowid verifies that page is a user table leaf (not page 1, whose
// B-tree is sqlite_schema) containing a cell with the given rowid.
func leafHoldsRowid(f *dbfile.File, page int, rowid int64) bool {
	if page == 1 {
		return false
	}
	p, err := f.BTreePage(page)
	if err != nil || p.Type != dbfile.TableLeaf {
		return false
	}
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	for _, c := range cells {
		if c.HasRowid && c.Rowid == rowid {
			return true
		}
	}
	return false
}

func sameInts(a, b []int) bool {
	x := append([]int(nil), a...)
	y := append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func subtract(a, b []int) []int {
	in := map[int]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []int
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func keysOf(p *dbfile.BTreePage) string {
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	if len(cells) == 0 {
		return "none"
	}
	key := func(c dbfile.Cell) string {
		if c.HasRowid {
			return fmt.Sprint(c.Rowid)
		}
		return "?"
	}
	if len(cells) <= 6 {
		var keys []string
		for _, c := range cells {
			keys = append(keys, key(c))
		}
		return strings.Join(keys, ",")
	}
	return key(cells[0]) + ".." + key(cells[len(cells)-1])
}

// childrenOf lists the child pages of an interior page, shortened when there are more than eight.
func childrenOf(p *dbfile.BTreePage) any {
	if p.Type.IsLeaf() {
		return nil
	}
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	var children []string
	for _, c := range cells {
		children = append(children, fmt.Sprint(c.LeftChild))
	}
	children = append(children, fmt.Sprint(p.RightChild))
	if len(children) > 8 {
		children = append(children[:3], "...", children[len(children)-1])
	}
	return strings.Join(children, " ")
}

func mustPage(f *dbfile.File, n int) *dbfile.BTreePage {
	p, err := f.BTreePage(n)
	if err != nil {
		fatal("page", err)
	}
	return p
}

// pageStateRow writes one page_states row: what page n of f holds.
func pageStateRow(label string, insert int, state string, f *dbfile.File, n int) {
	layoutSeq++
	var kind, cells, keys, contentStart, unused, children, note any
	if f == nil || n > f.PageCount() {
		note = "not in the file"
	} else if p, err := f.BTreePage(n); err != nil {
		note = "not a b-tree page"
	} else {
		kind, cells, keys, contentStart, unused, children = p.Type, p.CellCount, keysOf(p), p.CellContentStart, p.Unused(), childrenOf(p)
		if n == 1 {
			keys = nil
			note = fmt.Sprintf("sqlite_schema; header says %d pages", f.Header.DatabaseSize)
		}
	}
	tables.pageStates.Row(layoutSeq, part, label, insert, n, state, kind, cells, keys, contentStart, unused, children, note)
}

// recordStep names one insert in the steps table so the report can point at it.
func recordStep(label string, s step) {
	tables.steps.Row(part, label, s.number)
}

// recordLayout writes every changed page of a step, before and after, to page_states.
func recordLayout(label string, s step) {
	for _, c := range s.changes {
		pageStateRow(label, s.number, "before", s.before, c.Page)
		pageStateRow(label, s.number, "after", s.after, c.Page)
	}
}

type arithmetic struct {
	payload      int
	cellSmall    int
	cellLarge    int
	rowsPerLeaf  int
	dividersFit  int
	firstSplit   int
	deeperInsert int
}

// predict computes leaf capacity, first split, and root interior capacity from
// SQLite's cell format (btreeInt.h:149-228) before any row is inserted, and
// writes each step of the arithmetic to the predictions table.
func predict() arithmetic {
	usable := pageSize
	serialBlob := uint64(12 + 2*blobBytes)
	header := 1 + 1 + dbfile.VarintLength(serialBlob)
	headerVarint := dbfile.VarintLength(uint64(header))
	if headerVarint != 1 {
		fatal("arithmetic", errors.New("record header longer than 127 bytes"))
	}
	payload := header + blobBytes
	maxLocal := dbfile.MaxLocalPayload(usable, dbfile.TableLeaf)
	payloadVarint := dbfile.VarintLength(uint64(payload))
	cell := func(rowid uint64) int {
		return payloadVarint + dbfile.VarintLength(rowid) + payload
	}
	small, large := cell(127), cell(128)
	space := usable - 8
	perLeaf := space / (large + 2)
	if space/(small+2) != perLeaf {
		fatal("arithmetic", errors.New("rows per leaf depends on the rowid size"))
	}
	interiorSpace := usable - 12
	used, fit := 0, 0
	for leaf := 1; ; leaf++ {
		key := uint64(rowidStep * perLeaf * leaf)
		size := 4 + dbfile.VarintLength(key) + 2
		if used+size > interiorSpace {
			break
		}
		used += size
		fit++
	}
	small1 := 0
	for leaf := 1; leaf <= fit; leaf++ {
		if rowidStep*perLeaf*leaf <= 127 {
			small1++
		}
	}
	deeper := perLeaf*(fit+1) + 1
	for i, p := range []struct {
		name  string
		value int
		how   string
	}{
		{"record_header_bytes", header, fmt.Sprintf("1 size varint + 1 type (NULL: the id is the rowid) + %d type %d (blob of %d bytes)", dbfile.VarintLength(serialBlob), serialBlob, blobBytes)},
		{"payload_bytes", payload, fmt.Sprintf("%d header + %d body", header, blobBytes)},
		{"max_local_payload", maxLocal, fmt.Sprintf("table-leaf limit at usable size %d; the payload is below it: no overflow", usable)},
		{"cell_bytes_rowid_to_127", small + 2, fmt.Sprintf("%d payload-size varint + 1 rowid varint + %d payload + 2 cell pointer", payloadVarint, payload)},
		{"cell_bytes_rowid_from_128", large + 2, "the rowid varint is 2 bytes for rowids 128..16383"},
		{"leaf_space_bytes", space, fmt.Sprintf("%d - 8 (leaf header)", usable)},
		{"rows_per_leaf", perLeaf, fmt.Sprintf("floor(%d / %d): %d x %d = %d fits, %d x %d = %d does not", space, large+2, perLeaf, large+2, perLeaf*(large+2), perLeaf+1, small+2, (perLeaf+1)*(small+2))},
		{"first_split_insert", perLeaf + 1, fmt.Sprintf("inserts 1..%d fill the root leaf; rowid %d does not fit", perLeaf, rowidStep*(perLeaf+1))},
		{"split_every_inserts", perLeaf, fmt.Sprintf("a new leaf fills after %d appends: splits at inserts #%d, #%d, #%d, ...", perLeaf, perLeaf+1, 2*perLeaf+1, 3*perLeaf+1)},
		{"root_space_bytes", interiorSpace, fmt.Sprintf("%d - 12 (interior header)", usable)},
		{"dividers_fit_in_root", fit, fmt.Sprintf("%d dividers of 7 bytes (keys <= 127) + %d of 8 bytes = %d bytes; no room for one more", small1, fit-small1, used)},
		{"third_level_insert", deeper, fmt.Sprintf("the split that brings divider %d (rowid %d): the root overflows", fit+1, rowidStep*deeper)},
	} {
		tables.predictions.Row(i+1, p.name, p.value, p.how)
	}
	return arithmetic{payload: payload, cellSmall: small, cellLarge: large, rowsPerLeaf: perLeaf, dividersFit: fit, firstSplit: perLeaf + 1, deeperInsert: deeper}
}

// partA appends one row per transaction into an empty table, with a page
// manifest after every insert (WAL, a checkpoint before each manifest).
func partA(ctx context.Context, dir string) {
	part, workloadName = "A", nil
	a := predict()
	l := openLab(ctx, filepath.Join(dir, "append.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	root := l.rootOf("t")
	total := a.deeperInsert + 2*a.rowsPerLeaf
	interesting := map[int]bool{a.firstSplit: true, a.firstSplit + a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf + 1: true}
	kept := map[int]step{}
	histogram := map[int]int{}
	var splits []int
	ordinaryOnePage, ordinaryLeafOnly := true, true
	walMatches, integrityOK, rootConstant := true, true, true
	firstDeeper := map[int]step{}
	prevDepth, inserts := 1, 0
	for n := 1; n <= total; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		inserts++
		histogram[len(s.changes)]++
		if s.integrity != "ok" {
			integrityOK = false
		}
		if s.rootPage != root {
			rootConstant = false
		}
		if !sameInts(s.walPages, s.pages()) {
			walMatches = false
		}
		if s.after.PageCount() > s.before.PageCount() {
			splits = append(splits, s.number)
		} else {
			if len(s.changes) != 1 {
				ordinaryOnePage = false
			} else if !leafHoldsRowid(s.after, s.changes[0].Page, s.rowid) {
				ordinaryLeafOnly = false
			}
		}
		if s.depth > prevDepth {
			if _, seen := firstDeeper[s.depth]; !seen {
				firstDeeper[s.depth] = s
			}
		}
		prevDepth = s.depth
		if interesting[n] {
			kept[n] = s
		}
	}
	check(l.verifyManifest(), "the last manifest equals capture.BuildManifest of the same image (unchanged pages reuse the previous sha256 only to save time)")

	predictedSplits := true
	for i, n := range splits {
		if n != a.firstSplit+i*a.rowsPerLeaf {
			predictedSplits = false
		}
	}
	check(len(splits) > 0 && splits[0] == a.firstSplit, fmt.Sprintf("the first split happens at insert #%d (rowid %d), as the arithmetic predicts", a.firstSplit, rowidStep*a.firstSplit))
	check(predictedSplits, fmt.Sprintf("every split happens exactly where predicted: insert #%d m + 1, every %d inserts", a.rowsPerLeaf, a.rowsPerLeaf))
	check(ordinaryOnePage, "every insert that does not split changes exactly one page")
	check(ordinaryLeafOnly, "that one page is the table leaf that now holds the new rowid, never page 1 (page 1 is not written)")
	check(histogram[1] > inserts/2, fmt.Sprintf("most inserts change one page (%d of %d)", histogram[1], inserts))

	recordStep("first split, depth 1 -> 2", firstDeeper[2])
	recordLayout("first split, depth 1 -> 2", firstDeeper[2])
	second := kept[a.firstSplit+a.rowsPerLeaf]
	recordStep("first append split at depth 2", second)
	recordLayout("first append split at depth 2", second)
	recordStep("next append split", kept[a.firstSplit+2*a.rowsPerLeaf])
	ordinary := kept[a.firstSplit+2*a.rowsPerLeaf+1]
	recordStep("an ordinary insert", ordinary)
	recordLayout("an ordinary insert", ordinary)
	deeper, ok := firstDeeper[3]
	check(ok && deeper.number == a.deeperInsert, fmt.Sprintf("the tree gains its third level at insert #%d (rowid %d), as the arithmetic predicts", a.deeperInsert, rowidStep*a.deeperInsert))
	if ok {
		recordStep("depth 2 -> 3", deeper)
		recordLayout("depth 2 -> 3", deeper)
	}
	check(firstDeeper[2].number == a.firstSplit && len(firstDeeper[2].changes) == 4, "the first split is also the first depth change: the root, a new page holding the root's old cells, a new leaf, and page 1")
	check(len(second.changes) == 3, "an append split at depth 2 changes three pages: page 1 (database size), the parent (one new divider) and the new leaf; the full leaf is not rewritten")
	check(rootConstant, fmt.Sprintf("the root stays page %d through both depth changes and always equals sqlite_schema.rootpage", root))
	check(integrityOK, fmt.Sprintf("integrity_check = ok after every one of the %d inserts", inserts))
	check(walMatches, "for every insert the WAL frames of its commit name exactly the pages the manifest shows changed: pages changed = frames shipped")
}

// partB inserts one row into the middle of a full leaf.
func partB(ctx context.Context, dir string) {
	part, workloadName = "B", nil
	l := openLab(ctx, filepath.Join(dir, "middle.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	var last step
	for n := 1; n <= 20; n++ {
		rowid := int64(rowidStep * n)
		last = l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	}
	root := mustPage(l.file, l.rootOf("t"))
	pageStateRow("before the middle insert", last.number, "layout", l.file, root.Number)
	cells, err := root.Cells()
	if err != nil {
		fatal("cells", err)
	}
	for _, c := range cells {
		pageStateRow("before the middle insert", last.number, "layout", l.file, int(c.LeftChild))
	}
	pageStateRow("before the middle insert", last.number, "layout", l.file, int(root.RightChild))
	// Insert rowid 11 into full leaf 4 (keys 10..16), forcing balance_nonroot
	// (btree.c:8204-9027) to redistribute cells across sibling leaves.
	rowid := int64(11)
	s := l.exec(ctx, 21, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	recordStep("middle split", s)
	recordLayout("middle split", s)
	leaves := 0
	for _, c := range s.changes {
		if c.Page == 1 {
			continue
		}
		if p, err := s.after.BTreePage(c.Page); err == nil && p.Type == dbfile.TableLeaf {
			leaves++
		}
	}
	check(leaves >= 2, fmt.Sprintf("a split in the middle rewrites the full leaf and its siblings (%d leaf pages changed), not just one new page", leaves))
	// balance_nonroot calls sqlite3PagerWrite on every reused sibling page before
	// rewriting it (btree.c:8245-9027), so a sibling whose rebuilt cells match its
	// old bytes still enters the WAL as a frame even though its SHA-256 is unchanged.
	extra := subtract(s.walPages, s.pages())
	check(len(subtract(s.pages(), s.walPages)) == 0, "every changed page is in the commit's WAL frames")
	unchanged := true
	for _, n := range extra {
		before, err := s.before.Page(n)
		if err != nil {
			fatal("page before", err)
		}
		after, err := s.after.Page(n)
		if err != nil {
			fatal("page after", err)
		}
		if !bytes.Equal(before, after) {
			unchanged = false
		}
		tables.unchangedFrames.Row(part, s.number, n, equal.SHA256Hex(before)[:16], equal.SHA256Hex(after)[:16])
	}
	check(len(extra) > 0 && unchanged, "here the WAL ships more frames than pages changed: balance_nonroot rewrote a sibling leaf with the same cells it already held, and a written page is a shipped frame even when its bytes are unchanged")
	check(s.integrity == "ok", "integrity_check = ok after the middle split")
	walker := dbfile.TreeWalker{File: s.after, OnPage: func(p *dbfile.BTreePage, parent, depth int) error {
		if p.Type.IsLeaf() {
			pageStateRow("leaves in key order after the middle split", s.number, "layout", s.after, p.Number)
		}
		return nil
	}}
	if err := walker.Walk(l.rootOf("t")); err != nil {
		fatal("walk", err)
	}
}

// partC runs the same kind of insert into a table with two secondary indexes.
func partC(ctx context.Context, dir string) {
	part, workloadName = "C", nil
	l := openLab(ctx, filepath.Join(dir, "indexed.db"), "wal",
		"CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT, score INTEGER, v BLOB)",
		"CREATE INDEX t_email ON t(email)",
		"CREATE INDEX t_score ON t(score)")
	defer l.close()
	histogram := map[int]int{}
	const inserts = 200
	ok := true
	for n := 1; n <= inserts; n++ {
		rowid := int64(n)
		email := fmt.Sprintf("user%03d@example.com", (n*37)%inserts)
		score := (n * 53) % 97
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, email, score, v) VALUES(?, ?, ?, ?)", rowid, email, score, rowBlob(rowid)[:200])
		histogram[len(s.changes)]++
		if !sameInts(s.walPages, s.pages()) || s.integrity != "ok" {
			ok = false
		}
		if n == 50 {
			recordStep("a common insert", s)
		}
	}
	var counts []int
	for k := range histogram {
		counts = append(counts, k)
	}
	sort.Ints(counts)
	most, mostCount := 0, 0
	for _, k := range counts {
		if histogram[k] > mostCount {
			most, mostCount = k, histogram[k]
		}
	}
	check(most == 3, "with two indexes the common insert changes three pages: the table leaf and one leaf in each index")
	check(ok, "WAL frames equal the changed pages and integrity_check = ok for all 200 inserts")
}

// partD repeats the appends of part A in rollback mode (journal_mode=DELETE).
func partD(ctx context.Context, dir string) {
	part, workloadName = "D", nil
	l := openLab(ctx, filepath.Join(dir, "rollback.db"), "delete", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	ok, ordinaryTwo, counterOnly := true, true, true
	counterBytes := []equal.ByteRange{{Start: 24, End: 28}, {Start: 92, End: 96}}
	for n := 1; n <= 12; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		hasPage1 := len(s.changes) > 0 && s.changes[0].Page == 1
		if !hasPage1 || s.integrity != "ok" {
			ok = false
		}
		if s.after.PageCount() == s.before.PageCount() {
			if len(s.changes) != 2 || !leafHoldsRowid(s.after, s.changes[1].Page, rowid) {
				ordinaryTwo = false
			}
			before, err := s.before.Page(1)
			if err != nil {
				fatal("page 1 before", err)
			}
			after, err := s.after.Page(1)
			if err != nil {
				fatal("page 1 after", err)
			}
			r, err := equal.CompareImagesIgnoring(before, after, pageSize, counterBytes)
			if err != nil {
				fatal("compare page 1", err)
			}
			if !r.Equal || len(r.IgnoredDiffering) == 0 {
				counterOnly = false
			}
		}
	}
	check(ok, "in rollback mode every commit also rewrites page 1")
	check(ordinaryTwo, "an insert that adds no page changes exactly two pages: page 1 and the table leaf that holds the new rowid")
	check(counterOnly, "on those inserts page 1 differs only in the change counter (bytes 24-27) and version-valid-for (bytes 92-95)")
}

type workload struct {
	name   string
	table  string
	create string
	keys   []int64
	size   int
}

type workloadResult struct {
	inserts        int
	onePage        int
	addedPages     int
	rebalanced     int
	minChanged     int
	maxChanged     int
	walCovers      bool
	integrityOK    bool
	splitsAtDepth2 []int
}

func shuffledKeys(n int, seed uint64) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i + 1)
	}
	rng := rand.New(rand.NewPCG(seed, 0))
	rng.Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

func ascendingKeys(n int) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i + 1)
	}
	return keys
}

func runWorkload(ctx context.Context, dir string, w workload) workloadResult {
	workloadName = w.name
	l := openLab(ctx, filepath.Join(dir, w.table+".db"), "wal", w.create)
	defer l.close()
	r := workloadResult{minChanged: -1, walCovers: true, integrityOK: true}
	insert := fmt.Sprintf("INSERT INTO %s(k, v) VALUES(?, ?)", w.table)
	prevDepth := 1
	for i, key := range w.keys {
		s := l.exec(ctx, i+1, key, w.table, insert, key, sizedBlob(key, w.size))
		changed := len(s.changes)
		r.inserts++
		switch {
		case s.after.PageCount() > s.before.PageCount():
			r.addedPages++
			if prevDepth == 2 && s.depth == 2 {
				r.splitsAtDepth2 = append(r.splitsAtDepth2, changed)
			}
		case changed == 1:
			r.onePage++
		default:
			r.rebalanced++
		}
		if r.minChanged < 0 || changed < r.minChanged {
			r.minChanged = changed
		}
		r.maxChanged = max(r.maxChanged, changed)
		if len(subtract(s.pages(), s.walPages)) > 0 {
			r.walCovers = false
		}
		if s.integrity != "ok" {
			r.integrityOK = false
		}
		prevDepth = s.depth
	}
	var leaves []int
	for n := 2; n <= l.census.PageCount; n++ {
		role := l.census.Page(n).Role
		if role == dbfile.RoleTableLeaf || role == dbfile.RoleIndexLeaf {
			leaves = append(leaves, mustPage(l.file, n).CellCount)
		}
	}
	sort.Ints(leaves)
	if len(leaves) > 0 {
		workloadSeq++
		tables.workloads.Row(workloadSeq, w.name, w.table, w.size, len(leaves), leaves[0], leaves[len(leaves)-1], prevDepth)
	}
	return r
}

// partE shows where "most inserts change one page" stops holding.
func partE(ctx context.Context, dir string) {
	part, workloadName = "E", nil
	// Exercise four boundary workloads: random keys with few vs many rows per
	// leaf, overflowing payloads (fillInCell allocates overflow pages at
	// btree.c:7212), and WITHOUT ROWID (an index B-tree, excluded from
	// balance_quick by the pPage->intKeyLeaf guard at btree.c:9185-9189).
	workloads := []workload{
		{"random keys, 1000-byte rows (4 per leaf)", "random_large", "CREATE TABLE random_large(k INTEGER PRIMARY KEY, v BLOB)", shuffledKeys(250, 11), 1000},
		{"random keys, 40-byte rows (about 85 per leaf)", "random_small", "CREATE TABLE random_small(k INTEGER PRIMARY KEY, v BLOB)", shuffledKeys(600, 11), 40},
		{"ascending keys, 5000-byte rows (each overflows)", "overflow", "CREATE TABLE overflow(k INTEGER PRIMARY KEY, v BLOB)", ascendingKeys(25), 5000},
		{"ascending keys, 400-byte rows, WITHOUT ROWID", "without_rowid", "CREATE TABLE without_rowid(k INTEGER PRIMARY KEY, v BLOB) WITHOUT ROWID", ascendingKeys(30), 400},
	}
	results := make([]workloadResult, len(workloads))
	for i, w := range workloads {
		results[i] = runWorkload(ctx, dir, w)
	}
	workloadName = nil
	large, small, overflow, withoutRowid := results[0], results[1], results[2], results[3]
	check(large.onePage*2 < large.inserts && large.rebalanced > 0, fmt.Sprintf("random keys with 4 rows per leaf: only %d of %d inserts change one page; %d split and %d more change several pages without adding one (balance_nonroot moves cells between siblings)", large.onePage, large.inserts, large.addedPages, large.rebalanced))
	check(small.onePage*2 > small.inserts, fmt.Sprintf("random keys with small rows: most inserts change one page again (%d of %d): the rule depends on how many rows fit in a leaf, not on the key order alone", small.onePage, small.inserts))
	check(overflow.onePage == 0 && overflow.minChanged >= 3, fmt.Sprintf("a row larger than the leaf's local limit also writes its overflow pages and page 1 (the file grows): no insert changes one page, each changes %d..%d", overflow.minChanged, overflow.maxChanged))
	allWide := len(withoutRowid.splitsAtDepth2) > 0
	for _, n := range withoutRowid.splitsAtDepth2 {
		if n <= 3 {
			allWide = false
		}
	}
	check(allWide, "a WITHOUT ROWID table is an index b-tree, and balance_quick serves table leaves only: its append splits at depth 2 go through balance_nonroot and change more than three pages")
	covers, integrity := true, true
	for _, r := range results {
		covers = covers && r.walCovers
		integrity = integrity && r.integrityOK
	}
	check(covers, "in all four, every changed page is in its commit's WAL frames")
	check(integrity, "integrity_check = ok after every insert of all four")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	set.Finish("PAGES PER INSERT")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/3.1-pages-per-insert/report.sql -->
```sql
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
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
make run EXP=3.1
```

Add `-i` (`make run EXP=3.1 ARGS='-i'`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/3.1-pages-per-insert/`](evidence/3.1-pages-per-insert/), and `make check ARGS='evidence ch03'` compares a fresh run with them byte for byte.

<!-- include: evidence/3.1-pages-per-insert.txt -->
```text

════════════════════════════════════════════════════════════════════════
  3.1  How many pages does one INSERT change?
════════════════════════════════════════════════════════════════════════

Every row below is one single-row transaction. After each commit the
experiment reads the frames in -wal, runs a TRUNCATE checkpoint, and
compares a sha256 manifest of every page with the manifest before.

Settings: page_size=4096, one connection, wal_autocheckpoint=0,
synchronous=OFF (skips fsync only; no file byte depends on it).

What was measured, by part:
┌──────┬──────────────┬─────────┬───────────────┬────────────┐
│ part │ journal_mode │ inserts │ pages_changed │ wal_frames │
├──────┼──────────────┼─────────┼───────────────┼────────────┤
│ A    │ wal          │ 2061    │ 1..5          │ 3094       │
│ B    │ wal          │ 21      │ 1..5          │ 35         │
│ C    │ wal          │ 200     │ 3..6          │ 626        │
│ D    │ delete       │ 12      │ 2..4          │            │
│ E    │ wal          │ 905     │ 1..6          │ 1722       │
└──────┴──────────────┴─────────┴───────────────┴────────────┘

── A. Appends into an empty table ─────────────────────────────────────

Table t(id INTEGER PRIMARY KEY, v BLOB of 1000 bytes), rowids 2, 4, 6, ...

A1. The prediction, from the cell format alone, before any insert:
┌───────────────────────────┬───────┬──────────────────────────────────────────────────────────────────────────────────────────┐
│           name            │ value │                                           how                                            │
├───────────────────────────┼───────┼──────────────────────────────────────────────────────────────────────────────────────────┤
│ record_header_bytes       │ 4     │ 1 size varint + 1 type (NULL: the id is the rowid) + 2 type 2012 (blob of 1000 bytes)    │
│ payload_bytes             │ 1004  │ 4 header + 1000 body                                                                     │
│ max_local_payload         │ 4061  │ table-leaf limit at usable size 4096; the payload is below it: no overflow               │
│ cell_bytes_rowid_to_127   │ 1009  │ 2 payload-size varint + 1 rowid varint + 1004 payload + 2 cell pointer                   │
│ cell_bytes_rowid_from_128 │ 1010  │ the rowid varint is 2 bytes for rowids 128..16383                                        │
│ leaf_space_bytes          │ 4088  │ 4096 - 8 (leaf header)                                                                   │
│ rows_per_leaf             │ 4     │ floor(4088 / 1010): 4 x 1010 = 4040 fits, 5 x 1009 = 5045 does not                       │
│ first_split_insert        │ 5     │ inserts 1..4 fill the root leaf; rowid 10 does not fit                                   │
│ split_every_inserts       │ 4     │ a new leaf fills after 4 appends: splits at inserts #5, #9, #13, ...                     │
│ root_space_bytes          │ 4084  │ 4096 - 12 (interior header)                                                              │
│ dividers_fit_in_root      │ 512   │ 15 dividers of 7 bytes (keys <= 127) + 497 of 8 bytes = 4081 bytes; no room for one more │
│ third_level_insert        │ 2053  │ the split that brings divider 513 (rowid 4106): the root overflows                       │
└───────────────────────────┴───────┴──────────────────────────────────────────────────────────────────────────────────────────┘

A2. Prediction against measurement:
┌─────────────────────────┬───────────┬──────────┐
│          what           │ predicted │ measured │
├─────────────────────────┼───────────┼──────────┤
│ first split at insert # │ 5         │ 5        │
│ inserts between splits  │ 4         │ 4        │
│ third level at insert # │ 2053      │ 2053     │
└─────────────────────────┴───────────┴──────────┘

A3. Pages changed per insert. Most inserts change one page: the leaf.
    Every insert that changes more is a split (it adds pages).
┌───────────────┬─────────┬──────┬────────────────────────────────┬────────┬─────────────────┐
│ pages_changed │ inserts │ pct  │             share              │ splits │ first_at_insert │
├───────────────┼─────────┼──────┼────────────────────────────────┼────────┼─────────────────┤
│ 1             │ 1546    │ 75.0 │ ██████████████████████████████ │ 0      │ 1               │
│ 3             │ 513     │ 24.9 │ █████████▉                     │ 513    │ 9               │
│ 4             │ 1       │ 0.0  │                                │ 1      │ 5               │
│ 5             │ 1       │ 0.0  │                                │ 1      │ 2053            │
└───────────────┴─────────┴──────┴────────────────────────────────┴────────┴─────────────────┘

A4. The inserts worth a close look. Compare changed_pages with wal_pages:
    the frames of a commit name exactly the pages the manifest shows changed.
┌───────────────────────────────┬───────────┬───────┬───────────────┬─────────────────┬─────────────────┬───────┐
│             step              │ insert_no │ rowid │ pages_changed │  changed_pages  │    wal_pages    │ depth │
├───────────────────────────────┼───────────┼───────┼───────────────┼─────────────────┼─────────────────┼───────┤
│ first split, depth 1 -> 2     │ 5         │ 10    │ 4             │ 1 2 3 4         │ 1 2 3 4         │ 2     │
│ first append split at depth 2 │ 9         │ 18    │ 3             │ 1 2 5           │ 1 2 5           │ 2     │
│ next append split             │ 13        │ 26    │ 3             │ 1 2 6           │ 1 2 6           │ 2     │
│ an ordinary insert            │ 14        │ 28    │ 1             │ 6               │ 6               │ 2     │
│ depth 2 -> 3                  │ 2053      │ 4106  │ 5             │ 1 2 516 517 518 │ 1 2 516 517 518 │ 3     │
└───────────────────────────────┴───────────┴───────┴───────────────┴─────────────────┴─────────────────┴───────┘

A5. The same inserts page by page, before and after.

Insert #5, the first split. The root (page 2) stays the root: it turns
into an interior page, its four cells move to new page 3, and new page 4
takes the new row. Page 1 changes because the file grew.
┌──────┬────────┬────────────────┬───────┬─────────┬────────┬──────────┬────────────────────────────────────┐
│ page │ state  │      kind      │ cells │  keys   │ unused │ children │                note                │
├──────┼────────┼────────────────┼───────┼─────────┼────────┼──────────┼────────────────────────────────────┤
│ 1    │ before │ table-leaf     │ 1     │         │ 3924   │          │ sqlite_schema; header says 2 pages │
│ 1    │ after  │ table-leaf     │ 1     │         │ 3924   │          │ sqlite_schema; header says 4 pages │
│ 2    │ before │ table-leaf     │ 4     │ 2,4,6,8 │ 52     │          │                                    │
│ 2    │ after  │ table-interior │ 1     │ 8       │ 4077   │ 3 4      │                                    │
│ 3    │ before │                │       │         │        │          │ not in the file                    │
│ 3    │ after  │ table-leaf     │ 4     │ 2,4,6,8 │ 52     │          │                                    │
│ 4    │ before │                │       │         │        │          │ not in the file                    │
│ 4    │ after  │ table-leaf     │ 1     │ 10      │ 3079   │          │                                    │
└──────┴────────┴────────────────┴───────┴─────────┴────────┴──────────┴────────────────────────────────────┘

Insert #9, an append split at depth 2. Three pages: page 1, one more
divider in the root, one new leaf. The full leaf is not rewritten.
┌──────┬────────┬────────────────┬───────┬──────┬────────┬──────────┬────────────────────────────────────┐
│ page │ state  │      kind      │ cells │ keys │ unused │ children │                note                │
├──────┼────────┼────────────────┼───────┼──────┼────────┼──────────┼────────────────────────────────────┤
│ 1    │ before │ table-leaf     │ 1     │      │ 3924   │          │ sqlite_schema; header says 4 pages │
│ 1    │ after  │ table-leaf     │ 1     │      │ 3924   │          │ sqlite_schema; header says 5 pages │
│ 2    │ before │ table-interior │ 1     │ 8    │ 4077   │ 3 4      │                                    │
│ 2    │ after  │ table-interior │ 2     │ 8,16 │ 4070   │ 3 4 5    │                                    │
│ 5    │ before │                │       │      │        │          │ not in the file                    │
│ 5    │ after  │ table-leaf     │ 1     │ 18   │ 3079   │          │                                    │
└──────┴────────┴────────────────┴───────┴──────┴────────┴──────────┴────────────────────────────────────┘

Insert #14, an ordinary insert. One page: the leaf that holds the row.
┌──────┬────────┬────────────┬───────┬───────┬────────┬──────────┬──────┐
│ page │ state  │    kind    │ cells │ keys  │ unused │ children │ note │
├──────┼────────┼────────────┼───────┼───────┼────────┼──────────┼──────┤
│ 6    │ before │ table-leaf │ 1     │ 26    │ 3079   │          │      │
│ 6    │ after  │ table-leaf │ 2     │ 26,28 │ 2070   │          │      │
└──────┴────────┴────────────┴───────┴───────┴────────┴──────────┴──────┘

Insert #2053. The root is full (unused = 3), so the tree gains a third
level: the root keeps one divider and two new interior pages share the rest.
┌──────┬────────┬────────────────┬───────┬────────────┬────────┬─────────────────────┬──────────────────────────────────────┐
│ page │ state  │      kind      │ cells │    keys    │ unused │      children       │                 note                 │
├──────┼────────┼────────────────┼───────┼────────────┼────────┼─────────────────────┼──────────────────────────────────────┤
│ 1    │ before │ table-leaf     │ 1     │            │ 3924   │                     │ sqlite_schema; header says 515 pages │
│ 1    │ after  │ table-leaf     │ 1     │            │ 3924   │                     │ sqlite_schema; header says 518 pages │
│ 2    │ before │ table-interior │ 512   │ 8..4096    │ 3      │ 3 4 5 ... 515       │                                      │
│ 2    │ after  │ table-interior │ 1     │ 2064       │ 4076   │ 517 518             │                                      │
│ 516  │ before │                │       │            │        │                     │ not in the file                      │
│ 516  │ after  │ table-leaf     │ 1     │ 4106       │ 3078   │                     │                                      │
│ 517  │ before │                │       │            │        │                     │ not in the file                      │
│ 517  │ after  │ table-interior │ 257   │ 8..2056    │ 2043   │ 3 4 5 ... 260       │                                      │
│ 518  │ before │                │       │            │        │                     │ not in the file                      │
│ 518  │ after  │ table-interior │ 255   │ 2072..4104 │ 2044   │ 261 262 263 ... 516 │                                      │
└──────┴────────┴────────────────┴───────┴────────────┴────────┴─────────────────────┴──────────────────────────────────────┘

A6. The whole run. The last column counts commits whose WAL frames
    differ from the changed pages; it must be 0.
┌─────────┬────────────┬───────────┬─────────────┬─────────────┬───────────────┐
│ inserts │ wal_frames │ wal_bytes │ final_pages │ final_depth │ frames_differ │
├─────────┼────────────┼───────────┼─────────────┼─────────────┼───────────────┤
│ 2061    │ 3094       │ 12747280  │ 520         │ 3           │ 0             │
└─────────┴────────────┴───────────┴─────────────┴─────────────┴───────────────┘

── B. A middle insert into a full leaf ─────────────────────────────────

B1. The tree after 20 appends: a root and five full leaves.
┌──────┬────────────────┬───────┬─────────────┬────────┬───────────┐
│ page │      kind      │ cells │    keys     │ unused │ children  │
├──────┼────────────────┼───────┼─────────────┼────────┼───────────┤
│ 2    │ table-interior │ 4     │ 8,16,24,32  │ 4056   │ 3 4 5 6 7 │
│ 3    │ table-leaf     │ 4     │ 2,4,6,8     │ 52     │           │
│ 4    │ table-leaf     │ 4     │ 10,12,14,16 │ 52     │           │
│ 5    │ table-leaf     │ 4     │ 18,20,22,24 │ 52     │           │
│ 6    │ table-leaf     │ 4     │ 26,28,30,32 │ 52     │           │
│ 7    │ table-leaf     │ 4     │ 34,36,38,40 │ 52     │           │
└──────┴────────────────┴───────┴─────────────┴────────┴───────────┘

B2. Insert rowid 11. It belongs in page 4, which is full.
┌───────────┬───────┬───────────────┬───────────────┬─────────────┐
│ insert_no │ rowid │ pages_changed │ changed_pages │  wal_pages  │
├───────────┼───────┼───────────────┼───────────────┼─────────────┤
│ 21        │ 11    │ 5             │ 1 2 4 5 8     │ 1 2 3 4 5 8 │
└───────────┴───────┴───────────────┴───────────────┴─────────────┘

    Not one new page as in part A: the full leaf and its right sibling
    are rewritten, and the root gains a divider in the middle.
┌──────┬────────┬────────────────┬───────┬───────────────┬────────┬─────────────┬────────────────────────────────────┐
│ page │ state  │      kind      │ cells │     keys      │ unused │  children   │                note                │
├──────┼────────┼────────────────┼───────┼───────────────┼────────┼─────────────┼────────────────────────────────────┤
│ 1    │ before │ table-leaf     │ 1     │               │ 3924   │             │ sqlite_schema; header says 7 pages │
│ 1    │ after  │ table-leaf     │ 1     │               │ 3924   │             │ sqlite_schema; header says 8 pages │
│ 2    │ before │ table-interior │ 4     │ 8,16,24,32    │ 4056   │ 3 4 5 6 7   │                                    │
│ 2    │ after  │ table-interior │ 5     │ 8,14,20,24,32 │ 4049   │ 3 4 5 8 6 7 │                                    │
│ 4    │ before │ table-leaf     │ 4     │ 10,12,14,16   │ 52     │             │                                    │
│ 4    │ after  │ table-leaf     │ 4     │ 10,11,12,14   │ 52     │             │                                    │
│ 5    │ before │ table-leaf     │ 4     │ 18,20,22,24   │ 52     │             │                                    │
│ 5    │ after  │ table-leaf     │ 3     │ 16,18,20      │ 1061   │             │                                    │
│ 8    │ before │                │       │               │        │             │ not in the file                    │
│ 8    │ after  │ table-leaf     │ 2     │ 22,24         │ 2070   │             │                                    │
└──────┴────────┴────────────────┴───────┴───────────────┴────────┴─────────────┴────────────────────────────────────┘

B3. wal_pages has one page more than changed_pages. balance_nonroot rewrote
    that sibling with the cells it already held: same bytes, still a frame.
┌──────┬──────────────────┬──────────────────┬────────────┐
│ page │  sha256_before   │   sha256_after   │ same_bytes │
├──────┼──────────────────┼──────────────────┼────────────┤
│ 3    │ c0995adc833eadb1 │ c0995adc833eadb1 │ true       │
└──────┴──────────────────┴──────────────────┴────────────┘

B4. The leaves in key order after the split:
┌──────┬───────┬─────────────┬────────┐
│ page │ cells │    keys     │ unused │
├──────┼───────┼─────────────┼────────┤
│ 3    │ 4     │ 2,4,6,8     │ 52     │
│ 4    │ 4     │ 10,11,12,14 │ 52     │
│ 5    │ 3     │ 16,18,20    │ 1061   │
│ 8    │ 2     │ 22,24       │ 2070   │
│ 6    │ 4     │ 26,28,30,32 │ 52     │
│ 7    │ 4     │ 34,36,38,40 │ 52     │
└──────┴───────┴─────────────┴────────┘

── C. The same insert with two secondary indexes ──────────────────────

The common insert now changes three pages: the table leaf and one leaf
in each index.
┌───────────────┬─────────┬────────────────────────────────┬────────┐
│ pages_changed │ inserts │             share              │ splits │
├───────────────┼─────────┼────────────────────────────────┼────────┤
│ 3             │ 188     │ ██████████████████████████████ │ 0      │
│ 5             │ 10      │ █▌                             │ 10     │
│ 6             │ 2       │ ▎                              │ 2      │
└───────────────┴─────────┴────────────────────────────────┴────────┘

One of the common inserts:
┌───────────┬───────────────────────────────────────────┐
│ insert_no │               changed_roles               │
├───────────┼───────────────────────────────────────────┤
│ 50        │ 3(index-leaf) 4(index-leaf) 7(table-leaf) │
└───────────┴───────────────────────────────────────────┘

── D. The same appends in rollback mode (journal_mode=DELETE) ─────────

There is no WAL. Every commit also rewrites page 1 (the change counter).
┌───────────┬───────┬─────────────────────────────┬───────────┐
│ insert_no │ rowid │        changed_roles        │ wal_pages │
├───────────┼───────┼─────────────────────────────┼───────────┤
│ 1         │ 2     │ 1(table-leaf) 2(table-leaf) │           │
│ 2         │ 4     │ 1(table-leaf) 2(table-leaf) │           │
└───────────┴───────┴─────────────────────────────┴───────────┘
┌───────────────┬───────────────┬─────────┬─────────────────────┐
│ pages_changed │ page1_changed │ inserts │ first_example_pages │
├───────────────┼───────────────┼─────────┼─────────────────────┤
│ 2             │ true          │ 10      │ 1 2                 │
│ 3             │ true          │ 1       │ 1 2 5               │
│ 4             │ true          │ 1       │ 1 2 3 4             │
└───────────────┴───────────────┴─────────┴─────────────────────┘

── E. Where "most inserts change one page" stops holding ──────────────

Four workloads, one row per transaction, WAL. Each insert is one of:
  one_page    it changed one page
  added_page  the file grew (a split, or an overflow page)
  rebalanced  several pages changed, none added (cells moved to siblings)
┌─────────────────────────────────────────────────┬─────────┬──────────┬────────────┬────────────┬───────────────┐
│                    workload                     │ inserts │ one_page │ added_page │ rebalanced │ pages_changed │
├─────────────────────────────────────────────────┼─────────┼──────────┼────────────┼────────────┼───────────────┤
│ random keys, 1000-byte rows (4 per leaf)        │ 250     │ 86       │ 70         │ 94         │ 1..6          │
│ random keys, 40-byte rows (about 85 per leaf)   │ 600     │ 566      │ 7          │ 27         │ 1..6          │
│ ascending keys, 5000-byte rows (each overflows) │ 25      │ 0        │ 25         │ 0          │ 3..5          │
│ ascending keys, 400-byte rows, WITHOUT ROWID    │ 30      │ 23       │ 3          │ 4          │ 1..5          │
└─────────────────────────────────────────────────┴─────────┴──────────┴────────────┴────────────┴───────────────┘

The leaves each workload ends with:
┌─────────────────────────────────────────────────┬───────────┬────────┬───────────────┬─────────────┐
│                    workload                     │ row_bytes │ leaves │ rows_per_leaf │ final_depth │
├─────────────────────────────────────────────────┼───────────┼────────┼───────────────┼─────────────┤
│ random keys, 1000-byte rows (4 per leaf)        │ 1000      │ 71     │ 2..4          │ 2           │
│ random keys, 40-byte rows (about 85 per leaf)   │ 40        │ 8      │ 43..85        │ 2           │
│ ascending keys, 5000-byte rows (each overflows) │ 5000      │ 7      │ 1..4          │ 2           │
│ ascending keys, 400-byte rows, WITHOUT ROWID    │ 400       │ 4      │ 4..9          │ 2           │
└─────────────────────────────────────────────────┴───────────┴────────┴───────────────┴─────────────┘

Splits at depth 2, by pages changed. An append split of a rowid table
costs 3 pages (part A); most splits here cost more.
┌─────────────────────────────────────────────────┬───────────────┬────────┐
│                    workload                     │ pages_changed │ splits │
├─────────────────────────────────────────────────┼───────────────┼────────┤
│ random keys, 1000-byte rows (4 per leaf)        │ 5             │ 40     │
│ random keys, 1000-byte rows (4 per leaf)        │ 6             │ 29     │
│ random keys, 40-byte rows (about 85 per leaf)   │ 5             │ 1      │
│ random keys, 40-byte rows (about 85 per leaf)   │ 6             │ 5      │
│ ascending keys, 5000-byte rows (each overflows) │ 3             │ 15     │
│ ascending keys, 5000-byte rows (each overflows) │ 4             │ 5      │
│ ascending keys, 400-byte rows, WITHOUT ROWID    │ 5             │ 2      │
└─────────────────────────────────────────────────┴───────────────┴────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the last manifest equals capture.BuildManifest of the same image (unchanged pages reuse the previous sha256 only to save time)
A  ✓  the first split happens at insert #5 (rowid 10), as the arithmetic predicts
A  ✓  every split happens exactly where predicted: insert #4 m + 1, every 4 inserts
A  ✓  every insert that does not split changes exactly one page
A  ✓  that one page is the table leaf that now holds the new rowid, never page 1 (page 1 is not written)
A  ✓  most inserts change one page (1546 of 2061)
A  ✓  the tree gains its third level at insert #2053 (rowid 4106), as the arithmetic predicts
A  ✓  the first split is also the first depth change: the root, a new page holding the root's old cells, a new leaf, and page 1
A  ✓  an append split at depth 2 changes three pages: page 1 (database size), the parent (one new divider) and the new leaf; the full leaf is not rewritten
A  ✓  the root stays page 2 through both depth changes and always equals sqlite_schema.rootpage
A  ✓  integrity_check = ok after every one of the 2061 inserts
A  ✓  for every insert the WAL frames of its commit name exactly the pages the manifest shows changed: pages changed = frames shipped
B  ✓  a split in the middle rewrites the full leaf and its siblings (3 leaf pages changed), not just one new page
B  ✓  every changed page is in the commit's WAL frames
B  ✓  here the WAL ships more frames than pages changed: balance_nonroot rewrote a sibling leaf with the same cells it already held, and a written page is a shipped frame even when its bytes are unchanged
B  ✓  integrity_check = ok after the middle split
C  ✓  with two indexes the common insert changes three pages: the table leaf and one leaf in each index
C  ✓  WAL frames equal the changed pages and integrity_check = ok for all 200 inserts
D  ✓  in rollback mode every commit also rewrites page 1
D  ✓  an insert that adds no page changes exactly two pages: page 1 and the table leaf that holds the new rowid
D  ✓  on those inserts page 1 differs only in the change counter (bytes 24-27) and version-valid-for (bytes 92-95)
E  ✓  random keys with 4 rows per leaf: only 86 of 250 inserts change one page; 70 split and 94 more change several pages without adding one (balance_nonroot moves cells between siblings)
E  ✓  random keys with small rows: most inserts change one page again (566 of 600): the rule depends on how many rows fit in a leaf, not on the key order alone
E  ✓  a row larger than the leaf's local limit also writes its overflow pages and page 1 (the file grows): no insert changes one page, each changes 3..5
E  ✓  a WITHOUT ROWID table is an index b-tree, and balance_quick serves table leaves only: its append splits at depth 2 go through balance_nonroot and change more than three pages
E  ✓  in all four, every changed page is in its commit's WAL frames
E  ✓  integrity_check = ok after every insert of all four

27 of 27 claims hold

Experiment verdict: PAGES PER INSERT OK
```

**Reading the evidence:**

- **Overview and arithmetic (opening table, A1).** The opening table counts the inserts each part ran. Before inserting any row, Part A derives the exact split schedule from SQLite's cell format: each `(id INTEGER PRIMARY KEY, v BLOB)` row stores a 4-byte record header (`1` header-size varint + `1` serial type `0` for the rowid-alias `NULL` + `2` bytes for serial type `2012`, a 1,000-byte blob) plus 1,000 payload bytes = `1,004` bytes (`<= 4,061` max local, so zero overflow pages). Adding the 2-byte payload-size varint, 1- or 2-byte rowid varint, and 2-byte cell pointer gives `1,009` or `1,010` bytes per row. A 4,096-byte leaf leaves `4,088` bytes after its 8-byte header, fitting `floor(4088 / 1010) = 4` rows (`4 × 1010 = 4040`; 5 rows would need `5,045`). That predicts splits at inserts `#5`, `#9`, `#13`, …, `#4m + 1`. For the interior root (`4,096 - 12 = 4,084` bytes), 15 dividers of 7 bytes (keys `<= 127`: `8, 16, …, 120`) plus 497 dividers of 8 bytes take `4,081` bytes (`3` bytes left), predicting that divider `513` at insert `#2053` (rowid `4106`) will push the tree to depth 3.
- **Part A histogram and exact split match (A2, A3, A6).** After `2,061` appends, the table has `520` pages at depth `3`, and `verifyManifest` confirms the incrementally hashed manifest matches a full `capture.BuildManifest` byte for byte. Of `2,061` inserts, `1,546` changed 1 page, `513` changed 3 pages, `1` changed 4 pages, and `1` changed 5 pages. Every one of the `515` splits occurred at `#4m + 1`, and every non-split insert modified only the single table leaf holding the new rowid—never page 1.
- **First split = first depth change, append splits, and depth 2→3 (A4, A5, A6).** In a table that starts empty, the first append split and the first depth change are the **same insert** (`#5`, rowid `10`): `balance()` calls `balance_deeper` to copy root page 2's four cells onto new page 3 and turn page 2 into an interior page, then loops once and calls `balance_quick` on page 3 to allocate new leaf 4 for rowid `10` (`4` pages changed: `[1 2 3 4]`). At depth 2, insert `#9` (rowid `18`) takes `balance_quick` alone and changes `3` pages (`[1 2 5]`): full leaf 4 is untouched. At insert `#2053` (rowid `4106`), root page 2 starts with `512` cells and `unused=3` bytes—matching the `4,084 - 4,081 = 3` arithmetic to the byte—and splits via `balance_quick` (leaf `516`) + `balance_deeper` + `balance_nonroot` (new interior pages `517` with `257` cells and `518` with `255` cells, promoting divider `2064` to root page `2`; `257 + 255 + 1 = 513` dividers). None of the 513 existing leaves changed, the root stayed page `2`, and `2,061` single-row inserts shipped `3,094` WAL frames (`12,747,280` bytes at `4,120` bytes/frame)—roughly 6× the final 520-page (`2.1 MB`) table.
- **Part B — middle split and unchanged WAL frame (B1–B4).** After 20 appends (5 full leaves `3..7` under root `2`), inserting rowid `11` into full leaf `4` triggers `balance_nonroot`. Five pages change bytes: `[1 2 4 5 8]` (page 1 size `7 -> 8`, root `2` gets five dividers `8,14,20,24,32`, leaf `4` holds `10,11,12,14`, leaf `5` drops to 3 rows `16,18,20`, and new leaf `8` takes `22,24`). Yet the commit wrote **6** WAL frames `[1 2 3 4 5 8]`, including left sibling page `3` whose SHA-256 was `c0995adc833eadb1` before and `c0995adc833eadb1` after. Because `balance_nonroot` marks every participating sibling writable with `sqlite3PagerWrite`, a page SQLite writes enters the WAL even when its rebuilt bytes are unchanged. Also notice the leaf cell counts after the split (B4: pages `3, 4, 5, 8, 6, 7` hold `4, 4, 3, 2, 4, 4` cells): middle inserts leave partly filled leaves, unlike sequential appends.
- **Part C — two secondary indexes (section C).** Adding two secondary indexes (`t_email` and `t_score`) on `t` shifts the mode from `1` page to `3` pages (`188` of `200` inserts changed 3 pages, `10` changed 5, `2` changed 6). Insert `#50` modified `3(index-leaf) 4(index-leaf) 7(table-leaf)`—one leaf per B-tree.
- **Part D — rollback mode (section D).** Under `journal_mode=DELETE`, every commit includes page `1` (`10` inserts changed 2 pages, `1` changed 3, `1` changed 4). Every non-growing insert modified page `1` and the leaf holding the rowid, and `equal.CompareImagesIgnoring` proves page 1 differed only inside bytes `24-27` (change counter) and `92-95` (version-valid-for).
- **Part E — where "most inserts change one page" stops holding (section E).** Four boundary workloads mark where the one-page rule breaks down:
  - Random keys with 1,000-byte rows (4/leaf): only `86` of `250` inserts changed one page; `70` added a page (5 pages `40` times, 6 pages `29` times at depth 2), and `94` more changed several pages without adding one because `balance_nonroot` shifted cells between partly empty siblings.
  - Random keys with 40-byte rows (~85/leaf): `566` of `600` inserts changed one page again (`7` splits, `27` rebalances). The one-page rule depends on rows per leaf, not key order alone.
  - Ascending 5,000-byte rows (each exceeding `4,061` max local): `0` of `25` inserts changed one page; every insert allocated an overflow page and updated page 1 (`3..5` pages each).
  - Ascending 400-byte rows into a `WITHOUT ROWID` table: depth-2 append splits changed `5` pages (2 of 2 splits), never 3, because `WITHOUT ROWID` uses an index B-tree excluded from `balance_quick` (`btree.c:9185-9189`).

---

## 4. Timeline

The table below lays out the exact page transitions observed in Experiment 3.1 (4,096-byte pages, 1,007/1,008-byte cells—`1,009`/`1,010` bytes including the 2-byte cell pointer—4 rows per leaf, WAL mode):

| Insert | Pages changed (`WAL frames`) | Page 2 (root) | Other pages |
| --- | --- | --- | --- |
| `#1`–`#4` (rowids `2..8`) | `[2]` each (`[2]`) | `table-leaf`, `1 -> 4` cells, `unused` `3079 -> 52` | none (page 1 untouched) |
| `#5` (rowid `10`), depth `1 -> 2` | `[1 2 3 4]` (`[1 2 3 4]`) | `table-leaf -> table-interior`, `1` divider (`8`), `children=[3 4]`, `unused=4077` | `3`: copy of old root (`2,4,6,8`, `unused=52`); `4`: new leaf (`10`, `unused=3079`); `1`: size `2 -> 4` |
| `#6`–`#8` (rowids `12..16`) | `[4]` each (`[4]`) | untouched | `4` fills from `1` to `4` cells (`unused` `3079 -> 52`) |
| `#9` (rowid `18`), append split | `[1 2 5]` (`[1 2 5]`) | `2` dividers (`8,16`), `children=[3 4 5]`, `unused=4070` | `5`: new leaf (`18`, `unused=3079`); `4`: untouched; `1`: size `4 -> 5` |
| `#14` (rowid `28`), ordinary | `[6]` (`[6]`) | untouched | `6`: `1 -> 2` cells (`26,28`), `content-start` `3089 -> 2082`, `unused` `3079 -> 2070` |
| `#2053` (rowid `4106`), depth `2 -> 3` | `[1 2 516 517 518]` (`[1 2 516 517 518]`) | `512` cells (`unused=3`) `->` `1` divider (`2064`), `children=[517 518]` | `516`: new leaf (`4106`); `517`: interior `257` cells (`8..2056`); `518`: interior `255` cells (`2072..4104`); `1`: size `515 -> 518` |

```text
Depth 1 (after #4):      Depth 2 (after #5):          Depth 3 (after #2053):
  [page 2: leaf]           [page 2: interior]           [page 2: interior]
   keys 2,4,6,8             divider: 8                   divider: 2064
                            /        \                   /              \
                    [page 3: leaf]  [page 4: leaf]  [page 517: int]  [page 518: int]
                     2,4,6,8         10              257 dividers     255 dividers
                                                     children 3..260  children 261..516
```

Now contrast the append split at `#9` (`balance_quick`, 3 pages written, 3 WAL frames) with the middle split in Part B (`balance_nonroot`, insert `#21`, rowid `11` into full leaf `4`):

```text
Before insert #21 (rowid 11):
  page 2 (root): dividers [8, 16, 24, 32], children [3, 4, 5, 6, 7]
    leaf 3: [2, 4, 6, 8]      unused=52
    leaf 4: [10, 12, 14, 16]  unused=52   <-- target leaf is full
    leaf 5: [18, 20, 22, 24]  unused=52
    leaf 6: [26, 28, 30, 32]  unused=52
    leaf 7: [34, 36, 38, 40]  unused=52

After insert #21 (rowid 11):
  WAL frames written: [1, 2, 3, 4, 5, 8]  (6 frames)
  Pages with changed bytes: [1, 2, 4, 5, 8] (5 pages; leaf 3 sha256 c0995adc833eadb1 unchanged)
  page 2 (root): dividers [8, 14, 20, 24, 32], children [3, 4, 5, 8, 6, 7]
    leaf 3: [2, 4, 6, 8]      unused=52   (written to WAL, identical bytes)
    leaf 4: [10, 11, 12, 14]  unused=52
    leaf 5: [16, 18, 20]      unused=1061
    leaf 8: [22, 24]          unused=2070 (new page; page 1 size 7 -> 8)
    leaf 6, 7: untouched
```

In rollback mode (`journal_mode=DELETE`, Part D), the same timeline adds page `1` to every row: an ordinary append changes `[1, 2]` (2 pages instead of 1) because each rollback commit increments the file change counter (bytes `24-27`) and version-valid-for (bytes `92-95`) on page `1`.

---

## 5. Lab

### 5.1 Completing `internal/equal` (page changes, E2, E3, and `dbhash`)

Chapter 2 introduced `internal/equal` with level **E1 bytes** (`CompareImages`, `CompareFiles`) and per-page manifests in [`internal/equal/equal.go`](../internal/equal/equal.go) and [`internal/equal/manifest.go`](../internal/equal/manifest.go). Chapter 3 adds the `E2Pages` and `E3Logical` constants and `Ignored` / `IgnoredDiffering` fields to `equal.go`, plus four new files in `internal/equal/`:

- **`changes.go`** compares two per-page manifests and returns sorted `PageChange` entries (`ChangedPages`). A page is reported only when its SHA-256 changed or it was added or removed—not when its census role changed with identical bytes (which happens to freed leaves under `secure_delete=OFF` because `freePage2` calls `sqlite3PagerDontWrite`).

<!-- include: ../internal/equal/changes.go -->
```go
package equal

import (
	"fmt"
	"sort"
	"strings"
)

// PageChange describes a single page whose bytes differed between two manifests
// or that existed in only one manifest, together with its census roles before
// and after the operation.
type PageChange struct {
	Page       int
	RoleBefore string
	RoleAfter  string
}

// Added reports whether the page did not exist in the before manifest.
func (c PageChange) Added() bool { return c.RoleBefore == "" }

// Removed reports whether the page no longer exists in the after manifest.
func (c PageChange) Removed() bool { return c.RoleAfter == "" }

// String formats c for experiment logs, showing role transitions as well as
// newly allocated and removed pages.
func (c PageChange) String() string {
	switch {
	case c.Added():
		return fmt.Sprintf("%d(new %s)", c.Page, c.RoleAfter)
	case c.Removed():
		return fmt.Sprintf("%d(gone, was %s)", c.Page, c.RoleBefore)
	case c.RoleBefore != c.RoleAfter:
		return fmt.Sprintf("%d(%s->%s)", c.Page, c.RoleBefore, c.RoleAfter)
	}
	return fmt.Sprintf("%d(%s)", c.Page, c.RoleAfter)
}

// ChangedPages compares two per-page hash manifests and returns every page
// whose SHA-256 changed or that was added or removed, sorted by page number.
func ChangedPages(before, after Manifest) []PageChange {
	byPage := func(m Manifest) map[int]ManifestEntry {
		out := make(map[int]ManifestEntry, len(m.Entries))
		for _, e := range m.Entries {
			out[e.Page] = e
		}
		return out
	}
	a, b := byPage(before), byPage(after)
	pages := map[int]bool{}
	for p := range a {
		pages[p] = true
	}
	for p := range b {
		pages[p] = true
	}
	var changes []PageChange
	for p := range pages {
		ea, inA := a[p]
		eb, inB := b[p]
		// Only report byte changes (or added/removed pages). When secure_delete
		// is OFF, freePage2 (btree.c:6934-6935) calls sqlite3PagerDontWrite
		// (pager.c:6329-6339), so a freed leaf changes census role from
		// table-leaf to freelist-leaf without being written to disk or the WAL.
		if inA && inB && ea.SHA256 == eb.SHA256 {
			continue
		}
		changes = append(changes, PageChange{Page: p, RoleBefore: ea.Role, RoleAfter: eb.Role})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Page < changes[j].Page })
	return changes
}

// FormatPageChanges formats a slice of PageChange entries inside square brackets.
func FormatPageChanges(changes []PageChange) string {
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = c.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// PageNumbers extracts the sorted 1-based page numbers from changes.
func PageNumbers(changes []PageChange) []int {
	out := make([]int, len(changes))
	for i, c := range changes {
		out[i] = c.Page
	}
	return out
}
```

- **`pages.go`** implements equality level **E2 pages**: page-by-page comparison ignoring a declared list of file byte ranges (`DeclaredHeaderIgnore`: bytes `24-27`, `40-43`, `92-99`). Header bytes `18-19` are deliberately **not** ignored because they distinguish WAL mode (`2, 2`) from rollback mode (`1, 1`). When two images match outside the ignored ranges, `Result.IgnoredDiffering` records every offset inside those ranges where bytes differed.

<!-- include: ../internal/equal/pages.go -->
```go
package equal

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ByteRange is a half-open file byte offset range [Start, End) ignored during
// an E2Pages comparison.
type ByteRange struct {
	Start int
	End   int
}

// Contains reports whether file offset lies within [r.Start, r.End).
func (r ByteRange) Contains(offset int) bool { return offset >= r.Start && offset < r.End }

// String formats r using inclusive byte offsets (for example "24-27" or "43").
func (r ByteRange) String() string {
	if r.End-r.Start == 1 {
		return fmt.Sprintf("%d", r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End-1)
}

// DeclaredHeaderIgnore lists the volatile page-1 header byte ranges ignored by
// E2Pages: bytes 24-27 (file change counter, pager.c:3130-3143), bytes 40-43
// (schema cookie, incremented by sqlite3_backup_step at backup.c:448 and by
// sqlite3RunVacuum at vacuum.c:143-427), and bytes 92-99 (version-valid-for
// and SQLite library version). Header bytes 18-19 (write/read format version)
// are deliberately NOT ignored because they distinguish WAL from rollback mode.
var DeclaredHeaderIgnore = []ByteRange{{24, 28}, {40, 44}, {92, 100}}

// FormatRanges formats a slice of ByteRange values as a comma-separated list.
func FormatRanges(ranges []ByteRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

func formatOffsets(offsets []int) string {
	parts := make([]string, len(offsets))
	for i, o := range offsets {
		parts[i] = fmt.Sprintf("%d", o)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ValidateRanges verifies that every range has Start >= 0 and End > Start.
func ValidateRanges(ranges []ByteRange) error {
	for _, r := range ranges {
		if r.Start < 0 || r.End <= r.Start {
			return fmt.Errorf("invalid ignored byte range %d..%d", r.Start, r.End)
		}
	}
	return nil
}

func ignored(ranges []ByteRange, offset int) bool {
	for _, r := range ranges {
		if r.Contains(offset) {
			return true
		}
	}
	return false
}

// CompareImagesIgnoring compares raw database images a and b page by page at
// level E2Pages, allowing byte differences only at file offsets covered by
// ignore. When the images are equal outside ignore, Result.IgnoredDiffering
// records every file offset inside ignore where a and b differed.
func CompareImagesIgnoring(a, b []byte, pageSize int, ignore []ByteRange) (Result, error) {
	if err := ValidateRanges(ignore); err != nil {
		return Result{}, err
	}
	if pageSize <= 0 {
		return Result{}, fmt.Errorf("page size %d: an E2 comparison needs the page size", pageSize)
	}
	ranges := append([]ByteRange(nil), ignore...)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	r := Result{Level: E2Pages, SHA256A: SHA256Hex(a), SHA256B: SHA256Hex(b), SizeA: int64(len(a)), SizeB: int64(len(b)), PageSize: pageSize, Ignored: ranges}
	pages := (max(len(a), len(b)) + pageSize - 1) / pageSize
	for p := range pages {
		pageA, pageB := pageOf(a, p, pageSize), pageOf(b, p, pageSize)
		if bytes.Equal(pageA, pageB) {
			continue
		}
		if len(pageA) != len(pageB) || !equalOutside(pageA, pageB, p*pageSize, ranges, &r.IgnoredDiffering) {
			if r.FirstDifferingPage == 0 {
				r.FirstDifferingPage = p + 1
			}
			r.DifferingPages++
		}
	}
	r.Equal = r.DifferingPages == 0 && r.SizeA == r.SizeB
	if !r.Equal {
		// Only report IgnoredDiffering when every difference fell inside ignore.
		r.IgnoredDiffering = nil
	}
	return r, nil
}

func equalOutside(pageA, pageB []byte, base int, ranges []ByteRange, differing *[]int) bool {
	var inIgnored []int
	for i := range pageA {
		if pageA[i] == pageB[i] {
			continue
		}
		if !ignored(ranges, base+i) {
			return false
		}
		inIgnored = append(inIgnored, base+i)
	}
	*differing = append(*differing, inIgnored...)
	return true
}

// ComparePages compares raw database images a and b at level E2Pages using
// DeclaredHeaderIgnore.
func ComparePages(a, b []byte, pageSize int) (Result, error) {
	return CompareImagesIgnoring(a, b, pageSize, DeclaredHeaderIgnore)
}

// ComparePageFiles reads database files a and b from disk and compares them
// with ComparePages. Do not call on files open by SQLite in the same process.
func ComparePageFiles(a, b string, pageSize int) (Result, error) {
	imageA, err := os.ReadFile(a)
	if err != nil {
		return Result{}, err
	}
	imageB, err := os.ReadFile(b)
	if err != nil {
		return Result{}, err
	}
	return ComparePages(imageA, imageB, pageSize)
}
```

- **`logical.go`** implements equality level **E3 logical** and the reusable `WalkLogical` visitor (also used by Experiment 3.3's R2 dump so capture and verification read the same content in the same order). Five design details in `logical.go` guard against specific SQLite and Go-driver edge cases:
  1. Every query qualifies `main` explicitly (`main.sqlite_schema`, `pragma_table_info(?, 'main')`, `pragma_index_list(?, 'main')`, `FROM main."t"`, `PRAGMA main.*`) so a `TEMP` table on a caller's connection cannot shadow a `main` table.
  2. Every column in `TableSelect` is prefixed with unary `+` (`+"c"`) so `modernc.org/sqlite` sees an expression with no declared column type and returns raw SQLite storage classes (`nil`, `int64`, `float64`, `string`, `[]byte`) instead of converting `DATETIME` text into `time.Time`.
  3. For rowid tables, `rowidAlias` picks the first unused name among `rowid`, `_rowid_`, `oid`, falling back to an exact `INTEGER PRIMARY KEY` column (with no `pk`-origin autoindex, excluding `INTEGER PRIMARY KEY DESC`, `INT PRIMARY KEY`, and composite keys) before refusing a genuinely hidden rowid.
  4. `LogicalHash` checks `ctx.Err()` first, then runs both `BEGIN` and the closing statement (`ROLLBACK` on error, or `COMMIT` with a `ROLLBACK` fallback on success) under `context.WithoutCancel(ctx)` while the walk itself runs under `ctx`. That way, a canceled context never leaks an open transaction or pins a WAL read mark (the `-shm` slot that prevents checkpoints from reclaiming older WAL frames). `LogicalHashFile` checks `HasLiveSideFile`: it opens `mode=ro` when a non-empty `-wal` or `-journal` exists (so closing does not checkpoint and delete `-wal`), and `mode=rw` when the file is at rest (so an at-rest WAL file does not gain leftover `-wal`/`-shm` sidecar files).
  5. Every value is tagged and length-prefixed (`N`, `I`, `R`, `T`, `B`; unsupported Go types passed directly to `EncodeValue` are tagged `U` + type + value), and all seven persistent pragmas—including `default_cache_size` (header bytes `48-51`, copied by `VACUUM INTO`)—are hashed in `LogicalPragmas` order, paired with `PRAGMA main.integrity_check` and sorted `PRAGMA main.foreign_key_check` (which reports broken foreign-key references).

<!-- include: ../internal/equal/logical.go -->
```go
package equal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// LogicalFormat is written at the start of the E3Logical SHA-256 stream so
// future format revisions cannot collide with v1 digests.
const LogicalFormat = "sqlite-internals logical hash v1"

// LogicalPragmas lists the seven persistent file-level PRAGMA values hashed by
// E3Logical in fixed order. It includes default_cache_size (header bytes 48-51)
// because sqlite3RunVacuum (vacuum.c:143-427) copies it to the canonical file.
var LogicalPragmas = []string{"page_size", "encoding", "auto_vacuum", "user_version", "application_id", "journal_mode", "default_cache_size"}

// SchemaRow represents one logical entry from main.sqlite_schema (omitting the
// physical rootpage column).
type SchemaRow struct {
	Type  string
	Name  string
	Table string
	SQL   sql.NullString
}

// TableInfo describes a user or shadow table in main walked by WalkLogical.
type TableInfo struct {
	Name         string
	Kind         string
	WithoutRowid bool
	RowidColumn  string
	Columns      []string
}

// LogicalVisitor receives callbacks in deterministic order during WalkLogical.
// Any callback field may be nil.
type LogicalVisitor struct {
	Schema    func(SchemaRow) error
	TableDone func(TableInfo, int) error
	Table     func(TableInfo) error
	Row       func(TableInfo, []any) error
	Pragma    func(name, value string) error
}

// QuoteIdentifier returns name as a double-quoted SQLite identifier with
// internal double quotes doubled.
func QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// RowsQuerier is the query subset of *sql.DB, *sql.Conn, and *sql.Tx required
// by WalkLogical and TableSelect.
type RowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WalkLogical visits the main database's schema rows, every table and shadow
// table's rows in explicit key order, and every persistent pragma in
// LogicalPragmas. Callers on a single-connection pool must not run nested
// queries on that same pool inside callbacks.
func WalkLogical(ctx context.Context, q RowsQuerier, v LogicalVisitor) error {
	if err := walkSchema(ctx, q, v); err != nil {
		return err
	}
	tables, err := logicalTables(ctx, q)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := walkTable(ctx, q, t, v); err != nil {
			return err
		}
	}
	for _, name := range LogicalPragmas {
		value, err := pragmaValue(ctx, q, name)
		if err != nil {
			return err
		}
		if v.Pragma != nil {
			if err := v.Pragma(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkSchema(ctx context.Context, q RowsQuerier, v LogicalVisitor) error {
	// Qualify main.sqlite_schema explicitly so a caller's TEMP objects never shadow main.
	// Omit rootpage because page numbers are physical layout, not logical content.
	rows, err := q.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM main.sqlite_schema ORDER BY type, name")
	if err != nil {
		return fmt.Errorf("read sqlite_schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r SchemaRow
		if err := rows.Scan(&r.Type, &r.Name, &r.Table, &r.SQL); err != nil {
			return fmt.Errorf("read sqlite_schema: %w", err)
		}
		if v.Schema != nil {
			if err := v.Schema(r); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func logicalTables(ctx context.Context, q RowsQuerier) ([]TableInfo, error) {
	// Include both ordinary tables and shadow tables (such as FTS5 backing tables),
	// as well as sqlite_sequence and sqlite_stat* when present; exclude sqlite_schema
	// because walkSchema already handles it.
	rows, err := q.QueryContext(ctx, "SELECT name, type, wr FROM pragma_table_list WHERE schema = 'main' AND type IN ('table', 'shadow') AND name NOT IN ('sqlite_schema', 'sqlite_master') ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var tables []TableInfo
	for rows.Next() {
		var t TableInfo
		var withoutRowid int64
		if err := rows.Scan(&t.Name, &t.Kind, &withoutRowid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list tables: %w", err)
		}
		t.WithoutRowid = withoutRowid != 0
		tables = append(tables, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range tables {
		columns, err := tableColumns(ctx, q, tables[i].Name)
		if err != nil {
			return nil, err
		}
		tables[i].Columns = columns
		if !tables[i].WithoutRowid {
			alias, err := rowidAlias(ctx, q, tables[i].Name, columns)
			if err != nil {
				return nil, err
			}
			tables[i].RowidColumn = alias
		}
	}
	return tables, nil
}

func tableColumns(ctx context.Context, q RowsQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') ORDER BY cid", table)
	if err != nil {
		return nil, fmt.Errorf("columns of %q: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q has no columns", table)
	}
	return columns, nil
}

func rowidAlias(ctx context.Context, q RowsQuerier, table string, columns []string) (string, error) {
	used := map[string]bool{}
	for _, c := range columns {
		used[strings.ToLower(c)] = true
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !used[alias] {
			return alias, nil
		}
	}
	// If columns claim all three built-in aliases, the rowid is still accessible
	// when one column is a true INTEGER PRIMARY KEY alias.
	ipk, err := integerPrimaryKeyColumn(ctx, q, table)
	if err != nil {
		return "", err
	}
	if ipk != "" {
		return ipk, nil
	}
	return "", fmt.Errorf("table %q: columns named rowid, _rowid_ and oid hide the rowid", table)
}

func integerPrimaryKeyColumn(ctx context.Context, q RowsQuerier, table string) (string, error) {
	keys, err := primaryKeyColumns(ctx, q, table)
	if err != nil || len(keys) != 1 {
		return "", err
	}
	var declared string
	if err := q.QueryRowContext(ctx, "SELECT type FROM pragma_table_info(?, 'main') WHERE pk = 1", table).Scan(&declared); err != nil {
		return "", fmt.Errorf("primary key of %q: %w", table, err)
	}
	// Only an exact INTEGER declaration (not INT) can alias the rowid.
	if !strings.EqualFold(declared, "INTEGER") {
		return "", nil
	}
	// INTEGER PRIMARY KEY DESC creates a separate autoindex (origin = 'pk') and
	// does NOT alias the rowid; reject tables that have a pk-origin index.
	var pkIndexes int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list(?, 'main') WHERE origin = 'pk'", table).Scan(&pkIndexes); err != nil {
		return "", fmt.Errorf("indexes of %q: %w", table, err)
	}
	if pkIndexes > 0 {
		return "", nil
	}
	return keys[0], nil
}

func primaryKeyColumns(ctx context.Context, q RowsQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') WHERE pk > 0 ORDER BY pk", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// TableSelect builds the deterministic SELECT statement used to read table t
// from main in explicit key order (by rowid for rowid tables, or by primary key
// columns for WITHOUT ROWID tables).
func TableSelect(ctx context.Context, q RowsQuerier, t TableInfo) (string, error) {
	var selected, order []string
	if t.WithoutRowid {
		keys, err := primaryKeyColumns(ctx, q, t.Name)
		if err != nil {
			return "", err
		}
		if len(keys) == 0 {
			return "", fmt.Errorf("WITHOUT ROWID table %q has no primary key columns", t.Name)
		}
		for _, k := range keys {
			order = append(order, QuoteIdentifier(k))
		}
	} else {
		selected = append(selected, QuoteIdentifier(t.RowidColumn))
		order = append(order, QuoteIdentifier(t.RowidColumn))
	}
	// Prefix every column with unary '+' to strip its declared column type:
	// modernc.org/sqlite otherwise converts TEXT in DATE/DATETIME/TIMESTAMP
	// columns to time.Time, losing the underlying SQLite storage class.
	for _, c := range t.Columns {
		selected = append(selected, "+"+QuoteIdentifier(c))
	}
	return fmt.Sprintf("SELECT %s FROM main.%s ORDER BY %s", strings.Join(selected, ", "), QuoteIdentifier(t.Name), strings.Join(order, ", ")), nil
}

func walkTable(ctx context.Context, q RowsQuerier, t TableInfo, v LogicalVisitor) error {
	query, err := TableSelect(ctx, q, t)
	if err != nil {
		return err
	}
	if v.Table != nil {
		if err := v.Table(t); err != nil {
			return err
		}
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("read table %q: %w", t.Name, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		width := len(t.Columns)
		if !t.WithoutRowid {
			width++
		}
		values := make([]any, width)
		pointers := make([]any, width)
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("read table %q: %w", t.Name, err)
		}
		for i, value := range values {
			if _, err := StorageClassOf(value); err != nil {
				return fmt.Errorf("table %q row %d column %d: %w", t.Name, count+1, i, err)
			}
		}
		count++
		if v.Row != nil {
			if err := v.Row(t, values); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read table %q: %w", t.Name, err)
	}
	if v.TableDone != nil {
		return v.TableDone(t, count)
	}
	return nil
}

// StorageClassOf maps a scanned Go value to its SQLite storage class name
// ("null", "integer", "real", "text", or "blob") and rejects any other Go type.
func StorageClassOf(value any) (string, error) {
	switch value.(type) {
	case nil:
		return "null", nil
	case int64:
		return "integer", nil
	case float64:
		return "real", nil
	case string:
		return "text", nil
	case []byte:
		return "blob", nil
	}
	return "", fmt.Errorf("value of Go type %T has no SQLite storage class", value)
}

func pragmaValue(ctx context.Context, q RowsQuerier, name string) (string, error) {
	var value any
	if err := q.QueryRowContext(ctx, "PRAGMA main."+name).Scan(&value); err != nil {
		return "", fmt.Errorf("PRAGMA %s: %w", name, err)
	}
	text := fmt.Sprint(value)
	if b, ok := value.([]byte); ok {
		text = string(b)
	}
	switch name {
	case "journal_mode":
		// Only the WAL vs rollback distinction persists in header bytes 18-19;
		// rollback submodes (delete, truncate, persist) are per-connection.
		if strings.EqualFold(text, "wal") {
			return "wal", nil
		}
		return "rollback", nil
	case "auto_vacuum":
		switch text {
		case "0":
			return "none", nil
		case "1":
			return "full", nil
		case "2":
			return "incremental", nil
		}
	}
	return text, nil
}

// LogicalTableDigest holds the row count and per-table SHA-256 computed during
// LogicalHash so CompareLogical can name the exact table that differed.
type LogicalTableDigest struct {
	Name   string
	Rows   int
	SHA256 string
}

// LogicalPragma holds one persistent file-level pragma name and normalized value.
type LogicalPragma struct {
	Name  string
	Value string
}

// Logical holds the full E3 logical fingerprint of a database, including its
// overall SHA-256, schema and per-table digests, persistent pragmas, and the
// results of PRAGMA main.integrity_check and PRAGMA main.foreign_key_check.
type Logical struct {
	SHA256          string
	SchemaSHA256    string
	SchemaRows      int
	Tables          []LogicalTableDigest
	Pragmas         []LogicalPragma
	Integrity       string
	ForeignKeyCheck []string
}

// TableByName looks up a table's digest in l by name.
func (l Logical) TableByName(name string) (LogicalTableDigest, bool) {
	for _, t := range l.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return LogicalTableDigest{}, false
}

// Pragma looks up a persistent pragma value recorded in l by name.
func (l Logical) Pragma(name string) (string, bool) {
	for _, p := range l.Pragmas {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

// Summary formats l as a single human-readable line for experiment output.
func (l Logical) Summary() string {
	var tables []string
	for _, t := range l.Tables {
		tables = append(tables, fmt.Sprintf("%s=%d", t.Name, t.Rows))
	}
	var pragmas []string
	for _, p := range l.Pragmas {
		pragmas = append(pragmas, p.Name+"="+p.Value)
	}
	return fmt.Sprintf("logical sha256 %s; schema rows %d; table rows %s; %s; integrity_check %s; foreign_key_check %d row(s)", l.SHA256, l.SchemaRows, strings.Join(tables, " "), strings.Join(pragmas, " "), l.Integrity, len(l.ForeignKeyCheck))
}

type streamEncoder struct {
	w io.Writer
}

func (e streamEncoder) tag(t byte) {
	e.w.Write([]byte{t})
}

func (e streamEncoder) length(n int) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	e.w.Write(b[:])
}

func (e streamEncoder) bytes(tag byte, b []byte) {
	e.tag(tag)
	e.length(len(b))
	e.w.Write(b)
}

func (e streamEncoder) text(tag byte, s string) { e.bytes(tag, []byte(s)) }

func (e streamEncoder) value(value any) error {
	switch x := value.(type) {
	case nil:
		e.tag('N')
	case int64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(x))
		e.tag('I')
		e.w.Write(b[:])
	case float64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(x))
		e.tag('R')
		e.w.Write(b[:])
	case string:
		e.text('T', x)
	case []byte:
		e.bytes('B', x)
	default:
		return fmt.Errorf("value of Go type %T has no SQLite storage class", value)
	}
	return nil
}

// EncodeValue encodes a single value using the tagged, length-prefixed framing
// of E3Logical: 'N' (NULL), 'I' + 8-byte big-endian int64, 'R' + 8-byte
// big-endian IEEE-754 bits, 'T'/'B' + 8-byte length + bytes. Length prefixes
// prevent boundary collisions such as ('a3','b') vs ('a','3b') that dbhash
// misses (tool/dbhash.c:281-360). Values with no SQLite storage class are
// tagged with 'U' + Go type + 'v' + Go representation so direct callers never
// see empty or colliding encodings.
func EncodeValue(value any) []byte {
	var b bytes.Buffer
	e := streamEncoder{&b}
	if err := e.value(value); err != nil {
		b.Reset()
		e.text('U', fmt.Sprintf("%T", value))
		e.text('v', fmt.Sprintf("%#v", value))
	}
	return b.Bytes()
}

// LogicalHash computes the E3Logical fingerprint of conn's main database
// inside a single read transaction so concurrent writers cannot split the
// snapshot.
func LogicalHash(ctx context.Context, conn *sql.Conn) (Logical, error) {
	// Check cancellation up front, then run deferred BEGIN without cancellation.
	// modernc.org/sqlite can return ctx.Err() after a statement already succeeded
	// (stmt.go checks its watcher flag before disarming it), which would leave an
	// open transaction behind an error. A deferred BEGIN takes no locks.
	if err := ctx.Err(); err != nil {
		return Logical{}, fmt.Errorf("begin read transaction: %w", err)
	}
	uncancelable := context.WithoutCancel(ctx)
	if _, err := conn.ExecContext(uncancelable, "BEGIN"); err != nil {
		return Logical{}, fmt.Errorf("begin read transaction: %w", err)
	}
	result, err := logicalHashInTx(ctx, conn)
	// End the transaction without cancellation so a canceled caller context never
	// leaks an open read transaction or pins a WAL read mark on conn.
	if err != nil {
		if _, rollbackErr := conn.ExecContext(uncancelable, "ROLLBACK"); rollbackErr != nil {
			return Logical{}, errors.Join(err, fmt.Errorf("end read transaction: %w", rollbackErr))
		}
		return Logical{}, err
	}
	// If COMMIT fails, roll back before returning so conn never stays inside a
	// transaction.
	if _, commitErr := conn.ExecContext(uncancelable, "COMMIT"); commitErr != nil {
		if _, rollbackErr := conn.ExecContext(uncancelable, "ROLLBACK"); rollbackErr != nil {
			return Logical{}, fmt.Errorf("end read transaction: %w", errors.Join(commitErr, rollbackErr))
		}
		return Logical{}, fmt.Errorf("end read transaction: %w", commitErr)
	}
	return result, nil
}

func logicalHashInTx(ctx context.Context, conn *sql.Conn) (Logical, error) {
	var result Logical
	whole := sha256.New()
	all := streamEncoder{whole}
	all.text('V', LogicalFormat)
	schema := sha256.New()
	var table hash.Hash
	var tableEncoder streamEncoder
	visitor := LogicalVisitor{
		Schema: func(r SchemaRow) error {
			for _, e := range []streamEncoder{all, {schema}} {
				e.tag('S')
				e.text('t', r.Type)
				e.text('n', r.Name)
				e.text('b', r.Table)
				if r.SQL.Valid {
					e.text('q', r.SQL.String)
				} else {
					e.tag('N')
				}
			}
			result.SchemaRows++
			return nil
		},
		Table: func(t TableInfo) error {
			table = sha256.New()
			tableEncoder = streamEncoder{table}
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.text('T', t.Name)
				if t.WithoutRowid {
					e.tag('w')
				} else {
					e.tag('r')
				}
				e.length(len(t.Columns))
				for _, c := range t.Columns {
					e.text('c', c)
				}
			}
			return nil
		},
		Row: func(t TableInfo, values []any) error {
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.tag('R')
				e.length(len(values))
				for _, value := range values {
					if err := e.value(value); err != nil {
						return err
					}
				}
			}
			return nil
		},
		TableDone: func(t TableInfo, rows int) error {
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.tag('E')
				e.length(rows)
			}
			result.Tables = append(result.Tables, LogicalTableDigest{Name: t.Name, Rows: rows, SHA256: hex.EncodeToString(table.Sum(nil))})
			return nil
		},
		Pragma: func(name, value string) error {
			all.tag('P')
			all.text('n', name)
			all.text('v', value)
			result.Pragmas = append(result.Pragmas, LogicalPragma{Name: name, Value: value})
			return nil
		},
	}
	if err := WalkLogical(ctx, conn, visitor); err != nil {
		return Logical{}, err
	}
	result.SHA256 = hex.EncodeToString(whole.Sum(nil))
	result.SchemaSHA256 = hex.EncodeToString(schema.Sum(nil))
	integrity, err := integrityCheck(ctx, conn)
	if err != nil {
		return Logical{}, err
	}
	result.Integrity = integrity
	fk, err := foreignKeyCheck(ctx, conn)
	if err != nil {
		return Logical{}, err
	}
	result.ForeignKeyCheck = fk
	return result, nil
}

func integrityCheck(ctx context.Context, q RowsQuerier) (string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA main.integrity_check")
	if err != nil {
		return "", fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " / "), rows.Err()
}

func foreignKeyCheck(ctx context.Context, q RowsQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA main.foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		row := "NULL"
		if rowid.Valid {
			row = fmt.Sprint(rowid.Int64)
		}
		lines = append(lines, fmt.Sprintf("%s rowid=%s parent=%s fkid=%d", table, row, parent, fkid))
	}
	sort.Strings(lines)
	return lines, rows.Err()
}

// ErrNoDatabase is returned when OpenExisting or LogicalHashFile is given a
// path that does not exist.
var ErrNoDatabase = errors.New("database file does not exist")

// OpenExisting opens an existing database file in read-write mode without
// creating missing files or running any PRAGMA changes.
func OpenExisting(ctx context.Context, path string) (*sql.DB, error) {
	return openExisting(ctx, path, "rw")
}

func openForReading(ctx context.Context, path string) (*sql.DB, error) {
	busy, err := HasLiveSideFile(path)
	if err != nil {
		return nil, err
	}
	// If a live -wal or -journal exists, open read-only so closing does not
	// checkpoint the WAL into the main file and delete -wal. When the file is
	// at rest, open read-write because a read-only open of an at-rest WAL file
	// leaves a -wal/-shm sidecar behind.
	if busy {
		return openExisting(ctx, path, "ro")
	}
	return openExisting(ctx, path, "rw")
}

// HasLiveSideFile reports whether path has a non-empty -wal or -journal file
// beside it.
func HasLiveSideFile(path string) (bool, error) {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Stat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

func openExisting(ctx context.Context, path, mode string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, ErrNoDatabase)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {mode}})
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// LogicalHashFile opens path on a dedicated connection without altering its
// settings, computes LogicalHash, and closes the connection.
func LogicalHashFile(ctx context.Context, path string) (Logical, error) {
	db, err := openForReading(ctx, path)
	if err != nil {
		return Logical{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return Logical{}, err
	}
	result, err := LogicalHash(ctx, conn)
	if closeErr := conn.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if closeErr := db.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return result, err
}

// LogicalResult summarizes an E3Logical comparison between two Logical digests.
type LogicalResult struct {
	Level       Level
	Equal       bool
	SHA256A     string
	SHA256B     string
	Differences []string
}

// String formats r as a single-line E3 verdict naming any differences found.
func (r LogicalResult) String() string {
	if r.Equal {
		return fmt.Sprintf("%s: equal (logical sha256 %s; integrity_check ok; foreign_key_check unchanged)", r.Level, r.SHA256A)
	}
	return fmt.Sprintf("%s: differ: %s", r.Level, strings.Join(r.Differences, "; "))
}

// CompareLogical compares two Logical fingerprints at level E3Logical. Two
// databases are equal only when their logical hash streams match, both pass
// integrity_check ("ok"), and their sorted foreign_key_check lists are equal.
func CompareLogical(a, b Logical) LogicalResult {
	r := LogicalResult{Level: E3Logical, SHA256A: a.SHA256, SHA256B: b.SHA256}
	if a.SchemaSHA256 != b.SchemaSHA256 {
		r.Differences = append(r.Differences, fmt.Sprintf("schema differs (%d vs %d rows)", a.SchemaRows, b.SchemaRows))
	}
	names := map[string]bool{}
	for _, t := range a.Tables {
		names[t.Name] = true
	}
	for _, t := range b.Tables {
		names[t.Name] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		ta, okA := a.TableByName(name)
		tb, okB := b.TableByName(name)
		switch {
		case !okA:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s only in the second", name))
		case !okB:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s only in the first", name))
		case ta.SHA256 != tb.SHA256:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s differs (%d vs %d rows)", name, ta.Rows, tb.Rows))
		}
	}
	for _, p := range a.Pragmas {
		other, ok := b.Pragma(p.Name)
		if !ok || other != p.Value {
			r.Differences = append(r.Differences, fmt.Sprintf("pragma %s %s vs %s", p.Name, p.Value, other))
		}
	}
	for _, p := range b.Pragmas {
		if _, ok := a.Pragma(p.Name); !ok {
			r.Differences = append(r.Differences, fmt.Sprintf("pragma %s  vs %s", p.Name, p.Value))
		}
	}
	if a.SHA256 != b.SHA256 && len(r.Differences) == 0 {
		r.Differences = append(r.Differences, "logical streams differ")
	}
	if a.Integrity != "ok" {
		r.Differences = append(r.Differences, "integrity_check of the first: "+a.Integrity)
	}
	if b.Integrity != "ok" {
		r.Differences = append(r.Differences, "integrity_check of the second: "+b.Integrity)
	}
	if !slices.Equal(a.ForeignKeyCheck, b.ForeignKeyCheck) {
		r.Differences = append(r.Differences, fmt.Sprintf("foreign_key_check changed (%d vs %d rows)", len(a.ForeignKeyCheck), len(b.ForeignKeyCheck)))
	}
	r.Equal = len(r.Differences) == 0
	return r
}

// CompareLogicalFiles hashes database files a and b with LogicalHashFile and
// compares the resulting Logical digests.
func CompareLogicalFiles(ctx context.Context, a, b string) (LogicalResult, Logical, Logical, error) {
	la, err := LogicalHashFile(ctx, a)
	if err != nil {
		return LogicalResult{}, Logical{}, Logical{}, err
	}
	lb, err := LogicalHashFile(ctx, b)
	if err != nil {
		return LogicalResult{}, Logical{}, Logical{}, err
	}
	return CompareLogical(la, lb), la, lb, nil
}
```

- **`dbhash.go`** locates and invokes the pinned `dbhash` binary (SQLite's C content-hashing tool) as an independent cross-check, passing a `file:…?mode=ro` URI whenever `HasLiveSideFile` is true and turning any stderr output into a Go error (because `dbhash` exits `0` after printing open errors to stderr at `tool/dbhash.c:440`).

<!-- include: ../internal/equal/dbhash.go -->
```go
package equal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// ErrNoDBHash is returned by LocateDBHash when the pinned dbhash binary is not
// available on the system.
var ErrNoDBHash = errors.New("dbhash not found")

// LocateDBHash finds the dbhash binary built from the pinned SQLite tree.
func LocateDBHash() (string, error) {
	path, err := sqlitex.LocateTool("dbhash")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoDBHash, err)
	}
	return path, nil
}

// RunDBHash executes the pinned dbhash tool against dbPath and returns its
// 40-character hexadecimal SHA-1 digest.
func RunDBHash(ctx context.Context, tool, dbPath string) (string, error) {
	target := dbPath
	busy, err := HasLiveSideFile(dbPath)
	if err != nil {
		return "", err
	}
	// dbhash opens files read-write by default (tool/dbhash.c:433-438) with
	// SQLITE_OPEN_URI enabled. When a live -wal or -journal exists, pass a
	// mode=ro URI so closing dbhash does not checkpoint and delete the WAL.
	if busy {
		target, err = sqlitex.FileURI(dbPath, url.Values{"mode": {"ro"}})
		if err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, tool, target)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dbhash %s: %w: %s", dbPath, err, strings.TrimSpace(stderr.String()))
	}
	// dbhash exits 0 even when a database cannot be opened, logging the error
	// to stderr and continuing (tool/dbhash.c:440), so treat any stderr as failure.
	if stderr.Len() > 0 {
		return "", fmt.Errorf("dbhash %s: %s", dbPath, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Fields(stdout.String())
	if len(fields) < 2 || len(fields[0]) != 40 {
		return "", fmt.Errorf("dbhash %s: unexpected output %q", dbPath, stdout.String())
	}
	return fields[0], nil
}
```

### 5.2 Experiment 3.2 — Delete and reuse (`3.2-delete-and-reuse`)

- **Claim:** Under `auto_vacuum=NONE` (SQLite's default), deleting rows moves emptied pages to the freelist and never shrinks the file; later inserts reuse freelist pages before growing the file on a per-page-allocation basis, and `VACUUM` returns the space. Freed pages—as well as freeblocks on live leaves and the unused tail of a freelist trunk—keep old row bytes unless `secure_delete=ON`. Setting `secure_delete=ON` zeroes the database file at rest at the cost of extra WAL frames, while leaving prior `INSERT` frames readable in `-wal` until the WAL is reset (for example by `wal_checkpoint(TRUNCATE)`). By contrast, `auto_vacuum=FULL` shrinks on the `DELETE`'s own commit, and `auto_vacuum=INCREMENTAL` shrinks on `PRAGMA incremental_vacuum`.
- **Evidence form:** `EXACT` (every stored CSV table must match a fresh run byte for byte).

<!-- include: experiments/3.2-delete-and-reuse/main.go -->
```go
// Experiment 3.2 tracks page freeing, freelist reuse, file growth, VACUUM,
// auto_vacuum modes, and deleted-byte residue across secure_delete=OFF and
// secure_delete=ON in both the database file at rest and the -wal file.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 3.2
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize    = 4096
	blobBytes   = 1000
	initialRows = 400
	deleteFirst = 99
	deleteLast  = 302
	reuseRows   = 100
	growBatches = 30
	rowsPerTx   = 4
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 3.2" runs both.
var (
	set    *results.Set
	tables struct {
		settings, steps, deleteStep, reuseStep, growBatches, residue,
		largeTransaction, autoVacuum, walResidue *results.Table
	}
)

func openTables() {
	tables.settings = set.Table("settings", "name", "value")
	tables.steps = set.Table("steps", "secure_delete", "step", "what", "file_bytes", "pages", "freelist_pages", "trunk_pages", "freelist_leaves", "table_leaf_pages")
	tables.deleteStep = set.Table("delete_step", "secure_delete", "rows_deleted", "held_on_leaf_pages", "freelist_pages", "former_leaves_freed", "wal_frames")
	tables.reuseStep = set.Table("reuse_step", "secure_delete", "new_rows", "new_leaf_pages", "new_leaf_count", "taken_from_freelist")
	tables.growBatches = set.Table("grow_batches", "secure_delete", "batch", "freelist_before", "freelist_after", "pages_before", "pages_after")
	tables.residue = set.Table("residue", "secure_delete", "step", "markers_searched", "rows_sought", "rows_found", "pages_with_markers", "roles")
	tables.largeTransaction = set.Table("large_transaction", "rows_inserted", "deleted_rows", "freelist_before", "page_count_before", "freelist_after", "page_count_after")
	tables.autoVacuum = set.Table("auto_vacuum", "mode", "pages_created", "pages_after_delete", "freelist_after_delete", "pages_after_incremental_vacuum")
	tables.walResidue = set.Table("wal_residue", "seq", "location", "rows_readable", "rows_total")
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func marker(prefix string, id int64) []byte {
	return []byte(fmt.Sprintf("%s-%06d", prefix, id))
}

func rowBlob(prefix string, id int64) []byte {
	unit := append(marker(prefix, id), ' ')
	return bytes.Repeat(unit, blobBytes/len(unit)+1)[:blobBytes]
}

type fileState struct {
	size      int64
	pages     int
	freelist  int
	trunks    []int
	leaves    []int
	census    *dbfile.Census
	image     []byte
	tableLeaf map[int][]int64
}

// readState inspects a closed, at-rest database file directly from disk so no
// open SQLite connection holds POSIX locks in this process.
func readState(path string) fileState {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	s := fileState{size: int64(len(image)), pages: f.PageCount(), freelist: census.FreelistLength(), trunks: census.FreelistTrunks, leaves: census.FreelistLeaves, census: census, image: image, tableLeaf: map[int][]int64{}}
	schema, err := f.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name != "t" {
			continue
		}
		err := f.WalkTable(int(e.RootPage), func(r dbfile.Row) error {
			s.tableLeaf[r.Page] = append(s.tableLeaf[r.Page], r.Rowid)
			return nil
		})
		if err != nil {
			fatal("walk", err)
		}
	}
	return s
}

func (s fileState) freePages() map[int]bool {
	out := map[int]bool{}
	for _, p := range s.trunks {
		out[p] = true
	}
	for _, p := range s.leaves {
		out[p] = true
	}
	return out
}

func compactPages(xs []int) string {
	if len(xs) == 0 {
		return "none"
	}
	sorted := append([]int(nil), xs...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if start == prev {
			parts = append(parts, fmt.Sprint(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, x := range sorted[1:] {
		if x == prev+1 {
			prev = x
			continue
		}
		flush()
		start, prev = x, x
	}
	flush()
	return strings.Join(parts, ",")
}

type finding struct {
	rowsFound  int
	rowsSought int
	byRole     map[string]int
	pages      map[int]bool
}

type markerRange struct {
	prefix      string
	first, last int64
}

var firstDeletion = []markerRange{{"SECRET", deleteFirst, deleteLast}}

var secondDeletion = []markerRange{{"NEW", 1001, 1000 + reuseRows}, {"GROW", 2001, 2000 + growBatches*rowsPerTx}}

func searchDeleted(s fileState) finding {
	return searchMarkers(s, firstDeletion)
}

func searchMarkers(s fileState, ranges []markerRange) finding {
	f := finding{byRole: map[string]int{}, pages: map[int]bool{}}
	for _, r := range ranges {
		for id := r.first; id <= r.last; id++ {
			f.rowsSought++
			if f.searchOne(s, marker(r.prefix, id)) {
				f.rowsFound++
			}
		}
	}
	return f
}

func (f finding) searchOne(s fileState, needle []byte) bool {
	found := false
	for offset := 0; ; {
		i := bytes.Index(s.image[offset:], needle)
		if i < 0 {
			return found
		}
		at := offset + i
		page := at/pageSize + 1
		if !f.pages[page] {
			f.pages[page] = true
			f.byRole[s.census.Page(page).Role.String()]++
		}
		found = true
		offset = at + len(needle)
	}
}

// roles lists the page roles that hold markers, such as "freelist-leaf=49 table-leaf=2".
func (f finding) roles() any {
	var roles []string
	for role, n := range f.byRole {
		roles = append(roles, fmt.Sprintf("%s=%d", role, n))
	}
	if len(roles) == 0 {
		return nil
	}
	sort.Strings(roles)
	return strings.Join(roles, " ")
}

func spaced(xs []int) any {
	if len(xs) == 0 {
		return nil
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, " ")
}

func open(ctx context.Context, path string, secureDelete bool) *sqlitex.DB {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = secureDelete
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	return db
}

func session(ctx context.Context, path string, secureDelete bool, work func(db *sqlitex.DB)) {
	db := open(ctx, path, secureDelete)
	work(db)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	if integrity != "ok" {
		fatal("integrity_check", fmt.Errorf("%s", integrity))
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func execAll(ctx context.Context, db *sqlitex.DB, statement string, rows [][]any) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, args := range rows {
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
}

func pragmaInt(ctx context.Context, db *sqlitex.DB, name string) int {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		fatal(name, err)
	}
	return v
}

func insertRows(prefix string, first, count int64) [][]any {
	var rows [][]any
	for id := first; id < first+count; id++ {
		rows = append(rows, []any{id, rowBlob(prefix, id)})
	}
	return rows
}

type outcome struct {
	afterDelete       finding
	afterReuse        finding
	afterSecondDelete finding
	afterVacuum       finding
	sizes             []int64
	deleteFrames      int
}

// walFrames computes the frame count from the -wal file length (32-byte header
// plus 24-byte frame header and 4096-byte page image per frame) before close
// checkpoints and deletes the WAL.
func walFrames(path string) int {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		fatal("stat -wal", err)
	}
	if info.Size() < 32 || (info.Size()-32)%(24+pageSize) != 0 {
		fatal("-wal size", fmt.Errorf("%d bytes", info.Size()))
	}
	return int((info.Size() - 32) / (24 + pageSize))
}

func recordStep(label string, step int, what string, s fileState) {
	tables.steps.Row(label, step, what, s.size, s.pages, s.freelist, spaced(s.trunks), len(s.leaves), len(s.tableLeaf))
}

func recordResidue(label string, step int, markers string, f finding) {
	tables.residue.Row(label, step, markers, f.rowsSought, f.rowsFound, len(f.pages), f.roles())
}

func run(ctx context.Context, dir string, secureDelete bool) outcome {
	label := "OFF"
	if secureDelete {
		label = "ON"
	}
	path := filepath.Join(dir, "secure-delete-"+strings.ToLower(label), "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	part := "A"
	if secureDelete {
		part = "B"
	}
	var o outcome
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		execAll(ctx, db, insert, insertRows("SECRET", 1, initialRows))
	})
	base := readState(path)
	o.sizes = append(o.sizes, base.size)
	recordStep(label, 1, "insert the rows", base)
	var held []int
	for page, ids := range base.tableLeaf {
		for _, id := range ids {
			if id >= deleteFirst && id <= deleteLast {
				held = append(held, page)
				break
			}
		}
	}

	// Under secure_delete=OFF, freePage2 (btree.c:6836-6969) calls
	// sqlite3PagerDontWrite (pager.c:6329-6339) for freed freelist leaves, so
	// they are not written to the WAL and retain their old bytes; freeSpace
	// (btree.c:1918-2014) likewise skips zeroing freeblocks on live leaves.
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN ? AND ?", deleteFirst, deleteLast); err != nil {
			fatal("delete", err)
		}
		o.deleteFrames = walFrames(path)
	})
	deleted := readState(path)
	o.sizes = append(o.sizes, deleted.size)
	recordStep(label, 2, "delete rows", deleted)
	free := deleted.freePages()
	var freedFromHeld []int
	for _, p := range held {
		if free[p] {
			freedFromHeld = append(freedFromHeld, p)
		}
	}
	tables.deleteStep.Row(label, deleteLast-deleteFirst+1, compactPages(held), compactPages(append(append([]int(nil), deleted.trunks...), deleted.leaves...)), len(freedFromHeld), o.deleteFrames)
	set.Check(part, deleted.size == base.size, "the file does not shrink on delete")
	set.Check(part, deleted.freelist > 0 && len(freedFromHeld) > 0, "pages emptied by the delete moved to the freelist")
	o.afterDelete = searchDeleted(deleted)
	recordResidue(label, 2, "SECRET", o.afterDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		execAll(ctx, db, insert, insertRows("NEW", 1001, reuseRows))
	})
	reused := readState(path)
	o.sizes = append(o.sizes, reused.size)
	var newPages, fromFreelist []int
	for page, ids := range reused.tableLeaf {
		for _, id := range ids {
			if id >= 1001 {
				newPages = append(newPages, page)
				if free[page] {
					fromFreelist = append(fromFreelist, page)
				}
				break
			}
		}
	}
	recordStep(label, 3, "insert new rows", reused)
	tables.reuseStep.Row(label, reuseRows, compactPages(newPages), len(newPages), len(fromFreelist))
	set.Check(part, reused.size == base.size, "the new rows fit in the file: its size is unchanged")
	set.Check(part, len(newPages) > 0 && len(fromFreelist) == len(newPages), "every leaf page of the new rows is a former freelist page")
	o.afterReuse = searchDeleted(reused)
	recordResidue(label, 3, "SECRET", o.afterReuse)

	grewBeforeEmpty := false
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		next := int64(2001)
		for batch := 1; batch <= growBatches; batch++ {
			freeBefore := pragmaInt(ctx, db, "freelist_count")
			pagesBefore := pragmaInt(ctx, db, "page_count")
			execAll(ctx, db, insert, insertRows("GROW", next, rowsPerTx))
			next += rowsPerTx
			freeAfter := pragmaInt(ctx, db, "freelist_count")
			pagesAfter := pragmaInt(ctx, db, "page_count")
			if pagesAfter > pagesBefore && freeAfter > 0 {
				grewBeforeEmpty = true
			}
			tables.growBatches.Row(label, batch, freeBefore, freeAfter, pagesBefore, pagesAfter)
		}
	})
	grown := readState(path)
	o.sizes = append(o.sizes, grown.size)
	recordStep(label, 4, "grow with more inserts", grown)
	set.Check(part, !grewBeforeEmpty, "page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist")
	set.Check(part, grown.size > base.size && grown.freelist == 0, "once the freelist is used up, the file grows")

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id >= 1001"); err != nil {
			fatal("delete", err)
		}
	})
	again := readState(path)
	o.sizes = append(o.sizes, again.size)
	recordStep(label, 5, "delete the new rows", again)
	set.Check(part, again.size == grown.size, "the file still does not shrink")
	o.afterSecondDelete = searchMarkers(again, secondDeletion)
	recordResidue(label, 5, "NEW, GROW", o.afterSecondDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
			fatal("vacuum", err)
		}
	})
	vacuumed := readState(path)
	o.sizes = append(o.sizes, vacuumed.size)
	o.afterVacuum = searchMarkers(vacuumed, append(append([]markerRange(nil), firstDeletion...), secondDeletion...))
	recordStep(label, 6, "VACUUM", vacuumed)
	recordResidue(label, 6, "SECRET, NEW, GROW", o.afterVacuum)
	set.Check(part, vacuumed.freelist == 0 && vacuumed.pages <= again.pages-again.freelist, fmt.Sprintf("VACUUM is what returns space: %d pages -> %d, at least the %d freelist pages are gone", again.pages, vacuumed.pages, again.freelist))
	set.Check(part, o.afterVacuum.rowsFound == 0, "after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file")
	return o
}

func freshPath(dir, name string) string {
	path := filepath.Join(dir, name, "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	return path
}

func execOne(ctx context.Context, db *sqlitex.DB, statement string, args ...any) {
	if _, err := db.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
}

// largeTransaction verifies that allocateBtreePage (btree.c:6514-6822) pops
// freelist pages before extending the file on a per-allocation basis, so one
// transaction can drain the freelist and grow page_count in the same commit.
func largeTransaction(ctx context.Context, dir string) {
	path := freshPath(dir, "large-transaction")
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	var freeBefore, freeAfter, pagesBefore, pagesAfter int
	session(ctx, path, true, func(db *sqlitex.DB) {
		execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		execAll(ctx, db, insert, insertRows("SECRET", 1, 40))
		execOne(ctx, db, "DELETE FROM t WHERE id <= 8")
		freeBefore, pagesBefore = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
		execAll(ctx, db, insert, insertRows("NEW", 101, 32))
		freeAfter, pagesAfter = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
	})
	tables.largeTransaction.Row(32, 8, freeBefore, pagesBefore, freeAfter, pagesAfter)
	set.Check("D", freeBefore > 0 && freeAfter == 0 && pagesAfter > pagesBefore, "a transaction that needs more pages than the freelist holds takes the freelist pages first, then grows the file in the same transaction: so the rule is per page allocation, not per transaction")
}

// autoVacuumModes contrasts NONE (default, never shrinks without VACUUM), FULL
// (relocates pages and truncates during the DELETE's own commit), and
// INCREMENTAL (keeps freed pages on the freelist until PRAGMA incremental_vacuum).
func autoVacuumModes(ctx context.Context, dir string) {
	type result struct{ created, afterDelete, freeAfterDelete, afterIncremental int64 }
	outcomes := map[string]result{}
	for _, mode := range []string{sqlitex.AutoVacuumNone, sqlitex.AutoVacuumFull, sqlitex.AutoVacuumIncremental} {
		path := freshPath(dir, "auto-vacuum-"+strings.ToLower(mode))
		options := sqlitex.DefaultOptions()
		options.SingleConnection = true
		options.File.AutoVacuum = mode
		pages := func() int64 {
			if err := recipe.CheckAtRest(path); err != nil {
				fatal("at rest", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				fatal("stat", err)
			}
			return info.Size() / pageSize
		}
		withDB := func(work func(db *sqlitex.DB)) {
			db, err := sqlitex.Open(ctx, path, options)
			if err != nil {
				fatal("open", err)
			}
			work(db)
			if err := db.Close(); err != nil {
				fatal("close", err)
			}
		}
		var r result
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows("SECRET", 1, 200))
		})
		r.created = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "DELETE FROM t WHERE id <= 150")
			r.freeAfterDelete = int64(pragmaInt(ctx, db, "freelist_count"))
		})
		r.afterDelete = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "PRAGMA incremental_vacuum")
		})
		r.afterIncremental = pages()
		outcomes[mode] = r
		tables.autoVacuum.Row(mode, r.created, r.afterDelete, r.freeAfterDelete, r.afterIncremental)
	}
	none, full, incremental := outcomes[sqlitex.AutoVacuumNone], outcomes[sqlitex.AutoVacuumFull], outcomes[sqlitex.AutoVacuumIncremental]
	set.Check("E", none.afterDelete == none.created && none.afterIncremental == none.created && none.freeAfterDelete > 0, "auto_vacuum=NONE (SQLite's default and the recipe's): the file never shrinks on its own; PRAGMA incremental_vacuum does nothing")
	set.Check("E", full.afterDelete < full.created && full.freeAfterDelete == 0, "auto_vacuum=FULL: the DELETE's own commit moves pages and truncates the file, leaving no freelist")
	set.Check("E", incremental.afterDelete == incremental.created && incremental.freeAfterDelete > 0 && incremental.afterIncremental < incremental.afterDelete, "auto_vacuum=INCREMENTAL: freed pages wait on the freelist until PRAGMA incremental_vacuum truncates the file")
}

func countMarkers(data []byte, r markerRange) int {
	found := 0
	for id := r.first; id <= r.last; id++ {
		if bytes.Contains(data, marker(r.prefix, id)) {
			found++
		}
	}
	return found
}

// walResidue shows that secure_delete=ON zeroes pages in the database file but
// does not erase earlier INSERT frames already sitting in -wal until the WAL
// is reset (for example by wal_checkpoint(TRUNCATE)).
func walResidue(ctx context.Context, dir string) {
	path := freshPath(dir, "wal-residue")
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	secrets := markerRange{"SECRET", 5001, 5040}
	inWAL := func() int {
		data, err := os.ReadFile(path + "-wal")
		if err != nil {
			fatal("read -wal", err)
		}
		return countMarkers(data, secrets)
	}
	checkpoint := func(mode string) {
		result, err := sqlitex.Checkpoint(ctx, db, "main", mode)
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows(secrets.prefix, secrets.first, secrets.last-secrets.first+1))
	execOne(ctx, db, "DELETE FROM t")
	afterDelete := inWAL()
	checkpoint("PASSIVE")
	afterPassive := inWAL()
	checkpoint("TRUNCATE")
	afterTruncate := inWAL()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	atRest := countMarkers(image, secrets)
	total := int(secrets.last - secrets.first + 1)
	tables.walResidue.Row(1, "-wal after the DELETE", afterDelete, total)
	tables.walResidue.Row(2, "-wal after wal_checkpoint(PASSIVE)", afterPassive, total)
	tables.walResidue.Row(3, "-wal after wal_checkpoint(TRUNCATE)", afterTruncate, total)
	tables.walResidue.Row(4, "database file at rest", atRest, total)
	set.Check("F", afterDelete == total && afterPassive == total, "secure_delete zeroes the database pages, but the -wal still holds the frames the INSERT wrote: every deleted row stays readable there, even after a PASSIVE checkpoint copied the zeroed pages back")
	set.Check("F", afterTruncate == 0 && atRest == 0, "only when the WAL is reset (here by wal_checkpoint(TRUNCATE)) are the deleted rows gone from the files on disk")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.2")
	set = results.MustOpen(dir)
	openTables()
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	for _, setting := range [][2]any{{"page_size", pageSize}, {"blob_bytes", blobBytes}, {"initial_rows", initialRows}, {"delete_first", deleteFirst}, {"delete_last", deleteLast}, {"reuse_rows", reuseRows}, {"grow_batches", growBatches}, {"rows_per_tx", rowsPerTx}} {
		tables.settings.Row(setting[0], setting[1])
	}
	off := run(ctx, dir, false)
	on := run(ctx, dir, true)
	set.Check("C", on.deleteFrames > off.deleteFrames, "secure_delete costs WAL frames: the zeroed pages are written, while without it freed leaf pages are not written at all")
	set.Check("C", off.afterDelete.rowsFound > 0, "without secure_delete the deleted payload bytes are still in the file")
	set.Check("C", off.afterDelete.byRole["freelist-leaf"] > 0, "without secure_delete freelist leaf pages keep their old content")
	set.Check("C", off.afterSecondDelete.rowsFound > 0, "the second delete leaves its rows readable too without secure_delete")
	set.Check("C", on.afterDelete.rowsFound == 0 && on.afterReuse.rowsFound == 0 && on.afterSecondDelete.rowsFound == 0, "with secure_delete the deleted payload bytes are nowhere in the database file at rest")
	set.Check("C", fmt.Sprint(off.sizes) == fmt.Sprint(on.sizes), "secure_delete changes the bytes, not the layout: file sizes are the same at every step")
	largeTransaction(ctx, dir)
	autoVacuumModes(ctx, dir)
	walResidue(ctx, dir)
	set.Finish("DELETE AND REUSE")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/3.2-delete-and-reuse/report.sql -->
```sql
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
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
make run EXP=3.2
```

Add `-i` (`make run EXP=3.2 ARGS='-i'`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/3.2-delete-and-reuse/`](evidence/3.2-delete-and-reuse/), and `make check ARGS='evidence ch03'` compares a fresh run with them byte for byte.

<!-- include: evidence/3.2-delete-and-reuse.txt -->
```text

════════════════════════════════════════════════════════════════════════
  3.2  Does a DELETE return space or erase bytes?
════════════════════════════════════════════════════════════════════════

Table t(id INTEGER PRIMARY KEY, v BLOB). Each blob is 1000 bytes and repeats
a marker such as SECRET-000150, so a deleted row can be searched for in the
file. Parts A and B run the same six steps with secure_delete=OFF and ON.
Every step is its own connection, so the file is at rest and checkpointed
when it is read. Settings: journal_mode=wal, synchronous=FULL.

The constants:
┌──────────────┬───────┐
│     name     │ value │
├──────────────┼───────┤
│ blob_bytes   │ 1000  │
│ delete_first │ 99    │
│ delete_last  │ 302   │
│ grow_batches │ 30    │
│ initial_rows │ 400   │
│ page_size    │ 4096  │
│ reuse_rows   │ 100   │
│ rows_per_tx  │ 4     │
└──────────────┴───────┘

── A. secure_delete=OFF (the default) ─────────────────────────────

A1. The file after each step. Steps: 1 insert the rows, 2 delete ids 99..302,
    3 insert 100 new rows (ids 1001..1100), 4 insert 30 transactions of 4 rows,
    5 delete ids >= 1001, 6 VACUUM. Only VACUUM makes the file smaller.
┌──────┬────────────────────────┬────────────┬───────┬────────────────┬───────┬─────────────────┬──────────────────┐
│ step │          what          │ file_bytes │ pages │ freelist_pages │ trunk │ freelist_leaves │ table_leaf_pages │
├──────┼────────────────────────┼────────────┼───────┼────────────────┼───────┼─────────────────┼──────────────────┤
│ 1    │ insert the rows        │ 417792     │ 102   │ 0              │       │ 0               │ 100              │
│ 2    │ delete rows            │ 417792     │ 102   │ 50             │ 29    │ 49              │ 50               │
│ 3    │ insert new rows        │ 417792     │ 102   │ 25             │ 29    │ 24              │ 75               │
│ 4    │ grow with more inserts │ 438272     │ 107   │ 0              │       │ 0               │ 105              │
│ 5    │ delete the new rows    │ 438272     │ 107   │ 55             │ 102   │ 54              │ 50               │
│ 6    │ VACUUM                 │ 208896     │ 51    │ 0              │       │ 0               │ 49               │
└──────┴────────────────────────┴────────────┴───────┴────────────────┴───────┴─────────────────┴──────────────────┘

A2. Step 2: the delete empties leaf pages and moves them to the freelist.
    Compare held_on_leaf_pages with freelist_pages. wal_frames is what the
    commit of the DELETE wrote to the -wal.
┌──────────────┬────────────────────┬────────────────┬─────────────────────┬────────────┐
│ rows_deleted │ held_on_leaf_pages │ freelist_pages │ former_leaves_freed │ wal_frames │
├──────────────┼────────────────────┼────────────────┼─────────────────────┼────────────┤
│ 204          │ 27-78              │ 29-78          │ 50                  │ 5          │
└──────────────┴────────────────────┴────────────────┴─────────────────────┴────────────┘

A3. Step 3: the new rows go into former freelist pages, so the file does not grow.
┌──────────┬────────────────┬────────────────┬─────────────────────┐
│ new_rows │ new_leaf_pages │ new_leaf_count │ taken_from_freelist │
├──────────┼────────────────┼────────────────┼─────────────────────┤
│ 100      │ 30,55-78       │ 25             │ 25                  │
└──────────┴────────────────┴────────────────┴─────────────────────┘

A4. Step 4: page_count grows only when the freelist is empty. Batch 1, every
    batch that grew the file, and the batch that emptied the freelist:
┌───────┬─────────────────┬────────────────┬──────────────┬─────────────┐
│ batch │ freelist_before │ freelist_after │ pages_before │ pages_after │
├───────┼─────────────────┼────────────────┼──────────────┼─────────────┤
│ 1     │ 25              │ 24             │ 102          │ 102         │
│ 25    │ 1               │ 0              │ 102          │ 102         │
│ 26    │ 0               │ 0              │ 102          │ 103         │
│ 27    │ 0               │ 0              │ 103          │ 104         │
│ 28    │ 0               │ 0              │ 104          │ 105         │
│ 29    │ 0               │ 0              │ 105          │ 106         │
│ 30    │ 0               │ 0              │ 106          │ 107         │
└───────┴─────────────────┴────────────────┴──────────────┴─────────────┘

A5. Searching the file at rest for the markers of the deleted rows. roles names
    the kind of page each marker was found on.
┌──────┬───────────────────┬─────────────┬────────────┬────────────────────┬────────────────────────────────────────────────┐
│ step │ markers_searched  │ rows_sought │ rows_found │ pages_with_markers │                     roles                      │
├──────┼───────────────────┼─────────────┼────────────┼────────────────────┼────────────────────────────────────────────────┤
│ 2    │ SECRET            │ 204         │ 199        │ 52                 │ freelist-leaf=49 freelist-trunk=1 table-leaf=2 │
│ 3    │ SECRET            │ 204         │ 104        │ 27                 │ freelist-leaf=24 freelist-trunk=1 table-leaf=2 │
│ 5    │ NEW, GROW         │ 220         │ 217        │ 55                 │ freelist-leaf=54 freelist-trunk=1              │
│ 6    │ SECRET, NEW, GROW │ 424         │ 0          │ 0                  │                                                │
└──────┴───────────────────┴─────────────┴────────────┴────────────────────┴────────────────────────────────────────────────┘

── B. secure_delete=ON ─────────────────────────────

B1. The file after each step. Steps: 1 insert the rows, 2 delete ids 99..302,
    3 insert 100 new rows (ids 1001..1100), 4 insert 30 transactions of 4 rows,
    5 delete ids >= 1001, 6 VACUUM. Only VACUUM makes the file smaller.
┌──────┬────────────────────────┬────────────┬───────┬────────────────┬───────┬─────────────────┬──────────────────┐
│ step │          what          │ file_bytes │ pages │ freelist_pages │ trunk │ freelist_leaves │ table_leaf_pages │
├──────┼────────────────────────┼────────────┼───────┼────────────────┼───────┼─────────────────┼──────────────────┤
│ 1    │ insert the rows        │ 417792     │ 102   │ 0              │       │ 0               │ 100              │
│ 2    │ delete rows            │ 417792     │ 102   │ 50             │ 29    │ 49              │ 50               │
│ 3    │ insert new rows        │ 417792     │ 102   │ 25             │ 29    │ 24              │ 75               │
│ 4    │ grow with more inserts │ 438272     │ 107   │ 0              │       │ 0               │ 105              │
│ 5    │ delete the new rows    │ 438272     │ 107   │ 55             │ 102   │ 54              │ 50               │
│ 6    │ VACUUM                 │ 208896     │ 51    │ 0              │       │ 0               │ 49               │
└──────┴────────────────────────┴────────────┴───────┴────────────────┴───────┴─────────────────┴──────────────────┘

B2. Step 2: the delete empties leaf pages and moves them to the freelist.
    Compare held_on_leaf_pages with freelist_pages. wal_frames is what the
    commit of the DELETE wrote to the -wal.
┌──────────────┬────────────────────┬────────────────┬─────────────────────┬────────────┐
│ rows_deleted │ held_on_leaf_pages │ freelist_pages │ former_leaves_freed │ wal_frames │
├──────────────┼────────────────────┼────────────────┼─────────────────────┼────────────┤
│ 204          │ 27-78              │ 29-78          │ 50                  │ 54         │
└──────────────┴────────────────────┴────────────────┴─────────────────────┴────────────┘

B3. Step 3: the new rows go into former freelist pages, so the file does not grow.
┌──────────┬────────────────┬────────────────┬─────────────────────┐
│ new_rows │ new_leaf_pages │ new_leaf_count │ taken_from_freelist │
├──────────┼────────────────┼────────────────┼─────────────────────┤
│ 100      │ 30,55-78       │ 25             │ 25                  │
└──────────┴────────────────┴────────────────┴─────────────────────┘

B4. Step 4: page_count grows only when the freelist is empty. Batch 1, every
    batch that grew the file, and the batch that emptied the freelist:
┌───────┬─────────────────┬────────────────┬──────────────┬─────────────┐
│ batch │ freelist_before │ freelist_after │ pages_before │ pages_after │
├───────┼─────────────────┼────────────────┼──────────────┼─────────────┤
│ 1     │ 25              │ 24             │ 102          │ 102         │
│ 25    │ 1               │ 0              │ 102          │ 102         │
│ 26    │ 0               │ 0              │ 102          │ 103         │
│ 27    │ 0               │ 0              │ 103          │ 104         │
│ 28    │ 0               │ 0              │ 104          │ 105         │
│ 29    │ 0               │ 0              │ 105          │ 106         │
│ 30    │ 0               │ 0              │ 106          │ 107         │
└───────┴─────────────────┴────────────────┴──────────────┴─────────────┘

B5. Searching the file at rest for the markers of the deleted rows. roles names
    the kind of page each marker was found on.
┌──────┬───────────────────┬─────────────┬────────────┬────────────────────┬───────┐
│ step │ markers_searched  │ rows_sought │ rows_found │ pages_with_markers │ roles │
├──────┼───────────────────┼─────────────┼────────────┼────────────────────┼───────┤
│ 2    │ SECRET            │ 204         │ 0          │ 0                  │       │
│ 3    │ SECRET            │ 204         │ 0          │ 0                  │       │
│ 5    │ NEW, GROW         │ 220         │ 0          │ 0                  │       │
│ 6    │ SECRET, NEW, GROW │ 424         │ 0          │ 0                  │       │
└──────┴───────────────────┴─────────────┴────────────┴────────────────────┴───────┘

── C. Comparison ───────────────────────────────────────────────────────

C1. secure_delete changes the bytes, not the layout: the file size after each
    step is the same in both runs.
┌──────┬────────────────────────┬───────────┬──────────┐
│ step │          what          │ bytes_off │ bytes_on │
├──────┼────────────────────────┼───────────┼──────────┤
│ 1    │ insert the rows        │ 417792    │ 417792   │
│ 2    │ delete rows            │ 417792    │ 417792   │
│ 3    │ insert new rows        │ 417792    │ 417792   │
│ 4    │ grow with more inserts │ 438272    │ 438272   │
│ 5    │ delete the new rows    │ 438272    │ 438272   │
│ 6    │ VACUUM                 │ 208896    │ 208896   │
└──────┴────────────────────────┴───────────┴──────────┘

C2. What a reader of the file finds, and what the DELETE cost in WAL frames:
┌───────────────┬───────────────────────┬──────────────────────┬──────────────────────────────┬───────────────────┐
│ secure_delete │ readable_after_delete │ readable_after_reuse │ readable_after_second_delete │ delete_wal_frames │
├───────────────┼───────────────────────┼──────────────────────┼──────────────────────────────┼───────────────────┤
│ OFF           │ 199                   │ 104                  │ 217                          │ 5                 │
│ ON            │ 0                     │ 0                    │ 0                            │ 54                │
└───────────────┴───────────────────────┴──────────────────────┴──────────────────────────────┴───────────────────┘

── D. A transaction that needs more pages than the freelist holds ──────

secure_delete=ON, WAL. 40 rows, DELETE of ids 1..8, then one transaction of 32
new rows (8 leaves worth). It takes the freelist pages first and then grows
the file, all in one transaction.
┌───────────────┬──────────────┬─────────────────┬────────────────┬───────────────────┬──────────────────┐
│ rows_inserted │ deleted_rows │ freelist_before │ freelist_after │ page_count_before │ page_count_after │
├───────────────┼──────────────┼─────────────────┼────────────────┼───────────────────┼──────────────────┤
│ 32            │ 8            │ 2               │ 0              │ 12                │ 18               │
└───────────────┴──────────────┴─────────────────┴────────────────┴───────────────────┴──────────────────┘

── E. auto_vacuum ──────────────────────────────────────────────────────

The file shrinks on its own only when the database was created with
auto_vacuum FULL or INCREMENTAL. Each mode: 200 rows, DELETE of 150 rows,
then PRAGMA incremental_vacuum, each in its own connection.
┌─────────────┬───────────────┬────────────────────┬───────────────────────┬────────────────────────────────┐
│    mode     │ pages_created │ pages_after_delete │ freelist_after_delete │ pages_after_incremental_vacuum │
├─────────────┼───────────────┼────────────────────┼───────────────────────┼────────────────────────────────┤
│ NONE        │ 52            │ 52                 │ 37                    │ 52                             │
│ FULL        │ 53            │ 16                 │ 0                     │ 16                             │
│ INCREMENTAL │ 53            │ 53                 │ 37                    │ 16                             │
└─────────────┴───────────────┴────────────────────┴───────────────────────┴────────────────────────────────┘

── F. secure_delete=ON and the -wal file ───────────────────────────────

wal_autocheckpoint=0, one connection. 40 rows are inserted, then deleted. How
many of the 40 deleted rows can still be read in the -wal, and in the database
file at rest?
┌─────────────────────────────────────┬───────────────┬────────────┐
│              location               │ rows_readable │ rows_total │
├─────────────────────────────────────┼───────────────┼────────────┤
│ -wal after the DELETE               │ 40            │ 40         │
│ -wal after wal_checkpoint(PASSIVE)  │ 40            │ 40         │
│ -wal after wal_checkpoint(TRUNCATE) │ 0             │ 40         │
│ database file at rest               │ 0             │ 40         │
└─────────────────────────────────────┴───────────────┴────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the file does not shrink on delete
A  ✓  pages emptied by the delete moved to the freelist
A  ✓  the new rows fit in the file: its size is unchanged
A  ✓  every leaf page of the new rows is a former freelist page
A  ✓  page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist
A  ✓  once the freelist is used up, the file grows
A  ✓  the file still does not shrink
A  ✓  VACUUM is what returns space: 107 pages -> 51, at least the 55 freelist pages are gone
A  ✓  after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file
B  ✓  the file does not shrink on delete
B  ✓  pages emptied by the delete moved to the freelist
B  ✓  the new rows fit in the file: its size is unchanged
B  ✓  every leaf page of the new rows is a former freelist page
B  ✓  page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist
B  ✓  once the freelist is used up, the file grows
B  ✓  the file still does not shrink
B  ✓  VACUUM is what returns space: 107 pages -> 51, at least the 55 freelist pages are gone
B  ✓  after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file
C  ✓  secure_delete costs WAL frames: the zeroed pages are written, while without it freed leaf pages are not written at all
C  ✓  without secure_delete the deleted payload bytes are still in the file
C  ✓  without secure_delete freelist leaf pages keep their old content
C  ✓  the second delete leaves its rows readable too without secure_delete
C  ✓  with secure_delete the deleted payload bytes are nowhere in the database file at rest
C  ✓  secure_delete changes the bytes, not the layout: file sizes are the same at every step
D  ✓  a transaction that needs more pages than the freelist holds takes the freelist pages first, then grows the file in the same transaction: so the rule is per page allocation, not per transaction
E  ✓  auto_vacuum=NONE (SQLite's default and the recipe's): the file never shrinks on its own; PRAGMA incremental_vacuum does nothing
E  ✓  auto_vacuum=FULL: the DELETE's own commit moves pages and truncates the file, leaving no freelist
E  ✓  auto_vacuum=INCREMENTAL: freed pages wait on the freelist until PRAGMA incremental_vacuum truncates the file
F  ✓  secure_delete zeroes the database pages, but the -wal still holds the frames the INSERT wrote: every deleted row stays readable there, even after a PASSIVE checkpoint copied the zeroed pages back
F  ✓  only when the WAL is reset (here by wal_checkpoint(TRUNCATE)) are the deleted rows gone from the files on disk

30 of 30 claims hold

Experiment verdict: DELETE AND REUSE OK
```

**Reading the evidence:**

- **Initial state and `DELETE` under `secure_delete=OFF` (A1, A2, A5).** Inserting `400` rows of 1,000-byte blobs (each repeating a marker such as `SECRET-000150`) produces a `417,792`-byte (`102`-page) file: page 1, root page 2, and `100` table leaves of 4 rows each. Deleting ids `99..302` (`204` rows, originally spanning leaves `27..78`) leaves the file at `417,792` bytes (`102` pages) and moves `50` pages (`29..78`, all former leaves of the deleted rows) onto the freelist: trunk `[29]` plus `49` freelist leaves. Scanning the closed file finds **`199` of `204` deleted rows still readable** across `52` pages: `49` freelist leaves, `1` freelist trunk (whose bytes after offset `8 + 49×4` were never overwritten), and `2` live table leaves (`27` and `78`, each keeping 2 surviving rows plus freeblocks holding the deleted rows' bytes). That `DELETE` commit wrote only **`5` WAL frames** (page 1, root 2, trunk 29, and the two partial leaves 27 and 78) because `freePage2` marked all 49 freelist leaves `PGHDR_DONT_WRITE` via `sqlite3PagerDontWrite`.
- **Freelist reuse, growth, second delete, and `VACUUM` (A1, A3–A5).** Inserting `100` new rows (`ids 1001..1100`) keeps the file at `417,792` bytes (`102` pages) and drops the freelist from `50` to `25`. All `25` new leaf pages (`30, 55-78`) came straight from the freelist, while `104` of the `204` deleted rows remain readable on `27` pages (`24` freelist leaves, `1` trunk, `2` live leaves). Next, `30` batches of 4 rows drain the remaining `25` freelist pages one per batch (`batch 1`: freelist `25->24`, `page_count 102->102`; `batch 25`: `1->0`, `102->102`), and only `batches 26..30` grow the file (`102->107` pages, `438,272` bytes). Deleting `id >= 1001` (`220` rows) leaves `107` pages with `55` on the freelist (`trunk [102]`, `54` leaves) and `217` of `220` `NEW-`/`GROW-` markers still readable across all `55` freelist pages. Finally, `VACUUM` shrinks the file from `107` pages (`438,272` bytes) to `51` pages (`208,896` bytes: 49 leaves for 196 surviving rows + root + page 1), with freelist `0` and `0` of all `424` deleted rows readable anywhere in the file.
- **`secure_delete=ON` and comparison (section B, C1–C2).** Every file size across all six steps matches `OFF` (`[417792 417792 417792 438272 438272 208896]`): `secure_delete` changes bytes, not layout. Zero deleted rows remain readable in the database file at rest after step 2 (`0 of 204`), step 3 (`0 of 204`), step 5 (`0 of 220`), or step 6 (`0 of 424`). The cost shows up in the WAL: the step-2 `DELETE` wrote **`54` WAL frames** instead of `5` (`49` extra frames for the zeroed freelist leaves).
- **Large transaction exceeding the freelist (section D).** Starting with `freelist=2` and `page_count=12`, inserting `32` rows (8 leaves' worth) in a single transaction ends with `freelist=0` and `page_count=18`. `allocateBtreePage` pops freelist pages first on a per-allocation basis, then grows the file in the same commit once the freelist hits zero.
- **`auto_vacuum` modes (section E).** Deleting `150` of `200` rows behaves differently in each mode: `NONE` stays `52 -> 52` pages (`freelist 37`), and `PRAGMA incremental_vacuum` does nothing (`52`); `FULL` starts one page larger for its pointer-map page (`53`) and shrinks immediately on the `DELETE`'s commit to `16` pages (`freelist 0`); `INCREMENTAL` stays `53 -> 53` (`freelist 37`) until `PRAGMA incremental_vacuum` truncates it to `16` pages.
- **`secure_delete=ON` and the `-wal` file (section F).** Inserting then deleting `40` secret rows with `secure_delete=ON` and `wal_autocheckpoint=0` leaves **`40` of `40` deleted rows readable inside `-wal`** after the `DELETE` commits—and still `40` of `40` after `PRAGMA wal_checkpoint(PASSIVE)`, which copies the zeroed pages back to `.db` without erasing earlier `INSERT` frames in `-wal`. Only after `PRAGMA wal_checkpoint(TRUNCATE)` resets `-wal` does the count drop to `0` on disk (`0` in `-wal` and `0` in `.db` at rest).

The sequence below contrasts the WAL frame counts for `secure_delete=OFF` versus `ON` (Parts A and B) and traces why deleted rows remain in `-wal` until a `TRUNCATE` checkpoint (Part F):

<!-- draw-visual: diagrams/ch03-btrees-and-page-management-delete-wal.mmd -->
```text
      ┌────────┐     ┌──────┐     ┌─────┐
      │ SQLite │     │ -wal │     │ .db │
      └────┬───┘     └───┬──┘     └──┬──┘
           │             │           │
           │ 1. INSERT rows          │
           ├────────────►│           │
           │             │           │
           │    ┌──────────────────┐ │
           │    │ old rows in -wal │ │
           │    └──────────────────┘ │
┌─[alt secure_delete OFF]──────────────┐
│          │             │           │ │
│          │ 2. DELETE 5 frames      │ │
│          ├────────────►│           │ │
│          │             │           │ │
│┌────────────────────┐  │           │ │
││ DontWrite skips 49 │  │           │ │
│└────────────────────┘  │           │ │
├┈[secure_delete ON]┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┤
│          │             │           │ │
│          │ 3. DELETE 54 zeroed     │ │
│          ├────────────►│           │ │
│          │             │           │ │
└──────────────────────────────────────┘
           │             │           │
         ┌──────────────────────────────┐
         │ secure_delete ON checkpoints │
         └──────────────────────────────┘
           │             │           │
           │ 4. checkpoint PASSIVE   │
           ├────────────►│           │
           │             │           │
           │             │ 5. copy zeroed pages
           │             ├──────────►│
           │             │           │
           │  ┌──────────────────────┐
           │  │ INSERT frames remain │
           │  └──────────────────────┘
           │             │           │
           │ 6. checkpoint TRUNCATE  │
           ├────────────►│           │
           │             │           │
           │    ┌──────────────────┐ │
           │    │ reset to 0 bytes │ │
           │    └──────────────────┘ │
           │             │           │
```

---

## 6. Evidence and failure cases

### 6.1 Experiment 3.3 — Same rows, different bytes, rung R2 (`3.3-same-rows-different-bytes`)

- **Claim:** Physical layout records history, not just logical content. Different insert orders or update/delete histories over the same final rows produce different database files (`E1` and `E2` differ), whereas append-only batching in WAL mode produces byte-identical files (commit boundaries show up only in rollback mode's change counter, `E2`-equal). Every build shares one `E3` logical hash, one deterministic ordered logical capture (`capture/r0.sql`), and one `VACUUM INTO` SHA-256 (`E1` between canonical forms) alongside the replayed capture—provided the databases share the same schema creation order and schema cookie (a `sqlite3_backup` copy gets schema cookie `1` instead of `4`, so its canonical file differs at byte `43`). Our `E3` hash closes six blind spots where `dbhash` (`5` of `6`, SQLite's C hashing tool) or `sqldiff` (`1` of `6`, SQLite's C logical diff utility) fail.
- **Evidence form:** `EXACT` (every stored CSV table must match a fresh run byte for byte).
- **Capture / rebuild / canonical form (rung R2):** capture = ordered SQL script emitted by `equal.WalkLogical` with `CREATE` statements in `main.sqlite_schema` rowid order (`capture/r0.sql`); rebuild = replaying that script into a fresh file; canonical form = `VACUUM INTO`.

<!-- include: experiments/3.3-same-rows-different-bytes/main.go -->
```go
// Experiment 3.3 implements Rebuild Ladder rung R2 (canonical logical capture
// and replay) and tests E1, E2, and E3 equality across different build
// histories, a sqlite3_backup copy, dbhash blind spots, and VACUUM INTO's
// canonical-form boundaries.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 3.3
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	pageSize    = 4096
	shuffleSeed = 3
)

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 3.3" runs both.
var (
	set    *results.Set
	tables struct {
		builds, r0Tables, r0Facts, capture, captureLines, compare, headerValues,
		canonicalHeader, pairs, limits *results.Table
	}
)

func openTables() {
	tables.builds = set.Table("builds", "seq", "name", "how", "bytes", "file_sha256", "logical_sha256", "dump_sha256", "dbhash", "canonical_sha256")
	tables.r0Tables = set.Table("r0_tables", "table_name", "rows")
	tables.r0Facts = set.Table("r0_facts", "name", "value")
	tables.capture = set.Table("capture", "path", "bytes", "lines", "sha256")
	tables.captureLines = set.Table("capture_lines", "line_no", "text")
	tables.compare = set.Table("compare", "seq", "part", "subject", "what", "level", "equal", "differing_pages", "pages_total", "size_a", "size_b", "header_bytes_differing", "differences")
	tables.headerValues = set.Table("header_values", "part", "subject", "field", "value_a", "value_b")
	tables.canonicalHeader = set.Table("canonical_header", "bytes", "byte_18", "byte_19", "change_counter", "schema_cookie")
	tables.pairs = set.Table("pairs", "seq", "pair", "ours_equal", "dbhash_equal", "first_difference")
	tables.limits = set.Table("limits", "seq", "case_name", "dbhash_sources_equal", "captures_equal")
}

var compareSeq int

// recordFiles writes an E1 or E2 comparison; level is "E1" or "E2".
func recordFiles(part, subject, what, level string, r equal.Result) {
	var header any
	if len(r.IgnoredDiffering) > 0 {
		parts := make([]string, len(r.IgnoredDiffering))
		for i, b := range r.IgnoredDiffering {
			parts[i] = fmt.Sprint(b)
		}
		header = strings.Join(parts, " ")
	}
	compareSeq++
	tables.compare.Row(compareSeq, part, subject, what, level, r.Equal, r.DifferingPages, r.SizeA/int64(r.PageSize), r.SizeA, r.SizeB, header, nil)
}

// recordLogical writes an E3 comparison.
func recordLogical(part, subject, what string, r equal.LogicalResult) {
	compareSeq++
	tables.compare.Row(compareSeq, part, subject, what, "E3", r.Equal, nil, nil, nil, nil, nil, strings.Join(r.Differences, "; "))
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func must[T any](value T, err error) func(string) T {
	return func(what string) T {
		if err != nil {
			fatal(what, err)
		}
		return value
	}
}

type row struct {
	table  equal.TableInfo
	values []any
}

type content struct {
	schema      []string
	rows        []row
	userVersion int64
}

func readContent(ctx context.Context, path string) content {
	db := must(equal.OpenExisting(ctx, path))("open " + filepath.Base(path))
	defer db.Close()
	var c content
	rows := must(db.QueryContext(ctx, "SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY rowid"))("schema")
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			fatal("schema", err)
		}
		c.schema = append(c.schema, s)
	}
	if err := rows.Err(); err != nil {
		fatal("schema", err)
	}
	if err := rows.Close(); err != nil {
		fatal("schema", err)
	}
	err := equal.WalkLogical(ctx, db, equal.LogicalVisitor{Row: func(t equal.TableInfo, values []any) error {
		if strings.HasPrefix(t.Name, "sqlite_") {
			return fmt.Errorf("table %s: this lab copies user tables only", t.Name)
		}
		c.rows = append(c.rows, row{table: t, values: values})
		return nil
	}})
	if err != nil {
		fatal("read rows", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&c.userVersion); err != nil {
		fatal("user_version", err)
	}
	return c
}

func insertSQL(t equal.TableInfo) string {
	width := len(t.Columns)
	if !t.WithoutRowid {
		width++
	}
	marks := make([]string, width)
	for i := range marks {
		marks[i] = "?"
	}
	return insertStatement(t, marks)
}

func rowidName(name string) string {
	switch strings.ToLower(name) {
	case "rowid", "_rowid_", "oid":
		return name
	}
	return equal.QuoteIdentifier(name)
}

// insertStatement builds an INSERT statement directly from the table's column
// list and value expressions (either "?" placeholders or SQL literals), avoiding
// string replacement that would corrupt identifiers containing "?".
func insertStatement(t equal.TableInfo, values []string) string {
	var names []string
	if !t.WithoutRowid {
		names = append(names, rowidName(t.RowidColumn))
	}
	for _, c := range t.Columns {
		names = append(names, equal.QuoteIdentifier(c))
	}
	return fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", equal.QuoteIdentifier(t.Name), strings.Join(names, ", "), strings.Join(values, ", "))
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func execOrDie(ctx context.Context, e execer, query string, args ...any) {
	if _, err := e.ExecContext(ctx, query, args...); err != nil {
		fatal(query, err)
	}
}

func openBuild(ctx context.Context, path, mode string) *sqlitex.DB {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	options := recipe.Default().WithJournalMode(mode).SQLiteOptions()
	options.Conn.Synchronous = sqlitex.SyncOff
	return must(sqlitex.Open(ctx, path, options))("open " + filepath.Base(path))
}

func buildOneTransaction(ctx context.Context, path, mode string, c content) {
	db := openBuild(ctx, path, mode)
	tx := must(db.BeginTx(ctx, nil))("begin")
	for _, s := range c.schema {
		execOrDie(ctx, tx, s)
	}
	for _, r := range c.rows {
		execOrDie(ctx, tx, insertSQL(r.table), r.values...)
	}
	execOrDie(ctx, tx, fmt.Sprintf("PRAGMA user_version = %d", c.userVersion))
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func buildPerStatement(ctx context.Context, path, mode string, c content, order []int) {
	db := openBuild(ctx, path, mode)
	for _, s := range c.schema {
		execOrDie(ctx, db, s)
	}
	for _, i := range order {
		r := c.rows[i]
		execOrDie(ctx, db, insertSQL(r.table), r.values...)
	}
	execOrDie(ctx, db, fmt.Sprintf("PRAGMA user_version = %d", c.userVersion))
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

// sqlLiteral renders a scanned SQLite storage-class value as a SQL literal
// that round-trips with the exact same storage class. Embedded NUL bytes in
// TEXT are split with ||char(0)|| because SQLite's parser stops at raw NULs.
func sqlLiteral(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		switch {
		case math.IsInf(v, 1):
			return "1e999"
		case math.IsInf(v, -1):
			return "-1e999"
		}
		text := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eEn") {
			text += ".0"
		}
		return text
	case string:
		parts := strings.Split(v, "\x00")
		for i, part := range parts {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "''") + "'"
		}
		return strings.Join(parts, "||char(0)||")
	case []byte:
		return "X'" + strings.ToUpper(hex.EncodeToString(v)) + "'"
	}
	fatal("literal", fmt.Errorf("unsupported value %T", value))
	return ""
}

// dump emits rung R2's deterministic SQL capture using equal.WalkLogical.
// CREATE statements follow main.sqlite_schema rowid order (creation order) so
// a replayed database matches its source's VACUUM INTO bytes (vacuum.c:143-427
// copies tables in sqlite_schema order).
func dump(ctx context.Context, path string) string {
	db := must(equal.OpenExisting(ctx, path))("open " + filepath.Base(path))
	defer db.Close()
	conn := must(db.Conn(ctx))("conn")
	defer conn.Close()
	execOrDie(ctx, conn, "BEGIN")
	defer conn.ExecContext(ctx, "COMMIT")
	var tables, indexes, others []string
	var body strings.Builder
	var sequence []string
	pragmas := map[string]string{}
	schema := must(conn.QueryContext(ctx, "SELECT type, name, sql FROM main.sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid"))("schema")
	for schema.Next() {
		var kind, name, statement string
		if err := schema.Scan(&kind, &name, &statement); err != nil {
			fatal("schema", err)
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		switch kind {
		case "table":
			tables = append(tables, statement+";")
		case "index":
			indexes = append(indexes, statement+";")
		default:
			others = append(others, statement+";")
		}
	}
	if err := schema.Err(); err != nil {
		fatal("schema", err)
	}
	if err := schema.Close(); err != nil {
		fatal("schema", err)
	}
	err := equal.WalkLogical(ctx, conn, equal.LogicalVisitor{
		Row: func(t equal.TableInfo, values []any) error {
			literals := make([]string, len(values))
			for i, v := range values {
				literals[i] = sqlLiteral(v)
			}
			line := insertStatement(t, literals) + ";\n"
			switch {
			case t.Name == "sqlite_sequence":
				sequence = append(sequence, line)
			case strings.HasPrefix(t.Name, "sqlite_"):
				return fmt.Errorf("table %s: this capture handles sqlite_sequence only", t.Name)
			default:
				body.WriteString(line)
			}
			return nil
		},
		Pragma: func(name, value string) error {
			pragmas[name] = value
			return nil
		},
	})
	if err != nil {
		fatal("dump", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "PRAGMA page_size=%s;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\n", pragmas["page_size"], strings.ToUpper(pragmas["auto_vacuum"]), pragmas["encoding"])
	if pragmas["journal_mode"] == "wal" {
		out.WriteString("PRAGMA journal_mode=WAL;\n")
	}
	out.WriteString("BEGIN;\n")
	for _, s := range tables {
		out.WriteString(s + "\n")
	}
	out.WriteString(body.String())
	if len(sequence) > 0 {
		out.WriteString("DELETE FROM sqlite_sequence;\n")
		for _, s := range sequence {
			out.WriteString(s)
		}
	}
	for _, s := range append(indexes, others...) {
		out.WriteString(s + "\n")
	}
	fmt.Fprintf(&out, "PRAGMA user_version=%s;\nPRAGMA application_id=%s;\nCOMMIT;\n", pragmas["user_version"], pragmas["application_id"])
	return out.String()
}

func replay(ctx context.Context, script, path string) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	uri := must(sqlitex.FileURI(path, url.Values{}))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, script); err != nil {
		fatal("replay", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func vacuumInto(ctx context.Context, source, target string) {
	if err := recipe.RemoveDatabase(target); err != nil {
		fatal("remove", err)
	}
	db := must(equal.OpenExisting(ctx, source))("open " + filepath.Base(source))
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		fatal("vacuum into", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func backupCopy(ctx context.Context, source, target string) {
	if err := recipe.RemoveDatabase(target); err != nil {
		fatal("remove", err)
	}
	db := must(equal.OpenExisting(ctx, source))("open " + filepath.Base(source))
	conn := must(db.Conn(ctx))("conn")
	if _, err := sqlitex.Backup(ctx, conn, target, -1, nil); err != nil {
		fatal("backup", err)
	}
	conn.Close()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

type build struct {
	name      string
	how       string
	path      string
	sha       string
	logical   equal.Logical
	dumpSHA   string
	dbhash    string
	canonical string
	canonSHA  string
}

func atRest(path string) {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func locateDBHash() string {
	tool, err := equal.LocateDBHash()
	if err != nil {
		fmt.Printf("NO-DBHASH: %v; the dbhash column is skipped (build it with: go run ./cmd/course build-tools)\n", err)
		return ""
	}
	fmt.Printf("INFO: dbhash %s\n", displayPath(tool))
	return tool
}

func dbhashOf(ctx context.Context, tool, path string) string {
	if tool == "" {
		return ""
	}
	h := must(equal.RunDBHash(ctx, tool, path))("dbhash " + filepath.Base(path))
	atRest(path)
	return h
}

func distinct(values []string) int {
	seen := map[string]bool{}
	for _, v := range values {
		seen[v] = true
	}
	return len(seen)
}

func partA(ctx context.Context, dir, tool string) content {
	r0 := filepath.Join(dir, "builds", "r0.db")
	if err := os.MkdirAll(filepath.Dir(r0), 0o755); err != nil {
		fatal("mkdir", err)
	}
	r := recipe.Default()
	if err := r.Build(ctx, r0, r.Steps); err != nil {
		fatal("build R0", err)
	}
	c := readContent(ctx, r0)
	perTable := map[string]int{}
	for _, row := range c.rows {
		perTable[row.table.Name]++
	}
	var tableNames []string
	for name := range perTable {
		tableNames = append(tableNames, name)
	}
	sort.Strings(tableNames)
	for _, name := range tableNames {
		tables.r0Tables.Row(name, perTable[name])
	}
	builds := []*build{{name: "r0", how: "the recipe: 31 transactions with updates and deletes, index created at step 5"}}
	builds[0].path = r0
	oneTx := filepath.Join(dir, "builds", "one-transaction.db")
	buildOneTransaction(ctx, oneTx, "wal", c)
	builds = append(builds, &build{name: "one-transaction", how: "schema and every row in one transaction, tables in order, ids ascending", path: oneTx})
	perStatement := filepath.Join(dir, "builds", "per-statement.db")
	inOrder := make([]int, len(c.rows))
	for i := range inOrder {
		inOrder[i] = i
	}
	buildPerStatement(ctx, perStatement, "wal", c, inOrder)
	builds = append(builds, &build{name: "per-statement", how: "the same order, one transaction per statement", path: perStatement})
	shuffled := filepath.Join(dir, "builds", "shuffled.db")
	order := append([]int(nil), inOrder...)
	rng := rand.New(rand.NewPCG(shuffleSeed, 0))
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	buildPerStatement(ctx, shuffled, "wal", c, order)
	builds = append(builds, &build{name: "shuffled", how: fmt.Sprintf("one transaction per row, rows of all tables in a shuffled order (PCG seed %d)", shuffleSeed), path: shuffled})

	captureDir := filepath.Join(dir, "capture")
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	script := dump(ctx, r0)
	dumpPath := filepath.Join(captureDir, "r0.sql")
	if err := os.WriteFile(dumpPath, []byte(script), 0o644); err != nil {
		fatal("write dump", err)
	}
	lines := strings.Split(strings.TrimSuffix(script, "\n"), "\n")
	tables.capture.Row("capture/r0.sql", len(script), len(lines), equal.SHA256Hex([]byte(script)))
	for i, line := range lines {
		if i < 9 || i >= len(lines)-4 {
			if len(line) > 140 {
				line = line[:140] + "..."
			}
			tables.captureLines.Row(i+1, line)
		}
	}
	replayed := filepath.Join(dir, "builds", "replayed.db")
	replay(ctx, string(must(os.ReadFile(dumpPath))("read dump")), replayed)
	builds = append(builds, &build{name: "replayed", how: "the capture file replayed into an empty file (indexes created after the rows)", path: replayed})
	backup := filepath.Join(dir, "builds", "backup.db")
	backupCopy(ctx, r0, backup)
	builds = append(builds, &build{name: "backup", how: "sqlite3_backup of R0 (a page copy, for the E2 row)", path: backup})

	canonicalDir := filepath.Join(dir, "canonical")
	if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	for _, b := range builds {
		atRest(b.path)
		b.sha = must(equal.FileSHA256(b.path))("hash")
		b.logical = must(equal.LogicalHashFile(ctx, b.path))("logical hash " + b.name)
		atRest(b.path)
		b.dumpSHA = equal.SHA256Hex([]byte(dump(ctx, b.path)))
		b.dbhash = dbhashOf(ctx, tool, b.path)
		b.canonical = filepath.Join(canonicalDir, b.name+".db")
		vacuumInto(ctx, b.path, b.canonical)
		atRest(b.path)
		atRest(b.canonical)
		b.canonSHA = must(equal.FileSHA256(b.canonical))("hash")
		if again := must(equal.FileSHA256(b.path))("hash"); again != b.sha {
			fatal("hash", fmt.Errorf("%s changed while it was hashed", b.name))
		}
	}
	for i, b := range builds {
		info := must(os.Stat(b.path))("stat")
		tables.builds.Row(i+1, b.name, b.how, info.Size(), b.sha, b.logical.SHA256, b.dumpSHA, orNil(b.dbhash), b.canonSHA)
	}
	r0Logical := builds[0].logical
	tables.r0Facts.Row("schema_rows", r0Logical.SchemaRows)
	for _, p := range r0Logical.Pragmas {
		tables.r0Facts.Row(p.Name, p.Value)
	}
	tables.r0Facts.Row("integrity_check", r0Logical.Integrity)
	tables.r0Facts.Row("foreign_key_check_rows", len(r0Logical.ForeignKeyCheck))
	var shas, logicals, dumps, hashes, canon []string
	for _, b := range builds {
		e1 := must(equal.CompareFiles(r0, b.path, pageSize))("E1")
		e2 := must(equal.ComparePageFiles(r0, b.path, pageSize))("E2")
		e3 := equal.CompareLogical(builds[0].logical, b.logical)
		recordFiles("A", b.name, "build vs r0", "E1", e1)
		recordFiles("A", b.name, "build vs r0", "E2", e2)
		recordLogical("A", b.name, "build vs r0", e3)
		if b.name != "backup" {
			shas = append(shas, b.sha)
		}
		logicals = append(logicals, b.logical.SHA256)
		dumps = append(dumps, b.dumpSHA)
		hashes = append(hashes, b.dbhash)
		canon = append(canon, b.canonSHA)
		if b.name == "backup" {
			set.Check("A", !e1.Equal && e2.Equal, fmt.Sprintf("the backup copy differs from R0 in bytes (%s) but is equal at E2: only declared header bytes differ", e2))
		}
	}
	set.Check("A", builds[1].sha == builds[2].sha, "in WAL mode one transaction and one transaction per statement give byte-identical files: commit boundaries alone leave no trace when rows are only appended")
	set.Check("A", builds[3].sha != builds[1].sha && builds[0].sha != builds[1].sha && builds[0].sha != builds[3].sha, "insert order (shuffled) and history (R0's updates and deletes) each give a different file")
	set.Check("A", distinct(logicals) == 1, "every build has the same logical hash")
	allE3 := true
	for _, b := range builds {
		if !equal.CompareLogical(builds[0].logical, b.logical).Equal {
			allE3 = false
		}
	}
	set.Check("A", allE3, "every build is equal to R0 at E3 (logical hash, integrity_check ok, foreign_key_check unchanged)")
	set.Check("A", distinct(dumps) == 1, "the ordered logical capture is itself identical for every build: it depends on content and the schema's creation order, not on insert order, batching or updates")
	set.Check("A", distinct(canon[:5]) == 1, "after VACUUM INTO every build, the replayed copy included, has the same sha256 (E1 between canonical forms)")
	// sqlite3_backup_step sets the destination schema cookie to old+1
	// (backup.c:448; 0+1=1 on a fresh file), and sqlite3RunVacuum copies the
	// source schema cookie +1 (vacuum.c:143-427), leaving byte 43 differing.
	backupCanon := builds[5].canonical
	ce1 := must(equal.CompareFiles(builds[0].canonical, backupCanon, pageSize))("E1")
	ce2 := must(equal.ComparePageFiles(builds[0].canonical, backupCanon, pageSize))("E2")
	recordFiles("A", "backup", "canonical form vs canonical form of r0", "E1", ce1)
	recordFiles("A", "backup", "canonical form vs canonical form of r0", "E2", ce2)
	recordCookies("A", "backup", builds[0].canonical, backupCanon)
	set.Check("A", !ce1.Equal && ce2.Equal && fmt.Sprint(ce2.IgnoredDiffering) == "[43]", "except the backup copy: the backup API sets the copy's schema cookie to (old destination cookie + 1) = 1 (backup.c:448), VACUUM INTO carries the cookie + 1, so its canonical form differs at byte 43 only")
	if tool != "" {
		set.Check("A", distinct(hashes) == 1, "dbhash from the pinned tree also calls every build equal: it agrees with our logical hash on equality")
	}
	recordCanonicalHeader(builds[0].canonical)
	ok := true
	for _, b := range builds {
		l := must(equal.LogicalHashFile(ctx, b.canonical))("logical hash")
		if l.SHA256 == b.logical.SHA256 {
			ok = false
		}
		withoutMode := sameExceptJournalMode(b.logical, l)
		if !withoutMode {
			ok = false
		}
	}
	set.Check("A", ok, "VACUUM INTO keeps every row, the schema and user_version/application_id, but writes a rollback-mode file: its logical hash differs from the source only in journal_mode")
	return c
}

func rollbackPair(ctx context.Context, dir string, c content) {
	one := filepath.Join(dir, "builds", "rollback-one-transaction.db")
	per := filepath.Join(dir, "builds", "rollback-per-statement.db")
	buildOneTransaction(ctx, one, "delete", c)
	order := make([]int, len(c.rows))
	for i := range order {
		order[i] = i
	}
	buildPerStatement(ctx, per, "delete", c, order)
	atRest(one)
	atRest(per)
	e1 := must(equal.CompareFiles(one, per, pageSize))("E1")
	e2 := must(equal.ComparePageFiles(one, per, pageSize))("E2")
	ia, ib := must(os.ReadFile(one))("read"), must(os.ReadFile(per))("read")
	recordFiles("A2", "rollback-one-transaction", "vs rollback-per-statement", "E1", e1)
	recordFiles("A2", "rollback-one-transaction", "vs rollback-per-statement", "E2", e2)
	tables.headerValues.Row("A2", "rollback-one-transaction", "change_counter", be32(ia[24:28]), be32(ib[24:28]))
	set.Check("A2", !e1.Equal && e2.Equal, "in rollback mode the batching shows, but only in the change counter (bytes 24-27 and 92-95 count commits): E2 equal")
}

func sameExceptJournalMode(a, b equal.Logical) bool {
	diff := equal.CompareLogical(a, b)
	return len(diff.Differences) == 1 && strings.HasPrefix(diff.Differences[0], "pragma journal_mode")
}

func recordCanonicalHeader(path string) {
	image := must(os.ReadFile(path))("read")
	tables.canonicalHeader.Row(len(image), image[18], image[19], be32(image[24:28]), be32(image[40:44]))
}

func recordCookies(part, subject, a, b string) {
	ia, ib := must(os.ReadFile(a))("read"), must(os.ReadFile(b))("read")
	tables.headerValues.Row(part, subject, "schema_cookie", be32(ia[40:44]), be32(ib[40:44]))
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func newDB(ctx context.Context, path string, statements ...string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	db := must(sqlitex.Open(ctx, path, options))("open")
	for _, s := range statements {
		execOrDie(ctx, db, s)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	atRest(path)
	return path
}

type pair struct {
	name   string
	a, b   []string
	ours   string
	dbhash bool
}

// partB exercises six logical-difference pairs where weaker tools fail,
// including dbhash's unframed value concatenation and missing table boundaries
// in tool/dbhash.c:281-360 and tool/dbhash.c:379-491.
func partB(ctx context.Context, dir, tool string) {
	pairs := []pair{
		{"implicit rowids (1,2 vs 5,9)", []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(1, 'x'), (2, 'y')"}, []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(5, 'x'), (9, 'y')"}, "table t differs", true},
		{"sqlite_sequence (1 vs 2)", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')", "INSERT INTO t(v) VALUES('y')", "DELETE FROM t WHERE id = 2"}, "table sqlite_sequence differs", true},
		{"user_version (1 vs 2)", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 1"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 2"}, "pragma user_version 1 vs 2", true},
		{"integer 1 vs real 1.0", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1)"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1.0)"}, "table t differs", false},
		{"value boundary ('a3','b' vs 'a','3b')", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a3', 'b')"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a', '3b')"}, "table t differs", true},
		{"row moved between tables", []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x'), (2, 'y')", "INSERT INTO b(rowid, v) VALUES(1, 'z')"}, []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x')", "INSERT INTO b(rowid, v) VALUES(1, 'y'), (2, 'z')"}, "table a differs", true},
	}
	type claim struct {
		ok   bool
		text string
	}
	var claims []claim
	for i, p := range pairs {
		a := newDB(ctx, filepath.Join(dir, "pairs", fmt.Sprintf("%d-a.db", i+1)), p.a...)
		b := newDB(ctx, filepath.Join(dir, "pairs", fmt.Sprintf("%d-b.db", i+1)), p.b...)
		result, _, _, err := equal.CompareLogicalFiles(ctx, a, b)
		if err != nil {
			fatal("compare", err)
		}
		var dbhashEqual any
		if tool != "" {
			ha, hb := dbhashOf(ctx, tool, a), dbhashOf(ctx, tool, b)
			dbhashEqual = ha == hb
			claims = append(claims, claim{(ha == hb) == p.dbhash, fmt.Sprintf("%s: dbhash says %s", p.name, sameWord(ha == hb))})
		}
		first := ""
		if len(result.Differences) > 0 {
			first = result.Differences[0]
		}
		tables.pairs.Row(i+1, p.name, result.Equal, dbhashEqual, orNil(first))
		claims = append(claims, claim{!result.Equal && strings.Contains(first, p.ours), fmt.Sprintf("%s: our logical hash tells the two apart", p.name)})
	}
	for _, c := range claims {
		set.Check("B", c.ok, c.text)
	}
}

func sameWord(equalValues bool) string {
	if equalValues {
		return "equal"
	}
	return "differ"
}

// partC tests the three boundaries of VACUUM INTO as a canonical form
// (vacuum.c:143-427): table creation order, prior schema-cookie increments,
// and index creation timing.
func partC(ctx context.Context, dir, tool string) {
	cases := []struct {
		name string
		a, b []string
	}{
		{"tables created in another order", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')", "INSERT INTO b VALUES(1, 'y')"},
			[]string{"CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')", "INSERT INTO b VALUES(1, 'y')"}},
		{"a table created and dropped in the history", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')"},
			[]string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE scratch(x)", "DROP TABLE scratch", "INSERT INTO a VALUES(1, 'x')"}},
		{"an index created before vs after the rows", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE INDEX a_v ON a(v)", "INSERT INTO a VALUES(1, 'x'), (2, 'y')"},
			[]string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x'), (2, 'y')", "CREATE INDEX a_v ON a(v)"}},
	}
	for i, c := range cases {
		a := newDB(ctx, filepath.Join(dir, "limits", fmt.Sprintf("%d-a.db", i+1)), c.a...)
		b := newDB(ctx, filepath.Join(dir, "limits", fmt.Sprintf("%d-b.db", i+1)), c.b...)
		e3, _, _, err := equal.CompareLogicalFiles(ctx, a, b)
		if err != nil {
			fatal("compare", err)
		}
		ca, cb := strings.TrimSuffix(a, ".db")+"-vacuum.db", strings.TrimSuffix(b, ".db")+"-vacuum.db"
		vacuumInto(ctx, a, ca)
		vacuumInto(ctx, b, cb)
		e1 := must(equal.CompareFiles(ca, cb, pageSize))("E1")
		e2 := must(equal.ComparePageFiles(ca, cb, pageSize))("E2")
		recordLogical("C", c.name, "sources", e3)
		recordFiles("C", c.name, "VACUUM INTO outputs", "E1", e1)
		recordFiles("C", c.name, "VACUUM INTO outputs", "E2", e2)
		recordCookies("C", c.name, ca, cb)
		var dbhashEqual, capturesEqual any
		if tool != "" {
			dbhashEqual = dbhashOf(ctx, tool, a) == dbhashOf(ctx, tool, b)
		}
		switch i {
		case 0:
			set.Check("C", e3.Equal && !e1.Equal && !e2.Equal, "same content, tables created in another order: logically equal, but VACUUM INTO copies tables in sqlite_schema order, so the canonical files differ beyond the header")
			scriptA, scriptB := dump(ctx, a), dump(ctx, b)
			rb := strings.TrimSuffix(b, ".db") + "-replayed.db"
			replay(ctx, scriptB, rb)
			crb := strings.TrimSuffix(rb, ".db") + "-vacuum.db"
			vacuumInto(ctx, rb, crb)
			replayE1 := must(equal.CompareFiles(cb, crb, pageSize))("E1")
			capturesEqual = scriptA == scriptB
			recordFiles("C", c.name, "replay of the second source's capture, canonicalized, vs the second source's canonical form", "E1", replayE1)
			set.Check("C", scriptA != scriptB && replayE1.Equal, "the capture writes CREATE TABLE in sqlite_schema order, so it records the creation order too: the replay of a source canonicalizes to the source's own canonical bytes")
		case 1:
			set.Check("C", e3.Equal && !e1.Equal && e2.Equal, "a schema change that left no trace in the content still raises the schema cookie, and VACUUM INTO carries the cookie (+1): the canonical files differ only at bytes 40-43")
		case 2:
			set.Check("C", e3.Equal && e1.Equal, "an index built before or after the rows canonicalizes to the same bytes: VACUUM INTO rebuilds every index")
		}
		tables.limits.Row(i+1, c.name, dbhashEqual, capturesEqual)
	}
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.3")
	set = results.MustOpen(dir)
	openTables()
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	tool := locateDBHash()
	c := partA(ctx, dir, tool)
	rollbackPair(ctx, dir, c)
	partB(ctx, dir, tool)
	partC(ctx, dir, tool)
	set.Finish("SAME ROWS")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/3.3-same-rows-different-bytes/report.sql -->
```sql
-- Walkthrough of experiment 3.3. Run it with:
--   go run ./cmd/course run 3.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   builds           part A: one row per build of the same rows, with its hashes
--   r0_tables        part A: rows per table of R0, the database of the recipe
--   r0_facts         part A: schema, pragmas and checks of R0, as the logical hash saw them
--   capture          part A: the ordered logical capture (rung R2) of R0, its size and hash
--   capture_lines    part A: the first nine and the last four lines of that capture
--   compare          one row per comparison of two files at one level (E1, E2, E3)
--   header_values    schema cookies and change counters of the two files compared
--   canonical_header part A: the header of the VACUUM INTO copy of R0
--   pairs            part B: one row per pair of databases that differ in content
--   limits           part C: dbhash and capture results for the three cases
--   checks           every claim the experiment asserted (the runner lists it last)

-- The words of one comparison, as a verdict.
CREATE VIEW verdicts AS
SELECT *,
       CASE WHEN level = 'E3' AND equal THEN 'equal'
            WHEN level = 'E3' THEN 'differ: ' || differences
            WHEN equal AND header_bytes_differing IS NOT NULL
                 THEN 'equal except header bytes [' || header_bytes_differing || ']'
            WHEN equal THEN 'equal'
            WHEN size_a <> size_b
                 THEN 'differ (' || differing_pages || ' pages differ, sizes differ)'
            ELSE 'differ (' || differing_pages || ' of ' || pages_total || ' pages)'
       END AS verdict
FROM compare;

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  3.3  The same rows, different bytes: which equality holds?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Three levels of equality between two database files:'
.print '  E1  the file sha256 is the same'
.print '  E2  the pages are the same, ignoring header bytes 24-27, 40-43 and 92-99'
.print '  E3  the logical hash is the same: schema, every row, the persistent pragmas,'
.print '      integrity_check ok and foreign_key_check unchanged'
.print 'dbhash is the SQLite tool, built from the pinned tree: it prints a SHA1 of'
.print 'the schema and every row it reads. It is skipped, and its column is empty,'
.print 'when the tool is not built.'


.print
.print '── A. The same rows built six ways ─────────────────────────────────────'
.print
.print 'A.1 R0 is the database of the recipe: seed 1, 30 steps of inserts, updates and'
.print 'deletes. Its content, as the logical hash saw it:'

SELECT table_name AS item, rows AS value FROM r0_tables
UNION ALL
SELECT name, value FROM r0_facts
ORDER BY item;

.print
.print 'A.2 The ordered logical capture of R0 (rung R2): a SQL script of the schema,'
.print 'the rows in a fixed order and the pragmas. Its size and hash:'

SELECT path, bytes, lines, sha256 FROM capture;

.print
.print 'The first nine and the last four lines of the capture:'
.mode list
.separator ' | '

SELECT line_no::VARCHAR AS line, text FROM capture_lines WHERE line_no <= 9
UNION ALL
SELECT '...', '(' || (SELECT lines - 13 FROM capture) || ' INSERT lines not shown)'
UNION ALL
SELECT line_no::VARCHAR, text FROM capture_lines WHERE line_no > 9
ORDER BY CASE WHEN line = '...' THEN 9.5 ELSE line::DOUBLE END;

.mode box

.print
.print 'A.3 The builds. All of them hold the same rows.'

SELECT seq, name, bytes, how FROM builds ORDER BY seq;

.print
.print 'A.4 Each build against R0 at E1, E2 and E3. The files differ in size because'
.print 'the builds lay the rows out differently. The backup is a page copy: it differs'
.print 'from R0 only in header bytes.'

SELECT subject AS build,
       max(verdict) FILTER (level = 'E1') AS e1,
       max(verdict) FILTER (level = 'E2') AS e2,
       max(verdict) FILTER (level = 'E3') AS e3
FROM verdicts
WHERE part = 'A' AND what = 'build vs r0'
GROUP BY subject
ORDER BY min(seq);

.print
.print 'A.5 The same builds by hash (first 16 hex digits of each). The file'
.print 'sha256 differs; the logical hash, the capture hash and dbhash agree for all of'
.print 'them. After VACUUM INTO, every build but the backup has one sha256.'

SELECT name,
       left(file_sha256, 16)      AS file_sha256,
       left(logical_sha256, 16)   AS logical_sha256,
       left(dump_sha256, 16)      AS dump_sha256,
       left(dbhash, 16)           AS dbhash,
       left(canonical_sha256, 16) AS vacuum_into_sha256
FROM builds ORDER BY seq;

.print
.print 'Distinct values among the builds:'

SELECT count(DISTINCT file_sha256) FILTER (name <> 'backup')       AS file_sha256_without_backup,
       count(DISTINCT logical_sha256)                              AS logical,
       count(DISTINCT dump_sha256)                                 AS dump,
       count(DISTINCT dbhash)                                      AS dbhash,
       count(DISTINCT canonical_sha256) FILTER (name <> 'backup')  AS vacuum_into_without_backup
FROM builds;

.print
.print 'A.6 The backup after VACUUM INTO, against R0 after VACUUM INTO: the files differ,'
.print 'but at E2 only in the schema cookie (byte 43).'

SELECT max(v.verdict) FILTER (v.level = 'E1') AS e1,
       max(v.verdict) FILTER (v.level = 'E2') AS e2,
       any_value(h.value_a)                    AS cookie_r0,
       any_value(h.value_b)                    AS cookie_backup
FROM verdicts v JOIN header_values h USING (part, subject)
WHERE v.part = 'A' AND v.subject = 'backup' AND v.what LIKE 'canonical%';

.print
.print 'A.7 The header of R0 after VACUUM INTO. Bytes 18-19 = 1,1 mean a rollback'
.print 'journal: VACUUM INTO writes a rollback-mode file, so its logical hash differs'
.print 'from the source only in journal_mode.'

SELECT bytes, byte_18, byte_19, change_counter, schema_cookie FROM canonical_header;

.print
.print '── A2. The same two batchings in rollback mode (journal_mode=DELETE) ───'
.print
.print 'A2.1 One transaction against one transaction per statement. The files differ at E1'
.print 'and at E2 only in header bytes: the change counter (bytes 24-27 and 92-95)'
.print 'counts commits.'

SELECT subject, what, max(verdict) FILTER (level = 'E1') AS e1, max(verdict) FILTER (level = 'E2') AS e2
FROM verdicts WHERE part = 'A2' GROUP BY subject, what;

SELECT field, value_a AS one_transaction, value_b AS one_per_statement
FROM header_values WHERE part = 'A2';

.print
.print '── B. Pairs that differ in content where a weaker hash says equal ──────'
.print
.print 'B.1 Six pairs of databases that differ in content. Our logical hash tells every'
.print 'pair apart; dbhash misses all but integer 1 against real 1.0. (sqldiff is not'
.print 'built by build-tools; the research probe found it reports integer 1 and real'
.print '1.0 as equal: docs/knowledge/determinism-and-rebuild.md, Q4.)'

SELECT pair,
       CASE WHEN ours_equal THEN 'equal' ELSE 'differ' END   AS ours,
       CASE WHEN dbhash_equal THEN 'equal' WHEN NOT dbhash_equal THEN 'differ' END AS dbhash,
       first_difference AS our_first_difference
FROM pairs ORDER BY seq;

.print
.print '── C. Where VACUUM INTO stops being a canonical form ───────────────────'
.print
.print 'C.1 Three cases of two databases with the same content. The sources are compared'
.print 'at E3, their VACUUM INTO outputs at E1 and E2.'

SELECT l.case_name,
       max(v.verdict) FILTER (v.what = 'sources')                                   AS sources_e3,
       max(v.verdict) FILTER (v.what = 'VACUUM INTO outputs' AND v.level = 'E1')     AS outputs_e1,
       max(v.verdict) FILTER (v.what = 'VACUUM INTO outputs' AND v.level = 'E2')     AS outputs_e2
FROM limits l JOIN verdicts v ON v.part = 'C' AND v.subject = l.case_name
GROUP BY l.case_name, l.seq ORDER BY l.seq;

.print
.print 'C.2 The schema cookie of each output, and dbhash of the sources.'

SELECT l.case_name, h.value_a AS cookie_a, h.value_b AS cookie_b,
       CASE WHEN l.dbhash_sources_equal THEN 'equal' WHEN NOT l.dbhash_sources_equal THEN 'differ' END AS dbhash_of_sources
FROM limits l JOIN header_values h ON h.part = 'C' AND h.subject = l.case_name
ORDER BY l.seq;
.print
.print 'C.3 For the first case, the capture records the creation order too: the two'
.print 'captures of the two sources differ, and replaying the second capture and'
.print 'canonicalizing it gives the canonical bytes of the second source.'

SELECT l.case_name,
       CASE WHEN l.captures_equal THEN 'equal' ELSE 'differ' END AS captures_of_the_two_sources,
       v.verdict AS replay_vs_canonical_form_e1
FROM limits l JOIN verdicts v ON v.part = 'C' AND v.subject = l.case_name AND v.what LIKE 'replay%'
ORDER BY l.seq;
```

Run from the repository root (this starts the `lab` container, runs the experiment in it, and prints the walkthrough):

```text
make run EXP=3.3
```

Add `-i` (`make run EXP=3.3 ARGS='-i'`) to stay in the DuckDB shell afterwards with the tables loaded, for your own queries.

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/3.3-same-rows-different-bytes/`](evidence/3.3-same-rows-different-bytes/), and `make check ARGS='evidence ch03'` compares a fresh run with them byte for byte.

<!-- include: evidence/3.3-same-rows-different-bytes.txt -->
```text

════════════════════════════════════════════════════════════════════════
  3.3  The same rows, different bytes: which equality holds?
════════════════════════════════════════════════════════════════════════

Three levels of equality between two database files:
  E1  the file sha256 is the same
  E2  the pages are the same, ignoring header bytes 24-27, 40-43 and 92-99
  E3  the logical hash is the same: schema, every row, the persistent pragmas,
      integrity_check ok and foreign_key_check unchanged
dbhash is the SQLite tool, built from the pinned tree: it prints a SHA1 of
the schema and every row it reads. It is skipped, and its column is empty,
when the tool is not built.

── A. The same rows built six ways ─────────────────────────────────────

A.1 R0 is the database of the recipe: seed 1, 30 steps of inserts, updates and
deletes. Its content, as the logical hash saw it:
┌────────────────────────┬────────────┐
│          item          │   value    │
├────────────────────────┼────────────┤
│ accounts               │ 183        │
│ application_id         │ 1397836120 │
│ auto_vacuum            │ none       │
│ default_cache_size     │ -2000      │
│ documents              │ 96         │
│ encoding               │ UTF-8      │
│ events                 │ 1177       │
│ foreign_key_check_rows │ 0          │
│ integrity_check        │ ok         │
│ journal_mode           │ wal        │
│ page_size              │ 4096       │
│ schema_rows            │ 4          │
│ user_version           │ 30         │
└────────────────────────┴────────────┘

A.2 The ordered logical capture of R0 (rung R2): a SQL script of the schema,
the rows in a fixed order and the pragmas. Its size and hash:
┌────────────────┬─────────┬───────┬──────────────────────────────────────────────────────────────────┐
│      path      │  bytes  │ lines │                              sha256                              │
├────────────────┼─────────┼───────┼──────────────────────────────────────────────────────────────────┤
│ capture/r0.sql │ 2005315 │ 1468  │ 92d3898538e25714f5943079b3b5046b31b4f637430abbc1aa5d642a2d95c687 │
└────────────────┴─────────┴───────┴──────────────────────────────────────────────────────────────────┘

The first nine and the last four lines of the capture:
line | text
1 | PRAGMA page_size=4096;
2 | PRAGMA auto_vacuum=NONE;
3 | PRAGMA encoding='UTF-8';
4 | PRAGMA journal_mode=WAL;
5 | BEGIN;
6 | CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT NOT NULL, balance INTEGER NOT NULL);
7 | CREATE TABLE documents(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, title TEXT NOT NULL, body BLOB NOT NULL);
8 | CREATE TABLE events(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, kind TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT NOT NULL);
9 | INSERT INTO "accounts"(rowid, "id", "name", "balance") VALUES(1, 1, 'wxhyvco', 37188);
... | (1455 INSERT lines not shown)
1465 | CREATE INDEX events_by_account ON events(account_id, id);
1466 | PRAGMA user_version=30;
1467 | PRAGMA application_id=1397836120;
1468 | COMMIT;

A.3 The builds. All of them hold the same rows.
┌─────┬─────────────────┬─────────┬───────────────────────────────────────────────────────────────────────────────┐
│ seq │      name       │  bytes  │                                      how                                      │
├─────┼─────────────────┼─────────┼───────────────────────────────────────────────────────────────────────────────┤
│ 1   │ r0              │ 1105920 │ the recipe: 31 transactions with updates and deletes, index created at step 5 │
│ 2   │ one-transaction │ 1089536 │ schema and every row in one transaction, tables in order, ids ascending       │
│ 3   │ per-statement   │ 1089536 │ the same order, one transaction per statement                                 │
│ 4   │ shuffled        │ 1093632 │ one transaction per row, rows of all tables in a shuffled order (PCG seed 3)  │
│ 5   │ replayed        │ 1089536 │ the capture file replayed into an empty file (indexes created after the rows) │
│ 6   │ backup          │ 1105920 │ sqlite3_backup of R0 (a page copy, for the E2 row)                            │
└─────┴─────────────────┴─────────┴───────────────────────────────────────────────────────────────────────────────┘

A.4 Each build against R0 at E1, E2 and E3. The files differ in size because
the builds lay the rows out differently. The backup is a page copy: it differs
from R0 only in header bytes.
┌─────────────────┬─────────────────────────────────────────┬─────────────────────────────────────────┬───────┐
│      build      │                   e1                    │                   e2                    │  e3   │
├─────────────────┼─────────────────────────────────────────┼─────────────────────────────────────────┼───────┤
│ r0              │ equal                                   │ equal                                   │ equal │
│ one-transaction │ differ (268 pages differ, sizes differ) │ differ (268 pages differ, sizes differ) │ equal │
│ per-statement   │ differ (268 pages differ, sizes differ) │ differ (268 pages differ, sizes differ) │ equal │
│ shuffled        │ differ (270 pages differ, sizes differ) │ differ (270 pages differ, sizes differ) │ equal │
│ replayed        │ differ (262 pages differ, sizes differ) │ differ (262 pages differ, sizes differ) │ equal │
│ backup          │ differ (1 of 270 pages)                 │ equal except header bytes [27 43 95]    │ equal │
└─────────────────┴─────────────────────────────────────────┴─────────────────────────────────────────┴───────┘

A.5 The same builds by hash (first 16 hex digits of each). The file
sha256 differs; the logical hash, the capture hash and dbhash agree for all of
them. After VACUUM INTO, every build but the backup has one sha256.
┌─────────────────┬──────────────────┬──────────────────┬──────────────────┬──────────────────┬────────────────────┐
│      name       │   file_sha256    │  logical_sha256  │   dump_sha256    │      dbhash      │ vacuum_into_sha256 │
├─────────────────┼──────────────────┼──────────────────┼──────────────────┼──────────────────┼────────────────────┤
│ r0              │ 24aa158fdf713808 │ 3de6a8870f70a57c │ 92d3898538e25714 │ 9bee16896cb395b9 │ 927e39a45becbb84   │
│ one-transaction │ 224810279caf4016 │ 3de6a8870f70a57c │ 92d3898538e25714 │ 9bee16896cb395b9 │ 927e39a45becbb84   │
│ per-statement   │ 224810279caf4016 │ 3de6a8870f70a57c │ 92d3898538e25714 │ 9bee16896cb395b9 │ 927e39a45becbb84   │
│ shuffled        │ dc2b3bbb223253be │ 3de6a8870f70a57c │ 92d3898538e25714 │ 9bee16896cb395b9 │ 927e39a45becbb84   │
│ replayed        │ e3c97131367e136d │ 3de6a8870f70a57c │ 92d3898538e25714 │ 9bee16896cb395b9 │ 927e39a45becbb84   │
│ backup          │ a268f05a569a3c07 │ 3de6a8870f70a57c │ 92d3898538e25714 │ 9bee16896cb395b9 │ 9324d9446d407e55   │
└─────────────────┴──────────────────┴──────────────────┴──────────────────┴──────────────────┴────────────────────┘

Distinct values among the builds:
┌────────────────────────────┬─────────┬──────┬────────┬────────────────────────────┐
│ file_sha256_without_backup │ logical │ dump │ dbhash │ vacuum_into_without_backup │
├────────────────────────────┼─────────┼──────┼────────┼────────────────────────────┤
│ 4                          │ 1       │ 1    │ 1      │ 1                          │
└────────────────────────────┴─────────┴──────┴────────┴────────────────────────────┘

A.6 The backup after VACUUM INTO, against R0 after VACUUM INTO: the files differ,
but at E2 only in the schema cookie (byte 43).
┌─────────────────────────┬────────────────────────────────┬───────────┬───────────────┐
│           e1            │               e2               │ cookie_r0 │ cookie_backup │
├─────────────────────────┼────────────────────────────────┼───────────┼───────────────┤
│ differ (1 of 266 pages) │ equal except header bytes [43] │ 5         │ 2             │
└─────────────────────────┴────────────────────────────────┴───────────┴───────────────┘

A.7 The header of R0 after VACUUM INTO. Bytes 18-19 = 1,1 mean a rollback
journal: VACUUM INTO writes a rollback-mode file, so its logical hash differs
from the source only in journal_mode.
┌─────────┬─────────┬─────────┬────────────────┬───────────────┐
│  bytes  │ byte_18 │ byte_19 │ change_counter │ schema_cookie │
├─────────┼─────────┼─────────┼────────────────┼───────────────┤
│ 1089536 │ 1       │ 1       │ 1              │ 5             │
└─────────┴─────────┴─────────┴────────────────┴───────────────┘

── A2. The same two batchings in rollback mode (journal_mode=DELETE) ───

A2.1 One transaction against one transaction per statement. The files differ at E1
and at E2 only in header bytes: the change counter (bytes 24-27 and 92-95)
counts commits.
┌──────────────────────────┬───────────────────────────┬─────────────────────────┬─────────────────────────────────────────┐
│         subject          │           what            │           e1            │                   e2                    │
├──────────────────────────┼───────────────────────────┼─────────────────────────┼─────────────────────────────────────────┤
│ rollback-one-transaction │ vs rollback-per-statement │ differ (1 of 266 pages) │ equal except header bytes [26 27 94 95] │
└──────────────────────────┴───────────────────────────┴─────────────────────────┴─────────────────────────────────────────┘
┌────────────────┬─────────────────┬───────────────────┐
│     field      │ one_transaction │ one_per_statement │
├────────────────┼─────────────────┼───────────────────┤
│ change_counter │ 3               │ 1463              │
└────────────────┴─────────────────┴───────────────────┘

── B. Pairs that differ in content where a weaker hash says equal ──────

B.1 Six pairs of databases that differ in content. Our logical hash tells every
pair apart; dbhash misses all but integer 1 against real 1.0. (sqldiff is not
built by build-tools; the research probe found it reports integer 1 and real
1.0 as equal: docs/knowledge/determinism-and-rebuild.md, Q4.)
┌───────────────────────────────────────┬────────┬────────┬─────────────────────────────────────────────┐
│                 pair                  │  ours  │ dbhash │            our_first_difference             │
├───────────────────────────────────────┼────────┼────────┼─────────────────────────────────────────────┤
│ implicit rowids (1,2 vs 5,9)          │ differ │ equal  │ table t differs (2 vs 2 rows)               │
│ sqlite_sequence (1 vs 2)              │ differ │ equal  │ table sqlite_sequence differs (1 vs 1 rows) │
│ user_version (1 vs 2)                 │ differ │ equal  │ pragma user_version 1 vs 2                  │
│ integer 1 vs real 1.0                 │ differ │ differ │ table t differs (1 vs 1 rows)               │
│ value boundary ('a3','b' vs 'a','3b') │ differ │ equal  │ table t differs (1 vs 1 rows)               │
│ row moved between tables              │ differ │ equal  │ table a differs (2 vs 1 rows)               │
└───────────────────────────────────────┴────────┴────────┴─────────────────────────────────────────────┘

── C. Where VACUUM INTO stops being a canonical form ───────────────────

C.1 Three cases of two databases with the same content. The sources are compared
at E3, their VACUUM INTO outputs at E1 and E2.
┌────────────────────────────────────────────┬────────────┬───────────────────────┬────────────────────────────────┐
│                 case_name                  │ sources_e3 │      outputs_e1       │           outputs_e2           │
├────────────────────────────────────────────┼────────────┼───────────────────────┼────────────────────────────────┤
│ tables created in another order            │ equal      │ differ (3 of 3 pages) │ differ (3 of 3 pages)          │
│ a table created and dropped in the history │ equal      │ differ (1 of 2 pages) │ equal except header bytes [43] │
│ an index created before vs after the rows  │ equal      │ equal                 │ equal                          │
└────────────────────────────────────────────┴────────────┴───────────────────────┴────────────────────────────────┘

C.2 The schema cookie of each output, and dbhash of the sources.
┌────────────────────────────────────────────┬──────────┬──────────┬───────────────────┐
│                 case_name                  │ cookie_a │ cookie_b │ dbhash_of_sources │
├────────────────────────────────────────────┼──────────┼──────────┼───────────────────┤
│ tables created in another order            │ 3        │ 3        │ equal             │
│ a table created and dropped in the history │ 2        │ 4        │ equal             │
│ an index created before vs after the rows  │ 3        │ 3        │ equal             │
└────────────────────────────────────────────┴──────────┴──────────┴───────────────────┘

C.3 For the first case, the capture records the creation order too: the two
captures of the two sources differ, and replaying the second capture and
canonicalizing it gives the canonical bytes of the second source.
┌─────────────────────────────────┬─────────────────────────────┬─────────────────────────────┐
│            case_name            │ captures_of_the_two_sources │ replay_vs_canonical_form_e1 │
├─────────────────────────────────┼─────────────────────────────┼─────────────────────────────┤
│ tables created in another order │ differ                      │ equal                       │
└─────────────────────────────────┴─────────────────────────────┴─────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the backup copy differs from R0 in bytes (E2 pages: equal ignoring header bytes 24-27, 40-43, 92-99 (bytes differ only at [27 43 95])) but is equal at E2: only declared header bytes differ
A  ✓  in WAL mode one transaction and one transaction per statement give byte-identical files: commit boundaries alone leave no trace when rows are only appended
A  ✓  insert order (shuffled) and history (R0's updates and deletes) each give a different file
A  ✓  every build has the same logical hash
A  ✓  every build is equal to R0 at E3 (logical hash, integrity_check ok, foreign_key_check unchanged)
A  ✓  the ordered logical capture is itself identical for every build: it depends on content and the schema's creation order, not on insert order, batching or updates
A  ✓  after VACUUM INTO every build, the replayed copy included, has the same sha256 (E1 between canonical forms)
A  ✓  except the backup copy: the backup API sets the copy's schema cookie to (old destination cookie + 1) = 1 (backup.c:448), VACUUM INTO carries the cookie + 1, so its canonical form differs at byte 43 only
A  ✓  dbhash from the pinned tree also calls every build equal: it agrees with our logical hash on equality
A  ✓  VACUUM INTO keeps every row, the schema and user_version/application_id, but writes a rollback-mode file: its logical hash differs from the source only in journal_mode
A2  ✓  in rollback mode the batching shows, but only in the change counter (bytes 24-27 and 92-95 count commits): E2 equal
B  ✓  implicit rowids (1,2 vs 5,9): dbhash says equal
B  ✓  implicit rowids (1,2 vs 5,9): our logical hash tells the two apart
B  ✓  sqlite_sequence (1 vs 2): dbhash says equal
B  ✓  sqlite_sequence (1 vs 2): our logical hash tells the two apart
B  ✓  user_version (1 vs 2): dbhash says equal
B  ✓  user_version (1 vs 2): our logical hash tells the two apart
B  ✓  integer 1 vs real 1.0: dbhash says differ
B  ✓  integer 1 vs real 1.0: our logical hash tells the two apart
B  ✓  value boundary ('a3','b' vs 'a','3b'): dbhash says equal
B  ✓  value boundary ('a3','b' vs 'a','3b'): our logical hash tells the two apart
B  ✓  row moved between tables: dbhash says equal
B  ✓  row moved between tables: our logical hash tells the two apart
C  ✓  same content, tables created in another order: logically equal, but VACUUM INTO copies tables in sqlite_schema order, so the canonical files differ beyond the header
C  ✓  the capture writes CREATE TABLE in sqlite_schema order, so it records the creation order too: the replay of a source canonicalizes to the source's own canonical bytes
C  ✓  a schema change that left no trace in the content still raises the schema cookie, and VACUUM INTO carries the cookie (+1): the canonical files differ only at bytes 40-43
C  ✓  an index built before or after the rows canonicalizes to the same bytes: VACUUM INTO rebuilds every index

27 of 27 claims hold

Experiment verdict: SAME ROWS OK
```

**Reading the evidence:**

- **Part A — R0, five rebuilds, and rung R2 (A.1–A.7).** The deterministic 30-step R0 database (`internal/recipe`, seed 1) has `4` schema statements, `accounts=183`, `documents=96`, `events=1177`, and `user_version=30`. Its rung R2 logical capture `capture/r0.sql` is `2,005,315` bytes (`1,468` lines, sha256 `92d38985…`, about 1.8× R0's `1,105,920`-byte size because blobs are hex-encoded). Comparing all six builds against R0:
  - **WAL append-only batching:** `one-transaction` and `per-statement` both produce `1,089,536` bytes with the **exact same SHA-256** (`224810279caf4016`). Commit boundaries alone leave no trace in a WAL-mode file when rows are only appended, because WAL mode writes page 1's change counter only when page 1 is already dirty. Among the five non-backup builds (`r0`, `one-transaction`, `per-statement`, `shuffled`, `replayed`), there are **`4` distinct file SHA-256s**: history (R0's updates/deletes, `24aa158f…`), append order (`22481027…`), shuffled order (`dc2b3bbb…`, `1,093,632` bytes), and post-load index creation (`e3c97131…`, `1,089,536` bytes).
  - **Level E2 (`backup`):** `sqlite3_backup` of R0 (`a268f05a…`) differs from R0 on `1 of 270 pages` at E1, and passes **E2** (`equal except header bytes [27 43 95]`): only the change counter, schema cookie, and version-valid-for differ.
  - **Level E3 and R2 capture:** All six builds share the exact same logical SHA-256 (`3de6a8870f70a57c…`), the exact same `capture/r0.sql` SHA-256 (`92d3898538e25714…`), and the exact same `dbhash` SHA-1 (`9bee16896cb395b91df1ef4de2cbf4095ab44d13`).
  - **`VACUUM INTO` canonicalization and the backup exception:** All five non-backup builds—including `replayed`—canonicalize to one SHA-256 (`927e39a45becbb84`, `1,089,536` bytes, rollback header bytes 18–19 set to `1,1`, change counter `1`, schema cookie `5`). The backup copy canonicalizes to `9324d9446d407e55`, differing from R0's canonical form only at byte `43` (`E2` equal): `sqlite3_backup_step` set the backup's schema cookie to `0 + 1 = 1` (`backup.c:448`), and `VACUUM INTO` carried each source's cookie `+1` (`4 -> 5` vs `1 -> 2`). Every canonical file differs from its WAL source at E3 only in `pragma journal_mode` (`wal` vs `rollback`).
- **Part A2 — rollback-mode batching (A2.1).** Repeating `one-transaction` vs `per-statement` in `journal_mode=DELETE` produces two files that differ at E1 (`1 of 266 pages`) and match at E2 (`equal except header bytes [26 27 94 95]`), with change counters `3` vs `1463` counting the commits.
- **Part B — six blind spots of weaker logical hashes (B.1).** Our E3 logical hash distinguishes all six pairs, naming the exact table or pragma that differs, while `dbhash` calls **five of the six** `equal`:
  1. `implicit rowids (1,2 vs 5,9)` — `dbhash` blind (`SELECT *` omits hidden rowid).
  2. `sqlite_sequence (1 vs 2)` — `dbhash` blind (skips `sqlite_*` tables).
  3. `user_version (1 vs 2)` — `dbhash` blind (hashes no file-header pragmas).
  4. `integer 1 vs real 1.0` — `dbhash` distinguishes it, whereas `sqldiff` reports them equal (recorded in this repository's design note `docs/knowledge/determinism-and-rebuild.md`, Q4; `sqldiff` is not built by `cmd/course build-tools`).
  5. `value boundary ('a3','b' vs 'a','3b')` — `dbhash` blind (`hash_one_query` in `tool/dbhash.c:281-360` hashes `"3a33b"` for both because TEXT has no length prefix).
  6. `row moved between tables` — `dbhash` blind (no row or table delimiter between tables `a` and `b`).
- **Part C — where `VACUUM INTO` stops being a canonical form (C.1–C.3).**
  1. **Tables created in another order:** `CREATE TABLE a` then `b` vs `b` then `a` are E3-equal (and `dbhash`-equal), but `VACUUM INTO` copies tables in `sqlite_schema` rowid order, so the canonical files differ at E1 **and** E2 (`3 of 3 pages`, both schema cookie `3`). Because our R2 `dump()` writes `CREATE TABLE` in `sqlite_schema` rowid order, the two sources' captures differ, and replaying the second source's capture and running `VACUUM INTO` matches the second source's canonical form at E1 (`equal`).
  2. **A dropped scratch table in history:** Creating and dropping `scratch` leaves no logical trace (E3-equal, `dbhash`-equal), yet raises the schema cookie, so `VACUUM INTO` outputs differ at byte `43` (schema cookies `2` and `4`, E1 differ, E2 equal).
  3. **Index created before vs after rows:** E3-equal and canonical E1-equal (schema cookies `3` and `3`), because `VACUUM INTO` always rebuilds indexes from scratch.

The flow below maps how rung R2 capture, replay, and `VACUUM INTO` canonicalization produce E1, E2, and E3 results (both the source builds and `replayed .db` pass through `VACUUM INTO`):

<!-- draw-visual: diagrams/ch03-btrees-and-page-management-r2-canonical.mmd -->
```text
┌────────────────┐
│ source builds  │
└────────┬───────┘
         │
         ├────────────────────┐
         │                    │
       dump              WalkLogical
         │                    │
         ▼                    ▼
┌────────────────┐    ┌───────────────┐
│ capture/r0.sql │    │E3 logical hash│
└────────┬───────┘    └───────────────┘
         │
      replay
         ▼
┌────────────────┐
│  replayed .db  │
└────────┬───────┘
         │
    VACUUM INTO
         ▼
┌────────────────┐
│ canonical .db  │
└────────┬───────┘
         │
         ├────────────────────┬─────────────────────┐
         │                    │                     │
    same schema          cookie diff           diff order
         │                    │                     │
         ▼                    ▼                     ▼
┌────────────────┐    ┌───────────────┐    ┌────────────────┐
│E1 SHA-256 match│    │E2 only byte 43│    │E1 and E2 differ│
└────────────────┘    └───────────────┘    └────────────────┘
```

### 6.2 Summary of outline corrections

Ten subtle SQLite behaviors and edge-case boundaries measured in this chapter's experiments and stress tests qualify the simplified mental model (parenthetical codes such as `EQ-03`, `SPL-01`, and `DEL-02` name stress tests listed under "What attacked this chapter" below):

1. **WAL append-only batching** (`3.3` Part A/A2): one transaction vs per-statement produces identical bytes in WAL mode; commit boundaries appear only in rollback mode's change counter (E2-equal).
2. **`VACUUM INTO` canonical limits** (`3.3` Part A/C, `EQ-03`, `R2-02`): canonical E1 equality requires the same table creation order, the same schema cookie (`sqlite3_backup` copies reset it to `1`), and the same `default_cache_size` (included in E3).
3. **`dbhash` extra blind spots** (`3.3` Part B): `dbhash` also misses unframed value boundaries (`'a3','b'` vs `'a','3b'`) and rows moved between tables.
4. **WAL frames `>=` changed pages** (`3.1` Part B): `balance_nonroot` can rewrite a sibling leaf with identical bytes, shipping 6 WAL frames for 5 byte-changed pages.
5. **First split = first depth change** (`3.1` Part A): on an empty table, insert `#5` runs both `balance_deeper` and `balance_quick`.
6. **Source spans** (`SPANS.txt`, this chapter's cited C line-range manifest): `balance_nonroot`'s header comment is `btree.c:8204-8244` before the function at `8245-9027`; `btreeInt.h:149-228` covers the comment (`149-215`) plus `MX_CELL_SIZE`/`MX_CELL` (`222-229`).
7. **Scope of "most inserts change one page"** (`3.1` Part E, `SPL-01`/`SPL-03`/`SPL-05`): breaks for random keys in few-row leaves (`86/250`), overflowing rows (`0/25`), and `WITHOUT ROWID` append splits (`5` pages at depth 2 via `balance_nonroot`).
8. **Scope of "file never shrinks on its own"** (`3.2`, `DEL-02`): holds only for `auto_vacuum=NONE`; `FULL` shrinks at the `DELETE` commit and `INCREMENTAL` on `PRAGMA incremental_vacuum`.
9. **Scope of `secure_delete=ON`** (`3.2`, `DEL-05`): scrubs the database file at rest (`0/40`), but `-wal` retains prior `INSERT` frames (`40/40` even after a `PASSIVE` checkpoint) until reset by `TRUNCATE`.
10. **Per-allocation freelist reuse** (`3.2`, `DEL-06`): one large transaction can exhaust the freelist and grow `page_count` in the same commit (`freelist 2 -> 0`, `page_count 12 -> 18`).

### What attacked this chapter

<!-- adversarial-results -->

Four standalone Go stress-test programs in this repository (`30` attacks total, driven by [`run-all.sh`](../agent_work/ch03/adversarial/run-all.sh)) tried to break this chapter's claims, `internal/equal`, and Experiments 3.1–3.3 against the pinned SQLite `3.53.4` build (`HELD` means the code or claim stood up to the test; `BROKEN` means the test exposed a bug or an overly broad claim):

- [`splits-stress`](../agent_work/ch03/adversarial/splits-stress/main.go) (`SPL-01`–`SPL-07b`, 8 attacks) tested random insert orders across 3 fixed PCG seeds (`11`, `42`, `99`) with 1,000-byte and 40-byte blobs (`SPL-01`); verified exact cell-size split arithmetic across `50B`, `400B`, `1900B`, and the `1013B` rowid-varint boundary where leaf capacity shifts from 4 to 3 at rowid `128` (`SPL-02`); tested overflowing `5000B` and `10000B` rows (`SPL-03`); scaled secondary indexes across `0, 1, 2, 4` indexes (`SPL-04`, mode `1+k`, constant roots); tested `WITHOUT ROWID` ascending appends (`SPL-05`); verified same-size in-place `UPDATE` (`1` page) vs growing `UPDATE` (`5` pages, `6` WAL frames) (`SPL-06`); and tested `equal.ChangedPages` on leaf-aligned and mid-leaf `DELETE`s under `secure_delete=OFF` (`SPL-07a`, `SPL-07b`).
- [`delete-leaks`](../agent_work/ch03/adversarial/delete-leaks/main.go) (`DEL-01`–`DEL-06`, 6 attacks) checked deleted tenant markers across source, plain file copy, `sqlite3_backup`, `sqlite3_serialize`, and `VACUUM INTO` under `OFF` (`95, 95, 95, 95, 0`) vs `ON` (`0` in all five) (`DEL-01`); tested `auto_vacuum=NONE`, `FULL`, and `INCREMENTAL` (`DEL-02`); tested `secure_delete` `OFF` vs `ON` across overflow deletes, index deletes, shrinking updates, `DROP TABLE`, and unqualified `DELETE FROM t` (`DEL-03`); tested `PRAGMA secure_delete=FAST` (`mode=2`, scrubbing live-page updates `0/1` while leaving `38/80` markers on freelist leaves, `DEL-04`); tested `-wal` residue with `secure_delete=ON` after `DELETE`, `PASSIVE`, and `TRUNCATE` (`DEL-05`); and tested a single 32-row transaction exhausting a 2-page freelist and growing `page_count` from `12` to `18` (`DEL-06`).
- [`equal-falsify`](../agent_work/ch03/adversarial/equal-falsify/main.go) (`EQ-01`–`EQ-09`, 9 attacks) tested 13 logical-difference pairs against `LogicalHash` (`0/13` missed) and `dbhash` (`10/13` blind) (`EQ-01`); tested 13 distinct values (including `DATETIME`, `±Inf`, `INT64_MIN/MAX`, embedded NUL), FTS5 shadow tables, and `WITHOUT ROWID` foreign-key orphans (`EQ-02`); tested `PRAGMA default_cache_size` vs `VACUUM INTO` (`EQ-03`); tested `CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)` (`EQ-04`); tested `LogicalHashFile` and `RunDBHash` on an uncheckpointed WAL snapshot (`.db` + `-wal`) (`EQ-05`); tested E2 edge cases (`EQ-06`); tested `RunDBHash` on paths with spaces, missing files, and corrupt files (`EQ-07`); tested `TEMP` table shadowing and `EncodeValue` on unsupported Go types (`EQ-08`); and tested mid-walk context cancellation on `LogicalHash` (`EQ-09`).
- [`r2-canonical-and-pin`](../agent_work/ch03/adversarial/r2-canonical-and-pin/main.go) (`R2-01`–`R2-06`, 7 attacks) tested WAL one-tx vs per-statement E1 equality under secondary indexes, `cache_size=4` spills, and mixed `INSERT`+`DELETE`+re-`INSERT` (`R2-01`); tested R2 dump/replay/`VACUUM INTO` on tables created out of alphabetical order (`zebra` before `alpha`, `R2-02`); tested R2 dump/replay on `AUTOINCREMENT`/`sqlite_sequence`, `WITHOUT ROWID`, `DATETIME`, `±Inf`, quotes/newlines, empty blobs, embedded NULs (`char(0)`), and `?`-containing identifiers (`R2-03a`, `R2-03b`); verified `VACUUM INTO` header preservation and existing-target refusal (`R2-04`); verified pin-mismatch failure on all three experiments in a scratch repo (`R2-05`); and verified Experiment 3.3's `NO-DBHASH` graceful fallback (`R2-06`).

**Run 1** ([`results-run1.txt`](../agent_work/ch03/adversarial/results-run1.txt)) produced **16 HELD, 14 BROKEN**.

**Code defects exposed and fixed:**

- `SPL-07b`: `equal.ChangedPages` previously reported pages whose census role changed with identical bytes (`17` pages reported vs `5` byte changes and `5` WAL frames on a `secure_delete=OFF` `DELETE`); fixed to report only SHA-256 changes and added/removed pages (`internal/equal/changes.go`).
- `EQ-03`: `PRAGMA default_cache_size` (header bytes `48-51`, copied by `VACUUM INTO`) was missing from `LogicalPragmas`, allowing E3-equal databases with identical schema history to canonicalize differently; added as the seventh pragma in `LogicalPragmas`.
- `EQ-04`: `rowidAlias` rejected `CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)`; fixed with `integerPrimaryKeyColumn` fallback (accepting exact `INTEGER PRIMARY KEY` without a `pk`-origin autoindex, while still rejecting `DESC`, `INT`, and composite keys).
- `EQ-05`: `LogicalHashFile` and `RunDBHash` opened uncheckpointed WAL snapshots read-write and checkpointed/deleted `-wal` on close; fixed with `HasLiveSideFile` opening live snapshots `mode=ro` (leaving `.db` and `-wal` byte-identical).
- `EQ-08`: `WalkLogical` lacked `main.` qualification (allowing `TEMP` tables to shadow `main`), and `EncodeValue` returned empty bytes for unsupported Go types; fixed by qualifying every query on `main` and encoding unsupported types with tag `'U'` + type + value.
- `EQ-09`: canceling `ctx` mid-walk caused `LogicalHash`'s cleanup `COMMIT` to fail with `ctx.Err()`, leaking an open read transaction on `conn`; fixed by ending the transaction with `context.WithoutCancel(ctx)` (`ROLLBACK` on error, `COMMIT` on success).
- `R2-02` and `R2-03b`: Experiment 3.3's `dump()` previously sorted `CREATE TABLE` alphabetically (breaking canonical E1 match when tables were created out of name order), emitted raw NUL bytes inside SQL string literals, and used `strings.Replace` on `?` (corrupting identifiers containing `?`); fixed to emit schema statements in `main.sqlite_schema` rowid order, split NUL bytes into `'a'||char(0)||'b'`, and build `INSERT` statements directly with `insertStatement`.

**Claims scoped to match measured SQLite behavior:**

- `SPL-01`, `SPL-03`, `SPL-05`: "most inserts change one page" and "depth-2 append splits change 3 pages" are scoped in Experiment 3.1 Part E (random keys with 4 rows/leaf: `86/250`; overflowing rows: `0/25`; `WITHOUT ROWID` depth-2 append splits: `5` pages via `balance_nonroot`).
- `DEL-02`: "the file never shrinks on its own" is scoped to `auto_vacuum=NONE` (Experiment 3.2 `auto_vacuum` section).
- `DEL-05`: `secure_delete=ON` scrubs the database file at rest, not prior `INSERT` frames sitting in `-wal` until `TRUNCATE` resets the WAL (Experiment 3.2 `-wal` section).
- `DEL-06`: freelist reuse is per page allocation, not per transaction (Experiment 3.2 step 4 and large-transaction section).

**Run 2** ([`results-run2.txt`](../agent_work/ch03/adversarial/results-run2.txt), after the fixes with attacks unedited) moved to **22 HELD, 8 BROKEN** (the 6 scoped claims plus `R2-02` and `R2-03b`, whose script kept a frozen pre-fix copy of `dump()`). After refreshing `r2-canonical-and-pin` to mirror the updated `dump()` and aligning the 6 scoped expectations, **Run 3** ([`results-run3.txt`](../agent_work/ch03/adversarial/results-run3.txt) and [`results.txt`](../agent_work/ch03/adversarial/results.txt)) passed **30 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every operational rule below follows directly from this chapter's experiments and stress tests:

1. **WAL shipping bandwidth is driven by dirtied pages, not row size (Experiment 3.1 Parts A, B, C, and E; `SPL-01`–`SPL-06`).** Every modified page ships as a `4,120`-byte WAL frame. `2,061` single-row appends of 1 KB rows shipped `3,094` frames (`12.7 MB`) for a `520`-page (`2.1 MB`) table—roughly `6×` write amplification. Adding `k` secondary indexes multiplies the common per-insert frame count to `1 + k` (`3` frames for 2 indexes in Part C; `1 + k` through `k = 4` in `SPL-04`). Random-key inserts into few-row leaves (`86/250` one-page in Part E), overflowing rows (`3..5` pages every insert), and `WITHOUT ROWID` splits (`5` pages at depth 2) amplify frame counts further.
2. **WAL frames shipped can exceed byte-changed pages (Experiment 3.1 Part B).** `balance_nonroot` wrote page `3` as a 6th WAL frame even though its bytes did not change (`c0995adc833eadb1` before and after). A WAL shipper must expect redundant page frames, whereas a page-diff replicator (such as SQLite's `sqlite3_rsync`, which compares page digests and transfers only changed pages) can skip pages whose hash did not change.
3. **Rollback mode adds page 1 to every commit (Experiment 3.1 Part D).** Under `journal_mode=DELETE`, every commit rewrites page 1 (bytes `24-27` and `92-95`), doubling the database-file page writes on an ordinary insert before even counting the rollback journal.
4. **Databases do not shrink on delete under default `auto_vacuum=NONE` (Experiment 3.2, `DEL-02`).** Deleting `204` of `400` rows left the file at `417,792` bytes with `50` freelist pages; later inserts reused those pages before growing the file, and only `VACUUM` shrank it (`107 -> 51` pages). Choose `auto_vacuum=FULL` (shrinks on `DELETE`, `53 -> 16` pages) or `INCREMENTAL` (shrinks on `PRAGMA incremental_vacuum`) at database creation time if automatic reclamation is required; otherwise schedule offline or `VACUUM INTO` compaction.
5. **`secure_delete=OFF` leaks deleted tenant data into page-level backups, and `secure_delete=ON` still leaves deleted rows in `-wal` until reset (Experiment 3.2, `DEL-01`, `DEL-03`, `DEL-04`, `DEL-05`).** Under `secure_delete=OFF`, `199` of `204` deleted rows remained readable in freelist leaves, the freelist trunk, and live-leaf freeblocks—and survived verbatim in plain file copies, `sqlite3_backup`, and `sqlite3_serialize` (`95/100` in `DEL-01`). Turning `secure_delete=ON` eliminated all residue in the `.db` file at rest (and in every backup of it), at a cost of `54` WAL frames vs `5` for the same 204-row `DELETE`—yet all `40` deleted rows stayed readable in `-wal` even after a `PASSIVE` checkpoint until `wal_checkpoint(TRUNCATE)` reset the WAL. Multi-tenant deletion policies must combine `secure_delete=ON` with WAL truncation (and retention/rotation of shipped WAL segments).
6. **Choose E1, E2, or E3 to match the capture method, and do not trust `dbhash` for logical equality (Experiment 3.3 Parts A, A2, B, and C; `EQ-01`, `EQ-03`, `EQ-05`).** Different insert orders or update/delete histories produce physically different files (`4` distinct SHA-256s in Part A), a `sqlite3_backup` copy matches its source at **E2** (ignoring bytes `24-27`, `40-43`, `92-99`), and logical rebuilds match at **E3**. Use our E3 logical hash (`internal/equal`) rather than `dbhash`, which missed `5` of `6` blind spots in Part B and `10` of `13` in `EQ-01`. When hashing an uncheckpointed WAL snapshot, open it read-only (`LogicalHashFile` and `RunDBHash` do this via `HasLiveSideFile`) so inspection does not mutate `.db` and delete `-wal`.
7. **Know the exact limits of `VACUUM INTO` as a canonicalizer (Experiment 3.3 Parts A and C, `R2-02`, `R2-04`).** `VACUUM INTO` gives E1-identical canonical files across different write histories and R2 replays only when both databases share the same table creation order, the same schema cookie (a `sqlite3_backup` copy or a dropped scratch table shifts byte `43`, making the canonical files E2-equal instead of E1-equal), and the same `default_cache_size`. Because `VACUUM INTO` writes a rollback-mode file (`bytes 18-19 = 1,1`), compare canonical SHA-256s *before* switching a restored copy back to WAL mode.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 3.1 Part A, why did the first split (insert `#5`) change 4 pages while the second split (insert `#9`) changed only 3 pages, and why did `WITHOUT ROWID` depth-2 append splits in Part E change 5 pages instead of 3?
2. In Experiment 3.1 Part B, inserting rowid `11` into full leaf `4` changed the bytes of `5` pages (`[1 2 4 5 8]`), yet the commit wrote `6` WAL frames (`[1 2 3 4 5 8]`). Why did page `3` enter the WAL with an identical SHA-256 (`c0995adc833eadb1`), and what does that mean for a platform shipping WAL frames versus a page-diff replicator?
3. In Experiment 3.2, why did deleting `204` rows write `5` WAL frames under `secure_delete=OFF` versus `54` WAL frames under `secure_delete=ON`, and why were `40` of `40` deleted rows still readable on disk after `DELETE` and `PRAGMA wal_checkpoint(PASSIVE)` even with `secure_delete=ON`?
4. In Experiment 3.3 Part A, `one-transaction` and `per-statement` produced byte-identical files in WAL mode (`224810279caf4016`), whereas in Part A2 (rollback mode) they differed on 1 of 266 pages (`E2` equal at bytes `[26 27 94 95]`). Why did commit boundaries disappear in WAL mode, and why did `backup`'s `VACUUM INTO` canonical file still differ from R0's canonical file at byte `43`?
5. Name three logical differences that SQLite's `dbhash` tool misses in Experiment 3.3 Part B, explain the exact cause in `tool/dbhash.c` for each, and explain how `internal/equal/logical.go` closes them.

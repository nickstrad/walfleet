# Chapter 2 — Database file format and pages

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `btreeInt.h`, `btree.c`, `util.c` |
| Linked C sources (in the pinned tree, not copied) | `vdbeaux.c`, `pager.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Docker with Docker Compose, and Go 1.26 (`go.mod` specifies `go 1.26.0`) to invoke the repository's experiment runner (`cmd/course`) from the host. Build the `lab` container image (`Dockerfile.lab`) once from the repository root:
>   ```text
>   make docker-build
>   ```
>   The image bundles the pinned SQLite `3.53.4` source tree, the compiled SQLite C tools (`sqlite3`, `showwal`, `dbhash`; Experiment 2.1 cross-checks its header decode against the `sqlite3` CLI's `.dbinfo` command), `strace`, and the DuckDB CLI that renders each experiment's CSV results as a walkthrough.
> - **Run an experiment** from the repository root. Each command starts the `lab` container, runs the experiment inside it, and prints the rendered walkthrough; add `-i` (`make run EXP=2.2 ARGS='-i'`) to stay in the DuckDB shell afterwards with all result tables loaded:
>   ```text
>   make run EXP=2.1
>   make run EXP=2.2
>   make run EXP=2.3
>   ```
>   Inside the container, scratch databases and CSV tables are written under `/work/2.1/`, `/work/2.2/`, and `/work/2.3/` (the Docker `lab-work` volume). When run directly on the host, they go under `_work/2.1/`, `_work/2.2/`, and `_work/2.3/`, which are git-ignored and safe to delete.
> - **Running an experiment directly:** Each experiment is an ordinary Go program (`make run-native EXP=2.2`). Run directly on the host, it prints only its verdict and writes its CSV tables under `_work/2.2/results/`. Experiment 2.1's `.dbinfo` cross-check then requires a matching `sqlite3` CLI on the host (`make fetch-source` to fetch the pinned SQLite source, then `make build-tools` to compile the C tools). If no `sqlite3` CLI is found anywhere, Experiment 2.1 still runs, prints `NO-CLI:`, and leaves the `.dbinfo` column as `?`. If `$SQLITE3_BIN` names a missing path or any discovered CLI reports a different `sqlite_source_id()`, Experiment 2.1 fails with exit status 1.
> - **Cross-platform notes and macOS:** Nothing in Chapter 2 is **Linux only** (no `strace`, no `/proc`). Experiment 2.2 part E creates and deletes a 1 GiB **sparse file** (a file whose unwritten middle is recorded as a hole rather than allocated disk blocks; APFS on macOS also supports sparse files). On macOS, all three experiments also run directly on the host.
> - **Measured platform:** The stored evidence in this chapter was captured in the Debian `lab` container on `linux/arm64` (Go 1.26).

---

## 1. Model

An SQLite database file is a flat array of fixed-size blocks called **pages**, numbered starting at 1. Page *n* occupies byte range `(n - 1) * pageSize` through `n * pageSize - 1`. Every valid **page size** is a power of two from 512 to 65,536 bytes (4,096 bytes by default). A small suffix of each page—0 bytes by default, stored at header offset 20—can be reserved for extensions; the remaining `pageSize - reservedBytes` bytes are the page's **usable size**.

Every multi-byte integer in the file format is **big-endian** (most-significant byte first). Page 1 always begins with a 100-byte **database header** describing the file's geometry, format versions, counters, and persistent settings. The rest of page 1, starting at byte offset 100, is the **root page** (top node) of `sqlite_schema`, the catalog table listing every table, index, view, and trigger in the database.

```text
Database file (size = page_count * page_size)
+---------------------------------------------------------------+
| Page 1  (bytes 0 .. page_size-1)                              |
|   [0..99]     100-byte database header (magic, page size,     |
|               counters, freelist head, encoding, versions...) |
|   [100..107]  B-tree page header (8 B leaf / 12 B interior)   |
|   [108..]     Cell pointer array (2 B/cell, sorted)     |     |
|               ... unallocated space ...                 v     |
|               Cell content area (cells + freeblocks)    ^     |
|               [optional reserved suffix, default 0 B]   |     |
+---------------------------------------------------------------+
| Page 2  (bytes page_size .. 2*page_size-1)                    |
|   One of the page roles below (no 100-byte file header)       |
+---------------------------------------------------------------+
| ...                                                           |
+---------------------------------------------------------------+
| Page N  (bytes (N-1)*page_size .. N*page_size-1)              |
+---------------------------------------------------------------+

Every page in the file holds exactly one role:
  B-tree pages (header byte 0 identifies the type):
    0x05  table-interior   rowid divider keys + 4-byte child pointers
    0x0d  table-leaf       rowid + binary record payload (+ overflow ptr)
    0x02  index-interior   key payload + 4-byte child pointers
    0x0a  index-leaf       key payload (+ overflow ptr)
  Non-B-tree pages:
    overflow        4-byte next-overflow page + up to (usable-4) payload bytes
    freelist-trunk  4-byte next-trunk + 4-byte leaf count + leaf page numbers
    freelist-leaf   unallocated page listed by a trunk (zeroed if secure_delete)
    pointer-map     reverse-pointer map (present only when auto_vacuum != NONE)
    lock-byte       page covering byte 1,073,741,824 (1 GiB); never holds data
```

Each structural unit in that diagram has one job:

1. **B-tree pages.** Every table and every index is its own **B-tree** (a balanced search tree of pages) rooted at the page number listed in `sqlite_schema`. **Interior pages** store divider keys and 4-byte child page pointers; **leaf pages** store rows or index entries as **cells**. A B-tree page starts with an 8-byte header on leaves or a 12-byte header on interior pages (bytes 8–11 hold the rightmost child pointer). Immediately after the header sits the **cell pointer array**: 2-byte big-endian offsets pointing to each cell in key order, growing downward. Cells themselves occupy the **cell content area** at the bottom of the page, growing upward. Reclaimed regions of 4 bytes or larger join a sorted linked list of **freeblocks** inside the cell content area; leftover gaps of 1–3 bytes are counted as **fragmented bytes** (header byte 7).
2. **Cells, varints, and records.** A table-leaf cell stores two **varints** (big-endian variable-length integers of 1 to 9 bytes)—the total payload length and the 64-bit signed **rowid**—followed by the local bytes of the row's **record**. A binary record starts with a header giving its own byte length and one **serial type** varint per column, followed by the packed column values. Each serial type specifies whether the column is `NULL`, a signed 1-, 2-, 3-, 4-, 6-, or 8-byte integer, an IEEE 754 double, the zero-byte integer constant `0` or `1`, a blob, or text.
3. **Overflow pages.** When a cell's payload exceeds the page's local limit (`usableSize - 35` on a table leaf, or 4,061 bytes at a 4,096-byte usable size), SQLite stores a prefix on the B-tree page, appends a 4-byte page number to the cell, and spills the rest onto a singly linked chain of **overflow pages**. Each overflow page holds a 4-byte next-page pointer (`0` on the last page) followed by up to `usableSize - 4` (4,092) payload bytes.
4. **Freelist pages.** When `auto_vacuum=NONE`, pages emptied by deletes or dropped tables stay in the file instead of being truncated. They join a linked list of **freelist trunk** pages rooted at header offset 32. Each trunk stores the next trunk's page number (bytes 0–3), a leaf count (bytes 4–7), and an array of 4-byte page numbers naming reusable **freelist leaf** pages. Header offset 36 holds the total count of trunks plus leaves.
5. **Pointer-map pages.** When `auto_vacuum` is `FULL` or `INCREMENTAL` (header offset 52 non-zero), SQLite relocates pages toward the front of the file so it can truncate empty pages off the end. **Pointer-map pages** record 5-byte reverse-parent entries for up to `usableSize / 5` subsequent pages, appearing at fixed intervals starting at page 2 so SQLite can locate a page's parent without scanning every B-tree.
6. **The lock-byte page.** Unix rollback-mode **POSIX advisory locks** (OS byte-range locks acquired via `fcntl`) sit at byte offset `1,073,741,824` (`0x40000000`, 1 GiB). Once a database grows past 1 GiB, the single page containing that byte (`1073741824 / pageSize + 1`) is reserved as the **lock-byte page** and is never allocated to any B-tree, overflow chain, freelist, or pointer map.

**WAL mode as the platform default.** Platform databases in this book run in **WAL** (write-ahead log) mode (`journal_mode=wal`), including every deterministic **R0** test database built in this chapter (a seeded 30-step workload defined in this repository's `internal/recipe` package). Measuring the file format under WAL mode has two consequences and four deliberate rollback-mode (`journal_mode=delete`) test exceptions:

- **Consequence 1 (header change counter).** In WAL mode, commits append frames to the sibling `-wal` file and leave `.db` untouched until a **checkpoint** copies committed frames back into `.db`. Even inside `-wal`, header bytes 24–27 do not advance on every commit (Experiment 2.1 part C).
- **Consequence 2 (at-rest capture).** A database is **at rest** when `.db` alone holds the complete committed state: no non-empty `-wal` file and no **hot journal** (a `-journal` file left by an interrupted rollback-mode transaction whose header has not been zeroed). Taking a page-level capture of a WAL database requires a checkpoint first (or a live read transaction, implemented as rung R4 in Chapter 6).
- **Exceptions using rollback mode.** Four test setups deliberately use `journal_mode=delete`: Experiment 2.1 part B measures rollback-mode header updates on commit; Experiment 2.1 part D switches a file between rollback and WAL modes to observe header bytes 18–19; Experiment 2.1 part E and Experiment 2.2 parts E and F open hand-edited synthetic files read-only so SQLite neither leaves sidecar files behind nor repairs damaged files on open; and Experiment 2.3 part D2 constructs a real hot journal alongside part D1's uncheckpointed WAL.

**The Rebuild Ladder (rung R1).** Across this book, the **Rebuild Ladder** climbs from a single reproducible database file (rung **R0**) up to full fleet recovery, verifying each rebuild against an explicit equality level. Chapter 1 built rung **R0 (Recipe)**: a seeded workload whose closed database file has a known SHA-256 hash after every step. This chapter builds rung **R1 (Pages)**: read every page of a closed database with a pure-Go parser (`internal/dbfile`), write the raw page images plus a per-page SHA-256 **manifest** (`internal/capture`), rebuild the file from those pages alone, and verify the rebuild at equality level **E1 bytes** (`internal/equal`: identical file SHA-256 and zero differing pages). Later rungs—R3 (WAL replay), R4 (live snapshot), and R6 (point-in-time restore)—reuse the page reader and manifest built here.

---

## 2. Question

Given only the raw bytes of a closed SQLite database file, can a Go program with no SQLite library linked into it decode the 100-byte header, assign the sole role of every page in the file, and rebuild a working database byte for byte from its pages?

This chapter's experiments reuse three shared Go packages already present in this repository (linked here rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite with explicit file and connection settings, checks the pinned `sqlite_source_id()`, manages `_work/N.M/` scratch directories, and locates the pinned C `sqlite3` CLI.
- [`internal/trace`](../internal/trace/): provides `SelfCommand` so Experiments 2.1 and 2.3 can spawn child processes to inspect or modify files without calling `os.Open` inside a process that holds an SQLite connection (on POSIX systems, closing any file descriptor drops all POSIX locks that process holds on that file).
- [`internal/recipe`](../internal/recipe/): builds the deterministic 30-step R0 workload database (tables `accounts`, `documents`, `events`, and index `events_by_account`) and matches rebuilt files against the cached per-step SHA-256 table (`MatchStep`).

This chapter introduces three new shared packages—`internal/dbfile` (read-only file parser and page census), `internal/equal` (equality level E1 and page manifests), and `internal/capture` (rung R1 at-rest check, page capture, and verified rebuild)—inlined in full in Section 5.

---

## 3. Call path

This section walks the pinned SQLite `version-3.53.4` C source for every file-format rule that `internal/dbfile` re-implements. Three files are copied whole into this chapter directory (`btreeInt.h`, `btree.c`, `util.c`); `vdbeaux.c` and `pager.c` are linked in the pinned SQLite source tree (`$SQLITE_SRC` inside the `lab` container, or `_src/sqlite-3.53.4` on the host after `make fetch-source`).

### 3.1 File-format specification and the 100-byte database header

1. **In-source format specification** — [`src/btreeInt.h:1-228`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btreeInt.h#L1-L228) (and `FORMAT DETAILS` at [`src/btreeInt.h:45-228`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btreeInt.h#L45-L228), local copy: [`btreeInt.h`](btreeInt.h)). Documents the entire on-disk layout in one comment block: the 100-byte header field table (offsets 0–99), the B-tree page layout diagram, the 8/12-byte page header, freeblocks and fragments, 1-to-9-byte varints, cell layouts per page type, overflow pages, and freelist trunk/leaf pages. Lines 222–229 define `MX_CELL_SIZE` (`pageSize - 8`) and `MX_CELL` (`(pageSize - 8) / 6`, based on a minimum 4-byte cell plus 2-byte cell pointer).
2. **`newDatabase`** — [`src/btree.c:3510-3547`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3510-L3547) (local copy: [`btree.c`](btree.c)). Runs on the first write transaction to an empty file and writes the initial 100-byte header onto page 1:
   ```c
   memcpy(data, zMagicHeader, sizeof(zMagicHeader));
   assert( sizeof(zMagicHeader)==16 );
   data[16] = (u8)((pBt->pageSize>>8)&0xff);
   data[17] = (u8)((pBt->pageSize>>16)&0xff);
   data[18] = 1;
   data[19] = 1;
   data[20] = (u8)(pBt->pageSize - pBt->usableSize);
   data[21] = 64;
   data[22] = 32;
   data[23] = 32;
   memset(&data[24], 0, 100-24);
   zeroPage(pP1, PTF_INTKEY|PTF_LEAF|PTF_LEAFDATA );
   ```
   Lines 3526–3527 explain why page size 65,536 is stored as `00 01` (value `1`): shifting `65536` (`0x10000`) right by 8 gives `0x00` in byte 16, and shifting right by 16 gives `0x01` in byte 17. Lines 3528–3529 start every fresh file at write/read versions `1/1` (rollback mode); switching to WAL later updates bytes 18–19 to `2/2`. Line 3536 formats the rest of page 1 as an empty table-leaf page (`0x0d`) for `sqlite_schema`. Lines 3541–3542 write the auto-vacuum and incremental-vacuum flags into offsets 52 and 64, and line 3545 writes the initial page count `1` into byte 31 (the low byte of offset 28–31).
3. **`lockBtree`** — [`src/btree.c:3282-3459`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3282-L3459) (local copy: [`btree.c`](btree.c)). Runs on a connection's first transaction: takes a shared lock (`sqlite3PagerSharedLock`, line 3290), fetches page 1 (`btreeGetPage`, line 3292), and validates the header:
   - **In-header database size (lines 3298–3302):** reads the 4-byte page count at offset 28 (`nPage`) and the OS file size in pages (`nPageFile`). It falls back to `nPageFile` whenever `nPage == 0` or bytes 24–27 (change counter) differ from bytes 92–95 (version-valid-for):
     ```c
     nPage = get4byte(28+(u8*)pPage1->aData);
     sqlite3PagerPagecount(pBt->pPager, (int*)&nPageFile);
     if( nPage==0 || memcmp(24+(u8*)pPage1->aData, 92+(u8*)pPage1->aData,4)!=0 ){
       nPage = nPageFile;
     }
     ```
     Lines 3405–3412 reject `nPage > nPageFile` with `SQLITE_CORRUPT_BKPT` unless `writable_schema` is set.
   - **Magic, format versions, and WAL switch (lines 3314–3356):** checks the 16-byte magic `"SQLite format 3\0"`. If write version (byte 18) exceeds 2, SQLite forces the handle read-only; if read version (byte 19) exceeds 2, it returns `SQLITE_NOTADB`. If byte 19 is `2` and the WAL is not yet open (lines 3341–3352), SQLite opens the WAL, releases page 1, and returns so the caller retries `lockBtree` and reads the latest page 1 from the WAL.
   - **Payload fractions, page size, and usable size (lines 3365–3425):** verifies bytes 21–23 equal `64, 32, 32`. It decodes page size as `(page1[16]<<8) | (page1[17]<<16)` (line 3371, turning `00 01` back into `65536`) and requires a power of two in `512..65536` (lines 3374–3379). If the file's page size differs from the assumed size, it updates the pager and returns to re-read page 1 (lines 3389–3404). It requires `usableSize >= 480` (line 3416) and reads `autoVacuum` and `incrVacuum` from offsets 52 and 64 (lines 3423–3424).
   - **Local payload limits (lines 3441–3444):** precomputes four spill thresholds from `usableSize`: `maxLocal = (usableSize-12)*64/255 - 23`, `minLocal = (usableSize-12)*32/255 - 23`, `maxLeaf = usableSize - 35`, and `minLeaf = (usableSize-12)*32/255 - 23`. At usable size 4,096, those equal `1002`, `489`, `4061`, and `489`.

### 3.2 Page headers, cells, and the local/overflow split

4. **`decodeFlags`** — [`src/btree.c:2028-2085`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L2028-L2085) (local copy: [`btree.c`](btree.c)). Maps the 1-byte page type at offset 0 of the page header to structural flags and cell-parsing function pointers: `0x0d` (table leaf, `intKeyLeaf=1`, `btreeParseCellPtr`, `maxLeaf`/`minLeaf`), `0x0a` (index leaf, `btreeParseCellPtrIndex`, `maxLocal`/`minLocal`), `0x02` (index interior, `childPtrSize=4`, `btreeParseCellPtrIndex`), and `0x05` (table interior, `childPtrSize=4`, `btreeParseCellPtrNoPayload`). Any other byte returns `SQLITE_CORRUPT_PAGE`.
5. **`btreeInitPage`** — [`src/btree.c:2218-2265`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L2218-L2265) (local copy: [`btree.c`](btree.c)). Reads the page header at `hdrOffset` (100 on page 1, 0 on every other page) and calls `decodeFlags(pPage, data[0])` (line 2234). It sets the cell-pointer array start to `hdrOffset + 8 + childPtrSize` (line 2240), reads `nCell` from bytes 3–4, and rejects `nCell > MX_CELL(pBt)` (lines 2246–2250). Free space is marked `nFree = -1` for lazy calculation (line 2259), and `btreeCellSizeCheck` runs when `PRAGMA cell_size_check` is enabled (lines 2261–2263).
6. **`btreeParseCellPtr`** — [`src/btree.c:1259-1346`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1259-L1346) and **`btreeParseCellPtrIndex`** — [`src/btree.c:1347-1385`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1347-L1385) (local copy: [`btree.c`](btree.c)). `btreeParseCellPtr` parses a table-leaf cell: it decodes the payload-size varint (lines 1280–1289) and unrolled rowid varint (lines 1298–1328). If `nPayload <= maxLocal`, the entire payload sits inline and `nSize` is clamped up to at least 4 bytes (line 1341) so deleting the cell always leaves room for a 4-byte freeblock header. `btreeParseCellPtrIndex` parses index cells: it skips `childPtrSize` (0 on leaves, 4 on interior pages), decodes the payload-size varint (no rowid), and applies the same local-vs-overflow check.
7. **`btreeParseCellAdjustSizeForOverflow`** — [`src/btree.c:1178-1207`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1178-L1207) (local copy: [`btree.c`](btree.c)). Decides how many bytes stay on the B-tree page when a cell overflows:
   ```c
   minLocal = pPage->minLocal;
   maxLocal = pPage->maxLocal;
   surplus = minLocal + (pInfo->nPayload - minLocal)%(pPage->pBt->usableSize-4);
   if( surplus <= maxLocal ){
     pInfo->nLocal = (u16)surplus;
   }else{
     pInfo->nLocal = (u16)minLocal;
   }
   pInfo->nSize = (u16)(&pInfo->pPayload[pInfo->nLocal] - pCell) + 4;
   ```
   Taking the remainder modulo `usableSize - 4` (one overflow page's payload capacity) packs full overflow pages and puts the leftover remainder locally whenever it fits in `minLocal..maxLocal`, minimizing wasted space on the last overflow page. The trailing `+ 4` holds the 4-byte page number of the first overflow page. As the comment at lines 1189–1190 warns, this formula is frozen as part of the file format.

### 3.3 Varints, records, overflow chains, and the freelist

8. **`sqlite3GetVarint`** — [`src/util.c:1633-1781`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/util.c#L1633-L1781) (local copy: [`util.c`](util.c)). Decodes a 1-to-9-byte big-endian varint using fast paths for 1-byte (`< 0x80`) and 2-byte values, followed by an unrolled 3-to-9-byte decoder. Bytes 1–8 carry 7 value bits with bit 7 set while more bytes follow; byte 9 carries all 8 value bits so any 64-bit integer fits in 9 bytes.
9. **`sqlite3VdbeSerialGet`** — [`src/vdbeaux.c:4123-4214`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L4123-L4214) (in the pinned source tree). Decodes one column value from its serial type and body bytes: `0` is `NULL`; `1..4` are big-endian signed 1-to-4-byte integers; `5` is a 6-byte signed integer; `6` and `7` delegate to `serialGet` for an 8-byte signed integer or IEEE 754 double (where an IEEE `NaN` becomes `NULL`, [`src/vdbeaux.c:4103`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L4103)); `8` and `9` are the zero-byte integer constants `0` and `1`; even `N >= 12` is a blob of `(N - 12) / 2` bytes; and odd `N >= 13` is text of `(N - 13) / 2` bytes. Serial types `10` (internal virtual-table update marker) and `11` (reserved) never belong in a database file, so `internal/dbfile` rejects them.
10. **`getOverflowPage`** — [`src/btree.c:5007-5062`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L5007-L5062) (local copy: [`btree.c`](btree.c)). Follows an overflow chain from page `ovfl`. Under `auto_vacuum=NONE`, it reads the first 4 bytes of `ovfl` (`get4byte(pPage->aData)` at line 5051) as the next overflow page number. When auto-vacuum is enabled (lines 5027–5043), it first guesses `ovfl + 1` (skipping pointer-map and lock-byte pages) and checks the pointer map for a matching `PTRMAP_OVERFLOW2` parent pointer, avoiding a read of the overflow page itself when seeking past it.
11. **`allocateBtreePage`** — [`src/btree.c:6514-6822`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6514-L6822) (local copy: [`btree.c`](btree.c)). Allocates a new page for a B-tree or overflow chain. When header offset 36 (freelist count) is non-zero (line 6535), it decrements offset 36 (line 6572) and pops a page from the freelist rooted at header offset 32 (line 6590)—either the trunk itself when its leaf count is 0 (lines 6607–6618, advancing offset 32 to the trunk's next pointer) or one of its leaves. When the freelist is empty, it increments `pBt->nPage`, skips `PENDING_BYTE_PAGE(pBt)` (the 1 GiB lock-byte page, lines 6779 and 6797), allocates a pointer-map page first if auto-vacuum requires one (lines 6782–6798), and writes the new page count into header offset 28 (line 6800).
12. **`freePage2`** — [`src/btree.c:6836-6969`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6836-L6969) (local copy: [`btree.c`](btree.c)). Returns page `iPage` to the freelist: it increments header offset 36 (lines 6861–6862), zeroes the entire page when `secure_delete=ON` (lines 6864–6874), and updates the pointer map if auto-vacuum is enabled (lines 6879–6882). If a trunk page already exists with fewer than `usableSize/4 - 8` leaf entries (line 6910), `freePage2` appends `iPage` to that trunk's leaf array (lines 6932–6933). Otherwise, `iPage` becomes the new head trunk: its bytes 0–3 point to the old trunk, its leaf count (bytes 4–7) is 0, and header offset 32 is updated to `iPage` (lines 6957–6959). Readers still accept up to `usableSize/4 - 2` entries (line 6906, matching `allocateBtreePage` line 6621); the 6-entry writer margin keeps files readable by SQLite releases prior to 3.6.0 (lines 6914–6928).

### 3.4 Change counter updates: rollback mode versus WAL mode

Four functions in `src/pager.c` (in the pinned source tree) govern header bytes 24–27 (file change counter), 92–95 (version-valid-for), and 96–99 (`SQLITE_VERSION_NUMBER`):

- **`pager_write_changecounter`** — [`src/pager.c:3130-3143`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3130-L3143). Writes `pPager->dbFileVers + 1` into bytes 24–27 and bytes 92–95, and writes `SQLITE_VERSION_NUMBER` (`3053004`) into bytes 96–99. Rather than incrementing bytes 24–27 in place, it computes `dbFileVers + 1` from the cached copy last read from page 1.
- **`readDbPage`** — [`src/pager.c:3067-3120`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3067-L3120). Reads a page from the WAL if present, or from `.db` otherwise. Whenever the page read is page 1, it copies bytes 24–39 into the pager's in-memory `pPager->dbFileVers` buffer (lines 3093–3110).
- **`pager_incr_changecounter`** — [`src/pager.c:6363-6437`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6363-L6437). The rollback-mode commit path: once per write transaction, it makes page 1 writable and calls `pager_write_changecounter` (line 6409), so rollback mode increments bytes 24–27 and 92–95 on every commit and writes page 1 to `.db`.
- **`pagerWalFrames`** — [`src/pager.c:3225-3282`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3225-L3282). The WAL-mode commit path: calls `pager_write_changecounter` only when the first page in the transaction's sorted dirty-page list is page 1 (line 3264). A WAL commit that does not modify page 1 never runs `pager_write_changecounter`. When a single WAL connection repeatedly modifies page 1 while page 1 stays in its page cache, `dbFileVers` is never refreshed between commits, so every commit writes the same `dbFileVers + 1`. Evicting and re-reading page 1 through `readDbPage`—either on a new connection or via `PRAGMA shrink_memory`—refreshes `dbFileVers` so the next page-1 commit advances the counter.

### Version notes

Behavior differences across SQLite releases relevant to the database file format (grounded in the repository's SQLite source inventory [`docs/knowledge/source-inventory-ch01-04.md`](../docs/knowledge/source-inventory-ch01-04.md), SQLite's upstream documentation `fileformat2.html` and `changes.html`, and the copied C source):

- **In-header database size validation (added in 3.7.0, 2010-07-21).** Header bytes 28–31 are trusted only when non-zero and bytes 24–27 equal bytes 92–95 ([`src/btree.c:3300`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3300), `fileformat2.html`). Older writers that do not update bytes 92–95 leave a mismatch, causing 3.7.0+ readers to fall back to the OS file size.
- **Fixed payload fractions `64/32/32` and freelist trunk margin (3.6.0).** Header bytes 21–23 became fixed constants in 3.6.0 ([`src/btree.c:3360-3367`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3360-L3367)). Writers still cap freelist trunks at `usableSize/4 - 8` leaves rather than `usableSize/4 - 2` so pre-3.6.0 readers do not report corruption ([`src/btree.c:6906-6928`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6906-L6928)).
- **Default page size 4,096 bytes (changed in 3.12.0, `changes.html`, `pgszchng2016.html`).** Earlier releases defaulted to 1,024 bytes.
- **`sqlite_schema` naming.** `sqlite_schema` is the preferred name; the legacy name `sqlite_master` remains accepted ([`src/sqliteInt.h:1246-1248`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/sqliteInt.h#L1246-L1248)).
- **Virtual tables used as oracles.** `sqlite_dbpage` (which reads raw database pages through SQL) was added in 3.21.0 (resizing via `INSERT` in 3.47.0); `dbstat` (which reports per-page B-tree statistics) gained aggregate mode in 3.31.0 (`changes.html`).
- **CLI `.diskused` (3.54.x).** Added after this book's 3.53.4 pin (`sqlite3_analyzer` deprecated in 3.54.x); not available at this pin.

### Experiment 2.1 — Header and what a commit changes (`2.1-header-and-what-a-commit-changes`)

- **Claim:** Plain Go without SQLite decodes all 23 fields of the 100-byte header and matches `PRAGMA` and the pinned CLI's `.dbinfo` on all 18 rows that have an oracle (`version-valid-for` has neither; on negative `user_version` or `application_id` values, `PRAGMA` and `internal/dbfile` decode signed `int32` while `.dbinfo` formats all 4-byte fields unsigned with `%u`). In rollback mode, every committed write transaction advances header bytes 24–27 and 92–95 by 1 in `.db`. In WAL mode, `.db` does not change until checkpoint, and commits that do not dirty page 1 change no header byte anywhere. WAL commits that dirty page 1 write `dbFileVers + 1`, where `dbFileVers` refreshes whenever `readDbPage` reads page 1 into the page cache—holding at `base + 1` on one connection while page 1 stays cached (C1), and advancing when page 1 is re-read on a new connection (C3) or after `PRAGMA shrink_memory` (C4). The in-header page count (bytes 28–31) is trusted only when non-zero and bytes 24–27 equal bytes 92–95, and page size 65,536 is stored as `00 01` (`1`) in bytes 16–17.
- **Evidence form:** `EXACT` (every stored CSV table must match a fresh run byte for byte).
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the rendered walkthrough; add `-i` to stay in the DuckDB shell afterwards with all result tables loaded):
  ```text
  make run EXP=2.1
  ```

<!-- include: experiments/2.1-header-and-what-a-commit-changes/main.go -->
```go
// Experiment 2.1 decodes the 100-byte SQLite database header field by field
// with internal/dbfile, checks every field against SQLite PRAGMAs and the
// pinned CLI's .dbinfo, and measures which header bytes change on commit in
// rollback mode versus WAL mode, on journal-mode switches, on edited in-header
// page counts, and across page sizes 512, 4096, and 65536.
//
// Run from the repo root:
//
//	go run ./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"

	_ "modernc.org/sqlite"
)

const (
	pinnedVersionNumber = 3053004
	childReadHeader     = "child-read-header"
)

var (
	set     *results.Set
	bases   *results.Table
	events  *results.Table
	changes *results.Table
)

func openTables(dir string) {
	set = results.MustOpen(dir)
	bases = set.Table("bases", "part", "name", "journal_mode", "built_to_step", "write_version", "read_version", "change_counter", "in_header_size", "file_pages")
	events = set.Table("events", "seq", "part", "scenario", "event", "wal_state", "detail")
	changes = set.Table("header_changes", "event_seq", "view", "byte_offset", "size", "field", "before_value", "after_value", "changed_bytes")
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

func openReadOnly(path string) *sql.DB {
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {"ro"}})
	if err != nil {
		fatal("uri", err)
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		fatal("open read-only", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func queryText(ctx context.Context, q sqlitex.Querier, query string) (string, error) {
	var value any
	if err := q.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return "", err
	}
	if b, ok := value.([]byte); ok {
		return string(b), nil
	}
	return fmt.Sprint(value), nil
}

// readHeaderFromChild reads the first 100 bytes of the database file in a
// separate child process so this process never opens and closes a file
// descriptor on a database while holding a live SQLite connection (closing any
// fd drops POSIX locks for the whole process).
func readHeaderFromChild(path string) []byte {
	command := must(trace.SelfCommand(childReadHeader, path))("self command")
	out, err := exec.Command(command[0], command[1:]...).Output()
	if err != nil {
		fatal("child read header", err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(raw) != dbfile.HeaderSize {
		fatal("child output", fmt.Errorf("%q: %v", out, err))
	}
	return raw
}

func childMain(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	header := make([]byte, dbfile.HeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(hex.EncodeToString(header))
}

// logicalHeader reads SQLite's current view of page 1 through sqlite_dbpage,
// which includes uncheckpointed frames from the -wal in WAL mode.
func logicalHeader(ctx context.Context, q sqlitex.Querier) []byte {
	var data []byte
	if err := q.QueryRowContext(ctx, "SELECT data FROM sqlite_dbpage WHERE pgno = 1").Scan(&data); err != nil {
		fatal("sqlite_dbpage page 1", err)
	}
	return data[:dbfile.HeaderSize]
}

func parse(raw []byte) dbfile.Header {
	return must(dbfile.ParseHeader(raw))("parse header")
}

type change struct {
	offset, size int
	name         string
	before       string
	after        string
	bytes        string
}

func changedFields(before, after []byte) []change {
	var found []change
	old, updated := parse(before).Fields(), parse(after).Fields()
	for i, field := range old {
		if bytes.Equal(before[field.Offset:field.Offset+field.Size], after[field.Offset:field.Offset+field.Size]) {
			continue
		}
		var offsets []string
		for b := field.Offset; b < field.Offset+field.Size; b++ {
			if before[b] != after[b] {
				offsets = append(offsets, strconv.Itoa(b))
			}
		}
		found = append(found, change{field.Offset, field.Size, field.Name, field.Value, updated[i].Value, strings.Join(offsets, " ")})
	}
	return found
}

var eventCount int

// newEvent records one point in a scenario and returns its number.
func newEvent(part, scenario, event, walState, detail string) int {
	eventCount++
	events.Row(eventCount, part, scenario, event, walState, detail)
	return eventCount
}

// recordChanges writes the header fields that differ between two copies of
// page 1; one row with no field says that no header byte changed.
func recordChanges(event int, view string, before, after []byte) {
	found := changedFields(before, after)
	if len(found) == 0 {
		changes.Row(event, view, nil, nil, nil, nil, nil, nil)
	}
	for _, c := range found {
		changes.Row(event, view, c.offset, c.size, c.name, c.before, c.after, c.bytes)
	}
}

// recordBase writes what a base database's header says before a scenario starts.
func recordBase(part, name, mode string, steps int, path string) []byte {
	header := readHeaderFromChild(path)
	counter, _ := counters(header)
	info := must(os.Stat(path))("stat " + name)
	bases.Row(part, name, mode, steps, header[18], header[19], counter, binary.BigEndian.Uint32(header[28:32]), info.Size()/4096)
	return header
}

func counters(raw []byte) (uint32, uint32) {
	return binary.BigEndian.Uint32(raw[24:28]), binary.BigEndian.Uint32(raw[92:96])
}

func copyFile(from, to string) {
	data := must(os.ReadFile(from))("read " + filepath.Base(from))
	if err := os.WriteFile(to, data, 0o644); err != nil {
		fatal("write copy", err)
	}
}

func walSize(path string) int64 {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		return -1
	}
	return info.Size()
}

func walText(path string) string {
	size := walSize(path)
	if size < 0 {
		return "no -wal file"
	}
	return fmt.Sprintf("-wal %d bytes", size)
}

type fieldRow struct {
	name    string
	ours    string
	pragma  string
	cliName string
}

func parseDBInfo(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) > 0 {
			values[strings.TrimSpace(name)] = fields[0]
		}
	}
	return values
}

func partA(ctx context.Context, dir string, cli *sqlitex.CLI) {
	r := recipe.Default()
	path := filepath.Join(dir, "r0.db")
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build R0", err)
	}
	sum := must(equal.FileSHA256(path))("hash R0")
	f := must(dbfile.Open(path))("parse R0")
	h := f.Header
	pageCount, source := f.PageCount(), f.PageCountSource()
	f.Close()
	set.Table("r0", "recipe", "built_to_step", "sha256", "page_count", "page_count_source", "in_header_size_valid").
		Row(r, r.Steps, sum, pageCount, source, h.InHeaderSizeValid())
	fields := set.Table("r0_header", "byte_offset", "size", "field", "value")
	for _, field := range h.Fields() {
		fields.Row(field.Offset, field.Size, field.Name, field.Value)
	}

	// Open R0 through a normal read-write connection rather than mode=ro: a
	// read-only connection cannot checkpoint and remove the -wal on close.
	db := must(r.Open(ctx, path))("open R0")
	pragma := func(name string) string {
		return must(queryText(ctx, db, "PRAGMA "+name))("PRAGMA " + name)
	}
	autoVacuumNames := map[string]string{"0": "NONE", "1": "FULL", "2": "INCREMENTAL"}
	rows := []fieldRow{
		{"page size", fmt.Sprint(h.PageSize), pragma("page_size"), "database page size"},
		{"write version", fmt.Sprint(h.WriteVersion), "-", "write format"},
		{"read version", fmt.Sprint(h.ReadVersion), "-", "read format"},
		{"reserved bytes", fmt.Sprint(h.ReservedBytes), "-", "reserved bytes"},
		{"file change counter", fmt.Sprint(h.ChangeCounter), "-", "file change counter"},
		{"page count", fmt.Sprint(pageCount), pragma("page_count"), "database page count"},
		{"freelist page count", fmt.Sprint(h.FreelistCount), pragma("freelist_count"), "freelist page count"},
		{"schema cookie", fmt.Sprint(h.SchemaCookie), pragma("schema_version"), "schema cookie"},
		{"schema format", fmt.Sprint(h.SchemaFormat), "-", "schema format"},
		{"default cache size", fmt.Sprint(h.DefaultCacheSize), "-", "default cache size"},
		{"auto_vacuum", h.AutoVacuumName(), autoVacuumNames[pragma("auto_vacuum")], ""},
		{"largest root page", fmt.Sprint(h.LargestRootPage), "-", "autovacuum top root"},
		{"incremental vacuum", fmt.Sprint(h.IncrementalVacuum), "-", "incremental vacuum"},
		{"text encoding", h.EncodingName(), pragma("encoding"), ""},
		{"text encoding number", fmt.Sprint(h.TextEncoding), "-", "text encoding"},
		{"user version", fmt.Sprint(h.UserVersion), pragma("user_version"), "user version"},
		{"application id", fmt.Sprint(h.ApplicationID), pragma("application_id"), "application id"},
		{"version-valid-for", fmt.Sprint(h.VersionValidFor), "-", ""},
		{"SQLite version number", fmt.Sprint(h.SQLiteVersion), "-", "software version"},
	}
	db.Close()
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("R0 after pragmas", err)
	}
	set.Check("A", must(equal.FileSHA256(path))("rehash") == sum, "opening R0, reading the pragmas and closing left it byte-identical and at rest")

	dbinfo := map[string]string{}
	if cli != nil {
		out, err := cli.Exec(ctx, path, ".dbinfo\n")
		if err != nil {
			fatal("CLI .dbinfo", err)
		}
		dbinfo = parseDBInfo(out)
	}
	oracles := set.Table("oracles", "seq", "field", "ours", "pragma", "dbinfo")
	allMatch := true
	checked, unchecked := 0, 0
	for i, row := range rows {
		var pragmaValue, cliValue any
		if row.pragma != "-" {
			pragmaValue = row.pragma
		}
		if row.cliName != "" && cli != nil {
			cliValue = dbinfo[row.cliName]
		}
		hasPragma, hasCLI := pragmaValue != nil, cliValue != nil
		if hasPragma || hasCLI {
			match := (!hasPragma || row.pragma == row.ours) && (!hasCLI || cliValue == row.ours)
			allMatch = allMatch && match
			checked++
		} else {
			unchecked++
		}
		oracles.Row(i+1, row.name, row.ours, pragmaValue, cliValue)
	}
	oracleNames := "PRAGMA or .dbinfo"
	if cli == nil {
		oracleNames = "PRAGMA only, no CLI"
	}
	set.Check("A", allMatch && checked > 0, fmt.Sprintf("each of the %d decoded header fields that has an oracle here (%s) equals SQLite's own report of it; %d field(s) have none (n/a)", checked, oracleNames, unchecked))
	set.Check("A", h.SQLiteVersion == pinnedVersionNumber, fmt.Sprintf("bytes 96-99 read %d (the library that last wrote the file)", pinnedVersionNumber))
	set.Check("A", h.WALMode(), "bytes 18 and 19 are 2 and 2 in a WAL-mode file")
	set.Check("A", h.ChangeCountersAgree() && source == dbfile.PageCountFromHeader, "bytes 24-27 equal 92-95, so the in-header size at 28 is used")
}

func buildBase(ctx context.Context, dir, name, mode string, steps int) string {
	path := filepath.Join(dir, name)
	if err := recipe.Default().WithJournalMode(mode).Build(ctx, path, steps); err != nil {
		fatal("build "+name, err)
	}
	return path
}

func partB(ctx context.Context, dir string) {
	r := recipe.Default().WithJournalMode("delete")
	base := buildBase(ctx, dir, "rollback-base.db", "delete", 27)
	recordBase("B", "rollback-base.db", "delete", 27, base)
	path := filepath.Join(dir, "rollback-one-connection.db")
	copyFile(base, path)
	before := readHeaderFromChild(path)
	db := must(r.Open(ctx, path))("open")
	advancedEveryCommit := true
	for step := 28; step <= 30; step++ {
		if err := r.ApplyStep(ctx, db, step); err != nil {
			fatal("apply", err)
		}
		after := readHeaderFromChild(path)
		set.Check("B", bytes.Equal(after, logicalHeader(ctx, db)), fmt.Sprintf("after step %d the file's page 1 equals SQLite's current page 1 (no -wal in rollback mode)", step))
		recordChanges(newEvent("B", "B1", fmt.Sprintf("commit of step %d", step), walText(path), ""), "file vs previous", before, after)
		oldCounter, oldValid := counters(before)
		newCounter, newValid := counters(after)
		advancedEveryCommit = advancedEveryCommit && newCounter == oldCounter+1 && newValid == oldValid+1 && newCounter == newValid
		before = after
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	set.Check("B", advancedEveryCommit, "rollback mode: bytes 24-27 and 92-95 each advance by exactly one per commit, on one connection")
	final := readHeaderFromChild(path)
	set.Check("B", bytes.Equal(final, before), "closing the connection changed nothing in page 1")
}

func partC(ctx context.Context, dir string) {
	r := recipe.Default()
	base := buildBase(ctx, dir, "wal-base.db", "wal", 27)
	baseHeader := recordBase("C", "wal-base.db", "wal", 27, base)
	c0, _ := counters(baseHeader)
	path := filepath.Join(dir, "wal-one-connection.db")
	copyFile(base, path)
	db := must(r.Open(ctx, path))("open")
	previous := logicalHeader(ctx, db)
	var currentCounters []uint32
	fileUnchanged := true
	for step := 28; step <= 30; step++ {
		if err := r.ApplyStep(ctx, db, step); err != nil {
			fatal("apply", err)
		}
		current := logicalHeader(ctx, db)
		file := readHeaderFromChild(path)
		event := newEvent("C", "C1", fmt.Sprintf("commit of step %d", step), walText(path), "")
		recordChanges(event, "current vs previous", previous, current)
		recordChanges(event, "file vs base", baseHeader, file)
		counter, valid := counters(current)
		currentCounters = append(currentCounters, counter)
		set.Check("C", counter == valid, fmt.Sprintf("step %d: the current page 1 has 24-27 equal to 92-95 (%d)", step, counter))
		fileUnchanged = fileUnchanged && bytes.Equal(file, baseHeader)
		previous = current
	}
	set.Check("C", currentCounters[0] == c0+1 && currentCounters[1] == c0+1 && currentCounters[2] == c0+1, fmt.Sprintf("WAL, one connection: every commit writes the same change counter %d (base + 1), it does not advance per commit", c0+1))
	set.Check("C", fileUnchanged, "WAL: the database file's page 1 does not change at commit; the new page 1 is a frame in the -wal")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	afterClose := readHeaderFromChild(path)
	recordChanges(newEvent("C", "C1", "after the last close", walText(path), ""), "file vs base", baseHeader, afterClose)
	set.Check("C", bytes.Equal(afterClose, previous), "the checkpoint at close copied the current page 1 into the file")

	path = filepath.Join(dir, "wal-no-page1.db")
	copyFile(base, path)
	db = must(r.Open(ctx, path))("open")
	start := logicalHeader(ctx, db)
	unchanged, eachWroteOneRowAndGrewWAL := true, true
	previousWAL := walSize(path)
	for i := 1; i <= 3; i++ {
		result, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1")
		if err != nil {
			fatal("update", err)
		}
		affected := must(result.RowsAffected())("rows affected")
		current := logicalHeader(ctx, db)
		recordChanges(newEvent("C", "C2", fmt.Sprintf("UPDATE commit %d", i), walText(path), ""), "current vs base", start, current)
		unchanged = unchanged && bytes.Equal(current, start)
		eachWroteOneRowAndGrewWAL = eachWroteOneRowAndGrewWAL && affected == 1 && walSize(path) > previousWAL
		previousWAL = walSize(path)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	afterClose = readHeaderFromChild(path)
	set.Check("C", eachWroteOneRowAndGrewWAL, "WAL: each of the three UPDATEs changed one row and appended frames to the -wal")
	set.Check("C", must(equal.FileSHA256(path))("hash") != must(equal.FileSHA256(base))("hash base"), "after the checkpoint at close the database file differs from the base")
	set.Check("C", unchanged && bytes.Equal(afterClose, baseHeader), "WAL: commits that do not write page 1 leave every header byte unchanged, even the change counter, before and after the checkpoint")

	path = filepath.Join(dir, "wal-new-connections.db")
	copyFile(base, path)
	holder := must(r.Open(ctx, path))("open holder")
	previous = logicalHeader(ctx, holder)
	var perSession []uint32
	for step := 28; step <= 30; step++ {
		writer := must(r.Open(ctx, path))("open writer")
		if err := r.ApplyStep(ctx, writer, step); err != nil {
			fatal("apply", err)
		}
		if err := writer.Close(); err != nil {
			fatal("close writer", err)
		}
		current := logicalHeader(ctx, holder)
		recordChanges(newEvent("C", "C3", fmt.Sprintf("step %d, new connection", step), walText(path), ""), "current vs previous", previous, current)
		counter, _ := counters(current)
		perSession = append(perSession, counter)
		previous = current
	}
	file := readHeaderFromChild(path)
	set.Check("C", bytes.Equal(file, baseHeader), "WAL: with another connection open, the file's page 1 still equals the base after three commits")
	set.Check("C", perSession[0] == c0+1 && perSession[1] == c0+2 && perSession[2] == c0+3, "WAL: each new connection reads page 1 afresh and writes counter + 1, so the counter advances once per connection that writes page 1")
	if err := holder.Close(); err != nil {
		fatal("close holder", err)
	}
	set.Check("C", bytes.Equal(readHeaderFromChild(path), previous) && walSize(path) == -1, "closing the last connection checkpointed page 1 into the file and removed the -wal")

	// Evicting page 1 from the connection's page cache before each commit forces
	// readDbPage (src/pager.c:3093-3110) to refresh dbFileVers, proving that C1's
	// constant counter is a page-cache effect rather than a per-connection rule.
	path = filepath.Join(dir, "wal-one-connection-shrink.db")
	copyFile(base, path)
	db = must(r.Open(ctx, path))("open")
	previous = logicalHeader(ctx, db)
	var shrunk []uint32
	for step := 28; step <= 30; step++ {
		if _, err := db.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
			fatal("shrink_memory", err)
		}
		if err := r.ApplyStep(ctx, db, step); err != nil {
			fatal("apply", err)
		}
		current := logicalHeader(ctx, db)
		recordChanges(newEvent("C", "C4", fmt.Sprintf("commit of step %d", step), walText(path), ""), "current vs previous", previous, current)
		counter, _ := counters(current)
		shrunk = append(shrunk, counter)
		previous = current
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	set.Check("C", shrunk[0] == c0+1 && shrunk[1] == c0+2 && shrunk[2] == c0+3, fmt.Sprintf("WAL, one connection, page 1 dropped from the cache before each commit: the counter advances once per commit (%d, %d, %d), so C1's constant counter comes from page 1 staying cached, not from the connection", c0+1, c0+2, c0+3))
}

func partD(ctx context.Context, dir string) {
	base := buildBase(ctx, dir, "switch.db", "delete", 3)
	before := recordBase("D", "switch.db", "delete", 3, base)
	plain := func(statement string) int {
		uri := must(sqlitex.FileURI(base, nil))("uri")
		db := must(sql.Open("sqlite", uri))("open")
		db.SetMaxOpenConns(1)
		mode := must(queryText(ctx, db, statement))(statement)
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		return newEvent("D", "D", statement, walText(base), "returns "+mode)
	}
	event := plain("PRAGMA journal_mode=WAL")
	afterWAL := readHeaderFromChild(base)
	recordChanges(event, "file vs before", before, afterWAL)
	set.Check("D", afterWAL[18] == 2 && afterWAL[19] == 2, "PRAGMA journal_mode=WAL sets bytes 18 and 19 to 2")
	event = plain("PRAGMA journal_mode=DELETE")
	afterDelete := readHeaderFromChild(base)
	recordChanges(event, "file vs WAL", afterWAL, afterDelete)
	set.Check("D", afterDelete[18] == 1 && afterDelete[19] == 1, "PRAGMA journal_mode=DELETE sets bytes 18 and 19 back to 1")
}

func partE(ctx context.Context, dir string) {
	// Part E tests lockBtree's in-header size rule (src/btree.c:3298-3302,
	// 3405-3412) on rollback-mode copies opened read-only so SQLite does not
	// repair or modify the edited files.
	base := buildBase(ctx, dir, "size-base.db", "delete", 3)
	image := must(os.ReadFile(base))("read")
	filePages := len(image) / 4096
	recordBase("E", "size-base.db", "delete", 3, base)
	sizes := set.Table("size_variants", "variant", "description", "in_header_size", "counters_disagree", "ours", "sqlite")
	variants := []struct {
		name     string
		size     uint32
		disagree bool
	}{
		{"in-header size = file pages - 1, counters agree", uint32(filePages - 1), false},
		{"in-header size = file pages - 1, byte 95 changed so counters disagree", uint32(filePages - 1), true},
		{"in-header size = file pages + 5, counters agree", uint32(filePages + 5), false},
		{"in-header size = 0, counters agree", 0, false},
	}
	for i, v := range variants {
		edited := append([]byte(nil), image...)
		binary.BigEndian.PutUint32(edited[28:32], v.size)
		if v.disagree {
			edited[95]++
		}
		path := filepath.Join(dir, fmt.Sprintf("size-%d.db", i+1))
		if err := os.WriteFile(path, edited, 0o644); err != nil {
			fatal("write", err)
		}
		ours := "error"
		f, err := dbfile.FromBytes(edited)
		if err == nil {
			ours = fmt.Sprintf("%d (from %s)", f.PageCount(), f.PageCountSource())
		} else if errors.Is(err, dbfile.ErrCorrupt) {
			ours = "error: in-header size larger than the file"
		}
		db := openReadOnly(path)
		theirs, qerr := queryText(ctx, db, "PRAGMA page_count")
		db.Close()
		if qerr != nil {
			theirs = "error: " + qerr.Error()
		}
		sizes.Row(fmt.Sprintf("E%d", i+1), v.name, v.size, v.disagree, ours, theirs)
		switch i {
		case 0:
			set.Check("E", theirs == fmt.Sprint(filePages-1) && f != nil && f.PageCount() == filePages-1, "agreeing counters: SQLite and our parser both trust the in-header size")
		case 1:
			set.Check("E", theirs == fmt.Sprint(filePages) && f != nil && f.PageCount() == filePages, "disagreeing counters: both ignore bytes 28-31 and use the file size")
		case 2:
			set.Check("E", qerr != nil && strings.Contains(qerr.Error(), "malformed") && errors.Is(err, dbfile.ErrCorrupt), "an agreeing in-header size larger than the file is corruption to both")
		case 3:
			set.Check("E", theirs == fmt.Sprint(filePages) && f != nil && f.PageCount() == filePages, "an in-header size of 0 is ignored by both")
		}
	}
}

func partF(ctx context.Context, dir string) {
	sizes := set.Table("page_sizes", "requested", "byte16", "byte17", "stored", "decoded", "sqlite_reports")
	for _, size := range []int{512, 4096, 65536} {
		path := filepath.Join(dir, fmt.Sprintf("page-size-%d.db", size))
		options := sqlitex.DefaultOptions()
		options.File.PageSize = size
		options.SingleConnection = true
		db := must(sqlitex.Open(ctx, path, options))("open")
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
			fatal("create", err)
		}
		pragma := must(queryText(ctx, db, "PRAGMA page_size"))("page_size")
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		raw := readHeaderFromChild(path)
		h := parse(raw)
		sizes.Row(size, fmt.Sprintf("%02x", raw[16]), fmt.Sprintf("%02x", raw[17]), binary.BigEndian.Uint16(raw[16:18]), h.PageSize, pragma)
		wantStored := uint16(size)
		if size == 65536 {
			wantStored = 1
		}
		set.Check("F", h.PageSize == size && fmt.Sprint(h.PageSize) == pragma && binary.BigEndian.Uint16(raw[16:18]) == wantStored, fmt.Sprintf("page size %d is stored as %d and decodes to the size asked for and to what SQLite reports", size, wantStored))
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == childReadHeader {
		childMain(os.Args[2])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("2.1")
	openTables(dir)
	var cli *sqlitex.CLI
	// Skip the .dbinfo oracle only when no CLI exists anywhere (ErrNoCLI); a
	// missing explicit $SQLITE3_BIN or a CLI with the wrong pin fails the run.
	located, err := sqlitex.LocateCLI(ctx)
	switch {
	case err == nil:
		cli = &located
		fmt.Println(located.PinLine())
		fmt.Printf("INFO: CLI found via %s\n", located.Origin)
	case errors.Is(err, sqlitex.ErrNoCLI):
		fmt.Printf("NO-CLI: %v; the .dbinfo cross-check is skipped\n", err)
	default:
		set.Check("A", false, fmt.Sprintf("the sqlite3 CLI that was found or named reports the pinned source id: %v", err))
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	partA(ctx, dir, cli)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	partF(ctx, dir)
	set.Finish("HEADER")
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

<!-- include: experiments/2.1-header-and-what-a-commit-changes/report.sql -->
```sql
-- Walkthrough of experiment 2.1. Run it with:
--   go run ./cmd/course run 2.1        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   r0              part A: one row, the R0 database the header is decoded from
--   r0_header       part A: one row per header field, as decoded by internal/dbfile
--   oracles         part A: one row per field, with SQLite's own report of it (NULL: no oracle)
--   bases           parts B-E: one row per base database a scenario starts from
--   events          parts B-D: one row per commit, close or statement the header is read after
--   header_changes  parts B-D: one row per header field that differs between two copies of page 1
--                   (one row with no field: no header byte changed)
--   size_variants   part E: one row per edited copy of a database, with both page counts
--   page_sizes      part F: one row per page size
--   checks          every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  2.1  What is in the 100-byte header, and what does a commit change?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The first 100 bytes of a database file are the header. The experiment'
.print 'decodes it field by field with internal/dbfile and compares every field'
.print 'with what SQLite itself reports. Then it reads page 1 after commits, in'
.print 'rollback mode and in WAL mode, to see which header bytes move and when.'
.print 'In WAL mode two copies of page 1 exist: "current" is the view of SQLite'
.print '(sqlite_dbpage, WAL included) and "file" is the bytes in the database'
.print 'file, read by a child process.'

.print
.print '── A. Decode the header of the R0 database ─────────────────────────────'
.print
.print 'A1. The R0 database: the recipe, the page count we derive and where it comes from.'

SELECT recipe, sha256 FROM r0;

SELECT page_count, page_count_source, in_header_size_valid FROM r0;

.print
.print 'A2. The 100 bytes, decoded by internal/dbfile.'

SELECT byte_offset, size, field, value FROM r0_header ORDER BY byte_offset;

.print
.print 'A3. Every decoded field against SQLite: PRAGMA through the Go driver and the'
.print '.dbinfo of the pinned sqlite3 CLI. "-" means that oracle does not report the field.'

SELECT field,
       ours,
       coalesce(pragma, '-')                  AS pragma,
       coalesce(CAST(dbinfo AS VARCHAR), '-') AS dbinfo,
       CASE WHEN pragma IS NULL AND dbinfo IS NULL THEN 'n/a'
            ELSE CAST((pragma IS NULL OR pragma = ours)
                  AND (dbinfo IS NULL OR CAST(dbinfo AS VARCHAR) = ours) AS VARCHAR)
       END                                    AS match
FROM oracles
ORDER BY seq;

.print
.print '── B. Rollback mode: what one commit changes in page 1 ─────────────────'
.print
.print 'B1. Steps 28, 29 and 30 on one connection; page 1 is read from the database'
.print 'file after each commit. Each commit advances the file change counter (24-27)'
.print 'and version-valid-for (92-95) by one, and moves the size, freelist and'
.print 'user-version fields.'
.print
.print 'The base database, built to step 27 and closed:'

SELECT name, journal_mode, built_to_step, write_version AS byte18, read_version AS byte19,
       change_counter, in_header_size, file_pages
FROM bases WHERE part = 'B' ORDER BY name;

.print
.print 'The header fields that differ from the page 1 before the commit:'

SELECT e.event,
       c.view,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       coalesce(c.field, 'no header byte changed')               AS field,
       c.before_value AS before,
       c.after_value  AS after,
       c.changed_bytes
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.part = 'B'
ORDER BY c.event_seq, c.byte_offset;

.print
.print '── C. WAL mode (the platform default): what changes in page 1, and when ──'
.print
.print 'The base database, built to step 27 and closed:'

SELECT name, journal_mode, built_to_step, write_version AS byte18, read_version AS byte19,
       change_counter, in_header_size, file_pages
FROM bases WHERE part = 'C' ORDER BY name;

.print
.print 'C1. Steps 28, 29, 30 on one connection; each step writes page 1 (user_version).'
.print 'Look at the "current vs previous" rows: the change counter moves at step 28 and'
.print 'then stays put, although page 1 is rewritten each time. The "file vs base"'
.print 'rows show page 1 of the database file unchanged until the last connection'
.print 'closes and the checkpoint copies the current page 1 into it.'
.print
.print 'The commits and the size of the -wal after each:'

SELECT event, wal_state FROM events WHERE scenario = 'C1' ORDER BY seq;

.print
.print 'The header fields that differ, by view:'

SELECT e.event,
       c.view,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       coalesce(c.field, 'no header byte changed')               AS field,
       c.before_value AS before,
       c.after_value  AS after,
       c.changed_bytes
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C1'
ORDER BY c.event_seq, c.view, c.byte_offset;

.print
.print 'C2. Three commits on one connection that do not touch page 1 (UPDATE one'
.print 'account balance). The -wal grows, the header does not change: not even the'
.print 'change counter, before or after the checkpoint at close.'

SELECT e.event, e.wal_state, c.view, coalesce(c.field, 'no header byte changed') AS field
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C2'
ORDER BY c.event_seq, c.byte_offset;

.print
.print 'C3. Steps 28, 29, 30, each on a new connection, while a second idle connection'
.print 'stays open so that closing a writer does not checkpoint. Now the counter'
.print 'advances at every step: each new connection reads page 1 afresh and writes'
.print 'counter + 1.'

SELECT e.event,
       e.wal_state,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       c.field,
       c.before_value AS before,
       c.after_value  AS after
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C3'
ORDER BY c.event_seq, c.byte_offset;

.print
.print 'C4. Steps 28, 29, 30 on one connection again, with PRAGMA shrink_memory before'
.print 'each step. It drops page 1 from the cache of the connection, and the counter'
.print 'advances once per commit: the constant counter of C1 comes from page 1 staying'
.print 'cached, not from the connection.'

SELECT e.event,
       e.wal_state,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       c.field,
       c.before_value AS before,
       c.after_value  AS after
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.scenario = 'C4'
ORDER BY c.event_seq, c.byte_offset;

.print
.print '── D. Bytes 18-19: switching journal mode rewrites the header ──────────'
.print
.print 'The base database is in rollback mode, built to step 3. Each statement runs'
.print 'on its own connection that is then closed, and no -wal file exists. The'
.print 'table shows what the file header differs by afterwards.'

SELECT name, journal_mode, built_to_step, write_version AS byte18, read_version AS byte19,
       change_counter, in_header_size, file_pages
FROM bases WHERE part = 'D' ORDER BY name;

SELECT e.event,
       e.detail,
       c.view,
       CASE WHEN c.size = 1 THEN CAST(c.byte_offset AS VARCHAR)
            ELSE c.byte_offset || '-' || (c.byte_offset + c.size - 1) END AS bytes,
       c.field,
       c.before_value AS before,
       c.after_value  AS after
FROM header_changes c JOIN events e ON e.seq = c.event_seq
WHERE e.part = 'D'
ORDER BY c.event_seq, c.byte_offset;

.print
.print '── E. The in-header size (bytes 28-31) is used only when 24-27 equal 92-95 ──'
.print
.print 'Four edited copies of a rollback-mode database, opened read-only. The page'
.print 'count of each according to our parser and to SQLite (PRAGMA page_count).'
.print
.print 'The base database, built to step 3:'

SELECT name, journal_mode, built_to_step, change_counter, in_header_size, file_pages
FROM bases WHERE part = 'E' ORDER BY name;

SELECT variant, description, in_header_size
FROM size_variants ORDER BY variant;

.print
.print 'The page count each of them reports:'

SELECT variant, ours AS "our parser", sqlite AS "SQLite page_count"
FROM size_variants ORDER BY variant;

.print
.print '── F. The page size lives in bytes 16-17; 65536 is stored as 1 ───────────'
.print
.print '65536 does not fit in two bytes. Each row is a new database created with'
.print 'PRAGMA page_size = requested; bytes 16 and 17 are shown in hex.'

SELECT requested, byte16, byte17, stored, decoded, sqlite_reports
FROM page_sizes ORDER BY requested;
```

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/2.1-header-and-what-a-commit-changes/`](evidence/2.1-header-and-what-a-commit-changes/), and `make check ARGS='evidence ch02'` compares a fresh run with them byte for byte.

<!-- include: evidence/2.1-header-and-what-a-commit-changes.txt -->
```text

════════════════════════════════════════════════════════════════════════
  2.1  What is in the 100-byte header, and what does a commit change?
════════════════════════════════════════════════════════════════════════

The first 100 bytes of a database file are the header. The experiment
decodes it field by field with internal/dbfile and compares every field
with what SQLite itself reports. Then it reads page 1 after commits, in
rollback mode and in WAL mode, to see which header bytes move and when.
In WAL mode two copies of page 1 exist: "current" is the view of SQLite
(sqlite_dbpage, WAL included) and "file" is the bytes in the database
file, read by a child process.

── A. Decode the header of the R0 database ─────────────────────────────

A1. The R0 database: the recipe, the page count we derive and where it comes from.
┌──────────────────────────────────┬──────────────────────────────────────────────────────────────────┐
│              recipe              │                              sha256                              │
├──────────────────────────────────┼──────────────────────────────────────────────────────────────────┤
│ seed=1 steps=30 journal_mode=wal │ 24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74 │
└──────────────────────────────────┴──────────────────────────────────────────────────────────────────┘
┌────────────┬───────────────────┬──────────────────────┐
│ page_count │ page_count_source │ in_header_size_valid │
├────────────┼───────────────────┼──────────────────────┤
│ 270        │ in-header size    │ true                 │
└────────────┴───────────────────┴──────────────────────┘

A2. The 100 bytes, decoded by internal/dbfile.
┌─────────────┬──────┬─────────────────────────────────┬───────────────────────┐
│ byte_offset │ size │              field              │         value         │
├─────────────┼──────┼─────────────────────────────────┼───────────────────────┤
│ 0           │ 16   │ magic                           │ "SQLite format 3\x00" │
│ 16          │ 2    │ page size                       │ 4096                  │
│ 18          │ 1    │ write version                   │ 2                     │
│ 19          │ 1    │ read version                    │ 2                     │
│ 20          │ 1    │ reserved bytes per page         │ 0                     │
│ 21          │ 1    │ max embedded payload fraction   │ 64                    │
│ 22          │ 1    │ min embedded payload fraction   │ 32                    │
│ 23          │ 1    │ leaf payload fraction           │ 32                    │
│ 24          │ 4    │ file change counter             │ 32                    │
│ 28          │ 4    │ database size in pages          │ 270                   │
│ 32          │ 4    │ first freelist trunk page       │ 230                   │
│ 36          │ 4    │ freelist page count             │ 3                     │
│ 40          │ 4    │ schema cookie                   │ 4                     │
│ 44          │ 4    │ schema format                   │ 4                     │
│ 48          │ 4    │ default cache size              │ 0                     │
│ 52          │ 4    │ largest root page (auto-vacuum) │ 0                     │
│ 56          │ 4    │ text encoding                   │ 1 (UTF-8)             │
│ 60          │ 4    │ user version                    │ 30                    │
│ 64          │ 4    │ incremental vacuum              │ 0                     │
│ 68          │ 4    │ application id                  │ 1397836120            │
│ 72          │ 20   │ reserved for expansion          │ all zero              │
│ 92          │ 4    │ version-valid-for               │ 32                    │
│ 96          │ 4    │ SQLite version number           │ 3053004 (3.53.4)      │
└─────────────┴──────┴─────────────────────────────────┴───────────────────────┘

A3. Every decoded field against SQLite: PRAGMA through the Go driver and the
.dbinfo of the pinned sqlite3 CLI. "-" means that oracle does not report the field.
┌───────────────────────┬────────────┬────────────┬────────────┬───────┐
│         field         │    ours    │   pragma   │   dbinfo   │ match │
├───────────────────────┼────────────┼────────────┼────────────┼───────┤
│ page size             │ 4096       │ 4096       │ 4096       │ true  │
│ write version         │ 2          │ -          │ 2          │ true  │
│ read version          │ 2          │ -          │ 2          │ true  │
│ reserved bytes        │ 0          │ -          │ 0          │ true  │
│ file change counter   │ 32         │ -          │ 32         │ true  │
│ page count            │ 270        │ 270        │ 270        │ true  │
│ freelist page count   │ 3          │ 3          │ 3          │ true  │
│ schema cookie         │ 4          │ 4          │ 4          │ true  │
│ schema format         │ 4          │ -          │ 4          │ true  │
│ default cache size    │ 0          │ -          │ 0          │ true  │
│ auto_vacuum           │ NONE       │ NONE       │ -          │ true  │
│ largest root page     │ 0          │ -          │ 0          │ true  │
│ incremental vacuum    │ 0          │ -          │ 0          │ true  │
│ text encoding         │ UTF-8      │ UTF-8      │ -          │ true  │
│ text encoding number  │ 1          │ -          │ 1          │ true  │
│ user version          │ 30         │ 30         │ 30         │ true  │
│ application id        │ 1397836120 │ 1397836120 │ 1397836120 │ true  │
│ version-valid-for     │ 32         │ -          │ -          │ n/a   │
│ SQLite version number │ 3053004    │ -          │ 3053004    │ true  │
└───────────────────────┴────────────┴────────────┴────────────┴───────┘

── B. Rollback mode: what one commit changes in page 1 ─────────────────

B1. Steps 28, 29 and 30 on one connection; page 1 is read from the database
file after each commit. Each commit advances the file change counter (24-27)
and version-valid-for (92-95) by one, and moves the size, freelist and
user-version fields.

The base database, built to step 27 and closed:
┌──────────────────┬──────────────┬───────────────┬────────┬────────┬────────────────┬────────────────┬────────────┐
│       name       │ journal_mode │ built_to_step │ byte18 │ byte19 │ change_counter │ in_header_size │ file_pages │
├──────────────────┼──────────────┼───────────────┼────────┼────────┼────────────────┼────────────────┼────────────┤
│ rollback-base.db │ delete       │ 27            │ 1      │ 1      │ 30             │ 244            │ 244        │
└──────────────────┴──────────────┴───────────────┴────────┴────────┴────────────────┴────────────────┴────────────┘

The header fields that differ from the page 1 before the commit:
┌───────────────────┬──────────────────┬───────┬───────────────────────────┬────────┬───────┬───────────────┐
│       event       │       view       │ bytes │           field           │ before │ after │ changed_bytes │
├───────────────────┼──────────────────┼───────┼───────────────────────────┼────────┼───────┼───────────────┤
│ commit of step 28 │ file vs previous │ 24-27 │ file change counter       │ 30     │ 31    │ 27            │
│ commit of step 28 │ file vs previous │ 28-31 │ database size in pages    │ 244    │ 251   │ 31            │
│ commit of step 28 │ file vs previous │ 32-35 │ first freelist trunk page │ 82     │ 131   │ 35            │
│ commit of step 28 │ file vs previous │ 60-63 │ user version              │ 27     │ 28    │ 63            │
│ commit of step 28 │ file vs previous │ 92-95 │ version-valid-for         │ 30     │ 31    │ 95            │
│ commit of step 29 │ file vs previous │ 24-27 │ file change counter       │ 31     │ 32    │ 27            │
│ commit of step 29 │ file vs previous │ 28-31 │ database size in pages    │ 251    │ 259   │ 30 31         │
│ commit of step 29 │ file vs previous │ 32-35 │ first freelist trunk page │ 131    │ 0     │ 35            │
│ commit of step 29 │ file vs previous │ 36-39 │ freelist page count       │ 1      │ 0     │ 39            │
│ commit of step 29 │ file vs previous │ 60-63 │ user version              │ 28     │ 29    │ 63            │
│ commit of step 29 │ file vs previous │ 92-95 │ version-valid-for         │ 31     │ 32    │ 95            │
│ commit of step 30 │ file vs previous │ 24-27 │ file change counter       │ 32     │ 33    │ 27            │
│ commit of step 30 │ file vs previous │ 28-31 │ database size in pages    │ 259    │ 270   │ 31            │
│ commit of step 30 │ file vs previous │ 32-35 │ first freelist trunk page │ 0      │ 230   │ 35            │
│ commit of step 30 │ file vs previous │ 36-39 │ freelist page count       │ 0      │ 3     │ 39            │
│ commit of step 30 │ file vs previous │ 60-63 │ user version              │ 29     │ 30    │ 63            │
│ commit of step 30 │ file vs previous │ 92-95 │ version-valid-for         │ 32     │ 33    │ 95            │
└───────────────────┴──────────────────┴───────┴───────────────────────────┴────────┴───────┴───────────────┘

── C. WAL mode (the platform default): what changes in page 1, and when ──

The base database, built to step 27 and closed:
┌─────────────┬──────────────┬───────────────┬────────┬────────┬────────────────┬────────────────┬────────────┐
│    name     │ journal_mode │ built_to_step │ byte18 │ byte19 │ change_counter │ in_header_size │ file_pages │
├─────────────┼──────────────┼───────────────┼────────┼────────┼────────────────┼────────────────┼────────────┤
│ wal-base.db │ wal          │ 27            │ 2      │ 2      │ 29             │ 244            │ 244        │
└─────────────┴──────────────┴───────────────┴────────┴────────┴────────────────┴────────────────┴────────────┘

C1. Steps 28, 29, 30 on one connection; each step writes page 1 (user_version).
Look at the "current vs previous" rows: the change counter moves at step 28 and
then stays put, although page 1 is rewritten each time. The "file vs base"
rows show page 1 of the database file unchanged until the last connection
closes and the checkpoint copies the current page 1 into it.

The commits and the size of the -wal after each:
┌──────────────────────┬───────────────────┐
│        event         │     wal_state     │
├──────────────────────┼───────────────────┤
│ commit of step 28    │ -wal 82432 bytes  │
│ commit of step 29    │ -wal 156592 bytes │
│ commit of step 30    │ -wal 280192 bytes │
│ after the last close │ no -wal file      │
└──────────────────────┴───────────────────┘

The header fields that differ, by view:
┌──────────────────────┬─────────────────────┬───────┬───────────────────────────┬────────┬───────┬───────────────┐
│        event         │        view         │ bytes │           field           │ before │ after │ changed_bytes │
├──────────────────────┼─────────────────────┼───────┼───────────────────────────┼────────┼───────┼───────────────┤
│ commit of step 28    │ current vs previous │ 24-27 │ file change counter       │ 29     │ 30    │ 27            │
│ commit of step 28    │ current vs previous │ 28-31 │ database size in pages    │ 244    │ 251   │ 31            │
│ commit of step 28    │ current vs previous │ 32-35 │ first freelist trunk page │ 82     │ 131   │ 35            │
│ commit of step 28    │ current vs previous │ 60-63 │ user version              │ 27     │ 28    │ 63            │
│ commit of step 28    │ current vs previous │ 92-95 │ version-valid-for         │ 29     │ 30    │ 95            │
│ commit of step 28    │ file vs base        │       │ no header byte changed    │        │       │               │
│ commit of step 29    │ current vs previous │ 28-31 │ database size in pages    │ 251    │ 259   │ 30 31         │
│ commit of step 29    │ current vs previous │ 32-35 │ first freelist trunk page │ 131    │ 0     │ 35            │
│ commit of step 29    │ current vs previous │ 36-39 │ freelist page count       │ 1      │ 0     │ 39            │
│ commit of step 29    │ current vs previous │ 60-63 │ user version              │ 28     │ 29    │ 63            │
│ commit of step 29    │ file vs base        │       │ no header byte changed    │        │       │               │
│ commit of step 30    │ current vs previous │ 28-31 │ database size in pages    │ 259    │ 270   │ 31            │
│ commit of step 30    │ current vs previous │ 32-35 │ first freelist trunk page │ 0      │ 230   │ 35            │
│ commit of step 30    │ current vs previous │ 36-39 │ freelist page count       │ 0      │ 3     │ 39            │
│ commit of step 30    │ current vs previous │ 60-63 │ user version              │ 29     │ 30    │ 63            │
│ commit of step 30    │ file vs base        │       │ no header byte changed    │        │       │               │
│ after the last close │ file vs base        │ 24-27 │ file change counter       │ 29     │ 30    │ 27            │
│ after the last close │ file vs base        │ 28-31 │ database size in pages    │ 244    │ 270   │ 30 31         │
│ after the last close │ file vs base        │ 32-35 │ first freelist trunk page │ 82     │ 230   │ 35            │
│ after the last close │ file vs base        │ 36-39 │ freelist page count       │ 1      │ 3     │ 39            │
│ after the last close │ file vs base        │ 60-63 │ user version              │ 27     │ 30    │ 63            │
│ after the last close │ file vs base        │ 92-95 │ version-valid-for         │ 29     │ 30    │ 95            │
└──────────────────────┴─────────────────────┴───────┴───────────────────────────┴────────┴───────┴───────────────┘

C2. Three commits on one connection that do not touch page 1 (UPDATE one
account balance). The -wal grows, the header does not change: not even the
change counter, before or after the checkpoint at close.
┌─────────────────┬──────────────────┬─────────────────┬────────────────────────┐
│      event      │    wal_state     │      view       │         field          │
├─────────────────┼──────────────────┼─────────────────┼────────────────────────┤
│ UPDATE commit 1 │ -wal 4152 bytes  │ current vs base │ no header byte changed │
│ UPDATE commit 2 │ -wal 8272 bytes  │ current vs base │ no header byte changed │
│ UPDATE commit 3 │ -wal 12392 bytes │ current vs base │ no header byte changed │
└─────────────────┴──────────────────┴─────────────────┴────────────────────────┘

C3. Steps 28, 29, 30, each on a new connection, while a second idle connection
stays open so that closing a writer does not checkpoint. Now the counter
advances at every step: each new connection reads page 1 afresh and writes
counter + 1.
┌─────────────────────────┬───────────────────┬───────┬───────────────────────────┬────────┬───────┐
│          event          │     wal_state     │ bytes │           field           │ before │ after │
├─────────────────────────┼───────────────────┼───────┼───────────────────────────┼────────┼───────┤
│ step 28, new connection │ -wal 82432 bytes  │ 24-27 │ file change counter       │ 29     │ 30    │
│ step 28, new connection │ -wal 82432 bytes  │ 28-31 │ database size in pages    │ 244    │ 251   │
│ step 28, new connection │ -wal 82432 bytes  │ 32-35 │ first freelist trunk page │ 82     │ 131   │
│ step 28, new connection │ -wal 82432 bytes  │ 60-63 │ user version              │ 27     │ 28    │
│ step 28, new connection │ -wal 82432 bytes  │ 92-95 │ version-valid-for         │ 29     │ 30    │
│ step 29, new connection │ -wal 156592 bytes │ 24-27 │ file change counter       │ 30     │ 31    │
│ step 29, new connection │ -wal 156592 bytes │ 28-31 │ database size in pages    │ 251    │ 259   │
│ step 29, new connection │ -wal 156592 bytes │ 32-35 │ first freelist trunk page │ 131    │ 0     │
│ step 29, new connection │ -wal 156592 bytes │ 36-39 │ freelist page count       │ 1      │ 0     │
│ step 29, new connection │ -wal 156592 bytes │ 60-63 │ user version              │ 28     │ 29    │
│ step 29, new connection │ -wal 156592 bytes │ 92-95 │ version-valid-for         │ 30     │ 31    │
│ step 30, new connection │ -wal 280192 bytes │ 24-27 │ file change counter       │ 31     │ 32    │
│ step 30, new connection │ -wal 280192 bytes │ 28-31 │ database size in pages    │ 259    │ 270   │
│ step 30, new connection │ -wal 280192 bytes │ 32-35 │ first freelist trunk page │ 0      │ 230   │
│ step 30, new connection │ -wal 280192 bytes │ 36-39 │ freelist page count       │ 0      │ 3     │
│ step 30, new connection │ -wal 280192 bytes │ 60-63 │ user version              │ 29     │ 30    │
│ step 30, new connection │ -wal 280192 bytes │ 92-95 │ version-valid-for         │ 31     │ 32    │
└─────────────────────────┴───────────────────┴───────┴───────────────────────────┴────────┴───────┘

C4. Steps 28, 29, 30 on one connection again, with PRAGMA shrink_memory before
each step. It drops page 1 from the cache of the connection, and the counter
advances once per commit: the constant counter of C1 comes from page 1 staying
cached, not from the connection.
┌───────────────────┬───────────────────┬───────┬───────────────────────────┬────────┬───────┐
│       event       │     wal_state     │ bytes │           field           │ before │ after │
├───────────────────┼───────────────────┼───────┼───────────────────────────┼────────┼───────┤
│ commit of step 28 │ -wal 82432 bytes  │ 24-27 │ file change counter       │ 29     │ 30    │
│ commit of step 28 │ -wal 82432 bytes  │ 28-31 │ database size in pages    │ 244    │ 251   │
│ commit of step 28 │ -wal 82432 bytes  │ 32-35 │ first freelist trunk page │ 82     │ 131   │
│ commit of step 28 │ -wal 82432 bytes  │ 60-63 │ user version              │ 27     │ 28    │
│ commit of step 28 │ -wal 82432 bytes  │ 92-95 │ version-valid-for         │ 29     │ 30    │
│ commit of step 29 │ -wal 156592 bytes │ 24-27 │ file change counter       │ 30     │ 31    │
│ commit of step 29 │ -wal 156592 bytes │ 28-31 │ database size in pages    │ 251    │ 259   │
│ commit of step 29 │ -wal 156592 bytes │ 32-35 │ first freelist trunk page │ 131    │ 0     │
│ commit of step 29 │ -wal 156592 bytes │ 36-39 │ freelist page count       │ 1      │ 0     │
│ commit of step 29 │ -wal 156592 bytes │ 60-63 │ user version              │ 28     │ 29    │
│ commit of step 29 │ -wal 156592 bytes │ 92-95 │ version-valid-for         │ 30     │ 31    │
│ commit of step 30 │ -wal 280192 bytes │ 24-27 │ file change counter       │ 31     │ 32    │
│ commit of step 30 │ -wal 280192 bytes │ 28-31 │ database size in pages    │ 259    │ 270   │
│ commit of step 30 │ -wal 280192 bytes │ 32-35 │ first freelist trunk page │ 0      │ 230   │
│ commit of step 30 │ -wal 280192 bytes │ 36-39 │ freelist page count       │ 0      │ 3     │
│ commit of step 30 │ -wal 280192 bytes │ 60-63 │ user version              │ 29     │ 30    │
│ commit of step 30 │ -wal 280192 bytes │ 92-95 │ version-valid-for         │ 31     │ 32    │
└───────────────────┴───────────────────┴───────┴───────────────────────────┴────────┴───────┘

── D. Bytes 18-19: switching journal mode rewrites the header ──────────

The base database is in rollback mode, built to step 3. Each statement runs
on its own connection that is then closed, and no -wal file exists. The
table shows what the file header differs by afterwards.
┌───────────┬──────────────┬───────────────┬────────┬────────┬────────────────┬────────────────┬────────────┐
│   name    │ journal_mode │ built_to_step │ byte18 │ byte19 │ change_counter │ in_header_size │ file_pages │
├───────────┼──────────────┼───────────────┼────────┼────────┼────────────────┼────────────────┼────────────┤
│ switch.db │ delete       │ 3             │ 1      │ 1      │ 6              │ 39             │ 39         │
└───────────┴──────────────┴───────────────┴────────┴────────┴────────────────┴────────────────┴────────────┘
┌────────────────────────────┬────────────────┬────────────────┬───────┬─────────────────────┬────────┬───────┐
│           event            │     detail     │      view      │ bytes │        field        │ before │ after │
├────────────────────────────┼────────────────┼────────────────┼───────┼─────────────────────┼────────┼───────┤
│ PRAGMA journal_mode=WAL    │ returns wal    │ file vs before │ 18    │ write version       │ 1      │ 2     │
│ PRAGMA journal_mode=WAL    │ returns wal    │ file vs before │ 19    │ read version        │ 1      │ 2     │
│ PRAGMA journal_mode=WAL    │ returns wal    │ file vs before │ 24-27 │ file change counter │ 6      │ 7     │
│ PRAGMA journal_mode=WAL    │ returns wal    │ file vs before │ 92-95 │ version-valid-for   │ 6      │ 7     │
│ PRAGMA journal_mode=DELETE │ returns delete │ file vs WAL    │ 18    │ write version       │ 2      │ 1     │
│ PRAGMA journal_mode=DELETE │ returns delete │ file vs WAL    │ 19    │ read version        │ 2      │ 1     │
│ PRAGMA journal_mode=DELETE │ returns delete │ file vs WAL    │ 24-27 │ file change counter │ 7      │ 8     │
│ PRAGMA journal_mode=DELETE │ returns delete │ file vs WAL    │ 92-95 │ version-valid-for   │ 7      │ 8     │
└────────────────────────────┴────────────────┴────────────────┴───────┴─────────────────────┴────────┴───────┘

── E. The in-header size (bytes 28-31) is used only when 24-27 equal 92-95 ──

Four edited copies of a rollback-mode database, opened read-only. The page
count of each according to our parser and to SQLite (PRAGMA page_count).

The base database, built to step 3:
┌──────────────┬──────────────┬───────────────┬────────────────┬────────────────┬────────────┐
│     name     │ journal_mode │ built_to_step │ change_counter │ in_header_size │ file_pages │
├──────────────┼──────────────┼───────────────┼────────────────┼────────────────┼────────────┤
│ size-base.db │ delete       │ 3             │ 6              │ 39             │ 39         │
└──────────────┴──────────────┴───────────────┴────────────────┴────────────────┴────────────┘
┌─────────┬───────────────────────────────────────────────────────────────────────┬────────────────┐
│ variant │                              description                              │ in_header_size │
├─────────┼───────────────────────────────────────────────────────────────────────┼────────────────┤
│ E1      │ in-header size = file pages - 1, counters agree                       │ 38             │
│ E2      │ in-header size = file pages - 1, byte 95 changed so counters disagree │ 38             │
│ E3      │ in-header size = file pages + 5, counters agree                       │ 44             │
│ E4      │ in-header size = 0, counters agree                                    │ 0              │
└─────────┴───────────────────────────────────────────────────────────────────────┴────────────────┘

The page count each of them reports:
┌─────────┬────────────────────────────────────────────┬──────────────────────────────────────────────┐
│ variant │                 our parser                 │              SQLite page_count               │
├─────────┼────────────────────────────────────────────┼──────────────────────────────────────────────┤
│ E1      │ 38 (from in-header size)                   │ 38                                           │
│ E2      │ 39 (from file size)                        │ 39                                           │
│ E3      │ error: in-header size larger than the file │ error: database disk image is malformed (11) │
│ E4      │ 39 (from file size)                        │ 39                                           │
└─────────┴────────────────────────────────────────────┴──────────────────────────────────────────────┘

── F. The page size lives in bytes 16-17; 65536 is stored as 1 ───────────

65536 does not fit in two bytes. Each row is a new database created with
PRAGMA page_size = requested; bytes 16 and 17 are shown in hex.
┌───────────┬────────┬────────┬────────┬─────────┬────────────────┐
│ requested │ byte16 │ byte17 │ stored │ decoded │ sqlite_reports │
├───────────┼────────┼────────┼────────┼─────────┼────────────────┤
│ 512       │ 02     │ 00     │ 512    │ 512     │ 512            │
│ 4096      │ 10     │ 00     │ 4096   │ 4096    │ 4096           │
│ 65536     │ 00     │ 01     │ 1      │ 65536   │ 65536          │
└───────────┴────────┴────────┴────────┴─────────┴────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  opening R0, reading the pragmas and closing left it byte-identical and at rest
A  ✓  each of the 18 decoded header fields that has an oracle here (PRAGMA or .dbinfo) equals SQLite's own report of it; 1 field(s) have none (n/a)
A  ✓  bytes 96-99 read 3053004 (the library that last wrote the file)
A  ✓  bytes 18 and 19 are 2 and 2 in a WAL-mode file
A  ✓  bytes 24-27 equal 92-95, so the in-header size at 28 is used
B  ✓  after step 28 the file's page 1 equals SQLite's current page 1 (no -wal in rollback mode)
B  ✓  after step 29 the file's page 1 equals SQLite's current page 1 (no -wal in rollback mode)
B  ✓  after step 30 the file's page 1 equals SQLite's current page 1 (no -wal in rollback mode)
B  ✓  rollback mode: bytes 24-27 and 92-95 each advance by exactly one per commit, on one connection
B  ✓  closing the connection changed nothing in page 1
C  ✓  step 28: the current page 1 has 24-27 equal to 92-95 (30)
C  ✓  step 29: the current page 1 has 24-27 equal to 92-95 (30)
C  ✓  step 30: the current page 1 has 24-27 equal to 92-95 (30)
C  ✓  WAL, one connection: every commit writes the same change counter 30 (base + 1), it does not advance per commit
C  ✓  WAL: the database file's page 1 does not change at commit; the new page 1 is a frame in the -wal
C  ✓  the checkpoint at close copied the current page 1 into the file
C  ✓  WAL: each of the three UPDATEs changed one row and appended frames to the -wal
C  ✓  after the checkpoint at close the database file differs from the base
C  ✓  WAL: commits that do not write page 1 leave every header byte unchanged, even the change counter, before and after the checkpoint
C  ✓  WAL: with another connection open, the file's page 1 still equals the base after three commits
C  ✓  WAL: each new connection reads page 1 afresh and writes counter + 1, so the counter advances once per connection that writes page 1
C  ✓  closing the last connection checkpointed page 1 into the file and removed the -wal
C  ✓  WAL, one connection, page 1 dropped from the cache before each commit: the counter advances once per commit (30, 31, 32), so C1's constant counter comes from page 1 staying cached, not from the connection
D  ✓  PRAGMA journal_mode=WAL sets bytes 18 and 19 to 2
D  ✓  PRAGMA journal_mode=DELETE sets bytes 18 and 19 back to 1
E  ✓  agreeing counters: SQLite and our parser both trust the in-header size
E  ✓  disagreeing counters: both ignore bytes 28-31 and use the file size
E  ✓  an agreeing in-header size larger than the file is corruption to both
E  ✓  an in-header size of 0 is ignored by both
F  ✓  page size 512 is stored as 512 and decodes to the size asked for and to what SQLite reports
F  ✓  page size 4096 is stored as 4096 and decodes to the size asked for and to what SQLite reports
F  ✓  page size 65536 is stored as 1 and decodes to the size asked for and to what SQLite reports

32 of 32 claims hold

Experiment verdict: HEADER OK
```

#### Reading the evidence

- **Pin and tools (the experiment's own output when run directly; not part of the DuckDB walkthrough).** Both the Go driver and the `sqlite3` CLI report the pinned 3.53.4 Fossil check-in (SQLite's source-control commit hash returned by `sqlite_source_id()`). The `INFO:` lines record where the CLI was found (the tools directory in the `lab` container) and where scratch files live (`/work/2.1` in the container, `_work/2.1` when run directly). If no CLI is installed anywhere, `LocateCLI` returns `sqlitex.ErrNoCLI` and the experiment prints `NO-CLI:`; a missing explicit `$SQLITE3_BIN` or a wrong-pin CLI fails the run with exit status 1.
- **Section A (Part A: full header decode of R0).** The 30-step WAL-mode R0 database hashes to `24aa158fdf713808…`, matching the step-30 R0 hash established in Chapter 1 (table A1). Table A2 lists all 23 fields covering bytes 0–99: page size `4096`, write/read versions `2/2` (WAL), change counter `32`, in-header database size `270`, first freelist trunk `230`, freelist page count `3`, schema cookie `4` (three `CREATE TABLE` statements in step 0 plus one `CREATE INDEX` in step 5), schema format `4`, `auto_vacuum` `NONE` (offsets 52 and 64 both `0`), text encoding `1 (UTF-8)`, `user_version` `30`, `application_id` `1397836120`, `version-valid-for` `32`, and SQLite version `3053004 (3.53.4)`. Because bytes 24–27 equal bytes 92–95 (`32 == 32`), the page count (`270`) comes from the in-header size. Opening R0 on a normal read-write connection to read the pragmas and closing it left the file byte-identical and at rest (a Part A check). In the oracle table (A3; verified in the checks list), 18 of 19 rows have an oracle (`PRAGMA` or `.dbinfo`) and print `true`, while `version-valid-for` has neither and prints `n/a`. Without a CLI, 8 rows are checked against `PRAGMA` and 11 print `n/a`.
- **B1 (Part B: rollback-mode commits).** Starting from a rollback-mode recipe at step 27 (change counter `30`), steps 28, 29, and 30 run on one connection. After each commit, the child process reads page 1 directly from `.db` and confirms it equals SQLite's `sqlite_dbpage` view. Every commit advances bytes 24–27 and bytes 92–95 by 1 (`30 -> 31 -> 32 -> 33`), updates database size (`244 -> 251 -> 259 -> 270`), updates freelist trunk and count as pages are reused and freed, and advances `user_version` (`27 -> 28 -> 29 -> 30`). Closing the connection changes nothing on page 1 (a Part B check).
- **Section C (Part C: WAL-mode commits).** Starting from a WAL-mode recipe at step 27 (change counter `29`), the experiment compares `current` (`sqlite_dbpage`, WAL included) against `file` (raw `.db` bytes read by a child process):
  - **C1 (one connection, steps 28–30):** Every recipe step writes `PRAGMA user_version`, dirtying page 1. Step 28 grows `-wal` to `82432` bytes and moves `current` bytes 24–27 and 92–95 from `29 -> 30`, while `file vs base` reads `no header byte changed`. Steps 29 and 30 grow `-wal` to `156592` and `280192` bytes and update size, freelist, and `user_version` in `current`, but because page 1 stays in the connection's page cache, bytes 24–27 and 92–95 hold at `30` (the `current vs previous` rows of steps 29 and 30). Only when the connection closes does the checkpoint copy page 1 into `.db` and delete `-wal`, leaving counter `30` (not `32`) in `.db` (the `after the last close` rows).
  - **C2 (one connection, three `UPDATE`s not touching page 1):** Each single-row update modifies one row and appends one 4,120-byte frame (`24`-byte frame header + `4096`-byte page) to `-wal` (`4152 -> 8272 -> 12392` bytes, after the 32-byte WAL header). After close, `.db`'s SHA-256 differs from the base, yet zero header bytes change in `current` or in `.db` before or after the checkpoint.
  - **C3 (new connection per step under an idle holder):** Each new writer calls `readDbPage` on page 1, refreshing `dbFileVers` from the latest WAL frame. The counter advances `29 -> 30 -> 31 -> 32` across the three connections while `.db` stays untouched until the idle holder closes.
  - **C4 (one connection with `PRAGMA shrink_memory` before each step):** Dropping unused pages—page 1 included—from the connection's cache before each step forces `readDbPage` to re-read page 1 from `-wal` and refresh `dbFileVers`. On that single connection, the counter advances on every commit (`29 -> 30 -> 31 -> 32`; `-wal` `82432 -> 156592 -> 280192`), proving C1's flat counter comes from page 1 staying cached rather than from staying on one connection.
- **Section D (Part D: bytes 18–19).** Switching a rollback database to WAL with `PRAGMA journal_mode=WAL` changes bytes 18 and 19 from `1` to `2` and increments bytes 24–27 and 92–95 (`6 -> 7`). Switching back with `PRAGMA journal_mode=DELETE` returns bytes 18 and 19 to `1` (`7 -> 8`).
- **Section E (Part E: in-header size rule).** On four edited copies of a 39-page rollback database: **E1** sets offset 28 to `38` with matching counters → both `internal/dbfile` and SQLite report `38`; **E2** increments byte 95 so counters disagree → both fall back to file size `39`; **E3** sets offset 28 to `44` with matching counters → both reject it (`in-header size larger than the file` vs `database disk image is malformed (11)`); **E4** sets offset 28 to `0` → both fall back to `39`.
- **Section F (Part F: page-size encoding).** `512` is stored as `02 00` (`512`); `4096` is stored as `10 00` (`4096`); `65536` is stored as `00 01` (`1`) and decodes to `65536` in both `internal/dbfile` and `PRAGMA page_size`.

---

## 4. Timeline

Comparing Parts B, C, and D of Experiment 2.1 gives the exact timeline of when header bytes change in the two journal modes.

### 4.1 Rollback mode (`journal_mode=delete`)

Every committed write transaction enters `pager_incr_changecounter` ([`src/pager.c:6363-6437`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6363-L6437)) during commit phase one, journals the old page 1, updates page 1 in place, and writes the new page 1 to `.db` before deleting `-journal`.

| Bytes | Field | When it changes |
| --- | --- | --- |
| `24–27` | File change counter | Every commit: `+1` (`dbFileVers + 1`, once per transaction) |
| `92–95` | Version-valid-for | Every commit: written equal to bytes `24–27` |
| `96–99` | SQLite version number | Every commit: rewritten with `3053004` (same value on one build) |
| `28–31` | Database size in pages | When pages are appended or truncated ([`allocateBtreePage`, `src/btree.c:6800`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6800)) |
| `32–35`, `36–39` | First freelist trunk, freelist count | When pages are freed (`freePage2`) or reused (`allocateBtreePage`) |
| `40–43` | Schema cookie | On DDL changes (R0 reaches `4`: three `CREATE TABLE` + one `CREATE INDEX`) |
| `60–63` | User version | When the application runs `PRAGMA user_version = N` (every recipe step) |
| `18–19` | Write/read format versions | On `PRAGMA journal_mode` transitions between rollback (`1,1`) and WAL (`2,2`) |

### 4.2 WAL mode (`journal_mode=wal`, the platform default)

In WAL mode, commits append frames to `-wal` and never write `.db`. Header bytes move on two separate timelines—the latest page-1 frame inside `-wal`, and the copy on disk inside `.db`—depending on whether page 1 was dirtied and whether page 1 was re-read into the connection's page cache ([`readDbPage`, `src/pager.c:3093-3110`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3093-L3110)). The sequence below traces how caching `dbFileVers` produces C1's flat counter and why evicting page 1 in C4 makes step 29 advance it, before the final close checkpoints page 1 into `.db`:

<!-- draw-visual: diagrams/ch02-file-format-and-pages-wal-changecounter.mmd -->
```text
┌───────────────────┐     ┌───────────────┐     ┌────────────┐
│ Conn (page cache) │     │ -wal (frames) │     │ .db (file) │
└─────────┬─────────┘     └───────┬───────┘     └──────┬─────┘
          │                       │                    │
         ┌──────────────────────────────────────────────┐
         │        Step 27 base: .db counter = 29        │
         └──────────────────────────────────────────────┘
          │                       │                    │
          │ 1. readDbPage(1)      │                    │
          ├───────────────────────────────────────────►│
          │                       │                    │
          │ 2. dbFileVers = 29    │                    │
          │◄┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┤
          │                       │                    │
         ┌──────────────────────────────────────────────┐
         │           C1: page 1 stays cached            │
         └──────────────────────────────────────────────┘
          │                       │                    │
          │ 3. step 28: pg 1 (30) │                    │
          ├──────────────────────►│                    │
          │                       │                    │
          │ 4. step 29: pg 1 (30) │                    │
          ├──────────────────────►│                    │
          │                       │                    │
         ┌──────────────────────────────────────────────┐
         │     C4: PRAGMA shrink_memory evicts pg 1     │
         └──────────────────────────────────────────────┘
          │                       │                    │
          │ 5. readDbPage(1)      │                    │
          ├──────────────────────►│                    │
          │                       │                    │
          │ 6. dbFileVers = 30    │                    │
          │◄┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┤                    │
          │                       │                    │
          │ 7. step 29: pg 1 (31) │                    │
          ├──────────────────────►│                    │
          │                       │                    │
         ┌──────────────────────────────────────────────┐
         │            Last connection closes            │
         └──────────────────────────────────────────────┘
          │                       │                    │
          │                       │ 8. checkpoint pg 1 │
          │                       ├───────────────────►│
          │                       │                    │
```

| Event (from base counter `29` at step 27) | `-wal` page-1 counter (bytes 24–27 & 92–95) | `.db` file page-1 counter | `-wal` size |
| --- | --- | --- | --- |
| **C1: single connection, page-1 dirty (page 1 stays cached)** | | | |
| Commit step 28 (`user_version=28`) | `30` (`29 + 1`, `pagerWalFrames` calls `pager_write_changecounter`) | `29` (unchanged) | `82,432` B |
| Commit step 29 (`user_version=29`) | `30` (still `dbFileVers + 1`, `dbFileVers` was not refreshed) | `29` (unchanged) | `156,592` B |
| Commit step 30 (`user_version=30`) | `30` (still `29 + 1`) | `29` (unchanged) | `280,192` B |
| Last connection closes (checkpoint) | no `-wal` | `30` (copied from last page-1 frame) | deleted |
| **C2: single connection, page 1 untouched** | | | |
| Commit 1 (`UPDATE accounts ... id = 1`) | `29` (no page-1 frame written) | `29` (unchanged) | `4,152` B |
| Commit 2 (`UPDATE accounts ... id = 1`) | `29` (no page-1 frame written) | `29` (unchanged) | `8,272` B |
| Commit 3 (`UPDATE accounts ... id = 1`) | `29` (no page-1 frame written) | `29` (unchanged) | `12,392` B |
| Last connection closes (checkpoint) | no `-wal` | `29` (all 100 header bytes identical to base) | deleted |
| **C3: new connection per step (holder open)** | | | |
| Writer 1 commits step 28, closes | `30` (read `29`, wrote `30`) | `29` (holder blocks close-checkpoint) | `82,432` B |
| Writer 2 commits step 29, closes | `31` (read `30` from WAL, wrote `31`) | `29` (unchanged) | `156,592` B |
| Writer 3 commits step 30, closes | `32` (read `31` from WAL, wrote `32`) | `29` (unchanged) | `280,192` B |
| Holder closes (checkpoint) | no `-wal` | `32` (copied from last page-1 frame) | deleted |
| **C4: single connection, `PRAGMA shrink_memory` before each step** | | | |
| Shrink + commit step 28 (`user_version=28`) | `30` (`readDbPage` read `29`, wrote `30`) | not re-read in C4 | `82,432` B |
| Shrink + commit step 29 (`user_version=29`) | `31` (page 1 evicted; `readDbPage` read `30`, wrote `31`) | not re-read in C4 | `156,592` B |
| Shrink + commit step 30 (`user_version=30`) | `32` (page 1 evicted; `readDbPage` read `31`, wrote `32`) | not re-read in C4 | `280,192` B |

---

## 5. Lab

This section builds three shared Go packages (`internal/dbfile`, `internal/equal`, and `internal/capture`), verifies the page parser against SQLite's `dbstat` virtual table and `SELECT` across every corner of the format (Experiment 2.2), and completes Rebuild Ladder rung R1 (Experiment 2.3).

### 5.1 The `internal/dbfile` package

`internal/dbfile` parses SQLite database files in pure Go without importing any SQLite driver. Its unit tests and fuzz target live in [`internal/dbfile/dbfile_test.go`](../internal/dbfile/dbfile_test.go) (`make test PACKAGE=./internal/dbfile` from the repository root). The suite covers varint boundaries, four page sizes (`512`, `1024`, `4096`, `65536`), UTF-16 schemas, local payload boundaries, multi-trunk freelists, auto-vacuum pointer maps, `WITHOUT ROWID` tables, a sparse > 1 GiB lock-byte file, page-header and last-overflow-page corruption, `WALMode()` read-version handling, 3,000 single-bit flips plus 8 truncations (0 panics), and the `FuzzParse` fuzz target.

#### `internal/dbfile/errors.go`

<!-- include: ../internal/dbfile/errors.go -->
```go
package dbfile

import (
	"errors"
	"fmt"
)

// Sentinel errors returned inside FormatError so callers can classify malformed
// or truncated files with errors.Is.
var (
	// ErrNotADatabase reports an invalid magic string or header field value.
	ErrNotADatabase = errors.New("not an SQLite database file")
	// ErrTruncated reports a file shorter than the header or not a whole number of pages.
	ErrTruncated = errors.New("file is truncated")
	// ErrCorrupt reports an internal structural error inside the file, a page, a chain, or a tree.
	ErrCorrupt = errors.New("database file is malformed")
	// ErrUnsupported reports a valid format value this parser does not handle (for example, > 2^32-2 pages).
	ErrUnsupported = errors.New("unsupported database file feature")
	// ErrPageOutOfFile reports a page request outside 1..PageCount.
	ErrPageOutOfFile = errors.New("page number outside the file")
)

// FormatError describes a file-format violation along with the 1-based page
// number (0 for file-level errors) and byte offset (-1 when not byte-specific).
type FormatError struct {
	Kind    error
	Page    int
	Offset  int
	Problem string
}

// Error formats the error kind, page or file offset, and problem description.
func (e *FormatError) Error() string {
	location := "file"
	if e.Page > 0 {
		location = fmt.Sprintf("page %d", e.Page)
		if e.Offset >= 0 {
			location += fmt.Sprintf(" offset %d", e.Offset)
		}
	} else if e.Offset >= 0 {
		location = fmt.Sprintf("file offset %d", e.Offset)
	}
	return fmt.Sprintf("%s: %s: %s", e.Kind, location, e.Problem)
}

// Unwrap returns the underlying sentinel error (Kind) for errors.Is matching.
func (e *FormatError) Unwrap() error { return e.Kind }

func corruptAt(page, offset int, format string, args ...any) error {
	return &FormatError{Kind: ErrCorrupt, Page: page, Offset: offset, Problem: fmt.Sprintf(format, args...)}
}

func corruptPage(page int, format string, args ...any) error {
	return corruptAt(page, -1, format, args...)
}
```

`errors.go` defines five sentinel errors (`ErrNotADatabase`, `ErrTruncated`, `ErrCorrupt`, `ErrUnsupported`, `ErrPageOutOfFile`) and wraps them in `*FormatError` with the 1-based page number and byte offset so callers can both match error classes with `errors.Is` and report the exact broken location.

#### `internal/dbfile/varint.go`

<!-- include: ../internal/dbfile/varint.go -->
```go
package dbfile

import "errors"

var errVarintTruncated = errors.New("varint runs past the end of its buffer")

// MaxVarintLength is the maximum byte length of an SQLite variable-length
// integer (src/btreeInt.h:170-176, src/util.c:1633-1781).
const MaxVarintLength = 9

// Varint decodes one big-endian 1-to-9-byte SQLite varint from b, returning its
// unsigned 64-bit value and byte length. Bytes 1..8 contribute 7 bits each with
// bit 7 as the continuation flag; byte 9 contributes all 8 bits so a full
// 64-bit integer fits in 9 bytes (sqlite3GetVarint, src/util.c:1633-1781).
func Varint(b []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < MaxVarintLength-1; i++ {
		if i >= len(b) {
			return 0, 0, errVarintTruncated
		}
		value = value<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return value, i + 1, nil
		}
	}
	if len(b) < MaxVarintLength {
		return 0, 0, errVarintTruncated
	}
	// The 9th byte has no continuation bit; all 8 bits belong to the value.
	value = value<<8 | uint64(b[MaxVarintLength-1])
	return value, MaxVarintLength, nil
}

// AppendVarint encodes value as an SQLite 1-to-9-byte varint and appends it to dst.
func AppendVarint(dst []byte, value uint64) []byte {
	// Values above 2^56 - 1 require all 9 bytes, with the low 8 bits in byte 9.
	if value > 0x00ffffffffffffff {
		var out [MaxVarintLength]byte
		out[8] = byte(value)
		value >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(value&0x7f) | 0x80
			value >>= 7
		}
		return append(dst, out[:]...)
	}
	var reversed [MaxVarintLength]byte
	n := 0
	for {
		reversed[n] = byte(value&0x7f) | 0x80
		n++
		value >>= 7
		if value == 0 {
			break
		}
	}
	// Clear the continuation bit on the least-significant (last-emitted) byte.
	reversed[0] &= 0x7f
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, reversed[i])
	}
	return dst
}

// VarintLength returns the encoded byte length of value as an SQLite varint.
func VarintLength(value uint64) int {
	return len(AppendVarint(nil, value))
}
```

`varint.go` implements SQLite's 1-to-9-byte big-endian varints matching `sqlite3GetVarint` ([`src/util.c:1633-1781`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/util.c#L1633-L1781)): bytes 1–8 contribute 7 bits each while bit 7 is set, and byte 9 contributes all 8 bits so 64-bit unsigned integers round-trip in at most 9 bytes. A truncated buffer returns an error rather than reading past the slice.

#### `internal/dbfile/header.go`

<!-- include: ../internal/dbfile/header.go -->
```go
package dbfile

import (
	"encoding/binary"
	"fmt"
)

// File-format constants from src/btreeInt.h:47-84 and lockBtree (src/btree.c:3314-3418).
const (
	// HeaderSize is the byte length of the database file header at the start of page 1.
	HeaderSize = 100
	// MagicString is the 16-byte signature at offset 0 of every valid SQLite 3 file.
	MagicString = "SQLite format 3\x00"
	// LockByteOffset is the fixed 1 GiB (0x40000000) byte offset reserved for POSIX locks.
	LockByteOffset = 1073741824
	// MinPageSize is the smallest legal database page size in bytes.
	MinPageSize = 512
	// MaxPageSize is the largest legal database page size in bytes (stored as 1 at offset 16).
	MaxPageSize = 65536
	// MinUsableSize is the minimum allowed usable space per page (page size minus reserved bytes).
	MinUsableSize = 480
	// TextEncodingUTF8 is the header code (offset 56) for UTF-8 text.
	TextEncodingUTF8 = 1
	// TextEncodingUTF16L is the header code (offset 56) for little-endian UTF-16 text.
	TextEncodingUTF16L = 2
	// TextEncodingUTF16B is the header code (offset 56) for big-endian UTF-16 text.
	TextEncodingUTF16B = 3
)

// Header holds the decoded fields of the 100-byte database file header at the
// start of page 1, plus the raw bytes (src/btreeInt.h:57-83).
type Header struct {
	Raw                 [HeaderSize]byte
	PageSize            int
	WriteVersion        int
	ReadVersion         int
	ReservedBytes       int
	MaxPayloadFraction  int
	MinPayloadFraction  int
	LeafPayloadFraction int
	ChangeCounter       uint32
	DatabaseSize        uint32
	FirstFreelistTrunk  uint32
	FreelistCount       uint32
	SchemaCookie        uint32
	SchemaFormat        uint32
	DefaultCacheSize    int32
	LargestRootPage     uint32
	TextEncoding        uint32
	UserVersion         int32
	IncrementalVacuum   uint32
	ApplicationID       int32
	VersionValidFor     uint32
	SQLiteVersion       uint32
}

func be16(b []byte) int     { return int(binary.BigEndian.Uint16(b)) }
func be32(b []byte) uint32  { return binary.BigEndian.Uint32(b) }
func be32i(b []byte) int32  { return int32(binary.BigEndian.Uint32(b)) }
func be32int(b []byte) int  { return int(binary.BigEndian.Uint32(b)) }
func be64i(b []byte) int64  { return int64(binary.BigEndian.Uint64(b)) }
func be64u(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

// DecodePageSize converts the 2-byte big-endian integer at offset 16 into a
// page size in bytes. Because 65536 does not fit in 16 bits, SQLite encodes it
// as 1 (newDatabase src/btree.c:3526-3527, lockBtree src/btree.c:3371).
func DecodePageSize(stored int) int {
	if stored == 1 {
		return MaxPageSize
	}
	return stored
}

// ParseHeader decodes and validates the 100-byte database header from b,
// mirroring the header checks in lockBtree (src/btree.c:3282-3459).
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderSize {
		return h, &FormatError{Kind: ErrTruncated, Offset: len(b), Problem: fmt.Sprintf("header needs %d bytes, have %d", HeaderSize, len(b))}
	}
	copy(h.Raw[:], b[:HeaderSize])
	if string(b[:16]) != MagicString {
		return h, &FormatError{Kind: ErrNotADatabase, Offset: 0, Problem: fmt.Sprintf("magic is %q", b[:16])}
	}
	h.PageSize = DecodePageSize(be16(b[16:18]))
	h.WriteVersion = int(b[18])
	h.ReadVersion = int(b[19])
	h.ReservedBytes = int(b[20])
	h.MaxPayloadFraction = int(b[21])
	h.MinPayloadFraction = int(b[22])
	h.LeafPayloadFraction = int(b[23])
	h.ChangeCounter = be32(b[24:28])
	h.DatabaseSize = be32(b[28:32])
	h.FirstFreelistTrunk = be32(b[32:36])
	h.FreelistCount = be32(b[36:40])
	h.SchemaCookie = be32(b[40:44])
	h.SchemaFormat = be32(b[44:48])
	h.DefaultCacheSize = be32i(b[48:52])
	h.LargestRootPage = be32(b[52:56])
	h.TextEncoding = be32(b[56:60])
	h.UserVersion = be32i(b[60:64])
	h.IncrementalVacuum = be32(b[64:68])
	h.ApplicationID = be32i(b[68:72])
	h.VersionValidFor = be32(b[92:96])
	h.SQLiteVersion = be32(b[96:100])
	if err := h.validate(); err != nil {
		return h, err
	}
	return h, nil
}

func (h Header) validate() error {
	bad := func(offset int, format string, args ...any) error {
		return &FormatError{Kind: ErrNotADatabase, Offset: offset, Problem: fmt.Sprintf(format, args...)}
	}
	// Page size must be a power of two in 512..65536 (lockBtree, src/btree.c:3374-3379).
	if h.PageSize < MinPageSize || h.PageSize > MaxPageSize || h.PageSize&(h.PageSize-1) != 0 {
		return bad(16, "page size %d is not a power of two between %d and %d", h.PageSize, MinPageSize, MaxPageSize)
	}
	// Reject format versions other than 1 or 2 on both read and write bytes;
	// SQLite opens write version > 2 read-only (src/btree.c:3326-3331), which a
	// read-only inspector has no reason to distinguish.
	if h.ReadVersion != 1 && h.ReadVersion != 2 {
		return bad(19, "read version %d is not 1 (rollback) or 2 (WAL)", h.ReadVersion)
	}
	if h.WriteVersion != 1 && h.WriteVersion != 2 {
		return bad(18, "write version %d is not 1 (rollback) or 2 (WAL)", h.WriteVersion)
	}
	// Payload fractions have been fixed at 64/32/32 since SQLite 3.6.0 (src/btree.c:3360-3367).
	if h.MaxPayloadFraction != 64 || h.MinPayloadFraction != 32 || h.LeafPayloadFraction != 32 {
		return bad(21, "payload fractions %d/%d/%d are not 64/32/32", h.MaxPayloadFraction, h.MinPayloadFraction, h.LeafPayloadFraction)
	}
	if h.UsableSize() < MinUsableSize {
		return bad(20, "usable size %d (page size %d minus %d reserved) is below %d", h.UsableSize(), h.PageSize, h.ReservedBytes, MinUsableSize)
	}
	// Encoding 0 is permitted for a newly initialized header before the first schema write.
	if h.TextEncoding > TextEncodingUTF16B {
		return bad(56, "text encoding %d is not 1, 2 or 3", h.TextEncoding)
	}
	if h.SchemaFormat > 4 {
		return bad(44, "schema format %d is above 4", h.SchemaFormat)
	}
	return nil
}

// UsableSize returns the bytes per page available to B-tree and overflow data
// (page size minus the reserved suffix at offset 20, src/btree.c:3388).
func (h Header) UsableSize() int { return h.PageSize - h.ReservedBytes }

// ChangeCountersAgree reports whether the file change counter (bytes 24-27)
// equals version-valid-for (bytes 92-95).
func (h Header) ChangeCountersAgree() bool { return h.ChangeCounter == h.VersionValidFor }

// InHeaderSizeValid reports whether SQLite trusts the in-header page count at
// offset 28: it must be non-zero and bytes 24-27 must equal bytes 92-95
// (lockBtree, src/btree.c:3298-3302).
func (h Header) InHeaderSizeValid() bool {
	return h.DatabaseSize != 0 && h.ChangeCountersAgree()
}

// WALMode reports whether the file uses WAL mode, which lockBtree decides from
// the read format version byte alone (byte 19 == 2, src/btree.c:3341).
func (h Header) WALMode() bool { return h.ReadVersion == 2 }

// AutoVacuum reports whether auto-vacuum (FULL or INCREMENTAL) is enabled,
// indicated by a non-zero largest-root-page field at offset 52 (src/btree.c:3423).
func (h Header) AutoVacuum() bool { return h.LargestRootPage != 0 }

// LockBytePage returns the 1-based page number that contains LockByteOffset (1 GiB).
func (h Header) LockBytePage() int { return LockByteOffset/h.PageSize + 1 }

// EncodingName returns the human-readable name for the text encoding at offset 56.
func (h Header) EncodingName() string {
	switch h.TextEncoding {
	case 0:
		return "unset"
	case TextEncodingUTF8:
		return "UTF-8"
	case TextEncodingUTF16L:
		return "UTF-16le"
	case TextEncodingUTF16B:
		return "UTF-16be"
	}
	return fmt.Sprintf("unknown(%d)", h.TextEncoding)
}

// AutoVacuumName returns "NONE", "FULL", or "INCREMENTAL" from offsets 52 and 64.
func (h Header) AutoVacuumName() string {
	switch {
	case h.LargestRootPage == 0:
		return "NONE"
	case h.IncrementalVacuum != 0:
		return "INCREMENTAL"
	}
	return "FULL"
}

// FormatSQLiteVersion formats a packed SQLITE_VERSION_NUMBER (X*1000000 + Y*1000 + Z)
// such as 3053004 into "3.53.4".
func FormatSQLiteVersion(v uint32) string {
	return fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000)
}

// HeaderField describes one named field in the 100-byte database header for
// table display and byte-diff attribution.
type HeaderField struct {
	Offset int
	Size   int
	Name   string
	Value  string
}

// Fields returns all 23 header fields in offset order, partitioning bytes 0..99
// without gaps or overlaps.
func (h Header) Fields() []HeaderField {
	u := func(v uint32) string { return fmt.Sprint(v) }
	i := func(v int32) string { return fmt.Sprint(v) }
	stored := be16(h.Raw[16:18])
	pageSize := fmt.Sprint(h.PageSize)
	if stored != h.PageSize {
		pageSize = fmt.Sprintf("%d (stored as %d)", h.PageSize, stored)
	}
	return []HeaderField{
		{0, 16, "magic", fmt.Sprintf("%q", string(h.Raw[:16]))},
		{16, 2, "page size", pageSize},
		{18, 1, "write version", fmt.Sprint(h.WriteVersion)},
		{19, 1, "read version", fmt.Sprint(h.ReadVersion)},
		{20, 1, "reserved bytes per page", fmt.Sprint(h.ReservedBytes)},
		{21, 1, "max embedded payload fraction", fmt.Sprint(h.MaxPayloadFraction)},
		{22, 1, "min embedded payload fraction", fmt.Sprint(h.MinPayloadFraction)},
		{23, 1, "leaf payload fraction", fmt.Sprint(h.LeafPayloadFraction)},
		{24, 4, "file change counter", u(h.ChangeCounter)},
		{28, 4, "database size in pages", u(h.DatabaseSize)},
		{32, 4, "first freelist trunk page", u(h.FirstFreelistTrunk)},
		{36, 4, "freelist page count", u(h.FreelistCount)},
		{40, 4, "schema cookie", u(h.SchemaCookie)},
		{44, 4, "schema format", u(h.SchemaFormat)},
		{48, 4, "default cache size", i(h.DefaultCacheSize)},
		{52, 4, "largest root page (auto-vacuum)", u(h.LargestRootPage)},
		{56, 4, "text encoding", fmt.Sprintf("%d (%s)", h.TextEncoding, h.EncodingName())},
		{60, 4, "user version", i(h.UserVersion)},
		{64, 4, "incremental vacuum", u(h.IncrementalVacuum)},
		{68, 4, "application id", i(h.ApplicationID)},
		{72, 20, "reserved for expansion", reservedText(h.Raw[72:92])},
		{92, 4, "version-valid-for", u(h.VersionValidFor)},
		{96, 4, "SQLite version number", fmt.Sprintf("%d (%s)", h.SQLiteVersion, FormatSQLiteVersion(h.SQLiteVersion))},
	}
}

func reservedText(b []byte) string {
	for _, c := range b {
		if c != 0 {
			return fmt.Sprintf("%x", b)
		}
	}
	return "all zero"
}

// HeaderFieldAt returns the header field definition covering byte offset (0..99).
func HeaderFieldAt(offset int) (HeaderField, bool) {
	for _, f := range (Header{}).Fields() {
		if offset >= f.Offset && offset < f.Offset+f.Size {
			return HeaderField{Offset: f.Offset, Size: f.Size, Name: f.Name}, true
		}
	}
	return HeaderField{}, false
}
```

`header.go` decodes all 23 fields of the 100-byte database header, turns stored page size `1` into `65536`, and mirrors `lockBtree`'s validation checks ([`src/btree.c:3282-3459`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3282-L3459)): magic string, power-of-two page size in `512..65536`, format versions `1` or `2`, fixed payload fractions `64/32/32`, usable size `>= 480`, text encoding `<= 3`, and schema format `<= 4`. `Fields()` partitions bytes 0–99 without gaps or overlaps. `InHeaderSizeValid()` encodes the `lockBtree` trust rule (`DatabaseSize != 0 && ChangeCounter == VersionValidFor`), and `WALMode()` checks byte 19 alone (`ReadVersion == 2`, matching `lockBtree` at [`src/btree.c:3341`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3341)).

#### `internal/dbfile/file.go`

<!-- include: ../internal/dbfile/file.go -->
```go
// Package dbfile parses SQLite 3 database files directly from raw bytes without
// calling SQLite.
//
// It decodes the 100-byte database header, B-tree pages and cells, overflow
// chains, records, sqlite_schema, the freelist, pointer-map pages, and the
// 1 GiB lock-byte page, and produces a complete per-page census. Every malformed
// input returns a typed FormatError rather than panicking.
package dbfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// PageCountSource values reported by File.PageCountSource.
const (
	// PageCountFromHeader means the page count came from header bytes 28-31.
	PageCountFromHeader = "in-header size"
	// PageCountFromFileSize means bytes 28-31 were 0 or stale, so file size / page size was used.
	PageCountFromFileSize = "file size"
)

// File provides read-only page and structure access to an SQLite database file.
type File struct {
	reader          io.ReaderAt
	closer          io.Closer
	size            int64
	Header          Header
	pageCount       int
	pageCountSource string
}

// Open opens an SQLite database file from disk and validates its header.
//
// Do not call Open on a file while the same process holds an open SQLite
// connection to it: closing any descriptor on a file drops all POSIX locks
// held by the process on that file.
func Open(path string) (*File, error) {
	osFile, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := osFile.Stat()
	if err != nil {
		osFile.Close()
		return nil, err
	}
	f, err := New(osFile, info.Size())
	if err != nil {
		osFile.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.closer = osFile
	return f, nil
}

// FromBytes wraps an in-memory database file image as a read-only File.
func FromBytes(image []byte) (*File, error) {
	return New(bytes.NewReader(image), int64(len(image)))
}

// New validates the header and page count of a database stored in reader of
// length size bytes.
func New(reader io.ReaderAt, size int64) (*File, error) {
	if size < HeaderSize {
		return nil, &FormatError{Kind: ErrTruncated, Offset: int(size), Problem: fmt.Sprintf("file has %d bytes, the header alone needs %d", size, HeaderSize)}
	}
	raw := make([]byte, HeaderSize)
	if _, err := reader.ReadAt(raw, 0); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	header, err := ParseHeader(raw)
	if err != nil {
		return nil, err
	}
	f := &File{reader: reader, size: size, Header: header}
	// Stricter than SQLite: SQLite rounds a partial trailing page up and zero-fills
	// it on read, but a page-based capture could not rebuild a partial page.
	if size%int64(header.PageSize) != 0 {
		return nil, &FormatError{Kind: ErrTruncated, Offset: int(size), Problem: fmt.Sprintf("file size %d is not a multiple of the page size %d", size, header.PageSize)}
	}
	filePages := size / int64(header.PageSize)
	if filePages > 0xfffffffe {
		return nil, &FormatError{Kind: ErrUnsupported, Offset: -1, Problem: fmt.Sprintf("file holds %d pages, more than a page number can address", filePages)}
	}
	f.pageCount = int(filePages)
	f.pageCountSource = PageCountFromFileSize
	// Mirror lockBtree (src/btree.c:3298-3302, 3405-3412): trust the in-header
	// page count only when non-zero and bytes 24-27 match 92-95, and treat an
	// in-header size larger than the file as corruption.
	if header.InHeaderSizeValid() {
		if int64(header.DatabaseSize) > filePages {
			return nil, &FormatError{Kind: ErrCorrupt, Offset: 28, Problem: fmt.Sprintf("in-header size %d pages is larger than the file (%d pages) while the change counters agree", header.DatabaseSize, filePages)}
		}
		f.pageCount = int(header.DatabaseSize)
		f.pageCountSource = PageCountFromHeader
	}
	return f, nil
}

// Close releases the underlying file descriptor if File was created by Open.
func (f *File) Close() error {
	if f.closer == nil {
		return nil
	}
	err := f.closer.Close()
	f.closer = nil
	return err
}

// FileSize returns the byte length of the underlying file or buffer.
func (f *File) FileSize() int64 { return f.size }

// PageSize returns the database page size in bytes.
func (f *File) PageSize() int { return f.Header.PageSize }

// UsableSize returns the usable byte count per page (page size minus reserved bytes).
func (f *File) UsableSize() int { return f.Header.UsableSize() }

// PageCount returns the effective number of pages in the database.
func (f *File) PageCount() int { return f.pageCount }

// PageCountSource reports whether PageCount came from PageCountFromHeader or
// PageCountFromFileSize.
func (f *File) PageCountSource() string { return f.pageCountSource }

// LockBytePage returns the 1-based lock-byte page number if this file is large
// enough to contain offset 1 GiB, or 0 otherwise.
func (f *File) LockBytePage() int {
	page := f.Header.LockBytePage()
	if page > f.pageCount {
		return 0
	}
	return page
}

func (f *File) checkPageNumber(n int) error {
	if n < 1 || n > f.pageCount {
		return &FormatError{Kind: ErrPageOutOfFile, Page: max(n, 0), Offset: -1, Problem: fmt.Sprintf("page %d is outside 1..%d", n, f.pageCount)}
	}
	return nil
}

// Page reads and returns a fresh copy of 1-based page n.
func (f *File) Page(n int) ([]byte, error) {
	if err := f.checkPageNumber(n); err != nil {
		return nil, err
	}
	data := make([]byte, f.Header.PageSize)
	read, err := f.reader.ReadAt(data, int64(n-1)*int64(f.Header.PageSize))
	if err != nil && !(errors.Is(err, io.EOF) && read == len(data)) {
		return nil, fmt.Errorf("read page %d: %w", n, err)
	}
	return data, nil
}

// PointerMapPage returns the pointer-map page governing page n when auto-vacuum
// is enabled, or 0 otherwise. Matches ptrmapPageno (src/btree.c:1036-1048): one
// pointer-map page holds usableSize/5 5-byte entries and therefore appears every
// usableSize/5 + 1 pages starting at page 2, shifted past the lock-byte page.
func (f *File) PointerMapPage(n int) int {
	if !f.Header.AutoVacuum() || n < 2 {
		return 0
	}
	perMap := f.UsableSize()/5 + 1
	mapPage := (n-2)/perMap*perMap + 2
	if mapPage == f.Header.LockBytePage() {
		mapPage++
	}
	return mapPage
}

// IsPointerMapPage reports whether page n is itself a pointer-map page.
func (f *File) IsPointerMapPage(n int) bool {
	return f.Header.AutoVacuum() && n >= 2 && n != f.Header.LockBytePage() && f.PointerMapPage(n) == n
}
```

`file.go` opens a database from disk (`Open`) or memory (`FromBytes`, `New`) and verifies that the file size is an exact multiple of the page size (stricter than SQLite on purpose; see Section 6.1). It resolves the effective page count using `InHeaderSizeValid()`, provides 1-based page reads (`Page`), and computes the lock-byte page (`LockBytePage`) and auto-vacuum pointer-map positions (`PointerMapPage`, `IsPointerMapPage`, matching `ptrmapPageno` at [`src/btree.c:1036-1048`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1036-L1048)).

#### `internal/dbfile/btree.go`

<!-- include: ../internal/dbfile/btree.go -->
```go
package dbfile

import (
	"fmt"
	"sort"
)

// PageType is the 1-byte flag at offset 0 of a B-tree page header
// (decodeFlags, src/btree.c:2028-2085).
type PageType uint8

// Valid B-tree page flag bytes (src/btreeInt.h:129, src/btree.c:2025-2083).
const (
	// IndexInterior (0x02) marks an interior index B-tree page (PTF_ZERODATA).
	IndexInterior PageType = 0x02
	// TableInterior (0x05) marks an interior table B-tree page (PTF_LEAFDATA|PTF_INTKEY).
	TableInterior PageType = 0x05
	// IndexLeaf (0x0a) marks a leaf index B-tree page (PTF_ZERODATA|PTF_LEAF).
	IndexLeaf PageType = 0x0a
	// TableLeaf (0x0d) marks a leaf table B-tree page (PTF_LEAFDATA|PTF_INTKEY|PTF_LEAF).
	TableLeaf PageType = 0x0d
)

// Valid reports whether t is one of the four legal B-tree page types.
func (t PageType) Valid() bool {
	return t == IndexInterior || t == TableInterior || t == IndexLeaf || t == TableLeaf
}

// IsLeaf reports whether t is a leaf page (no child pointers).
func (t PageType) IsLeaf() bool { return t == IndexLeaf || t == TableLeaf }

// IsTable reports whether t belongs to an intkey table B-tree rather than an index B-tree.
func (t PageType) IsTable() bool { return t == TableInterior || t == TableLeaf }

// HeaderSize returns the byte length of the B-tree page header: 8 for leaves,
// 12 for interior pages (which append the 4-byte rightmost child pointer).
func (t PageType) HeaderSize() int {
	if t.IsLeaf() {
		return 8
	}
	return 12
}

// String returns the canonical role name for a B-tree page type.
func (t PageType) String() string {
	switch t {
	case IndexInterior:
		return "index-interior"
	case TableInterior:
		return "table-interior"
	case IndexLeaf:
		return "index-leaf"
	case TableLeaf:
		return "table-leaf"
	}
	return fmt.Sprintf("invalid(0x%02x)", uint8(t))
}

// Freeblock describes one linked free region (at least 4 bytes) inside a B-tree
// page's cell content area (src/btreeInt.h:152-164).
type Freeblock struct {
	Offset int
	Size   int
}

// BTreePage holds the decoded header, cell pointer array, freeblock chain, and
// raw bytes of one B-tree page (btreeInitPage, src/btree.c:2218-2265).
type BTreePage struct {
	Number           int
	Type             PageType
	HeaderOffset     int
	FirstFreeblock   int
	CellCount        int
	CellContentStart int
	FragmentedBytes  int
	RightChild       uint32
	CellPointers     []int
	Freeblocks       []Freeblock
	Data             []byte
	UsableSize       int
}

func headerOffsetFor(number int) int {
	if number == 1 {
		return HeaderSize
	}
	return 0
}

// ParseBTreePage decodes and validates a B-tree page header, its cell pointer
// array, its freeblock chain, its free-space bounds (btreeComputeFreeSpace,
// src/btree.c:2146-2166), and its full cell/freeblock byte coverage
// (checkTreePage, src/btree.c:11076-11108).
func ParseBTreePage(data []byte, number int, usableSize int) (*BTreePage, error) {
	if usableSize < MinUsableSize || usableSize > len(data) {
		return nil, corruptPage(number, "usable size %d does not fit a %d-byte page", usableSize, len(data))
	}
	// Page 1's B-tree header starts after the 100-byte database file header.
	offset := headerOffsetFor(number)
	if offset+8 > usableSize {
		return nil, corruptAt(number, offset, "page too small for a b-tree page header")
	}
	p := &BTreePage{Number: number, HeaderOffset: offset, Data: data, UsableSize: usableSize}
	p.Type = PageType(data[offset])
	if !p.Type.Valid() {
		return nil, corruptAt(number, offset, "page type byte 0x%02x is not 0x02, 0x05, 0x0a or 0x0d", data[offset])
	}
	p.FirstFreeblock = be16(data[offset+1:])
	p.CellCount = be16(data[offset+3:])
	p.CellContentStart = be16(data[offset+5:])
	// On a 65536-byte page with no cells, 65536 wraps to 0 in the 2-byte field (get2byteNotZero, src/btree.c:2257).
	if p.CellContentStart == 0 {
		p.CellContentStart = 65536
	}
	p.FragmentedBytes = int(data[offset+7])
	if !p.Type.IsLeaf() {
		p.RightChild = be32(data[offset+8:])
	}
	pointerStart := offset + p.Type.HeaderSize()
	pointerEnd := pointerStart + 2*p.CellCount
	if pointerEnd > usableSize {
		return nil, corruptAt(number, offset+3, "%d cells do not fit: the cell pointer array would end at %d, usable size %d", p.CellCount, pointerEnd, usableSize)
	}
	if p.CellContentStart < pointerEnd || p.CellContentStart > usableSize {
		return nil, corruptAt(number, offset+5, "cell content area starts at %d, outside %d..%d", p.CellContentStart, pointerEnd, usableSize)
	}
	p.CellPointers = make([]int, p.CellCount)
	// Every cell occupies at least 4 bytes (leaves) or 5 bytes (interior: 4-byte child + 1-byte varint).
	lastCellStart := usableSize - 4
	if !p.Type.IsLeaf() {
		lastCellStart--
	}
	for i := range p.CellCount {
		cellOffset := be16(data[pointerStart+2*i:])
		if cellOffset < p.CellContentStart || cellOffset > lastCellStart {
			return nil, corruptAt(number, pointerStart+2*i, "cell %d pointer %d is outside the cell content area %d..%d", i, cellOffset, p.CellContentStart, lastCellStart)
		}
		p.CellPointers[i] = cellOffset
	}
	// Walk the freeblock list verifying ascending offsets separated by at least
	// 4 bytes (btreeComputeFreeSpace, src/btree.c:2146) and minimum 4-byte size
	// (src/btreeInt.h:152-164).
	previousEnd := 0
	for next := p.FirstFreeblock; next != 0; {
		if len(p.Freeblocks) >= usableSize/4 {
			return nil, corruptAt(number, next, "freeblock chain does not end")
		}
		if next < p.CellContentStart || next > usableSize-4 {
			return nil, corruptAt(number, next, "freeblock at %d is outside the cell content area %d..%d", next, p.CellContentStart, usableSize-4)
		}
		if previousEnd > 0 && next < previousEnd+4 {
			return nil, corruptAt(number, next, "freeblock at %d is less than 4 bytes after the freeblock ending at %d", next, previousEnd)
		}
		size := be16(data[next+2:])
		if size < 4 || next+size > usableSize {
			return nil, corruptAt(number, next, "freeblock at %d has size %d, outside 4..%d", next, size, usableSize-next)
		}
		p.Freeblocks = append(p.Freeblocks, Freeblock{Offset: next, Size: size})
		previousEnd = next + size
		next = be16(data[next:])
	}
	// Total free space must fit within the usable page (btreeComputeFreeSpace, src/btree.c:2166).
	if free := p.CellContentStart + p.FragmentedBytes + p.FreeblockBytes(); free > usableSize {
		return nil, corruptAt(number, offset+7, "content start %d + fragmented bytes %d + freeblock bytes %d = %d exceeds the usable size %d", p.CellContentStart, p.FragmentedBytes, p.FreeblockBytes(), free, usableSize)
	}
	if err := p.checkCoverage(); err != nil {
		return nil, err
	}
	return p, nil
}

// byteRange is an inclusive [first, last] byte span inside a page's cell content area.
type byteRange struct{ first, last int }

// checkCoverage mirrors checkTreePage (src/btree.c:11076-11108): no byte of the
// cell content area may belong to two cells or to both a cell and a freeblock,
// and the bytes covered by neither must equal header byte 7 (FragmentedBytes).
func (p *BTreePage) checkCoverage() error {
	ranges := make([]byteRange, 0, p.CellCount+len(p.Freeblocks))
	for i := range p.CellCount {
		c, err := p.Cell(i)
		if err != nil {
			return err
		}
		ranges = append(ranges, byteRange{c.Offset, c.Offset + c.Size - 1})
	}
	for _, b := range p.Freeblocks {
		ranges = append(ranges, byteRange{b.Offset, b.Offset + b.Size - 1})
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].first != ranges[j].first {
			return ranges[i].first < ranges[j].first
		}
		return ranges[i].last < ranges[j].last
	})
	fragmented := 0
	previousLast := p.CellContentStart - 1
	for _, r := range ranges {
		if r.first <= previousLast {
			return corruptAt(p.Number, r.first, "byte %d is used twice (by two cells, or a cell and a freeblock)", r.first)
		}
		fragmented += r.first - previousLast - 1
		previousLast = r.last
	}
	fragmented += p.UsableSize - previousLast - 1
	if fragmented != p.FragmentedBytes {
		return corruptAt(p.Number, p.HeaderOffset+7, "%d bytes of the cell content area belong to no cell or freeblock, the header reports %d fragmented bytes", fragmented, p.FragmentedBytes)
	}
	return nil
}

// BTreePage reads page n from f and parses it as a B-tree page.
func (f *File) BTreePage(n int) (*BTreePage, error) {
	data, err := f.Page(n)
	if err != nil {
		return nil, err
	}
	return ParseBTreePage(data, n, f.UsableSize())
}

// FreeblockBytes returns the sum of the byte sizes of all freeblocks on p.
func (p *BTreePage) FreeblockBytes() int {
	total := 0
	for _, b := range p.Freeblocks {
		total += b.Size
	}
	return total
}

// Unused returns the total free bytes on p (unallocated gap + fragmented bytes
// + freeblocks), matching dbstat's unused column.
func (p *BTreePage) Unused() int {
	pointerEnd := p.HeaderOffset + p.Type.HeaderSize() + 2*p.CellCount
	return p.CellContentStart - pointerEnd + p.FragmentedBytes + p.FreeblockBytes()
}

// Cell holds one parsed B-tree cell: its optional left-child pointer, payload
// size, rowid, local payload slice, and optional first overflow page number.
type Cell struct {
	Index         int
	Offset        int
	LeftChild     uint32
	PayloadSize   uint64
	Rowid         int64
	HasRowid      bool
	LocalSize     int
	Local         []byte
	FirstOverflow uint32
	Size          int
}

// Overflows reports whether part of the cell's payload spills onto overflow pages.
func (c Cell) Overflows() bool { return c.PayloadSize > uint64(c.LocalSize) }

// MaxLocalPayload returns the maximum payload bytes stored locally in one cell
// before spilling to overflow pages (lockBtree, src/btree.c:3441-3444).
func MaxLocalPayload(usableSize int, t PageType) int {
	if t.IsTable() {
		return usableSize - 35
	}
	return (usableSize-12)*64/255 - 23
}

// MinLocalPayload returns the minimum payload bytes retained locally when a
// cell overflows (lockBtree, src/btree.c:3442, 3444).
func MinLocalPayload(usableSize int) int {
	return (usableSize-12)*32/255 - 23
}

// LocalPayloadSize computes how many bytes of a payload of size payloadSize are
// stored on the B-tree page itself rather than in overflow pages. Matches
// btreeParseCellAdjustSizeForOverflow (src/btree.c:1178-1207), which maximizes
// occupancy of the last overflow page.
func LocalPayloadSize(usableSize int, t PageType, payloadSize uint64) int {
	maxLocal := MaxLocalPayload(usableSize, t)
	if payloadSize <= uint64(maxLocal) {
		return int(payloadSize)
	}
	minLocal := MinLocalPayload(usableSize)
	surplus := uint64(minLocal) + (payloadSize-uint64(minLocal))%uint64(usableSize-4)
	if surplus <= uint64(maxLocal) {
		return int(surplus)
	}
	return minLocal
}

// MaxPayloadSize is the largest legal cell payload size (2^31 - 1 bytes).
const MaxPayloadSize = 0x7fffffff

// Cell parses the i-th cell on p according to p.Type, mirroring
// btreeParseCellPtr, btreeParseCellPtrIndex, and btreeParseCellPtrNoPayload
// (src/btree.c:1242-1385).
func (p *BTreePage) Cell(i int) (Cell, error) {
	if i < 0 || i >= p.CellCount {
		return Cell{}, fmt.Errorf("page %d has %d cells, no cell %d", p.Number, p.CellCount, i)
	}
	offset := p.CellPointers[i]
	c := Cell{Index: i, Offset: offset}
	limit := p.Data[:p.UsableSize]
	pos := offset
	readVarint := func(what string) (uint64, error) {
		value, n, err := Varint(limit[pos:])
		if err != nil {
			return 0, corruptAt(p.Number, pos, "cell %d %s: %v", i, what, err)
		}
		pos += n
		return value, nil
	}
	if !p.Type.IsLeaf() {
		if pos+4 > len(limit) {
			return c, corruptAt(p.Number, pos, "cell %d left child pointer runs past the usable size", i)
		}
		c.LeftChild = be32(limit[pos:])
		pos += 4
	}
	if p.Type == TableInterior {
		key, err := readVarint("rowid")
		if err != nil {
			return c, err
		}
		c.Rowid, c.HasRowid = int64(key), true
		c.Size = pos - offset
		return c, nil
	}
	payloadSize, err := readVarint("payload size")
	if err != nil {
		return c, err
	}
	if payloadSize > MaxPayloadSize {
		return c, corruptAt(p.Number, offset, "cell %d payload size %d is larger than %d", i, payloadSize, MaxPayloadSize)
	}
	c.PayloadSize = payloadSize
	if p.Type == TableLeaf {
		key, err := readVarint("rowid")
		if err != nil {
			return c, err
		}
		c.Rowid, c.HasRowid = int64(key), true
	}
	c.LocalSize = LocalPayloadSize(p.UsableSize, p.Type, payloadSize)
	end := pos + c.LocalSize
	if c.Overflows() {
		end += 4
	}
	if end > len(limit) {
		return c, corruptAt(p.Number, offset, "cell %d ends at %d, past the usable size %d", i, end, len(limit))
	}
	c.Local = limit[pos : pos+c.LocalSize]
	if c.Overflows() {
		c.FirstOverflow = be32(limit[pos+c.LocalSize:])
		if c.FirstOverflow == 0 {
			return c, corruptAt(p.Number, pos+c.LocalSize, "cell %d overflows but its first overflow page is 0", i)
		}
	}
	c.Size = end - offset
	// SQLite enforces a 4-byte minimum cell size so any freed cell can become a
	// 4-byte freeblock header (src/btree.c:1341, 1380).
	if c.Size < 4 {
		c.Size = 4
	}
	return c, nil
}

// Cells parses and returns all cells on p in cell-pointer (key) order.
func (p *BTreePage) Cells() ([]Cell, error) {
	cells := make([]Cell, 0, p.CellCount)
	for i := range p.CellCount {
		c, err := p.Cell(i)
		if err != nil {
			return nil, err
		}
		cells = append(cells, c)
	}
	return cells, nil
}
```

`btree.go` decodes B-tree page headers at offset 100 on page 1 and offset 0 elsewhere (`ParseBTreePage`), validates the page type (`0x02`, `0x05`, `0x0a`, `0x0d`), and checks that the cell-pointer array and every cell offset stay inside the usable bounds. It walks the freeblock chain requiring ascending offsets separated by at least 4 bytes ([`src/btree.c:2146`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L2146)), enforces `CellContentStart + FragmentedBytes + FreeblockBytes <= usableSize` ([`src/btree.c:2166`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L2166)), and runs `checkCoverage()` ([`src/btree.c:11076-11108`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L11076-L11108)) to reject overlapping cells or freeblocks and any mismatch against header byte 7 (`FragmentedBytes`). It also computes `Unused()` identically to `dbstat`, implements the local/overflow split (`MaxLocalPayload`, `MinLocalPayload`, `LocalPayloadSize`, matching [`src/btree.c:1178-1207`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1178-L1207)), and parses individual cells (`Cell`, `Cells`).

#### `internal/dbfile/payload.go`

<!-- include: ../internal/dbfile/payload.go -->
```go
package dbfile

import "fmt"

// OverflowPage represents one page in a cell's singly linked overflow chain
// (src/btreeInt.h:198-205, getOverflowPage src/btree.c:5007-5062).
type OverflowPage struct {
	Number   int
	Next     uint32
	Content  []byte
	Payload  int
	IsLast   bool
	OwnerRow int64
}

func (f *File) checkOverflowTarget(from, target int) error {
	if target < 1 || target > f.pageCount {
		return corruptPage(from, "overflow pointer to page %d is outside 1..%d", target, f.pageCount)
	}
	if target == 1 || target == f.LockBytePage() || f.IsPointerMapPage(target) {
		return corruptPage(from, "overflow pointer to page %d, which can never be an overflow page", target)
	}
	return nil
}

// OverflowChain follows and validates all overflow pages referenced by cell c
// on sourcePage. Each overflow page begins with a 4-byte next-page number
// followed by up to usableSize-4 payload bytes (src/btree.c:5051).
func (f *File) OverflowChain(sourcePage int, c Cell) ([]OverflowPage, error) {
	if !c.Overflows() {
		return nil, nil
	}
	perPage := f.UsableSize() - 4
	remaining := c.PayloadSize - uint64(c.LocalSize)
	count := int((remaining + uint64(perPage) - 1) / uint64(perPage))
	if count > f.pageCount {
		return nil, corruptPage(sourcePage, "cell %d needs %d overflow pages, the file has %d pages", c.Index, count, f.pageCount)
	}
	chain := make([]OverflowPage, 0, count)
	seen := make(map[int]bool, count)
	from := sourcePage
	next := int(c.FirstOverflow)
	for i := range count {
		if err := f.checkOverflowTarget(from, next); err != nil {
			return nil, err
		}
		if seen[next] {
			return nil, corruptPage(from, "overflow chain of cell %d loops back to page %d", c.Index, next)
		}
		seen[next] = true
		data, err := f.Page(next)
		if err != nil {
			return nil, err
		}
		take := perPage
		if uint64(take) > remaining {
			take = int(remaining)
		}
		page := OverflowPage{Number: next, Next: be32(data[0:4]), Content: data[4 : 4+take], Payload: take, IsLast: i == count-1}
		chain = append(chain, page)
		remaining -= uint64(take)
		from = next
		next = int(page.Next)
		if i < count-1 && next == 0 {
			return nil, corruptPage(from, "overflow chain of cell %d ends after %d of %d pages", c.Index, i+1, count)
		}
		// The final overflow page terminates the list with next == 0; checkList
		// (src/btree.c:10729-10783) rejects a non-zero trailing pointer.
		if i == count-1 && next != 0 {
			return nil, corruptAt(from, 0, "last overflow page of cell %d on page %d has non-zero next pointer %d", c.Index, sourcePage, next)
		}
	}
	return chain, nil
}

// Payload assembles the complete payload bytes of cell c on sourcePage by
// concatenating its local slice with its overflow chain.
func (f *File) Payload(sourcePage int, c Cell) ([]byte, []OverflowPage, error) {
	// Guard against a corrupt payload-size varint requesting gigabytes of memory.
	if c.PayloadSize > uint64(f.size) {
		return nil, nil, corruptPage(sourcePage, "cell %d payload of %d bytes is larger than the file", c.Index, c.PayloadSize)
	}
	chain, err := f.OverflowChain(sourcePage, c)
	if err != nil {
		return nil, nil, err
	}
	payload := make([]byte, 0, c.PayloadSize)
	payload = append(payload, c.Local...)
	for _, page := range chain {
		payload = append(payload, page.Content...)
	}
	if uint64(len(payload)) != c.PayloadSize {
		return nil, nil, fmt.Errorf("page %d cell %d: assembled %d payload bytes, want %d", sourcePage, c.Index, len(payload), c.PayloadSize)
	}
	return payload, chain, nil
}
```

`payload.go` follows a cell's singly linked overflow chain (`OverflowChain`), computing the expected overflow page count `ceil((payload - local) / (usable - 4))`. It rejects loops, premature `next == 0` links, a non-zero `next` pointer on the last overflow page (matching `checkList`, [`src/btree.c:10729-10783`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L10729-L10783)), and pointers to page 1, the lock-byte page, or pointer-map pages. `Payload` concatenates local and overflow bytes after checking `PayloadSize <= FileSize` so a corrupt varint cannot trigger a huge allocation.

#### `internal/dbfile/record.go`

<!-- include: ../internal/dbfile/record.go -->
```go
package dbfile

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf16"
)

// StorageClass identifies the SQLite storage class of a decoded record column.
type StorageClass int

// SQLite's five on-disk value storage classes.
const (
	// Null represents SQL NULL.
	Null StorageClass = iota
	// Integer represents a signed 64-bit integer.
	Integer
	// Real represents an IEEE 754 64-bit floating-point number.
	Real
	// Text represents encoded text bytes (UTF-8 or UTF-16 per header offset 56).
	Text
	// Blob represents raw binary bytes.
	Blob
)

// String returns the lowercase SQLite typeof() name of s.
func (s StorageClass) String() string {
	switch s {
	case Null:
		return "null"
	case Integer:
		return "integer"
	case Real:
		return "real"
	case Text:
		return "text"
	case Blob:
		return "blob"
	}
	return fmt.Sprintf("class(%d)", int(s))
}

// Value holds one decoded column from an SQLite binary record
// (sqlite3VdbeSerialGet, src/vdbeaux.c:4123-4214).
type Value struct {
	SerialType uint64
	Class      StorageClass
	Int        int64
	Float      float64
	Bytes      []byte
}

// String formats v for diagnostic output.
func (v Value) String() string {
	switch v.Class {
	case Null:
		return "NULL"
	case Integer:
		return fmt.Sprint(v.Int)
	case Real:
		return fmt.Sprint(v.Float)
	case Text:
		return fmt.Sprintf("%q", v.Bytes)
	}
	return fmt.Sprintf("x'%x'", v.Bytes)
}

// SerialTypeLength returns the body byte length encoded by record serial type t
// (src/vdbeaux.c:4123-4214). Reserved serial types 10 and 11 return an error.
func SerialTypeLength(t uint64) (int, error) {
	switch {
	case t <= 4:
		return []int{0, 1, 2, 3, 4}[t], nil
	case t == 5:
		return 6, nil
	case t == 6 || t == 7:
		return 8, nil
	case t == 8 || t == 9:
		return 0, nil
	case t == 10 || t == 11:
		return 0, fmt.Errorf("serial type %d is reserved", t)
	case t > 2*MaxPayloadSize+13:
		return 0, fmt.Errorf("serial type %d is larger than any payload", t)
	}
	return int((t - 12) / 2), nil
}

func signedBigEndian(b []byte) int64 {
	var value int64
	if len(b) > 0 && b[0]&0x80 != 0 {
		value = -1
	}
	for _, c := range b {
		value = value<<8 | int64(c)
	}
	return value
}

// DecodeValue decodes one record field of serial type t from the front of body,
// matching sqlite3VdbeSerialGet (src/vdbeaux.c:4123-4214).
func DecodeValue(t uint64, body []byte) (Value, error) {
	length, err := SerialTypeLength(t)
	if err != nil {
		return Value{}, err
	}
	if length > len(body) {
		return Value{}, fmt.Errorf("serial type %d needs %d bytes, %d left", t, length, len(body))
	}
	v := Value{SerialType: t}
	switch {
	case t == 0:
		v.Class = Null
	case t <= 6:
		v.Class = Integer
		v.Int = signedBigEndian(body[:length])
	case t == 7:
		v.Class = Real
		v.Float = math.Float64frombits(binary.BigEndian.Uint64(body[:8]))
		// SQLite treats IEEE NaN stored with serial type 7 as SQL NULL (serialGet, src/vdbeaux.c:4103).
		if math.IsNaN(v.Float) {
			v.Class = Null
			v.Float = 0
		}
	case t == 8 || t == 9:
		// Schema format 4 encodes integer constants 0 and 1 in zero body bytes (src/vdbeaux.c:4193-4200).
		v.Class = Integer
		v.Int = int64(t - 8)
	case t%2 == 0:
		v.Class = Blob
		v.Bytes = body[:length]
	default:
		v.Class = Text
		v.Bytes = body[:length]
	}
	return v, nil
}

// DecodeRecord decodes an SQLite binary record payload (header-size varint,
// column serial-type varints, then packed column values).
func DecodeRecord(payload []byte) ([]Value, error) {
	headerSize, n, err := Varint(payload)
	if err != nil {
		return nil, fmt.Errorf("record header size: %w", err)
	}
	if headerSize < uint64(n) || headerSize > uint64(len(payload)) {
		return nil, fmt.Errorf("record header size %d is outside %d..%d", headerSize, n, len(payload))
	}
	header := payload[n:headerSize]
	body := payload[headerSize:]
	var values []Value
	for len(header) > 0 {
		t, used, err := Varint(header)
		if err != nil {
			return nil, fmt.Errorf("record column %d serial type: %w", len(values), err)
		}
		header = header[used:]
		v, err := DecodeValue(t, body)
		if err != nil {
			return nil, fmt.Errorf("record column %d: %w", len(values), err)
		}
		length, _ := SerialTypeLength(t)
		body = body[length:]
		values = append(values, v)
	}
	return values, nil
}

// DecodeText converts raw text bytes using the database header's encoding code
// (0 or 1 for UTF-8, 2 for UTF-16le, 3 for UTF-16be).
func DecodeText(raw []byte, encoding uint32) (string, error) {
	switch encoding {
	case 0, TextEncodingUTF8:
		return string(raw), nil
	case TextEncodingUTF16L, TextEncodingUTF16B:
		if len(raw)%2 != 0 {
			return "", fmt.Errorf("UTF-16 text has an odd length %d", len(raw))
		}
		units := make([]uint16, len(raw)/2)
		for i := range units {
			if encoding == TextEncodingUTF16L {
				units[i] = binary.LittleEndian.Uint16(raw[2*i:])
			} else {
				units[i] = binary.BigEndian.Uint16(raw[2*i:])
			}
		}
		return string(utf16.Decode(units)), nil
	}
	return "", fmt.Errorf("unknown text encoding %d", encoding)
}

// RecordColumn returns the byte offset, byte length, and serial type of 0-based
// column within an SQLite binary record payload without decoding the values.
// Callers use it to locate exact column bytes inside a leaf cell (for example,
// to flip one payload byte in Experiment 9.3).
func RecordColumn(payload []byte, column int) (start, length int, serialType uint64, err error) {
	headerSize, n, err := Varint(payload)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("record header size: %w", err)
	}
	if headerSize < uint64(n) || headerSize > uint64(len(payload)) {
		return 0, 0, 0, fmt.Errorf("record header size %d is outside %d..%d", headerSize, n, len(payload))
	}
	header := payload[n:headerSize]
	start = int(headerSize)
	for i := 0; len(header) > 0; i++ {
		t, used, err := Varint(header)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("record column %d serial type: %w", i, err)
		}
		header = header[used:]
		size, err := SerialTypeLength(t)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("record column %d: %w", i, err)
		}
		if i == column {
			if start+size > len(payload) {
				return 0, 0, 0, fmt.Errorf("record column %d ends at %d, past the %d-byte payload", i, start+size, len(payload))
			}
			return start, size, t, nil
		}
		start += size
	}
	return 0, 0, 0, fmt.Errorf("record has no column %d", column)
}
```

`record.go` decodes SQLite binary records (`DecodeRecord`, `DecodeValue`), mapping every serial type per `sqlite3VdbeSerialGet` ([`src/vdbeaux.c:4123-4214`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L4123-L4214)). It sign-extends big-endian 1-, 2-, 3-, 4-, 6-, and 8-byte integers, converts IEEE `NaN` floats to `NULL`, rejects reserved serial types `10` and `11`, and decodes UTF-8, UTF-16le, and UTF-16be strings (`DecodeText`).

#### `internal/dbfile/tree.go`

<!-- include: ../internal/dbfile/tree.go -->
```go
package dbfile

import "fmt"

// MaxTreeDepth matches SQLite's BTCURSOR_MAX_DEPTH (20), bounding B-tree
// recursion against corrupt deep or cyclic trees.
const MaxTreeDepth = 20

// TreeWalker traverses one table or index B-tree in key order, invoking OnPage
// on entry to each page and OnCell for each cell (after descending into that
// cell's left child on interior pages).
type TreeWalker struct {
	File   *File
	OnPage func(p *BTreePage, parent int, depth int) error
	OnCell func(p *BTreePage, c Cell) error
}

// Walk traverses the B-tree rooted at page root, rejecting cycles, depth >=
// MaxTreeDepth, mixed table/index page types, and invalid child page numbers.
func (w TreeWalker) Walk(root int) error {
	rootPage, err := w.File.BTreePage(root)
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	return w.walk(rootPage, rootPage.Type.IsTable(), 0, 0, seen)
}

func (w TreeWalker) walk(p *BTreePage, table bool, parent, depth int, seen map[int]bool) error {
	if depth >= MaxTreeDepth {
		return corruptPage(p.Number, "b-tree is deeper than %d levels", MaxTreeDepth)
	}
	if seen[p.Number] {
		return corruptPage(parent, "b-tree points to page %d a second time", p.Number)
	}
	seen[p.Number] = true
	if p.Type.IsTable() != table {
		return corruptPage(p.Number, "%s page inside a %s b-tree", p.Type, treeKind(table))
	}
	if w.OnPage != nil {
		if err := w.OnPage(p, parent, depth); err != nil {
			return err
		}
	}
	for i := range p.CellCount {
		c, err := p.Cell(i)
		if err != nil {
			return err
		}
		if !p.Type.IsLeaf() {
			if err := w.descend(p, int(c.LeftChild), table, depth, seen); err != nil {
				return err
			}
		}
		if w.OnCell != nil {
			if err := w.OnCell(p, c); err != nil {
				return err
			}
		}
	}
	if !p.Type.IsLeaf() {
		return w.descend(p, int(p.RightChild), table, depth, seen)
	}
	return nil
}

func (w TreeWalker) descend(p *BTreePage, child int, table bool, depth int, seen map[int]bool) error {
	// Page 1 can only be sqlite_schema's root, never a child; lock-byte and
	// pointer-map pages are never B-tree pages.
	if child < 2 || child > w.File.PageCount() || child == w.File.LockBytePage() || w.File.IsPointerMapPage(child) {
		return corruptPage(p.Number, "child pointer to page %d, which cannot be a b-tree page", child)
	}
	childPage, err := w.File.BTreePage(child)
	if err != nil {
		return err
	}
	return w.walk(childPage, table, p.Number, depth+1, seen)
}

func treeKind(table bool) string {
	if table {
		return "table"
	}
	return "index"
}

// Row holds one decoded table row along with its leaf page and any overflow
// page numbers visited while assembling its payload.
type Row struct {
	Rowid         int64
	Values        []Value
	Page          int
	OverflowPages []int
}

// WalkTable scans the table B-tree rooted at page root in rowid order, calling
// visit for each decoded leaf row.
func (f *File) WalkTable(root int, visit func(Row) error) error {
	walker := TreeWalker{File: f}
	walker.OnPage = func(p *BTreePage, parent, depth int) error {
		if depth == 0 && !p.Type.IsTable() {
			return corruptPage(p.Number, "page %d is the root of an index, not a table", p.Number)
		}
		return nil
	}
	walker.OnCell = func(p *BTreePage, c Cell) error {
		if p.Type != TableLeaf {
			return nil
		}
		payload, chain, err := f.Payload(p.Number, c)
		if err != nil {
			return err
		}
		values, err := DecodeRecord(payload)
		if err != nil {
			return corruptAt(p.Number, c.Offset, "row %d: %v", c.Rowid, err)
		}
		row := Row{Rowid: c.Rowid, Values: values, Page: p.Number}
		for _, o := range chain {
			row.OverflowPages = append(row.OverflowPages, o.Number)
		}
		return visit(row)
	}
	return walker.Walk(root)
}

// SchemaEntry represents one row of sqlite_schema (rooted at page 1).
type SchemaEntry struct {
	Type      string
	Name      string
	TableName string
	RootPage  int
	SQL       string
	Rowid     int64
}

func (f *File) textValue(v Value, what string) (string, error) {
	switch v.Class {
	case Null:
		return "", nil
	case Text:
		return DecodeText(v.Bytes, f.Header.TextEncoding)
	}
	return "", fmt.Errorf("schema column %s holds %s, want text", what, v.Class)
}

// Schema decodes every row of sqlite_schema on page 1 (columns: type, name,
// tbl_name, rootpage, sql) to discover all table and index B-tree root pages.
func (f *File) Schema() ([]SchemaEntry, error) {
	var entries []SchemaEntry
	err := f.WalkTable(1, func(row Row) error {
		if len(row.Values) < 5 {
			return corruptPage(row.Page, "schema row %d has %d columns, want 5", row.Rowid, len(row.Values))
		}
		e := SchemaEntry{Rowid: row.Rowid}
		var err error
		names := []string{"type", "name", "tbl_name"}
		targets := []*string{&e.Type, &e.Name, &e.TableName}
		for i, target := range targets {
			if *target, err = f.textValue(row.Values[i], names[i]); err != nil {
				return corruptPage(row.Page, "schema row %d: %v", row.Rowid, err)
			}
		}
		if e.SQL, err = f.textValue(row.Values[4], "sql"); err != nil {
			return corruptPage(row.Page, "schema row %d: %v", row.Rowid, err)
		}
		// Views and triggers have rootpage 0 or NULL; tables and indexes must point inside 1..pageCount.
		switch root := row.Values[3]; root.Class {
		case Null:
		case Integer:
			if root.Int < 0 || root.Int > int64(f.pageCount) {
				return corruptPage(row.Page, "schema row %d (%s): root page %d is outside 0..%d", row.Rowid, e.Name, root.Int, f.pageCount)
			}
			e.RootPage = int(root.Int)
		default:
			return corruptPage(row.Page, "schema row %d (%s): root page holds %s", row.Rowid, e.Name, root.Class)
		}
		entries = append(entries, e)
		return nil
	})
	return entries, err
}
```

`tree.go` walks any table or index B-tree in key order (`TreeWalker`), bounding depth to `MaxTreeDepth = 20` (`BTCURSOR_MAX_DEPTH`), rejecting page cycles, mixed table/index page types, and invalid child page numbers. `WalkTable` decodes every leaf row along with its leaf and overflow page numbers, and `Schema()` walks page 1 (`sqlite_schema`) to collect every table and index root page.

#### `internal/dbfile/census.go`

<!-- include: ../internal/dbfile/census.go -->
```go
package dbfile

import (
	"fmt"
	"sort"
)

// Role classifies the sole structural job of one page in an SQLite file.
type Role int

// Page roles assigned by File.Census.
const (
	// RoleUnaccounted marks a page not reached by any tree, overflow chain, freelist, pointer map, or lock byte.
	RoleUnaccounted Role = iota
	// RoleTableInterior marks an interior table B-tree page (type 0x05).
	RoleTableInterior
	// RoleTableLeaf marks a leaf table B-tree page (type 0x0d).
	RoleTableLeaf
	// RoleIndexInterior marks an interior index B-tree page (type 0x02).
	RoleIndexInterior
	// RoleIndexLeaf marks a leaf index B-tree page (type 0x0a).
	RoleIndexLeaf
	// RoleOverflow marks a cell payload overflow page.
	RoleOverflow
	// RoleFreelistTrunk marks a freelist trunk page holding next-trunk and leaf page numbers.
	RoleFreelistTrunk
	// RoleFreelistLeaf marks a free page listed by a freelist trunk.
	RoleFreelistLeaf
	// RolePointerMap marks an auto-vacuum pointer-map page.
	RolePointerMap
	// RoleLockByte marks the reserved page containing byte offset 1 GiB (1073741824).
	RoleLockByte
)

// AllRoles lists every Role in the display order used by census summaries.
var AllRoles = []Role{RoleTableInterior, RoleTableLeaf, RoleIndexInterior, RoleIndexLeaf, RoleOverflow, RoleFreelistTrunk, RoleFreelistLeaf, RolePointerMap, RoleLockByte, RoleUnaccounted}

// String returns the canonical kebab-case name of r.
func (r Role) String() string {
	switch r {
	case RoleUnaccounted:
		return "unaccounted"
	case RoleTableInterior:
		return "table-interior"
	case RoleTableLeaf:
		return "table-leaf"
	case RoleIndexInterior:
		return "index-interior"
	case RoleIndexLeaf:
		return "index-leaf"
	case RoleOverflow:
		return "overflow"
	case RoleFreelistTrunk:
		return "freelist-trunk"
	case RoleFreelistLeaf:
		return "freelist-leaf"
	case RolePointerMap:
		return "pointer-map"
	case RoleLockByte:
		return "lock-byte"
	}
	return fmt.Sprintf("role(%d)", int(r))
}

// ParseRole looks up a Role by its canonical String name.
func ParseRole(name string) (Role, bool) {
	for _, r := range AllRoles {
		if r.String() == name {
			return r, true
		}
	}
	return 0, false
}

func roleOf(t PageType) Role {
	switch t {
	case TableInterior:
		return RoleTableInterior
	case TableLeaf:
		return RoleTableLeaf
	case IndexInterior:
		return RoleIndexInterior
	}
	return RoleIndexLeaf
}

// PageInfo records one page's census role, owning tree, parent page, and the
// per-page statistics comparable to SQLite's dbstat virtual table.
type PageInfo struct {
	Number     int
	Role       Role
	Tree       string
	Parent     int
	CellCount  int
	Payload    int
	Unused     int
	MaxPayload uint64
}

// Tree summarizes one B-tree discovered during a census.
type Tree struct {
	Name     string
	Type     string
	RootPage int
	Pages    int
	Overflow int
	Cells    int
}

// Census holds the complete page-by-page accounting of a database file.
type Census struct {
	PageSize       int
	UsableSize     int
	PageCount      int
	Pages          []PageInfo
	Trees          []Tree
	Schema         []SchemaEntry
	FreelistTrunks []int
	FreelistLeaves []int
	Unaccounted    []int
}

// Page returns the PageInfo for 1-based page number n.
func (c *Census) Page(n int) PageInfo { return c.Pages[n-1] }

// RoleCounts tallies how many pages hold each Role.
func (c *Census) RoleCounts() map[Role]int {
	counts := map[Role]int{}
	for _, p := range c.Pages {
		counts[p.Role]++
	}
	return counts
}

// FreelistLength returns the total number of freelist pages (trunks plus leaves).
func (c *Census) FreelistLength() int { return len(c.FreelistTrunks) + len(c.FreelistLeaves) }

// Complete returns an error naming unaccounted pages if any page was left
// unclaimed by the census.
func (c *Census) Complete() error {
	if len(c.Unaccounted) == 0 {
		return nil
	}
	shown := c.Unaccounted
	if len(shown) > 10 {
		shown = shown[:10]
	}
	return corruptPage(c.Unaccounted[0], "%d page(s) have no role: %v", len(c.Unaccounted), shown)
}

type censusBuilder struct {
	file   *File
	census *Census
}

func (info PageInfo) describe() string {
	if info.Tree == "" {
		return info.Role.String()
	}
	return fmt.Sprintf("%s of %q", info.Role, info.Tree)
}

func (b *censusBuilder) claim(n int, info PageInfo) error {
	if n < 1 || n > b.census.PageCount {
		return corruptPage(max(n, 0), "page %d (%s) is outside 1..%d", n, info.describe(), b.census.PageCount)
	}
	existing := b.census.Pages[n-1]
	if existing.Role != RoleUnaccounted {
		return corruptPage(n, "page claimed twice: as %s and as %s", existing.describe(), info.describe())
	}
	info.Number = n
	b.census.Pages[n-1] = info
	return nil
}

// Census walks every structural owner in f—the lock-byte page, auto-vacuum
// pointer-map pages, sqlite_schema, every table and index B-tree with its
// overflow chains, and the freelist—and assigns every page at most one role.
// Any page claimed twice returns ErrCorrupt; pages never reached are listed in
// Census.Unaccounted.
func (f *File) Census() (*Census, error) {
	c := &Census{PageSize: f.PageSize(), UsableSize: f.UsableSize(), PageCount: f.pageCount, Pages: make([]PageInfo, f.pageCount)}
	for i := range c.Pages {
		c.Pages[i].Number = i + 1
	}
	b := &censusBuilder{file: f, census: c}
	if lock := f.LockBytePage(); lock != 0 {
		if err := b.claim(lock, PageInfo{Role: RoleLockByte}); err != nil {
			return nil, err
		}
	}
	if f.Header.AutoVacuum() {
		for n := 2; n <= f.pageCount; n++ {
			if f.IsPointerMapPage(n) {
				if err := b.claim(n, PageInfo{Role: RolePointerMap}); err != nil {
					return nil, err
				}
			}
		}
	}
	schema, err := f.Schema()
	if err != nil {
		return nil, fmt.Errorf("schema table: %w", err)
	}
	c.Schema = schema
	trees := []Tree{{Name: "sqlite_schema", Type: "table", RootPage: 1}}
	for _, e := range schema {
		if e.RootPage > 0 {
			trees = append(trees, Tree{Name: e.Name, Type: e.Type, RootPage: e.RootPage})
		}
	}
	for i := range trees {
		if err := b.walkTree(&trees[i]); err != nil {
			return nil, fmt.Errorf("tree %q (root %d): %w", trees[i].Name, trees[i].RootPage, err)
		}
	}
	c.Trees = trees
	if err := b.walkFreelist(); err != nil {
		return nil, fmt.Errorf("freelist: %w", err)
	}
	for _, p := range c.Pages {
		if p.Role == RoleUnaccounted {
			c.Unaccounted = append(c.Unaccounted, p.Number)
		}
	}
	return c, nil
}

func (b *censusBuilder) walkTree(t *Tree) error {
	f := b.file
	walker := TreeWalker{File: f}
	walker.OnPage = func(p *BTreePage, parent, depth int) error {
		// A WITHOUT ROWID table has type "table" in sqlite_schema while storing
		// its rows in an index B-tree (0x02/0x0a), so only an index entry whose
		// root is a table B-tree page is corrupt.
		if depth == 0 && p.Type.IsTable() && t.Type != "table" {
			return corruptPage(p.Number, "root of %s %q is a %s page", t.Type, t.Name, p.Type)
		}
		t.Pages++
		t.Cells += p.CellCount
		return b.claim(p.Number, PageInfo{Role: roleOf(p.Type), Tree: t.Name, Parent: parent, CellCount: p.CellCount, Unused: p.Unused()})
	}
	walker.OnCell = func(p *BTreePage, c Cell) error {
		info := &b.census.Pages[p.Number-1]
		if p.Type == TableInterior {
			return nil
		}
		info.Payload += c.LocalSize
		info.MaxPayload = max(info.MaxPayload, c.PayloadSize)
		chain, err := f.OverflowChain(p.Number, c)
		if err != nil {
			return err
		}
		perPage := f.UsableSize() - 4
		for _, o := range chain {
			t.Overflow++
			// Overflow unused bytes are (usableSize - 4) - payload, matching dbstat.
			err := b.claim(o.Number, PageInfo{Role: RoleOverflow, Tree: t.Name, Parent: p.Number, Payload: o.Payload, Unused: perPage - o.Payload})
			if err != nil {
				return err
			}
		}
		return nil
	}
	return walker.Walk(t.RootPage)
}

func (b *censusBuilder) walkFreelist() error {
	f := b.file
	want := int(f.Header.FreelistCount)
	// Readers accept up to usableSize/4 - 2 leaf entries per trunk (allocateBtreePage
	// src/btree.c:6621, freePage2 src/btree.c:6906), even though writers stop at -8
	// for pre-3.6.0 compatibility (src/btree.c:6910-6928).
	maxLeaves := f.UsableSize()/4 - 2
	counted := 0
	trunk := int(f.Header.FirstFreelistTrunk)
	parent := 0
	for trunk != 0 {
		if counted >= want {
			return corruptPage(trunk, "freelist continues past the %d pages the header counts", want)
		}
		if err := b.claim(trunk, PageInfo{Role: RoleFreelistTrunk, Parent: parent}); err != nil {
			return err
		}
		counted++
		b.census.FreelistTrunks = append(b.census.FreelistTrunks, trunk)
		data, err := f.Page(trunk)
		if err != nil {
			return err
		}
		leaves := be32int(data[4:8])
		if leaves > maxLeaves {
			return corruptAt(trunk, 4, "trunk lists %d leaves, at most %d fit", leaves, maxLeaves)
		}
		b.census.Pages[trunk-1].CellCount = leaves
		for i := range leaves {
			leaf := be32int(data[8+4*i:])
			if leaf == 1 || leaf == f.LockBytePage() {
				return corruptAt(trunk, 8+4*i, "freelist leaf %d can never be free", leaf)
			}
			if err := b.claim(leaf, PageInfo{Role: RoleFreelistLeaf, Parent: trunk}); err != nil {
				return err
			}
			b.census.FreelistLeaves = append(b.census.FreelistLeaves, leaf)
		}
		counted += leaves
		parent = trunk
		trunk = be32int(data[0:4])
	}
	if counted != want {
		return corruptAt(0, 36, "header counts %d freelist pages, the trunk chain holds %d", want, counted)
	}
	return nil
}

// SortedTrees returns a copy of c.Trees ordered by RootPage.
func (c *Census) SortedTrees() []Tree {
	out := append([]Tree(nil), c.Trees...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].RootPage < out[j].RootPage })
	return out
}
```

`census.go` assigns every page in the file at most one `Role`. It claims the lock-byte page (if the file exceeds 1 GiB) and any auto-vacuum pointer-map pages, decodes `sqlite_schema`, walks every B-tree and overflow chain, and traverses the freelist from header offset 32 while verifying the total against header offset 36. A `table` entry in `sqlite_schema` may point to an index B-tree root (`WITHOUT ROWID` tables store rows in index B-trees, so their pages take `index-interior`, `index-leaf`, or `overflow` roles while `Tree.Type` stays `"table"`); only an `index` entry pointing to a table B-tree root is refused (`WalkTable` still refuses index roots because decoding every table kind is out of scope). Any page claimed twice returns `ErrCorrupt` naming both owners, and any page left unclaimed appears in `Unaccounted` and is reported by `Complete()`.

The reachability graph below summarizes how `File.Census` traces every page role from page 1's header:

<!-- draw-visual: diagrams/ch02-file-format-and-pages-page-census.mmd -->
```text
┌──────────┐      ┌──────────────┐      ┌─────────────┐     ┌────────┐
│Page 1 hdr├─100─►│sqlite_schema ├─tbl─►│ table B-tree├─ov─►│overflow│
└─────┬────┘      └───────┬──────┘      └─────────────┘     └────────┘
      │                   │                                      ▲
      │                   └──────────┐                           │
      │           ┌──────────────┐  idx ┌─────────────┐          │
      ├──────52──►│ pointer-map  │   └─►│ index B-tree├───ov─────┘
      │           └──────────────┘      └─────────────┘
      │
      │
      │           ┌──────────────┐      ┌─────────────┐
      ├──────32──►│freelist-trunk├─pg──►│freelist-leaf│
      │           └──────────────┘      └─────────────┘
      │
      │
      │           ┌──────────────┐
      └──────1G──►│  lock-byte   │
                  └──────────────┘
```

Edge key: `100` = B-tree header at offset 100 on page 1; `52` = auto-vacuum flag at header offset 52; `32` = first freelist trunk pointer at header offset 32; `1G` = 1 GiB lock-byte offset; `tbl`/`idx` = `sqlite_schema` root pages; `ov` = overflow chains; `pg` = freelist leaf entries.

### 5.2 The `internal/equal` package

`internal/equal` implements the book's three-level equality ladder (E1 exact bytes, E2 pages ignoring volatile header bytes, and E3 logical schema and row hashes) and its per-page manifest format. Chapter 2 adds level **E1 bytes** (`E1Bytes`); Chapter 3 adds levels E2 and E3 without breaking E1's `Result` type. Unit tests live in [`internal/equal/equal_test.go`](../internal/equal/equal_test.go) (`make test PACKAGE=./internal/equal`).

#### `internal/equal/equal.go`

<!-- include: ../internal/equal/equal.go -->
```go
// Package equal implements the course's Rebuild Ladder equality checks and
// per-page hash manifests.
//
// Chapter 2 introduces level E1 (exact byte equality via SHA-256 and page-by-page
// diffing) and the text manifest format; Chapter 3 extends the package with
// levels E2 (page comparison ignoring volatile header bytes) and E3 (logical
// content hash).
package equal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// Level identifies a rung on the course's three-level equality ladder.
type Level int

// Equality levels supported by this package.
const (
	// E1Bytes checks exact byte-for-byte equality via SHA-256 and locates the first differing page.
	E1Bytes Level = 1
	// E2Pages compares every page while ignoring a declared list of volatile header byte ranges.
	E2Pages Level = 2
	// E3Logical hashes schema, rows (with explicit rowids and storage classes), and persistent pragmas.
	E3Logical Level = 3
)

// String returns the human-readable label for equality level l.
func (l Level) String() string {
	switch l {
	case E1Bytes:
		return "E1 bytes"
	case E2Pages:
		return "E2 pages"
	case E3Logical:
		return "E3 logical"
	}
	return fmt.Sprintf("E%d", int(l))
}

// Result summarizes the outcome of comparing two database images or files at a
// given Level, including both SHA-256 digests, sizes, differing-page counts,
// and (for E2Pages) any ignored header byte ranges and differing offsets within them.
type Result struct {
	Level              Level
	Equal              bool
	SHA256A            string
	SHA256B            string
	SizeA              int64
	SizeB              int64
	PageSize           int
	FirstDifferingPage int
	DifferingPages     int
	Ignored            []ByteRange
	IgnoredDiffering   []int
}

// String formats r as a single-line equality verdict for experiment output.
func (r Result) String() string {
	if r.Equal && len(r.IgnoredDiffering) > 0 {
		return fmt.Sprintf("%s: equal ignoring header bytes %s (bytes differ only at %s)", r.Level, FormatRanges(r.Ignored), formatOffsets(r.IgnoredDiffering))
	}
	if r.Equal {
		return fmt.Sprintf("%s: equal (sha256 %s)", r.Level, r.SHA256A)
	}
	text := fmt.Sprintf("%s: differ", r.Level)
	if r.FirstDifferingPage > 0 {
		text += fmt.Sprintf(", first differing page %d, %d page(s) differ", r.FirstDifferingPage, r.DifferingPages)
	}
	if r.SizeA != r.SizeB {
		text += fmt.Sprintf(", sizes %d and %d bytes", r.SizeA, r.SizeB)
	}
	return text
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReaderSHA256 streams r to EOF, returning its lowercase hex SHA-256 digest and
// byte count.
func ReaderSHA256(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FileSHA256 hashes the contents of path and returns its lowercase hex SHA-256 digest.
// Do not call on a file currently open by an SQLite connection in the same process.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum, _, err := ReaderSHA256(f)
	return sum, err
}

// CompareImages compares two raw database images at level E1Bytes. When they
// differ and pageSize > 0, it walks every page slot to record the 1-based first
// differing page and total differing page count.
func CompareImages(a, b []byte, pageSize int) Result {
	r := Result{Level: E1Bytes, SHA256A: SHA256Hex(a), SHA256B: SHA256Hex(b), SizeA: int64(len(a)), SizeB: int64(len(b)), PageSize: pageSize}
	r.Equal = r.SHA256A == r.SHA256B
	if r.Equal || pageSize <= 0 {
		return r
	}
	pages := (max(len(a), len(b)) + pageSize - 1) / pageSize
	for p := range pages {
		if !bytes.Equal(pageOf(a, p, pageSize), pageOf(b, p, pageSize)) {
			if r.FirstDifferingPage == 0 {
				r.FirstDifferingPage = p + 1
			}
			r.DifferingPages++
		}
	}
	return r
}

func pageOf(image []byte, index, pageSize int) []byte {
	start := index * pageSize
	if start >= len(image) {
		return nil
	}
	return image[start:min(start+pageSize, len(image))]
}

// CompareFiles reads both files from disk and compares them with CompareImages.
func CompareFiles(a, b string, pageSize int) (Result, error) {
	imageA, err := os.ReadFile(a)
	if err != nil {
		return Result{}, err
	}
	imageB, err := os.ReadFile(b)
	if err != nil {
		return Result{}, err
	}
	return CompareImages(imageA, imageB, pageSize), nil
}
```

`equal.go` computes SHA-256 digests (`SHA256Hex`, `ReaderSHA256`, `FileSHA256`) and compares two database images or files page by page (`CompareImages`, `CompareFiles`), reporting both digests, both byte sizes, the 1-based `FirstDifferingPage`, and total `DifferingPages`.

#### `internal/equal/manifest.go`

<!-- include: ../internal/equal/manifest.go -->
```go
package equal

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ManifestFormat is the version header line required at line 1 of every page manifest.
const ManifestFormat = "sqlite-internals page manifest v1"

// ManifestEntry records one page's 1-based number, census role name, and SHA-256
// hex digest. Role is a plain string so manifests parse without importing dbfile.
type ManifestEntry struct {
	Page   int
	Role   string
	SHA256 string
}

// String formats e as one manifest line: "<page> <role> <sha256>".
func (e ManifestEntry) String() string {
	return fmt.Sprintf("%d %s %s", e.Page, e.Role, e.SHA256)
}

// Manifest is an ordered per-page hash inventory of a database file.
type Manifest struct {
	PageSize int
	Entries  []ManifestEntry
}

// PageCount returns the number of page entries in m.
func (m Manifest) PageCount() int { return len(m.Entries) }

// Text serializes m into the canonical v1 line-oriented manifest format.
func (m Manifest) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\npage-size %d\npages %d\n", ManifestFormat, m.PageSize, len(m.Entries))
	for _, e := range m.Entries {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// SHA256 returns the SHA-256 hex digest of m's serialized Text.
func (m Manifest) SHA256() string { return SHA256Hex([]byte(m.Text())) }

var (
	roleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseManifest reads and strictly validates a v1 page manifest from r,
// verifying the header lines, role/hash syntax, and declared page count.
func ParseManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	scanner := bufio.NewScanner(r)
	line := 0
	next := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		line++
		return scanner.Text(), true
	}
	if text, ok := next(); !ok || text != ManifestFormat {
		return m, fmt.Errorf("manifest line 1: want %q", ManifestFormat)
	}
	readNumber := func(key string) (int, error) {
		text, ok := next()
		value, found := strings.CutPrefix(text, key+" ")
		if !ok || !found {
			return 0, fmt.Errorf("manifest line %d: want %q", line, key+" N")
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("manifest line %d: bad %s %q", line, key, value)
		}
		return n, nil
	}
	var err error
	if m.PageSize, err = readNumber("page-size"); err != nil {
		return m, err
	}
	declared, err := readNumber("pages")
	if err != nil {
		return m, err
	}
	for {
		text, ok := next()
		if !ok {
			break
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return m, fmt.Errorf("manifest line %d: want 'page role sha256', got %q", line, text)
		}
		page, err := strconv.Atoi(fields[0])
		if err != nil || page < 1 {
			return m, fmt.Errorf("manifest line %d: bad page number %q", line, fields[0])
		}
		if !roleName.MatchString(fields[1]) || !sha256Re.MatchString(fields[2]) {
			return m, fmt.Errorf("manifest line %d: bad role or sha256 in %q", line, text)
		}
		m.Entries = append(m.Entries, ManifestEntry{Page: page, Role: fields[1], SHA256: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return m, err
	}
	if declared != len(m.Entries) {
		return m, fmt.Errorf("manifest declares %d pages and lists %d", declared, len(m.Entries))
	}
	return m, nil
}

// SequenceError reports a missing, duplicate, or out-of-order page entry in a Manifest.
type SequenceError struct {
	Line    int
	Page    int
	Problem string
}

// Error formats the entry index, page number, and sequence violation.
func (e *SequenceError) Error() string {
	return fmt.Sprintf("manifest entry %d: page %d: %s", e.Line, e.Page, e.Problem)
}

// CheckSequence verifies that m lists pages 1..N exactly once and in ascending
// order. Duplicates are checked before gaps so a repeated page number is named
// directly rather than misreported as a missing neighbor.
func (m Manifest) CheckSequence() error {
	seenAt := map[int]int{}
	for i, e := range m.Entries {
		if first, dup := seenAt[e.Page]; dup {
			return &SequenceError{Line: i + 1, Page: e.Page, Problem: fmt.Sprintf("listed twice (entries %d and %d)", first, i+1)}
		}
		seenAt[e.Page] = i + 1
	}
	for i, e := range m.Entries {
		want := i + 1
		if e.Page == want {
			continue
		}
		if _, present := seenAt[want]; !present {
			return &SequenceError{Line: want, Page: want, Problem: fmt.Sprintf("missing (entry %d lists page %d)", want, e.Page)}
		}
		return &SequenceError{Line: want, Page: e.Page, Problem: fmt.Sprintf("out of order (entry %d should be page %d)", want, want)}
	}
	return nil
}

// ManifestDiff summarizes the entry-by-entry comparison of two Manifest values.
type ManifestDiff struct {
	Equal              bool
	FirstDifferingPage int
	DifferingPages     int
	Reason             string
}

// String formats d as a single-line summary naming the first differing page and reason.
func (d ManifestDiff) String() string {
	if d.Equal {
		return "manifests identical"
	}
	if d.FirstDifferingPage == 0 {
		return "manifests differ: " + d.Reason
	}
	return fmt.Sprintf("manifests differ: first differing page %d (%s), %d page(s) differ", d.FirstDifferingPage, d.Reason, d.DifferingPages)
}

// CompareManifests compares two manifests page by page, reporting the first
// mismatching page, the reason for the first mismatch, and the count of
// differing entries.
func CompareManifests(a, b Manifest) ManifestDiff {
	if a.PageSize != b.PageSize {
		return ManifestDiff{Reason: fmt.Sprintf("page size %d vs %d", a.PageSize, b.PageSize)}
	}
	d := ManifestDiff{}
	for i := range max(len(a.Entries), len(b.Entries)) {
		var ea, eb ManifestEntry
		var reason string
		switch {
		case i >= len(a.Entries):
			eb = b.Entries[i]
			reason = "only in the second manifest"
		case i >= len(b.Entries):
			ea = a.Entries[i]
			reason = "only in the first manifest"
		default:
			ea, eb = a.Entries[i], b.Entries[i]
			switch {
			case ea.Page != eb.Page:
				reason = fmt.Sprintf("entry %d lists page %d vs %d", i+1, ea.Page, eb.Page)
			case ea.Role != eb.Role:
				reason = fmt.Sprintf("role %s vs %s", ea.Role, eb.Role)
			case ea.SHA256 != eb.SHA256:
				reason = "page image differs"
			}
		}
		if reason == "" {
			continue
		}
		if d.FirstDifferingPage == 0 {
			d.FirstDifferingPage = i + 1
			d.Reason = reason
		}
		d.DifferingPages++
	}
	d.Equal = d.DifferingPages == 0
	return d
}
```

`manifest.go` serializes, parses, validates, and diffs the line-oriented `sqlite-internals page manifest v1` format (`page-size N`, `pages N`, then `<page> <role> <sha256>` per line). `CheckSequence()` verifies pages `1..N` appear once in ascending order—checking duplicates before gaps so a repeated page is named directly—and `CompareManifests` reports the first differing page and reason (`page image differs`, role mismatch, or length mismatch). Roles are plain strings so `internal/equal` does not depend on `internal/dbfile`.

### 5.3 The `internal/capture` package

`internal/capture` implements Rebuild Ladder rung **R1 (Pages)** on top of `internal/dbfile` and `internal/equal`. Unit tests live in [`internal/capture/capture_test.go`](../internal/capture/capture_test.go) (`make test PACKAGE=./internal/capture`).

#### `internal/capture/capture.go`

<!-- include: ../internal/capture/capture.go -->
```go
// Package capture implements Rebuild Ladder rung R1: capturing every page of
// a closed SQLite database into raw page images plus a per-page hash manifest,
// and rebuilding a verified database file from that capture alone.
package capture

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
)

// Standard file names written inside a rung R1 capture directory.
const (
	// ManifestFile holds the serialized equal.Manifest text listing every page's role and SHA-256.
	ManifestFile = "manifest.txt"
	// PagesFile holds the raw page images concatenated in 1..N page order.
	PagesFile = "pages.bin"
)

// ErrNotAtRest is wrapped by NotAtRestError when a database has uncheckpointed
// WAL frames or a hot rollback journal.
var ErrNotAtRest = errors.New("database is not at rest")

// NotAtRestError explains why dbPath cannot safely be captured as a standalone file.
type NotAtRestError struct {
	Path   string
	Reason string
}

// Error formats the sentinel error, database path, and specific sidecar reason.
func (e *NotAtRestError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrNotAtRest, e.Path, e.Reason)
}

// Unwrap returns ErrNotAtRest for errors.Is matching.
func (e *NotAtRestError) Unwrap() error { return ErrNotAtRest }

// CheckAtRest verifies that dbPath is complete on its own: no non-empty -wal
// file and no hot -journal file. A zero-byte -wal (after TRUNCATE checkpoint or
// PERSIST_WAL) and a zero-byte or zero-header -journal (TRUNCATE/PERSIST modes)
// are accepted as at rest.
func CheckAtRest(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	wal, err := os.Stat(dbPath + "-wal")
	switch {
	case err == nil && wal.Size() > 0:
		return &NotAtRestError{Path: dbPath, Reason: fmt.Sprintf("-wal holds %d bytes of frames not yet checkpointed into the database file", wal.Size())}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	hot, size, err := hotJournal(dbPath + "-journal")
	if err != nil {
		return err
	}
	if hot {
		return &NotAtRestError{Path: dbPath, Reason: fmt.Sprintf("hot -journal (%d bytes with a live header): a transaction was interrupted and the next open will roll it back", size)}
	}
	return nil
}

// hotJournal mirrors SQLite's hasHotJournal test (src/pager.c:5180): a rollback
// journal is hot when it exists, is non-empty, and its first header byte is
// non-zero (PERSIST mode zeroes the header on commit).
func hotJournal(path string) (bool, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, 0, err
	}
	if info.Size() == 0 {
		return false, 0, nil
	}
	first := make([]byte, 1)
	if _, err := f.ReadAt(first, 0); err != nil {
		return false, info.Size(), err
	}
	return first[0] != 0, info.Size(), nil
}

// BuildManifest runs a complete page census on f and hashes every page 1..N.
func BuildManifest(f *dbfile.File) (equal.Manifest, error) {
	census, err := f.Census()
	if err != nil {
		return equal.Manifest{}, err
	}
	if err := census.Complete(); err != nil {
		return equal.Manifest{}, err
	}
	m := equal.Manifest{PageSize: f.PageSize(), Entries: make([]equal.ManifestEntry, 0, f.PageCount())}
	for n := 1; n <= f.PageCount(); n++ {
		page, err := f.Page(n)
		if err != nil {
			return equal.Manifest{}, err
		}
		m.Entries = append(m.Entries, equal.ManifestEntry{Page: n, Role: census.Page(n).Role.String(), SHA256: equal.SHA256Hex(page)})
	}
	return m, nil
}

// Pages captures a closed database at dbPath into dir as pages.bin plus
// manifest.txt. It refuses databases that are not at rest or whose byte size
// disagrees with PageCount*PageSize, removes any prior capture in dir, writes
// temporary .capture-tmp files, re-checks at-rest status, size, and mtime, and
// renames the files into place only if every check passes so a failed capture
// leaves no partial or stale capture behind.
func Pages(dbPath, dir string) (equal.Manifest, error) {
	if err := CheckAtRest(dbPath); err != nil {
		return equal.Manifest{}, err
	}
	before, err := os.Stat(dbPath)
	if err != nil {
		return equal.Manifest{}, err
	}
	f, err := dbfile.Open(dbPath)
	if err != nil {
		return equal.Manifest{}, err
	}
	defer f.Close()
	if int64(f.PageCount())*int64(f.PageSize()) != f.FileSize() {
		return equal.Manifest{}, fmt.Errorf("%s: file holds %d bytes but the database is %d pages of %d bytes; a page capture would not rebuild the file", dbPath, f.FileSize(), f.PageCount(), f.PageSize())
	}
	m, err := BuildManifest(f)
	if err != nil {
		return equal.Manifest{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return equal.Manifest{}, err
	}
	pagesPath, manifestPath := filepath.Join(dir, PagesFile), filepath.Join(dir, ManifestFile)
	pagesTmp, manifestTmp := pagesPath+captureTmpSuffix, manifestPath+captureTmpSuffix
	removeAll := func(paths ...string) {
		for _, p := range paths {
			os.Remove(p)
		}
	}
	for _, p := range []string{manifestPath, pagesPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return equal.Manifest{}, err
		}
	}
	if err := writeCapture(f, dbPath, before, m, pagesTmp, manifestTmp); err != nil {
		removeAll(pagesTmp, manifestTmp)
		return equal.Manifest{}, err
	}
	if err := os.Rename(pagesTmp, pagesPath); err != nil {
		removeAll(pagesTmp, manifestTmp)
		return equal.Manifest{}, err
	}
	if err := os.Rename(manifestTmp, manifestPath); err != nil {
		removeAll(pagesPath, manifestTmp)
		return equal.Manifest{}, err
	}
	return m, nil
}

// captureTmpSuffix is appended to pages.bin and manifest.txt until the final
// post-capture stability checks pass.
const captureTmpSuffix = ".capture-tmp"

// afterCaptureWritten is a test hook invoked after the temp files are written
// and before the final at-rest and stat checks run.
var afterCaptureWritten = func(dbPath string) {}

// writeCapture streams pages.bin and manifest.txt to temporary paths and then
// verifies the source database remained at rest with unchanged size and mtime.
func writeCapture(f *dbfile.File, dbPath string, before os.FileInfo, m equal.Manifest, pagesPath, manifestPath string) error {
	if err := writePages(f, pagesPath, m); err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, []byte(m.Text()), 0o644); err != nil {
		return err
	}
	afterCaptureWritten(dbPath)
	if err := CheckAtRest(dbPath); err != nil {
		return fmt.Errorf("database changed during the capture: %w", err)
	}
	after, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("%s: database changed during the capture", dbPath)
	}
	return nil
}

// writePages writes every page image in manifest order to path, re-hashing each
// page as it goes so a concurrent overwrite fails immediately.
func writePages(f *dbfile.File, path string, m equal.Manifest) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	for _, e := range m.Entries {
		page, err := f.Page(e.Page)
		if err != nil {
			out.Close()
			return err
		}
		if equal.SHA256Hex(page) != e.SHA256 {
			out.Close()
			return fmt.Errorf("page %d changed while it was captured", e.Page)
		}
		if _, err := w.Write(page); err != nil {
			out.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ReadManifest opens and parses ManifestFile inside capture directory dir.
func ReadManifest(dir string) (equal.Manifest, error) {
	f, err := os.Open(filepath.Join(dir, ManifestFile))
	if err != nil {
		return equal.Manifest{}, err
	}
	defer f.Close()
	return equal.ParseManifest(f)
}

// PageError reports a page image inside pages.bin that failed manifest verification.
type PageError struct {
	Page    int
	Problem string
}

// Error formats the offending capture page number and verification failure.
func (e *PageError) Error() string { return fmt.Sprintf("capture page %d: %s", e.Page, e.Problem) }

// Rebuild reconstructs a database file at outPath from a capture directory
// alone. It validates the manifest sequence (requiring at least one page),
// refuses stale sidecar files next to outPath, verifies every page's SHA-256
// while streaming into a temporary file, fsyncs, runs a full page census on
// the rebuilt image to confirm it agrees with the manifest, and atomically
// renames into place; on any failure no output or temp file is left behind.
func Rebuild(dir, outPath string) (equal.Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return m, err
	}
	if err := m.CheckSequence(); err != nil {
		return m, err
	}
	if m.PageCount() == 0 {
		return m, errors.New("the manifest lists no pages; a database has at least one")
	}
	if m.PageSize < dbfile.MinPageSize || m.PageSize > dbfile.MaxPageSize || m.PageSize&(m.PageSize-1) != 0 {
		return m, fmt.Errorf("manifest page size %d is not a valid page size", m.PageSize)
	}
	// Refuse to write beside an existing -wal, -journal, or -shm because SQLite
	// would replay or roll back that sidecar against the freshly rebuilt file.
	for _, suffix := range []string{"-wal", "-journal", "-shm"} {
		if _, err := os.Stat(outPath + suffix); err == nil {
			return m, fmt.Errorf("%s%s exists; SQLite would apply it to the rebuilt file", outPath, suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return m, err
		}
	}
	in, err := os.Open(filepath.Join(dir, PagesFile))
	if err != nil {
		return m, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return m, err
	}
	want := int64(m.PageCount()) * int64(m.PageSize)
	if info.Size() != want {
		return m, fmt.Errorf("%s holds %d bytes, the manifest lists %d pages of %d bytes (%d bytes)", PagesFile, info.Size(), m.PageCount(), m.PageSize, want)
	}
	tmp := outPath + ".rebuild-tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return m, err
	}
	fail := func(err error) (equal.Manifest, error) {
		out.Close()
		os.Remove(tmp)
		return m, err
	}
	reader := bufio.NewReader(in)
	page := make([]byte, m.PageSize)
	for _, e := range m.Entries {
		if _, err := io.ReadFull(reader, page); err != nil {
			return fail(err)
		}
		if equal.SHA256Hex(page) != e.SHA256 {
			return fail(&PageError{Page: e.Page, Problem: "image sha256 does not match the manifest"})
		}
		if _, err := out.Write(page); err != nil {
			return fail(err)
		}
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return m, err
	}
	if err := checkRebuilt(tmp, m); err != nil {
		os.Remove(tmp)
		return m, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return m, err
	}
	return m, nil
}

// checkRebuilt runs dbfile's page census on the rebuilt temp file and compares
// the resulting manifest against m, catching coordinated truncation (where both
// pages.bin and manifest.txt drop trailing pages) and tampered page roles.
func checkRebuilt(path string, m equal.Manifest) error {
	f, err := dbfile.Open(path)
	if err != nil {
		return fmt.Errorf("the rebuilt file is not a database the page census accepts: %w", err)
	}
	defer f.Close()
	if int64(f.PageCount())*int64(f.PageSize()) != f.FileSize() {
		return fmt.Errorf("the rebuilt file holds %d bytes but its header describes %d pages of %d bytes", f.FileSize(), f.PageCount(), f.PageSize())
	}
	got, err := BuildManifest(f)
	if err != nil {
		return fmt.Errorf("the rebuilt file is not a database the page census accepts: %w", err)
	}
	if diff := equal.CompareManifests(m, got); !diff.Equal {
		if diff.FirstDifferingPage == 0 {
			return fmt.Errorf("the rebuilt file's census disagrees with the manifest: %s", diff.Reason)
		}
		return &PageError{Page: diff.FirstDifferingPage, Problem: "the manifest disagrees with the rebuilt file's census: " + diff.Reason}
	}
	return nil
}

// VerifyImage hashes every page of a raw database image against manifest m and
// returns the 1-based page numbers whose SHA-256 differs. Unlike BuildManifest,
// it performs no B-tree or header parsing, so it can identify damaged pages in
// a structurally corrupt file without failing the census first.
func VerifyImage(m equal.Manifest, image []byte) ([]int, error) {
	if m.PageSize <= 0 {
		return nil, fmt.Errorf("manifest page size %d", m.PageSize)
	}
	if err := m.CheckSequence(); err != nil {
		return nil, err
	}
	if len(image) != m.PageCount()*m.PageSize {
		return nil, fmt.Errorf("image is %d bytes, the manifest lists %d pages of %d bytes", len(image), m.PageCount(), m.PageSize)
	}
	var differing []int
	for i, entry := range m.Entries {
		if equal.SHA256Hex(image[i*m.PageSize:(i+1)*m.PageSize]) != entry.SHA256 {
			differing = append(differing, entry.Page)
		}
	}
	return differing, nil
}
```

`capture.go` enforces the at-rest check, captures a closed database into `pages.bin` plus `manifest.txt`, and rebuilds a verified database from that directory alone:

- **`CheckAtRest`** refuses a non-empty `-wal` or a hot `-journal` (non-empty with a non-zero first header byte, mirroring `hasHotJournal` at [`src/pager.c:5180`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5180)).
- **`Pages`** removes any prior `manifest.txt` and `pages.bin` in `dir`, streams pages and manifest into `.capture-tmp` files (`writeCapture` and `writePages`, re-verifying every page hash while writing), re-checks at-rest status, size, and mtime, and renames the temporary files into place only after every check passes. On any failure, it deletes the temporary files so no partial or stale capture remains.
- **`Rebuild`** validates the manifest sequence, refuses a 0-page manifest or invalid page size, and rejects stale sidecar files beside `outPath` (propagating non-`ErrNotExist` stat errors). It then verifies `pages.bin` length and every page's SHA-256 while writing `outPath.rebuild-tmp`, calls `Sync`, runs `checkRebuilt` (which executes `dbfile`'s full page census on the temp file and compares its manifest against the input, catching coordinated truncation and tampered roles), and renames atomically into place. On any failure, `tmp` is removed and no output file is written.

End to end, a rung R1 capture and verified rebuild pass through these gates:

<!-- draw-visual: diagrams/ch02-file-format-and-pages-r1-capture-rebuild.mmd -->
```text
┌───────────────────────────────────────┐
│              .db at rest              │
└───────────────────┬───────────────────┘
                    ▼
┌───────────────────────────────────────┐
│CheckAtRest: empty -wal, no hot journal│
└───────────────────┬───────────────────┘
                    │
                   ok
                    ▼
┌───────────────────────────────────────┐
│  Write .capture-tmp + verify SHA-256  │
└───────────────────┬───────────────────┘
                    ▼
┌───────────────────────────────────────┐
│     Re-check at-rest, size, mtime     │
└───────────────────┬───────────────────┘
                    │
                   ok
                    ▼
┌───────────────────────────────────────┐
│   Rename -> pages.bin + manifest.txt  │
└───────────────────┬───────────────────┘
                    ▼
┌───────────────────────────────────────┐
│ Rebuild: check sequence + page SHA-256│
└───────────────────┬───────────────────┘
                    ▼
┌───────────────────────────────────────┐
│   Write .rebuild-tmp + Sync + census  │
└───────────────────┬───────────────────┘
                    │
                  match
                    ▼
┌───────────────────────────────────────┐
│        Atomic rename -> E1 .db        │
└───────────────────────────────────────┘
```

### 5.4 Experiment 2.2 — Page census (`2.2-page-census`)

- **Claim:** Every page in an SQLite database file has exactly one structural role, and a pure-Go reader can classify it from raw bytes alone: decoding `sqlite_schema` on page 1, walking every table and index B-tree (including `WITHOUT ROWID` tables, verified in unit and adversarial tests), following every overflow chain, walking multi-trunk freelists, and accounting for auto-vacuum pointer-map pages and the 1 GiB lock-byte page. The census matches `PRAGMA page_count`, `PRAGMA freelist_count`, `dbstat` (tree, page type, `ncell`, `payload`, `unused`, `mx_payload`), and SQLite's `SELECT` rows, and rejects malformed files with errors rather than panics.
- **Evidence form:** `EXACT` (every stored CSV table must match a fresh run byte for byte).
- **Scope:** The page census lets a platform account for every byte of a stored database file, and its page walk is reused by rungs R1, R3 (WAL replay), R4 (live snapshot), and R6 (point-in-time restore). Decoding every record type in every kind of table is not needed for page accounting and is out of scope.
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the rendered walkthrough; add `-i` to stay in the DuckDB shell afterwards with all result tables loaded):
  ```text
  make run EXP=2.2
  ```

<!-- include: experiments/2.2-page-census/main.go -->
```go
// Experiment 2.2 runs internal/dbfile's complete page census on the closed R0
// database, validates every B-tree and overflow page against SQLite's dbstat
// virtual table and every table's rows against SELECT, exercises the local
// payload boundary, multi-trunk freelists, auto-vacuum pointer-map pages, and
// the 1 GiB lock-byte page, and tests nine damaged files against integrity_check.
//
// Run from the repo root:
//
//	go run ./ch02-file-format-and-pages/experiments/2.2-page-census
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"

	_ "modernc.org/sqlite"
)

var (
	set       *results.Set
	files     *results.Table
	roles     *results.Table
	freelists *results.Table
	constants *results.Table
	special   *results.Table
)

func openTables(dir string) {
	set = results.MustOpen(dir)
	files = set.Table("files", "part", "scenario", "page_size", "usable_size", "page_count", "page_count_source", "pragma_page_count", "freelist_count", "pragma_freelist_count", "btree_pages_compared", "not_in_dbstat")
	roles = set.Table("roles", "scenario", "seq", "role", "pages")
	freelists = set.Table("freelist_pages", "scenario", "seq", "page", "kind", "leaves_listed")
	constants = set.Table("constants", "part", "name", "value", "note")
	special = set.Table("special_pages", "scenario", "role", "page")
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

func openSQLite(path string, readOnly bool) *sql.DB {
	query := url.Values{}
	if readOnly {
		query.Set("mode", "ro")
	}
	uri := must(sqlitex.FileURI(path, query))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	return db
}

func queryInt(ctx context.Context, db *sql.DB, query string) int {
	var v int
	if err := db.QueryRowContext(ctx, query).Scan(&v); err != nil {
		fatal(query, err)
	}
	return v
}

func closeAtRest(db *sql.DB, path string) {
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

type statRow struct {
	Tree    string
	Type    string
	Cells   int
	Payload int
	Unused  int
	MaxPay  int
}

func readDBStat(ctx context.Context, db *sql.DB) map[int]statRow {
	rows, err := db.QueryContext(ctx, "SELECT pageno, name, pagetype, ncell, payload, unused, mx_payload FROM dbstat")
	if err != nil {
		fatal("dbstat", err)
	}
	defer rows.Close()
	out := map[int]statRow{}
	for rows.Next() {
		var n int
		var r statRow
		if err := rows.Scan(&n, &r.Tree, &r.Type, &r.Cells, &r.Payload, &r.Unused, &r.MaxPay); err != nil {
			fatal("dbstat row", err)
		}
		out[n] = r
	}
	if err := rows.Err(); err != nil {
		fatal("dbstat rows", err)
	}
	return out
}

func dbstatType(r dbfile.Role) string {
	switch r {
	case dbfile.RoleTableInterior, dbfile.RoleIndexInterior:
		return "internal"
	case dbfile.RoleTableLeaf, dbfile.RoleIndexLeaf:
		return "leaf"
	case dbfile.RoleOverflow:
		return "overflow"
	}
	return ""
}

type comparison struct {
	compared   int
	mismatches []string
	notInStat  int
}

func compareWithDBStat(c *dbfile.Census, stat map[int]statRow, pages *results.Table) comparison {
	var result comparison
	for _, p := range c.Pages {
		kind := dbstatType(p.Role)
		theirs, listed := stat[p.Number]
		if pages != nil {
			if listed {
				pages.Row(p.Number, p.Role, p.Tree, kind, p.CellCount, p.Payload, p.Unused, p.MaxPayload, theirs.Tree, theirs.Type, theirs.Cells, theirs.Payload, theirs.Unused, theirs.MaxPay)
			} else {
				pages.Row(p.Number, p.Role, p.Tree, kind, p.CellCount, p.Payload, p.Unused, p.MaxPayload, nil, nil, nil, nil, nil, nil)
			}
		}
		if kind == "" {
			if listed {
				result.mismatches = append(result.mismatches, fmt.Sprintf("page %d (%s) is in dbstat", p.Number, p.Role))
			}
			result.notInStat++
			continue
		}
		ours := statRow{p.Tree, kind, p.CellCount, p.Payload, p.Unused, int(p.MaxPayload)}
		result.compared++
		switch {
		case !listed:
			result.mismatches = append(result.mismatches, fmt.Sprintf("page %d missing from dbstat", p.Number))
		case ours != theirs:
			result.mismatches = append(result.mismatches, fmt.Sprintf("page %d: ours %+v dbstat %+v", p.Number, ours, theirs))
		}
	}
	if result.compared != len(stat) {
		result.mismatches = append(result.mismatches, fmt.Sprintf("dbstat lists %d pages, the census has %d b-tree and overflow pages", len(stat), result.compared))
	}
	return result
}

// recordRoles writes how many pages of each role the census found and returns the total.
func recordRoles(scenario string, c *dbfile.Census) int {
	counts := c.RoleCounts()
	total := 0
	for i, r := range dbfile.AllRoles {
		total += counts[r]
		roles.Row(scenario, i+1, r, counts[r])
	}
	return total
}

// recordFreelist writes the freelist pages in the order the file lists them.
func recordFreelist(scenario string, c *dbfile.Census) {
	seq := 0
	for _, trunk := range c.FreelistTrunks {
		seq++
		freelists.Row(scenario, seq, trunk, "trunk", c.Page(trunk).CellCount)
	}
	for _, leaf := range c.FreelistLeaves {
		seq++
		freelists.Row(scenario, seq, leaf, "leaf", nil)
	}
}

// census loads the closed database file into memory and runs dbfile.Census so
// no os.File descriptor stays open when later checks open SQLite on the same path.
func census(path string) (*dbfile.File, *dbfile.Census) {
	image := must(os.ReadFile(path))("read " + filepath.Base(path))
	f := must(dbfile.FromBytes(image))("parse " + filepath.Base(path))
	c := must(f.Census())("census " + filepath.Base(path))
	return f, c
}

// treeNamed looks up a B-tree entry in c by its schema name.
func treeNamed(c *dbfile.Census, name string) dbfile.Tree {
	for _, t := range c.Trees {
		if t.Name == name {
			return t
		}
	}
	fatal("tree "+name, errors.New("not in the schema"))
	return dbfile.Tree{}
}

func canonicalWrite(h hash.Hash, class string, text string) {
	fmt.Fprintf(h, "%s:%d:%s|", class, len(text), text)
}

func ourValue(h hash.Hash, v dbfile.Value) {
	switch v.Class {
	case dbfile.Null:
		canonicalWrite(h, "null", "")
	case dbfile.Integer:
		canonicalWrite(h, "integer", fmt.Sprint(v.Int))
	case dbfile.Real:
		canonicalWrite(h, "real", fmt.Sprintf("%016x", math.Float64bits(v.Float)))
	case dbfile.Text:
		canonicalWrite(h, "text", hex.EncodeToString(v.Bytes))
	default:
		canonicalWrite(h, "blob", hex.EncodeToString(v.Bytes))
	}
}

func sqliteValue(h hash.Hash, class string, value any) {
	switch class {
	case "null":
		canonicalWrite(h, "null", "")
	case "integer":
		canonicalWrite(h, "integer", fmt.Sprint(value.(int64)))
	case "real":
		canonicalWrite(h, "real", fmt.Sprintf("%016x", math.Float64bits(value.(float64))))
	case "text":
		canonicalWrite(h, "text", hex.EncodeToString([]byte(value.(string))))
	default:
		canonicalWrite(h, "blob", hex.EncodeToString(value.([]byte)))
	}
}

type tableHash struct {
	rows int
	sum  string
}

// ourTableHash walks a table B-tree directly from raw bytes and hashes its
// canonical rows; column 0 NULL is replaced by the rowid because SQLite stores
// an INTEGER PRIMARY KEY column as NULL in the record and keeps its value in
// the cell's rowid varint.
func ourTableHash(f *dbfile.File, root int) tableHash {
	h := sha256.New()
	rows := 0
	err := f.WalkTable(root, func(r dbfile.Row) error {
		rows++
		fmt.Fprintf(h, "row %d|", r.Rowid)
		for i, v := range r.Values {
			if i == 0 && v.Class == dbfile.Null {
				v = dbfile.Value{Class: dbfile.Integer, Int: r.Rowid}
			}
			ourValue(h, v)
		}
		h.Write([]byte("\n"))
		return nil
	})
	if err != nil {
		fatal("walk table", err)
	}
	return tableHash{rows, hex.EncodeToString(h.Sum(nil))}
}

func sqliteTableHash(ctx context.Context, db *sql.DB, table string, columns []string) tableHash {
	var selects []string
	for _, c := range columns {
		selects = append(selects, fmt.Sprintf("typeof(%s), %s", c, c))
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT rowid, %s FROM %s ORDER BY rowid", strings.Join(selects, ", "), table))
	if err != nil {
		fatal("select "+table, err)
	}
	defer rows.Close()
	h := sha256.New()
	count := 0
	for rows.Next() {
		var rowid int64
		cells := make([]any, 2*len(columns))
		targets := []any{&rowid}
		for i := range cells {
			targets = append(targets, &cells[i])
		}
		if err := rows.Scan(targets...); err != nil {
			fatal("scan", err)
		}
		count++
		fmt.Fprintf(h, "row %d|", rowid)
		for i := range columns {
			sqliteValue(h, cells[2*i].(string), cells[2*i+1])
		}
		h.Write([]byte("\n"))
	}
	if err := rows.Err(); err != nil {
		fatal("rows", err)
	}
	return tableHash{count, hex.EncodeToString(h.Sum(nil))}
}

var recipeColumns = map[string][]string{
	"accounts":  {"id", "name", "balance"},
	"documents": {"id", "account_id", "title", "body"},
	"events":    {"id", "account_id", "kind", "amount", "note"},
}

func partA(ctx context.Context, dir string) {
	r := recipe.Default()
	path := filepath.Join(dir, "r0.db")
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build", err)
	}
	f, c := census(path)
	schema := set.Table("schema_entries", "entry", "type", "name", "tbl_name", "rootpage", "sql")
	for _, e := range c.Schema {
		schema.Row(e.Rowid, e.Type, e.Name, e.TableName, e.RootPage, e.SQL)
	}
	trees := set.Table("trees", "name", "type", "root_page", "btree_pages", "overflow_pages", "cells")
	for _, t := range c.SortedTrees() {
		trees.Row(t.Name, t.Type, t.RootPage, t.Pages, t.Overflow, t.Cells)
	}
	recordFreelist("r0", c)
	total := recordRoles("r0", c)
	set.Check("A", total == c.PageCount, fmt.Sprintf("the census sums to page_count %d", c.PageCount))
	set.Check("A", c.Complete() == nil, "every page has exactly one role: none counted twice (the census fails on a second claim), none missed")

	db := openSQLite(path, false)
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	stat := readDBStat(ctx, db)
	pages := set.Table("pages", "page", "role", "tree", "kind", "ncell", "payload", "unused", "mx_payload", "db_tree", "db_type", "db_ncell", "db_payload", "db_unused", "db_mx_payload")
	cmp := compareWithDBStat(c, stat, pages)
	files.Row("A", "r0", c.PageSize, c.UsableSize, c.PageCount, f.PageCountSource(), pageCount, c.FreelistLength(), freelist, cmp.compared, cmp.notInStat)
	set.Check("A", pageCount == c.PageCount, fmt.Sprintf("PRAGMA page_count = %d", pageCount))
	set.Check("A", freelist == c.FreelistLength(), fmt.Sprintf("freelist length %d = PRAGMA freelist_count %d", c.FreelistLength(), freelist))
	set.Check("A", len(cmp.mismatches) == 0, fmt.Sprintf("all %d b-tree and overflow pages match dbstat in tree, type, ncell, payload, unused and mx_payload; the %d other pages are not in dbstat", cmp.compared, cmp.notInStat))

	chainRows := set.Table("overflow_chains", "row_id", "body_bytes", "leaf_page", "overflow_pages", "chain_length")
	var overflowPages int
	err := f.WalkTable(treeNamed(c, "documents").RootPage, func(row dbfile.Row) error {
		if len(row.OverflowPages) == 0 {
			return nil
		}
		overflowPages += len(row.OverflowPages)
		chainRows.Row(row.Rowid, len(row.Values[3].Bytes), row.Page, strings.Trim(fmt.Sprint(row.OverflowPages), "[]"), len(row.OverflowPages))
		return nil
	})
	if err != nil {
		fatal("documents", err)
	}
	set.Check("A", overflowPages == c.RoleCounts()[dbfile.RoleOverflow], "the chains followed from the documents rows hold every overflow page in the file")

	hashes := set.Table("row_hashes", "table_name", "our_rows", "sqlite_rows", "our_sha256", "sqlite_sha256")
	for _, e := range c.Schema {
		columns, isRecipe := recipeColumns[e.Name]
		if !isRecipe {
			continue
		}
		ours := ourTableHash(f, e.RootPage)
		theirs := sqliteTableHash(ctx, db, e.Name, columns)
		hashes.Row(e.Name, ours.rows, theirs.rows, ours.sum, theirs.sum)
		set.Check("A", ours == theirs, fmt.Sprintf("table %s: same rows and values, read from the bytes and through SQLite", e.Name))
	}
	closeAtRest(db, path)
}

func newDB(ctx context.Context, path string, edit func(*sqlitex.Options), statements ...string) {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	if edit != nil {
		edit(&options)
	}
	db := must(sqlitex.Open(ctx, path, options))("open " + filepath.Base(path))
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func checkAgainstSQLite(ctx context.Context, path, part string, f *dbfile.File, c *dbfile.Census, label string) {
	db := openSQLite(path, false)
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	cmp := compareWithDBStat(c, readDBStat(ctx, db), nil)
	closeAtRest(db, path)
	files.Row(part, strings.TrimSuffix(filepath.Base(path), ".db"), c.PageSize, c.UsableSize, c.PageCount, f.PageCountSource(), pageCount, c.FreelistLength(), freelist, cmp.compared, cmp.notInStat)
	set.Check(part, pageCount == c.PageCount && freelist == c.FreelistLength() && c.Complete() == nil && len(cmp.mismatches) == 0,
		fmt.Sprintf("%s: census complete, page_count %d and freelist_count %d agree, %d pages match dbstat", label, pageCount, freelist, cmp.compared))
}

func partB(ctx context.Context, dir string) {
	maxLocal, minLocal := dbfile.MaxLocalPayload(4096, dbfile.TableLeaf), dbfile.MinLocalPayload(4096)
	constants.Row("B", "table leaf maxLocal", maxLocal, "usable-35, usable size 4096")
	constants.Row("B", "table leaf minLocal", minLocal, "(usable-12)*32/255-23")
	constants.Row("B", "index maxLocal", dbfile.MaxLocalPayload(4096, dbfile.IndexLeaf), "(usable-12)*64/255-23")
	path := filepath.Join(dir, "limit.db")
	newDB(ctx, path, nil,
		"CREATE TABLE b(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO b VALUES(1, zeroblob(4057))",
		"INSERT INTO b VALUES(2, zeroblob(4058))",
		"INSERT INTO b VALUES(3, zeroblob(8149))")
	f, c := census(path)
	// Record each row's parsed local/overflow split from WalkTable rather than
	// checking standalone arithmetic constants.
	type split struct {
		payload    uint64
		local      int
		overflow   int
		lastFilled int
	}
	splits := map[int64]split{}
	splitRows := set.Table("payload_splits", "row_id", "blob_bytes", "payload_bytes", "local_bytes", "overflow_pages", "last_overflow_bytes")
	err := f.WalkTable(2, func(row dbfile.Row) error {
		page := must(f.BTreePage(row.Page))("page")
		for i := range page.CellCount {
			cell := must(page.Cell(i))("cell")
			if cell.Rowid != row.Rowid {
				continue
			}
			chain := must(f.OverflowChain(row.Page, cell))("chain")
			s := split{payload: cell.PayloadSize, local: cell.LocalSize, overflow: len(chain)}
			if len(chain) > 0 {
				s.lastFilled = chain[len(chain)-1].Payload
			}
			splits[row.Rowid] = s
			splitRows.Row(row.Rowid, len(row.Values[1].Bytes), cell.PayloadSize, cell.LocalSize, len(chain), s.lastFilled)
		}
		return nil
	})
	if err != nil {
		fatal("walk", err)
	}
	set.Check("B", len(splits) == 3, "limit.db holds the three rows 1, 2 and 3")
	set.Check("B", splits[1] == split{4061, 4061, 0, 0}, "row 1: payload 4061 (= maxLocal) stays entirely on the leaf")
	set.Check("B", splits[2] == split{4062, minLocal, 1, 4062 - minLocal}, fmt.Sprintf("row 2: payload 4062 keeps only minLocal %d bytes on the leaf and spills %d to one overflow page", minLocal, 4062-minLocal))
	set.Check("B", splits[3] == split{8153, 4061, 1, 4092}, "row 3: payload 8153 keeps 4061 on the leaf so its overflow exactly fills one page (4092 bytes)")
	checkAgainstSQLite(ctx, path, "B", f, c, "local limit")
}

func partC(ctx context.Context, dir string) {
	path := filepath.Join(dir, "freelist.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 1024 },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 600) INSERT INTO big SELECT i, zeroblob(1000) FROM n ORDER BY i",
		"DELETE FROM big")
	f, c := census(path)
	constants.Row("C", "leaf numbers per trunk", c.UsableSize/4-2, "usable/4-2 at page size 1024")
	recordFreelist("freelist", c)
	recordRoles("freelist", c)
	set.Check("C", len(c.FreelistTrunks) >= 2, "the freelist spans more than one trunk page")
	checkAgainstSQLite(ctx, path, "C", f, c, "long freelist")
}

func partD(ctx context.Context, dir string) {
	path := filepath.Join(dir, "autovacuum.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 1024; o.File.AutoVacuum = sqlitex.AutoVacuumFull },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 400) INSERT INTO big SELECT i, zeroblob(900) FROM n ORDER BY i")
	f, c := census(path)
	var maps []int
	for _, p := range c.Pages {
		if p.Role == dbfile.RolePointerMap {
			maps = append(maps, p.Number)
		}
	}
	constants.Row("D", "header byte 52 (largest root page)", f.Header.LargestRootPage, "not 0, so auto-vacuum is on: "+f.Header.AutoVacuumName())
	constants.Row("D", "pointer-map spacing", c.UsableSize/5+1, "usable/5+1 pages, from page 2")
	for _, n := range maps {
		special.Row("autovacuum", dbfile.RolePointerMap, n)
	}
	recordRoles("autovacuum", c)
	set.Check("D", slices.Equal(maps, []int{2, 2 + c.UsableSize/5 + 1}), "pointer-map pages are pages 2 and 2 + usable/5 + 1 in this 408-page file")
	checkAgainstSQLite(ctx, path, "D", f, c, "auto_vacuum=FULL")
}

func partE(ctx context.Context, dir string) {
	// Construct a > 1 GiB sparse file at page size 65536 so the 1 GiB
	// lock-byte page (1073741824/65536 + 1 = 16385) lies inside the database
	// without allocating 1 GiB of disk blocks; page 3 lists all remaining pages
	// except 16385 as freelist leaves so SQLite's integrity_check passes.
	path := filepath.Join(dir, "large.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 65536; o.File.JournalMode = sqlitex.JournalDelete },
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')")
	const pageSize = 65536
	lock := dbfile.LockByteOffset/pageSize + 1
	pages := lock + 1
	image := must(os.ReadFile(path))("read")
	var leaves []int
	for n := 4; n <= pages; n++ {
		if n != lock {
			leaves = append(leaves, n)
		}
	}
	binary.BigEndian.PutUint32(image[28:], uint32(pages))
	binary.BigEndian.PutUint32(image[32:], 3)
	binary.BigEndian.PutUint32(image[36:], uint32(1+len(leaves)))
	trunk := make([]byte, pageSize)
	binary.BigEndian.PutUint32(trunk[4:], uint32(len(leaves)))
	for i, leaf := range leaves {
		binary.BigEndian.PutUint32(trunk[8+4*i:], uint32(leaf))
	}
	out := must(os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0))("open for edit")
	_, err := out.Write(image[:2*pageSize])
	if err == nil {
		_, err = out.WriteAt(trunk, 2*pageSize)
	}
	if err == nil {
		err = out.Truncate(int64(pages) * pageSize)
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		fatal("write sparse file", err)
	}
	constants.Row("E", "in-header size (pages)", pages, "page 3 is a freelist trunk listing pages 4.."+fmt.Sprint(pages)+" except the lock-byte page")
	constants.Row("E", "file size (bytes)", int64(pages)*pageSize, "1 GiB is 1073741824; everything after page 3 is a hole")
	constants.Row("E", "lock-byte page", lock, "1073741824/65536 + 1")
	fmt.Printf("INFO: blocks on disk: %s\n", diskUsage(path))
	// A > 1 GiB sparse file is too large to load with os.ReadFile, so open it
	// directly with dbfile.Open and close the descriptor before SQLite opens it.
	f := must(dbfile.Open(path))("parse " + filepath.Base(path))
	c, err := f.Census()
	source := f.PageCountSource()
	if closeErr := f.Close(); closeErr != nil {
		fatal("close parsed file", closeErr)
	}
	if err != nil {
		fatal("census "+filepath.Base(path), err)
	}
	recordRoles("large", c)
	special.Row("large", dbfile.RoleLockByte, lock)
	set.Check("E", c.Page(lock).Role == dbfile.RoleLockByte && c.Complete() == nil, fmt.Sprintf("page %d is counted as the lock-byte page, not as lost", lock))
	db := openSQLite(path, true)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	closeAtRest(db, path)
	files.Row("E", "large", c.PageSize, c.UsableSize, c.PageCount, source, pageCount, c.FreelistLength(), freelist, nil, nil)
	constants.Row("E", "SQLite integrity_check", integrity, "on the edited file, read-only")
	set.Check("E", integrity == "ok" && pageCount == c.PageCount && freelist == c.FreelistLength(), "SQLite accepts the file: integrity_check ok, and page_count and freelist_count equal the census")
	if err := os.Remove(path); err != nil {
		fatal("remove large file", err)
	}
}

type corruption struct {
	name string
	edit func(image []byte)
}

func partF(ctx context.Context, dir string) {
	// Each case damages one structure in a 6-page rollback-mode copy; rollback
	// mode and mode=ro keep SQLite from altering the damaged file on open.
	base := filepath.Join(dir, "reject-base.db")
	newDB(ctx, base, func(o *sqlitex.Options) { o.File.JournalMode = sqlitex.JournalDelete },
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO t VALUES(1, zeroblob(10000))",
		"INSERT INTO t VALUES(2, 'two')",
		"INSERT INTO t VALUES(3, zeroblob(10000))",
		"DELETE FROM t WHERE id = 3")
	good := must(os.ReadFile(base))("read")
	_, c := census(base)
	basePages := set.Table("reject_base_pages", "page", "role")
	for _, p := range c.Pages {
		basePages.Row(p.Number, p.Role)
	}
	page2 := 4096
	cellPointer := page2 + 8
	firstCell := int(binary.BigEndian.Uint16(good[cellPointer:]))
	overflowOfRow1 := 0
	for _, p := range c.Pages {
		if p.Role == dbfile.RoleOverflow && overflowOfRow1 == 0 {
			overflowOfRow1 = p.Number
		}
	}
	cases := []corruption{
		{"magic: byte 0 changed", func(b []byte) { b[0] = 'X' }},
		{"truncated: last page (a freelist leaf) cut in half", nil},
		{"page 2 type byte 0x0d -> 0x07", func(b []byte) { b[page2] = 0x07 }},
		{"page 2 cell count 2 -> 3000", func(b []byte) { binary.BigEndian.PutUint16(b[page2+3:], 3000) }},
		{"page 2 first cell pointer -> 9", func(b []byte) { binary.BigEndian.PutUint16(b[cellPointer:], 9) }},
		{"row 1 payload size varint -> 0x7f", func(b []byte) { b[page2+firstCell] = 0x7f }},
		{fmt.Sprintf("overflow page %d points back to itself", overflowOfRow1), func(b []byte) {
			binary.BigEndian.PutUint32(b[(overflowOfRow1-1)*4096:], uint32(overflowOfRow1))
		}},
		{"freelist trunk (header 32) -> page 2, a table page", func(b []byte) { binary.BigEndian.PutUint32(b[32:], 2) }},
		{"freelist count (header 36) + 1", func(b []byte) { b[39]++ }},
	}
	rejections := set.Table("rejections", "case_no", "damage", "ours", "sqlite")
	for i, k := range cases {
		image := append([]byte(nil), good...)
		if k.edit == nil {
			image = image[:len(image)-2048]
		} else {
			k.edit(image)
		}
		path := filepath.Join(dir, fmt.Sprintf("reject-%d.db", i+1))
		if err := os.WriteFile(path, image, 0o644); err != nil {
			fatal("write", err)
		}
		ours := parseAll(image)
		theirs := sqliteVerdict(ctx, path)
		rejections.Row(fmt.Sprintf("F%d", i+1), k.name, ours, theirs)
		set.Check("F", ours != "accepted" && !strings.HasPrefix(ours, "PANIC"), fmt.Sprintf("F%d: our parser returns an error, without panicking", i+1))
		if k.edit == nil {
			set.Check("F", strings.HasSuffix(theirs, ": ok"), "F2: SQLite rounds a partial last page up and reads the missing bytes as zeros, so it reports ok; our parser is stricter and refuses the file")
		}
	}
}

func parseAll(image []byte) (verdict string) {
	defer func() {
		if r := recover(); r != nil {
			verdict = fmt.Sprintf("PANIC %v", r)
		}
	}()
	f, err := dbfile.FromBytes(image)
	if err != nil {
		return "error: " + err.Error()
	}
	c, err := f.Census()
	if err == nil {
		err = c.Complete()
	}
	if err == nil {
		for _, e := range c.Schema {
			if e.Type == "table" && e.RootPage > 0 {
				if err = f.WalkTable(e.RootPage, func(dbfile.Row) error { return nil }); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return "accepted"
}

func sqliteVerdict(ctx context.Context, path string) string {
	db := openSQLite(path, true)
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "error: " + err.Error()
		}
		lines = append(lines, strings.Split(line, "\n")...)
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	if len(lines) > 4 {
		lines = append(lines[:4], fmt.Sprintf("(+%d more lines)", len(lines)-4))
	}
	return "integrity_check: " + strings.Join(lines, " / ")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("2.2")
	openTables(dir)
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	partF(ctx, dir)
	set.Finish("CENSUS")
}

var errNoUsage = errors.New("disk usage unknown on this system")

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

Build-tag helpers (`//go:build unix` for Linux and macOS, `//go:build !unix` elsewhere) report allocated disk blocks for part E's sparse file on an `INFO:` line of the experiment's own output (not part of the walkthrough or the stored evidence):

<!-- include: experiments/2.2-page-census/usage_unix.go -->
```go
//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// diskUsage reports the actual disk blocks allocated for path on Unix systems
// (blocks are counted in 512-byte units in syscall.Stat_t), demonstrating that
// the > 1 GiB test file in part E is sparse.
func diskUsage(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errNoUsage.Error()
	}
	return fmt.Sprintf("%d bytes allocated for a %d-byte file", stat.Blocks*512, info.Size())
}
```

<!-- include: experiments/2.2-page-census/usage_other.go -->
```go
//go:build !unix

package main

// diskUsage reports that sparse-file block allocation is unavailable on non-Unix systems.
func diskUsage(path string) string { return errNoUsage.Error() }
```

The experiment prints only its verdict. It writes every measurement to CSV tables, and this DuckDB script presents them as a walkthrough:

<!-- include: experiments/2.2-page-census/report.sql -->
```sql
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
```

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/2.2-page-census/`](evidence/2.2-page-census/), and `make check ARGS='evidence ch02'` compares a fresh run with them byte for byte.

<!-- include: evidence/2.2-page-census.txt -->
```text

════════════════════════════════════════════════════════════════════════
  2.2  Can a parser account for every page of a database file?
════════════════════════════════════════════════════════════════════════

internal/dbfile reads a closed database file and gives every page one
role: b-tree interior or leaf, overflow, freelist trunk or leaf, pointer
map, or the lock-byte page. The experiment checks the census against
SQLite (PRAGMA page_count, freelist_count, the dbstat virtual table, a
SELECT of every table), builds files that stress the corners, and tries
to break the parser with nine damaged files.

── A. Census of the R0 database (recipe default, WAL, 30 steps, closed) ──

A1. The file as the census and SQLite see it. The page count comes from the
in-header size; both page counts and both freelist lengths must agree.
┌───────────┬─────────────┬────────────┬───────────────────┬──────────────┬────────────┬─────────────┐
│ page_size │ usable_size │ page_count │ page_count_source │ sqlite_pages │ free_pages │ sqlite_free │
├───────────┼─────────────┼────────────┼───────────────────┼──────────────┼────────────┼─────────────┤
│ 4096      │ 4096        │ 270        │ in-header size    │ 270          │ 3          │ 3           │
└───────────┴─────────────┴────────────┴───────────────────┴──────────────┴────────────┴─────────────┘

A2. The schema table on page 1, decoded by our record reader. The sql column
is cut into lines of at most 50 characters.
┌───────┬───────┬───────────────────┬───────────┬──────────┬────────────────────────────────────────────────────┐
│ entry │ type  │       name        │ tbl_name  │ rootpage │                        sql                         │
├───────┼───────┼───────────────────┼───────────┼──────────┼────────────────────────────────────────────────────┤
│ 1     │ table │ accounts          │ accounts  │ 2        │ CREATE TABLE accounts(id INTEGER PRIMARY KEY, name │
│       │       │                   │           │          │  TEXT NOT NULL, balance INTEGER NOT NULL)          │
│ 2     │ table │ documents         │ documents │ 3        │ CREATE TABLE documents(id INTEGER PRIMARY KEY, acc │
│       │       │                   │           │          │ ount_id INTEGER NOT NULL, title TEXT NOT NULL, bod │
│       │       │                   │           │          │ y BLOB NOT NULL)                                   │
│ 3     │ table │ events            │ events    │ 4        │ CREATE TABLE events(id INTEGER PRIMARY KEY, accoun │
│       │       │                   │           │          │ t_id INTEGER NOT NULL, kind TEXT NOT NULL, amount  │
│       │       │                   │           │          │ INTEGER NOT NULL, note TEXT NOT NULL)              │
│ 4     │ index │ events_by_account │ events    │ 28       │ CREATE INDEX events_by_account ON events(account_i │
│       │       │                   │           │          │ d, id)                                             │
└───────┴───────┴───────────────────┴───────────┴──────────┴────────────────────────────────────────────────────┘

A3. The b-trees walked from those roots.
┌───────────────────┬───────┬───────────┬─────────────┬────────────────┬───────┐
│       name        │ type  │ root_page │ btree_pages │ overflow_pages │ cells │
├───────────────────┼───────┼───────────┼─────────────┼────────────────┼───────┤
│ accounts          │ table │ 2         │ 3           │ 0              │ 184   │
│ documents         │ table │ 3         │ 70          │ 170            │ 164   │
│ events            │ table │ 4         │ 18          │ 0              │ 1193  │
│ events_by_account │ index │ 28        │ 5           │ 0              │ 1177  │
│ sqlite_schema     │ table │ 1         │ 1           │ 0              │ 4     │
└───────────────────┴───────┴───────────┴─────────────┴────────────────┴───────┘

A4. The freelist from header offset 32: the trunk and the leaves it lists.
┌────────┬─────────┐
│ trunks │ leaves  │
├────────┼─────────┤
│ 230    │ 231 232 │
└────────┴─────────┘

A5. Pages by role. Every page has exactly one role, so the roles sum to the
page count (270) and none is unaccounted for.
┌────────────────┬───────┐
│      role      │ pages │
├────────────────┼───────┤
│ table-interior │ 3     │
│ table-leaf     │ 89    │
│ index-interior │ 1     │
│ index-leaf     │ 4     │
│ overflow       │ 170   │
│ freelist-trunk │ 1     │
│ freelist-leaf  │ 2     │
│ pointer-map    │ 0     │
│ lock-byte      │ 0     │
│ unaccounted    │ 0     │
│ total          │ 270   │
└────────────────┴───────┘

A6. The page-role census against dbstat, page by page. Every b-tree and
overflow page must match on tree, type, cell count, payload, unused bytes and
largest payload. The other pages are not b-tree pages, so dbstat does not list them.
┌──────┬────────────────┬───────────────────┬───────┬─────────┬────────┬────────┬───────────────────────────────────┐
│ page │      role      │       tree        │ ncell │ payload │ unused │ mx_pay │              dbstat               │
├──────┼────────────────┼───────────────────┼───────┼─────────┼────────┼────────┼───────────────────────────────────┤
│ 1    │ table-leaf     │ sqlite_schema     │ 4     │ 520     │ 3450   │ 162    │ match                             │
│ 2    │ table-interior │ accounts          │ 1     │ 0       │ 4076   │ 0      │ match                             │
│ 3    │ table-interior │ documents         │ 68    │ 0       │ 3608   │ 0      │ match                             │
│ 4    │ table-interior │ events            │ 16    │ 0       │ 3957   │ 0      │ match                             │
│ 5    │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 6    │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 7    │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 8    │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 9    │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 10   │ table-leaf     │ documents         │ 1     │ 3625    │ 454    │ 15901  │ match                             │
│ 11   │ table-leaf     │ documents         │ 1     │ 2193    │ 1890   │ 2193   │ match                             │
│ 12   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 13   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 14   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 15   │ table-leaf     │ documents         │ 1     │ 3114    │ 965    │ 15390  │ match                             │
│ 16   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 17   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 18   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 19   │ table-leaf     │ documents         │ 1     │ 2568    │ 1511   │ 14844  │ match                             │
│ 20   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 21   │ table-leaf     │ documents         │ 1     │ 2885    │ 1194   │ 15161  │ match                             │
│ 22   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 23   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 24   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 25   │ table-leaf     │ documents         │ 1     │ 3535    │ 548    │ 3535   │ match                             │
│ 26   │ table-leaf     │ events            │ 63    │ 3143    │ 693    │ 78     │ match                             │
│ 27   │ table-leaf     │ events            │ 64    │ 3406    │ 408    │ 77     │ match                             │
│ 28   │ index-interior │ events_by_account │ 3     │ 25      │ 4038   │ 9      │ match                             │
│ 29   │ overflow       │ documents         │ 0     │ 3620    │ 472    │ 0      │ match                             │
│ 30   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 31   │ table-leaf     │ documents         │ 2     │ 2253    │ 1817   │ 13840  │ match                             │
│ 32   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 33   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 34   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 35   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 36   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 37   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 38   │ table-leaf     │ documents         │ 1     │ 1967    │ 2112   │ 6059   │ match                             │
│ 39   │ table-leaf     │ events            │ 65    │ 3133    │ 630    │ 77     │ match                             │
│ 40   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 41   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 42   │ table-leaf     │ documents         │ 2     │ 3364    │ 706    │ 10354  │ match                             │
│ 43   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 44   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 45   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 46   │ table-leaf     │ documents         │ 1     │ 3391    │ 688    │ 7483   │ match                             │
│ 47   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 48   │ table-leaf     │ documents         │ 3     │ 3642    │ 419    │ 13900  │ match                             │
│ 49   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 50   │ table-leaf     │ documents         │ 1     │ 690     │ 3389   │ 4782   │ match                             │
│ 51   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 52   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 53   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 54   │ table-leaf     │ documents         │ 1     │ 3363    │ 716    │ 15639  │ match                             │
│ 55   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 56   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 57   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 58   │ table-leaf     │ documents         │ 1     │ 3766    │ 317    │ 3766   │ match                             │
│ 59   │ table-leaf     │ events            │ 67    │ 3297    │ 456    │ 76     │ match                             │
│ 60   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 61   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 62   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 63   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 64   │ table-leaf     │ documents         │ 1     │ 3972    │ 111    │ 3972   │ match                             │
│ 65   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 66   │ table-leaf     │ events            │ 69    │ 3357    │ 386    │ 77     │ match                             │
│ 67   │ overflow       │ documents         │ 0     │ 3644    │ 448    │ 0      │ match                             │
│ 68   │ table-leaf     │ documents         │ 1     │ 1772    │ 2307   │ 14048  │ match                             │
│ 69   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 70   │ table-leaf     │ documents         │ 1     │ 2938    │ 1141   │ 11122  │ match                             │
│ 71   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 72   │ table-leaf     │ documents         │ 1     │ 3262    │ 817    │ 7354   │ match                             │
│ 73   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 74   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 75   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 76   │ table-leaf     │ documents         │ 1     │ 2407    │ 1676   │ 2407   │ match                             │
│ 77   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 78   │ table-leaf     │ documents         │ 1     │ 2751    │ 1332   │ 2751   │ match                             │
│ 79   │ table-leaf     │ documents         │ 1     │ 2444    │ 1639   │ 2444   │ match                             │
│ 80   │ table-leaf     │ documents         │ 1     │ 3749    │ 330    │ 11933  │ match                             │
│ 81   │ table-leaf     │ documents         │ 1     │ 3565    │ 514    │ 7657   │ match                             │
│ 82   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 83   │ table-leaf     │ documents         │ 1     │ 1106    │ 2973   │ 9290   │ match                             │
│ 84   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 85   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 86   │ table-leaf     │ documents         │ 1     │ 3800    │ 279    │ 11984  │ match                             │
│ 87   │ table-leaf     │ events            │ 65    │ 3395    │ 368    │ 78     │ match                             │
│ 88   │ index-leaf     │ events_by_account │ 331   │ 2750    │ 345    │ 9      │ match                             │
│ 89   │ index-leaf     │ events_by_account │ 314   │ 2822    │ 324    │ 9      │ match                             │
│ 90   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 91   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 92   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 93   │ table-leaf     │ documents         │ 2     │ 3939    │ 131    │ 13932  │ match                             │
│ 94   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 95   │ table-leaf     │ documents         │ 1     │ 2062    │ 2021   │ 2062   │ match                             │
│ 96   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 97   │ table-leaf     │ documents         │ 1     │ 3174    │ 905    │ 7266   │ match                             │
│ 98   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 99   │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 100  │ table-leaf     │ documents         │ 1     │ 3034    │ 1045   │ 11218  │ match                             │
│ 101  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 102  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 103  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 104  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 105  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 106  │ table-leaf     │ documents         │ 3     │ 3602    │ 459    │ 13160  │ match                             │
│ 107  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 108  │ table-leaf     │ events            │ 69    │ 3519    │ 224    │ 78     │ match                             │
│ 109  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 110  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 111  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 112  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 113  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 114  │ table-leaf     │ documents         │ 1     │ 3817    │ 262    │ 12001  │ match                             │
│ 115  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 116  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 117  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 118  │ table-leaf     │ documents         │ 2     │ 2400    │ 1670   │ 14187  │ match                             │
│ 119  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 120  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 121  │ overflow       │ documents         │ 0     │ 3832    │ 260    │ 0      │ match                             │
│ 122  │ table-leaf     │ documents         │ 1     │ 3689    │ 394    │ 3689   │ match                             │
│ 123  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 124  │ table-leaf     │ events            │ 71    │ 3641    │ 92     │ 77     │ match                             │
│ 125  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 126  │ table-leaf     │ documents         │ 1     │ 2116    │ 1963   │ 10300  │ match                             │
│ 127  │ table-leaf     │ documents         │ 1     │ 2641    │ 1438   │ 14917  │ match                             │
│ 128  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 129  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 130  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 131  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 132  │ table-leaf     │ events            │ 76    │ 3525    │ 183    │ 78     │ match                             │
│ 133  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 134  │ table-leaf     │ documents         │ 1     │ 3594    │ 485    │ 7686   │ match                             │
│ 135  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 136  │ table-leaf     │ documents         │ 1     │ 3419    │ 660    │ 7511   │ match                             │
│ 137  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 138  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 139  │ table-leaf     │ documents         │ 2     │ 1653    │ 2417   │ 9348   │ match                             │
│ 140  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 141  │ overflow       │ documents         │ 0     │ 3975    │ 117    │ 0      │ match                             │
│ 142  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 143  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 144  │ table-leaf     │ documents         │ 1     │ 2772    │ 1307   │ 10956  │ match                             │
│ 145  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 146  │ table-leaf     │ documents         │ 1     │ 3556    │ 523    │ 7648   │ match                             │
│ 147  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 148  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 149  │ table-leaf     │ documents         │ 1     │ 2071    │ 2008   │ 10255  │ match                             │
│ 150  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 151  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 152  │ table-leaf     │ documents         │ 1     │ 3129    │ 950    │ 11313  │ match                             │
│ 153  │ table-leaf     │ documents         │ 1     │ 3631    │ 452    │ 3631   │ match                             │
│ 154  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 155  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 156  │ table-leaf     │ documents         │ 2     │ 3846    │ 224    │ 10306  │ match                             │
│ 157  │ table-leaf     │ events            │ 72    │ 3633    │ 95     │ 77     │ match                             │
│ 158  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 159  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 160  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 161  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 162  │ table-leaf     │ documents         │ 2     │ 3974    │ 96     │ 10896  │ match                             │
│ 163  │ table-leaf     │ documents         │ 2     │ 3789    │ 281    │ 14051  │ match                             │
│ 164  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 165  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 166  │ table-leaf     │ documents         │ 1     │ 1193    │ 2886   │ 5285   │ match                             │
│ 167  │ index-leaf     │ events_by_account │ 279   │ 2511    │ 740    │ 9      │ match                             │
│ 168  │ table-leaf     │ events            │ 67    │ 3611    │ 142    │ 76     │ match                             │
│ 169  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 170  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 171  │ table-leaf     │ documents         │ 1     │ 2866    │ 1217   │ 2866   │ match                             │
│ 172  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 173  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 174  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 175  │ table-leaf     │ documents         │ 3     │ 3197    │ 864    │ 14000  │ match                             │
│ 176  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 177  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 178  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 179  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 180  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 181  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 182  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 183  │ table-leaf     │ documents         │ 3     │ 4041    │ 20     │ 13926  │ match                             │
│ 184  │ table-leaf     │ events            │ 70    │ 3658    │ 80     │ 78     │ match                             │
│ 185  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 186  │ table-leaf     │ documents         │ 1     │ 2944    │ 1139   │ 2944   │ match                             │
│ 187  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 188  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 189  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 190  │ table-leaf     │ documents         │ 2     │ 2981    │ 1089   │ 10352  │ match                             │
│ 191  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 192  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 193  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 194  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 195  │ table-leaf     │ documents         │ 1     │ 2815    │ 1264   │ 15091  │ match                             │
│ 196  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 197  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 198  │ overflow       │ documents         │ 0     │ 3700    │ 392    │ 0      │ match                             │
│ 199  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 200  │ table-leaf     │ documents         │ 1     │ 3095    │ 984    │ 15371  │ match                             │
│ 201  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 202  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 203  │ table-leaf     │ documents         │ 1     │ 2161    │ 1918   │ 10345  │ match                             │
│ 204  │ table-leaf     │ events            │ 70    │ 3562    │ 176    │ 77     │ match                             │
│ 205  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 206  │ table-leaf     │ documents         │ 1     │ 2144    │ 1935   │ 10328  │ match                             │
│ 207  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 208  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 209  │ table-leaf     │ documents         │ 2     │ 3452    │ 618    │ 11147  │ match                             │
│ 210  │ table-leaf     │ events            │ 71    │ 3573    │ 160    │ 78     │ match                             │
│ 211  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 212  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 213  │ table-leaf     │ documents         │ 2     │ 3079    │ 991    │ 14866  │ match                             │
│ 214  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 215  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 216  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 217  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 218  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 219  │ table-leaf     │ events            │ 73    │ 3723    │ 0      │ 78     │ match                             │
│ 220  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 221  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 222  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 223  │ table-leaf     │ documents         │ 2     │ 3831    │ 239    │ 14706  │ match                             │
│ 224  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 225  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 226  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 227  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 228  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 229  │ table-leaf     │ documents         │ 2     │ 3962    │ 108    │ 9858   │ match                             │
│ 230  │ freelist-trunk │                   │ 2     │ 0       │ 0      │ 0      │ not a b-tree page (not in dbstat) │
│ 231  │ freelist-leaf  │                   │ 0     │ 0       │ 0      │ 0      │ not a b-tree page (not in dbstat) │
│ 232  │ freelist-leaf  │                   │ 0     │ 0       │ 0      │ 0      │ not a b-tree page (not in dbstat) │
│ 233  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 234  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 235  │ table-leaf     │ documents         │ 1     │ 4025    │ 54     │ 12209  │ match                             │
│ 236  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 237  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 238  │ table-leaf     │ documents         │ 4     │ 3321    │ 731    │ 9460   │ match                             │
│ 239  │ overflow       │ documents         │ 0     │ 3584    │ 508    │ 0      │ match                             │
│ 240  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 241  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 242  │ index-leaf     │ events_by_account │ 250   │ 2310    │ 1028   │ 10     │ match                             │
│ 243  │ table-leaf     │ events            │ 72    │ 3693    │ 35     │ 78     │ match                             │
│ 244  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 245  │ table-leaf     │ documents         │ 1     │ 2677    │ 1406   │ 2677   │ match                             │
│ 246  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 247  │ table-leaf     │ documents         │ 1     │ 3562    │ 517    │ 7654   │ match                             │
│ 248  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 249  │ table-leaf     │ documents         │ 2     │ 2711    │ 1359   │ 10291  │ match                             │
│ 250  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 251  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 252  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 253  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 254  │ table-leaf     │ documents         │ 2     │ 3431    │ 639    │ 15136  │ match                             │
│ 255  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 256  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 257  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 258  │ table-leaf     │ documents         │ 1     │ 3452    │ 627    │ 11636  │ match                             │
│ 259  │ table-leaf     │ events            │ 73    │ 3666    │ 57     │ 77     │ match                             │
│ 260  │ table-leaf     │ accounts          │ 177   │ 3306    │ 24     │ 25     │ match                             │
│ 261  │ table-leaf     │ accounts          │ 6     │ 118     │ 3940   │ 24     │ match                             │
│ 262  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 263  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 264  │ table-leaf     │ documents         │ 2     │ 2880    │ 1190   │ 12723  │ match                             │
│ 265  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 266  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 267  │ overflow       │ documents         │ 0     │ 4050    │ 42     │ 0      │ match                             │
│ 268  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
│ 269  │ table-leaf     │ documents         │ 1     │ 2181    │ 1898   │ 6273   │ match                             │
│ 270  │ overflow       │ documents         │ 0     │ 4092    │ 0      │ 0      │ match                             │
└──────┴────────────────┴───────────────────┴───────┴─────────┴────────┴────────┴───────────────────────────────────┘

A7. The verdicts in total.
┌───────────────────────────────────┬───────┐
│              dbstat               │ pages │
├───────────────────────────────────┼───────┤
│ match                             │ 267   │
│ not a b-tree page (not in dbstat) │ 3     │
└───────────────────────────────────┴───────┘

A8. Overflow chains of the documents table: the first five rows that overflow,
then the totals over all of them.
┌────────┬────────────┬───────────┬────────────────┐
│ row_id │ body_bytes │ leaf_page │ overflow_chain │
├────────┼────────────┼───────────┼────────────────┤
│ 1      │ 15875      │ 10        │ 5 6 7          │
│ 3      │ 15347      │ 15        │ 12 13 14       │
│ 4      │ 14821      │ 19        │ 16 17 18       │
│ 6      │ 15129      │ 21        │ 9 8 23         │
│ 8      │ 13816      │ 31        │ 22 20 30       │
└────────┴────────────┴───────────┴────────────────┘
┌────────────────────┬───────────────────────┬───────────────┐
│ rows_that_overflow │ overflow_pages_in_all │ longest_chain │
├────────────────────┼───────────────────────┼───────────────┤
│ 83                 │ 170                   │ 3             │
└────────────────────┴───────────────────────┴───────────────┘

A9. Every row of every recipe table, hashed from the page bytes by our parser
and read through SELECT ... ORDER BY rowid with every value tagged by typeof().
The first 16 hex digits of each sha256 are shown.
┌────────────┬──────────┬─────────────┬──────────────────┬──────────────────┬──────┐
│ table_name │ our_rows │ sqlite_rows │    our_sha256    │  sqlite_sha256   │ same │
├────────────┼──────────┼─────────────┼──────────────────┼──────────────────┼──────┤
│ accounts   │ 183      │ 183         │ 8017137363561914 │ 8017137363561914 │ true │
│ documents  │ 96       │ 96          │ 973d4aa8fa24c3aa │ 973d4aa8fa24c3aa │ true │
│ events     │ 1177     │ 1177        │ afdcccdab84ea9c5 │ afdcccdab84ea9c5 │ true │
└────────────┴──────────┴─────────────┴──────────────────┴──────────────────┴──────┘

── B. A payload exactly at the local limit, and one byte over ───────────

A table leaf at usable size 4096 keeps a whole payload when it is at most
maxLocal. A larger one keeps only minLocal bytes (or maxLocal, when the
overflow would then fill its last page exactly) and spills the rest.
┌─────────────────────┬───────┬─────────────────────────────┐
│        name         │ value │            note             │
├─────────────────────┼───────┼─────────────────────────────┤
│ index maxLocal      │ 1002  │ (usable-12)*64/255-23       │
│ table leaf maxLocal │ 4061  │ usable-35, usable size 4096 │
│ table leaf minLocal │ 489   │ (usable-12)*32/255-23       │
└─────────────────────┴───────┴─────────────────────────────┘

B1. Three blobs. The payload is the blob plus a 4-byte record header.
Row 1 is exactly maxLocal and stays on the leaf; row 2 is one byte over.
┌────────┬────────────┬───────────────┬─────────────┬────────────────┬─────────────────────┐
│ row_id │ blob_bytes │ payload_bytes │ local_bytes │ overflow_pages │ last_overflow_bytes │
├────────┼────────────┼───────────────┼─────────────┼────────────────┼─────────────────────┤
│ 1      │ 4057       │ 4061          │ 4061        │ 0              │ 0                   │
│ 2      │ 4058       │ 4062          │ 489         │ 1              │ 3573                │
│ 3      │ 8149       │ 8153          │ 4061        │ 1              │ 4092                │
└────────┴────────────┴───────────────┴─────────────┴────────────────┴─────────────────────┘

B2. The census of that file against SQLite.
┌──────────┬────────────┬──────────────┬────────────┬─────────────┬──────────────────────┬───────────────┐
│ scenario │ page_count │ sqlite_pages │ free_pages │ sqlite_free │ btree_pages_compared │ not_in_dbstat │
├──────────┼────────────┼──────────────┼────────────┼─────────────┼──────────────────────┼───────────────┤
│ limit    │ 7          │ 7            │ 0          │ 0           │ 7                    │ 0             │
└──────────┴────────────┴──────────────┴────────────┴─────────────┴──────────────────────┴───────────────┘

── C. A freelist longer than one trunk page ────────────────────────────

Page size 1024, 600 rows of 1000 bytes inserted and then all deleted.
┌────────────────────────┬───────┬──────────────────────────────┐
│          name          │ value │             note             │
├────────────────────────┼───────┼──────────────────────────────┤
│ leaf numbers per trunk │ 254   │ usable/4-2 at page size 1024 │
└────────────────────────┴───────┴──────────────────────────────┘

C1. The trunks, in the order the file chains them, with how many leaves each lists.
┌─────┬────────────┬───────────────┐
│ seq │ trunk_page │ leaves_listed │
├─────┼────────────┼───────────────┤
│ 1   │ 502        │ 175           │
│ 2   │ 252        │ 248           │
│ 3   │ 3          │ 248           │
└─────┴────────────┴───────────────┘

C2. Trunks plus leaves are the free pages, and they agree with SQLite.
┌────────┬────────┬────────────┬──────────┬───────────────────────┐
│ trunks │ leaves │ free_pages │ of_pages │ pragma_freelist_count │
├────────┼────────┼────────────┼──────────┼───────────────────────┤
│ 3      │ 671    │ 674        │ 676      │ 674                   │
└────────┴────────┴────────────┴──────────┴───────────────────────┘

C3. Pages by role.
┌────────────────┬───────┐
│      role      │ pages │
├────────────────┼───────┤
│ table-interior │ 0     │
│ table-leaf     │ 2     │
│ index-interior │ 0     │
│ index-leaf     │ 0     │
│ overflow       │ 0     │
│ freelist-trunk │ 3     │
│ freelist-leaf  │ 671   │
│ pointer-map    │ 0     │
│ lock-byte      │ 0     │
│ unaccounted    │ 0     │
│ total          │ 676   │
└────────────────┴───────┘

── D. auto_vacuum=FULL adds pointer-map pages ──────────────────────────

The course runs auto_vacuum=NONE; the parser still accounts for pointer maps.
Page size 1024, 400 rows of 900 bytes, auto_vacuum=FULL.
┌────────────────────────────────────┬───────┬───────────────────────────────────┐
│                name                │ value │               note                │
├────────────────────────────────────┼───────┼───────────────────────────────────┤
│ header byte 52 (largest root page) │ 3     │ not 0, so auto-vacuum is on: FULL │
│ pointer-map spacing                │ 205   │ usable/5+1 pages, from page 2     │
└────────────────────────────────────┴───────┴───────────────────────────────────┘

D1. The pointer-map pages found: page 2, then one every spacing pages.
┌────────────┬─────────────┬──────┐
│  scenario  │    role     │ page │
├────────────┼─────────────┼──────┤
│ autovacuum │ pointer-map │ 2    │
│ autovacuum │ pointer-map │ 207  │
└────────────┴─────────────┴──────┘

D2. Pages by role.
┌────────────────┬───────┐
│      role      │ pages │
├────────────────┼───────┤
│ table-interior │ 5     │
│ table-leaf     │ 401   │
│ index-interior │ 0     │
│ index-leaf     │ 0     │
│ overflow       │ 0     │
│ freelist-trunk │ 0     │
│ freelist-leaf  │ 0     │
│ pointer-map    │ 2     │
│ lock-byte      │ 0     │
│ unaccounted    │ 0     │
│ total          │ 408   │
└────────────────┴───────┘

D3. The census against SQLite.
┌────────────┬────────────┬──────────────┬────────────┬─────────────┬──────────────────────┬───────────────┐
│  scenario  │ page_count │ sqlite_pages │ free_pages │ sqlite_free │ btree_pages_compared │ not_in_dbstat │
├────────────┼────────────┼──────────────┼────────────┼─────────────┼──────────────────────┼───────────────┤
│ autovacuum │ 408        │ 408          │ 0          │ 0           │ 406                  │ 2             │
└────────────┴────────────┴──────────────┴────────────┴─────────────┴──────────────────────┴───────────────┘

── E. A file larger than 1 GiB: the lock-byte page ─────────────────────

The page that holds byte offset 1073741824 is never used. The file is built
by editing a 2-page database with page size 65536: the in-header size is
raised, page 3 becomes a freelist trunk that lists every later page except the
lock-byte page, and the rest of the file is a hole (sparse).
┌────────────────────────┬────────────┬─────────────────────────────────────────────────────────────────────────────┐
│          name          │   value    │                                    note                                     │
├────────────────────────┼────────────┼─────────────────────────────────────────────────────────────────────────────┤
│ SQLite integrity_check │ ok         │ on the edited file, read-only                                               │
│ file size (bytes)      │ 1073872896 │ 1 GiB is 1073741824; everything after page 3 is a hole                      │
│ in-header size (pages) │ 16386      │ page 3 is a freelist trunk listing pages 4..16386 except the lock-byte page │
│ lock-byte page         │ 16385      │ 1073741824/65536 + 1                                                        │
└────────────────────────┴────────────┴─────────────────────────────────────────────────────────────────────────────┘

E1. Pages by role, and the lock-byte page the census found.
┌────────────────┬───────┐
│      role      │ pages │
├────────────────┼───────┤
│ table-interior │ 0     │
│ table-leaf     │ 2     │
│ index-interior │ 0     │
│ index-leaf     │ 0     │
│ overflow       │ 0     │
│ freelist-trunk │ 1     │
│ freelist-leaf  │ 16382 │
│ pointer-map    │ 0     │
│ lock-byte      │ 1     │
│ unaccounted    │ 0     │
│ total          │ 16386 │
└────────────────┴───────┘
┌───────────┬───────┐
│   role    │ page  │
├───────────┼───────┤
│ lock-byte │ 16385 │
└───────────┴───────┘

E2. SQLite accepts the file: its page count and freelist count equal the census.
┌──────────┬────────────┬──────────────┬────────────┬─────────────┐
│ scenario │ page_count │ sqlite_pages │ free_pages │ sqlite_free │
├──────────┼────────────┼──────────────┼────────────┼─────────────┤
│ large    │ 16386      │ 16386        │ 16383      │ 16383       │
└──────────┴────────────┴──────────────┴────────────┴─────────────┘

── F. What the inspector must reject ───────────────────────────────────

One damaged structure per file, in a 6-page rollback-mode copy. For each,
what our parser says and what PRAGMA integrity_check says. Texts are cut
into lines of at most 100 characters.

F0. The pages of the undamaged file (table t has root page 2):
┌──────────────────────────────────────────────────────────────────────────────────┐
│                                      pages                                       │
├──────────────────────────────────────────────────────────────────────────────────┤
│ 1=table-leaf 2=table-leaf 3=overflow 4=overflow 5=freelist-trunk 6=freelist-leaf │
└──────────────────────────────────────────────────────────────────────────────────┘

F1. The nine damaged copies, one structure each.
┌─────────┬────────────────────────────────────────────────────┐
│ case_no │                       damage                       │
├─────────┼────────────────────────────────────────────────────┤
│ F1      │ magic: byte 0 changed                              │
│ F2      │ truncated: last page (a freelist leaf) cut in half │
│ F3      │ page 2 type byte 0x0d -> 0x07                      │
│ F4      │ page 2 cell count 2 -> 3000                        │
│ F5      │ page 2 first cell pointer -> 9                     │
│ F6      │ row 1 payload size varint -> 0x7f                  │
│ F7      │ overflow page 3 points back to itself              │
│ F8      │ freelist trunk (header 32) -> page 2, a table page │
│ F9      │ freelist count (header 36) + 1                     │
└─────────┴────────────────────────────────────────────────────┘

F2. What each parser says about each copy. Our parser must return an error
without panicking. SQLite does not always object: for F2 it reads the missing
half of the last page as zeros and calls the file ok.
┌─────────┬────────┬───────────────────────────────────────────────────────────────────────────────────────┐
│ case_no │  who   │                                        verdict                                        │
├─────────┼────────┼───────────────────────────────────────────────────────────────────────────────────────┤
│ F1      │ ours   │ error: not an SQLite database file: file offset 0: magic is "XQLite format 3\x00"     │
│         │ SQLite │ error: file is not a database (26)                                                    │
│ F2      │ ours   │ error: file is truncated: file offset 22528: file size 22528 is not a multiple of the │
│         │        │  page size 4096                                                                       │
│         │ SQLite │ integrity_check: ok                                                                   │
│ F3      │ ours   │ error: tree "t" (root 2): database file is malformed: page 2 offset 0: page type byte │
│         │        │  0x07 is not 0x02, 0x05, 0x0a or 0x0d                                                 │
│         │ SQLite │ error: database disk image is malformed (11)                                          │
│ F4      │ ours   │ error: tree "t" (root 2): database file is malformed: page 2 offset 3: 3000 cells do  │
│         │        │ not fit: the cell pointer array would end at 6008, usable size 4096                   │
│         │ SQLite │ error: database disk image is malformed (11)                                          │
│ F5      │ ours   │ error: tree "t" (root 2): database file is malformed: page 2 offset 8: cell 0 pointer │
│         │        │  9 is outside the cell content area 2260..4092                                        │
│         │ SQLite │ integrity_check: *** in database main *** / Tree 2 page 2 cell 0: Offset 9 out of ran │
│         │        │ ge 2260..4092 / Page 3: never used / Page 4: never used / (+1 more lines)             │
│ F6      │ ours   │ error: tree "t" (root 2): database file is malformed: page 2 offset 7: 1699 bytes of  │
│         │        │ the cell content area belong to no cell or freeblock, the header reports 0 fragmented │
│         │        │  bytes                                                                                │
│         │ SQLite │ integrity_check: *** in database main *** / Tree 2 page 2 cell 0: Rowid 21 out of ord │
│         │        │ er / Fragmentation of 1699 bytes reported as 0 on page 2 / Page 3: never used / (+2 m │
│         │        │ ore lines)                                                                            │
│ F7      │ ours   │ error: tree "t" (root 2): database file is malformed: page 3: overflow chain of cell  │
│         │        │ 0 loops back to page 3                                                                │
│         │ SQLite │ integrity_check: *** in database main *** / Tree 2 page 2 cell 0: 2nd reference to pa │
│         │        │ ge 3 / Page 4: never used                                                             │
│ F8      │ ours   │ error: freelist: database file is malformed: page 2: page claimed twice: as table-lea │
│         │        │ f of "t" and as freelist-trunk                                                        │
│         │ SQLite │ integrity_check: *** in database main *** / Freelist: freelist leaf count too big on  │
│         │        │ page 2 / Freelist: invalid page number 218103808 / 2nd reference to page 2 / (+4 more │
│         │        │  lines)                                                                               │
│ F9      │ ours   │ error: freelist: database file is malformed: file offset 36: header counts 3 freelist │
│         │        │  pages, the trunk chain holds 2                                                       │
│         │ SQLite │ integrity_check: *** in database main *** / Freelist: size is 2 but should be 3       │
└─────────┴────────┴───────────────────────────────────────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  the census sums to page_count 270
A  ✓  every page has exactly one role: none counted twice (the census fails on a second claim), none missed
A  ✓  PRAGMA page_count = 270
A  ✓  freelist length 3 = PRAGMA freelist_count 3
A  ✓  all 267 b-tree and overflow pages match dbstat in tree, type, ncell, payload, unused and mx_payload; the 3 other pages are not in dbstat
A  ✓  the chains followed from the documents rows hold every overflow page in the file
A  ✓  table accounts: same rows and values, read from the bytes and through SQLite
A  ✓  table documents: same rows and values, read from the bytes and through SQLite
A  ✓  table events: same rows and values, read from the bytes and through SQLite
B  ✓  limit.db holds the three rows 1, 2 and 3
B  ✓  row 1: payload 4061 (= maxLocal) stays entirely on the leaf
B  ✓  row 2: payload 4062 keeps only minLocal 489 bytes on the leaf and spills 3573 to one overflow page
B  ✓  row 3: payload 8153 keeps 4061 on the leaf so its overflow exactly fills one page (4092 bytes)
B  ✓  local limit: census complete, page_count 7 and freelist_count 0 agree, 7 pages match dbstat
C  ✓  the freelist spans more than one trunk page
C  ✓  long freelist: census complete, page_count 676 and freelist_count 674 agree, 2 pages match dbstat
D  ✓  pointer-map pages are pages 2 and 2 + usable/5 + 1 in this 408-page file
D  ✓  auto_vacuum=FULL: census complete, page_count 408 and freelist_count 0 agree, 406 pages match dbstat
E  ✓  page 16385 is counted as the lock-byte page, not as lost
E  ✓  SQLite accepts the file: integrity_check ok, and page_count and freelist_count equal the census
F  ✓  F1: our parser returns an error, without panicking
F  ✓  F2: our parser returns an error, without panicking
F  ✓  F2: SQLite rounds a partial last page up and reads the missing bytes as zeros, so it reports ok; our parser is stricter and refuses the file
F  ✓  F3: our parser returns an error, without panicking
F  ✓  F4: our parser returns an error, without panicking
F  ✓  F5: our parser returns an error, without panicking
F  ✓  F6: our parser returns an error, without panicking
F  ✓  F7: our parser returns an error, without panicking
F  ✓  F8: our parser returns an error, without panicking
F  ✓  F9: our parser returns an error, without panicking

30 of 30 claims hold

Experiment verdict: CENSUS OK
```

#### Reading the evidence

- **Section A (Part A: full census of R0).**
  - **Schema and trees (A2–A4):** Decoding page 1 yields the four `sqlite_schema` rows: tables `accounts` (root 2), `documents` (root 3), `events` (root 4), and index `events_by_account` (root 28). Walking those roots plus `sqlite_schema` (root 1) accounts for 97 B-tree pages and 170 overflow pages. Following header offset 32 finds freelist trunk `230` listing leaves `[231 232]`.
  - **Role sum (A1, A5):** `table-interior=3`, `table-leaf=89`, `index-interior=1`, `index-leaf=4`, `overflow=170`, `freelist-trunk=1`, `freelist-leaf=2`, `pointer-map=0`, `lock-byte=0`, `unaccounted=0`; sum `270`, matching `PRAGMA page_count = 270` and `PRAGMA freelist_count = 3`.
  - **`dbstat` cross-check (A6–A7):** All 267 B-tree and overflow pages print `match` across all six columns (`name`, `pagetype`, `ncell`, `payload`, `unused`, `mx_payload`). Freelist pages `230`, `231`, and `232` are absent from `dbstat`. `accounts` has 183 rows (`177` on leaf 260 + `6` on leaf 261) and `184` cells total because its interior root (page 2) holds `1` divider cell.
  - **Overflow chains and row hashes (A8–A9):** 83 `documents` rows overflow across all 170 overflow pages (longest chain 3 pages). Row 6's chain `[9 8 23]` is non-monotonic because pages 8 and 9 were reused from the freelist. Canonical row hashes (tagging every value with `typeof()` and substituting the cell's `rowid` for column 0 `NULL`, which is how SQLite stores an `INTEGER PRIMARY KEY` column in the record body) match SQLite identically for all three tables: `accounts` (`183` rows, `8017137363561914`), `documents` (`96` rows, `973d4aa8fa24c3aa`), and `events` (`1177` rows, `afdcccdab84ea9c5`).
- **Section B (Part B: local payload boundary).** At usable size 4,096, `maxLocal` is `4061` and `minLocal` is `489`. With a 4-byte record header (`CREATE TABLE b(id INTEGER PRIMARY KEY, v BLOB)`), the experiment records each row's parsed `(payload, local, overflow pages, bytes on the last overflow page)` from `WalkTable` and `OverflowChain`: row 1 (4,057-byte blob, payload `4061`) stays 100% local (`0` overflow pages); row 2 (4,058-byte blob, payload `4062`) drops local storage to `489` bytes and spills `3573` bytes onto 1 overflow page; row 3 (8,149-byte blob, payload `8153`) keeps `4061` bytes locally so its overflow fills one overflow page to the exact byte (`4092` bytes).
- **Section C (Part C: multi-trunk freelist).** At page size 1,024, a trunk holds at most `1024/4 - 2 = 254` leaves on read, while `freePage2` caps writes at `1024/4 - 8 = 248` leaves. Deleting 600 rows leaves three trunks in chain order: trunk `502` (newest head, `175` leaves), trunk `252` (`248` leaves), and trunk `3` (`248` leaves), totaling `3 + 671 = 674` free pages out of `676`.
- **Section D (Part D: `auto_vacuum=FULL` pointer-map pages).** At page size 1,024, header byte 52 is `3` (`FULL`), and pointer-map pages appear every `1024/5 + 1 = 205` pages starting at page 2: pages `2` and `207`. All 406 non-pointer-map pages match `dbstat`, and the 2 pointer-map pages are accounted for cleanly.
- **Section E (Part E: > 1 GiB lock-byte page).** Editing a 2-page, 65,536-byte-page database into a 16,386-page (`1,073,872,896`-byte) sparse file places the lock-byte page at `1073741824 / 65536 + 1 = 16385`. Page 3 is a freelist trunk listing all `16,382` pages from 4..16386 except 16385 (actual allocated disk blocks are printed on an `INFO:` line of the experiment's own output, not stored). Parts A–D load closed files into memory with `os.ReadFile` + `dbfile.FromBytes`; Part E opens its 1 GiB sparse file with `dbfile.Open`, runs the census, and closes the descriptor before opening SQLite so no `os.Open` descriptor ever overlaps a live SQLite connection. Both the census (`lock-byte=1`, `unaccounted=0`) and SQLite (`integrity_check=ok`, `page_count=16386`, `freelist_count=16383`) accept the file.
- **Section F (Part F: nine damaged files).** Every case returns a structured error without panicking; Section 6.1 examines all nine cases in detail.

### 5.5 Experiment 2.3 — Page manifest, rung R1 (`2.3-page-manifest`)

- **Claim:** The ordered list of page images (`pages.bin`) plus its per-page SHA-256 manifest (`manifest.txt`) is a complete saved state of a closed database: writing those verified pages back in order rebuilds a database file that is E1 byte-identical to the original and passes `PRAGMA integrity_check`. Two independent builds of R0 produce line-for-line identical manifests. Tampered captures are refused with the offending page named, and databases that are not at rest (non-empty `-wal` or hot `-journal`) are refused before capture.
- **Capture / rebuild:** capture = `pages.bin` + `manifest.txt` via `capture.Pages`; rebuild = verified sequential page write via `capture.Rebuild`.
- **Evidence form:** `EXACT` (every stored CSV table must match a fresh run byte for byte).
- **Run command** (from the repository root; starts the `lab` container, runs the experiment inside it, and prints the rendered walkthrough; add `-i` to stay in the DuckDB shell afterwards with all result tables loaded):
  ```text
  make run EXP=2.3
  ```

<!-- include: experiments/2.3-page-manifest/main.go -->
```go
// Experiment 2.3 demonstrates Rebuild Ladder rung R1: capturing every page of
// the closed R0 database with internal/capture into pages.bin plus a per-page
// SHA-256 manifest, rebuilding a byte-identical (E1) database from the capture
// alone, refusing five kinds of tampered captures with the offending page named,
// and refusing databases that are not at rest (uncheckpointed -wal or hot -journal).
//
// Run from the repo root:
//
//	go run ./ch02-file-format-and-pages/experiments/2.3-page-manifest
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"

	_ "modernc.org/sqlite"
)

const (
	childLeaveWAL         = "child-leave-wal"
	childLeaveHotJournal  = "child-leave-hot-journal"
	childSpillCacheSize   = 10
	childExitWithoutClose = 3
)

var (
	set         *results.Set
	comparisons *results.Table
	notAtRest   *results.Table
)

func openTables(dir string) {
	set = results.MustOpen(dir)
	comparisons = set.Table("comparisons", "part", "name", "result", "equal", "first_differing_page", "differing_pages")
	notAtRest = set.Table("not_at_rest", "scenario", "seq", "observation", "value")
}

var observations = map[string]int{}

// observe records one numbered observation of a not-at-rest scenario.
func observe(scenario, observation string, value any) {
	observations[scenario]++
	notAtRest.Row(scenario, observations[scenario], observation, value)
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

// nilIfZero turns a missing page number into an empty field.
func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func relative(dir, text string) string {
	return strings.ReplaceAll(text, dir+string(os.PathSeparator), "")
}

func integrityCheck(ctx context.Context, path string) string {
	db := must(recipe.Default().Open(ctx, path))("open " + filepath.Base(path))
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return result
}

func partA(ctx context.Context, dir string) (string, string) {
	r := recipe.Default()
	captureRows := set.Table("captures", "run", "database", "capture_dir", "steps", "pages", "page_size", "pages_bin_bytes", "manifest_format", "manifest_sha256")
	manifestRows := set.Table("manifest", "run", "page", "role", "sha256")
	var manifests []equal.Manifest
	var paths, captures []string
	for run := 1; run <= 2; run++ {
		path := filepath.Join(dir, fmt.Sprintf("run-%d", run), "r0.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal("mkdir", err)
		}
		if err := r.Build(ctx, path, r.Steps); err != nil {
			fatal("build", err)
		}
		captureDir := filepath.Join(dir, fmt.Sprintf("run-%d", run), "capture")
		m := must(capture.Pages(path, captureDir))("capture")
		captureRows.Row(run, relative(dir, path), relative(dir, captureDir), r.Steps, m.PageCount(), m.PageSize,
			must(os.Stat(filepath.Join(captureDir, capture.PagesFile)))("stat").Size(), equal.ManifestFormat, m.SHA256())
		for _, e := range m.Entries {
			manifestRows.Row(run, e.Page, e.Role, e.SHA256)
		}
		manifests = append(manifests, m)
		paths = append(paths, path)
		captures = append(captures, captureDir)
	}
	diff := equal.CompareManifests(manifests[0], manifests[1])
	comparisons.Row("A", "run 1 vs run 2", diff, diff.Equal, nilIfZero(diff.FirstDifferingPage), diff.DifferingPages)
	set.Check("A", diff.Equal && manifests[0].Text() == manifests[1].Text(), "two runs of the recipe give manifests identical line for line")
	return paths[0], captures[0]
}

func partB(ctx context.Context, dir, original, captureDir string) {
	rebuilt := filepath.Join(dir, "rebuilt", "from-capture.db")
	if err := os.MkdirAll(filepath.Dir(rebuilt), 0o755); err != nil {
		fatal("mkdir", err)
	}
	m := must(capture.Rebuild(captureDir, rebuilt))("rebuild")
	result := must(equal.CompareFiles(original, rebuilt, m.PageSize))("compare")
	comparisons.Row("B", "original vs rebuilt", result, result.Equal, nilIfZero(result.FirstDifferingPage), result.DifferingPages)
	set.Check("B", result.Equal, "sha256 of the rebuild equals sha256 of the original")
	match := must(recipe.Default().MatchStep(ctx, rebuilt))("match step")
	integrity := integrityCheck(ctx, rebuilt)
	set.Table("rebuild", "rebuilt", "from_capture", "pages_written", "sha256", "recipe_lookup", "integrity_check").
		Row(relative(dir, rebuilt), relative(dir, captureDir), m.PageCount(), result.SHA256B, match, integrity)
	set.Check("B", match.Found() && match.Exact && match.Step == 30, "the rebuild is the recipe's state after step 30, byte for byte")
	set.Check("B", integrity == "ok", "SQLite opens the rebuild and integrity_check returns ok")
	again := must(equal.FileSHA256(rebuilt))("hash")
	set.Check("B", again == result.SHA256A && recipe.CheckAtRest(rebuilt) == nil, "opening the rebuild with SQLite and closing it changed nothing and left no side files")

	step29 := filepath.Join(dir, "step-29", "r.db")
	if err := os.MkdirAll(filepath.Dir(step29), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.Default().Build(ctx, step29, 29); err != nil {
		fatal("build step 29", err)
	}
	m29 := must(capture.Pages(step29, filepath.Join(dir, "step-29", "capture")))("capture step 29")
	diff := equal.CompareManifests(m29, m)
	comparisons.Row("B", "step 29 vs step 30", diff, diff.Equal, nilIfZero(diff.FirstDifferingPage), diff.DifferingPages)
	set.Check("B", !diff.Equal && diff.FirstDifferingPage == 1, "the manifest comparison names the first differing page (page 1: the header changes on every recipe step)")
}

// tamper defines one capture-corruption test case and the page-naming substring
// that capture.Rebuild's refusal must contain.
type tamper struct {
	name string
	want string
	edit func(entries []string) []string
	page func(pages []byte, pageSize int)
}

func partC(dir, captureDir string) {
	tampered := set.Table("tampered", "case_no", "damage", "wanted", "refusal", "output_written")
	original := must(os.ReadFile(filepath.Join(captureDir, capture.ManifestFile)))("read manifest")
	pages := must(os.ReadFile(filepath.Join(captureDir, capture.PagesFile)))("read pages")
	cases := []tamper{
		{name: "drop the manifest entry for page 100", want: "page 100: missing", edit: func(e []string) []string { return append(e[:99:99], e[100:]...) }},
		{name: "list page 50 twice (in place of page 51)", want: "page 50: listed twice", edit: func(e []string) []string { e[50] = e[49]; return e }},
		{name: "swap the entries for pages 120 and 121", want: "page 121: out of order", edit: func(e []string) []string { e[119], e[120] = e[120], e[119]; return e }},
		{name: "flip one byte in the image of page 200", want: "capture page 200:", page: func(p []byte, size int) { p[199*size+1000] ^= 0x01 }},
		{name: "swap the images of pages 7 and 8", want: "capture page 7:", page: func(p []byte, size int) {
			a := append([]byte(nil), p[6*size:7*size]...)
			copy(p[6*size:7*size], p[7*size:8*size])
			copy(p[7*size:8*size], a)
		}},
	}
	for i, c := range cases {
		target := filepath.Join(dir, "tampered", fmt.Sprintf("c%d", i+1))
		if err := os.MkdirAll(target, 0o755); err != nil {
			fatal("mkdir", err)
		}
		manifestText := string(original)
		pageBytes := append([]byte(nil), pages...)
		if c.edit != nil {
			lines := strings.Split(strings.TrimSuffix(manifestText, "\n"), "\n")
			entries := c.edit(append([]string(nil), lines[3:]...))
			lines = append(lines[:3:3], entries...)
			lines[2] = fmt.Sprintf("pages %d", len(entries))
			manifestText = strings.Join(lines, "\n") + "\n"
			if len(entries)*4096 < len(pageBytes) {
				pageBytes = pageBytes[:len(entries)*4096]
			}
		}
		if c.page != nil {
			c.page(pageBytes, 4096)
		}
		if err := os.WriteFile(filepath.Join(target, capture.ManifestFile), []byte(manifestText), 0o644); err != nil {
			fatal("write", err)
		}
		if err := os.WriteFile(filepath.Join(target, capture.PagesFile), pageBytes, 0o644); err != nil {
			fatal("write", err)
		}
		out := filepath.Join(target, "rebuilt.db")
		_, err := capture.Rebuild(target, out)
		_, statErr := os.Stat(out)
		tampered.Row(fmt.Sprintf("C%d", i+1), c.name, c.want, err, !errors.Is(statErr, os.ErrNotExist))
		set.Check("C", err != nil && strings.Contains(err.Error(), c.want) && errors.Is(statErr, os.ErrNotExist), fmt.Sprintf("C%d: the rebuild is refused naming %q, and no output file is written", i+1, c.want))
	}
}

func runChild(mode, path string) int {
	command := must(trace.SelfCommand(mode, path))("self command")
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		fatal("child", err)
	}
	return 0
}

// childLeave runs in a separate process that leaves the database not at rest
// and exits with status 3 without closing its SQLite handle. In WAL mode it
// commits one UPDATE so frames remain in -wal; in rollback mode it sets a tiny
// cache_size=10 so updating every document spills dirty pages to the database
// file and syncs a hot -journal header (src/pager.c:1494-1501, 4377) before exit.
func childLeave(mode, path string) {
	ctx := context.Background()
	r := recipe.Default()
	options := r.SQLiteOptions()
	if mode == childLeaveHotJournal {
		options = r.WithJournalMode("delete").SQLiteOptions()
		options.Conn.CacheSize = childSpillCacheSize
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if mode == childLeaveWAL {
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1000 WHERE id = 1"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(childExitWithoutClose)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE documents SET body = zeroblob(length(body))")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(childExitWithoutClose)
}

func copyMainFileOnly(from, to string) {
	if err := os.WriteFile(to, must(os.ReadFile(from))("read"), 0o644); err != nil {
		fatal("copy", err)
	}
}

// queryString runs a single-column SQL query against path (passing mode=ro via
// url.Values when readOnly is true) and fails immediately on query or close error.
func queryString(ctx context.Context, path string, readOnly bool, query string) string {
	values := url.Values{}
	if readOnly {
		values.Set("mode", "ro")
	}
	uri := must(sqlitex.FileURI(path, values))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	var value string
	if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
		fatal(query, err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return value
}

func sideFiles(path string) string {
	var parts []string
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if info, err := os.Stat(path + suffix); err == nil {
			parts = append(parts, fmt.Sprintf("%s %d bytes", suffix, info.Size()))
		}
	}
	if len(parts) == 0 {
		return "no side files"
	}
	return strings.Join(parts, ", ")
}

func partD(ctx context.Context, dir string) {
	r := recipe.Default()

	walDir := filepath.Join(dir, "not-at-rest", "wal")
	if err := os.MkdirAll(walDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(walDir, "r.db")
	if err := r.Build(ctx, path, 6); err != nil {
		fatal("build", err)
	}
	before := must(equal.FileSHA256(path))("hash")
	exit := runChild(childLeaveWAL, path)
	observe("D1", "child exit status", exit)
	observe("D1", "side files after the child", sideFiles(path))
	_, err := capture.Pages(path, filepath.Join(walDir, "capture"))
	observe("D1", "capture", relative(walDir, fmt.Sprint(err)))
	set.Check("D", errors.Is(err, capture.ErrNotAtRest), "D1: the capture is refused: the -wal holds committed frames")
	stale := filepath.Join(walDir, "stale-copy.db")
	copyMainFileOnly(path, stale)
	balanceQuery := "SELECT balance FROM accounts WHERE id = 1"
	staleBalance := queryString(ctx, stale, true, balanceQuery)
	observe("D1", "database file alone: sha256 unchanged by the commit", must(equal.FileSHA256(path))("hash") == before)
	observe("D1", "database file alone: account 1 balance", staleBalance)
	liveBalance := queryString(ctx, path, false, balanceQuery)
	observe("D1", "SQLite with the -wal: account 1 balance", liveBalance)
	observe("D1", "side files after the last close", sideFiles(path))
	set.Check("D", staleBalance != liveBalance, "D1: a page capture taken from the file alone would have lost the committed UPDATE")
	m := must(capture.Pages(path, filepath.Join(walDir, "capture")))("capture after checkpoint")
	observe("D1", "capture after the checkpoint: pages", m.PageCount())
	observe("D1", "capture after the checkpoint: manifest sha256", m.SHA256())
	set.Check("D", must(equal.FileSHA256(path))("hash") != before, "D1: once at rest, the capture includes the UPDATE (the file changed at the checkpoint)")

	journalDir := filepath.Join(dir, "not-at-rest", "journal")
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path = filepath.Join(journalDir, "r.db")
	rollback := r.WithJournalMode("delete")
	if err := rollback.Build(ctx, path, 6); err != nil {
		fatal("build", err)
	}
	before = must(equal.FileSHA256(path))("hash")
	bodyHashQuery := "SELECT count(*) || ' rows, ' || sum(body = zeroblob(length(body))) || ' bodies all zero' FROM documents"
	exit = runChild(childLeaveHotJournal, path)
	observe("D2", "child exit status", exit)
	observe("D2", "side files after the child", sideFiles(path))
	_, err = capture.Pages(path, filepath.Join(journalDir, "capture"))
	observe("D2", "capture", relative(journalDir, fmt.Sprint(err)))
	set.Check("D", errors.Is(err, capture.ErrNotAtRest), "D2: the capture is refused: a hot -journal exists")
	torn := filepath.Join(journalDir, "torn-copy.db")
	copyMainFileOnly(path, torn)
	changed := must(equal.FileSHA256(path))("hash") != before
	tornView := queryString(ctx, torn, true, bodyHashQuery)
	observe("D2", "database file alone: changed by the spill", changed)
	observe("D2", "database file alone: documents", tornView)
	recovered := queryString(ctx, path, false, bodyHashQuery)
	observe("D2", "SQLite with the hot journal (rolls it back): documents", recovered)
	observe("D2", "side files after the last close", sideFiles(path))
	set.Check("D", changed && tornView != recovered, "D2: the file alone holds part of an uncommitted transaction; a capture of it would be torn")
	set.Check("D", must(equal.FileSHA256(path))("hash") == before, "D2: after SQLite rolled the hot journal back, the file is byte-identical to its state before the transaction")
	m = must(capture.Pages(path, filepath.Join(journalDir, "capture")))("capture after rollback")
	observe("D2", "capture after the rollback: pages", m.PageCount())
}

func main() {
	if len(os.Args) == 3 && (os.Args[1] == childLeaveWAL || os.Args[1] == childLeaveHotJournal) {
		childLeave(os.Args[1], os.Args[2])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("2.3")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	openTables(dir)
	original, captureDir := partA(ctx, dir)
	partB(ctx, dir, original, captureDir)
	partC(dir, captureDir)
	partD(ctx, dir)
	set.Finish("MANIFEST")
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

<!-- include: experiments/2.3-page-manifest/report.sql -->
```sql
-- Walkthrough of experiment 2.3. Run it with:
--   go run ./cmd/course run 2.3        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   captures      part A: one row per run of the recipe, with its capture directory
--   manifest      part A: one row per page of each run's manifest (page, role, sha256)
--   comparisons   parts A and B: one row per manifest or file comparison, as the library reports it
--   rebuild       part B: one row, the database rebuilt from the capture alone
--   tampered      part C: one row per damaged capture, with the refusal
--   not_at_rest   part D: numbered observations of the two not-at-rest scenarios
--   checks        every claim the experiment asserted (the runner lists it last)

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  2.3  Can the pages of a database be captured and rebuilt exactly?'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'Rebuild Ladder rung R1: internal/capture reads every page of the closed R0'
.print 'database into pages.bin and writes manifest.txt, one line per page with'
.print 'its role and the sha256 of its bytes. The experiment rebuilds a database'
.print 'from the capture alone and checks it is byte-identical, refuses five kinds'
.print 'of damaged captures, and refuses databases that are not at rest.'

.print
.print '── A. Capture: every page of the closed R0 database ────────────────────'
.print
.print 'A1. The recipe is run twice and each result captured. pages.bin holds'
.print 'pages x page_size bytes; manifest.txt starts with the format line, the page'
.print 'size and the page count, then one line per page.'

SELECT run, database, capture_dir, steps, pages, page_size, pages_bin_bytes
FROM captures ORDER BY run;

.print
.print 'A2. The manifest as a whole: its format line and the sha256 of its text.'
.print 'Both runs must give the same manifest.'

SELECT run, manifest_format, manifest_sha256 FROM captures ORDER BY run;

.print
.print 'A3. Run 1 against run 2, as the library compares them, and from the rows:'
.print 'the number of pages whose role or sha256 differ between the runs.'

SELECT name, result, equal, differing_pages
FROM comparisons WHERE part = 'A' ORDER BY name;

SELECT count(*) FILTER (a.role IS DISTINCT FROM b.role OR a.sha256 IS DISTINCT FROM b.sha256) AS differing_pages,
       count(*) AS pages_compared
FROM (SELECT * FROM manifest WHERE run = 1) a
FULL JOIN (SELECT * FROM manifest WHERE run = 2) b USING (page);

.print
.print 'A4. The roles in the manifest (run 1).'

SELECT role, count(*) AS pages FROM manifest WHERE run = 1 GROUP BY role ORDER BY pages DESC, role;

.print
.print 'A5. The manifest of run 1, one line per page.'

SELECT page, role, sha256 FROM manifest WHERE run = 1 ORDER BY page;

.print
.print '── B. Rebuild from the capture alone and prove it equal (E1) ───────────'
.print
.print 'B1. The rebuild: pages written in order, its sha256, what the recipe lookup'
.print 'says it is, and the integrity_check of SQLite.'

SELECT rebuilt, from_capture, pages_written, recipe_lookup, integrity_check FROM rebuild;

.print
.print 'B2. The rebuild against the original, and the manifest after step 29'
.print 'against the manifest after step 30. They differ first at page 1: the header'
.print 'changes on every recipe step.'

SELECT name, equal, first_differing_page, differing_pages
FROM comparisons WHERE part = 'B' ORDER BY name;

.print
.print 'The words of the library for each comparison:'

SELECT result FROM comparisons WHERE part = 'B' ORDER BY name;

.print
.print 'The sha256 of the rebuild, which is also the sha256 of the original:'

SELECT sha256 FROM rebuild;

.print
.print '── C. A damaged capture is refused, and the refusal names the page ─────'
.print
.print 'Each row is a copy of the capture with one thing changed. The rebuild must'
.print 'refuse it with a message that contains the text in wanted, and must not write'
.print 'an output file.'

SELECT case_no, damage, wanted, output_written FROM tampered ORDER BY case_no;

.print
.print 'The refusals:'

SELECT case_no, refusal FROM tampered ORDER BY case_no;

.print
.print '── D. A database that is not at rest is refused ────────────────────────'
.print
.print 'Its file alone is stale or torn. Each scenario starts a child process that'
.print 'leaves the database not at rest and exits with status 3 without closing'
.print 'its connection.'
.print
.print 'D1. WAL mode: the child commits one UPDATE (account 1 gains 1000). The'
.print 'commit lives in the -wal, so the database file alone is stale.'

SELECT CASE WHEN g.i = 0 THEN seq END AS seq,
       CASE WHEN g.i = 0 THEN observation END AS observation,
       substr(value, g.i * 70 + 1, 70) AS value
FROM not_at_rest, generate_series(0, (length(value) - 1) // 70) AS g(i)
WHERE scenario = 'D1'
ORDER BY not_at_rest.seq, g.i;

.print
.print 'D2. Rollback mode: the child sets cache_size=10, updates every document body'
.print 'in one transaction, which spills dirty pages to the file, and exits before'
.print 'COMMIT. The file alone holds part of an uncommitted transaction.'

SELECT CASE WHEN g.i = 0 THEN seq END AS seq,
       CASE WHEN g.i = 0 THEN observation END AS observation,
       substr(value, g.i * 70 + 1, 70) AS value
FROM not_at_rest, generate_series(0, (length(value) - 1) // 70) AS g(i)
WHERE scenario = 'D2'
ORDER BY not_at_rest.seq, g.i;
```

Captured evidence: the walkthrough as the `lab` container rendered it. The tables behind it are stored in [`evidence/2.3-page-manifest/`](evidence/2.3-page-manifest/), and `make check ARGS='evidence ch02'` compares a fresh run with them byte for byte.

<!-- include: evidence/2.3-page-manifest.txt -->
```text

════════════════════════════════════════════════════════════════════════
  2.3  Can the pages of a database be captured and rebuilt exactly?
════════════════════════════════════════════════════════════════════════

Rebuild Ladder rung R1: internal/capture reads every page of the closed R0
database into pages.bin and writes manifest.txt, one line per page with
its role and the sha256 of its bytes. The experiment rebuilds a database
from the capture alone and checks it is byte-identical, refuses five kinds
of damaged captures, and refuses databases that are not at rest.

── A. Capture: every page of the closed R0 database ────────────────────

A1. The recipe is run twice and each result captured. pages.bin holds
pages x page_size bytes; manifest.txt starts with the format line, the page
size and the page count, then one line per page.
┌─────┬─────────────┬───────────────┬───────┬───────┬───────────┬─────────────────┐
│ run │  database   │  capture_dir  │ steps │ pages │ page_size │ pages_bin_bytes │
├─────┼─────────────┼───────────────┼───────┼───────┼───────────┼─────────────────┤
│ 1   │ run-1/r0.db │ run-1/capture │ 30    │ 270   │ 4096      │ 1105920         │
│ 2   │ run-2/r0.db │ run-2/capture │ 30    │ 270   │ 4096      │ 1105920         │
└─────┴─────────────┴───────────────┴───────┴───────┴───────────┴─────────────────┘

A2. The manifest as a whole: its format line and the sha256 of its text.
Both runs must give the same manifest.
┌─────┬───────────────────────────────────┬──────────────────────────────────────────────────────────────────┐
│ run │          manifest_format          │                         manifest_sha256                          │
├─────┼───────────────────────────────────┼──────────────────────────────────────────────────────────────────┤
│ 1   │ sqlite-internals page manifest v1 │ 8d9ad6e1251dfbbe513d57fa7dcd5e8159fa7c62d227d28008ebc1a9ed92f523 │
│ 2   │ sqlite-internals page manifest v1 │ 8d9ad6e1251dfbbe513d57fa7dcd5e8159fa7c62d227d28008ebc1a9ed92f523 │
└─────┴───────────────────────────────────┴──────────────────────────────────────────────────────────────────┘

A3. Run 1 against run 2, as the library compares them, and from the rows:
the number of pages whose role or sha256 differ between the runs.
┌────────────────┬─────────────────────┬───────┬─────────────────┐
│      name      │       result        │ equal │ differing_pages │
├────────────────┼─────────────────────┼───────┼─────────────────┤
│ run 1 vs run 2 │ manifests identical │ true  │ 0               │
└────────────────┴─────────────────────┴───────┴─────────────────┘
┌─────────────────┬────────────────┐
│ differing_pages │ pages_compared │
├─────────────────┼────────────────┤
│ 0               │ 270            │
└─────────────────┴────────────────┘

A4. The roles in the manifest (run 1).
┌────────────────┬───────┐
│      role      │ pages │
├────────────────┼───────┤
│ overflow       │ 170   │
│ table-leaf     │ 89    │
│ index-leaf     │ 4     │
│ table-interior │ 3     │
│ freelist-leaf  │ 2     │
│ freelist-trunk │ 1     │
│ index-interior │ 1     │
└────────────────┴───────┘

A5. The manifest of run 1, one line per page.
┌──────┬────────────────┬──────────────────────────────────────────────────────────────────┐
│ page │      role      │                              sha256                              │
├──────┼────────────────┼──────────────────────────────────────────────────────────────────┤
│ 1    │ table-leaf     │ 1b9902e64e6c0f5d1eb4e501973304e85e7aff7d6e44912a06fbcc00e07debe9 │
│ 2    │ table-interior │ 46d56d23abed588975c19161aa6dc1f9cf7f9d31a72cfbd40b3441bf38d8cc1e │
│ 3    │ table-interior │ 860bd520e10f3ed30a0cf84ada13addbbf2fb19cc5584699ff702868986c24b0 │
│ 4    │ table-interior │ 3c53ee0581571b0fd924cb47c637b6b1ebe6425f4a04320f26390083cfd37072 │
│ 5    │ overflow       │ 7aad2880eb458697ca2b782d104c7bc12ab581778d1617616113daf7fec3d4c8 │
│ 6    │ overflow       │ 7e7afe23ae214297b337e3c4675ffdd9ab84024fc3074c15e812494dd20e4983 │
│ 7    │ overflow       │ a534c6fdb6abf78b7a7b52b27b3051790c85036beb7218b4d0ff6e6decac0a0f │
│ 8    │ overflow       │ fba5521b93297c4b8f3bef088245f945ccb51124446b306cd489d6f3e4217b15 │
│ 9    │ overflow       │ ca9a4af9e802b51d7f6b06fbd720c6514584a367091c9873a5f9aa21a71067f2 │
│ 10   │ table-leaf     │ 5bb69638130cf6381f376435e2423a5e0d06bdf068fcaf571022fc1e0ee59370 │
│ 11   │ table-leaf     │ 724eb0ed932ef10781b9172d5e32a48a1b8863c254a982c4f8ddc4d082338b96 │
│ 12   │ overflow       │ 7f5502748230a361201e00b9e58fbd6a0db0125c9ac0e24b6f138b301090d9ef │
│ 13   │ overflow       │ 12a4b4e75b3e86d31bbd4c27ee214c6b806652f6ad9af57ee636219644b7c301 │
│ 14   │ overflow       │ 34962cb641e4debab98b6f3a2fecb54e612793071b88eb82c39836a7d4aa728a │
│ 15   │ table-leaf     │ 4d3175e4b2c34d53defc5dbb504cb680b4e3296c0eb81083024329a3d622646e │
│ 16   │ overflow       │ a0572382d487533308f59de5d509f29bafed75da79a73f9812acedc32e35f10a │
│ 17   │ overflow       │ 15b724903840c0ad7abb639fa0700fedb4a1f58f43e3b0ff9005ca78bb3b5491 │
│ 18   │ overflow       │ 383820231ab24d46be1913e631aa92713455b9b6c6c9f1d021610207a9f7da14 │
│ 19   │ table-leaf     │ e3bb3b6b00cacac610514dcd58061d2a1ab831786006278c228b4785d3844497 │
│ 20   │ overflow       │ 3b65dbee9941c10a07b33942455612512b666d94d3609ff23becc45a79251df6 │
│ 21   │ table-leaf     │ deb86ca0005d14f7ab3b7da9c5ac31d4282c568de0b03e0cddc20973682cdb14 │
│ 22   │ overflow       │ e245c6247cbcc8c212299bb2f89b5327d72dfa799529b2dece4790eff92112ee │
│ 23   │ overflow       │ 59134759e86d787f8fdebc82af48ad8bc9c55a135c7fc67af8b38cb6a56c7ffe │
│ 24   │ overflow       │ 68c7d34bd3ef689df419818c41e6ddbbf30b4499c90acbd1ffd3d5bd06775cdb │
│ 25   │ table-leaf     │ 732423d9158389d0e946170f10075404559e4d8112bb83ef80c72c7a4bf15fd7 │
│ 26   │ table-leaf     │ 8006a99485269b70b926860d50f87f4ca908a513a655597a9af43fcbfc4f038f │
│ 27   │ table-leaf     │ d45416dbdf129e59d604e2fc3b7f9c802ffab7696837438dda8d2b60ed29e586 │
│ 28   │ index-interior │ 51e6ac1f4e34240f1bbf86abf31c101ad643155a33da16f3a6ee4f089ce0fc71 │
│ 29   │ overflow       │ a4e416cafa8141e1ed4cc9150cf7bc91e6bd363b510d2daf4748d511c8d253f3 │
│ 30   │ overflow       │ 2d8423154a553dbcdb5b400d9e42cc68018148d63a4ea559731e2f0c2d25ff7c │
│ 31   │ table-leaf     │ f3da9777ca90fcf540b92d3aef099433d7c73021688064744b62e39f16555a35 │
│ 32   │ overflow       │ ec7d9f3bb6c902490f0bd3cf49f07a0a3a50b8f4d00b1a32ffae31caa8e088f1 │
│ 33   │ overflow       │ 4552b82b526e55ab64d4b4c8f906cff8be626594f828c2aad32dec6beff99c94 │
│ 34   │ overflow       │ ce92b5493316efc5c6dcc493c40b2a39bcf69230138f41891dbe4ba1b33b86d0 │
│ 35   │ overflow       │ 7673f433fe64cf17405855c1361fbabf3a8fa0309a283068178d32ed3e478321 │
│ 36   │ overflow       │ 568c08e129d6dbf91a58337b21f0ec334d437816422939b5999310ac1653f069 │
│ 37   │ overflow       │ 02cf9a93058eb383f8c1e4bf6c2736e428a50bc009277feafaafc5072504d51c │
│ 38   │ table-leaf     │ c2fae8bcd1c019e21ab735b11ffa65e2e88e94bdea3924ca82466fcfa92d16ea │
│ 39   │ table-leaf     │ 568908de0766d844eaaa85f1870b7cf9d4fe78d3bf20cb38aa947bb1732b0ac8 │
│ 40   │ overflow       │ 91be9b798474f1b35c757fd09064e640b39ecb7476dcdb27cebf869725ba9559 │
│ 41   │ overflow       │ de1b5cde831492f35afa92c9be4e794c9249cfef58555b50010499a9e8aecef9 │
│ 42   │ table-leaf     │ 3a30707ad61b33660e0ded2a8d178499c314c5e6cf495359734dff401cb1957b │
│ 43   │ overflow       │ b221d22756aae42e95f33c00b839cfa8a0b0548870cc0598e0b5fa490d923c29 │
│ 44   │ overflow       │ 887b2f0e56135974ac1471a3ce699b43fb4e1bbdb6581cc031f45e506e421965 │
│ 45   │ overflow       │ 5c63bd72305ee4300a2762a0e1028b8a01861e003e41936502ae435b9f0cd45d │
│ 46   │ table-leaf     │ c8bee6ed976fd2a7ea7e61eeb0d594c73ee4a72a0159bd6132383fae8051fe94 │
│ 47   │ overflow       │ 06d3326df34a4041c367f5e94ed81bfe1e6f680a90da3a33f0a47c51a7f8b099 │
│ 48   │ table-leaf     │ d893cb79a18ea5fc17b23c8fcd7298af35196233d4058d7dfac10a994cafb39f │
│ 49   │ overflow       │ 1981d7c68d1e32ba7361d5ae699892f30fece85ee6404cc689b261fa32ec58f3 │
│ 50   │ table-leaf     │ 73fd216d724396dae4897b0180cbad5be03987df5bcf36e490ccd81ff7c34442 │
│ 51   │ overflow       │ 0c62745060da6ad711e8b596d3c84e925ed8126ba38f8cf6c4d2f6b10fb36c99 │
│ 52   │ overflow       │ 9abb60aa174193ffeac20bd41da49d251318a3166fb4ab358360dc451188aa93 │
│ 53   │ overflow       │ 4dfa14a601f073d5a84d21e3948b69f47d04c2a4d29142f6f5bf307044c962b1 │
│ 54   │ table-leaf     │ 15130a78f58e4eaa8209aa8fdd5c81f3bd5f999fe9e342040358f1cbac317d5a │
│ 55   │ overflow       │ c736558d3fbf4b32b223e2638859498c08c9cd71aafcebf88700ea3aa775722a │
│ 56   │ overflow       │ c043a6192cc1dea5f3891ef71135adee8cd2bae5680e87cd686be55f655d45b7 │
│ 57   │ overflow       │ 36f9a238db5b34f0ad7f3a32863b747f923985c01c227589aba0a4c0ace2747a │
│ 58   │ table-leaf     │ 862957a42085704cea1a2e36da9110425f16a0a454b77a8988c88cd6c0261646 │
│ 59   │ table-leaf     │ 6012e5651530caceb1c97f08704c30f56849f59fd78b490bcb93cb13386aa757 │
│ 60   │ overflow       │ 0ef5ec177aafb28e3f0e28f7b5fa5d2ffbe2cf38858d3a7ec4636ea1664b281e │
│ 61   │ overflow       │ 0a5c815ffe5216011f595370a3bd50b8fb950ab6e2b2e01ed9f3f01019fa69cf │
│ 62   │ overflow       │ 758650437fa2ea8bb875b8efd46eecf166acb1e4ae5b472d048513970a95fbed │
│ 63   │ overflow       │ ccfb4d0853cb3cf0610c9c4c9275be042c5365fcd77bd500c04272f6954f9902 │
│ 64   │ table-leaf     │ 732fee5afc87573193067e4ae0da1f7f9b0f9932f7f069369340d90be01fd692 │
│ 65   │ overflow       │ 2cc0ddd717bafbcb1c57098c3271106c1677deeb35a97018958ae2659b28163d │
│ 66   │ table-leaf     │ eb7d545f2ee9266beafe0d4d6d7cc58a8baab1bfe4710ea5eb536fc7e5b9a449 │
│ 67   │ overflow       │ 88fa136200d8b002867b73ebd94ae78e513d10d24f88ba8bfb302bf33ede5cc0 │
│ 68   │ table-leaf     │ b0ac4bd16e8ffa500762646b6dc7c6c7398fac999e487eed713851238fc86b9f │
│ 69   │ overflow       │ 88bbbe03f7e1b3f93fedce3262856d0369829891a78b2d4cb7a3c5b499f62a03 │
│ 70   │ table-leaf     │ dcba5028b293ab80f2d09bf42e1555968ac75b7f69d696542a69345daee29d2a │
│ 71   │ overflow       │ 9ad4b590ee93d7d431db66e5dcf4fccacd095d6ba3ae565c04f0627f0c170687 │
│ 72   │ table-leaf     │ 9fd9e4ebcc248c76a69dee2f7b87cb5e42af0fe6d3c947e5f71bea3719571d01 │
│ 73   │ overflow       │ ea4a139a0d21dc40233abe44fa65ed0438f486a962a527154f27bbaaefb09817 │
│ 74   │ overflow       │ 6c5289f3250687b7c3619713518d3dc6fbeac59eb981bd1e908ae5101180f5bb │
│ 75   │ overflow       │ a740c5597fb7ef676b27728a9a0828a3285a9a1bab20a5d9d2c98c341b69cc40 │
│ 76   │ table-leaf     │ e368ff348ef725f9309256c3b8cdb7f25e5ab6436c47875c29cf3f77729e6de6 │
│ 77   │ overflow       │ 8f58298ac0df2a6e4e47585ab26e7291ffc65a851133da66fe0f9ccc5c5a59fe │
│ 78   │ table-leaf     │ 0bbec48152ee56fd0cde848ee5b6c27955a27712457ad8b57422d6d26eb0080e │
│ 79   │ table-leaf     │ a9ad5b3bbee64f2b82c38c2317eaa98f36c814c104bbb75cfbf5801054474e7e │
│ 80   │ table-leaf     │ aa911e9e62211cdead52ffbc7cfeb4e3fa77a1644a998013b319806d672f6fc9 │
│ 81   │ table-leaf     │ 88b1c57ea604ea1108fda629f0ad7af846e0f6a5036bc77842e0bd88249af210 │
│ 82   │ overflow       │ c402483d0808de9a1a843fa2bf5a1dcc8dde050e6b0791e68b5052b01a4dc95a │
│ 83   │ table-leaf     │ 79064bddf27aafb5e2050cf8af0c74ce0756589248e8a77489f13d96239c8572 │
│ 84   │ overflow       │ 72c3d06469fa69d849fe5ea16c284b489a5ed550f6c18153f3b0124242b1b4ef │
│ 85   │ overflow       │ 808cf95246048d2300d190bd5bb9e7899137e1e02cc305c4b09bbb4b825e5985 │
│ 86   │ table-leaf     │ ec8b82a7792a9c53cb14f41f197e891bbddb5eee92987eea9bf8714ac9e87a74 │
│ 87   │ table-leaf     │ 727d286cec6c7242a80c5ffe1b7d531c5b0f687f8e66758d3f33cb3486e0f48e │
│ 88   │ index-leaf     │ f348efbbf9aa30ea9af8f21a5ef9fc491856d71102c39d798836bb130680a1af │
│ 89   │ index-leaf     │ 64e83d5312baf8a3c73f3efc2d7b2d05a46e3603223d638d908100729dd99df9 │
│ 90   │ overflow       │ 988c00b7986f0b06e4a047ddd647029196844bf91cfae3384884afd448d34420 │
│ 91   │ overflow       │ 03878852ef827430aa7b2447d43340d4a1dc07556969bb51d0eacc887c282ccc │
│ 92   │ overflow       │ c18d4b56b6862c94633cdb486b9471676cb2fecad2faecad206d4b6eb86dcbd9 │
│ 93   │ table-leaf     │ 09dc4cbd982af09308de11c5d05ce4a2808f6f215005b8ee6a2708e8142a25f9 │
│ 94   │ overflow       │ 0273b357f14b648d9335d42d8c346624456b8981c57425a6854e8fe840bdd9fd │
│ 95   │ table-leaf     │ 565bf12e4a91fdc96cf89a3b90c4961df44bcacc6804129ca842807df95b6e3d │
│ 96   │ overflow       │ fca46a535e78cbed584dca90532a9d4ada1a4401cb29d3457cccd332fbd9c443 │
│ 97   │ table-leaf     │ ef08059b194ea9a5dbc287ac6bcd3c82cd2b3a76b9e2d0cec9477e5ad684ecc9 │
│ 98   │ overflow       │ 2885bb2d9da559ed257254d403c2b0c4137363c6df0b953e2fbc0cadd9303bf7 │
│ 99   │ overflow       │ ad43a659e2a57a51c86f87beb182b018bbd3424daaef771ea2c8326ea871f972 │
│ 100  │ table-leaf     │ 42abd0d5c512933133653865ad518c8e6d12112bff9c31c913c075beb9afee8d │
│ 101  │ overflow       │ 55edb83f4053180be82e7661ccbe17d0b5bd9bc5022348ef2264a53c6b7d3d55 │
│ 102  │ overflow       │ df06845273d305ae660324a369356b54f727ecbe4fd9b94dc9bcda4b9b0c379d │
│ 103  │ overflow       │ 1435a614c08d5479b7d1bfd8a16abf1f1c0a4c3e451aecbc7920c97444b3d8c9 │
│ 104  │ overflow       │ 813cf888feaae9b51ac066fb71929159288a23d9a11b7d307c8a35d84b4a0cc9 │
│ 105  │ overflow       │ 632b547d5af4e4847c92c640ce8d45c68dc0dca75505a5624887b0ec3e20f94e │
│ 106  │ table-leaf     │ 848e089a6e152a2611c1e07ef3cb8b888de3c267f833e2e6f610d7a6eadffbc9 │
│ 107  │ overflow       │ 49cc6fe894ea6da5213523ec43a26facb16f7a3b040524ee5c13fe76f0c771cf │
│ 108  │ table-leaf     │ 8abeb3283e0b47b7db4b4c194094a5526e090003c6e0e35cdd9675a75c1c5aaf │
│ 109  │ overflow       │ 54f44a88e044984e44d373a03680dd3759789746fde5d08a4ffaac18a42e8d68 │
│ 110  │ overflow       │ e80b8c0b0947e336d3c819f690377af2f615b55fd9915f3a8ba7d22da59ab978 │
│ 111  │ overflow       │ d14e526d57b5bafa2a3e08fe01effedb64cf89e2dc10a5847f37bdfaca36437f │
│ 112  │ overflow       │ 95e50efa0e78ff80b87ad799328c2823e6e352c3e5e3379d9a80742ace967a7a │
│ 113  │ overflow       │ 97ea1d43a7c5e8fb0da485be88980a55490614f7f5b890aa2e2598c2167e33f8 │
│ 114  │ table-leaf     │ 5cdaa78217d2150a0bf2d3c80aa12024ea256da8cd33982a4bf059dacdb5b5f8 │
│ 115  │ overflow       │ 1d9cbda1a0e90bdc4c7c0a11c5e51090922b1ada96ac1936956bc37f176f939c │
│ 116  │ overflow       │ 2b48e052073c6bfc240028e5a4ceb16ddb3f0e04d0f53170f9edaa186da0cfd4 │
│ 117  │ overflow       │ 04234553cddc6fcd4069febd77b87f4d64811dd63bfaec0bfba6efc0c93b1562 │
│ 118  │ table-leaf     │ cc77e8c53bc44d3f9ec2e862a5e3c62f1384166a376fbc591b06f04a66b9a7b6 │
│ 119  │ overflow       │ 130806d3885b0211707f546e7e6c894bb09b4a0e556bba8ad80de6d114b3a68f │
│ 120  │ overflow       │ 4551c32df32ecf04c413ad17a59413b720421274c8314a6ad26f89fba4fc4def │
│ 121  │ overflow       │ 8863d6b8d94deaebb95bdd1a02f8743503e473e482e9b4ee9b34e98090e83f56 │
│ 122  │ table-leaf     │ 73b66a24dc411f00dcdbcceffe5f21778e0b90b603f7853b884dc0d97fd536de │
│ 123  │ overflow       │ 0794d43b88322f22e8250c0c79a9efcaaf459a54a8b07a071267dd33f16fae1e │
│ 124  │ table-leaf     │ 1430942e4bbd8d54d254e2dc5dac3db31f52193a489b0973ff1037284f04d910 │
│ 125  │ overflow       │ d283ee9250e0ce6430d788da2431cf9e5605c0053165ce862232569caba9f788 │
│ 126  │ table-leaf     │ 1b782ab4ae9d83cd884e84e88c97db9bcf2ae98850a12be281e3b3330932401f │
│ 127  │ table-leaf     │ b522ef1af3b0b8779c2ea43d3ff801a9d2f50d4830152b4c716df93d54a5c985 │
│ 128  │ overflow       │ 2ff02aeb9c1d60400994aa5d13455f7522be6b032eac801443989405352192c1 │
│ 129  │ overflow       │ ad5ebe9e004f84fa0135c9d3d1dccb41e23beee3df3c8fdfd5dac5358aa9fd56 │
│ 130  │ overflow       │ 2237eb84c5d68a348d8fa11409b505b67c8399eb94771a7da70f58f8c0037e1c │
│ 131  │ overflow       │ 04b162883872aba167b6982659fe122eacf674e99f171d08418be41fcd53fd21 │
│ 132  │ table-leaf     │ f7c462f63279bf6acd4d3b2b609190281a0e796e5a74fdb7536273d2c5ec6c3f │
│ 133  │ overflow       │ 3d1a288db1f0a6e69e64a1b5ce5b47f8d421da38b1eaa98f081c148de2583e30 │
│ 134  │ table-leaf     │ 39338cfa073abb3a5571e03b029f29139f8d10a58467c6f5c93de236ea3fe8b7 │
│ 135  │ overflow       │ b52bb1074cf3c25075249a0a9957dcc55a7a1ab67ea573e7d3e4f022dd7ac417 │
│ 136  │ table-leaf     │ 21322aaa8277e28109421c975ca1f1d6ceb296c687a0a53c1b1c2c49367b7cbe │
│ 137  │ overflow       │ ba5c54d41839876a1a145420a7edf0ee9cf0eea1f24d57b4bc797d9125b8d0ef │
│ 138  │ overflow       │ 4283a503f149a51b09e069a2b1bf7cf7dd0fd92783d71b1bee53914a7b838ed9 │
│ 139  │ table-leaf     │ b84214635122e91fb75100a6d935786fb002bedbc4535d7e2dfe9b7a507dc7d0 │
│ 140  │ overflow       │ 96e2cd05720136d0384e5ec79ec3a5157100e1d6e6980c4b189516fbd0f6a86c │
│ 141  │ overflow       │ 787db4dc093cfc8ad6ded6b7e82482a21b84572f2da80c7e5b5c8cc971d65da8 │
│ 142  │ overflow       │ 875c8b067fac66afcf8c9a958d1ae4897535fb08cd74aa90e935a22a8c7802f0 │
│ 143  │ overflow       │ 2da5c6392c5500f4e0bcfb7f6f01fb91b160d6ead868ae2d830664e758b21fbb │
│ 144  │ table-leaf     │ e173043bd290f8ded7aa15a968abac512a3fc0c5de3cf914475646d4f1bcd11e │
│ 145  │ overflow       │ 645f01d7974adc719c5abcea988f44a96c91232d85f6af6f26bb7427cf147cd9 │
│ 146  │ table-leaf     │ 232999b442843f030ab01ee02a9c1b529de85183ed1a3a956654b5bf0fa69adf │
│ 147  │ overflow       │ c6c2eaf7a8cf228898658b1095e5d8432e4bf1e6b97c207e3fb02a5364e03bd8 │
│ 148  │ overflow       │ 32d01b24c2c5d20dad20e84c5341a2c67389e85b2654f83adedc53dbabd72e35 │
│ 149  │ table-leaf     │ bfc28ec86893abb9bc5b44eda41bdba963744e659485d5a74e3bcde7a5f5158c │
│ 150  │ overflow       │ 1cf7cb226955104cf78d7e92d646b414ca576b7156535f9961543ff96c125e40 │
│ 151  │ overflow       │ 7f9e9b145d1335574de859e7f21f6f430939b9286de85c93c64a1ef35012ee33 │
│ 152  │ table-leaf     │ ae3c2642dacd1fc71aadd09f5933318a89c7a09ee4217060836ee8f8386c880e │
│ 153  │ table-leaf     │ d2e52f7dfaf1ee997c3966b0e8f94e357a8700f87923650c2c0250e735868b3f │
│ 154  │ overflow       │ 6baed52dc619ec51af512f078dc3a94e6df5d722bf2ad4f405eb99e80d6f581e │
│ 155  │ overflow       │ 6013788ab488ebedf636362170e6c90eca3fb7efed38c6e04f56e652ed2f9909 │
│ 156  │ table-leaf     │ eb71eba626a462ed55f68c03299f5e71201620fcbe3c4e98048f75708b93ffc7 │
│ 157  │ table-leaf     │ f1415e89843d7b50ec128f8be3965d85efa1a3e547c5a0dbe2c659e57274f152 │
│ 158  │ overflow       │ 939f128f7a957470606e7867f81d0bacbfc7cc8898778607fafdd1a3bec6a15c │
│ 159  │ overflow       │ d5b9772cff3af4d364f3651f4502207e61763c26c585b9fa8f995deca0a77ec8 │
│ 160  │ overflow       │ 70305b14df663528b9592d561c85c976f74d65571bb3acceb3d95ddb8cad78df │
│ 161  │ overflow       │ 8aed8e506b72467da1a95f823a863f2a7b9f3c92026ac0389044945606db8575 │
│ 162  │ table-leaf     │ b5c2bc6b4ca810c938c99c69af8e2e6697b4e080f313ae4d882c88ea99a6548d │
│ 163  │ table-leaf     │ 81d2edf79ac8eefa90c0f5f7ad6d7e753b6b53f675abe0431c63038d24a53c45 │
│ 164  │ overflow       │ 3a01461230c9643760562274b8fad32005a2351faf173a4dc7d76660a675c309 │
│ 165  │ overflow       │ 20fd3f79abcdef139324d0a308b7174da51e2ee9508fec383a1655ba3dd792e7 │
│ 166  │ table-leaf     │ 3605a1f6ed1917f4ea9b388d621cc9be26011ea8336bd5e0ad69220d0cd7d8db │
│ 167  │ index-leaf     │ ad8ae738c43075cb8c49c8b62ea69eb8fb6224d0deab4c53379c4f3ef0d7c803 │
│ 168  │ table-leaf     │ 900856304d8ad4d43446f715d96fc0be12b5fc344576ba4bc0980d1fb75971c9 │
│ 169  │ overflow       │ 3cd1484621a6930deda2fd63d2be32072c15bac0807933bf28f8f19162d457b8 │
│ 170  │ overflow       │ 7a2c6d554fbfc58aec2ceae45162d16225d83993e13aa57e1d3800af6ddc4d24 │
│ 171  │ table-leaf     │ 25bf59c0709b6edbf627c221ccc7338337a4a9abaf723f4de4752e4f28f09bf4 │
│ 172  │ overflow       │ c292b98a605b9531581d84d1412b1fedc4ba05cbfb5d7cf342864b4df789a0e1 │
│ 173  │ overflow       │ 81929ae1a5c151c988ddb036210aa3975d422058eb73872d780ea0814c9c2afa │
│ 174  │ overflow       │ c2a669aed711efcecde3ba1faa7b7a3c912a6705a92c767dabf0d32381962412 │
│ 175  │ table-leaf     │ 9e6a00f61e695edf8ce07ce8c69603a755e814108b84779e3f21fc6614d2b6e5 │
│ 176  │ overflow       │ 70a3c713eeb87a3b873bf5d032b21d679f9adb8a8c527f0af600017c843813a7 │
│ 177  │ overflow       │ 812040bb47ff3134003e4e1da04f70eec0522ccacf08fde842cd722e4571ade5 │
│ 178  │ overflow       │ 41770f11407c484eb54ca98bd72cb7f0f1328c5de8c0fcdf32f3045b6daa3d71 │
│ 179  │ overflow       │ 4d6e7156e5210c3c810638264c46f8d0ed428cd3698b9be38641df055351a929 │
│ 180  │ overflow       │ 36774019b141812d8bb2f1401a52c33efb19e3a32df6f39537c768214773476b │
│ 181  │ overflow       │ 3621cd7ebfbf961a3ae3bda905453238cd837d97f7f1dfabb8506f53fa2316e6 │
│ 182  │ overflow       │ b8edd5b7ed028935b68768b0accf7f5c6bface649d460ea7b3d79f2154c20803 │
│ 183  │ table-leaf     │ 5d01dd4623fd4a750ce3e5f87e0f440b80311491e94f6b8b6c9e572db5429957 │
│ 184  │ table-leaf     │ ea016b0904d23d14bb2fa62c036f7d8b2ab72727f2b60ba7b820d2b55d30b8ae │
│ 185  │ overflow       │ 147612121f23e6007d1cf95d651f89ac9cbc2031f70e53cb3b2e8a860c8ad9d0 │
│ 186  │ table-leaf     │ 3743d3e1ce7de8542889854bd07635e0db484b63b8fc4ba62018577d0d57a260 │
│ 187  │ overflow       │ 5cd072ba58d0bef1f3f3487c3d651153cdecb877e4b435878ca5fa44bd563ad4 │
│ 188  │ overflow       │ ac3e560ca6e7a95deb6051928ce624c2c780a3bcb2b7c22634f3e4be24c33cf9 │
│ 189  │ overflow       │ 977213a029c072749990cd83d118d8eb01a1fd45fcecf7e0807c58698fb5337c │
│ 190  │ table-leaf     │ 7c51f6f4da511db86b9b55b22893aff7e99dcd80cf7134ab9b92053303b88235 │
│ 191  │ overflow       │ 674df96ffea07223102545c9083939726e035966a8c29873da38c13379dac5a6 │
│ 192  │ overflow       │ a1dda57fb84ecd5b9bbeaa6fb5801ede1af7fb7b56fbc3772bc5936e43d0876b │
│ 193  │ overflow       │ 0bb9d64731e39b27c628079e03c1439d9c6e3d6ef32c28cb6459f12cfc8129dc │
│ 194  │ overflow       │ f2207c072673589a7c154fb192f515ea68a963a53d78cf49517b3b8539e0cadb │
│ 195  │ table-leaf     │ c7208e06bd98746438e93f1ae7fb45155b5baab1c270ed342964ea1fa38e0592 │
│ 196  │ overflow       │ 201d8f2bc00f73b32d62b428d247b8a56fcac19b1de078e75906e0bd9fd1e79e │
│ 197  │ overflow       │ 80dd673f81bdf47ebe2e8819123cfc693afc052fa64be1d494005e4aa57596c0 │
│ 198  │ overflow       │ 87d09f9c392d694fdda13788e46e84d6f03d733510d1537a1c8a7769d5fde29f │
│ 199  │ overflow       │ 48a4c890d6966899da4b367e2a5ece906be9ad32dc9711c1035038e55560fdfa │
│ 200  │ table-leaf     │ a9efcc50448876b41b2209921d713496d5ab111c48df306b000bc92b874ad353 │
│ 201  │ overflow       │ 9cf34393c30f1156740d2ed575c1abbf6841852cd281524a99923a1da88d67a6 │
│ 202  │ overflow       │ ede9cdcfc3289dfe634b55a17d6a868326d7c41929a5ccb5a5e3bf1036d65f3b │
│ 203  │ table-leaf     │ 08d351b2c10fc9dab3e14ba46777aca4ce0fc934ac421f6215b9eb679ceb3a11 │
│ 204  │ table-leaf     │ fa1a04e7ddf7118f43624a24a749e60975842ac6337474c9061bfb0e9fa44a90 │
│ 205  │ overflow       │ 4ed0ea698b6d12687455d50e0b543c7516d81164ae67867f959bdd7e40547e38 │
│ 206  │ table-leaf     │ 5cf13eaedd56113712759a8b058b8b3e636c1196e4f3ea5ccf7c5526beca43bb │
│ 207  │ overflow       │ e6b89650492f6d2f49b2ea0d73ae5b07713522937b244fdd30c374b37b3f78df │
│ 208  │ overflow       │ d3f153dbc7e7ce01491bd4dabd842617719dd562d961eccb5f00c91cea5772d7 │
│ 209  │ table-leaf     │ 1cb112ddea905435efa8ea4ca7d2570c4bb5a6fb94d80e8d68fb2fdb5d79ce5a │
│ 210  │ table-leaf     │ 897e54e68f312e2aa2edc958548d60456a558a5b0e456e52c5cacf4095d16580 │
│ 211  │ overflow       │ 3b3706aafeb0b2c1b995302e3001910c857f9d086c21045e8cd08767106ba1f5 │
│ 212  │ overflow       │ 6815b9e928bffcad27ff4a952ac5cdda584a0928c4341434fc7d92fe327c02be │
│ 213  │ table-leaf     │ f8487727c0cd0a88e92bb202e6c92db12f71eeed692e2ce76529831af744962e │
│ 214  │ overflow       │ 8dae397cea317c949b255ccd90f7ae29610ae219098fa5c429a1015a40dab220 │
│ 215  │ overflow       │ 24ebaa59a26af623e3767de407269b9aefa2577b72d98f503fac15d955917e19 │
│ 216  │ overflow       │ 44a8f5918fb867a2d919f3db0db8c443a754da0cda5efb998826024e3a36c05a │
│ 217  │ overflow       │ 40a96f5f5ddfd323bef78fcdaddebc3a13e8d39e3a1841c10d8b359ee177c9b6 │
│ 218  │ overflow       │ c6f12c1ed6acdfa1d3374c21ee425c7134ecbc01868633408e36609228e7dc3e │
│ 219  │ table-leaf     │ 97a711c0dc20de2e6a34ce51f61ebf0aedb29e3b1a268982526816115b85eedd │
│ 220  │ overflow       │ cdd31ed7c770fc1608d4ac32d291f30f867a28639b34488ac32ace07a95cf5c6 │
│ 221  │ overflow       │ 00bea5b536dbdbd201729ed609ed925463cfb4b87588d5e5a0df8c43b7cf6d33 │
│ 222  │ overflow       │ b414b6e822682648c2e8e29f84456cb700ee3a512e512309a1ca942fe6d2efae │
│ 223  │ table-leaf     │ 0a0961052a04adc43a4549f5538499dd109c982a7724f32cfb1b27e31b9e7b0e │
│ 224  │ overflow       │ fa7fb2c4f7c22de155dad918fe70990861571ff387933cc7b3adcf00c90d079d │
│ 225  │ overflow       │ 115afee8ef26a376070491221cd33f49840a49a67d480c54c3133653c7227629 │
│ 226  │ overflow       │ 9754ccc3bbb0a6ab9c7802b69d32a178faee6b9ed6b2d02003180b56dd08745d │
│ 227  │ overflow       │ 6c29865d25e5796669f89d13e6478c2795f5d044eac2ee58f57d1fb1abdabe50 │
│ 228  │ overflow       │ 3bff5c2e1be3bc6b9ae72cf82c7570ef6075a68c7ce06e923e3f723b5b102404 │
│ 229  │ table-leaf     │ 8782ee7b6fce201a6c8b70a0b505889a6989265cee17eb319a86e670fd0998ce │
│ 230  │ freelist-trunk │ c87a78226061b340db326b28249cf4d70558f57ee712591a70210e847f8e3f7e │
│ 231  │ freelist-leaf  │ ad7facb2586fc6e966c004d7d1d16b024f5805ff7cb47c7a85dabd8b48892ca7 │
│ 232  │ freelist-leaf  │ ad7facb2586fc6e966c004d7d1d16b024f5805ff7cb47c7a85dabd8b48892ca7 │
│ 233  │ overflow       │ bd0dba7fcb0d77b4ec4a17e044f099ab20b87d8d32a12b865785bbe5f8cfcf06 │
│ 234  │ overflow       │ 6cfbfc530bb4df138c6e21665fd52937a439052067246c57db1c8b558860c1c2 │
│ 235  │ table-leaf     │ a48fe108915400cc05afc9b34ca6feb46a9b040b49afd633f45cef2152c59216 │
│ 236  │ overflow       │ e568253890d7a9695f10ef88b5ffe111c5ffc441499e9245f15c8191a84b9de4 │
│ 237  │ overflow       │ 0ec08eae5b32429eac790f8246de897a4535b2d9f60a51ae533f596c50281f9f │
│ 238  │ table-leaf     │ c24df11ef4497f2d0672276394c9d3f67eebe3b9d9584d27da51a6d7b9713fa1 │
│ 239  │ overflow       │ 80f767d0d279a9e7d99c8982e6f4dd09d972b430a80dada3f777025187fb977f │
│ 240  │ overflow       │ aa93bed9b60983a91008131c9a10c81584ae4965359b434bfcf95ef460a22a70 │
│ 241  │ overflow       │ 9359ca48c0108d0e5c44e6ab26a928326f078a46d1efcf0f799fc01d9b0ec7fc │
│ 242  │ index-leaf     │ a1477703add342d21b8c339556803b964e8d9e3e478c63b8bbbd5430120c282b │
│ 243  │ table-leaf     │ c0f6b1c2094fb08df1735df4ae0aec63b28c579867f4c337420bb1fbb3fce722 │
│ 244  │ overflow       │ fb6a6b25d3fefc28db75adeffb88930587588f87221426e86e10867874da92bf │
│ 245  │ table-leaf     │ 00bc00cd7e93f90137bff6e90a154914152dc9299b0821c99899ee7c00fa0aba │
│ 246  │ overflow       │ 570986ec39ec913b852019c4a2695a6303f94dbf9d479eb0b23cfe9dc6e8afee │
│ 247  │ table-leaf     │ 24293084e423ced4143675d4f2e02c18fe1d5c0f40acd80d672b219f66ffac8d │
│ 248  │ overflow       │ 29b63daaa8287b7720f690ce5c4bda7efa76da8f40b58956034d9da23cc62056 │
│ 249  │ table-leaf     │ 0f413743b990730eacfb64df4386633394ebd9790075700338c583b2d10403b1 │
│ 250  │ overflow       │ 7184accfa8fe098c38a16bc9030f61dbbb74c852d0ee7087277a5736c27aed6d │
│ 251  │ overflow       │ ceede5817f74dadd8a0691f264861423282366ae054c47dfa982cdbb14781d5d │
│ 252  │ overflow       │ 1f2073e0d2e776181321daadb6669a0e197ef9ca6d103bcf94e84a758cc9beaa │
│ 253  │ overflow       │ 427e0ce90b40a2bcfba0322802f4550898b3b8ed9d7076b3c7be944e87124f8b │
│ 254  │ table-leaf     │ 1c9f1fcfef357551296e963e475d138653a8e0acb975f5c75cf953d87cd4cbd9 │
│ 255  │ overflow       │ 8acaa7d09e431d93d9a6154f7fd7d089885c2b259aa5958ede44f9070f1eaf29 │
│ 256  │ overflow       │ a77956d277cfc9febddb4904fa3c335d0599fc09ef1221bbdbcb312dcaee733d │
│ 257  │ overflow       │ 3b9c2faae264b478d329daf0d3300571271de0c20043a2ae441d0b070eedfa0f │
│ 258  │ table-leaf     │ 9bb2466c861ded02acd4106470c4bc8b1b465fa8b476033e3e2eb3465133ad00 │
│ 259  │ table-leaf     │ 1b13dc75619b6e2f5f0a9ee32a619f95e9030874fbf5376433c7b480de3e2f3b │
│ 260  │ table-leaf     │ e778d1107f06dded66181037839c530ccde3b75a9e0d17c4339140f569dd241c │
│ 261  │ table-leaf     │ 7323ace1550498a42c3320cdde9df3efdd1d0ec312a012e55f0ec7346adeb07d │
│ 262  │ overflow       │ 487a9bc24226973a0283e4d3cbb91d73503681f3a983b2c24082cae7bae37f16 │
│ 263  │ overflow       │ a99d77b547c0e68761b258014ac7c528460b0411627d3ae501bcdd69496959af │
│ 264  │ table-leaf     │ ca0bcc06007923846a5f28f669b60dc09681826ea74591f19bf6456d22310879 │
│ 265  │ overflow       │ 89ee12580173f6765466613a75b5318cec32f679c5e4b09e43f1f56f3d032003 │
│ 266  │ overflow       │ 0866cbbace434723e8da1512f24f6eb663235646c61412e2f88dea322743c892 │
│ 267  │ overflow       │ e6ae8ae984a89238c365a8138d93b3b3dd0b2919b12856794f35652852852350 │
│ 268  │ overflow       │ 07b5f98d5d313031a249f66f23e496a6bbec79da3013c3fb4bdfad5a7bc12ee0 │
│ 269  │ table-leaf     │ b1a76d96595574cbf5e5445e90da4b831ffbee44931da0ad942e0cd6cd825db1 │
│ 270  │ overflow       │ 991abeed9b2f89b4b29abcc92073e6dfb59497192f0b33ff7dcc4da79f80065d │
└──────┴────────────────┴──────────────────────────────────────────────────────────────────┘

── B. Rebuild from the capture alone and prove it equal (E1) ───────────

B1. The rebuild: pages written in order, its sha256, what the recipe lookup
says it is, and the integrity_check of SQLite.
┌─────────────────────────┬───────────────┬───────────────┬───────────────────────┬─────────────────┐
│         rebuilt         │ from_capture  │ pages_written │     recipe_lookup     │ integrity_check │
├─────────────────────────┼───────────────┼───────────────┼───────────────────────┼─────────────────┤
│ rebuilt/from-capture.db │ run-1/capture │ 270           │ step 30 (exact bytes) │ ok              │
└─────────────────────────┴───────────────┴───────────────┴───────────────────────┴─────────────────┘

B2. The rebuild against the original, and the manifest after step 29
against the manifest after step 30. They differ first at page 1: the header
changes on every recipe step.
┌─────────────────────┬───────┬──────────────────────┬─────────────────┐
│        name         │ equal │ first_differing_page │ differing_pages │
├─────────────────────┼───────┼──────────────────────┼─────────────────┤
│ original vs rebuilt │ true  │                      │ 0               │
│ step 29 vs step 30  │ false │ 1                    │ 30              │
└─────────────────────┴───────┴──────────────────────┴─────────────────┘

The words of the library for each comparison:
┌───────────────────────────────────────────────────────────────────────────────────────────┐
│                                          result                                           │
├───────────────────────────────────────────────────────────────────────────────────────────┤
│ E1 bytes: equal (sha256 24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74) │
│ manifests differ: first differing page 1 (page image differs), 30 page(s) differ          │
└───────────────────────────────────────────────────────────────────────────────────────────┘

The sha256 of the rebuild, which is also the sha256 of the original:
┌──────────────────────────────────────────────────────────────────┐
│                              sha256                              │
├──────────────────────────────────────────────────────────────────┤
│ 24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74 │
└──────────────────────────────────────────────────────────────────┘

── C. A damaged capture is refused, and the refusal names the page ─────

Each row is a copy of the capture with one thing changed. The rebuild must
refuse it with a message that contains the text in wanted, and must not write
an output file.
┌─────────┬──────────────────────────────────────────┬────────────────────────┬────────────────┐
│ case_no │                  damage                  │         wanted         │ output_written │
├─────────┼──────────────────────────────────────────┼────────────────────────┼────────────────┤
│ C1      │ drop the manifest entry for page 100     │ page 100: missing      │ false          │
│ C2      │ list page 50 twice (in place of page 51) │ page 50: listed twice  │ false          │
│ C3      │ swap the entries for pages 120 and 121   │ page 121: out of order │ false          │
│ C4      │ flip one byte in the image of page 200   │ capture page 200:      │ false          │
│ C5      │ swap the images of pages 7 and 8         │ capture page 7:        │ false          │
└─────────┴──────────────────────────────────────────┴────────────────────────┴────────────────┘

The refusals:
┌─────────┬───────────────────────────────────────────────────────────────────────────┐
│ case_no │                                  refusal                                  │
├─────────┼───────────────────────────────────────────────────────────────────────────┤
│ C1      │ manifest entry 100: page 100: missing (entry 100 lists page 101)          │
│ C2      │ manifest entry 51: page 50: listed twice (entries 50 and 51)              │
│ C3      │ manifest entry 120: page 121: out of order (entry 120 should be page 120) │
│ C4      │ capture page 200: image sha256 does not match the manifest                │
│ C5      │ capture page 7: image sha256 does not match the manifest                  │
└─────────┴───────────────────────────────────────────────────────────────────────────┘

── D. A database that is not at rest is refused ────────────────────────

Its file alone is stale or torn. Each scenario starts a child process that
leaves the database not at rest and exits with status 3 without closing
its connection.

D1. WAL mode: the child commits one UPDATE (account 1 gains 1000). The
commit lives in the -wal, so the database file alone is stale.
┌─────┬─────────────────────────────────────────────────────┬────────────────────────────────────────────────────────────────────────┐
│ seq │                     observation                     │                                 value                                  │
├─────┼─────────────────────────────────────────────────────┼────────────────────────────────────────────────────────────────────────┤
│ 1   │ child exit status                                   │ 3                                                                      │
│ 2   │ side files after the child                          │ -wal 4152 bytes, -shm 32768 bytes                                      │
│ 3   │ capture                                             │ database is not at rest: r.db: -wal holds 4152 bytes of frames not yet │
│     │                                                     │  checkpointed into the database file                                   │
│ 4   │ database file alone: sha256 unchanged by the commit │ true                                                                   │
│ 5   │ database file alone: account 1 balance              │ 43047                                                                  │
│ 6   │ SQLite with the -wal: account 1 balance             │ 44047                                                                  │
│ 7   │ side files after the last close                     │ no side files                                                          │
│ 8   │ capture after the checkpoint: pages                 │ 60                                                                     │
│ 9   │ capture after the checkpoint: manifest sha256       │ 2a17d23aa48e660bf7431827e6a0aa391794544f201166e0cd729508dc4431fc       │
└─────┴─────────────────────────────────────────────────────┴────────────────────────────────────────────────────────────────────────┘

D2. Rollback mode: the child sets cache_size=10, updates every document body
in one transaction, which spills dirty pages to the file, and exits before
COMMIT. The file alone holds part of an uncommitted transaction.
┌─────┬────────────────────────────────────────────────────────┬────────────────────────────────────────────────────────────────────────┐
│ seq │                      observation                       │                                 value                                  │
├─────┼────────────────────────────────────────────────────────┼────────────────────────────────────────────────────────────────────────┤
│ 1   │ child exit status                                      │ 3                                                                      │
│ 2   │ side files after the child                             │ -journal 203312 bytes                                                  │
│ 3   │ capture                                                │ database is not at rest: r.db: hot -journal (203312 bytes with a live  │
│     │                                                        │ header): a transaction was interrupted and the next open will roll it  │
│     │                                                        │ back                                                                   │
│ 4   │ database file alone: changed by the spill              │ true                                                                   │
│ 5   │ database file alone: documents                         │ 17 rows, 15 bodies all zero                                            │
│ 6   │ SQLite with the hot journal (rolls it back): documents │ 17 rows, 0 bodies all zero                                             │
│ 7   │ side files after the last close                        │ no side files                                                          │
│ 8   │ capture after the rollback: pages                      │ 60                                                                     │
└─────┴────────────────────────────────────────────────────────┴────────────────────────────────────────────────────────────────────────┘

── Checks ─────────────────────────────────────────────────────────────

Every claim the experiment asserted while it ran:

A  ✓  two runs of the recipe give manifests identical line for line
B  ✓  sha256 of the rebuild equals sha256 of the original
B  ✓  the rebuild is the recipe's state after step 30, byte for byte
B  ✓  SQLite opens the rebuild and integrity_check returns ok
B  ✓  opening the rebuild with SQLite and closing it changed nothing and left no side files
B  ✓  the manifest comparison names the first differing page (page 1: the header changes on every recipe step)
C  ✓  C1: the rebuild is refused naming "page 100: missing", and no output file is written
C  ✓  C2: the rebuild is refused naming "page 50: listed twice", and no output file is written
C  ✓  C3: the rebuild is refused naming "page 121: out of order", and no output file is written
C  ✓  C4: the rebuild is refused naming "capture page 200:", and no output file is written
C  ✓  C5: the rebuild is refused naming "capture page 7:", and no output file is written
D  ✓  D1: the capture is refused: the -wal holds committed frames
D  ✓  D1: a page capture taken from the file alone would have lost the committed UPDATE
D  ✓  D1: once at rest, the capture includes the UPDATE (the file changed at the checkpoint)
D  ✓  D2: the capture is refused: a hot -journal exists
D  ✓  D2: the file alone holds part of an uncommitted transaction; a capture of it would be torn
D  ✓  D2: after SQLite rolled the hot journal back, the file is byte-identical to its state before the transaction

17 of 17 claims hold

Experiment verdict: MANIFEST OK
```

#### Reading the evidence

- **Section A (Part A: deterministic capture and full manifest).** Two independent builds of R0 (`run-1/r0.db` and `run-2/r0.db`) each capture 270 pages (`pages.bin` = `1,105,920` bytes = `270 * 4096`) and produce the exact same manifest digest `8d9ad6e1251dfbbe…f523`. In table A5, freelist leaves 231 and 232 share the identical SHA-256 `ad7facb2586fc6e9…8892ca7`—the hash of 4,096 zero bytes—because the recipe enables `secure_delete=ON`, causing `freePage2` to zero freed pages.
- **Section B (Part B: verified E1 rebuild and step diff).** `capture.Rebuild` writes `rebuilt/from-capture.db` from `run-1/capture` alone. `equal.CompareFiles` reports `E1 bytes: equal (sha256 24aa158fdf713808…)`, `recipe.MatchStep` (which matches a file's SHA-256 against the cached R0 step hashes) identifies it as `step 30 (exact bytes)`, `PRAGMA integrity_check` returns `ok`, and opening and closing the rebuild leaves its SHA-256 unchanged with no sidecar files. Comparing the manifests of step 29 and step 30 pinpoints `first differing page 1 (page image differs), 30 page(s) differ`.
- **Section C (Part C: five tampered captures) and Section D (Part D: not-at-rest refusals).** Analyzed in Sections 6.2 and 6.3 below.

---

## 6. Evidence and failure cases

A page inspector and a page-capture tool sit below SQLite on raw bytes. Every failure mode tested in Experiments 2.2 and 2.3 marks an exact boundary of what the tools must refuse. (Parenthetical codes such as `COR-01` and `HDR-03` refer to tests in the chapter's adversarial suite summarized under "What attacked this chapter" below.)

### 6.1 Nine malformed database structures (Experiment 2.2 part F)

Starting from a 6-page rollback-mode database (`1=table-leaf`, `2=table-leaf`, `3=overflow`, `4=overflow`, `5=freelist-trunk`, `6=freelist-leaf`), Experiment 2.2 part F damages one structure per copy and compares `internal/dbfile` against SQLite's `PRAGMA integrity_check`:

| Case | Damage injected | `internal/dbfile` verdict | SQLite verdict |
| --- | --- | --- | --- |
| **F1** | Byte 0 changed (`'S'` → `'X'`) | `not an SQLite database file: file offset 0` | `file is not a database (26)` |
| **F2** | Last page (freelist leaf 6) cut in half (`22,528` B) | `file is truncated: file offset 22528: file size 22528 is not a multiple of the page size 4096` | `integrity_check: ok` |
| **F3** | Page 2 type byte `0x0d` → `0x07` | `page 2 offset 0: page type byte 0x07 is not 0x02, 0x05, 0x0a or 0x0d` | `database disk image is malformed (11)` |
| **F4** | Page 2 cell count `2` → `3000` | `page 2 offset 3: 3000 cells do not fit: the cell pointer array would end at 6008, usable size 4096` | `database disk image is malformed (11)` |
| **F5** | Page 2 first cell pointer → `9` | `page 2 offset 8: cell 0 pointer 9 is outside the cell content area 2260..4092` | `Offset 9 out of range 2260..4092` (+ unused pages 3, 4) |
| **F6** | Row 1 payload-size varint → `0x7f` | `tree "t" (root 2): ... page 2 offset 7: 1699 bytes of the cell content area belong to no cell or freeblock, the header reports 0 fragmented bytes` | `Rowid 21 out of order` + `Fragmentation of 1699 bytes reported as 0 on page 2` + `Page 3: never used` |
| **F7** | Overflow page 3 next pointer → `3` (self-loop) | `page 3: overflow chain of cell 0 loops back to page 3` | `2nd reference to page 3` + `Page 4: never used` |
| **F8** | Header offset 32 (freelist trunk) → `2` (table page) | `page 2: page claimed twice: as table-leaf of "t" and as freelist-trunk` | `freelist leaf count too big on page 2` + `2nd reference to page 2` |
| **F9** | Header offset 36 (freelist count) `+ 1` (`2` → `3`) | `file offset 36: header counts 3 freelist pages, the trunk chain holds 2` | `Freelist: size is 2 but should be 3` |

Two cases clarify deliberate design boundaries:

- **F2 (partial last page — stricter than SQLite on purpose).** SQLite rounds a 22,528-byte file up to 6 whole pages, accepts the short read in `readDbPage` ([`src/pager.c:3088-3090`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3088-L3090)), and zero-fills the missing 2,048 bytes. Because page 6 is an unread freelist leaf, `integrity_check` returns `ok`. `internal/dbfile` returns `ErrTruncated` instead, because a page-level capture of a partial page could not reconstruct the file.
- **F6 (what `ParseBTreePage` checks versus what remains out of scope).** Shrinking row 1's payload-size varint to `0x7f` shortens cell 0 on page 2, leaving 1,699 bytes of the cell content area uncovered while header byte 7 reports `0`. `ParseBTreePage`'s `checkCoverage()` catches that exact `1699` vs `0` fragmentation error on page 2—matching SQLite's own `Fragmentation of 1699 bytes reported as 0 on page 2`—alongside cell-to-cell and cell-to-freeblock byte overlaps and freeblock spacing rules. `internal/dbfile` still does not verify key ordering across cells or pointer-map entry contents; those belong to `PRAGMA integrity_check` (Chapters 3 and 9).
- **No-panic guarantee.** Beyond F1–F9, [`internal/dbfile/dbfile_test.go`](../internal/dbfile/dbfile_test.go) runs 3,000 single-bit flips on a 6-step recipe file (319 rejected; the rest land inside opaque payload data bytes) plus 8 truncations and the `FuzzParse` fuzz target. The adversarial suite below adds 656 exhaustive header mutations (`COR-01`) and 10,000 seeded mutations (`COR-06`), all with zero panics.

### 6.2 Five tampered rung R1 captures (Experiment 2.3 part C)

Every tampered copy of the 270-page capture is refused by `capture.Rebuild`. Each check asserts that the refusal names the offending page substring and writes no output file:

- **C1 (drop manifest entry for page 100):** `manifest entry 100: page 100: missing (entry 100 lists page 101)` (checks `"page 100: missing"`).
- **C2 (list page 50 twice in place of 51):** `manifest entry 51: page 50: listed twice (entries 50 and 51)` (checks `"page 50: listed twice"`).
- **C3 (swap manifest entries 120 and 121):** `manifest entry 120: page 121: out of order (entry 120 should be page 120)` (checks `"page 121: out of order"`).
- **C4 (flip one byte inside page 200 of `pages.bin`):** `capture page 200: image sha256 does not match the manifest` (checks `"capture page 200:"`).
- **C5 (swap the images of pages 7 and 8 in `pages.bin`):** `capture page 7: image sha256 does not match the manifest` (checks `"capture page 7:"`).

In addition, `capture.Rebuild` refuses a 0-page manifest and runs `dbfile`'s full page census on the rebuilt temp file (`checkRebuilt`) before renaming it into place, rejecting coordinated truncation (where both `manifest.txt` and `pages.bin` drop trailing pages) and tampered page roles in `manifest.txt`.

### 6.3 Databases not at rest (Experiment 2.3 part D)

`capture.CheckAtRest` defines "not at rest" as any condition where `.db` alone does not hold the complete committed state: a `-wal` file with size `> 0`, or a hot `-journal` file (size `> 0` and first header byte non-zero, matching SQLite's `hasHotJournal` at [`src/pager.c:5180`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5180)). A zero-byte `-wal` (left after `wal_checkpoint(TRUNCATE)` or under `PERSIST_WAL`, even while `-shm` remains or an open connection holds an uncommitted write transaction) and a zero-byte or zero-header `-journal` (left by `TRUNCATE` or `PERSIST` journal modes) count as at rest because every committed frame is already in `.db`. If a writer commits mid-capture, `-wal` grows and the post-capture check in `capture.Pages` fails and removes the temporary capture files.

- **D1 (WAL mode with uncheckpointed frames).** A child process commits `UPDATE accounts SET balance = balance + 1000 WHERE id = 1` on a 6-step WAL database and exits with status `3` without closing, leaving `-wal` (4,152 bytes) and `-shm` (32,768 bytes). `capture.Pages` refuses with `-wal holds 4152 bytes of frames not yet checkpointed into the database file`. Copying `.db` alone leaves its SHA-256 unchanged and account 1's balance stale (`43047`). Opening the database with its `-wal` returns the committed balance (`44047`), checkpoints on close, removes the sidecar files, and lets `capture.Pages` succeed (60 pages).
- **D2 (rollback mode with a hot journal).** An interrupted rollback-mode transaction creates a hot journal only after SQLite syncs the journal header—either at `COMMIT` or when dirty pages spill from the page cache ([`src/pager.c:1494-1501`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1494-L1501) writes the initial header with zeroed magic; [`src/pager.c:4377`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4377) writes the live magic at sync). Part D2 forces a mid-transaction spill using `cache_size=10` while updating every document body, then exits before `COMMIT`, leaving a 203,312-byte hot `-journal`. `capture.Pages` refuses it. Copying `.db` alone yields a torn state (`17 rows, 15 bodies all zero`) that no transaction ever committed. Opening the database with SQLite rolls the hot journal back (`17 rows, 0 bodies all zero`), removes `-journal`, restores the exact pre-transaction SHA-256, and lets capture succeed.

### What attacked this chapter

<!-- adversarial-results -->

Four standalone Go stress-test programs (`36` attacks total, driven by [`run-all.sh`](../agent_work/ch02/adversarial/run-all.sh)) tried to break this chapter's code and claims against the pinned SQLite `3.53.4` build (`HELD` means the code or claim stood up to the test; `BROKEN` means the test exposed a bug or an overly broad claim):

- [`header-falsify`](../agent_work/ch02/adversarial/header-falsify/main.go) (`HDR-01`–`HDR-10`) tested Experiment 2.1 and `internal/dbfile/header.go` across all 8 page sizes, file truncations, WAL cache eviction (`PRAGMA shrink_memory` and `cache_size=2`), checkpointed WAL commits, rollback no-op and rolled-back transactions, negative signed header fields, `0xffffffff` counter wraparound, mismatched format version bytes (`write=1, read=2`), mutated `sqlite/PIN` (the repository's SQLite version pin file), and a wrong-version `$SQLITE3_BIN`.
- [`census-edge`](../agent_work/ch02/adversarial/census-edge/main.go) (`CEN-01`–`CEN-08`) tested Experiment 2.2 and `internal/dbfile` with 512-byte multi-trunk freelists at reader capacity (`126`/`127`), local/overflow boundaries across 512/4096/65536-byte pages and index interior pages, `WITHOUT ROWID` tables, multi-page overflowing `sqlite_schema`, non-zero reserved bytes, a sparse > 1 GiB 4 KiB-page file with a live overflow page past the lock-byte page, all 8 page sizes, and UTF-16le/be schemas with emoji, `STRICT` tables, and `ALTER TABLE ADD COLUMN`.
- [`corrupt-fuzz`](../agent_work/ch02/adversarial/corrupt-fuzz/main.go) (`COR-01`–`COR-06`) tested `internal/dbfile`'s no-panic and corruption-rejection guarantees with 656 exhaustive single-byte header mutations, page-header corruptions, overflow pointer corruptions (including the last overflow page's `next` pointer), 14 structural tree/freelist/pointer-map corruptions, anti-OOM huge varints, duplicate overlapping cell pointers, and a 10,000-iteration seeded mutation storm.
- [`manifest-capture`](../agent_work/ch02/adversarial/manifest-capture/main.go) (`CAP-01`–`CAP-08`) tested Experiment 2.3, `internal/capture`, and `internal/equal` with 13 capture tampers, coordinated last-page truncation, tampered manifest roles, 0-page manifests, 8 not-at-rest and stale-sidecar conditions, live open WAL connections after `wal_checkpoint(TRUNCATE)`, mid-capture concurrent WAL writes, cross-config E1 round-trips, `WITHOUT ROWID` captures, and manifest parsing edge cases.

**Run 1** ([`results-run1.txt`](../agent_work/ch02/adversarial/results-run1.txt)) produced **22 HELD, 14 BROKEN**. Eleven broken attacks exposed bugs in the Go packages and were fixed:

- `HDR-08`: `Header.WALMode()` was updated to check byte 19 alone (`ReadVersion == 2`), matching `lockBtree` ([`src/btree.c:3341`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3341)).
- `HDR-10`: `sqlitex.LocateCLI` added `ErrNoCLI` so Experiment 2.1 skips `.dbinfo` only when no CLI exists anywhere and fails (exit 1) when `$SQLITE3_BIN` names a missing path or wrong-pin binary.
- `CEN-03`, `CAP-07`: `census.go` was updated to accept `WITHOUT ROWID` tables (index B-tree roots for `type = 'table'` rows), refusing only an `index` entry with a table root.
- `COR-02`, `COR-05b`: `ParseBTreePage` added the 4-byte freeblock gap check, total free-space bound, and `checkCoverage()` (rejecting overlapping cells or freeblocks and mismatched fragmented-byte counts).
- `COR-03b`: `OverflowChain` was updated to reject a non-zero `next` pointer on the last overflow page.
- `CAP-02a`, `CAP-02b`, `CAP-02c`: `capture.Rebuild` was updated to reject 0-page manifests and run `checkRebuilt` (full page census plus manifest comparison) before renaming into place.
- `CAP-05`: `capture.Pages` was updated to remove prior capture files, write `.capture-tmp` files, and rename only after the final stability checks pass, leaving an empty directory on mid-capture failure.

After those fixes, **Run 2** ([`results-run2.txt`](../agent_work/ch02/adversarial/results-run2.txt)) produced **33 HELD, 3 BROKEN**. Those three failures exposed overly broad claims or oracle discrepancies rather than code defects:

- `HDR-03` disproved the broad "one connection ⇒ constant WAL change counter" claim: `PRAGMA shrink_memory` evicted page 1 on one connection and advanced the counter on every commit (`9 -> 10 -> 11`). Experiment 2.1 Part C4 demonstrates the exact cache rule (`dbFileVers + 1`, refreshed whenever `readDbPage` reads page 1).
- `HDR-06` exposed an oracle disagreement on negative `user_version` and `application_id` values: `PRAGMA` and `internal/dbfile` decode signed `int32` (`-1`), while CLI `.dbinfo` prints every 4-byte field unsigned with `%u` (`4294967295`, [`src/shell.c.in:5561-5562`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/shell.c.in#L5561-L5562)).
- `CAP-04` tested capturing a WAL database whose `-wal` was `0` bytes (`-shm` `32,768` bytes) while a live connection held an uncommitted `+99` write transaction after `wal_checkpoint(TRUNCATE)`. Under the at-rest rule in Section 6.3—completeness of `.db` alone, without probing `-shm` locks—`capture.Pages` accepts a 0-byte `-wal` and rebuilds the committed state (`integrity_check = ok`, balance `43048`, uncommitted `+99` excluded, E1-equal after rollback).

Once `HDR-03`, `HDR-06`, and `CAP-04` were updated to check the verified behavior, the final run of [`run-all.sh`](../agent_work/ch02/adversarial/run-all.sh) passed **36 HELD, 0 BROKEN** ([`results.txt`](../agent_work/ch02/adversarial/results.txt)).

---

## 7. Platform implications

Every operational rule below follows directly from this chapter's experiments:

1. **The page is the unit of storage, replication, and verification (Experiments 2.1 C, 2.2 A, 2.3 A).** R0 is 270 pages of 4,096 bytes (`1,105,920` bytes); every WAL commit appends whole 4,096-byte pages wrapped in 24-byte frame headers (`+4,120` bytes per modified page in 2.1 C2); rung R1 captures and hashes one page per manifest line.
2. **Choose page size once at creation (Experiment 2.1 F, `lockBtree`).** Page size is recorded in header bytes 16–17 (`65536` stored as `1`) and fixed when page 1 is initialized; changing it later requires rebuilding the database (`VACUUM`).
3. **Never use header bytes 24–27 as a change detector in WAL mode (Experiment 2.1 B, C1–C4).** Bytes 24–27 advance on every commit in rollback mode only. In WAL mode, commits do not touch `.db` until checkpoint, and commits that skip page 1 leave all 100 header bytes unchanged even after checkpoint (C2). Commits that dirty page 1 write `dbFileVers + 1`, holding steady on one connection while page 1 stays cached (C1) and advancing only when page 1 is re-read—on a new connection (C3) or after `PRAGMA shrink_memory` (C4). Detect WAL changes with `PRAGMA data_version` per connection or by watching `-wal`, never header bytes 24–27.
4. **E1 byte hashes are stable only within one SQLite library version (Experiment 2.1 A).** Header bytes 96–99 store the last writer's `SQLITE_VERSION_NUMBER` (`3053004`). Upgrading SQLite changes those four bytes on the next write; cross-version verification requires equality levels E2 (page comparison ignoring volatile header bytes) or E3 (logical content hashing), introduced in Chapter 3.
5. **Never copy or page-capture a database that is not at rest (Experiment 2.3 D1–D2).** Copying a WAL database without its non-empty `-wal` silently drops committed writes (`43047` vs `44047`); copying a rollback database beside a hot `-journal` captures a torn uncommitted spill (`15` of `17` bodies zeroed). In WAL mode, take an R1 capture only after a `TRUNCATE` checkpoint or after every connection closes; otherwise take a live snapshot inside a read transaction (rung R4, Chapter 6).
6. **Do not inspect closed WAL databases with `mode=ro` if they must stay clean on disk (Experiment 2.1 A).** Opening a WAL-mode database read-only and closing it leaves `-wal` and `-shm` sidecar files behind because a read-only connection cannot run the close checkpoint that removes them. Either inspect closed WAL files with a read-only byte parser (`internal/dbfile`) or open a normal read-write connection when checking pragmas.
7. **Never call `os.Open` and `Close` on a database file inside a process that holds an SQLite connection to it (Experiment 2.1 child reader, Experiment 2.2).** On POSIX systems, closing any file descriptor for a file drops every POSIX byte-range lock that process holds on that file. Read raw bytes from a closed database before opening any SQLite connection (as Experiment 2.2 does via `os.ReadFile` + `FromBytes`, or explicit `Close` in Part E) or from a separate child process (as Experiment 2.1 does).
8. **Page accounting reveals where space actually goes and prevents false lost-page alarms (Experiment 2.2 A, D, E).** In R0, 170 of 270 pages (63%) are overflow pages of `documents`—invisible if a service only counts tables or rows. Services that grow files past 1 GiB must account for the single unused lock-byte page (`1073741824 / pageSize + 1`), and databases with `auto_vacuum` enabled must account for periodic pointer-map pages.
9. **Enable `secure_delete=ON` if freed pages must not leak stale bytes (Experiment 2.3 A, `freePage2`).** With `secure_delete=ON`, freelist leaves are zeroed on free (pages 231 and 232 both hash to `ad7facb2…`), preventing deleted customer data or history-dependent bytes from surviving in page captures.

| Operational rule | Supporting evidence |
| --- | --- |
| Size storage, WAL shipping, and verification in whole pages | Experiment 2.1 C2 (`+4,120` B/frame); Experiment 2.2 A; Experiment 2.3 A (`270 * 4,096`) |
| Fix `page_size` at database creation (`65536` stored as `1`) | Experiment 2.1 F; `lockBtree` ([`src/btree.c:3371-3404`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3371-L3404)) |
| Never use header bytes 24–27 to detect WAL commits | Experiment 2.1 B, C1–C4; `HDR-03`, `HDR-04` |
| Scope E1 byte hashes to one SQLite version (header bytes 96–99) | Experiment 2.1 A (`3053004`) |
| Refuse R1 page captures unless at rest (empty `-wal`, no hot `-journal`) | Experiment 2.3 D1–D2; `CAP-03`–`CAP-05` |
| Avoid `mode=ro` opens on closed WAL files that must stay sidecar-free | Experiment 2.1 A |
| Never `os.Open`/`Close` a database file in a process with a live SQLite handle | Experiment 2.1 child reader; Experiment 2.2 (`os.ReadFile` / explicit `Close`) |
| Track space by page role, including lock-byte (> 1 GiB) and pointer-map pages | Experiment 2.2 A (`170/270` overflow), D, E; `CEN-03`, `CEN-06` |
| Set `secure_delete=ON` so freelist leaves are zeroed in captures | Experiment 2.3 A (pages 231–232 hash `ad7facb2…`); `freePage2` |

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. Under what exact condition do SQLite (`lockBtree`, [`src/btree.c:3298-3302`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L3298-L3302)) and `internal/dbfile` trust the 4-byte in-header database size at offset 28, and what happens in Experiment 2.1 part E when the two counters agree but offset 28 is larger than the OS file size?
2. In Experiment 2.1 part C, explain all four WAL-mode counter outcomes: why three consecutive recipe commits on one connection froze bytes 24–27 at `30` (C1); why running `PRAGMA shrink_memory` before each step on that same connection advanced the counter to `30`, `31`, `32` (C4); why three `UPDATE accounts` commits left all 100 header bytes unchanged even after close (C2); and why opening a new connection per step also advanced the counter (C3).
3. On a table-leaf page with usable size 4,096 bytes (`maxLocal = 4061`, `minLocal = 489`), Experiment 2.2 part B inserted three rows with payloads of 4,061, 4,062, and 8,153 bytes. How many bytes stayed local and how many overflow pages were allocated for each row, and why did adding 1 byte past `4061` reduce local bytes from `4061` to `489`?
4. In Experiment 2.2 part F2, why did SQLite's `PRAGMA integrity_check` report `ok` on a database whose last page was truncated in half, while `internal/dbfile` returned `ErrTruncated`?
5. In Experiment 2.3 part D, what concrete data corruption or loss would result from capturing `.db` alone in D1 (WAL mode) versus D2 (rollback mode), and why did D2 need `cache_size=10` and a full-table `UPDATE` to produce a hot journal before `COMMIT`?

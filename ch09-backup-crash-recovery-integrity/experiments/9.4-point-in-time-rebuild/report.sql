-- Walkthrough of experiment 9.4. Run it with:
--   go run ./cmd/course run 9.4        (add -i to stay in DuckDB afterwards)
-- The runner loads every CSV the experiment wrote as a table of the same name:
--   steps            one row per step a child process ran (writer or outsider);
--                    next_cycle is the shipper cycle that ran after it
--   shipper_starts   one row per time the shipper was opened
--   cycles           one row per shipper cycle: what it saw in the WAL, the segment
--                    it shipped (if any) and its PASSIVE checkpoint
--   snapshots        one row per snapshot the shipper stored (cycle is empty for
--                    the one taken when the shipper opened)
--   store            the store as the restore reads it (one row)
--   restores         one row per stream position restored from the store
--   final            the newest restore written to a file, and the live database
--   tampered         one row per damaged copy of the store
--   checks           every claim the experiment asserted (the runner lists it last)
-- Hashes are the first 16 hex digits of a SHA-256. WAL generations are numbered
-- in the order they appear, because their salts are random.

.print
.print '════════════════════════════════════════════════════════════════════════'
.print '  9.4  Point-in-time rebuild from a snapshot and WAL segments'
.print '════════════════════════════════════════════════════════════════════════'
.print
.print 'The recipe database is built to step 2. Two child processes, writer and'
.print 'outsider, each hold a connection (wal_autocheckpoint=0); the writer commits'
.print 'every step on a fresh connection. A shipper (internal/walship) copies each'
.print 'new run of committed WAL frames to a store as one segment, then runs a'
.print 'PASSIVE checkpoint, and stores a snapshot every 3 segments. A stream'
.print 'position counts the commits the shipper has shipped.'

.print
.print '── A. Phase 1: shipping steps 3-14 ─────────────────────────────────────'
.print
.print 'A1. The shipper opens on an empty store and takes the initial snapshot.'

SELECT snapshot, reason, position, pages, user_version, state_sha256,
       CASE WHEN generation = 'no WAL' THEN 'none (no WAL yet)'
            ELSE after_frame || ' of ' || generation END AS after_wal_frame
FROM snapshots WHERE cycle IS NULL ORDER BY position;

.print
.print 'A2. The writer commits a batch of steps, then the shipper runs one cycle.'
.print '    Each batch becomes one segment. The checkpoint copies every frame, so'
.print '    the next commit restarts the WAL: every segment is a new generation'
.print '    and starts at frame 1.'

SELECT c.cycle,
       (SELECT string_agg(split_part(s.action, ' ', 2), ' ' ORDER BY s.step)
        FROM steps s WHERE s.next_cycle = c.cycle AND s.child = 'writer') AS steps_committed,
       c.segment,
       c.first_position || '-' || c.last_position         AS positions,
       c.first_frame || '-' || c.last_frame               AS frames,
       c.generation,
       c.first_user_version || '-' || c.last_user_version AS user_version,
       'copied ' || c.checkpointed || ' of ' || c.checkpoint_log AS checkpoint
FROM cycles c WHERE c.part = 'A' ORDER BY c.cycle;

.print
.print 'A3. After every third segment the shipper also stores a snapshot of the'
.print '    state that segment leaves:'

SELECT cycle, snapshot, reason, position, pages, user_version, state_sha256,
       after_frame || ' of ' || generation AS after_wal_frame
FROM snapshots WHERE part = 'A' AND cycle IS NOT NULL ORDER BY cycle;

.print
.print '── B. Phase 2: the shipper is down while another process checkpoints ──'
.print
.print 'B1. With the shipper closed, the writer commits 15 and 16, the outsider'
.print '    runs a TRUNCATE checkpoint (it copies them into the database file and'
.print '    empties the WAL), and the writer commits 17.'

SELECT step, child, action, outcome FROM steps WHERE part = 'B' AND next_cycle = 9 ORDER BY step;

.print
.print 'B2. The shipper reopens from the store, which ends at position 14:'

SELECT started, position FROM shipper_starts WHERE part = 'B' ORDER BY position;

.print
.print 'B3. Its first cycle finds a WAL header it never shipped. It cannot know'
.print '    what was committed in between, so it ships no segment and stores a'
.print '    new snapshot instead. That snapshot holds step 17 at position 15.'

SELECT cycle, wal, coalesce(segment, 'none') AS segment,
       'copied ' || checkpointed || ' of ' || checkpoint_log AS checkpoint
FROM cycles WHERE part = 'B' AND segment IS NULL ORDER BY cycle;

SELECT cycle, snapshot, reason, position, pages, user_version, state_sha256,
       after_frame || ' of ' || generation AS after_wal_frame
FROM snapshots WHERE part = 'B' ORDER BY cycle;

.print
.print 'B4. Shipping resumes from the new snapshot: steps 18-20 become positions'
.print '    16-18. From here on the position and the recipe step differ by 2.'

SELECT c.cycle,
       (SELECT string_agg(split_part(s.action, ' ', 2), ' ' ORDER BY s.step)
        FROM steps s WHERE s.next_cycle = c.cycle AND s.child = 'writer') AS steps_committed,
       c.segment,
       c.first_position || '-' || c.last_position         AS positions,
       c.first_frame || '-' || c.last_frame               AS frames,
       c.generation,
       c.first_user_version || '-' || c.last_user_version AS user_version,
       'copied ' || c.checkpointed || ' of ' || c.checkpoint_log AS checkpoint
FROM cycles c WHERE c.part = 'B' AND c.segment IS NOT NULL ORDER BY c.cycle;

.print
.print 'B5. Each segment names the database state it starts from and the state'
.print '    it leaves. Every segment starts from the state the store held just'
.print '    before it: the previous segment, or the snapshot after the gap.'

WITH states AS (
    SELECT cycle, segment AS object, after_sha256 AS sha256 FROM cycles WHERE segment IS NOT NULL
    UNION ALL
    SELECT coalesce(cycle, 0), snapshot, state_sha256 FROM snapshots
)
SELECT c.cycle, c.segment, c.before_sha256, c.after_sha256,
       (SELECT object FROM states s WHERE s.cycle < c.cycle
        ORDER BY s.cycle DESC, s.object LIMIT 1)       AS starts_from,
       c.before_sha256 = (SELECT sha256 FROM states s WHERE s.cycle < c.cycle
                          ORDER BY s.cycle DESC, s.object LIMIT 1) AS links
FROM cycles c WHERE c.segment IS NOT NULL ORDER BY c.cycle;

.print
.print '── C. Restore every position ───────────────────────────────────────────'
.print
.print 'C1. What the store holds (every object passed its checks):'

SELECT snapshots, snapshot_positions, segments, damaged FROM store ORDER BY snapshots;

.print
.print 'C2. Each position restored from the newest snapshot at or before it, then'
.print '    compared with the recipe built to the same step. Every restore is'
.print '    byte-identical (E1), and none replays more than 3 segments.'

SELECT position, snapshot, segments, user_version, sha256, recipe_sha256, e1_equal
FROM restores ORDER BY position;

.print
.print 'C3. The same positions restored from every older snapshot. Before the'
.print '    discontinuity the older snapshots give the same bytes; after it no'
.print '    segment chain leads from them to position 15 or later, and the plan'
.print '    refuses.'

SELECT position, snapshot AS newest_snapshot, older_snapshots, older_same_bytes, older_cannot_reach
FROM restores ORDER BY position;

.print
.print 'C4. Recipe steps with no restore point: the two that were committed and'
.print '    checkpointed away while the shipper was down.'

SELECT step AS recipe_step
FROM range(2, 21) t(step)
WHERE step NOT IN (SELECT user_version FROM restores)
ORDER BY step;

.print
.print 'C5. The newest position written out as a file, beside the live database'
.print '    once every process has closed it. Both are recipe step 20, byte for byte.'

SELECT file, position, integrity_check, recipe_step, exact_bytes FROM final ORDER BY position NULLS LAST;

.print
.print '── D. Damaged stores ───────────────────────────────────────────────────'
.print
.print 'D1. Three copies of the store, each damaged one way. Every restore is'
.print '    refused while planning, before any page is applied.'

SELECT seq, damage, restore_to, refused_while_planning FROM tampered ORDER BY seq;

.print
.print 'D2. Why each was refused. Every error is walship.ErrBrokenChain; each'
.print '    further line is the cause of the line above it.'

WITH parts AS (SELECT seq, damage, string_split(error, ': ') AS p FROM tampered)
SELECT CASE WHEN i = 1 THEN damage ELSE '' END AS damage,
       repeat('  ', i - 1) || p[i]           AS error
FROM parts, range(1, 10) r(i)
WHERE i <= len(p)
ORDER BY seq, i;

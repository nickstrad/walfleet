// Experiment 9.4 demonstrates Rebuild Ladder rung R6 using internal/walship:
// continuous snapshot-plus-WAL-segment capture across shipper-owned checkpoints
// and WAL restarts (Phase 1), discontinuity detection and re-basing after an
// outside TRUNCATE checkpoint while the shipper was stopped (Phase 2), exact E1
// point-in-time restore of every shipped position from both newest and older
// snapshots, and pre-replay rejection of missing, swapped, and corrupted
// segments.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 9.4
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

const (
	baseStep      = 2
	snapshotEvery = 3
)

var shippedBatches = [][]int{{3}, {4, 5}, {6}, {7, 8, 9}, {10}, {11, 12}, {13}, {14}}

type run struct {
	ctx      context.Context
	lab      *procs.Lab
	r        recipe.Recipe
	dir      string
	db       string
	store    *walship.DirStore
	shipper  *walship.Shipper
	writer   *procs.Child
	outsider *procs.Child
	names    walship.GenerationNames
	set      *results.Set
	part     string
	cycles   int
	tampers  int
	tables   struct {
		steps, starts, cycles, snapshots, store, restores, final, tampered *results.Table
	}
}

func (x *run) openTables() {
	x.tables.steps = x.set.Table("steps", "step", "part", "next_cycle", "child", "action", "outcome")
	x.tables.starts = x.set.Table("shipper_starts", "part", "started", "position")
	x.tables.cycles = x.set.Table("cycles", "cycle", "part", "wal", "segment", "first_position", "last_position", "first_frame", "last_frame", "generation", "first_user_version", "last_user_version", "before_sha256", "after_sha256", "checkpoint_busy", "checkpoint_log", "checkpointed", "every_frame_copied")
	x.tables.snapshots = x.set.Table("snapshots", "cycle", "part", "snapshot", "reason", "position", "pages", "user_version", "state_sha256", "after_frame", "generation")
	x.tables.store = x.set.Table("store", "snapshots", "snapshot_positions", "segments", "damaged")
	x.tables.restores = x.set.Table("restores", "position", "snapshot", "segments", "user_version", "sha256", "recipe_sha256", "e1_equal", "older_snapshots", "older_same_bytes", "older_cannot_reach")
	x.tables.final = x.set.Table("final", "file", "position", "integrity_check", "recipe_step", "exact_bytes")
	x.tables.tampered = x.set.Table("tampered", "seq", "damage", "restore_to", "refused_while_planning", "error")
}

func (x *run) generation(g walship.Generation) string {
	return strings.TrimPrefix(x.names.Name(g), "WAL generation ")
}

func (x *run) snapshotRow(cycle any, m walship.SnapshotMeta) {
	x.tables.snapshots.Row(cycle, x.part, walship.SnapshotKey(m.Position), m.Reason, m.Position, m.Pages, m.UserVersion, m.StateSHA256[:16], m.WAL.Frame, x.generation(m.WAL.Generation))
}

func (x *run) openShipper() {
	shipper, err := walship.Open(x.ctx, walship.Config{Database: x.db, Store: x.store, Options: x.r.SQLiteOptions(), SnapshotEvery: snapshotEvery, Origin: baseStep})
	if err != nil {
		x.lab.Fatal("open shipper", err)
	}
	x.shipper = shipper
	if snapshot := shipper.OpenSnapshot(); snapshot != nil {
		x.tables.starts.Row(x.part, "opened on an empty store: initial snapshot", snapshot.Position)
		x.snapshotRow(nil, *snapshot)
	} else {
		x.tables.starts.Row(x.part, "reopened: resumed from the store", shipper.Position())
	}
}

func (x *run) commit(steps ...int) {
	for _, k := range steps {
		x.lab.Must(x.writer, "recipe-session", strconv.Itoa(k))
	}
}

func (x *run) cycle() walship.CycleReport {
	report, err := x.shipper.Cycle(x.ctx)
	if err != nil {
		x.lab.Fatal("ship cycle", err)
	}
	x.cycles++
	wal := report.Change.String()
	if report.Reason != "" {
		wal += ": " + report.Reason
	}
	cp := report.Checkpoint
	if m := report.Segment; m != nil {
		x.tables.cycles.Row(x.cycles, x.part, wal, walship.SegmentKey(m.FirstPosition), m.FirstPosition, m.LastPosition, m.FirstFrame, m.LastFrame, x.generation(m.Generation),
			m.Commits[0].UserVersion, m.Commits[len(m.Commits)-1].UserVersion, m.BeforeSHA256[:16], m.AfterSHA256[:16], cp.Busy, cp.LogFrames, cp.Checkpointed, report.FullBackfill)
	} else {
		x.tables.cycles.Row(x.cycles, x.part, wal, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, cp.Busy, cp.LogFrames, cp.Checkpointed, report.FullBackfill)
	}
	if report.Snapshot != nil {
		x.snapshotRow(x.cycles, *report.Snapshot)
	}
	return report
}

func (x *run) closeShipper() {
	if err := x.shipper.Close(); err != nil {
		x.lab.Fatal("close shipper", err)
	}
}

// restoreEveryPosition rebuilds every stream position from baseStep to last,
// checks E1 equality against recipe.StepHashes, verifies that every older
// snapshot either reproduces identical bytes or refuses across a discontinuity
// gap with ErrBrokenChain, and checks that newest-snapshot restores never
// replay more than snapshotEvery segments.
func (x *run) restoreEveryPosition(catalog *walship.Catalog, hashes []recipe.FileHashes) []int {
	last, _ := catalog.LastPosition()
	var steps []int
	worst := 0
	for position := int64(baseStep); position <= last; position++ {
		plan, err := catalog.Plan(position)
		if err != nil {
			x.lab.Fatal(fmt.Sprintf("plan %d", position), err)
		}
		image, report, err := plan.Execute()
		if err != nil {
			x.lab.Fatal(fmt.Sprintf("restore %d", position), err)
		}
		step := int(plan.Commit.UserVersion)
		steps = append(steps, step)
		worst = max(worst, len(plan.Segments))
		same, refused := 0, 0
		for _, snapshot := range catalog.Snapshots {
			if snapshot.Meta.Position >= plan.Snapshot.Meta.Position {
				continue
			}
			older, err := catalog.PlanFrom(snapshot.Meta.Position, position)
			if errors.Is(err, walship.ErrBrokenChain) {
				refused++
				continue
			}
			if err != nil {
				x.lab.Fatal("plan from an older snapshot", err)
			}
			olderImage, _, err := older.Execute()
			if err != nil {
				x.lab.Fatal("restore from an older snapshot", err)
			}
			if walship.SHA256Hex(olderImage) == report.StateSHA256 {
				same++
			}
		}
		digest := walship.SHA256Hex(image)
		equal := step < len(hashes) && digest == hashes[step].Exact
		older := countOlder(catalog, plan.Snapshot.Meta.Position)
		x.tables.restores.Row(position, plan.Snapshot.Meta.Position, len(plan.Segments), step, digest[:16], hashes[step].Exact[:16], equal, older, same, refused)
		x.set.Check(x.part, equal && same+refused == older, fmt.Sprintf("position %d restores byte-identical to recipe step %d from every snapshot that reaches it", position, step))
	}
	x.set.Check(x.part, worst <= snapshotEvery, fmt.Sprintf("a restore from the newest snapshot never replays more than %d segments (worst here: %d)", snapshotEvery, worst))
	return steps
}

func countOlder(catalog *walship.Catalog, position int64) int {
	n := 0
	for _, snapshot := range catalog.Snapshots {
		if snapshot.Meta.Position < position {
			n++
		}
	}
	return n
}

// tamper copies the finished store, applies one fault (missing, swapped, or
// byte-flipped segment), and verifies Catalog.Plan fails with ErrBrokenChain
// before any page is replayed.
func (x *run) tamper(label string, target int64, damage func(walship.Store) error) {
	copied, err := walship.NewDirStore(filepath.Join(x.dir, "tampered", label))
	if err != nil {
		x.lab.Fatal("tampered store", err)
	}
	if _, err := walship.CopyStore(x.ctx, x.store, copied); err != nil {
		x.lab.Fatal("copy store", err)
	}
	if err := damage(copied); err != nil {
		x.lab.Fatal("damage "+label, err)
	}
	catalog, err := walship.LoadCatalog(x.ctx, copied)
	if err != nil {
		x.lab.Fatal("catalog", err)
	}
	_, err = catalog.Plan(target)
	refused := errors.Is(err, walship.ErrBrokenChain)
	var text any
	if err != nil {
		text = err.Error()
	}
	x.tampers++
	x.tables.tampered.Row(x.tampers, label, target, refused, text)
	x.set.Check(x.part, refused, fmt.Sprintf("%s: refused while planning, before any page is applied", label))
}

func swap(ctx context.Context, store walship.Store, a, b string) error {
	first, err := store.Get(ctx, a)
	if err != nil {
		return err
	}
	second, err := store.Get(ctx, b)
	if err != nil {
		return err
	}
	if err := errors.Join(store.Delete(ctx, a), store.Delete(ctx, b)); err != nil {
		return err
	}
	return errors.Join(store.PutIfAbsent(ctx, a, second), store.PutIfAbsent(ctx, b, first))
}

func flipByte(ctx context.Context, store walship.Store, key string) error {
	data, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	data[len(data)-100] ^= 0x40
	if err := store.Delete(ctx, key); err != nil {
		return err
	}
	return store.PutIfAbsent(ctx, key, data)
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	lab := procs.NewQuietLab(os.Stdout)
	x := &run{ctx: ctx, lab: lab, r: recipe.Default(), dir: dir, db: filepath.Join(dir, "live", "r.db"), set: results.MustOpen(dir), part: "A"}
	// The steps table records each child step.
	lab.OnStep = func(e procs.StepEvent) {
		x.tables.steps.Row(e.Result.Step, x.part, x.cycles+1, e.Child.Name, e.Action, e.Result.Outcome())
	}
	x.openTables()
	if err := os.MkdirAll(filepath.Dir(x.db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(ctx, x.db, baseStep); err != nil {
		lab.Fatal("build", err)
	}
	store, err := walship.NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		lab.Fatal("store", err)
	}
	x.store = store
	options := procs.SQLOptions{Path: x.db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}
	x.writer = lab.SpawnSQL("writer", options)
	x.outsider = lab.SpawnSQL("outsider", options)
	x.openShipper()

	// Part A: the writer commits steps 3-14 in batches; the shipper runs one cycle after each batch.
	generations := map[walship.Generation]bool{}
	for _, batch := range shippedBatches {
		x.commit(batch...)
		report := x.cycle()
		last := batch[len(batch)-1]
		segment := report.Segment
		x.set.Check("A", segment != nil && segment.LastPosition == int64(last) && len(segment.Commits) == len(batch) && segment.Commits[len(batch)-1].UserVersion == uint32(last) && report.FullBackfill,
			fmt.Sprintf("steps %v shipped as one segment of %d commit(s), then the shipper's checkpoint copied every frame", batch, len(batch)))
		if segment != nil {
			generations[segment.Generation] = true
		}
	}
	x.set.Check("A", len(generations) == len(shippedBatches), fmt.Sprintf("every segment came from a new WAL generation: the WAL restarted after each shipper checkpoint (%d generations)", len(generations)))

	// Part B: the shipper stops; the writer commits 15 and 16; the outsider runs
	// a TRUNCATE checkpoint; the writer commits 17; the shipper restarts.
	x.part = "B"
	x.closeShipper()
	x.commit(15, 16)
	truncate := x.lab.Must(x.outsider, "checkpoint", sqlitex.CheckpointTruncate)
	x.set.Check("B", truncate.Value == "busy=0 log=0 checkpointed=0", "the outsider's TRUNCATE checkpoint copied steps 15-16 into the database file and emptied the WAL")
	x.commit(17)
	x.openShipper()
	report := x.cycle()
	x.set.Check("B", report.Change == walship.WALDiscontinuity && report.Snapshot != nil && report.Snapshot.Reason == walship.ReasonDiscontinuity && report.Snapshot.UserVersion == 17 && report.Segment == nil,
		"the restarted shipper sees a WAL header it did not ship, cannot vouch for what happened in between, and takes a new snapshot (it holds step 17)")
	x.commit(18)
	x.cycle()
	x.commit(19, 20)
	x.cycle()
	x.closeShipper()
	lab.StopChildren()

	// Part C: restore every position from the store and compare with the recipe.
	x.part = "C"
	catalog, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		lab.Fatal("catalog", err)
	}
	x.tables.store.Row(len(catalog.Snapshots), joinPositions(snapshotPositions(catalog)), len(catalog.Segments), len(catalog.Damaged))
	hashes, err := x.r.StepHashes(ctx)
	if err != nil {
		lab.Fatal("recipe hashes", err)
	}
	steps := x.restoreEveryPosition(catalog, hashes)
	var missing []int
	for k := baseStep; k <= 20; k++ {
		if !slices.Contains(steps, k) {
			missing = append(missing, k)
		}
	}
	x.set.Check("C", slices.Equal(missing, []int{15, 16}), "only steps 15 and 16 have no restore point: they were committed and checkpointed away while the shipper was down")

	last, _ := catalog.LastPosition()
	plan, err := catalog.Plan(last)
	if err != nil {
		lab.Fatal("plan", err)
	}
	image, _, err := plan.Execute()
	if err != nil {
		lab.Fatal("restore", err)
	}
	restored := filepath.Join(dir, "restored.db")
	if err := walship.WriteDatabase(restored, image); err != nil {
		lab.Fatal("write", err)
	}
	restoredMatch, err := x.r.MatchStep(ctx, restored)
	if err != nil {
		lab.Fatal("match", err)
	}
	inspection, err := procs.InspectDatabase(ctx, restored, sqlitex.JournalWAL)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	liveMatch, err := x.r.MatchStep(ctx, x.db)
	if err != nil {
		lab.Fatal("match live", err)
	}
	x.tables.final.Row("restored.db", last, procs.IntegritySummary(inspection.Integrity), restoredMatch.Step, restoredMatch.Exact)
	x.tables.final.Row("the live database after every process closed", nil, nil, liveMatch.Step, liveMatch.Exact)
	x.set.Check("C", inspection.Integrity == "ok" && restoredMatch.Exact && restoredMatch.Step == 20 && liveMatch.Exact && liveMatch.Step == 20, "the newest restore point, written to a file, opens, passes integrity_check and equals the live database's final state (step 20)")

	// Part D: damaged stores, each a copy of the store above.
	x.part = "D"
	x.tamper("segment 7 deleted", 9, func(s walship.Store) error { return s.Delete(ctx, walship.SegmentKey(7)) })
	x.tamper("segments 10 and 11 swapped", 11, func(s walship.Store) error { return swap(ctx, s, walship.SegmentKey(10), walship.SegmentKey(11)) })
	x.tamper("one byte of segment 13 flipped", 13, func(s walship.Store) error { return flipByte(ctx, s, walship.SegmentKey(13)) })
	x.set.Finish("POINT IN TIME")
}

func joinPositions(positions []int64) string {
	text := make([]string, len(positions))
	for i, p := range positions {
		text[i] = fmt.Sprint(p)
	}
	return strings.Join(text, " ")
}

func snapshotPositions(catalog *walship.Catalog) []int64 {
	var positions []int64
	for _, s := range catalog.Snapshots {
		positions = append(positions, s.Meta.Position)
	}
	return positions
}

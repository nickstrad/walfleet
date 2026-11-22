// Experiment 7.2 measures exact pwrite64 calls, bytes written, fsync/fdatasync
// calls, opens, unlinks, and ftruncates for inserting 1,000 rows across 12
// configurations: journal_mode {delete, wal} x synchronous {NORMAL, FULL} x
// batching {1x1000, 20x50, 1000x1}. Each cell runs a child Go process under
// Linux strace and splits the syscall log per commit using a marker file.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.2 (strace needs Linux, which
// the lab container provides).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const (
	packagePath   = "ch07-vfs-and-filesystem-io/experiments/7.2-io-cost-by-pattern"
	totalRows     = 1000
	valueBytes    = 100
	dbName        = "t.db"
	markerName    = "commit-marker"
	parallelCells = 4
)

type cell struct {
	journal string
	sync    string
	batch   int
}

func (c cell) commits() int {
	return totalRows / c.batch
}

func (c cell) label() string {
	return fmt.Sprintf("%-6s %-6s %4d x %-4d", c.journal, c.sync, c.commits(), c.batch)
}

func (c cell) dirName() string {
	return fmt.Sprintf("%s-%s-%d", c.journal, strings.ToLower(c.sync), c.batch)
}

func cells() []cell {
	var out []cell
	for _, journal := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		for _, sync := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
			for _, batch := range []int{totalRows, 50, 1} {
				out = append(out, cell{journal, sync, batch})
			}
		}
	}
	return out
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.2" runs both.
var set *results.Set

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		child(os.Args[2:])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	base := sqlitex.MustWorkDir("7.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(base))
	set = results.MustOpen(base)
	cellTable := set.Table("cells", "cell", "journal", "sync", "commits", "rows_per_commit")
	for i, c := range cells() {
		cellTable.Row(i+1, c.journal, c.sync, c.commits(), c.batch)
	}
	tracing := set.Table("tracing", "syscalls")
	if !trace.Supported {
		tracing.Row(trace.LinuxOnlyMessage("counting writes, bytes and syncs with strace", packagePath))
		for _, c := range cells() {
			dir := filepath.Join(base, c.dirName())
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fatal("mkdir", err)
			}
			if err := runWorkload(ctx, dir, c, func() {}); err != nil {
				fatal(c.label(), err)
			}
		}
		trace.PrintLinuxOnly("counting writes, bytes and syncs with strace", packagePath)
		set.Finish("IO COST BY PATTERN")
		return
	}
	tracing.Row("traced with strace")
	measured := map[cell]*measurement{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallelCells)
	errs := map[cell]error{}
	for _, c := range cells() {
		dir := filepath.Join(base, c.dirName())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			m, err := measure(ctx, dir, c)
			mu.Lock()
			defer mu.Unlock()
			measured[c] = m
			errs[c] = err
		}()
	}
	wg.Wait()
	for _, c := range cells() {
		if errs[c] != nil {
			fatal(c.label(), errs[c])
		}
	}
	report(measured)
	set.Finish("IO COST BY PATTERN")
}

func options(c cell) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = c.journal
	o.Conn.Synchronous = c.sync
	o.SingleConnection = true
	return o
}

func value(id int) []byte {
	v := make([]byte, valueBytes)
	for i := range v {
		v[i] = byte((id*31 + i*7) % 251)
	}
	return v
}

func runWorkload(ctx context.Context, dir string, c cell, afterCommit func()) error {
	path := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, path, options(c))
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB NOT NULL)"); err != nil {
		db.Close()
		return err
	}
	afterCommit()
	id := 0
	for range c.commits() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			return err
		}
		for range c.batch {
			id++
			if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", id, value(id)); err != nil {
				tx.Rollback()
				db.Close()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			return err
		}
		afterCommit()
	}
	if err := db.Close(); err != nil {
		return err
	}
	afterCommit()
	return nil
}

// child runs one workload cell inside a dedicated straced process, pinning its
// goroutine to one OS thread and opening markerName after CREATE, after every
// commit, and after Close so the parent can segment the strace log per commit.
func child(args []string) {
	runtime.LockOSThread()
	if len(args) != 4 {
		fatal("child", errors.New("usage: child DIR JOURNAL SYNC BATCH"))
	}
	batch, err := strconv.Atoi(args[3])
	if err != nil {
		fatal("child", err)
	}
	dir := args[0]
	marker := filepath.Join(dir, markerName)
	touch := func() {
		if _, err := os.ReadFile(marker); err != nil {
			fatal("marker", err)
		}
	}
	if err := runWorkload(context.Background(), dir, cell{args[1], args[2], batch}, touch); err != nil {
		fatal("child", err)
	}
}

type counts struct {
	writes    int
	bytes     int64
	syncs     map[string]int
	opens     map[string]int
	unlinks   map[string]int
	truncates map[string]int
}

func newCounts() *counts {
	return &counts{syncs: map[string]int{}, opens: map[string]int{}, unlinks: map[string]int{}, truncates: map[string]int{}}
}

func (c *counts) totalSyncs() int {
	n := 0
	for _, v := range c.syncs {
		n += v
	}
	return n
}

func fileRole(path string) string {
	switch path {
	case dbName:
		return "db"
	case dbName + "-journal":
		return "journal"
	case dbName + "-wal":
		return "wal"
	case dbName + "-shm":
		return "shm"
	case ".":
		return "dir"
	}
	return ""
}

func (c *counts) add(s trace.Syscall) {
	role := fileRole(s.Path)
	if role == "" || !s.Succeeded() {
		return
	}
	switch trace.CanonicalName(s.Name) {
	case "pwrite64":
		c.writes++
		if s.Result > 0 {
			c.bytes += s.Result
		}
	case "sync":
		c.syncs[role]++
	case "open":
		c.opens[role]++
	case "unlink":
		c.unlinks[role]++
	case "ftruncate":
		c.truncates[role]++
	}
}

type commitShape struct {
	syncs      string
	dbWrites   bool
	totalSyncs int
}

// measurement holds one cell's counts: all sums the commits, segments has one
// entry per commit and a last one for the close, and dbWrites says whether that
// segment wrote the database file.
type measurement struct {
	all      *counts
	segments []*counts
	dbWrites []bool
	sequence []commitShape
}

// classifyWALCommits verifies every WAL commit fits the plain, checkpoint, or
// post-checkpoint restart shape and that at least one checkpoint+restart pair
// occurred (preventing vacuous passes if wal_autocheckpoint never triggered).
func classifyWALCommits(sequences [][]commitShape, plain, restart, checkpoint string) (ok bool, checkpoints, restarts int) {
	ok = true
	for _, sequence := range sequences {
		for i, s := range sequence {
			switch {
			case !s.dbWrites && s.syncs == plain:
			case s.dbWrites && s.syncs == checkpoint:
				checkpoints++
			case !s.dbWrites && s.syncs == restart && i > 0 && sequence[i-1].dbWrites:
				restarts++
			default:
				ok = false
			}
		}
	}
	return ok && checkpoints > 0 && restarts == checkpoints, checkpoints, restarts
}

func roleList(m map[string]int) string {
	roles := make([]string, 0, len(m))
	for r, n := range m {
		if n > 0 {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = fmt.Sprintf("%s:%d", r, m[r])
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func measure(ctx context.Context, dir string, c cell) (*measurement, error) {
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0o644); err != nil {
		return nil, err
	}
	command, err := trace.SelfCommand("child", dir, c.journal, c.sync, strconv.Itoa(c.batch))
	if err != nil {
		return nil, err
	}
	result, err := trace.Run(ctx, trace.Options{Command: command, Dir: dir, Syscalls: trace.DefaultSyscalls})
	if errors.Is(err, trace.ErrPtraceDenied) {
		return nil, fmt.Errorf("%w (run: docker compose run --rm lab go run ./%s)", err, packagePath)
	}
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("child exit %d: %s%s", result.ExitCode, result.Stdout, result.Stderr)
	}
	calls := trace.RelativeTo(dir, result.Log.Calls)
	var segments [][]trace.Syscall
	var current []trace.Syscall
	started := false
	for _, s := range calls {
		if trace.CanonicalName(s.Name) == "open" && s.Path == markerName {
			if started {
				segments = append(segments, current)
			}
			started = true
			current = nil
			continue
		}
		if started {
			current = append(current, s)
		}
	}
	if len(segments) != c.commits()+1 {
		return nil, fmt.Errorf("found %d marker segments, want %d commits + close", len(segments), c.commits())
	}
	m := &measurement{all: newCounts()}
	for i, seg := range segments {
		target := newCounts()
		dbWrites := false
		for _, s := range seg {
			target.add(s)
			if i < c.commits() {
				m.all.add(s)
			}
			if trace.CanonicalName(s.Name) == "pwrite64" && s.Path == dbName {
				dbWrites = true
			}
		}
		m.segments = append(m.segments, target)
		m.dbWrites = append(m.dbWrites, dbWrites)
		if i < c.commits() {
			m.sequence = append(m.sequence, commitShape{syncs: roleList(target.syncs), dbWrites: dbWrites, totalSyncs: target.totalSyncs()})
		}
	}
	return m, nil
}

// roles and kinds name the per-file columns of the segments table, such as
// syncs_db or unlinks_wal.
var (
	roles = []string{"db", "dir", "journal", "shm", "wal"}
	kinds = []struct {
		name   string
		counts func(*counts) map[string]int
	}{
		{"syncs", func(c *counts) map[string]int { return c.syncs }},
		{"opens", func(c *counts) map[string]int { return c.opens }},
		{"unlinks", func(c *counts) map[string]int { return c.unlinks }},
		{"ftruncates", func(c *counts) map[string]int { return c.truncates }},
	}
)

func report(measured map[cell]*measurement) {
	columns := []string{"cell", "seq", "phase", "pwrites", "bytes", "db_writes"}
	for _, k := range kinds {
		for _, r := range roles {
			columns = append(columns, k.name+"_"+r)
		}
	}
	table := set.Table("segments", columns...)
	for i, c := range cells() {
		m := measured[c]
		for j, seg := range m.segments {
			phase := "commit"
			if j == c.commits() {
				phase = "close"
			}
			row := []any{i + 1, j + 1, phase, seg.writes, seg.bytes, m.dbWrites[j]}
			for _, k := range kinds {
				for _, r := range roles {
					row = append(row, k.counts(seg)[r])
				}
			}
			table.Row(row...)
		}
	}

	expectEvery := func(c cell, want string) bool {
		sequence := measured[c].sequence
		for _, s := range sequence {
			if s != sequence[0] {
				return false
			}
		}
		return len(sequence) > 0 && sequence[0].syncs == want
	}
	for _, sync := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
		want := "db:1 dir:1 journal:1"
		n := 3
		if sync == sqlitex.SyncFull {
			want = "db:1 dir:1 journal:2"
			n = 4
		}
		ok := true
		for _, batch := range []int{totalRows, 50, 1} {
			ok = ok && expectEvery(cell{sqlitex.JournalDelete, sync, batch}, want)
		}
		set.Check("B", ok, fmt.Sprintf("rollback (DELETE) at %s: every commit, at every batch size, makes %d syncs [%s]", sync, n, want))
	}
	for _, sync := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
		plain, restart, checkpoint := walShapes(sync)
		var sequences [][]commitShape
		for _, batch := range []int{totalRows, 50, 1} {
			sequences = append(sequences, measured[cell{sqlitex.JournalWAL, sync, batch}].sequence)
		}
		ok, checkpoints, restarts := classifyWALCommits(sequences, plain, restart, checkpoint)
		set.Check("B", ok, fmt.Sprintf("WAL at %s: a commit makes [%s]; the commit that runs an automatic checkpoint makes [%s] (%d such), and the next commit, which restarts the WAL and writes a new WAL header, makes [%s] (%d such)", sync, plain, checkpoint, checkpoints, restart, restarts))
	}
	one := measured[cell{sqlitex.JournalDelete, sqlitex.SyncFull, 1}].all.totalSyncs()
	all := measured[cell{sqlitex.JournalDelete, sqlitex.SyncFull, totalRows}].all.totalSyncs()
	set.Check("A", one == 1000*all, fmt.Sprintf("rollback FULL: 1000 one-row commits cost %d syncs, one 1000-row commit costs %d (x%d)", one, all, one/max(all, 1)))
	walOne := measured[cell{sqlitex.JournalWAL, sqlitex.SyncNormal, 1}].all
	delOne := measured[cell{sqlitex.JournalDelete, sqlitex.SyncNormal, 1}].all
	set.Check("A", walOne.totalSyncs() < delOne.totalSyncs() && walOne.bytes < delOne.bytes, fmt.Sprintf("1000 one-row commits at NORMAL: WAL %d syncs and %d bytes written, rollback %d syncs and %d bytes", walOne.totalSyncs(), walOne.bytes, delOne.totalSyncs(), delOne.bytes))
}

// walShapes returns the expected per-commit sync patterns in WAL mode:
// plain commits sync nothing at NORMAL (wal:1 at FULL), an automatic checkpoint
// commit syncs both WAL and db (wal.c:2277, 2333), and the immediately following
// restart commit writes and syncs a fresh WAL header (wal.c:3875-3915, 4114-4124).
func walShapes(sync string) (plain, restart, checkpoint string) {
	if sync == sqlitex.SyncFull {
		return "wal:1", "wal:2", "db:1 wal:2"
	}
	return "-", "wal:1", "db:1 wal:1"
}

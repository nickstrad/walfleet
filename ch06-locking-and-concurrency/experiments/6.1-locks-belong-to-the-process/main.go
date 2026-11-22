// Command 6.1-locks-belong-to-the-process demonstrates that SQLite's POSIX
// advisory locks are byte-range locks owned by the OS process on a specific
// inode—the database file in rollback mode (src/os.h:160-166) and the -shm
// file in WAL mode (src/os_unix.c:4602-4603, src/wal.c:294-299)—and that
// closing any file descriptor to that inode inside the holding process drops
// every lock it holds on that file (src/os_unix.c:1189-1255).
//
// Part A contrasts rollback mode; Parts B, C, and D show all three WAL
// outcomes after a stray close of -shm (SQLITE_PROTOCOL on an empty WAL,
// silent overwrite on an active WAL, and silent loss when the contender exits
// first and deletes -wal/-shm).
//
// It prints only its verdict. The steps, lock states and outcomes go to CSV
// tables that report.sql presents: go run ./cmd/course run 6.1
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process"

const rowsQuery = "SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '') FROM (SELECT who FROM lab ORDER BY id)"

// Every step, lock probe and outcome goes to a CSV table in <work dir>/results/;
// part labels the rows written while a part runs.
var (
	set     *results.Set
	part    string
	viewSeq int
	tables  struct {
		steps, lockViews, lockProbes, procLocks, finalState, files, infoRun, infoProcesses *results.Table
	}
)

func openTables() {
	tables.steps = set.Table("steps", "part", "step", "process", "action", "result", "code", "value")
	tables.lockViews = set.Table("lock_views", "seq", "part", "after_step", "file", "proc_locks_listed", "proc_locks")
	tables.lockProbes = set.Table("lock_probes", "part", "after_step", "file", "lock", "first_byte", "last_byte", "state", "holder")
	tables.procLocks = set.Table("proc_locks", "part", "after_step", "file", "process", "class", "type", "bytes", "lock")
	tables.finalState = set.Table("final_state", "part", "integrity", "contents")
	tables.files = set.Table("files", "seq", "part", "moment", "file", "state")
	tables.infoRun = set.Table("info_run", "work_dir", "goos")
	tables.infoProcesses = set.Table("info_processes", "part", "process", "pid")
}

// recorder is a procs.Lab that writes each step, lock view and claim to the
// tables instead of printing them.
type recorder struct{ *procs.Lab }

func newRecorder() *recorder {
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil
	return &recorder{lab}
}

func (l *recorder) Do(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, action, args, l.Lab.Do(child, action, args...))
}

func (l *recorder) Must(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, action, args, l.Lab.Must(child, action, args...))
}

func (l *recorder) step(child *procs.Child, action string, args []string, r procs.Result) procs.Result {
	value := r.Value
	if r.Code == procs.CodeNotSQLite {
		value = r.Error
	}
	tables.steps.Row(part, r.Step, child.Name, procs.ActionText(action, args), r.CodeName(), r.Code, value)
	return r
}

func (l *recorder) Check(ok bool, claim string) { set.Check(part, ok, claim) }

// Locks probes ranges on path from the parent (fcntl F_GETLK) and, on Linux,
// lists the children's /proc/locks entries for path.
func (l *recorder) Locks(file, path string, ranges []procs.LockRange) procs.LockView {
	states, err := procs.ProbeRanges(path, ranges)
	if err != nil {
		l.Fatal("probe locks", err)
	}
	step := l.Script.Step()
	for _, s := range states {
		// F_GETLK names one arbitrary holder of a shared read lock, so only a
		// write lock has a holder worth naming.
		var holder any
		if s.Holder.Held && s.Holder.Write {
			holder = l.PidName(s.Holder.Pid)
		}
		tables.lockProbes.Row(part, step, file, s.Range.Name, s.Range.Start, s.Range.Start+s.Range.Length-1, s.Holder.Kind(), holder)
	}
	view := procs.LockView{Ranges: states}
	if procs.ProcLocksSupported {
		view.Proc = l.procLocks(file, path, ranges, step)
		view.ProcListed = true
	}
	viewSeq++
	tables.lockViews.Row(viewSeq, part, step, file, view.ProcListed, len(view.Proc))
	return view
}

func (l *recorder) procLocks(file, path string, ranges []procs.LockRange, step int) []procs.ProcLock {
	inode, err := procs.FileInode(path)
	if err != nil {
		l.Fatal("inode", err)
	}
	all, err := procs.ReadProcLocks()
	if err != nil {
		l.Fatal(procs.ProcLocksPath, err)
	}
	names := map[int]string{}
	pids := map[int]bool{}
	for _, child := range l.Script.Children() {
		names[child.Pid] = child.Name
		pids[child.Pid] = true
	}
	mine := procs.FilterProcLocks(all, inode, pids)
	sorted := slices.SortedFunc(slices.Values(mine), func(a, b procs.ProcLock) int {
		return cmp.Or(cmp.Compare(names[a.Pid], names[b.Pid]), cmp.Compare(a.Type, b.Type), cmp.Compare(a.Start, b.Start))
	})
	for _, lock := range sorted {
		tables.procLocks.Row(part, step, file, names[lock.Pid], lock.Class, lock.Type, lock.RangeText(), rangeLabel(lock, ranges))
	}
	return mine
}

// rangeLabel names the SQLite lock range a kernel lock covers exactly, or
// "(A+B)" when the kernel merged adjacent ranges into one lock.
func rangeLabel(lock procs.ProcLock, ranges []procs.LockRange) string {
	var covered []string
	for _, r := range ranges {
		if lock.Start == r.Start && !lock.ToEOF && lock.Length() == r.Length {
			return r.Name
		}
		if r.Start <= lock.End && lock.Start <= r.Start+r.Length-1 {
			covered = append(covered, r.Name)
		}
	}
	if len(covered) == 0 {
		return "(no named range)"
	}
	return "(" + strings.Join(covered, "+") + ")"
}

func prepare(ctx context.Context, lab *recorder, dir, journal string) (string, *procs.Child, *procs.Child) {
	return prepareIn(ctx, lab, filepath.Join(dir, journal), procs.SQLOptions{JournalMode: journal})
}

func prepareIn(ctx context.Context, lab *recorder, partDir string, options procs.SQLOptions) (string, *procs.Child, *procs.Child) {
	journal := options.JournalMode
	db := filepath.Join(partDir, "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, journal, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)"); err != nil {
		lab.Fatal("create", err)
	}
	options.Path = db
	holder := lab.SpawnSQL("holder", options)
	contender := lab.SpawnSQL("contender", options)
	tables.infoProcesses.Row(part, holder.Name, holder.Pid)
	tables.infoProcesses.Row(part, contender.Name, contender.Pid)
	return db, holder, contender
}

func kernelHolders(lab *recorder, view procs.LockView, rangeName string, want map[int]string, claim string) {
	if !view.ProcListed {
		return
	}
	lab.Check(maps.Equal(view.ProcHolders(rangeName), want), claim)
}

func kernelNone(lab *recorder, view procs.LockView, claim string) {
	if !view.ProcListed {
		return
	}
	lab.Check(len(view.Proc) == 0, claim)
}

func finalRows(ctx context.Context, lab *recorder, db, journal string) procs.Inspection {
	lab.StopChildren()
	inspection, err := procs.InspectDatabase(ctx, db, journal, rowsQuery)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	tables.finalState.Row(part, inspection.Integrity, inspection.Values[0])
	return inspection
}

// rollbackMode runs Part A: in journal_mode=delete, a stray os.Open+Close of
// the database file drops RESERVED and SHARED, admits a second writer whose
// COMMIT returns SQLITE_OK, and causes the holder's later COMMIT to overwrite
// the page and fail with SQLITE_IOERR_DELETE_NOENT (5898, src/os_unix.c:6842-6847).
func rollbackMode(ctx context.Context, lab *recorder, dir string) {
	part = "A"
	db, holder, contender := prepare(ctx, lab, dir, sqlitex.JournalDelete)
	ranges := procs.DatabaseLockRanges()
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	before := lab.Locks("database file", db, ranges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "RESERVED", holder.Pid) && procs.ReadLocked(before.Ranges, "SHARED") && procs.Unlocked(before.Ranges, "PENDING"),
		"the holder's open write transaction is a write lock on the RESERVED byte and a read lock on the SHARED range of the database file")
	kernelHolders(lab, before, "RESERVED", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: the RESERVED byte is write-locked by the holder alone")
	kernelHolders(lab, before, "SHARED", map[int]string{holder.Pid: "READ"}, "/proc/locks: the SHARED range is read-locked by the holder alone; the contender, with no transaction, holds nothing")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused the write lock: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "db")
	after := lab.Locks("database file", db, ranges)
	lab.Check(procs.Unlocked(after.Ranges), "after the holder opened and closed the database file with os.Open, none of its locks remain")
	kernelNone(lab, after, "/proc/locks: neither process holds any lock on the database file")
	inside := lab.Do(holder, "query", "SELECT count(*) FROM lab")
	lab.Check(inside.OK() && inside.Value == "1", "the holder still sees its uncommitted row: as far as SQLite knows, its write transaction is intact")
	admitted := lab.Do(contender, "begin", "immediate")
	lab.Check(admitted.OK(), "the second process is now admitted: BEGIN IMMEDIATE returns SQLITE_OK while the holder's transaction is still open")
	lab.Must(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')")
	during := lab.Locks("database file", db, ranges)
	lab.Check(procs.WriteLockedBy(during.Ranges, "RESERVED", contender.Pid), "the RESERVED byte is now write-locked by the second process")
	kernelHolders(lab, during, "SHARED", map[int]string{contender.Pid: "READ"}, "/proc/locks: the SHARED range is read-locked by the second process only; the holder, mid-transaction, holds no lock at all")
	contenderCommit := lab.Do(contender, "commit")
	lab.Check(contenderCommit.OK(), "the second process commits: its COMMIT returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.Code == sqlitex.CodeIOErrDeleteNoEnt,
		"the holder's COMMIT writes its pages, then fails to delete the rollback journal the other process already deleted: SQLITE_IOERR_DELETE_NOENT (5898)")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalDelete)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): holder",
		"the file passes integrity_check but holds only the holder's row: the commit that returned SQLITE_OK is lost")
}

// walMode runs Part B: in WAL mode with an empty WAL, writer exclusion lives on
// -shm byte 120 (WAL_WRITE_LOCK). A stray close of the database file drops only
// the holder's SHARED lock and still refuses the contender; a stray close of
// -shm drops all -shm locks, admits the contender, and sends the holder's
// COMMIT (on WAL_READ_LOCK(0)) into walRestartLog -> walTryBeginRead retries
// until SQLITE_PROTOCOL (15, src/wal.c:2963, 3053-3056, 3261-3265, 3875-3915).
func walMode(ctx context.Context, lab *recorder, dir string) {
	part = "B"
	db, holder, contender := prepare(ctx, lab, dir, sqlitex.JournalWAL)
	shm := db + "-shm"
	shmRanges, dbRanges := procs.ShmLockRanges(), procs.DatabaseLockRanges()
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	before := lab.Locks("-shm", shm, shmRanges)
	dbBefore := lab.Locks("database file", db, dbRanges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "WAL_WRITE_LOCK", holder.Pid) && procs.ReadLocked(before.Ranges, "WAL_READ_LOCK(0)") && procs.ReadLocked(before.Ranges, "DMS"),
		"the holder's write transaction is a write lock on -shm byte 120 (WAL_WRITE_LOCK), next to its read lock on byte 123 (WAL_READ_LOCK(0))")
	lab.Check(procs.ReadLocked(dbBefore.Ranges, "SHARED") && procs.Unlocked(dbBefore.Ranges, "PENDING", "RESERVED"),
		"on the database file the SHARED range is read-locked and PENDING and RESERVED are free: WAL mode does not use RESERVED for writers")
	kernelHolders(lab, before, "WAL_WRITE_LOCK", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: -shm byte 120 is write-locked by the holder alone")
	kernelHolders(lab, before, "DMS", map[int]string{holder.Pid: "READ", contender.Pid: "READ"}, "/proc/locks: both processes read-lock the DMS byte 128 of -shm")
	kernelHolders(lab, dbBefore, "SHARED", map[int]string{holder.Pid: "READ", contender.Pid: "READ"}, "/proc/locks: both processes read-lock the SHARED range of the database file")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "db")
	afterDB := lab.Locks("-shm", shm, shmRanges)
	dbAfter := lab.Locks("database file", db, dbRanges)
	lab.Check(procs.WriteLockedBy(afterDB.Ranges, "WAL_WRITE_LOCK", holder.Pid), "a stray close of the database file leaves the -shm write lock in place")
	kernelHolders(lab, dbAfter, "SHARED", map[int]string{contender.Pid: "READ"}, "/proc/locks: on the database file only the contender's SHARED read lock is left")
	kernelHolders(lab, afterDB, "WAL_WRITE_LOCK", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: -shm byte 120 is still write-locked by the holder")
	stillRefused := lab.Do(contender, "begin", "immediate")
	lab.Check(stillRefused.Code == sqlitex.CodeBusy, "so the second process is still refused")
	lab.Must(holder, "stray-close", "-shm")
	afterShm := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.Unlocked(afterShm.Ranges, "WAL_WRITE_LOCK", "WAL_READ_LOCK(0)"), "a stray close of the -shm file releases the holder's -shm locks")
	kernelHolders(lab, afterShm, "DMS", map[int]string{contender.Pid: "READ"}, "/proc/locks: the holder has no -shm lock left; only the contender's DMS read lock remains")
	if afterShm.ProcListed {
		lab.Check(len(afterShm.Proc) == 1, "/proc/locks: that DMS read lock is the only lock on -shm")
	}
	inside := lab.Do(holder, "query", "SELECT count(*) FROM lab")
	lab.Check(inside.OK() && inside.Value == "1", "the holder still sees its uncommitted row")
	admitted := lab.Do(contender, "begin", "immediate")
	lab.Check(admitted.OK(), "the second process is now admitted while the holder's transaction is still open")
	lab.Must(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')")
	contenderCommit := lab.Do(contender, "commit")
	lab.Check(contenderCommit.OK(), "the second process commits: its COMMIT returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.Code == sqlitex.CodeProtocol,
		"the holder's COMMIT finds the wal-index header changed under its write lock, retries, and gives up with SQLITE_PROTOCOL (15)")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): contender",
		"the file passes integrity_check and holds only the second process's row: the holder's transaction is lost, and the holder was told")
}

// walWithFrames runs Part C: when the WAL already holds an uncheckpointed commit,
// the holder starts on WAL_READ_LOCK(1) (byte 124). Because walRestartLog only
// runs when pWal->readLock == 0 (src/wal.c:3879), the holder skips the header
// check after the stray close of -shm, writes its frames over the contender's,
// and both COMMITs return SQLITE_OK while the contender's row silently vanishes.
func walWithFrames(ctx context.Context, lab *recorder, dir string) {
	part = "C"
	db, holder, contender := prepareIn(ctx, lab, filepath.Join(dir, "wal-with-frames"), procs.SQLOptions{JournalMode: sqlitex.JournalWAL})
	shm := db + "-shm"
	shmRanges := procs.ShmLockRanges()
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'seed')")
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (2, 'holder')")
	before := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "WAL_WRITE_LOCK", holder.Pid) && procs.ReadLocked(before.Ranges, "WAL_READ_LOCK(1)") && procs.Unlocked(before.Ranges, "WAL_READ_LOCK(0)"),
		"with frames in the WAL the holder reads through WAL_READ_LOCK(1), not WAL_READ_LOCK(0)")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "-shm")
	after := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.Unlocked(after.Ranges, "WAL_WRITE_LOCK", "WAL_READ_LOCK(1)"), "the stray close of -shm releases the holder's write lock and read lock")
	admitted := []procs.Result{lab.Do(contender, "begin", "immediate"), lab.Do(contender, "exec", "INSERT INTO lab VALUES (3, 'contender')"), lab.Do(contender, "commit")}
	lab.Check(allOK(admitted), "the second process is admitted, writes and commits: every statement returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.OK(),
		"the holder's COMMIT also returns SQLITE_OK: it is not on WAL_READ_LOCK(0), so it never re-reads the wal-index header and writes its frames over the other commit's")
	seen := lab.Do(contender, "query", rowsQuery)
	lab.Check(seen.OK() && seen.Value == "2 row(s): seed,holder",
		"the second process, still open, no longer sees the row it committed")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "2 row(s): seed,holder",
		"the file passes integrity_check and holds the seed and the holder's row: a commit that returned SQLITE_OK is lost and neither process was told")
}

// walContenderExits runs Part D: after stray closes of both db and -shm, the
// contender commits and closes before the holder commits. Seeing no other locks,
// sqlite3WalClose (src/wal.c:2507-2571) closes as the last connection and unlinks
// -wal and -shm; the holder then commits SQLITE_OK into its unlinked -wal fd and
// only the holder's row survives.
func walContenderExits(ctx context.Context, lab *recorder, dir string) {
	part = "D"
	db, holder, contender := prepareIn(ctx, lab, filepath.Join(dir, "wal-contender-exits"), procs.SQLOptions{JournalMode: sqlitex.JournalWAL})
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	lab.Must(holder, "stray-close", "db")
	lab.Must(holder, "stray-close", "-shm")
	admitted := []procs.Result{lab.Do(contender, "begin", "immediate"), lab.Do(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')"), lab.Do(contender, "commit")}
	lab.Check(allOK(admitted), "the second process is admitted, writes and commits: every statement returns SQLITE_OK")
	if err := contender.Stop(); err != nil {
		lab.Fatal("stop contender", err)
	}
	tables.files.Row(1, part, "after the contender exited", filepath.Base(db)+"-wal", presence(db+"-wal"))
	tables.files.Row(2, part, "after the contender exited", filepath.Base(db)+"-shm", presence(db+"-shm"))
	lab.Check(presence(db+"-wal") == "gone" && presence(db+"-shm") == "gone",
		"seeing no other process's locks, the second process closed as the last connection and deleted -wal and -shm")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.OK(), "the holder's COMMIT returns SQLITE_OK")
	tables.files.Row(3, part, "after the holder's commit", filepath.Base(db)+"-wal", presence(db+"-wal"))
	lab.Check(presence(db+"-wal") == "gone", "no -wal file exists under that name: the holder wrote its commit into the deleted file it still had open")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): holder",
		"the file passes integrity_check and holds only the holder's row: the second process's commit is lost and neither process was told")
}

func presence(path string) string {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return "present"
	case errors.Is(err, fs.ErrNotExist):
		return "gone"
	default:
		return "unreadable: " + err.Error()
	}
}

func allOK(results []procs.Result) bool {
	for _, r := range results {
		if !r.OK() {
			return false
		}
	}
	return true
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.1")
	fmt.Printf("INFO: work dir %s, GOOS %s\n", sqlitex.DisplayPath(dir), runtime.GOOS)
	if !procs.ProcLocksSupported {
		trace.PrintLinuxOnly("the /proc/locks listing (the fcntl F_GETLK probe and every behavioural check still run)", packagePath)
	}
	set = results.MustOpen(dir)
	openTables()
	tables.infoRun.Row(sqlitex.DisplayPath(dir), runtime.GOOS)
	lab := newRecorder()
	rollbackMode(ctx, lab, dir)
	walMode(ctx, lab, dir)
	walWithFrames(ctx, lab, dir)
	walContenderExits(ctx, lab, dir)
	set.Finish("LOCKS BELONG TO THE PROCESS")
}

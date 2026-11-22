// Command 6.2-busy-rules scripts cross-process reader/writer and writer/writer
// interleavings to verify SQLite's exact extended result codes: WAL reader and
// writer concurrency (including BEGIN EXCLUSIVE behaving like BEGIN IMMEDIATE
// under locking_mode=NORMAL), BEGIN DEFERRED vs BEGIN IMMEDIATE contention with
// busy_timeout=0 and 120s, immediate SQLITE_BUSY (5) and SQLITE_BUSY_SNAPSHOT
// (517) on a deferred read-then-write upgrade (src/btree.c:3740-3741,
// src/wal.c:3734-3737), rollback-mode COMMIT contention and retained PENDING
// locks (Part D), and alternating multi-process recipe steps under contention (Part E).
//
// It prints only its verdict. The steps and outcomes go to CSV tables that
// report.sql presents: go run ./cmd/course run 6.2
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	// longTimeout exceeds stepDeadline so any statement that waited out its busy
	// handler instead of returning at once (Part C3) would time out the child loudly.
	longTimeout   = 120 * time.Second
	stepDeadline  = 30 * time.Second
	countQuery    = "SELECT count(*) FROM lab"
	recipeBase    = 5
	recipeWrites  = 10
	insertPattern = "INSERT INTO lab(who) VALUES ('%s')"
)

// Every step and outcome goes to a CSV table in <work dir>/results/; part
// labels the rows written while a part runs.
var (
	set     *results.Set
	part    string
	stepSeq int
	tables  struct {
		steps, settings, recipeResult *results.Table
	}
)

func openTables() {
	tables.steps = set.Table("steps", "seq", "part", "step", "process", "action", "waiting_at_250ms", "result", "code", "value")
	tables.settings = set.Table("settings", "part", "journal_mode", "busy_timeout_ms")
	tables.recipeResult = set.Table("recipe_result", "part", "user_version", "matched_step", "exact")
}

// recorder is a procs.Lab that writes each step and claim to the tables
// instead of printing them.
type recorder struct {
	*procs.Lab
	sent map[*procs.Call]*sentCall
}

// sentCall is a step sent with Begin: who runs it and, once StillWaiting has
// looked, whether it was still unanswered.
type sentCall struct {
	child   *procs.Child
	waiting any
}

func newRecorder() *recorder {
	lab := procs.NewLab(os.Stdout)
	lab.Script.Out = nil
	return &recorder{Lab: lab, sent: map[*procs.Call]*sentCall{}}
}

func (l *recorder) Do(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, procs.ActionText(action, args), nil, l.Lab.Do(child, action, args...))
}

func (l *recorder) Must(child *procs.Child, action string, args ...string) procs.Result {
	return l.step(child, procs.ActionText(action, args), nil, l.Lab.Must(child, action, args...))
}

// Begin sends a step without waiting for its answer.
func (l *recorder) Begin(child *procs.Child, action string, args ...string) *procs.Call {
	call, err := l.Script.Begin(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	l.sent[call] = &sentCall{child: child}
	return call
}

// StillWaiting reports whether call is still unanswered after procs.StillWaitingWindow.
func (l *recorder) StillWaiting(call *procs.Call) bool {
	answered, err := call.AnsweredWithin(procs.StillWaitingWindow)
	if err != nil {
		l.Fatal("poll", err)
	}
	l.sent[call].waiting = !answered
	return !answered
}

func (l *recorder) Finish(call *procs.Call) procs.Result {
	sent := l.sent[call]
	delete(l.sent, call)
	request := call.Request()
	return l.step(sent.child, procs.ActionText(request.Action, request.Args), sent.waiting, l.Lab.Finish(call))
}

func (l *recorder) step(child *procs.Child, action string, waiting any, r procs.Result) procs.Result {
	value := r.Value
	if r.Code == procs.CodeNotSQLite {
		value = r.Error
	}
	stepSeq++
	tables.steps.Row(stepSeq, part, r.Step, child.Name, action, waiting, r.CodeName(), r.Code, value)
	return r
}

func (l *recorder) Check(ok bool, claim string) { set.Check(part, ok, claim) }

func insert(who string) string { return fmt.Sprintf(insertPattern, who) }

func database(ctx context.Context, lab *recorder, dir, name, journal string) string {
	db := filepath.Join(dir, name, "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, journal, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)", insert("seed")); err != nil {
		lab.Fatal("create", err)
	}
	return db
}

func pair(ctx context.Context, lab *recorder, dir, name, journal string, timeout time.Duration, first, second string) (*procs.Child, *procs.Child) {
	db := database(ctx, lab, dir, name, journal)
	options := procs.SQLOptions{Path: db, JournalMode: journal, BusyTimeout: timeout}
	tables.settings.Row(part, journal, timeout.Milliseconds())
	return lab.SpawnSQL(first, options), lab.SpawnSQL(second, options)
}

// readersAndWriter runs Part A: with busy_timeout=0 in WAL mode, readers and
// writers never block each other, and BEGIN EXCLUSIVE behaves like BEGIN IMMEDIATE.
func readersAndWriter(ctx context.Context, lab *recorder, dir string) {
	part = "A"
	reader, writer := pair(ctx, lab, dir, "a-reader-writer", sqlitex.JournalWAL, 0, "reader", "writer")
	lab.Must(reader, "begin", "deferred")
	snapshot := lab.Must(reader, "query", countQuery)
	commits := []procs.Result{lab.Do(writer, "begin", "immediate"), lab.Do(writer, "exec", insert("writer")), lab.Do(writer, "commit")}
	lab.Check(allOK(commits), "with a read transaction open, the writer begins, writes and commits without waiting")
	again := lab.Must(reader, "query", countQuery)
	lab.Check(again.Value == snapshot.Value, "the reader still sees its snapshot ("+snapshot.Value+" rows) after the commit")
	lab.Must(reader, "commit")
	lab.Must(writer, "begin", "immediate")
	lab.Must(writer, "exec", insert("uncommitted"))
	lab.Must(reader, "begin", "deferred")
	during := lab.Do(reader, "query", countQuery)
	lab.Check(during.OK() && during.Value == "2", "with a write transaction open, a reader starts and reads the last committed state (2 rows) without waiting")
	lab.Must(writer, "commit")
	lab.Must(reader, "commit")
	lab.Must(reader, "begin", "deferred")
	lab.Must(reader, "query", countQuery)
	exclusive := []procs.Result{lab.Do(writer, "begin", "exclusive"), lab.Do(writer, "exec", insert("exclusive"))}
	readDuring := lab.Do(reader, "query", countQuery)
	exclusive = append(exclusive, lab.Do(writer, "commit"))
	lab.Check(allOK(exclusive) && readDuring.OK(), "BEGIN EXCLUSIVE in WAL mode behaves like BEGIN IMMEDIATE: it starts beside an open reader and the reader keeps reading")
	lab.Must(reader, "commit")
	lab.StopChildren()
}

// writersWithoutTimeout runs Part B: with busy_timeout=0, BEGIN DEFERRED defers
// SQLITE_BUSY (5) until the first write statement, whereas BEGIN IMMEDIATE fails
// at BEGIN itself (src/build.c:5245-5274).
func writersWithoutTimeout(ctx context.Context, lab *recorder, dir string) {
	part = "B"
	first, second := pair(ctx, lab, dir, "b-writers-no-timeout", sqlitex.JournalWAL, 0, "writer-1", "writer-2")
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	deferredBegin := lab.Do(second, "begin", "deferred")
	deferredRead := lab.Do(second, "query", countQuery)
	deferredWrite := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(deferredBegin.OK() && deferredRead.OK() && deferredWrite.Code == sqlitex.CodeBusy,
		"BEGIN DEFERRED and a read succeed; the first write statement returns SQLITE_BUSY (5)")
	lab.Must(second, "rollback")
	immediate := lab.Do(second, "begin", "immediate")
	lab.Check(immediate.Code == sqlitex.CodeBusy, "BEGIN IMMEDIATE returns SQLITE_BUSY (5) at the BEGIN itself")
	lab.Must(first, "commit")
	lab.StopChildren()
}

// writersWithTimeout runs Part C: with a 120s busy_timeout, transactions that
// have not read yet (C1 BEGIN IMMEDIATE, C2 BEGIN DEFERRED writing first) wait
// on the busy handler (src/main.c:1730-1792), while a deferred transaction that
// already read (C3) skips the busy handler (src/btree.c:3740-3741) and gets
// SQLITE_BUSY (5) then SQLITE_BUSY_SNAPSHOT (517, src/wal.c:3734-3737) at once.
func writersWithTimeout(ctx context.Context, lab *recorder, dir string) {
	part = "C"
	first, second := pair(ctx, lab, dir, "c-writers-timeout", sqlitex.JournalWAL, longTimeout, "writer-1", "writer-2")

	part = "C1"
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	waiting := lab.Begin(second, "begin", "immediate")
	blocked := lab.StillWaiting(waiting)
	lab.Must(first, "commit")
	admitted := lab.Finish(waiting)
	lab.Check(blocked && admitted.OK(), "BEGIN IMMEDIATE did not return while the other writer held the lock, and returned SQLITE_OK after it committed")
	lab.Must(second, "exec", insert("writer-2"))
	lab.Must(second, "commit")

	part = "C2"
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	lab.Must(second, "begin", "deferred")
	pending := lab.Begin(second, "exec", insert("writer-2"))
	blocked = lab.StillWaiting(pending)
	lab.Must(first, "commit")
	written := lab.Finish(pending)
	lab.Check(blocked && written.OK(), "the first statement of a deferred transaction waits like BEGIN IMMEDIATE and then succeeds")
	lab.Must(second, "commit")

	part = "C3"
	lab.Must(second, "begin", "deferred")
	lab.Must(second, "query", countQuery)
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	upgrade := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(upgrade.Code == sqlitex.CodeBusy, "its first write returns SQLITE_BUSY (5) at once although the busy timeout is 120000ms")
	lab.Must(first, "commit")
	stale := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(stale.Code == sqlitex.CodeBusySnapshot, "after the other writer committed, the same write returns SQLITE_BUSY_SNAPSHOT (517) at once: the snapshot it read is no longer the newest")
	lab.Must(second, "rollback")
	retry := []procs.Result{lab.Do(second, "begin", "immediate"), lab.Do(second, "exec", insert("writer-2")), lab.Do(second, "commit")}
	lab.Check(allOK(retry), "the same write succeeds once the transaction is restarted")
	lab.StopChildren()
}

// rollbackContrast runs Part D: in rollback mode a reader's SHARED lock blocks
// the writer's COMMIT (EXCLUSIVE), and the failed COMMIT retains PENDING so new
// readers are blocked until the writer retries (src/os_unix.c:1898-1905).
func rollbackContrast(ctx context.Context, lab *recorder, dir string) {
	part = "D"
	reader, writer := pair(ctx, lab, dir, "d-rollback", sqlitex.JournalDelete, 0, "reader", "writer")
	lab.Must(reader, "begin", "deferred")
	lab.Must(reader, "query", countQuery)
	writes := []procs.Result{lab.Do(writer, "begin", "immediate"), lab.Do(writer, "exec", insert("writer"))}
	lab.Check(allOK(writes), "the writer takes RESERVED and writes beside the reader's SHARED lock")
	blockedCommit := lab.Do(writer, "commit")
	lab.Check(blockedCommit.Code == sqlitex.CodeBusy, "the writer's COMMIT returns SQLITE_BUSY (5): it needs EXCLUSIVE and the reader holds SHARED")
	lab.Must(reader, "commit")
	newReader := lab.Do(reader, "query", countQuery)
	lab.Check(newReader.Code == sqlitex.CodeBusy, "a new read now returns SQLITE_BUSY (5): the writer kept PENDING after its failed COMMIT")
	retried := lab.Do(writer, "commit")
	lab.Check(retried.OK(), "the writer's second COMMIT succeeds once no reader is left")
	after := lab.Do(reader, "query", countQuery)
	lab.Check(after.OK() && after.Value == "2", "and the reader then reads the committed row")
	lab.StopChildren()
}

// recipeUnderContention runs Part E: two processes alternate recipe steps 6-15
// using BEGIN IMMEDIATE and a 120s busy_timeout; each waiter sends BEGIN IMMEDIATE
// while the holder still owns WAL_WRITE_LOCK so contention is verified by StillWaiting.
func recipeUnderContention(ctx context.Context, lab *recorder, dir string) {
	part = "E"
	r := recipe.Default()
	db := filepath.Join(dir, "e-recipe", "r.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(db); err != nil {
		lab.Fatal("remove", err)
	}
	if err := r.Build(ctx, db, recipeBase); err != nil {
		lab.Fatal("build", err)
	}
	options := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
	tables.settings.Row(part, options.JournalMode, longTimeout.Milliseconds())
	writers := []*procs.Child{lab.SpawnSQL("writer-1", options), lab.SpawnSQL("writer-2", options)}
	allWaited, allCommitted := true, true
	for k := recipeBase + 1; k < recipeBase+recipeWrites; k += 2 {
		holder, waiter := writers[0], writers[1]
		lab.Must(holder, "begin", "immediate")
		lab.Must(holder, "recipe-exec", strconv.Itoa(k))
		call := lab.Begin(waiter, "begin", "immediate")
		allWaited = lab.StillWaiting(call) && allWaited
		allCommitted = lab.Do(holder, "commit").OK() && allCommitted
		allCommitted = lab.Finish(call).OK() && allCommitted
		allCommitted = lab.Do(waiter, "recipe-exec", strconv.Itoa(k+1)).OK() && allCommitted
		allCommitted = lab.Do(waiter, "commit").OK() && allCommitted
		writers[0], writers[1] = writers[1], writers[0]
	}
	last := lab.Must(writers[0], "user-version")
	lab.Check(allWaited && allCommitted, "every waiting BEGIN IMMEDIATE was still waiting while the other writer held the lock, and every transaction committed")
	lab.StopChildren()
	match, err := r.MatchStep(ctx, db)
	if err != nil {
		lab.Fatal("match", err)
	}
	var matched any
	if match.Found() {
		matched = match.Step
	}
	tables.recipeResult.Row(part, last.Value, matched, match.Exact)
	lab.Check(last.Value == strconv.Itoa(recipeBase+recipeWrites) && match.Found() && match.Step == recipeBase+recipeWrites,
		fmt.Sprintf("the final database equals the recipe's state after step %d", recipeBase+recipeWrites))
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
	dir := sqlitex.MustWorkDir("6.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	lab := newRecorder()
	lab.Script.Timeout = stepDeadline
	readersAndWriter(ctx, lab, dir)
	writersWithoutTimeout(ctx, lab, dir)
	writersWithTimeout(ctx, lab, dir)
	rollbackContrast(ctx, lab, dir)
	recipeUnderContention(ctx, lab, dir)
	set.Finish("BUSY RULES")
}

// Command 6.3-torn-copy demonstrates why plain file copies of a live SQLite
// database are unsafe: in WAL mode, copying only the main .db file silently
// drops uncheckpointed commits (Part A1), and copying .db across an active
// checkpoint produces a torn, corrupt file (Part A2); in rollback mode, copying
// .db across a live commit tears the copy the same way (Part B).
//
// It prints only its verdict. The steps and what each copy holds go to CSV
// tables that report.sql presents: go run ./cmd/course run 6.3
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const (
	baseStep       = 5
	checkpointedAt = 8
	lastCommitted  = 10
	pageSize       = 4096
)

var describeQueries = []string{
	"PRAGMA user_version",
	"SELECT (SELECT count(*) FROM accounts) || ' accounts, ' || (SELECT count(*) FROM events) || ' events, ' || (SELECT count(*) FROM documents) || ' documents'",
}

// Every step and every examined copy goes to a CSV table in <work dir>/results/;
// part labels the rows written while a part runs.
var (
	set    *results.Set
	part   string
	tables struct {
		steps, copies, integrityProblems, infoRun *results.Table
	}
)

func openTables() {
	tables.steps = set.Table("steps", "part", "step", "process", "action", "result", "code", "value")
	tables.copies = set.Table("copies", "part", "copy", "bytes", "pages", "open_error", "integrity", "user_version", "contents", "matched_step", "exact")
	tables.integrityProblems = set.Table("integrity_problems", "part", "copy", "n", "problem")
	tables.infoRun = set.Table("info_run", "work_dir", "db_bytes_at_copy")
}

// newLab returns a quiet procs.Lab whose hooks write each step and claim to
// the tables.
func newLab() *procs.Lab {
	lab := procs.NewQuietLab(os.Stdout)
	lab.OnStep = func(e procs.StepEvent) {
		r := e.Result
		tables.steps.Row(part, r.Step, e.Child.Name, e.Action, r.CodeName(), r.Code, r.Value)
	}
	lab.OnCheck = func(ok bool, claim string) { set.Check(part, ok, claim) }
	return lab
}

type copyReport struct {
	name       string
	bytes      int64
	match      recipe.Match
	inspection procs.Inspection
}

// examine verifies that inspecting path does not modify it, matches path against
// the recipe's known states, and records its size, integrity_check result, and counts.
func examine(ctx context.Context, lab *procs.Lab, r recipe.Recipe, path, journal string) copyReport {
	info, err := os.Stat(path)
	if err != nil {
		lab.Fatal("stat", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		lab.Fatal("match", err)
	}
	before, err := recipe.HashFile(path)
	if err != nil {
		lab.Fatal("hash", err)
	}
	inspection, err := procs.InspectDatabase(ctx, path, journal, describeQueries...)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	after, err := recipe.HashFile(path)
	if err != nil {
		lab.Fatal("hash", err)
	}
	if before != after {
		lab.Fatal("inspect", fmt.Errorf("opening %s changed it", filepath.Base(path)))
	}
	report := copyReport{name: filepath.Base(path), bytes: info.Size(), match: match, inspection: inspection}
	problems := procs.IntegrityProblems(inspection.Integrity)
	integrity := inspection.Integrity
	if len(problems) > 0 {
		integrity = fmt.Sprintf("%d problem(s)", len(problems))
	}
	var openError, matched any
	if inspection.OpenError != "" {
		openError = inspection.OpenError
	}
	if match.Found() {
		matched = match.Step
	}
	tables.copies.Row(part, report.name, report.bytes, report.bytes/pageSize, openError, integrity, inspection.Values[0], inspection.Values[1], matched, match.Exact)
	for i, problem := range problems {
		tables.integrityProblems.Row(part, report.name, i+1, problem)
	}
	return report
}

func prepare(ctx context.Context, lab *procs.Lab, r recipe.Recipe, dir, name, journal string) (string, string) {
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	db := filepath.Join(root, "r.db")
	if err := r.Build(ctx, db, baseStep); err != nil {
		lab.Fatal("build", err)
	}
	return db, root
}

// walMode runs Part A: commits steps 6-10 with a PASSIVE checkpoint after step 8,
// inspects a main-file-only copy (A1: valid step 8, commits 9-10 silently lost),
// and splits a copy across a second checkpoint (A2: torn and corrupt).
func walMode(ctx context.Context, lab *procs.Lab, dir string) {
	part = "A"
	r := recipe.Default()
	db, root := prepare(ctx, lab, r, dir, "wal", sqlitex.JournalWAL)
	writer := lab.SpawnSQL("writer", procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL})
	copier := lab.Spawn("copier", procs.RoleCopier, "-db", db, "-out", root)
	for k := baseStep + 1; k <= checkpointedAt; k++ {
		lab.Must(writer, "recipe-step", strconv.Itoa(k))
	}
	lab.Must(writer, "checkpoint", sqlitex.CheckpointPassive)
	for k := checkpointedAt + 1; k <= lastCommitted; k++ {
		lab.Must(writer, "recipe-step", strconv.Itoa(k))
	}
	sizes := lab.Must(copier, "copy", "db", "main-only.db")
	lab.Must(copier, "copy", "-wal", "wal-at-copy")
	part = "A1"
	mainOnly := examine(ctx, lab, r, filepath.Join(root, "main-only.db"), sqlitex.JournalWAL)
	lab.Check(mainOnly.inspection.OpenError == "" && mainOnly.inspection.Integrity == "ok" && mainOnly.inspection.Values[0] == strconv.Itoa(checkpointedAt),
		"the copy of the database file without its -wal opens cleanly and passes integrity_check")
	lab.Check(mainOnly.match.Found() && mainOnly.match.Step == checkpointedAt,
		fmt.Sprintf("but it equals recipe step %d, the last checkpoint, not step %d, the last commit: commits %d-%d are silently missing", checkpointedAt, lastCommitted, checkpointedAt+1, lastCommitted))
	tables.infoRun.Row(sqlitex.DisplayPath(dir), sizes.Value)

	part = "A2"
	half := lab.Must(writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size")
	lab.Must(copier, "copy-range", "db", "torn.db", "0", half.Value)
	result := lab.Must(writer, "checkpoint", sqlitex.CheckpointPassive)
	lab.Check(result.Value == "busy=0 log=40 checkpointed=40", "the checkpoint copied every WAL frame (steps 9-10) into the database file")
	lab.Must(copier, "copy-range", "db", "torn.db", half.Value, "end")
	torn := examine(ctx, lab, r, filepath.Join(root, "torn.db"), sqlitex.JournalWAL)
	lab.Check(torn.inspection.Integrity != "ok" || !torn.match.Found(), "the torn copy fails integrity_check or equals no recipe state")
	lab.StopChildren()
}

// rollbackMode runs Part B: copies the first half of a rollback-mode database,
// commits recipe step 6, copies the second half, and verifies the torn copy fails.
func rollbackMode(ctx context.Context, lab *procs.Lab, dir string) {
	part = "B"
	r := recipe.Default().WithJournalMode(sqlitex.JournalDelete)
	db, root := prepare(ctx, lab, r, dir, "rollback", sqlitex.JournalDelete)
	writer := lab.SpawnSQL("writer", procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete})
	copier := lab.Spawn("copier", procs.RoleCopier, "-db", db, "-out", root)
	half := lab.Must(writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size")
	lab.Must(copier, "copy-range", "db", "torn.db", "0", half.Value)
	lab.Must(writer, "recipe-step", strconv.Itoa(baseStep+1))
	lab.Must(copier, "copy-range", "db", "torn.db", half.Value, "end")
	lab.StopChildren()
	torn := examine(ctx, lab, r, filepath.Join(root, "torn.db"), sqlitex.JournalDelete)
	lab.Check(torn.inspection.Integrity != "ok" || !torn.match.Found(), "the torn copy fails integrity_check or equals no recipe state")
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	openTables()
	lab := newLab()
	walMode(ctx, lab, dir)
	rollbackMode(ctx, lab, dir)
	set.Finish("TORN COPY")
}

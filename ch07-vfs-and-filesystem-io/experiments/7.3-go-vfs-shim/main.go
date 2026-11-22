// Experiment 7.3 registers the pure-Go forwarding VFS shim (internal/govfs) on
// top of modernc.org/sqlite's default VFS. Part A compares the Go driver's
// per-commit VFS call sequence call-for-call against the C CLI's -vfstrace in
// DELETE and WAL modes at NORMAL and FULL, also recording the real SQLITE_SYNC_*
// flag values. Part B fails every Nth xWrite and xSync of a recipe step with
// SQLITE_IOERR in both journal modes and verifies SQLite rolls back cleanly to
// the exact pre-step database bytes.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.3 (it needs the pinned CLI,
// from go run ./cmd/course build-tools).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/govfs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const (
	vfsName    = "course-govfs"
	dbName     = "t.db"
	setupSQL   = "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one')"
	measureSQL = "INSERT INTO t(id, v) VALUES(2, 'two')"
	faultStep  = 4
)

type config struct {
	journal string
	sync    string
}

func (c config) String() string {
	return c.journal + "/" + c.sync
}

var configs = []config{
	{sqlitex.JournalDelete, sqlitex.SyncNormal},
	{sqlitex.JournalDelete, sqlitex.SyncFull},
	{sqlitex.JournalWAL, sqlitex.SyncNormal},
	{sqlitex.JournalWAL, sqlitex.SyncFull},
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.3" runs both.
var (
	set    *results.Set
	tables struct {
		aCalls, bSteps, bCleanCalls, bTrials *results.Table
	}
)

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	cli := sqlitex.MustLocateCLI(ctx)
	v, err := govfs.Register(vfsName, govfs.Options{})
	if err != nil {
		fatal("register", err)
	}
	set = results.MustOpen(dir)
	set.Table("vfs", "name", "io_version", "forwards_to").Row(v.Name(), v.IOVersion(), "the default VFS (self-check passed)")
	tables.aCalls = set.Table("a_calls", "journal", "sync", "seq", "method", "call", "sync_flags", "vfstrace_label", "go_key", "cli_method", "cli_key")
	tables.bSteps = set.Table("b_steps", "journal", "synchronous", "seed", "base_step", "base_bytes", "step", "summary", "marker_id")
	tables.bCleanCalls = set.Table("b_clean_calls", "journal", "seq", "method", "file", "amount", "call")
	tables.bTrials = set.Table("b_trials", "journal", "method", "n", "fired", "target", "step_error", "step_code", "error_at", "close_error", "user_version", "marker_row", "integrity", "file_state", "held")

	for _, c := range configs {
		partA(ctx, cli, v, dir, c)
	}
	for _, journal := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		partB(ctx, v, dir, journal)
	}
	if err := v.Err(); err != nil {
		set.Check("B", false, "the Go VFS reported no internal error: "+err.Error())
	}
	set.Finish("GO VFS SHIM")
}

func goOptions(c config) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = c.journal
	o.Conn.Synchronous = c.sync
	o.SingleConnection = true
	o.VFS = vfsName
	return o
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func cliPrelude(o sqlitex.Options) string {
	f, c := o.File, o.Conn
	return fmt.Sprintf("PRAGMA page_size=%d;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\nPRAGMA journal_mode=%s;\nPRAGMA application_id=%d;\nPRAGMA user_version=%d;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		f.PageSize, f.AutoVacuum, f.Encoding, f.JournalMode, f.ApplicationID, f.UserVersion, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete), c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

// goKey normalizes a recorded govfs.Call to the same comparison tuple produced
// by cliKey for CLI -vfstrace output, including vfstrace's sync flag label
// (vfstrace.c:453-458).
func goKey(c govfs.Call) string {
	switch c.Method {
	case "xRead", "xWrite":
		return fmt.Sprintf("%s %s n=%d ofst=%d", c.Method, c.File, c.Amount, c.Offset)
	case "xSync":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.VFSTraceSyncLabel(c.Flags))
	case "xLock", "xUnlock":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.LockLevelString(c.Flags))
	case "xFileControl":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.FileControlName(c.Flags))
	case "xShmLock":
		return fmt.Sprintf("%s %s ofst=%d n=%d %s", c.Method, c.File, c.Offset, c.Amount, govfs.ShmLockFlagsString(c.Flags))
	case "xShmMap":
		return fmt.Sprintf("%s %s region=%d size=%d write=%d", c.Method, c.File, c.Offset, c.Amount, c.Flags)
	case "xTruncate":
		return fmt.Sprintf("%s %s %d", c.Method, c.File, c.Amount)
	case "xOpen":
		return fmt.Sprintf("%s %s flags=0x%x", c.Method, c.File, c.Flags)
	case "xDelete", "xAccess":
		return fmt.Sprintf("%s %s %d", c.Method, c.File, c.Flags)
	case "xShmUnmap":
		return fmt.Sprintf("%s %s delete=%d", c.Method, c.File, c.Flags)
	}
	return c.Method + " " + c.File
}

func argValue(arg string) string {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[i+1:]
	}
	return arg
}

func cliKey(c trace.VFSCall) string {
	file := filepath.Base(strings.Trim(c.File, `"`))
	arg := func(i int) string {
		if i < len(c.Args) {
			return argValue(c.Args[i])
		}
		return ""
	}
	switch c.Method {
	case "xRead", "xWrite":
		return fmt.Sprintf("%s %s n=%s ofst=%s", c.Method, file, arg(1), arg(2))
	case "xSync", "xLock", "xUnlock":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xFileControl":
		op := arg(1)
		if i := strings.IndexAny(op, ",["); i >= 0 {
			op = op[:i]
		}
		return fmt.Sprintf("%s %s %s", c.Method, file, op)
	case "xShmLock":
		ofst := arg(1)
		if i := strings.IndexByte(ofst, '('); i >= 0 {
			ofst = ofst[:i]
		}
		return fmt.Sprintf("%s %s ofst=%s n=%s %s", c.Method, file, ofst, arg(2), arg(3))
	case "xShmMap":
		return fmt.Sprintf("%s %s region=%s size=%s write=%s", c.Method, file, arg(1), arg(2), arg(3))
	case "xTruncate":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xOpen":
		return fmt.Sprintf("%s %s flags=%s", c.Method, file, arg(1))
	case "xDelete", "xAccess":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xShmUnmap":
		return fmt.Sprintf("%s %s delete=%s", c.Method, file, arg(1))
	case "xRandomness", "xCurrentTime", "xCurrentTimeInt64", "xSleep":
		return c.Method + " "
	}
	return c.Method + " " + file
}

func runCLI(ctx context.Context, cli sqlitex.CLI, dir string, c config) ([]trace.VFSCall, error) {
	path := filepath.Join(dir, "cli-"+strings.ToLower(c.journal+"-"+c.sync)+".db")
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	script := cliPrelude(goOptions(c)) + setupSQL + ";\n.print MARK-BEGIN\n" + measureSQL + ";\n.print MARK-END\n"
	result, err := cli.Run(ctx, sqlitex.CLIRun{Args: []string{"-vfstrace"}, Stdin: strings.NewReader(sqlitex.OpenScript(path, script)), Dir: dir})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("cli exit %d: %s", result.ExitCode, result.Stderr)
	}
	var calls []trace.VFSCall
	inside := false
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		switch line {
		case "MARK-BEGIN":
			inside = true
			continue
		case "MARK-END":
			inside = false
			continue
		}
		if !inside {
			continue
		}
		call, ok := trace.ParseVFSLine(line)
		if ok {
			calls = append(calls, call)
		}
	}
	return trace.RelativeVFS(dir, calls), nil
}

func runGo(ctx context.Context, v *govfs.VFS, dir string, c config) ([]govfs.Call, error) {
	path := filepath.Join(dir, "go-"+strings.ToLower(c.journal+"-"+c.sync)+".db")
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	db, err := sqlitex.Open(ctx, path, goOptions(c))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, setupSQL); err != nil {
		return nil, err
	}
	v.Reset()
	if _, err := db.ExecContext(ctx, measureSQL); err != nil {
		return nil, err
	}
	calls := v.TakeCalls()
	return calls, nil
}

func renameFiles(calls []govfs.Call, from, to string) []govfs.Call {
	out := make([]govfs.Call, len(calls))
	for i, c := range calls {
		c.File = strings.Replace(c.File, from, to, 1)
		out[i] = c
	}
	return out
}

func partA(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS, dir string, c config) {
	goCalls, err := runGo(ctx, v, dir, c)
	if err != nil {
		fatal("go "+c.String(), err)
	}
	cliCalls, err := runCLI(ctx, cli, dir, c)
	if err != nil {
		fatal("cli "+c.String(), err)
	}
	name := "go-" + strings.ToLower(c.journal+"-"+c.sync) + ".db"
	goCalls = renameFiles(goCalls, name, dbName)
	cliName := "cli-" + strings.ToLower(c.journal+"-"+c.sync) + ".db"
	goKeys := make([]string, len(goCalls))
	for i, call := range goCalls {
		goKeys[i] = goKey(call)
	}
	cliKeys := make([]string, len(cliCalls))
	for i, call := range cliCalls {
		cliKeys[i] = strings.Replace(cliKey(call), cliName, dbName, 1)
	}
	for i := range max(len(goCalls), len(cliCalls)) {
		var method, text, flags, label, goK, cliMethod, cliK any
		if i < len(goCalls) {
			call := goCalls[i]
			method, text, goK = call.Method, call.String(), goKeys[i]
			if call.Method == "xSync" {
				flags = fmt.Sprintf("0x%x(%s)", call.Flags, govfs.SyncFlagsString(call.Flags))
				label = govfs.VFSTraceSyncLabel(call.Flags)
			}
		}
		if i < len(cliCalls) {
			cliMethod, cliK = cliCalls[i].Method, cliKeys[i]
		}
		tables.aCalls.Row(c.journal, c.sync, i+1, method, text, flags, label, goK, cliMethod, cliK)
	}
	same := len(goKeys) == len(cliKeys)
	for i := range min(len(goKeys), len(cliKeys)) {
		if goKeys[i] != cliKeys[i] {
			same = false
			break
		}
	}
	goCounts := govfs.CountCalls(goCalls)
	cliCounts := govfs.Counts{}
	for _, call := range cliCalls {
		cliCounts[call.Method]++
	}
	set.Check("A", same && len(goCalls) > 0 && goCounts.String() == cliCounts.String(), fmt.Sprintf("%s: the Go driver makes the same %d VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI", c, len(goCalls)))
}

type outcome struct {
	stepErr     string
	stepCode    int
	where       string
	closeErr    string
	userVersion int
	rowPresent  bool
	integrity   string
	match       recipe.Match
}

// rolledBack reports whether a faulted trial returned primary SQLITE_IOERR,
// closed cleanly, and left the database byte-identical to step faultStep-1.
func (o outcome) rolledBack() bool {
	return o.stepCode == int(govfs.CodeIOErr) && o.closeErr == "" && !o.rowPresent && o.integrity == "ok" && o.match.Step == faultStep-1 && o.match.Exact && o.userVersion == faultStep-1
}

// committed reports whether an unreached-fault control trial committed cleanly
// and left the database byte-identical to step faultStep.
func (o outcome) committed() bool {
	return o.stepErr == "" && o.closeErr == "" && o.rowPresent && o.match.Step == faultStep && o.match.Exact
}

// trialHeld enforces that faults armed within 1..total actually fired and rolled
// back, while the N=total+1 control did not fire and committed normally.
func trialHeld(n, total int, fired bool, o outcome) bool {
	if n <= total {
		return fired && o.rolledBack()
	}
	return !fired && o.committed()
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func errorText(err error) string {
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("SQLite error code %d (%s)", code, strings.TrimSpace(err.Error()))
	}
	return err.Error()
}

// applyThroughVFS runs one recipe step inside a single transaction through the
// shim VFS with an optional armed fault, disarms and drains recorded calls, and
// closes the connection before returning so the caller can safely reopen and
// inspect the file.
func applyThroughVFS(ctx context.Context, r recipe.Recipe, path string, step recipe.Step, v *govfs.VFS, fault *govfs.Fault) (stepErr error, where string, report govfs.FaultReport, calls []govfs.Call, closeErr error) {
	o := r.SQLiteOptions()
	o.VFS = vfsName
	db, err := sqlitex.Open(ctx, path, o)
	if err != nil {
		fatal("open through the Go VFS", err)
	}
	v.Reset()
	if fault != nil {
		if err := v.Arm(*fault); err != nil {
			fatal("arm", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		stepErr, where = err, "BEGIN"
	} else {
		for i, s := range step.Statements {
			if _, err := tx.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				stepErr, where = err, fmt.Sprintf("statement %d/%d (%s)", i+1, len(step.Statements), firstWords(s.SQL))
				break
			}
		}
		if stepErr == nil {
			if err := tx.Commit(); err != nil {
				stepErr, where = err, "COMMIT"
			}
		} else {
			tx.Rollback()
		}
	}
	report = v.Disarm()
	calls = v.TakeCalls()
	closeErr = db.Close()
	return stepErr, where, report, calls, closeErr
}

func firstWords(sql string) string {
	if i := strings.IndexByte(sql, '('); i >= 0 {
		sql = sql[:i]
	}
	fields := strings.Fields(sql)
	if len(fields) > 3 {
		fields = fields[:3]
	}
	return strings.Join(fields, " ")
}

func markerEventID(step recipe.Step) int64 {
	for _, s := range step.Statements {
		if strings.HasPrefix(s.SQL, "INSERT INTO events") {
			switch id := s.Args[0].(type) {
			case int:
				return int64(id)
			case int64:
				return id
			}
		}
	}
	return -1
}

func inspect(ctx context.Context, r recipe.Recipe, path string, markerID int64) outcome {
	var o outcome
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("reopen", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&o.userVersion); err != nil {
		fatal("user_version", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE id = ?", markerID).Scan(&n); err != nil {
		fatal("marker row", err)
	}
	o.rowPresent = n > 0
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&o.integrity); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close after inspect", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	o.match = match
	return o
}

func partB(ctx context.Context, v *govfs.VFS, dir, journal string) {
	r := recipe.Default().WithJournalMode(journal).WithSteps(faultStep + 1)
	if _, err := r.StepHashes(ctx); err != nil {
		fatal("step hashes", err)
	}
	base := filepath.Join(dir, "base-"+journal+".db")
	if err := r.Build(ctx, base, faultStep-1); err != nil {
		fatal("build", err)
	}
	step, err := r.Step(faultStep)
	if err != nil {
		fatal("step", err)
	}
	markerID := markerEventID(step)
	baseInfo, err := os.Stat(base)
	if err != nil {
		fatal("stat base", err)
	}
	tables.bSteps.Row(journal, r.SQLiteOptions().Conn.Synchronous, r.Seed, faultStep-1, baseInfo.Size(), faultStep, step.Summary(), markerID)

	clean := filepath.Join(dir, "clean-"+journal+".db")
	if err := copyFile(base, clean); err != nil {
		fatal("copy", err)
	}
	stepErr, _, _, calls, closeErr := applyThroughVFS(ctx, r, clean, step, v, nil)
	if stepErr != nil || closeErr != nil {
		fatal("clean run", fmt.Errorf("%v / %v", stepErr, closeErr))
	}
	cleanOutcome := inspect(ctx, r, clean, markerID)
	for _, call := range calls {
		call.File = strings.Replace(call.File, "clean-"+journal+".db", dbName, 1)
		tables.bCleanCalls.Row(journal, call.Seq, call.Method, call.File, call.Amount, call.String())
	}
	cleanHeld := cleanOutcome.match.Step == faultStep && cleanOutcome.match.Exact && cleanOutcome.rowPresent
	trialRow(journal, "none", 0, false, nil, cleanOutcome, cleanHeld)
	set.Check("B", cleanHeld, fmt.Sprintf("%s: without a fault the step commits and the file equals recipe step %d exactly", journal, faultStep))

	for _, method := range []string{"xWrite", "xSync"} {
		total := len(govfs.Filter(calls, govfs.Methods(method)))
		allRolledBack := true
		var summary []string
		for n := 1; n <= total+1; n++ {
			trial := filepath.Join(dir, fmt.Sprintf("fault-%s-%s-%03d.db", journal, method, n))
			if err := copyFile(base, trial); err != nil {
				fatal("copy", err)
			}
			fault := govfs.Fault{Method: method, Nth: n, Code: govfs.CodeIOErr}
			stepErr, where, report, _, closeErr := applyThroughVFS(ctx, r, trial, step, v, &fault)
			o := inspect(ctx, r, trial, markerID)
			if stepErr != nil {
				o.stepErr, o.where = errorText(stepErr), where
				o.stepCode, _ = sqlitex.PrimaryErrorCode(stepErr)
			}
			if closeErr != nil {
				o.closeErr = errorText(closeErr)
			}
			var target any
			if report.Fired {
				target = strings.Replace(report.Call.String(), filepath.Base(trial), dbName, 1)
			}
			held := trialHeld(n, total, report.Fired, o)
			trialRow(journal, method, n, report.Fired, target, o, held)
			if n <= total {
				if !held {
					allRolledBack = false
					summary = append(summary, fmt.Sprintf("%s #%d", method, n))
				}
			} else {
				set.Check("B", held, fmt.Sprintf("%s: with %s #%d never reached, the step commits normally", journal, method, n))
			}
			if err := recipe.RemoveDatabase(trial); err != nil {
				fatal("remove", err)
			}
		}
		claim := fmt.Sprintf("%s: for each of the %d %s calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step %d)", journal, total, method, faultStep-1)
		if !allRolledBack {
			claim += "; exceptions: " + strings.Join(summary, ", ")
		}
		set.Check("B", allRolledBack && total > 0, claim)
	}
}

// trialRow writes one run of the fault step: method "none" is the run
// without a fault.
func trialRow(journal, method string, n int, fired bool, target any, o outcome, held bool) {
	var stepErr, code, where, closeErr any
	if o.stepErr != "" {
		stepErr, code, where = o.stepErr, o.stepCode, o.where
	}
	if o.closeErr != "" {
		closeErr = o.closeErr
	}
	row := "absent"
	if o.rowPresent {
		row = "present"
	}
	tables.bTrials.Row(journal, method, n, fired, target, stepErr, code, where, closeErr, o.userVersion, row, o.integrity, o.match.String(), held)
}

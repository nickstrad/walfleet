package procs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

var heldLock *os.File

var testRoles = map[string]Role{
	RoleSQL:    SQLRole,
	RoleCopier: CopierRole,
	"echo": func(ctx context.Context, args []string) (*Actor, error) {
		return &Actor{Actions: map[string]Handler{
			"echo": func(ctx context.Context, args []string) (string, error) { return strings.Join(args, "|"), nil },
			"fail": func(ctx context.Context, args []string) (string, error) { return "", errors.New("asked to fail") },
			"crash": func(ctx context.Context, args []string) (string, error) {
				os.Exit(7)
				return "", nil
			},
			"sleep": func(ctx context.Context, args []string) (string, error) {
				d, err := time.ParseDuration(args[0])
				if err != nil {
					return "", err
				}
				time.Sleep(d)
				return "slept", nil
			},
			"lie": func(ctx context.Context, args []string) (string, error) {
				line, _ := json.Marshal(Result{Step: 999, Action: "other"})
				os.NewFile(replyFD, "replies").Write(append(line, '\n'))
				return "", nil
			},
			"lock": func(ctx context.Context, args []string) (string, error) {
				file, err := os.OpenFile(args[0], os.O_RDWR, 0)
				if err != nil {
					return "", err
				}
				heldLock = file
				lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 120, Len: 1}
				return "", syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock)
			},
			"stray-close": func(ctx context.Context, args []string) (string, error) {
				file, err := os.Open(args[0])
				if err != nil {
					return "", err
				}
				return "", file.Close()
			},
		}, Close: func() error {
			if len(args) > 0 && args[0] == "close-fails" {
				return errors.New("close failed on purpose")
			}
			return nil
		}}, nil
	},
	"refuse": func(ctx context.Context, args []string) (*Actor, error) {
		return nil, errors.New("this role never starts")
	},
}

// TestMain dispatches re-executed test binaries into child-role mode when ChildMarker is present.
func TestMain(m *testing.M) {
	if Serve(testRoles) {
		return
	}
	os.Exit(m.Run())
}

// TestResultCodeNames checks SQLite result code naming and Result.Outcome formatting.
func TestResultCodeNames(t *testing.T) {
	cases := map[int]string{0: "SQLITE_OK", 5: "SQLITE_BUSY", 517: "SQLITE_BUSY_SNAPSHOT", 261: "SQLITE_BUSY_RECOVERY", 15: "SQLITE_PROTOCOL", 5898: "SQLITE_IOERR_DELETE_NOENT", 1290: "SQLITE_IOERR+extended(5)", 9999: "SQLITE_PROTOCOL+extended(39)", 200: "SQLITE_code_200"}
	for code, want := range cases {
		if got := sqlitex.ResultCodeName(code); got != want {
			t.Errorf("ResultCodeName(%d) = %q, want %q", code, got, want)
		}
	}
	if got := (Result{Code: 517, Error: "x"}).Outcome(); got != "SQLITE_BUSY_SNAPSHOT (517)" {
		t.Errorf("Outcome = %q", got)
	}
	if got := (Result{Value: "3"}).Outcome(); got != "SQLITE_OK 3" {
		t.Errorf("Outcome = %q", got)
	}
	if got := (Result{Code: CodeNotSQLite, Error: "boom"}).Outcome(); got != "ERROR boom" {
		t.Errorf("Outcome = %q", got)
	}
}

// TestResultForNonSQLiteError confirms plain Go errors map to CodeNotSQLite.
func TestResultForNonSQLiteError(t *testing.T) {
	r := ResultFor(Request{Step: 4, Action: "a"}, "", errors.New("plain"))
	if r.Code != CodeNotSQLite || r.OK() || r.Step != 4 || r.Action != "a" {
		t.Fatalf("got %+v", r)
	}
}

func runChildInProcess(t *testing.T, role string, requests string) ([]Result, error) {
	t.Helper()
	var out bytes.Buffer
	err := RunChild(context.Background(), testRoles, role, nil, strings.NewReader(requests), &out)
	var results []Result
	decoder := json.NewDecoder(&out)
	for {
		var r Result
		if decodeErr := decoder.Decode(&r); decodeErr == io.EOF {
			break
		} else if decodeErr != nil {
			t.Fatalf("decode: %v", decodeErr)
		}
		results = append(results, r)
	}
	return results, err
}

// TestRunChildProtocol exercises ready, echo, failing, unknown, and exit actions in-process.
func TestRunChildProtocol(t *testing.T) {
	results, err := runChildInProcess(t, "echo", `{"step":1,"action":"echo","args":["a","b"]}
{"step":2,"action":"fail"}
{"step":3,"action":"nope"}
{"step":-1,"action":"exit"}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 || results[0].Action != ActionReady {
		t.Fatalf("results %+v", results)
	}
	if results[1].Value != "a|b" || !results[1].OK() {
		t.Errorf("echo: %+v", results[1])
	}
	if results[2].OK() || results[2].Code != CodeNotSQLite || results[2].Error != "asked to fail" {
		t.Errorf("fail: %+v", results[2])
	}
	if results[3].OK() || !strings.Contains(results[3].Error, "no action") {
		t.Errorf("unknown action: %+v", results[3])
	}
	if results[4].Step != -1 || !results[4].OK() {
		t.Errorf("exit: %+v", results[4])
	}
}

// TestRunChildErrors verifies RunChild fails loudly on unknown roles, startup errors, bad JSON, and premature EOF.
func TestRunChildErrors(t *testing.T) {
	if _, err := runChildInProcess(t, "missing", ""); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("unknown role: %v", err)
	}
	if _, err := runChildInProcess(t, "refuse", ""); err == nil {
		t.Error("a role that fails to start must be an error")
	}
	if _, err := runChildInProcess(t, "echo", "not json\n"); err == nil {
		t.Error("malformed request accepted")
	}
	if _, err := runChildInProcess(t, "echo", `{"step":1,"action":"echo"}`+"\n"); err == nil {
		t.Error("request pipe closed without exit must be an error")
	}
}

// TestChildRoundTripAndStop tests spawning a real child, running a step, rejecting step 0, and ErrStopped after Stop.
func TestChildRoundTripAndStop(t *testing.T) {
	child, err := StartChild("one", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if child.Pid <= 0 || child.Pid == os.Getpid() {
		t.Fatalf("pid %d", child.Pid)
	}
	result, err := child.Do(1, "echo", "x")
	if err != nil || result.Value != "x" {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := child.Do(0, "echo"); err == nil {
		t.Error("step 0 accepted")
	}
	if err := child.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Do(2, "echo"); !errors.Is(err, ErrStopped) {
		t.Errorf("after stop: %v", err)
	}
}

// TestChildCloseErrorIsReported checks that an Actor.Close error is returned by Child.Stop.
func TestChildCloseErrorIsReported(t *testing.T) {
	child, err := StartChild("closer", "echo", "close-fails")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Stop(); err == nil || !strings.Contains(err.Error(), "close failed on purpose") {
		t.Fatalf("Stop = %v", err)
	}
}

// TestChildThatDiesIsReported confirms a child exiting mid-step returns ErrChildDied with its exit status.
func TestChildThatDiesIsReported(t *testing.T) {
	child, err := StartChild("crasher", "echo")
	if err != nil {
		t.Fatal(err)
	}
	_, err = child.Do(1, "crash")
	if !errors.Is(err, ErrChildDied) || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("crash: %v", err)
	}
}

// TestRoleThatFailsToStart verifies startup errors surface through ErrChildDied with child stderr.
func TestRoleThatFailsToStart(t *testing.T) {
	_, err := StartChild("refuser", "refuse")
	if !errors.Is(err, ErrChildDied) || !strings.Contains(err.Error(), "this role never starts") {
		t.Fatalf("start: %v", err)
	}
}

// TestChildTimeoutKillsIt checks that a hung child triggers ErrTimeout and is killed and reaped.
func TestChildTimeoutKillsIt(t *testing.T) {
	child, err := StartChild("sleeper", "echo")
	if err != nil {
		t.Fatal(err)
	}
	child.Timeout = 200 * time.Millisecond
	_, err = child.Do(1, "sleep", "1h")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: %v", err)
	}
	select {
	case <-child.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("child still running after the timeout")
	}
}

// TestAsyncCallAndOneOutstandingRequest verifies non-blocking Answered polling and ErrBusyChild enforcement.
func TestAsyncCallAndOneOutstandingRequest(t *testing.T) {
	child, err := StartChild("async", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	call, err := child.Start(1, "sleep", "300ms")
	if err != nil {
		t.Fatal(err)
	}
	if answered, err := call.Answered(); answered || err != nil {
		t.Fatalf("answered %v err %v right after sending", answered, err)
	}
	if _, err := child.Start(2, "echo"); !errors.Is(err, ErrBusyChild) {
		t.Fatalf("second request: %v", err)
	}
	result, err := call.Wait()
	if err != nil || result.Value != "slept" {
		t.Fatalf("%+v %v", result, err)
	}
	if answered, err := call.Answered(); !answered || err != nil {
		t.Fatalf("after Wait: %v %v", answered, err)
	}
	if err := child.Stop(); err != nil {
		t.Fatal(err)
	}
}

// TestStopWithOutstandingRequestFails confirms Stop kills the child and returns ErrBusyChild when a call is pending.
func TestStopWithOutstandingRequestFails(t *testing.T) {
	child, err := StartChild("pending", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Start(1, "sleep", "1h"); err != nil {
		t.Fatal(err)
	}
	if err := child.Stop(); !errors.Is(err, ErrBusyChild) {
		t.Fatalf("Stop = %v", err)
	}
}

// TestMismatchedReplyIsAnError verifies a child replying with the wrong step or action triggers ErrStepMismatch.
func TestMismatchedReplyIsAnError(t *testing.T) {
	child, err := StartChild("liar", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	if _, err := child.Do(1, "lie"); !errors.Is(err, ErrStepMismatch) {
		t.Fatalf("lie: %v", err)
	}
}

// TestScriptNumbersStepsAndPrints checks duplicate name rejection, NameOfPid lookup, and transcript formatting.
func TestScriptNumbersStepsAndPrints(t *testing.T) {
	var out bytes.Buffer
	script := NewScript(&out)
	child, err := script.Spawn("a", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.Spawn("a", "echo"); err == nil {
		t.Error("duplicate child name accepted")
	}
	if _, err := script.Do(child, "echo", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := script.Do(child, "fail"); err != nil {
		t.Fatal(err)
	}
	if name, ok := script.NameOfPid(child.Pid); !ok || name != "a" {
		t.Errorf("NameOfPid = %q %v", name, ok)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	want := "step  1  a          echo 1                               -> SQLITE_OK 1\nstep  2  a          fail                                 -> ERROR asked to fail\n"
	if out.String() != want {
		t.Fatalf("transcript:\n%q\nwant\n%q", out.String(), want)
	}
}

// TestLabCheckAndReport verifies Lab.Check tracking and Lab.Report exit status on pass vs failure.
func TestLabCheckAndReport(t *testing.T) {
	var out bytes.Buffer
	lab := NewLab(&out)
	exitCode := -1
	lab.exit = func(code int) { exitCode = code }
	lab.Check(true, "yes")
	lab.Report("T")
	if exitCode != -1 || !strings.Contains(out.String(), "T OK") {
		t.Fatalf("passing report: %q exit %d", out.String(), exitCode)
	}
	lab.Check(false, "no")
	lab.Report("T")
	if exitCode != 1 || !strings.Contains(out.String(), "T FAILED: 1 claim(s)") || len(lab.Failures()) != 1 {
		t.Fatalf("failing report: %q exit %d", out.String(), exitCode)
	}
}

// TestQuietLabHandsStepsAndChecksToHooks verifies a quiet Lab prints nothing
// but FATAL, and that OnStep and OnCheck see every step, poll and claim.
func TestQuietLabHandsStepsAndChecksToHooks(t *testing.T) {
	var out bytes.Buffer
	lab := NewQuietLab(&out)
	exitCode := -1
	lab.exit = func(code int) { exitCode = code }
	var steps []string
	lab.OnStep = func(e StepEvent) {
		steps = append(steps, fmt.Sprintf("%d %s %s %s polled=%v waiting=%v", e.Result.Step, e.Child.Name, e.Action, e.Result.Outcome(), e.Polled, e.Waiting))
	}
	var checks []string
	lab.OnCheck = func(ok bool, claim string) { checks = append(checks, fmt.Sprintf("%v %s", ok, claim)) }
	child := lab.Spawn("q", "echo")
	lab.Do(child, "fail")
	lab.Must(child, "echo", "a", "b")
	blocked := lab.Begin(child, "sleep", "1s")
	if !lab.StillWaiting(blocked) {
		t.Fatal("a blocked action was reported as answered")
	}
	lab.Finish(blocked)
	unpolled := lab.Begin(child, "echo", "later")
	lab.Finish(unpolled)
	lab.Check(true, "yes")
	lab.Check(false, "no")
	lab.StopChildren()
	if out.Len() != 0 {
		t.Fatalf("quiet lab printed %q", out.String())
	}
	want := []string{
		"1 q fail ERROR asked to fail polled=false waiting=false",
		"2 q echo a b SQLITE_OK a|b polled=false waiting=false",
		"3 q sleep 1s SQLITE_OK slept polled=true waiting=true",
		"4 q echo later SQLITE_OK later polled=false waiting=false",
	}
	if strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s\nwant\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(checks, ",") != "true yes,false no" || len(lab.Failures()) != 1 {
		t.Fatalf("checks %q failures %q", checks, lab.Failures())
	}
	lab.Fatal("boom", errors.New("broken"))
	if exitCode != 1 || out.String() != "FATAL boom: broken\n" {
		t.Fatalf("fatal: %q exit %d", out.String(), exitCode)
	}
}

// TestLabHooksKeepTheTranscript verifies hooks on a printing Lab leave its
// transcript and check lines as they were.
func TestLabHooksKeepTheTranscript(t *testing.T) {
	var out bytes.Buffer
	lab := NewLab(&out)
	var steps, checks int
	lab.OnStep = func(StepEvent) { steps++ }
	lab.OnCheck = func(bool, string) { checks++ }
	child := lab.Spawn("p", "echo")
	lab.Must(child, "echo", "1")
	lab.Finish(lab.Begin(child, "echo", "2"))
	lab.Check(true, "yes")
	lab.StopChildren()
	want := "step  1  p          echo 1                               -> SQLITE_OK 1\n" +
		"step  2  p          echo 2                               .. sent, not waiting for the answer\n" +
		"step  2  p          echo 2                               -> SQLITE_OK 2\n" +
		"check ok: yes\n"
	if out.String() != want || steps != 2 || checks != 1 {
		t.Fatalf("transcript:\n%q\nwant\n%q\nsteps %d checks %d", out.String(), want, steps, checks)
	}
}

// TestQuietLabHandsLocksToOnLocks verifies a quiet Lab prints no lock dump and
// passes the probe, the /proc/locks entries and the child names to OnLocks.
func TestQuietLabHandsLocksToOnLocks(t *testing.T) {
	if !LockProbeSupported {
		t.Skip("no fcntl probe on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "locked")
	if err := os.WriteFile(path, make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	lab := NewQuietLab(&out)
	var events []LocksEvent
	lab.OnLocks = func(e LocksEvent) { events = append(events, e) }
	child := lab.Spawn("locker", "echo")
	lab.Must(child, "lock", path)
	view := lab.Locks("-shm", path, ShmLockRanges())
	lab.StopChildren()
	if out.Len() != 0 {
		t.Fatalf("quiet lab printed %q", out.String())
	}
	if len(events) != 1 {
		t.Fatalf("%d OnLocks calls", len(events))
	}
	e := events[0]
	if e.Step != 1 || e.Label != "-shm" || e.Names[child.Pid] != "locker" || !WriteLockedBy(e.View.Ranges, "WAL_WRITE_LOCK", child.Pid) {
		t.Fatalf("event %+v", e)
	}
	if e.View.ProcListed != ProcLocksSupported || len(e.View.Proc) != len(view.Proc) {
		t.Fatalf("event view %+v, returned view %+v", e.View, view)
	}
	if ProcLocksSupported && (len(e.View.Proc) != 1 || e.View.Proc[0].Pid != child.Pid || e.View.Proc[0].Type != "WRITE") {
		t.Fatalf("/proc/locks entries %+v", e.View.Proc)
	}
}

// TestProbeSeesAnotherProcessLockAndAStrayCloseDropsIt verifies F_GETLK and /proc/locks see a child's lock and its release after stray close.
func TestProbeSeesAnotherProcessLockAndAStrayCloseDropsIt(t *testing.T) {
	if !LockProbeSupported {
		t.Skip("no fcntl probe on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "locked")
	if err := os.WriteFile(path, make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}
	holder, err := ProbeLock(path, 0, 1000)
	if err != nil || holder.Held {
		t.Fatalf("before: %+v %v", holder, err)
	}
	child, err := StartChild("locker", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	if result, err := child.Do(1, "lock", path); err != nil || !result.OK() {
		t.Fatalf("lock: %+v %v", result, err)
	}
	states, err := ProbeRanges(path, ShmLockRanges())
	if err != nil {
		t.Fatal(err)
	}
	if !WriteLockedBy(states, "WAL_WRITE_LOCK", child.Pid) || !Unlocked(states, "WAL_CKPT_LOCK", "DMS") || ReadLocked(states, "WAL_WRITE_LOCK") {
		t.Fatalf("states %+v", states)
	}
	lines := FormatRangeStates(states[:2], func(pid int) string { return "locker" })
	if lines[0] != "WAL_WRITE_LOCK     byte 120         write-locked by locker" || lines[1] != "WAL_CKPT_LOCK      byte 121         unlocked" {
		t.Fatalf("lines %q", lines)
	}
	if ProcLocksSupported {
		all, err := ReadProcLocks()
		if err != nil {
			t.Fatal(err)
		}
		inode, err := FileInode(path)
		if err != nil {
			t.Fatal(err)
		}
		mine := FilterProcLocks(all, inode, map[int]bool{child.Pid: true})
		if len(mine) != 1 || mine[0].Type != "WRITE" || mine[0].Start != 120 || mine[0].End != 120 || mine[0].Class != "POSIX" {
			t.Fatalf("/proc/locks entries %+v", mine)
		}
		described := DescribeProcLocks(mine, map[int]string{child.Pid: "locker"}, ShmLockRanges())
		if len(described) != 1 || !strings.HasSuffix(described[0], "WAL_WRITE_LOCK") {
			t.Fatalf("described %q", described)
		}
	}
	if result, err := child.Do(2, "stray-close", path); err != nil || !result.OK() {
		t.Fatalf("stray close: %+v %v", result, err)
	}
	holder, err = ProbeLock(path, 120, 1)
	if err != nil || holder.Held {
		t.Fatalf("after the stray close: %+v %v", holder, err)
	}
	if _, err := ProbeLock(path, -1, 1); err == nil {
		t.Error("negative start accepted")
	}
	if _, err := ProbeLock(path, 0, 0); err == nil {
		t.Error("empty range accepted")
	}
}

// TestParseProcLocks verifies parsing of POSIX, blocked, FLOCK-to-EOF, and OFDLCK lines plus merged range labels.
func TestParseProcLocks(t *testing.T) {
	text := `1: POSIX  ADVISORY  WRITE 4242 fd:01:786470 1073741825 1073741825
2: -> POSIX  ADVISORY  READ  4243 08:1f:12 1073741826 1073742335
3: FLOCK  ADVISORY  WRITE 99 fd:01:5 0 EOF
4: OFDLCK ADVISORY  READ  -1 00:2e:77 128 128

`
	locks, err := ParseProcLocks(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 4 {
		t.Fatalf("%d locks", len(locks))
	}
	first := locks[0]
	if first.ID != 1 || first.Blocked || first.Class != "POSIX" || first.Type != "WRITE" || first.Pid != 4242 || first.DevMajor != 0xfd || first.DevMinor != 1 || first.Inode != 786470 || first.Start != ReservedByte || first.Length() != 1 || first.RangeText() != "1073741825" {
		t.Errorf("first %+v", first)
	}
	if !locks[1].Blocked || locks[1].DevMinor != 0x1f || locks[1].Length() != SharedSize || locks[1].RangeText() != "1073741826-1073742335" {
		t.Errorf("second %+v", locks[1])
	}
	if !locks[2].ToEOF || locks[2].RangeText() != "0-EOF" {
		t.Errorf("third %+v", locks[2])
	}
	if locks[3].Pid != -1 || locks[3].Class != "OFDLCK" {
		t.Errorf("fourth %+v", locks[3])
	}
	names := map[int]string{4242: "holder", 4243: "other"}
	got := DescribeProcLocks(FilterProcLocks(locks, 786470, map[int]bool{4242: true}), names, DatabaseLockRanges())
	if len(got) != 1 || got[0] != "holder     POSIX WRITE 1073741825            RESERVED" {
		t.Errorf("described %q", got)
	}
	merged := ProcLock{Class: "POSIX", Type: "WRITE", Pid: 4242, Start: 120, End: 121}
	if got := DescribeProcLocks([]ProcLock{merged}, names, ShmLockRanges()); !strings.HasSuffix(got[0], "(WAL_WRITE_LOCK+WAL_CKPT_LOCK)") {
		t.Errorf("merged range %q", got)
	}
}

// TestParseProcLocksRejectsMalformedLines ensures invalid /proc/locks lines return ErrProcLocksFormat.
func TestParseProcLocksRejectsMalformedLines(t *testing.T) {
	bad := []string{
		"",
		"POSIX ADVISORY WRITE 1 fd:01:2 0 0",
		"x: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0",
		"0: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0 extra",
		"1: WEIRD  ADVISORY  WRITE 1 fd:01:2 0 0",
		"1: POSIX  SOMETIMES WRITE 1 fd:01:2 0 0",
		"1: POSIX  ADVISORY  SCRIBBLE 1 fd:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE x fd:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01 0 0",
		"1: POSIX  ADVISORY  WRITE 1 zz:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:zz:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:-2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 -1 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 5 4",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 5 later",
	}
	for _, line := range bad {
		if _, err := ParseProcLockLine(line); !errors.Is(err, ErrProcLocksFormat) {
			t.Errorf("%q: err %v", line, err)
		}
	}
	if _, err := ParseProcLocks("1: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0\nbroken\n"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("multi-line error %v", err)
	}
}

// TestIntegritySummary checks splitting and condensing PRAGMA integrity_check diagnostic output.
func TestIntegritySummary(t *testing.T) {
	if IntegrityProblems("ok") != nil || IntegritySummary("ok") != "ok" {
		t.Error("ok")
	}
	text := "*** in database main ***\nPage 3: never used\nPage 4: never used; row 9 missing from index i"
	problems := IntegrityProblems(text)
	if len(problems) != 3 || problems[0] != "Page 3: never used" || problems[2] != "row 9 missing from index i" {
		t.Fatalf("%q", problems)
	}
	if got := IntegritySummary(text); got != "3 problem(s), the first: Page 3: never used" {
		t.Errorf("%q", got)
	}
}

// TestCopierRole tests whole-file and byte-range copies, missing sidecars, and rejection of bad target names or offsets.
func TestCopierRole(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "src.db")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	actor, err := CopierRole(context.Background(), []string{"-db", db, "-out", out})
	if err != nil {
		t.Fatal(err)
	}
	do := func(action string, args ...string) (string, error) {
		return actor.Actions[action](context.Background(), args)
	}
	if v, err := do("copy-range", "db", "c", "0", "4"); err != nil || v != "bytes 0-4" {
		t.Fatalf("%q %v", v, err)
	}
	if err := os.WriteFile(db, []byte("abcdefghijkl"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := do("copy-range", "db", "c", "4", "end"); err != nil || v != "bytes 4-12" {
		t.Fatalf("%q %v", v, err)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "c")); string(data) != "0123efghijkl" {
		t.Fatalf("torn copy %q", data)
	}
	if v, err := do("copy", "-wal", "w"); err != nil || v != "missing" {
		t.Fatalf("missing source: %q %v", v, err)
	}
	if v, err := do("copy", "db", "whole"); err != nil || v != "12 bytes" {
		t.Fatalf("%q %v", v, err)
	}
	for _, args := range [][]string{{"db", "../x"}, {"db", ""}, {"nope", "x"}} {
		if _, err := do("copy", args...); err == nil {
			t.Errorf("copy %q accepted", args)
		}
	}
	for _, args := range [][]string{{"db", "c", "5", "4"}, {"db", "c", "-1", "end"}, {"db", "c", "x", "end"}, {"db", "c", "0"}} {
		if _, err := do("copy-range", args...); err == nil {
			t.Errorf("copy-range %q accepted", args)
		}
	}
	if _, err := CopierRole(context.Background(), []string{"-db", db}); err == nil {
		t.Error("copier without -out accepted")
	}
}

// TestSQLRolesAcrossProcesses exercises cross-process BUSY, BUSY_SNAPSHOT, checkpoints, dbpage-write, and InspectDatabase.
func TestSQLRolesAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "lab.db")
	if err := CreateDatabase(ctx, db, sqlitex.JournalWAL, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT)"); err != nil {
		t.Fatal(err)
	}
	options := SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, PagesOut: filepath.Join(dir, "pages.bin")}
	var out bytes.Buffer
	script := NewScript(&out)
	defer script.Kill()
	first, err := script.Spawn("first", RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	second, err := script.Spawn("second", RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	expect := func(child *Child, wantCode int, action string, args ...string) Result {
		t.Helper()
		r, err := script.Do(child, action, args...)
		if err != nil {
			t.Fatal(err)
		}
		if r.Code != wantCode {
			t.Fatalf("%s %s: %s %s", child.Name, action, r.Outcome(), r.Error)
		}
		return r
	}
	expect(first, 0, "begin", "immediate")
	expect(first, 0, "exec", "INSERT INTO lab VALUES (1, 'first')")
	expect(second, sqlitex.CodeBusy, "begin", "immediate")
	expect(second, 0, "begin", "deferred")
	expect(second, 0, "query", "SELECT count(*) FROM lab")
	expect(first, 0, "commit")
	expect(second, sqlitex.CodeBusySnapshot, "exec", "INSERT INTO lab VALUES (2, 'second')")
	expect(second, 0, "rollback")
	if r := expect(second, 0, "query", "SELECT who FROM lab"); r.Value != "first" {
		t.Fatalf("value %q", r.Value)
	}
	if r := expect(second, 0, "query", "SELECT NULL"); r.Value != "NULL" {
		t.Fatalf("null %q", r.Value)
	}
	expect(first, CodeNotSQLite, "begin", "sideways")
	expect(first, CodeNotSQLite, "exec")
	expect(first, CodeNotSQLite, "stray-close", "elsewhere")
	expect(first, 0, "timeout", "10")
	expect(first, CodeNotSQLite, "timeout", "-1")
	if r := expect(first, 0, "checkpoint", "PASSIVE"); !strings.HasPrefix(r.Value, "busy=0 ") {
		t.Fatalf("checkpoint %q", r.Value)
	}
	if r := expect(first, 0, "dbpage-write", "1", "2"); r.Value != "2 pages" {
		t.Fatalf("dbpage-write %q", r.Value)
	}
	expect(first, CodeNotSQLite, "dbpage-write", "2", "1")
	if info, err := os.Stat(options.PagesOut); err != nil || info.Size() != 2*4096 {
		t.Fatalf("pages file %v %v", info, err)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT count(*) FROM lab", "SELECT no_such_column FROM lab")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Integrity != "ok" || inspection.Values[0] != "1" || !strings.HasPrefix(inspection.Values[1], "error SQLITE_ERROR") {
		t.Fatalf("%+v", inspection)
	}
}

// TestSQLOptionsRoundTrip verifies SQLOptions.Args round-trips through parseSQLOptions and rejects missing or extra args.
func TestSQLOptionsRoundTrip(t *testing.T) {
	options := SQLOptions{Path: "/x/y.db", JournalMode: "delete", BusyTimeout: 1500 * time.Millisecond, WALAutocheckpoint: 7, PagesOut: "/x/p"}
	parsed, err := parseSQLOptions(options.Args())
	if err != nil || parsed != options {
		t.Fatalf("%+v %v", parsed, err)
	}
	if _, err := parseSQLOptions([]string{"-journal", "wal"}); err == nil {
		t.Error("missing -db accepted")
	}
	if _, err := parseSQLOptions([]string{"-db", "x", "extra"}); err == nil {
		t.Error("extra argument accepted")
	}
}

// TestAnsweredWithinWaitsWithoutKilling checks that AnsweredWithin returns false on expiry without killing the child and true on immediate reply.
func TestAnsweredWithinWaitsWithoutKilling(t *testing.T) {
	child, err := StartChild("window", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	slow, err := child.Start(1, "sleep", "1s")
	if err != nil {
		t.Fatal(err)
	}
	if answered, err := slow.AnsweredWithin(100 * time.Millisecond); answered || err != nil {
		t.Fatalf("blocked action: answered %v err %v", answered, err)
	}
	if result, err := slow.Wait(); err != nil || result.Value != "slept" {
		t.Fatalf("after the window the child must still answer: %+v %v", result, err)
	}
	fast, err := child.Start(2, "echo", "x")
	if err != nil {
		t.Fatal(err)
	}
	if answered, err := fast.AnsweredWithin(5 * time.Second); !answered || err != nil {
		t.Fatalf("immediate action: answered %v err %v", answered, err)
	}
	if result, err := fast.Wait(); err != nil || result.Value != "x" {
		t.Fatalf("%+v %v", result, err)
	}
	if err := child.Stop(); err != nil {
		t.Fatal(err)
	}
}

// TestLabStillWaitingCanFail verifies Lab.StillWaiting returns false for an immediate answer and true for a blocked one.
func TestLabStillWaitingCanFail(t *testing.T) {
	var out bytes.Buffer
	lab := NewLab(&out)
	child := lab.Spawn("w", "echo")
	immediate := lab.Begin(child, "echo", "now")
	if lab.StillWaiting(immediate) {
		t.Fatalf("an action that answers at once was reported as waiting: %q", out.String())
	}
	lab.Finish(immediate)
	blocked := lab.Begin(child, "sleep", "1s")
	if !lab.StillWaiting(blocked) {
		t.Fatalf("a blocked action was reported as answered: %q", out.String())
	}
	lab.Finish(blocked)
	lab.StopChildren()
	if !strings.Contains(out.String(), ".. already answered") || !strings.Contains(out.String(), ".. no answer after 250ms") {
		t.Fatalf("transcript %q", out.String())
	}
}

// TestStillWaitingSeesImmediateBusy confirms Lab.StillWaiting reports immediate SQLITE_BUSY at busy_timeout=0 as already answered.
func TestStillWaitingSeesImmediateBusy(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "lab.db")
	ctx := context.Background()
	if err := CreateDatabase(ctx, db, sqlitex.JournalWAL, "CREATE TABLE lab(who TEXT)"); err != nil {
		t.Fatal(err)
	}
	lab := NewLab(io.Discard)
	options := SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	holder, waiter := lab.SpawnSQL("holder", options), lab.SpawnSQL("waiter", options)
	lab.Must(holder, "begin", "immediate")
	call := lab.Begin(waiter, "begin", "immediate")
	if lab.StillWaiting(call) {
		t.Fatal("busy_timeout 0: BEGIN IMMEDIATE returns SQLITE_BUSY at once, StillWaiting must say so")
	}
	if result := lab.Finish(call); result.Code != sqlitex.CodeBusy {
		t.Fatalf("waiter: %s", result.Outcome())
	}
	lab.Must(holder, "commit")
	lab.StopChildren()
}

// TestStopKillsChildWhenExitAnswerIsWrong ensures Stop kills and reaps the child when the exit reply fails with ErrStepMismatch.
func TestStopKillsChildWhenExitAnswerIsWrong(t *testing.T) {
	child, err := StartChild("liar", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	if _, err := child.Do(1, "lie"); !errors.Is(err, ErrStepMismatch) {
		t.Fatalf("lie: %v", err)
	}
	if err := child.Stop(); !errors.Is(err, ErrStepMismatch) {
		t.Fatalf("Stop = %v", err)
	}
	select {
	case <-child.exited:
	default:
		t.Fatal("Stop returned an error but left the child running")
	}
	if err := child.Stop(); err != nil {
		t.Fatalf("second Stop = %v", err)
	}
}

// TestParseProcLocksHostVariants verifies acceptance of valid Linux fs/locks.c variants (<none>:0, MSNFS, UNKNOWN, NONE) and rejection of bad ones.
func TestParseProcLocksHostVariants(t *testing.T) {
	lines := map[string]ProcLock{
		"10: FLOCK  ADVISORY  WRITE 4321 <none>:0 0 EOF":      {ID: 10, Class: "FLOCK", Mode: "ADVISORY", Type: "WRITE", Pid: 4321, ToEOF: true, End: 1<<63 - 1},
		"11: FLOCK  MSNFS     WRITE 4322 08:01:1234 0 EOF":    {ID: 11, Class: "FLOCK", Mode: "MSNFS", Type: "WRITE", Pid: 4322, DevMajor: 8, DevMinor: 1, Inode: 1234, ToEOF: true, End: 1<<63 - 1},
		"12: UNKNOWN UNKNOWN  READ  4323 08:01:1235 5 9":      {ID: 12, Class: "UNKNOWN", Mode: "UNKNOWN", Type: "READ", Pid: 4323, DevMajor: 8, DevMinor: 1, Inode: 1235, Start: 5, End: 9},
		"13: FLOCK  MSNFS     NONE  4324 08:01:1236 0 EOF":    {ID: 13, Class: "FLOCK", Mode: "MSNFS", Type: "NONE", Pid: 4324, DevMajor: 8, DevMinor: 1, Inode: 1236, ToEOF: true, End: 1<<63 - 1},
		"14: -> POSIX  *NOINODE* WRITE 4325 <none>:0 120 120": {ID: 14, Blocked: true, Class: "POSIX", Mode: "*NOINODE*", Type: "WRITE", Pid: 4325, Start: 120, End: 120},
	}
	for line, want := range lines {
		got, err := ParseProcLockLine(line)
		if err != nil || got != want {
			t.Errorf("%q: got %+v err %v, want %+v", line, got, err, want)
		}
	}
	for _, line := range []string{
		"15: FLOCK  ADVISORY  WRITE 4321 <none>:1 0 EOF",
		"16: FLOCK  ADVISORY  WRITE 4321 <none>:0 9 3",
		"17: FLOCK  ADVISORY  WRITE 4321 <none>:0 x EOF",
	} {
		if _, err := ParseProcLockLine(line); !errors.Is(err, ErrProcLocksFormat) {
			t.Errorf("%q: err %v", line, err)
		}
	}
}

// TestReadRangeKeepsOnlyBytesRead confirms readRange truncates to the bytes actually read instead of zero-padding on short EOF.
func TestReadRangeKeepsOnlyBytesRead(t *testing.T) {
	got, err := readRange(strings.NewReader("0123"), 2, 10)
	if err != nil || string(got) != "23" {
		t.Fatalf("short read: %q %v", got, err)
	}
	got, err = readRange(strings.NewReader("0123"), 6, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("past the end: %q %v", got, err)
	}
	got, err = readRange(strings.NewReader("0123456789"), 2, 5)
	if err != nil || string(got) != "234" {
		t.Fatalf("full read: %q %v", got, err)
	}
}

// TestLockViewProcHolders verifies exact, kernel-merged, and to-EOF /proc/locks entries map to the expected named range holders.
func TestLockViewProcHolders(t *testing.T) {
	ranges := ShmLockRanges()
	states := make([]RangeState, len(ranges))
	for i, r := range ranges {
		states[i] = RangeState{Range: r}
	}
	view := LockView{Ranges: states, ProcListed: true, Proc: []ProcLock{
		{Pid: 1, Type: "WRITE", Start: 120, End: 121},
		{Pid: 2, Type: "READ", Start: 128, End: 128},
		{Pid: 1, Type: "READ", Start: 123, End: 123},
		{Pid: 3, Type: "READ", Start: 100, ToEOF: true, End: 1<<63 - 1},
	}}
	if got := view.ProcHolders("WAL_WRITE_LOCK"); len(got) != 2 || got[1] != "WRITE" || got[3] != "READ" {
		t.Errorf("WAL_WRITE_LOCK holders %v", got)
	}
	if got := view.ProcHolders("WAL_CKPT_LOCK"); len(got) != 2 || got[1] != "WRITE" {
		t.Errorf("merged range holders %v", got)
	}
	if got := view.ProcHolders("DMS"); len(got) != 2 || got[2] != "READ" {
		t.Errorf("DMS holders %v", got)
	}
	if got := view.ProcHolders("WAL_READ_LOCK(1)"); len(got) != 1 || got[3] != "READ" {
		t.Errorf("READ(1) holders %v", got)
	}
	if got := view.ProcHolders("no such range"); len(got) != 0 {
		t.Errorf("unknown range %v", got)
	}
}

// TestRecipeSessionCommitsOnAFreshConnection verifies that recipe-session commits on a fresh connection, keeps the WAL alive via the held connection, rejects replays/bad args, and matches the canonical step at E1 on close.
func TestRecipeSessionCommitsOnAFreshConnection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("COURSE_WORK_DIR", filepath.Join(dir, "work"))
	db := filepath.Join(dir, "r.db")
	r := recipe.Default().WithSteps(3)
	if err := r.Build(ctx, db, 1); err != nil {
		t.Fatal(err)
	}
	options := SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: time.Second}
	var out bytes.Buffer
	script := NewScript(&out)
	defer script.Kill()
	writer, err := script.Spawn("writer", RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"2", "3"} {
		if r, err := script.Do(writer, "recipe-session", k); err != nil || r.Code != 0 {
			t.Fatalf("recipe-session %s: %+v %v", k, r, err)
		}
	}
	if info, err := os.Stat(db + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the held connection should keep the WAL: %v %v", info, err)
	}
	if r, err := script.Do(writer, "user-version"); err != nil || r.Value != "3" {
		t.Fatalf("user-version %+v %v", r, err)
	}
	if r, err := script.Do(writer, "recipe-session", "3"); err != nil || r.Code == 0 {
		t.Fatalf("re-applying step 3 must fail: %+v %v", r, err)
	}
	if r, err := script.Do(writer, "recipe-session", "x"); err != nil || r.Code != CodeNotSQLite {
		t.Fatalf("bad argument: %+v %v", r, err)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	match, err := r.MatchStep(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !match.Found() || match.Step != 3 || !match.Exact {
		t.Fatalf("match %s", match)
	}
}

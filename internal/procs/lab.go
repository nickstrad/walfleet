package procs

import (
	"fmt"
	"io"
	"os"
	"time"
)

// StillWaitingWindow is how long Lab.StillWaiting waits for a reply before
// concluding that a child is genuinely blocked in SQLite's busy-retry loop.
// 250ms is far longer than a local pipe round-trip (so an immediate SQLITE_BUSY
// arrives inside the window and fails the check) and far shorter than the
// 120s busy_timeout used in contention labs.
const StillWaitingWindow = 250 * time.Millisecond

// Lab wraps Script with experiment ergonomics: fatal-on-infrastructure-error
// helpers, claim assertions, lock inspection, and final pass/fail reporting.
// The optional hooks hand each answered step, check and lock look to the
// experiment, whether or not the lab also prints them.
type Lab struct {
	Out    io.Writer
	Script *Script

	// OnStep, if set, is called for every step answered through Do, Must or Finish.
	OnStep func(StepEvent)
	// OnCheck, if set, is called for every Check.
	OnCheck func(ok bool, claim string)
	// OnLocks, if set, is called for every Locks look.
	OnLocks func(LocksEvent)

	quiet    bool
	polled   map[*Call]bool
	failures []string
	exit     func(int)
}

// StepEvent is one answered step as OnStep receives it.
type StepEvent struct {
	Child  *Child
	Action string // ActionText of the action and its arguments
	Result Result
	// Polled is true when the step was sent with Begin and StillWaiting looked
	// at it; Waiting then says whether it was still unanswered.
	Polled  bool
	Waiting bool
}

// LocksEvent is one Locks look as OnLocks receives it.
type LocksEvent struct {
	Step  int // Script.Step() when the locks were probed
	Label string
	View  LockView
	Names map[int]string // child PID -> child name at the time of the look
}

// NewLab creates a Lab that writes transcripts and check results to out and
// exits the process via os.Exit on Fatal or a failing Report.
func NewLab(out io.Writer) *Lab {
	return &Lab{Out: out, Script: NewScript(out), exit: os.Exit}
}

// NewQuietLab creates a Lab that prints no step transcript, check lines or
// lock dumps; experiments receive them through the hooks instead. Fatal and
// Report still print to out and exit via os.Exit as they do for NewLab.
func NewQuietLab(out io.Writer) *Lab {
	return &Lab{Out: out, Script: NewScript(nil), quiet: true, exit: os.Exit}
}

// Fatal kills all children, prints an error line, and exits with status 1.
func (l *Lab) Fatal(what string, err error) {
	l.Script.Kill()
	fmt.Fprintf(l.Out, "FATAL %s: %v\n", what, err)
	l.exit(1)
}

// Check prints one "check ok:" or "check FAILED:" line (unless quiet), calls
// OnCheck, and records any failed claim so Report exits non-zero at the end of
// the experiment.
func (l *Lab) Check(ok bool, claim string) bool {
	status := "ok"
	if !ok {
		status = "FAILED"
		l.failures = append(l.failures, claim)
	}
	if !l.quiet {
		fmt.Fprintf(l.Out, "check %s: %s\n", status, claim)
	}
	if l.OnCheck != nil {
		l.OnCheck(ok, claim)
	}
	return ok
}

// Failures returns a copy of all claim strings that failed Check so far.
func (l *Lab) Failures() []string { return append([]string(nil), l.failures...) }

// Spawn starts a child in role under name, aborting via Fatal on error.
func (l *Lab) Spawn(name, role string, args ...string) *Child {
	child, err := l.Script.Spawn(name, role, args...)
	if err != nil {
		l.Fatal("spawn "+name, err)
	}
	return child
}

// SpawnSQL starts a RoleSQL child configured with options.
func (l *Lab) SpawnSQL(name string, options SQLOptions) *Child {
	return l.Spawn(name, RoleSQL, options.Args()...)
}

// Do runs action on child and returns its Result (whether SQLITE_OK or an error
// code); transport or child-death errors are fatal.
func (l *Lab) Do(child *Child, action string, args ...string) Result {
	result := l.do(child, action, args)
	l.stepDone(StepEvent{Child: child, Action: ActionText(action, args), Result: result})
	return result
}

// Must runs action on child and aborts via Fatal unless the result is SQLITE_OK.
func (l *Lab) Must(child *Child, action string, args ...string) Result {
	result := l.do(child, action, args)
	if !result.OK() {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), fmt.Errorf("%s %s: %s %s", child.Name, ActionText(action, args), result.Outcome(), result.Error))
	}
	l.stepDone(StepEvent{Child: child, Action: ActionText(action, args), Result: result})
	return result
}

func (l *Lab) do(child *Child, action string, args []string) Result {
	result, err := l.Script.Do(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	return result
}

func (l *Lab) stepDone(event StepEvent) {
	if l.OnStep != nil {
		l.OnStep(event)
	}
}

// Begin sends action to child without waiting for the reply and, unless quiet,
// logs that the step is in flight.
func (l *Lab) Begin(child *Child, action string, args ...string) *Call {
	call, err := l.Script.Begin(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	if l.quiet {
		return call
	}
	request := call.Request()
	fmt.Fprintf(l.Out, "step %2d  %-10s %-36s .. sent, not waiting for the answer\n", request.Step, child.Name, ActionText(request.Action, request.Args))
	return call
}

// StillWaiting waits StillWaitingWindow for call to answer, logs whether it
// stayed unanswered (unless quiet), remembers the answer for OnStep, and
// returns true if the child is still blocked.
func (l *Lab) StillWaiting(call *Call) bool {
	answered, err := call.AnsweredWithin(StillWaitingWindow)
	if err != nil {
		l.Fatal("poll", err)
	}
	if l.polled == nil {
		l.polled = map[*Call]bool{}
	}
	l.polled[call] = !answered
	if l.quiet {
		return !answered
	}
	request := call.Request()
	state := fmt.Sprintf("no answer after %v", StillWaitingWindow)
	if answered {
		state = "already answered"
	}
	fmt.Fprintf(l.Out, "step %2d  %-10s %-36s .. %s\n", request.Step, call.child.Name, ActionText(request.Action, request.Args), state)
	return !answered
}

// Finish waits for an in-flight Call started with Begin and prints its outcome
// (unless quiet).
func (l *Lab) Finish(call *Call) Result {
	result, err := l.Script.Finish(call)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", call.Request().Step), err)
	}
	waiting, polled := l.polled[call]
	delete(l.polled, call)
	request := call.Request()
	l.stepDone(StepEvent{Child: call.child, Action: ActionText(request.Action, request.Args), Result: result, Polled: polled, Waiting: waiting})
	return result
}

// StopChildren cleanly stops every running child, aborting on any close error.
func (l *Lab) StopChildren() {
	if err := l.Script.Close(); err != nil {
		l.Fatal("stop children", err)
	}
}

// PidName returns the logical child name for pid, or "another process" if unknown.
func (l *Lab) PidName(pid int) string {
	if name, ok := l.Script.NameOfPid(pid); ok {
		return name
	}
	return "another process"
}

// LockView combines the portable fcntl(F_GETLK) probe results with Linux's
// per-child /proc/locks entries so callers can assert both views.
type LockView struct {
	Ranges     []RangeState
	Proc       []ProcLock
	ProcListed bool
}

// ProcHolders returns a map of PID -> lock type ("READ" or "WRITE") for every
// /proc/locks entry in v.Proc that covers the entire named range (including
// kernel-merged ranges and locks extending to EOF).
func (v LockView) ProcHolders(name string) map[int]string {
	holders := map[int]string{}
	for _, s := range v.Ranges {
		if s.Range.Name != name {
			continue
		}
		last := s.Range.Start + s.Range.Length - 1
		for _, lock := range v.Proc {
			if lock.Start <= s.Range.Start && (lock.ToEOF || lock.End >= last) {
				holders[lock.Pid] = lock.Type
			}
		}
	}
	return holders
}

// Locks probes ranges on path from the parent process (which holds no SQLite
// connection, so opening and closing path here drops no SQLite locks) and, on
// Linux, also lists and returns the children's /proc/locks entries for path.
// Unless quiet it prints both views; it then calls OnLocks.
func (l *Lab) Locks(label, path string, ranges []LockRange) LockView {
	states, err := ProbeRanges(path, ranges)
	if err != nil {
		l.Fatal("probe locks", err)
	}
	if !l.quiet {
		fmt.Fprintf(l.Out, "  %s locks (fcntl F_GETLK from the parent):\n", label)
		for _, line := range FormatRangeStates(states, l.PidName) {
			fmt.Fprintf(l.Out, "    %s\n", line)
		}
	}
	names := map[int]string{}
	for _, child := range l.Script.Children() {
		names[child.Pid] = child.Name
	}
	view := LockView{Ranges: states}
	if ProcLocksSupported {
		view.Proc = l.procLocks(label, path, ranges, names)
		view.ProcListed = true
	}
	if l.OnLocks != nil {
		l.OnLocks(LocksEvent{Step: l.Script.Step(), Label: label, View: view, Names: names})
	}
	return view
}

func (l *Lab) procLocks(label, path string, ranges []LockRange, names map[int]string) []ProcLock {
	inode, err := FileInode(path)
	if err != nil {
		l.Fatal("inode", err)
	}
	all, err := ReadProcLocks()
	if err != nil {
		l.Fatal(ProcLocksPath, err)
	}
	pids := map[int]bool{}
	for pid := range names {
		pids[pid] = true
	}
	mine := FilterProcLocks(all, inode, pids)
	if l.quiet {
		return mine
	}
	lines := DescribeProcLocks(mine, names, ranges)
	if len(lines) == 0 {
		fmt.Fprintf(l.Out, "  %s locks in %s: none\n", label, ProcLocksPath)
		return mine
	}
	fmt.Fprintf(l.Out, "  %s locks in %s:\n", label, ProcLocksPath)
	for _, line := range lines {
		fmt.Fprintf(l.Out, "    %s\n", line)
	}
	return mine
}

// StateOf looks up the probed LockHolder for the named range inside states.
func StateOf(states []RangeState, name string) (LockHolder, bool) {
	for _, s := range states {
		if s.Range.Name == name {
			return s.Holder, true
		}
	}
	return LockHolder{}, false
}

// WriteLockedBy reports whether name is exclusively write-locked by pid.
func WriteLockedBy(states []RangeState, name string, pid int) bool {
	holder, ok := StateOf(states, name)
	return ok && holder.Held && holder.Write && holder.Pid == pid
}

// ReadLocked reports whether name is held under a shared read lock.
func ReadLocked(states []RangeState, name string) bool {
	holder, ok := StateOf(states, name)
	return ok && holder.Held && !holder.Write
}

// Unlocked reports whether all named ranges (or every range in states if names
// is empty) have no conflicting lock.
func Unlocked(states []RangeState, names ...string) bool {
	for _, s := range states {
		if len(names) > 0 && !contains(names, s.Range.Name) {
			continue
		}
		if s.Holder.Held {
			return false
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Report prints the final summary banner and exits with status 1 if any Check failed.
func (l *Lab) Report(title string) {
	fmt.Fprintln(l.Out)
	if len(l.failures) > 0 {
		fmt.Fprintf(l.Out, "%s FAILED: %d claim(s) did not hold\n", title, len(l.failures))
		for _, f := range l.failures {
			fmt.Fprintf(l.Out, "  - %s\n", f)
		}
		l.exit(1)
		return
	}
	fmt.Fprintf(l.Out, "%s OK\n", title)
}

// Experiment 7.1 runs the pinned sqlite3 C CLI with -vfstrace under Linux
// strace to merge VFS calls and kernel syscalls into a single ordered stream.
// At SQLite's default mmap_size=0, it verifies that every xRead/xWrite/xSync
// maps to the expected pread64, pwrite64, or fsync/fdatasync syscall, that no
// byte-moving syscall on database/sidecar files escapes the VFS, that directory
// syncs happen inside the first xSync of a new journal or WAL, and that
// -wal/-shm are created at first use and removed at last close.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.1 (the strace part needs
// Linux, which the lab container provides).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls"

const dbName = "t.db"

var statements = []struct {
	label string
	sql   string
}{
	{"set WAL mode", "PRAGMA journal_mode=WAL;"},
	{"set synchronous FULL", "PRAGMA synchronous=FULL;"},
	{"create a table", "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL);"},
	{"commit one insert", "INSERT INTO t(id, v) VALUES(1, 'one');"},
	{"checkpoint", "PRAGMA wal_checkpoint(TRUNCATE);"},
	{"close", ""},
}

var unixFunction = map[string]string{
	"xOpen":                  "unixOpen",
	"xDelete":                "unixDelete",
	"xAccess":                "unixAccess",
	"xFullPathname":          "unixFullPathname",
	"xRandomness":            "unixRandomness",
	"xSleep":                 "unixSleep",
	"xCurrentTime":           "unixCurrentTime",
	"xCurrentTimeInt64":      "unixCurrentTimeInt64",
	"xClose":                 "unixClose",
	"xRead":                  "unixRead",
	"xWrite":                 "unixWrite",
	"xTruncate":              "unixTruncate",
	"xSync":                  "unixSync",
	"xFileSize":              "unixFileSize",
	"xLock":                  "unixLock",
	"xUnlock":                "unixUnlock",
	"xCheckReservedLock":     "unixCheckReservedLock",
	"xFileControl":           "unixFileControl",
	"xSectorSize":            "unixSectorSize",
	"xDeviceCharacteristics": "unixDeviceCharacteristics",
	"xShmMap":                "unixShmMap",
	"xShmLock":               "unixShmLock",
	"xShmBarrier":            "unixShmBarrier",
	"xShmUnmap":              "unixShmUnmap",
	"xFetch":                 "unixFetch",
	"xUnfetch":               "unixUnfetch",
}

// tracedSyscalls extends trace.DefaultSyscalls with stat and access families so
// xAccess (os_unix.c:6881-6903), xFileSize, and xFullPathname show their kernel
// calls. fchown is omitted because unixOpen only calls it when running as root.
var tracedSyscalls = append(append([]string(nil), trace.DefaultSyscalls...), "fstat", "newfstatat", "statx", "stat", "lstat", "access", "faccessat", "faccessat2")

// byteMovingSyscalls lists every traced canonical syscall that transfers file
// bytes, used to verify no database/journal/WAL/shm byte I/O escapes the VFS.
var byteMovingSyscalls = []string{"pread64", "pwrite64", "read", "write", "writev"}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.1" runs both.
var (
	set    *results.Set
	stream *results.Table
)

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func script() string {
	var b strings.Builder
	for i, s := range statements {
		fmt.Fprintf(&b, ".print STEP %d %s\n", i+1, s.label)
		if s.sql != "" {
			b.WriteString(s.sql + "\n")
		}
	}
	return b.String()
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.1")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	cli := sqlitex.MustLocateCLI(ctx)
	set = results.MustOpen(dir)
	steps := set.Table("statements", "step", "label", "sql")
	for i, s := range statements {
		sql := any(s.sql)
		if s.sql == "" {
			sql = "(end of input: the CLI closes the database)"
		}
		steps.Row(i+1, s.label, sql)
	}
	stream = set.Table("stream", "seq", "step", "kind", "nested", "charged_to", "call", "file", "text")
	tracing := set.Table("tracing", "syscalls")
	request := trace.CLITrace{Dir: dir, DB: filepath.Join(dir, dbName), SQL: script()}

	if !trace.Supported {
		tracing.Row(trace.LinuxOnlyMessage("strace of the CLI (VFS calls merged with syscalls)", packagePath))
		portable(ctx, cli, request, dir)
		trace.PrintLinuxOnly("strace of the CLI (VFS calls merged with syscalls)", packagePath)
		set.Finish("VFS CALLS TO SYSCALLS")
		return
	}
	tracing.Row("traced with strace")
	merged, result, err := trace.RunCLIMerged(ctx, cli, request, trace.Options{Syscalls: tracedSyscalls})
	if errors.Is(err, trace.ErrPtraceDenied) {
		fatal("strace", fmt.Errorf("%w (run: docker compose run --rm lab go run ./%s)", err, packagePath))
	}
	if err != nil {
		fatal("strace", err)
	}
	if result.ExitCode != 0 {
		fatal("cli", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr))
	}
	analyze(merged.Relative(dir), dir)
	set.Finish("VFS CALLS TO SYSCALLS")
}

func portable(ctx context.Context, cli sqlitex.CLI, request trace.CLITrace, dir string) {
	vt, err := trace.RunCLIVFSTrace(ctx, cli, request)
	if err != nil {
		fatal("vfstrace", err)
	}
	calls := trace.RelativeVFS(dir, vt.Calls)
	seq := 0
	for _, c := range calls {
		if c.Method == "xRandomness" {
			continue
		}
		seq++
		stream.Row(seq, nil, "VFS", nil, nil, c.Method, c.File, c.String())
	}
	walOpen, walDelete, shmDelete := closeLifecycle(calls)
	set.Check("D", walOpen, "the -wal file is opened through xOpen")
	set.Check("D", walDelete && shmDelete, "at the last close the -wal is deleted (xDelete) and the -shm is unmapped with delFlag=1")
}

// closeLifecycle checks the VFS-only trace (used on non-Linux hosts) for WAL
// open, WAL deletion, and shm unmap with delFlag=1 (vfstrace.c:843).
func closeLifecycle(calls []trace.VFSCall) (walOpen, walDelete, shmDelete bool) {
	for _, c := range calls {
		switch {
		case c.Method == "xOpen" && c.File == dbName+"-wal":
			walOpen = true
		case c.Method == "xDelete" && strings.HasSuffix(c.File, dbName+"-wal"):
			walDelete = true
		case c.Method == "xShmUnmap" && slices.Contains(c.Args, "delFlag=1"):
			shmDelete = true
		}
	}
	return walOpen, walDelete, shmDelete
}

func onDisk(s trace.Syscall) bool {
	return s.Path == "." || s.Path == dbName || strings.HasPrefix(s.Path, dbName+"-")
}

func syscallLine(s trace.Syscall) string {
	return s.Canonical().String()
}

func analyze(m *trace.Merged, dir string) {
	relevant := onDisk
	var kept []int
	for i, e := range m.Events {
		if e.Kind == trace.EventSyscall && !relevant(*e.Syscall) {
			continue
		}
		kept = append(kept, i)
	}

	// charged maps each file syscall to the VFS call it is counted against:
	// the call it ran inside, or, for xOpen, the unenclosed syscalls just
	// before its line, because vfstraceOpen calls the underlying xOpen before
	// printing its trace line (vfstrace.c:890-893).
	charged := map[int]int{}
	for i, e := range m.Events {
		if e.Kind != trace.EventVFS {
			continue
		}
		if e.VFS.Method != "xOpen" {
			for j, s := range m.Events {
				if s.Kind == trace.EventSyscall && s.Enclosed == i && relevant(*s.Syscall) {
					charged[j] = i
				}
			}
			continue
		}
		for j := i - 1; j >= 0; j-- {
			s := m.Events[j]
			if s.Kind == trace.EventVFS {
				break
			}
			if s.Kind == trace.EventSyscall && s.Enclosed < 0 && relevant(*s.Syscall) {
				charged[j] = i
			}
		}
	}
	syscallsOf := func(i int) []trace.Syscall {
		var out []trace.Syscall
		for _, j := range kept {
			if v, ok := charged[j]; ok && v == i {
				out = append(out, m.Events[j].Syscall.Canonical())
			}
		}
		return out
	}

	// xRandomness is left out of the stream table: where SQLite first asks the
	// VFS for random bytes differs between CLI builds. Its position goes to an
	// info table.
	infoRandomness := set.Table("info_randomness", "after_seq", "call")
	seqOf := map[int]int{}
	seq := 0
	for _, i := range kept {
		e := m.Events[i]
		if e.Kind == trace.EventVFS && e.VFS.Method == "xRandomness" {
			infoRandomness.Row(seq, e.VFS.String())
			continue
		}
		seq++
		seqOf[i] = seq
	}
	step := 0
	for _, i := range kept {
		n, ok := seqOf[i]
		if !ok {
			continue
		}
		e := m.Events[i]
		switch e.Kind {
		case trace.EventVFS:
			stream.Row(n, step, "VFS", nil, nil, e.VFS.Method, e.VFS.File, e.VFS.String())
		case trace.EventSyscall:
			s := e.Syscall.Canonical()
			var to any
			if v, ok := charged[i]; ok {
				to = seqOf[v]
			}
			stream.Row(n, step, "SYS", e.Enclosed >= 0, to, s.Name, s.Path, s.String())
		case trace.EventOutput:
			if label, ok := strings.CutPrefix(e.Output, "STEP "); ok {
				fmt.Sscanf(label, "%d", &step)
			}
			stream.Row(n, step, "OUT", nil, nil, nil, nil, e.Output)
		}
	}

	problems := set.Table("problems", "rule", "detail")
	calls := map[string]int{}
	writesOK, readsOK := 0, 0
	var dirSyncs []string
	syncCalls := 0
	var walOpenedWithCreate, shmOpenedInMap, walUnlinkedInDelete, shmUnlinkedInUnmap bool
	syncsPerFile := map[string]int{}
	writeProblems, readProblems, syncProblems := 0, 0, 0
	for i, e := range m.Events {
		if e.Kind != trace.EventVFS {
			continue
		}
		c := e.VFS
		seen := syscallsOf(i)
		calls[c.Method]++
		switch c.Method {
		case "xWrite":
			if len(seen) == 1 && seen[0].Name == "pwrite64" && seen[0].Path == c.File && seen[0].Offset == c.Offset && seen[0].Length == c.Amount && seen[0].Result == c.Amount {
				writesOK++
			} else {
				writeProblems++
				problems.Row("xWrite", fmt.Sprintf("%s encloses %v", c.String(), seen))
			}
		case "xRead":
			if len(seen) == 1 && seen[0].Name == "pread64" && seen[0].Path == c.File && seen[0].Offset == c.Offset && seen[0].Length == c.Amount {
				readsOK++
			} else {
				readProblems++
				problems.Row("xRead", fmt.Sprintf("%s encloses %v", c.String(), seen))
			}
		case "xSync":
			syncCalls++
			own := 0
			for _, s := range seen {
				if s.Name != "sync" {
					continue
				}
				if s.Path == c.File {
					own++
					syncsPerFile[c.File]++
				} else if s.Path == "." {
					dirSyncs = append(dirSyncs, fmt.Sprintf("inside xSync(%s) number %d", c.File, syncsPerFile[c.File]))
				}
			}
			if own != 1 {
				syncProblems++
				problems.Row("xSync", fmt.Sprintf("%s encloses %d syncs of its own file", c.String(), own))
			}
		case "xOpen":
			if c.File == dbName+"-wal" {
				for _, s := range seen {
					if s.Name == "open" && s.Path == dbName+"-wal" && strings.Contains(s.Flags, "O_CREAT") {
						walOpenedWithCreate = true
					}
				}
			}
		case "xShmMap":
			for _, s := range seen {
				if s.Name == "open" && s.Path == dbName+"-shm" && strings.Contains(s.Flags, "O_CREAT") {
					shmOpenedInMap = true
				}
			}
		case "xDelete":
			for _, s := range seen {
				if s.Name == "unlink" && s.Path == dbName+"-wal" {
					walUnlinkedInDelete = true
				}
			}
		case "xShmUnmap":
			for _, s := range seen {
				if s.Name == "unlink" && s.Path == dbName+"-shm" {
					shmUnlinkedInUnmap = true
				}
			}
		}
	}

	outsideIO := 0
	for _, i := range kept {
		e := m.Events[i]
		if e.Kind != trace.EventSyscall {
			continue
		}
		name := trace.CanonicalName(e.Syscall.Name)
		if !slices.Contains(byteMovingSyscalls, name) {
			continue
		}
		if e.Enclosed < 0 {
			outsideIO++
			problems.Row("outside", syscallLine(*e.Syscall)+" (outside any VFS call)")
			continue
		}
		method := m.Events[e.Enclosed].VFS.Method
		if method != "xRead" && method != "xWrite" && method != "xShmMap" {
			outsideIO++
			problems.Row("outside", syscallLine(*e.Syscall)+" inside "+method)
		}
	}

	methods := set.Table("vfs_methods", "method", "unix_function", "calls")
	names := make([]string, 0, len(calls))
	for method := range calls {
		names = append(names, method)
	}
	sort.Strings(names)
	for _, method := range names {
		methods.Row(method, unixFunction[method], calls[method])
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		fatal("read dir", err)
	}
	filesLeft := set.Table("files_left", "name")
	var left []string
	for _, e := range entries {
		if e.Name() == results.DirName {
			continue
		}
		left = append(left, e.Name())
		filesLeft.Row(e.Name())
	}

	set.Check("B", calls["xWrite"] > 0 && writeProblems == 0, fmt.Sprintf("each of the %d xWrite calls made exactly one pwrite64 to the same file, offset and length", writesOK))
	set.Check("B", calls["xRead"] > 0 && readProblems == 0, fmt.Sprintf("each of the %d xRead calls made exactly one pread64 of the same file, offset and length", readsOK))
	set.Check("B", outsideIO == 0, "every pread64/pwrite64/read/write/writev on the database, journal, WAL and shm files happened inside xRead, xWrite or xShmMap")
	set.Check("C", syncCalls > 0 && syncProblems == 0, fmt.Sprintf("each of the %d xSync calls made exactly one fsync/fdatasync of its own file", syncCalls))
	set.Check("C", len(dirSyncs) == 2 && strings.Contains(dirSyncs[0], "-journal) number 1") && strings.Contains(dirSyncs[1], "-wal) number 1"), "the directory is synced twice, inside the first xSync of the new journal and of the new WAL (unixSync's one-time directory sync)")
	set.Check("D", walOpenedWithCreate, "the -wal file is created (O_CREAT) at its first use, by xOpen")
	set.Check("D", shmOpenedInMap, "the -shm file is created inside the first xShmMap")
	set.Check("D", walUnlinkedInDelete && shmUnlinkedInUnmap, "at the last close xDelete unlinks the -wal and xShmUnmap unlinks the -shm")
	set.Check("D", slices.Equal(left, []string{dbName}), "only the database file is left after the CLI exits")
}

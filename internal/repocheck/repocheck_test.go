package repocheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TestCheckSources verifies SOURCES.sha256 matching and reporting of drift, missing files, stray files, and root .go files.
func TestCheckSources(t *testing.T) {
	tree := t.TempDir()
	root := t.TempDir()
	writeFile(t, filepath.Join(tree, "src", "wal.c"), "int wal;\n")
	writeFile(t, filepath.Join(tree, "src", "pager.c"), "int pager;\n")
	chapter := Chapter{Number: "05", Dir: "ch05-wal", Path: filepath.Join(root, "ch05-wal")}
	writeFile(t, filepath.Join(chapter.Path, "ch05-wal.md"), "# x\n")
	writeFile(t, filepath.Join(chapter.Path, "wal.c"), "int wal;\n")
	writeFile(t, filepath.Join(chapter.Path, "pager.c"), "int pager;\n")
	writeFile(t, filepath.Join(chapter.Path, "SOURCES.sha256"),
		SourcesLine(sha("int wal;\n"), "src/wal.c")+"\n"+SourcesLine(sha("int pager;\n"), "src/pager.c")+"\n")
	problems, count, err := CheckSources(chapter, tree)
	if err != nil || len(problems) != 0 || count != 2 {
		t.Fatalf("clean chapter: %v %d %v", problems, count, err)
	}
	writeFile(t, filepath.Join(chapter.Path, "wal.c"), "int wal; /* edited */\n")
	writeFile(t, filepath.Join(chapter.Path, "stray.h"), "x")
	writeFile(t, filepath.Join(chapter.Path, "helper.go"), "package x\n")
	writeFile(t, filepath.Join(chapter.Path, "SOURCES.sha256"),
		SourcesLine(sha("int wal;\n"), "src/wal.c")+"\n"+SourcesLine(sha("stale\n"), "src/pager.c")+"\n"+
			SourcesLine(sha("x"), "src/missing.c")+"\nnot a line\n")
	problems, _, err = CheckSources(chapter, tree)
	if err != nil {
		t.Fatal(err)
	}
	text := joinProblems(problems)
	for _, want := range []string{"copy wal.c differs", "listed sha256", "src/missing.c is not in the pinned tree", "not '<sha256>  <path>'", "stray.h", "holds no Go files"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing problem %q in:\n%s", want, text)
		}
	}
}

func joinProblems(problems []Problem) string {
	var lines []string
	for _, problem := range problems {
		lines = append(lines, problem.String())
	}
	return strings.Join(lines, "\n")
}

// TestCheckSpans checks SPANS.txt symbol matching on word boundaries, out-of-range lines, and missing files.
func TestCheckSpans(t *testing.T) {
	tree := t.TempDir()
	root := t.TempDir()
	var source strings.Builder
	for i := 1; i <= 40; i++ {
		switch i {
		case 10:
			source.WriteString("static int unixLock(sqlite3_file *id, int eFileLock){\n")
		case 20:
			source.WriteString("    case OP_Insert: {\n")
		case 30:
			source.WriteString("int unixLockHelper(void){\n")
		default:
			source.WriteString("  x++;\n")
		}
	}
	writeFile(t, filepath.Join(tree, "src", "os_unix.c"), source.String())
	chapter := Chapter{Dir: "ch06-locks", Path: filepath.Join(root, "ch06-locks")}
	writeFile(t, filepath.Join(chapter.Path, "SPANS.txt"), "# file start end symbol\nsrc/os_unix.c 9 25 unixLock\nsrc/os_unix.c 20 22 OP_Insert\n")
	problems, count, err := CheckSpans(chapter, tree)
	if err != nil || len(problems) != 0 || count != 2 {
		t.Fatalf("good spans: %v %d %v", problems, count, err)
	}
	writeFile(t, filepath.Join(chapter.Path, "SPANS.txt"), strings.Join([]string{
		"src/os_unix.c 25 35 unixLock",
		"src/os_unix.c 1 50 unixLock",
		"src/os_unix.c 30 31 unixLockHelp",
		"src/nope.c 1 2 x",
		"src/os_unix.c 5 3 x",
		"too few fields",
	}, "\n"))
	problems, _, err = CheckSpans(chapter, tree)
	if err != nil {
		t.Fatal(err)
	}
	text := joinProblems(problems)
	for _, want := range []string{"SPANS.txt:1: unixLock not found in lines 25-35", "SPANS.txt:2: src/os_unix.c has 40 lines", "SPANS.txt:3: unixLockHelp not found", "src/nope.c is not in the pinned tree", "bad line range", "want 'file start end symbol'"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing problem %q in:\n%s", want, text)
		}
	}
	if _, count, err := CheckSpans(Chapter{Path: t.TempDir()}, tree); err != nil || count != 0 {
		t.Fatalf("chapter without SPANS.txt: %d %v", count, err)
	}
}

// TestInline exercises include-block checking, rewriting, line slices, dynamic backtick fences, and error cases.
func TestInline(t *testing.T) {
	dir := t.TempDir()
	code := "package main\n\nfunc main() {}\n"
	writeFile(t, filepath.Join(dir, "experiments", "1.1-a", "main.go"), code)
	writeFile(t, filepath.Join(dir, "evidence", "1.1-a.txt"), "line one\nline two\nline three\n")
	writeFile(t, filepath.Join(dir, "ticks.go"), "package x\n\nvar s = `\n```\n`\n")
	markdown := strings.Join([]string{
		"# Chapter",
		"<!-- include: experiments/1.1-a/main.go -->",
		"```go",
		"stale",
		"```",
		"text",
		"<!-- include: evidence/1.1-a.txt lines=2-3 -->",
		"",
		"<!-- include: ticks.go -->",
		"```go",
		"```",
		"end",
		"",
	}, "\n")
	md := filepath.Join(dir, "ch01-x.md")
	writeFile(t, md, markdown)
	result, err := InlineFile(md, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Problems) != 3 {
		t.Fatalf("check problems: %v", result.Problems)
	}
	data, _ := os.ReadFile(md)
	if string(data) != markdown {
		t.Fatal("-check modified the file")
	}
	result, err = InlineFile(md, false)
	if err != nil || len(result.Problems) != 0 || result.Changed != 3 {
		t.Fatalf("rewrite: %v %d %v", result.Problems, result.Changed, err)
	}
	data, _ = os.ReadFile(md)
	want := strings.Join([]string{
		"# Chapter",
		"<!-- include: experiments/1.1-a/main.go -->",
		"```go",
		"package main",
		"",
		"func main() {}",
		"```",
		"text",
		"<!-- include: evidence/1.1-a.txt lines=2-3 -->",
		"```text",
		"line two",
		"line three",
		"```",
		"",
		"<!-- include: ticks.go -->",
		"````go",
		"package x",
		"",
		"var s = `",
		"```",
		"`",
		"````",
		"end",
		"",
	}, "\n")
	if string(data) != want {
		t.Fatalf("rewritten:\n%s\nwant:\n%s", data, want)
	}
	result, err = InlineFile(md, true)
	if err != nil || len(result.Problems) != 0 || len(result.Includes) != 3 {
		t.Fatalf("after rewrite: %v %v", result.Problems, err)
	}
	writeFile(t, filepath.Join(dir, "experiments", "1.1-a", "main.go"), code+"\n")
	result, _ = InlineFile(md, true)
	if len(result.Problems) != 1 || !strings.Contains(result.Problems[0].String(), "ch01-x.md:2") {
		t.Fatalf("byte-level change not detected: %v", result.Problems)
	}
	writeFile(t, md, "<!-- include: missing.go -->\n```go\n```\n<!-- include: evidence/1.1-a.txt lines=2-9 -->\n```text\n```\n")
	result, _ = InlineFile(md, false)
	if len(result.Problems) != 2 {
		t.Fatalf("missing targets: %v", result.Problems)
	}
	writeFile(t, md, "<!-- include: ticks.go -->\n```go\nnever closed\n")
	result, _ = InlineFile(md, true)
	if len(result.Problems) != 1 || !strings.Contains(result.Problems[0].Message, "never closed") {
		t.Fatalf("unclosed fence: %v", result.Problems)
	}
}

// TestIsDirective confirms Go compiler/tool directives are distinguished from ordinary comments.
func TestIsDirective(t *testing.T) {
	for comment, want := range map[string]bool{
		"//go:build linux":     true,
		"//go:embed PIN":       true,
		"//go:generate x":      true,
		"// +build linux":      true,
		"//line a.go:1":        true,
		"//export goFunc":      true,
		"//nolint:all":         true,
		"// go:build linux":    false,
		"// plain comment":     false,
		"/* block */":          false,
		"//Plain:not":          false,
		"// TODO: something":   false,
		"//":                   false,
		"//http://example.com": false,
		"//note:unauthorized prose disguised with a colon": false,
		"//todo:fix this later":                            false,
		"//go:this is prose":                               false,
		"//go:noinline":                                    true,
		"//go:linkname local remote":                       true,
		"//nolint:errcheck,gosec":                          true,
		"//nolint:errcheck because the error is ignored":   false,
	} {
		if got := IsDirective(comment); got != want {
			t.Errorf("IsDirective(%q) = %v, want %v", comment, got, want)
		}
	}
}

const codeOnly = `package demo

import "fmt"

type Point struct {
	X int
	LongName string
}

func Add(a, b int) int {
	sum := a + b
	fmt.Println(sum)
	return sum
}
`

const codeCommented = `// Package demo adds numbers.
package demo

import "fmt"

// Point is a point.
type Point struct {
	X        int    // the x coordinate
	LongName string // a long name
}

/*
Add adds two numbers.
*/
func Add(a, b int) int {
	// First compute the sum.
	sum := a + b

	// Then print it.
	fmt.Println(sum) /* inline */
	return sum
}
`

// TestSameCode verifies that comment-only edits pass while operator, literal, directive, or syntax changes fail.
func TestSameCode(t *testing.T) {
	same, detail, err := SameCode("demo.go", []byte(codeOnly), []byte(codeCommented))
	if err != nil || !same {
		t.Fatalf("comment-only change rejected: %s %v", detail, err)
	}
	changed := strings.Replace(codeCommented, "a + b", "a - b", 1)
	if same, _, _ := SameCode("demo.go", []byte(codeOnly), []byte(changed)); same {
		t.Fatal("operator change accepted")
	}
	changed = strings.Replace(codeCommented, `fmt.Println(sum)`, `fmt.Println("sum")`, 1)
	if same, _, _ := SameCode("demo.go", []byte(codeOnly), []byte(changed)); same {
		t.Fatal("literal change accepted")
	}
	withDirective := "//go:build linux\n\n" + codeOnly
	if same, _, _ := SameCode("demo.go", []byte(codeOnly), []byte(withDirective)); same {
		t.Fatal("added build directive accepted")
	}
	if _, _, err := SameCode("demo.go", []byte(codeOnly), []byte("package demo\nfunc (")); err == nil {
		t.Fatal("unparsable new revision accepted")
	}
}

type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := &gitRepo{t: t, dir: t.TempDir()}
	repo.git("", "init", "-q", "-b", "main")
	return repo
}

func (r *gitRepo) git(author string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=Builder", "-c", "user.email=b@example.com"}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	if author != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME="+author, "GIT_AUTHOR_EMAIL=a@example.com")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *gitRepo) commit(author, message string, files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		path := filepath.Join(r.dir, filepath.FromSlash(name))
		if content == "" {
			os.Remove(path)
			continue
		}
		writeFile(r.t, path, content)
	}
	r.git(author, "add", "-A")
	r.git(author, "commit", "-q", "-m", message)
	return r.git("", "rev-parse", "HEAD")
}

// TestCheckComments runs CheckComments across commits to catch code changes, added files, and removed files.
func TestCheckComments(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.commit("Opus", "code", map[string]string{"pkg/demo.go": codeOnly, "kb/vendored.go": "package kb\n", "README.md": "x\n"})
	prose := repo.commit("Gemini 4 Neon", "comments", map[string]string{"pkg/demo.go": codeCommented, "kb/vendored.go": "package kb\n\nvar Changed = 1\n", "README.md": "y\n"})
	problems, count, err := CheckComments(repo.dir, base, prose, CommentsOptions{})
	if err != nil || len(problems) != 0 || count != 1 {
		t.Fatalf("comment-only pass: %v %d %v", problems, count, err)
	}
	bad := repo.commit("Gemini 4 Neon", "sneaky", map[string]string{"pkg/demo.go": strings.Replace(codeCommented, "return sum", "return sum + 1", 1), "pkg/new.go": "package demo\n"})
	problems, _, err = CheckComments(repo.dir, base, bad, CommentsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	text := joinProblems(problems)
	if !strings.Contains(text, "pkg/demo.go: code changed") || !strings.Contains(text, "pkg/new.go: added") {
		t.Fatalf("problems:\n%s", text)
	}
	removed := repo.commit("Opus", "remove", map[string]string{"pkg/new.go": ""})
	problems, _, _ = CheckComments(repo.dir, bad, removed, CommentsOptions{})
	if !strings.Contains(joinProblems(problems), "pkg/new.go: removed") {
		t.Fatalf("removal not reported: %v", problems)
	}
}

// TestCheckProse verifies git-blame authorship checks on Go comments and chapter markdown while exempting include blocks.
func TestCheckProse(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("Research Agent", "old", map[string]string{"agent_work/probe.go": "// old research comment\npackage probe\n"})
	base := repo.commit("Claude Opus 5.5", "code", map[string]string{
		"pkg/demo.go":                       codeOnly,
		"ch01-intro/experiments/1.1-a/x.go": "package main\n\nfunc main() {}\n",
	})
	chapter := strings.Join([]string{
		"# Chapter one",
		"",
		"Some prose.",
		"<!-- include: experiments/1.1-a/x.go -->",
		"```go",
		"package main",
		"",
		"func main() {}",
		"```",
		"",
	}, "\n")
	repo.commit("Gemini 4 Neon", "prose", map[string]string{
		"pkg/demo.go":              codeCommented,
		"ch01-intro/ch01-intro.md": chapter,
		"ch01-intro/answers.md":    "1. Yes.\n",
	})
	head := repo.commit("Claude Opus 5.5", "inline refresh", map[string]string{
		"ch01-intro/ch01-intro.md": strings.Replace(chapter, "func main() {}\n```", "func main() {\n}\n```", 1),
	})
	problems, counts, err := CheckProse(repo.dir, base, head, ProseOptions{})
	if err != nil || len(problems) != 0 {
		t.Fatalf("clean prose: %v %v", problems, err)
	}
	if counts.InRange < 10 || counts.BeforeRange != 1 {
		t.Fatalf("counts %+v", counts)
	}
	bad := repo.commit("Claude Opus 5.5", "opus writes prose", map[string]string{
		"pkg/demo.go":              strings.Replace(codeCommented, "// Then print it.", "// Then print it, says Opus.", 1),
		"ch01-intro/ch01-intro.md": strings.Replace(chapter, "Some prose.", "Some prose by Opus.", 1),
	})
	problems, _, err = CheckProse(repo.dir, base, bad, ProseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	text := joinProblems(problems)
	if len(problems) != 2 || !strings.Contains(text, "pkg/demo.go:19") || !strings.Contains(text, "ch01-intro/ch01-intro.md:3") || !strings.Contains(text, `"Claude Opus 5.5"`) {
		t.Fatalf("problems:\n%s", text)
	}
	problems, _, _ = CheckProse(repo.dir, base, bad, ProseOptions{Exclude: []string{"pkg"}})
	if len(problems) != 1 {
		t.Fatalf("exclude ignored: %v", problems)
	}
	if _, _, err := ParseRange("abc"); err == nil {
		t.Fatal("range without .. accepted")
	}
	if _, _, err := ParseRange("a...b"); err == nil {
		t.Fatal("three-dot range accepted")
	}
}

// TestGofmtAndPathSweeps checks CheckGofmt and CheckPaths exclusions on a temporary git repository.
func TestGofmtAndPathSweeps(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("Opus", "files", map[string]string{
		"good.go":          "package x\n",
		"bad.go":           "package x\nvar  y = 1\n",
		"kb/ignored.go":    "package kb\nvar  z = 1\n",
		"nested/go.mod":    "module nested\n",
		"nested/skip.go":   "package nested\nvar  z = 1\n",
		"docs/notes.md":    "see " + ForbiddenPathFragment + "Software/x\n",
		"agent_work/ok.md": ForbiddenPathFragment + "is fine here\n",
		"STATE.md":         ForbiddenPathFragment + " fine\n",
		"docs/clean.md":    "nothing local\n",
	})
	problems, count, err := CheckGofmt(repo.dir)
	if err != nil || count != 2 || len(problems) != 1 || problems[0].Where != "bad.go" {
		t.Fatalf("gofmt: %v %d %v", problems, count, err)
	}
	problems, _, err = CheckPaths(repo.dir)
	if err != nil || len(problems) != 1 || problems[0].Where != "docs/notes.md:1" {
		t.Fatalf("paths: %v %v", problems, err)
	}
}

// TestChapters checks chapter directory discovery, sorting, filtering, and missing-chapter errors.
func TestChapters(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"ch02-file-format", "ch01-architecture", "chapter-notes", "ch1-bad"} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
	}
	chapters, err := Chapters(root, "")
	if err != nil || len(chapters) != 2 || chapters[0].Dir != "ch01-architecture" {
		t.Fatalf("chapters %v %v", chapters, err)
	}
	chapters, err = Chapters(root, "ch02")
	if err != nil || len(chapters) != 1 || chapters[0].Markdown() != filepath.Join(root, "ch02-file-format", "ch02-file-format.md") {
		t.Fatalf("ch02 %v %v", chapters, err)
	}
	if _, err := Chapters(root, "ch09"); err == nil {
		t.Fatal("missing chapter accepted")
	}
}

// TestCheckEvidence builds and runs synthetic experiments covering PASS, DIFF, MISSING, FAIL, and numeric ordering.
func TestCheckEvidence(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/evidence\n\ngo 1.26\n")
	chapter := Chapter{Number: "01", Dir: "ch01-x", Path: filepath.Join(root, "ch01-x")}
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.2-stable", "main.go"),
		"package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {\n\tfmt.Println(\"claim holds\")\n\tfmt.Println(\"INFO: pid\", os.Getpid())\n}\n")
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.10-fails", "main.go"),
		"package main\n\nimport \"os\"\n\nfunc main() { os.Exit(3) }\n")
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.3-drift", "main.go"),
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"new output\") }\n")
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.4-noevidence", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(chapter.Path, "evidence", "1.2-stable.txt"), "claim holds\nINFO: pid 1\n")
	writeFile(t, filepath.Join(chapter.Path, "evidence", "1.10-fails.txt"), "")
	writeFile(t, filepath.Join(chapter.Path, "evidence", "1.3-drift.txt"), "old output\n")
	experiments, err := Experiments(chapter)
	if err != nil || len(experiments) != 4 {
		t.Fatalf("experiments %v %v", experiments, err)
	}
	var order []string
	statuses := map[string]string{}
	for _, experiment := range experiments {
		order = append(order, experiment.Name)
		outcome := CheckEvidence(context.Background(), root, experiment, 2)
		statuses[experiment.Name] = outcome.Status
	}
	if strings.Join(order, ",") != "1.2-stable,1.3-drift,1.4-noevidence,1.10-fails" {
		t.Fatalf("order %v", order)
	}
	want := map[string]string{"1.2-stable": "PASS", "1.3-drift": "DIFF", "1.4-noevidence": "MISSING", "1.10-fails": "FAIL"}
	for name, status := range want {
		if statuses[name] != status {
			t.Errorf("%s: %s, want %s", name, statuses[name], status)
		}
	}
}

// TestParseBlamePorcelain checks parsing of repeated commit headers, author names, and boundary markers.
func TestParseBlamePorcelain(t *testing.T) {
	text := strings.Join([]string{
		"1111111111111111111111111111111111111111 1 1 2",
		"author Gemini 4 Neon",
		"summary prose",
		"\t// first",
		"1111111111111111111111111111111111111111 2 2",
		"\t// second",
		"2222222222222222222222222222222222222222 5 3 1",
		"author Old Agent",
		"boundary",
		"\told line",
	}, "\n")
	lines, err := ParseBlamePorcelain(text)
	if err != nil || len(lines) != 3 {
		t.Fatalf("lines %v %v", lines, err)
	}
	if lines[1].Author != "Gemini 4 Neon" || lines[1].Line != 2 || lines[1].Text != "// second" {
		t.Fatalf("line 2 %+v", lines[1])
	}
	if !lines[2].Boundary || lines[2].Line != 3 {
		t.Fatalf("line 3 %+v", lines[2])
	}
}

// TestCheckEvidenceTables verifies an experiment with a report.sql is checked by its CSV tables, that "info" tables are ignored, and that a changed table is a DIFF.
func TestCheckEvidenceTables(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := t.TempDir()
	t.Setenv("COURSE_WORK_DIR", filepath.Join(root, "work"))
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/evidence\n\ngo 1.26\n")
	chapter := Chapter{Number: "01", Dir: "ch01-x", Path: filepath.Join(root, "ch01-x")}
	program := `package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	dir := filepath.Join(os.Getenv("COURSE_WORK_DIR"), "1.5", "results")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "rows.csv"), []byte("n,name\n1,one\n2,two\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "info_run.csv"), []byte(fmt.Sprintf("pid\n%d\n", os.Getpid())), 0o644)
	fmt.Println("TABLES OK")
}
`
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.5-tables", "main.go"), program)
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.5-tables", "report.sql"), ".print 'rows'\nSELECT * FROM rows ORDER BY n;\n")
	experiments, err := Experiments(chapter)
	if err != nil || len(experiments) != 1 {
		t.Fatalf("experiments %v %v", experiments, err)
	}
	experiment := experiments[0]
	if experiment.ID() != "1.5" || !experiment.HasReport() {
		t.Fatalf("experiment %+v", experiment)
	}
	ctx := context.Background()
	if outcome := CheckEvidence(ctx, root, experiment, 1); outcome.Status != "MISSING" {
		t.Fatalf("no evidence: %+v", outcome)
	}
	writeFile(t, experiment.Evidence, "placeholder\n")
	if outcome := CheckEvidence(ctx, root, experiment, 1); outcome.Status != "DIFF" {
		t.Fatalf("no stored tables: %+v", outcome)
	}
	results := experiment.ResultsDir(root)
	rendered := "not rendered\n"
	if _, err := DuckDB(ctx); err == nil {
		if err := UpdateEvidence(ctx, experiment, results, "TABLES OK"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(experiment.Evidence)
		if err != nil {
			t.Fatal(err)
		}
		rendered = string(data)
		if !strings.Contains(rendered, "│ 2 │ two  │") || !strings.Contains(rendered, "Experiment verdict: TABLES OK") {
			t.Fatalf("rendered report:\n%s", rendered)
		}
		if _, err := os.Stat(filepath.Join(experiment.EvidenceDir(), "info_run.csv")); err == nil {
			t.Fatal("an info table was stored as evidence")
		}
	} else {
		writeFile(t, experiment.Evidence, rendered)
		writeFile(t, filepath.Join(experiment.EvidenceDir(), "rows.csv"), "n,name\n1,one\n2,two\n")
	}
	if outcome := CheckEvidence(ctx, root, experiment, 2); outcome.Status != "PASS" {
		t.Fatalf("same tables: %+v", outcome)
	}
	writeFile(t, filepath.Join(experiment.EvidenceDir(), "rows.csv"), "n,name\n1,one\n2,TWO\n")
	if outcome := CheckEvidence(ctx, root, experiment, 1); outcome.Status != "DIFF" || !strings.Contains(outcome.Detail, "rows.csv") {
		t.Fatalf("changed table: %+v", outcome)
	}
	writeFile(t, filepath.Join(experiment.EvidenceDir(), "rows.csv"), "n,name\n1,one\n2,two\n")
	writeFile(t, filepath.Join(experiment.EvidenceDir(), "extra.csv"), "a\n")
	if outcome := CheckEvidence(ctx, root, experiment, 1); outcome.Status != "DIFF" || !strings.Contains(outcome.Detail, "extra") {
		t.Fatalf("extra stored table: %+v", outcome)
	}
}

// TestWalkthroughScript verifies the generated DuckDB script loads each CSV as a table, reads the report, lists the checks, and restores the shell defaults only when interactive.
func TestWalkthroughScript(t *testing.T) {
	results := filepath.Join(t.TempDir(), "results")
	if err := os.MkdirAll(results, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := WalkthroughScript(results, "/x/report.sql", "", "OK", false); err == nil {
		t.Fatal("no error for a results directory without CSV tables")
	}
	writeFile(t, filepath.Join(results, "inserts.csv"), "a\n1\n")
	script, err := WalkthroughScript(results, "/x/it's/report.sql", "", "X OK", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CREATE TABLE inserts AS FROM read_csv('inserts.csv', sample_size=-1);\n",
		".read '/x/it''s/report.sql'\n",
		".print 'Experiment verdict: X OK'\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, ".mode duckbox") || strings.Contains(script, "FROM checks") || strings.Contains(script, "info.sql") {
		t.Errorf("unexpected interactive ending or checks section:\n%s", script)
	}
	writeFile(t, filepath.Join(results, "checks.csv"), "seq,part,status,claim\n1,A,ok,x\n")
	interactive, _ := WalkthroughScript(results, "/x/report.sql", "/x/info.sql", "X OK", true)
	if !strings.Contains(interactive, ".read '/x/report.sql'\n.read '/x/info.sql'\n") || !strings.Contains(interactive, "FROM checks ORDER BY seq") || !strings.HasSuffix(interactive, ".mode duckbox\n.nullvalue NULL\n") {
		t.Errorf("interactive script:\n%s", interactive)
	}
}

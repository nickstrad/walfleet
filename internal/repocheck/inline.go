package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var includeMarker = regexp.MustCompile(`^<!-- include: (\S+)(?: lines=(\d+)-(\d+))? -->$`)

var fenceOpen = regexp.MustCompile("^(`{3,})(\\S*)\\s*$")

var outerFenceOpen = regexp.MustCompile("^(`{3,}|~{3,})")

func outerFenceEnd(lines []string, start int) int {
	open := outerFenceOpen.FindString(lines[start])
	if open == "" {
		return -1
	}
	for j := start + 1; j < len(lines); j++ {
		closing := strings.TrimRight(lines[j], " \t\r\n")
		if len(closing) >= len(open) && strings.Trim(closing, open[:1]) == "" {
			return j
		}
	}
	return len(lines) - 1
}

// Include describes one "<!-- include: rel/path [lines=A-B] -->" marker and
// the fenced code block immediately following it in a chapter markdown file.
type Include struct {
	MarkerLine int
	Target     string
	FromLine   int
	ToLine     int
	FenceStart int
	FenceEnd   int
	Language   string
	Fence      string
	Body       string
}

// InlineResult holds the parsed include blocks, any detected drift or syntax
// problems, the regenerated markdown text, and the count of blocks that changed.
type InlineResult struct {
	Includes []Include
	Problems []Problem
	Output   string
	Changed  int
}

func splitKeepingNewlines(text string) []string {
	var lines []string
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

func languageFor(target string) string {
	if strings.HasSuffix(target, ".go") {
		return "go"
	}
	if strings.HasSuffix(target, ".sql") {
		return "sql"
	}
	return "text"
}

func includeContent(baseDir string, include Include) (string, error) {
	data, err := os.ReadFile(filepath.Join(baseDir, filepath.FromSlash(include.Target)))
	if err != nil {
		return "", err
	}
	text := string(data)
	if include.FromLine > 0 {
		lines := splitKeepingNewlines(text)
		if include.ToLine > len(lines) {
			return "", fmt.Errorf("lines=%d-%d but %s has %d lines", include.FromLine, include.ToLine, include.Target, len(lines))
		}
		text = strings.Join(lines[include.FromLine-1:include.ToLine], "")
	}
	// Fenced markdown blocks always end on a newline before the closing fence.
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text, nil
}

// fenceFor chooses a backtick fence longer than any backtick run inside content
// so raw Go strings containing triple backticks cannot close the block early.
func fenceFor(content string) string {
	longest := 0
	run := 0
	for _, c := range content {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// FindIncludeBlocks scans markdown text and returns every include marker paired
// with its immediately following closed fenced block (used by CheckProse to
// exempt inlined code from prose authorship checks), ignoring markers inside
// other backtick or tilde code fences.
func FindIncludeBlocks(text string) []Include {
	lines := splitKeepingNewlines(text)
	var blocks []Include
	for i := 0; i < len(lines); i++ {
		if end := outerFenceEnd(lines, i); end >= 0 {
			i = end
			continue
		}
		m := includeMarker.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
		if m == nil || i+1 >= len(lines) {
			continue
		}
		open := fenceOpen.FindStringSubmatch(strings.TrimRight(lines[i+1], "\r\n"))
		if open == nil {
			continue
		}
		for j := i + 2; j < len(lines); j++ {
			if strings.TrimRight(lines[j], "\r\n") == open[1] {
				blocks = append(blocks, Include{MarkerLine: i + 1, Target: m[1], FenceStart: i + 2, FenceEnd: j + 1, Fence: open[1], Language: open[2], Body: strings.Join(lines[i+2:j], "")})
				i = j
				break
			}
		}
	}
	return blocks
}

// ProcessInline resolves every include marker in text relative to markdownPath's
// directory (ignoring markers inside other backtick or tilde code fences),
// reports any missing or stale blocks in Result.Problems, and builds the
// synchronized markdown output.
func ProcessInline(markdownPath string, text string) InlineResult {
	baseDir := filepath.Dir(markdownPath)
	lines := splitKeepingNewlines(text)
	var result InlineResult
	var out strings.Builder
	for i := 0; i < len(lines); i++ {
		if end := outerFenceEnd(lines, i); end >= 0 {
			out.WriteString(strings.Join(lines[i:end+1], ""))
			i = end
			continue
		}
		line := lines[i]
		out.WriteString(line)
		m := includeMarker.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			continue
		}
		include := Include{MarkerLine: i + 1, Target: m[1]}
		where := fmt.Sprintf("%s:%d", filepath.Base(markdownPath), i+1)
		if m[2] != "" {
			include.FromLine, _ = strconv.Atoi(m[2])
			include.ToLine, _ = strconv.Atoi(m[3])
			if include.FromLine < 1 || include.ToLine < include.FromLine {
				result.Problems = append(result.Problems, Problem{where, fmt.Sprintf("bad lines=%s-%s", m[2], m[3])})
				continue
			}
		}
		want, err := includeContent(baseDir, include)
		if err != nil {
			result.Problems = append(result.Problems, Problem{where, err.Error()})
			continue
		}
		include.Language = languageFor(include.Target)
		hasBlock := false
		if i+1 < len(lines) {
			if open := fenceOpen.FindStringSubmatch(strings.TrimRight(lines[i+1], "\r\n")); open != nil {
				closeAt := -1
				for j := i + 2; j < len(lines); j++ {
					if strings.TrimRight(lines[j], "\r\n") == open[1] {
						closeAt = j
						break
					}
				}
				if closeAt < 0 {
					result.Problems = append(result.Problems, Problem{where, "code fence after the include marker is never closed"})
					continue
				}
				hasBlock = true
				include.Fence = open[1]
				include.FenceStart = i + 2
				include.FenceEnd = closeAt + 1
				if open[2] != "" {
					include.Language = open[2]
				}
				include.Body = strings.Join(lines[i+2:closeAt], "")
				i = closeAt
			}
		}
		if !hasBlock {
			include.Fence = fenceFor(want)
			result.Problems = append(result.Problems, Problem{where, "include marker is not immediately followed by a fenced block"})
		} else if include.Body != want {
			result.Problems = append(result.Problems, Problem{where, fmt.Sprintf("block differs from %s%s (%s)", include.Target, linesSuffix(include), firstDifference(include.Body, want))})
		}
		if !hasBlock || include.Body != want {
			result.Changed++
		}
		fence := include.Fence
		if strings.Contains(want, fence) {
			fence = fenceFor(want)
		}
		out.WriteString(fence + include.Language + "\n")
		out.WriteString(want)
		out.WriteString(fence + "\n")
		result.Includes = append(result.Includes, include)
	}
	result.Output = out.String()
	return result
}

func linesSuffix(include Include) string {
	if include.FromLine == 0 {
		return ""
	}
	return fmt.Sprintf(" lines %d-%d", include.FromLine, include.ToLine)
}

func firstDifference(got, want string) string {
	gotLines := splitKeepingNewlines(got)
	wantLines := splitKeepingNewlines(want)
	for i := 0; i < max(len(gotLines), len(wantLines)); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			return fmt.Sprintf("first difference at line %d: have %q, want %q", i+1, strings.TrimRight(g, "\n"), strings.TrimRight(w, "\n"))
		}
	}
	return "identical"
}

// InlineFile checks or rewrites all include blocks in markdownPath. When check
// is true the file is never modified; when check is false it rewrites the file
// unless there are fatal errors (missing target files, bad line ranges, or
// unclosed fences).
func InlineFile(markdownPath string, check bool) (InlineResult, error) {
	data, err := os.ReadFile(markdownPath)
	if err != nil {
		return InlineResult{}, err
	}
	result := ProcessInline(markdownPath, string(data))
	if check || result.Changed == 0 {
		return result, nil
	}
	var fatal []Problem
	for _, problem := range result.Problems {
		if !strings.Contains(problem.Message, "block differs") && !strings.Contains(problem.Message, "not immediately followed") {
			fatal = append(fatal, problem)
		}
	}
	if len(fatal) > 0 {
		result.Problems = fatal
		return result, nil
	}
	result.Problems = nil
	return result, os.WriteFile(markdownPath, []byte(result.Output), 0o644)
}

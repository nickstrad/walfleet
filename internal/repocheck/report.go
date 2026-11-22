package repocheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DuckDBVersion is the DuckDB CLI release the lab image carries (Dockerfile.lab).
// A rendered report is compared with its stored copy only under this version.
const DuckDBVersion = "2.0.0"

// resultsDirName matches internal/results.DirName.
const resultsDirName = "results"

// ID returns the experiment number, such as "3.1" for "3.1-pages-per-insert".
func (e Experiment) ID() string {
	id, _, _ := strings.Cut(e.Name, "-")
	return id
}

// InfoReport returns the path of the optional info.sql next to report.sql. It
// presents the "info" tables (run-dependent values). "course run" reads it
// after the report; the stored evidence leaves it out.
func (e Experiment) InfoReport() string {
	return filepath.Join(e.Chapter.Path, "experiments", e.Name, "info.sql")
}

// Report returns the path of the experiment's DuckDB walkthrough, report.sql.
func (e Experiment) Report() string {
	return filepath.Join(e.Chapter.Path, "experiments", e.Name, "report.sql")
}

// HasReport reports whether the experiment writes CSV tables and has a
// report.sql. Its evidence is then EvidenceDir (the tables) and Evidence (the
// rendered report) instead of its stdout.
func (e Experiment) HasReport() bool {
	_, err := os.Stat(e.Report())
	return err == nil
}

// EvidenceDir returns the directory of stored CSV tables, evidence/<name>/.
func (e Experiment) EvidenceDir() string {
	return strings.TrimSuffix(e.Evidence, ".txt")
}

// ResultsDir returns where a run of the experiment writes its CSV tables:
// <work base>/<id>/results, with the work base $COURSE_WORK_DIR or <root>/_work.
func (e Experiment) ResultsDir(root string) string {
	base := os.Getenv("COURSE_WORK_DIR")
	if base == "" {
		base = filepath.Join(root, "_work")
	}
	return filepath.Join(base, e.ID(), resultsDirName)
}

// resultTables lists the CSV files in dir by table name. Tables whose name
// starts with "info" hold run-dependent values and are left out unless all is set.
func resultTables(dir string, all bool) ([]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, name := range names {
		table := strings.TrimSuffix(filepath.Base(name), ".csv")
		if all || !strings.HasPrefix(table, "info") {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	return tables, nil
}

func sqlQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

// VerdictLine returns the last non-empty line of an experiment's stdout ("... OK").
func VerdictLine(stdout string) string {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	return lines[len(lines)-1]
}

// WalkthroughScript builds the DuckDB script for one run: load every CSV in
// results as a table named after its file, read report, list the checks table
// if there is one, and print the verdict. A non-empty info names a script read
// after the report for run-dependent values. With interactive set, the script
// ends by restoring the shell's default output mode for the reader's own queries.
func WalkthroughScript(results, report, info, verdict string, interactive bool) (string, error) {
	tables, err := resultTables(results, true)
	if err != nil {
		return "", err
	}
	if len(tables) == 0 {
		return "", fmt.Errorf("the experiment wrote no CSV tables in %s", results)
	}
	var b strings.Builder
	b.WriteString(".mode box\n.nullvalue ''\n")
	hasChecks := false
	for _, table := range tables {
		hasChecks = hasChecks || table == "checks"
		// sample_size=-1: column types come from every row, not the first few thousand.
		fmt.Fprintf(&b, "CREATE TABLE %s AS FROM read_csv(%s, sample_size=-1);\n", table, sqlQuote(table+".csv"))
	}
	fmt.Fprintf(&b, ".read %s\n", sqlQuote(report))
	if info != "" {
		fmt.Fprintf(&b, ".read %s\n", sqlQuote(info))
	}
	if hasChecks {
		b.WriteString(checksSection)
	}
	fmt.Fprintf(&b, ".print\n.print %s\n", sqlQuote("Experiment verdict: "+verdict))
	if interactive {
		fmt.Fprintf(&b, ".print\n.print %s\n", sqlQuote("Tables: "+strings.Join(tables, ", ")+". Try DESCRIBE "+tables[0]+"; leave with .quit"))
		b.WriteString(".mode duckbox\n.nullvalue NULL\n")
	}
	return b.String(), nil
}

// checksSection ends every walkthrough the same way: the claims the experiment
// asserted while it ran, from the table internal/results.Set.Check writes.
const checksSection = `.print
.print '── Checks ─────────────────────────────────────────────────────────────'
.print
.print 'Every claim the experiment asserted while it ran:'
.print
.mode list
.headers off
.separator '  '
SELECT CAST(part AS VARCHAR), CASE status WHEN 'ok' THEN '✓' ELSE '✗ FAILED' END, claim FROM checks ORDER BY seq;
.print
SELECT count(*) FILTER (status = 'ok') || ' of ' || count(*) || ' claims hold' FROM checks;
.headers on
.mode box
`

// DuckDB returns the path of the duckdb CLI when it is installed at DuckDBVersion.
func DuckDB(ctx context.Context) (string, error) {
	path, err := exec.LookPath("duckdb")
	if err != nil {
		return "", errors.New("duckdb is not installed")
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("duckdb --version: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "v"+DuckDBVersion+" ") {
		return "", fmt.Errorf("duckdb is %s, the reports need v%s", strings.TrimSpace(string(out)), DuckDBVersion)
	}
	return path, nil
}

// WriteWalkthrough writes the walkthrough script next to the CSV tables and returns its path.
func WriteWalkthrough(results, report, info, verdict string, interactive bool) (string, error) {
	script, err := WalkthroughScript(results, report, info, verdict, interactive)
	if err != nil {
		return "", err
	}
	path := filepath.Join(results, "walkthrough.sql")
	return path, os.WriteFile(path, []byte(script), 0o644)
}

// RenderReport runs the walkthrough for the tables in results, without
// info.sql, and returns the text DuckDB prints. This text is the stored evidence.
func RenderReport(ctx context.Context, duckdb, results, report, verdict string) (string, error) {
	script, err := WriteWalkthrough(results, report, "", verdict, false)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, duckdb, "-no-agent", "-bail", "-f", script)
	cmd.Dir = results
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("report.sql: %v: %s", err, lastLines(stderr.String(), 8))
	}
	if stderr.Len() > 0 {
		return "", fmt.Errorf("report.sql: %s", lastLines(stderr.String(), 8))
	}
	return stdout.String(), nil
}

// compareTables compares the CSV tables in results with the stored ones in
// evidenceDir and returns "" when the same tables hold the same bytes.
func compareTables(results, evidenceDir string) (string, error) {
	got, err := resultTables(results, false)
	if err != nil {
		return "", err
	}
	want, err := resultTables(evidenceDir, false)
	if err != nil {
		return "", err
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		return fmt.Sprintf("tables [%s], evidence has [%s]", strings.Join(got, " "), strings.Join(want, " ")), nil
	}
	for _, table := range got {
		a, err := os.ReadFile(filepath.Join(results, table+".csv"))
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(filepath.Join(evidenceDir, table+".csv"))
		if err != nil {
			return "", err
		}
		if !bytes.Equal(a, b) {
			return table + ".csv " + firstDifference(string(a), string(b)), nil
		}
	}
	return "", nil
}

// UpdateEvidence replaces the stored evidence of an experiment with the tables
// of its last run in results and the report rendered from them.
func UpdateEvidence(ctx context.Context, experiment Experiment, results, verdict string) error {
	duckdb, err := DuckDB(ctx)
	if err != nil {
		return err
	}
	rendered, err := RenderReport(ctx, duckdb, results, experiment.Report(), verdict)
	if err != nil {
		return err
	}
	dir := experiment.EvidenceDir()
	old, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return err
	}
	for _, name := range old {
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tables, err := resultTables(results, false)
	if err != nil {
		return err
	}
	for _, table := range tables {
		data, err := os.ReadFile(filepath.Join(results, table+".csv"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, table+".csv"), data, 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(experiment.Evidence, []byte(rendered), 0o644)
}

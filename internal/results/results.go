// Package results writes an experiment's measurements as CSV tables under
// <work dir>/results/, one file per table. A report.sql next to the
// experiment reads them with DuckDB ("course run N.M"), and "course check
// evidence" compares them with the stored copies under evidence/.
//
// A table whose name starts with "info" is for values that differ between
// runs or machines (timings, process ids, paths); the evidence check skips it.
package results

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// DirName is the directory under an experiment's work directory that holds its tables.
const DirName = "results"

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Set is the group of tables one experiment run writes.
type Set struct {
	dir      string
	tables   []*Table
	checks   *Table
	claims   int
	failures []string
}

// Table is one CSV file with a fixed header.
type Table struct {
	name    string
	columns int
	file    *os.File
	writer  *csv.Writer
}

// Open creates <workDir>/results and returns an empty Set.
func Open(workDir string) (*Set, error) {
	dir := filepath.Join(workDir, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Set{dir: dir}, nil
}

// MustOpen calls Open and exits 1 on error.
func MustOpen(workDir string) *Set {
	set, err := Open(workDir)
	if err != nil {
		fmt.Printf("FATAL results: %v\n", err)
		os.Exit(1)
	}
	return set
}

// Table creates <name>.csv with the given header. Table and column names must
// be lower-case SQL identifiers so a report can use them unquoted.
func (s *Set) Table(name string, columns ...string) *Table {
	for _, id := range append([]string{name}, columns...) {
		if !namePattern.MatchString(id) {
			panic(fmt.Sprintf("results: %q is not a lower-case identifier", id))
		}
	}
	file, err := os.Create(filepath.Join(s.dir, name+".csv"))
	if err != nil {
		panic(fmt.Sprintf("results: %v", err))
	}
	t := &Table{name: name, columns: len(columns), file: file, writer: csv.NewWriter(file)}
	if err := t.writer.Write(columns); err != nil {
		panic(fmt.Sprintf("results: %s: %v", name, err))
	}
	s.tables = append(s.tables, t)
	return t
}

// Row appends one row. A nil value is written as an empty field, which DuckDB
// reads as NULL; every other value is written with fmt.Sprint.
func (t *Table) Row(values ...any) {
	if len(values) != t.columns {
		panic(fmt.Sprintf("results: table %s has %d columns, row has %d values", t.name, t.columns, len(values)))
	}
	record := make([]string, len(values))
	for i, v := range values {
		if v != nil {
			record[i] = fmt.Sprint(v)
		}
	}
	if err := t.writer.Write(record); err != nil {
		panic(fmt.Sprintf("results: %s: %v", t.name, err))
	}
}

// Close flushes and closes every table.
func (s *Set) Close() error {
	var first error
	for _, t := range s.tables {
		t.writer.Flush()
		err := t.writer.Error()
		if closeErr := t.file.Close(); err == nil {
			err = closeErr
		}
		if err != nil && first == nil {
			first = fmt.Errorf("results: %s: %w", t.name, err)
		}
	}
	s.tables = nil
	return first
}

// Check records one claim in the "checks" table (seq, part, status, claim)
// with status "ok" or "FAILED". part names the section of the experiment the
// claim belongs to, such as "A"; the claim text must be the same on every run.
func (s *Set) Check(part string, ok bool, claim string) {
	if s.checks == nil {
		s.checks = s.Table("checks", "seq", "part", "status", "claim")
	}
	status := "ok"
	if !ok {
		status = "FAILED"
		s.failures = append(s.failures, claim)
	}
	s.claims++
	s.checks.Row(s.claims, part, status, claim)
}

// Failures returns the claims that did not hold so far.
func (s *Set) Failures() []string { return s.failures }

// Finish closes every table and prints the verdict line: "<name> OK", or
// "<name> FAILED" with the claims that did not hold, followed by exit status 1.
func (s *Set) Finish(name string) {
	if err := s.Close(); err != nil {
		fmt.Printf("FATAL %v\n", err)
		os.Exit(1)
	}
	if len(s.failures) > 0 {
		fmt.Printf("%s FAILED: %d claim(s) did not hold\n", name, len(s.failures))
		for _, claim := range s.failures {
			fmt.Printf("  - %s\n", claim)
		}
		os.Exit(1)
	}
	fmt.Printf("%s OK\n", name)
}

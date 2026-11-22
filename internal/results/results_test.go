package results

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetWritesCSV verifies the header, nil-as-empty, quoting, and the column count guard.
func TestSetWritesCSV(t *testing.T) {
	dir := t.TempDir()
	set, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	table := set.Table("inserts", "insert_no", "note", "ok")
	table.Row(1, "a, b", true)
	table.Row(2, nil, false)
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, DirName, "inserts.csv"))
	if err != nil {
		t.Fatal(err)
	}
	want := "insert_no,note,ok\n1,\"a, b\",true\n2,,false\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	set, _ = Open(dir)
	defer set.Close()
	for name, f := range map[string]func(){
		"bad name":    func() { set.Table("Bad-Name", "a") },
		"wrong count": func() { set.Table("t", "a", "b").Row(1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

// TestCheckWritesChecksTable verifies Check numbers the claims and collects the failed ones.
func TestCheckWritesChecksTable(t *testing.T) {
	dir := t.TempDir()
	set, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	set.Check("A", true, "first")
	set.Check("B", false, "second")
	if got := set.Failures(); len(got) != 1 || got[0] != "second" {
		t.Fatalf("failures %v", got)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, DirName, "checks.csv"))
	if err != nil {
		t.Fatal(err)
	}
	want := "seq,part,status,claim\n1,A,ok,first\n2,B,FAILED,second\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

package sqlitero

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func names(t *testing.T, folder string) []string {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func count(t *testing.T, path string) int {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("select count(*) from t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("insert into t values (99)"); err == nil {
		t.Fatal("a write went through")
	}
	return n
}

func TestOpenReadsAWALDatabaseAWriterHoldsAndWritesNothingBesideOne(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "state.db")
	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"pragma journal_mode=wal", "create table t (x)", "insert into t values (1)", "insert into t values (2)"} {
		if _, err := writer.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	held := names(t, folder)
	if got := count(t, path); got != 2 {
		t.Fatalf("read %d rows while the writer holds it", got)
	}
	if after := names(t, folder); !slices.Equal(after, held) {
		t.Fatalf("files beside the database changed from %v to %v", held, after)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	closed := names(t, folder)
	if got := count(t, path); got != 2 {
		t.Fatalf("read %d rows after the writer closed", got)
	}
	if after := names(t, folder); !slices.Equal(after, closed) {
		t.Fatalf("files beside the closed database changed from %v to %v", closed, after)
	}
}

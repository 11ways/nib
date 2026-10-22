package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// databaseFixture is state.db, a WAL database an open writer holds, with a table runs of 30 rows, a WITHOUT ROWID table pins and a view.
func databaseFixture(t *testing.T) (*Engine, *Context, string) {
	t.Helper()
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	writer, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	statements := []string{
		"pragma journal_mode=wal",
		"create table runs (id integer primary key, status text, log text, data blob)",
		"create table pins (name text primary key, rev text, pinned timestamp) without rowid",
		"insert into pins values ('zeta', 'z1', '2026-10-19 10:00:00'), ('alpha', 'a1', NULL)",
		"create view failed as select id, status from runs where status = 'fail'",
	}
	for i := 1; i <= 30; i++ {
		status := "ok"
		if i%10 == 0 {
			status = "fail"
		}
		statements = append(statements, fmt.Sprintf("insert into runs (status, log) values ('%s', 'run %d')", status, i))
	}
	statements = append(statements,
		"update runs set log = 'first line'||char(10)||'second | line' where id = 2",
		"update runs set log = '"+strings.Repeat("x", 500)+"' where id = 3",
		"update runs set data = x'000102' where id = 4",
	)
	for _, statement := range statements {
		if _, err := writer.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return New(), &Context{Ctx: context.Background(), Directory: directory, Session: "s"}, directory
}

func TestReadOfADatabaseOutlinesItsTablesAndViews(t *testing.T) {
	e, ctx, _ := databaseFixture(t)
	want := strings.Join([]string{
		"[state.db] SQLite database, read-only",
		"table runs, 30 rows: id INTEGER, status TEXT, log TEXT, data BLOB",
		"table pins, 2 rows, without rowid: name TEXT, rev TEXT, pinned timestamp",
		"view failed, 3 rows: id INTEGER, status TEXT",
		"[read state.db:TABLE for its rows, state.db:TABLE:-15 for the last 15, state.db:SELECT ... for a query]",
	}, "\n")
	if got := readText(t, e, ctx, "state.db"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestADbPrefixIsNamedAsTheFileItself(t *testing.T) {
	e, ctx, directory := databaseFixture(t)
	entry := "db:" + filepath.Join(directory, "state.db") + ":SELECT 1"
	_, err := e.Read(ctx, ReadArgs{Paths: []string{entry}})
	if want := entry + " does not exist; for a SQLite file, name the file itself, as in " + filepath.Join(directory, "state.db") + ":SELECT 1"; err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
}

func TestReadOfATableGivesItsRowsByPosition(t *testing.T) {
	e, ctx, _ := databaseFixture(t)
	last := readText(t, e, ctx, "state.db:runs:-3")
	want := strings.Join([]string{
		"[state.db] table runs, rows 28-30 of 30, in rowid order",
		"id | status | log | data",
		"28 | ok | run 28 | NULL",
		"29 | ok | run 29 | NULL",
		"30 | fail | run 30 | NULL",
	}, "\n")
	if last != want {
		t.Fatalf("got\n%s\nwant\n%s", last, want)
	}
	first := readText(t, e, ctx, "state.db:runs:2-4")
	for _, row := range []string{
		"[state.db] table runs, rows 2-4 of 30, in rowid order",
		`2 | ok | first line\nsecond \| line | NULL`,
		"3 | ok | " + strings.Repeat("x", 120) + "... (+380 chars) | NULL",
		"4 | ok | run 4 | <blob 3 bytes>",
	} {
		if !strings.Contains(first, row+"\n") && !strings.HasSuffix(first, row) {
			t.Fatalf("no row %q in\n%s", row, first)
		}
	}
	if got := readText(t, e, ctx, "state.db:runs:29+5"); !strings.HasPrefix(got, "[state.db] table runs, rows 29-30 of 30") {
		t.Fatalf("got\n%s", got)
	}
}

func TestReadOfAWholeTableAViewAndATableWithoutRowid(t *testing.T) {
	e, ctx, _ := databaseFixture(t)
	pins := readText(t, e, ctx, "state.db:pins")
	if want := "[state.db] table pins, rows 1-2 of 2, in primary key order\nname | rev | pinned\nalpha | a1 | NULL\nzeta | z1 | 2026-10-19 10:00:00"; pins != want {
		t.Fatalf("got\n%s\nwant\n%s", pins, want)
	}
	view := readText(t, e, ctx, "state.db:failed")
	if want := "[state.db] view failed, rows 1-3 of 3, in the view's order\nid | status\n10 | fail\n20 | fail\n30 | fail"; view != want {
		t.Fatalf("got\n%s\nwant\n%s", view, want)
	}
	runs := readText(t, e, ctx, "state.db:runs")
	if !strings.Contains(runs, "rows 1-30 of 30") || strings.Count(runs, "\n") != 31 {
		t.Fatalf("got\n%s", runs)
	}
}

func TestReadOfAnUnknownTableOrALineRangeOfADatabaseSaysHowToReadIt(t *testing.T) {
	e, ctx, _ := databaseFixture(t)
	if got := readText(t, e, ctx, "state.db:nope"); got != "[state.db has no table or view nope; it has runs, pins, failed]" {
		t.Fatalf("got %q", got)
	}
	if got := readText(t, e, ctx, "state.db:1-20"); got != "[state.db is a SQLite database, read by table: state.db:TABLE:1-20]" {
		t.Fatalf("got %q", got)
	}
}

func TestReadingADatabaseWritesNothingAndLeavesItUneditable(t *testing.T) {
	e, ctx, directory := databaseFixture(t)
	listing := func() []string {
		entries, _ := os.ReadDir(directory)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	before := listing()
	readText(t, e, ctx, "state.db", "state.db:runs:-2")
	if after := listing(); !slices.Equal(before, after) {
		t.Fatalf("files changed from %v to %v", before, after)
	}
	original, _ := os.ReadFile(filepath.Join(directory, "state.db"))
	if _, err := e.Patch(ctx, "[state.db]\nPUT >$:\n+x"); err == nil {
		t.Fatal("an edit of a database went through")
	}
	if now, _ := os.ReadFile(filepath.Join(directory, "state.db")); string(now) != string(original) {
		t.Fatal("the database changed")
	}
}

func TestReadOfSeveralTablesOrALargeOneNotesWhatItLeftOut(t *testing.T) {
	e, ctx, directory := databaseFixture(t)
	both := readText(t, e, ctx, "state.db:pins,failed")
	if want := "[state.db]\ntable pins, rows 1-2 of 2, in primary key order\nname | rev | pinned\nalpha | a1 | NULL\nzeta | z1 | 2026-10-19 10:00:00\nview failed, rows 1-3 of 3, in the view's order\nid | status\n10 | fail\n20 | fail\n30 | fail"; both != want {
		t.Fatalf("got\n%s\nwant\n%s", both, want)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "big.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("create table events (n integer, body text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("with recursive c(n) as (select 1 union all select n + 1 from c where n < 3000) insert into events select n, printf('%0100d', n) from c"); err != nil {
		t.Fatal(err)
	}
	big := readText(t, e, ctx, "big.db:events")
	if !strings.HasPrefix(big, "[big.db] table events, rows 1-500 of 3000, in rowid order\nn | body\n1 | ") {
		t.Fatalf("got\n%s", big[:200])
	}
	lines := strings.Split(big, "\n")
	shown := len(lines) - 3
	if want := fmt.Sprintf("[rows %d-3000 not shown; read big.db:events:%d+50 for more]", shown+1, shown+1); lines[len(lines)-1] != want {
		t.Fatalf("ends %q, want %q, %d chars", lines[len(lines)-1], want, len(big))
	}
}

func TestReadOfADatabaseQueryPrintsItsRowsLikeATableRead(t *testing.T) {
	e, ctx, _ := databaseFixture(t)
	for query, want := range map[string]string{
		"SELECT id, status FROM runs WHERE status = 'fail' ORDER BY id DESC LIMIT 2": "[state.db] query, 2 rows\nid | status\n30 | fail\n20 | fail",
		" with f as (select id, log from runs where id between 2 and 4) select * from f": "[state.db] query, 3 rows\nid | log\n" +
			`2 | first line\nsecond \| line` + "\n3 | " + strings.Repeat("x", 120) + "... (+380 chars)\n4 | run 4",
		// A colon inside a literal belongs to the query, and a timestamp column's text stays as stored.
		"select name, pinned from pins where pinned = '2026-10-19 10:00:00' or rev = 'a:1' order by name desc": "[state.db] query, 1 row\nname | pinned\nzeta | 2026-10-19 10:00:00",
		"select name, pinned from pins order by name desc":                                                     "[state.db] query, 2 rows\nname | pinned\nzeta | 2026-10-19 10:00:00\nalpha | NULL",
		"PRAGMA table_info(pins)":                        "[state.db] query, 3 rows\ncid | name | type | notnull | dflt_value | pk\n0 | name | TEXT | 1 | NULL | 1\n1 | rev | TEXT | 0 | NULL | 0\n2 | pinned | timestamp | 0 | NULL | 0",
		"values (1, 'a:b');":                             "[state.db] query, 1 row\ncolumn1 | column2\n1 | a:b",
		"select data from runs where id = 4 -- the blob": "[state.db] query, 1 row\ndata\n<blob 3 bytes>",
	} {
		if got := readText(t, e, ctx, "state.db:"+query); got != want {
			t.Errorf("%s: got\n%s\nwant\n%s", query, got, want)
		}
	}
}

func TestADatabaseQueryNeverWritesAndRunsOneStatement(t *testing.T) {
	e, ctx, directory := databaseFixture(t)
	original, _ := os.ReadFile(filepath.Join(directory, "state.db"))
	for _, query := range []string{
		"WITH x AS (SELECT 1) INSERT INTO runs (status) SELECT 'x' FROM x",
		"PRAGMA user_version=5",
	} {
		if got := readText(t, e, ctx, "state.db:"+query); !strings.HasPrefix(got, "[state.db: ") || !strings.Contains(got, "readonly") || strings.Contains(got, "\n") {
			t.Errorf("%s: got %q", query, got)
		}
	}
	if got := readText(t, e, ctx, "state.db:INSERT INTO runs (status) VALUES ('x')"); !strings.Contains(got, "a query starts with SELECT, WITH, VALUES, PRAGMA or EXPLAIN") {
		t.Errorf("an insert got %q", got)
	}
	if got := readText(t, e, ctx, "state.db:SELECT 1; DELETE FROM runs"); got != "[state.db: read runs one statement; drop what follows its first ;]" {
		t.Errorf("two statements got %q", got)
	}
	if got := readText(t, e, ctx, "state.db:SELECT ';' AS semi; -- done"); got != "[state.db] query, 1 row\nsemi\n;" {
		t.Errorf("a quoted semicolon got %q", got)
	}
	if got := readText(t, e, ctx, "state.db:SELECT FROM"); !strings.HasPrefix(got, "[state.db: ") || !strings.Contains(got, "syntax error") {
		t.Errorf("a syntax error got %q", got)
	}
	if now, _ := os.ReadFile(filepath.Join(directory, "state.db")); string(now) != string(original) {
		t.Fatal("the database changed")
	}
	check, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var runs, version int
	if err := check.QueryRow("select count(*), (select user_version from pragma_user_version) from runs").Scan(&runs, &version); err != nil || runs != 30 || version != 0 {
		t.Fatalf("runs %d, user_version %d, %v", runs, version, err)
	}
}

func TestALargeOrEndlessDatabaseQueryIsCappedOrStopped(t *testing.T) {
	e, ctx, directory := databaseFixture(t)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "big.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("create table events (n integer, body text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("with recursive c(n) as (select 1 union all select n + 1 from c where n < 3000) insert into events select n, printf('%0100d', n) from c"); err != nil {
		t.Fatal(err)
	}
	capped := readText(t, e, ctx, "big.db:select n from events")
	if lines := strings.Split(capped, "\n"); lines[0] != "[big.db] query, first 500 rows; more follow, narrow it with LIMIT/OFFSET or WHERE" || len(lines) != 502 || lines[501] != "500" {
		t.Fatalf("got %d lines starting %q", len(lines), lines[0])
	}
	budget := readText(t, e, ctx, "big.db:select n, body from events where n <= 450")
	lines := strings.Split(budget, "\n")
	shown := len(lines) - 3
	if want := fmt.Sprintf("[rows %d-450 not shown; narrow it with LIMIT/OFFSET or WHERE]", shown+1); lines[0] != "[big.db] query, 450 rows" || lines[len(lines)-1] != want {
		t.Fatalf("starts %q, ends %q, want %q", lines[0], lines[len(lines)-1], want)
	}
	queryTimeout = 300 * time.Millisecond
	defer func() { queryTimeout = 5 * time.Second }()
	start := time.Now()
	endless := readText(t, e, ctx, "big.db:WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c")
	if endless != "[big.db: the query ran past 300ms and was stopped]" || time.Since(start) > 5*time.Second {
		t.Fatalf("got %q after %s", endless, time.Since(start))
	}
}

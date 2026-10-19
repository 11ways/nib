package opencode

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// schema is the part of OpenCode 2.0.30's database a lookup reads, as its migrations create it.
const schema = `
create table session_v2 (id text primary key, project_id text not null, parent_id text, fork_session_id text, fork_boundary text,
	time_created integer not null);
create table session_message (id text primary key, session_id text not null, type text not null, seq integer not null,
	time_created integer not null, time_updated integer not null, data text not null);
`

// database writes a fixture database: a parent session with settled messages, a fork through one of them copied the way OpenCode's
// projector copies (same seq and times, data as is, the running message left out), a fork before a message, a subagent's child session.
func database(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{schema,
		`insert into session_v2 values ('ses_parent', 'p', null, null, null, 1000)`,
		`insert into session_v2 values ('ses_fork', 'p', null, 'ses_parent', '{"type":"through","messageID":"msg_a2"}', 9000)`,
		`insert into session_v2 values ('ses_before', 'p', null, 'ses_parent', '{"type":"before","messageID":"msg_u2"}', 9000)`,
		`insert into session_v2 values ('ses_lost', 'p', null, 'ses_parent', '{"type":"through","messageID":"msg_gone"}', 9000)`,
		`insert into session_v2 values ('ses_child', 'p', 'ses_parent', null, null, 2000)`,
	}
	parent := [][]any{
		{"msg_u1", "user", 4, 1100, 1101, `{"time":{"created":1100},"text":"read a.txt"}`},
		{"msg_a1", "assistant", 5, 1200, 1999, `{"time":{"created":1200,"completed":1998},"content":[]}`},
		{"msg_u2", "user", 9, 3000, 3001, `{"time":{"created":3000},"text":"now b"}`},
		{"msg_a2", "assistant", 12, 3100, 4500, `{"time":{"created":3100,"completed":4400},"content":[]}`},
		{"msg_a3", "assistant", 20, 5000, 5000, `{"time":{"created":5000},"content":[]}`},
	}
	for _, row := range parent {
		statements = append(statements, sqlRow("ses_parent", row))
		// The forks copy the settled rows up to their boundary; their own new rows come after.
		if row[2].(int) <= 12 {
			statements = append(statements, sqlRow("ses_fork", append([]any{"evt_" + row[0].(string)}, row[1:]...)))
		}
		if row[2].(int) < 9 {
			statements = append(statements, sqlRow("ses_before", append([]any{"evt2_" + row[0].(string)}, row[1:]...)))
		}
	}
	statements = append(statements, sqlRow("ses_fork", []any{"msg_new", "user", 13, 9100, 9100, `{"time":{"created":9100}}`}))
	statements = append(statements, sqlRow("ses_before", []any{"msg_new2", "user", 6, 9100, 9900, `{"time":{"created":9100}}`}))
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func sqlRow(session string, row []any) string {
	db := func(s string) string { return "'" + s + "'" }
	return "insert into session_message values (" + db(row[0].(string)) + ", " + db(session) + ", " + db(row[1].(string)) + ", " +
		strconv.Itoa(row[2].(int)) + ", " + strconv.Itoa(row[3].(int)) + ", " + strconv.Itoa(row[4].(int)) + ", " + db(row[5].(string)) + ")"
}

func TestOfReadsTheParentAndTheMoment(t *testing.T) {
	data := t.TempDir()
	folder := filepath.Join(data, "opencode")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "opencode.db")
	database(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	databases := Databases(func(name string) string { return map[string]string{"XDG_DATA_HOME": data}[name] })
	for session, want := range map[string]Origin{
		// Through msg_a2: its completion is the latest moment in the copy; the fork's own later row does not count.
		"ses_fork": {Parent: "ses_parent", Fork: true, At: time.UnixMilli(4401)},
		// Before msg_u2: only the first exchange is copied, and the fork's own row with a lower seq than the boundary does not count.
		"ses_before": {Parent: "ses_parent", Fork: true, At: time.UnixMilli(1999)},
		// A boundary nib cannot find leaves the moment unknown, not the parent.
		"ses_lost": {Parent: "ses_parent", Fork: true},
		// A subagent's child session was spawned when it was created.
		"ses_child": {Parent: "ses_parent", At: time.UnixMilli(2001)},
	} {
		got, ok := Of(databases, session)
		if !ok || got.Parent != want.Parent || got.Fork != want.Fork || !got.At.Equal(want.At) {
			t.Errorf("%s: got %+v %v, want %+v", session, got, ok, want)
		}
	}
	for _, session := range []string{"ses_parent", "ses_unknown"} {
		if got, ok := Of(databases, session); ok {
			t.Errorf("%s has no parent, got %+v", session, got)
		}
	}
	if after, _ := os.ReadFile(path); !slices.Equal(before, after) {
		t.Fatal("a lookup must never write to OpenCode's database")
	}
}

func TestCompactedIsTheLatestCompactionOfASessionOpenCode2Holds(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "opencode.db")
	database(t, path)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{
		{"msg_c1", "compaction", 21, 5500, 5500, `{"type":"compaction","reason":"auto","summary":"s","recent":""}`},
		{"msg_c2", "compaction", 22, 6000, 6000, `{"type":"compaction","reason":"manual","summary":"s","recent":""}`},
	} {
		if _, err := db.Exec(sqlRow("ses_parent", row)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	// OpenCode's first database, without OpenCode 2's tables, says nothing; the next one holds the sessions.
	v1 := filepath.Join(folder, "v1.db")
	if old, err := sql.Open("sqlite", "file:"+v1); err != nil {
		t.Fatal(err)
	} else if _, err := old.Exec(`create table session (id text primary key)`); err != nil {
		t.Fatal(err)
	} else {
		old.Close()
	}
	databases := []string{v1, path}
	if got, ok := Compacted(databases, "ses_parent"); !ok || !got.Equal(time.UnixMilli(6001)) {
		t.Fatalf("parent: %v %v", got, ok)
	}
	if got, ok := Compacted(databases, "ses_child"); !ok || !got.IsZero() {
		t.Fatalf("a session never compacted: %v %v", got, ok)
	}
	for _, missing := range [][]string{{v1}, databases} {
		if got, ok := Compacted(missing, "ses_unknown"); ok {
			t.Fatalf("%v: a session no database holds: %v", missing, got)
		}
	}
	if got, ok := Compacted([]string{v1}, "ses_parent"); ok {
		t.Fatalf("a database without OpenCode 2's tables cannot tell: %v", got)
	}
}

func TestDatabasesAreWhereOpenCodeKeepsThem(t *testing.T) {
	data := t.TempDir()
	folder := filepath.Join(data, "opencode")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"opencode-beta-2.db", "opencode-alpha.db"} {
		if err := os.WriteFile(filepath.Join(folder, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if got := Databases(env(map[string]string{"XDG_DATA_HOME": data})); !slices.Equal(got, []string{
		filepath.Join(folder, "opencode.db"), filepath.Join(folder, "opencode-alpha.db"), filepath.Join(folder, "opencode-beta-2.db"),
	}) {
		t.Fatalf("got %v", got)
	}
	if got := Databases(env(map[string]string{"HOME": "/home/u"})); !slices.Equal(got, []string{filepath.FromSlash("/home/u/.local/share/opencode/opencode.db")}) {
		t.Fatalf("got %v", got)
	}
	if got := Databases(env(map[string]string{"XDG_DATA_HOME": data, "OPENCODE_DB": "custom.db"})); !slices.Equal(got, []string{filepath.Join(folder, "custom.db")}) {
		t.Fatalf("got %v", got)
	}
	if got := Databases(env(map[string]string{"XDG_DATA_HOME": data, "OPENCODE_DB": ":memory:"})); got != nil {
		t.Fatalf("an in-memory database cannot be read, got %v", got)
	}
	// A file that is not OpenCode 2's database is passed over.
	if _, ok := Of([]string{filepath.Join(folder, "opencode-alpha.db"), filepath.Join(folder, "missing.db")}, "ses_x"); ok {
		t.Fatal("got an origin from no database")
	}
}

func TestCredentialIsAProvidersLoginReadOnly(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		// As OpenCode 2.0.30's migrations create it.
		`create table credential (id text primary key, integration_id text not null, label text, value text not null, connector_id text,
			method_id text, active integer, time_created integer not null, time_updated integer not null)`,
		`insert into credential values ('c1', 'deepseek', 'API key', '{"type":"key","key":"sk-old"}', null, null, 0, 1, 9)`,
		`insert into credential values ('c2', 'deepseek', 'API key', '{"type":"key","key":"sk-live"}', null, null, 1, 1, 2)`,
		`insert into credential values ('c3', 'openai', 'OAuth', '{"type":"oauth","refresh":"r","access":"a","expires":1}', null, null, 1, 1, 1)`,
		`insert into credential values ('c4', 'broken', 'API key', 'not json', null, null, 1, 1, 1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db.Close()
	before, _ := os.ReadFile(path)
	// A database without the table, like OpenCode's first one, is passed over.
	v1 := filepath.Join(folder, "v1.db")
	if old, err := sql.Open("sqlite", "file:"+v1); err != nil {
		t.Fatal(err)
	} else if _, err := old.Exec(`create table session (id text primary key)`); err != nil {
		t.Fatal(err)
	} else {
		old.Close()
	}
	databases := []string{filepath.Join(folder, "missing.db"), v1, path}
	if got, ok := Credential(databases, "deepseek"); !ok || got != (Login{Kind: "key", Key: "sk-live"}) {
		t.Fatalf("the active key wins: %+v %v", got, ok)
	}
	if got, ok := Credential(databases, "openai"); !ok || got != (Login{Kind: "oauth"}) {
		t.Fatalf("oauth: %+v %v", got, ok)
	}
	for _, provider := range []string{"broken", "mistral"} {
		if got, ok := Credential(databases, provider); ok {
			t.Fatalf("%s: %+v", provider, got)
		}
	}
	if after, _ := os.ReadFile(path); !slices.Equal(before, after) {
		t.Fatal("a lookup must never write to OpenCode's database")
	}
}

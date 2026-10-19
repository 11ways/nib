// Package opencode reads what nib needs from OpenCode 2's own database: which session a session was forked or spawned from, and when, when
// it was last compacted, and a provider's login. OpenCode tells an MCP server only the session making a call, and starts its servers with
// its own environment, so nib finds the database where OpenCode does and opens it read-only.
package opencode

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/11ways/nib/internal/platform"
)

// Origin is where a session comes from: its parent session, whether it is a fork of it (which copies its history) or a subagent's child
// session, and At, the first moment the parent's work may be missing from what the session was given, zero when that cannot be told.
type Origin struct {
	Parent string
	Fork   bool
	At     time.Time
}

// Databases are the database files OpenCode run with this environment may use, the likeliest first: OPENCODE_DB when set, relative to
// OpenCode's data folder ($XDG_DATA_HOME/opencode or ~/.local/share/opencode, on every OS), else opencode.db there and then the
// per-channel ones.
func Databases(getenv func(string) string) []string {
	data := platform.DataHome(getenv)
	if data == "" {
		return nil
	}
	folder := filepath.Join(data, "opencode")
	if name := getenv("OPENCODE_DB"); name != "" {
		if name == ":memory:" {
			return nil
		}
		if !filepath.IsAbs(name) {
			name = filepath.Join(folder, name)
		}
		return []string{name}
	}
	channels, _ := filepath.Glob(filepath.Join(folder, "opencode-*.db"))
	slices.Sort(channels)
	return append([]string{filepath.Join(folder, "opencode.db")}, channels...)
}

// Of is where session comes from, by the first of databases that holds it; ok is false for a session that has no parent, or that none of
// them holds or can be read.
func Of(databases []string, session string) (origin Origin, ok bool) {
	for _, path := range databases {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if origin, found, err := lookup(path, session); err == nil && found {
			return origin, origin.Parent != ""
		}
	}
	return Origin{}, false
}

// lookup reads session's origin from one database; found is false when it does not hold the session. A forked session holds copies of the
// parent's settled messages up to its boundary, which keep their times: the moment is one millisecond past the latest completion or
// creation among them, as OpenCode stamps whole milliseconds and nib stamps seen lines before the message showing them completes, and the
// parent's later calls run in messages created after those. A child session's moment is one millisecond past its creation.
func lookup(path, session string) (origin Origin, found bool, err error) {
	db, err := open(path)
	if err != nil {
		return Origin{}, false, err
	}
	defer db.Close()
	var parent, child, boundary sql.NullString
	var created int64
	err = db.QueryRow("select fork_session_id, parent_id, fork_boundary, time_created from session_v2 where id = ?", session).Scan(&parent, &child, &boundary, &created)
	if err == sql.ErrNoRows {
		return Origin{}, false, nil
	}
	if err != nil {
		return Origin{}, false, err
	}
	if !parent.Valid || parent.String == "" {
		if child.Valid && child.String != "" && created > 0 {
			return Origin{Parent: child.String, At: time.UnixMilli(created + 1)}, true, nil
		}
		return Origin{}, true, nil
	}
	fork := Origin{Parent: parent.String, Fork: true}
	var edge struct {
		Type      string `json:"type"`
		MessageID string `json:"messageID"`
	}
	if json.Unmarshal([]byte(boundary.String), &edge) != nil || (edge.Type != "before" && edge.Type != "through") {
		return fork, true, nil
	}
	// The last of the parent's messages the fork copied: through its boundary message, or before it.
	var copied sql.NullInt64
	err = db.QueryRow(`select max(p.seq) from session_message p, session_message b
		where b.session_id = ?1 and b.id = ?2 and p.session_id = ?1 and (p.seq < b.seq or (?3 and p.seq = b.seq))`,
		parent.String, edge.MessageID, edge.Type == "through").Scan(&copied)
	if err != nil || !copied.Valid {
		return fork, true, nil
	}
	var latest sql.NullInt64
	err = db.QueryRow(`select max(coalesce(json_extract(data, '$.time.completed'), time_created)) from session_message
		where session_id = ? and seq <= ?`, session, copied.Int64).Scan(&latest)
	if err == nil && latest.Valid && latest.Int64 > 0 {
		fork.At = time.UnixMilli(latest.Int64 + 1)
	}
	return fork, true, nil
}

// Compacted is when OpenCode last compacted session, by the first of databases that holds it, zero for never; ok is false when none of them
// holds it or can be read, so nib cannot tell. A fork holds copies of its parent's messages, compactions included, so a compaction before its
// boundary counts for it too.
func Compacted(databases []string, session string) (at time.Time, ok bool) {
	for _, path := range databases {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if at, found, err := compaction(path, session); err == nil && found {
			return at, true
		}
	}
	return time.Time{}, false
}

// compaction reads session's latest compaction from one database; found is false when it does not hold the session. OpenCode appends a
// compaction message when the compaction ends, stamped then, in whole milliseconds; nib makes no call for the session while it runs.
func compaction(path, session string) (at time.Time, found bool, err error) {
	db, err := open(path)
	if err != nil {
		return time.Time{}, false, err
	}
	defer db.Close()
	var held sql.NullInt64
	var latest sql.NullInt64
	err = db.QueryRow(`select (select 1 from session_v2 where id = ?1),
		(select max(time_created) from session_message where session_id = ?1 and type = 'compaction')`, session).Scan(&held, &latest)
	if err != nil || !held.Valid {
		return time.Time{}, false, err
	}
	if latest.Valid && latest.Int64 > 0 {
		// One millisecond past it drops the lines shown in that millisecond too, which can only drop more.
		at = time.UnixMilli(latest.Int64 + 1)
	}
	return at, true, nil
}

// Login is a provider's credential as OpenCode 2 keeps it: Kind is "key" for an API key, with Key set, or "oauth" for a login it refreshes.
type Login struct {
	Kind string
	Key  string
}

// Credential is provider's login, by the first of databases that holds one; ok is false when none does or can be read. An active
// credential wins over an inactive one, and the latest update over older ones.
func Credential(databases []string, provider string) (login Login, ok bool) {
	for _, path := range databases {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if login, found := credential(path, provider); found {
			return login, true
		}
	}
	return Login{}, false
}

// credential reads provider's login from one database; anything unexpected in its value is no login.
func credential(path, provider string) (Login, bool) {
	db, err := open(path)
	if err != nil {
		return Login{}, false
	}
	defer db.Close()
	var value string
	err = db.QueryRow(`select value from credential where integration_id = ? order by coalesce(active, 0) desc, time_updated desc limit 1`,
		provider).Scan(&value)
	if err != nil {
		return Login{}, false
	}
	var stored struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal([]byte(value), &stored) != nil || stored.Type == "" {
		return Login{}, false
	}
	return Login{Kind: stored.Type, Key: stored.Key}, true
}

// open opens a database read-only, so nib never writes to OpenCode's data. A file URI holds the path with slashes and a leading one, so a
// Windows path C:\x reads file:///C:/x.
func open(path string) (*sql.DB, error) {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	location := url.URL{Scheme: "file", Path: slashed, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"}
	return sql.Open("sqlite", location.String())
}

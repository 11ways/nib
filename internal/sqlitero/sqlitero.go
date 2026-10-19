// Package sqlitero opens SQLite databases strictly read-only, the one way nib opens a database it does not own.
package sqlitero

import (
	"bytes"
	"database/sql"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Magic starts every SQLite 3 database file.
var Magic = []byte("SQLite format 3\x00")

// Open opens path read-only with writes refused by the connection too. Even a read-only connection to a WAL database creates its -wal and
// -shm files when no other connection has them, so a WAL database nobody holds open is opened immutable, which creates nothing; a writer
// that starts during that read can make it fail, never change the file. A file URI holds the path with slashes and a leading one, so a
// Windows path C:\x reads file:///C:/x.
func Open(path string) (*sql.DB, error) {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	query := "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
	if wal(path) {
		if _, err := os.Lstat(path + "-shm"); err != nil {
			query = "immutable=1&" + query
		}
	}
	location := url.URL{Scheme: "file", Path: slashed, RawQuery: query}
	return sql.Open("sqlite", location.String())
}

// wal reports whether the database's header says it is in WAL mode: its read and write versions are 2.
func wal(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return bytes.HasPrefix(header, Magic) && (header[18] == 2 || header[19] == 2)
}

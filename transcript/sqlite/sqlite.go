// Package sqlite holds the session codecs for agents that keep their
// conversations in a SQLite database: goose and hermes. It is a module of
// its own so that agentprotocol's core stays free of a database driver;
// importing it registers these codecs through transcript.Register, so
// transcript/all can find them.
//
// The driver is modernc.org/sqlite, a pure-Go build, so this module needs no
// cgo. Databases are opened read-only and immutable, which lets a store be
// read while the agent that owns it is running.
package sqlite

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// openRO opens a database read-only and immutable, so a running agent's lock
// does not block a read and the read never writes a journal.
func openRO(path string) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?mode=ro&immutable=1&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	return db, nil
}

// openRW opens a database for reading and writing, creating it if missing.
func openRW(path string) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	return db, nil
}

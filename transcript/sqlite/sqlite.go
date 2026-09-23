// Package sqlite holds the session codecs for agents that keep their
// conversations in a SQLite database: goose and hermes. It is a module of
// its own so that agentprotocol's core stays free of a database driver;
// importing it registers these codecs through transcript.Register, so
// transcript/all can find them.
//
// The driver is modernc.org/sqlite, a pure-Go build, so this module needs no
// cgo. Reads never change an agent's data, and see rows still in its
// write-ahead log; see openRO.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// openRO opens a database for reading, and sees every committed row,
// including rows still in the write-ahead log. It never changes the data. On
// a database in WAL mode SQLite may create the -wal and -shm files beside it,
// which is how any reader joins a WAL database, the agent's own included. Agents write
// through the log and a live or just-exited agent may not have folded it into
// the main file, so the main file alone can hold no tables at all.
//
// A plain read-only open reads the log, but SQLite must be able to create or
// map the -shm index beside the database, which fails when the directory
// belongs to another user. In that case openRO snapshots the database and its
// log into a private temporary directory and reads the copy, so the read
// still sees the log instead of silently dropping it the way an immutable
// open would. The returned cleanup removes the snapshot.
func openRO(path string) (*sql.DB, func(), error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if err := db.Ping(); err == nil {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err == nil {
			return db, func() {}, nil
		}
	}
	db.Close()
	return openSnapshot(path)
}

// openSnapshot copies a database with its -wal into a temporary directory
// and opens the copy read-write, so SQLite can replay the log there.
func openSnapshot(path string) (*sql.DB, func(), error) {
	dir, err := os.MkdirTemp("", "transcript-sqlite-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	dst := filepath.Join(dir, filepath.Base(path))
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(path+suffix, dst+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			cleanup()
			return nil, nil, fmt.Errorf("sqlite: snapshot %s: %w", path+suffix, err)
		}
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(dst)+"?_pragma=busy_timeout(2000)")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("sqlite: open snapshot of %s: %w", path, err)
	}
	return db, func() { db.Close(); cleanup() }, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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

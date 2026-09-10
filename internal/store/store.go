// Package store owns the SQLite database. The issues table doubles as the
// pipeline queue: each worker leases rows in one state, works, and advances
// them in the same transaction that commits its result. Nothing is handed
// between workers in memory, so a crash loses at most one lease interval.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *sql.DB
}

// Open creates the database if it does not exist, applies the schema, and
// returns a ready store. Passing ":memory:" gives an in-process database for
// tests.
func Open(path string) (*Store, error) {
	dsn, err := dsn(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite serializes writes anyway, and every query here is small and
	// indexed. One connection removes lock contention entirely and costs
	// nothing at a few queries per second.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func dsn(path string) (string, error) {
	pragmas := "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		return "file::memory:?" + pragmas, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// WAL survives in the file itself, but setting it per connection keeps a
	// freshly created database correct on its very first write.
	return "file:" + url.PathEscape(path) + "?_pragma=journal_mode(WAL)&" + pragmas, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the few callers that need a raw query, such as the
// status subcommand's reports.
func (s *Store) DB() *sql.DB { return s.db }

func timeOrZero(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}

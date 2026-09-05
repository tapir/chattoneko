package db

import (
	"database/sql"
	"strings"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// Open opens the SQLite database at path (or ":memory:") with the pragmas the
// app relies on, applied to every connection the pool opens:
//
//   - foreign_keys=ON (without it, ON DELETE CASCADE clauses silently do nothing)
//   - journal_mode=WAL + synchronous=NORMAL (durability/perf tradeoff for the
//     streaming-flush write pattern)
//   - busy_timeout=5000 (writers retry instead of failing instantly under
//     contention)
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, ":memory:") {
		// Every new connection to ":memory:" would be a separate database, so
		// pin the pool to one connection.
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

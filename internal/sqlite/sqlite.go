// Package sqlite configures the SQLite connections shared by the metadata and
// log stores. Schema ownership remains with each store.
package sqlite

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Options tunes both the writer and the reader pool of one database file.
type Options struct {
	// Synchronous is "full" (the default) or "normal". In WAL mode "normal"
	// stays consistent after a process crash but can lose the most recent
	// commits on power loss, in exchange for one fewer fsync per commit.
	Synchronous string
}

// readerConns bounds the read-only pool. Readers never block the writer in
// WAL mode, so API reads cannot delay run transitions.
const readerConns = 4

// journalSizeLimit truncates the WAL back to this size after a checkpoint,
// so one large transaction does not leave a permanently large file.
const journalSizeLimit = 64 << 20

// Open keeps a single warm writer connection, with durability and integrity
// settings applied by the driver every time database/sql creates a
// replacement. Transactions start with BEGIN IMMEDIATE so a read-then-write
// transaction takes the write lock up front instead of failing with
// SQLITE_BUSY_SNAPSHOT when a reader pool is also using the file.
func Open(path string, opt ...Options) (*sql.DB, error) {
	o := options(opt)
	query := url.Values{
		"_pragma": {"synchronous(" + o.Synchronous + ")", "foreign_keys(ON)", "busy_timeout(5000)", fmt.Sprintf("journal_size_limit(%d)", journalSizeLimit)},
		"_txlock": {"immediate"},
	}
	db, err := open(path, query)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// OpenReader opens a small read-only pool for queries that must not queue
// behind writes. The file must already exist and use WAL mode.
func OpenReader(path string) (*sql.DB, error) {
	query := url.Values{
		"mode":    {"ro"},
		"_pragma": {"query_only(1)", "foreign_keys(ON)", "busy_timeout(5000)"},
	}
	db, err := open(path, query)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(readerConns)
	db.SetMaxIdleConns(readerConns)
	return db, nil
}

func options(opt []Options) Options {
	var o Options
	if len(opt) > 0 {
		o = opt[0]
	}
	switch o.Synchronous {
	case "normal", "NORMAL":
		o.Synchronous = "NORMAL"
	default:
		o.Synchronous = "FULL"
	}
	return o
}

func open(path string, query url.Values) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	return db, nil
}

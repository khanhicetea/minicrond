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

// Open keeps a single warm connection, with durability and integrity settings
// applied by the driver every time database/sql creates a replacement.
func Open(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	query := url.Values{"_pragma": {"synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(5000)"}}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

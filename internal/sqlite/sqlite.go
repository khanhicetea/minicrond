// Package sqlite configures the SQLite connections shared by the metadata and
// log stores. Schema ownership remains with each store.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	sqlitedrv "modernc.org/sqlite"
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

// OpenImmutable opens a database on storage this process cannot write (a
// read-only volume or directory) for reading only. SQLite cannot open a WAL
// database there without its -shm file, so the file is opened immutable: no
// locking and no change detection. Only use it while the caller holds the
// data directory lock, so no other minicrond writes the file. Every write
// fails with SQLITE_READONLY.
func OpenImmutable(path string) (*sql.DB, error) {
	query := url.Values{
		"mode":      {"ro"},
		"immutable": {"1"},
		"_pragma":   {"foreign_keys(ON)"},
	}
	db, err := open(path, query)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(readerConns)
	db.SetMaxIdleConns(readerConns)
	return db, nil
}

// SyncFull reports whether opt selects synchronous=FULL (the default).
func SyncFull(opt ...Options) bool { return options(opt).Synchronous == "FULL" }

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

// IsConstraint reports whether err is an SQLite constraint violation
// (SQLITE_CONSTRAINT and its extended codes), as opposed to the database being
// unavailable (full, read-only, busy, I/O error, closed, or a canceled wait).
func IsConstraint(err error) bool {
	var e *sqlitedrv.Error
	if errors.As(err, &e) {
		return e.Code()&0xff == 19 // SQLITE_CONSTRAINT
	}
	return false
}

// ErrUnsupportedSchema reports a database file this build cannot use: one
// written by a pre-0.6 development build, by another program, or by a newer
// minicrond.
var ErrUnsupportedSchema = errors.New("unsupported database schema")

// EnsureSchema creates the schema in an empty database, in one transaction
// that also records appID and version, or checks that an existing database
// carries both. There are no upgrade steps. name labels errors.
func EnsureSchema(ctx context.Context, db *sql.DB, name string, appID, version int, schema string) error {
	gotApp, gotVersion, err := SchemaID(ctx, db)
	if err != nil {
		return err
	}
	if gotApp == 0 && gotVersion == 0 {
		var objects int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&objects); err != nil {
			return err
		}
		if objects == 0 {
			ddl := fmt.Sprintf("BEGIN;\n%s\nPRAGMA application_id=%d;\nPRAGMA user_version=%d;\nCOMMIT;", schema, appID, version)
			if _, err := db.ExecContext(ctx, ddl); err != nil {
				return fmt.Errorf("create %s schema: %w", name, err)
			}
			return nil
		}
	}
	return CheckSchema(name, appID, version, gotApp, gotVersion)
}

// SchemaID reads the application_id and user_version PRAGMAs.
func SchemaID(ctx context.Context, db *sql.DB) (appID, version int, err error) {
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, 0, err
	}
	return appID, version, nil
}

// CheckSchema compares a database's identity with the one this build needs.
func CheckSchema(name string, wantApp, wantVersion, gotApp, gotVersion int) error {
	switch {
	case gotApp != wantApp:
		return fmt.Errorf("%w: the %s database was not created by minicrond 0.6 or later (application_id %#x, schema %d); pre-0.6 development databases cannot be upgraded, remove the data directory", ErrUnsupportedSchema, name, gotApp, gotVersion)
	case gotVersion > wantVersion:
		return fmt.Errorf("%w: %s database schema %d is newer than supported schema %d", ErrUnsupportedSchema, name, gotVersion, wantVersion)
	case gotVersion != wantVersion:
		return fmt.Errorf("%w: %s database schema %d is not supported (want %d)", ErrUnsupportedSchema, name, gotVersion, wantVersion)
	}
	return nil
}

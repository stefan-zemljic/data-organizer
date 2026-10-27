// Package store keeps everything in one SQLite database: backups and their
// items (backups.go, items.go), the duplicate analysis (analyze.go), browsing
// and exclusions (entries.go, exclusions.go), restores (restores.go) and the
// index of the file contents (contents.go), whose bytes are in pack files
// next to the database. The schema is in migrations/, applied by Open.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite database and the content store's pack files.
type Store struct {
	db      *sql.DB
	packDir string // pack files of the content store
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	// WAL for concurrent reads while a backup writes; per connection a 256 MB
	// page cache and temp tables in memory (memory mapping made inserts slower).
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=cache_size(-262144)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	migrationsFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		db.Close()
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrationsFS)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := provider.Up(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db, packDir: PackDir(path)}, nil
}

// PackDir is the folder of the content store's pack files for the database at
// dbPath: next to it, named after it (data_organizer.db -> data_organizer.packs).
func PackDir(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".packs"
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) backupIDs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

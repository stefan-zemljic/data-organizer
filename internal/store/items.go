package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stefan-zemljic/data-organizer/internal/content"
	"github.com/stefan-zemljic/data-organizer/internal/scan"
)

const (
	itemBatchSize = 10_000
	// Bounds the name cache; past this it is cleared and refilled on demand.
	maxCachedNames = 2_000_000
)

// ItemWriter inserts the items of one backup in batched transactions.
// It is not safe for concurrent use; one goroutine owns it.
type ItemWriter struct {
	db       *sql.DB
	backupID int64
	tx       *sql.Tx
	insItem  *sql.Stmt
	insDir   *sql.Stmt
	getName  *sql.Stmt
	insName  *sql.Stmt
	pending  int
	nameIDs  map[string]int64
	contents *content.Writer // nil when no contents are stored
}

// NewItemWriter returns a writer for a backup's items. contents (may be nil) is
// the backup's content writer: every batch also saves its new index rows.
func (s *Store) NewItemWriter(backupID int64, contents *content.Writer) *ItemWriter {
	return &ItemWriter{db: s.db, backupID: backupID, nameIDs: make(map[string]int64), contents: contents}
}

func (w *ItemWriter) begin(ctx context.Context) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	prepare := func(query string) *sql.Stmt {
		if err != nil {
			return nil
		}
		var st *sql.Stmt
		st, err = tx.PrepareContext(ctx, query)
		return st
	}
	w.insItem = prepare(`INSERT INTO items (id, backup_id, parent_id, name_id, kind, size, mtime, hash, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	w.insDir = prepare(`INSERT INTO backup_dirs (id, backup_id, parent_id, name) VALUES (?, ?, ?, ?)`)
	w.getName = prepare(`SELECT id FROM names WHERE name = ?`)
	w.insName = prepare(`INSERT INTO names (name) VALUES (?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	w.tx = tx
	return nil
}

// Write queues an item; it is committed with its batch.
func (w *ItemWriter) Write(ctx context.Context, it scan.Item) error {
	if w.tx == nil {
		if err := w.begin(ctx); err != nil {
			return err
		}
	}
	if it.Listed {
		return w.writeDir(ctx, it)
	}
	nameID, err := w.nameID(ctx, it.Name)
	if err != nil {
		return err
	}

	var parent, size, hash, errText any
	if it.ParentID != 0 {
		parent = it.ParentID
	}
	if it.Kind == scan.KindFile && it.Err == "" {
		size = it.Size
	}
	if it.Err == "" {
		hash = it.Hash[:]
	} else {
		errText = it.Err
	}
	var mtime any
	if it.ModTime != 0 {
		mtime = it.ModTime
	}
	if _, err := w.insItem.ExecContext(ctx, it.ID, w.backupID, parent, nameID, int(it.Kind), size, mtime, hash, errText); err != nil {
		return err
	}
	w.pending++
	if w.pending >= itemBatchSize {
		return w.Flush()
	}
	return nil
}

func (w *ItemWriter) nameID(ctx context.Context, name string) (int64, error) {
	if id, ok := w.nameIDs[name]; ok {
		return id, nil
	}
	var id int64
	err := w.getName.QueryRowContext(ctx, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var res sql.Result
		if res, err = w.insName.ExecContext(ctx, name); err == nil {
			id, err = res.LastInsertId()
		}
	}
	if err != nil {
		return 0, err
	}
	if len(w.nameIDs) >= maxCachedNames {
		clear(w.nameIDs)
	}
	w.nameIDs[name] = id
	return id, nil
}

// Flush commits the current batch, if any, with the contents stored so
// far (their bytes are synced first; an item's content is stored before
// the item is written, so it is never saved later than the item).
func (w *ItemWriter) Flush() error {
	if w.contents != nil {
		chunks, files, err := w.contents.Commit()
		if err != nil {
			return err
		}
		if len(chunks) > 0 || len(files) > 0 {
			ctx := context.Background()
			if w.tx == nil {
				if err := w.begin(ctx); err != nil {
					return err
				}
			}
			if err := saveContents(ctx, w.tx, chunks, files); err != nil {
				return err
			}
		}
	}
	if w.tx == nil {
		return nil
	}
	err := w.tx.Commit()
	w.tx, w.insItem, w.insDir, w.getName, w.insName = nil, nil, nil, nil, nil
	w.pending = 0
	return err
}

// writeDir records a directory as found (see scan.Item.Listed).
func (w *ItemWriter) writeDir(ctx context.Context, it scan.Item) error {
	var parent any
	if it.ParentID != 0 {
		parent = it.ParentID
	}
	if _, err := w.insDir.ExecContext(ctx, it.ID, w.backupID, parent, it.Name); err != nil {
		return err
	}
	w.pending++
	if w.pending >= itemBatchSize {
		return w.Flush()
	}
	return nil
}

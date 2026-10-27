package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/scan"
)

// Backup statuses. Running and paused are the only active ones; a backup left
// active when the program exits is resumed on the next start (see
// RecoverBackup), or becomes interrupted when that is not possible.
const (
	StatusRunning     = "running"
	StatusPaused      = "paused"
	StatusCompleted   = "completed"
	StatusStopped     = "stopped"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// Backup is a backup as recorded in the database.
type Backup struct {
	ID         int64         `json:"id"`
	Status     string        `json:"status"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
	Roots      []string      `json:"roots"`
	Progress   scan.Progress `json:"progress"`
	Error      string        `json:"error,omitempty"`
	// Ready is set once the duplicate analysis of a completed backup ran.
	Ready bool `json:"ready"`
}

// CreateBackup records a new running backup with its root paths; its items
// will get ids from firstItemID on (see NextItemID).
func (s *Store) CreateBackup(ctx context.Context, roots []string, firstItemID int64, startedAt time.Time) (id int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	res, err := tx.ExecContext(ctx, `INSERT INTO backups (status, started_at, first_item_id, resumable) VALUES (?, ?, ?, 1)`,
		StatusRunning, startedAt.UnixMilli(), firstItemID)
	if err != nil {
		return 0, err
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	for i, root := range roots {
		if _, err = tx.ExecContext(ctx, `INSERT INTO backup_roots (backup_id, position, path) VALUES (?, ?, ?)`, id, i, root); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// SetStatus sets the status of backup id.
func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET status = ? WHERE id = ?`, status, id)
	return err
}

// FinishBackup stores the final status and counters of a backup. It can no
// longer be resumed, so its directory records are dropped.
func (s *Store) FinishBackup(ctx context.Context, id int64, status string, p scan.Progress, finishedAt time.Time, errMsg string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE backups SET status = ?, finished_at = ?, dirs_found = ?, dirs_done = ?, files_found = ?,
			       files_done = ?, bytes_found = ?, bytes_done = ?, errors = ?, error = NULLIF(?, ''), resumable = 0
			WHERE id = ?`,
			status, finishedAt.UnixMilli(), p.DirsFound, p.DirsDone, p.FilesFound, p.FilesDone, p.BytesFound, p.BytesDone, p.Errors, errMsg, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM backup_dirs WHERE backup_id = ?`, id)
		}
		return err
	})
}

// SaveProgress stores the counters of an unfinished backup (shown in the
// history while the program is not running).
func (s *Store) SaveProgress(ctx context.Context, id int64, p scan.Progress) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE backups SET dirs_found = ?, dirs_done = ?, files_found = ?, files_done = ?, bytes_found = ?,
		       bytes_done = ?, errors = ? WHERE id = ?`,
		p.DirsFound, p.DirsDone, p.FilesFound, p.FilesDone, p.BytesFound, p.BytesDone, p.Errors, id)
	return err
}

// RecoverBackup finds the backup that was still active when the program last
// exited and can be resumed: one that records its directories and is the
// newest backup (its item ids must stay above every other backup's). Other
// active backups become interrupted, as does that one on failure (see
// MarkInterrupted).
func (s *Store) RecoverBackup(ctx context.Context) (sc Backup, ok bool, err error) {
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM backups WHERE status IN (?, ?) AND resumable = 1
		AND id = (SELECT MAX(id) FROM backups)`, StatusRunning, StatusPaused).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Backup{}, false, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE backups SET status = ?, resumable = 0 WHERE status IN (?, ?) AND id != ?`,
		StatusInterrupted, StatusRunning, StatusPaused, id); err != nil {
		return Backup{}, false, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM backup_dirs WHERE backup_id != ?`, id); err != nil {
		return Backup{}, false, err
	}
	if id == 0 {
		return Backup{}, false, nil
	}
	sc, err = s.GetBackup(ctx, id)
	return sc, err == nil, err
}

// MarkInterrupted ends a backup that could not be resumed, keeping its
// last saved counters.
func (s *Store) MarkInterrupted(ctx context.Context, id int64, errMsg string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE backups SET status = ?, error = NULLIF(?, ''), resumable = 0 WHERE id = ?`,
			StatusInterrupted, errMsg, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM backup_dirs WHERE backup_id = ?`, id)
		}
		return err
	})
}

// LoadPartial reads what an unfinished backup has stored (see scan.Partial).
func (s *Store) LoadPartial(ctx context.Context, backupID int64) (scan.Partial, error) {
	var p scan.Partial
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, COALESCE(d.parent_id, 0), d.name FROM backup_dirs d
		WHERE d.backup_id = ? AND NOT EXISTS (SELECT 1 FROM items i WHERE i.id = d.id)`, backupID)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var d scan.PartialDir
		if err := rows.Scan(&d.ID, &d.ParentID, &d.Name); err != nil {
			rows.Close()
			return p, err
		}
		p.Dirs = append(p.Dirs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT n.name FROM items i JOIN names n ON n.id = i.name_id
		WHERE i.backup_id = ? AND i.parent_id IS NULL`, backupID)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return p, err
		}
		p.DoneRoots = append(p.DoneRoots, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}

	// Counted over the backup's id range (the primary key); see backupRange.
	first, end, err := backupRange(ctx, s.db, backupID)
	if err != nil {
		return p, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT kind, error IS NOT NULL, COUNT(*), COALESCE(SUM(size), 0)
		FROM items WHERE id >= ? AND id < ? AND backup_id = ? GROUP BY 1, 2`, first, end, backupID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	st := &p.Stored
	for rows.Next() {
		var kind, n, size int64
		var failed bool
		if err := rows.Scan(&kind, &failed, &n, &size); err != nil {
			return p, err
		}
		if scan.Kind(kind) == scan.KindDir {
			st.DirsFound += n
			if !failed {
				st.DirsDone += n
			}
		} else {
			st.FilesFound += n
			if !failed {
				st.FilesDone += n
				st.BytesFound += size
				st.BytesDone += size
			}
		}
		if failed {
			st.Errors += n
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}

	p.Children = func(dirID int64) ([]scan.StoredChild, error) {
		rows, err := s.db.QueryContext(context.Background(), `
			SELECT n.name, i.hash FROM items i JOIN names n ON n.id = i.name_id WHERE i.parent_id = ?`, dirID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var children []scan.StoredChild
		for rows.Next() {
			var c scan.StoredChild
			var hash []byte
			if err := rows.Scan(&c.Name, &hash); err != nil {
				return nil, err
			}
			c.OK = len(hash) == len(c.Hash)
			copy(c.Hash[:], hash)
			children = append(children, c)
		}
		return children, rows.Err()
	}
	return p, nil
}

// ListBackups returns all backups, newest first.
func (s *Store) ListBackups(ctx context.Context) ([]Backup, error) {
	return s.queryBackups(ctx, `ORDER BY id DESC`)
}

// GetBackup returns backup id, or ErrNotFound.
func (s *Store) GetBackup(ctx context.Context, id int64) (Backup, error) {
	backups, err := s.queryBackups(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Backup{}, err
	}
	if len(backups) == 0 {
		return Backup{}, ErrNotFound
	}
	return backups[0], nil
}

func (s *Store) queryBackups(ctx context.Context, where string, args ...any) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status, started_at, finished_at, dirs_found, dirs_done, files_found,
		       files_done, bytes_found, bytes_done, errors, COALESCE(error, ''), ready
		FROM backups `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	backups := []Backup{}
	byID := map[int64]*Backup{}
	for rows.Next() {
		var sc Backup
		var started int64
		var finished sql.NullInt64
		p := &sc.Progress
		if err := rows.Scan(&sc.ID, &sc.Status, &started, &finished, &p.DirsFound, &p.DirsDone,
			&p.FilesFound, &p.FilesDone, &p.BytesFound, &p.BytesDone, &p.Errors, &sc.Error, &sc.Ready); err != nil {
			return nil, err
		}
		sc.StartedAt = time.UnixMilli(started)
		if finished.Valid {
			t := time.UnixMilli(finished.Int64)
			sc.FinishedAt = &t
		}
		sc.Roots = []string{}
		backups = append(backups, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range backups {
		byID[backups[i].ID] = &backups[i]
	}
	if len(backups) == 0 {
		return backups, nil
	}

	rootRows, err := s.db.QueryContext(ctx, `SELECT backup_id, path FROM backup_roots ORDER BY backup_id, position`)
	if err != nil {
		return nil, err
	}
	defer rootRows.Close()
	for rootRows.Next() {
		var id int64
		var path string
		if err := rootRows.Scan(&id, &path); err != nil {
			return nil, err
		}
		if sc := byID[id]; sc != nil {
			sc.Roots = append(sc.Roots, path)
		}
	}
	return backups, rootRows.Err()
}

// NextItemID returns the first unused item id. Item ids are assigned by the
// scanner (parents get theirs before their children are written), which is
// safe because only one backup writes at a time. Ids of listed directories
// count as used: a resumed backup keeps them.
func (s *Store) NextItemID(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(COALESCE((SELECT MAX(id) FROM items), 0), COALESCE((SELECT MAX(id) FROM backup_dirs), 0))`).Scan(&max); err != nil {
		return 0, err
	}
	return max.Int64 + 1, nil
}

// queryRower is a *sql.DB or a *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// backupRange returns the id range [first, end) holding a backup's items
// (empty when it has none): a backup's ids are contiguous and end where the
// next backup's begin. Other backups' rows can only be in deleted gaps.
func backupRange(ctx context.Context, q queryRower, backupID int64) (first, end int64, err error) {
	var a, b sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT first_item_id FROM backups WHERE id = ?`, backupID).Scan(&a); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	if !a.Valid {
		return 0, 0, nil
	}
	if err := q.QueryRowContext(ctx, `SELECT MIN(first_item_id) FROM backups WHERE id > ?`, backupID).Scan(&b); err != nil {
		return 0, 0, err
	}
	if !b.Valid {
		if err := q.QueryRowContext(ctx, `SELECT MAX(id) + 1 FROM items`).Scan(&b); err != nil {
			return 0, 0, err
		}
	}
	return a.Int64, max(b.Int64, a.Int64), nil
}

// Deleted counts what DeleteBackup removed.
type Deleted struct {
	Items      int64 `json:"items"`
	Restores   int64 `json:"restores"`
	Names      int64 `json:"names"`      // file names no other backup uses
	Contents   int64 `json:"contents"`   // file contents no other backup uses
	BytesFreed int64 `json:"bytesFreed"` // in the content store, by compaction
}

// DeleteBackup removes a backup and everything derived from it: its items
// (with their exclusions and duplicate data), the records of its restores,
// the names and file contents only it used; then it compacts the content
// store. Files on disk, including restored ones, are not touched. The caller
// makes sure no backup or restore is active.
func (s *Store) DeleteBackup(ctx context.Context, backupID int64) (d Deleted, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		first, end, err := backupRange(ctx, tx, backupID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM restore_items WHERE restore_id IN (SELECT id FROM restores WHERE backup_id = ?)`, backupID); err != nil {
			return err
		}
		if d.Restores, err = affected(tx.ExecContext(ctx, `DELETE FROM restores WHERE backup_id = ?`, backupID)); err != nil {
			return err
		}
		// By id range (the primary key): there is no plain backup_id index.
		if d.Items, err = affected(tx.ExecContext(ctx,
			`DELETE FROM items WHERE id >= ? AND id < ? AND backup_id = ?`, first, end, backupID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM backup_dirs WHERE backup_id = ?`, backupID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM backup_roots WHERE backup_id = ?`, backupID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, backupID); err != nil {
			return err
		}
		if d.Names, err = affected(tx.ExecContext(ctx, `DELETE FROM names WHERE id NOT IN (SELECT name_id FROM items)`)); err != nil {
			return err
		}
		d.Contents, err = dropUnusedContents(ctx, tx)
		return err
	})
	if err != nil {
		return d, err
	}
	d.BytesFreed, err = s.CompactContents(ctx)
	return d, err
}

func affected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

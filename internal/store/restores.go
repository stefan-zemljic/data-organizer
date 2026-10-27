package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrRestoreUnfinished = errors.New("a restore of this backup is running or paused; exclusions can change when it has finished or was stopped")

// Restore statuses. Running and paused restores are unfinished: the next
// start continues them (pausing and stopping are in-memory only).
const (
	RestoreRunning     = "running"
	RestorePaused      = "paused"
	RestoreCompleted   = "completed"
	RestoreStopped     = "stopped"
	RestoreFailed      = "failed"
	RestoreInterrupted = "interrupted"
)

// Outcome of a restored item (restore_items.status).
const (
	RestoreItemOK     = 1 // restored as backed up
	RestoreItemFailed = 2 // not written to the target, or its stored content is damaged
)

// Directory flags (restore_items.flags): what happened somewhere below.
const RestoreFlagErrorsBelow = 1 // a failed item below

// RestoreProgress counts a restore's work. FilesDone = OK + Failed.
type RestoreProgress struct {
	FilesTotal  int64 `json:"filesTotal"`
	BytesTotal  int64 `json:"bytesTotal"`
	FilesDone   int64 `json:"filesDone"`
	BytesDone   int64 `json:"bytesDone"`
	FilesOK     int64 `json:"filesOk"`
	FilesFailed int64 `json:"filesFailed"`
	DirsCreated int64 `json:"dirsCreated"`
}

// Restore is a restore as recorded.
type Restore struct {
	ID         int64           `json:"id"`
	BackupID   int64           `json:"backupId"`
	Target     string          `json:"target"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	Progress   RestoreProgress `json:"progress"`
	Error      string          `json:"error,omitempty"`
}

func (s *Store) CreateRestore(ctx context.Context, backupID int64, target string, startedAt time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO restores (backup_id, target, status, started_at) VALUES (?, ?, ?, ?)`,
		backupID, target, RestoreRunning, startedAt.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishRestore(ctx context.Context, id int64, status string, p RestoreProgress, finishedAt time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE restores SET status = ?, finished_at = ?, files_total = ?, bytes_total = ?, files_done = ?, bytes_done = ?,
		       files_ok = ?, files_failed = ?, dirs_created = ?, error = NULLIF(?, '')
		WHERE id = ?`,
		status, finishedAt.UnixMilli(), p.FilesTotal, p.BytesTotal, p.FilesDone, p.BytesDone,
		p.FilesOK, p.FilesFailed, p.DirsCreated, errMsg, id)
	return err
}

// SaveRestoreProgress records the status and counters of an unfinished
// restore (paused, or running when the program exits).
func (s *Store) SaveRestoreProgress(ctx context.Context, id int64, status string, p RestoreProgress) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE restores SET status = ?, files_total = ?, bytes_total = ?, files_done = ?, bytes_done = ?,
		       files_ok = ?, files_failed = ?, dirs_created = ?
		WHERE id = ?`,
		status, p.FilesTotal, p.BytesTotal, p.FilesDone, p.BytesDone, p.FilesOK, p.FilesFailed, p.DirsCreated, id)
	return err
}

// RecoverRestore returns the newest unfinished (running or paused) restore,
// the one the last run left behind. Older unfinished ones become interrupted.
func (s *Store) RecoverRestore(ctx context.Context) (Restore, bool, error) {
	restores, err := s.queryRestores(ctx, `WHERE status IN (?, ?) ORDER BY id DESC`, RestoreRunning, RestorePaused)
	if err != nil || len(restores) == 0 {
		return Restore{}, false, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE restores SET status = ? WHERE status IN (?, ?) AND id != ?`,
		RestoreInterrupted, RestoreRunning, RestorePaused, restores[0].ID); err != nil {
		return Restore{}, false, err
	}
	return restores[0], true, nil
}

// unfinishedRestore refuses changes to a backup's exclusions while one of its
// restores is unfinished: a resumed restore plans again from the exclusions.
func (s *Store) unfinishedRestore(ctx context.Context, backupID int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM restores WHERE backup_id = ? AND status IN (?, ?)`,
		backupID, RestoreRunning, RestorePaused).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrRestoreUnfinished
	}
	return nil
}

// RestoreOutcomes calls fn for every item outcome a restore has recorded.
func (s *Store) RestoreOutcomes(ctx context.Context, restoreID int64, fn func(itemID int64, status int)) error {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, status FROM restore_items WHERE restore_id = ?`, restoreID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var status int
		if err := rows.Scan(&id, &status); err != nil {
			return err
		}
		fn(id, status)
	}
	return rows.Err()
}

func (s *Store) ListRestores(ctx context.Context) ([]Restore, error) {
	return s.queryRestores(ctx, `ORDER BY id DESC`)
}

func (s *Store) GetRestore(ctx context.Context, id int64) (Restore, error) {
	restores, err := s.queryRestores(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Restore{}, err
	}
	if len(restores) == 0 {
		return Restore{}, ErrNotFound
	}
	return restores[0], nil
}

func (s *Store) queryRestores(ctx context.Context, where string, args ...any) ([]Restore, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, backup_id, target, status, started_at, finished_at, files_total, bytes_total, files_done, bytes_done,
		       files_ok, files_failed, dirs_created, COALESCE(error, '')
		FROM restores `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	restores := []Restore{}
	for rows.Next() {
		var c Restore
		var started int64
		var finished sql.NullInt64
		p := &c.Progress
		if err := rows.Scan(&c.ID, &c.BackupID, &c.Target, &c.Status, &started, &finished, &p.FilesTotal, &p.BytesTotal,
			&p.FilesDone, &p.BytesDone, &p.FilesOK, &p.FilesFailed, &p.DirsCreated, &c.Error); err != nil {
			return nil, err
		}
		c.StartedAt = time.UnixMilli(started)
		if finished.Valid {
			t := time.UnixMilli(finished.Int64)
			c.FinishedAt = &t
		}
		restores = append(restores, c)
	}
	return restores, rows.Err()
}

// PlanBase is the compact per-item data of a backup needed to plan a restore,
// indexed by item id - MinID (a backup's item ids are contiguous).
type PlanBase struct {
	MinID  int64
	Parent []int64 // parent id, 0 for roots
	Marked []bool  // explicitly excluded
	OK     []bool  // readable during the backup (has a hash)
	IsDir  []bool
	Size   []int64
	Exists []bool // id belongs to the backup
}

// RestorePlanBase loads every item of a backup in id order (parents before
// children), reading the backup's primary key range (see backupRange).
func (s *Store) RestorePlanBase(ctx context.Context, backupID int64) (*PlanBase, error) {
	first, end, err := backupRange(ctx, s.db, backupID)
	if err != nil {
		return nil, err
	}
	if end <= first {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, backup_id, COALESCE(parent_id, 0), excluded, error IS NULL, kind = 1, COALESCE(size, 0)
		FROM items WHERE id >= ? AND id < ? ORDER BY id`, first, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b := &PlanBase{MinID: first}
	for rows.Next() {
		var id, backup, parent, size int64
		var marked, ok, isDir bool
		if err := rows.Scan(&id, &backup, &parent, &marked, &ok, &isDir, &size); err != nil {
			return nil, err
		}
		if backup != backupID {
			continue
		}
		i := int(id - b.MinID)
		for len(b.Parent) <= i {
			b.Parent = append(b.Parent, 0)
			b.Marked = append(b.Marked, false)
			b.OK = append(b.OK, false)
			b.IsDir = append(b.IsDir, false)
			b.Size = append(b.Size, 0)
			b.Exists = append(b.Exists, false)
		}
		b.Parent[i], b.Marked[i], b.OK[i], b.IsDir[i], b.Size[i], b.Exists[i] = parent, marked, ok, isDir, size, true
	}
	return b, rows.Err()
}

// DuplicatesInRankOrder calls fn for every duplicate of a backup, grouped by
// hash and ordered by path rank within each group.
func (s *Store) DuplicatesInRankOrder(ctx context.Context, backupID int64, fn func(id int64, hash []byte)) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, hash FROM items WHERE backup_id = ? AND dup_count > 0 ORDER BY hash, dup_rank`, backupID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var hash []byte
		if err := rows.Scan(&id, &hash); err != nil {
			return err
		}
		fn(id, hash)
	}
	return rows.Err()
}

// RestoreSource is what the restore walk needs about one item.
type RestoreSource struct {
	ID      int64
	Parent  int64
	Name    string
	IsDir   bool
	Size    int64
	ModTime int64
	Hash    []byte
}

// RestoreSourceRoots returns the root items of a backup.
func (s *Store) RestoreSourceRoots(ctx context.Context, backupID int64) ([]RestoreSource, error) {
	return s.queryRestoreSources(ctx, `WHERE i.backup_id = ? AND i.parent_id IS NULL ORDER BY i.id`, backupID)
}

// RestoreSourceChildren returns the children of the given directories.
func (s *Store) RestoreSourceChildren(ctx context.Context, parentIDs []int64) ([]RestoreSource, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(parentIDs))
	for i, id := range parentIDs {
		args[i] = id
	}
	return s.queryRestoreSources(ctx, `WHERE i.parent_id IN (?`+strings.Repeat(",?", len(parentIDs)-1)+`)`, args...)
}

func (s *Store) queryRestoreSources(ctx context.Context, where string, args ...any) ([]RestoreSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, COALESCE(i.parent_id, 0), n.name, i.kind = 1, COALESCE(i.size, 0), COALESCE(i.mtime, 0), i.hash
		FROM items i JOIN names n ON n.id = i.name_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []RestoreSource
	for rows.Next() {
		var it RestoreSource
		if err := rows.Scan(&it.ID, &it.Parent, &it.Name, &it.IsDir, &it.Size, &it.ModTime, &it.Hash); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// RestoreOutcome is one row of restore_items.
type RestoreOutcome struct {
	ItemID int64
	Status int
	Flags  int
	Err    string
}

// RestoreItemWriter inserts restore outcomes in batches; one goroutine owns it.
type RestoreItemWriter struct {
	db        *sql.DB
	restoreID int64
	tx        *sql.Tx
	ins       *sql.Stmt
	pending   int
}

func (s *Store) NewRestoreItemWriter(restoreID int64) *RestoreItemWriter {
	return &RestoreItemWriter{db: s.db, restoreID: restoreID}
}

func (w *RestoreItemWriter) Write(ctx context.Context, o RestoreOutcome) error {
	if w.tx == nil {
		tx, err := w.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		ins, err := tx.PrepareContext(ctx, `INSERT INTO restore_items (restore_id, item_id, status, flags, error) VALUES (?, ?, ?, ?, NULLIF(?, ''))
			ON CONFLICT (restore_id, item_id) DO UPDATE SET status = excluded.status, flags = excluded.flags, error = excluded.error`)
		if err != nil {
			tx.Rollback()
			return err
		}
		w.tx, w.ins = tx, ins
	}
	if _, err := w.ins.ExecContext(ctx, w.restoreID, o.ItemID, o.Status, o.Flags, o.Err); err != nil {
		return err
	}
	w.pending++
	if w.pending >= itemBatchSize {
		return w.Flush()
	}
	return nil
}

func (w *RestoreItemWriter) Flush() error {
	if w.tx == nil {
		return nil
	}
	err := w.tx.Commit()
	w.tx, w.ins, w.pending = nil, nil, 0
	return err
}

// RestoreEntry is an item as shown in a restore.
type RestoreEntry struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Size        *int64 `json:"size,omitempty"`
	Status      string `json:"status"` // ok, failed
	ErrorsBelow bool   `json:"errorsBelow"`
	Error       string `json:"error,omitempty"`
}

var restoreStatusNames = map[int]string{RestoreItemOK: "ok", RestoreItemFailed: "failed"}

func (s *Store) restoreBackupID(ctx context.Context, restoreID int64) (int64, error) {
	var backupID int64
	err := s.db.QueryRowContext(ctx, `SELECT backup_id FROM restores WHERE id = ?`, restoreID).Scan(&backupID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return backupID, err
}

const restoreEntrySelect = `SELECT i.id, n.name, i.kind, i.size, c.status, c.flags, COALESCE(c.error, '')
	FROM items i JOIN names n ON n.id = i.name_id JOIN restore_items c ON c.restore_id = ? AND c.item_id = i.id `

// RestoreEntryRoots returns the restored root items of a restore.
func (s *Store) RestoreEntryRoots(ctx context.Context, restoreID int64) ([]RestoreEntry, error) {
	backupID, err := s.restoreBackupID(ctx, restoreID)
	if err != nil {
		return nil, err
	}
	return s.queryRestoreEntries(ctx, restoreEntrySelect+`WHERE i.backup_id = ? AND i.parent_id IS NULL `+entryOrder, restoreID, backupID)
}

// RestoreEntryChildren returns one page of a directory's children in a restore.
func (s *Store) RestoreEntryChildren(ctx context.Context, restoreID, parentID int64, offset, limit int) (EntryPage, []RestoreEntry, error) {
	if _, err := s.restoreBackupID(ctx, restoreID); err != nil {
		return EntryPage{}, nil, err
	}
	page := EntryPage{Offset: offset, Limit: limit}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items i JOIN restore_items c ON c.restore_id = ? AND c.item_id = i.id
		WHERE i.parent_id = ?`, restoreID, parentID).Scan(&page.Total); err != nil {
		return EntryPage{}, nil, err
	}
	entries, err := s.queryRestoreEntries(ctx, restoreEntrySelect+`WHERE i.parent_id = ? `+entryOrder+` LIMIT ? OFFSET ?`,
		restoreID, parentID, limit, offset)
	if err != nil {
		return EntryPage{}, nil, err
	}
	page.NextOffset = min(offset+limit, page.Total)
	return page, entries, nil
}

func (s *Store) queryRestoreEntries(ctx context.Context, query string, args ...any) ([]RestoreEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []RestoreEntry{}
	for rows.Next() {
		var e RestoreEntry
		var kind, status, flags int
		var size sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Name, &kind, &size, &status, &flags, &e.Error); err != nil {
			return nil, err
		}
		e.Kind = "file"
		if kind == 1 {
			e.Kind = "dir"
		}
		if size.Valid && kind == 0 {
			e.Size = &size.Int64
		}
		e.Status = restoreStatusNames[status]
		if e.Status == "" {
			e.Status = fmt.Sprint(status)
		}
		e.ErrorsBelow = flags&RestoreFlagErrorsBelow != 0
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

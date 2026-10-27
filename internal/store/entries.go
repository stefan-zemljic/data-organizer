package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotReady is returned for backups that did not complete or whose
// duplicate analysis has not run yet.
var ErrNotReady = errors.New("a backup can be browsed once it has completed")

// BackupEntry is a backed-up item as shown in the backup view.
type BackupEntry struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"` // full path for roots
	Kind     string     `json:"kind"` // "dir" or "file"
	Size     *int64     `json:"size,omitempty"`
	Modified *time.Time `json:"modified,omitempty"`
	Error    string     `json:"error,omitempty"`
	DupCount int        `json:"dupCount"` // copies found by the backup, 0 = unique
	// ExcludedSelf: explicitly excluded; Excluded: itself or an ancestor.
	ExcludedSelf bool `json:"excludedSelf"`
	Excluded     bool `json:"excluded"`
	// Excluded is derived on read: the item or an ancestor is marked.
	// NotFirst: a copy with a lower path exists that is not excluded, so
	// this one is a later duplicate (shown struck through). Excluded items
	// are never NotFirst.
	NotFirst bool `json:"notFirst"`
}

// EntryPage is one page of a directory's children; see fsbrowse.Page.
type EntryPage struct {
	Offset     int           `json:"offset"`
	Limit      int           `json:"limit"`
	Total      int           `json:"total"`
	NextOffset int           `json:"nextOffset"`
	Entries    []BackupEntry `json:"entries"`
}

const entryColumns = `i.id, n.name, i.kind, i.size, i.mtime, COALESCE(i.error, ''), i.dup_count,
	i.excluded, i.parent_id, i.hash, i.dup_rank`

const entryFrom = `FROM items i JOIN names n ON n.id = i.name_id`

// Directories first, then by name, matching the live file browser.
const entryOrder = `ORDER BY i.kind DESC, lower(n.name), n.name`

func (s *Store) checkReady(ctx context.Context, backupID int64) error {
	var ready bool
	err := s.db.QueryRowContext(ctx, `SELECT ready FROM backups WHERE id = ?`, backupID).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !ready {
		return ErrNotReady
	}
	return nil
}

// EntryRoots returns the root items of a completed backup.
func (s *Store) EntryRoots(ctx context.Context, backupID int64) ([]BackupEntry, error) {
	if err := s.checkReady(ctx, backupID); err != nil {
		return nil, err
	}
	return s.queryEntries(ctx, backupID, `
		SELECT `+entryColumns+` `+entryFrom+`
		WHERE i.backup_id = ? AND i.parent_id IS NULL `+entryOrder, backupID)
}

// EntryChildren returns one page of the children of a directory item.
func (s *Store) EntryChildren(ctx context.Context, backupID, parentID int64, offset, limit int) (EntryPage, error) {
	if err := s.checkReady(ctx, backupID); err != nil {
		return EntryPage{}, err
	}
	var owner int64
	err := s.db.QueryRowContext(ctx, `SELECT backup_id FROM items WHERE id = ?`, parentID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != backupID) {
		return EntryPage{}, ErrNotFound
	}
	if err != nil {
		return EntryPage{}, err
	}

	page := EntryPage{Offset: offset, Limit: limit}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE parent_id = ?`, parentID).Scan(&page.Total); err != nil {
		return EntryPage{}, err
	}
	page.Entries, err = s.queryEntries(ctx, backupID, `
		SELECT `+entryColumns+` `+entryFrom+`
		WHERE i.parent_id = ? `+entryOrder+` LIMIT ? OFFSET ?`, parentID, limit, offset)
	if err != nil {
		return EntryPage{}, err
	}
	page.NextOffset = min(offset+limit, page.Total)
	return page, nil
}

// EntryState returns the current entries for the given item ids (e.g. all
// items the client has loaded, after marks changed). Unknown ids are skipped.
func (s *Store) EntryState(ctx context.Context, backupID int64, itemIDs []int64) ([]BackupEntry, error) {
	if err := s.checkReady(ctx, backupID); err != nil {
		return nil, err
	}
	if len(itemIDs) > MaxItemsPerRequest {
		return nil, fmt.Errorf("%w (at most %d)", ErrTooManyItems, MaxItemsPerRequest)
	}
	const chunk = 5000 // stays below SQLite's parameter limit
	entries := []BackupEntry{}
	for start := 0; start < len(itemIDs); start += chunk {
		ids := itemIDs[start:min(start+chunk, len(itemIDs))]
		args := make([]any, 0, len(ids)+1)
		args = append(args, backupID)
		for _, id := range ids {
			args = append(args, id)
		}
		part, err := s.queryEntries(ctx, backupID, `SELECT `+entryColumns+` `+entryFrom+`
			WHERE i.backup_id = ? AND i.id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		entries = append(entries, part...)
	}
	return entries, nil
}

// queryEntries reads entries, then derives Excluded (the item or an
// ancestor is marked) and NotFirst (a lower-ranked copy exists that is not
// excluded) with one memoizing resolver for the whole request.
func (s *Store) queryEntries(ctx context.Context, backupID int64, query string, args ...any) ([]BackupEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	entries := []BackupEntry{}
	type extra struct {
		parent sql.NullInt64
		hash   []byte
		rank   int64
	}
	var extras []extra
	for rows.Next() {
		var e BackupEntry
		var x extra
		var kind int
		var size, mtime sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Name, &kind, &size, &mtime, &e.Error, &e.DupCount,
			&e.ExcludedSelf, &x.parent, &x.hash, &x.rank); err != nil {
			rows.Close()
			return nil, err
		}
		e.Kind = "file"
		if kind == 1 {
			e.Kind = "dir"
		}
		if size.Valid {
			e.Size = &size.Int64
		}
		if mtime.Valid {
			t := time.Unix(0, mtime.Int64)
			e.Modified = &t
		}
		entries = append(entries, e)
		extras = append(extras, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	r, err := newExclusionResolver(ctx, s.db)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for i := range entries {
		r.remember(entries[i].ID, extras[i].parent, entries[i].ExcludedSelf)
	}
	for i := range entries {
		e := &entries[i]
		if e.Excluded, err = r.excluded(e.ID); err != nil {
			return nil, err
		}
		if e.DupCount > 0 && !e.Excluded && extras[i].rank > 1 {
			if e.NotFirst, err = s.includedCopyBefore(ctx, r, backupID, extras[i].hash, extras[i].rank); err != nil {
				return nil, err
			}
		}
	}
	return entries, nil
}

// includedCopyBefore reports whether a copy ranked below rank exists that
// is not excluded. It reads the group in rank order in small batches and
// usually stops at the very first copy.
func (s *Store) includedCopyBefore(ctx context.Context, r *exclusionResolver, backupID int64, hash []byte, rank int64) (bool, error) {
	const batch = 64
	after := int64(0)
	for {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, parent_id, excluded, dup_rank FROM items
			WHERE backup_id = ? AND hash = ? AND dup_count > 0 AND dup_rank > ? AND dup_rank < ?
			ORDER BY dup_rank LIMIT ?`, backupID, hash, after, rank, batch)
		if err != nil {
			return false, err
		}
		var ids []int64
		for rows.Next() {
			var id, rk int64
			var parent sql.NullInt64
			var marked bool
			if err := rows.Scan(&id, &parent, &marked, &rk); err != nil {
				rows.Close()
				return false, err
			}
			r.remember(id, parent, marked)
			ids = append(ids, id)
			after = rk
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		for _, id := range ids {
			excluded, err := r.excluded(id)
			if err != nil {
				return false, err
			}
			if !excluded {
				return true, nil
			}
		}
		if len(ids) < batch {
			return false, nil
		}
	}
}

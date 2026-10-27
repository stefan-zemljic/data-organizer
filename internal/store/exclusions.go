package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MaxItemsPerRequest bounds the items of one exclusion or state request.
const MaxItemsPerRequest = 100_000

// Errors for invalid item lists in exclusion and state requests.
var (
	ErrNotExcludable = errors.New("these items cannot be excluded (none given, or unreadable)")
	ErrTooManyItems  = errors.New("too many items in one request")
)

// SetExcluded sets or clears the explicit exclusion mark of items. Only
// the marked items are written: an item counts as excluded while it or
// any ancestor is marked, which is derived when reading (exclusionResolver),
// so this is O(number of items given) however large the subtrees are.
// Copies elsewhere are not affected. Unreadable items are skipped; if
// nothing is left, ErrNotExcludable.
func (s *Store) SetExcluded(ctx context.Context, backupID int64, itemIDs []int64, excluded bool) error {
	if len(itemIDs) == 0 {
		return ErrNotExcludable
	}
	if len(itemIDs) > MaxItemsPerRequest {
		return fmt.Errorf("%w (at most %d)", ErrTooManyItems, MaxItemsPerRequest)
	}
	if err := s.checkReady(ctx, backupID); err != nil {
		return err
	}
	if err := s.unfinishedRestore(ctx, backupID); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		check, err := tx.PrepareContext(ctx, `SELECT error IS NOT NULL FROM items WHERE id = ? AND backup_id = ?`)
		if err != nil {
			return err
		}
		defer check.Close()
		set, err := tx.PrepareContext(ctx, `UPDATE items SET excluded = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer set.Close()
		applied := 0
		for _, id := range itemIDs {
			var unreadable bool
			err := check.QueryRowContext(ctx, id, backupID).Scan(&unreadable)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if unreadable {
				continue
			}
			if _, err := set.ExecContext(ctx, excluded, id); err != nil {
				return err
			}
			applied++
		}
		if applied == 0 {
			return ErrNotExcludable
		}
		return nil
	})
}

// ResetExclusions removes every exclusion mark of a backup (partial index on
// marked items: only those rows are touched). Returns how many were removed.
func (s *Store) ResetExclusions(ctx context.Context, backupID int64) (int, error) {
	if err := s.checkReady(ctx, backupID); err != nil {
		return 0, err
	}
	if err := s.unfinishedRestore(ctx, backupID); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE items SET excluded = 0 WHERE backup_id = ? AND excluded != 0`, backupID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// exclusionResolver answers "is this item excluded?" (it or an ancestor is
// marked) by walking parent links, memoizing every item it visits. One
// resolver serves one read request, so the rows of a page share their
// ancestors' lookups.
type exclusionResolver struct {
	ctx    context.Context
	db     *sql.DB
	stmt   *sql.Stmt
	known  map[int64]bool           // id -> excluded (effective)
	direct map[int64]resolverRecord // id -> its own data, when already read
}

type resolverRecord struct {
	parent sql.NullInt64
	marked bool
}

func newExclusionResolver(ctx context.Context, db *sql.DB) (*exclusionResolver, error) {
	stmt, err := db.PrepareContext(ctx, `SELECT parent_id, excluded FROM items WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	return &exclusionResolver{ctx: ctx, db: db, stmt: stmt, known: map[int64]bool{}, direct: map[int64]resolverRecord{}}, nil
}

func (r *exclusionResolver) Close() { r.stmt.Close() }

// remember records an item's own data read elsewhere (saves a lookup).
func (r *exclusionResolver) remember(id int64, parent sql.NullInt64, marked bool) {
	r.direct[id] = resolverRecord{parent, marked}
}

func (r *exclusionResolver) excluded(id int64) (bool, error) {
	// Walk up until an item with a known answer, a marked item or a root.
	var chain []int64
	result := false
	for cur := id; ; {
		if v, ok := r.known[cur]; ok {
			result = v
			break
		}
		rec, ok := r.direct[cur]
		if !ok {
			if err := r.stmt.QueryRowContext(r.ctx, cur).Scan(&rec.parent, &rec.marked); err != nil {
				return false, err
			}
		}
		chain = append(chain, cur)
		if rec.marked {
			result = true
			break
		}
		if !rec.parent.Valid {
			break
		}
		cur = rec.parent.Int64
	}
	for _, c := range chain {
		r.known[c] = result
	}
	return result, nil
}

package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// AnalyzeDuplicates marks, within one backup, every item whose hash is shared
// by at least one other item (dup_count = group size) and ranks the copies
// of each group by path (dup_rank, 1 = lowest; see computeDuplicates).
// Unreadable items have no hash and stay unique.
func (s *Store) AnalyzeDuplicates(ctx context.Context, backupID int64) error {
	res, err := s.computeDuplicates(ctx, backupID)
	if err != nil {
		return fmt.Errorf("duplicate analysis: %w", err)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.writeDuplicates(ctx, tx, backupID, res); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE backups SET ready = 1 WHERE id = ?`, backupID)
		return err
	})
}

// dupResult is the duplicate analysis of one backup: for every item that
// shares its hash with another item, the group size and its rank by path.
type dupResult struct {
	minID int64
	count []int32 // per item index; 0 = unique
	rank  []int32 // 1 = lowest path in its group
}

// computeDuplicates groups the backup's items by hash and ranks the copies
// of each group by path: compared component by component, ASCII
// case-insensitively first and then exactly (as SQLite's
// ORDER BY lower(path), path did). Ranks come from a preorder walk of the
// tree with siblings sorted that way, so no path strings are built.
func (s *Store) computeDuplicates(ctx context.Context, backupID int64) (*dupResult, error) {
	first, end, err := backupRange(ctx, s.db, backupID)
	if err != nil || end <= first {
		return &dupResult{minID: first}, err
	}
	n := int(end - first)
	parent := make([]int32, n) // parent index, -1 for roots and foreign rows
	nameID := make([]int64, n)
	hashes := make([][32]byte, n)
	hasHash := make([]bool, n)
	inBackup := make([]bool, n)
	for i := range parent {
		parent[i] = -1
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, backup_id, parent_id, name_id, hash FROM items WHERE id >= ? AND id < ? ORDER BY id`, first, end)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, backup, nid int64
		var p sql.NullInt64
		var h []byte
		if err := rows.Scan(&id, &backup, &p, &nid, &h); err != nil {
			rows.Close()
			return nil, err
		}
		if backup != backupID {
			continue
		}
		i := int(id - first)
		inBackup[i] = true
		nameID[i] = nid
		if p.Valid {
			parent[i] = int32(p.Int64 - first)
		}
		if len(h) == 32 {
			copy(hashes[i][:], h)
			hasHash[i] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Group by hash: sort the hashed items by hash.
	order := make([]int32, 0, n)
	for i := range n {
		if hasHash[i] {
			order = append(order, int32(i))
		}
	}
	slices.SortFunc(order, func(a, b int32) int { return bytes.Compare(hashes[a][:], hashes[b][:]) })
	res := &dupResult{minID: first, count: make([]int32, n), rank: make([]int32, n)}
	var groups [][]int32
	for start := 0; start < len(order); {
		stop := start + 1
		for stop < len(order) && hashes[order[stop]] == hashes[order[start]] {
			stop++
		}
		if stop-start > 1 {
			g := order[start:stop]
			for _, i := range g {
				res.count[i] = int32(len(g))
			}
			groups = append(groups, g)
		}
		start = stop
	}
	if len(groups) == 0 {
		return res, nil
	}

	// Only duplicates and their ancestors take part in the ordering.
	needed := make([]bool, n)
	for _, g := range groups {
		for _, i := range g {
			for cur := i; cur >= 0 && !needed[cur]; cur = parent[cur] {
				needed[cur] = true
			}
		}
	}
	names, err := s.loadNames(ctx, nameID, needed)
	if err != nil {
		return nil, err
	}

	// Siblings compare by name; roots by their path split into components
	// (as the SQL did after replacing separators with char(1)).
	children := make(map[int32][]int32)
	var roots []int32
	lowerNames := make(map[int32]string)
	for i := range n {
		if !needed[i] {
			continue
		}
		lowerNames[int32(i)] = asciiLower(names[nameID[i]])
		if p := parent[i]; p >= 0 {
			children[p] = append(children[p], int32(i))
		} else {
			roots = append(roots, int32(i))
		}
	}
	byName := func(a, b int32) int {
		if c := strings.Compare(lowerNames[a], lowerNames[b]); c != 0 {
			return c
		}
		return strings.Compare(names[nameID[a]], names[nameID[b]])
	}
	rootParts := func(i int32) []string {
		return strings.Split(strings.NewReplacer(`\`, "\x01", "/", "\x01").Replace(names[nameID[i]]), "\x01")
	}
	slices.SortFunc(roots, func(a, b int32) int {
		pa, pb := rootParts(a), rootParts(b)
		if c := slices.CompareFunc(pa, pb, func(x, y string) int { return strings.Compare(asciiLower(x), asciiLower(y)) }); c != 0 {
			return c
		}
		return slices.Compare(pa, pb)
	})

	// Preorder positions: a path that is lower component-wise comes first.
	pos := make([]int32, n)
	next := int32(0)
	stack := make([]int32, 0, 64)
	for r := len(roots) - 1; r >= 0; r-- {
		stack = append(stack, roots[r])
	}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		pos[i] = next
		next++
		kids := children[i]
		slices.SortFunc(kids, byName)
		for k := len(kids) - 1; k >= 0; k-- {
			stack = append(stack, kids[k])
		}
	}
	for _, g := range groups {
		slices.SortFunc(g, func(a, b int32) int { return int(pos[a]) - int(pos[b]) })
		for r, i := range g {
			res.rank[i] = int32(r + 1)
		}
	}
	return res, nil
}

// loadNames reads the names of the needed items.
func (s *Store) loadNames(ctx context.Context, nameID []int64, needed []bool) (map[int64]string, error) {
	set := map[int64]bool{}
	for i, ok := range needed {
		if ok {
			set[nameID[i]] = true
		}
	}
	ids := make([]any, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	names := make(map[int64]string, len(ids))
	const chunk = 5000
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM names WHERE id IN (?`+strings.Repeat(",?", len(part)-1)+`)`, part...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			names[id] = name
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// asciiLower lowercases A-Z only, like SQLite's built-in lower().
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// writeDuplicates stores counts and ranks for the backup's items, clearing
// earlier values first.
func (s *Store) writeDuplicates(ctx context.Context, tx *sql.Tx, backupID int64, r *dupResult) error {
	end := r.minID + int64(len(r.count))
	if _, err := tx.ExecContext(ctx, `UPDATE items SET dup_count = 0, dup_rank = 0
		WHERE id >= ? AND id < ? AND backup_id = ? AND (dup_count != 0 OR dup_rank != 0)`, r.minID, end, backupID); err != nil {
		return err
	}
	// One statement per row is dominated by driver overhead; instead load
	// the values into an in-memory temp table 500 rows per statement and
	// apply them with a single UPDATE ... FROM.
	steps := []string{
		`DROP TABLE IF EXISTS temp.dup_values`,
		`CREATE TEMP TABLE dup_values (id INTEGER PRIMARY KEY, n INTEGER, rnk INTEGER)`,
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	const perStmt = 500
	full, err := tx.PrepareContext(ctx, `INSERT INTO temp.dup_values VALUES `+strings.TrimSuffix(strings.Repeat("(?,?,?),", perStmt), ","))
	if err != nil {
		return err
	}
	defer full.Close()
	args := make([]any, 0, 3*perStmt)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		var err error
		if len(args) == 3*perStmt {
			_, err = full.ExecContext(ctx, args...)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO temp.dup_values VALUES `+strings.TrimSuffix(strings.Repeat("(?,?,?),", len(args)/3), ","), args...)
		}
		args = args[:0]
		return err
	}
	for i, c := range r.count {
		if c == 0 {
			continue
		}
		args = append(args, r.minID+int64(i), c, r.rank[i])
		if len(args) == 3*perStmt {
			if err := flush(); err != nil {
				return fmt.Errorf("writing duplicates: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return fmt.Errorf("writing duplicates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET dup_count = v.n, dup_rank = v.rnk
		FROM temp.dup_values v WHERE items.id = v.id`); err != nil {
		return fmt.Errorf("writing duplicates: %w", err)
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE temp.dup_values`)
	return err
}

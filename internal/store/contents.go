package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/stefan-zemljic/data-organizer/internal/content"
)

// The index of the content store (internal/content): chunks and which
// chunks make each file content. The bytes are in pack files in s.packDir.

// NewContentWriter returns a writer for a backup's file contents; it knows
// every chunk and content stored so far.
func (s *Store) NewContentWriter(ctx context.Context) (*content.Writer, error) {
	idx := content.Index{Chunks: make(map[[32]byte]int64), Files: make(map[[32]byte]struct{}), NextChunk: 1}
	rows, err := s.db.QueryContext(ctx, `SELECT id, hash FROM chunks`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var h []byte
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return nil, err
		}
		idx.Chunks[[32]byte(h)] = id
		idx.NextChunk = max(idx.NextChunk, id+1)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if rows, err = s.db.QueryContext(ctx, `SELECT hash FROM contents`); err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		idx.Files[[32]byte(h)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return content.NewWriter(s.packDir, idx)
}

func (s *Store) NewContentReader() (*content.Reader, error) {
	return content.NewReader(s.packDir)
}

// FileChunks returns the chunks of a stored file content in order.
func (s *Store) FileChunks(ctx context.Context, hash []byte) ([]content.Chunk, error) {
	var list []byte
	err := s.db.QueryRowContext(ctx, `SELECT chunks FROM contents WHERE hash = ?`, hash).Scan(&list)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: not in the store", content.ErrDamaged)
	}
	if err != nil {
		return nil, err
	}
	ids, err := content.DecodeIDs(list)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]content.Chunk, len(ids))
	const batch = 500
	for i := 0; i < len(ids); i += batch {
		part := ids[i:min(i+batch, len(ids))]
		args := make([]any, len(part))
		for k, id := range part {
			args[k] = id
		}
		chunks, err := queryChunks(ctx, s.db, `WHERE id IN (?`+strings.Repeat(",?", len(part)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, c := range chunks {
			byID[c.ID] = c
		}
	}
	out := make([]content.Chunk, len(ids))
	for i, id := range ids {
		c, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: chunk %d is missing", content.ErrDamaged, id)
		}
		out[i] = c
	}
	return out, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryChunks reads chunk places (without hashes).
func queryChunks(ctx context.Context, q querier, where string, args ...any) ([]content.Chunk, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, pack, offset, stored, size, codec FROM chunks `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []content.Chunk
	for rows.Next() {
		var c content.Chunk
		if err := rows.Scan(&c.ID, &c.Pack, &c.Offset, &c.Stored, &c.Size, &c.Codec); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// saveContents inserts what a content writer stored since its last commit.
func saveContents(ctx context.Context, tx *sql.Tx, chunks []content.Chunk, files []content.File) error {
	if len(chunks) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT INTO chunks (id, hash, pack, offset, stored, size, codec) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, c := range chunks {
			if _, err := st.ExecContext(ctx, c.ID, c.Hash[:], c.Pack, c.Offset, c.Stored, c.Size, c.Codec); err != nil {
				return err
			}
		}
	}
	if len(files) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT INTO contents (hash, size, chunks) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, f := range files {
			if _, err := st.ExecContext(ctx, f.Hash[:], f.Size, content.EncodeIDs(f.Chunks)); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropUnusedContents removes contents no file item references anymore and
// the chunks no content uses; it returns how many contents went. Their
// bytes stay in the packs until compaction.
func dropUnusedContents(ctx context.Context, tx *sql.Tx) (int64, error) {
	n, err := affected(tx.ExecContext(ctx,
		`DELETE FROM contents WHERE hash NOT IN (SELECT hash FROM items WHERE kind = 0 AND hash IS NOT NULL)`))
	if err != nil || n == 0 {
		return n, err
	}
	var used []uint64 // bit set of chunk ids
	rows, err := tx.QueryContext(ctx, `SELECT chunks FROM contents`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var list []byte
		if err := rows.Scan(&list); err != nil {
			rows.Close()
			return 0, err
		}
		ids, err := content.DecodeIDs(list)
		if err != nil {
			rows.Close()
			return 0, err
		}
		for _, id := range ids {
			for int(id/64) >= len(used) {
				used = append(used, 0)
			}
			used[id/64] |= 1 << (id % 64)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	all, err := queryChunks(ctx, tx, ``)
	if err != nil {
		return 0, err
	}
	del, err := tx.PrepareContext(ctx, `DELETE FROM chunks WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer del.Close()
	for _, c := range all {
		if int(c.ID/64) < len(used) && used[c.ID/64]&(1<<(c.ID%64)) != 0 {
			continue
		}
		if _, err := del.ExecContext(ctx, c.ID); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// CompactContents frees pack space: packs no chunk uses are deleted (also
// leftovers of a crash), packs mostly unused are rewritten. It returns the
// bytes freed. No backup may write meanwhile.
func (s *Store) CompactContents(ctx context.Context) (int64, error) {
	packs, err := content.Packs(s.packDir)
	if err != nil {
		return 0, err
	}
	if len(packs) == 0 {
		return 0, nil
	}
	live := make(map[int64]int64)
	rows, err := s.db.QueryContext(ctx, `SELECT pack, SUM(stored) FROM chunks GROUP BY pack`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var pack, n int64
		if err := rows.Scan(&pack, &n); err != nil {
			rows.Close()
			return 0, err
		}
		live[pack] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var remove, rewrite []int64
	var before int64
	for id, size := range packs {
		switch n := live[id]; {
		case n == 0:
			remove = append(remove, id)
			before += size
		case float64(n) < content.CompactRatio*float64(size):
			rewrite = append(rewrite, id)
			before += size
		}
	}
	if len(rewrite) > 0 {
		args := make([]any, len(rewrite))
		for i, id := range rewrite {
			args[i] = id
		}
		chunks, err := queryChunks(ctx, s.db, `WHERE pack IN (?`+strings.Repeat(",?", len(rewrite)-1)+`)`, args...)
		if err != nil {
			return 0, err
		}
		moved, err := content.Rewrite(s.packDir, chunks)
		if err != nil {
			return 0, err
		}
		err = s.inTx(ctx, func(tx *sql.Tx) error {
			st, err := tx.PrepareContext(ctx, `UPDATE chunks SET pack = ?, offset = ? WHERE id = ?`)
			if err != nil {
				return err
			}
			defer st.Close()
			for _, c := range moved {
				if _, err := st.ExecContext(ctx, c.Pack, c.Offset, c.ID); err != nil {
					return err
				}
				before -= c.Stored
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		remove = append(remove, rewrite...)
	}
	return before, content.RemovePacks(s.packDir, remove)
}

// PackDir returns the folder of this store's pack files.
func (s *Store) PackDir() string { return s.packDir }

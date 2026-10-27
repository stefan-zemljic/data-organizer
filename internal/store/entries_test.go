package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/scan"
)

// tree builds items for one backup from a list of paths below a root;
// entries ending in "/" are directories, the others files with the given hash.
type treeItem struct {
	path string
	hash byte // files only; 0 = unreadable
}

func writeTree(t *testing.T, s *Store, root string, items []treeItem) (backupID int64, ids map[string]int64) {
	t.Helper()
	ctx := context.Background()
	next, _ := s.NextItemID(ctx)
	backupID, err := s.CreateBackup(ctx, []string{root}, next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids = map[string]int64{"": next}
	next++
	w := s.NewItemWriter(backupID, nil)
	for _, it := range items {
		parentPath, name := splitLast(it.path)
		id := next
		next++
		ids[trimSlash(it.path)] = id
		item := scan.Item{ID: id, ParentID: ids[parentPath], Name: name}
		switch {
		case it.path[len(it.path)-1] == '/':
			item.Kind = scan.KindDir
			item.Name = trimSlash(name)
			item.Hash = [32]byte{0xD0, byte(id)} // unique per directory
		case it.hash == 0:
			item.Err = "denied"
		default:
			item.Hash = [32]byte{it.hash}
			item.Size = int64(it.hash)
		}
		if err := w.Write(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Write(ctx, scan.Item{ID: ids[""], Name: root, Kind: scan.KindDir, Hash: [32]byte{0xEE}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackup(ctx, backupID, StatusCompleted, scan.Progress{}, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	return backupID, ids
}

func splitLast(p string) (parent, name string) {
	p2 := trimSlash(p)
	for i := len(p2) - 1; i >= 0; i-- {
		if p2[i] == '/' {
			return p2[:i], p[i+1:]
		}
	}
	return "", p
}

func trimSlash(p string) string {
	if len(p) > 0 && p[len(p)-1] == '/' {
		return p[:len(p)-1]
	}
	return p
}

func TestAnalyzeDuplicates(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	backupID, ids := writeTree(t, s, `C:\R`, []treeItem{
		{path: "a/"},
		{path: "a b/"},
		{path: "B/"},
		{path: "a b/x.txt", hash: 7}, // "a b" sorts after "a" per component...
		{path: "a/z.txt", hash: 7},   // ...so this one is first
		{path: "B/y.txt", hash: 7},
		{path: "a/Q.txt", hash: 9}, // case-insensitive: "a/Q.txt" < "B/q.txt"
		{path: "B/q.txt", hash: 9},
		{path: "a/unique.txt", hash: 3},
		{path: "a/locked.txt"}, // unreadable: no hash, never a duplicate
		{path: "B/locked.txt"},
	})

	if _, err := s.EntryRoots(ctx, backupID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("before analysis: err = %v", err)
	}
	if err := s.AnalyzeDuplicates(ctx, backupID); err != nil {
		t.Fatal(err)
	}

	want := map[string][2]int{ // path -> {dupCount, dupFirst}
		"a/z.txt": {3, 1}, "a b/x.txt": {3, 0}, "B/y.txt": {3, 0},
		"a/Q.txt": {2, 1}, "B/q.txt": {2, 0},
		"a/unique.txt": {0, 0}, "a/locked.txt": {0, 0}, "B/locked.txt": {0, 0}, "a": {0, 0},
	}
	for path, w := range want {
		var count, first int
		s.db.QueryRow(`SELECT dup_count, dup_rank = 1 FROM items WHERE id = ?`, ids[path]).Scan(&count, &first)
		if count != w[0] || first != w[1] {
			t.Errorf("%s: dup_count=%d first=%d, want %d/%d", path, count, first, w[0], w[1])
		}
	}

	// Browsing entries.
	roots, err := s.EntryRoots(ctx, backupID)
	if err != nil || len(roots) != 1 || roots[0].Name != `C:\R` || roots[0].Kind != "dir" {
		t.Fatalf("roots = %+v, %v", roots, err)
	}
	page, err := s.EntryChildren(ctx, backupID, roots[0].ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || page.NextOffset != 2 || len(page.Entries) != 2 || page.Entries[0].Name != "a" || page.Entries[1].Name != "a b" {
		t.Errorf("page = %+v", page)
	}
	page, _ = s.EntryChildren(ctx, backupID, ids["a"], 0, 10)
	got := map[string]BackupEntry{}
	for _, e := range page.Entries {
		got[e.Name] = e
	}
	if e := got["z.txt"]; e.NotFirst || e.DupCount != 3 || e.Size == nil || *e.Size != 7 {
		t.Errorf("z.txt = %+v", e)
	}
	if e := got["locked.txt"]; e.Error != "denied" || e.DupCount != 0 {
		t.Errorf("locked.txt = %+v", e)
	}
	if _, err := s.EntryChildren(ctx, backupID+1, ids["a"], 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong backup: err = %v", err)
	}

	// Re-running is idempotent.
	if err := s.AnalyzeDuplicates(ctx, backupID); err != nil {
		t.Fatal(err)
	}
	var firsts int
	s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE backup_id = ? AND dup_rank = 1`, backupID).Scan(&firsts)
	if firsts != 2 {
		t.Errorf("firsts after re-run = %d, want 2", firsts)
	}
}

func TestDuplicatesAreScopedToBackup(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	first, _ := writeTree(t, s, `C:\R`, []treeItem{{path: "a.txt", hash: 5}})
	second, ids := writeTree(t, s, `C:\R`, []treeItem{{path: "a.txt", hash: 5}})
	s.AnalyzeDuplicates(ctx, first)
	s.AnalyzeDuplicates(ctx, second)
	var count int
	s.db.QueryRow(`SELECT dup_count FROM items WHERE id = ?`, ids["a.txt"]).Scan(&count)
	if count != 0 {
		t.Errorf("same file in another backup counted as duplicate: %d", count)
	}
}

// TestAnalyzeLargeBackup measures the analysis at the expected maximum scale.
// Run with DATA_ORGANIZER_BENCH=1 (takes a while).
func TestAnalyzeLargeBackup(t *testing.T) {
	if os.Getenv("DATA_ORGANIZER_BENCH") == "" {
		t.Skip("set DATA_ORGANIZER_BENCH=1 to run")
	}
	const items = 2_400_000
	ctx := context.Background()
	s := openTemp(t)
	backupID, _ := s.CreateBackup(ctx, []string{`C:\`}, 1, time.Now())
	w := s.NewItemWriter(backupID, nil)

	// 10 files per directory, directories nested up to 8 deep; about half of
	// the files share their content with others.
	start := time.Now()
	id := int64(1)
	rootID := id
	dirs := []int64{rootID}
	for id < items {
		parent := dirs[(id/10)%int64(len(dirs))]
		id++
		var h [32]byte
		if id%11 == 0 {
			dirs = append(dirs, id)
			binary.LittleEndian.PutUint64(h[:], uint64(id))
			h[31] = 1
			if err := w.Write(ctx, scan.Item{ID: id, ParentID: parent, Name: fmt.Sprintf("dir%d", id%5000), Kind: scan.KindDir, Hash: h}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		content := uint64(id)
		if id%2 == 0 {
			content = uint64(id % 200_000) // shared content
		}
		binary.LittleEndian.PutUint64(h[:], content)
		if err := w.Write(ctx, scan.Item{ID: id, ParentID: parent, Name: fmt.Sprintf("file%d.dat", id%50_000), Kind: scan.KindFile, Size: 1, Hash: h}); err != nil {
			t.Fatal(err)
		}
	}
	w.Write(ctx, scan.Item{ID: rootID, Name: `C:\`, Kind: scan.KindDir, Hash: [32]byte{9}})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d items in %v", id, time.Since(start))
	s.FinishBackup(ctx, backupID, StatusCompleted, scan.Progress{}, time.Now(), "")

	start = time.Now()
	if _, err := s.computeDuplicates(ctx, backupID); err != nil {
		t.Fatal(err)
	}
	t.Logf("computing alone took %v", time.Since(start))
	start = time.Now()
	if err := s.AnalyzeDuplicates(ctx, backupID); err != nil {
		t.Fatal(err)
	}
	var dups, firsts int
	s.db.QueryRow(`SELECT COUNT(*), SUM(dup_rank = 1) FROM items WHERE backup_id = ? AND dup_count > 0`, backupID).Scan(&dups, &firsts)
	t.Logf("analysis took %v: %d duplicate items in %d groups", time.Since(start), dups, firsts)
}

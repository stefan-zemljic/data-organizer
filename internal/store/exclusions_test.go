package store

import (
	"context"
	"errors"
	"testing"
)

// entriesFixture:
//
//	R/a/x.txt (7)   R/a/sub/deep.txt (9)
//	R/b/y.txt (7)   R/b/deep2.txt (9)
//	R/z.txt (7)     R/only.txt (3)     R/locked.txt (unreadable)
func entriesFixture(t *testing.T) (*Store, int64, map[string]int64, func(path string) BackupEntry) {
	t.Helper()
	ctx := context.Background()
	s := openTemp(t)
	backupID, ids := writeTree(t, s, `C:\R`, []treeItem{
		{path: "a/"},
		{path: "a/x.txt", hash: 7},
		{path: "a/sub/"},
		{path: "a/sub/deep.txt", hash: 9},
		{path: "b/"},
		{path: "b/y.txt", hash: 7},
		{path: "b/deep2.txt", hash: 9},
		{path: "z.txt", hash: 7},
		{path: "only.txt", hash: 3},
		{path: "locked.txt"},
	})
	if err := s.AnalyzeDuplicates(ctx, backupID); err != nil {
		t.Fatal(err)
	}
	entry := func(path string) BackupEntry {
		t.Helper()
		got, err := s.EntryState(ctx, backupID, []int64{ids[path]})
		if err != nil || len(got) != 1 {
			t.Fatalf("state of %s: %v %v", path, got, err)
		}
		return got[0]
	}
	return s, backupID, ids, entry
}

func excludePaths(t *testing.T, s *Store, backupID int64, ids map[string]int64, on bool, paths ...string) {
	t.Helper()
	var list []int64
	for _, p := range paths {
		list = append(list, ids[p])
	}
	if err := s.SetExcluded(context.Background(), backupID, list, on); err != nil {
		t.Fatal(err)
	}
}

func TestExclusionIsRecursive(t *testing.T) {
	ctx := context.Background()
	s, backupID, ids, entry := entriesFixture(t)

	excludePaths(t, s, backupID, ids, true, "a")
	if e := entry("a"); !e.ExcludedSelf || !e.Excluded {
		t.Errorf("a: %+v", e)
	}
	for _, p := range []string{"a/x.txt", "a/sub", "a/sub/deep.txt"} {
		if e := entry(p); e.ExcludedSelf || !e.Excluded {
			t.Errorf("%s must be excluded through a only: %+v", p, e)
		}
	}
	// Copies elsewhere are not affected.
	if e := entry("b/y.txt"); e.Excluded {
		t.Errorf("copy outside a excluded: %+v", e)
	}

	// Nested marks are independent: clearing a keeps the explicit mark below.
	excludePaths(t, s, backupID, ids, true, "a/sub/deep.txt")
	excludePaths(t, s, backupID, ids, false, "a")
	if !entry("a/sub/deep.txt").Excluded || entry("a/sub").Excluded || entry("a/x.txt").Excluded {
		t.Errorf("after including a: deep=%+v sub=%+v x=%+v", entry("a/sub/deep.txt"), entry("a/sub"), entry("a/x.txt"))
	}

	if err := s.SetExcluded(ctx, backupID, []int64{ids["locked.txt"]}, true); !errors.Is(err, ErrNotExcludable) {
		t.Errorf("exclude unreadable: %v", err)
	}
	if err := s.SetExcluded(ctx, backupID+1, []int64{ids["only.txt"]}, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong backup: %v", err)
	}
}

// Later copies (by path) are NotFirst; excluded copies do not count, so
// excluding the first copy makes the next one first.
func TestNotFirstFollowsExclusion(t *testing.T) {
	s, backupID, ids, entry := entriesFixture(t)
	notFirst := func(want map[string]bool) {
		t.Helper()
		for p, w := range want {
			if e := entry(p); e.NotFirst != w {
				t.Errorf("%s: notFirst=%v, want %v (%+v)", p, e.NotFirst, w, e)
			}
		}
	}
	// Paths: R\a\x.txt < R\b\y.txt < R\z.txt; R\a\sub\deep.txt < R\b\deep2.txt.
	notFirst(map[string]bool{"a/x.txt": false, "b/y.txt": true, "z.txt": true,
		"a/sub/deep.txt": false, "b/deep2.txt": true, "only.txt": false, "a": false})

	excludePaths(t, s, backupID, ids, true, "a")
	notFirst(map[string]bool{"a/x.txt": false, "b/y.txt": false, "z.txt": true, "b/deep2.txt": false})

	excludePaths(t, s, backupID, ids, false, "a")
	notFirst(map[string]bool{"a/x.txt": false, "b/y.txt": true, "z.txt": true, "b/deep2.txt": true})
}

// Reset removes every exclusion.
func TestResetExclusions(t *testing.T) {
	ctx := context.Background()
	s, backupID, ids, entry := entriesFixture(t)
	excludePaths(t, s, backupID, ids, true, "a", "only.txt")
	removed, err := s.ResetExclusions(ctx, backupID)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for _, p := range []string{"a", "a/x.txt", "a/sub/deep.txt", "only.txt"} {
		if e := entry(p); e.Excluded || e.ExcludedSelf {
			t.Errorf("%s after reset: %+v", p, e)
		}
	}
	if e := entry("b/y.txt"); !e.NotFirst {
		t.Errorf("y must be a later copy again: %+v", e)
	}
	if n, err := s.ResetExclusions(ctx, backupID); err != nil || n != 0 {
		t.Errorf("second reset: n=%d err=%v", n, err)
	}
}

// Marking is O(1): only the marked item's row changes, the subtree is
// derived on read, including for items far below.
func TestExclusionWritesOnlyMarkedRows(t *testing.T) {
	s, backupID, ids, entry := entriesFixture(t)
	excludePaths(t, s, backupID, ids, true, "")
	var marked int
	s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE backup_id = ? AND excluded != 0`, backupID).Scan(&marked)
	if marked != 1 {
		t.Errorf("rows marked = %d, want 1", marked)
	}
	if e := entry("a/sub/deep.txt"); !e.Excluded || e.ExcludedSelf || e.NotFirst {
		t.Errorf("deep item below the marked root: %+v", e)
	}
	// The resolver memoizes across rows: listing a page shares ancestors.
	page, err := s.EntryChildren(context.Background(), backupID, ids["a"], 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Entries {
		if !e.Excluded {
			t.Errorf("%s not excluded", e.Name)
		}
	}
}

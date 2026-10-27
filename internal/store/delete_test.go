package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/scan"
)

func TestDeleteBackup(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	first, ids1 := writeTree(t, s, `C:\one`, []treeItem{{"shared.txt", 1}, {"only-one.txt", 2}})
	second, _ := writeTree(t, s, `C:\two`, []treeItem{{"shared.txt", 1}, {"d/", 0}})
	third, _ := writeTree(t, s, `C:\three`, []treeItem{{"x.txt", 3}})
	for _, id := range []int64{first, second, third} {
		if err := s.AnalyzeDuplicates(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetExcluded(ctx, first, []int64{ids1["shared.txt"]}, true); err != nil {
		t.Fatal(err)
	}
	restoreID, err := s.CreateRestore(ctx, first, `D:\backup`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := s.NewRestoreItemWriter(restoreID)
	if err := w.Write(ctx, RestoreOutcome{ItemID: ids1["only-one.txt"], Status: RestoreItemOK}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	d, err := s.DeleteBackup(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	// Root, shared.txt, only-one.txt; names `C:\one` and "only-one.txt".
	if d != (Deleted{Items: 3, Restores: 1, Names: 2}) {
		t.Errorf("deleted = %+v", d)
	}
	if _, err := s.GetBackup(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Errorf("backup still there: %v", err)
	}
	if _, err := s.GetRestore(ctx, restoreID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore still there: %v", err)
	}
	count := func(query string) (n int) {
		s.db.QueryRow(query).Scan(&n)
		return n
	}
	if n := count(`SELECT COUNT(*) FROM restore_items`); n != 0 {
		t.Errorf("%d restore items left", n)
	}
	if n := count(`SELECT COUNT(*) FROM backup_roots WHERE path = 'C:\one'`); n != 0 {
		t.Errorf("%d roots left", n)
	}
	if n := count(`SELECT COUNT(*) FROM names WHERE name = 'shared.txt'`); n != 1 {
		t.Error("a name still used by another backup was deleted")
	}

	// The neighbours are untouched (the id range ends where they begin).
	for _, id := range []int64{second, third} {
		roots, err := s.EntryRoots(ctx, id)
		if err != nil || len(roots) != 1 {
			t.Errorf("backup %d: roots %v, %v", id, roots, err)
		}
	}
	if _, err := s.DeleteBackup(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting again: %v, want ErrNotFound", err)
	}
}

// A backup stopped before its root folder was complete has no root item;
// its items are still found through the stored first item id.
func TestDeleteBackupWithoutRootItem(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	next, _ := s.NextItemID(ctx)
	id, err := s.CreateBackup(ctx, []string{`C:\r`}, next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := s.NewItemWriter(id, nil)
	// Child of the (never written) root item next.
	if err := w.Write(ctx, scan.Item{ID: next + 1, ParentID: next, Name: "a.txt", Hash: [32]byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	d, err := s.DeleteBackup(ctx, id)
	if err != nil || d.Items != 1 {
		t.Errorf("deleted = %+v, %v; want 1 item", d, err)
	}
}

package backupjob

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

// bigTree has enough files that a backup can be caught halfway.
func bigTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for i := range 3000 {
		path := filepath.Join(root, fmt.Sprintf("d%d", i%7), fmt.Sprintf("e%d", i%11), fmt.Sprintf("f%04d", i))
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// startAndLeave starts a backup, lets it hash some files and shuts the
// manager down as the program does on exit.
func startAndLeave(t *testing.T, m *Manager, root string) int64 {
	t.Helper()
	ctx := context.Background()
	started, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cur, ok := m.Current(); !ok || cur.Progress.FilesDone >= 500 {
			break
		}
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	sc, err := m.Store().GetBackup(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Status != store.StatusRunning || sc.Progress.FilesDone >= 3000 {
		t.Fatalf("after shutdown: %+v; want an unfinished, running backup", sc)
	}
	return started.ID
}

// rootHash returns a backup's root item hash and its number of items.
func rootHash(t *testing.T, db *sql.DB, backupID int64) (hash []byte, items int) {
	t.Helper()
	if err := db.QueryRow(`SELECT hash FROM items WHERE backup_id = ? AND parent_id IS NULL`, backupID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM items WHERE backup_id = ?`, backupID).Scan(&items)
	return hash, items
}

// checkResumed recovers in a new manager (the next program start), lets the
// backup finish and compares it with a fresh backup of the same tree.
func checkResumed(t *testing.T, st *store.Store, dbPath, root string, id int64) {
	t.Helper()
	ctx := context.Background()
	m := New(st, scan.Options{Workers: 4})
	if err := m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, ok := m.Current(); !ok || cur.ID != id {
		t.Fatalf("recovered %+v, want backup %d", cur, id)
	}
	waitDone(t, m)
	sc, err := m.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	p := sc.Progress
	if sc.Status != store.StatusCompleted || !sc.Ready || p.FilesDone != 3000 || p.FilesFound != 3000 ||
		p.DirsDone != 1+7+77 || p.DirsFound != p.DirsDone || p.Errors != 0 {
		t.Fatalf("resumed backup = %+v", sc)
	}

	fresh, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, m)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gotHash, gotItems := rootHash(t, db, id)
	wantHash, wantItems := rootHash(t, db, fresh.ID)
	if !bytes.Equal(gotHash, wantHash) || gotItems != wantItems {
		t.Errorf("resumed backup: %d items, hash %x; fresh backup: %d items, hash %x", gotItems, gotHash, wantItems, wantHash)
	}
	var dirs int
	db.QueryRow(`SELECT COUNT(*) FROM backup_dirs`).Scan(&dirs)
	if dirs != 0 {
		t.Errorf("%d directory records left after finishing", dirs)
	}
}

func TestRestoreAfterShutdown(t *testing.T) {
	m, _, dbPath := setup(t)
	root := bigTree(t)
	id := startAndLeave(t, m, root)
	checkResumed(t, m.Store(), dbPath, root, id)
}

func TestRestoreAfterCrash(t *testing.T) {
	m, _, dbPath := setup(t)
	root := bigTree(t)
	id := startAndLeave(t, m, root)

	// Lose the batches a crash would: every third stored file, and with
	// each its folders (a folder is stored only after all its children).
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`
		WITH RECURSIVE lost(id, parent_id) AS (
			SELECT id, parent_id FROM items WHERE backup_id = ? AND kind = 0 AND id % 3 = 0
			UNION
			SELECT p.id, p.parent_id FROM lost JOIN items p ON p.id = lost.parent_id
		)
		DELETE FROM items WHERE id IN (SELECT id FROM lost)`, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("nothing deleted")
	}
	db.Close()
	checkResumed(t, m.Store(), dbPath, root, id)
}

func TestRestorePausedStaysPaused(t *testing.T) {
	ctx := context.Background()
	m, _, _ := setup(t)
	root := bigTree(t)
	id := startAndLeave(t, m, root)
	m.Store().SetStatus(ctx, id, store.StatusPaused)

	m2 := New(m.Store(), scan.Options{Workers: 4})
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cur, ok := m2.Current()
	if !ok || cur.Status != store.StatusPaused || cur.Progress.FilesDone >= 3000 {
		t.Fatalf("recovered %+v, want paused", cur)
	}
	if _, err := m2.Start(ctx, []string{root}); err == nil {
		t.Error("a new backup started while a recovered one is paused")
	}
	// Leaving again while paused keeps it paused.
	if err := m2.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if sc, _ := m2.Get(ctx, id); sc.Status != store.StatusPaused {
		t.Fatalf("status after second shutdown = %q", sc.Status)
	}
	m3 := New(m.Store(), scan.Options{Workers: 4})
	if err := m3.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m3.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitDone(t, m3)
	if sc, _ := m3.Get(ctx, id); sc.Status != store.StatusCompleted || sc.Progress.FilesDone != 3000 {
		t.Fatalf("after resume: %+v", sc)
	}
}

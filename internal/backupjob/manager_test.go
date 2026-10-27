package backupjob

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

func setup(t *testing.T) (*Manager, string, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "backup.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	root := t.TempDir()
	for i := range 200 {
		path := filepath.Join(root, fmt.Sprintf("d%d", i%5), fmt.Sprintf("f%03d.txt", i))
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(strings.Repeat("x", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, scan.Options{Workers: 4}), root, dbPath
}

func waitDone(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	j := m.active
	m.mu.Unlock()
	if j == nil {
		return
	}
	select {
	case <-j.done:
	case <-time.After(30 * time.Second):
		t.Fatal("backup did not finish")
	}
}

func waitStatus(t *testing.T, m *Manager, id int64, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sc, err := m.Get(context.Background(), id); err == nil && sc.Status == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("backup %d never reached status %q", id, status)
}

func TestBackupIsStored(t *testing.T) {
	ctx := context.Background()
	m, root, dbPath := setup(t)
	started, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, m)

	sc, err := m.Get(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Status != store.StatusCompleted || sc.FinishedAt == nil || !sc.Ready {
		t.Fatalf("backup = %+v", sc)
	}
	if p := sc.Progress; p.FilesDone != 200 || p.DirsDone != 6 || p.FilesFound != 200 || p.Errors != 0 {
		t.Errorf("progress = %+v", p)
	}

	// Rebuild one path from parent ids and shared names.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var path string
	err = db.QueryRow(`
		WITH RECURSIVE up(id, parent_id, path) AS (
			SELECT i.id, i.parent_id, n.name FROM items i JOIN names n ON n.id = i.name_id
			WHERE i.backup_id = ? AND n.name = 'f007.txt'
			UNION ALL
			SELECT p.id, p.parent_id, n.name || '/' || up.path
			FROM up JOIN items p ON p.id = up.parent_id JOIN names n ON n.id = p.name_id
		)
		SELECT path FROM up WHERE parent_id IS NULL`, sc.ID).Scan(&path)
	if err != nil {
		t.Fatal(err)
	}
	if want := root + "/d2/f007.txt"; path != want {
		t.Errorf("rebuilt path = %q, want %q", path, want)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM items WHERE backup_id = ?`, sc.ID).Scan(&count)
	if count != 206 {
		t.Errorf("items = %d, want 206", count)
	}
}

func TestOnlyOneBackupAndPauseResume(t *testing.T) {
	ctx := context.Background()
	m, root, _ := setup(t)
	first, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pause(first.ID); err != nil && !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if cur, ok := m.Current(); ok && cur.Status != store.StatusCompleted {
		waitStatus(t, m, first.ID, store.StatusPaused)
		if _, err := m.Start(ctx, []string{root}); !errors.Is(err, ErrBusy) {
			t.Errorf("second backup while paused: err = %v, want ErrBusy", err)
		}
		if _, err := m.Delete(ctx, first.ID); !errors.Is(err, ErrBusy) {
			t.Errorf("delete while paused: err = %v, want ErrBusy", err)
		}
		if _, err := m.Pause(first.ID); !errors.Is(err, ErrInvalidState) {
			t.Errorf("pause while paused: err = %v", err)
		}
		if _, err := m.Resume(first.ID); err != nil {
			t.Fatal(err)
		}
	}
	waitDone(t, m)
	sc, _ := m.Get(ctx, first.ID)
	if sc.Status != store.StatusCompleted || sc.Progress.FilesDone != 200 {
		t.Errorf("after resume: %+v", sc)
	}

	// The slot is free again.
	second, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, m)
	if _, err := m.Resume(second.ID); !errors.Is(err, ErrNotActive) {
		t.Errorf("resume finished backup: err = %v", err)
	}
}

func TestStop(t *testing.T) {
	ctx := context.Background()
	m, root, _ := setup(t)
	started, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop(started.ID); err != nil && !errors.Is(err, ErrNotActive) {
		t.Fatal(err)
	}
	waitDone(t, m)
	sc, _ := m.Get(ctx, started.ID)
	if sc.Status != store.StatusStopped && sc.Status != store.StatusCompleted {
		t.Errorf("status = %q", sc.Status)
	}
	if _, ok := m.Current(); ok {
		t.Error("stopped backup still active")
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	m, root, _ := setup(t)
	sc, err := m.Start(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, m)

	m.SetGuard(func() error { return ErrBusy }) // e.g. a restore is running
	if _, err := m.Delete(ctx, sc.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("delete while guarded: err = %v, want ErrBusy", err)
	}
	m.SetGuard(nil)

	d, err := m.Delete(ctx, sc.ID)
	if err != nil || d.Items != 206 { // root, 5 folders, 200 files
		t.Errorf("deleted = %+v, %v", d, err)
	}
	if m.Busy() {
		t.Error("still busy after deleting")
	}
	if _, err := m.Get(ctx, sc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}

func TestInvalidRoots(t *testing.T) {
	m, _, _ := setup(t)
	if _, err := m.Start(context.Background(), []string{"relative"}); !errors.Is(err, ErrInvalidRoots) {
		t.Errorf("err = %v", err)
	}
	if _, err := m.Start(context.Background(), nil); !errors.Is(err, ErrInvalidRoots) {
		t.Errorf("err = %v", err)
	}
}

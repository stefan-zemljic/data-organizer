package restorejob

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/backupjob"
	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

const bigFiles = 3000

// bigBackup backs up enough files that a restore can be caught halfway.
func bigBackup(t *testing.T) (st *store.Store, dbPath string, backupID int64, src string) {
	t.Helper()
	ctx := context.Background()
	src = t.TempDir()
	for i := range bigFiles {
		path := filepath.Join(src, fmt.Sprintf("d%d", i%7), fmt.Sprintf("e%d", i%11), fmt.Sprintf("f%04d", i))
		write(t, path, string(bytes.Repeat([]byte{byte(i)}, i)))
	}
	dbPath = filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backups := backupjob.New(st, scan.Options{Workers: 4})
	b, err := backups.Start(ctx, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); backups.Busy() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	return st, dbPath, b.ID, src
}

func slowManager(st *store.Store) *Manager {
	m := New(st, func() bool { return false })
	m.workers = 1
	return m
}

// startAndCatch starts a restore and returns once some files are restored.
func startAndCatch(t *testing.T, m *Manager, backupID int64, target string) int64 {
	t.Helper()
	r, err := m.Start(context.Background(), backupID, target)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cur, ok := m.Current(); !ok || cur.Progress.FilesDone >= 300 {
			break
		}
	}
	return r.ID
}

// waitStatus waits until the restore has the status (in memory or saved).
func waitStatus(t *testing.T, m *Manager, id int64, status string) store.Restore {
	t.Helper()
	var r store.Restore
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		r, _ = m.Get(context.Background(), id)
		if r.Status == status {
			return r
		}
	}
	t.Fatalf("restore %d: %+v, want status %s", id, r, status)
	return r
}

// checkRestored compares the target with the backed-up tree, file by file,
// and the counters with the number of files.
func checkRestored(t *testing.T, r store.Restore, src string) {
	t.Helper()
	p := r.Progress
	var bytesTotal int64
	for i := range bigFiles {
		bytesTotal += int64(i)
	}
	if r.Status != store.RestoreCompleted || p.FilesTotal != bigFiles || p.FilesOK != bigFiles || p.FilesFailed != 0 ||
		p.FilesDone != bigFiles || p.BytesDone != bytesTotal || p.DirsCreated != 1+7+77 {
		t.Errorf("restore = %+v", r)
	}
	dst := rootTarget(r.Target, src)
	files := 0
	filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		rel, _ := filepath.Rel(dst, path)
		want, _ := os.ReadFile(filepath.Join(src, rel))
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, want %d (%v)", rel, len(got), len(want), err)
		}
		return nil
	})
	if files != bigFiles {
		t.Errorf("%d files in the target, want %d", files, bigFiles)
	}
}

func TestPauseResume(t *testing.T) {
	ctx := context.Background()
	st, _, backupID, src := bigBackup(t)
	m := slowManager(st)
	id := startAndCatch(t, m, backupID, filepath.Join(t.TempDir(), "target"))
	if _, err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	r := waitStatus(t, m, id, store.RestorePaused)
	if r.Progress.FilesDone >= bigFiles {
		t.Fatalf("paused restore is complete already: %+v", r)
	}
	if saved, _ := st.GetRestore(ctx, id); saved.Status != store.RestorePaused || saved.Progress != r.Progress {
		t.Errorf("saved %+v, live %+v", saved, r)
	}
	if !m.Busy() {
		t.Error("a paused restore must keep backups from starting")
	}
	if _, err := m.Pause(id); !errors.Is(err, ErrInvalidState) {
		t.Errorf("pause while paused: %v", err)
	}
	roots, _ := st.EntryRoots(ctx, backupID)
	if err := st.SetExcluded(ctx, backupID, []int64{roots[0].ID}, true); !errors.Is(err, store.ErrRestoreUnfinished) {
		t.Errorf("exclusion during a paused restore: %v", err)
	}
	if _, err := m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, waitStatus(t, m, id, store.RestoreCompleted), src)
	if m.Busy() {
		t.Error("still busy after completing")
	}
}

func TestRecoverAfterShutdown(t *testing.T) {
	ctx := context.Background()
	st, _, backupID, src := bigBackup(t)
	m := slowManager(st)
	id := startAndCatch(t, m, backupID, filepath.Join(t.TempDir(), "target"))
	m.Shutdown(ctx)
	if r, _ := st.GetRestore(ctx, id); r.Status != store.RestoreRunning || r.Progress.FilesDone >= bigFiles {
		t.Fatalf("after shutdown: %+v; want an unfinished, running restore", r)
	}

	m2 := slowManager(st)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, waitStatus(t, m2, id, store.RestoreCompleted), src)
}

func TestRecoverAfterCrash(t *testing.T) {
	ctx := context.Background()
	st, dbPath, backupID, src := bigBackup(t)
	m := slowManager(st)
	target := filepath.Join(t.TempDir(), "target")
	id := startAndCatch(t, m, backupID, target)
	m.Shutdown(ctx)

	// A crash loses the last outcomes: files on disk without a record, one
	// of them only partly written.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`DELETE FROM restore_items WHERE restore_id = ? AND item_id % 3 = 0
		AND item_id IN (SELECT id FROM items WHERE kind = 0)`, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("nothing deleted")
	}
	db.Close()
	partial := filepath.Join(rootTarget(target, src), "d0", "e0", "f2310")
	write(t, partial, "x")

	m2 := slowManager(st)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, waitStatus(t, m2, id, store.RestoreCompleted), src)
}

func TestRecoverPausedStaysPaused(t *testing.T) {
	ctx := context.Background()
	st, _, backupID, _ := bigBackup(t)
	m := slowManager(st)
	id := startAndCatch(t, m, backupID, filepath.Join(t.TempDir(), "target"))
	m.Pause(id)
	paused := waitStatus(t, m, id, store.RestorePaused)
	m.Shutdown(ctx)

	m2 := slowManager(st)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cur, ok := m2.Current()
	if !ok || cur.Status != store.RestorePaused || cur.Progress != paused.Progress {
		t.Fatalf("recovered %+v, want paused as %+v", cur, paused.Progress)
	}
	m2.Shutdown(ctx) // leaving while paused keeps it paused
	if r, _ := st.GetRestore(ctx, id); r.Status != store.RestorePaused {
		t.Fatalf("status after second shutdown = %q", r.Status)
	}

	m3 := slowManager(st)
	if err := m3.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := m3.Stop(ctx, id)
	if err != nil || r.Status != store.RestoreStopped || r.FinishedAt == nil {
		t.Fatalf("stop paused: %+v, %v", r, err)
	}
	if m3.Busy() {
		t.Error("busy after stopping a paused restore")
	}
}

package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/scan"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccessKeyIsKept(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	first, err := s.AccessKey(ctx)
	if err != nil || len(first) < 26 {
		t.Fatalf("key %q, err %v", first, err)
	}
	again, err := s.AccessKey(ctx)
	if err != nil || again != first {
		t.Errorf("second call: %q, %v; want %q", again, err, first)
	}
}

func TestBackupLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	roots := []string{`C:\b`, `C:\a`}
	id, err := s.CreateBackup(ctx, roots, 1, time.UnixMilli(1000))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackup(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || !slices.Equal(got.Roots, roots) || got.FinishedAt != nil {
		t.Errorf("new backup = %+v", got)
	}

	p := scan.Progress{DirsFound: 2, DirsDone: 2, FilesFound: 5, FilesDone: 4, BytesDone: 99, Errors: 1}
	if err := s.FinishBackup(ctx, id, StatusCompleted, p, time.UnixMilli(2000), ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBackup(ctx, id)
	if got.Status != StatusCompleted || got.Progress != p || got.FinishedAt == nil || got.FinishedAt.UnixMilli() != 2000 {
		t.Errorf("finished backup = %+v", got)
	}

	if _, err := s.GetBackup(ctx, id+1); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing backup: err = %v", err)
	}
}

func TestRecoverBackup(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	running, _ := s.CreateBackup(ctx, []string{`C:\x`}, 1, time.Now())
	paused, _ := s.CreateBackup(ctx, []string{`C:\y`}, 1, time.Now())
	s.SetStatus(ctx, paused, StatusPaused)
	done, _ := s.CreateBackup(ctx, []string{`C:\z`}, 1, time.Now())
	s.FinishBackup(ctx, done, StatusCompleted, scan.Progress{}, time.Now(), "")

	status := func() map[int64]string {
		t.Helper()
		list, err := s.ListBackups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]string{}
		for _, sc := range list {
			m[sc.ID] = sc.Status
		}
		return m
	}

	// Active backups older than the newest cannot resume (id ranges).
	if _, ok, err := s.RecoverBackup(ctx); err != nil || ok {
		t.Fatalf("recovered an old backup: ok=%v err=%v", ok, err)
	}
	if st := status(); st[running] != StatusInterrupted || st[paused] != StatusInterrupted || st[done] != StatusCompleted {
		t.Errorf("statuses = %v", st)
	}

	// The newest active backup resumes, unless it predates directory records.
	newest, _ := s.CreateBackup(ctx, []string{`C:\w`}, 1, time.Now())
	s.SetStatus(ctx, newest, StatusPaused)
	sc, ok, err := s.RecoverBackup(ctx)
	if err != nil || !ok || sc.ID != newest || sc.Status != StatusPaused {
		t.Fatalf("recovered %+v ok=%v err=%v, want backup %d paused", sc, ok, err, newest)
	}
	s.db.ExecContext(ctx, `UPDATE backups SET resumable = 0 WHERE id = ?`, newest)
	if _, ok, _ := s.RecoverBackup(ctx); ok || status()[newest] != StatusInterrupted {
		t.Error("a backup without directory records must become interrupted")
	}
}

func TestItemWriterSharesNames(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	for range 2 {
		first, err := s.NextItemID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		backupID, _ := s.CreateBackup(ctx, []string{`C:\r`}, first, time.Now())
		w := s.NewItemWriter(backupID, nil)
		items := []scan.Item{
			{ID: first + 1, ParentID: first, Name: "readme.txt", Kind: scan.KindFile, Size: 3, ModTime: 7, Hash: [32]byte{1}},
			{ID: first + 2, ParentID: first, Name: "locked.bin", Kind: scan.KindFile, Err: "access denied"},
			{ID: first, Name: `C:\r`, Kind: scan.KindDir, Hash: [32]byte{2}},
		}
		for _, it := range items {
			if err := w.Write(ctx, it); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}

	var names, items int
	s.db.QueryRow(`SELECT COUNT(*) FROM names`).Scan(&names)
	s.db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&items)
	if names != 3 || items != 6 {
		t.Errorf("names = %d (want 3, shared across backups), items = %d (want 6)", names, items)
	}

	var hashNull, sizeNull bool
	var errText string
	s.db.QueryRow(`SELECT hash IS NULL, size IS NULL, error FROM items i JOIN names n ON n.id = i.name_id
		WHERE n.name = 'locked.bin' LIMIT 1`).Scan(&hashNull, &sizeNull, &errText)
	if !hashNull || !sizeNull || errText != "access denied" {
		t.Errorf("error item: hashNull=%v sizeNull=%v error=%q", hashNull, sizeNull, errText)
	}
	var rootParentNull bool
	s.db.QueryRow(`SELECT parent_id IS NULL FROM items i JOIN names n ON n.id = i.name_id WHERE n.name = 'C:\r' LIMIT 1`).Scan(&rootParentNull)
	if !rootParentNull {
		t.Error("root item must have NULL parent_id")
	}
}

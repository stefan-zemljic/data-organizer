package restorejob

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/backupjob"
	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backedUp backs up a small tree and returns the store, backup id and root.
//
//	src/a/one.txt   "same"     first copy (a < b)
//	src/a/two.txt   "two"      changed after the backup: restored as backed up
//	src/a/gone.txt  "gone"     deleted after the backup: restored as backed up
//	src/b/one.txt   "same"     later copy -> skipped
//	src/b/keep.txt  "keep"
//	src/skip/x.txt  "x"        excluded with folder skip
//	src/empty/                 created even though empty
func backedUp(t *testing.T) (*store.Store, int64, string) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	write(t, filepath.Join(src, "a", "one.txt"), "same")
	write(t, filepath.Join(src, "a", "two.txt"), "two")
	write(t, filepath.Join(src, "a", "gone.txt"), "gone")
	write(t, filepath.Join(src, "b", "one.txt"), "same")
	write(t, filepath.Join(src, "b", "keep.txt"), "keep")
	write(t, filepath.Join(src, "skip", "x.txt"), "x")
	os.MkdirAll(filepath.Join(src, "empty"), 0o755)

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := backupjob.New(st, scan.Options{Workers: 2})
	sc, err := m.Start(ctx, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s, _ := st.GetBackup(ctx, sc.ID); s.Ready {
			break
		}
	}
	return st, sc.ID, src
}

func childByName(t *testing.T, st *store.Store, restoreID, parent int64, name string) store.RestoreEntry {
	t.Helper()
	_, entries, err := st.RestoreEntryChildren(context.Background(), restoreID, parent, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("%s not in restore entries (%+v)", name, entries)
	return store.RestoreEntry{}
}

func TestRestore(t *testing.T) {
	ctx := context.Background()
	st, backupID, src := backedUp(t)

	// Exclude folder "skip"; then change and delete files after the backup.
	roots, _ := st.EntryRoots(ctx, backupID)
	page, _ := st.EntryChildren(ctx, backupID, roots[0].ID, 0, 100)
	for _, e := range page.Entries {
		if e.Name == "skip" {
			if err := st.SetExcluded(ctx, backupID, []int64{e.ID}, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(t, filepath.Join(src, "a", "two.txt"), "changed")
	os.Remove(filepath.Join(src, "a", "gone.txt"))

	target := filepath.Join(t.TempDir(), "target")
	m := New(st, func() bool { return false })
	c, err := m.Start(ctx, backupID, target)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); m.Busy() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	c, _ = m.Get(ctx, c.ID)
	if c.Status != store.RestoreCompleted {
		t.Fatalf("restore = %+v", c)
	}
	p := c.Progress
	if p.FilesTotal != 4 || p.FilesOK != 4 || p.FilesFailed != 0 {
		t.Errorf("progress = %+v", p)
	}

	// Files on disk, as backed up: root mapped below the target (drive as a folder).
	dst := rootTarget(target, src)
	for path, want := range map[string]string{"a/one.txt": "same", "a/two.txt": "two", "a/gone.txt": "gone", "b/keep.txt": "keep"} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", path, got, err)
		}
	}
	for _, path := range []string{"b/one.txt", "skip", "skip/x.txt"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(path))); err == nil {
			t.Errorf("%s must not exist in the restore", path)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty folder not created: %v", err)
	}

	// Entries: statuses and folder markers.
	rr, err := st.RestoreEntryRoots(ctx, c.ID)
	if err != nil || len(rr) != 1 {
		t.Fatalf("roots = %+v, %v", rr, err)
	}
	if rr[0].ErrorsBelow || rr[0].Status != "ok" {
		t.Errorf("root: %+v", rr[0])
	}
	a := childByName(t, st, c.ID, rr[0].ID, "a")
	if e := childByName(t, st, c.ID, a.ID, "two.txt"); e.Status != "ok" {
		t.Errorf("two.txt: %+v", e)
	}
	b := childByName(t, st, c.ID, rr[0].ID, "b")
	if _, entries, _ := st.RestoreEntryChildren(ctx, c.ID, b.ID, 0, 100); len(entries) != 1 {
		t.Errorf("b should list only keep.txt: %+v", entries)
	}

	// A second restore into the now non-empty target is refused.
	if _, err := m.Start(ctx, backupID, target); err == nil {
		t.Error("restore into a non-empty folder was accepted")
	}
}

func TestStartRefusedDuringBackup(t *testing.T) {
	st, backupID, _ := backedUp(t)
	m := New(st, func() bool { return true })
	if _, err := m.Start(context.Background(), backupID, filepath.Join(t.TempDir(), "t")); err != ErrBackupBusy {
		t.Errorf("err = %v, want ErrBackupBusy", err)
	}
}

func TestRootTarget(t *testing.T) {
	for root, want := range map[string]string{
		`C:\`:           `T:\c\C`,
		`C:\Users\x`:    `T:\c\C\Users\x`,
		`\\srv\share\d`: `T:\c\srv_share\d`,
	} {
		if got := rootTarget(`T:\c`, root); got != want && filepath.Separator == '\\' {
			t.Errorf("rootTarget(%q) = %q, want %q", root, got, want)
		}
	}
}

// A damaged content store fails the affected files instead of writing
// wrong data.
func TestRestoreDamagedStore(t *testing.T) {
	ctx := context.Background()
	st, backupID, _ := backedUp(t)
	packs, _ := filepath.Glob(filepath.Join(st.PackDir(), "*.pack"))
	if len(packs) == 0 {
		t.Fatal("no packs")
	}
	for _, p := range packs {
		info, _ := os.Stat(p)
		os.WriteFile(p, make([]byte, info.Size()), 0o644) // same size, zeroed
	}
	target := filepath.Join(t.TempDir(), "target")
	m := New(st, func() bool { return false })
	c, err := m.Start(ctx, backupID, target)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); m.Busy() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	c, _ = m.Get(ctx, c.ID)
	if p := c.Progress; p.FilesOK != 0 || p.FilesFailed != 5 { // nothing excluded here
		t.Errorf("progress = %+v", p)
	}
	rr, _ := st.RestoreEntryRoots(ctx, c.ID)
	if len(rr) != 1 || !rr[0].ErrorsBelow {
		t.Errorf("roots = %+v", rr)
	}
	files := 0
	filepath.WalkDir(target, func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Errorf("%d broken files left in the target", files)
	}
}

// Deleting a backup rewrites packs left mostly unused; what other backups
// still need restores exactly afterwards.
func TestCompactionKeepsSharedContents(t *testing.T) {
	ctx := context.Background()
	big := make([]byte, 3<<20)
	rand.NewChaCha8([32]byte{7}).Read(big) // incompressible, several chunks
	one, two := filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")
	write(t, filepath.Join(one, "big.bin"), string(big))
	write(t, filepath.Join(one, "small.txt"), "shared")
	write(t, filepath.Join(two, "small.txt"), "shared")

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backups := backupjob.New(st, scan.Options{Workers: 2})
	backupOf := func(root string) int64 {
		sc, err := backups.Start(ctx, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); backups.Busy() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		}
		return sc.ID
	}
	first, second := backupOf(one), backupOf(two)

	d, err := st.DeleteBackup(ctx, first)
	if err != nil || d.Contents != 1 || d.BytesFreed < 3<<20 {
		t.Fatalf("deleted = %+v, %v", d, err)
	}
	packs, _ := filepath.Glob(filepath.Join(st.PackDir(), "*.pack"))
	var size int64
	for _, p := range packs {
		info, _ := os.Stat(p)
		size += info.Size()
	}
	if size > 1<<10 {
		t.Errorf("packs hold %d bytes after compaction, want only the small file", size)
	}

	target := filepath.Join(t.TempDir(), "target")
	m := New(st, func() bool { return false })
	c, err := m.Start(ctx, second, target)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); m.Busy() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	if c, _ = m.Get(ctx, c.ID); c.Progress.FilesOK != 1 {
		t.Fatalf("restore = %+v", c)
	}
	if got, err := os.ReadFile(filepath.Join(rootTarget(target, two), "small.txt")); err != nil || string(got) != "shared" {
		t.Errorf("small.txt = %q, %v", got, err)
	}
}

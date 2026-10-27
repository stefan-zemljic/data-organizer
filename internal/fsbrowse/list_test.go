package fsbrowse

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// setupDir creates two directories, two files and (where permitted) a
// symlink to a directory, which must never show up in listings.
func setupDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"beta", "Alpha"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{"b.txt": "hello", "a.txt": ""}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "beta"), filepath.Join(dir, "link")); err != nil {
		t.Logf("symlink not created (%v); symlink exclusion not exercised", err)
	}
	return dir
}

func names(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestListOrderAndFields(t *testing.T) {
	dir := setupDir(t)
	page, err := List(dir, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Alpha", "beta", "a.txt", "b.txt"}
	if got := names(page.Entries); !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if page.Total != 4 || page.NextOffset != 4 {
		t.Errorf("total/nextOffset = %d/%d, want 4/4", page.Total, page.NextOffset)
	}
	hasCreated := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	for _, e := range page.Entries {
		if e.Path != filepath.Join(dir, e.Name) {
			t.Errorf("%s: path = %q", e.Name, e.Path)
		}
		if e.Modified == nil {
			t.Errorf("%s: missing modified time", e.Name)
		}
		if hasCreated && e.Created == nil {
			t.Errorf("%s: missing created time", e.Name)
		}
		switch e.Kind {
		case KindDir:
			if e.Size != nil {
				t.Errorf("%s: directory must not have a size", e.Name)
			}
		case KindFile:
			if e.Size == nil {
				t.Errorf("%s: file must have a size", e.Name)
			}
		}
	}
	if s := page.Entries[3].Size; s == nil || *s != 5 {
		t.Errorf("b.txt size = %v, want 5", s)
	}
}

func TestListPagination(t *testing.T) {
	dir := setupDir(t)
	page, err := List(dir, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(page.Entries), []string{"beta", "a.txt"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if page.NextOffset != 3 || page.Total != 4 {
		t.Errorf("nextOffset/total = %d/%d, want 3/4", page.NextOffset, page.Total)
	}

	page, err = List(dir, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 0 || page.NextOffset != 4 {
		t.Errorf("past the end: %d entries, nextOffset %d", len(page.Entries), page.NextOffset)
	}
	if page.Entries == nil {
		t.Error("entries must be an empty slice, not nil (serializes as [])")
	}
}

func TestListValidation(t *testing.T) {
	dir := setupDir(t)
	cases := []struct {
		name          string
		path          string
		offset, limit int
	}{
		{"limit zero", dir, 0, 0},
		{"limit too large", dir, 0, MaxLimit + 1},
		{"negative offset", dir, -1, 10},
		{"relative path", "relative/dir", 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := List(tc.path, tc.offset, tc.limit); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if _, err := List(dir, 0, MaxLimit); err != nil {
		t.Errorf("limit %d rejected: %v", MaxLimit, err)
	}
}

func TestListRejectsNonDirectories(t *testing.T) {
	dir := setupDir(t)
	if _, err := List(filepath.Join(dir, "a.txt"), 0, 10); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("file: err = %v, want ErrNotDirectory", err)
	}
	if _, err := List(filepath.Join(dir, "missing"), 0, 10); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v, want fs.ErrNotExist", err)
	}
	link := filepath.Join(dir, "link")
	if _, err := os.Lstat(link); err == nil {
		if _, err := List(link, 0, 10); !errors.Is(err, ErrNotDirectory) {
			t.Errorf("symlink: err = %v, want ErrNotDirectory", err)
		}
	}
}

func TestRoots(t *testing.T) {
	roots, err := Roots()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("no roots")
	}
	for _, r := range roots {
		if r.Kind != KindDir || !filepath.IsAbs(r.Path) {
			t.Errorf("bad root %+v", r)
		}
	}
}

// Paging reuses the cached listing, but a change to the folder is seen
// immediately (its modification time changes).
func TestListingCacheSeesChanges(t *testing.T) {
	dir := setupDir(t)
	if p, _ := List(dir, 0, 10); p.Total != 4 {
		t.Fatalf("total = %d", p.Total)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make sure the folder time differs even on coarse clocks.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(dir, future, future)
	if p, _ := List(dir, 0, 10); p.Total != 5 {
		t.Errorf("total after adding a file = %d, want 5", p.Total)
	}
}

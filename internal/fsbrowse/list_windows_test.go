package fsbrowse

import (
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// Junctions need no admin rights (unlike symlinks) and are common in Windows
// user profiles, so they are the realistic case to exclude.
func TestListSkipsJunctions(t *testing.T) {
	dir := setupDir(t)
	junction := filepath.Join(dir, "junction")
	out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, filepath.Join(dir, "beta")).CombinedOutput()
	if err != nil {
		t.Skipf("mklink /J failed: %v: %s", err, out)
	}
	page, err := List(dir, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(page.Entries), "junction") {
		t.Errorf("junction listed: %v", names(page.Entries))
	}
	if page.Total != 4 {
		t.Errorf("total = %d, want 4", page.Total)
	}
	if _, err := List(junction, 0, 10); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("listing a junction: err = %v, want ErrNotDirectory", err)
	}
}

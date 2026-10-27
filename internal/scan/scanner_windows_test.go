package scan

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLockedFileAndJunction(t *testing.T) {
	root := makeTree(t)
	locked := filepath.Join(root, "sub", "locked.bin")
	writeFile(t, locked, "secret")

	// Open without any share mode, like the OS does for pagefile.sys.
	p, err := syscall.UTF16PtrFromString(locked)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)

	junction := filepath.Join(root, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, filepath.Join(root, "sub")).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}

	r := run(t, []string{root}, Options{}, runToEnd)
	var lockedItem *Item
	for i, it := range r.items {
		if it.Name == "junction" {
			t.Error("junction was scanned")
		}
		if it.Name == "locked.bin" {
			lockedItem = &r.items[i]
		}
	}
	if lockedItem == nil || lockedItem.Err == "" {
		t.Fatalf("locked file item = %+v, want an error", lockedItem)
	}
	t.Logf("locked file error: %s", lockedItem.Err)

	// Hashes must equal a tree without the locked file and the junction:
	// the reference hasher reads them as missing, so compare via a copy.
	syscall.CloseHandle(h)
	parentHash := r.byID[lockedItem.ParentID].Hash
	r2 := run(t, []string{filepath.Join(root, "sub")}, Options{Exclude: []string{locked}}, runToEnd)
	for _, it := range r2.items {
		if it.ParentID == 0 && it.Hash != parentHash {
			t.Error("parent hash must not include the unreadable file")
		}
	}
}

package scan

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// refHash is an independent, sequential implementation of the hash format.
func refHash(t *testing.T, path string) [32]byte {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().IsRegular() {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(append([]byte{0}, content...))
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	var acc [32]byte
	for _, e := range entries {
		if !e.Type().IsDir() && !e.Type().IsRegular() {
			continue
		}
		child := refHash(t, filepath.Join(path, e.Name()))
		var buf bytes.Buffer
		binary.Write(&buf, binary.LittleEndian, uint64(len(e.Name())))
		buf.WriteString(e.Name())
		buf.Write(child[:])
		d := sha256.Sum256(buf.Bytes())
		for i := range acc {
			acc[i] ^= d[i]
		}
	}
	return sha256.Sum256(append([]byte{1}, acc[:]...))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeTree builds a small tree with nested, empty and duplicate-content items.
func makeTree(t *testing.T) string {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "sub", "b.bin"), "\x00\x01\x02")
	writeFile(t, filepath.Join(root, "sub", "deep", "c.txt"), "hello")
	writeFile(t, filepath.Join(root, "sub", "deep", "empty.txt"), "")
	if err := os.Mkdir(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

type result struct {
	seq     []Item // everything sent, in order, including directory records
	items   []Item
	byID    map[int64]Item
	scanner *Scanner
}

// run scans roots to completion, pausing first if pauseFirst is set.
func run(t *testing.T, roots []string, opts Options, phases func(s *Scanner)) result {
	t.Helper()
	out := make(chan Item, 16)
	collected := make(chan []Item)
	go func() {
		var items []Item
		for it := range out {
			items = append(items, it)
		}
		collected <- items
	}()
	s := New(roots, 1, out, opts)
	phases(s)
	close(out)
	return newResult(t, <-collected, s)
}

// newResult indexes the items of a scan, leaving out directory records.
func newResult(t *testing.T, seq []Item, s *Scanner) result {
	t.Helper()
	r := result{seq: seq, byID: map[int64]Item{}, scanner: s}
	for _, it := range seq {
		if it.Listed {
			continue
		}
		r.items = append(r.items, it)
		if _, dup := r.byID[it.ID]; dup {
			t.Fatalf("item id %d emitted twice", it.ID)
		}
		r.byID[it.ID] = it
	}
	return r
}

func runToEnd(s *Scanner) {
	s.Start()
	s.Wait()
}

// path rebuilds an item's path from parent ids and names, as the database will.
func (r result) path(it Item) string {
	if it.ParentID == 0 {
		return it.Name
	}
	return filepath.Join(r.path(r.byID[it.ParentID]), it.Name)
}

func (r result) checkAgainstReference(t *testing.T) {
	t.Helper()
	for _, it := range r.items {
		if it.Err != "" {
			continue
		}
		if want := refHash(t, r.path(it)); it.Hash != want {
			t.Errorf("%s: hash mismatch", r.path(it))
		}
	}
}

func TestScanHashesMatchReference(t *testing.T) {
	root := makeTree(t)
	r := run(t, []string{root}, Options{Workers: 4}, runToEnd)
	if !r.scanner.Done() {
		t.Fatal("scan not done")
	}
	if len(r.items) != 8 { // root, a.txt, sub, b.bin, deep, c.txt, empty.txt, emptydir
		t.Fatalf("got %d items", len(r.items))
	}
	r.checkAgainstReference(t)

	want := Progress{DirsFound: 4, DirsDone: 4, FilesFound: 4, FilesDone: 4, BytesFound: 13, BytesDone: 13}
	if p := r.scanner.Progress(); p != want {
		t.Errorf("progress = %+v, want %+v", p, want)
	}

	// Same content, different domains: a file "x" and the hash prefix bytes
	// must not collide, and identical files hash identically.
	hashes := map[string][32]byte{}
	for _, it := range r.items {
		hashes[filepath.Base(r.path(it))] = it.Hash
	}
	if hashes["a.txt"] != hashes["c.txt"] {
		t.Error("identical contents must hash identically")
	}
	if hashes["empty.txt"] == hashes["emptydir"] {
		t.Error("empty file and empty directory must differ")
	}
}

func TestDirHashIgnoresOrderButNotNames(t *testing.T) {
	a := t.TempDir()
	writeFile(t, filepath.Join(a, "x"), "1")
	writeFile(t, filepath.Join(a, "y"), "2")
	b := t.TempDir()
	writeFile(t, filepath.Join(b, "y"), "2")
	writeFile(t, filepath.Join(b, "x"), "1")
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "x"), "2")
	writeFile(t, filepath.Join(c, "y"), "1")

	rootHash := func(dir string) [32]byte {
		r := run(t, []string{dir}, Options{}, runToEnd)
		for _, it := range r.items {
			if it.ParentID == 0 {
				return it.Hash
			}
		}
		t.Fatal("no root item")
		return [32]byte{}
	}
	if rootHash(a) != rootHash(b) {
		t.Error("same entries in different creation order must hash the same")
	}
	if rootHash(a) == rootHash(c) {
		t.Error("swapped names must change the hash")
	}
}

func TestPauseResumeGivesSameResult(t *testing.T) {
	root := t.TempDir()
	var total int64
	for i := range 300 {
		content := fmt.Sprint(i, "-", bytes.Repeat([]byte("x"), i*37))
		total += int64(len(content))
		writeFile(t, filepath.Join(root, fmt.Sprintf("d%d", i%7), fmt.Sprintf("f%03d", i)), content)
	}

	pauses := 0
	r := run(t, []string{root}, Options{Workers: 8}, func(s *Scanner) {
		s.Pause() // workers of the first phase exit before doing anything
		runToEnd(s)
		if s.Done() {
			t.Fatal("done while paused before start")
		}
		for !s.Done() {
			before := s.Progress().FilesDone
			s.Resume()
			s.Start()
			// Let each phase make some progress, then interrupt it mid-run.
			for !s.Done() && s.Progress().FilesDone < before+20 {
				time.Sleep(50 * time.Microsecond)
			}
			s.Pause()
			s.Wait()
			pauses++
		}
	})
	t.Logf("completed after %d pause/resume cycles", pauses)
	r.checkAgainstReference(t)
	p := r.scanner.Progress()
	if p.FilesDone != 300 || p.FilesFound != 300 || p.BytesDone != total {
		t.Errorf("progress = %+v, want 300 files and %d bytes (no double counting)", p, total)
	}
}

func TestStop(t *testing.T) {
	root := makeTree(t)
	r := run(t, []string{root}, Options{}, func(s *Scanner) {
		s.Stop()
		runToEnd(s)
	})
	if r.scanner.Done() || len(r.items) != 0 {
		t.Errorf("stopped scan: done=%v items=%d", r.scanner.Done(), len(r.items))
	}
}

func TestErrorsAreExcluded(t *testing.T) {
	root := makeTree(t)
	missing := filepath.Join(t.TempDir(), "missing")
	r := run(t, []string{root, missing}, Options{}, runToEnd)
	if !r.scanner.Done() {
		t.Fatal("not done")
	}
	var failed []Item
	for _, it := range r.items {
		if it.Err != "" {
			failed = append(failed, it)
		}
	}
	if len(failed) != 1 || failed[0].Name != missing || failed[0].ParentID != 0 {
		t.Fatalf("failed items = %+v", failed)
	}
	if p := r.scanner.Progress(); p.Errors != 1 || p.FilesDone != 4 {
		t.Errorf("progress = %+v", p)
	}
	r.checkAgainstReference(t)
}

func TestExclude(t *testing.T) {
	root := makeTree(t)
	db := filepath.Join(root, "Scan.DB")
	writeFile(t, db, "database")
	r := run(t, []string{root}, Options{Exclude: []string{filepath.Join(root, "scan.db")}}, runToEnd)
	for _, it := range r.items {
		if it.Name == "Scan.DB" {
			t.Fatal("excluded file was scanned")
		}
	}
	// The root hash must equal the hash of the tree without the excluded file.
	os.Remove(db)
	r.checkAgainstReference(t)
}

func TestFileRoot(t *testing.T) {
	root := makeTree(t)
	file := filepath.Join(root, "a.txt")
	r := run(t, []string{file}, Options{}, runToEnd)
	if len(r.items) != 1 || r.items[0].Name != file || r.items[0].Kind != KindFile {
		t.Fatalf("items = %+v", r.items)
	}
	r.checkAgainstReference(t)
}

func TestNormalizeRoots(t *testing.T) {
	sep := string(filepath.Separator)
	base := t.TempDir()
	a := filepath.Join(base, "a")
	got, err := NormalizeRoots([]string{
		a + sep + "x",
		a + " b",
		a,
		a + sep,
		filepath.Join(base, "c"),
		a + sep + "y" + sep + ".." + sep + "z",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{a, a + " b", filepath.Join(base, "c")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := NormalizeRoots([]string{"relative"}); err == nil {
		t.Error("relative path accepted")
	}
	if _, err := NormalizeRoots(nil); err != ErrNoRoots {
		t.Errorf("empty: err = %v", err)
	}
}

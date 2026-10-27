package scan

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// partialOf builds the stored state a database would hold after a crash
// that kept the first items of seq (items are stored in the order sent).
func partialOf(seq []Item) (Partial, int64) {
	var p Partial
	stored := map[int64]bool{}
	children := map[int64][]StoredChild{}
	var maxID int64
	for _, it := range seq {
		maxID = max(maxID, it.ID)
		if it.Listed {
			continue
		}
		stored[it.ID] = true
		if it.ParentID == 0 {
			p.DoneRoots = append(p.DoneRoots, it.Name)
		} else {
			children[it.ParentID] = append(children[it.ParentID], StoredChild{Name: it.Name, Hash: it.Hash, OK: it.Err == ""})
		}
		switch {
		case it.Kind == KindDir:
			p.Stored.DirsFound++
			if it.Err == "" {
				p.Stored.DirsDone++
			}
		default:
			p.Stored.FilesFound++
			if it.Err == "" {
				p.Stored.FilesDone++
				p.Stored.BytesFound += it.Size
				p.Stored.BytesDone += it.Size
			}
		}
		if it.Err != "" {
			p.Stored.Errors++
		}
	}
	for _, it := range seq {
		if it.Listed && !stored[it.ID] {
			p.Dirs = append(p.Dirs, PartialDir{ID: it.ID, ParentID: it.ParentID, Name: it.Name})
		}
	}
	p.Children = func(id int64) ([]StoredChild, error) { return children[id], nil }
	return p, maxID + 1
}

// resumeRun resumes from the first cut items of seq and returns everything
// stored in the end: the kept prefix followed by what the resumed scan sent.
func resumeRun(t *testing.T, roots []string, seq []Item, cut int) result {
	t.Helper()
	p, next := partialOf(seq[:cut])
	out := make(chan Item, 16)
	collected := make(chan []Item)
	go func() {
		var items []Item
		for it := range out {
			items = append(items, it)
		}
		collected <- items
	}()
	s := Resume(roots, p, next, out, Options{Workers: 4})
	runToEnd(s)
	close(out)
	if !s.Done() || s.Err() != nil {
		t.Fatalf("cut %d: resumed scan done=%v err=%v", cut, s.Done(), s.Err())
	}
	all := append(append([]Item{}, seq[:cut]...), <-collected...)
	return newResult(t, all, s)
}

// checkComplete verifies a combined result: every file and directory once,
// with the right hash, and progress counting each file once.
func (r result) checkComplete(t *testing.T, root string, files int, bytes int64) {
	t.Helper()
	r.checkAgainstReference(t)
	paths := map[string]bool{}
	for _, it := range r.items {
		p := r.path(it)
		if paths[p] {
			t.Fatalf("%s stored twice", p)
		}
		paths[p] = true
	}
	n := 0
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		n++
		if !paths[path] {
			t.Errorf("%s missing", path)
		}
		return nil
	})
	if n != len(r.items) {
		t.Errorf("%d items stored, %d on disk", len(r.items), n)
	}
	p := r.scanner.Progress()
	if p.FilesDone != int64(files) || p.FilesFound != int64(files) || p.BytesDone != bytes || p.BytesFound != bytes ||
		p.DirsDone != p.DirsFound || p.Errors != 0 {
		t.Errorf("progress = %+v, want %d files, %d bytes", p, files, bytes)
	}
}

func TestResumeAfterCrashAtAnyPoint(t *testing.T) {
	root := t.TempDir()
	var total int64
	const files = 120
	for i := range files {
		content := fmt.Sprint(i, "-", i*i)
		total += int64(len(content))
		writeFile(t, filepath.Join(root, fmt.Sprintf("d%d", i%4), fmt.Sprintf("e%d", i%3), fmt.Sprintf("f%03d", i)), content)
	}
	os.MkdirAll(filepath.Join(root, "empty", "deeper"), 0o755)
	roots := []string{root}

	full := run(t, roots, Options{Workers: 4}, runToEnd)
	full.checkComplete(t, root, files, total)

	rng := rand.New(rand.NewPCG(1, 2))
	cuts := []int{0, 1, 2, len(full.seq) / 2, len(full.seq) - 1, len(full.seq)}
	for range 20 {
		cuts = append(cuts, rng.IntN(len(full.seq)+1))
	}
	for _, cut := range cuts {
		r := resumeRun(t, roots, full.seq, cut)
		r.checkComplete(t, root, files, total)

		// A crash while resuming: resume again from a cut of that.
		again := resumeRun(t, roots, r.seq, cut+rng.IntN(len(r.seq)-cut+1))
		again.checkComplete(t, root, files, total)
	}
}

func TestResumeSeesChangesSinceTheCrash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep", "a"), "a")
	writeFile(t, filepath.Join(root, "gone", "b"), "b")
	writeFile(t, filepath.Join(root, "c"), "c")
	roots := []string{root}
	full := run(t, roots, Options{Workers: 1}, runToEnd)

	// Crash after discovery, before anything was hashed.
	cut := 0
	for cut < len(full.seq) && full.seq[cut].Listed {
		cut++
	}
	os.RemoveAll(filepath.Join(root, "gone"))
	writeFile(t, filepath.Join(root, "new", "d"), "d")

	r := resumeRun(t, roots, full.seq, cut)
	var errs []string
	for _, it := range r.items {
		if it.Err != "" {
			errs = append(errs, r.path(it))
		}
	}
	// The vanished folder was listed before: it is reported as unreadable
	// and left out; the new one is scanned.
	if len(errs) != 1 || errs[0] != filepath.Join(root, "gone") {
		t.Errorf("errors at %v, want only the vanished folder", errs)
	}
	for _, it := range r.items {
		if it.ParentID == 0 && it.Hash != refHash(t, root) {
			t.Error("root hash does not match the tree as it is now")
		}
	}
}

func TestResumeOfFinishedScan(t *testing.T) {
	root := makeTree(t)
	full := run(t, []string{root}, Options{}, runToEnd)
	// Crashed while finding duplicates: everything is stored.
	r := resumeRun(t, []string{root}, full.seq, len(full.seq))
	if len(r.items) != len(full.items) {
		t.Errorf("resuming a finished scan added %d items", len(r.items)-len(full.items))
	}
}

package scan

import "path/filepath"

// Partial is the stored state of an unfinished scan, read back to resume
// it after a restart or crash. Items are complete before they are sent
// and stored in order, so any stored prefix is consistent: a stored
// directory has all its children stored, and every stored item's parent is
// stored or listed (Item.Listed).
type Partial struct {
	// Dirs are the listed directories without a stored item: the ones not
	// finished yet.
	Dirs []PartialDir
	// DoneRoots are the roots with a stored item (finished or unreadable).
	DoneRoots []string
	// Stored counts the stored items: Done counters and Errors, with the
	// Found counters set to what those items account for.
	Stored Progress
	// Children returns the stored items directly inside a directory. It is
	// called from many goroutines.
	Children func(dirID int64) ([]StoredChild, error)
}

type PartialDir struct {
	ID       int64
	ParentID int64  // 0 for roots
	Name     string // the full path for roots
}

// StoredChild is a stored item inside an unfinished directory; OK is false
// for unreadable items (left out of the directory's hash).
type StoredChild struct {
	Name string
	Hash [32]byte
	OK   bool
}

// Resume continues an unfinished scan of roots from its stored state.
// Unfinished directories keep their ids and are listed again; only what
// is not stored yet is read. New items get ids from nextID on, which must
// be above every id the scan used. Like New, it may send on out at once.
func Resume(roots []string, p Partial, nextID int64, out chan<- Item, opts Options) *Scanner {
	s := newScanner(nextID, out, opts)
	s.children = p.Children
	s.dirsFound.Store(p.Stored.DirsFound)
	s.dirsDone.Store(p.Stored.DirsDone)
	s.filesFound.Store(p.Stored.FilesFound)
	s.filesDone.Store(p.Stored.FilesDone)
	s.bytesFound.Store(p.Stored.BytesFound)
	s.bytesDone.Store(p.Stored.BytesDone)
	s.errorCount.Store(p.Stored.Errors)

	dirs := make(map[int64]PartialDir, len(p.Dirs))
	for _, d := range p.Dirs {
		dirs[d.ID] = d
	}
	isRoot := make(map[string]bool, len(roots))
	for _, r := range roots {
		isRoot[r] = true
	}

	// Build the nodes parents first. A directory whose chain does not lead
	// to a root cannot occur (a parent is stored only after its children);
	// such a one is skipped, as nothing could finish it.
	nodes := make(map[int64]*dirNode, len(p.Dirs))
	var node func(id int64) *dirNode
	node = func(id int64) *dirNode {
		if n, ok := nodes[id]; ok {
			return n
		}
		d, ok := dirs[id]
		if !ok {
			nodes[id] = nil
			return nil
		}
		n := &dirNode{id: d.ID, name: d.Name, pending: 1, resumed: true}
		if d.ParentID == 0 {
			if !isRoot[d.Name] {
				n = nil
			} else {
				n.path = d.Name
			}
		} else if parent := node(d.ParentID); parent == nil {
			n = nil
		} else {
			n.parent, n.path = parent, filepath.Join(parent.path, d.Name)
			parent.pending++
			parent.known = append(parent.known, d.Name)
		}
		nodes[id] = n
		return n
	}
	resumedRoots := map[string]bool{}
	for _, d := range p.Dirs {
		if n := node(d.ID); n != nil {
			s.dirQ = append(s.dirQ, n)
			s.dirsFound.Add(1)
			if n.parent == nil {
				resumedRoots[n.name] = true
			}
		}
	}

	done := make(map[string]bool, len(p.DoneRoots))
	for _, r := range p.DoneRoots {
		done[r] = true
	}
	var fresh []string
	for _, r := range roots {
		if !done[r] {
			s.rootsLeft++
			if !resumedRoots[r] {
				fresh = append(fresh, r) // never listed: start it anew
			}
		}
	}
	for _, r := range fresh {
		s.addRoot(r)
	}
	s.discovered = len(s.dirQ) == 0
	s.done = s.rootsLeft == 0
	return s
}

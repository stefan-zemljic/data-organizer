package scan

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

type Kind uint8

const (
	KindFile Kind = 0
	KindDir  Kind = 1
)

// Item is one scanned file or directory, emitted once it is complete
// (directories only after all their children). Root items have ParentID 0
// and carry their full path as Name. Hash is valid only when Err is empty;
// items with an error are excluded from their parent's hash.
//
// Items with Listed set are not results but records of directories just
// found (ID, ParentID, Name only), sent before anything inside them: a
// resumed scan (see Resume) needs them to give unfinished directories
// their ids back. The same directory is sent again when complete.
type Item struct {
	ID       int64
	ParentID int64
	Name     string
	Kind     Kind
	Size     int64 // files only
	ModTime  int64 // unix nanoseconds
	Hash     [32]byte
	Err      string
	Listed   bool
}

// Progress counts what a scan has found and finished. Items that failed
// are counted in Errors, not in the Done counters or BytesDone.
type Progress struct {
	// Discovering is true until every directory has been listed; until
	// then the Found counters still grow.
	Discovering bool  `json:"discovering"`
	DirsFound   int64 `json:"dirsFound"`
	DirsDone    int64 `json:"dirsDone"`
	FilesFound  int64 `json:"filesFound"`
	FilesDone   int64 `json:"filesDone"`
	BytesFound  int64 `json:"bytesFound"` // listed size of the files found
	BytesDone   int64 `json:"bytesDone"`  // bytes hashed
	Errors      int64 `json:"errors"`
}

var ErrNoRoots = errors.New("no paths to scan")

// NormalizeRoots cleans the given absolute paths, removes duplicates and
// drops paths that lie inside another given path (they are scanned anyway).
func NormalizeRoots(paths []string) ([]string, error) {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("path must be absolute: %q", p)
		}
		cleaned = append(cleaned, filepath.Clean(p))
	}
	slices.Sort(cleaned)
	cleaned = slices.Compact(cleaned)

	// Sorted order puts parents before descendants, but not always directly
	// ("C:\a b" sorts between "C:\a" and "C:\a\x"), so check every kept root.
	var roots []string
	for _, p := range cleaned {
		if !slices.ContainsFunc(roots, func(r string) bool { return within(p, r) }) {
			roots = append(roots, p)
		}
	}
	if len(roots) == 0 {
		return nil, ErrNoRoots
	}
	return roots, nil
}

// within reports whether path lies inside dir (dir itself excluded).
func within(path, dir string) bool {
	sep := string(filepath.Separator)
	if !strings.HasSuffix(dir, sep) {
		dir += sep
	}
	return len(path) > len(dir) && strings.HasPrefix(path, dir)
}

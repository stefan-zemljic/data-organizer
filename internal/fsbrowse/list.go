// Package fsbrowse lists directory contents page by page for the web UI.
// Only regular files and real directories are reported; symlinks, junctions,
// devices, pipes, sockets and other special entries are skipped.
package fsbrowse

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxLimit is the largest page size a client may request.
const MaxLimit = 1000

var (
	// ErrInvalidRequest marks errors caused by bad client parameters.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrNotDirectory is returned when the requested path is not a real directory.
	ErrNotDirectory = errors.New("not a directory")
)

type Kind string

const (
	KindDir  Kind = "dir"
	KindFile Kind = "file"
)

// Entry describes one file or directory. Size is set for files only;
// Created is omitted where the platform does not expose a birth time.
type Entry struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Kind     Kind       `json:"kind"`
	Size     *int64     `json:"size,omitempty"`
	Created  *time.Time `json:"created,omitempty"`
	Modified *time.Time `json:"modified,omitempty"`
}

// Page is one slice of a directory listing. Total counts all eligible
// entries in the directory; NextOffset is the offset for the following page
// and equals Total when there is nothing more to load.
type Page struct {
	Path       string  `json:"path"`
	Offset     int     `json:"offset"`
	Limit      int     `json:"limit"`
	Total      int     `json:"total"`
	NextOffset int     `json:"nextOffset"`
	Entries    []Entry `json:"entries"`
}

// List returns up to limit entries of dir starting at offset. Entries are
// ordered directories first, then case-insensitively by name, so offsets
// stay stable between requests as long as the directory does not change.
func List(dir string, offset, limit int) (Page, error) {
	if limit < 1 || limit > MaxLimit {
		return Page{}, fmt.Errorf("%w: limit must be between 1 and %d, got %d", ErrInvalidRequest, MaxLimit, limit)
	}
	if offset < 0 {
		return Page{}, fmt.Errorf("%w: offset must not be negative, got %d", ErrInvalidRequest, offset)
	}
	if !filepath.IsAbs(dir) {
		return Page{}, fmt.Errorf("%w: path must be absolute, got %q", ErrInvalidRequest, dir)
	}
	dir = filepath.Clean(dir)

	info, err := os.Lstat(dir)
	if err != nil {
		return Page{}, err
	}
	if !info.IsDir() {
		return Page{}, fmt.Errorf("%w: %s", ErrNotDirectory, dir)
	}

	eligible, err := sortedEntries(dir, info.ModTime())
	if err != nil {
		return Page{}, err
	}

	total := len(eligible)
	start := min(offset, total)
	end := min(start+limit, total)
	page := Page{
		Path:       dir,
		Offset:     offset,
		Limit:      limit,
		Total:      total,
		NextOffset: end,
		Entries:    make([]Entry, 0, end-start),
	}
	for _, de := range eligible[start:end] {
		info, err := de.Info()
		if err != nil {
			// Removed since ReadDir; skip it rather than failing the page.
			continue
		}
		if !isEligible(info.Mode().Type()) {
			continue
		}
		page.Entries = append(page.Entries, newEntry(dir, info))
	}
	return page, nil
}

func isEligible(t fs.FileMode) bool {
	return t.IsDir() || t.IsRegular()
}

func compareEntries(a, b fs.DirEntry) int {
	if a.IsDir() != b.IsDir() {
		if a.IsDir() {
			return -1
		}
		return 1
	}
	if c := strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name())); c != 0 {
		return c
	}
	return strings.Compare(a.Name(), b.Name())
}

func newEntry(dir string, info fs.FileInfo) Entry {
	modified := info.ModTime()
	e := Entry{
		Name:     info.Name(),
		Path:     filepath.Join(dir, info.Name()),
		Kind:     KindDir,
		Modified: &modified,
	}
	if created, ok := creationTime(info); ok {
		e.Created = &created
	}
	if info.Mode().IsRegular() {
		size := info.Size()
		e.Kind = KindFile
		e.Size = &size
	}
	return e
}

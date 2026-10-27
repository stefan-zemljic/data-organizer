package content

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"slices"
)

// CompactRatio: a pack whose live bytes are below this share of its size
// is rewritten. Rewriting a pack costs at most as much as it frees.
const CompactRatio = 0.5

// Rewrite copies the stored bytes of chunks into new packs and returns the
// chunks with their new places, synced to disk. The caller saves the new
// places and only then removes the old packs (RemovePacks): until then
// the new packs are unreferenced and a crash merely leaves garbage.
func Rewrite(dir string, chunks []Chunk) ([]Chunk, error) {
	next, err := nextPackID(dir)
	if err != nil {
		return nil, err
	}
	moved := slices.Clone(chunks)
	slices.SortFunc(moved, func(a, b Chunk) int { // sequential reads
		return cmp.Or(cmp.Compare(a.Pack, b.Pack), cmp.Compare(a.Offset, b.Offset))
	})
	out := packFile{dir: dir, next: next}
	defer out.close()
	var (
		in     *os.File
		inPack int64
		buf    []byte
	)
	defer func() {
		if in != nil {
			in.Close()
		}
	}()
	for i := range moved {
		c := &moved[i]
		if in == nil || inPack != c.Pack {
			if in != nil {
				in.Close()
			}
			if in, err = os.Open(packPath(dir, c.Pack)); err != nil {
				return nil, err
			}
			inPack = c.Pack
		}
		buf = grow(buf, int(c.Stored))
		if _, err := in.ReadAt(buf, c.Offset); err != nil {
			return nil, err
		}
		if c.Pack, c.Offset, err = out.append(buf); err != nil {
			return nil, err
		}
	}
	if err := out.close(); err != nil {
		return nil, err
	}
	return moved, nil
}

// RemovePacks deletes the given pack files (missing ones are fine).
func RemovePacks(dir string, ids []int64) error {
	var errs []error
	for _, id := range ids {
		if err := os.Remove(packPath(dir, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

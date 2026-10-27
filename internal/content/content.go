// Package content stores file contents once for all backups: files are split
// into content-defined chunks (about 1 MiB, so a changed large file shares
// most chunks with its earlier version), every distinct chunk is stored
// once, zstd-compressed when that helps, appended to pack files in one
// folder. The index (which chunk is where, which chunks make a file) lives
// in the database; this package only handles the bytes.
//
// Pack files are only appended to; the index is saved after the packs are
// synced, so a crash can leave unindexed bytes, never index rows without
// data. Unindexed bytes and chunks no backup needs anymore are removed by
// compaction (compact.go).
package content

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	CodecRaw  = 0
	CodecZstd = 1

	packTarget = 64 << 20 // a pack is closed once it reaches this size
	packSuffix = ".pack"
)

// hashPrefix is the file hash domain byte (see scan/hash.go): a chunk's
// hash uses the file hash formula, so a file of one chunk has the chunk's
// hash.
const hashPrefix = 0x00

// Chunk is one stored chunk and where its bytes are.
type Chunk struct {
	ID     int64
	Hash   [32]byte
	Pack   int64
	Offset int64
	Stored int64 // bytes in the pack
	Size   int64 // bytes of data
	Codec  uint8
}

// File is a newly stored file content: its hash, size and chunk ids.
type File struct {
	Hash   [32]byte
	Size   int64
	Chunks []int64
}

// Index is what a Writer needs to know about the stored contents.
type Index struct {
	Chunks    map[[32]byte]int64 // chunk hash -> id
	Files     map[[32]byte]struct{}
	NextChunk int64
}

var ErrDamaged = errors.New("stored content is damaged")

func chunkHash(data []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{hashPrefix})
	h.Write(data)
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// EncodeIDs encodes chunk ids as varint deltas (consecutive ids take one
// byte each).
func EncodeIDs(ids []int64) []byte {
	buf := make([]byte, 0, len(ids)+8)
	var prev int64
	for _, id := range ids {
		buf = binary.AppendVarint(buf, id-prev)
		prev = id
	}
	return buf
}

func DecodeIDs(buf []byte) ([]int64, error) {
	var ids []int64
	var prev int64
	for len(buf) > 0 {
		d, n := binary.Varint(buf)
		if n <= 0 {
			return nil, fmt.Errorf("%w: bad chunk list", ErrDamaged)
		}
		prev += d
		ids = append(ids, prev)
		buf = buf[n:]
	}
	return ids, nil
}

func packPath(dir string, id int64) string {
	return filepath.Join(dir, fmt.Sprintf("%08d%s", id, packSuffix))
}

// Packs lists the pack files in dir with their sizes.
func Packs(dir string) (map[int64]int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	packs := make(map[int64]int64)
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), packSuffix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		id, err := strconv.ParseInt(name, 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		packs[id] = info.Size()
	}
	return packs, nil
}

func nextPackID(dir string) (int64, error) {
	packs, err := Packs(dir)
	if err != nil {
		return 0, err
	}
	next := int64(1)
	for id := range packs {
		next = max(next, id+1)
	}
	return next, nil
}

// packFile appends to numbered pack files, starting a new one when the
// current one is full. Not safe for concurrent use.
type packFile struct {
	dir  string
	next int64
	id   int64
	f    *os.File
	size int64
}

// append writes data and returns where it went.
func (p *packFile) append(data []byte) (pack, offset int64, err error) {
	if p.f != nil && p.size >= packTarget {
		if err := p.close(); err != nil {
			return 0, 0, err
		}
	}
	if p.f == nil {
		f, err := os.OpenFile(packPath(p.dir, p.next), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return 0, 0, err
		}
		p.f, p.id, p.size = f, p.next, 0
		p.next++
	}
	if _, err := p.f.Write(data); err != nil {
		return 0, 0, err
	}
	offset = p.size
	p.size += int64(len(data))
	return p.id, offset, nil
}

func (p *packFile) sync() error {
	if p.f == nil {
		return nil
	}
	return p.f.Sync()
}

func (p *packFile) close() error {
	if p.f == nil {
		return nil
	}
	err := p.f.Sync()
	if cerr := p.f.Close(); err == nil {
		err = cerr
	}
	p.f = nil
	return err
}

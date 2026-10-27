package content

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/restic/chunker"
)

// pol is the chunking polynomial. It must never change: chunks cut with
// another one would not match the stored ones.
const pol = chunker.Pol(0x3DA3358B4DC173)

const (
	// Chunks of at least probeMin bytes are first compressed on a probe
	// of probeSize bytes; data that barely shrinks there (media, archives)
	// is stored raw without spending time on the rest.
	probeMin   = 128 << 10
	probeSize  = 64 << 10
	probeLimit = probeSize * 97 / 100 // compressed probe bytes below which compression is used
)

// Writer stores file contents during a backup and collects the index rows
// to save. Safe for concurrent use.
type Writer struct {
	enc   *zstd.Encoder
	state sync.Pool // *fileState

	mu       sync.Mutex
	err      error // first pack write error; every later Commit returns it
	index    Index
	pack     packFile
	newChunk []Chunk
	newFiles []File
}

type fileState struct {
	ch   *chunker.Chunker
	data []byte // chunk buffer, grows up to chunker.MaxSize
	out  []byte // compression buffer
}

// NewWriter appends to new pack files in dir (created if needed); index
// holds what is stored already and is owned by the writer from now on.
func NewWriter(dir string, index Index) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	next, err := nextPackID(dir)
	if err != nil {
		return nil, err
	}
	// Fastest level: scanning should stay close to disk speed. No frame
	// checksum: restores verify the file hash.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		return nil, err
	}
	if index.Chunks == nil {
		index.Chunks = make(map[[32]byte]int64)
	}
	if index.Files == nil {
		index.Files = make(map[[32]byte]struct{})
	}
	return &Writer{enc: enc, index: index, pack: packFile{dir: dir, next: next}}, nil
}

// StoreFile reads r to the end, stores its chunks and returns the file
// hash (sha256(0x00 ‖ content)) and size. progress gets the bytes of each
// chunk read. When stop returns true it gives up early with stopped set;
// chunks stored so far stay (they are reused when the file is read again).
func (w *Writer) StoreFile(r io.Reader, stop func() bool, progress func(n int)) (hash [32]byte, size int64, stopped bool, err error) {
	st, _ := w.state.Get().(*fileState)
	if st == nil {
		st = &fileState{ch: chunker.New(r, pol)}
	} else {
		st.ch.Reset(r, pol)
	}
	defer w.state.Put(st)

	h := sha256.New()
	h.Write([]byte{hashPrefix})
	var ids []int64
	single := false
	for {
		if stop() {
			return hash, 0, true, nil
		}
		c, err := st.ch.Next(st.data)
		if err == io.EOF {
			break
		}
		if err != nil {
			return hash, 0, false, err
		}
		st.data = c.Data[:0]
		ch := chunkHash(c.Data)
		// A first chunk shorter than the minimum is the whole file, and its
		// hash is the file hash: no second pass over the data.
		if single = len(ids) == 0 && len(c.Data) < chunker.MinSize; !single {
			h.Write(c.Data)
		}
		id, err := w.putChunk(ch, c.Data, st)
		if err != nil {
			return hash, 0, false, err
		}
		ids = append(ids, id)
		size += int64(len(c.Data))
		progress(len(c.Data))
		if single {
			hash = ch
		}
	}
	if !single {
		h.Sum(hash[:0])
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.index.Files[hash]; !ok {
		w.index.Files[hash] = struct{}{}
		w.newFiles = append(w.newFiles, File{Hash: hash, Size: size, Chunks: ids})
	}
	return hash, size, false, nil
}

// putChunk returns the id of the chunk with hash h, storing data first if
// it is new.
func (w *Writer) putChunk(h [32]byte, data []byte, st *fileState) (int64, error) {
	w.mu.Lock()
	id, ok := w.index.Chunks[h]
	w.mu.Unlock()
	if ok {
		return id, nil
	}

	// Compress outside the lock. Two workers may compress the same new
	// chunk at once; the second one finds it below and drops its copy.
	stored, codec := data, uint8(CodecRaw)
	if len(data) < probeMin || len(w.enc.EncodeAll(data[:probeSize], st.out[:0])) < probeLimit {
		st.out = w.enc.EncodeAll(data, st.out[:0])
		if len(st.out) < len(data) {
			stored, codec = st.out, CodecZstd
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if id, ok := w.index.Chunks[h]; ok {
		return id, nil
	}
	if w.err != nil {
		return 0, w.err
	}
	pack, offset, err := w.pack.append(stored)
	if err != nil {
		w.err = err
		return 0, err
	}
	id = w.index.NextChunk
	w.index.NextChunk++
	w.index.Chunks[h] = id
	w.newChunk = append(w.newChunk, Chunk{ID: id, Hash: h, Pack: pack, Offset: offset,
		Stored: int64(len(stored)), Size: int64(len(data)), Codec: codec})
	return id, nil
}

// Commit makes the stored bytes durable and returns the chunks and files
// stored since the last Commit; save them only after it returns nil.
func (w *Writer) Commit() ([]Chunk, []File, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, nil, w.err
	}
	if err := w.pack.sync(); err != nil {
		w.err = err
		return nil, nil, err
	}
	chunks, files := w.newChunk, w.newFiles
	w.newChunk, w.newFiles = nil, nil
	return chunks, files, nil
}

// Close closes the current pack; call Commit first to save what is left.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enc.Close()
	err := w.pack.close()
	if err == nil && (len(w.newChunk) > 0 || len(w.newFiles) > 0) {
		err = errors.New("content writer closed with unsaved index rows")
	}
	return err
}

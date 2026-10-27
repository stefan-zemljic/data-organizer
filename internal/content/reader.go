package content

import (
	"fmt"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Reader reads stored chunks. Safe for concurrent use.
type Reader struct {
	dir string
	dec *zstd.Decoder

	mu    sync.Mutex
	packs map[int64]*os.File
}

func NewReader(dir string) (*Reader, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.IgnoreChecksum(true))
	if err != nil {
		return nil, err
	}
	return &Reader{dir: dir, dec: dec, packs: make(map[int64]*os.File)}, nil
}

// Chunk returns the data of c, using buf (and raw for the stored bytes) as
// buffers to reuse; the result may alias either.
func (r *Reader) Chunk(c Chunk, buf, raw []byte) (data, rawOut []byte, err error) {
	f, err := r.pack(c.Pack)
	if err != nil {
		return nil, raw, err
	}
	raw = grow(raw, int(c.Stored))
	if _, err := f.ReadAt(raw, c.Offset); err != nil {
		return nil, raw, fmt.Errorf("%w: pack %d: %v", ErrDamaged, c.Pack, err)
	}
	switch c.Codec {
	case CodecRaw:
		data = raw
	case CodecZstd:
		if data, err = r.dec.DecodeAll(raw, buf[:0]); err != nil {
			return nil, raw, fmt.Errorf("%w: %v", ErrDamaged, err)
		}
	default:
		return nil, raw, fmt.Errorf("%w: unknown codec %d", ErrDamaged, c.Codec)
	}
	if int64(len(data)) != c.Size {
		return nil, raw, fmt.Errorf("%w: chunk %d has the wrong size", ErrDamaged, c.ID)
	}
	return data, raw, nil
}

func (r *Reader) pack(id int64) (*os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.packs[id]; ok {
		return f, nil
	}
	f, err := os.Open(packPath(r.dir, id))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	r.packs[id] = f
	return f, nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, f := range r.packs {
		f.Close()
		delete(r.packs, id)
	}
	r.dec.Close()
	return nil
}

func grow(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

package content

import (
	"bytes"
	"crypto/sha256"
	"math/rand/v2"
	"strings"
	"testing"
)

func random(n int, seed uint64) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{byte(seed)}).Read(b)
	return b
}

func fileHash(data []byte) [32]byte {
	return sha256.Sum256(append([]byte{hashPrefix}, data...))
}

func store(t *testing.T, w *Writer, data []byte) [32]byte {
	t.Helper()
	read := 0
	h, size, stopped, err := w.StoreFile(bytes.NewReader(data), func() bool { return false }, func(n int) { read += n })
	if err != nil || stopped {
		t.Fatalf("StoreFile: %v, stopped %v", err, stopped)
	}
	if h != fileHash(data) || size != int64(len(data)) || read != len(data) {
		t.Fatalf("hash/size/progress wrong for %d bytes (size %d, read %d)", len(data), size, read)
	}
	return h
}

// restore reads a file back from the committed chunks.
func restore(t *testing.T, r *Reader, chunks map[int64]Chunk, f File) []byte {
	t.Helper()
	var out, data, raw []byte
	for _, id := range f.Chunks {
		var err error
		data, raw, err = r.Chunk(chunks[id], data, raw)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data...)
	}
	return out
}

func TestStoreAndRead(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, Index{NextChunk: 1})
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{
		"empty":        {},
		"small":        []byte("hello"),
		"text 3 MiB":   []byte(strings.Repeat("some compressible text, ", 3<<20/24)),
		"random 3 MiB": random(3<<20, 1),
		"random 20MiB": random(20<<20, 2),
	}
	for _, data := range inputs {
		store(t, w, data)
	}
	chunks, files, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(inputs) {
		t.Fatalf("%d files, want %d", len(files), len(inputs))
	}
	byID := map[int64]Chunk{}
	var stored, size int64
	codecs := map[uint8]int{}
	for _, c := range chunks {
		byID[c.ID] = c
		stored += c.Stored
		size += c.Size
		codecs[c.Codec]++
	}
	if codecs[CodecRaw] == 0 || codecs[CodecZstd] == 0 {
		t.Errorf("codecs = %v: random data must be raw, text compressed", codecs)
	}
	if stored > size-3<<20+1<<20 {
		t.Errorf("stored %d of %d bytes: text not compressed", stored, size)
	}

	r, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for name, data := range inputs {
		var f File
		for _, ff := range files {
			if ff.Hash == fileHash(data) {
				f = ff
			}
		}
		if got := restore(t, r, byID, f); !bytes.Equal(got, data) {
			t.Errorf("%s: restored %d bytes differ", name, len(got))
		}
	}

	// Storing again adds nothing; a shifted copy shares most chunks.
	store(t, w, inputs["random 20MiB"])
	store(t, w, inputs["small"])
	shifted := append([]byte("0123456789"), inputs["random 20MiB"]...)
	store(t, w, shifted)
	more, files2, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if len(files2) != 1 || len(files2[0].Chunks) < 5 || len(more) > 2 {
		t.Errorf("shifted file: %d new chunks of %d, %d new files", len(more), len(files2[0].Chunks), len(files2))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreStopped(t *testing.T) {
	w, _ := NewWriter(t.TempDir(), Index{NextChunk: 1})
	defer w.Close()
	_, _, stopped, err := w.StoreFile(bytes.NewReader(random(3<<20, 3)), func() bool { return true }, func(int) {})
	if !stopped || err != nil {
		t.Errorf("stopped = %v, err = %v", stopped, err)
	}
	w.Commit()
}

func TestRewrite(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, Index{NextChunk: 1})
	a, b := random(2<<20, 4), []byte("keep me")
	store(t, w, a)
	store(t, w, b)
	chunks, files, _ := w.Commit()
	w.Close()

	// Keep only b: rewrite its chunk, then remove the old pack.
	var keep []Chunk
	var fb File
	for _, f := range files {
		if f.Hash == fileHash(b) {
			fb = f
		}
	}
	for _, c := range chunks {
		if c.ID == fb.Chunks[0] {
			keep = append(keep, c)
		}
	}
	old, _ := Packs(dir)
	moved, err := Rewrite(dir, keep)
	if err != nil || len(moved) != 1 || moved[0].Pack == keep[0].Pack {
		t.Fatalf("moved = %+v, %v", moved, err)
	}
	var oldIDs []int64
	for id := range old {
		oldIDs = append(oldIDs, id)
	}
	if err := RemovePacks(dir, oldIDs); err != nil {
		t.Fatal(err)
	}
	if packs, _ := Packs(dir); len(packs) != 1 || packs[moved[0].Pack] != moved[0].Stored {
		t.Errorf("packs after compaction: %v", packs)
	}
	r, _ := NewReader(dir)
	defer r.Close()
	if got := restore(t, r, map[int64]Chunk{moved[0].ID: moved[0]}, fb); !bytes.Equal(got, b) {
		t.Errorf("restored %q", got)
	}
}

func TestIDs(t *testing.T) {
	ids := []int64{5, 6, 7, 3, 1000000, 1000001}
	got, err := DecodeIDs(EncodeIDs(ids))
	if err != nil || len(got) != len(ids) {
		t.Fatalf("%v, %v", got, err)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Errorf("ids = %v", got)
		}
	}
	if n := len(EncodeIDs([]int64{100, 101, 102, 103})); n != 5 { // 2 bytes for 100, then 1 per id
		t.Errorf("consecutive ids take %d bytes, want 5", n)
	}
}

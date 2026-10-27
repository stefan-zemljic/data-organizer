package scan

import (
	"crypto/sha256"
	"encoding/binary"
)

// Hash domain prefixes: the first byte written to every item hash, so a
// file and a directory can never produce the same hash.
const (
	filePrefix byte = 0
	dirPrefix  byte = 1
)

// Hash format (changing it changes every stored hash):
//
//	file  = sha256(0x00 ‖ content)
//	entry = sha256(uint64_le(len(name)) ‖ name ‖ childHash)   name is the UTF-8 item name, not the path
//	dir   = sha256(0x01 ‖ XOR of entry over all readable children)
//
// XOR makes the directory hash independent of listing order. Children that
// could not be read are left out entirely. An empty directory hashes 32 zero
// bytes after the prefix.

// entryDigest is a child's contribution to its parent directory hash.
func entryDigest(name string, childHash [32]byte) [32]byte {
	h := sha256.New()
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(name)))
	h.Write(n[:])
	h.Write([]byte(name))
	h.Write(childHash[:])
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// dirHash finalizes a directory hash from the XOR of its entry digests.
func dirHash(acc [32]byte) [32]byte {
	var buf [33]byte
	buf[0] = dirPrefix
	copy(buf[1:], acc[:])
	return sha256.Sum256(buf[:])
}

func xorInto(acc *[32]byte, d [32]byte) {
	for i := range acc {
		acc[i] ^= d[i]
	}
}

package scanner

import (
	"encoding/binary"
	"hash/fnv"
)

const quickHashBuckets = 10000

// quickSampled is a deterministic, path-hashed sample. Hashing the path avoids
// the ordering bias of "every Nth entry" while keeping a resumed run stable.
func quickSampled(path string, salt uint64, percent int) bool {
	if percent >= 100 {
		return true
	}
	if percent < 1 {
		percent = 1
	}
	var prefix [8]byte
	binary.LittleEndian.PutUint64(prefix[:], salt)
	h := fnv.New64a()
	_, _ = h.Write(prefix[:])
	_, _ = h.Write([]byte(path))
	return h.Sum64()%quickHashBuckets < uint64(percent*100)
}

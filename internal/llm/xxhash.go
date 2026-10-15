package llm

import (
	"encoding/binary"
	"math/bits"
)

const (
	xxPrime1 uint64 = 11400714785074694791
	xxPrime2 uint64 = 14029467366897019727
	xxPrime3 uint64 = 1609587929392839161
	xxPrime4 uint64 = 9650029242287828579
	xxPrime5 uint64 = 2870177450012600261
)

func xxRound(acc, input uint64) uint64 {
	acc += input * xxPrime2
	acc = bits.RotateLeft64(acc, 31)
	return acc * xxPrime1
}

func xxMergeRound(acc, val uint64) uint64 {
	val = xxRound(0, val)
	acc ^= val
	return acc*xxPrime1 + xxPrime4
}

// xxHash64 computes XXH64 of b with the given seed.
func xxHash64(b []byte, seed uint64) uint64 {
	n := len(b)
	var h uint64
	if n >= 32 {
		v1 := seed + xxPrime1 + xxPrime2
		v2 := seed + xxPrime2
		v3 := seed
		v4 := seed - xxPrime1
		for len(b) >= 32 {
			v1 = xxRound(v1, binary.LittleEndian.Uint64(b[0:8]))
			v2 = xxRound(v2, binary.LittleEndian.Uint64(b[8:16]))
			v3 = xxRound(v3, binary.LittleEndian.Uint64(b[16:24]))
			v4 = xxRound(v4, binary.LittleEndian.Uint64(b[24:32]))
			b = b[32:]
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) +
			bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		h = xxMergeRound(h, v1)
		h = xxMergeRound(h, v2)
		h = xxMergeRound(h, v3)
		h = xxMergeRound(h, v4)
	} else {
		h = seed + xxPrime5
	}
	h += uint64(n)
	for len(b) >= 8 {
		h ^= xxRound(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*xxPrime1 + xxPrime4
		b = b[8:]
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * xxPrime1
		h = bits.RotateLeft64(h, 23)*xxPrime2 + xxPrime3
		b = b[4:]
	}
	for _, c := range b {
		h ^= uint64(c) * xxPrime5
		h = bits.RotateLeft64(h, 11) * xxPrime1
	}
	h ^= h >> 33
	h *= xxPrime2
	h ^= h >> 29
	h *= xxPrime3
	h ^= h >> 32
	return h
}

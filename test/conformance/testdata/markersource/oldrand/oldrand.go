package oldrand

import (
	"encoding/binary"
	"math/rand"
)

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

func markerBytes() [8]byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], rand.Uint64())
	return b
}

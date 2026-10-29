package good

import (
	"encoding/binary"
	"math/rand/v2"
)

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

func markerBytes() [8]byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], rand.Uint64())
	return b
}

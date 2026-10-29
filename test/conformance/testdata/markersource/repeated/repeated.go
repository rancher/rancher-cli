package repeated

import (
	"encoding/binary"
	"math/rand/v2"
)

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

// markerBytes copies one 16-bit value into each 2-byte part of the array.
func markerBytes() [8]byte {
	var b [8]byte
	v := uint16(rand.Uint64())
	for i := range 4 {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

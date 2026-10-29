package spread

import (
	"encoding/binary"
	"math/rand/v2"
)

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

// markerBytes spreads 32 random bits over all 8 bytes with a multiplier.
func markerBytes() [8]byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], (rand.Uint64()>>32)*0x9E3779B97F4A7C15)
	return b
}

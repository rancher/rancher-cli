package pcg

import "math/rand/v2"

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

func markerBytes() [8]byte {
	var b [8]byte
	gen := rand.New(rand.NewPCG(1, 2))
	for i := range b {
		b[i] = byte(gen.Uint64())
	}
	return b
}

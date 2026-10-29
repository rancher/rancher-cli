package cryptorand

import "crypto/rand"

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

func markerBytes() [8]byte {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return b
}

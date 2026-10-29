package otherpackage

import (
	"encoding/binary"

	"github.com/rancher/rancher-cli/test/conformance/testdata/markersource/fakerand"
)

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

func markerBytes() [8]byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], fakerand.Uint64())
	return b
}

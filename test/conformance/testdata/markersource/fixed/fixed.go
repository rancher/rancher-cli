package fixed

var RemedyMarker = markerFromBytes(markerBytes())

func markerFromBytes(b [8]byte) string { return string(b[:]) }

func markerBytes() [8]byte { return [8]byte{1, 2, 3, 4, 5, 6, 7, 8} }

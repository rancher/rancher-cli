package clean

import "strings"

// reads only: a local variable of the same name, integers beside the range
// and text without the characters are not findings.
func use() string {
	Rancher := "local"
	Rancher = Rancher + "x"
	var b strings.Builder
	b.WriteRune(0x205f)
	b.WriteRune(0x2065)
	n := 8287 + 8293
	return Rancher + RemedyMarker + b.String() + string(rune(n)) + "\xe2\x81\xa5"
}

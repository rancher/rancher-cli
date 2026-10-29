package foldedinteger

import "strings"

func command() string {
	var b strings.Builder
	b.WriteRune(0x2060 + 3)
	return b.String()
}

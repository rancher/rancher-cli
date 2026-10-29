package writerune

import "strings"

func command() string {
	var b strings.Builder
	b.WriteRune(0x2063)
	return b.String()
}

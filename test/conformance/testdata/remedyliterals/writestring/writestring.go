package writestring

import "strings"

func command() string {
	var sb strings.Builder
	sb.WriteString("rancher ")
	return sb.String()
}

package callarg

import (
	"fmt"
	"strings"
)

func sprint(v string) string {
	return fmt.Sprint("# Kubeconfig ", v, "\n")
}

func appended(v []byte) []byte {
	return append([]byte("# Kubeconfig "), v...)
}

func join(v string) string {
	return strings.Join([]string{"# a", v}, "")
}

func fixed() string {
	return fmt.Sprint("# fixed")
}

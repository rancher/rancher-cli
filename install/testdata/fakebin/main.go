// Command fakebin stands in for a release binary in the Windows installer
// tests: it prints "<name> version <tag>", where name is its own file name
// and tag is read from the bytes appended after the last tagMarker in its
// own executable. Appending bytes to a built executable leaves it runnable,
// so one build serves every release tag a test registers.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const tagMarker = "\nRANCHER-FAKE-TAG:"

func main() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	idx := bytes.LastIndex(raw, []byte(tagMarker))
	if idx < 0 {
		fmt.Fprintln(os.Stderr, "no tag appended")
		os.Exit(1)
	}
	tag := strings.TrimSpace(string(raw[idx+len(tagMarker):]))
	name := strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
	fmt.Printf("%s version %s\n", name, tag)
}

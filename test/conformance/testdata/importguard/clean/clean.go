// Package clean is a fixture for the import guard: its closure is the
// standard library without network packages.
package clean

import "strings"

// Up is a use of the import.
func Up(s string) string { return strings.ToUpper(s) }

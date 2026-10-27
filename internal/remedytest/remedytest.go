// Package remedytest is test support for the checks that a remedy command
// names the config directory: which top-level commands read one, from the
// command tree's own schema manifest golden. Tests only; no production
// package imports it.
package remedytest

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// DirectoryCommandWords is every top-level command the schema manifest
// golden at path lists that reads the config directory (clierr.DirectoryFreeCommands
// lists the others), as a regexp
// alternation: the command tree itself, so a new command is covered with
// no list to keep.
func DirectoryCommandWords(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var m struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, c := range m.Commands {
		if w, _, _ := strings.Cut(c.Name, " "); !clierr.DirectoryFreeCommands[w] {
			seen[w] = true
		}
	}
	if !seen["auth"] || !seen["kubeconfig"] {
		return "", fmt.Errorf("%s lists no auth or kubeconfig command", path)
	}
	words := slices.Sorted(maps.Keys(seen))
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return strings.Join(words, "|"), nil
}

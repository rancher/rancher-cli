package cmdtest

import (
	"bytes"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// Input is a runtime.Input over fixed maps: Str answers String, Flag
// answers Bool, Pos is the positional argument list. Strings and IsSet
// always answer empty and false.
type Input struct {
	Str  map[string]string
	Flag map[string]bool
	Pos  []string
}

func (x Input) String(k string) string  { return x.Str[k] }
func (x Input) Bool(k string) bool      { return x.Flag[k] }
func (x Input) Strings(string) []string { return nil }
func (x Input) IsSet(string) bool       { return false }
func (x Input) Args() []string          { return x.Pos }

var _ runtime.Input = Input{}

// StdinPrompter builds a Prompter whose input is the canned text tok, for
// commands that read a token from stdin.
func StdinPrompter(tok string) *ui.Prompter {
	return ui.New(&bytes.Buffer{}, strings.NewReader(tok), true, false)
}

// View is a runtime.ConfigView over fixed context and server names.
type View struct {
	Contexts []string
	Servers  []string
	Dir      string
}

func (v View) ContextNames() []string { return v.Contexts }
func (v View) ServerNames() []string  { return v.Servers }
func (v View) ConfigDir() string      { return v.Dir }

var _ runtime.ConfigView = View{}

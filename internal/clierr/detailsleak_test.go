package clierr_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// gatedInput is the Input of a command that takes no flags and no arguments.
type gatedInput struct{}

func (gatedInput) String(string) string    { return "" }
func (gatedInput) Bool(string) bool        { return false }
func (gatedInput) Strings(string) []string { return nil }
func (gatedInput) IsSet(string) bool       { return false }
func (gatedInput) Args() []string          { return nil }

// gatedCommand is a read command with a capability requirement, which the
// pipeline refuses before it runs.
type gatedCommand struct{ req runtime.CapabilityRequirement }

func (c gatedCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "get widget", CommandType: runtime.ReadOnly, Capability: c.req}
}

func (gatedCommand) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

// fixedCapability answers every check with one decision.
type fixedCapability struct{ dec runtime.CapabilityDecision }

func (f fixedCapability) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	return f.dec
}

// decodedStrings returns every string of a rendered json or yaml document,
// keys and values, as its reader sees them.
func decodedStrings(t *testing.T, document string) []string {
	t.Helper()
	var doc any
	require.NoError(t, yaml.Unmarshal([]byte(document), &doc))
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				out = append(out, k)
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// leaksMarker reports whether a rendered document holds one of the five
// characters, as raw bytes or in a string it decodes to.
func leaksMarker(t *testing.T, document string) bool {
	t.Helper()
	return holdsMarkerRune(document) || slices.ContainsFunc(decodedStrings(t, document), holdsMarkerRune)
}

// TestCapabilityRefusalsLeaveNoMarkerInStructuredOutput asserts the two
// refusals of the capability stage, whose details payloads have no mapping of
// their own, write no character of the remedy marker (U+2060 to U+2064) to
// their json and yaml output when the pipeline renders them for an invocation
// against a config directory.
func TestCapabilityRefusalsLeaveNoMarkerInStructuredOutput(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		req  runtime.CapabilityRequirement
		dec  runtime.CapabilityDecision
		code string
	}{
		"a feature that needs Rancher Prime": {
			runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widgets"},
			runtime.CapabilityDecision{Outcome: runtime.CapabilityPrimeRequired, Server: "https://r.example"},
			clierr.CodePrimeRequired,
		},
		"an API the server does not serve": {
			runtime.CapabilityRequirement{Tier: runtime.CapabilityExtDiscovery, Feature: "widgets", Unsupported: "the widget workflow"},
			runtime.CapabilityDecision{Outcome: runtime.CapabilityUnsupported, Server: "https://r.example"},
			clierr.CodeCapabilityUnsupported,
		},
	} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(name+" "+format, func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: t.TempDir(), Capability: fixedCapability{tc.dec}}
				_, ce := runtime.Run(t.Context(), gatedCommand{tc.req}, gatedInput{}, streams, safety.Binding{})
				require.NotNil(t, ce)
				require.Equal(t, tc.code, ce.Code)
				assert.Contains(t, decodedStrings(t, errBuf.String()), "widgets", "the details reach the output")
				assert.False(t, leaksMarker(t, errBuf.String()), "the output holds a character of the remedy marker: %q", errBuf.String())
			})
		}
	}
}

package output

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestTheErrorEnvelopeNeverSaysAnErrorWhoseResultIsUnknownIsRetryable
// asserts that the JSON and YAML envelopes of an error with outcomeUnknown
// say retryable false even when a caller set Retryable to true on it
// directly, and that any other error keeps its own retryable.
func TestTheErrorEnvelopeNeverSaysAnErrorWhoseResultIsUnknownIsRetryable(t *testing.T) {
	t.Parallel()
	lost := clierr.UnknownOutcome("the request to create configmaps 'web'", clierr.OutcomeCause(io.EOF), io.EOF, clierr.Command{})
	lost.Retryable = true
	plain := clierr.New(clierr.CodeGeneric, "the server may be slow; retry later").WithRetryable(true)
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			decode := func(ce *clierr.Error) map[string]any {
				var buf bytes.Buffer
				require.NoError(t, Render(&buf, format, ErrorEnvelope(ce)))
				raw := buf.Bytes()
				if format == "yaml" {
					var err error
					raw, err = yaml.YAMLToJSON(raw)
					require.NoError(t, err)
				}
				var env struct {
					Data struct {
						Error map[string]any `json:"error"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(raw, &env))
				return env.Data.Error
			}
			got := decode(lost)
			assert.Equal(t, false, got["retryable"])
			assert.Equal(t, true, got["outcomeUnknown"])
			assert.Equal(t, true, decode(plain)["retryable"])
		})
	}
}

// TestTheJSONEnvelopeWritesHTMLCharactersOfAnErrorAsTheyAre asserts that the
// JSON envelope of an error writes <, > and & of its message as they are, as
// it does for every other text: clierr.Error.MarshalJSON leaves the choice to
// the output encoder. It reads the rendered bytes, because decoding cannot
// tell the character from its escape \u003c.
func TestTheJSONEnvelopeWritesHTMLCharactersOfAnErrorAsTheyAre(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "json", ErrorEnvelope(clierr.New(clierr.CodeValidation, "a bool flag takes its value only as --dry-run=<value> & nothing else"))))
	assert.Contains(t, buf.String(), `--dry-run=<value> & nothing else`)
}

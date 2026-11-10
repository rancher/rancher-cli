package token

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestTheNoTerminalErrorSaysToLogInWithoutATerminalAfterItsFacts asserts the
// lead of the piped login in each form of the plugin's no-terminal refusal
// (the refusal of one auth config, of a choice between several auth configs
// when none signs in without a terminal, and of that choice with candidates
// that do), crossed with each clause the login can need before it (none, a
// name of the operator's choice, an inline CA from each place it comes from,
// and a name with an inline CA): it follows the clause, or the refusal when
// there is no clause, and says "without a terminal", never "without one".
func TestTheNoTerminalErrorSaysToLogInWithoutATerminalAfterItsFacts(t *testing.T) {
	const url = "https://r.example.com"
	pem := selfSignedCAPEM(t)
	const lead = "to log in without a terminal, pipe an API token to: "
	const placeholder = "<name> stands for a name of your choice"
	inlineFacts := func(holder string) string {
		return holder + " and --cacert needs a file; " + config.CAFileMeaning
	}

	heads := []struct {
		name, head string
		err        error
		code       string
	}{
		{"one auth config", "interactive login required for auth config local; this call has no terminal to answer it",
			&needsTerminal{authConfig: "local"}, clierr.CodeAuth},
		{"several auth configs, none that signs in without a terminal", "interactive login required for one of the auth configs local, openldap; this call has no terminal to answer it",
			&providerChoiceNeedsTerminal{text: "choose", configs: []string{"local", "openldap"}}, clierr.CodeAuth},
		{"several auth configs, with candidates", "this server has several auth configs and this call has no terminal to choose one",
			&providerChoiceNeedsTerminal{text: "choose", configs: []string{"local", "corp-saml"},
				noTerminal: []clierr.Candidate{{ID: "corp-saml", Name: "corp-saml"}}}, clierr.CodeAmbiguousSelection},
	}
	facts := []struct {
		name  string
		call  noTerminalCall
		facts string
	}{
		{"no clause", noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url}, ""},
		{"a name of the operator's choice", noTerminalCall{clusterID: "c-1", serverURL: url}, placeholder + "; "},
		{"an inline CA of the exec entry", noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, caCert: pem, caFrom: config.CASource{Kind: config.CASourceFlag}},
			inlineFacts("this exec entry carries the CA inline") + "; "},
		{"an inline CA of the variable", noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, caCert: pem, caFrom: config.CASource{Kind: config.CASourceVariable}},
			inlineFacts("RANCHER_CLI_CACERT carries the CA inline") + "; "},
		{"an inline CA of a server entry", noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, caCert: pem, caFrom: entryFrom("other")},
			inlineFacts("server entry 'other' carries the CA inline in config.yaml") + "; "},
		{"a name of the operator's choice and an inline CA", noTerminalCall{clusterID: "c-1", serverURL: url, caCert: pem, caFrom: config.CASource{Kind: config.CASourceVariable}},
			placeholder + "; " + inlineFacts("RANCHER_CLI_CACERT carries the CA inline") + "; "},
	}
	for _, h := range heads {
		for _, f := range facts {
			t.Run(h.name+", "+f.name, func(t *testing.T) {
				err := tokenCmd{dir: t.TempDir()}.withNoTerminalRemedies(h.err, f.call)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, h.code, ce.Code)
				assert.True(t, strings.HasPrefix(ce.Message, h.head+"; "+f.facts+lead), ce.Message)
				assert.NotContains(t, ce.Message, "without one")
			})
		}
	}
}

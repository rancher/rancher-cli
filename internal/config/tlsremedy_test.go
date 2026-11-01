package config

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestEffectiveWordsACertificateFailureForTheConnection asserts the
// certificate failure wording of an ordinary command, row by row: a named
// entry whose CA is saved, absent or from RANCHER_CLI_CACERT, a server named
// only by RANCHER_CLI_URL (also when the URL leaves the current entry), and
// no server at all, which stays the zero wording. Mutations: change any
// string, read the wrong row for the variable, or key the entry rows on
// the current entry rather than the effective one.
func TestEffectiveWordsACertificateFailureForTheConnection(t *testing.T) {
	c := &Config{
		CurrentServer: "prod",
		Servers: map[string]Server{
			"prod": {URL: "https://prod.example.com", CA: "PROD-CA"},
			"odd'": {URL: "https://odd.example.com"},
		},
	}
	odd := &Config{CurrentServer: "odd'", Servers: c.Servers}
	save := func(name string) string {
		return "; <file> is a file that holds the server's CA certificate; save it for this entry with: " + clierr.Rancher + " config update-server " + name + " --cacert <file>"
	}
	variableOnEntry := func(name string) string {
		return "; the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA; <file> is a file that holds the server's CA certificate; to save a CA for this entry, run: " +
			clierr.Rancher + " config update-server " + name + " --cacert <file>"
	}
	for _, tc := range []struct {
		name    string
		cfg     *Config
		url, ca string
		want    tlstrust.Remedy
	}{
		{"a named entry", c, "", "",
			tlstrust.Remedy{CA: save("'prod'"), URL: "the server URL saved for entry 'prod'", Fix: "a different CA"}},
		{"a named entry with no saved CA", odd, "", "",
			tlstrust.Remedy{CA: save(`'odd'\'''`), URL: `the server URL saved for entry 'odd'\'''`, Fix: "a different CA"}},
		{"a named entry and the variable", c, "", "ENV-CA",
			tlstrust.Remedy{CA: variableOnEntry("'prod'"), URL: "the server URL saved for entry 'prod'", Fix: "a different CA"}},
		{"the same URL as the entry", c, "https://prod.example.com/", "",
			tlstrust.Remedy{CA: save("'prod'"), URL: "the server URL saved for entry 'prod'", Fix: "a different CA"}},
		{"RANCHER_CLI_URL with no entry", &Config{}, "https://env.example.com", "",
			tlstrust.Remedy{CA: "; set RANCHER_CLI_CACERT to it", URL: "RANCHER_CLI_URL", Fix: "a different CA"}},
		{"RANCHER_CLI_URL with no entry and the variable", &Config{}, "https://env.example.com", "ENV-CA",
			tlstrust.Remedy{CA: "; the CA came from RANCHER_CLI_CACERT - set it to the server's CA", URL: "RANCHER_CLI_URL", Fix: "a different CA"}},
		{"RANCHER_CLI_URL leaving the current entry", c, "https://staging.example.com", "",
			tlstrust.Remedy{CA: "; set RANCHER_CLI_CACERT to it", URL: "RANCHER_CLI_URL", Fix: "a different CA"}},
		{"no server", &Config{}, "", "", tlstrust.Remedy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvURL, tc.url)
			t.Setenv(EnvCACert, tc.ca)
			eff := tc.cfg.EffectiveConnection()
			assert.Equal(t, tc.want, eff.TLSRemedy)
		})
	}
}

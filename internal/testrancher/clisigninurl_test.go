package testrancher_test

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCLISignInURLReadsTheURLTheDashboardSignInPageAccepts asserts the
// sign-in URL the CLI prints is read as the dashboard sign-in page reads it:
// the request id and public key come back, and each refusal the dashboard
// sign-in page makes is an error.
func TestParseCLISignInURLReadsTheURLTheDashboardSignInPageAccepts(t *testing.T) {
	t.Parallel()
	const base = "https://rancher.example.com/dashboard/auth/login"
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"kubeconfig", base + "?cli=true&requestId=r1&publicKey=k1&responseType=kubeconfig", ""},
		{"surrounding space", " " + base + "?cli=true&requestId=r1&publicKey=k1&responseType=kubeconfig\n", ""},
		{"json", base + "?cli=true&requestId=r1&publicKey=k1&responseType=json", `responseType="json"`},
		{"cluster scoped kubeconfig", base + "?cli=true&requestId=r1&publicKey=k1&responseType=kubeconfig_c-1", `responseType="kubeconfig_c-1"`},
		{"no responseType", base + "?cli=true&requestId=r1&publicKey=k1", `responseType=""`},
		{"empty requestId", base + "?cli=true&requestId=&publicKey=k1&responseType=kubeconfig", "empty requestId"},
		{"no requestId", base + "?cli=true&publicKey=k1&responseType=kubeconfig", "empty requestId"},
		{"empty publicKey", base + "?cli=true&requestId=r1&publicKey=&responseType=kubeconfig", "empty publicKey"},
		{"both empty", base + "?cli=true&responseType=kubeconfig", "empty requestId and publicKey"},
		{"cli missing", base + "?requestId=r1&publicKey=k1&responseType=kubeconfig", `cli=""`},
		{"cli false", base + "?cli=false&requestId=r1&publicKey=k1&responseType=kubeconfig", `cli="false"`},
		{"another path", "https://rancher.example.com/login?cli=true&requestId=r1&publicKey=k1&responseType=kubeconfig", "/dashboard/auth/login"},
		{"not a URL", "%zz", "does not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, key, err := testrancher.ParseCLISignInURL(tc.raw)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, id)
				assert.Empty(t, key)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "r1", id)
			assert.Equal(t, "k1", key)
		})
	}
}

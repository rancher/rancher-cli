package cli

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEditionDecodersAgree asserts that the server edition the command reads
// (the version document decoded by the provider and mapped by the client
// factory to the engine's edition) and the edition the completion source reads
// (the extension package's decoder) agree for every value of the RancherPrime
// field, and that the factory maps each of the three editions to its own
// engine value.
func TestEditionDecodersAgree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want engine.Edition
	}{
		{"true", engine.EditionPrime},
		{"false", engine.EditionCommunity},
		{"TRUE", engine.EditionPrime},
		{" true ", engine.EditionPrime},
		{"", engine.EditionUnknown},
		{"yes", engine.EditionUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body, err := json.Marshal(map[string]string{"Version": "v2.15.3", "RancherPrime": tt.raw})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
			dir := t.TempDir()
			cfg := &config.Config{
				CurrentServer: "acme",
				Servers:       map[string]config.Server{"acme": {URL: srv.URL, CA: string(certPEM)}},
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())

			got, err := newClientFactory(dir, &countingStore{token: "id:secret"}).ServerEdition(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "the factory's edition for %q", tt.raw)

			leaf := extension.ParseEdition(tt.raw)
			switch tt.want {
			case engine.EditionPrime:
				assert.Equal(t, extension.EditionPrime, leaf, "the extension package's edition for %q", tt.raw)
			case engine.EditionCommunity:
				assert.Equal(t, extension.EditionCommunity, leaf, "the extension package's edition for %q", tt.raw)
			default:
				assert.Equal(t, extension.EditionUnknown, leaf, "the extension package's edition for %q", tt.raw)
			}
		})
	}
}

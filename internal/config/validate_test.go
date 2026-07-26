package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string // substring; "" means Validate must return nil
	}{
		{
			name: "valid config",
			cfg: &Config{
				CurrentContext:  "prod",
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				Contexts:        map[string]Context{"prod": {Server: "acme"}},
				CredentialStore: "file",
			},
		},
		{
			name: "valid config with unset credentialStore",
			cfg: &Config{
				Servers:  map[string]Server{"acme": {URL: "https://acme.ex"}},
				Contexts: map[string]Context{"prod": {Server: "acme"}},
			},
		},
		{
			name: "dangling context server",
			cfg: &Config{
				Servers:  map[string]Server{"acme": {URL: "https://acme.ex"}},
				Contexts: map[string]Context{"prod": {Server: "gone"}},
			},
			wantErr: `context "prod" references unknown server "gone"`,
		},
		{
			name: "dangling currentContext",
			cfg: &Config{
				CurrentContext: "ghost",
				Servers:        map[string]Server{"acme": {URL: "https://acme.ex"}},
				Contexts:       map[string]Context{"prod": {Server: "acme"}},
			},
			wantErr: `currentContext "ghost" is not a defined context`,
		},
		{
			name: "illegal credentialStore value",
			cfg: &Config{
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				Contexts:        map[string]Context{"prod": {Server: "acme"}},
				CredentialStore: "vault",
			},
			wantErr: `credentialStore "vault" is not one of: auto, file, keychain`,
		},
		{
			name: "auto and keychain are legal credentialStore values",
			cfg: &Config{
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				Contexts:        map[string]Context{"prod": {Server: "acme"}},
				CredentialStore: "auto",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.cfg)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

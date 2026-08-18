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
				CurrentServer:   "acme",
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				CredentialStore: "file",
			},
		},
		{
			name: "valid config with unset credentialStore",
			cfg: &Config{
				Servers: map[string]Server{"acme": {URL: "https://acme.ex"}},
			},
		},
		{
			name: "dangling currentServer",
			cfg: &Config{
				CurrentServer: "ghost",
				Servers:       map[string]Server{"acme": {URL: "https://acme.ex"}},
			},
			wantErr: `currentServer "ghost" is not a defined server`,
		},
		{
			name: "illegal credentialStore value",
			cfg: &Config{
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				CredentialStore: "vault",
			},
			wantErr: `credentialStore: unknown credential store "vault" (valid: keychain, file, auto, exec:<name>)`,
		},
		{
			name: "auto and keychain are legal credentialStore values",
			cfg: &Config{
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				CredentialStore: "auto",
			},
		},
		{
			name: "exec helper is a legal credentialStore value",
			cfg: &Config{
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				CredentialStore: "exec:vault",
			},
		},
		{
			name: "exec helper with invalid name is rejected",
			cfg: &Config{
				Servers:         map[string]Server{"acme": {URL: "https://acme.ex"}},
				CredentialStore: "exec:Not_Valid",
			},
			wantErr: `invalid credential helper name "Not_Valid"`,
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

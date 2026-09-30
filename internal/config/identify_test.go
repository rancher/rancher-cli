package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIdentifyEntry asserts how a --name becomes an entry: an exact entry
// name, else the entry named by the name less "-<cluster>", nothing looser,
// over every entry in the directory, never for the legacy shim, and an
// identified entry at another URL reported as such.
func TestIdentifyEntry(t *testing.T) {
	t.Parallel()
	cfg := &Config{Servers: map[string]Server{
		"prod":       {URL: "https://r.ex"},
		"prod-eu":    {URL: "https://r.ex"},
		"prod-elsew": {URL: "https://elsewhere.ex"},
		"legacy:zed": {URL: "https://r.ex"},
	}}
	cases := []struct {
		name, wantName, cluster string
		// server is the call's server, https://r.ex when empty.
		server string
		shim   bool
		want   Identification
	}{
		{name: "an exact entry name", wantName: "prod-eu", cluster: "c-1", want: Identification{Entry: "prod-eu"}},
		{name: "an exact entry name, no cluster", wantName: "prod-eu", want: Identification{Entry: "prod-eu"}},
		{name: "the entry-cluster form", wantName: "prod-eu-c-1", cluster: "c-1", want: Identification{Entry: "prod-eu"}},
		{name: "the entry-cluster form for the shorter entry", wantName: "prod-c-1", cluster: "c-1", want: Identification{Entry: "prod"}},
		{name: "the entry-cluster form needs the call's own cluster", wantName: "prod-eu-c-1", cluster: "c-2"},
		{name: "the entry-cluster form needs a cluster", wantName: "prod-eu-c-1"},
		{name: "a name that only begins with an entry's name identifies nothing", wantName: "prod-west-c-1", cluster: "c-1"},
		{name: "a name that names no entry", wantName: "kube-c-1", cluster: "c-1"},
		{name: "no name", cluster: "c-1"},
		{name: "the cluster alone is not an entry", wantName: "-c-1", cluster: "c-1"},
		{name: "the shim never identifies", wantName: "prod-eu", cluster: "c-1", shim: true},
		{name: "the shim never identifies a hand-edited legacy entry", wantName: "legacy:zed", shim: true},
		{name: "an exact entry at another URL", wantName: "prod-elsew", cluster: "c-1", want: Identification{Entry: "prod-elsew", OtherURL: true}},
		{name: "an entry-cluster entry at another URL", wantName: "prod-elsew-c-1", cluster: "c-1", want: Identification{Entry: "prod-elsew", OtherURL: true}},
		{name: "another spelling of the same URL is not another URL", wantName: "prod-eu", cluster: "c-1", server: "HTTPS://R.EX:443", want: Identification{Entry: "prod-eu"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := tc.server
			if server == "" {
				server = "https://r.ex"
			}
			assert.Equal(t, tc.want, IdentifyEntry(cfg, server, tc.wantName, tc.cluster, tc.shim))
		})
	}
}

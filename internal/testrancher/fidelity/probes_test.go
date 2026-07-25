package fidelity

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/token"
)

func TestAllProbesAreValidAndUniquelyNamed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, p := range All() {
		require.NoError(t, p.Validate(), p.Name)
		assert.False(t, seen[p.Name], "duplicate probe name %s", p.Name)
		seen[p.Name] = true
	}
	assert.Len(t, All(), 15)
}

func TestExpectedProbeSet(t *testing.T) {
	t.Parallel()
	want := []string{
		"auth/no-token", "auth/garbage-token", "auth/revoked-token", "auth/standard-mint",
		"auth/forbidden-read", "auth/forbidden-write",
		"tok/mint-shape", "tok/dryrun-delete", "tok/stale-precondition-delete", "tok/revoke",
		"mut/dryrun-create", "mut/dryrun-invalid", "mut/stale-rv-update",
		"mut/rv-precondition-delete", "mut/dryrun-then-apply",
	}
	var got []string
	for _, p := range All() {
		got = append(got, p.Name)
	}
	assert.ElementsMatch(t, want, got)
}

func TestRBACDenialProbesAreOracleOnly(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if p.Name == "auth/forbidden-read" || p.Name == "auth/forbidden-write" {
			assert.True(t, p.OracleOnly, p.Name)
		}
	}
}

func TestOracleOnlyProbesHaveNoFacadeGolden(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if !p.OracleOnly {
			continue
		}
		path := GoldenPath(filepath.Join("testdata", "facade"), p.Name)
		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err),
			"oracle-only probe %s must not have a façade golden at %s", p.Name, path)
	}
}

func TestTokenProbesUseTheProductionPathConstant(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if !strings.HasPrefix(p.Name, "tok/") && p.Name != "auth/standard-mint" {
			continue
		}
		for _, o := range p.Observations {
			assert.True(t, strings.HasPrefix(o.Request.Path, token.MintTokensPath),
				"%s %s path %q must be rooted at token.MintTokensPath", p.Name, o.Name, o.Request.Path)
		}
	}
}

func TestFixtureNamesCarryTheFidelityPrefix(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		for _, o := range p.Observations {
			if strings.Contains(o.Request.Body, `"name"`) {
				assert.Contains(t, o.Request.Body, "e2e-fid-",
					"%s %s creates a named fixture without the e2e-fid- prefix", p.Name, o.Name)
			}
		}
	}
}

func TestMintCapturesDeriveTheNameFromTheBearer(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		for _, o := range p.Observations {
			c, ok := o.Captures["tokname"]
			if !ok {
				continue
			}
			assert.Equal(t, Capture{Path: "status.bearerToken", ExtBearerName: true}, c,
				"%s %s: the mint response carries no metadata.name; the name must derive from the ext bearer", p.Name, o.Name)
		}
	}
}

func TestRVSequencesStartWithARecordedGet(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		switch p.Name {
		case "mut/stale-rv-update", "mut/rv-precondition-delete", "mut/dryrun-then-apply":
			require.NotEmpty(t, p.Setup, "%s creates its fixture in idempotent setup", p.Name)
			first := p.Observations[0]
			assert.Equal(t, http.MethodGet, first.Request.Method,
				"%s must record the initial GET the CLI pipeline performs", p.Name)
			assert.Contains(t, first.Captures, "rv1", p.Name)
		}
	}
}

func TestDryrunThenApplySendsByteIdenticalBodies(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if p.Name != "mut/dryrun-then-apply" {
			continue
		}
		var dryRun, apply string
		for _, o := range p.Observations {
			switch o.Name {
			case "dry-run-update":
				dryRun = o.Request.Body
			case "apply":
				apply = o.Request.Body
			}
		}
		require.NotEmpty(t, dryRun)
		assert.Equal(t, dryRun, apply,
			"execution must apply exactly the object the preview rendered")
	}
}

package kinds

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConformsToMCPVocabulary(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/mcp-kinds-050e375.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]struct{ Group, Version, Resource string }
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	have := map[string]Kind{}
	for _, k := range All() {
		have[k.Key] = k
		w, ok := want[k.Key]
		if !ok {
			t.Errorf("kind %q not in the MCP fixture — vocabulary drift", k.Key)
			continue
		}
		if k.GVR.Group != w.Group || k.GVR.Version != w.Version || k.GVR.Resource != w.Resource {
			t.Errorf("kind %q GVR %v diverges from MCP %+v", k.Key, k.GVR, w)
		}
	}
	// Bidirectional: every fixture key must exist in the table — a dropped
	// kind must fail conformance, not silently shrink the surface.
	for key := range want {
		if _, ok := have[key]; !ok {
			t.Errorf("fixture kind %q missing from the table", key)
		}
	}
}

func TestValidateRejectsCollidingAliases(t *testing.T) {
	t.Parallel()
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
	// A distinct-Key table whose alias collides with another kind's name.
	collide := []Kind{
		{Key: "a", Name: "foo"},
		{Key: "b", Name: "bar", Aliases: []string{"foo"}},
	}
	if err := validateKinds(collide); err == nil {
		t.Error("colliding alias must fail validation")
	}
}

func TestValidateRejectsDuplicateKey(t *testing.T) {
	t.Parallel()
	dup := []Kind{
		{Key: "same", Name: "one"},
		{Key: "same", Name: "two"},
	}
	if err := validateKinds(dup); err == nil {
		t.Error("duplicate Key must fail validation")
	}
}

func TestBindingsGeneratedPerKind(t *testing.T) {
	t.Parallel()
	b := Bindings()

	gc, ok := b["get cluster"]
	require.True(t, ok, "get cluster must have a generated binding")
	assert.Equal(t, safety.TierCRD, gc.Tier)
	assert.Equal(t, safety.Beta, gc.Stability)

	dp, ok := b["delete project"]
	require.True(t, ok, "delete project must have a generated binding")
	assert.Equal(t, safety.TierPublic, dp.Tier)
	assert.Equal(t, safety.Stable, dp.Stability)

	_, ok = b["delete cluster"]
	assert.False(t, ok, "clusters are not deletable — no delete binding")
}

func TestLookupByNameAliasAndKey(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"cluster", "clusters", "managementcluster"} {
		if k, ok := Lookup(q); !ok || k.Key != "managementcluster" {
			t.Errorf("Lookup(%q) = %+v, %v", q, k, ok)
		}
	}
	if _, ok := Lookup("gitrepo"); ok {
		t.Error("unknown kind must not resolve")
	}
}

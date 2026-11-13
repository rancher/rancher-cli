package token

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
)

// TestMaskedCacheServerAndKey asserts a cache record's server and key show as
// stored, except that a server which is the identity of the URL of an entry
// that holds an @ after the host shows as unusable in both, the key the way a
// deleted key does.
func TestMaskedCacheServerAndKey(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Servers: map[string]config.Server{
		"plain":  {URL: "https://hostzq7"},
		"userid": {URL: "https://u:pwzq7@other.example"},
		"misred": {URL: "https://admin:1234/pwzq7@x"},
		"y":      {URL: "https://admin:1234"},
	}}
	for _, tc := range []struct{ name, server, key, wantServer, wantKey string }{
		{"a plain server", "https://hostzq7", "x@https://hostzq7/c-1", "https://hostzq7", "x@https://hostzq7/c-1"},
		{"a server of an entry with userinfo", "https://other.example", "x@https://other.example/c-1", "https://other.example", "x@https://other.example/c-1"},
		{"the host read from an entry URL", "https://admin:1234", "x@https://admin:1234/c-1", "<unusable URL>", "x@<unusable URL>/c-1"},
		{"a key with a user id", "https://admin:1234", "user-id:u-abc12/x@https://admin:1234/c-1", "<unusable URL>", "user-id:u-abc12/x@<unusable URL>/c-1"},
		{"another server", "https://elsewhere.example", "x@https://elsewhere.example/c-1", "https://elsewhere.example", "x@https://elsewhere.example/c-1"},
		{"a usable entry at the same identity", "https://admin:1234", "y@https://admin:1234/c-1", "<unusable URL>", "y@<unusable URL>/c-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shown, _ := maskedCacheServer(cfg, tc.server)
			assert.Equal(t, tc.wantServer, shown)
			assert.Equal(t, tc.wantKey, maskedCacheKey(cfg, tc.server, tc.key))
		})
	}
}

// TestTheMaskedServerOfACacheItemIsDecidedWhenItIsPlanned asserts that the
// server a cache item shows is the one decided when the item was planned,
// before the delete changes the entry: after the entry's URL changes, as an
// update does before the report, the item still shows the server as unusable,
// and its revoke decision (the reason) is not read from that URL.
func TestTheMaskedServerOfACacheItemIsDecidedWhenItIsPlanned(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Servers: map[string]config.Server{"x": {URL: "https://admin:1234/pwzq7@x"}}}
	d := &Deletion{req: LocalDelete{Command: DeleteForUpdateServer, Targets: []string{"x"}}, cfg: cfg, view: cfg, removed: map[string]bool{}}
	it := d.planCacheRecord(cfg, config.CachedRecord{Key: "x@https://admin:1234/c-1", Name: "x", Server: "https://admin:1234", Cluster: "c-1"})
	d.items = append(d.items, it)
	assert.Equal(t, reasonUnproven, it.reason, "a record with no token is classified before its entry is found")
	assert.Equal(t, "<unusable URL>", d.shownServer(it))

	cfg.Servers["x"] = config.Server{URL: "https://usable.example"}
	assert.Equal(t, "<unusable URL>", d.shownServer(it), "the update changed the entry; the planned decision stays")
	assert.Equal(t, []string{"x@<unusable URL>/c-1"}, d.shownKeys([]string{"x@https://admin:1234/c-1"}))
}

// TestAKeyConflictNamesTheRecordNameShellQuoted asserts a record name that
// starts with a dash is written after "--" in the repair.
func TestAKeyConflictNamesTheRecordNameShellQuoted(t *testing.T) {
	t.Parallel()
	msg := clierr.ExpandRemedies(maskedKeyConflict(maskedMatch{n: 2, name: "-dash"}).Message, "")
	assert.Contains(t, msg, "to remove the 2 cached credentials named '-dash', run: rancher auth delete-token -- '-dash'")
}

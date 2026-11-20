package resources

import (
	"context"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// generateNameManifests are the manifests of the rows that createWarningManifest
// cannot build, by row key. Each names its object on the line "  name: x". No
// row needs one now.
var generateNameManifests = map[string]string{}

// generateNameExcluded are the rows no fixture can run, by row key, each with
// the reason.
// No row is excluded now: every row without NoCreateViaManifest runs.
var generateNameExcluded = map[string]string{}

// namingClient answers a create the way an API server does: the answer carries
// the object's name, and for an object that has only metadata.generateName a
// name the server made up.
type namingClient struct {
	createClient
}

func (c *namingClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	out, err := c.createClient.Create(ctx, gvr, ns, obj, dryRun)
	if err != nil {
		return nil, err
	}
	answer := out.DeepCopy()
	if answer.GetName() == "" {
		suffix := "dryr1"
		if !dryRun {
			suffix = "real2"
		}
		answer.SetName(answer.GetGenerateName() + suffix)
	}
	return answer, nil
}

// generateNameFixture is the manifest and input of row k with its metadata.name
// line replaced by metadata.generateName, or with the name kept.
func generateNameFixture(t *testing.T, k kinds.Kind, withName bool) ([]byte, input) {
	t.Helper()
	manifest, in := createWarningManifest(k)
	text := string(manifest)
	if custom, ok := generateNameManifests[k.Key]; ok {
		text = custom
	}
	naming := "  generateName: gen-\n"
	if withName {
		naming = "  name: x\n"
	}
	require.Contains(t, text, "  name: x\n", "the manifest of %s names its object", k.Key)
	return []byte(strings.Replace(text, "  name: x\n", naming, 1)), in
}

// TestCreateResourcePreviewNamesTheGenerateNamePrefix asserts, for every row
// that a manifest can create, that the preview of a manifest with only
// metadata.generateName names the prefix and not the name the server made up for
// the dry-run, that a manifest with a name previews that name, and that the
// result of the real create names the created object and never carries the
// marker of the preview. Every row is run or excluded with a reason, never both.
func TestCreateResourcePreviewNamesTheGenerateNamePrefix(t *testing.T) {
	t.Parallel()
	rows := map[string]kinds.Kind{}
	for _, k := range kinds.All() {
		rows[k.Key] = k
	}
	for key := range generateNameManifests {
		require.Contains(t, rows, key, "generateNameManifests names a key that is no row")
	}
	for key, reason := range generateNameExcluded {
		require.Contains(t, rows, key, "generateNameExcluded names a key that is no row")
		assert.NotEmpty(t, reason, "the exclusion of %s has no reason", key)
		assert.NotContains(t, generateNameManifests, key, "%s is both run and excluded", key)
	}
	for _, k := range kinds.All() {
		if k.NoCreateViaManifest != "" {
			continue
		}
		if reason, excluded := generateNameExcluded[k.Key]; excluded {
			t.Logf("excluded: %s: %s", k.Key, reason)
			continue
		}
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			manifest, in := generateNameFixture(t, k, false)
			client := &namingClient{}
			fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			eff, exec, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": manifest}), in)
			require.NoError(t, err)
			assert.Equal(t, k.GVR.Resource+"/gen-*", eff.Target)
			assert.Equal(t, "gen-*", eff.DisplayName)
			assert.NotEmpty(t, eff.DisplayKind)

			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Contains(t, res.Text, "gen-real2", "the result names the object the real create made")
			assert.NotContains(t, res.Text, "*")

			named, in := generateNameFixture(t, k, true)
			eff, _, err = planCreate(t, NewCreate(&recFactory{client: &namingClient{}, resolve: map[string]string{"prod": "c-abc"}}, mapFileSource{"m.yaml": named}), in)
			require.NoError(t, err)
			assert.Equal(t, k.GVR.Resource+"/x", eff.Target)
			assert.Equal(t, "x", eff.DisplayName)
		})
	}
}

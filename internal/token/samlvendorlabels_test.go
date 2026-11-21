package token

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite golden files")

const samlLabelsGoldenFile = "saml-vendor-labels.golden.json"

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sumPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// samlLabelsGolden is the recorded file: where the words were read and the
// label of every SAML provider name.
type samlLabelsGolden struct {
	Source struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Commit     string `json:"commit"`
		File       string `json:"file"`
		Key        string `json:"key"`
		SHA256     string `json:"sha256"`
	} `json:"source"`
	Labels map[string]string `json:"labels"`
}

// samlProviderNames are the provider names of every type in samlProviders,
// sorted.
func samlProviderNames() []string {
	var names []string
	for typ := range samlProviders {
		names = append(names, providerName(typ))
	}
	slices.Sort(names)
	return names
}

// readSamlLabelsGolden decodes the recorded file. It decodes and does not
// compare bytes. A checkout that changes line endings passes.
func readSamlLabelsGolden(t *testing.T) samlLabelsGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", samlLabelsGoldenFile))
	require.NoError(t, err)
	var golden samlLabelsGolden
	require.NoError(t, json.Unmarshal(data, &golden))
	return golden
}

// writeSamlLabelsGolden rewrites the recorded file from a YAML translations
// file: SAML_LABELS_YAML is its path, SAML_LABELS_REF and SAML_LABELS_COMMIT
// say which ref and commit it was taken from. It makes no network call and
// starts no process.
func writeSamlLabelsGolden(t *testing.T) {
	t.Helper()
	var env [3]string
	for i, name := range []string{"SAML_LABELS_YAML", "SAML_LABELS_REF", "SAML_LABELS_COMMIT"} {
		value, ok := os.LookupEnv(name)
		require.True(t, ok && value != "", "-update needs %s", name)
		env[i] = value
	}
	path, ref, commit := env[0], env[1], env[2]
	require.Regexp(t, commitPattern, commit, "SAML_LABELS_COMMIT must be 40 lower-case hex characters")
	data, err := os.ReadFile(path)
	require.NoError(t, err, "SAML_LABELS_YAML %s is unreadable", path)
	var doc struct {
		Model struct {
			AuthConfig struct {
				Provider map[string]any `json:"provider"`
			} `json:"authConfig"`
		} `json:"model"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))

	var golden samlLabelsGolden
	golden.Source.Repository = "rancher/dashboard"
	golden.Source.Ref = ref
	golden.Source.Commit = commit
	golden.Source.File = "shell/assets/translations/en-us.yaml"
	golden.Source.Key = "model.authConfig.provider"
	sum := sha256.Sum256(data)
	golden.Source.SHA256 = hex.EncodeToString(sum[:])
	golden.Labels = map[string]string{}
	for _, key := range samlProviderNames() {
		label, ok := doc.Model.AuthConfig.Provider[key].(string)
		require.True(t, ok && label != "", "model.authConfig.provider.%s has no string value in %s", key, path)
		golden.Labels[key] = label
	}
	out, err := json.MarshalIndent(golden, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join("testdata", samlLabelsGoldenFile), append(out, '\n'), 0o644))
}

// TestSamlVendorLabelsGolden asserts the labels the sign-in notice uses equal
// the recorded words, that the recorded words cover every SAML provider name
// and no other, and that the record names a full commit and a checksum. A
// hand-written file fails. It cannot assert the words equal what a real
// sign-in page shows: they are read from the page's English translations and
// recorded. Run with -update (see writeSamlLabelsGolden for its input) to
// record them again.
func TestSamlVendorLabelsGolden(t *testing.T) {
	if *update {
		writeSamlLabelsGolden(t)
	}
	golden := readSamlLabelsGolden(t)

	assert.Equal(t, samlProviderNames(), slices.Sorted(maps.Keys(golden.Labels)), "the golden has a label for every SAML provider name and no other")
	for key, label := range golden.Labels {
		assert.NotEmpty(t, label, key)
	}
	assert.Equal(t, golden.Labels, samlVendorLabels)
	assert.Regexp(t, commitPattern, golden.Source.Commit, "source.commit")
	assert.Regexp(t, sumPattern, golden.Source.SHA256, "source.sha256")
}

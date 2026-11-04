package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	apiFieldsScript = "scripts/rancher-api-fields"

	apiFieldsCommunityLabel = "org.opencontainers.image.revision"
	apiFieldsPrimeLabel     = "org.opencontainers.image.oss.revision"

	apiFieldsCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	apiFieldsCommitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	apiFieldsCommitC = "cccccccccccccccccccccccccccccccccccccccc"

	apiFieldsStubDocker = `#!/bin/sh
echo "$*" >> "$STUB_MARKER"
exit 1
`
)

// apiFieldsPlatform is the part of one platform's image description the
// script reads.
func apiFieldsPlatform(labels map[string]string) map[string]any {
	return map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]any{"Labels": labels},
	}
}

// apiFieldsMulti is the buildx output of a multi-platform image: a map keyed
// by platform. The attestation entry is left in to assert it is skipped.
func apiFieldsMulti(amd64, arm64 map[string]string) map[string]any {
	return map[string]any{
		"linux/amd64":     apiFieldsPlatform(amd64),
		"linux/arm64":     apiFieldsPlatform(arm64),
		"unknown/unknown": map[string]any{"architecture": "unknown", "os": "unknown"},
	}
}

func apiFieldsLabels(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

type apiFieldsRun struct {
	stdout, stderr string
	err            error
	dockerCalls    string
}

// runAPIFieldsScript runs scripts/rancher-api-fields with the label hook set
// to the given file content. The module lookup cannot reach a network
// (GOPROXY=off, an empty module cache, no toolchain download), and a docker
// stub first on PATH records every call, so a run that reaches the registry
// is seen. extraEnv entries come last and replace those above.
func runAPIFieldsScript(t *testing.T, hook map[string]any, extraEnv ...string) apiFieldsRun {
	t.Helper()
	testfs.SkipOnWindows(t, posixOnlyReason)
	raw, err := json.Marshal(hook)
	require.NoError(t, err)
	hookFile := filepath.Join(t.TempDir(), "labels.json")
	require.NoError(t, os.WriteFile(hookFile, raw, 0o644))
	stubs := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stubs, "docker"), []byte(apiFieldsStubDocker), 0o755))
	marker := filepath.Join(t.TempDir(), "docker-calls")

	cmd := exec.CommandContext(t.Context(), "bash", apiFieldsScript)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RANCHER_API_FIELDS_LABELS_FILE="+hookFile,
		"RANCHER_API_FIELDS_UPDATE=",
		"STUB_MARKER="+marker,
		"GOPROXY=off",
		"GOMODCACHE="+t.TempDir(),
		"GOTOOLCHAIN=local",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := apiFieldsRun{err: cmd.Run()}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	if data, err := os.ReadFile(marker); err == nil {
		res.dockerCalls = string(data)
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	return res
}

// TestRancherAPIFieldsScriptLabels asserts scripts/rancher-api-fields stops
// with a message naming the edition, the label and the problem when an
// oracle image's labels are missing, malformed or inconsistent; reads the
// label the edition's table row names and no other; and, for good labels,
// gets past the label step to the module lookup, which fails offline with
// the commit in its message; and that the golden update refuses the label
// hook. The label hook stands in for the registry, so no row calls docker
// or the network. The test runs the script, so it lives in this package,
// which never comes from the test cache.
func TestRancherAPIFieldsScriptLabels(t *testing.T) {
	good := apiFieldsMulti(
		apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitA),
		apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitA),
	)
	goodPrime := apiFieldsMulti(
		apiFieldsLabels(apiFieldsPrimeLabel, apiFieldsCommitB),
		apiFieldsLabels(apiFieldsPrimeLabel, apiFieldsCommitB),
	)
	single := func(kv ...string) map[string]any { return apiFieldsPlatform(apiFieldsLabels(kv...)) }

	bad := []struct {
		name string
		hook map[string]any
		want []string
	}{
		{
			"community multi-platform without its label",
			map[string]any{"community": apiFieldsMulti(apiFieldsLabels(apiFieldsPrimeLabel, apiFieldsCommitA), apiFieldsLabels(apiFieldsPrimeLabel, apiFieldsCommitA)), "prime": goodPrime},
			[]string{"community commit from label " + apiFieldsCommunityLabel, "linux/amd64: the image has no label " + apiFieldsCommunityLabel},
		},
		{
			"prime with only the community label",
			map[string]any{"community": good, "prime": apiFieldsMulti(apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitB), apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitB))},
			[]string{"prime commit from label " + apiFieldsPrimeLabel, "the image has no label " + apiFieldsPrimeLabel},
		},
		{
			"upper-case hex",
			map[string]any{"community": apiFieldsMulti(apiFieldsLabels(apiFieldsCommunityLabel, strings.ToUpper(apiFieldsCommitA)), apiFieldsLabels(apiFieldsCommunityLabel, strings.ToUpper(apiFieldsCommitA))), "prime": goodPrime},
			[]string{"community commit", strings.ToUpper(apiFieldsCommitA), "not 40 lowercase hex digits"},
		},
		{
			"short value",
			map[string]any{"community": good, "prime": apiFieldsMulti(apiFieldsLabels(apiFieldsPrimeLabel, "bbbbbbb"), apiFieldsLabels(apiFieldsPrimeLabel, "bbbbbbb"))},
			[]string{"prime commit", `"bbbbbbb"`, "not 40 lowercase hex digits"},
		},
		{
			"platforms disagree",
			map[string]any{"community": apiFieldsMulti(apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitA), apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitC)), "prime": goodPrime},
			[]string{"community commit", "the platforms disagree on label " + apiFieldsCommunityLabel, apiFieldsCommitA, apiFieldsCommitC},
		},
		{
			"single platform without its label, community",
			map[string]any{"community": single(apiFieldsPrimeLabel, apiFieldsCommitA), "prime": goodPrime},
			[]string{"community commit from label " + apiFieldsCommunityLabel, "single platform: the image has no label"},
		},
		{
			"single platform without its label, prime",
			map[string]any{"community": good, "prime": single(apiFieldsCommunityLabel, apiFieldsCommitB)},
			[]string{"prime commit from label " + apiFieldsPrimeLabel, "single platform: the image has no label"},
		},
		{
			"edition missing from the file",
			map[string]any{"community": good},
			[]string{"prime commit", "has no entry for the edition prime"},
		},
		{
			"no platform",
			map[string]any{"community": map[string]any{"unknown/unknown": map[string]any{}}, "prime": goodPrime},
			[]string{"community commit", "names no platform"},
		},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			res := runAPIFieldsScript(t, tc.hook)
			require.Error(t, res.err)
			_, ok := errors.AsType[*exec.ExitError](res.err)
			require.True(t, ok, "%v", res.err)
			for _, w := range tc.want {
				assert.Contains(t, res.stderr, w)
			}
			assert.NotContains(t, res.stderr, "cannot resolve", "the label step must stop the script first")
			assert.Empty(t, res.dockerCalls, "the label hook must bypass docker")
		})
	}

	t.Run("good labels reach the module lookup", func(t *testing.T) {
		res := runAPIFieldsScript(t, map[string]any{
			"community": single(apiFieldsCommunityLabel, apiFieldsCommitA),
			"prime": apiFieldsMulti(
				apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitC, apiFieldsPrimeLabel, apiFieldsCommitB),
				apiFieldsLabels(apiFieldsCommunityLabel, apiFieldsCommitC, apiFieldsPrimeLabel, apiFieldsCommitB),
			),
		})
		require.Error(t, res.err)
		assert.Contains(t, res.stdout, "community: label "+apiFieldsCommunityLabel)
		assert.Contains(t, res.stdout, "is "+apiFieldsCommitA)
		assert.Contains(t, res.stderr, "the community labels come from RANCHER_API_FIELDS_LABELS_FILE=")
		assert.Contains(t, res.stdout, "prime: label "+apiFieldsPrimeLabel)
		assert.Contains(t, res.stdout, "is "+apiFieldsCommitB, "the prime commit is the value of its own label")
		assert.NotContains(t, res.stdout, apiFieldsCommitC)
		assert.Contains(t, res.stderr, "cannot resolve github.com/rancher/rancher/pkg/apis at commit "+apiFieldsCommitA)
		assert.Empty(t, res.dockerCalls, "the label hook must bypass docker")
	})

	t.Run("the update refuses the label hook", func(t *testing.T) {
		res := runAPIFieldsScript(t, map[string]any{"community": good, "prime": goodPrime}, "RANCHER_API_FIELDS_UPDATE=1")
		_, ok := errors.AsType[*exec.ExitError](res.err)
		require.True(t, ok, "%v", res.err)
		assert.Contains(t, res.stderr, "the golden is written only from the registries' labels")
		assert.NotContains(t, res.stdout, "community: label", "the update must stop before reading any label")
		assert.Empty(t, res.dockerCalls, "the label hook must bypass docker")
	})
}

//go:build e2e_fidelity

package fidelity_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kubernetesVersionsData is the `get kubernetes-versions` envelope data.
type kubernetesVersionsData struct {
	Versions       []kubernetesVersionRow `json:"versions"`
	SupportedRange string                 `json:"supportedRange"`
}

type kubernetesVersionRow struct {
	Distro    string `json:"distro"`
	Version   string `json:"version"`
	Default   bool   `json:"default"`
	Newest    bool   `json:"newest"`
	Supported *bool  `json:"supported"`
}

// TestCLIKubernetesVersions asserts that `get kubernetes-versions` marks
// DEFAULT and SUPPORTED the way the server's own settings say, against a
// real Rancher. It reads the three settings through the API, computes the
// expected default row itself (a release equals the setting once a leading
// "v" is trimmed from both), and compares that with the CLI's output. It
// creates nothing on the server and records no values: the release list and
// the settings follow published data, so only their relationship is checked.
func TestCLIKubernetesVersions(t *testing.T) {
	base := os.Getenv("E2E_FIDELITY_URL")
	adminToken := os.Getenv("E2E_FIDELITY_ADMIN_TOKEN")
	caPEM := os.Getenv("E2E_FIDELITY_CACERT")
	const envNeeded = "E2E_FIDELITY_URL, E2E_FIDELITY_ADMIN_TOKEN, and E2E_FIDELITY_CACERT must all be set (run via scripts/e2e-fidelity)"
	require.NotEmpty(t, base, envNeeded)
	require.NotEmpty(t, adminToken, envNeeded)
	require.NotEmpty(t, caPEM, envNeeded)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(caPEM)), "E2E_FIDELITY_CACERT: no certificates parsed")
	apiClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   30 * time.Second,
	}

	binPath, err := buildCLI(t)
	require.NoError(t, err)

	ctx := t.Context()
	defaults := []struct{ distro, setting, value string }{
		{distro: "rke2", setting: "rke2-default-version"},
		{distro: "k3s", setting: "k3s-default-version"},
	}
	for i := range defaults {
		defaults[i].value = readSettingValue(ctx, t, apiClient, base, adminToken, defaults[i].setting)
		require.NotEmpty(t, defaults[i].value, "%s is empty on the server: it could not load release data, which this test cannot check around", defaults[i].setting)
	}
	supportedRange := readSettingValue(ctx, t, apiClient, base, adminToken, "ui-k8s-supported-versions-range")

	cmd := exec.CommandContext(ctx, binPath, "get", "kubernetes-versions", "-o", "json")
	cmd.Env = []string{
		"RANCHER_CLI_URL=" + base,
		"RANCHER_CLI_TOKEN=" + adminToken,
		"RANCHER_CLI_CACERT=" + caPEM,
		"RANCHER_CLI_CONFIG_DIR=" + t.TempDir(),
		"RANCHER_CLI_CREDENTIAL_STORE=file",
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
	}
	stdout, runErr := cmd.Output()
	require.NoError(t, runErr, "running 'get kubernetes-versions': stderr=%s", func() string {
		if ee, ok := errors.AsType[*exec.ExitError](runErr); ok {
			return string(ee.Stderr)
		}
		return ""
	}())

	var env cliEnvelope
	require.NoError(t, json.Unmarshal(stdout, &env), "decoding CLI JSON output")
	assert.Equal(t, "KubernetesVersionList", env.Kind)
	var data kubernetesVersionsData
	require.NoError(t, json.Unmarshal(env.Data, &data), "decoding data field")

	for _, r := range data.Versions {
		assert.True(t, strings.HasPrefix(r.Version, "v"), "release %q of %s does not start with v", r.Version, r.Distro)
	}

	for _, d := range defaults {
		distro, value := d.distro, d.value
		var rows []kubernetesVersionRow
		for _, r := range data.Versions {
			if r.Distro == distro {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		unlisted := fmt.Sprintf("%s is %q on the server, which names no listed release; no default marked", d.setting, value)
		wantListed := slices.ContainsFunc(rows, func(r kubernetesVersionRow) bool { return namesRelease(r.Version, value) })
		if wantListed {
			for _, r := range rows {
				assert.Equal(t, namesRelease(r.Version, value), r.Default, "%s release %q: default (setting %q)", distro, r.Version, value)
			}
			assert.NotContains(t, env.Warnings, unlisted, "%s: setting %q names a listed release", distro, value)
			continue
		}
		for _, r := range rows {
			assert.False(t, r.Default, "%s release %q marked default; setting %q names no listed release", distro, r.Version, value)
		}
		assert.Contains(t, env.Warnings, unlisted, "%s: setting %q names no listed release", distro, value)
	}

	if supportedRange == "" {
		return
	}
	assert.Equal(t, supportedRange, data.SupportedRange, "supportedRange")
	// Rancher writes the range with a "v" before each operand. Without one,
	// the setting still holds its built-in default, which Rancher keeps when
	// it could not compute the range from the release data; whether a row
	// falls inside that default says nothing about the server.
	if !strings.Contains(supportedRange, "v") {
		return
	}
	anySupported := false
	for _, r := range data.Versions {
		if assert.NotNil(t, r.Supported, "%s release %q: supported is null for range %q", r.Distro, r.Version, supportedRange) && *r.Supported {
			anySupported = true
		}
	}
	assert.True(t, anySupported, "no release is supported under range %q", supportedRange)
}

// readSettingValue reads one Setting through the API and returns its value
// by the CLI's own rule: the value when non-empty, else the default.
func readSettingValue(ctx context.Context, t *testing.T, client *http.Client, base, token, name string) string {
	t.Helper()
	obj, err := doJSON(ctx, client, token, http.MethodGet,
		fmt.Sprintf("%s/k8s/clusters/local/apis/management.cattle.io/v3/settings/%s", base, name), nil, http.StatusOK)
	require.NoError(t, err, "reading setting %s", name)
	if v, _ := obj["value"].(string); v != "" {
		return v
	}
	v, _ := obj["default"].(string)
	return v
}

// namesRelease reports whether setting names release: equal once a leading
// "v" is trimmed from each, the comparison the CLI makes.
func namesRelease(release, setting string) bool {
	return strings.TrimPrefix(release, "v") == strings.TrimPrefix(setting, "v")
}

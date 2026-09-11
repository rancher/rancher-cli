//go:build e2e_fidelity

package fidelity_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIImagesWorkload(t *testing.T) {
	base := os.Getenv("E2E_FIDELITY_URL")
	adminToken := os.Getenv("E2E_FIDELITY_ADMIN_TOKEN")
	caPEM := os.Getenv("E2E_FIDELITY_CACERT")
	require.NotEmpty(t, base, "E2E_FIDELITY_URL, E2E_FIDELITY_ADMIN_TOKEN, and E2E_FIDELITY_CACERT must all be set (run via scripts/e2e-fidelity)")
	require.NotEmpty(t, adminToken, "E2E_FIDELITY_URL, E2E_FIDELITY_ADMIN_TOKEN, and E2E_FIDELITY_CACERT must all be set (run via scripts/e2e-fidelity)")
	require.NotEmpty(t, caPEM, "E2E_FIDELITY_URL, E2E_FIDELITY_ADMIN_TOKEN, and E2E_FIDELITY_CACERT must all be set (run via scripts/e2e-fidelity)")

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(caPEM)), "E2E_FIDELITY_CACERT: no certificates parsed")
	apiClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   30 * time.Second,
	}

	binPath, err := buildCLI(t)
	require.NoError(t, err)

	const (
		depName      = "e2e-fid-images-dep"
		depNS        = "default"
		depImage     = "busybox:1.36"
		fixtureLabel = "e2e-fid-images"
	)
	depListPath := fmt.Sprintf("%s/k8s/clusters/local/apis/apps/v1/namespaces/%s/deployments", base, depNS)
	podListPath := fmt.Sprintf("%s/k8s/clusters/local/api/v1/namespaces/%s/pods", base, depNS)

	deleteDeployment(t.Context(), t, apiClient, adminToken, depListPath, depName)
	t.Cleanup(func() { deleteDeployment(context.Background(), t, apiClient, adminToken, depListPath, depName) }) // survives the test body

	// After deleting a prior fixture deployment, wait for all pods carrying the
	// fixture label to leave the API before creating a new one. k3s retains
	// Running-phase pods until they are fully removed, and the CLI's owner-chain
	// walk would generate a uid-mismatch warning for any old pod whose ownerRef
	// chain resolves to a same-name-but-different-uid Deployment.
	ctx := t.Context()
	require.NoError(t,
		waitPodsGone(ctx, t, apiClient, adminToken,
			fmt.Sprintf("%s?labelSelector=app%%3D%s", podListPath, fixtureLabel),
			30*time.Second),
		"timed out waiting for old fixture pods to be removed")

	dep := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      depName,
			"namespace": depNS,
		},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{
				"matchLabels": map[string]any{"app": fixtureLabel},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"app": fixtureLabel},
				},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":    "main",
							"image":   depImage,
							"command": []any{"sleep", "3600"},
						},
					},
				},
			},
		},
	}
	depBody, err := json.Marshal(dep)
	require.NoError(t, err)

	created, err := doJSON(ctx, apiClient, adminToken, http.MethodPost, depListPath, depBody, http.StatusCreated)
	require.NoError(t, err, "creating deployment")

	meta, _ := created["metadata"].(map[string]any)
	depUID, _ := meta["uid"].(string)
	require.NotEmpty(t, depUID, "deployment metadata.uid must not be empty")

	t.Logf("deployment %s created with uid %s; waiting for available replica", depName, depUID)

	depURL := fmt.Sprintf("%s/%s", depListPath, depName)
	require.NoError(t, pollDeploymentAvailable(ctx, t, apiClient, adminToken, depURL, 3*time.Minute))

	configDir := t.TempDir()
	cmd := exec.CommandContext(ctx, binPath,
		"images", "--cluster", "local", "--by", "workload", "--match", depImage, "-o", "json",
	)
	cmd.Env = []string{
		"RANCHER_CLI_URL=" + base,
		"RANCHER_CLI_TOKEN=" + adminToken,
		"RANCHER_CLI_CACERT=" + caPEM,
		"RANCHER_CLI_CONFIG_DIR=" + configDir,
		"RANCHER_CLI_CREDENTIAL_STORE=file",
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
	}
	stdout, runErr := cmd.Output()
	require.NoError(t, runErr, "running 'images --by workload': stderr=%s", func() string {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return string(ee.Stderr)
		}
		return ""
	}())

	var env cliEnvelope
	require.NoError(t, json.Unmarshal(stdout, &env), "decoding CLI JSON output")
	assert.Equal(t, "ClusterImages", env.Kind)

	var data imagesResultData
	require.NoError(t, json.Unmarshal(env.Data, &data), "decoding data field")
	assert.Equal(t, "workload", data.By)

	var matched *imagesWorkloadRow
	for i := range data.Rows {
		r := &data.Rows[i]
		if r.Image == depImage && r.Namespace == depNS &&
			r.Workload.Kind == "deployment" && r.Workload.Name == depName {
			matched = r
			break
		}
	}
	require.NotNil(t, matched,
		"no row with image=%s namespace=%s workload.kind=deployment workload.name=%s; rows=%d",
		depImage, depNS, depName, len(data.Rows))

	assert.Equal(t, "apps/v1", matched.Workload.APIVersion, "workload.apiVersion")
	assert.GreaterOrEqual(t, matched.Pods, 1, "pods count must be >= 1")

	// Decisive integrity check: the row's workload UID must equal the live
	// Deployment's metadata.uid. This proves the terminal identity in the owner
	// chain is the real object, not a cached or stale reference.
	assert.Equal(t, depUID, matched.Workload.UID,
		"workload.uid must equal the live Deployment metadata.uid")

	// The successful row match (workload.name == depName) is itself proof the
	// owner-chain walk succeeded: a degraded walk falls back to the
	// first-level ownerRef (the ReplicaSet), so a failed walk would produce a
	// row with the RS name, not the Deployment name - and the row search above
	// would not have matched. Verify no warning mentions the deployment to
	// double-confirm no degradation path was taken.
	for _, w := range env.Warnings {
		assert.NotContains(t, w, depName,
			"no warning should mention %s (owner-chain walk should have succeeded)", depName)
	}
}

func buildCLI(t *testing.T) (string, error) {
	t.Helper()
	binPath := t.TempDir() + "/rancher"
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/rancher/rancher-cli").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building rancher CLI: %s", out)
	}
	return binPath, nil
}

func deleteDeployment(ctx context.Context, t *testing.T, client *http.Client, token, depListPath, name string) {
	t.Helper()
	url := depListPath + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		t.Logf("deleteDeployment: building request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("deleteDeployment: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusNoContent &&
		resp.StatusCode != http.StatusNotFound {
		t.Logf("deleteDeployment: unexpected status %d", resp.StatusCode)
	}
}

func doJSON(ctx context.Context, client *http.Client, token, method, url string, body []byte, wantStatus int) (map[string]any, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, fmt.Errorf("%s %s: building request: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading body: %w", method, url, err)
	}
	if resp.StatusCode != wantStatus {
		return nil, fmt.Errorf("%s %s: status %d, want %d: %.400s", method, url, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("%s %s: decoding JSON: %w", method, url, err)
		}
	}
	return out, nil
}

func waitPodsGone(ctx context.Context, t *testing.T, client *http.Client, token, podListURL string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		body, err := doJSON(ctx, client, token, http.MethodGet, podListURL, nil, http.StatusOK)
		if err != nil {
			t.Logf("poll error: %v", err)
		} else {
			items, _ := body["items"].([]any)
			if len(items) == 0 {
				return nil
			}
			t.Logf("waitPodsGone: %d pod(s) still present", len(items))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("old fixture pods not gone within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func pollDeploymentAvailable(ctx context.Context, t *testing.T, client *http.Client, token, depURL string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		body, err := doJSON(ctx, client, token, http.MethodGet, depURL, nil, http.StatusOK)
		if err != nil {
			t.Logf("poll error: %v", err)
		} else {
			status, _ := body["status"].(map[string]any)
			avail, _ := status["availableReplicas"].(float64)
			if avail >= 1 {
				t.Logf("deployment available (availableReplicas=%.0f)", avail)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment not available within %s", timeout)
		}
		t.Log("waiting for deployment to become available…")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

type cliEnvelope struct {
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings,omitempty"`
}

type imagesResultData struct {
	By   string              `json:"by"`
	Rows []imagesWorkloadRow `json:"rows"`
}

type imagesWorkloadRow struct {
	Image     string      `json:"image"`
	Namespace string      `json:"namespace"`
	Workload  workloadRef `json:"workload"`
	Pods      int         `json:"pods"`
}

type workloadRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

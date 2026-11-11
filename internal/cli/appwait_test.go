package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppWaitFlagsWithoutARequest asserts --wait and --timeout send nothing
// and wait for nothing in a preview, and that a --timeout past 55m is
// refused there too.
func TestAppWaitFlagsWithoutARequest(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	for name, args := range map[string][]string{
		"install --wait":          withArgs(appInstallArgs, "--wait", "--dry-run", "-o", "json"),
		"install --timeout":       withArgs(appInstallArgs, "--timeout", "1m", "--dry-run", "-o", "json"),
		"uninstall --wait":        withArgs(appUninstallArgs, "--wait", "--dry-run", "-o", "json"),
		"uninstall --timeout":     withArgs(appUninstallArgs, "--timeout", "1m", "--dry-run", "-o", "json"),
		"install without apply":   withArgs(appInstallArgs, "--wait", "-o", "json"),
		"install --timeout 55m":   withArgs(appInstallArgs, "--timeout", "55m", "--dry-run", "-o", "json"),
		"uninstall without --yes": withArgs(appUninstallArgs, "--wait", "--apply", "-o", "json"),
	} {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			s.putApp("deployed")
			start := time.Now()
			_, _, _ = appRun(t, s, args...)
			assert.Zero(t, s.postCount())
			assert.Zero(t, s.getCount(appOperationPath(appNS, appOpName)))
			assert.Less(t, time.Since(start), 5*time.Second)
		})
	}
	for _, args := range [][]string{
		withArgs(appInstallArgs, "--timeout", "56m", "--dry-run", "-o", "json"),
		withArgs(appUninstallArgs, "--timeout", "56m", "--dry-run", "-o", "json"),
	} {
		s := newAppServer(t, "community")
		s.putApp("deployed")
		_, stderr, err := appRun(t, s, args...)
		require.Error(t, err)
		ce := decodeErrorEnvelope(t, stderr)
		assert.Equal(t, "VALIDATION", ce.Code)
		assert.Equal(t, "--timeout 56m is longer than 55m; Rancher removes the operation one hour after it starts", ce.Message)
		assert.Zero(t, s.postCount())
	}
}

// TestAppWaitInterrupted asserts a wait the caller interrupts ends with
// INTERRUPTED and the committed details, and names the request id. The
// interrupt comes when the server answers the wait's first read of the
// Operation.
func TestAppWaitInterrupted(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := newAppServer(t, "community")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.completeOnAction(opStatus([3]string{"Reconciling", "True", "running operation"}, [3]string{"Stalled", "False", ""}), nil)
	var once sync.Once
	s.plant = func(resource string, _ map[string]any) {
		if resource == "operations" {
			once.Do(cancel)
		}
	}
	dir := grConfigDir(t, s.srv)
	root := grRoot(t)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	err := root.Run(ctx, append([]string{"rancher", "--config", dir}, withArgs(appInstallArgs, "--apply", "--wait", "-o", "json")...))
	require.Error(t, err)
	var env struct {
		Data struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Details struct {
					Committed    bool   `json:"committed"`
					ResourceKind string `json:"resourceKind"`
					ResourceName string `json:"resourceName"`
					Recovery     string `json:"recovery"`
				} `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env), errOut.String())
	ce := env.Data.Error
	assert.Equal(t, "INTERRUPTED", ce.Code)
	assert.True(t, ce.Details.Committed)
	assert.Equal(t, "app", ce.Details.ResourceKind)
	assert.Equal(t, appRelease, ce.Details.ResourceName)
	assert.Contains(t, ce.Details.Recovery, "get app 'my-nginx'")
	posts := s.recorded()
	require.Len(t, posts, 1)
	charts, _ := posts[0].body["charts"].([]any)
	chart, _ := charts[0].(map[string]any)
	ann, _ := chart["annotations"].(map[string]any)
	id, _ := ann["cli.rancher.io/request-id"].(string)
	require.NotEmpty(t, id)
	assert.Contains(t, ce.Message, "this request's id is "+id)
	assert.Contains(t, ce.Message, "operation 'helm-operation-abcde' in namespace 'web' may still run Helm")
}

// TestAppWaitEnvelope asserts the decoded envelope of an install and an
// uninstall whose wait succeeded: the status, the revision of an install,
// and the one follow-up command.
func TestAppWaitEnvelope(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	done := opStatus([3]string{"Reconciling", "False", ""}, [3]string{"Stalled", "False", ""})
	t.Run("install", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.completeOnAction(done, func(id string) map[string]any { return appObject("deployed", id, appFirstRevision, "Install complete") })
		stdout, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "--wait", "-o", "json")...)
		require.NoError(t, err, stderr)
		env := decodeApp(t, stdout)
		assert.Equal(t, "deployed", env.Data["status"])
		assert.InDelta(t, appFirstRevision, env.Data["revision"], 0)
		follow, _ := env.Data["followUp"].([]any)
		require.Len(t, follow, 1)
		assert.Contains(t, follow[0], " get app 'my-nginx' --cluster 'prod' --namespace 'web'")
	})
	t.Run("uninstall", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.putApp("deployed")
		s.completeOnAction(done, func(string) map[string]any { return nil })
		stdout, stderr, err := appRun(t, s, withArgs(appUninstallArgs, "--apply", "--yes", "--wait", "-o", "json")...)
		require.NoError(t, err, stderr)
		env := decodeApp(t, stdout)
		assert.Equal(t, "uninstalled", env.Data["status"])
		assert.NotContains(t, env.Data, "revision")
		assert.Equal(t, []any{}, env.Data["followUp"], "an App that is gone has no get app")
	})
	t.Run("uninstall keeping its history", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.putApp("deployed")
		s.completeOnAction(done, func(string) map[string]any {
			return appObject("uninstalled", "", appFirstRevision, "Uninstallation complete")
		})
		stdout, stderr, err := appRun(t, s, withArgs(appUninstallArgs, "--keep-history", "--apply", "--yes", "--wait", "-o", "json")...)
		require.NoError(t, err, stderr)
		env := decodeApp(t, stdout)
		assert.Equal(t, "uninstalled", env.Data["status"])
		follow, _ := env.Data["followUp"].([]any)
		require.Len(t, follow, 1)
		assert.Contains(t, follow[0], " get app 'my-nginx' --cluster 'prod' --namespace 'web'")
	})
	t.Run("operation step skipped", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.allowed = false
		s.put(appRepoPath(appRepo), map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "ClusterRepo",
			"metadata": map[string]any{"name": appRepo, "resourceVersion": "1"},
			"spec":     map[string]any{"url": "https://charts.example.test", "serviceAccount": "helm", "serviceAccountNamespace": "tools"},
		})
		s.completeOnAction(done, func(id string) map[string]any { return appObject("deployed", id, appFirstRevision, "Install complete") })
		stdout, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "--wait", "-o", "json")...)
		require.NoError(t, err, stderr)
		env := decodeApp(t, stdout)
		skipped := 0
		for _, w := range env.Warnings {
			if strings.HasPrefix(w, "you cannot get operations in namespace 'web'; the wait reads the app only") {
				skipped++
			}
		}
		assert.Equal(t, 1, skipped, "the warning appears once: %v", env.Warnings)
		assert.Zero(t, s.getCount(appOperationPath(appNS, appOpName)))
	})
}

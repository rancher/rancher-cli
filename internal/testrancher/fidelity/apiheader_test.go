package fidelity

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApiSendsTheAdminTokenOffTheCommandLine asserts that the function api of
// the script, run against a real curl and a local server, sends the admin token
// in the Authorization header of every call, with the body of a call that takes
// it from stdin, and that the token is on no argument list and in no file: a
// wrapper in front of curl records each argument list, each call carries a
// -H argument that names a pipe, and nothing under the test's directory holds
// the token. The environment of the run is an exact list, so no curl
// configuration file and no proxy variable can change the result.
func TestApiSendsTheAdminTokenOffTheCommandLine(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	const token = "api-header-secret-4b9d1e7c"

	type request struct{ method, authorization, contentType, body string }
	var mu sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, request{r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o700))
	argvFile := filepath.Join(dir, "argv")
	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, nil, 0o600))
	require.NoError(t, os.WriteFile(argvFile, nil, 0o600))
	testfs.WriteExecutable(t, filepath.Join(binDir, "curl"), []byte(`#!/bin/sh
{ echo call; printf '%s\n' "$@"; } >> "$STUB_ARGV"
exec "$REAL_CURL" "$@"
`))

	program := "set -euo pipefail\n" +
		shellFunction(t, "api") +
		`api "` + server.URL + `/get"` + "\n" +
		`api -X POST -d @- "` + server.URL + `/post" <<JSON` + "\n" +
		`{"name":"value"}` + "\n" +
		"JSON\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", program)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"ADMIN_TOKEN=" + token,
		"CA_FILE=" + caFile,
		"TMPDIR=" + dir,
		"REAL_CURL=" + realCurl,
		"STUB_ARGV=" + argvFile,
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "output: %s", out)

	raw, err := os.ReadFile(argvFile)
	require.NoError(t, err)
	var calls [][]string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "call" {
			calls = append(calls, nil)
			continue
		}
		require.NotEmpty(t, calls, "an argument before the first call: %q", line)
		calls[len(calls)-1] = append(calls[len(calls)-1], line)
	}
	require.Len(t, calls, 2)
	for i, url := range []string{server.URL + "/get", server.URL + "/post"} {
		assert.Contains(t, calls[i], url, "call %d", i+1)
		var headers []string
		for j, arg := range calls[i] {
			if arg == "-H" && j+1 < len(calls[i]) {
				headers = append(headers, calls[i][j+1])
			}
		}
		assert.Truef(t, slices.ContainsFunc(headers, func(h string) bool { return strings.HasPrefix(h, "@/dev/fd/") }), "call %d: no header names a pipe: %v", i+1, headers)
		assert.Contains(t, headers, "Content-Type: application/json", "call %d", i+1)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 2)
	for i, want := range []request{{method: http.MethodGet}, {method: http.MethodPost, body: `{"name":"value"}`}} {
		assert.Equal(t, want.method, requests[i].method)
		assert.Equal(t, "Bearer "+token, requests[i].authorization, "request %d", i+1)
		assert.Equal(t, "application/json", requests[i].contentType, "request %d", i+1)
		assert.Equal(t, want.body, requests[i].body, "request %d", i+1)
	}

	assert.NotContains(t, string(raw), token)
	assert.NotContains(t, string(raw), "Authorization")
	require.NoError(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assert.NotContains(t, string(content), token, "the token is in %s", path)
		return nil
	}))
}

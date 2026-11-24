package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	pempkg "encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/require"
)

// fuzzPlanted makes a secret of fixed length and pattern: "rcfz-" and 32 lower-case hex digits.
func fuzzPlanted(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(tb, err)
	return "rcfz-" + hex.EncodeToString(b)
}

// fuzzSeedCachedHit writes a config with prod as its current server, and the credentials the exec
// plugin serves for the cluster c-1 without a request: the plugin's, under the name prod, and the
// legacy token command's, for the user kube. Both hold token.
func fuzzSeedCachedHit(tb testing.TB, dir, server, ca, token string) {
	tb.Helper()
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: server, CA: ca, CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(tb, cfg.Write())
	store := config.NewFileStore(dir)
	canon := config.CanonicalDir(dir)
	identity := config.CanonicalServerIdentity(server)
	expiry := time.Now().Add(24 * time.Hour)
	plugin := config.CacheKey("prod", identity, "c-1")
	require.NoError(tb, store.SetCachedCred(plugin, token, expiry, "prod", server, "c-1",
		config.CacheBindingForSeedOf(canon, cfg, "prod", server, "c-1", "", "")))
	shim := config.LegacyCachePrefix + "kube"
	require.NoError(tb, store.SetCachedCred(config.CacheKeyFor(shim, identity, "c-1", "", ""), token, expiry, shim, server, "c-1",
		config.CacheBindingForServer(canon, cfg, server, true)))
}

// fuzzPluginLine builds the line a form names: the exec plugin's flags, or the legacy token
// command's, with each value in the next word or after "=". An empty field leaves its flag out.
func fuzzPluginLine(form uint8, server, cluster, name, userID, authProvider, cacert string, extra []string) []string {
	var line []string
	flag := func(name, value string, equals bool) {
		switch {
		case value == "":
		case equals:
			line = append(line, name+"="+value)
		default:
			line = append(line, name, value)
		}
	}
	equals := form%4 == 1 || form%4 == 2
	if form%4 < 2 {
		line = []string{"auth", "get-token"}
		flag("--server", server, equals)
		flag("--cluster", cluster, equals)
		flag("--name", name, equals)
		flag("--user-id", userID, equals)
		flag("--auth-provider", authProvider, equals)
		flag("--cacert", cacert, equals)
	} else {
		line = []string{"token"}
		flag("--server", server, equals)
		flag("--user", name, equals)
		flag("--cluster", cluster, equals)
		flag("--cacerts", cacert, equals)
	}
	return append(line, extra...)
}

// fuzzHasServerWord reports whether a word is a spelling of --server.
func fuzzHasServerWord(words []string) bool {
	for _, w := range words {
		name, _, _ := strings.Cut(w, "=")
		if name == "--server" || name == "-server" {
			return true
		}
	}
	return false
}

// fuzzExecCredential is the part of an ExecCredential the plugin's output is checked for.
type fuzzExecCredential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		Token string `json:"token"`
	} `json:"status"`
}

// FuzzExecPluginInput fuzzes what kubectl hands the exec plugin: its flags, the
// KUBERNETES_EXEC_INFO document and the token variable. The plugin line runs through run, as the
// process runs it, against a fake server on the machine, with no terminal. Never call
// t.Parallel() here: it panics inside f.Fuzz.
func FuzzExecPluginInput(f *testing.F) {
	dir := fuzzHermetic(f)
	// A mux in front of the facade redirects a path with a double slash, as Rancher's server does
	// for the address typed with a trailing slash.
	mux := http.NewServeMux()
	mux.Handle("/", testrancher.New(testrancher.Options{}))
	// An exec cluster entry in KUBERNETES_EXEC_INFO names the fake server as the cluster endpoint,
	// and the plugin probes it with a SelfSubjectReview before it hands a token to kubectl; an
	// endpoint that never authenticates the token keeps the plugin waiting for its whole bound.
	mux.HandleFunc("POST /apis/authentication.k8s.io/v1/selfsubjectreviews", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"fuzz"}}}`))
	})
	facade := httptest.NewTLSServer(mux)
	f.Cleanup(facade.Close)
	pem := string(pempkg.EncodeToMemory(&pempkg.Block{Type: "CERTIFICATE", Bytes: facade.Certificate().Raw}))
	cached, envToken, execToken, execKey := fuzzPlanted(f), fuzzPlanted(f), fuzzPlanted(f), fuzzPlanted(f)
	secrets := []string{cached, envToken, execToken, execKey}
	fuzzSeedCachedHit(f, dir, facade.URL, pem, cached)
	servers := []string{facade.URL, facade.URL + "/", "http://127.0.0.1:1", ""}
	root, err := BuildRoot("v-fuzz")
	require.NoError(f, err)

	execDocument := func(spec, status string) string {
		doc := `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential"`
		if spec != "" {
			doc += `,"spec":` + spec
		}
		if status != "" {
			doc += `,"status":` + status
		}
		return doc + "}"
	}
	caData := base64.StdEncoding.EncodeToString([]byte(pem))
	clusterJSON := `{"server":"` + facade.URL + `","certificate-authority-data":"` + caData + `"}`
	cluster := `{"cluster":` + clusterJSON + `}`
	execInfos := []string{
		"",
		execDocument(`{"interactive":false}`, ""),
		strings.Replace(execDocument(cluster, ""), "/v1\"", "/v1beta1\"", 1),
		strings.Replace(execDocument(`{"interactive":false}`, ""), "/v1\"", "/v1beta1\"", 1),
		execDocument(cluster, ""),
		"null",
		`{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecC`,
		execDocument("", `{"token":"`+execToken+`","clientKeyData":"`+execKey+`"}`),
	}
	for _, withToken := range []bool{false, true} {
		for server := range uint8(2) {
			for _, info := range execInfos {
				f.Add(uint8(0), server, withToken, "c-1", "prod", "", "", "", "", info)
				f.Add(uint8(1), server, withToken, "c-1", "prod", "", "", "", "", info)
				f.Add(uint8(2), server, withToken, "c-1", "kube", "", "", "", "", info)
				f.Add(uint8(3), server, withToken, "c-1", "kube", "", "", "", "", info)
				f.Add(uint8(0), server, withToken, "c-m-abcde", "ex", "u-abcde", "keycloakoidc", pem, "", info)
				f.Add(uint8(2), server, withToken, "c-m-abcde", "user-abcde", "", "", "/parse/ca.pem", "", info)
			}
		}
	}

	f.Fuzz(func(t *testing.T, form, server uint8, withToken bool, cluster, name, userID, authProvider, cacert, extraBlob, execInfo string) {
		fields := []string{cluster, name, userID, authProvider, cacert, extraBlob, execInfo}
		for _, field := range fields {
			if strings.Contains(field, "rcfz-") {
				t.Skip("a planted secret in a field")
			}
			if field != extraBlob && strings.Contains(field, "\x00") {
				t.Skip("a NUL byte in a field that is not a word list")
			}
			if len(field) > 16<<10 {
				t.Skip("field over 16 KiB")
			}
		}
		extra := strings.Split(extraBlob, "\x00")
		if extraBlob == "" {
			extra = nil
		}
		if fuzzHasServerWord(extra) || fuzzHasConfigWord(extra) {
			t.Skip("a --server or --config word")
		}
		if form%4 < 2 && cacert != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(cacert)) {
			t.Skip("a CA text with no certificate in it ends in a plain error")
		}
		line := fuzzPluginLine(form, servers[int(server)%len(servers)], cluster, name, userID, authProvider, cacert, extra)
		if fuzzHasConfigWord(line) {
			t.Skip("--config")
		}

		if withToken {
			t.Setenv(config.EnvToken, envToken)
		} else {
			t.Setenv(config.EnvToken, "")
			require.NoError(t, os.Unsetenv(config.EnvToken))
		}
		var doc map[string]any
		if json.Unmarshal([]byte(execInfo), &doc) == nil && doc != nil {
			if status, ok := doc["status"].(map[string]any); ok {
				if _, ok := status["token"]; ok {
					status["token"] = execToken
				}
				if _, ok := status["clientKeyData"]; ok {
					status["clientKeyData"] = execKey
				}
				if b, err := json.Marshal(doc); err == nil {
					execInfo = string(b)
				}
			}
		}
		if spec, ok := doc["spec"].(map[string]any); ok {
			if c, ok := spec["cluster"]; ok {
				// A cluster entry names the endpoint kubectl talks to. Only the fake server,
				// reached with its own certificate, is allowed: any other entry could send a
				// request to another host or to the process's default transport.
				m, ok := c.(map[string]any)
				if !ok || len(m) != 2 || m["server"] != facade.URL || m["certificate-authority-data"] != caData {
					t.Skip("a cluster entry that names another endpoint")
				}
			}
		}
		t.Setenv("KUBERNETES_EXEC_INFO", execInfo)
		if execInfo == "" {
			require.NoError(t, os.Unsetenv("KUBERNETES_EXEC_INFO"))
		}

		var stdout, stderr bytes.Buffer
		root.Writer, root.ErrWriter, root.Reader = &stdout, &stderr, strings.NewReader("")
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		res := run(ctx, root, append([]string{"rancher"}, line...))

		if res.code == 0 {
			var cred fuzzExecCredential
			dec := json.NewDecoder(strings.NewReader(stdout.String()))
			require.NoError(t, dec.Decode(&cred), "rancher %q exited 0 with stdout %q", line, stdout.String())
			_, err := dec.Token()
			require.ErrorIs(t, err, io.EOF, "rancher %q wrote more than one document: %q", line, stdout.String())
			require.Contains(t, []string{"client.authentication.k8s.io/v1", "client.authentication.k8s.io/v1beta1"}, cred.APIVersion)
			require.Equal(t, "ExecCredential", cred.Kind)
			require.NotEmpty(t, cred.Status.Token, "rancher %q: no token in %q", line, stdout.String())
		} else {
			require.Empty(t, stdout.String(), "rancher %q exited %d and wrote to stdout", line, res.code)
			ce, ok := clierrOf(res.err)
			require.True(t, ok, "rancher %q exited %d without a *clierr.Error: %v", line, res.code, res.err)
			// A connection that cannot be made (the unused address, or no free local port while
			// the fuzz run opens thousands of connections) ends in a plain error, which resolveExit
			// wraps as GENERIC; every other failure has a typed code.
			if !strings.Contains(res.err.Error(), "dial tcp") {
				require.NotEqual(t, clierr.CodeGeneric, ce.Code, "rancher %q exited %d with a GENERIC error: %v", line, res.code, res.err)
			}
		}
		text := stderr.String()
		if res.err != nil {
			text += "\n" + res.err.Error()
			if ce, ok := clierrOf(res.err); ok {
				text += "\n" + ce.Message + "\n" + ce.Hint
			}
		}
		for _, secret := range secrets {
			require.NotContains(t, text, secret, "rancher %q repeated a planted secret", line)
		}
		_, statErr := os.Stat(filepath.Join(dir, "telemetry", "events"))
		require.True(t, os.IsNotExist(statErr), "rancher %q wrote a telemetry event", line)
		for _, h := range httpx.AnsweredHosts() {
			require.Equal(t, facade.URL, h.Origin, "rancher %q sent a request to a host other than the fake server", line)
		}
	})
}

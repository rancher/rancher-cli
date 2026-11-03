package kubeconfig

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/ui"
)

// refusedRunes are the 35 characters the YAML library refuses when they
// appear raw (it changes U+0085 into a space): DEL, U+0080 to U+009F, U+FFFE
// and U+FFFF.
var refusedRunes = func() []rune {
	rs := []rune{0x7f}
	for r := rune(0x80); r <= 0x9f; r++ {
		rs = append(rs, r)
	}
	return append(rs, 0xfffe, 0xffff)
}()

// neighbourRunes are code points beside the set that the library accepts.
var neighbourRunes = []rune{0x7e, 0xa0, 0xfffd, 0x4e16, 0x1f600}

// fullMarker holds the 35 refused characters and the five neighbours.
func fullMarker() string {
	return string(refusedRunes) + string(neighbourRunes)
}

// assertNoRefusedRune asserts none of the 35 characters is in raw as itself.
func assertNoRefusedRune(t *testing.T, raw []byte) {
	t.Helper()
	for _, r := range refusedRunes {
		assert.False(t, bytes.ContainsRune(raw, r), "%U is written raw", r)
	}
}

// clearSessionEnv removes the variables that choose the server, the CA, the
// token and the store: a row sees only its own config directory.
func clearSessionEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCACert, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
}

// runKubeconfig runs the command for the cluster id in dir, with a file
// store and a prompter that records the notices.
func runKubeconfig(t *testing.T, dir string, f stubFactory, in map[string]string) (runtime.Result, string, error) {
	t.Helper()
	var notices bytes.Buffer
	p := ui.New(&notices, nil, true, false)
	res, err := newTestCommand(dir, config.NewFileStore(dir), f).(runtime.ReadCommand).Execute(t.Context(), execInput{s: in}, p)
	return res, notices.String(), err
}

// writeServers writes a config.yaml that holds servers, with current as the
// current server, and returns its directory. Config.Write stores any value.
func writeServers(t *testing.T, servers map[string]config.Server, current string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{CurrentServer: current, Servers: servers}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

// wantKubeconfig is what a kubeconfig the command wrote must hold: the
// proxy entry name (the name of the proxy context, its cluster and the one
// user), the current context, the server of every context (a context and
// its cluster share a name), and the value of the exec entry's --server
// argument when it is set.
type wantKubeconfig struct {
	proxy     string
	current   string
	servers   map[string]string
	serverArg string
}

// argAfter returns the argument that follows flag in args, and whether flag
// is there.
func argAfter(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// verifyWritten asserts the bytes the command wrote hold none of the 35
// characters raw and read back as want through clientcmd.Load and through the
// loader kubectl uses (a file read with an explicit path), and returns the
// exec entry.
func verifyWritten(t *testing.T, res runtime.Result, want wantKubeconfig) *clientcmdapi.ExecConfig {
	t.Helper()
	require.NotEmpty(t, res.RawBytes)
	assertNoRefusedRune(t, res.RawBytes)

	kc := loadKubeconfig(t, res)
	assert.Equal(t, want.current, kc.CurrentContext)
	servers := map[string]string{}
	for name, c := range kc.Clusters {
		servers[name] = c.Server
	}
	assert.Equal(t, want.servers, servers)
	require.Len(t, kc.Contexts, len(want.servers))
	for name := range want.servers {
		c := kc.Contexts[name]
		require.NotNil(t, c, "the context %q", name)
		assert.Equal(t, name, c.Cluster)
		assert.Equal(t, want.proxy, c.AuthInfo)
	}
	require.Len(t, kc.AuthInfos, 1)
	user := kc.AuthInfos[want.proxy]
	require.NotNil(t, user, "the user %q", want.proxy)
	require.NotNil(t, user.Exec)
	name, ok := argAfter(user.Exec.Args, "--name")
	assert.True(t, ok)
	assert.Equal(t, want.proxy, name)
	if want.serverArg != "" {
		server, ok := argAfter(user.Exec.Args, "--server")
		assert.True(t, ok)
		assert.Equal(t, want.serverArg, server)
	}

	file := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(file, res.RawBytes, 0o600))
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: file},
		&clientcmd.ConfigOverrides{CurrentContext: want.proxy})
	raw, err := loader.RawConfig()
	require.NoError(t, err)
	assert.Equal(t, want.current, raw.CurrentContext)
	rawServers := map[string]string{}
	for name, c := range raw.Clusters {
		rawServers[name] = c.Server
	}
	assert.Equal(t, want.servers, rawServers)
	assert.ElementsMatch(t, slices.Collect(maps.Keys(want.servers)), slices.Collect(maps.Keys(raw.Contexts)))
	require.NotNil(t, raw.AuthInfos[want.proxy])
	assert.Equal(t, user.Exec.Args, raw.AuthInfos[want.proxy].Exec.Args)
	rest, err := loader.ClientConfig()
	require.NoError(t, err)
	require.NotNil(t, rest.ExecProvider)
	assert.Equal(t, user.Exec.Args, rest.ExecProvider.Args)
	assert.Equal(t, user.Exec.Env, rest.ExecProvider.Env)
	return user.Exec
}

// proxyServer is the server of the proxy context for cluster id c-1.
const proxyServer = recordsURL + "/k8s/clusters/c-1"

// TestWriteKubeconfigEntryNames asserts a stored entry whose name holds a
// character of the set gives a kubeconfig whose context, cluster, user and
// --name argument carry the name exactly: U+FFFF and U+FFFE (names
// `auth login --name` accepts), and DEL, U+0085 and U+009B (names an older
// config.yaml can hold).
func TestWriteKubeconfigEntryNames(t *testing.T) {
	for _, tc := range []struct{ name, entry string }{
		{"U+FFFF", "a\uffffb"},
		{"U+FFFE", "a\ufffeb"},
		{"DEL, U+0085 and U+009B", "a\u007f\u0085\u009bb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSessionEnv(t)
			dir := writeServers(t, map[string]config.Server{tc.entry: {URL: recordsURL}}, tc.entry)
			res, _, err := runKubeconfig(t, dir, stubFactory{}, map[string]string{"cluster": "c-1"})
			require.NoError(t, err)
			ctx := tc.entry + "-c-1"
			verifyWritten(t, res, wantKubeconfig{proxy: ctx, current: ctx, servers: map[string]string{ctx: proxyServer}, serverArg: recordsURL})
		})
	}
}

// TestWriteKubeconfigNodeContextNames asserts a node whose requested
// hostname holds all 40 characters of the full marker gives a context and a
// cluster named with them exactly, and that context is current.
func TestWriteKubeconfigNodeContextNames(t *testing.T) {
	clearSessionEnv(t)
	dir := writeTestConfig(t, "")
	objects, lists := aceObjects("c-abc", "", testCAPEM(t), controlPlaneNode("n1", "c-abc-"+fullMarker(), "10.0.0.1", true))
	res, _, err := runKubeconfig(t, dir, stubFactory{objects: objects, lists: lists}, map[string]string{"cluster": "c-abc"})
	require.NoError(t, err)
	node := "prod-c-abc-" + fullMarker()
	e := verifyWritten(t, res, wantKubeconfig{
		proxy: "prod-c-abc", current: node,
		servers: map[string]string{"prod-c-abc": recordsURL + "/k8s/clusters/c-abc", node: "https://10.0.0.1:6443"},
	})
	assert.True(t, e.ProvideClusterInfo)
}

// TestWriteKubeconfigDirectEndpointHosts asserts an endpoint FQDN or a node
// address holding any of the 35 characters but DEL (34: validEndpointURL
// accepts them) reaches the cluster's server exactly, and one holding DEL is
// left out with the notice that names the omission.
func TestWriteKubeconfigDirectEndpointHosts(t *testing.T) {
	shapes := []struct {
		name    string
		factory func(t *testing.T, c string) stubFactory
		context string
		server  func(c string) string
	}{
		{"fqdn", func(t *testing.T, c string) stubFactory {
			t.Helper()
			objects, lists := aceObjects("c-abc", "a"+c+"b.example.com", testCAPEM(t))
			return stubFactory{objects: objects, lists: lists}
		}, "prod-c-abc-fqdn", func(c string) string { return "https://a" + c + "b.example.com" }},
		{"node address", func(t *testing.T, c string) stubFactory {
			t.Helper()
			objects, lists := aceObjects("c-abc", "", testCAPEM(t), controlPlaneNode("n1", "c-abc-cp1", "10.0.0."+c+"1", true))
			return stubFactory{objects: objects, lists: lists}
		}, "prod-c-abc-cp1", func(c string) string { return "https://10.0.0." + c + "1:6443" }},
	}
	proxy := recordsURL + "/k8s/clusters/c-abc"
	for _, shape := range shapes {
		for _, r := range refusedRunes {
			t.Run(fmt.Sprintf("%s %U", shape.name, r), func(t *testing.T) {
				clearSessionEnv(t)
				dir := writeTestConfig(t, "")
				res, notices, err := runKubeconfig(t, dir, shape.factory(t, string(r)), map[string]string{"cluster": "c-abc"})
				require.NoError(t, err)
				if r == 0x7f {
					verifyWritten(t, res, wantKubeconfig{proxy: "prod-c-abc", current: "prod-c-abc", servers: map[string]string{"prod-c-abc": proxy}})
					assert.Contains(t, notices, "direct endpoint contexts omitted")
					return
				}
				verifyWritten(t, res, wantKubeconfig{
					proxy: "prod-c-abc", current: shape.context,
					servers: map[string]string{"prod-c-abc": proxy, shape.context: shape.server(string(r))},
				})
			})
		}
	}
}

// TestWriteKubeconfigServerURL asserts a server URL holding any of the 35
// characters but DEL, from the stored entry or from RANCHER_CLI_URL, reaches
// the cluster's server and the exec entry's --server argument exactly (the
// context name of the variable's form is derived from the host), and one
// holding DEL is refused as VALIDATION with nothing written.
func TestWriteKubeconfigServerURL(t *testing.T) {
	refused := func(t *testing.T, res runtime.Result, err error, prefix string) {
		t.Helper()
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, prefix)
		assert.Nil(t, res.RawBytes)
	}
	for _, r := range refusedRunes {
		url := "https://r" + string(r) + ".example.com"
		t.Run(fmt.Sprintf("stored entry %U", r), func(t *testing.T) {
			clearSessionEnv(t)
			dir := writeServers(t, map[string]config.Server{"prod": {URL: url}}, "prod")
			res, _, err := runKubeconfig(t, dir, stubFactory{}, map[string]string{"cluster": "c-1"})
			if r == 0x7f {
				refused(t, res, err, "the server URL in server entry 'prod' cannot be used: ")
				return
			}
			require.NoError(t, err)
			verifyWritten(t, res, wantKubeconfig{
				proxy: "prod-c-1", current: "prod-c-1",
				servers:   map[string]string{"prod-c-1": url + "/k8s/clusters/c-1"},
				serverArg: url,
			})
		})
		t.Run(fmt.Sprintf("RANCHER_CLI_URL %U", r), func(t *testing.T) {
			clearSessionEnv(t)
			dir := writeServers(t, map[string]config.Server{"prod": {URL: recordsURL}}, "prod")
			t.Setenv(config.EnvURL, url)
			res, _, err := runKubeconfig(t, dir, stubFactory{}, map[string]string{"cluster": "c-1"})
			if r == 0x7f {
				refused(t, res, err, "the server URL in RANCHER_CLI_URL cannot be used: ")
				return
			}
			require.NoError(t, err)
			ctx := "r" + string(r) + ".example.com-c-1"
			verifyWritten(t, res, wantKubeconfig{
				proxy: ctx, current: ctx,
				servers:   map[string]string{ctx: url + "/k8s/clusters/c-1"},
				serverArg: url,
			})
		})
	}
}

// TestWriteKubeconfigOwnerHoldingAnUnusableCharacter asserts a stored owner
// that holds U+FFFF (a user id has only lower-case letters, digits, hyphens
// and dots) gives no --user-id and no --auth-provider, the notice that names
// the missing user, and a file that is still written.
func TestWriteKubeconfigOwnerHoldingAnUnusableCharacter(t *testing.T) {
	clearSessionEnv(t)
	dir := writeServers(t, map[string]config.Server{"prod": {URL: recordsURL, TokenUserID: "u-te\uffffst", TokenAuthConfig: "ap"}}, "prod")
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: recordsURL}, "tok"))
	res, notices, err := runKubeconfig(t, dir, stubFactory{}, map[string]string{"cluster": "c-1"})
	require.NoError(t, err)
	e := verifyWritten(t, res, wantKubeconfig{proxy: "prod-c-1", current: "prod-c-1", servers: map[string]string{"prod-c-1": proxyServer}, serverArg: recordsURL})
	assert.NotContains(t, e.Args, "--user-id")
	assert.NotContains(t, e.Args, "--auth-provider")
	assert.Contains(t, notices, "this kubeconfig names no Rancher user")
}

// TestWriteKubeconfigConfigDirectoryName asserts a config directory whose
// name holds DEL, U+0085 and U+009B is written exactly in the exec entry's
// environment. A name with U+FFFE or U+FFFF cannot be created on macOS; the
// writer tests cover them.
func TestWriteKubeconfigConfigDirectoryName(t *testing.T) {
	testfs.SkipOnWindows(t, "a directory name with these characters is not tested on NTFS")
	clearSessionEnv(t)
	dir := filepath.Join(t.TempDir(), "d\u007f\u0085\u009b")
	cfg := prodCurrent(nil)
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res, _, err := runKubeconfig(t, dir, stubFactory{}, map[string]string{"cluster": "c-1"})
	require.NoError(t, err)
	e := verifyWritten(t, res, wantKubeconfig{proxy: "prod-c-1", current: "prod-c-1", servers: map[string]string{"prod-c-1": proxyServer}, serverArg: recordsURL})
	v, ok := envValue(e, config.EnvConfigDir)
	assert.True(t, ok)
	assert.Equal(t, config.CanonicalDir(dir), v)
}

// TestWriteKubeconfigExecCommandPath asserts an --exec-command path that
// holds all five characters DEL, U+0085, U+009B, U+FFFE and U+FFFF (a path
// that is never created) is written exactly as the exec entry's command.
func TestWriteKubeconfigExecCommandPath(t *testing.T) {
	clearSessionEnv(t)
	dir := writeServers(t, map[string]config.Server{"prod": {URL: recordsURL}}, "prod")
	cmdPath := filepath.Join(t.TempDir(), "bin\u007f\u0085\u009b\ufffe\uffff")
	res, _, err := runKubeconfig(t, dir, stubFactory{}, map[string]string{"cluster": "c-1", "exec-command": cmdPath})
	require.NoError(t, err)
	e := verifyWritten(t, res, wantKubeconfig{proxy: "prod-c-1", current: "prod-c-1", servers: map[string]string{"prod-c-1": proxyServer}, serverArg: recordsURL})
	assert.Equal(t, cmdPath, e.Command)
}

// namedConfig builds the shape the command builds, with name as the proxy
// context, its cluster and its user, and as an argument of the exec entry.
func namedConfig(name string) clientcmdapi.Config {
	return clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{name: {Server: recordsURL + "/k8s/clusters/c-1"}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{name: {Exec: &clientcmdapi.ExecConfig{
			APIVersion:      "client.authentication.k8s.io/v1beta1",
			Command:         "rancher",
			Args:            []string{"auth", "get-token", "--name", name},
			Env:             []clientcmdapi.ExecEnvVar{{Name: config.EnvConfigDir, Value: "/d"}},
			InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
		}}},
		Contexts:       map[string]*clientcmdapi.Context{name: {Cluster: name, AuthInfo: name}},
		CurrentContext: name,
	}
}

// TestWriteKubeconfigWritesEachCharacterAlone asserts each of the 35
// characters alone, all 35 together and the five neighbours, as a<c>b in the
// name of a context, a cluster and a user, are written with none of the 35
// raw and read back exactly by clientcmd.Load.
func TestWriteKubeconfigWritesEachCharacterAlone(t *testing.T) {
	cases := map[string]string{"all 35": string(refusedRunes)}
	for _, r := range slices.Concat(refusedRunes, neighbourRunes) {
		cases[fmt.Sprintf("%U", r)] = string(r)
	}
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		t.Run(name, func(t *testing.T) {
			entry := "a" + cases[name] + "b"
			b, err := writeKubeconfig(namedConfig(entry))
			require.NoError(t, err)
			assertNoRefusedRune(t, b)
			kc, err := clientcmd.Load(b)
			require.NoError(t, err)
			assert.Equal(t, entry, kc.CurrentContext)
			assert.Equal(t, []string{entry}, slices.Collect(maps.Keys(kc.Clusters)))
			assert.Equal(t, []string{entry}, slices.Collect(maps.Keys(kc.AuthInfos)))
			assert.Equal(t, []string{entry}, slices.Collect(maps.Keys(kc.Contexts)))
			assert.Equal(t, entry, kc.Contexts[entry].Cluster)
			assert.Equal(t, entry, kc.Contexts[entry].AuthInfo)
			assert.Equal(t, []string{"auth", "get-token", "--name", entry}, kc.AuthInfos[entry].Exec.Args)
		})
	}
}

// TestWriteKubeconfigMatchesClientcmdWriteThroughTheCommand asserts the
// bytes the command writes for values that hold none of the 35 characters
// equal what clientcmd.Write returns for the kubeconfig read back from them;
// TestWriteKubeconfigReadsBackEveryField covers a field the writer leaves out.
func TestWriteKubeconfigMatchesClientcmdWriteThroughTheCommand(t *testing.T) {
	ca := testCAPEM(t)
	rows := []struct {
		name string
		// setup returns the config directory, the factory and the cluster id.
		setup func(t *testing.T) (dir string, f stubFactory, cluster string)
		// provideClusterInfo is the value of the exec entry's provideClusterInfo.
		provideClusterInfo bool
	}{
		{name: "proxy only", setup: func(t *testing.T) (string, stubFactory, string) {
			t.Helper()
			return writeTestConfig(t, ""), stubFactory{}, "c-1"
		}},
		{name: "CA data and --cacert", setup: func(t *testing.T) (string, stubFactory, string) {
			t.Helper()
			return writeTestConfig(t, string(ca)), stubFactory{}, "c-1"
		}},
		{name: "fqdn context", provideClusterInfo: true, setup: func(t *testing.T) (string, stubFactory, string) {
			t.Helper()
			objects, lists := aceObjects("c-abc", "ace.example.test", ca)
			return writeTestConfig(t, ""), stubFactory{objects: objects, lists: lists}, "c-abc"
		}},
		{name: "two node contexts", provideClusterInfo: true, setup: func(t *testing.T) (string, stubFactory, string) {
			t.Helper()
			objects, lists := aceObjects("c-abc", "", ca,
				controlPlaneNode("n1", "c-abc-cp1", "10.0.0.1", true), controlPlaneNode("n2", "c-abc-cp2", "10.0.0.2", false))
			return writeTestConfig(t, ""), stubFactory{objects: objects, lists: lists}, "c-abc"
		}},
		{name: "user id and auth config", setup: func(t *testing.T) (string, stubFactory, string) {
			t.Helper()
			dir := writeServers(t, map[string]config.Server{"prod": {URL: recordsURL, TokenUserID: "u-test", TokenAuthConfig: "ap"}}, "prod")
			require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: recordsURL}, "tok"))
			return dir, stubFactory{}, "c-1"
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSessionEnv(t)
			dir, f, cluster := row.setup(t)
			res, _, err := runKubeconfig(t, dir, f, map[string]string{"cluster": cluster})
			require.NoError(t, err)
			kc := loadKubeconfig(t, res)
			for _, user := range kc.AuthInfos {
				assert.Equal(t, row.provideClusterInfo, user.Exec.ProvideClusterInfo)
			}
			want, err := clientcmd.Write(*kc)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(res.RawBytes))
		})
	}
}

// realisticConfig holds every kind of entry a kubeconfig can carry that the
// command does not build: a token user, an auth provider, impersonation, a
// proxy URL, a TLS server name, an entry with no field set and preferences.
func realisticConfig() clientcmdapi.Config {
	return clientcmdapi.Config{
		Preferences: clientcmdapi.Preferences{Colors: true},
		Clusters: map[string]*clientcmdapi.Cluster{
			"a": {Server: "https://a.example.com", CertificateAuthorityData: []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"),
				TLSServerName: "tls.example.com", ProxyURL: "http://proxy.example.com:3128", DisableCompression: true},
			"b": {Server: "https://b.example.com", InsecureSkipTLSVerify: true},
			"c": {},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"exec": {Exec: &clientcmdapi.ExecConfig{
				APIVersion:         "client.authentication.k8s.io/v1beta1",
				Command:            "rancher",
				Args:               []string{"auth", "get-token", "", "a: b", "- c", "#d", "yes", "null", "1e3", "x\ty", "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"},
				Env:                []clientcmdapi.ExecEnvVar{{Name: "E", Value: ""}, {Name: "DIR", Value: `C:\Users\u\.rancher`}},
				InstallHint:        "install it:\n<https://example.com/?a=1&b=2>",
				InteractiveMode:    clientcmdapi.IfAvailableExecInteractiveMode,
				ProvideClusterInfo: true,
			}},
			"token": {Token: "tok", Impersonate: "someone", ImpersonateUID: "uid-1", ImpersonateGroups: []string{"g1", "g2"},
				ImpersonateUserExtra: map[string][]string{"scopes": {"s1", "s2"}}},
			"provider": {AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "oidc", Config: map[string]string{"client-id": "x", "idp-issuer-url": "https://i.example.com"}}},
			"basic":    {Username: "u", Password: "p", ClientCertificateData: []byte("cert"), ClientKeyData: []byte("key")},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"x": {Cluster: "a", AuthInfo: "exec", Namespace: "kube-system"},
			"y": {Cluster: "b", AuthInfo: "token"},
			"z": {},
		},
		CurrentContext: "x",
	}
}

// TestWriteKubeconfigMatchesClientcmdWriteForHandBuiltValues asserts the
// bytes for a value that holds none of the 35 characters equal what
// clientcmd.Write returns, for shapes the command cannot build: an empty
// config, one that carries every kind of entry, and one with every field set.
func TestWriteKubeconfigMatchesClientcmdWriteForHandBuiltValues(t *testing.T) {
	var filled clientcmdapi.Config
	f := &configFiller{t: t, marker: "plain", fields: map[string]bool{}}
	f.fill(reflect.ValueOf(&filled).Elem(), configRoot)
	for name, kc := range map[string]clientcmdapi.Config{
		"empty":               {},
		"empty with maps":     *clientcmdapi.NewConfig(),
		"every kind of entry": realisticConfig(),
		"every field set":     filled,
		"command built shape": namedConfig("prod-c-1"),
	} {
		t.Run(name, func(t *testing.T) {
			want, err := clientcmd.Write(kc)
			require.NoError(t, err)
			got, err := writeKubeconfig(kc)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
		})
	}
}

// configRoot is the path of the root of the value the reflection test fills.
const configRoot = "Config"

// kubeconfigExempt lists the fields of clientcmdapi.Config the reflection
// test leaves at their zero value, each with the reason. A path is the Go
// field names joined by dots; a map value adds {} and a map key adds {key}.
var kubeconfigExempt = map[string]string{
	"Config.Kind":                   "the writer sets kind to Config; the conversion does not carry the internal value",
	"Config.APIVersion":             "the writer sets apiVersion to v1; the conversion does not carry the internal value",
	"Config.Extensions":             "a runtime.Object the fill cannot build; the command writes none",
	"Config.Preferences.Extensions": "a runtime.Object the fill cannot build; the command writes none",
	"Config.Clusters{}.Extensions":  "a runtime.Object the fill cannot build; the command writes none",
	"Config.AuthInfos{}.Extensions": "a runtime.Object the fill cannot build; the command writes none",
	"Config.Contexts{}.Extensions":  "a runtime.Object the fill cannot build; the command writes none",
}

func jsonName(sf reflect.StructField) string {
	name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
	return name
}

// configFiller sets every field of a value to a string that holds a marker,
// and records the path of every field it reaches.
type configFiller struct {
	t      *testing.T
	marker string
	fields map[string]bool
	marked int
}

func (f *configFiller) fill(v reflect.Value, path string) {
	f.t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if !sf.IsExported() || jsonName(sf) == "-" {
				continue
			}
			p := path + "." + sf.Name
			require.NotEmpty(f.t, jsonName(sf), "the field %s has no json name; the fill cannot tell what is written", p)
			f.fields[p] = true
			if _, ok := kubeconfigExempt[p]; ok {
				continue
			}
			f.fill(v.Field(i), p)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(v.Elem(), path)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte(path + f.marker))
			f.marked++
			return
		}
		s := reflect.MakeSlice(v.Type(), 1, 1)
		f.fill(s.Index(0), path+"[]")
		v.Set(s)
	case reflect.Map:
		require.Equal(f.t, reflect.String, v.Type().Key().Kind(), "the map at %q has a key the fill cannot reach", path)
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		k.SetString(path + "{key}" + f.marker)
		f.marked++
		e := reflect.New(v.Type().Elem()).Elem()
		f.fill(e, path+"{}")
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString(path + f.marker)
		f.marked++
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Bool:
		v.SetBool(true)
	default:
		require.Failf(f.t, "a field the fill cannot reach", "the field %q has kind %s; extend the fill for it", path, v.Kind())
	}
}

// configEntry is one string, byte slice, map key, bool or integer of a
// value, with whether it is text.
type configEntry struct {
	value string
	text  bool
}

// collectEntries records every entry of v by its path, leaving out the
// exempt fields.
func collectEntries(v reflect.Value, path string, into map[string]configEntry) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if !sf.IsExported() || jsonName(sf) == "-" {
				continue
			}
			p := path + "." + sf.Name
			if _, ok := kubeconfigExempt[p]; ok {
				continue
			}
			collectEntries(v.Field(i), p, into)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			collectEntries(v.Elem(), path, into)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			into[path] = configEntry{value: string(v.Bytes()), text: true}
			return
		}
		for i := range v.Len() {
			collectEntries(v.Index(i), path+"[]", into)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			into[path+"{key}"] = configEntry{value: k.String(), text: true}
			collectEntries(v.MapIndex(k), path+"{}", into)
		}
	case reflect.String:
		into[path] = configEntry{value: v.String(), text: true}
	default:
		into[path] = configEntry{value: fmt.Sprint(v.Interface())}
	}
}

// fillAndWrite fills a clientcmdapi.Config with marker and writes it, and
// returns the value, the bytes and the entries of the value.
func fillAndWrite(t *testing.T, marker string) (clientcmdapi.Config, []byte, map[string]configEntry, int) {
	t.Helper()
	var kc clientcmdapi.Config
	f := &configFiller{t: t, marker: marker, fields: map[string]bool{}}
	f.fill(reflect.ValueOf(&kc).Elem(), configRoot)
	b, err := writeKubeconfig(kc)
	require.NoError(t, err)
	in := map[string]configEntry{}
	collectEntries(reflect.ValueOf(kc), configRoot, in)
	return kc, b, in, f.marked
}

// assertFieldsReadBack writes a value with every field marked and asserts
// the entries read back through clientcmd.Load are the entries written, that
// every text entry holds the marker, and that the bytes hold none of the 35
// characters raw.
func assertFieldsReadBack(t *testing.T, marker string) int {
	t.Helper()
	_, b, in, marked := fillAndWrite(t, marker)
	assertNoRefusedRune(t, b)
	for p, e := range in {
		if e.text {
			assert.Contains(t, e.value, marker, "the entry at %q is not marked; the fill missed it", p)
		}
	}
	got, err := clientcmd.Load(b)
	require.NoError(t, err)
	out := map[string]configEntry{}
	collectEntries(reflect.ValueOf(*got), configRoot, out)
	assert.Equal(t, in, out)
	return marked
}

// TestWriteKubeconfigReadsBackEveryField asserts every string, byte slice
// and map key of clientcmdapi.Config that the conversion writes is read back
// as it was filled, for the full marker and for each of the 35 characters
// alone (36 writes), and that a value of ordinary text is written with the
// bytes clientcmd.Write returns. A field of a kind the fill cannot set fails
// the test.
func TestWriteKubeconfigReadsBackEveryField(t *testing.T) {
	t.Run("full marker", func(t *testing.T) {
		marked := assertFieldsReadBack(t, fullMarker())
		t.Logf("the fill marked %d strings, byte slices and map keys", marked)
		assert.Positive(t, marked)
	})
	for _, r := range refusedRunes {
		t.Run(fmt.Sprintf("%U alone", r), func(t *testing.T) {
			assertFieldsReadBack(t, string(r))
		})
	}
	t.Run("ordinary text", func(t *testing.T) {
		kc, b, _, _ := fillAndWrite(t, "plain")
		want, err := clientcmd.Write(kc)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(b))
	})
}

// TestWriteKubeconfigExemptTableNamesExistingFields asserts every exempt
// path is a field of clientcmdapi.Config.
func TestWriteKubeconfigExemptTableNamesExistingFields(t *testing.T) {
	var kc clientcmdapi.Config
	f := &configFiller{t: t, marker: "m", fields: map[string]bool{}}
	f.fill(reflect.ValueOf(&kc).Elem(), configRoot)
	for p, reason := range kubeconfigExempt {
		assert.True(t, f.fields[p], "the exempt table names %q, which the type does not have", p)
		assert.NotEmpty(t, reason, "the exempt row %q has no reason", p)
	}
}

// TestWriteKubeconfigWritesItsOwnKindAndAPIVersion asserts a kind and an
// apiVersion set to the marker are not written: the file holds Config and v1,
// and the value read back holds neither. The fill leaves them out for that
// reason.
func TestWriteKubeconfigWritesItsOwnKindAndAPIVersion(t *testing.T) {
	b, err := writeKubeconfig(clientcmdapi.Config{CurrentContext: "x", Kind: fullMarker(), APIVersion: fullMarker()})
	require.NoError(t, err)
	got, err := clientcmd.Load(b)
	require.NoError(t, err)
	assert.Empty(t, got.Kind)
	assert.Empty(t, got.APIVersion)
	var doc map[string]any
	require.NoError(t, sigsyaml.Unmarshal(b, &doc))
	assert.Equal(t, "Config", doc["kind"])
	assert.Equal(t, "v1", doc["apiVersion"])
}

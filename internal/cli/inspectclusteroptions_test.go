package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registration token of the node command option runs, and the node
// commands the test server serves with it, in Rancher's shape.
const (
	regOptToken       = "tok-REGOPT-CLI-51d2e8"
	regOptNodeCommand = " curl -fL https://rancher.example/system-agent-install.sh | sudo  sh -s - --server https://rancher.example --label 'cattle.io/os=linux' --token " + regOptToken + " --ca-checksum abc123"
	regOptInsecure    = " curl --insecure -fL https://rancher.example/system-agent-install.sh | sudo  sh -s - --server https://rancher.example --label 'cattle.io/os=linux' --token " + regOptToken
	regOptWindows     = " curl.exe -fL https://rancher.example/wins-agent-install.ps1 -o install.ps1; Set-ExecutionPolicy Bypass -Scope Process -Force; ./install.ps1 -Server https://rancher.example -Label 'cattle.io/os=windows' -Token " + regOptToken + " -Worker"
	regOptWinInsecure = " curl.exe --insecure -fL https://rancher.example/wins-agent-install.ps1 -o install.ps1; Set-ExecutionPolicy Bypass -Scope Process -Force; ./install.ps1 -Server https://rancher.example -Label 'cattle.io/os=windows' -Token " + regOptToken + " -Worker"
	// regOptNodes are the management Nodes of c-abc: an etcd, a control
	// plane and a worker node.
	regOptNodes = `{"apiVersion":"management.cattle.io/v3","kind":"NodeList","items":[` +
		`{"metadata":{"name":"n1","namespace":"c-abc"},"spec":{"etcd":true,"controlPlane":false,"worker":false}},` +
		`{"metadata":{"name":"n2","namespace":"c-abc"},"spec":{"etcd":false,"controlPlane":true,"worker":false}},` +
		`{"metadata":{"name":"n3","namespace":"c-abc"},"spec":{"etcd":false,"controlPlane":false,"worker":true}}]}`
	regOptNodesPath = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/nodes"
	// regOptNoCluster stands for a provisioning list with no Cluster of c-abc.
	regOptNoCluster = "none"
)

// regOptCluster is the management Cluster c-abc as the Steve API returns it.
const regOptCluster = `{"id":"c-abc","type":"management.cattle.io.cluster","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"},` +
	`"status":{"provider":"rke2","info":{"kubernetesVersion":"v1.33.4+rke2r1"},"conditions":[{"type":"Connected","status":"True"},{"type":"Ready","status":"True"}]}}`

// regOptClusterWith is the management Cluster c-abc with provider and, when
// not empty, version as status.info.kubernetesVersion.
func regOptClusterWith(provider, version string) string {
	info := ""
	if version != "" {
		info = `"info":{"kubernetesVersion":"` + version + `"},`
	}
	return `{"id":"c-abc","type":"management.cattle.io.cluster","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"},` +
		`"status":{"provider":"` + provider + `",` + info + `"conditions":[{"type":"Connected","status":"True"},{"type":"Ready","status":"True"}]}}`
}

// ambiguousRow marks the row whose refusal carries the ambiguous selection
// code and its candidates.
const ambiguousRow = "ambiguous"

// regOptAmbiguous is a provisioning Cluster list with two Clusters of c-abc.
const regOptAmbiguous = `{"apiVersion":"provisioning.cattle.io/v1","kind":"ClusterList","items":[` +
	`{"apiVersion":"provisioning.cattle.io/v1","kind":"Cluster","metadata":{"name":"b","namespace":"fleet-default"},"spec":{"rkeConfig":{}},"status":{"clusterName":"c-abc"}},` +
	`{"apiVersion":"provisioning.cattle.io/v1","kind":"Cluster","metadata":{"name":"a","namespace":"fleet-default"},"spec":{"rkeConfig":{}},"status":{"clusterName":"c-abc"}}]}`

// regOptProvisioning is a provisioning Cluster list with one Cluster of
// c-abc; rkeConfig is its spec.rkeConfig, "" for an imported cluster, and
// regOptNoCluster gives a list with no Cluster.
func regOptProvisioning(rkeConfig string) string {
	if rkeConfig == regOptNoCluster {
		return `{"apiVersion":"provisioning.cattle.io/v1","kind":"ClusterList","items":[]}`
	}
	spec := `"kubernetesVersion":"v1.33.4+rke2r1"`
	if rkeConfig != "" {
		spec += `,"rkeConfig":` + rkeConfig
	}
	return `{"apiVersion":"provisioning.cattle.io/v1","kind":"ClusterList","items":[{"apiVersion":"provisioning.cattle.io/v1","kind":"Cluster",` +
		`"metadata":{"name":"my-custom","namespace":"fleet-default"},"spec":{` + spec + `},"status":{"clusterName":"c-abc"}}]}`
}

// regOptTokenBody is the default token of c-abc; nodeCommand and
// windowsNodeCommand replace the secure node commands.
func regOptTokenBody(nodeCommand, windowsNodeCommand string) string {
	status := map[string]string{
		"nodeCommand": nodeCommand, "insecureNodeCommand": regOptInsecure,
		"windowsNodeCommand": windowsNodeCommand, "insecureWindowsNodeCommand": regOptWinInsecure,
		"command":     "curl -sfL https://rancher.example/v3/import/" + regOptToken + ".yaml | kubectl apply -f -",
		"manifestUrl": "https://rancher.example/v3/import/" + regOptToken + ".yaml", "token": regOptToken,
	}
	b, _ := json.Marshal(map[string]any{
		"id": "c-abc/default-token", "type": "management.cattle.io.clusterregistrationtoken",
		"metadata": map[string]any{"namespace": "c-abc", "name": "default-token"}, "spec": map[string]any{"clusterName": "c-abc"},
		"status": status,
	})
	return string(b)
}

// regOptServer points the environment at a test server that serves c-abc
// with rkeConfig as its provisioning spec.rkeConfig and tokenHandler for its
// default token, and returns the config directory and the server.
func regOptServer(t *testing.T, rkeConfig string, tokenHandler http.HandlerFunc) (string, *factsServer) {
	t.Helper()
	dir, srv := factsEnv(t)
	jsonHandler := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	srv.handle("/v1/management.cattle.io.clusters", jsonHandler(`{"type":"collection","revision":"1","count":1,"data":[`+regOptCluster+`]}`))
	srv.handle("/v1/management.cattle.io.clusters/c-abc", jsonHandler(regOptCluster))
	srv.handle("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters", jsonHandler(regOptProvisioning(rkeConfig)))
	srv.handle("/v1/management.cattle.io.clusterregistrationtokens/c-abc/default-token", tokenHandler)
	srv.handle(regOptNodesPath, jsonHandler(regOptNodes))
	return dir, srv
}

// tokenWith returns a handler that responds to the default token read with
// nodeCommand as its secure node command.
func tokenWith(nodeCommand string) http.HandlerFunc { return tokenWithBoth(nodeCommand, regOptWindows) }

// tokenWithBoth returns a handler that responds to the default token read with
// the two secure node commands given.
func tokenWithBoth(nodeCommand, windowsNodeCommand string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(regOptTokenBody(nodeCommand, windowsNodeCommand)))
	}
}

// TestNoNodeCommandOptionOutputCarriesTheToken asserts that no error,
// warning, hint or recorded event of an inspect cluster run with node
// command options carries the registration token or a composed command:
// every refusal and the left-out warning, run through the built command tree
// against a test server, with the recorded events read back as telemetry show
// lists them. NOT parallel: the recording environment is process-wide.
func TestNoNodeCommandOptionOutputCarriesTheToken(t *testing.T) {
	composed := []string{
		regOptNodeCommand + " --worker", regOptInsecure + " --worker",
		regOptNodeCommand + " --worker --label 'tier=db'", regOptInsecure + " --worker --label 'tier=db'",
		regOptWindows + " -NodeName 'win1'", regOptWinInsecure + " -NodeName 'win1'",
	}
	failing := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"type":"error","status":"500","message":"token store unavailable"}`, http.StatusInternalServerError)
	}
	for _, row := range []struct {
		name      string
		args      []string
		rkeConfig string
		token     http.HandlerFunc
		code      string
		message   string
		warning   string
		nodes     http.HandlerFunc
		// cluster replaces the management Cluster; prov replaces the
		// provisioning list response.
		cluster string
		prov    http.HandlerFunc
	}{
		{name: "an option flag without --registration", args: []string{"--worker", "--label", "tier=db"}, rkeConfig: "{}",
			code: "VALIDATION", message: "--worker needs --registration"},
		{name: "a refused value", args: []string{"--registration", "--label", "a b=c"}, rkeConfig: "{}",
			code: "VALIDATION", message: `--label "a b=c": the key has " "`},
		{name: "a label key given twice", args: []string{"--registration", "--label", "tier=a", "--label", "tier=b"}, rkeConfig: "{}",
			code: "VALIDATION", message: "--label tier is given twice"},
		{name: "an empty value", args: []string{"--registration", "--node-name", ""}, rkeConfig: "{}",
			code: "VALIDATION", message: "--node-name is empty; pass a value or omit --node-name"},
		{name: "an imported cluster", args: []string{"--registration", "--worker"}, rkeConfig: "",
			code: "VALIDATION", message: "--worker: node command options apply only to a custom cluster; c-abc is imported"},
		{name: "a cluster with no provisioning Cluster", args: []string{"--registration", "--worker"}, rkeConfig: regOptNoCluster,
			code: "VALIDATION", message: "--worker: node command options apply only to a custom cluster; c-abc has no provisioning.cattle.io Cluster"},
		{name: "a label key Rancher sets", args: []string{"--registration", "--label", "rke.cattle.io/node-name=n1"}, rkeConfig: "{}",
			code: "VALIDATION", message: "--label rke.cattle.io/node-name: Rancher sets this label on the node itself"},
		{name: "a value Kubernetes refuses", args: []string{"--registration", "--node-name", "Node1"}, rkeConfig: "{}",
			code: "VALIDATION", message: `--node-name "Node1": this name breaks the Kubernetes rule for a node name`},
		{name: "a cluster with machine pools", args: []string{"--registration", "--worker"}, rkeConfig: `{"machinePools":[{"name":"p1","quantity":1,"workerRole":true}]}`,
			code: "VALIDATION", message: "--worker: node command options apply only to a custom cluster; c-abc has machine pools"},
		{name: "a failed token read", args: []string{"--registration", "--worker"}, rkeConfig: "{}", token: failing,
			code: "GENERIC", message: "--worker: cannot read the cluster registration token: "},
		{name: "a role flag with --windows", args: []string{"--registration", "--windows", "--worker"}, rkeConfig: "{}",
			code: "VALIDATION", message: "--worker conflicts with --windows; remove --worker (Windows nodes can only be workers)"},
		{name: "a CNI the Windows node command does not take", args: []string{"--registration", "--windows", "--node-name", "win1"},
			rkeConfig: `{"machineGlobalConfig":{"cni":"cilium"}}`, code: "VALIDATION", message: `--windows: the cluster's CNI is "cilium"`},
		{name: "a cluster with no worker node", args: []string{"--registration", "--windows", "--node-name", "win1"}, rkeConfig: "{}",
			nodes: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"NodeList","items":[]}`))
			},
			code: "GENERIC", message: "--windows: the Windows node command is available once the cluster is connected"},
		{name: "a failed Node List", args: []string{"--registration", "--windows", "--node-name", "win1"}, rkeConfig: "{}", nodes: failing,
			code: "GENERIC", message: "--windows: cannot tell whether the cluster accepts Windows nodes: "},
		{name: "a failed token read with --windows", args: []string{"--registration", "--windows", "--node-name", "win1"}, rkeConfig: "{}", token: failing,
			code: "GENERIC", message: "--node-name: cannot read the cluster registration token: "},
		{name: "a failed provisioning lookup with --windows", args: []string{"--registration", "--windows"}, rkeConfig: "{}", prov: failing,
			code: "GENERIC", message: "--windows: cannot tell whether the cluster accepts Windows nodes: "},
		{name: "an ambiguous provisioning lookup with --windows", args: []string{"--registration", "--windows"}, rkeConfig: "{}",
			prov: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(regOptAmbiguous))
			},
			code: ambiguousRow, message: "--windows: cannot tell whether the cluster accepts Windows nodes: 2 provisioning.cattle.io Clusters"},
		{name: "a K3s cluster", args: []string{"--registration", "--windows"}, rkeConfig: "{}", cluster: regOptClusterWith("k3s", "v1.33.4+k3s1"),
			code: "VALIDATION", message: "--windows: the cluster runs K3s"},
		{name: "a Kubernetes version below v1.21.0", args: []string{"--registration", "--windows"}, rkeConfig: "{}", cluster: regOptClusterWith("rke2", "v1.20.15+rke2r1"),
			code: "VALIDATION", message: `--windows: the cluster's Kubernetes version "v1.20.15+rke2r1" is older than v1.21.0`},
		{name: "no Kubernetes version yet", args: []string{"--registration", "--windows"}, rkeConfig: "{}", cluster: regOptClusterWith("rke2", ""),
			code: "GENERIC", message: "--windows: the Windows node command is available once the cluster reports its Kubernetes version"},
		{name: "the Windows left-out warning", args: []string{"--registration", "--windows", "--node-name", "win1"}, rkeConfig: "{}",
			token:   tokenWithBoth(regOptNodeCommand, "curl.exe x; ./install.ps1 -Token {token} -Worker"),
			warning: "registration token c-abc/default-token: windowsNodeCommand not formatted by the server; the node command with options is left out"},
		{name: "the left-out warning", args: []string{"--registration", "--worker", "--label", "tier=db"}, rkeConfig: "{}",
			token:   tokenWith("curl -fL https://rancher.example/system-agent-install.sh | sudo sh -s - --token {token}"),
			warning: "registration token c-abc/default-token: nodeCommand not formatted by the server; the node command with options is left out"},
	} {
		t.Run(row.name, func(t *testing.T) {
			token := row.token
			if token == nil {
				token = tokenWith(regOptNodeCommand)
			}
			dir, srv := regOptServer(t, row.rkeConfig, token)
			if row.nodes != nil {
				srv.handle(regOptNodesPath, row.nodes)
			}
			if row.prov != nil {
				srv.handle("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters", row.prov)
			}
			if row.cluster != "" {
				body := row.cluster
				srv.handle("/v1/management.cattle.io.clusters", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"type":"collection","revision":"1","count":1,"data":[` + body + `]}`))
				})
				srv.handle("/v1/management.cattle.io.clusters/c-abc", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				})
			}
			args := append([]string{"inspect", "cluster", "c-abc", "-o", "json"}, row.args...)
			run := runRecorded(t, newRoot(t), args...)
			assertNoTokenIn(t, "stderr", run.stderr, composed)

			if row.warning != "" {
				require.Zero(t, run.res.code, run.stderr)
				var env struct {
					Warnings []string `json:"warnings"`
				}
				require.NoError(t, json.Unmarshal([]byte(run.stdout), &env))
				assert.Contains(t, env.Warnings, row.warning)
				for _, w := range env.Warnings {
					assertNoTokenIn(t, "a warning", w, composed)
				}
			} else {
				require.NotZero(t, run.res.code)
				var env struct {
					Data struct {
						Error struct {
							Code       string             `json:"code"`
							Message    string             `json:"message"`
							Hint       string             `json:"hint"`
							Details    any                `json:"details"`
							Candidates []clierr.Candidate `json:"candidates"`
						} `json:"error"`
					} `json:"data"`
					Warnings []string `json:"warnings"`
				}
				require.NoError(t, json.Unmarshal([]byte(run.stderr), &env), run.stderr)
				if row.code == ambiguousRow {
					assert.True(t, env.Data.Error.Code == clierr.CodeAmbiguousSelection, env.Data.Error.Code)
					assert.Equal(t, []clierr.Candidate{
						{ID: "fleet-default/a", Name: "fleet-default/a"}, {ID: "fleet-default/b", Name: "fleet-default/b"},
					}, env.Data.Error.Candidates)
				} else {
					assert.Equal(t, row.code, env.Data.Error.Code)
				}
				assert.True(t, strings.HasPrefix(env.Data.Error.Message, row.message), env.Data.Error.Message)
				assertNoTokenIn(t, "the message", env.Data.Error.Message, composed)
				assertNoTokenIn(t, "the hint", env.Data.Error.Hint, composed)
				details, err := json.Marshal(env.Data.Error.Details)
				require.NoError(t, err)
				assertNoTokenIn(t, "the details", string(details), composed)
				candidates, err := json.Marshal(env.Data.Error.Candidates)
				require.NoError(t, err)
				assertNoTokenIn(t, "the candidates", string(candidates), composed)

				for _, w := range env.Warnings {
					assertNoTokenIn(t, "a warning", w, composed)
				}
				assert.Empty(t, run.stdout)
			}

			evs := storedRuns(t, dir)
			require.Len(t, evs, 1)
			b, err := json.Marshal(evs)
			require.NoError(t, err)
			assertNoTokenIn(t, "the recorded events", string(b), composed)
			show := runRecorded(t, newRoot(t), "telemetry", "show", "-o", "json")
			require.Zero(t, show.res.code, show.stderr)
			var listed struct {
				Data []map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal([]byte(show.stdout), &listed), show.stdout)
			var commands []any
			for _, ev := range listed.Data {
				commands = append(commands, ev["io.rancher.cli.command"])
			}
			assert.Contains(t, commands, "inspect cluster", "telemetry show lists the run")
			assertNoTokenIn(t, "telemetry show", show.stdout+show.stderr, composed)
		})
	}
}

// assertNoTokenIn asserts text has neither the registration token nor any
// composed command.
func assertNoTokenIn(t *testing.T, where, text string, composed []string) {
	t.Helper()
	assert.NotContains(t, text, regOptToken, "%s carries the registration token", where)
	for _, c := range composed {
		assert.NotContains(t, text, c, "%s carries a composed command", where)
	}
}

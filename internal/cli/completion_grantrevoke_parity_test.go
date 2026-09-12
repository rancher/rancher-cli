package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// grantRevokeServer serves every management collection grant's and
// revoke's live sites read - users, clusters, one cluster's projects,
// roletemplates, globalroles - and counts the requests per path, so a
// parity case can assert both the candidates and the requests behind
// them.
type grantRevokeServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

const (
	usersPath         = "/k8s/clusters/local/apis/management.cattle.io/v3/users"
	clustersPath      = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	projectsPath      = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-aaa11/projects"
	roleTemplatesPath = "/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates"
	globalRolesPath   = "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
)

func newGrantRevokeServer(t *testing.T) *grantRevokeServer {
	t.Helper()
	s := &grantRevokeServer{hits: map[string]int{}}
	bodies := map[string]string{
		usersPath: `{"items":[
			{"metadata":{"name":"u-alice"},"username":"alice","principalIds":["local://u-alice"]},
			{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
			{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
			{"metadata":{"name":"u-sys"},"username":"system-svc","principalIds":["system://u-sys"]},
			{"metadata":{"name":"u-badname"},"username":"bad name!","principalIds":["local://u-badname"]},
			{"metadata":{"name":"u-nopids"},"username":"nopids"},
			{"metadata":{"name":"sameid"},"username":"sameid","principalIds":["local://sameid"]}
		]}`,
		clustersPath: `{"items":[
			{"metadata":{"name":"c-aaa11"},"spec":{"displayName":"prod"}},
			{"metadata":{"name":"local"},"spec":{"displayName":"local"}},
			{"metadata":{"name":"c-bbb22"},"spec":{"displayName":"dup"}},
			{"metadata":{"name":"c-ccc33"},"spec":{"displayName":"dup"}}
		]}`,
		projectsPath: `{"items":[
			{"metadata":{"name":"p-1"},"spec":{"displayName":"Default"}},
			{"metadata":{"name":"p-2"},"spec":{"displayName":"System"}}
		]}`,
		roleTemplatesPath: `{"items":[
			{"metadata":{"name":"project-owner"},"context":"project"},
			{"metadata":{"name":"project-member"},"context":"project"},
			{"metadata":{"name":"project-member"},"context":"project"},
			{"metadata":{"name":"cluster-owner"},"context":"cluster"},
			{"metadata":{"name":"bad name!"},"context":"project"},
			{"metadata":{"name":"no-context-role"}}
		]}`,
		globalRolesPath: `{"items":[
			{"metadata":{"name":"admin"}},
			{"metadata":{"name":"user"}}
		]}`,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// reset clears the per-path counts and returns them as they were.
func (s *grantRevokeServer) reset() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.hits
	s.hits = map[string]int{}
	return old
}

// completionFixture points a hermetic config directory at srvURL with the
// network switch as given and a management token in the environment, the
// way the full-callback completion tests set up a TAB.
func completionFixture(t *testing.T, srvURL string, networkOn bool) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srvURL}},
		Completion:    config.CompletionConfig{Network: networkOn},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvToken, "mgmt-token")
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
}

// runCompletion runs one v2-protocol TAB for words with the current word
// through the real root and returns the emitted candidates, one per
// line. The argv globals CaptureArgv mutates are restored afterwards.
func runCompletion(t *testing.T, words []string, currentWord string) []string {
	t.Helper()
	origArgv, origWord, origProto, origV1 := rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() {
		rawArgv = origArgv
		rawCurrentWord = origWord
		rawProtoV2 = origProto
		rawV1Completion = origV1
	})
	scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
	scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
	text := strings.TrimRight(out.String(), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestGrantRevokeSitesFullCallback asserts, for grant and for revoke, the
// candidates and the requests of every live site of the two commands
// through the real TAB callback: the --role value under each scope and
// under none, the subject positional, the --cluster value and the
// --project value. The table is the parity contract for migrating any of
// these sites to a live completion source: the same TAB input answers the
// same candidates with the same requests before and after.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestGrantRevokeSitesFullCallback(t *testing.T) {
	s := newGrantRevokeServer(t)
	users := []string{"alice", "bob", "nopids", "sameid", "u-alice", "u-badname", "u-bob", "u-nopids"}
	clusters := []string{"c-aaa11", "c-bbb22", "c-ccc33", "local", "prod"}
	projects := []string{"Default", "System", "p-1", "p-2"}
	tests := []struct {
		name  string
		tail  []string // after the command name
		cur   string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		{name: "--role global", tail: []string{"jane", "--global", "--role"}, want: []string{"admin", "user"}, hits: map[string]int{globalRolesPath: 1}},
		{name: "--role= global equals form", tail: []string{"jane", "--global"}, cur: "--role=", want: []string{"--role=admin", "--role=user"}, hits: map[string]int{globalRolesPath: 1}},
		{name: "--role project", tail: []string{"jane", "--project", "Default", "--cluster", "c-aaa11", "--role"}, want: []string{"project-member", "project-owner"}, hits: map[string]int{roleTemplatesPath: 1}},
		{name: "--role project equals form", tail: []string{"jane", "--project=Default", "--cluster=c-aaa11"}, cur: "--role=", want: []string{"--role=project-member", "--role=project-owner"}, hits: map[string]int{roleTemplatesPath: 1}},
		{name: "--role cluster", tail: []string{"jane", "--cluster", "c-aaa11", "--role"}, want: []string{"cluster-owner"}, hits: map[string]int{roleTemplatesPath: 1}},
		{name: "--role cluster by display name is not resolved", tail: []string{"jane", "--cluster", "prod", "--role"}, want: []string{"cluster-owner"}, hits: map[string]int{roleTemplatesPath: 1}},
		{name: "--role cluster with an unusable cluster value still answers", tail: []string{"jane", "--cluster", "prod cluster", "--role"}, want: []string{"cluster-owner"}, hits: map[string]int{roleTemplatesPath: 1}},
		{name: "--role no scope", tail: []string{"jane", "--role"}, hits: map[string]int{}},
		{name: "--role conflicting scope", tail: []string{"jane", "--global", "--project", "Default", "--role"}, hits: map[string]int{}},
		{name: "--role network off", tail: []string{"jane", "--global", "--role"}, noNet: true, hits: map[string]int{}},
		{name: "subject positional", tail: nil, want: users, hits: map[string]int{usersPath: 1}},
		{name: "subject positional with an unusable cluster value still answers", tail: []string{"--cluster", "prod cluster"}, want: users, hits: map[string]int{usersPath: 1}},
		{name: "subject positional --type group", tail: []string{"--type", "group"}, hits: map[string]int{}},
		{name: "subject positional --type=group", tail: []string{"--type=group"}, hits: map[string]int{}},
		{name: "subject positional network off", tail: nil, noNet: true, hits: map[string]int{}},
		{name: "second positional completes nothing", tail: []string{"jane"}, hits: map[string]int{}},
		{name: "--type static enum", tail: []string{"jane", "--type"}, want: []string{"group", "user"}, hits: map[string]int{}},
		{name: "--cluster value", tail: []string{"jane", "--cluster"}, want: clusters, hits: map[string]int{clustersPath: 1}},
		{name: "--cluster= equals form", tail: []string{"jane"}, cur: "--cluster=", want: []string{"--cluster=c-aaa11", "--cluster=c-bbb22", "--cluster=c-ccc33", "--cluster=local", "--cluster=prod"}, hits: map[string]int{clustersPath: 1}},
		{name: "--project value with a cluster id", tail: []string{"jane", "--cluster", "c-aaa11", "--project"}, want: projects, hits: map[string]int{projectsPath: 1}},
		{name: "--project value with a cluster display name resolves it first", tail: []string{"jane", "--cluster", "prod", "--project"}, want: projects, hits: map[string]int{clustersPath: 1, projectsPath: 1}},
		{name: "--project= equals form", tail: []string{"jane", "--cluster", "c-aaa11"}, cur: "--project=", want: []string{"--project=Default", "--project=System", "--project=p-1", "--project=p-2"}, hits: map[string]int{projectsPath: 1}},
		{name: "--project value without a cluster", tail: []string{"jane", "--project"}, hits: map[string]int{}},
		{name: "--project value with an unusable cluster value", tail: []string{"jane", "--cluster", "prod cluster", "--project"}, hits: map[string]int{}},
	}
	for _, cmd := range []string{"grant", "revoke"} {
		for _, tt := range tests {
			t.Run(cmd+" "+tt.name, func(t *testing.T) {
				completionFixture(t, s.srv.URL, !tt.noNet)
				s.reset()
				got := runCompletion(t, append([]string{cmd}, tt.tail...), tt.cur)
				assert.Equal(t, tt.want, got)
				assert.Equal(t, tt.hits, s.reset())
			})
		}
		t.Run(cmd+" --role under an exec-credential-only token", func(t *testing.T) {
			idOnlyFixture(t, s.srv, "c-aaa11")
			s.reset()
			got := runCompletion(t, []string{cmd, "jane", "--cluster", "c-aaa11", "--role"}, "")
			assert.Nil(t, got, "a management listing is refused under an id-only token")
			assert.Equal(t, map[string]int{}, s.reset())
		})
	}
}

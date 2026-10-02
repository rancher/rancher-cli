package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGrantRevokeServer serves every collection grant's and revoke's live
// sites read: users, clusters, one cluster's projects, roletemplates and
// globalroles.
func newGrantRevokeServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, map[string]string{
		mgmtUsersPath:     usersBody,
		mgmtClustersPath:  clustersBody,
		steveProjectsPath: projectsBody,
		roleTemplatesPath: items(
			`{"metadata":{"name":"project-owner"},"context":"project"}`,
			`{"metadata":{"name":"project-member"},"context":"project"}`,
			`{"metadata":{"name":"project-member"},"context":"project"}`,
			`{"metadata":{"name":"cluster-owner"},"context":"cluster"}`,
			`{"metadata":{"name":"bad name!"},"context":"project"}`,
			`{"metadata":{"name":"no-context-role"}}`),
		globalRolesPath: items(plain("admin"), plain("user")),
	}, nil)
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
	setStoredConfigForTest(t, cfg, dir)
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
	users := []string{"alice", "bob", "sameid", "u-alice", "u-badname", "u-bob"}
	clusters := []string{"c-aaa11", "c-bbb22", "c-ccc33", "c-ddd44", "local", "prod"}
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
		// A flag typed twice takes its last spelling, as the command reads it.
		{name: "--role global then global=false with a cluster selects cluster", tail: []string{"jane", "--global", "--global=false", "--cluster", "c-aaa11", "--role"}, want: []string{"cluster-owner"}, hits: map[string]int{roleTemplatesPath: 1}},
		{name: "--role global=false then global with a cluster conflicts", tail: []string{"jane", "--global=false", "--global", "--cluster", "c-aaa11", "--role"}, hits: map[string]int{}},
		{name: "subject positional", tail: nil, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "subject positional with an unusable cluster value still answers", tail: []string{"--cluster", "prod cluster"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "subject positional --type group", tail: []string{"--type", "group"}, hits: map[string]int{}},
		{name: "subject positional --type=group", tail: []string{"--type=group"}, hits: map[string]int{}},
		{name: "subject positional --type group then --type user", tail: []string{"--type", "group", "--type", "user"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "subject positional --type user then --type group", tail: []string{"--type", "user", "--type", "group"}, hits: map[string]int{}},
		{name: "subject positional network off", tail: nil, noNet: true, hits: map[string]int{}},
		{name: "second positional completes nothing", tail: []string{"jane"}, hits: map[string]int{}},
		{name: "--type static enum", tail: []string{"jane", "--type"}, want: []string{"group", "user"}, hits: map[string]int{}},
		{name: "--cluster value", tail: []string{"jane", "--cluster"}, want: clusters, hits: map[string]int{mgmtClustersPath: 1}},
		{name: "--cluster= equals form", tail: []string{"jane"}, cur: "--cluster=", want: []string{"--cluster=c-aaa11", "--cluster=c-bbb22", "--cluster=c-ccc33", "--cluster=c-ddd44", "--cluster=local", "--cluster=prod"}, hits: map[string]int{mgmtClustersPath: 1}},
		{name: "--project value with a cluster id", tail: []string{"jane", "--cluster", "c-aaa11", "--project"}, want: projects, hits: map[string]int{steveProjectsPath: 1}},
		{name: "--project value with a cluster display name resolves it first", tail: []string{"jane", "--cluster", "prod", "--project"}, want: projects, hits: map[string]int{mgmtClustersPath: 1, steveProjectsPath: 1}},
		{name: "--project= equals form", tail: []string{"jane", "--cluster", "c-aaa11"}, cur: "--project=", want: []string{"--project=Default", "--project=System", "--project=p-1", "--project=p-2"}, hits: map[string]int{steveProjectsPath: 1}},
		{name: "--project value without a cluster", tail: []string{"jane", "--project"}, hits: map[string]int{}},
		{name: "--project value with a spaced display name resolves it first", tail: []string{"jane", "--cluster", "prod cluster", "--project"}, hits: map[string]int{mgmtClustersPath: 1, "/v1/management.cattle.io.projects/c-ddd44": 1}},
		{name: "--project value with a display name nothing carries", tail: []string{"jane", "--cluster", "nope", "--project"}, hits: map[string]int{mgmtClustersPath: 1}},
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

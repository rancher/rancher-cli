package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// hostileDisplayServer serves every listing a description site reads with
// display names that hold a newline and an escape sequence, the text a
// server may carry and a shell must never receive as a second line or a
// control sequence.
func hostileDisplayServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, map[string]string{
		mgmtUsersPath: items(
			`{"metadata":{"name":"u-1"},"username":"one","displayName":"a\nb","principalIds":["local://u-1"]}`,
			`{"metadata":{"name":"u-2"},"username":"two","displayName":"x\u001b[31m","principalIds":["local://u-2"]}`),
		mgmtClustersPath: items(
			`{"metadata":{"name":"c-aaa11"},"spec":{"displayName":"a\nb"}}`,
			`{"metadata":{"name":"c-bbb22"},"spec":{"displayName":"x\u001b[31m"}}`),
		steveProjectsPath: `{"count":2,"revision":"1","data":[
			{"id":"c-aaa11/p-1","metadata":{"name":"p-1"},"spec":{"clusterName":"c-aaa11","displayName":"a\nb"}},
			{"id":"c-aaa11/p-2","metadata":{"name":"p-2"},"spec":{"clusterName":"c-aaa11","displayName":"x\u001b[31m"}}
		]}`,
		roleTemplatesPath: items(
			`{"metadata":{"name":"role-1"},"context":"project","displayName":"a\nb"}`,
			`{"metadata":{"name":"role-2"},"context":"project","displayName":"x\u001b[31m"}`),
		globalRolesPath: items(
			`{"metadata":{"name":"global-1"},"displayName":"a\nb"}`,
			`{"metadata":{"name":"global-2"},"displayName":"x\u001b[31m"}`),
		nodeDriversPath: items(
			`{"metadata":{"name":"driver-1"},"spec":{"displayName":"a\nb"}}`,
			`{"metadata":{"name":"driver-2"},"spec":{"displayName":"x\u001b[31m"}}`),
	}, map[string]steveFixturePage{
		"": {Data: []map[string]any{steveFixtureItem("c-aaa11", "a\nb"), steveFixtureItem("c-bbb22", "x\x1b[31m")}, Count: new(2), Revision: "9"},
	})
}

// TestHostileDescriptionsArriveAsOneLine asserts, through the real TAB
// callback at every site that describes a value, that a display name with
// a newline or an escape sequence reaches the shell as one line with no
// control character: one output line per value, in each shape the site
// has (space form, equals form, name positional).
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestHostileDescriptionsArriveAsOneLine(t *testing.T) {
	s := hostileDisplayServer(t)
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
	}{
		{name: "get user", words: []string{"get", "user"}, want: []string{"u-1:one (a b)", "u-2:two (x [31m)"}},
		{name: "--as", words: []string{"access", "can", "get", "pods", "--as"}, want: []string{"one", "two", "u-1:one (a b)", "u-2:two (x [31m)"}},
		{name: "grant subject", words: []string{"grant"}, want: []string{"one", "two", "u-1:one (a b)", "u-2:two (x [31m)"}},
		{name: "get cluster", words: []string{"get", "cluster"}, want: []string{"c-aaa11:a b", "c-bbb22:x [31m"}},
		{name: "--cluster", words: []string{"get", "pod", "--cluster"}, want: []string{"c-aaa11:a b", "c-bbb22:x [31m"}},
		{name: "--cluster equals form", words: []string{"get", "pod"}, cur: "--cluster=", want: []string{"--cluster=c-aaa11:a b", "--cluster=c-bbb22:x [31m"}},
		{name: "get project", words: []string{"get", "project", "--cluster", "c-aaa11"}, want: []string{"p-1:a b", "p-2:x [31m"}},
		{name: "inspect project", words: []string{"inspect", "project", "--cluster", "c-aaa11"}, want: []string{"p-1:a b", "p-2:x [31m"}},
		{name: "--project", words: []string{"get", "pod", "--cluster", "c-aaa11", "--project"}, want: []string{"p-1:a b", "p-2:x [31m"}},
		{name: "--role project", words: []string{"grant", "jane", "--project", "Default", "--cluster", "c-aaa11", "--role"}, want: []string{"role-1:a b", "role-2:x [31m"}},
		{name: "--role global", words: []string{"grant", "jane", "--global", "--role"}, want: []string{"global-1:a b", "global-2:x [31m"}},
		{name: "get roletemplate", words: []string{"get", "roletemplate"}, want: []string{"role-1:a b", "role-2:x [31m"}},
		{name: "get globalrole", words: []string{"get", "globalrole"}, want: []string{"global-1:a b", "global-2:x [31m"}},
		{name: "get nodedriver", words: []string{"get", "nodedriver"}, want: []string{"driver-1:a b", "driver-2:x [31m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionFixture(t, s.srv.URL, true)
			s.reset()
			got := runCompletion(t, tt.words, tt.cur)
			assert.Equal(t, tt.want, got)
			for _, line := range got {
				assert.False(t, strings.ContainsAny(line, "\x1b\r\t\x07"), "no control character: %q", line)
			}
		})
	}
}

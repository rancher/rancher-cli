package cli

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// serverFixture is three test servers and what each one's facts file says.
type serverFixture struct {
	urls     map[string]string
	versions map[string]string
}

// newServerFixture starts three servers, A, B and C, that answer every
// request with 404.
func newServerFixture(t *testing.T) serverFixture {
	t.Helper()
	f := serverFixture{
		urls:     map[string]string{},
		versions: map[string]string{"A": "2.9.1", "B": "2.9.2", "C": "2.9.3"},
	}
	for name := range f.versions {
		srv := httptest.NewUnstartedServer(http.NotFoundHandler())
		srv.Config.ErrorLog = log.New(io.Discard, "", 0)
		srv.StartTLS()
		t.Cleanup(srv.Close)
		f.urls[name] = srv.URL
	}
	return f
}

// writeFacts writes a facts file with a distinct version for each server.
func (f serverFixture) writeFacts(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	s, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	for name, url := range f.urls {
		var facts telemetry.Facts
		facts.SetVersion(f.versions[name], telemetry.EditionCommunity, time.Now())
		require.NoError(t, s.WriteFacts(url, facts))
	}
}

// expand replaces {A}, {B}, {C} with the server URLs and {a}, {b}, {c} with
// the same URLs without the scheme.
func (f serverFixture) expand(text string) string {
	var pairs []string
	for name, url := range f.urls {
		pairs = append(pairs, "{"+name+"}", url, "{"+strings.ToLower(name)+"}", strings.TrimPrefix(url, "https://"))
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// TestTheRecordedServerIsTheOneEachCommandNames asserts, for each command
// that names its own server and each way it names one, and for the commands
// that use the current server and those with none, that the stored event
// carries the version of the server expected: the config has B as the
// current server and C as an entry named named, and each server has a facts
// file with its own version.
func TestTheRecordedServerIsTheOneEachCommandNames(t *testing.T) {
	const config3 = "currentServer: cur\nservers:\n  cur:\n    url: {B}\n  named:\n    url: {C}\n"
	rows := []struct {
		name string
		args string
		env  string
		// want is A, B or C, or "" for no server.
		want string
	}{
		{"get-token typed server", "auth get-token --server {A} --cluster c-1", "", "A"},
		{"get-token schemeless server", "auth get-token --server {a} --cluster c-1", "", "A"},
		{"get-token variable", "auth get-token --cluster c-1", "{A}", "A"},
		{"get-token schemeless variable", "auth get-token --cluster c-1", "{a}", "A"},
		{"get-token current server", "auth get-token --cluster c-1", "", "B"},
		{"get-token typed server beats the variable", "auth get-token --server {C} --cluster c-1", "{A}", "C"},
		{"token typed server", "token --server {A} --user kube --cluster c-1", "", "A"},
		{"token schemeless server", "token --server {a} --user kube --cluster c-1", "", "A"},
		{"token no server", "token --user kube --cluster c-1", "", ""},
		{"login typed url", "auth login --url {A}", "", "A"},
		{"login named entry", "auth login named", "", "C"},
		{"login current entry", "auth login", "", "B"},
		{"login current entry with the variable set", "auth login", "{A}", "B"},
		{"login absent entry", "auth login nope", "", ""},
		{"logout named entry", "auth logout named", "", "C"},
		{"logout current entry", "auth logout", "", "B"},
		{"logout current entry with the variable set", "auth logout", "{A}", "B"},
		{"logout all", "auth logout --all", "", ""},
		{"logout all and a name", "auth logout --all named", "", ""},
		{"logout absent entry", "auth logout nope", "", ""},
		{"delete-token", "auth delete-token nope", "", ""},
		{"add-server", "config add-server x --url {A}", "", "A"},
		{"update-server typed url", "config update-server named --url {A}", "", "A"},
		{"update-server named entry", "config update-server named", "", "C"},
		{"delete-server named entry", "config delete-server named", "", "C"},
		{"delete-server absent entry", "config delete-server nope", "", ""},
		{"auth status", "auth status", "", "B"},
		{"auth status with the variable set", "auth status", "{A}", "A"},
		{"kubeconfig", "kubeconfig", "", "B"},
		{"engine command", "get cluster", "", "B"},
		{"engine command with the variable set", "get cluster", "{A}", "A"},
		{"engine command with a schemeless variable", "get cluster", "{a}", ""},
		{"command with no server access", "config path", "", ""},
		{"use-server", "config use-server named", "", ""},
	}
	f := newServerFixture(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := recordingEnv(t)
			writeConfig(t, dir, f.expand(config3))
			f.writeFacts(t, dir)
			if row.env != "" {
				t.Setenv(config.EnvURL, f.expand(row.env))
			}
			runRecorded(t, newRoot(t), strings.Fields(f.expand(row.args))...)
			evs := storedRuns(t, dir)
			require.Len(t, evs, 1)
			fields := evs[0].Fields
			if row.want == "" {
				assert.NotContains(t, fields, "io.rancher.manager.version")
				assert.Equal(t, "no_server", fields["io.rancher.cli.install_id.missing_reason"])
				return
			}
			assert.Equal(t, f.versions[row.want], fields["io.rancher.manager.version"])
		})
	}
}

// serverMapViolations returns one line per command in ds that names its own
// server and is neither a row of serverRows nor a command with no server
// access, per command in current that uses the current server without being
// in the list of those, and per key of serverRows that is no command.
func serverMapViolations(ds []runtime.Descriptor, current []string) []string {
	var out []string
	registered := map[string]bool{}
	var usesCurrent []string
	for _, d := range ds {
		registered[d.Name] = true
		_, row := serverRows[d.Name]
		access := d.EffectiveServerAccess()
		if d.CommandType == runtime.Local && access != runtime.ServerAccessNone && !row {
			usesCurrent = append(usesCurrent, d.Name)
		}
		names := slices.Contains(entryArgumentCommands, d.Name)
		for _, f := range d.Flags {
			names = names || f.Name == "server" || f.Name == "url"
		}
		if names && !row && access != runtime.ServerAccessNone {
			out = append(out, d.Name+" names a server and has no row")
		}
	}
	slices.Sort(usesCurrent)
	for _, name := range usesCurrent {
		if !slices.Contains(current, name) {
			out = append(out, name+" is a local command with server access and no row")
		}
	}
	for _, name := range current {
		if !slices.Contains(usesCurrent, name) {
			out = append(out, name+" is listed as using the current server and does not")
		}
	}
	for name := range serverRows {
		if !registered[name] {
			out = append(out, name+" is a row and no command")
		}
	}
	slices.Sort(out)
	return out
}

// entryArgumentCommands take a server entry as a positional argument.
var entryArgumentCommands = []string{
	"auth login", "auth logout", "config delete-server", "config update-server", "config use-server",
}

// TestTheServerMapCoversItsClass asserts every local command with server
// access read or write is a row of the server map or one of the two that
// use the current server, every command that declares a server or url flag
// or takes an entry as an argument is a row or has no server access, and
// every row names a registered command.
func TestTheServerMapCoversItsClass(t *testing.T) {
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var ds []runtime.Descriptor
	for _, c := range wr.reg.Commands() {
		ds = append(ds, c.Describe())
	}
	assert.Empty(t, serverMapViolations(ds, []string{"auth status", "kubeconfig"}))
}

// aboutFresh returns the lines that are about the command fresh command.
func aboutFresh(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "fresh command") {
			out = append(out, l)
		}
	}
	return out
}

// TestServerMapCheckReportsWhatItShould asserts the check above reports a
// new local write command with a server flag that has no row, a listed
// command that is not one, and a row for a command that does not exist.
func TestServerMapCheckReportsWhatItShould(t *testing.T) {
	fresh := runtime.Descriptor{
		Name:         "fresh command",
		CommandType:  runtime.Local,
		ServerAccess: runtime.ServerAccessWrite,
		Flags:        []runtime.ArgSpec{{Name: "server", Type: "string"}},
	}
	got := aboutFresh(serverMapViolations([]runtime.Descriptor{fresh}, nil))
	assert.Equal(t, []string{
		"fresh command is a local command with server access and no row",
		"fresh command names a server and has no row",
	}, got)
	none := fresh
	none.ServerAccess = runtime.ServerAccessNone
	assert.Empty(t, aboutFresh(serverMapViolations([]runtime.Descriptor{none}, nil)))
	assert.Contains(t, serverMapViolations(nil, []string{"ghost"}), "ghost is listed as using the current server and does not")
	assert.Contains(t, serverMapViolations(nil, nil), "auth login is a row and no command")
}

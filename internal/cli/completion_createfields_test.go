package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// TestContainerLimitKeyCandidates asserts the key=value list completion of
// create project's --container-default-resource-limit: unused keys only,
// the typed pairs kept as the prefix, equals form, and ownership of every
// value site even when nothing is offered.
func TestContainerLimitKeyCandidates(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{Name: "create project"}
	site := func(partial string) CompletionSite {
		return CompletionSite{Kind: SiteFlagValue, FlagName: "container-default-resource-limit",
			FlagSpelling: "--container-default-resource-limit", Partial: partial}
	}
	cases := []struct {
		name, partial string
		want          []string
	}{
		{"empty offers every key", "", []string{"limitsCpu=", "limitsMemory=", "requestsCpu=", "requestsMemory="}},
		{"partial filters", "req", []string{"requestsCpu=", "requestsMemory="}},
		{"after a pair, the unused keys", "limitsCpu=1,", []string{"limitsCpu=1,limitsMemory=", "limitsCpu=1,requestsCpu=", "limitsCpu=1,requestsMemory="}},
		{"after a pair, filtered", "limitsCpu=1,limitsM", []string{"limitsCpu=1,limitsMemory="}},
		{"every key used", "limitsCpu=1,limitsMemory=1Gi,requestsCpu=1,requestsMemory=1Gi,", nil},
		{"value in progress", "limitsCpu=", nil},
		{"value in progress after a pair", "limitsCpu=1,requestsCpu=5", nil},
		{"unmatched partial", "zzz", nil},
		{"unknown typed key", "limitsCPU=1,", nil},
		{"typed pair without a value", "limitsCpu=,", nil},
		{"typed pair without equals", "limitsCpu,", nil},
		{"repeated typed key", "limitsCpu=1,limitsCpu=2,", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, owned := containerLimitKeyCandidates(site(tc.partial), d, nil)
			assert.True(t, owned)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("equals form", func(t *testing.T) {
		t.Parallel()
		s := site("limitsCpu=1,r")
		s.EqualsForm = true
		got, owned := containerLimitKeyCandidates(s, d, nil)
		assert.True(t, owned)
		assert.Equal(t, []string{
			"--container-default-resource-limit=limitsCpu=1,requestsCpu=",
			"--container-default-resource-limit=limitsCpu=1,requestsMemory=",
		}, got)
	})

	t.Run("a second instance of the flag offers nothing", func(t *testing.T) {
		t.Parallel()
		first := []string{"web", "--cluster", "c-x", "--container-default-resource-limit"}
		got, owned := containerLimitKeyCandidates(site(""), d, first)
		assert.True(t, owned)
		assert.Len(t, got, 4, "the first instance offers every key")

		second := []string{"web", "--container-default-resource-limit", "limitsCpu=1", "--container-default-resource-limit"}
		got, owned = containerLimitKeyCandidates(site(""), d, second)
		assert.True(t, owned)
		assert.Empty(t, got, "space form after an earlier instance")

		equals := site("lim")
		equals.EqualsForm = true
		got, owned = containerLimitKeyCandidates(equals, d, []string{"web", "--container-default-resource-limit=limitsCpu=1"})
		assert.True(t, owned)
		assert.Empty(t, got, "equals form after an earlier instance")

		for _, earlier := range [][]string{
			{"web", "-container-default-resource-limit", "limitsCpu=1", "--container-default-resource-limit"},
			{"web", "-container-default-resource-limit=limitsCpu=1", "--container-default-resource-limit"},
		} {
			got, owned = containerLimitKeyCandidates(site(""), d, earlier)
			assert.True(t, owned)
			assert.Empty(t, got, "single-dash earlier instance %q", earlier)
		}
	})

	t.Run("other flags and commands are not owned", func(t *testing.T) {
		t.Parallel()
		_, owned := containerLimitKeyCandidates(CompletionSite{Kind: SiteFlagValue, FlagName: "quota-limit"}, d, nil)
		assert.False(t, owned)
		_, owned = containerLimitKeyCandidates(site(""), runtime.Descriptor{Name: "create namespace"}, nil)
		assert.False(t, owned)
	})

	t.Run("every offered key is one the command accepts", func(t *testing.T) {
		t.Parallel()
		got, _ := containerLimitKeyCandidates(site(""), d, nil)
		var keys []string
		for _, tok := range got {
			keys = append(keys, tok[:len(tok)-1])
		}
		assert.Equal(t, projects.ContainerDefaultResourceLimitKeys(), keys)
	})
}

// TestFlagInstances asserts every spelling the parser accepts counts as an
// instance of a flag, and nothing else does.
func TestFlagInstances(t *testing.T) {
	t.Parallel()
	f := runtime.ArgSpec{Name: "filename", Aliases: []string{"f"}}
	cases := []struct {
		tail []string
		want int
	}{
		{[]string{"--filename", "a"}, 1},
		{[]string{"-filename", "a"}, 1},
		{[]string{"--filename=a", "-filename=b"}, 2},
		{[]string{"-f", "a", "--f=b"}, 2},
		{[]string{"--filenames", "a", "--file", "b", "filename"}, 0},
		{[]string{"--other", "-1", "--", "--filename", "a"}, 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, flagInstances(tc.tail, f), "%q", tc.tail)
	}
}

// runCreateFieldCompletion drives one completion request through
// BuildRoot and root.Run, with live completion off and a server URL on a
// closed port, and returns the candidate output. NOT parallel: it mutates
// the package's argv globals, the stored config and the environment.
func runCreateFieldCompletion(t *testing.T, words []string, currentWord string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "http://127.0.0.1:1"}},
		Completion:    config.CompletionConfig{Network: false},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")

	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() {
		rawArgv = origArgv
		rawCurrentWord = origWord
		rawCompleting = origProto
	})
	scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
	scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
	return out.String()
}

// TestCreateFieldFlagsFullCallback asserts the completion of the create
// project and create user field flags through the real command tree: the
// container limit keys in both flag forms, the new flag names, and no
// value suggestions for the free-text --description.
func TestCreateFieldFlagsFullCallback(t *testing.T) {
	t.Run("container limit keys, space form", func(t *testing.T) {
		got := runCreateFieldCompletion(t, []string{"create", "project", "web", "--cluster", "c-x", "--container-default-resource-limit"}, "lim")
		assert.Equal(t, "limitsCpu=\nlimitsMemory=\n", got)
	})
	t.Run("container limit keys, equals form", func(t *testing.T) {
		got := runCreateFieldCompletion(t, []string{"create", "project", "web", "--cluster", "c-x"}, "--container-default-resource-limit=limitsCpu=1,requestsM")
		assert.Equal(t, "--container-default-resource-limit=limitsCpu=1,requestsMemory=\n", got)
	})
	t.Run("second instance offers nothing", func(t *testing.T) {
		got := runCreateFieldCompletion(t, []string{"create", "project", "web", "--container-default-resource-limit", "limitsCpu=1", "--container-default-resource-limit"}, "")
		assert.Empty(t, got)
	})
	t.Run("a flag given once is not offered again", func(t *testing.T) {
		for _, tc := range []struct {
			words []string
			cur   string
		}{
			{[]string{"create", "project", "web", "--cluster", "c-x", "--container-default-resource-limit", "limitsCpu=1"}, "--cont"},
			{[]string{"create", "project", "web", "--cluster", "c-x", "-container-default-resource-limit=limitsCpu=1"}, "--cont"},
			{[]string{"create", "project", "web", "--quota-limit", "limitsCpu=1"}, "--quo"},
			{[]string{"create", "project", "web", "-namespace-default-quota", "limitsCpu=1"}, "--names"},
			{[]string{"create", "secret", "s", "--from-literal", "a=1"}, "--from"},
		} {
			got := runCreateFieldCompletion(t, tc.words, tc.cur)
			assert.Empty(t, got, "%q then %q", tc.words, tc.cur)
		}
		got := runCreateFieldCompletion(t, []string{"create", "project", "web", "--quota-limit", "limitsCpu=1"}, "--names")
		assert.Contains(t, got, "--namespace-default-quota", "an unused Once flag is still offered")
		got = runCreateFieldCompletion(t, []string{"create", "oidc-client", "app", "--redirect-uri", "https://a.invalid/cb"}, "--redir")
		assert.Contains(t, got, "--redirect-uri", "a repeatable flag is still offered after it was given")
	})
	t.Run("create project offers the flag name", func(t *testing.T) {
		got := runCreateFieldCompletion(t, []string{"create", "project", "web"}, "--container")
		assert.Contains(t, got, "--container-default-resource-limit")
	})
	t.Run("create user offers the new flag names", func(t *testing.T) {
		got := runCreateFieldCompletion(t, []string{"create", "user", "alice"}, "--")
		assert.Contains(t, got, "--description:")
		assert.Contains(t, got, "--must-change-password:")
	})
	t.Run("create user --description takes free text", func(t *testing.T) {
		got := runCreateFieldCompletion(t, []string{"create", "user", "alice", "--description"}, "")
		assert.Empty(t, got)
	})
}

// TestKeyValueFlagsRefuseASecondInstance asserts, through the real command
// tree, that each key=value flag given twice is a VALIDATION error naming
// the flag and the remedy, in the space and the equals form, at parse time:
// no server is configured, so a run that got past parsing would fail with
// another error. NOT parallel: it sets the package's raw argv and the
// environment.
func TestKeyValueFlagsRefuseASecondInstance(t *testing.T) {
	cases := []struct {
		flag string
		args []string
	}{
		{"container-default-resource-limit", []string{"create", "project", "web", "--cluster", "c-x",
			"--container-default-resource-limit", "limitsCpu=1", "--container-default-resource-limit=limitsMemory=1Gi"}},
		{"quota-limit", []string{"create", "project", "web", "--cluster", "c-x",
			"--quota-limit=limitsCpu=1", "--quota-limit", "limitsMemory=1Gi", "--namespace-default-quota", "limitsCpu=1,limitsMemory=1Gi"}},
		{"namespace-default-quota", []string{"create", "project", "web", "--cluster", "c-x", "--quota-limit", "limitsCpu=1,limitsMemory=1Gi",
			"--namespace-default-quota", "limitsCpu=1", "--namespace-default-quota", "limitsMemory=1Gi"}},
		{"from-literal", []string{"create", "secret", "s", "--namespace", "team-a", "--cluster", "c-x",
			"--from-literal", "a=1", "--from-literal", "b=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out, errBuf bytes.Buffer
			root.Writer, root.ErrWriter = &out, &errBuf
			root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

			argv := append(append([]string{"rancher", "--config", t.TempDir(), "-o", "json"}, tc.args...), "--dry-run")
			// A parse error renders before flags parse, in the format read
			// from the raw argv the entry point captured.
			origArgv := rawArgv
			t.Cleanup(func() { rawArgv = origArgv })
			rawArgv = argv[1:]
			require.Error(t, root.Run(t.Context(), argv))
			var env errEnvelope
			require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env), errBuf.String())
			assert.Equal(t, "VALIDATION", env.Data.Error.Code)
			assert.Contains(t, env.Data.Error.Message, "--"+tc.flag+" may be given once")
		})
	}
}

// TestCreateFieldFlagsRefuseEmptyAndSeparateBoolValues asserts, through the
// real command tree against a request-recording server, that the create
// project and create user field flags follow the rules every command
// follows: a value flag typed empty is refused naming the flag, and a word
// typed on its own after the bool flag --must-change-password is refused
// by the argument count or as a bool value, with the hint those rules
// give. Each row is VALIDATION at exit 2 with no request sent. Not
// parallel: it sets the process environment and the captured command line.
func TestCreateFieldFlagsRefuseEmptyAndSeparateBoolValues(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	for _, tc := range []struct {
		name    string
		argv    []string
		message string
		hint    string
	}{
		{
			name:    "an empty --container-default-resource-limit",
			argv:    []string{"create", "project", "web", "--cluster", "c-x", "--container-default-resource-limit", "", "--dry-run", "-o", "json"},
			message: "--container-default-resource-limit is empty; pass a value or omit --container-default-resource-limit",
		},
		{
			name:    "an empty --description",
			argv:    []string{"create", "user", "u", "--password-stdin", "--description", "", "--dry-run", "-o", "json"},
			message: "--description is empty; pass a value or omit --description",
		},
		{
			name:    "an empty word past the argument, a command with no secret input, is named as typed",
			argv:    []string{"create", "namespace", "ns1", "--dry-run", "", "--cluster", "c-x", "-o", "json"},
			message: "create namespace takes 1 argument (<name>); unexpected argument ''",
			hint:    "rancher create namespace --help",
		},
		{
			name:    "an empty word after --must-change-password, past the username, is named by position",
			argv:    []string{"create", "user", "u", "--password-stdin", "--must-change-password", "", "--dry-run", "-o", "json"},
			message: "create user takes 1 argument (<username>); unexpected argument at position 2",
			hint:    "rancher create user --help",
		},
		{
			name:    "an empty word after --must-change-password, in the username's place",
			argv:    []string{"create", "user", "--password-stdin", "--must-change-password", "", "--dry-run", "-o", "json"},
			message: "argument <username> is required",
		},
		{
			name:    "--from-literal after the end of the flags is an argument, not a second instance",
			argv:    []string{"create", "secret", "s1", "--cluster", "c-x", "--from-literal", "a=1", "--dry-run", "-o", "json", "--", "--from-literal", "b=2"},
			message: "create secret takes 1 argument (<name>); unexpected arguments at positions 2 3",
			hint:    "rancher create secret --help",
		},
		{
			name:    "false after --must-change-password: a command with a secret input hints its help",
			argv:    []string{"create", "user", "u", "--password-stdin", "--must-change-password", "false", "--dry-run", "-o", "json"},
			message: "create user: argument at position 2 was typed after the bool flag --must-change-password" + boolRule("--must-change-password"),
			hint:    "rancher create user --help",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stderr, code := runOnce(t, root, tc.argv)
			assert.Equal(t, 2, code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, "VALIDATION", ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Empty(t, rec.take(), "no request")
		})
	}
}

// TestCompletionOffersNothingOnALineWithAOnceFlagTwice asserts, through the
// built root and a marked line with network completion on and a
// request-recording server, that completing --cluster on a create secret
// line that holds --from-literal twice, in either dash spelling and either
// value form, offers nothing and sends no request: the command refuses
// that line while parsing. The same line with one instance sends the
// cluster list request, so the server sees what a completion asks for. Not
// parallel: it sets the process environment and the captured command line.
func TestCompletionOffersNothingOnALineWithAOnceFlagTwice(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: os.Getenv(config.EnvURL)}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(os.Getenv(config.EnvConfigDir)))
	require.NoError(t, cfg.Write())

	complete := func(t *testing.T, words []string) (string, []string) {
		t.Helper()
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		scriptArgs := append(append([]string{protoSentinel}, words...), curPrefix, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		var out bytes.Buffer
		root.Writer, root.ErrWriter, root.Reader = &out, &out, strings.NewReader("")
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String(), rec.take()
	}

	for name, words := range map[string][]string{
		"two instances, two dashes":          {"create", "secret", "s1", "--from-literal", "a=1", "--from-literal", "b=2", "--cluster"},
		"two instances, one dash and equals": {"create", "secret", "s1", "-from-literal", "a=1", "--from-literal=b=2", "--cluster"},
	} {
		t.Run(name, func(t *testing.T) {
			got, requests := complete(t, words)
			assert.Empty(t, got)
			assert.Empty(t, requests, "no request")
		})
	}
	t.Run("one instance sends the cluster request", func(t *testing.T) {
		_, requests := complete(t, []string{"create", "secret", "s1", "--from-literal", "a=1", "--cluster"})
		assert.NotEmpty(t, requests)
	})
}

// TestOnceFlagNamesFollowTheDescriptors asserts that the names the
// completion check reads from the built tree as given-once flags are
// exactly the names and aliases of the flags each command declares
// runtime.ArgSpec.Once, for every command.
func TestOnceFlagNamesFollowTheDescriptors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	nodes := levelCommands(t)
	withOnce := 0
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		want := map[string]string{}
		for _, f := range d.Flags {
			if !f.Once {
				continue
			}
			for _, n := range append([]string{f.Name}, f.Aliases...) {
				want[n] = f.Name
			}
		}
		node, ok := nodes[d.Name]
		if !ok {
			assert.Empty(t, want, "%s declares a Once flag but is not in the tree", d.Name)
			continue
		}
		if len(want) > 0 {
			withOnce++
		}
		assert.Equal(t, want, onceFlagNames(node), "%s: %v", d.Name, slices.Sorted(maps.Keys(want)))
	}
	assert.GreaterOrEqual(t, withOnce, 2, "create project and create secret declare Once flags")
}

// onceAliasCmd is a local command whose flags of each type the adapter
// builds may be given once and carry an alias, beside a repeatable aliased
// flag: no registered command has an aliased Once flag.
type onceAliasCmd struct{}

func (onceAliasCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "oncealias", CommandType: runtime.Local, Flags: []runtime.ArgSpec{
		{Name: "mode", Aliases: []string{"m"}, Type: "string", Once: true, Enum: []string{"fast", "slow"}},
		{Name: "quiet", Aliases: []string{"q"}, Type: "bool", Once: true},
		{Name: "tag", Aliases: []string{"t"}, Type: "stringSlice", Once: true},
		{Name: "note", Aliases: []string{"n"}, Type: "string"},
	}}
}

func (onceAliasCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{Kind: "Done"}, nil
}

// TestOnceFlagGivenByNameAndAlias asserts, on a leaf the adapter builds,
// that onceFlagNames maps each alias of a flag that may be given once to
// the flag's name, for the string, bool and list flag types, and that a
// line holding such a flag once by name and once by alias is rejected,
// while one instance, or a repeatable flag by name and alias, is not. Its
// completion offers nothing on a line whose current word is a second
// instance in the equals form, where the first instance alone offers the
// flag's values. Not parallel: it sets the captured command line.
func TestOnceFlagGivenByNameAndAlias(t *testing.T) {
	restoreRawArgv(t)
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	newRoot := func(out *bytes.Buffer) (*ucli.Command, *ucli.Command) {
		leaf := Adapt(reg, onceAliasCmd{})
		root := &ucli.Command{Name: "rancher", Flags: GlobalFlags(), Commands: []*ucli.Command{leaf},
			EnableShellCompletion: true, Writer: out, ErrWriter: out}
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
		return root, leaf
	}
	_, leaf := newRoot(&bytes.Buffer{})

	assert.Equal(t, map[string]string{
		"mode": "mode", "m": "mode",
		"quiet": "quiet", "q": "quiet",
		"tag": "tag", "t": "tag",
	}, onceFlagNames(leaf))

	for _, words := range [][]string{
		{"--mode", "fast", "-m", "slow"},
		{"-m=fast", "--mode=slow"},
		{"--quiet", "-q"},
		{"-t", "a", "--tag", "b"},
	} {
		assert.True(t, lineRejected(leaf, words, 0), "%q is rejected", words)
	}
	for _, words := range [][]string{
		{"--mode", "fast", "-q", "-t", "a"},
		{"--note", "a", "-n", "b"},
	} {
		assert.False(t, lineRejected(leaf, words, 0), "%q is accepted", words)
	}

	complete := func(words []string, cur string) string {
		var out bytes.Buffer
		root, _ := newRoot(&out)
		scriptArgs := append(append(append([]string{protoSentinel}, "oncealias"), words...), curPrefix+cur, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}
	assert.NotEmpty(t, complete(nil, "--mode="), "the first instance offers the values")
	assert.Empty(t, complete([]string{"--mode", "fast"}, "-m="), "a second instance by alias")
	assert.Empty(t, complete([]string{"-m", "fast"}, "--mode="), "a second instance by name")
}

// TestOnceFlagCountStopsAtTheEndOfTheFlags asserts that lineRejected counts
// the instances of a flag that may be given once only up to a "--": a
// --from-literal typed after it is an argument, so the line is not
// rejected as a second instance, while the same two instances before the
// "--" are. maxWords is set past the words typed so that the argument
// count, a separate rule, decides neither line.
func TestOnceFlagCountStopsAtTheEndOfTheFlags(t *testing.T) {
	t.Parallel()
	leaf := levelCommands(t)["create secret"]
	require.NotNil(t, leaf)
	assert.False(t, lineRejected(leaf, []string{"create", "secret", "s1", "--from-literal", "a=1", "--", "--from-literal", "b=2"}, 10),
		"after -- the second --from-literal is an argument")
	assert.True(t, lineRejected(leaf, []string{"create", "secret", "s1", "--from-literal", "a=1", "--from-literal", "b=2", "--"}, 10),
		"both instances before -- are counted")
}

package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// repoFlagAllowlist names the commands whose --repo flag is not a ClusterRepo
// name (a Helm or Git repository URL, for example), each with the reason.
var repoFlagAllowlist = map[string]string{}

// badRepoValues are typed --repo values that are not a ClusterRepo name.
var badRepoValues = []string{"a/b", "A"}

// withRepo returns args with its --repo value set to v: an existing --repo
// pair is replaced, otherwise one is added.
func withRepo(args []string, v string) []string {
	out := slices.Clone(args)
	if i := slices.Index(out, "--repo"); i >= 0 && i+1 < len(out) {
		out[i+1] = v
		return out
	}
	return append(out, "--repo", v)
}

// takesRepo reports whether d lists a flag named repo.
func takesRepo(d runtime.Descriptor) bool {
	return slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "repo" })
}

// repoFlagFailures drives every command in cmds that has a --repo flag, and
// is not in allow, through run with line(descriptor) as its command line. It
// returns the names of the commands it drove and one text per failure (an
// allowlist entry that names no command with a --repo flag is one too): a bad
// value must give VALIDATION, exit 2, a message that names --repo and says it
// is not a ClusterRepo name, and no request; a ClusterRepo name must send a
// request, so that "no request" shows the check and not a command that sends
// nothing.
func repoFlagFailures(cmds []runtime.Command, allow map[string]string, line func(runtime.Descriptor) []string, run func(args ...string) contractRun) (driven, failures []string) {
	for _, c := range cmds {
		d := c.Describe()
		if !takesRepo(d) {
			continue
		}
		if _, ok := allow[d.Name]; ok {
			continue
		}
		driven = append(driven, d.Name)
		base := line(d)
		for _, bad := range badRepoValues {
			args := withRepo(base, bad)
			r := run(args...)
			switch {
			case r.err == nil:
				failures = append(failures, fmt.Sprintf("%s: --repo %q gave no error envelope (exit %d)", d.Name, bad, r.exit))
			case r.err.Code != "VALIDATION":
				failures = append(failures, fmt.Sprintf("%s: --repo %q gave %s, want VALIDATION", d.Name, bad, r.err.Code))
			case !strings.Contains(r.err.Message, "--repo") || !strings.Contains(r.err.Message, "is not a ClusterRepo name"):
				failures = append(failures, fmt.Sprintf("%s: --repo %q gave another VALIDATION refusal: %s", d.Name, bad, r.err.Message))
			}
			if r.exit != validationExit {
				failures = append(failures, fmt.Sprintf("%s: --repo %q exit %d, want %d", d.Name, bad, r.exit, validationExit))
			}
			if len(r.requests) != 0 {
				failures = append(failures, fmt.Sprintf("%s: --repo %q sent %d requests: %v", d.Name, bad, len(r.requests), r.requests))
			}
		}
		if r := run(withRepo(base, "rancher-charts")...); len(r.requests) == 0 {
			failures = append(failures, fmt.Sprintf("%s: --repo rancher-charts sent no request, so the zero requests of a bad value show nothing", d.Name))
		}
	}
	for name := range allow {
		if !slices.ContainsFunc(cmds, func(c runtime.Command) bool {
			d := c.Describe()
			return d.Name == name && takesRepo(d)
		}) {
			failures = append(failures, fmt.Sprintf("allowlist entry %q names no command with a --repo flag", name))
		}
	}
	return driven, failures
}

// TestEveryRepoFlagRefusesANameThatIsNotAClusterRepo asserts every command
// of the real tree with a --repo flag refuses a value that is not a
// ClusterRepo name as VALIDATION before any request.
func TestEveryRepoFlagRefusesANameThatIsNotAClusterRepo(t *testing.T) {
	h := newContractHarness(t)
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	driven, failures := repoFlagFailures(wr.reg.Commands(), repoFlagAllowlist,
		func(d runtime.Descriptor) []string { return h.contractArgs(d, true) },
		func(args ...string) contractRun { return h.run(t, args...) })
	t.Logf("commands with a --repo flag: %v", driven)
	assert.Empty(t, failures)
	assert.Subset(t, driven, []string{"charts", "app install"})
}

// TestRepoFlagTypedEmptyKeepsTheEmptyValueMessage asserts a typed empty
// --repo, for every command with the flag, still gets the empty-value
// refusal, unchanged and before any request.
func TestRepoFlagTypedEmptyKeepsTheEmptyValueMessage(t *testing.T) {
	h := newContractHarness(t)
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	seen := 0
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if !takesRepo(d) {
			continue
		}
		seen++
		i := slices.IndexFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "repo" })
		r := h.run(t, withRepo(h.contractArgs(d, true), "")...)
		require.NotNil(t, r.err, "%s: %s", d.Name, r.stderr)
		assert.Equal(t, "VALIDATION", r.err.Code, d.Name)
		assert.Equal(t, emptyValueMessage(d.Flags[i]), r.err.Message, d.Name)
		assert.Equal(t, validationExit, r.exit, d.Name)
		assert.Empty(t, r.requests, d.Name)
	}
	assert.GreaterOrEqual(t, seen, 2)
}

// fakeRepoCommand is a command with a --repo flag and no name check.
type fakeRepoCommand struct{}

func (fakeRepoCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "fake", Flags: []runtime.ArgSpec{{Name: "repo", Type: "string"}}}
}

// stubRefusal is the run of a command that refuses with code and message,
// exit 2, and sends nothing.
func stubRefusal(code, message string) contractRun {
	r := contractRun{exit: validationExit}
	body, err := json.Marshal(map[string]string{"code": code, "message": message})
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(body, &r.err); err != nil {
		panic(err)
	}
	return r
}

// repoValueOf is the value after --repo in args.
func repoValueOf(args []string) string {
	i := slices.Index(args, "--repo")
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// TestRepoFlagGuardCatchesWhatItMustCatch asserts, with stub runners, that
// the guard reports a failure for each way a command can miss the rule, and
// none for a command that meets it.
func TestRepoFlagGuardCatchesWhatItMustCatch(t *testing.T) {
	const refusalText = "--repo 'x' is not a ClusterRepo name: details"
	cmds := []runtime.Command{fakeRepoCommand{}}
	line := func(d runtime.Descriptor) []string { return []string{d.Name} }
	sends := contractRun{requests: []string{"GET /v1/x"}}

	t.Run("no check: the command sends a request and succeeds", func(t *testing.T) {
		driven, failures := repoFlagFailures(cmds, nil, line, func(...string) contractRun { return sends })
		assert.Equal(t, []string{"fake"}, driven)
		require.NotEmpty(t, failures)
		assert.Contains(t, failures[0], "gave no error envelope")
		assert.True(t, slices.ContainsFunc(failures, func(f string) bool { return strings.Contains(f, "sent 1 requests") }), "%v", failures)
	})
	t.Run("another validation refusal does not pass", func(t *testing.T) {
		_, failures := repoFlagFailures(cmds, nil, line, func(args ...string) contractRun {
			if repoValueOf(args) == "rancher-charts" {
				return sends
			}
			return stubRefusal("VALIDATION", "--cluster is required")
		})
		require.Len(t, failures, len(badRepoValues))
		for _, f := range failures {
			assert.Contains(t, f, "another VALIDATION refusal")
		}
	})
	t.Run("a command that sends nothing for a ClusterRepo name shows nothing", func(t *testing.T) {
		_, failures := repoFlagFailures(cmds, nil, line, func(...string) contractRun {
			return stubRefusal("VALIDATION", refusalText)
		})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], "--repo rancher-charts sent no request")
	})
	t.Run("a command that meets the rule passes", func(t *testing.T) {
		driven, failures := repoFlagFailures(cmds, nil, line, func(args ...string) contractRun {
			if repoValueOf(args) == "rancher-charts" {
				return sends
			}
			return stubRefusal("VALIDATION", refusalText)
		})
		assert.Equal(t, []string{"fake"}, driven)
		assert.Empty(t, failures)
	})
	t.Run("an allowlisted command is not driven", func(t *testing.T) {
		driven, failures := repoFlagFailures(cmds, map[string]string{"fake": "takes a URL"}, line, func(...string) contractRun { return sends })
		assert.Empty(t, driven)
		assert.Empty(t, failures)
	})
	t.Run("an allowlist entry for no command with the flag fails", func(t *testing.T) {
		_, failures := repoFlagFailures(cmds, map[string]string{"gone": "no such command"}, line, func(args ...string) contractRun {
			if repoValueOf(args) == "rancher-charts" {
				return sends
			}
			return stubRefusal("VALIDATION", refusalText)
		})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], `allowlist entry "gone"`)
	})
}

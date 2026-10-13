package cli

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// Validation-text conformance: the refusal a missing required input gets is
// written from the command's own declaration, so it cannot drift from it.
//
//   - allowedValues: a required argument or flag that declares an Enum is
//     refused, when missing, with a message that lists every value.
//   - clusterRequired: a required --cluster carries the text that fits the
//     command (engine.ClusterRequiredMessage when it declares --namespace or
//     --all-namespaces, engine.ClusterRequiredPlainMessage otherwise) and
//     engine.ClusterRequiredHint.
//
// The test drives runtime.Run only as far as validateInput, so no command
// runs. It does not cover messages built from a value inside Plan or
// Execute (a duration that does not parse, a selector list), an optional
// --cluster, or the namespaced argument a caller passes to
// engine.ResolveDownstreamCluster: those are asserted at their own sites.
// A command whose --project is required gets the declaration check only,
// since a set --project defers the --cluster check. A missing argument is blanked by its position among the
// required arguments, which matches validateInput while no optional
// argument is declared before a required one.
const (
	ruleAllowedValues   = "allowedValues"
	ruleClusterRequired = "clusterRequired"
)

type allowEntry struct {
	rule   string
	reason string
}

// validationTextAllowlist maps a command path to the rule it is exempt from.
// Adding an entry is a decision for the maintainers, not a way to make this
// test pass: every entry states the constraint that keeps the command out of
// the rule, and the test fails once the command passes the rule.
var validationTextAllowlist = map[string]allowEntry{
	"auth get-token": {
		rule: ruleClusterRequired,
		reason: "auth get-token declares --cluster without the shared missing-cluster text; " +
			"delete this entry when it uses engine.ClusterFlag",
	},
}

// enumInput is a required argument or flag that declares an Enum.
type enumInput struct {
	name     string
	enum     []string
	argIndex int
	isFlag   bool
	slice    bool
}

// enumInputs lists the required inputs of d that declare an Enum.
func enumInputs(d runtime.Descriptor) []enumInput {
	var out []enumInput
	reqIdx := 0
	for _, a := range d.Args {
		if !a.Required {
			continue
		}
		if len(a.Enum) > 0 {
			out = append(out, enumInput{name: a.Name, enum: a.Enum, argIndex: reqIdx})
		}
		reqIdx++
	}
	for _, f := range d.Flags {
		if f.Required && len(f.Enum) > 0 && f.Type != "bool" {
			out = append(out, enumInput{name: f.Name, enum: f.Enum, isFlag: true, slice: f.Type == "stringSlice"})
		}
	}
	return out
}

// missingValues returns the enum values msg does not mention.
func missingValues(msg string, enum []string) []string {
	var missing []string
	for _, v := range enum {
		if !strings.Contains(msg, v) {
			missing = append(missing, v)
		}
	}
	return missing
}

// declaresNamespace reports whether d takes --namespace or
// --all-namespaces.
func declaresNamespace(d runtime.Descriptor) bool {
	return slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool {
		return f.Name == "namespace" || f.Name == "all-namespaces"
	})
}

// clusterDeclarationProblem is what is wrong with a required --cluster
// declaration of d: its text or its hint. Empty when it fits.
func clusterDeclarationProblem(d runtime.Descriptor, f runtime.ArgSpec) string {
	want := engine.ClusterRequiredPlainMessage
	if declaresNamespace(d) {
		want = engine.ClusterRequiredMessage
	}
	switch {
	case f.MissingMessage != want:
		return "MissingMessage is " + quoteOrEmpty(f.MissingMessage) + ", want " + quoteOrEmpty(want)
	case f.MissingHint != engine.ClusterRequiredHint:
		return "MissingHint is " + quoteOrEmpty(f.MissingHint) + ", want " + quoteOrEmpty(engine.ClusterRequiredHint)
	}
	return ""
}

func quoteOrEmpty(s string) string {
	if s == "" {
		return "empty"
	}
	return `"` + s + `"`
}

func newSweepInput() sweepInput {
	return sweepInput{s: map[string]string{}, sl: map[string][]string{}, set: map[string]bool{}}
}

// fillRequired fills every required input of d except skipArg (the index
// among the required args, -1 for none, left empty) and skipFlag (left
// unset).
func fillRequired(d runtime.Descriptor, skipArg int, skipFlag string) sweepInput {
	in := newSweepInput()
	reqIdx := 0
	for _, a := range d.Args {
		if !a.Required {
			continue
		}
		v := "x"
		if len(a.Enum) > 0 {
			v = a.Enum[0]
		}
		if reqIdx == skipArg {
			v = ""
		}
		in.args = append(in.args, v)
		reqIdx++
	}
	for _, f := range d.Flags {
		if !f.Required || f.Type == "bool" || f.Name == skipFlag {
			continue
		}
		v := "x"
		if len(f.Enum) > 0 {
			v = f.Enum[0]
		}
		in.s[f.Name], in.sl[f.Name], in.set[f.Name] = v, []string{v}, true
	}
	return in
}

// refusal runs cmd on in through the pipeline and returns the refusal. A
// command that runs fails the test.
func refusal(t *testing.T, cmd runtime.Command, in sweepInput) (code, message string, refused bool) {
	t.Helper()
	d := cmd.Describe()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s: the command ran: %v", d.Name, r)
			}
		}()
		var out, errOut bytes.Buffer
		_, ce := runtime.Run(t.Context(), cmd, in, runtime.IO{Out: &out, Err: &errOut}, safety.Binding{})
		if ce != nil {
			code, message, refused = ce.Code, ce.Message, true
		}
	}()
	return code, message, refused
}

// TestValidationTextFitsTheDeclaration asserts the two rules above for
// every registered command, one subtest per command and rule, so one run
// lists every failure.
func TestValidationTextFitsTheDeclaration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	byName := map[string]runtime.Command{}
	for _, cmd := range wr.reg.Commands() {
		byName[cmd.Describe().Name] = cmd
	}

	for name, e := range validationTextAllowlist {
		assert.NotEmpty(t, e.reason, "allowlist entry %q has no reason", name)
		assert.Contains(t, []string{ruleAllowedValues, ruleClusterRequired}, e.rule, "allowlist entry %q names an unknown rule", name)
		assert.Contains(t, byName, name, "allowlist entry %q names no command", name)
	}

	names := slices.Sorted(maps.Keys(byName))

	for _, name := range names {
		cmd := byName[name]
		d := cmd.Describe()

		t.Run(name+"/"+ruleAllowedValues, func(t *testing.T) {
			var problems []string
			for _, ei := range enumInputs(d) {
				skipArg, skipFlag := -1, ""
				if ei.isFlag {
					skipFlag = ei.name
				} else {
					skipArg = ei.argIndex
				}
				code, msg, refused := refusal(t, cmd, fillRequired(d, skipArg, skipFlag))
				switch {
				case !refused:
					problems = append(problems, ei.name+": missing input not refused")
				case code != "VALIDATION":
					problems = append(problems, ei.name+": code "+code)
				default:
					if miss := missingValues(msg, ei.enum); len(miss) > 0 {
						problems = append(problems, ei.name+": message "+quoteOrEmpty(msg)+" lacks "+strings.Join(miss, ", "))
					}
				}
			}
			checkAllowlist(t, name, ruleAllowedValues, problems)
		})

		t.Run(name+"/"+ruleClusterRequired, func(t *testing.T) {
			var problems []string
			for _, f := range d.Flags {
				if f.Name != "cluster" || !f.Required {
					continue
				}
				if p := clusterDeclarationProblem(d, f); p != "" {
					problems = append(problems, "--cluster: "+p)
				}
				if slices.ContainsFunc(d.Flags, func(p runtime.ArgSpec) bool { return p.Name == "project" && p.Required }) {
					continue
				}
				want := engine.ClusterRequiredPlainMessage
				if declaresNamespace(d) {
					want = engine.ClusterRequiredMessage
				}
				code, msg, refused := refusal(t, cmd, fillRequired(d, -1, "cluster"))
				switch {
				case !refused:
					problems = append(problems, "--cluster missing: not refused")
				case code != "VALIDATION" || msg != want:
					problems = append(problems, "--cluster missing: refused with "+code+" "+quoteOrEmpty(msg)+", want "+quoteOrEmpty(want))
				}
			}
			checkAllowlist(t, name, ruleClusterRequired, problems)
		})
	}
}

// checkAllowlist reports problems for command under rule unless the
// allowlist exempts it, and reports an exemption that no longer applies.
func checkAllowlist(t *testing.T, command, rule string, problems []string) {
	t.Helper()
	e, listed := validationTextAllowlist[command]
	listed = listed && e.rule == rule
	switch {
	case listed && len(problems) == 0:
		t.Errorf("%s is allowlisted for %s but passes: delete the entry", command, rule)
	case !listed:
		for _, p := range problems {
			t.Errorf("%s: %s: %s", command, rule, p)
		}
	}
}

func TestValidationTextRuleAllowedValuesCatchesAMissingValue(t *testing.T) {
	enum := []string{"a", "b", "c"}
	assert.Equal(t, []string{"c"}, missingValues("argument <x> is required; one of a|b", enum))
	assert.Empty(t, missingValues("argument <x> is required; one of a|b|c", enum))

	d := runtime.Descriptor{
		Name: "plant",
		Args: []runtime.ArgSpec{{Name: "first", Required: true}, {Name: "kind", Required: true, Enum: enum}},
		Flags: []runtime.ArgSpec{
			{Name: "mode", Type: "string", Required: true, Enum: enum},
			{Name: "other", Type: "string", Enum: enum},
		},
	}
	got := enumInputs(d)
	require.Len(t, got, 2)
	assert.Equal(t, enumInput{name: "kind", enum: enum, argIndex: 1}, got[0])
	assert.Equal(t, enumInput{name: "mode", enum: enum, isFlag: true}, got[1])

	in := fillRequired(d, got[0].argIndex, "")
	assert.Equal(t, []string{"x", ""}, in.args)
	assert.Equal(t, "a", in.s["mode"])
	in = fillRequired(d, -1, "mode")
	assert.Equal(t, []string{"x", "a"}, in.args)
	assert.False(t, in.set["mode"])
}

func TestValidationTextRuleClusterRequiredCatchesTheWrongText(t *testing.T) {
	plain := runtime.Descriptor{Name: "plain", Flags: []runtime.ArgSpec{{Name: "cluster", Required: true}}}
	namespaced := runtime.Descriptor{Name: "namespaced", Flags: []runtime.ArgSpec{
		{Name: "cluster", Required: true}, {Name: "namespace"},
	}}
	allNamespaces := runtime.Descriptor{Name: "all", Flags: []runtime.ArgSpec{
		{Name: "cluster", Required: true}, {Name: "all-namespaces", Type: "bool"},
	}}
	generic := runtime.ArgSpec{Name: "cluster", Required: true}
	withHint := func(msg string) runtime.ArgSpec {
		return runtime.ArgSpec{Name: "cluster", Required: true, MissingMessage: msg, MissingHint: engine.ClusterRequiredHint}
	}

	assert.NotEmpty(t, clusterDeclarationProblem(plain, generic), "the generic text fails")
	assert.NotEmpty(t, clusterDeclarationProblem(plain, withHint(engine.ClusterRequiredMessage)), "the namespace text fails on a command without a namespace flag")
	assert.NotEmpty(t, clusterDeclarationProblem(namespaced, withHint(engine.ClusterRequiredPlainMessage)), "the plain text fails on a command with --namespace")
	assert.NotEmpty(t, clusterDeclarationProblem(allNamespaces, withHint(engine.ClusterRequiredPlainMessage)), "the plain text fails on a command with --all-namespaces")
	assert.NotEmpty(t, clusterDeclarationProblem(plain, runtime.ArgSpec{Name: "cluster", Required: true, MissingMessage: engine.ClusterRequiredPlainMessage}), "a missing hint fails")
	assert.Empty(t, clusterDeclarationProblem(plain, withHint(engine.ClusterRequiredPlainMessage)))
	assert.Empty(t, clusterDeclarationProblem(namespaced, withHint(engine.ClusterRequiredMessage)))
	assert.Empty(t, clusterDeclarationProblem(allNamespaces, withHint(engine.ClusterRequiredMessage)))
}

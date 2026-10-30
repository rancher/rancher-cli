package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type countingResolver struct {
	calls int
	p     Project
	warn  string
	err   error
}

func (r *countingResolver) ResolveProject(_ context.Context, _ Input, _ string) (Project, string, error) {
	r.calls++
	return r.p, r.warn, r.err
}

// projectRead reads the project unless its input check refuses first.
type projectRead struct {
	d      Descriptor
	refuse bool
	got    Project
	ok     bool
}

func (c *projectRead) Describe() Descriptor { return c.d }
func (c *projectRead) Execute(ctx context.Context, in Input, _ Prompter) (Result, error) {
	if c.refuse {
		return Result{}, clierr.New(clierr.CodeValidation, "input refused")
	}
	var err error
	c.got, c.ok, err = TypedProjectOn(ctx, in, "c-1")
	return Result{Kind: "X", Text: "ok"}, err
}

type hinted struct{ projectRead }

func (hinted) ProjectRefusalHint(Input) string { return "rancher x" }

func typedProject(v string) mapInput {
	return mapInput{s: map[string]string{"project": v}, is: map[string]bool{"project": true}}
}

func declaring() Descriptor {
	return Descriptor{Name: "get pod", CommandType: ReadOnly, Shape: ShapeText,
		Flags: []ArgSpec{{Name: "cluster", Type: "string", Required: true}, ProjectFlag(false, "d")}}
}

// TestProjectStageRefusesAnUndeclaredProject asserts a typed --project on a
// command that does not list it is VALIDATION, with the command's hint
// when it has one, and never reaches the resolver.
func TestProjectStageRefusesAnUndeclaredProject(t *testing.T) {
	r := &countingResolver{}
	cmd := &hinted{projectRead{d: Descriptor{Name: "project remove-namespace", CommandType: ReadOnly, Shape: ShapeText}}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, typedProject("web"), IO{Out: &out, Err: &errBuf, Format: "json", Projects: r}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, "VALIDATION", ce.Code)
	assert.Equal(t, "rancher x", ce.Hint)
	assert.Equal(t, 0, r.calls)
}

// TestProjectStageResolvesOnceAndDefersTheMissingCluster asserts a declaring
// command with a required --cluster reaches the resolver once, on its own
// read, with the missing --cluster left to it, and receives the project
// and its warning.
func TestProjectStageResolvesOnceAndDefersTheMissingCluster(t *testing.T) {
	reads := 0
	defer SetProjectReadObserverForTest(func(string) { reads++ })()
	r := &countingResolver{p: Project{Cluster: "c-1", ID: "p-1"}, warn: "w"}
	cmd := &projectRead{d: declaring()}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, typedProject("web"), IO{Out: &out, Err: &errBuf, Format: "json", Projects: r}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 1, r.calls)
	assert.Equal(t, 1, reads, "the command read the project it takes")
	assert.True(t, cmd.ok)
	assert.Equal(t, "p-1", cmd.got.ID)
	var env struct{ Warnings []string }
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, []string{"w"}, env.Warnings)
}

// TestProjectStageLeavesInputChecksAheadOfTheLookup asserts a declaring
// command whose own input check refuses before it reads the project sends
// the lookup nothing.
func TestProjectStageLeavesInputChecksAheadOfTheLookup(t *testing.T) {
	r := &countingResolver{p: Project{Cluster: "c-1", ID: "p-1"}}
	cmd := &projectRead{d: declaring(), refuse: true}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, typedProject("web"), IO{Out: &out, Err: &errBuf, Format: "json", Projects: r}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, "input refused", ce.Message)
	assert.Equal(t, 0, r.calls)
}

// TestMissingClusterErrorIsTheLookupsRefusal asserts the missing-cluster
// path returns the resolver's refusal when --project is typed and nothing
// otherwise.
func TestMissingClusterErrorIsTheLookupsRefusal(t *testing.T) {
	require.NoError(t, MissingClusterError(t.Context()))
	refusal := clierr.New(clierr.CodeValidation, "candidates")
	r := &countingResolver{err: refusal}
	ctx, _, ce := withProject(t.Context(), &projectRead{d: declaring()}, typedProject("web"), IO{Projects: r})
	require.Nil(t, ce)
	assert.Equal(t, refusal, MissingClusterError(ctx))
	assert.Equal(t, refusal, MissingClusterError(ctx))
	assert.Equal(t, 1, r.calls)
}

// TestEmptyValueIsRefusedUnlessAllowed asserts a typed empty value of any
// value-taking flag is VALIDATION, the omitted flag is not, and AllowEmpty
// exempts a flag.
func TestEmptyValueIsRefusedUnlessAllowed(t *testing.T) {
	d := Descriptor{Name: "revoke", Flags: []ArgSpec{{Name: "role", Type: "string"}, {Name: "set", Type: "stringSlice"}}}
	assert.Nil(t, validateInput(d, mapInput{}, false))
	ce := validateInput(d, mapInput{s: map[string]string{"role": ""}, is: map[string]bool{"role": true}}, false)
	require.NotNil(t, ce)
	assert.Equal(t, "--role is empty; pass a value or omit --role", ce.Message)
	ce = validateInput(d, mapInput{sl: map[string][]string{"set": {""}}, is: map[string]bool{"set": true}}, false)
	require.NotNil(t, ce)
	ce = validateInput(d, mapInput{s: map[string]string{"role": "  "}, is: map[string]bool{"role": true}}, false)
	require.NotNil(t, ce, "spaces only are empty")
	d.Flags[0].AllowEmpty = true
	assert.Nil(t, validateInput(d, mapInput{s: map[string]string{"role": ""}, is: map[string]bool{"role": true}}, false))
}

// TestProjectOnRefusesAnotherCluster asserts a project resolved on one
// cluster is never handed to a command acting on another.
func TestProjectOnRefusesAnotherCluster(t *testing.T) {
	ctx := WithProject(t.Context(), Project{Cluster: "c-1", ID: "p-1"})
	_, ok, err := projectOn(ctx, "c-2")
	require.Error(t, err)
	assert.False(t, ok)
	ce, isCE := errors.AsType[*clierr.Error](err)
	require.True(t, isCE)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	p, ok, err := projectOn(ctx, "c-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "p-1", p.ID)
}

// TestEmptyOutputAndColorAreRefused asserts a blank -o or --color is
// VALIDATION before the command runs.
func TestEmptyOutputAndColorAreRefused(t *testing.T) {
	for _, tc := range []struct{ flag, value string }{{"output", ""}, {"color", " "}} {
		cmd := &projectRead{d: declaring()}
		r := &countingResolver{}
		var out, errBuf bytes.Buffer
		in := mapInput{s: map[string]string{tc.flag: tc.value, "cluster": "c-1"}, is: map[string]bool{tc.flag: true}}
		_, ce := Run(t.Context(), cmd, in, IO{Out: &out, Err: &errBuf, Projects: r}, safety.Binding{})
		require.NotNil(t, ce, tc.flag)
		assert.Equal(t, "--"+tc.flag+" is empty; pass a value or omit --"+tc.flag, ce.Message)
		assert.False(t, cmd.ok, "the command did not run")
	}
}

// TestEmptyProjectHasItsOwnText asserts a blank --project gets the
// project's own refusal, not the generic empty-value text.
func TestEmptyProjectHasItsOwnText(t *testing.T) {
	for _, v := range []string{"", "  "} {
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), &projectRead{d: declaring()}, typedProject(v), IO{Out: &out, Err: &errBuf, Format: "json", Projects: &countingResolver{}}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, "--project is empty; pass a project id or display name, or omit --project", ce.Message, "%q", v)
	}
}

// TestOnlyClusterIsDeferred asserts a typed --project defers the missing
// --cluster to the lookup and no other required flag.
func TestOnlyClusterIsDeferred(t *testing.T) {
	d := declaring()
	d.Flags = append(d.Flags, ArgSpec{Name: "role", Type: "string", Required: true})
	r := &countingResolver{}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), &projectRead{d: d}, typedProject("web"), IO{Out: &out, Err: &errBuf, Format: "json", Projects: r}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, "--role is required", ce.Message)
	assert.Equal(t, 0, r.calls)
}

type fixedNotices struct{ n []string }

func (f *fixedNotices) DrainNotices() []string { out := f.n; f.n = nil; return out }

// TestProjectWarningPrecedesOtherNotices asserts the lookup's warning (the
// value is one project's id and another's display name) comes before the
// notices the invocation's store recorded.
func TestProjectWarningPrecedesOtherNotices(t *testing.T) {
	r := &countingResolver{p: Project{Cluster: "c-1", ID: "p-1"}, warn: "w"}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), &projectRead{d: declaring()}, typedProject("web"),
		IO{Out: &out, Err: &errBuf, Format: "json", Projects: r, Notices: &fixedNotices{n: []string{"store notice"}}}, safety.Binding{})
	require.Nil(t, ce)
	var env struct{ Warnings []string }
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, []string{"w", "store notice"}, env.Warnings)
}

// TestTypedProjectOnRefusesAProjectNobodyLookedUp asserts a command reading
// the project it takes gets the project when the pipeline looked it up,
// nothing when no --project was typed, and a GENERIC internal error -
// never an unscoped run - when --project was typed but not looked up.
func TestTypedProjectOnRefusesAProjectNobodyLookedUp(t *testing.T) {
	p, ok, err := TypedProjectOn(WithProject(t.Context(), Project{Cluster: "c-1", ID: "p-1"}), typedProject("web"), "c-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "p-1", p.ID)

	_, ok, err = TypedProjectOn(t.Context(), mapInput{}, "c-1")
	require.NoError(t, err)
	assert.False(t, ok)

	for _, in := range []mapInput{typedProject("web"), {s: map[string]string{"project": "web"}}} {
		_, ok, err = TypedProjectOn(t.Context(), in, "c-1")
		assert.False(t, ok)
		ce, isCE := errors.AsType[*clierr.Error](err)
		require.True(t, isCE, "%v", err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Equal(t, "internal error: --project was typed but no project lookup was set up before the command ran", ce.Message)
	}
}

// TestProjectStageWithoutAResolverIsAnInternalError asserts a command that
// takes --project, typed, reaching the project stage with no resolver
// set up is GENERIC before the command runs.
func TestProjectStageWithoutAResolverIsAnInternalError(t *testing.T) {
	cmd := &projectRead{d: declaring()}
	var out, errBuf bytes.Buffer
	in := typedProject("web")
	in.s["cluster"] = "c-1"
	_, ce := Run(t.Context(), cmd, in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "internal error: get pod takes --project but no project resolver is set up", ce.Message)
	assert.False(t, cmd.ok, "the command did not run")
}

// TestProjectInternalErrorMessagesStartWithThePrefix asserts every message
// of a defect in how the project stage is used starts with "internal error: "
// and has the GENERIC code.
func TestProjectInternalErrorMessagesStartWithThePrefix(t *testing.T) {
	asError := func(t *testing.T, err error) *clierr.Error {
		t.Helper()
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		return ce
	}
	for _, tc := range []struct {
		name string
		got  func(t *testing.T) *clierr.Error
		want string
	}{
		{"the project resolved on another cluster", func(t *testing.T) *clierr.Error {
			t.Helper()
			_, _, err := projectOn(WithProject(t.Context(), Project{Cluster: "c-1"}), "c-2")
			return asError(t, err)
		}, "internal error: --project was resolved on cluster c-1; the command acts on c-2"},
		{"a typed project with no lookup", func(t *testing.T) *clierr.Error {
			t.Helper()
			_, _, err := TypedProjectOn(t.Context(), typedProject("web"), "c-1")
			return asError(t, err)
		}, "internal error: --project was typed but no project lookup was set up before the command ran"},
		{"a project resolved with no cluster", func(t *testing.T) *clierr.Error {
			t.Helper()
			return asError(t, MissingClusterError(WithProject(t.Context(), Project{Cluster: "c-1"})))
		}, "internal error: --project resolved with no --cluster"},
		{"no project resolver", func(t *testing.T) *clierr.Error {
			t.Helper()
			in := typedProject("web")
			in.s["cluster"] = "c-1"
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), &projectRead{d: declaring()}, in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
			require.NotNil(t, ce)
			return ce
		}, "internal error: get pod takes --project but no project resolver is set up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce := tc.got(t)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, strings.HasPrefix(ce.Message, "internal error: "), "message %q", ce.Message)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}

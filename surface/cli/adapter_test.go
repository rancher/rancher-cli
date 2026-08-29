package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

type pingCmd struct{}

func (pingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "ping", CommandType: runtime.Local}
}
func (pingCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{Kind: "Pong", Data: map[string]string{"msg": "pong"}}, nil
}

func TestAdaptRunsAndRendersJSON(t *testing.T) {
	var out bytes.Buffer
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	root := &ucli.Command{
		Name:     "rancher",
		Writer:   &out,
		Flags:    GlobalFlags(),
		Commands: []*ucli.Command{Adapt(reg, pingCmd{})},
	}
	err := root.Run(t.Context(), []string{"rancher", "ping", "-o", "json"})
	require.NoError(t, err)

	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Pong", env["kind"])
}

type primePingCmd struct{}

func (primePingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "primeping", CommandType: runtime.Local,
		Capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}}
}
func (primePingCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{Kind: "Pong"}, nil
}

// TestPrimeDescriptionMarksHelpWithoutTouchingUsage pins the normal --help
// seam: a Prime requirement sets urfave's Description field (rendered in the
// DESCRIPTION: block by urfave's own default help template) and leaves Usage
// (which feeds the TOC, the manifest, and the command card's usage line)
// untouched.
func TestPrimeDescriptionMarksHelpWithoutTouchingUsage(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{Name: "get widget", Usage: "List widgets",
		Capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}}
	assert.Equal(t, runtime.PrimeSentence, primeDescription(d))
	assert.Equal(t, "List widgets", d.Usage, "primeDescription must never mutate Usage")

	assert.Empty(t, primeDescription(runtime.Descriptor{Name: "get pod"}), "an ordinary command gets no Description")
}

// TestAdaptedPrimeCommandShowsHelpMarker proves the marker actually reaches
// urfave's rendered --help output for an Adapt-wired command.
func TestAdaptedPrimeCommandShowsHelpMarker(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	root := &ucli.Command{
		Name:     "rancher",
		Writer:   &out,
		Flags:    GlobalFlags(),
		Commands: []*ucli.Command{Adapt(reg, primePingCmd{})},
	}
	err := root.Run(t.Context(), []string{"rancher", "primeping", "--help"})
	require.NoError(t, err)
	assert.Contains(t, out.String(), runtime.PrimeSentence)
}

// TestAdaptedPrimeCommandExecutionFailsLoudWithoutChecker closes the loop the
// runtime unit test opens: Adapt() never wires IO.Capability, so actually
// EXECUTING a requirement-bearing command through the bare path must hit the
// pipeline's loud mis-wiring error — never silently proceed.
func TestAdaptedPrimeCommandExecutionFailsLoudWithoutChecker(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	root := &ucli.Command{
		Name:      "rancher",
		Writer:    &out,
		ErrWriter: &errOut,
		Flags:     GlobalFlags(),
		Commands:  []*ucli.Command{Adapt(reg, primePingCmd{})},
		// The default handler os.Exits the test binary on the error path.
		ExitErrHandler: func(context.Context, *ucli.Command, error) {},
	}
	err := root.Run(t.Context(), []string{"rancher", "primeping"})
	require.Error(t, err)
	assert.Contains(t, errOut.String(), "no capability checker is wired")
	assert.NotContains(t, out.String(), `"Pong"`, "execution must never reach the command")
}

// TestArgsUsageRendersRequiredAndOptionalArgs pins PS-9's helper: required
// Args render "<name>", optional Args render "[name]", space-joined in
// declared order — the SAME Descriptor.Args the manifest publishes, so help
// text cannot drift from schema. Args with a non-empty Enum render the
// pipe-joined values instead of the bare name.
func TestArgsUsageRendersRequiredAndOptionalArgs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", argsUsage(nil))
	assert.Equal(t, "[command]", argsUsage([]runtime.ArgSpec{{Name: "command", Type: "string"}}))
	assert.Equal(t, "<name>", argsUsage([]runtime.ArgSpec{{Name: "name", Type: "string", Required: true}}))
	assert.Equal(t, "<name> [extra]", argsUsage([]runtime.ArgSpec{
		{Name: "name", Type: "string", Required: true},
		{Name: "extra", Type: "string"},
	}))
	// Enum: required arg renders "<v1|v2|...>" instead of "<name>".
	assert.Equal(t, "<bash|zsh|fish|pwsh>", argsUsage([]runtime.ArgSpec{
		{Name: "shell", Type: "string", Required: true, Enum: []string{"bash", "zsh", "fish", "pwsh"}},
	}))
	// Enum: optional arg renders "[v1|v2|...]" instead of "[name]".
	assert.Equal(t, "[bash|zsh|fish|pwsh]", argsUsage([]runtime.ArgSpec{
		{Name: "shell", Type: "string", Enum: []string{"bash", "zsh", "fish", "pwsh"}},
	}))
}

// TestAdaptSetsArgsUsageFromDescriptorArgs pins the Adapt wiring: the urfave
// ArgsUsage field is derived from the same Descriptor.Args the manifest
// already publishes.
func TestAdaptSetsArgsUsageFromDescriptorArgs(t *testing.T) {
	t.Parallel()
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	cmd := Adapt(reg, schemaLikeCmd{})
	assert.Equal(t, "[command]", cmd.ArgsUsage)
}

// schemaLikeCmd stands in for the real `schema` command's Descriptor (an
// optional positional arg) without depending on internal/runtime's schema_cmd
// wiring.
type schemaLikeCmd struct{}

func (schemaLikeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name: "schema", CommandType: runtime.ReadOnly,
		Args: []runtime.ArgSpec{{Name: "command", Type: "string"}},
	}
}
func (schemaLikeCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

// resolveReadOnlyFlags is the minimal flag set resolveReadOnly reads from.
func resolveReadOnlyFlags() []ucli.Flag {
	return []ucli.Flag{&ucli.StringFlag{Name: "config"}, &ucli.BoolFlag{Name: "read-only"}}
}

func runResolveReadOnly(t *testing.T, argv []string) (bool, []string) {
	t.Helper()
	var gotActive bool
	var gotSources []string
	cmd := &ucli.Command{
		Name:  "x",
		Flags: resolveReadOnlyFlags(),
		Action: func(_ context.Context, c *ucli.Command) error {
			gotActive, gotSources = resolveReadOnly(c)
			return nil
		},
	}
	require.NoError(t, cmd.Run(t.Context(), argv))
	return gotActive, gotSources
}

func TestResolveReadOnlyNoSourcesActive(t *testing.T) {
	active, sources := runResolveReadOnly(t, []string{"x", "--config", t.TempDir()})
	assert.False(t, active)
	assert.Empty(t, sources)
}

func TestResolveReadOnlyFlagSource(t *testing.T) {
	active, sources := runResolveReadOnly(t, []string{"x", "--config", t.TempDir(), "--read-only"})
	assert.True(t, active)
	assert.Equal(t, []string{"--read-only"}, sources)
}

func TestResolveReadOnlyEnvSourceNonEmptyIncludingFalse(t *testing.T) {
	t.Setenv("RANCHER_CLI_READONLY", "false")
	active, sources := runResolveReadOnly(t, []string{"x", "--config", t.TempDir()})
	assert.True(t, active, "RANCHER_CLI_READONLY=false is still non-empty, so the one-way rule turns read-only ON")
	assert.Equal(t, []string{"RANCHER_CLI_READONLY"}, sources)
}

func TestResolveReadOnlyConfigSource(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))
	active, sources := runResolveReadOnly(t, []string{"x", "--config", dir})
	assert.True(t, active)
	assert.Equal(t, []string{"config readOnly"}, sources)
}

func TestResolveReadOnlyAllThreeCombineOrdered(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))
	t.Setenv("RANCHER_CLI_READONLY", "1")
	_, sources := runResolveReadOnly(t, []string{"x", "--config", dir, "--read-only"})
	assert.Equal(t, []string{"config readOnly", "RANCHER_CLI_READONLY", "--read-only"}, sources)
}

func TestBuildIOReadsReadOnlyOverride(t *testing.T) {
	t.Cleanup(func() { config.SetReadOnlyOverride(false, nil) })
	config.SetReadOnlyOverride(true, []string{"RANCHER_CLI_READONLY"})

	var got runtime.IO
	cmd := &ucli.Command{
		Name: "x",
		Action: func(_ context.Context, c *ucli.Command) error {
			got = buildIO(c)
			return nil
		},
	}
	require.NoError(t, cmd.Run(t.Context(), []string{"x"}))
	assert.True(t, got.ReadOnly)
	assert.Equal(t, []string{"RANCHER_CLI_READONLY"}, got.ReadOnlySources)
}

// sliceEchoCmd captures what the adapted input delivered.
type sliceEchoCmd struct {
	got        []string
	setPresent bool
	boolOn     bool
	boolSet    bool
}

func (c *sliceEchoCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "slice-echo",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			{Name: "set", Type: "stringSlice"},
			{Name: "monitoring", Type: "bool"},
		},
		Shape: runtime.ShapeTabular,
	}
}

// emptyTabularData is a Tabular fixture with no rows — the table-format
// closed-world contract requires Tabular Data even for a payload that
// carries nothing to display.
type emptyTabularData struct{}

func (emptyTabularData) Columns() []string { return []string{"NAME"} }
func (emptyTabularData) Rows() [][]string  { return nil }

func (c *sliceEchoCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	c.got = in.Strings("set")
	c.setPresent = in.IsSet("set")
	c.boolOn = in.Bool("monitoring")
	c.boolSet = in.IsSet("monitoring")
	return runtime.Result{Kind: "Echo", Data: emptyTabularData{}}, nil
}

// TestAdaptedStringSliceFlagRoundTrip pins MC-6a/MC-5a through the real
// urfave adapter: a stringSlice descriptor becomes a StringSliceFlag whose
// occurrences arrive ordered and NEVER comma-split, and IsSet reports flag
// presence for tri-state booleans.
func TestAdaptedStringSliceFlagRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("occurrences ordered, commas preserved, presence reported", func(t *testing.T) {
		t.Parallel()
		echo := &sliceEchoCmd{}
		cmd := Adapt(runtime.NewRegistry(runtime.MapResolver(provider.NewResolver())), echo)
		cmd.Writer = &bytes.Buffer{}
		cmd.ErrWriter = &bytes.Buffer{}
		err := cmd.Run(t.Context(), []string{"slice-echo",
			"--set", "tags=a,b", "--set", "region=nyc3", "--monitoring=false"})
		require.NoError(t, err)
		assert.Equal(t, []string{"tags=a,b", "region=nyc3"}, echo.got,
			"one ordered entry per occurrence; no comma splitting")
		assert.True(t, echo.setPresent)
		assert.False(t, echo.boolOn)
		assert.True(t, echo.boolSet, "--monitoring=false is present-and-false, not absent")
	})

	t.Run("omitted flags report absent", func(t *testing.T) {
		t.Parallel()
		echo := &sliceEchoCmd{}
		cmd := Adapt(runtime.NewRegistry(runtime.MapResolver(provider.NewResolver())), echo)
		cmd.Writer = &bytes.Buffer{}
		cmd.ErrWriter = &bytes.Buffer{}
		err := cmd.Run(t.Context(), []string{"slice-echo"})
		require.NoError(t, err)
		assert.Empty(t, echo.got)
		assert.False(t, echo.setPresent)
		assert.False(t, echo.boolSet)
	})
}

package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestTestBinaryForcesRecordingOff asserts a test binary is off by the
// first control, whatever the other controls say, unless a test turned
// recording on.
func TestTestBinaryForcesRecordingOff(t *testing.T) {
	got := Resolve(ControlInput{Getenv: envOf(map[string]string{EnvTelemetry: "true"}), Config: ConfigOn})
	assert.Equal(t, Decision{On: false, By: ControlTestBinary}, got)
	assert.Equal(t, Decision{On: false, By: ControlTestBinary}, Resolve(ControlInput{}))

	restore := SetEnabledForTest()
	defer restore()
	assert.Equal(t, Decision{On: true, By: ControlDefault}, Resolve(ControlInput{}))
}

// TestControlsFirstMatchWins asserts the order of the controls and the
// value each accepts, with the test-binary check passed.
func TestControlsFirstMatchWins(t *testing.T) {
	t.Cleanup(SetEnabledForTest())
	cases := []struct {
		name string
		env  map[string]string
		cfg  ConfigSetting
		want Decision
	}{
		{"default is on", nil, ConfigUnset, Decision{On: true, By: ControlDefault}},
		{"variable true", map[string]string{EnvTelemetry: "true"}, ConfigUnset, Decision{On: true, By: ControlVariable}},
		{"variable true beats config off", map[string]string{EnvTelemetry: "true"}, ConfigOff, Decision{On: true, By: ControlVariable}},
		{"variable false beats config on", map[string]string{EnvTelemetry: "false"}, ConfigOn, Decision{On: false, By: ControlVariable}},
		{"empty variable is unset", map[string]string{EnvTelemetry: ""}, ConfigOff, Decision{On: false, By: ControlConfig}},
		{"variable on is ignored", map[string]string{EnvTelemetry: "on"}, ConfigOff, Decision{On: false, By: ControlConfig, IgnoredVariable: "on"}},
		{"variable off is ignored", map[string]string{EnvTelemetry: "off"}, ConfigUnset, Decision{On: true, By: ControlDefault, IgnoredVariable: "off"}},
		{"variable 1 is ignored", map[string]string{EnvTelemetry: "1"}, ConfigOn, Decision{On: true, By: ControlConfig, IgnoredVariable: "1"}},
		{"uppercase variable is ignored", map[string]string{EnvTelemetry: "FALSE"}, ConfigUnset, Decision{On: true, By: ControlDefault, IgnoredVariable: "FALSE"}},
		{"variable yes is ignored", map[string]string{EnvTelemetry: "yes"}, ConfigOff, Decision{On: false, By: ControlConfig, IgnoredVariable: "yes"}},
		{"variable with a leading space is ignored", map[string]string{EnvTelemetry: " true"}, ConfigUnset, Decision{On: true, By: ControlDefault, IgnoredVariable: " true"}},
		{"do not track is not read", map[string]string{"DO_NOT_TRACK": "1"}, ConfigUnset, Decision{On: true, By: ControlDefault}},
		{"config on", nil, ConfigOn, Decision{On: true, By: ControlConfig}},
		{"config off", nil, ConfigOff, Decision{On: false, By: ControlConfig}},
		{"an invalid key leaves the default", nil, ConfigInvalid, Decision{On: true, By: ControlDefault}},
		{"an invalid key under a false variable", map[string]string{EnvTelemetry: "false"}, ConfigInvalid, Decision{On: false, By: ControlVariable}},
		{"a config that failed to load is off", nil, ConfigUnreadable, Decision{On: false, By: ControlConfig}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Resolve(ControlInput{Getenv: envOf(c.env), Config: c.cfg}), c.name)
	}
}

// TestControlsListFollowsResolve asserts Controls lists the controls in the
// order Resolve checks them: with every input set, each control in turn is
// the first to match as the controls before it are removed.
func TestControlsListFollowsResolve(t *testing.T) {
	all := ControlInput{Getenv: envOf(map[string]string{EnvTelemetry: "false"}), Config: ConfigOff}
	got := []Control{Resolve(all).By}

	t.Cleanup(SetEnabledForTest())
	got = append(got, Resolve(all).By)
	all.Getenv = nil
	got = append(got, Resolve(all).By)
	all.Config = ConfigUnset
	got = append(got, Resolve(all).By)
	assert.Equal(t, Controls(), got)
}

// TestEveryControlConstantIsListed asserts a source scan of the package
// finds every constant of type Control and that Controls lists each exactly
// once.
func TestEveryControlConstantIsListed(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var declared []Control
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Control" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					require.True(t, ok, "a Control constant must be a string literal")
					text, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					declared = append(declared, Control(text))
				}
			}
		}
	}
	require.NotEmpty(t, declared)
	assert.ElementsMatch(t, declared, Controls(), "every Control constant appears in Controls exactly once")
}

// TestDefaultIsOnOutsideATestBinary asserts the default without the test
// override, as a shipped binary sees it.
func TestDefaultIsOnOutsideATestBinary(t *testing.T) {
	old := underTest
	underTest = func() bool { return false }
	t.Cleanup(func() { underTest = old })
	assert.Equal(t, Decision{On: true, By: ControlDefault}, Resolve(ControlInput{}))
	assert.Equal(t, Decision{On: true, By: ControlDefault}, Resolve(ControlInput{Getenv: envOf(map[string]string{"DO_NOT_TRACK": "1"})}))
}

// TestConfigSettingOf asserts the config key's text: only the exact texts
// true and false count.
func TestConfigSettingOf(t *testing.T) {
	text := func(s string) *string { return &s }
	assert.Equal(t, ConfigUnset, ConfigSettingOf(nil))
	for in, want := range map[string]ConfigSetting{
		"true": ConfigOn, "false": ConfigOff,
		"on": ConfigInvalid, "off": ConfigInvalid, "True": ConfigInvalid, "FALSE": ConfigInvalid,
		" true": ConfigInvalid, "false ": ConfigInvalid, "": ConfigInvalid, "maybe": ConfigInvalid,
	} {
		assert.Equal(t, want, ConfigSettingOf(text(in)), in)
	}
}

// TestSetServerHolder asserts the holder keeps the last call and resets.
func TestSetServerHolder(t *testing.T) {
	ResetServerForTest()
	t.Cleanup(ResetServerForTest)
	url, ca := Server()
	assert.Empty(t, url)
	assert.Empty(t, ca)
	SetServer("https://a.example", "ca-a")
	SetServer("https://b.example", "")
	url, ca = Server()
	assert.Equal(t, "https://b.example", url)
	assert.Empty(t, ca)
	SetServer("", "ca")
	url, _ = Server()
	assert.Empty(t, url, "an empty URL counts as not set")
}

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mapInput struct {
	s map[string]string
	b map[string]bool
}

func (m mapInput) String(k string) string { return m.s[k] }
func (m mapInput) Bool(k string) bool     { return m.b[k] }
func (m mapInput) Args() []string         { return nil }

type localCmd struct{ executed *bool }

func (localCmd) Describe() Descriptor                               { return Descriptor{Name: "login", CommandType: Local} }
func (localCmd) Plan(context.Context, Input) (safety.Effect, error) { return safety.Effect{}, nil }
func (l localCmd) Execute(context.Context, Input, safety.Effect) (Result, error) {
	*l.executed = true
	return Result{Kind: "LoginResult", Data: map[string]string{"ok": "true"}}, nil
}

func TestDryRunRejectedOnLocalNeverExecutes(t *testing.T) {
	executed := false
	var out, errBuf bytes.Buffer
	cerr := Run(context.Background(), localCmd{&executed}, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true})
	require.NotNil(t, cerr)
	assert.Equal(t, clierr.CodeDryRunNotApplicable, cerr.Code)
	assert.False(t, executed, "login must NOT run under --dry-run")
}

func TestLocalExecutesWhenNoDryRun(t *testing.T) {
	executed := false
	var out, errBuf bytes.Buffer
	cerr := Run(context.Background(), localCmd{&executed}, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"})
	require.Nil(t, cerr)
	assert.True(t, executed)
	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "LoginResult", env["kind"])
}

type mutCmd struct {
	effect   safety.Effect
	executed *bool
}

func (mutCmd) Describe() Descriptor                                 { return Descriptor{Name: "clusters scale", CommandType: Mutating} }
func (m mutCmd) Plan(context.Context, Input) (safety.Effect, error) { return m.effect, nil }
func (m mutCmd) Execute(context.Context, Input, safety.Effect) (Result, error) {
	*m.executed = true
	return Result{Kind: "ScaleResult"}, nil
}

func TestMutatingRefusesWithComputedHint(t *testing.T) {
	executed := false
	var out, errBuf bytes.Buffer
	cerr := Run(context.Background(), mutCmd{safety.Effect{Destructive: true}, &executed}, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"}) // no --apply, no --yes
	require.NotNil(t, cerr)
	assert.Equal(t, clierr.CodeConfirmationRequired, cerr.Code)
	assert.Contains(t, cerr.Hint, "--apply")
	assert.Contains(t, cerr.Hint, "--yes")
	assert.False(t, executed)
}

func TestMutatingExecutesWithAllFlags(t *testing.T) {
	executed := false
	var out, errBuf bytes.Buffer
	cerr := Run(context.Background(), mutCmd{safety.Effect{Destructive: true}, &executed}, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true})
	require.Nil(t, cerr)
	assert.True(t, executed)
}

type rawCmd struct{}

func (rawCmd) Describe() Descriptor                               { return Descriptor{Name: "token", CommandType: Local} }
func (rawCmd) Plan(context.Context, Input) (safety.Effect, error) { return safety.Effect{}, nil }
func (rawCmd) Execute(context.Context, Input, safety.Effect) (Result, error) {
	return Result{Raw: true, Kind: "ExecCredential",
		Data: map[string]any{"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential"}}, nil
}

func TestRawResultBypassesEnvelope(t *testing.T) {
	var out, errBuf bytes.Buffer
	cerr := Run(context.Background(), rawCmd{}, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"})
	require.Nil(t, cerr)
	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", env["apiVersion"])
	assert.NotContains(t, out.String(), "cli.rancher.io/v1")
}

type flaggedCmd struct{}

func (flaggedCmd) Describe() Descriptor {
	return Descriptor{Name: "x", CommandType: Local, Flags: []ArgSpec{
		{Name: "token", Type: "string", Required: true},
		{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}},
	}}
}
func (flaggedCmd) Plan(context.Context, Input) (safety.Effect, error) { return safety.Effect{}, nil }
func (flaggedCmd) Execute(context.Context, Input, safety.Effect) (Result, error) {
	return Result{Kind: "OK"}, nil
}

func TestValidateInput(t *testing.T) {
	var out, errBuf bytes.Buffer
	t.Run("required missing", func(t *testing.T) {
		cerr := Run(context.Background(), flaggedCmd{}, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"})
		require.NotNil(t, cerr)
		assert.Equal(t, clierr.CodeValidation, cerr.Code)
	})
	t.Run("bad enum", func(t *testing.T) {
		in := mapInput{s: map[string]string{"token": "id:sec", "auth-flow": "bogus"}}
		cerr := Run(context.Background(), flaggedCmd{}, in, IO{Out: &out, Err: &errBuf, Format: "json"})
		require.NotNil(t, cerr)
		assert.Equal(t, clierr.CodeValidation, cerr.Code)
	})
}

func TestQuietErrorStillRendersJSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	cerr := Run(context.Background(), flaggedCmd{}, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "quiet"})
	require.NotNil(t, cerr)
	assert.Equal(t, clierr.CodeValidation, cerr.Code)
	assert.NotEmpty(t, errBuf.String(), "error must not be silent under --quiet")
	var env map[string]any
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env), "error envelope must be valid JSON under --quiet")
	assert.Equal(t, "Error", env["kind"])
	data := env["data"].(map[string]any)
	assert.NotEmpty(t, data["error"], "error envelope must contain error details")
}

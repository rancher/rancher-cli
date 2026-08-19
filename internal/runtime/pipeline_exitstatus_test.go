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

type exitStatusCmd struct {
	exitDeclCmd
	status int
}

func (c exitStatusCmd) Execute(ctx context.Context, in Input, p Prompter) (Result, error) {
	res, err := c.exitDeclCmd.Execute(ctx, in, p)
	res.ExitStatus = c.status
	return res, err
}

// TestRunHonorsDeclaredExitStatus: a declared status is surfaced only after
// the result rendered fully on stdout.
func TestRunHonorsDeclaredExitStatus(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	streams := IO{Out: &out, Err: &errBuf, Format: "json"}
	cmd := exitStatusCmd{exitDeclCmd{name: "probe", codes: []int{0, 7}}, 7}
	status, ce := Run(t.Context(), cmd, mapInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 7, status)

	var env struct {
		Kind string            `json:"kind"`
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "probe", env.Kind)
	assert.Equal(t, "ok", env.Data["probe"])
	assert.Empty(t, errBuf.String())
}

// TestRunRejectsUndeclaredExitStatus: an undeclared status is an internal
// error — JSON error envelope on stderr, nothing on stdout, status 0.
func TestRunRejectsUndeclaredExitStatus(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	streams := IO{Out: &out, Err: &errBuf, Format: "json"}
	cmd := exitStatusCmd{exitDeclCmd{name: "probe", codes: nil}, 7}
	status, ce := Run(t.Context(), cmd, mapInput{}, streams, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, 0, status)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Empty(t, out.Bytes())

	var env struct {
		Data struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, "GENERIC", env.Data.Error.Code)
}

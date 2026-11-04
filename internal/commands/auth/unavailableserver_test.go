package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/rancher/rancher-cli/internal/ui"
)

const unavailableBody = `{"type":"https://developers.cloudflare.com/error-1033/","title":"Error 1033","status":530}`

// renderedError is the failure envelope as an agent parses it.
func renderedError(t *testing.T, ce *clierr.Error) (code string, retryable bool) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, runtime.RenderError(&buf, "json", ce))
	var env struct {
		Data struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env), buf.String())
	return env.Data.Error.Code, env.Data.Error.Retryable
}

// TestLoginAndStatusReadAnUnavailableServerAndARefusedToken asserts, for a
// server that answers 530 with a Retry-After header of 120 seconds and for
// one that answers 401, that auth status and the verify step of auth login
// make one request and end at once: 530 is a retryable GENERIC error that
// starts with the server and the status, 401 is AUTH with the exit code it
// has for every command; the failure envelope keeps its code and retryable
// fields; a login that fails stores no token. Not parallel: the token comes
// from the environment.
func TestLoginAndStatusReadAnUnavailableServerAndARefusedToken(t *testing.T) {
	for _, status := range []int{530, 401} {
		answer := func(t *testing.T) *testtransport.Answers {
			t.Helper()
			return testtransport.NewAnswers(t, true, status, "120", "application/json", unavailableBody)
		}
		check := func(t *testing.T, srv *testtransport.Answers, err error) {
			t.Helper()
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 1, srv.Count())
			code, retryable := renderedError(t, ce)
			assert.Equal(t, ce.Code, code)
			assert.Equal(t, ce.Retryable, retryable)
			if status == 401 {
				assert.Equal(t, clierr.CodeAuth, ce.Code)
				assert.Equal(t, 3, ce.ExitCode())
				return
			}
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, 1, ce.ExitCode())
			assert.True(t, ce.Retryable)
			assert.Contains(t, ce.Message, config.DisplayServerURL(srv.URL())+" answered HTTP 530: a proxy or load balancer in front of Rancher could not reach Rancher - retry later")
		}
		t.Run("auth status "+strconv.Itoa(status), func(t *testing.T) {
			t.Setenv("RANCHER_CLI_TOKEN", "id:secret")
			srv := answer(t)
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL(), srv.CertPEM(), true)
			cmd := &authCheckCmd{
				dir: dir, store: config.NewFileStore(dir),
				preflight:      provider.Preflight,
				rancherVersion: func(context.Context, config.Effective) (string, error) { return "v2.15.0", nil },
			}
			start := time.Now()
			_, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
			assert.Less(t, time.Since(start), 5*time.Second)
			check(t, srv, err)
		})
		t.Run("auth login verify "+strconv.Itoa(status), func(t *testing.T) {
			srv := answer(t)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL(), srv.CertPEM(), false)
			in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
			start := time.Now()
			_, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
			assert.Less(t, time.Since(start), 5*time.Second)
			check(t, srv, err)
			tok, _ := store.GetToken(config.ServerRef{Name: "prod"})
			assert.Empty(t, tok, "a login that failed stores no token")
		})
	}
}

// TestLoginReadsAnUnavailableServerAtTheProviderListAsItAlwaysDid asserts a
// sign-in against a server that answers 530 with a Retry-After header ends
// at the provider list with one request, the retryable GENERIC error and the
// text the list request has always had, which names no server: the exec
// plugin reaches the same request and keeps that text.
func TestLoginReadsAnUnavailableServerAtTheProviderListAsItAlwaysDid(t *testing.T) {
	srv := testtransport.NewAnswers(t, true, 530, "120", "application/json", unavailableBody)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL(), srv.CertPEM(), false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	start := time.Now()
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	assert.Less(t, time.Since(start), 5*time.Second)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 1, srv.Count())
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, `error listing auth providers: an error on the server ("status 530") has prevented the request from succeeding - retrying may succeed`)
	assert.NotContains(t, ce.Message, "answered HTTP")
}

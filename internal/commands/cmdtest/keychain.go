package cmdtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// SecurityExitError is the error go-keyring returns on macOS when
// /usr/bin/security exits with status: an *exec.ExitError that carries
// the status and nothing else. Status 128 is a locked keychain whose
// unlock prompt was cancelled.
func SecurityExitError(t *testing.T, status int) error {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to produce an exit status")
	}
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	_, ok := errors.AsType[*exec.ExitError](err)
	require.True(t, ok, "got %v", err)
	return err
}

// FailingKeychain is a keyring seam whose selected calls fail with Err
// and whose other calls go to go-keyring's in-memory mock. A locked
// keychain fails every call; one that locks between the read and the
// delete of a logout fails only the delete.
type FailingKeychain struct {
	Err                          error
	FailGet, FailSet, FailDelete bool
}

func (k FailingKeychain) Set(service, user, password string) error {
	if k.FailSet {
		return k.Err
	}
	return keyring.Set(service, user, password)
}

func (k FailingKeychain) Get(service, user string) (string, error) {
	if k.FailGet {
		return "", k.Err
	}
	return keyring.Get(service, user)
}

func (k FailingKeychain) Delete(service, user string) error {
	if k.FailDelete {
		return k.Err
	}
	return keyring.Delete(service, user)
}

// ErrorFields is the error of a JSON envelope as an agent reads it.
type ErrorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
}

// DecodeErrorEnvelope decodes the JSON error envelope a command printed.
func DecodeErrorEnvelope(t *testing.T, out []byte) ErrorFields {
	t.Helper()
	var env struct {
		Kind string `json:"kind"`
		Data struct {
			Error *ErrorFields `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out, &env), "%s", out)
	require.Equal(t, "Error", env.Kind, "%s", out)
	require.NotNil(t, env.Data.Error, "%s", out)
	return *env.Data.Error
}

// RenderedError renders err as the CLI prints it with -o json and returns
// the decoded error and the exit status it maps to.
func RenderedError(t *testing.T, err error) (ErrorFields, int) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "not a structured error: %v", err)
	var buf bytes.Buffer
	require.NoError(t, runtime.RenderError(&buf, "json", ce))
	return DecodeErrorEnvelope(t, buf.Bytes()), ce.ExitCode()
}

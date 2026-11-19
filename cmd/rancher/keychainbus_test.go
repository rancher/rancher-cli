package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoginStartsNoSessionBusWithoutOne asserts, for main run as a process
// on Linux with no session bus address and none at /run/user/<uid>, that a
// login with the default credential store runs no dbus-launch (a fake one
// on PATH records any call), exits 0, and stores the token in the file
// store, which `config status` reports as the pin.
func TestLoginStartsNoSessionBusWithoutOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process test runs on Linux only")
	}
	u, err := user.Current()
	require.NoError(t, err)
	for _, name := range []string{"bus", "dbus-session"} {
		if _, err := os.Stat(filepath.Join("/run/user", u.Uid, name)); err == nil {
			t.Skipf("this host has a session bus at /run/user/%s/%s", u.Uid, name)
		}
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "dbus-launch-ran")
	fake := fmt.Sprintf("#!/bin/sh\n: > '%s'\nexit 1\n", marker)
	testfs.WriteExecutable(t, filepath.Join(bin, "dbus-launch"), []byte(fake))
	env := []string{
		"PATH=" + bin,
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"GOCOVERDIR=" + t.TempDir(),
		config.EnvConfigDir + "=" + t.TempDir(),
		mainChildBusCheckEnv + "=1",
	}
	work := t.TempDir()

	_, stderr, code := runMainProcessStdin(t, strings.NewReader("t-user:secret\n"), env, work,
		"auth", "login", "t", "--url", "https://example.invalid", "--token-stdin", "--no-verify")
	require.Equal(t, 0, code, stderr)
	assert.NoFileExists(t, marker, "dbus-launch ran")

	stdout, stderr, code := runMainProcess(t, env, work, "config", "status", "-o", "json")
	require.Equal(t, 0, code, stderr)
	assert.NoFileExists(t, marker, "dbus-launch ran")
	var out struct {
		Data struct {
			Pin string `json:"pin"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	assert.Equal(t, "file", out.Data.Pin)
}

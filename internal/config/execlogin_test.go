package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExecLoginEntryName asserts the plugin login entry carries the
// reserved prefix over the shared host derivation, and that the namespace
// test matches exactly the names that prefix produces.
func TestExecLoginEntryName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "exec-login-rancher.example.com-8443", ExecLoginEntryName("https://rancher.example.com:8443"))
	assert.Equal(t, "exec-login-unknown", ExecLoginEntryName("::not a url"))
	assert.True(t, IsExecLoginEntryName("exec-login-rancher.example.com"))
	assert.True(t, IsExecLoginEntryName("exec-login-"))
	assert.False(t, IsExecLoginEntryName("exec-login"))
	assert.False(t, IsExecLoginEntryName("prod"))
	assert.Equal(t, "exec-login-", ExecLoginPrefix)
}

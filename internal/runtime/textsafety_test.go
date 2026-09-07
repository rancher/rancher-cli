package runtime

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
)

// TestMirrorWarningsStripsControlCharacters asserts a warning carrying
// server text cannot drive the terminal or forge a second warning line.
func TestMirrorWarningsStripsControlCharacters(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	mirrorWarnings(&buf, []string{"condition: a\x1b[2Jb\n# warning: forged"}, nil, style.Style{})
	assert.Equal(t, "# warning: condition: a[2Jb# warning: forged\n", buf.String())
}

// TestErrorTextStripsControlCharacters asserts the default-format error
// block strips control characters from the message, the hint, and the
// candidates, all of which may carry server text.
func TestErrorTextStripsControlCharacters(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "denied: a\x1b[2Jb\nforged line").
		WithHint("rancher get x\r").
		WithCandidates([]clierr.Candidate{{Name: "n\x1b[0m", ID: "id\n"}})
	out := errorText(ce, style.Style{})
	assert.NotContains(t, out, "\x1b")
	assert.NotContains(t, out, "\r")
	assert.Contains(t, out, "denied: a[2Jbforged line")
	assert.Contains(t, out, "candidates: n[0m (id)")
}

// TestDryRunTextStripsControlCharacters asserts a diff value from the
// server's dry-run answer cannot forge a preview line.
func TestDryRunTextStripsControlCharacters(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{Action: safety.ActionCreate, DisplayKind: "project", DisplayName: "web",
		Diff: []safety.DiffEntry{{Path: "/spec/description", From: "", To: "a\nb\x1b[2J"}}}
	out := dryRunText(eff)
	assert.Contains(t, out, "/spec/description:  -> ab[2J")
	assert.NotContains(t, out, "\x1b")
}

package install_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTruncatedScriptMidFunctionExecutesNothing asserts that a download cut
// inside a function body cannot execute a partial install: the sh interpreter
// rejects the malformed input and the home directory is unchanged.
//
// The cut is placed inside main()'s body, past all phase-function definitions.
// This byte offset (> 17 396) exceeds the pre-wrap install.sh length, so
// truncating that version at the same offset leaves it intact and it runs in
// full. With RANCHER_CLI_VERSION set the pre-wrap script bypasses network
// resolution, reaches `note "downloading ..."`, and writes to stdout before
// the fetch fails — the `require.Empty(out)` assertion below would fail
// against the pre-wrap script, proving the wrap is load-bearing.
func TestTruncatedScriptMidFunctionExecutesNothing(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../install.sh")
	require.NoError(t, err)

	// Locate a deterministic cut point inside main()'s body, after all phase
	// function definitions. The sequence below is unique to main().
	marker := []byte("  stage\n  link\n  write_receipt\n")
	idx := bytes.Index(content, marker)
	require.Greater(t, idx, -1, "cut marker must be present in install.sh")
	cutAt := idx + 4 // four bytes into "  stage\n", well inside main()

	// Assert the cut lands inside a function body: an opening-brace line
	// exists before the cut with no closing-brace line between it and the cut.
	lastBrace := bytes.LastIndex(content[:cutAt], []byte("{\n"))
	require.Greater(t, lastBrace, -1, "cut must follow a function opening brace")
	require.Equal(t, -1, bytes.Index(content[lastBrace:cutAt], []byte("\n}")),
		"cut must be inside a function body, not past its closing brace")

	truncPath := filepath.Join(t.TempDir(), "install-truncated.sh")
	require.NoError(t, os.WriteFile(truncPath, content[:cutAt], 0o755))

	home := t.TempDir()
	tmp := t.TempDir()
	homeSnap := digestTree(t, home)
	tmpSnap := digestTree(t, tmp)

	var out, errb strings.Builder
	cmd := exec.Command("sh", truncPath)
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = []string{
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"PATH=" + toolPath(t, baseTools...),
		// RANCHER_CLI_VERSION bypasses network resolution in the pre-wrap
		// script so it reaches the note("downloading ...") stdout line,
		// making require.Empty(out) below a discriminating assertion.
		"RANCHER_CLI_VERSION=v1.2.3",
		// http:// is rejected by curl's --proto '=https', so the pre-wrap
		// script fails fast at fetch without an actual network connection.
		"RANCHER_CLI_INSTALL_BASE_URL=http://127.0.0.1:1",
	}
	require.Error(t, cmd.Run(), "truncated script mid-function must exit non-zero (syntax error)")
	require.Empty(t, out.String(), "syntax error must produce no stdout — pre-wrap script would output 'downloading ...' here")
	require.Equal(t, homeSnap, digestTree(t, home),
		"truncated script must not write anything to HOME")
	require.Equal(t, tmpSnap, digestTree(t, tmp),
		"truncated script must not write anything to TMPDIR")
}

// TestTruncatedScriptAtFunctionBoundaryIsSilentNoOp documents the accepted
// silent-no-op property: a download cut exactly after the last complete
// function definition but before `main "$@"` leaves the interpreter with
// only declarations and no entry point. Exit 0, nothing installed — the
// same trade-off rustup accepts.
func TestTruncatedScriptAtFunctionBoundaryIsSilentNoOp(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../install.sh")
	require.NoError(t, err)

	// A well-formed file ends with \n, so splitting on "\n" yields an empty
	// last element. The second-to-last element is the final non-empty line.
	lines := strings.Split(string(content), "\n")
	require.Empty(t, lines[len(lines)-1], "install.sh must end with a newline")
	lastLine := lines[len(lines)-2]
	require.Equal(t, `main "$@"`, lastLine,
		"last non-empty line of install.sh must be the main invocation")

	// Everything up to but not including the `main "$@"` line.
	truncated := []byte(strings.Join(lines[:len(lines)-2], "\n") + "\n")

	truncPath := filepath.Join(t.TempDir(), "install-truncated.sh")
	require.NoError(t, os.WriteFile(truncPath, truncated, 0o755))

	home := t.TempDir()
	tmp := t.TempDir()
	homeSnap := digestTree(t, home)
	tmpSnap := digestTree(t, tmp)

	var out, errb strings.Builder
	cmd := exec.Command("sh", truncPath)
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = []string{
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"PATH=" + toolPath(t, baseTools...),
	}
	require.NoError(t, cmd.Run(),
		"truncated-at-boundary script must exit 0 (no entry point, nothing to fail)")
	require.Equal(t, homeSnap, digestTree(t, home),
		"truncated-at-boundary script must not write anything to HOME")
	require.Equal(t, tmpSnap, digestTree(t, tmp),
		"truncated-at-boundary script must not write anything to TMPDIR")
	require.Empty(t, out.String(), "must produce no stdout")
	require.Empty(t, errb.String(), "must produce no stderr")
}

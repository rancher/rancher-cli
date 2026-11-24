package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
)

// TestFilteredTextConstructorsMatchTheStringForms asserts that TextLinesOf,
// MutationTextOf and StyleEveryLineBoldOf give the text and the styled text
// the string forms give for the same lines.
func TestFilteredTextConstructorsMatchTheStringForms(t *testing.T) {
	t.Parallel()
	enabled := style.Style{Enabled: true}

	got := TextLinesOf(clierr.Plain("server x added"), clierr.Plain("url: https://x.example.com"), clierr.Plain("ca: pinned"))
	want := TextLines("server x added", "url: https://x.example.com", "ca: pinned")
	assert.Equal(t, want.Text, got.Text)
	assert.Equal(t, want.StyledText(enabled), got.StyledText(enabled))

	got = MutationTextOf("token", clierr.Plain("t-1"), "created", clierr.Plain("expires: never"))
	want = MutationText("token", "t-1", "created", "expires: never")
	assert.Equal(t, want.Text, got.Text)
	assert.Equal(t, "token/t-1 created\n  expires: never\n", got.Text)
	assert.Equal(t, want.StyledText(enabled), got.StyledText(enabled))

	text := "cluster/a changed nothing\ncluster/a upgraded\n"
	assert.Equal(t, StyleEveryLineBold(text)(enabled), StyleEveryLineBoldOf(clierr.Plain(text))(enabled))
}

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/token"
)

// unprintableValues are values a command cannot print so that every supported
// shell reads them as written.
var unprintableValues = map[string]string{
	"zero width space":       "abc\u200b",
	"quote":                  "a'b",
	"right single quotation": "a\u2019b",
	"two backslashes":        `a\\b`,
}

// TestLogoutRerunIsLeftOutForATargetThatCannotBePrinted asserts the closing
// line of a logout that stopped keeps the fact that the credentials are gone
// and says the command is left out, with no logout command in it, when a
// target name cannot be printed; the logout of every server stays a command.
func TestLogoutRerunIsLeftOutForATargetThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	cancelled := errors.Join(errors.New("locking the credential-helper records"), context.Canceled)
	detailsOf := func(t *testing.T, rerun clierr.Command) LogoutAllPartialFailure {
		t.Helper()
		err := logoutCmd{}.localFailure(&sweepFailure{sweep: cancelled}, &token.Deletion{}, token.RevocationReport{}, rerun, nil)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		details, ok := ce.Details.(LogoutAllPartialFailure)
		require.True(t, ok)
		return details
	}
	closing := func(details LogoutAllPartialFailure) string {
		lines := details.TextDetails()
		return lines[len(lines)-1].String()
	}
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			details := detailsOf(t, logoutRerun([]string{value}, false))
			assert.Empty(t, details.Rerun)
			line := closing(details)
			assert.Equal(t, "those credentials are gone regardless of this failure; "+clierr.LeftOutNote, line)
			assert.NotContains(t, line, "auth logout")
		})
	}
	all := detailsOf(t, logoutRerun([]string{"a'b"}, true))
	assert.Equal(t, clierr.Rancher+" auth logout --all", all.Rerun)
	assert.Contains(t, closing(all), "to finish, run: "+clierr.Rancher+" auth logout --all")
	one := detailsOf(t, logoutRerun([]string{"prod"}, false))
	assert.Contains(t, closing(one), "to finish, run: "+clierr.Rancher+" auth logout 'prod'")
}

// TestLoginRemediesLeaveOutTheCommandForAnEntryNameThatCannotBePrinted asserts
// the texts and hints of a login that name an entry give no command and carry
// the note once when the entry name cannot be printed.
func TestLoginRemediesLeaveOutTheCommandForAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for what, text := range map[string]string{
				"the CA from the variable": loginVariableRemedy(false, value),
				"save the CA":              loginSaveCARemedy(value),
				"no saved CA notice":       variableNotice(value, ""),
				"different CA notice":      variableNotice(value, "/etc/saved.pem"),
			} {
				assert.NotContains(t, text, "config update-server", what)
				assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), what)
			}

			collision := nameCollisionError(value, "https://old.example.com", "https://new.example.com", false)
			assert.Empty(t, collision.Hint)
			assert.Contains(t, collision.Message, "is registered at https://old.example.com")
			assert.Equal(t, 1, strings.Count(collision.Message, clierr.LeftOutNote))

			byURL := nameCollisionError("prod", "https://old.example.com", "https://o"+value+".example.com", false)
			assert.Empty(t, byURL.Hint)
			assert.Equal(t, 1, strings.Count(byURL.Message, clierr.LeftOutNote))

			cfg := &config.Config{Servers: map[string]config.Server{value: {URL: "https://r.example.com"}}}
			_, err := resolveLoginTarget(cfg, value, "", "/etc/ca.pem")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Empty(t, ce.Hint)
			assert.Contains(t, ce.Message, "--cacert without --url changes trust")
			assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote))
		})
	}
}

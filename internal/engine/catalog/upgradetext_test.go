package catalog

import (
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installWord matches the word install in any of its forms the texts of an
// upgrade must not hold; "uninstall" has no word boundary before "install".
var installWord = regexp.MustCompile(`(?i)\binstall(s|ed)?\b`)

// installCommands are the command names that may hold the word install in a
// text about an upgrade: the install command a refusal names, and the
// uninstall command a remedy names.
var installCommands = strings.NewReplacer("rancher app install", "", "app uninstall", "")

// upgradeWaitFor is the wait of an upgrade that says what the texts need: a
// request id, the cleanup option and a wait that cannot read the Operation.
func upgradeWaitFor(mode waitMode) *appWait {
	return &appWait{
		t:  actionTarget{typedCluster: testTypedCluster, namespace: testNS, release: testRelease, requestID: testRequestID},
		op: actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, filter: Values{}.filterFunc(),
		mode: mode, cleanupOnFail: true, skip: true, timeout: appMargin,
	}
}

// errorTexts is the message and the hint of an error, which are all the
// words a reader sees of it besides its details.
func errorTexts(err error) []string {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		return []string{err.Error()}
	}
	return []string{ce.Message, ce.Hint}
}

// upgradeTextBuilders builds the texts of each error constructor an upgrade
// routes to, in upgrade mode: the wait's errors, the skip warning of its
// preview and result, and the error of a request whose answer did not say
// whether Rancher ran it. The failure of an upgrade is the constructor
// failureError gives in upgrade mode.
var upgradeTextBuilders = map[string]func(w *appWait) []string{
	"timeoutError": func(w *appWait) []string {
		return errorTexts(w.timeoutError(clierr.Retryable(clierr.CodeGeneric, "the server answered 503")))
	},
	"failureError": func(w *appWait) []string {
		return errorTexts(w.failureError(revision{status: "failed", description: "hook failed", version: 4}, &stall{message: "exit code: 1"}))
	},
	"anotherChangeError": func(w *appWait) []string { return errorTexts(w.anotherChangeError(4)) },
	"anotherChangeAfterFailureError": func(w *appWait) []string {
		return errorTexts(w.anotherChangeAfterFailureError(stall{message: "exit code: 1"}, 4))
	},
	"leftPendingError":       func(w *appWait) []string { return errorTexts(w.leftPendingError(&stall{message: "exit code: 137"})) },
	"atStatusError":          func(w *appWait) []string { return errorTexts(w.atStatusError(revision{status: "superseded"})) },
	"deployedButFailedError": func(w *appWait) []string { return errorTexts(w.deployedButFailedError(stall{message: "exit code: 1"})) },
	"failedAtStatusError": func(w *appWait) []string {
		return errorTexts(w.failedAtStatusError(stall{message: "exit code: 1"}, revision{status: "superseded"}))
	},
	"noRevisionError": func(w *appWait) []string {
		return append(errorTexts(w.noRevisionError(stall{message: "exit code: 1"}, &revision{status: "deployed", version: 3})),
			errorTexts(w.noRevisionError(stall{}, nil))...)
	},
	"cutOffError":    func(w *appWait) []string { return errorTexts(w.cutOffError()) },
	"cancelledError": func(w *appWait) []string { return errorTexts(w.cancelledError()) },
	"skipWarning": func(w *appWait) []string {
		return []string{skipWarning(w.mode, true, testNS), skipWarning(w.mode, false, testNS)}
	},
	"outcomeUnknown": func(w *appWait) []string {
		return errorTexts(outcomeUnknown(w.t, &engine.ActionOutcomeUnknown{Err: errors.New("connection reset")}, Values{}))
	},
}

// TestEveryUpgradeTextNamesTheUpgrade asserts no message or hint of an error
// constructor an upgrade routes to, built in upgrade mode, holds the word
// install, installs or installed outside the command names rancher app
// install and app uninstall, and that the same table in install mode does
// hold it: without that the check could never fail.
func TestEveryUpgradeTextNamesTheUpgrade(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(upgradeTextBuilders)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			texts := upgradeTextBuilders[name](upgradeWaitFor(modeUpgrade))
			require.NotEmpty(t, texts)
			for _, text := range texts {
				assert.NotRegexp(t, installWord, installCommands.Replace(text), "the text names the install: %s", text)
			}
		})
	}
	t.Run("install mode names the install", func(t *testing.T) {
		t.Parallel()
		var holders []string
		for _, name := range slices.Sorted(maps.Keys(upgradeTextBuilders)) {
			for _, text := range upgradeTextBuilders[name](upgradeWaitFor(modeInstall)) {
				if installWord.MatchString(installCommands.Replace(text)) {
					holders = append(holders, name)
					break
				}
			}
		}
		assert.Contains(t, holders, "anotherChangeError")
		assert.Contains(t, holders, "failureError")
	})
}

// TestUpgradeTextRowsCoverTheErrorConstructors asserts the table of texts
// holds a row for every constructor of waiterrors.go the wait of an upgrade can
// reach, and for none of the constructors that only an uninstall reaches.
func TestUpgradeTextRowsCoverTheErrorConstructors(t *testing.T) {
	t.Parallel()
	uninstallOnly := []string{"uninstalledButFailedError", "staysInstalledError", "keptHistoryError"}
	reached := map[string]bool{}
	for _, name := range waitErrorBuilderNames(t) {
		reached[name] = true
	}
	for _, name := range uninstallOnly {
		delete(reached, name)
	}
	// The builders that carry no text of their own, and the upgrade's
	// failure, which failureError builds in upgrade mode.
	for _, name := range []string{"committedError", "abortedError", "passThroughError", "upgradeFailureError"} {
		delete(reached, name)
	}
	for name := range reached {
		assert.Contains(t, upgradeTextBuilders, name, "%s has no row in the table of upgrade texts", name)
	}
	for name := range upgradeTextBuilders {
		assert.NotContains(t, uninstallOnly, name)
	}
}

// hintRules are the rules a hint follows: one command with concrete values,
// or none.
var (
	hintPlaceholder = regexp.MustCompile(`<[^<>\s]+>`)
	hintOptional    = regexp.MustCompile(`\[[^\[\]]+\]`)
)

// hintViolation says which rule a hint breaks, or "".
func hintViolation(hint string) string {
	switch {
	case hint == "":
		return ""
	case !strings.HasPrefix(hint, clierr.Rancher+" "):
		return "a hint is one rancher command"
	case strings.Contains(hint, "run:"):
		return `a "run:" prefix`
	case strings.Contains(hint, " OR "):
		return `an " OR " between two commands`
	case hintOptional.MatchString(hint):
		return "an [optional] part"
	case hintPlaceholder.MatchString(hint):
		return "a <placeholder>"
	case strings.Count(hint, clierr.Rancher+" ") > 1:
		return "more than one rancher command"
	}
	return ""
}

// commandsIn lists the rancher commands a message writes: each runs from
// "rancher " to the next semicolon or the end of the message.
func commandsIn(message string) []string {
	var out []string
	rest := message
	for {
		i := strings.Index(rest, clierr.Rancher+" ")
		if i < 0 {
			return out
		}
		rest = rest[i:]
		end := strings.Index(rest, ";")
		if end < 0 {
			end = len(rest)
		}
		out = append(out, strings.TrimRight(rest[:end], " ."))
		rest = rest[end:]
	}
}

// commandViolation says which rule a command written in a message breaks, or
// "": a command that changes a release carries no gate flag, since the
// message tells the reader to add them after reading the preview.
func commandViolation(command string) string {
	for _, mutating := range []string{" app install", " app upgrade", " app uninstall"} {
		if strings.HasPrefix(command, clierr.Rancher+mutating) &&
			(strings.Contains(command, " --apply") || strings.Contains(command, " --yes")) {
			return "a command in a message carries no gate flag"
		}
	}
	return ""
}

// TestUpgradeRemediesPassTheTextRules asserts the three remedies of an
// upgrade that are prose, the denied read of the release Secret, the release
// that is uninstalled and the chart that is not the App's, put the remedy in
// the message and give a hint that is one concrete command or none, and
// no command a message writes carries a gate flag.
func TestUpgradeRemediesPassTheTextRules(t *testing.T) {
	t.Parallel()
	rows := map[string]func(t *testing.T) *clierr.Error{
		"the denied read of the release Secret": func(t *testing.T) *clierr.Error {
			t.Helper()
			f := upgradeFake(t, nil)
			f.root.answers[releaseSecretPath(releaseSecretName(3))] = rootAnswer{status: http.StatusForbidden, body: `{"message":"forbidden"}`}
			return upgradeRefusal(t, f, nil, upgradeInput())
		},
		"the release that is uninstalled": func(t *testing.T) *clierr.Error {
			t.Helper()
			f := upgradeFake(t, nil, func(a *upgradeApp) { a.status = "uninstalled" })
			return upgradeRefusal(t, f, nil, upgradeInput())
		},
		"the chart that is not the App's": func(t *testing.T) *clierr.Error {
			t.Helper()
			return upgradeRefusal(t, upgradeFake(t, nil), nil, upgradeInput(withFlag(flagChart, "other")))
		},
	}
	for name, build := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ce := build(t)
			assert.Empty(t, hintViolation(ce.Hint), "hint %q", ce.Hint)
			assert.NotContains(t, ce.Message, "\n")
			for _, command := range commandsIn(ce.Message) {
				assert.Empty(t, commandViolation(command), "message command %q", command)
			}
		})
	}
	t.Run("the rules refuse what they name", func(t *testing.T) {
		t.Parallel()
		for _, hint := range []string{"run: rancher get app", "rancher get app <release>", "rancher get app [x]", "rancher a OR rancher b", "rancher a; rancher b", "get app"} {
			assert.NotEmpty(t, hintViolation(hint), hint)
		}
		for _, command := range commandsIn("install it with " + clierr.Rancher + " app install --apply --yes; then") {
			assert.NotEmpty(t, commandViolation(command), command)
		}
		for _, command := range commandsIn("run " + clierr.Rancher + " app install --cluster 'prod' --dry-run 'x'") {
			assert.Empty(t, commandViolation(command), command)
		}
	})
}

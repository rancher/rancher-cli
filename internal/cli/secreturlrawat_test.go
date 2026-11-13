package cli

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userinfoURLRE matches a server URL with userinfo in a row: a scheme and an
// authority that holds an @. rawAtURLRE matches one with an @ after the host,
// as the rows write it: the canary, then the @.
var (
	userinfoURLRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^/?#\s]*@[^/?#\s]*`)
	rawAtURLRE    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^/?#\s]*/` + regexp.QuoteMeta(urlCanary+"@x"))
)

// rowForms is what a row says about the server URL it feeds: which forms the
// URLs of its command line, environment, stored entry and earlier command
// lines take, and a key that is the row with each of those URLs replaced by a
// placeholder. Two rows with the same key differ only in the form of the URL.
// A URL a record holds is part of the key and is not read as a form: the
// rows of a record's server have their own cases.
type rowForms struct {
	key     string
	userURL bool
	rawAt   bool
}

func formsOf(urlParts []string, other ...any) rowForms {
	var f rowForms
	for _, p := range urlParts {
		f.userURL = f.userURL || userinfoURLRE.MatchString(p)
		f.rawAt = f.rawAt || rawAtURLRE.MatchString(p)
	}
	// A newline separates the parts: the URL patterns stop at whitespace.
	key := strings.Join(urlParts, "\n") + fmt.Sprintf("\n%+v", other)
	key = userinfoURLRE.ReplaceAllString(key, "{form}")
	f.key = rawAtURLRE.ReplaceAllString(key, "{form}")
	return f
}

// refusalForms is formsOf for a row of secretURLRows.
func refusalForms(r secretRow) rowForms {
	parts := slices.Clone(r.argv)
	for _, k := range slices.Sorted(maps.Keys(r.setEnv)) {
		parts = append(parts, k+"="+r.setEnv[k])
	}
	for _, line := range r.before {
		parts = append(parts, line...)
	}
	var stored any
	if r.stored != nil {
		parts = append(parts, r.stored.url)
		stored = *r.stored
	}
	return formsOf(parts, r.flag, r.env, r.stdin, stored, r.code)
}

// keptForms is formsOf for a row of secretOKRows.
func keptForms(r secretOKRow) rowForms {
	parts := append(slices.Clone(r.argv), r.stored.url)
	return formsOf(parts, r.stdin, r.stored, r.seed != nil)
}

// rawAtParityAllowlist names a row about userinfo that has no row for an @
// after the host, by its name, with the reason.
var rawAtParityAllowlist = map[string]string{
	"config delete-server x, json, the entry holds user information on an ftp server and a login": "a scheme that is not http or https gives its own reason; an @ after the host is refused whatever the scheme, which the http rows assert",
}

// assertRawAtParity asserts that every row whose URL has userinfo has a row of
// the same command with an @ after the host in its place, unless the allowlist
// names it with a reason.
func assertRawAtParity(t *testing.T, names []string, forms []rowForms) {
	t.Helper()
	require.Len(t, forms, len(names))
	paired := map[string]bool{}
	for _, f := range forms {
		if f.rawAt {
			paired[f.key] = true
		}
	}
	for name, reason := range rawAtParityAllowlist {
		assert.NotEmpty(t, reason, "the allowlist entry %q has no reason", name)
	}
	for i, f := range forms {
		if !f.userURL || paired[f.key] {
			continue
		}
		if _, skipped := rawAtParityAllowlist[names[i]]; skipped {
			continue
		}
		assert.Failf(t, "no row for an @ after the host", "the row %q has userinfo in its URL and no row of the same command has an @ after the host", names[i])
	}
}

// TestEveryUserinfoRowHasARowForAnAtSignAfterTheHost asserts that each row of
// the server URL tables whose server URL has userinfo has a row of the same
// command line, environment and stored entry whose URL has an @ after the host
// instead, unless the allowlist names it with a reason, and that both forms of
// the refusal rows feed the same flags and variables. Rows are paired by the
// URL they hold, not by their names. It asserts parity of the rows that exist:
// a new route that accepts a server URL is found by the route scan of the
// conformance package.
func TestEveryUserinfoRowHasARowForAnAtSignAfterTheHost(t *testing.T) {
	refusals := secretURLRows()
	var names []string
	var forms []rowForms
	feeds := map[string]map[string]bool{"userinfo": {}, "raw @": {}}
	for _, r := range refusals {
		names = append(names, r.name)
		f := refusalForms(r)
		forms = append(forms, f)
		for form, on := range map[string]bool{"userinfo": f.userURL, "raw @": f.rawAt} {
			if on {
				feeds[form]["flag "+r.flag+" env "+r.env] = true
			}
		}
	}
	t.Run("refusals", func(t *testing.T) { assertRawAtParity(t, names, forms) })
	t.Run("refusals feed the same flags and variables", func(t *testing.T) {
		assert.Equal(t, slices.Sorted(maps.Keys(feeds["userinfo"])), slices.Sorted(maps.Keys(feeds["raw @"])))
	})

	names, forms = nil, nil
	for _, r := range secretOKRows() {
		names = append(names, r.name)
		forms = append(forms, keptForms(r))
	}
	t.Run("commands that keep working", func(t *testing.T) { assertRawAtParity(t, names, forms) })
	t.Run("the allowlist names rows that exist", func(t *testing.T) {
		for name := range rawAtParityAllowlist {
			assert.Contains(t, names, name)
		}
	})
}

// TestAnAtSignAfterTheHostRefusalLeavesTheRestOfTheURLOut asserts that for
// each refusal row of a URL with an @ after the host (a row that is not
// refused is left out), the message and the
// hint of the error hold no byte of the part after the host (the canary and
// the text @x that follows it), neither does anything written to standard
// output, and the process exit status is the one of the row's error code (2
// for VALIDATION). Not parallel: it sets the process environment.
func TestAnAtSignAfterTheHostRefusalLeavesTheRestOfTheURLOut(t *testing.T) {
	restoreRawArgv(t)
	rows := 0
	for _, row := range secretURLRows() {
		if row.noRefusal || !refusalForms(row).rawAt {
			continue
		}
		rows++
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, code := runSecretRow(t, row)
			require.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			assert.Equal(t, clierr.ExitCode(row.code), code, "stderr: %s", stderr)
			ce := renderedError(t, "json", stderr)
			for _, text := range []string{ce.Message, ce.Hint, stdout} {
				assert.NotContains(t, text, urlCanary)
				assert.NotContains(t, text, "@x")
			}
		})
	}
	require.NotZero(t, rows, "the table has rows for an @ after the host")
}

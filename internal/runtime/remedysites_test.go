package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/auth"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/token"
)

// plantedRemedy is the text a server could send to look like a remedy:
// U+2063 followed by the word rancher, which a server, a file or another
// process can send.
const plantedRemedy = "\u2063rancher delete x"

// siteInput is the Input of a command that takes no flags and no arguments.
type siteInput struct{}

func (siteInput) String(string) string    { return "" }
func (siteInput) Bool(string) bool        { return false }
func (siteInput) Strings(string) []string { return nil }
func (siteInput) IsSet(string) bool       { return false }
func (siteInput) Args() []string          { return nil }

// siteRead is a read command that prints a notice through its prompter and
// returns its result and error.
type siteRead struct {
	notice string
	res    runtime.Result
	err    error
}

func (siteRead) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "config status", CommandType: runtime.Local}
}

func (c siteRead) Execute(_ context.Context, _ runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	if c.notice != "" {
		p.Notice(c.notice)
	}
	return c.res, c.err
}

// siteMutating is a mutating command with a fixed preview and result.
type siteMutating struct {
	d   runtime.Descriptor
	eff safety.Effect
	res runtime.Result
}

func (c siteMutating) Describe() runtime.Descriptor { return c.d }

func (c siteMutating) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return c.eff, func(context.Context) (runtime.Result, error) { return c.res, nil }, nil
}

// siteNotices is a notice source that holds the notices it was given.
type siteNotices struct{ notices []string }

func (s *siteNotices) DrainNotices() []string {
	out := s.notices
	s.notices = nil
	return out
}

// siteRun is one run of a site: the text in its channel, the output format
// and the config directory the invocation names. For a row of an error,
// cli, when it is not empty, is a command of the CLI put in another field of
// the same error.
type siteRun struct {
	text, cli, format, dir string
}

// io is the invocation's IO, writing to the two buffers.
func (r siteRun) io(out, errBuf *bytes.Buffer) runtime.IO {
	return runtime.IO{Out: out, Err: errBuf, Format: r.format, RemedyDir: r.dir}
}

// siteRow is one row of the table: the site it drives, as the enclosing
// function and the expansion it calls, the channel it plants text in, and
// the function that drives the site and returns what the run wrote to
// standard output and standard error.
type siteRow struct {
	site, channel string
	// jsonOnly is true for a channel the text output does not print, which has
	// no text run.
	jsonOnly bool
	// textOnly is true for a channel only the text output prints, which has
	// no json run.
	textOnly bool
	drive    func(t *testing.T, r siteRun) (stdout, stderr string)
}

// drivenResult runs a command that returns res and err through Run.
func drivenResult(t *testing.T, r siteRun, cmd runtime.Command, mutate func(*runtime.IO)) (string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	streams := r.io(&out, &errBuf)
	if mutate != nil {
		mutate(&streams)
	}
	_, _ = runtime.Run(t.Context(), cmd, siteInput{}, streams, safety.Binding{})
	return out.String(), errBuf.String()
}

// reportResult is a successful result that renders in every format.
func reportResult() runtime.Result {
	return runtime.Result{Kind: "Report", Data: map[string]string{"state": "ok"}, Text: "ok\n"}
}

// errorRows are the channels of an error that both ExpandError sites
// expand: its message, hint and warning, and every remedy field of each
// production details payload that has a MapText method. The payload of a
// row is the type of the literal that is the first argument of withDetails.
var errorRows = []struct {
	channel  string
	jsonOnly bool
	textOnly bool
	build    func(text string) *clierr.Error
}{
	{"message", false, false, func(s string) *clierr.Error { return clierr.New(clierr.CodeGeneric, "failed: "+s) }},
	{"hint", false, false, func(s string) *clierr.Error { return clierr.New(clierr.CodeGeneric, "failed").WithHint(s) }},
	{"warning", false, false, func(s string) *clierr.Error { return clierr.New(clierr.CodeGeneric, "failed").WithWarnings(s) }},
	{"LoginFailureDetails.Primary.Message", false, false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{Primary: clierr.PrimaryFailure{Message: s}})
	}},
	// The text form (LoginFailureDetails.TextDetails) never prints Primary.Hint.
	{"LoginFailureDetails.Primary.Hint", true, false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{Primary: clierr.PrimaryFailure{Hint: s}})
	}},
	{"LoginFailureDetails.Primary.Details", false, false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{Primary: clierr.PrimaryFailure{Details: clierr.CommittedContextDetails{Recovery: s}}})
	}},
	{"LoginFailureDetails.CredentialRecovery.Command", false, false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{CredentialRecovery: &clierr.CredentialRecovery{Command: s}})
	}},
	{"LoginFailureDetails.ConfigRecovery.Command", false, false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{ConfigRecovery: &clierr.ConfigRecovery{Entry: "p", Command: s}})
	}},
	{"LoginFailureDetails.Recovery", false, false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{Recovery: s})
	}},
	{"CommittedContextDetails.Recovery", false, false, func(s string) *clierr.Error {
		return withDetails(clierr.CommittedContextDetails{Recovery: s})
	}},
	{"RevokePartialFailure.Failed.Message", false, false, func(s string) *clierr.Error {
		return withDetails(rbac.RevokePartialFailure{Failed: rbac.RevokeFailure{Name: "b-1", Code: clierr.CodeGeneric, Message: s}})
	}},
	{"DeleteProjectPartialFailure.Objects.Message", false, false, func(s string) *clierr.Error {
		return withDetails(resources.DeleteProjectPartialFailure{Objects: []safety.Object{{Kind: "namespace", Name: "ns", Outcome: safety.OutcomeFailed, Code: clierr.CodeGeneric, Message: s}}})
	}},
	{"RevocationReport.NotRevoked.Error", false, false, func(s string) *clierr.Error {
		return withDetails(token.RevocationReport{NotRevoked: []token.NotRevokedToken{liveToken(func(n *token.NotRevokedToken) { n.Error = s })}})
	}},
	{"RevocationReport.NotRevoked.Recovery", true, false, func(s string) *clierr.Error {
		return withDetails(token.RevocationReport{NotRevoked: []token.NotRevokedToken{liveToken(func(n *token.NotRevokedToken) { n.Recovery = []string{s} })}})
	}},
	{"DeleteTokenFailure.NotRevoked.Error", false, false, func(s string) *clierr.Error {
		return withDetails(token.DeleteTokenFailure{NotRevoked: []token.NotRevokedToken{liveToken(func(n *token.NotRevokedToken) { n.Error = s })}})
	}},
	{"DeleteTokenFailure.NotRevoked.Recovery", true, false, func(s string) *clierr.Error {
		return withDetails(token.DeleteTokenFailure{NotRevoked: []token.NotRevokedToken{liveToken(func(n *token.NotRevokedToken) { n.Recovery = []string{s} })}})
	}},
	// Rerun is not part of the structured output.
	{"LogoutAllPartialFailure.Rerun", false, true, func(s string) *clierr.Error {
		return withDetails(auth.LogoutAllPartialFailure{LoggedOut: []string{"a"}, Rerun: s})
	}},
	{"LogoutAllPartialFailure.NotRevoked.Error", false, false, func(s string) *clierr.Error {
		return withDetails(auth.LogoutAllPartialFailure{LoggedOut: []string{"a"}, NotRevoked: []token.NotRevokedToken{liveToken(func(n *token.NotRevokedToken) { n.Error = s })}})
	}},
	{"LogoutAllPartialFailure.NotRevoked.Recovery", true, false, func(s string) *clierr.Error {
		return withDetails(auth.LogoutAllPartialFailure{LoggedOut: []string{"a"}, NotRevoked: []token.NotRevokedToken{liveToken(func(n *token.NotRevokedToken) { n.Recovery = []string{s} })}})
	}},
}

// liveToken is a token that may still be live, after a certificate failure
// that prints its error message in the text output, with set applied.
func liveToken(set func(*token.NotRevokedToken)) token.NotRevokedToken {
	n := token.NotRevokedToken{
		Ref:    &token.TokenRef{Kind: token.TokenKindExt, Name: "t-1"},
		Source: "derived", Server: "https://r.invalid", Reason: "unreachable", Code: clierr.CodeTLSTrust, Retry: "yes",
	}
	set(&n)
	return n
}

func withDetails(d clierr.TextDetails) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, "failed").WithDetails(d)
}

// withOtherCommand is ce with the command of the CLI in a field other than
// the one channel names: the hint, or the message for the hint row.
func withOtherCommand(ce *clierr.Error, channel, cli string) *clierr.Error {
	if cli == "" {
		return ce
	}
	if channel == "hint" {
		ce.Message += "; run " + cli
		return ce
	}
	return ce.WithHint(cli)
}

// siteRows is the table, written as data: one row for each channel of each
// place a text is expanded for the invocation. The two occurrences of
// IO.ExpandError have one row for the message, the hint, the warning and each
// remedy field of a details payload. The scans in test/conformance compare
// the keys of the rows (the site, and the payload and field of an error row)
// with the sites and payloads of the production tree; they cannot tell
// whether a row's drive runs the site its key names, so the drive of each row
// must run that site, and a row that stands in for a site of another package
// says so.
func siteRows() []siteRow {
	rows := []siteRow{
		{site: "internal/runtime.runRead ExpandText", channel: "prompter notice", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, siteRead{notice: r.text, res: reportResult()}, nil)
		}},
		{site: "internal/runtime.renderResult expandAll", channel: "drained notice", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, siteRead{res: reportResult()}, func(io *runtime.IO) { io.Notices = &siteNotices{[]string{r.text}} })
		}},
		{site: "internal/runtime.renderResult expandAll", channel: "result warning", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			res := reportResult()
			res.Warnings = []string{r.text}
			return drivenResult(t, r, siteRead{res: res}, nil)
		}},
		{site: "internal/runtime.renderResult ExpandText", channel: "once-only notice", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			res := reportResult()
			res.OnceOnlyNotice = r.text
			res.Warnings = []string{r.text}
			res.CommittedContext = &runtime.CommittedContext{ResourceKind: "token", ResourceName: "t"}
			cmd := siteMutating{
				d:   runtime.Descriptor{Name: "create widget", CommandType: runtime.Mutating, Delivery: runtime.DeliveryOnceOnly},
				eff: safety.Effect{Action: safety.ActionCreate, DisplayKind: "token", DisplayName: "t"},
				res: res,
			}
			return drivenResult(t, r, cmd, func(io *runtime.IO) { io.Apply = true })
		}},
		{site: "internal/runtime.renderEffect expandAll", channel: "drained notice", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, dryRun(safety.Effect{}), func(io *runtime.IO) {
				io.DryRun = true
				io.Notices = &siteNotices{[]string{r.text}}
			})
		}},
		{site: "internal/runtime.renderEffect expandAll", channel: "effect warning", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, dryRun(safety.Effect{Warnings: []string{r.text}}), func(io *runtime.IO) { io.DryRun = true })
		}},
		{site: "internal/runtime.renderErrDrained expandAll", channel: "drained notice", jsonOnly: false, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			cmd := siteRead{err: clierr.New(clierr.CodeGeneric, "failed")}
			return drivenResult(t, r, cmd, func(io *runtime.IO) { io.Notices = &siteNotices{[]string{r.text}} })
		}},
	}
	rows = append(rows, textRows()...)
	rows = append(rows,
		siteRow{site: "internal/runtime.renderResult expandData", channel: "result data", jsonOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, siteRead{res: runtime.Result{Kind: "Report", Data: map[string]string{"next": r.text}}}, nil)
		}},
		siteRow{site: "internal/runtime.renderEffect expandData", channel: "dry-run envelope", jsonOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, previewCommand(r.text), func(io *runtime.IO) { io.DryRun = true })
		}},
	)
	for _, row := range errorRows {
		rows = append(rows,
			siteRow{site: "internal/runtime.renderErrDrained ExpandError", channel: row.channel, jsonOnly: row.jsonOnly, textOnly: row.textOnly, drive: func(t *testing.T, r siteRun) (string, string) {
				t.Helper()
				return drivenResult(t, r, siteRead{err: withOtherCommand(row.build(r.text), row.channel, r.cli)}, nil)
			}},
			// renderDepsError is in package cli, which this package cannot
			// import: the row drives IO.ExpandError with RenderErrorStyled,
			// the two calls that function makes.
			siteRow{site: "internal/cli.renderDepsError ExpandError", channel: row.channel, jsonOnly: row.jsonOnly, textOnly: row.textOnly, drive: func(t *testing.T, r siteRun) (string, string) {
				t.Helper()
				var out, errBuf bytes.Buffer
				require.NoError(t, runtime.RenderErrorStyled(&errBuf, r.format, r.io(&out, &errBuf).ExpandError(withOtherCommand(row.build(r.text), row.channel, r.cli)), style.Style{}))
				return out.String(), errBuf.String()
			}},
		)
	}
	rows = append(rows, siteRow{
		// completionDebugf is in package cli, which this package cannot import:
		// the row drives IO.ExpandText and writes the line the way the
		// completion trace does.
		site: "internal/cli.completionDebugf ExpandText", channel: "completion trace", textOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			var out, errBuf bytes.Buffer
			_, err := fmt.Fprintf(&errBuf, "completion +1ms %s\n", r.io(&out, &errBuf).ExpandText(r.text))
			require.NoError(t, err)
			return out.String(), errBuf.String()
		}})
	return rows
}

// dryRun is a mutating command whose preview is eff, named and shown.
func dryRun(eff safety.Effect) runtime.Command {
	eff.Action, eff.DisplayKind, eff.DisplayName = safety.ActionUpdate, "Cluster", "c"
	return siteMutating{d: runtime.Descriptor{Name: "clusters scale", CommandType: runtime.Mutating}, eff: eff}
}

// previewCommand is a mutating command whose preview holds displayName in
// the field that keeps a command of the CLI: DisplayName is not filtered by
// output.Printable, which removes the marker from the diff lines and the
// object names.
func previewCommand(displayName string) runtime.Command {
	return siteMutating{
		d:   runtime.Descriptor{Name: "clusters scale", CommandType: runtime.Mutating},
		eff: safety.Effect{Action: safety.ActionUpdate, DisplayKind: "Cluster", DisplayName: displayName},
	}
}

// textRows are the rows of the text channels of a result and of the dry-run
// preview. Each row builds its own result: the styled and wide forms need Text
// beside them, and a summary needs tabular data and no Text.
func textRows() []siteRow {
	return []siteRow{
		{site: "internal/runtime.renderResult ExpandLines", channel: "result text", textOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, resultCommand(runtime.ShapeText, runtime.Result{Text: r.text + "\n"}), nil)
		}},
		{site: "internal/runtime.renderResult ExpandLines", channel: "styled text", textOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			res := runtime.Result{Text: r.text + "\n", StyledText: func(s style.Style) string { return s.Bold(r.text) + "\n" }}
			return drivenResult(t, r, resultCommand(runtime.ShapeText, res), func(io *runtime.IO) { io.Color = "always" })
		}},
		{site: "internal/runtime.renderResult ExpandLines", channel: "wide text", textOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			res := runtime.Result{Text: "narrow\n", WideText: r.text + "\n"}
			return drivenResult(t, r, resultCommand(runtime.ShapeText, res), func(io *runtime.IO) { io.Format = "wide" })
		}},
		{site: "internal/runtime.renderResult ExpandLines", channel: "summary print", textOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, resultCommand(runtime.ShapeTabular, runtime.Result{Data: summaryTable{}, Summary: r.text}), nil)
		}},
		{site: "internal/runtime.renderEffect ExpandLines", channel: "preview text", textOnly: true, drive: func(t *testing.T, r siteRun) (string, string) {
			t.Helper()
			return drivenResult(t, r, previewCommand(r.text), func(io *runtime.IO) { io.DryRun = true })
		}},
	}
}

// siteRowCounts is the number of rows each site has, kept apart from the
// rows: a site with a row added or removed fails until this list matches.
// The scan in test/conformance holds the sites themselves.
var siteRowCounts = map[string]int{
	"internal/runtime.runRead ExpandText":           1,
	"internal/runtime.renderResult expandAll":       2,
	"internal/runtime.renderResult ExpandText":      1,
	"internal/runtime.renderResult ExpandLines":     4,
	"internal/runtime.renderResult expandData":      1,
	"internal/runtime.renderEffect expandAll":       2,
	"internal/runtime.renderEffect ExpandLines":     1,
	"internal/runtime.renderEffect expandData":      1,
	"internal/runtime.renderErrDrained ExpandError": 19,
	"internal/runtime.renderErrDrained expandAll":   1,
	"internal/cli.renderDepsError ExpandError":      19,
	"internal/cli.completionDebugf ExpandText":      1,
}

// visibleText is every string a run wrote, as a reader sees it: a JSON
// document decoded, any other output as it is.
func visibleText(t *testing.T, outputs ...string) string {
	t.Helper()
	var parts []string
	for _, o := range outputs {
		if strings.HasPrefix(strings.TrimSpace(o), "{") {
			var doc any
			if err := json.Unmarshal([]byte(o), &doc); err == nil {
				parts = append(parts, jsonStrings(doc)...)
				continue
			}
		}
		parts = append(parts, o)
	}
	return strings.Join(parts, "\n")
}

func jsonStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, jsonStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range x {
			out = append(out, k)
			out = append(out, jsonStrings(e)...)
		}
		return out
	}
	return nil
}

// TestEverySiteThatExpandsRemediesIgnoresServerText asserts, for every row of
// the table, with a config directory that is not the default and in the text
// and the json output, that text holding U+2063 and the word rancher in the
// row's channel is written without the directory, and that a command of the
// CLI names it, in four runs: the planted text alone, the command alone, both
// in the one text, and (for an error) the planted text in the row's channel
// with the command in another field. A text whose channel the output does not
// print has no text run, only the json run; a channel only the text output
// prints runs in the default, table and wide formats and has no json run, and
// its command-alone run holds no character of the remedy marker (the wide row
// writes the wide form in each of the three).
func TestEverySiteThatExpandsRemediesIgnoresServerText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	cliCommand := clierr.Rancher + " get pods"
	plantedExpanded := "rancher" + prefix + " delete x"
	cliExpanded := "rancher" + prefix + " get pods"
	both := "server said " + plantedRemedy + "; run " + cliCommand
	for _, row := range siteRows() {
		formats := []string{"", "json"}
		if row.textOnly {
			formats = []string{"", "table", "wide"}
		}
		for _, format := range formats {
			if format == "" && row.jsonOnly {
				continue
			}
			t.Run(row.site+" "+row.channel+" "+format, func(t *testing.T) {
				t.Parallel()
				run := func(r siteRun) string {
					r.format, r.dir = format, dir
					out, errOut := row.drive(t, r)
					if row.textOnly && r.text == cliCommand {
						assert.False(t, holdsMarkerRune(out+errOut), "a character of the marker is left in the text output: %q", out)
					}
					shown := visibleText(t, out, errOut)
					assert.NotContains(t, shown, plantedExpanded, "the planted text gets no directory")
					assert.NotContains(t, shown, clierr.RemedyMarker, "no marker is left")
					return shown
				}
				shown := run(siteRun{text: plantedRemedy})
				assert.Contains(t, shown, "rancher delete x", "the planted text reaches the output")
				assert.NotContains(t, shown, "--config", "no directory in a text that holds no command of the CLI")

				shown = run(siteRun{text: cliCommand})
				assert.Contains(t, shown, cliExpanded, "the command of the CLI names the directory")

				shown = run(siteRun{text: both})
				assert.Contains(t, shown, "rancher delete x", "the planted text reaches the output")
				assert.Contains(t, shown, cliExpanded, "the command of the CLI names the directory beside the planted text")

				if strings.Contains(row.site, "ExpandError") {
					shown = run(siteRun{text: plantedRemedy, cli: cliCommand})
					assert.Contains(t, shown, "rancher delete x", "the planted text reaches the output")
					assert.Contains(t, shown, cliExpanded, "the command in another field names the directory")
				}
			})
		}
	}
}

// TestSiteRowsAreTheRowsOfTheListedSites asserts the rows of the table are
// the ones the list names, with their counts, so that a site that gains or
// loses a channel fails until its rows and the list agree.
func TestSiteRowsAreTheRowsOfTheListedSites(t *testing.T) {
	t.Parallel()
	got := map[string]int{}
	seen := map[string]bool{}
	for _, row := range siteRows() {
		got[row.site]++
		key := row.site + "|" + row.channel
		assert.False(t, seen[key], "two rows for %s", key)
		seen[key] = true
	}
	assert.Equal(t, siteRowCounts, got)
}

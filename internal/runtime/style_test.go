package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStylesForEligibility asserts the eligibility rule ahead of activation:
// only ""/"table"/"wide" ever produce an Enabled Style, regardless of
// --color, TTY-ness, or the environment — json, yaml, name, and template
// always get the disabled zero value.
func TestStylesForEligibility(t *testing.T) {
	for _, format := range []string{"", "table", "wide"} {
		t.Run("eligible format="+format, func(t *testing.T) {
			out, err := stylesFor(IO{Format: format, Color: "always", OutTTY: false, ErrTTY: false})
			assert.True(t, out.Enabled)
			assert.True(t, err.Enabled)
		})
	}
	for _, format := range []string{"json", "yaml", "name", "template={{.}}"} {
		t.Run("ineligible format="+format, func(t *testing.T) {
			out, err := stylesFor(IO{Format: format, Color: "always", OutTTY: true, ErrTTY: true})
			assert.False(t, out.Enabled)
			assert.False(t, err.Enabled)
		})
	}
}

// TestBoldShownOnceLine asserts boldShownOnceLine's scope: only the line ending
// in ShownOnceSuffix is wrapped; every other line, and all whitespace, is
// untouched.
func TestBoldShownOnceLine(t *testing.T) {
	t.Parallel()
	enabled := style.Style{Enabled: true}

	text := "token/t-abc created\n  bearer token: tok:xxxx" + ShownOnceSuffix + "\n  expires: 2026-08-01T00:00:00Z\n"
	got := boldShownOnceLine(text, enabled)
	assert.Equal(t, text, style.Strip(got), "strip must restore the exact plain text")
	lines := splitKeepNewline(got)
	require.Len(t, lines, 3)
	assert.NotContains(t, lines[0], "\x1b[", "headline must not be styled")
	assert.Contains(t, lines[1], "\x1b[1m", "the shown-once line must be bold")
	assert.Contains(t, lines[1], "\x1b[0m")
	assert.NotContains(t, lines[2], "\x1b[", "a later unrelated line must not be styled")

	t.Run("no match returns text unchanged", func(t *testing.T) {
		t.Parallel()
		plain := "token/t-abc created\n  no shown-once line here\n"
		assert.Equal(t, plain, boldShownOnceLine(plain, enabled))
	})

	t.Run("disabled style returns text unchanged", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, text, boldShownOnceLine(text, style.Style{}))
	})
}

func splitKeepNewline(s string) []string {
	var lines []string
	start := 0
	for i := range s {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// TestRenderErrStyledStripInvariant asserts the content-invariant across the
// error render path: strip(styled) == plain byte-for-byte, and the styled
// form actually differs (proving the wrap ran, not that it was a no-op).
func TestRenderErrStyledStripInvariant(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "bad value").WithHint("try again")

	var plainBuf, styledBuf bytes.Buffer
	_ = renderErr(IO{Err: &plainBuf, Format: "table"}, ce)
	_ = renderErr(IO{Err: &styledBuf, Format: "table", Color: "always", ErrTTY: true}, ce)

	assert.NotEqual(t, plainBuf.String(), styledBuf.String())
	assert.Equal(t, plainBuf.String(), style.Strip(styledBuf.String()))
	assert.Contains(t, styledBuf.String(), "\x1b[31m", "error prefix must be red")
	assert.Contains(t, styledBuf.String(), "\x1b[36m", "hint prefix must be cyan")
}

// TestRenderErrNeverStylesIneligibleFormats asserts the eligibility gate
// holds even when --color/TTY would otherwise turn styling on: json, yaml,
// name, and template render byte-identical with styling forced on.
func TestRenderErrNeverStylesIneligibleFormats(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "bad value").WithHint("try again")

	for _, format := range []string{"json", "yaml", "name", "template={{.}}"} {
		t.Run("format="+format, func(t *testing.T) {
			t.Parallel()
			var plainBuf, forcedBuf bytes.Buffer
			_ = renderErr(IO{Err: &plainBuf, Format: format}, ce)
			_ = renderErr(IO{Err: &forcedBuf, Format: format, Color: "always", ErrTTY: true}, ce)
			assert.Equal(t, plainBuf.String(), forcedBuf.String())
			assert.NotContains(t, forcedBuf.String(), "\x1b[")
		})
	}
}

// TestRenderResultTableHeaderStyledStripInvariant asserts the table-render
// content-invariant through the real renderResult path (not just
// output.RenderStyled directly): the header row is bold, every row is
// plain, and stripping restores the unstyled render exactly.
func TestRenderResultTableHeaderStyledStripInvariant(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "get thing", Shape: ShapeTabular}
	res := Result{Kind: "Thing", Data: fooTabularData{Foo: "bar"}}

	var plainBuf, styledBuf bytes.Buffer
	_, ce := renderResult(IO{Out: &plainBuf, Err: new(bytes.Buffer), Format: "table"}, d, res)
	require.Nil(t, ce)
	_, ce = renderResult(IO{Out: &styledBuf, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)

	assert.NotEqual(t, plainBuf.String(), styledBuf.String())
	assert.Equal(t, plainBuf.String(), style.Strip(styledBuf.String()))
	lines := splitKeepNewline(styledBuf.String())
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "\x1b[1m", "header row must be bold")
	assert.NotContains(t, lines[1], "\x1b[", "data rows must never carry an escape code")
}

// TestRenderResultShownOnceLineBoldStripInvariant asserts the once-only text
// scope: the shown-once line is bold, the headline and warnings mirror are
// untouched, and stripping restores the plain render exactly.
func TestRenderResultShownOnceLineBoldStripInvariant(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "create token", Delivery: DeliveryOnceOnly, Shape: ShapeText}
	shownOnce := "this token value is shown exactly once — store it now"
	res := Result{
		Kind:           "TokenCreated",
		Data:           map[string]string{"name": "t-abc"},
		Text:           MutationText("token", "t-abc", "created", "bearer token: tok:xxxx"+ShownOnceSuffix).Text,
		Warnings:       []string{shownOnce},
		OnceOnlyNotice: shownOnce,
	}

	var plainOut, plainErr, styledOut, styledErr bytes.Buffer
	_, ce := renderResult(IO{Out: &plainOut, Err: &plainErr, Format: "table"}, d, res)
	require.Nil(t, ce)
	_, ce = renderResult(IO{Out: &styledOut, Err: &styledErr, Format: "table", Color: "always", OutTTY: true, ErrTTY: true}, d, res)
	require.Nil(t, ce)

	assert.Equal(t, plainErr.String(), styledErr.String(), "the shown-once warning is silenced on both paths — nothing left to style")
	assert.NotEqual(t, plainOut.String(), styledOut.String())
	assert.Equal(t, plainOut.String(), style.Strip(styledOut.String()))

	lines := splitKeepNewline(styledOut.String())
	require.Len(t, lines, 2)
	assert.NotContains(t, lines[0], "\x1b[", "the headline must not be styled")
	assert.Contains(t, lines[1], "\x1b[1m", "the shown-once line must be bold")
}

// TestRenderResultOrdinaryTextNeverBoldedByShownOnceSuffixCoincidence asserts
// the gate: only a DeliveryOnceOnly command's Text is scanned for
// ShownOnceSuffix, so an ordinary command's line ending in the identical
// bytes is never bolded.
func TestRenderResultOrdinaryTextNeverBoldedByShownOnceSuffixCoincidence(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "do thing", Shape: ShapeText}
	res := Result{
		Kind: "ThingDone",
		Data: map[string]string{"name": "x"},
		Text: MutationText("thing", "x", "done", "value: y"+ShownOnceSuffix).Text,
	}
	var out bytes.Buffer
	_, ce := renderResult(IO{Out: &out, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)
	assert.NotContains(t, out.String(), "\x1b[", "an ordinary command's Text is never scanned for the once-only marker")
}

// TestMirrorWarningsYellowPrefixStripInvariant asserts the warning-mirror
// styling scope: only the "# warning:" prefix carries the yellow wrap, the
// message never does, and stripping restores the plain line exactly.
func TestMirrorWarningsYellowPrefixStripInvariant(t *testing.T) {
	t.Parallel()
	var plainBuf, styledBuf bytes.Buffer
	mirrorWarnings(&plainBuf, []string{"cert expires soon"}, nil, style.Style{})
	mirrorWarnings(&styledBuf, []string{"cert expires soon"}, nil, style.Style{Enabled: true})

	assert.Equal(t, "# warning: cert expires soon\n", plainBuf.String())
	assert.Equal(t, plainBuf.String(), style.Strip(styledBuf.String()))
	assert.Contains(t, styledBuf.String(), "\x1b[33m# warning:\x1b[0m cert expires soon\n")
}

// TestRenderResultStyledTextCalledWithActiveStyle asserts the Result.StyledText
// seam: when set, renderResult's Text branch calls it with the resolved
// output Style and writes its return value instead of Text verbatim; when
// styling is inactive, StyledText still runs but on this builder returns
// Text unchanged, matching the plain render byte-for-byte.
func TestRenderResultStyledTextCalledWithActiveStyle(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "do thing", Shape: ShapeText}
	plain := "thing done\n"
	res := Result{
		Kind: "ThingDone",
		Data: map[string]string{"name": "x"},
		Text: plain,
		StyledText: func(s style.Style) string {
			return s.Bold(strings.TrimSuffix(plain, "\n")) + "\n"
		},
	}

	var plainOut, styledOut bytes.Buffer
	_, ce := renderResult(IO{Out: &plainOut, Err: new(bytes.Buffer), Format: "table"}, d, res)
	require.Nil(t, ce)
	_, ce = renderResult(IO{Out: &styledOut, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)

	assert.Equal(t, plain, plainOut.String(), "an inactive style must render exactly what StyledText returns for the disabled Style")
	assert.NotEqual(t, plain, styledOut.String())
	assert.Equal(t, plain, style.Strip(styledOut.String()))
	assert.Contains(t, styledOut.String(), "\x1b[1m")
}

// TestRenderResultStyledTextKeepsShownOnceBold asserts the composition rule:
// a DeliveryOnceOnly Result that also sets StyledText keeps the shown-once
// line's bold — the two treatments compose instead of the builder silencing
// the credential emphasis; a line the builder already styled is not wrapped
// twice.
func TestRenderResultStyledTextKeepsShownOnceBold(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "make credential", Shape: ShapeText, Delivery: DeliveryOnceOnly}
	plain := "credential/x created\n  value: v" + ShownOnceSuffix + "\n"
	res := Result{
		Kind:             "CredentialCreated",
		Data:             map[string]string{"name": "x"},
		Text:             plain,
		OnceOnlyNotice:   "this value is shown exactly once — store it now",
		CommittedContext: &CommittedContext{ResourceKind: "Credential", ResourceName: "x", Recovery: "get credential x"},
		StyledText: func(s style.Style) string {
			return StyleLines(plain, s, func(i int, body string) (string, bool) {
				if i != 0 {
					return "", false
				}
				return s.Bold(body), true
			})
		},
	}

	var styledOut bytes.Buffer
	_, ce := renderResult(IO{Out: &styledOut, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)
	got := styledOut.String()
	assert.Equal(t, plain, style.Strip(got))
	lines := strings.SplitAfter(got, "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "\x1b[1m", "the builder's own first-line bold survives")
	assert.Contains(t, lines[1], "\x1b[1m", "the shown-once line gains bold through composition")
	assert.Equal(t, 1, strings.Count(lines[1], "\x1b[1m"), "the shown-once line is wrapped once, not twice")
}

// TestCheckResultShapeRejectsStyledTextWithoutText asserts the pairing rule: a
// non-nil StyledText with no Text is a command bug, regardless of shape.
func TestCheckResultShapeRejectsStyledTextWithoutText(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "do thing", Shape: ShapeText}
	res := Result{StyledText: func(s style.Style) string { return "" }}
	ce := checkResultShape(d, res, "table")
	require.NotNil(t, ce)
	assert.Contains(t, ce.Error(), "returned StyledText but no Text")
}

// TestStyleSchemaTOCStripInvariant asserts the bare TOC's bold spans: the
// banner line and every named group label are bold, command rows are not,
// stripping restores the plain render exactly, and the disabled style
// returns the plain render unchanged.
func TestStyleSchemaTOCStripInvariant(t *testing.T) {
	t.Parallel()
	m := tocManifest()
	text := renderTOC(m)

	styled := styleSchemaTOC(text, m, allPrimeGroups(m), style.Style{Enabled: true})
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleSchemaTOC(text, m, allPrimeGroups(m), style.Style{}), "disabled style returns text unchanged")

	plainLines := splitKeepNewline(text)
	styledLines := splitKeepNewline(styled)
	require.Equal(t, len(plainLines), len(styledLines))
	assert.Contains(t, styledLines[0], "\x1b[1m", "the banner line must be bold")

	authIdx := -1
	for i, l := range plainLines {
		if l == "auth:\n" {
			authIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, authIdx, 0, "fixture must contain a named group label")
	assert.Contains(t, styledLines[authIdx], "\x1b[1m", "the auth: group label must be bold")
	assert.NotContains(t, styledLines[authIdx+1], "\x1b[", "a command row must not be styled")
}

// TestStyleSchemaTOCColorsPrimeMarkers asserts the green span wraps exactly
// "[Prime]" on a Prime command's row and on an all-Prime group's own label
// line, while a non-Prime row or a mixed group's label stays entirely plain
// beyond the pre-existing bold.
func TestStyleSchemaTOCColorsPrimeMarkers(t *testing.T) {
	t.Parallel()
	m := Manifest{
		CLIVersion: "v-test",
		Commands: []SchemaCommand{
			{Name: "oidc-client create", Usage: "Create a client", Prime: true},
			{Name: "cluster scale", Usage: "Scale a pool"},
			{Name: "cluster set-autoscaling", Usage: "Turn on autoscaling", Prime: true},
		},
		Groups: []SchemaGroup{
			{Name: "oidc-client", Commands: []string{"oidc-client create"}},
			{Name: "cluster", Commands: []string{"cluster scale", "cluster set-autoscaling"}},
		},
	}
	text := renderTOCText(m, false)
	s := style.Style{Enabled: true}
	styled := styleSchemaTOC(text, m, allPrimeGroups(m), s)
	assert.Equal(t, text, style.Strip(styled))

	greenPrime := s.Green("[Prime]")
	oidcLabel := lineContainingRT(t, styled, "oidc-client:")
	assert.Contains(t, oidcLabel, greenPrime, "an all-Prime group's label carries the green marker")
	assert.Contains(t, oidcLabel, "\x1b[1m", "the label line stays bold")

	clusterLabel := lineContainingRT(t, styled, "cluster:")
	assert.NotContains(t, clusterLabel, "[Prime]", "a mixed group's label carries no marker")

	primeRow := lineContainingRT(t, styled, "cluster set-autoscaling")
	assert.Contains(t, primeRow, greenPrime, "a Prime command row's marker is green")

	scaleRow := lineContainingRT(t, styled, "cluster scale —")
	assert.NotContains(t, scaleRow, "\x1b[", "a non-Prime row stays entirely plain")
}

// lineContainingRT returns the first line of text containing substr, failing
// the test if none matches.
func lineContainingRT(t *testing.T, text, substr string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, text)
	return ""
}

// TestStyleSchemaTOCLabelMarkerFollowsPassedPrimeGroups asserts the styler
// colors a group label's marker from the primeGroups it is handed, never
// from the (possibly filtered) manifest it is styling: a mixed group whose
// only surviving member is Prime renders and stays a plain label, and an
// all-Prime group's label marker is green.
func TestStyleSchemaTOCLabelMarkerFollowsPassedPrimeGroups(t *testing.T) {
	t.Parallel()
	full := Manifest{
		Commands: []SchemaCommand{
			{Name: "p one", Usage: "One", Prime: true},
			{Name: "mixed prime", Usage: "Prime member", Prime: true},
			{Name: "mixed plain", Usage: "Plain member"},
		},
		Groups: []SchemaGroup{
			{Name: "p", Commands: []string{"p one"}},
			{Name: "mixed", Commands: []string{"mixed prime", "mixed plain"}},
		},
	}
	fm := full
	fm.Commands = []SchemaCommand{full.Commands[0], full.Commands[1]}
	fm.Groups = filterGroups(full.Groups, map[string]bool{"p one": true, "mixed prime": true})
	prime := allPrimeGroups(full)
	text := renderFilteredTOC(fm, schemaFilters{search: "x"}, prime)
	s := style.Style{Enabled: true}
	styled := styleSchemaTOC(text, fm, prime, s)
	assert.Contains(t, styled, s.Bold("p: "+s.Green(PrimeMarker)), "all-Prime label marker is green inside the bold label")
	assert.Contains(t, styled, s.Bold("mixed:")+"\n", "mixed group label stays plain and unmarked")
	assert.NotContains(t, styled, "mixed: "+s.Green(PrimeMarker))
	assert.Equal(t, text, style.Strip(styled))
}

// TestStyleSchemaFilteredTOCStripInvariant asserts the filtered TOC's bold
// header line, plus the strip invariant and the inactive-style identity.
func TestStyleSchemaFilteredTOCStripInvariant(t *testing.T) {
	t.Parallel()
	m := tocManifest()
	f := schemaFilters{verb: "get"}
	fm := filterManifest(m, f)
	text := renderFilteredTOC(fm, f, allPrimeGroups(m))

	styled := styleSchemaTOC(text, fm, allPrimeGroups(m), style.Style{Enabled: true})
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleSchemaTOC(text, fm, allPrimeGroups(m), style.Style{}))

	styledLines := splitKeepNewline(styled)
	assert.Contains(t, styledLines[0], "\x1b[1m", "the match-count header line must be bold")
}

// TestStyleSchemaGroupCardStripInvariant asserts the group card's bold spans:
// the "group: <name>" header line and the "commands:" label, nothing else —
// plus the strip invariant and the inactive-style identity.
func TestStyleSchemaGroupCardStripInvariant(t *testing.T) {
	t.Parallel()
	m := tocManifest()
	g, ok := lookupGroup(m.Groups, "auth")
	require.True(t, ok)
	gv := newGroupView(m, g)
	text := renderGroupCard(m, gv)

	styled := styleSchemaGroupCard(text, true, gv.prime, gv.bespoke, style.Style{Enabled: true})
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleSchemaGroupCard(text, true, gv.prime, gv.bespoke, style.Style{}))

	plainLines := splitKeepNewline(text)
	styledLines := splitKeepNewline(styled)
	assert.Contains(t, styledLines[0], "\x1b[1m", "the group: <name> line must be bold")

	cmdIdx := -1
	for i, l := range plainLines {
		if l == "commands:\n" {
			cmdIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, cmdIdx, 0)
	assert.Contains(t, styledLines[cmdIdx], "\x1b[1m", "the commands: label must be bold")
	assert.NotContains(t, styledLines[cmdIdx+1], "\x1b[", "a command row must not be styled")
}

// TestStyleSchemaGroupCardColorsPrimeHeader asserts an all-Prime group's
// "group: <name>" header carries a green "[Prime]" span alongside its
// existing whole-line bold, and that its Prime member's own row is
// colorized too — while a non-Prime group's header and rows carry none.
func TestStyleSchemaGroupCardColorsPrimeHeader(t *testing.T) {
	t.Parallel()
	s := style.Style{Enabled: true}

	primeCmds := []SchemaCommand{{Name: "oidc-client create", Usage: "Create a client", Prime: true}}
	primeText := "group: oidc-client [Prime]\ncommands:\n  " + tocLine(primeCmds[0]) + "\n"
	styled := styleSchemaGroupCard(primeText, true, true, primeCmds, s)
	assert.Equal(t, primeText, style.Strip(styled))
	lines := strings.SplitAfter(styled, "\n")
	assert.Contains(t, lines[0], "\x1b[1m", "the header stays bold")
	assert.Contains(t, lines[0], s.Green("[Prime]"), "the header's marker is green")
	assert.Contains(t, lines[2], s.Green("[Prime]"), "the Prime member's own row is colorized")

	mixedCmds := []SchemaCommand{{Name: "cluster scale", Usage: "Scale a pool"}}
	mixedText := "group: cluster\ncommands:\n  " + tocLine(mixedCmds[0]) + "\n"
	styledMixed := styleSchemaGroupCard(mixedText, true, false, mixedCmds, s)
	mixedLines := strings.SplitAfter(styledMixed, "\n")
	assert.NotContains(t, mixedLines[0], "[Prime]")
	assert.NotContains(t, mixedLines[2], "\x1b[", "a non-Prime row stays entirely plain")
}

// TestStyleSchemaCommandCardStripInvariant asserts the command card's bold
// spans — the title line, each section label token (not its values), and
// every flag name (not its type/description/env) — plus the strip
// invariant and the inactive-style identity.
func TestStyleSchemaCommandCardStripInvariant(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:  "delete project",
		Usage: "Delete a project",
		Desc:  "Removes the project and its namespaces after a server-side dry-run preview.",
		Args:  []ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []ArgSpec{
			{Name: "filename", Aliases: []string{"f"}, Type: "string", Required: true,
				Desc: "manifest file", Env: []string{"RANCHER_FILE"}},
		},
		Examples:    []string{"rancher delete project my-project --apply"},
		CommandType: Mutating,
		Destructive: "true",
		Contract:    "internal",
		Stability:   "stable",
	}
	text := renderCommandCard(cardManifest(), c)

	styled := styleSchemaCommandCard(text, true, true, style.Style{Enabled: true})
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleSchemaCommandCard(text, true, true, style.Style{}))

	plainLines := splitKeepNewline(text)
	styledLines := splitKeepNewline(styled)
	assert.Contains(t, styledLines[0], "\x1b[1m", "the title line must be bold")

	find := func(prefix string) int {
		for i, l := range plainLines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		t.Fatalf("no line with prefix %q", prefix)
		return -1
	}
	assert.True(t, strings.HasPrefix(styledLines[find("flags:")], "\x1b[1mflags:\x1b[0m"))
	assert.True(t, strings.HasPrefix(styledLines[find("contract:")], "\x1b[1mcontract:\x1b[0m "))
	assert.True(t, strings.HasPrefix(styledLines[find("gates:")], "\x1b[1mgates:\x1b[0m "))
	assert.True(t, strings.HasPrefix(styledLines[find("examples:")], "\x1b[1mexamples:\x1b[0m"))
	assert.True(t, strings.HasPrefix(styledLines[find("exit codes:")], "\x1b[1mexit codes:\x1b[0m "))
	assert.True(t, strings.HasPrefix(styledLines[find("global flags:")], "\x1b[1mglobal flags:\x1b[0m "))

	flagIdx := find("  --filename")
	assert.True(t, strings.HasPrefix(styledLines[flagIdx], "  \x1b[1m--filename/-f\x1b[0m "),
		"flag name is bold, type/description/env stay plain")
}

// TestStyleSchemaCommandCardColorsPrimeSentence asserts a Prime command
// card's PrimeSentence line renders entirely green, while every other line
// keeps its existing (non-green) styling.
func TestStyleSchemaCommandCardColorsPrimeSentence(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{Name: "cluster set-autoscaling", Usage: "Turn on autoscaling", Prime: true, CommandType: Local}
	text := renderCommandCard(cardManifest(), c)
	require.Contains(t, text, PrimeSentence)

	s := style.Style{Enabled: true}
	styled := styleSchemaCommandCard(text, false, false, s)
	assert.Equal(t, text, style.Strip(styled))
	assert.Contains(t, styled, s.Green(PrimeSentence), "the PrimeSentence line renders entirely green")
}

// TestStyleSchemaCommandCardFlagShapedDescStaysPlain asserts flag-name
// bolding is anchored to the flags section's own rows: an indented Desc
// line shaped like a flag mention ("  --word ...") stays plain, while the
// real rows between the flags: header and the next blank line take the
// bold.
func TestStyleSchemaCommandCardFlagShapedDescStaysPlain(t *testing.T) {
	t.Parallel()
	card := strings.Join([]string{
		"rancher do thing [name]",
		"",
		"flags:",
		"  --real string  a real flag",
		"",
		"contract: local",
		"gates: none (local command)",
		"",
		"Some behavior.",
		"  --not-a-real-flag is just prose in the description.",
		"More text.",
		"",
		"exit codes: GENERIC=1",
		"global flags: see `rancher schema --all -o json` (globalFlags)",
	}, "\n") + "\n"

	s := style.Style{Enabled: true}
	got := styleSchemaCommandCard(card, true, false, s)
	assert.Equal(t, card, style.Strip(got))
	assert.Contains(t, got, "  \x1b[1m--real\x1b[0m string", "the real flag row is bold")
	assert.Contains(t, got, "  --not-a-real-flag is just prose in the description.\n", "the flag-shaped Desc line stays plain")
	assert.NotContains(t, got, "\x1b[1m--not-a-real-flag")
}

// TestStyleSchemaCommandCardAbsentSectionNoTheft asserts an absent
// conditional section donates no bold: with no examples section rendered, a
// Desc line exactly equal to "examples:" stays plain; same for a group card
// whose command list is empty.
func TestStyleSchemaCommandCardAbsentSectionNoTheft(t *testing.T) {
	t.Parallel()
	card := strings.Join([]string{
		"rancher do thing [name]",
		"",
		"contract: local",
		"gates: none (local command)",
		"",
		"See below.",
		"examples:",
		"More text about the thing.",
		"",
		"exit codes: GENERIC=1",
		"global flags: see `rancher schema --all -o json` (globalFlags)",
	}, "\n") + "\n"

	s := style.Style{Enabled: true}
	got := styleSchemaCommandCard(card, false, false, s)
	assert.Equal(t, card, style.Strip(got))
	assert.NotContains(t, got, "\x1b[1mexamples:", "a Desc line must not take an absent section's bold")
	assert.Contains(t, got, "\x1b[1mcontract:\x1b[0m local")

	group := "group: g\nDesc first.\ncommands:\n"
	gotGroup := styleSchemaGroupCard(group, false, false, nil, s)
	assert.NotContains(t, gotGroup, "\x1b[1mcommands:", "an empty group's Desc line must stay plain")
}

// TestStyleSchemaCommandCardDescCollision asserts the structural anchoring:
// a Desc line that starts with (or equals) a section-label word must stay
// plain while the real label — before Desc for contract/gates, after it for
// examples/exit codes/global flags — gets the bold. The card text mirrors
// renderCommandCard's section order.
func TestStyleSchemaCommandCardDescCollision(t *testing.T) {
	t.Parallel()
	card := strings.Join([]string{
		"rancher do thing [name]",
		"  Do the thing",
		"",
		"flags:",
		"  --force bool  do it harder",
		"",
		"contract: local",
		"gates: none (local command)",
		"",
		"See below.",
		"examples: check docs for more.",
		"gates: are described above.",
		"More text.",
		"",
		"examples:",
		"  rancher do thing x",
		"",
		"exit codes: GENERIC=1",
		"global flags: see `rancher schema --all -o json` (globalFlags)",
	}, "\n") + "\n"

	s := style.Style{Enabled: true}
	got := styleSchemaCommandCard(card, true, true, s)

	assert.Equal(t, card, style.Strip(got))
	assert.Contains(t, got, "\x1b[1mexamples:\x1b[0m\n", "the real examples header is bold")
	assert.Contains(t, got, "examples: check docs for more.\n", "the Desc line stays plain")
	assert.NotContains(t, got, "\x1b[1mexamples:\x1b[0m check docs")
	assert.Contains(t, got, "\x1b[1mgates:\x1b[0m none (local command)", "the real gates line is bold")
	assert.Contains(t, got, "gates: are described above.\n", "the Desc gates line stays plain")
	assert.Equal(t, card, styleSchemaCommandCard(card, true, true, style.Style{}), "disabled style is identity")
}

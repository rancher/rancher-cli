package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planningRefPatterns match the ways a repository file can point at a
// design document or planning artifact instead of stating the constraint
// the code holds: slice and part names, rule and spec ids, review and
// finding labels, task, step and tier numbers, paths under docs/, and a
// short fixed list of pointers to a plan, a spec or a design document.
// Free-English pointers are left to review: a pattern cannot tell them from
// plain prose reliably.
var planningRefPatterns = []*regexp.Regexp{
	// Slice, part and landing names, also inside identifiers.
	regexp.MustCompile(`\bC[0-3][abc]?\b`),
	regexp.MustCompile(`\b(?:Test|test|is|has)(?:C[0-3][abc]?|D[1-9]|R[1-9])[A-Z_0-9]`),
	regexp.MustCompile(`\b(?:Test|test)Slice(?:Cache|C[0-3])`),
	regexp.MustCompile(`(?i)\bslice[- ](?:cache|c[0-3][abc]?)\b`),
	regexp.MustCompile(`\bCACHE\b`),
	regexp.MustCompile(`\b[Ll]anding[- ][0-9]`),
	// "Part A of ..." is blanked first (planningRefNotReferences).
	regexp.MustCompile(`\bPart [A-H][0-9]?\b`),
	regexp.MustCompile(`\bD[1-9]\b`),
	// Rule and spec ids: any LETTERS-DIGITS family (standard names are
	// blanked first, see planningRefNotReferences), dotted R ids, revision
	// numbers, and ADR numbers.
	regexp.MustCompile(`\b[A-Z]{1,5}-[0-9]+[a-z]?\b`),
	regexp.MustCompile(`\bR[1-9](?:\.[0-9]+)?\b`),
	// Known limit: a Kubernetes revision number (a Deployment rolled back
	// to a numbered revision, a ReplicaSet's revision) is flagged too; one
	// in the tree needs an allowlist phrase.
	regexp.MustCompile(`\b[Rr]ev(?:ision)?[- ]?[0-9]`),
	regexp.MustCompile(`\bADR[- ]?[0-9]`),
	// The same ids inside identifiers and test names.
	regexp.MustCompile(`\b(?:Test|is|has)?(?:AC|MC|SE|IC|IM|PS|MF|PC|FA|UU|LS|DR|CS|CW|XP|SC|RF|EXT)[0-9]{1,3}[a-z]?(?:[A-Z_]|\b)`),
	// Review and finding labels, numbered with a digit, a "#" or a word.
	regexp.MustCompile(`\b[Ee]rrat(?:a|um)\b`),
	regexp.MustCompile(`\b[Ff]indings?[- ]?#?[0-9]`),
	regexp.MustCompile(`\b[Rr]ound[- ]?#?[0-9]`),
	// A spelled number followed by "of" is blanked first
	// (planningRefNotReferences). Known limit: other English uses of a
	// spelled number after these words are flagged.
	regexp.MustCompile(`(?i)\b(?:finding|round|decision|section)s? (?:one|two|three|four|five|six|seven|eight|nine|ten)\b`),
	regexp.MustCompile(`\b[Ff]ix[- ]?(?:round\b|#?[0-9])`),
	regexp.MustCompile(`\bfriction[- ]?[0-9]|\brun-[0-9]`),
	regexp.MustCompile(`\bF[0-9]\b`),
	regexp.MustCompile(`(?i)\b(?:review issue|review pass|audit row|audit L)[- ]?#?[0-9]`),
	regexp.MustCompile(`\b(?:Critical|High|Medium|Low)(?:[- ][0-9]|[- ]findings?\b)`),
	regexp.MustCompile(`\bP[123]\b`),
	regexp.MustCompile(`\bW[0-9]\b`),
	regexp.MustCompile(`\b(?:[Rr]e-?review|[Ii]mpl-review|[Ee]xternal review|[Ii]ndependent review|[Pp]lan-review|[Rr]eview[- ]rounds?|[Aa]djudicat\w*|[Rr]eviewers?)\b`),
	// Numbered decision, item, block, section, task, step, tier and phase.
	regexp.MustCompile(`\b(?:[Dd]ecision|[Ii]tem|[Bb]lock|[Ss]ection|[Tt]ask|[Ss]tep|[Tt]ier|[Pp]hase|[Ww]ave)[ -]?#?[0-9]`),
	regexp.MustCompile(`\b[Tt]ranche\b|\bledger(?:ed)?\b|\bthis slice\b|\btracked follow-up\b`),
	regexp.MustCompile(`\bBACKLOG\b|\b(?i:backlog)\.md\b`),
	// Exercise and acceptance passes, and reproduction notes.
	regexp.MustCompile(`\b[Ll]ive[- ]exercise\b|\b[Ee]xercise[- ]?(?:bug|[0-9])`),
	regexp.MustCompile(`\b(?:Linux|Windows) acceptance\b|\bacceptance pass\b`),
	regexp.MustCompile(`\brepro\b`),
	// The word for passing work on (not identifiers such as SAMLHandoff).
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])[Hh]and-?offs?\b`),
	// Paths under docs/ and the fixed pointers to a plan, a spec or a
	// design, at the start of a sentence too.
	regexp.MustCompile(`\bdocs/(?:design|plans|reviews|hand-?offs|backlog)\b`),
	regexp.MustCompile(`(?i)\b(?:per|see) the (?:plan|spec|design)\b`),
	regexp.MustCompile(`(?i)\bthe design\b`),
	regexp.MustCompile(`(?i)\bthe plan (?:says|said|calls for|requires|states)\b`),
	regexp.MustCompile(`(?i)\b(?:in|by|per|from|see|as) the spec\b|\bthe spec (?:allows|says|requires|names|defines|calls|states|lists)\b`),
	regexp.MustCompile(`\b(?i:the) spec'd\b|\b(?i:per) spec\b|\b(?i:the) spec's\b|\bspec (?:wording|[A-Z]{1,4}-)`),
}

// planningRefNotReferences match text that reads like a reference but is
// not one; the matcher blanks each match before it looks for references.
var planningRefNotReferences = []*regexp.Regexp{
	// Standard names: ISO-8601, RFC-6901, HTTP-2, ASN-1, UTF-16, CRC-32,
	// SHA-1, AES-256, RSA-4096, TLS-1, and the two single-letter names
	// X-509 and the NIST curves P-192 to P-521 (any other P-<n> is a label).
	regexp.MustCompile(`\b(?:ISO|RFC|HTTP|ASN|UTF|CRC|SHA|AES|RSA|TLS)-[0-9]+\b|\bX-509\b|\bP-(?:192|224|256|384|521)\b`),
	// "Part A of", "round one of": a part or a round of something else.
	regexp.MustCompile(`\bPart [A-H][0-9]? of\b`),
	regexp.MustCompile(`(?i)\b(?:finding|round|decision|section)s? (?:one|two|three|four|five|six|seven|eight|nine|ten) of\b`),
	// A field or template of a Kubernetes object's spec.
	regexp.MustCompile(`(?i)\bthe spec(?:'s)? (?:fields?|templates?)\b`),
}

// planningRefAllowlist names the exact phrases that look like a planning
// reference but are not, each with the reviewed reason. A phrase, never a
// whole file, is exempt: the rest of its line still has to pass. A phrase
// counts as used only when it suppressed a match in a file other than this
// one, so a stale entry fails the test, and no phrase may contain another.
var planningRefAllowlist = []struct{ phrase, reason string }{
	{"id: repro", "GitHub issue-form field id for the reproduction steps"},
	{"docs, reviews, rounds or findings, handoffs", "AGENTS.md states the rule this test enforces"},
	{`docs/(design|handoffs|reviews|plans|backlog)`, "scripts/validate spells the banned docs subdirectories in its gate regex"},
	{"every C0 byte", "ASCII control characters in install.sh"},
	{"C0 control", "ASCII control characters"},
	{"asserts the boundary: C0", "ASCII control characters"},
	{"Handoff *SAMLHandoff", "field of the SAML sign-in result"},
	{"browser handoff", "SAML sign-in term: the CLI hands the login URL to the browser (SAMLHandoff)"},
	{"SAML handoff", "SAML sign-in term (SAMLHandoff)"},
	{"handoff facts", "the SAML handoff facts recorded for recovery"},
	{"item 0 has no id", "runtime error text about a list item index"},
	{"item 0 is not an object", "runtime error text about a list item index"},
	{"absent from item 0", "assert message about a list item index"},
	{"absent from item 1", "assert message about a list item index"},
	{"DNS-1123", "Kubernetes DNS label standard"},
	{"step 1 is of no known kind", "credential-store transition error text about the first step of a recorded move"},
	{"unusable: step 1 is", "credential-store transition error text about the first step of a recorded move"},
	{`" phase 7\n"`, "completion debug trace format, not a planning phase"},
}

// planningRefCount counts the pattern matches in s.
func planningRefCount(s string) int {
	n := 0
	for _, re := range planningRefPatterns {
		n += len(re.FindAllString(s, -1))
	}
	return n
}

// planningRefMatch reports the first planning reference in s after its
// whitespace runs are collapsed and the standard names and allowlisted
// phrases are removed, or "" when s is clean. When used is not nil, it
// records each phrase whose removal reduced the number of matches.
func planningRefMatch(s string, used map[string]bool) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, re := range planningRefNotReferences {
		s = re.ReplaceAllString(s, " ")
	}
	before := planningRefCount(s)
	if before == 0 {
		return ""
	}
	for _, a := range planningRefAllowlist {
		if used != nil && strings.Contains(s, a.phrase) &&
			planningRefCount(strings.ReplaceAll(s, a.phrase, " ")) < before {
			used[a.phrase] = true
		}
	}
	for _, a := range planningRefAllowlist {
		s = strings.ReplaceAll(s, a.phrase, " ")
	}
	for _, re := range planningRefPatterns {
		if m := re.FindString(s); m != "" {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

// commentKind classifies a line as part of a "//" or "#" comment block, a
// Markdown paragraph (markdown reports true and the line is not blank), or
// neither.
func commentKind(line string, markdown bool) (kind, text string) {
	t := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(t, "//"):
		return "//", strings.TrimSpace(strings.TrimPrefix(t, "//"))
	case strings.HasPrefix(t, "#"):
		return "#", strings.TrimSpace(strings.TrimLeft(t, "#"))
	case markdown && t != "":
		return "para", t
	}
	return "", ""
}

// trailingCommentRE finds a "//" or "#" comment after code on one line.
var trailingCommentRE = regexp.MustCompile(`\S\s+(//|#)(?:\s|$)`)

type planningRefHit struct {
	line int // 1-based; for a comment block, the first line of the block
	ref  string
	text string
}

// planningRefHits scans text and returns every planning reference in it. A
// run of comment lines (or a Markdown paragraph) is joined into one string
// and scanned as a whole, so a reference wrapped across two lines is found
// and an allowlisted phrase wrapped across two lines is recognized; a line
// of code that ends in a comment starts a block with the comment lines
// that follow it. A hit is reported at the first line of its block. Every
// other line is scanned on its own. Known limits: trailing comments on
// successive lines of code are not joined with each other, and a trailing
// comment followed directly by a doc comment joins with it, so a hit in
// that doc comment is reported at the line of code.
func planningRefHits(text string, markdown bool, used map[string]bool) []planningRefHit {
	lines := slices.Collect(strings.SplitSeq(text, "\n"))
	var hits []planningRefHit
	for i := 0; i < len(lines); {
		kind, first := commentKind(lines[i], markdown)
		joined := []string{first}
		if kind == "" {
			joined = []string{lines[i]}
			if m := trailingCommentRE.FindStringSubmatch(lines[i]); m != nil && !markdown {
				kind = m[1]
			}
		}
		j := i + 1
		if kind != "" {
			for ; j < len(lines); j++ {
				k, t := commentKind(lines[j], markdown)
				if k != kind {
					break
				}
				joined = append(joined, t)
			}
		}
		if m := planningRefMatch(strings.Join(joined, " "), used); m != "" {
			hits = append(hits, planningRefHit{i + 1, m, strings.TrimSpace(lines[i])})
		}
		i = j
	}
	return hits
}

// planningRefSelf is this file. Its sample tables are deliberate examples
// of what the scan must flag, so the scan of this file skips the lines
// between the two markers below; every other line is scanned.
const planningRefSelf = "test/conformance/planningrefs_test.go"

// planningRefSamplesBegin and planningRefSamplesEnd bracket the sample
// tables: planningRefFlagged (lines the matcher must flag),
// planningRefClean (lines it must leave alone), planningRefAllowlisted (an
// allowlisted phrase suppresses only its own text) and planningRefBlocks
// (comment blocks whose reference is split across lines, with the line of
// the one hit, or 0 for a known limit that is not reported). The rows that
// spell a docs/ path are built from two literals so scripts/validate's
// docs-path gate does not read this test data as a reference.
// TestPlanningRefSamplesHoldOnlyTheSampleTables asserts that each marker
// appears once, begin before end, with only those four declarations and no
// comment between them.
const (
	planningRefSamplesBegin = "// samples: begin"
	planningRefSamplesEnd   = "// samples: end"
)

// samples: begin

var planningRefFlagged = []string{
	"// see C3 for the rest",
	"// the C2b change decided this",
	"// CACHE reads the record",
	"// Landing 2 adds the cache",
	"// Landing-2 adds the cache",
	"// Part B adds it",
	"// slice-cache state",
	"// the slice CACHE work",
	"// (AC-19: one load per TAB)",
	"assert.True(t, ok, \"MC-6a: repeatable flag\")",
	"// spec D-R1-3 names both commands",
	"// D-R1 and nothing else",
	"// R2-9 covers the store",
	"// R7.3 applies here",
	"// EXT-R-8 provider flow",
	"// per XP-6, decided",
	"// R7.3 and MC-6a and XP-6 and DR-001",
	"// rev 23 of the design",
	"// revision 23",
	"// the rev-5 ruling",
	"// per ADR 7",
	"func TestAC22NoKeyringOnCompletionPath(t *testing.T) {",
	"func isAC10Verb(name string) bool {",
	"func TestC2bStoreChange(t *testing.T) {",
	"func testC2bThing() {",
	"func TestD1Outcome(t *testing.T) {",
	"func TestR7Thing(t *testing.T) {",
	"func TestSliceCacheRecord(t *testing.T) {",
	"// the errata say so",
	"// see errata block 2",
	"// the erratum says otherwise",
	"// finding 3: not-found must return empty",
	"// Finding #3",
	"// finding three",
	"// Finding-2 repro",
	"// round-5 encoding",
	"// Round 4 change",
	"// Round two",
	"// the fix-round change",
	"// Fix 3 covers this",
	"// fix-4 fixture",
	"// the omit-when-unset idiom (fix #5)",
	"// friction-4 semantics",
	"// asserts run-1 finding: when",
	"// must not fall through - F3",
	"// (config-write-lock plan, review issue 2)",
	"// review pass 2",
	"// audit row 6 says so",
	"// audit L404",
	"// High-1 caveat",
	"// This is the High finding",
	"// a Medium finding here",
	"// the review-round shape",
	"// P1 finding",
	"// P2 label",
	"// (P2)",
	"// (P-2)",
	"// the P-1 case",
	"// X-4 applies",
	"// the W4 result shape",
	"// the D1 outcome contract",
	"// the impl-review found this",
	"// the re-review asked for it",
	"// the reviewer's repro",
	"// Decision 5 - a bearer token",
	"// Task 7 adds the seam",
	"// Step 2 leaves the pin",
	"// the Step-0 record",
	"// tier 1 wins",
	"// see Phase 3",
	"// section #4",
	"// shipped in this slice",
	"// a tracked follow-up (auth-token-modernization)",
	"// see BACKLOG.md",
	"// tranche D provisioning",
	"// a ledgered follow-up",
	"// live exercise saw this",
	"// exercise bug 7",
	"// Linux acceptance F6",
	"// the acceptance pass corrects it",
	"// repro: unknown command",
	"// see the handoff for details",
	"// as agreed with the handoff lands",
	"// the browser handoff or the handoff lands",
	"// " + "docs/" + "design/notes.md has the rule",
	"// " + "docs/" + "handoffs/x.md",
	"// see " + "docs/" + "plans/x.md",
	"// per the plan",
	"// Per the plan",
	"// see the Plan",
	"// as the plan says",
	"// the plan calls for this",
	"// see the spec",
	"// See the spec for the rest",
	"// as the spec says",
	"// As the spec says",
	"// the spec allows only this",
	"// in the spec",
	"// the design spells it out",
	"// The design says so",
	"// decided in the design review",
	"// the spec'd shape",
	"// per spec",
	"// spec wording says so",
	"// spec PC-4 applies",
	"// (plan-review round 3, P3)",
	"// selfplant see C3 and finding 3, per the plan",
	"id: repro see Task 3",
	"// every  finding   4 counts",
}

var planningRefClean = []string{
	"// every C0 byte except tab/CR/backspace/formfeed",
	"// every C0 control byte",
	"// a hash of the SHA-256 digest, UTF-8 encoded",
	"// DNS-1123-ish: lowercase alphanumerics",
	"// ISO-8601 time, RFC-6901 pointer, X-509 certificate",
	"// HTTP-2 frame, ASN-1 encoding, a P-256 curve",
	"// UTF-16 text, CRC-32 sum, a SHA-1 digest",
	"// AES-256 cipher, an RSA-4096 key, TLS-1 floor",
	"// the SAML handoff says where to look for it",
	"// the browser handoff is recorded first",
	"res.Handoff = &SAMLHandoff{Provider: p, HandoffAt: t}",
	"func newHandoffPrompter(onHandoff func(id string)) *handoffPrompter {",
	"TLS 1.2 is the minimum: tls.VersionTLS12",
	"// a preview builds the Effect, then executes the plan",
	"// the plan executes after the gates",
	"// the plan carries a warning naming that manager",
	"// by design the store is not probed",
	"// it used to fail, and no longer does",
	"// error-code-3 is not a planning id",
	"x := round(f)",
	"// the round trip to /rancherversion",
	"// a step in the credential-store transition",
	"// tiered pricing is not a planning tier",
	"// the finding of a loose-permission notice",
	"// to fix one entry",
	"// commands are list items one level under",
	"// the internal-phase one-liner",
	"// Kubernetes SelfSubjectReview answers this",
	"// asserts the spec each builder sends",
	"// the provisioning spec's desired version",
	"// set in the spec field",
	"// as the spec field states",
	"// from the spec's template",
	"// a spec'd value",
	"// the deployment's revision history",
	"func latestR1Version() {}",
	"// latestC1Thing",
	"// Part A of the multipart body",
	"// round one of retries",
	"// exercise the branch",
	"func TestCompletionNeverTouchesKeyring(t *testing.T) {",
	"func TestSliceFlagRoundTrip(t *testing.T) {",
	"Name: \"slice-echo\",",
	"// a high finding rate is not a label",
	"// use a slice of pointers",
	"// see the https://example.com page",
}

var planningRefAllowlisted = []struct {
	line    string
	flagged bool
}{
	{"runtime error: item 0 has no id", false},
	{"// item 0 has no id, see Task 3", true},
}

var planningRefBlocks = []struct {
	lines []string
	hit   int
}{
	{[]string{"// asserts finding", "// 4's ordering contract"}, 1},
	{[]string{"// the ONLY derived trouble summarization the", "// spec allows"}, 1},
	{[]string{"// createcluster.go, Task", "// 7, always sets it"}, 1},
	{[]string{"// shipped in this", "// slice"}, 1},
	{[]string{"// mirroring the", "// spec's inference-conflict rule"}, 1},
	{[]string{"# documented in the", "# spec (which layer it oracles)"}, 1},
	{[]string{"// Fix", "// 3 covers this"}, 1},
	{[]string{"// see finding", "//", "// 4 for the reason"}, 1},
	{[]string{"x := 1 // see finding", "// 4 for the reason"}, 1},
	{[]string{"run x # see the", "# design"}, 1},
	{[]string{"\ta int // per the", "\tb int // design"}, 0},
	{[]string{"\ta int // ok", "// Doc: see finding", "// 4 for the reason", "func x() {}"}, 1},
}

// samples: end

// TestPlanningRefMatcher asserts the matcher itself: the lines a scan must
// flag and the lines it must leave alone, so the scan cannot silently
// match nothing.
func TestPlanningRefMatcher(t *testing.T) {
	t.Parallel()
	for _, line := range planningRefFlagged {
		assert.NotEmpty(t, planningRefMatch(line, nil), "must flag: %s", line)
		assert.NotEmpty(t, planningRefHits(line, false, nil), "must report: %s", line)
	}
	for _, line := range planningRefClean {
		assert.Empty(t, planningRefMatch(line, nil), "must not flag: %s", line)
		assert.Empty(t, planningRefHits(line, false, nil), "must not report: %s", line)
	}
	for _, row := range planningRefAllowlisted {
		assert.Equal(t, row.flagged, planningRefMatch(row.line, nil) != "", row.line)
	}
	for _, block := range planningRefBlocks {
		hits := planningRefHits(strings.Join(block.lines, "\n"), false, nil)
		if block.hit == 0 {
			assert.Empty(t, hits, "a known limit, not reported: %q", block.lines)
		} else if assert.Len(t, hits, 1, "must flag the joined block: %q", block.lines) {
			assert.Equal(t, block.hit, hits[0].line, "the block is reported at the line of its first code or comment: %q", block.lines)
		}
		for _, line := range block.lines {
			assert.Empty(t, planningRefHits(line, false, nil), "each line of the block is clean alone: %q", line)
		}
	}
}

// TestPlanningRefAllowlistShape asserts that no allowlist phrase contains
// another, so each phrase's reported use is its own.
func TestPlanningRefAllowlistShape(t *testing.T) {
	t.Parallel()
	for i, a := range planningRefAllowlist {
		assert.NotEmpty(t, a.reason, "allowlist phrase %q needs a reason", a.phrase)
		for j, b := range planningRefAllowlist {
			if i != j {
				assert.NotContains(t, a.phrase, b.phrase, "allowlist phrase %q contains another phrase", a.phrase)
			}
		}
	}
}

// planningRefSamplesSpan returns the 0-based indexes of the begin and end
// marker lines in lines. Each marker must appear exactly once, begin
// before end: a missing, renamed or repeated marker would otherwise widen
// the lines the scan skips.
func planningRefSamplesSpan(lines []string) (begin, end int, err error) {
	begin, end = -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case planningRefSamplesBegin:
			if begin >= 0 {
				return 0, 0, errors.New("the begin marker appears more than once")
			}
			begin = i
		case planningRefSamplesEnd:
			if end >= 0 {
				return 0, 0, errors.New("the end marker appears more than once")
			}
			end = i
		}
	}
	if begin < 0 || end < 0 || end < begin {
		return 0, 0, fmt.Errorf("want one begin marker before one end marker, got begin line %d, end line %d", begin+1, end+1)
	}
	return begin, end, nil
}

// withoutPlanningRefSamples blanks the lines from the begin marker to the
// end marker, keeping every other line and the line numbering.
func withoutPlanningRefSamples(text string) (string, error) {
	lines := slices.Collect(strings.SplitSeq(text, "\n"))
	begin, end, err := planningRefSamplesSpan(lines)
	if err != nil {
		return "", err
	}
	for i := begin; i <= end; i++ {
		lines[i] = ""
	}
	return strings.Join(lines, "\n"), nil
}

// TestPlanningRefSamplesHoldOnlyTheSampleTables asserts that the lines the
// scan skips in this file are exactly the four sample declarations: each
// marker appears once, begin before end, and every other line between them
// is blank or part of one of those declarations, with no comment.
func TestPlanningRefSamplesHoldOnlyTheSampleTables(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(planningRefSelf)
	require.NoError(t, err)
	lines := slices.Collect(strings.SplitSeq(string(data), "\n"))
	begin, end, err := planningRefSamplesSpan(lines)
	require.NoError(t, err)
	// 1-based lines strictly between the markers.
	inside := func(line int) bool { return line > begin+1 && line < end+1 }

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, planningRefSelf, data, parser.ParseComments)
	require.NoError(t, err)
	want := []string{"planningRefFlagged", "planningRefClean", "planningRefAllowlisted", "planningRefBlocks"}
	var got []string
	covered := map[int]bool{}
	for _, decl := range file.Decls {
		from, to := fset.Position(decl.Pos()).Line, fset.Position(decl.End()).Line
		if !inside(from) && !inside(to) {
			continue
		}
		name := ""
		if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.VAR && len(g.Specs) == 1 {
			if vs, ok := g.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) == 1 {
				name = vs.Names[0].Name
			}
		}
		assert.True(t, slices.Contains(want, name) && inside(from) && inside(to),
			"lines %d-%d: only the sample declarations may lie between the markers", from, to)
		got = append(got, name)
		for l := from; l <= to; l++ {
			covered[l] = true
		}
	}
	assert.ElementsMatch(t, want, got, "the sample declarations between the markers")
	for _, group := range file.Comments {
		from, to := fset.Position(group.Pos()).Line, fset.Position(group.End()).Line
		assert.False(t, inside(from) || inside(to),
			"line %d: a comment between the sample markers is never scanned; move it above them", from)
	}
	for i := begin + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			assert.True(t, covered[i+1], "line %d: only the sample declarations may lie between the markers", i+1)
		}
	}
}

// TestNoPlanningReferences fails on any authored text in the repository
// (every file git lists that decodes as UTF-8 without a NUL byte, go.sum
// excepted) that points at a design document or planning artifact: the
// comment, test name, message string or Markdown line must state the
// constraint itself. A reference wrapped across the lines of one comment
// block or Markdown paragraph counts. It names each offending file:line.
func TestNoPlanningReferences(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	used := map[string]bool{}
	var offenders []string
	for _, path := range files {
		if path == "go.sum" {
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			continue // binary, not authored text
		}
		text, record := string(data), used
		if path == planningRefSelf {
			text, err = withoutPlanningRefSamples(text)
			require.NoError(t, err)
			// The allowlist table spells every phrase, so each would
			// count itself as used here.
			record = nil
		}
		for _, h := range planningRefHits(text, strings.HasSuffix(path, ".md"), record) {
			offenders = append(offenders, fmt.Sprintf("%s:%d: %q in %q", path, h.line, h.ref, h.text))
		}
	}
	for _, o := range offenders {
		t.Errorf("state the constraint, not the plan it came from: %s", o)
	}
	for _, a := range planningRefAllowlist {
		assert.True(t, used[a.phrase], "allowlist phrase %q (%s) suppresses no match; remove it", a.phrase, a.reason)
	}
}

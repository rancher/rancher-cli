package conformance

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// scanText is a file's text read as one run of words: every whitespace
// run is one space, and a comment or quote marker (#, // or >) at the start
// of a line is dropped, so a phrase wrapped across lines of a comment, a
// block quote or a paragraph is one phrase. starts holds, for each source
// line that has words, the offset in text where it begins and its line
// number.
type scanText struct {
	text   string
	starts []int
	lines  []int
}

func newScanText(src string) scanText {
	var b strings.Builder
	var s scanText
	for i, line := range strings.Split(src, "\n") {
		words := strings.Fields(trimLineMarkers(line))
		if len(words) == 0 {
			continue
		}
		s.starts = append(s.starts, b.Len())
		s.lines = append(s.lines, i+1)
		b.WriteString(strings.Join(words, " "))
		b.WriteByte(' ')
	}
	s.text = b.String()
	return s
}

// trimLineMarkers drops the comment and quote markers that start a line.
func trimLineMarkers(line string) string {
	for {
		line = strings.TrimLeft(line, " \t")
		switch {
		case strings.HasPrefix(line, "//"):
			line = line[2:]
		case strings.HasPrefix(line, "#"), strings.HasPrefix(line, ">"):
			line = line[1:]
		default:
			return line
		}
	}
}

// lineAt is the source line number that holds text offset off.
func (s scanText) lineAt(off int) int {
	i := sort.Search(len(s.starts), func(i int) bool { return s.starts[i] > off })
	if i == 0 {
		return 0
	}
	return s.lines[i-1]
}

// hits reports "name:line" for the start of every match of re.
func (s scanText) hits(name string, re *regexp.Regexp) []string {
	var out []string
	for _, m := range re.FindAllStringIndex(s.text, -1) {
		out = append(out, fmt.Sprintf("%s:%d", name, s.lineAt(m[0])))
	}
	return out
}

var publicInstallPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)internal\s+repositor`),
	regexp.MustCompile(`(?i)repositor\w*\s+is\s+internal`),
	regexp.MustCompile(`(?i)private/internal`),
	regexp.MustCompile(`(?i)internal\s+repo\b`),
	regexp.MustCompile(`(?i)private\s+repo`),
	regexp.MustCompile(`(?i)internal[- ]phase`),
	regexp.MustCompile(`(?i)\bSSO\b`),
}

// publicInstallHits reports "name:line" for each use of a phrase that says
// the repository is internal or asks for single sign-on authorization.
func publicInstallHits(name, src string) []string {
	s := newScanText(src)
	var out []string
	for _, re := range publicInstallPatterns {
		out = append(out, s.hits(name, re)...)
	}
	return out
}

// publicInstallFiles lists the files TestInstallTextNamesNoInternalRepository
// reads: the two installers, README.md, REFERENCE.md and every Go file under
// install/. It fails when one of the four named files or the Go files are
// missing, so a rename cannot empty the check.
func publicInstallFiles(t *testing.T) []string {
	t.Helper()
	all, err := repositoryFiles()
	require.NoError(t, err)
	named := []string{"install.sh", "install.ps1", "README.md", "REFERENCE.md"}
	var files []string
	goFiles := 0
	for _, p := range all {
		switch {
		case slices.Contains(named, p):
			files = append(files, p)
		case strings.HasPrefix(p, "install/") && path.Ext(p) == ".go":
			files = append(files, p)
			goFiles++
		}
	}
	for _, n := range named {
		require.Contains(t, files, n, "%s is not in the repository file list", n)
	}
	require.NotZero(t, goFiles, "no Go file under install/")
	return files
}

// TestInstallTextNamesNoInternalRepository asserts that no text a reader of
// the installers or the install documentation sees says the repository is
// internal or asks for single sign-on authorization. The rule covers
// install.sh, install.ps1, README.md, REFERENCE.md and every Go file under
// install/. Each file is read whole: comments, code spans, fences and
// message strings all count, and a phrase wrapped across lines of a comment
// or a block quote is found. The patterns are "internal repository",
// "repository is internal", "private/internal", "internal phase" and the
// word SSO; a bare "internal" is fine. A hit is fixed by rewording the text;
// nothing is allowlisted. SSO is refused everywhere in README.md and
// REFERENCE.md because no sign-in text in them uses the word today: a later
// auth provider paragraph that needs it gets a rewording or a decision, not a
// quiet allowlist row.
func TestInstallTextNamesNoInternalRepository(t *testing.T) {
	t.Parallel()
	for _, name := range publicInstallFiles(t) {
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		for _, hit := range publicInstallHits(name, string(src)) {
			assert.Fail(t, "text says the repository is internal or asks for SSO", "%s: reword it", hit)
		}
	}
}

func TestPublicInstallMatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"internal repository", "an internal repository needs a token", []string{"f:1"}},
		{"repository is internal", "While the repository is internal, fetch it", []string{"f:1"}},
		{"private slash internal", "// a private/internal setup", []string{"f:1"}},
		{"internal repo", "an internal repo needs a token", []string{"f:1"}},
		{"private repository", "a private repository", []string{"f:1"}},
		{"private repo", "the private repo", []string{"f:1"}},
		{"internal phase", "the internal-phase one-liner", []string{"f:1"}},
		{"internal phase with space", "the internal phase one-liner", []string{"f:1"}},
		{"sso", "missing scope or SSO authorization", []string{"f:1"}},
		{"wrapped over comment lines", "x\n# While the repository\n# is internal, use\n", []string{"f:2"}},
		{"wrapped over go comment lines", "x\n\t// an internal\n\t// repository needs it\n", []string{"f:2"}},
		{"wrapped over a block quote", "x\n> While the repository is\n> internal, the commands fail\n", []string{"f:2"}},
		{"inside a fence", "```\nan internal repository\n```\n", []string{"f:2"}},
		{"gh auth login alone", "log in with gh auth login", nil},
		{"contract values", "contract (`public` | `internal`)", nil},
		{"token value", "test-internal-token", nil},
		{"single sign-on", "single sign-on", nil},
		{"the word inside another word", "assoc and ssoc", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, publicInstallHits("f", tc.src))
		})
	}
}

// alphaAllowRow lets the lines of file that hold substring use the word
// alpha: lines is the exact number of such lines, so a new use fails.
type alphaAllowRow struct {
	file      string
	substring string
	lines     int
	reason    string
}

var alphaWord = regexp.MustCompile(`(?i)\balpha\b`)

// alphaProblems lists every line that holds the word alpha with no row to
// cover it, every row whose line count differs from the lines found, and
// every row with an empty reason.
func alphaProblems(files map[string]string, rows []alphaAllowRow) []string {
	var problems []string
	found := make([]int, len(rows))
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for i, line := range strings.Split(files[name], "\n") {
			if !alphaWord.MatchString(line) {
				continue
			}
			covered := false
			for r, row := range rows {
				if row.file == name && strings.Contains(line, row.substring) {
					found[r]++
					covered = true
				}
			}
			if !covered {
				problems = append(problems, fmt.Sprintf("%s:%d uses the word alpha with no allowlist row", name, i+1))
			}
		}
	}
	for r, row := range rows {
		if strings.TrimSpace(row.reason) == "" {
			problems = append(problems, fmt.Sprintf("row %s %q has no reason", row.file, row.substring))
		}
		if found[r] != row.lines {
			problems = append(problems, fmt.Sprintf("row %s %q covers %d lines, found %d", row.file, row.substring, row.lines, found[r]))
		}
	}
	return problems
}

var userFacingAlphaRows = []alphaAllowRow{
	{"REFERENCE.md", "`-alpha` builds of Rancher", 1, "Rancher's own version suffix"},
	{"REFERENCE.md", "cli.rancher.io/v0.1.0-alpha.", 5, "example User-Agent values"},
	{"install.sh", "(alpha|beta|rc)", 1, "the release tag grammar the installer accepts"},
	{"install.sh", "{alpha|beta|rc}.N", 1, "the release tag grammar in the tag error message"},
	{"install.sh", "order alpha < beta", 1, "the prerelease class order of the tag comparison"},
	{"install.sh", "alpha.*)", 1, "the prerelease class of the tag comparison"},
	{"install.ps1", "(alpha|beta|rc)", 1, "the release tag grammar the installer accepts"},
	{"install.ps1", "{alpha|beta|rc}.N", 1, "the release tag grammar in the tag error message"},
	{"install.ps1", "order alpha < beta", 1, "the prerelease class order of the tag comparison"},
	{"install.ps1", "alpha = 0", 1, "the prerelease class of the tag comparison"},
}

var userFacingAlphaFiles = []string{"README.md", "REFERENCE.md", "MIGRATING.md", "skills/rancher/SKILL.md", "install.sh", "install.ps1"}

// TestUserFacingDocsStateNoAlphaStatus asserts that no user-facing document
// or installer states the product status as alpha, and that README.md
// carries the experimental status note. The files are README.md,
// REFERENCE.md, MIGRATING.md, skills/rancher/SKILL.md, install.sh and
// install.ps1, each read whole, case-insensitive and whole-word. A use of the
// word is allowed only by a row of userFacingAlphaRows (file, a distinctive
// substring of the line, the exact number of lines that hold it, and a
// reason): a Rancher version suffix, an example value, or the grammar of the
// release tags the installers accept. A new use of the word needs a row with
// its reason, or a rewording. The status note changes with the first stable
// release; this assert and the rows then change together.
func TestUserFacingDocsStateNoAlphaStatus(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for _, name := range userFacingAlphaFiles {
		src, err := os.ReadFile(name)
		require.NoError(t, err, "%s must exist", name)
		files[name] = string(src)
	}
	for _, p := range alphaProblems(files, userFacingAlphaRows) {
		assert.Fail(t, "alpha status", p)
	}
	readme := newScanText(files["README.md"]).text
	for _, want := range []string{"**Experimental.**", "under heavy development", "considered experimental"} {
		assert.Contains(t, readme, want, "README.md must carry the experimental status note")
	}
}

func TestAlphaMatcher(t *testing.T) {
	t.Parallel()
	rows := []alphaAllowRow{{"a.md", "-alpha builds", 1, "a version suffix"}}
	t.Run("status sentence is a hit", func(t *testing.T) {
		got := alphaProblems(map[string]string{"a.md": "rancher-cli is in alpha"}, nil)
		assert.Equal(t, []string{"a.md:1 uses the word alpha with no allowlist row"}, got)
	})
	t.Run("a longer word is not a hit", func(t *testing.T) {
		assert.Empty(t, alphaProblems(map[string]string{"a.md": "the alphabet"}, nil))
	})
	t.Run("an allowlisted line passes", func(t *testing.T) {
		assert.Empty(t, alphaProblems(map[string]string{"a.md": "x\n-alpha builds of Rancher"}, rows))
	})
	t.Run("a second use in the file fails", func(t *testing.T) {
		got := alphaProblems(map[string]string{"a.md": "-alpha builds of Rancher\nstill in alpha"}, rows)
		assert.Equal(t, []string{"a.md:2 uses the word alpha with no allowlist row"}, got)
	})
	t.Run("a second line holding the substring fails the count", func(t *testing.T) {
		got := alphaProblems(map[string]string{"a.md": "-alpha builds\n-alpha builds"}, rows)
		assert.Len(t, got, 1)
		assert.Contains(t, got[0], "covers 1 lines, found 2")
	})
	t.Run("a stale row fails", func(t *testing.T) {
		got := alphaProblems(map[string]string{"a.md": "nothing here"}, rows)
		assert.Len(t, got, 1)
		assert.Contains(t, got[0], "covers 1 lines, found 0")
	})
	t.Run("an empty reason fails", func(t *testing.T) {
		bare := []alphaAllowRow{{"a.md", "-alpha builds", 1, " "}}
		got := alphaProblems(map[string]string{"a.md": "-alpha builds"}, bare)
		assert.Equal(t, []string{`row a.md "-alpha builds" has no reason`}, got)
	})
	t.Run("a row of another file does not cover", func(t *testing.T) {
		got := alphaProblems(map[string]string{"b.md": "-alpha builds"}, rows)
		assert.Len(t, got, 2)
	})
}

var allowedFormTypes = []string{"markdown", "input", "textarea", "dropdown", "checkboxes"}

var formIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type issueFormItem struct {
	Type       string `yaml:"type"`
	ID         string `yaml:"id"`
	Attributes struct {
		Label       string `yaml:"label"`
		Description string `yaml:"description"`
		Value       string `yaml:"value"`
		Render      string `yaml:"render"`
		Placeholder string `yaml:"placeholder"`
		Options     []any  `yaml:"options"`
	} `yaml:"attributes"`
	Validations *struct {
		Required *bool `yaml:"required"`
	} `yaml:"validations"`
}

type issueForm struct {
	Name        string          `yaml:"name"`
	Description string          `yaml:"description"`
	Labels      []string        `yaml:"labels"`
	Body        []issueFormItem `yaml:"body"`
}

// optionProblem reports what is wrong with one entry of a dropdown or
// checkboxes list, or "" when it is valid. A dropdown entry is a non-empty
// string; a checkboxes entry is a mapping with a label and an optional
// required flag.
func optionProblem(kind string, option any) string {
	switch kind {
	case "dropdown":
		if s, ok := option.(string); !ok || strings.TrimSpace(s) == "" {
			return "an option is not a non-empty string"
		}
	case "checkboxes":
		m, ok := option.(map[string]any)
		if !ok {
			return "an option is not a mapping"
		}
		if label, _ := m["label"].(string); strings.TrimSpace(label) == "" {
			return "an option has no label"
		}
		for key := range m {
			if key != "label" && key != "required" {
				return fmt.Sprintf("an option has the unknown key %q", key)
			}
		}
	}
	return ""
}

// issueFormProblems checks the structure GitHub needs of an issue form and
// that each wanted field exists. The decode refuses a key the form types do
// not name, so a misspelled key fails. Each entry of required and optional is
// "id: <name>"; a required field has validations.required true and an
// optional one has no validations key.
func issueFormProblems(name string, src []byte, required, optional []string) []string {
	var form issueForm
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&form); err != nil {
		return []string{name + ": " + err.Error()}
	}
	var problems []string
	if form.Name == "" {
		problems = append(problems, name+": no name")
	}
	if form.Description == "" {
		problems = append(problems, name+": no description")
	}
	if len(form.Labels) == 0 {
		problems = append(problems, name+": no labels")
	}
	if len(form.Body) == 0 {
		problems = append(problems, name+": no body")
	}
	byID := map[string]issueFormItem{}
	labels := map[string]bool{}
	for i, item := range form.Body {
		if !slices.Contains(allowedFormTypes, item.Type) {
			problems = append(problems, fmt.Sprintf("%s: body entry %d has type %q", name, i+1, item.Type))
		}
		if item.Type == "markdown" {
			if strings.TrimSpace(item.Attributes.Value) == "" {
				problems = append(problems, fmt.Sprintf("%s: body entry %d is markdown with no value", name, i+1))
			}
			continue
		}
		if item.Attributes.Label == "" {
			problems = append(problems, fmt.Sprintf("%s: body entry %d has no label", name, i+1))
		} else if labels[item.Attributes.Label] {
			problems = append(problems, fmt.Sprintf("%s: label %q is used twice", name, item.Attributes.Label))
		}
		labels[item.Attributes.Label] = true
		if item.Type == "dropdown" || item.Type == "checkboxes" {
			if len(item.Attributes.Options) == 0 {
				problems = append(problems, fmt.Sprintf("%s: body entry %d has no options", name, i+1))
			}
			for _, option := range item.Attributes.Options {
				if p := optionProblem(item.Type, option); p != "" {
					problems = append(problems, fmt.Sprintf("%s: body entry %d: %s", name, i+1, p))
				}
			}
		}
		if item.ID == "" {
			problems = append(problems, fmt.Sprintf("%s: body entry %d has no id", name, i+1))
			continue
		}
		if !formIDPattern.MatchString(item.ID) {
			problems = append(problems, fmt.Sprintf("%s: id %q has a character other than a letter, digit, - or _", name, item.ID))
		}
		if _, dup := byID[item.ID]; dup {
			problems = append(problems, fmt.Sprintf("%s: id %q is used twice", name, item.ID))
		}
		byID[item.ID] = item
	}
	for _, want := range required {
		item, ok := byID[strings.TrimPrefix(want, "id: ")]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: no field with %s", name, want))
		case item.Validations == nil || item.Validations.Required == nil || !*item.Validations.Required:
			problems = append(problems, fmt.Sprintf("%s: %s is not required", name, want))
		}
	}
	for _, want := range optional {
		item, ok := byID[strings.TrimPrefix(want, "id: ")]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: no field with %s", name, want))
		case item.Validations != nil:
			problems = append(problems, fmt.Sprintf("%s: %s must be optional", name, want))
		}
	}
	return problems
}

type issueChooserConfig struct {
	BlankIssuesEnabled *bool `yaml:"blank_issues_enabled"`
	ContactLinks       []struct {
		Name  string `yaml:"name"`
		URL   string `yaml:"url"`
		About string `yaml:"about"`
	} `yaml:"contact_links"`
}

func issueChooserProblems(src []byte) []string {
	var cfg issueChooserConfig
	if err := yaml.Unmarshal(src, &cfg); err != nil {
		return []string{"config.yml: " + err.Error()}
	}
	var problems []string
	if cfg.BlankIssuesEnabled == nil || *cfg.BlankIssuesEnabled {
		problems = append(problems, "config.yml: blank_issues_enabled must be false")
	}
	security, forums := false, false
	for _, link := range cfg.ContactLinks {
		if link.Name == "" || link.About == "" {
			problems = append(problems, "config.yml: a contact link has no name or about")
		}
		security = security || strings.Contains(link.URL, "SECURITY.md")
		forums = forums || strings.Contains(link.URL, "forums.rancher.com")
	}
	if !security {
		problems = append(problems, "config.yml: no contact link to SECURITY.md")
	}
	if !forums {
		problems = append(problems, "config.yml: no contact link to the forums")
	}
	return problems
}

var bugFormRequired = []string{"id: version", "id: rancher-version", "id: platform", "id: command", "id: expected", "id: repro"}
var bugFormOptional = []string{"id: output"}
var featureFormRequired = []string{"id: workflow", "id: version"}

// TestCommunityFilesExist asserts that the community files exist and say
// what the project decided: CONTRIBUTING.md and the pull request template
// state that the project is not accepting code contributions, CONTRIBUTING.md
// names SECURITY.md, rancher --version and -o json, and the issue forms and
// the chooser configuration are valid. GitHub shows a form with an error only
// once the repository is public, so the structure is checked here.
func TestCommunityFilesExist(t *testing.T) {
	t.Parallel()
	read := func(name string) []byte {
		src, err := os.ReadFile(name)
		require.NoError(t, err, "%s must exist", name)
		return src
	}
	contributing := newScanText(string(read("CONTRIBUTING.md"))).text
	read("SECURITY.md")
	template := newScanText(string(read(".github/PULL_REQUEST_TEMPLATE.md"))).text
	assert.Contains(t, contributing, "not accepting code contributions")
	assert.Contains(t, template, "not accepting code contributions")
	for _, want := range []string{"SECURITY.md", "rancher --version", "-o json"} {
		assert.Contains(t, contributing, want, "CONTRIBUTING.md must name %s", want)
	}
	for _, p := range issueChooserProblems(read(".github/ISSUE_TEMPLATE/config.yml")) {
		assert.Fail(t, "issue chooser", p)
	}
	for _, p := range issueFormProblems("bug.yml", read(".github/ISSUE_TEMPLATE/bug.yml"), bugFormRequired, bugFormOptional) {
		assert.Fail(t, "bug form", p)
	}
	for _, p := range issueFormProblems("feature.yml", read(".github/ISSUE_TEMPLATE/feature.yml"), featureFormRequired, nil) {
		assert.Fail(t, "feature form", p)
	}
}

const sampleForm = `name: Sample
description: A sample
labels: ["bug"]
body:
  - type: markdown
    attributes:
      value: text
  - type: input
    id: version
    attributes:
      label: Version
    validations:
      required: true
  - type: textarea
    id: output
    attributes:
      label: Output
  - type: dropdown
    id: runby
    attributes:
      label: Run by
      options:
        - A person
        - A script
  - type: checkboxes
    id: redacted
    attributes:
      label: Redaction
      options:
        - label: I removed secrets
          required: true
`

func TestIssueFormMatcher(t *testing.T) {
	t.Parallel()
	required, optional := []string{"id: version"}, []string{"id: output"}
	assert.Empty(t, issueFormProblems("f", []byte(sampleForm), required, optional))

	tests := []struct {
		name string
		src  string
		want string
	}{
		{"same id twice", strings.Replace(sampleForm, "id: output", "id: version", 1), `id "version" is used twice`},
		{"missing label", strings.Replace(sampleForm, "label: Output", "description: Output", 1), "has no label"},
		{"unknown type", strings.Replace(sampleForm, "type: textarea", "type: area", 1), `has type "area"`},
		{"required field not required", strings.Replace(sampleForm, "required: true", "required: false", 1), "id: version is not required"},
		{"optional field with validations", strings.Replace(sampleForm, "      label: Output\n", "      label: Output\n    validations:\n      required: false\n", 1), "id: output must be optional"},
		{"missing field", strings.Replace(sampleForm, "id: output", "id: other", 1), "no field with id: output"},
		{"no name", strings.Replace(sampleForm, "name: Sample\n", "", 1), "no name"},
		{"no labels", strings.Replace(sampleForm, "labels: [\"bug\"]\n", "", 1), "no labels"},
		{"misspelled key", strings.Replace(sampleForm, "validations:", "validation:", 1), "field validation not found"},
		{"misspelled attribute key", strings.Replace(sampleForm, "label: Version", "lable: Version", 1), "field lable not found"},
		{"id with a space", strings.Replace(sampleForm, "id: output", "id: out put", 1), "has a character other than"},
		{"id with a dot", strings.Replace(sampleForm, "id: output", "id: out.put", 1), "has a character other than"},
		{"label used twice", strings.Replace(sampleForm, "label: Output", "label: Version", 1), `label "Version" is used twice`},
		{"dropdown without options", strings.Replace(sampleForm, "      options:\n        - A person\n        - A script\n", "      options: []\n", 1), "has no options"},
		{"dropdown with an empty option", strings.Replace(sampleForm, "- A script", "- \"\"", 1), "an option is not a non-empty string"},
		{"checkbox without a label", strings.Replace(sampleForm, "- label: I removed secrets\n          required: true\n", "- required: true\n", 1), "an option has no label"},
		{"checkbox with an unknown key", strings.Replace(sampleForm, "          required: true\n", "          require: true\n", 1), "an option has the unknown key"},
		{"markdown without a value", strings.Replace(sampleForm, "      value: text\n", "      label: text\n", 1), "is markdown with no value"},
		{"not yaml", "name: [", "yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := issueFormProblems("f", []byte(tc.src), required, optional)
			require.NotEmpty(t, got)
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}

	chooser := "blank_issues_enabled: false\ncontact_links:\n  - name: Security\n    url: https://x/SECURITY.md\n    about: a\n  - name: Q\n    url: https://forums.rancher.com/\n    about: b\n"
	assert.Empty(t, issueChooserProblems([]byte(chooser)))
	assert.NotEmpty(t, issueChooserProblems([]byte("blank_issues_enabled: false\n")))
	assert.NotEmpty(t, issueChooserProblems([]byte(strings.Replace(chooser, "false", "true", 1))))
}

var welcomePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)contributions?\s+(?:are\s+)?welcome`),
	regexp.MustCompile(`(?i)(?:pull\s+requests?|\bPRs?)\s+(?:are\s+)?(?:welcome|appreciated|encouraged)`),
	regexp.MustCompile(`(?i)welcome\s+(?:to\s+)?(?:contribut|submit|send|open\s+a\s+pull)`),
	regexp.MustCompile(`(?i)feel\s+free\s+to\s+(?:contribute|submit|send|open\s+a\s+pull)`),
	regexp.MustCompile(`(?i)we\s+(?:welcome|accept|encourage)\s+(?:code|pull|contributions?|patches)`),
	regexp.MustCompile(`(?i)(?:send|submit|open)\s+(?:us\s+)?a\s+pull\s+request`),
}

// welcomeHits reports "name:line" for each use of wording that invites code
// contributions or pull requests.
func welcomeHits(name, src string) []string {
	s := newScanText(src)
	var out []string
	for _, re := range welcomePatterns {
		out = append(out, s.hits(name, re)...)
	}
	return out
}

// TestNoDocumentWelcomesCodeContributions asserts that no tracked Markdown
// file, the pull request template or an issue form invites code
// contributions or pull requests: the project takes no code contributions.
// Every tracked *.md file is read except those under docs/ or a testdata
// directory, AGENTS.md and CLAUDE.md, whole and case-insensitive. AGENTS.md
// and CLAUDE.md are guidance for the team's own agents, not text for outside
// readers, and a line there such as "open a pull request" would be the team's
// own instruction. A hit is fixed by rewording the text; nothing is
// allowlisted.
func TestNoDocumentWelcomesCodeContributions(t *testing.T) {
	t.Parallel()
	all, err := repositoryFiles()
	require.NoError(t, err)
	var files []string
	for _, p := range all {
		switch {
		case p == "AGENTS.md" || p == "CLAUDE.md" || strings.HasPrefix(p, "docs/") || inTestdata(p):
		case path.Ext(p) == ".md":
			files = append(files, p)
		case strings.HasPrefix(p, ".github/ISSUE_TEMPLATE/") && path.Ext(p) == ".yml":
			files = append(files, p)
		}
	}
	require.Contains(t, files, "README.md")
	for _, name := range files {
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		for _, hit := range welcomeHits(name, string(src)) {
			assert.Fail(t, "welcomes code contributions", "%s: reword it", hit)
		}
	}
}

func TestWelcomeMatcher(t *testing.T) {
	t.Parallel()
	hit := []string{
		"Contributions are welcome.",
		"contribution welcome here",
		"Pull requests are welcome",
		"PRs welcome",
		"Pull requests appreciated",
		"Welcome to contribute",
		"welcome to submit patches",
		"Feel free to open a pull request",
		"We welcome contributions",
		"We accept patches",
		"Please send us a pull request",
		"open a pull request",
		"Pull requests\n> are welcome",
	}
	for _, src := range hit {
		assert.NotEmpty(t, welcomeHits("f", src), src)
	}
	for _, src := range []string{
		"We are not accepting code contributions at the moment.",
		"Pull requests are closed without review.",
		"Show it with -o json and open an issue",
		"the PRTB projectName field",
	} {
		assert.Empty(t, welcomeHits("f", src), src)
	}
}

package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const admissionCheckScript = "scripts/admission-text-check"

// admissionCheckVersions are the module versions each edition of the rows
// below is built at.
var admissionCheckVersions = map[string]map[string]string{
	"community":  {"k8s.io/apiserver": "v0.36.5", "k8s.io/kubernetes": "v1.36.5", "k8s.io/pod-security-admission": "v0.36.5"},
	"prime":      {"k8s.io/apiserver": "v0.35.6", "k8s.io/kubernetes": "v1.35.6", "k8s.io/pod-security-admission": "v0.35.6"},
	"repository": {"k8s.io/apiserver": "v0.37.1", "k8s.io/kubernetes": "v1.37.1", "k8s.io/pod-security-admission": "v0.37.1"},
}

// admissionCheckSetup is a tree of fake Kubernetes modules, one for each
// edition and module, with a Go file under the directory of each variant of
// the words file that has the variant's words in one string literal.
type admissionCheckSetup struct {
	t        *testing.T
	bin      string
	root     string
	words    admissionWords
	editions []string
}

type admissionCheckRun struct {
	stdout, stderr string
	exit           int
}

func newAdmissionCheckSetup(t *testing.T, bin string, editions ...string) *admissionCheckSetup {
	t.Helper()
	testfs.SkipOnWindows(t, posixOnlyReason)
	s := &admissionCheckSetup{t: t, bin: bin, root: t.TempDir(), words: readAdmissionWords(t), editions: editions}
	for _, edition := range editions {
		for _, text := range s.words.Texts {
			for i, v := range text.Variants {
				content := "package p\n\nvar s = " + strconv.Quote(strings.Join(v.Words, "%s")) + "\n"
				s.write(edition, s.file(edition, text, i), content)
			}
		}
	}
	return s
}

func (s *admissionCheckSetup) moduleDir(edition, module string) string {
	return filepath.Join(s.root, edition, filepath.FromSlash(module))
}

func (s *admissionCheckSetup) text(id string) admissionWordsText {
	s.t.Helper()
	for _, text := range s.words.Texts {
		if text.ID == id {
			return text
		}
	}
	require.FailNow(s.t, "the words file has no row for "+id)
	return admissionWordsText{}
}

// file is the path of the Go file written for variant i of the text.
func (s *admissionCheckSetup) file(edition string, text admissionWordsText, i int) string {
	dir := text.Variants[i].Dir
	if dir == "" {
		dir = text.Dir
	}
	return filepath.Join(s.moduleDir(edition, text.Module), filepath.FromSlash(dir), text.ID+"-"+strconv.Itoa(i)+".go")
}

func (s *admissionCheckSetup) write(edition, path, content string) {
	s.t.Helper()
	require.NoError(s.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(s.t, os.WriteFile(path, []byte(content), 0o644))
}

// replaceVariant rewrites the Go file of variant i of the text in the edition.
func (s *admissionCheckSetup) replaceVariant(edition, id string, i int, content string) {
	s.t.Helper()
	text := s.text(id)
	s.write(edition, s.file(edition, text, i), content)
}

func (s *admissionCheckSetup) removeVariant(edition, id string, i int) {
	s.t.Helper()
	require.NoError(s.t, os.Remove(s.file(edition, s.text(id), i)))
}

// modulesFile writes the modules hook file: the directory of each module of
// each edition and, when withVersions is true, its version.
func (s *admissionCheckSetup) modulesFile(withVersions bool, omit ...string) string {
	s.t.Helper()
	hook := map[string]map[string]map[string]string{}
	for _, edition := range s.editions {
		hook[edition] = map[string]map[string]string{}
		for _, module := range s.words.Modules {
			if omit != nil && omit[0] == edition && omit[1] == module {
				continue
			}
			entry := map[string]string{"dir": s.moduleDir(edition, module)}
			if withVersions {
				entry["version"] = admissionCheckVersions[edition][module]
			}
			hook[edition][module] = entry
		}
	}
	return s.writeJSON("modules.json", hook)
}

func (s *admissionCheckSetup) writeJSON(name string, v any) string {
	s.t.Helper()
	data, err := json.Marshal(v)
	require.NoError(s.t, err)
	path := filepath.Join(s.t.TempDir(), name)
	require.NoError(s.t, os.WriteFile(path, data, 0o644))
	return path
}

// goModDir writes one <edition>.mod file for each entry of files.
func (s *admissionCheckSetup) goModDir(files map[string]string) string {
	s.t.Helper()
	dir := s.t.TempDir()
	for edition, content := range files {
		require.NoError(s.t, os.WriteFile(filepath.Join(dir, edition+".mod"), []byte(content), 0o644))
	}
	return dir
}

// run runs the script with the hooks and no network: no module proxy, an
// empty module cache and no toolchain download.
func (s *admissionCheckSetup) run(env ...string) admissionCheckRun {
	s.t.Helper()
	cmd := exec.CommandContext(s.t.Context(), "bash", admissionCheckScript)
	cmd.Env = append(testfs.EnvWithoutGit(),
		"GOPROXY=off",
		"GOMODCACHE="+s.t.TempDir(),
		"GOTOOLCHAIN=local",
		"ADMISSION_TEXT_CHECK_GOMOD_DIR=",
		"ADMISSION_TEXT_CHECK_MODULES_FILE=",
		"ADMISSION_TEXT_CHECK_WORDS_FILE=",
		"ADMISSION_TEXT_CHECK_BINARY="+s.bin,
	)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := admissionCheckRun{}
	if err := cmd.Run(); err != nil {
		exit, ok := errors.AsType[*exec.ExitError](err)
		require.True(s.t, ok, "%v", err)
		res.exit = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// runWithVersions runs the script with the modules hook file that carries each
// module's version and no go.mod hook.
func (s *admissionCheckSetup) runWithVersions(env ...string) admissionCheckRun {
	s.t.Helper()
	return s.run(append([]string{"ADMISSION_TEXT_CHECK_MODULES_FILE=" + s.modulesFile(true)}, env...)...)
}

// checkedLine is the last line of a run that passes: the texts of the words
// file, the ones with words, and the number of editions.
func (s *admissionCheckSetup) checkedLine(editions int) string {
	withWords := 0
	for _, text := range s.words.Texts {
		if len(text.Variants) > 0 {
			withWords++
		}
	}
	return fmt.Sprintf("checked %d texts (%d with words) in %d editions\n", len(s.words.Texts), withWords, editions)
}

func linesWithPrefix(output, prefix string) []string {
	var lines []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// admissionGoMod is a go.mod file with the given directives.
func admissionGoMod(directives ...string) string {
	return "module example.com/root\n\ngo 1.27\n\n" + strings.Join(directives, "\n") + "\n"
}

// admissionReplaceAll is the go.mod text of an edition whose three modules are
// replaced by their own path at the versions of the edition. A module in
// override is replaced by the given target instead.
func admissionReplaceAll(edition string, override map[string]string) string {
	var directives []string
	for _, module := range []string{"k8s.io/apiserver", "k8s.io/kubernetes", "k8s.io/pod-security-admission"} {
		target := module + " " + admissionCheckVersions[edition][module]
		if o, ok := override[module]; ok {
			target = o
		}
		directives = append(directives, "replace "+module+" => "+target)
	}
	return admissionGoMod(directives...)
}

// buildAdmissionCheck has the script compile its check program once, to a path
// in a temporary directory of t, and returns that path. Every run of the script
// in the rows then uses the compiled program and does not build it again.
func buildAdmissionCheck(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "check")
	cmd := exec.CommandContext(t.Context(), "bash", admissionCheckScript)
	cmd.Env = append(testfs.EnvWithoutGit(),
		"GOPROXY=off",
		"GOMODCACHE="+t.TempDir(),
		"GOTOOLCHAIN=local",
		"ADMISSION_TEXT_CHECK_GOMOD_DIR=",
		"ADMISSION_TEXT_CHECK_MODULES_FILE=",
		"ADMISSION_TEXT_CHECK_WORDS_FILE="+filepath.Join(t.TempDir(), "missing.json"),
		"ADMISSION_TEXT_CHECK_BINARY="+bin,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit, ok := errors.AsType[*exec.ExitError](err)
	require.True(t, ok, "the run with a missing words file must exit 1: %v", err)
	require.Equal(t, 1, exit.ExitCode(), "%s", stderr.String())
	require.Contains(t, stderr.String(), "reading the words file")
	_, err = os.Stat(bin)
	require.NoError(t, err, "the script must build the program to the path it was given")
	return bin
}

// TestAdmissionTextCheckScript compiles the check program of
// scripts/admission-text-check once and runs admissionCheckSearch,
// admissionCheckInputs and admissionCheckVersionRules with it.
func TestAdmissionTextCheckScript(t *testing.T) {
	testfs.SkipOnWindows(t, posixOnlyReason)
	t.Parallel()
	bin := buildAdmissionCheck(t)
	t.Run("search", func(t *testing.T) {
		t.Parallel()
		admissionCheckSearch(t, bin)
	})
	t.Run("inputs", func(t *testing.T) {
		t.Parallel()
		admissionCheckInputs(t, bin)
	})
	t.Run("versions", func(t *testing.T) {
		t.Parallel()
		admissionCheckVersionRules(t, bin)
	})
}

// admissionCheckSearch asserts what scripts/admission-text-check
// counts as the words of an admission text being written by a module: all the
// words of one variant in one string literal (a concatenation of literals
// included) of a non-test Go file under the variant's directory, in each
// edition. A change in one word of one edition, words only in a comment or a
// test file, words spread over two literals, and words under another
// directory each fail the run and name the text, the edition, the module with
// its version and the words; a text with two variants passes when one of them
// is found. The rows run the script with directories of fake modules in place
// of the downloads, so no row reaches a network.
func admissionCheckSearch(t *testing.T, bin string) {
	t.Helper()

	t.Run("every word present", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		res := s.runWithVersions()
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Len(t, linesWithPrefix(res.stdout, "ok "), 6, "one line for each edition and module")
		assert.Contains(t, res.stdout, "ok community k8s.io/apiserver@v0.36.5\n")
		assert.Contains(t, res.stdout, "ok prime k8s.io/pod-security-admission@v0.35.6\n")
		assert.Contains(t, res.stdout, s.checkedLine(2))
		assert.Len(t, linesWithPrefix(res.stdout, "resolved "), 6)
		assert.Less(t, strings.Index(res.stdout, "resolved prime"), strings.Index(res.stdout, "ok community"), "the versions are printed before the check")
	})

	t.Run("a word changed in one literal of the first edition", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		s.replaceVariant("community", "quota-insufficient", 0, "package p\n\nvar s = \"insufficient quota for: %s\"\n")
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, `error: admission text quota-insufficient (community k8s.io/apiserver@v0.36.5): no string literal under pkg/admission/plugin/resourcequota has the words "insufficient quota to consume: " in this order`)
		assert.NotContains(t, res.stderr, "(prime ")
		assert.Empty(t, linesWithPrefix(res.stdout, "ok "), "no ok line after a failure")
	})

	t.Run("a word changed in one literal of the second edition", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		s.replaceVariant("prime", "quota-exceeded", 0, "package p\n\nvar s = \"exceeded quota: %s, requested: %s, used: %s, limit: %s\"\n")
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, `admission text quota-exceeded (prime k8s.io/apiserver@v0.35.6): no string literal under pkg/admission/plugin/resourcequota has the words "exceeded quota: " | ", requested: " | ", used: " | ", limited: " in this order`)
		assert.NotContains(t, res.stderr, "(community ")
	})

	t.Run("every failure is listed", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		s.replaceVariant("community", "quota-negative", 0, "package p\n\nvar s = \"x\"\n")
		s.replaceVariant("prime", "namespace-immortal", 0, "package p\n\nvar s = \"y\"\n")
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "admission text quota-negative (community ")
		assert.Contains(t, res.stderr, "admission text namespace-immortal (prime ")
	})

	for _, tc := range []struct {
		name    string
		content string
		wantOK  bool
	}{
		{
			name:    "words only in a comment",
			content: "package p\n\n// insufficient quota to consume: %s\nvar s = \"x\"\n",
		},
		{
			name:    "words only in a block comment",
			content: "package p\n\n/* insufficient quota to consume: %s */\nvar s = \"x\"\n",
		},
		{
			name:    "words spread over two literals",
			content: "package p\n\nvar a, b = \"insufficient quota \", \"to consume: %s\"\n",
		},
		{
			name:    "words in a concatenation of literals",
			content: "package p\n\nvar s = \"insufficient quota \" + \"to consume: %s\"\n",
			wantOK:  true,
		},
		{
			name:    "words in a raw string literal",
			content: "package p\n\nvar s = `insufficient quota to consume: %s`\n",
			wantOK:  true,
		},
		{
			name:    "words in a concatenation inside a function",
			content: "package p\n\nfunc f() string { return \"a\" + \"insufficient quota to consume: %s\" }\n",
			wantOK:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newAdmissionCheckSetup(t, bin, "community", "prime")
			s.replaceVariant("community", "quota-insufficient", 0, tc.content)
			res := s.runWithVersions()
			if tc.wantOK {
				assert.Zero(t, res.exit, "%s", res.stderr)
				return
			}
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, "admission text quota-insufficient (community ")
		})
	}

	t.Run("words only in a test file", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		text := s.text("quota-insufficient")
		original := s.file("community", text, 0)
		require.NoError(t, os.Rename(original, strings.TrimSuffix(original, ".go")+"_test.go"))
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "admission text quota-insufficient (community ")
	})

	t.Run("words only under another directory of the module", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		text := s.text("quota-insufficient")
		original := s.file("community", text, 0)
		moved := filepath.Join(s.moduleDir("community", text.Module), "pkg", "admission", "plugin", "other", filepath.Base(original))
		require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o755))
		require.NoError(t, os.Rename(original, moved))
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "admission text quota-insufficient (community ")
	})

	t.Run("words in a subdirectory of the directory", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		text := s.text("quota-insufficient")
		original := s.file("community", text, 0)
		moved := filepath.Join(filepath.Dir(original), "nested", filepath.Base(original))
		require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o755))
		require.NoError(t, os.Rename(original, moved))
		res := s.runWithVersions()
		assert.Zero(t, res.exit, "%s", res.stderr)
	})

	t.Run("one variant of a text present while the other is missing", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		s.replaceVariant("community", "validatingadmissionpolicy", 0, "package p\n\nvar s = \"x\"\n")
		s.removeVariant("prime", "validatingadmissionpolicy", 1)
		res := s.runWithVersions()
		assert.Zero(t, res.exit, "%s", res.stderr)
	})

	t.Run("both variants of a text missing", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		s.replaceVariant("community", "validatingadmissionpolicy", 0, "package p\n\nvar s = \"x\"\n")
		s.replaceVariant("community", "validatingadmissionpolicy", 1, "package p\n\nvar s = \"y\"\n")
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, `admission text validatingadmissionpolicy (community k8s.io/apiserver@v0.36.5): no string literal under pkg/admission/plugin/policy/validating has the words "ValidatingAdmissionPolicy '" | "' with binding '" | "' denied request: " in this order`)
		assert.Contains(t, res.stderr, `has the words "ValidatingAdmissionPolicy '" | "' denied request: " in this order`)
	})

	t.Run("a text with a variant in each of three directories", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		s.removeVariant("community", "not-ready", 0)
		s.removeVariant("community", "not-ready", 1)
		res := s.runWithVersions()
		assert.Zero(t, res.exit, "%s", res.stderr)

		s.removeVariant("community", "not-ready", 2)
		res = s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "admission text not-ready (community ")
		assert.Contains(t, res.stderr, "no string literal under pkg/admission/plugin/policy/generic has the words")
	})

	t.Run("the third edition, built from the go.mod of the repository", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime", "repository")
		s.removeVariant("repository", "quota-insufficient", 0)
		res := s.runWithVersions()
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "admission text quota-insufficient (repository k8s.io/apiserver@v0.37.1)")
		assert.NotContains(t, res.stderr, "(community ")
		assert.NotContains(t, res.stderr, "(prime ")

		s = newAdmissionCheckSetup(t, bin, "community", "prime", "repository")
		res = s.runWithVersions()
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Len(t, linesWithPrefix(res.stdout, "ok "), 9)
		assert.Contains(t, res.stdout, s.checkedLine(3))
	})
}

// admissionCheckInputs asserts the script stops with exit 1 when
// the hook names no module for an edition or no edition, and when the words
// file is unreadable, empty, invalid or lists no text or no module.
func admissionCheckInputs(t *testing.T, bin string) {
	t.Helper()

	t.Run("the hook names no module for an edition", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		res := s.run("ADMISSION_TEXT_CHECK_MODULES_FILE=" + s.modulesFile(true, "prime", "k8s.io/kubernetes"))
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "names no k8s.io/kubernetes for the edition prime")
	})

	t.Run("the hook lists no edition", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		res := s.run("ADMISSION_TEXT_CHECK_MODULES_FILE=" + s.writeJSON("modules.json", map[string]any{}))
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "names no edition")
		assert.Empty(t, linesWithPrefix(res.stdout, "checked "))
	})

	t.Run("the hook file is null in place of an object", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		res := s.run("ADMISSION_TEXT_CHECK_MODULES_FILE=" + s.writeJSON("modules.json", nil))
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "is not a JSON object")
		assert.NotContains(t, res.stderr, "cannot resolve", "a null file must not take the path that reads the network")
		assert.Empty(t, linesWithPrefix(res.stdout, "checked "))
	})

	t.Run("the binary hook is a relative path", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		res := s.runWithVersions("ADMISSION_TEXT_CHECK_BINARY=check")
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "ADMISSION_TEXT_CHECK_BINARY must be an absolute path")
	})

	t.Run("the binary hook is a directory", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		res := s.runWithVersions("ADMISSION_TEXT_CHECK_BINARY=" + t.TempDir())
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "ADMISSION_TEXT_CHECK_BINARY is not an executable file")
	})

	t.Run("the binary hook is a file that is not executable", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		file := filepath.Join(t.TempDir(), "check")
		require.NoError(t, os.WriteFile(file, []byte("data"), 0o644))
		res := s.runWithVersions("ADMISSION_TEXT_CHECK_BINARY=" + file)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "ADMISSION_TEXT_CHECK_BINARY is not an executable file")
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, "data", string(data), "the script must not overwrite the file")
	})

	t.Run("the go.mod directory has a misspelled file name", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community", "prime", "repository")
		dir := s.goModDir(map[string]string{
			"community":  admissionReplaceAll("community", nil),
			"prime":      admissionReplaceAll("prime", nil),
			"repositroy": admissionReplaceAll("repository", nil),
		})
		res := s.run("ADMISSION_TEXT_CHECK_GOMOD_DIR="+dir, "ADMISSION_TEXT_CHECK_MODULES_FILE="+s.modulesFile(false))
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "repositroy.mod, which is not the file of a checked edition")
		assert.Empty(t, linesWithPrefix(res.stdout, "resolved "), "the check stops before it reads any version")
	})

	t.Run("the go.mod directory has no .mod file", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		res := s.run("ADMISSION_TEXT_CHECK_GOMOD_DIR="+s.goModDir(map[string]string{}), "ADMISSION_TEXT_CHECK_MODULES_FILE="+s.modulesFile(false))
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "has no .mod file")
	})

	t.Run("the hook gives no version and there is no go.mod hook", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		res := s.run("ADMISSION_TEXT_CHECK_MODULES_FILE=" + s.modulesFile(false))
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "has no version for k8s.io/apiserver of the edition community")
	})

	words := func(t *testing.T, mutate func(w *admissionWords)) string {
		t.Helper()
		w := readAdmissionWords(t)
		mutate(&w)
		data, err := json.Marshal(w)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "words.json")
		require.NoError(t, os.WriteFile(path, data, 0o644))
		return path
	}
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
		want string
	}{
		{"an empty list of texts", func(t *testing.T) string {
			t.Helper()
			return words(t, func(w *admissionWords) { w.Texts = []admissionWordsText{} })
		}, "lists no texts"},
		{"an empty list of modules", func(t *testing.T) string {
			t.Helper()
			return words(t, func(w *admissionWords) { w.Modules = []string{} })
		}, "lists no modules"},
		{"an unreadable file", func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "missing.json")
		}, "reading the words file"},
		{"an empty file", func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "empty.json")
			require.NoError(t, os.WriteFile(path, nil, 0o644))
			return path
		}, "is empty"},
		{"an invalid file", func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "invalid.json")
			require.NoError(t, os.WriteFile(path, []byte("{"), 0o644))
			return path
		}, "is not valid"},
		{"a text with no words and no reason", func(t *testing.T) string {
			t.Helper()
			return words(t, func(w *admissionWords) { w.Texts[0].Variants = nil })
		}, `needs "variants" or "reason", one of the two`},
		{"a directory of a text with a parent element", func(t *testing.T) string {
			t.Helper()
			return words(t, func(w *admissionWords) { w.Texts[0].Dir = "../x" })
		}, `must be a relative path with no ".." element`},
		{"a directory of a variant that is absolute", func(t *testing.T) string {
			t.Helper()
			return words(t, func(w *admissionWords) { w.Texts[0].Variants[0].Dir = "/x" })
		}, `must be a relative path with no ".." element`},
		{"a text of a module that is not listed", func(t *testing.T) string {
			t.Helper()
			return words(t, func(w *admissionWords) { w.Texts[0].Module = "k8s.io/unknown" })
		}, "is not in its modules list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newAdmissionCheckSetup(t, bin, "community", "prime")
			path := tc.path(t)
			res := s.runWithVersions("ADMISSION_TEXT_CHECK_WORDS_FILE=" + path)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, path)
			assert.Contains(t, res.stderr, tc.want)
			assert.Empty(t, linesWithPrefix(res.stdout, "checked "))
		})
	}
}

// admissionCheckVersionRules asserts how the script reads the
// version of each module from a go.mod file: the replace directive wins over
// the require directive, a module with a replace and no require is found, and a
// replace by a local path, a replace to another module path, a module in
// neither and a version that differs from the one the modules hook file names
// each stop the script naming the edition and the module. For the repository
// edition only, k8s.io/pod-security-admission with no directive takes the
// version of k8s.io/apiserver.
func admissionCheckVersionRules(t *testing.T, bin string) {
	t.Helper()
	run := func(t *testing.T, mods map[string]string, mutate func(s *admissionCheckSetup)) admissionCheckRun {
		t.Helper()
		editions := make([]string, 0, len(mods))
		for edition := range mods {
			editions = append(editions, edition)
		}
		s := newAdmissionCheckSetup(t, bin, editions...)
		if mutate != nil {
			mutate(s)
		}
		return s.run(
			"ADMISSION_TEXT_CHECK_GOMOD_DIR="+s.goModDir(mods),
			"ADMISSION_TEXT_CHECK_MODULES_FILE="+s.modulesFile(false),
		)
	}

	t.Run("the replace directive wins over the require directive", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"community": admissionGoMod(
				"require (",
				"\tk8s.io/apiserver v0.1.0",
				"\tk8s.io/kubernetes v0.1.0",
				")",
				"replace k8s.io/apiserver => k8s.io/apiserver v0.36.5",
				"replace k8s.io/kubernetes => k8s.io/kubernetes v1.36.5",
				"replace k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.36.5",
			),
		}
		res := run(t, mods, nil)
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Contains(t, res.stdout, "resolved community k8s.io/apiserver@v0.36.5\n")
		assert.Contains(t, res.stdout, "resolved community k8s.io/kubernetes@v1.36.5\n")
		assert.Contains(t, res.stdout, "resolved community k8s.io/pod-security-admission@v0.36.5\n")
		assert.NotContains(t, res.stdout, "v0.1.0")
	})

	t.Run("a require directive alone gives the version", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"community": admissionGoMod(
				"require (",
				"\tk8s.io/apiserver v0.36.4",
				"\tk8s.io/kubernetes v1.36.4",
				"\tk8s.io/pod-security-admission v0.36.4",
				")",
			),
		}
		res := run(t, mods, nil)
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Contains(t, res.stdout, "resolved community k8s.io/apiserver@v0.36.4\n")
	})

	t.Run("a replace by a local path", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"community": admissionReplaceAll("community", map[string]string{"k8s.io/apiserver": "../apiserver"}),
			"prime":     admissionReplaceAll("prime", nil),
		}
		res := run(t, mods, nil)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "the edition community: k8s.io/apiserver is replaced by the local path ../apiserver")
	})

	t.Run("a replace to another module path", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"community": admissionReplaceAll("community", nil),
			"prime":     admissionReplaceAll("prime", map[string]string{"k8s.io/kubernetes": "example.com/fork v1.0.0"}),
		}
		res := run(t, mods, nil)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "the edition prime: k8s.io/kubernetes is replaced by another module, example.com/fork")
	})

	t.Run("a module in neither directive", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"community": admissionGoMod(
				"replace k8s.io/apiserver => k8s.io/apiserver v0.36.5",
				"replace k8s.io/kubernetes => k8s.io/kubernetes v1.36.5",
			),
		}
		res := run(t, mods, nil)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "the edition community: no replace or require directive names k8s.io/pod-security-admission")
	})

	t.Run("the version of the modules file differs from the go.mod file", func(t *testing.T) {
		t.Parallel()
		s := newAdmissionCheckSetup(t, bin, "community")
		hook := s.writeJSON("modules.json", map[string]any{"community": map[string]any{
			"k8s.io/apiserver":              map[string]string{"dir": s.moduleDir("community", "k8s.io/apiserver"), "version": "v0.99.0"},
			"k8s.io/kubernetes":             map[string]string{"dir": s.moduleDir("community", "k8s.io/kubernetes")},
			"k8s.io/pod-security-admission": map[string]string{"dir": s.moduleDir("community", "k8s.io/pod-security-admission")},
		}})
		res := s.run(
			"ADMISSION_TEXT_CHECK_GOMOD_DIR="+s.goModDir(map[string]string{"community": admissionReplaceAll("community", nil)}),
			"ADMISSION_TEXT_CHECK_MODULES_FILE="+hook,
		)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "the edition community: the go.mod file has k8s.io/apiserver@v0.36.5")
		assert.Contains(t, res.stderr, "v0.99.0")

		res = s.run(
			"ADMISSION_TEXT_CHECK_GOMOD_DIR="+s.goModDir(map[string]string{"community": admissionReplaceAll("community", nil)}),
			"ADMISSION_TEXT_CHECK_MODULES_FILE="+s.modulesFile(true),
		)
		assert.Zero(t, res.exit, "a version that equals the go.mod file passes: %s", res.stderr)
	})

	t.Run("the go.mod file of an edition is missing", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{"community": admissionReplaceAll("community", nil)}
		s := newAdmissionCheckSetup(t, bin, "community", "prime")
		res := s.run(
			"ADMISSION_TEXT_CHECK_GOMOD_DIR="+s.goModDir(mods),
			"ADMISSION_TEXT_CHECK_MODULES_FILE="+s.modulesFile(false),
		)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "prime.mod")
	})

	t.Run("the repository takes k8s.io/pod-security-admission at the version of k8s.io/apiserver", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"repository": admissionGoMod(
				"replace k8s.io/apiserver => k8s.io/apiserver v0.37.1",
				"replace k8s.io/kubernetes => k8s.io/kubernetes v1.37.1",
			),
		}
		res := run(t, mods, nil)
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Contains(t, res.stdout, "resolved repository k8s.io/pod-security-admission@v0.37.1\n")
	})

	t.Run("the repository names k8s.io/pod-security-admission", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"repository": admissionGoMod(
				"replace k8s.io/apiserver => k8s.io/apiserver v0.37.1",
				"replace k8s.io/kubernetes => k8s.io/kubernetes v1.37.1",
				"require k8s.io/pod-security-admission v0.37.0",
			),
		}
		res := run(t, mods, nil)
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Contains(t, res.stdout, "resolved repository k8s.io/pod-security-admission@v0.37.0\n")
	})

	t.Run("the repository edition reads the go.mod of this repository", func(t *testing.T) {
		t.Parallel()
		cmd := exec.CommandContext(t.Context(), "go", "mod", "edit", "-json", "go.mod")
		cmd.Env = testfs.EnvWithoutGit()
		out, err := cmd.Output()
		require.NoError(t, err)
		var edit struct {
			Replace []struct {
				Old struct{ Path, Version string }
				New struct{ Path, Version string }
			}
		}
		require.NoError(t, json.Unmarshal(out, &edit))
		want := map[string]string{}
		for _, r := range edit.Replace {
			if r.Old.Version == "" && r.New.Path == r.Old.Path {
				want[r.Old.Path] = r.New.Version
			}
		}
		require.NotEmpty(t, want["k8s.io/apiserver"], "go.mod must replace k8s.io/apiserver")
		require.NotEmpty(t, want["k8s.io/kubernetes"], "go.mod must replace k8s.io/kubernetes")

		s := newAdmissionCheckSetup(t, bin, "community", "prime", "repository")
		res := s.run(
			"ADMISSION_TEXT_CHECK_GOMOD_DIR="+s.goModDir(map[string]string{
				"community": admissionReplaceAll("community", nil),
				"prime":     admissionReplaceAll("prime", nil),
			}),
			"ADMISSION_TEXT_CHECK_MODULES_FILE="+s.modulesFile(false),
		)
		require.Zero(t, res.exit, "%s", res.stderr)
		assert.Contains(t, res.stdout, "resolved repository k8s.io/apiserver@"+want["k8s.io/apiserver"]+"\n")
		assert.Contains(t, res.stdout, "resolved repository k8s.io/kubernetes@"+want["k8s.io/kubernetes"]+"\n")
		assert.Contains(t, res.stdout, "resolved repository k8s.io/pod-security-admission@"+want["k8s.io/apiserver"]+"\n")
	})

	t.Run("an edition other than the repository gets no such version", func(t *testing.T) {
		t.Parallel()
		mods := map[string]string{
			"prime": admissionGoMod(
				"replace k8s.io/apiserver => k8s.io/apiserver v0.35.6",
				"replace k8s.io/kubernetes => k8s.io/kubernetes v1.35.6",
			),
		}
		res := run(t, mods, nil)
		assert.Equal(t, 1, res.exit)
		assert.Contains(t, res.stderr, "the edition prime: no replace or require directive names k8s.io/pod-security-admission")
	})
}

// TestAdmissionTextCheckScriptMode asserts the script is executable, because
// the Makefile runs ./scripts/<target>.
func TestAdmissionTextCheckScriptMode(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	info, err := os.Stat(filepath.FromSlash(admissionCheckScript))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "%s must have an execute bit", admissionCheckScript)
}

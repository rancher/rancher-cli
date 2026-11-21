package conformance

import (
	"fmt"
	"maps"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	sbomScript        = "scripts/release-sbom"
	sbomCheckScript   = "scripts/release-sbom-check"
	bundlesScript     = "scripts/release-bundles"
	assetsCheckScript = "scripts/release-assets-check"

	syftURLPrefix  = "https://github.com/anchore/syft/releases/download/"
	syftChecksumID = "SYFT_SHA256"
)

// releaseSbomScripts are the four scripts that write and check the SBOMs and
// the Sigstore bundles. They read files and write files and nothing else.
var releaseSbomScripts = []string{sbomScript, sbomCheckScript, bundlesScript, assetsCheckScript}

// releaseBinaries returns the binaries scripts/release-build builds, in the
// order of its platform loop: the CLI and then its credential helper for each
// platform, with .exe on Windows.
func releaseBinaries() ([]string, error) {
	platforms, err := releasePlatforms()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		ext := ""
		if goos == "windows" {
			ext = ".exe"
		}
		out = append(out,
			fmt.Sprintf("rancher-%s-%s%s", goos, goarch, ext),
			fmt.Sprintf("rancher-credential-vault-%s-%s%s", goos, goarch, ext))
	}
	return out, nil
}

// sbomStepID is the id of the step that attests the SBOM of a binary: the
// binary's name without .exe, which step ids cannot hold next to a dot.
func sbomStepID(binary string) string {
	return "sbom-" + strings.TrimSuffix(binary, ".exe")
}

// releaseBundlesEnv is the value of BUNDLES in the step that writes the
// Sigstore bundles: one line for each attestation step, the build provenance
// first, then the SBOMs in the order of the steps.
func releaseBundlesEnv(binaries []string) string {
	var b strings.Builder
	b.WriteString("provenance.sigstore.json ${{ steps.provenance.outputs.bundle-path }}\n")
	for _, bin := range binaries {
		fmt.Fprintf(&b, "%s.sbom.sigstore.json ${{ steps.%s.outputs.bundle-path }}\n", bin, sbomStepID(bin))
	}
	return b.String()
}

// attestActions are the actions that make an attestation; each step that
// uses one needs an id and a line of BUNDLES.
var attestActions = []string{
	"actions/attest-build-provenance@",
	"actions/attest-sbom@",
	"actions/attest@",
}

var bundleLineRE = regexp.MustCompile(`^(\S+) \$\{\{ steps\.([A-Za-z0-9_-]+)\.outputs\.bundle-path \}\}$`)

// releaseBundleProblems returns what is wrong with the attestation steps and
// the BUNDLES lines of each job: every step that uses an attestation action
// has an id that appears in exactly one line of BUNDLES in its job, that line
// names the bundle asset of the step (provenance.sigstore.json for the build
// provenance, <binary>.sbom.sigstore.json for the SBOM of the binary named
// by the step's subject-path), and every line names a step of its job.
func releaseBundleProblems(wf gateWorkflow) []string {
	var out []string
	for _, job := range slices.Sorted(maps.Keys(wf.Jobs)) {
		type attestation struct{ id, want string }
		var attests []attestation
		var lines []string
		for i, step := range wf.Jobs[job].Steps {
			where := fmt.Sprintf("job %s step %d", job, i+1)
			env, err := stringMap(step.Env)
			if err != nil {
				out = append(out, where+": env is not a mapping of scalars")
				continue
			}
			if value, has := env["BUNDLES"]; has {
				lines = append(lines, strings.Split(strings.TrimSuffix(value, "\n"), "\n")...)
			}
			if !slices.ContainsFunc(attestActions, func(a string) bool { return strings.HasPrefix(step.Uses, a) }) {
				continue
			}
			if step.ID == "" {
				out = append(out, where+" attests and has no id")
				continue
			}
			want := "provenance.sigstore.json"
			if !strings.HasPrefix(step.Uses, "actions/attest-build-provenance@") {
				with, err := stringMap(step.With)
				if err != nil {
					out = append(out, where+": with is not a mapping of scalars")
					continue
				}
				subject := with["subject-path"]
				if !strings.HasPrefix(subject, "dist/") || strings.ContainsAny(subject, "\n*") {
					out = append(out, fmt.Sprintf("%s has the subject-path %q, want dist/<one binary>", where, subject))
					continue
				}
				want = path.Base(subject) + ".sbom.sigstore.json"
			}
			attests = append(attests, attestation{step.ID, want})
		}
		ids := map[string]string{}
		for _, a := range attests {
			ids[a.id] = a.want
		}
		count := map[string]int{}
		for _, line := range lines {
			m := bundleLineRE.FindStringSubmatch(line)
			if m == nil {
				out = append(out, fmt.Sprintf("job %s: the BUNDLES line %q is not '<asset name> ${{ steps.<id>.outputs.bundle-path }}'", job, line))
				continue
			}
			want, known := ids[m[2]]
			switch {
			case !known:
				out = append(out, fmt.Sprintf("job %s: the BUNDLES line %q names no attestation step of the job", job, line))
			case m[1] != want:
				out = append(out, fmt.Sprintf("job %s: the BUNDLES line for step %s names %q, want %q", job, m[2], m[1], want))
			}
			count[m[2]]++
		}
		for _, a := range attests {
			if count[a.id] != 1 {
				out = append(out, fmt.Sprintf("job %s: step %s has %d lines in BUNDLES, want exactly 1", job, a.id, count[a.id]))
			}
		}
	}
	return out
}

// syftInstallTail is the last five lines of the Syft install step, as exact
// text. The first two lines are the version and the sha256 of the archive.
var syftInstallTail = []string{
	`curl -sSfL -o "$RUNNER_TEMP/syft.tar.gz" "https://github.com/anchore/syft/releases/download/${SYFT_VERSION}/syft_${SYFT_VERSION#v}_linux_amd64.tar.gz"`,
	`echo "${SYFT_SHA256}  $RUNNER_TEMP/syft.tar.gz" | sha256sum -c -`,
	`mkdir -p "$RUNNER_TEMP/syft"`,
	`tar -xzf "$RUNNER_TEMP/syft.tar.gz" -C "$RUNNER_TEMP/syft" syft`,
	`"$RUNNER_TEMP/syft/syft" version`,
}

var (
	syftVersionLineRE = regexp.MustCompile(`^SYFT_VERSION=v[0-9]+\.[0-9]+\.[0-9]+$`)
	syftChecksumLine  = regexp.MustCompile(`^SYFT_SHA256=[0-9a-f]{64}$`)
)

// syftInstallRunProblems returns what is wrong with the run text of the Syft
// install step: after the one trailing newline of the block scalar it must be
// exactly seven lines (no empty line, no continuation of a line with a
// backslash): the version, the sha256, the download, the check of the sha256
// against the downloaded file, the directory, the extraction and a call that
// fails when nothing was installed.
func syftInstallRunProblems(run string) []string {
	lines := strings.Split(strings.TrimSuffix(run, "\n"), "\n")
	var out []string
	if len(lines) != 2+len(syftInstallTail) {
		out = append(out, fmt.Sprintf("the Syft install has %d lines, want %d", len(lines), 2+len(syftInstallTail)))
	}
	for i, line := range lines {
		if strings.HasSuffix(line, `\`) {
			out = append(out, fmt.Sprintf("Syft install line %d ends in a backslash", i+1))
		}
		var ok bool
		switch {
		case i == 0:
			ok = syftVersionLineRE.MatchString(line)
		case i == 1:
			ok = syftChecksumLine.MatchString(line)
		case i-2 < len(syftInstallTail):
			ok = line == syftInstallTail[i-2]
		}
		if !ok {
			out = append(out, fmt.Sprintf("Syft install line %d is %q, which is not the allowed line", i+1, line))
		}
	}
	return out
}

// syftInstallProblems returns what is wrong with the one step of release.yaml
// that downloads Syft: it is the fourth step of the sbom job, has exactly the
// keys name and run, and its run text passes syftInstallRunProblems. Any
// other step, in any job, that names the download URL or the sha256 variable
// is refused. Syft is fetched only in a job that has no write permissions.
func syftInstallProblems(text string) []string {
	var wf struct {
		Jobs map[string]struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		return []string{err.Error()}
	}
	var out []string
	found := 0
	for _, job := range slices.Sorted(maps.Keys(wf.Jobs)) {
		for i, step := range wf.Jobs[job].Steps {
			everything := fmt.Sprint(step)
			if !strings.Contains(everything, syftURLPrefix) && !strings.Contains(everything, syftChecksumID) {
				continue
			}
			where := fmt.Sprintf("job %s step %d", job, i+1)
			if job != "sbom" || i != 3 {
				out = append(out, where+" names the Syft download; only the fourth step of job sbom may")
				continue
			}
			found++
			keys := slices.Sorted(maps.Keys(step))
			if !slices.Equal(keys, []string{"name", "run"}) {
				out = append(out, fmt.Sprintf("%s has the keys %v, want name and run", where, keys))
			}
			run, _ := step["run"].(string)
			out = append(out, syftInstallRunProblems(run)...)
		}
	}
	if found != 1 {
		out = append(out, fmt.Sprintf("found %d Syft install steps in job sbom, want 1", found))
	}
	return out
}

// requiredReleaseScripts are the scripts the commands of release.yaml must
// reach, so that a scan that loses one of them fails.
var requiredReleaseScripts = []string{
	"check-release-tag", "release-assets-check", "release-build", "release-bundles",
	"release-rehearsal-check", "release-sbom", "release-sbom-check", "release-upsert",
}

// missingReleaseScripts returns a problem for each script of
// requiredReleaseScripts that the scan of the scripts release.yaml runs did not
// reach.
func missingReleaseScripts(reached []string) []string {
	var out []string
	for _, want := range requiredReleaseScripts {
		if !slices.Contains(reached, want) {
			out = append(out, "the scan of the scripts release.yaml runs does not reach scripts/"+want)
		}
	}
	return out
}

// syftScriptProblems returns the scripts that release.yaml runs, directly or
// through other scripts, that name the Syft download URL or its sha256
// variable, and what is missing from the set of scripts the scan reached.
func syftScriptProblems(t *testing.T, commands []string) []string {
	t.Helper()
	reached := scriptsRunBy(t, commands)
	out := missingReleaseScripts(reached)
	for _, name := range reached {
		text := readText(t, "scripts/"+name)
		if strings.Contains(text, syftURLPrefix) || strings.Contains(text, syftChecksumID) {
			out = append(out, "scripts/"+name+" names the Syft download or its sha256")
		}
	}
	return out
}

// networkWords are the commands a script that only reads and writes files
// must not run at command position.
const networkWords = `curl|wget|gh|nc|ssh|scp|ftp|telnet|socat|rsync|pip|pip3|npm|npx|node|perl|ruby|python|python3`

var networkCommandRE = func() *regexp.Regexp {
	prefix := "(?:(?:^|[|;&({!`]|\\$\\()\\s*|(?:\\b(?:if|then|elif|do|command|env|exec|xargs|sudo|time|nohup)|\\b[A-Za-z_][A-Za-z0-9_]*=\\S*)\\s+)+"
	pathPrefix := `(?:[^\s|;&()]*/)?`
	word := `(?:(?:` + networkWords + `)(?:[\s|;&)]|$)` +
		`|openssl\s+s_client\b` +
		`|git(?:\s+-C\s+\S+|\s+-c\s+\S+)*\s+(?:fetch|clone|pull|ls-remote)\b` +
		`|go\s+(?:install|get|run|mod\s+download)\b)`
	return regexp.MustCompile(prefix + pathPrefix + word)
}()

// withoutQuotedText replaces the text inside quotes with spaces, so that a
// word in a message is not read as a command. A command substitution inside
// double quotes stays: it runs.
func withoutQuotedText(line string) string {
	var out strings.Builder
	var quote rune
	depth := 0
	inBackticks := false
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case depth > 0:
			out.WriteRune(r)
			switch r {
			case '(':
				depth++
			case ')':
				depth--
			}
		case inBackticks:
			out.WriteRune(r)
			if r == '`' {
				inBackticks = false
			}
		case quote == '\'':
			if r == '\'' {
				quote = 0
				out.WriteRune(r)
			} else {
				out.WriteRune(' ')
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
				out.WriteRune(r)
			case r == '$' && i+1 < len(runes) && runes[i+1] == '(':
				out.WriteString("$(")
				i++
				depth = 1
			case r == '`':
				out.WriteRune(r)
				inBackticks = true
			default:
				out.WriteRune(' ')
			}
		case r == '\'' || r == '"':
			quote = r
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// networkProblems returns the code lines of a script that call a command
// that reaches a network, at command position, or name /dev/tcp. A comment
// and a word inside a quoted message do not count. It finds the common forms
// only; it does not prove that a script cannot reach the network.
func networkProblems(text string) []string {
	var out []string
	for _, line := range shellCodeOf(text) {
		if strings.Contains(line, "/dev/tcp") || networkCommandRE.MatchString(withoutQuotedText(line)) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// shaFilterRE matches the one line that sets the jq filter for the SHA-256
// that Syft records for a binary.
var shaFilterRE = regexp.MustCompile(`(?m)^sha256_filter=.*$`)

// shaFilterProblems returns what is wrong with the sha256_filter lines of the
// scripts that read the SBOMs: each has exactly one, and the three are the
// same text.
func shaFilterProblems(texts map[string]string) []string {
	var out []string
	first := ""
	for _, name := range slices.Sorted(maps.Keys(texts)) {
		found := shaFilterRE.FindAllString(texts[name], -1)
		if len(found) != 1 {
			out = append(out, fmt.Sprintf("%s has %d lines that start with sha256_filter=, want 1", name, len(found)))
			continue
		}
		if first == "" {
			first = found[0]
		} else if found[0] != first {
			out = append(out, name+" has a sha256_filter line that differs from the others")
		}
	}
	return out
}

// TestReleaseSbomStepsAndBundles asserts, on release.yaml, that every
// attestation step has its line in BUNDLES with the right asset name, that
// the one step that downloads Syft passes the allowed list of lines, and that
// no script release.yaml runs names the Syft download.
func TestReleaseSbomStepsAndBundles(t *testing.T) {
	t.Parallel()
	wf := readGateWorkflow(t, releaseWorkflowPath)
	assert.Empty(t, releaseBundleProblems(wf))
	assert.Empty(t, syftInstallProblems(readText(t, releaseWorkflowPath)))
	assert.Empty(t, syftScriptProblems(t, workflowCommands(wf)))
	for _, job := range []string{"rehearse", "publish"} {
		assert.NotEmpty(t, wf.Jobs[job].Steps, job)
	}
	assert.Equal(t, 20, parseTimeout(t, "sbom"), "sbom has timeout-minutes 20")
}

// TestMissingReleaseScriptsRules asserts the check of the scripts the scan
// reached accepts the real set and reports each required script that is
// dropped from it.
func TestMissingReleaseScriptsRules(t *testing.T) {
	t.Parallel()
	reached := scriptsRunBy(t, workflowCommands(readGateWorkflow(t, releaseWorkflowPath)))
	require.Empty(t, missingReleaseScripts(reached))
	for _, name := range requiredReleaseScripts {
		without := slices.DeleteFunc(slices.Clone(reached), func(s string) bool { return s == name })
		assert.Equal(t, []string{"the scan of the scripts release.yaml runs does not reach scripts/" + name}, missingReleaseScripts(without), name)
	}
	assert.Len(t, missingReleaseScripts(nil), len(requiredReleaseScripts))
}

// TestReleaseBundleRules asserts what the check of the BUNDLES lines refuses:
// a step with no line, a line with no step, two lines for one step, a wrong
// asset name, a missing id, and a subject that is not one binary.
func TestReleaseBundleRules(t *testing.T) {
	t.Parallel()
	text := readText(t, releaseWorkflowPath)
	require.Empty(t, releaseBundleProblems(parseGateWorkflow(t, text)))
	const arm64 = "            rancher-linux-arm64.sbom.sigstore.json ${{ steps.sbom-rancher-linux-arm64.outputs.bundle-path }}\n"
	for _, c := range []struct{ name, old, repl string }{
		{"a line dropped", arm64, ""},
		{"a line twice", arm64, arm64 + arm64},
		{"a wrong asset name", "rancher-linux-arm64.sbom.sigstore.json ${{", "rancher-linux-amd64.sbom.sigstore.json ${{"},
		{"a name that is not a bundle", "rancher-linux-arm64.sbom.sigstore.json ${{", "rancher-linux-arm64.json ${{"},
		{"a line for no step", arm64, arm64 + "            other.sigstore.json ${{ steps.other.outputs.bundle-path }}\n"},
		{"a line that is not a pair", arm64, "            rancher-linux-arm64.sbom.sigstore.json\n"},
		{"a step with no id", "        id: sbom-rancher-linux-arm64\n", ""},
		{"a subject glob", "          subject-path: dist/rancher-linux-arm64\n", "          subject-path: dist/rancher-*\n"},
		{"the provenance line dropped", "            provenance.sigstore.json ${{ steps.provenance.outputs.bundle-path }}\n", ""},
		{"the provenance bundle under another name", "            provenance.sigstore.json ${{", "            slsa.sigstore.json ${{"},
	} {
		require.Positive(t, strings.Count(text, c.old), c.name)
		changed := strings.Replace(text, c.old, c.repl, 1)
		assert.NotEmpty(t, releaseBundleProblems(parseGateWorkflow(t, changed)), c.name)
	}
	for _, name := range attestActions {
		wf := parseGateWorkflow(t, "jobs:\n  a:\n    steps:\n      - uses: "+name+"0\n        with:\n          subject-path: dist/x\n")
		assert.NotEmpty(t, releaseBundleProblems(wf), name+" with no id and no line")
	}
}

// TestSyftInstallRules asserts what the check of the Syft install step
// refuses, as changes of its run text and of the steps around it.
func TestSyftInstallRules(t *testing.T) {
	t.Parallel()
	text := readText(t, releaseWorkflowPath)
	require.Empty(t, syftInstallProblems(text))
	const hex = "ad92bd3ec1b94a3df01d4d0aec199afa9c42b72e86da436ff5f837914d32706b"
	good := "SYFT_VERSION=v1.57.2\nSYFT_SHA256=" + hex + "\n" + strings.Join(syftInstallTail, "\n") + "\n"
	require.Empty(t, syftInstallRunProblems(good))
	curl, check := syftInstallTail[0], syftInstallTail[1]
	for _, c := range []struct{ name, old, repl string }{
		{"no sha256 check", check + "\n", ""},
		{"the latest release", "${SYFT_VERSION}/syft_", "latest/syft_"},
		{"a check of another path", check, `echo "${SYFT_SHA256}  $RUNNER_TEMP/other.tar.gz" | sha256sum -c -`},
		{"a piped installer", curl + "\n", curl + "\ncurl -sSfL https://example.com/install.sh | /bin/sh\n"},
		{"a piped installer to sudo", curl + "\n", curl + "\ncurl -sSfL https://example.com/install.sh | sudo bash\n"},
		{"a process substitution", curl + "\n", curl + "\nbash <(curl -sSfL https://example.com/install.sh)\n"},
		{"a second download", curl + "\n", curl + "\ncurl -sSfL https://example.com/other -o x\n"},
		{"a check that ignores its result", check, check + " || true"},
		{"a download split by a backslash", curl, strings.Replace(curl, `" "https`, "\" \\\n\"https", 1)},
		{"a blank line", curl + "\n", curl + "\n\n"},
		{"two trailing newlines", check + "\nmkdir", check + "\n\nmkdir"},
		{"a version that is not three numbers", "SYFT_VERSION=v1.57.2", "SYFT_VERSION=latest"},
		{"a sha256 that is short", hex, hex[:63]},
		{"an uppercase sha256", hex, strings.ToUpper(hex)},
		{"the call of the install dropped", syftInstallTail[4] + "\n", ""},
		{"the order of two lines changed", syftInstallTail[2] + "\n" + syftInstallTail[3], syftInstallTail[3] + "\n" + syftInstallTail[2]},
	} {
		require.Contains(t, good, c.old, c.name)
		assert.NotEmpty(t, syftInstallRunProblems(strings.Replace(good, c.old, c.repl, 1)), c.name)
	}
	assert.NotEmpty(t, syftInstallRunProblems(strings.TrimSuffix(good, "\n")+"\n\n"), "two trailing newlines")

	// The step around the run text.
	const step = "      - name: Install pinned Syft\n        run: |\n"
	require.Equal(t, 1, strings.Count(text, step))
	for _, c := range []struct{ name, old, repl string }{
		{"an env on the step", step, "      - name: Install pinned Syft\n        env:\n          X: y\n        run: |\n"},
		{"a shell on the step", step, "      - name: Install pinned Syft\n        shell: bash\n        run: |\n"},
		{"an id on the step", step, "      - name: Install pinned Syft\n        id: syft\n        run: |\n"},
		{"a with on the step", step, "      - name: Install pinned Syft\n        with:\n          x: y\n        run: |\n"},
		{"no name", step, "      - run: |\n"},
	} {
		assert.NotEmpty(t, syftInstallProblems(strings.Replace(text, c.old, c.repl, 1)), c.name)
	}

	// Another step that names the download or the sha256 variable.
	wf := parseGateWorkflow(t, text)
	install := wf.Jobs["sbom"].Steps[3].Run
	moved := strings.Replace(text, "      - name: Build release assets\n        env:\n          TAG: ${{ inputs.tag }}\n        run: scripts/release-build \"$TAG\" sbom\n",
		"      - name: Install pinned Syft\n        run: |\n"+indent(install, "          ")+"      - name: Build release assets\n        env:\n          TAG: ${{ inputs.tag }}\n        run: scripts/release-build \"$TAG\" sbom\n", 1)
	require.NotEqual(t, text, moved)
	assert.NotEmpty(t, syftInstallProblems(moved), "the install step in rehearse")
	for _, c := range []struct{ name, old, repl string }{
		{"the URL in a build step", "        run: scripts/release-sbom \"$TAG\"\n", "        run: scripts/release-sbom \"$TAG\"\n      - run: curl -sSfL " + syftURLPrefix + "v1/x\n"},
		{"the sha256 variable in an env", "          SYFT: ${{ runner.temp }}/syft/syft\n", "          SYFT: ${{ runner.temp }}/syft/syft\n          SYFT_SHA256: abc\n"},
	} {
		require.Equal(t, 1, strings.Count(text, c.old), c.name)
		assert.NotEmpty(t, syftInstallProblems(strings.Replace(text, c.old, c.repl, 1)), c.name)
	}
}

func indent(text, prefix string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

// TestReleaseSbomScriptsGuards asserts the four SBOM and bundle scripts call no
// network command, and that the three that read an SBOM carry the same line
// that selects the recorded SHA-256, once each.
func TestReleaseSbomScriptsGuards(t *testing.T) {
	t.Parallel()
	for _, script := range releaseSbomScripts {
		assert.Empty(t, networkProblems(readText(t, script)), script)
	}
	texts := map[string]string{}
	for _, script := range []string{sbomScript, sbomCheckScript, assetsCheckScript} {
		texts[script] = readText(t, script)
	}
	assert.Empty(t, shaFilterProblems(texts))

	changed := maps.Clone(texts)
	changed[assetsCheckScript] = strings.Replace(changed[assetsCheckScript], `"SHA256"`, `"SHA512"`, 1)
	assert.NotEmpty(t, shaFilterProblems(changed), "one line differs")
	changed[assetsCheckScript] = texts[assetsCheckScript] + "sha256_filter=x\n"
	assert.NotEmpty(t, shaFilterProblems(changed), "two lines")
	changed[assetsCheckScript] = strings.Replace(texts[assetsCheckScript], "sha256_filter=", "filter=", 1)
	assert.NotEmpty(t, shaFilterProblems(changed), "no line")
}

// TestNetworkProblemsRules asserts the network scan finds a command at
// command position in each of the usual forms, and ignores comments, quoted
// words, other words that contain one of the names, and options of other
// commands.
func TestNetworkProblemsRules(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"curl -sSf https://example.com",
		"  wget https://example.com",
		"x=$(curl https://example.com)",
		"echo hi | nc example.com 80",
		"true && gh api repos/o/r",
		"true || ssh host",
		"true; scp a b",
		"if curl x; then true; fi",
		"/usr/bin/curl x",
		"sudo curl x",
		"env FOO=1 curl x",
		"FOO=bar curl x",
		"command curl x",
		"exec wget x",
		"xargs curl x",
		"time rsync a b",
		"(python3 -c 'x')",
		"`wget x`",
		"{ ftp host; }",
		"! telnet host",
		"echo \"$(curl x)\"",
		"openssl s_client -connect a:1",
		"git fetch origin",
		"git -C dir -c a=b clone x",
		"git ls-remote x",
		"go install x",
		"go mod download",
		"go run x",
		"cat < /dev/tcp/host/80",
		"npm install",
		"perl -e x",
		"pip3 install x",
		"foo | node x",
	} {
		assert.NotEmpty(t, networkProblems(line), line)
	}
	for _, line := range []string{
		"# curl is not run here",
		"set -euo pipefail",
		`echo "error: curl is not installed" >&2`,
		`echo 'use wget'`,
		"echo \"x; curl y\"",
		"git log -1 --format=%ct",
		"git rev-parse HEAD",
		"go build ./...",
		"openssl dgst -sha256 file",
		"command -v jq",
		"gh_version=1",
		"FOO=curl",
		"mycurl x",
		"curlish x",
		"awk '{ print $1 }' file",
		"sha256sum -- file | awk '{ print $1 }'",
		"jq -e . file",
	} {
		assert.Empty(t, networkProblems(line), line)
	}
}

// TestReleaseScriptsAreExecutable asserts every script that release.yaml runs,
// directly or through other scripts, is tracked with mode 100755: the
// workflow runs them by path, and the editors that create files give them
// mode 644.
func TestReleaseScriptsAreExecutable(t *testing.T) {
	t.Parallel()
	wf := readGateWorkflow(t, releaseWorkflowPath)
	scripts := scriptsRunBy(t, workflowCommands(wf))
	require.NotEmpty(t, scripts)
	for _, name := range scripts {
		cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-s", "--", "scripts/"+name)
		cmd.Env = testfs.EnvWithoutGit()
		out, err := cmd.Output()
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(out), "100755 "), "scripts/%s must be tracked with mode 100755, git has %q", name, strings.TrimSpace(string(out)))
	}
}

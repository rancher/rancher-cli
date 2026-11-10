package install_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installerKind selects the quoting and argument rules of the source being
// scanned.
type installerKind int

const (
	shellInstaller installerKind = iota
	powerShellInstaller
)

// attestationScan is what scanAttestationCalls found in one installer source.
type attestationScan struct {
	probes    int
	checked   int
	offenders []string
	stale     []string
}

var (
	attestationVerifyPair = regexp.MustCompile(`["']?attestation["']?(?:\s*,\s*|\s+)["']?verify\b["']?`)
	runnerWord            = regexp.MustCompile(`\bgh_run\b|\bgh\b|\bInvoke-Process\b|\$gh\b`)
	shellRunnerPrefix     = regexp.MustCompile(`^(?:\w+="?\$\()?gh_run$`)
	powerShellStartWord   = regexp.MustCompile(`\bInvoke-Process\b|\bStart-Process\b|\bInvoke-Expression\b|\bSystem\.Diagnostics\.Process\b|\$gh\b|(?:^|[^\w$.-])iex\b|(?:^|[^&>])&(?:[^&]|$)|(?:^|[^\w$.-])gh(?:\.exe)?(?:[^\w.-]|$)`)
	messageFirstWords     = []string{"err", "note", "warn", "Fail", "Write-Note", "Write-Warning"}
)

const (
	powerShellCallPrefix = `$r = Invoke-Process $gh @(`
	shellCheckedIdentity = `"https://github.com/$REPO/.github/workflows/release.yaml@refs/tags/$TAG"`
	powerShellIdentity   = `"https://github.com/$Repo/.github/workflows/release.yaml@refs/tags/$($S.Tag)"`
	attestationGuide     = "every attestation verify call must name the exact workflow file and the tag being installed; see the comment above the verify loop"
)

// attestationArgs is the exact argument list of a checked verify call: the
// file, then each flag followed by its value, in any order.
type attestationArgs struct {
	file  string
	flags [][2]string
}

var (
	shellCheckedArgs = attestationArgs{file: `"$_b"`, flags: [][2]string{
		{"--repo", `"$REPO"`},
		{"--cert-identity", shellCheckedIdentity},
		{"--source-ref", `"refs/tags/$TAG"`},
	}}
	powerShellCheckedArgs = attestationArgs{file: `$b`, flags: [][2]string{
		{`'--repo'`, `$Repo`},
		{`'--cert-identity'`, powerShellIdentity},
		{`'--source-ref'`, `"refs/tags/$($S.Tag)"`},
	}}
)

// powerShellStartAllowlist is every line of install.ps1 that starts a process,
// or that holds the gh path in a variable, with the reason it is allowed. The
// checked verify call is allowed by the call scan, not listed.
var powerShellStartAllowlist = []struct{ line, reason string }{
	{`$gh = Find-Command 'gh'`, "looks up gh (not a process start)"},
	{`if (-not $gh) { return }`, "tests the lookup (not a process start)"},
	{`if (-not $gh) { return $null }`, "tests the lookup (not a process start)"},
	{`$r = Invoke-Process $gh @('auth', 'token', '--hostname', $hostName)`, "token lookup"},
	{`$r = Invoke-Process $gh @('attestation', 'verify', '--help')`, "capability probe"},
	{`$r = Invoke-Process $gh @('auth', 'status')`, "capability probe"},
	{`return $gh`, "returns the lookup (not a process start)"},
	{`$gh = Test-GhCapable`, "assigns the probe result (not a process start)"},
	{`if ($gh) {`, "tests the probe result (not a process start)"},
	{`$r = Invoke-Process (Join-Path $S.BinDir $CliName) @('--version')`, "the installed CLI prints its version"},
	{`$p = New-Object System.Diagnostics.Process`, "the process object Invoke-Process runs"},
}

// stripQuoted blanks the text inside quotes, so a word in a message is not read
// as a command word.
func stripQuoted(s string, kind installerKind) string {
	var b strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' && kind == shellInstaller && i+1 < len(s) {
				i++
			} else if c == q {
				q = 0
			}
			b.WriteByte(' ')
		case c == '\'' || c == '"':
			q = c
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// cutCall cuts s at the first unquoted `||`, `&&`, `;`, `|`, `&` or
// redirection, and drops the file descriptor digit before a redirection.
func cutCall(s string, kind installerKind) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' && kind == shellInstaller && i+1 < len(s) {
				i++
			} else if c == q {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		case strings.IndexByte("|&;<>", c) >= 0:
			s = strings.TrimRight(s[:i], " \t")
			if f := strings.Fields(s); len(f) > 0 && strings.Trim(f[len(f)-1], "0123456789") == "" && strings.HasSuffix(s, " "+f[len(f)-1]) {
				s = strings.TrimSuffix(s, f[len(f)-1])
			}
			return s
		}
	}
	return s
}

// splitArgs splits s into words at white space (and commas for PowerShell
// arrays) outside quotes. A quoted word stays whole, quotes included.
func splitArgs(s string, kind installerKind) []string {
	var words []string
	var cur strings.Builder
	var q byte
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			cur.WriteByte(c)
			if c == '\\' && q == '"' && kind == shellInstaller && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else if c == q {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
			cur.WriteByte(c)
		case c == ' ' || c == '\t' || (c == ',' && kind == powerShellInstaller):
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return words
}

// logicalLines removes carriage returns and comment lines, and joins the
// backslash continuations of a shell source.
func logicalLines(kind installerKind, src string) []string {
	var out []string
	var cur string
	continued := false
	for raw := range strings.SplitSeq(strings.ReplaceAll(src, "\r", ""), "\n") {
		if !continued && strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if kind == shellInstaller && strings.HasSuffix(raw, `\`) {
			part := strings.TrimSuffix(raw, `\`)
			if continued {
				part = strings.TrimLeft(part, " \t")
			}
			cur += part
			if continued || strings.TrimSpace(part) != "" {
				cur += " "
			}
			continued = true
			continue
		}
		if continued {
			raw = cur + strings.TrimLeft(raw, " \t")
			cur, continued = "", false
		}
		out = append(out, raw)
	}
	if continued {
		out = append(out, cur)
	}
	return out
}

// isMessageLine reports whether a line is text: its first word is a message
// command and no command word that runs gh stands outside its quoted strings.
func isMessageLine(line string, kind installerKind) bool {
	words := strings.Fields(line)
	if len(words) == 0 || !slices.Contains(messageFirstWords, words[0]) {
		return false
	}
	return !runnerWord.MatchString(stripQuoted(line, kind))
}

// scanAttestationCalls scans one installer source. A call is any line that
// holds the words attestation and verify (after joining continuations, with
// comments and message lines left out). The probe is the call whose only
// argument is --help. Every other call must be written exactly as the checked
// form: run through gh_run (install.sh) or Invoke-Process (install.ps1), and
// holding only the file, --repo, --cert-identity and --source-ref with their
// exact values, each once. A call that runs gh any other way, or holds any
// other token, is an offender, so a new gh flag needs no entry in a deny list.
// For install.ps1 every other line that starts a process or holds the gh path
// must be on powerShellStartAllowlist, and an entry with no line is stale.
func scanAttestationCalls(kind installerKind, src string) attestationScan {
	var res attestationScan
	accepted := map[string]bool{}
	reported := map[string]bool{}
	offend := func(line, rule string) {
		res.offenders = append(res.offenders, line+" ["+rule+"]")
		reported[squash(line)] = true
	}
	args := shellCheckedArgs
	if kind == powerShellInstaller {
		args = powerShellCheckedArgs
	}
	for _, line := range logicalLines(kind, src) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isMessageLine(trimmed, kind) {
			continue
		}
		loc := attestationVerifyPair.FindStringIndex(trimmed)
		if loc == nil {
			continue
		}
		prefix := squash(trimmed[:loc[0]])
		seg := strings.TrimSpace(cutCall(trimmed[loc[1]:], kind))
		if kind == powerShellInstaller {
			seg = strings.TrimSuffix(strings.TrimLeft(seg, ", \t"), ")")
		}
		words := splitArgs(seg, kind)
		isProbe := len(words) > 0 && strings.Trim(words[0], "'") == "--help"
		okPrefix := shellRunnerPrefix.MatchString(prefix)
		if kind == powerShellInstaller {
			okPrefix = prefix == powerShellCallPrefix
		}
		if !okPrefix {
			offend(trimmed, "gh is run in a way the scan cannot read")
			continue
		}
		if isProbe {
			if len(words) != 1 {
				offend(trimmed, "the capability probe holds more than --help")
				continue
			}
			res.probes++
			accepted[squash(trimmed)] = true
			continue
		}
		if why := checkAttestationArgs(words, args); why != "" {
			offend(trimmed, why)
			continue
		}
		res.checked++
		accepted[squash(trimmed)] = true
	}
	if kind == powerShellInstaller {
		scanPowerShellStarts(src, accepted, reported, &res)
	}
	return res
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// checkAttestationArgs returns why words is not the checked argument list, or
// "" when it is.
func checkAttestationArgs(words []string, want attestationArgs) string {
	rest := slices.Clone(words)
	for _, f := range want.flags {
		i := slices.Index(rest, f[0])
		if i < 0 {
			return "the call has no " + strings.Trim(f[0], "'")
		}
		if i+1 >= len(rest) || rest[i+1] != f[1] {
			return "the value of " + strings.Trim(f[0], "'") + " is not " + f[1]
		}
		rest = slices.Delete(rest, i, i+2)
	}
	if len(rest) != 1 || rest[0] != want.file {
		return fmt.Sprintf("the call holds arguments beyond the checked form: %q", rest)
	}
	return ""
}

// scanPowerShellStarts applies the allowlist of process starts to install.ps1.
func scanPowerShellStarts(src string, accepted, reported map[string]bool, res *attestationScan) {
	used := map[string]bool{}
	for _, line := range logicalLines(powerShellInstaller, src) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "function Invoke-Process") {
			continue
		}
		if !powerShellStartWord.MatchString(stripQuoted(trimmed, powerShellInstaller)) {
			continue
		}
		norm := squash(trimmed)
		listed := slices.ContainsFunc(powerShellStartAllowlist, func(e struct{ line, reason string }) bool { return e.line == norm })
		if listed {
			used[norm] = true
		}
		if listed || accepted[norm] || reported[norm] {
			continue
		}
		res.offenders = append(res.offenders, trimmed+" [a process start that is not on the allowlist]")
	}
	for _, e := range powerShellStartAllowlist {
		if !used[e.line] {
			res.stale = append(res.stale, e.line)
		}
	}
}

// TestEveryAttestationVerifyNamesTheTag asserts, over the real install.sh and
// install.ps1, that every attestation verify call is the checked form: it names
// the exact release.yaml identity for refs/tags/<tag being installed> and the
// same ref as the source ref, and holds no other argument. The scan joins
// continuation lines, removes carriage returns and skips comments and message
// lines. It cuts a call at the first unquoted `||`, `&&`, `;`, `|` or
// redirection, so the error text that follows is not read as arguments. Every
// call it cannot read (eval, a variable of arguments, & $gh, Start-Process) is
// an offender. It asserts that each file has one capability probe and at least
// one checked call, so a new call is checked and never ignored. In install.ps1
// it also asserts that every line that runs a command through Invoke-Process,
// Start-Process, the call operator (&), Invoke-Expression, a bare gh word or
// System.Diagnostics.Process, or that holds $gh, is on an allowlist with no
// stale entry. The scan does not see these forms: in install.sh a gh_run call
// whose subcommand is not a literal word (for example a variable); in
// install.ps1 a process started through cmd /c, Invoke-Command, a script
// block, [Diagnostics.Process]::Start without the System prefix, or
// InvokeCommand.InvokeScript. The tests that run an install require exactly
// two verify calls with the exact arguments, so a call of these forms that
// runs is caught there. The allowlist of tokens makes substring
// matching of short flags moot: any token that is not listed is an offender.
func TestEveryAttestationVerifyNamesTheTag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		kind installerKind
	}{
		{"../install.sh", shellInstaller},
		{"../install.ps1", powerShellInstaller},
	} {
		t.Run(filepath.Base(tc.file), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(tc.file)
			require.NoError(t, err)
			res := scanAttestationCalls(tc.kind, string(raw))
			t.Logf("%s: %d probe, %d checked call(s)", tc.file, res.probes, res.checked)
			for _, o := range res.offenders {
				t.Errorf("%s: %s\n%s", tc.file, o, attestationGuide)
			}
			assert.Equal(t, 1, res.probes, "one capability probe")
			assert.GreaterOrEqual(t, res.checked, 1, "at least one checked call")
			assert.Empty(t, res.stale, "allowlist entries that match no line")
		})
	}
}

const (
	shellDecidedOneLine = `gh_run attestation verify "$_b" --repo "$REPO" --cert-identity "https://github.com/$REPO/.github/workflows/release.yaml@refs/tags/$TAG" --source-ref "refs/tags/$TAG" >/dev/null 2>&1 || err "build provenance attestation failed for $_b - it needs an attestation that release.yaml made for refs/tags/$TAG"` + "\n"

	shellDecidedMultiLine = `      gh_run attestation verify "$_b" --repo "$REPO" \
        --cert-identity "https://github.com/$REPO/.github/workflows/release.yaml@refs/tags/$TAG" \
        --source-ref "refs/tags/$TAG" >/dev/null 2>&1 || \
        err "build provenance attestation failed for $_b - it needs an attestation that release.yaml made for refs/tags/$TAG"
`

	shellProbe = `  _help="$(gh_run attestation verify --help 2>/dev/null)" || return 1` + "\n"

	powerShellDecided = `                $r = Invoke-Process $gh @('attestation', 'verify', $b, '--repo', $Repo, '--cert-identity', "https://github.com/$Repo/.github/workflows/release.yaml@refs/tags/$($S.Tag)", '--source-ref', "refs/tags/$($S.Tag)")` + "\n"
)

// mutateSource returns src with the first from replaced by to, and fails the
// test when from is absent, so a row cannot pass by changing nothing.
func mutateSource(t *testing.T, src, from, to string) string {
	t.Helper()
	require.Contains(t, src, from, "the row's edit must apply")
	return strings.Replace(src, from, to, 1)
}

// TestAttestationVerifyScanRows asserts, over rows of source text, that the
// scan reports exactly one offender for each call written any way other than
// the checked form (one per call, however many things are wrong with it) and
// none for the checked form, comments, messages and the capability probe.
func TestAttestationVerifyScanRows(t *testing.T) {
	t.Parallel()
	sh, ps := shellInstaller, powerShellInstaller
	one := shellDecidedOneLine
	rows := []struct {
		name string
		kind installerKind
		src  string
		want int
	}{
		{"decided shell call", sh, one, 0},
		{"decided shell call over continuation lines", sh, shellDecidedMultiLine, 0},
		{"decided shell call with carriage returns", sh, strings.ReplaceAll(shellDecidedMultiLine, "\n", "\r\n"), 0},
		{"decided PowerShell call", ps, powerShellDecided, 0},
		{"decided PowerShell call with carriage returns", ps, strings.ReplaceAll(powerShellDecided, "\n", "\r\n"), 0},
		{"shell probe", sh, shellProbe, 0},
		{"source ref dropped", sh, mutateSource(t, one, ` --source-ref "refs/tags/$TAG"`, ""), 1},
		{"cert identity dropped", sh, mutateSource(t, one, ` --cert-identity "https://github.com/$REPO/.github/workflows/release.yaml@refs/tags/$TAG"`, ""), 1},
		{"source ref of the main branch", sh, mutateSource(t, one, `--source-ref "refs/tags/$TAG"`, `--source-ref "refs/heads/main"`), 1},
		{"source ref of the bare tag", sh, mutateSource(t, one, `--source-ref "refs/tags/$TAG"`, `--source-ref "$TAG"`), 1},
		{"source ref spelled with braces", sh, mutateSource(t, one, `--source-ref "refs/tags/$TAG"`, `--source-ref "refs/tags/${TAG}"`), 1},
		{"identity of another tag", sh, mutateSource(t, one, `release.yaml@refs/tags/$TAG"`, `release.yaml@refs/tags/$OTHER"`), 1},
		{"signer workflow in place of the identity", sh, mutateSource(t, one,
			`--cert-identity "https://github.com/$REPO/.github/workflows/release.yaml@refs/tags/$TAG"`,
			`--signer-workflow "$REPO/.github/workflows/release.yaml"`), 1},
		{"signer workflow added back", sh, mutateSource(t, one, ` --source-ref "refs/tags/$TAG"`,
			` --signer-workflow "$REPO/.github/workflows/release.yaml" --source-ref "refs/tags/$TAG"`), 1},
		{"identity regex in place of the identity", sh, mutateSource(t, one, `--cert-identity "https`, `--cert-identity-regex "https`), 1},
		{"owner added", sh, mutateSource(t, one, ` --source-ref`, ` --owner rancher --source-ref`), 1},
		{"predicate type added", sh, mutateSource(t, one, ` --source-ref`, ` --predicate-type x --source-ref`), 1},
		{"short flag added", sh, mutateSource(t, one, ` --source-ref`, ` -i x --source-ref`), 1},
		{"two wrong flags on one call", sh, mutateSource(t, one, ` --source-ref`, ` --owner x --format json --source-ref`), 1},
		{"short flag inside a value", sh, mutateSource(t, one, `--repo "$REPO"`, `--repo "$REPO-i"`), 1},
		{"flags in the error text after the cut", sh, mutateSource(t, one, `failed for $_b`, `failed for $_b -i x --signer-workflow y`), 0},
		{"second call of the checksums without the flags", sh, one + `gh_run attestation verify "$WORK/SHA256SUMS" --repo "$REPO"` + "\n", 1},
		{"call through eval", sh, `eval 'gh_run attestation verify "$_b" --repo "$REPO"'` + "\n", 1},
		{"call after another command", sh, `warn "x"; gh_run attestation verify "$_b" --repo "$REPO"` + "\n", 1},
		{"probe with an extra flag", sh, `gh_run attestation verify --help --format json` + "\n", 1},
		{"comment holding a bad call", sh, "# gh_run attestation verify x --signer-workflow y\n", 0},
		{"message naming the flag", sh, `err "needs a gh whose 'gh attestation verify' supports --signer-workflow"` + "\n", 0},
		{"PowerShell tag without the subexpression", ps, mutateSource(t, powerShellDecided, `"refs/tags/$($S.Tag)")`, `"refs/tags/$S.Tag")`), 1},
		{"PowerShell call through the call operator", ps, `$r = & $gh attestation verify $b --repo $Repo` + "\n", 1},
		{"PowerShell call as a multi-line array", ps, "$r = Invoke-Process $gh @('attestation',\n    'verify', $b, '--repo', $Repo)\n", 1},
		{"PowerShell call with the pair built in pieces", ps, `$a = @('attestation'); Invoke-Process $gh ($a + 'verify' + $b)` + "\n", 1},
		{"PowerShell call through the call operator and a variable of arguments", ps, "$r = & $tool @a\n", 1},
		{"PowerShell call through the call operator and a quoted path", ps, "$r = & \"C:\\Tools\\runner.exe\" attestation\n", 1},
		{"PowerShell redirection of the error stream", ps, "$x = Get-Thing 2>&1\n", 0},
		{"PowerShell call through a bare gh", ps, "$v = 'verify'\ngh attestation $v $b --repo $Repo\n", 1},
		{"PowerShell call through gh.exe", ps, "$v = 'verify'\ngh.exe attestation $v $b --repo $Repo\n", 1},
		{"PowerShell call through the call operator and a variable", ps, "$null = & gh 'attestation' $v $b\n", 1},
		{"PowerShell call through Invoke-Expression", ps, `Invoke-Expression "gh attestation $v $b"` + "\n", 1},
		{"PowerShell call through iex", ps, `iex "gh attestation $v $b"` + "\n", 1},
		{"PowerShell process object outside Invoke-Process", ps, "$q = New-Object System.Diagnostics.Process\n", 1},
		{"PowerShell message naming gh", ps, `Write-Note "gh is not installed"` + "\n", 0},
		{"PowerShell call with an argument variable", ps, `$r = Invoke-Process $gh $verifyArgs` + "\n", 1},
		{"PowerShell Start-Process", ps, `Start-Process gh` + "\n", 1},
		{"PowerShell verify call with a variable of arguments and the pair on one line", ps,
			`$verifyArgs = @('attestation', 'verify', (Join-Path $S.Work 'SHA256SUMS'), '--repo', $Repo)` + "\n" +
				`$r = Invoke-Process $gh $verifyArgs` + "\n", 2},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := scanAttestationCalls(tc.kind, tc.src)
			assert.Lenf(t, res.offenders, tc.want, "offenders: %q", res.offenders)
		})
	}

	t.Run("stale allowlist entry", func(t *testing.T) {
		t.Parallel()
		res := scanAttestationCalls(ps, powerShellDecided)
		assert.Len(t, res.stale, len(powerShellStartAllowlist), "a source with none of the listed lines leaves every entry stale")
		raw, err := os.ReadFile("../install.ps1")
		require.NoError(t, err)
		dropped := mutateSource(t, string(raw), "return $gh\n", "return $null\n")
		assert.Equal(t, []string{"return $gh"}, scanAttestationCalls(ps, dropped).stale)
	})
}

// attestingGh builds a dir with a gh that records each call as one line, every
// argument in brackets, in the file named by RANCHER_TEST_GH_CALL_LOG, and acts
// like gh against an asset whose only attestation was made for the ref in
// RANCHER_TEST_GH_ATTESTED_REF (for example refs/tags/v1.2.3).
//
// The help text of "attestation verify" lists --cert-identity and
// --signer-workflow, and --source-ref unless RANCHER_TEST_GH_HELP is old (a gh
// that predates it, which then fails the flag as unknown). Verifying a file
// exits 1 with gh's message when two of the identity flags are given. It exits
// 0 only when --cert-identity is exactly the release.yaml identity for the
// attested ref and --source-ref is exactly the attested ref, case included. The
// exact comparison of --source-ref matches gh 2.102.0 and newer and is stricter
// than older gh, which ignores case: the result of an install must not depend
// on that difference, and with the exact identity it does not. gh auth status
// succeeds and gh auth token fails.
func attestingGh(t *testing.T) (dir, callLog string) {
	t.Helper()
	dir = t.TempDir()
	callLog = filepath.Join(t.TempDir(), "gh-calls")
	script := `#!/bin/sh
for a in "$@"; do printf '[%s]' "$a" >> "$RANCHER_TEST_GH_CALL_LOG"; done
printf '\n' >> "$RANCHER_TEST_GH_CALL_LOG"
case "$1 $2" in
  'auth token') exit 1 ;;
  'auth status') exit 0 ;;
  'attestation verify') ;;
  *) exit 0 ;;
esac
if [ "$3" = --help ]; then
  echo '      --cert-identity string     Enforce that the certificate identity matches the provided value'
  echo '      --signer-workflow string   Enforce that the workflow that signed the attestation matches the provided value'
  if [ "${RANCHER_TEST_GH_HELP:-}" != old ]; then
    echo '      --source-ref string        Enforce that the git ref matches the provided value'
  fi
  exit 0
fi
shift 3
ident=
ref=
group=0
while [ $# -gt 0 ]; do
  case "$1" in
    --cert-identity) ident="$2"; group=$((group + 1)); shift 2 ;;
    --cert-identity-regex|--signer-repo|--signer-workflow) group=$((group + 1)); shift 2 ;;
    --source-ref)
      if [ "${RANCHER_TEST_GH_HELP:-}" = old ]; then echo 'unknown flag: --source-ref' >&2; exit 1; fi
      ref="$2"; shift 2 ;;
    --repo) shift 2 ;;
    *) shift ;;
  esac
done
if [ "$group" -ge 2 ]; then
  echo 'if any flags in the group [cert-identity cert-identity-regex signer-repo signer-workflow] are set none of the others can be' >&2
  exit 1
fi
want="https://github.com/rancher/rancher-cli/.github/workflows/release.yaml@$RANCHER_TEST_GH_ATTESTED_REF"
if [ "$ident" = "$want" ] && [ "$ref" = "$RANCHER_TEST_GH_ATTESTED_REF" ]; then exit 0; fi
echo 'Error: verifying with issuer "sigstore.dev"' >&2
exit 1
`
	testfs.WriteExecutable(t, filepath.Join(dir, "gh"), []byte(script))
	return dir, callLog
}

// attestRun describes one install.sh run against attestingGh.
type attestRun struct {
	tag      string // the release the stub serves
	attested string // the ref the asset's attestation was made for
	helpOld  bool   // gh predates --source-ref
	parser   bool   // jq and python3 are on PATH (needed to resolve --pre)
	latest   bool   // the stub's latest release is tag
	env      []string
	args     []string
}

// attestResult is what one run produced.
type attestResult struct {
	out, errOut string
	code        int
	calls       []string
	home        string
	binDir      string
}

func runAttesting(t *testing.T, r attestRun) attestResult {
	t.Helper()
	s := newStub(t)
	writeRelease(s, r.tag, false)
	if r.latest {
		s.setLatest(r.tag)
	}
	ghDir, callLog := attestingGh(t)
	tools := baseTools
	if r.parser {
		tools = append(slices.Clone(baseTools), "jq", "python3")
	}
	res := attestResult{home: t.TempDir(), binDir: filepath.Join(t.TempDir(), "bin")}
	env := slices.Concat(envHome(res.home), []string{
		"PATH=" + ghDir + string(os.PathListSeparator) + toolPath(t, tools...),
		"RANCHER_TEST_GH_CALL_LOG=" + callLog,
		"RANCHER_TEST_GH_ATTESTED_REF=" + r.attested,
	}, r.env)
	if r.helpOld {
		env = append(env, "RANCHER_TEST_GH_HELP=old")
	}
	args := append([]string{"--bin-dir", res.binDir}, r.args...)
	res.out, res.errOut, res.code = runInstall(t, s, env, args...)
	if _, err := os.Stat(callLog); err == nil {
		res.calls = readLines(t, callLog)
	}
	return res
}

// verifyCall is one "gh attestation verify <file> ..." line of the call log.
type verifyCall struct {
	file string
	rest string // the bracketed arguments after the file
}

// verifyCalls returns the calls that verify a file; the capability probe
// (--help) is not one.
func verifyCalls(calls []string) []verifyCall {
	const head = "[attestation][verify]["
	var out []verifyCall
	for _, c := range calls {
		after, ok := strings.CutPrefix(c, head)
		if !ok || strings.HasPrefix(after, "--help]") {
			continue
		}
		file, rest, _ := strings.Cut(after, "]")
		out = append(out, verifyCall{file: file, rest: rest})
	}
	return out
}

// assertNothingInstalled asserts install.sh left no installed file, link,
// receipt or version directory under the given home and bin dir.
func assertNothingInstalled(t *testing.T, home, binDir string) {
	t.Helper()
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		_, err := os.Lstat(filepath.Join(binDir, name))
		assert.Truef(t, os.IsNotExist(err), "%s must not be installed: %v", name, err)
	}
	_, err := os.Lstat(filepath.Join(home, ".config", "rancher-cli", "install-receipt.json"))
	assert.True(t, os.IsNotExist(err), "no receipt may be written: %v", err)
	data := filepath.Join(home, ".local", "share", "rancher-cli")
	_, err = os.Lstat(filepath.Join(data, "current"))
	assert.True(t, os.IsNotExist(err), "no current link may exist: %v", err)
	entries, err := os.ReadDir(filepath.Join(data, "versions"))
	if err == nil {
		assert.Empty(t, entries, "no version may be staged")
	} else {
		assert.True(t, os.IsNotExist(err), "versions: %v", err)
	}
}

// TestAttestationVerifyNamesTheInstalledTag asserts that, whichever way the
// tag is chosen (the latest release, --version, RANCHER_CLI_VERSION, --pre),
// install.sh verifies each of the two binaries with exactly the repository, the
// release.yaml identity for refs/tags/<tag> and that ref as the source ref, and
// that an attestation made for that ref passes.
func TestAttestationVerifyNamesTheInstalledTag(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		run  attestRun
	}{
		{"latest release", attestRun{tag: "v1.2.3", latest: true}},
		{"version flag", attestRun{tag: "v1.2.3", args: []string{"--version", "v1.2.3"}}},
		{"version variable", attestRun{tag: "v1.2.3", env: []string{"RANCHER_CLI_VERSION=v1.2.3"}}},
		{"pre-release", attestRun{tag: "v1.3.0-rc.1", parser: true, args: []string{"--pre"}}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.run
			r.attested = "refs/tags/" + r.tag
			r.args = append(slices.Clone(r.args), "--verify-attestation")
			res := runAttesting(t, r)
			require.Equalf(t, 0, res.code, "stderr: %s", res.errOut)
			assert.Contains(t, res.out+res.errOut, "build provenance attestation verified")
			wantRest := "][--repo][rancher/rancher-cli][--cert-identity]" +
				"[https://github.com/rancher/rancher-cli/.github/workflows/release.yaml@refs/tags/" + r.tag + "]" +
				"[--source-ref][refs/tags/" + r.tag + "]"
			calls := verifyCalls(res.calls)
			require.Lenf(t, calls, 2, "one verify call per binary: %q", res.calls)
			var helpers int
			for _, c := range calls {
				assert.Equal(t, wantRest, "]"+c.rest, "arguments after the file")
				if strings.HasPrefix(filepath.Base(c.file), "rancher-credential-vault-") {
					helpers++
				}
			}
			assert.Equal(t, 1, helpers, "exactly one call names the helper: %q", calls)
		})
	}
}

// TestAttestationOfAnotherRefFailsTheInstall asserts that an attestation made
// for another ref (a rehearsal on the main branch, another tag, a longer tag,
// the tag in another case) fails the install, with and without
// --verify-attestation, and leaves nothing installed.
func TestAttestationOfAnotherRefFailsTheInstall(t *testing.T) {
	t.Parallel()
	for _, attested := range []string{"refs/heads/main", "refs/tags/v1.2.2", "refs/tags/v1.2.3-rc.1", "refs/tags/V1.2.3"} {
		for _, flag := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s with flag %t", attested, flag), func(t *testing.T) {
				t.Parallel()
				r := attestRun{tag: "v1.2.3", attested: attested, args: []string{"--version", "v1.2.3"}}
				if flag {
					r.args = append(r.args, "--verify-attestation")
				}
				res := runAttesting(t, r)
				require.NotEqual(t, 0, res.code, "stdout: %s", res.out)
				assert.Contains(t, res.errOut, "build provenance attestation failed")
				assert.Contains(t, res.errOut, "refs/tags/v1.2.3")
				assertNothingInstalled(t, res.home, res.binDir)
			})
		}
	}
}

// TestGhWithoutSourceRefSkipsAttestation asserts that a gh whose help does not
// list --source-ref takes the skip path: a notice and a normal install, or a
// stop with the version message under --verify-attestation. No file is
// verified in either case.
func TestGhWithoutSourceRefSkipsAttestation(t *testing.T) {
	t.Parallel()
	t.Run("without the flag", func(t *testing.T) {
		t.Parallel()
		res := runAttesting(t, attestRun{tag: "v1.2.3", attested: "refs/tags/v1.2.3", helpOld: true, latest: true})
		require.Equalf(t, 0, res.code, "stderr: %s", res.errOut)
		assert.Contains(t, res.out+res.errOut, "notice: attestation skipped (gh is not installed, is older than 2.68.0, or is not logged in)")
		assert.Empty(t, verifyCalls(res.calls), "no file may be verified: %q", res.calls)
	})
	t.Run("with the flag", func(t *testing.T) {
		t.Parallel()
		res := runAttesting(t, attestRun{tag: "v1.2.3", attested: "refs/tags/v1.2.3", helpOld: true, latest: true,
			args: []string{"--verify-attestation"}})
		require.NotEqual(t, 0, res.code)
		assert.Contains(t, res.errOut, "--verify-attestation needs gh 2.68.0 or newer")
		assert.Empty(t, verifyCalls(res.calls), "no file may be verified: %q", res.calls)
		assertNothingInstalled(t, res.home, res.binDir)
	})
}

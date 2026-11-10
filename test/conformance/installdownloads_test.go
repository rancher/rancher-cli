package conformance

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every release asset download in install.sh and install.ps1 goes through
// one retry function, which is the only caller of the attempt functions that
// write an asset file. These scans read each script, split it into function
// bodies by name, and fail when a request that writes a file appears outside
// the attempt functions, when a request of any kind sits in a function no
// allowlist row names, or when an attempt function is called from anywhere
// but the retry function.

// shellFunctions maps each function name of a POSIX sh script to its text
// without comment lines. A function is a line "name() {" through the next
// line that is just "}", or one line "name() { ... }".
func shellFunctions(t *testing.T, path string) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	start := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(\) \{(.*)$`)
	funcs := map[string]string{}
	var outside strings.Builder
	for i := 0; i < len(lines); i++ {
		m := start.FindStringSubmatch(lines[i])
		if m == nil {
			if !isCommentLine(lines[i]) {
				outside.WriteString(lines[i] + "\n")
			}
			continue
		}
		body := []string{m[2]}
		if !strings.HasSuffix(strings.TrimSpace(m[2]), "}") {
			for i++; i < len(lines) && lines[i] != "}"; i++ {
				body = append(body, lines[i])
			}
		}
		funcs[m[1]] = stripComments(strings.Join(body, "\n"))
	}
	return funcs, outside.String()
}

// powerShellFunctions maps each function declared at four spaces of
// indentation in install.ps1 (the phase functions of Install-RancherCli) to
// its text without comment lines, from "    function Name" through the next
// line that is "    }". The second result is the rest of the file without
// comment lines.
func powerShellFunctions(t *testing.T, path string) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	// A statement continued with a backtick is one line.
	text := regexp.MustCompile("`\\n\\s*").ReplaceAllString(strings.ReplaceAll(string(raw), "\r\n", "\n"), " ")
	lines := strings.Split(text, "\n")
	start := regexp.MustCompile(`^    function ([A-Za-z][A-Za-z0-9-]*)\b`)
	funcs := map[string]string{}
	var outside strings.Builder
	for i := 0; i < len(lines); i++ {
		m := start.FindStringSubmatch(lines[i])
		if m == nil {
			if !isCommentLine(lines[i]) {
				outside.WriteString(lines[i] + "\n")
			}
			continue
		}
		body := []string{lines[i]}
		if !strings.HasSuffix(strings.TrimSpace(lines[i]), "}") {
			for i++; i < len(lines) && lines[i] != "    }"; i++ {
				body = append(body, lines[i])
			}
		}
		funcs[m[1]] = stripComments(strings.Join(body, "\n"))
	}
	return funcs, outside.String()
}

func isCommentLine(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }

func stripComments(s string) string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if !isCommentLine(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// shellRequestStart matches a request as a command: curl, curl_authed or
// fetch at the start of a line or after a pipe, semicolon, "&&", "||", "!",
// "$(" or a shell keyword that begins a command - not inside a message string
// or the preflight tool list.
var shellRequestStart = regexp.MustCompile(`(?m)(?:^|[|;!]|&&|\$\(|\b(?:if|then|else|elif|do|while|until))\s*(curl_authed|curl|fetch)\b`)

// shellFileWrite matches a curl option that writes a file: -o alone or at the
// end of a short-option group (-fsSLo), and --output.
var shellFileWrite = regexp.MustCompile(`(?:^|\s)(?:-[A-Za-z]*o|--output)\s+(\S+)`)

// shellRedirect matches a redirect of standard output on a request line.
var shellRedirect = regexp.MustCompile(`(?:^|[^&0-9<>])>\s*(\S+)`)

// shellRequestAllowlist names every install.sh function that may start a
// curl, curl_authed or fetch request, with the reason. The functions that
// write an asset file are in shellAttempts.
var shellRequestAllowlist = map[string]string{
	"curl_authed":     "the single place the token reaches curl",
	"fetch":           "resolution requests: the release tag and the release listing",
	"resolve_tag":     "resolution request: the latest release redirect",
	"resolve_pre_tag": "resolution request: the release listing",
	"fetch_asset_ids": "resolution request: the release metadata",
}

// shellAttempts are the install.sh functions that make one download attempt
// and write the asset file. They are called only from shellRetry.
var shellAttempts = []string{"dl_attempt_browser", "dl_attempt_api"}

const shellRetry = "download_asset"

func TestInstallShAssetDownloadsAllGoThroughTheRetry(t *testing.T) {
	t.Parallel()
	funcs, outside := shellFunctions(t, "install.sh")
	require.Contains(t, funcs, shellRetry)
	for _, name := range shellAttempts {
		require.Contains(t, funcs, name)
	}

	// The text outside any function is scanned like a function with no
	// allowlist row.
	scanned := map[string]string{"(outside any function)": outside}
	maps.Copy(scanned, funcs)
	for name, body := range scanned {
		isAttempt := slices.Contains(shellAttempts, name)
		if isAttempt {
			continue
		}
		if shellRequestStart.MatchString(body) {
			assert.Contains(t, shellRequestAllowlist, name,
				"%s starts a request but no allowlist row names it", name)
		}
		for _, m := range shellFileWrite.FindAllStringSubmatch(body, -1) {
			assert.Equalf(t, "/dev/null", m[1],
				"%s writes a file with curl %s; asset files are written only by the attempt functions", name, m[0])
		}
		for line := range strings.SplitSeq(body, "\n") {
			if !shellRequestStart.MatchString(line) {
				continue
			}
			for _, m := range shellRedirect.FindAllStringSubmatch(line, -1) {
				assert.Equalf(t, "/dev/null", m[1],
					"%s redirects the output of a request to %s; asset files are written only by the attempt functions", name, m[1])
			}
		}
	}
	for name := range shellRequestAllowlist {
		assert.Contains(t, funcs, name, "an allowlist row names a function install.sh no longer has")
	}

	// The attempt functions are called only from the retry function.
	for _, a := range shellAttempts {
		call := regexp.MustCompile(`\b` + a + `\b`)
		assert.Regexp(t, call, funcs[shellRetry], "%s must call %s", shellRetry, a)
		for name, body := range scanned {
			if name == shellRetry || name == a {
				continue
			}
			assert.NotRegexp(t, call, body, "%s calls %s; only %s may", name, a, shellRetry)
		}
	}
}

// powerShellAttempts are the install.ps1 functions that make one download
// attempt and write the asset file. They are called only from powerShellRetry.
var powerShellAttempts = []string{"Get-ReleaseAsset", "Get-DirectAsset"}

const powerShellRetry = "Save-Asset"

// powerShellRequestAllowlist names every install.ps1 function that may call
// Invoke-Http without writing a file, with the reason.
var powerShellRequestAllowlist = map[string]string{
	"Resolve-Tag":      "resolution request: the latest release redirect",
	"Resolve-PreTag":   "resolution request: the release listing",
	"Resolve-AssetIds": "resolution request: the release metadata",
}

// powerShellFileWrite matches the ways a PowerShell function writes a file.
var powerShellFileWrite = regexp.MustCompile(`-OutFile\b|Set-Content|Out-File|Add-Content|WriteAll|\[IO\.File\]::(?:Create|Open|Write)`)

// powerShellHTTPClients are the functions that may use an HTTP client
// directly: the one that builds the shared client and the one that sends a
// request with it.
var powerShellHTTPClients = []string{"Get-Http", "Invoke-Http"}

// powerShellHTTPUse matches the other ways install.ps1 could send a request
// without going through Invoke-Http: the web cmdlets and their aliases, a
// send on a client, and the construction of a client.
var powerShellHTTPUse = regexp.MustCompile(`(?i)\b(?:Invoke-WebRequest|Invoke-RestMethod|iwr|irm|SendAsync|GetStringAsync|GetByteArrayAsync|GetStreamAsync|GetAsync|PostAsync)\b|\bWebClient\b|New-Object\s+(?:System\.Net\.Http\.)?HttpClient\b|\[(?:System\.Net\.Http\.)?HttpClient\]::new`)

func TestInstallPs1AssetDownloadsAllGoThroughTheRetry(t *testing.T) {
	t.Parallel()
	funcs, outside := powerShellFunctions(t, "install.ps1")
	require.Contains(t, funcs, powerShellRetry)
	for _, name := range powerShellAttempts {
		require.Contains(t, funcs, name)
	}
	request := regexp.MustCompile(`\bInvoke-Http\b`)

	scanned := map[string]string{"(outside any phase function)": outside}
	maps.Copy(scanned, funcs)
	for name, body := range scanned {
		if slices.Contains(powerShellAttempts, name) || name == "Invoke-Http" {
			continue
		}
		if request.MatchString(body) {
			assert.Contains(t, powerShellRequestAllowlist, name,
				"%s calls Invoke-Http but no allowlist row names it", name)
			assert.NotRegexp(t, powerShellFileWrite, body,
				"%s makes a request and writes a file; asset files are written only by the attempt functions", name)
		}
	}
	for name, body := range scanned {
		if !slices.Contains(powerShellHTTPClients, name) {
			assert.NotRegexp(t, powerShellHTTPUse, body,
				"%s sends a request or builds an HTTP client; only %s may", name, strings.Join(powerShellHTTPClients, " and "))
		}
	}
	for _, name := range powerShellHTTPClients {
		assert.Contains(t, funcs, name)
		assert.Regexp(t, powerShellHTTPUse, funcs[name], "%s no longer uses an HTTP client; update powerShellHTTPClients", name)
	}
	for name := range powerShellRequestAllowlist {
		assert.Contains(t, funcs, name, "an allowlist row names a function install.ps1 no longer has")
	}
	for _, a := range powerShellAttempts {
		call := regexp.MustCompile(`\b` + regexp.QuoteMeta(a) + `\b`)
		assert.Regexp(t, call, funcs[powerShellRetry], "%s must call %s", powerShellRetry, a)
		for name, body := range scanned {
			if name == powerShellRetry || name == a {
				continue
			}
			assert.NotRegexp(t, call, body, "%s calls %s; only %s may", name, a, powerShellRetry)
		}
	}
}

package fidelity

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinLinePattern finds every pin line of scripts/e2e-fidelity: a top-level
// literal assignment of a variable named *ORACLE_IMAGE. It excludes the golden
// image copy (a reference to another variable), the indented reassignments
// inside the profile branches and the export line. The trailing \s* accepts
// trailing spaces and a CR.
var pinLinePattern = regexp.MustCompile(`(?m)^([A-Z_]*ORACLE_IMAGE)="[^"$]+"\s*$`)

var (
	sha256Suffix  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	k3sImagePin   = regexp.MustCompile(`^rancher/k3s:[A-Za-z0-9_][A-Za-z0-9_.-]*@sha256:[0-9a-f]{64}$`)
	chartVersion  = regexp.MustCompile(`rancher-([^/@]+)\.tgz(@|$)`)
	validBootName = map[string]bool{"container": true, "helm": true}
)

// scriptPinNames lists the variable names of the pin lines of a script.
func scriptPinNames(script string) []string {
	var names []string
	for _, m := range pinLinePattern.FindAllStringSubmatch(script, -1) {
		names = append(names, m[1])
	}
	return names
}

// pinPresent reports whether the script has a NAME="..." assignment line, the
// form parseScriptImagePin reads.
func pinPresent(script, name string) bool {
	return regexp.MustCompile(fmt.Sprintf(`(?m)^\s*%s\s*=\s*"[^"]*"\s*$`, regexp.QuoteMeta(name))).MatchString(script)
}

// imageRepository is the repository path of an image reference, without its
// tag and digest.
func imageRepository(image string) string {
	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name
}

// checkPins applies every rule of the pin guard to the text of a script and
// returns one message per broken rule. The rules: a1 every image pin has a
// boot line; a2 the boot is container or helm; b1 and b2 a helm pin has a
// chart archive and a k3s image of the node container; c1 and c2 a container
// pin has neither; d a rancher-server image is helm; e1 to e4 the chart, the
// k3s image and the chart version of the helm-check. Each rule reports on its
// own: a value that is missing is reported by its existence rule only, and a
// pin whose boot is missing or invalid is not checked against rules b and c.
func checkPins(t *testing.T, script string) []string {
	t.Helper()
	var broken []string
	for _, name := range scriptPinNames(script) {
		prefix := strings.TrimSuffix(name, "_IMAGE")
		bootName, chartName, k3sName := prefix+"_BOOT", prefix+"_CHART", prefix+"_LOCAL_K3S_IMAGE"
		if !pinPresent(script, bootName) {
			broken = append(broken, fmt.Sprintf("rule a1: %s has no %s line", name, bootName))
			continue
		}
		boot := parseScriptImagePin(t, script, bootName)
		if !validBootName[boot] {
			broken = append(broken, fmt.Sprintf("rule a2: %s is %q, not container or helm", bootName, boot))
		}
		chart, hasChart := "", pinPresent(script, chartName)
		if hasChart {
			chart = parseScriptImagePin(t, script, chartName)
		}
		k3s, hasK3s := "", pinPresent(script, k3sName)
		if hasK3s {
			k3s = parseScriptImagePin(t, script, k3sName)
		}
		switch boot {
		case "helm":
			if !hasChart || !sha256Suffix.MatchString(chart) {
				broken = append(broken, fmt.Sprintf("rule b1: %s is helm; %s must be an archive URL ending in @sha256: and 64 hex digits", bootName, chartName))
			}
			if !hasK3s || !k3sImagePin.MatchString(k3s) {
				broken = append(broken, fmt.Sprintf("rule b2: %s is helm; %s must be a rancher/k3s image with a tag and an @sha256: digest", bootName, k3sName))
			}
		case "container":
			if hasChart {
				broken = append(broken, fmt.Sprintf("rule c1: %s is container; it has no %s", bootName, chartName))
			}
			if hasK3s {
				broken = append(broken, fmt.Sprintf("rule c2: %s is container; it has no %s", bootName, k3sName))
			}
		}
		image := parseScriptImagePin(t, script, name)
		if strings.HasSuffix(imageRepository(image), "/rancher-server") && boot != "helm" {
			broken = append(broken, fmt.Sprintf("rule d: %s names a rancher-server image; %s must be helm", name, bootName))
		}
	}
	return append(broken, checkHelmCheckPins(t, script)...)
}

// checkHelmCheckPins applies rules e1 to e4: the pins of the helm-check.
func checkHelmCheckPins(t *testing.T, script string) []string {
	t.Helper()
	var broken []string
	chart, hasChart := "", pinPresent(script, "HELMCHECK_CHART")
	if hasChart {
		chart = parseScriptImagePin(t, script, "HELMCHECK_CHART")
	}
	if !hasChart || !sha256Suffix.MatchString(chart) {
		broken = append(broken, "rule e1: HELMCHECK_CHART must exist and end in @sha256: and 64 hex digits")
	}
	if !pinPresent(script, "HELMCHECK_LOCAL_K3S_IMAGE") {
		broken = append(broken, "rule e2: HELMCHECK_LOCAL_K3S_IMAGE must exist")
	} else if k3s := parseScriptImagePin(t, script, "HELMCHECK_LOCAL_K3S_IMAGE"); !k3sImagePin.MatchString(k3s) {
		broken = append(broken, "rule e3: HELMCHECK_LOCAL_K3S_IMAGE must be a rancher/k3s image with a tag and an @sha256: digest")
	}
	if hasChart && pinPresent(script, "ORACLE_IMAGE") {
		image := parseScriptImagePin(t, script, "ORACLE_IMAGE")
		name, _, _ := strings.Cut(image, "@")
		tag := name[strings.LastIndex(name, ":")+1:]
		m := chartVersion.FindStringSubmatch(chart)
		if m == nil || "v"+m[1] != tag {
			broken = append(broken, fmt.Sprintf("rule e4: the version in HELMCHECK_CHART must equal the tag of ORACLE_IMAGE without its v (%s)", tag))
		}
	}
	return broken
}

func realScriptText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "e2e-fidelity"))
	require.NoError(t, err)
	return string(raw)
}

// replaceLine replaces the one line of a script that starts with prefix.
func replaceLine(t *testing.T, script, prefix, line string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(prefix) + `.*$`)
	require.Len(t, re.FindAllStringIndex(script, -1), 1, "no single line starts with %s", prefix)
	return re.ReplaceAllLiteralString(script, line)
}

const (
	sampleHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sampleK3s = `rancher/k3s:v1.36.2-k3s1@sha256:` + sampleHex
)

// TestPinGuardPassesTheScript asserts that scripts/e2e-fidelity breaks no
// rule of the pin guard, and that the enumeration finds the two image pins and
// no other line (not the golden copy, the indented reassignments or the
// export).
func TestPinGuardPassesTheScript(t *testing.T) {
	script := realScriptText(t)
	assert.ElementsMatch(t, []string{"ORACLE_IMAGE", "PRIME_ORACLE_IMAGE"}, scriptPinNames(script))
	assert.Empty(t, checkPins(t, script))
}

// TestPinGuardMutations asserts that each rule fails on its own mutation of
// the script, with the message of that rule alone.
func TestPinGuardMutations(t *testing.T) {
	image := "rancher/rancher:v2.15.2@sha256:" + sampleHex
	rows := []struct {
		rule   string
		mutate func(t *testing.T, script string) string
	}{
		{"a1", func(t *testing.T, script string) string {
			t.Helper()
			return script + "\nFOO_ORACLE_IMAGE=\"" + image + "\"\n"
		}},
		{"a2", func(t *testing.T, script string) string {
			t.Helper()
			return replaceLine(t, script, `ORACLE_BOOT=`, `ORACLE_BOOT="docker"`)
		}},
		{"b1", func(t *testing.T, script string) string {
			t.Helper()
			script = replaceLine(t, script, `PRIME_ORACLE_BOOT=`, `PRIME_ORACLE_BOOT="helm"`)
			return script + "\nPRIME_ORACLE_LOCAL_K3S_IMAGE=\"" + sampleK3s + "\"\n"
		}},
		{"b2", func(t *testing.T, script string) string {
			t.Helper()
			script = replaceLine(t, script, `PRIME_ORACLE_BOOT=`, `PRIME_ORACLE_BOOT="helm"`)
			return script + "\nPRIME_ORACLE_CHART=\"https://charts.example.com/rancher-2.14.5.tgz@sha256:" + sampleHex + "\"\n"
		}},
		{"c1", func(t *testing.T, script string) string {
			t.Helper()
			return script + "\nORACLE_CHART=\"https://charts.example.com/rancher-2.15.2.tgz@sha256:" + sampleHex + "\"\n"
		}},
		{"c2", func(t *testing.T, script string) string {
			t.Helper()
			return script + "\nORACLE_LOCAL_K3S_IMAGE=\"" + sampleK3s + "\"\n"
		}},
		{"d", func(t *testing.T, script string) string {
			t.Helper()
			return replaceLine(t, script, `ORACLE_IMAGE=`, `ORACLE_IMAGE="rancher/rancher-server:v2.15.2@sha256:`+sampleHex+`"`)
		}},
		{"e1", func(t *testing.T, script string) string {
			t.Helper()
			return replaceLine(t, script, `HELMCHECK_CHART=`, `HELMCHECK_CHART="https://releases.rancher.com/server-charts/latest/rancher-2.15.2.tgz"`)
		}},
		{"e2", func(t *testing.T, script string) string {
			t.Helper()
			return replaceLine(t, script, `HELMCHECK_LOCAL_K3S_IMAGE=`, ``)
		}},
		{"e3", func(t *testing.T, script string) string {
			t.Helper()
			return replaceLine(t, script, `HELMCHECK_LOCAL_K3S_IMAGE=`, `HELMCHECK_LOCAL_K3S_IMAGE="rancher/k3s:v1.36.2-k3s1"`)
		}},
		{"e4", func(t *testing.T, script string) string {
			t.Helper()
			return replaceLine(t, script, `HELMCHECK_CHART=`, `HELMCHECK_CHART="https://releases.rancher.com/server-charts/latest/rancher-2.15.3.tgz@sha256:`+sampleHex+`"`)
		}},
	}
	script := realScriptText(t)
	for _, row := range rows {
		t.Run(row.rule, func(t *testing.T) {
			got := checkPins(t, row.mutate(t, script))
			require.Len(t, got, 1, "only rule %s fails: %v", row.rule, got)
			assert.True(t, strings.HasPrefix(got[0], "rule "+row.rule+": "), got[0])
		})
	}
}

// TestPinGuardToleratesACarriageReturn asserts that a CR at the end of a pin
// line, as a Windows checkout with CRLF line endings leaves, changes nothing:
// the enumeration still finds the pin, parseScriptImagePin reads its value
// and no rule fails.
func TestPinGuardToleratesACarriageReturn(t *testing.T) {
	script := realScriptText(t)
	want := parseScriptImagePin(t, script, "ORACLE_IMAGE")
	crlf := strings.ReplaceAll(script, "\n", "\r\n")
	assert.ElementsMatch(t, []string{"ORACLE_IMAGE", "PRIME_ORACLE_IMAGE"}, scriptPinNames(crlf))
	assert.Equal(t, want, parseScriptImagePin(t, crlf, "ORACLE_IMAGE"))
	assert.Empty(t, checkPins(t, crlf))

	line := regexp.MustCompile(`(?m)^ORACLE_IMAGE=.*$`).FindString(script)
	require.NotEmpty(t, line)
	one := strings.Replace(script, line, line+"\r", 1)
	assert.ElementsMatch(t, []string{"ORACLE_IMAGE", "PRIME_ORACLE_IMAGE"}, scriptPinNames(one))
	assert.Equal(t, want, parseScriptImagePin(t, one, "ORACLE_IMAGE"))
	assert.Empty(t, checkPins(t, one))
}

// bootstrapPasswordForms are the only lines of scripts/e2e-fidelity that may
// name BOOTSTRAP_PW: its generation, the environment file of the container
// boot, the secret manifest here-document and the login body here-document.
// The password reaches no argument of an external command.
var bootstrapPasswordForms = []*regexp.Regexp{
	regexp.MustCompile(`^\s*BOOTSTRAP_PW="\$\(head -c 24 /dev/urandom \| base64 \| tr -dc 'a-zA-Z0-9' \| head -c 20\)"$`),
	regexp.MustCompile(`^\s*\( umask 177; printf 'CATTLE_BOOTSTRAP_PASSWORD=%s\\n' "\$\{BOOTSTRAP_PW\}" >"\$\{ENV_FILE\}" \)$`),
	regexp.MustCompile(`^\s*bootstrapPassword: "\$\{BOOTSTRAP_PW\}"$`),
	regexp.MustCompile(`^\{"username":"admin","password":"\$\{BOOTSTRAP_PW\}"\}$`),
}

// unlistedPasswordLines returns the lines of a script that name BOOTSTRAP_PW
// in a form that bootstrapPasswordForms does not list. Comment lines are not
// code and may name the variable.
func unlistedPasswordLines(script string) []string {
	var lines []string
	for line := range strings.SplitSeq(script, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.Contains(line, "BOOTSTRAP_PW") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		listed := false
		for _, form := range bootstrapPasswordForms {
			listed = listed || form.MatchString(line)
		}
		if !listed {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestBootstrapPasswordForms asserts that every line of scripts/e2e-fidelity
// that names BOOTSTRAP_PW is one of the listed forms, and that a line that
// passes the password as an argument of docker exec or of python3 is not.
func TestBootstrapPasswordForms(t *testing.T) {
	script := realScriptText(t)
	assert.Empty(t, unlistedPasswordLines(script))
	assert.Positive(t, strings.Count(script, "BOOTSTRAP_PW"))

	for name, line := range map[string]string{
		"docker exec -e": `  docker exec -e "PW=${BOOTSTRAP_PW}" -i "${CONTAINER}" kubectl apply -f -`,
		"python3 argv":   `  python3 -I -c 'print(1)' "${BOOTSTRAP_PW}"`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, []string{line}, unlistedPasswordLines(script+"\n"+line+"\n"))
		})
	}
}

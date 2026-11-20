package conformance

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sbomFixtureTag         = "v0.1.0-beta.3"
	sbomFixtureSpdxVersion = "2.3"
	sbomFixtureSpdx        = "https://spdx.dev/Document/v" + sbomFixtureSpdxVersion
	sbomFixtureSlsa        = "https://slsa.dev/provenance/v1"
	sbomFixtureCLI         = "rancher-linux-amd64"
)

// sbomFixtureBinaries are the binaries of the fixtures: one with a plain
// name, a credential helper and one with .exe in the middle of its asset
// names.
var sbomFixtureBinaries = []string{"rancher-linux-amd64", "rancher-credential-vault-linux-amd64", "rancher-windows-amd64.exe"}

func sbomDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func sbomBinaryContent(name string) string { return "binary " + name + "\n" }

// requireSbomTools skips the test when jq is missing, and fails it when CI is
// set, as the other tests of release scripts do.
func requireSbomTools(t *testing.T) {
	t.Helper()
	testfs.SkipOnWindows(t, posixOnlyReason)
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI is set: a missing jq fails instead of skipping: %v", err)
		}
		t.Skipf("jq not installed: %v", err)
	}
}

// sbomDocument returns the SBOM Syft writes for a binary, after the rewrite
// of release-sbom: SPDX 2.3, the namespace of the asset in the release, the
// SHA-256 in the root package and in the entry for the file.
func sbomDocument(name, tag string, recorded ...string) map[string]any {
	checksums := func(value string) []any {
		return []any{map[string]any{"algorithm": "SHA1", "checksumValue": "1"}, map[string]any{"algorithm": "SHA256", "checksumValue": value}}
	}
	if len(recorded) == 0 {
		d := sbomDigest(sbomBinaryContent(name))
		recorded = []string{d, d}
	}
	return map[string]any{
		"spdxVersion":       "SPDX-" + sbomFixtureSpdxVersion,
		"name":              name,
		"documentNamespace": "https://github.com/rancher/rancher-cli/releases/download/" + tag + "/" + name + ".spdx.json",
		"creationInfo":      map[string]any{"created": "2023-11-14T22:13:20Z"},
		"packages": []any{
			map[string]any{"name": "example.com/module", "checksums": checksums("ffff")},
			map[string]any{"name": name, "checksums": checksums(recorded[0])},
		},
		"files": []any{map[string]any{"fileName": name, "checksums": checksums(recorded[1])}},
	}
}

func sbomMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return append(data, '\n')
}

// sbomBundle returns a Sigstore bundle that holds the in-toto statement. It
// carries no signature: the check reads the statement and verifies nothing.
func sbomBundle(t *testing.T, statement map[string]any) []byte {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString(sbomMarshal(t, statement))
	return sbomMarshal(t, map[string]any{
		"mediaType":    "application/vnd.dev.sigstore.bundle.v0.3+json",
		"dsseEnvelope": map[string]any{"payload": payload, "payloadType": "application/vnd.in-toto+json", "signatures": []any{}},
	})
}

func sbomStatement(predicateType string, subjects []map[string]any, predicate any) map[string]any {
	list := make([]any, len(subjects))
	for i, s := range subjects {
		list[i] = s
	}
	out := map[string]any{"_type": "https://in-toto.io/Statement/v1", "subject": list, "predicateType": predicateType}
	if predicate != nil {
		out["predicate"] = predicate
	}
	return out
}

func sbomSubject(name, content string) map[string]any {
	return map[string]any{"name": name, "digest": map[string]any{"sha256": sbomDigest(content)}}
}

// writeFixtureFile writes a file of a fixture directory.
func writeFixtureFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
}

// writeReleaseDist writes the files release-build leaves in a directory:
// the binaries, the schema, the installers and SHA256SUMS.
func writeReleaseDist(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"rancher-schema.json": "{}\n",
		"install.sh":          "#!/bin/sh\n",
		"install.ps1":         "# ps1\n",
	}
	for _, b := range sbomFixtureBinaries {
		files[b] = sbomBinaryContent(b)
	}
	var sums strings.Builder
	for _, name := range slices.Sorted(maps.Keys(files)) {
		writeFixtureFile(t, dir, name, []byte(files[name]))
		fmt.Fprintf(&sums, "%s  %s\n", sbomDigest(files[name]), name)
	}
	writeFixtureFile(t, dir, "SHA256SUMS", []byte(sums.String()))
}

// writeReleaseAssets writes the complete set of release assets of the
// fixture tag into dir: what release-assets-check accepts.
func writeReleaseAssets(t *testing.T, dir string) {
	t.Helper()
	writeReleaseDist(t, dir)
	provenance := []map[string]any{}
	for _, name := range []string{"install.ps1", "install.sh", "rancher-schema.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		provenance = append(provenance, sbomSubject(name, string(data)))
	}
	for _, b := range sbomFixtureBinaries {
		doc := sbomDocument(b, sbomFixtureTag)
		docBytes := sbomMarshal(t, doc)
		writeFixtureFile(t, dir, b+".spdx.json", docBytes)
		writeFixtureFile(t, dir, b+".sbom.sigstore.json", sbomBundle(t, sbomStatement(sbomFixtureSpdx, []map[string]any{sbomSubject(b, sbomBinaryContent(b))}, doc)))
		provenance = append(provenance, sbomSubject(b, sbomBinaryContent(b)), sbomSubject(b+".spdx.json", string(docBytes)))
	}
	writeFixtureFile(t, dir, "provenance.sigstore.json", sbomBundle(t, sbomStatement(sbomFixtureSlsa, provenance, map[string]any{"buildDefinition": map[string]any{}})))
}

// runScriptCopy runs a copy of a repository script through bash and returns
// the exit code, standard output and standard error.
func runScriptCopy(t *testing.T, script, dir string, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	root := t.TempDir()
	copyRepositoryFile(t, script, root)
	copyRepositoryFile(t, "scripts/check-release-tag", root)
	cmd := scriptCommand(t.Context(), filepath.Join(root, script), args...)
	cmd.Dir = dir
	cmd.Env = append(gitFreeEnv(), env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	return exitCodeOf(t, cmd.Run()), out.String(), errOut.String()
}

func editFile(t *testing.T, path string, change func([]byte) []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, change(data), 0o644))
}

// editJSONFile rewrites a JSON file after change edits its decoded object.
func editJSONFile(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	editFile(t, path, func(data []byte) []byte {
		var v map[string]any
		require.NoError(t, json.Unmarshal(data, &v))
		change(v)
		return sbomMarshal(t, v)
	})
}

// editStatement rewrites the in-toto statement inside a Sigstore bundle.
func editStatement(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	editJSONFile(t, path, func(bundle map[string]any) {
		envelope := bundle["dsseEnvelope"].(map[string]any)
		raw, err := base64.StdEncoding.DecodeString(envelope["payload"].(string))
		require.NoError(t, err)
		var statement map[string]any
		require.NoError(t, json.Unmarshal(raw, &statement))
		change(statement)
		envelope["payload"] = base64.StdEncoding.EncodeToString(sbomMarshal(t, statement))
	})
}

// TestReleaseAssetsCheck asserts release-assets-check passes a complete set of
// assets and fails each defect with its own message: a missing or extra
// file, an SBOM or a Sigstore bundle in SHA256SUMS, a wrong digest, an SBOM
// that records no SHA-256 or another one for its binary, a namespace of
// another asset, and bundles whose statements do not match the files.
func TestReleaseAssetsCheck(t *testing.T) {
	t.Parallel()
	requireSbomTools(t)
	const b = sbomFixtureCLI
	good := sbomDigest(sbomBinaryContent(b))
	bad := sbomDigest("another")
	for _, c := range []struct {
		name   string
		change func(t *testing.T, dir string)
		want   string
	}{
		{"a complete directory", func(*testing.T, string) {}, ""},
		{"a missing SBOM", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, b+".spdx.json")))
		}, "missing SBOM " + b + ".spdx.json"},
		{"a missing SBOM bundle", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, b+".sbom.sigstore.json")))
		}, "missing Sigstore bundle " + b + ".sbom.sigstore.json"},
		{"a missing provenance bundle", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, "provenance.sigstore.json")))
		}, "missing Sigstore bundle provenance.sigstore.json"},
		{"a missing installer", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, "install.sh")))
		}, "missing file install.sh"},
		{"an extra file", func(t *testing.T, dir string) {
			t.Helper()
			writeFixtureFile(t, dir, "extra.txt", []byte("x"))
		}, "unexpected file 'extra.txt'"},
		{"an SBOM of no binary", func(t *testing.T, dir string) {
			t.Helper()
			writeFixtureFile(t, dir, "rancher-other.spdx.json", []byte("{}"))
		}, "unexpected file 'rancher-other.spdx.json'"},
		{"an SBOM in SHA256SUMS", func(t *testing.T, dir string) {
			t.Helper()
			editFile(t, filepath.Join(dir, "SHA256SUMS"), func(d []byte) []byte { return append(d, []byte("abc  "+b+".spdx.json\n")...) })
		}, "SHA256SUMS lists the SBOM " + b + ".spdx.json"},
		{"a Sigstore bundle in SHA256SUMS", func(t *testing.T, dir string) {
			t.Helper()
			editFile(t, filepath.Join(dir, "SHA256SUMS"), func(d []byte) []byte { return append(d, []byte("abc  provenance.sigstore.json\n")...) })
		}, "SHA256SUMS lists the Sigstore bundle provenance.sigstore.json"},
		{"a wrong digest in a sums line", func(t *testing.T, dir string) {
			t.Helper()
			editFile(t, filepath.Join(dir, "SHA256SUMS"), func(d []byte) []byte {
				return []byte(strings.Replace(string(d), good+"  "+b, bad+"  "+b, 1))
			})
		}, "SHA256SUMS has the wrong SHA-256 for " + b},
		{"a file missing from SHA256SUMS", func(t *testing.T, dir string) {
			t.Helper()
			editFile(t, filepath.Join(dir, "SHA256SUMS"), func(d []byte) []byte {
				var keep []string
				for line := range strings.SplitSeq(strings.TrimSpace(string(d)), "\n") {
					if !strings.HasSuffix(line, "  install.sh") {
						keep = append(keep, line)
					}
				}
				return []byte(strings.Join(keep, "\n") + "\n")
			})
		}, "SHA256SUMS does not list install.sh"},
		{"a file twice in SHA256SUMS", func(t *testing.T, dir string) {
			t.Helper()
			editFile(t, filepath.Join(dir, "SHA256SUMS"), func(d []byte) []byte { return append(d, []byte(good+"  "+b+"\n")...) })
		}, "SHA256SUMS lists " + b + " more than once"},
		{"an SBOM with no SHA-256 for its binary", func(t *testing.T, dir string) {
			t.Helper()
			editJSONFile(t, filepath.Join(dir, b+".spdx.json"), func(v map[string]any) {
				v["packages"] = []any{}
				v["files"] = []any{}
			})
		}, b + ".spdx.json records no SHA-256 for " + b},
		{"an SBOM that records another SHA-256", func(t *testing.T, dir string) {
			t.Helper()
			writeFixtureFile(t, dir, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag, bad, bad)))
		}, `records the SHA-256 ["` + bad + `","` + bad + `"] for ` + b},
		{"an SBOM whose two recorded values differ", func(t *testing.T, dir string) {
			t.Helper()
			writeFixtureFile(t, dir, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag, good, bad)))
		}, `records the SHA-256 ["` + good + `","` + bad + `"] for ` + b},
		{"a wrong documentNamespace", func(t *testing.T, dir string) {
			t.Helper()
			editJSONFile(t, filepath.Join(dir, b+".spdx.json"), func(v map[string]any) { v["documentNamespace"] = "https://anchore.com/syft/file/x" })
		}, b + ".spdx.json has documentNamespace 'https://anchore.com/syft/file/x'"},
		{"another SPDX version", func(t *testing.T, dir string) {
			t.Helper()
			editJSONFile(t, filepath.Join(dir, b+".spdx.json"), func(v map[string]any) { v["spdxVersion"] = "SPDX-" + "2.2" })
		}, b + ".spdx.json has spdxVersion 'SPDX-" + "2.2'"},
		{"an SBOM bundle whose subject digest differs", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, b+".sbom.sigstore.json"), func(s map[string]any) {
				s["subject"].([]any)[0].(map[string]any)["digest"] = map[string]any{"sha256": bad}
			})
		}, b + ".sbom.sigstore.json has the subjects '" + b + " " + bad + "'"},
		{"an SBOM bundle with two subjects", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, b+".sbom.sigstore.json"), func(s map[string]any) {
				s["subject"] = append(s["subject"].([]any), sbomSubject("rancher-windows-amd64.exe", sbomBinaryContent("rancher-windows-amd64.exe")))
			})
		}, b + ".sbom.sigstore.json has the subjects"},
		{"an SBOM bundle with another predicate type", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, b+".sbom.sigstore.json"), func(s map[string]any) { s["predicateType"] = "https://spdx.dev/Document" })
		}, b + ".sbom.sigstore.json has predicate type 'https://spdx.dev/Document'"},
		{"an SBOM bundle whose predicate differs", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, b+".sbom.sigstore.json"), func(s map[string]any) {
				s["predicate"].(map[string]any)["name"] = "another"
			})
		}, "the predicate of " + b + ".sbom.sigstore.json differs from " + b + ".spdx.json"},
		{"a provenance bundle missing one subject", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, "provenance.sigstore.json"), func(s map[string]any) {
				var keep []any
				for _, subject := range s["subject"].([]any) {
					if subject.(map[string]any)["name"] != "install.sh" {
						keep = append(keep, subject)
					}
				}
				s["subject"] = keep
			})
		}, "provenance.sigstore.json has no subject 'install.sh " + sbomDigest("#!/bin/sh\n") + "'"},
		{"a provenance bundle with a subject of another digest", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, "provenance.sigstore.json"), func(s map[string]any) {
				s["subject"].([]any)[0].(map[string]any)["digest"] = map[string]any{"sha256": bad}
			})
		}, "provenance.sigstore.json has the subject '"},
		{"a provenance bundle of another predicate type", func(t *testing.T, dir string) {
			t.Helper()
			editStatement(t, filepath.Join(dir, "provenance.sigstore.json"), func(s map[string]any) { s["predicateType"] = "https://slsa.dev/provenance/v0.2" })
		}, "provenance.sigstore.json has predicate type 'https://slsa.dev/provenance/v0.2'"},
		{"a provenance bundle with two JSON values", func(t *testing.T, dir string) {
			t.Helper()
			editFile(t, filepath.Join(dir, "provenance.sigstore.json"), func(d []byte) []byte { return append(d, d...) })
		}, "provenance.sigstore.json holds 2 JSON values"},
		{"a bundle that is not a Sigstore bundle", func(t *testing.T, dir string) {
			t.Helper()
			writeFixtureFile(t, dir, b+".sbom.sigstore.json", []byte("{\"mediaType\":\"x\"}\n"))
		}, b + ".sbom.sigstore.json is not a Sigstore bundle"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeReleaseAssets(t, dir)
			c.change(t, dir)
			code, stdout, stderr := runScriptCopy(t, assetsCheckScript, "", nil, dir, sbomFixtureTag)
			if c.want == "" {
				assert.Zero(t, code, stderr)
				assert.Empty(t, stderr)
				assert.Contains(t, stdout, "release-assets-check:")
				return
			}
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, c.want)
			assert.Empty(t, stdout)
		})
	}
}

// TestReleaseAssetsCheckArguments asserts the usage and the refusal of a
// directory that does not exist or a tag that is empty.
func TestReleaseAssetsCheckArguments(t *testing.T) {
	t.Parallel()
	requireSbomTools(t)
	code, _, stderr := runScriptCopy(t, assetsCheckScript, "", nil)
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "usage: release-assets-check")
	code, _, stderr = runScriptCopy(t, assetsCheckScript, "", nil, filepath.Join(t.TempDir(), "missing"), sbomFixtureTag)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "is not a directory")
	code, _, stderr = runScriptCopy(t, assetsCheckScript, "", nil, t.TempDir(), "")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "the tag is empty")
}

// TestReleaseAssetsCheckNeedsJq asserts that with no jq on PATH the script
// prints one plain error line that names jq.
func TestReleaseAssetsCheckNeedsJq(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	bin := t.TempDir()
	for _, name := range []string{"bash", "sh", "dirname"} {
		target, err := exec.LookPath(name)
		require.NoError(t, err)
		testfs.Symlink(t, target, filepath.Join(bin, name))
	}
	code, _, stderr := runScriptCopy(t, assetsCheckScript, "", []string{"PATH=" + bin}, t.TempDir(), sbomFixtureTag)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "jq is not installed")
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "one line, got %q", stderr)
}

// writeSbomCheckInputs writes a dist directory and the directory of the SBOMs
// that match it.
func writeSbomCheckInputs(t *testing.T) (dist, sbom string) {
	t.Helper()
	dist, sbom = t.TempDir(), t.TempDir()
	writeReleaseDist(t, dist)
	for _, b := range sbomFixtureBinaries {
		writeFixtureFile(t, sbom, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag)))
	}
	return dist, sbom
}

// TestReleaseSbomCheck asserts release-sbom-check passes SBOMs that record the
// digests SHA256SUMS lists and refuses, with its own message, each other
// case: a binary with no SBOM, a file that names no binary, an SBOM with an
// empty, missing, or different SHA-256, two recorded values that differ, a
// binary with no line in SHA256SUMS, and both an empty value and no line.
func TestReleaseSbomCheck(t *testing.T) {
	t.Parallel()
	requireSbomTools(t)
	const b = sbomFixtureCLI
	good := sbomDigest(sbomBinaryContent(b))
	bad := sbomDigest("another")
	for _, c := range []struct {
		name   string
		change func(t *testing.T, dist, sbom string)
		want   string
	}{
		{"a complete pair", func(*testing.T, string, string) {}, ""},
		{"an SBOM missing for a binary", func(t *testing.T, _, sbom string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(sbom, b+".spdx.json")))
		}, "no SBOM " + b + ".spdx.json for the binary " + b},
		{"a file that names no binary", func(t *testing.T, _, sbom string) {
			t.Helper()
			writeFixtureFile(t, sbom, "x.txt", []byte("x"))
		}, "'x.txt' in the SBOM directory does not match any binary in"},
		{"an SBOM of no binary", func(t *testing.T, _, sbom string) {
			t.Helper()
			writeFixtureFile(t, sbom, "rancher-other.spdx.json", []byte("{}"))
		}, "'rancher-other.spdx.json' in the SBOM directory does not match any binary in"},
		{"an SBOM that records an empty SHA-256", func(t *testing.T, _, sbom string) {
			t.Helper()
			writeFixtureFile(t, sbom, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag, "", "")))
		}, b + ".spdx.json records an empty SHA-256 for " + b},
		{"a binary with no SHA256SUMS line", func(t *testing.T, dist, _ string) {
			t.Helper()
			editFile(t, filepath.Join(dist, "SHA256SUMS"), func(d []byte) []byte {
				return []byte(strings.Replace(string(d), good+"  "+b+"\n", "", 1))
			})
		}, "SHA256SUMS has no line for the binary " + b},
		{"a recorded SHA-256 that differs from the line", func(t *testing.T, _, sbom string) {
			t.Helper()
			writeFixtureFile(t, sbom, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag, bad, bad)))
		}, `records the SHA-256 ["` + bad + `","` + bad + `"] for ` + b + "; SHA256SUMS lists " + good},
		{"an SBOM that records no SHA-256", func(t *testing.T, _, sbom string) {
			t.Helper()
			editJSONFile(t, filepath.Join(sbom, b+".spdx.json"), func(v map[string]any) {
				v["packages"] = []any{}
				v["files"] = []any{}
			})
		}, b + ".spdx.json records no SHA-256 for " + b},
		{"two recorded values that differ", func(t *testing.T, _, sbom string) {
			t.Helper()
			writeFixtureFile(t, sbom, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag, good, bad)))
		}, `records the SHA-256 ["` + good + `","` + bad + `"] for ` + b},
		{"the only value is the one of the files entry and it differs", func(t *testing.T, _, sbom string) {
			t.Helper()
			editJSONFile(t, filepath.Join(sbom, b+".spdx.json"), func(v map[string]any) {
				v["packages"] = []any{}
				v["files"].([]any)[0].(map[string]any)["checksums"] = []any{map[string]any{"algorithm": "SHA256", "checksumValue": bad}}
			})
		}, `records the SHA-256 ["` + bad + `"] for ` + b},
		{"an empty value and no line", func(t *testing.T, dist, sbom string) {
			t.Helper()
			writeFixtureFile(t, sbom, b+".spdx.json", sbomMarshal(t, sbomDocument(b, sbomFixtureTag, "", "")))
			editFile(t, filepath.Join(dist, "SHA256SUMS"), func(d []byte) []byte {
				return []byte(strings.Replace(string(d), good+"  "+b+"\n", "", 1))
			})
		}, b + ".spdx.json records an empty SHA-256 for " + b},
		{"a line twice", func(t *testing.T, dist, _ string) {
			t.Helper()
			editFile(t, filepath.Join(dist, "SHA256SUMS"), func(d []byte) []byte { return append(d, []byte(good+"  "+b+"\n")...) })
		}, "SHA256SUMS has 2 lines for the binary " + b},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dist, sbom := writeSbomCheckInputs(t)
			c.change(t, dist, sbom)
			code, _, stderr := runScriptCopy(t, sbomCheckScript, "", nil, dist, sbom)
			if c.want == "" {
				assert.Zero(t, code, stderr)
				assert.Empty(t, stderr)
				return
			}
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, c.want)
		})
	}
}

// TestReleaseSbomCheckLooksUpTheWholeName asserts that the SHA256SUMS line of
// a binary is found by its whole name: a line whose name differs from it in
// the character that a regular expression would match for the dot, and a line
// whose name only contains it, come before the right line and are skipped.
func TestReleaseSbomCheckLooksUpTheWholeName(t *testing.T) {
	t.Parallel()
	requireSbomTools(t)
	dist, sbom := t.TempDir(), t.TempDir()
	const exe = "rancher-windows-amd64.exe"
	writeFixtureFile(t, dist, exe, []byte(sbomBinaryContent(exe)))
	writeFixtureFile(t, sbom, exe+".spdx.json", sbomMarshal(t, sbomDocument(exe, sbomFixtureTag)))
	sums := fmt.Sprintf("%s  rancher-windows-amd64Xexe\n%s  prefix-rancher-windows-amd64.exe\n%s  %s\n",
		sbomDigest("one"), sbomDigest("two"), sbomDigest(sbomBinaryContent(exe)), exe)
	writeFixtureFile(t, dist, "SHA256SUMS", []byte(sums))
	code, _, stderr := runScriptCopy(t, sbomCheckScript, "", nil, dist, sbom)
	assert.Zero(t, code, stderr)
	assert.Empty(t, stderr)
}

// sbomRepo is a temporary git repository with the script release-sbom, its
// helper script, a dist directory and one commit at a fixed time.
type sbomRepo struct {
	root, stub, work string
	env              []string
}

const sbomCommitTime = "1700000000"

// The stub replaces Syft. It writes a document with a time that changes on
// every call, a namespace of random text and the SHA-256 of the file it was
// given, where Syft records it, and it records its arguments and its working
// directory.
const sbomStubSyft = `#!/bin/sh
printf '%s\n' "$PWD" > "$STUB_DIR/cwd"
printf '%s\n' "$*" >> "$STUB_DIR/args"
file="${2#file:}"
out="${4#spdx-json@2.3=}"
n=$(cat "$STUB_DIR/count" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "$STUB_DIR/count"
if command -v sha256sum >/dev/null 2>&1; then
  digest=$(sha256sum "$file" | awk '{ print $1 }')
else
  digest=$(shasum -a 256 "$file" | awk '{ print $1 }')
fi
if [ -n "${STUB_NO_SHA:-}" ]; then digest=; fi
jq -n --arg version 2.3 --arg name "$file" --arg digest "$digest" --arg ns "https://anchore.com/syft/file/$file-$$-$n" --arg created "2020-01-01T00:00:0${n}Z" '
  {spdxVersion: ("SPDX-" + $version), name: $name, documentNamespace: $ns, creationInfo: {created: $created},
   packages: [{name: $name, checksums: (if $digest == "" then [] else [{algorithm: "SHA256", checksumValue: $digest}] end)}],
   files: [{fileName: $name, checksums: (if $digest == "" then [] else [{algorithm: "SHA256", checksumValue: $digest}] end)}]}
  | if env.STUB_NO_NAMESPACE != null then del(.documentNamespace) else . end' > "$out"
`

// gitFreeEnv returns the environment of this process without the variables git
// sets for a hook or reads from the caller (GIT_DIR, GIT_INDEX_FILE and the
// others). A test that runs from a hook inherits them, and they would send the
// git commands of a temporary repository to the repository being committed.
func gitFreeEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

func newSbomRepo(t *testing.T) *sbomRepo {
	t.Helper()
	requireSbomTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not installed: %v", err)
	}
	r := &sbomRepo{root: t.TempDir(), stub: filepath.Join(t.TempDir(), "syft"), work: t.TempDir()}
	r.env = []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_AUTHOR_DATE=" + sbomCommitTime + " +0000", "GIT_COMMITTER_DATE=" + sbomCommitTime + " +0000",
		"STUB_DIR=" + r.work,
	}
	for _, name := range []string{"scripts/release-sbom", "scripts/check-release-tag"} {
		copyRepositoryFile(t, name, r.root)
	}
	testfs.WriteExecutable(t, r.stub, []byte(sbomStubSyft))
	for _, b := range sbomFixtureBinaries {
		writeFixtureFile(t, r.root, "dist/"+b, []byte(sbomBinaryContent(b)))
	}
	writeFixtureFile(t, r.root, "dist/rancher-schema.json", []byte("{}\n"))
	for _, args := range [][]string{{"init", "-q", "."}, {"add", "-A"}, {"commit", "-q", "-m", "x"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = r.root
		cmd.Env = append(gitFreeEnv(), r.env...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	return r
}

func (r *sbomRepo) run(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := scriptCommand(t.Context(), filepath.Join(r.root, "scripts/release-sbom"), args...)
	cmd.Dir = r.root
	cmd.Env = append(append(gitFreeEnv(), r.env...), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	return exitCodeOf(t, cmd.Run()), stderr.String()
}

func (r *sbomRepo) sboms(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.root, "sbom"))
	require.NoError(t, err)
	out := map[string][]byte{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(r.root, "sbom", e.Name()))
		require.NoError(t, err)
		out[e.Name()] = data
	}
	return out
}

// TestReleaseSbomIsReproducible asserts release-sbom, run with a Syft that
// writes the time of its run and a random namespace, gives byte-identical
// files in two runs; that the creation time is the commit time and the
// namespace is the download URL of the asset; that it writes one SBOM for each
// binary and nothing else; and that Syft ran in dist with the file's relative
// name, the source name, the source version and the SPDX 2.3 format.
func TestReleaseSbomIsReproducible(t *testing.T) {
	t.Parallel()
	r := newSbomRepo(t)
	env := []string{"SYFT=" + r.stub}
	code, stderr := r.run(t, env, sbomFixtureTag)
	require.Zero(t, code, stderr)
	first := r.sboms(t)
	code, stderr = r.run(t, env, sbomFixtureTag)
	require.Zero(t, code, stderr)
	second := r.sboms(t)
	assert.Equal(t, first, second)

	var names []string
	for name := range first {
		names = append(names, name)
	}
	slices.Sort(names)
	assert.Equal(t, []string{
		"rancher-credential-vault-linux-amd64.spdx.json",
		"rancher-linux-amd64.spdx.json",
		"rancher-windows-amd64.exe.spdx.json",
	}, names)
	for _, b := range sbomFixtureBinaries {
		var doc struct {
			DocumentNamespace string `json:"documentNamespace"`
			CreationInfo      struct {
				Created string `json:"created"`
			} `json:"creationInfo"`
		}
		require.NoError(t, json.Unmarshal(first[b+".spdx.json"], &doc), b)
		assert.Equal(t, "2023-11-14T22:13:20Z", doc.CreationInfo.Created, b)
		assert.Equal(t, "https://github.com/rancher/rancher-cli/releases/download/"+sbomFixtureTag+"/"+b+".spdx.json", doc.DocumentNamespace, b)
	}

	args, err := os.ReadFile(filepath.Join(r.work, "args"))
	require.NoError(t, err)
	for _, b := range sbomFixtureBinaries {
		assert.Contains(t, string(args), "scan file:"+b+" -o spdx-json@2.3=", b)
		assert.Contains(t, string(args), "--source-name "+b+" --source-version "+sbomFixtureTag, b)
	}
	cwd, err := os.ReadFile(filepath.Join(r.work, "cwd"))
	require.NoError(t, err)
	wantCwd, err := filepath.EvalSymlinks(filepath.Join(r.root, "dist"))
	require.NoError(t, err)
	gotCwd, err := filepath.EvalSymlinks(strings.TrimSpace(string(cwd)))
	require.NoError(t, err)
	assert.Equal(t, wantCwd, gotCwd, "Syft runs in dist")
}

// TestReleaseSbomRefusals asserts release-sbom refuses a dist directory with
// no binary, a Syft output without the SHA-256 of the binary or without a
// namespace, a missing or relative SYFT, a missing dist and a bad tag, each
// with one plain error line.
func TestReleaseSbomRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		setup  func(t *testing.T, r *sbomRepo)
		env    func(r *sbomRepo) []string
		tag    string
		want   string
		status int
	}{
		{"no binary in dist", func(t *testing.T, r *sbomRepo) {
			t.Helper()
			entries, err := os.ReadDir(filepath.Join(r.root, "dist"))
			require.NoError(t, err)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "rancher-") && !strings.HasSuffix(e.Name(), ".json") {
					require.NoError(t, os.Remove(filepath.Join(r.root, "dist", e.Name())))
				}
			}
		}, nil, sbomFixtureTag, "holds no binary", 1},
		{"a Syft output without the SHA-256", nil, func(r *sbomRepo) []string { return []string{"SYFT=" + r.stub, "STUB_NO_SHA=1"} }, sbomFixtureTag, "does not record the SHA-256", 1},
		{"a Syft output without a namespace", nil, func(r *sbomRepo) []string { return []string{"SYFT=" + r.stub, "STUB_NO_NAMESPACE=1"} }, sbomFixtureTag, "no creationInfo.created or no documentNamespace", 1},
		{"SYFT unset", nil, func(*sbomRepo) []string { return []string{"SYFT="} }, sbomFixtureTag, "SYFT must be the absolute path", 1},
		{"SYFT relative", nil, func(*sbomRepo) []string { return []string{"SYFT=syft"} }, sbomFixtureTag, "SYFT must be the absolute path", 1},
		{"SYFT not executable", nil, func(r *sbomRepo) []string {
			return []string{"SYFT=" + filepath.Join(r.root, "dist", "rancher-schema.json")}
		}, sbomFixtureTag, "is not an executable file", 1},
		{"no dist", func(t *testing.T, r *sbomRepo) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(r.root, "dist")))
		}, nil, sbomFixtureTag, "dist/ does not exist", 1},
		{"a tag that is not a release tag", nil, nil, "main", "is not a valid release tag", 1},
		{"no tag", nil, nil, "", "usage: release-sbom", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newSbomRepo(t)
			if c.setup != nil {
				c.setup(t, r)
			}
			env := []string{"SYFT=" + r.stub}
			if c.env != nil {
				env = c.env(r)
			}
			args := []string{c.tag}
			if c.tag == "" {
				args = nil
			}
			code, stderr := r.run(t, env, args...)
			assert.Equal(t, c.status, code, stderr)
			assert.Contains(t, stderr, c.want)
			assert.Equal(t, 1, strings.Count(stderr, "\n"), "one line, got %q", stderr)
		})
	}
}

// TestReleaseBundles asserts release-bundles copies each bundle to dist under
// its asset name, and refuses a file that holds no JSON value or two, a file
// that is missing, a name that exists, a name with a slash, a line that is not
// a pair and an empty list.
func TestReleaseBundles(t *testing.T) {
	t.Parallel()
	requireSbomTools(t)
	bundle := `{"mediaType":"x","dsseEnvelope":{}}` + "\n"
	for _, c := range []struct {
		name    string
		files   map[string]string
		bundles string
		want    string
	}{
		{"two bundles", map[string]string{"a.json": bundle, "b.json": bundle}, "provenance.sigstore.json @A@\nrancher-linux-amd64.sbom.sigstore.json @B@\n", ""},
		{"a one-line JSON Lines file", map[string]string{"a.json": bundle, "b.json": bundle}, "x.sigstore.json @A@\n", ""},
		{"two JSON values in a file", map[string]string{"a.json": bundle + bundle}, "x.sigstore.json @A@\n", "holds 2 JSON values"},
		{"no JSON value in a file", map[string]string{"a.json": ""}, "x.sigstore.json @A@\n", "holds 0 JSON values"},
		{"a file that is not JSON", map[string]string{"a.json": "not json\n"}, "x.sigstore.json @A@\n", "is not JSON"},
		{"a missing file", nil, "x.sigstore.json @A@\n", "does not exist"},
		{"a name that exists", map[string]string{"a.json": bundle}, "x.sigstore.json @A@\nx.sigstore.json @A@\n", "dist/x.sigstore.json exists already"},
		{"a name with a slash", map[string]string{"a.json": bundle}, "sub/x.sigstore.json @A@\n", "is not a plain asset name"},
		{"a line with one word", map[string]string{"a.json": bundle}, "x.sigstore.json\n", "is not '<asset name> <bundle path>'"},
		{"a line with three words", map[string]string{"a.json": bundle}, "x.sigstore.json @A@ extra\n", "is not '<asset name> <bundle path>'"},
		{"an empty list", nil, "\n", "BUNDLES lists no bundle"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root, dir := t.TempDir(), t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "dist"), 0o755))
			for name, content := range c.files {
				writeFixtureFile(t, dir, name, []byte(content))
			}
			list := strings.ReplaceAll(strings.ReplaceAll(c.bundles, "@A@", filepath.Join(dir, "a.json")), "@B@", filepath.Join(dir, "b.json"))
			copyRepositoryFile(t, bundlesScript, root)
			cmd := scriptCommand(t.Context(), filepath.Join(root, bundlesScript))
			cmd.Dir = root
			cmd.Env = append(gitFreeEnv(), "BUNDLES="+list)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			code := exitCodeOf(t, cmd.Run())
			if c.want == "" {
				assert.Zero(t, code, stderr.String())
				for line := range strings.SplitSeq(strings.TrimSpace(list), "\n") {
					name, _, _ := strings.Cut(line, " ")
					got, err := os.ReadFile(filepath.Join(root, "dist", name))
					require.NoError(t, err)
					assert.Equal(t, bundle, string(got), name)
				}
				return
			}
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr.String(), c.want)
		})
	}
}

// TestGitFreeEnvDropsEveryGitVariable asserts the environment the tests give
// to git commands in a temporary repository holds no GIT_ variable of the
// process that runs them, so that a run from a git hook cannot reach the
// repository being committed.
func TestGitFreeEnvDropsEveryGitVariable(t *testing.T) {
	t.Setenv("GIT_DIR", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "index"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	env := gitFreeEnv()
	assert.NotEmpty(t, env)
	for _, kv := range env {
		assert.False(t, strings.HasPrefix(kv, "GIT_"), kv)
	}
}

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fromFileCommandRows names the test that asserts each command declaring
// --from-file; a command with the flag and no row fails the closed-world test
// below, and a row for a command without the flag fails it too.
var fromFileCommandRows = map[string]string{
	"create cloud-credential": "TestEveryFromFileFieldDropsLeadingByteOrderMarks",
}

// ccSecretFieldRow is one secret field of the driver table with the other
// fields the driver needs.
type ccSecretFieldRow struct {
	driver   string
	field    string
	required []string
	others   []string
}

// ccSecretFieldRows is every secret field of the driver table: the node
// driver fixtures the stub serves and the static s3 and alibaba rows.
func ccSecretFieldRows(t *testing.T) []ccSecretFieldRow {
	t.Helper()
	var drivers []cloudcredfields.Driver
	for _, row := range ccDrivers {
		d, ok := cloudcredfields.FromAnnotations(row.name, cloudcredfields.Annotations{
			Public: row.public, Private: row.private, Password: row.password, Optional: row.optional, Defaults: row.defaults})
		require.True(t, ok, row.name)
		drivers = append(drivers, d)
	}
	for _, prefix := range cloudcredfields.StaticPrefixes() {
		d, ok := cloudcredfields.Static(prefix)
		require.True(t, ok, prefix)
		drivers = append(drivers, d)
	}
	var rows []ccSecretFieldRow
	for _, d := range drivers {
		secrets := d.Names(func(f cloudcredfields.Field) bool { return f.Secret })
		required := d.Names(func(f cloudcredfields.Field) bool { return !f.Secret && f.Required })
		for _, name := range secrets {
			others := slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return s == name })
			rows = append(rows, ccSecretFieldRow{driver: d.Prefix, field: name, required: required, others: others})
		}
	}
	require.NotEmpty(t, rows)
	return rows
}

// ccFieldValue is the value the test sends for a field: a kubeconfig for the
// Harvester kubeconfig, which Rancher's check reads, a plain word otherwise.
func ccFieldValue(field string) string {
	if field == "kubeconfigContent" {
		return strings.TrimSpace(ccKube)
	}
	return "fake-bom-" + field + "-0001"
}

// ccPostedValue returns the value of a config field in the first POST body.
func ccPostedValue(t *testing.T, posts []string, driver, field string) string {
	t.Helper()
	require.Len(t, posts, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(posts[0]), &body))
	config, ok := body[driver+"credentialConfig"].(map[string]any)
	require.True(t, ok, "the body has no %scredentialConfig object", driver)
	value, _ := config[field].(string)
	return value
}

// TestEveryFromFileFieldDropsLeadingByteOrderMarks asserts, for every secret
// field of the driver table read from stdin and from a file, the value that
// reaches the request: every leading UTF-8 byte order mark is dropped, a mark
// after the first byte stays, and a value that starts with a UTF-16 mark or
// has a NUL byte is refused before any request is sent. The check reads the
// bytes of the file as they are, before the marks are dropped: a UTF-16 mark
// that follows a UTF-8 mark is part of the text. Not parallel: it
// sets the environment.
func TestEveryFromFileFieldDropsLeadingByteOrderMarks(t *testing.T) {
	restoreRawArgv(t)
	const mark = "\xef\xbb\xbf"
	rows := ccSecretFieldRows(t)
	for _, row := range rows {
		want := ccFieldValue(row.field)
		for _, source := range []string{"stdin", "file"} {
			for _, v := range []struct {
				name, prefix, suffix, want string
			}{
				{"no mark", "", "\n", want},
				{"one mark", mark, "\r\n", want},
				{"two marks", mark + mark, "\n", want},
				{"a mark after the first byte", "x", "\n", "x" + want},
				{"a UTF-16 mark after a UTF-8 mark", mark + "\xff\xfe", "\n", "\ufffd\ufffd" + want},
			} {
				t.Run(row.driver+" "+row.field+" "+source+" "+v.name, func(t *testing.T) {
					content := v.prefix + want + v.suffix
					if v.name == "a UTF-16 mark after a UTF-8 mark" && row.field == "kubeconfigContent" {
						t.Skip("a value with a leading word is not a kubeconfig")
					}
					if v.name == "a mark after the first byte" {
						content = "x" + mark + want + v.suffix
						v.want = "x" + mark + want
						if row.field == "kubeconfigContent" {
							t.Skip("a value with a leading word is not a kubeconfig")
						}
					}
					argv, stdin := ccFromFileArgv(t, row, source, content)
					var posts []string
					stdout, stderr, code, _ := ccRun(t, ccStub(ccNorman{status: 201, body: `{"id":"cattle-global-data:cc-abcde"}`}, &posts, false), argv, stdin)
					require.Zero(t, code, stdout+stderr)
					assert.Equal(t, v.want, ccPostedValue(t, posts, row.driver, row.field))
				})
			}
			for _, v := range []struct{ name, content string }{
				{"UTF-16 little endian mark", "\xff\xfea\x00b\x00"},
				{"UTF-16 big endian mark", "\xfe\xff\x00a\x00b"},
				{"a NUL byte", "ab\x00cd"},
				{"a UTF-16 mark before a UTF-8 mark", "\xff\xfe" + mark + "ab"},
			} {
				t.Run(row.driver+" "+row.field+" "+source+" "+v.name, func(t *testing.T) {
					argv, stdin := ccFromFileArgv(t, row, source, v.content)
					var posts []string
					stdout, stderr, _, _ := ccRun(t, ccStub(ccNorman{status: 201, body: `{"id":"x"}`}, &posts, false), argv, stdin)
					assert.Empty(t, posts, "a refused value sends no request")
					assert.Contains(t, stdout+stderr, "VALIDATION")
					assert.Contains(t, stdout+stderr, row.field)
					assert.Contains(t, stdout+stderr, "UTF-16")
				})
			}
		}
	}
}

// ccFromFileArgv builds the command line that sends content for the row's
// field from stdin or from a file, with every other required field given. The
// other secret fields come from a file.
func ccFromFileArgv(t *testing.T, row ccSecretFieldRow, source, content string) (argv []string, stdin string) {
	t.Helper()
	argv = []string{"create", "cloud-credential", row.driver, "cc-" + strings.ToLower(row.driver), "--apply", "--wait=false"}
	for _, name := range row.required {
		argv = append(argv, "--set", name+"=public")
	}
	dir := t.TempDir()
	for _, name := range row.others {
		path := filepath.Join(dir, name+".txt")
		require.NoError(t, os.WriteFile(path, []byte(ccFieldValue(name)+"\n"), 0o600))
		argv = append(argv, "--from-file", name+"="+path)
	}
	if source == "stdin" {
		return append(argv, "--from-file", row.field+"=-"), content
	}
	path := filepath.Join(dir, "value.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return append(argv, "--from-file", row.field+"="+path), ""
}

// TestEveryCommandWithFromFileHasAByteOrderMarkRow asserts that every command
// that declares --from-file has a row in fromFileCommandRows and that no row
// names a command without the flag.
func TestEveryCommandWithFromFileHasAByteOrderMarkRow(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var declaring []string
	for _, c := range wr.reg.Commands() {
		if slices.Contains(declaredFlags(c), "from-file") {
			declaring = append(declaring, c.Describe().Name)
		}
	}
	var rows []string
	for name := range fromFileCommandRows {
		rows = append(rows, name)
	}
	assert.ElementsMatch(t, rows, declaring)
}

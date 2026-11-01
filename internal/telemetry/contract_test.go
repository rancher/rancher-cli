package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type publishedList struct {
	Versions []struct {
		Version string            `json:"version"`
		Files   map[string]string `json:"files"`
	} `json:"versions"`
}

var versionDirRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// contractProblems checks a contract root: every listed version's files
// match their recorded hashes and nothing else is in the directory, no
// listed version is marked draft, and every version directory is listed or
// marked draft. It returns one line per problem.
func contractProblems(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "published.json"))
	require.NoError(t, err)
	var list publishedList
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&list))

	var out []string
	listed := map[string]bool{}
	for _, v := range list.Versions {
		listed[v.Version] = true
		dir := filepath.Join(root, v.Version)
		have := map[string]string{}
		walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			have[filepath.ToSlash(rel)] = "sha256:" + hex.EncodeToString(sum[:])
			return nil
		})
		if walkErr != nil {
			out = append(out, "version "+v.Version+": directory unreadable")
			continue
		}
		for name, want := range v.Files {
			got, ok := have[name]
			switch {
			case !ok:
				out = append(out, "version "+v.Version+": "+name+" is missing")
			case got != want:
				out = append(out, "version "+v.Version+": "+name+" changed")
			}
		}
		for name := range have {
			if _, ok := v.Files[name]; !ok {
				out = append(out, "version "+v.Version+": "+name+" is not listed")
			}
		}
		if _, ok := have["DRAFT"]; ok {
			out = append(out, "version "+v.Version+": published but marked DRAFT")
		}
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !versionDirRe.MatchString(e.Name()) {
			out = append(out, "directory "+e.Name()+" is not a version name")
			continue
		}
		if listed[e.Name()] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "DRAFT")); err != nil {
			out = append(out, "version "+e.Name()+": neither listed in published.json nor marked DRAFT")
		}
	}
	slices.Sort(out)
	return out
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		return os.WriteFile(dst, data, 0o644)
	}))
}

// TestPublishedVersionsStayUnchanged asserts the hash comparison on the
// fixture that lists a published version: the committed fixture is clean;
// an edited file, an added file, a missing file and a DRAFT marker on a
// published version each fail.
func TestPublishedVersionsStayUnchanged(t *testing.T) {
	fixture := filepath.Join("testdata", "contract")
	assert.Empty(t, contractProblems(t, fixture), "the committed fixture")

	edit := func(name string, change func(root string), want string) {
		root := t.TempDir()
		copyTree(t, fixture, root)
		change(root)
		got := contractProblems(t, root)
		assert.True(t, slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }), "%s: %v", name, got)
	}
	edit("edited file", func(r string) {
		require.NoError(t, os.WriteFile(filepath.Join(r, "0.0.1", "event.schema.json"), []byte("changed\n"), 0o644))
	}, "event.schema.json changed")
	edit("added file", func(r string) {
		require.NoError(t, os.WriteFile(filepath.Join(r, "0.0.1", "extra.md"), []byte("x"), 0o644))
	}, "extra.md is not listed")
	edit("missing file", func(r string) {
		require.NoError(t, os.Remove(filepath.Join(r, "0.0.1", "examples", "note.txt")))
	}, "examples/note.txt is missing")
	edit("draft marker on a published version", func(r string) {
		require.NoError(t, os.WriteFile(filepath.Join(r, "0.0.1", "DRAFT"), nil, 0o644))
	}, "published but marked DRAFT")
}

// TestEveryVersionDirectoryIsListedOrDraft asserts a version directory with
// neither a listing nor a DRAFT file is reported, on the fixture and on the
// committed contract directory, which holds no such directory.
func TestEveryVersionDirectoryIsListedOrDraft(t *testing.T) {
	assert.Empty(t, contractProblems(t, contractRoot), "the committed contract directory")

	root := t.TempDir()
	copyTree(t, filepath.Join("testdata", "contract"), root)
	require.NoError(t, os.Mkdir(filepath.Join(root, "2.0.0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "2.0.0", "fields.md"), []byte("x"), 0o644))
	got := contractProblems(t, root)
	assert.Contains(t, got, "version 2.0.0: neither listed in published.json nor marked DRAFT")
	require.NoError(t, os.Remove(filepath.Join(root, "0.0.2", "DRAFT")))
	assert.Contains(t, contractProblems(t, root), "version 0.0.2: neither listed in published.json nor marked DRAFT")

	other := t.TempDir()
	copyTree(t, filepath.Join("testdata", "contract"), other)
	for _, name := range []string{"latest", "v1.1.0", "1.1.0-rc1"} {
		require.NoError(t, os.Mkdir(filepath.Join(other, name), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(other, name, "fields.md"), []byte("x"), 0o644))
		assert.Contains(t, contractProblems(t, other), "directory "+name+" is not a version name", name)
	}
}

// TestEverySchemaKeyIsDocumented asserts the generated field table names the
// schema's own keys (the ones that are not JSON Schema keywords) and every
// attribute; a key added to the schema without a sentence in the table
// fails.
func TestEverySchemaKeyIsDocumented(t *testing.T) {
	files, err := ContractFiles(ContractInputs{ErrorCodes: []string{"RBAC", "GENERIC"}})
	require.NoError(t, err)
	var schema any
	require.NoError(t, json.Unmarshal(files[SchemaFile], &schema))
	var keys []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				if strings.HasPrefix(k, "x-") {
					keys = append(keys, k)
				}
				walk(sub)
			}
		case []any:
			for _, sub := range x {
				walk(sub)
			}
		}
	}
	walk(schema)
	require.NotEmpty(t, keys)
	for _, a := range Attributes() {
		keys = append(keys, a.Name)
	}
	table := string(files[FieldsFile])
	for _, k := range keys {
		assert.Contains(t, table, "`"+k+"`", "%s is in the schema and not in the field table", k)
	}
}

// TestCurrentVersionIsADraftWithNoPublishedVersions asserts the committed
// state: the current version directory is marked DRAFT and published.json
// lists nothing, until an upload stage publishes a version.
func TestCurrentVersionIsADraftWithNoPublishedVersions(t *testing.T) {
	_, err := os.Stat(filepath.Join(versionDir, "DRAFT"))
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(contractRoot, "published.json"))
	require.NoError(t, err)
	var list publishedList
	require.NoError(t, json.Unmarshal(data, &list))
	assert.Empty(t, list.Versions)
}

// TestContractFilesUseLineFeeds asserts the committed contract files hold no
// carriage return; their hashes are the same on every platform.
func TestContractFilesUseLineFeeds(t *testing.T) {
	require.NoError(t, filepath.WalkDir(contractRoot, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "\r", p)
		return nil
	}))
}

// TestMappingAppendsTheEncodingSection asserts the OTLP mapping holds no
// encoding section when the input has none, and otherwise ends with its last
// paragraph, exactly one blank line and the section text, with no line feed
// added after the section: the text is written as given.
func TestMappingAppendsTheEncodingSection(t *testing.T) {
	codes := []string{"RBAC"}
	without, err := ContractFiles(ContractInputs{ErrorCodes: codes})
	require.NoError(t, err)
	base := string(without[OTLPMappingFile])
	committed, err := os.ReadFile(filepath.Join(versionDir, OTLPMappingFile))
	require.NoError(t, err)
	before, _, found := strings.Cut(string(committed), "\n## JSON encoding\n")
	require.True(t, found, "the committed mapping holds the section")
	assert.Equal(t, base, before, "without a section the mapping is the committed one up to the blank line before the section")
	assert.NotContains(t, base, "JSON encoding")
	require.True(t, strings.HasSuffix(base, ".\n"), "the mapping ends with its last paragraph and one line feed")

	const section = "## JSON encoding\n\nText of the section.\n"
	with, err := ContractFiles(ContractInputs{ErrorCodes: codes, OTLPEncoding: section})
	require.NoError(t, err)
	got := string(with[OTLPMappingFile])
	assert.Equal(t, base+"\n"+section, got)
	assert.True(t, strings.HasSuffix(got, ".\n\n"+section), "one blank line between the last paragraph and the section")
	assert.False(t, strings.HasSuffix(got, "\n\n\n"+section), "no second blank line")
	assert.Equal(t, 1, strings.Count(got, "## JSON encoding"))
}

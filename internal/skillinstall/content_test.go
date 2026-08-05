package skillinstall

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var updateGoldens = flag.Bool("update", false, "rewrite goldens")

func testSource(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: rancher\ndescription: Operate Rancher.\n---\n\n# Body\n")},
		"ref.md":   &fstest.MapFile{Data: []byte("reference\n")},
	}
}

func frontmatterBytes(t *testing.T, skillMD []byte) []byte {
	t.Helper()
	rest, found := bytes.CutPrefix(skillMD, []byte("---\n"))
	require.True(t, found)
	fm, _, found := bytes.Cut(rest, []byte("\n---\n"))
	require.True(t, found)
	return fm
}

func TestRenderedStampsInstallRecord(t *testing.T) {
	SetSource(testSource(t))
	files, err := Rendered("v1.2.3")
	require.NoError(t, err)
	rec, ok := ReadInstallRecord(files["SKILL.md"])
	require.True(t, ok)
	assert.Equal(t, "skill-install/v1", rec.Owner)
	assert.Equal(t, "v1.2.3", rec.CLIVersion)
	assert.Len(t, rec.ContentDigest, 64)
	assert.Equal(t, []byte("reference\n"), files["ref.md"])
	assert.Equal(t, []byte("skill-install/v1\n"), files[OwnerMarkerFile])
}

func TestRenderedPreservesNameAndDescription(t *testing.T) {
	SetSource(testSource(t))
	files, err := Rendered("v1.2.3")
	require.NoError(t, err)
	var fm struct {
		Name        string         `yaml:"name"`
		Description string         `yaml:"description"`
		Metadata    map[string]any `yaml:"metadata"`
	}
	require.NoError(t, yaml.Unmarshal(frontmatterBytes(t, files["SKILL.md"]), &fm))
	assert.Equal(t, "rancher", fm.Name)
	assert.Equal(t, "Operate Rancher.", fm.Description)
	keys := make([]string, 0, len(fm.Metadata))
	for k := range fm.Metadata {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"rancherCliInstaller", "cliVersion", "contentDigest"}, keys)
}

// TestDigestRoundTrip: recomputing the canonical digest over the rendered
// tree equals the stamped value — install-then-status in-sync depends on it.
func TestDigestRoundTrip(t *testing.T) {
	SetSource(testSource(t))
	files, err := Rendered("v1.2.3")
	require.NoError(t, err)
	rec, _ := ReadInstallRecord(files["SKILL.md"])
	got, err := DigestTree(files)
	require.NoError(t, err)
	assert.Equal(t, rec.ContentDigest, got)
}

func TestDigestDetectsBodyEdit(t *testing.T) {
	SetSource(testSource(t))
	files, _ := Rendered("v1.2.3")
	rec, _ := ReadInstallRecord(files["SKILL.md"])
	files["ref.md"] = []byte("tampered\n")
	got, err := DigestTree(files)
	require.NoError(t, err)
	assert.NotEqual(t, rec.ContentDigest, got)
}

func TestDigestGolden(t *testing.T) {
	SetSource(testSource(t))
	files, _ := Rendered("v0.0.0-golden")
	got, err := DigestTree(files)
	require.NoError(t, err)
	golden := filepath.Join("testdata", "digest.golden")
	if *updateGoldens {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestReadInstallRecordForeign(t *testing.T) {
	t.Parallel()
	_, ok := ReadInstallRecord([]byte("---\nname: other\n---\nbody"))
	assert.False(t, ok)
}

// ReadTree rejects anything that is not a regular file — an internal file
// symlink must never be followed (it could read outside the installation).
func TestReadTreeRejectsNonRegularEntries(t *testing.T) {
	skipIfNoSymlinks(t)
	t.Parallel()
	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("outside\n"), 0o644))

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.md")))
	_, err := ReadTree(dir)
	require.ErrorIs(t, err, ErrNonRegular)

	dir2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "SKILL.md"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir2, "subdir")))
	_, err = ReadTree(dir2)
	require.ErrorIs(t, err, ErrNonRegular)
}

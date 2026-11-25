package skills

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func treeMap(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()
	m := map[string]string{}
	require.NoError(t, fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, path)
		require.NoError(t, err)
		m[path] = string(b)
		return nil
	}))
	return m
}

// TestEmbeddedSkillMatchesDisk asserts the embedded tree equals the
// rancher/ directory on disk: complete relative-path and byte equality,
// both directions.
func TestEmbeddedSkillMatchesDisk(t *testing.T) {
	embedded := treeMap(t, Rancher())
	disk := treeMap(t, os.DirFS("rancher"))
	assert.Equal(t, disk, embedded)
}

// maxSkillFileBytes is the size above which an agent harness may cut the
// output of one read of a skill file. Bytes are counted as stored on disk;
// the margin below the smallest known limit covers CRLF on a Windows
// checkout.
const maxSkillFileBytes = 20000

// TestEverySkillFileIsBelowTheSizeLimit asserts no file under rancher/ is
// larger than maxSkillFileBytes.
func TestEverySkillFileIsBelowTheSizeLimit(t *testing.T) {
	disk := os.DirFS("rancher")
	count := 0
	require.NoError(t, fs.WalkDir(disk, ".", func(name string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		count++
		info, err := d.Info()
		require.NoError(t, err)
		assert.LessOrEqual(t, info.Size(), int64(maxSkillFileBytes), "%s is too large for one read", name)
		return nil
	}))
	assert.Greater(t, count, 1, "the skill must be split into a core and topic files")
}

// referenceFiles lists the names of the files under rancher/references/,
// in the form references/<name>.md.
func referenceFiles(t *testing.T) []string {
	t.Helper()
	var names []string
	require.NoError(t, fs.WalkDir(os.DirFS("rancher"), "references", func(name string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if !d.IsDir() {
			names = append(names, name)
		}
		return nil
	}))
	slices.Sort(names)
	return names
}

var topicsSectionPattern = regexp.MustCompile(`(?ms)^## Topics$(.*?)(?:^## |\z)`)

var topicFilePattern = regexp.MustCompile("`(references/[a-z0-9-]+\\.md)`")

// TestTheIndexNamesEveryReferenceFile asserts the "## Topics" section of
// SKILL.md names every file under references/ and names no file that does
// not exist.
func TestTheIndexNamesEveryReferenceFile(t *testing.T) {
	core, err := os.ReadFile("rancher/SKILL.md")
	require.NoError(t, err)
	section := topicsSectionPattern.FindSubmatch(core)
	require.NotNil(t, section, "SKILL.md has no ## Topics section")
	var named []string
	for _, m := range topicFilePattern.FindAllSubmatch(section[1], -1) {
		named = append(named, string(m[1]))
	}
	slices.Sort(named)
	require.NotEmpty(t, named)
	assert.Equal(t, referenceFiles(t), named)
}

var topicMentionPattern = regexp.MustCompile("(?i)the\\s+topic\\s+`([^`]+)`")

// TestEveryTopicNamedInTheSkillIsAFile asserts each "the topic `<name>`" in
// a file of the skill names a file in references/.
func TestEveryTopicNamedInTheSkillIsAFile(t *testing.T) {
	disk := os.DirFS("rancher")
	files := map[string]bool{}
	for _, name := range referenceFiles(t) {
		files[strings.TrimSuffix(path.Base(name), ".md")] = true
	}
	mentions := 0
	require.NoError(t, fs.WalkDir(disk, ".", func(name string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		text, err := fs.ReadFile(disk, name)
		require.NoError(t, err)
		for _, m := range topicMentionPattern.FindAllSubmatch(text, -1) {
			mentions++
			assert.True(t, files[string(m[1])], "%s names the topic %q, which is not in references/", name, m[1])
		}
		return nil
	}))
	assert.Positive(t, mentions)
}

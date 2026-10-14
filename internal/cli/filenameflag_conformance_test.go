package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// filenameFlagSuffix ends the description of every flag named filename:
// that name means a file the command reads, with - for standard input.
const filenameFlagSuffix = ", or - for stdin"

// filenameFlagProblems lists the flags of d named filename whose
// description does not end with filenameFlagSuffix, and how many flags of
// d are named filename. The registry does not tell how a flag's value is
// used, so the test asserts the documented contract of the name: a file
// the command reads. A flag that names a file the command writes takes
// another name, such as output-file.
func filenameFlagProblems(d runtime.Descriptor) (problems []string, seen int) {
	for _, f := range d.Flags {
		if f.Name != "filename" {
			continue
		}
		seen++
		if !strings.HasSuffix(f.Desc, filenameFlagSuffix) {
			problems = append(problems, "--filename has description \""+f.Desc+"\", want one ending \""+filenameFlagSuffix+"\"")
		}
	}
	return problems, seen
}

// TestFilenameFlagNamesAFileTheCommandReads asserts, for every registered
// command, that each flag named filename describes a file the command
// reads. One subtest per command, so one run lists every failure. There is
// no allowlist.
func TestFilenameFlagNamesAFileTheCommandReads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	total := 0
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		problems, seen := filenameFlagProblems(d)
		total += seen

		t.Run(d.Name, func(t *testing.T) {
			for _, p := range problems {
				assert.Fail(t, d.Name+": "+p)
			}
		})
	}
	assert.NotZero(t, total, "no command declares a flag named filename; the walk saw nothing")
}

// TestFilenameFlagRuleCatchesADeviation asserts that the rule accepts a
// flag that reads a file and reports one that does not.
func TestFilenameFlagRuleCatchesADeviation(t *testing.T) {
	ok := runtime.Descriptor{Flags: []runtime.ArgSpec{
		{Name: "filename", Desc: "manifest `file`, or - for stdin"},
		{Name: "other", Desc: "write to `file`"},
	}}
	problems, seen := filenameFlagProblems(ok)
	assert.Empty(t, problems)
	assert.Equal(t, 1, seen)

	bad := runtime.Descriptor{Flags: []runtime.ArgSpec{
		{Name: "filename", Desc: "write the tar.gz archive to `file`"},
	}}
	problems, seen = filenameFlagProblems(bad)
	assert.Len(t, problems, 1)
	assert.Equal(t, 1, seen)
}

package skillinstall

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testfs"
)

const (
	symlinkWhat    = "is a symbolic link to"
	irregularWhat  = "is a Windows directory link (NTFS junction) or another entry that is not a directory"
	plainFileWhat  = "is not a directory"
	unreadableWhat = "is a symbolic link whose destination could not be read"
	forcePhrase    = "; --force does not prevent this error"
)

func linkHint(component string) string {
	return "remove the symbolic link at " + component + ", then run this command again"
}

func directoryLinkHint(component string) string {
	return "remove the Windows directory link (NTFS junction) or other entry at " + component +
		", then run this command again"
}

func notDirectoryHint(component string) string {
	return "move " + component + " away or replace it with a directory, then run this command again"
}

func checkHint(component string) string {
	return "check " + component +
		" and the directories above it for the problem the error names, then run this command again"
}

// refusalPrefix is the text of a refusal at component for the entry described
// by what: the form for a parent of the skill directory, or the form for the
// skill directory itself.
func refusalPrefix(component, dir, what string) string {
	if component == dir {
		return "the skill directory " + dir + " " + what
	}
	return "cannot use the skill directory " + dir + ": " + component + ", on the path to it, " + what
}

// linkKind is one thing a component of a project path can be instead of a
// real directory.
type linkKind struct {
	name string
	// finalOK is true when the kind can also stand in for the skill directory
	// itself.
	finalOK bool
	// hasLead is true when the entry leads to a directory the test fills.
	hasLead bool
	// inside is true when that directory is inside the checkout.
	inside bool
	what   string
	plant  func(t *testing.T, component, lead, outside string)
}

func linkKinds() []linkKind {
	absolute := func(t *testing.T, component, lead, _ string) {
		t.Helper()
		testfs.Symlink(t, lead, component)
	}
	kinds := []linkKind{
		{name: "relative-link", finalOK: true, hasLead: true, what: symlinkWhat,
			plant: func(t *testing.T, component, lead, _ string) {
				t.Helper()
				rel, err := filepath.Rel(filepath.Dir(component), lead)
				if err != nil {
					t.Fatalf("setup: %v", err)
				}
				testfs.Symlink(t, rel, component)
			}},
		{name: "absolute-link", finalOK: true, hasLead: true, what: symlinkWhat, plant: absolute},
		{name: "inside-link", finalOK: true, hasLead: true, inside: true, what: symlinkWhat, plant: absolute},
		{name: "dangling-link", finalOK: true, what: symlinkWhat,
			plant: func(t *testing.T, component, _, outside string) {
				t.Helper()
				testfs.Symlink(t, filepath.Join(outside, "gone"), component)
			}},
		{name: "plain-file",
			what: plainFileWhat,
			plant: func(t *testing.T, component, _, _ string) {
				t.Helper()
				if err := os.WriteFile(component, []byte("x"), 0o644); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}},
	}
	if testfs.OnWindows() {
		kinds = append(kinds, linkKind{name: "junction", finalOK: true, hasLead: true,
			what: irregularWhat,
			plant: func(t *testing.T, component, lead, _ string) {
				t.Helper()
				testfs.Junction(t, lead, component)
			}})
	}
	return kinds
}

// victimMode is what the directory a link leads to holds.
type victimMode struct {
	name string
	// kind is "foreign" (a directory with a file rancher-cli did not
	// install), "installed" (a real rancher-cli install) or "empty" (the
	// parents of the skill directory, and not the skill directory).
	kind string
}

// operation is one entry point of the package that reaches a project path.
type operation struct {
	name string
	// verb is the verb Acquire takes; empty for StatusTarget.
	verb  string
	force bool
	// use runs on the lease; nil means Acquire alone.
	use func(l *Lease) error
	// installedVictim is true when the directory the link leads to is a real
	// rancher-cli install: the one thing uninstall may remove, and what a
	// status check reports on.
	installedVictim bool
}

func operations() []operation {
	return []operation{
		{name: "install", verb: "install", use: func(l *Lease) error {
			_, err := l.Install("v1.0.0", false)
			return err
		}},
		{name: "install-force", verb: "install", force: true, use: func(l *Lease) error {
			_, err := l.Install("v1.0.0", true)
			return err
		}},
		{name: "install-force-if-changed", verb: "install", force: true, use: func(l *Lease) error {
			_, _, err := l.InstallIfChanged("v1.0.0", true)
			return err
		}},
		{name: "install-if-changed", verb: "install", use: func(l *Lease) error {
			_, _, err := l.InstallIfChanged("v1.0.0", false)
			return err
		}},
		{name: "uninstall", verb: "uninstall", installedVictim: true, use: func(l *Lease) error {
			_, _, err := l.Uninstall()
			return err
		}},
		{name: "status", installedVictim: true},
		{name: "acquire-install", verb: "install"},
		{name: "acquire-install-force", verb: "install", force: true},
		{name: "acquire-uninstall", verb: "uninstall", installedVictim: true},
	}
}

// runOperation runs op on the target the way a command does and returns the
// first error.
func runOperation(op operation, tg Target, lockDir string) error {
	if op.verb == "" {
		_, _, err := StatusTarget(tg, "v1.0.0")
		return err
	}
	leases, err := Acquire(lockDir, []Target{tg}, op.verb, op.force)
	if err != nil {
		return err
	}
	defer ReleaseAll(leases)
	if op.use == nil {
		return nil
	}
	return op.use(leases[0])
}

// installAt puts a real rancher-cli install at dir.
func installAt(t *testing.T, dir string) {
	t.Helper()
	tg := Target{Harness: "claude", Scope: ScopeUser, Dir: dir}
	leases, err := Acquire(t.TempDir(), []Target{tg}, "install", false)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err = leases[0].Install("v1.0.0", false)
	ReleaseAll(leases)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// fillVictim makes the directory a link leads to hold, below lead, the rest
// of the path to the skill directory.
func fillVictim(t *testing.T, lead string, rest []string, kind string) {
	t.Helper()
	victim := filepath.Join(append([]string{lead}, rest...)...)
	var err error
	switch kind {
	case "foreign":
		if err = os.MkdirAll(victim, 0o755); err == nil {
			err = os.WriteFile(filepath.Join(victim, "precious.txt"), []byte("precious\n"), 0o644)
		}
	case "installed":
		if err = os.MkdirAll(filepath.Dir(victim), 0o755); err == nil {
			installAt(t, victim)
		}
	default:
		dir := lead
		if len(rest) > 0 {
			dir = filepath.Join(append([]string{lead}, rest[:len(rest)-1]...)...)
		}
		err = os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// projectRow is one planted link: a target of a fresh checkout, the position
// of the component that was replaced and what it leads to.
type projectRow struct {
	repo, outside, lead string
	tg                  Target
	segments            []string
	component           string
	rest                []string
}

func projectTargets(t *testing.T, repo string) []Target {
	t.Helper()
	targets, err := Targets("all", ScopeProject, repo)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return targets
}

func segmentsBelow(t *testing.T, root, dir string) []string {
	t.Helper()
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return strings.Split(rel, string(filepath.Separator))
}

// newProjectRow makes a fresh checkout and an outside directory and chooses
// the component; nothing is planted yet.
func newProjectRow(t *testing.T, k linkKind, index, pos int) projectRow {
	t.Helper()
	repo, outside := t.TempDir(), t.TempDir()
	tg := projectTargets(t, repo)[index]
	segments := segmentsBelow(t, repo, tg.Dir)
	r := projectRow{
		repo: repo, outside: outside, tg: tg, segments: segments,
		component: filepath.Join(append([]string{repo}, segments[:pos+1]...)...),
		rest:      segments[pos+1:],
		lead:      filepath.Join(outside, "lead"),
	}
	if k.inside {
		r.lead = filepath.Join(repo, "shared")
	}
	return r
}

// plant fills the victim and replaces the component with the entry of kind k.
func (r projectRow) plant(t *testing.T, k linkKind, victim string) {
	t.Helper()
	if k.hasLead {
		fillVictim(t, r.lead, r.rest, victim)
	}
	if err := os.MkdirAll(filepath.Dir(r.component), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	k.plant(t, r.component, r.lead, r.outside)
}

func victimModes(k linkKind, op operation) []victimMode {
	if !k.hasLead {
		return []victimMode{{name: "no-victim", kind: "empty"}}
	}
	first := victimMode{name: "foreign", kind: "foreign"}
	if op.installedVictim {
		first = victimMode{name: "installed", kind: "installed"}
	}
	return []victimMode{first, {name: "empty", kind: "empty"}}
}

// requireRefusal asserts err is the refusal of kind k at component, as a
// typed validation error with a hint.
func requireRefusal(t *testing.T, err error, k linkKind, component, dir, verb string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "not a typed error: %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	wantHint := linkHint(component)
	switch k.name {
	case "plain-file":
		wantHint = notDirectoryHint(component)
	case "junction":
		wantHint = directoryLinkHint(component)
	}
	assert.Equal(t, wantHint, ce.Hint)
	if component == dir {
		assert.Equal(t, 1, strings.Count(err.Error(), dir), "the skill directory is named once: %v", err)
	}
	require.ErrorContains(t, err, refusalPrefix(component, dir, k.what))
	if verb == "install" && k.name != "plain-file" {
		require.ErrorContains(t, err, forcePhrase)
	} else {
		assert.NotContains(t, err.Error(), "--force")
	}
	assert.NotContains(t, err.Error(), "another skill operation is in progress")
}

// TestProjectScopeRefusesLinkAtEveryComponent asserts, for every project
// target, every component below the working directory and every kind of
// entry that is not a real directory, that install, uninstall and status
// refuse with the link text and change nothing, with or without --force.
func TestProjectScopeRefusesLinkAtEveryComponent(t *testing.T) {
	SetSource(testSource(t))
	probe := t.TempDir()
	targets := projectTargets(t, probe)
	var got, want []string
	for _, tg := range targets {
		got = append(got, tg.Dir)
	}
	for _, h := range Harnesses() {
		want = append(want, h.ProjectDir(probe))
	}
	slices.Sort(want)
	assert.ElementsMatch(t, slices.Compact(want), got, "a harness row is missing from the targets")

	for index, tg := range targets {
		segments := segmentsBelow(t, probe, tg.Dir)
		for pos := range segments {
			for _, k := range linkKinds() {
				if pos == len(segments)-1 && !k.finalOK {
					continue
				}
				for _, op := range operations() {
					for _, mode := range victimModes(k, op) {
						name := strings.Join([]string{op.name, k.name, mode.name,
							filepath.ToSlash(filepath.Join(segments[:pos+1]...))}, "/")
						t.Run(name, func(t *testing.T) {
							r := newProjectRow(t, k, index, pos)
							r.plant(t, k, mode.kind)
							lockDir := t.TempDir()
							beforeRepo, beforeOutside := testfs.Snapshot(t, r.repo), testfs.Snapshot(t, r.outside)

							err := runOperation(op, r.tg, lockDir)
							verb := op.verb
							if verb == "" {
								verb = "status"
							}
							requireRefusal(t, err, k, r.component, r.tg.Dir, verb)
							if op.verb != "" {
								again := runOperation(op, r.tg, lockDir)
								require.Error(t, again)
								assert.Equal(t, err.Error(), again.Error())
							}
							assert.Equal(t, beforeRepo, testfs.Snapshot(t, r.repo))
							assert.Equal(t, beforeOutside, testfs.Snapshot(t, r.outside))
						})
					}
				}
			}
		}
	}

	for index, tg := range targets {
		segments := segmentsBelow(t, probe, tg.Dir)
		for pos := range segments {
			for _, k := range linkKinds() {
				if pos == len(segments)-1 && !k.finalOK {
					continue
				}
				for _, op := range []operation{operations()[0], operations()[1], operations()[4]} {
					name := strings.Join([]string{"full-list", op.name, k.name,
						filepath.ToSlash(filepath.Join(segments[:pos+1]...))}, "/")
					t.Run(name, func(t *testing.T) {
						r := newProjectRow(t, k, index, pos)
						victim := "foreign"
						if op.installedVictim {
							victim = "installed"
						}
						r.plant(t, k, victim)
						beforeRepo, beforeOutside := testfs.Snapshot(t, r.repo), testfs.Snapshot(t, r.outside)

						leases, err := Acquire(t.TempDir(), projectTargets(t, r.repo), op.verb, op.force)
						ReleaseAll(leases)
						requireRefusal(t, err, k, r.component, r.tg.Dir, op.verb)
						assert.Equal(t, beforeRepo, testfs.Snapshot(t, r.repo))
						assert.Equal(t, beforeOutside, testfs.Snapshot(t, r.outside))
					})
				}
			}
		}
	}
}

// TestProjectScopeRefusesLinkPlantedAfterAcquire asserts that a component
// replaced by a link after Acquire is refused by every method of the lease,
// before the staging directory, the temporary files, the removal of leftovers
// or the removal of the skill directory can reach the place it leads to.
func TestProjectScopeRefusesLinkPlantedAfterAcquire(t *testing.T) {
	SetSource(testSource(t))
	methods := []operation{
		{name: "install", verb: "install", use: func(l *Lease) error {
			_, err := l.Install("v1.0.0", false)
			return err
		}},
		{name: "install-force", verb: "install", force: true, use: func(l *Lease) error {
			_, err := l.Install("v1.0.0", true)
			return err
		}},
		{name: "install-if-changed", verb: "install", use: func(l *Lease) error {
			_, _, err := l.InstallIfChanged("v1.0.0", false)
			return err
		}},
		{name: "install-force-if-changed", verb: "install", force: true, use: func(l *Lease) error {
			_, _, err := l.InstallIfChanged("v1.0.0", true)
			return err
		}},
		{name: "uninstall", verb: "uninstall", installedVictim: true, use: func(l *Lease) error {
			_, _, err := l.Uninstall()
			return err
		}},
	}
	probe := t.TempDir()
	for index, tg := range projectTargets(t, probe) {
		segments := segmentsBelow(t, probe, tg.Dir)
		for pos := range segments {
			for _, k := range linkKinds() {
				if pos == len(segments)-1 && !k.finalOK {
					continue
				}
				for _, m := range methods {
					name := strings.Join([]string{m.name, k.name,
						filepath.ToSlash(filepath.Join(segments[:pos+1]...))}, "/")
					t.Run(name, func(t *testing.T) {
						r := newProjectRow(t, k, index, pos)
						if m.verb == "uninstall" {
							installAt(t, r.tg.Dir)
						}
						leases, err := Acquire(t.TempDir(), []Target{r.tg}, m.verb, m.force)
						if err != nil {
							t.Fatalf("setup: %v", err)
						}
						defer ReleaseAll(leases)
						if err := os.RemoveAll(r.component); err != nil {
							t.Fatalf("setup: %v", err)
						}
						victim := "foreign"
						if m.installedVictim {
							victim = "installed"
						}
						r.plant(t, k, victim)
						if k.hasLead && m.verb == "install" && len(r.rest) > 0 {
							leftovers := filepath.Join(append([]string{r.lead}, r.rest[:len(r.rest)-1]...)...)
							if err := os.MkdirAll(filepath.Join(leftovers, stagingPrefix+"abc"), 0o755); err != nil {
								t.Fatalf("setup: %v", err)
							}
							if err := os.WriteFile(filepath.Join(leftovers, probePrefix+"abc"), []byte("x"), 0o644); err != nil {
								t.Fatalf("setup: %v", err)
							}
						}
						beforeRepo, beforeOutside := testfs.Snapshot(t, r.repo), testfs.Snapshot(t, r.outside)

						requireRefusal(t, m.use(leases[0]), k, r.component, r.tg.Dir, m.verb)
						assert.Equal(t, beforeRepo, testfs.Snapshot(t, r.repo))
						assert.Equal(t, beforeOutside, testfs.Snapshot(t, r.outside))
					})
				}
			}
		}
	}
}

// TestProjectScopeTargetWithoutRootIsRefused asserts that a project target
// with no working directory, whose directory is not inside it, with a .. element
// in the directory or in the working directory, or whose paths have no relative
// form (another volume on Windows), is refused by Acquire and by StatusTarget
// with the text for its case, and nothing is created.
func TestProjectScopeTargetWithoutRootIsRefused(t *testing.T) {
	SetSource(testSource(t))
	base, other := t.TempDir(), t.TempDir()
	sep := string(filepath.Separator)
	dir := filepath.Join(base, ".claude", "skills", "rancher")
	dirDotDot := base + sep + "link" + sep + ".." + sep + filepath.Join(".claude", "skills", "rancher")
	rootDotDot := base + sep + "link" + sep + ".."
	below := filepath.Join(dir, "below")
	outside := func(root string) string {
		return "the skill directory " + dir + " is not inside the working directory " + root
	}
	cases := []struct {
		name, root, dir, want, hint string
		noRelativeForm              bool
	}{
		{name: "empty root", root: "", dir: dir,
			want: "cannot check the skill directory " + dir + ": the working directory is not known"},
		{name: "another directory", root: other, dir: dir, want: outside(other)},
		{name: "root is the skill dir", root: dir, dir: dir, want: outside(dir)},
		{name: "root is below the target", root: below, dir: dir, want: outside(below)},
		{name: "the directory has a .. element", root: base, dir: dirDotDot,
			want: "the skill directory " + dirDotDot + " has a .. element"},
		{name: "the root has a .. element", root: rootDotDot, dir: dir,
			want: "the working directory " + rootDotDot + " has a .. element",
			hint: "change to the working directory by a path with no .. element, then run this command again"},
		{name: "the paths have no relative form", root: base, dir: dir, want: outside(base), noRelativeForm: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.noRelativeForm {
				old := relativePath
				relativePath = func(string, string) (string, error) { return "", errors.New("no relative form") }
				t.Cleanup(func() { relativePath = old })
			}
			tg := Target{Harness: "claude", Scope: ScopeProject, Dir: c.dir, Root: c.root}
			before := testfs.Snapshot(t, base)
			check := func(err error) {
				t.Helper()
				require.ErrorContains(t, err, c.want)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, c.hint, ce.Hint)
			}
			lockDir := t.TempDir()
			for _, verb := range []string{"install", "uninstall"} {
				leases, err := Acquire(lockDir, []Target{tg}, verb, true)
				ReleaseAll(leases)
				check(err)
			}
			_, _, err := StatusTarget(tg, "v1.0.0")
			check(err)
			assert.Equal(t, before, testfs.Snapshot(t, base))
		})
	}
}

// TestProjectScopeSpellingWithoutDotDotIsCheckedAsCleaned asserts that a
// hand-built project target whose directory or working directory has a doubled
// separator, a . element or a trailing separator is not refused for its
// spelling: a real tree passes, and a link on the way is refused naming the
// component as cleaned.
func TestProjectScopeSpellingWithoutDotDotIsCheckedAsCleaned(t *testing.T) {
	testfs.SkipOnWindows(t, "the spellings below are Unix paths")
	repo, outside := t.TempDir(), t.TempDir()
	sep := string(filepath.Separator)
	skills := filepath.Join(repo, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	clean := filepath.Join(skills, "rancher")
	spellings := []struct{ name, root, dir string }{
		{"a doubled separator", repo, repo + sep + sep + ".claude" + sep + "skills" + sep + "rancher"},
		{"a dot element", repo, repo + sep + "." + sep + ".claude" + sep + "skills" + sep + "rancher"},
		{"a trailing separator on the directory", repo, clean + sep},
		{"a trailing separator on the root", repo + sep, clean},
		{"a doubled separator in the root", repo + sep + sep, clean},
	}
	targets := make([]Target, 0, len(spellings))
	for _, c := range spellings {
		targets = append(targets, Target{Harness: "claude", Scope: ScopeProject, Dir: c.dir, Root: c.root})
		require.NoError(t, refuseLinkedPath(targets[len(targets)-1], "install"), c.name)
	}
	if err := os.RemoveAll(skills); err != nil {
		t.Fatalf("setup: %v", err)
	}
	testfs.Symlink(t, outside, skills)
	for i, tg := range targets {
		require.ErrorContains(t, refuseLinkedPath(tg, "install"), skills+", on the path to it, "+symlinkWhat, spellings[i].name)
	}
}

// TestProjectScopeLinkAtSkillDirectoryWithATrailingSeparatorIsRefused asserts
// that a symbolic link at the skill directory is refused, in project and user
// scope, when the directory is written with a trailing separator, which makes
// Lstat follow the link.
func TestProjectScopeLinkAtSkillDirectoryWithATrailingSeparatorIsRefused(t *testing.T) {
	testfs.SkipOnWindows(t, "the spelling below is a Unix path")
	repo, outside := t.TempDir(), t.TempDir()
	sep := string(filepath.Separator)
	skills := filepath.Join(repo, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	dir := filepath.Join(skills, "rancher")
	testfs.Symlink(t, outside, dir)
	for _, tg := range []Target{
		{Harness: "claude", Scope: ScopeProject, Dir: dir + sep, Root: repo},
		{Harness: "claude", Scope: ScopeUser, Dir: dir + sep},
	} {
		err := refuseLinkedPath(tg, "install")
		require.ErrorContains(t, err, "the skill directory "+tg.Dir+" "+symlinkWhat, string(tg.Scope))
		require.ErrorContains(t, err, forcePhrase, string(tg.Scope))
	}
}

// TestProjectScopeDirectoryWithTheIrregularBitIsAccepted asserts that an entry
// Lstat reports as a directory that is also irregular (a Windows directory
// with a reparse tag that is not a link) is accepted at a parent and at the
// skill directory, as a real directory is.
func TestProjectScopeDirectoryWithTheIrregularBitIsAccepted(t *testing.T) {
	SetSource(testSource(t))
	repo := t.TempDir()
	tg := projectTargets(t, repo)[0]
	for _, component := range []string{filepath.Join(repo, ".claude"), tg.Dir} {
		t.Run(filepath.Base(component), func(t *testing.T) {
			info := fakeEntry(fs.ModeDir | fs.ModeIrregular)
			old := lstat
			lstat = func(name string) (os.FileInfo, error) {
				if name == component {
					return info, nil
				}
				return old(name)
			}
			t.Cleanup(func() { lstat = old })
			require.NoError(t, refuseLinkedPath(tg, "install"))
		})
	}
}

// TestProjectScopeUserScopeRefusesLinkAtSkillDirectory asserts that a user
// scope target whose skill directory is a symbolic link is refused by every
// entry point as a typed validation error with the user scope hint, and the
// directory the link leads to is unchanged. The directories above it are
// not checked in user scope.
func TestProjectScopeUserScopeRefusesLinkAtSkillDirectory(t *testing.T) {
	SetSource(testSource(t))
	base, outside := t.TempDir(), t.TempDir()
	lead := filepath.Join(outside, "lead")
	if err := os.MkdirAll(lead, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lead, "precious.txt"), []byte("precious\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	tg := Target{Harness: "claude", Scope: ScopeUser, Dir: filepath.Join(base, ".claude", "skills", "rancher")}
	if err := os.MkdirAll(filepath.Dir(tg.Dir), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	testfs.Symlink(t, lead, tg.Dir)
	before := testfs.Snapshot(t, outside)
	lockDir := t.TempDir()

	check := func(err error, install bool) {
		t.Helper()
		require.ErrorContains(t, err, "the skill directory "+tg.Dir+" "+symlinkWhat)
		if install {
			require.ErrorContains(t, err, forcePhrase)
		}
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, linkHint(tg.Dir), ce.Hint)
		assert.Equal(t, 1, strings.Count(err.Error(), tg.Dir))
	}
	for _, verb := range []string{"install", "uninstall"} {
		leases, err := Acquire(lockDir, []Target{tg}, verb, true)
		ReleaseAll(leases)
		check(err, verb == "install")
	}
	_, _, err := StatusTarget(tg, "v1.0.0")
	check(err, false)
	assert.Equal(t, before, testfs.Snapshot(t, outside))
}

// TestProjectScopeUnreadableDirectoryIsRefusedWithTheCheckText asserts that a
// directory on the way to the skill directory, or the skill directory, that
// cannot be read is refused with the text that names it, by Acquire and by
// StatusTarget. The two rows restrict the real permissions of .claude (the
// walk) and of .claude/skills, which holds the skill directory, so the check of
// the skill directory itself fails. They skip on Windows and as root.
func TestProjectScopeUnreadableDirectoryIsRefusedWithTheCheckText(t *testing.T) {
	SetSource(testSource(t))
	rows := []struct {
		name       string
		restricted []string
		want       func(dir string) string
		component  func(dir string) string
	}{
		{"a parent directory", []string{".claude"}, func(dir string) string {
			return "could not check " + filepath.Dir(dir) + " on the path to the skill directory " + dir + ": permission denied"
		}, func(dir string) string { return filepath.Dir(dir) }},
		{"the directory that holds the skill directory", []string{".claude", "skills"}, func(dir string) string {
			return "could not check the skill directory " + dir + ": permission denied"
		}, func(dir string) string { return dir }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".claude", "skills"), 0o755); err != nil {
				t.Fatalf("setup: %v", err)
			}
			testfs.RestrictAccess(t, filepath.Join(append([]string{repo}, row.restricted...)...), 0o000)
			tg := projectTargets(t, repo)[0]
			want := row.want(tg.Dir)
			check := func(err error) {
				t.Helper()
				require.ErrorContains(t, err, want)
				assert.NotContains(t, err.Error(), "lstat")
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, checkHint(row.component(tg.Dir)), ce.Hint)
			}
			for _, verb := range []string{"install", "uninstall"} {
				leases, err := Acquire(t.TempDir(), []Target{tg}, verb, false)
				ReleaseAll(leases)
				check(err)
			}
			_, _, err := StatusTarget(tg, "v1.0.0")
			check(err)
		})
	}
}

// TestProjectScopeCheckFailureIsRefusedOnEveryOS asserts that an error from
// the check of a directory on the way to the skill directory, other than
// not-exist, is refused with the text that names it, for a parent and for the
// skill directory itself, by Acquire and StatusTarget, on every OS and as root.
// A path error shows only the operating system's error, without the path.
func TestProjectScopeCheckFailureIsRefusedOnEveryOS(t *testing.T) {
	SetSource(testSource(t))
	repo := t.TempDir()
	tg := projectTargets(t, repo)[0]
	parent := filepath.Join(repo, ".claude", "skills")
	plain := errors.New("induced failure")
	rows := []struct {
		name, fails, want string
		err               func(path string) error
	}{
		{"a parent directory", parent,
			"could not check " + parent + " on the path to the skill directory " + tg.Dir + ": induced failure",
			func(string) error { return plain }},
		{"the skill directory", tg.Dir, "could not check the skill directory " + tg.Dir + ": induced failure",
			func(string) error { return plain }},
		{"a path error", parent,
			"could not check " + parent + " on the path to the skill directory " + tg.Dir + ": permission denied",
			func(path string) error {
				return &fs.PathError{Op: "lstat", Path: path, Err: errors.New("permission denied")}
			}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatalf("setup: %v", err)
			}
			old := lstat
			lstat = func(name string) (os.FileInfo, error) {
				if name == row.fails {
					return nil, row.err(name)
				}
				return old(name)
			}
			t.Cleanup(func() { lstat = old })
			before := testfs.Snapshot(t, repo)
			check := func(err error) {
				t.Helper()
				require.ErrorContains(t, err, row.want)
				assert.NotContains(t, err.Error(), "lstat")
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, checkHint(row.fails), ce.Hint)
			}
			for _, verb := range []string{"install", "uninstall"} {
				leases, err := Acquire(t.TempDir(), []Target{tg}, verb, true)
				ReleaseAll(leases)
				check(err)
			}
			_, _, err := StatusTarget(tg, "v1.0.0")
			check(err)
			assert.Equal(t, before, testfs.Snapshot(t, repo))
		})
	}
}

// fakeInfo is the file info Lstat would return for an entry with mode; the
// code under test reads only the mode.
type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (f fakeInfo) IsDir() bool       { return f.mode.IsDir() }

func fakeEntry(mode fs.FileMode) fs.FileInfo { return fakeInfo{mode: mode} }

// TestProjectScopeEntryKindsAreRefusedThroughTheCheck asserts, on every OS,
// the refusal text and hint for an entry that Lstat reports as a Windows
// directory link or other irregular entry, as a symbolic link whose
// destination cannot be read, and as another entry that is not a directory,
// at a parent and at the skill directory, with the force clause for install
// only. The entries come through the lstat seam, so the branches that only
// Windows reaches with a real entry run here too.
func TestProjectScopeEntryKindsAreRefusedThroughTheCheck(t *testing.T) {
	SetSource(testSource(t))
	repo := t.TempDir()
	tg := projectTargets(t, repo)[0]
	parent := filepath.Join(repo, ".claude")
	kinds := []struct {
		name       string
		mode       fs.FileMode
		what       string
		hint       func(string) string
		parentOnly bool
		force      bool
	}{
		{"irregular entry", fs.ModeIrregular, irregularWhat, directoryLinkHint, false, true},
		{"symbolic link with an unreadable destination", fs.ModeSymlink, unreadableWhat, linkHint, false, true},
		{"named pipe", fs.ModeNamedPipe, plainFileWhat, notDirectoryHint, true, false},
	}
	for _, k := range kinds {
		for _, component := range []string{parent, tg.Dir} {
			if k.parentOnly && component == tg.Dir {
				continue
			}
			t.Run(k.name+"/"+filepath.Base(component), func(t *testing.T) {
				info := fakeEntry(k.mode)
				old := lstat
				lstat = func(name string) (os.FileInfo, error) {
					if name == component {
						return info, nil
					}
					return old(name)
				}
				t.Cleanup(func() { lstat = old })
				before := testfs.Snapshot(t, repo)
				want := refusalPrefix(component, tg.Dir, k.what)
				check := func(err error, install bool) {
					t.Helper()
					require.ErrorContains(t, err, want)
					if install && k.force {
						require.ErrorContains(t, err, forcePhrase)
					} else {
						assert.NotContains(t, err.Error(), "--force")
					}
					ce, ok := errors.AsType[*clierr.Error](err)
					require.True(t, ok)
					assert.Equal(t, clierr.CodeValidation, ce.Code)
					assert.Equal(t, k.hint(component), ce.Hint)
				}
				for _, verb := range []string{"install", "uninstall"} {
					leases, err := Acquire(t.TempDir(), []Target{tg}, verb, true)
					ReleaseAll(leases)
					check(err, verb == "install")
				}
				_, _, err := StatusTarget(tg, "v1.0.0")
				check(err, false)
				assert.Equal(t, before, testfs.Snapshot(t, repo))
			})
		}
	}
}

// TestProjectScopeWorkingDirectoryIsCleanedUnlessItHasDotDot asserts that
// Targets takes an unclean working directory with no .. element (a trailing
// separator, a doubled separator, a /. at the end) as its clean form, and keeps
// one with a .. element as it is, so a target built from it is refused.
func TestProjectScopeWorkingDirectoryIsCleanedUnlessItHasDotDot(t *testing.T) {
	testfs.SkipOnWindows(t, "the spellings below are Unix paths")
	repo := t.TempDir()
	sep := string(filepath.Separator)
	if err := os.Mkdir(filepath.Join(repo, "x"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cases := []struct{ name, cwd, want string }{
		{"a trailing separator", repo + sep, repo},
		{"a separator and a dot", repo + sep + ".", repo},
		{"a doubled separator", repo + sep + sep + "x", repo + sep + "x"},
		{"a dot dot element", repo + sep + "x" + sep + "..", repo + sep + "x" + sep + ".."},
		{"a clean path", repo, repo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, workingDirectory(c.cwd))
			targets, err := Targets("claude", ScopeProject, c.cwd)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			assert.Equal(t, c.want, targets[0].Root)
		})
	}
}

// TestProjectScopeRefusesLinkPlantedDuringInstall asserts that a component
// replaced by a link after the files are staged, and before the existing
// skill directory is removed, is refused by the check that runs just before
// the removal, and the directory the link leads to keeps its content and gets
// no leftover.
func TestProjectScopeRefusesLinkPlantedDuringInstall(t *testing.T) {
	SetSource(testSource(t))
	methods := []struct {
		name string
		call func(l *Lease) error
	}{
		{"install", func(l *Lease) error { _, err := l.Install("v1.0.0", true); return err }},
		{"install-if-changed", func(l *Lease) error { _, _, err := l.InstallIfChanged("v1.0.0", true); return err }},
	}
	probe := t.TempDir()
	for index, tg := range projectTargets(t, probe) {
		segments := segmentsBelow(t, probe, tg.Dir)
		for pos := range segments {
			for _, k := range linkKinds() {
				if !k.hasLead {
					continue
				}
				for _, m := range methods {
					name := strings.Join([]string{m.name, k.name, filepath.ToSlash(filepath.Join(segments[:pos+1]...))}, "/")
					t.Run(name, func(t *testing.T) {
						r := newProjectRow(t, k, index, pos)
						leases, err := Acquire(t.TempDir(), []Target{r.tg}, "install", true)
						if err != nil {
							t.Fatalf("setup: %v", err)
						}
						defer ReleaseAll(leases)
						postStageHook = func(string) error {
							if _, err := os.Lstat(r.component); err == nil {
								if err := os.Rename(r.component, r.lead); err != nil {
									t.Fatalf("setup: %v", err)
								}
							}
							r.plant(t, k, "foreign")
							return nil
						}
						t.Cleanup(func() { postStageHook = nil })

						requireRefusal(t, m.call(leases[0]), k, r.component, r.tg.Dir, "install")

						victim := filepath.Join(append([]string{r.lead}, r.rest...)...)
						assert.Equal(t, []string{"precious.txt"}, testfs.Tree(t, victim))
						for _, dir := range []string{r.repo, r.outside} {
							for _, entry := range testfs.Tree(t, dir) {
								assert.NotContains(t, entry, stagingPrefix)
								assert.NotContains(t, entry, markerTmpPrefix)
							}
						}
					})
				}
			}
		}
	}
}

// TestProjectScopeLinkedDirectoryIsNotMergedByTargets asserts that a project
// directory that is a link to another harness directory stays in the list of
// targets and is refused with the link text, before any lock is taken or
// directory is created.
func TestProjectScopeLinkedDirectoryIsNotMergedByTargets(t *testing.T) {
	SetSource(testSource(t))
	repo := t.TempDir()
	claude := filepath.Join(repo, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	codex := filepath.Join(repo, ".codex")
	testfs.Symlink(t, claude, codex)

	targets, err := Targets("all", ScopeProject, repo)
	require.NoError(t, err)
	var names []string
	for _, tg := range targets {
		names = append(names, tg.Harness)
	}
	assert.Equal(t, []string{"claude", "codex", "agents"}, names)

	before := testfs.Snapshot(t, repo)
	for _, verb := range []string{"install", "uninstall"} {
		leases, err := Acquire(t.TempDir(), targets, verb, true)
		ReleaseAll(leases)
		require.ErrorContains(t, err, "cannot use the skill directory "+filepath.Join(codex, "skills", "rancher")+": "+codex+", on the path to it, "+symlinkWhat)
		assert.NotContains(t, err.Error(), "another skill operation is in progress")
	}
	assert.Equal(t, before, testfs.Snapshot(t, repo))
}

// fileOperationNames are the functions of os and filepath that read, create,
// remove or rename a path.
var fileOperationNames = map[string]bool{
	"os.Stat": true, "os.Lstat": true, "os.Readlink": true, "os.ReadFile": true, "os.ReadDir": true,
	"os.Open": true, "os.OpenFile": true, "os.Create": true, "os.CreateTemp": true, "os.WriteFile": true,
	"os.Mkdir": true, "os.MkdirAll": true, "os.MkdirTemp": true, "os.Remove": true, "os.RemoveAll": true,
	"os.Rename": true, "filepath.EvalSymlinks": true, "filepath.Walk": true, "filepath.WalkDir": true,
	"filepath.Glob": true, "os.Chmod": true, "os.Chtimes": true, "os.Chown": true, "os.Truncate": true,
	"os.Symlink": true, "os.Link": true, "os.OpenRoot": true, "os.DirFS": true, "os.CopyFS": true,
}

// fileOperationCounts parses the non-test files of the package and returns,
// per "<file>:<function>:<name>", how many times the function names an os or
// filepath function of fileOperationNames, in a call or as a value. A method
// is written Type.Method. A use inside a closure counts for the function that
// holds it.
func fileOperationCounts(t *testing.T) map[string]int {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	counts := map[string]int{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		require.NoError(t, err)
		packages := map[string]string{}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			name := ""
			switch path {
			case "os":
				name = "os"
			case "path/filepath":
				name = "filepath"
			default:
				continue
			}
			local := name
			if imp.Name != nil {
				local = imp.Name.Name
			}
			packages[local] = name
		}
		for _, decl := range file.Decls {
			scope := "(package level)"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				scope = functionName(fn)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok {
					if name, ok := packages[pkg.Name]; ok && fileOperationNames[name+"."+sel.Sel.Name] {
						counts[fmt.Sprintf("%s:%s:%s.%s", e.Name(), scope, name, sel.Sel.Name)]++
					}
				}
				return true
			})
		}
	}
	return counts
}

func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// fileOperationTable maps each use of a file operation in the package to the
// check that runs before it, or to the reason it reaches no project path. The
// checks are the walk of refuseLinkedPath at: the first pass of Acquire,
// validateTarget (Acquire, the second check of Lease.install and
// Lease.Uninstall), the top of Lease.Install and Lease.InstallIfChanged, and
// StatusTarget.
var fileOperationTable = map[string]struct {
	count int
	why   string
}{
	"ops.go:canonicalTarget:filepath.EvalSymlinks":    {1, "no project path: the result only names a lock file and is never opened"},
	"content.go:ReadTree:os.ReadFile":                 {1, "reads a tree its callers walked: current() runs after the top of Lease.InstallIfChanged, StatusTarget after its own walk"},
	"content.go:ReadTree:filepath.WalkDir":            {1, "reads a tree its callers walked: current() runs after the top of Lease.InstallIfChanged, StatusTarget after its own walk"},
	"ops.go:probeWritable:os.CreateTemp":              {1, "in the parent, after the first pass of Acquire and validateTarget"},
	"ops.go:probeWritable:os.Remove":                  {1, "in the parent, after the first pass of Acquire and validateTarget"},
	"ops.go:Acquire:os.MkdirAll":                      {2, "the lock directory (no project path) and the parent of the skill directory, after the first pass of Acquire and validateTarget"},
	"ops.go:Acquire:os.Lstat":                         {1, "the parent, after the first pass of Acquire and validateTarget"},
	"ops.go:validateTarget:os.Stat":                   {1, "validateTarget, after its walk in the same function"},
	"ops.go:owned:os.Lstat":                           {1, "SKILL.md of a target, after validateTarget or the walk of StatusTarget, or of a staging directory in the checked parent"},
	"ops.go:owned:os.ReadFile":                        {1, "SKILL.md of a target, after validateTarget or the walk of StatusTarget, or of a staging directory in the checked parent"},
	"ops.go:markerComplete:os.Lstat":                  {1, "inside a staging directory in the parent that the top of Lease.Install and Lease.InstallIfChanged checked"},
	"ops.go:markerComplete:os.ReadFile":               {1, "inside a staging directory in the parent that the top of Lease.Install and Lease.InstallIfChanged checked"},
	"ops.go:cleanStaging:os.ReadDir":                  {2, "the parent and its staging directories, after the top of Lease.Install and Lease.InstallIfChanged"},
	"ops.go:cleanStaging:os.Remove":                   {2, "leftover files in the parent, after the top of Lease.Install and Lease.InstallIfChanged"},
	"ops.go:cleanStaging:os.RemoveAll":                {1, "a leftover staging directory in the parent, after the top of Lease.Install and Lease.InstallIfChanged"},
	"ops.go:Lease.install:os.MkdirAll":                {2, "the staging directory and its subdirectories in the parent, after the top of Lease.Install and Lease.InstallIfChanged"},
	"ops.go:Lease.install:os.WriteFile":               {2, "files in the parent and in the staging directory, after the top of Lease.Install and Lease.InstallIfChanged"},
	"ops.go:Lease.install:os.Rename":                  {2, "inside the parent, and the final swap after the second validateTarget"},
	"ops.go:Lease.install:os.Remove":                  {1, "the marker temporary file in the parent, after the top of Lease.Install and Lease.InstallIfChanged"},
	"ops.go:Lease.install:os.RemoveAll":               {2, "the staging directory in the parent, and the target after the second validateTarget"},
	"ops.go:Lease.Uninstall:os.Stat":                  {1, "after validateTarget at the start of Lease.Uninstall"},
	"ops.go:Lease.Uninstall:os.RemoveAll":             {1, "after validateTarget at the start of Lease.Uninstall"},
	"ops.go:removeEmptyParent:os.Lstat":               {1, "the parent, which validateTarget walked at the start of Lease.Uninstall"},
	"ops.go:removeEmptyParent:os.ReadDir":             {2, "the parent, which validateTarget walked at the start of Lease.Uninstall"},
	"ops.go:removeEmptyParent:os.Remove":              {1, "the parent, which validateTarget walked at the start of Lease.Uninstall"},
	"ops.go:StatusTarget:os.Stat":                     {1, "StatusTarget, after its walk in the same function"},
	"projectpath.go:(package level):os.Lstat":         {1, "the walk itself, through the seam of the walk and of the check of the skill directory: Lstat does not follow the last component"},
	"projectpath.go:linkTarget:filepath.EvalSymlinks": {1, "reads where a refused link leads, for the message"},
	"projectpath.go:linkTarget:os.Readlink":           {1, "reads where a refused link leads, for the message"},
	"table.go:dirExists:os.Stat":                      {1, "no project path: user scope detection of a harness in the home directory"},
}

// TestProjectScopeEveryFileOperationIsCovered asserts that every use of an os
// or filepath function that reads, creates, removes or renames a path is a row
// of fileOperationTable with the count the code has, so a new use or a new
// function names the check that protects it, or the reason it needs none.
func TestProjectScopeEveryFileOperationIsCovered(t *testing.T) {
	counts := fileOperationCounts(t)
	for key, n := range counts {
		row, ok := fileOperationTable[key]
		if !ok {
			t.Errorf("%s is used %d times and has no row in fileOperationTable", key, n)
			continue
		}
		assert.Equal(t, row.count, n, key)
	}
	for key, row := range fileOperationTable {
		assert.NotEmpty(t, row.why, key)
		if _, ok := counts[key]; !ok {
			t.Errorf("fileOperationTable has a row for %s, which the code no longer uses", key)
		}
	}
}

// exportedOperations lists every exported function and method of the package:
// the ones a row of TestProjectScopeRefusesLinkAtEveryComponent covers, and
// the ones that reach no project path, with the reason.
var exportedOperations = map[string]string{
	"Acquire":                "covered by the rows of TestProjectScopeRefusesLinkAtEveryComponent",
	"Lease.Install":          "covered by the rows of TestProjectScopeRefusesLinkAtEveryComponent",
	"Lease.InstallIfChanged": "covered by the rows of TestProjectScopeRefusesLinkAtEveryComponent",
	"Lease.Uninstall":        "covered by the rows of TestProjectScopeRefusesLinkAtEveryComponent",
	"StatusTarget":           "covered by the rows of TestProjectScopeRefusesLinkAtEveryComponent",
	"Targets":                "covered by TestProjectScopeLinkedDirectoryIsNotMergedByTargets and the rows of TestProjectScopeRefusesLinkAtEveryComponent, which take their targets from it",
	"ReleaseAll":             "touches no project path: unlocks the lock files",
	"Lease.Target":           "touches no project path: returns the target",
	"DefaultLockDir":         "touches no project path: the lock directory under the user cache directory",
	"Harnesses":              "touches no project path: the table of harness rows",
	"AgentNames":             "touches no project path: the names of the harness rows",
	"Rendered":               "touches no project path: renders the embedded skill text",
	"SetSource":              "touches no project path: sets the embedded skill text",
	"DigestTree":             "touches no project path: hashes a tree held in memory",
	"ReadInstallRecord":      "touches no project path: parses SKILL.md text held in memory",
	"Report.TextDetails":     "touches no project path: formats outcomes",
	"SetRenameHookForTest":   "touches no project path: a test seam",
	"ReadTree":               "reads the directory it is given; its callers, current and StatusTarget, are covered",
}

// TestProjectScopeCoversEveryExportedOperation asserts that every exported
// function and method of the package is listed in exportedOperations, so a new
// entry point is covered by the link tests or says why it needs no check.
func TestProjectScopeCoversEveryExportedOperation(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			name := functionName(fn)
			if fn.Recv != nil && !ast.IsExported(strings.SplitN(name, ".", 2)[0]) {
				continue
			}
			found[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, ok := exportedOperations[name]; !ok {
			t.Errorf("%s is exported and is not listed in exportedOperations", name)
		}
	}
	for name, why := range exportedOperations {
		assert.NotEmpty(t, why, name)
		assert.True(t, found[name], "exportedOperations lists %s, which the package does not export", name)
	}
}

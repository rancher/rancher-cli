package skillinstall

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/gofrs/flock"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// Verdict classifies one target for `skill status`.
type Verdict string

const (
	VerdictInSync   Verdict = "in-sync"
	VerdictSkew     Verdict = "skew"
	VerdictModified Verdict = "modified"
	VerdictForeign  Verdict = "foreign"
	VerdictAbsent   Verdict = "absent"
)

// Outcome is one target's machine-readable result row.
type Outcome struct {
	Target     Target  `json:"target"`
	Action     string  `json:"action,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Verdict    Verdict `json:"verdict,omitempty"`
	CLIVersion string  `json:"cliVersion,omitempty"`
}

// Report is the details payload on a partial-failure error: the per-target
// outcomes. Cleanup warnings ride the error's own WithWarnings instead of a
// field here - one channel, no details/top-level duplicate.
type Report struct {
	Outcomes []Outcome `json:"outcomes"`
}

// TextDetails projects the first three outcomes onto one line each, a
// "… and N more" line when there are more, and a pointer at the full report -
// the default format stays one screen even for a large batch.
func (r Report) TextDetails() []clierr.DetailLine {
	shown := min(len(r.Outcomes), 3)
	lines := make([]clierr.DetailLine, 0, shown+2)
	for _, o := range r.Outcomes[:shown] {
		state := o.Action
		if state == "" {
			state = string(o.Verdict)
		}
		parts := []clierr.DetailPart{clierr.ValuePart("", o.Target.Dir), clierr.ValuePart(": ", state)}
		if o.Reason != "" {
			parts = append(parts, clierr.ValuePart(", ", o.Reason))
		}
		lines = append(lines, clierr.Line(parts...))
	}
	if n := len(r.Outcomes) - shown; n > 0 {
		lines = append(lines, clierr.Line(clierr.LabelPart(fmt.Sprintf("… and %d more", n))))
	}
	parts := []clierr.DetailPart{clierr.LabelPart("to see each target's state, run: ")}
	parts = append(parts, r.statusCommand()...)
	parts = append(parts, clierr.LabelPart("; to see every reason, run the command again with -o json"))
	return append(lines, clierr.Line(parts...))
}

// statusCommand is the skill status command for the targets of the report:
// --agent names the harness when the targets have one, --scope the scope
// when they have one.
func (r Report) statusCommand() []clierr.DetailPart {
	var harness, scope string
	harnesses, scopes := map[string]bool{}, map[Scope]bool{}
	for _, o := range r.Outcomes {
		harnesses[o.Target.Harness] = true
		scopes[o.Target.Scope] = true
		harness, scope = o.Target.Harness, string(o.Target.Scope)
	}
	cmd := []clierr.DetailPart{clierr.LabelPart("rancher skill status")}
	if len(harnesses) == 1 {
		cmd = append(cmd, clierr.ValuePart(" --agent ", harness))
	}
	if len(scopes) == 1 {
		cmd = append(cmd, clierr.ValuePart(" --scope ", scope))
	}
	return cmd
}

const (
	stagingPrefix   = ".rancher-skill-staging-"
	markerTmpPrefix = ".rancher-skill-marker-"
	probePrefix     = ".rancher-skill-probe-"
)

// DefaultLockDir is the production lock directory: the OS-user cache dir -
// stable per OS user and deliberately NOT the CLI config dir, because
// RANCHER_CLI_CONFIG_DIR is a supported isolation mechanism while the
// filesystem targets it locks are shared.
func DefaultLockDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("could not find the user cache directory for the skill lock files: %w", err)
	}
	return filepath.Join(base, "rancher"), nil
}

// canonicalTarget resolves dir for lock keying: the longest existing
// ancestor is resolved through symlinks and the nonexistent remainder is
// appended textually - two paths addressing the same physical target key
// the same lock even when reached through symlinked parents.
//
// On Windows (NTFS) and macOS (APFS) the result is further lowercased: both
// filesystems are case-insensitive by default, so a case-only alias of the
// same directory must produce the same key. A case-sensitive volume on those
// platforms degrades only to two lock files for two genuinely distinct
// directories - the safe direction.
func canonicalTarget(dir string) string {
	fold := func(s string) string {
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			return strings.ToLower(s)
		}
		return s
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fold(filepath.Clean(dir))
	}
	prefix, rest := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			return fold(filepath.Clean(filepath.Join(resolved, rest)))
		}
		rest = filepath.Join(filepath.Base(prefix), rest)
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return fold(abs)
		}
		prefix = parent
	}
}

// lockFilePath maps a target dir to its lock file within the lock domain.
// Hex-hashed canonical name: no path separators, no length limits.
func lockFilePath(lockDir, targetDir string) string {
	sum := sha256.Sum256([]byte(canonicalTarget(targetDir)))
	return filepath.Join(lockDir, "skill-locks", hex.EncodeToString(sum[:])+".lock")
}

// renameHook, when non-nil, runs between remove and rename - the
// deterministic crash seam for tests. Always nil in production.
var renameHook func() error

// SetRenameHookForTest installs the crash/failure seam for tests outside
// this package (the command-level partial-failure test). Test-only.
func SetRenameHookForTest(f func() error) { renameHook = f }

// Lease is one validated, locked target. Only a held lease can mutate.
// Every lease - including one whose target is absent - holds a real lock
// in the OS-user-cache lock domain (DefaultLockDir); there is no lock-less path.
type Lease struct {
	target Target
	fl     *flock.Flock
}

// Target returns the lease's target (read-only accessor for outcomes).
func (l *Lease) Target() Target { return l.target }

// refuseSymlink enforces the Lstat rule at the final target: install,
// status ownership checks, and uninstall never operate through a link.
func refuseSymlink(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to use the skill directory %s: it is a symbolic link", dir)
	}
	return nil
}

// owned reports whether dir is an installation of ours: exact OwnerKey/
// OwnerValue match in SKILL.md metadata. SKILL.md itself is Lstat-checked
// first - a symlinked or otherwise non-regular SKILL.md is NEVER followed
// and can prove nothing, so the target is foreign until a user decides.
func owned(dir string) (bool, InstallRecord, error) {
	p := filepath.Join(dir, "SKILL.md")
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, InstallRecord{}, nil
		}
		return false, InstallRecord{}, err
	}
	if !info.Mode().IsRegular() {
		return false, InstallRecord{}, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false, InstallRecord{}, err
	}
	rec, ok := ReadInstallRecord(b)
	if !ok || rec.Owner != OwnerValue {
		return false, InstallRecord{}, nil
	}
	return true, rec, nil
}

// validateTarget is the per-target preflight shared by Acquire: symlink
// refusal and ownership authorization. force authorizes foreign
// replacement for install; uninstall never touches foreign content.
func validateTarget(t Target, verb string, force bool) error {
	if err := refuseSymlink(t.Dir); err != nil {
		return err
	}
	if _, err := os.Stat(t.Dir); os.IsNotExist(err) {
		return nil
	}
	isOurs, _, err := owned(t.Dir)
	if err != nil {
		return err
	}
	if isOurs {
		return nil
	}
	if verb == "install" && force {
		return nil
	}
	if verb == "install" {
		return fmt.Errorf("%s has content that rancher-cli did not install; use --force to replace it", t.Dir)
	}
	return fmt.Errorf("%s has content that rancher-cli did not install; not removing it", t.Dir)
}

// probeWritable proves the parent accepts writes while we hold its lock.
// The temp file uses probePrefix so a later cleanStaging call can reclaim
// it if os.Remove fails (e.g. a transient permission lock on Windows).
func probeWritable(parent string) error {
	f, err := os.CreateTemp(parent, probePrefix+"*")
	if err != nil {
		return fmt.Errorf("parent directory not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// Acquire validates and locks every target in sorted-Dir order before
// anything can mutate. Locks live under lockDir - in production
// DefaultLockDir(), the OS-user cache dir - so harness paths are never
// touched to acquire one, and an absent uninstall target is serialized
// exactly like a present one (the appear-mid-batch race is closed by the
// lock, not by rechecking). Any failure releases all held locks: zero changed.
func Acquire(lockDir string, targets []Target, verb string, force bool) ([]*Lease, error) {
	if err := os.MkdirAll(filepath.Join(lockDir, "skill-locks"), 0o700); err != nil {
		return nil, err
	}
	sorted := append([]Target{}, targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Dir < sorted[j].Dir })

	var leases []*Lease
	fail := func(err error) ([]*Lease, error) {
		ReleaseAll(leases)
		return nil, err
	}
	for _, t := range sorted {
		fl := flock.New(lockFilePath(lockDir, t.Dir))
		locked, err := fl.TryLock()
		if err != nil {
			return fail(err)
		}
		if !locked {
			return fail(fmt.Errorf("another skill operation is in progress on %s", t.Dir))
		}
		leases = append(leases, &Lease{target: t, fl: fl})
		// validate UNDER the lock, so the state cannot shift before use
		if err := validateTarget(t, verb, force); err != nil {
			return fail(err)
		}
		parent := filepath.Dir(t.Dir)
		if verb == "install" {
			// install creates the parent chain; uninstall never does
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return fail(err)
			}
		}
		if _, err := os.Lstat(parent); err == nil {
			if err := probeWritable(parent); err != nil {
				return fail(err)
			}
		}
	}
	return leases, nil
}

// ReleaseAll unlocks every lease; safe on nil entries and after failure.
func ReleaseAll(leases []*Lease) {
	for _, l := range leases {
		if l != nil && l.fl != nil {
			_ = l.fl.Unlock()
		}
	}
}

// markerComplete reports whether dir carries the complete owner marker
// file. The marker is written via temp+rename, so it is complete-or-absent
// by construction - never a truncation judgment call.
func markerComplete(dir string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, OwnerMarkerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, OwnerMarkerFile))
	if err != nil {
		return false, err
	}
	return string(b) == OwnerValue+"\n", nil
}

// cleanStaging classifies leftovers in crash-safe buckets: stray marker TMP
// FILES in our namespace → removed; EMPTY staging dir (crash before the
// marker landed - carries no data) → removed; complete-marker staging dir
// (ours) → removed; anything else → warned, never removed. A
// removal/inspection failure is an error - installing next to leftovers we
// cannot clear risks compounding partial state.
func cleanStaging(parent string) (warnings []string, err error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), markerTmpPrefix) && strings.HasSuffix(e.Name(), ".tmp") {
			if rmErr := os.Remove(filepath.Join(parent, e.Name())); rmErr != nil {
				return nil, fmt.Errorf("could not remove the temporary file %s left by an earlier install: %w", e.Name(), rmErr)
			}
			continue
		}
		if !e.IsDir() && strings.HasPrefix(e.Name(), probePrefix) {
			if rmErr := os.Remove(filepath.Join(parent, e.Name())); rmErr != nil {
				return nil, fmt.Errorf("could not remove the file %s left by an earlier check that the directory is writable: %w", e.Name(), rmErr)
			}
			continue
		}
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		inner, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("could not check the temporary directory %s left by an earlier install: %w", dir, err)
		}
		remove := len(inner) == 0
		if !remove {
			ours, err := markerComplete(dir)
			if err != nil {
				return nil, fmt.Errorf("could not check the temporary directory %s left by an earlier install: %w", dir, err)
			}
			remove = ours
		}
		if remove {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				return nil, fmt.Errorf("could not remove the temporary directory %s left by an earlier install: %w", dir, rmErr)
			}
			continue
		}
		warnings = append(warnings, fmt.Sprintf("left %s in place: rancher-cli does not recognize it as a temporary directory of its own", dir))
	}
	return warnings, nil
}

// stageHook, when non-nil, runs before each staged file write, and
// postStageHook after population but before staged-tree validation - the
// deterministic failure/corruption seams for tests. Always nil in production.
var (
	stageHook     func(relPath string) error
	postStageHook func(stagingDir string) error
)

// validateStaged proves the completed staged tree is exactly what will be
// advertised: complete owner marker, our install record, and a canonical
// digest that matches the stamped value.
func validateStaged(staging string) error {
	ours, err := markerComplete(staging)
	if err != nil {
		return err
	}
	if !ours {
		return fmt.Errorf("the file %s, which marks the directory as installed by rancher-cli, is missing or incomplete", OwnerMarkerFile)
	}
	isOurs, prov, err := owned(staging)
	if err != nil {
		return err
	}
	if !isOurs {
		return fmt.Errorf("the SKILL.md to install does not record that rancher-cli installed it")
	}
	tree, err := ReadTree(staging)
	if err != nil {
		return err
	}
	digest, err := DigestTree(tree)
	if err != nil {
		return err
	}
	if digest != prov.ContentDigest {
		return fmt.Errorf("the digest of the files to install (%s) does not match the digest recorded in SKILL.md (%s)", digest, prov.ContentDigest)
	}
	return nil
}

// Install writes the rendered tree at the leased target. Guarantee:
// ABSENT-OR-CONSISTENT - the final rename is atomic; a crash between
// remove and rename leaves the target absent, repaired by re-running.
// Every pre-rename failure removes the staging dir; a cleanup failure is
// reported alongside the primary error, never swallowed. SKILL.md is
// written FIRST so the leftover staging entry is owner-marked from its first file -
// an interrupted population can never strand foreign-looking leftovers.
func (l *Lease) Install(cliVersion string, force bool) ([]string, error) {
	warnings, err := cleanStaging(filepath.Dir(l.target.Dir))
	if err != nil {
		return warnings, err
	}
	return l.install(cliVersion, force, warnings)
}

// install is Install after the leftovers in the parent were cleaned; warnings
// are the ones that cleaning returned, and install returns them with its own.
func (l *Lease) install(cliVersion string, force bool, warnings []string) ([]string, error) {
	t := l.target
	parent := filepath.Dir(t.Dir)
	files, err := Rendered(cliVersion)
	if err != nil {
		return warnings, err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return warnings, err
	}
	staging := filepath.Join(parent, stagingPrefix+hex.EncodeToString(suffix[:]))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return warnings, err
	}
	cleanup := func(primary error) error {
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			return fmt.Errorf("%w (removing the temporary directory %s also failed: %v)", primary, staging, rmErr)
		}
		return primary
	}
	// Owner marker FIRST - temp file written OUTSIDE staging (in the
	// parent, our marker-tmp namespace), then renamed into the still-EMPTY
	// staging dir. The rename is atomic, so staging transitions
	// empty→marked with no intermediate non-empty-unmarked state: every
	// crash shape is a removable bucket (stray marker tmp file, empty
	// staging dir, or marker-complete staging dir).
	if stageHook != nil {
		if err := stageHook(OwnerMarkerFile); err != nil {
			return warnings, cleanup(err)
		}
	}
	tmp := filepath.Join(parent, markerTmpPrefix+hex.EncodeToString(suffix[:])+".tmp")
	if err := os.WriteFile(tmp, files[OwnerMarkerFile], 0o644); err != nil {
		return warnings, cleanup(err)
	}
	if err := os.Rename(tmp, filepath.Join(staging, OwnerMarkerFile)); err != nil {
		_ = os.Remove(tmp)
		return warnings, cleanup(err)
	}
	rest := make([]string, 0, len(files)-1)
	for p := range files {
		if p != OwnerMarkerFile {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	for _, path := range rest {
		if stageHook != nil {
			if err := stageHook(path); err != nil {
				return warnings, cleanup(err)
			}
		}
		dst := filepath.Join(staging, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return warnings, cleanup(err)
		}
		if err := os.WriteFile(dst, files[path], 0o644); err != nil {
			return warnings, cleanup(err)
		}
	}
	if postStageHook != nil {
		if err := postStageHook(staging); err != nil {
			return warnings, cleanup(err)
		}
	}
	// Validate the COMPLETED on-disk staged tree before touching the
	// target: ownership marker, install record, and recomputed canonical
	// digest must all hold, or the target stays exactly as it was.
	if err := validateStaged(staging); err != nil {
		return warnings, cleanup(fmt.Errorf("the files to install failed validation: %w", err))
	}
	// revalidate the target under the held lock, then remove and swap
	if err := validateTarget(t, "install", force); err != nil {
		return warnings, cleanup(err)
	}
	if err := os.RemoveAll(t.Dir); err != nil {
		return warnings, cleanup(err)
	}
	if renameHook != nil {
		if err := renameHook(); err != nil {
			return warnings, cleanup(err)
		}
	}
	err = os.Rename(staging, t.Dir)
	return warnings, err
}

// InstallIfChanged is Install for a target that may already hold what Install
// would write. A target is current when force is false and every file in it,
// SKILL.md and the owner marker included, has the same path and the same
// bytes as the rendered tree. A current target is not touched, apart from the
// removal of leftovers in its parent; changed is false. Any other state,
// including a failed read, is installed as Install does and reports the same
// errors. The bytes are compared, not the version or the recorded digest: two
// builds can carry one version and different skill text.
func (l *Lease) InstallIfChanged(cliVersion string, force bool) (changed bool, warnings []string, err error) {
	warnings, err = cleanStaging(filepath.Dir(l.target.Dir))
	if err != nil {
		return false, warnings, err
	}
	if !force && l.current(cliVersion) {
		return false, warnings, nil
	}
	warnings, err = l.install(cliVersion, force, warnings)
	return err == nil, warnings, err
}

// current reports whether the target holds exactly the rendered tree. It
// reads the target and writes nothing.
func (l *Lease) current(cliVersion string) bool {
	want, err := Rendered(cliVersion)
	if err != nil {
		return false
	}
	have, err := ReadTree(l.target.Dir)
	if err != nil {
		return false
	}
	return maps.EqualFunc(have, want, bytes.Equal)
}

// removeParentHook, when non-nil, replaces the removal of the emptied parent
// directory - the failure seam for tests. Always nil in production.
var removeParentHook func(path string) error

// Uninstall removes the leased owned installation. Absent = no-op success -
// stable under the held OS-user-cache lock: a concurrent rancher install
// of the same target serializes on the same lock file, so absence cannot
// flip mid-operation. After it removed the target, the parent (the harness's
// skills directory) is removed too when it is empty; an empty parent that
// cannot be removed adds a warning and does not fail the uninstall.
func (l *Lease) Uninstall() (removed bool, warnings []string, err error) {
	t := l.target
	if err := validateTarget(t, "uninstall", false); err != nil {
		return false, nil, err
	}
	if _, err := os.Stat(t.Dir); os.IsNotExist(err) {
		return false, nil, nil
	}
	if err := os.RemoveAll(t.Dir); err != nil {
		return false, nil, err
	}
	if w := removeEmptyParent(filepath.Dir(t.Dir)); w != "" {
		warnings = append(warnings, w)
	}
	return true, warnings, nil
}

// removeEmptyParent removes dir when it is a real, empty directory and
// returns "" for every state it leaves alone: a symlink, a non-directory, a
// directory that holds any entry (leftovers of the install included). It
// returns a warning when an empty directory could not be removed or the state
// of dir could not be read.
func removeEmptyParent(dir string) string {
	warn := func(err error) string {
		return fmt.Sprintf("could not remove the empty directory %s: %v", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		return warn(err)
	}
	if !info.IsDir() {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return warn(err)
	}
	if len(entries) > 0 {
		return ""
	}
	remove := os.Remove
	if removeParentHook != nil {
		remove = removeParentHook
	}
	rmErr := remove(dir)
	if rmErr == nil || os.IsNotExist(rmErr) {
		return ""
	}
	// Something else may have put an entry into dir since it was read.
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return ""
	}
	return warn(rmErr)
}

// StatusTarget classifies one target. Read-only and lock-free.
func StatusTarget(t Target, cliVersion string) (Verdict, string, error) {
	if err := refuseSymlink(t.Dir); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(t.Dir); os.IsNotExist(err) {
		return VerdictAbsent, "", nil
	}
	isOurs, rec, err := owned(t.Dir)
	if err != nil {
		return "", "", err
	}
	if !isOurs {
		return VerdictForeign, "", nil
	}
	onDisk, err := ReadTree(t.Dir)
	if err != nil {
		if errors.Is(err, ErrNonRegular) {
			// A non-regular entry inside an owned tree IS a modification;
			// nothing was read through it (ReadTree rejects before reading).
			return VerdictModified, rec.CLIVersion, nil
		}
		return "", "", err
	}
	digest, err := DigestTree(onDisk)
	if err != nil {
		return "", "", err
	}
	if digest != rec.ContentDigest {
		return VerdictModified, rec.CLIVersion, nil
	}
	if rec.CLIVersion != cliVersion {
		return VerdictSkew, rec.CLIVersion, nil
	}
	return VerdictInSync, rec.CLIVersion, nil
}

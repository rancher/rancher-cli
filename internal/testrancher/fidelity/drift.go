package fidelity

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The drift run compares an upcoming Rancher minor with the community
// goldens. Nothing here asserts per probe: every probe whose MaxMinor allows
// the minor runs, every place its answer differs from its golden is a
// Difference, and the differences are matched against a tracked file of known
// ones and written as one report.

// Difference is one place where a probe's answer differs from its golden,
// after both went through the goldens' own normalization.
type Difference struct {
	Probe string
	// Pointer is an RFC 6901 JSON Pointer into the probe's observations;
	// its first segment is the observation name.
	Pointer string
	// Want and Got are the canonical encoding (json.Marshal of the decoded
	// value) of what each side holds at Pointer; nil when that side has
	// nothing there, the bytes null for a JSON null.
	Want json.RawMessage
	Got  json.RawMessage
	// WaitRanOut marks every difference of an observation whose wait for a
	// status ran out: its whole answer is the last one, not a settled one.
	WaitRanOut bool
}

// String is the one-line form the test log shows.
func (d Difference) String() string {
	return fmt.Sprintf("%s: want %s, got %s", d.Pointer, describeRaw(d.Want), describeRaw(d.Got))
}

// Differences compares two recordings of one probe place by place. The
// document compared per side is an object of observation name to {status,
// contentType, body, text}, with an absent body or text left out. Objects
// are compared key by key, arrays element by element when their lengths
// agree and otherwise as one difference, scalars and values of different
// JSON types as one difference at that node. completed is false when the
// probe returned an error: golden observations missing from the results are
// then not compared. OracleImage is not compared. The result is sorted by
// pointer. Numbers are compared as float64, the type goldens and results are
// decoded into, so integers that differ only beyond 2^53 compare equal.
func Differences(probe string, want, got Golden, completed bool) []Difference {
	wantDoc := observationDocument(want)
	gotDoc := observationDocument(got)
	ranOut := map[string]bool{}
	for _, o := range got.Observations {
		if o.WaitRanOut {
			ranOut[o.Name] = true
		}
	}
	names := map[string]bool{}
	for name := range wantDoc {
		names[name] = true
	}
	for name := range gotDoc {
		names[name] = true
	}
	var out []Difference
	for name := range names {
		w, wantOK := wantDoc[name]
		g, gotOK := gotDoc[name]
		if wantOK && !gotOK && !completed {
			continue
		}
		start := len(out)
		walkDifferences(&out, probe, "/"+escapePointerSegment(name), w, wantOK, g, gotOK)
		if ranOut[name] {
			for i := start; i < len(out); i++ {
				out[i].WaitRanOut = true
			}
		}
	}
	slices.SortFunc(out, func(a, b Difference) int { return strings.Compare(a.Pointer, b.Pointer) })
	return out
}

func walkDifferences(out *[]Difference, probe, pointer string, want any, wantOK bool, got any, gotOK bool) {
	if !wantOK && !gotOK {
		return
	}
	if wantOK != gotOK {
		*out = append(*out, difference(probe, pointer, want, wantOK, got, gotOK))
		return
	}
	switch w := want.(type) {
	case map[string]any:
		if g, ok := got.(map[string]any); ok {
			keys := map[string]bool{}
			for k := range w {
				keys[k] = true
			}
			for k := range g {
				keys[k] = true
			}
			for k := range keys {
				wv, wvOK := w[k]
				gv, gvOK := g[k]
				walkDifferences(out, probe, pointer+"/"+escapePointerSegment(k), wv, wvOK, gv, gvOK)
			}
			return
		}
	case []any:
		if g, ok := got.([]any); ok && len(w) == len(g) {
			for i := range w {
				walkDifferences(out, probe, pointer+"/"+strconv.Itoa(i), w[i], true, g[i], true)
			}
			return
		}
	}
	if !reflect.DeepEqual(want, got) {
		*out = append(*out, difference(probe, pointer, want, true, got, true))
	}
}

func difference(probe, pointer string, want any, wantOK bool, got any, gotOK bool) Difference {
	d := Difference{Probe: probe, Pointer: pointer}
	if wantOK {
		d.Want = canonicalValue(want)
	}
	if gotOK {
		d.Got = canonicalValue(got)
	}
	return d
}

// canonicalValue is the encoding Want and Got hold: the decoded value
// re-encoded with sorted keys and no extra whitespace.
func canonicalValue(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		// A decoded JSON value always encodes.
		return nil
	}
	return raw
}

// canonicalRaw decodes one JSON value and encodes it canonically, so the
// indentation of a value in a file never matters. nil stays nil (nothing at
// the pointer).
func canonicalRaw(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return canonicalValue(v), nil
}

// observationDocument is one side of the comparison: observation name to
// {status, contentType, body, text} as the omitempty shape of
// ObservationResult encodes them.
func observationDocument(g Golden) map[string]any {
	doc := make(map[string]any, len(g.Observations))
	for _, o := range g.Observations {
		raw, err := json.Marshal(o)
		if err != nil {
			// An observation result always encodes; a miss here shows up as
			// an observation on one side only.
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		delete(m, "name")
		doc[o.Name] = m
	}
	return doc
}

// escapePointerSegment applies RFC 6901 escaping: ~ as ~0 and / as ~1.
func escapePointerSegment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// documentNodes maps the escaped pointer of every node of an observation
// document, containers included, to the node's canonical value.
func documentNodes(doc map[string]any) map[string]json.RawMessage {
	nodes := map[string]json.RawMessage{}
	for name, v := range doc {
		addNodes(nodes, "/"+escapePointerSegment(name), v)
	}
	return nodes
}

func addNodes(nodes map[string]json.RawMessage, pointer string, v any) {
	nodes[pointer] = canonicalValue(v)
	switch n := v.(type) {
	case map[string]any:
		for k, child := range n {
			addNodes(nodes, pointer+"/"+escapePointerSegment(k), child)
		}
	case []any:
		for i, child := range n {
			addNodes(nodes, pointer+"/"+strconv.Itoa(i), child)
		}
	}
}

// Rulings of a known difference. The empty ruling means none yet.
const (
	RulingFakeFollows   = "fake-follows"
	RulingCLIChange     = "cli-change"
	RulingExpectedAtGA  = "expected-at-ga"
	RulingNewerPatch    = "newer-patch"
	RulingRancherDefect = "rancher-defect"
)

// Causes of a known difference. Install is the cause of a difference that
// comes from running the Rancher chart in a cluster instead of the single
// container image; a helm-check run gives it to every new difference that the
// downstream k3s version does not explain.
const (
	CauseRancher      = "rancher"
	CauseKubernetes   = "kubernetes"
	CauseInstall      = "install"
	CauseUndetermined = "undetermined"
)

// Profiles of a drift run: an upcoming minor compared with the community
// goldens, or the helm-check, which boots the community pin's image with the
// Rancher chart and compares it with the goldens recorded from the same
// image.
const (
	ProfileUpcoming  = "upcoming"
	ProfileHelmCheck = "helm-check"
)

var (
	knownRulings = []string{RulingFakeFollows, RulingCLIChange, RulingExpectedAtGA, RulingNewerPatch, RulingRancherDefect}
	knownCauses  = []string{CauseRancher, CauseKubernetes, CauseInstall, CauseUndetermined}
)

// oneOf writes a list for a message: "a, b or c".
func oneOf(items ...string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// The two suites a drift run reports on.
const (
	SuiteOracle     = "TestOracle"
	SuiteDownstream = "TestDownstream"
)

// DriftSuites lists the suites in report order.
var DriftSuites = []string{SuiteOracle, SuiteDownstream}

// KnownEntry is one known difference. Want or Got left out means nothing at
// the pointer on that side; the JSON null is a value. With GotVaries the
// upcoming value changes from build to build: the entry has no Got and
// matches any value present at the pointer.
type KnownEntry struct {
	Probe     string          `json:"probe"`
	Pointer   string          `json:"pointer"`
	Want      json.RawMessage `json:"want,omitempty"`
	Got       json.RawMessage `json:"got,omitempty"`
	GotVaries bool            `json:"gotVaries,omitempty"`
	FirstSeen string          `json:"firstSeen"`
	Cause     string          `json:"cause"`
	Ruling    string          `json:"ruling"`
	URL       string          `json:"url,omitempty"`
}

// KnownFile is the tracked file that describes the differences of one
// upcoming minor.
type KnownFile struct {
	Minor   string       `json:"minor"`
	Entries []KnownEntry `json:"entries"`
}

// knownURLRepositories is the exact list of repositories a known entry's url
// may name; any other repository is refused.
var knownURLRepositories = []string{
	"rancher/rancher",
	"rancher/rancher-cli",
	"rancher/webhook",
	"rancher/steve",
	"rancher/dashboard",
	"kubernetes/kubernetes",
	"kubernetes-sigs/controller-runtime",
	"k3s-io/k3s",
}

var (
	// The repositories hold only letters, digits, "-" and "/", so they join
	// into the expression without quoting.
	knownURLPattern = regexp.MustCompile(`^https://github\.com/(` +
		strings.Join(knownURLRepositories, "|") +
		`)/(issues|pull)/[1-9][0-9]*$`)
	firstSeenPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	minorPattern     = regexp.MustCompile(`^[1-9][0-9]*\.(0|[1-9][0-9]*)$`)
	// pointerPattern is an RFC 6901 pointer with at least one segment: every
	// segment starts with "/" and holds no "~" that is not ~0 or ~1.
	pointerPattern = regexp.MustCompile(`^(/([^/~]|~[01])*)+$`)
)

// encodeDriftJSON is the encoder of the known file and the report: two-space
// indentation, a final newline, and every non-ASCII character written as
// \uXXXX.
func encodeDriftJSON(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	var out bytes.Buffer
	for _, r := range string(raw) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.Bytes(), nil
}

// ParseKnownFile decodes and validates a known file's bytes: unknown fields,
// the minor, each entry's pointer, enums, url, firstSeen, got against
// gotVaries, the order, duplicates and the canonical form. The entries it
// returns hold canonical want and got values. It reads no golden;
// CheckGoldens does.
func ParseKnownFile(raw []byte) (KnownFile, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var k KnownFile
	if err := dec.Decode(&k); err != nil {
		return KnownFile{}, fmt.Errorf("decoding the known-differences file: %w", err)
	}
	var errs []error
	if !minorPattern.MatchString(k.Minor) {
		errs = append(errs, fmt.Errorf("minor %q is not <major>.<minor>", k.Minor))
	}
	if k.Entries == nil {
		errs = append(errs, errors.New("entries is missing; write an empty list for none"))
	}
	for i, e := range k.Entries {
		where := fmt.Sprintf("entry %d (%s %s)", i, e.Probe, e.Pointer)
		for _, err := range validateKnownEntry(e) {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		if i == 0 {
			continue
		}
		prev := k.Entries[i-1]
		switch c := cmp.Or(strings.Compare(prev.Probe, e.Probe), strings.Compare(prev.Pointer, e.Pointer)); {
		case c == 0:
			errs = append(errs, fmt.Errorf("%s: duplicate entry; one entry per probe and pointer", where))
		case c > 0:
			errs = append(errs, fmt.Errorf("%s: entries are not sorted by probe, then pointer", where))
		}
	}
	if len(errs) > 0 {
		return KnownFile{}, errors.Join(errs...)
	}
	canonical, err := encodeDriftJSON(k)
	if err != nil {
		return KnownFile{}, fmt.Errorf("encoding the known-differences file: %w", err)
	}
	if !bytes.Equal(canonical, raw) {
		return KnownFile{}, errors.New("the file is not in canonical form: two-space indentation, one final newline, entries in the field order of the format, non-ASCII written as \\uXXXX; fix it by hand, or copy entries from the known, knownNoRuling, noLongerSeen or notChecked lists of a drift report, which are already in this form")
	}
	// Entries are compared in canonical form, so the file's indentation and
	// key order inside a value never matter. The values were checked above.
	for i := range k.Entries {
		k.Entries[i].Want, _ = canonicalRaw(k.Entries[i].Want)
		k.Entries[i].Got, _ = canonicalRaw(k.Entries[i].Got)
	}
	return k, nil
}

func validateKnownEntry(e KnownEntry) []error {
	var errs []error
	if e.Probe == "" {
		errs = append(errs, errors.New("probe is empty"))
	}
	if !pointerPattern.MatchString(e.Pointer) {
		errs = append(errs, fmt.Errorf("pointer %q does not name a place in an observation; a pointer starts with / and every ~ is ~0 or ~1", e.Pointer))
	}
	for field, raw := range map[string]json.RawMessage{"want": e.Want, "got": e.Got} {
		if raw != nil {
			if _, err := canonicalRaw(raw); err != nil {
				errs = append(errs, fmt.Errorf("%s is not JSON: %w", field, err))
			}
		}
	}
	if e.GotVaries && e.Got != nil {
		errs = append(errs, errors.New("got and gotVaries are both set; gotVaries means no got"))
	}
	if !firstSeenPattern.MatchString(e.FirstSeen) {
		errs = append(errs, fmt.Errorf("firstSeen %q is not 7 to 40 lowercase hex digits", e.FirstSeen))
	}
	if !slices.Contains(knownCauses, e.Cause) {
		errs = append(errs, fmt.Errorf("cause %q is not %s", e.Cause, oneOf(knownCauses...)))
	}
	if e.Ruling != "" && !slices.Contains(knownRulings, e.Ruling) {
		errs = append(errs, fmt.Errorf("ruling %q is not %s", e.Ruling, strings.Join(knownRulings, ", ")+" or empty"))
	}
	switch {
	case e.Ruling == RulingRancherDefect && e.URL == "":
		errs = append(errs, errors.New("ruling rancher-defect needs a url"))
	case e.URL != "" && e.Ruling == "":
		errs = append(errs, errors.New("url is set with an empty ruling; a url goes with rancher-defect or cli-change"))
	case e.URL != "" && e.Ruling != RulingRancherDefect && e.Ruling != RulingCLIChange:
		errs = append(errs, fmt.Errorf("ruling %q takes no url; a url goes with rancher-defect or cli-change", e.Ruling))
	case e.URL != "" && !knownURLPattern.MatchString(e.URL):
		errs = append(errs, fmt.Errorf("url %q is not an issue or pull request of an allowed repository (%s)", e.URL, strings.Join(knownURLRepositories, ", ")))
	}
	return errs
}

// CheckGoldens checks every entry of a parsed file against the current
// goldens: the probe exists, and want equals the golden's value at the pointer (read from
// downstreamDir for a downstream probe, oracleDir otherwise), or an absent
// want means nothing is there. A golden re-record that moves a value fails
// until the entry is updated.
func (k KnownFile) CheckGoldens(probes []Probe, oracleDir, downstreamDir string) error {
	byName := make(map[string]Probe, len(probes))
	for _, p := range probes {
		byName[p.Name] = p
	}
	var errs []error
	nodesByProbe := map[string]map[string]json.RawMessage{}
	for i, e := range k.Entries {
		where := fmt.Sprintf("entry %d (%s %s)", i, e.Probe, e.Pointer)
		p, ok := byName[e.Probe]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no probe with that name", where))
			continue
		}
		nodes, ok := nodesByProbe[p.Name]
		if !ok {
			dir := oracleDir
			if p.Downstream {
				dir = downstreamDir
			}
			golden, err := LoadGolden(GoldenPath(dir, p.Name))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
				continue
			}
			nodes = documentNodes(observationDocument(golden))
			nodesByProbe[p.Name] = nodes
		}
		if have := nodes[e.Pointer]; !bytes.Equal(have, e.Want) {
			errs = append(errs, fmt.Errorf("%s: want %s differs from the golden's %s; update the entry's want or remove the entry", where, describeRaw(e.Want), describeRaw(have)))
		}
	}
	return errors.Join(errs...)
}

func describeRaw(raw json.RawMessage) string {
	if raw == nil {
		return "nothing"
	}
	return string(raw)
}

// KnownFileLoad is what reading a known file produced: the parsed file, or
// Exists false when there is no file, or the read or validation error.
type KnownFileLoad struct {
	File   KnownFile
	Exists bool
	Err    error
}

// LoadKnownFile reads and validates the known file at path. A missing file
// is not an error: Exists is false and every difference is new.
func LoadKnownFile(path string) KnownFileLoad {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return KnownFileLoad{}
	}
	if err != nil {
		return KnownFileLoad{Exists: true, Err: fmt.Errorf("reading %s: %w", path, err)}
	}
	k, err := ParseKnownFile(raw)
	if err != nil {
		return KnownFileLoad{Exists: true, Err: fmt.Errorf("%s: %w", path, err)}
	}
	return KnownFileLoad{File: k, Exists: true}
}

// The six drift variables scripts/e2e-fidelity sets for a drift run, and the
// profile variable it also sets for the test binary.
const (
	EnvDriftProfile     = "E2E_FIDELITY_DRIFT_PROFILE"
	EnvDriftReport      = "E2E_FIDELITY_DRIFT_REPORT"
	EnvDriftKnown       = "E2E_FIDELITY_DRIFT_KNOWN"
	EnvDriftStarted     = "E2E_FIDELITY_DRIFT_STARTED"
	EnvGoldenImage      = "E2E_FIDELITY_GOLDEN_IMAGE"
	EnvGoldenK3sImage   = "E2E_FIDELITY_GOLDEN_K3S_IMAGE"
	EnvDownstreamImage  = "E2E_FIDELITY_DOWNSTREAM_IMAGE"
	envFidelityMinor    = "E2E_FIDELITY_MINOR"
	envFidelityOracleIm = "E2E_FIDELITY_ORACLE_IMAGE"
)

var driftVariables = []string{
	EnvDriftReport, EnvDriftKnown, EnvDriftStarted,
	EnvGoldenImage, EnvGoldenK3sImage, EnvDownstreamImage,
}

// DriftConfig is a drift run's configuration. OracleImage is the upcoming
// Rancher image under test and DownstreamImage the downstream's k3s image;
// the golden images are what the community goldens were recorded against.
type DriftConfig struct {
	Minor           string
	ReportPath      string
	KnownPath       string
	StartedAt       string
	GoldenImage     string
	GoldenK3sImage  string
	DownstreamImage string
	OracleImage     string
	// Profile is ProfileUpcoming, ProfileHelmCheck or empty, which means
	// ProfileUpcoming.
	Profile string
}

// reportProfile is the profile the report names.
func (c DriftConfig) reportProfile() string {
	if c.Profile == "" {
		return ProfileUpcoming
	}
	return c.Profile
}

// DriftConfigFromEnv reads the six drift variables. None set means drift
// mode is off. All set means on. Some but not all, or any set while goldens
// are being recorded (update), is an error, and so is a missing
// E2E_FIDELITY_MINOR. The profile variable is read only in drift mode: empty
// means the upcoming profile, and a value other than upcoming or helm-check
// is an error.
func DriftConfigFromEnv(getenv func(string) string, update bool) (DriftConfig, bool, error) {
	var missing []string
	for _, name := range driftVariables {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == len(driftVariables) {
		return DriftConfig{}, false, nil
	}
	if update {
		return DriftConfig{}, false, errors.New("drift mode compares only; it never records goldens (-update is set)")
	}
	if len(missing) > 0 {
		return DriftConfig{}, false, fmt.Errorf("drift mode needs every drift variable; missing: %s; scripts/e2e-fidelity sets them when E2E_FIDELITY_DRIFT=1", strings.Join(missing, ", "))
	}
	minor := getenv(envFidelityMinor)
	if minor == "" {
		return DriftConfig{}, false, errors.New("drift mode needs E2E_FIDELITY_MINOR")
	}
	profile := getenv(EnvDriftProfile)
	if profile != "" && profile != ProfileUpcoming && profile != ProfileHelmCheck {
		return DriftConfig{}, false, fmt.Errorf("%s=%s is not %s or %s", EnvDriftProfile, profile, ProfileUpcoming, ProfileHelmCheck)
	}
	return DriftConfig{
		Minor:           minor,
		Profile:         profile,
		ReportPath:      getenv(EnvDriftReport),
		KnownPath:       getenv(EnvDriftKnown),
		StartedAt:       getenv(EnvDriftStarted),
		GoldenImage:     getenv(EnvGoldenImage),
		GoldenK3sImage:  getenv(EnvGoldenK3sImage),
		DownstreamImage: getenv(EnvDownstreamImage),
		OracleImage:     getenv(envFidelityOracleIm),
	}, true, nil
}

// RancherVersion is the answer of /rancherversion.
type RancherVersion struct {
	Version   string
	GitCommit string
}

// FetchRancherVersion reads /rancherversion through the target's client.
// An answer without Version or GitCommit is an error, never an empty field.
// The errors do not name the path; the harness failure that carries them does.
func FetchRancherVersion(ctx context.Context, tgt Target) (RancherVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+"/rancherversion", nil)
	if err != nil {
		return RancherVersion{}, err
	}
	resp, err := tgt.Client().Do(req)
	if err != nil {
		return RancherVersion{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return RancherVersion{}, fmt.Errorf("reading the answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return RancherVersion{}, fmt.Errorf("answered status %d", resp.StatusCode)
	}
	var v RancherVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return RancherVersion{}, fmt.Errorf("decoding the answer: %w", err)
	}
	if v.Version == "" || v.GitCommit == "" {
		return RancherVersion{}, errors.New("the answer has no Version or no GitCommit")
	}
	return v, nil
}

// ProbeRun is one probe's drift result: its differences and the error it
// returned, if any (the differences then cover only the observations that
// completed). A probe is not checked when the server's minor is newer than its
// MaxMinor, or when it sets OwnPairOnly and the server is not the pair
// scripts/e2e-fidelity booted or kept: NotCheckedReason says why, and there
// are no differences or error.
type ProbeRun struct {
	Suite            string
	Probe            string
	Downstream       bool
	Differences      []Difference
	Err              string
	NotCheckedReason string
}

// CLITestRun is one CLI test's result. Started is true once the CLI command
// was about to run; a failure before that is a harness failure.
type CLITestRun struct {
	Name    string `json:"name"`
	Started bool   `json:"started"`
	Passed  bool   `json:"passed"`
}

// DriftCollector gathers what a drift run's tests observe. Its methods are
// safe for concurrent use; FinishDriftRun reads the fields after the tests
// ended.
type DriftCollector struct {
	mu       sync.Mutex
	tests    int
	failed   []string
	probes   []ProbeRun
	cliTests []CLITestRun
}

// Test records that a test or subtest ran and whether it failed.
func (c *DriftCollector) Test(name string, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tests++
	if failed {
		c.failed = append(c.failed, name)
	}
}

// Probe records one probe's differences and error in a suite.
func (c *DriftCollector) Probe(suite, probe string, downstream bool, diffs []Difference, err error) {
	run := ProbeRun{Suite: suite, Probe: probe, Downstream: downstream, Differences: diffs}
	if err != nil {
		run.Err = err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = append(c.probes, run)
}

// AddNotCheckedProbe records a probe that is not checked in a suite, because
// the server's minor is newer than its MaxMinor. reason says why.
func (c *DriftCollector) AddNotCheckedProbe(suite, probe string, downstream bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = append(c.probes, ProbeRun{Suite: suite, Probe: probe, Downstream: downstream, NotCheckedReason: reason})
}

// cliTest returns the record of a CLI test, adding it on first use. The
// caller holds the lock.
func (c *DriftCollector) cliTest(name string) *CLITestRun {
	for i := range c.cliTests {
		if c.cliTests[i].Name == name {
			return &c.cliTests[i]
		}
	}
	c.cliTests = append(c.cliTests, CLITestRun{Name: name})
	return &c.cliTests[len(c.cliTests)-1]
}

// CLIStarted records that a CLI test is about to run its CLI command.
func (c *DriftCollector) CLIStarted(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cliTest(name).Started = true
}

// CLITest records a CLI test's final result.
func (c *DriftCollector) CLITest(name string, passed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cliTest(name).Passed = passed
}

// HarnessFailure is a failure of the run itself rather than a difference
// from the goldens; any one makes the run exit 1.
type HarnessFailure struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ErroredProbe is a probe that returned an error.
type ErroredProbe struct {
	Probe string `json:"probe"`
	Error string `json:"error"`
}

// NotCheckedProbe is a probe that is not checked, because the server's minor
// is newer than its MaxMinor.
type NotCheckedProbe struct {
	Probe  string `json:"probe"`
	Reason string `json:"reason"`
}

// SuiteResult is how many probes a suite ran.
type SuiteResult struct {
	Name   string `json:"name"`
	Probes int    `json:"probes"`
}

// NewDifference is a difference no known entry matches, in the file's entry
// format with the ruling empty.
type NewDifference struct {
	Entry      KnownEntry `json:"entry"`
	WaitRanOut bool       `json:"waitRanOut"`
}

// DriftCounts are the report's counts, in the order the script's summary
// line prints them (harness failures are not part of it). NotChecked counts
// the known entries that are not checked; NotCheckedProbes counts the probes
// that are not checked.
type DriftCounts struct {
	New               int `json:"new"`
	WaitRanOut        int `json:"waitRanOut"`
	Known             int `json:"known"`
	KnownNoRuling     int `json:"knownNoRuling"`
	NoLongerSeen      int `json:"noLongerSeen"`
	NotChecked        int `json:"notChecked"`
	NotCheckedProbes  int `json:"notCheckedProbes"`
	Errored           int `json:"errored"`
	FailedCLITests    int `json:"failedCLITests"`
	SuitesWithNoProbe int `json:"suitesWithNoProbe"`
	HarnessFailures   int `json:"harnessFailures"`
}

// Report is the drift run's result file.
type Report struct {
	Minor             string            `json:"minor"`
	Profile           string            `json:"profile"`
	StartedAt         string            `json:"startedAt"`
	OracleImage       string            `json:"oracleImage"`
	DownstreamImage   string            `json:"downstreamImage"`
	GoldenOracleImage string            `json:"goldenOracleImage"`
	GoldenK3sImage    string            `json:"goldenK3sImage"`
	RancherVersion    string            `json:"rancherVersion"`
	GitCommit         string            `json:"gitCommit"`
	KnownFile         string            `json:"knownFile"`
	KnownFileExists   bool              `json:"knownFileExists"`
	New               []NewDifference   `json:"new"`
	Known             []KnownEntry      `json:"known"`
	KnownNoRuling     []KnownEntry      `json:"knownNoRuling"`
	NoLongerSeen      []KnownEntry      `json:"noLongerSeen"`
	NotChecked        []KnownEntry      `json:"notChecked"`
	NotCheckedProbes  []NotCheckedProbe `json:"notCheckedProbes"`
	Errored           []ErroredProbe    `json:"errored"`
	CLITests          []CLITestRun      `json:"cliTests"`
	Suites            []SuiteResult     `json:"suites"`
	HarnessFailures   []HarnessFailure  `json:"harnessFailures"`
	Counts            DriftCounts       `json:"counts"`
	Clean             bool              `json:"clean"`
	Complete          bool              `json:"complete"`
}

// Compare matches the run's differences against the known entries and
// builds the report. A difference is known when probe, pointer and want
// equal an entry's and so does got, unless the entry sets gotVaries and the
// run has a value at the pointer; otherwise it is new. An entry no
// difference matches is no longer seen, except when its probe errored or did
// not run: then it is not checked. The errored probe's completed
// observations are compared like any other. A probe that is not checked because
// of its MaxMinor is listed in NotCheckedProbes. It is not an error. Its known
// entries are listed in NotChecked. A suite whose every probe was not checked
// is not counted in SuitesWithNoProbe. A difference whose observation's wait
// ran out is matched like any other and, when new, carries the flag.
// header is a report with the identity fields filled (images, versions, the
// known file's path and whether it exists); Compare adds the rest. The
// entries of known are in canonical form, as ParseKnownFile returns them.
func Compare(known KnownFile, header Report, probes []ProbeRun, cliTests []CLITestRun, harness []HarnessFailure, testsRan int) Report {
	r := header
	r.New = []NewDifference{}
	r.Known = []KnownEntry{}
	r.KnownNoRuling = []KnownEntry{}
	r.NoLongerSeen = []KnownEntry{}
	r.NotChecked = []KnownEntry{}
	r.NotCheckedProbes = []NotCheckedProbe{}
	r.Errored = []ErroredProbe{}
	r.CLITests = []CLITestRun{}
	r.Suites = []SuiteResult{}
	r.HarnessFailures = append([]HarnessFailure{}, harness...)
	matched := make([]bool, len(known.Entries))
	ran := map[string]bool{}
	errored := map[string]bool{}
	suiteProbes := map[string]int{}
	suiteNotChecked := map[string]int{}

	for _, pr := range probes {
		if pr.NotCheckedReason != "" {
			suiteNotChecked[pr.Suite]++
			r.NotCheckedProbes = append(r.NotCheckedProbes, NotCheckedProbe{Probe: pr.Probe, Reason: pr.NotCheckedReason})
			continue
		}
		ran[pr.Probe] = true
		suiteProbes[pr.Suite]++
		if pr.Err != "" {
			errored[pr.Probe] = true
			r.Errored = append(r.Errored, ErroredProbe{Probe: pr.Probe, Error: pr.Err})
		}
		for _, d := range pr.Differences {
			idx := -1
			for i, e := range known.Entries {
				if e.Probe != d.Probe || e.Pointer != d.Pointer || !bytes.Equal(e.Want, d.Want) {
					continue
				}
				if e.GotVaries {
					if d.Got == nil {
						continue
					}
				} else if !bytes.Equal(e.Got, d.Got) {
					continue
				}
				idx = i
				break
			}
			if idx >= 0 {
				matched[idx] = true
				continue
			}
			// A difference in a downstream probe whose downstream k3s differs
			// from the goldens' comes from the k3s version: kubernetes in a
			// helm-check run, undetermined in an upcoming run. Any other new
			// difference is install in a helm-check run and rancher in an
			// upcoming run.
			cause := CauseRancher
			switch {
			case r.Profile == ProfileHelmCheck && pr.Downstream && r.DownstreamImage != r.GoldenK3sImage:
				cause = CauseKubernetes
			case r.Profile == ProfileHelmCheck:
				cause = CauseInstall
			case pr.Downstream && r.DownstreamImage != r.GoldenK3sImage:
				cause = CauseUndetermined
			}
			r.New = append(r.New, NewDifference{
				Entry: KnownEntry{
					Probe:     d.Probe,
					Pointer:   d.Pointer,
					Want:      d.Want,
					Got:       d.Got,
					FirstSeen: r.GitCommit,
					Cause:     cause,
				},
				WaitRanOut: d.WaitRanOut,
			})
		}
	}

	for i, e := range known.Entries {
		switch {
		case matched[i] && e.Ruling == "":
			r.KnownNoRuling = append(r.KnownNoRuling, e)
		case matched[i]:
			r.Known = append(r.Known, e)
		case !ran[e.Probe] || errored[e.Probe]:
			r.NotChecked = append(r.NotChecked, e)
		default:
			r.NoLongerSeen = append(r.NoLongerSeen, e)
		}
	}

	for _, t := range cliTests {
		r.CLITests = append(r.CLITests, t)
		if !t.Passed && !t.Started {
			r.HarnessFailures = append(r.HarnessFailures, HarnessFailure{
				Name:   t.Name,
				Reason: "the CLI test failed before its CLI command ran",
			})
		}
	}
	for _, name := range DriftSuites {
		r.Suites = append(r.Suites, SuiteResult{Name: name, Probes: suiteProbes[name]})
	}

	slices.SortFunc(r.New, func(a, b NewDifference) int {
		return cmp.Or(strings.Compare(a.Entry.Probe, b.Entry.Probe), strings.Compare(a.Entry.Pointer, b.Entry.Pointer))
	})
	slices.SortFunc(r.NotCheckedProbes, func(a, b NotCheckedProbe) int { return strings.Compare(a.Probe, b.Probe) })
	slices.SortFunc(r.Errored, func(a, b ErroredProbe) int { return strings.Compare(a.Probe, b.Probe) })
	slices.SortFunc(r.CLITests, func(a, b CLITestRun) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(r.HarnessFailures, func(a, b HarnessFailure) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Reason, b.Reason))
	})

	c := &r.Counts
	c.New = len(r.New)
	for _, n := range r.New {
		if n.WaitRanOut {
			c.WaitRanOut++
		}
	}
	c.Known = len(r.Known)
	c.KnownNoRuling = len(r.KnownNoRuling)
	c.NoLongerSeen = len(r.NoLongerSeen)
	c.NotChecked = len(r.NotChecked)
	c.NotCheckedProbes = len(r.NotCheckedProbes)
	c.Errored = len(r.Errored)
	for _, t := range r.CLITests {
		if t.Started && !t.Passed {
			c.FailedCLITests++
		}
	}
	for _, s := range r.Suites {
		if s.Probes == 0 && suiteNotChecked[s.Name] == 0 {
			c.SuitesWithNoProbe++
		}
	}
	c.HarnessFailures = len(r.HarnessFailures)
	r.Clean = c.New == 0 && c.Errored == 0 && c.FailedCLITests == 0 && c.SuitesWithNoProbe == 0 && testsRan > 0
	return r
}

// DriftInputs is what FinishDriftRun decides from, gathered by TestMain
// after m.Run: the configuration, the known file's load result, the
// collector, the /rancherversion result or error, the transport guard's
// result and the exit code m.Run returned.
type DriftInputs struct {
	Config     DriftConfig
	Known      KnownFileLoad
	Collector  *DriftCollector
	Version    RancherVersion
	VersionErr error
	// TargetErr is a failure to build the oracle target, before
	// /rancherversion could be asked.
	TargetErr error
	GuardErr  error
	RunCode   int
}

// FinishDriftRun applies every rule that runs after m.Run and returns the
// report and the exit code of the test binary. A failed /rancherversion
// fetch, a failing transport guard, an unreadable or invalid known file or
// one for another minor, a failed test that is not a CLI test and a CLI test
// that failed before its command are harness failures. A non-zero m.Run
// code that no failed CLI test or failed test explains is the harness
// failure "unexplained test failure". The exit code is 0 only when m.Run
// returned 0, the guard passed, no harness failure was found and the report
// is clean; the script chooses 3 or 1 from the report.
func FinishDriftRun(in DriftInputs) (Report, int) {
	c := in.Collector
	c.mu.Lock()
	defer c.mu.Unlock()
	var harness []HarnessFailure
	known := in.Known.File
	switch {
	case in.Known.Err != nil:
		known = KnownFile{}
		harness = append(harness, HarnessFailure{Name: "known file", Reason: in.Known.Err.Error()})
	case in.Known.Exists && in.Known.File.Minor != in.Config.Minor:
		known = KnownFile{}
		harness = append(harness, HarnessFailure{
			Name:   "known file",
			Reason: fmt.Sprintf("%s describes minor %s, not %s", in.Config.KnownPath, in.Known.File.Minor, in.Config.Minor),
		})
	}
	if in.TargetErr != nil {
		harness = append(harness, HarnessFailure{Name: "oracle target", Reason: in.TargetErr.Error()})
	}
	if in.VersionErr != nil {
		harness = append(harness, HarnessFailure{Name: "/rancherversion", Reason: in.VersionErr.Error()})
	}
	if in.GuardErr != nil {
		harness = append(harness, HarnessFailure{Name: "default transport guard", Reason: in.GuardErr.Error()})
	}
	for _, name := range c.failed {
		harness = append(harness, HarnessFailure{Name: name, Reason: "failed; see the go test output"})
	}
	explained := len(c.failed) > 0
	for _, t := range c.cliTests {
		if !t.Passed {
			explained = true
		}
	}
	if in.RunCode != 0 && !explained {
		harness = append(harness, HarnessFailure{Name: "go test", Reason: "unexplained test failure"})
	}

	report := Compare(known, Report{
		Minor:             in.Config.Minor,
		Profile:           in.Config.reportProfile(),
		StartedAt:         in.Config.StartedAt,
		OracleImage:       in.Config.OracleImage,
		DownstreamImage:   in.Config.DownstreamImage,
		GoldenOracleImage: in.Config.GoldenImage,
		GoldenK3sImage:    in.Config.GoldenK3sImage,
		RancherVersion:    in.Version.Version,
		GitCommit:         in.Version.GitCommit,
		KnownFile:         in.Config.KnownPath,
		KnownFileExists:   in.Known.Exists,
	}, c.probes, c.cliTests, harness, c.tests)
	code := 1
	if in.RunCode == 0 && in.GuardErr == nil && report.Clean && report.Counts.HarnessFailures == 0 {
		code = 0
	}
	return report, code
}

// WriteReport writes the report with the file's encoder and complete set,
// through a temporary file in the same directory and a rename, so a reader
// sees either no report or a whole one. Nothing is left behind on an error.
func WriteReport(path string, r Report) error {
	r.Complete = true
	raw, err := encodeDriftJSON(r)
	if err != nil {
		return fmt.Errorf("encoding the drift report: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".drift-report-*")
	if err != nil {
		return fmt.Errorf("creating the drift report: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("writing the drift report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("writing the drift report: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("writing the drift report: %w", err)
	}
	return nil
}

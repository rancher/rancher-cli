// Package fidelity defines the probe vocabulary for the testrancher fidelity
// oracle: probes recorded against a real, pinned Rancher and replayed against
// the e2e composite facade.
package fidelity

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Identity names the credential an observation is sent with. Targets map
// admin, standard, and revoked to concrete bearers; none and garbage are
// resolved by the executor itself, identically for every target.
type Identity string

const (
	IdentityNone     Identity = "none"
	IdentityGarbage  Identity = "garbage"
	IdentityRevoked  Identity = "revoked"
	IdentityAdmin    Identity = "admin"
	IdentityStandard Identity = "standard"
	// IdentityMember is a local user holding one ProjectRoleTemplateBinding
	// (project-member) on the downstream cluster's Default project and no
	// other role: what the access commands call a member.
	IdentityMember Identity = "member"
	// IdentityClusterOwner is a local user holding the baseline user
	// GlobalRole and one cluster-owner ClusterRoleTemplateBinding on the
	// downstream cluster, and no access to the local cluster. A probe's
	// setup creates it (clusterOwnerFixture) and records its bearer as the
	// clusterOwnerBearer capture; the executor sends that bearer.
	IdentityClusterOwner Identity = "clusterowner"
)

// Target is one server a probe executes against: the real Rancher oracle or
// the e2e composite facade.
type Target interface {
	// BaseURL is the server root without a trailing slash.
	BaseURL() string
	// Client returns an HTTP client that trusts the target's CA.
	Client() *http.Client
	// Bearer resolves admin, standard, member, or revoked to a bearer
	// token. The executor never calls it for none or garbage.
	Bearer(ctx context.Context, id Identity) (string, error)
	// Downstream is the id of a downstream cluster the target can reach
	// through the proxy, or empty when it has none. The facade always
	// returns empty.
	Downstream() string
}

// RequestSpec is one HTTP request. Path, Query values, Headers values, and
// Body may contain {name} placeholders resolved from earlier observations'
// captures. The executor defaults Content-Type to application/json when Body
// is non-empty and no explicit Content-Type header is given.
type RequestSpec struct {
	Method   string
	Path     string
	Query    url.Values
	Headers  map[string]string
	Body     string
	Identity Identity
}

// Capture extracts one value from an observation's raw (pre-normalization)
// response body. Path is dotted. ExtBearerName additionally derives the token
// name embedded in an ext-style bearer ("ext/<name>:<secret>") from the
// captured string - the imperative create response carries its credential only
// under status.bearerToken, so the created name is not otherwise available.
type Capture struct {
	Path          string
	ExtBearerName bool
}

// Observation is one named request/response pair; every observation's
// normalized response is recorded in the golden. Captures maps a placeholder
// name to a Capture into this observation's response; later observations and
// teardown steps reference it as {name}. A declared capture that cannot be
// resolved or transformed FAILS the probe - capture-dependent requests never
// execute against a synthetic URL, and no golden is saved.
type Observation struct {
	Name     string
	Request  RequestSpec
	Captures map[string]Capture
	// WaitForStatus, when non-zero, re-sends the request until the answer
	// has this status, for at most waitForStatusTimeout; past that the
	// probe fails and no golden is saved. Rancher authenticates a bearer
	// from informer caches: a token just deleted or logged out can still
	// authenticate, and an ext token just created can still be refused,
	// until the watch event reaches the cache. Set it to the status the
	// golden records on an observation that uses a token right after its
	// create, delete or logout. An observation that captures never waits:
	// it must not be sent twice.
	WaitForStatus int
	// Until, when set, sends the request again every untilInterval until it
	// reports done, on a context of its own: the observation ends at the earlier
	// of untilTimeout after its first read and the time a setup step stored
	// under waitDeadlineCapture. state describes what the answer showed
	// (condition types and statuses, an App status), never a body or a value; a
	// wait that ends first fails the probe and names it. A read that runs past
	// its limit or gets a transport error is sent again until the wait ends. An
	// err, or an error building the request or resolving its bearer, fails the
	// observation at once. The recorded answer is the one Until accepted; a wait
	// that ran out is never recorded, whatever the RunOptions. Only a GET can
	// set it, because the request is sent again, and never with Captures or
	// WaitForStatus.
	Until func(decoded map[string]any, status int, captured map[string]string) (done bool, state string, err error)
	// Normalize holds rules for this observation's body only, applied
	// after DefaultNormalizeRules and the probe's own rules.
	Normalize []NormalizeRule
}

// waitingFor returns o with WaitForStatus set to status.
func waitingFor(status int, o Observation) Observation {
	o.WaitForStatus = status
	return o
}

// NormalizeRule scrubs one field of a recorded body. Path is dotted; a "*"
// segment matches every key of an object, and arrays are always traversed
// element-wise. A non-empty Replace overwrites the value in place; an empty
// Replace deletes the field. KeepPrefix, mutually exclusive with Replace,
// treats the addressed value as an array of strings: every element that
// does not start with the prefix is replaced by "<group>" in place, order
// and count kept, so a fact like "system:masters is absent from the
// caller's groups" survives while every instance-chosen element (a SAML
// group principal name, say) does not. KeepKeys, exclusive with every other
// mode, treats the addressed value as an object and deletes every key not
// listed, compared literally, so one annotation whose name holds dots can
// stay while the rest of the map goes; a value that is not an object
// becomes "<object>". An object with none of the kept keys records as `{}`,
// which means none of the kept keys, not an empty map on the server.
type NormalizeRule struct {
	Path       string
	Replace    string
	KeepPrefix string
	// SortBy, when set, sorts the array at Path by the string at this
	// dot-separated sub-path of each element (stable, ascending); "."
	// sorts an array of strings by the strings themselves. Server list
	// order follows generated names, so a recorded list with names
	// normalized away needs an order of its own.
	SortBy string
	// DropPrefix, when set, removes from the array at Path every element
	// whose string at the DropBy sub-path ("." for the element itself)
	// starts with it, keeping the order of the rest; when CountKey is set
	// and the object holding the array has that key, it becomes the number
	// of elements kept, or is deleted when none is kept, as steve omits
	// count on an empty collection. A collection read while another
	// probe's fixtures exist then records only what no probe creates.
	DropPrefix string
	DropBy     string
	CountKey   string
	KeepKeys   []string
}

// Step is a fixture action run before or after a probe's observations.
// Fixture behavior asserted by the oracle belongs in Observations, never
// here - steps exist only to create fixtures and clean them up. captured
// is the probe's capture set: during setup it holds only the downstream
// id of a Downstream probe, and a setup step may record values in it - the
// id of a fixture it created, for the teardown to delete by, or a value an
// observation's request needs as a {name} placeholder; it is complete
// during teardown. Recorded values are substituted like captures when
// SubstituteCaptures is set, except those whose name starts with
// unrecordedPrefix: such a value is an input to later requests and waits
// (a label selector, a detected grammar), never rewritten into a recorded
// body or text, and it never contains a "{" placeholder.
type Step struct {
	Name string
	Do   func(ctx context.Context, tgt Target, captured map[string]string) error
}

// Probe is data, not code: an ordered sequence of observations recorded
// against the oracle and asserted against the composite facade.
type Probe struct {
	Name         string
	Setup        []Step
	Observations []Observation
	Normalize    []NormalizeRule
	Teardown     []Step
	// FacadeDivergence, when non-empty, marks a known divergence: the
	// conformance test asserts the facade matches its own recorded golden
	// (testdata/facade/<probe>.json) exactly AND that that golden still
	// differs from the oracle golden.
	FacadeDivergence string
	// OracleOnly probes run in record and verify-real modes only: skipped
	// by conformance, never have a facade golden.
	OracleOnly bool
	// Downstream marks a probe that needs a downstream cluster reached
	// through the proxy: RunProbe seeds the "downstream" capture from the
	// target's Downstream() before the first observation and refuses to
	// run (never records) when the target has none. Recorded under
	// testdata/downstream, never under oracle or facade; skipped by
	// conformance and by TestOracle.
	Downstream bool
	// SubstituteCaptures replaces every captured value (the downstream
	// cluster id, anything an observation captured) with "{name}" in the
	// keys and string values of every recorded body, after Normalize. A
	// label key built from the cluster id then records as the template
	// the next boot reproduces. Captures whose name starts with
	// unrecordedPrefix are the exception: they are never substituted.
	SubstituteCaptures bool
	// MaxMinor, when set, is the newest Rancher minor the probe runs on,
	// written major.minor (for example "2.15"). A server of that minor is
	// still checked. Against a newer server the probe does not run: it
	// sends only the request for the server version and runs no setup or
	// teardown step. Only an OracleOnly or a Downstream probe can set it,
	// because the facade has no Rancher version.
	MaxMinor string
	// OwnPairOnly marks a probe that writes objects only the pair
	// scripts/e2e-fidelity booted or kept may hold. Before any setup step
	// the executor compares the server's install-uuid setting with the
	// install id the target names (OracleTarget.PairInstallUUID); when the
	// target names none or the two differ, the probe is not checked and
	// sends nothing else. Only an OracleOnly or a Downstream probe can set
	// it.
	OwnPairOnly bool
}

// maxMinorPattern is the form of Probe.MaxMinor: major.minor, no leading v, no
// leading zeros.
var maxMinorPattern = regexp.MustCompile(`^([1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Validate enforces the probe-definition invariants.
func (p Probe) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("probe has no name")
	}
	if len(p.Observations) == 0 {
		return fmt.Errorf("probe %s: no observations", p.Name)
	}
	seen := make(map[string]bool, len(p.Observations))
	for _, o := range p.Observations {
		if o.Name == "" {
			return fmt.Errorf("probe %s: observation has no name", p.Name)
		}
		if seen[o.Name] {
			return fmt.Errorf("probe %s: duplicate observation %q", p.Name, o.Name)
		}
		seen[o.Name] = true
		if o.Request.Method == "" || o.Request.Path == "" {
			return fmt.Errorf("probe %s: observation %q: method and path are required", p.Name, o.Name)
		}
		if o.WaitForStatus != 0 && (o.WaitForStatus < 100 || o.WaitForStatus > 599) {
			return fmt.Errorf("probe %s: observation %q: WaitForStatus %d is not an HTTP status", p.Name, o.Name, o.WaitForStatus)
		}
		if o.WaitForStatus != 0 && len(o.Captures) > 0 {
			return fmt.Errorf("probe %s: observation %q: an observation with captures cannot set WaitForStatus", p.Name, o.Name)
		}
		for _, name := range slices.Sorted(maps.Keys(o.Captures)) {
			if strings.HasPrefix(name, unrecordedPrefix) {
				return fmt.Errorf("probe %s: observation %q: capture %q: the %q prefix is reserved for setup values that are never recorded", p.Name, o.Name, name, unrecordedPrefix)
			}
		}
		if o.Until != nil {
			if o.Request.Method != http.MethodGet {
				return fmt.Errorf("probe %s: observation %q: Until sends the request again and needs method GET, not %s", p.Name, o.Name, o.Request.Method)
			}
			if len(o.Captures) > 0 {
				return fmt.Errorf("probe %s: observation %q: an observation with captures cannot set Until", p.Name, o.Name)
			}
			if o.WaitForStatus != 0 {
				return fmt.Errorf("probe %s: observation %q: Until and WaitForStatus are mutually exclusive", p.Name, o.Name)
			}
		}
		if err := validateRules(o.Normalize); err != nil {
			return fmt.Errorf("probe %s: observation %q: %w", p.Name, o.Name, err)
		}
	}
	if p.OracleOnly && p.FacadeDivergence != "" {
		return fmt.Errorf("probe %s: OracleOnly and FacadeDivergence are mutually exclusive", p.Name)
	}
	if p.Downstream && p.OracleOnly {
		return fmt.Errorf("probe %s: Downstream and OracleOnly are mutually exclusive", p.Name)
	}
	if p.Downstream && p.FacadeDivergence != "" {
		return fmt.Errorf("probe %s: Downstream and FacadeDivergence are mutually exclusive", p.Name)
	}
	if p.MaxMinor != "" {
		if !maxMinorPattern.MatchString(p.MaxMinor) {
			return fmt.Errorf("probe %s: MaxMinor %q is not in the form major.minor, for example 2.15", p.Name, p.MaxMinor)
		}
		if !p.OracleOnly && !p.Downstream {
			return fmt.Errorf("probe %s: MaxMinor can be set only on an OracleOnly or a Downstream probe, because the facade has no Rancher version", p.Name)
		}
	}
	if p.OwnPairOnly && !p.OracleOnly && !p.Downstream {
		return fmt.Errorf("probe %s: OwnPairOnly can be set only on an OracleOnly or a Downstream probe, because only the oracle has an install id", p.Name)
	}
	if err := validateRules(p.Normalize); err != nil {
		return fmt.Errorf("probe %s: %w", p.Name, err)
	}
	return nil
}

// validateRules checks the normalize rules of a probe or of one observation.
func validateRules(rules []NormalizeRule) error {
	for _, r := range rules {
		if r.Replace != "" && r.KeepPrefix != "" {
			return fmt.Errorf("normalize rule %s: Replace and KeepPrefix are mutually exclusive", r.Path)
		}
		if r.SortBy != "" && (r.Replace != "" || r.KeepPrefix != "") {
			return fmt.Errorf("normalize rule %s: SortBy is exclusive with Replace and KeepPrefix", r.Path)
		}
		if r.DropPrefix != "" && r.DropBy == "" {
			return fmt.Errorf("normalize rule %s: DropPrefix needs DropBy", r.Path)
		}
		if r.DropPrefix != "" && (r.Replace != "" || r.KeepPrefix != "" || r.SortBy != "") {
			return fmt.Errorf("normalize rule %s: DropPrefix is exclusive with Replace, KeepPrefix and SortBy", r.Path)
		}
		if len(r.KeepKeys) > 0 && (r.Replace != "" || r.KeepPrefix != "" || r.SortBy != "" || r.DropPrefix != "") {
			return fmt.Errorf("normalize rule %s: KeepKeys is exclusive with Replace, KeepPrefix, SortBy and DropPrefix", r.Path)
		}
		if slices.Contains(r.KeepKeys, "") {
			return fmt.Errorf("normalize rule %s: KeepKeys has an empty entry", r.Path)
		}
	}
	return nil
}

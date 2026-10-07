// Package fidelity defines the probe vocabulary for the testrancher fidelity
// oracle: probes recorded against a real, pinned Rancher and replayed against
// the e2e composite facade.
package fidelity

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
}

// NormalizeRule scrubs one field of a recorded body. Path is dotted; a "*"
// segment matches every key of an object, and arrays are always traversed
// element-wise. A non-empty Replace overwrites the value in place; an empty
// Replace deletes the field. KeepPrefix, mutually exclusive with Replace,
// treats the addressed value as an array of strings: every element that
// does not start with the prefix is replaced by "<group>" in place, order
// and count kept, so a fact like "system:masters is absent from the
// caller's groups" survives while every instance-chosen element (a SAML
// group principal name, say) does not.
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
}

// Step is a fixture action run before or after a probe's observations.
// Fixture behavior asserted by the oracle belongs in Observations, never
// here - steps exist only to create fixtures and clean them up. captured
// is the probe's capture set: during setup it holds only the downstream
// id of a Downstream probe, and a setup step may record values in it - the
// id of a fixture it created, for the teardown to delete by, or a value an
// observation's request needs as a {name} placeholder; it is complete
// during teardown. Recorded values are substituted like captures when
// SubstituteCaptures is set.
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
	// the next boot reproduces.
	SubstituteCaptures bool
}

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
	for _, r := range p.Normalize {
		if r.Replace != "" && r.KeepPrefix != "" {
			return fmt.Errorf("probe %s: normalize rule %s: Replace and KeepPrefix are mutually exclusive", p.Name, r.Path)
		}
		if r.SortBy != "" && (r.Replace != "" || r.KeepPrefix != "") {
			return fmt.Errorf("probe %s: normalize rule %s: SortBy is exclusive with Replace and KeepPrefix", p.Name, r.Path)
		}
		if r.DropPrefix != "" && r.DropBy == "" {
			return fmt.Errorf("probe %s: normalize rule %s: DropPrefix needs DropBy", p.Name, r.Path)
		}
		if r.DropPrefix != "" && (r.Replace != "" || r.KeepPrefix != "" || r.SortBy != "") {
			return fmt.Errorf("probe %s: normalize rule %s: DropPrefix is exclusive with Replace, KeepPrefix and SortBy", p.Name, r.Path)
		}
	}
	return nil
}

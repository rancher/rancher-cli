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
)

// Target is one server a probe executes against: the real Rancher oracle or
// the e2e composite facade.
type Target interface {
	// BaseURL is the server root without a trailing slash.
	BaseURL() string
	// Client returns an HTTP client that trusts the target's CA.
	Client() *http.Client
	// Bearer resolves admin, standard, or revoked to a bearer token. The
	// executor never calls it for none or garbage.
	Bearer(ctx context.Context, id Identity) (string, error)
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
// Replace deletes the field.
type NormalizeRule struct {
	Path    string
	Replace string
}

// Step is a fixture action run before or after a probe's observations.
// Fixture behavior asserted by the oracle belongs in Observations, never
// here - steps exist only for cleanup and idempotency. captured is the
// probe's resolved capture set: empty during setup, complete during
// teardown, usable via {name} placeholders in step-built paths.
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
}

// Validate enforces the probe-definition invariants from the spec.
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
	return nil
}

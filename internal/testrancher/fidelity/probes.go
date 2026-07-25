package fidelity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
)

// crtCollectionPath is the Steve collection the bearer-validation auth probes
// read. It is testrancher's exported constant (fidelity imports its parent),
// so there is one copy on the test side.
const crtCollectionPath = testrancher.CRTCollectionPath

// localSecretsPath is the kube-proxy path prefix for core/v1 Secrets in the
// local cluster — the exact URL shape client-go's dynamic client produces for
// internal/client/provider's Create/Update/Delete calls.
const localSecretsPath = "/k8s/clusters/local/api/v1/namespaces/default/secrets"

// All returns the first-slice probe set: bearer-validation and RBAC-denial
// auth probes, façade-owned ext-token mutation probes, and kube-proxy
// mutation-pipeline probes. Each family's oracle target is documented in the
// spec ("which layer it oracles").
func All() []Probe {
	probes := []Probe{}
	probes = append(probes, authProbes()...)
	probes = append(probes, tokProbes()...)
	probes = append(probes, mutProbes()...)
	return probes
}

func authProbes() []Probe {
	crtGet := func(name string, id Identity) Probe {
		return Probe{
			Name: name,
			Observations: []Observation{
				{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: crtCollectionPath, Identity: id}},
			},
		}
	}
	// Same delta for all three bearer-rejection identities: the composite
	// façade's bearer check rejects the read with a bodyless 401, where the
	// real server answers 401 application/json with a Norman error envelope.
	const bearer401Divergence = "façade bearer-check returns a bodyless 401 with an empty Content-Type; the oracle returns 401 application/json with a Norman error body {message,status,type}"
	noToken := crtGet("auth/no-token", IdentityNone)
	noToken.FacadeDivergence = bearer401Divergence
	garbage := crtGet("auth/garbage-token", IdentityGarbage)
	garbage.FacadeDivergence = bearer401Divergence
	revoked := crtGet("auth/revoked-token", IdentityRevoked)
	revoked.FacadeDivergence = bearer401Divergence

	standardMint := Probe{
		Name: "auth/standard-mint",
		Observations: []Observation{
			{
				Name:     "mint",
				Request:  RequestSpec{Method: http.MethodPost, Path: token.MintTokensPath, Body: `{"spec":{}}`, Identity: IdentityStandard},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
		},
		// The standard user is provisioned fresh each run, so its id (embedded
		// in the token name, the user-id label, and the userPrincipal) and the
		// token's own generated name vary; scrub them to the shape that matters.
		Normalize: []NormalizeRule{
			{Path: "metadata.name", Replace: "<name>"},
			{Path: "metadata.labels.*", Replace: "<label>"},
			{Path: "spec.userID", Replace: "<user-id>"},
			{Path: "spec.userPrincipal.name", Replace: "<principal>"},
		},
		FacadeDivergence: "façade mint returns a minimal Token {apiVersion,kind,status:{bearerToken,expiresAt}}; the oracle returns the full ext Token with metadata (labels, generateName, uid), spec (ttl, userID, userPrincipal), and status (current/expired/lastUpdateTime)",
		Teardown:         []Step{deleteFixture("minted standard token", token.MintTokensPath+"/{tokname}")},
	}

	forbiddenRead := Probe{
		Name:       "auth/forbidden-read",
		OracleOnly: true,
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/secrets", Identity: IdentityStandard}},
		},
		// The Steve collection carries a top-level revision (its list
		// resourceVersion) that advances between runs.
		Normalize: []NormalizeRule{{Path: "revision", Replace: "<rev>"}},
	}
	forbiddenWrite := Probe{
		Name:       "auth/forbidden-write",
		OracleOnly: true,
		Observations: []Observation{
			{
				Name: "post",
				Request: RequestSpec{
					Method:   http.MethodPost,
					Path:     "/v1/management.cattle.io.globalroles",
					Body:     `{"apiVersion":"management.cattle.io/v3","kind":"GlobalRole","metadata":{"name":"e2e-fid-forbidden"}}`,
					Identity: IdentityStandard,
				},
			},
		},
	}
	return []Probe{noToken, garbage, revoked, standardMint, forbiddenRead, forbiddenWrite}
}

func tokProbes() []Probe {
	// The imperative mint response carries its credential only under
	// status.bearerToken (testrancher.writeExtToken includes no metadata),
	// so the minted name is derived from the ext-shaped bearer — the same
	// mechanism RealTarget.revokedBearer uses.
	mint := Observation{
		Name:     "mint",
		Request:  RequestSpec{Method: http.MethodPost, Path: token.MintTokensPath, Body: `{"spec":{}}`, Identity: IdentityAdmin},
		Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
	}
	namedPath := token.MintTokensPath + "/{tokname}"
	revokeLeftover := deleteFixture("minted token", namedPath)

	// The façade's ext-token store returns a minimal Token; every full-Token
	// observation therefore diverges on shape (missing metadata labels/uid,
	// spec.userPrincipal, and status.hash/current/expired/lastUpdateTime, plus
	// an empty spec.clusterName the façade emits on GET).
	const tokenShapeDivergence = "façade returns a minimal Token {apiVersion,kind,status:{bearerToken,expiresAt}} on mint and {metadata.name/rv, spec.clusterName, status.expiresAt} on GET; the oracle returns the full ext Token with metadata labels/generateName/uid, spec ttl/userID/userPrincipal, and status hash/current/expired/lastUpdateTime"

	// The mint/get/revoke Token bodies carry the acting admin's per-install
	// user id (spec.userID, the cattle.io/user-id label, and the userPrincipal
	// name "local://<id>"), which differs across fresh boots; scrub it exactly
	// as auth/standard-mint does for its own user. metadata.labels.* stays
	// probe-level on purpose — as a default rule it would blank meaningful
	// labels on other kinds (e.g. the mut/* Secrets).
	tokenIdentity := func() []NormalizeRule {
		return []NormalizeRule{
			{Path: "metadata.name", Replace: "<name>"},
			{Path: "metadata.labels.*", Replace: "<label>"},
			{Path: "spec.userID", Replace: "<user-id>"},
			{Path: "spec.userPrincipal.name", Replace: "<principal>"},
		}
	}

	mintShape := Probe{
		Name: "tok/mint-shape",
		Observations: []Observation{
			mint,
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		Normalize:        tokenIdentity(),
		FacadeDivergence: tokenShapeDivergence,
		Teardown:         []Step{revokeLeftover},
	}

	dryrunDelete := Probe{
		Name: "tok/dryrun-delete",
		Observations: []Observation{
			mint,
			{
				Name:    "dry-run-delete",
				Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Body: `{"dryRun":["All"]}`, Identity: IdentityAdmin},
			},
			{Name: "still-there", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		Normalize:        tokenIdentity(),
		FacadeDivergence: tokenShapeDivergence,
		Teardown:         []Step{revokeLeftover},
	}

	stalePrecondition := Probe{
		Name: "tok/stale-precondition-delete",
		Observations: []Observation{
			mint,
			{
				Name: "stale-delete",
				Request: RequestSpec{
					Method:   http.MethodDelete,
					Path:     namedPath,
					Body:     `{"preconditions":{"resourceVersion":"0-stale"}}`,
					Identity: IdentityAdmin,
				},
			},
		},
		// The 500 Status embeds the generated token name and its live
		// resourceVersion in the top-level message and in details.causes.
		Normalize: append(tokenIdentity(),
			NormalizeRule{Path: "message", Replace: "<message>"},
			NormalizeRule{Path: "details.causes.message", Replace: "<message>"},
		),
		FacadeDivergence: "façade returns 409 Conflict with no body where the 2.14.3 oracle returns 500 InternalError with a k8s Status for a stale preconditions.resourceVersion delete; the mint observation also diverges on Token shape (minimal vs full).",
		Teardown:         []Step{revokeLeftover},
	}

	revoke := Probe{
		Name: "tok/revoke",
		Observations: []Observation{
			mint,
			{Name: "revoke", Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Identity: IdentityAdmin}},
			{Name: "gone", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		// The 404 "gone" Status names the deleted token in details.name and in
		// the message; both carry the generated token name.
		Normalize: append(tokenIdentity(),
			NormalizeRule{Path: "details.name", Replace: "<name>"},
			NormalizeRule{Path: "message", Replace: "<message>"},
		),
		FacadeDivergence: "façade mint and revoke return a minimal Token; the oracle returns the full ext Token (metadata labels/generateName/uid, spec userPrincipal, status hash/current/expired/lastUpdateTime). The 404 gone observation conforms.",
	}
	return []Probe{mintShape, dryrunDelete, stalePrecondition, revoke}
}

func mutProbes() []Probe {
	secretBody := func(name string) string {
		return fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":%q},"stringData":{"k":"v"}}`, name)
	}
	secretUpdate := func(name, rv, label string) string {
		return fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":%q,"resourceVersion":%q,"labels":{"step":%q}},"stringData":{"k":"v"}}`, name, rv, label)
	}
	getRV := func(name string) Observation {
		return Observation{
			Name:     "get",
			Request:  RequestSpec{Method: http.MethodGet, Path: localSecretsPath + "/" + name, Identity: IdentityAdmin},
			Captures: map[string]Capture{"rv1": {Path: "metadata.resourceVersion"}},
		}
	}
	dryRunAll := url.Values{"dryRun": []string{"All"}}

	dryrunCreate := Probe{
		Name:  "mut/dryrun-create",
		Setup: []Step{deleteFixture("stale dry-run fixture", localSecretsPath+"/e2e-fid-dryrun")},
		Observations: []Observation{
			{
				Name:    "dry-run-create",
				Request: RequestSpec{Method: http.MethodPost, Path: localSecretsPath, Query: dryRunAll, Body: secretBody("e2e-fid-dryrun"), Identity: IdentityAdmin},
			},
			{Name: "not-persisted", Request: RequestSpec{Method: http.MethodGet, Path: localSecretsPath + "/e2e-fid-dryrun", Identity: IdentityAdmin}},
		},
		Teardown: []Step{deleteFixture("dry-run leftover", localSecretsPath+"/e2e-fid-dryrun")},
	}

	dryrunInvalid := Probe{
		Name: "mut/dryrun-invalid",
		Observations: []Observation{
			{
				Name: "dry-run-invalid",
				Request: RequestSpec{
					Method: http.MethodPost, Path: localSecretsPath, Query: dryRunAll,
					Body:     `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"e2e-fid-Bad_Name!"}}`,
					Identity: IdentityAdmin,
				},
			},
		},
	}

	staleUpdate := Probe{
		Name:  "mut/stale-rv-update",
		Setup: []Step{createSecretFixture("e2e-fid-rv", secretBody("e2e-fid-rv"))},
		Observations: []Observation{
			getRV("e2e-fid-rv"),
			{
				Name:    "bump",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-rv", Body: secretUpdate("e2e-fid-rv", "{rv1}", "bump"), Identity: IdentityAdmin},
			},
			{
				Name:    "stale-update",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-rv", Body: secretUpdate("e2e-fid-rv", "{rv1}", "stale"), Identity: IdentityAdmin},
			},
		},
		Teardown: []Step{deleteFixture("rv fixture", localSecretsPath+"/e2e-fid-rv")},
	}

	staleDelete := Probe{
		Name:  "mut/rv-precondition-delete",
		Setup: []Step{createSecretFixture("e2e-fid-del", secretBody("e2e-fid-del"))},
		Observations: []Observation{
			getRV("e2e-fid-del"),
			{
				Name:    "bump",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-del", Body: secretUpdate("e2e-fid-del", "{rv1}", "bump"), Identity: IdentityAdmin},
			},
			{
				Name:    "stale-delete",
				Request: RequestSpec{Method: http.MethodDelete, Path: localSecretsPath + "/e2e-fid-del", Body: `{"preconditions":{"resourceVersion":"{rv1}"}}`, Identity: IdentityAdmin},
			},
		},
		// The 409 Status message quotes both the precondition and the record
		// resourceVersion, which advance between runs.
		Normalize: []NormalizeRule{{Path: "message", Replace: "<message>"}},
		Teardown:  []Step{deleteFixture("del fixture", localSecretsPath+"/e2e-fid-del")},
	}

	// The dry-run PUT and the real PUT send the byte-identical body — the
	// CLI safety invariant is that execution applies exactly the object the
	// preview rendered, so the probe must not vary it between the two.
	planned := secretUpdate("e2e-fid-seq", "{rv1}", "planned")
	dryrunThenApply := Probe{
		Name:  "mut/dryrun-then-apply",
		Setup: []Step{createSecretFixture("e2e-fid-seq", secretBody("e2e-fid-seq"))},
		Observations: []Observation{
			getRV("e2e-fid-seq"),
			{
				Name:    "dry-run-update",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-seq", Query: dryRunAll, Body: planned, Identity: IdentityAdmin},
			},
			{
				Name:    "apply",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-seq", Body: planned, Identity: IdentityAdmin},
			},
		},
		Teardown: []Step{deleteFixture("seq fixture", localSecretsPath+"/e2e-fid-seq")},
	}

	return []Probe{dryrunCreate, dryrunInvalid, staleUpdate, staleDelete, dryrunThenApply}
}

// adminDo sends one admin-authenticated request for fixture steps and
// returns the status code. Transport and bearer failures are real errors —
// steps surface them, never swallow them.
func adminDo(ctx context.Context, tgt Target, method, path, body string) (int, error) {
	tok, err := tgt.Bearer(ctx, IdentityAdmin)
	if err != nil {
		return 0, fmt.Errorf("resolving admin bearer: %w", err)
	}
	build := func() (*http.Request, error) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, tgt.BaseURL()+path, rdr)
		if err != nil {
			return nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return req, nil
	}
	resp, err := doResilient(tgt.Client(), build)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// deleteFixture DELETEs pathTemplate (captures resolved) as admin. Only an
// explicit 404 or a 2xx counts as clean; auth and transport failures are
// surfaced. An unresolved template is surfaced too — the fixture may have
// materialized without its capture resolving, and a silent skip would leak
// it on a kept server.
func deleteFixture(name, pathTemplate string) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			path := substitute(pathTemplate, captured)
			if strings.Contains(path, "{") {
				return fmt.Errorf("cleanup %s: capture in %q unresolved; a fixture may have leaked", name, pathTemplate)
			}
			status, err := adminDo(ctx, tgt, http.MethodDelete, path, "")
			if err != nil {
				return fmt.Errorf("cleanup %s: %w", name, err)
			}
			if status == http.StatusNotFound || (status >= 200 && status < 300) {
				return nil
			}
			return fmt.Errorf("cleanup %s: unexpected status %d", name, status)
		},
	}
}

// createSecretFixture is idempotent delete-before-create: an interrupted
// prior run's leftover is removed first, so a kept-container rerun never
// trips AlreadyExists.
func createSecretFixture(name, body string) Step {
	path := localSecretsPath + "/" + name
	return Step{
		Name: "create " + name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			if err := deleteFixture("stale "+name, path).Do(ctx, tgt, captured); err != nil {
				return err
			}
			status, err := adminDo(ctx, tgt, http.MethodPost, localSecretsPath, body)
			if err != nil {
				return fmt.Errorf("creating fixture %s: %w", name, err)
			}
			if status != http.StatusCreated {
				return fmt.Errorf("creating fixture %s: status %d, want %d", name, status, http.StatusCreated)
			}
			return nil
		},
	}
}

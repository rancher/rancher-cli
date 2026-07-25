package fidelity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/token"
)

// crtCollectionPath is the façade-owned Steve collection the bearer-validation
// auth probes read. It mirrors testrancher's own crtCollectionPath (itself
// pinned to internal/engine/provisioning's constant).
const crtCollectionPath = "/v1/management.cattle.io.clusterregistrationtokens"

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
	noToken := crtGet("auth/no-token", IdentityNone)
	garbage := crtGet("auth/garbage-token", IdentityGarbage)
	revoked := crtGet("auth/revoked-token", IdentityRevoked)

	standardMint := Probe{
		Name: "auth/standard-mint",
		Observations: []Observation{
			{
				Name:     "mint",
				Request:  RequestSpec{Method: http.MethodPost, Path: token.MintTokensPath, Body: `{"spec":{}}`, Identity: IdentityStandard},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
		},
		Teardown: []Step{deleteFixture("minted standard token", token.MintTokensPath+"/{tokname}")},
	}

	forbiddenRead := Probe{
		Name:       "auth/forbidden-read",
		OracleOnly: true,
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/secrets", Identity: IdentityStandard}},
		},
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

	mintShape := Probe{
		Name: "tok/mint-shape",
		Observations: []Observation{
			mint,
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		Normalize: []NormalizeRule{{Path: "metadata.name", Replace: "<name>"}},
		Teardown:  []Step{revokeLeftover},
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
		Normalize: []NormalizeRule{{Path: "metadata.name", Replace: "<name>"}},
		Teardown:  []Step{revokeLeftover},
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
		Normalize: []NormalizeRule{{Path: "metadata.name", Replace: "<name>"}},
		Teardown:  []Step{revokeLeftover},
	}

	revoke := Probe{
		Name: "tok/revoke",
		Observations: []Observation{
			mint,
			{Name: "revoke", Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Identity: IdentityAdmin}},
			{Name: "gone", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		Normalize: []NormalizeRule{{Path: "metadata.name", Replace: "<name>"}},
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
		Teardown: []Step{deleteFixture("del fixture", localSecretsPath+"/e2e-fid-del")},
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
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, tgt.BaseURL()+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := tgt.Client().Do(req)
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

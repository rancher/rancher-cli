//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
	"github.com/rancher/rancher-cli/internal/token"
)

var updateFacade = flag.Bool("update-facade", false,
	"record facade goldens for divergence-pinned probes")

// fidelityDir is the fidelity library's testdata root, relative to this
// package directory (test/e2e/).
var fidelityDir = filepath.Join("..", "..", "internal", "testrancher", "fidelity", "testdata")

// compositeTarget is the fidelity Target for the same facade-plus-envtest
// composite TestScenarios wires per script: testrancher.Facade in front, the
// bearer-checked router behind, envtest behind that.
type compositeTarget struct {
	srv    *httptest.Server
	facade *testrancher.Facade

	standard fidelity.LazyBearer
	revoked  fidelity.LazyBearer
}

func newCompositeTarget(t *testing.T) *compositeTarget {
	t.Helper()
	ct := &compositeTarget{}
	var facade *testrancher.Facade
	router := newRouter(facadeIdentityResolver(shimIdentities, func(tok string) bool {
		return facade != nil && facade.ValidBearer(tok)
	}))
	facade = testrancher.New(testrancher.Options{Fallthrough: router, SeedToken: testToken,
		WritesWithoutFieldManager: "fidelity probes replay the requests recorded against a real Rancher"})
	ct.facade = facade
	ct.srv = httptest.NewTLSServer(facade)
	t.Cleanup(ct.srv.Close)
	return ct
}

func (c *compositeTarget) BaseURL() string      { return c.srv.URL }
func (c *compositeTarget) Client() *http.Client { return c.srv.Client() }

// Downstream is always empty: the facade has no downstream cluster.
func (c *compositeTarget) Downstream() string { return "" }

func (c *compositeTarget) Bearer(ctx context.Context, id fidelity.Identity) (string, error) {
	switch id {
	case fidelity.IdentityAdmin:
		return testToken, nil
	case fidelity.IdentityStandard, fidelity.IdentityMember:
		// The facade has no downstream and no projects: every non-admin
		// identity is its one derived user, and the member probes are
		// downstream-only and never run here.
		return c.standard.Get(func() (string, error) {
			bearer, _, err := c.derivedBearer(ctx)
			return bearer, err
		})
	case fidelity.IdentityRevoked:
		return c.revoked.Get(func() (string, error) {
			bearer, name, err := c.derivedBearer(ctx)
			if err != nil {
				return "", err
			}
			if err := c.deleteToken(ctx, name); err != nil {
				return "", err
			}
			return bearer, nil
		})
	default:
		return "", fmt.Errorf("identity %q is resolved by the executor, not the target", id)
	}
}

// derivedBearer logs in as the facade's one user with no responseType and
// returns the v3 session token's bearer and its name.
func (c *compositeTarget) derivedBearer(ctx context.Context) (bearer, name string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.srv.URL+"/v1-public/login",
		newJSONBody(`{"type":"localProvider","username":"e2e-user","password":"e2e-pass"}`))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("facade login: status %d", resp.StatusCode)
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := decodeJSONBody(resp, &created); err != nil {
		return "", "", err
	}
	if created.Token == "" || created.ID == "" {
		return "", "", errors.New("facade login: empty token or id")
	}
	return created.Token, created.ID, nil
}

// deleteToken deletes the v3 session token name through the proxy route with
// the seed token.
func (c *compositeTarget) deleteToken(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.srv.URL+token.V3TokensPath+"/"+name, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking facade token %s: status %d", name, resp.StatusCode)
	}
	return nil
}

func TestFidelityConformance(t *testing.T) {
	tgt := newCompositeTarget(t)
	for _, p := range fidelity.All() {
		t.Run(p.Name, func(t *testing.T) {
			if p.OracleOnly {
				t.Skipf("oracle-only probe: recorded against the real server, no conformance claim")
			}
			if p.Downstream {
				t.Skipf("downstream probe: the facade has no downstream cluster, no conformance claim")
			}
			oracle, err := fidelity.LoadGolden(fidelity.GoldenPath(filepath.Join(fidelityDir, "oracle"), p.Name))
			if errors.Is(err, os.ErrNotExist) {
				t.Skipf("no oracle golden yet: record with make e2e-fidelity-update")
			}
			require.NoError(t, err)

			results, err := fidelity.RunProbe(t.Context(), tgt, p)
			require.NoError(t, err)
			got := fidelity.Golden{Observations: results}

			facadePath := fidelity.GoldenPath(filepath.Join(fidelityDir, "facade"), p.Name)
			if p.FacadeDivergence == "" {
				assert.Empty(t, fidelity.Diff(oracle, got),
					"facade diverges from the oracle; either fix the facade or pin with FacadeDivergence + a facade golden")
				return
			}

			if *updateFacade {
				require.NoError(t, fidelity.SaveGolden(facadePath, got))
			}
			facadeGolden, err := fidelity.LoadGolden(facadePath)
			require.NoError(t, err,
				"divergence-pinned probe %s needs a facade golden: run with -update-facade", p.Name)
			assert.Empty(t, fidelity.Diff(facadeGolden, got),
				"facade drifted from its own pin (%s)", p.FacadeDivergence)
			assert.NotEmpty(t, fidelity.Diff(oracle, facadeGolden),
				"facade now matches the oracle: remove the FacadeDivergence pin and its facade golden")
		})
	}
}

func newJSONBody(s string) *strings.Reader { return strings.NewReader(s) }

func decodeJSONBody(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}

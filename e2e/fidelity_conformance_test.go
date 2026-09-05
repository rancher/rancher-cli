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
	"record facade goldens for divergence-pinned probes (wave-4 reconciliation)")

// fidelityDir is the fidelity library's testdata root, relative to this
// package directory (e2e/).
var fidelityDir = filepath.Join("..", "internal", "testrancher", "fidelity", "testdata")

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
	router := newRouter(func(tok string) bool {
		return tok == testToken || (facade != nil && facade.ValidBearer(tok))
	})
	facade = testrancher.New(testrancher.Options{Fallthrough: router, SeedToken: testToken})
	ct.facade = facade
	ct.srv = httptest.NewTLSServer(facade)
	t.Cleanup(ct.srv.Close)
	return ct
}

func (c *compositeTarget) BaseURL() string      { return c.srv.URL }
func (c *compositeTarget) Client() *http.Client { return c.srv.Client() }

func (c *compositeTarget) Bearer(ctx context.Context, id fidelity.Identity) (string, error) {
	switch id {
	case fidelity.IdentityAdmin:
		return testToken, nil
	case fidelity.IdentityStandard:
		return c.standard.Get(func() (string, error) { return c.derivedBearer(ctx) })
	case fidelity.IdentityRevoked:
		return c.revoked.Get(func() (string, error) {
			bearer, err := c.derivedBearer(ctx)
			if err != nil {
				return "", err
			}
			name, ok := fidelity.NameFromExtBearer(bearer)
			if !ok {
				return "", fmt.Errorf("facade bearer is not ext-shaped (credential value withheld)")
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

func (c *compositeTarget) derivedBearer(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.srv.URL+"/v1-public/login",
		newJSONBody(`{"type":"localProvider","username":"e2e-user","password":"e2e-pass"}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("facade login: status %d", resp.StatusCode)
	}
	var created struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		} `json:"status"`
	}
	if err := decodeJSONBody(resp, &created); err != nil {
		return "", err
	}
	if created.Status.BearerToken == "" {
		return "", errors.New("facade login: empty bearerToken")
	}
	return created.Status.BearerToken, nil
}

func (c *compositeTarget) deleteToken(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.srv.URL+extTokensPathForTest+"/"+name, nil)
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
			oracle, err := fidelity.LoadGolden(fidelity.GoldenPath(filepath.Join(fidelityDir, "oracle"), p.Name))
			if errors.Is(err, os.ErrNotExist) {
				t.Skipf("no oracle golden yet: record with make e2e-real-update")
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

// extTokensPathForTest re-exports the production constant for the delete
// helper without a bare literal.
const extTokensPathForTest = token.ExtTokensPath

func newJSONBody(s string) *strings.Reader { return strings.NewReader(s) }

func decodeJSONBody(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}

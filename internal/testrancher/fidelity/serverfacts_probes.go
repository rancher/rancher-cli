package fidelity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/rancher/rancher-cli/internal/token"
)

const (
	// versionPath is the unauthenticated read of the server's version and
	// edition.
	versionPath = "/rancherversion"
	// installIDPath is the steve read of the setting that holds the
	// server's install id, at the server root.
	installIDPath = "/v1/management.cattle.io.settings/install-uuid"
	// installIDPlaceholder stands for the install id in a recorded body.
	installIDPlaceholder = "<install-id>"
	// roleLessBearer is the setup value that holds the login bearer of the
	// user with no global role. It is an input to a request only, never
	// rewritten into a recorded body.
	roleLessBearer = unrecordedPrefix + "rolelessbearer"
)

// installIDPattern is the lowercase hyphenated UUID form the CLI accepts as
// an install id.
var installIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// installIDHeaders are the headers the CLI's install id request sends.
var installIDHeaders = map[string]string{"Accept": "application/json"}

// versionRules keep the keys the CLI reads from /rancherversion (Version and
// RancherPrime) and drop the build's commit.
var versionRules = []NormalizeRule{
	{Path: "GitCommit", Replace: ""},
}

// installIDRules keep the id and type of the setting and replace its value:
// the steve envelope (metadata, which repeats the value under fields, links
// and actions) goes whole.
var installIDRules = []NormalizeRule{
	{Path: "metadata", Replace: ""},
	{Path: "links", Replace: ""},
	{Path: "actions", Replace: ""},
	{Path: "value", Replace: installIDPlaceholder},
}

// serverFactsProbes record the two reads the server facts refresh depends
// on. /rancherversion is kept beside the observation inside the capability
// probe because this probe's golden holds exactly the keys the CLI reads
// (Version, RancherPrime), one probe for each fact. The install id setting
// is read as a standard user (200), as an administrator (200) and as a user
// with no global role, which Rancher answers with 404 (the CLI stores a 403
// or 404 as not permitted). The facade serves neither path, so every probe
// is oracle-only.
func serverFactsProbes() []Probe {
	installID := func(name string, id Identity) Probe {
		return Probe{
			Name:       "serverfacts/" + name,
			OracleOnly: true,
			Setup:      []Step{installIDIsAUUID()},
			Observations: []Observation{
				{Name: "read", Request: RequestSpec{Method: http.MethodGet, Path: installIDPath, Headers: installIDHeaders, Identity: id}},
			},
			Normalize: installIDRules,
		}
	}
	return []Probe{
		{
			Name:         "serverfacts/rancherversion",
			OracleOnly:   true,
			Observations: []Observation{{Name: "read", Request: RequestSpec{Method: http.MethodGet, Path: versionPath, Identity: IdentityNone}}},
			Normalize:    versionRules,
		},
		installID("install-uuid-standard", IdentityStandard),
		installID("install-uuid-admin", IdentityAdmin),
		{
			Name:       "serverfacts/install-uuid-no-global-role",
			OracleOnly: true,
			Setup:      []Step{roleLessUserFixture()},
			Observations: []Observation{
				{Name: "read", Request: RequestSpec{Method: http.MethodGet, Path: installIDPath,
					Headers:  map[string]string{"Accept": "application/json", "Authorization": "Bearer {" + roleLessBearer + "}"},
					Identity: IdentityNone}},
			},
			Normalize: installIDRules,
			Teardown: []Step{
				deleteFixture("role-less user's login token", token.V3TokensPath+"/{rolelesslogin}"),
				deleteFixture("role-less user", "/v3/users/{rolelessuid}"),
			},
		},
	}
}

// installIDIsAUUID is the setup step that reads the setting as the
// administrator and fails the run unless its value is a lowercase hyphenated
// UUID. The recorded body holds a placeholder for the value, so this step is
// where the oracle's real value is checked against the form the CLI accepts.
func installIDIsAUUID() Step {
	return Step{
		Name: "the install id is a UUID",
		Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
			tok, err := tgt.Bearer(ctx, IdentityAdmin)
			if err != nil {
				return fmt.Errorf("resolving admin bearer: %w", err)
			}
			build := func() (*http.Request, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+installIDPath, nil)
				if err != nil {
					return nil, err
				}
				req.Header.Set("Accept", "application/json")
				req.Header.Set("Authorization", "Bearer "+tok)
				return req, nil
			}
			resp, err := doResilient(tgt.Client(), build)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("reading the install id as admin: status %d, want %d", resp.StatusCode, http.StatusOK)
			}
			var setting struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(raw, &setting); err != nil {
				return fmt.Errorf("reading the install id as admin: the body is not JSON: %w", err)
			}
			if !installIDPattern.MatchString(setting.Value) {
				return fmt.Errorf("the install id is not a lowercase hyphenated UUID (%d characters)", len(setting.Value))
			}
			return nil
		},
	}
}

// roleLessUserFixture creates a local user with no global role binding, as
// the user API leaves it, signs it in, and records its login bearer, the
// login token's id and the user's id for the observation and the teardown,
// which deletes the token and the user by those ids. The name and password
// are new on every run, so no earlier run's user is looked up or removed.
func roleLessUserFixture() Step {
	return Step{
		Name: "create a user with no global role and sign in",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			var b [12]byte
			if _, err := rand.Read(b[:]); err != nil {
				return fmt.Errorf("generating the user's name and password: %w", err)
			}
			suffix := hex.EncodeToString(b[:])
			name, password := "e2e-fid-roleless-"+suffix[:8], "e2e-fid-roleless-pw-"+suffix
			created, status, err := adminJSON(ctx, tgt, http.MethodPost, "/v3/users",
				fmt.Sprintf(`{"type":"user","enabled":true,"mustChangePassword":false,"username":%q,"password":%q}`, name, password))
			if err != nil {
				return fmt.Errorf("creating the user: %w", err)
			}
			id, _ := created["id"].(string)
			if status != http.StatusCreated || id == "" {
				return fmt.Errorf("creating the user: status %d, want %d with an id", status, http.StatusCreated)
			}
			captured["rolelessuid"] = id
			body, status, err := bearerJSON(ctx, tgt, "", http.MethodPost, loginPath,
				fmt.Sprintf(`{"type":"localProvider","username":%q,"password":%q,"responseType":"json"}`, name, password))
			if err != nil {
				return fmt.Errorf("signing in as the user: %w", err)
			}
			bearer, _ := body["token"].(string)
			loginID, _ := body["id"].(string)
			if loginID != "" {
				captured["rolelesslogin"] = loginID
			}
			if status/100 != 2 || bearer == "" || loginID == "" {
				return fmt.Errorf("signing in as the user: status %d, want 2xx with a token and an id", status)
			}
			captured[roleLessBearer] = bearer
			return nil
		},
	}
}

package fidelity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loginGoldenError reports why a login probe's golden does not record the
// token rule of the role the probe binds: the binding observation names the
// role, and the ext token create answered extStatus (201 with a Token, or
// 403 with a Forbidden Status for tokens in ext.cattle.io). A missing observation is an error, never a
// skip. It returns nil when the golden records the fact.
func loginGoldenError(g Golden, role string, extStatus int) error {
	find := func(name string) (ObservationResult, error) {
		i := slices.IndexFunc(g.Observations, func(r ObservationResult) bool { return r.Name == name })
		if i < 0 {
			return ObservationResult{}, fmt.Errorf("the golden has no %s observation", name)
		}
		return g.Observations[i], nil
	}
	binding, err := find("binding")
	if err != nil {
		return err
	}
	if got := binding.Body["globalRoleId"]; got != role {
		return fmt.Errorf("the binding observation records globalRoleId %v, want %s", got, role)
	}
	create, err := find("ext-create-with-the-login-bearer")
	if err != nil {
		return err
	}
	if create.Status != extStatus {
		return fmt.Errorf("the ext token create records status %d, want %d", create.Status, extStatus)
	}
	if extStatus == http.StatusCreated {
		if got := create.Body["kind"]; got != "Token" {
			return fmt.Errorf("the ext token create records kind %v, want Token", got)
		}
		spec, _ := create.Body["spec"].(map[string]any)
		if got := spec["userID"]; got != "{owneruid}" {
			return fmt.Errorf("the ext token create records spec.userID %v, want {owneruid}", got)
		}
		return nil
	}
	if got := create.Body["kind"]; got != "Status" {
		return fmt.Errorf("the ext token create records kind %v, want Status", got)
	}
	if got := create.Body["reason"]; got != "Forbidden" {
		return fmt.Errorf("the ext token create records reason %v, want Forbidden", got)
	}
	details, _ := create.Body["details"].(map[string]any)
	if got := details["group"]; got != "ext.cattle.io" {
		return fmt.Errorf("the ext token create records details.group %v, want ext.cattle.io", got)
	}
	if got := details["kind"]; got != "tokens" {
		return fmt.Errorf("the ext token create records details.kind %v, want tokens", got)
	}
	return nil
}

func loadLoginGolden(t *testing.T, probe string) Golden {
	t.Helper()
	g, err := LoadGolden(GoldenPath(filepath.Join("testdata", "oracle"), probe))
	require.NoError(t, err)
	return g
}

// TestLoginGoldensRecordTheTokenRule asserts the recorded goldens show a
// user whose only global role is user-base creating the ext token a login
// needs (201), and a user whose only global role is clusters-create refused
// (403): a re-record that loses either fact fails the build.
func TestLoginGoldensRecordTheTokenRule(t *testing.T) {
	t.Parallel()
	require.NoError(t, loginGoldenError(loadLoginGolden(t, "login/user-base-user-creates-an-ext-token"), "user-base", http.StatusCreated))
	require.NoError(t, loginGoldenError(loadLoginGolden(t, "login/clusters-create-user-cannot-create-an-ext-token"), "clusters-create", http.StatusForbidden))
}

// TestLoginGoldenCheckRejectsAGoldenThatLostTheFact asserts the check
// fails, with its own message, on a copy of each golden changed in memory:
// another status, another role, another body, a missing observation.
func TestLoginGoldenCheckRejectsAGoldenThatLostTheFact(t *testing.T) {
	t.Parallel()
	const (
		userBase = "login/user-base-user-creates-an-ext-token"
		control  = "login/clusters-create-user-cannot-create-an-ext-token"
		binding  = "binding"
		create   = "ext-create-with-the-login-bearer"
	)
	// mutate loads a copy of the probe's golden and applies change to its
	// observation named name, or removes the observation when change is nil.
	// An observation the golden lacks fails the row.
	mutate := func(t *testing.T, probe, name string, change func(o *ObservationResult)) Golden {
		t.Helper()
		raw, err := json.Marshal(loadLoginGolden(t, probe))
		require.NoError(t, err)
		var g Golden
		require.NoError(t, json.Unmarshal(raw, &g))
		i := slices.IndexFunc(g.Observations, func(r ObservationResult) bool { return r.Name == name })
		require.GreaterOrEqual(t, i, 0, "the golden of %s has no %s observation", probe, name)
		if change == nil {
			g.Observations = slices.Delete(g.Observations, i, i+1)
			return g
		}
		change(&g.Observations[i])
		return g
	}
	rows := []struct {
		name        string
		probe       string
		role        string
		status      int
		observation string
		change      func(o *ObservationResult)
		want        string
	}{
		{"user-base create refused", userBase, "user-base", http.StatusCreated, create,
			func(o *ObservationResult) { o.Status = http.StatusForbidden },
			"records status 403, want 201"},
		{"control create allowed", control, "clusters-create", http.StatusForbidden, create,
			func(o *ObservationResult) { o.Status = http.StatusCreated },
			"records status 201, want 403"},
		{"user-base bound to another role", userBase, "user-base", http.StatusCreated, binding,
			func(o *ObservationResult) { o.Body["globalRoleId"] = "user" },
			"records globalRoleId user, want user-base"},
		{"control bound to user-base", control, "clusters-create", http.StatusForbidden, binding,
			func(o *ObservationResult) { o.Body["globalRoleId"] = "user-base" },
			"records globalRoleId user-base, want clusters-create"},
		{"control refusal is not a Status", control, "clusters-create", http.StatusForbidden, create,
			func(o *ObservationResult) { o.Body["kind"] = "Token" },
			"records kind Token, want Status"},
		{"control refusal is not Forbidden", control, "clusters-create", http.StatusForbidden, create,
			func(o *ObservationResult) { o.Body["reason"] = "Unauthorized" },
			"records reason Unauthorized, want Forbidden"},
		{"control refusal names another group", control, "clusters-create", http.StatusForbidden, create,
			func(o *ObservationResult) { o.Body["details"].(map[string]any)["group"] = "management.cattle.io" },
			"records details.group management.cattle.io, want ext.cattle.io"},
		{"control refusal names another resource", control, "clusters-create", http.StatusForbidden, create,
			func(o *ObservationResult) { o.Body["details"].(map[string]any)["kind"] = "clusters" },
			"records details.kind clusters, want tokens"},
		{"user-base create answers no Token", userBase, "user-base", http.StatusCreated, create,
			func(o *ObservationResult) { o.Body["kind"] = "Status" },
			"records kind Status, want Token"},
		{"user-base token belongs to another user", userBase, "user-base", http.StatusCreated, create,
			func(o *ObservationResult) { o.Body["spec"].(map[string]any)["userID"] = "<user-id>" },
			"records spec.userID <user-id>, want {owneruid}"},
		{"binding observation missing", userBase, "user-base", http.StatusCreated, binding, nil,
			"has no binding observation"},
		{"create observation missing", control, "clusters-create", http.StatusForbidden, create, nil,
			"has no ext-create-with-the-login-bearer observation"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			g := mutate(t, row.probe, row.observation, row.change)
			assert.ErrorContains(t, loginGoldenError(g, row.role, row.status), row.want)
		})
	}
}

// TestOnlyBindingStepFailsOnAnotherBinding asserts the step passes for the
// user's one binding with the fixture's role, and fails when the list
// holds a second binding of the user (of another role or the same one), a
// binding of another role, no binding of the user, no data, or is paged, and
// when the list answers with another status. Bindings of other users are
// ignored.
func TestOnlyBindingStepFailsOnAnotherBinding(t *testing.T) {
	t.Parallel()
	own := `{"id":"grb-own","userId":"u-1","globalRoleId":"user-base"}`
	rows := []struct {
		name   string
		list   string
		status int
		want   string
	}{
		{"one binding", `{"data":[` + own + `,{"id":"grb-other","userId":"u-2","globalRoleId":"user"}]}`, http.StatusOK, ""},
		{"a default binding added", `{"data":[` + own + `,{"id":"grb-default","userId":"u-1","globalRoleId":"user"}]}`, http.StatusOK, "the user has the binding grb-default (user)"},
		{"a second binding of the same role", `{"data":[` + own + `,{"id":"grb-twin","userId":"u-1","globalRoleId":"user-base"}]}`, http.StatusOK, "the user has the binding grb-twin (user-base)"},
		{"the role changed", `{"data":[{"id":"grb-own","userId":"u-1","globalRoleId":"user"}]}`, http.StatusOK, "the user has the binding grb-own (user)"},
		{"no binding", `{"data":[{"id":"grb-other","userId":"u-2","globalRoleId":"user"}]}`, http.StatusOK, "the user has 0 global role bindings"},
		{"no data", `{}`, http.StatusOK, "the user has 0 global role bindings"},
		{"paged list", `{"data":[` + own + `],"pagination":{"next":"/v3/globalrolebindings?marker=1"}}`, http.StatusOK, "the list is paged"},
		{"another status", `{"type":"error","status":"403"}`, http.StatusForbidden, "listing global role bindings: status 403"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(row.status)
				_, _ = w.Write([]byte(row.list))
			}))
			defer srv.Close()
			captured := map[string]string{"owneruid": "u-1", "ownergrb": "grb-own"}
			err := onlyBindingStep("only one", "user-base").Do(t.Context(), adminTarget(srv), captured)
			if row.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, row.want)
		})
	}
}

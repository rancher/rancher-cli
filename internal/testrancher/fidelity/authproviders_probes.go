package fidelity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	// authProvidersPath is the public list the CLI reads to decide what a
	// person can sign in with. Rancher answers it without a credential.
	authProvidersPath = "/v1-public/authproviders"

	// authConfigsPath is the collection of auth configs, reached through the
	// local cluster's proxy.
	authConfigsPath = "/k8s/clusters/local/apis/management.cattle.io/v3/authconfigs"

	// authCleanupAnnotation decides what Rancher does with a config it sees
	// disabled or enabled: a disabled config marked "unlocked" is reset and
	// every user, token and binding of its provider is deleted, and an enabled
	// config marked "rancher-locked" is changed to "unlocked". Every write of
	// these probes therefore sends the annotation and the value of enabled in
	// one request, so Rancher never sees an enabled config marked
	// "rancher-locked" or a disabled one marked "unlocked".
	authCleanupAnnotation = "management.cattle.io/auth-provider-cleanup"
	authCleanupLocked     = "rancher-locked"
	authCleanupUserLocked = "user-locked"

	mergePatchContentType = "application/merge-patch+json"
)

// authProvidersPollInterval is the pause between two reads of the list while
// a step waits for it to change. The step's own context, 30 seconds from the
// executor, ends the wait.
const authProvidersPollInterval = 3 * time.Second

// authConfigEdit is one auth config the external-providers probe enables: its
// name, and the fields besides enabled and the cleanup annotation that its
// list entry reads. Every value is a constant with the e2e-fid- prefix or a
// .invalid host, never a real address.
type authConfigEdit struct {
	name   string
	fields map[string]any
}

// authConfigEdits are the only objects the authproviders probes write, in the
// order they are enabled. The fields are the ones Rancher's
// TransformToAuthProvider reads, plus the one the webhook requires of an
// enabled LDAP config: at least one server.
var authConfigEdits = []authConfigEdit{
	{name: "openldap", fields: map[string]any{
		"servers": []string{"e2e-fid-ldap.invalid"},
	}},
	{name: "okta", fields: map[string]any{
		"rancherApiHost": "https://e2e-fid-okta.invalid",
	}},
	{name: "azuread", fields: map[string]any{
		"tenantId":           "e2e-fid-tenant",
		"applicationId":      "e2e-fid-app",
		"endpoint":           "https://e2e-fid-azure.invalid/",
		"authEndpoint":       "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/authorize",
		"tokenEndpoint":      "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/token",
		"deviceAuthEndpoint": "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/devicecode",
		"rancherUrl":         "https://e2e-fid-rancher.invalid",
	}},
}

// authProviderProbes records the public list of auth providers:
//
//   - authproviders/only-local-enabled: the list of a server where nothing
//     but the local provider is enabled, which the setup requires. It writes
//     nothing and runs against the facade too, which lists the local entry
//     without the three logout booleans Rancher sends on every entry.
//   - authproviders/external-providers-enabled: the list with OpenLDAP, Okta
//     (SAML) and Azure AD (azureADProvider) enabled beside local, the three
//     shapes of entry the CLI reads. The probe sorts the list by id
//     (Rancher's order is not fixed). The setup enables the three configs
//     by writing enabled, the cleanup annotation and the fields their entries
//     read, waits until the list shows them, and the teardown puts every
//     field back and waits until the list holds local alone. The probe is
//     oracle-only: the facade holds no auth config to write to, so a test in
//     internal/testrancher compares the facade's entries with this recording.
//
// A fresh Rancher v2.16 server creates the auth config of local only. The
// second probe therefore declares MaxMinor "2.15": against a newer server it
// sends only the request for the server version and writes nothing.
//
// The only objects written are authconfigs/openldap, authconfigs/okta and
// authconfigs/azuread, by merge patch. An enabled config is not signed in
// through: no request leaves Rancher for an identity provider.
func authProviderProbes() []Probe {
	names := make([]string, 0, len(authConfigEdits))
	for _, e := range authConfigEdits {
		names = append(names, e.name)
	}
	setup := []Step{
		authProvidersAreLocalAlone(),
		readAuthConfigOriginals(authConfigEdits),
	}
	for _, e := range authConfigEdits {
		setup = append(setup, enableAuthConfig(e))
	}
	setup = append(setup, authProvidersAreListed("wait for the enabled auth providers to be listed",
		append([]string{"local"}, names...), authProvidersPollInterval))

	var teardown []Step
	for _, e := range slices.Backward(authConfigEdits) {
		teardown = append(teardown, restoreAuthConfig(e))
	}
	teardown = append(teardown,
		readBackAuthConfigs(authConfigEdits),
		whenAuthConfigsWereWritten(authConfigEdits, authProvidersAreListed("wait for the auth provider list to hold local alone",
			[]string{"local"}, authProvidersPollInterval)),
	)

	list := Observation{Name: "list", Request: RequestSpec{Method: http.MethodGet, Path: authProvidersPath, Identity: IdentityNone}}
	return []Probe{
		{
			Name:             "authproviders/only-local-enabled",
			Setup:            []Step{authProvidersAreLocalAlone()},
			Observations:     []Observation{list},
			FacadeDivergence: "the facade omits the three logout booleans Rancher sends on every entry; the CLI reads none",
		},
		{
			Name:         "authproviders/external-providers-enabled",
			OracleOnly:   true,
			MaxMinor:     "2.15",
			Setup:        setup,
			Observations: []Observation{list},
			Normalize:    []NormalizeRule{{Path: "data", SortBy: "id"}},
			Teardown:     teardown,
		},
	}
}

func authConfigPath(name string) string { return authConfigsPath + "/" + name }

// authConfigWroteKey is the capture a step sets before it sends the first
// write to an auth config, so a write whose answer was lost is still put back.
func authConfigWroteKey(name string) string {
	return unrecordedPrefix + "authconfig." + name + ".wrote"
}

// authConfigOriginalKey is the capture that holds, as JSON text, the original
// value of every field the probe sets on an auth config: enabled (null when
// the field was absent), the cleanup annotation under "annotation", and each
// field of the edit (null when absent).
func authConfigOriginalKey(name string) string {
	return unrecordedPrefix + "authconfig." + name + ".original"
}

// authProviderIDs reads the public list and returns the id of every entry,
// sorted. Any status but 200 is an error.
func authProviderIDs(ctx context.Context, tgt Target) ([]string, error) {
	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+authProvidersPath, nil)
	}
	resp, err := doResilient(tgt.Client(), build)
	if err != nil {
		return nil, fmt.Errorf("reading the auth provider list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the auth provider list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading the auth provider list: status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("reading the auth provider list: the body is not a list: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, entry := range list.Data {
		ids = append(ids, entry.ID)
	}
	slices.Sort(ids)
	return ids, nil
}

// authProvidersAreLocalAlone is the setup step that fails the run unless the
// list holds the local entry and nothing else: a recorded list must not carry
// a provider a previous run or another session left enabled.
func authProvidersAreLocalAlone() Step {
	return Step{
		Name: "the auth provider list holds local alone",
		Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
			ids, err := authProviderIDs(ctx, tgt)
			if err != nil {
				return err
			}
			if slices.Equal(ids, []string{"local"}) {
				return nil
			}
			return fmt.Errorf("the auth provider list holds %q, want local alone%s", ids, authProviderListAdvice(ids))
		},
	}
}

// authProviderListAdvice says what to do about the entries a list holds
// beyond local: the three configs these probes enable must be disabled again,
// with the annotation Rancher needs to leave them alone; any other entry is
// not one these probes enabled.
func authProviderListAdvice(ids []string) string {
	var ours, others []string
	for _, id := range ids {
		switch {
		case id == "local":
		case slices.ContainsFunc(authConfigEdits, func(e authConfigEdit) bool { return e.name == id }):
			ours = append(ours, "authconfigs/"+id)
		default:
			others = append(others, id)
		}
	}
	var advice []string
	if len(ours) > 0 {
		advice = append(advice, fmt.Sprintf("a previous run may have left %s enabled: disable it again with annotation %s set to %s",
			strings.Join(ours, ", "), authCleanupAnnotation, authCleanupLocked))
	}
	if len(others) > 0 {
		advice = append(advice, fmt.Sprintf("these probes did not enable %q", others))
	}
	if !slices.Contains(ids, "local") {
		advice = append(advice, "local is not listed")
	}
	if len(advice) == 0 {
		return ""
	}
	return ": " + strings.Join(advice, "; ")
}

// authProvidersAreListed waits until the list holds exactly the ids in want,
// polling every interval until the step's context ends, and fails with the
// last list it read. A status other than 200 fails at once.
func authProvidersAreListed(name string, want []string, interval time.Duration) Step {
	want = slices.Sorted(slices.Values(want))
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
			var last []string
			for {
				ids, err := authProviderIDs(ctx, tgt)
				if err != nil {
					if ctx.Err() != nil && last != nil {
						return fmt.Errorf("%s: not ready, the last list held %q, want %q%s: %w", name, last, want, missingLocal(last, want), ctx.Err())
					}
					return fmt.Errorf("%s: %w", name, err)
				}
				if slices.Equal(ids, want) {
					return nil
				}
				last = ids
				select {
				case <-ctx.Done():
					return fmt.Errorf("%s: not ready, the last list held %q, want %q%s: %w", name, last, want, missingLocal(last, want), ctx.Err())
				case <-time.After(interval):
				}
			}
		},
	}
}

// missingLocal is the hint added to a wait that ran out with local missing
// from the list it wanted: Rancher drops local from the list when the
// hide-local-auth-provider feature is on and any other provider is enabled.
func missingLocal(last, want []string) string {
	if slices.Contains(want, "local") && !slices.Contains(last, "local") {
		return "; local is missing: the hide-local-auth-provider feature may be on"
	}
	return ""
}

// readAuthConfigOriginals is the setup step that reads each auth config as
// admin and fails the run, before anything is written, unless the config
// exists, is disabled (enabled absent or false), carries the "rancher-locked"
// cleanup annotation, and lacks every field the probe sets. It records the
// original of each in the capture the restore reads. Only the fields the probe
// sets are required absent: Rancher also adds status.conditions to an enabled
// config, and rewrites Okta's SAML fields and openLdapConfig on the first
// sync of an enabled Okta config, and the restore leaves those as they are.
func readAuthConfigOriginals(edits []authConfigEdit) Step {
	return Step{
		Name: "read the originals of the auth configs",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			originals := make(map[string]string, len(edits))
			for _, e := range edits {
				doc, status, err := adminJSON(ctx, tgt, http.MethodGet, authConfigPath(e.name), "")
				if err != nil {
					return fmt.Errorf("reading auth config %s: %w", e.name, err)
				}
				if status == http.StatusNotFound {
					return fmt.Errorf("auth config %s does not exist on this server (a fresh Rancher v2.16 server creates only local)", e.name)
				}
				if status != http.StatusOK || doc == nil {
					return fmt.Errorf("reading auth config %s: status %d, want %d with an object", e.name, status, http.StatusOK)
				}
				original := map[string]json.RawMessage{}
				switch enabled, present := doc["enabled"]; {
				case !present || enabled == nil:
					original["enabled"] = json.RawMessage("null")
				case enabled == false:
					original["enabled"] = json.RawMessage("false")
				default:
					return fmt.Errorf("auth config %s is not disabled: enabled is %v", e.name, enabled)
				}
				meta, _ := doc["metadata"].(map[string]any)
				annotations, _ := meta["annotations"].(map[string]any)
				if got, _ := annotations[authCleanupAnnotation].(string); got != authCleanupLocked {
					return fmt.Errorf("auth config %s has annotation %s %q, want %q", e.name, authCleanupAnnotation, got, authCleanupLocked)
				}
				original["annotation"] = json.RawMessage(`"` + authCleanupLocked + `"`)
				for _, field := range slices.Sorted(maps.Keys(e.fields)) {
					if v, present := doc[field]; present && v != nil {
						return fmt.Errorf("auth config %s already has field %s", e.name, field)
					}
					original[field] = json.RawMessage("null")
				}
				text, err := json.Marshal(original)
				if err != nil {
					return fmt.Errorf("recording the original of auth config %s: %w", e.name, err)
				}
				originals[e.name] = string(text)
			}
			for name, text := range originals {
				captured[authConfigOriginalKey(name)] = text
			}
			return nil
		},
	}
}

// mergePatchAdmin sends one PATCH as admin with the content type the
// Kubernetes API requires for a JSON merge patch, and returns the status and
// the start of the answer's body. A PATCH is never retried.
func mergePatchAdmin(ctx context.Context, tgt Target, path string, patch map[string]any) (int, string, error) {
	body, err := json.Marshal(patch)
	if err != nil {
		return 0, "", err
	}
	tok, err := tgt.Bearer(ctx, IdentityAdmin)
	if err != nil {
		return 0, "", fmt.Errorf("resolving admin bearer: %w", err)
	}
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, tgt.BaseURL()+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mergePatchContentType)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req, nil
	}
	resp, err := doResilient(tgt.Client(), build)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, strings.TrimSpace(string(raw)), nil
}

// patchFailure formats the error of a write the server did not accept.
func patchFailure(what string, status int, body string) error {
	if body == "" {
		return fmt.Errorf("%s: status %d", what, status)
	}
	return fmt.Errorf("%s: status %d: %s", what, status, body)
}

// enableAuthConfig enables one auth config: enabled true, the annotation
// "user-locked" (Rancher resets and cleans up neither a config marked so nor
// an enabled one), and the fields of its list entry, in one request. The
// capture that marks the config as written is set before the request is
// sent, so a request that reached the server but was answered with an error
// or lost is still restored by the teardown.
func enableAuthConfig(e authConfigEdit) Step {
	return Step{
		Name: "enable auth config " + e.name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			if captured[authConfigOriginalKey(e.name)] == "" {
				return fmt.Errorf("the original of auth config %s was not recorded: read it before writing", e.name)
			}
			captured[authConfigWroteKey(e.name)] = "true"
			patch := map[string]any{
				"enabled":  true,
				"metadata": map[string]any{"annotations": map[string]any{authCleanupAnnotation: authCleanupUserLocked}},
			}
			maps.Copy(patch, e.fields)
			status, body, err := mergePatchAdmin(ctx, tgt, authConfigPath(e.name), patch)
			if err != nil {
				return fmt.Errorf("enabling auth config %s: %w", e.name, err)
			}
			if status/100 != 2 {
				return patchFailure("enabling auth config "+e.name, status, body)
			}
			return nil
		},
	}
}

// originalsOf decodes the originals the read step recorded for an auth config.
func originalsOf(e authConfigEdit, captured map[string]string) (map[string]json.RawMessage, error) {
	text := captured[authConfigOriginalKey(e.name)]
	if text == "" {
		return nil, fmt.Errorf("auth config %s was written but its original was not recorded", e.name)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &original); err != nil {
		return nil, fmt.Errorf("the recorded original of auth config %s is not JSON: %w", e.name, err)
	}
	for _, key := range append([]string{"enabled", "annotation"}, slices.Collect(maps.Keys(e.fields))...) {
		if _, ok := original[key]; !ok {
			return nil, fmt.Errorf("the recorded original of auth config %s has no %s", e.name, key)
		}
	}
	return original, nil
}

// authRestoreAttempts and authRestorePause bound the retries of a restore
// after a transport error, a 5xx answer or a 429 answer. The restore body
// holds absolute values, so sending it again is harmless; any other 4xx
// answer is final.
const (
	authRestoreAttempts = 4
	authRestorePause    = 500 * time.Millisecond
)

// patchRestore sends a restore patch and sends it again, up to
// authRestoreAttempts times in all, after a transport error, a 5xx or a 429
// answer, while the step's context lasts. It returns the last status, body
// and error.
func patchRestore(ctx context.Context, tgt Target, path string, patch map[string]any) (int, string, error) {
	for attempt := 1; ; attempt++ {
		status, body, err := mergePatchAdmin(ctx, tgt, path, patch)
		if (err == nil && status < 500 && status != http.StatusTooManyRequests) || attempt == authRestoreAttempts || ctx.Err() != nil {
			return status, body, err
		}
		select {
		case <-ctx.Done():
			return status, body, err
		case <-time.After(authRestorePause):
		}
	}
}

// restoreAuthConfig puts one auth config back as the read step recorded it,
// in one request: enabled, the annotation and every field the enable step
// set, an absent original sent as null, which a merge patch reads as delete.
// It sends the patch again after a transport error, a 5xx or a 429 answer.
// It sends nothing when its enable step never ran. A config whose write
// started but whose original is missing is an error.
func restoreAuthConfig(e authConfigEdit) Step {
	return Step{
		Name: "restore auth config " + e.name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			if captured[authConfigWroteKey(e.name)] == "" {
				return nil
			}
			original, err := originalsOf(e, captured)
			if err != nil {
				return err
			}
			patch := map[string]any{
				"enabled":  original["enabled"],
				"metadata": map[string]any{"annotations": map[string]any{authCleanupAnnotation: original["annotation"]}},
			}
			for field := range e.fields {
				patch[field] = original[field]
			}
			status, body, err := patchRestore(ctx, tgt, authConfigPath(e.name), patch)
			if err != nil {
				return fmt.Errorf("restoring auth config %s: %w", e.name, err)
			}
			if status/100 != 2 {
				return patchFailure("restoring auth config "+e.name, status, body)
			}
			return nil
		},
	}
}

// readBackAuthConfigs reads every auth config the probe wrote back as admin
// and fails the run unless enabled, the annotation and each field the enable
// step set equal their originals. Absent and false are the same value for
// enabled. A status other than 200 fails at once; it sends nothing when no
// config was written.
func readBackAuthConfigs(edits []authConfigEdit) Step {
	return Step{
		Name: "read back the auth configs",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			var errs []error
			for _, e := range edits {
				if captured[authConfigWroteKey(e.name)] == "" {
					continue
				}
				original, err := originalsOf(e, captured)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				doc, status, err := adminJSON(ctx, tgt, http.MethodGet, authConfigPath(e.name), "")
				if err != nil {
					return fmt.Errorf("reading back auth config %s: %w", e.name, err)
				}
				if status != http.StatusOK || doc == nil {
					return fmt.Errorf("reading back auth config %s: status %d, want %d with an object", e.name, status, http.StatusOK)
				}
				errs = append(errs, authConfigDifferences(e, doc, original)...)
			}
			return errors.Join(errs...)
		},
	}
}

// authConfigDifferences lists how doc differs from the originals in the
// fields the probe sets.
func authConfigDifferences(e authConfigEdit, doc map[string]any, original map[string]json.RawMessage) []error {
	var errs []error
	if wasEnabled, isEnabled := string(original["enabled"]) == "true", doc["enabled"] == true; wasEnabled != isEnabled {
		errs = append(errs, fmt.Errorf("auth config %s after the restore: enabled is %v, the original was %s", e.name, doc["enabled"], original["enabled"]))
	}
	meta, _ := doc["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	if got, _ := json.Marshal(annotations[authCleanupAnnotation]); !jsonEqual(got, original["annotation"]) {
		errs = append(errs, fmt.Errorf("auth config %s after the restore: annotation %s is %s, the original was %s", e.name, authCleanupAnnotation, got, original["annotation"]))
	}
	for _, field := range slices.Sorted(maps.Keys(e.fields)) {
		got, _ := json.Marshal(doc[field])
		if !jsonEqual(got, original[field]) {
			errs = append(errs, fmt.Errorf("auth config %s after the restore: field %s is %s, the original was %s", e.name, field, got, original[field]))
		}
	}
	return errs
}

// jsonEqual reports whether two JSON texts hold the same value in compact form.
func jsonEqual(a, b []byte) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// whenAuthConfigsWereWritten runs step only when an enable step marked one of
// edits as written, and returns nil at once otherwise: a run that failed before
// any write has nothing to wait for.
func whenAuthConfigsWereWritten(edits []authConfigEdit, step Step) Step {
	return Step{
		Name: step.Name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			if !slices.ContainsFunc(edits, func(e authConfigEdit) bool { return captured[authConfigWroteKey(e.name)] != "" }) {
				return nil
			}
			return step.Do(ctx, tgt, captured)
		},
	}
}

package fidelity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/token"
)

func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder appends a newline; trim it so callers get compact output.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// LazyBearer memoizes a bearer provisioned on first use. It caches only on
// success - a failed provision leaves the slot empty so the next Get retries -
// and serializes concurrent callers so provisioning happens at most once.
type LazyBearer struct {
	mu  sync.Mutex
	val string
}

// Get returns the memoized bearer, invoking provision under the lock the first
// time (and again after any failure) until it succeeds.
func (l *LazyBearer) Get(provision func() (string, error)) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.val != "" {
		return l.val, nil
	}
	v, err := provision()
	if err != nil {
		return "", err
	}
	l.val = v
	return v, nil
}

// OracleTarget is the recorded oracle: a real Rancher reached through the
// E2E_FIDELITY_* environment scripts/e2e-fidelity exports. Standard and revoked
// bearers are provisioned lazily against the live server and memoized.
type OracleTarget struct {
	baseURL    string
	client     *http.Client
	admin      string
	downstream string

	standard LazyBearer
	member   LazyBearer
	revoked  LazyBearer
}

// systemCACert is the literal E2E_FIDELITY_CACERT value that means "trust the
// operating system's root store" instead of an inline PEM - the shape an
// external target with a publicly trusted certificate needs, since it has
// no CA of its own to pin.
const systemCACert = "system"

// NewOracleTargetFromEnv builds the oracle client from E2E_FIDELITY_URL,
// E2E_FIDELITY_ADMIN_TOKEN, and E2E_FIDELITY_CACERT (inline PEM, or the literal
// "system" to trust the OS root store). The CA is mandatory: this harness
// never skips TLS verification. E2E_FIDELITY_DOWNSTREAM, if set, names a
// downstream cluster the target can reach through the proxy.
func NewOracleTargetFromEnv() (*OracleTarget, error) {
	base := os.Getenv("E2E_FIDELITY_URL")
	admin := os.Getenv("E2E_FIDELITY_ADMIN_TOKEN")
	ca := os.Getenv("E2E_FIDELITY_CACERT")
	if base == "" || admin == "" || ca == "" {
		return nil, fmt.Errorf("E2E_FIDELITY_URL, E2E_FIDELITY_ADMIN_TOKEN, and E2E_FIDELITY_CACERT must all be set (run via scripts/e2e-fidelity)")
	}
	opts := httpx.Options{CACert: ca}
	if ca == systemCACert {
		opts = httpx.Options{}
	}
	client, err := httpx.New(opts)
	if err != nil {
		return nil, fmt.Errorf("E2E_FIDELITY_CACERT: %w", err)
	}
	return &OracleTarget{
		baseURL:    strings.TrimSuffix(base, "/"),
		admin:      admin,
		client:     client,
		downstream: os.Getenv("E2E_FIDELITY_DOWNSTREAM"),
	}, nil
}

// BaseURL is the oracle server root without a trailing slash.
func (t *OracleTarget) BaseURL() string { return t.baseURL }

// Downstream is the id of the downstream cluster named by
// E2E_FIDELITY_DOWNSTREAM, or empty when it is unset.
func (t *OracleTarget) Downstream() string { return t.downstream }

// Client is an HTTP client that trusts the oracle's CA (TLS verification is
// never skipped), refuses cross-origin redirects, and honours the proxy
// environment variables, exactly like every Rancher-scoped client the CLI
// builds; a LAN-bound oracle behind HTTPS_PROXY is reached the way the CLI
// would reach it.
func (t *OracleTarget) Client() *http.Client { return t.client }

// Bearer resolves the admin, standard, or revoked identity to a live bearer
// against the real server. The admin token is supplied by the harness; the
// standard and revoked bearers are provisioned lazily and memoized. none and
// garbage are resolved by the executor, not here.
func (t *OracleTarget) Bearer(ctx context.Context, id Identity) (string, error) {
	switch id {
	case IdentityAdmin:
		return t.admin, nil
	case IdentityStandard:
		return t.standardBearer(ctx)
	case IdentityMember:
		return t.memberBearer(ctx)
	case IdentityRevoked:
		return t.revokedBearer(ctx)
	default:
		return "", fmt.Errorf("identity %q is resolved by the executor, not the target", id)
	}
}

const standardUsername = "e2e-fid-standard"

const memberUsername = "e2e-fid-member"

// newStandardPassword generates a fresh random password for the throwaway standard
// user. It is generated per OracleTarget instance rather than hardcoded so no
// usable credential for a provisioned user lives in this (public) source.
func newStandardPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating standard-user password: %w", err)
	}
	return "e2e-fid-standard-pw-" + hex.EncodeToString(b[:]), nil
}

// standardBearer provisions the e2e-fid-standard local user (idempotently:
// delete-before-create keeps kept-container reruns clean), binds the
// baseline `user` global role - a bare /v3/users create attaches no global
// role bindings, and a role-less user is not the probes' "standard identity":
// it gets schema-hidden responses (an empty 200 collection, a bodyless 404)
// rather than a baseline user's, so the role realizes that identity - and
// logs in as it. Cleanup relies on Rancher cascading a deleted user's global
// role bindings.
func (t *OracleTarget) standardBearer(ctx context.Context) (string, error) {
	return t.standard.Get(func() (string, error) {
		password, err := newStandardPassword()
		if err != nil {
			return "", err
		}
		if err := t.deleteStandardUser(ctx); err != nil {
			return "", err
		}
		create := map[string]any{
			"type":               "user",
			"enabled":            true,
			"mustChangePassword": false,
			"username":           standardUsername,
			"password":           password,
		}
		created, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/users", create, http.StatusCreated)
		if err != nil {
			return "", fmt.Errorf("creating standard user: %w", err)
		}
		userID, _ := created["id"].(string)
		if userID == "" {
			return "", fmt.Errorf("creating standard user: create response has no id")
		}
		grb := map[string]any{
			"type":         "globalRoleBinding",
			"globalRoleId": "user",
			"userId":       userID,
		}
		if _, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/globalrolebindings", grb, http.StatusCreated); err != nil {
			return "", fmt.Errorf("binding baseline user role to standard user: %w", err)
		}
		return t.login(ctx, standardUsername, password)
	})
}

// memberBearer provisions the e2e-fid-member local user (idempotently, as
// standardBearer does) with the baseline user GlobalRole and one
// project-member ProjectRoleTemplateBinding on the downstream cluster's
// Default project, and logs in as it. The downstream is required: a member
// identity without a project to be a member of does not exist. Cleanup
// relies on Rancher cascading a deleted user's bindings.
func (t *OracleTarget) memberBearer(ctx context.Context) (string, error) {
	return t.member.Get(func() (string, error) {
		if t.downstream == "" {
			return "", fmt.Errorf("the member identity needs a downstream cluster (E2E_FIDELITY_DOWNSTREAM)")
		}
		password, err := newStandardPassword()
		if err != nil {
			return "", err
		}
		if _, err := t.deleteUsersNamed(ctx, memberUsername); err != nil {
			return "", err
		}
		create := map[string]any{
			"type":               "user",
			"enabled":            true,
			"mustChangePassword": false,
			"username":           memberUsername,
			"password":           password,
		}
		created, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/users", create, http.StatusCreated)
		if err != nil {
			return "", fmt.Errorf("creating member user: %w", err)
		}
		userID, _ := created["id"].(string)
		if userID == "" {
			return "", fmt.Errorf("creating member user: create response has no id")
		}
		grb := map[string]any{"type": "globalRoleBinding", "globalRoleId": "user", "userId": userID}
		if _, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/globalrolebindings", grb, http.StatusCreated); err != nil {
			return "", fmt.Errorf("binding baseline user role to member user: %w", err)
		}
		projects, status, err := t.adminJSON(ctx, http.MethodGet, "/v3/projects?clusterId="+t.downstream+"&name=Default", nil, 0)
		if err != nil {
			return "", fmt.Errorf("looking up the downstream Default project: %w", err)
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("looking up the downstream Default project: status %d", status)
		}
		data, _ := projects["data"].([]any)
		if len(data) != 1 {
			return "", fmt.Errorf("looking up the downstream Default project: %d matches", len(data))
		}
		project, _ := data[0].(map[string]any)
		projectID, _ := project["id"].(string)
		if projectID == "" {
			return "", fmt.Errorf("looking up the downstream Default project: entry without id")
		}
		prtb := map[string]any{
			"type":           "projectRoleTemplateBinding",
			"projectId":      projectID,
			"roleTemplateId": "project-member",
			"userId":         userID,
		}
		if _, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/projectroletemplatebindings", prtb, http.StatusCreated); err != nil {
			return "", fmt.Errorf("binding project-member to member user: %w", err)
		}
		return t.login(ctx, memberUsername, password)
	})
}

// Cleanup removes the users the target provisioned (the standard and the
// member identities) and waits until Rancher has cascaded their
// GlobalRoleBindings and ProjectRoleTemplateBindings, so a kept oracle or an
// operator's own Rancher is left as it was found. Idempotent: a user that
// was never provisioned, or is already gone, is not an error. Every
// identity and every phase (delete, binding wait per deleted user) is
// attempted regardless of the others' outcome, so a failure on one user
// never leaves the other behind; the errors are joined, each naming its
// user and phase.
func (t *OracleTarget) Cleanup(ctx context.Context) error {
	var errs []error
	for _, username := range []string{standardUsername, memberUsername} {
		if err := t.cleanupUser(ctx, username); err != nil {
			errs = append(errs, fmt.Errorf("cleaning up %s: %w", username, err))
		}
	}
	return errors.Join(errs...)
}

// cleanupUser deletes every user carrying username and waits for the
// bindings of each deleted user to disappear. The ids come from the delete
// itself (exactly the users whose DELETE the server accepted), so a wait
// is never skipped for a removed user nor spent on one that is still
// there; every wait runs even when the delete or an earlier wait failed.
func (t *OracleTarget) cleanupUser(ctx context.Context, username string) error {
	var errs []error
	ids, err := t.deleteUsersNamed(ctx, username)
	if err != nil {
		errs = append(errs, fmt.Errorf("delete: %w", err))
	}
	for _, id := range ids {
		if err := t.waitBindingsGone(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("bindings of %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// waitBindingsGone waits for the GlobalRoleBindings and
// ProjectRoleTemplateBindings of a deleted user to disappear (Rancher
// cascades them asynchronously) and reports what remains after the wait.
func (t *OracleTarget) waitBindingsGone(ctx context.Context, userID string) error {
	deadline := time.Now().Add(bindingsGoneTimeout)
	for {
		remaining := 0
		for _, path := range []string{"/v3/globalrolebindings?userId=" + userID, "/v3/projectroletemplatebindings?userId=" + userID} {
			body, status, err := t.adminJSON(ctx, http.MethodGet, path, nil, 0)
			if err != nil {
				return fmt.Errorf("listing bindings of deleted user %s: %w", userID, err)
			}
			if status != http.StatusOK {
				return fmt.Errorf("listing bindings of deleted user %s: status %d", userID, status)
			}
			data, _ := body["data"].([]any)
			remaining += len(data)
		}
		if remaining == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d binding(s) of deleted user %s still present after %s", remaining, userID, bindingsGoneTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// bindingsGoneTimeout bounds the wait for Rancher to cascade a deleted
// user's bindings.
var bindingsGoneTimeout = 30 * time.Second

// revokedBearer creates a derived token as admin, then revokes it by the name
// embedded in the ext bearer.
func (t *OracleTarget) revokedBearer(ctx context.Context) (string, error) {
	return t.revoked.Get(func() (string, error) {
		body, _, err := t.adminJSON(ctx, http.MethodPost, token.ExtTokensPath, map[string]any{"spec": map[string]any{}}, http.StatusCreated)
		if err != nil {
			return "", fmt.Errorf("creating token to revoke: %w", err)
		}
		bearer := extBearerFrom(body)
		name, ok := NameFromExtBearer(bearer)
		if !ok {
			return "", fmt.Errorf("create response bearer is not ext-shaped (credential value withheld)")
		}
		if _, _, err := t.adminJSON(ctx, http.MethodDelete, token.ExtTokensPath+"/"+name, nil, http.StatusOK); err != nil {
			return "", fmt.Errorf("revoking token %s: %w", name, err)
		}
		return bearer, nil
	})
}

// standardUserGoneTimeout bounds the wait for a deleted standard user to
// leave the username lookup. Rancher deletes users asynchronously: on the
// pinned 2.15.1 the lookup still returned the deleted user about 250 ms
// after DELETE answered 200, and a create in that window can be refused
// as a duplicate username.
var standardUserGoneTimeout = 10 * time.Second

// deleteStandardUser removes any prior e2e-fid-standard user by username
// lookup and waits until the lookup no longer returns it. An absent user
// (empty data array) is clean; every HTTP failure, malformed 200 shape, or
// rejected delete is surfaced - a silently-kept stale user would break
// kept-run recovery at the subsequent create.
func (t *OracleTarget) deleteStandardUser(ctx context.Context) error {
	_, err := t.deleteUsersNamed(ctx, standardUsername)
	return err
}

// deleteUsersNamed removes every prior user with the given username and
// waits for the deletion to finish, so the create that follows never races
// the finalizer. Every listed entry gets its own DELETE attempt whatever
// happened to the ones before it; the failures are joined, and the
// disappearance wait runs only when every entry was accepted (a refused
// entry would keep the name listed). It returns the ids whose DELETE the
// server accepted, also on error, so a caller can still verify the cascade
// of what was removed.
func (t *OracleTarget) deleteUsersNamed(ctx context.Context, username string) (deleted []string, err error) {
	data, err := t.lookupUsersNamed(ctx, username)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var errs []error
	for _, el := range data {
		m, _ := el.(map[string]any)
		id, _ := m["id"].(string)
		if id == "" {
			errs = append(errs, fmt.Errorf("looking up user %s: collection entry without id", username))
			continue
		}
		_, dstatus, derr := t.adminJSON(ctx, http.MethodDelete, "/v3/users/"+id, nil, 0)
		if derr != nil {
			errs = append(errs, fmt.Errorf("deleting stale user %s: %w", id, derr))
			continue
		}
		switch dstatus {
		case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
			deleted = append(deleted, id)
		default:
			errs = append(errs, fmt.Errorf("deleting stale user %s: status %d", id, dstatus))
		}
	}
	if len(errs) > 0 {
		return deleted, errors.Join(errs...)
	}
	deadline := time.Now().Add(standardUserGoneTimeout)
	for {
		remaining, err := t.lookupUsersNamed(ctx, username)
		if err != nil {
			return deleted, err
		}
		if len(remaining) == 0 {
			return deleted, nil
		}
		if time.Now().After(deadline) {
			return deleted, fmt.Errorf("stale user %s still listed %s after delete", username, standardUserGoneTimeout)
		}
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// lookupUsersNamed returns the collection entries the username lookup
// reports; a malformed answer is an error.
func (t *OracleTarget) lookupUsersNamed(ctx context.Context, username string) ([]any, error) {
	body, status, err := t.adminJSON(ctx, http.MethodGet, "/v3/users?username="+username, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("looking up user %s: %w", username, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("looking up user %s: status %d", username, status)
	}
	data, ok := body["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("looking up user %s: 200 response has no data array", username)
	}
	return data, nil
}

// login posts local-provider credentials and returns the bearer, accepting
// both the v3 ({token}) and ext ({status.bearerToken}) response shapes:
// supported Rancher versions answer a login with a v3 token, Rancher v2.17
// with an ext Token.
func (t *OracleTarget) login(ctx context.Context, user, pass string) (string, error) {
	payload := map[string]any{"type": "localProvider", "username": user, "password": pass}
	body, status, err := t.plainJSON(ctx, http.MethodPost, "/v1-public/login", payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("login as %s: status %d", user, status)
	}
	if bearer := extBearerFrom(body); bearer != "" {
		return bearer, nil
	}
	if tok, _ := body["token"].(string); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("login as %s: no token in response", user)
}

func extBearerFrom(body map[string]any) string {
	st, _ := body["status"].(map[string]any)
	bearer, _ := st["bearerToken"].(string)
	return bearer
}

// adminJSON sends an admin-authenticated JSON request. wantStatus 0 skips
// the status check.
func (t *OracleTarget) adminJSON(ctx context.Context, method, path string, payload map[string]any, wantStatus int) (map[string]any, int, error) {
	return t.doJSON(ctx, method, path, payload, t.admin, wantStatus)
}

func (t *OracleTarget) plainJSON(ctx context.Context, method, path string, payload map[string]any) (map[string]any, int, error) {
	return t.doJSON(ctx, method, path, payload, "", 0)
}

func (t *OracleTarget) doJSON(ctx context.Context, method, path string, payload map[string]any, bearer string, wantStatus int) (map[string]any, int, error) {
	var marshaled []byte
	if payload != nil {
		var err error
		marshaled, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
	}
	build := func() (*http.Request, error) {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(marshaled)
		}
		req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, body)
		if err != nil {
			return nil, fmt.Errorf("building %s %s: %w", method, path, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return req, nil
	}
	resp, err := doResilient(t.client, build)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading %s %s response: %w", method, path, err)
	}
	if wantStatus != 0 && resp.StatusCode != wantStatus {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, redactBody(raw))
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return decoded, resp.StatusCode, nil
}

// redactBody scrubs credential fields from a response body before it is
// embedded in an error message; non-JSON bodies are summarized, never echoed -
// an unexpected-status response can still carry a live bearer.
func redactBody(raw []byte) string {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Sprintf("<non-JSON body, %d bytes>", len(raw))
	}
	Normalize(decoded, DefaultNormalizeRules())
	out, err := jsonMarshalNoEscape(decoded)
	if err != nil {
		return fmt.Sprintf("<unrenderable body, %d bytes>", len(raw))
	}
	return string(out)
}

package rbac

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	managementPrefix = "/k8s/clusters/local/apis/management.cattle.io/v3/"
	prtbPath         = "namespaces/p-x/projectroletemplatebindings"

	logProject  = "GET namespaces/c-x/projects/p-x"
	logRole     = "GET roletemplates/project-member"
	logBindings = "GET " + prtbPath
	logDryRun   = "POST " + prtbPath + " (dry-run)"
	logCreate   = "POST " + prtbPath
)

// refusedReadServer fakes a caller who may create and delete role bindings
// but may read no user: reads of users answer 403 unless the row says
// otherwise. It records every request as "METHOD path", with " (dry-run)"
// on a dry-run write, and the body of every create.
type refusedReadServer struct {
	// readableUsers are answered by name to a read; a name in usersGet gets
	// that status instead; any other name is refused.
	readableUsers []map[string]any
	usersGet      map[string]int
	// usersList is the status of the list of users; zero is a refusal.
	usersList int
	// bindings and bindingsStatus answer the list of the bindings of the
	// scope; a zero status is 200.
	bindings       []map[string]any
	bindingsStatus int
	// roleStatus answers the read of the role, roleListStatus the list of
	// role templates; a zero status is 200 (an empty list for the list).
	roleStatus     int
	roleListStatus int
	// createStatus answers a dry-run create, deleteStatus a dry-run delete;
	// a zero status is a success.
	createStatus int
	deleteStatus int

	mu      sync.Mutex
	log     []string
	creates []createdBinding
}

type createdBinding struct {
	dryRun bool
	body   map[string]any
}

func statusBody(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"message": message, "reason": reason, "code": code,
	})
}

func refuse(w http.ResponseWriter, verb, resource, name string) {
	subject := resource + ".management.cattle.io"
	if name != "" {
		subject += " " + fmt.Sprintf("%q", name)
	}
	statusBody(w, http.StatusForbidden, "Forbidden", fmt.Sprintf(
		"%s is forbidden: User \"u-owner\" cannot %s resource %q in API group \"management.cattle.io\" at the cluster scope",
		subject, verb, resource))
}

func answer(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func listBody(kind string, items []map[string]any) map[string]any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	return map[string]any{"apiVersion": "management.cattle.io/v3", "kind": kind + "List", "metadata": map[string]any{}, "items": out}
}

func (s *refusedReadServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, managementPrefix)
	entry := r.Method + " " + path
	raw, _ := io.ReadAll(r.Body)
	// A create names a dry-run in the query, a delete in its options.
	dry := r.URL.Query().Get("dryRun") != ""
	if r.Method == http.MethodDelete {
		var opts struct {
			DryRun []string `json:"dryRun"`
		}
		_ = json.Unmarshal(raw, &opts)
		dry = len(opts.DryRun) > 0
	}
	if dry {
		entry += " (dry-run)"
	}
	s.mu.Lock()
	s.log = append(s.log, entry)
	s.mu.Unlock()

	parts := strings.Split(path, "/")
	switch {
	case parts[0] == "roletemplates" || parts[0] == "globalroles":
		s.serveRole(w, parts)
	case parts[0] == "users":
		s.serveUsers(w, parts)
	case parts[0] == "namespaces" && len(parts) == 4 && parts[2] == "projects":
		answer(w, http.StatusOK, map[string]any{
			"apiVersion": "management.cattle.io/v3", "kind": "Project",
			"metadata": map[string]any{"name": parts[3], "namespace": parts[1]},
		})
	case parts[0] == "namespaces" && len(parts) >= 3:
		s.serveBindings(w, r, raw, parts[2], parts[3:], dry)
	case parts[0] == "globalrolebindings":
		s.serveBindings(w, r, raw, parts[0], parts[1:], dry)
	default:
		statusBody(w, http.StatusNotFound, "NotFound", "unexpected request "+entry)
	}
}

func (s *refusedReadServer) serveRole(w http.ResponseWriter, parts []string) {
	if len(parts) == 1 {
		switch s.roleListStatus {
		case 0:
			answer(w, http.StatusOK, listBody("RoleTemplate", nil))
		case http.StatusForbidden:
			refuse(w, "list", parts[0], "")
		default:
			statusBody(w, s.roleListStatus, "InternalError", "the server failed")
		}
		return
	}
	switch s.roleStatus {
	case 0:
		obj := map[string]any{"apiVersion": "management.cattle.io/v3", "metadata": map[string]any{"name": parts[1]}}
		if parts[0] == "roletemplates" {
			obj["context"] = "project"
		}
		answer(w, http.StatusOK, obj)
	case http.StatusForbidden:
		refuse(w, "get", parts[0], parts[1])
	case http.StatusNotFound:
		statusBody(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s.management.cattle.io %q not found", parts[0], parts[1]))
	default:
		statusBody(w, s.roleStatus, "InternalError", "the server failed")
	}
}

func (s *refusedReadServer) serveUsers(w http.ResponseWriter, parts []string) {
	if len(parts) == 1 {
		switch s.usersList {
		case http.StatusOK:
			answer(w, http.StatusOK, listBody("User", s.readableUsers))
		case 0, http.StatusForbidden:
			refuse(w, "list", "users", "")
		default:
			statusBody(w, s.usersList, "InternalError", "the server failed")
		}
		return
	}
	name := parts[1]
	if code, ok := s.usersGet[name]; ok {
		switch code {
		case http.StatusNotFound:
			statusBody(w, code, "NotFound", fmt.Sprintf("users.management.cattle.io %q not found", name))
		case http.StatusForbidden:
			refuse(w, "get", "users", name)
		default:
			statusBody(w, code, "InternalError", "the server failed")
		}
		return
	}
	for _, u := range s.readableUsers {
		if u["metadata"].(map[string]any)["name"] == name {
			answer(w, http.StatusOK, u)
			return
		}
	}
	refuse(w, "get", "users", name)
}

func (s *refusedReadServer) serveBindings(w http.ResponseWriter, r *http.Request, raw []byte, resource string, rest []string, dry bool) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		switch s.bindingsStatus {
		case 0:
			answer(w, http.StatusOK, listBody("Binding", s.bindings))
		case http.StatusForbidden:
			refuse(w, "list", resource, "")
		default:
			statusBody(w, s.bindingsStatus, "InternalError", "the server failed")
		}
	case len(rest) == 0 && r.Method == http.MethodPost:
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			statusBody(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		if dry && s.createStatus != 0 {
			statusBody(w, s.createStatus, "Conflict", "duplicate prtb not allowed")
			return
		}
		s.mu.Lock()
		s.creates = append(s.creates, createdBinding{dryRun: dry, body: body})
		s.mu.Unlock()
		meta, _ := body["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			body["metadata"] = meta
		}
		if prefix, _ := meta["generateName"].(string); prefix != "" {
			meta["name"] = prefix + "xxxxx"
		}
		meta["resourceVersion"] = "1"
		answer(w, http.StatusCreated, body)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		if dry && s.deleteStatus != 0 {
			statusBody(w, s.deleteStatus, "Conflict", "the object has been modified")
			return
		}
		answer(w, http.StatusOK, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Success"})
	default:
		statusBody(w, http.StatusNotFound, "NotFound", "unexpected request")
	}
}

// refusedReadResult is what one run of a row's command left behind.
type refusedReadResult struct {
	eff      safety.Effect
	planErr  error
	execErr  error
	log      []string
	creates  []createdBinding
	executed bool
}

// runRefusedRead plans the command of row against its server through the real
// provider client, so a 403 reaches the code through the real error mapping,
// and runs the execute function when the plan succeeded.
func runRefusedRead(t *testing.T, row refusedReadRow) refusedReadResult {
	t.Helper()
	srv := row.server
	if srv == nil {
		srv = &refusedReadServer{}
	}
	ts := httptest.NewTLSServer(srv)
	t.Cleanup(ts.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}))
	rc, err := kube.RESTConfig(config.Effective{URL: ts.URL, CA: caPEM, Token: "id:secret"}, engine.LocalCluster)
	require.NoError(t, err)
	client, err := provider.NewClusterClient(rc, engine.LocalCluster)
	require.NoError(t, err)
	factory := fakeFactory{c: client}

	strs := map[string]string{}
	if row.role != "" {
		strs["role"] = row.role
	}
	if !row.global {
		strs["project"] = "p-x"
		strs["cluster"] = "c-x"
	}
	if row.subjectType != "" {
		strs["type"] = row.subjectType
	}
	in := stubInput{strings: strs, bools: map[string]bool{"global": row.global}, args: []string{row.subject}}

	var cmd runtime.Command
	switch row.command {
	case "grant":
		cmd = NewGrant(factory)
	case "revoke":
		cmd = NewRevoke(factory)
	default:
		t.Fatalf("unknown command %q", row.command)
	}
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	var res refusedReadResult
	var exec runtime.ExecuteFunc
	res.eff, exec, res.planErr = mc.Plan(typedProjectContext(t, in), in)
	if res.planErr == nil {
		res.executed = true
		_, res.execErr = exec(t.Context())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	res.log = append([]string(nil), srv.log...)
	res.creates = append([]createdBinding(nil), srv.creates...)
	return res
}

// refusedReadRow is one case of a command whose reads the server refuses. The
// name is the subtest name.
type refusedReadRow struct {
	name        string
	command     string
	subject     string
	subjectType string
	role        string
	global      bool
	server      *refusedReadServer

	// wantLog is every request the CLI sent, in order.
	wantLog []string
	// wantWarnings are the warnings of the Effect, or of the error when the
	// command fails.
	wantWarnings []string
	// wantCode is the error code of a failure; empty means the command
	// succeeds.
	wantCode string
	// wantSentence is the subject the added sentence of a refused read of
	// users names; empty means the error has no such sentence.
	wantSentence string
	// wantMessage is text the error message holds.
	wantMessage []string
	// wantCreate is the binding every create sends: its kind, userName and
	// userPrincipalName (empty means the key is absent).
	wantCreate *wantedBinding
}

type wantedBinding struct {
	kind      string
	userName  string
	principal string
	group     string
}

func readableUser(id string) map[string]any {
	return userObj(id, "", id, "local://"+id)
}

func warnUnread(id string) string { return userReadRefusedWarning(ResolvedSubject{UserID: id}) }

func binding(name, role, userName, principal string) map[string]any {
	obj := prtbObj(name, userName, principal, "", role)
	obj["metadata"].(map[string]any)["namespace"] = "p-x"
	obj["metadata"].(map[string]any)["resourceVersion"] = "7"
	return obj
}

// revokeBindings are the bindings of the revoke rows: two of the user that
// hold a userName (one per role), one of the same user that has only a
// principal, and one of another user.
func revokeBindings() []map[string]any {
	return []map[string]any{
		binding("pm-1", "project-member", "u-lpnxw", ""),
		binding("pm-2", "project-member", "u-other", ""),
		binding("pm-4", "project-owner", "", "local://u-lpnxw"),
		binding("ro-1", "read-only", "u-lpnxw", ""),
	}
}

func deletePath(name string) string { return "DELETE " + prtbPath + "/" + name }

func logOf(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func lines(l ...string) []string { return l }

// refusedReadRows is the table of cases for grant and revoke when the server
// refuses reads. Every read of the two commands has a row here or an existing
// test; see TestEveryPreReadOfGrantAndRevokeHasARule.
func refusedReadRows() []refusedReadRow {
	hashID := ProvisionedUserID("okta_user://alice")
	grantReads := lines(logProject, logRole)
	withUserRead := func(id string) []string { return append(append([]string(nil), grantReads...), "GET users/"+id) }
	created := lines(logBindings, logDryRun, logCreate)
	return []refusedReadRow{
		{
			name: "grant bare id, user read refused", command: "grant", subject: "u-lpnxw", role: "project-member",
			wantLog:      logOf(withUserRead("u-lpnxw"), created),
			wantWarnings: []string{warnUnread("u-lpnxw")},
			wantCreate:   &wantedBinding{kind: "ProjectRoleTemplateBinding", userName: "u-lpnxw"},
		},
		{
			name: "grant local principal, user read refused", command: "grant", subject: "local://u-lpnxw", role: "project-member",
			wantLog:      logOf(withUserRead("u-lpnxw"), created),
			wantWarnings: []string{warnUnread("u-lpnxw")},
			wantCreate:   &wantedBinding{kind: "ProjectRoleTemplateBinding", userName: "u-lpnxw", principal: "local://u-lpnxw"},
		},
		{
			name: "grant bare name, user read refused", command: "grant", subject: "jane", role: "project-member",
			wantLog: withUserRead("jane"), wantCode: clierr.CodeRBAC, wantSentence: "jane",
			wantMessage: []string{`users.management.cattle.io "jane" is forbidden`},
		},
		{
			name: "grant username user-b, user read refused", command: "grant", subject: "user-b", role: "project-member",
			wantLog: withUserRead("user-b"), wantCode: clierr.CodeRBAC, wantSentence: "user-b",
		},
		{
			name: "grant username user-alice, user read refused", command: "grant", subject: "user-alice", role: "project-member",
			wantLog: withUserRead("user-alice"), wantCode: clierr.CodeRBAC, wantSentence: "user-alice",
		},
		{
			name: "grant username u-admin, user read refused", command: "grant", subject: "u-admin", role: "project-member",
			wantLog: withUserRead("u-admin"), wantCode: clierr.CodeRBAC, wantSentence: "u-admin",
		},
		{
			name: "grant username m-smith, user read refused", command: "grant", subject: "m-smith", role: "project-member",
			wantLog: withUserRead("m-smith"), wantCode: clierr.CodeRBAC, wantSentence: "m-smith",
		},
		{
			name: "grant bare id, user not found and users list refused", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:  &refusedReadServer{usersGet: map[string]int{"u-lpnxw": http.StatusNotFound}},
			wantLog: logOf(withUserRead("u-lpnxw"), lines("GET users")), wantCode: clierr.CodeRBAC, wantSentence: "u-lpnxw",
		},
		{
			name: "grant derived id with other letters, user read refused", command: "grant", subject: "u-wx3sx2pm3y", role: "project-member",
			wantLog: withUserRead("u-wx3sx2pm3y"), wantCode: clierr.CodeRBAC, wantSentence: "u-wx3sx2pm3y",
			wantMessage: []string{"pass the user id as local://<id>"},
		},
		{
			name: "grant external principal, user read refused", command: "grant", subject: "okta_user://alice", role: "project-member",
			wantLog: withUserRead(hashID), wantCode: clierr.CodeRBAC, wantSentence: "okta_user://alice",
		},
		{
			name: "grant external principal, user not found and users list refused", command: "grant", subject: "okta_user://alice", role: "project-member",
			server:  &refusedReadServer{usersGet: map[string]int{hashID: http.StatusNotFound}},
			wantLog: logOf(withUserRead(hashID), lines("GET users")), wantCode: clierr.CodeRBAC, wantSentence: "okta_user://alice",
		},
		{
			name: "grant readable user, bindings list refused", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:       &refusedReadServer{readableUsers: []map[string]any{readableUser("u-lpnxw")}, bindingsStatus: http.StatusForbidden},
			wantLog:      logOf(withUserRead("u-lpnxw"), created),
			wantWarnings: []string{bindingListRefusedWarning},
			wantCreate:   &wantedBinding{kind: "ProjectRoleTemplateBinding", userName: "u-lpnxw", principal: "local://u-lpnxw"},
		},
		{
			name: "grant readable user, bindings list fails", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:  &refusedReadServer{readableUsers: []map[string]any{readableUser("u-lpnxw")}, bindingsStatus: http.StatusInternalServerError},
			wantLog: logOf(withUserRead("u-lpnxw"), lines(logBindings)), wantCode: clierr.CodeGeneric,
		},
		{
			name: "grant bare id, user read and bindings list refused", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:       &refusedReadServer{bindingsStatus: http.StatusForbidden},
			wantLog:      logOf(withUserRead("u-lpnxw"), created),
			wantWarnings: []string{warnUnread("u-lpnxw"), bindingListRefusedWarning},
			wantCreate:   &wantedBinding{kind: "ProjectRoleTemplateBinding", userName: "u-lpnxw"},
		},
		{
			name: "grant bare id, user read fails", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:  &refusedReadServer{usersGet: map[string]int{"u-lpnxw": http.StatusInternalServerError}},
			wantLog: withUserRead("u-lpnxw"), wantCode: clierr.CodeGeneric,
		},
		{
			name: "grant readable user, role read refused", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:     &refusedReadServer{readableUsers: []map[string]any{readableUser("u-lpnxw")}, roleStatus: http.StatusForbidden},
			wantLog:    logOf(withUserRead("u-lpnxw"), created),
			wantCreate: &wantedBinding{kind: "ProjectRoleTemplateBinding", userName: "u-lpnxw", principal: "local://u-lpnxw"},
		},
		{
			name: "grant missing role, role templates list refused", command: "grant", subject: "u-lpnxw", role: "no-such-role",
			server:  &refusedReadServer{roleStatus: http.StatusNotFound, roleListStatus: http.StatusForbidden},
			wantLog: lines(logProject, "GET roletemplates/no-such-role", "GET roletemplates"), wantCode: clierr.CodeNotFound,
			wantMessage: []string{"role not found: no-such-role"},
		},
		{
			name: "grant readable user, everything readable", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:     &refusedReadServer{readableUsers: []map[string]any{readableUser("u-lpnxw")}},
			wantLog:    logOf(withUserRead("u-lpnxw"), created),
			wantCreate: &wantedBinding{kind: "ProjectRoleTemplateBinding", userName: "u-lpnxw", principal: "local://u-lpnxw"},
		},
		{
			name: "grant at global scope, user read refused", command: "grant", subject: "u-lpnxw", role: "admin", global: true,
			wantLog:      lines("GET globalroles/admin", "GET users/u-lpnxw", "GET globalrolebindings", "POST globalrolebindings (dry-run)", "POST globalrolebindings"),
			wantWarnings: []string{warnUnread("u-lpnxw")},
			wantCreate:   &wantedBinding{kind: "GlobalRoleBinding", userName: "u-lpnxw"},
		},
		{
			name: "grant bare id, user read refused and the dry-run create fails", command: "grant", subject: "u-lpnxw", role: "project-member",
			server:  &refusedReadServer{bindingsStatus: http.StatusForbidden, createStatus: http.StatusConflict},
			wantLog: logOf(withUserRead("u-lpnxw"), lines(logBindings, logDryRun)), wantCode: clierr.CodeConflict,
			wantWarnings: []string{warnUnread("u-lpnxw"), bindingListRefusedWarning},
		},
		{
			name: "grant group principal reads no user", command: "grant", subject: "okta_group://gid", subjectType: "group", role: "project-member",
			wantLog:    logOf(grantReads, created),
			wantCreate: &wantedBinding{kind: "ProjectRoleTemplateBinding", group: "okta_group://gid"},
		},
		{
			name: "grant local principal that is not an object name, user read refused", command: "grant", subject: "local://Jane", role: "project-member",
			wantLog: withUserRead("Jane"), wantCode: clierr.CodeRBAC, wantSentence: "local://Jane",
		},
		{
			name: "revoke bare id, user read refused", command: "revoke", subject: "u-lpnxw",
			server: &refusedReadServer{bindings: revokeBindings()},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings,
				deletePath("pm-1")+" (dry-run)", deletePath("ro-1")+" (dry-run)", deletePath("pm-1"), deletePath("ro-1")),
			wantWarnings: []string{warnUnread("u-lpnxw"), "2 bindings are deleted sequentially; on failure some may already be deleted"},
		},
		{
			name: "revoke bare id of one role, user read refused", command: "revoke", subject: "u-lpnxw", role: "project-member",
			server: &refusedReadServer{bindings: revokeBindings()},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings,
				deletePath("pm-1")+" (dry-run)", deletePath("pm-1")),
			wantWarnings: []string{warnUnread("u-lpnxw")},
		},
		{
			name: "revoke bare id, no binding matches", command: "revoke", subject: "u-lpnxw",
			server:  &refusedReadServer{bindings: []map[string]any{binding("pm-2", "project-member", "u-other", ""), binding("pm-4", "project-owner", "", "local://u-lpnxw")}},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings), wantCode: clierr.CodeNotFound,
			wantWarnings: []string{warnUnread("u-lpnxw")}, wantMessage: []string{`no projectroletemplatebindings for subject "u-lpnxw"`},
		},
		{
			name: "revoke bare id, user read refused and the dry-run delete fails", command: "revoke", subject: "u-lpnxw",
			server: &refusedReadServer{bindings: revokeBindings(), deleteStatus: http.StatusConflict},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings,
				deletePath("pm-1")+" (dry-run)"),
			wantCode: clierr.CodeConflict, wantWarnings: []string{warnUnread("u-lpnxw")},
		},
		{
			name: "revoke external principal with no user, no binding has a userName", command: "revoke", subject: "okta_user://alice",
			server: &refusedReadServer{
				usersGet:  map[string]int{hashID: http.StatusNotFound},
				usersList: http.StatusOK,
				bindings: []map[string]any{
					{"apiVersion": "management.cattle.io/v3", "kind": "ProjectRoleTemplateBinding",
						"metadata":           map[string]any{"name": "pm-g", "namespace": "p-x", "resourceVersion": "7"},
						"roleTemplateName":   "project-member",
						"groupPrincipalName": "okta_group://gid"},
					binding("pm-b", "project-member", "", "okta_user://bob"),
				},
			},
			wantLog: lines(logProject, "GET users/"+hashID, "GET users", logBindings), wantCode: clierr.CodeNotFound,
			wantMessage: []string{`no projectroletemplatebindings for subject "okta_user://alice"`},
		},
		{
			name: "revoke bare name, user read refused", command: "revoke", subject: "jane",
			server:  &refusedReadServer{bindings: revokeBindings()},
			wantLog: lines(logProject, "GET users/jane"), wantCode: clierr.CodeRBAC, wantSentence: "jane",
		},
		{
			name: "revoke bare id, user read and bindings list refused", command: "revoke", subject: "u-lpnxw",
			server:  &refusedReadServer{bindingsStatus: http.StatusForbidden},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings), wantCode: clierr.CodeRBAC,
			wantMessage: []string{"projectroletemplatebindings.management.cattle.io is forbidden"},
		},
		{
			name: "revoke local principal, user read refused", command: "revoke", subject: "local://u-lpnxw",
			server: &refusedReadServer{bindings: revokeBindings()},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings,
				deletePath("pm-1")+" (dry-run)", deletePath("pm-4")+" (dry-run)", deletePath("ro-1")+" (dry-run)",
				deletePath("pm-1"), deletePath("pm-4"), deletePath("ro-1")),
			wantWarnings: []string{warnUnread("u-lpnxw"), "3 bindings are deleted sequentially; on failure some may already be deleted"},
		},
		{
			name: "revoke readable user", command: "revoke", subject: "u-lpnxw",
			server: &refusedReadServer{readableUsers: []map[string]any{readableUser("u-lpnxw")}, bindings: revokeBindings()},
			wantLog: lines(logProject, "GET users/u-lpnxw", logBindings,
				deletePath("pm-1")+" (dry-run)", deletePath("ro-1")+" (dry-run)", deletePath("pm-1"), deletePath("ro-1")),
			wantWarnings: []string{"2 bindings are deleted sequentially; on failure some may already be deleted"},
		},
	}
}

func assertRefusedReadRow(t *testing.T, row refusedReadRow) {
	t.Helper()
	res := runRefusedRead(t, row)
	assert.Equal(t, row.wantLog, res.log, "the requests the CLI sent")

	if row.wantCode != "" {
		ce, ok := errors.AsType[*clierr.Error](res.planErr)
		require.True(t, ok, "want a %s error, got %v", row.wantCode, res.planErr)
		assert.Equal(t, row.wantCode, ce.Code)
		for _, m := range row.wantMessage {
			require.ErrorContains(t, res.planErr, m)
		}
		sentence := "the CLI could not read users to find "
		if row.wantSentence == "" {
			assert.NotContains(t, ce.Message, sentence)
		} else {
			assert.Empty(t, ce.Hint, "the sentence is in the message; the error has no hint field")
			require.ErrorContains(t, res.planErr, fmt.Sprintf("%s%q; pass the user id as local://<id> (the id is the name of the User object, for example u-lpnxw); an administrator can list ids with: ", sentence, row.wantSentence))
			assert.True(t, strings.HasSuffix(ce.Message, clierr.Rancher+" get users"), "the message ends with the command: %q", ce.Message)
		}
		assert.Equal(t, row.wantWarnings, nilIfEmpty(ce.Warnings()), "the warnings of the error")
		assert.Empty(t, res.creates, "no binding is created after a refusal")
		return
	}

	require.NoError(t, res.planErr)
	require.NoError(t, res.execErr)
	assert.Equal(t, row.wantWarnings, nilIfEmpty(res.eff.Warnings), "the warnings of the Effect")
	if row.wantCreate == nil {
		assert.Empty(t, res.creates)
		return
	}
	require.Len(t, res.creates, 2, "the dry-run create and the real create")
	assert.True(t, res.creates[0].dryRun)
	assert.False(t, res.creates[1].dryRun)
	for _, c := range res.creates {
		assert.Equal(t, row.wantCreate.kind, c.body["kind"])
		un, hasUN := c.body["userName"]
		up, hasUP := c.body["userPrincipalName"]
		gp, hasGP := c.body["groupPrincipalName"]
		assert.Equal(t, row.wantCreate.userName != "", hasUN, "userName present")
		if hasUN {
			assert.Equal(t, row.wantCreate.userName, un)
		}
		assert.Equal(t, row.wantCreate.principal != "", hasUP, "userPrincipalName present: an empty principal is never sent")
		if hasUP {
			assert.Equal(t, row.wantCreate.principal, up)
		}
		assert.Equal(t, row.wantCreate.group != "", hasGP, "groupPrincipalName present")
		if hasGP {
			assert.Equal(t, row.wantCreate.group, gp)
		}
	}
	if row.wantCreate.userName != "" {
		assert.Contains(t, diffPaths(res.eff.Diff), "/userName")
	}
	assert.Equal(t, row.wantCreate.principal != "", slices.Contains(diffPaths(res.eff.Diff), "/userPrincipalName"))
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// TestUserReadRefusedWarningText asserts the preview warning of a user the
// server refused to read.
func TestUserReadRefusedWarningText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `cannot read user "u-lpnxw" (Rancher refused the read); using "u-lpnxw" as the user id, exactly as typed; `+
		`the CLI could not check that a user with this id exists, and a binding for a user that does not exist has no effect; `+
		`the CLI does not find bindings made with another principal of the same user`, userReadRefusedWarning(ResolvedSubject{UserID: "u-lpnxw"}))
	assert.Equal(t, `cannot list the existing bindings (Rancher refused the read); `+
		`the CLI could not check whether the subject already has this role; the server-side dry-run checks only that Rancher accepts the create`, bindingListRefusedWarning)
}

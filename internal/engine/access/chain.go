package access

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/output"
)

var (
	roleGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	clusterRoleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
)

// maxChainDepth and maxChainNodes bound the Rancher chain: RoleTemplate
// inheritance is a graph a server may make arbitrarily deep or cyclic, and
// one binding may name many parents. Package variables so tests can lower
// them.
var (
	maxChainDepth = 32
	maxChainNodes = 256
)

// chainState says how much of the chain the tree shows.
type chainState string

const (
	chainComplete chainState = "complete"
	chainPartial  chainState = "partial"
	chainUnknown  chainState = "unknown"
)

// chainNode is one object in the tree; Children is never nil in JSON.
type chainNode struct {
	Kind      string      `json:"kind"`
	Name      string      `json:"name"`
	Namespace string      `json:"namespace"`
	Note      string      `json:"note"`
	Children  []chainNode `json:"children"`
}

// chainIssue is one thing the traversal could not establish: a request
// that failed (Code from the mapped error, Retryable copied from it) or a
// traversal diagnostic (never retryable).
type chainIssue struct {
	Stage     string `json:"stage"`
	Object    string `json:"object,omitempty"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

const (
	stageReasonParse    = "reasonParse"
	stageBindingRead    = "bindingRead"
	stageRoleRead       = "roleRead"
	stageLabelTraversal = "labelTraversal"
	stageManagementRead = "managementRead"

	codeUnparseable         = "unparseable"
	codeNoSource            = "noSource"
	codeUnverifiedCandidate = "unverifiedCandidate"
	codeCycle               = "cycle"
	codeLimitExceeded       = "limitExceeded"
)

// Node kinds the tree uses; management kinds are Rancher's own names.
const (
	nodeBindings       = "Bindings"
	nodeHeldBindings   = "HeldBindings"
	nodeGlobalRoleBind = "GlobalRoleBinding"
	nodeGlobalRole     = "GlobalRole"
	nodeRoleTemplate   = "RoleTemplate"
)

// Notes the tree states in the CLI's own words. noSourceNote says only
// what was not found: Rancher itself creates unlabelled bindings and
// roles, so who created an object is never claimed.
const (
	noSourceNote        = "no Rancher owner label or annotation"
	unverifiedNote      = "no Rancher parent could be verified"
	acceptedBindingNote = "one binding the server accepted"
	rulesUnreadableNote = "rules not readable by the caller"
	labelsUnreadable    = "binding labels not readable by the caller"
	heldBindingsTitle   = "Bindings held by the subject"
	bindingsTitle       = "bindings on the cluster naming the subject or its groups"
)

// The GlobalRole back-references Rancher writes: two annotations on the
// ClusterRoleBinding it creates for an admin (globaladmin-<user>) on
// every cluster, a label on the ClusterRoleBinding it creates for any
// GlobalRoleBinding on the management cluster and on the
// ClusterRoleTemplateBindings a GlobalRole's inheritedClusterRoles create,
// and a label on the ClusterRole it creates for a GlobalRole.
const (
	crbGlobalRoleAnnotation        = "authz.cluster.cattle.io/globalrole"
	crbGlobalRoleBindingAnnotation = "authz.cluster.cattle.io/globalrolebinding"
	grbOwnerLabel                  = "authz.management.cattle.io/grb-owner"
	grOwnerLabel                   = "authz.management.cattle.io/gr-owner"
	// clusterRoleOwnerKey names a ClusterRole's RoleTemplate: an
	// annotation on the classic path, a label on the aggregation path.
	clusterRoleOwnerKey = "authz.cluster.cattle.io/clusterrole-owner"
)

// chainSubject is the subject the chain is about, in every spelling a
// binding may use: the Kubernetes name (a Rancher user id, a group, a
// service account username), the Rancher user id and principal ids when
// the subject is a Rancher user, and its groups.
type chainSubject struct {
	Kind         SubjectKind
	Name         string
	ID           string
	PrincipalIDs []string
	Groups       []string
}

type nodeKey struct{ kind, ns, name string }

// chainWalker walks one explanation. cluster is the downstream as the
// caller (the client whose reads the caller is entitled to); mgmt is the
// management cluster.
type chainWalker struct {
	ctx       context.Context
	cluster   engine.ClusterClient
	mgmt      engine.ClusterClient
	clusterID string
	subject   chainSubject
	question  Question

	issues   []chainIssue
	requests int
	nodes    int
	seen     map[nodeKey]bool
	active   map[nodeKey]bool
	// crtbs and prtbs are the aggregation-grammar candidates, listed at
	// most once per explanation.
	crtbs, prtbs *unstructured.UnstructuredList
	crtbsErr     *chainIssue
	prtbsErr     *chainIssue
	crtbsListed  bool
	prtbsListed  bool
	// heldShown is set once the held-bindings fallback node was added.
	heldShown bool
}

func newChainWalker(ctx context.Context, cluster, mgmt engine.ClusterClient, clusterID string, subject chainSubject, q Question) *chainWalker {
	return &chainWalker{ctx: ctx, cluster: cluster, mgmt: mgmt, clusterID: clusterID, subject: subject, question: q,
		seen: map[nodeKey]bool{}, active: map[nodeKey]bool{}}
}

// state is complete when nothing was left unestablished, partial otherwise.
func (w *chainWalker) state() chainState {
	if len(w.issues) == 0 {
		return chainComplete
	}
	return chainPartial
}

// issue records one chain issue; traversal diagnostics are never
// retryable.
func (w *chainWalker) issue(stage, object, code, message string) {
	w.issues = append(w.issues, chainIssue{Stage: stage, Object: object, Code: code, Message: message})
}

// requestIssue records a failed request with the mapped error's code,
// retryability and message. A budget deadline is reported as such.
func (w *chainWalker) requestIssue(stage, object string, err error) {
	if errors.Is(context.Cause(w.ctx), errFanOutBudget) {
		w.issues = append(w.issues, chainIssue{Stage: stage, Object: object, Code: clierr.CodeGeneric, Retryable: true,
			Message: fmt.Sprintf("fan-out budget of %s exhausted", fanOutBudget)})
		return
	}
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		w.issues = append(w.issues, chainIssue{Stage: stage, Object: object, Code: ce.Code, Retryable: ce.Retryable, Message: ce.Message})
		return
	}
	w.issues = append(w.issues, chainIssue{Stage: stage, Object: object, Code: clierr.CodeGeneric, Message: err.Error()})
}

func (w *chainWalker) get(c engine.ClusterClient, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	w.requests++
	return c.Get(w.ctx, gvr, ns, name)
}

func (w *chainWalker) list(c engine.ClusterClient, gvr schema.GroupVersionResource, ns string) (*unstructured.UnstructuredList, error) {
	w.requests++
	return c.List(w.ctx, gvr, ns, "")
}

// enter claims a node for the walk. It returns a leaf instead when the
// node is on the active path (a cycle), was already shown (a diamond), or
// would exceed the depth or node bound; the caller adds that leaf and
// stops. leave must be called for every true return.
func (w *chainWalker) enter(kind, ns, name string, depth int) (ok bool, leaf chainNode) {
	key := nodeKey{kind, ns, name}
	object := objectName(kind, ns, name)
	switch {
	case w.active[key]:
		w.issue(stageLabelTraversal, object, codeCycle, "the chain returns to "+object)
		return false, chainNode{Kind: kind, Name: name, Namespace: ns, Note: "cycle to " + object, Children: []chainNode{}}
	case w.seen[key]:
		return false, chainNode{Kind: kind, Name: name, Namespace: ns, Note: "already shown", Children: []chainNode{}}
	case depth > maxChainDepth:
		w.issue(stageLabelTraversal, object, codeLimitExceeded, fmt.Sprintf("chain deeper than %d; not followed", maxChainDepth))
		return false, chainNode{Kind: kind, Name: name, Namespace: ns, Note: "not followed: chain too deep", Children: []chainNode{}}
	case w.nodes >= maxChainNodes:
		w.issue(stageLabelTraversal, object, codeLimitExceeded, fmt.Sprintf("more than %d objects; not followed", maxChainNodes))
		return false, chainNode{Kind: kind, Name: name, Namespace: ns, Note: "not followed: too many objects", Children: []chainNode{}}
	}
	w.nodes++
	w.seen[key], w.active[key] = true, true
	return true, chainNode{}
}

func (w *chainWalker) leave(kind, ns, name string) { delete(w.active, nodeKey{kind, ns, name}) }

func objectName(kind, ns, name string) string {
	if ns != "" {
		return kind + " " + ns + "/" + name
	}
	return kind + " " + name
}

// explainAllowed builds the tree for an allowed answer from its reason.
// An unparseable reason leaves the tree empty and the state unknown.
func (w *chainWalker) explainAllowed(reason string) (*chainNode, chainState) {
	rb, ok := parseReason(reason)
	if !ok {
		w.issue(stageReasonParse, "", codeUnparseable, "reason did not match the RBAC authorizer's template: "+reason)
		return nil, chainUnknown
	}
	node := w.bindingNode(rb.Kind, rb.Namespace, rb.Name, rb.RoleKind, rb.RoleName, acceptedBindingNote, true, 1)
	return &node, w.state()
}

// explainDenied builds the tree for a denied answer: the bindings on the
// cluster naming the subject or its groups, each with its Rancher chain.
// namespace is the question's namespace ("" for cluster scope).
func (w *chainWalker) explainDenied(namespace string) (chainNode, chainState) {
	root := chainNode{Kind: nodeBindings, Note: bindingsTitle, Children: []chainNode{}}
	type found struct{ kind, ns, name, roleKind, roleName string }
	var hits []found
	collect := func(list *unstructured.UnstructuredList, kind string) {
		for i := range list.Items {
			b := &list.Items[i]
			if !w.namesSubject(b) {
				continue
			}
			roleKind, _, _ := unstructured.NestedString(b.Object, "roleRef", "kind")
			roleName, _, _ := unstructured.NestedString(b.Object, "roleRef", "name")
			hits = append(hits, found{kind, b.GetNamespace(), b.GetName(), roleKind, roleName})
		}
	}
	readable := true
	if namespace != "" {
		list, err := w.list(w.cluster, roleBindingGVR, namespace)
		if err != nil {
			readable = false
			w.requestIssue(stageBindingRead, objectName("RoleBindings", namespace, "*"), err)
		} else {
			collect(list, bindingKindRoleBinding)
		}
	}
	list, err := w.list(w.cluster, clusterRoleBindingGVR, "")
	if err != nil {
		readable = false
		w.requestIssue(stageBindingRead, "ClusterRoleBindings", err)
	} else {
		collect(list, bindingKindClusterRoleBinding)
	}
	slices.SortFunc(hits, func(a, b found) int {
		return strings.Compare(a.kind+"\x00"+a.ns+"\x00"+a.name, b.kind+"\x00"+b.ns+"\x00"+b.name)
	})
	for _, h := range hits {
		root.Children = append(root.Children, w.bindingNode(h.kind, h.ns, h.name, h.roleKind, h.roleName, "", false, 1))
	}
	if !readable {
		w.heldBindings(&root, 1)
	}
	return root, w.state()
}

// namesSubject reports whether a binding's subjects name the subject: a
// User or ServiceAccount by its Kubernetes username, or a Group among the
// subject's groups.
func (w *chainWalker) namesSubject(b *unstructured.Unstructured) bool {
	subjects, _, _ := unstructured.NestedSlice(b.Object, "subjects")
	for _, s := range subjects {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		ns, _ := m["namespace"].(string)
		switch kind {
		case "User":
			if name == w.subject.Name {
				return true
			}
		case "ServiceAccount":
			if w.subject.Kind == KindServiceAccount && serviceAccountPrefix+ns+":"+name == w.subject.Name {
				return true
			}
		case "Group":
			if (w.subject.Kind == KindGroup && name == w.subject.Name) || slices.Contains(w.subject.Groups, name) {
				return true
			}
		}
	}
	return false
}

// bindingNode GETs a binding and builds its node: the role (with the rules
// covering the request) and the Rancher parents behind the binding. When
// the GET is refused the node still stands on the authorizer's facts
// (kind, name, namespace, role). accepted marks the one binding the
// authorizer named: for it, a refused read or a missing Rancher parent is
// a chain issue and the held-bindings fallback is added; in the denied
// listing an unlabelled binding is an ordinary fact and only gets the
// note.
func (w *chainWalker) bindingNode(kind, ns, name, roleKind, roleName, note string, accepted bool, depth int) chainNode {
	ok, leaf := w.enter(kind, ns, name, depth)
	if !ok {
		return leaf
	}
	defer w.leave(kind, ns, name)
	node := chainNode{Kind: kind, Name: name, Namespace: ns, Note: note, Children: []chainNode{}}
	gvr := clusterRoleBindingGVR
	if kind == bindingKindRoleBinding {
		gvr = roleBindingGVR
	}
	b, err := w.get(w.cluster, gvr, ns, name)
	if err != nil {
		w.requestIssue(stageBindingRead, objectName(kind, ns, name), err)
		node.Children = append(node.Children, w.roleNode(roleKind, roleName, ns, nil, depth+1))
		if accepted {
			w.issue(stageLabelTraversal, objectName(kind, ns, name), clierr.CodeRBAC, labelsUnreadable)
			w.heldBindings(&node, depth+1)
		}
		return node
	}
	if roleName == "" {
		roleKind, _, _ = unstructured.NestedString(b.Object, "roleRef", "kind")
		roleName, _, _ = unstructured.NestedString(b.Object, "roleRef", "name")
	}
	parents := w.rancherParents(b, kind, roleName, depth+1)
	node.Children = append(node.Children, w.roleNode(roleKind, roleName, ns, parents.roleOwner, depth+1))
	node.Children = append(node.Children, parents.nodes...)
	if len(parents.nodes) == 0 {
		// A binding whose labels name a parent nothing matched already has
		// its unverifiedCandidate issue; only a binding with no
		// back-reference at all is a noSource.
		note := noSourceNote
		if parents.referenced {
			note = unverifiedNote
		}
		node.Note = strings.TrimPrefix(node.Note+"; "+note, "; ")
		if accepted {
			if !parents.referenced {
				w.issue(stageLabelTraversal, objectName(kind, ns, name), codeNoSource, noSourceNote)
			}
			w.heldBindings(&node, depth+1)
		}
	}
	return node
}

// parentSet is what rancherParents found behind one binding.
type parentSet struct {
	nodes []chainNode
	// referenced is set when the binding carries any back-reference,
	// verified or not.
	referenced bool
	// roleOwner is set when a Rancher parent was verified: the role node
	// then makes no source claim of its own.
	roleOwner *string
}

// rancherParents follows every back-reference on a binding: the classic
// grammar, the aggregation grammar, and the GlobalRole annotations or
// label. Children are sorted by kind then name.
func (w *chainWalker) rancherParents(b *unstructured.Unstructured, kind, roleName string, depth int) parentSet {
	var out parentSet
	labels, annotations := b.GetLabels(), b.GetAnnotations()
	verified := false
	refs := classicOwners(labels, w.clusterID)
	for _, ref := range refs {
		out.nodes = append(out.nodes, w.ownerNode(ref, depth))
		out.referenced, verified = true, true
	}
	if len(refs) == 0 && classicReferenced(labels) {
		// The label is there but names no <namespace>_<name>.
		out.referenced = true
		w.issue(stageLabelTraversal, objectName(kind, b.GetNamespace(), b.GetName()), codeUnverifiedCandidate, "the owner label does not name a <namespace>_<name>")
	}
	for _, key := range aggregationKeys(labels) {
		out.referenced = true
		nodes, ok := w.aggregationParents(b, key, roleName, depth)
		out.nodes = append(out.nodes, nodes...)
		verified = verified || ok
	}
	if grb := annotations[crbGlobalRoleBindingAnnotation]; grb != "" {
		out.nodes = append(out.nodes, w.globalRoleBindingNode(grb, annotations[crbGlobalRoleAnnotation], depth))
		out.referenced, verified = true, true
	} else if grb := labels[grbOwnerLabel]; grb != "" {
		out.nodes = append(out.nodes, w.globalRoleBindingNode(grb, "", depth))
		out.referenced, verified = true, true
	}
	if verified {
		owner := roleName
		out.roleOwner = &owner
	}
	slices.SortFunc(out.nodes, func(a, b chainNode) int {
		return strings.Compare(a.Kind+"\x00"+a.Namespace+"\x00"+a.Name, b.Kind+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	return out
}

// ownerNode GETs a ClusterRoleTemplateBinding or ProjectRoleTemplateBinding
// on the management cluster and follows its RoleTemplate (and, for a CRTB
// a GlobalRoleBinding created, that GlobalRoleBinding).
func (w *chainWalker) ownerNode(ref ownerRef, depth int) chainNode {
	kind := string(ref.Kind)
	ok, leaf := w.enter(kind, ref.Namespace, ref.Name, depth)
	if !ok {
		return leaf
	}
	defer w.leave(kind, ref.Namespace, ref.Name)
	node := chainNode{Kind: kind, Name: ref.Name, Namespace: ref.Namespace, Children: []chainNode{}}
	row := "clusterroletemplatebinding"
	if ref.Kind == ownerPRTB {
		row = "projectroletemplatebinding"
	}
	k, _ := kinds.Lookup(row)
	obj, err := w.get(w.mgmt, k.GVR, ref.Namespace, ref.Name)
	if err != nil {
		w.requestIssue(stageManagementRead, objectName(kind, ref.Namespace, ref.Name), err)
		return node
	}
	return w.fillOwnerNode(node, obj, depth)
}

// fillOwnerNode completes a CRTB or PRTB node from the object.
func (w *chainWalker) fillOwnerNode(node chainNode, obj *unstructured.Unstructured, depth int) chainNode {
	rt, _, _ := unstructured.NestedString(obj.Object, "roleTemplateName")
	node.Note = strings.TrimSpace(bindingSubjectText(obj) + ", RoleTemplate " + rt)
	if project, _, _ := unstructured.NestedString(obj.Object, "projectName"); project != "" {
		node.Note = "project " + project + ", " + node.Note
	}
	if rt != "" {
		node.Children = append(node.Children, w.roleTemplateNode(rt, depth+1))
	}
	if grb := obj.GetLabels()[grbOwnerLabel]; grb != "" {
		node.Children = append(node.Children, w.globalRoleBindingNode(grb, "", depth+1))
	}
	return node
}

// bindingSubjectText names a management binding's subject in Rancher's
// own fields.
func bindingSubjectText(obj *unstructured.Unstructured) string {
	if g, _, _ := unstructured.NestedString(obj.Object, "groupPrincipalName"); g != "" {
		return "group " + g
	}
	if u, _, _ := unstructured.NestedString(obj.Object, "userName"); u != "" {
		return "user " + u
	}
	if p, _, _ := unstructured.NestedString(obj.Object, "userPrincipalName"); p != "" {
		return "user " + p
	}
	return ""
}

// aggregationParents resolves one aggregation-grammar key: the candidates
// are the cluster's CRTBs or the cluster's projects' PRTBs, listed once;
// a candidate matches when its name yields the key and it names the
// subject; several survivors narrow by the bound role's project, else the
// candidates are reported and none is followed.
func (w *chainWalker) aggregationParents(b *unstructured.Unstructured, key aggregationKey, roleName string, depth int) ([]chainNode, bool) {
	list, issue := w.aggregationCandidates(key.Kind)
	object := objectName(b.GetKind(), b.GetNamespace(), b.GetName())
	if issue != nil {
		w.issues = append(w.issues, chainIssue{Stage: stageManagementRead, Object: object, Code: issue.Code, Retryable: issue.Retryable, Message: issue.Message})
		return nil, false
	}
	names := make([]string, 0, len(list.Items))
	byName := map[string][]*unstructured.Unstructured{}
	for i := range list.Items {
		item := &list.Items[i]
		names = append(names, item.GetName())
		byName[item.GetName()] = append(byName[item.GetName()], item)
	}
	var matched []*unstructured.Unstructured
	for _, name := range slices.Compact(slices.Sorted(slices.Values(matchAggregationKey(key.Kind, key.Key, names)))) {
		for _, item := range byName[name] {
			if w.managementBindingNamesSubject(item) {
				matched = append(matched, item)
			}
		}
	}
	if len(matched) > 1 && key.Kind == ownerPRTB {
		matched = narrowByProject(matched, b, roleName)
	}
	switch len(matched) {
	case 0:
		w.issue(stageLabelTraversal, object, codeUnverifiedCandidate, fmt.Sprintf("no %s of the subject matches the label %s", key.Kind, key.Key))
		return nil, false
	case 1:
		item := matched[0]
		kind := string(key.Kind)
		ok, leaf := w.enter(kind, item.GetNamespace(), item.GetName(), depth)
		if !ok {
			return []chainNode{leaf}, true
		}
		defer w.leave(kind, item.GetNamespace(), item.GetName())
		node := chainNode{Kind: kind, Name: item.GetName(), Namespace: item.GetNamespace(), Children: []chainNode{}}
		return []chainNode{w.fillOwnerNode(node, item, depth)}, true
	default:
		var cands []string
		for _, m := range matched {
			cands = append(cands, m.GetNamespace()+"/"+m.GetName())
		}
		w.issue(stageLabelTraversal, object, codeUnverifiedCandidate, fmt.Sprintf("%d %ss of the subject match the label %s: %s", len(matched), key.Kind, key.Key, strings.Join(cands, ", ")))
		return nil, false
	}
}

// narrowByProject keeps the PRTBs whose project owns the binding: a
// ClusterRoleBinding bound to "<project>-namespaces-<rt>" names the
// project in the role; a RoleBinding's namespace carries
// field.cattle.io/projectId. When neither applies, all are kept.
func narrowByProject(matched []*unstructured.Unstructured, b *unstructured.Unstructured, roleName string) []*unstructured.Unstructured {
	project := ""
	if p, _, ok := strings.Cut(roleName, "-namespaces-"); ok && p != "" {
		project = p
	}
	if project == "" {
		return matched
	}
	var out []*unstructured.Unstructured
	for _, m := range matched {
		pn, _, _ := unstructured.NestedString(m.Object, "projectName")
		if _, short, ok := strings.Cut(pn, ":"); ok && short == project {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return matched
	}
	return out
}

// aggregationCandidates lists the CRTBs of the cluster or the PRTBs of
// the cluster's projects, once per explanation.
func (w *chainWalker) aggregationCandidates(kind ownerKind) (*unstructured.UnstructuredList, *chainIssue) {
	if kind == ownerCRTB {
		if !w.crtbsListed {
			w.crtbsListed = true
			k, _ := kinds.Lookup("clusterroletemplatebinding")
			list, err := w.list(w.mgmt, k.GVR, w.clusterID)
			if err != nil {
				w.crtbsErr = w.issueOf(err)
			} else {
				w.crtbs = list
			}
		}
		return w.crtbs, w.crtbsErr
	}
	if !w.prtbsListed {
		w.prtbsListed = true
		k, _ := kinds.Lookup("projectroletemplatebinding")
		list, err := w.list(w.mgmt, k.GVR, "")
		if err != nil {
			w.prtbsErr = w.issueOf(err)
		} else {
			kept := &unstructured.UnstructuredList{}
			for i := range list.Items {
				pn, _, _ := unstructured.NestedString(list.Items[i].Object, "projectName")
				if strings.HasPrefix(pn, w.clusterID+":") {
					kept.Items = append(kept.Items, list.Items[i])
				}
			}
			w.prtbs = kept
		}
	}
	return w.prtbs, w.prtbsErr
}

// issueOf maps a request error to an issue body without recording it.
func (w *chainWalker) issueOf(err error) *chainIssue {
	before := len(w.issues)
	w.requestIssue("", "", err)
	iss := w.issues[before]
	w.issues = w.issues[:before]
	return &iss
}

// managementBindingNamesSubject reports whether a CRTB or PRTB names the
// subject: userName equal to the Rancher user id, userPrincipalName among
// the principal ids, or groupPrincipalName among the groups.
func (w *chainWalker) managementBindingNamesSubject(obj *unstructured.Unstructured) bool {
	userName, _, _ := unstructured.NestedString(obj.Object, "userName")
	userPrincipal, _, _ := unstructured.NestedString(obj.Object, "userPrincipalName")
	groupPrincipal, _, _ := unstructured.NestedString(obj.Object, "groupPrincipalName")
	switch {
	case userName != "" && (userName == w.subject.ID || userName == w.subject.Name):
		return true
	case userPrincipal != "" && slices.Contains(w.subject.PrincipalIDs, userPrincipal):
		return true
	case groupPrincipal != "" && ((w.subject.Kind == KindGroup && groupPrincipal == w.subject.Name) || slices.Contains(w.subject.Groups, groupPrincipal)):
		return true
	}
	return false
}

// roleNode GETs the bound role and notes the rules in it covering the
// request. owner is non-nil when a Rancher parent was verified for the
// binding: the role then makes no source claim; otherwise a
// clusterrole-owner mark (annotation or label) yields a RoleTemplate
// child, a gr-owner label a GlobalRole child, and nothing else is claimed.
func (w *chainWalker) roleNode(roleKind, roleName, bindingNS string, owner *string, depth int) chainNode {
	ns := ""
	gvr := clusterRoleGVR
	if roleKind == "Role" {
		ns, gvr = bindingNS, roleGVR
	}
	ok, leaf := w.enter(roleKind, ns, roleName, depth)
	if !ok {
		return leaf
	}
	defer w.leave(roleKind, ns, roleName)
	node := chainNode{Kind: roleKind, Name: roleName, Namespace: ns, Children: []chainNode{}}
	role, err := w.get(w.cluster, gvr, ns, roleName)
	if err != nil {
		w.requestIssue(stageRoleRead, objectName(roleKind, ns, roleName), err)
		node.Note = rulesUnreadableNote
		return node
	}
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	node.Note = rulesCoveringNote(rules, w.question)
	// An aggregating ClusterRole names the roles it aggregates through its
	// selectors: a verified ClusterRole -> ClusterRole edge on the
	// aggregation path (<rt>-aggregator -> <rt> and the inherited
	// templates' roles), followed with one LIST per selector.
	node.Children = append(node.Children, w.aggregatedRoles(role, depth+1)...)
	if owner != nil {
		return node
	}
	rt := role.GetAnnotations()[clusterRoleOwnerKey]
	if rt == "" {
		rt = role.GetLabels()[clusterRoleOwnerKey]
	}
	switch {
	case rt != "":
		node.Note += "; RoleTemplate " + rt
		node.Children = append(node.Children, w.roleTemplateNode(rt, depth+1))
	case role.GetLabels()[grOwnerLabel] != "":
		gr := role.GetLabels()[grOwnerLabel]
		node.Note += "; GlobalRole " + gr
		node.Children = append(node.Children, w.globalRoleNode(gr, depth+1))
	}
	return node
}

// aggregatedRoles follows a ClusterRole's aggregationRule: every
// clusterRoleSelectors entry's matchLabels becomes one LIST of ClusterRoles,
// each match a child role node noted as aggregated by the parent. Selectors
// with matchExpressions are not followed (Rancher writes matchLabels only);
// a failed LIST is a roleRead issue.
func (w *chainWalker) aggregatedRoles(role *unstructured.Unstructured, depth int) []chainNode {
	selectors, _, _ := unstructured.NestedSlice(role.Object, "aggregationRule", "clusterRoleSelectors")
	var out []chainNode
	for _, sel := range selectors {
		m, ok := sel.(map[string]any)
		if !ok {
			continue
		}
		labels, _, _ := unstructured.NestedMap(m, "matchLabels")
		if len(labels) == 0 {
			continue
		}
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			v, _ := labels[k].(string)
			parts = append(parts, k+"="+v)
		}
		selector := strings.Join(parts, ",")
		w.requests++
		list, err := w.cluster.List(w.ctx, clusterRoleGVR, "", selector)
		if err != nil {
			w.requestIssue(stageRoleRead, "ClusterRoles "+selector, err)
			continue
		}
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].GetName())
		}
		slices.Sort(names)
		for _, name := range names {
			if name == role.GetName() {
				continue
			}
			ok, leaf := w.enter("ClusterRole", "", name, depth)
			if !ok {
				out = append(out, leaf)
				continue
			}
			child := chainNode{Kind: "ClusterRole", Name: name, Note: "aggregated by " + role.GetName() + " (" + selector + ")", Children: []chainNode{}}
			for i := range list.Items {
				if list.Items[i].GetName() == name {
					child.Children = append(child.Children, w.aggregatedRoles(&list.Items[i], depth+1)...)
				}
			}
			w.leave("ClusterRole", "", name)
			out = append(out, child)
		}
	}
	return out
}

// rulesCoveringNote lists the rules of a role that cover the question, in
// the role's own order: "<group>/<resource> [verbs]" per rule, or the
// non-resource URL. It is a display filter; the server chose the binding.
func rulesCoveringNote(rules []any, q Question) string {
	var parts []string
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		verbs := stringSlice(m["verbs"])
		if !slices.Contains(verbs, "*") && !slices.Contains(verbs, q.Verb) {
			continue
		}
		if q.NonResourceURL != "" {
			for _, u := range stringSlice(m["nonResourceURLs"]) {
				if u == q.NonResourceURL || (strings.HasSuffix(u, "*") && strings.HasPrefix(q.NonResourceURL, strings.TrimSuffix(u, "*"))) {
					parts = append(parts, u+" ["+strings.Join(verbs, " ")+"]")
					break
				}
			}
			continue
		}
		groups := stringSlice(m["apiGroups"])
		if !slices.Contains(groups, "*") && !slices.Contains(groups, q.Group) {
			continue
		}
		want := q.Resource
		if q.Subresource != "" {
			want += "/" + q.Subresource
		}
		resources := stringSlice(m["resources"])
		matched := slices.Contains(resources, "*") || slices.Contains(resources, want) ||
			(q.Subresource != "" && slices.Contains(resources, "*/"+q.Subresource))
		if !matched {
			continue
		}
		if names := stringSlice(m["resourceNames"]); len(names) > 0 && (q.Name == "" || !slices.Contains(names, q.Name)) {
			continue
		}
		group := q.Group
		if group == "" {
			group = "core"
		}
		parts = append(parts, group+"/"+want+" ["+strings.Join(verbs, " ")+"]")
	}
	if len(parts) == 0 {
		return "no rule in this role covers the request as read; the server chose the binding"
	}
	return "rules covering the request: " + strings.Join(parts, ", ")
}

func stringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// roleTemplateNode GETs a RoleTemplate and follows roleTemplateNames.
func (w *chainWalker) roleTemplateNode(name string, depth int) chainNode {
	ok, leaf := w.enter(nodeRoleTemplate, "", name, depth)
	if !ok {
		return leaf
	}
	defer w.leave(nodeRoleTemplate, "", name)
	node := chainNode{Kind: nodeRoleTemplate, Name: name, Children: []chainNode{}}
	k, _ := kinds.Lookup("roletemplate")
	rt, err := w.get(w.mgmt, k.GVR, "", name)
	if err != nil {
		w.requestIssue(stageManagementRead, objectName(nodeRoleTemplate, "", name), err)
		return node
	}
	inherits, _, _ := unstructured.NestedStringSlice(rt.Object, "roleTemplateNames")
	slices.Sort(inherits)
	if external, _, _ := unstructured.NestedBool(rt.Object, "external"); external {
		node.Note = "external: binds the cluster's own ClusterRole " + name
	}
	if len(inherits) > 0 {
		node.Note = strings.TrimSpace(strings.TrimPrefix(node.Note+"; inherits RoleTemplate "+strings.Join(inherits, ", "), "; "))
	}
	for _, child := range inherits {
		node.Children = append(node.Children, w.roleTemplateNode(child, depth+1))
	}
	return node
}

// globalRoleBindingNode GETs a GlobalRoleBinding, verifies it names the
// subject and (when the binding said which) the GlobalRole, and follows
// the GlobalRole.
func (w *chainWalker) globalRoleBindingNode(name, expectGlobalRole string, depth int) chainNode {
	ok, leaf := w.enter(nodeGlobalRoleBind, "", name, depth)
	if !ok {
		return leaf
	}
	defer w.leave(nodeGlobalRoleBind, "", name)
	node := chainNode{Kind: nodeGlobalRoleBind, Name: name, Children: []chainNode{}}
	k, _ := kinds.Lookup("globalrolebinding")
	grb, err := w.get(w.mgmt, k.GVR, "", name)
	if err != nil {
		w.requestIssue(stageManagementRead, objectName(nodeGlobalRoleBind, "", name), err)
		return node
	}
	gr, _, _ := unstructured.NestedString(grb.Object, "globalRoleName")
	node.Note = strings.TrimSpace(bindingSubjectText(grb) + ", GlobalRole " + gr)
	if !w.managementBindingNamesSubject(grb) {
		w.issue(stageLabelTraversal, objectName(nodeGlobalRoleBind, "", name), codeUnverifiedCandidate, "the GlobalRoleBinding does not name the subject")
		return node
	}
	if expectGlobalRole != "" && gr != expectGlobalRole {
		w.issue(stageLabelTraversal, objectName(nodeGlobalRoleBind, "", name), codeUnverifiedCandidate,
			fmt.Sprintf("the binding is annotated with GlobalRole %s but the GlobalRoleBinding names %s", expectGlobalRole, gr))
		return node
	}
	if gr != "" {
		node.Children = append(node.Children, w.globalRoleNode(gr, depth+1))
	}
	return node
}

// globalRoleNode GETs a GlobalRole and follows inheritedClusterRoles.
func (w *chainWalker) globalRoleNode(name string, depth int) chainNode {
	ok, leaf := w.enter(nodeGlobalRole, "", name, depth)
	if !ok {
		return leaf
	}
	defer w.leave(nodeGlobalRole, "", name)
	node := chainNode{Kind: nodeGlobalRole, Name: name, Children: []chainNode{}}
	k, _ := kinds.Lookup("globalrole")
	gr, err := w.get(w.mgmt, k.GVR, "", name)
	if err != nil {
		w.requestIssue(stageManagementRead, objectName(nodeGlobalRole, "", name), err)
		return node
	}
	inherited, _, _ := unstructured.NestedStringSlice(gr.Object, "inheritedClusterRoles")
	slices.Sort(inherited)
	if len(inherited) > 0 {
		node.Note = "inheritedClusterRoles: RoleTemplate " + strings.Join(inherited, ", ")
	}
	for _, rt := range inherited {
		node.Children = append(node.Children, w.roleTemplateNode(rt, depth+1))
	}
	return node
}

// heldBindings adds, once, the bindings the subject holds on this cluster
// on the management plane: what a caller can read when the downstream
// refuses the RBAC objects, labelled as held, never as the source of the
// accepted binding.
func (w *chainWalker) heldBindings(parent *chainNode, depth int) {
	if w.heldShown {
		return
	}
	w.heldShown = true
	node := chainNode{Kind: nodeHeldBindings, Note: heldBindingsTitle, Children: []chainNode{}}
	userID := w.subject.ID
	if userID == "" {
		userID = w.subject.Name
	}
	w.requests++
	clusterRows, _, err := rbac.HeldClusterBindings(w.ctx, w.mgmt, userID, w.subject.PrincipalIDs, w.subject.Groups, nil)
	if err != nil {
		w.requestIssue(stageManagementRead, "ClusterRoleTemplateBindings", err)
	}
	w.requests++
	projectRows, _, err := rbac.HeldProjectBindings(w.ctx, w.mgmt, userID, w.subject.PrincipalIDs, w.subject.Groups, nil)
	if err != nil {
		w.requestIssue(stageManagementRead, "ProjectRoleTemplateBindings", err)
	}
	for _, scope := range clusterRows {
		if scope.ID != w.clusterID {
			continue
		}
		for _, row := range scope.Rows {
			node.Children = append(node.Children, w.ownerNode(ownerRef{Kind: ownerCRTB, Namespace: w.clusterID, Name: row.Binding}, depth+1))
		}
	}
	for _, scope := range projectRows {
		if !strings.HasPrefix(scope.ID, w.clusterID+":") {
			continue
		}
		for _, row := range scope.Rows {
			// A row carries the binding name and the project id, not the
			// PRTB's namespace (the project's backing namespace); the PRTB is
			// found in the memoized list by both, since every project's
			// creator binding shares one name.
			node.Children = append(node.Children, w.prtbFor(scope.ID, row.Binding, depth+1))
		}
	}
	slices.SortFunc(node.Children, func(a, b chainNode) int {
		return strings.Compare(a.Kind+"\x00"+a.Namespace+"\x00"+a.Name, b.Kind+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	parent.Children = append(parent.Children, node)
}

// prtbFor finds the PRTB named name in the project projectName
// ("<cluster>:<project>") through the memoized PRTB list (one LIST per
// explanation) and builds its node. Both fields are needed: every project's
// creator binding is named creator-project-owner.
func (w *chainWalker) prtbFor(projectName, name string, depth int) chainNode {
	list, issue := w.aggregationCandidates(ownerPRTB)
	if issue != nil {
		w.issues = append(w.issues, chainIssue{Stage: stageManagementRead, Object: objectName(string(ownerPRTB), "", name), Code: issue.Code, Retryable: issue.Retryable, Message: issue.Message})
		return chainNode{Kind: string(ownerPRTB), Name: name, Children: []chainNode{}}
	}
	for i := range list.Items {
		item := &list.Items[i]
		pn, _, _ := unstructured.NestedString(item.Object, "projectName")
		if item.GetName() != name || pn != projectName {
			continue
		}
		ok, leaf := w.enter(string(ownerPRTB), item.GetNamespace(), name, depth)
		if !ok {
			return leaf
		}
		defer w.leave(string(ownerPRTB), item.GetNamespace(), name)
		return w.fillOwnerNode(chainNode{Kind: string(ownerPRTB), Name: name, Namespace: item.GetNamespace(), Children: []chainNode{}}, item, depth)
	}
	w.issue(stageManagementRead, objectName(string(ownerPRTB), "", name), clierr.CodeNotFound, "listed as held in "+projectName+" but not among the cluster's ProjectRoleTemplateBindings")
	return chainNode{Kind: string(ownerPRTB), Name: name, Children: []chainNode{}}
}

// renderTree renders a node and its children as ASCII lines: "+--" per
// child, "|" continuation, node text "<Kind> <ns>/<name>" with the note
// after "-". mark ("[allowed]" or "[denied]") prefixes the root only.
func renderTree(node chainNode, mark string) []string {
	var lines []string
	var walk func(n chainNode, prefix string, last bool, root bool)
	walk = func(n chainNode, prefix string, last bool, root bool) {
		label := n.Kind
		switch {
		case n.Namespace != "" && n.Name != "":
			label += " " + n.Namespace + "/" + n.Name
		case n.Name != "":
			label += " " + n.Name
		}
		if root && mark != "" {
			label = mark + " " + label
		}
		if n.Note != "" {
			label += " - " + output.Printable(n.Note)
		}
		if root {
			lines = append(lines, label)
		} else {
			lines = append(lines, prefix+"+-- "+label)
		}
		childPrefix := prefix
		if !root {
			if last {
				childPrefix += "    "
			} else {
				childPrefix += "|   "
			}
		}
		for i, c := range n.Children {
			walk(c, childPrefix, i == len(n.Children)-1, false)
		}
	}
	walk(node, "", true, true)
	return lines
}

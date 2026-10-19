package resources

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// handleLookupTimeout bounds the read a lookup makes after a miss. It is
// below the 30 s request limit; a slow server cannot stretch a command that
// already failed. A test that lowers it runs serially and restores it.
var handleLookupTimeout = 10 * time.Second

// argName is the name of a generated verb's positional argument: the row's
// handle name where it has one, "name" otherwise.
func argName(k kinds.Kind) string {
	if k.Handle != nil {
		return k.Handle.Arg
	}
	return "name"
}

// argSpec declares a generated verb's positional argument.
func argSpec(k kinds.Kind, required bool) runtime.ArgSpec {
	return runtime.ArgSpec{Name: argName(k), Type: "string", Required: required, Desc: k.ArgDesc}
}

// handleMatch is one object whose looked-up field equals the typed text:
// its id, the name to show for it, and the labels of the fields that match.
type handleMatch struct {
	id, name string
	labels   []string
}

// lookupHandle adds what the caller needs to a miss on a kind whose
// argument is an id: when the typed text is another field of an object, the
// message names that object's id, the candidates list the matches and the
// hint is the command that reads the object. err is the error of the Get
// that carried typed; ns is the namespace that Get used, which for a
// project is its cluster id.
//
// It acts on a NOT_FOUND and, for a project, also on an RBAC refusal: a
// project member's role grants get on the project's id only; a member who
// types a display name is refused instead of told NOT_FOUND. Any other
// error, no match, a failed or cancelled lookup, and a timeout return err
// itself. It only reads and never acts on a match.
func lookupHandle(ctx context.Context, clients engine.ClusterClientFactory, client engine.ClusterClient, k kinds.Kind, ns, typed string, err error) error {
	if k.Handle == nil || err == nil || typed == "" {
		return err
	}
	refused := false
	base := clierr.AsNotFound(err)
	if base == nil {
		ce, ok := errors.AsType[*clierr.Error](err)
		if !ok || k.Key != "project" || ce.Code != clierr.CodeRBAC {
			return err
		}
		base, refused = ce, true
	}
	lctx, cancel := context.WithTimeout(ctx, handleLookupTimeout)
	defer cancel()
	matches, ok := findHandleMatches(lctx, clients, client, k, ns, typed)
	if !ok || lctx.Err() != nil || len(matches) == 0 {
		return err
	}
	slices.SortFunc(matches, func(a, b handleMatch) int { return strings.Compare(a.id, b.id) })

	quoted := clierr.ShellQuote(typed)
	var clusterFlag string
	if k.Key == "project" {
		clusterFlag = " --cluster " + clierr.ShellQuote(ns)
	}
	candidates := make([]clierr.Candidate, 0, len(matches))
	for _, m := range matches {
		candidates = append(candidates, clierr.Candidate{ID: m.id, Name: m.name})
	}
	if len(matches) == 1 {
		m := matches[0]
		msg := base.Message + "; " + quoted + " is the " + strings.Join(m.labels, " and ") + " of " + k.Name + " " + m.id
		return base.WithMessage(msg).WithCandidates(candidates).
			WithHint(clierr.Rancher + " get " + k.Name + " " + clierr.ShellQuote(m.id) + clusterFlag)
	}
	labels := make([]string, 0, len(k.Handle.Lookup))
	for _, f := range k.Handle.Lookup {
		labels = append(labels, f.Label)
	}
	msg := base.Message + "; " + strconv.Itoa(len(matches)) + " " + k.Aliases[0] + " have that " + strings.Join(labels, " or ")
	out := base.WithMessage(msg).WithCandidates(candidates)
	switch {
	case refused:
		out = out.WithHint(clierr.Rancher + " access projects" + clusterFlag)
	case out.Hint == "":
		out = out.WithHint("list with: " + clierr.Rancher + " get " + k.Aliases[0])
	}
	return out
}

// findHandleMatches returns the objects of k whose Lookup fields equal typed,
// exactly and case-sensitively, one entry per id; false when the lookup
// cannot be made or fails. A project is found among the projects the caller
// can read on its cluster, through the factory's project lister; every other
// kind through one LIST with the client the verb already has.
func findHandleMatches(ctx context.Context, clients engine.ClusterClientFactory, client engine.ClusterClient, k kinds.Kind, ns, typed string) ([]handleMatch, bool) {
	var matches []handleMatch
	if k.Key == "project" {
		lister, ok := clients.(engine.ProjectNameLister)
		if !ok || ns == "" {
			return nil, false
		}
		refs, err := lister.ListProjects(ctx, ns)
		if err != nil {
			return nil, false
		}
		for _, r := range refs {
			if r.DisplayName == typed && !slices.ContainsFunc(matches, func(m handleMatch) bool { return m.id == r.ID }) {
				matches = append(matches, handleMatch{id: r.ID, name: r.DisplayName, labels: []string{k.Handle.Lookup[0].Label}})
			}
		}
		return matches, true
	}
	list, err := client.List(ctx, k.GVR, ns, "")
	if err != nil || list == nil {
		return nil, false
	}
	for _, item := range list.Items {
		m := handleMatch{id: item.GetName()}
		for _, f := range k.Handle.Lookup {
			v, _, _ := unstructured.NestedString(item.Object, f.Path...)
			if v == "" {
				continue
			}
			m.name = v
			if v == typed {
				m.labels = append(m.labels, f.Label)
			}
		}
		if len(m.labels) > 0 && !slices.ContainsFunc(matches, func(o handleMatch) bool { return o.id == m.id }) {
			matches = append(matches, m)
		}
	}
	return matches, true
}

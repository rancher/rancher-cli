package fidelity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// The observations of a delete probe, by name.
const (
	dwgGet      = "get"
	dwgDryRun   = "dry-run-delete"
	dwgMismatch = "delete-with-a-different-uid"
)

// deleteProbeRequest names the object the observations of a delete probe
// address, as its golden records it with placeholders for the names, and the
// observations the probe sends.
type deleteProbeRequest struct {
	gvr      schema.GroupVersionResource
	ns, name string
	// uidCapture is the capture the object's uid records as in a Status
	// ({<uidCapture>}); an object in the response records its uid as <uid>.
	uidCapture   string
	observations []string
}

var (
	dwgConfigMaps   = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	dwgNamespaces   = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	dwgClusterRoles = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	dwgProbeObjects = schema.GroupVersionResource{Group: "{crdgroup}", Version: "v1", Resource: dwgCRDPlural}
)

// deleteProbeRequests has a row for every probe deleteCannotGetProbes
// returns.
var deleteProbeRequests = map[string]deleteProbeRequest{
	"delete/configmap-dry-run-and-uid-mismatch": {dwgConfigMaps, "{ns}", dwgConfigMap, "cmuid", []string{dwgDryRun, dwgMismatch}},
	"delete/pod-dry-run": {schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "{ns}", dwgPod, "poduid",
		[]string{dwgDryRun}},
	"delete/custom-resource-dry-run":                       {dwgProbeObjects, "{ns}", dwgResource, "cruid", []string{dwgDryRun}},
	"delete/namespace-dry-run-and-uid-mismatch":            {dwgNamespaces, "", "{ns}", "nsuid", []string{dwgDryRun, dwgMismatch}},
	"delete/get-refused-for-a-delete-only-role":            {dwgConfigMaps, "{ns}", dwgConfigMap, "cmuid", []string{dwgGet, dwgDryRun, dwgMismatch}},
	"delete-downstream/get-refused-for-a-delete-only-role": {dwgConfigMaps, "{ns}", dwgConfigMap, "cmuid", []string{dwgGet, dwgDryRun, dwgMismatch}},
	"delete/get-refused-at-the-cluster-scope":              {dwgClusterRoles, "", "{role}", "", []string{dwgGet}},
	"delete/get-refused-for-a-namespace":                   {dwgNamespaces, "", "{ns}", "", []string{dwgGet}},
	"delete/get-refused-in-a-named-group":                  {dwgProbeObjects, "{ns}", dwgResource, "", []string{dwgGet}},
}

// deleteProbeRun replaces the {ns}, {role}, {crdgroup} and {userid}
// placeholders of a delete golden with example names; the uid placeholders
// stay.
var deleteProbeRun = strings.NewReplacer(
	"{ns}", fixturePrefix+"dwg-0a1b2c3d",
	"{role}", fixturePrefix+"dwg-4e5f6a7b",
	"{crdgroup}", fixturePrefix+"8c9d0e1f.example",
	"{userid}", "u-dwg",
)

// deleteGoldenError reports why golden g of probe p does not record a fact
// the delete of an object the caller cannot get depends on. It checks that the
// probe sends exactly the observations of req and that the golden has each.
// It serves each recorded response, with example names in place of the
// placeholders, to the cluster client the CLI uses, and reads it as the
// observation requires: a get observation must be the authorizer's refusal of
// that GET (clierr.AuthorizerRefused), a dry-run-delete observation must give
// DeleteWithUID the uid of the object, and a delete-with-a-different-uid
// observation must be a uid conflict (clierr.UIDConflict). It returns nil
// when the golden records every fact.
func deleteGoldenError(ctx context.Context, p Probe, g Golden, req deleteProbeRequest) error {
	var sent []string
	for _, o := range p.Observations {
		sent = append(sent, o.Name)
	}
	if !slices.Equal(sent, req.observations) {
		return fmt.Errorf("the probe sends the observations %v, want %v", sent, req.observations)
	}
	for _, name := range req.observations {
		i := slices.IndexFunc(g.Observations, func(r ObservationResult) bool { return r.Name == name })
		if i < 0 {
			return fmt.Errorf("the golden has no %s observation", name)
		}
		if err := deleteObservationError(ctx, g.Observations[i], req); err != nil {
			return fmt.Errorf("the %s observation %w", name, err)
		}
	}
	return nil
}

// deleteObservationError serves o to a cluster client and reports why the
// client does not read it as the observation requires.
func deleteObservationError(ctx context.Context, o ObservationResult, req deleteProbeRequest) error {
	body := []byte(o.Text)
	if o.Body != nil {
		var err error
		if body, err = json.Marshal(o.Body); err != nil {
			return fmt.Errorf("cannot be encoded: %w", err)
		}
	}
	body = []byte(deleteProbeRun.Replace(string(body)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", o.ContentType)
		w.WriteHeader(o.Status)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c, err := provider.NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
	if err != nil {
		return fmt.Errorf("cannot be checked: no cluster client: %w", err)
	}
	gvr := req.gvr
	gvr.Group = deleteProbeRun.Replace(gvr.Group)
	ns, name := deleteProbeRun.Replace(req.ns), deleteProbeRun.Replace(req.name)
	d, ok := c.(engine.UIDDeleter)
	if !ok {
		return errors.New("cannot be checked: the cluster client cannot send a uid precondition")
	}
	switch o.Name {
	case dwgGet:
		_, err := c.Get(ctx, gvr, ns, name)
		if !clierr.AuthorizerRefused(err, "get", gvr.Group, gvr.Resource, ns, name) {
			return fmt.Errorf("records status %d, which is not the authorizer's refusal of the GET: %v", o.Status, err)
		}
	case dwgDryRun:
		uid, err := d.DeleteWithUID(ctx, gvr, ns, name, "", true)
		if err != nil {
			return fmt.Errorf("records status %d, which DeleteWithUID returns as an error: %w", o.Status, err)
		}
		if want := "{" + req.uidCapture + "}"; uid != want && uid != "<uid>" {
			return fmt.Errorf("records the uid %q, want %s or <uid>", uid, want)
		}
	case dwgMismatch:
		_, err := d.DeleteWithUID(ctx, gvr, ns, name, dwgOtherUID, false)
		if !clierr.UIDConflict(err) {
			return fmt.Errorf("records status %d, which is not a uid conflict: %v", o.Status, err)
		}
	default:
		return errors.New("has no check")
	}
	return nil
}

// loadDeleteGolden is the recorded golden of probe p: under testdata/downstream
// for a downstream probe, else under testdata/oracle.
func loadDeleteGolden(t *testing.T, p Probe) Golden {
	t.Helper()
	dir := "oracle"
	if p.Downstream {
		dir = "downstream"
	}
	g, err := LoadGolden(GoldenPath(filepath.Join("testdata", dir), p.Name))
	require.NoError(t, err)
	return g
}

// TestTheClusterClientReadsTheRecordedDeleteResponses asserts, for every
// delete probe, that the cluster client and clierr read each recorded
// response as the delete of an object the caller cannot get needs: a
// refused GET as the authorizer's refusal of that GET, the dry-run delete as
// the uid of the object, and the delete with a different uid as a uid
// conflict. A re-recording that loses one of them fails this test; a delete
// probe with no row in deleteProbeRequests fails it too.
func TestTheClusterClientReadsTheRecordedDeleteResponses(t *testing.T) {
	t.Parallel()
	probes := deleteCannotGetProbes()
	for _, p := range probes {
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			req, ok := deleteProbeRequests[p.Name]
			require.True(t, ok, "%s has a row in deleteProbeRequests", p.Name)
			require.NoError(t, deleteGoldenError(t.Context(), p, loadDeleteGolden(t, p), req))
		})
	}
	for name := range deleteProbeRequests {
		assert.True(t, slices.ContainsFunc(probes, func(p Probe) bool { return p.Name == name }), "%s is listed in deleteProbeRequests and is not a delete probe; remove it", name)
	}
}

// dwgStatus is the Status of se as the body of an observation, as the API
// server writes it.
func dwgStatus(t *testing.T, se *apierrors.StatusError) map[string]any {
	t.Helper()
	st := se.ErrStatus
	st.Kind, st.APIVersion = "Status", "v1"
	raw, err := json.Marshal(st)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

// writtenDeleteGolden returns a golden for req with each response as the API
// server writes it and the recording stores it: for a get the authorizer's
// 403; for a dry-run delete the object marked for deletion (a Pod or a
// Namespace), or else a Status with details.uid; for a delete with a
// different uid the 409 of a uid precondition that does not match.
func writtenDeleteGolden(t *testing.T, req deleteProbeRequest) Golden {
	t.Helper()
	gr := schema.GroupResource{Group: req.gvr.Group, Resource: req.gvr.Resource}
	var g Golden
	for _, name := range req.observations {
		o := ObservationResult{Name: name, ContentType: "application/json"}
		switch name {
		case dwgGet:
			scope := " at the cluster scope"
			switch {
			case req.ns != "":
				scope = " in the namespace " + strconv.Quote(req.ns)
			case req.gvr.Resource == "namespaces" && req.gvr.Group == "":
				scope = " in the namespace " + strconv.Quote(req.name)
			}
			o.Status = http.StatusForbidden
			o.Body = dwgStatus(t, apierrors.NewForbidden(gr, req.name, fmt.Errorf("User %q cannot get resource %q in API group %q%s",
				"{userid}", req.gvr.Resource, req.gvr.Group, scope)))
		case dwgDryRun:
			o.Status = http.StatusOK
			// The API server responds to the dry-run delete of a Pod or a
			// Namespace with the object it marks for deletion, and to the
			// dry-run delete of a ConfigMap or a custom resource with a Status
			// whose details have the uid.
			switch req.gvr.Resource {
			case "pods", "namespaces":
				kind := map[string]string{"pods": "Pod", "namespaces": "Namespace"}[req.gvr.Resource]
				o.Body = map[string]any{"kind": kind, "apiVersion": "v1",
					"metadata": map[string]any{"name": req.name, "uid": "<uid>", "deletionTimestamp": "<ts>"}}
			default:
				details := map[string]any{"name": req.name, "kind": req.gvr.Resource, "uid": "{" + req.uidCapture + "}"}
				if req.gvr.Group != "" {
					details["group"] = req.gvr.Group
				}
				o.Body = map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Success",
					"details": details}
			}
		case dwgMismatch:
			text := "the UID in the precondition (" + dwgOtherUID + ") does not match the UID in record ({" + req.uidCapture + "}). The object might have been deleted and then recreated"
			if req.gvr.Resource == "namespaces" {
				text = "Precondition failed: UID in precondition: " + dwgOtherUID + ", UID in object meta: {" + req.uidCapture + "}"
			}
			// The API server names a ConfigMap by its kind in this 409 and a
			// namespace by its resource.
			conflict := gr
			if req.gvr.Resource == "configmaps" {
				conflict.Resource = "ConfigMap"
			}
			o.Status = http.StatusConflict
			o.Body = dwgStatus(t, apierrors.NewConflict(conflict, req.name, errors.New(text)))
		}
		g.Observations = append(g.Observations, o)
	}
	return g
}

// TestTheDeleteGoldenCheckRejectsAGoldenThatLostTheFact asserts that the check
// passes the golden of every delete probe with its responses as the API server
// writes them, and that it fails, with a message for each case, on such a
// golden changed in memory: a GET that returns another status or is refused
// for another verb, a dry-run delete with no uid or refused, a 409 that is not
// a uid conflict, a missing observation, and an observation the check does not
// know.
func TestTheDeleteGoldenCheckRejectsAGoldenThatLostTheFact(t *testing.T) {
	t.Parallel()
	byName := map[string]Probe{}
	for _, p := range deleteCannotGetProbes() {
		byName[p.Name] = p
	}
	for name, req := range deleteProbeRequests {
		t.Run("the golden of "+name+" as the server writes it", func(t *testing.T) {
			t.Parallel()
			p, ok := byName[name]
			require.True(t, ok, "%s is a delete probe", name)
			require.NoError(t, deleteGoldenError(t.Context(), p, writtenDeleteGolden(t, req), req))
		})
	}

	const probe = "delete/get-refused-for-a-delete-only-role"
	req := deleteProbeRequests[probe]
	rows := []struct {
		name        string
		observation string
		change      func(o *ObservationResult)
		want        string
	}{
		{"the GET is not found", dwgGet, func(o *ObservationResult) {
			o.Status, o.Body["code"], o.Body["reason"] = http.StatusNotFound, 404, "NotFound"
		},
			"the get observation records status 404, which is not the authorizer's refusal of the GET"},
		{"the GET is refused for another verb", dwgGet,
			func(o *ObservationResult) {
				o.Body["message"] = strings.Replace(o.Body["message"].(string), "cannot get", "cannot list", 1)
			},
			"the get observation records status 403, which is not the authorizer's refusal of the GET"},
		{"the dry-run delete has no uid", dwgDryRun, func(o *ObservationResult) { delete(o.Body["details"].(map[string]any), "uid") },
			`the dry-run-delete observation records the uid "", want {cmuid} or <uid>`},
		{"the dry-run delete is refused", dwgDryRun, func(o *ObservationResult) {
			o.Status, o.Body["status"], o.Body["code"], o.Body["reason"], o.Body["message"] = http.StatusForbidden, "Failure", 403, "Forbidden", "forbidden"
		}, "the dry-run-delete observation records status 403, which DeleteWithUID returns as an error"},
		{"the 409 is a resourceVersion race", dwgMismatch,
			func(o *ObservationResult) {
				o.Body["message"] = "the object has been modified; please apply your changes to the latest version and try again"
			},
			"the delete-with-a-different-uid observation records status 409, which is not a uid conflict"},
		{"the dry-run delete is missing", dwgDryRun, nil, "the golden has no dry-run-delete observation"},
		{"an observation the check does not know", dwgGet, func(o *ObservationResult) { o.Name = "list" }, "the golden has no get observation"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			g := writtenDeleteGolden(t, req)
			i := slices.IndexFunc(g.Observations, func(r ObservationResult) bool { return r.Name == row.observation })
			require.GreaterOrEqual(t, i, 0)
			if row.change == nil {
				g.Observations = slices.Delete(g.Observations, i, i+1)
			} else {
				row.change(&g.Observations[i])
			}
			assert.ErrorContains(t, deleteGoldenError(t.Context(), byName[probe], g, req), row.want)
		})
	}

	t.Run("a probe that sends fewer observations", func(t *testing.T) {
		t.Parallel()
		p := byName[probe]
		p.Observations = slices.Clone(p.Observations[:1])
		assert.ErrorContains(t, deleteGoldenError(t.Context(), p, writtenDeleteGolden(t, req), req),
			"the probe sends the observations [get], want [get dry-run-delete delete-with-a-different-uid]")
	})
	t.Run("an observation with no check", func(t *testing.T) {
		t.Parallel()
		err := deleteObservationError(t.Context(), ObservationResult{Name: "list", Status: http.StatusOK, ContentType: "application/json", Body: map[string]any{}}, req)
		assert.ErrorContains(t, err, "has no check")
	})
}

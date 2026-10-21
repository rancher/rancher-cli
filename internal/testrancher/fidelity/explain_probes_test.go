package fidelity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/engine/rbac"
)

const testDownstream = "c-m-test"

func adminTarget(srv *httptest.Server) *fakeTarget {
	return &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "admin-tok"}, downstream: testDownstream}
}

func probeByName(t *testing.T, name string) Probe {
	t.Helper()
	for _, p := range All() {
		if p.Name == name {
			return p
		}
	}
	require.FailNow(t, "no probe named "+name)
	return Probe{}
}

func stepByName(t *testing.T, steps []Step, name string) Step {
	t.Helper()
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	require.FailNow(t, "no step named "+name)
	return Step{}
}

func featureServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != aggregatedRoleTemplatesFeaturePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// TestDetectBindingGrammarReadsTheFeatureInRancherOrder asserts the
// detection step picks status.lockedValue, then spec.value, then
// status.default, the order Rancher's features.IsEnabled checks, treating
// null and absent as missing.
func TestDetectBindingGrammarReadsTheFeatureInRancherOrder(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body string
		want bindingGrammar
	}{
		"locked true over spec false":     {`{"spec":{"value":false},"status":{"lockedValue":true,"default":false}}`, grammarAggregation},
		"spec true over default false":    {`{"spec":{"value":true},"status":{"lockedValue":null,"default":false}}`, grammarAggregation},
		"spec false over default true":    {`{"spec":{"value":false},"status":{"lockedValue":null,"default":true}}`, grammarClassic},
		"null spec with default true":     {`{"spec":{"value":null},"status":{"lockedValue":null,"default":true}}`, grammarAggregation},
		"absent spec with default false":  {`{"spec":{},"status":{"default":false}}`, grammarClassic},
		"locked false over spec true":     {`{"spec":{"value":true},"status":{"lockedValue":false,"default":true}}`, grammarClassic},
		"community pair: feature off":     {`{"spec":{"value":null},"status":{"default":false,"lockedValue":null}}`, grammarClassic},
		"no spec object, default true":    {`{"status":{"default":true}}`, grammarAggregation},
		"no status object, spec true":     {`{"spec":{"value":true}}`, grammarAggregation},
		"spec value false, no default":    {`{"spec":{"value":false}}`, grammarClassic},
		"locked true, spec null, default": {`{"spec":{"value":null},"status":{"lockedValue":true,"default":false}}`, grammarAggregation},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := featureServer(http.StatusOK, tc.body)
			defer srv.Close()
			captured := map[string]string{"downstream": testDownstream}
			require.NoError(t, detectBindingGrammarStep().Do(t.Context(), adminTarget(srv), captured))
			assert.Equal(t, string(tc.want), captured[grammarKey])
		})
	}
}

// TestDetectBindingGrammarErrorsNameTheRequestAndTheProblem asserts every
// answer the step cannot read fails the setup with the request path in the
// message, and stores nothing.
func TestDetectBindingGrammarErrorsNameTheRequestAndTheProblem(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		status int
		body   string
		want   string
	}{
		"string spec value":           {http.StatusOK, `{"spec":{"value":"true"},"status":{"default":true}}`, "spec.value is string"},
		"string locked value":         {http.StatusOK, `{"spec":{"value":true},"status":{"lockedValue":"yes"}}`, "status.lockedValue is string"},
		"number default":              {http.StatusOK, `{"spec":{"value":true},"status":{"default":1}}`, "status.default is float64"},
		"not found":                   {http.StatusNotFound, `{"message":"missing"}`, "status 404"},
		"non JSON 200":                {http.StatusOK, `<html>login</html>`, "not a JSON object"},
		"JSON array 200":              {http.StatusOK, `[]`, "not a JSON object"},
		"no boolean in any field":     {http.StatusOK, `{"spec":{"value":null},"status":{"lockedValue":null}}`, "no boolean"},
		"objects without the fields":  {http.StatusOK, `{"spec":{},"status":{}}`, "no boolean"},
		"fields on the wrong objects": {http.StatusOK, `{"value":true,"default":true,"lockedValue":true}`, "no boolean"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := featureServer(tc.status, tc.body)
			defer srv.Close()
			captured := map[string]string{"downstream": testDownstream}
			err := detectBindingGrammarStep().Do(t.Context(), adminTarget(srv), captured)
			require.ErrorContains(t, err, aggregatedRoleTemplatesFeaturePath)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, map[string]string{"downstream": testDownstream}, captured, "a failed detection stores nothing")
		})
	}
}

// TestDetectBindingGrammarRefusesAStoredPlaceholder asserts a selector
// that still holds a placeholder after the cluster id is resolved fails
// the step: a stored value is never substituted again.
func TestDetectBindingGrammarRefusesAStoredPlaceholder(t *testing.T) {
	t.Parallel()
	srv := featureServer(http.StatusOK, `{"status":{"default":false}}`)
	defer srv.Close()
	captured := map[string]string{}
	err := detectBindingGrammarStep().Do(t.Context(), adminTarget(srv), captured)
	require.ErrorContains(t, err, "placeholder")
	assert.Empty(t, captured)
}

// TestDetectBindingGrammarStoresCompleteValues asserts both grammars store
// the grammar and the three selectors under the unrecorded prefix, none
// holding a "{".
func TestDetectBindingGrammarStoresCompleteValues(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body string
		want map[string]string
	}{
		"classic": {`{"spec":{"value":null},"status":{"default":false}}`, map[string]string{
			grammarKey:                      "classic",
			selectorPRTBRoleBindings:        "authz.cluster.cattle.io/rtb-owner-updated=c-m-test-p-e2e-fid-explain_e2e-fid-explain-prtb",
			selectorPRTBClusterRoleBindings: "c-m-test-p-e2e-fid-explain_e2e-fid-explain-prtb=owner-user",
			selectorCRTBClusterRoleBindings: "authz.cluster.cattle.io/rtb-owner-updated=c-m-test_e2e-fid-explain-crtb",
		}},
		"aggregation": {`{"spec":{"value":null},"status":{"default":true}}`, map[string]string{
			grammarKey:                      "aggregation",
			selectorPRTBRoleBindings:        "authz.cluster.cattle.io/prtb-owner-e2e-fid-explain-prtb=true,management.cattle.io/roletemplate-aggregation=true",
			selectorPRTBClusterRoleBindings: "authz.cluster.cattle.io/prtb-owner-e2e-fid-explain-prtb=true,management.cattle.io/roletemplate-aggregation=true",
			selectorCRTBClusterRoleBindings: "authz.cluster.cattle.io/crtb-owner-e2e-fid-explain-crtb=true,management.cattle.io/roletemplate-aggregation=true",
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := featureServer(http.StatusOK, tc.body)
			defer srv.Close()
			captured := map[string]string{"downstream": testDownstream}
			require.NoError(t, detectBindingGrammarStep().Do(t.Context(), adminTarget(srv), captured))
			stored := 0
			for key, value := range captured {
				if !strings.HasPrefix(key, unrecordedPrefix) {
					continue
				}
				stored++
				assert.NotContains(t, value, "{", "stored value %s is complete", key)
			}
			assert.Equal(t, len(tc.want), stored)
			for key, want := range tc.want {
				assert.Equal(t, want, captured[key], key)
			}
		})
	}
}

func reconciledObject(kind string, labels, annotations map[string]any, principal string, lastUpdate string) map[string]any {
	obj := map[string]any{
		"metadata":          map[string]any{"labels": labels, "annotations": annotations},
		"userPrincipalName": principal,
	}
	if kind == "crtb" && lastUpdate != "" {
		obj["status"] = map[string]any{"lastUpdateTime": lastUpdate}
	}
	return obj
}

func reconciledParts(kind string) (labels, annotations map[string]any) {
	labels = map[string]any{
		"auth.management.cattle.io/crb-rb-labels-updated": "true",
		"authz.cluster.cattle.io/crb-rb-labels-updated":   "true",
	}
	annotations = map[string]any{
		"lifecycle.cattle.io/create.mgmt-auth-" + kind + "-controller":  "true",
		"lifecycle.cattle.io/create.cluster-" + kind + "-sync_c-m-test": "true",
	}
	return labels, annotations
}

// TestBindingReconciledPerGrammar asserts the conditions of each grammar:
// the labels-updated labels count only in the classic one, and every other
// condition counts in both.
func TestBindingReconciledPerGrammar(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"prtb", "crtb"} {
		t.Run(kind+" without the labels-updated labels", func(t *testing.T) {
			t.Parallel()
			labels, annotations := reconciledParts(kind)
			clear(labels)
			obj := reconciledObject(kind, labels, annotations, "local://u-x", "2026-10-21T00:00:00Z")
			assert.False(t, bindingReconciled(obj, http.StatusOK, kind, grammarClassic))
			assert.True(t, bindingReconciled(obj, http.StatusOK, kind, grammarAggregation))
		})
		for _, label := range []string{"auth.management.cattle.io/crb-rb-labels-updated", "authz.cluster.cattle.io/crb-rb-labels-updated"} {
			t.Run(kind+" classic without "+label, func(t *testing.T) {
				t.Parallel()
				labels, annotations := reconciledParts(kind)
				delete(labels, label)
				obj := reconciledObject(kind, labels, annotations, "local://u-x", "2026-10-21T00:00:00Z")
				assert.False(t, bindingReconciled(obj, http.StatusOK, kind, grammarClassic))
				assert.True(t, bindingReconciled(obj, http.StatusOK, kind, grammarAggregation))
			})
		}
		t.Run(kind+" with every label is reconciled in both", func(t *testing.T) {
			t.Parallel()
			labels, annotations := reconciledParts(kind)
			obj := reconciledObject(kind, labels, annotations, "local://u-x", "2026-10-21T00:00:00Z")
			assert.True(t, bindingReconciled(obj, http.StatusOK, kind, grammarClassic))
			assert.True(t, bindingReconciled(obj, http.StatusOK, kind, grammarAggregation))
		})
		for _, grammar := range []bindingGrammar{grammarClassic, grammarAggregation} {
			t.Run(fmt.Sprintf("%s %s each other condition alone", kind, grammar), func(t *testing.T) {
				t.Parallel()
				fresh := func() (map[string]any, map[string]any) { return reconciledParts(kind) }
				labels, annotations := fresh()
				assert.False(t, bindingReconciled(reconciledObject(kind, labels, annotations, "local://u-x", "t"), http.StatusAccepted, kind, grammar), "status 200")

				labels, annotations = fresh()
				delete(annotations, "lifecycle.cattle.io/create.mgmt-auth-"+kind+"-controller")
				assert.False(t, bindingReconciled(reconciledObject(kind, labels, annotations, "local://u-x", "t"), http.StatusOK, kind, grammar), "mgmt-auth annotation")

				labels, annotations = fresh()
				delete(annotations, "lifecycle.cattle.io/create.cluster-"+kind+"-sync_c-m-test")
				assert.False(t, bindingReconciled(reconciledObject(kind, labels, annotations, "local://u-x", "t"), http.StatusOK, kind, grammar), "sync annotation")

				labels, annotations = fresh()
				assert.False(t, bindingReconciled(reconciledObject(kind, labels, annotations, "", "t"), http.StatusOK, kind, grammar), "userPrincipalName")

				labels, annotations = fresh()
				obj := reconciledObject(kind, labels, annotations, "local://u-x", "")
				assert.Equal(t, kind == "prtb", bindingReconciled(obj, http.StatusOK, kind, grammar), "status.lastUpdateTime counts for a CRTB only")
			})
		}
	}
}

// boundedContext limits a wait step to five seconds: a mutant that never
// becomes ready then fails in seconds, not after the wait's own bound.
func boundedContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestWaitBindingReconciledFailsWithoutAGrammar asserts the reconciled
// wait's ready function fails naming the missing grammar when the
// detection step stored none or an unknown one, and the step proceeds on
// the stored one.
func TestWaitBindingReconciledFailsWithoutAGrammar(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"prtb", "crtb"} {
		ready := waitBindingReconciled(kind)
		obj := reconciledObject(kind, map[string]any{}, mustAnnotations(kind), "local://u-x", "2026-10-21T00:00:00Z")
		for name, captured := range map[string]map[string]string{
			"none":    {"downstream": testDownstream},
			"unknown": {"downstream": testDownstream, grammarKey: "bogus"},
		} {
			done, err := ready(obj, http.StatusOK, captured)
			require.ErrorContains(t, err, "binding grammar", "%s %s", kind, name)
			assert.False(t, done)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"labels":{},"annotations":{"lifecycle.cattle.io/create.mgmt-auth-prtb-controller":"true","lifecycle.cattle.io/create.cluster-prtb-sync_c-m-test":"true"}},"userPrincipalName":"local://u-x"}`))
	}))
	defer srv.Close()
	wait := stepByName(t, explainSetupSteps(), "prtb reconciled")

	err := wait.Do(boundedContext(t), adminTarget(srv), map[string]string{"downstream": testDownstream})
	require.ErrorContains(t, err, "binding grammar")
	err = wait.Do(boundedContext(t), adminTarget(srv), map[string]string{"downstream": testDownstream, grammarKey: "bogus"})
	require.ErrorContains(t, err, "binding grammar")
	require.NoError(t, wait.Do(boundedContext(t), adminTarget(srv), map[string]string{"downstream": testDownstream, grammarKey: "aggregation"}))
}

func mustAnnotations(kind string) map[string]any {
	_, annotations := reconciledParts(kind)
	return annotations
}

// TestWaitBindingReconciledUsesTheStoredGrammar asserts the ready function
// of the reconciled waits applies the grammar the detection step stored
// and the kind it was built for: in the classic grammar an object without
// the two labels-updated labels is not reconciled and the same object
// with them is.
func TestWaitBindingReconciledUsesTheStoredGrammar(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"prtb", "crtb"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ready := waitBindingReconciled(kind)
			classic := map[string]string{grammarKey: string(grammarClassic)}
			aggregation := map[string]string{grammarKey: string(grammarAggregation)}

			labels, annotations := reconciledParts(kind)
			clear(labels)
			without := reconciledObject(kind, labels, annotations, "local://u-x", "2026-10-21T00:00:00Z")
			done, err := ready(without, http.StatusOK, classic)
			require.NoError(t, err)
			assert.False(t, done, "classic needs the labels-updated labels")
			done, err = ready(without, http.StatusOK, aggregation)
			require.NoError(t, err)
			assert.True(t, done, "aggregation does not")

			labels, annotations = reconciledParts(kind)
			with := reconciledObject(kind, labels, annotations, "local://u-x", "2026-10-21T00:00:00Z")
			done, err = ready(with, http.StatusOK, classic)
			require.NoError(t, err)
			assert.True(t, done, "classic with both labels")
		})
	}
}

// TestExplainSetupDetectsTheGrammarFirst asserts both explain probes begin
// with the detection step, which stores a grammar, and wait for the
// bindings only after it.
func TestExplainSetupDetectsTheGrammarFirst(t *testing.T) {
	t.Parallel()
	srv := featureServer(http.StatusOK, `{"status":{"default":true}}`)
	t.Cleanup(srv.Close)
	for _, name := range []string{"access-downstream/explain-fixture-chain", "access-downstream/explain-fixture-reason-grammar"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := probeByName(t, name)
			require.NotEmpty(t, p.Setup)
			captured := map[string]string{"downstream": testDownstream}
			require.NoError(t, p.Setup[0].Do(t.Context(), adminTarget(srv), captured))
			assert.Equal(t, "aggregation", captured[grammarKey], "Setup[0] detects the grammar")
			for _, wait := range []string{"prtb reconciled", "crtb reconciled"} {
				idx := slices.IndexFunc(p.Setup, func(s Step) bool { return s.Name == wait })
				assert.Greater(t, idx, 0, "%s waits after the detection step", wait)
			}
		})
	}
}

type selectorRequest struct{ path, selector string }

// selectorServer answers the feature as the given body and records every
// request after it.
func selectorServer(t *testing.T, featureBody string) (*httptest.Server, func() (features int, auth []string, lists []selectorRequest)) {
	t.Helper()
	var mu sync.Mutex
	var featureCalls int
	var featureAuth []string
	var lists []selectorRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == aggregatedRoleTemplatesFeaturePath {
			featureCalls++
			featureAuth = append(featureAuth, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(featureBody))
			return
		}
		lists = append(lists, selectorRequest{r.URL.Path, r.URL.Query().Get("labelSelector")})
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() (int, []string, []selectorRequest) {
		mu.Lock()
		defer mu.Unlock()
		return featureCalls, slices.Clone(featureAuth), slices.Clone(lists)
	}
}

// TestExplainObservationsSendTheSelectorsOfTheDetectedGrammar asserts the
// three binding-list observations of the explain fixture chain send the
// exact label selector of the grammar the detection step found, that a
// run returns only the observations (never the setup result), and that
// the detection request goes out once with the admin bearer.
func TestExplainObservationsSendTheSelectorsOfTheDetectedGrammar(t *testing.T) {
	t.Parallel()
	const (
		rbPath  = "/k8s/clusters/c-m-test/apis/rbac.authorization.k8s.io/v1/namespaces/e2e-fid-explain/rolebindings"
		crbPath = "/k8s/clusters/c-m-test/apis/rbac.authorization.k8s.io/v1/clusterrolebindings"
		agg     = "management.cattle.io/roletemplate-aggregation=true"
	)
	tests := map[string]struct {
		feature string
		want    []selectorRequest
	}{
		"classic": {`{"spec":{"value":null},"status":{"default":false}}`, []selectorRequest{
			{rbPath, "authz.cluster.cattle.io/rtb-owner-updated=c-m-test-p-e2e-fid-explain_e2e-fid-explain-prtb"},
			{crbPath, "c-m-test-p-e2e-fid-explain_e2e-fid-explain-prtb=owner-user"},
			{crbPath, "authz.cluster.cattle.io/rtb-owner-updated=c-m-test_e2e-fid-explain-crtb"},
		}},
		"aggregation": {`{"spec":{"value":null},"status":{"default":true}}`, []selectorRequest{
			{rbPath, "authz.cluster.cattle.io/prtb-owner-e2e-fid-explain-prtb=true," + agg},
			{crbPath, "authz.cluster.cattle.io/prtb-owner-e2e-fid-explain-prtb=true," + agg},
			{crbPath, "authz.cluster.cattle.io/crtb-owner-e2e-fid-explain-crtb=true," + agg},
		}},
	}
	names := []string{"prtb-rolebindings", "prtb-clusterrolebindings", "crtb-clusterrolebindings"}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := probeByName(t, "access-downstream/explain-fixture-chain")
			var observations []Observation
			for _, n := range names {
				idx := slices.IndexFunc(src.Observations, func(o Observation) bool { return o.Name == n })
				require.GreaterOrEqual(t, idx, 0, n)
				observations = append(observations, src.Observations[idx])
			}
			p := Probe{
				Name: "fam/selectors", Downstream: true, SubstituteCaptures: true,
				Setup:        []Step{explainSetupSteps()[0]},
				Observations: observations,
			}
			srv, seen := selectorServer(t, tc.feature)
			results, err := RunProbe(t.Context(), adminTarget(srv), p)
			require.NoError(t, err)

			got := make([]string, len(results))
			for i, r := range results {
				got[i] = r.Name
			}
			assert.Equal(t, names, got, "a run returns the observations only")
			features, auth, lists := seen()
			assert.Equal(t, 1, features)
			assert.Equal(t, []string{"Bearer admin-tok"}, auth)
			assert.Equal(t, tc.want, lists)
		})
	}
}

// TestRunProbeKeepsUnrecordedCapturesVerbatim asserts a value stored under
// the unrecorded prefix stays as it is in recorded JSON values, keys and
// text while the other captures still become placeholders.
func TestRunProbeKeepsUnrecordedCapturesVerbatim(t *testing.T) {
	t.Parallel()
	const selector = "authz.cluster.cattle.io/prtb-owner-e2e-fid-explain-prtb=true,management.cattle.io/roletemplate-aggregation=true"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/text" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(selector + " on c-m-test"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"selector":%q,"%s":"c-m-test","key":"management.cattle.io/roletemplate-aggregation"}`, selector, selector)
	}))
	defer srv.Close()
	store := Step{Name: "store", Do: func(_ context.Context, _ Target, captured map[string]string) error {
		captured[unrecordedPrefix+"selector"] = selector
		return nil
	}}
	p := Probe{
		Name: "fam/unrecorded", Downstream: true, SubstituteCaptures: true,
		Setup: []Step{store},
		Observations: []Observation{
			{Name: "json", Request: RequestSpec{Method: http.MethodGet, Path: "/json", Identity: IdentityAdmin}},
			{Name: "text", Request: RequestSpec{Method: http.MethodGet, Path: "/text", Identity: IdentityAdmin}},
		},
	}
	results, err := RunProbe(t.Context(), adminTarget(srv), p)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, selector, results[0].Body["selector"], "a stored selector is never replaced by its placeholder in a value")
	assert.Equal(t, "{downstream}", results[0].Body[selector], "a stored selector is never replaced in a key, and the other captures still are")
	assert.Equal(t, "management.cattle.io/roletemplate-aggregation", results[0].Body["key"])
	assert.Equal(t, selector+" on {downstream}", results[1].Text, "a stored selector is never replaced in text")
}

// TestSubstituteCapturedSkipsTheUnrecordedPrefix asserts both substitution
// functions leave unrecorded captures alone when called directly.
func TestSubstituteCapturedSkipsTheUnrecordedPrefix(t *testing.T) {
	t.Parallel()
	captured := map[string]string{"downstream": "c-m-test", unrecordedPrefix + "sel": "owner=c-m-test-longer-than-downstream"}
	body := map[string]any{"a": "owner=c-m-test-longer-than-downstream", "owner=c-m-test-longer-than-downstream": "c-m-test"}
	substituteCaptured(body, captured)
	assert.Equal(t, map[string]any{"a": "owner={downstream}-longer-than-downstream", "owner={downstream}-longer-than-downstream": "{downstream}"}, body)
	assert.Equal(t, "owner={downstream}-longer-than-downstream", substituteCapturedString("owner=c-m-test-longer-than-downstream", captured))
}

// TestOwnerLabelKeyMatchesSafeConcatName asserts the probe's label key is
// the key the CLI's copy of Rancher's rule computes for the two fixture
// names and for long names that take each shortening branch.
func TestOwnerLabelKeyMatchesSafeConcatName(t *testing.T) {
	t.Parallel()
	const longAlnum = "e2e-fid-explain-binding-with-a-name-long-enough-to-shorten"
	const longDash = "e2e-fid-explain-binds-with-a-long-name-for-the-dash-branch"
	require.GreaterOrEqual(t, len("authz.cluster.cattle.io/prtb-owner-"+longAlnum), 64)
	require.GreaterOrEqual(t, len("authz.cluster.cattle.io/prtb-owner-"+longDash), 64)
	for _, kind := range []string{"prtb", "crtb"} {
		prefix := "authz.cluster.cattle.io/" + kind + "-owner"
		for _, name := range []string{explainPRTBName, explainCRTBName, longAlnum, longDash} {
			assert.Equal(t, rbac.SafeConcatName(prefix, name), ownerLabelKey(kind, name), "%s %s", kind, name)
		}
		alnum := ownerLabelKey(kind, longAlnum)
		assert.Len(t, alnum, 63)
		assert.Equal(t, byte('-'), alnum[57], "the cut keeps 57 characters")
		dash := ownerLabelKey(kind, longDash)
		assert.Len(t, dash, 63)
		assert.Equal(t, byte('-'), dash[56], "a cut that would end on a dash keeps 56 characters")
	}
	assert.Equal(t, "authz.cluster.cattle.io/prtb-owner-e2e-fid-explain-prtb", ownerLabelKey("prtb", explainPRTBName))
}

// TestOwnerLabelKeyLengthBoundary asserts the key is the plain join at 63
// characters and shortened at 64, the same as rbac.SafeConcatName, for
// both kinds.
func TestOwnerLabelKeyLengthBoundary(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"prtb", "crtb"} {
		prefix := "authz.cluster.cattle.io/" + kind + "-owner"
		for length, name := range map[int]string{63: strings.Repeat("a", 28), 64: strings.Repeat("a", 29)} {
			join := prefix + "-" + name
			require.Len(t, join, length, "%s join", kind)
			want := rbac.SafeConcatName(prefix, name)
			assert.Equal(t, want, ownerLabelKey(kind, name), "%s %d", kind, length)
			if length == 63 {
				assert.Equal(t, join, want, "63 characters are kept whole")
			} else {
				assert.NotEqual(t, join, want, "64 characters are shortened")
			}
		}
	}
}

// TestValidateRefusesAReservedCaptureName asserts an observation capture
// named under the unrecorded prefix is refused, naming the probe, the
// observation and the capture, while a setup step may store such a value.
func TestValidateRefusesAReservedCaptureName(t *testing.T) {
	t.Parallel()
	p := Probe{
		Name: "fam/reserved",
		Observations: []Observation{{
			Name:     "read",
			Request:  RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityAdmin},
			Captures: map[string]Capture{unrecordedPrefix + "selector": {Path: "metadata.name"}},
		}},
	}
	err := p.Validate()
	require.ErrorContains(t, err, `probe fam/reserved: observation "read": capture "unrecorded.selector"`)
	require.ErrorContains(t, err, "reserved for setup values that are never recorded")

	p.Observations[0].Captures = map[string]Capture{"selector": {Path: "metadata.name"}}
	p.Setup = []Step{detectBindingGrammarStep()}
	require.NoError(t, p.Validate())
}

type recordedRequest struct{ method, path, body string }

// TestExplainBindingRequestsAreUnchanged asserts every request the explain
// probes send for the PRTB and the CRTB (their creation, the reconciled
// waits, the teardown deletes and the two object observations) has the
// method, path and body below, typed in full.
func TestExplainBindingRequestsAreUnchanged(t *testing.T) {
	t.Parallel()
	const (
		mgmt        = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/"
		prtbPath    = mgmt + "c-m-test-p-e2e-fid-explain/projectroletemplatebindings"
		crtbPath    = mgmt + "c-m-test/clusterroletemplatebindings"
		prtbBodyLit = `{"apiVersion":"management.cattle.io/v3","kind":"ProjectRoleTemplateBinding","metadata":{"name":"e2e-fid-explain-prtb","namespace":"c-m-test-p-e2e-fid-explain"},"userName":"e2e-fid-explain-user","roleTemplateName":"project-member","projectName":"c-m-test:p-e2e-fid-explain"}`
		crtbBodyLit = `{"apiVersion":"management.cattle.io/v3","kind":"ClusterRoleTemplateBinding","metadata":{"name":"e2e-fid-explain-crtb","namespace":"c-m-test"},"userName":"e2e-fid-explain-user","roleTemplateName":"cluster-member","clusterName":"c-m-test"}`
	)
	var mu sync.Mutex
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, recordedRequest{r.Method, r.URL.Path, string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"metadata":{"labels":{},"annotations":{"lifecycle.cattle.io/create.mgmt-auth-prtb-controller":"true","lifecycle.cattle.io/create.cluster-prtb-sync_c-m-test":"true","lifecycle.cattle.io/create.mgmt-auth-crtb-controller":"true","lifecycle.cattle.io/create.cluster-crtb-sync_c-m-test":"true"}},"userPrincipalName":"local://u-x","status":{"lastUpdateTime":"2026-10-21T00:00:00Z"}}`))
		}
	}))
	defer srv.Close()
	tgt := adminTarget(srv)
	captured := map[string]string{"downstream": testDownstream, grammarKey: string(grammarAggregation)}

	setup := explainSetupSteps()
	for _, name := range []string{"prtb", "crtb", "prtb reconciled", "crtb reconciled"} {
		require.NoError(t, stepByName(t, setup, name).Do(boundedContext(t), tgt, captured), name)
	}
	chain := probeByName(t, "access-downstream/explain-fixture-chain")
	for _, name := range []string{"crtb", "prtb"} {
		require.NoError(t, stepByName(t, chain.Teardown, name).Do(boundedContext(t), tgt, captured), name)
	}
	reasons := probeByName(t, "access-downstream/explain-fixture-reason-grammar")
	for _, name := range []string{"crtb", "prtb"} {
		require.NoError(t, stepByName(t, reasons.Teardown, name).Do(boundedContext(t), tgt, captured), name)
	}
	var objects []Observation
	for _, name := range []string{"prtb-object", "crtb-object"} {
		idx := slices.IndexFunc(chain.Observations, func(o Observation) bool { return o.Name == name })
		require.GreaterOrEqual(t, idx, 0, name)
		objects = append(objects, chain.Observations[idx])
	}
	_, err := RunProbe(boundedContext(t), tgt, Probe{Name: "fam/objects", Downstream: true, Observations: objects})
	require.NoError(t, err)

	want := []recordedRequest{
		{http.MethodPost, prtbPath, prtbBodyLit},
		{http.MethodPost, crtbPath, crtbBodyLit},
		{http.MethodGet, prtbPath + "/e2e-fid-explain-prtb", ""},
		{http.MethodGet, crtbPath + "/e2e-fid-explain-crtb", ""},
		{http.MethodDelete, crtbPath + "/e2e-fid-explain-crtb", ""},
		{http.MethodDelete, prtbPath + "/e2e-fid-explain-prtb", ""},
		{http.MethodDelete, crtbPath + "/e2e-fid-explain-crtb", ""},
		{http.MethodDelete, prtbPath + "/e2e-fid-explain-prtb", ""},
		{http.MethodGet, "/v1/management.cattle.io.projectroletemplatebindings/c-m-test-p-e2e-fid-explain/e2e-fid-explain-prtb", ""},
		{http.MethodGet, "/v1/management.cattle.io.clusterroletemplatebindings/c-m-test/e2e-fid-explain-crtb", ""},
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, want, got)
}

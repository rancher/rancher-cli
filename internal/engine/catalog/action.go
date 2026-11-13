package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AppType is the steve type of an App.
const AppType = "catalog.cattle.io.apps"

// The annotations an install or upgrade sends in its chart entry. The two source
// annotations are the ones the Rancher UI sends; with them an uninstall
// finds the ClusterRepo whose service account runs Helm. The request id
// marks the App revision this request created: Rancher copies a chart
// entry's annotations into the chart's metadata, which the App shows at
// spec.chart.metadata.annotations.
const (
	annotationRequestID      = "cli.rancher.io/request-id"
	annotationSourceRepoType = "catalog.cattle.io/ui-source-repo-type"
	annotationSourceRepo     = "catalog.cattle.io/ui-source-repo"
	sourceRepoTypeCluster    = "cluster"
)

// installBody is the request body of the install action. Its JSON names
// are the fields of Rancher's install action types; every field sent here
// exists on every Rancher minor the CLI supports.
type installBody struct {
	Timeout                  string         `json:"timeout"`
	Wait                     bool           `json:"wait"`
	NoHooks                  bool           `json:"noHooks,omitempty"`
	DisableOpenAPIValidation bool           `json:"disableOpenAPIValidation,omitempty"`
	Namespace                string         `json:"namespace"`
	Charts                   []installChart `json:"charts"`
}

// installChart is one chart entry of the install body.
type installChart struct {
	ChartName   string            `json:"chartName"`
	Version     string            `json:"version"`
	ReleaseName string            `json:"releaseName"`
	Description string            `json:"description,omitempty"`
	Values      *requestValues    `json:"values,omitempty"`
	Annotations map[string]string `json:"annotations"`
}

// upgradeBody is the request body of the upgrade action. Its JSON names are
// the fields of Rancher's upgrade action types; every field sent here exists
// on every Rancher minor the CLI supports. It never carries install, which
// Rancher always sets, and carries every other option only when it was
// typed.
type upgradeBody struct {
	Timeout                  string         `json:"timeout"`
	Wait                     bool           `json:"wait"`
	NoHooks                  bool           `json:"noHooks,omitempty"`
	DisableOpenAPIValidation bool           `json:"disableOpenAPIValidation,omitempty"`
	Force                    bool           `json:"force,omitempty"`
	TakeOwnership            bool           `json:"takeOwnership,omitempty"`
	HistoryMax               int            `json:"historyMax,omitempty"`
	CleanupOnFail            bool           `json:"cleanupOnFail,omitempty"`
	Namespace                string         `json:"namespace"`
	Charts                   []upgradeChart `json:"charts"`
}

// upgradeChart is one chart entry of the upgrade body.
type upgradeChart struct {
	ChartName   string            `json:"chartName"`
	Version     string            `json:"version"`
	ReleaseName string            `json:"releaseName"`
	ResetValues bool              `json:"resetValues,omitempty"`
	Description string            `json:"description,omitempty"`
	Values      *requestValues    `json:"values,omitempty"`
	Annotations map[string]string `json:"annotations"`
}

// uninstallBody is the request body of the uninstall action. It never
// carries the action's dryRun option.
type uninstallBody struct {
	NoHooks     bool   `json:"noHooks,omitempty"`
	KeepHistory bool   `json:"keepHistory,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
}

// actionAnswer is the part of Rancher's 201 answer the commands read.
type actionAnswer struct {
	OperationName      string `json:"operationName"`
	OperationNamespace string `json:"operationNamespace"`
}

// installPath is the install action of the ClusterRepo repo on cluster, a
// cluster id.
func installPath(cluster, repo string) string {
	return actionRequest(cluster, ClusterRepoType, "", repo).Path() + "?action=install"
}

// upgradePath is the upgrade action of the ClusterRepo repo on cluster, a
// cluster id.
func upgradePath(cluster, repo string) string {
	return actionRequest(cluster, ClusterRepoType, "", repo).Path() + "?action=upgrade"
}

// uninstallPath is the uninstall action of the App ns/name on cluster.
func uninstallPath(cluster, ns, name string) string {
	return actionRequest(cluster, AppType, ns, name).Path() + "?action=uninstall"
}

func actionRequest(cluster, typ, ns, name string) steve.Request {
	req := steve.Request{Type: typ, Namespace: ns, Name: name}
	if cluster != engine.LocalCluster {
		req.Cluster = cluster
	}
	return req
}

// actionTarget is what the follow-up commands and the outcome-unknown error
// name: the cluster as typed, the release and its namespace, and for an
// install the request id.
type actionTarget struct {
	typedCluster string
	namespace    string
	release      string
	requestID    string
}

// getApp is the command that shows the App of the release.
func (t actionTarget) getApp() clierr.Command {
	return clierr.NewCommand("get app").Arg(t.release).FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace)
}

// getOperations is the command that lists the operations of the namespace.
func (t actionTarget) getOperations() clierr.Command {
	return clierr.NewCommand("get operations").FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace)
}

// postAction sends body once through the action port and decodes Rancher's
// answer. An answer that does not tell whether Rancher ran the action is
// GENERIC and not retryable, naming what to read before any rerun; every
// other error keeps its code, with the server's text passed through the
// values filter.
func postAction(ctx context.Context, actions engine.ActionClientFactory, path string, body any, t actionTarget, values Values) (actionAnswer, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return actionAnswer{}, err
	}
	client, err := actions.Action()
	if err != nil {
		return actionAnswer{}, err
	}
	answer, err := client.PostAction(ctx, path, raw)
	if err != nil {
		if _, ok := errors.AsType[*engine.ActionOutcomeUnknown](err); ok {
			return actionAnswer{}, outcomeUnknown(t, err, values)
		}
		if r, ok := errors.AsType[*engine.ActionRefused](err); ok {
			return actionAnswer{}, refused(path, r, values)
		}
		return actionAnswer{}, values.FilterError(err)
	}
	var a actionAnswer
	if err := json.Unmarshal(answer, &a); err != nil || a.OperationName == "" || a.OperationNamespace == "" {
		return actionAnswer{}, clierr.New(clierr.CodeGeneric,
			clierr.Text("Rancher accepted the request but its answer names no operation; list the operations of the namespace, then read the app",
				clierr.Lead(": "), t.getApp())).
			WithHintCommand(t.getOperations())
	}
	return a, nil
}

// refused is the error for an action the server refused: the answer
// classified again by clierr's status table, as the action client did,
// from a body whose server text has the values hidden, with the server's
// text added where the table's message does not hold it. The table's code,
// retry advice, hint and wording stay; the filter runs on the whole text
// before the table cuts it, so no part of a value is left, and a
// Kubernetes Status keeps its whole message. The hint is a constant of the
// table and is never filtered.
func refused(path string, r *engine.ActionRefused, values Values) error {
	body := filteredBody(r.Body, values.filterFunc())
	said := serverText(body)
	status, ok := clierr.HTTPStatus(r.Err)
	if !ok {
		// r.Err's message holds the server's text as sent and is never read
		// here.
		src := clierr.From(r.Err)
		msg := "POST " + path + " on the Rancher server was refused"
		if said != "" {
			msg += ": " + said
		}
		return clierr.New(src.Code, msg).WithRetryable(src.Retryable)
	}
	prefix := "POST " + path + " on the Rancher server"
	ce := clierr.FromResponse(prefix, http.MethodPost, status, body, r.Request)
	// The server text is looked for after the prefix only, so a short text
	// that the path holds, such as the action name, is still shown.
	if said != "" && !strings.EqualFold(said, http.StatusText(status)) && !strings.Contains(strings.TrimPrefix(ce.Message, prefix), strings.TrimSuffix(said, "…")) {
		ce = ce.WithMessage(ce.Message + ": " + said)
	}
	return ce
}

// serverText is the server's message in a body whose values are already
// hidden. It runs no second filter, which would replace a value that is
// part of <hidden> inside the marker.
func serverText(filtered []byte) string {
	return clierr.FilteredServerMessage(filtered, func(s string) string { return s })
}

// filteredBody is body with the values hidden in its server text: in a
// Kubernetes Status, its message, details.name and each cause's message and
// field; in any other JSON object, every string; or the whole of any other
// body. A Status's kind, apiVersion, status, reason and code stay as sent,
// so it is still read as that Status.
func filteredBody(body []byte, filter func(string) string) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return []byte(filter(string(body)))
	}
	var st metav1.Status
	if json.Unmarshal(body, &st) == nil && st.Kind == "Status" {
		filterStatus(obj, filter)
	} else {
		filterStrings(obj, filter)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return []byte(filter(string(body)))
	}
	return out
}

// filterStatus runs filter on the text fields of a Status decoded into obj.
// Keys match without regard to case, as encoding/json reads them.
func filterStatus(obj map[string]any, filter func(string) string) {
	filterField(obj, "message", filter)
	for _, details := range fields(obj, "details") {
		d, ok := details.(map[string]any)
		if !ok {
			continue
		}
		filterField(d, "name", filter)
		for _, causes := range fields(d, "causes") {
			list, ok := causes.([]any)
			if !ok {
				continue
			}
			for _, cause := range list {
				if c, ok := cause.(map[string]any); ok {
					filterField(c, "message", filter)
					filterField(c, "field", filter)
				}
			}
		}
	}
}

// fields returns the values of obj's keys that equal name without regard to
// case.
func fields(obj map[string]any, name string) []any {
	var out []any
	for k, v := range obj {
		if strings.EqualFold(k, name) {
			out = append(out, v)
		}
	}
	return out
}

// filterField runs filter on each string under a key of obj that equals name
// without regard to case.
func filterField(obj map[string]any, name string, filter func(string) string) {
	for k, v := range obj {
		if s, ok := v.(string); ok && strings.EqualFold(k, name) {
			obj[k] = filter(s)
		}
	}
}

// filterStrings returns v with filter run on every string in it.
func filterStrings(v any, filter func(string) string) any {
	switch t := v.(type) {
	case string:
		return filter(t)
	case map[string]any:
		for k, e := range t {
			t[k] = filterStrings(e, filter)
		}
	case []any:
		for i, e := range t {
			t[i] = filterStrings(e, filter)
		}
	}
	return v
}

// outcomeUnknown is the error for an action request that was sent but whose
// answer did not say whether Rancher ran it. It is not retryable: a second
// request would run Helm a second time.
func outcomeUnknown(t actionTarget, err error, values Values) error {
	cause := outcomeCause(err)
	if u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err); ok {
		if said := clierr.FilteredServerMessage(u.Body, values.Filter); said != "" {
			cause += ": " + said
		}
	}
	parts := []any{fmt.Sprintf("the request for release %s was sent, but the answer did not say whether Rancher ran it (%s); "+
		"before you run the command again, list the operations of the namespace and read the app",
		clierr.ShellQuote(t.release), cause), clierr.Lead(": "), t.getApp()}
	if t.requestID != "" {
		parts = append(parts, fmt.Sprintf("; this request's id is %s, and an app whose spec.chart.metadata.annotations hold %s: %s is the revision this request created",
			t.requestID, annotationRequestID, t.requestID))
	}
	return clierr.New(clierr.CodeGeneric, clierr.Text(parts...)).WithRetryable(false).
		WithHintCommand(t.getOperations())
}

// outcomeCause words the transport failure without the URL, which names
// nothing the reader needs.
func outcomeCause(err error) string {
	if status, ok := clierr.HTTPStatus(err); ok {
		return fmt.Sprintf("the server answered %d", status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "the request was cancelled"
	}
	return "the connection failed"
}

// Helm timeout bounds. The upper bound is the CLI's own: Rancher removes a
// run's access one hour after the operation starts, and the chart's hooks
// can make a run longer than the timeout.
const (
	minHelmTimeout     = time.Second
	maxHelmTimeout     = 15 * time.Minute
	defaultHelmTimeout = 10 * time.Minute
	flagHelmTimeout    = "helm-timeout"
	helmTimeoutBounds  = "--helm-timeout must be at least 1s and at most 15m"
)

// helmTimeout reads --helm-timeout: def when omitted ("" sends none), a Go
// duration between the bounds otherwise.
func helmTimeout(raw string, def time.Duration) (string, error) {
	if raw == "" {
		if def == 0 {
			return "", nil
		}
		return def.String(), nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--helm-timeout %s is not a duration such as 90s or 10m", clierr.ShellQuote(raw)))
	}
	switch {
	case d < minHelmTimeout:
		return "", clierr.New(clierr.CodeValidation, helmTimeoutBounds)
	case d > maxHelmTimeout:
		return "", clierr.New(clierr.CodeValidation, helmTimeoutBounds+
			"; the chart's hooks can make a run longer, and Rancher removes a run's access one hour after the operation starts")
	}
	return d.String(), nil
}

// trimmedList splits an annotation that is a comma list, with the spaces
// around each entry cut and empty entries dropped, as the Rancher UI reads
// catalog.cattle.io/requires-gvr and catalog.cattle.io/auto-install.
func trimmedList(v string) []string {
	var out []string
	for e := range strings.SplitSeq(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

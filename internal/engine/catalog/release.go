package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// secretType is the steve collection of Secrets.
const secretType = "secrets"

// releaseRef names the release whose values an upgrade sends: the App Plan
// read and the Helm release Secret its owner reference names.
type releaseRef struct {
	target actionTarget
	// name and namespace are the App's spec.name and the namespace of the
	// App; revision is its spec.version.
	name      string
	namespace string
	revision  int64
	// secret is the name of the release Secret, "" when the App has none.
	secret string
}

// storedRelease is what the read of a release Secret keeps: the user values
// of that revision and the revision's number and status. The chart, the
// manifest and the hooks of the release are never decoded.
type storedRelease struct {
	secret   string
	revision int64
	status   string
	values   Values
}

// releaseFailure is a read of the release's values that gave none. err is the
// error the upgrade ends with; reason says what happened in a clause that
// names no remedy, for the warning an upgrade with --reset-values shows
// instead. hard marks a failure that stays an error with --reset-values:
// the request itself failed in a way that says nothing about the release.
type releaseFailure struct {
	reason string
	err    *clierr.Error
	hard   bool
}

// ownerSecret is the name of the Secret of kind Secret among the App's
// owner references, the release Secret of the revision the App shows.
func ownerSecret(app *unstructured.Unstructured) string {
	for _, ref := range app.GetOwnerReferences() {
		if ref.Kind == "Secret" {
			return ref.Name
		}
	}
	return ""
}

// releaseSecret is the part of a Secret that steve returns the read keeps.
type releaseSecret struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Data struct {
		Release json.RawMessage `json:"release"`
	} `json:"data"`
}

// helmRelease is the part of the release that steve decodes the read keeps.
type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int64  `json:"version"`
	Info      struct {
		Status string `json:"status"`
	} `json:"info"`
	Config map[string]any `json:"config"`
}

// releaseReadHelp is the sentence of the app upgrade help that says how the
// release's values are read, with the name of the query the read sends.
func releaseReadHelp() string {
	return "The command reads them from the release Secret through the Steve API with includeHelmData=true. "
}

// readReleaseValues reads the release's values of the revision the App shows,
// from the release Secret the App's owner reference names, through steve
// with includeHelmData=true, which makes steve decode the release. Without
// that query steve drops the release data. It is the one read of Secret data
// of any app command: it needs get on Secrets in the namespace, and it keeps
// only the release's config.
func readReleaseValues(ctx context.Context, root engine.RootClient, cluster string, ref releaseRef) (storedRelease, *releaseFailure) {
	t := ref.target
	if ref.secret == "" {
		return storedRelease{}, &releaseFailure{
			reason: "the app has no owner reference to a release Secret",
			err: clierr.New(clierr.CodeGeneric, fmt.Sprintf("app %s has no owner reference to a release Secret and the release's values cannot be read; add --reset-values and a --values file that holds every value the release needs",
				clierr.ShellQuote(t.release))).WithHintCommand(t.getApp()),
		}
	}
	req := steve.Request{Type: secretType, Namespace: ref.namespace, Name: ref.secret}
	if cluster != engine.LocalCluster {
		req.Cluster = cluster
	}
	body, err := root.GetJSON(ctx, req.Path()+"?includeHelmData=true")
	switch {
	case err != nil && forbidden(err):
		return storedRelease{}, &releaseFailure{
			reason: fmt.Sprintf("you cannot get Secret %s in namespace %s", clierr.ShellQuote(ref.secret), clierr.ShellQuote(ref.namespace)),
			err: clierr.New(clierr.CodeRBAC, fmt.Sprintf(
				"you cannot get Secret %s in namespace %s, where Helm keeps the values of release %s; the upgrade sends these values and has to read them first; "+
					"ask for permission to get Secrets in the namespace, or add --reset-values and a --values file that holds every value the release needs",
				clierr.ShellQuote(ref.secret), clierr.ShellQuote(ref.namespace), clierr.ShellQuote(t.release))),
		}
	case err != nil && notFound(err):
		return storedRelease{}, ref.moved("Secret " + clierr.ShellQuote(ref.secret) + " was not found")
	case err != nil:
		return storedRelease{}, &releaseFailure{reason: "the read failed", err: clierr.From(err), hard: true}
	}

	var sec releaseSecret
	if err := json.Unmarshal(body, &sec); err != nil || sec.Metadata.Labels["owner"] != "helm" || !isJSONObject(sec.Data.Release) {
		return storedRelease{}, &releaseFailure{
			reason: "Rancher did not return the release's values",
			err: clierr.New(clierr.CodeGeneric, fmt.Sprintf("Rancher did not return the values of release %s; add --reset-values and a --values file that holds every value the release needs",
				clierr.ShellQuote(t.release))).WithHintCommand(t.getApp()),
		}
	}
	var rel helmRelease
	if err := json.Unmarshal(sec.Data.Release, &rel); err != nil {
		return storedRelease{}, &releaseFailure{
			reason: "Rancher did not return the release's values",
			err: clierr.New(clierr.CodeGeneric, fmt.Sprintf("Rancher did not return the values of release %s; add --reset-values and a --values file that holds every value the release needs",
				clierr.ShellQuote(t.release))).WithHintCommand(t.getApp()),
		}
	}
	labels := sec.Metadata.Labels
	if rel.Name != ref.name || rel.Namespace != ref.namespace || rel.Version != ref.revision ||
		(labels["name"] != "" && labels["name"] != ref.name) || (labels["version"] != "" && labels["version"] != fmt.Sprint(ref.revision)) {
		return storedRelease{}, ref.moved("Secret " + clierr.ShellQuote(ref.secret) + " holds another release or revision")
	}
	cfg := rel.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	return storedRelease{secret: ref.secret, revision: rel.Version, status: rel.Info.Status, values: Values{tree: cfg}}, nil
}

// moved is the failure when the release is not the one the App named: Rancher
// writes the App apart from Helm, so the App may be a revision behind. Run
// again reads it afresh.
func (r releaseRef) moved(reason string) *releaseFailure {
	return &releaseFailure{
		reason: reason,
		err: clierr.New(clierr.CodeConflict, "app "+clierr.ShellQuote(r.target.release)+" changed while it was read; run the command again").
			WithRetryable(true),
	}
}

// isJSONObject reports whether raw is a JSON object: steve returns the
// release decoded as one, and leaves it as an encoded string when it cannot
// decode it.
func isJSONObject(raw json.RawMessage) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{"))
}

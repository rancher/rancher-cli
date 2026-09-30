// crtobject.go is the policy-neutral CRT boundary: the flat
// decoded view of a Steve clusterregistrationtoken object, the typed-error
// decoders, the placeholder REPORTER (which names placeholder-bearing
// protected fields and decides nothing), and the narrow default-token
// fetch that backs inspect cluster's registration section. The policies
// live solely in the callers: named get rejects (crt.go), list get blanks
// and warns (crt.go), inspect cluster omits and warns (inspect/cluster.go).
package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// CRTObject is the policy-neutral decoded view of one Steve
// clusterregistrationtoken object: identity plus the seven formatter-
// protected command/manifest fields and the token/expiry. Values are
// carried exactly as decoded - including a still-unformatted "{token}"
// placeholder - because deciding what to do about a placeholder is the
// caller's job, not the decoder's.
type CRTObject struct {
	Namespace   string
	Name        string
	ClusterName string
	// Labels are metadata.labels, kept so a list can be filtered by
	// --selector locally (this reader never assumes Steve's query grammar).
	Labels map[string]string

	Command                    string
	InsecureCommand            string
	NodeCommand                string
	InsecureNodeCommand        string
	WindowsNodeCommand         string
	InsecureWindowsNodeCommand string
	ManifestURL                string
	Token                      string
	ExpiresAt                  string
}

// guardedFieldPtrs returns pointers into the seven command/manifest fields
// the Steve formatter's ReplaceAll protects, paired with their JSON field
// name - the single definition PlaceholderFields (report) and
// clearPlaceholderFields (blank) enumerate from, so the guarded-field set
// can never drift between the callers. Token and ExpiresAt are
// deliberately excluded: they are not template strings the formatter
// substitutes into - Token IS the substitution source.
func (o *CRTObject) guardedFieldPtrs() []struct {
	name string
	val  *string
} {
	return []struct {
		name string
		val  *string
	}{
		{"command", &o.Command},
		{"insecureCommand", &o.InsecureCommand},
		{"nodeCommand", &o.NodeCommand},
		{"insecureNodeCommand", &o.InsecureNodeCommand},
		{"windowsNodeCommand", &o.WindowsNodeCommand},
		{"insecureWindowsNodeCommand", &o.InsecureWindowsNodeCommand},
		{"manifestUrl", &o.ManifestURL},
	}
}

// PlaceholderFields reports the JSON field names of o's protected fields
// still carrying the literal "{token}" placeholder (the k8s proxy's own
// unformatted value - the server never spliced in the real token).
// Policy-neutral by design: it reports, the caller decides. Named get
// rejects, list get blanks and warns, inspect cluster omits and warns.
func PlaceholderFields(o CRTObject) []string {
	var fields []string
	for _, f := range o.guardedFieldPtrs() {
		if strings.Contains(*f.val, placeholderToken) {
			fields = append(fields, f.name)
		}
	}
	return fields
}

// clearPlaceholderFields blanks any protected field still carrying the
// placeholder and returns the blanked field names (nil when o was clean) -
// the list get's degrade-in-place mechanism, expressed over the same
// guarded-field set so the two views cannot drift.
func clearPlaceholderFields(o *CRTObject) []string {
	var blanked []string
	for _, f := range o.guardedFieldPtrs() {
		if strings.Contains(*f.val, placeholderToken) {
			*f.val = ""
			blanked = append(blanked, f.name)
		}
	}
	return blanked
}

// crtView is the thin wire decode of a Steve clusterregistrationtoken
// object: only the fields this package reads (the dynamic-client design
// position - no generated type for this kind). ALL SEVEN formatter-
// protected fields (rancher clusterregistrationtoken.go's commandFields
// set) plus Token and ExpiresAt are modeled, so none silently vanishes
// from output or evades the placeholder reporter. Status is a POINTER so
// a payload carrying no status object at all is distinguishable from one
// whose status fields are empty - the shape invariant in validate
// requires the object's presence.
type crtView struct {
	ID       string `json:"id"`
	Metadata struct {
		Namespace string            `json:"namespace"`
		Name      string            `json:"name"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ClusterName string `json:"clusterName"`
	} `json:"spec"`
	Status *crtViewStatus `json:"status"`
}

type crtViewStatus struct {
	Command                    string `json:"command"`
	InsecureCommand            string `json:"insecureCommand"`
	NodeCommand                string `json:"nodeCommand"`
	InsecureNodeCommand        string `json:"insecureNodeCommand"`
	WindowsNodeCommand         string `json:"windowsNodeCommand"`
	InsecureWindowsNodeCommand string `json:"insecureWindowsNodeCommand"`
	ManifestURL                string `json:"manifestUrl"`
	Token                      string `json:"token"`
	ExpiresAt                  string `json:"expiresAt"`
}

// validate enforces the minimum structural invariant every Steve
// clusterregistrationtoken object carries: a non-empty
// metadata.namespace, a non-empty metadata.name, and a PRESENT status
// object (its fields may be empty - a controller may not have filled
// them yet - but the object itself is always served). json.Unmarshal
// alone would silently accept {} or an unrelated payload, and a caller
// like FetchDefaultCRT would then treat a structurally invalid response
// as a successful empty read instead of surfacing an error.
func (v crtView) validate(path string) error {
	if v.Metadata.Namespace == "" || v.Metadata.Name == "" || v.Status == nil {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("decoding %s: valid JSON but not a clusterregistrationtoken object (metadata.namespace, metadata.name, and status are required)", path))
	}
	return nil
}

// object flattens a VALIDATED view; validate guarantees Status non-nil,
// and the nil-guard keeps a future misuse a zero value rather than a panic.
func (v crtView) object() CRTObject {
	st := crtViewStatus{}
	if v.Status != nil {
		st = *v.Status
	}
	return CRTObject{
		Namespace:                  v.Metadata.Namespace,
		Name:                       v.Metadata.Name,
		Labels:                     v.Metadata.Labels,
		ClusterName:                v.Spec.ClusterName,
		Command:                    st.Command,
		InsecureCommand:            st.InsecureCommand,
		NodeCommand:                st.NodeCommand,
		InsecureNodeCommand:        st.InsecureNodeCommand,
		WindowsNodeCommand:         st.WindowsNodeCommand,
		InsecureWindowsNodeCommand: st.InsecureWindowsNodeCommand,
		ManifestURL:                st.ManifestURL,
		Token:                      st.Token,
		ExpiresAt:                  st.ExpiresAt,
	}
}

// DecodeCRTObject decodes a single Steve object body into the policy-
// neutral view. A malformed body (never valid JSON) OR a structurally
// invalid one (valid JSON missing metadata.namespace, metadata.name, or
// the status object - the invariant validate states) is a typed
// CodeGeneric error naming the path - never a panic, never an empty
// CRTObject standing in for a decode failure.
func DecodeCRTObject(path string, body []byte) (CRTObject, error) {
	var v crtView
	if err := json.Unmarshal(body, &v); err != nil {
		return CRTObject{}, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("decoding %s: %v", path, err))
	}
	if err := v.validate(path); err != nil {
		return CRTObject{}, err
	}
	return v.object(), nil
}

// DecodeCRTCollection decodes a Steve collection envelope ({"data": [...]}).
// The ENVELOPE carries its own shape invariant: a JSON-ARRAY "data" field
// must be present - {"data":[]} is a valid empty collection, while a
// missing, null, or non-array data field is a typed wrong-shape error (a
// bare {} or an error payload must never pass as a successful empty
// collection). Data is a slice POINTER so absent/null are detectable (nil
// pointer), and a non-array value fails json.Unmarshal itself - both land
// in the same typed "decoding <path>" CodeGeneric contract. Element
// decoding then applies DecodeCRTObject's per-object invariant: decode
// failures abort the whole collection - only the PLACEHOLDER policy
// degrades per row, and that lives in the callers.
func DecodeCRTCollection(path string, body []byte) ([]CRTObject, error) {
	var env struct {
		Data *[]crtView `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("decoding %s: %v", path, err))
	}
	if env.Data == nil {
		return nil, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("decoding %s: valid JSON but not a Steve collection envelope (a %q array is required)", path, "data"))
	}
	out := make([]CRTObject, 0, len(*env.Data))
	for i, v := range *env.Data {
		if err := v.validate(fmt.Sprintf("%s (object %d)", path, i)); err != nil {
			return nil, err
		}
		out = append(out, v.object())
	}
	return out, nil
}

// FetchDefaultCRT reads a cluster's default registration token -
// "<managementClusterID>/default-token" on the same Steve /v1 path the
// get override reads - and returns the policy-neutral decoded object.
// Narrow by design and used ONLY by inspect cluster's registration
// section: a single default-token fetch cannot back the collection get,
// which fetches the whole Steve collection, preserves tokens across
// namespaces, and filters locally.
func FetchDefaultCRT(ctx context.Context, root engine.RootClient, managementClusterID string) (CRTObject, error) {
	path := crtCollectionPath + "/" + managementClusterID + "/default-token"
	body, err := root.GetJSON(ctx, path)
	if err != nil {
		return CRTObject{}, err
	}
	return DecodeCRTObject(path, body)
}

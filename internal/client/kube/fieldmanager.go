package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// FieldManager is the name the server records in an object's managed
// fields for every write the CLI sends to a Kubernetes API: the client-go
// writes (set as the FieldManager of the Create, Update, Patch and Apply
// options) and the raw requests NewWriteRequest builds alike. Without it
// the apiserver derives a manager from the User-Agent's first token, which
// changes with the binary name. The CLI's domain, unversioned, is a stable
// identity: it survives a binary rename and an output-schema bump, and it
// is the root any later server-side-apply manager names build on.
const FieldManager = "cli.rancher.io"

// fieldManagerParam is the query parameter that carries the field manager.
const fieldManagerParam = "fieldManager"

// NewWriteRequest builds a POST, PUT or PATCH request to a Kubernetes API
// with the query parameter fieldManager=FieldManager added, and leaves the
// rest of the URL as it was: the existing query keeps its order and bytes,
// and the fragment and the escaped path are unchanged. It refuses any other
// method, a URL it cannot parse, a query it cannot decode, and a URL that
// already names a field manager other than FieldManager or names one more
// than once. A URL that already names FieldManager is left as it is.
//
// Every Kubernetes write the CLI builds by hand goes through this function
// (a test in test/conformance fails on any other); the writes client-go
// sends set FieldManager in their options instead.
func NewWriteRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return nil, fmt.Errorf("a Kubernetes write request uses POST, PUT or PATCH, not %q", method)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing the Kubernetes write URL: %w", err)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("reading the query of the Kubernetes write URL: %w", err)
	}
	switch managers := query[fieldManagerParam]; {
	case len(managers) > 1:
		return nil, fmt.Errorf("the Kubernetes write URL names %d field managers; it names one", len(managers))
	case len(managers) == 1 && managers[0] != FieldManager:
		return nil, fmt.Errorf("the Kubernetes write URL names the field manager %q; it names %q", managers[0], FieldManager)
	case len(managers) == 0:
		param := fieldManagerParam + "=" + url.QueryEscape(FieldManager)
		if u.RawQuery == "" {
			u.RawQuery = param
		} else {
			u.RawQuery += "&" + param
		}
	}
	return http.NewRequestWithContext(ctx, method, u.String(), body)
}

package testrancher

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseCLISignInURL reads the sign-in URL the CLI prints for a browser
// sign-in, the way Rancher's dashboard sign-in page reads it, and returns the
// request id and public key the dashboard sign-in page sends back to Rancher.
//
// The dashboard sign-in page shows an "invalid parameters" message and offers
// no sign-in button unless responseType is exactly "kubeconfig" and requestId
// and publicKey are not empty. This function returns an error in those
// cases. It also requires the path to end in /dashboard/auth/login and
// cli=true, which the CLI always sends; the dashboard sign-in page checks
// only responseType, requestId and publicKey. The dashboard sign-in page on
// Rancher main also accepts kubeconfig_<cluster id>; this function refuses it
// because the CLI does not send it.
//
// Every test that reads a request id or public key from a sign-in URL calls
// this function, so a sign-in URL the dashboard sign-in page would refuse
// fails the test.
func ParseCLISignInURL(raw string) (requestID, publicKey string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("sign-in URL does not parse: %w", err)
	}
	if !strings.HasSuffix(u.Path, "/dashboard/auth/login") {
		return "", "", fmt.Errorf("sign-in URL path is %q, want one ending in /dashboard/auth/login", u.Path)
	}
	q := u.Query()
	if got := q.Get("cli"); got != "true" {
		return "", "", fmt.Errorf("sign-in URL has cli=%q, want true", got)
	}
	if got := q.Get("responseType"); got != "kubeconfig" {
		return "", "", fmt.Errorf("sign-in URL has responseType=%q, and the dashboard sign-in page accepts only kubeconfig", got)
	}
	requestID, publicKey = q.Get("requestId"), q.Get("publicKey")
	var missing []string
	if requestID == "" {
		missing = append(missing, "requestId")
	}
	if publicKey == "" {
		missing = append(missing, "publicKey")
	}
	if len(missing) > 0 {
		return "", "", errors.New("sign-in URL has an empty " + strings.Join(missing, " and ") + ", and the dashboard sign-in page refuses it")
	}
	return requestID, publicKey, nil
}

package httpx

import (
	"fmt"
	"net/http"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// maxRedirects matches net/http's default hop cap so the only behavioral
// change from the default policy is the origin check.
const maxRedirects = 10

// RejectCrossOriginRedirect is the CheckRedirect policy of every client
// this package builds: a redirect is followed only when its target shares
// the origin (scheme, hostname, effective port) of the first request in
// the chain. A 307/308 replays the request body and a same-site redirect
// keeps the bearer, so following one across origins would hand
// credentials to a server the pinned CA was never meant to vouch for. The
// wording names only the two origins: the same policy guards Rancher and
// identity-provider dials, so it must not point at a server entry.
//
// Every refusal is a structured, non-retryable GENERIC error, including the
// hop cap and an unusable redirect target: the error adapters preserve a
// structured error as is, and a bare error would be relabeled a retryable
// connectivity failure whose remedy (check the CA) cannot apply.
func RejectCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		// The hop cap wins over any origin problem: a loop is the more
		// useful diagnosis, and the origin is named only when it parses.
		msg := fmt.Sprintf("stopped after %d redirects", maxRedirects)
		if first, err := OriginOf(via[0].URL); err == nil {
			msg += " from " + first.String()
		}
		return clierr.New(clierr.CodeGeneric, msg)
	}
	first, err := OriginOf(via[0].URL)
	if err != nil {
		return clierr.Wrap(err, "refused to follow a redirect: the original request has no usable origin: "+err.Error())
	}
	next, err := OriginOf(req.URL)
	if err != nil {
		return clierr.Wrap(err, fmt.Sprintf("refused to follow a redirect from %s: the redirect target has no usable origin: %s", first, err.Error()))
	}
	if first.Equal(next) {
		return nil
	}
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
		"refused to follow a redirect from %s to %s: a redirect to another origin is not followed, to keep credentials from being sent there; check that %s is the correct address",
		first, next, first))
}

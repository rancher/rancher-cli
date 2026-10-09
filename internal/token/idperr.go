package token

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
)

// idpCertificateError rewords a certificate-verification failure on an
// identity-provider endpoint as an AUTH error that names the provider's
// host and the failure kind. It never produces TLS_TRUST and never offers
// guided trust: the provider is verified against the operating system
// store, and only the unknown-authority kind can be repaired there. Any
// other error, and a certificate failure at a different origin, is
// returned unchanged.
func idpCertificateError(err error, endpoint string) error {
	u, perr := url.Parse(endpoint)
	if perr != nil {
		return err
	}
	origin, oerr := httpx.OriginOf(u)
	if oerr != nil {
		return err
	}
	kind, ok := tlstrust.KindOf(err, origin, time.Now())
	if !ok {
		return err
	}
	host := origin.Hostname()
	switch kind {
	case tlstrust.KindUnknownAuthority:
		return clierr.New(clierr.CodeAuth, "identity provider "+host+": certificate not trusted by the operating system trust store; --cacert applies only to the Rancher server, add the identity provider's CA to the operating system trust store")
	case tlstrust.KindHostnameMismatch:
		return clierr.New(clierr.CodeAuth, "identity provider "+host+": certificate is not valid for this host name")
	case tlstrust.KindExpired:
		return clierr.New(clierr.CodeAuth, "identity provider "+host+": certificate has expired")
	case tlstrust.KindNotYetValid:
		return clierr.New(clierr.CodeAuth, "identity provider "+host+": certificate is not yet valid")
	default:
		return clierr.New(clierr.CodeAuth, "identity provider "+host+": certificate chain is invalid")
	}
}

// idpError reports a failed request to the identity provider's token or
// device endpoint, prefix naming the step. A 4xx answer is AUTH: the
// provider refused the sign-in, and the status table would read a 400 as
// VALIDATION. A 408 or 429 is retryable GENERIC: the provider asks to try
// again. A 5xx goes through the status table, so it is retryable. Any
// other error keeps its text, a certificate failure reworded.
func idpError(err error, endpoint, prefix string) error {
	re, ok := errors.AsType[*oauth2.RetrieveError](err)
	if !ok || re.Response == nil {
		return fmt.Errorf("%s: %w", prefix, idpCertificateError(err, endpoint))
	}
	status := re.Response.StatusCode
	switch {
	case status >= 400 && status < 500:
		msg := fmt.Sprintf("%s: the identity provider refused the sign-in (%d)", prefix, status)
		if re.ErrorCode != "" {
			msg += ": " + re.ErrorCode
		}
		cause := clierr.ResponseError(http.MethodPost, status, re.Body)
		if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			return clierr.Retryable(clierr.CodeGeneric, msg).WithCause(cause)
		}
		return clierr.New(clierr.CodeAuth, msg).WithCause(cause)
	case status >= 500:
		return clierr.FromResponse(prefix, http.MethodPost, status, re.Body, nil)
	}
	return fmt.Errorf("%s: %w", prefix, idpCertificateError(err, endpoint))
}

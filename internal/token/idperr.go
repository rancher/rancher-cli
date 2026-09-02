package token

import (
	"net/url"
	"time"

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

// A transport put on a config, and a wrapper added to its chain.
package planted

import (
	"net/http"

	"k8s.io/client-go/rest"
)

func Transport(rc *rest.Config) {
	rc.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return rt }
	rc.Transport = http.DefaultTransport
	rc.Wrap(func(rt http.RoundTripper) http.RoundTripper { return rt })
}

// Uses of a client-go config that the scan leaves alone: a config handed in,
// a value copy of one, a field that is no transport, and a transport set on
// something that is no config.
package clean

import (
	"net/http"

	"k8s.io/client-go/rest"
)

func Handed(rc *rest.Config) *rest.Config {
	c := *rc
	c.Timeout = 0
	c.Host = rc.Host
	return &c
}

func Pointer() *rest.Config { return nil }

func Client() *http.Client {
	hc := &http.Client{}
	hc.Transport = http.DefaultTransport
	return hc
}

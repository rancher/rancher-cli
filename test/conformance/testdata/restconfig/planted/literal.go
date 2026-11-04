// A composite literal of a client-go config.
package planted

import "k8s.io/client-go/rest"

func Literal() *rest.Config { return &rest.Config{Host: "https://example.com"} }

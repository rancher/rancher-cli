// A config made with new and with a zero-value declaration.
package planted

import "k8s.io/client-go/rest"

func New() *rest.Config { return new(rest.Config) }

func Var() *rest.Config {
	var c rest.Config
	return &c
}

package restpost

import "k8s.io/client-go/rest"

func send(c rest.Interface) {
	_ = c.Post()
}

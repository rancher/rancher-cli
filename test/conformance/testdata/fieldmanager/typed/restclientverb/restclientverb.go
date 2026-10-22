package restclientverb

import "k8s.io/client-go/rest"

func send(c *rest.RESTClient) {
	_ = c.Verb("POST")
}

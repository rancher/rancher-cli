package restget

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func read(c rest.Interface, cs *kubernetes.Clientset) {
	_ = c.Get()
	_ = cs.Discovery().RESTClient().Get()
}

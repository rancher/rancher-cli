package kubeconfig

import (
	"sync"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"

	"github.com/rancher/rancher-cli/internal/yamlwrite"
)

// kubeconfigScheme holds the internal and v1 kubeconfig types and the
// conversion between them, the one clientcmd.Write runs. It is built on first
// use: a process that never writes a kubeconfig pays nothing for it.
var kubeconfigScheme = sync.OnceValues(func() (*k8sruntime.Scheme, error) {
	s := k8sruntime.NewScheme()
	if err := clientcmdapi.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := clientcmdv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
})

// writeKubeconfig returns the v1 kubeconfig kubectl reads: the bytes
// clientcmd.Write returns for a value that holds none of the characters
// internal/yamlwrite names. clientcmd.Write passes its JSON to the YAML
// library with no step between, and the library refuses those characters;
// this function writes them through yamlwrite.Marshal as escapes, and every
// value reads back unchanged.
func writeKubeconfig(kc clientcmdapi.Config) ([]byte, error) {
	s, err := kubeconfigScheme()
	if err != nil {
		return nil, err
	}
	out, err := s.ConvertToVersion(&kc, clientcmdv1.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}
	return yamlwrite.Marshal(out)
}

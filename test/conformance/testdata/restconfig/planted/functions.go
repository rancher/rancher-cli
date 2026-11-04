// The functions that build or copy a client-go config.
package planted

import (
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/auth/exec"
	"k8s.io/client-go/tools/clientcmd"
)

func Functions(rc *rest.Config) {
	_ = rest.CopyConfig(rc)
	_, _ = rest.InClusterConfig()
	_ = rest.AnonymousClientConfig(rc)
	_, _, _ = exec.LoadExecCredentialFromEnv()
	_, _ = clientcmd.BuildConfigFromFlags("", "")
	_ = clientcmd.NewDefaultClientConfigLoadingRules()
	_, _ = clientcmd.LoadFromFile("")
}

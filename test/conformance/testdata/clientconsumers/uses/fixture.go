// Every use of a client consumer and of the install id fetcher registration
// that the scans report, and a use of a builder they leave alone.
package uses

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"k8s.io/client-go/rest"
)

func Uses(rc *rest.Config) {
	_, _ = kube.HTTPClient(rc)
	_, _ = kube.Dynamic(rc)
	_, _ = kube.Discovery(rc)
	_, _ = kube.Clientset(rc)
	_, _ = kube.TableClient(rc)
	_ = httpx.NewExternal(0)
	f := kube.Dynamic
	_ = f
	httpx.RegisterInstallIDFetcher("https://example.com", func(context.Context) (string, int, error) { return "", 0, nil })
	_ = kube.BaseURL("https://example.com")
}

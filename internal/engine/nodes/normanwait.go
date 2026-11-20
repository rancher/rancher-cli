package nodes

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// steveWaitClient is the client of a wait on the Norman path: its Get reads
// the management node through the Steve API and returns it as Steve serves it,
// with metadata.state and the links. It embeds the local client only to be a
// engine.ClusterClient: the wait calls nothing else, and the embedded
// interface offers no watch; the wait polls.
type steveWaitClient struct {
	engine.ClusterClient
	get steve.Getter
}

func (c steveWaitClient) Get(ctx context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	obj, err := readSteveNode(ctx, c.get, target{id: ns, mid: name}, nil)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

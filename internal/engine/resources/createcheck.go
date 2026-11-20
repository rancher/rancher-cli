package resources

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CreateCheckInput is what `create resource` gives a CreateCheck: the client
// of the cluster the create is sent to, the root client factory and server
// URL of the check's entry, and the manifest as parsed and routed (its
// namespace is the one the create uses).
type CreateCheckInput struct {
	Local  engine.ClusterClient
	Roots  engine.RootClientFactory
	Server func() string
	Obj    *unstructured.Unstructured
}

// CreateCheck is a check `create resource` runs for the manifests of one kind
// row, beyond the row's own ObjectChecks. A check needs reads of the server, so
// it lives in the package that makes them and is passed to NewCreate; this
// package cannot import it.
type CreateCheck struct {
	// Key is the key of the kind row the check is for.
	Key string
	// Roots and Server are handed to the check in CreateCheckInput.
	Roots  engine.RootClientFactory
	Server func() string
	// Before runs before the server dry-run. It returns warnings for the
	// preview, or an error that ends the command as it is.
	Before func(ctx context.Context, in CreateCheckInput) ([]string, error)
	// After runs after the dry-run with the server's answer. An error ends the
	// command as it is, and nothing is written.
	After func(ctx context.Context, in CreateCheckInput, observed *unstructured.Unstructured) error
}

// ObjectChecked is implemented by the generated patch and delete commands:
// they run the row's ObjectChecks. The build of the command tree refuses a row
// with ObjectChecks whose patch or delete command does not implement it.
type ObjectChecked interface {
	RunsObjectChecks()
}

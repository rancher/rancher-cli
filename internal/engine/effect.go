package engine

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PlanCreate previews a create via server-side dry-run and returns the Effect.
// A create has no prior state, so ResourceVersion is empty and Diff is empty —
// the dry-run itself validates admission; action+target are the preview. (Richer
// field-level create diffs are a later enhancement.)
func PlanCreate(ctx context.Context, repo Repo, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, tier, stability string) (safety.Effect, error) {
	out, err := repo.Create(ctx, gvr, ns, obj, true)
	if err != nil {
		return safety.Effect{}, err
	}
	return safety.Effect{
		Action:            "create",
		Target:            gvr.Resource + "/" + out.GetName(),
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       false,
		Tier:              tier,
		Stability:         stability,
	}, nil
}

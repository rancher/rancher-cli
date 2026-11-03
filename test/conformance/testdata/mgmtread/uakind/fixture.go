// A kind looked up by the name userattribute, whose GVR reaches Get.
package uakind

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

func Read(ctx context.Context, local engine.ClusterClient, userID string) {
	k, _ := kinds.Lookup("userattribute")
	_, _ = local.Get(ctx, k.GVR, "", userID)
}

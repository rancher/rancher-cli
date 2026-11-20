package catalog

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
)

// extensionFeature is the Rancher feature that turns UI extensions on.
const extensionFeature = "uiextension"

// checkExtensionsAvailable runs the checks every extension command makes
// first, in this order: the feature uiextension is not turned off, and the
// namespace Rancher installs extensions into exists. A feature that does not
// exist, or that has no value, counts as on, as in the Rancher UI. Each
// refusal has one text; any other failure is the server's answer.
func checkExtensionsAvailable(ctx context.Context, client engine.ClusterClient) error {
	feature, err := client.Get(ctx, kind("feature").GVR, "", extensionFeature)
	switch {
	case err == nil:
		if on := engine.FeatureEffectiveValue(feature); on != nil && !*on {
			return clierr.New(clierr.CodeValidation,
				"UI extensions are turned off on this server (feature "+extensionFeature+
					"); turning the feature on takes effect after Rancher restarts")
		}
	case !notFound(err):
		return err
	}
	if _, err := client.Get(ctx, kind("namespace").GVR, "", extension.Namespace); err != nil {
		if notFound(err) {
			return clierr.New(clierr.CodeNotFound, "namespace "+extension.Namespace+
				" not found; Rancher creates it when the features "+extensionFeature+" and MCM are on")
		}
		return err
	}
	return nil
}

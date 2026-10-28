// Every use of the server URL machinery the route scan reports.
package routes

import (
	"os"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

func Uses(cfg *config.Config, store config.CredStore) {
	_, _ = cfg.Effective(store)
	_ = cfg.EffectiveConnection()
	_, _ = kube.RESTConfig(config.Effective{}, "c-1")
	_, _ = credhelper.CanonicalServerURL("https://example.com")
	_, _ = credhelper.CheckServerURL("https://example.com")
	_ = config.CheckServerURL("https://example.com")
	_ = clierr.RecoverySequence("https://example.com", "u-abc12", clierr.TokenRef{})
	f := clierr.RecoverySequence
	_ = f
	_ = os.Getenv(config.EnvURL)
	_ = os.Getenv("RANCHER_CLI_URL")
	const local = "RANCHER_CLI_URL"
	_, _ = os.LookupEnv(local)
	_ = os.Getenv("RANCHER_CLI_TOKEN")
}

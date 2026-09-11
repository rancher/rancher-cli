package cmdtest

import (
	"os"

	"github.com/rancher/rancher-cli/internal/config"
)

// UnsetResolutionEnv clears the RANCHER_CLI_URL, RANCHER_CLI_TOKEN,
// RANCHER_CLI_CACERT and RANCHER_CLI_CREDENTIAL_STORE variables, which the
// config resolution reads ahead of the stored file, and RANCHER_CLI_READONLY,
// which the surface reads as a read-only source, so an operator's shell
// cannot leak into a test's assertions. The TestMains of the packages under
// internal/commands, internal/cli and internal/engine/kubeconfig call it;
// internal/config and internal/token clear the same variables with their
// own loops (config cannot import this package).
func UnsetResolutionEnv() {
	for _, v := range []string{
		config.EnvURL, config.EnvToken, config.EnvCACert, config.EnvCredentialStore, config.EnvReadOnly,
	} {
		_ = os.Unsetenv(v)
	}
}

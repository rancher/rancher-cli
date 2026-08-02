package config

// Env var names the CLI resolves from the process environment. Centralized
// so a rename touches one line instead of every call site.
const (
	EnvConfigDir         = "RANCHER_CLI_CONFIG_DIR"
	EnvURL               = "RANCHER_CLI_URL"
	EnvToken             = "RANCHER_CLI_TOKEN"
	EnvCACert            = "RANCHER_CLI_CACERT"
	EnvContext           = "RANCHER_CLI_CONTEXT"
	EnvCredentialStore   = "RANCHER_CLI_CREDENTIAL_STORE"
	EnvReadOnly          = "RANCHER_CLI_READONLY"
	EnvOAuthAuthFlow     = "RANCHER_CLI_OAUTH_AUTH_FLOW"
	EnvOAuthCallbackPort = "RANCHER_CLI_OAUTH_CALLBACK_PORT"

	// EnvLegacyOAuthAuthFlow/EnvLegacyOAuthCallbackPort are the legacy
	// CLI's spellings honored by the OAuth flow (both the native get-token
	// path and the `token` compat shim share a single resolver), so
	// legacy-generated environments keep working. Precedence:
	// flag, then RANCHER_CLI_*, then CATTLE_*.
	EnvLegacyOAuthAuthFlow     = "CATTLE_OAUTH_AUTH_FLOW"
	EnvLegacyOAuthCallbackPort = "CATTLE_OAUTH_CALLBACK_PORT"
)

package conformance

const (
	whyMainText             = "built in the login and token code together with that package's own error texts"
	whyMainGated            = "deletes a token or kubeconfig the login code created or recorded: the command keeps the gates of its command"
	whyMainCreateKubeconfig = "the exec plugin's no-terminal remedy: it creates a kubeconfig for kubectl to use at once; the command keeps the gates of its command"
	whyMainPositional       = "the login and token code writes the value into the command with its own quoting"
)

// pasteMainTextExceptions records the remedy texts of the login and token code
// by exact text.
var pasteMainTextExceptions = []pasteTextException{
	{"internal/clierr/loginfailure.go", pasteCheckDryRun, "rancher delete management-token {v} --apply --yes --allow-deprecated", whyMainGated},
	{"internal/clierr/loginfailure.go", pasteCheckDryRun, "rancher delete token {v} --apply --yes", whyMainGated},
	{"internal/engine/kubeconfig/records.go", pasteCheckEnd, ", then run rancher kubeconfig --cluster '{v}' again", whyMainText},
	{"internal/engine/kubeconfig/records.go", pasteCheckSchema, "pipe an API token to rancher auth login '{v}' --token-stdin{v}", whyMainText},
	{"internal/engine/kubeconfig/records.go", pasteCheckSchema, "pipe an API token to rancher auth login '{v}' --token-stdin{v}", whyMainText},
	{"internal/engine/kubeconfig/records.go", pasteCheckEnd, "remove {v} with rancher auth logout {v}, then rancher config delete-server {v}{v}", whyMainText},
	{"internal/engine/kubeconfig/records.go", pasteCheckEnd, "the kubeconfig's --name '{v}' would start with '{v}', which auth get-token refuses; log in with a server entry of another name: pipe an API token to {v}, then rancher config use-server <name>, then rancher kubeconfig --cluster '{v}'", whyMainText},
	{"internal/token/deletetoken.go", pasteCheckEnd, "specify either a credential name or --all, not both (e.g. rancher auth delete-token prod, or rancher auth delete-token --all)", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "; or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "rancher config use-server {v} (this sets the directory's current server, which is unset now)", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "rancher config use-server {v} (this switches the directory's current server; switch back with rancher config use-server '{v}')", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server '{v}'), then rancher kubeconfig --cluster {v}", whyMainText},
	{"internal/token/entry.go", pasteCheckSchema, "more than one server entry for <unusable URL> has a stored login ({v}) and the legacy rancher token command cannot name one; regenerate the kubeconfig from the entry you want with {v}, and replace this kubeconfig with the one it prints", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "more than one server entry for <unusable URL> has a stored login ({v}) and the legacy rancher token command cannot name one; regenerate the kubeconfig from the entry you want with {v}, and replace this kubeconfig with the one it prints", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "the exec entry names server entry {v}, which is registered at {v}, but this call runs for {v}; to use {v}, {v}; to use {v}, regenerate it from an entry registered there with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server '{v}'), then rancher kubeconfig --cluster {v}", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "rancher auth login {v} (if it has no login), then rancher kubeconfig --cluster {v}", whyMainText},
	{"internal/token/entry.go", pasteCheckEnd, "rancher config use-server {v} (this switches the directory's current server; switch back with rancher config use-server '{v}'), then {v}", whyMainText},
	{"internal/token/failure.go", pasteCheckEnd, "a session token may have been created through {v} {v}; the CLI stopped waiting at {v}, but the browser sign-in can still complete after that, and the token may be newer; these facts narrow the candidates but cannot identify one token; inspect them with: rancher get management-tokens -o json, or in the UI under Account & API Keys", whyMainText},
	{"internal/token/noterminal.go", pasteCheckDryRun, "rancher create kubeconfig --clusters {v} --apply --yes", whyMainCreateKubeconfig},
	{"internal/token/noterminal.go", pasteCheckEnd, "rancher config use-server <name> (this switches the directory's current server; switch back with rancher config use-server '{v}'), then rancher kubeconfig --cluster {v}", whyMainText},
	{"internal/token/owner.go", pasteCheckEnd, "remove {v} with rancher auth logout {v}, then rancher config delete-server {v}; the next call signs in as '{v}'{v} in a terminal, or uses {v}{v}", whyMainText},
	{"internal/token/owner.go", pasteCheckEnd, "pipe an API token to {v}, then rancher config use-server <name> (this switches the directory's current server; switch back with rancher config use-server '{v}'), then rancher kubeconfig --cluster '{v}'", whyMainText},
	{"internal/token/owner.go", pasteCheckEnd, "{v} regenerate the kubeconfig from an entry that holds the login of '{v}' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server '{v}'), then rancher kubeconfig --cluster '{v}'", whyMainText},
	{"internal/token/pluginlogin.go", pasteCheckEnd, "the credential store (policy {v}) cannot store a login credential, and one cannot be reused across clusters; fix the store, or turn the setting off with: rancher config set-token-reuse off ({v})", whyMainText},
	{"internal/token/revocationplan.go", pasteCheckEnd, " (rancher auth login '{v}')", whyMainText},
	{"internal/token/signin.go", pasteCheckEnd, "{v} can delete it with: {v}, from a directory signed in as {v} (rancher auth login signs one in)", whyMainText},
}

// pasteMainPositionalExceptions records the positional operands of the login
// and token code.
var pasteMainPositionalExceptions = []pastePositionalException{
	{"internal/clierr/loginfailure.go", "RecoveryCommands", "ref.Name", whyMainPositional},
	{"internal/clierr/loginfailure.go", "RecoveryCommands", "ref.Name", whyMainPositional},
	{"internal/clierr/recovery.go", "RecoverySequence", "entry", whyMainPositional},
	{"internal/clierr/recovery.go", "RecoverySequence", "entry", whyMainPositional},
	{"internal/clierr/recovery.go", "RecoverySequence", "entry", whyMainPositional},
	{"internal/commands/auth/logout.go", "logoutRerun", "clierr.ShellQuote(strings.Join(targets, \" \"))", whyMainPositional},
	{"internal/engine/kubeconfig/records.go", "recordedOwner", "clierr.ShellQuote(lk.Entry)", whyMainPositional},
	{"internal/engine/kubeconfig/records.go", "recordedOwner", "clierr.ShellQuote(lk.Identified)", whyMainPositional},
	{"internal/engine/kubeconfig/records.go", "recordedOwner", "entry", whyMainPositional},
	{"internal/engine/kubeconfig/records.go", "recordedOwner", "entry", whyMainPositional},
	{"internal/token/deletetoken.go", "Execute", "clierr.ShellQuote(name)", whyMainPositional},
	{"internal/token/entry.go", "pluginEntriesError", "clierr.ShellQuote(identified)", whyMainPositional},
	{"internal/token/entry.go", "useServerRemedy", "entry", whyMainPositional},
	{"internal/token/entry.go", "useServerRemedy", "entry", whyMainPositional},
	{"internal/token/entry.go", "useServerRemedy", "clierr.ShellQuote(current)", whyMainPositional},
	{"internal/token/entry.go", "entryNotNamedError", "clusterArg", whyMainPositional},
	{"internal/token/entry.go", "namedEntryRoute", "entry", whyMainPositional},
	{"internal/token/entry.go", "namedEntryRoute", "cluster", whyMainPositional},
	{"internal/token/failure.go", "credentialRecoveryFor", "clierr.ShellQuote(server)", whyMainPositional},
	{"internal/token/failure.go", "reLogin", "clierr.ShellQuote(server)", whyMainPositional},
	{"internal/token/noterminal.go", "noTerminalError", "cluster", whyMainPositional},
	{"internal/token/noterminal.go", "CreateKubeconfigRemedy", "cluster", whyMainPositional},
	{"internal/token/noterminal.go", "NoTerminalLogin", "cluster", whyMainPositional},
	{"internal/token/noterminal.go", "NoTerminalLogin", "entry", whyMainPositional},
	{"internal/token/owner.go", "relogin", "quoted", whyMainPositional},
	{"internal/token/owner.go", "relogin", "quoted", whyMainPositional},
	{"internal/token/owner.go", "relogin", "quoted", whyMainPositional},
	{"internal/token/owner.go", "authConfigRemedy", "clierr.ShellQuote(entry)", whyMainPositional},
	{"internal/token/revocationplan.go", "revokeTLSRemedy", "entry", whyMainPositional},
	{"internal/token/revocationplan.go", "Warnings", "clierr.ShellQuote(e)", whyMainPositional},
	{"internal/token/revocationplan.go", "nextLoginText", "clierr.ShellQuote(entry)", whyMainPositional},
	{"internal/token/token.go", "classifyRejection", "clierr.ShellQuote(sc.name)", whyMainPositional},
}

package conformance

// evProductionAllow is the rows the guard 3a walk consults over the
// production tree.
var evProductionAllow = evAllow{
	originCallees: evOriginCallees,
	originThrough: evOriginThrough,
	originParams:  evOriginParams,
}

// evOriginCallees are the calls guard 3a accepts as the source of a value,
// each with its reason: a value read from the server, the config or the
// typed line, or the canonical form of a server URL that is the command's
// meaning.
var evOriginCallees = map[string]string{
	"internal/runtime/completion.TailFlag":                "reads a flag's value from the typed line as it was typed",
	"internal/commands/auth.logoutTargets":                "returns the names of config entries as the config holds them",
	"internal/engine/catalog.runningOperation":            "returns the name of an operation object as the server holds it",
	"internal/engine.ResolveClusterArgument":              "returns the cluster id the server lists for the typed name or id",
	"internal/engine.ClusterClientFactory.ResolveCluster": "returns the cluster id the server lists for the typed name or id",
	"internal/engine/oidcclient.resolveSecretName":        "returns the name of a client secret as the server holds it, or the typed name",
	"internal/engine/resources.Route":                     "returns the cluster and namespace the server and the typed flags name",
	"internal/token.identifyEntry":                        "returns the name of a config entry as the config holds it",
	"internal/config.ExecLoginEntryNameFor":               "returns the name of the exec login entry the config holds or will hold",
	"internal/config.CollisionFreeName":                   "returns a config entry name the config does not hold yet, which the command creates",
	"internal/token.tokenCmd.resolveConnWithCA":           "returns the server, cluster and entry the config and the typed flags name",
	"internal/config.CanonicalServerIdentity":             "the canonical identity of a server URL, the form the config is read by",
	"internal/commands/register.CanonicalizeServerURL":    "the canonical form of a server URL, the form the config stores",
	"internal/clierr.RecoveryEntryName":                   "the name of the entry a recovery creates, derived from the server's host",
	"internal/config.DisplayServerURL":                    "the server URL in the form the config shows and accepts",
	"internal/cli.remedyDirOf":                            "the config directory the invocation opens, in the form its remedies name it, or none when they need no --config",
	"internal/client/kube.BaseURL":                        "the server URL reduced to its scheme and host, the form a kubeconfig's cluster server names",
	"internal/token.entryOfName":                          "returns the config entry identifyEntry finds, or the entry the exec entry's name was made from",
	"credhelper.CheckServerURL":                           "the canonical identity of a server URL, the form the cache and the config are read by",
}

// evOriginThrough are calls that return one of their arguments unchanged:
// the walk goes on through their arguments.
var evOriginThrough = map[string]string{
	"slices.Compact": "drops repeated neighbours and keeps every element it returns unchanged",
	"cmp.Or":         "returns the first of its arguments that is not empty, unchanged",
}

// evOriginParams are the functions whose parameters reach the builder
// through a function value or an interface method, which the walk does not
// follow, each with what its callers pass.
var evOriginParams = map[string]string{
	"internal/client/provider.dynamicClient.Get":             "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.List":            "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.Logs":            "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.Create":          "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.Update":          "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.Delete":          "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.ServerResources": "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.ListTable":       "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.dynamicClient.GetTable":        "a method of the provider client, called through the engine.ClusterClient interface: its receiver's cluster id is the id the engine resolved from the server or the typed --cluster, and the namespace and name come from the typed line or the server unchanged",
	"internal/client/provider.Identity":                      "a provider function the CLI layer holds as a function value: the effective config it is given names the server entry and the known servers as the config holds them",
	"internal/client/provider.Owner":                         "a provider function the CLI layer holds as a function value: the effective config it is given names the server entry and the known servers as the config holds them",
	"internal/client/provider.Preflight":                     "a provider function the CLI layer holds as a function value: the effective config it is given names the server entry and the known servers as the config holds them",
	"internal/client/provider.EffectiveTokenMetadata":        "a provider function the CLI layer holds as a function value: the effective config it is given names the server entry and the known servers as the config holds them",
	"internal/client/provider.V3TokenMetadata":               "a provider function the CLI layer holds as a function value: the effective config it is given names the server entry and the known servers as the config holds them",
	"internal/cli.clientFactory.ResolveClusterArgument":      "the client factory's method, called through engine.ClusterClientFactory: callers pass the cluster name or id as typed",
	"internal/commands/auth.loginCmd.CompletionTarget":       "the CompletionTarget of a command, called through the completion interface: the config and the typed completion request, read as they are",
	"internal/token.tokenCmd.CompletionTarget":               "the CompletionTarget of a command, called through the completion interface: the config and the typed completion request, read as they are",
	"internal/token.legacyTokenCmd.CompletionTarget":         "the CompletionTarget of a command, called through the completion interface: the config and the typed completion request, read as they are",
	"internal/commands/auth.loginCmd.authenticateAndStore":   "a function literal handed to LoginTokenWarning or a failure renderer: it is called with the TokenFate of a token the CLI created, whose Ref names the token as the create response did",
	"internal/token.FinishUnsupportedStore":                  "a function literal handed to LoginTokenWarning or a failure renderer: it is called with the TokenFate of a token the CLI created, whose Ref names the token as the create response did",
	"internal/token.tokenCmd.pluginLogin":                    "a function literal handed to LoginTokenWarning or a failure renderer: it is called with the TokenFate of a token the CLI created, whose Ref names the token as the create response did",
	"internal/token.signInForClusterToken":                   "a function literal called with the outcome of the cluster token's create: its UserID is the owner the create response named",
	"internal/token.clusterSignIn.warning":                   "a function literal handed to LoginTokenWarning or a failure renderer: it is called with the TokenFate of a token the CLI created, whose Ref names the token as the create response did",
	"internal/config.DeleteLoginToken":                       "held as a function value so a test can replace it: callers pass the ServerRef of a config entry, whose Name is the entry name as the config holds it",
	"internal/config.keyringStore.GetToken":                  "a method of the credential store, called through the config.CredStore interface: callers pass the ServerRef of a config entry as the config holds it",
	"internal/engine/catalog.appWait.steps":                  "a step of the app wait, called through its waiter interface: the release, namespace and cluster are the ones the command resolved, and the operation's name and namespace the ones the server sent",
	"internal/engine/kinds.operationStalledRemedy":           "the StalledRemedy of a kinds table row, called as a function value with the observed object and --cluster as typed",
	"internal/engine/provisioning.scaleCmd.Plan":             "a function literal planClusterFamilyMutation calls with the cluster object the server sent and its name",
	"internal/runtime.IO.ExpandText":                         "a method of the invocation's streams, called through an interface: RemedyDir is the config directory the invocation opens, set by the CLI layer",
	"internal/runtime.IO.ExpandLines":                        "a method of the invocation's streams, called through an interface: RemedyDir is the config directory the invocation opens, set by the CLI layer",
	"internal/token.tokenCmd.Execute":                        "the command's Execute, called through the runtime.Command interface: the receiver's user check holds the server, cluster, auth config and CA source the command resolved from the config and the typed line",
}

// evOriginSites are single calls guard 3a accepts between a value and the
// builder, each with its reason.
var evOriginSites = map[evOriginSite]string{
	{"internal/token.legacyTokenCmd.Execute", "strings.TrimSpace"}:               "the CLI acts on the trimmed --cluster value, so the command names what it acted on",
	{"internal/cli.boolValueRefusal", "strings.TrimSpace"}:                       "the CLI acts on the trimmed joined bool value, so the hint names what it read",
	{"internal/cli.typedWord", "strings.Cut"}:                                    "splits a typed --flag=value word into the flag and the value the parser read",
	{"internal/config.ErrNotLoggedInFor", "strings.TrimLeft"}:                    "the entry name a login of the host gets: auth login creates it under the name without a leading dash, which would read as a flag",
	{"internal/engine/provisioning.resolveCredentialObj", "strings.TrimPrefix"}:  "the namespaced id form names the same Secret; the command names the Secret by the name the lookup used",
	{"internal/engine/kubeconfig.kubeconfigCmd.Execute", "canonicalDir"}:         "config.CanonicalDir held in a variable: the absolute config directory the exec entry names, so kubectl's working directory never chooses another one",
	{"internal/engine/provisioning.createMachineConfigCmd.build", "strings.Cut"}: "the --set key as the parser splits key=value; the refusal names that key",
}

// Keys of the whole-text filters, for the rows below.
const (
	evPrintable         = "internal/output.Printable"
	evPrintableLines    = "internal/output.PrintableLines"
	evPrintableRemedies = "internal/runtime.PrintableRemedies"
	evWithoutUnsafe     = "internal/clierr.WithoutUnsafeToDisplay"
	evValuesFilter      = "internal/engine/catalog.Values.Filter"
	evValuesFilterError = "internal/engine/catalog.Values.FilterError"
	evValuesFilterFunc  = "internal/engine/catalog.Values.filterFunc"
)

// evFilterRows is every function that references a whole-text filter, with
// whether the filtered text may hold a command and, for a row whose text
// holds none, why a relayed text the walk reaches holds none.
var evFilterRows = []evFilterRow{
	{"internal/cli.completionDebugf", evPrintableRemedies, true, "", "a debug line of the completion trace, which may relay a remedy"},
	{"internal/commands/auth.credentialReport", evPrintable, false, "", "the auth config and the facts of a stored credential, named in prose"},
	{"internal/commands/auth.renderAuthCheckText", evPrintable, false, "", "the server version and the identity the server reports, named in prose"},
	{"internal/commands/auth.trustCard", evPrintable, false, "", "the lines of the trust card: the server's certificate facts"},
	{"internal/commands/configcmd.entrySection", evPrintable, false, "the stored text was written by an earlier process; it holds no remedy marker of this process, so nothing in it is a command this process expands", "the fields of a server entry as the config holds them"},
	{"internal/commands/configcmd.recordLines", evPrintable, false, "the stored text was written by an earlier process; it holds no remedy marker of this process, so nothing in it is a command this process expands", "a superseded token's reference and its last revoke attempt as the config holds them"},
	{"internal/commands/configcmd.tokenSection", evPrintable, false, "", "a token reference as the config holds it"},
	{"internal/engine/access.canText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.canText", evPrintableRemedies, true, "", "the note of an access answer, which can hold a remedy"},
	{"internal/engine/access.clustersText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.clustersText", evPrintableRemedies, true, "", "the message of a stage that could not be read, which can hold a remedy"},
	{"internal/engine/access.explainText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.explainText", evPrintableRemedies, true, "", "the note of an access answer, which can hold a remedy"},
	{"internal/engine/access.grantNames", evPrintable, false, "", "the names of grants the server lists"},
	{"internal/engine/access.listText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.listText", evPrintableRemedies, true, "", "the note of an access answer, which can hold a remedy"},
	{"internal/engine/access.listWideText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.listWideText", evPrintableRemedies, true, "", "the note and the issues of an access answer, which can hold a remedy"},
	{"internal/engine/access.printableAll", evPrintableRemedies, true, "", "the lines of an access answer, which can hold a remedy"},
	{"internal/engine/access.projectsText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.projectsText", evPrintableRemedies, true, "", "the note and the issues of an access answer, which can hold a remedy"},
	{"internal/engine/access.renderTree", evPrintableRemedies, true, "", "the note of a node of the access chain, which can hold a remedy"},
	{"internal/engine/access.whoCanText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/access.whoCanText", evPrintableRemedies, true, "", "the note of an access answer, which can hold a remedy"},
	{"internal/engine/catalog.Values.Filter", evValuesFilterFunc, true, "", "the values filter itself"},
	{"internal/engine/catalog.Values.FilterError", evValuesFilter, true, "", "the values filter over an error's message and hint"},
	{"internal/engine/catalog.appWait.clean", evPrintable, false, "", "the condition and status messages a wait reads from the server"},
	{"internal/engine/catalog.installCmd.execute", evValuesFilterFunc, true, "", "the values filter handed to the install wait"},
	{"internal/engine/catalog.outcomeUnknown", evValuesFilter, true, "", "the values filter over the server's message"},
	{"internal/engine/catalog.postAction", evValuesFilterError, true, "", "the values filter over the error of an action"},
	{"internal/engine/catalog.refused", evValuesFilterFunc, true, "", "the values filter over the body of a refused action"},
	{"internal/engine/catalog.uninstallCmd.Plan", evValuesFilterFunc, true, "", "the values filter handed to the uninstall wait"},
	{"internal/engine/catalog.upgradeCmd.execute", evValuesFilterFunc, true, "", "the values filter handed to the upgrade wait"},
	{"internal/engine/extres.createTokenCmd.Plan", evPrintable, false, "", "the expiry and the name of the created token, named in prose"},
	{"internal/engine/inspect.clusterCmd.Execute", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderAgentLine", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderClusterText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderConditionsSection", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderDeploymentText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderMachinePoolsSection", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderMachinesSection", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderMembersSection", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderProjectText", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderRoleRow", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderUserGroups", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.renderUserIdentity", evPrintable, false, "", "names, states and messages the server sent, named in prose; no command is built from them here"},
	{"internal/engine/inspect.scopeLabel", evPrintable, false, "", "a scope id the server sent"},
	{"internal/engine/inspect.writeRegistrationField", evPrintable, false, "", "a registration value the server sent"},
	{"internal/engine/inspect.writeSortedMap", evPrintable, false, "", "the keys and values of a label or annotation map the server sent"},
	{"internal/engine/kubeconfig.installHint", evPrintable, false, "the commands are file text built with FileCommandIn, which holds no remedy marker, and their values passed NotQuotable, which refuses every character Printable removes", "the install hint written into a kubeconfig file: the exec command's path and the regenerate commands"},
	{"internal/engine/kubeconfig.kubeconfigCmd.Execute", evPrintableRemedies, true, "", "the reasons an entry was not written, which can hold a remedy"},
	{"internal/engine/kubeconfig.lookupCall.recordedOwner", evPrintableRemedies, true, "", "a notice that holds a remedy"},
	{"internal/engine/kubeconfig.lookupNotice", evPrintableRemedies, true, "", "a notice that holds a remedy"},
	{"internal/engine/kubeconfig.machineNotice", evPrintable, false, "", "a config directory recorded in a kubeconfig, named in prose"},
	{"internal/engine/oidcclient.addSecretCmd.execute", evPrintable, false, "", "the name of the added secret, named in prose"},
	{"internal/engine/oidcclient.createCmd.execute", evPrintable, false, "", "the client id and name the server returned, named in prose"},
	{"internal/engine/oidcclient.rotateSecretCmd.execute", evPrintable, false, "", "the name of the regenerated secret, named in prose"},
	{"internal/engine/projects.removeNamespaceCmd.Plan", evPrintable, false, "", "the name of the project a namespace was removed from"},
	{"internal/engine/resources.summarizeState", evPrintable, false, "", "a condition message the server sent"},
	{"internal/engine/resources.waitFor", evPrintable, false, "", "the stalled message the server sent"},
	{"internal/output.Printable", evWithoutUnsafe, false, "", "the filter itself"},
	{"internal/output.renderName", evPrintable, true, "", "the name writer of result data, which is not a remedy channel"},
	{"internal/output.writePlainTable", evPrintable, true, "", "the table and wide writer of result data, which is not a remedy channel"},
	{"internal/remedytest.AssertIDShownWithEscapes", evWithoutUnsafe, false, "", "a test helper that compares an id with its filtered form"},
	{"internal/runtime.ObjectLines", evPrintable, false, "", "the lines of an object's details"},
	{"internal/runtime.OnceOnlyLine", evPrintable, false, "", "compares a value with its filtered form; no text is printed from it"},
	{"internal/runtime.PrintableRemedies", evPrintableLines, true, "", "the filter itself"},
	{"internal/runtime.RenderErrorStyled", evPrintable, false, "", "the warnings of an error"},
	{"internal/runtime.WriteError", evPrintable, false, "the renderer filters the message after the invocation expanded every remedy in it, so no marker is left to remove", "the fallback line of an error whose render failed"},
	{"internal/runtime.detailLineText", evPrintable, false, "", "a details line, each value shown with its hidden characters as escapes"},
	{"internal/runtime.dryRunText", evPrintable, false, "", "the changes a dry run reports, field paths and values"},
	{"internal/runtime.dryRunText", evPrintableRemedies, true, "", "the server and the lines of a dry run, which can hold a remedy"},
	{"internal/runtime.errorText", evPrintable, false, "the renderer filters the message and the hint after the invocation expanded every remedy in them, so no marker is left to remove", "the default text of an error"},
	{"internal/runtime.mirrorWarnings", evPrintable, false, "", "the warnings an invocation mirrors to stderr"},
	{"internal/runtime.printableText", evPrintableLines, true, "", "the text of a result, which can hold a remedy"},
	{"internal/runtime.renderEffect", evPrintableLines, true, "", "the text of a dry run, which can hold a remedy"},
	{"internal/runtime.renderResult", evPrintableRemedies, true, "", "the summary of a result, which can hold a remedy"},
	{"internal/token.Deletion.textLine", evPrintable, false, "", "the token reference, named in prose"},
	{"internal/token.Deletion.textLine", evPrintableRemedies, true, "", "the reason a token was not revoked, which can hold a remedy"},
	{"internal/token.certificateFailureLines", evPrintable, false, "", "the lines of a certificate failure: the certificate's facts"},
	{"internal/token.certificateFailureLines", evPrintableRemedies, true, "", "the message of a certificate failure, which can hold a remedy"},
	{"internal/token.chooseAuthConfig", evPrintable, false, "", "the labels of the auth configs the server lists"},
	{"internal/token.legacyFallbackNotice", evPrintable, false, "", "the expiry of a token, named in prose"},
	{"internal/token.oauthAuthCodeAuth", evPrintable, false, "", "the sign-in URL the server sent"},
	{"internal/token.oauthDeviceCodeAuth", evPrintable, false, "", "the verification URL and code the server sent"},
	{"internal/token.samlAuth", evPrintableRemedies, true, "", "relays the message of a failed delete, which can hold a remedy"},
	{"internal/token.selectAuthProvider", evPrintable, false, "", "the label of the auth config the server lists, and its note"},
	{"internal/token.signInNotice", evPrintable, false, "", "the label of the chosen auth config"},
	{"internal/token.tokenCmd.classifyRejection", evPrintableRemedies, true, "", "relays the message of the rejection, which can hold a remedy"},
	{"internal/ui.Prompter.Notice", evPrintableLines, true, "", "a notice, which can hold a remedy"},
	{"internal/ui.Prompter.writeErr", evPrintableLines, true, "", "text written to stderr, which can hold a remedy"},
}

// evCommandRows are the guard 4 sites that stay as they are, each with its
// reason.
var evCommandRows = []evCommandRow{
	{"4.3", "internal/config.removeEntriesText", "a list of commands joined in a loop; every element appended is a string, a Lead or a Command, and the site's test asserts the text"},
	{"4.3", "internal/config.Unpinned.Err", "a list of commands joined in a loop; every element appended is a string, a Lead or a Command, and the site's test asserts the text"},
	{"4.3", "internal/engine/catalog.outcomeUnknown", "a list of commands joined in a loop; every element appended is a string, a Lead or a Command, and the site's test asserts the text"},
	{"4.3", "internal/engine/catalog.installPlan.checkNoApp", "a list of commands joined in a loop; every element appended is a string, a Lead or a Command, and the site's test asserts the text"},
	{"4.3", "internal/engine/user.notBoundClauses", "a list of commands joined in a loop; every element appended is a string, a Lead or a Command, and the site's test asserts the text"},
	{"4.5", evTypedLineUse, "the line refusal hint: every word it prints passed ShellWord or NotQuotable, and the hint is file text with a plain rancher"},
	{"4.6", "internal/cli.hintLine", "returns the TypedLine's Field, the hint of the line refusal, which passes through BoolValueError's line unchanged"},
	{"4.5", "internal/commands/auth.logoutCmd.Execute", "two independent alternatives (log out of one server, or of all), constant commands with no value"},
	{"4.5", "internal/commands/auth.logoutTargets", "two independent alternatives (log out of one server, or of all), constant commands with no value"},
	{"4.5", "internal/clierr.MarkRemedy", "writes the marker before a command built elsewhere; its argument is the whole command"},
	{"4.6", "internal/clierr.MarkRemedy", "writes the marker before a command built elsewhere; its argument is the whole command"},
	{"4.6", "internal/engine/extres.kubeconfigHeader", "the header of a kubeconfig file: the revoke goes through clierr.Text, and the header of a left-out revoke names only its list command, which takes no value"},
	{"4.5", "internal/remedytest.markedLeaves", "a helper of tests that builds marked text to assert the expansion; it is never printed"},
	{"4.6", "internal/remedytest.markedLeaves", "a helper of tests that builds marked text to assert the expansion; it is never printed"},
}

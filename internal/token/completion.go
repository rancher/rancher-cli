package token

import (
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	tokencomplete "github.com/rancher/rancher-cli/internal/token/complete"
)

// LiveSource implements completion.Completer: --auth-provider completes
// the names of the server's auth configs.
func (tokenCmd) LiveSource() completion.Source { return tokencomplete.AuthConfigSource() }

// CompletionTarget is the server and CA auth get-token would connect with
// for the line being completed, over cfg: --server, else RANCHER_CLI_URL,
// else the current server entry; --name only identifies the entry whose CA
// is used. A --name that identifies an entry at another URL, or an
// unreadable CA, gives none, as the command refuses both.
func (tokenCmd) CompletionTarget(cfg *config.Config, req completion.Request) (server, caPEM string, ok bool) {
	return completionTarget(cfg, req, "cacert", false)
}

// LiveSource implements completion.Completer: --auth-provider completes
// the names of the server's auth configs.
func (legacyTokenCmd) LiveSource() completion.Source { return tokencomplete.AuthConfigSource() }

// CompletionTarget is the server and CA the legacy token command would
// connect with for the line being completed: --server, which it requires,
// never the current server entry, and --cacerts. It gives none with
// --skip-verify, with which the command does not verify the server.
func (legacyTokenCmd) CompletionTarget(cfg *config.Config, req completion.Request) (server, caPEM string, ok bool) {
	if tailBool(req.Tail, legacyTokenCmd{}.Describe().Flags, "skip-verify") {
		return "", "", false
	}
	return completionTarget(cfg, req, "cacerts", true)
}

// completionTarget answers CompletionTarget for either command through
// connectionTarget, the choice the commands themselves make.
func completionTarget(cfg *config.Config, req completion.Request, caFlag string, legacy bool) (server, caPEM string, ok bool) {
	server, ca, _, _, err := connectionTarget(cfg, strings.TrimSpace(completion.TailFlag(req.Tail, "server")), completion.TailFlag(req.Tail, "cluster"),
		strings.TrimSpace(completion.TailFlag(req.Tail, caFlag)), completion.TailFlag(req.Tail, "name"), legacy, false)
	if err != nil {
		return "", "", false
	}
	caPEM, err = config.ResolveCA(ca)
	if err != nil {
		return "", "", false
	}
	return server, caPEM, true
}

// tailBool reports whether the bool flag name is set on the line: a
// completion.TailFlag cannot see one, since it takes the next word as the
// flag's value. A flag is read with one or two leading dashes and by its
// aliases, as the command line itself is; value-taking flags of specs are
// skipped with their value, and "--" ends the flags.
func tailBool(tail []string, specs []runtime.ArgSpec, name string) bool {
	set := false
	for i := 0; i < len(tail); i++ {
		arg := tail[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		flag, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		var spec *runtime.ArgSpec
		for j := range specs {
			if specs[j].Name == flag || slices.Contains(specs[j].Aliases, flag) {
				spec = &specs[j]
			}
		}
		switch {
		case spec == nil:
		case spec.Type == "bool" && spec.Name == name:
			set = !hasValue || value == "true"
		case spec.Type != "bool" && !hasValue:
			i++
		}
	}
	return set
}

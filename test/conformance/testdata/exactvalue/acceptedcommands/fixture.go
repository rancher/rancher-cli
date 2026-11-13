// Package acceptedcommands is a fixture of the exact-value scans.
package acceptedcommands

import (
	"fmt"
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// state picks between two constant words.
func state(on bool) clierr.Command {
	w := clierr.CommandWord("off")
	if on {
		w = "on"
	}
	return clierr.NewCommand("config set-token-reuse").Word(w)
}

// kind writes words from the kinds table.
func kind(id string) clierr.Command {
	k, ok := kinds.Lookup("user")
	if !ok {
		return clierr.Command{}
	}
	return clierr.NewCommand("get").Word(k.Word()).Arg(id)
}

// text puts a command after a Lead and a revoke after prose.
func text(name string, ref clierr.TokenRef) string {
	c := clierr.NewCommand("get users").Arg(name)
	return clierr.Text("the user "+clierr.ShellQuote(name)+" is gone", clierr.Lead("; list them with: "), c, "; it could not be revoked", clierr.Lead("; revoke it with: "), clierr.Revoke(ref))
}

// helper returns a text built by Text.
func helper(name string) string {
	return clierr.Text("no user", clierr.Lead("; list them with: "), clierr.NewCommand("get users").Arg(name))
}

// sequence returns steps as one Sequence.
func sequence(entry, cluster string) *clierr.Error {
	s := clierr.NewSequence().
		Then("", clierr.NewCommand("config use-server").Arg(entry)).
		Then("then ", clierr.NewCommand("kubeconfig").FlagValue("--cluster", cluster))
	return clierr.New("FIXTURE", "run: "+s.String())
}

// independent lists one command per entry, under an allowlist row.
func independent(a, b string) *clierr.Error {
	return clierr.New("FIXTURE", clierr.NewCommand("config delete-server").Arg(a).String()+"; "+clierr.NewCommand("config delete-server").Arg(b).String())
}

// fragment asks for a flag value through a fragment.
func fragment(provider string) *clierr.Error {
	frag, ok := clierr.FlagFragmentWord("--auth-provider", provider)
	if !ok {
		return clierr.New("FIXTURE", clierr.WithLeftOutNote("the login needs a provider"))
	}
	return clierr.New("FIXTURE", "the login needs a provider; add "+frag)
}

// subject names a flag value in prose, then asks for another flag.
func subject(flag clierr.CommandWord, v, noun string) *clierr.Error {
	s, ok := clierr.FlagFragment(flag, v)
	if !ok {
		return clierr.New("FIXTURE", clierr.WithLeftOutNote("the value is ambiguous"))
	}
	msg := fmt.Sprintf("%s is the id of %s", s, noun)
	if frag, ok := clierr.FlagFragment("--namespace", v); ok {
		msg += "; run it with " + frag
	}
	return clierr.New("FIXTURE", msg)
}

// hint sets a hint from a built command.
func hint(name string) *clierr.Error {
	return clierr.New("NOT_FOUND", "no user").WithHintCommand(clierr.NewCommand("get users").Arg(name))
}

// recovery sets a command-only field from Field.
func recovery(entry string) clierr.ConfigRecovery {
	return clierr.ConfigRecovery{Entry: entry, Command: clierr.NewCommand("config delete-server").Arg(entry).Field()}
}

// prose names a quoted value in prose.
func prose(name string) string {
	return fmt.Sprintf("the user %s is gone; nothing else changed", clierr.ShellQuote(name))
}

// flagText writes constant flag text after a command.
func flagText() *clierr.Error {
	return clierr.New("FIXTURE", clierr.NewCommand("kubeconfig").FlagText(" --cluster <id>").String()+"; pass the id of the cluster")
}

package builder

import (
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func newCommand() string { return fmt.Sprintf("%q", clierr.NewCommand("get users").String()) }

func fileCommand() string { return strconv.Quote(clierr.FileCommand("get users").String()) }

func chained(name string) string {
	return strconv.Quote(clierr.NewCommand("get users").Arg(name).Field())
}

func localCommand(name string) string {
	cmd := clierr.NewCommand("get users").Arg(name)
	return fmt.Sprintf("%q", cmd.String())
}

func sequence() string { return strconv.Quote(clierr.NewSequence().String()) }

func typedLine() string { return strconv.Quote(clierr.NewTypedLine().Field()) }

func fragment(name string) string {
	f, _ := clierr.FlagFragment("--cluster", name)
	return strconv.Quote(f)
}

func shellWord(name string) string {
	w, _ := clierr.ShellWord(name)
	return strconv.Quote(w)
}

func text(c clierr.Command) string { return strconv.Quote(clierr.Text("run: ", c)) }

func commandToPlainVerb(name string) string {
	return fmt.Sprintf("%s %q", clierr.NewCommand("get users").String(), name)
}

func plainNote(name string) string { return strconv.Quote(clierr.LeftOutNote + name) }

func fileCommandIn() string { return strconv.Quote(clierr.FileCommandIn("dir", "get users").String()) }

func revoke(name string) string {
	return strconv.Quote(clierr.Revoke(clierr.TokenRef{Name: name}).String())
}

func revokeKubeconfig(name string) string {
	return strconv.Quote(clierr.RevokeKubeconfig(name).String())
}

func fileRevokeKubeconfig(name string) string {
	return strconv.Quote(clierr.FileRevokeKubeconfig(name).String())
}

func fragmentText() string { return strconv.Quote(clierr.FlagFragmentText("--set", "name=<value>")) }

func fragmentWord(name string) string {
	f, _ := clierr.FlagFragmentWord("--cluster", name)
	return strconv.Quote(f)
}

func fragmentAfterWord(name string) string {
	f, _ := clierr.FlagFragmentAfterWord("--name", "<entry>-", name)
	return strconv.Quote(f)
}

func directoryFree() string { return strconv.Quote(clierr.DirectoryFree("auth --help")) }

func markRemedy() string { return fmt.Sprintf("%q", clierr.MarkRemedy("rancher get users")) }

func remedyMarker() string { return strconv.Quote(clierr.RemedyMarker + "rancher get users") }

// The shapes the node command text scan is checked on: each function above
// the line that says so composes text from a node command, or writes a
// composed command other than from ComposeNodeCommand; the functions after
// it do neither.
package nodecommandtext

import (
	"fmt"
	"io"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/provisioning"
)

// withOptions stands for the type that carries the composed commands.
type withOptions struct {
	NodeCommand        string
	WindowsNodeCommand string
}

// other has the same field names and is not matched.
type other struct{ NodeCommand string }

func plusChain(o provisioning.CRTObject) withOptions {
	var w withOptions
	w.NodeCommand = o.NodeCommand + " --worker"
	return w
}

func plusFromLocal(o provisioning.CRTObject) withOptions {
	var w withOptions
	s := o.NodeCommand
	w.NodeCommand = s + " --worker"
	return w
}

func plusAssign(o provisioning.CRTObject) string {
	s := "x"
	s += o.NodeCommand
	return s
}

func sprintf(o provisioning.CRTObject) withOptions {
	var w withOptions
	w.NodeCommand = fmt.Sprintf("%s --worker", o.NodeCommand)
	return w
}

func fprintf(out io.Writer, o *provisioning.CRTObject) {
	_, _ = fmt.Fprintf(out, "%s --worker", o.InsecureNodeCommand)
}

func join(o provisioning.CRTObject) string {
	return strings.Join([]string{o.NodeCommand, "--worker"}, " ")
}

func builder(o provisioning.CRTObject) withOptions {
	var b strings.Builder
	b.WriteString(o.NodeCommand)
	var w withOptions
	w.NodeCommand = b.String()
	return w
}

func builderBytes(o provisioning.CRTObject) string {
	var b strings.Builder
	_, _ = b.Write([]byte(o.InsecureWindowsNodeCommand))
	b.WriteString(o.WindowsNodeCommand)
	return b.String()
}

func literal(o provisioning.CRTObject) withOptions {
	return withOptions{NodeCommand: o.NodeCommand}
}

func windowsField(o provisioning.CRTObject) withOptions {
	var w withOptions
	w.WindowsNodeCommand = o.WindowsNodeCommand + " -NodeName 'a'"
	return w
}

func composeThenAppended(o provisioning.CRTObject, v provisioning.ValidNodeCommandOptions) withOptions {
	var w withOptions
	var r provisioning.ComposeResult
	w.NodeCommand, r = provisioning.ComposeNodeCommand(o.NodeCommand, v)
	_ = r
	w.NodeCommand += " --worker"
	return w
}

var packageValue = func(o provisioning.CRTObject) string { return o.NodeCommand + " --worker" }

// The functions below compose no text from a node command.

func composed(o provisioning.CRTObject, v provisioning.ValidNodeCommandOptions) withOptions {
	var w withOptions
	var r provisioning.ComposeResult
	w.NodeCommand, r = provisioning.ComposeNodeCommand(o.NodeCommand, v)
	if r != provisioning.Composed {
		w.WindowsNodeCommand, _ = provisioning.ComposeNodeCommand(o.WindowsNodeCommand, v)
	}
	return w
}

func copies(o provisioning.CRTObject) (other, bool) {
	r := other{NodeCommand: o.NodeCommand}
	return r, o.NodeCommand == ""
}

func otherType(r other) string { return r.NodeCommand + " --worker" }

func emptyLiteral(w withOptions) bool { return w == withOptions{} }

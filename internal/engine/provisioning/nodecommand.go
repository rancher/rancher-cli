package provisioning

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The names of the flags that add options to a node command.
const (
	NodeOptionEtcd            = "etcd"
	NodeOptionControlPlane    = "controlplane"
	NodeOptionWorker          = "worker"
	NodeOptionNodeName        = "node-name"
	NodeOptionAddress         = "address"
	NodeOptionInternalAddress = "internal-address"
	NodeOptionLabel           = "label"
	NodeOptionTaint           = "taint"
	NodeOptionWindows         = "windows"
)

// NodeOptionFlags lists every node command option flag. A refusal that names
// one option flag names the first one set in this order, and the typed values
// are checked in this order.
var NodeOptionFlags = []string{
	NodeOptionEtcd, NodeOptionControlPlane, NodeOptionWorker,
	NodeOptionNodeName, NodeOptionAddress, NodeOptionInternalAddress,
	NodeOptionLabel, NodeOptionTaint, NodeOptionWindows,
}

// NodeCommandOptions are the node command options as typed: Labels and
// Taints have one entry per occurrence of the flag, in the order typed.
type NodeCommandOptions struct {
	Etcd, ControlPlane, Worker bool
	NodeName                   string
	Address                    string
	InternalAddress            string
	Labels                     []string
	Taints                     []string
	Windows                    bool
}

// SetFlags returns the names of the option flags o sets, in NodeOptionFlags
// order.
func (o NodeCommandOptions) SetFlags() []string {
	set := map[string]bool{
		NodeOptionEtcd:            o.Etcd,
		NodeOptionControlPlane:    o.ControlPlane,
		NodeOptionWorker:          o.Worker,
		NodeOptionNodeName:        o.NodeName != "",
		NodeOptionAddress:         o.Address != "",
		NodeOptionInternalAddress: o.InternalAddress != "",
		NodeOptionLabel:           len(o.Labels) > 0,
		NodeOptionTaint:           len(o.Taints) > 0,
		NodeOptionWindows:         o.Windows,
	}
	var out []string
	for _, f := range NodeOptionFlags {
		if set[f] {
			out = append(out, f)
		}
	}
	return out
}

// ValidNodeCommandOptions are options ValidateNodeCommandOptions accepted.
// ComposeNodeCommand takes only this type: no unchecked value reaches a
// composed command.
type ValidNodeCommandOptions struct {
	o NodeCommandOptions
}

// Windows reports whether the options compose the Windows node command.
func (v ValidNodeCommandOptions) Windows() bool { return v.o.Windows }

// rancherNodeLabelKeys are the node label keys Rancher sets on a node that
// joins with the node command: Rancher's node command passes cattle.io/os, and
// Rancher adds the role, node name, machine and cluster labels and the plan
// labels. A typed label with one of these keys is refused: Rancher keeps its
// own value on the machine, while the node would get the typed one. The list
// covers every supported Rancher minor; a key only newer minors set is refused
// on older ones too.
var rancherNodeLabelKeys = []string{
	"cattle.io/os",
	"rke.cattle.io/cluster-name",
	"rke.cattle.io/control-plane-role",
	"rke.cattle.io/etcd-role",
	"rke.cattle.io/machine-id",
	"rke.cattle.io/machine",
	"rke.cattle.io/node-name",
	"rke.cattle.io/worker-role",
	"cluster.x-k8s.io/control-plane",
	"cluster.x-k8s.io/cluster-name",
	"plan.cattle.io/cluster-group",
	"plan.cattle.io/cluster-kind",
	"plan.cattle.io/cluster-name",
	"plan.cattle.io/machine-group",
	"plan.cattle.io/machine-kind",
	"plan.cattle.io/machine-name",
}

// taintEffects are the taint effects a typed taint may name.
var taintEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

// The characters each typed value may have: the characters the Rancher UI
// keeps in the same field. The install script and Rancher split a label or
// taint at ",", "=" and ":", and the script reads an address unquoted; no
// other character is accepted. Every set is ASCII; a value outside it is
// refused, never changed.
func inKeySet(r rune) bool     { return inValueSet(r) || r == '/' }
func inAddressSet(r rune) bool { return inValueSet(r) || r == ':' }
func inValueSet(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
}

// The characters each set accepts, as a refusal names them.
const (
	keyChars     = `letters, digits, ".", "/", "_" or "-"`
	valueChars   = `letters, digits, ".", "_" or "-"`
	addressChars = `letters, digits, ".", ":", "_" or "-", with "," between addresses`
)

// firstOutside returns the first character of s that inSet refuses: one
// whole rune, or one byte that is not UTF-8.
func firstOutside(s string, inSet func(rune) bool) (string, bool) {
	for i, r := range s {
		if r >= utf8.RuneSelf || !inSet(r) {
			_, size := utf8.DecodeRuneInString(s[i:])
			return s[i : i+size], true
		}
	}
	return "", false
}

// optionError is a VALIDATION refusal of one typed value for reason. The
// value is printed with every non-ASCII rune, control byte and invalid byte
// escaped.
func optionError(flag, value, reason string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %+q: %s", flag, value, reason))
}

// checkChars refuses a part of a typed value that has a character outside its
// set.
func checkChars(flag, value, part, s string, inSet func(rune) bool, allowed string) error {
	if c, bad := firstOutside(s, inSet); bad {
		return optionError(flag, value, fmt.Sprintf("%s has %+q, which the Rancher UI would remove; use %s", part, c, allowed))
	}
	return nil
}

// kubernetesRefusal refuses a part of a typed value that breaks the Kubernetes
// rule for that kind of value.
func kubernetesRefusal(flag, value, part, kind string, msgs []string) error {
	if len(msgs) == 0 {
		return nil
	}
	return optionError(flag, value, fmt.Sprintf("%s breaks the Kubernetes rule for a %s: %s", part, kind, strings.Join(msgs, "; ")))
}

// ValidateNodeCommandOptions checks the typed options, flag by flag in
// NodeOptionFlags order; the first refusal is returned, a VALIDATION error
// whose message starts with the flag. A value is accepted only as typed: no
// character is ever removed or changed.
func ValidateNodeCommandOptions(o NodeCommandOptions) (ValidNodeCommandOptions, error) {
	checks := []func() error{
		func() error { return roleWithWindows(NodeOptionEtcd, o.Etcd, o.Windows) },
		func() error { return roleWithWindows(NodeOptionControlPlane, o.ControlPlane, o.Windows) },
		func() error { return roleWithWindows(NodeOptionWorker, o.Worker, o.Windows) },
		func() error { return checkNodeName(o.NodeName) },
		func() error { return checkAddress(NodeOptionAddress, o.Address) },
		func() error { return checkAddress(NodeOptionInternalAddress, o.InternalAddress) },
		func() error { return checkLabels(o.Labels, o.Windows) },
		func() error { return checkTaints(o.Taints, o.Windows) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return ValidNodeCommandOptions{}, err
		}
	}
	return ValidNodeCommandOptions{o: o}, nil
}

// roleWithWindows refuses a role flag with --windows: Rancher's Windows node
// command already ends in -Worker, and a Windows node is only a worker.
func roleWithWindows(flag string, set, windows bool) error {
	if set && windows {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s conflicts with --%s; remove --%s (Windows nodes can only be workers)", flag, NodeOptionWindows, flag))
	}
	return nil
}

func checkNodeName(v string) error {
	if v == "" {
		return nil
	}
	if err := checkChars(NodeOptionNodeName, v, "the node name", v, inValueSet, valueChars); err != nil {
		return err
	}
	if len(validation.IsDNS1123Subdomain(v)) > 0 {
		return optionError(NodeOptionNodeName, v, `this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`)
	}
	return nil
}

// checkAddress accepts one address or several separated by ",", each
// non-empty.
func checkAddress(flag, v string) error {
	if v == "" {
		return nil
	}
	for piece := range strings.SplitSeq(v, ",") {
		if piece == "" {
			return optionError(flag, v, `the list has an empty address; separate addresses with one ","`)
		}
		if err := checkChars(flag, v, "an address", piece, inAddressSet, addressChars); err != nil {
			return err
		}
	}
	return nil
}

// checkKey checks the key of a label or a taint: the characters, then
// Kubernetes' label key rule, which Kubernetes also applies to a taint key.
func checkKey(flag, v, key, shape string) error {
	if key == "" {
		return optionError(flag, v, fmt.Sprintf("the key is empty; give the %s as %s", flag, shape))
	}
	if err := checkChars(flag, v, "the key", key, inKeySet, keyChars); err != nil {
		return err
	}
	return kubernetesRefusal(flag, v, "the key", flag+" key", content.IsLabelKey(key))
}

// checkValue checks the value of a label or a taint: the characters, then
// Kubernetes' label value rule, which Kubernetes also applies to a taint
// value.
func checkValue(flag, v, value, shape string) error {
	if value == "" {
		return optionError(flag, v, fmt.Sprintf("the value is empty; give the %s as %s", flag, shape))
	}
	if err := checkChars(flag, v, "the value", value, inValueSet, valueChars); err != nil {
		return err
	}
	return kubernetesRefusal(flag, v, "the value", flag+" value", content.IsLabelValue(value))
}

const (
	labelShape = "key=value"
	taintShape = "key=value:effect"
)

// checkLabels checks every typed label, split at the first "=". With
// --windows no label is accepted: Rancher's Windows node command already
// passes -Label, and PowerShell refuses a parameter bound twice.
func checkLabels(labels []string, windows bool) error {
	if len(labels) > 0 && windows {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s conflicts with --%s; remove --%s (the Windows node command already passes -Label)", NodeOptionLabel, NodeOptionWindows, NodeOptionLabel))
	}
	seen := map[string]bool{}
	for _, v := range labels {
		key, value, found := strings.Cut(v, "=")
		if !found {
			return optionError(NodeOptionLabel, v, `the label has no "="; give it as `+labelShape)
		}
		if err := checkKey(NodeOptionLabel, v, key, labelShape); err != nil {
			return err
		}
		if err := checkValue(NodeOptionLabel, v, value, labelShape); err != nil {
			return err
		}
		if slices.Contains(rancherNodeLabelKeys, key) {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %s: Rancher sets this label on the node itself", NodeOptionLabel, key))
		}
		if seen[key] {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %s is given twice", NodeOptionLabel, key))
		}
		seen[key] = true
	}
	return nil
}

// checkTaints checks every typed taint: the key before the first "=", the
// effect after the last ":", the value between them. With --windows one taint
// is accepted: the Windows node command binds -Taint once.
func checkTaints(taints []string, windows bool) error {
	if len(taints) > 1 && windows {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s given more than once conflicts with --%s; remove all but one --%s", NodeOptionTaint, NodeOptionWindows, NodeOptionTaint))
	}
	for _, v := range taints {
		key, rest, found := strings.Cut(v, "=")
		if !found {
			return optionError(NodeOptionTaint, v, `the taint has no "="; give it as `+taintShape)
		}
		value, effect, found := strings.CutLast(rest, ":")
		if !found {
			return optionError(NodeOptionTaint, v, `the taint has no ":"; give it as `+taintShape)
		}
		if err := checkKey(NodeOptionTaint, v, key, taintShape); err != nil {
			return err
		}
		if err := checkValue(NodeOptionTaint, v, value, taintShape); err != nil {
			return err
		}
		if effect == "" {
			return optionError(NodeOptionTaint, v, "the effect is empty; use "+effectList)
		}
		if !slices.Contains(taintEffects, effect) {
			return optionError(NodeOptionTaint, v, fmt.Sprintf("the effect is %+q; use %s", effect, effectList))
		}
	}
	return nil
}

const effectList = "NoSchedule, PreferNoSchedule or NoExecute"

// ComposeNodeCommand returns base, Rancher's node command as the server
// returned it, unchanged, followed by one space and the options in the order
// the Rancher UI adds them. Linux: the roles (all three when no role flag is
// set), --address, --internal-address, --node-name, each --label, and each
// taint as --taints (the name the Rancher UI prints), in the order typed.
// Windows: -Address, -InternalAddress, -NodeName and -Taint; no role and no
// -Label, which Rancher's Windows node command already has. Every value is
// single-quoted by clierr.FlagFragment and every option name is a constant.
// With --windows and no other option, base is returned unchanged. When no
// command is composed the text is empty and the result says why:
// BaseNotFormatted for an empty node command or one that still has the token
// placeholder, ValueNotQuotable for a value FlagFragment refuses; no value
// ValidateNodeCommandOptions accepts is one.
func ComposeNodeCommand(base string, v ValidNodeCommandOptions) (string, ComposeResult) {
	if base == "" || strings.Contains(base, placeholderToken) {
		return "", BaseNotFormatted
	}
	o := v.o
	var words []string
	ok := true
	add := func(fragment string, quoted bool) {
		words = append(words, fragment)
		ok = ok && quoted
	}
	if o.Windows {
		if o.Address != "" {
			add(clierr.FlagFragment("-Address", o.Address))
		}
		if o.InternalAddress != "" {
			add(clierr.FlagFragment("-InternalAddress", o.InternalAddress))
		}
		if o.NodeName != "" {
			add(clierr.FlagFragment("-NodeName", o.NodeName))
		}
		for _, t := range o.Taints {
			add(clierr.FlagFragment("-Taint", t))
		}
	} else {
		allRoles := !o.Etcd && !o.ControlPlane && !o.Worker
		if allRoles || o.Etcd {
			words = append(words, "--etcd")
		}
		if allRoles || o.ControlPlane {
			words = append(words, "--controlplane")
		}
		if allRoles || o.Worker {
			words = append(words, "--worker")
		}
		if o.Address != "" {
			add(clierr.FlagFragment("--address", o.Address))
		}
		if o.InternalAddress != "" {
			add(clierr.FlagFragment("--internal-address", o.InternalAddress))
		}
		if o.NodeName != "" {
			add(clierr.FlagFragment("--node-name", o.NodeName))
		}
		for _, l := range o.Labels {
			add(clierr.FlagFragment("--label", l))
		}
		for _, t := range o.Taints {
			add(clierr.FlagFragment("--taints", t))
		}
	}
	if !ok {
		return "", ValueNotQuotable
	}
	if len(words) == 0 {
		return base, Composed
	}
	return base + " " + strings.Join(words, " "), Composed
}

// ComposeResult is whether ComposeNodeCommand composed a command, and if not,
// why.
type ComposeResult int

const (
	// Composed: the command was composed.
	Composed ComposeResult = iota
	// BaseNotFormatted: the node command is empty or still has the token
	// placeholder.
	BaseNotFormatted
	// ValueNotQuotable: a value cannot be single-quoted so that every
	// supported shell reads it as written.
	ValueNotQuotable
)

// The Kubernetes versions the Windows node command needs, and the Rancher
// minors whose rules differ.
const (
	windowsMinKubernetes        = "v1.21.0"
	windowsFlannelMinKubernetes = "v1.29.2"
	// Below this minor only calico (or no CNI set) takes Windows nodes.
	windowsFlannelServerFloor = "2.14"
	// Below this minor the version is the provisioning spec's desired one;
	// from it on, the management Cluster's status.info.kubernetesVersion.
	windowsObservedVersionFloor = "2.15"
)

// WindowsClusterFacts are the values that decide whether the Windows node
// command applies to a custom cluster.
type WindowsClusterFacts struct {
	// ServerVersion is the Rancher version the server states, when
	// ServerVersionRead; an unread version or one with no number gets the
	// rules of the newest minor.
	ServerVersion     string
	ServerVersionRead bool
	// Provider is the management Cluster's status.provider.
	Provider string
	// DesiredVersion is the provisioning Cluster's spec.kubernetesVersion.
	DesiredVersion string
	// ObservedVersion is the management Cluster's
	// status.info.kubernetesVersion.
	ObservedVersion string
	// CNI is the provisioning Cluster's spec.rkeConfig.machineGlobalConfig.cni.
	CNI string
}

// serverBelow reports whether the server's version is known and below floor.
func (f WindowsClusterFacts) serverBelow(floor string) bool {
	if !f.ServerVersionRead {
		return false
	}
	below, known := config.VersionBelow(f.ServerVersion, floor)
	return known && below
}

// CheckWindowsCommand applies the rule the Rancher UI uses to show the Windows
// node command for the server's Rancher version: not K3s, a Kubernetes version
// of v1.21.0 or newer, and calico or no CNI set (on v2.14 and newer, flannel
// from Kubernetes v1.29.2 too). It returns nil when the rule accepts the cluster,
// a VALIDATION error naming the reason, or a retryable GENERIC error when the
// cluster reports no Kubernetes version yet. A version whose
// MAJOR.MINOR.PATCH does not parse, a pre-release one included, is refused.
func CheckWindowsCommand(f WindowsClusterFacts) error {
	if f.Provider == "k3s" {
		return windowsRefusal("the cluster runs K3s; Windows nodes join only RKE2 clusters")
	}
	version := f.ObservedVersion
	if f.serverBelow(windowsObservedVersionFloor) {
		version = f.DesiredVersion
	}
	if version == "" {
		return clierr.Retryable(clierr.CodeGeneric, fmt.Sprintf(
			"--%s: the Windows node command is available once the cluster reports its Kubernetes version; try again then", NodeOptionWindows))
	}
	if _, _, _, _, parsed := clusterops.ParseVersionKey(version); !parsed {
		return windowsRefusal(fmt.Sprintf("the cluster's Kubernetes version %q is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with %s, the oldest version the Rancher UI shows the Windows node command for", version, windowsMinKubernetes))
	}
	if clusterops.CompareVersions(version, windowsMinKubernetes) < 0 {
		return windowsRefusal(fmt.Sprintf("the cluster's Kubernetes version %q is older than %s, the oldest version the Rancher UI shows the Windows node command for", version, windowsMinKubernetes))
	}
	switch {
	case f.CNI == "" || f.CNI == "calico":
		return nil
	case f.serverBelow(windowsFlannelServerFloor):
		return windowsRefusal(fmt.Sprintf("the cluster's CNI is %q; on Rancher before v%s the Rancher UI shows the Windows node command only for calico", f.CNI, windowsFlannelServerFloor))
	case f.CNI == "flannel" && clusterops.CompareVersions(version, windowsFlannelMinKubernetes) >= 0:
		return nil
	}
	return windowsRefusal(fmt.Sprintf("the cluster's CNI is %q; the Rancher UI shows the Windows node command only for calico, and for flannel from Kubernetes %s", f.CNI, windowsFlannelMinKubernetes))
}

func windowsRefusal(reason string) error {
	return clierr.New(clierr.CodeValidation, "--"+NodeOptionWindows+": "+reason)
}

// WindowsNotReadyError is the refusal of --windows on a cluster that is not
// connected yet or lacks a node for any of the three roles. The state changes
// on its own, which is why the error is retryable.
func WindowsNotReadyError() error {
	return clierr.Retryable(clierr.CodeGeneric, fmt.Sprintf(
		"--%s: the Windows node command is available once the cluster is connected and has etcd, control plane and worker nodes; try again then", NodeOptionWindows))
}

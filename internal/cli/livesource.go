package cli

import (
	"fmt"
	"maps"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"

	accesscomplete "github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	projectscomplete "github.com/rancher/rancher-cli/internal/engine/projects/complete"
	rbaccomplete "github.com/rancher/rancher-cli/internal/engine/rbac/complete"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	usercomplete "github.com/rancher/rancher-cli/internal/engine/user/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// The live completion layer keeps its policy here and takes its knowledge
// from the engine packages through completion.Source values. Three tables
// wire the shared sources; a command wires its own by implementing
// completion.Completer. BuildRoot validates every table entry and every
// command's source before the tree is built: a key nothing declares, a
// kind that cannot complete, or a function declared outside the closed
// set of complete packages fails the build rather than a TAB.

const modulePath = "github.com/rancher/rancher-cli"

// liveSourcePackages is the closed set of packages that may declare the
// functions of a live completion source. A function declared anywhere
// else - a method of a command, a helper in an owner package - is
// rejected by BuildRoot, so no code outside these packages runs at TAB
// time.
var liveSourcePackages = map[string]bool{
	modulePath + "/internal/engine/kinds/complete":        true,
	modulePath + "/internal/engine/access/complete":       true,
	modulePath + "/internal/engine/projects/complete":     true,
	modulePath + "/internal/engine/rbac/complete":         true,
	modulePath + "/internal/engine/user/complete":         true,
	modulePath + "/internal/engine/provisioning/complete": true,
	modulePath + "/internal/engine/observe/complete":      true,
	modulePath + "/internal/engine/oidcclient/complete":   true,
}

// sharedCompletionFlags are the flag names whose live source is shared
// across every command declaring the flag, so flagSources may key them:
// the scope flags, and the access commands' --as.
var sharedCompletionFlags = map[string]bool{"cluster": true, "project": true, "namespace": true, "node": true, "as": true}

// flagSources maps a shared flag name to its live source. A command's own
// source takes precedence for the same flag: `access projects` keeps its
// steve cluster walk while every other command lists the management
// clusters. --as is the subject projection grant and revoke use for
// their subject positional, declared once in the rbac complete package.
var flagSources = map[string]completion.Source{
	"cluster":   accesscomplete.ClusterSource(),
	"project":   projectscomplete.ProjectSource(),
	"namespace": kindscomplete.NamespaceSource(),
	"node":      kindscomplete.NodeSource(),
	"as":        rbaccomplete.SubjectSource(),
}

// kindSources maps a kind's Key to the live source for its name
// positional on the generated verbs, for kinds whose names need a
// bespoke projection (ids plus unique display names, system accounts
// left out); a Completable kind with no entry - the provisioning cluster
// among them - completes through the generic kinds source.
var kindSources = map[string]completion.Source{
	"managementcluster": accesscomplete.ClusterSource(),
	"project":           projectscomplete.ProjectIDSource(),
	"user":              usercomplete.UserIDSource(),
}

// positionalKinds maps a bespoke command's name to the Key of the kind its
// first positional completes as: the three inspect leaves whose positional
// is a management object, the cluster-family verbs' provisioning cluster,
// the oidc-client secret verbs' client, and the existing user
// create password-change-request addresses. create user is absent: its
// positional is a new username, never an existing one.
var positionalKinds = map[string]string{
	"inspect cluster":                "managementcluster",
	"inspect user":                   "user",
	"cluster scale":                  "provisioningcluster",
	"cluster set-autoscaling":        "provisioningcluster",
	"cluster clear-autoscaling":      "provisioningcluster",
	"cluster pause-autoscaling":      "provisioningcluster",
	"cluster resume-autoscaling":     "provisioningcluster",
	"oidc-client add-secret":         "oidcclient",
	"oidc-client rotate-secret":      "oidcclient",
	"oidc-client remove-secret":      "oidcclient",
	"create password-change-request": "user",
}

// allowanceLimit maps a granted allowance class to its request count:
// one plain listing; the two discovery roots plus, for each root that
// does not serve the aggregated document, its plain document and one
// document per group up to the seam's group limit; or the Steve cluster
// page walk bounded like the access commands' own. A value outside the
// class is not granted.
func allowanceLimit(a completion.Allowance) (int, bool) {
	switch a {
	case completion.AllowanceOne:
		return 1, true
	case completion.AllowanceDiscovery:
		return 2 + 2*(1+completion.DiscoveryGroupLimit), true
	case completion.AllowanceSteveClusterPages:
		return steve.MaxPages, true
	case completion.AllowanceSteveProjectPages:
		return steve.MaxPages, true
	}
	return 0, false
}

// declaringPackage returns the import path of the package that declares
// fn, a function value, or "" for a nil function.
func declaringPackage(fn any) string {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func || v.IsNil() {
		return ""
	}
	f := goruntime.FuncForPC(v.Pointer())
	if f == nil {
		return ""
	}
	name := f.Name()
	// The symbol is <import path>.<declaration>[.<closure>][-fm]; the
	// import path may itself contain dots before its last slash.
	slash := strings.LastIndex(name, "/")
	dot := strings.Index(name[slash+1:], ".")
	if dot < 0 {
		return ""
	}
	return name[:slash+1+dot]
}

// checkSourceDeclared rejects a source with a missing function or one
// declared outside liveSourcePackages; where names the wiring for the
// message.
func checkSourceDeclared(where string, src completion.Source) error {
	for _, f := range []struct {
		name string
		fn   any
	}{{"Claim", src.Claim}, {"Complete", src.Complete}} {
		name, fn := f.name, f.fn
		pkg := declaringPackage(fn)
		if pkg == "" {
			return fmt.Errorf("live completion source for %s has no %s function", where, name)
		}
		if !liveSourcePackages[pkg] {
			return fmt.Errorf("live completion source for %s: %s is declared in %s, not in a complete package", where, name, pkg)
		}
	}
	return nil
}

// validateLiveWiring checks the three tables and every command's own
// source against the adapted leaves: leaves maps each command name to
// its descriptor, completers each command that implements
// completion.Completer to the source it returned.
func validateLiveWiring(leaves map[string]runtime.Descriptor, completers map[string]completion.Source, flags map[string]completion.Source, kindTable map[string]completion.Source, positional map[string]string) error {
	// The generic kinds source is reachable for every Completable kind
	// without a table entry, so it is checked whether or not a table
	// happens to carry it.
	if err := checkSourceDeclared("the generic kinds source", kindscomplete.KindSource("")); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(completers)) {
		if err := checkSourceDeclared("command "+name, completers[name]); err != nil {
			return err
		}
	}
	for _, flag := range slices.Sorted(maps.Keys(flags)) {
		if !sharedCompletionFlags[flag] {
			return fmt.Errorf("flagSources: --%s is not a shared completion flag", flag)
		}
		declared := false
		for _, d := range leaves {
			if slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == flag }) {
				declared = true
				break
			}
		}
		if !declared {
			return fmt.Errorf("flagSources: no command declares --%s", flag)
		}
		if err := checkSourceDeclared("--"+flag, flags[flag]); err != nil {
			return err
		}
	}
	for _, key := range slices.Sorted(maps.Keys(kindTable)) {
		k, ok := kinds.ByKey(key)
		if !ok {
			return fmt.Errorf("kindSources: %q is not a kind key", key)
		}
		if !k.Completable {
			return fmt.Errorf("kindSources: kind %q is not Completable", key)
		}
		if err := checkSourceDeclared("kind "+key, kindTable[key]); err != nil {
			return err
		}
	}
	for _, cmd := range slices.Sorted(maps.Keys(positional)) {
		if _, ok := leaves[cmd]; !ok {
			return fmt.Errorf("positionalKinds: %q is not an adapted command", cmd)
		}
		k, ok := kinds.ByKey(positional[cmd])
		if !ok {
			return fmt.Errorf("positionalKinds: %q names %q, which is not a kind key", cmd, positional[cmd])
		}
		if !k.Completable {
			return fmt.Errorf("positionalKinds: %q names kind %q, which is not Completable", cmd, positional[cmd])
		}
	}
	return nil
}

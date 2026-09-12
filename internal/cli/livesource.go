package cli

import (
	"fmt"
	"maps"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
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
// across every command declaring the flag, so flagSources may key them.
var sharedCompletionFlags = map[string]bool{"cluster": true, "project": true, "namespace": true, "node": true}

// flagSources maps a shared flag name to its live source. A command's own
// source takes precedence for the same flag.
var flagSources = map[string]completion.Source{}

// kindSources maps a kind's Key to the live source for its name
// positional on the generated verbs, for kinds whose names need a
// bespoke projection; a Completable kind with no entry completes through
// the generic kinds source.
var kindSources = map[string]completion.Source{}

// positionalKinds maps a bespoke command's name to the Key of the kind its
// first positional completes as.
var positionalKinds = map[string]string{}

// allowanceLimit maps a granted allowance class to its request count:
// one plain listing, the two discovery roots, or the Steve cluster page
// walk bounded like the access commands' own. A value outside the class
// is not granted.
func allowanceLimit(a completion.Allowance) (int, bool) {
	switch a {
	case completion.AllowanceOne:
		return 1, true
	case completion.AllowanceDiscovery:
		return 2, true
	case completion.AllowanceSteveClusterPages:
		return access.SteveMaxPages, true
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

// kindByKey returns the kind row whose Key is key.
func kindByKey(key string) (kinds.Kind, bool) {
	for _, k := range kinds.All() {
		if k.Key == key {
			return k, true
		}
	}
	return kinds.Kind{}, false
}

// validateLiveWiring checks the three tables and every command's own
// source against the adapted leaves: leaves maps each command name to
// its descriptor, completers each command that implements
// completion.Completer to the source it returned.
func validateLiveWiring(leaves map[string]runtime.Descriptor, completers map[string]completion.Source, flags map[string]completion.Source, kindTable map[string]completion.Source, positional map[string]string) error {
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
		k, ok := kindByKey(key)
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
		k, ok := kindByKey(positional[cmd])
		if !ok {
			return fmt.Errorf("positionalKinds: %q names %q, which is not a kind key", cmd, positional[cmd])
		}
		if !k.Completable {
			return fmt.Errorf("positionalKinds: %q names kind %q, which is not Completable", cmd, positional[cmd])
		}
	}
	return nil
}

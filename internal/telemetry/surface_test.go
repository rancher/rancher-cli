package telemetry

import (
	"go/types"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// surfaceRules says which exported names may take a string. Every entry is
// an API that runs a normalizer, a closed list or a path or URL it does not
// write into an event as given.
type surfaceRules struct {
	// events are the event types: no exported field, no setter.
	events []string
	// funcs are the functions and methods ("Type.Method") that may take a
	// string, a list of strings, or a struct holding one.
	funcs []string
	// types are the exported struct types that may hold an exported string.
	types []string
}

var realSurface = surfaceRules{
	events: []string{"CommandRun", "EventsDropped"},
	funcs: []string{
		"NormalizeVersion", "NormalizeUUID", "NormalizeRandomID", "NormalizeOS", "NormalizeArch", "NormalizeCommand",
		"NormalizeFlags", "NormalizeErrorType", "NormalizeAgent", "ConfigSettingOf",
		"NewCommandRun", "NewEventsDropped", "ClientTypeFor",
		"FactsFileName", "SetServer", "OpenStore", "Record", "ContractFiles", "Resolve",
		"AttributesOf", "SortedNames",
		"Store.WriteFacts", "Store.ReadFacts",
		"Facts.SetVersion", "Facts.SetInstallID",
	},
	types: []string{
		"Attribute", "ContractInputs", "EventSpec", "Options", "RunInput", "ControlInput",
		"Decision", "Facts", "Fact", "Failure", "StoredEvent",
	},
}

// carriesString reports whether a value of type t can hold free text: the
// predeclared string, a byte slice, an interface (any value can be text), or
// a slice, array, pointer, map or struct that holds one. A named type whose
// underlying type is string is a closed type this package defines and does
// not count.
func carriesString(t types.Type, self *types.Package, seen map[types.Type]bool) bool {
	t = types.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.(type) {
	case *types.Basic:
		return u.Kind() == types.String
	case *types.Interface:
		return true
	case *types.Slice:
		if b, ok := u.Elem().(*types.Basic); ok && b.Kind() == types.Byte {
			return true
		}
		return carriesString(u.Elem(), self, seen)
	case *types.Array:
		return carriesString(u.Elem(), self, seen)
	case *types.Pointer:
		return carriesString(u.Elem(), self, seen)
	case *types.Map:
		return carriesString(u.Key(), self, seen) || carriesString(u.Elem(), self, seen)
	case *types.Named:
		if b, ok := u.Underlying().(*types.Basic); ok && b.Kind() == types.String && u.Obj().Pkg() != nil {
			return false
		}
		st, isStruct := u.Underlying().(*types.Struct)
		if !isStruct {
			return carriesString(u.Underlying(), self, seen)
		}
		for f := range st.Fields() {
			// A struct from another package counts by its exported
			// fields only; its private state is that package's own.
			if (u.Obj().Pkg() == self || f.Exported()) && carriesString(f.Type(), self, seen) {
				return true
			}
		}
	case *types.Struct:
		for f := range u.Fields() {
			if carriesString(f.Type(), self, seen) {
				return true
			}
		}
	}
	return false
}

func takesString(sig *types.Signature, self *types.Package) bool {
	for p := range sig.Params().Variables() {
		if carriesString(p.Type(), self, map[types.Type]bool{}) {
			return true
		}
	}
	return false
}

// surfaceViolations type-checks the package at pattern (relative to the
// module root) and returns one line per exported API that could put free
// text into an event.
func surfaceViolations(t *testing.T, pattern string, rules surfaceRules) []string {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles,
		Dir:  "../..",
	}
	pkgs, err := packages.Load(cfg, pattern)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors)
	self := pkgs[0].Types
	scope := self.Scope()

	var out []string
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			if takesString(o.Type().(*types.Signature), self) && !slices.Contains(rules.funcs, name) {
				out = append(out, "function "+name+" can carry free text")
			}
		case *types.TypeName:
			named, ok := o.Type().(*types.Named)
			if !ok {
				continue
			}
			if st, ok := named.Underlying().(*types.Struct); ok {
				isEvent := slices.Contains(rules.events, name)
				for f := range st.Fields() {
					switch {
					case isEvent && f.Exported():
						out = append(out, "event type "+name+" has the exported field "+f.Name())
					case !isEvent && f.Exported() && carriesString(f.Type(), self, map[types.Type]bool{}) && !slices.Contains(rules.types, name):
						out = append(out, "type "+name+" has an exported string field "+f.Name())
					}
				}
			}
			for m := range named.Methods() {
				if !m.Exported() {
					continue
				}
				full := name + "." + m.Name()
				if takesString(m.Type().(*types.Signature), self) && !slices.Contains(rules.funcs, full) {
					out = append(out, "method "+full+" can carry free text")
				}
			}
			ptr := types.NewPointer(named)
			if ms := types.NewMethodSet(ptr); ms != nil {
				for sel := range ms.Methods() {
					fn, ok := sel.Obj().(*types.Func)
					if !ok || !fn.Exported() || fn.Pkg() != self {
						continue
					}
					full := name + "." + fn.Name()
					if takesString(fn.Type().(*types.Signature), self) && !slices.Contains(rules.funcs, full) && !slices.Contains(out, "method "+full+" can carry free text") {
						out = append(out, "method "+full+" can carry free text")
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestNoExportedAPIPutsFreeTextIntoAnEvent asserts the package's exported
// surface: the event types have no exported field, and the only exported
// functions and methods that take a string are the listed ones, each of
// which runs a normalizer. A new exported setter with a string parameter
// fails until a person lists it.
func TestNoExportedAPIPutsFreeTextIntoAnEvent(t *testing.T) {
	assert.Empty(t, surfaceViolations(t, "./internal/telemetry", realSurface))
}

// TestFreeStringSurfaceCheckCatchesAFixture asserts the check reports the
// fixture package's exported field, its setter and its constructors.
func TestFreeStringSurfaceCheckCatchesAFixture(t *testing.T) {
	got := surfaceViolations(t, "./internal/telemetry/testdata/freestring", surfaceRules{events: []string{"Event"}})
	assert.Equal(t, []string{
		"event type Event has the exported field Name",
		"function Make can carry free text",
		"function Names can carry free text",
		"method Event.SetAny can carry free text",
		"method Event.SetBytes can carry free text",
		"method Event.SetCommand can carry free text",
	}, got)
	for _, g := range got {
		assert.NotContains(t, g, "Count")
	}
}

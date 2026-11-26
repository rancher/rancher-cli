package secret_test

import (
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// apiRows maps each exported function and method of the package ("Name" or
// "Value.Name") to its result types. A new exported identifier, or a changed
// signature, fails until its row is added.
var apiRows = map[string]string{
	"New":                   "Value",
	"FromBytes":             "Value",
	"Read":                  "Value, error",
	"Reveal":                "string",
	"Value.Len":             "int",
	"Value.IsZero":          "bool",
	"Value.IsBlank":         "bool",
	"Value.Equal":           "bool",
	"Value.EqualString":     "bool",
	"Value.EqualFoldString": "bool",
	"Value.Cut":             "Value, Value, bool",
	"Value.String":          "string",
	"Value.GoString":        "string",
	"Value.Format":          "",
	"Value.MarshalJSON":     "[]byte, error",
	"Value.MarshalText":     "[]byte, error",
	"Value.MarshalYAML":     "any, error",
	"Value.MarshalLog":      "any",
	"Value.LogValue":        "log/slog.Value",
}

// textResultAllowed lists the functions that may return a string, a byte
// slice, an interface or an error: the reveal function, the printing and
// encoding methods that TestValuePrintsHidden covers, and Read, whose error is
// the caller's reader's.
var textResultAllowed = map[string]string{
	"Reveal":            "the only function that returns the text",
	"Read":              "returns the error of the caller's reader",
	"Value.String":      "prints <hidden>",
	"Value.GoString":    "prints <hidden>",
	"Value.MarshalJSON": "encodes <hidden>",
	"Value.MarshalText": "encodes <hidden>",
	"Value.MarshalYAML": "encodes <hidden>",
	"Value.MarshalLog":  "encodes <hidden>",
	"Value.LogValue":    "encodes <hidden>",
}

// resultTypes lists the types of a function's results, qualified by package
// path for a type outside the package.
func resultTypes(sig *types.Signature, pkg *types.Package) []string {
	out := make([]string, 0, sig.Results().Len())
	for r := range sig.Results().Variables() {
		out = append(out, types.TypeString(r.Type(), types.RelativeTo(pkg)))
	}
	return out
}

// paramTypes lists the types of a function's parameters, the receiver left out.
func paramTypes(sig *types.Signature, pkg *types.Package) []string {
	out := make([]string, 0, sig.Params().Len())
	for p := range sig.Params().Variables() {
		out = append(out, types.TypeString(p.Type(), types.RelativeTo(pkg)))
	}
	return out
}

// safeParamTypes is the closed set of types any exported function or method may
// take. A function that takes any other type could be handed a callback, an
// interface or a pointer that the text is written into or returned through.
var safeParamTypes = map[string]bool{
	"bool": true, "int": true, "rune": true, "string": true, "Value": true,
}

// boundParamTypes lists the parameter types only one function may take: Format
// is given the state of fmt, which it writes <hidden> to, Read is given the
// reader of a file, which it calls with a path, and FromBytes is given the
// bytes it copies. A function that takes a byte slice or a state could write
// the text into it.
var boundParamTypes = map[string]string{
	"Value.Format": "fmt.State",
	"Read":         "func(path string) ([]byte, error)",
	"FromBytes":    "[]byte",
}

// safeResultTypes is the closed set of types an exported function or method
// may return: none can carry the text of a Value.
var safeResultTypes = map[string]bool{"bool": true, "int": true, "Value": true}

// textResultTypes are the types that can carry the text, or an error that
// has it: only a function of textResultAllowed may return one.
var textResultTypes = map[string]bool{"string": true, "[]byte": true, "any": true, "error": true, "log/slog.Value": true}

// TestSecretPackageReturnsTextOnlyFromListedFunctions asserts the exported
// functions and methods of the package, with their result types, are the rows
// of apiRows, that every parameter type is in safeParamTypes or is the one type
// boundParamTypes allows for that function, and that every result type is in
// safeResultTypes or, for a function of textResultAllowed, in
// textResultTypes. A type outside these closed sets (a pointer, a rune slice, a
// callback) fails until the function is removed or its type is reviewed into a
// set.
func TestSecretPackageReturnsTextOnlyFromListedFunctions(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes, Env: testfs.EnvWithoutGit()}
	pkgs, err := packages.Load(cfg, ".")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors)
	pkg := pkgs[0].Types
	got := map[string]string{}
	gotParams := map[string][]string{}
	var other []string
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			got[name] = strings.Join(resultTypes(o.Signature(), pkg), ", ")
			gotParams[name] = paramTypes(o.Signature(), pkg)
		case *types.TypeName:
			if name != "Value" {
				other = append(other, "type "+name)
				continue
			}
			named := o.Type().(*types.Named)
			if st, ok := named.Underlying().(*types.Struct); ok {
				for f := range st.Fields() {
					if f.Exported() {
						other = append(other, "field "+f.Name())
					}
				}
			}
			ms := types.NewMethodSet(types.NewPointer(named))
			for sel := range ms.Methods() {
				fn := sel.Obj().(*types.Func)
				if fn.Exported() {
					got["Value."+fn.Name()] = strings.Join(resultTypes(fn.Signature(), pkg), ", ")
					gotParams["Value."+fn.Name()] = paramTypes(fn.Signature(), pkg)
				}
			}
		default:
			other = append(other, name)
		}
	}
	assert.Empty(t, other, "an exported identifier that is not a function, a method or the type Value")
	for _, name := range slices.Sorted(maps.Keys(got)) {
		want, ok := apiRows[name]
		if !ok {
			t.Errorf("%s returns (%s) and has no row in apiRows: add the row, and a reason in textResultAllowed when a result can carry the text", name, got[name])
			continue
		}
		assert.Equal(t, want, got[name], name+": the results differ from the row")
		for _, param := range gotParams[name] {
			if !safeParamTypes[param] && boundParamTypes[name] != param {
				t.Errorf("%s takes %s, which is not in safeParamTypes and is not the type boundParamTypes allows for it", name, param)
			}
		}
		_, listed := textResultAllowed[name]
		if got[name] == "" {
			continue
		}
		for r := range strings.SplitSeq(got[name], ", ") {
			switch {
			case safeResultTypes[r]:
			case textResultTypes[r] && listed:
			default:
				t.Errorf("%s returns %s, which is not in safeResultTypes and is not a text result of a function in textResultAllowed", name, r)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(apiRows)) {
		if _, ok := got[name]; !ok {
			t.Errorf("apiRows has a row for %s, which the package no longer declares: remove it", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(textResultAllowed)) {
		assert.Contains(t, apiRows, name, name)
	}
}

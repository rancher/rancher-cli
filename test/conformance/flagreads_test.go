package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A command gets a flag marked Secret only through Input.Secrets, and a flag
// not marked only through Input.String and Input.Strings: the Input of
// internal/cli returns nil for the wrong method, and a command reads that as a
// flag that was not typed. The scan is a go/types walk of every production
// package that finds the calls of the Input methods with a constant flag name
// and the flags each file declares:
//
//   - a flag name is declared by a runtime.ArgSpec literal with a constant Name;
//     it is Secret there when the literal has Secret: true;
//   - a call of Secrets with a name the file declares without Secret, or, when
//     the file does not declare it, a name that no file marks Secret, fails;
//   - a call of String or Strings with a name the file declares only as Secret,
//     or, when the file does not declare it, a name that is Secret in some file
//     and plain in none, fails;
//   - a call whose flag name is not a constant, or whose constant name is
//     ambiguous (Secret on one command and plain on another, read in a file
//     that declares it both ways or does not declare it), cannot be matched:
//     its function must be in unresolvedFlagReads with the reason it reads the
//     right flag, and a row that matches no call is stale.
//
// This scan adds to the type secret.Value, which is the protection. It
// misses these reads, and a miss makes a command see a flag as not typed, with
// no leak:
//
//   - a flag declared by an ArgSpec literal whose Name or Secret is not a
//     constant: the literal declares nothing, or counts as plain; a Secret
//     flag built that way is not known;
//   - a call through a method value (f := in.Strings; f(name)), and a call on a
//     concrete type that overrides an Input method;
//   - a new read inside a function that unresolvedFlagReads already lists:
//     the rows are per function and the scan does not count the reads in it;
//   - a read of an ambiguous name in a file that declares it only one way,
//     which is matched to that declaration.
//
// Test files are not scanned. The scan loads the packages for the platform the
// test runs on and for windows.

const (
	flagReadFixtureDir = "test/conformance/testdata/flagreads"
	flagReadRuntimeDir = modulePrefix + "internal/runtime"
	flagReadCLIDir     = modulePrefix + "internal/cli"

	flagReadRuleSecretStrings = "a flag marked Secret read through String or Strings"
	flagReadRulePlainSecrets  = "a flag not marked Secret read through Secrets"
	flagReadRuleUnresolved    = "a call of String, Strings or Secrets with a flag name that is not a constant or is ambiguous, in a function with no row in unresolvedFlagReads"
	flagReadRuleStale         = "a row of unresolvedFlagReads that matches no call"
	flagReadRuleReason        = "a row of unresolvedFlagReads with no reason"
)

// unresolvedFlagReads maps "<file>:<function>" to why the function reads a flag
// by a name that is not a constant, or by a name that is Secret on one command
// and plain on another.
var unresolvedFlagReads = map[string]string{
	"internal/runtime/pipeline.go:Run":                                                 "reads the global flags (output, color and the others) by the names GlobalFlags lists; none is Secret",
	"internal/runtime/pipeline.go:validateInput":                                       "reads each flag of the descriptor by its name: a Secret flag goes through Secrets in the branch before, any other through String or Strings",
	"internal/runtime/pipeline.go:emptyValueError":                                     "reads each flag of the descriptor by its name: a Secret flag goes through Secrets in the branch that decides, and the String of a Secret flag only seeds a value that branch replaces",
	"internal/runtime/duration.go:DurationFlag":                                        "reads the duration flag its caller names, a plain flag",
	"internal/engine/access/can.go:parseQuestion":                                      "reads the flags of the scope its caller lists, all plain",
	"internal/engine/provisioning/createmachineconfig.go:createMachineConfigCmd.build": "reads the plain flags that name the fields of a machine config; its --set is plain",
	"internal/engine/provisioning/upgradestrategy.go:readStrategyFlags":                "reads the plain upgrade strategy flags, named from the pool group",
	"internal/engine/resources/create.go:overrideInput.String":                         "forwards String of the wrapped input for a name its caller gives",
}

// flagReadCall is one call of Secrets, String or Strings with a constant name.
type flagReadCall struct {
	file, at, method, name string
	// key is "<file>:<function>"; unresolved is true when the name is not a
	// constant.
	key        string
	unresolved bool
}

// flagReadFacts is what the scan found: for each file, the flag names its
// ArgSpec literals declare as Secret and as plain, and every call.
type flagReadFacts struct {
	secret, plain map[string]map[string]bool
	calls         map[string]flagReadCall
}

func newFlagReadFacts() flagReadFacts {
	return flagReadFacts{secret: map[string]map[string]bool{}, plain: map[string]map[string]bool{}, calls: map[string]flagReadCall{}}
}

func (f flagReadFacts) merge(o flagReadFacts) {
	for _, pair := range [][2]map[string]map[string]bool{{f.secret, o.secret}, {f.plain, o.plain}} {
		for file, names := range pair[1] {
			if pair[0][file] == nil {
				pair[0][file] = map[string]bool{}
			}
			maps.Copy(pair[0][file], names)
		}
	}
	maps.Copy(f.calls, o.calls)
}

func (f flagReadFacts) declare(into map[string]map[string]bool, file, name string) {
	if into[file] == nil {
		into[file] = map[string]bool{}
	}
	into[file][name] = true
}

// flagReadIsNamed reports whether t is the named type name of the package at
// pkgPath, behind pointers.
func flagReadIsNamed(t types.Type, pkgPath, name string) bool {
	for {
		p, ok := types.Unalias(t).(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Name() == name && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath
}

// flagReadConstString returns the constant string value of e.
func flagReadConstString(info *types.Info, e ast.Expr) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// flagReadScan returns the declarations and calls of the non-test files of pkg.
func flagReadScan(pkg *packages.Package, wd string) flagReadFacts {
	info := pkg.TypesInfo
	facts := newFlagReadFacts()
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(f.Pos()).Filename, "_test.go") {
			continue
		}
		rel := byIDRel(pkg, f.Pos(), wd)
		for _, decl := range f.Decls {
			key := rel + ":" + stdinDocFunctionName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					if !flagReadIsNamed(info.TypeOf(n), flagReadRuntimeDir, "ArgSpec") {
						return true
					}
					name, isSecret := "", false
					for _, el := range n.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						switch key.Name {
						case "Name":
							name, _ = flagReadConstString(info, kv.Value)
						case "Secret":
							if tv, ok := info.Types[kv.Value]; ok && tv.Value != nil && tv.Value.Kind() == constant.Bool {
								isSecret = constant.BoolVal(tv.Value)
							}
						}
					}
					switch {
					case name == "":
					case isSecret:
						facts.declare(facts.secret, rel, name)
					default:
						facts.declare(facts.plain, rel, name)
					}
				case *ast.CallExpr:
					fn := methodOf(info, n)
					if fn == nil || len(n.Args) == 0 || (fn.Name() != "Secrets" && fn.Name() != "String" && fn.Name() != "Strings") {
						return true
					}
					sig, ok := fn.Type().(*types.Signature)
					if !ok || sig.Recv() == nil {
						return true
					}
					if !flagReadIsNamed(sig.Recv().Type(), flagReadRuntimeDir, "Input") && !flagReadIsNamed(sig.Recv().Type(), flagReadCLIDir, "Input") {
						return true
					}
					name, ok := flagReadConstString(info, n.Args[0])
					p := pkg.Fset.Position(n.Pos())
					at := fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column)
					facts.calls[at] = flagReadCall{file: rel, at: at, method: fn.Name(), name: name, key: key, unresolved: !ok}
				}
				return true
			})
		}
	}
	return facts
}

// flagReadLoads scans the module for the platform the test runs on and for
// windows, once per process, and merges the facts.
var flagReadLoads = sync.OnceValues(func() (flagReadFacts, error) {
	pkgs, err := fieldManagerPackages()
	if err != nil {
		return flagReadFacts{}, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return flagReadFacts{}, err
	}
	merged := newFlagReadFacts()
	for _, pkg := range pkgs {
		merged.merge(flagReadScan(pkg, wd))
	}
	windows, err := otherGOOSPackages("windows")
	if err != nil {
		return flagReadFacts{}, err
	}
	for _, pkg := range windows {
		merged.merge(flagReadScan(pkg, wd))
	}
	return merged, nil
})

// flagReadUnion is the set of names declared in any file.
func flagReadUnion(per map[string]map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, names := range per {
		maps.Copy(out, names)
	}
	return out
}

// flagReadCheck returns one problem per call that breaks a rule, each starting
// with its rule.
func flagReadCheck(facts flagReadFacts, rows map[string]string) []string {
	var problems []string
	anySecret, anyPlain := flagReadUnion(facts.secret), flagReadUnion(facts.plain)
	unresolved := map[string]bool{}
	for _, at := range slices.Sorted(maps.Keys(facts.calls)) {
		c := facts.calls[at]
		secretHere, plainHere := facts.secret[c.file][c.name], facts.plain[c.file][c.name]
		declared := secretHere || plainHere
		ambiguous := !c.unresolved && ((secretHere && plainHere) || (!declared && anySecret[c.name] && anyPlain[c.name]))
		if c.unresolved || ambiguous {
			unresolved[c.key] = true
			if _, ok := rows[c.key]; !ok {
				why := "a name the scan cannot resolve"
				if ambiguous {
					why = fmt.Sprintf("the name %q, which is Secret on one command and plain on another", c.name)
				}
				problems = append(problems, fmt.Sprintf("%s: %s: %s with %s; add %q to unresolvedFlagReads with the reason, or pass the name as a constant", flagReadRuleUnresolved, c.at, c.method, why, c.key))
			}
			continue
		}
		if c.method == "Secrets" {
			if (declared && !secretHere) || (!declared && !anySecret[c.name]) {
				problems = append(problems, fmt.Sprintf("%s: %s: Secrets(%q); read a flag not marked Secret through String or Strings, or mark the flag Secret", flagReadRulePlainSecrets, at, c.name))
			}
			continue
		}
		if (secretHere && !plainHere) || (!declared && anySecret[c.name] && !anyPlain[c.name]) {
			problems = append(problems, fmt.Sprintf("%s: %s: %s(%q); read a flag marked Secret through Secrets", flagReadRuleSecretStrings, at, c.method, c.name))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		switch {
		case !unresolved[key]:
			problems = append(problems, fmt.Sprintf("%s: %s matched nothing: remove it", flagReadRuleStale, key))
		case rows[key] == "":
			problems = append(problems, fmt.Sprintf("%s: %s", flagReadRuleReason, key))
		}
	}
	return problems
}

// TestEverySecretFlagIsReadThroughSecretsAndNoOtherThrough asserts every call
// of Input.Secrets names a flag marked Secret, and no call of Input.String or
// Input.Strings names a flag that is only ever marked Secret.
func TestEverySecretFlagIsReadThroughSecretsAndNoOtherThrough(t *testing.T) {
	t.Parallel()
	facts, err := flagReadLoads()
	require.NoError(t, err)
	assert.NotEmpty(t, flagReadUnion(facts.secret), "the scan found no flag marked Secret")
	assert.NotEmpty(t, facts.calls, "the scan found no call of an Input method")
	for _, problem := range flagReadCheck(facts, unresolvedFlagReads) {
		t.Error(problem)
	}
}

// flagReadFixtures type-checks the fixture packages once.
var flagReadFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+flagReadFixtureDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// flagReadFixtureCases lists every fixture package and the rules the check
// reports for it, in order.
var flagReadFixtureCases = []struct {
	name    string
	fixture string
	rows    map[string]string
	want    []string
}{
	{"matching reads", "correct", nil, nil},
	{"a Secret flag read through Strings", "secretstrings", nil, []string{flagReadRuleSecretStrings}},
	{"a Secret flag read through String", "secretstring", nil, []string{flagReadRuleSecretStrings}},
	{"a plain flag read through Secrets", "plainsecrets", nil, []string{flagReadRulePlainSecrets}},
	{"a name no file marks Secret read through Secrets", "undeclared", nil, []string{flagReadRulePlainSecrets}},
	{"two reads with a name that is not a constant, not listed", "dynamic", nil, []string{flagReadRuleUnresolved, flagReadRuleUnresolved}},
	{"an ambiguous name, not listed", "ambiguous", nil, []string{flagReadRuleUnresolved}},
	{"an ambiguous name, listed", "ambiguous", map[string]string{flagReadFixtureDir + "/ambiguous/read.go:Read": "fixture"}, nil},
	{"names that are not constants, listed", "dynamic", map[string]string{flagReadFixtureDir + "/dynamic/dynamic.go:Read": "fixture"}, nil},
	{"a row with no reason", "dynamic", map[string]string{flagReadFixtureDir + "/dynamic/dynamic.go:Read": ""}, []string{flagReadRuleReason}},
	{"a row that matches no call", "correct", map[string]string{flagReadFixtureDir + "/correct/correct.go:Gone": "fixture"}, []string{flagReadRuleStale}},
}

// TestFlagReadScanFixtures asserts each rule on type-checked fixture packages:
// a flag marked Secret read through String or Strings, a flag not marked read
// through Secrets, a name no file marks Secret read through Secrets and a call
// with a name that is not a constant and has no row are reported, with a stale
// row and a row with no reason; the matching reads and a listed call are not.
func TestFlagReadScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := flagReadFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range flagReadFixtureCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, problem := range flagReadCheck(flagReadScan(pkg, wd), tc.rows) {
				rule, _, _ := strings.Cut(problem, ": ")
				got = append(got, rule)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestFlagReadScanFixturesAreAllListed asserts every fixture package on disk is
// in the table above.
func TestFlagReadScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(flagReadFixtureDir)
	require.NoError(t, err)
	pkgs, err := flagReadFixtures()
	require.NoError(t, err)
	var onDisk, listed, loaded []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range flagReadFixtureCases {
		if !slices.Contains(listed, tc.fixture) {
			listed = append(listed, tc.fixture)
		}
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, onDisk)
	assert.ElementsMatch(t, listed, loaded)
}

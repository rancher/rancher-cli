package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The upload lock is the innermost lock: the code that holds it takes no other
// lock, and nothing takes it while holding another. The scan lists the one
// function that takes it, the one reference to the sender's entry, the one use
// of the variable that holds it and the one call site of the function that
// calls it; the closure of the sender holds no package that takes a lock or
// reads a credential.
const (
	uploadLockTaker    = "internal/telemetrysend/send.go:run"
	senderReference    = "internal/cli/upload.go:"
	senderVariable     = "sendStoredEvents"
	senderVariableCall = "internal/cli/upload.go:uploadStoredEvents"
	uploadStoreCaller  = "internal/cli/run.go:run"
	uploadStoreFunc    = "uploadStoredEvents"
	rootRunWrapperFunc = "runArgv"
)

// enclosingFunc returns the name of the function declaration that holds pos,
// "" for a package-level declaration.
func enclosingFunc(file *ast.File, pos token.Pos) string {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos <= fd.End() {
			return fd.Name.Name
		}
	}
	return ""
}

// uploadLockFindings parses the non-test Go files in sources (path to text)
// and returns one line per way the lock rules above are broken: a call of
// TakeUploadLock outside the listed function or more than once, a reference to
// telemetrysend.Send outside the listed place or more than once, a use of the
// variable that holds the sender (its declaration and its one call) anywhere
// else or more than once, a call of the upload function outside the listed
// function, more than once, or before the root command ran.
func uploadLockFindings(t *testing.T, sources map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	takes := map[string]int{}
	references := map[string]int{}
	variableUses := map[string]int{}
	var uploadCalls []string
	var uploadPos, rootRunPos token.Pos
	var out []string
	for _, path := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(fset, path, sources[path], 0)
		require.NoError(t, err, path)
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if v.Name == senderVariable {
					variableUses[path+":"+enclosingFunc(file, v.Pos())]++
				}
			case *ast.SelectorExpr:
				if v.Sel.Name == "TakeUploadLock" {
					takes[path+":"+enclosingFunc(file, v.Pos())]++
				}
				if x, ok := v.X.(*ast.Ident); ok && x.Name == "telemetrysend" && v.Sel.Name == "Send" {
					references[path+":"+enclosingFunc(file, v.Pos())]++
				}
			case *ast.CallExpr:
				id, ok := v.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				where := path + ":" + enclosingFunc(file, v.Pos())
				switch id.Name {
				case uploadStoreFunc:
					uploadCalls = append(uploadCalls, where)
					uploadPos = v.Pos()
				case rootRunWrapperFunc:
					if where == uploadStoreCaller {
						rootRunPos = v.Pos()
					}
				}
			}
			return true
		})
	}
	for where, n := range takes {
		if where != uploadLockTaker || n != 1 {
			out = append(out, "TakeUploadLock called "+where+" x"+strconv.Itoa(n))
		}
	}
	if takes[uploadLockTaker] != 1 {
		out = append(out, "TakeUploadLock is not called once in "+uploadLockTaker)
	}
	for where, n := range references {
		if where != senderReference || n != 1 {
			out = append(out, "telemetrysend.Send referenced "+where+" x"+strconv.Itoa(n))
		}
	}
	if references[senderReference] != 1 {
		out = append(out, "telemetrysend.Send is not referenced once in "+senderReference)
	}
	wantUses := map[string]int{senderReference: 1, senderVariableCall: 1}
	for where, n := range variableUses {
		if wantUses[where] != n {
			out = append(out, senderVariable+" used "+where+" x"+strconv.Itoa(n))
		}
	}
	for where := range wantUses {
		if variableUses[where] != wantUses[where] {
			out = append(out, senderVariable+" is not used once in "+where)
		}
	}
	if len(uploadCalls) != 1 || uploadCalls[0] != uploadStoreCaller {
		out = append(out, "the upload function is called "+strings.Join(uploadCalls, ", ")+", want once in "+uploadStoreCaller)
	} else if rootRunPos == token.NoPos || uploadPos < rootRunPos {
		out = append(out, "the upload function is called before the root command ran")
	}
	slices.Sort(out)
	return out
}

// The function the sender calls to resolve the recording controls while it
// holds the upload lock, and the names that take a lock: the upload lock, the
// config lock forms and the helpers under them.
const (
	recordingCallbackFile = "internal/cli/upload.go"
	recordingCallbackFunc = "recordingStillOn"
	flockImport           = "github.com/gofrs/flock"
	skillinstallImport    = modulePrefix + "internal/skillinstall"
)

var lockTakingNames = []string{
	"TakeUploadLock", "WithLock", "WithPluginLoginLock", "withDirLock", "tryDirLock", "tryWithLock", "waitForFlock",
}

// recordingLockFindings parses the non-test Go files in sources (path to text)
// and returns one line per way the function that resolves the recording
// controls under the upload lock takes a lock: it, or a function it reaches in
// the repository, calls a name in lockTakingNames, calls into the flock
// package or calls into the skill installer. A call reaches the functions of
// that name in the same package, in the repository package its import names,
// and, for a method call, the methods of that name in every package.
func recordingLockFindings(t *testing.T, sources map[string]string) []string {
	t.Helper()
	type decl struct {
		file    string
		fd      *ast.FuncDecl
		imports map[string]string
	}
	fset := token.NewFileSet()
	funcs := map[string][]*decl{}
	methods := map[string][]*decl{}
	var start *decl
	for _, file := range slices.Sorted(maps.Keys(sources)) {
		f, err := parser.ParseFile(fset, file, sources[file], 0)
		require.NoError(t, err, file)
		imports := map[string]string{}
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err, file)
			name := path.Base(ip)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = ip
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			de := &decl{file: file, fd: fd, imports: imports}
			if fd.Recv != nil {
				methods[fd.Name.Name] = append(methods[fd.Name.Name], de)
				continue
			}
			funcs[path.Dir(file)+":"+fd.Name.Name] = append(funcs[path.Dir(file)+":"+fd.Name.Name], de)
			if file == recordingCallbackFile && fd.Name.Name == recordingCallbackFunc {
				start = de
			}
		}
	}
	if start == nil {
		return []string{recordingCallbackFunc + " is not declared in " + recordingCallbackFile}
	}
	seen := map[*decl]bool{start: true}
	queue := []*decl{start}
	found := map[string]bool{}
	reach := func(ds []*decl) {
		for _, d := range ds {
			if !seen[d] {
				seen[d] = true
				queue = append(queue, d)
			}
		}
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		where := d.file + ":" + d.fd.Name.Name
		var walk func(n ast.Node) bool
		walk = func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if slices.Contains(lockTakingNames, v.Sel.Name) {
					found[where+" calls "+v.Sel.Name] = true
				}
				x, isIdent := v.X.(*ast.Ident)
				ip, isImport := "", false
				if isIdent {
					ip, isImport = d.imports[x.Name]
				}
				switch {
				case isImport && ip == flockImport:
					found[where+" calls into the flock package"] = true
				case isImport && ip == skillinstallImport:
					found[where+" calls into the skill installer"] = true
				case isImport && strings.HasPrefix(ip, modulePrefix):
					reach(funcs[strings.TrimPrefix(ip, modulePrefix)+":"+v.Sel.Name])
				case isImport:
				default:
					reach(methods[v.Sel.Name])
				}
				ast.Inspect(v.X, walk)
				return false
			case *ast.Ident:
				if slices.Contains(lockTakingNames, v.Name) {
					found[where+" calls "+v.Name] = true
				}
				reach(funcs[path.Dir(d.file)+":"+v.Name])
			}
			return true
		}
		ast.Inspect(d.fd.Body, walk)
	}
	return slices.Sorted(maps.Keys(found))
}

// TestTheUploadLockIsTakenOnlyWhereListed asserts the upload lock is taken
// from one function of the sender; that the sender's entry is referenced once,
// in a variable; that the function building the sender's input calls that
// variable once; that run calls that function once, after the root command
// returned; that the function the sender calls to resolve the recording
// controls, and every function it reaches, takes no lock; and that the import
// closure of the sender holds no package that takes a lock or reads a
// credential on any release platform. The fixtures assert each way of breaking
// a rule is reported, and that a closure holding the token, config, skill
// installer or keychain package is reported.
func TestTheUploadLockIsTakenOnlyWhereListed(t *testing.T) {
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) {
			continue
		}
		b, err := os.ReadFile(path)
		require.NoError(t, err, path)
		sources[path] = string(b)
	}
	assert.Empty(t, uploadLockFindings(t, sources))
	assert.Empty(t, recordingLockFindings(t, sources))

	for _, p := range ciPlatforms {
		assert.Empty(t, lockHolderFindings(t, p.goos, p.goarch, "./"+telemetrysendDir), p.goos+"/"+p.goarch)
	}

	const base = "./test/conformance/testdata/importguard/"
	for fixture, holder := range map[string]string{
		"token":        modulePrefix + "internal/token",
		"config":       modulePrefix + "internal/config",
		"skillinstall": modulePrefix + "internal/skillinstall",
		"keyring":      "github.com/zalando/go-keyring",
	} {
		assert.Contains(t, lockHolderFindings(t, "linux", "amd64", base+fixture), "linux/amd64: closure holds "+holder, fixture)
	}
	assert.Empty(t, lockHolderFindings(t, "linux", "amd64", base+"clean"))

	good := map[string]string{
		uploadLockTakerFile: "package a\nfunc run() { s.TakeUploadLock() }\n",
		"internal/cli/upload.go": "package cli\nvar sendStoredEvents = telemetrysend.Send\n" +
			"func uploadStoredEvents() { sendStoredEvents() }\n",
		"internal/cli/run.go": "package cli\nfunc run() { runArgv(); uploadStoredEvents() }\n",
	}
	assert.Empty(t, uploadLockFindings(t, good))
	for name, mutate := range map[string]func(map[string]string){
		"a take from internal/cli": func(m map[string]string) {
			m["internal/cli/other.go"] = "package cli\nfunc other() { s.TakeUploadLock() }\n"
		},
		"a take in another function": func(m map[string]string) {
			m[uploadLockTakerFile] = "package a\nfunc other() { s.TakeUploadLock() }\n"
		},
		"two takes": func(m map[string]string) {
			m[uploadLockTakerFile] = "package a\nfunc run() { s.TakeUploadLock(); s.TakeUploadLock() }\n"
		},
		"a second reference to the sender": func(m map[string]string) {
			m["internal/cli/other.go"] = "package cli\nvar x = telemetrysend.Send\n"
		},
		"a second call of the sender's variable from another function": func(m map[string]string) {
			m["internal/cli/other.go"] = "package cli\nfunc other() { sendStoredEvents() }\n"
		},
		"a second call of the sender's variable in the upload function": func(m map[string]string) {
			m["internal/cli/upload.go"] = "package cli\nvar sendStoredEvents = telemetrysend.Send\n" +
				"func uploadStoredEvents() { sendStoredEvents(); sendStoredEvents() }\n"
		},
		"no call of the sender's variable": func(m map[string]string) {
			m["internal/cli/upload.go"] = "package cli\nvar sendStoredEvents = telemetrysend.Send\n" +
				"func uploadStoredEvents() {}\n"
		},
		"the upload before the root command ran": func(m map[string]string) {
			m["internal/cli/run.go"] = "package cli\nfunc run() { uploadStoredEvents(); runArgv() }\n"
		},
		"the upload from another function": func(m map[string]string) {
			m["internal/cli/run.go"] = "package cli\nfunc run() { runArgv() }\nfunc other() { uploadStoredEvents() }\n"
		},
	} {
		m := maps.Clone(good)
		mutate(m)
		assert.NotEmpty(t, uploadLockFindings(t, m), name)
	}
}

const uploadLockTakerFile = "internal/telemetrysend/send.go"

// TestTheRecordingCallbackReachesNoLock asserts each way a function reached
// from the recording controls callback takes a lock is reported, and that a
// callback that only reads is not.
func TestTheRecordingCallbackReachesNoLock(t *testing.T) {
	const callback = "package cli\n" +
		"import \"github.com/rancher/rancher-cli/internal/config\"\n" +
		"func recordingStillOn(dir string) bool { c, _ := config.Load(dir); return c.read() }\n"
	good := map[string]string{
		recordingCallbackFile:         callback,
		"internal/cli/other.go":       "package cli\nfunc other() { s.TakeUploadLock() }\n",
		"internal/config/config.go":   "package config\nfunc Load(p string) (*Config, error) { return load(p) }\nfunc load(p string) (*Config, error) { return nil, nil }\n",
		"internal/config/lockform.go": "package config\nimport \"github.com/gofrs/flock\"\nfunc withDirLock() { flock.New(\"x\") }\n",
		"internal/config/methods.go":  "package config\nfunc (c *Config) read() bool { return true }\n",
	}
	assert.Empty(t, recordingLockFindings(t, good))
	for name, mutate := range map[string]func(map[string]string){
		"a config lock in the callback": func(m map[string]string) {
			m[recordingCallbackFile] = "package cli\nfunc recordingStillOn(dir string) bool { config.WithLock(nil, dir, nil); return true }\n"
		},
		"an upload lock take in the callback": func(m map[string]string) {
			m[recordingCallbackFile] = "package cli\nfunc recordingStillOn(dir string) bool { s.TakeUploadLock(); return true }\n"
		},
		"a lock take in a function of another package that the callback calls": func(m map[string]string) {
			m["internal/config/config.go"] = "package config\nfunc Load(p string) (*Config, error) { return load(p) }\n" +
				"func load(p string) (*Config, error) { lockedRead(); return nil, nil }\n"
			m["internal/config/lockform.go"] = "package config\nimport \"github.com/gofrs/flock\"\n" +
				"func lockedRead() { fl := flock.New(\"x\"); fl.Lock() }\n"
		},
		"a lock take in a method that the callback calls": func(m map[string]string) {
			m["internal/config/methods.go"] = "package config\nfunc (c *Config) read() bool { return withHelperRecordsLock() }\n" +
				"func withHelperRecordsLock() bool { WithLock(nil, \"\", nil); return true }\n"
		},
		"a call into the skill installer": func(m map[string]string) {
			m["internal/config/config.go"] = "package config\nimport \"github.com/rancher/rancher-cli/internal/skillinstall\"\n" +
				"func Load(p string) (*Config, error) { skillinstall.Install(); return nil, nil }\n"
		},
		"a lock take in a function the callback calls in its own package": func(m map[string]string) {
			m[recordingCallbackFile] = "package cli\nfunc recordingStillOn(dir string) bool { return helper() }\n" +
				"func helper() bool { s.TakeUploadLock(); return true }\n"
		},
		"no callback": func(m map[string]string) {
			delete(m, recordingCallbackFile)
		},
	} {
		m := maps.Clone(good)
		mutate(m)
		assert.NotEmpty(t, recordingLockFindings(t, m), name)
	}
}

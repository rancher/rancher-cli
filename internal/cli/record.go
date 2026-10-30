package cli

import (
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/token"
	ucli "github.com/urfave/cli/v3"
)

// keptLine is the parse runArgv made of a command line, and whether it made
// one: it makes none for an empty argv or a completion request.
type keptLine struct {
	line   parsedLine
	parsed bool
}

// recordingState is what the root Before hook keeps for the run: the
// resolved config directory, the telemetry key of the config it loaded
// (unreadable when the load failed), whether recording is on, and, only when
// it is on, whether the directory existed when the hook ran. The hook checks
// before any command can create the directory.
type recordingState struct {
	dir        string
	setting    telemetry.ConfigSetting
	on         bool
	dirExisted bool
}

var (
	recordingMu    sync.Mutex
	recordingHooks recordingState
)

// resetRecordingState says recording is off. run calls it before urfave
// runs, and the hook before it decides; a run that never reaches the hook
// records nothing.
func resetRecordingState() {
	recordingMu.Lock()
	defer recordingMu.Unlock()
	recordingHooks = recordingState{}
}

// noteRecording decides, for the run the hook belongs to, whether recording
// is on, and checks the directory only when it is.
func noteRecording(dir string, setting telemetry.ConfigSetting) {
	st := recordingState{dir: dir, setting: setting}
	st.on = telemetry.Resolve(telemetry.ControlInput{Getenv: os.Getenv, Config: setting}).On
	if st.on {
		st.dirExisted = telemetry.DirExists(dir)
	}
	recordingMu.Lock()
	defer recordingMu.Unlock()
	recordingHooks = st
}

// recordingStateNow returns the state the hook last kept.
func recordingStateNow() recordingState {
	recordingMu.Lock()
	defer recordingMu.Unlock()
	return recordingHooks
}

// telemetryExcluded maps the registered names of the commands that record
// nothing to the reason. The telemetry commands show and delete what
// recording keeps; the completion command prints a shell script.
var telemetryExcluded = map[string]string{
	"completion":       "prints a shell script",
	"telemetry status": "reports what recording keeps",
	"telemetry on":     "changes whether recording is on",
	"telemetry off":    "changes whether recording is on and deletes what it kept",
	"telemetry reset":  "deletes what recording kept",
	"telemetry show":   "lists what recording kept",
}

// now reads the clock for the duration of a run. It is a variable so a test
// controls the duration.
var now = time.Now

// newRunEvent builds the event of a recorded run. It is a variable so a test
// counts the calls: a run that records nothing builds nothing.
var newRunEvent = telemetry.NewCommandRun

// recordDecision says whether a run records and, when it does, the
// descriptor of the command it ran. It reads only the parse and the hook
// state; a run that records nothing does no other work. A run records
// when the line was parsed, urfave runs a leaf command that has a descriptor
// and is not excluded, and the hook found recording on and the config
// directory present. A line the CLI refuses before urfave parses it never
// reaches the hook, and run clears the state first: the state says off.
//
// p.levels holds the walk to endIfBlankAccepted and urfaveEnd is where
// urfave ends; they differ only on lines the CLI refuses before urfave runs
// them, which never get this far; indexing the levels by urfaveEnd is
// safe.
func recordDecision(k keptLine, st recordingState) (runtime.Descriptor, bool) {
	if !k.parsed || !st.on || !st.dirExisted {
		return runtime.Descriptor{}, false
	}
	p := k.line
	end := p.urfaveEnd
	if end.kind != lineRuns || end.level >= len(p.levels) {
		return runtime.Descriptor{}, false
	}
	lv := p.levels[end.level]
	if !lv.leaf {
		return runtime.Descriptor{}, false
	}
	d, ok := commandDescriptor(lv.cmd)
	if !ok {
		return runtime.Descriptor{}, false
	}
	if _, excluded := telemetryExcluded[d.Name]; excluded {
		return runtime.Descriptor{}, false
	}
	return d, true
}

// recordingTarget is recordDecision, and the canonical URL of the server the
// run targeted, "" when it had none. It is the decision that a run records an
// event, and for which server, that the event and the server facts refresh
// follow; the first run of a command in firstLoginCommands on a fresh install has
// its own, firstLoginRefresh.
func recordingTarget(k keptLine, st recordingState) (d runtime.Descriptor, server string, ok bool) {
	d, ok = recordDecision(k, st)
	if !ok {
		return runtime.Descriptor{}, "", false
	}
	return d, recordedServer(d, k.line, storedConfigSnapshot()), true
}

// recordRun records one usage event for the run that ended with res, when
// the decision says so. Nothing it does changes the run's output or exit
// code: a failure to record is dropped, and a panic is recovered. It returns
// what the server facts refresh needs: the server, the recording options and
// what the event read of the server's facts file; for the first run of a
// command in firstLoginCommands on a fresh install, which records no event,
// what firstLoginRefresh gives; the zero value when the run records nothing.
func recordRun(root *ucli.Command, k keptLine, res runResult, start time.Time) (in refreshInput) {
	defer func() {
		if recover() != nil {
			in = refreshInput{}
		}
	}()
	st := recordingStateNow()
	d, server, ok := recordingTarget(k, st)
	if !ok {
		return firstLoginRefresh(k, st, res)
	}
	run := telemetry.RunInput{
		Invocation:  currentInvocation(),
		Command:     d.Name,
		Flags:       typedFlagNames(k.line),
		Outcome:     telemetry.OutcomeOK,
		ExitCode:    res.code,
		ExitCodeSet: true,
		Duration:    now().Sub(start),
		StdoutTTY:   fileIsTTY(root.Writer),
	}
	if res.err != nil {
		run.Outcome = telemetry.OutcomeError
		run.ErrorType = res.err.Code
		run.ErrorCodes = clierr.AllCodes()
	}
	telemetry.SetServer(server, "")
	opts := telemetry.Options{
		ConfigDir:  st.dir,
		DirExisted: st.dirExisted,
		Getenv:     os.Getenv,
		Config:     st.setting,
	}
	_, read := telemetry.Record(newRunEvent(run), opts)
	return refreshInput{server: server, opts: opts, start: read}
}

// firstLoginCommands are the commands whose run may create the config
// directory and then write the server's facts file, with no event: the first
// run of auth login, auth get-token or token on a fresh install.
var firstLoginCommands = []string{"auth login", "auth get-token", "token"}

// firstLoginRefresh is what a run that created the config directory hands the
// server facts refresh, and the zero value when the run is not one. It says
// yes when recording was on and the directory was missing when the run
// started, the command is one of firstLoginCommands, the run succeeded, the
// line names a server, and the directory exists now. The directory check is
// last: every other run with recording on and no directory stops before it
// and does no file operation. The run records no event and calls neither
// telemetry.Record nor telemetry.SetServer; the refresh starts from what the
// server's facts file holds now; a value another process stored is kept.
func firstLoginRefresh(k keptLine, st recordingState, res runResult) refreshInput {
	if !st.on || st.dirExisted {
		return refreshInput{}
	}
	existed := st
	existed.dirExisted = true
	d, ok := recordDecision(k, existed)
	if !ok || !slices.Contains(firstLoginCommands, d.Name) || res.code != 0 {
		return refreshInput{}
	}
	server := recordedServer(d, k.line, storedConfigSnapshot())
	if server == "" || !telemetry.DirExists(st.dir) {
		return refreshInput{}
	}
	opts := telemetry.Options{
		ConfigDir:  st.dir,
		DirExisted: true,
		Getenv:     os.Getenv,
		Config:     st.setting,
	}
	return refreshInput{server: server, opts: opts, start: telemetry.ReadServerFacts(server, opts)}
}

// typedFlagNames returns the long names of the flags typed on the line:
// the flags each level declares itself, set from a typed word, the built-in
// help and version flags left out. A flag set from the environment was not
// typed, and a word after -- is not a flag.
func typedFlagNames(p parsedLine) []string {
	var names []string
	for _, lv := range p.levels {
		for _, f := range lv.flags[:lv.own] {
			if f.set && !f.builtin && !slices.Contains(names, f.names[0]) {
				names = append(names, f.names[0])
			}
		}
	}
	return names
}

// typed returns the text of the value flag called name as typed on the line,
// and whether it was typed.
func (lv *lineLevel) typed(name string) (string, bool) {
	for _, f := range lv.flags[:lv.own] {
		if f.set && slices.Contains(f.names, name) {
			return f.text, true
		}
	}
	return "", false
}

// typedOn reports whether the bool flag called name was typed as true.
func (lv *lineLevel) typedOn(name string) bool {
	for _, f := range lv.flags[:lv.own] {
		if f.set && f.value && slices.Contains(f.names, name) {
			return true
		}
	}
	return false
}

// serverInput is what a command's server row reads: the leaf level of the
// line, with its typed flags and positional words, and the config the root
// Before hook loaded, nil when it loaded none.
type serverInput struct {
	leaf *lineLevel
	cfg  *config.Config
}

// positional returns the first positional word, or "".
func (in serverInput) positional() string {
	if len(in.leaf.args) == 0 {
		return ""
	}
	return in.leaf.args[0]
}

// entryURL returns the URL of the server entry called name, or "".
func (in serverInput) entryURL(name string) string {
	if in.cfg == nil {
		return ""
	}
	return in.cfg.Servers[name].URL
}

// currentURL returns the URL of the current server entry, or "".
func (in serverInput) currentURL() string {
	if in.cfg == nil {
		return ""
	}
	_, s, err := in.cfg.CurrentServerEntry()
	if err != nil {
		return ""
	}
	return s.URL
}

// entryOrCurrentURL returns the URL of the entry the positional word names,
// else of the current entry. A name no entry has gives "".
func (in serverInput) entryOrCurrentURL() string {
	if name := in.positional(); name != "" {
		return in.entryURL(name)
	}
	return in.currentURL()
}

// serverRows maps the registered name of each command that names its own
// server to the function that reads that server from the line. A command
// with server access read or write that is not here records the current
// server of the loaded config. The functions return the URL as typed or
// stored; recordedServer canonicalizes it.
var serverRows = map[string]func(serverInput) string{
	"auth get-token": func(in serverInput) string {
		v, typed := in.leaf.typed("server")
		if !typed {
			v = os.Getenv(config.EnvURL)
		}
		if v == "" && in.cfg != nil {
			v = effectiveConnectionURL(in.cfg)
		}
		return token.ApplyDefaultScheme(v)
	},
	"token": func(in serverInput) string {
		v, _ := in.leaf.typed("server")
		return token.ApplyDefaultScheme(strings.TrimSpace(v))
	},
	"auth login": func(in serverInput) string {
		if v, _ := in.leaf.typed("url"); v != "" {
			return v
		}
		return in.entryOrCurrentURL()
	},
	"auth logout": func(in serverInput) string {
		if in.leaf.typedOn("all") {
			return ""
		}
		return in.entryOrCurrentURL()
	},
	"auth delete-token": func(serverInput) string { return "" },
	"config add-server": func(in serverInput) string {
		v, _ := in.leaf.typed("url")
		return v
	},
	"config update-server": func(in serverInput) string {
		if v, _ := in.leaf.typed("url"); v != "" {
			return v
		}
		return in.entryURL(in.positional())
	},
	"config delete-server": func(in serverInput) string {
		return in.entryURL(in.positional())
	},
}

// effectiveConnectionURL is the URL the current server and the environment
// give, or "" when it is refused: a refused URL records no server.
func effectiveConnectionURL(cfg *config.Config) string {
	eff := cfg.EffectiveConnection()
	if eff.URLErr != nil {
		return ""
	}
	return eff.URL
}

// recordedServer returns the canonical URL of the server the run targeted,
// or "" when it had none. A command whose server access is none has none.
// Otherwise a command in serverRows reads its own server, and any other
// command targets the current server of cfg. A URL that does not
// canonicalize is no server.
func recordedServer(d runtime.Descriptor, p parsedLine, cfg *config.Config) string {
	if d.EffectiveServerAccess() == runtime.ServerAccessNone {
		return ""
	}
	var raw string
	if row, ok := serverRows[d.Name]; ok {
		raw = row(serverInput{leaf: p.levels[p.urfaveEnd.level], cfg: cfg})
	} else if cfg != nil {
		raw = effectiveConnectionURL(cfg)
	}
	if raw == "" {
		return ""
	}
	canonical, err := credhelper.CanonicalServerURL(raw)
	if err != nil {
		return ""
	}
	return canonical
}

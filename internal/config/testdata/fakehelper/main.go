// Command fakehelper is a test credential helper. Behavior is selected by
// FAKEHELPER_MODE: ok (in-memory via state file), notfound, unsupported,
// fail, garbage, hang, expired, expiresat (uses FAKEHELPER_EXPIRESAT),
// badexpiry (get returns a token with a non-RFC3339 expiresAt), silent
// (store/erase exit 0 with NO stdout - protocol-compliant empty success;
// get behaves like ok), commit-fail / commit-garbage / commit-hang (store
// persists the credential and only then fails, answers garbage, or hangs),
// slow-ok (behaves like ok, but every call first waits FAKEHELPER_DELAY, a
// Go duration, so a test can tell an answer that is late from one that
// never comes).
// FAKEHELPER_STATE points at a JSON state file.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/config"
)

// envPrefix lets one binary serve as two distinct helpers: a copy named
// rancher-credential-fake2 reads FAKEHELPER2_* variables (falling back to
// FAKEHELPER_* when unset), so mixed-success delete tests can drive two
// helpers into different modes.
func envPrefix() string {
	if strings.Contains(filepath.Base(os.Args[0]), "fake2") {
		return "FAKEHELPER2"
	}
	return "FAKEHELPER"
}

func envVal(suffix string) string {
	if v := os.Getenv(envPrefix() + "_" + suffix); v != "" {
		return v
	}
	return os.Getenv("FAKEHELPER_" + suffix)
}

func main() {
	// noop: exit 0 at once with no side effects. Build sites run it once
	// right after building, so the binary's first run happens outside any
	// helper time limit.
	if len(os.Args) == 2 && os.Args[1] == "noop" {
		os.Exit(0)
	}

	// lockhold <lockpath> <duration>: acquire the flock at lockpath, announce
	// it on stdout AFTER acquisition, hold for the duration. Used by the
	// cross-process lock tests (contention, bounded wait, death). The caller
	// supplies the full lock-file path so the file name has exactly one
	// definition (helperRecordsLockName in internal/config) - a hardcoded
	// copy here would silently turn the contention tests into false passes
	// if the real name ever changed.
	if len(os.Args) >= 4 && os.Args[1] == "lockhold" {
		d, err := time.ParseDuration(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "lockhold:", err)
			os.Exit(1)
		}
		fl := flock.New(os.Args[2])
		if err := fl.Lock(); err != nil {
			fmt.Fprintln(os.Stderr, "lockhold:", err)
			os.Exit(1)
		}
		fmt.Println("locked")
		time.Sleep(d)
		os.Exit(0)
	}

	// configwrite [-nolock] <dir> <server-name>: load config.yaml, announce
	// "loaded" on stdout, wait for <dir>/release, add the named server entry,
	// write, announce "done". The "loaded" line carries the names of the
	// servers the load found, sorted and separated by one space: "loaded" alone
	// for a config.yaml with no server, "loaded server-a" for one entry. With
	// -nolock the entire sequence runs without calling config.WithLock; without
	// -nolock it runs inside WithLock. The ready/start/loaded/release/done
	// barrier protocol lets the parent synchronize two children so that both
	// have loaded before either writes, which deterministically exposes the
	// lost-update bug the lock closes.
	if len(os.Args) >= 4 && os.Args[1] == "configwrite" {
		nolock := false
		args := os.Args[2:]
		if args[0] == "-nolock" {
			nolock = true
			args = args[1:]
		}
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "configwrite: usage: configwrite [-nolock] <dir> <server-name>")
			os.Exit(1)
		}
		dir, serverName := args[0], args[1]

		pollFor := func(phase, path string) {
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(path); err == nil {
					return
				}
				if time.Now().After(deadline) {
					fmt.Fprintf(os.Stderr, "configwrite: timed out waiting for %s\n", phase)
					os.Exit(1)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}

		fmt.Println("ready")
		pollFor("start", filepath.Join(dir, "start"))

		doWork := func() error {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return err
			}
			loaded := "loaded"
			for _, name := range slices.Sorted(maps.Keys(cfg.Servers)) {
				loaded += " " + name
			}
			fmt.Println(loaded)
			pollFor("release", filepath.Join(dir, "release"))
			if cfg.Servers == nil {
				cfg.Servers = map[string]config.Server{}
			}
			cfg.Servers[serverName] = config.Server{URL: "https://" + serverName + ".example"}
			if err := cfg.Write(); err != nil {
				return err
			}
			// Print "done" while the lock is still held: the second writer
			// takes the lock, and loads, only after this entry is on disk.
			fmt.Println("done")
			return nil
		}

		var err error
		if nolock {
			err = doWork()
		} else {
			err = config.WithLock(context.Background(), dir, doWork)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "configwrite:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	mode := envVal("MODE")
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "fakehelper: deliberate failure")
		os.Exit(1)
	case "garbage":
		fmt.Print("this is not json")
		os.Exit(0)
	case "hang":
		time.Sleep(10 * time.Minute)
	case "slow-ok":
		d, err := time.ParseDuration(os.Getenv("FAKEHELPER_DELAY"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakehelper: slow-ok needs FAKEHELPER_DELAY:", err)
			os.Exit(1)
		}
		time.Sleep(d)
	case "badexpiry":
		// Bypass the kit (it formats valid RFC 3339): emit raw JSON.
		fmt.Print(`{"protocolVersion":1,"token":"t","expiresAt":"not-a-timestamp"}`)
		os.Exit(0)
	case "silent":
		// store/erase: exit 0, no stdout (compliant empty success).
		if len(os.Args) > 1 && os.Args[1] != "get" {
			os.Exit(0)
		}
	}
	credhelper.Serve(handler{mode: mode, state: envVal("STATE")})
}

type handler struct{ mode, state string }

type stateFile struct {
	Creds map[string]string `json:"creds"` // serverURL+"\n"+name -> token
	Log   []string          `json:"log"`   // verb + " " + serverURL + " " + name, for raw-identity assertions
}

func (h handler) load() (*stateFile, error) {
	s := &stateFile{Creds: map[string]string{}}
	b, err := os.ReadFile(h.state)
	if err == nil {
		if jerr := json.Unmarshal(b, s); jerr != nil {
			return nil, jerr
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (h handler) save(s *stateFile) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(h.state, b, 0o600)
}

func (h handler) log(s *stateFile, verb, u, n string) { s.Log = append(s.Log, verb+" "+u+" "+n) }

func (h handler) Get(u, n string) (*credhelper.Credentials, error) {
	switch h.mode {
	case "notfound":
		return nil, nil
	case "expired":
		return &credhelper.Credentials{Token: "stale", ExpiresAt: time.Now().Add(-time.Hour)}, nil
	case "expiresat":
		exp, err := time.Parse(time.RFC3339, os.Getenv("FAKEHELPER_EXPIRESAT"))
		if err != nil {
			return nil, err
		}
		return &credhelper.Credentials{Token: "short-lived", ExpiresAt: exp}, nil
	}
	s, err := h.load()
	if err != nil {
		return nil, err
	}
	h.log(s, "get", u, n)
	defer func() { _ = h.save(s) }()
	if tok, ok := s.Creds[u+"\n"+n]; ok {
		return &credhelper.Credentials{Token: tok}, nil
	}
	return nil, nil
}

func (h handler) Store(u, n, tok string) error {
	if h.mode == "unsupported" {
		return credhelper.ErrStoreUnsupported
	}
	s, err := h.load()
	if err != nil {
		return err
	}
	h.log(s, "store", u, n)
	s.Creds[u+"\n"+n] = tok
	if err := h.save(s); err != nil {
		return err
	}
	// The commit-* modes prove a credential can land and the helper still
	// fail to say so: the caller must classify these as possiblyCommitted.
	switch h.mode {
	case "commit-fail":
		fmt.Fprintln(os.Stderr, "fakehelper: stored, then deliberate failure")
		os.Exit(1)
	case "commit-garbage":
		fmt.Print("this is not json")
		os.Exit(0)
	case "commit-hang":
		time.Sleep(10 * time.Minute)
	}
	return nil
}

func (h handler) Erase(u, n string) (credhelper.EraseOutcome, error) {
	s, err := h.load()
	if err != nil {
		return credhelper.EraseUnknown, err
	}
	h.log(s, "erase", u, n)
	outcome := credhelper.EraseNothingStored
	if _, ok := s.Creds[u+"\n"+n]; ok {
		outcome = credhelper.EraseDeleted
	}
	delete(s.Creds, u+"\n"+n)
	return outcome, h.save(s)
}

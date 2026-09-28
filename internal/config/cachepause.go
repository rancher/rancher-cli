package config

import (
	"os"
	"slices"
)

// A change of an entry's credential - a login, a logout, a URL change, a
// deletion - is several durable writes to different files, and a cache
// writer taking no lock can land between any two. Its first config write
// therefore advances the entry's cache binding id and pauses the entry's
// cache writes in one write, so every record made before it is a miss and
// none is made after it; the pause is cleared by a later write, here,
// once nothing the change left can still move the entry's readers.

// The reasons a pause stays. A resumer that holds the config lock clears
// every pause but these.
const (
	// PauseReasonPinStep: the directory's store change has a pin step
	// left whose store is not the one readers resolve now, so writing it
	// will move every entry's readers.
	PauseReasonPinStep = "pinStep"
	// PauseReasonChangeRecordUnreadable: the store change record cannot
	// be read, so nobody knows what it still has to do.
	PauseReasonChangeRecordUnreadable = "changeRecordUnreadable"
	// PauseReasonNoLock: the change was made with
	// RANCHER_CLI_CONFIG_NO_LOCK, and only the process that made it, or the
	// entry's next change, clears its pause.
	PauseReasonNoLock = "noLock"
	// PauseReasonNone: nothing holds it; the next exec-credential cache
	// miss or credential change resumes it.
	PauseReasonNone = "none"
)

// StartCredentialChange is the first write's change to an entry whose
// credential is about to change: a fresh cache binding id, and its cache
// writes paused, marked as made without the config lock when
// RANCHER_CLI_CONFIG_NO_LOCK is set. The caller writes the config before
// it writes or deletes any of the entry's credential.
func StartCredentialChange(e *Server) error {
	if err := AdvanceCacheBinding(e); err != nil {
		return err
	}
	e.CacheWritesPaused = PauseLocked
	if os.Getenv(EnvConfigNoLock) != "" {
		e.CacheWritesPaused = PauseNoLock
	}
	return nil
}

// ResumeCacheWrites clears, in cfg, every pause this process may clear,
// and reports whether it cleared any. own names the entries whose pause
// this process's own change set. The caller holds the config lock - the
// flock, or under RANCHER_CLI_CONFIG_NO_LOCK only the in-process
// semaphore - and writes cfg.
//
// Nothing is cleared while the store change record cannot be read, or
// while it has a pin step left that would move readers: a record written
// from the store readers are about to leave would pass the store check
// now and be served after the pin lands. A delete step left keeps
// nothing: readers already left the store it deletes from, and the store
// check refuses a writer that did not. Otherwise a process clears its own
// entries' pauses and, holding the flock, every pause made under the
// lock: every change under the lock runs inside one lock span, so a
// holder that finds one knows the change that made it has ended. A pause
// made without the lock is cleared only by its own change.
func ResumeCacheWrites(dir string, cfg *Config, own ...string) bool {
	return resumeWith(cfg, pauseHold(dir, cfg), own)
}

func resumeWith(cfg *Config, hold string, own []string) bool {
	if hold != "" {
		return false
	}
	locked := os.Getenv(EnvConfigNoLock) == ""
	changed := false
	for name, s := range cfg.Servers {
		if !s.Paused() {
			continue
		}
		if !slices.Contains(own, name) && (!locked || s.CacheWritesPaused == PauseNoLock) {
			continue
		}
		s.CacheWritesPaused = ""
		cfg.Servers[name] = s
		changed = true
	}
	return changed
}

// pauseHold is why the directory keeps every pause it has, or empty when
// nothing does.
func pauseHold(dir string, cfg *Config) string {
	state, m, err := LoadTransition(dir, cfg)
	switch {
	case err != nil:
		return PauseReasonChangeRecordUnreadable
	case state == TransitionActive && pinMovesReaders(dir, cfg, m):
		return PauseReasonPinStep
	}
	return ""
}

// pinMovesReaders reports whether a pin step the change has left names a
// store other than the one readers resolve now from cfg's declaration and
// the pin, or readers resolve none. A reader with the variable set on an
// undeclared directory ignores the pin; the ones without it are the ones
// a pin moves, so the variable is not applied here.
func pinMovesReaders(dir string, cfg *Config, m *transitionManifest) bool {
	for _, s := range m.Steps {
		if s.Kind != transitionPin {
			continue
		}
		now, _, err := resolveBackend(declaredPolicy(cfg), dir, func() (*Config, error) { return cfg, nil })
		if err != nil || now != s.Target {
			return true
		}
	}
	return false
}

// PausedEntry is an entry whose exec-credential cache writes are paused,
// and why the pause stays: one of the PauseReason values.
type PausedEntry struct {
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// PausedEntries lists the entries of cfg whose cache writes are paused,
// sorted, each with its reason.
func PausedEntries(dir string, cfg *Config) []PausedEntry {
	var names []string
	for name, s := range cfg.Servers {
		if s.Paused() {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	hold := pauseHold(dir, cfg)
	out := make([]PausedEntry, 0, len(names))
	for _, name := range names {
		out = append(out, PausedEntry{Entry: name, Reason: pauseReason(cfg.Servers[name], hold)})
	}
	return out
}

// pauseReason is why s's pause stays, given the directory's hold.
func pauseReason(s Server, hold string) string {
	switch {
	case !s.Paused():
		return ""
	case hold != "":
		return hold
	case s.CacheWritesPaused == PauseNoLock:
		return PauseReasonNoLock
	}
	return PauseReasonNone
}

// ResumeResult is what the exec plugin's resume did.
type ResumeResult struct {
	// Tried is true when the config lock was taken and the pauses looked
	// at.
	Tried bool
	// Stays is why the entry's pause stays, one of the PauseReason values,
	// or empty when it no longer does.
	Stays string
}

// TryResumeCacheWrites is the exec plugin's resume of a pause nobody
// cleared - a change that died inside its lock span, or a failure path
// that made no further write. It tries the config lock once, never
// waiting, and never under RANCHER_CLI_CONFIG_NO_LOCK, where a free lock
// proves nothing. Holding it, it clears every pause ResumeCacheWrites
// allows in one config write and says why entry's pause stays, if it
// does. Without it someone is inside a change, and nothing is done. The
// caller holds no other lock: the config lock is never taken while the
// token-cache, identity or helper-records lock is held.
func TryResumeCacheWrites(dir, entry string) (ResumeResult, error) {
	var res ResumeResult
	tried, err := tryWithLock(dir, func() error {
		cfg, err := Load(FilePath(dir))
		if err != nil {
			return err
		}
		hold := pauseHold(dir, cfg)
		if resumeWith(cfg, hold, nil) {
			if werr := writeStep("resume", cfg); werr != nil {
				return werr
			}
		}
		res.Stays = pauseReason(cfg.Servers[entry], hold)
		return nil
	})
	res.Tried = tried
	return res, err
}

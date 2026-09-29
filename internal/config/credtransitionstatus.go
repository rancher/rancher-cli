package config

import (
	"fmt"
)

// The states a change record is reported in.
const (
	// ChangeRecordActive: config.yaml names the record, and it has work
	// left that the next credential command retries.
	ChangeRecordActive = "active"
	// ChangeRecordAbandoned: a valid record config.yaml does not name. The
	// login that wrote it died before starting the change, so none of its
	// work was done, and the next credential command removes it.
	ChangeRecordAbandoned = "abandoned"
	// ChangeRecordDamaged: a record that cannot be read or trusted, named
	// or not. Every credential command that retries a change refuses on it.
	ChangeRecordDamaged = "damaged"
)

// ChangeRecordStatus is the directory's credential store change record as
// the status commands report it: what the prelude of every credential
// command sees, read the same way.
type ChangeRecordStatus struct {
	State string `json:"state"`
	// ID is the record's own id, when it can be read; Named is the id
	// config.yaml names, when it names one.
	ID    string `json:"id,omitempty"`
	Named string `json:"named,omitempty"`
	File  string `json:"file"`
	// Error is why a damaged record cannot be used, with what to do.
	Error     string             `json:"error,omitempty"`
	Remaining []ChangeRecordStep `json:"remaining,omitempty"`
}

// ChangeRecordStep is one piece of work a change still has: its kind, the
// entry it is for, the store it works on, whether any retry can still
// finish it, and why its last attempt stopped.
type ChangeRecordStep struct {
	Kind         string `json:"kind"`
	Entry        string `json:"entry,omitempty"`
	Store        string `json:"store"`
	CannotFinish bool   `json:"cannotFinish,omitempty"`
	LastError    string `json:"lastError,omitempty"`
}

// ChangeRecordStatusOf reports the directory's change record, or nil when
// there is none to report: nothing recorded, or a change whose last step
// finished. It reads what LoadTransition reads - one stat when config.yaml
// names no change and there is no file - opens no credential store and
// takes no lock.
func ChangeRecordStatusOf(dir string, cfg *Config) *ChangeRecordStatus {
	named := ""
	if cfg != nil {
		named = cfg.CredentialTransitionID
	}
	state, m, err := LoadTransition(dir, cfg)
	s := &ChangeRecordStatus{Named: named, File: TransitionPath(dir)}
	switch {
	case err != nil:
		s.State, s.Error = ChangeRecordDamaged, err.Error()
		return s
	case state == TransitionActive:
		s.State = ChangeRecordActive
	case state == TransitionAbandoned:
		s.State = ChangeRecordAbandoned
	default:
		return nil
	}
	s.ID = m.ID
	for _, st := range m.Steps {
		store := st.Source
		if st.Kind == transitionPin {
			store = st.Target
		}
		s.Remaining = append(s.Remaining, ChangeRecordStep{
			Kind: st.Kind, Entry: st.Entry, Store: store, CannotFinish: st.Terminal, LastError: st.LastError,
		})
	}
	return s
}

// Unfinished reports whether the record stops or holds back work: a
// change in flight, or a record that cannot be read. A change that never
// started is not: nothing it records was done.
func (s *ChangeRecordStatus) Unfinished() bool {
	return s != nil && (s.State == ChangeRecordActive || s.State == ChangeRecordDamaged)
}

// Pending is what an error with a remedy of its own says about the
// record: the note while it is unfinished, and the status report as the
// hint while it cannot be read, since every command such a remedy names
// refuses on it.
func (s *ChangeRecordStatus) Pending() UnfinishedChange {
	switch {
	case !s.Unfinished():
		return UnfinishedChange{}
	case s.State == ChangeRecordDamaged:
		return UnfinishedChange{Note: unreadableChangeNote, Refuses: true}
	}
	return UnfinishedChange{Note: unfinishedChangeNote}
}

// TextLines is the report as text, one field per line; the lines under
// remaining are indented two spaces. Both status commands indent it
// under their own heading.
func (s *ChangeRecordStatus) TextLines() []string {
	var lines []string
	switch s.State {
	case ChangeRecordActive:
		lines = append(lines, "state: active - unfinished; the next auth login, auth logout, config delete-server or config update-server --url retries the work left")
	case ChangeRecordAbandoned:
		lines = append(lines, "state: abandoned - a change that never started; the next auth login, auth logout, config delete-server or config update-server --url removes it")
	default:
		lines = append(lines, "state: damaged - "+s.Error)
	}
	if s.ID != "" {
		lines = append(lines, "id: "+s.ID)
	} else if s.Named != "" {
		lines = append(lines, "named: "+s.Named)
	}
	lines = append(lines, "file: "+s.File)
	if len(s.Remaining) > 0 {
		lines = append(lines, "remaining:")
		for _, st := range s.Remaining {
			lines = append(lines, "  "+st.text())
		}
	}
	return lines
}

// text is one step as a line of the report.
func (st ChangeRecordStep) text() string {
	what := fmt.Sprintf("delete the old copy of %s's credential from %s", st.Entry, st.Store)
	if st.Kind == transitionPin {
		what = "switch the pin to " + st.Store
	}
	switch {
	case st.CannotFinish:
		return what + " - cannot finish: " + st.LastError
	case st.LastError != "":
		return what + " - could not finish: " + st.LastError
	}
	return what + " - not tried yet"
}

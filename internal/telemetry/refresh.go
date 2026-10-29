package telemetry

import (
	"errors"
	"io/fs"
	"path"
	"time"
)

// How long a record waits before it is asked for again.
const (
	// refreshAfterSuccess is the wait after a good value.
	refreshAfterSuccess = 24 * time.Hour
	// refreshAfterInvalid is the wait after a 2xx answer the CLI cannot use.
	refreshAfterInvalid = 24 * time.Hour
	// refreshAfterForbidden is the wait after a 403 or 404.
	refreshAfterForbidden = 6 * time.Hour
	// refreshAfterNetwork is the wait after any other failed answer.
	refreshAfterNetwork = time.Hour
)

// Answer is what one request for a server fact got, in plain values:
// telemetry holds no network package. The caller reads the status and the
// body; Value is the version or the install id from a 2xx body and is empty
// when the body had none, was empty, or was longer than the read limit.
type Answer struct {
	// Status is the HTTP status, or 0 when no response came back.
	Status int
	// Value is the value parsed from a 2xx body.
	Value string
	// Edition is the edition parsed from a version answer.
	Edition Edition
	// Cancelled is true when the run's context ended the request.
	Cancelled bool
}

// answerOutcome says what an answer stores: nothing for a request the caller
// cancelled, the value for a 200 that carries one, else the failure kind.
func answerOutcome(a Answer) (store, success bool, kind FailureKind) {
	switch {
	case a.Cancelled:
		return false, false, ""
	case a.Status == 200 && a.Value != "":
		return true, true, ""
	case a.Status >= 200 && a.Status < 300:
		return true, false, FailureInvalid
	case a.Status == 403 || a.Status == 404:
		return true, false, FailureForbidden
	}
	return true, false, FailureNetwork
}

// StoreVersion stores what the answer says into the version record and
// reports whether it changed anything: a good value replaces the record and
// its failure, a failure is kept beside the last good value, and a request
// the caller cancelled stores nothing. The version goes through the version
// rule, as SetVersion does.
func (f *Facts) StoreVersion(a Answer, now time.Time) bool {
	store, success, kind := answerOutcome(a)
	switch {
	case !store:
		return false
	case success:
		f.SetVersion(a.Value, a.Edition, now)
	default:
		f.FailVersion(kind, now)
	}
	return true
}

// StoreInstallID is StoreVersion for the install id record. A value that is
// not a UUID is stored as an invalid failure, as SetInstallID does.
func (f *Facts) StoreInstallID(a Answer, now time.Time) bool {
	store, success, kind := answerOutcome(a)
	switch {
	case !store:
		return false
	case success:
		f.SetInstallID(a.Value, now)
	default:
		f.FailInstallID(kind, now)
	}
	return true
}

// VersionDue reports whether the version record is due for a request at now.
func (f Facts) VersionDue(now time.Time) bool { return f.Version.due(now, false) }

// InstallIDDue reports whether the install id record is due for a request
// at now.
func (f Facts) InstallIDDue(now time.Time) bool { return f.InstallID.due(now, false) }

// InstallIDDueSkippingFailureWait is InstallIDDue for a command that stored the
// token the request carries: a stored failure of any kind is due at once,
// because it may have come from a different token. The wait after a good
// value does not change.
func (f Facts) InstallIDDueSkippingFailureWait(now time.Time) bool { return f.InstallID.due(now, true) }

// due reports whether the record is due at now: nothing is stored, or the
// stored time is later than now (a clock set back never stops a request);
// else a stored failure waits from its own time, and a good value waits from
// the time it was read. With skipFailureWait a stored failure is due at
// once.
func (r Fact) due(now time.Time, skipFailureWait bool) bool {
	if r.Failure != nil {
		if skipFailureWait || r.Failure.At.After(now) {
			return true
		}
		return !now.Before(r.Failure.At.Add(failureWait(r.Failure.Kind)))
	}
	if r.At.IsZero() || r.At.After(now) {
		return true
	}
	return !now.Before(r.At.Add(refreshAfterSuccess))
}

func failureWait(kind FailureKind) time.Duration {
	switch kind {
	case FailureForbidden:
		return refreshAfterForbidden
	case FailureInvalid:
		return refreshAfterInvalid
	}
	return refreshAfterNetwork
}

// FactsRead is what Record read of the server's facts file for the event it
// wrote. Usable is true when the file was read, was missing, or held text
// that is not facts (Facts is then empty); it is false when the run had no
// server or the read failed for another reason, and a writer must not write
// over a file it could not read.
type FactsRead struct {
	Facts  Facts
	Usable bool
}

// SaveFacts writes the facts file of a server under the recording gates: the
// controls are on and the caller says the configuration directory existed:
// at the start of the run for an event, at the end of the run for the facts
// file of the first login. It never creates the configuration directory and
// writes nothing through a telemetry path OpenStore refuses. It reports
// whether the file was written; a failure or a panic is dropped.
func SaveFacts(canonicalURL string, f Facts, opts Options) (written bool) {
	if !Resolve(ControlInput{Getenv: opts.Getenv, Config: opts.Config}).On || !opts.DirExisted {
		return false
	}
	defer func() {
		if recover() != nil {
			written = false
		}
	}()
	s, err := OpenStore(opts.ConfigDir)
	if err != nil {
		return false
	}
	defer func() { _ = s.Close() }()
	return s.WriteFacts(canonicalURL, f) == nil
}

// ReadServerFacts reads the facts file of a server under the recording
// gates, as SaveFacts writes it: the controls are on and the caller says the
// configuration directory existed. The result means what Record's read means:
// a facts file that is read or missing, or no telemetry directory, is usable;
// a file that holds text that is not facts counts as missing; any other read
// failure, a telemetry path OpenStore refuses, an off control, a missing
// directory and a panic are not usable, and a writer must not write over a
// file it could not read. It never creates a file or a directory.
func ReadServerFacts(canonicalURL string, opts Options) (read FactsRead) {
	if !Resolve(ControlInput{Getenv: opts.Getenv, Config: opts.Config}).On || !opts.DirExisted {
		return FactsRead{}
	}
	defer func() {
		if recover() != nil {
			read = FactsRead{}
		}
	}()
	s, err := OpenStore(opts.ConfigDir)
	if err != nil {
		return FactsRead{}
	}
	defer func() { _ = s.Close() }()
	if s.tel == nil {
		return FactsRead{Usable: true}
	}
	data, err := s.readFile(path.Join(dirServers, FactsFileName(canonicalURL)))
	return classifyFactsRead(data, err)
}

// classifyFactsRead turns the result of reading a facts file into what the
// reader may rely on: a read file is usable with its facts (empty when the
// text is not facts), a missing file is usable and empty, and any other
// error is not usable.
func classifyFactsRead(data []byte, err error) (read FactsRead) {
	switch {
	case err == nil:
		read.Facts, _ = parseFacts(data)
		read.Usable = true
	case errors.Is(err, fs.ErrNotExist):
		read.Usable = true
	}
	return read
}

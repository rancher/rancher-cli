package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Paths below the telemetry directory that belong to the upload stage.
const (
	dirUpload        = "upload"
	fileLock         = "lock"
	fileState        = "state.json"
	lockTempPrefix   = ".lock-"
	stateTempName    = ".state.json.tmp"
	maxStateSize     = 64 * 1024
	maxLockSize      = 512
	oversizedContent = "(oversized)"
)

var (
	// ErrUploadLockHeld means another process holds the upload lock.
	ErrUploadLockHeld = errors.New("another upload is running")
	// ErrUploadLockUnreadable means a lock exists that cannot be read: it is
	// not a regular file, a symlink that leads nowhere, or a file the process
	// may not read. Such a lock is never judged stale and never taken over;
	// `telemetry off` and `telemetry reset` remove it with the rest of
	// telemetry/upload. On Windows a regular file whose read fails with access
	// denied counts as in use by another process, and is held instead.
	ErrUploadLockUnreadable = errors.New("the upload lock cannot be read")
	// ErrNoTelemetryDirectory means the configuration directory has no
	// telemetry directory, so there is nothing to upload and no lock to take.
	ErrNoTelemetryDirectory = errors.New("no telemetry directory")
	// ErrUnsafeUploadDir is the error for a telemetry/upload path that is a
	// symlink or not a directory. UploadLockStatus also returns it, wrapped,
	// for a path whose type cannot be read.
	ErrUnsafeUploadDir = errors.New("the upload directory is a symlink or not a directory")
)

// UploadLock is the upload lock of one telemetry directory, held through the
// Store that took it; after that Store is closed, Release and WriteState
// return an error.
//
// The lock is the file telemetry/upload/lock, which holds the time it was
// created and a random token. It is placed by writing a temporary file and
// linking it to that name, so the link fails when a lock exists and a reader
// never sees a lock that is empty or half written. A lock is stale when the
// time in it is a minute or more in the past, more than a minute in the
// future, or does not parse; it is never judged by the file's modification
// time. The lock is never taken while another lock is held, and code holding
// it takes no other lock. It is not refreshed: a holder finishes well inside
// the minute, or another process takes it over. Two takers that reach the
// removal of one stale lock in the same short window can both hold it, which
// costs only duplicate sends, because the endpoint removes duplicates.
type UploadLock struct {
	s     *Store
	token string
}

// TakeUploadLock creates telemetry/upload when it is missing and takes the
// upload lock. It returns ErrNoTelemetryDirectory when there is no telemetry
// directory, and creates nothing then. It returns ErrUploadLockHeld when
// another process holds a lock that is not stale, and ErrUploadLockUnreadable
// when the lock cannot be read. A link that fails for any
// other reason, such as a file system without hard links, is returned as it
// is: the upload is skipped, and the lock is never created another way.
func (s *Store) TakeUploadLock() (*UploadLock, error) {
	if s.tel == nil {
		return nil, ErrNoTelemetryDirectory
	}
	if err := s.ensureUploadDir(); err != nil {
		return nil, err
	}
	token, lost, err := s.placeLock()
	if err != nil && !lost {
		return nil, err
	}
	if lost {
		token, err = s.takeOverStaleLock()
		if err != nil {
			return nil, err
		}
	}
	s.removeOldLockTemps()
	return &UploadLock{s: s, token: token}, nil
}

// ensureUploadDir creates telemetry/upload when it is missing and refuses a
// path that is a symlink or not a directory.
func (s *Store) ensureUploadDir() error {
	if err := s.tel.Mkdir(dirUpload, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := s.tel.Lstat(dirUpload)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeUploadDir
	}
	return nil
}

// placeLock writes the lock content to a temporary file and links it to the
// lock name. lost is true when the link failed because a lock exists. The
// temporary name is removed whatever the result.
func (s *Store) placeLock() (token string, lost bool, err error) {
	token, err = newUUID()
	if err != nil {
		return "", false, err
	}
	content := nowFunc().UTC().Format(time.RFC3339Nano) + " " + token
	tmp := path.Join(dirUpload, lockTempPrefix+token)
	f, err := s.tel.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, err
	}
	werr := writeAll(f, tmp, []byte(content))
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = s.tel.Remove(tmp)
		return "", false, err
	}
	pause("lock-created")
	err = s.tel.Link(tmp, path.Join(dirUpload, fileLock))
	_ = s.tel.Remove(tmp)
	if err != nil {
		return "", errors.Is(err, fs.ErrExist), err
	}
	return token, false, nil
}

// takeOverStaleLock runs after a link found a lock. It returns
// ErrUploadLockHeld unless that lock is stale and still unchanged when it is
// removed, and then links once more; whoever links first holds the lock.
func (s *Store) takeOverStaleLock() (string, error) {
	lock := path.Join(dirUpload, fileLock)
	first, err := s.readLockContent(lock)
	if err != nil {
		return "", s.lockReadFailure(lock, err)
	}
	if !lockStale(first, nowFunc().UTC()) {
		return "", ErrUploadLockHeld
	}
	pause("lock-judged")
	if second, err := s.readLockContent(lock); err != nil || second != first {
		return "", ErrUploadLockHeld
	}
	if err := s.tel.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", ErrUploadLockHeld
	}
	token, lost, err := s.placeLock()
	switch {
	case lost:
		return "", ErrUploadLockHeld
	case err != nil:
		return "", err
	}
	return token, nil
}

// lockReadFailure is the result of a take whose lock could not be read. A
// lock that is gone again, one that was replaced between the read and this
// check, and a regular file another process has open on Windows are held;
// anything else is a lock nobody can judge.
func (s *Store) lockReadFailure(name string, readErr error) error {
	info, err := s.tel.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrUploadLockHeld
	case err == nil && info.Mode().IsRegular() && (isBusy(readErr) || errors.Is(readErr, fs.ErrNotExist)):
		return ErrUploadLockHeld
	}
	return fmt.Errorf("%w: %w", ErrUploadLockUnreadable, readErr)
}

// readLockContent reads a lock or a lock temporary file. A file over
// maxLockSize reads as a fixed text that does not parse as a lock.
func (s *Store) readLockContent(name string) (string, error) {
	data, big, err := s.readLimited(name, maxLockSize)
	switch {
	case err != nil:
		return "", err
	case big:
		return oversizedContent, nil
	}
	return string(data), nil
}

// parseLockContent splits the content of a lock into the time written in it
// and its token. ok is false when it is not a time and a token.
func parseLockContent(content string) (written time.Time, token string, ok bool) {
	fields := strings.Fields(content)
	if len(fields) != 2 {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return time.Time{}, "", false
	}
	return t, fields[1], true
}

// lockStale reports whether the lock content is stale at now: its time is
// idStaleAfter or more before now, more than idStaleAfter after it, or the
// content does not parse.
func lockStale(content string, now time.Time) bool {
	written, _, ok := parseLockContent(content)
	if !ok {
		return true
	}
	age := now.Sub(written)
	return age >= idStaleAfter || age < -idStaleAfter
}

// removeOldLockTemps removes temporary lock files a crashed taker left: those
// whose written time parses and is idStaleAfter or more in the past. One that
// does not parse may be another taker's write in progress and stays.
func (s *Store) removeOldLockTemps() {
	entries, err := s.tel.ReadDir(dirUpload)
	if err != nil {
		return
	}
	now := nowFunc().UTC()
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), lockTempPrefix) {
			continue
		}
		name := path.Join(dirUpload, e.Name())
		content, err := s.readLockContent(name)
		if err != nil {
			continue
		}
		if written, _, ok := parseLockContent(content); ok && now.Sub(written) >= idStaleAfter {
			_ = s.tel.Remove(name)
		}
	}
}

// Release removes the lock when it still holds this holder's token. A lock
// that is gone, or that another holder replaced, stays as it is and is no
// error. A removal that fails while another process has the file open is tried
// once more; if it still fails the error is returned, and the lock goes stale
// on its own. A holder that runs past the stale age can lose its lock to a
// new holder between the check of the token and the removal, and then removes
// the new holder's lock; the time bound on a holder keeps that rare. Release
// creates nothing.
func (l *UploadLock) Release() error {
	name := path.Join(dirUpload, fileLock)
	data, big, err := l.s.readLimited(name, maxLockSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case big:
		return nil
	}
	if _, token, ok := parseLockContent(string(data)); !ok || token != l.token {
		return nil
	}
	err = l.s.tel.Remove(name)
	if err != nil && isBusy(err) {
		time.Sleep(retryDelay)
		err = l.s.tel.Remove(name)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// UploadState is what the upload stage keeps between runs, in
// telemetry/upload/state.json. Every key is optional.
type UploadState struct {
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	// HTTPStatus is the status of the last answer, 0 when there was none.
	HTTPStatus int `json:"httpStatus"`
	// NoAnswer says why there was no answer: empty, "timeout", "network
	// error", "refused redirect" or "lock unavailable" (the attempt could not
	// take the upload lock and sent nothing).
	NoAnswer string `json:"noAnswer"`
	Sent     int    `json:"sent"`
	// Refused is the number of events the endpoint refused; nil when not
	// known.
	Refused     *int      `json:"refused"`
	Dropped     int       `json:"dropped"`
	NextAttempt time.Time `json:"nextAttempt,omitzero"`
	// Failures counts consecutive attempts that kept files.
	Failures int `json:"failures"`
	// CoveredIDs are the record ids an answer covered whose files are still
	// stored: a file split across requests whose last part was not sent, a file
	// whose removal is waiting, and a deletion skipped for time. The next
	// attempt leaves them out of its requests and counts them as sent when it
	// decides which files are covered.
	CoveredIDs []string `json:"coveredIds,omitempty"`
}

// MaxAttemptWait is the longest wait the upload writes between two attempts.
const MaxAttemptWait = 24 * time.Hour

// nextAttemptSlack is how far past MaxAttemptWait a stored next attempt may
// lie before it counts as damaged, or as written before a clock was set back.
const nextAttemptSlack = 10 * time.Minute

// AttemptDue reports whether the state allows an upload attempt at now: no
// next attempt is stored, it is not after now, or it lies further ahead than
// any wait the upload writes.
func (st UploadState) AttemptDue(now time.Time) bool {
	if st.NextAttempt.IsZero() || !st.NextAttempt.After(now) {
		return true
	}
	return st.NextAttempt.After(now.Add(MaxAttemptWait + nextAttemptSlack))
}

// ReadUploadState reads the state file. A missing file, one over 64 KiB, a
// directory, content that does not parse and no telemetry directory read as
// the zero value and false. Keys it does not know are ignored.
func (s *Store) ReadUploadState() (UploadState, bool) {
	if s.tel == nil {
		return UploadState{}, false
	}
	data, big, err := s.readLimited(path.Join(dirUpload, fileState), maxStateSize)
	if err != nil || big {
		return UploadState{}, false
	}
	data = bytes.TrimSpace(data)
	var st UploadState
	if len(data) == 0 || data[0] != '{' || json.Unmarshal(data, &st) != nil {
		return UploadState{}, false
	}
	return st, true
}

// WriteState replaces the state file through a temporary file and a rename,
// so a reader sees the old file or the new one; a rename that fails while
// another process has the file open is tried once more. It never creates
// telemetry/upload: when that directory is gone it returns the error and
// leaves nothing behind. A holder whose lock another process took over can
// share the temporary name with the new holder; the worst result is a state
// file that does not parse, which reads as the zero value.
func (l *UploadLock) WriteState(st UploadState) error {
	return l.s.writeState(st)
}

// WriteStateWithoutLock replaces the state file like WriteState, with no lock
// held. It is only for an attempt whose TakeUploadLock failed with
// ErrUploadLockUnreadable or a link error: no process can hold the lock then,
// and two writers race only on an advisory file. It never creates
// telemetry/upload.
func (s *Store) WriteStateWithoutLock(st UploadState) error {
	return s.writeState(st)
}

// writeState is the write both WriteState and WriteStateWithoutLock make.
func (s *Store) writeState(st UploadState) error {
	if s.tel == nil {
		return ErrNoTelemetryDirectory
	}
	st.LastAttempt = st.LastAttempt.UTC()
	st.NextAttempt = st.NextAttempt.UTC()
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path.Join(dirUpload, stateTempName)
	final := path.Join(dirUpload, fileState)
	if err := s.tel.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := s.tel.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = s.tel.Remove(tmp)
		return err
	}
	err = s.tel.Rename(tmp, final)
	if err != nil && isBusy(err) {
		time.Sleep(retryDelay)
		err = s.tel.Rename(tmp, final)
	}
	if err != nil {
		_ = s.tel.Remove(tmp)
		return err
	}
	return nil
}

// UploadLockStatus reports the state of the upload lock without taking or
// removing anything, by the rules TakeUploadLock applies. dirExists is false
// when there is no telemetry directory or no telemetry/upload. err is nil when
// there is no lock or the lock is stale, ErrUploadLockHeld when another
// process holds it, ErrUploadLockUnreadable when it cannot be read and
// ErrUnsafeUploadDir when telemetry/upload is a symlink, not a directory, or a
// path the process cannot look at.
func (s *Store) UploadLockStatus() (dirExists bool, err error) {
	if s.tel == nil {
		return false, nil
	}
	info, err := s.tel.Lstat(dirUpload)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return true, fmt.Errorf("%w: %w", ErrUnsafeUploadDir, err)
	case info.Mode()&fs.ModeSymlink != 0 || !info.IsDir():
		return true, ErrUnsafeUploadDir
	}
	lock := path.Join(dirUpload, fileLock)
	if _, err := s.tel.Lstat(lock); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	content, err := s.readLockContent(lock)
	if err != nil {
		return true, s.lockReadFailure(lock, err)
	}
	if lockStale(content, nowFunc().UTC()) {
		return true, nil
	}
	return true, ErrUploadLockHeld
}

// DeleteUploadState removes telemetry/upload with the lock, the state file and
// any temporary file in it. A removal that fails while another process has a
// file open is tried once more. With no telemetry directory or no upload
// directory it does nothing.
func (s *Store) DeleteUploadState() error {
	if s.tel == nil {
		return nil
	}
	err := s.tel.RemoveAll(dirUpload)
	if err != nil && isBusy(err) {
		time.Sleep(retryDelay)
		err = s.tel.RemoveAll(dirUpload)
	}
	return err
}

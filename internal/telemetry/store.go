package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Paths below the telemetry directory, slash-separated as os.Root wants
// them. dirTelemetry is the one path relative to the configuration
// directory.
const (
	dirTelemetry  = "telemetry"
	dirEvents     = "events"
	dirServers    = "servers"
	fileID        = "config-directory-id"
	fileMarker    = "pruned"
	hourLayout    = "2006010215"
	droppedPrefix = "dropped-"
	slotSuffix    = ".json"
	jsonlSuffix   = ".jsonl"
)

var errUnsafeDir = errors.New("telemetry path is not a plain directory")

// Status is what Record did.
type Status int

// The statuses. A failed or dropped recording is never an error to the
// caller: an event is not a command result.
const (
	// StatusOff means a control turned recording off; no file was touched.
	StatusOff Status = iota
	// StatusNoDirectory means the configuration directory did not exist
	// when the run started; nothing is created.
	StatusNoDirectory
	// StatusRecorded means the event was written.
	StatusRecorded
	// StatusDropped means every slot tried was taken.
	StatusDropped
	// StatusFailed means the event was dropped because a file operation
	// failed, the random source failed, or recording panicked.
	StatusFailed
)

// Options is what Record needs beyond the event.
type Options struct {
	// ConfigDir is the configuration directory. The store never creates it.
	ConfigDir string
	// DirExisted is true when the caller saw the directory exist at the
	// start of the run; false records nothing.
	DirExisted bool
	// Getenv reads the control variables; nil reads none.
	Getenv func(string) string
	// Config is the telemetry key of the loaded config.yaml.
	Config ConfigSetting
}

// Record writes one event when the controls allow it and the configuration
// directory already existed. It resolves the controls first and returns
// before any file operation when recording is off. A write that cannot
// finish, a failed random source and a panic anywhere below it drop the
// event without any error, and nothing is written to standard output or
// standard error.
func Record(ev Event, opts Options) (st Status) {
	if !Resolve(ControlInput{Getenv: opts.Getenv, Config: opts.Config}).On {
		return StatusOff
	}
	if !opts.DirExisted {
		return StatusNoDirectory
	}
	if ev.failed() {
		return StatusFailed
	}
	defer func() {
		if recover() != nil {
			st = StatusFailed
		}
	}()
	s, err := OpenStore(opts.ConfigDir)
	if err != nil {
		return StatusFailed
	}
	defer func() { _ = s.Close() }()
	return s.record(ev)
}

// Store is the telemetry directory of one configuration directory. It holds
// the configuration directory's root only to look at and create the
// telemetry directory; every other path is relative to a root opened on the
// telemetry directory; no path can leave it.
type Store struct {
	cfg       *root
	tel       *root
	configDir string
	deadline  time.Time
}

// OpenStore opens the configuration directory and, when it holds a
// telemetry directory, that directory. It never creates either, and it
// refuses a telemetry path that is a symlink or not a directory.
func OpenStore(configDir string) (*Store, error) {
	cfg, err := openRoot(configDir)
	if err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, configDir: configDir}
	info, err := cfg.Lstat(dirTelemetry)
	switch {
	case err == nil:
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			_ = cfg.Close()
			return nil, errUnsafeDir
		}
		tel, err := cfg.OpenRoot(dirTelemetry)
		if err != nil {
			_ = cfg.Close()
			return nil, err
		}
		s.tel = tel
	case errors.Is(err, fs.ErrNotExist):
	default:
		_ = cfg.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the directories.
func (s *Store) Close() error {
	var err error
	if s.tel != nil {
		err = s.tel.Close()
	}
	return errors.Join(err, s.cfg.Close())
}

// ensureDir creates the telemetry directory when it is missing, with mode
// 0700 and the inheritable owner-only access list set once on the new
// directory, and opens it. A process that dies between the create and the
// access list call leaves the directory without the list.
func (s *Store) ensureDir() error {
	if s.tel != nil {
		return nil
	}
	created := true
	if err := s.cfg.Mkdir(dirTelemetry, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		created = false
	}
	if created {
		if err := hardenDir(filepath.Join(s.configDir, dirTelemetry)); err != nil {
			_ = s.cfg.Remove(dirTelemetry)
			return err
		}
	}
	tel, err := s.cfg.OpenRoot(dirTelemetry)
	if err != nil {
		return err
	}
	s.tel = tel
	return nil
}

// readFile is a read of a whole file, retried once while another handle
// holds it open.
func (s *Store) readFile(name string) ([]byte, error) {
	data, err := s.tel.ReadFile(name)
	if err != nil && isBusy(err) {
		time.Sleep(retryDelay)
		data, err = s.tel.ReadFile(name)
	}
	return data, err
}

// readLimited reads at most limit bytes of a file, retried once while
// another handle holds it open.
func (s *Store) readLimited(name string, limit int64) ([]byte, bool, error) {
	data, big, err := s.tel.ReadFileMax(name, limit)
	if err != nil && isBusy(err) {
		time.Sleep(retryDelay)
		data, big, err = s.tel.ReadFileMax(name, limit)
	}
	return data, big, err
}

func hourName(t time.Time) string { return t.UTC().Format(hourLayout) }

func (s *Store) record(ev Event) Status {
	if err := s.ensureDir(); err != nil {
		return StatusFailed
	}
	now := nowFunc().UTC()
	id := s.directoryID(now)
	due := s.pruneDue(now)

	var facts Facts
	url, _ := Server()
	if url != "" {
		if data, err := s.readFile(path.Join(dirServers, FactsFileName(url))); err == nil {
			facts, _ = parseFacts(data)
		}
	}
	ev = ev.withFacts(url != "", facts, now).withDirectoryID(id)

	st := s.writeSlot(ev)
	if due {
		s.touchMarker(now)
		s.pass(now, ev.producerInfo(), id)
	}
	return st
}

// writeSlot creates a random free slot of the event's hour directory
// exclusively and writes the event in one write. After SlotTries taken
// slots the event is dropped and a zero-byte marker is left for the
// hourly pass to count.
func (s *Store) writeSlot(ev Event) Status {
	dir := path.Join(dirEvents, hourName(ev.Hour()))
	data := ev.stored()
	madeDir := false
	taken := 0
	for taken < slotTries {
		name := path.Join(dir, fmt.Sprintf("%04d%s", randIntn(slotCount), slotSuffix))
		f, err := s.tel.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		switch {
		case err == nil:
			pause("created")
			werr := writeAll(f, name, data)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = s.tel.Remove(name)
				return StatusFailed
			}
			return StatusRecorded
		case errors.Is(err, fs.ErrExist):
			taken++
		case errors.Is(err, fs.ErrNotExist) && !madeDir:
			madeDir = true
			if s.tel.MkdirAll(dir, 0o700) != nil {
				return StatusFailed
			}
		default:
			return StatusFailed
		}
	}
	marker := path.Join(dir, droppedPrefix+randomHex(8))
	if f, err := s.tel.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_ = f.Close()
	}
	return StatusDropped
}

func writeAll(f *file, name string, data []byte) error {
	if writeHook != nil {
		if err := writeHook(name, len(data)); err != nil {
			return err
		}
	}
	_, err := f.Write(data)
	return err
}

func randomHex(n int) string {
	var sb strings.Builder
	const digits = "0123456789abcdef"
	for range n {
		sb.WriteByte(digits[randIntn(len(digits))])
	}
	return sb.String()
}

// directoryID returns the configuration directory's id, creating it on
// the first event. The id file is created exclusively and written in one
// write; a process that loses the race reads the winner's id. An empty,
// partial or not version 4 file younger than a minute leaves the id absent
// from this event; an older one is removed and created again exclusively,
// and the loser of that race reads the winner's id. Two processes that
// replace a stale file at the same moment can give one event an id the
// directory does not keep; afterwards exactly one id survives.
func (s *Store) directoryID(now time.Time) string {
	data, err := s.readFile(fileID)
	switch {
	case err == nil:
		if id, ok := NormalizeRandomID(strings.TrimSpace(string(data))); ok {
			return id
		}
		return s.replaceStaleID(now)
	case errors.Is(err, fs.ErrNotExist):
		return s.createID()
	}
	return ""
}

func (s *Store) replaceStaleID(now time.Time) string {
	info, err := s.tel.Stat(fileID)
	if err != nil || now.Sub(info.ModTime()) < idStaleAfter {
		return ""
	}
	pause("stale-id")
	data, err := s.readFile(fileID)
	if err != nil {
		return ""
	}
	if id, ok := NormalizeRandomID(strings.TrimSpace(string(data))); ok {
		return id
	}
	if err := s.tel.Remove(fileID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return s.createID()
}

// createID creates the id file exclusively. The id is made first; a
// failed random source leaves no file.
func (s *Store) createID() string {
	id, err := newUUID()
	if err != nil {
		return ""
	}
	pause("create-id")
	f, err := s.tel.OpenFile(fileID, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			if data, err := s.readFile(fileID); err == nil {
				if won, ok := NormalizeRandomID(strings.TrimSpace(string(data))); ok {
					return won
				}
			}
		}
		return ""
	}
	_, werr := f.Write([]byte(id))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return ""
	}
	return id
}

// pruneDue reports whether the hourly pass is due: the marker file is
// missing or was last refreshed an hour ago or more.
func (s *Store) pruneDue(now time.Time) bool {
	info, err := s.tel.Stat(fileMarker)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return now.Sub(info.ModTime()) >= pruneInterval
}

// touchMarker creates the marker when it is missing and sets its modification
// time to now, before the pass runs.
func (s *Store) touchMarker(now time.Time) {
	f, err := s.tel.OpenFile(fileMarker, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_ = f.Close()
	_ = s.tel.Chtimes(fileMarker, now, now)
}

// WriteFacts writes the facts file of a server through a temporary name and
// a rename; a reader sees the old file or the new one. When two
// processes refresh at once the last rename wins. A process that dies
// between the create and the rename leaves a temporary file, which the
// hourly pass removes once it is an hour old.
func (s *Store) WriteFacts(canonicalURL string, f Facts) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	if err := s.tel.MkdirAll(dirServers, 0o700); err != nil {
		return err
	}
	data, err := marshalFacts(f)
	if err != nil {
		return err
	}
	final := path.Join(dirServers, FactsFileName(canonicalURL))
	tmp := path.Join(dirServers, factsTempPrefix+path.Base(final)+"-"+randomHex(8))
	file, err := s.tel.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := file.Write(data)
	cerr := file.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = s.tel.Remove(tmp)
		return err
	}
	if err := s.tel.Rename(tmp, final); err != nil {
		if isBusy(err) {
			time.Sleep(retryDelay)
			err = s.tel.Rename(tmp, final)
		}
		if err != nil {
			_ = s.tel.Remove(tmp)
			return err
		}
	}
	return nil
}

// factsTempPrefix starts the name of a facts temporary file.
const factsTempPrefix = "."

// ReadFacts reads the facts file of a server. A missing or unreadable
// file, or no telemetry directory, is an empty Facts and false.
func (s *Store) ReadFacts(canonicalURL string) (Facts, bool) {
	if s.tel == nil {
		return Facts{}, false
	}
	data, err := s.readFile(path.Join(dirServers, FactsFileName(canonicalURL)))
	if err != nil {
		return Facts{}, false
	}
	return parseFacts(data)
}

// DeleteEvents removes every recorded event and drop marker.
func (s *Store) DeleteEvents() error {
	if s.tel == nil {
		return nil
	}
	return s.tel.RemoveAll(dirEvents)
}

// DeleteFacts removes every server facts file.
func (s *Store) DeleteFacts() error {
	if s.tel == nil {
		return nil
	}
	return s.tel.RemoveAll(dirServers)
}

// DeleteDirectoryID removes the configuration directory id.
func (s *Store) DeleteDirectoryID() error {
	if s.tel == nil {
		return nil
	}
	if err := s.tel.Remove(fileID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Reset removes the events, the server facts and the configuration
// directory id, and nothing else; it keeps trying after a failure and
// returns every failure. With no telemetry directory it does nothing.
func (s *Store) Reset() error {
	return errors.Join(s.DeleteEvents(), s.DeleteFacts(), s.DeleteDirectoryID())
}

// StoredEvent is one event read back from disk.
type StoredEvent struct {
	Name string
	UID  string
	// Time is the event time; zero when the stored text did not parse.
	Time time.Time
	// Fields holds every key of the stored event except local_format.
	Fields map[string]any
}

// parseEvent checks the structural minimum of a stored event and nothing
// more: a JSON object of at most maxEventSize bytes with a string event.name,
// a string time, a string schema_version and a version 4 log.record.uid.
// Keys and values it does not know, written by a newer CLI that shares the
// directory, are kept.
func parseEvent(data []byte) (StoredEvent, bool) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || len(data) > maxEventSize {
		return StoredEvent{}, false
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return StoredEvent{}, false
	}
	name, _ := fields[keyEventName].(string)
	ts, _ := fields[keyTime].(string)
	version, _ := fields[keySchemaVersion].(string)
	uidText, _ := fields[attrRecordUID].(string)
	uid, ok := NormalizeRandomID(uidText)
	if name == "" || ts == "" || version == "" || !ok {
		return StoredEvent{}, false
	}
	delete(fields, keyLocalFormat)
	ev := StoredEvent{Name: name, UID: uid, Fields: fields}
	ev.Time, _ = time.Parse(time.RFC3339, ts)
	return ev, true
}

type hourDir struct {
	name string
	t    time.Time
}

func parseHour(name string) (time.Time, bool) {
	if len(name) != len(hourLayout) {
		return time.Time{}, false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return time.Time{}, false
		}
	}
	t, err := time.Parse(hourLayout, name)
	return t, err == nil
}

// hours lists the hour directories, oldest first.
func (s *Store) hours() ([]hourDir, error) {
	if s.tel == nil {
		return nil, nil
	}
	entries, err := s.tel.ReadDir(dirEvents)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []hourDir
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if t, ok := parseHour(e.Name()); ok {
			out = append(out, hourDir{e.Name(), t})
		}
	}
	slices.SortFunc(out, func(a, b hourDir) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// hourFiles is the content of one hour directory by kind.
type hourFiles struct {
	slots, markers, jsonls []string
}

func isSlotName(n string) bool {
	if len(n) != 4+len(slotSuffix) || !strings.HasSuffix(n, slotSuffix) {
		return false
	}
	for _, c := range n[:4] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *Store) listHour(h hourDir) (hourFiles, error) {
	entries, err := s.tel.ReadDir(path.Join(dirEvents, h.name))
	if err != nil {
		return hourFiles{}, err
	}
	var files hourFiles
	for _, e := range entries {
		n := e.Name()
		switch {
		case e.IsDir():
		case isSlotName(n):
			files.slots = append(files.slots, n)
		case strings.HasPrefix(n, droppedPrefix):
			files.markers = append(files.markers, n)
		case strings.HasPrefix(n, h.name) && strings.HasSuffix(n, jsonlSuffix):
			files.jsonls = append(files.jsonls, n)
		}
	}
	return files, nil
}

// readLines returns the parsed events of a compacted file, skipping any
// line that does not parse (a file cut short by a crash ends in one). The
// bool is false when the file could not be read whole.
func (s *Store) readLines(name string) ([]StoredEvent, bool) {
	data, big, err := s.readLimited(name, maxHourFileSize)
	if err != nil || big {
		return nil, false
	}
	var out []StoredEvent
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if ev, ok := parseEvent(line); ok {
			out = append(out, ev)
		}
	}
	return out, true
}

// walk calls visit with each stored event, oldest hour first, compacted files
// before slots, skipping unparsable slots and lines and removing duplicates
// by record id. It stops when visit returns false. With no telemetry
// directory it visits nothing.
func (s *Store) walk(visit func(StoredEvent) bool) error {
	hours, err := s.hours()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	emit := func(ev StoredEvent) bool {
		if seen[ev.UID] {
			return true
		}
		seen[ev.UID] = true
		return visit(ev)
	}
	for _, h := range hours {
		files, err := s.listHour(h)
		if err != nil {
			continue
		}
		dir := path.Join(dirEvents, h.name)
		for _, j := range files.jsonls {
			lines, _ := s.readLines(path.Join(dir, j))
			for _, ev := range lines {
				if !emit(ev) {
					return nil
				}
			}
		}
		for _, n := range files.slots {
			data, big, err := s.readLimited(path.Join(dir, n), maxEventSize)
			if err != nil || big {
				continue
			}
			if ev, ok := parseEvent(data); ok && !emit(ev) {
				return nil
			}
		}
	}
	return nil
}

// List returns every stored event, oldest first, skipping what does not
// parse and removing duplicates by record id.
func (s *Store) List() ([]StoredEvent, error) {
	var out []StoredEvent
	err := s.walk(func(ev StoredEvent) bool {
		out = append(out, ev)
		return true
	})
	return out, err
}

// Count returns the number of stored events, up to limit.
func (s *Store) Count(limit int) (int, error) {
	n := 0
	err := s.walk(func(StoredEvent) bool {
		n++
		return n < limit
	})
	return n, err
}

// DroppedCount returns how many events were lost: the counts of every
// dropped event, plus the markers no pass has counted yet.
func (s *Store) DroppedCount() (int, error) {
	total := 0
	err := s.walk(func(ev StoredEvent) bool {
		if ev.Name == EventEventsDropped {
			if n, ok := ev.Fields[attrDroppedCount].(float64); ok && n > 0 {
				total += int(n)
			}
		}
		return true
	})
	if err != nil {
		return total, err
	}
	hours, err := s.hours()
	if err != nil {
		return total, err
	}
	for _, h := range hours {
		if files, err := s.listHour(h); err == nil {
			total += len(files.markers)
		}
	}
	return total, nil
}

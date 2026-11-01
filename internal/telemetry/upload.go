package telemetry

import (
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// ReadyOptions says which stored events a ReadyList returns.
type ReadyOptions struct {
	// CurrentHour lists the events of the current hour too. Without it only
	// finished hours are listed.
	CurrentHour bool
	// Written are the compacted files the hourly pass of this run just wrote
	// (the third result of Record). A listed file younger than a minute is
	// skipped unless its path is here.
	Written []string
}

// ReadyList is the stored events an upload may send, and what is needed to
// tell which stored files a set of sent events covers.
type ReadyList struct {
	// Events are the events in the order of List: oldest hour first,
	// compacted files before slots, each record id once.
	Events []StoredEvent
	files  []listedFile
	// dir is the configuration directory the list was read from.
	dir string
}

// listedFile is one stored file that holds listed events.
type listedFile struct {
	path string
	// ids are the record ids of every parsed event in the file, including
	// those the list left out because an earlier file holds them too.
	ids []string
	// slotID is the record id read from a slot; empty for a compacted file.
	slotID string
	// whole is false for a compacted file that has a line which does not
	// parse other than an unterminated last line.
	whole bool
}

// Coverage is the stored files a set of sent events covers. Only a ReadyList
// makes one, so a deletion cannot name a file the list did not hold. It
// remembers the configuration directory of the list, and a Store on another
// directory refuses it.
type Coverage struct {
	files []coveredFile
	dir   string
}

type coveredFile struct {
	path string
	// id is the record id the slot held when it was listed.
	id string
}

// Ready lists the stored events of finished hours that an upload may send. An
// hour more than MaxUploadAge old is left out, and so is a compacted file
// younger than a minute unless ReadyOptions.Written names it: another process
// may still be writing it. It reads files only: it creates and removes
// nothing, and with no telemetry directory or no events directory the list is
// empty.
func (s *Store) Ready(opts ReadyOptions) (*ReadyList, error) {
	now := nowFunc().UTC()
	current := now.Truncate(time.Hour)
	oldest := now.Add(-MaxUploadAge)
	o := walkOptions{
		skipHour: func(h hourDir) bool {
			if h.t.After(current) || (h.t.Equal(current) && !opts.CurrentHour) {
				return true
			}
			return h.t.Before(oldest)
		},
		skipFile: func(name string) bool {
			if slices.Contains(opts.Written, name) {
				return false
			}
			info, err := s.tel.Stat(name)
			return err != nil || now.Sub(info.ModTime()) < youngFileAge
		},
	}
	list := &ReadyList{dir: s.configDir}
	seen := map[string]bool{}
	err := s.walkFiles(o, func(f fileRead) bool {
		lf := listedFile{path: f.name, whole: f.clean}
		for _, ev := range f.events {
			lf.ids = append(lf.ids, ev.UID)
			if f.slot {
				lf.slotID = ev.UID
			}
			if !seen[ev.UID] {
				seen[ev.UID] = true
				list.Events = append(list.Events, ev)
			}
		}
		list.files = append(list.files, lf)
		return true
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// Cover returns the files of the list whose every event is in sent, a set of
// record ids. A compacted file with a line that does not parse, other than an
// unterminated last line, is never covered.
func (r *ReadyList) Cover(sent map[string]bool) Coverage {
	c := Coverage{dir: r.dir}
	for _, f := range r.files {
		if !f.whole || slices.ContainsFunc(f.ids, func(id string) bool { return !sent[id] }) {
			continue
		}
		c.files = append(c.files, coveredFile{path: f.path, id: f.slotID})
	}
	return c
}

// DeleteResult says what DeleteCovered did, by path below the telemetry
// directory.
type DeleteResult struct {
	// Deleted are the files removed, and those already gone.
	Deleted []string
	// Waiting are the files left for a later attempt: one that could not be
	// removed (on Windows, open in another process), and a slot that now
	// holds another event.
	Waiting []string
	// Refused are the paths that are not an event file the store may remove.
	Refused []string
}

// DeleteCovered removes the files of c. For each it checks the path again,
// and that it is a regular file; a slot is read again and removed only when it
// still holds the event that was listed. A file that is already gone counts
// as deleted. A file that cannot be removed is reported as waiting; the call
// has no error for it. It never removes a directory or a drop marker. A
// coverage made from a list of another configuration directory is refused:
// every path is reported as refused and nothing is removed. The directory is
// compared as the text the Store was opened with, so another spelling of the
// same directory is refused too.
//
// Two short windows remain. A slot is read again and then removed: in
// between, the hourly pass of another process can compact and remove it, and
// a writer that was paused across the hour boundary can create a slot with
// the same name, whose event is then removed unsent. A compacted file is
// checked by its name and not by the file itself: after another process took
// over the upload lock, a second sender can remove a file and the hourly pass
// can write a new file of the same name before the first sender's removal.
// Each needs several processes to act within microseconds and costs at most
// one unsent slot or file.
func (s *Store) DeleteCovered(c Coverage) DeleteResult {
	var res DeleteResult
	if s.tel == nil {
		return res
	}
	if c.dir != s.configDir {
		for _, f := range c.files {
			res.Refused = append(res.Refused, f.path)
		}
		return res
	}
	for _, f := range c.files {
		slot, ok := eventFilePath(f.path)
		if !ok {
			res.Refused = append(res.Refused, f.path)
			continue
		}
		info, err := s.tel.Lstat(f.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.Deleted = append(res.Deleted, f.path)
			continue
		case err != nil:
			res.Waiting = append(res.Waiting, f.path)
			continue
		case !info.Mode().IsRegular():
			res.Refused = append(res.Refused, f.path)
			continue
		}
		if slot {
			switch holds, err := s.slotHolds(f.path, f.id); {
			case errors.Is(err, fs.ErrNotExist):
				res.Deleted = append(res.Deleted, f.path)
				continue
			case err != nil || !holds:
				res.Waiting = append(res.Waiting, f.path)
				continue
			}
		}
		switch err := s.tel.Remove(f.path); {
		case err == nil, errors.Is(err, fs.ErrNotExist):
			res.Deleted = append(res.Deleted, f.path)
		default:
			res.Waiting = append(res.Waiting, f.path)
		}
	}
	return res
}

// slotHolds reads the slot again and reports whether it parses to the event
// with the record id. The error is the read's: a not-exist when the slot is
// gone.
func (s *Store) slotHolds(name, id string) (bool, error) {
	data, big, err := s.readLimited(name, maxEventSize)
	if err != nil || big {
		return false, err
	}
	ev, ok := parseEvent(data)
	return ok && ev.UID == id, nil
}

// eventFilePath reports whether p names a file DeleteCovered may remove: a
// clean slash path of three parts, events, an hour and then a slot name, the
// hour's file <hour>.jsonl or one of its later files <hour>-<n>.jsonl. A drop
// marker, a directory name and any other path is not one. slot is true for a
// slot name.
func eventFilePath(p string) (slot, ok bool) {
	if path.Clean(p) != p || strings.ContainsAny(p, `\:`) || strings.HasSuffix(p, ".") {
		return false, false
	}
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[0] != dirEvents {
		return false, false
	}
	hour, name := parts[1], parts[2]
	if _, ok := parseHour(hour); !ok {
		return false, false
	}
	if isSlotName(name) {
		return true, true
	}
	rest, found := strings.CutPrefix(name, hour)
	if !found {
		return false, false
	}
	if rest == jsonlSuffix {
		return false, true
	}
	n, found := strings.CutPrefix(rest, "-")
	if !found {
		return false, false
	}
	n, found = strings.CutSuffix(n, jsonlSuffix)
	if !found || n == "" || n[0] == '0' || strings.TrimLeft(n, "0123456789") != "" {
		return false, false
	}
	return false, true
}

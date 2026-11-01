package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

type hourResult int

const (
	// hourSkipped means the hour had nothing the pass could do; the next
	// hour is tried.
	hourSkipped hourResult = iota
	// hourDone means the hour made progress; the pass compacts no more.
	hourDone
	// hourStopped means another process is writing a file of the hour, a
	// write failed or the pass ran out of time; the pass compacts no more.
	hourStopped
)

// expired reports whether the read phase has used its time. The clock is
// read each time; a zero deadline never expires. Only the read phase of a
// pass checks it, after each slot read; writing a file and removing what it
// holds is bounded by count (at most N slots and maxMarkersPerFile markers),
// not by the clock.
func (s *Store) expired() bool {
	return !s.deadline.IsZero() && nowFunc().After(s.deadline)
}

// pass is the hourly pass. It compacts at most one finished hour, removes
// at most maxHoursPerPass compacted hours older than AgeCap, oldest first,
// at most maxFactsPerPass facts files older than FactsMaxAge and at most
// maxFactsPerPass facts temporary files older than an hour. It never
// touches the current hour and tolerates directories that disappear under
// it. Reading slots stops after readLimit and the pass writes what it read;
// what is left waits for the next hourly pass. The caller refreshed the
// marker first.
func (s *Store) pass(now time.Time, p producer, dirID string) {
	s.deadline = nowFunc().Add(readLimit)
	hours, err := s.hours()
	if err != nil {
		return
	}
	current := now.UTC().Truncate(time.Hour)
	for _, h := range hours {
		if !h.t.Before(current) {
			break
		}
		if s.compactHour(h, now, p, dirID) != hourSkipped {
			break
		}
	}
	removed := 0
	for _, h := range hours {
		if removed >= maxHoursPerPass || !h.t.Before(current.Add(-AgeCap)) || s.expired() {
			break
		}
		files, err := s.listHour(h)
		if err != nil || len(files.slots) > 0 || len(files.markers) > 0 {
			continue
		}
		if s.tel.RemoveAll(path.Join(dirEvents, h.name)) == nil {
			removed++
		}
	}
	if !s.expired() {
		s.pruneFacts(now)
	}
}

// compactHour moves the events of one finished hour into one new file,
// written in one write, synced and closed before any slot is removed. The
// file is the hour's file or, when the hour already has one, the next free
// <hour>-<n>.jsonl; it is created exclusively, and a process that finds the
// name taken stops. A file of the hour under a minute old means another
// process is writing it, and the pass stops. The pass has two phases. The
// read phase lists the hour, reads the earlier files and then the slots; it
// stops after readLimit, checked after each slot read and never before the
// first; every pass reads at least one slot. The write phase writes one
// file, syncs and closes it, and then removes what the file holds; it does
// not check the clock. A write that follows a read phase that stopped early
// (a partial write) holds the slots read so far, carries no dropped event and
// removes no marker; the rest of the hour waits for the next pass. A write
// of a fully read hour carries at most one dropped event, counting up to
// maxMarkersPerFile drop markers, and removes exactly those markers first and
// then the slots; a stop after the markers leaves only slots the next pass
// removes as duplicates. A crash between a write and the removals can count
// the same drops again in the next file. An empty or partial slot, or one over maxEventSize, is
// removed only once now is more than an hour past the end of its hour; a
// writer paused at the boundary keeps its slot. A slot that cannot be read is
// kept. A slot whose record id is already in a file of the hour is removed
// as a duplicate.
func (s *Store) compactHour(h hourDir, now time.Time, p producer, dirID string) hourResult {
	dir := path.Join(dirEvents, h.name)
	files, err := s.listHour(h)
	if err != nil || (len(files.slots) == 0 && len(files.markers) == 0) {
		return hourSkipped
	}
	for _, j := range files.jsonls {
		info, err := s.tel.Stat(path.Join(dir, j))
		if err != nil || now.Sub(info.ModTime()) < youngFileAge {
			return hourStopped
		}
	}
	pause("listed")

	known := map[string]bool{}
	for _, j := range files.jsonls {
		events, ok := s.readLines(path.Join(dir, j))
		if !ok {
			return hourSkipped
		}
		for _, ev := range events {
			known[ev.UID] = true
		}
	}

	var lines [][]byte
	var remove []string
	seen := map[string]bool{}
	partial := false
	settled := !now.Before(h.t.Add(2 * time.Hour))
	for i, n := range files.slots {
		data, big, err := s.readLimited(path.Join(dir, n), maxEventSize)
		if err == nil {
			var ev StoredEvent
			ok := false
			if !big {
				ev, ok = parseEvent(data)
			}
			switch {
			case !ok:
				if settled {
					remove = append(remove, n)
				}
			case known[ev.UID] || seen[ev.UID]:
				remove = append(remove, n)
			default:
				var line bytes.Buffer
				if json.Compact(&line, data) == nil {
					seen[ev.UID] = true
					lines = append(lines, line.Bytes())
					remove = append(remove, n)
				}
			}
		}
		if s.expired() && i < len(files.slots)-1 {
			partial = true
			break
		}
	}
	if !partial && len(files.markers) > 0 {
		counted := files.markers[:min(len(files.markers), markerCap)]
		d := newEventsDropped(h.t, len(counted), p)
		if !d.failed() {
			lines = append(lines, d.withDirectoryID(dirID).stored())
			remove = append(slices.Clone(counted), remove...)
		}
	}
	if partial && len(lines) == 0 && len(remove) == 0 {
		return hourStopped
	}

	if len(lines) > 0 {
		target := h.name + jsonlSuffix
		if len(files.jsonls) > 0 {
			n := 1
			for slices.Contains(files.jsonls, fmt.Sprintf("%s-%d%s", h.name, n, jsonlSuffix)) {
				n++
			}
			target = fmt.Sprintf("%s-%d%s", h.name, n, jsonlSuffix)
		}
		if !s.writeJSONL(path.Join(dir, target), lines) {
			return hourStopped
		}
		s.compacted = append(s.compacted, path.Join(dir, target))
		pause("file-closed")
	}
	removed := false
	for _, n := range remove {
		if removeHook != nil && !removeHook(n) {
			break
		}
		if s.tel.Remove(path.Join(dir, n)) == nil {
			removed = true
		}
	}
	if len(lines) > 0 || removed {
		return hourDone
	}
	return hourSkipped
}

// writeJSONL creates name exclusively and writes every line in one write,
// then syncs and closes it. It reports whether the file is complete; a
// file that failed is removed.
func (s *Store) writeJSONL(name string, lines [][]byte) bool {
	data := append(bytes.Join(lines, []byte{'\n'}), '\n')
	f, err := s.tel.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	pause("file-created")
	err = writeAll(f, name, data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = s.tel.Remove(name)
		return false
	}
	return true
}

// pruneFacts removes up to maxFactsPerPass facts files whose stored time is
// past FactsMaxAge, and up to maxFactsPerPass temporary files left by a
// writer that died, each at least factsTempAge old.
func (s *Store) pruneFacts(now time.Time) {
	entries, err := s.tel.ReadDir(dirServers)
	if err != nil {
		return
	}
	oldFacts, oldTemps := 0, 0
	for _, e := range entries {
		if s.expired() {
			return
		}
		n := e.Name()
		name := path.Join(dirServers, n)
		switch {
		case e.IsDir():
		case strings.HasPrefix(n, factsTempPrefix):
			if oldTemps >= maxFactsPerPass {
				continue
			}
			info, err := s.tel.Stat(name)
			if err != nil || now.Sub(info.ModTime()) < factsTempAge {
				continue
			}
			if s.tel.Remove(name) == nil {
				oldTemps++
			}
		case strings.HasSuffix(n, ".json"):
			if oldFacts >= maxFactsPerPass {
				continue
			}
			data, err := s.readFile(name)
			if err != nil {
				continue
			}
			f, ok := parseFacts(data)
			if !ok || f.UpdatedAt.IsZero() || now.Sub(f.UpdatedAt) <= FactsMaxAge {
				continue
			}
			if s.tel.Remove(name) == nil {
				oldFacts++
			}
		}
	}
}

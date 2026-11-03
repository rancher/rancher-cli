package telemetry

import (
	"math/rand/v2"
	"time"

	"github.com/rancher/rancher-cli/internal/fsacl"
)

// Limits the contract publishes.
const (
	// SlotsPerHour is N: the most events one hour directory holds.
	SlotsPerHour = 1024
	// SlotTries is k: how many random slots a writer tries before it drops
	// the event.
	SlotTries = 8
	// AgeCap is how long a compacted hour stays on disk.
	AgeCap = 72 * time.Hour
	// MaxUploadAge is the oldest event the upload sends.
	MaxUploadAge = 30 * 24 * time.Hour
	// CountLimit is the most events Count reports.
	CountLimit = 10000

	maxHoursPerPass = 2
	maxFactsPerPass = 8
	pruneInterval   = time.Hour
	idStaleAfter    = time.Minute
	// UploadCommandBound is the longest the upload command runs. It stays below
	// idStaleAfter: no process takes over the upload lock the command holds.
	UploadCommandBound = 30 * time.Second
	// youngFileAge is how long a compacted file counts as still being
	// written by another process.
	youngFileAge = time.Minute
	// readLimit bounds the read phase of one hourly pass: it stops reading
	// slots once it has passed, and writes what it read. What is left waits
	// for the next pass.
	readLimit = 150 * time.Millisecond
	// maxMarkersPerFile is the most drop markers one written file counts and
	// removes.
	maxMarkersPerFile = 1024
	// maxEventSize is the most bytes a stored event may have; a bigger file
	// is not an event.
	maxEventSize = 16 * 1024
	// maxIDSize is the most bytes read from the id file; a longer file is
	// not an id.
	maxIDSize = 256
	// maxHourFileSize is the most bytes read from one compacted file.
	maxHourFileSize = 24 * 1024 * 1024
	// factsTempAge is how old a facts temporary file must be before the
	// pass removes it.
	factsTempAge = time.Hour
)

// Seams. Tests replace them; the calling test must not use t.Parallel().
var (
	nowFunc   = time.Now
	randIntn  = rand.IntN
	slotCount = SlotsPerHour
	slotTries = SlotTries
	hardenDir = fsacl.HardenInheritable
	isBusy    = isShareViolation
	observer  func(op string)
	writeHook func(path string, size int) error
	syncHook  func(path string) error
	// removeHook is called before each removal that follows a compacted
	// file; false stops the removals, as a crash would.
	removeHook func(path string) bool
	readHook   func(path string) error
	pauseHook  func(stage string)
	// linkHook, removeFileHook, removeAllHook and renameHook run before the
	// file operation they are named for; an error they return is the
	// result and the operation does not run.
	linkHook       func(oldname, newname string) error
	removeFileHook func(name string) error
	removeAllHook  func(name string) error
	renameHook     func(oldname, newname string) error
	retryDelay     = 10 * time.Millisecond
	// markerCap is maxMarkersPerFile as a variable, for tests.
	markerCap = maxMarkersPerFile
)

// SetObserverForTest sets a function called with "<operation> <path>" for
// each file operation the package makes, in order, the path relative to the
// directory the operation ran in; call the returned restore in
// cleanup. The calling test must not use t.Parallel(): the seam is
// process-wide.
func SetObserverForTest(fn func(op string)) (restore func()) {
	old := observer
	observer = fn
	return func() { observer = old }
}

func pause(stage string) {
	if pauseHook != nil {
		pauseHook(stage)
	}
}

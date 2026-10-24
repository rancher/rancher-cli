package telemetrycmd

import (
	"context"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// changes is everything the commands change: the telemetry key of
// config.yaml and the three kinds of recorded data. The commands hold one,
// set by their constructor to the production implementation, so a test can
// replace any step with a failing one.
type changes interface {
	// writeKey stores text as the telemetry key under the config lock. It
	// returns the text the key held before when that text was neither
	// absent, "true" nor "false".
	writeKey(ctx context.Context, dir, text string) (replaced *string, err error)
	deleteEvents(dir string) error
	deleteFacts(dir string) error
	deleteDirectoryID(dir string) error
}

type fileChanges struct{}

func (fileChanges) writeKey(ctx context.Context, dir, text string) (*string, error) {
	var replaced *string
	err := config.WithLock(ctx, dir, func() error {
		cfg, err := config.Load(config.FilePath(dir))
		if err != nil {
			return err
		}
		if telemetry.ConfigSettingOf(cfg.Telemetry) == telemetry.ConfigInvalid {
			old := *cfg.Telemetry
			replaced = &old
		}
		cfg.Telemetry = &text
		return cfg.Write()
	})
	if err != nil {
		return nil, err
	}
	return replaced, nil
}

func (fileChanges) deleteEvents(dir string) error {
	return withStore(dir, (*telemetry.Store).DeleteEvents)
}

func (fileChanges) deleteFacts(dir string) error {
	return withStore(dir, (*telemetry.Store).DeleteFacts)
}

func (fileChanges) deleteDirectoryID(dir string) error {
	return withStore(dir, (*telemetry.Store).DeleteDirectoryID)
}

// openError is a store that could not be opened, as opposed to a delete that
// failed once it was open.
type openError struct{ err error }

func (e *openError) Error() string { return e.err.Error() }
func (e *openError) Unwrap() error { return e.err }

// withStore opens the telemetry store of dir, runs fn and closes it. The
// store never creates anything, and it refuses a telemetry path that is a
// symlink.
func withStore(dir string, fn func(*telemetry.Store) error) error {
	s, err := telemetry.OpenStore(dir)
	if err != nil {
		return &openError{err: err}
	}
	defer func() { _ = s.Close() }()
	return fn(s)
}

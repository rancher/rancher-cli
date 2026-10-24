package telemetrycmd

import (
	"context"
	"sync"
)

// fakeChanges runs the production changes except where a test sets an
// error: that step reports the error and does nothing else. It records every
// step in order.
type fakeChanges struct {
	mu    sync.Mutex
	calls []string

	keyErr, eventsErr, factsErr, idErr error
}

func (f *fakeChanges) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeChanges) taken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeChanges) writeKey(ctx context.Context, dir, text string) (*string, error) {
	f.record("writeKey")
	if f.keyErr != nil {
		return nil, f.keyErr
	}
	return fileChanges{}.writeKey(ctx, dir, text)
}

func (f *fakeChanges) deleteEvents(dir string) error {
	f.record("deleteEvents")
	if f.eventsErr != nil {
		return f.eventsErr
	}
	return fileChanges{}.deleteEvents(dir)
}

func (f *fakeChanges) deleteFacts(dir string) error {
	f.record("deleteFacts")
	if f.factsErr != nil {
		return f.factsErr
	}
	return fileChanges{}.deleteFacts(dir)
}

func (f *fakeChanges) deleteDirectoryID(dir string) error {
	f.record("deleteDirectoryID")
	if f.idErr != nil {
		return f.idErr
	}
	return fileChanges{}.deleteDirectoryID(dir)
}

var _ changes = (*fakeChanges)(nil)

package runtime

import "context"

// readOnlyKey is the context key of the read-only state runRead gives the
// read command it executes.
type readOnlyKey struct{}

// readOnlyState is the read-only state of one run: whether read-only mode
// is on, and the names of the sources that turned it on.
type readOnlyState struct {
	active  bool
	sources []string
}

// withReadOnly returns a context that carries the read-only state of the
// run. A command reads it from the context its Execute receives: the
// Input runRead passes has only the Input methods and TextTable.
func withReadOnly(ctx context.Context, active bool, sources []string) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, readOnlyState{active: active, sources: sources})
}

// readOnlyFrom returns the read-only state ctx carries; when ctx carries
// none, read-only mode is off and there are no sources.
func readOnlyFrom(ctx context.Context) (bool, []string) {
	s, _ := ctx.Value(readOnlyKey{}).(readOnlyState)
	return s.active, s.sources
}

package config

import "errors"

// CommitToken persists token through store and, when the backend can safely
// reverse the write, returns a compensation func that restores exactly the
// prior state: the prior token when one existed, or a delete when the read
// affirmatively found none (ErrNoToken). A NonAtomicStore backend (an exec
// helper) returns a nil compensate on success — its own write-ahead record
// already names the one durable side effect a caller needs to report, never
// something to invent a restore for. A prior read that fails with anything
// OTHER than ErrNoToken (a transient keyring/file error) is not evidence of
// absence: also returns a nil compensate, since a delete-compensation there
// could erase a credential that is still there. store.SetToken's error,
// including ErrTokenStoreUnsupported, is returned unwrapped so callers keep
// using errors.Is against it.
func CommitToken(store CredStore, ref ServerRef, token string) (compensate func() error, err error) {
	if na, ok := store.(NonAtomicStore); ok && na.NonAtomic() {
		if err := store.SetToken(ref, token); err != nil {
			return nil, err
		}
		return nil, nil
	}
	prior, priorErr := store.GetToken(ref)
	if err := store.SetToken(ref, token); err != nil {
		return nil, err
	}
	switch {
	case priorErr == nil:
		return func() error { return store.SetToken(ref, prior) }, nil
	case errors.Is(priorErr, ErrNoToken):
		return func() error { return store.DeleteToken(ref) }, nil
	default:
		return nil, nil
	}
}

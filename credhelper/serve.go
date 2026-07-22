package credhelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrStoreUnsupported is returned by a Handler's Store to declare the helper
// read-only; Serve translates it into the {"unsupported":true} response.
var ErrStoreUnsupported = errors.New("store is not supported by this helper")

// Credentials is a Handler's answer to Get.
type Credentials struct {
	Token     string
	ExpiresAt time.Time // zero = long-lived
}

// Handler is what a helper author implements; Serve handles verb dispatch,
// JSON codec, versioning, and exit codes. Get returns (nil, nil) when no
// credential exists. Erase of a missing credential returns nil (idempotent).
type Handler interface {
	Get(serverURL, name string) (*Credentials, error)
	Store(serverURL, name, token string) error
	Erase(serverURL, name string) error
}

// Serve runs one helper invocation from os.Args/stdio and exits.
func Serve(h Handler) {
	os.Exit(serve(h, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func serve(h Handler, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, format+"\n", a...)
		return 1
	}
	if len(args) != 1 {
		return fail("usage: <helper> get|store|erase (request JSON on stdin)")
	}
	verb := args[0]
	switch verb {
	case VerbGet, VerbStore, VerbErase:
	default:
		return fail("unknown verb %q (supported: get, store, erase)", verb)
	}

	var req Request
	dec := json.NewDecoder(stdin) // lenient: unknown request fields are ignored per protocol v1
	if err := dec.Decode(&req); err != nil {
		return fail("decoding request: %v", err)
	}
	if req.ProtocolVersion != ProtocolVersion {
		return fail("unsupported protocol version %d (helper speaks %d)", req.ProtocolVersion, ProtocolVersion)
	}
	if req.ServerURL == "" || req.Name == "" {
		return fail("request missing serverURL or name")
	}

	resp := Response{ProtocolVersion: ProtocolVersion}
	switch verb {
	case VerbGet:
		creds, err := h.Get(req.ServerURL, req.Name)
		if err != nil {
			return fail("get: %v", err)
		}
		if creds != nil {
			resp.Token = creds.Token
			if !creds.ExpiresAt.IsZero() {
				resp.ExpiresAt = creds.ExpiresAt.UTC().Format(time.RFC3339)
			}
		}
	case VerbStore:
		if req.Token == "" {
			return fail("store: request missing token")
		}
		switch err := h.Store(req.ServerURL, req.Name, req.Token); {
		case errors.Is(err, ErrStoreUnsupported):
			resp.Unsupported = true
		case err != nil:
			return fail("store: %v", err)
		}
	case VerbErase:
		if err := h.Erase(req.ServerURL, req.Name); err != nil {
			return fail("erase: %v", err)
		}
	}
	if err := json.NewEncoder(stdout).Encode(resp); err != nil {
		return fail("encoding response: %v", err)
	}
	return 0
}

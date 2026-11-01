package telemetrysend

import "github.com/rancher/rancher-cli/internal/telemetry"

// Request is one upload request: the path under the receiver's origin, the
// content type, and the body as it goes on the wire before any compression.
type Request struct {
	Path        string
	ContentType string
	Body        []byte
}

// Encoder turns stored events into a request and reads a receiver's answer.
// An implementation is a pure function of its input: it reads no file and
// makes no request.
type Encoder interface {
	// Encode returns the request for events, in order. It fails only for an
	// empty list; an event never makes it fail.
	Encode(events []telemetry.StoredEvent) (Request, error)
	// RefusedCount reads the number of records the answer says the receiver
	// refused. known is false when the answer gives no usable number.
	RefusedCount(answer []byte) (count int, known bool)
}

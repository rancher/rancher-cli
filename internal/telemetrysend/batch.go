package telemetrysend

import (
	"bytes"
	"compress/gzip"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// The limits of one request, from the receiver's: records, values (the
// attributes and array items of the events together), the body after
// decompression and the body as sent.
const (
	maxRequestEvents = 1000
	maxRequestValues = 250000
	maxBodyBytes     = 8 << 20
	maxSentBytes     = 1 << 20
)

// schemaVersion returns the contract version a stored event names.
func schemaVersion(ev telemetry.StoredEvent) string {
	v, _ := ev.Fields[keySchemaVersion].(string)
	return v
}

// eventValues is an upper bound of the attributes and array items the encoder
// writes for an event: its keys plus the items of its list values.
func eventValues(ev telemetry.StoredEvent) int {
	n := len(ev.Fields)
	for _, v := range ev.Fields {
		if list, ok := v.([]any); ok {
			n += len(list)
		}
	}
	return n
}

// cut groups events by contract version, in the order of each version's first
// event, and cuts each group into batches of at most maxRequestEvents events
// and maxRequestValues values. Events keep their order within a version. A
// batch always holds at least one event.
func cut(events []telemetry.StoredEvent) [][]telemetry.StoredEvent {
	var order []string
	groups := map[string][]telemetry.StoredEvent{}
	for _, ev := range events {
		v := schemaVersion(ev)
		if _, ok := groups[v]; !ok {
			order = append(order, v)
		}
		groups[v] = append(groups[v], ev)
	}
	var batches [][]telemetry.StoredEvent
	for _, v := range order {
		var cur []telemetry.StoredEvent
		values := 0
		for _, ev := range groups[v] {
			n := eventValues(ev)
			if len(cur) > 0 && (len(cur) >= maxRequestEvents || values+n > maxRequestValues) {
				batches = append(batches, cur)
				cur, values = nil, 0
			}
			cur = append(cur, ev)
			values += n
		}
		if len(cur) > 0 {
			batches = append(batches, cur)
		}
	}
	return batches
}

// built is one request ready to send: the events it holds, the encoder's
// request and its body compressed.
type built struct {
	events []telemetry.StoredEvent
	req    Request
	body   []byte
}

// fit encodes the batch and compresses the body. When the body is over the
// size limits and holds more than one event, it halves the batch and encodes
// the first half again; rest is what was cut off, to be sent after. A body over
// the decompressed limit is halved before it is compressed.
func fit(enc Encoder, batch []telemetry.StoredEvent) (b built, rest []telemetry.StoredEvent, err error) {
	for {
		req, err := enc.Encode(batch)
		if err != nil {
			return built{}, nil, err
		}
		if len(req.Body) > maxBodyBytes && len(batch) > 1 {
			batch, rest = halve(batch, rest)
			continue
		}
		body, err := compress(req.Body)
		if err != nil {
			return built{}, nil, err
		}
		if len(body) > maxSentBytes && len(batch) > 1 {
			batch, rest = halve(batch, rest)
			continue
		}
		return built{events: batch, req: req, body: body}, rest, nil
	}
}

// halve returns the first half of the batch, and the second half put in front
// of rest. The append runs on a slice with its capacity cut, so it never writes
// into the batch.
func halve(batch, rest []telemetry.StoredEvent) (first, newRest []telemetry.StoredEvent) {
	half := len(batch) / 2
	return batch[:half], append(batch[half:len(batch):len(batch)], rest...)
}

// compress returns data in gzip form.
func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

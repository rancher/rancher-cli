package steve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// MaxPages bounds a paged collection read. steve's default page holds
// 100000 items, so a real installation fits in one; the bound exists so a
// server that keeps handing out continue tokens cannot keep the CLI reading.
const MaxPages = 100

// Decoder turns one raw collection item into a row and the id it is
// deduplicated by. index is the item's position in the raw stream.
type Decoder[T comparable] func(path string, index int, raw json.RawMessage) (id string, row T, err error)

// page is the part of a collection response the reader consumes: count is
// the collection total before paging, the same on every page; continue is
// a per-page token; revision is the cache's resourceVersion for the page.
type page struct {
	Data     []json.RawMessage `json:"data"`
	Count    *int              `json:"count"`
	Continue string            `json:"continue"`
	Revision string            `json:"revision"`
}

// ReadCollection reads a whole collection. The read is retried once as a
// whole when its pages disagree with each other; a second disagreement is
// the retryable collectionChanged error. A malformed item or response is
// terminal on the first read.
func ReadCollection[T comparable](ctx context.Context, get Getter, r Request, decode Decoder[T]) ([]T, error) {
	var lastErr error
	for attempt := range 2 {
		rows, err := readOnce(ctx, get, r, decode)
		if err == nil {
			return rows, nil
		}
		ce, ok := errors.AsType[*clierr.Error](err)
		if !ok || !ce.Retryable || attempt == 1 {
			return nil, err
		}
		if _, answered := clierr.HTTPStatus(err); answered {
			// A server's refusal is returned at once; its retryable flag
			// is the caller's to act on.
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func readOnce[T comparable](ctx context.Context, get Getter, r Request, decode Decoder[T]) ([]T, error) {
	path := r.Path()
	var (
		rows     []T
		byID     = map[string]T{}
		raw      int
		total    *int
		revision string
		seen     = map[string]bool{}
	)
	for n := 0; ; n++ {
		if n == MaxPages {
			return nil, protocolError(path, fmt.Sprintf("more than %d pages", MaxPages))
		}
		body, err := get(ctx, r)
		if err != nil {
			return nil, err
		}
		var col page
		if err := json.Unmarshal(body, &col); err != nil {
			return nil, protocolError(path, "not a steve collection: "+err.Error())
		}
		if col.Revision == "" {
			return nil, protocolError(path, "a page without a revision")
		}
		if col.Count == nil && n == 0 && len(col.Data) == 0 && col.Continue == "" {
			// The apiserver omits a zero count: an empty first and only
			// page is an empty collection.
			return nil, nil
		}
		if col.Count == nil || *col.Count < 0 {
			return nil, protocolError(path, "a page without a count")
		}
		if n == 0 {
			revision, total = col.Revision, col.Count
		} else {
			if col.Revision != revision {
				return nil, collectionChanged(path, "revision moved between pages")
			}
			if *col.Count != *total {
				return nil, collectionChanged(path, fmt.Sprintf("count moved between pages (%d, then %d)", *total, *col.Count))
			}
		}
		for i, item := range col.Data {
			id, row, err := decode(path, raw+i, item)
			if err != nil {
				return nil, err
			}
			if prev, dup := byID[id]; dup {
				if prev != row {
					return nil, collectionChanged(path, "repeated item "+id+" disagrees with itself")
				}
				continue
			}
			byID[id] = row
			rows = append(rows, row)
		}
		raw += len(col.Data)
		if col.Continue == "" {
			break
		}
		if seen[col.Continue] {
			return nil, protocolError(path, "the continue token repeated")
		}
		seen[col.Continue] = true
		r.Continue = col.Continue
	}
	if *total != raw {
		return nil, collectionChanged(path, fmt.Sprintf("count %d, read %d", *total, raw))
	}
	return rows, nil
}

// collectionChanged is the retryable error a read ends with when its pages
// cannot have come from one state of the collection.
func collectionChanged(path, why string) *clierr.Error {
	return clierr.Retryable(clierr.CodeGeneric,
		fmt.Sprintf("the collection %s changed while it was being read (%s); retry the command", path, why))
}

// protocolError is the terminal answer to a response the reader cannot
// trust: a shape it does not recognize is never guessed at.
func protocolError(path, why string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("unexpected answer from %s: %s", path, why))
}

package steve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

const (
	// UserAttributesType is the steve type of the UserAttribute collection.
	UserAttributesType = "management.cattle.io.userattributes"

	// userAttributeSchemaID is steve's schema id of the type, singular; the
	// plural resolves to it.
	userAttributeSchemaID = "management.cattle.io.userattribute"
)

// refusal marks an error the reader returns for a read steve or the
// authorizer refused, as opposed to a failure of the request.
type refusal struct{ cause *clierr.Error }

func (r *refusal) Error() string { return r.cause.Message }

func (r *refusal) Unwrap() error { return r.cause }

// IsUserAttributesRefusal reports whether err is the reader's answer for a
// read the caller may not make: the UserAttributes are unavailable, which is
// a different fact from there being none. Any other error is a failure of
// the request.
func IsUserAttributesRefusal(err error) bool {
	_, ok := errors.AsType[*refusal](err)
	return ok
}

// UserAttributes reads Rancher's UserAttributes through steve (/v1), which
// applies the caller's RBAC. Rancher's proxy to the local cluster refuses
// userattributes to a caller without access to that cluster whatever their
// roles say, so no read goes through it. One reader serves one command run:
// it asks steve for its schema and the authorizer for the list right at most
// once each.
type UserAttributes struct {
	get    Getter
	review func(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error)

	schema   memo[bool]
	listable memo[listCheck]
}

// NewUserAttributes builds the reader from a steve getter and the local
// cluster client, which posts the list check.
func NewUserAttributes(get Getter, local engine.ClusterClient) *UserAttributes {
	return &UserAttributes{get: get, review: local.PostReview}
}

// Get reads one UserAttribute. It returns found=false with no error when
// the user has none (the user never logged in). A refusal is an error for
// which IsUserAttributesRefusal is true.
//
// A steve 404 for one object does not say whether the object is missing or
// the caller may not read the type: steve answers 404 for both. Only a 404
// is followed by a read of steve's schema for the type, which steve serves
// to a caller with get on it.
func (u *UserAttributes) Get(ctx context.Context, id string) (*unstructured.Unstructured, bool, error) {
	r := Request{Type: UserAttributesType, Name: id}
	body, err := u.get(ctx, r)
	if err != nil {
		st, ok := clierr.HTTPStatus(err)
		switch {
		case ok && st == http.StatusForbidden:
			return nil, false, refusedByID(r, st, false, err)
		case ok && st == http.StatusNotFound:
			visible, serr := u.schemaVisible(ctx)
			if serr != nil {
				return nil, false, serr
			}
			if visible {
				return nil, false, nil
			}
			return nil, false, refusedByID(r, st, true, err)
		}
		return nil, false, clierr.From(err)
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &obj.Object); err != nil || obj.Object == nil {
		return nil, false, protocolError(r.Path(), "not a JSON object")
	}
	return obj, true, nil
}

// List reads every UserAttribute. A 200 answer is complete only when the
// authorizer says the caller may list the type: a caller with get but no
// list gets 200 with no items. A refusal is an error for which
// IsUserAttributesRefusal is true.
func (u *UserAttributes) List(ctx context.Context) ([]*unstructured.Unstructured, error) {
	r := Request{Type: UserAttributesType}
	rows, err := ReadCollection(ctx, u.get, r, decodeUserAttributeRow)
	if err != nil {
		if st, ok := clierr.HTTPStatus(err); ok && (st == http.StatusForbidden || st == http.StatusNotFound) {
			return nil, refusedList(r, st, err)
		}
		return nil, clierr.From(err)
	}
	check, err := u.mayList(ctx)
	if nr, ok := errors.AsType[*noReviewResponse](err); ok {
		check, err = listCheck{answer: nr.err.Message}, nil
	}
	if err != nil {
		return nil, err
	}
	if !check.allowed {
		return nil, &refusal{cause: clierr.New(clierr.CodeRBAC, fmt.Sprintf(
			"you cannot list every userattribute (a SelfSubjectAccessReview for list on userattributes.management.cattle.io answered %s); GET %s may leave items out",
			check.answer, r.Path())+userAttributesRemedy())}
	}
	out := make([]*unstructured.Unstructured, 0, len(rows))
	for _, row := range rows {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal([]byte(row.JSON), &obj.Object); err != nil {
			return nil, protocolError(r.Path(), "an item is not a JSON object")
		}
		out = append(out, obj)
	}
	return out, nil
}

// userAttributeRow is one collection item: the raw JSON as a string keeps
// the row comparable, which the collection reader needs.
type userAttributeRow struct{ ID, JSON string }

func decodeUserAttributeRow(path string, index int, raw json.RawMessage) (string, userAttributeRow, error) {
	var item struct {
		ID       string `json:"id"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", userAttributeRow{}, protocolError(path, fmt.Sprintf("item %d is not a JSON object", index))
	}
	id := item.ID
	if id == "" {
		id = item.Metadata.Name
	}
	if id == "" {
		return "", userAttributeRow{}, protocolError(path, fmt.Sprintf("item %d has no id", index))
	}
	return id, userAttributeRow{ID: id, JSON: string(raw)}, nil
}

// schemaVisible reports whether steve serves the type's schema to the caller.
func (u *UserAttributes) schemaVisible(ctx context.Context) (bool, error) {
	return u.schema.do(ctx, func() (bool, error) {
		_, err := u.get(ctx, Request{Type: "schemas", Name: userAttributeSchemaID})
		if err == nil {
			return true, nil
		}
		if st, ok := clierr.HTTPStatus(err); ok && st == http.StatusNotFound {
			return false, nil
		}
		return false, clierr.From(err)
	})
}

// listCheck is the authorizer's answer to whether the caller may list the
// type: allowed, or the words of the answer that said no.
type listCheck struct {
	allowed bool
	answer  string
}

// noReviewResponse is the list check's failure to get any answer from the
// server. It travels as an error so the memo does not store it; List turns
// it into the refusal.
type noReviewResponse struct{ err *clierr.Error }

func (e *noReviewResponse) Error() string { return e.err.Message }

func (e *noReviewResponse) Unwrap() error { return e.err }

// mayList asks the local cluster's authorizer (a SelfSubjectAccessReview
// through the proxy) whether the caller may list userattributes. A review
// that answered with a failure status is an answer of no; a review that got
// no answer is a noReviewResponse; a 401 passes through, and so does the
// context's error when the context is done, whatever the review reported.
func (u *UserAttributes) mayList(ctx context.Context) (listCheck, error) {
	return u.listable.do(ctx, func() (listCheck, error) {
		gvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
			"spec": map[string]any{"resourceAttributes": map[string]any{
				"verb": "list", "group": "management.cattle.io", "resource": "userattributes",
			}},
		}}
		resp, err := u.review(ctx, engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: obj})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return listCheck{}, clierr.From(ctxErr)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return listCheck{}, clierr.From(err)
			}
			ce := clierr.From(err)
			if ce.Code == clierr.CodeAuth {
				return listCheck{}, ce
			}
			return listCheck{}, &noReviewResponse{err: ce}
		}
		switch {
		case resp.Status == http.StatusUnauthorized:
			if resp.Mapped != nil {
				return listCheck{}, resp.Mapped
			}
			return listCheck{}, clierr.New(clierr.CodeAuth, "the server answered 401 to the list check")
		case resp.Status < 200 || resp.Status > 299 || resp.Object == nil:
			msg := fmt.Sprintf("HTTP %d", resp.Status)
			if resp.Mapped != nil {
				msg = resp.Mapped.Message
			}
			return listCheck{answer: msg}, nil
		}
		allowed, _, _ := unstructured.NestedBool(resp.Object.Object, "status", "allowed")
		if !allowed {
			return listCheck{answer: "denied"}, nil
		}
		return listCheck{allowed: true}, nil
	})
}

// userAttributesRemedy is the grant that lets a caller read UserAttributes.
func userAttributesRemedy() string {
	return clierr.Text("; a GlobalRole with get and list on userattributes.management.cattle.io allows it",
		clierr.Lead("; see: "), clierr.NewCommand("get globalroles"))
}

func noTypeClause(status int) string {
	if status == http.StatusNotFound {
		return "; Rancher's API shows you no userattributes type"
	}
	return ""
}

func refusedByID(r Request, status int, schemaChecked bool, cause error) error {
	msg := fmt.Sprintf("you cannot read userattributes (GET %s answered %d)", r.Path(), status)
	if schemaChecked {
		msg += noTypeClause(status)
	}
	return &refusal{cause: clierr.New(clierr.CodeRBAC, msg+userAttributesRemedy()).WithCause(cause)}
}

func refusedList(r Request, status int, cause error) error {
	msg := fmt.Sprintf("you cannot list userattributes (GET %s answered %d)", r.Path(), status) + noTypeClause(status)
	return &refusal{cause: clierr.New(clierr.CodeRBAC, msg+userAttributesRemedy()).WithCause(cause)}
}

// memo holds one answer for the life of a reader. The first call computes
// it under the caller's context; a context error, a 401 and a retryable
// failure are returned and never stored, so one cancelled or interrupted
// read cannot answer for the next. An answer that says no, a failure status
// of the list check included, is an answer and is stored.
type memo[T any] struct {
	mu   sync.Mutex
	done bool
	val  T
	err  error
}

func (m *memo[T]) do(ctx context.Context, f func() (T, error)) (T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return m.val, m.err
	}
	v, err := f()
	if err == nil || storable(ctx, err) {
		m.done, m.val, m.err = true, v, err
	}
	return v, err
}

func storable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[*noReviewResponse](err); ok {
		return false
	}
	ce, ok := errors.AsType[*clierr.Error](err)
	return !ok || (ce.Code != clierr.CodeAuth && !ce.Retryable)
}

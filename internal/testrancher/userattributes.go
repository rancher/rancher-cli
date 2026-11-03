package testrancher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
)

const (
	// UserAttributesCollectionPath is the steve collection of UserAttributes;
	// UserAttributesCollectionPath/<id> is one object. The facade serves it
	// only when Options.UserAttributeAccess says what each bearer may do.
	UserAttributesCollectionPath = "/v1/management.cattle.io.userattributes"

	// userAttributeSchemaPath and userAttributeSchemaPluralPath are steve's
	// schema of the type; the plural spelling resolves to the same schema.
	userAttributeSchemaPath       = "/v1/schemas/management.cattle.io.userattribute"
	userAttributeSchemaPluralPath = "/v1/schemas/management.cattle.io.userattributes"

	// localSelfSubjectAccessReviewsPath is where a SelfSubjectAccessReview
	// goes through the local-cluster proxy.
	localSelfSubjectAccessReviewsPath = "/k8s/clusters/local/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
)

// UserAttributeLevel is what a bearer may do with userattributes.
type UserAttributeLevel int

const (
	// UserAttributesNone: no rule on userattributes. Steve answers 404 with
	// an empty body to every read, and the schema is not served.
	UserAttributesNone UserAttributeLevel = iota
	// UserAttributesGet: get without list. A read by id works, the
	// collection answers 200 with no items, and a list review is denied.
	UserAttributesGet
	// UserAttributesGetList: get and list, as an administrator has.
	UserAttributesGetList
)

// userAttributeLevel is the bearer's level; ok is false for a bearer the
// facade does not know.
func (f *Facade) userAttributeLevel(r *http.Request) (UserAttributeLevel, bool) {
	return f.opts.UserAttributeAccess(bearerFromHeader(r))
}

// isUserAttributeRoute reports whether path is one of the steve paths the
// userattributes option serves.
func isUserAttributeRoute(path string) bool {
	return path == UserAttributesCollectionPath || strings.HasPrefix(path, UserAttributesCollectionPath+"/") ||
		path == userAttributeSchemaPath || path == userAttributeSchemaPluralPath
}

// handleUserAttributes answers the steve reads of UserAttributes and of
// their schema per the bearer's level.
func (f *Facade) handleUserAttributes(w http.ResponseWriter, r *http.Request) {
	level, ok := f.userAttributeLevel(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if level == UserAttributesNone {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	path := r.URL.Path
	switch path {
	case userAttributeSchemaPath, userAttributeSchemaPluralPath:
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "management.cattle.io.userattribute", "type": "schema",
			"links":           map[string]any{},
			"resourceMethods": []string{"GET"}, "collectionMethods": []string{"GET"},
		})
	case UserAttributesCollectionPath:
		items := []map[string]any{}
		if level == UserAttributesGetList {
			items = f.userAttributeItems()
		}
		f.serveSteveCollection(w, r, "management.cattle.io.userattribute", items)
	default:
		id := strings.TrimPrefix(path, UserAttributesCollectionPath+"/")
		for _, item := range f.userAttributeItems() {
			if item["id"] == id {
				writeJSON(w, http.StatusOK, item)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{
			"type": "error", "links": map[string]any{}, "code": "NotFound", "status": http.StatusNotFound,
			"message": fmt.Sprintf("userattributes.management.cattle.io %q not found", id),
		})
	}
}

// userAttributeItems are the stored UserAttributes as steve serves them,
// sorted by id.
func (f *Facade) userAttributeItems() []map[string]any {
	var items []map[string]any
	if f.opts.UserAttributeObjects != nil {
		for _, obj := range f.opts.UserAttributeObjects() {
			item := maps.Clone(obj)
			meta, _ := item["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			item["id"], item["type"], item["links"] = name, "management.cattle.io.userattribute", map[string]any{}
			meta = maps.Clone(meta)
			item["metadata"] = meta
			addSteveState(item)
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
	return items
}

// handleUserAttributeReview answers the SelfSubjectAccessReview the CLI
// posts through the local-cluster proxy to learn whether the caller may list
// userattributes: allowed for get and list, denied otherwise, 401 for a
// bearer the facade does not know. A review about anything else falls
// through.
func (f *Facade) handleUserAttributeReview(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var review struct {
		Spec struct {
			ResourceAttributes struct {
				Verb, Group, Resource string
			} `json:"resourceAttributes"`
		} `json:"spec"`
	}
	ra := &review.Spec.ResourceAttributes
	if json.Unmarshal(body, &review) != nil || ra.Verb != "list" || ra.Group != "management.cattle.io" || ra.Resource != "userattributes" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fallthroughTo(w, r)
		return
	}
	level, ok := f.userAttributeLevel(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"status": map[string]any{"allowed": level == UserAttributesGetList},
	})
}

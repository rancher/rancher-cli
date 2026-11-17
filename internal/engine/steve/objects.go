package steve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// objectRow is one collection item kept as Steve returned it: the raw JSON as
// a string keeps objectRow comparable, which the collection reader needs.
type objectRow struct{ ID, JSON string }

// objectDecoder keeps each item the inner decoder accepts as JSON text. Every
// path in required must also hold a non-empty string in the item: the
// commands that show the objects name and sort them by those fields.
func objectDecoder[T comparable](inner Decoder[T], required ...[]string) Decoder[objectRow] {
	return func(path string, index int, raw json.RawMessage) (string, objectRow, error) {
		id, _, err := inner(path, index, raw)
		if err != nil {
			return "", objectRow{}, err
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", objectRow{}, protocolError(path, fmt.Sprintf("item %d is a JSON %s, not an object", index, jsonClass(raw)))
		}
		for _, field := range required {
			if v, _, _ := unstructured.NestedString(obj, field...); v == "" {
				return "", objectRow{}, protocolError(path, fmt.Sprintf("item %q has no %s", id, strings.Join(field, ".")))
			}
		}
		return id, objectRow{ID: id, JSON: string(raw)}, nil
	}
}

func objectsOf(r Request, rows []objectRow) ([]unstructured.Unstructured, error) {
	out := make([]unstructured.Unstructured, 0, len(rows))
	for _, row := range rows {
		var obj unstructured.Unstructured
		if err := json.Unmarshal([]byte(row.JSON), &obj.Object); err != nil || obj.Object == nil {
			return nil, protocolError(r.Path(), fmt.Sprintf("item %q is not a JSON object", row.ID))
		}
		out = append(out, obj)
	}
	return out, nil
}

// ClusterObjects reads the clusters the caller can read, each as Steve returns
// it (KubernetesObject converts one to the Kubernetes shape).
func ClusterObjects(ctx context.Context, get Getter) ([]unstructured.Unstructured, error) {
	r := Request{Type: ClustersType}
	rows, err := ReadCollection(ctx, get, r, objectDecoder(decodeCluster, []string{"metadata", "name"}))
	if err != nil {
		return nil, collectionRefusal(r, err)
	}
	return objectsOf(r, rows)
}

// ProjectObjects reads the projects the caller can read on clusterID, or on
// every cluster when clusterID is "", each as Steve returns it.
func ProjectObjects(ctx context.Context, get Getter, clusterID string) ([]unstructured.Unstructured, error) {
	r := Request{Type: ProjectsType, Namespace: clusterID}
	rows, err := ReadCollection(ctx, get, r, objectDecoder(projectDecoder(clusterID), []string{"metadata", "name"}, []string{"metadata", "namespace"}))
	if err != nil {
		return nil, collectionRefusal(r, err)
	}
	return objectsOf(r, rows)
}

// GetProject reads project id of clusterID through Steve. A non-2xx response
// is returned as the getter's error, whose status clierr.HTTPStatus reads.
func GetProject(ctx context.Context, get Getter, clusterID, id string) (*unstructured.Unstructured, error) {
	r := Request{Type: ProjectsType, Namespace: clusterID, Name: id}
	body, err := get(ctx, r)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &obj.Object); err != nil || obj.Object == nil {
		return nil, protocolError(r.Path(), "not a JSON object")
	}
	if name := obj.GetName(); name != id {
		return nil, protocolError(r.Path(), fmt.Sprintf("the response names project %q, not the project requested", name))
	}
	if ns := obj.GetNamespace(); ns != clusterID {
		return nil, protocolError(r.Path(), fmt.Sprintf("the response is in namespace %q, not %q, the id of the cluster requested", ns, clusterID))
	}
	return obj, nil
}

// GetClusterObject reads cluster id through Steve and returns the object as
// Steve returns it. It fails with the same errors as GetCluster, and a body
// that does not name the cluster requested is a protocol error.
func GetClusterObject(ctx context.Context, get Getter, id string) (*unstructured.Unstructured, error) {
	obj, err := GetCluster(ctx, get, id)
	if err != nil {
		return nil, err
	}
	if name := obj.GetName(); name != id {
		return nil, protocolError(Request{Type: ClustersType, Name: id}.Path(), fmt.Sprintf("the response names cluster %q, not the cluster requested", name))
	}
	return obj, nil
}

// KubernetesObject returns a copy of a Steve object in the shape the
// Kubernetes API returns it. It never modifies obj. It removes what Steve adds:
// id, type, links and actions; metadata.fields, metadata.state and
// metadata.relationships; the error and transitioning keys of each status
// condition; a condition's lastUpdateTime when it is empty (Steve fills a
// missing one from lastTransitionTime, which can also be missing, and a
// Kubernetes object never stores an empty one); and, for a Cluster,
// status.connected and the cpuRaw and memoryRaw values of status.requested and
// status.allocatable. A Steve upgrade can add more.
//
// If an object has its own top-level field named id, type, links or actions,
// Steve renames it to _id, _type, _links or _actions and puts its own addition
// under the original name. KubernetesObject renames such a field back. A
// top-level _id, _type, _links or _actions that holds the same value as
// Steve's addition of that name is a duplicate, which Steve's cache makes when
// it adds the id to an object a second time. KubernetesObject removes it.
func KubernetesObject(obj map[string]any) map[string]any {
	out, _ := deepCopy(obj).(map[string]any)
	added := map[string]any{}
	for _, k := range []string{"id", "type", "links", "actions"} {
		if v, ok := out[k]; ok {
			added[k] = v
			delete(out, k)
		}
	}
	for k, steveValue := range map[string]any{"id": added["id"], "type": added["type"], "links": added["links"], "actions": added["actions"]} {
		v, ok := out["_"+k]
		if !ok {
			continue
		}
		delete(out, "_"+k)
		if !reflect.DeepEqual(v, steveValue) {
			out[k] = v
		}
	}
	if meta, ok := out["metadata"].(map[string]any); ok {
		for _, k := range []string{"fields", "state", "relationships"} {
			delete(meta, k)
		}
	}
	status, ok := out["status"].(map[string]any)
	if !ok {
		return out
	}
	if conds, ok := status["conditions"].([]any); ok {
		for _, c := range conds {
			if cond, ok := c.(map[string]any); ok {
				delete(cond, "error")
				delete(cond, "transitioning")
				if t, ok := cond["lastUpdateTime"].(string); ok && t == "" {
					delete(cond, "lastUpdateTime")
				}
			}
		}
	}
	if kind, _ := out["kind"].(string); kind == "Cluster" {
		delete(status, "connected")
		for _, k := range []string{"requested", "allocatable"} {
			if res, ok := status[k].(map[string]any); ok {
				delete(res, "cpuRaw")
				delete(res, "memoryRaw")
			}
		}
	}
	return out
}

// deepCopy copies the maps and slices of a decoded JSON value.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(t))
		for k, e := range t {
			c[k] = deepCopy(e)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, e := range t {
			c[i] = deepCopy(e)
		}
		return c
	}
	return v
}

// jsonClass names the type of a JSON value by its first byte: object,
// array, string, boolean, null or number. raw is valid JSON.
func jsonClass(raw []byte) string {
	b := bytes.TrimLeft(raw, " \t\r\n")
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

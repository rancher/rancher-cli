package engine

import "encoding/json"

// GroupListed decodes body as a Kubernetes APIGroupList and reports whether it
// lists the API group. readable is false unless the body decodes as an
// APIGroupList: its kind is APIGroupList, or it has a groups array. Any other
// body, such as {} or an unrelated object, is not readable, and neither is a
// body that does not decode.
func GroupListed(body []byte, group string) (listed, readable bool) {
	var list struct {
		Kind   string `json:"kind"`
		Groups []struct {
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, false
	}
	if list.Kind != "APIGroupList" && list.Groups == nil {
		return false, false
	}
	for _, g := range list.Groups {
		if g.Name == group {
			return true, true
		}
	}
	return false, true
}

// ResourceNames decodes body as a Kubernetes APIResourceList of the group
// version (<group>/<version>) and returns the names of its resources.
// readable is false unless the body has a resources array and a groupVersion
// that equals groupVersion; any other body, and a body that does not decode, is
// not readable.
func ResourceNames(body []byte, groupVersion string) (names []string, readable bool) {
	var list struct {
		GroupVersion string `json:"groupVersion"`
		Resources    []struct {
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Resources == nil || list.GroupVersion != groupVersion {
		return nil, false
	}
	names = make([]string, 0, len(list.Resources))
	for _, r := range list.Resources {
		names = append(names, r.Name)
	}
	return names, true
}

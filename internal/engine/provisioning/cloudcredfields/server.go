package cloudcredfields

import (
	"encoding/json"
	"slices"
	"strings"
)

// NodeDriver is the part of a management.cattle.io/v3 NodeDriver that decides
// whether it takes a cloud credential and which fields the credential holds.
type NodeDriver struct {
	// ID is metadata.name.
	ID string
	// DisplayName is spec.displayName: the prefix of the driver's credential
	// config, and what the command takes as <driver>.
	DisplayName string
	// Active is spec.active.
	Active bool
	// AddCloudCredential is spec.addCloudCredential: Rancher builds the
	// credential schema of the driver while it is set, active or not.
	AddCloudCredential bool
	Annotations        Annotations
}

// NodeDriverOf reads a NodeDriver from its decoded object.
func NodeDriverOf(object map[string]any) NodeDriver {
	metadata, _ := object["metadata"].(map[string]any)
	spec, _ := object["spec"].(map[string]any)
	n := NodeDriver{}
	n.ID, _ = metadata["name"].(string)
	n.DisplayName, _ = spec["displayName"].(string)
	n.Active, _ = spec["active"].(bool)
	n.AddCloudCredential, _ = spec["addCloudCredential"].(bool)
	annotations := map[string]string{}
	if raw, ok := metadata["annotations"].(map[string]any); ok {
		for key, value := range raw {
			if s, ok := value.(string); ok {
				annotations[key] = s
			}
		}
	}
	n.Annotations = AnnotationsOf(annotations)
	return n
}

// HasSchema reports whether Rancher builds the credential schema of the
// driver: while it is active or has spec.addCloudCredential.
func (n NodeDriver) HasSchema() bool { return n.Active || n.AddCloudCredential }

// Driver is the credential config of the node driver, false when its
// annotations name no credential field.
func (n NodeDriver) Driver() (Driver, bool) { return FromAnnotations(n.DisplayName, n.Annotations) }

// OfferedPrefixes lists the drivers a create accepts on this server: each node
// driver with a credential schema and credential fields, sorted by display
// name, then the static s3, and alibaba when alibabaActive. Duplicates of a
// display name are listed once.
func OfferedPrefixes(drivers []NodeDriver, alibabaActive bool) []string {
	var out []string
	for _, n := range drivers {
		if _, ok := n.Driver(); ok && n.HasSchema() && n.DisplayName != "" && !slices.Contains(out, n.DisplayName) {
			out = append(out, n.DisplayName)
		}
	}
	out = append(out, "s3")
	if alibabaActive {
		out = append(out, "alibaba")
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// OperatorActive reports whether the setting kev2-operators, given as its
// decoded object, marks the operator called name active. The setting holds a
// JSON list of {"name","active"}; an absent, empty or unreadable value means
// the operator is inactive, as Rancher's default is for alibaba.
func OperatorActive(setting map[string]any, name string) bool {
	value, _ := setting["value"].(string)
	if strings.TrimSpace(value) == "" {
		value, _ = setting["default"].(string)
	}
	var operators []struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}
	if json.Unmarshal([]byte(value), &operators) != nil {
		return false
	}
	for _, o := range operators {
		if o.Name == name {
			return o.Active
		}
	}
	return false
}

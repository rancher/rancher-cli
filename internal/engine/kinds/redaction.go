package kinds

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LastAppliedAnnotation is the annotation kubectl apply writes with a copy
// of the applied object, secret values included.
const LastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// Redaction is the set of paths whose values a preview or a read never
// shows, for one resource. Kind.Redaction builds the one a preview and the
// result of an apply use. Kind.ReadRedaction builds the one a read (get)
// uses, which hides the same values except that a Secret keeps its data and
// stringData. Only those two (for a registered row) and RowlessRedaction
// build one that is not the zero value, so a call that forgets to pass its
// redaction is detectable (IsZero) and the preview planners refuse it. The
// redaction carries the resource it was built for (GVR) so a call that
// passes another kind's redaction is detectable too. Every name a redaction
// matches is matched with case folded: a field the server reads in any case
// is hidden in any case.
type Redaction struct {
	gvr schema.GroupVersionResource
	// built is false only for the zero value.
	built bool
	// secretData hides the Secret payload: /data and /stringData.
	secretData bool
	// keepSecretData leaves /data and /stringData visible although
	// secretData is set, so the redaction still hides the last-applied
	// annotation. Only Kind.ReadRedaction sets it.
	keepSecretData bool
	// read marks a redaction built by Kind.ReadRedaction.
	read bool
	// paths are key sequences below the object root, each hiding the value
	// at the path and everything under it. A "*" segment is a wildcard: it
	// matches any one segment of an object path (a list index or a map key).
	paths [][]string
	// secretNames are top-level field names of a machine config kind, each
	// hiding the field and everything under it.
	secretNames []string
}

// Redaction returns the redaction of the registered row with this kind's
// Key: the row's RedactPaths, the Secret payload paths of a Sensitive row and
// the secret fields of a machine config kind. A row with nothing to hide
// still returns a value that is not the zero value, carrying the row's GVR.
// A kind that is not a registered row (a literal, or a copy of a row with
// another Key or GVR) returns the zero value, which the preview planners
// refuse; a copy of a row with the same Key and GVR returns the registered
// row's redaction, never one built from the copy's own fields.
func (k Kind) Redaction() Redaction {
	row, ok := ByKey(k.Key)
	if !ok || row.GVR != k.GVR {
		return Redaction{}
	}
	r := Redaction{
		gvr:        row.GVR,
		built:      true,
		secretData: row.Sensitive,
		paths:      slices.Clone(row.RedactPaths),
	}
	if row.GVR.Group == MachineConfigGroup {
		if f, ok := MachineConfigFieldsFor(row.Name); ok {
			r.secretNames = slices.Clone(f.Secret)
		}
	}
	return r
}

// secretsGVR is the resource of the core v1 Secret row.
var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// ReadRedaction returns the redaction a read of the object uses: what get
// shows of an object a caller asked the server for. It hides the same
// values as Redaction, except that a Secret keeps its top-level data and
// stringData, because a secret's data is what get secret is for and kubectl
// shows the same. The last-applied annotation stays hidden for a Secret too.
// A kind that is not a registered row returns the zero value, as Redaction
// does. A read redaction reports IsRead and no preview or apply result is
// built from one.
func (k Kind) ReadRedaction() Redaction {
	r := k.Redaction()
	if r.IsZero() {
		return r
	}
	r.read = true
	r.keepSecretData = r.gvr == secretsGVR
	return r
}

// RowlessRedaction builds the redaction of a create whose resource has no
// kind row: the resource, the key sequences of its secret fields and the
// reason the resource has no row. An empty reason returns the zero value.
func RowlessRedaction(gvr schema.GroupVersionResource, reason string, paths ...[]string) Redaction {
	if reason == "" {
		return Redaction{}
	}
	return Redaction{gvr: gvr, built: true, paths: slices.Clone(paths)}
}

// IsRead reports whether r was built by Kind.ReadRedaction.
func (r Redaction) IsRead() bool { return r.read }

// IsZero reports whether r is the zero value, which no constructor returns.
func (r Redaction) IsZero() bool { return !r.built }

// GVR is the resource the redaction was built for.
func (r Redaction) GVR() schema.GroupVersionResource { return r.gvr }

// Hides reports whether the redaction hides any path.
func (r Redaction) Hides() bool {
	return r.secretData || len(r.paths) > 0 || len(r.secretNames) > 0
}

// Covers reports whether the value at the object path keys is hidden: the
// path is a hidden path or lies under one. Names are compared ignoring case.
// A "*" segment of a hidden path is a wildcard: it matches any one segment
// of keys, such as a list index "0", the "-" of a JSON patch append, or a
// map key. A "*" in keys is an ordinary name: it matches only a "*" segment
// of the hidden path. The last-applied annotation is hidden for every kind
// whose redaction hides anything, because it holds a copy of the applied
// object.
func (r Redaction) Covers(keys []string) bool {
	if len(keys) == 0 || !r.Hides() {
		return false
	}
	if r.secretData && !r.keepSecretData && (strings.EqualFold(keys[0], "data") || strings.EqualFold(keys[0], "stringData")) {
		return true
	}
	for _, p := range r.paths {
		if hasFoldedPrefix(keys, p) {
			return true
		}
	}
	for _, name := range r.secretNames {
		if strings.EqualFold(keys[0], name) {
			return true
		}
	}
	return hasFoldedPrefix(keys, lastAppliedPath)
}

// lastAppliedPath is the path of the last-applied annotation.
var lastAppliedPath = []string{"metadata", "annotations", LastAppliedAnnotation}

// wildcardSegment is the segment of a hidden path that matches any one
// segment of an object path: a list index or a map key.
const wildcardSegment = "*"

// hasFoldedPrefix reports whether keys equals prefix or lies under it, each
// segment compared ignoring case. A "*" segment of prefix matches exactly one
// segment of keys, whatever it is; a "*" in keys is compared as a name. An
// empty prefix matches nothing.
func hasFoldedPrefix(keys, prefix []string) bool {
	if len(prefix) == 0 || len(keys) < len(prefix) {
		return false
	}
	return slices.EqualFunc(keys[:len(prefix)], prefix, func(key, segment string) bool {
		return segment == wildcardSegment || strings.EqualFold(key, segment)
	})
}

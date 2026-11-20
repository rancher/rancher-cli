// Package cloudcredfields holds the pure rules for the fields of a cloud
// credential: which fields a driver takes, which of them are secret, which
// hold several lines, the label the Rancher UI uses for a driver, and the
// white space rules for a value. The create command and its completion both
// read it, so the completion offers only what the command accepts. It imports
// the standard library only: it reads no server, file or environment.
package cloudcredfields

import (
	"slices"
	"sort"
	"strings"
)

// Field is one field of a driver's credential config.
type Field struct {
	Name string
	// Secret marks a field Rancher types as a password: a value of it comes
	// from a file or stdin, never from an argument.
	Secret bool
	// Required marks a field Rancher refuses a credential without. A field with
	// a default is not required.
	Required bool
	// Multiline marks a field whose value holds several lines: a PEM, a JSON
	// key file or a kubeconfig. It may be given from a file even when it is
	// public.
	Multiline bool
}

// Driver is the credential config of one driver: its prefix, the name Rancher
// gives its <prefix>credentialConfig object, and its fields sorted by name.
type Driver struct {
	Prefix string
	Fields []Field
}

// Field returns the field with exactly this name.
func (d Driver) Field(name string) (Field, bool) {
	for _, f := range d.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// SecretFolded returns the secret field whose name equals name without regard
// to case, for a refusal that must not let a different spelling of a secret
// field through as a public one.
func (d Driver) SecretFolded(name string) (Field, bool) {
	for _, f := range d.Fields {
		if f.Secret && strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return Field{}, false
}

// Names returns the names of the fields for which keep is true, sorted.
func (d Driver) Names(keep func(Field) bool) []string {
	var out []string
	for _, f := range d.Fields {
		if keep(f) {
			out = append(out, f.Name)
		}
	}
	return out
}

// sshKeyFields are the fields Rancher always types as a password, whether or
// not a driver lists them as private.
var sshKeyFields = []string{"sshKeyContents", "sshKey", "privateKeyFile"}

// multilineFields are the fields whose value holds several lines.
var multilineFields = []string{"authEncodedJson", "kubeconfigContent", "privateKeyContents", "sshKeyContents", "defaultEndpointCA"}

// IsMultiline reports whether a field of this name holds several lines.
func IsMultiline(name string) bool { return slices.Contains(multilineFields, name) }

// Annotations are the NodeDriver annotations that name the credential fields
// of a driver, each a comma-separated list; Defaults is a comma-separated list
// of field:value pairs.
type Annotations struct {
	Public, Private, Password, Optional, Defaults string
}

// Annotation keys of a NodeDriver.
const (
	AnnotationPublic   = "publicCredentialFields"
	AnnotationPrivate  = "privateCredentialFields"
	AnnotationPassword = "passwordFields"
	AnnotationOptional = "optionalCredentialFields"
	AnnotationDefaults = "defaults"
)

// AnnotationsOf reads the credential annotations out of a NodeDriver's
// annotations.
func AnnotationsOf(annotations map[string]string) Annotations {
	return Annotations{
		Public: annotations[AnnotationPublic], Private: annotations[AnnotationPrivate],
		Password: annotations[AnnotationPassword], Optional: annotations[AnnotationOptional],
		Defaults: annotations[AnnotationDefaults],
	}
}

func set(list string) map[string]bool {
	out := map[string]bool{}
	for item := range strings.SplitSeq(list, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out[item] = true
		}
	}
	return out
}

// FromAnnotations builds the credential config of the node driver whose
// spec.displayName is prefix, as Rancher builds its schema: the fields are the
// public and the private credential fields; a field is secret when it is
// private, named in passwordFields, or one of Rancher's SSH key fields; it is
// required unless it is optional or has a default. The amazonec2 config also
// takes the optional defaultRegion. It reports false when the annotations name
// no credential field.
func FromAnnotations(prefix string, a Annotations) (Driver, bool) {
	public, private, password, optional := set(a.Public), set(a.Private), set(a.Password), set(a.Optional)
	defaults := map[string]bool{}
	for pair := range strings.SplitSeq(a.Defaults, ",") {
		if key, _, ok := strings.Cut(pair, ":"); ok {
			if key = strings.TrimSpace(key); key != "" {
				defaults[key] = true
			}
		}
	}
	names := map[string]bool{}
	for name := range public {
		names[name] = true
	}
	for name := range private {
		names[name] = true
	}
	d := Driver{Prefix: prefix}
	for name := range names {
		secret := private[name] || password[name] || slices.Contains(sshKeyFields, name)
		d.Fields = append(d.Fields, Field{Name: name, Secret: secret, Required: !optional[name] && !defaults[name], Multiline: IsMultiline(name)})
	}
	if prefix == "amazonec2" {
		if _, ok := d.Field("defaultRegion"); !ok {
			d.Fields = append(d.Fields, Field{Name: "defaultRegion"})
		}
	}
	if len(d.Fields) == 0 {
		return Driver{}, false
	}
	sort.Slice(d.Fields, func(i, j int) bool { return d.Fields[i].Name < d.Fields[j].Name })
	return d, true
}

// Static returns the credential config of a driver that has no node driver:
// s3 and alibaba.
func Static(prefix string) (Driver, bool) {
	switch prefix {
	case "s3":
		return Driver{Prefix: "s3", Fields: []Field{
			{Name: "accessKey", Required: true},
			{Name: "defaultBucket"},
			{Name: "defaultEndpoint"},
			{Name: "defaultEndpointCA", Multiline: true},
			{Name: "defaultFolder"},
			{Name: "defaultRegion"},
			{Name: "defaultSkipSSLVerify"},
			{Name: "secretKey", Secret: true, Required: true},
		}}, true
	case "alibaba":
		return Driver{Prefix: "alibaba", Fields: []Field{
			{Name: "accessKeyId"},
			{Name: "accessKeySecret", Secret: true},
		}}, true
	}
	return Driver{}, false
}

// IsStatic reports whether prefix names a driver with no node driver.
func IsStatic(prefix string) bool {
	_, ok := Static(prefix)
	return ok
}

// StaticPrefixes are the drivers that have no node driver.
func StaticPrefixes() []string { return []string{"alibaba", "s3"} }

// Label is the value of the annotation provisioning.cattle.io/driver for a
// driver: the label the Rancher UI gives it, which its cluster forms match.
func Label(prefix string) string {
	switch prefix {
	case "amazonec2":
		return "aws"
	case "google":
		return "gcp"
	case "oci":
		return "oracle"
	}
	return strings.ToLower(prefix)
}

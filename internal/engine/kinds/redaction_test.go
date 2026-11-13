package kinds

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestRedactionZeroValueIsDetectable asserts only a registered row and the
// row-less constructor give a redaction that is not the zero value, so a
// planner call that forgets its redaction is detectable, even for a row that
// hides nothing.
func TestRedactionZeroValueIsDetectable(t *testing.T) {
	t.Parallel()
	assert.True(t, Redaction{}.IsZero())
	row, ok := Lookup("namespace")
	require.True(t, ok)
	r := row.Redaction()
	assert.False(t, r.IsZero(), "a row with nothing to hide is not the zero value")
	assert.False(t, r.Hides())
	assert.Equal(t, row.GVR, r.GVR())
	assert.False(t, r.Covers([]string{"data", "k"}))
	for _, k := range All() {
		assert.False(t, k.Redaction().IsZero(), k.Name)
		assert.Equal(t, k.GVR, k.Redaction().GVR(), k.Name)
	}
}

// TestRedactionComesOnlyFromARegisteredRow asserts a kind that is not a
// registered row gives the zero value, a copy of a row with the same Key and
// GVR gives exactly the registered row's redaction whatever its own fields
// say, and a registered row gives its own.
func TestRedactionComesOnlyFromARegisteredRow(t *testing.T) {
	t.Parallel()
	secret, ok := Lookup("secret")
	require.True(t, ok)
	user, ok := Lookup("user")
	require.True(t, ok)

	assert.True(t, Kind{}.Redaction().IsZero(), "an empty literal")
	assert.True(t, Kind{GVR: secret.GVR, Sensitive: true}.Redaction().IsZero(), "a literal with a row's GVR and no Key")
	assert.True(t, Kind{Key: "nosuchkind", GVR: secret.GVR, Sensitive: true}.Redaction().IsZero(), "an unregistered Key")

	otherKey := secret
	otherKey.Key = "nosuchkind"
	assert.True(t, otherKey.Redaction().IsZero(), "a copy of a row with another Key")

	otherGVR := secret
	otherGVR.GVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	assert.True(t, otherGVR.Redaction().IsZero(), "a copy of a row with another GVR")

	weaker := secret
	weaker.Sensitive = false
	weaker.RedactPaths = nil
	assert.Equal(t, secret.Redaction(), weaker.Redaction(), "a copy with the same Key and GVR gives the registered row's redaction")
	assert.True(t, weaker.Redaction().Covers([]string{"data", "k"}))

	stronger := user
	stronger.RedactPaths = append(slices.Clone(user.RedactPaths), []string{"extra"})
	assert.Equal(t, user.Redaction(), stronger.Redaction())
	assert.False(t, stronger.Redaction().Covers([]string{"extra"}))
	assert.True(t, user.Redaction().Covers([]string{"password"}), "a registered row gives its own")
}

// TestRowlessRedactionNeedsAReason asserts a row-less redaction names its
// resource and the reason the resource has no row; without the reason it is
// the zero value.
func TestRowlessRedactionNeedsAReason(t *testing.T) {
	t.Parallel()
	gvr := schema.GroupVersionResource{Group: "ext.cattle.io", Resource: "passwordchangerequests"}
	assert.True(t, RowlessRedaction(gvr, "", []string{"spec", "newPassword"}).IsZero())
	r := RowlessRedaction(gvr, "create-only", []string{"spec", "newPassword"})
	require.False(t, r.IsZero())
	assert.Equal(t, gvr, r.GVR())
	assert.True(t, r.Hides())
	assert.True(t, r.Covers([]string{"spec", "newPassword"}))
	assert.True(t, r.Covers([]string{"spec", "newPassword", "x"}))
	assert.False(t, r.Covers([]string{"spec", "userID"}))
	assert.False(t, r.Covers([]string{"spec"}))
}

// TestRedactionCovers asserts the path rules: the Secret payload of a
// Sensitive row, a RedactPaths path and everything under it, the last-applied
// and objectset applied annotations of any row that hides something, and a
// machine config secret name, each name compared with its case folded.
func TestRedactionCovers(t *testing.T) {
	t.Parallel()
	secret, _ := Lookup("secret")
	user, _ := Lookup("user")
	namespace, _ := Lookup("namespace")
	aws, _ := Lookup("amazonec2config")
	bundle, _ := Lookup("bundle")
	lastApplied := []string{"metadata", "annotations", LastAppliedAnnotation}
	applied := []string{"metadata", "annotations", "objectset.rio.cattle.io/applied"}
	for _, tc := range []struct {
		name string
		k    Kind
		keys []string
		want bool
	}{
		{"secret data", secret, []string{"data"}, true},
		{"secret data key", secret, []string{"data", "k"}, true},
		{"secret stringData key", secret, []string{"stringData", "k", "deep"}, true},
		{"secret type", secret, []string{"type"}, false},
		{"secret data prefix", secret, []string{"dataExtra"}, false},
		{"secret last applied", secret, lastApplied, true},
		{"secret other annotation", secret, []string{"metadata", "annotations", "owner"}, false},
		{"secret annotations map", secret, []string{"metadata", "annotations"}, false},
		{"secret root", secret, nil, false},
		{"user password", user, []string{"password"}, true},
		{"user password below", user, []string{"password", "x"}, true},
		{"user passwordHint", user, []string{"passwordHint"}, false},
		{"user data", user, []string{"data"}, true},
		{"user last applied", user, lastApplied, true},
		{"namespace data", namespace, []string{"data"}, false},
		{"namespace last applied", namespace, lastApplied, false},
		{"aws secretKey", aws, []string{"secretKey"}, true},
		{"aws SECRETKEY", aws, []string{"SECRETKEY"}, true},
		{"aws sessionToken below", aws, []string{"sessionToken", "k"}, true},
		{"aws sessiontoken", aws, []string{"sessiontoken"}, true},
		{"aws accessKey", aws, []string{"accessKey"}, false},
		{"aws region", aws, []string{"region"}, false},
		{"aws nested secretKey", aws, []string{"tags", "secretKey"}, false},
		{"aws last applied", aws, lastApplied, true},
		{"aws data", aws, []string{"data"}, false},
		{"secret StringData", secret, []string{"StringData", "k"}, true},
		{"secret DATA", secret, []string{"DATA"}, true},
		{"secret Data below", secret, []string{"Data", "k", "deep"}, true},
		{"secret last applied cased", secret, []string{"Metadata", "Annotations", "Kubectl.Kubernetes.io/Last-Applied-Configuration"}, true},
		{"secret last applied below", secret, append(slices.Clone(lastApplied), "x"), true},
		{"user Password", user, []string{"Password"}, true},
		{"user PASSWORD below", user, []string{"PASSWORD", "x"}, true},
		{"user PasswordHint", user, []string{"PasswordHint"}, false},
		{"namespace applied", namespace, applied, false},
	} {
		assert.Equal(t, tc.want, tc.k.Redaction().Covers(tc.keys), tc.name)
	}
	for _, k := range []Kind{secret, user, aws, bundle} {
		assert.True(t, k.Redaction().Covers(applied), k.Name+" applied")
		assert.True(t, k.Redaction().Covers([]string{"Metadata", "Annotations", "Objectset.Rio.Cattle.Io/Applied"}), k.Name+" applied cased")
		assert.True(t, k.Redaction().Covers(append(slices.Clone(applied), "x")), k.Name+" applied below")
		for _, name := range []string{
			"objectset.rio.cattle.io/id",
			"objectset.rio.cattle.io/owner-gvk",
			"objectset.rio.cattle.io/owner-name",
			"objectset.rio.cattle.io/owner-namespace",
			"objectset.rio.cattle.io/hash",
			"objectset.rio.cattle.io/applied-by",
		} {
			assert.False(t, k.Redaction().Covers([]string{"metadata", "annotations", name}), k.Name+" "+name)
		}
	}
}

// TestEveryRowHidesTheHiddenAnnotationsWhenItHidesAnything asserts, for every
// row and every annotation that holds a copy of the applied object, that a
// preview redaction and a read redaction cover the annotation exactly when the
// row hides anything, and that the list of those annotations is the two known
// names, without duplicates, and is returned as a copy.
func TestEveryRowHidesTheHiddenAnnotationsWhenItHidesAnything(t *testing.T) {
	t.Parallel()
	names := HiddenAnnotations()
	assert.Equal(t, []string{"kubectl.kubernetes.io/last-applied-configuration", "objectset.rio.cattle.io/applied"}, names)
	folded := map[string]bool{}
	for _, name := range names {
		assert.False(t, folded[strings.ToLower(name)], "%s listed twice", name)
		folded[strings.ToLower(name)] = true
	}
	names[0] = "changed"
	assert.Equal(t, LastAppliedAnnotation, HiddenAnnotations()[0], "the returned slice is a copy")

	require.NotEmpty(t, All())
	for _, k := range All() {
		want := k.Redaction().Hides()
		for _, name := range HiddenAnnotations() {
			path := []string{"metadata", "annotations", name}
			cased := []string{"METADATA", "Annotations", strings.ToUpper(name)}
			assert.Equal(t, want, k.Redaction().Covers(path), "%s preview %s", k.Name, name)
			assert.Equal(t, want, k.ReadRedaction().Covers(path), "%s read %s", k.Name, name)
			assert.Equal(t, want, k.Redaction().Covers(cased), "%s preview cased %s", k.Name, name)
			assert.Equal(t, want, k.ReadRedaction().Covers(cased), "%s read cased %s", k.Name, name)
		}
	}
}

// TestRedactionCoversAWildcardSegment asserts a "*" segment of a hidden path
// matches exactly one segment of the object path, whatever it is (an index,
// the "-" of an append, any name), with the other segments compared ignoring
// case, and that a "*" in the object path is compared as a name.
func TestRedactionCoversAWildcardSegment(t *testing.T) {
	t.Parallel()
	helmop, _ := Lookup("helmop")
	bundledeployment, _ := Lookup("bundledeployment")
	for _, tc := range []struct {
		name string
		k    Kind
		keys []string
		want bool
	}{
		{"index 0", helmop, []string{"spec", "targets", "0", "helm", "values"}, true},
		{"index 7", helmop, []string{"spec", "targets", "7", "helm", "templateValues"}, true},
		{"append", helmop, []string{"spec", "targets", "-", "helm", "values"}, true},
		{"folded case", helmop, []string{"Spec", "TARGETS", "0", "Helm", "VALUES"}, true},
		{"a key below values", helmop, []string{"spec", "targets", "0", "helm", "values", "password"}, true},
		{"literal * where the hidden path has *", helmop, []string{"spec", "targets", "*", "helm", "values"}, true},
		{"shorter path", helmop, []string{"spec", "targets", "0", "helm"}, false},
		{"the targets list itself", helmop, []string{"spec", "targets"}, false},
		{"valuesFrom is not values", helmop, []string{"spec", "targets", "0", "helm", "valuesFrom"}, false},
		{"two segments for one wildcard", helmop, []string{"spec", "targets", "0", "1", "helm", "values"}, false},
		{"no segment for the wildcard", helmop, []string{"spec", "targets", "helm", "values", "x"}, false},
		{"literal * where the hidden path has a name", helmop, []string{"spec", "*", "values"}, false},
		{"top-level values", helmop, []string{"spec", "helm", "values"}, true},
		{"options values", bundledeployment, []string{"spec", "options", "helm", "values"}, true},
		{"a key below staged templateValues", bundledeployment, []string{"spec", "stagedOptions", "helm", "templateValues", "k"}, true},
		{"options valuesFrom is not options values", bundledeployment, []string{"spec", "options", "helm", "valuesFrom"}, false},
		{"literal * in place of options", bundledeployment, []string{"spec", "*", "helm", "values"}, false},
	} {
		assert.Equal(t, tc.want, tc.k.Redaction().Covers(tc.keys), tc.name)
	}
}

// TestExtTokenAndKubeconfigHideTheirValue asserts the two ext rows hide the
// value the server returns once on create: a token's status.bearerToken and
// status.value, and a kubeconfig's status.value, in any letter case.
func TestExtTokenAndKubeconfigHideTheirValue(t *testing.T) {
	t.Parallel()
	token, _ := Lookup("token")
	kubeconfig, _ := Lookup("kubeconfig")
	for _, tc := range []struct {
		name string
		k    Kind
		keys []string
		want bool
	}{
		{"token bearerToken", token, []string{"status", "bearerToken"}, true},
		{"token BEARERTOKEN", token, []string{"STATUS", "BEARERTOKEN"}, true},
		{"token value", token, []string{"status", "value"}, true},
		{"token expiresAt", token, []string{"status", "expiresAt"}, false},
		{"token description", token, []string{"spec", "description"}, false},
		{"kubeconfig value", kubeconfig, []string{"status", "value"}, true},
		{"kubeconfig Value", kubeconfig, []string{"Status", "Value"}, true},
		{"kubeconfig summary", kubeconfig, []string{"status", "summary"}, false},
	} {
		assert.Equal(t, tc.want, tc.k.Redaction().Covers(tc.keys), tc.name)
	}
}

// TestMachineConfigSecretListsAreComplete asserts every machine config kind
// carries a secret list, with each name once whatever its case, and that the
// names the drivers publish beyond Rancher's password typing are listed.
func TestMachineConfigSecretListsAreComplete(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"digitaloceanconfig":  {"accessToken", "sshKeyContents"},
		"amazonec2config":     {"secretKey", "sessionToken", "sshKeyContents"},
		"azureconfig":         {"clientSecret"},
		"harvesterconfig":     {"kubeconfigContent", "sshPassword"},
		"vmwarevsphereconfig": {"password", "sshPassword"},
	}
	seen := 0
	for _, k := range All() {
		if k.GVR.Group != MachineConfigGroup {
			continue
		}
		seen++
		f, ok := MachineConfigFieldsFor(k.Name)
		require.True(t, ok, k.Name)
		assert.ElementsMatch(t, want[k.Name], f.Secret, k.Name)
		folded := map[string]bool{}
		for _, name := range f.Secret {
			assert.False(t, folded[strings.ToLower(name)], "%s lists %s twice", k.Name, name)
			folded[strings.ToLower(name)] = true
		}
	}
	assert.Equal(t, len(want), seen)
}

// TestReadRedactionKeepsOnlyASecretPayload asserts the read redaction of the
// secret row leaves data and stringData in any letter case visible and
// still covers the last-applied and objectset applied annotations, so it
// still hides something, while the redaction of the same row for a preview
// covers all four.
func TestReadRedactionKeepsOnlyASecretPayload(t *testing.T) {
	t.Parallel()
	secret, ok := Lookup("secret")
	require.True(t, ok)
	read := secret.ReadRedaction()
	assert.True(t, read.Hides(), "the read redaction of a secret still hides the annotation")
	for _, keys := range [][]string{
		{"data"}, {"data", "k"}, {"stringData"}, {"stringData", "k", "deep"},
		{"DATA"}, {"StringData", "k"}, {"STRINGDATA"},
	} {
		assert.False(t, read.Covers(keys), "read of %v", keys)
		assert.True(t, secret.Redaction().Covers(keys), "preview of %v", keys)
	}
	lastApplied := []string{"metadata", "annotations", LastAppliedAnnotation}
	assert.True(t, read.Covers(lastApplied))
	assert.True(t, read.Covers([]string{"Metadata", "Annotations", "Kubectl.Kubernetes.io/Last-Applied-Configuration"}))
	assert.True(t, read.Covers([]string{"metadata", "annotations", "objectset.rio.cattle.io/applied"}))
	assert.True(t, read.Covers([]string{"Metadata", "Annotations", "Objectset.Rio.Cattle.Io/Applied"}))
	assert.False(t, read.Covers([]string{"metadata", "annotations", "owner"}))
	assert.False(t, read.Covers([]string{"type"}))
	assert.Equal(t, secret.GVR, read.GVR())
}

// TestReadRedactionEqualsTheRedactionExceptForASecret asserts every row but
// secret reads with the redaction it previews with, apart from the mark
// that says it is a read, and that the secret row's redaction for a preview
// and an apply result still covers data and stringData.
func TestReadRedactionEqualsTheRedactionExceptForASecret(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		read, preview := k.ReadRedaction(), k.Redaction()
		assert.True(t, read.IsRead(), k.Name)
		assert.False(t, preview.IsRead(), k.Name)
		assert.Equal(t, preview.GVR(), read.GVR(), k.Name)
		assert.Equal(t, preview.Hides(), read.Hides(), k.Name)
		if k.Key == "secret" {
			continue
		}
		read.read = false
		assert.Equal(t, preview, read, k.Name)
	}
	secret, ok := Lookup("secret")
	require.True(t, ok)
	assert.True(t, secret.Redaction().Covers([]string{"data"}))
	assert.True(t, secret.Redaction().Covers([]string{"stringData", "k"}))
}

// TestReadRedactionHidesPayloadOfEverySensitiveRowButSecret asserts a Sensitive row other
// than secret hides data and stringData on a read.
func TestReadRedactionHidesPayloadOfEverySensitiveRowButSecret(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		if !k.Sensitive || k.Key == "secret" {
			continue
		}
		read := k.ReadRedaction()
		assert.True(t, read.Covers([]string{"data", "k"}), k.Name)
		assert.True(t, read.Covers([]string{"StringData"}), k.Name)
	}
}

// TestReadRedactionOfAKindThatIsNotARowIsTheZeroValue asserts a literal kind
// and a copy of a row with another Key or GVR read with the zero value, as
// they preview with it.
func TestReadRedactionOfAKindThatIsNotARowIsTheZeroValue(t *testing.T) {
	t.Parallel()
	secret, ok := Lookup("secret")
	require.True(t, ok)
	assert.True(t, Kind{}.ReadRedaction().IsZero())
	assert.False(t, Kind{}.ReadRedaction().IsRead())
	assert.True(t, Kind{Key: "secret", Sensitive: true}.ReadRedaction().IsZero(), "a literal with a row's Key and no GVR")
	otherKey := secret
	otherKey.Key = "nosuchkind"
	assert.True(t, otherKey.ReadRedaction().IsZero())
}

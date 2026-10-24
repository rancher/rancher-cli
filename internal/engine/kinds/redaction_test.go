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
// annotation of any row that hides something, and a machine config secret
// name, each name compared with its case folded.
func TestRedactionCovers(t *testing.T) {
	t.Parallel()
	secret, _ := Lookup("secret")
	user, _ := Lookup("user")
	namespace, _ := Lookup("namespace")
	aws, _ := Lookup("amazonec2config")
	lastApplied := []string{"metadata", "annotations", LastAppliedAnnotation}
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

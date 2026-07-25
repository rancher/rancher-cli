package provisioning

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// mcInput is a full runtime.Input fake covering all five axes — create
// machine-config is the first command that consumes Strings and IsSet.
type mcInput struct {
	s  map[string]string
	b  map[string]bool
	sl []string        // the --set occurrences, in order
	is map[string]bool // explicit flag presence (MC-5a)
	a  []string
}

func (i mcInput) String(k string) string { return i.s[k] }
func (i mcInput) Bool(k string) bool     { return i.b[k] }
func (i mcInput) Strings(k string) []string {
	if k == "set" {
		return i.sl
	}
	return nil
}
func (i mcInput) IsSet(k string) bool { return i.is[k] }
func (i mcInput) Args() []string      { return i.a }

var _ runtime.Input = mcInput{}

func buildMachineConfig(t *testing.T, in runtime.Input) (*unstructured.Unstructured, error) {
	t.Helper()
	cmd := NewCreateMachineConfig(fakeFactory{}).(*createMachineConfigCmd)
	obj, _, err := cmd.build(in)
	return obj, err
}

// TestCreateMachineConfigDescribe pins the MC-1/MC-2/MC-9 surface shape.
func TestCreateMachineConfigDescribe(t *testing.T) {
	t.Parallel()
	d := NewCreateMachineConfig(fakeFactory{}).Describe()
	assert.Equal(t, "create machine-config", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
	assert.Equal(t, "false", d.Destructive)
	assert.True(t, d.Network)
	assert.True(t, d.Auth)

	require.Len(t, d.Args, 2)
	assert.Equal(t, "kind", d.Args[0].Name)
	assert.True(t, d.Args[0].Required)
	// LITERAL enum (independent-source doctrine): a kind-table edit must
	// trip this, not be silently restated.
	assert.Equal(t, []string{
		"amazonec2config", "azureconfig", "digitaloceanconfig",
		"harvesterconfig", "vmwarevsphereconfig",
	}, d.Args[0].Enum)
	assert.Equal(t, "name", d.Args[1].Name)
	assert.True(t, d.Args[1].Required)
	assert.Empty(t, d.Args[1].Enum)

	var setFlag *runtime.ArgSpec
	for i := range d.Flags {
		if d.Flags[i].Name == "set" {
			setFlag = &d.Flags[i]
		}
	}
	require.NotNil(t, setFlag)
	assert.Equal(t, "stringSlice", setFlag.Type)
	// MC-9: the desc names the two-step flow and the --set-only kinds.
	assert.Contains(t, d.Desc, "create cluster --machine-config")
	assert.Contains(t, d.Desc, "--set")
}

// TestCreateMachineConfigBuildRootLevelFields pins MC-6/MC-10: driver
// fields sit at the CR ROOT alongside apiVersion/kind/metadata — no spec
// wrapper — in fleet-default, with the kind row's K8sKind casing.
func TestCreateMachineConfigBuildRootLevelFields(t *testing.T) {
	t.Parallel()
	obj, err := buildMachineConfig(t, mcInput{
		a:  []string{"digitaloceanconfig", "web-pool"},
		s:  map[string]string{"region": "nyc3", "size": "s-2vcpu-4gb", "tags": "web,prod"},
		is: map[string]bool{"region": true, "size": true, "tags": true},
	})
	require.NoError(t, err)
	assert.Equal(t, "rke-machine-config.cattle.io/v1", obj.GetAPIVersion())
	assert.Equal(t, "DigitaloceanConfig", obj.GetKind())
	assert.Equal(t, "web-pool", obj.GetName())
	assert.Equal(t, "fleet-default", obj.GetNamespace())
	assert.Equal(t, "nyc3", obj.Object["region"])
	assert.Equal(t, "s-2vcpu-4gb", obj.Object["size"])
	assert.Equal(t, "web,prod", obj.Object["tags"], "tags is a comma-separated string field")
	_, hasSpec := obj.Object["spec"]
	assert.False(t, hasSpec, "machine-config CRs have NO spec wrapper")
}

// TestCreateMachineConfigBoolTriState pins MC-5a for every curated bool:
// omitted → field absent; explicit true → true; explicit false → false.
func TestCreateMachineConfigBoolTriState(t *testing.T) {
	t.Parallel()
	bools := map[string]string{ // flag → field
		"monitoring":         "monitoring",
		"ipv6":               "ipv6",
		"private-networking": "privateNetworking",
	}
	for flag, field := range bools {
		t.Run(flag+" omitted → absent", func(t *testing.T) {
			t.Parallel()
			obj, err := buildMachineConfig(t, mcInput{a: []string{"digitaloceanconfig", "x"}})
			require.NoError(t, err)
			_, present := obj.Object[field]
			assert.False(t, present)
		})
		t.Run(flag+" true", func(t *testing.T) {
			t.Parallel()
			obj, err := buildMachineConfig(t, mcInput{
				a:  []string{"digitaloceanconfig", "x"},
				b:  map[string]bool{flag: true},
				is: map[string]bool{flag: true},
			})
			require.NoError(t, err)
			assert.Equal(t, true, obj.Object[field])
		})
		t.Run(flag+" explicit false", func(t *testing.T) {
			t.Parallel()
			obj, err := buildMachineConfig(t, mcInput{
				a:  []string{"digitaloceanconfig", "x"},
				b:  map[string]bool{flag: false},
				is: map[string]bool{flag: true},
			})
			require.NoError(t, err)
			assert.Equal(t, false, obj.Object[field])
		})
	}
}

// TestCreateMachineConfigSetValueParsing pins MC-6's value grammar
// against Rancher's node-driver schema (FlagToField in rancher's
// pkg/controllers/management/drivers/nodedriver/utils.go): booleans stay
// JSON booleans (BoolFlag → "boolean"), arrays stay JSON arrays
// (StringSliceFlag → "array[string]"), null stays null (Nullable), but
// NUMBERS submit as their literal string — IntFlag fields are published
// as "string", so a JSON number fails the CRD type check. Anything else
// is the raw string; '=' allowed in the value; every occurrence lands
// (MC-6a).
func TestCreateMachineConfigSetValueParsing(t *testing.T) {
	t.Parallel()
	obj, err := buildMachineConfig(t, mcInput{
		a: []string{"amazonec2config", "x"},
		sl: []string{
			`instanceType=t3a.medium`,
			`rootSize=16`,
			`sshPort=22`,
			`spotPrice=0.50`,
			`encryptEbsVolume=true`,
			`securityGroup=["a","b"]`,
			`userdata=a=b`,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "t3a.medium", obj.Object["instanceType"])
	assert.Equal(t, "16", obj.Object["rootSize"], "numeric values submit as literal strings (Rancher IntFlag fields are string-typed)")
	assert.Equal(t, "22", obj.Object["sshPort"])
	assert.Equal(t, "0.50", obj.Object["spotPrice"], "the LITERAL string, not a float round-trip")
	assert.Equal(t, true, obj.Object["encryptEbsVolume"])
	assert.Equal(t, []any{"a", "b"}, obj.Object["securityGroup"])
	assert.Equal(t, "a=b", obj.Object["userdata"], "cut at the FIRST =")
}

// TestCreateMachineConfigSetMalformed pins MC-6's shape errors.
func TestCreateMachineConfigSetMalformed(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"no-equals", "=value"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, mcInput{
				a: []string{"amazonec2config", "x"}, sl: []string{entry},
			})
			ce := requireValidation(t, err)
			assert.Contains(t, ce.Message, entry)
		})
	}
}

// TestCreateMachineConfigSetEnvelopeKeysRejected pins MC-6's envelope
// guard: apiVersion, kind, metadata, and status via --set are VALIDATION
// before any overlay — they are object identity (or server-owned status),
// never driver fields, and must not overwrite what build assembles.
func TestCreateMachineConfigSetEnvelopeKeysRejected(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"apiVersion", "kind", "metadata", "status"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, mcInput{
				a: []string{"amazonec2config", "x"}, sl: []string{key + "=hijack"},
			})
			ce := requireValidation(t, err)
			assert.Contains(t, ce.Message, key)
		})
	}
}

// TestCreateMachineConfigSetDuplicateKey pins MC-6: duplicate keys across
// occurrences are VALIDATION, naming the key.
func TestCreateMachineConfigSetDuplicateKey(t *testing.T) {
	t.Parallel()
	_, err := buildMachineConfig(t, mcInput{
		a: []string{"amazonec2config", "x"}, sl: []string{"rootSize=16", "rootSize=32"},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "rootSize")
}

// TestCreateMachineConfigSetCuratedCollision pins MC-7: on the curated
// kind, a --set key naming a curated flag's field is VALIDATION with both
// spellings named — never silent precedence.
func TestCreateMachineConfigSetCuratedCollision(t *testing.T) {
	t.Parallel()
	_, err := buildMachineConfig(t, mcInput{
		a: []string{"digitaloceanconfig", "x"}, sl: []string{"region=nyc3"},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "region")
	assert.Contains(t, ce.Message, "--region")
}

// TestCreateMachineConfigCuratedFlagForeignKind pins MC-7a for one flag of
// each class — string, bool (incl. explicit-false presence), list: a DO
// curated flag on a non-DO kind is VALIDATION naming flag and kind, with a
// --set hint, and the field is never written.
func TestCreateMachineConfigCuratedFlagForeignKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   mcInput
		flag string
	}{
		{"string", mcInput{
			a: []string{"azureconfig", "x"},
			s: map[string]string{"region": "nyc3"}, is: map[string]bool{"region": true},
		}, "--region"},
		{"bool explicit false", mcInput{
			a: []string{"azureconfig", "x"},
			b: map[string]bool{"monitoring": false}, is: map[string]bool{"monitoring": true},
		}, "--monitoring"},
		{"list", mcInput{
			a: []string{"azureconfig", "x"},
			s: map[string]string{"tags": "a,b"}, is: map[string]bool{"tags": true},
		}, "--tags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, tc.in)
			ce := requireValidation(t, err)
			assert.Contains(t, ce.Message, tc.flag)
			assert.Contains(t, ce.Message, "digitaloceanconfig")
			assert.Contains(t, ce.Message, "azureconfig")
			assert.Contains(t, ce.Hint, "--set")
		})
	}
}

// TestCreateMachineConfigCredentialRejectedPerKind pins MC-8: at least one
// credential-carried key per kind, passed as a WELL-FORMED key=value
// assignment (so the credential check, not the shape check, is what
// fires), is VALIDATION naming the key with a cloud-credential hint —
// exact-match on the confirmed casing.
func TestCreateMachineConfigCredentialRejectedPerKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"digitaloceanconfig":  "accessToken",
		"amazonec2config":     "secretKey",
		"azureconfig":         "clientSecret",
		"harvesterconfig":     "kubeconfigContent",
		"vmwarevsphereconfig": "password",
	}
	for kind, key := range cases {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, mcInput{a: []string{kind, "x"}, sl: []string{key + "=dummy"}})
			ce := requireValidation(t, err)
			assert.Contains(t, ce.Message, key)
			assert.Contains(t, ce.Hint, "cloud credential")
		})
	}
}

// TestCreateMachineConfigArgValidation: missing kind/name and a
// non-machine-config kind are VALIDATION (build re-checks defensively;
// MC-2's Enum also guards at the runtime layer once registered).
func TestCreateMachineConfigArgValidation(t *testing.T) {
	t.Parallel()
	_, err := buildMachineConfig(t, mcInput{})
	_ = requireValidation(t, err)

	_, err = buildMachineConfig(t, mcInput{a: []string{"digitaloceanconfig"}})
	_ = requireValidation(t, err)

	_, err = buildMachineConfig(t, mcInput{a: []string{"pod", "x"}})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "pod")
}

// TestDoCuratedFlagsMatchKindsCurated guards against drift between the two
// sources that name the digitaloceanconfig curated field set: doCuratedFlags
// (the flag table used by build) and kinds.MachineConfigFieldsFor's Curated
// slice (used by completion and documentation). A field added to one side
// only causes a gap: completion suggests a key that validation rejects, or
// a flag is built that completion never surfaces. The test enforces SET
// equality — order is irrelevant, membership is not.
func TestDoCuratedFlagsMatchKindsCurated(t *testing.T) {
	t.Parallel()
	meta, ok := kinds.MachineConfigFieldsFor(doKind)
	require.True(t, ok, "digitaloceanconfig must have a MachineConfigFields entry")

	flagFields := make([]string, 0, len(doCuratedFlags))
	for _, cf := range doCuratedFlags {
		flagFields = append(flagFields, cf.field)
	}
	assert.ElementsMatch(t, meta.Curated, flagFields)
}

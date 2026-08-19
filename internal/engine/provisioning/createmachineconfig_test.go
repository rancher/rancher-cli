package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// mcRecClient records creates and can serve a configured dry-run result,
// standing in for a server that prunes unknown CRD fields (MC-12).
type mcRecClient struct {
	ccRecClient
	dryRunResult *unstructured.Unstructured
	execErr      error
}

func (r *mcRecClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	out, err := r.ccRecClient.Create(ctx, gvr, ns, obj, dryRun)
	if err != nil {
		return nil, err
	}
	if dryRun && r.dryRunResult != nil {
		return r.dryRunResult, nil
	}
	if !dryRun && r.execErr != nil {
		return nil, r.execErr
	}
	return out, nil
}

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

// TestCreateMachineConfigSetMalformed pins MC-6's shape errors — and that
// they NEVER reflect the entry's content: a bare pasted secret (no '=') or
// an empty-key assignment ('=secret') would otherwise echo the secret to
// stderr and into shell history/CI logs. The error names the offending
// occurrence by its 1-based index only.
func TestCreateMachineConfigSetMalformed(t *testing.T) {
	t.Parallel()
	const secret = "dop_v1_9f2c4e6a8b0d1f3a5c7e9b2d4f6a8c0e"
	cases := []struct {
		name    string
		sl      []string
		message string
	}{
		{"no equals", []string{secret}, "--set argument #1 must be <field>=<value>"},
		{"no equals second occurrence", []string{"region=us-east-1", secret}, "--set argument #2 must be <field>=<value>"},
		{"empty key", []string{"=" + secret}, "--set argument #1 has an empty field name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, mcInput{
				a: []string{"amazonec2config", "x"}, sl: tc.sl,
			})
			ce := requireValidation(t, err)
			assert.Equal(t, tc.message, ce.Message)
			assert.NotContains(t, ce.Message, secret, "the entry's content must never be reflected")
			assert.NotContains(t, ce.Hint, secret, "the entry's content must never be reflected")
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
// here in the canonical confirmed casing (case folding is pinned by
// TestCreateMachineConfigCredentialRejectedCaseInsensitive).
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

// TestCreateMachineConfigCredentialRejectedCaseInsensitive pins MC-8's
// case folding: a credential key in ANY casing (--set AccessKey=...,
// --set ACCESSTOKEN=...) must not slip past the rejection and persist a
// secret as a plaintext CR field. The VALIDATION message names the
// CANONICAL casing from the metadata so the user learns the real field
// name, with the cloud-credential hint.
func TestCreateMachineConfigCredentialRejectedCaseInsensitive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind      string
		key       string
		canonical string
	}{
		{"digitaloceanconfig", "AccessToken", "accessToken"},
		{"digitaloceanconfig", "ACCESSTOKEN", "accessToken"},
		{"vmwarevsphereconfig", "Password", "password"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+" "+tc.key, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, mcInput{a: []string{tc.kind, "x"}, sl: []string{tc.key + "=dummy"}})
			ce := requireValidation(t, err)
			assert.Contains(t, ce.Message, `"`+tc.canonical+`"`, "names the canonical casing")
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

// TestCreateMachineConfigPlanDiffLessEffect pins MC-11: one server-side
// dry-run, action=create, identity-only target, no diff, non-destructive,
// cluster local. All curated VALUE fields are set so the fallthrough warning
// does not fire; bool fields are not set (they can't go stale).
func TestCreateMachineConfigPlanDiffLessEffect(t *testing.T) {
	t.Parallel()
	client := &mcRecClient{}
	cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)

	eff, exec, err := cmd.Plan(t.Context(), mcInput{
		a:  []string{"digitaloceanconfig", "web-pool"},
		s:  map[string]string{"region": "nyc3", "size": "s-2vcpu-4gb", "image": "ubuntu-24-10-x64", "tags": "web"},
		is: map[string]bool{"region": true, "size": true, "image": true, "tags": true},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, []bool{true}, client.dryRuns)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "digitaloceanconfigs/web-pool", eff.Target)
	assert.Equal(t, "local", eff.Cluster)
	assert.Empty(t, eff.Diff)
	assert.False(t, eff.Destructive)
	assert.Empty(t, eff.Warnings, "no prune, all curated set → no warning")
	assert.Equal(t, "fleet-default", client.createdNS)
}

// TestCreateMachineConfigPrunedKeyWarning pins MC-12: a top-level key the
// dry-run response dropped (minus the k8s envelope) is warned about BY
// NAME; envelope keys never trigger it. Uses amazonec2config (no curated
// fields) so only the prune warning fires.
func TestCreateMachineConfigPrunedKeyWarning(t *testing.T) {
	t.Parallel()
	client := &mcRecClient{dryRunResult: &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke-machine-config.cattle.io/v1",
		"kind":       "Amazonec2Config",
		"metadata":   map[string]any{"name": "web-pool", "namespace": "fleet-default"},
		"region":     "us-east-1",
		"status":     map[string]any{},
	}}}
	cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)

	eff, _, err := cmd.Plan(t.Context(), mcInput{
		a:  []string{"amazonec2config", "web-pool"},
		sl: []string{"region=us-east-1", "regoin=typo"},
	})
	require.NoError(t, err)
	require.Len(t, eff.Warnings, 1)
	assert.Equal(t, "field(s) pruned by the server: regoin — unknown field?", eff.Warnings[0])
}

// TestCreateMachineConfigNullValuePruned pins the --set field=null edge:
// the value parses to JSON null, so the SUBMITTED object carries the key
// with a nil value — the key exists. When the server's dry-run response
// drops that key, the pruned-key comparison (a presence check, not a
// value check) must treat it like any other pruned key and warn BY NAME,
// so a null-valued submission never vanishes silently.
func TestCreateMachineConfigNullValuePruned(t *testing.T) {
	t.Parallel()
	client := &mcRecClient{dryRunResult: &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke-machine-config.cattle.io/v1",
		"kind":       "Amazonec2Config",
		"metadata":   map[string]any{"name": "web-pool", "namespace": "fleet-default"},
		"region":     "us-east-1",
	}}}
	cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)

	eff, _, err := cmd.Plan(t.Context(), mcInput{
		a:  []string{"amazonec2config", "web-pool"},
		sl: []string{"region=us-east-1", "keypairName=null"},
	})
	require.NoError(t, err)
	require.NotNil(t, client.created)
	v, submitted := client.created.Object["keypairName"]
	assert.True(t, submitted, "null value still submits the key")
	assert.Nil(t, v)
	require.Len(t, eff.Warnings, 1)
	assert.Equal(t, "field(s) pruned by the server: keypairName — unknown field?", eff.Warnings[0])
}

// TestCreateMachineConfigNullValueKept pins the other half of the null
// edge: when the server's dry-run response KEEPS the submitted-nil key,
// no warning fires — presence, not value, decides.
func TestCreateMachineConfigNullValueKept(t *testing.T) {
	t.Parallel()
	client := &mcRecClient{dryRunResult: &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":  "rke-machine-config.cattle.io/v1",
		"kind":        "Amazonec2Config",
		"metadata":    map[string]any{"name": "web-pool", "namespace": "fleet-default"},
		"region":      "us-east-1",
		"keypairName": nil,
	}}}
	cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)

	eff, _, err := cmd.Plan(t.Context(), mcInput{
		a:  []string{"amazonec2config", "web-pool"},
		sl: []string{"region=us-east-1", "keypairName=null"},
	})
	require.NoError(t, err)
	assert.Empty(t, eff.Warnings)
}

// TestCreateMachineConfigGateBehavior pins MC-1/MC-19 through the real
// pipeline: bare → CONFIRMATION_REQUIRED (nothing executed), --dry-run
// renders the Effect envelope (decoded, never string-matched) and stops,
// --apply executes the real create. No --yes: the effect is
// non-destructive.
func TestCreateMachineConfigGateBehavior(t *testing.T) {
	t.Parallel()
	binding := safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}
	in := mcInput{
		a:  []string{"digitaloceanconfig", "web-pool"},
		s:  map[string]string{"region": "nyc3"},
		is: map[string]bool{"region": true},
	}

	t.Run("bare → CONFIRMATION_REQUIRED", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client})
		var out, errOut bytes.Buffer
		_, ce := runtime.Run(t.Context(), cmd, in,
			runtime.IO{Out: &out, Err: &errOut}, binding)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
		assert.NotContains(t, ce.Hint, "--yes", "non-destructive: --apply alone gates it")
		assert.Equal(t, []bool{true}, client.dryRuns, "plan dry-ran; nothing executed")
	})

	t.Run("--dry-run renders the Effect and stops", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client})
		var out, errOut bytes.Buffer
		_, ce := runtime.Run(t.Context(), cmd, in,
			runtime.IO{Out: &out, Err: &errOut, Format: "json", DryRun: true}, binding)
		require.Nil(t, ce)
		var env struct {
			Data struct {
				Action string `json:"action"`
				Target string `json:"target"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &env))
		assert.Equal(t, "create", env.Data.Action)
		assert.Equal(t, "digitaloceanconfigs/web-pool", env.Data.Target)
		assert.Equal(t, []bool{true}, client.dryRuns)
	})

	t.Run("--apply executes", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client})
		var out, errOut bytes.Buffer
		_, ce := runtime.Run(t.Context(), cmd, in,
			runtime.IO{Out: &out, Err: &errOut, Format: "json", Apply: true}, binding)
		require.Nil(t, ce)
		assert.Equal(t, []bool{true, false}, client.dryRuns)
	})
}

// TestCreateMachineConfigExecutePropagatesError pins MC-13's engine-side
// contract: the executor surfaces the client error unchanged (the provider
// maps a name collision to the structured CONFLICT; the e2e scenario pins
// the mapped code end to end).
func TestCreateMachineConfigExecutePropagatesError(t *testing.T) {
	t.Parallel()
	client := &mcRecClient{execErr: clierr.New(clierr.CodeConflict, "digitaloceanconfigs \"web-pool\" already exists")}
	cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)

	_, exec, err := cmd.Plan(t.Context(), mcInput{
		a:  []string{"digitaloceanconfig", "web-pool"},
		s:  map[string]string{"region": "nyc3"},
		is: map[string]bool{"region": true},
	})
	require.NoError(t, err)
	_, err = exec(t.Context())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

// TestCreateMachineConfigFallthroughWarning pins the driver-default
// fallthrough warning: Plan appends a warning when any curated VALUE field
// is left unset, listing only those unset fields sorted. Bool fields
// (monitoring, ipv6, privateNetworking) are excluded — they can't go stale
// like value defaults. No warning when all value fields are set (bools may
// still be unset); no warning for non-curated kinds.
func TestCreateMachineConfigFallthroughWarning(t *testing.T) {
	t.Parallel()

	// allValueFieldsInput sets the four value fields but leaves bools unset.
	allValueFieldsInput := mcInput{
		a:  []string{"digitaloceanconfig", "web-pool"},
		s:  map[string]string{"region": "nyc3", "size": "s-2vcpu-4gb", "image": "ubuntu-24-10-x64", "tags": "web"},
		is: map[string]bool{"region": true, "size": true, "image": true, "tags": true},
	}

	t.Run("all-unset emits warning listing only value fields", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)
		eff, _, err := cmd.Plan(t.Context(), mcInput{
			a: []string{"digitaloceanconfig", "web-pool"},
		})
		require.NoError(t, err)
		require.Len(t, eff.Warnings, 1)
		assert.Equal(t,
			"unset field(s) inherit the node driver's declared defaults, which may be stale: "+
				"image, region, size, tags"+
				" — set them explicitly (a stale default, such as a retired image, makes provisioning fail)",
			eff.Warnings[0])
	})

	t.Run("some-set warning lists only the unset value fields", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)
		eff, _, err := cmd.Plan(t.Context(), mcInput{
			a:  []string{"digitaloceanconfig", "web-pool"},
			s:  map[string]string{"image": "ubuntu-24-10-x64", "region": "nyc3"},
			is: map[string]bool{"image": true, "region": true},
		})
		require.NoError(t, err)
		require.Len(t, eff.Warnings, 1)
		assert.Equal(t,
			"unset field(s) inherit the node driver's declared defaults, which may be stale: "+
				"size, tags"+
				" — set them explicitly (a stale default, such as a retired image, makes provisioning fail)",
			eff.Warnings[0])
	})

	t.Run("all-value-fields-set bools unset no warning", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)
		eff, _, err := cmd.Plan(t.Context(), allValueFieldsInput)
		require.NoError(t, err)
		assert.Empty(t, eff.Warnings, "bool fields unset must not trigger the warning")
	})

	t.Run("non-curated kind no warning", func(t *testing.T) {
		t.Parallel()
		client := &mcRecClient{}
		cmd := NewCreateMachineConfig(fakeFactory{c: client}).(*createMachineConfigCmd)
		eff, _, err := cmd.Plan(t.Context(), mcInput{
			a: []string{"amazonec2config", "aws-pool"},
		})
		require.NoError(t, err)
		assert.Empty(t, eff.Warnings)
	})
}

// TestCreateMachineConfigSetForeignKindHintArticle pins the article-aware
// hint: "an" before a vowel-initial kind name (azureconfig, amazonec2config),
// "a" before a consonant-initial kind name (harvesterconfig).
func TestCreateMachineConfigSetForeignKindHintArticle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind    string
		article string
	}{
		{"azureconfig", "an"},
		{"amazonec2config", "an"},
		{"harvesterconfig", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			_, err := buildMachineConfig(t, mcInput{
				a:  []string{tc.kind, "x"},
				s:  map[string]string{"region": "nyc3"},
				is: map[string]bool{"region": true},
			})
			ce := requireValidation(t, err)
			assert.Contains(t, ce.Hint, tc.article+" "+tc.kind, "hint article must match kind's initial vowel/consonant")
		})
	}
}

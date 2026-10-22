package resources

import (
	"testing"
	"testing/synctest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var waitFlagNames = []string{"wait", "for", "timeout"}

func flagNames(d runtime.Descriptor) []string {
	var names []string
	for _, f := range d.Flags {
		names = append(names, f.Name)
	}
	return names
}

// TestGetDeclaresTheWaitFlagsAndDeleteAndPatchDoNot asserts every kind's get
// declares --wait, --for and --timeout while its delete and patch declare
// none of them.
func TestGetDeclaresTheWaitFlagsAndDeleteAndPatchDoNot(t *testing.T) {
	t.Parallel()
	for _, k := range kinds.All() {
		got := flagNames(NewGet(k, nil).Describe())
		del := flagNames(NewDelete(k, nil).Describe())
		patch := flagNames(NewPatch(k, nil, nil).Describe())
		for _, name := range waitFlagNames {
			assert.Contains(t, got, name, "get %s", k.Name)
			assert.NotContains(t, del, name, "delete %s", k.Name)
			assert.NotContains(t, patch, name, "patch %s", k.Name)
		}
		for _, name := range del {
			assert.Contains(t, patch, name, "patch %s keeps delete's flag %s", k.Name, name)
		}
	}
}

// TestWaitFlagNamesAreWaitForTimeout asserts the list of wait flag names.
func TestWaitFlagNamesAreWaitForTimeout(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"wait", "for", "timeout"}, WaitFlagNames)
}

// TestGetFlagsAppendExactlyTheWaitFlags asserts the wait flags getFlags
// declares, in order, are the names in WaitFlagNames, and that
// WithoutWaitFlags removes exactly those.
func TestGetFlagsAppendExactlyTheWaitFlags(t *testing.T) {
	t.Parallel()
	for _, k := range kinds.All() {
		flags := getFlags(k)
		require.GreaterOrEqual(t, len(flags), 3, k.Name)
		last := len(flags) - 3
		assert.Equal(t, []string{"wait", "for", "timeout"}, flagNames(runtime.Descriptor{Flags: flags[last:]}), k.Name)
		assert.Equal(t, flagNames(runtime.Descriptor{Flags: flags[:last]}), flagNames(runtime.Descriptor{Flags: WithoutWaitFlags(getFlags(k))}), k.Name)
	}
}

// waitFlagRowInput is the input a get of k receives for one named object:
// the routing flags k's own descriptor declares (cluster and namespace),
// the given flags, and IsSet answering true for each string flag present.
func waitFlagRowInput(k kinds.Kind, flags map[string]string, bools map[string]bool) input {
	in := input{args: []string{"x"}, s: map[string]string{}, b: map[string]bool{}, isSet: map[string]bool{}}
	for _, f := range NewGet(k, nil).Describe().Flags {
		switch f.Name {
		case FlagCluster:
			in.s[FlagCluster] = "c-x"
		case FlagNamespace:
			in.s[FlagNamespace] = "ns"
		}
	}
	for name, v := range flags {
		in.s[name] = v
		in.isSet[name] = true
	}
	for name, v := range bools {
		in.b[name] = v
		in.isSet[name] = true
	}
	return in
}

// satisfiedAfterOnePoll is a client that answers an unsatisfied object, then
// one satisfying expr, so a command that waits reads twice and one that reads
// once does not.
func satisfiedAfterOnePoll(t *testing.T, expr string) *seqClient {
	t.Helper()
	cond, err := parseWaitCondition(expr)
	require.NoError(t, err)
	if cond.kind == "state" {
		return &seqClient{objs: []*unstructured.Unstructured{stateObj("pending", ""), stateObj(cond.name, "")}}
	}
	return &seqClient{objs: []*unstructured.Unstructured{conditionObj(cond.name, "False"), conditionObj(cond.name, "True")}}
}

// TestGetEveryKindStartsTheWaitFromForOrTimeoutAlone asserts, for every get
// kind, that --for alone waits on its expression, and that --timeout alone
// waits on the kind's default wait condition or, with none, refuses naming
// --for. The shipped clusterregistrationtoken get is a separate command; the
// row here covers the generic get built from its kind.
func TestGetEveryKindStartsTheWaitFromForOrTimeoutAlone(t *testing.T) {
	withFastPoll(t)
	for _, k := range kinds.All() {
		t.Run(k.Name+" for", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := satisfiedAfterOnePoll(t, "state=x")
				cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
				_, err := cmd.Execute(t.Context(), waitFlagRowInput(k, map[string]string{"for": "state=x"}, nil), nil)
				require.NoError(t, err)
				assert.Equal(t, 2, client.calls, "--for alone must poll to the state")
			})
		})
		t.Run(k.Name+" timeout", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				expr := k.WaitDefault
				if expr == "" {
					expr = "state=x"
				}
				client := satisfiedAfterOnePoll(t, expr)
				cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
				_, err := cmd.Execute(t.Context(), waitFlagRowInput(k, map[string]string{"timeout": "1s"}, nil), nil)
				if k.WaitDefault == "" {
					var ce *clierr.Error
					require.ErrorAs(t, err, &ce)
					assert.Equal(t, clierr.CodeValidation, ce.Code)
					require.ErrorContains(t, err, k.Name+" has no default wait condition; pass --for")
					assert.Zero(t, client.calls)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, 2, client.calls, "--timeout alone must poll to the default wait condition")
			})
		})
	}
}

func execWaitFlags(t *testing.T, kind string, in input) error {
	t.Helper()
	k := mustKind(t, kind)
	_, err := NewGet(k, &recFactory{client: &memClient{}}).(runtime.ReadCommand).Execute(t.Context(), in, nil)
	return err
}

func requireWaitFlagError(t *testing.T, err error, msg string) {
	t.Helper()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, msg)
}

// TestGetWaitFlagValidation asserts each check on the wait flags, its order
// and its wording.
func TestGetWaitFlagValidation(t *testing.T) {
	t.Parallel()
	named := []string{"web"}
	routed := map[string]string{"cluster": "prod", "namespace": "ns"}
	with := func(extra map[string]string) map[string]string { return mergeStrings(routed, extra) }
	rows := []struct {
		name  string
		kind  string
		in    input
		wants string
	}{
		{"wait=false with for", "pod", input{args: named, s: with(map[string]string{"for": "state=x"}), b: map[string]bool{"wait": false}, isSet: map[string]bool{"wait": true}}, "--wait=false conflicts with --for; drop one"},
		{"wait=false with timeout", "pod", input{args: named, s: with(map[string]string{"timeout": "1s"}), b: map[string]bool{"wait": false}, isSet: map[string]bool{"wait": true}}, "--wait=false conflicts with --timeout; drop one"},
		{"wait=false with both names for", "pod", input{args: named, s: with(map[string]string{"for": "state=x", "timeout": "1s"}), b: map[string]bool{"wait": false}, isSet: map[string]bool{"wait": true}}, "--wait=false conflicts with --for; drop one"},
		{"wait=false conflict comes before the missing name", "pod", input{s: with(map[string]string{"for": "state=x"}), b: map[string]bool{"wait": false}, isSet: map[string]bool{"wait": true}}, "--wait=false conflicts with --for; drop one"},
		{"wait without a name", "pod", input{s: routed, b: map[string]bool{"wait": true}}, "--wait requires a resource name"},
		{"for without a name", "pod", input{s: with(map[string]string{"for": "state=x"})}, "--for requires a resource name"},
		{"timeout without a name", "pod", input{s: with(map[string]string{"timeout": "1s"})}, "--timeout requires a resource name"},
		{"for and timeout without a name", "pod", input{s: with(map[string]string{"for": "state=x", "timeout": "1s"})}, "--for requires a resource name"},
		{"wait and timeout without a name", "pod", input{s: with(map[string]string{"timeout": "1s"}), b: map[string]bool{"wait": true}}, "--wait requires a resource name"},
		{"for without an id", "user", input{s: map[string]string{"for": "state=x"}}, "--for requires a resource id"},
		{"timeout on a kind without a default", "secret", input{args: named, s: with(map[string]string{"timeout": "1s"})}, "secret has no default wait condition; pass --for state=<value> or --for condition=<Type>[=<Status>]"},
		{"wait on a kind without a default", "secret", input{args: named, s: routed, b: map[string]bool{"wait": true}}, "secret has no default wait condition"},
		{"for that is not a grammar", "pod", input{args: named, s: with(map[string]string{"for": "garbage"})}, "is not a supported wait grammar"},
		{"timeout that is not a duration", "pod", input{args: named, s: with(map[string]string{"timeout": "soon"})}, `--timeout "soon" is not a valid duration, e.g. 5m`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			requireWaitFlagError(t, execWaitFlags(t, row.kind, row.in), row.wants)
		})
	}
}

// TestGetWaitFalseStartsNoWait asserts --wait=false alone is a plain read, of
// one object and of a list.
func TestGetWaitFalseStartsNoWait(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	obj := stateObj("pending", "")
	client := &memClient{items: []unstructured.Unstructured{*obj}}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	routed := map[string]string{"cluster": "prod", "namespace": "ns"}
	flags := map[string]bool{"wait": false}
	set := map[string]bool{"wait": true}

	_, err := cmd.Execute(t.Context(), input{args: []string{"x"}, s: routed, b: flags, isSet: set}, nil)
	require.NoError(t, err)
	_, err = cmd.Execute(t.Context(), input{s: routed, b: flags, isSet: set}, nil)
	require.NoError(t, err)
	assert.Len(t, client.getName, 1)
	assert.Len(t, client.listNS, 1)
}

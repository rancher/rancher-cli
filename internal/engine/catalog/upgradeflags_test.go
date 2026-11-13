package catalog

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// changeCase is one way of typing a flag and whether the upgrade then has
// nothing to change.
type changeCase struct {
	name    string
	edit    func(*appInput)
	nothing bool
}

// changeRows are the cases of each flag and argument of app upgrade, run on a
// release that already runs the target version with the values the command
// would send. The flags that say how and where, and the version, the
// repository and the values that equal what the release has, change nothing;
// every other flag typed alone makes a change.
func changeRows() map[string][]changeCase {
	typed := func(name, value string) func(*appInput) { return withFlag(name, value) }
	on := func(name string) func(*appInput) { return withBool(name, true) }
	return map[string][]changeCase{
		"release":            {{"the argument alone", func(*appInput) {}, true}},
		engine.FlagCluster:   {{"the cluster name", typed(engine.FlagCluster, testTypedCluster), true}, {"the cluster id", typed(engine.FlagCluster, testCluster), true}},
		engine.FlagNamespace: {{"the release's namespace", typed(engine.FlagNamespace, testNS), true}},
		flagWait:             {{"--wait", on(flagWait), true}},
		flagTimeout:          {{"--timeout", typed(flagTimeout, "10m"), true}},
		flagRepo:             {{"the source repository", typed(flagRepo, testRepo), true}},
		flagVersion: {
			{"the version the release runs", typed(flagVersion, "1.0.0"), true},
			{"another version", typed(flagVersion, "2.0.0"), false},
		},
		flagValues: {
			{"a file that sets what is stored", func(in *appInput) { in.strs[flagValues] = []string{"same.yaml"} }, true},
			{"a file that sets another value", func(in *appInput) { in.strs[flagValues] = []string{"other.yaml"} }, false},
		},
		flagChart:                    {{"the app's chart", typed(flagChart, testChart), false}},
		flagResetValues:              {{"--reset-values", on(flagResetValues), false}},
		flagForce:                    {{"--force", on(flagForce), false}},
		flagTakeOwnership:            {{"--take-ownership", on(flagTakeOwnership), false}},
		flagCleanupOnFail:            {{"--cleanup-on-fail", on(flagCleanupOnFail), false}},
		flagHistoryMax:               {{"--history-max", typed(flagHistoryMax, "5"), false}},
		flagHelmWait:                 {{"--helm-wait", on(flagHelmWait), false}, {"--helm-wait=false", withBool(flagHelmWait, false), false}},
		flagHelmTimeout:              {{"--helm-timeout 10m", typed(flagHelmTimeout, "10m"), false}},
		flagNoHooks:                  {{"--no-hooks", on(flagNoHooks), false}},
		flagDisableOpenAPIValidation: {{"--disable-openapi-validation", on(flagDisableOpenAPIValidation), false}},
		flagDescription:              {{"--description", typed(flagDescription, "again"), false}},
		flagSystemDefaultRegistry:    {{"--system-default-registry", typed(flagSystemDefaultRegistry, storedRegistry), false}},
		flagNoSystemDefaultRegistry:  {{"--no-system-default-registry", on(flagNoSystemDefaultRegistry), false}},
	}
}

// TestUpgradeWhichFlagsMakeAChange asserts, for every flag and argument of
// app upgrade, whether typing it alone on a release that already runs the
// target version with the values the command would send leaves nothing to
// change or makes a plan with an Effect. A flag with no row fails the test,
// as does a row for a flag the command lacks.
func TestUpgradeWhichFlagsMakeAChange(t *testing.T) {
	t.Parallel()
	d := (&upgradeCmd{}).Describe()
	var names []string
	for _, a := range d.Args {
		names = append(names, a.Name)
	}
	for _, a := range d.Flags {
		names = append(names, a.Name)
	}
	rows := changeRows()
	for _, name := range names {
		assert.Contains(t, rows, name, "--%s has no row in the table of flags that make a change", name)
	}
	for name := range rows {
		assert.Contains(t, names, name, "the table has a row for %s, which the command does not take", name)
	}
	src := files{
		"same.yaml":  "global:\n  cattle:\n    clusterId: " + testCluster + "\n",
		"other.yaml": "extra: value\n",
	}
	for name, cases := range rows {
		for _, tc := range cases {
			t.Run(name+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				f := upgradeFake(t, storedValues(registryTree(storedRegistry)))
				declareRegistry(f, "")
				eff, exec, err := planUpgrade(t, f, src, upgradeInput(tc.edit))
				if !tc.nothing {
					require.NoError(t, err)
					assert.NotNil(t, exec)
					assert.NotEmpty(t, eff.Diff)
					assert.Equal(t, "apps/"+testRelease, eff.Target)
					return
				}
				require.Error(t, err)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, "release 'my-nginx' already has version '1.0.0' of chart 'nginx' installed with these values; nothing to change", ce.Message)
				assert.Zero(t, f.postCount())
			})
		}
	}
	t.Run("no flag", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, storedValues(registryTree(storedRegistry)))
		declareRegistry(f, "")
		_, _, err := planUpgrade(t, f, src, upgradeInput())
		require.ErrorContains(t, err, "nothing to change")
	})
}

package configcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestRankingWritesMoveTheBindingStamp asserts that config set-token-reuse
// writes a fresh binding stamp when it changes the setting, and that it
// and config use-server leave the stamp alone when they write the value
// the config already has (TestUseServerAdvancesTheBindingStamp is the
// pointer move). The
// current server and the setting decide which entry the exec plugin reads,
// so a cache hit whose config load came before such a write must see the
// stamp move and judge the record again.
func TestRankingWritesMoveTheBindingStamp(t *testing.T) {
	cases := []struct {
		name    string
		cmd     func(dir string) runtime.Command
		arg     string
		stamped bool
	}{
		{name: "use-server names the current server", cmd: NewServerSwitch, arg: "prod"},
		{name: "set-token-reuse turns the setting on", cmd: NewSetTokenReuse, arg: "on", stamped: true},
		{name: "set-token-reuse writes the value it has", cmd: NewSetTokenReuse, arg: "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
				"prod":    {URL: "https://r.ex", CacheBindingID: "b-prod"},
				"staging": {URL: "https://r.ex", CacheBindingID: "b-staging"},
			}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			before, ok := config.ReadBindingStamp(dir)
			require.True(t, ok)
			require.NotEmpty(t, before)

			_, err := tc.cmd(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{tc.arg}}, ui.Discard())
			require.NoError(t, err)

			after, ok := config.ReadBindingStamp(dir)
			require.True(t, ok)
			reloaded, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			assert.Equal(t, after, reloaded.BindingStamp, "the file and the config carry the same value")
			if tc.stamped {
				assert.NotEqual(t, before, after, "the write moves the stamp")
				return
			}
			assert.Equal(t, before, after, "a write of the same value leaves the stamp alone")
		})
	}
}

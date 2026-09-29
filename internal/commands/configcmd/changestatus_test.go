package configcmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// changeRow decodes config status's credentialStoreChange field, or nil
// when the result has none.
func changeRow(t *testing.T, res runtime.Result) map[string]any {
	t.Helper()
	v, ok := res.Data.(map[string]any)["credentialStoreChange"]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var row map[string]any
	require.NoError(t, json.Unmarshal(raw, &row))
	return row
}

// TestConfigStatusReportsTheCredentialStoreChange asserts config status
// shows the directory's change record in every state the credential
// commands can meet it in - a change in flight with the work it has left,
// one that never started, one that cannot be read whether or not
// config.yaml names it - never fails because of it and opens no
// credential store for it, and shows nothing for a change that finished.
func TestConfigStatusReportsTheCredentialStoreChange(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, dir string)
		state     string
		named     bool
		remaining []any
	}{
		{name: "nothing recorded"},
		{name: "a change whose last step finished", setup: func(t *testing.T, dir string) {
			t.Helper()
			cmdtest.NameChangeRecord(t, dir, "0123456789abcdef0123456789abcdef")
		}},
		{name: "a change in flight", state: config.ChangeRecordActive, named: true,
			remaining: []any{
				map[string]any{"kind": "delete", "entry": "qa1", "store": "keychain", "lastError": "the keychain is locked"},
				map[string]any{"kind": "pin", "store": "file"},
			},
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cmdtest.WriteChangeRecord(t, dir, true,
					cmdtest.ChangeStep{Kind: "delete", Entry: "qa1", Source: "keychain", Token: "old",
						SourceAccountID: "0123456789abcdef0123456789abcdef", LastError: "the keychain is locked"},
					cmdtest.ChangeStep{Kind: "pin", Target: "file"})
			}},
		{name: "a change that never started", state: config.ChangeRecordAbandoned,
			remaining: []any{map[string]any{"kind": "pin", "store": "file"}},
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cmdtest.WriteChangeRecord(t, dir, false, cmdtest.ChangeStep{Kind: "pin", Target: "file"})
			}},
		{name: "a named record that cannot be read", state: config.ChangeRecordDamaged, named: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cmdtest.WriteDamagedChangeRecord(t, dir, true)
			}},
		{name: "a record nothing names that cannot be read", state: config.ChangeRecordDamaged,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cmdtest.WriteDamagedChangeRecord(t, dir, false)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := cmdtest.FilePolicyDir(t)
			addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
			if tc.setup != nil {
				tc.setup(t, dir)
			}

			var res runtime.Result
			counts := cmdtest.CountOperations(t, func() { res = execConfigInfoResult(t, dir) })
			assert.Zero(t, counts.Keychain, "no credential store is opened for the report")
			assert.Zero(t, counts.ConfigLocks+counts.Flocks, "and no lock is taken")

			row := changeRow(t, res)
			if tc.state == "" {
				assert.Nil(t, row)
				assert.NotContains(t, res.Text, "credentialStoreChange:")
				return
			}
			require.NotNil(t, row)
			assert.Equal(t, tc.state, row["state"])
			assert.Equal(t, config.TransitionPath(dir), row["file"])
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			if tc.named {
				assert.Equal(t, cfg.CredentialTransitionID, row["named"])
			} else {
				assert.NotContains(t, row, "named")
			}
			if tc.state == config.ChangeRecordDamaged {
				assert.Contains(t, row["error"], config.TransitionPath(dir))
				assert.Contains(t, row["error"], "restore a valid copy, or move it aside")
				assert.NotContains(t, row, "remaining")
			} else {
				assert.Equal(t, tc.remaining, row["remaining"])
			}
			assert.Contains(t, res.Text, "credentialStoreChange:\n  state: "+tc.state+" - ")
			assert.Contains(t, styleConfigInfoText(res.Text, style.Style{Enabled: true}), "\x1b[1mcredentialStoreChange:\x1b[0m\n",
				"the section label is bold, like the other sections")
		})
	}
}

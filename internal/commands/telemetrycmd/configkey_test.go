package telemetrycmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// TestTheTelemetryKeyLoadsAsExactTexts asserts every spelling of the key
// through a written config.yaml and config.Load: YAML's own boolean forms
// arrive as the texts true and false, any other value is kept as written and
// is invalid with the rest of the file loading, and a list or a map fails to
// load.
func TestTheTelemetryKeyLoadsAsExactTexts(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		line    string
		key     *string
		setting telemetry.ConfigSetting
		loadErr bool
	}{
		{name: "absent", line: "", setting: telemetry.ConfigUnset},
		{name: "empty value", line: "telemetry:", setting: telemetry.ConfigUnset},
		{name: "null", line: "telemetry: ~", setting: telemetry.ConfigUnset},
		{name: "true", line: "telemetry: true", key: text("true"), setting: telemetry.ConfigOn},
		{name: "on", line: "telemetry: on", key: text("true"), setting: telemetry.ConfigOn},
		{name: "yes", line: "telemetry: yes", key: text("true"), setting: telemetry.ConfigOn},
		{name: "y", line: "telemetry: y", key: text("true"), setting: telemetry.ConfigOn},
		{name: "True", line: "telemetry: True", key: text("true"), setting: telemetry.ConfigOn},
		{name: "quoted true", line: `telemetry: "true"`, key: text("true"), setting: telemetry.ConfigOn},
		{name: "false", line: "telemetry: false", key: text("false"), setting: telemetry.ConfigOff},
		{name: "off", line: "telemetry: off", key: text("false"), setting: telemetry.ConfigOff},
		{name: "no", line: "telemetry: no", key: text("false"), setting: telemetry.ConfigOff},
		{name: "n", line: "telemetry: n", key: text("false"), setting: telemetry.ConfigOff},
		{name: "False", line: "telemetry: False", key: text("false"), setting: telemetry.ConfigOff},
		{name: "quoted false", line: `telemetry: "false"`, key: text("false"), setting: telemetry.ConfigOff},
		{name: "quoted off", line: `telemetry: "off"`, key: text("off"), setting: telemetry.ConfigInvalid},
		{name: "quoted False", line: `telemetry: "False"`, key: text("False"), setting: telemetry.ConfigInvalid},
		{name: "quoted with spaces", line: `telemetry: " true "`, key: text(" true "), setting: telemetry.ConfigInvalid},
		{name: "single-quoted empty", line: `telemetry: ''`, key: text(""), setting: telemetry.ConfigInvalid},
		{name: "double-quoted empty", line: `telemetry: ""`, key: text(""), setting: telemetry.ConfigInvalid},
		{name: "maybe", line: "telemetry: maybe", key: text("maybe"), setting: telemetry.ConfigInvalid},
		{name: "zero", line: "telemetry: 0", key: text("0"), setting: telemetry.ConfigInvalid},
		{name: "list", line: "telemetry: [a]", loadErr: true},
		{name: "map", line: "telemetry: {a: b}", loadErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDir(t)
			writeConfig(t, dir, "currentServer: prod\n"+tc.line+"\ntokenReuse: true\n")
			cfg, err := config.Load(config.FilePath(dir))
			if tc.loadErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.key, cfg.Telemetry)
			assert.Equal(t, tc.setting, telemetry.ConfigSettingOf(cfg.Telemetry))
			assert.Equal(t, "prod", cfg.CurrentServer, "the rest of the file loads")
			assert.True(t, cfg.TokenReuse)
		})
	}
}

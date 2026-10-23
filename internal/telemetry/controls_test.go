package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestTestBinaryForcesRecordingOff asserts a test binary is off by the
// first control, whatever the other controls say, unless a test turned
// recording on.
func TestTestBinaryForcesRecordingOff(t *testing.T) {
	got := Resolve(ControlInput{Getenv: envOf(map[string]string{EnvTelemetry: "on"}), Config: ConfigOn})
	assert.Equal(t, Decision{On: false, By: ControlTestBinary}, got)
	assert.Equal(t, Decision{On: false, By: ControlTestBinary}, Resolve(ControlInput{}))

	restore := SetEnabledForTest()
	defer restore()
	assert.Equal(t, Decision{On: true, By: ControlDefault}, Resolve(ControlInput{}))
}

// TestControlsFirstMatchWins asserts the order of the controls and the
// value each accepts, with the test-binary check passed.
func TestControlsFirstMatchWins(t *testing.T) {
	t.Cleanup(SetEnabledForTest())
	cases := []struct {
		name string
		env  map[string]string
		cfg  ConfigSetting
		want Decision
	}{
		{"default is on", nil, ConfigUnset, Decision{true, ControlDefault}},
		{"do not track beats the variable", map[string]string{EnvDoNotTrack: "1", EnvTelemetry: "on"}, ConfigOn, Decision{false, ControlDoNotTrack}},
		{"do not track with any value", map[string]string{EnvDoNotTrack: "0"}, ConfigUnset, Decision{false, ControlDoNotTrack}},
		{"empty do not track is unset", map[string]string{EnvDoNotTrack: ""}, ConfigUnset, Decision{true, ControlDefault}},
		{"variable on beats config off", map[string]string{EnvTelemetry: "on"}, ConfigOff, Decision{true, ControlVariable}},
		{"variable off beats config on", map[string]string{EnvTelemetry: "off"}, ConfigOn, Decision{false, ControlVariable}},
		{"empty variable is unset", map[string]string{EnvTelemetry: ""}, ConfigOff, Decision{false, ControlConfig}},
		{"any other variable value is off", map[string]string{EnvTelemetry: "yes"}, ConfigOn, Decision{false, ControlVariable}},
		{"uppercase value is off", map[string]string{EnvTelemetry: "ON"}, ConfigUnset, Decision{false, ControlVariable}},
		{"config on", nil, ConfigOn, Decision{true, ControlConfig}},
		{"config off", nil, ConfigOff, Decision{false, ControlConfig}},
		{"a config that failed to load is off", nil, ConfigUnreadable, Decision{false, ControlConfig}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Resolve(ControlInput{Getenv: envOf(c.env), Config: c.cfg}), c.name)
	}
}

// TestDefaultIsOnOutsideATestBinary asserts the default without the test
// override, as a shipped binary sees it.
func TestDefaultIsOnOutsideATestBinary(t *testing.T) {
	old := underTest
	underTest = func() bool { return false }
	t.Cleanup(func() { underTest = old })
	assert.Equal(t, Decision{true, ControlDefault}, Resolve(ControlInput{}))
	assert.Equal(t, Decision{false, ControlDoNotTrack}, Resolve(ControlInput{Getenv: envOf(map[string]string{EnvDoNotTrack: "1"})}))
}

// TestParseConfigSetting asserts the config key's text.
func TestParseConfigSetting(t *testing.T) {
	for in, want := range map[string]ConfigSetting{"on": ConfigOn, "off": ConfigOff, "ON": ConfigOn, " off ": ConfigOff, "": ConfigUnset, "maybe": ConfigUnset} {
		assert.Equal(t, want, ParseConfigSetting(in), in)
	}
}

// TestSetServerHolder asserts the holder keeps the last call and resets.
func TestSetServerHolder(t *testing.T) {
	ResetServerForTest()
	t.Cleanup(ResetServerForTest)
	url, ca := Server()
	assert.Empty(t, url)
	assert.Empty(t, ca)
	SetServer("https://a.example", "ca-a")
	SetServer("https://b.example", "")
	url, ca = Server()
	assert.Equal(t, "https://b.example", url)
	assert.Empty(t, ca)
	SetServer("", "ca")
	url, _ = Server()
	assert.Empty(t, url, "an empty URL counts as not set")
}

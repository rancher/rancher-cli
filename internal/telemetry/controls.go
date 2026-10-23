package telemetry

import (
	"strings"
	"sync/atomic"
	"testing"
)

// Environment variables the controls read.
const (
	// EnvDoNotTrack turns recording off when set to any non-empty value.
	EnvDoNotTrack = "DO_NOT_TRACK"
	// EnvTelemetry turns recording on or off: on or off; empty counts as
	// unset and any other value is off.
	EnvTelemetry = "RANCHER_CLI_TELEMETRY"
)

// Control names the control that decided whether recording is on.
type Control string

// The controls, in the order they are checked; the first match decides.
const (
	ControlTestBinary Control = "test binary"
	ControlDoNotTrack Control = "DO_NOT_TRACK"
	ControlVariable   Control = "RANCHER_CLI_TELEMETRY"
	ControlConfig     Control = "config"
	ControlDefault    Control = "default"
)

// ConfigSetting is the telemetry key of config.yaml as the caller loaded it.
type ConfigSetting int

// The settings. ConfigUnreadable is a config that failed to load: off.
const (
	ConfigUnset ConfigSetting = iota
	ConfigOn
	ConfigOff
	ConfigUnreadable
)

// ParseConfigSetting reads the key's text: on, off, or anything else unset.
func ParseConfigSetting(s string) ConfigSetting {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on":
		return ConfigOn
	case "off":
		return ConfigOff
	}
	return ConfigUnset
}

// ControlInput is everything Resolve reads.
type ControlInput struct {
	// Getenv reads an environment variable; nil reads none.
	Getenv func(string) string
	Config ConfigSetting
}

// Decision is whether recording is on and which control decided.
type Decision struct {
	On bool
	By Control
}

// underTest reports whether the process is a test binary.
var underTest = testing.Testing

var testRecordingOn atomic.Bool

// SetEnabledForTest lets recording run inside a test binary until the
// returned function is called. The calling test must not use t.Parallel():
// the setting is process-wide.
func SetEnabledForTest() (restore func()) {
	old := testRecordingOn.Swap(true)
	return func() { testRecordingOn.Store(old) }
}

// Resolve decides whether recording is on. The first match wins: a test
// binary (off unless the test turned recording on); DO_NOT_TRACK set to
// any non-empty value (off); RANCHER_CLI_TELEMETRY (on or off, empty counts
// as unset, any other value is off); the config key (a config that failed
// to load is off); else the default, on.
func Resolve(in ControlInput) Decision {
	if underTest() && !testRecordingOn.Load() {
		return Decision{On: false, By: ControlTestBinary}
	}
	get := in.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	if get(EnvDoNotTrack) != "" {
		return Decision{On: false, By: ControlDoNotTrack}
	}
	if v := get(EnvTelemetry); v != "" {
		return Decision{On: v == "on", By: ControlVariable}
	}
	switch in.Config {
	case ConfigOn:
		return Decision{On: true, By: ControlConfig}
	case ConfigOff, ConfigUnreadable:
		return Decision{On: false, By: ControlConfig}
	}
	return Decision{On: true, By: ControlDefault}
}

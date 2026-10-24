package telemetry

import (
	"sync/atomic"
	"testing"
)

// EnvTelemetry is the environment variable that turns recording on or off:
// exactly true or false. Empty counts as unset and any other value is
// ignored.
const EnvTelemetry = "RANCHER_CLI_TELEMETRY"

// Control names the control that decided whether recording is on.
type Control string

// The controls, in the order they are checked; the first match decides.
const (
	ControlTestBinary Control = "test binary"
	ControlVariable   Control = "RANCHER_CLI_TELEMETRY"
	ControlConfig     Control = "config"
	ControlDefault    Control = "default"
)

// Controls returns every control in the order Resolve checks them.
func Controls() []Control {
	return []Control{ControlTestBinary, ControlVariable, ControlConfig, ControlDefault}
}

// ConfigSetting is the telemetry key of config.yaml as the caller loaded it.
type ConfigSetting int

// The settings. ConfigInvalid is a key whose text is neither true nor false;
// the default decides then. ConfigUnreadable is a config that failed to
// load: off.
const (
	ConfigUnset ConfigSetting = iota
	ConfigOn
	ConfigOff
	ConfigUnreadable
	ConfigInvalid
)

// ConfigSettingOf reads the key as loaded: nil (the key is absent) is unset,
// exactly "true" is on, exactly "false" is off, and any other text, the
// empty text included, is invalid. It trims nothing and folds no case.
func ConfigSettingOf(v *string) ConfigSetting {
	if v == nil {
		return ConfigUnset
	}
	switch *v {
	case "true":
		return ConfigOn
	case "false":
		return ConfigOff
	}
	return ConfigInvalid
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
	// IgnoredVariable is the text of RANCHER_CLI_TELEMETRY when it is set to
	// something other than true or false; the variable then decides nothing.
	IgnoredVariable string
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
// binary (off unless the test turned recording on); RANCHER_CLI_TELEMETRY
// set to exactly true or false (empty counts as unset; any other value is
// ignored and returned in IgnoredVariable); the config key (an invalid key
// counts as unset; a config that failed to load is off); else the default,
// on.
func Resolve(in ControlInput) Decision {
	if underTest() && !testRecordingOn.Load() {
		return Decision{On: false, By: ControlTestBinary}
	}
	get := in.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	var ignored string
	switch v := get(EnvTelemetry); v {
	case "true":
		return Decision{On: true, By: ControlVariable}
	case "false":
		return Decision{On: false, By: ControlVariable}
	case "":
	default:
		ignored = v
	}
	switch in.Config {
	case ConfigOn:
		return Decision{On: true, By: ControlConfig, IgnoredVariable: ignored}
	case ConfigOff, ConfigUnreadable:
		return Decision{On: false, By: ControlConfig, IgnoredVariable: ignored}
	}
	return Decision{On: true, By: ControlDefault, IgnoredVariable: ignored}
}

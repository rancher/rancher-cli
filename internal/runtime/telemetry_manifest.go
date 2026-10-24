package runtime

import "github.com/rancher/rancher-cli/internal/telemetry"

// TelemetryManifest is the usage contract version the CLI follows and the
// controls that decide whether recording is on, in the order they are
// checked. The text is the same on every machine.
type TelemetryManifest struct {
	ContractVersion string             `json:"contractVersion"`
	Controls        []TelemetryControl `json:"controls"`
}

// TelemetryControl is one control. Name is what `telemetry status` shows as
// the deciding control; Key names the config.yaml key of a control that is
// one.
type TelemetryControl struct {
	Name   string   `json:"name"`
	Source string   `json:"source"`
	Key    string   `json:"key,omitempty"`
	Values []string `json:"values"`
	Desc   string   `json:"desc"`
}

// telemetryControlText is the manifest text of each control the user can
// act on. The test-binary control belongs to tests and is not listed.
var telemetryControlText = map[telemetry.Control]TelemetryControl{
	telemetry.ControlVariable: {
		Source: "environment",
		Values: []string{"true", "false"},
		Desc:   "true or false; empty counts as unset; any other value is ignored",
	},
	telemetry.ControlConfig: {
		Source: "config",
		Key:    "telemetry",
		Values: []string{"true", "false"},
		Desc:   "the config.yaml key, written by telemetry on and telemetry off; any other value leaves the default",
	},
	telemetry.ControlDefault: {
		Source: "default",
		Values: []string{"on"},
		Desc:   "applies when nothing above decides",
	},
}

// telemetryManifest builds the manifest field from the telemetry package's
// own control list and version.
func telemetryManifest() TelemetryManifest {
	m := TelemetryManifest{ContractVersion: telemetry.ContractVersion, Controls: []TelemetryControl{}}
	for _, c := range telemetry.Controls() {
		if c == telemetry.ControlTestBinary {
			continue
		}
		entry := telemetryControlText[c]
		entry.Name = string(c)
		m.Controls = append(m.Controls, entry)
	}
	return m
}

// Package experiment holds the table of experimental settings that the
// RANCHER_CLI_EXPERIMENT environment variable turns on, and the parser of the
// variable. It reads no environment and no file and imports only the
// standard library.
package experiment

import "strings"

// EnvVar is the environment variable that lists the experiments to turn on.
const EnvVar = "RANCHER_CLI_EXPERIMENT"

// TelemetryUpload is the name of the experiment that sets the upload
// endpoint: a development build sends the stored events of finished hours to
// it at most once an hour, after a recorded run, never from the kubectl exec
// plugin.
const TelemetryUpload = "telemetryupload"

// Experiment describes one experimental setting.
type Experiment struct {
	// Name matches [a-z][a-z0-9]*.
	Name string
	// Description says what the experiment does, for the schema and the
	// reference.
	Description string
	// ValueForm says what the value after = looks like.
	ValueForm string
	// DevOnly is true when only a development build honors the experiment.
	DevOnly bool
}

// Table returns every experiment, in the order they are documented.
func Table() []Experiment {
	return []Experiment{
		{
			Name: TelemetryUpload,
			Description: "the endpoint for uploading usage data: a development build sends the stored events of " +
				"finished hours to it at most once an hour, after a recorded run, never from the kubectl exec plugin",
			ValueForm: "scheme://host[:port] with path empty or /; https, or http to localhost, ::1 or 127.0.0.0/8; a host that is an IP address or a DNS name in ASCII (write an international name in its xn-- form); a port from 1 to 65535, read as a number (leading zeros and the default port are dropped); no user name, password, query or fragment",
			DevOnly:   true,
		},
	}
}

// Lookup returns the table row of a name.
func Lookup(name string) (Experiment, bool) {
	for _, e := range Table() {
		if e.Name == name {
			return e, true
		}
	}
	return Experiment{}, false
}

// ItemKind says what an item of the variable is.
type ItemKind int

const (
	// Unrecognized is an item whose name does not match [a-z][a-z0-9]*.
	Unrecognized ItemKind = iota
	// Unknown is an item whose name matches the pattern and is not in the
	// table.
	Unknown
	// Known is an item whose name is in the table.
	Known
)

// Item is one item of the variable.
type Item struct {
	// Position is the 1-based position among the items split on a comma,
	// empty items counted, so a reader finds the item by counting commas.
	Position int
	Kind     ItemKind
	// Name and Value are empty for an Unrecognized item: its text can hold a
	// password and is never returned.
	Name  string
	Value string
}

// Parse splits the text of the variable on commas, each item at its first =
// (the value runs to the next comma), skips empty items and trims nothing. An
// empty value is the same as a bare name.
func Parse(text string) []Item {
	var items []Item
	pos := 0
	for raw := range strings.SplitSeq(text, ",") {
		pos++
		if raw == "" {
			continue
		}
		name, value, _ := strings.Cut(raw, "=")
		it := Item{Position: pos}
		switch {
		case !validName(name):
			it.Kind = Unrecognized
		default:
			it.Name, it.Value = name, value
			if _, ok := Lookup(name); ok {
				it.Kind = Known
			} else {
				it.Kind = Unknown
			}
		}
		items = append(items, it)
	}
	return items
}

// validName reports whether name matches [a-z][a-z0-9]*.
func validName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

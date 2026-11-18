package nodes

import (
	"strconv"
	"time"
)

// formatDuration writes d as a flag value: whole units, zero units left out
// (2m, 1h30m, 1m30s), as --help shows a default. A duration that is not a
// whole number of seconds is written with its fraction.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Second != 0 {
		return d.String()
	}
	hours := d / time.Hour
	d -= hours * time.Hour
	minutes := d / time.Minute
	d -= minutes * time.Minute
	seconds := d / time.Second
	var out string
	if hours > 0 {
		out += strconv.Itoa(int(hours)) + "h"
	}
	if minutes > 0 {
		out += strconv.Itoa(int(minutes)) + "m"
	}
	if seconds > 0 {
		out += strconv.Itoa(int(seconds)) + "s"
	}
	return out
}

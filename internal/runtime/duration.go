package runtime

import (
	"fmt"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// DurationFlag reads a duration-valued flag: an omitted flag is def; any
// other value must parse as a positive Go duration, else the result is a
// VALIDATION error naming the flag and showing example.
func DurationFlag(in Input, name string, def time.Duration, example string) (time.Duration, error) {
	s := in.String(name)
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %q is not a valid duration, e.g. %s", name, s, example))
	}
	if d <= 0 {
		return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %q must be a positive duration, e.g. %s", name, s, example))
	}
	return d, nil
}

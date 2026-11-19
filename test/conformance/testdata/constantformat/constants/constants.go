package constants

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var listing = clierr.Rancher + " get tokens"

func marked(x string) string { return clierr.Rancher + fmt.Sprintf(" auth login %s", x) }

func asArgument(x string) string {
	return fmt.Sprintf("listed by: %s (%s)", listing, x)
}

func joinedOutside(x string) string {
	return fmt.Sprintf("listed by: (%s) ", x) + listing
}

func literal() string { return fmt.Sprintf("literal") }

func wrapped(x string) error { return fmt.Errorf("failed %s", x) }

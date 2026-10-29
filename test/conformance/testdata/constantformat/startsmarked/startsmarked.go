package startsmarked

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func command(entry string) string {
	return fmt.Sprintf(clierr.Rancher+" auth login %s", entry)
}

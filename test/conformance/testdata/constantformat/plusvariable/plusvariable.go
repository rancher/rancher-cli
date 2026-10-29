package plusvariable

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/config"
)

func warning(entry string, err error) string {
	return fmt.Sprintf("could not update %s (%v); they are listed by: "+config.StatusCommand, entry, err)
}

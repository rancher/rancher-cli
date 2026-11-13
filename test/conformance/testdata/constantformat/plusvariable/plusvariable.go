package plusvariable

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func warning(entry string, err error) string {
	return fmt.Sprintf("could not update %s (%v); they are listed by: "+clierr.AuthStatusRemedy, entry, err)
}

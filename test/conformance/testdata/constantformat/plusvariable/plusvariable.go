package plusvariable

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var listing = clierr.Rancher + " get tokens"

func warning(entry string, err error) string {
	return fmt.Sprintf("could not update %s (%v); they are listed by: "+listing, entry, err)
}

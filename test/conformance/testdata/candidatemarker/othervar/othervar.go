package othervar

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func build() clierr.Candidate {
	return clierr.Candidate{ID: "c-1", Name: fmt.Sprint(clierr.DirectoryFreeCommands)}
}

package platform

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func quotedOnLinux() string { return fmt.Sprintf("%q", clierr.Rancher) }

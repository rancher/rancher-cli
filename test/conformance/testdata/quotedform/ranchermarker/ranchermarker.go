package ranchermarker

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func quoted() string { return fmt.Sprintf("%q", clierr.Rancher+" get users") }

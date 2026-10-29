package variableinit

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var text = fmt.Sprintf(clierr.Rancher+" get %s", "pods")

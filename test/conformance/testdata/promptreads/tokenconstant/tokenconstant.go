package tokenconstant

import (
	"os"

	"github.com/rancher/rancher-cli/internal/config"
)

// Read reads the token variable by its constant.
func Read() string { return os.Getenv(config.EnvToken) }

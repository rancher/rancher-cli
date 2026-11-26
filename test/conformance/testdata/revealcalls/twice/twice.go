package twice

import "github.com/rancher/rancher-cli/internal/secret"

var sink string

func init() { sink = secret.Reveal(secret.New("a")) }

func init() { sink = secret.Reveal(secret.New("b")) }

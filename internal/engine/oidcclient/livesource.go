package oidcclient

import (
	"github.com/rancher/rancher-cli/internal/engine/oidcclient/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface the secret-name source: the second
// positional completes the secrets of the client typed as the first;
// every other site is declined (the client positional is the kind's
// own).
func (rotateSecretCmd) LiveSource() completion.Source { return complete.SecretNameSource() }

// LiveSource hands the surface the secret-name source.
func (removeSecretCmd) LiveSource() completion.Source { return complete.SecretNameSource() }

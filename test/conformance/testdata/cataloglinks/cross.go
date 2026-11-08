package fixture

import "github.com/rancher/rancher-cli/test/conformance/testdata/cataloglinks/head"

// cross completes a link request from a constant of another package.
func cross() string { return head.Head + "=index" }

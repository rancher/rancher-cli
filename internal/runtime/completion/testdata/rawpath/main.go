// Command rawpath must not compile: GetJSON takes a Path only the
// completion package's builders produce, so a string literal cannot be
// requested. The package's tests build this program and assert the
// compiler's refusal.
package main

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

func main() {
	var conn *completion.Connection
	var dst any
	_ = conn.GetJSON(context.Background(), "/k8s/clusters/local/api/v1/namespaces", "", &dst)
}

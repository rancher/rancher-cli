package aliased

import (
	ce "github.com/rancher/rancher-cli/internal/clierr"
)

type request = ce.Request

func renamedImport() *ce.Request { return &ce.Request{Server: "s"} }

func typeAlias() *request { return &request{Server: "s"} }

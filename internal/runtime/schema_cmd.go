package runtime

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// schemaCmd emits the capability manifest. It is readOnly (produces no server
// mutation) and needs no auth.
type schemaCmd struct {
	reg        *Registry
	cliVersion string
}

// NewSchemaCommand builds the `schema` command bound to a registry.
func NewSchemaCommand(reg *Registry, cliVersion string) Command {
	return &schemaCmd{reg: reg, cliVersion: cliVersion}
}

func (s *schemaCmd) Describe() Descriptor {
	return Descriptor{
		Name:        "schema",
		Usage:       "Print the machine-readable capability manifest",
		CommandType: ReadOnly,
		Examples:    []string{"rancher schema -o json"},
	}
}

func (s *schemaCmd) Plan(context.Context, Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (s *schemaCmd) Execute(context.Context, Input, safety.Effect) (Result, error) {
	return Result{Kind: "Schema", Data: s.reg.Schema(s.cliVersion)}, nil
}

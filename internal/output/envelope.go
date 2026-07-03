package output

import "github.com/rancher/rancher-cli/internal/clierr"

// APIVersion is the versioned output-schema identifier agents pin to.
const APIVersion = "cli.rancher.io/v1"

// Envelope is the single document emitted per invocation on stdout.
type Envelope struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind" yaml:"kind"`
	Tier       string   `json:"tier,omitempty" yaml:"tier,omitempty"`
	Stability  string   `json:"stability,omitempty" yaml:"stability,omitempty"`
	Data       any      `json:"data,omitempty" yaml:"data,omitempty"`
	Warnings   []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}

func NewEnvelope(kind string, data any) *Envelope {
	return &Envelope{APIVersion: APIVersion, Kind: kind, Data: data}
}

// errorPayload is the {"error": {...}} shape for the Error envelope.
type errorPayload struct {
	Error *clierr.Error `json:"error" yaml:"error"`
}

// ErrorEnvelope wraps a structured error as kind:"Error".
func ErrorEnvelope(e *clierr.Error) *Envelope {
	return &Envelope{APIVersion: APIVersion, Kind: "Error", Data: errorPayload{Error: e}}
}

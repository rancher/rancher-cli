package planted

import (
	"fmt"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/rancher/rancher-cli/internal/clierr"
)

func callError(p jsonpatch.Patch, doc []byte) string {
	_, err := p.Apply(doc)
	if err != nil {
		return err.Error()
	}
	return ""
}

func wrapped(p jsonpatch.Patch, doc []byte) error {
	_, err := p.Apply(doc)
	return fmt.Errorf("apply: %w", err)
}

func printed(p jsonpatch.Patch, doc []byte) string {
	_, err := p.Apply(doc)
	return fmt.Sprintf("apply: %v", err)
}

func asText(p jsonpatch.Patch, doc []byte) *clierr.Error {
	_, err := p.Apply(doc)
	return clierr.New(clierr.CodeValidation, "cannot apply: "+err.Error())
}

func viaCause(p jsonpatch.Patch, doc []byte) *clierr.Error {
	_, err := p.Apply(doc)
	return clierr.New(clierr.CodeValidation, "cannot apply").WithCause(err)
}

func alias(p jsonpatch.Patch, doc []byte) error {
	_, err := p.Apply(doc)
	e := err
	return e
}

func returned(p jsonpatch.Patch, doc []byte) ([]byte, error) {
	return p.Apply(doc)
}

func returnedMerge(doc, patch []byte) ([]byte, error) {
	return jsonpatch.MergePatch(doc, patch)
}

func methodValue(p jsonpatch.Patch, doc []byte) string {
	_, err := p.Apply(doc)
	f := err.Error
	return f()
}

func methodExpression(p jsonpatch.Patch, doc []byte) string {
	_, err := p.Apply(doc)
	return error.Error(err)
}

func appended(p jsonpatch.Patch, doc []byte) []byte {
	_, err := p.Apply(doc)
	return fmt.Appendf(nil, "apply: %v", err)
}

func panicked(p jsonpatch.Patch, doc []byte) {
	_, err := p.Apply(doc)
	panic(err)
}

var packageLevel = func(p jsonpatch.Patch, doc []byte) string {
	_, err := p.Apply(doc)
	return err.Error()
}

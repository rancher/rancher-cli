package allowed

import (
	"errors"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

func explainOperation(err error) string {
	if err == nil {
		return ""
	}
	return "the operation does not apply"
}

func use(p jsonpatch.Patch, doc []byte) string {
	out, err := p.Apply(doc)
	if err == nil {
		return string(out)
	}
	if errors.Is(err, jsonpatch.ErrMissing) {
		return "missing"
	}
	if _, ok := errors.AsType[*jsonpatch.AccumulatedCopySizeError](err); ok {
		return "size"
	}
	return explainOperation(err)
}

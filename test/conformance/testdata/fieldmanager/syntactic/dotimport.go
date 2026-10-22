package fixture

import (
	"context"
	. "net/http"
)

func build() (*Request, error) {
	return NewRequest(MethodPost, "https://example.com/apis", nil)
}

func use(ctx context.Context) {
	_ = NewRequestWithContext
}

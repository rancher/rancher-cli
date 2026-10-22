package fixture

import "net/http"

func build(method string) (*http.Request, error) {
	return http.NewRequest(method, "https://example.com/apis", nil)
}

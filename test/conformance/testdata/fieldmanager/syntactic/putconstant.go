package fixture

import "net/http"

func build() (*http.Request, error) {
	return http.NewRequest(http.MethodPut, "https://example.com/apis", nil)
}

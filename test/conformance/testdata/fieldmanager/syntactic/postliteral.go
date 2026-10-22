package fixture

import "net/http"

func build() (*http.Request, error) {
	return http.NewRequest("POST", "https://example.com/apis", nil)
}

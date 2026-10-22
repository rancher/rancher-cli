package fixture

import "net/http"

func build() (*http.Request, error) {
	return http.NewRequest("post", "https://example.com/apis", nil)
}

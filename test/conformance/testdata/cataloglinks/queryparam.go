package fixture

import "net/url"

// viaValues sets the link as a query parameter.
func viaValues() string {
	q := url.Values{}
	q.Set("link", "index")
	return q.Encode()
}

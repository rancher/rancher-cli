package urlassignpass

import "net/url"

func set(u *url.URL) {
	u.RawQuery = "a=b"
}

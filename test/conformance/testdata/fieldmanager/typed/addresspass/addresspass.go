package addresspass

import "net/url"

func set(*string) {}

func flip(u *url.URL) {
	set(&u.RawQuery)
}

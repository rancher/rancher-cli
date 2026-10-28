// Reads fields the scan ignores: the URL field of an unrelated struct.
package readsother

import "net/url"

type holder struct{ URL string }

func Read(u url.URL, h holder) string {
	return u.Host + h.URL + u.Path
}

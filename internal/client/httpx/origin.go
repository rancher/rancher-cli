package httpx

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Origin is the normalized (scheme, hostname, effective port) of a URL.
// Fields are private: OriginOf is the only constructor, so a value that
// exists is normalized, and the zero value is invalid.
type Origin struct {
	scheme   string
	hostname string
	port     string
}

var defaultPort = map[string]string{"http": "80", "https": "443"}

// OriginOf normalizes u: scheme and hostname lowercased, hostname without
// IPv6 brackets, port defaulted (443 https, 80 http) and canonicalized so
// ":0443" and ":443" are one origin. Errors: nil URL, scheme other than
// http/https, empty hostname, port outside 1..65535.
func OriginOf(u *url.URL) (Origin, error) {
	if u == nil {
		return Origin{}, errors.New("URL is missing")
	}
	scheme := strings.ToLower(u.Scheme)
	if _, ok := defaultPort[scheme]; !ok {
		return Origin{}, fmt.Errorf("URL scheme %q is not http or https", u.Scheme)
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return Origin{}, errors.New("URL has no host")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort[scheme]
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return Origin{}, fmt.Errorf("URL port %q is invalid", port)
	}
	return Origin{scheme: scheme, hostname: hostname, port: strconv.FormatUint(n, 10)}, nil
}

// Valid is false for the zero value.
func (o Origin) Valid() bool { return o.scheme != "" && o.hostname != "" && o.port != "" }

// Equal is false when either side is invalid.
func (o Origin) Equal(other Origin) bool { return o.Valid() && other.Valid() && o == other }

// String renders scheme://host[:port], the port shown only when it is not
// the scheme default; IPv6 hosts are bracketed. Empty for an invalid value.
func (o Origin) String() string {
	if !o.Valid() {
		return ""
	}
	return o.scheme + "://" + o.hostPort()
}

// hostPort is the one host[:port] formatting rule String and HostPort
// share: the port is omitted when it is the scheme default, and an IPv6
// host is bracketed either way.
func (o Origin) hostPort() string {
	if o.port == defaultPort[o.scheme] {
		if strings.Contains(o.hostname, ":") {
			return "[" + o.hostname + "]"
		}
		return o.hostname
	}
	return net.JoinHostPort(o.hostname, o.port)
}

// Hostname is the normalized host, the name a certificate is verified for.
func (o Origin) Hostname() string { return o.hostname }

// HostPort is String without the scheme: host[:port], the port shown only
// when it is not the scheme default, for messages that must say which of
// two same-named servers on different ports failed. Empty for an invalid
// value.
func (o Origin) HostPort() string {
	if !o.Valid() {
		return ""
	}
	return o.hostPort()
}

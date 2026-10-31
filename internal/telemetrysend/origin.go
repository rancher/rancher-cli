// Package telemetrysend checks the value of the telemetryupload experiment.
// It makes no request and imports no HTTP package.
package telemetrysend

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/rancher/rancher-cli/credhelper"
)

// The reasons Origin refuses a value. Each is a fixed sentence that holds no
// part of the value, because the value may hold a password.
var (
	errScheme = errors.New("it must use https; http is accepted only for localhost, ::1 and 127.0.0.0/8")
	errQuery  = errors.New("it holds a query or fragment")
	errPath   = errors.New("it holds a path; give only scheme://host[:port]")
	errPort   = errors.New("it holds a port that is not a number from 1 to 65535")
	errHost   = errors.New("it holds a host that is not an IP address or a DNS name")
	errIP     = errors.New("it holds a host that is not a valid IP address")
	errASCII  = errors.New("it holds a host with non-ASCII characters; write the host in its ASCII (xn--) form")
	errSyntax = errors.New("it is not a valid URL")
)

// Origin checks the value of the telemetryupload experiment and returns its
// canonical origin, scheme://host[:port] with a lower-case host and the
// default port dropped and a port written without leading zeros. The value
// must be an origin: https, or http only for localhost, ::1 and 127.0.0.0/8,
// with no user name, password, query, fragment or path other than /, a port
// from 1 to 65535 when there is one, and a host that is an IP address or a DNS
// name in its ASCII form. The error is a fixed sentence that holds no part of
// the value.
func Origin(value string) (string, error) {
	origin, err := credhelper.CheckServerURL(value)
	if err != nil {
		return "", err
	}
	return originFrom(value, origin)
}

// originFrom applies the rules after the URL check, which returned origin for
// value. It is separate so a test reaches the failures of the later parses,
// which the check rules out for any value it accepts.
func originFrom(value, origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", errSyntax
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return "", errScheme
	}
	if strings.ContainsAny(value, "?#") {
		return "", errQuery
	}
	raw, err := url.Parse(value)
	if err != nil {
		return "", errSyntax
	}
	if p := raw.EscapedPath(); p != "" && p != "/" {
		return "", errPath
	}
	port, err := portNumber(u)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if err := checkHost(host); err != nil {
		return "", err
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == defaultPort(u.Scheme) || port == "" {
		return u.Scheme + "://" + host, nil
	}
	return u.Scheme + "://" + host + ":" + port, nil
}

// defaultPort is the port a scheme uses when none is given.
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// portNumber returns the port of u read as a number without leading zeros,
// or "" when u has none. An empty port, 0 and a port above 65535 are refused.
func portNumber(u *url.URL) (string, error) {
	if strings.HasSuffix(u.Host, ":") {
		return "", errPort
	}
	port := u.Port()
	if port == "" {
		return "", nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errPort
	}
	return strconv.Itoa(n), nil
}

// checkHost refuses a host that is not an IP address with no zone or a DNS
// name: dot-separated labels of letters, digits and hyphens, none starting or
// ending with a hyphen, at most 63 characters each and 253 in all. A name of
// only digits and dots must be a valid IPv4 address. A non-ASCII name is
// refused with a reason that asks for its xn-- form.
func checkHost(host string) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return errHost
		}
		return nil
	}
	if strings.IndexFunc(host, func(c rune) bool { return c > unicode.MaxASCII }) >= 0 {
		return errASCII
	}
	if host == "" || len(host) > 253 {
		return errHost
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" {
			return errHost
		}
	}
	if strings.Trim(host, "0123456789.") == "" {
		return errIP
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errHost
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return errHost
			}
		}
	}
	return nil
}

// isLoopbackHost reports whether host is localhost, ::1 or an address in
// 127.0.0.0/8. An IPv4 address written in IPv6 form, an address with a zone
// and a name with a trailing dot are not.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" || addr.Is4In6() {
		return false
	}
	return addr.IsLoopback()
}

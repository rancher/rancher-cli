package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type Options struct {
	CACert   string
	Insecure bool
	ProxyURL string
	Timeout  time.Duration
}

func New(o Options) (*http.Client, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if o.Insecure {
		tlsConfig.InsecureSkipVerify = true
	} else if o.CACert != "" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(o.CACert)) {
			return nil, errors.New("failed to append CA certs from PEM")
		}
		tlsConfig.RootCAs = roots
	}

	var proxyFunc func(*http.Request) (*url.URL, error)
	if o.ProxyURL != "" {
		proxyURLParsed, err := url.Parse(o.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		proxyFunc = http.ProxyURL(proxyURLParsed)
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	tr := &http.Transport{
		Proxy:                 proxyFunc,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           dialer.DialContext,
	}

	return &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
	}, nil
}

// Do executes req and reads the full response body, closing it before
// returning — the one request/response shape every hand-rolled Rancher API
// caller in this codebase needs (token's provider dance, login's ext-token
// mint/revoke), so it lives here once instead of once per package.
func Do(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return resp, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

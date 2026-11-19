package compliant

import "github.com/rancher/rancher-cli/internal/clierr"

func withLogin(login clierr.Command) *clierr.Request {
	return &clierr.Request{Server: "s", Login: login}
}

func sendsNone() *clierr.Request { return &clierr.Request{NoCredential: true} }

type holder struct{ request *clierr.Request }

func unnamedResult() (*clierr.Request, error) { return nil, nil }

func pointers() []*clierr.Request { return make([]*clierr.Request, 2) }

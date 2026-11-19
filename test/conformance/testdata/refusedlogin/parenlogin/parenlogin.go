package parenlogin

import "github.com/rancher/rancher-cli/internal/clierr"

func request() *clierr.Request { return &clierr.Request{Server: "s", Login: (clierr.Command{})} }

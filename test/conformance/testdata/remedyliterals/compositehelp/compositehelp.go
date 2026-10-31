package compositehelp

import "fmt"

type descriptor struct {
	Usage []string
	Desc  string
}

func help(name string) descriptor {
	return descriptor{
		Usage: []string{"rancher get users"},
		Desc:  fmt.Sprintf("rancher get users %s", name),
	}
}

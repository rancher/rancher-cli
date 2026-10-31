package prosebackticks

type descriptor struct {
	Desc  string
	Usage string
}

var bare = descriptor{Desc: "create it with `create cluster` first"}

var quoted = descriptor{Usage: "use 'create cluster'"}

var word = descriptor{Desc: "(see: rancher get users)"}

var good = descriptor{Desc: "create it with `rancher create cluster` first"}

var split = descriptor{Desc: "see `" + "rancher get users` for the list"}

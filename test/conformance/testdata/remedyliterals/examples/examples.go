package examples

type descriptor struct{ Examples []string }

var help = descriptor{Examples: []string{
	"get users",
	"rancher get users",
	"cat f | rancher create resource -f -",
	"diff <(rancher get users) x",
	"get " + name,
}}

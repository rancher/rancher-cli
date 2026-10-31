package helpfields

type descriptor struct {
	Examples []string
	Desc     string
	Usage    string
	ArgDesc  string
}

var help = descriptor{
	Examples: []string{"rancher get users", "rancher " + "get roletemplates"},
	Desc:     "run `rancher get users` first",
	Usage:    "see: `rancher get users`" + " and `rancher get globalroles`",
	ArgDesc:  "use `rancher access list`",
}

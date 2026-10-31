package chainedcommands

func adjacent() string { return "get users; get roletemplates" }

func oneLiteral() string { return "x, then get users, then get roletemplates" }

func chain(n string) string { return "ask " + "get users " + n + "; use access list" }

package positions

type descriptor struct{ Name string }

var named = descriptor{Name: "get users"}

var keyed = map[string]bool{"get users": true}

var firstLoginCommands = []string{"auth login", "auth get-token"}

func compare(command string) bool {
	switch command {
	case "get users":
		return true
	}
	return command == "get users" || command != "delete token"
}

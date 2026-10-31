package helptables

type kind struct{ NoDeleteReason string }

var groupUsages = map[string]string{"x": "use rancher get users"}

func descriptions() map[string]string {
	groupDescs := map[string]string{"y": "create it with `rancher create user`"}
	return groupDescs
}

var row = kind{NoDeleteReason: "rancher get users"}

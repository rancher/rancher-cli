package builton

import "github.com/rancher/rancher-cli/internal/clierr"

var Direct = clierr.Rancher + " auth login"

var Chained = Direct + ", then log in again"

var Wrapped = "run: " + Chained

var Plain = "rancher auth login"

var FromMarker = clierr.RemedyMarker + "rancher access clusters"

var FromFunction = remedyText()

func remedyText() string { return clierr.Rancher + " access clusters" }

var FromLocalInFunction = viaLocal()

func viaLocal() string {
	m := clierr.Rancher
	return m + " access clusters"
}

var FromNestedFunction = outer()

func outer() string { return remedyText() + " --for x" }

var PlainFromFunction = plainText()

func plainText() string { return "rancher access clusters" }

var textFn = remedyText

var FromFunctionValue = textFn()

type hints struct{}

func (hints) text() string { return clierr.Rancher + " access clusters" }

var FromMethod = hints{}.text()

var FromNamedResult = named()

func named() (s string) {
	s = clierr.Rancher + " access clusters"
	return
}

var Parts = []string{clierr.Rancher + " access clusters"}

var ByName = map[string]string{"access": clierr.Rancher + " access clusters"}

var Set = struct{ Access string }{clierr.Rancher + " access clusters"}

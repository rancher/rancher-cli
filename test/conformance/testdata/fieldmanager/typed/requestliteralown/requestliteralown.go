package requestliteralown

type own struct {
	Method string
}

var _ = own{Method: "POST"}

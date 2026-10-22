package fixture

func NewRequest(method string) string {
	return method
}

var _ = NewRequest("POST")

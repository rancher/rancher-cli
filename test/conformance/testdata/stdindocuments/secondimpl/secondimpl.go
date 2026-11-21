package secondimpl

// second is a second source of documents.
type second struct{}

// Read returns a document.
func (second) Read(path string) ([]byte, error) { return []byte(path), nil }

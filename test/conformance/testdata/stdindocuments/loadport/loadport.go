package loadport

// port is a source of documents under another name.
type port interface {
	Load(path string) ([]byte, error)
}

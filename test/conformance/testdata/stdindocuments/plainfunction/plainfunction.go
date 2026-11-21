package plainfunction

// Read is a plain function with the signature (string) ([]byte, error).
func Read(path string) ([]byte, error) { return []byte(path), nil }

// Load calls it.
func Load() string {
	b, _ := Read("x")
	return string(b)
}

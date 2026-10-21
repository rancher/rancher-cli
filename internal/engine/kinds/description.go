package kinds

// DescriptionPaths returns the field paths that describe an object of the
// kind beside its name in completion: the fields a person knows it by, the
// row's Handle.Lookup paths in order. A kind without a Handle has none.
func (k Kind) DescriptionPaths() [][]string {
	if k.Handle == nil {
		return nil
	}
	paths := make([][]string, len(k.Handle.Lookup))
	for i, f := range k.Handle.Lookup {
		paths[i] = f.Path
	}
	return paths
}

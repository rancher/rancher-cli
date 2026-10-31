package joined

func header(v string) string {
	return "# " + v + "\n"
}

func note(v string) string {
	return "# note: " + "with " + v
}

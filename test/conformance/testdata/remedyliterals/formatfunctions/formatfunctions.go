package formatfunctions

import "io"

func logf(format string, args ...any) {}

func other(text string, args ...any) {}

func command(w io.Writer, name string) {
	logf("rancher %s", name)
	other("see rancher %s", name)
	say := func(w io.Writer, format string, args ...any) {}
	say(w, "then rancher %s", name)
}

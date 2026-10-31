package nested

import (
	"fmt"
	"io"
	"os"
)

const name = "x"

const fixedHeader = "# fixed header\n"

type holder struct{ p, v string }

func parameter(name string) string {
	return "# parameter " + name
}

func callResult(v string) string {
	return fmt.Sprint("# call result ") + v
}

func sliced(v string) string {
	return "# sliced text"[:8] + v
}

func keyed(v string) string {
	h := holder{p: "# keyed ", v: v}
	return h.p + h.v
}

func written(w io.Writer) {
	fmt.Fprint(w, "# written\n")
}

func file(p string) error {
	return os.WriteFile(p, []byte(fixedHeader), 0o600)
}

func variable(v string) string {
	h := "# variable "
	return h + v
}

func literalArgument(p string) error {
	return os.WriteFile(p, []byte("# literal argument\n"), 0o600)
}

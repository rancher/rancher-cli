package formatargument

import (
	"fmt"
	"io"
)

func command(w io.Writer) {
	_ = fmt.Sprintf("%s get x", "rancher")
	fmt.Fprintf(w, "%s get y", "rancher ")
}

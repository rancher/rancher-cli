package printfamily

import (
	"fmt"
	"io"
)

func command(w io.Writer, n string) {
	_ = fmt.Sprint("rancher", " inspect cluster ", n)
	fmt.Fprint(w, "rancher ", n)
	fmt.Println("then rancher")
	_ = fmt.Sprint("see rancher %s", n)
}

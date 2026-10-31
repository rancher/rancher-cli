package printmarker

import (
	"fmt"
	"io"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func command(w io.Writer, n string) {
	_ = fmt.Sprint(clierr.Rancher, "get", " users")
	_ = fmt.Sprint(clierr.Rancher, " get users")
	_ = fmt.Sprintln(clierr.Rancher, "list")
	fmt.Fprint(w, clierr.Rancher, "fetch")
	_ = fmt.Sprint(clierr.Rancher, n)
}

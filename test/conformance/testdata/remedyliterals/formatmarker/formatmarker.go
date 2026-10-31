package formatmarker

import (
	"fmt"
	"io"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func command(w io.Writer, name string) {
	_ = fmt.Sprintf("%sget users", clierr.Rancher)
	_ = fmt.Sprintf("%s get users", clierr.Rancher)
	_ = fmt.Sprintf("enable with: %s", clierr.Rancher)
	_ = fmt.Sprintf("%s: %s get %s", name, clierr.Rancher, name)
	_ = fmt.Sprintf("%s %sget %s", name, clierr.Rancher, name)
	_ = fmt.Sprintf("%[1]sget", clierr.Rancher)
	_ = fmt.Sprintf("%s"+"get roletemplates", clierr.Rancher)
	_ = fmt.Sprintf("100%% %sget clusters", clierr.Rancher)
	fmt.Fprintf(w, "%sget tokens", clierr.Rancher)
	_ = fmt.Sprintf(name+"%sget", clierr.Rancher)
}

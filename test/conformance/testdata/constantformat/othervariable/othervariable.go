package othervariable

import "fmt"

// StatusCommand has the name of a variable of internal/config and is not it.
var StatusCommand = "rancher config status"

func warning(entry string) string {
	return fmt.Sprintf("listed by: "+StatusCommand+" (%s)", entry)
}

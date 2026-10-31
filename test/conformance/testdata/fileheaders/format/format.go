package format

import "fmt"

func header(v string) string {
	return fmt.Sprintf("# generated for %s\n", v)
}

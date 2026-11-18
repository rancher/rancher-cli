package pathprojects

import "fmt"

func path(cluster, id string) string {
	return fmt.Sprintf("/v1/management.cattle.io.projects/%s/%s", cluster, id)
}

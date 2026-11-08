package fixture

// handPath builds an index link path by hand.
func handPath(repo string) string {
	return "/v1/catalog.cattle.io.clusterrepos/" + repo + "?link=index"
}

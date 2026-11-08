package fixture

// clean mentions the words in comments and identifiers only:
// link=index, skipFilter and k8sVersion.
func clean(link string) string {
	skipFilter := "x"
	return link + skipFilter
}

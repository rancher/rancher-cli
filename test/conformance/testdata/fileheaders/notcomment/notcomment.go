package notcomment

var texts = []string{"a # b", "see #3"}

func trailing(v string) string {
	return "key: value # " + v
}

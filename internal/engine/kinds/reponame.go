package kinds

import "k8s.io/apimachinery/pkg/util/validation"

// ClusterRepoName reports whether v is a name the API server accepts for a
// ClusterRepo, a custom resource: a DNS subdomain of at most 253 characters,
// each part between dots made of lowercase letters, digits and "-", starting
// and ending with a letter or digit.
func ClusterRepoName(v string) bool {
	return len(validation.IsDNS1123Subdomain(v)) == 0
}

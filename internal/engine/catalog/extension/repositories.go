// Package extension holds the rules for Rancher UI extensions that need no
// request: the two repositories the Rancher UI adds, the rule that a chart is
// an extension, whether a version of an extension is compatible with a server,
// and how an installed extension pairs with its chart. It reads nothing from a
// server and from no file, which a conformance test asserts through its
// imports, so a command and its completion can both call it.
package extension

import "slices"

// Repository is a Rancher extension repository: a ClusterRepo the Rancher UI's
// Extensions page adds from a git repository.
type Repository struct {
	// Name is the ClusterRepo's metadata.name.
	Name string `json:"name"`
	// URL is the ClusterRepo's spec.gitRepo.
	URL string `json:"url"`
	// Branch is the ClusterRepo's spec.gitBranch.
	Branch string `json:"branch"`
	// Label is the text of the Rancher UI's checkbox for the repository.
	Label string `json:"label"`
	// PrimeOnly is true when the Rancher UI offers the repository only on
	// Rancher Prime.
	PrimeOnly bool `json:"primeOnly"`
}

// repositories is the table of the two repositories, with the names, URLs and
// branch the Rancher UI uses. The Rancher UI knows a third repository,
// community-extensions, and never adds it, so it is not here.
var repositories = []Repository{
	{
		Name:      "rancher-ui-plugins",
		URL:       "https://github.com/rancher/ui-plugin-charts",
		Branch:    "main",
		Label:     "Add Rancher Prime Extensions Repository",
		PrimeOnly: true,
	},
	{
		Name:   "partner-extensions",
		URL:    "https://github.com/rancher/partner-extensions",
		Branch: "main",
		Label:  "Add Partners Extensions Repository",
	},
}

// Repositories returns the two Rancher extension repositories, the Rancher
// Prime one first.
func Repositories() []Repository { return slices.Clone(repositories) }

// RepositoryNamed returns the Rancher extension repository with the given
// ClusterRepo name.
func RepositoryNamed(name string) (Repository, bool) {
	for _, r := range repositories {
		if r.Name == name {
			return r, true
		}
	}
	return Repository{}, false
}

// ClusterRepo holds the fields of an existing ClusterRepo that tell where it
// reads charts from.
type ClusterRepo struct {
	Name      string
	StatusURL string
	GitRepo   string
	URL       string
}

// DisplayURL is the address the Rancher UI shows for the ClusterRepo: the
// first of status.url, spec.gitRepo and spec.url that is not empty.
func (r ClusterRepo) DisplayURL() string {
	for _, u := range []string{r.StatusURL, r.GitRepo, r.URL} {
		if u != "" {
			return u
		}
	}
	return ""
}

// AlreadyAdded returns the first ClusterRepo whose DisplayURL equals url
// exactly, as the Rancher UI decides that a repository is already added: a
// trailing slash or a ".git" suffix makes a different address.
func AlreadyAdded(repos []ClusterRepo, url string) (ClusterRepo, bool) {
	if url == "" {
		return ClusterRepo{}, false
	}
	for _, r := range repos {
		if r.DisplayURL() == url {
			return r, true
		}
	}
	return ClusterRepo{}, false
}

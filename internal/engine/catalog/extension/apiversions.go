package extension

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// apiVersionsJSON is the table from a Rancher release to the extensions API
// version of the Rancher UI it ships. It is written by scripts/extension-sources
// and holds one row per release of each supported minor.
//
//go:embed extensions-api-versions.json
var apiVersionsJSON []byte

// APIVersionRow is one row of the table: a Rancher release tag, the dashboard
// tag its image names, and the extensions API version of that dashboard.
type APIVersionRow struct {
	Rancher       string `json:"rancher"`
	Dashboard     string `json:"dashboard"`
	ExtensionsAPI string `json:"extensionsApi"`
}

var apiVersionRows = sync.OnceValue(func() map[string]APIVersionRow {
	var rows []APIVersionRow
	if err := json.Unmarshal(apiVersionsJSON, &rows); err != nil {
		panic("extension: the extensions API version table is not valid JSON: " + err.Error())
	}
	byTag := make(map[string]APIVersionRow, len(rows))
	for _, r := range rows {
		byTag[r.Rancher] = r
	}
	return byTag
})

// APIVersions returns every row of the table.
func APIVersions() []APIVersionRow {
	rows := make([]APIVersionRow, 0, len(apiVersionRows()))
	for _, r := range apiVersionRows() {
		rows = append(rows, r)
	}
	return rows
}

// ExtensionsAPIVersion returns the extensions API version of the Rancher UI
// that the Rancher release with the given version, such as v2.15.3, ships. The
// version must be a release tag of the table: a head, release candidate or
// newer patch version is not found, because the version can change inside a
// minor.
func ExtensionsAPIVersion(rancherVersion string) (string, bool) {
	r, ok := apiVersionRows()[rancherVersion]
	if !ok || r.ExtensionsAPI == "" {
		return "", false
	}
	return r.ExtensionsAPI, true
}

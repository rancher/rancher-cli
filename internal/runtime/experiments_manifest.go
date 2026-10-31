package runtime

import "github.com/rancher/rancher-cli/internal/experiment"

// SchemaExperiment is one experiment of the manifest, from the table in
// internal/experiment.
type SchemaExperiment struct {
	Name      string `json:"name"`
	Desc      string `json:"desc"`
	ValueForm string `json:"valueForm"`
	DevOnly   bool   `json:"devOnly"`
}

// experimentsManifest lists the experiment table for the manifest. The list
// is never nil.
func experimentsManifest() []SchemaExperiment {
	out := []SchemaExperiment{}
	for _, e := range experiment.Table() {
		out = append(out, SchemaExperiment{Name: e.Name, Desc: e.Description, ValueForm: e.ValueForm, DevOnly: e.DevOnly})
	}
	return out
}

package kinds

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
)

// SteveTableBuilder builds the Table the cluster's API server would send for
// objs, a list of one kind read through the Steve API, with the columns and
// the cells the server's own printer writes. now is the clock of the AGE cell.
// Each row carries its object, which the project filter of a get reads.
type SteveTableBuilder func(objs []unstructured.Unstructured, now time.Time) *metav1.Table

// steveTableBuilders maps the key of each kind that sets both ServerTable and
// SteveReadOnForbidden to its builder: the server refused the Table request,
// and Steve returns objects, not a Table. validateKinds refuses a row that
// sets both and has no entry.
var steveTableBuilders = map[string]SteveTableBuilder{
	"namespace": namespaceTable,
}

// downstreamSteveReadKeys are the keys of the downstream kinds that have a
// Steve read and may set SteveReadOnForbidden. validateKinds refuses the flag
// on any other downstream kind.
var downstreamSteveReadKeys = []string{"namespace"}

// SteveTable returns the table builder of the kind with key, and false when
// the kind has none.
func SteveTable(key string) (SteveTableBuilder, bool) {
	b, ok := steveTableBuilders[key]
	return b, ok
}

// namespaceTable is the table of the API server's namespace printer: the
// columns Name, Status and Age, all of priority 0; the cells are the name,
// status.phase as it is, and the age of metadata.creationTimestamp in the form
// of duration.HumanDuration. A namespace with no creation time, which Steve
// returns for a namespace the caller cannot get, has a blank age, as the age
// column of the tables get builds from objects does (resources.ageCell).
func namespaceTable(objs []unstructured.Unstructured, now time.Time) *metav1.Table {
	t := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string"},
			{Name: "Age", Type: "string"},
		},
		Rows: make([]metav1.TableRow, 0, len(objs)),
	}
	t.APIVersion, t.Kind = "meta.k8s.io/v1", "Table"
	for i := range objs {
		obj := objs[i].DeepCopy()
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		t.Rows = append(t.Rows, metav1.TableRow{
			Cells:  []any{obj.GetName(), phase, ageSince(obj, now)},
			Object: runtime.RawExtension{Object: obj},
		})
	}
	return t
}

// ageSince is the age of obj's metadata.creationTimestamp at now, blank when
// the timestamp is missing or is not an RFC 3339 time.
func ageSince(obj *unstructured.Unstructured, now time.Time) string {
	ts, _, _ := unstructured.NestedString(obj.Object, "metadata", "creationTimestamp")
	if ts == "" {
		return ""
	}
	created, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return duration.HumanDuration(now.Sub(created))
}

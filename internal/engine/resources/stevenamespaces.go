package resources

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// steveTableAnswer is the table of items, read through the Steve API for a row
// that sets ServerTable and SteveReadOnForbidden, built with the row's table
// builder: the server refused the Table request, and Steve returns objects.
// A row with no builder, which validateKinds refuses, gets the objects alone.
func (c getCmd) steveTableAnswer(items []unstructured.Unstructured) engine.TableAnswer {
	build, ok := kinds.SteveTable(c.k.Key)
	if !ok {
		return engine.TableAnswer{Items: items}
	}
	return engine.TableAnswer{Table: build(items, now())}
}

// steveListAnswer finishes a list of namespaces read through the Steve API,
// already narrowed to the project scope. Steve returns an item with a few
// fields only for a namespace the caller cannot get, and the list says what
// it has: without --selector the item stays and is counted; with it the item
// is left out and counted, because its labels are not readable. A list
// without --project also says that the Kubernetes API refused the list of
// every namespace, and names the command that lists the projects; with
// --project the list is the project's namespaces, as the Kubernetes API's
// list is. Every other row returns items as they are.
func (c getCmd) steveListAnswer(items []unstructured.Unstructured, sel labels.Selector, cluster string, scope *ProjectScope) ([]unstructured.Unstructured, []string) {
	if c.k.Key != "namespace" {
		return items, nil
	}
	kept := make([]unstructured.Unstructured, 0, len(items))
	var shownFewFields, leftOut int
	for i := range items {
		switch full := steve.ReadableInFull(&items[i]); {
		case full && (sel == nil || sel.Matches(labels.Set(items[i].GetLabels()))):
			kept = append(kept, items[i])
		case !full && sel == nil:
			kept = append(kept, items[i])
			shownFewFields++
		case !full:
			leftOut++
		}
	}
	var warnings []string
	if scope == nil {
		warnings = append(warnings, clierr.Text(
			"the Kubernetes API refused the list of every namespace (403); showing the namespaces the Steve API lists for you",
			clierr.Lead("; list the projects you can see with "),
			clierr.NewCommand("access projects").FlagValue("--cluster", cluster)))
	}
	if shownFewFields > 0 {
		warnings = append(warnings, fewFieldsWarning(shownFewFields))
	}
	if leftOut > 0 {
		warnings = append(warnings, leftOutWarning(leftOut))
	}
	return kept, warnings
}

// fewFieldsWarning counts the namespaces shown with the fields the Steve API
// returns for a namespace the caller has no permission to get.
func fewFieldsWarning(n int) string {
	if n == 1 {
		return "1 namespace is shown with only the fields the Steve API returns for a namespace you have no permission to get"
	}
	return fmt.Sprintf("%d namespaces are shown with only the fields the Steve API returns for a namespace you have no permission to get", n)
}

// leftOutWarning counts the namespaces a --selector list leaves out because
// the Steve API returns only a few fields for them.
func leftOutWarning(n int) string {
	if n == 1 {
		return "1 namespace is left out of the list: the Steve API returns only a few fields for a namespace you have no permission to get, and --selector needs its labels"
	}
	return fmt.Sprintf("%d namespaces are left out of the list: the Steve API returns only a few fields for a namespace you have no permission to get, and --selector needs their labels", n)
}

// readThroughSteve reports whether client, a client wrapSteveWait returned,
// has read an object through the Steve API.
func readThroughSteve(client engine.ClusterClient) bool {
	r, ok := client.(interface{ readThroughSteve() bool })
	return ok && r.readThroughSteve()
}

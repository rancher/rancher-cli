package access

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestAccessTextKeepsTheMarkerOfEveryMessage asserts each place the text
// output prints a message field, an issue's message, a row's note or the
// note of an explain tree node, keeps the remedy marker, so the invocation
// can name its config directory in a command the message names.
func TestAccessTextKeepsTheMarkerOfEveryMessage(t *testing.T) {
	t.Parallel()
	message := clierr.Rancher + " inspect cluster x"
	issue := &Issue{Code: "NotFound", Message: message}
	bindingIssues := []stageIssue{{Stage: "prtb", Issue: Issue{Code: "NotFound", Message: message}}}
	refusedCluster := clusterRow{Cluster: "c-1", Access: "refused", Issue: issue}
	cases := []struct {
		name string
		text func() string
	}{
		{"can namespace issue", func() string {
			return canText(canResult{Scope: Scope{Cluster: "local"}, Namespaces: []canNamespace{{Namespace: "ns", Issue: issue}}}, false)
		}},
		{"explain chain issue", func() string {
			return explainText(explainResult{Scope: Scope{Cluster: "local"}, ChainIssues: []chainIssue{{Stage: stageManagementRead, Code: "NotFound", Message: message}}})
		}},
		{"explain tree note", func() string {
			return explainText(explainResult{Scope: Scope{Cluster: "local"}, Tree: &chainNode{Kind: "ClusterRoleBinding", Name: "x", Note: message}})
		}},
		{"list namespace issue", func() string {
			return listText(listResult{Scope: Scope{Cluster: "local"}, Namespaces: []listNamespace{{Namespace: "ns", Issue: issue}}}, false)
		}},
		{"list wide namespace issue", func() string {
			return listWideText(listResult{Scope: Scope{Cluster: "local"}, Namespaces: []listNamespace{{Namespace: "ns", Issue: issue}}})
		}},
		{"clusters bindings issue", func() string {
			return clustersText(clustersResult{BindingsDiscovery: bindingsDiscovery{Status: "partial", Issues: bindingIssues}}, false)
		}},
		{"projects namespace discovery issue", func() string {
			row := projectsClusterRow{NamespaceDiscovery: &namespaceDiscovery{Issue: issue}}
			row.Cluster, row.Access = "c-1", reachable
			return projectsText(projectsResult{Clusters: []projectsClusterRow{row}}, false)
		}},
		{"projects membership issue", func() string {
			return projectsText(projectsResult{BindingsDiscovery: bindingsDiscovery{Status: "complete", Issues: bindingIssues}}, false)
		}},
		{"projects bindings issue", func() string {
			return projectsText(projectsResult{MemberOf: &memberOfSet{}, BindingsDiscovery: bindingsDiscovery{Status: "partial", Issues: bindingIssues}}, false)
		}},
		{"projects row note", func() string {
			row := projectsClusterRow{Projects: []projectRow{{Project: "p-1", Note: message}}}
			row.Cluster, row.Access = "c-1", reachable
			return projectsText(projectsResult{Clusters: []projectsClusterRow{row}}, false)
		}},
		{"clusters table cell", func() string {
			return clustersText(clustersResult{Clusters: []clusterRow{refusedCluster}}, false)
		}},
		{"projects table cell", func() string {
			return projectsText(projectsResult{Clusters: []projectsClusterRow{{clusterRow: refusedCluster}}}, false)
		}},
		{"who-can table cell", func() string {
			return whoCanText(subjectsResult{Scope: Scope{Cluster: "local"}, Subjects: []subjectRow{{Kind: KindGroup, Name: "g", Decision: DecisionUnknown, Issue: issue}}}, false, false)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, tc.text(), clierr.Rancher)
		})
	}
}

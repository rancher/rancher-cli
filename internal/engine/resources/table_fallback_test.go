package resources

import (
	"maps"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func bindingObj(name string, fields map[string]any) unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "ClusterRoleTemplateBinding",
		"metadata":   map[string]any{"name": name},
	}
	maps.Copy(obj, fields)
	return unstructured.Unstructured{Object: obj}
}

func TestColumnAltPathsFirstNonEmptyWins(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{
		Name: "clusterroletemplatebinding",
		Columns: []kinds.Column{{
			Header:   "MEMBER",
			Path:     []string{"userName"},
			AltPaths: [][]string{{"groupPrincipalName"}, {"userPrincipalName"}},
		}},
	}
	tbl := newTable(k, []unstructured.Unstructured{
		bindingObj("b-user", map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice"}),
		bindingObj("b-group", map[string]any{"groupPrincipalName": "github_org://8341"}),
		bindingObj("b-provisioned", map[string]any{"userName": "", "userPrincipalName": "okta_user://pending"}),
		bindingObj("b-none", map[string]any{}),
	}, false)

	assert.Equal(t, []string{"NAME", "MEMBER"}, tbl.Columns())
	rows := tbl.Rows()
	assert.Equal(t, [][]string{
		{"b-user", "u-alice"},
		{"b-group", "github_org://8341"},
		{"b-provisioned", "okta_user://pending"},
		{"b-none", ""},
	}, rows)
}

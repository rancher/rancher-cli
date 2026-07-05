package provider

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestMapErr(t *testing.T) {
	gr := schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"nil", nil, ""},
		{"notfound", apierrors.NewNotFound(gr, "p-x"), clierr.CodeNotFound},
		{"forbidden", apierrors.NewForbidden(gr, "p-x", errors.New("nope")), clierr.CodeRBAC},
		{"unauthorized", apierrors.NewUnauthorized("bad token"), clierr.CodeAuth},
		{"conflict", apierrors.NewConflict(gr, "p-x", errors.New("rv")), clierr.CodeConflict},
		{"exists", apierrors.NewAlreadyExists(gr, "p-x"), clierr.CodeConflict},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: gr.Group, Kind: "Project"}, "p-x", nil), clierr.CodeValidation},
		{"badrequest", apierrors.NewBadRequest("bad"), clierr.CodeValidation},
		{"other", errors.New("boom"), clierr.CodeGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.err)
			if tc.code == "" {
				assert.NoError(t, got)
				return
			}
			var ce *clierr.Error
			require.ErrorAs(t, got, &ce)
			assert.Equal(t, tc.code, ce.Code)
		})
	}
}

package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSafeConcatNameMatchesWrangler asserts the copy of wrangler's rule
// on the four shapes that matter: a short name is the plain join, a join
// of exactly 63 characters is kept, a long name is cut to 57 characters
// plus a five-hex digest (the key Rancher 2.14.5 wrote for the CRTB
// c-m-g9vv28t9-fleet-default-owner), and a cut that would end on a "-" is
// cut one shorter with a six-hex digest (both expectations computed with
// wrangler's rule).
func TestSafeConcatNameMatchesWrangler(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		parts []string
		want  string
	}{
		"short":                 {[]string{"authz.cluster.cattle.io/crtb-owner", "crtb-dx6zh"}, "authz.cluster.cattle.io/crtb-owner-crtb-dx6zh"},
		"exactly 63 is kept":    {[]string{"authz.cluster.cattle.io/prtb-owner", "prtb-abcdefghijklmnopqrstuvw"}, "authz.cluster.cattle.io/prtb-owner-prtb-abcdefghijklmnopqrstuvw"},
		"truncated five hex":    {[]string{"authz.cluster.cattle.io/crtb-owner", "c-m-g9vv28t9-fleet-default-owner"}, "authz.cluster.cattle.io/crtb-owner-c-m-g9vv28t9-fleet-def-f6563"},
		"cut on a dash six hex": {[]string{"authz.cluster.cattle.io/crtb-owner", "abcdefghijklmnopqrstu-vwxyz0123456789"}, "authz.cluster.cattle.io/crtb-owner-abcdefghijklmnopqrstu-7a527f"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SafeConcatName(tc.parts...))
		})
	}
}

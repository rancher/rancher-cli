package rbac

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SafeConcatName joins parts with "-" and keeps the result under 64
// characters the way Rancher does when it names the owner labels it puts
// on the bindings it creates on a downstream cluster
// (authz.cluster.cattle.io/crtb-owner-<name>, prtb-owner-<name>). A copy
// of github.com/rancher/wrangler/v3/pkg/name.SafeConcatName, and it must
// stay byte for byte equal to it: the traversal recomputes the key from
// a candidate binding's name to match the label the server wrote, so any
// difference in the cut point or the digest would make a real Rancher
// label unmatchable. Do not "improve" it. The rule: under 64 characters
// the join is returned as is; otherwise the first 57 characters are kept
// and the last six are "-" plus five hex digits of the SHA-256 of the
// full join, except when the character at index 56 is not a lowercase
// letter or digit (an invalid last character for a Kubernetes name), in
// which case 56 characters are kept and six hex digits follow the "-".
func SafeConcatName(parts ...string) string {
	full := strings.Join(parts, "-")
	if len(full) < 64 {
		return full
	}
	digest := hex.EncodeToString(func() []byte { d := sha256.Sum256([]byte(full)); return d[:] }())
	c := full[56]
	if 'a' <= c && c <= 'z' || '0' <= c && c <= '9' {
		return full[:57] + "-" + digest[:5]
	}
	return full[:56] + "-" + digest[:6]
}

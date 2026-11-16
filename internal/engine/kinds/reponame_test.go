package kinds

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClusterRepoNameTable asserts, with literal expectations, which names
// the API server accepts for a ClusterRepo and which it refuses.
func TestClusterRepoNameTable(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"a", strings.Repeat("a", 253), "0abc", "a.b", "a.b-c", "rancher-charts",
	}
	refused := []string{
		"", "A", "a_b", "a/b", "a b", "..", "a..b", "a.-b", "a-.b", "-a", "a-", "a.",
		strings.Repeat("a", 254), "a\u200bb", "a\rb", `a\b`,
	}
	for _, v := range accepted {
		assert.True(t, ClusterRepoName(v), "accepted: %q", v)
	}
	for _, v := range refused {
		assert.False(t, ClusterRepoName(v), "refused: %q", v)
	}
}

// TestClusterRepoNameGeneratedNames asserts that generated names are accepted
// when every part between dots is made of lowercase letters, digits and "-"
// and starts and ends with a letter or digit, and that the same name with an
// upper case letter or a slash added is refused.
func TestClusterRepoNameGeneratedNames(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	const middle = alnum + "-"
	pick := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[rng.IntN(len(set))]
		}
		return string(b)
	}
	for range 200 {
		var parts []string
		for range 1 + rng.IntN(3) {
			part := pick(alnum, 1)
			if n := rng.IntN(8); n > 0 {
				part += pick(middle, n) + pick(alnum, 1)
			}
			parts = append(parts, part)
		}
		name := strings.Join(parts, ".")
		assert.True(t, ClusterRepoName(name), "accepted: %q", name)
		assert.False(t, ClusterRepoName(strings.ToUpper(name+"A")), "refused: %q", name)
		assert.False(t, ClusterRepoName(name+"/"+name), "refused: %q", name)
	}
}

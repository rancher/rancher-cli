//go:build !windows

package completions

// Deliberately plain-stdlib (no testify): these tests execute under a real
// external fish and are kept dependency-light on purpose.

import "testing"

// TestFishShowsTheDescriptionBesideTheToken asserts the script splits a
// candidate line at its first colon into the token and its description
// (fish shows them as "token<TAB>description"), keeps a colon inside the
// description, and prints a bare token as it is.
func TestFishShowsTheDescriptionBesideTheToken(t *testing.T) {
	t.Parallel()
	got := fishScriptOutput(t, []string{"rancher", "get", "user"}, "", []string{
		"u-abc12:alice (Alice Smith)",
		"u-col01:dev: build team",
		"u-plain",
	})
	want := []string{
		"u-abc12\talice (Alice Smith)",
		"u-col01\tdev: build team",
		"u-plain",
	}
	if !sliceEqual(got, want) {
		t.Errorf("fish output\n got:  %q\n want: %q", got, want)
	}
}

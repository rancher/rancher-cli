//go:build !windows

package completions

// Deliberately plain-stdlib (no testify): these tests execute under a real
// external bash and are kept dependency-light on purpose.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// bashListEntries runs the completer the way bash's second TAB does
// (COMP_TYPE=63) under bashBin against a fake rancher printing lines.
func bashListEntries(t *testing.T, bashBin string, cur string, lines []string) []string {
	t.Helper()
	return bashCompletionEntries(t, bashBin, []string{"rancher", "get", "user", cur}, 3, lines, bashDriverOpts{compType: "63"})
}

// TestBashListModeShowsDescriptions asserts the list the second TAB shows:
// a described candidate is "token  -- description" with the token padded to
// the longest token, a bare candidate is the token alone, a colon inside a
// description is kept, and only candidates that match the typed prefix are
// listed. The same input on the first TAB inserts the token alone. It runs
// under every bash, bash 3.2 included.
func TestBashListModeShowsDescriptions(t *testing.T) {
	t.Parallel()
	lines := []string{
		"u-abc12:alice (Alice Smith)",
		"u-col01:dev: build team",
		"u-plain",
		"u-x:x",
		"zz-other:other",
	}
	for _, bashBin := range nospaceBashes(t) {
		t.Run(strings.TrimPrefix(strings.ReplaceAll(bashBin, "/", "_"), "_"), func(t *testing.T) {
			t.Parallel()
			want := []string{
				"u-abc12   -- alice (Alice Smith)",
				"u-col01   -- dev: build team",
				"u-plain",
				"u-x       -- x",
			}
			// The token column is as wide as the longest token of the whole
			// answer, "zz-other", whether or not it matches the prefix.
			got := bashListEntries(t, bashBin, "u-", lines)
			if !sliceEqual(got, want) {
				t.Errorf("list under %s\n got:  %q\n want: %q", bashBin, got, want)
			}

			got = bashListEntries(t, bashBin, "u-c", lines)
			if want := []string{"u-col01   -- dev: build team"}; !sliceEqual(got, want) {
				t.Errorf("list with a typed prefix under %s\n got:  %q\n want: %q", bashBin, got, want)
			}

			first := bashCompletionEntries(t, bashBin, []string{"rancher", "get", "user", "u-"}, 3, lines, bashDriverOpts{})
			if want := []string{"u-abc12 ", "u-col01 ", "u-plain ", "u-x "}; !sliceEqual(first, want) {
				t.Errorf("first TAB under %s inserts tokens only\n got:  %q\n want: %q", bashBin, first, want)
			}
		})
	}
}

// TestBashListModeOfALargeAnswerIsLinear asserts the second TAB of a large
// live list costs the same per row as a small one: 2000 described
// candidates list in under two seconds of CPU time under every bash (CPU
// time, so a busy machine does not fail the test). A command substitution
// per row, a fork each, takes several times that.
func TestBashListModeOfALargeAnswerIsLinear(t *testing.T) {
	lines := make([]string, 2000)
	for i := range lines {
		lines[i] = fmt.Sprintf("u-%05d:user number %d (User %d)", i, i, i)
	}
	for _, bashBin := range nospaceBashes(t) {
		t.Run(strings.TrimPrefix(strings.ReplaceAll(bashBin, "/", "_"), "_"), func(t *testing.T) {
			var cpu time.Duration
			got := bashCompletionEntries(t, bashBin, []string{"rancher", "get", "user", "u-"}, 3, lines, bashDriverOpts{compType: "63", cpu: &cpu})
			if len(got) != len(lines) {
				t.Fatalf("listed %d entries under %s, want %d", len(got), bashBin, len(lines))
			}
			if want := "u-00000  -- user number 0 (User 0)"; got[0] != want {
				t.Errorf("first entry under %s\n got:  %q\n want: %q", bashBin, got[0], want)
			}
			if want := "u-01999  -- user number 1999 (User 1999)"; got[len(got)-1] != want {
				t.Errorf("last entry under %s\n got:  %q\n want: %q", bashBin, got[len(got)-1], want)
			}
			if cpu > 2*time.Second {
				t.Errorf("listing 2000 rows under %s used %s of CPU time, want under 2s", bashBin, cpu)
			}
		})
	}
}

// TestBashListModeSpawnsNoProcessPerRow asserts the list-mode code of the
// script holds no command substitution: each one forks a process per row,
// which made the second TAB of a large live list take seconds.
func TestBashListModeSpawnsNoProcessPerRow(t *testing.T) {
	t.Parallel()
	script := string(bashScript)
	_, rest, ok := strings.Cut(script, `"${COMP_TYPE:-}" == "63"`)
	if !ok {
		t.Fatal("the script has no COMP_TYPE=63 list mode")
	}
	body, _, ok := strings.Cut(rest, "\n    elif ")
	if !ok || !strings.Contains(body, "for ((") {
		t.Fatalf("cannot find the list-mode loop in the script; found:\n%s", body)
	}
	for _, banned := range []string{"$(", "`", "<("} {
		if strings.Contains(body, banned) {
			t.Errorf("list-mode code holds %q, which forks per row:\n%s", banned, body)
		}
	}
}

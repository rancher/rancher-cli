package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestActivateColor pins every row of the activation table, plus the named
// edge cases: empty NO_COLOR counts as unset, CLICOLOR_FORCE=0/FORCE_COLOR=0
// count as unset, and disagreeing force variables let the non-"0" one win.
func TestActivateColor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		colorFlag     string
		noColor       string
		cliColorForce string
		forceColor    string
		isTTY         bool
		want          bool
	}{
		// Row 1: --color=never is off under every NO_COLOR/force/TTY combination.
		{"never plain wins over TTY", "never", "", "", "", true, false},
		{"never wins over NO_COLOR unset and force set", "never", "", "1", "", true, false},
		{"never wins over NO_COLOR set", "never", "1", "", "", false, false},
		{"never wins over both force vars set", "never", "", "1", "1", true, false},

		// Row 2: --color=always is on under every NO_COLOR/force/TTY combination.
		{"always wins over NO_COLOR set, no TTY", "always", "1", "", "", false, true},
		{"always wins with nothing else set", "always", "", "", "", false, true},
		{"always wins over force vars set to 0", "always", "", "0", "0", false, true},

		// Row 3: absent/auto + NO_COLOR set non-empty is off regardless of force/TTY.
		{"auto off when NO_COLOR set, TTY true", "auto", "1", "", "", true, false},
		{"absent off when NO_COLOR set, force set, TTY true", "", "x", "1", "1", true, false},
		{"auto off when NO_COLOR set to any non-empty value", "auto", "0", "", "", true, false},

		// Row 4: absent/auto + NO_COLOR unset/empty + a force var set non-empty-non-0 is on.
		{"auto on via CLICOLOR_FORCE, no TTY", "auto", "", "1", "", false, true},
		{"absent on via FORCE_COLOR, no TTY", "", "", "", "1", false, true},
		{"auto on via CLICOLOR_FORCE with empty NO_COLOR, no TTY", "auto", "", "yes", "", false, true},

		// Row 5: absent/auto + NO_COLOR unset/empty + force unset/empty/0 tracks isTTY.
		{"auto on iff TTY: TTY true", "auto", "", "", "", true, true},
		{"auto on iff TTY: TTY false", "auto", "", "", "", false, false},
		{"absent on iff TTY: TTY true", "", "", "", "", true, true},
		{"absent on iff TTY: TTY false", "", "", "", "", false, false},

		// Empty NO_COLOR counts as unset.
		{"empty NO_COLOR does not suppress TTY-on", "auto", "", "", "", true, true},

		// CLICOLOR_FORCE=0 / FORCE_COLOR=0 count as unset.
		{"CLICOLOR_FORCE=0 counts as unset, no TTY -> off", "auto", "", "0", "", false, false},
		{"FORCE_COLOR=0 counts as unset, no TTY -> off", "auto", "", "", "0", false, false},
		{"both force vars 0 count as unset, TTY true -> on via TTY", "auto", "", "0", "0", true, true},

		// Disagreeing force variables: the non-"0" one wins, no error either way.
		{"CLICOLOR_FORCE=0 FORCE_COLOR=1 -> on", "auto", "", "0", "1", false, true},
		{"CLICOLOR_FORCE=1 FORCE_COLOR=0 -> on", "auto", "", "1", "0", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ActivateColor(tc.colorFlag, tc.noColor, tc.cliColorForce, tc.forceColor, tc.isTTY)
			assert.Equal(t, tc.want, got)
		})
	}
}

package safety

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		action Action
		want   bool
	}{
		{ActionCreate, true},
		{ActionUpdate, true},
		{ActionDelete, true},
		{ActionScale, true},
		{Action(""), false},
		{Action("destroy"), false},
	}
	for _, tt := range tests {
		if got := tt.action.Valid(); got != tt.want {
			t.Errorf("Action(%q).Valid() = %v, want %v", tt.action, got, tt.want)
		}
	}
}

func TestEffectJSONShape(t *testing.T) {
	t.Parallel()
	eff := Effect{
		Action: ActionDelete, Target: "projects/web",
		Server: "prod", Cluster: "c-abc",
		ResourceVersion: "42", ConflictDetection: true,
		Destructive: true, Tier: TierPublic, Stability: Stable,
	}
	b, err := json.Marshal(eff)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"action":"delete"`, `"server":"prod"`, `"cluster":"c-abc"`, `"tier":"public"`, `"stability":"stable"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("Effect JSON missing %s; got %s", key, b)
		}
	}
}

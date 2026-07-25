package fidelity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// ObservationResult is one observation's normalized recorded response.
type ObservationResult struct {
	Name        string         `json:"name"`
	Status      int            `json:"status"`
	ContentType string         `json:"contentType"`
	Body        map[string]any `json:"body,omitempty"`
}

// Golden is one probe's recorded truth. OracleImage is provenance — the
// literal image reference the recording ran against — and is deliberately
// excluded from Diff.
type Golden struct {
	OracleImage  string              `json:"oracleImage"`
	Observations []ObservationResult `json:"observations"`
}

// GoldenPath maps a probe name to its file under dir; the probe's "/" family
// separator becomes a subdirectory.
func GoldenPath(dir, probeName string) string {
	return filepath.Join(dir, filepath.FromSlash(probeName)+".json")
}

// LoadGolden reads one golden. A missing file satisfies
// errors.Is(err, os.ErrNotExist) so callers can distinguish not-yet-recorded
// from corrupt. The path is built by trusted callers from a fixed testdata
// root and a probe name — the suites legitimately address it via a "../.."
// relative path — never from external input, so it is read directly. The
// gosec G304 answer here is provenance, not a ".." reject that would break
// those relative paths.
func LoadGolden(path string) (Golden, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Golden{}, fmt.Errorf("loading golden: %w", err)
	}
	var g Golden
	if err := json.Unmarshal(raw, &g); err != nil {
		return Golden{}, fmt.Errorf("decoding golden %s: %w", path, err)
	}
	return g, nil
}

// SaveGolden writes one golden, creating parent directories. The 0755/0644
// permissions are deliberate: goldens are committed, non-secret repository
// files (credentials are scrubbed by the normalizer before they are written),
// so world-readable perms are correct, not a leak.
func SaveGolden(path string, g Golden) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating golden dir: %w", err)
	}
	raw, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding golden %s: %w", path, err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Diff reports behavioral mismatches between two recordings, one string per
// finding. OracleImage is provenance, not behavior, and is ignored.
func Diff(want, got Golden) []string {
	var diffs []string
	if len(want.Observations) != len(got.Observations) {
		return []string{fmt.Sprintf("observation count: want %d, got %d", len(want.Observations), len(got.Observations))}
	}
	for i, w := range want.Observations {
		g := got.Observations[i]
		if w.Name != g.Name {
			diffs = append(diffs, fmt.Sprintf("observation %d name: want %q, got %q", i, w.Name, g.Name))
			continue
		}
		if w.Status != g.Status {
			diffs = append(diffs, fmt.Sprintf("%s status: want %d, got %d", w.Name, w.Status, g.Status))
		}
		if w.ContentType != g.ContentType {
			diffs = append(diffs, fmt.Sprintf("%s contentType: want %q, got %q", w.Name, w.ContentType, g.ContentType))
		}
		if !reflect.DeepEqual(w.Body, g.Body) {
			wantJSON, _ := json.Marshal(w.Body)
			gotJSON, _ := json.Marshal(g.Body)
			diffs = append(diffs, fmt.Sprintf("%s body: want %s, got %s", w.Name, wantJSON, gotJSON))
		}
	}
	return diffs
}

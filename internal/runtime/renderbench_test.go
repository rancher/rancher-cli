package runtime

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// largeList is the data of a list result: n pod-shaped objects with eight
// labels and three containers each, holding no remedy command.
func largeList(n int) []any {
	items := make([]any, n)
	for i := range items {
		labels := map[string]any{}
		for l := range 8 {
			labels[fmt.Sprintf("label-%d", l)] = fmt.Sprintf("value-%d-%d", i, l)
		}
		containers := make([]any, 3)
		for c := range containers {
			containers[c] = map[string]any{
				"name":            fmt.Sprintf("container-%d", c),
				"image":           fmt.Sprintf("registry.example.com/team/app-%d:1.%d.0", c, i%10),
				"imagePullPolicy": "IfNotPresent",
				"restartCount":    i % 3,
				"ready":           true,
			}
		}
		items[i] = map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":              fmt.Sprintf("pod-%05d", i),
				"namespace":         fmt.Sprintf("ns-%d", i%20),
				"uid":               fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
				"resourceVersion":   fmt.Sprintf("%d", 100000+i),
				"creationTimestamp": "2026-10-01T12:00:00Z",
				"labels":            labels,
			},
			"spec": map[string]any{
				"nodeName":   fmt.Sprintf("node-%d", i%50),
				"containers": containers,
			},
			"status": map[string]any{
				"phase": "Running",
				"podIP": fmt.Sprintf("10.%d.%d.%d", i%250, (i/250)%250, i%200),
			},
		}
	}
	return items
}

// BenchmarkRenderResultLargeList measures the json and yaml render of a
// result of 5000 pod-shaped objects that hold no remedy command, written to a
// buffer, for a config directory other than the default.
func BenchmarkRenderResultLargeList(b *testing.B) {
	res := Result{Kind: "List", Data: largeList(5000)}
	for _, format := range []string{"json", "yaml"} {
		b.Run(format, func(b *testing.B) {
			var out bytes.Buffer
			streams := IO{Out: &out, Err: io.Discard, Format: format, RemedyDir: b.TempDir()}
			for b.Loop() {
				out.Reset()
				if _, ce := renderResult(streams, Descriptor{Name: "get pods", CommandType: ReadOnly}, res); ce != nil {
					b.Fatal(ce)
				}
			}
		})
	}
}

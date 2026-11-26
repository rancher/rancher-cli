package clusterops

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipBase64 compresses raw and encodes it as Rancher stores the cluster spec.
func gzipBase64(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// metadataOf makes snapshotFile.metadata from the entries of its decoded map.
func metadataOf(t *testing.T, entries map[string]any) string {
	t.Helper()
	b, err := json.Marshal(entries)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// metadataWithSpec makes the metadata of a snapshot whose cluster spec is spec.
func metadataWithSpec(t *testing.T, spec string) string {
	t.Helper()
	return metadataOf(t, map[string]any{snapshotSpecKey: gzipBase64(t, []byte(spec))})
}

func snapshotWithMetadata(metadata string) map[string]any {
	return map[string]any{"snapshotFile": map[string]any{"metadata": metadata}}
}

// TestDecodeSnapshotSpecReads asserts the Kubernetes version and whether the
// spec has an RKE configuration, for an object, an empty object, null and an
// absent rkeConfig.
func TestDecodeSnapshotSpecReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, spec string
		want       SnapshotSpec
	}{
		{"object", `{"kubernetesVersion":"v1.30.5+rke2r1","rkeConfig":{"chartValues":{"x":1}}}`, SnapshotSpec{KubernetesVersion: "v1.30.5+rke2r1", HasRKEConfig: true}},
		{"empty object", `{"kubernetesVersion":"v1.30.5","rkeConfig":{}}`, SnapshotSpec{KubernetesVersion: "v1.30.5", HasRKEConfig: true}},
		{"null", `{"kubernetesVersion":"v1.30.5","rkeConfig":null}`, SnapshotSpec{KubernetesVersion: "v1.30.5"}},
		{"absent", `{"kubernetesVersion":"v1.30.5"}`, SnapshotSpec{KubernetesVersion: "v1.30.5"}},
		{"no version", `{"rkeConfig":{}}`, SnapshotSpec{HasRKEConfig: true}},
		{"other fields", `{"kubernetesVersion":"v1.30.5","cloudCredentialSecretName":["wrong type is read here"]}`, SnapshotSpec{KubernetesVersion: "v1.30.5"}},
	} {
		got := DecodeSnapshotSpec(snapshotWithMetadata(metadataWithSpec(t, tc.spec)))
		assert.Equal(t, tc.want, got, tc.name)
	}
}

// TestDecodeSnapshotSpecUnreadable asserts each way the metadata fails to
// decode is reported as unreadable, as Rancher fails to read the same value.
func TestDecodeSnapshotSpecUnreadable(t *testing.T) {
	t.Parallel()
	good := gzipBase64(t, []byte(`{"kubernetesVersion":"v1.30.5"}`))
	for name, snapshot := range map[string]map[string]any{
		"no snapshotFile":       {},
		"empty metadata":        snapshotWithMetadata(""),
		"bad base64":            snapshotWithMetadata("!!"),
		"not json":              snapshotWithMetadata(base64.StdEncoding.EncodeToString([]byte("nope"))),
		"no entry":              snapshotWithMetadata(metadataOf(t, map[string]any{"other": good})),
		"empty entry":           snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: ""})),
		"whitespace entry":      snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: " \n\t "})),
		"non-string value":      snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: good, "n": 1})),
		"entry bad base64":      snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: "!!"})),
		"entry not gzip":        snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: base64.StdEncoding.EncodeToString([]byte("plain"))})),
		"entry gzip not json":   snapshotWithMetadata(metadataWithSpec(t, "nope")),
		"version not a string":  snapshotWithMetadata(metadataWithSpec(t, `{"kubernetesVersion":1}`)),
		"rkeConfig not object":  snapshotWithMetadata(metadataWithSpec(t, `{"kubernetesVersion":"v1.30.5","rkeConfig":"x"}`)),
		"rkeConfig is an array": snapshotWithMetadata(metadataWithSpec(t, `{"kubernetesVersion":"v1.30.5","rkeConfig":[]}`)),
		"metadata not a string": {"snapshotFile": map[string]any{"metadata": 5}},
	} {
		assert.Equal(t, SnapshotSpec{State: SnapshotSpecUnreadable}, DecodeSnapshotSpec(snapshot), name)
	}
	truncated := gzipBase64(t, []byte(`{"kubernetesVersion":"v1.30.5"}`))
	raw, err := base64.StdEncoding.DecodeString(truncated)
	require.NoError(t, err)
	badTail := base64.StdEncoding.EncodeToString(raw[:len(raw)-6])
	assert.Equal(t, SnapshotSpec{State: SnapshotSpecUnreadable},
		DecodeSnapshotSpec(snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: badTail}))), "gzip cut short")
}

// specOfSize is a JSON cluster spec of exactly size bytes whose
// kubernetesVersion comes last, after padding.
func specOfSize(size int) []byte {
	const head, tail = `{"padding":"`, `","kubernetesVersion":"v1.30.5"}`
	return []byte(head + strings.Repeat("a", size-len(head)-len(tail)) + tail)
}

// TestDecodeSnapshotSpecBound asserts a spec of exactly 16 MiB reads and one
// byte more is too large.
func TestDecodeSnapshotSpecBound(t *testing.T) {
	t.Parallel()
	exact := DecodeSnapshotSpec(snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: gzipBase64(t, specOfSize(16<<20))})))
	assert.Equal(t, SnapshotSpec{KubernetesVersion: "v1.30.5"}, exact)
	over := DecodeSnapshotSpec(snapshotWithMetadata(metadataOf(t, map[string]any{snapshotSpecKey: gzipBase64(t, specOfSize(16<<20+1))})))
	assert.Equal(t, SnapshotSpec{State: SnapshotSpecTooLarge}, over)
}

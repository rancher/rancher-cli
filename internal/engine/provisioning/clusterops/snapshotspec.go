package clusterops

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
)

// maxSnapshotSpecBytes is the most this package reads of the decompressed
// cluster spec in an etcd snapshot object's metadata: 16 MiB. The spec Rancher
// stores there is the spec of one provisioning Cluster, far below that; the
// bound stops a compressed value that expands without end.
const maxSnapshotSpecBytes = 16 << 20

// snapshotSpecKey is the key of snapshotFile.metadata's decoded map that has
// the compressed cluster spec.
const snapshotSpecKey = "provisioning-cluster-spec"

// SnapshotSpecState says how far the cluster spec recorded in an etcd snapshot
// object could be read.
type SnapshotSpecState int

const (
	// SnapshotSpecRead: the spec decoded.
	SnapshotSpecRead SnapshotSpecState = iota
	// SnapshotSpecUnreadable: snapshotFile.metadata is absent, or it, its
	// provisioning-cluster-spec entry or the spec in the entry does not decode;
	// Rancher cannot read such a snapshot's spec either.
	SnapshotSpecUnreadable
	// SnapshotSpecTooLarge: the decompressed spec is over 16 MiB; this package
	// does not read it.
	SnapshotSpecTooLarge
)

// SnapshotSpec is what this package keeps of the cluster spec recorded in an
// etcd snapshot object: its Kubernetes version and whether it has an RKE
// configuration. Nothing else of the spec is kept.
type SnapshotSpec struct {
	State SnapshotSpecState
	// KubernetesVersion and HasRKEConfig are set only when State is
	// SnapshotSpecRead. HasRKEConfig is false for an absent or null rkeConfig.
	KubernetesVersion string
	HasRKEConfig      bool
}

// DecodeSnapshotSpec reads the cluster spec recorded in an etcd snapshot
// object's snapshotFile.metadata the way Rancher does: base64 of a JSON map
// of strings whose provisioning-cluster-spec entry is base64 of gzipped JSON.
// Unlike Rancher, which decodes the whole spec, it decodes only
// kubernetesVersion and rkeConfig; a spec with a wrongly typed field
// elsewhere still reads here.
func DecodeSnapshotSpec(snapshot map[string]any) SnapshotSpec {
	unreadable := SnapshotSpec{State: SnapshotSpecUnreadable}
	metadata := nestedString(snapshot, "snapshotFile", "metadata")
	if metadata == "" {
		return unreadable
	}
	outer, err := base64.StdEncoding.DecodeString(metadata)
	if err != nil {
		return unreadable
	}
	var entries map[string]string
	if err := json.Unmarshal(outer, &entries); err != nil {
		return unreadable
	}
	entry := entries[snapshotSpecKey]
	if strings.TrimSpace(entry) == "" {
		return unreadable
	}
	compressed, err := base64.StdEncoding.DecodeString(entry)
	if err != nil {
		return unreadable
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return unreadable
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, maxSnapshotSpecBytes+1))
	if len(raw) > maxSnapshotSpecBytes {
		return SnapshotSpec{State: SnapshotSpecTooLarge}
	}
	if err != nil {
		return unreadable
	}
	var spec struct {
		KubernetesVersion string          `json:"kubernetesVersion"`
		RKEConfig         json.RawMessage `json:"rkeConfig"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return unreadable
	}
	config := bytes.TrimSpace(spec.RKEConfig)
	switch {
	case len(config) == 0 || string(config) == "null":
		return SnapshotSpec{State: SnapshotSpecRead, KubernetesVersion: spec.KubernetesVersion}
	case config[0] == '{':
		return SnapshotSpec{State: SnapshotSpecRead, KubernetesVersion: spec.KubernetesVersion, HasRKEConfig: true}
	}
	return unreadable
}

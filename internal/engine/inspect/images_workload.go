package inspect

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// workloadIdentity is IM-9's full owner identity. UID distinguishes
// generations of a deleted-and-recreated controller: the recreated object
// does not own the old generation's pods, so collapsing them would
// overstate the effect of acting on the row (IM-17).
type workloadIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type imageWorkloadRow struct {
	Image     string           `json:"image"`
	Namespace string           `json:"namespace"`
	Workload  workloadIdentity `json:"workload"`
	Pods      int              `json:"pods"`
}

// imageWorkloadRows is `images --by workload`'s payload: one row per
// (image, namespace, owning workload object), pod-counted.
type imageWorkloadRows []imageWorkloadRow

// ownerKey memoizes resolution per first-level controller reference
// (IM-15): apiVersion and uid are part of the identity — same kind/name in
// another API group or a recreated generation is a different owner.
type ownerKey struct{ ns, apiVersion, kind, name, uid string }

type rowKey struct {
	image string
	ns    string
	id    workloadIdentity
}

// workloadImagesFor scans active pods, filters by needles, resolves each
// pod's owning workload (memoized per unique owner reference), and
// aggregates per-pod image sets by (image, namespace, workload identity).
func workloadImagesFor(ctx context.Context, client engine.ClusterClient, needles []string) (imageWorkloadRows, []string, error) {
	podKind, _ := kinds.Lookup("pod")
	list, err := client.List(ctx, podKind.GVR, "", "")
	if err != nil {
		return nil, nil, err
	}
	memo := map[ownerKey]workloadIdentity{}
	counts := map[rowKey]int{}
	var warnings []string
	for i := range list.Items {
		pod := &list.Items[i]
		if !podActive(pod.Object) {
			continue
		}
		imgSet := map[string]bool{}
		for _, img := range podImageRefs(pod.Object) {
			if matchesAny(img, needles) {
				imgSet[img] = true
			}
		}
		if len(imgSet) == 0 {
			continue
		}
		ns := pod.GetNamespace()
		id, warns := resolveWorkload(ctx, client, ns, pod, memo)
		warnings = append(warnings, warns...)
		for img := range imgSet {
			counts[rowKey{image: img, ns: ns, id: id}]++
		}
	}
	rows := make(imageWorkloadRows, 0, len(counts))
	for k, n := range counts {
		rows = append(rows, imageWorkloadRow{Image: k.image, Namespace: k.ns, Workload: k.id, Pods: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Image != b.Image {
			return a.Image < b.Image
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		ca := kindGroupName(a.Workload.APIVersion, a.Workload.Kind, a.Workload.Name)
		cb := kindGroupName(b.Workload.APIVersion, b.Workload.Kind, b.Workload.Name)
		if ca != cb {
			return ca < cb
		}
		return a.Workload.UID < b.Workload.UID
	})
	return rows, warnings, nil
}

// resolveWorkload maps a pod to its owning workload identity. Ownerless
// pods are their own workload (IM-16: apiVersion v1, the pod's own uid).
// The owner chain runs once per unique owner reference; a memo hit emits
// no repeat warnings, which is IM-16's warning dedup.
func resolveWorkload(ctx context.Context, client engine.ClusterClient, ns string, pod *unstructured.Unstructured, memo map[ownerKey]workloadIdentity) (workloadIdentity, []string) {
	ref, ok := controllerRef(pod)
	if !ok {
		return workloadIdentity{
			APIVersion: "v1", Kind: "pod",
			Name: pod.GetName(), UID: string(pod.GetUID()),
		}, nil
	}
	key := ownerKey{ns: ns, apiVersion: ref.apiVersion, kind: ref.kind, name: ref.name, uid: ref.uid}
	if id, hit := memo[key]; hit {
		return id, nil
	}
	owners, warns := ownerChain(ctx, client, ns, pod)
	// IM-16: any walk failure retains the FIRST-level reference — a
	// partially-walked chain's last element is an unverified guess.
	src := owners[len(owners)-1]
	if len(warns) > 0 {
		src = owners[0]
	}
	id := workloadIdentity{
		APIVersion: stringAt(src, "apiVersion"),
		Kind:       strings.ToLower(stringAt(src, "kind")),
		Name:       stringAt(src, "name"),
		UID:        stringAt(src, "uid"),
	}
	memo[key] = id
	return id, warns
}

func stringAt(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (r imageWorkloadRows) Columns() []string {
	return []string{"IMAGE", "NAMESPACE", "WORKLOAD", "PODS"}
}

func (r imageWorkloadRows) Rows() [][]string {
	suffix := generationSuffixes(r)
	out := make([][]string, 0, len(r))
	for _, row := range r {
		cell := kindGroupName(row.Workload.APIVersion, row.Workload.Kind, row.Workload.Name)
		if s, ok := suffix[row.Workload.UID]; ok {
			cell += s
		}
		out = append(out, []string{row.Image, row.Namespace, cell, strconv.Itoa(row.Pods)})
	}
	return out
}

type generationKey struct{ ns, apiVersion, kind, name string }

// generationSuffixes finds workload identities that collide when uid and
// image are ignored (IM-11): every generation of such an identity gets an
// @uid-prefix suffix, image-independent, so the old-image/new-image
// rollover case stays readable.
func generationSuffixes(rows imageWorkloadRows) map[string]string {
	uidsByIdentity := map[generationKey]map[string]bool{}
	for _, row := range rows {
		k := generationKey{row.Namespace, row.Workload.APIVersion, row.Workload.Kind, row.Workload.Name}
		if uidsByIdentity[k] == nil {
			uidsByIdentity[k] = map[string]bool{}
		}
		uidsByIdentity[k][row.Workload.UID] = true
	}
	out := map[string]string{}
	for _, uids := range uidsByIdentity {
		if len(uids) < 2 {
			continue
		}
		all := make([]string, 0, len(uids))
		for u := range uids {
			all = append(all, u)
		}
		for _, u := range all {
			out[u] = "@" + uniquePrefix(u, all)
		}
	}
	return out
}

// uniquePrefix is the shortest prefix of uid, minimum 8 characters, that
// no other uid shares; the full uid when nothing shorter is unique.
func uniquePrefix(uid string, all []string) string {
	for n := 8; n < len(uid); n++ {
		p := uid[:n]
		unique := true
		for _, o := range all {
			if o != uid && strings.HasPrefix(o, p) {
				unique = false
				break
			}
		}
		if unique {
			return p
		}
	}
	return uid
}

// clusterImageWorkloads is --all-clusters --by workload's payload.
// Rendering is per cluster, so generation disambiguation scopes per
// rendered cluster (IM-11).
type clusterImageWorkloads map[string]imageWorkloadRows

func (c clusterImageWorkloads) Columns() []string {
	return []string{"CLUSTER", "IMAGE", "NAMESPACE", "WORKLOAD", "PODS"}
}

func (c clusterImageWorkloads) Rows() [][]string {
	ids := make([]string, 0, len(c))
	for id := range c {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out [][]string
	for _, id := range ids {
		for _, r := range c[id].Rows() {
			out = append(out, append([]string{id}, r...))
		}
	}
	return out
}

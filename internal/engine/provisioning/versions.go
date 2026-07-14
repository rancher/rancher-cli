// versions.go implements D-7's `get kubernetes-versions`: it merges the
// channel-server's rke2/k3s release lists with the two default-version
// Settings and the ui-k8s-supported-versions-range Setting into one table,
// so a caller can see every installable version, which one Rancher installs
// by default per distro, and whether each version falls inside the
// server-advertised supported range.
package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// rke2ReleasePath and k3sReleasePath are the unauthenticated mux routes
	// rancher's routes.go:139-140 registers server-side for the
	// channel-server release lists. RootClient sends credentials to them
	// anyway, per the one-policy rule (spec D-10) — no anonymous
	// special-casing.
	rke2ReleasePath = "/v1-rke2-release/release"
	k3sReleasePath  = "/v1-k3s-release/release"

	// rke2DefaultVersionSetting/k3sDefaultVersionSetting name the
	// management.cattle.io/v3 Settings carrying each distro's
	// Rancher-selected default version (rancher setting.go:173-174).
	rke2DefaultVersionSetting = "rke2-default-version"
	k3sDefaultVersionSetting  = "k3s-default-version"
	// supportedRangeSetting names the Setting carrying the advisory
	// supported-version-range grammar (rancher setting.go:358), e.g.
	// ">= 1.11.0 <=1.14.x".
	supportedRangeSetting = "ui-k8s-supported-versions-range"
)

// settingsGVR is the management.cattle.io/v3 Settings resource the two
// default-version Settings and the supported-range Setting live under
// (cluster-scoped: ns "" on ClusterClient.Get).
var settingsGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "settings"}

// Version carries one merged `get kubernetes-versions` row; also consumed by
// Task 7's warn-only create-time validation.
type Version struct {
	Distro  string `json:"distro"` // "rke2" | "k3s"
	Version string `json:"version"`
	Default bool   `json:"default"`
	// Supported reports whether Version falls inside the server's
	// ui-k8s-supported-versions-range setting (spec D-7's
	// in-supported-range?, review R1-5). nil = range setting absent or
	// unparseable — unknown, not false.
	Supported *bool `json:"supported,omitempty"`
}

// normalizeVersion strips the leading "v" and everything from "+" off a raw
// channel-server version string (e.g. "v1.34.1+rke2r1" -> "1.34.1"). Its own
// tiny function so a regression here is caught by its own dedicated test
// rather than only showing up as a mysteriously-nil Supported field.
func normalizeVersion(raw string) string {
	v := strings.TrimPrefix(raw, "v")
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	return v
}

// rangeClause is one comparator+operand pair from a parsed
// ui-k8s-supported-versions-range value. wildcard marks an operand whose
// patch component is "x" (or omitted): the patch position is ignored on
// both sides when this clause is evaluated.
type rangeClause struct {
	op           string
	major, minor int
	patch        int
	wildcard     bool
}

// rangeTokenRE scans a raw range string for `>=`/`<=`/`>`/`<`/`=` immediately
// (optionally with whitespace) followed by a `MAJOR.MINOR[.PATCH|.x]`
// operand — the entire grammar spec D-7 requires, and nothing more. A
// string with no such token (empty, or genuinely unparseable garbage) never
// matches, which is exactly how parseSupportedRange reports "advisory data
// unavailable" without a special-case.
var rangeTokenRE = regexp.MustCompile(`(>=|<=|>|<|=)\s*(\d+)\.(\d+)(?:\.(\d+)|\.(x))?`)

// parseSupportedRange parses raw into its AND-ed comparator clauses. ok is
// false when raw carries no recognizable clause at all (empty or garbage) —
// the caller's contract is "unknown", never an error: the range is
// advisory (spec D-7).
func parseSupportedRange(raw string) (clauses []rangeClause, ok bool) {
	matches := rangeTokenRE.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return nil, false
	}
	for _, m := range matches {
		major, _ := strconv.Atoi(m[2])
		minor, _ := strconv.Atoi(m[3])
		c := rangeClause{op: m[1], major: major, minor: minor}
		switch {
		case m[5] == "x":
			c.wildcard = true
		case m[4] != "":
			c.patch, _ = strconv.Atoi(m[4])
		default:
			c.wildcard = true // MAJOR.MINOR only: no patch given, any patch matches
		}
		clauses = append(clauses, c)
	}
	return clauses, true
}

// compareTriple compares (aMajor,aMinor,aPatch) to (bMajor,bMinor,bPatch),
// returning -1/0/1. When wildcard is set the patch position is dropped from
// the comparison entirely — that is what a ".x" operand means.
func compareTriple(aMajor, aMinor, aPatch, bMajor, bMinor, bPatch int, wildcard bool) int {
	if aMajor != bMajor {
		if aMajor < bMajor {
			return -1
		}
		return 1
	}
	if aMinor != bMinor {
		if aMinor < bMinor {
			return -1
		}
		return 1
	}
	if wildcard {
		return 0
	}
	if aPatch != bPatch {
		if aPatch < bPatch {
			return -1
		}
		return 1
	}
	return 0
}

func clauseHolds(c rangeClause, major, minor, patch int) bool {
	cmp := compareTriple(major, minor, patch, c.major, c.minor, c.patch, c.wildcard)
	switch c.op {
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	case "=":
		return cmp == 0
	default:
		return false
	}
}

// versionSupported evaluates rawVersion (a RAW, un-normalized channel-server
// version string) against rangeRaw, the advisory
// ui-k8s-supported-versions-range setting value. nil means "unknown" — an
// absent/empty/unparseable range, or a version string normalizeVersion
// cannot resolve to at least MAJOR.MINOR — never an error, per spec D-7.
func versionSupported(rangeRaw, rawVersion string) *bool {
	clauses, ok := parseSupportedRange(rangeRaw)
	if !ok {
		return nil
	}
	parts := strings.SplitN(normalizeVersion(rawVersion), ".", 3)
	if len(parts) < 2 {
		return nil
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return nil
	}
	patch := 0
	if len(parts) == 3 {
		patch, _ = strconv.Atoi(parts[2])
	}
	for _, c := range clauses {
		if !clauseHolds(c, major, minor, patch) {
			supported := false
			return &supported
		}
	}
	supported := true
	return &supported
}

// fetchReleaseVersions GETs path against root and decodes its channel-server
// release envelope ({"data":[{"version": "..."}, ...]}, channelserver
// model/config.go:22-30 — the collection key is "data", pinned against the
// fixture testrancher serves; the acceptance pass against a real server
// corrects this if the shape differs). A malformed body is a typed
// CodeGeneric error naming "decoding" and path, mirroring Task 5's
// decodeCRTCollection contract exactly — never a panic, never an empty slice
// standing in for the failure silently. Any error root.GetJSON itself
// returns (including a 404) is returned as-is, already clierr-mapped.
func fetchReleaseVersions(ctx context.Context, root engine.RootClient, path string) ([]string, error) {
	body, err := root.GetJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("decoding %s: %v", path, err))
	}
	versions := make([]string, 0, len(env.Data))
	for _, d := range env.Data {
		versions = append(versions, d.Version)
	}
	return versions, nil
}

// isNotFoundError reports whether err is engine.RootClient's documented 404
// mapping (clierr.CodeNotFound) — the "older server, route doesn't exist
// yet" case that degrades to a warning rather than failing the whole read.
func isNotFoundError(err error) bool {
	var ce *clierr.Error
	return errors.As(err, &ce) && ce.Code == clierr.CodeNotFound
}

// settingValue reads a Setting object's "value" field, falling back to
// "default" when value is absent or empty (rancher setting.go convention;
// mirrors internal/client/provider's ServerVersionSetting reading the same
// top-level "value" field).
func settingValue(obj *unstructured.Unstructured) string {
	if v, found, _ := unstructured.NestedString(obj.Object, "value"); found && v != "" {
		return v
	}
	v, _, _ := unstructured.NestedString(obj.Object, "default")
	return v
}

// getSettingValue reads the named management.cattle.io/v3 Setting and
// returns its value; ANY failure (not found, RBAC, decode) degrades to ""
// rather than propagating — a missing/empty setting means "no default
// marked" / "range unknown", never an error (spec D-7).
func getSettingValue(ctx context.Context, client engine.ClusterClient, name string) string {
	obj, err := client.Get(ctx, settingsGVR, "", name)
	if err != nil || obj == nil {
		return ""
	}
	return settingValue(obj)
}

// fetchVersionsDetailed is FetchVersions' full implementation, additionally
// returning the raw supported-range setting (for the command envelope's
// transparency field) and any degraded-fetch warnings. FetchVersions itself
// discards the latter two so its signature stays exactly what Task 7
// consumes; NewGetKubernetesVersions's Execute uses this directly to
// surface both on the command Result.
func fetchVersionsDetailed(ctx context.Context, roots engine.RootClientFactory, clients engine.ClusterClientFactory) ([]Version, string, []string, error) {
	root, err := roots.Root()
	if err != nil {
		return nil, "", nil, err
	}
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return nil, "", nil, err
	}

	// The two release fetches and the three setting reads below depend only
	// on the already-resolved root/client above, never on each other, so
	// they run concurrently via errgroup rather than paying 5 sequential
	// round trips (same idiom as internal/engine/inspect/images.go's
	// --all-clusters fan-out and internal/commands/authcheck.go's 3
	// concurrent probes). Each goroutine writes only to its own dedicated
	// result variable(s), so there is no data race between them; the
	// ASSEMBLY into rows/warnings below happens only after g.Wait() joins
	// all five, in the same fixed order (rke2 then k3s) the original
	// sequential code produced — so output stays deterministic regardless
	// of which goroutine happens to finish first.
	var (
		rke2Versions, k3sVersions []string
		rke2Warning, k3sWarning   string
		rke2Default, k3sDefault   string
		rangeRaw                  string
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		vs, err := fetchReleaseVersions(gctx, root, rke2ReleasePath)
		if err != nil {
			if !isNotFoundError(err) {
				return err
			}
			rke2Warning = "channel server route " + rke2ReleasePath + " not found (older server?): rke2 versions omitted"
			vs = nil
		}
		rke2Versions = vs
		return nil
	})
	g.Go(func() error {
		vs, err := fetchReleaseVersions(gctx, root, k3sReleasePath)
		if err != nil {
			if !isNotFoundError(err) {
				return err
			}
			k3sWarning = "channel server route " + k3sReleasePath + " not found (older server?): k3s versions omitted"
			vs = nil
		}
		k3sVersions = vs
		return nil
	})
	g.Go(func() error {
		rke2Default = getSettingValue(gctx, client, rke2DefaultVersionSetting)
		return nil
	})
	g.Go(func() error {
		k3sDefault = getSettingValue(gctx, client, k3sDefaultVersionSetting)
		return nil
	})
	g.Go(func() error {
		rangeRaw = getSettingValue(gctx, client, supportedRangeSetting)
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, "", nil, err
	}

	var warnings []string
	if rke2Warning != "" {
		warnings = append(warnings, rke2Warning)
	}
	if k3sWarning != "" {
		warnings = append(warnings, k3sWarning)
	}

	rows := make([]Version, 0, len(rke2Versions)+len(k3sVersions))
	for _, v := range rke2Versions {
		rows = append(rows, Version{Distro: "rke2", Version: v, Default: v == rke2Default, Supported: versionSupported(rangeRaw, v)})
	}
	for _, v := range k3sVersions {
		rows = append(rows, Version{Distro: "k3s", Version: v, Default: v == k3sDefault, Supported: versionSupported(rangeRaw, v)})
	}
	return rows, rangeRaw, warnings, nil
}

// FetchVersions merges the channel-server release lists
// (/v1-rke2-release/release and /v1-k3s-release/release) with the
// default-version Settings into one table. A 404 on either release route
// (an older server that predates that route) degrades to the other
// distro's rows alone — never an error; the omission is only visible as a
// warning on NewGetKubernetesVersions's Result, since this function's
// signature carries no warnings channel (Task 7 consumes exactly this
// shape).
func FetchVersions(ctx context.Context, roots engine.RootClientFactory, clients engine.ClusterClientFactory) ([]Version, error) {
	rows, _, _, err := fetchVersionsDetailed(ctx, roots, clients)
	return rows, err
}

// versionsResult is `get kubernetes-versions`' payload: the merged rows
// plus the raw supported-range setting, echoed at the top level for
// transparency (spec D-7) alongside the per-row Supported boolean. It
// implements output.Tabular so -o table shows
// DISTRO/VERSION/DEFAULT/SUPPORTED while -o json/-o yaml also carry the raw
// range string under "supportedRange".
type versionsResult struct {
	Versions       []Version `json:"data"`
	SupportedRange string    `json:"supportedRange,omitempty"`
}

func (r versionsResult) Columns() []string {
	return []string{"DISTRO", "VERSION", "DEFAULT", "SUPPORTED"}
}

func (r versionsResult) Rows() [][]string {
	out := make([][]string, 0, len(r.Versions))
	for _, v := range r.Versions {
		def := "no"
		if v.Default {
			def = "yes"
		}
		sup := ""
		if v.Supported != nil {
			sup = "no"
			if *v.Supported {
				sup = "yes"
			}
		}
		out = append(out, []string{v.Distro, v.Version, def, sup})
	}
	return out
}

type getKubernetesVersionsCmd struct {
	roots   engine.RootClientFactory
	clients engine.ClusterClientFactory
}

// NewGetKubernetesVersions builds `get kubernetes-versions`: merges the
// channel-server release lists (/v1-rke2-release/release and
// /v1-k3s-release/release — unauthenticated mux routes server-side,
// rancher routes.go:139-140; credentials sent anyway per the RootClient
// one-policy rule) with the default-version Settings into one table.
func NewGetKubernetesVersions(roots engine.RootClientFactory, clients engine.ClusterClientFactory) runtime.Command {
	return &getKubernetesVersionsCmd{roots: roots, clients: clients}
}

func (c *getKubernetesVersionsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "get kubernetes-versions",
		Usage:       "List available Kubernetes versions (rke2/k3s) merged with the server's default and supported-range settings",
		CommandType: runtime.ReadOnly,
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c *getKubernetesVersionsCmd) Execute(ctx context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	rows, rangeRaw, warnings, err := fetchVersionsDetailed(ctx, c.roots, c.clients)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{
		Kind:     "KubernetesVersionList",
		Data:     versionsResult{Versions: rows, SupportedRange: rangeRaw},
		Warnings: warnings,
	}, nil
}

var _ runtime.ReadCommand = (*getKubernetesVersionsCmd)(nil)

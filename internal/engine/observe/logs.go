package observe

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	flagTail       = "tail"
	flagSince      = "since"
	flagPrevious   = "previous"
	flagContainer  = "container"
	flagTimestamps = "timestamps"
	flagPod        = "pod"
)

const defaultTailLines = 200

type logsCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewLogs builds `logs <target> [--cluster] [--tail N=200] [--since <dur>]
// [--previous] [--container] [--timestamps] [--pod]`: curated,
// verified-location logs for a Rancher component, merging every matched
// pod/container into one prefixed body, or narrowing to one pod with --pod.
func NewLogs(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &logsCmd{clients: clients, roots: roots}
}

func (c *logsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "logs",
		Usage: "Show logs of Rancher components (server, agent, fleet-agent, kube-api-auth, webhook, api-extension)",
		Args:  []runtime.ArgSpec{{Name: "target", Type: "string", Required: true, Enum: Names()}},
		Flags: []runtime.ArgSpec{
			{Name: engine.FlagCluster, Type: "string", Desc: "target cluster `id|name`"},
			{Name: flagTail, Type: "string", Desc: "max `lines` (default 200)"},
			{Name: flagSince, Type: "string", Desc: "logs newer than this `duration`, e.g. 30m, 2h; omit for no limit"},
			{Name: flagPrevious, Type: "bool", Desc: "logs from the previous container instance"},
			{Name: flagContainer, Type: "string", Desc: "`container` name (default: the containers and init containers of each pod)"},
			{Name: flagTimestamps, Type: "bool", Desc: "prefix each line with a timestamp"},
			{Name: flagPod, Type: "string", Desc: "restrict to one `pod` name, to read the logs of a single replica"},
		},
		CommandType: runtime.ReadOnly,
		Examples: []string{
			"rancher logs agent --cluster prod",
			"rancher logs server --since 1h",
			"rancher logs server --pod rancher-7f58899d95-r87tk --timestamps",
		},
		Auth:      true,
		Shape:     runtime.ShapeText,
		PlainText: true,
	}
}

func (c *logsCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]
	// The pipeline's enum validation (target's ArgSpec.Enum, sourced from
	// Names()) already rejects an unlisted name before Execute runs in the
	// real CLI path; this check stays as the guard for direct-Execute
	// callers (this package's own tests included) that bypass the pipeline.
	target, ok := Lookup(name)
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("unknown log target %q; choose one of: %s", name, strings.Join(Names(), ", ")))
	}

	tail, err := parseTailFlag(in.String(flagTail))
	if err != nil {
		return runtime.Result{}, err
	}
	since, err := parseSinceFlag(in.String(flagSince))
	if err != nil {
		return runtime.Result{}, err
	}

	resolved, err := Resolve(ctx, c.clients, steve.RootFactoryGetter(c.roots), target, in)
	if err != nil {
		return runtime.Result{}, err
	}

	// With zero resolved pods, skip the name filter and fall through to
	// the no-logs NOT_FOUND below - it names the target, namespace, and
	// cluster, which is the actual problem; "pod not found" with an empty
	// candidate list would point at a list that does not exist.
	if podFilter := in.String(flagPod); podFilter != "" && len(resolved.Pods) > 0 {
		pod, perr := selectPod(resolved.Pods, podFilter)
		if perr != nil {
			return runtime.Result{}, perr
		}
		resolved.Pods = []unstructured.Unstructured{pod}
	}

	containerFilter := in.String(flagContainer)
	base := engine.LogOptions{
		TailLines:    tail,
		SinceSeconds: since,
		Previous:     in.Bool(flagPrevious),
		Timestamps:   in.Bool(flagTimestamps),
	}

	var entries []logEntry
	var warnings []string
	var refusals []*clierr.Error
	for i := range resolved.Pods {
		podName := resolved.Pods[i].GetName()
		for _, cname := range engine.ContainerNames(resolved.Pods[i].Object) {
			if containerFilter != "" && cname != containerFilter {
				continue
			}
			opts := base
			opts.Container = cname
			out, lerr := resolved.Client.Logs(ctx, resolved.Namespace, podName, opts)
			if lerr != nil {
				warnings = append(warnings, fmt.Sprintf("could not read the logs of %s/%s: %v", podName, cname, lerr))
				refusals = append(refusals, clierr.From(lerr))
				continue
			}
			entries = append(entries, logEntry{Pod: podName, Container: cname, Line: out})
		}
	}

	if len(entries) == 0 {
		if ce := sameRefusal(refusals); ce != nil {
			return runtime.Result{}, ce.WithWarnings(warnings...)
		}
		return runtime.Result{}, clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("no logs available for target %q in namespace %s on cluster %s; confirm the target is running, or check the warnings for the pods and containers whose logs could not be read",
				target.Name, resolved.Namespace, resolved.ClusterID))
	}

	return runtime.Result{
		Kind:     "TargetLogs",
		Text:     mergeText(entries),
		Data:     logsResult{Pods: podNames(resolved.Pods), Entries: entries},
		Warnings: warnings,
	}, nil
}

// sameRefusal returns the first error, in pod and container order, when
// every log read was refused with one code, AUTH or RBAC; otherwise nil.
func sameRefusal(errs []*clierr.Error) *clierr.Error {
	if len(errs) == 0 || (errs[0].Code != clierr.CodeAuth && errs[0].Code != clierr.CodeRBAC) {
		return nil
	}
	for _, e := range errs[1:] {
		if e.Code != errs[0].Code {
			return nil
		}
	}
	return errs[0]
}

// parseTailFlag defaults an empty --tail to 200 and rejects anything that is
// not a non-negative integer with a VALIDATION error. Returns *int64 (never
// nil) matching LogOptions.TailLines' pointer type - nil there means "field
// unset", which a --tail value, defaulted or explicit, never is.
func parseTailFlag(raw string) (*int64, error) {
	if raw == "" {
		n := int64(defaultTailLines)
		return &n, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--tail %q is not a valid non-negative integer", raw))
	}
	return &n, nil
}

// parseSinceFlag wraps extres.ParseTTL for the duration GRAMMAR without its
// zero-sentinel semantics: omitted → nil (no limit); a parsed
// duration → *int64(seconds); an explicit zero ("0s") or a positive value
// under one second ("500us") → VALIDATION, since a pointer *0 means "logs
// since now" to PodLogOptions.SinceSeconds - silently empty output, never
// what --since 0s meant to ask for.
func parseSinceFlag(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	d, err := extres.ParseTTL(flagSince, raw)
	if err != nil {
		return nil, err
	}
	if d == 0 {
		return nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--since %q must be greater than zero; omit --since for no time limit, or pass a positive duration (30m, 2h)", raw))
	}
	secs, err := extres.TTLSeconds(flagSince, d, raw)
	if err != nil {
		return nil, err
	}
	return &secs, nil
}

// mergeText renders entries as the Text-mode default: every log line
// prefixed with "[pod/container]", interleaved in fetch order - no
// timestamp re-sort; --timestamps lets callers sort themselves.
func mergeText(entries []logEntry) string {
	var b strings.Builder
	for _, e := range entries {
		prefix := fmt.Sprintf("[%s/%s] ", e.Pod, e.Container)
		lines := strings.Split(e.Line, "\n")
		if len(lines) > 1 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		for _, l := range lines {
			b.WriteString(prefix)
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// logEntry is one container's fetched log line set, tagged with its source
// (Data's {pods, entries} shape).
type logEntry struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Line      string `json:"line"`
}

type logsResult struct {
	Pods    []string   `json:"pods"`
	Entries []logEntry `json:"entries"`
}

// selectPod narrows a target's resolved pods to the one named pod, for
// reading the logs of a single replica (the HA-triage case: grep one replica's logs,
// not the merged stream). An unknown name is a NOT_FOUND naming the target's
// actual pod names as candidates, as engine.MatchOne's NOT_FOUND does.
func selectPod(pods []unstructured.Unstructured, name string) (unstructured.Unstructured, error) {
	for i := range pods {
		if pods[i].GetName() == name {
			return pods[i], nil
		}
	}
	names := podNames(pods)
	cands := make([]clierr.Candidate, len(names))
	for i, n := range names {
		cands[i] = clierr.Candidate{ID: n, Name: n}
	}
	return unstructured.Unstructured{}, clierr.New(clierr.CodeNotFound, fmt.Sprintf("pod %q not found", name)).
		WithCandidates(cands)
}

func podNames(pods []unstructured.Unstructured) []string {
	names := make([]string, len(pods))
	for i := range pods {
		names[i] = pods[i].GetName()
	}
	return names
}

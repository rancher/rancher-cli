package observe

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	flagTail       = "tail"
	flagSince      = "since"
	flagPrevious   = "previous"
	flagContainer  = "container"
	flagTimestamps = "timestamps"
)

const defaultTailLines = 200

type logsCmd struct {
	clients engine.ClusterClientFactory
}

// NewLogs builds `logs <target> [--cluster] [--tail N=200] [--since <dur>]
// [--previous] [--container] [--timestamps]` (LS-1/LS-2): curated,
// verified-location logs for a Rancher component, merging every matched
// pod/container into one prefixed body.
func NewLogs(clients engine.ClusterClientFactory) runtime.Command {
	return &logsCmd{clients: clients}
}

func (c *logsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "logs",
		Usage: "Curated Rancher-component logs (server, agent, fleet-agent, kube-api-auth, webhook)",
		Args:  []runtime.ArgSpec{{Name: "target", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: engine.FlagCluster, Type: "string"},
			{Name: flagTail, Type: "string"},
			{Name: flagSince, Type: "string"},
			{Name: flagPrevious, Type: "bool"},
			{Name: flagContainer, Type: "string"},
			{Name: flagTimestamps, Type: "bool"},
		},
		CommandType: runtime.ReadOnly,
		Examples: []string{
			"rancher logs agent --cluster prod",
			"rancher logs server --since 1h",
		},
		Toolset: "core",
		Network: true,
		Auth:    true,
	}
}

func (c *logsCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]
	target, ok := Lookup(name)
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown log target %q", name)).
			WithHint(fmt.Sprintf("choose one of: %s", strings.Join(Names(), ", ")))
	}

	tail, err := parseTailFlag(in.String(flagTail))
	if err != nil {
		return runtime.Result{}, err
	}
	since, err := parseSinceFlag(in.String(flagSince))
	if err != nil {
		return runtime.Result{}, err
	}

	resolved, err := Resolve(ctx, c.clients, target, in)
	if err != nil {
		return runtime.Result{}, err
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
				warnings = append(warnings, fmt.Sprintf("%s/%s: %v", podName, cname, lerr))
				continue
			}
			entries = append(entries, logEntry{Pod: podName, Container: cname, Line: out})
		}
	}

	if len(entries) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("no logs available for target %q in namespace %s on cluster %s", target.Name, resolved.Namespace, resolved.ClusterID)).
			WithHint("confirm the target is running, or check the warnings for per-pod fetch failures")
	}

	return runtime.Result{
		Kind:     "TargetLogs",
		Text:     mergeText(entries),
		Data:     logsResult{Pods: podNames(resolved.Pods), Entries: entries},
		Warnings: warnings,
	}, nil
}

// parseTailFlag defaults an empty --tail to 200 and rejects anything that is
// not a non-negative integer with a VALIDATION error. Returns *int64 (never
// nil) matching LogOptions.TailLines' pointer type — nil there means "field
// unset", which a --tail value, defaulted or explicit, never is.
func parseTailFlag(raw string) (*int64, error) {
	if raw == "" {
		n := int64(defaultTailLines)
		return &n, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--tail %q is not a valid non-negative integer", raw)).
			WithHint("pass --tail <N>")
	}
	return &n, nil
}

// parseSinceFlag wraps extres.ParseTTL for the duration GRAMMAR without its
// zero-sentinel semantics (LS-2): omitted → nil (no limit); a parsed
// duration → *int64(seconds); an explicit zero ("0s") → VALIDATION, since a
// pointer *0 means "logs since now" to PodLogOptions.SinceSeconds — silently
// empty output, never what --since 0s meant to ask for.
func parseSinceFlag(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	d, err := extres.ParseTTL(raw)
	if err != nil {
		return nil, err
	}
	if d == 0 {
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("--since %q must be greater than zero", raw)).
			WithHint("omit --since for no time limit, or pass a positive duration (30m, 2h)")
	}
	secs := int64(d.Seconds())
	return &secs, nil
}

// mergeText renders entries as the Text-mode default: every log line
// prefixed with "[pod/container]", interleaved in fetch order (LS-2) — no
// timestamp re-sort in v1; --timestamps lets callers sort themselves.
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
// (Data's {pods, entries} shape from LS-3).
type logEntry struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Line      string `json:"line"`
}

type logsResult struct {
	Pods    []string   `json:"pods"`
	Entries []logEntry `json:"entries"`
}

func podNames(pods []unstructured.Unstructured) []string {
	names := make([]string, len(pods))
	for i := range pods {
		names[i] = pods[i].GetName()
	}
	return names
}

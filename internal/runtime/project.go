package runtime

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// FlagProject is the global --project flag; FlagCluster names the cluster
// a project is resolved on; FlagReadOnly is the global --read-only flag.
const (
	FlagProject  = "project"
	FlagCluster  = "cluster"
	FlagReadOnly = "read-only"
)

// ProjectFlag is the entry a command lists in Descriptor.Flags to take the
// global --project flag; desc says what the project does for the command.
// The entry is never a second command-line flag: --project is parsed once,
// as a global flag, so a value typed before the command name reaches the
// command too. The pipeline refuses a typed --project on every command
// that does not list it, before any request.
func ProjectFlag(required bool, desc string) ArgSpec {
	return ArgSpec{Name: FlagProject, Type: "string", Required: required, Desc: desc}
}

// TakesProject reports whether d lists the global --project flag.
func TakesProject(d Descriptor) bool {
	return slices.ContainsFunc(d.Flags, func(f ArgSpec) bool { return f.Name == FlagProject })
}

// Project is what a typed --project resolved to: the project's
// metadata.name on the cluster --cluster names.
type Project struct {
	Cluster     string
	ID          string
	DisplayName string // spec.displayName, for a command that shows it
	// Namespaces reads the project's namespaces on Cluster. The resolver
	// sets it, so the read follows the same rules as the lookup.
	Namespaces func(ctx context.Context) (map[string]bool, error)
}

// ProjectResolver resolves a command's typed --project. With no --cluster
// it refuses, naming the clusters a project with that id or display name
// is on. warning is "", or says that the value is one project's id and
// another project's display name.
type ProjectResolver interface {
	ResolveProject(ctx context.Context, in Input, value string) (p Project, warning string, err error)
}

// ProjectRefusalHinter is implemented by a command that does not take
// --project but whose callers may still type it: the hint is the command
// to run instead, built from the typed values ("" for none).
type ProjectRefusalHinter interface {
	ProjectRefusalHint(in Input) string
}

// lazyProject resolves the typed --project once, on the command's first
// read, so every check the command makes from its input alone still comes
// before the lookup's requests. It is a sync.Once, not sync.OnceValues: the
// lookup runs under the first reader's context (an access command's
// --timeout budget bounds it), and OnceValues' function takes none.
type lazyProject struct {
	once    sync.Once
	resolve func(ctx context.Context) (Project, string, error)
	notice  *leadingNotice
	project Project
	err     error
}

func (l *lazyProject) get(ctx context.Context) (Project, error) {
	l.once.Do(func() {
		var warning string
		l.project, warning, l.err = l.resolve(ctx)
		if l.err == nil && warning != "" {
			l.notice.set(warning)
		}
	})
	return l.project, l.err
}

type projectKey struct{}

// WithProject is ctx carrying p as the project the pipeline resolved.
func WithProject(ctx context.Context, p Project) context.Context {
	l := &lazyProject{resolve: func(context.Context) (Project, string, error) { return p, "", nil }, notice: &leadingNotice{}}
	return context.WithValue(ctx, projectKey{}, l)
}

// WithProjectLookup is ctx carrying a lookup the command's first TypedProjectOn
// calls, as the pipeline sets it up: a test of a command can tell whether,
// and when, the command reads the project.
func WithProjectLookup(ctx context.Context, lookup func(ctx context.Context) (Project, error)) context.Context {
	l := &lazyProject{notice: &leadingNotice{}, resolve: func(ctx context.Context) (Project, string, error) {
		p, err := lookup(ctx)
		return p, "", err
	}}
	return context.WithValue(ctx, projectKey{}, l)
}

// projectOn returns the project the lookup on ctx resolves, for a command
// acting on clusterID: resolved on the first call of the invocation, from
// memory after it; ok is false when ctx carries no lookup. A project
// resolved on another cluster is a command bug, refused rather than used.
func projectOn(ctx context.Context, clusterID string) (p Project, ok bool, err error) {
	l, typed := ctx.Value(projectKey{}).(*lazyProject)
	if !typed {
		return Project{}, false, nil
	}
	if fn := projectReadObserver.Load(); fn != nil {
		(*fn)(clusterID)
	}
	p, err = l.get(ctx)
	if err != nil {
		return Project{}, false, err
	}
	if p.Cluster != clusterID {
		return Project{}, false, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: --project was resolved on cluster %s, the command acts on %s", p.Cluster, clusterID))
	}
	return p, true, nil
}

// TypedProjectOn returns the project the typed --project names, for a
// command that takes --project acting on clusterID - the one way a command
// reads it: resolved on the first call of the invocation, from memory
// after it; ok is false only when no --project was typed. Two cases are
// command bugs, refused rather than used: a project resolved on another
// cluster, and a typed --project the pipeline did not look up (a direct
// call that skipped the pipeline), so a command can never act unscoped on
// a project the caller named.
func TypedProjectOn(ctx context.Context, in Input, clusterID string) (p Project, ok bool, err error) {
	p, ok, err = projectOn(ctx, clusterID)
	if err != nil || ok {
		return p, ok, err
	}
	if in.IsSet(FlagProject) || in.String(FlagProject) != "" {
		return Project{}, false, clierr.New(clierr.CodeGeneric,
			"command bug: --project was typed but the pipeline did not look it up")
	}
	return Project{}, false, nil
}

// MissingClusterError is the refusal of an absent --cluster for an
// invocation that typed --project: the lookup's answer naming the clusters
// the project is on. nil when no --project was typed, so the caller keeps
// its own refusal.
func MissingClusterError(ctx context.Context) error {
	l, typed := ctx.Value(projectKey{}).(*lazyProject)
	if !typed {
		return nil
	}
	_, err := l.get(ctx)
	if err == nil {
		return clierr.New(clierr.CodeGeneric, "command bug: --project resolved with no --cluster")
	}
	return err
}

// checkProjectFlag refuses a typed --project the command does not take,
// and an empty one, before any request.
func checkProjectFlag(cmd Command, d Descriptor, in Input) *clierr.Error {
	if !in.IsSet(FlagProject) {
		return nil
	}
	if !TakesProject(d) {
		ce := clierr.New(clierr.CodeValidation, "--project does not apply to "+d.Name)
		if h, ok := cmd.(ProjectRefusalHinter); ok {
			ce = ce.WithHint(h.ProjectRefusalHint(in))
		}
		return ce
	}
	if blank(in.String(FlagProject)) {
		return clierr.New(clierr.CodeValidation, "--project is empty; pass a project id or display name, or omit --project")
	}
	return nil
}

// withProject puts the lookup of a typed --project on the context: it runs
// on the command's first read, and no request is sent before it. Its
// warning is written before the invocation's other notices.
func withProject(ctx context.Context, cmd Command, in Input, streams IO) (context.Context, IO, *clierr.Error) {
	if streams.Projects == nil {
		return ctx, streams, clierr.New(clierr.CodeGeneric,
			"command bug: "+cmd.Describe().Name+" takes --project but no project resolver is wired")
	}
	notice := &leadingNotice{src: streams.Notices}
	streams.Notices = notice
	value := in.String(FlagProject)
	l := &lazyProject{notice: notice, resolve: func(ctx context.Context) (Project, string, error) {
		return streams.Projects.ResolveProject(ctx, in, value)
	}}
	return context.WithValue(ctx, projectKey{}, l), streams, nil
}

// leadingNotice returns its notice, once set, before src's notices.
type leadingNotice struct {
	mu     sync.Mutex
	notice string
	src    NoticeSource
}

func (n *leadingNotice) set(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notice = s
}

func (n *leadingNotice) DrainNotices() []string {
	n.mu.Lock()
	var out []string
	if n.notice != "" {
		out, n.notice = []string{n.notice}, ""
	}
	n.mu.Unlock()
	if n.src != nil {
		out = append(out, n.src.DrainNotices()...)
	}
	return out
}

// projectReadObserver, when set, is called on every project read of an
// invocation that typed --project, with the cluster the command acts on.
var projectReadObserver atomic.Pointer[func(string)]

// SetProjectReadObserverForTest makes every project read of an
// invocation that typed --project call f, so a test can tell a command
// that takes --project and reads it from one that takes it and never
// does; nil clears it.
func SetProjectReadObserverForTest(f func(clusterID string)) (restore func()) {
	var next *func(string)
	if f != nil {
		next = &f
	}
	prev := projectReadObserver.Swap(next)
	return func() { projectReadObserver.Store(prev) }
}

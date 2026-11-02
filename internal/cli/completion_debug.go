package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// Completion offers no candidates on every failure and never writes to
// stderr on its own. A non-empty RANCHER_CLI_COMPLETION_DEBUG turns on a
// per-phase trace so a silent-empty result can be measured in place: one
// line per phase, prefixed with the time elapsed since process start, so
// both "why empty" and "where did the time go" are answerable from one
// run. The trace goes to stderr only; candidates on stdout stay
// byte-identical whether or not the variable is set, and the trace is
// emitted only while a completion request is being served - never on a
// regular command.

var (
	processStart = time.Now()

	// completionDebugOut is the trace sink; tests substitute a buffer.
	completionDebugOut io.Writer = os.Stderr
)

func completionDebugEnabled() bool {
	return completionActive() && os.Getenv(config.EnvCompletionDebug) != ""
}

// completionDebugf writes one trace line when tracing is on and is a no-op
// otherwise; callers pass the phase and its outcome, never a trailing newline.
// The formatted text has every character a terminal line cannot show removed
// (control and Unicode format characters, line and paragraph separators,
// newlines), the remedy commands of this process excepted; they are expanded
// next.
func completionDebugf(format string, args ...any) {
	if !completionDebugEnabled() {
		return
	}
	line := runtime.PrintableRemedies(fmt.Sprintf(format, args...))
	if clierr.HasRemedy(line) {
		line = completionTraceIO().ExpandText(line)
	}
	_, _ = fmt.Fprintf(completionDebugOut, "completion +%s %s\n",
		time.Since(processStart).Round(time.Millisecond), line)
}

// completionTraceIO is the IO that expands the remedy commands of a trace
// line: the config directory the completion request loaded, named by the
// rule every command uses, or none when no directory is stored.
func completionTraceIO() runtime.IO {
	storedConfigMu.RLock()
	defer storedConfigMu.RUnlock()
	if storedConfigDir == "" {
		return runtime.IO{}
	}
	return runtime.IO{RemedyDir: remedyDirOf(runtime.Descriptor{}, nil, storedConfigDir)}
}

// completionDeadline applies the live-completion time limit to ctx and
// traces it, so every live candidate fetch shares one definition of the deadline.
func completionDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	timeLimit := completionTimeout()
	completionDebugf("time limit: %s", timeLimit)
	return context.WithTimeout(ctx, timeLimit)
}

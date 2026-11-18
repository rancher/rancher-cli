package nodes

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// maxPDBCommands is how many get poddisruptionbudgets commands a failed
// drain's message lists before it counts the rest.
const maxPDBCommands = 5

// drainRetryCommand is the dry-run command that tries the drain again after
// Rancher ended it as failed: every option typed, the flags the failure
// message names when they were not typed, and, for a drain that did not
// complete in time, a --drain-timeout of twice the time used, up to the most a
// drain accepts.
func drainRetryCommand(node, cluster string, f drainFlags, message string) clierr.Command {
	cmd := clierr.NewCommand("node drain").Arg(node).FlagValue("--cluster", cluster)
	if f.force || strings.Contains(message, "--force") {
		cmd = cmd.Flag("--force")
	}
	if f.deleteEmptyDir || strings.Contains(message, "--delete-emptydir-data") {
		cmd = cmd.Flag("--delete-emptydir-data")
	}
	if f.gracePeriodText != "" {
		cmd = cmd.FlagValue("--grace-period", f.gracePeriodText)
	}
	doubled := min(2*f.drainTimeout, maxDrainTimeout)
	switch {
	case strings.Contains(message, "did not complete within") && doubled > f.drainTimeout:
		cmd = cmd.FlagValue("--drain-timeout", formatDuration(doubled))
	case f.drainTimeoutText != "":
		cmd = cmd.FlagValue("--drain-timeout", f.drainTimeoutText)
	}
	return cmd.Flag("--dry-run")
}

// pdbText is prose followed by the commands that read the
// PodDisruptionBudgets of the namespaces of the pods the drain evicts: at most
// maxPDBCommands, then the count of the rest.
func pdbText(prose, cluster string, namespaces []string) string {
	seq := clierr.NewSequence()
	for _, ns := range namespaces[:min(maxPDBCommands, len(namespaces))] {
		seq = seq.Then("", clierr.NewCommand("get poddisruptionbudgets").FlagValue("--cluster", cluster).FlagValue("--namespace", ns))
	}
	switch more := len(namespaces) - maxPDBCommands; {
	case more == 1:
		return clierr.Text(prose, clierr.Lead("; read the PodDisruptionBudgets that can block it with "), seq, "; and 1 more namespace")
	case more > 1:
		return clierr.Text(prose, clierr.Lead("; read the PodDisruptionBudgets that can block it with "), seq, fmt.Sprintf("; and %d more namespaces", more))
	}
	return clierr.Text(prose, clierr.Lead("; read the PodDisruptionBudgets that can block it with "), seq)
}

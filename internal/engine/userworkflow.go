package engine

import (
	"errors"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// UserPasswordWorkflowCapability is the discovery-based requirement shared
// by create user and create password-change-request: the user-password
// workflow (the hashed password Secret plus the ext.cattle.io
// PasswordChangeRequest resource) is absent entirely on older servers,
// with no Feature flag involved, so its capability signal is the resource's
// presence in ext.cattle.io discovery.
func UserPasswordWorkflowCapability() runtime.CapabilityRequirement {
	return runtime.CapabilityRequirement{
		Tier: runtime.CapabilityExtDiscovery, Feature: "passwordchangerequests",
		Unsupported: "the user-password workflow",
	}
}

// RefuseUnsupportedWorkflow reclassifies a NOT_FOUND from a Plan-time
// dry-run into the same CAPABILITY_UNSUPPORTED refusal the discovery check
// produces, naming the caller's evidence: when discovery itself was
// unreadable the capability check proceeds (unknown never blocks), and the
// dry-run is then the first request that can prove the workflow absent -
// the operator must never see a raw NOT_FOUND for what is really a missing
// workflow. The message carries the observed evidence rather than a
// version diagnosis, because a NOT_FOUND cannot distinguish an old server
// from one whose workflow objects were removed; detail names the way(s)
// out - never a hint, since recovering from a missing workflow is never a
// single concrete command. Every other error passes through unchanged: a
// real, actionable failure (RBAC, network) must never be silently
// reclassified.
func RefuseUnsupportedWorkflow(err error, evidence, detail string) error {
	if !clierr.IsNotFound(err) {
		return err
	}
	return clierr.New(clierr.CodeCapabilityUnsupported,
		"this server does not support the user-password workflow ("+evidence+"); "+detail)
}

// CommittedPartialSuccess wraps a post-commit step failure with the given
// clierr.CommittedContextDetails, preserving the underlying error's
// Code/Hint/Retryable when it is already a *clierr.Error rather than
// flattening it to CodeGeneric - a caller inspecting Retryable to decide
// whether to retry must see the classification the underlying call
// actually carried, not always false.
//
// The committed details replace the error's own details. A TLS_TRUST
// error's message points at the certificate facts its details held, so
// the message gains them: the certificate's subject, issuer and
// fingerprint.
func CommittedPartialSuccess(err error, details clierr.CommittedContextDetails) *clierr.Error {
	ce := clierr.From(err)
	msg := ce.Message
	if _, ok := errors.AsType[*clierr.Error](err); ok {
		// A wrapped classified error keeps the text its wrapper adds.
		msg = err.Error()
	}
	if ce.Code == clierr.CodeTLSTrust {
		msg += certificateFacts(ce)
	}
	return clierr.New(ce.Code, msg).WithHint(ce.Hint).WithRetryable(ce.Retryable).WithDetails(details)
}

// certificateFacts renders the certificate lines of a TLS_TRUST error's
// details (subject, issuer and fingerprint) as a message suffix, or ""
// when the details hold none. The validity line is left out.
func certificateFacts(ce *clierr.Error) string {
	td, ok := ce.Details.(clierr.TextDetails)
	if _, committed := ce.Details.(clierr.CommittedContextDetails); !ok || committed {
		return ""
	}
	var facts []string
	for _, line := range td.TextDetails() {
		if !strings.HasPrefix(line, "valid: ") {
			facts = append(facts, line)
		}
	}
	if len(facts) == 0 {
		return ""
	}
	return "; " + strings.Join(facts, "; ")
}

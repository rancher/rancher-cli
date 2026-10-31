package auth

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// trustCandidate is a guided-trust-accepted certificate: the exact PEM to
// store as the entry's CA selector and the fingerprint tlstrust.Classify
// already computed for it, kept together so the commit step never
// re-derives one from the other.
type trustCandidate struct {
	pem         string
	fingerprint string
}

// dynamiclistenerCAMarker is the common-name substring Rancher's generated
// CA carries. Self-identification is an UNAUTHENTICATED heuristic - names
// are attacker-controlled - used only to word the card, never to skip the
// fingerprint check.
const dynamiclistenerCAMarker = "dynamiclistener-ca"

// offerGuidedTrust inspects a preflight failure for a classified
// unknown-authority TLS_TRUST error and, when the session can prompt, shows the
// certificate trust card and asks for explicit consent. priorFingerprint is the
// entry's previously recorded CAFingerprint, if any - non-empty makes the card
// open with a changed-identity warning naming it next to the new candidate's,
// rather than rendering first-contact-indistinguishable.
//
// declined is true exactly when the caller should treat err as the
// ORIGINAL classified failure (not a TLS_TRUST error, can't prompt, not
// unknown-authority, no extractable certificate, or an explicit decline) -
// byte-for-byte parity with the non-interactive refusal, since a decline
// must never be distinguishable from never having been asked. declined is
// false when a consent was given but the candidate then failed to
// re-verify: err there describes the CANDIDATE, a new and different
// failure the caller must surface as-is, never through the
// original-failure's own remediation wording.
//
// On success, version is the re-verify's confirmed server version
// (provider.RancherVersion never returns an empty version on a genuine
// success - only on the failures the underlying tlsPreflight swallows
// as non-fatal: connectivity, a non-200, or an unreadable version
// body), letting a delegated-registration
// caller build its versionWarning without a second network round trip
// against the same candidate.
func offerGuidedTrust(ctx context.Context, failure error, p runtime.Prompter, tlsPreflight register.TLSPreflightFunc, serverURL, priorFingerprint string) (accepted *trustCandidate, version string, declined bool, err error) {
	ce, ok := errors.AsType[*clierr.Error](failure)
	if !ok || ce.Code != clierr.CodeTLSTrust || !p.CanPrompt() {
		return nil, "", true, failure
	}
	details, ok := ce.Details.(tlstrust.Details)
	if !ok || details.Kind != tlstrust.KindUnknownAuthority {
		return nil, "", true, failure
	}
	cert := details.Certificate()
	if cert == nil {
		return nil, "", true, failure
	}
	if !confirmTrust(ctx, p, details, priorFingerprint) {
		return nil, "", true, failure
	}
	candidatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	if tlsPreflight == nil {
		return &trustCandidate{pem: candidatePEM, fingerprint: details.Fingerprint}, "", false, nil
	}
	// "The candidate becomes the verified CA" is proven here, never
	// assumed: every failure tlsPreflight swallows as non-fatal -
	// DNS/refused/timeout, a non-200, an unreadable version body -
	// returns ("", nil), which would otherwise read as a silent pass for
	// a certificate that was never actually re-checked.
	v, verr := tlsPreflight(ctx, serverURL, candidatePEM)
	switch {
	case verr != nil:
		return nil, "", false, reverifyFailedError(details, verr)
	case v == "":
		return nil, "", false, reverifyFailedError(details, nil)
	default:
		return &trustCandidate{pem: candidatePEM, fingerprint: details.Fingerprint}, v, false, nil
	}
}

// reverifyFailedError builds the failure for a just-consented candidate
// that did not re-verify: it names the candidate the operator consented
// to, not the entry's previously stored (still-failing) trust, and never
// suggests --cacert <path> - the operator holds no file for a certificate
// that arrived over the wire.
func reverifyFailedError(details tlstrust.Details, cause error) *clierr.Error {
	msg := fmt.Sprintf("the accepted certificate (fingerprint %s) could not be confirmed against the server", details.Fingerprint)
	if cause != nil {
		msg += ": " + cause.Error()
	} else {
		msg += " (the connection failed after accepting it)"
	}
	return clierr.New(clierr.CodeTLSTrust, msg+"; the certificate was not stored - try again")
}

// confirmTrust renders the trust card and asks the one explicit yes/no
// question. Any answer other than "y"/"yes"
// (case-insensitive) - including a read error - declines.
func confirmTrust(ctx context.Context, p runtime.Prompter, d tlstrust.Details, priorFingerprint string) bool {
	p.Notice(trustCard(d, priorFingerprint))
	answer, err := p.Prompt(ctx, "trust this certificate for this server entry? [y/N]: ")
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// trustCard renders the candidate's subject/issuer/fingerprint/validity -
// d.TextDetails(), which already labels the certificate role (serving
// certificate vs certificate authority) - plus the unauthenticated
// self-identification heuristic: names are attacker-controlled, so the
// card always follows with the reminder to confirm the fingerprint with the
// server's administrator, whatever the certificate claims about itself.
// priorFingerprint, when non-empty, opens the card with a changed-identity
// warning naming it next to the new candidate's - the SSH-changed-key
// moment: a re-prompt after a recorded consent must never render
// identically to a first contact.
func trustCard(d tlstrust.Details, priorFingerprint string) string {
	var lines []string
	if priorFingerprint != "" && priorFingerprint != d.Fingerprint {
		lines = append(lines,
			fmt.Sprintf("warning: this server previously presented a certificate with fingerprint %s; it now presents a different one - the certificate may have been replaced, or another server may be answering in its place", priorFingerprint))
	}
	for _, l := range d.TextDetails() {
		lines = append(lines, output.Printable(l))
	}
	if strings.Contains(strings.ToLower(d.Subject), dynamiclistenerCAMarker) {
		lines = append(lines, "the certificate identifies itself as "+dynamiclistenerCAMarker+" (Rancher's generated CA)")
	} else {
		lines = append(lines, "the certificate is not Rancher's generated CA: it may come from your organization's CA, or from a proxy between you and the server that replaces certificates")
	}
	lines = append(lines, "confirm the fingerprint with the server's administrator, whatever the subject above says")
	return strings.Join(lines, "\n")
}

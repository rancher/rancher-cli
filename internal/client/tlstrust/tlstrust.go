// Package tlstrust classifies TLS certificate verification failures the CLI
// observes when connecting to a Rancher server, and identifies the trust
// candidate a user could pin with --cacert to recover from an
// unknown-authority failure.
//
// Classify reuses the certificate chain Go's own TLS stack already captured
// on the failed handshake (tls.CertificateVerificationError.
// UnverifiedCertificates) rather than opening a second connection to
// re-observe it. That chain came from the exact route the caller's http.Client
// actually dialed — proxy decision, CONNECT tunneling, SNI, dialer, TLS
// minimum, and timeout all already applied — so there is nothing left to
// clone: a fresh, naive dial would risk observing a different peer (for
// example a proxy's certificate instead of Rancher's), while the failed
// handshake's own capture cannot drift from the route that produced it. The
// handshake also never reached the point of sending an application request,
// so this satisfies the same "no application traffic" requirement a separate
// probe would have needed to honor by construction.
package tlstrust

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// Kind classifies why TLS certificate verification failed. Only
// KindUnknownAuthority can be repaired by pinning a CA; the others are
// returned unchanged with their own kind because a trust decision cannot fix
// them.
type Kind string

const (
	KindUnknownAuthority Kind = "unknownAuthority"
	KindHostnameMismatch Kind = "hostnameMismatch"
	KindExpired          Kind = "expired"
	KindNotYetValid      Kind = "notYetValid"
	KindInvalidChain     Kind = "invalidChain"
)

// Details is the JSON-serializable metadata attached to a classified
// clierr.Error's Details field. Fingerprint/Subject/Issuer/NotAfter are
// nullable (omitted) when no candidate anchor could be extracted from the
// presented chain.
type Details struct {
	Kind        Kind   `json:"kind"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	NotBefore   string `json:"notBefore,omitempty"`
	NotAfter    string `json:"notAfter,omitempty"`
}

// Classify reports whether err is a TLS certificate verification failure and,
// if so, returns a fully structured clierr.Error: a stable message and hint
// for kind, plus Details carrying the candidate anchor's metadata when one
// could be extracted from the presented chain. Returns nil when err is not a
// certificate verification failure at all (DNS, connection-refused, timeout,
// and similar errors are the caller's to classify as ordinary connectivity
// failures).
// ClassifyForHost classifies err only when the request that failed was
// aimed at host (matched via the *url.Error in the chain). An OAuth
// device-code/auth-code exchange can carry a caPEM-scoped client to an
// EXTERNAL identity provider's endpoints; classifying its TLS failure as
// the Rancher server's would produce a confident, specific, and wrong
// diagnosis (wrong host, wrong --cacert fix). No *url.Error in the chain,
// or a different host: not classified — the raw error passes through.
func ClassifyForHost(err error, host string, now time.Time) *clierr.Error {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok {
		return nil
	}
	parsed, perr := url.Parse(urlErr.URL)
	if perr != nil || parsed.Host != host {
		return nil
	}
	return Classify(err, host, now)
}

func Classify(err error, host string, now time.Time) *clierr.Error {
	kind, chain, offending, ok := classifyKind(err, host, now)
	if !ok {
		return nil
	}
	// Unknown authority shows the CANDIDATE ANCHOR — the certificate a user
	// could pin. Every other kind shows the certificate that actually
	// failed verification (the anchor may be a perfectly valid CA while the
	// leaf is expired; describing the CA there would point the operator at
	// the wrong certificate).
	details := Details{Kind: kind}
	subject := offending
	if kind == KindUnknownAuthority {
		subject = CandidateAnchor(chain)
	}
	if subject == nil && len(chain) > 0 {
		subject = chain[0]
	}
	if subject != nil {
		details.Fingerprint = Fingerprint(subject)
		details.Subject = subject.Subject.String()
		details.Issuer = subject.Issuer.String()
		details.NotBefore = subject.NotBefore.UTC().Format(time.RFC3339)
		details.NotAfter = subject.NotAfter.UTC().Format(time.RFC3339)
	}
	switch kind {
	case KindUnknownAuthority:
		return clierr.New(clierr.CodeTLSTrust,
			"the server presented a certificate signed by an unknown authority").
			WithHint("obtain the server's CA through a trusted channel (or verify its fingerprint out of band against the one shown here) and pass it with --cacert <path>").
			WithDetails(details)
	case KindHostnameMismatch:
		return clierr.New(clierr.CodeTLSTrust,
			"the server's certificate is not valid for "+host).
			WithHint("check that --server matches the certificate's hostname; --cacert cannot fix a hostname mismatch").
			WithDetails(details)
	case KindExpired:
		return clierr.New(clierr.CodeTLSTrust, "the server's certificate has expired").
			WithHint("ask the server operator to renew the certificate; --cacert cannot fix an expired certificate").
			WithDetails(details)
	case KindNotYetValid:
		return clierr.New(clierr.CodeTLSTrust, "the server's certificate is not yet valid").
			WithHint("check the local clock, or ask the server operator to reissue the certificate; --cacert cannot fix this").
			WithDetails(details)
	default: // KindInvalidChain
		return clierr.New(clierr.CodeTLSTrust, "the server's certificate chain failed validation").
			WithHint("the certificate chain is malformed or incomplete; contact the server operator").
			WithDetails(details)
	}
}

// classifyKind extracts the presented chain and Kind from a TLS certificate
// verification failure. ok is false when err carries no
// tls.CertificateVerificationError at all (not a certificate-verification
// failure) or when the underlying handshake failed before any certificate
// could be parsed (chain is empty; Details will carry a Kind with no
// metadata — the nullable-details case).
//
// When the verification error carries none of the typed x509 errors, the
// failure came from a platform verifier (the system-roots path on macOS and
// Windows hands verification to the OS, which reports failures as bare
// strings — Go's own verifier is used only when a custom root pool is set).
// Classification must not depend on platform error strings, so the captured
// chain is reclassified from evidence the chain itself carries (see
// reclassifyUntyped). Without this, the most common first-contact case — a
// self-signed Rancher server dialed with system roots on macOS — lands in
// invalidChain with a contact-the-operator hint instead of unknownAuthority
// with the --cacert hint.
func classifyKind(err error, host string, now time.Time) (kind Kind, chain []*x509.Certificate, offending *x509.Certificate, ok bool) {
	cve, found := errors.AsType[*tls.CertificateVerificationError](err)
	if !found {
		return "", nil, nil, false
	}
	chain = cve.UnverifiedCertificates

	if kind, offending, ok = classifyVerifyError(cve.Err, chain, now); ok {
		return kind, chain, offending, true
	}
	if kind, offending, ok = reclassifyUntyped(chain, host, now); ok {
		return kind, chain, offending, true
	}
	return KindInvalidChain, chain, nil, true
}

// classifyVerifyError maps a typed x509 verification error to its Kind and
// the certificate the verifier flagged. ok is false when verr carries none
// of the typed x509 errors (a platform verifier's untyped failure).
func classifyVerifyError(verr error, chain []*x509.Certificate, now time.Time) (Kind, *x509.Certificate, bool) {
	if hostErr, isHost := errors.AsType[x509.HostnameError](verr); isHost {
		return KindHostnameMismatch, hostErr.Certificate, true
	}
	if authErr, isAuth := errors.AsType[x509.UnknownAuthorityError](verr); isAuth {
		return KindUnknownAuthority, authErr.Cert, true
	}
	if invalidErr, isInvalid := errors.AsType[x509.CertificateInvalidError](verr); isInvalid && invalidErr.Reason == x509.Expired {
		// x509 uses one Reason for both directions; disambiguate on the
		// certificate the verifier actually flagged — a not-yet-valid
		// INTERMEDIATE alongside a currently-valid leaf must not read as
		// "expired" (chain[0]'s dates say nothing about the flagged cert).
		flagged := invalidErr.Cert
		if flagged == nil && len(chain) > 0 {
			flagged = chain[0]
		}
		if flagged != nil && now.Before(flagged.NotBefore) {
			return KindNotYetValid, flagged, true
		}
		return KindExpired, flagged, true
	}
	if invalidErr, isInvalid := errors.AsType[x509.CertificateInvalidError](verr); isInvalid {
		return KindInvalidChain, invalidErr.Cert, true
	}
	return "", nil, false
}

// reclassifyUntyped classifies a platform verifier's untyped failure from
// the captured chain alone. Re-verifying against the system pool is NOT an
// option: on macOS a pool returned by x509.SystemCertPool routes Verify
// straight back to the platform verifier — the same untyped error, a loop.
// System roots are not enumerable there, so the checks are ones the chain
// itself can answer, in the typed verifier's own precedence order:
//
//  1. validity window of every considered certificate (expired/notYetValid);
//  2. the leaf's hostname match (hostnameMismatch);
//  3. everything else is unknownAuthority, mirroring Go's own taxonomy:
//     Go's verifier reports EVERY unresolvable path — self-signed leaf,
//     leaf whose issuer was not presented, private CA — as
//     x509.UnknownAuthorityError, and reserves its invalid-chain reasons
//     for typed CertificateInvalidError values the platform path never
//     produces. The pinnable candidate is CandidateAnchor's business, as
//     on the typed path. The deliberate edge: a PUBLICLY-chained server
//     the platform distrusts for platform-only reasons (revoked root, CT
//     policy) also lands here, and the --cacert remedy is mechanically
//     real for it too — a pinned pool uses Go's verifier — while the
//     fingerprint in the details supports the out-of-band check before
//     anyone pins anything.
//
// ok=false only for an empty chain (nothing to reason from), keeping the
// nullable-details invalidChain fallback. Window checks consult only the
// anchor path — the signature-valid route from the leaf — never the raw
// presented chain: a server can bundle an unrelated stale certificate (an
// old expired root, a leftover intermediate), and blaming its dates would
// misread an unknown-authority failure as "expired" with a renew-it hint
// that fixes nothing. The path walk carries CandidateAnchor's cap; the
// chain is attacker-controlled.
func reclassifyUntyped(chain []*x509.Certificate, host string, now time.Time) (Kind, *x509.Certificate, bool) {
	if len(chain) == 0 {
		return "", nil, false
	}
	for _, c := range anchorPath(chain) {
		if now.After(c.NotAfter) {
			return KindExpired, c, true
		}
		if now.Before(c.NotBefore) {
			return KindNotYetValid, c, true
		}
	}
	// host may carry a port (ClassifyForHost matches url.Error hosts, which
	// keep theirs); VerifyHostname wants the bare name. An empty host skips
	// the check, matching a Classify caller that has none.
	dnsName := host
	if h, _, serr := net.SplitHostPort(host); serr == nil {
		dnsName = h
	}
	leaf := chain[0]
	if dnsName != "" {
		if herr := leaf.VerifyHostname(dnsName); herr != nil {
			return KindHostnameMismatch, leaf, true
		}
	}
	return KindUnknownAuthority, leaf, true
}

// CandidateAnchor returns the highest certificate of a contiguous,
// signature-valid path built up from the observed leaf (chain[0]), requiring
// CA basic constraints on every issuer link. Array position alone is never
// evidence: each step scans the remaining certificates for one that actually
// signs the current certificate. Returns the leaf itself when no valid issuer
// path exists, and nil when chain is empty.
// maxConsideredCerts caps how much of a presented chain the anchor walk
// examines: the walk performs real signature verifications and the chain
// length is attacker-controlled (TLS caps only the handshake-message size),
// so an unbounded walk would let a hostile server burn O(n^2) CPU per
// failed login just to compute an error hint. Legitimate chains are 2-4
// certificates.
const maxConsideredCerts = 16

func CandidateAnchor(chain []*x509.Certificate) *x509.Certificate {
	path := anchorPath(chain)
	if len(path) == 0 {
		return nil
	}
	return path[len(path)-1]
}

// anchorPath returns the contiguous signature-valid path CandidateAnchor
// walks, from the observed leaf up to the candidate anchor inclusive. The
// path — not the raw presented chain — is what evidence-based reasoning may
// consult: certificates outside it were presented but play no verified role,
// so attributing a failure to one of them would blame a bystander.
func anchorPath(chain []*x509.Certificate) []*x509.Certificate {
	if len(chain) == 0 {
		return nil
	}
	if len(chain) > maxConsideredCerts {
		chain = chain[:maxConsideredCerts]
	}
	current := chain[0]
	path := []*x509.Certificate{current}
	used := make([]bool, len(chain))
	used[0] = true
	for {
		idx := findIssuer(current, chain, used)
		if idx < 0 {
			return path
		}
		used[idx] = true
		current = chain[idx]
		path = append(path, current)
	}
}

// findIssuer returns the index of the first not-yet-used certificate in
// chain that is a valid CA and correctly signs current, or -1 when none
// exists.
func findIssuer(current *x509.Certificate, chain []*x509.Certificate, used []bool) int {
	for i, c := range chain {
		if used[i] || !c.IsCA || !c.BasicConstraintsValid {
			continue
		}
		if err := current.CheckSignatureFrom(c); err != nil {
			continue
		}
		return i
	}
	return -1
}

// Fingerprint is the SHA-256 digest of cert's DER bytes, lowercase hex —
// the one stable hash input and encoding for trust metadata everywhere it is
// recorded or displayed.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

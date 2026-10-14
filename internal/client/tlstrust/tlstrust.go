// Package tlstrust classifies TLS certificate verification failures the CLI
// observes when connecting to a Rancher server, and identifies the trust
// candidate a user could pin with --cacert to recover from an
// unknown-authority failure.
//
// Classify reuses the certificate chain Go's own TLS stack already captured
// on the failed handshake (tls.CertificateVerificationError.
// UnverifiedCertificates) rather than opening a second connection to
// re-observe it. That chain came from the exact route the caller's http.Client
// actually dialed - proxy decision, CONNECT tunneling, SNI, dialer, TLS
// minimum, and timeout all already applied - so there is nothing left to
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
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
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
	// leaf is true only for KindUnknownAuthority when the presented chain
	// contained no valid issuer for the candidate (dynamiclistener commonly
	// serves the leaf alone): the fingerprint shown then describes the
	// SERVER's own certificate, never a certificate authority, and the
	// wording must say so or an operator matching it against a fetched CA
	// file gets a false mismatch. Never serialized: a JSON consumer already
	// has the certificate role from Kind plus the shape of the chain it
	// requested separately; this only steers the projected text.
	leaf bool
	// cert is the exact certificate Details describes (the candidate anchor
	// for KindUnknownAuthority, the offending certificate for every other
	// kind). Never serialized - a consent flow that must persist trust
	// reads it through Certificate(); JSON consumers see only the
	// projected fields.
	cert *x509.Certificate
}

// Certificate returns the exact certificate Details describes, or nil when
// none could be extracted from the presented chain (the nullable-details
// case).
func (d Details) Certificate() *x509.Certificate { return d.cert }

// IsLeafCertificate reports whether the fingerprint/subject shown describe
// the server's own certificate rather than a certificate authority -
// meaningful only when Kind is KindUnknownAuthority.
func (d Details) IsLeafCertificate() bool { return d.leaf }

// TextDetails projects the candidate/offending certificate's facts onto
// indented lines, each conditional on its own field: every certificate field
// may be absent when no candidate anchor could be extracted from the
// presented chain, and an absent field prints no line at all. For
// KindUnknownAuthority the first line names WHICH certificate the
// fingerprint describes - "certificate authority" or, when the chain
// carried no valid issuer, "serving certificate" - so the card is never
// read as naming a CA it cannot check against a fetched CA file.
func (d Details) TextDetails() []string {
	var lines []string
	if d.Subject != "" {
		label := "certificate"
		if d.Kind == KindUnknownAuthority {
			label = "certificate authority"
			if d.leaf {
				label = "serving certificate"
			}
		}
		lines = append(lines, label+": "+d.Subject)
	}
	if d.Issuer != "" {
		lines = append(lines, "issuer: "+d.Issuer)
	}
	if d.Fingerprint != "" {
		lines = append(lines, "fingerprint: "+d.Fingerprint)
	}
	if d.NotBefore != "" && d.NotAfter != "" {
		lines = append(lines, fmt.Sprintf("valid: %s to %s", d.NotBefore, d.NotAfter))
	}
	return lines
}

// Remedy words the part of a certificate failure message that depends on
// the command: how the CA reaches it, what the server URL is called, and
// which option "cannot fix" a failure the CA does not cause. Each empty
// field takes the wording of the auth login preflight, so the zero value
// gives the message of a command that takes --url and --cacert.
type Remedy struct {
	// CA follows the sentence about obtaining the CA of an unknown-authority
	// failure and carries its own connector (" and pass it with --cacert").
	CA string
	// URL is what a hostname mismatch tells the user to check against the
	// certificate's hostname ("--url").
	URL string
	// Fix is the option a hostname mismatch, an expired and a not-yet-valid
	// certificate say cannot fix the failure ("--cacert").
	Fix string
}

func (r Remedy) ca() string {
	if r.CA == "" {
		return " and pass it with --cacert"
	}
	return r.CA
}

func (r Remedy) url() string {
	if r.URL == "" {
		return "--url"
	}
	return r.URL
}

func (r Remedy) fix() string {
	if r.Fix == "" {
		return "--cacert"
	}
	return r.Fix
}

// UnknownAuthorityMessage is the message of an unknown-authority failure.
// leaf is Details.IsLeafCertificate. remedy follows the sentence about
// obtaining the CA and carries its own connector, so a caller whose
// remedy differs from --cacert (a login that took its CA from
// RANCHER_CLI_CACERT) builds the message itself instead of rewriting
// the text of Classify.
func UnknownAuthorityMessage(leaf bool, remedy string) string {
	const tail = "; if this server was already trusted, check the subject and issuer shown here - " +
		"a certificate you don't recognize can mean something in the network path is intercepting TLS"
	if leaf {
		return "the server presented a certificate signed by an unknown authority, with no separate certificate authority in the chain; " +
			"obtain the certificate through a trusted channel (or verify the fingerprint shown here out of band)" + remedy + tail
	}
	return "the server presented a certificate signed by an unknown authority; " +
		"obtain its CA through a trusted channel (or verify the fingerprint shown here out of band)" + remedy + tail
}

// Classify reports whether err is a TLS certificate verification failure and,
// if so, returns a fully structured clierr.Error: a stable message and hint
// for kind, plus Details carrying the candidate anchor's metadata when one
// could be extracted from the presented chain. Returns nil when err is not a
// certificate verification failure at all (DNS, connection-refused, timeout,
// and similar errors are the caller's to classify as ordinary connectivity
// failures).
func Classify(err error, host string, now time.Time, r Remedy) *clierr.Error {
	kind, chain, offending, ok := classifyKind(err, host, now)
	if !ok {
		return nil
	}
	// Unknown authority shows the CANDIDATE ANCHOR - the certificate a user
	// could pin. Every other kind shows the certificate that actually
	// failed verification (the anchor may be a perfectly valid CA while the
	// leaf is expired; describing the CA there would point the operator at
	// the wrong certificate).
	details := Details{Kind: kind}
	subject := offending
	if kind == KindUnknownAuthority {
		subject = CandidateAnchor(chain)
		details.leaf = subject != nil && len(chain) > 0 && subject == chain[0]
	}
	if subject == nil && len(chain) > 0 {
		subject = chain[0]
	}
	if subject != nil {
		details.cert = subject
		details.Fingerprint = Fingerprint(subject)
		details.Subject = subject.Subject.String()
		details.Issuer = subject.Issuer.String()
		details.NotBefore = subject.NotBefore.UTC().Format(time.RFC3339)
		details.NotAfter = subject.NotAfter.UTC().Format(time.RFC3339)
	}
	switch kind {
	case KindUnknownAuthority:
		msg := UnknownAuthorityMessage(details.leaf, r.ca())
		return clierr.New(clierr.CodeTLSTrust, msg).
			WithDetails(details)
	case KindHostnameMismatch:
		return clierr.New(clierr.CodeTLSTrust,
			"the server's certificate is not valid for "+host+
				"; check that "+r.url()+" matches the certificate's hostname; "+r.fix()+" cannot fix a hostname mismatch").
			WithDetails(details)
	case KindExpired:
		return clierr.New(clierr.CodeTLSTrust,
			"the server's certificate has expired; ask the server operator to renew it; "+r.fix()+" cannot fix an expired certificate").
			WithDetails(details)
	case KindNotYetValid:
		return clierr.New(clierr.CodeTLSTrust,
			"the server's certificate is not yet valid; check the local clock, or ask the server operator to reissue it; "+r.fix()+" cannot fix this").
			WithDetails(details)
	default: // KindInvalidChain
		return clierr.New(clierr.CodeTLSTrust,
			"the server's certificate chain failed validation; the chain is malformed or incomplete; contact the server operator").
			WithDetails(details)
	}
}

// ClassifyForOrigin classifies err only when the failed request's terminal
// URL (the *url.Error in the chain) has origin want. The comparison goes
// through httpx.OriginOf and Equal, so host case, an explicit default
// port, and IPv6 bracket spelling never defeat the match, while a
// same-host different-scheme failure is not attributed. An OAuth exchange
// can carry a failure from an external identity provider; classifying that
// as the Rancher server's would produce a confident, specific, and wrong
// diagnosis. Returns nil when err carries no *url.Error, the terminal URL
// does not parse, or want is invalid.
func ClassifyForOrigin(err error, want httpx.Origin, now time.Time, r Remedy) *clierr.Error {
	if !terminalOriginIs(err, want) {
		return nil
	}
	// host[:port], not the bare hostname: Classify verifies against the
	// name and names the same string in its hostname-mismatch message, so
	// two same-named servers on different ports stay distinguishable.
	return Classify(err, want.HostPort(), now, r)
}

// TypedError returns the TLS_TRUST error in err's chain, or nil. A site that
// would otherwise turn a transport error into plain text returns it
// unchanged, so the code, message and details a classifying transport
// produced reach the caller.
func TypedError(err error) *clierr.Error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeTLSTrust {
		return nil
	}
	return ce
}

// WrapTransport returns a wrapper that makes a certificate failure of a
// request to server a TLS_TRUST error worded by r. The wrapped transport's
// error stays reachable as the cause. A request to another origin, an error
// that is not a certificate failure and a failure to verify an https proxy
// are returned unchanged: Go verifies that proxy with the client's own
// roots, which are the Rancher CA, and reports the failure under the Rancher
// request URL, so classifying it would blame the server for the proxy's
// certificate.
func WrapTransport(server httpx.Origin, r Remedy) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return &classifyingTransport{next: next, server: server, remedy: r}
	}
}

// WrapTransportFor is WrapTransport for the origin of serverURL. It returns
// nil, which every caller treats as no wrapper, when serverURL has no usable
// origin: a request cannot then be attributed to the server.
func WrapTransportFor(serverURL string, r Remedy) func(http.RoundTripper) http.RoundTripper {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil
	}
	origin, err := httpx.OriginOf(u)
	if err != nil {
		return nil
	}
	return WrapTransport(origin, r)
}

type classifyingTransport struct {
	next   http.RoundTripper
	server httpx.Origin
	remedy Remedy
}

// WrappedRoundTripper returns the transport this one wraps. client-go cancels
// a timed-out request by walking its transport wrappers through this method;
// without it, a client-go client with a timeout logs that it cannot cancel.
func (t *classifyingTransport) WrappedRoundTripper() http.RoundTripper { return t.next }

func (t *classifyingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// http.Client applies its Timeout to a transport it does not recognize
	// through the deprecated Request.Cancel channel as well as the context
	// deadline, and a transport that sees both may report either: "request
	// canceled" instead of the deadline error the same client reports
	// without this wrapper. The context carries the deadline, so the inner
	// transport gets the request without the channel.
	if req.Cancel != nil { //nolint:staticcheck // the channel is set by http.Client itself; this is the only way to withhold it
		withoutCancel := *req
		withoutCancel.Cancel = nil //nolint:staticcheck // see above
		req = &withoutCancel
	}
	resp, err := t.next.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "proxyconnect" {
		return resp, err
	}
	origin, oerr := httpx.OriginOf(req.URL)
	if oerr != nil || !origin.Equal(t.server) {
		return resp, err
	}
	if ce := Classify(err, t.server.HostPort(), time.Now(), t.remedy); ce != nil {
		return resp, ce.WithCause(err)
	}
	return resp, err
}

// KindOf reports the certificate-verification failure kind of err when the
// failed request's terminal URL has origin want, under the same attribution
// rule as ClassifyForOrigin. It builds no TLS_TRUST error and extracts no
// candidate anchor: callers that must never offer guided trust (an external
// identity provider's certificate) use it to word their own error.
func KindOf(err error, want httpx.Origin, now time.Time) (Kind, bool) {
	if !terminalOriginIs(err, want) {
		return "", false
	}
	kind, _, _, ok := classifyKind(err, want.Hostname(), now)
	return kind, ok
}

func terminalOriginIs(err error, want httpx.Origin) bool {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok || !want.Valid() {
		return false
	}
	terminal, perr := url.Parse(urlErr.URL)
	if perr != nil {
		return false
	}
	got, oerr := httpx.OriginOf(terminal)
	return oerr == nil && got.Equal(want)
}

// classifyKind extracts the presented chain and Kind from a TLS certificate
// verification failure. ok is false when err carries no
// tls.CertificateVerificationError at all (not a certificate-verification
// failure) or when the underlying handshake failed before any certificate
// could be parsed (chain is empty; Details will carry a Kind with no
// metadata - the nullable-details case).
//
// When the verification error carries none of the typed x509 errors, the
// failure came from a platform verifier (the system-roots path on macOS and
// Windows hands verification to the OS, which reports failures as bare
// strings - Go's own verifier is used only when a custom root pool is set).
// Classification must not depend on platform error strings, so the captured
// chain is reclassified from evidence the chain itself carries (see
// reclassifyUntyped). Without this, the most common first-contact case - a
// self-signed Rancher server dialed with system roots on macOS - lands in
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
		// certificate the verifier actually flagged - a not-yet-valid
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
// straight back to the platform verifier - the same untyped error, a loop.
// System roots are not enumerable there, so the checks are ones the chain
// itself can answer, in the typed verifier's own precedence order:
//
//  1. validity window of every considered certificate (expired/notYetValid);
//  2. the leaf's hostname match (hostnameMismatch);
//  3. everything else is unknownAuthority, mirroring Go's own taxonomy:
//     Go's verifier reports EVERY unresolvable path - self-signed leaf,
//     leaf whose issuer was not presented, private CA - as
//     x509.UnknownAuthorityError, and reserves its invalid-chain reasons
//     for typed CertificateInvalidError values the platform path never
//     produces. The pinnable candidate is CandidateAnchor's business, as
//     on the typed path. The deliberate edge: a PUBLICLY-chained server
//     the platform distrusts for platform-only reasons (revoked root, CT
//     policy) also lands here, and the --cacert remedy is mechanically
//     real for it too - a pinned pool uses Go's verifier - while the
//     fingerprint in the details supports the out-of-band check before
//     anyone pins anything.
//
// ok=false only for an empty chain (nothing to reason from), keeping the
// nullable-details invalidChain fallback. Window checks consult only the
// anchor path - the signature-valid route from the leaf - never the raw
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
	// host may carry a port (callers pass the server's host[:port] so the
	// message can name it); VerifyHostname wants the bare name, and for an
	// IPv6 literal the bare address without brackets: SplitHostPort strips
	// them only when a port follows, so "[::1]" alone is unbracketed here
	// or VerifyHostname would treat it as a DNS name no IP SAN can match.
	// An empty host skips the check, matching a Classify caller that has
	// none.
	dnsName := host
	if h, _, serr := net.SplitHostPort(host); serr == nil {
		dnsName = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		dnsName = host[1 : len(host)-1]
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
// path - not the raw presented chain - is what evidence-based reasoning may
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

// Fingerprint is the SHA-256 digest of cert's DER bytes, lowercase hex -
// the one stable hash input and encoding for trust metadata everywhere it is
// recorded or displayed.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

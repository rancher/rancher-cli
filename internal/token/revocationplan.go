package token

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// Where a token a command deletes comes from.
const (
	sourceCache      = "cache"
	sourceSuperseded = "superseded"
	sourceLogin      = "login"
)

// Why a token is not revoked.
const (
	reasonFailed       = "failed"
	reasonUnconfirmed  = "unconfirmed"
	reasonBudget       = "budget"
	reasonCancelled    = "cancelled"
	reasonNoBearer     = "noBearer"
	reasonForbidden    = "forbidden"
	reasonV3           = "v3"
	reasonUnproven     = "unproven"
	reasonNoEntry      = "noEntry"
	reasonOtherServer  = "otherServer"
	reasonNotAttempted = "notAttempted"
	reasonNotDeleted   = "notDeleted"
	reasonOtherOwner   = "otherOwner"
	reasonUnknownOwner = "unknownOwner"
	reasonNoCLIBearer  = "noCliBearer"
	reasonUserSupplied = "userSupplied"
)

// What revokes a token that is not revoked now.
const (
	retryNextLogin = "nextLogin"
	retryManual    = "manual"
	retryNone      = "none"
)

// RevokedToken is a token the server confirmed deleted.
type RevokedToken struct {
	Ref    TokenRef `json:"ref"`
	Source string   `json:"source"`
	Server string   `json:"server"`
}

// NotRevokedToken is a token that may still be live: where it is recorded,
// why it was not revoked, and what revokes it.
type NotRevokedToken struct {
	Ref    *TokenRef `json:"ref,omitempty"`
	Source string    `json:"source"`
	Server string    `json:"server"`
	Entry  string    `json:"entry,omitempty"`
	Reason string    `json:"reason"`
	// Code is TLS_TRUST when the server's certificate failed verification,
	// and Error then says how to fix it. Certificate holds the facts about
	// that certificate.
	Code        string            `json:"code,omitempty"`
	Error       string            `json:"error,omitempty"`
	Certificate *tlstrust.Details `json:"certificate,omitempty"`
	Retry       string            `json:"retry"`
	Recovery    []string          `json:"recovery,omitempty"`
	// LoginAs is the user the first command of Recovery logs in as, the
	// token's owner; omitted when the recovery needs no login.
	LoginAs string `json:"loginAs,omitempty"`
}

// MapLiveTokens returns a copy of list with errorFn applied to each Error and
// recoveryFn to each Recovery item; a nil function leaves its field as it is.
// The copy has new slices: a list shared with another value is never
// written into. An item a function empties stays as an empty string, which
// keeps the shape of the list.
func MapLiveTokens(list []NotRevokedToken, errorFn, recoveryFn func(string) string) []NotRevokedToken {
	if list == nil {
		return nil
	}
	out := make([]NotRevokedToken, len(list))
	for i, n := range list {
		if errorFn != nil {
			n.Error = errorFn(n.Error)
		}
		if recoveryFn != nil && n.Recovery != nil {
			rec := make([]string, len(n.Recovery))
			for j, c := range n.Recovery {
				rec[j] = recoveryFn(c)
			}
			n.Recovery = rec
		}
		out[i] = n
	}
	return out
}

// TextLine is the line a failure's text details give a token that may
// still be live.
func (n NotRevokedToken) TextLine() string {
	what := n.Source + " token"
	if n.Ref != nil {
		what = n.Ref.String()
	}
	if n.Code != "" {
		return fmt.Sprintf("not revoked: %s on %s (%s, %s, retry %s)", what, n.Server, n.Reason, n.Code, n.Retry)
	}
	return fmt.Sprintf("not revoked: %s on %s (%s, retry %s)", what, n.Server, n.Reason, n.Retry)
}

// DetailLines is TextLine and, for a certificate failure, the message and
// the certificate's facts two spaces deeper.
func (n NotRevokedToken) DetailLines() []string {
	lines := []string{n.TextLine()}
	if n.Code == "" {
		return lines
	}
	for _, l := range certificateFailureLines(n.Error, n.Certificate) {
		lines = append(lines, "  "+l)
	}
	return lines
}

// certificateFailureLines is the message of a certificate failure and the
// facts about the certificate, one line each. The certificate comes from a
// server that just failed verification, so every line passes through
// output.Printable; the message keeps the markers of the remedy commands it
// holds, which the invocation expands when it prints the line.
func certificateFailureLines(message string, cert *tlstrust.Details) []string {
	lines := []string{runtime.PrintableRemedies(message)}
	if cert != nil {
		for _, l := range cert.TextDetails() {
			lines = append(lines, output.Printable(l))
		}
	}
	return lines
}

// RevocationReport is the two lists every deleting command reports, and
// the details of a deleting command's failure.
type RevocationReport struct {
	Revoked    []RevokedToken    `json:"revoked"`
	NotRevoked []NotRevokedToken `json:"notRevoked"`
}

// MapText maps the text of each token that may still be live: its error
// message and its recovery commands.
func (r RevocationReport) MapText(f func(string) string) clierr.TextDetails {
	r.NotRevoked = MapLiveTokens(r.NotRevoked, f, f)
	return r
}

// MapCommands maps the recovery commands of each token that may still be
// live, which a renderer that can offer no command empties.
func (r RevocationReport) MapCommands(f func(string) string) clierr.TextDetails {
	r.NotRevoked = MapLiveTokens(r.NotRevoked, nil, f)
	return r
}

// TextDetails lists each token that may still be live.
func (r RevocationReport) TextDetails() []string {
	var lines []string
	for _, n := range r.NotRevoked {
		lines = append(lines, n.DetailLines()...)
	}
	return lines
}

// revocationItem is one token a local delete may leave live: what it is,
// where it is recorded, and how it is revoked.
type revocationItem struct {
	ref    *TokenRef
	source string
	server string
	// recorded is the server as the record stored it, which may hold user
	// information that server (its identity) does not; empty when the item
	// has no record of its own.
	recorded  string
	cluster   string
	owner     string
	expiresAt string
	// cacheKey and cacheToken identify a cache record for the compare.
	cacheKey, cacheToken string
	// entry is the entry the token is recorded on, empty for none; record
	// asks the first write to add it there, and wrote says it did.
	entry  string
	record bool
	wrote  bool
	// loginTarget is the target whose login token this is.
	loginTarget string
	// matchLogin names the target whose stored login this superseded
	// record is: it is revoked with that token as its own bearer when the
	// run deletes that login.
	matchLogin string
	// hidden is a token expired past the margin: deleted, never listed.
	hidden bool
	// holdsLogin marks the login token of an entry that held a login the
	// CLI created and named.
	holdsLogin bool

	bearer     string
	self       bool
	provenance Provenance
	trust      string

	reason  string
	errText string
	// tlsErr is the TLS_TRUST error of a delete that failed on the server's
	// certificate, and cert the facts about that certificate when it carries
	// them.
	tlsErr    *clierr.Error
	cert      *tlstrust.Details
	attempted bool
	outcome   RevokeOutcome
	// revoked is a revoke the server confirmed: 200/204, or 404 to another
	// token of the same owner.
	revoked bool
}

// judge maps one answer onto the item: with its own bearer only 200/204
// confirms, 404 and 401 leave it unconfirmed and 403 is forbidden; with
// another bearer 404 confirms too and every other answer is failed. A v3
// token with its own bearer is judged by both of its answers instead.
func (it *revocationItem) judge(out RevokeOutcome) {
	if it.self && it.ref != nil && it.ref.Kind == TokenKindV3 {
		it.judgeV3(out)
		return
	}
	switch {
	case out.Status == http.StatusOK || out.Status == http.StatusNoContent:
		it.revoked = true
	case !it.self && out.Status == http.StatusNotFound:
		it.revoked = true
	case it.self && (out.Status == http.StatusNotFound || out.Status == http.StatusUnauthorized):
		it.reason = reasonUnconfirmed
	case it.self && out.Status == http.StatusForbidden:
		it.reason = reasonForbidden
	default:
		it.reason, it.errText = reasonFailed, out.Error
		it.noteCertificateFailure(out)
	}
}

// noteCertificateFailure keeps the TLS_TRUST error of a delete that failed
// on the server's certificate, and the certificate's facts.
func (it *revocationItem) noteCertificateFailure(out RevokeOutcome) {
	if out.TLSError == nil {
		return
	}
	it.tlsErr = out.TLSError
	if details, ok := out.TLSError.Details.(tlstrust.Details); ok {
		it.cert = &details
	}
}

// judgeV3 maps the logout and delete answers for a v3 token revoked with
// its own bearer onto the item, in this order: a 200 or 204 from either
// request confirms; a 404 to the delete is unconfirmed; a 401 from logout
// means the bearer no longer authenticates (the token is deleted, expired
// or disabled, and the CLI cannot tell which), so it is unconfirmed too;
// anything else is failed. The delete's 403 says nothing about the token
// for a standard user, so a v3 item is never forbidden, and a forbidden
// item's record is removed.
func (it *revocationItem) judgeV3(out RevokeOutcome) {
	switch {
	case out.LogoutStatus == http.StatusOK || out.LogoutStatus == http.StatusNoContent,
		out.Status == http.StatusOK || out.Status == http.StatusNoContent:
		it.revoked = true
	case out.Status == http.StatusNotFound, out.LogoutStatus == http.StatusUnauthorized:
		it.reason = reasonUnconfirmed
	default:
		it.reason, it.errText = reasonFailed, out.Error
		it.noteCertificateFailure(out)
	}
}

// plan classifies every token the delete removes. before holds the
// targets' entries as loaded; recs are every cache record.
func (d *Deletion) plan(cfg *config.Config, before map[string]config.Server, recs []config.CachedRecord) {
	for _, r := range recs {
		if d.selects(cfg, r) {
			d.items = append(d.items, d.planCacheRecord(cfg, r))
		}
	}
	for _, n := range d.req.Targets {
		if s, ok := before[n]; ok {
			d.planEntry(n, s)
		}
	}
}

// selects reports whether the command deletes the cache record.
func (d *Deletion) selects(cfg *config.Config, r config.CachedRecord) bool {
	switch d.req.Command {
	case DeleteForLogoutAll:
		return true
	case DeleteForDeleteToken:
		return d.req.Select == nil || d.req.Select(r)
	}
	return slices.ContainsFunc(d.req.Targets, func(t string) bool { return cfg.InDeleteScope(r, t) })
}

func (d *Deletion) planCacheRecord(cfg *config.Config, r config.CachedRecord) *revocationItem {
	it := &revocationItem{source: sourceCache, ref: r.TokenRef, server: config.CanonicalServerIdentity(r.Server), recorded: r.Server, cluster: r.Cluster,
		owner: r.TokenUserID, cacheKey: r.Key, cacheToken: r.Token}
	if !r.Expiry.IsZero() {
		it.expiresAt = r.Expiry.UTC().Format(time.RFC3339)
	}
	switch {
	case expiredPastMargin(it.expiresAt, d.now):
		it.hidden = true
		return it
	case r.TokenRef == nil:
		it.reason = reasonUnproven
		return it
	case !usableBearer(r.TokenRef.Kind, r.TokenRef.Name, r.Token):
		it.reason = reasonNoBearer
		return it
	}
	parent := d.parentOf(cfg, r)
	if parent == "" {
		it.reason = reasonNoEntry
		return it
	}
	it.entry, it.record = parent, true
	switch {
	case r.TokenRef.Kind == TokenKindV3 && (d.req.Command == DeleteForUpdateServer || r.Cluster != "" || !v3NameRE.MatchString(r.TokenRef.Name)):
		// Only an unscoped token is deleted with its own bearer: Rancher
		// refuses a cluster-scoped one on every request that names no
		// cluster, and the record a moved entry keeps has no bearer.
		it.reason = reasonV3
	case d.req.Command == DeleteForUpdateServer && !slices.Contains(d.req.Targets, parent) && config.SameServerURL(cfg.Servers[parent].URL, r.Server):
		// Another entry still names the old server: its next login
		// retires the token.
		it.reason = reasonNotAttempted
	case d.req.Command == DeleteForUpdateServer:
		it.reason = reasonOtherServer
	default:
		trust, ok := trustFor(cfg, parent, it.server)
		if !ok {
			it.reason = reasonNoEntry
			break
		}
		it.trust, it.bearer, it.self, it.provenance = trust, r.Token, true, ProvenanceDerived
	}
	return it
}

// parentOf is the entry a cache record is written on, by the rule its
// kind was written by. Empty for none.
func (d *Deletion) parentOf(cfg *config.Config, r config.CachedRecord) string {
	exists := func(n string) bool { _, ok := cfg.Servers[n]; return ok }
	plugin := func(n string) bool { return exists(n) && cfg.Servers[n].Provenance == clierr.ProvenanceExecLogin }
	switch r.Kind {
	case config.CacheRecordOwnCredential:
		if exists(r.Identified) {
			return r.Identified
		}
	case config.CacheRecordIdentified:
		switch {
		case !r.Listed:
			if exists(r.Identified) {
				return r.Identified
			}
		case plugin(r.Identified) && slices.Contains(r.Entries, r.Identified):
			return r.Identified
		default:
			if i := slices.IndexFunc(r.Entries, plugin); i >= 0 {
				return r.Entries[i]
			}
		}
	case config.CacheRecordServer:
		return d.serverRecordParent(cfg, r)
	}
	return ""
}

// serverRecordParent places a record of a call that identified no entry:
// among the existing listed entries that name the record's server, grouped
// by command - a logout's targets first, delete-server's and
// update-server's last - the first non-empty group wins, and in it the
// entry recording the token's owner, both known, else the first by name.
// Under update-server the moved entry is the fallback.
func (d *Deletion) serverRecordParent(cfg *config.Config, r config.CachedRecord) string {
	var targets, others []string
	for _, n := range r.Entries {
		s, ok := cfg.Servers[n]
		if !ok || !config.SameServerURL(s.URL, r.Server) {
			continue
		}
		if slices.Contains(d.req.Targets, n) {
			targets = append(targets, n)
		} else {
			others = append(others, n)
		}
	}
	var groups [][]string
	switch d.req.Command {
	case DeleteForLogout:
		groups = [][]string{targets, others}
	case DeleteForDeleteServer:
		groups = [][]string{others, targets}
	case DeleteForUpdateServer:
		groups = [][]string{others}
	default:
		groups = [][]string{slices.Concat(targets, others)}
	}
	for _, g := range groups {
		if len(g) == 0 {
			continue
		}
		slices.Sort(g)
		if r.TokenUserID != "" {
			if i := slices.IndexFunc(g, func(n string) bool { return cfg.Servers[n].TokenUserID == r.TokenUserID }); i >= 0 {
				return g[i]
			}
		}
		return g[0]
	}
	if d.req.Command == DeleteForUpdateServer && len(d.req.Targets) == 1 {
		return d.req.Targets[0]
	}
	return ""
}

// trustFor is the entry whose URL and CA a request for server uses: the
// record's parent when it names that server, else among the entries that
// do the one with a CA fingerprint, then one with a CA, then one that
// connects with RANCHER_CLI_CACERT, then the first by name. None means no current entry names the server, and nothing is
// sent.
func trustFor(cfg *config.Config, parent, server string) (string, bool) {
	if s, ok := cfg.Servers[parent]; ok && config.SameServerURL(s.URL, server) {
		return parent, true
	}
	names := cfg.MatchingServerNames(server)
	if len(names) == 0 {
		return "", false
	}
	for _, pick := range []func(config.Server) bool{
		func(s config.Server) bool { return s.CAFingerprint != "" },
		func(s config.Server) bool { return s.CA != "" },
		func(s config.Server) bool { return s.CAFromEnvironment },
	} {
		if i := slices.IndexFunc(names, func(n string) bool { return pick(cfg.Servers[n]) }); i >= 0 {
			return names[i], true
		}
	}
	return names[0], true
}

// planEntry classifies a target's superseded records and its login token.
func (d *Deletion) planEntry(name string, s config.Server) {
	server := config.CanonicalServerIdentity(s.URL)
	login := d.loginRead[name]
	for _, rec := range s.SupersededTokens {
		if s.TokenRef != nil && rec.Ref == *s.TokenRef {
			// The login row decides the token the entry names.
			continue
		}
		ref := rec.Ref
		it := &revocationItem{source: sourceSuperseded, ref: &ref, owner: rec.UserID, expiresAt: rec.ExpiresAt, entry: name,
			server: cmp.Or(config.CanonicalServerIdentity(rec.Server), server), recorded: rec.Server}
		d.items = append(d.items, it)
		matchesLogin := login != "" && usableBearer(rec.Ref.Kind, rec.Ref.Name, login) && it.server == server && s.CLICreated()
		switch {
		case expiredPastMargin(rec.ExpiresAt, d.now):
			it.hidden = true
		case rec.Ref.Kind == TokenKindV3 && (d.req.Command == DeleteForUpdateServer || !matchesLogin || !v3NameRE.MatchString(rec.Ref.Name)):
			// No bearer is stored for a v3 record, except the stored
			// login's own token.
			it.reason = reasonV3
		case d.req.Command == DeleteForUpdateServer:
			it.reason = reasonOtherServer
		case matchesLogin:
			it.matchLogin = name
		case it.server != server:
			it.reason = reasonOtherServer
		case !s.CLICreated():
			it.reason = reasonNoCLIBearer
		case config.RecordOwnerState(rec, s.TokenUserID) == config.OwnerDifferent:
			it.reason = reasonOtherOwner
		case config.RecordOwnerState(rec, s.TokenUserID) == config.OwnerUnknown:
			it.reason = reasonUnknownOwner
		case login == "" || s.TokenRef == nil || !usableBearer(s.TokenRef.Kind, s.TokenRef.Name, login):
			it.reason = reasonNoBearer
		default:
			it.bearer, it.provenance, it.trust = login, s.Provenance, name
		}
	}
	d.planLogin(name, s, server, login)
}

// planLogin classifies a target's login token.
func (d *Deletion) planLogin(name string, s config.Server, server, login string) {
	it := &revocationItem{source: sourceLogin, server: server, owner: s.TokenUserID, expiresAt: s.CredentialExpiresAt, loginTarget: name}
	switch {
	case s.CLIOwned():
		// An entry an earlier version's logout stamped keeps the name but
		// no login: nothing was read for it, so it has no bearer.
		ref := *s.TokenRef
		it.ref, it.entry, it.record, it.holdsLogin = &ref, name, true, s.LoggedOutAt == ""
		switch {
		case expiredPastMargin(it.expiresAt, d.now):
			it.hidden, it.entry, it.record = true, "", false
		case ref.Kind == TokenKindV3 && (d.req.Command == DeleteForUpdateServer || !v3NameRE.MatchString(ref.Name)):
			it.reason = reasonV3
		case d.req.Command == DeleteForUpdateServer:
			it.reason = reasonOtherServer
		case login != "" && usableBearer(ref.Kind, ref.Name, login):
			it.bearer, it.self, it.provenance, it.trust = login, true, ProvenanceDerived, name
		default:
			it.reason = reasonNoBearer
		}
	case s.LoggedOutAt != "" && d.req.Command != DeleteForUpdateServer:
		// Logged out already: no login is stored.
		return
	case s.CLICreated():
		it.reason = reasonUnproven
	default:
		it.reason = reasonUserSupplied
	}
	d.items = append(d.items, it)
}

// firstWrite applies the first write to cfg: the binding advance and its
// pause on every target, a superseded record for every token planned to be
// recorded that its entry does not hold already, the old server on the
// moved entry's records, and for a logout the cleared TokenRef and the
// LoggedOutAt stamp on every target. It reports whether anything changed.
func (d *Deletion) firstWrite(cfg *config.Config) (bool, error) {
	changed := false
	stamp := d.now.Format(time.RFC3339)
	for _, n := range d.req.Targets {
		e, ok := cfg.Servers[n]
		if !ok {
			continue
		}
		if d.req.Command.advancesBinding() {
			if err := config.StartCredentialChange(&e); err != nil {
				return false, err
			}
			changed = true
		}
		if d.req.Command == DeleteForUpdateServer {
			old := config.CanonicalServerIdentity(e.URL)
			for i := range e.SupersededTokens {
				if e.SupersededTokens[i].Server == "" {
					e.SupersededTokens[i].Server, changed = old, true
				}
			}
		}
		if d.req.Command.readsLogin() {
			e.TokenRef, e.LoggedOutAt, changed = nil, stamp, true
		}
		cfg.Servers[n] = e
	}
	for _, it := range d.items {
		if !it.record || it.entry == "" {
			continue
		}
		e, ok := cfg.Servers[it.entry]
		if !ok {
			continue
		}
		if slices.ContainsFunc(e.SupersededTokens, func(r config.SupersededToken) bool { return r.Ref == *it.ref }) {
			continue
		}
		e.SupersededTokens = append(e.SupersededTokens, config.SupersededToken{Ref: *it.ref, UserID: it.owner, ExpiresAt: it.expiresAt,
			SupersededAt: stamp, Server: it.server})
		cfg.Servers[it.entry] = e
		it.wrote, changed = true, true
	}
	return changed, nil
}

// ordered is the order the revokes are sent in: by server, and in each the
// cache tokens, then the records found on the entries, then the login
// tokens, which are the bearer of those records.
func (d *Deletion) ordered() []*revocationItem {
	rank := map[string]int{sourceCache: 0, sourceSuperseded: 1, sourceLogin: 2}
	out := slices.Clone(d.items)
	slices.SortStableFunc(out, func(a, b *revocationItem) int {
		return cmp.Or(strings.Compare(a.server, b.server), cmp.Compare(rank[a.source], rank[b.source]))
	})
	return out
}

// retryLogin is the target whose stored login retries an unconfirmed cache
// token: one read this run, CLI-created and named, of the token's server,
// whose entry records the token's owner, both known.
func (d *Deletion) retryLogin(it *revocationItem) (string, bool) {
	if it.owner == "" {
		return "", false
	}
	for _, n := range d.req.Targets {
		s, ok := d.cfg.Servers[n]
		login := d.loginRead[n]
		if !ok || login == "" || !s.CLICreated() || s.TokenUserID != it.owner || config.CanonicalServerIdentity(s.URL) != it.server {
			continue
		}
		if ref := d.loginRef(n); ref != nil && usableBearer(ref.Kind, ref.Name, login) {
			return n, true
		}
	}
	return "", false
}

// loginRef is the ref of the target's login token as the delete found it.
func (d *Deletion) loginRef(target string) *TokenRef {
	for _, it := range d.items {
		if it.source == sourceLogin && it.loginTarget == target {
			return it.ref
		}
	}
	return nil
}

// Report is the two lists, in send order. A login token the CLI did not
// create or name is listed only when the delete found one or did not
// complete.
func (d *Deletion) Report() RevocationReport {
	rep := RevocationReport{Revoked: []RevokedToken{}, NotRevoked: []NotRevokedToken{}}
	for _, it := range d.ordered() {
		if !d.listed(it) {
			continue
		}
		if it.revoked {
			rep.Revoked = append(rep.Revoked, RevokedToken{Ref: *it.ref, Source: it.source, Server: config.DisplayServerIdentity(it.server)})
			continue
		}
		recovery, loginAs := d.recovery(it)
		n := NotRevokedToken{Ref: it.ref, Source: it.source, Server: config.DisplayServerIdentity(it.server),
			Entry: d.reportedEntry(it), Reason: it.reason, Error: it.errText, Retry: d.retry(it), Recovery: recovery, LoginAs: loginAs}
		if it.tlsErr != nil {
			n.Code, n.Error, n.Certificate = clierr.CodeTLSTrust, d.tlsMessage(it), it.cert
		}
		rep.NotRevoked = append(rep.NotRevoked, n)
	}
	return rep
}

// tlsMessage is the message of a certificate failure, worded for what
// revokes the token now that the command's entries are final. Its remedy
// commands carry the marker; the invocation that prints the message expands
// them.
func (d *Deletion) tlsMessage(it *revocationItem) string {
	msg := it.tlsErr.Message
	if ce := tlstrust.Classify(it.tlsErr.Unwrap(), d.clients[it.trust].hostPort, time.Now(), d.revokeTLSRemedy(it)); ce != nil {
		msg = ce.Message
	}
	return msg
}

// revokeTLSRemedy words a certificate failure of the revoke request by what
// revokes the token: the next login of the entry it is recorded on, the
// recovery login, or a Rancher administrator's delete. The URL clause names
// the entry the failed request used; the CA clause names the entry the token
// is recorded on, which a next login uses.
func (d *Deletion) revokeTLSRemedy(it *revocationItem) tlstrust.Remedy {
	trust := clierr.ShellQuote(it.trust)
	r := tlstrust.Remedy{URL: "the server URL of entry " + trust, Fix: "a different CA"}
	fromVariable := d.clients[it.trust].fromVariable
	if d.retry(it) == retryNextLogin {
		entry := clierr.ShellQuote(it.entry)
		save := clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --cacert <file>", it.entry)
		if fromVariable {
			r.CA = "; the CA came from " + config.EnvCACert + " - unset it, or set it to the server's CA; to save a CA for entry " + entry + ", run: " + save
		} else {
			r.CA = "; save it for entry " + entry + " with: " + save
		}
		return r
	}
	switch {
	case it.ref != nil && it.ref.Kind == TokenKindV3 && fromVariable:
		r.CA = "; the CA came from " + config.EnvCACert + " in this command's environment - " +
			"a Rancher administrator who deletes the token sets it to the server's CA in their own environment"
	case it.ref != nil && it.ref.Kind == TokenKindV3:
		r.CA = "; a Rancher administrator who deletes the token needs that CA saved on their own entry for this server; run: " +
			clierr.Rancher + " config update-server <entry> --cacert <file>"
	case fromVariable:
		r.CA = "; the CA came from " + config.EnvCACert + " - set it to the server's CA before the recovery steps"
	default:
		r.CA = " and add --cacert <file> to the auth login in the recovery steps"
	}
	return r
}

func (d *Deletion) listed(it *revocationItem) bool {
	switch {
	case it.hidden:
		return false
	case it.source == sourceLogin && (it.reason == reasonUserSupplied || it.reason == reasonUnproven):
		return len(d.DeletedFrom[it.loginTarget]) > 0 || !d.loginDeleted[it.loginTarget]
	}
	return true
}

// reportedEntry is the entry a token is recorded on as the command ends:
// none for a record the write after the revokes removed, one on an entry
// the command left for its rerun to remove, or one on an entry that no
// longer exists.
func (d *Deletion) reportedEntry(it *revocationItem) string {
	if it.entry == "" || it.reason == reasonForbidden || d.removed[it.entry] {
		return ""
	}
	if _, ok := d.view.Servers[it.entry]; !ok {
		return ""
	}
	return it.entry
}

// retry is what revokes the token later: the next login the CLI creates
// for the entry it is recorded on, only while that entry records the
// token's owner (both known) and names the token's server; else a manual
// step; none for a pasted token.
func (d *Deletion) retry(it *revocationItem) string {
	switch it.reason {
	case reasonUserSupplied:
		return retryNone
	}
	if it.ref != nil && it.ref.Kind == TokenKindV3 && it.reason != reasonNotDeleted {
		// No later login deletes a v3 token.
		return retryManual
	}
	switch it.reason {
	case reasonUnproven, reasonForbidden, reasonNotDeleted, reasonV3, reasonNoEntry, reasonOtherServer, reasonOtherOwner, reasonUnknownOwner:
		return retryManual
	}
	if it.entry == "" || d.removed[it.entry] {
		return retryManual
	}
	e, ok := d.view.Servers[it.entry]
	if !ok || it.owner == "" || e.TokenUserID != it.owner || config.CanonicalServerIdentity(e.URL) != it.server {
		return retryManual
	}
	return retryNextLogin
}

// recovery is a manual token's way out: the reason's remedy, the
// administrator's delete for a v3 token, or the sequence that logs in as
// its owner and deletes it. loginAs is the user the sequence logs in as,
// empty for every other way out.
func (d *Deletion) recovery(it *revocationItem) (commands []string, loginAs string) {
	if d.retry(it) != retryManual {
		return nil, ""
	}
	switch {
	case it.reason == reasonNotDeleted && d.req.Rerun != "":
		return []string{d.req.Rerun}, ""
	case it.reason == reasonUnproven || it.ref == nil:
		list, _ := clierr.RecoveryCommands(TokenRef{Kind: TokenKindExt})
		return []string{list}, ""
	case it.ref.Kind == TokenKindV3:
		_, revoke := clierr.RecoveryCommands(*it.ref)
		return []string{revoke}, ""
	}
	advice := d.advise(it)
	return advice.Sequence, advice.LoginAs
}

// advise is what config.AdviseRecord says about the server of a manual
// token: where it is reported (the entry it is recorded on, that entry's URL
// as stored, and whether the entry was removed) and the token's server.
func (d *Deletion) advise(it *revocationItem) config.RecordAdvice {
	e, exists := d.view.Servers[it.entry]
	if !exists && d.cfg != nil {
		e = d.cfg.Servers[it.entry]
	}
	site := config.RecordSite{Entry: d.reportedEntry(it), EntryURL: e.URL, Removed: it.entry != "" && (d.removed[it.entry] || !exists)}
	return config.AdviseRecord(site, cmp.Or(it.recorded, it.server), it.owner, *it.ref)
}

// serverCATail says how a login connects to a server whose certificate a
// private CA signs.
const serverCATail = "add --cacert <file with the server's CA certificate> to the login, or set " + config.EnvCACert + " to that file"

// Warnings are one line per manual token, its remedy marked, and one line
// per entry for the tokens its next login revokes.
func (d *Deletion) Warnings() []string {
	var out []string
	next := map[string]int{}
	needsCA := map[string]int{}
	for _, it := range d.ordered() {
		if !d.listed(it) || it.revoked {
			continue
		}
		switch d.retry(it) {
		case retryNextLogin:
			next[it.entry]++
			if it.cert != nil && it.cert.Kind == tlstrust.KindUnknownAuthority && !d.clients[it.trust].fromVariable {
				needsCA[it.entry]++
			}
		case retryManual:
			out = append(out, d.manualWarning(it))
		}
	}
	for _, e := range slices.Sorted(maps.Keys(next)) {
		line := fmt.Sprintf("%d token(s) recorded on %s are revoked by %s", next[e], e, d.nextLoginText(e, true))
		if k := needsCA[e]; k > 0 {
			// Only a CA fixes an unknown authority, and the next login meets
			// the same certificate until the entry saves it.
			save := clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --cacert <file>", e)
			switch {
			case next[e] == 1:
				line += "; its revoke failed on the server's certificate; first save the server's CA with: " + save
			case k == next[e]:
				line += "; their revokes failed on the server's certificate; first save the server's CA with: " + save
			case k == 1:
				line += "; the revoke of 1 of them failed on the server's certificate; first save the server's CA with: " + save
			default:
				line += fmt.Sprintf("; the revokes of %d of them failed on the server's certificate; first save the server's CA with: %s", k, save)
			}
		}
		out = append(out, line)
	}
	return out
}

func (d *Deletion) manualWarning(it *revocationItem) string {
	what := describe(it)
	switch it.reason {
	case reasonNotDeleted:
		return fmt.Sprintf("the %s was not revoked: its local copy was not deleted and it stays in use; to delete it, run: %s", what, d.req.Rerun)
	case reasonUnproven:
		return fmt.Sprintf("the %s was not revoked: nothing proves the CLI created it; find it with: %s", what, clierr.Rancher+" get tokens")
	case reasonV3:
		return fmt.Sprintf("the %s was not revoked: %s", what, v3Remedy(*it.ref, it.expiresAt))
	}
	if it.ref != nil && it.ref.Kind == TokenKindV3 {
		return fmt.Sprintf("the %s was not revoked (%s); %s", what, reasonText(it), v3AdminRemedy(*it.ref, it.expiresAt))
	}
	advice := d.advise(it)
	if advice.Text != "" {
		return fmt.Sprintf("the %s was not revoked (%s); %s", what, reasonText(it), advice.Text)
	}
	revoke := "revoke it as " + config.RecordOwnerOrAdministrator + " with: "
	if advice.LoginAs != "" {
		revoke = "revoke it as user " + clierr.ShellQuote(advice.LoginAs) + " with: "
	}
	text := fmt.Sprintf("the %s was not revoked (%s); the sequence %s; %s%s", what, reasonText(it), clierr.RecoverySequenceAssumption, revoke, strings.Join(advice.Sequence, "; "))
	switch {
	case it.reason == reasonNoEntry:
		// No entry names the server, so the login in the sequence has no CA
		// to connect with; a private CA has to come from the operator.
		text += "; for a server whose certificate a private CA signs, " + serverCATail
	case it.cert != nil && it.cert.Kind == tlstrust.KindUnknownAuthority:
		// Only a CA fixes an unknown authority, and this line leaves out
		// the message that says so.
		text += "; " + serverCATail
	}
	return text
}

// nextLoginText names the login that revokes an entry's recorded tokens,
// with the command that makes it when withCommand is set.
func (d *Deletion) nextLoginText(entry string, withCommand bool) string {
	e := d.view.Servers[entry]
	as := ""
	if owner := clierr.WithoutUnsafeToDisplay(e.TokenUserID); owner != "" {
		as = " as " + owner
	}
	if e.Provenance == clierr.ProvenanceExecLogin || config.IsExecLoginEntryName(entry) {
		return "the next kubectl sign-in through " + entry + as
	}
	text := "the next login of " + entry + as
	if withCommand {
		text += "; run it with: " + clierr.Rancher + " auth login" + clierr.ShellQuoteArgs("", entry)
	}
	return text
}

// describe names a token in text: its ref and what it is.
func describe(it *revocationItem) string {
	kind := map[string]string{sourceCache: cacheTokenText(it), sourceSuperseded: "superseded token", sourceLogin: "login token"}[it.source]
	if it.ref == nil {
		return kind + " with no name recorded"
	}
	return kind + " " + it.ref.String()
}

// cacheTokenText names a cache token in text: a token with no cluster is
// a cached token, one with a cluster a cluster token.
func cacheTokenText(it *revocationItem) string {
	if it.cluster == "" {
		return "cached token"
	}
	return "cluster token"
}

func reasonText(it *revocationItem) string {
	if it.tlsErr != nil {
		return it.reason + ", " + clierr.CodeTLSTrust
	}
	if it.errText != "" {
		return it.reason + ": " + it.errText
	}
	return it.reason
}

// TextLines renders the report under each server: one line per token.
func (d *Deletion) TextLines() []string {
	var lines []string
	server := "\x00"
	for _, it := range d.ordered() {
		if !d.listed(it) {
			continue
		}
		if it.server != server {
			server = it.server
			header := "tokens on " + config.DisplayServerIdentity(server) + ":"
			if server == "" {
				header = "tokens with no server recorded:"
			}
			lines = append(lines, header)
		}
		lines = append(lines, "  "+d.textLine(it))
		if it.tlsErr != nil && !it.revoked {
			for _, l := range certificateFailureLines(d.tlsMessage(it), it.cert) {
				lines = append(lines, "    "+l)
			}
		}
	}
	return lines
}

func (d *Deletion) textLine(it *revocationItem) string {
	name := describe(it)
	if it.ref != nil {
		name = output.Printable(it.ref.String())
	}
	if it.revoked {
		what := map[string]string{sourceCache: cacheTokenText(it), sourceSuperseded: "superseded token", sourceLogin: "login token"}[it.source]
		if it.source == sourceCache && it.cluster != "" {
			what += ", " + it.cluster
		}
		return fmt.Sprintf("revoked %s (%s)", name, what)
	}
	if it.reason == reasonUserSupplied {
		if it.ref == nil {
			return "not revoked: the pasted login token, which is not the CLI's to revoke"
		}
		return fmt.Sprintf("not revoked %s (pasted login token)", name)
	}
	detail := runtime.PrintableRemedies(reasonText(it))
	if e := d.reportedEntry(it); e != "" {
		detail += "; recorded on " + e
		if d.retry(it) == retryNextLogin {
			detail += ": " + output.Printable(d.nextLoginText(e, false)) + " revokes it"
		}
	}
	return fmt.Sprintf("not revoked %s (%s)", name, detail)
}

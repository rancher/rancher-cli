package register

import (
	"context"
	"fmt"
	"maps"
	neturl "net/url"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TLSPreflightFunc is the shape of the every-https-path TLS preflight:
// fatal TLS_TRUST on a classified certificate failure, warn-only on a
// connectivity failure, harmless to call for http, and it also fetches the
// server version the registration's min-version check runs against.
// TLSPreflight is the production implementation; login's guided trust
// re-runs it with a candidate certificate, and tests substitute their own.
type TLSPreflightFunc func(ctx context.Context, serverURL, caPEM string) (string, error)

// Probe is what Prepare resolves OUTSIDE any lock and the caller threads
// into the commit: the canonicalized URL, the CA selector this registration
// will persist, the exact bytes that selector resolved to at probe time
// (empty for system roots; an inline PEM's bytes ARE the selector), and the
// min-version warning, if any. The create and update commits re-verify this
// snapshot under the lock before any write.
type Probe struct {
	CanonicalURL string
	CASelector   string
	CABytes      string
	// ConnectionBytes is what the preflight connected with: CABytes, or the
	// bytes of a CA a login took from RANCHER_CLI_CACERT, which is never
	// saved and so never rechecked at commit.
	ConnectionBytes string
	// FromEnvironment says ConnectionBytes came from RANCHER_CLI_CACERT.
	FromEnvironment bool
	VersionWarning  string
	// CAFingerprint is set only when CASelector is a guided-trust-accepted
	// certificate: the fingerprint tlstrust.Classify already computed for
	// the exact candidate this selector encodes. Every other registration
	// path (add-server, update-server, an explicit --cacert) leaves it
	// empty, and the create commit then writes no fingerprint at all - an
	// explicit CA replacement never carries one.
	CAFingerprint string
}

// ResolveCA resolves selector to its PEM bytes for TRUST purposes: only
// under https (a stale or unreadable stored CA must never break an http
// registration, which never touches TLS) and only when selector names
// something (empty selector is system roots, no bytes to resolve).
func ResolveCA(canonicalURL, selector string) (string, error) {
	if !strings.HasPrefix(canonicalURL, "https://") || selector == "" {
		return "", nil
	}
	return config.ResolveCA(selector)
}

// CanonicalizeServerURL canonicalizes rawURL for a registration call,
// producing the standard invalid-URL validation error every registration
// caller surfaces on failure. Callers thread the result into Prepare rather
// than letting it canonicalize the same URL a second time.
func CanonicalizeServerURL(rawURL string) (string, error) {
	canonical, err := credhelper.CanonicalServerURL(rawURL)
	if err != nil {
		return "", clierr.New(clierr.CodeValidation, "invalid server URL: "+rawURL)
	}
	// Scheme is checked here, not only in Prepare, so the
	// error can echo the URL as the operator typed it - canonicalization
	// strips path/query, and a fat-fingered URL is easier to spot whole.
	if !config.IsSupportedScheme(canonical) {
		return "", clierr.New(clierr.CodeValidation,
			"unsupported server URL scheme: "+rawURL+"; use http:// or https://")
	}
	return canonical, nil
}

// Prepare runs every network step a registration needs OUTSIDE
// config.WithLock: CA resolution, the TLS preflight, and the min-version
// floor check, against an already-canonicalized URL (see
// CanonicalizeServerURL). tlsPreflight runs for both schemes (an http server
// can still be probed for its version; TLS classification never fires
// against a non-TLS connection) so the min-version check is never lost under
// http.
func Prepare(ctx context.Context, tlsPreflight TLSPreflightFunc, canonicalURL, caSelector string) (Probe, error) {
	return PrepareConnecting(ctx, tlsPreflight, canonicalURL, caSelector, "")
}

// PrepareConnecting is Prepare for a login that connects with the CA of
// RANCHER_CLI_CACERT (variableCA, a path or a PEM; empty for none) instead
// of caSelector, the CA the entry saves. The preflight uses the variable's
// bytes; the probe still carries the saved selector and its own bytes,
// which the commit rechecks, and the variable's CA is never saved. An
// unreadable saved CA fails first: it is the entry's own state.
func PrepareConnecting(ctx context.Context, tlsPreflight TLSPreflightFunc, canonicalURL, caSelector, variableCA string) (Probe, error) {
	if !config.IsSupportedScheme(canonicalURL) {
		// The same gate config.Effective's credential-read path applies
		// (resolve.go) - a scheme it refuses must never reach a committed
		// entry or token: Effective would silently withhold the stored
		// credential later, surfacing as an unexplained NOT_LOGGED_IN after
		// a login that appeared to succeed.
		return Probe{}, clierr.New(clierr.CodeValidation,
			"unsupported server URL scheme: "+canonicalURL+"; use http:// or https://")
	}
	caBytes, raerr := ResolveCA(canonicalURL, caSelector)
	if raerr != nil {
		return Probe{}, clierr.New(clierr.CodeValidation, "reading --cacert: "+raerr.Error())
	}
	connection, fromEnvironment := caBytes, false
	if variableCA != "" {
		b, verr := ResolveCA(canonicalURL, variableCA)
		if verr != nil {
			return Probe{}, clierr.New(clierr.CodeValidation, "reading "+config.EnvCACert+": "+verr.Error())
		}
		connection, fromEnvironment = b, b != ""
	}
	var version string
	if tlsPreflight != nil {
		v, perr := tlsPreflight(ctx, canonicalURL, connection)
		if perr != nil {
			return Probe{}, perr
		}
		version = v
	}
	warning := config.UnknownServerVersionWarning
	if version != "" {
		_, warning = config.EvalMinVersionSupport(version)
	}
	return Probe{
		CanonicalURL:    canonicalURL,
		CASelector:      caSelector,
		CABytes:         caBytes,
		ConnectionBytes: connection,
		FromEnvironment: fromEnvironment,
		VersionWarning:  warning,
	}, nil
}

// CAPresence summarizes a stored CA selector's kind for a result envelope -
// presence and shape only, never the bytes: "none" (system roots), "inline"
// (a literal PEM), or "path" (a file path).
func CAPresence(selector string) string {
	switch {
	case selector == "":
		return "none"
	case strings.Contains(selector, "-----BEGIN"):
		return "inline"
	default:
		return "path"
	}
}

// registrationConflict builds the CONFLICT a commit returns when the
// re-verified state no longer matches what Prepare verified -
// naming step identifies which side of the commit drifted, for the message
// only. This seam is shared by add's create path and update's two
// validations, so it cannot know which top-level command the caller ran; a
// hint would have to guess the verb (and update's callers already have an
// entry that exists, so an add-server suggestion would itself conflict) -
// there is no value here the CLI can name, so guidance stays in the message
// and no hint is emitted. cause, when non-nil, is the resolution error a
// failed recheckBytes call carried - a transient read failure (a brief
// permission change, an antivirus lock, an NFS hiccup) fails the same byte
// comparison as genuine concurrent content drift, so naming it lets an
// operator tell the two apart instead of guessing from unrelated timing.
// No taxonomy change: CONFLICT stays the code either way.
func registrationConflict(name, step string, cause error) *clierr.Error {
	msg := fmt.Sprintf("server entry %q changed during registration (%s); no changes were written - re-run the command", name, step)
	if cause != nil {
		msg = fmt.Sprintf("server entry %q changed during registration (%s: %v); no changes were written - re-run the command", name, step, cause)
	}
	return clierr.New(clierr.CodeConflict, msg)
}

// CAUpdateArg renders the update-server flag that would set an entry's CA to
// selector: --clear-cacert for system roots (update-server has no --cacert
// spelling for "no CA"), else --cacert <selector> shell-quoted.
func CAUpdateArg(selector string) string {
	if selector == "" {
		return "--clear-cacert"
	}
	return "--cacert " + clierr.ShellQuote(selector)
}

// recheckBytes re-resolves selector under canonicalURL and requires the
// result to equal want exactly - a resolution failure (rotated-away path) is
// itself a mismatch. Both add's existing-identical baseline and update's two
// validations funnel through this: a "verified unchanged" or "verified
// trusted" claim must rest on bytes actually re-read at commit time, never
// the probe-time snapshot alone. The returned error is the resolution
// failure, if any - nil on a genuine content mismatch - so a caller
// building a CONFLICT message can name what actually went wrong.
func recheckBytes(canonicalURL, selector, want string) (bool, error) {
	now, err := ResolveCA(canonicalURL, selector)
	return err == nil && now == want, err
}

// CreateOutcome is the create commit's result (add-server, and login's
// delegated add), threaded into the caller's Result envelope. Created is
// entry-scoped: true only when this call wrote servers[name] for the first
// time, false for an identical existing entry. CurrentServer is the
// pointer's POST-COMMIT value, and PointerRepaired reports whether this
// call bootstrapped or repaired it.
type CreateOutcome struct {
	Name            string
	CanonicalURL    string
	CASelector      string
	Created         bool
	CurrentServer   string
	PointerRepaired bool
	// Notices are the removals a login's registration made of credentials
	// left under the name it created; empty for add-server.
	Notices []string
}

// bootstrapPointer sets cfg.CurrentServer to name when the pointer is empty
// or dangling (names no configured server), and reports whether it did.
// Shared by commitCreate and commitUpdate - pointer bootstrap/repair is a
// registration-only privilege; plain login and use-server never call this.
func bootstrapPointer(cfg *config.Config, name string) bool {
	if cfg.CurrentServer != "" {
		if _, ok := cfg.Servers[cfg.CurrentServer]; ok {
			return false
		}
	}
	cfg.CurrentServer = name
	return true
}

// commitCreate is the commit-time half of add-server and login's delegated
// add (never update-server, which runs commitUpdate instead): called under
// config.WithLock immediately before any write, with the exact probe
// Prepare produced. It re-resolves probe.CASelector's bytes and
// requires them to still equal probe.CABytes - a path-valued CA can rotate
// between the outside-the-lock probe and this commit, on both the
// name-absent and the existing-identical path - then requires the name to
// be absent, or present with the same canonical URL and CA selector
// (whether it existed when Prepare ran or was concurrently
// created identically). Anything else is CONFLICT with no write. A commit
// that proceeds on an absent name creates the entry; one that proceeds on an
// identical existing entry is an entry no-op - servers[name] is not
// rewritten, but the current-server pointer is still bootstrapped/repaired
// when empty or dangling, and cfg.Write() runs only when something actually
// changed (the entry, the pointer, or both).
func commitCreate(cfg *config.Config, name string, probe Probe) (CreateOutcome, error) {
	return commitCreateWith(cfg, name, probe, nil)
}

// commitCreateWith is commitCreate with beforeCreate run once the name is
// known to be absent and every check has passed, just before the entry is
// added; its error stops the commit with nothing written.
func commitCreateWith(cfg *config.Config, name string, probe Probe, beforeCreate func() error) (CreateOutcome, error) {
	if ok, rerr := recheckBytes(probe.CanonicalURL, probe.CASelector, probe.CABytes); !ok {
		return CreateOutcome{}, registrationConflict(name, "CA content changed", rerr)
	}

	existing, exists := cfg.Servers[name]
	created := false
	switch {
	case !exists:
		if beforeCreate != nil {
			if err := beforeCreate(); err != nil {
				return CreateOutcome{}, err
			}
		}
		// A new entry starts with a cache binding id of its own, so no
		// record an earlier entry of the same name left can match it.
		e := config.Server{URL: probe.CanonicalURL, CA: probe.CASelector, CAFingerprint: probe.CAFingerprint}
		if probe.CASelector == "" {
			e.CAFromEnvironment = probe.FromEnvironment
		}
		if err := config.AdvanceCacheBinding(&e); err != nil {
			return CreateOutcome{}, err
		}
		cfg.Servers[name] = e
		created = true
	case existing.URL == probe.CanonicalURL && existing.CA == probe.CASelector:
		// Entry no-op: identical existing entry, nothing to rewrite.
	case existing.URL != probe.CanonicalURL:
		return CreateOutcome{}, clierr.New(clierr.CodeConflict,
			fmt.Sprintf(
				"server entry %q already points at %s; to add a new server choose a different name, or to change this entry's URL run: rancher config update-server %s --url %s",
				name, existing.URL, name, probe.CanonicalURL)).
			WithHint(fmt.Sprintf("rancher config update-server %s --url %s", clierr.ShellQuote(name), clierr.ShellQuote(probe.CanonicalURL)))
	default:
		// URL matches; only the CA selector differs.
		return CreateOutcome{}, clierr.New(clierr.CodeConflict,
			fmt.Sprintf(
				"server entry %q already exists with a different CA; to add a new server choose a different name, or to change this entry's trust run: rancher config update-server %s %s",
				name, name, CAUpdateArg(probe.CASelector))).
			WithHint(fmt.Sprintf("rancher config update-server %s %s", clierr.ShellQuote(name), CAUpdateArg(probe.CASelector)))
	}

	repaired := bootstrapPointer(cfg, name)
	if created || repaired {
		if err := cfg.Write(); err != nil {
			return CreateOutcome{}, err
		}
	}
	return CreateOutcome{
		Name: name, CanonicalURL: probe.CanonicalURL, CASelector: probe.CASelector,
		Created: created, CurrentServer: cfg.CurrentServer, PointerRepaired: repaired,
	}, nil
}

// CommitCreateUnderLock is add-server's and login's delegated-add's shared
// commit step: load the current config under config.WithLock and run
// commitCreate against it. Kept in one place so a future change to this
// lock/load/commit sequence (lock scope, error wrapping, load-inside-lock
// ordering) can't drift between the two callers.
//
// Before a new name's entry is written, any credential left under that
// name is removed (config.RemoveOrphanCredentials), in the same lock, from
// the keychain, the file store and every credential helper the directory's
// helper records name for it, and each removal comes back in Notices - on
// an error too, carried on it. Left beside the new entry it would count as
// the entry's credential.
func CommitCreateUnderLock(ctx context.Context, dir, name string, probe Probe) (CreateOutcome, error) {
	var out CreateOutcome
	err := config.WithLock(ctx, dir, func() error {
		cfg, err := config.Load(config.FilePath(dir))
		if err != nil {
			return err
		}
		var notices []string
		out, err = commitCreateWith(cfg, name, probe, func() error {
			ns, rerr := config.RemoveOrphanCredentials(ctx, dir, cfg, name)
			notices = ns
			return rerr
		})
		out.Notices = notices
		return err
	})
	return out, config.WithNotices(err, out.Notices)
}

// UpdateSource is update-server's pre-probe snapshot of the entry being
// changed, captured by the caller before Prepare runs (the probe verifies
// the RESULTING selector; the source snapshot is compared independently at
// commit).
type UpdateSource struct {
	CanonicalURL string
	CASelector   string
	CABytes      string
	// ClearCA is set by an update that asked for system roots
	// (--clear-cacert): on an entry with no CA that records a login made
	// with RANCHER_CLI_CACERT, that is a change, because it clears the
	// record.
	ClearCA bool
}

// PrepareUpdateSource snapshots existing's canonical URL, CA selector, and
// (for a path-valued selector) resolved CA bytes, OUTSIDE any lock, before
// Prepare resolves the RESULTING selector - the two snapshots are
// independent and ValidateUpdate checks both.
func PrepareUpdateSource(existing config.Server) (UpdateSource, error) {
	caBytes, err := ResolveCA(existing.URL, existing.CA)
	if err != nil {
		return UpdateSource{}, clierr.New(clierr.CodeValidation, "reading stored CA: "+err.Error())
	}
	return UpdateSource{CanonicalURL: existing.URL, CASelector: existing.CA, CABytes: caBytes}, nil
}

// UpdateOutcome is the update commit's result. Unchanged is entry-scoped:
// true when servers[name] was not rewritten (a same-value update).
// ChangedFields names which of "url"/"ca" actually changed, for
// ServerUpdated's result. CredentialsCleared is true exactly when the URL
// changed (the caller performs the actual credential purge; this only
// reports that one is required).
type UpdateOutcome struct {
	Name               string
	CanonicalURL       string
	CASelector         string
	Unchanged          bool
	ChangedFields      []string
	CredentialsCleared bool
	CurrentServer      string
	PointerRepaired    bool
}

// UpdatePlan is the update commit's validated, not-yet-applied result: the
// entry has not been written yet, so a caller that must purge credentials
// on a URL change can do so BETWEEN validation and the write - delete-first
// ordering, matching delete-server: a purge failure must never leave
// config.yaml already pointed at the new, credential-less address.
type UpdatePlan struct {
	Name               string
	CanonicalURL       string
	CASelector         string
	ChangedFields      []string
	CredentialsCleared bool
}

// ValidateUpdate runs update-server's TWO independently named validations,
// called under config.WithLock before any purge or write. (1) SOURCE
// BASELINE: the entry must still exist and match src exactly - canonical
// URL, CA selector, and (for a path-valued source selector) resolved source
// bytes; absence, field drift, or source-byte drift is CONFLICT. (2) RESULT
// TRUST BINDING: the resulting selector (probe) must still resolve to the
// exact bytes the verification probe used; resolution failure or result-byte
// drift is CONFLICT. It performs no mutation.
func ValidateUpdate(cfg *config.Config, name string, src UpdateSource, probe Probe) (UpdatePlan, error) {
	existing, exists := cfg.Servers[name]
	if !exists || existing.URL != src.CanonicalURL || existing.CA != src.CASelector {
		return UpdatePlan{}, registrationConflict(name, "source entry changed", nil)
	}
	if ok, rerr := recheckBytes(src.CanonicalURL, src.CASelector, src.CABytes); !ok {
		return UpdatePlan{}, registrationConflict(name, "source CA content changed", rerr)
	}
	if ok, rerr := recheckBytes(probe.CanonicalURL, probe.CASelector, probe.CABytes); !ok {
		return UpdatePlan{}, registrationConflict(name, "resulting CA content changed", rerr)
	}
	var changed []string
	if existing.URL != probe.CanonicalURL {
		changed = append(changed, "url")
	}
	if existing.CA != probe.CASelector || (src.ClearCA && existing.CA == "" && existing.CAFromEnvironment) {
		changed = append(changed, "ca")
	}
	return UpdatePlan{
		Name: name, CanonicalURL: probe.CanonicalURL, CASelector: probe.CASelector,
		ChangedFields: changed, CredentialsCleared: existing.URL != probe.CanonicalURL,
	}, nil
}

// ApplyUpdate writes plan's result (only when something changed) and
// bootstraps/repairs the pointer when empty or dangling - even on a
// same-value update, for the same reason commitCreate's entry no-op does.
// Callers that must purge credentials on a URL change do so between
// ValidateUpdate and ApplyUpdate.
func ApplyUpdate(cfg *config.Config, plan UpdatePlan) (UpdateOutcome, error) {
	if len(plan.ChangedFields) > 0 {
		e := cfg.Servers[plan.Name]
		if plan.CredentialsCleared {
			// A URL change clears the entry's credentials, so the entry
			// keeps only its address, its trust and what its change's first
			// write gave it: the advanced cache binding id, the pause, which
			// the caller has cleared when it may, and the records of the
			// tokens it left on the old server, each naming that server so
			// no login of the new address tries them.
			e = config.Server{URL: plan.CanonicalURL, CA: plan.CASelector,
				CacheBindingID: e.CacheBindingID, CacheWritesPaused: e.CacheWritesPaused, SupersededTokens: e.SupersededTokens}
		} else {
			// A CA-only update changes no credential: every other field
			// is kept, a pause a change left included. The fingerprint
			// belonged to the certificate being replaced.
			e.URL, e.CA, e.CAFingerprint, e.CAFromEnvironment = plan.CanonicalURL, plan.CASelector, "", false
		}
		cfg.Servers[plan.Name] = e
	}
	repaired := bootstrapPointer(cfg, plan.Name)
	if len(plan.ChangedFields) > 0 || repaired {
		if err := config.WriteStep("update", cfg); err != nil {
			return UpdateOutcome{}, err
		}
	}
	return UpdateOutcome{
		Name: plan.Name, CanonicalURL: plan.CanonicalURL, CASelector: plan.CASelector,
		Unchanged: len(plan.ChangedFields) == 0, ChangedFields: plan.ChangedFields,
		CredentialsCleared: plan.CredentialsCleared,
		CurrentServer:      cfg.CurrentServer, PointerRepaired: repaired,
	}, nil
}

// AuthSnapshot is what login captures immediately before starting the auth
// flow: the exact name, canonical URL, CA selector, and resolved CA bytes
// authentication runs under. It is compared against the CURRENT entry at
// two points - the credential-write commit seam, and the stale-hint recheck
// after a failed delegated login - both via SnapshotMatchesCurrent, so the
// two can never diverge into separately-maintained comparisons.
type AuthSnapshot struct {
	Name         string
	CanonicalURL string
	CASelector   string
	CABytes      string
}

// SnapshotMatchesCurrent reports whether name's CURRENT entry in cfg still
// matches snap exactly: canonical URL, CA selector, and (for a path-valued
// selector) the resolved bytes re-read right now - never the stored selector
// string alone, which cannot see a file that rotated in place.
func SnapshotMatchesCurrent(cfg *config.Config, snap AuthSnapshot) bool {
	cur, ok := cfg.Servers[snap.Name]
	if !ok || cur.URL != snap.CanonicalURL || cur.CA != snap.CASelector {
		return false
	}
	matches, _ := recheckBytes(snap.CanonicalURL, snap.CASelector, snap.CABytes)
	return matches
}

// commitUpdate composes ValidateUpdate and ApplyUpdate with no credential
// purge in between - the shape callers without a URL-change side effect (and
// tests) use directly. configUpdateServerCmd calls the two steps separately
// so it can purge credentials between them.
func commitUpdate(cfg *config.Config, name string, src UpdateSource, probe Probe) (UpdateOutcome, error) {
	plan, err := ValidateUpdate(cfg, name, src, probe)
	if err != nil {
		return UpdateOutcome{}, err
	}
	return ApplyUpdate(cfg, plan)
}

// TLSPreflight performs the every-https-path TLS preflight: an
// unauthenticated GET against the same /rancherversion endpoint the
// version probe uses, built with caPEM as the trusted root (system roots
// when empty). Only the TLS handshake matters here - any response status
// proves the certificate verified. A classified certificate-verification
// failure returns tlstrust's structured TLS_TRUST error. Every other
// failure (DNS, connection refused, timeout - the server is unreachable for
// reasons that have nothing to do with certificate trust) is deliberately
// NOT surfaced as a failure here: that stays warn-only, folded into the
// registration min-version check's "cannot determine" posture.
func TLSPreflight(ctx context.Context, serverURL, caPEM string) (string, error) {
	v, err := provider.RancherVersion(ctx, config.Effective{URL: serverURL, CA: caPEM})
	if err != nil {
		if ce := tlstrust.Classify(err, ServerLabel(serverURL), time.Now(), tlstrust.Remedy{}); ce != nil {
			return "", ce
		}
		return "", nil
	}
	return v, nil
}

// ServerCandidates returns every configured server as a selectable
// candidate, sorted by name.
func ServerCandidates(cfg *config.Config) []clierr.Candidate {
	names := slices.Sorted(maps.Keys(cfg.Servers))
	cands := make([]clierr.Candidate, 0, len(names))
	for _, n := range names {
		cands = append(cands, clierr.Candidate{ID: n, Name: n})
	}
	return cands
}

// ServerNotFound builds the NOT_FOUND error that `config delete-server`,
// `config update-server`, `config use-server`, `auth logout <name>` and
// `auth login <name>` return for an unknown server name: every currently
// configured server offered back as a selectable candidate, from one
// implementation.
func ServerNotFound(cfg *config.Config, name string) *clierr.Error {
	return clierr.New(clierr.CodeNotFound, "server not found: "+name).
		WithCandidates(ServerCandidates(cfg))
}

// ServerLabel renders the server as host[:port] with a scheme-default port
// omitted, the spelling the origin-attributed classifier uses, so the plain
// and provider login paths name the same server identically in a
// certificate error. Falls back to the URL's host, lowercased like the
// origin spelling, when the URL has no usable origin.
func ServerLabel(serverURL string) string {
	if u, err := neturl.Parse(serverURL); err == nil {
		if origin, oerr := httpx.OriginOf(u); oerr == nil {
			return origin.HostPort()
		}
	}
	return strings.ToLower(config.ServerHost(serverURL))
}

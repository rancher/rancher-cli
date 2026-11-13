package register

import (
	"context"
	"crypto/x509"
	"encoding/pem"
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
// caller surfaces on failure. entry is "" for a URL typed with --url, or the
// name of the stored entry the URL belongs to: the error names that source
// and never the URL, which may hold a password. A URL that carries a user
// name or password is refused. Callers thread the result into Prepare rather
// than letting it canonicalize the same URL a second time.
func CanonicalizeServerURL(rawURL, entry string) (string, error) {
	origin := config.ServerURLOrigin{Source: "--url"}
	if entry != "" {
		origin = config.EntryOrigin(entry)
	}
	canonical, err := credhelper.CheckServerURL(rawURL)
	if err != nil {
		return "", origin.Refusal(err)
	}
	if !config.IsSupportedScheme(canonical) {
		return "", origin.Refusal(config.ErrServerURLScheme)
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
		return Probe{}, config.ServerURLOrigin{Source: "--url"}.Refusal(config.ErrServerURLScheme)
	}
	caBytes, raerr := ResolveCA(canonicalURL, caSelector)
	if raerr != nil {
		return Probe{}, clierr.New(clierr.CodeValidation, "reading --cacert: "+config.CAFileProblem(raerr))
	}
	connection, fromEnvironment := caBytes, false
	if variableCA != "" {
		b, verr := ResolveCA(canonicalURL, variableCA)
		if verr != nil {
			return Probe{}, clierr.New(clierr.CodeValidation, "reading "+config.EnvCACert+": "+config.CAFileProblem(verr))
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

// CAUpdateArg is the update-server flag that would set an entry's CA to
// selector: --clear-cacert for system roots (update-server has no --cacert
// spelling for "no CA"), else --cacert with the selector.
func CAUpdateArg(selector string) clierr.CommandOption {
	return func(c clierr.Command) clierr.Command {
		if selector == "" {
			return c.Flag("--clear-cacert")
		}
		return c.FlagValue("--cacert", selector)
	}
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
// time, false for an existing entry that is kept as it is. CurrentServer is the
// pointer's POST-COMMIT value, and PointerRepaired reports whether this
// call bootstrapped or repaired it.
type CreateOutcome struct {
	Name            string
	CanonicalURL    string
	CASelector      string
	Created         bool
	CurrentServer   string
	PointerRepaired bool
	// KeptSavedCA is true when the entry already had a CA, the caller named
	// another one, and both hold the same certificates: the entry stays as
	// saved, CASelector and CABytes are the saved CA's, and the caller says
	// so in a warning.
	KeptSavedCA bool
	// CABytes is the PEM of CASelector as the registration resolved it (empty
	// for system roots and under http). It is the probe's bytes, except when
	// KeptSavedCA is true: then it is the saved selector's bytes, which the
	// login that follows binds to the entry.
	CABytes string
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

// certificateFingerprints returns the SHA-256 fingerprints of the
// certificates a TLS root pool built from pemText would hold, sorted and
// without repeats: the PEM blocks of type CERTIFICATE with no headers that
// x509.ParseCertificate accepts. Any other block, and a block that does not
// parse, is skipped. Line endings may be LF or CRLF.
func certificateFingerprints(pemText string) []string {
	var fingerprints []string
	rest := []byte(pemText)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		fingerprints = append(fingerprints, tlstrust.Fingerprint(cert))
	}
	slices.Sort(fingerprints)
	return slices.Compact(fingerprints)
}

// caSide is one side of a CA comparison: the certificates it holds, or the
// reason it has none to compare.
type caSide struct {
	fingerprints []string
	// note is set when fingerprints is empty: "none (system roots)" for an
	// empty selector, or why the side cannot be compared.
	note string
}

// caSideOf describes the CA a selector names; text returns the PEM the
// selector resolves to. The result never carries the selector or the path:
// an unreadable file is described by config.CAFileProblem.
func caSideOf(selector string, text func() (string, error)) caSide {
	if selector == "" {
		return caSide{note: "none (system roots)"}
	}
	pemText, err := text()
	if err != nil {
		return caSide{note: "cannot be compared: " + config.CAFileProblem(err)}
	}
	fingerprints := certificateFingerprints(pemText)
	if len(fingerprints) == 0 {
		return caSide{note: "cannot be compared: the text holds no certificate"}
	}
	return caSide{fingerprints: fingerprints}
}

// sameCertificates reports whether both sides hold certificates and the
// same ones. A side that is empty or cannot be compared is never the same.
func sameCertificates(a, b caSide) bool {
	return len(a.fingerprints) > 0 && slices.Equal(a.fingerprints, b.fingerprints)
}

// text renders the side for the conflict message: the fingerprints the
// other side lacks (all of them when the other side holds none), or the
// note.
func (s caSide) text(other caSide) string {
	if len(s.fingerprints) == 0 {
		return s.note
	}
	var missing []string
	for _, f := range s.fingerprints {
		if !slices.Contains(other.fingerprints, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return "no certificate the other side lacks"
	}
	return strings.Join(missing, ", ")
}

// KeptSavedCAWarning is the warning a caller adds when the entry kept its
// saved CA because the CA it was given holds the same certificates. It names
// no flag: the given CA is a --cacert value, or a certificate accepted at
// the trust prompt.
func KeptSavedCAWarning(name string) string {
	return fmt.Sprintf("server entry %q keeps its saved CA, which holds the same certificates", name)
}

// commitCreate is the commit-time half of add-server and login's delegated
// add (never update-server, which runs commitUpdate instead): called under
// config.WithLock immediately before any write, with the exact probe
// Prepare produced. It re-resolves probe.CASelector's bytes and
// requires them to still equal probe.CABytes - a path-valued CA can rotate
// between the outside-the-lock probe and this commit, on both the
// name-absent and the existing-identical path - then requires the name to
// be absent, or present with the same canonical URL and a CA that holds the
// same certificates (whether it existed when Prepare ran or was concurrently
// created identically). Anything else is CONFLICT with no write. A commit
// that proceeds on an absent name creates the entry; one that proceeds on an
// existing entry is an entry no-op - servers[name] is not rewritten, even
// when the CA selector is other text with the same certificates, such as a
// copy on another path - but the current-server pointer is still
// bootstrapped/repaired when empty or dangling, and cfg.Write() runs only
// when something actually changed (the entry, the pointer, or both).
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
	keptSavedCA := false
	caSelector, caBytes := probe.CASelector, probe.CABytes
	switch {
	case !exists:
		// Before beforeCreate, which removes credentials left under the
		// name: a refused name must not trigger that.
		if ce := config.CheckNewEntryName(name); ce != nil {
			return CreateOutcome{}, ce
		}
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
		hint := clierr.NewCommand("config update-server").Arg(name).FlagValue("--url", probe.CanonicalURL)
		if _, problem := credhelper.CheckServerURL(existing.URL); problem != nil {
			// The entry's URL cannot be shown or compared: the entry is
			// what needs the repair.
			return CreateOutcome{}, clierr.New(clierr.CodeConflict, config.EntryOrigin(name).Text(problem)).WithHintCommand(hint)
		}
		return CreateOutcome{}, clierr.New(clierr.CodeConflict,
			clierr.Text(fmt.Sprintf("server entry %q already points at %s", name, config.DisplayServerURL(existing.URL)),
				"; to add a new server choose a different name",
				clierr.Lead(", or to change this entry's URL run: "), hint)).
			WithHintCommand(hint)
	default:
		// URL matches; only the CA selector differs as text. The saved CA is
		// read once: the text compared is the text a kept entry hands back.
		savedText, savedErr := config.ResolveCA(existing.CA)
		saved := caSideOf(existing.CA, func() (string, error) { return savedText, savedErr })
		given := caSideOf(probe.CASelector, func() (string, error) {
			if strings.HasPrefix(probe.CanonicalURL, "https://") {
				return probe.CABytes, nil
			}
			return config.ResolveCA(probe.CASelector)
		})
		if sameCertificates(saved, given) {
			// The saved selector stays and the login that follows binds to
			// its bytes (empty under http, as ResolveCA returns them), never
			// to this call's.
			keptSavedCA = true
			caSelector, caBytes = existing.CA, ""
			if strings.HasPrefix(probe.CanonicalURL, "https://") {
				caBytes = savedText
			}
			break
		}
		hint := clierr.NewCommand("config update-server").Arg(name).With(CAUpdateArg(probe.CASelector))
		return CreateOutcome{}, clierr.New(clierr.CodeConflict,
			clierr.Text(fmt.Sprintf(
				"server entry %q already exists with a different CA (saved: %s; --cacert: %s)",
				name, saved.text(given), given.text(saved)),
				"; to add a new server choose a different name",
				clierr.Lead(", or to change this entry's trust run: "), hint)).
			WithHintCommand(hint)
	}

	repaired := bootstrapPointer(cfg, name)
	if created || repaired {
		if err := cfg.Write(); err != nil {
			return CreateOutcome{}, err
		}
	}
	return CreateOutcome{
		Name: name, CanonicalURL: probe.CanonicalURL, CASelector: caSelector, CABytes: caBytes,
		Created: created, CurrentServer: cfg.CurrentServer, PointerRepaired: repaired, KeptSavedCA: keptSavedCA,
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
	// UserinfoRemoved is true when the stored URL held a user name or
	// password and was rewritten without it: the server is the same, so no
	// credential is cleared and ChangedFields does not name "url" (the
	// caller's envelope does).
	UserinfoRemoved bool
	CurrentServer   string
	PointerRepaired bool
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
	// RewriteURL is set when the stored URL holds a user name or password
	// and names the same server as the new URL: the entry is rewritten with
	// the canonical URL and nothing else about the server changes.
	RewriteURL bool
}

// storedUserinfoOnly reports whether the stored URL holds a user name or
// password and canonicalizes to canonical: only the user information
// differs, so the entry names the same server.
func storedUserinfoOnly(stored, canonical string) bool {
	u, err := neturl.Parse(stored)
	if err != nil || u.User == nil {
		return false
	}
	return config.CanonicalServerIdentity(stored) == canonical
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
	rewrite := existing.URL != probe.CanonicalURL && storedUserinfoOnly(existing.URL, probe.CanonicalURL)
	if existing.URL != probe.CanonicalURL && !rewrite {
		changed = append(changed, "url")
	}
	if existing.CA != probe.CASelector || (src.ClearCA && existing.CA == "" && existing.CAFromEnvironment) {
		changed = append(changed, "ca")
	}
	return UpdatePlan{
		Name: name, CanonicalURL: probe.CanonicalURL, CASelector: probe.CASelector,
		ChangedFields: changed, CredentialsCleared: existing.URL != probe.CanonicalURL && !rewrite, RewriteURL: rewrite,
	}, nil
}

// ApplyUpdate writes plan's result (only when something changed) and
// bootstraps/repairs the pointer when empty or dangling - even on a
// same-value update, for the same reason commitCreate's entry no-op does.
// Callers that must purge credentials on a URL change do so between
// ValidateUpdate and ApplyUpdate.
func ApplyUpdate(cfg *config.Config, plan UpdatePlan) (UpdateOutcome, error) {
	if len(plan.ChangedFields) > 0 || plan.RewriteURL {
		e := cfg.Servers[plan.Name]
		switch {
		case plan.CredentialsCleared:
			// A URL change clears the entry's credentials, so the entry
			// keeps only its address, its trust and what its change's first
			// write gave it: the advanced cache binding id, the pause, which
			// the caller has cleared when it may, and the records of the
			// tokens it left on the old server, each naming that server so
			// no login of the new address tries them.
			e = config.Server{URL: plan.CanonicalURL, CA: plan.CASelector,
				CacheBindingID: e.CacheBindingID, CacheWritesPaused: e.CacheWritesPaused, SupersededTokens: e.SupersededTokens}
		case len(plan.ChangedFields) > 0:
			// A CA-only update changes no credential: every other field
			// is kept, a pause a change left included. The fingerprint
			// belonged to the certificate being replaced.
			e.URL, e.CA, e.CAFingerprint, e.CAFromEnvironment = plan.CanonicalURL, plan.CASelector, "", false
		default:
			// Only the user information left the URL: the server, its
			// trust and every credential are the same.
			e.URL = plan.CanonicalURL
		}
		cfg.Servers[plan.Name] = e
	}
	repaired := bootstrapPointer(cfg, plan.Name)
	if len(plan.ChangedFields) > 0 || plan.RewriteURL || repaired {
		if err := config.WriteStep("update", cfg); err != nil {
			return UpdateOutcome{}, err
		}
	}
	return UpdateOutcome{
		Name: plan.Name, CanonicalURL: plan.CanonicalURL, CASelector: plan.CASelector,
		Unchanged: len(plan.ChangedFields) == 0 && !plan.RewriteURL, ChangedFields: plan.ChangedFields,
		CredentialsCleared: plan.CredentialsCleared, UserinfoRemoved: plan.RewriteURL,
		CurrentServer: cfg.CurrentServer, PointerRepaired: repaired,
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

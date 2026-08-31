package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// tlsPreflightFunc matches loginCmd.tlsPreflight's shape: the every-https-path
// TLS preflight (fatal TLS_TRUST on a classified certificate failure,
// warn-only on a connectivity failure, harmless to call for http) that also
// fetches the version registration's min-version check relocates onto.
type tlsPreflightFunc func(ctx context.Context, serverURL, caPEM string) (string, error)

// shquote is clierr.ShellQuote under the name this package's hint-building
// call sites already use.
func shquote(s string) string { return clierr.ShellQuote(s) }

// registrationProbe is what prepareRegistration resolves OUTSIDE any lock:
// the canonicalized URL, the CA selector this registration will persist, the
// exact bytes that selector resolved to at probe time (empty for system
// roots; an inline PEM's bytes ARE the selector), and the min-version
// warning, if any. commitCreate and commitUpdate re-verify this snapshot
// under the lock before any write.
type registrationProbe struct {
	canonicalURL   string
	caSelector     string
	caBytes        string
	versionWarning string
}

// resolveRegistrationCA resolves selector to its PEM bytes for TRUST
// purposes: only under https (a stale or unreadable stored CA must never
// break an http registration, which never touches TLS) and only when
// selector names something (empty selector is system roots, no bytes to
// resolve).
func resolveRegistrationCA(canonicalURL, selector string) (string, error) {
	if !strings.HasPrefix(canonicalURL, "https://") || selector == "" {
		return "", nil
	}
	return config.ResolveCA(selector)
}

// prepareRegistration runs every network step a registration needs OUTSIDE
// config.WithLock: URL canonicalization, CA resolution, the TLS preflight,
// and the min-version floor check. tlsPreflight runs for both schemes (an
// http server can still be probed for its version; TLS classification never
// fires against a non-TLS connection) so the min-version check is never lost
// under http.
func prepareRegistration(ctx context.Context, tlsPreflight tlsPreflightFunc, rawURL, caSelector string) (registrationProbe, error) {
	canonicalURL, cerr := credhelper.CanonicalServerURL(rawURL)
	if cerr != nil {
		return registrationProbe{}, clierr.New(clierr.CodeValidation, "invalid server URL: "+rawURL)
	}
	if !config.IsSupportedScheme(canonicalURL) {
		// The same gate config.Effective's credential-read path applies
		// (resolve.go) — a scheme it refuses must never reach a committed
		// entry or token: Effective would silently withhold the stored
		// credential later, surfacing as an unexplained NOT_LOGGED_IN after
		// a login that appeared to succeed.
		return registrationProbe{}, clierr.New(clierr.CodeValidation,
			"unsupported server URL scheme: "+rawURL+"; use http:// or https://")
	}
	caBytes, raerr := resolveRegistrationCA(canonicalURL, caSelector)
	if raerr != nil {
		return registrationProbe{}, clierr.New(clierr.CodeValidation, "reading --cacert: "+raerr.Error())
	}
	var version string
	if tlsPreflight != nil {
		v, perr := tlsPreflight(ctx, canonicalURL, caBytes)
		if perr != nil {
			return registrationProbe{}, perr
		}
		version = v
	}
	warning := config.UnknownServerVersionWarning
	if version != "" {
		_, warning = config.EvalMinVersionSupport(version)
	}
	return registrationProbe{
		canonicalURL:   canonicalURL,
		caSelector:     caSelector,
		caBytes:        caBytes,
		versionWarning: warning,
	}, nil
}

// validateEntryName rejects a server name reserved for the legacy token
// shim's cache slots (names are born at registration now — add-server and
// login's delegated add both call this before any probe or write).
func validateEntryName(name string) *clierr.Error {
	if !config.IsLegacyCacheSlot(name) {
		return nil
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("server names starting with %q are reserved for the legacy token shim's cache slots; use a different name", config.LegacyCachePrefix))
}

// caPresence summarizes a stored CA selector's kind for a result envelope —
// presence and shape only, never the bytes: "none" (system roots), "inline"
// (a literal PEM), or "path" (a file path).
func caPresence(selector string) string {
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
// re-verified state no longer matches what prepareRegistration verified —
// naming step identifies which side of the commit drifted, for the message
// only. This seam is shared by add's create path and update's two
// validations, so it cannot know which top-level command the caller ran; a
// hint would have to guess the verb (and update's callers already have an
// entry that exists, so an add-server suggestion would itself conflict) —
// there is no value here the CLI can name, so guidance stays in the message
// and no hint is emitted. cause, when non-nil, is the resolution error a
// failed recheckBytes call carried — a transient read failure (a brief
// permission change, an antivirus lock, an NFS hiccup) fails the same byte
// comparison as genuine concurrent content drift, so naming it lets an
// operator tell the two apart instead of guessing from unrelated timing.
// No taxonomy change: CONFLICT stays the code either way.
func registrationConflict(name, step string, cause error) *clierr.Error {
	msg := fmt.Sprintf("server entry %q changed during registration (%s); no changes were written — re-run the command", name, step)
	if cause != nil {
		msg = fmt.Sprintf("server entry %q changed during registration (%s: %v); no changes were written — re-run the command", name, step, cause)
	}
	return clierr.New(clierr.CodeConflict, msg)
}

// caUpdateArg renders the update-server flag that would set an entry's CA to
// selector: --clear-cacert for system roots (update-server has no --cacert
// spelling for "no CA"), else --cacert <selector> shell-quoted.
func caUpdateArg(selector string) string {
	if selector == "" {
		return "--clear-cacert"
	}
	return "--cacert " + shquote(selector)
}

// recheckBytes re-resolves selector under canonicalURL and requires the
// result to equal want exactly — a resolution failure (rotated-away path) is
// itself a mismatch. Both add's existing-identical baseline and update's two
// validations funnel through this: a "verified unchanged" or "verified
// trusted" claim must rest on bytes actually re-read at commit time, never
// the probe-time snapshot alone. The returned error is the resolution
// failure, if any — nil on a genuine content mismatch — so a caller
// building a CONFLICT message can name what actually went wrong.
func recheckBytes(canonicalURL, selector, want string) (bool, error) {
	now, err := resolveRegistrationCA(canonicalURL, selector)
	return err == nil && now == want, err
}

// createOutcome is commitCreate's result, threaded into the caller's Result
// envelope. unchanged and created are both entry-scoped: created is true
// only when this call wrote servers[name] for the first time; unchanged is
// its mirror (an identical existing entry, or an update-server no-op).
// currentServer is the pointer's POST-COMMIT value, and pointerRepaired
// reports whether this call bootstrapped or repaired it.
type createOutcome struct {
	name            string
	canonicalURL    string
	caSelector      string
	created         bool
	currentServer   string
	pointerRepaired bool
}

// bootstrapPointer sets cfg.CurrentServer to name when the pointer is empty
// or dangling (names no configured server), and reports whether it did.
// Shared by commitCreate and commitUpdate — pointer bootstrap/repair is a
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
// prepareRegistration produced. It re-resolves probe.caSelector's bytes and
// requires them to still equal probe.caBytes — a path-valued CA can rotate
// between the outside-the-lock probe and this commit, on both the
// name-absent and the existing-identical path — then requires the name to
// be absent, or present with the same canonical URL and CA selector
// (whether it existed when prepareRegistration ran or was concurrently
// created identically). Anything else is CONFLICT with no write. A commit
// that proceeds on an absent name creates the entry; one that proceeds on an
// identical existing entry is an entry no-op — servers[name] is not
// rewritten, but the current-server pointer is still bootstrapped/repaired
// when empty or dangling, and cfg.Write() runs only when something actually
// changed (the entry, the pointer, or both).
func commitCreate(cfg *config.Config, name string, probe registrationProbe) (createOutcome, error) {
	if ok, rerr := recheckBytes(probe.canonicalURL, probe.caSelector, probe.caBytes); !ok {
		return createOutcome{}, registrationConflict(name, "CA content changed", rerr)
	}

	existing, exists := cfg.Servers[name]
	created := false
	switch {
	case !exists:
		cfg.Servers[name] = config.Server{URL: probe.canonicalURL, CA: probe.caSelector}
		created = true
	case existing.URL == probe.canonicalURL && existing.CA == probe.caSelector:
		// Entry no-op: identical existing entry, nothing to rewrite.
	case existing.URL != probe.canonicalURL:
		return createOutcome{}, clierr.New(clierr.CodeConflict,
			fmt.Sprintf(
				"server entry %q already points at %s; to add a new server choose a different name, or to change this entry's URL run: rancher config update-server %s --url %s",
				name, existing.URL, name, probe.canonicalURL)).
			WithHint(fmt.Sprintf("rancher config update-server %s --url %s", shquote(name), shquote(probe.canonicalURL)))
	default:
		// URL matches; only the CA selector differs.
		return createOutcome{}, clierr.New(clierr.CodeConflict,
			fmt.Sprintf(
				"server entry %q already exists with a different CA; to add a new server choose a different name, or to change this entry's trust run: rancher config update-server %s %s",
				name, name, caUpdateArg(probe.caSelector))).
			WithHint(fmt.Sprintf("rancher config update-server %s %s", shquote(name), caUpdateArg(probe.caSelector)))
	}

	repaired := bootstrapPointer(cfg, name)
	if created || repaired {
		if err := cfg.Write(); err != nil {
			return createOutcome{}, err
		}
	}
	return createOutcome{
		name: name, canonicalURL: probe.canonicalURL, caSelector: probe.caSelector,
		created: created, currentServer: cfg.CurrentServer, pointerRepaired: repaired,
	}, nil
}

// commitCreateUnderLock is add-server's and login's delegated-add's shared
// commit step: load the current config under config.WithLock and run
// commitCreate against it. Kept in one place so a future change to this
// lock/load/commit sequence (lock scope, error wrapping, load-inside-lock
// ordering) can't drift between the two callers.
func commitCreateUnderLock(ctx context.Context, dir, name string, probe registrationProbe) (createOutcome, error) {
	var out createOutcome
	err := config.WithLock(ctx, dir, func() error {
		cfg, err := config.Load(config.FilePath(dir))
		if err != nil {
			return err
		}
		out, err = commitCreate(cfg, name, probe)
		return err
	})
	return out, err
}

// updateSource is update-server's pre-probe snapshot of the entry being
// changed, captured by the caller before prepareRegistration runs (the probe
// verifies the RESULTING selector; the source snapshot is compared
// independently at commit).
type updateSource struct {
	canonicalURL string
	caSelector   string
	caBytes      string
}

// prepareUpdateSource snapshots existing's canonical URL, CA selector, and
// (for a path-valued selector) resolved CA bytes, OUTSIDE any lock, before
// prepareRegistration resolves the RESULTING selector — the two snapshots
// are independent and commitUpdate validates both.
func prepareUpdateSource(existing config.Server) (updateSource, error) {
	caBytes, err := resolveRegistrationCA(existing.URL, existing.CA)
	if err != nil {
		return updateSource{}, clierr.New(clierr.CodeValidation, "reading stored CA: "+err.Error())
	}
	return updateSource{canonicalURL: existing.URL, caSelector: existing.CA, caBytes: caBytes}, nil
}

// updateOutcome is commitUpdate's result. unchanged mirrors createOutcome's
// entry-scoped meaning: true when servers[name] was not rewritten (a
// same-value update). changedFields names which of "url"/"ca" actually
// changed, for ServerUpdated's result. credentialsCleared is true exactly
// when the URL changed (the caller performs the actual credential purge;
// this only reports that one is required).
type updateOutcome struct {
	name               string
	canonicalURL       string
	caSelector         string
	unchanged          bool
	changedFields      []string
	credentialsCleared bool
	currentServer      string
	pointerRepaired    bool
}

// updatePlan is commitUpdate's validated, not-yet-applied result: the entry
// has not been written yet, so a caller that must purge credentials on a URL
// change can do so BETWEEN validation and the write — delete-first ordering,
// matching delete-server: a purge failure must never leave config.yaml
// already pointed at the new, credential-less address.
type updatePlan struct {
	name               string
	canonicalURL       string
	caSelector         string
	changedFields      []string
	credentialsCleared bool
}

// validateUpdate runs update-server's TWO independently named validations,
// called under config.WithLock before any purge or write. (1) SOURCE
// BASELINE: the entry must still exist and match src exactly — canonical
// URL, CA selector, and (for a path-valued source selector) resolved source
// bytes; absence, field drift, or source-byte drift is CONFLICT. (2) RESULT
// TRUST BINDING: the resulting selector (probe) must still resolve to the
// exact bytes the verification probe used; resolution failure or
// result-byte drift is CONFLICT. It performs no mutation.
func validateUpdate(cfg *config.Config, name string, src updateSource, probe registrationProbe) (updatePlan, error) {
	existing, exists := cfg.Servers[name]
	if !exists || existing.URL != src.canonicalURL || existing.CA != src.caSelector {
		return updatePlan{}, registrationConflict(name, "source entry changed", nil)
	}
	if ok, rerr := recheckBytes(src.canonicalURL, src.caSelector, src.caBytes); !ok {
		return updatePlan{}, registrationConflict(name, "source CA content changed", rerr)
	}
	if ok, rerr := recheckBytes(probe.canonicalURL, probe.caSelector, probe.caBytes); !ok {
		return updatePlan{}, registrationConflict(name, "resulting CA content changed", rerr)
	}
	var changed []string
	if existing.URL != probe.canonicalURL {
		changed = append(changed, "url")
	}
	if existing.CA != probe.caSelector {
		changed = append(changed, "ca")
	}
	return updatePlan{
		name: name, canonicalURL: probe.canonicalURL, caSelector: probe.caSelector,
		changedFields: changed, credentialsCleared: existing.URL != probe.canonicalURL,
	}, nil
}

// applyUpdate writes plan's result (only when something changed) and
// bootstraps/repairs the pointer when empty or dangling — even on a
// same-value update, for the same reason commitCreate's entry no-op does.
// Callers that must purge credentials on a URL change do so between
// validateUpdate and applyUpdate.
func applyUpdate(cfg *config.Config, plan updatePlan) (updateOutcome, error) {
	if len(plan.changedFields) > 0 {
		cfg.Servers[plan.name] = config.Server{URL: plan.canonicalURL, CA: plan.caSelector}
	}
	repaired := bootstrapPointer(cfg, plan.name)
	if len(plan.changedFields) > 0 || repaired {
		if err := cfg.Write(); err != nil {
			return updateOutcome{}, err
		}
	}
	return updateOutcome{
		name: plan.name, canonicalURL: plan.canonicalURL, caSelector: plan.caSelector,
		unchanged: len(plan.changedFields) == 0, changedFields: plan.changedFields,
		credentialsCleared: plan.credentialsCleared,
		currentServer:      cfg.CurrentServer, pointerRepaired: repaired,
	}, nil
}

// authSnapshot is what login captures immediately before starting the auth
// flow: the exact name, canonical URL, CA selector, and resolved CA bytes
// authentication runs under. It is compared against the CURRENT entry at
// two points — the credential-write commit seam, and the stale-hint recheck
// after a failed delegated login — both via snapshotMatchesCurrent, so the
// two can never diverge into separately-maintained comparisons.
type authSnapshot struct {
	name         string
	canonicalURL string
	caSelector   string
	caBytes      string
}

// snapshotMatchesCurrent reports whether name's CURRENT entry in cfg still
// matches snap exactly: canonical URL, CA selector, and (for a path-valued
// selector) the resolved bytes re-read right now — never the stored selector
// string alone, which cannot see a file that rotated in place.
func snapshotMatchesCurrent(cfg *config.Config, snap authSnapshot) bool {
	cur, ok := cfg.Servers[snap.name]
	if !ok || cur.URL != snap.canonicalURL || cur.CA != snap.caSelector {
		return false
	}
	matches, _ := recheckBytes(snap.canonicalURL, snap.caSelector, snap.caBytes)
	return matches
}

// commitUpdate composes validateUpdate and applyUpdate with no credential
// purge in between — the shape callers without a URL-change side effect (and
// tests) use directly. configUpdateServerCmd calls the two steps separately
// so it can purge credentials between them.
func commitUpdate(cfg *config.Config, name string, src updateSource, probe registrationProbe) (updateOutcome, error) {
	plan, err := validateUpdate(cfg, name, src, probe)
	if err != nil {
		return updateOutcome{}, err
	}
	return applyUpdate(cfg, plan)
}

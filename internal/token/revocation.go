package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// revocationBudget bounds every revoke of one command together; a var so
// tests shorten it.
var revocationBudget = 10 * time.Second

// revocationClock is the clock the revokes check the budget's deadline
// against; a var so a test can move it past the deadline between two
// requests.
var revocationClock = time.Now

// settleTimeout bounds the config-lock span that records what the revokes
// did. It runs detached from the command's cancellation, so a Ctrl-C during
// the revokes still settles the ones that were confirmed.
var settleTimeout = 15 * time.Second

// DeleteCommand names the command whose local delete is followed by the
// revokes; it decides which cache records are deleted, whether the login
// is read, and where a record of a server-wide call is written.
type DeleteCommand int

const (
	// DeleteForLogout is auth logout of named entries.
	DeleteForLogout DeleteCommand = iota
	// DeleteForLogoutAll is auth logout --all: every entry, the whole cache.
	DeleteForLogoutAll
	// DeleteForDeleteToken is auth delete-token: the records Select picks.
	DeleteForDeleteToken
	// DeleteForDeleteServer is config delete-server: as a logout of the
	// entry, which the command removes afterwards.
	DeleteForDeleteServer
	// DeleteForUpdateServer is config update-server's URL change: nothing
	// is read or revoked, the old server's tokens are recorded.
	DeleteForUpdateServer
)

// readsLogin reports whether the command logs its targets out: it reads
// their login tokens, clears their TokenRef and stamps LoggedOutAt.
func (c DeleteCommand) readsLogin() bool {
	return c == DeleteForLogout || c == DeleteForLogoutAll || c == DeleteForDeleteServer
}

func (c DeleteCommand) advancesBinding() bool { return c != DeleteForDeleteToken }

// LocalDelete is one command's delete of the tokens the CLI stored: whose,
// which cache records, and how a login token is deleted.
type LocalDelete struct {
	Dir     string
	Command DeleteCommand
	// Targets are the entries whose login token is deleted, in order; under
	// logout --all a name with no entry (an identity record left behind)
	// is deleted too.
	Targets []string
	// Cache is the store whose cache the two holds run on.
	Cache config.CredStore
	// Select picks auth delete-token's records by name; nil for --all
	// and for every other command.
	Select func(config.CachedRecord) bool
	// DeleteLogin deletes one target's login token from every backend.
	DeleteLogin func(ctx context.Context, ref config.ServerRef) (from, notices []string, err error)
	// Rerun is the command to run again when a local delete did not
	// complete, built on clierr.Rancher.
	Rerun string
}

// LoginDeleteError is a login-token delete that failed: the target and the
// cause. The deletes after it were not attempted.
type LoginDeleteError struct {
	Target string
	Err    error
}

func (e *LoginDeleteError) Error() string { return e.Err.Error() }
func (e *LoginDeleteError) Unwrap() error { return e.Err }

// Deletion is a local delete done and the revokes it plans: what was
// deleted, and every token that may still be live, each with how it is
// revoked or why it is not.
type Deletion struct {
	req LocalDelete
	now time.Time
	// cfg is the config as the first write wrote it, the restore included:
	// what the revokes are sent by.
	cfg *config.Config
	// view is the config the last span loaded - the first, the record
	// write after the revokes, or a span the command reports through
	// Refresh: what the report's entry and retry are judged by, so a login
	// or a URL change that landed meanwhile is seen.
	view  *config.Config
	items []*revocationItem
	// DeletedFrom lists, per target, the backends its login token was
	// deleted from.
	DeletedFrom map[string][]string
	// DeletedCache lists the cache keys the second hold deleted.
	DeletedCache []string
	// Kept lists the cache keys whose record was rewritten between the
	// holds, which the compare left in place.
	Kept []string
	// Notices are the delete's notices for the command's warnings.
	Notices []string
	// CacheUnreadable is set when the cache file could not be read: no
	// record was planned, and under the --all commands its tokens are
	// neither revoked nor recorded.
	CacheUnreadable bool
	// LoginErr is the login delete that failed, nil when none did; the
	// deletes after it were not attempted.
	LoginErr *LoginDeleteError
	// CacheErr is why the cache records could not be read or deleted, nil
	// when they were.
	CacheErr error

	loginRead map[string]string
	// loginStore is the store readLogins read the login tokens from.
	loginStore   config.CredStore
	loginDeleted map[string]bool
	cacheDeleted map[string]bool
	cacheFailed  bool
	// removed names the entries config delete-server removed, or left with
	// no login for its rerun to remove: no login of theirs retires their
	// records.
	removed map[string]bool
	clients map[string]revokeClient
}

type revokeClient struct {
	client *http.Client
	base   string
	err    error
	// hostPort is the trust entry's server as host[:port], the host a
	// certificate failure message names. fromVariable says RANCHER_CLI_CACERT
	// supplied the CA the client verifies against.
	hostPort     string
	fromVariable bool
}

// DeleteAndRecord is the local delete, run inside the caller's config-lock
// span over cfg, the config it loaded under that lock. It reads the
// targets' login tokens, then under one cache-lock hold reads the cache
// records it deletes and writes the config once: the binding advance and
// its pause, a superseded record for every token it may revoke, and for a
// logout the cleared TokenRef and the LoggedOutAt stamp. Then it deletes
// the login tokens, then, in a second hold, each planned record that still
// holds the token read. Every delete follows the write that records its
// token. A failure before that write deletes nothing and returns a nil
// Deletion; one after it returns the Deletion with the error, and the
// revokes still run from what was read.
func DeleteAndRecord(ctx context.Context, cfg *config.Config, req LocalDelete) (*Deletion, error) {
	d := &Deletion{req: req, now: time.Now().UTC(), cfg: cfg, view: cfg, DeletedFrom: map[string][]string{},
		loginRead: map[string]string{}, loginDeleted: map[string]bool{},
		cacheDeleted: map[string]bool{}, removed: map[string]bool{}, clients: map[string]revokeClient{}}
	before := map[string]config.Server{}
	for _, n := range req.Targets {
		if s, ok := cfg.Servers[n]; ok {
			before[n] = s.Clone()
		}
	}
	if req.Command.readsLogin() {
		d.readLogins(cfg, before)
	}

	var cacheErr error
	herr := req.Cache.HoldCachedCreds(func(recs []config.CachedRecord) error {
		return d.planAndWrite(cfg, before, recs)
	})
	_, unreadable := errors.AsType[*config.CacheReadError](herr)
	switch {
	case herr != nil && !unreadable:
		// The first write failed, or the hold did not run (the cache lock
		// not taken above all): nothing is deleted. Only a hold that could
		// not read the file returns a *CacheReadError.
		return nil, herr
	case herr != nil && req.Command == DeleteForDeleteToken && req.Select != nil:
		return nil, herr
	case herr != nil:
		// The cache file could not be read: nothing of it is planned, and
		// the login deletes still go ahead.
		d.CacheUnreadable = true
		cacheErr = herr
		if werr := d.planAndWrite(cfg, before, nil); werr != nil {
			return nil, werr
		}
	}

	loginErr := d.deleteLogins(ctx, cfg, before)
	if loginErr != nil {
		if rerr := d.restore(cfg, before); rerr != nil {
			loginErr = errors.Join(loginErr, rerr)
		}
	}
	if cerr := d.deleteCacheRecords(); cerr != nil {
		cacheErr = cerr
	}
	d.markNotDeleted()
	if d.CacheUnreadable && req.Command == DeleteForDeleteToken {
		// auth delete-token --all removes a file it cannot read and says
		// its tokens can be neither revoked nor recorded: the removal
		// answers the read error, and only its own failure is the
		// command's.
		cacheErr = req.Cache.DeleteAllCachedCreds()
	}
	d.CacheErr = cacheErr
	d.LoginErr, _ = errors.AsType[*LoginDeleteError](loginErr)
	switch {
	case loginErr != nil && cacheErr != nil:
		return d, errors.Join(loginErr, fmt.Errorf("exec-credential cache: %w", cacheErr))
	case loginErr != nil:
		return d, loginErr
	case cacheErr != nil:
		return d, fmt.Errorf("exec-credential cache: %w", cacheErr)
	}
	return d, nil
}

// readLogins reads, before any lock but the config lock, the login token
// of each target the CLI created and named while it holds a login, and of
// one in the state a failed restore leaves (CLI-created, no TokenRef,
// stamped). An entry the CLI did not create is never read: no revoke uses
// its bearer, and a credential helper may create a token on a read.
func (d *Deletion) readLogins(cfg *config.Config, before map[string]config.Server) {
	var store config.CredStore
	for _, n := range d.req.Targets {
		s, ok := before[n]
		if !ok || !loginReadable(s) {
			continue
		}
		if store == nil {
			st, err := config.OpenStoreFor(cfg, d.req.Dir)
			if err != nil {
				return
			}
			store = st
		}
		if tok, err := store.GetToken(config.ServerRef{Name: n, URL: s.URL}); err == nil && tok != "" {
			d.loginRead[n] = tok
		}
	}
	if ns, ok := store.(config.NoticeSource); ok {
		d.Notices = append(d.Notices, ns.DrainNotices()...)
	}
	d.loginStore = store
}

func loginReadable(s config.Server) bool {
	return (s.CLIOwned() && s.LoggedOutAt == "") || (s.CLICreated() && s.TokenRef == nil && s.LoggedOutAt != "")
}

// planAndWrite classifies every token and writes the config once; it runs
// inside the first cache-lock hold.
func (d *Deletion) planAndWrite(cfg *config.Config, before map[string]config.Server, recs []config.CachedRecord) error {
	d.plan(cfg, before, recs)
	changed, err := d.firstWrite(cfg)
	if err != nil || !changed {
		return err
	}
	return config.WriteStep("cachebinding", cfg)
}

// deleteLogins deletes each target's login token in order and stops at the
// first failure, so the targets after it keep their login. A failed delete
// after which the store no longer holds the token - a step after the
// keychain delete, such as removing the credential identity record,
// cancelled or timed out, or another backend's delete - still counts the
// login as deleted: nothing uses it, so it is revoked like any deleted
// login and its TokenRef is not put back. The failure stays the command's:
// another backend may still hold a copy.
func (d *Deletion) deleteLogins(ctx context.Context, cfg *config.Config, before map[string]config.Server) error {
	for _, n := range d.req.Targets {
		ref := config.ServerRef{Name: n, URL: before[n].URL}
		from, ns, err := d.req.DeleteLogin(ctx, ref)
		d.Notices = append(d.Notices, ns...)
		if err != nil {
			if d.loginGone(ref) {
				d.DeletedFrom[n] = from
				d.loginDeleted[n] = true
			}
			return &LoginDeleteError{Target: n, Err: err}
		}
		d.DeletedFrom[n] = from
		d.loginDeleted[n] = true
		if _, ok := before[n]; !ok && len(from) > 0 {
			d.items = append(d.items, &revocationItem{source: sourceLogin, reason: reasonUnproven, loginTarget: n})
		}
	}
	return nil
}

// loginGone reads, after a failed delete, the store readLogins read a
// target's login token from, and reports whether it no longer holds the
// token. A target whose token was not read, or a read that fails, counts
// as still holding it.
func (d *Deletion) loginGone(ref config.ServerRef) bool {
	if d.loginRead[ref.Name] == "" {
		return false
	}
	_, err := d.loginStore.GetToken(ref)
	if ns, ok := d.loginStore.(config.NoticeSource); ok {
		d.Notices = append(d.Notices, ns.DrainNotices()...)
	}
	return errors.Is(err, config.ErrNoToken)
}

// restore puts back, under the same lock, each target whose login delete
// did not complete: its TokenRef and LoggedOutAt as they were, and without
// the record the first write added for its login token.
func (d *Deletion) restore(cfg *config.Config, before map[string]config.Server) error {
	changed := false
	var restored []*revocationItem
	for _, n := range d.req.Targets {
		prior, ok := before[n]
		if d.loginDeleted[n] || !ok {
			continue
		}
		e, ok := cfg.Servers[n]
		if !ok {
			continue
		}
		if d.req.Command.readsLogin() && (e.LoggedOutAt != prior.LoggedOutAt || (e.TokenRef == nil) != (prior.TokenRef == nil)) {
			e.TokenRef, e.LoggedOutAt = prior.Clone().TokenRef, prior.LoggedOutAt
			changed = true
		}
		for _, it := range d.items {
			if it.source == sourceLogin && it.loginTarget == n && it.wrote {
				e.SupersededTokens = slices.DeleteFunc(e.SupersededTokens, func(r config.SupersededToken) bool { return r.Ref == *it.ref })
				restored = append(restored, it)
				changed = true
			}
		}
		if len(e.SupersededTokens) == 0 {
			e.SupersededTokens = nil
		}
		cfg.Servers[n] = e
	}
	if !changed {
		return nil
	}
	if err := config.WriteStep("restore", cfg); err != nil {
		// The records stay on disk, so the items keep their entry.
		return fmt.Errorf("restoring the server entries whose login token was not deleted: %w", err)
	}
	for _, it := range restored {
		it.wrote, it.entry = false, ""
	}
	return nil
}

// deleteCacheRecords is the second hold: each planned record is deleted
// only while it still holds the token the first hold read.
func (d *Deletion) deleteCacheRecords() error {
	want := map[string]string{}
	for _, it := range d.items {
		if it.source == sourceCache {
			want[it.cacheKey] = it.cacheToken
		}
	}
	if len(want) == 0 {
		d.DeletedCache = []string{}
		return nil
	}
	deleted, changed, err := d.req.Cache.DeleteCachedCredsIfUnchanged(want)
	if err != nil {
		d.cacheFailed = true
		return err
	}
	d.DeletedCache, d.Kept = append([]string{}, deleted...), changed
	for k := range want {
		d.cacheDeleted[k] = true
	}
	return nil
}

// markNotDeleted settles what the deletes decided: a login token the CLI
// created and named whose delete did not complete is still stored and in
// use, and so, under auth delete-token, is a cache token whose record the
// second hold could not delete; neither is revoked. Under the other
// commands such a record no longer serves anything - the binding advance
// made it a miss - so its token is revoked as planned.
func (d *Deletion) markNotDeleted() {
	for _, it := range d.items {
		switch {
		case it.hidden:
		case it.source == sourceLogin && it.holdsLogin && !d.loginDeleted[it.loginTarget]:
			it.reason, it.bearer = reasonNotDeleted, ""
		case it.source == sourceCache && it.reason == "" && !d.cacheDeleted[it.cacheKey] && d.req.Command == DeleteForDeleteToken:
			it.reason, it.bearer = reasonNotDeleted, ""
		}
	}
}

// RemoveEntry tells the deletion that the entry is gone, or holds no
// login and goes with the rerun the command names: the tokens recorded on
// it are then reported with no entry and a manual retry, since no login of
// that entry retires them.
func (d *Deletion) RemoveEntry(entry string) { d.removed[entry] = true }

// Refresh gives the deletion the config a later span of the command
// loaded, as it stands on disk, to judge the report by.
func (d *Deletion) Refresh(cfg *config.Config) { d.view = cfg }

// Cancelled reports whether a cancelled context left a token unattempted:
// a token whose reason is cancelled, which only a token ready to send can
// get. A signal that came after the last revoke left none.
func (d *Deletion) Cancelled() bool {
	return slices.ContainsFunc(d.items, func(it *revocationItem) bool { return it.reason == reasonCancelled })
}

// LoginDeleted reports whether the target's login token is gone from the
// store: its delete completed, or failed after the token went.
func (d *Deletion) LoginDeleted(target string) bool { return d.loginDeleted[target] }

// DeletedTargets lists, in order, the targets whose login delete
// completed: not one whose delete failed after its token was gone.
func (d *Deletion) DeletedTargets() []string {
	return slices.DeleteFunc(slices.Clone(d.req.Targets), func(n string) bool {
		return !d.loginDeleted[n] || d.LoginErr != nil && d.LoginErr.Target == n
	})
}

// AddUnproven lists a credential the command deleted with no entry to
// prove who created it - an orphaned helper credential - at server.
func (d *Deletion) AddUnproven(server string) {
	d.items = append(d.items, &revocationItem{source: sourceLogin, reason: reasonUnproven, server: server})
}

// RevokeTokens sends one DELETE per token it may revoke, sequentially and
// outside every lock, from the copies the local delete read, under one
// budget; a cancelled context stops before the next one. No update-server
// token is ever ready to send: each carries its reason from the plan.
func (d *Deletion) RevokeTokens(ctx context.Context) {
	budget, cancel := context.WithTimeout(context.WithoutCancel(ctx), revocationBudget)
	defer cancel()
	deadline, _ := budget.Deadline()
	for _, it := range d.ordered() {
		if !d.ready(it) {
			continue
		}
		switch {
		case ctx.Err() != nil:
			it.reason = reasonCancelled
			continue
		case !revocationClock().Before(deadline):
			// The clock decides, not the budget's Done channel, which only
			// fires once the deadline has passed: a passed deadline handed
			// to RevokeCreated would be ignored there and cost a full
			// cleanup bound.
			it.reason = reasonBudget
			continue
		}
		out, err := d.send(budget, it.trust, it.bearer, it.provenance, *it.ref)
		if err != nil {
			it.reason, it.errText = reasonFailed, err.Error()
			continue
		}
		it.attempted, it.outcome = true, out
		it.judge(out)
		if it.reason == reasonUnconfirmed && it.source == sourceCache {
			d.retryWithLogin(budget, deadline, it)
		}
	}
}

// ready reports whether it is sent now. A superseded record matching a
// target's stored login is revoked with that token as its own bearer only
// when this run deleted that login; otherwise it has no bearer.
func (d *Deletion) ready(it *revocationItem) bool {
	switch {
	case it.hidden || it.reason != "":
		return false
	case it.matchLogin != "":
		if !d.loginDeleted[it.matchLogin] {
			// A v3 record is the stored login's own token: the rerun that
			// deletes the login sends it.
			it.reason = reasonNoBearer
			if it.ref.Kind == TokenKindV3 {
				it.reason = reasonNotDeleted
			}
			return false
		}
		it.bearer, it.self, it.provenance, it.trust = d.loginRead[it.matchLogin], true, ProvenanceDerived, it.matchLogin
	}
	return true
}

// retryWithLogin retries a cache token whose own bearer could not confirm
// its delete with a stored login the local delete read: one of the same
// server whose entry records the token's owner. A 404 to that bearer
// confirms the token gone. A v3 token is never retried: with another
// token's bearer a standard user's delete always answers 403, so the
// request tells nothing.
func (d *Deletion) retryWithLogin(budget context.Context, deadline time.Time, it *revocationItem) {
	login, ok := d.retryLogin(it)
	if it.ref.Kind == TokenKindV3 || !ok || !revocationClock().Before(deadline) {
		return
	}
	s := d.cfg.Servers[login]
	out, err := d.send(budget, login, d.loginRead[login], s.Provenance, *it.ref)
	if err != nil {
		return
	}
	switch out.Status {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		it.reason, it.errText, it.revoked, it.outcome = "", "", true, out
	}
}

// send builds the credential for one DELETE and sends it to the server the
// trust entry names, with that entry's CA.
func (d *Deletion) send(budget context.Context, trust, bearer string, provenance Provenance, ref TokenRef) (RevokeOutcome, error) {
	c, ok := d.clients[trust]
	if !ok {
		c = d.newClient(trust)
		d.clients[trust] = c
	}
	if c.err != nil {
		return RevokeOutcome{}, c.err
	}
	// The credential is what the CLI stored: the bearer, the provenance
	// that proves the CLI created it, and the client and base URL of a
	// server a current entry names.
	cred := LoginCredential{Token: bearer, Provenance: provenance, client: c.client, base: c.base}
	return cred.RevokeCreated(budget, ref), nil
}

func (d *Deletion) newClient(trust string) revokeClient {
	s := d.cfg.Servers[trust]
	// An entry whose URL is unusable (hand-edited in config.yaml) is never
	// dialed and its stored token never sent.
	if _, err := credhelper.CheckServerURL(s.URL); err != nil {
		origin := config.EntryOrigin(trust)
		text := origin.Text(err)
		if d.req.Command == DeleteForDeleteServer && slices.Contains(d.req.Targets, trust) {
			// The command removes the entry: the repair is not the entry's URL.
			text = origin.RemovedText(err)
		}
		return revokeClient{err: errors.New(text + "; the token was not sent")}
	}
	pem, fromVariable, err := revokeCA(s)
	if err != nil {
		return revokeClient{err: err}
	}
	client, err := httpx.New(httpx.Options{
		CACert:        pem,
		Timeout:       httpx.DefaultTimeout,
		WrapTransport: tlstrust.WrapTransportFor(s.URL, tlstrust.Remedy{}),
	})
	if err != nil {
		return revokeClient{err: err}
	}
	rc := revokeClient{client: client, base: kube.BaseURL(s.URL), fromVariable: fromVariable}
	if u, perr := url.Parse(s.URL); perr == nil {
		if origin, oerr := httpx.OriginOf(u); oerr == nil {
			rc.hostPort = origin.HostPort()
		}
	}
	return rc
}

// revokeCA is the CA a revoke to an entry's server is verified against: the
// entry's saved CA. An entry that saves none because an auth login
// connected with RANCHER_CLI_CACERT (CAFromEnvironment) uses the variable
// where it applies to the entry's URL; a variable exported for another
// server does not reach it. Any other entry without a saved CA names a
// server the system roots verify, whatever the variable says. The second
// result says the variable supplied the CA.
func revokeCA(s config.Server) (string, bool, error) {
	if s.CA != "" {
		pem, err := config.ResolveCA(s.CA)
		return pem, false, err
	}
	if !s.CAFromEnvironment {
		return "", false, nil
	}
	pem, err := config.ResolveCA(config.EnvCACertFor(s.URL))
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", config.EnvCACert, err)
	}
	return pem, config.EnvCACertFor(s.URL) != "", nil
}

// Settle records what the revokes did, in one config-lock span after
// them, reloading first and writing only on a change: an attempted
// record is removed when its token is confirmed gone or its own bearer
// was refused (no later bearer of the same user does better), and stamped
// with the attempt otherwise. Under auth delete-token, a record the first
// write added for a token whose cache record could not be deleted is
// removed: the cache still serves that token. Nothing else is touched, so
// a login in between keeps its token and records. It runs detached from
// ctx's cancellation; a failure is a warning.
func (d *Deletion) Settle(ctx context.Context) []string {
	remove, stamp := map[string][]TokenRef{}, map[string]map[TokenRef]clierr.RevokeOutcome{}
	for _, it := range d.items {
		if it.entry == "" || it.ref == nil {
			continue
		}
		switch {
		case it.source == sourceCache && it.wrote && d.cacheFailed && d.req.Command == DeleteForDeleteToken:
			remove[it.entry] = append(remove[it.entry], *it.ref)
		case !it.attempted:
		case it.revoked || it.reason == reasonForbidden:
			remove[it.entry] = append(remove[it.entry], *it.ref)
		default:
			if stamp[it.entry] == nil {
				stamp[it.entry] = map[TokenRef]clierr.RevokeOutcome{}
			}
			stamp[it.entry][*it.ref] = clierr.RevokeOutcome{Result: it.outcome.Result, Error: it.outcome.Error}
		}
	}
	if len(remove) == 0 && len(stamp) == 0 {
		return nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	err := config.WithLock(sctx, d.req.Dir, func() error {
		cfg, err := config.Load(config.FilePath(d.req.Dir))
		if err != nil {
			return err
		}
		// The edits below touch only the records, which the report does
		// not read, so the view holds even when the write fails.
		d.view = cfg
		changed := false
		for name, s := range cfg.Servers {
			n := len(s.SupersededTokens)
			s.SupersededTokens = slices.DeleteFunc(s.SupersededTokens, func(r config.SupersededToken) bool {
				return slices.Contains(remove[name], r.Ref)
			})
			mod := len(s.SupersededTokens) != n
			for i := range s.SupersededTokens {
				if st, ok := stamp[name][s.SupersededTokens[i].Ref]; ok {
					s.SupersededTokens[i].LastAttempt = &st
					mod = true
				}
			}
			if !mod {
				continue
			}
			if len(s.SupersededTokens) == 0 {
				s.SupersededTokens = nil
			}
			cfg.Servers[name] = s
			changed = true
		}
		if !changed {
			return nil
		}
		return config.WriteStep("settle", cfg)
	})
	if err != nil {
		return []string{fmt.Sprintf("could not record the revocation results on the server entries (%v); the records stay, listed by: %s", err, config.StatusCommand)}
	}
	return nil
}

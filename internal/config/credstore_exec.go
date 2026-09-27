package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
)

// ShortLivedTokenStore is the optional interface a CredStore implements when it
// can report the in-memory expiry of the last short-lived token it served for
// an identity. get-token's legacy (pre-ext.cattle.io) fallback consults it so a
// helper-created short-lived token is never written to the on-disk cache past -
// or at all beyond - its real expiry.
type ShortLivedTokenStore interface {
	ShortLivedTokenExpiry(ref ServerRef) (time.Time, bool)
}

// ErrTokenStoreUnsupported is execStore.SetToken's typed result for a helper
// that answered {"unsupported":true}: the helper is read-only by design.
// Only `auth login` treats it specially (warn and proceed); every other
// caller handles it as a normal error.
var ErrTokenStoreUnsupported = errors.New("credential helper does not support storing tokens")

// execHelperTimeout bounds one helper invocation. Package var so tests can
// shrink it via SetExecHelperTimeoutForTest.
var execHelperTimeout = 30 * time.Second

// SetExecHelperTimeoutForTest shrinks the helper timeout and returns a
// restore func. In non-test code so other packages' tests can reach it.
func SetExecHelperTimeoutForTest(d time.Duration) (restore func()) {
	old := execHelperTimeout
	execHelperTimeout = d
	return func() { execHelperTimeout = old }
}

// expirySkew is the clock-skew margin for expiresAt: a token expiring within
// the margin counts as expired, and the in-process cache refreshes that early.
const expirySkew = 30 * time.Second

type cachedShortLived struct {
	token   string
	expires time.Time
}

// execStore delegates login-token storage to rancher-credential-<helper> on
// PATH, speaking credhelper protocol v1; the exec-credential cache stays on
// the file store. Tokens with expiresAt are cached in memory only.
type execStore struct {
	helper     string
	dir        string
	cache      CredStore
	now        func() time.Time
	mu         sync.Mutex
	shortLived map[string]cachedShortLived // key: canonicalURL+"\n"+name
}

func newExecStore(helper, dir string) *execStore {
	return &execStore{
		helper:     helper,
		dir:        dir,
		cache:      NewFileStore(dir),
		now:        time.Now,
		shortLived: map[string]cachedShortLived{},
	}
}

func (s *execStore) binary() string { return "rancher-credential-" + s.helper }

// invoke runs one helper verb with req on stdin and decodes the response.
// Decoding is verb-aware: get MUST return versioned JSON (not-found is a
// JSON response without a token), while store/erase may exit 0 with EMPTY
// stdout - a protocol-compliant success. Version is validated only when a
// response body is present. Helper stderr is surfaced in errors.
func (s *execStore) invoke(verb string, req credhelper.Request) (*credhelper.Response, error) {
	req.ProtocolVersion = credhelper.ProtocolVersion
	path, err := exec.LookPath(s.binary())
	if err != nil {
		return nil, fmt.Errorf(
			"credential helper %q not found in PATH; install it or change credentialStore (see CREDENTIAL_HELPERS.md)",
			s.binary())
	}
	in, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), execHelperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, verb)
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("credential helper %s %s timed out after %s", s.binary(), verb, execHelperTimeout)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return nil, fmt.Errorf("credential helper %s %s: %s", s.binary(), verb, msg)
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		if verb == credhelper.VerbGet {
			return nil, fmt.Errorf("credential helper %s get: empty response (a get must return JSON; not-found is a response without a token)", s.binary())
		}
		return &credhelper.Response{ProtocolVersion: credhelper.ProtocolVersion}, nil
	}
	var resp credhelper.Response
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("credential helper %s %s: malformed response: %v", s.binary(), verb, err)
	}
	if resp.ProtocolVersion != credhelper.ProtocolVersion {
		return nil, fmt.Errorf("credential helper %s %s: unsupported protocol version %d (CLI speaks %d)",
			s.binary(), verb, resp.ProtocolVersion, credhelper.ProtocolVersion)
	}
	return &resp, nil
}

// wireRequest canonicalizes ref into the protocol request. The canonical URL
// is the wire contract: every verb sees byte-identical identity fields.
func wireRequest(ref ServerRef) (credhelper.Request, error) {
	cu, err := credhelper.CanonicalServerURL(ref.URL)
	if err != nil {
		return credhelper.Request{}, fmt.Errorf("credential helper request: %w", err)
	}
	return credhelper.Request{ServerURL: cu, Name: ref.Name}, nil
}

func (s *execStore) GetToken(ref ServerRef) (string, error) {
	req, err := wireRequest(ref)
	if err != nil {
		return "", err
	}
	key := req.ServerURL + "\n" + req.Name

	s.mu.Lock()
	if c, ok := s.shortLived[key]; ok && s.now().Add(expirySkew).Before(c.expires) {
		s.mu.Unlock()
		return c.token, nil
	}
	s.mu.Unlock()

	resp, err := s.invoke(credhelper.VerbGet, req)
	if err != nil {
		return "", err
	}
	// Validate expiresAt FIRST, before any token-presence branch: a malformed
	// timestamp is a hard malformed-response error even on a token-less
	// response - never silently treated as not-found or non-expiring.
	exp, err := s.parseExpiry(resp)
	if err != nil {
		return "", err
	}
	if resp.Token != "" && !exp.IsZero() && s.expired(exp) {
		// Already expired at receipt: one immediate retry, then hard error.
		first := resp.ExpiresAt
		if resp, err = s.invoke(credhelper.VerbGet, req); err != nil {
			return "", err
		}
		if exp, err = s.parseExpiry(resp); err != nil {
			return "", err
		}
		if resp.Token != "" && !exp.IsZero() && s.expired(exp) {
			return "", fmt.Errorf("credential helper %s returned expired tokens twice (expiresAt %s, then %s)",
				s.binary(), first, resp.ExpiresAt)
		}
	}
	if resp.Token == "" {
		return "", ErrNoToken
	}
	s.mu.Lock()
	if !exp.IsZero() {
		s.shortLived[key] = cachedShortLived{token: resp.Token, expires: exp}
	} else {
		// Long-lived token (no expiresAt): clear any stale short-lived entry
		// for this identity so ShortLivedTokenExpiry no longer reports the
		// previous expiry for the new token.
		delete(s.shortLived, key)
	}
	s.mu.Unlock()
	return resp.Token, nil
}

// ShortLivedTokenExpiry reports the in-memory cached expiry for ref's identity
// (canonical URL + name), and true, when the last token this store served for
// it carried an expiresAt (a short-lived, memory-only credential). It returns
// false when no such token was cached - a long-lived token, or none served yet
// this process - or when ref cannot be canonicalized.
func (s *execStore) ShortLivedTokenExpiry(ref ServerRef) (time.Time, bool) {
	req, err := wireRequest(ref)
	if err != nil {
		return time.Time{}, false
	}
	key := req.ServerURL + "\n" + req.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.shortLived[key]; ok {
		return c.expires, true
	}
	return time.Time{}, false
}

// parseExpiry validates a present expiresAt; absent → zero time, nil error.
func (s *execStore) parseExpiry(resp *credhelper.Response) (time.Time, error) {
	if resp.ExpiresAt == "" {
		return time.Time{}, nil
	}
	exp, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("credential helper %s: malformed expiresAt %q: %v", s.binary(), resp.ExpiresAt, err)
	}
	return exp, nil
}

// expired reports whether exp is at/before now+skew.
func (s *execStore) expired(exp time.Time) bool {
	return !exp.After(s.now().Add(expirySkew))
}

func (s *execStore) SetToken(ref ServerRef, token string) error {
	_, err := s.SetTokenWithOutcome(ref, token, PriorCredential{})
	return err
}

// SetTokenWithOutcome is SetToken reporting what the write did. The
// protocol has no read or snapshot operation - a helper's get may create a
// fresh credential rather than reveal persisted state - so the prior token
// is always unknown and no compensation is ever offered. A helper that
// failed to answer may still have stored the credential, which is why an
// invocation error reports possiblyCommitted rather than a clean refusal;
// a helper that answered cleanly is still not atomic with the config write
// that follows it. The caller's prior classification is ignored for the
// same reason: a helper's namespace is the helper's own contract.
func (s *execStore) SetTokenWithOutcome(ref ServerRef, token string, _ PriorCredential) (CommitOutcome, error) {
	out := CommitOutcome{Backend: clierr.BackendExec, State: CommitNotWritten, PriorSource: clierr.PriorUnknown}
	req, err := wireRequest(ref)
	if err != nil {
		return out, err
	}
	entry := HelperRecord{Helper: s.helper, Name: ref.Name, CanonicalURL: req.ServerURL}
	lerr := withHelperRecordsLock(context.Background(), s.dir, func(st *helperRecords) error {
		// Write-ahead: persist the intent before the helper can create a
		// remote credential. persist-now, then invoke.
		st.add(entry)
		if perr := persistHelperRecords(s.dir, st); perr != nil {
			return fmt.Errorf("recording the credential-helper record (token NOT stored): %w", perr)
		}
		if execStoreLockedHook != nil {
			execStoreLockedHook("store")
		}
		req.Token = token
		resp, ierr := s.invoke(credhelper.VerbStore, req)
		if ierr != nil {
			out.State = CommitPossiblyCommitted
			out.HelperRecordRetained = true
			return ierr // intent stays: idempotent erase cleans up later
		}
		if resp.Unsupported {
			// Nothing stored - drop the intent, and persist that removal
			// BEFORE returning the sentinel: were the removal to fail, the
			// joined result must read as a hard error, never as clean
			// read-only success (login would warn-and-succeed while a stale
			// intent re-runs this helper on every future delete).
			st.remove(entry)
			if perr := persistHelperRecords(s.dir, st); perr != nil {
				out.HelperRecordRetained = true
				return fmt.Errorf("credential helper %s reported unsupported store, but recording that failed: %v", s.binary(), perr)
			}
			return fmt.Errorf("credential helper %s: %w", s.binary(), ErrTokenStoreUnsupported)
		}
		out.State = CommitHelperNonAtomic
		out.HelperRecordRetained = true
		afterWrite("helper store")
		return nil
	})
	return out, lerr
}

// eraseCredential runs the erase verb and interprets the response's erased
// field: deleted unless the helper affirmatively said nothing was stored
// (an absent field reads as deleted - the safe, over-reporting direction
// for logout's deletedFrom). The single interpretation every erase path
// shares.
func (s *execStore) eraseCredential(req credhelper.Request) (deleted bool, err error) {
	resp, err := s.invoke(credhelper.VerbErase, req)
	if err != nil {
		return false, err
	}
	afterWrite("helper erase")
	return resp.Erased == nil || *resp.Erased, nil
}

// DeleteToken erases through the helper and clears the matching helper record
// entry under the same operation-level lock; a helper or persistence failure
// retains the entry (retry keeps its target). Direct DeleteToken callers get
// the same guarantee the delete path has.
func (s *execStore) DeleteToken(ref ServerRef) error {
	req, err := wireRequest(ref)
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.shortLived, req.ServerURL+"\n"+req.Name)
	s.mu.Unlock()
	rec := HelperRecord{Helper: s.helper, Name: ref.Name, CanonicalURL: req.ServerURL}
	return withHelperRecordsLock(context.Background(), s.dir, func(st *helperRecords) error {
		if execStoreLockedHook != nil {
			execStoreLockedHook("erase")
		}
		// One erase implementation for every path, on this store's own
		// invoker. The deletion report is deliberately dropped:
		// DeleteToken's contract carries no such signal - wire it through
		// when a caller needs one.
		if _, ierr := s.eraseCredential(req); ierr != nil {
			return ierr // entry (if any) stays for retry
		}
		st.remove(rec)
		return nil
	})
}

// execStoreLockedHook is a test-only synchronization point, invoked (when
// non-nil) while the helper-records lock is HELD: "store" fires after the
// write-ahead intent is persisted and before the helper runs; "erase" fires
// before the helper erase. It lets tests force an exact interleaving of
// concurrent operations. Nil in production.
var execStoreLockedHook func(op string)

// DrainNotices implements runtime.NoticeSource by draining the wrapped
// exec-credential cache's buffer: the helper owns login tokens, but the
// cache is a file store with notices of its own (a loose-permission finding
// on token-cache.yaml), and those must reach the invocation's drain wherever
// this store is wired onto IO.Notices. Returns nil when the cache is not a
// NoticeSource.
func (s *execStore) DrainNotices() []string {
	if ns, ok := s.cache.(NoticeSource); ok {
		return ns.DrainNotices()
	}
	return nil
}

func (s *execStore) GetCachedCred(key string, anchor *CacheAnchor) (string, time.Time, error) {
	return s.cache.GetCachedCred(key, anchor)
}
func (s *execStore) GetCachedCredDetail(key string, anchor *CacheAnchor) (CachedCredDetail, error) {
	return s.cache.GetCachedCredDetail(key, anchor)
}
func (s *execStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string, binding CacheBinding) error {
	return s.cache.SetCachedCred(key, token, expiry, name, serverHost, cluster, binding)
}
func (s *execStore) MarkCachedCredReady(key, token, fingerprint string) (bool, error) {
	return s.cache.MarkCachedCredReady(key, token, fingerprint)
}
func (s *execStore) DeleteCachedCred(key string) error          { return s.cache.DeleteCachedCred(key) }
func (s *execStore) DeleteAllCachedCreds() error                { return s.cache.DeleteAllCachedCreds() }
func (s *execStore) ListCachedCreds() ([]CachedCredInfo, error) { return s.cache.ListCachedCreds() }

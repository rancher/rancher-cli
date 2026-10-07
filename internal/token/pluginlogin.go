package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// pluginLoginInput is what the plugin-created login needs from the
// invocation that missed its cache.
type pluginLoginInput struct {
	server, serverID, caPEM, clusterID string
	client                             *http.Client
	authProvider, authFlow             string
	newIdPClient                       func() *http.Client
	wantName                           string
	ignoreCurrentContext               bool
	// refreshEntry is the plugin entry a 401 named; "" on first use. The
	// re-login replaces that entry's credential as an update.
	refreshEntry string
}

func (t tokenCmd) pluginInput(server, serverID, caPEM, clusterID string, client *http.Client, params runParams) pluginLoginInput {
	return pluginLoginInput{server: server, serverID: serverID, caPEM: caPEM, clusterID: clusterID, client: client,
		authProvider: params.AuthProvider, authFlow: params.AuthFlow,
		newIdPClient: func() *http.Client { return httpx.NewExternal(httpx.DefaultTimeout) },
		wantName:     params.Name, ignoreCurrentContext: params.legacyShim}
}

// storeOpener opens the store the way a reader of the directory does,
// from the config the caller loaded: it probes nothing and writes no pin.
func (t tokenCmd) storeOpener() func(cfg *config.Config, dir string) (config.CredStore, error) {
	if t.openStore != nil {
		return func(cfg *config.Config, dir string) (config.CredStore, error) {
			store, _, err := t.openStore(cfg.CredentialStore, dir)
			return store, err
		}
	}
	return config.OpenStoreFor
}

// loginStoreOpener is the opener the login primitive gets. It differs
// from the read path's in one way that matters: it is called with a
// backend already resolved under the lock, so it never re-applies the
// variable and never writes a pin. The pin a login ends with is written
// by the change's own last step, after the credential commits.
func (t tokenCmd) loginStoreOpener() func(backend, dir string) (config.CredStore, string, error) {
	if t.openStore != nil {
		return t.openStore
	}
	return config.OpenResolvedStore
}

// pluginLogin runs the plugin-created login under the per-server flock:
// re-probe (another process may have finished), the store decision and
// its disclosure, acquisition, the cluster token that proves the new
// credential can create, then persistence through the shared primitive.
// Nothing durable exists until the credential commit succeeded; every
// failure after acquisition revokes the cluster token with the credential
// as bearer, then the credential itself. The flock is released before the
// retirement pass, which runs outside both locks.
func (t tokenCmd) pluginLogin(ctx context.Context, in pluginLoginInput, p runtime.Prompter) (Token, []string, error) {
	var (
		result   Token
		warnings []string
		entry    string
		cred     LoginCredential
		records  []config.SupersededToken
		acquired bool
	)
	err := config.WithPluginLoginLock(ctx, t.dir, in.serverID, func() error {
		cfg, err := config.Load(config.FilePath(t.dir))
		if err != nil {
			return err
		}
		if !cfg.TokenReuse {
			// Turned off while this invocation waited: the caller's
			// scoped interactive login applies.
			return errNoStoredCredential
		}
		// A waiter re-reads through a freshly opened store: the exec
		// store's in-process cache would still hold a rejected value. On a
		// bare install it finds nothing, without a probe or a pin: where
		// the credential goes is this login's own decision below.
		fresh, err := t.storeOpener()(cfg, t.dir)
		if err != nil {
			return err
		}
		// This load's binding is the one the login's record takes: it was
		// made before the reads below found no credential, so an entry that
		// gets a login after it has another id than the record carries.
		tok, _, lookup, perr := tokenCmd{dir: t.dir, store: fresh, anchor: t.anchor, user: t.user}.storedCredToken(ctx, in.client, in.server, in.clusterID, in.wantName, in.ignoreCurrentContext, p)
		switch {
		case perr == nil:
			result = tok
			return nil
		case errors.Is(perr, errNoStoredCredential):
		default:
			if _, rejected := errors.AsType[*pluginCredentialRejected](perr); !rejected {
				return perr
			}
		}

		entry = in.refreshEntry
		if entry == "" {
			entry = config.ExecLoginEntryName(in.serverID)
			// A call whose --name identifies a plugin login entry logs that
			// entry in, so the entry it reads next time holds the login;
			// the lookup's binding names the entry the --name identified.
			if identified := lookup.Entry(); cfg.Servers[identified].Provenance == clierr.ProvenanceExecLogin {
				entry = identified
			}
		}
		target := config.CollisionFreeName(cfg, entry, in.serverID)
		// Checked on this load, before anything is disclosed or signed in:
		// an entry another call logged in for another user while this one
		// waited is never written over. The caller signs in instead.
		if s, ok := cfg.Servers[target]; ok && t.user.otherOwner(s.TokenUserID) {
			return errNoStoredCredential
		}
		// Decided for the entry the login will write, suffix included.
		decision, disclosure, derr := storeDecision(t.dir, cfg, target)
		if derr != nil {
			return derr
		}
		if disclosure != "" {
			p.Notice(disclosure)
		}
		cred, err = AcquireLoginCredential(ctx, in.client, LoginRequest{Server: in.server, AuthProvider: in.authProvider,
			AuthFlow: in.authFlow, IdPClient: in.newIdPClient(), ResponseType: "json"}, 0, p)
		if err != nil {
			if ae, ok := errors.AsType[*AcquireError](err); ok {
				return FailAcquire(ae, entry)
			}
			return err
		}
		acquired = true
		base := kube.BaseURL(in.server)
		st := LoginStates{Config: clierr.ConfigNotWritten}
		if t.user.otherOwner(cred.UserID) {
			return FailLogin(t.user.pluginSignInRefused(cred.UserID), clierr.StageAuthentication, st, cred, entry)
		}
		resolved, warning, err := resolveClusterIDForCreate(ctx, in.client, base, cred.Token, in.clusterID)
		if err != nil {
			return FailLogin(freshCredentialError(err), clierr.StageClusterToken, st, cred, entry)
		}
		if warning != "" {
			p.Notice(warning)
		}
		ct, err := createClusterToken(ctx, in.client, base, cred.Token, resolved, TokenDescription(hostname(), cred.Marker))
		if err != nil {
			st.ClusterToken = clusterTokenFate(ctx, ct, cred)
			return FailLogin(freshCredentialError(err), clierr.StageClusterToken, st, cred, entry)
		}
		owner := cred.UserID
		if t.user.userID != "" {
			// Both creates must name the call's user where they name an
			// owner, and one of them must: nothing is stored otherwise.
			switch {
			case t.user.otherOwner(ct.UserID):
				st.ClusterToken = clusterTokenFate(ctx, ct, cred)
				return FailLogin(t.user.pluginSignInRefused(ct.UserID), clierr.StageClusterToken, st, cred, entry)
			case ct.UserID == "" && cred.UserID == "":
				st.ClusterToken = clusterTokenFate(ctx, ct, cred)
				return FailLogin(t.user.pluginSignInRefused(""), clierr.StageClusterToken, st, cred, entry)
			}
			owner = t.user.userID
		}
		out, perr := config.PersistLoginCredential(ctx, t.dir, config.PersistLoginInput{
			Name: entry, CollisionSuffix: true, CanonicalURL: in.serverID, CA: in.caPEM,
			Provenance: clierr.ProvenanceExecLogin, TokenRef: cred.Ref, TokenUserID: owner,
			Bearer: cred.Token, ExpiresAt: cred.ExpiresAt,
			OwnsEntry: true, Decision: &decision, ReadOnlyStoreIsFailure: true,
			Policy: decision.Policy, ObservedDeclaration: cfg.CredentialStore,
			Declaration: config.DeclarationFor(cfg.CredentialStore, "", decision.Policy),
			OpenStore:   t.loginStoreOpener(),
		})
		st.Warnings = append(st.Warnings, out.Warnings...)
		st.Config, st.Commit, st.ConfigRecovery, st.CompensationError = out.ConfigState, out.Commit, out.ConfigRecovery, out.CompensationError
		if perr != nil {
			primary := perr
			switch {
			case out.Aborted != "":
				primary = clierr.New(clierr.CodeConflict, out.Aborted).WithCause(perr)
			case errors.Is(perr, config.ErrTokenStoreUnsupported):
				primary = clierr.New(clierr.CodeGeneric,
					fmt.Sprintf("the credential store (policy %s) cannot store a login credential, so one cannot be reused across clusters; fix the store, or turn the setting off with: "+config.TokenReuseOffCommand+" (%v)", decision.Policy, perr)).
					WithCause(perr)
			}
			if cerr := ctx.Err(); cerr != nil {
				primary = cerr
			}
			st.ClusterToken = clusterTokenFate(ctx, ct, cred)
			name := out.Name
			if name == "" {
				name = entry
			}
			return FailLogin(primary, clierr.StageCommit, st, cred, name)
		}
		entry, records = out.Name, out.SupersededTokens
		// The record this invocation goes on to write is bound to what the
		// lookup above read, with the entry this login just wrote as its
		// last write wrote it - from the write itself, never a re-read.
		result = Token{Token: ct.Bearer, ExpiresAt: ct.ExpiresAt, fresh: newFreshToken(ct, in.client, base, cred.Token),
			binding: out.CacheBindingOver(t.cacheDir(), lookup)}
		warnings = append(warnings, st.Warnings...)
		return nil
	})
	if err != nil {
		return Token{}, nil, err
	}
	if acquired && len(records) > 0 {
		rep := RetireSuperseded(ctx, t.dir, entry, in.serverID, cred, records)
		warnings = append(warnings, rep.Warnings...)
	}
	return result, warnings, nil
}

// freshCredentialError names a rejection of the credential this login just
// created: a 401 here is not a stale credential to refresh but a hard
// failure, and a 403 is the permission error it always is.
func freshCredentialError(err error) error {
	switch {
	case errors.Is(err, errCredentialUnauthorized):
		return clierr.New(clierr.CodeAuth, "the server rejected the login credential it just issued: "+err.Error()).WithCause(err)
	case errors.Is(err, errCredentialForbidden):
		return clierr.New(clierr.CodeRBAC, "the login credential cannot create tokens: "+err.Error()).WithCause(err)
	}
	return err
}

// storeDecision records the backend the credential will reach, decided
// before the interactive login, and the disclosure to give when that
// backend is the plaintext file. It is the login's own answer - the same
// check `auth login` runs before its sign-in, with the probe the login
// pays anyway - so a login that will commit to the file store says so
// before anyone signs in, and one that would be refused is refused here:
// this login has a terminal only sometimes, and a refusal after the
// sign-in would have cost a server-issued token for a login that was
// never going to be accepted. The commit revalidates it.
func storeDecision(dir string, cfg *config.Config, entry string) (config.StoreDecision, string, error) {
	policy, err := config.EffectivePolicy(cfg.CredentialStore)
	if err != nil {
		return config.StoreDecision{}, "", err
	}
	if err := config.CheckOrphanCredentials(dir, cfg, entry); err != nil {
		return config.StoreDecision{}, "", err
	}
	backend, err := config.LoginStoreChange(dir, cfg, entry, policy, config.DeclarationFor(cfg.CredentialStore, "", policy))
	if err != nil {
		return config.StoreDecision{}, "", err
	}
	d := config.StoreDecision{RequireSetting: true, Policy: policy}
	if backend != "file" {
		return d, "", nil
	}
	path := config.CredentialsFilePath(dir)
	d.DisclosedPlaintextPath = path
	return d, fmt.Sprintf("token reuse is on and the credential store is the plaintext file %s: the login credential this sign-in creates (unscoped, reusable across clusters) will be written there", path), nil
}

// clusterTokenFate projects the cluster token's create onto its fate and
// revokes it when it is named, with the login credential as bearer: the
// unscoped credential reaches the management plane, which the cluster
// token's own scope would not.
func clusterTokenFate(ctx context.Context, ct CreateResult, cred LoginCredential) *clierr.TokenFate {
	if ct.Outcome == "" || ct.Outcome == CreateNotAttempted {
		return nil
	}
	f := &clierr.TokenFate{Outcome: ct.Outcome, Kind: ct.Kind, Ref: ct.Ref, Marked: ct.Marked, CreatedAt: ct.CreatedAt}
	if ct.Outcome == CreateKnown && ct.Ref != nil {
		out := cred.RevokeCreated(ctx, *ct.Ref)
		f.Cleanup = &out
	} else {
		f.Cleanup = &clierr.RevokeOutcome{Result: ResultNotAttempted}
	}
	return f
}

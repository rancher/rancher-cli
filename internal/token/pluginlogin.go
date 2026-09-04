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

func (t tokenCmd) storeOpener() func(policy, dir string) (config.CredStore, string, error) {
	if t.openStore != nil {
		return t.openStore
	}
	return config.OpenStoreWithNotice
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
		// store's in-process cache would still hold a rejected value.
		// On a clean first use nothing resolved a backend before this
		// point, so this open is the one that probes and pins under an
		// auto policy: its notice is the store-selection disclosure and
		// belongs in the warnings of every path that succeeds from here.
		fresh, notice, err := t.storeOpener()(cfg.CredentialStore, t.dir)
		if err != nil {
			return err
		}
		if notice != "" {
			warnings = append(warnings, notice)
		}
		tok, _, perr := tokenCmd{dir: t.dir, store: fresh}.storedCredToken(ctx, in.client, in.server, in.clusterID, in.wantName, in.ignoreCurrentContext, p)
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

		decision, disclosure := storeDecision(t.dir, cfg)
		if disclosure != "" {
			p.Notice(disclosure)
		}
		entry = in.refreshEntry
		if entry == "" {
			entry = config.ExecLoginEntryName(in.serverID)
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
		resolved, err := resolveClusterIDForCreate(ctx, in.client, base, cred.Token, in.clusterID)
		if err != nil {
			return FailLogin(freshCredentialError(err), clierr.StageClusterToken, st, cred, entry)
		}
		ct, err := createClusterToken(ctx, in.client, base, cred.Token, resolved, TokenDescription(hostname(), cred.Marker))
		if err != nil {
			st.ClusterToken = clusterTokenFate(ctx, ct, cred)
			return FailLogin(freshCredentialError(err), clierr.StageClusterToken, st, cred, entry)
		}
		out, perr := config.PersistLoginCredential(ctx, t.dir, config.PersistLoginInput{
			Name: entry, CollisionSuffix: true, CanonicalURL: in.serverID, CA: in.caPEM,
			Provenance: clierr.ProvenanceExecLogin, TokenRef: cred.Ref, TokenUserID: cred.UserID,
			Bearer: cred.Token, ExpiresAt: cred.ExpiresAt,
			OwnsEntry: true, Decision: &decision, ReadOnlyStoreIsFailure: true,
			OpenStore: t.storeOpener(),
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
					fmt.Sprintf("the credential store (policy %s) cannot store a login credential, so one cannot be reused across clusters; fix the store, or turn the setting off with: rancher config set-token-reuse off (%v)", decision.Policy, perr)).
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
		result = Token{Token: ct.Bearer, ExpiresAt: ct.ExpiresAt}
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
// backend is the plaintext file. The commit revalidates it.
func storeDecision(dir string, cfg *config.Config) (config.StoreDecision, string) {
	policy, pin, backend := config.ResolvedBackend(dir, cfg.CredentialStore)
	d := config.StoreDecision{RequireSetting: true, Policy: policy, Pin: pin}
	if backend != "file" {
		return d, ""
	}
	path := config.CredentialsFilePath(dir)
	d.DisclosedPlaintextPath = path
	return d, fmt.Sprintf("token reuse is on and the credential store is the plaintext file %s: the login credential this sign-in creates (unscoped, reusable across clusters) will be written there", path)
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

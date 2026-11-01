package configcmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

type configDeleteServerCmd struct {
	dir string
	// delete is the login-token delete, injectable for tests (the
	// authcheck preflight-seam pattern, authcheck.go); defaults to
	// config.DeleteLoginToken, which deletes it from every backend.
	delete func(ctx context.Context, dir string, ref config.ServerRef, policy string) (from, notices []string, err error)
}

// NewConfigDeleteServer builds `config delete-server <name>` (local): removes
// a configured server (clearing the current-server pointer when it names it)
// after deleting its stored credentials - the orphaned-creds cleanup nothing
// else performs - and revoking on the server the tokens among them the CLI
// created and can prove it created.
func NewConfigDeleteServer(dir string) runtime.Command {
	return &configDeleteServerCmd{dir: dir, delete: config.DeleteLoginToken}
}

// deleteServerCommand is the command that removes the server entry name.
func deleteServerCommand(name string) string {
	return clierr.Rancher + " config delete-server" + clierr.ShellQuoteArgs("", name)
}

func (configDeleteServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "config delete-server",
		Usage: "Delete a configured server and its stored credentials",
		Desc: "Deletes the entry's stored login token in every backend and the exec-credential cache records made from " +
			"its login, deletes on the server the tokens among them the CLI created and recorded, as auth logout does, " +
			"then removes the entry. data.revoked and data.notRevoked report each token; a token left on the removed " +
			"entry is manual, with recovery holding the commands that revoke it. A token the command could " +
			"not revoke because the server's certificate failed verification is listed with code TLS_TRUST and the " +
			"certificate's details. Refuses with CONFLICT, keeping the " +
			"entry, when a login or a URL change touched the entry while the command ran. Exits 0 when every local " +
			"delete succeeds, whatever the server answered. A Ctrl-C or termination signal that stops the local " +
			"delete, or leaves a token unreached, ends it with INTERRUPTED (exit 130), with both lists in the " +
			"error's details and the entry kept for a rerun to remove; a signal after the local delete that leaves " +
			"no token unsent does not stop it.",
		CommandType:  runtime.Local,
		ServerAccess: runtime.ServerAccessWrite,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:     []string{"rancher config delete-server prod"},
		Shape:        runtime.ShapeText,
	}
}

// Execute's ordering is safety-critical. The directory's unfinished store
// change goes first, after the not-found check and before any write, so a
// change record that cannot be read refuses with the config untouched and
// nothing deleted. The local delete (token.DeleteAndRecord) writes first -
// the binding advance, the pause, the stamp and a record of every token it
// may revoke - and deletes after; a delete failure aborts with the entry in
// place, its login and stamp restored unless the login token was already
// gone from the store. The revokes run outside the lock.
// Only then does the last span remove the entry - and only if it still
// carries the stamp, which every login and URL change clears: a login in
// between makes it refuse rather than delete a credential it never saw. A
// delete failure must never leave a live orphaned token behind a
// disappeared server entry - the exact bug this command exists to kill;
// the benign failure direction is creds gone + server still configured,
// never the reverse.
func (d configDeleteServerCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]

	var (
		srv      config.Server
		del      *token.Deletion
		notices  []string
		localErr error
	)
	if err := config.WithLock(cmdCtx, d.dir, func() error {
		cfg, err := config.Load(config.FilePath(d.dir))
		if err != nil {
			return err
		}
		var ok bool
		srv, ok = cfg.Servers[name]
		if !ok {
			return register.ServerNotFound(cfg, name)
		}
		policy, perr := config.EffectivePolicy(cfg.CredentialStore)
		if perr != nil {
			return perr
		}
		prelude, perr := config.PrepareCredentialDelete(cmdCtx, d.dir, cfg)
		if perr != nil {
			return perr
		}
		cache := config.NewFileStore(d.dir)
		del, localErr = token.DeleteAndRecord(cmdCtx, cfg, token.LocalDelete{
			Dir: d.dir, Command: token.DeleteForDeleteServer, Targets: []string{name}, Cache: cache,
			Rerun: deleteServerCommand(name),
			DeleteLogin: func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
				return d.delete(ctx, d.dir, ref, policy)
			},
		})
		notices = slices.Clone(prelude)
		if ns, ok := cache.(config.NoticeSource); ok {
			notices = append(notices, ns.DrainNotices()...)
		}
		if del == nil {
			return config.WithNotices(localErr, notices)
		}
		notices = append(notices, del.Notices...)
		return nil
	}); err != nil {
		return runtime.Result{}, err
	}

	del.RevokeTokens(cmdCtx)
	notices = append(notices, del.Settle(cmdCtx)...)
	if localErr != nil {
		cancelled := errors.Is(localErr, context.Canceled)
		if cancelled && del.LoginDeleted(name) {
			// The entry holds no login, and the rerun the message below
			// names removes it: its records are reported as that rerun
			// will report them.
			del.RemoveEntry(name)
		}
		rep := del.Report()
		notices = append(notices, del.Warnings()...)
		if cancelled {
			// Records may already be written and tokens deleted: the bare
			// cancellation would carry neither list.
			return runtime.Result{}, clierr.New(clierr.CodeInterrupted, fmt.Sprintf(
				"stopped while deleting the credentials of %s; the server entry is left in place; run it again to finish: %s",
				clierr.ShellQuote(name), deleteServerCommand(name))).
				WithDetails(rep).
				WithWarnings(notices...)
		}
		return runtime.Result{}, clierr.Wrap(localErr,
			fmt.Sprintf("failed to delete credentials for %q; the entry is kept with its cache writes paused, and logged out if its login token was deleted: %v", name, localErr)).
			WithDetails(rep).
			WithWarnings(notices...)
	}

	if del.Cancelled() {
		// The entry stays with no login - the local delete completed - and
		// the rerun the message names removes it: its records are reported
		// as that rerun will report them.
		del.RemoveEntry(name)
		rep := del.Report()
		notices = append(notices, del.Warnings()...)
		return runtime.Result{}, clierr.New(clierr.CodeInterrupted, fmt.Sprintf(
			"stopped before every token was revoked; the credentials are deleted and the server entry is left in place; run it again to remove it: %s",
			deleteServerCommand(name))).
			WithDetails(rep).
			WithWarnings(notices...)
	}

	// A signal after the first span that left no token unsent does not
	// stop the command: the entry is still removed, in a span the first signal
	// does not cancel (bounded by the config lock's wait; a second signal
	// ends the process).
	removed, currentCleared, final, ferr := d.removeEntry(context.WithoutCancel(cmdCtx), name)
	if final != nil {
		del.Refresh(final)
	}
	if removed {
		del.RemoveEntry(name)
	}
	rep := del.Report()
	notices = append(notices, del.Warnings()...)
	if ferr != nil {
		return runtime.Result{}, clierr.WrapUnlessCanceled(ferr, func(err error) error {
			return clierr.Wrap(err, err.Error()).
				WithDetails(rep).
				WithWarnings(notices...)
		})
	}

	deletedFrom := slices.Clone(del.DeletedFrom[name])
	slices.Sort(deletedFrom)
	deletedCache := del.DeletedCache
	data := map[string]any{
		"server":               name,
		"url":                  config.DisplayServerURL(srv.URL),
		"deletedFrom":          map[string][]string{"backend": deletedFrom},
		"deletedCache":         deletedCache,
		"currentServerCleared": currentCleared,
		"revoked":              rep.Revoked,
		"notRevoked":           rep.NotRevoked,
	}
	res := runtime.Result{Kind: "DeletedServer", Data: data, Warnings: notices}
	const pointerClearedLine = "was current - pointer cleared"
	var lines []string
	if currentCleared {
		lines = append(lines, pointerClearedLine)
	}
	if len(deletedFrom) > 0 {
		lines = append(lines, "credentials removed ("+strings.Join(deletedFrom, ", ")+")")
	}
	if len(deletedCache) > 0 {
		lines = append(lines, cachedCredsRemovedLine(len(deletedCache)))
	}
	lines = append(lines, del.TextLines()...)
	tr := runtime.TextLines("server "+name+" deleted", lines...)
	if currentCleared {
		tr = runtime.AccentLine(tr, pointerClearedLine)
	}
	res.Text, res.StyledText = tr.Text, tr.StyledText
	return res, nil
}

// removeEntry is delete-server's last span: the entry goes only while it
// still carries a LoggedOutAt stamp - any stamp, a concurrent logout's
// included - since every login and every URL change clears it; the binding
// id is not compared. An entry already gone (a concurrent delete-server of
// the same name finished first) is a success. Removing it clears the
// current-server pointer when it names it and resumes the other entries'
// cache writes where they may. It returns the config as it stands on disk
// when the span ends, nil when the span could not load it.
func (d configDeleteServerCmd) removeEntry(ctx context.Context, name string) (removed, currentCleared bool, final *config.Config, err error) {
	err = config.WithLock(ctx, d.dir, func() error {
		cfg, err := config.Load(config.FilePath(d.dir))
		if err != nil {
			return err
		}
		final = cfg
		s, ok := cfg.Servers[name]
		switch {
		case !ok:
			removed = true
			return nil
		case s.LoggedOutAt == "":
			return clierr.New(clierr.CodeConflict, fmt.Sprintf(
				"the entry %s changed since this command started (a login or a URL change); run it again: %s",
				clierr.ShellQuote(name), deleteServerCommand(name)))
		}
		delete(cfg.Servers, name)
		if cfg.CurrentServer == name {
			cfg.CurrentServer = ""
			currentCleared = true
		}
		config.ResumeCacheWrites(d.dir, cfg, name)
		if werr := config.WriteStep("remove", cfg); werr != nil {
			cfg.Servers[name] = s
			return werr
		}
		removed = true
		return nil
	})
	return removed, currentCleared, final, err
}

// CompleteArgs implements runtime.ShellCompleter: `config delete-server <TAB>`
// completes server names from the already-loaded config snapshot.
func (configDeleteServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView, _ []string) []string {
	return view.ServerNames()
}

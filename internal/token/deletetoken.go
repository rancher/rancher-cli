package token

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// Execute is auth delete-token: the records the argument names (a bare
// display name matches every record with that name, a composite key one),
// or every record with --all, deleted under the config lock, which also
// covers the write that records their tokens on the entries they belong
// to; then the revokes, outside every lock. An unknown or already-deleted
// name is a success with nothing removed, so teardown scripts can call it
// unconditionally.
func (d deleteTokenCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	all := in.Bool("all")
	args := in.Args()
	if all && len(args) > 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"specify either a credential name or --all, not both; to delete only "+clierr.ShellQuote(args[0])+", run: "+
				clierr.Rancher+" auth delete-token"+clierr.ShellQuoteArgs("", args[0])+"; to delete every cached credential, run: "+
				clierr.Rancher+" auth delete-token --all")
	}
	if !all && len(args) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"provide a cached-credential name or --all")
	}
	req := LocalDelete{Dir: d.dir, Command: DeleteForDeleteToken, Cache: d.store, Rerun: clierr.Rancher + " auth delete-token --all"}
	name, what := "", "the cached credentials"
	if !all {
		name = args[0]
		what = fmt.Sprintf("%q", name)
		req.Select = func(r config.CachedRecord) bool { return r.Name == name || r.Key == name }
		req.Rerun = clierr.Rancher + " auth delete-token" + clierr.ShellQuoteArgs("", name)
	}

	var del *Deletion
	var localErr error
	if err := config.WithLock(ctx, d.dir, func() error {
		cfg, err := config.Load(config.FilePath(d.dir))
		if err != nil {
			return err
		}
		del, localErr = DeleteAndRecord(ctx, cfg, req)
		if del == nil {
			return localErr
		}
		return nil
	}); err != nil {
		return runtime.Result{}, err
	}

	del.RevokeTokens(ctx)
	warnings := del.Settle(ctx)
	rep := del.Report()
	// The store reports both lists in key order.
	deleted, kept := del.DeletedCache, del.Kept
	if deleted == nil {
		deleted = []string{}
	}
	if kept == nil {
		kept = []string{}
	}
	for _, k := range kept {
		warnings = append(warnings, fmt.Sprintf("the cached credential %s was kept: the exec plugin wrote a new token to it while it was being deleted", k))
	}
	if del.CacheUnreadable {
		warnings = append(warnings, "token-cache.yaml could not be read; it was removed, and the tokens it held could not be revoked or recorded")
	}
	warnings = append(warnings, del.Warnings()...)

	if localErr != nil {
		return runtime.Result{}, clierr.WrapUnlessCanceled(localErr, func(err error) error {
			return clierr.Wrap(err, deleteFailureText(what, err, req.Rerun)).
				WithDetails(DeleteTokenFailure{Deleted: deleted, Kept: kept, Revoked: rep.Revoked, NotRevoked: rep.NotRevoked}).
				WithWarnings(warnings...)
		})
	}

	if del.Cancelled() {
		return runtime.Result{}, clierr.New(clierr.CodeInterrupted,
			"stopped before every token was revoked; the cached credentials are deleted, and the details list each token").
			WithDetails(DeleteTokenFailure{Deleted: deleted, Kept: kept, Revoked: rep.Revoked, NotRevoked: rep.NotRevoked}).
			WithWarnings(warnings...)
	}

	var lines []string
	switch {
	case all:
		lines = append(lines, fmt.Sprintf("deleted %d cached credentials", len(deleted)))
	case len(deleted) == 0 && len(kept) == 0:
		// Deletion is idempotent; ShapeText requires non-empty Text on
		// every default-format success, so the zero-deleted case needs
		// its own line too.
		lines = append(lines, "no matching cached credential")
	default:
		for _, key := range deleted {
			lines = append(lines, "cached credential "+key+" deleted")
		}
	}
	for _, key := range kept {
		lines = append(lines, "cached credential "+key+" kept: the exec plugin wrote a new token to it meanwhile")
	}
	lines = append(lines, del.TextLines()...)
	return runtime.Result{Kind: "TokenDelete", Data: map[string]any{
		"deleted": deleted, "kept": kept, "revoked": rep.Revoked, "notRevoked": rep.NotRevoked,
	}, Text: strings.Join(lines, "\n") + "\n", Warnings: warnings}, nil
}

// deleteFailureText is the message of a failed local delete: what it tried to
// delete, why it failed, and the command that finishes the delete.
func deleteFailureText(what string, err error, rerun string) string {
	return fmt.Sprintf("failed to delete %s: %v; to finish, run: %s", what, err, rerun)
}

// DeleteTokenFailure is auth delete-token's structured payload for a
// delete that did not complete: what was deleted and kept, and what the
// server did with each token.
type DeleteTokenFailure struct {
	Deleted    []string          `json:"deleted"`
	Kept       []string          `json:"kept"`
	Revoked    []RevokedToken    `json:"revoked"`
	NotRevoked []NotRevokedToken `json:"notRevoked"`
}

// TextDetails lists what was deleted and each token that may still be
// live.
func (p DeleteTokenFailure) TextDetails() []clierr.DetailLine {
	var lines []clierr.DetailLine
	if len(p.Deleted) > 0 {
		lines = append(lines, clierr.Line(clierr.ListParts("deleted: ", p.Deleted)...))
	}
	for _, n := range p.NotRevoked {
		lines = append(lines, n.DetailLines()...)
	}
	return lines
}

// MapText maps the text of each token that may still be live: its error
// message and its recovery commands.
func (p DeleteTokenFailure) MapText(f func(string) string) clierr.TextDetails {
	p.NotRevoked = MapLiveTokens(p.NotRevoked, f, f)
	return p
}

// MapCommands maps the recovery commands of each token that may still be
// live, which a renderer that can offer no command empties.
func (p DeleteTokenFailure) MapCommands(f func(string) string) clierr.TextDetails {
	p.NotRevoked = MapLiveTokens(p.NotRevoked, nil, f)
	return p
}

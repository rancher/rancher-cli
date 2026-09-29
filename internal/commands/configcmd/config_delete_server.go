package configcmd

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configDeleteServerCmd struct {
	dir string
	// delete is the credential/exec-cache delete, injectable for tests
	// (the authcheck preflight-seam pattern, authcheck.go); defaults to
	// config.DeleteTokenEverywhere - the single delete point (ARCHITECTURE.md), whose
	// only other caller is auth logout.
	delete func(ctx context.Context, dir string, ref config.ServerRef, policy string) (fromBackends, deletedCache, notices []string, err error)
}

// NewConfigDeleteServer builds `config delete-server <name>` (local): removes
// a configured server (clearing the current-server pointer when it names it)
// and deletes its stored credentials - the orphaned-creds cleanup nothing
// else performs today. It does NOT revoke the server-side token (that is
// `auth logout --revoke`, ledgered/out of scope).
func NewConfigDeleteServer(dir string) runtime.Command {
	return &configDeleteServerCmd{dir: dir, delete: config.DeleteTokenEverywhere}
}

func (configDeleteServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config delete-server",
		Usage:       "Delete a configured server and its stored credentials",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher config delete-server prod"},
		Shape:       runtime.ShapeText,
	}
}

// Execute's ordering is safety-critical. The directory's unfinished store
// change goes first, after the not-found check and before any write, so a
// change record that cannot be read refuses with the config untouched and
// nothing deleted. The first config write advances
// the entry's cache binding id and pauses its cache writes, and nothing
// else: a get-token racing the purge below then writes its record under an
// id the entry no longer has, or writes nothing. Credentials are deleted
// next, and a delete failure aborts with the entry, the pointer and every
// other field as they were - only the id and the pause have moved. Only
// after a successful delete does the last config write (server removed,
// current-server pointer cleared if it named the deleted server, other
// entries' pauses resumed where they may be) happen. A delete failure must
// never leave a live orphaned token behind a disappeared server entry -
// the exact bug this command exists to kill; the benign failure direction
// is creds gone + server still configured, never the reverse.
func (d configDeleteServerCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]

	var (
		srv            config.Server
		currentCleared bool
		deletedFrom    []string
		deletedCache   []string
		notices        []string
		recoveryData   map[string]any
		recoveryLines  []string
		hasRecovery    bool
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
		if srv.HoldsCLITokens() {
			// An entry whose provenance says the CLI created the token but
			// that names none has nothing to revoke: no recovery key.
			recoveryData, recoveryLines = recoveryReport(srv)
			hasRecovery = len(recoveryData) > 0
		}
		policy, perr := config.EffectivePolicy(cfg.CredentialStore)
		if perr != nil {
			return perr
		}
		prelude, perr := config.PrepareCredentialDelete(cmdCtx, d.dir, cfg)
		if perr != nil {
			return perr
		}
		entry := srv
		if berr := config.StartCredentialChange(&entry); berr != nil {
			return config.WithNotices(berr, prelude)
		}
		cfg.Servers[name] = entry
		if werr := config.WriteStep("cachebinding", cfg); werr != nil {
			return config.WithNotices(werr, prelude)
		}
		var deleted []string
		deletedFrom, deletedCache, deleted, err = d.delete(cmdCtx, d.dir, config.ServerRef{Name: name, URL: srv.URL}, policy)
		notices = slices.Concat(prelude, deleted)
		if err != nil {
			return clierr.WrapUnlessCanceled(err, func(err error) error {
				return clierr.Wrap(err,
					fmt.Sprintf("failed to delete credentials for %q; the server entry is left in place, with only its cache binding id advanced and its cache writes paused: %v", name, err)).
					WithWarnings(notices...)
			})
		}
		delete(cfg.Servers, name)
		if cfg.CurrentServer == name {
			cfg.CurrentServer = ""
			currentCleared = true
		}
		config.ResumeCacheWrites(d.dir, cfg, name)
		return config.WithNotices(config.WriteStep("remove", cfg), notices)
	}); err != nil {
		return runtime.Result{}, err
	}

	sort.Strings(deletedFrom)
	sort.Strings(deletedCache)
	data := map[string]any{
		"server":               name,
		"url":                  srv.URL,
		"deletedFrom":          map[string][]string{"backend": deletedFrom},
		"deletedCache":         deletedCache,
		"currentServerCleared": currentCleared,
	}
	if hasRecovery {
		data["recovery"] = recoveryData
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
	lines = append(lines, recoveryLines...)
	tr := runtime.TextLines("server "+name+" deleted", lines...)
	if currentCleared {
		tr = runtime.AccentLine(tr, pointerClearedLine)
	}
	res.Text, res.StyledText = tr.Text, tr.StyledText
	return res, nil
}

// CompleteArgs implements runtime.ShellCompleter: `config delete-server <TAB>`
// completes server names from the already-loaded config snapshot (AC-19).
func (configDeleteServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
}

// recoveryReport is delete-server's account of the tokens an entry names
// on the server, returned in the same invocation that removes the entry,
// so the operator keeps a way to revoke them.
func recoveryReport(s config.Server) (map[string]any, []string) {
	rep := map[string]any{}
	var lines []string
	if s.CLIOwned() {
		rep["current"] = tokenReport(s.URL, *s.TokenRef, s.TokenUserID, s.CredentialExpiresAt)
		owner := s.TokenUserID
		if owner == "" {
			owner = "unknown owner"
		}
		lines = append(lines, fmt.Sprintf("its login credential's token %s (%s) stays live on the server", s.TokenRef.String(), owner))
		lines = append(lines, sequenceLines("  ", clierr.RecoverySequence(s.URL, s.TokenUserID, *s.TokenRef))...)
	}
	if len(s.SupersededTokens) > 0 {
		recs := make([]map[string]any, 0, len(s.SupersededTokens))
		for _, r := range s.SupersededTokens {
			recs = append(recs, recordReport(s.URL, s, r))
		}
		rep["superseded"] = recs
		lines = append(lines, recordLines(s, "")...)
	}
	return rep, lines
}

package cli

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/require"
)

// fuzzCandidatesBody answers every request of the fake server: two names in the shape of a Steve
// list (data) and of a Kubernetes list (items). The second name holds characters that
// sanitizeCandidates drops.
const fuzzCandidatesBody = `{"count":2,"revision":"1",` +
	`"data":[{"id":"alpha","metadata":{"name":"alpha"},"spec":{"displayName":"alpha"}},` +
	`{"id":"-bad name;x","metadata":{"name":"-bad name;x"},"spec":{"displayName":"-bad name;x"}}],` +
	`"items":[{"metadata":{"name":"alpha"}},{"metadata":{"name":"-bad name;x"}}]}`

// FuzzCompletionCandidatesParse fuzzes the arguments typed before a completion request. A
// candidate that completion offers, put after the arguments, must give a command line the command
// accepts: the command line runs
// through the CLI's own checks and ends with no error, or with an error other than a flag or
// value error. Live sources answer from a fake server on the machine. Never call t.Parallel()
// here: it panics inside f.Fuzz. The seeds are the first two rows of each candidates file: every
// seed replays in each run of the package's tests, and a fuzz run reaches the other rows.
func FuzzCompletionCandidatesParse(f *testing.F) {
	for _, line := range fuzzGoldenLines(f, false, 2, "candidates") {
		words, current := splitCompletionLine(line)
		blob, has := fuzzJoin(words)
		f.Add(blob, has, current)
	}
	dir := fuzzHermetic(f)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fuzzCandidatesBody))
	}))
	f.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL, CA: ca}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(f, cfg.Write())
	f.Setenv(config.EnvToken, "fuzz-token")
	anchor := config.NewCacheAnchor(dir)
	setStoredConfig(cfg, dir)
	config.SetProcessCacheAnchor(anchor.WithConfig(cfg))
	f.Cleanup(func() {
		config.SetProcessCacheAnchor(nil)
		setStoredConfig(nil, "")
	})

	completionRoot, err := BuildRoot("v-fuzz")
	require.NoError(f, err)
	checked := newWrappedTree()
	require.NoError(f, checked.err)
	real, err := BuildRoot("v-fuzz")
	require.NoError(f, err)
	checked.root.Before = real.Before

	f.Fuzz(func(t *testing.T, argsBlob string, hasArgs bool, currentWord string) {
		if len(argsBlob)+len(currentWord) > 64<<10 {
			t.Skip("input over 64 KiB")
		}
		words := fuzzSplit(argsBlob, hasArgs)
		if fuzzHasConfigWord(words) {
			t.Skip("--config")
		}
		httpx.ClearRecords()

		var reply bytes.Buffer
		completionRoot.Writer, completionRoot.ErrWriter, completionRoot.Reader = &reply, io.Discard, strings.NewReader("")
		args := candidateLine(words, currentWord)
		CaptureArgv(args)
		_ = completionRoot.Run(t.Context(), append([]string{"rancher"}, args...))

		for line := range strings.SplitSeq(reply.String(), "\n") {
			if line == "" {
				continue
			}
			token := candidateToken(line)
			if strings.HasSuffix(token, "/") || strings.HasSuffix(token, "=") {
				continue
			}
			line := append(slices.Clone(words), token)
			*checked.obs = observed{}
			checked.root.Writer, checked.root.ErrWriter, checked.root.Reader = io.Discard, io.Discard, strings.NewReader("")
			CaptureArgv(line)
			_, oc := runArgv(t.Context(), checked.root, append([]string{"rancher"}, line...), false)
			if oc.err == nil {
				continue
			}
			if oc.kind == outcomeUsageError && oc.class == missingFlagValue {
				p := parseLine(checked.root, line)
				if p.levels[p.end.level].usageErr.word == token {
					continue
				}
			}
			ce, ok := clierrOf(oc.err)
			if !ok || ce.Code == clierr.CodeValidation {
				t.Fatalf("rancher %q (completing %q) offers %q, which the command rejects: %v", words, currentWord, token, oc.err)
			}
		}
		for _, h := range httpx.AnsweredHosts() {
			require.Equal(t, srv.URL, h.Origin, "a request went to a host other than the fake server")
		}
	})
}

package cmdtest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
)

// ChangeStep is one step of a credential store change record a test
// writes. Token is the credential a delete step may remove: the record
// carries its digest, as a real change does, never the token.
type ChangeStep struct {
	Kind, Entry, Source, Target string
	Token                       string
	CanonicalURL                string
	SourceAccountID             string
	LastError                   string
}

// WriteChangeRecord writes a change record with steps at the directory's
// fixed path, as raw YAML so no validation stands between the test and
// the file, and returns its id. With named, config.yaml names it, which
// is what makes it a change in flight; without, it is a change that never
// started.
func WriteChangeRecord(t *testing.T, dir string, named bool, steps ...ChangeStep) string {
	t.Helper()
	id := randomHex(t)
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nid: %s\nsteps:\n", id)
	for _, s := range steps {
		fmt.Fprintf(&b, "- kind: %s\n", s.Kind)
		for _, kv := range [][2]string{
			{"entry", s.Entry}, {"source", s.Source}, {"target", s.Target},
			{"canonicalUrl", s.CanonicalURL}, {"sourceAccountId", s.SourceAccountID},
			{"lastError", s.LastError},
		} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "  %s: %q\n", kv[0], kv[1])
			}
		}
		if s.Kind == "delete" {
			vid := randomHex(t)
			fmt.Fprintf(&b, "  verificationId: %s\n  credentialDigest: %s\n", vid, digest(vid, s.Token))
		}
	}
	require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte(b.String()), 0o600))
	if named {
		NameChangeRecord(t, dir, id)
	}
	return id
}

// WriteDamagedChangeRecord writes a change record no release can read and,
// with named, names an id in config.yaml. Returns the file's bytes.
func WriteDamagedChangeRecord(t *testing.T, dir string, named bool) []byte {
	t.Helper()
	data := []byte("not: [yaml")
	require.NoError(t, os.WriteFile(config.TransitionPath(dir), data, 0o600))
	if named {
		NameChangeRecord(t, dir, randomHex(t))
	}
	return data
}

// NameChangeRecord writes id into config.yaml as the change the directory
// has in flight. With no file under that id it reads as a change whose
// last step finished before its name was cleared.
func NameChangeRecord(t *testing.T, dir, id string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialTransitionID = id
	require.NoError(t, cfg.Write())
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// digest is the verifier a change record carries for a credential: an
// HMAC-SHA256 keyed by the step's verification id.
func digest(vid, token string) string {
	mac := hmac.New(sha256.New, []byte(vid))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

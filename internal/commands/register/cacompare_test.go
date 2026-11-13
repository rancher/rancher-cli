package register

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

const (
	caTestHTTPS   = "https://qa1.example.com"
	caTestHTTP    = "http://qa1.example.com"
	noCertificate = "cannot be compared: the text holds no certificate"
	noSystemRoots = "none (system roots)"
	noOtherLacks  = "no certificate the other side lacks"
	conflictTail  = "; to add a new server choose a different name, or to change this entry's trust run: "
)

// caFixture holds six generated certificates (index 1 to 6) with their
// SHA-256 fingerprints, and a temporary directory for the files the cases
// write.
type caFixture struct {
	dir  string
	pems [7]string
	fps  [7]string
}

func newCAFixture(t *testing.T) *caFixture {
	t.Helper()
	f := &caFixture{dir: t.TempDir()}
	for i := 1; i <= 6; i++ {
		f.pems[i] = cmdtest.CertificatePEM(t)
		f.fps[i] = onlyFingerprint(t, f.pems[i])
	}
	return f
}

func (f *caFixture) write(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

func onlyFingerprint(t *testing.T, pemText string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return tlstrust.Fingerprint(cert)
}

func readProblem(path string) string {
	_, err := os.ReadFile(path)
	return "cannot be compared: " + config.CAFileProblem(err)
}

func probeFor(t *testing.T, url, selector string) Probe {
	t.Helper()
	bytes, err := ResolveCA(url, selector)
	require.NoError(t, err)
	return Probe{CanonicalURL: url, CASelector: selector, CABytes: bytes}
}

func conflictHint(selector string) string {
	return clierr.NewCommand("config update-server").Arg("qa1").With(CAUpdateArg(selector)).String()
}

type caCase struct {
	name string
	// setup writes the files and returns the saved selector and the
	// --cacert selector.
	setup func(t *testing.T, f *caFixture) (saved, given string)
	// want is "noop" for the identical entry, "kept" for the same
	// certificates under another selector, "conflict" otherwise.
	want string
	// sides returns the text of the saved and the --cacert side in the
	// conflict message.
	sides func(f *caFixture, saved, given string) (string, string)
}

func caCases() []caCase {
	return []caCase{
		{
			name: "same file",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				a := f.write(t, "a.pem", f.pems[1])
				return a, a
			},
			want: "noop",
		},
		{
			name: "different path same certificate",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", f.pems[1])
			},
			want: "kept",
		},
		{
			name: "different path different certificate",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", f.pems[2])
			},
			want: "conflict",
			sides: func(f *caFixture, _, _ string) (string, string) {
				return f.fps[1], f.fps[2]
			},
		},
		{
			name: "bundle same certificates another order",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]+f.pems[2]), f.write(t, "b.pem", f.pems[2]+f.pems[1])
			},
			want: "kept",
		},
		{
			name: "bundle one extra certificate",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", f.pems[1]+f.pems[2])
			},
			want: "conflict",
			sides: func(f *caFixture, _, _ string) (string, string) {
				return noOtherLacks, f.fps[2]
			},
		},
		{
			name: "bundle one certificate fewer",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]+f.pems[2]), f.write(t, "b.pem", f.pems[1])
			},
			want: "conflict",
			sides: func(f *caFixture, _, _ string) (string, string) {
				return f.fps[2], noOtherLacks
			},
		},
		{
			name: "bundles sharing four certificates",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				shared := f.pems[1] + f.pems[2] + f.pems[3] + f.pems[4]
				return f.write(t, "a.pem", shared+f.pems[5]), f.write(t, "b.pem", shared+f.pems[6])
			},
			want: "conflict",
			sides: func(f *caFixture, _, _ string) (string, string) {
				return f.fps[5], f.fps[6]
			},
		},
		{
			name: "bundle with a repeated certificate",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", f.pems[1]+f.pems[1])
			},
			want: "kept",
		},
		{
			name: "CRLF copy of the same PEM",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", strings.ReplaceAll(f.pems[1], "\n", "\r\n"))
			},
			want: "kept",
		},
		{
			name: "CRLF in the header lines only",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				mixed := strings.ReplaceAll(f.pems[1], "CERTIFICATE-----\n", "CERTIFICATE-----\r\n")
				require.NotEqual(t, f.pems[1], mixed)
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", mixed)
			},
			want: "kept",
		},
		{
			name: "inline versus file",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.pems[1], f.write(t, "b.pem", f.pems[1])
			},
			want: "kept",
		},
		{
			name: "file versus inline",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), f.pems[1]
			},
			want: "kept",
		},
		{
			name: "extra block that is not a certificate",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				extra := f.pems[1] + "-----BEGIN PRIVATE KEY-----\nZHVtbXk=\n-----END PRIVATE KEY-----\ntrailing text\n"
				return f.write(t, "a.pem", f.pems[1]), f.write(t, "b.pem", extra)
			},
			want: "kept",
		},
		{
			name: "saved CA and system roots",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return f.write(t, "a.pem", f.pems[1]), ""
			},
			want: "conflict",
			sides: func(f *caFixture, _, _ string) (string, string) {
				return f.fps[1], noSystemRoots
			},
		},
		{
			name: "saved no CA and a --cacert",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return "", f.write(t, "b.pem", f.pems[1])
			},
			want: "conflict",
			sides: func(f *caFixture, _, _ string) (string, string) {
				return noSystemRoots, f.fps[1]
			},
		},
		{
			name: "unreadable saved CA",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return filepath.Join(f.dir, "gone.pem"), f.write(t, "b.pem", f.pems[1])
			},
			want: "conflict",
			sides: func(f *caFixture, saved, _ string) (string, string) {
				return readProblem(saved), f.fps[1]
			},
		},
		{
			name: "no certificate on either side",
			setup: func(t *testing.T, f *caFixture) (string, string) {
				t.Helper()
				return cmdtest.CAPem1, f.write(t, "b.pem", cmdtest.CAPem1)
			},
			want: "conflict",
			sides: func(*caFixture, string, string) (string, string) {
				return noCertificate, noCertificate
			},
		},
	}
}

// splitConflict cuts the message at the tail that keeps the --cacert value
// as typed, and returns the head and the tail.
func splitConflict(msg string) (string, string) {
	head, rest, ok := strings.Cut(msg, "; to add a new server")
	if !ok {
		return msg, ""
	}
	return head, "; to add a new server" + rest
}

// assertConflict asserts the CONFLICT of a different CA: the code, the
// head with the two sides and no path or PEM text, the unchanged tail and
// hint, and the entry untouched.
func assertConflict(t *testing.T, f *caFixture, err error, given, wantSaved, wantGiven string) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	head, tail := splitConflict(ce.Message)
	assert.Equal(t, `server entry "qa1" already exists with a different CA (saved: `+wantSaved+`; --cacert: `+wantGiven+`)`, head)
	assert.NotContains(t, head, f.dir)
	assert.NotContains(t, head, "-----BEGIN")
	assert.Equal(t, conflictTail+conflictHint(given), tail)
	assert.Equal(t, conflictHint(given), ce.Hint)
}

// TestCommitCreateSameURLCompareCertificatesNotSelectors asserts, over every
// way two selectors can relate, that an entry keeps its saved CA when the
// --cacert value holds the same certificates and that any other CA is a
// CONFLICT that lists the fingerprints that differ.
func TestCommitCreateSameURLCompareCertificatesNotSelectors(t *testing.T) {
	for _, tc := range caCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCAFixture(t)
			saved, given := tc.setup(t, f)
			original := config.Server{URL: caTestHTTPS, CA: saved}
			cfg := writableCfg(t, map[string]config.Server{"qa1": original}, "qa1")
			before := readBytes(t, cfg.Path())
			probe := probeFor(t, caTestHTTPS, given)

			out, err := commitCreate(cfg, "qa1", probe)

			assert.Equal(t, original, cfg.Servers["qa1"], "the entry is never rewritten")
			assert.Equal(t, before, readBytes(t, cfg.Path()), "no config write")
			switch tc.want {
			case "noop":
				require.NoError(t, err)
				assert.False(t, out.Created)
				assert.False(t, out.KeptSavedCA)
				assert.Equal(t, saved, out.CASelector)
				assert.Equal(t, probe.CABytes, out.CABytes)
			case "kept":
				require.NoError(t, err)
				assert.False(t, out.Created)
				assert.True(t, out.KeptSavedCA)
				assert.Equal(t, saved, out.CASelector, "the outcome describes the saved entry")
				savedBytes, rerr := config.ResolveCA(saved)
				require.NoError(t, rerr)
				assert.Equal(t, savedBytes, out.CABytes, "the bytes of the saved selector")
			default:
				wantSaved, wantGiven := tc.sides(f, saved, given)
				assertConflict(t, f, err, given, wantSaved, wantGiven)
			}
		})
	}
}

// TestCommitCreateKeptSavedCARepairsAnEmptyPointer asserts that keeping the
// saved CA still bootstraps an empty current-server pointer, as the
// identical entry does, and leaves the entry as saved.
func TestCommitCreateKeptSavedCARepairsAnEmptyPointer(t *testing.T) {
	f := newCAFixture(t)
	saved := f.write(t, "a.pem", f.pems[1])
	given := f.write(t, "b.pem", f.pems[1])
	original := config.Server{URL: caTestHTTPS, CA: saved}
	cfg := writableCfg(t, map[string]config.Server{"qa1": original}, "")

	out, err := commitCreate(cfg, "qa1", probeFor(t, caTestHTTPS, given))
	require.NoError(t, err)
	assert.True(t, out.KeptSavedCA)
	assert.False(t, out.Created)
	assert.True(t, out.PointerRepaired)
	assert.Equal(t, "qa1", out.CurrentServer)
	assert.Equal(t, original, cfg.Servers["qa1"])
}

// TestCommitCreateHTTPSameCertificateKeepsSavedCAWithNoBytes asserts that
// under http the saved CA is kept for the same certificates and the bytes
// handed back are empty (an http entry never reads its CA for trust), and
// that once the saved file is gone the same call is a CONFLICT with no
// other failure.
func TestCommitCreateHTTPSameCertificateKeepsSavedCAWithNoBytes(t *testing.T) {
	f := newCAFixture(t)
	saved := f.write(t, "a.pem", f.pems[1])
	given := f.write(t, "b.pem", strings.ReplaceAll(f.pems[1], "\n", "\r\n"))
	original := config.Server{URL: caTestHTTP, CA: saved}
	cfg := writableCfg(t, map[string]config.Server{"qa1": original}, "qa1")
	probe := probeFor(t, caTestHTTP, given)
	require.Empty(t, probe.CABytes)

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.True(t, out.KeptSavedCA)
	assert.Equal(t, saved, out.CASelector)
	assert.Empty(t, out.CABytes, "the bytes of an http selector are empty")

	require.NoError(t, os.Remove(saved))
	_, err = commitCreate(cfg, "qa1", probe)
	assertConflict(t, f, err, given, readProblem(saved), f.fps[1])
	assert.Equal(t, original, cfg.Servers["qa1"])
}

// TestCommitCreateHTTPUnreadableNewFileIsAConflict asserts that under http a
// --cacert file that cannot be read is a CONFLICT that says the --cacert
// side cannot be compared and names no part of the path in the text before
// the tail. A path below a plain file is the unreadable file.
func TestCommitCreateHTTPUnreadableNewFileIsAConflict(t *testing.T) {
	f := newCAFixture(t)
	saved := f.write(t, "a.pem", f.pems[1])
	plain := f.write(t, "plain.txt", "not a directory")
	given := filepath.Join(plain, "ca.pem")
	original := config.Server{URL: caTestHTTP, CA: saved}
	cfg := writableCfg(t, map[string]config.Server{"qa1": original}, "qa1")
	probe := probeFor(t, caTestHTTP, given)

	_, err := commitCreate(cfg, "qa1", probe)
	_, rerr := os.ReadFile(given)
	require.Error(t, rerr, "the --cacert path is unreadable on this system")
	assertConflict(t, f, err, given, f.fps[1], readProblem(given))
	assert.Equal(t, original, cfg.Servers["qa1"])
}

// TestCommitCreateHTTPSUnreadableNewFileFailsInPrepare asserts that under
// https a --cacert file that cannot be read never reaches the comparison:
// Prepare refuses it as VALIDATION.
func TestCommitCreateHTTPSUnreadableNewFileFailsInPrepare(t *testing.T) {
	f := newCAFixture(t)
	plain := f.write(t, "plain.txt", "not a directory")

	_, err := Prepare(t.Context(), nil, caTestHTTPS, filepath.Join(plain, "ca.pem"))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.True(t, strings.HasPrefix(ce.Message, "reading --cacert: "))
}

// TestCertificateFingerprintsFollowTheRootPoolRule asserts the set of
// certificates a text holds: sorted, without repeats, only blocks of type
// CERTIFICATE without headers that parse, CRLF line endings accepted.
func TestCertificateFingerprintsFollowTheRootPoolRule(t *testing.T) {
	f := newCAFixture(t)
	want := []string{f.fps[1], f.fps[2]}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	assert.Equal(t, want, certificateFingerprints(f.pems[2]+f.pems[1]+f.pems[2]))
	assert.Equal(t, want, certificateFingerprints(strings.ReplaceAll(f.pems[1]+f.pems[2], "\n", "\r\n")))

	withHeader := strings.Replace(f.pems[3], "-----BEGIN CERTIFICATE-----\n", "-----BEGIN CERTIFICATE-----\nProc-Type: 4,ENCRYPTED\n\n", 1)
	notCertificate := "-----BEGIN PRIVATE KEY-----\nZHVtbXk=\n-----END PRIVATE KEY-----\n"
	badBlock := "-----BEGIN CERTIFICATE-----\nZHVtbXk=\n-----END CERTIFICATE-----\n"
	assert.Equal(t, []string{f.fps[1]}, certificateFingerprints(withHeader+notCertificate+badBlock+f.pems[1]))
	assert.Empty(t, certificateFingerprints(cmdtest.CAPem1))
	assert.Empty(t, certificateFingerprints(""))
}

// TestKeptSavedCAWarningNamesNoFlag asserts the warning is true whether the
// given CA was a --cacert value or a certificate accepted at the trust
// prompt: it names no flag.
func TestKeptSavedCAWarningNamesNoFlag(t *testing.T) {
	warning := KeptSavedCAWarning("qa1")
	assert.Equal(t, `server entry "qa1" keeps its saved CA, which holds the same certificates`, warning)
	assert.NotContains(t, warning, "--")
}

package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// tlsCert is a self-signed certificate with subject name and the DNS names.
func tlsCert(t *testing.T, name string, dns ...string) *x509.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

// unknownAuthorityFailure is the error Go reports for a certificate no CA of
// the client signs; UnverifiedCertificates is set, without which the details
// carry no fingerprint.
func unknownAuthorityFailure(cert *x509.Certificate) error {
	return &tls.CertificateVerificationError{
		UnverifiedCertificates: []*x509.Certificate{cert},
		Err:                    x509.UnknownAuthorityError{Cert: cert},
	}
}

// hostnameFailure is the error Go reports for a certificate that names
// another host.
func hostnameFailure(cert *x509.Certificate, host string) error {
	return &tls.CertificateVerificationError{
		UnverifiedCertificates: []*x509.Certificate{cert},
		Err:                    x509.HostnameError{Certificate: cert, Host: host},
	}
}

// answer is what a seeded transport gives one request: a transport error,
// or a status.
type answer struct {
	err    error
	status int
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// seedClient puts a client for trust in d whose transport answers the logout
// and the delete request as given, wrapped by the certificate classifier for
// the fixture server's origin, with the origin's host:port as the revoke
// code keeps it. It runs after the local delete and before the revokes.
func (x *revFixture) seedClient(d *Deletion, trust string, fromVariable bool, logout, del answer) {
	x.t.Helper()
	x.seedClientFunc(d, trust, fromVariable, func(r *http.Request) answer {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, LogoutPath) {
			return logout
		}
		return del
	})
}

// seedClientFunc is seedClient with the answer chosen per request.
func (x *revFixture) seedClientFunc(d *Deletion, trust string, fromVariable bool, answerFor func(*http.Request) answer) {
	x.t.Helper()
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		a := answerFor(r)
		if a.err != nil {
			return nil, a.err
		}
		return &http.Response{StatusCode: a.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	})
	wrap := tlstrust.WrapTransportFor(x.url, tlstrust.Remedy{})
	require.NotNil(x.t, wrap)
	u, err := url.Parse(x.url)
	require.NoError(x.t, err)
	origin, err := httpx.OriginOf(u)
	require.NoError(x.t, err)
	d.clients[trust] = revokeClient{
		client:       &http.Client{Transport: wrap(next), Timeout: 5 * time.Second},
		base:         kube.BaseURL(x.url),
		hostPort:     origin.HostPort(),
		fromVariable: fromVariable,
	}
}

// runSeeded is the local delete, the revokes through the seeded client, the
// record write and the report; end runs between the record write and the
// report, as a command removes its entry there.
func (x *revFixture) runSeeded(req LocalDelete, trust string, fromVariable bool, logout, del answer, end func(*Deletion)) (*Deletion, RevocationReport) {
	x.t.Helper()
	d, err := x.localDelete(req)
	require.NoError(x.t, err)
	x.seedClient(d, trust, fromVariable, logout, del)
	d.RevokeTokens(x.t.Context())
	require.Empty(x.t, d.Settle(x.t.Context()))
	if end != nil {
		end(d)
	}
	return d, d.Report()
}

const (
	unknownAuthorityTail = "; if this server worked before, check the subject and issuer shown here - " +
		"a certificate you don't recognize can mean something between you and the server, such as a proxy, is replacing its certificate"
)

// tlsRevokeRow is one place a revoke can get its wording from: a token
// that the next login of its entry revokes, one only the recovery login
// revokes, and a v3 token only a Rancher administrator deletes.
type tlsRevokeRow struct {
	name string
	// setup builds the fixture and returns the request, the entry the
	// request used and the token's name.
	setup func(t *testing.T) (*revFixture, LocalDelete, string)
	retry string
	// ca is the clause an unknown authority failure carries, by whether the
	// variable supplied the CA.
	caEntry, caVariable string
}

func tlsRevokeRows() []tlsRevokeRow {
	return []tlsRevokeRow{
		{
			name: "nextLogin", retry: retryNextLogin,
			setup: func(t *testing.T) (*revFixture, LocalDelete, string) {
				t.Helper()
				x := newRevFixture(t, testrancher.Options{})
				login := x.login("prod", "alice")
				return x, x.req(DeleteForLogout, "prod"), login.Ref.Name
			},
			caEntry:    "; save it for entry 'prod' with: rancher config update-server 'prod' --cacert <file>" + unknownAuthorityTail,
			caVariable: "; the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA; to save a CA for entry 'prod', run: rancher config update-server 'prod' --cacert <file>" + unknownAuthorityTail,
		},
		{
			name: "manual", retry: retryManual,
			setup: func(t *testing.T) (*revFixture, LocalDelete, string) {
				t.Helper()
				x := newRevFixture(t, testrancher.Options{})
				x.login("prod", "bob")
				target := x.token("bob")
				// The record's owner is not the login's, so no later login of
				// the entry revokes it.
				x.cache("k", target, x.own("prod").WithCreatedToken(target.Ref, "user-other"), false)
				return x, x.req(DeleteForLogout, "prod"), target.Ref.Name
			},
			caEntry:    " and add --cacert <file> to the auth login in the recovery steps" + unknownAuthorityTail,
			caVariable: "; the CA came from RANCHER_CLI_CACERT - set it to the server's CA before the recovery steps" + unknownAuthorityTail,
		},
		{
			name: "v3", retry: retryManual,
			setup: func(t *testing.T) (*revFixture, LocalDelete, string) {
				t.Helper()
				x := stubV3Fixture(t, newV3Stub(t, 0, 0))
				return x, x.req(DeleteForLogout, "prod"), stubV3Name
			},
			caEntry:    "; a Rancher administrator who deletes the token needs that CA saved on their own entry for this server; run: rancher config update-server <entry> --cacert <file>" + unknownAuthorityTail,
			caVariable: "; the CA came from RANCHER_CLI_CACERT in this command's environment - a Rancher administrator who deletes the token sets it to the server's CA in their own environment" + unknownAuthorityTail,
		},
	}
}

// tlsFailure is a certificate failure on every request.
func tlsFailure(err error) answer { return answer{err: err} }

// TestEachRevokeRowWordsAnUnknownAuthorityForWhatRevokesTheToken asserts the
// CA clause of a certificate failure of a revoke for every row: what revokes
// the token (the next login of its entry, the recovery login, a Rancher
// administrator's delete) by whether RANCHER_CLI_CACERT supplied the CA, with
// the code, the certificate's fingerprint and no remedy marker. Mutations:
// ignore retry in revokeTLSRemedy; ignore fromVariable.
func TestEachRevokeRowWordsAnUnknownAuthorityForWhatRevokesTheToken(t *testing.T) {
	for _, row := range tlsRevokeRows() {
		for _, fromVariable := range []bool{false, true} {
			name := row.name + ", the CA from the entry or none"
			want := row.caEntry
			if fromVariable {
				name, want = row.name+", the CA from the variable", row.caVariable
			}
			t.Run(name, func(t *testing.T) {
				x, req, token := row.setup(t)
				cert := tlsCert(t, "private-ca")
				_, rep := x.runSeeded(req, "prod", fromVariable, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
				n := notRevoked(t, rep, token)
				assert.Equal(t, row.retry, n.Retry, "the fixture is the row's")
				assert.Equal(t, "failed", n.Reason)
				assert.Equal(t, clierr.CodeTLSTrust, n.Code)
				assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), want)
				assert.Contains(t, n.Error, "the server presented a certificate signed by an unknown authority")
				assert.Equal(t, strings.Contains(want, "rancher config update-server"), strings.Contains(n.Error, clierr.RemedyMarker), "the message opens a command with the marker exactly when it names one")
				require.NotNil(t, n.Certificate)
				assert.Equal(t, tlstrust.KindUnknownAuthority, n.Certificate.Kind)
				assert.Equal(t, tlstrust.Fingerprint(cert), n.Certificate.Fingerprint)
			})
		}
	}
}

// TestEachRevokeRowNamesTheEntryTheRequestUsed asserts the URL clause of a
// hostname mismatch names the entry the failed request used in every row,
// the next-login row included, whether or not the command removed the entry.
// Mutation: build the URL clause from another entry or text.
func TestEachRevokeRowNamesTheEntryTheRequestUsed(t *testing.T) {
	const clause = "check that the server URL of entry 'prod' matches the certificate's hostname; a different CA cannot fix a hostname mismatch"
	for _, row := range tlsRevokeRows() {
		for _, removed := range []bool{false, true} {
			name := row.name + ", the entry stays"
			if removed {
				name = row.name + ", the command removed the entry"
			}
			t.Run(name, func(t *testing.T) {
				x, _, token := row.setup(t)
				req := x.req(DeleteForLogout, "prod")
				if removed {
					req = x.req(DeleteForDeleteServer, "prod")
				}
				cert := tlsCert(t, "other", "other.example")
				_, rep := x.runSeeded(req, "prod", false, tlsFailure(hostnameFailure(cert, "r.example")), tlsFailure(hostnameFailure(cert, "r.example")),
					func(d *Deletion) {
						if removed {
							d.RemoveEntry("prod")
						}
					})
				n := notRevoked(t, rep, token)
				assert.Equal(t, clierr.CodeTLSTrust, n.Code)
				assert.Contains(t, n.Error, clause)
				assert.Contains(t, n.Error, "the server's certificate is not valid for ")
				require.NotNil(t, n.Certificate)
				assert.Equal(t, tlstrust.KindHostnameMismatch, n.Certificate.Kind)
			})
		}
	}
}

// TestARemovedEntrysTokenGetsTheRecoveryWording asserts a token recorded on
// an entry config delete-server removed is worded for the recovery login, as
// its retry is manual, and one recorded on an entry that stays for the
// next-login wording. Mutation: ignore what the command removed (retry reads
// the entry as kept).
func TestARemovedEntrysTokenGetsTheRecoveryWording(t *testing.T) {
	rows := tlsRevokeRows()
	for _, removed := range []bool{true, false} {
		name, want, retry := "the entry stays", rows[0].caEntry, retryNextLogin
		if removed {
			name, want, retry = "the command removed the entry", rows[1].caEntry, retryManual
		}
		t.Run(name, func(t *testing.T) {
			x, _, token := rows[0].setup(t)
			cert := tlsCert(t, "private-ca")
			_, rep := x.runSeeded(x.req(DeleteForDeleteServer, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)),
				func(d *Deletion) {
					if removed {
						d.RemoveEntry("prod")
					}
				})
			n := notRevoked(t, rep, token)
			assert.Equal(t, retry, n.Retry)
			assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), want)
		})
	}
}

// TestAV3TokenWhoseBothRequestsFailTheCertificateReportsTheMessageOnce
// asserts the delete's certificate failure is the whole error of a v3 token
// revoked with its own bearer, with no logout prefix, and the stamp keeps
// the two-half text of the plain client. Mutation: stamp the reported message.
func TestAV3TokenWhoseBothRequestsFailTheCertificateReportsTheMessageOnce(t *testing.T) {
	x := stubV3Fixture(t, newV3Stub(t, 0, 0))
	cert := tlsCert(t, "private-ca")
	_, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	n := notRevoked(t, rep, stubV3Name)
	assert.Equal(t, "failed", n.Reason)
	assert.Equal(t, clierr.CodeTLSTrust, n.Code)
	assert.True(t, strings.HasPrefix(n.Error, "the server presented a certificate signed by an unknown authority"), n.Error)
	assert.NotContains(t, n.Error, "logout ")
	assert.Equal(t, 1, strings.Count(n.Error, "the server presented"), "the message once")

	recs := x.load().Servers["prod"].SupersededTokens
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].LastAttempt)
	stamp := recs[0].LastAttempt.Error
	assert.True(t, strings.HasPrefix(stamp, "logout Post "), "the stamp keeps the two-half text: %s", stamp)
	assert.Contains(t, stamp, "; delete Delete ")
	assert.Contains(t, stamp, "tls: failed to verify certificate")
	assert.NotContains(t, stamp, "config update-server")
}

// TestAV3LogoutCertificateFailureThenADeleteStatusHasNoCode asserts a logout
// that failed the certificate check, followed by a delete the server
// answered, keeps today's reason and two-half text with no code. Mutation:
// set TLSError from the logout half.
func TestAV3LogoutCertificateFailureThenADeleteStatusHasNoCode(t *testing.T) {
	x := stubV3Fixture(t, newV3Stub(t, 0, 0))
	cert := tlsCert(t, "private-ca")
	_, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), answer{status: http.StatusForbidden}, nil)
	n := notRevoked(t, rep, stubV3Name)
	assert.Equal(t, "failed", n.Reason)
	assert.Empty(t, n.Code)
	assert.Nil(t, n.Certificate)
	assert.True(t, strings.HasPrefix(n.Error, "logout Post "), n.Error)
	assert.True(t, strings.HasSuffix(n.Error, "; delete 403 Forbidden"), n.Error)
}

// TestAV3LogoutUnauthorizedThenADeleteCertificateFailureIsUnconfirmed
// asserts a logout answered 401 followed by a delete that failed the
// certificate check stays unconfirmed with no error and no code, as the 401
// says the bearer no longer authenticates. Mutation: test TLSError before the
// unconfirmed arm in judgeV3.
func TestAV3LogoutUnauthorizedThenADeleteCertificateFailureIsUnconfirmed(t *testing.T) {
	x := stubV3Fixture(t, newV3Stub(t, 0, 0))
	cert := tlsCert(t, "private-ca")
	_, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, answer{status: http.StatusUnauthorized}, tlsFailure(unknownAuthorityFailure(cert)), nil)
	n := notRevoked(t, rep, stubV3Name)
	assert.Equal(t, "unconfirmed", n.Reason)
	assert.Empty(t, n.Code)
	assert.Empty(t, n.Error)
	assert.Nil(t, n.Certificate)
}

// TestTheStampKeepsTheTextOfTheRequestNotTheReportedMessage asserts the
// stamp a certificate failure leaves on a record is the request's own error
// text: no wording for what revokes the token, no remedy marker. Mutation:
// stamp the reported message.
func TestTheStampKeepsTheTextOfTheRequestNotTheReportedMessage(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	old := x.token("alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: *old.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	cert := tlsCert(t, "private-ca")
	cause := unknownAuthorityFailure(cert)
	_, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(cause), tlsFailure(cause), nil)
	assert.Equal(t, clierr.CodeTLSTrust, notRevoked(t, rep, old.Ref.Name).Code)

	var stamp *clierr.RevokeOutcome
	for _, r := range x.load().Servers["prod"].SupersededTokens {
		if r.Ref == *old.Ref {
			stamp = r.LastAttempt
		}
	}
	require.NotNil(t, stamp, "the record of the token is stamped")
	want := (&url.Error{Op: "Delete", URL: kube.BaseURL(x.url) + ExtTokensPath + "/" + old.Ref.Name, Err: cause}).Error()
	assert.Equal(t, want, stamp.Error)
	assert.NotContains(t, stamp.Error, clierr.RemedyMarker)
	assert.NotContains(t, stamp.Error, "update-server")
}

// TestACertificateFailureOfAKeptEntryOpensItsCommandsWithTheMarker asserts the report and
// the text of a not-revoked token whose entry stays (the row that holds a
// command) open the command with this process's marker, which the
// invocation that prints them expands.
func TestACertificateFailureOfAKeptEntryOpensItsCommandsWithTheMarker(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	cert := tlsCert(t, "private-ca")
	d, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), "rancher config update-server 'prod' --cacert <file>")
	assert.Contains(t, n.Error, clierr.Rancher)
	assert.Contains(t, strings.Join(rep.TextDetails(), "\n"), clierr.Rancher)
	assert.Contains(t, strings.Join(d.TextLines(), "\n"), clierr.Rancher)
}

// TestACertificateFailureShowsAfterTheTokenLineInTheSuccessText asserts the
// text of a revoke the command finishes: the token line with the code and the
// recorded-on clause, then the message on its own line, then the
// certificate's lines with the serving certificate label for a chain of one,
// each two spaces deeper than the token line. Mutation: drop the TextDetails
// lines.
func TestACertificateFailureShowsAfterTheTokenLineInTheSuccessText(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	cert := tlsCert(t, "private-ca")
	d, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	reported := notRevoked(t, rep, login.Ref.Name).Error
	assert.Contains(t, clierr.ExpandRemedies(reported, ""), "; save it for entry 'prod' with: rancher config update-server 'prod' --cacert <file>")
	lines := d.TextLines()
	var at int
	for i, l := range lines {
		if strings.Contains(l, login.Ref.Name) && strings.HasPrefix(l, "  not revoked") {
			at = i
		}
	}
	require.NotZero(t, at, "%q", lines)
	require.GreaterOrEqual(t, len(lines), at+5, "the message and the certificate follow the token line: %q", lines)
	assert.Equal(t, "  not revoked "+login.Ref.String()+
		" (failed, TLS_TRUST; recorded on prod: the next login of prod as user-alice revokes it)", lines[at])
	assert.Equal(t, "    "+reported, lines[at+1], "the text prints the message the report carries, worded for the revoke")
	assert.Equal(t, "    serving certificate: CN=private-ca", lines[at+2])
	assert.Equal(t, "    issuer: CN=private-ca", lines[at+3])
	assert.Equal(t, "    fingerprint: "+tlstrust.Fingerprint(cert), lines[at+4])
}

// TestACertificateFailureTextHoldsNoControlCharacters asserts a certificate
// subject holding an escape and a newline prints without them in the text
// of a revoke. Mutation: drop output.Printable at the certificate lines.
func TestACertificateFailureTextHoldsNoControlCharacters(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	cert := tlsCert(t, "evil\x1b[31m\nfake line: forged")
	d, _ := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	text := strings.Join(d.TextLines(), "\n")
	assert.Contains(t, text, "serving certificate: ")
	assert.NotContains(t, text, "\x1b")
	for _, l := range d.TextLines() {
		assert.NotContains(t, l, "\n", "a certificate field never starts a line of its own")
	}
	assert.Contains(t, text, "evil[31mfake line: forged")
}

// TestTheWarningNamesTheCodeAndAddsTheCATailForAnUnknownAuthorityOnly asserts
// the warning of a manual token that failed the certificate check says
// "failed, TLS_TRUST" rather than the message, ends with the --cacert clause
// for an unknown authority, and not for a hostname mismatch or a missing
// certificate. Mutations: use reasonText unchanged; drop the clause; add it
// for every kind; read the certificate's kind without the nil check.
func TestTheWarningNamesTheCodeAndAddsTheCATailForAnUnknownAuthorityOnly(t *testing.T) {
	const tail = "; add --cacert <file with the server's CA certificate> to the login, or set RANCHER_CLI_CACERT to that file"
	warningOf := func(t *testing.T, cause error) string {
		t.Helper()
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "bob")
		target := x.token("bob")
		x.cache("k", target, x.own("prod").WithCreatedToken(target.Ref, "user-other"), false)
		d, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(cause), tlsFailure(cause), nil)
		require.Equal(t, retryManual, notRevoked(t, rep, target.Ref.Name).Retry)
		for _, w := range d.Warnings() {
			if strings.Contains(w, target.Ref.Name) {
				return w
			}
		}
		require.Failf(t, "no warning", "%q", d.Warnings())
		return ""
	}
	t.Run("unknown authority", func(t *testing.T) {
		w := warningOf(t, unknownAuthorityFailure(tlsCert(t, "private-ca")))
		assert.Contains(t, w, "was not revoked (failed, TLS_TRUST); the sequence assumes RANCHER_CLI_URL")
		assert.NotContains(t, w, "the server presented")
		assert.True(t, strings.HasSuffix(w, tail), w)
	})
	t.Run("hostname mismatch", func(t *testing.T) {
		w := warningOf(t, hostnameFailure(tlsCert(t, "other", "other.example"), "r.example"))
		assert.Contains(t, w, "was not revoked (failed, TLS_TRUST); the sequence assumes RANCHER_CLI_URL")
		assert.NotContains(t, w, "--cacert")
	})
	t.Run("no certificate details", func(t *testing.T) {
		d := &Deletion{cfg: &config.Config{}, view: &config.Config{}, removed: map[string]bool{}}
		it := &revocationItem{source: sourceCache, ref: &TokenRef{Kind: TokenKindExt, Name: "token-a"}, server: "https://r.example",
			reason: reasonFailed, errText: "x", tlsErr: clierr.New(clierr.CodeTLSTrust, "m")}
		var w string
		require.NotPanics(t, func() { w = d.manualWarning(it) })
		assert.Contains(t, w, "was not revoked (failed, TLS_TRUST)")
		assert.NotContains(t, w, "--cacert <file with")
	})
}

// TestARevokeThroughTheClientOfADeletionWrapsACertificateFailure asserts a
// revoke through the client Deletion builds, to a server whose certificate the
// entry's CA does not verify, reports TLS_TRUST with the unknown-authority
// message for the entry's CA and the certificate's fingerprint, and leaves
// the token live. Mutation: drop WrapTransport in newClient.
func TestARevokeThroughTheClientOfADeletionWrapsACertificateFailure(t *testing.T) {
	x, _ := newTLSRevFixture(t)
	login := x.login("prod", "alice")
	x.withCA("prod", selfSignedCAPEM(t), false)

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.True(t, x.alive(login), "the token was not reached")
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Equal(t, "failed", n.Reason)
	assert.Equal(t, clierr.CodeTLSTrust, n.Code)
	assert.True(t, strings.HasPrefix(n.Error, "the server presented a certificate signed by an unknown authority"), n.Error)
	assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), "; save it for entry 'prod' with: rancher config update-server 'prod' --cacert <file>")
	require.NotNil(t, n.Certificate)
	assert.NotEmpty(t, n.Certificate.Fingerprint)
	assert.Equal(t, tlstrust.KindUnknownAuthority, n.Certificate.Kind)
}

// TestAMarkedEntryWithTheVariableUnsetIsWordedAsAnEntryWithNoCA asserts an
// entry marked as connected with RANCHER_CLI_CACERT, run with the variable
// unset or naming another server, verifies against the system roots and says
// "save it for entry", not that the variable supplied the CA; with the
// variable set it says the variable did. Mutation: fromVariable =
// s.CAFromEnvironment.
func TestAMarkedEntryWithTheVariableUnsetIsWordedAsAnEntryWithNoCA(t *testing.T) {
	for _, tc := range []struct {
		name       string
		variable   string
		rancherURL string
		fromEnv    bool
	}{
		{name: "the variable unset"},
		{name: "the variable for another server", variable: "wrong", rancherURL: "https://other.example"},
		{name: "the variable supplied the CA", variable: "wrong", fromEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, _ := newTLSRevFixture(t)
			login := x.login("prod", "alice")
			x.withCA("prod", "", true)
			value := ""
			if tc.variable != "" {
				value = selfSignedCAPEM(t)
			}
			t.Setenv(config.EnvCACert, value)
			t.Setenv(config.EnvURL, tc.rancherURL)
			if tc.fromEnv {
				t.Setenv(config.EnvURL, x.url)
			}
			_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
			require.NoError(t, err)
			n := notRevoked(t, rep, login.Ref.Name)
			assert.Equal(t, clierr.CodeTLSTrust, n.Code)
			if tc.fromEnv {
				assert.Contains(t, n.Error, "the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA")
				return
			}
			assert.Contains(t, n.Error, "; save it for entry 'prod' with: ")
			assert.NotContains(t, n.Error, "RANCHER_CLI_CACERT")
		})
	}
}

// TestNoCodeForAClosedServerOrAnUnreadableCA asserts a transport failure that
// is not a certificate failure (a closed server) and a CA that cannot be read
// report reason failed with no code and no certificate. Mutation: set the
// code on every default arm.
func TestNoCodeForAClosedServerOrAnUnreadableCA(t *testing.T) {
	t.Run("a closed server", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		login := x.login("prod", "alice")
		x.srv.Close()
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		n := notRevoked(t, rep, login.Ref.Name)
		assert.Equal(t, "failed", n.Reason)
		assert.Empty(t, n.Code)
		assert.Nil(t, n.Certificate)
		assert.NotEmpty(t, n.Error)
	})
	t.Run("an unreadable CA", func(t *testing.T) {
		x, _ := newTLSRevFixture(t)
		login := x.login("prod", "alice")
		x.withCA("prod", "", true)
		t.Setenv(config.EnvCACert, "/nonexistent/ca.pem")
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		n := notRevoked(t, rep, login.Ref.Name)
		assert.Equal(t, "failed", n.Reason)
		assert.Empty(t, n.Code)
		assert.Nil(t, n.Certificate)
		assert.Contains(t, n.Error, "reading RANCHER_CLI_CACERT: ")
	})
}

// TestAnUpdateServerListCarriesNoCode asserts the tokens config
// update-server lists, which sends no request, have no code or certificate
// in their JSON. It is a shape check.
func TestAnUpdateServerListCarriesNoCode(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	d, err := x.localDelete(x.req(DeleteForUpdateServer, "prod"))
	require.NoError(t, err)
	rep := d.Report()
	n := notRevoked(t, rep, login.Ref.Name)
	b, err := json.Marshal(n)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.NotContains(t, got, "code")
	assert.NotContains(t, got, "certificate")
}

// TestATLSErrorOfARevokeOutcomeIsNeverSerialized asserts an outcome holding
// the classified error marshals to JSON, and into the config.yaml a record
// stamp is written to, without it. Mutation: drop json:"-" from TLSError.
func TestATLSErrorOfARevokeOutcomeIsNeverSerialized(t *testing.T) {
	out := RevokeOutcome{Result: ResultFailed, Error: "x", TLSError: clierr.New(clierr.CodeTLSTrust, "the classified message")}
	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(b)), "tlserror")
	assert.NotContains(t, string(b), "classified")

	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {
		URL: "https://r.example",
		SupersededTokens: []config.SupersededToken{{Ref: TokenRef{Kind: TokenKindExt, Name: "token-a"}, UserID: "u-1",
			SupersededAt: "2026-10-01T00:00:00Z", LastAttempt: &out}},
	}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	raw, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "lastAttempt", "the stamp is written")
	assert.NotContains(t, strings.ToLower(string(raw)), "tlserror")
	assert.NotContains(t, string(raw), "classified")
}

// certificateFailureToken is a not-revoked token with a certificate failure,
// for the text forms of the failure details.
func certificateFailureToken(t *testing.T) NotRevokedToken {
	t.Helper()
	cert := tlsCert(t, "private-ca")
	ce := tlstrust.Classify(unknownAuthorityFailure(cert), "r.example", time.Now(), tlstrust.Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(tlstrust.Details)
	return NotRevokedToken{
		Ref: &TokenRef{Kind: TokenKindExt, Name: "token-a"}, Source: sourceCache, Server: "https://r.example", Entry: "prod",
		Reason: "failed", Code: clierr.CodeTLSTrust, Error: ce.Message, Certificate: &details, Retry: retryNextLogin,
	}
}

// TestTheDetailLinesOfACertificateFailureFollowTheTokenLine asserts the
// failure details' text form of a not-revoked token: the token line with the
// code, then the message and the certificate's lines two spaces deeper, and
// for a token with no code the token line alone. Mutation: keep TextLine in
// DetailLines.
func TestTheDetailLinesOfACertificateFailureFollowTheTokenLine(t *testing.T) {
	n := certificateFailureToken(t)
	lines := n.DetailLines()
	require.Len(t, lines, 6, "%q", lines)
	assert.Equal(t, "not revoked: ext/token-a on https://r.example (failed, TLS_TRUST, retry nextLogin)", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "  the server presented a certificate signed by an unknown authority"), lines[1])
	assert.Equal(t, "  serving certificate: CN=private-ca", lines[2])
	assert.Equal(t, "  fingerprint: "+n.Certificate.Fingerprint, lines[4])

	plain := NotRevokedToken{Ref: n.Ref, Source: n.Source, Server: n.Server, Reason: "failed", Error: "boom", Retry: retryManual}
	assert.Equal(t, []string{"not revoked: ext/token-a on https://r.example (failed, retry manual)"}, plain.DetailLines())
}

// TestARevocationReportsDetailsPrintTheCertificateFailure asserts
// RevocationReport's text details, which config delete-server's failures
// carry, print the message and the fingerprint under the token. Mutation:
// keep TextLine in RevocationReport.TextDetails.
func TestARevocationReportsDetailsPrintTheCertificateFailure(t *testing.T) {
	n := certificateFailureToken(t)
	text := strings.Join(RevocationReport{NotRevoked: []NotRevokedToken{n}}.TextDetails(), "\n")
	assert.Contains(t, text, "the server presented a certificate signed by an unknown authority")
	assert.Contains(t, text, "fingerprint: "+n.Certificate.Fingerprint)
}

// TestAuthDeleteTokensFailureDetailsPrintTheCertificateFailure asserts the
// text details of auth delete-token's failure print the message and the
// fingerprint under the token. Mutation: keep TextLine in
// DeleteTokenFailure.TextDetails.
func TestAuthDeleteTokensFailureDetailsPrintTheCertificateFailure(t *testing.T) {
	n := certificateFailureToken(t)
	text := strings.Join(DeleteTokenFailure{Deleted: []string{"k"}, NotRevoked: []NotRevokedToken{n}}.TextDetails(), "\n")
	assert.Contains(t, text, "deleted: k")
	assert.Contains(t, text, "the server presented a certificate signed by an unknown authority")
	assert.Contains(t, text, "fingerprint: "+n.Certificate.Fingerprint)
}

// TestTheSignInFailureCarriesTheCertificateFacts asserts withSignInFate puts
// the certificate of a TLS_TRUST cause in primary.details, prints it under
// the primary error in the text form, and gives a failure with no
// certificate (an owner mismatch, a cancelled wait) no primary.details at
// all, in JSON and in text. Mutations: keep primary.Details nil; assign
// without the nil check.
func TestTheSignInFailureCarriesTheCertificateFacts(t *testing.T) {
	si := clusterSignIn{Cluster: CreateResult{UserID: "u-1", ExpiresAt: "2026-10-12T12:00:00Z"}, Login: &clierr.TokenFate{Outcome: CreateKnown,
		Ref: &TokenRef{Kind: TokenKindV3, Name: "t"}, Cleanup: &RevokeOutcome{Result: ResultRevoked}}}
	fate := &clierr.TokenFate{Outcome: CreateKnown, Kind: TokenKindExt, Ref: &TokenRef{Kind: TokenKindExt, Name: "token-abc"},
		Cleanup: &RevokeOutcome{Result: ResultRevoked}}

	t.Run("a certificate failure", func(t *testing.T) {
		cert := tlsCert(t, "private-ca")
		cause := tlstrust.Classify(unknownAuthorityFailure(cert), "r.example", time.Now(), tlstrust.Remedy{})
		require.NotNil(t, cause)
		outer := clierr.New(clierr.CodeTLSTrust, "checking the signed-in user against --user-id 'u': "+cause.Message).
			WithCause(cause).WithDetails(directWaitDetails{Fate: fate})
		_, d := failureOf(t, si.withSignInFate(outer, nil))
		got, ok := d.Primary.Details.(tlstrust.Details)
		require.True(t, ok, "%T", d.Primary.Details)
		assert.Equal(t, tlstrust.Fingerprint(cert), got.Fingerprint)
		require.NotNil(t, d.ClusterToken, "the cluster token's fate stays")

		b, err := json.Marshal(d)
		require.NoError(t, err)
		var doc struct {
			Primary map[string]any `json:"primary"`
		}
		require.NoError(t, json.Unmarshal(b, &doc))
		details, _ := doc.Primary["details"].(map[string]any)
		assert.Equal(t, tlstrust.Fingerprint(cert), details["fingerprint"])
		assert.Contains(t, strings.Join(d.TextDetails(), "\n"), "fingerprint: "+tlstrust.Fingerprint(cert))
	})
	for name, err := range map[string]error{
		"an owner mismatch": clierr.New(clierr.CodeAuth, "the sign-in was made as another user").WithDetails(directWaitDetails{Fate: fate}),
		"a cancelled wait":  clierr.New(clierr.CodeGeneric, "cancelled").WithDetails(directWaitDetails{Fate: fate}),
		"no details":        clierr.New(clierr.CodeGeneric, "cancelled"),
	} {
		t.Run(name, func(t *testing.T) {
			_, d := failureOf(t, si.withSignInFate(err, nil))
			assert.Nil(t, d.Primary.Details)
			b, merr := json.Marshal(d)
			require.NoError(t, merr)
			var doc struct {
				Primary map[string]any `json:"primary"`
			}
			require.NoError(t, json.Unmarshal(b, &doc))
			assert.NotContains(t, doc.Primary, "details")
			require.NotPanics(t, func() { _ = d.TextDetails() })
		})
	}
}

// TestTheRealClientKeepsTheHostAndPortOfItsEntry asserts the client Deletion
// builds keeps its entry's host and port, and a hostname mismatch through it
// names them: the message reads "not valid for localhost:<port>". The server's
// certificate names 127.0.0.1 and example.com, so an entry that names the
// server as localhost fails the hostname check against the CA it saves.
// Mutation: leave hostPort empty.
func TestTheRealClientKeepsTheHostAndPortOfItsEntry(t *testing.T) {
	x, ca := newTLSRevFixture(t)
	login := x.login("prod", "alice")
	u, err := url.Parse(x.url)
	require.NoError(t, err)
	hostPort := "localhost:" + u.Port()
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.URL, s.CA = "https://"+hostPort, ca
		cfg.Servers["prod"] = s
	})

	d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, hostPort, d.clients["prod"].hostPort)
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Equal(t, clierr.CodeTLSTrust, n.Code)
	assert.Contains(t, n.Error, "the server's certificate is not valid for "+hostPort+"; check that the server URL of entry 'prod' matches")
	require.NotNil(t, n.Certificate)
	assert.Equal(t, tlstrust.KindHostnameMismatch, n.Certificate.Kind)
}

// TestTheURLClauseNamesTheTrustEntryAndTheCAClauseTheRecordedEntry asserts
// that for a token whose trust entry differs from the entry it is recorded
// on, the URL clause names the trust entry (the one whose URL the request
// used) and the next-login CA clause the recorded entry (the one whose login
// revokes the token). Mutation: build the URL clause from the recorded entry.
func TestTheURLClauseNamesTheTrustEntryAndTheCAClauseTheRecordedEntry(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.Server{
		"dev":  {URL: "https://r.example", TokenUserID: "u-1"},
		"prod": {URL: "https://r.example", TokenUserID: "u-1"},
	}}
	d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}, clients: map[string]revokeClient{"prod": {}}}
	it := &revocationItem{source: sourceCache, ref: &TokenRef{Kind: TokenKindExt, Name: "token-a"}, server: config.CanonicalServerIdentity("https://r.example"),
		owner: "u-1", entry: "dev", trust: "prod", reason: reasonFailed}
	require.Equal(t, retryNextLogin, d.retry(it))
	r := d.revokeTLSRemedy(it)
	assert.Equal(t, "the server URL of entry 'prod'", r.URL)
	assert.Equal(t, "; save it for entry 'dev' with: "+clierr.Rancher+" config update-server 'dev' --cacert <file>", r.CA)
}

// nextLoginWarning is the warning of entry's tokens a next login revokes.
func nextLoginWarning(t *testing.T, d *Deletion, entry string) string {
	t.Helper()
	for _, w := range d.Warnings() {
		if strings.Contains(w, "recorded on "+entry+" are revoked by") {
			return w
		}
	}
	require.Failf(t, "no warning", "%q", d.Warnings())
	return ""
}

// TestTheNextLoginWarningSaysToSaveTheCAFirstForAnUnknownAuthority asserts the
// warning of an entry's tokens a next login revokes says to save the server's
// CA first, with the command, when a revoke failed on an unknown authority
// the CA of which did not come from RANCHER_CLI_CACERT: "its revoke" for one
// token, "their revokes" when all of several failed, "the revoke of 1 of
// them" or "the revokes of K of them" when some did. It adds nothing for an
// expired or hostname-mismatch certificate or when the variable supplied the
// CA. Mutations: drop the clause; add it for every kind; word all of several
// as K of them; word one of several in the plural; change the text of K
// of several.
func TestTheNextLoginWarningSaysToSaveTheCAFirstForAnUnknownAuthority(t *testing.T) {
	unknown := unknownAuthorityFailure(tlsCert(t, "private-ca"))
	expired := &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Cert: tlsCert(t, "old"), Reason: x509.Expired},
		UnverifiedCertificates: []*x509.Certificate{tlsCert(t, "old")}}
	mismatch := hostnameFailure(tlsCert(t, "other", "other.example"), "r.example")

	oneToken := func(t *testing.T, cause error, fromVariable bool) string {
		t.Helper()
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		d, _ := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", fromVariable, tlsFailure(cause), tlsFailure(cause), nil)
		return nextLoginWarning(t, d, "prod")
	}
	t.Run("one token, an unknown authority", func(t *testing.T) {
		w := oneToken(t, unknown, false)
		assert.Contains(t, w, "1 token(s) recorded on prod are revoked by the next login of prod as user-alice; run it with: "+clierr.Rancher+" auth login 'prod'")
		assert.True(t, strings.HasSuffix(w, "; its revoke failed on the server's certificate; first save the server's CA with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>"), w)
	})
	t.Run("an expired certificate", func(t *testing.T) {
		assert.NotContains(t, oneToken(t, expired, false), "first save")
	})
	t.Run("a hostname mismatch", func(t *testing.T) {
		assert.NotContains(t, oneToken(t, mismatch, false), "first save")
	})
	t.Run("the variable supplied the CA", func(t *testing.T) {
		assert.NotContains(t, oneToken(t, unknown, true), "first save")
	})
	t.Run("several tokens, K of them failed on the certificate", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("prod-c-1", ct, x.own("prod"), true)
		d, _ := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknown), tlsFailure(unknown), nil)
		// Both revokes fail on the certificate: all of them.
		w := nextLoginWarning(t, d, "prod")
		assert.Contains(t, w, "2 token(s) recorded on prod")
		assert.Contains(t, w, "; their revokes failed on the server's certificate; first save the server's CA with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>")

		x2 := newRevFixture(t, testrancher.Options{})
		x2.login("prod", "alice")
		ct2 := x2.token("alice")
		x2.cache("prod-c-1", ct2, x2.own("prod"), true)
		dd, err := x2.localDelete(x2.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		x2.seedClientFunc(dd, "prod", false, func(r *http.Request) answer {
			if strings.HasSuffix(r.URL.Path, "/"+ct2.Ref.Name) {
				return answer{err: unknown}
			}
			return answer{status: http.StatusInternalServerError}
		})
		dd.RevokeTokens(t.Context())
		w = nextLoginWarning(t, dd, "prod")
		assert.Contains(t, w, "2 token(s) recorded on prod")
		assert.True(t, strings.HasSuffix(w, "; the revoke of 1 of them failed on the server's certificate; first save the server's CA with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>"), w)

		x3 := newRevFixture(t, testrancher.Options{})
		x3.login("prod", "alice")
		ca, cb := x3.token("alice"), x3.token("alice")
		x3.cache("prod-c-1", ca, x3.own("prod"), true)
		x3.cache("prod-c-2", cb, x3.own("prod"), true)
		d3, err := x3.localDelete(x3.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		x3.seedClientFunc(d3, "prod", false, func(r *http.Request) answer {
			if strings.HasSuffix(r.URL.Path, "/"+ca.Ref.Name) || strings.HasSuffix(r.URL.Path, "/"+cb.Ref.Name) {
				return answer{err: unknown}
			}
			return answer{status: http.StatusInternalServerError}
		})
		d3.RevokeTokens(t.Context())
		w = nextLoginWarning(t, d3, "prod")
		assert.Contains(t, w, "3 token(s) recorded on prod")
		assert.True(t, strings.HasSuffix(w, "; the revokes of 2 of them failed on the server's certificate; first save the server's CA with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>"), w)
	})
}

// TestTheNextLoginWarningOfAnExecLoginEntryFollowsTheSignInText asserts the
// same clause follows the text of an entry the kubectl sign-in revokes, which
// has no login command of its own.
func TestTheNextLoginWarningOfAnExecLoginEntryFollowsTheSignInText(t *testing.T) {
	const name = "exec-login-r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{name: {URL: "https://r.example.com", TokenUserID: "u-1", Provenance: clierr.ProvenanceExecLogin}}}
	d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}, clients: map[string]revokeClient{name: {}}}
	d.items = []*revocationItem{{source: sourceCache, ref: &TokenRef{Kind: TokenKindExt, Name: "token-a"}, server: config.CanonicalServerIdentity("https://r.example.com"),
		owner: "u-1", entry: name, trust: name, reason: reasonFailed, tlsErr: clierr.New(clierr.CodeTLSTrust, "m"),
		cert: &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority}}}
	w := nextLoginWarning(t, d, name)
	assert.Equal(t, "1 token(s) recorded on "+name+" are revoked by the next kubectl sign-in through "+name+" as u-1"+
		"; its revoke failed on the server's certificate; first save the server's CA with: "+clierr.Rancher+" config update-server '"+name+"' --cacert <file>", w)
}

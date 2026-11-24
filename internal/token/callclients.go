package token

import (
	"errors"
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// callClients builds the clients of one get-token call. A stored login
// goes only over the client that the verified method returns, which
// verifies the server, except for a plugin login entry that has no saved CA
// and whose login came from a sign-in that did not verify the server, when
// the call passes --skip-verify (storedCredToken). signInClient is the
// client of a new sign-in, the one client in the package that may skip
// verification. A client is built when first needed, and the call's own
// clients are built once.
type callClients struct {
	clientFor  func(httpx.Options) (*http.Client, error)
	server     string
	caPEM      string
	caFrom     config.CASource
	shim       bool
	skipVerify bool

	callVerified *http.Client
	unverified   *http.Client
}

// callClients returns the clients of a call to server that trusts caPEM,
// which came from caFrom.
func (t tokenCmd) callClients(server, caPEM string, caFrom config.CASource, params runParams) *callClients {
	return &callClients{clientFor: t.clientFor, server: server, caPEM: caPEM, caFrom: caFrom, shim: params.legacyShim, skipVerify: params.SkipVerify}
}

// callRemedy gives the wording of a certificate failure of the call's own
// connection.
func (c *callClients) callRemedy() tlstrust.Remedy {
	return tlsRemedyFor(c.caFrom, c.shim)
}

// verified is a client that verifies the server with the call's CA, or the
// system roots when the call has none, whatever --skip-verify says. remedy
// gives the wording of its certificate failures.
func (c *callClients) verified(remedy tlstrust.Remedy) (*http.Client, error) {
	own := remedy == c.callRemedy()
	if own && c.callVerified != nil {
		return c.callVerified, nil
	}
	client, err := c.clientFor(httpx.Options{
		CACert:        c.caPEM,
		Timeout:       httpx.DefaultTimeout,
		WrapTransport: tlstrust.WrapTransportFor(c.server, remedy),
	})
	if err != nil {
		return nil, err
	}
	if own {
		c.callVerified = client
	}
	return client, nil
}

// signInClient is the client of a new sign-in: for a call that passes
// --skip-verify one that does not verify the server, and the call's
// verified client otherwise.
func (c *callClients) signInClient() (*http.Client, error) {
	if !c.skipVerify {
		return c.verified(c.callRemedy())
	}
	if c.unverified != nil {
		return c.unverified, nil
	}
	client, err := c.clientFor(httpx.Options{
		Insecure:      true,
		Timeout:       httpx.DefaultTimeout,
		WrapTransport: tlstrust.WrapTransportFor(c.server, c.callRemedy()),
	})
	if err != nil {
		return nil, err
	}
	c.unverified = client
	return client, nil
}

// storedLoginRefusal is, for a call that passes --skip-verify, the lead of
// the refusal of sc's stored login when the server's certificate does not
// verify, and the remedy that refusal names. The remedy is the call's own,
// then the command that saves the CA on the server entry whose CA the call
// reads: the entry the CA came from, else caEntry, the entry whose CA the
// call would read (caForServer), else sc's entry. A CA from --cacerts or
// from RANCHER_CLI_CACERT overrides the entry's. For --cacerts the remedy
// says to remove the flag, and to unset the variable as well when it is set;
// for the variable it says that the variable overrides the CA saved on the
// entry. A plugin login entry without SignedInWithSkipVerify also gets the
// command that deletes it, then the next kubectl call signs in with
// --skip-verify.
func (c *callClients) storedLoginRefusal(sc storedCredential, caEntry string) (lead string, remedy tlstrust.Remedy) {
	entry := sc.name
	switch {
	case c.caFrom.Kind == config.CASourceEntry && c.caFrom.Entry != "":
		entry = c.caFrom.Entry
	case caEntry != "":
		entry = caEntry
	}
	remedy = c.callRemedy()
	quoted := clierr.ShellQuote(entry)
	save := clierr.NewCommand("config update-server").Arg(entry).FlagText(config.CAFileFlag)
	meaning := "; " + config.CAFileMeaning
	switch {
	case !save.OK():
		// The whole clause is left out, with the note that says so.
		remedy.CA = clierr.Text(remedy.CA, clierr.Lead(" with: "), save)
	case c.caFrom.Kind == config.CASourceVariable:
		// The variable's remedy already says what <file> is.
		remedy.CA = clierr.Text(remedy.CA, "; or save the CA on server entry "+quoted+
			" (while "+config.EnvCACert+" is set, it overrides a CA saved on a server entry)", clierr.Lead(" with: "), save)
	case c.caFrom.Kind == config.CASourceFlag && config.EnvCACertFor(c.server) != "":
		remedy.CA = clierr.Text(remedy.CA, ", or remove "+caFlagName(c.shim)+" from the command, unset "+config.EnvCACert+
			" and save the CA on server entry "+quoted, clierr.Lead(" with: "), save, meaning)
	case c.caFrom.Kind == config.CASourceFlag:
		remedy.CA = clierr.Text(remedy.CA, ", or remove "+caFlagName(c.shim)+" from the command and save the CA on server entry "+quoted,
			clierr.Lead(" with: "), save, meaning)
	case c.caFrom.Kind == config.CASourceEntry:
		remedy.CA = clierr.Text(remedy.CA, ", or replace the CA saved on server entry "+quoted, clierr.Lead(" with: "), save, meaning)
	default:
		remedy.CA = clierr.Text(remedy.CA, ", or save the CA on server entry "+quoted, clierr.Lead(" with: "), save, meaning)
	}
	if sc.entry.Provenance == clierr.ProvenanceExecLogin && !sc.entry.SignedInWithSkipVerify {
		remove := clierr.NewCommand(config.DeleteServerWords).Arg(sc.name)
		if remove.OK() {
			remedy.CA = clierr.Text(remedy.CA, "; or delete server entry "+clierr.ShellQuote(sc.name), clierr.Lead(" with: "), remove,
				"; then run kubectl again to sign in with --skip-verify")
		} else {
			remedy.CA = clierr.Text(remedy.CA, clierr.Lead(" with: "), remove)
		}
	}
	lead = "the stored login of server entry " + clierr.ShellQuote(sc.name) +
		" is sent only over a connection that verifies the server, and --skip-verify does not change that; "
	return lead, remedy
}

// withLead is err with lead in front of its message when err holds a
// TLS_TRUST error, which keeps its code and details; any other err is
// returned as it is.
func withLead(err error, lead string) error {
	if lead == "" {
		return err
	}
	ce := tlstrust.TypedError(err)
	if ce == nil {
		return err
	}
	return ce.WithMessage(lead + ce.Message)
}

// rejectedOverVerified is a rejection of a stored login that was sent over
// a client that verified the server: errStoredCredentialRejected or a
// *pluginCredentialRejected, both still reachable through errors.Is and
// errors.As. The sign-in that follows in the same call verifies the server
// too: the call has just verified it.
type rejectedOverVerified struct {
	err error
}

func (e *rejectedOverVerified) Error() string { return e.err.Error() }

func (e *rejectedOverVerified) Unwrap() error { return e.err }

// markRejection is err marked as a rejection over a verified client when
// verified is true and err is a rejection; any other err is returned as it
// is.
func markRejection(err error, verified bool) error {
	if !verified {
		return err
	}
	if _, plugin := errors.AsType[*pluginCredentialRejected](err); plugin || errors.Is(err, errStoredCredentialRejected) {
		return &rejectedOverVerified{err: err}
	}
	return err
}

// isRejectedOverVerified reports whether err is a rejection of a stored
// login that was sent over a verified client.
func isRejectedOverVerified(err error) bool {
	_, ok := errors.AsType[*rejectedOverVerified](err)
	return ok
}

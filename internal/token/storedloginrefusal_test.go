package token

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestStoredLoginRefusalNamesTheEntryAndTheRemedyOfEachCASource asserts, in
// full, the lead and the remedy that storedLoginRefusal writes for a call
// with --skip-verify: one text for each source of the call's CA (the
// variable, the flag with and without the variable set for the server, a
// server entry, none), the entry each text names when the refused entry, the
// entry the CA came from and the entry the call reads differ, and the
// command that deletes a plugin login entry whose login did not come from a
// sign-in that skipped verification.
func TestStoredLoginRefusalNamesTheEntryAndTheRemedyOfEachCASource(t *testing.T) {
	const (
		server  = "https://r.example.com"
		refused = "prod"
		fromCA  = "staging"
		read    = "other"

		wantLead     = "the stored login of server entry 'prod' is sent only over a connection that verifies the server, and --skip-verify does not change that; "
		flagBase     = " and pass it with --cacerts"
		variableBase = "; the CA came from RANCHER_CLI_CACERT - pass --cacerts <file>, or unset RANCHER_CLI_CACERT, or set it to the server's CA; " +
			"<file> is a file that holds the server's CA certificate"
		meaning      = "; <file> is a file that holds the server's CA certificate"
		deleteClause = "; or delete server entry 'prod' with: rancher config delete-server 'prod'; then run kubectl again to sign in with --skip-verify"
	)
	save := func(entry string) string {
		return "rancher config update-server " + clierr.ShellQuote(entry) + " --cacert <file>"
	}

	for _, c := range []struct {
		name string
		// variable is RANCHER_CLI_CACERT and url RANCHER_CLI_URL, set for
		// the row.
		variable, url string
		from          config.CASource
		caEntry       string
		want          string
	}{
		{
			name: "the CA from the variable", from: config.CASource{Kind: config.CASourceVariable}, caEntry: read, variable: "/ca.pem",
			want: variableBase + "; or save the CA on server entry 'other' (while RANCHER_CLI_CACERT is set, it overrides a CA saved on a server entry) with: " +
				save(read),
		},
		{
			name: "the CA from the variable, no entry read", from: config.CASource{Kind: config.CASourceVariable}, variable: "/ca.pem",
			want: variableBase + "; or save the CA on server entry 'prod' (while RANCHER_CLI_CACERT is set, it overrides a CA saved on a server entry) with: " +
				save(refused),
		},
		{
			name: "the CA from the flag, the variable set for this server", from: config.CASource{Kind: config.CASourceFlag}, caEntry: read,
			variable: "/ca.pem",
			want: flagBase + ", or remove --cacerts from the command, unset RANCHER_CLI_CACERT and save the CA on server entry 'other' with: " +
				save(read) + meaning,
		},
		{
			name: "the CA from the flag, the variable set for another server", from: config.CASource{Kind: config.CASourceFlag}, caEntry: read,
			variable: "/ca.pem", url: "https://another.example.com",
			want: flagBase + ", or remove --cacerts from the command and save the CA on server entry 'other' with: " + save(read) + meaning,
		},
		{
			name: "the CA from the flag, no variable", from: config.CASource{Kind: config.CASourceFlag}, caEntry: read,
			want: flagBase + ", or remove --cacerts from the command and save the CA on server entry 'other' with: " + save(read) + meaning,
		},
		{
			name: "the CA from the flag, no entry read", from: config.CASource{Kind: config.CASourceFlag},
			want: flagBase + ", or remove --cacerts from the command and save the CA on server entry 'prod' with: " + save(refused) + meaning,
		},
		{
			name: "the CA from an entry and another entry read, the entry the CA came from is named", from: config.CASource{Kind: config.CASourceEntry, Entry: fromCA}, caEntry: read,
			want: flagBase + ", or replace the CA saved on server entry 'staging' with: " + save(fromCA) + meaning,
		},
		{
			name: "the CA from an entry, no entry read", from: config.CASource{Kind: config.CASourceEntry, Entry: fromCA},
			want: flagBase + ", or replace the CA saved on server entry 'staging' with: " + save(fromCA) + meaning,
		},
		{
			name: "the CA from an entry with no name, the entry the call reads", from: config.CASource{Kind: config.CASourceEntry}, caEntry: read,
			want: flagBase + ", or replace the CA saved on server entry 'other' with: " + save(read) + meaning,
		},
		{
			name: "no CA, the entry the call reads has none", caEntry: read,
			want: flagBase + ", or save the CA on server entry 'other' with: " + save(read) + meaning,
		},
		{
			name: "no CA, no entry read",
			want: flagBase + ", or save the CA on server entry 'prod' with: " + save(refused) + meaning,
		},
	} {
		for _, entry := range []struct {
			name   string
			server config.Server
			delete bool
		}{
			{"a user entry", config.Server{URL: server, Provenance: clierr.ProvenanceDerived}, false},
			{"a plugin login entry", config.Server{URL: server, Provenance: clierr.ProvenanceExecLogin}, true},
			{"a plugin login entry whose login came from a sign-in that skipped verification", config.Server{URL: server, Provenance: clierr.ProvenanceExecLogin, SignedInWithSkipVerify: true}, false},
		} {
			t.Run(c.name+", "+entry.name, func(t *testing.T) {
				t.Setenv(config.EnvCACert, c.variable)
				t.Setenv(config.EnvURL, c.url)
				clients := &callClients{server: server, caFrom: c.from, shim: true, skipVerify: true}
				lead, remedy := clients.storedLoginRefusal(storedCredential{name: refused, entry: entry.server}, c.caEntry)
				want := c.want
				if entry.delete {
					want += deleteClause
				}
				assert.Equal(t, wantLead, lead)
				assert.Equal(t, want, clierr.ExpandRemedies(remedy.CA, ""))
			})
		}
	}
}

// TestStoredLoginRefusalLeavesOutTheClauseOfAnEntryNameThatCannotBeQuoted
// asserts that, when the name of the entry a command would name holds a
// single quote, which no printed command can carry exactly, the whole clause
// of that entry is left out - its prose, its command and, for the delete
// clause, the step that follows - and only the note that says so is added.
func TestStoredLoginRefusalLeavesOutTheClauseOfAnEntryNameThatCannotBeQuoted(t *testing.T) {
	const (
		server     = "https://r.example.com"
		unquotable = "it's"
		flagTxt    = " and pass it with --cacerts"
		varTxt     = "; the CA came from RANCHER_CLI_CACERT - pass --cacerts <file>, or unset RANCHER_CLI_CACERT, or set it to the server's CA; " +
			"<file> is a file that holds the server's CA certificate"
		meaning = "; <file> is a file that holds the server's CA certificate"
		note    = "; " + clierr.LeftOutNote
	)
	saveOther := "rancher config update-server 'other' --cacert <file>"
	for _, c := range []struct {
		name    string
		refused string
		from    config.CASource
		caEntry string
		plugin  bool
		// variable is RANCHER_CLI_CACERT, set for the row.
		variable string
		want     string
	}{
		{name: "no CA, the entry the call reads", refused: "prod", caEntry: unquotable, want: flagTxt + note},
		{name: "no CA, no entry read, the refused entry", refused: unquotable, want: flagTxt + note},
		{name: "the CA from the variable", refused: "prod", caEntry: unquotable, from: config.CASource{Kind: config.CASourceVariable}, want: varTxt + note},
		{
			name: "the CA from the flag, the variable set for this server", refused: "prod", caEntry: unquotable, variable: "/ca.pem",
			from: config.CASource{Kind: config.CASourceFlag}, want: flagTxt + note,
		},
		{name: "the CA from the flag", refused: "prod", caEntry: unquotable, from: config.CASource{Kind: config.CASourceFlag}, want: flagTxt + note},
		{name: "the CA from an entry", refused: "prod", from: config.CASource{Kind: config.CASourceEntry, Entry: unquotable}, want: flagTxt + note},
		{
			name: "the delete clause of a plugin login entry", refused: unquotable, caEntry: "other", plugin: true,
			want: flagTxt + ", or save the CA on server entry 'other' with: " + saveOther + meaning + note,
		},
		{name: "both clauses", refused: unquotable, plugin: true, want: flagTxt + note},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvCACert, c.variable)
			t.Setenv(config.EnvURL, "")
			entry := config.Server{URL: server, Provenance: clierr.ProvenanceDerived}
			if c.plugin {
				entry.Provenance = clierr.ProvenanceExecLogin
			}
			clients := &callClients{server: server, caFrom: c.from, shim: true, skipVerify: true}
			lead, remedy := clients.storedLoginRefusal(storedCredential{name: c.refused, entry: entry}, c.caEntry)
			assert.Equal(t, "the stored login of server entry "+clierr.ShellQuote(c.refused)+
				" is sent only over a connection that verifies the server, and --skip-verify does not change that; ", lead)
			assert.Equal(t, c.want, clierr.ExpandRemedies(remedy.CA, ""))
		})
	}
}

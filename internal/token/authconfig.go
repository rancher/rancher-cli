package token

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// providerName is Rancher's short name for an auth provider type: the type
// in lower case without a trailing "config" and "provider" (openLdapProvider
// is openldap). Up to Rancher v2.15 it is also the name of the type's one
// auth config.
func providerName(providerType string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(providerType), "config"), "provider")
}

// authConfigName is the name a message gives an entry of the provider list:
// its id, the name of its auth config, or the provider name of its type
// when the entry has no id.
func authConfigName(prov typedProvider) string {
	if id := prov.GetID(); id != "" {
		return id
	}
	return providerName(prov.GetType())
}

// authConfigLabel is how a menu and the candidates show an entry: the name
// of its auth config, then the provider name of its type in parentheses
// when that differs from it (corp-ldap (openldap), local). An entry with
// no id shows its provider name.
func authConfigLabel(prov typedProvider) string {
	name, kind := prov.GetID(), providerName(prov.GetType())
	if name == "" || name == kind {
		return authConfigName(prov)
	}
	return name + " (" + kind + ")"
}

// authConfigNames are the names --auth-provider can take for the entries,
// in order: the ids. An entry with no id has none.
func authConfigNames(entries []typedProvider) []string {
	var names []string
	for _, prov := range entries {
		if id := prov.GetID(); id != "" {
			names = append(names, id)
		}
	}
	return names
}

// authConfigCandidates are the entries as candidates: the id, valid as
// given to --auth-provider, and the menu's label. An entry with no id is
// not one.
func authConfigCandidates(entries []typedProvider) []clierr.Candidate {
	cs := make([]clierr.Candidate, 0, len(entries))
	for _, prov := range entries {
		if id := prov.GetID(); id != "" {
			cs = append(cs, clierr.Candidate{ID: id, Name: authConfigLabel(prov)})
		}
	}
	return cs
}

// samlVendorLabels are the words the Rancher sign-in page shows in English
// for a SAML provider, by provider name.
// testdata/saml-vendor-labels.golden.json records them, and a test asserts
// the two are equal.
var samlVendorLabels = map[string]string{
	"adfs":        "ADFS",
	"genericsaml": "Generic SAML",
	"keycloak":    "Keycloak",
	"okta":        "Okta",
	"ping":        "Ping Identity",
	"shibboleth":  "Shibboleth",
}

// signInPageName is the name the Rancher sign-in page shows for an auth
// config: its id, or the vendor label of its provider when the id in lower
// case is the provider name. labelForm reports the second case. An entry with
// no id takes the label, and a provider with no label takes its name.
func signInPageName(id, provider string) (name string, labelForm bool) {
	if id != "" && strings.ToLower(id) != provider {
		return id, false
	}
	if label, ok := samlVendorLabels[provider]; ok {
		return label, true
	}
	return provider, true
}

const (
	// samlNoticeID is the notice for an auth config the page shows by its id.
	samlNoticeID = `Select "%s" on the Rancher sign-in page in your browser; the CLI cannot select it for you. If the page opens on another provider, use "Choose a different provider".`
	// samlNoticeLabel is the notice for one the page shows by its vendor
	// label, with the name of the auth config.
	samlNoticeLabel = `Select "%s" (auth config %s) on the Rancher sign-in page in your browser; the CLI cannot select it for you. If the page opens on another provider, use "Choose a different provider".`
)

// signInNotice is what a sign-in through chosen says before it asks
// anyone to act, "" for none. A password or OAuth sign-in through a type
// that has several auth configs on the server names the one it uses. A
// SAML sign-in goes through the Rancher sign-in page, where the user selects
// the provider; with several SAML auth configs the notice names the one to
// select, as the page shows it.
func signInNotice(entries []typedProvider, chosen typedProvider) string {
	typ := chosen.GetType()
	if samlProviders[typ] {
		saml := 0
		for _, prov := range entries {
			if samlProviders[prov.GetType()] {
				saml++
			}
		}
		if saml > 1 {
			name, labelForm := signInPageName(chosen.GetID(), providerName(typ))
			if labelForm {
				return fmt.Sprintf(samlNoticeLabel, name, output.Printable(authConfigName(chosen)))
			}
			return fmt.Sprintf(samlNoticeID, output.Printable(name))
		}
		return ""
	}
	same := 0
	for _, prov := range entries {
		if prov.GetType() == typ {
			same++
		}
	}
	if same > 1 {
		return "Signing in through auth config " + output.Printable(authConfigLabel(chosen)) + "."
	}
	return ""
}

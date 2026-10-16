package token

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
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

// signInNotice is what a sign-in through chosen says before it asks
// anyone to act, "" for none. A password or OAuth sign-in through a type
// that has several auth configs on the server names the one it uses. A
// SAML sign-in goes through the Rancher sign-in page, which makes its own
// choice among the SAML auth configs when there are several.
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
			return "The Rancher sign-in page in your browser offers its own choice of auth config; the CLI cannot pass yours to it."
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
		return "Signing in through auth config " + authConfigLabel(chosen) + "."
	}
	return ""
}

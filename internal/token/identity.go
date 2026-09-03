package token

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type TokenKind = clierr.TokenKind
type TokenRef = clierr.TokenRef
type CreateOutcome = clierr.CreateOutcome
type RevokeOutcome = clierr.RevokeOutcome
type RevokeResult = clierr.RevokeResult
type Provenance = clierr.Provenance

const (
	TokenKindExt = clierr.TokenKindExt
	TokenKindV3  = clierr.TokenKindV3

	CreateNotAttempted = clierr.CreateNotAttempted
	CreateNotCreated   = clierr.CreateNotCreated
	CreateKnown        = clierr.CreateKnown
	CreateUnidentified = clierr.CreateUnidentified
	CreateUnknown      = clierr.CreateUnknown

	ResultRevoked       = clierr.ResultRevoked
	ResultAlreadyGone   = clierr.ResultAlreadyGone
	ResultFailed        = clierr.ResultFailed
	ResultNotAttempted  = clierr.ResultNotAttempted
	ResultNotApplicable = clierr.ResultNotApplicable

	ProvenanceDerived       = clierr.ProvenanceDerived
	ProvenanceLegacySession = clierr.ProvenanceLegacySession
	ProvenanceUserSupplied  = clierr.ProvenanceUserSupplied
)

// NewOperationMarker returns 12 lowercase hex characters from crypto/rand:
// a non-secret identity for one login operation, embedded in the
// description of every token the operation creates so a leftover can be
// found by listing tokens.
func NewOperationMarker() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating the operation marker: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TokenDescription renders the description every token this operation
// creates carries.
func TokenDescription(host, marker string) string {
	return fmt.Sprintf("rancher-cli login @ %s [op:%s]", host, marker)
}

var v3NameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// TokenRefFromValidatedBearer derives a token's resource name from a
// bearer. It exists for one caller: the SAML authtokens flow, whose
// response carries only the encrypted bearer. It requires exactly one non-empty name:secret
// split and a valid object name; anything else reports false and the
// token stays unidentified.
func TokenRefFromValidatedBearer(kind TokenKind, bearer string) (TokenRef, bool) {
	name, secret, ok := strings.Cut(bearer, ":")
	if !ok || name == "" || secret == "" || strings.Contains(secret, ":") || !v3NameRE.MatchString(name) {
		return TokenRef{}, false
	}
	return TokenRef{Kind: kind, Name: name}, true
}

// usableBearer reports whether bearer may be used as the credential for the
// token the response named. It requires exactly one name:secret split with
// a non-empty secret and an embedded name equal to name; ext bearers carry
// the ext/ prefix before the name. It only compares: the resource name is
// always taken from response metadata, never from the bearer.
func usableBearer(kind TokenKind, name, bearer string) bool {
	embedded, secret, ok := strings.Cut(bearer, ":")
	if !ok || secret == "" || strings.Contains(secret, ":") {
		return false
	}
	if kind == TokenKindExt {
		embedded, ok = strings.CutPrefix(embedded, "ext/")
		if !ok {
			return false
		}
	}
	return embedded != "" && embedded == name
}

// CreateResult is what one token create left behind, as far as the client
// can tell. It is filled on the error path too: Ref and Outcome may
// identify a committed resource whose bearer failed validation.
type CreateResult struct {
	Outcome CreateOutcome
	// Kind is the created token's kind when it is known independently of a
	// name: the derived token create endpoint creates only ext tokens, the SAML authtokens
	// flow only v3 tokens, and a recognized login body declares its shape. Empty when
	// a login response was lost or unrecognizable: the token may then be of
	// either kind.
	Kind      TokenKind
	Ref       *TokenRef
	Bearer    string // usable bearer, or ""
	ExpiresAt string
	Marked    bool
	CreatedAt string
}

// deterministicNotCreated lists the statuses that prove the server did not
// create anything. Every other non-2xx status, including 5xx, may follow a
// committed create.
var deterministicNotCreated = map[int]bool{400: true, 401: true, 403: true, 404: true, 409: true, 422: true}

// classifyCreate applies only to the POST requests that perform a token
// create (the login body for basic and OAuth, and the ext derived token create); the SAML
// authtokens flow has no create response and takes a different path.
func classifyCreate(resp *http.Response, body []byte, err error, createdAt string, marked bool, kind TokenKind) CreateResult {
	r := CreateResult{Outcome: CreateUnknown, Kind: kind, Marked: marked, CreatedAt: createdAt}
	if err != nil || resp == nil {
		return r
	}
	// The status is classified before the shape: an error body establishes
	// no token shape, and a deterministic 4xx proves absence whatever the
	// body or kind. A 5xx with no shape stays unknown, of unknown kind.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if deterministicNotCreated[resp.StatusCode] {
			r.Outcome = CreateNotCreated
		}
		return r
	}
	if kind == "" {
		return r // a 2xx that established no shape: unknown, of unknown kind, no bearer to read
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return r // unreadable 2xx: the create may have committed
	}
	name, bearer, expires := extractIdentity(kind, probe)
	r.ExpiresAt = expires
	switch {
	case name != "":
		r.Ref = &TokenRef{Kind: kind, Name: name}
		r.Outcome = CreateKnown
		if usableBearer(kind, name, bearer) {
			r.Bearer = bearer
		}
	default:
		r.Outcome = CreateUnidentified
	}
	return r
}

// LoginResponseShape reads which token kind a /v1-public/login body
// describes, and whether it describes one at all. An ext.cattle.io/v1
// object of kind Token is an ext token; a body with no apiVersion that
// carries a token or id field is the v3 Norman token; anything else
// (unreadable JSON, a foreign apiVersion, an ext object of another kind, an
// empty body) establishes no shape and the kind stays unknown. Rancher
// answers login with either shape while it migrates session tokens; the kind
// is a fact of the response, never of the request, and the absence of a
// readable response proves nothing. parseLoginResponse uses the same
// discriminator, so the two cannot drift.
func LoginResponseShape(body []byte) (kind TokenKind, recognized bool) {
	var hdr struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Token      json.RawMessage `json:"token"`
		ID         json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &hdr) != nil {
		return "", false
	}
	switch {
	case hdr.APIVersion == "ext.cattle.io/v1" && hdr.Kind == "Token":
		return TokenKindExt, true
	case hdr.APIVersion == "" && (len(hdr.Token) > 0 || len(hdr.ID) > 0):
		return TokenKindV3, true
	}
	return "", false
}

// extractIdentity reads the resource name, bearer, and expiry from the two
// response shapes the CLI receives: an ext Token object (metadata.name,
// status.bearerToken, status.expiresAt) and the v3 login response (id,
// token, expiresAt). One shape per kind; there is no cross-shape fallback.
func extractIdentity(kind TokenKind, probe map[string]json.RawMessage) (name, bearer, expires string) {
	str := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	if kind == TokenKindExt {
		var meta struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(probe["metadata"], &meta)
		var status struct {
			Bearer  string `json:"bearerToken"`
			Expires string `json:"expiresAt"`
		}
		_ = json.Unmarshal(probe["status"], &status)
		return meta.Name, status.Bearer, status.Expires
	}
	return str(probe["id"]), str(probe["token"]), str(probe["expiresAt"])
}

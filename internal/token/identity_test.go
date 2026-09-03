package token

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOperationMarkerShape(t *testing.T) {
	t.Parallel()
	a, err := NewOperationMarker()
	require.NoError(t, err)
	b, err := NewOperationMarker()
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{12}$`), a)
	assert.NotEqual(t, a, b)
	assert.Equal(t, "rancher-cli login @ host1 [op:"+a+"]", TokenDescription("host1", a))
}

// TestTokenRefFromValidatedBearer asserts the one place a name is derived
// from a bearer (the SAML authtokens flow): exactly one non-empty
// name:secret split.
func TestTokenRefFromValidatedBearer(t *testing.T) {
	t.Parallel()
	ref, ok := TokenRefFromValidatedBearer(TokenKindV3, "token-abc12:sup3rs3cret")
	require.True(t, ok)
	assert.Equal(t, TokenRef{Kind: TokenKindV3, Name: "token-abc12"}, ref)
	for _, bad := range []string{"", "token-abc12", ":secret", "token-abc12:", "a:b:c", "Bad Name:secret"} {
		_, ok := TokenRefFromValidatedBearer(TokenKindV3, bad)
		assert.False(t, ok, bad)
	}
}

// TestUsableBearer asserts the validator that decides whether a returned
// bearer may be used: one name:secret split, a non-empty secret, and an
// embedded name equal to the resource name the response identified. It
// only ever compares; it never supplies the name.
func TestUsableBearer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind   TokenKind
		name   string
		bearer string
		ok     bool
	}{
		{TokenKindExt, "ext-1", "ext/ext-1:s3", true},
		{TokenKindExt, "ext-1", "ext/ext-1:", false},
		{TokenKindExt, "ext-1", "ext/ext-1", false},
		{TokenKindExt, "ext-1", "ext/ext-2:s3", false},
		{TokenKindExt, "ext-1", "ext-1:s3", false},
		{TokenKindExt, "ext-1", "ext/ext-1:a:b", false},
		{TokenKindV3, "token-x", "token-x:k", true},
		{TokenKindV3, "token-x", "token-x:", false},
		{TokenKindV3, "token-x", "token-y:k", false},
		{TokenKindV3, "token-x", "malformed", false},
		{TokenKindV3, "token-x", "", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.ok, usableBearer(tc.kind, tc.name, tc.bearer), "%s %q %q", tc.kind, tc.name, tc.bearer)
	}
}

// TestClassifyCreate asserts the conservative classification: only a
// deterministic 4xx proves absence; transport errors, 5xx, and unreadable
// bodies are unknown; a name without a usable bearer is known with an
// empty bearer, whatever the body carried in the bearer field.
func TestClassifyCreate(t *testing.T) {
	t.Parallel()
	ext := func(body string) []byte { return []byte(body) }
	cases := []struct {
		name    string
		resp    *http.Response
		body    []byte
		err     error
		outcome CreateOutcome
		refName string
		bearer  string
	}{
		{"transport error", nil, nil, errors.New("EOF"), CreateUnknown, "", ""},
		{"500", &http.Response{StatusCode: 500}, nil, nil, CreateUnknown, "", ""},
		{"401", &http.Response{StatusCode: 401}, nil, nil, CreateNotCreated, "", ""},
		{"409", &http.Response{StatusCode: 409}, nil, nil, CreateNotCreated, "", ""},
		{"201 unreadable", &http.Response{StatusCode: 201}, ext(`{not json`), nil, CreateUnknown, "", ""},
		{"201 no name no bearer", &http.Response{StatusCode: 201}, ext(`{"broken":true}`), nil, CreateUnidentified, "", ""},
		{"201 name only", &http.Response{StatusCode: 201}, ext(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"ext-1"},"status":{}}`), nil, CreateKnown, "ext-1", ""},
		{"201 name, malformed bearer", &http.Response{StatusCode: 201}, ext(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"ext-1"},"status":{"bearerToken":"malformed"}}`), nil, CreateKnown, "ext-1", ""},
		{"201 name, empty secret", &http.Response{StatusCode: 201}, ext(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"ext-1"},"status":{"bearerToken":"ext/ext-1:"}}`), nil, CreateKnown, "ext-1", ""},
		{"201 name, bearer names another token", &http.Response{StatusCode: 201}, ext(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"ext-1"},"status":{"bearerToken":"ext/ext-9:s3"}}`), nil, CreateKnown, "ext-1", ""},
		{"201 valid", &http.Response{StatusCode: 201}, ext(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"ext-1"},"status":{"bearerToken":"ext/ext-1:s3","expiresAt":"2030-01-01T00:00:00Z"}}`), nil, CreateKnown, "ext-1", "ext/ext-1:s3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := classifyCreate(tc.resp, tc.body, tc.err, "2026-09-02T00:00:00Z", true, TokenKindExt)
			assert.Equal(t, tc.outcome, r.Outcome)
			assert.Equal(t, TokenKindExt, r.Kind, "the derived token create endpoint's kind is known whatever the response")
			assert.Equal(t, tc.bearer, r.Bearer)
			assert.True(t, r.Marked)
			assert.Equal(t, "2026-09-02T00:00:00Z", r.CreatedAt)
			if tc.refName == "" {
				assert.Nil(t, r.Ref)
			} else {
				require.NotNil(t, r.Ref)
				assert.Equal(t, tc.refName, r.Ref.Name)
				assert.Equal(t, TokenKindExt, r.Ref.Kind)
			}
		})
	}
}

// TestLoginResponseShapeFollowsTheBody asserts that the kind of a login
// token is read from the body and that nothing else is assumed: an ext
// Token object is ext, a Norman body is v3, and unreadable JSON, a foreign
// apiVersion, an ext object of another kind, and an empty object establish
// no shape.
func TestLoginResponseShapeFollowsTheBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body string
		kind TokenKind
		ok   bool
	}{
		{`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"ext-1"},"status":{"bearerToken":"ext/ext-1:s"}}`, TokenKindExt, true},
		{`{"id":"token-x","token":"token-x:k"}`, TokenKindV3, true},
		{`{"id":"token-x"}`, TokenKindV3, true},
		{`{not json`, "", false},
		{`{"apiVersion":"ext.cattle.io/v1","kind":"Session","metadata":{"name":"ext-1"},"status":{"bearerToken":"ext/ext-1:s"}}`, "", false},
		{`{"apiVersion":"other.io/v1","kind":"Token","token":"x:y"}`, "", false},
		{`{"broken":true}`, "", false},
		{`{"id": null}`, "", false},
		{`{"id": null, "token": null}`, "", false},
		{`{"id": null, "token": "token-x:k"}`, TokenKindV3, true},
	}
	for _, tc := range cases {
		kind, ok := LoginResponseShape([]byte(tc.body))
		assert.Equal(t, tc.kind, kind, tc.body)
		assert.Equal(t, tc.ok, ok, tc.body)
	}
}

// TestClassifyCreateDeterministic4xxWithUnknownKind asserts that a login
// rejection proves absence whatever the body: a bodyless or error-bodied
// 401/403/409 with no recognized shape is notCreated, not unknown, while a
// 500 with no shape stays unknown of unknown kind.
func TestClassifyCreateDeterministic4xxWithUnknownKind(t *testing.T) {
	t.Parallel()
	for _, status := range []int{401, 403, 404, 409, 422} {
		kind, _ := LoginResponseShape([]byte(`{"type":"error","status":` + fmt.Sprint(status) + `}`))
		require.Empty(t, kind)
		r := classifyCreate(&http.Response{StatusCode: status}, nil, nil, "t", true, kind)
		assert.Equal(t, clierr.CreateNotCreated, r.Outcome, status)
		assert.Empty(t, r.Kind)
	}
	r := classifyCreate(&http.Response{StatusCode: 500}, nil, nil, "t", true, "")
	assert.Equal(t, clierr.CreateUnknown, r.Outcome)
	assert.Empty(t, r.Kind)
}

// TestClassifyCreateExtVersionWithoutTokenKindIsNotKnown asserts that the
// classifier never consumes a bearer from a body the shape reader did not
// recognize: an ext apiVersion with another kind stays unknown with no
// bearer, and parseLoginResponse rejects the same body.
func TestClassifyCreateExtVersionWithoutTokenKindIsNotKnown(t *testing.T) {
	t.Parallel()
	body := []byte(`{"apiVersion":"ext.cattle.io/v1","kind":"Session","metadata":{"name":"ext-1"},"status":{"bearerToken":"ext/ext-1:s"}}`)
	kind, ok := LoginResponseShape(body)
	require.False(t, ok)
	r := classifyCreate(&http.Response{StatusCode: 201}, body, nil, "t", true, kind)
	assert.Equal(t, clierr.CreateUnknown, r.Outcome)
	assert.Empty(t, r.Bearer)
	assert.Nil(t, r.Ref)
	assert.Empty(t, r.Kind)
	_, perr := parseLoginResponse(body)
	require.Error(t, perr)
}

// TestClassifyCreateKubeconfigLoginExtShape asserts the façade's kubeconfig
// login response (an ext Token object) end to end through the kind reader
// and the classifier: known, ext kind, usable ext bearer.
func TestClassifyCreateKubeconfigLoginExtShape(t *testing.T) {
	t.Parallel()
	body := []byte(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"fac-token-1","resourceVersion":"1"},"status":{"bearerToken":"ext/fac-token-1:fac-secret-1","expiresAt":"2030-01-01T00:00:00Z"}}`)
	kind, ok := LoginResponseShape(body)
	require.True(t, ok)
	r := classifyCreate(&http.Response{StatusCode: 201}, body, nil, "t", false, kind)
	assert.Equal(t, clierr.CreateKnown, r.Outcome)
	assert.Equal(t, TokenKindExt, r.Kind)
	require.NotNil(t, r.Ref)
	assert.Equal(t, TokenRef{Kind: TokenKindExt, Name: "fac-token-1"}, *r.Ref)
	assert.Equal(t, "ext/fac-token-1:fac-secret-1", r.Bearer)
}

// TestClassifyCreateV3Login asserts the json login response shape: the
// resource name is the id field, the bearer is name:key, and a bearer
// that fails validation leaves a known token with no usable bearer.
func TestClassifyCreateV3Login(t *testing.T) {
	t.Parallel()
	r := classifyCreate(&http.Response{StatusCode: 201}, []byte(`{"token":"token-x:k","id":"token-x","expiresAt":"2030-01-01T00:00:00Z"}`), nil, "t", true, TokenKindV3)
	assert.Equal(t, clierr.CreateKnown, r.Outcome)
	require.NotNil(t, r.Ref)
	assert.Equal(t, TokenRef{Kind: TokenKindV3, Name: "token-x"}, *r.Ref)
	assert.Equal(t, "token-x:k", r.Bearer)
	for _, bad := range []string{`"malformed"`, `"token-x:"`, `"token-y:k"`, `""`} {
		r := classifyCreate(&http.Response{StatusCode: 201}, []byte(`{"token":`+bad+`,"id":"token-x"}`), nil, "t", true, TokenKindV3)
		assert.Equal(t, clierr.CreateKnown, r.Outcome, bad)
		require.NotNil(t, r.Ref, bad)
		assert.Equal(t, "token-x", r.Ref.Name, bad)
		assert.Empty(t, r.Bearer, bad)
	}
}

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
	assert.Equal(t, "cli.rancher.io login @ host1 [op:"+a+"]", TokenDescription("host1", a))
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
// bearer.
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
}

// TestClassifyCreateKubeconfigLoginExtShape asserts the facade's kubeconfig
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

// TestClassifyCreateReadsOwnerAndScope asserts the ext create response's
// spec.userID and spec.clusterName, and the v3 body's userId and
// clusterId, land on CreateResult; absent fields read as empty.
func TestClassifyCreateReadsOwnerAndScope(t *testing.T) {
	t.Parallel()
	ext := []byte(`{"metadata":{"name":"token-1"},"spec":{"clusterName":"c-test","userID":"user-a"},"status":{"bearerToken":"ext/token-1:s"}}`)
	r := classifyCreate(&http.Response{StatusCode: 201}, ext, nil, "t", true, TokenKindExt)
	assert.Equal(t, CreateKnown, r.Outcome)
	assert.Equal(t, "user-a", r.UserID)
	assert.Equal(t, "c-test", r.ClusterName)

	v3 := []byte(`{"id":"kubeconfig-u-x","token":"kubeconfig-u-x:s","clusterId":"c-test","userId":"user-b","expiresAt":"2026-01-01T00:00:00Z"}`)
	r = classifyCreate(&http.Response{StatusCode: 201}, v3, nil, "t", true, TokenKindV3)
	assert.Equal(t, "user-b", r.UserID)
	assert.Equal(t, "c-test", r.ClusterName)

	bare := []byte(`{"metadata":{"name":"token-2"},"status":{"bearerToken":"ext/token-2:s"}}`)
	r = classifyCreate(&http.Response{StatusCode: 201}, bare, nil, "t", true, TokenKindExt)
	assert.Empty(t, r.UserID)
	assert.Empty(t, r.ClusterName)
}

// TestLoginCreateResult asserts what a login answer of each shape and each
// malformed form leaves behind: the kind it declares, the identified token,
// the usable bearer and the expiry.
func TestLoginCreateResult(t *testing.T) {
	t.Parallel()
	created := &http.Response{StatusCode: 201}

	t.Run("a v3 token answer", func(t *testing.T) {
		body := []byte(`{"type":"token","id":"token-abc","token":"token-abc:secretxyz","expiresAt":"2026-08-01T12:00:00Z","userId":"user-123"}`)
		r := loginCreateResult(created, body, nil, "t", true)
		assert.Equal(t, clierr.CreateKnown, r.Outcome)
		assert.Equal(t, TokenKindV3, r.Kind)
		assert.Equal(t, &TokenRef{Kind: TokenKindV3, Name: "token-abc"}, r.Ref)
		assert.Equal(t, "token-abc:secretxyz", r.Bearer)
		assert.Equal(t, "2026-08-01T12:00:00Z", r.ExpiresAt)
		assert.Equal(t, "user-123", r.UserID)
	})

	t.Run("an ext Token answer keeps its ext prefix", func(t *testing.T) {
		body := []byte(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"token-xyz"},"spec":{"userID":"user-123"},"status":{"bearerToken":"ext/token-xyz:secretabc","expiresAt":"2026-08-01T12:00:00Z"}}`)
		r := loginCreateResult(created, body, nil, "t", true)
		assert.Equal(t, clierr.CreateKnown, r.Outcome)
		assert.Equal(t, TokenKindExt, r.Kind)
		assert.Equal(t, "ext/token-xyz:secretabc", r.Bearer)
		assert.Equal(t, "2026-08-01T12:00:00Z", r.ExpiresAt)
		assert.Equal(t, "user-123", r.UserID)
	})

	t.Run("a foreign apiVersion establishes no token", func(t *testing.T) {
		body := []byte(`{"apiVersion":"ext.cattle.io/v2","kind":"Token","status":{}}`)
		r := loginCreateResult(created, body, nil, "t", true)
		assert.Equal(t, clierr.CreateUnknown, r.Outcome)
		assert.Empty(t, r.Kind)
		assert.Empty(t, r.Bearer)
		assert.Nil(t, r.Ref)
	})

	t.Run("a versioned body that is not an ext Token is not read as v3", func(t *testing.T) {
		body := []byte(`{"apiVersion":"management.cattle.io/v3","kind":"somethingElse","token":"tok:sec","expiresAt":""}`)
		r := loginCreateResult(created, body, nil, "t", true)
		assert.Equal(t, clierr.CreateUnknown, r.Outcome)
		assert.Empty(t, r.Bearer)
		assert.Nil(t, r.Ref)
	})

	t.Run("an ext Token answer with no name and no bearer is unidentified", func(t *testing.T) {
		body := []byte(`{"apiVersion":"ext.cattle.io/v1","kind":"Token","status":{}}`)
		r := loginCreateResult(created, body, nil, "t", true)
		assert.Equal(t, clierr.CreateUnidentified, r.Outcome)
		assert.Equal(t, TokenKindExt, r.Kind)
		assert.Empty(t, r.Bearer)
		assert.Nil(t, r.Ref)
	})

	t.Run("unreadable JSON leaves the create unknown", func(t *testing.T) {
		r := loginCreateResult(created, []byte(`{not json`), nil, "t", true)
		assert.Equal(t, clierr.CreateUnknown, r.Outcome)
		assert.Empty(t, r.Kind)
		assert.Empty(t, r.Bearer)
	})

	t.Run("a login answer that carries createdTS and ttl keeps the expiry it gave", func(t *testing.T) {
		withExpiry := []byte(`{"id":"token-abc","token":"token-abc:s","expiresAt":"2026-08-01T12:00:00Z","createdTS":1785585600000,"ttl":3600000}`)
		r := loginCreateResult(created, withExpiry, nil, "t", true)
		assert.Equal(t, "2026-08-01T12:00:00Z", r.ExpiresAt)

		noExpiry := []byte(`{"id":"token-abc","token":"token-abc:s","expiresAt":"","createdTS":1785585600000,"ttl":3600000}`)
		r = loginCreateResult(created, noExpiry, nil, "t", true)
		assert.Empty(t, r.ExpiresAt, "the login path never computes an expiry")
	})
}

// TestClassifyCreateReadsCreatedTSAndTTLOfAV3Answer asserts that a v3 answer's
// createdTS and ttl are read as integers, and that no other kind and no
// other form of the fields is read.
func TestClassifyCreateReadsCreatedTSAndTTLOfAV3Answer(t *testing.T) {
	t.Parallel()
	ok := &http.Response{StatusCode: 201}
	v3 := func(fields string) CreateResult {
		body := []byte(`{"id":"token-1","token":"token-1:s","expiresAt":"",` + fields + `}`)
		return classifyCreate(ok, body, nil, "t", false, TokenKindV3)
	}
	r := v3(`"createdTS":1791564534000,"ttl":7776000000`)
	assert.Equal(t, int64(1791564534000), r.CreatedTS)
	assert.Equal(t, int64(7776000000), r.TTL)

	r = v3(`"createdTS":"1791564534000","ttl":1.5`)
	assert.Zero(t, r.CreatedTS, "a string is not an integer")
	assert.Zero(t, r.TTL, "a fraction is not an integer")

	r = v3(`"unrelated":1`)
	assert.Zero(t, r.CreatedTS)
	assert.Zero(t, r.TTL)

	ext := []byte(`{"metadata":{"name":"token-1"},"createdTS":1791564534000,"ttl":7776000000,"status":{"bearerToken":"ext/token-1:s"}}`)
	r = classifyCreate(ok, ext, nil, "t", false, TokenKindExt)
	assert.Zero(t, r.CreatedTS, "an ext answer's fields are not read")
	assert.Zero(t, r.TTL)
}

// TestComputedExpiry asserts the expiry a create answer's createdTS and ttl
// give: their sum in RFC 3339 UTC, and empty when either is not positive or
// the sum is out of range.
func TestComputedExpiry(t *testing.T) {
	t.Parallel()
	const day = int64(24 * 60 * 60 * 1000)
	const base = int64(1791564534000)
	tests := []struct {
		name      string
		createdTS int64
		ttl       int64
		want      string
	}{
		{"ninety days", base, 90 * day, "2027-01-07T16:48:54Z"},
		{"one second", base, 1000, "2026-10-09T16:48:55Z"},
		{"no ttl", base, 0, ""},
		{"negative ttl", base, -1, ""},
		{"no createdTS", 0, 90 * day, ""},
		{"negative createdTS", -1, 90 * day, ""},
		{"the last second of year 9999", 253402300799000 - 1000, 1000, "9999-12-31T23:59:59Z"},
		{"a sum past year 9999", 253402300799000, 1, ""},
		{"a sum that overflows", base, 1<<63 - 1, ""},
		{"a createdTS past year 9999", 253402300800000, 1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, computedExpiry(tc.createdTS, tc.ttl))
		})
	}
}

// TestHostLoginDescriptionNamesTheProduct asserts the description of a token
// created outside a login operation starts with the product identifier
// cli.rancher.io and carries no operation marker.
func TestHostLoginDescriptionNamesTheProduct(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "cli.rancher.io login @ "+hostname(), hostLoginDescription())
}

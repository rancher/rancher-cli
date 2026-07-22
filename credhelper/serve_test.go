package credhelper

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mapHandler struct {
	creds    map[string]string // key: serverURL+"\n"+name
	readOnly bool
}

func key(u, n string) string { return u + "\n" + n }

func (m *mapHandler) Get(u, n string) (*Credentials, error) {
	if t, ok := m.creds[key(u, n)]; ok {
		return &Credentials{Token: t}, nil
	}
	return nil, nil
}
func (m *mapHandler) Store(u, n, tok string) error {
	if m.readOnly {
		return ErrStoreUnsupported
	}
	m.creds[key(u, n)] = tok
	return nil
}
func (m *mapHandler) Erase(u, n string) error { delete(m.creds, key(u, n)); return nil }

func run(t *testing.T, h Handler, verb string, req Request) (int, Response, string) {
	t.Helper()
	in, err := json.Marshal(req)
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	code := serve(h, []string{verb}, bytes.NewReader(in), &out, &errOut)
	var resp Response
	if out.Len() > 0 {
		require.NoError(t, json.Unmarshal(out.Bytes(), &resp))
	}
	return code, resp, errOut.String()
}

func TestServeStoreGetErase(t *testing.T) {
	t.Parallel()
	h := &mapHandler{creds: map[string]string{}}
	req := Request{ProtocolVersion: 1, ServerURL: "https://r.example.com", Name: "prod"}

	code, _, _ := run(t, h, VerbStore, Request{ProtocolVersion: 1, ServerURL: req.ServerURL, Name: req.Name, Token: "id:sec"})
	assert.Equal(t, 0, code)

	code, resp, _ := run(t, h, VerbGet, req)
	assert.Equal(t, 0, code)
	assert.Equal(t, "id:sec", resp.Token)
	assert.Equal(t, 1, resp.ProtocolVersion)

	code, _, _ = run(t, h, VerbErase, req)
	assert.Equal(t, 0, code)

	code, resp, _ = run(t, h, VerbGet, req)
	assert.Equal(t, 0, code, "not-found is exit 0")
	assert.Empty(t, resp.Token, "absent token means not found")
}

func TestServeReadOnlyStore(t *testing.T) {
	t.Parallel()
	h := &mapHandler{creds: map[string]string{}, readOnly: true}
	code, resp, _ := run(t, h, VerbStore, Request{ProtocolVersion: 1, ServerURL: "https://r.example.com", Name: "p", Token: "t"})
	assert.Equal(t, 0, code)
	assert.True(t, resp.Unsupported)
}

func TestServeRejections(t *testing.T) {
	t.Parallel()
	h := &mapHandler{creds: map[string]string{}}
	// unknown verb
	code := serve(h, []string{"list"}, strings.NewReader("{}"), &bytes.Buffer{}, &bytes.Buffer{})
	assert.NotEqual(t, 0, code)
	// missing verb
	code = serve(h, nil, strings.NewReader("{}"), &bytes.Buffer{}, &bytes.Buffer{})
	assert.NotEqual(t, 0, code)
	// unsupported protocol version
	code, _, errOut := run(t, h, VerbGet, Request{ProtocolVersion: 99, ServerURL: "https://x", Name: "n"})
	assert.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "protocol version")
}

func TestServeUnknownRequestFieldsIgnored(t *testing.T) {
	t.Parallel()
	h := &mapHandler{creds: map[string]string{key("https://r.example.com", "prod"): "tok"}}
	in := `{"protocolVersion":1,"serverURL":"https://r.example.com","name":"prod","futureField":true}`
	var out, errOut bytes.Buffer
	code := serve(h, []string{VerbGet}, strings.NewReader(in), &out, &errOut)
	assert.Equal(t, 0, code, "unknown request fields must be ignored: %s", errOut.String())
}

func TestServeExpiry(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	h := handlerFunc{get: func(u, n string) (*Credentials, error) {
		return &Credentials{Token: "short", ExpiresAt: exp}, nil
	}}
	code, resp, _ := run(t, h, VerbGet, Request{ProtocolVersion: 1, ServerURL: "https://x", Name: "n"})
	assert.Equal(t, 0, code)
	assert.Equal(t, "2026-07-21T18:00:00Z", resp.ExpiresAt)
}

// handlerFunc adapts closures to Handler for one-off tests.
type handlerFunc struct {
	get   func(u, n string) (*Credentials, error)
	store func(u, n, tok string) error
	erase func(u, n string) error
}

func (h handlerFunc) Get(u, n string) (*Credentials, error) {
	if h.get == nil {
		return nil, nil
	}
	return h.get(u, n)
}
func (h handlerFunc) Store(u, n, tok string) error {
	if h.store == nil {
		return errors.New("unexpected store")
	}
	return h.store(u, n, tok)
}
func (h handlerFunc) Erase(u, n string) error {
	if h.erase == nil {
		return nil
	}
	return h.erase(u, n)
}

package credhelper

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestRoundTrip(t *testing.T) {
	t.Parallel()
	in := Request{ProtocolVersion: ProtocolVersion, ServerURL: "https://r.example.com", Name: "prod", Token: "id:secret"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var out Request
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestResponseIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	var r Response
	require.NoError(t, json.Unmarshal([]byte(`{"protocolVersion":1,"token":"t","future":"x"}`), &r))
	assert.Equal(t, 1, r.ProtocolVersion)
	assert.Equal(t, "t", r.Token)
}

package telemetrysend

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The types below read an OTLP/HTTP JSON body the way a strict receiver does:
// unknown fields are refused, an int64 must be a JSON string, and an AnyValue
// holds exactly one value field. They use only the standard library, so the
// test of the encoder does not share a type with the encoder.

type decExport struct {
	ResourceLogs []decResourceLogs `json:"resourceLogs"`
}

type decResourceLogs struct {
	Resource struct {
		Attributes []decKeyValue `json:"attributes"`
	} `json:"resource"`
	ScopeLogs []decScopeLogs `json:"scopeLogs"`
	SchemaURL string         `json:"schemaUrl"`
}

type decScopeLogs struct {
	Scope struct {
		Name    *string `json:"name"`
		Version *string `json:"version"`
	} `json:"scope"`
	LogRecords []decLogRecord `json:"logRecords"`
	SchemaURL  string         `json:"schemaUrl"`
}

type decLogRecord struct {
	TimeUnixNano *string       `json:"timeUnixNano"`
	EventName    *string       `json:"eventName"`
	Attributes   []decKeyValue `json:"attributes"`
}

type decKeyValue struct {
	Key   string    `json:"key"`
	Value decAnyVal `json:"value"`
}

type decAnyVal struct {
	StringValue *string `json:"stringValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
	ArrayValue  *struct {
		Values *[]decAnyVal `json:"values,omitempty"`
	} `json:"arrayValue,omitempty"`
}

// decodeExport decodes body strictly.
func decodeExport(t *testing.T, body []byte) decExport {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var out decExport
	require.NoError(t, dec.Decode(&out))
	require.False(t, dec.More(), "trailing data after the body")
	return out
}

// flatValue returns the Go value encoding/json gives the flat form of v.
func flatValue(t *testing.T, v decAnyVal) any {
	t.Helper()
	set := 0
	var out any
	if v.StringValue != nil {
		set++
		out = *v.StringValue
	}
	if v.BoolValue != nil {
		set++
		out = *v.BoolValue
	}
	if v.IntValue != nil {
		set++
		n, err := strconv.ParseInt(*v.IntValue, 10, 64)
		require.NoError(t, err)
		out = float64(n)
	}
	if v.ArrayValue != nil {
		set++
		require.NotNil(t, v.ArrayValue.Values, "an arrayValue without values")
		list := []any{}
		for _, e := range *v.ArrayValue.Values {
			list = append(list, flatValue(t, e))
		}
		out = list
	}
	require.Equal(t, 1, set, "an AnyValue must hold exactly one value field")
	return out
}

// rebuildFlat rebuilds the flat event of each resourceLogs entry of body.
func rebuildFlat(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rl := range decodeExport(t, body).ResourceLogs {
		require.Len(t, rl.ScopeLogs, 1)
		sl := rl.ScopeLogs[0]
		require.Len(t, sl.LogRecords, 1)
		rec := sl.LogRecords[0]
		flat := map[string]any{}
		if rec.EventName != nil {
			flat["event.name"] = *rec.EventName
		}
		if rec.TimeUnixNano != nil {
			n, err := strconv.ParseInt(*rec.TimeUnixNano, 10, 64)
			require.NoError(t, err)
			flat["time"] = time.Unix(0, n).UTC().Format(time.RFC3339Nano)
		}
		if sl.Scope.Version != nil {
			flat["schema_version"] = *sl.Scope.Version
		}
		for _, kv := range slices.Concat(rl.Resource.Attributes, rec.Attributes) {
			_, dup := flat[kv.Key]
			require.False(t, dup, "attribute %s written twice", kv.Key)
			flat[kv.Key] = flatValue(t, kv.Value)
		}
		name, hasName := flat["service.name"]
		if sl.Scope.Name == nil {
			require.False(t, hasName, "scope.name is absent and service.name is not")
		} else {
			require.Equal(t, name, *sl.Scope.Name, "scope.name must equal service.name")
		}
		out = append(out, flat)
	}
	return out
}

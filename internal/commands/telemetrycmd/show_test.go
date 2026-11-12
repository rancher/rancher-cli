package telemetrycmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/output"
)

// TestShowListsTheParsableDistinctEventsOldestFirst asserts show lists
// exactly the events a reader can parse, once each, oldest first: a store
// with two events, a third slot repeating a uid, an empty slot, a truncated
// slot, and a compacted hour holding one valid event and one garbage line.
func TestShowListsTheParsableDistinctEventsOldestFirst(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	h1, h2, h3 := "2026102010", "2026102011", "2026102012"
	putFile(t, dir, "telemetry/events/"+h1+"/0001.json", eventJSON(1, "2026-10-20T10:00:00Z", "get project", "ok", 0))
	putFile(t, dir, "telemetry/events/"+h1+"/0002.json", eventJSON(1, "2026-10-20T10:00:00Z", "get project", "ok", 0))
	putFile(t, dir, "telemetry/events/"+h1+"/0003.json", "")
	putFile(t, dir, "telemetry/events/"+h1+"/0004.json", eventJSON(9, "2026-10-20T10:00:00Z", "get project", "ok", 0)[:40])
	putFile(t, dir, "telemetry/events/"+h2+"/"+h2+".jsonl",
		eventJSON(2, "2026-10-20T11:00:00Z", "auth login", "error", 1)+"\nthis is not json\n")
	putFile(t, dir, "telemetry/events/"+h3+"/0007.json", eventJSON(3, "2026-10-20T12:00:00Z", "config path", "ok", 0))

	res, err := execute(t, NewShow(dir))
	require.NoError(t, err)
	assert.Equal(t, "TelemetryEventList", res.Kind)
	assert.Empty(t, res.Warnings)
	list, ok := res.Data.(eventList)
	require.True(t, ok)
	require.Len(t, list, 3)
	assert.Equal(t, uidOf(1), list[0]["log.record.uid"])
	assert.Equal(t, uidOf(2), list[1]["log.record.uid"])
	assert.Equal(t, uidOf(3), list[2]["log.record.uid"])
	assert.NotContains(t, list[0], "local_format")

	assert.Equal(t, []string{"HOUR", "EVENT", "COMMAND", "OUTCOME", "EXIT CODE"}, list.Columns())
	assert.Equal(t, [][]string{
		{"2026-10-20T10:00:00Z", "io.rancher.cli.command.run", "get project", "ok", "0"},
		{"2026-10-20T11:00:00Z", "io.rancher.cli.command.run", "auth login", "error", "1"},
		{"2026-10-20T12:00:00Z", "io.rancher.cli.command.run", "config path", "ok", "0"},
	}, list.Rows())

	var buf bytes.Buffer
	require.NoError(t, output.Render(&buf, "json", &output.Envelope{Kind: res.Kind, Data: res.Data}))
	var env struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.Len(t, env.Data, 3)
	assert.Equal(t, "get project", env.Data[0]["io.rancher.cli.command"])
	assert.EqualValues(t, 1, env.Data[1]["process.exit.code"])
}

// TestShowListsAnEventWithMissingFieldsAsEmptyCells asserts a field an event
// lacks is an empty cell.
func TestShowListsAnEventWithMissingFieldsAsEmptyCells(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	putFile(t, dir, "telemetry/events/2026102010/0001.json",
		`{"event.name":"io.rancher.cli.events.dropped","time":"2026-10-20T10:00:00Z","schema_version":"1.0.0","log.record.uid":"`+uidOf(5)+`"}`)
	res, err := execute(t, NewShow(dir))
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"2026-10-20T10:00:00Z", "io.rancher.cli.events.dropped", "", "", ""}}, res.Data.(eventList).Rows())
}

// TestShowEmptyStoreIsAnEmptyList asserts an empty store, a missing
// directory and a directory with no telemetry directory each give an empty
// list, never null, in JSON.
func TestShowEmptyStoreIsAnEmptyList(t *testing.T) {
	controlsEnv(t)
	for name, prepare := range map[string]func(dir string){
		"missing":      func(string) {},
		"empty":        func(dir string) { require.NoError(t, os.MkdirAll(dir, 0o700)) },
		"empty events": func(dir string) { require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry", "events"), 0o700)) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := newDir(t)
			prepare(dir)
			res, err := execute(t, NewShow(dir))
			require.NoError(t, err)
			list, ok := res.Data.(eventList)
			require.True(t, ok)
			require.NotNil(t, list)
			assert.Empty(t, list)
			b, err := json.Marshal(res.Data)
			require.NoError(t, err)
			assert.JSONEq(t, `[]`, string(b))
		})
	}
}

// TestShowReadsCompactedHours asserts events in a compacted hour file appear
// in the list, so a reader that skips those files fails here.
func TestShowReadsCompactedHours(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	h := "2026102010"
	putFile(t, dir, "telemetry/events/"+h+"/"+h+".jsonl",
		eventJSON(1, "2026-10-20T10:00:00Z", "get project", "ok", 0)+"\n"+eventJSON(2, "2026-10-20T10:00:00Z", "get cluster", "ok", 0)+"\n")
	res, err := execute(t, NewShow(dir))
	require.NoError(t, err)
	assert.Len(t, res.Data.(eventList), 2)
}

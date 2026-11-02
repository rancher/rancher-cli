package telemetrycmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
)

var uploadView = telemetry.ExperimentView{Names: []string{"telemetryupload"}, Endpoint: "https://h.example"}

// putState writes the upload state file of dir.
func putState(t *testing.T, dir string, st telemetry.UploadState) {
	t.Helper()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	putFile(t, dir, "telemetry/upload/state.json", string(b))
}

// putSlots writes n slot events into the hour directory of an hour that ended
// hoursAgo hours ago (0 is the current hour), numbered from first.
func putSlots(t *testing.T, dir string, hoursAgo, first, n int) {
	t.Helper()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(hoursAgo) * time.Hour)
	for i := range n {
		putFile(t, dir, "telemetry/events/"+hourDirName(hour)+"/"+fourDigits(i+1)+".json",
			eventJSON(first+i, hour.Format(time.RFC3339), "get project", "ok", 0))
	}
}

func fourDigits(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 4-len(s)) + s
}

// putCompacted writes a compacted file of n events, two minutes old, in the
// hour that ended hoursAgo hours ago.
func putCompacted(t *testing.T, dir string, hoursAgo, first, n int) {
	t.Helper()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(hoursAgo) * time.Hour)
	var b strings.Builder
	for i := range n {
		b.WriteString(eventJSON(first+i, hour.Format(time.RFC3339), "get project", "ok", 0) + "\n")
	}
	rel := "telemetry/events/" + hourDirName(hour) + "/" + hourDirName(hour) + ".jsonl"
	putFile(t, dir, rel, b.String())
	old := time.Now().Add(-2 * time.Minute)
	require.NoError(t, os.Chtimes(filepath.Join(dir, filepath.FromSlash(rel)), old, old))
}

func statusOf(t *testing.T, dir string, view telemetry.ExperimentView) (text string, data map[string]any, warnings []string) {
	t.Helper()
	res, err := execute(t, NewStatus(dir, view))
	require.NoError(t, err)
	return res.Text, dataMap(t, res), res.Warnings
}

// TestStatusShowsTheUploadState asserts, in the text lines and the data keys,
// the last attempt for each kind of answer, the next attempt as due or
// future, the events waiting without the covered ids and up to the count
// limit, the events of the current hour, the warnings for an upload lock that
// cannot be read, for a plain file in place of telemetry/upload, for a
// directory in place of the state file and for a telemetry directory that
// cannot be searched (named by the upload directory, not the lock), no warning
// for a held lock, no next attempt and no waiting lines for the bare name, and
// none of the lines for the zero view; and that status creates nothing.
func TestStatusShowsTheUploadState(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 30, 15, 0, time.UTC)
	atText := "2026-11-01T10:30:15Z"
	three := 3

	for _, tc := range []struct {
		name  string
		state telemetry.UploadState
		line  string
		data  map[string]any
	}{
		{"a 200 with counts", telemetry.UploadState{LastAttempt: at, HTTPStatus: 200, Sent: 10, Refused: &three},
			atText + ", HTTP 200, 7 accepted, 3 refused",
			map[string]any{"time": atText, "httpStatus": 200, "noAnswer": "", "sent": 10, "refused": 3, "dropped": 0}},
		{"a 200 with the refused count unknown", telemetry.UploadState{LastAttempt: at, HTTPStatus: 200, Sent: 10},
			atText + ", HTTP 200, counts unknown",
			map[string]any{"time": atText, "httpStatus": 200, "noAnswer": "", "sent": 10, "refused": nil, "dropped": 0}},
		{"a refused count above the sent count", telemetry.UploadState{LastAttempt: at, HTTPStatus: 200, Sent: 2, Refused: &three},
			atText + ", HTTP 200, counts unknown",
			map[string]any{"time": atText, "httpStatus": 200, "noAnswer": "", "sent": 2, "refused": nil, "dropped": 0}},
		{"a 400 with dropped events", telemetry.UploadState{LastAttempt: at, HTTPStatus: 400, Dropped: 5},
			atText + ", HTTP 400, counts unknown, 5 events deleted",
			map[string]any{"time": atText, "httpStatus": 400, "noAnswer": "", "sent": 0, "refused": nil, "dropped": 5}},
		{"no answer", telemetry.UploadState{LastAttempt: at, NoAnswer: "timeout"},
			atText + ", no answer: timeout",
			map[string]any{"time": atText, "httpStatus": 0, "noAnswer": "timeout", "sent": 0, "refused": nil, "dropped": 0}},
		{"an attempt that sent nothing", telemetry.UploadState{LastAttempt: at},
			atText + ", nothing sent",
			map[string]any{"time": atText, "httpStatus": 0, "noAnswer": "", "sent": 0, "refused": nil, "dropped": 0}},
		{"a lock that could not be taken", telemetry.UploadState{LastAttempt: at, NoAnswer: "lock unavailable"},
			atText + ", not sent: the upload lock cannot be taken",
			map[string]any{"time": atText, "httpStatus": 0, "noAnswer": "lock unavailable", "sent": 0, "refused": nil, "dropped": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlsEnv(t)
			dir := newDir(t)
			putState(t, dir, tc.state)
			text, data, _ := statusOf(t, dir, uploadView)
			assert.Contains(t, text, "  last upload attempt: "+tc.line+"\n")
			assert.Equal(t, tc.data, data["uploadLastAttempt"])
		})
	}

	t.Run("no attempt yet", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		text, data, warnings := statusOf(t, dir, uploadView)
		assert.Contains(t, text, "  last upload attempt: no attempt yet\n")
		assert.Contains(t, text, "  next upload attempt: at the next recorded run\n")
		assert.Contains(t, text, "  events waiting: 0\n")
		assert.Contains(t, text, "  events of the current hour: 0 (sent after the hour ends)\n")
		assert.Nil(t, data["uploadLastAttempt"])
		assert.Contains(t, data, "uploadNextAttempt")
		assert.Nil(t, data["uploadNextAttempt"])
		assert.Equal(t, 0, data["uploadEventsWaiting"])
		assert.Equal(t, false, data["uploadEventsWaitingLimitReached"])
		assert.Equal(t, 0, data["uploadEventsCurrentHour"])
		assert.Empty(t, warnings)
	})

	t.Run("the next attempt is due or in the future", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, NextAttempt: time.Now().Add(-time.Minute)})
		text, data, _ := statusOf(t, dir, uploadView)
		assert.Contains(t, text, "  next upload attempt: at the next recorded run\n")
		assert.Nil(t, data["uploadNextAttempt"])

		next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, NextAttempt: next})
		text, data, _ = statusOf(t, dir, uploadView)
		assert.Contains(t, text, "  next upload attempt: after "+next.Format(time.RFC3339)+"\n")
		assert.Equal(t, next.Format(time.RFC3339), data["uploadNextAttempt"])

		far := time.Now().Add(30 * time.Hour)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, NextAttempt: far})
		text, _, _ = statusOf(t, dir, uploadView)
		assert.Contains(t, text, "  next upload attempt: at the next recorded run\n", "a next attempt further ahead than any wait counts as due")
	})

	t.Run("events waiting leave out the covered ids", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 3)
		putSlots(t, dir, 0, 100, 2)
		text, data, _ := statusOf(t, dir, uploadView)
		assert.Contains(t, text, "  events waiting: 3\n")
		assert.Contains(t, text, "  events of the current hour: 2 (sent after the hour ends)\n")
		assert.Equal(t, 3, data["uploadEventsWaiting"])
		assert.Equal(t, 2, data["uploadEventsCurrentHour"])

		putState(t, dir, telemetry.UploadState{LastAttempt: at, CoveredIDs: []string{uidOf(1), uidOf(2)}})
		text, data, _ = statusOf(t, dir, uploadView)
		assert.Contains(t, text, "  events waiting: 1\n")
		assert.Equal(t, 1, data["uploadEventsWaiting"])
	})

	for _, tc := range []struct {
		name    string
		events  int
		reached bool
	}{
		{"below the count limit", telemetry.CountLimit - 1, false},
		{"at the count limit", telemetry.CountLimit, true},
		{"over the count limit", telemetry.CountLimit + 1, true},
	} {
		t.Run("events waiting "+tc.name, func(t *testing.T) {
			controlsEnv(t)
			dir := newDir(t)
			putCompacted(t, dir, 3, 1, tc.events)
			text, data, _ := statusOf(t, dir, uploadView)
			assert.Equal(t, min(tc.events, telemetry.CountLimit), data["uploadEventsWaiting"])
			assert.Equal(t, tc.reached, data["uploadEventsWaitingLimitReached"])
			assert.Equal(t, tc.reached, strings.Contains(text, "  events waiting: 10000 (counting stops at 10000)\n"), text)
		})
	}

	t.Run("an unreadable lock", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		lock := filepath.Join(dir, "telemetry", "upload", "lock")
		testfs.DirectoryInPlaceOfFile(t, lock)
		_, _, warnings := statusOf(t, dir, uploadView)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], lock)
		assert.Contains(t, warnings[0], "uploads are skipped until it is removed")
		assert.Contains(t, warnings[0], clierr.Rancher+" telemetry reset")
	})

	t.Run("a plain file in place of telemetry upload", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		upload := filepath.Join(dir, "telemetry", "upload")
		require.NoError(t, os.WriteFile(upload, []byte("x"), 0o600))
		_, _, warnings := statusOf(t, dir, uploadView)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], upload)
		assert.Contains(t, warnings[0], "uploads are skipped until it is removed")
	})

	t.Run("a directory in place of the state file", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		state := filepath.Join(dir, "telemetry", "upload", "state.json")
		testfs.DirectoryInPlaceOfFile(t, state)
		_, _, warnings := statusOf(t, dir, uploadView)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], state)
		assert.Contains(t, warnings[0], "not limited to once an hour")
		assert.Contains(t, warnings[0], clierr.Rancher+" telemetry reset")
	})

	t.Run("a symlink in place of the state file", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		putFile(t, dir, "telemetry/upload/elsewhere.json", "{}")
		state := filepath.Join(dir, "telemetry", "upload", "state.json")
		testfs.Symlink(t, filepath.Join(dir, "telemetry", "upload", "elsewhere.json"), state)
		_, _, warnings := statusOf(t, dir, uploadView)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], state)
		assert.Contains(t, warnings[0], "not limited to once an hour")
	})

	t.Run("a directory in place of the state file below a symlinked upload directory", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		target := filepath.Join(dir, "elsewhere")
		testfs.DirectoryInPlaceOfFile(t, filepath.Join(target, "state.json"))
		testfs.Symlink(t, target, filepath.Join(dir, "telemetry", "upload"))
		_, _, warnings := statusOf(t, dir, uploadView)
		require.Len(t, warnings, 1, "only the unsafe directory is named")
		assert.Contains(t, warnings[0], "is a symlink, not a directory")
	})

	t.Run("a telemetry directory that cannot be searched names the upload directory", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, HTTPStatus: 200})
		testfs.RestrictAccess(t, filepath.Join(dir, "telemetry"), 0o600)
		_, _, warnings := statusOf(t, dir, uploadView)
		upload := filepath.Join(dir, "telemetry", "upload")
		var named []string
		for _, w := range warnings {
			if strings.HasPrefix(w, upload+" ") {
				named = append(named, w)
			}
		}
		require.Len(t, named, 1, warnings)
		assert.Contains(t, named[0], "cannot be read")
		assert.NotContains(t, strings.Join(warnings, "\n"), filepath.Join(upload, "lock"), "the lock file is not blamed")
	})

	t.Run("a held lock adds no warning", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 1)
		putFile(t, dir, "telemetry/upload/lock", time.Now().UTC().Format(time.RFC3339Nano)+" 0123456789abcdef")
		_, _, warnings := statusOf(t, dir, uploadView)
		assert.Empty(t, warnings)
	})

	t.Run("the bare name", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 2)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, HTTPStatus: 200, Sent: 1})
		bare := telemetry.ExperimentView{Names: []string{"telemetryupload"}}
		text, data, _ := statusOf(t, dir, bare)
		assert.Contains(t, text, "  last upload attempt: "+atText)
		assert.NotContains(t, text, "next upload attempt")
		assert.NotContains(t, text, "events waiting")
		assert.NotContains(t, text, "events of the current hour")
		for _, key := range []string{"uploadNextAttempt", "uploadEventsWaiting", "uploadEventsWaitingLimitReached", "uploadEventsCurrentHour"} {
			assert.NotContains(t, data, key)
		}
	})

	t.Run("the zero view", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 2)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, HTTPStatus: 200})
		text, data, _ := statusOf(t, dir, telemetry.ExperimentView{})
		assert.NotContains(t, text, "upload")
		for key := range data {
			assert.NotContains(t, key, "upload")
		}
	})

	t.Run("status creates nothing", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		putSlots(t, dir, 3, 1, 2)
		putSlots(t, dir, 0, 50, 1)
		putState(t, dir, telemetry.UploadState{LastAttempt: at, HTTPStatus: 200, CoveredIDs: []string{uidOf(1)}})
		before := testfs.Snapshot(t, dir)
		statusOf(t, dir, uploadView)
		assert.Equal(t, before, testfs.Snapshot(t, dir))

		missing := newDir(t)
		statusOf(t, missing, uploadView)
		assert.Equal(t, []string{"missing"}, testfs.Snapshot(t, missing))
	})
}

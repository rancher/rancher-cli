package telemetry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

const goodInstallID = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"

// TestFactsFileNameIsTheURLHashOnly asserts the name is 32 hex characters
// and .json, depends on the canonical URL alone, and holds no text of the
// URL.
func TestFactsFileNameIsTheURLHashOnly(t *testing.T) {
	a := FactsFileName("https://rancher.example.com")
	assert.Regexp(t, `^[0-9a-f]{32}\.json$`, a)
	assert.Equal(t, a, FactsFileName("https://rancher.example.com"))
	assert.NotEqual(t, a, FactsFileName("https://rancher.example.org"))
	assert.NotContains(t, a, "rancher")
	assert.NotContains(t, a, "example")
}

// TestAFailureNeverOverwritesAGoodValue asserts a stored failure sits beside
// the last good version and install id, which stay usable.
func TestAFailureNeverOverwritesAGoodValue(t *testing.T) {
	now := fixedNoon
	var f Facts
	f.SetVersion("v2.15.2", EditionPrime, now)
	f.SetInstallID(goodInstallID, now)
	f.FailVersion(FailureNetwork, now.Add(time.Hour))
	f.FailInstallID(FailureForbidden, now.Add(time.Hour))
	f.SetInstallID("not a uuid", now.Add(2*time.Hour))

	v, e, ok := f.usableVersion(now.Add(3 * time.Hour))
	require.True(t, ok)
	assert.Equal(t, "2.15.2", v)
	assert.Equal(t, EditionPrime, e)
	id, reason := f.usableInstallID(now.Add(3 * time.Hour))
	assert.Equal(t, goodInstallID, id)
	assert.Empty(t, reason)
	assert.Equal(t, FailureInvalid, f.InstallID.Failure.Kind)
}

// TestFactsOlderThanThirtyDaysAreNotUsed asserts the 30 day limit on both
// records, and the reason that follows.
func TestFactsOlderThanThirtyDaysAreNotUsed(t *testing.T) {
	var f Facts
	f.SetVersion("2.15.2", EditionPrime, fixedNoon)
	f.SetInstallID(goodInstallID, fixedNoon)
	_, _, ok := f.usableVersion(fixedNoon.Add(FactsMaxAge))
	assert.True(t, ok, "exactly 30 days is still usable")
	_, _, ok = f.usableVersion(fixedNoon.Add(FactsMaxAge + time.Second))
	assert.False(t, ok)
	id, reason := f.usableInstallID(fixedNoon.Add(31 * 24 * time.Hour))
	assert.Empty(t, id)
	assert.Equal(t, ReasonNotFetched, reason)
}

// TestFactsFillTheEventAndExplainAMissingInstallID asserts Record reads the
// server's facts and puts the version, edition and install id into the
// event, and states the reason when the install id is missing.
func TestFactsFillTheEventAndExplainAMissingInstallID(t *testing.T) {
	enable(t)
	c := useClock(t, fixedNoon)
	const url = "https://rancher.example.com"

	cases := []struct {
		name       string
		facts      func(f *Facts)
		noServer   bool
		wantID     string
		wantReason string
		wantVer    string
	}{
		{name: "no server", noServer: true, wantReason: "no_server"},
		{name: "server without a facts file", wantReason: "not_fetched"},
		{name: "good facts", facts: func(f *Facts) {
			f.SetVersion("v2.15.2", EditionPrime, c.now())
			f.SetInstallID(goodInstallID, c.now())
		}, wantID: goodInstallID, wantVer: "2.15.2"},
		{name: "version only", facts: func(f *Facts) { f.SetVersion("2.15.2", EditionCommunity, c.now()) }, wantReason: "not_fetched", wantVer: "2.15.2"},
		{name: "forbidden", facts: func(f *Facts) { f.FailInstallID(FailureForbidden, c.now()) }, wantReason: "not_permitted"},
		{name: "invalid", facts: func(f *Facts) { f.SetInstallID("cluster-1", c.now()) }, wantReason: "invalid"},
		{name: "network failure", facts: func(f *Facts) { f.FailInstallID(FailureNetwork, c.now()) }, wantReason: "not_fetched"},
		{name: "stale", facts: func(f *Facts) {
			f.SetVersion("2.15.2", EditionPrime, c.now().Add(-40*24*time.Hour))
			f.SetInstallID(goodInstallID, c.now().Add(-40*24*time.Hour))
		}, wantReason: "not_fetched"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		ResetServerForTest()
		if !tc.noServer {
			SetServer(url, "")
		}
		if tc.facts != nil {
			s := openStore(t, dir)
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry"), 0o700))
			var f Facts
			tc.facts(&f)
			require.NoError(t, s.WriteFacts(url, f))
		}
		require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)), tc.name)
		list, err := openStore(t, dir).List()
		require.NoError(t, err)
		require.Len(t, list, 1, tc.name)
		fields := list[0].Fields
		if tc.wantID != "" {
			assert.Equal(t, tc.wantID, fields[attrInstallID], tc.name)
			assert.NotContains(t, fields, attrMissingReason, tc.name)
		} else {
			assert.NotContains(t, fields, attrInstallID, tc.name)
			assert.Equal(t, tc.wantReason, fields[attrMissingReason], tc.name)
		}
		if tc.wantVer != "" {
			assert.Equal(t, tc.wantVer, fields[attrManagerVersion], tc.name)
			assert.NotEmpty(t, fields[attrManagerEdition], tc.name)
		} else {
			assert.NotContains(t, fields, attrManagerVersion, tc.name)
		}
	}
}

// TestFactsWriteIsAtomicAndOwnerOnly asserts a facts file is written through
// a temporary name and a rename: it round-trips, leaves no temporary file,
// is owner-only, and concurrent writers leave one valid file.
func TestFactsWriteIsAtomicAndOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	const url = "https://rancher.example.com"
	var f Facts
	f.SetVersion("2.15.2", EditionPrime, fixedNoon)
	f.SetInstallID(goodInstallID, fixedNoon)
	require.NoError(t, s.WriteFacts(url, f))

	got, ok := s.ReadFacts(url)
	require.True(t, ok)
	assert.Equal(t, f.Version.Value, got.Version.Value)
	assert.Equal(t, goodInstallID, got.InstallID.Value)
	assert.True(t, got.UpdatedAt.Equal(fixedNoon))
	assert.Equal(t, []string{FactsFileName(url)}, readNames(t, filepath.Join(dir, "telemetry", "servers")))
	if !testfs.OnWindows() {
		info, err := os.Stat(filepath.Join(dir, "telemetry", "servers", FactsFileName(url)))
		require.NoError(t, err)
		assert.EqualValues(t, 0o600, info.Mode().Perm())
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			var w Facts
			w.SetVersion("2.15."+string(rune('0'+i)), EditionCommunity, fixedNoon)
			_ = s.WriteFacts(url, w)
		})
	}
	wg.Wait()
	final, ok := s.ReadFacts(url)
	require.True(t, ok, "one valid file after concurrent writers")
	assert.Regexp(t, `^2\.15\.[0-7]$`, final.Version.Value)
	assert.Len(t, readNames(t, filepath.Join(dir, "telemetry", "servers")), 1, "no temporary file left")

	_, ok = s.ReadFacts("https://other.example.com")
	assert.False(t, ok)
}

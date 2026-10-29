package telemetry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerRow is one answer of the outcome table and what each record stores.
type answerRow struct {
	name   string
	answer func(value string) Answer
	// kind is the stored failure; "" with stored true is a success.
	kind   FailureKind
	stored bool
}

func status(code int) func(string) Answer {
	return func(string) Answer { return Answer{Status: code} }
}

func withValue(code int) func(string) Answer {
	return func(v string) Answer { return Answer{Status: code, Value: v, Edition: EditionPrime} }
}

var answerRows = []answerRow{
	{name: "200 with a value", answer: withValue(200), stored: true},
	{name: "200 with no value", answer: status(200), stored: true, kind: FailureInvalid},
	{name: "204", answer: status(204), stored: true, kind: FailureInvalid},
	{name: "an unparsable body", answer: status(200), stored: true, kind: FailureInvalid},
	{name: "a body over the limit", answer: status(200), stored: true, kind: FailureInvalid},
	{name: "202 with a value", answer: withValue(202), stored: true, kind: FailureInvalid},
	{name: "403", answer: status(403), stored: true, kind: FailureForbidden},
	{name: "404", answer: status(404), stored: true, kind: FailureForbidden},
	{name: "401", answer: status(401), stored: true, kind: FailureNetwork},
	{name: "400", answer: status(400), stored: true, kind: FailureNetwork},
	{name: "429", answer: status(429), stored: true, kind: FailureNetwork},
	{name: "302", answer: status(302), stored: true, kind: FailureNetwork},
	{name: "500", answer: status(500), stored: true, kind: FailureNetwork},
	{name: "no response", answer: status(0), stored: true, kind: FailureNetwork},
	{name: "cancelled", answer: func(string) Answer { return Answer{Cancelled: true, Status: 200, Value: "v2.15.2"} }},
}

// TestTheOutcomeTableDecidesWhatEachRecordStores asserts, for each answer of
// the table, the stored result, that a failure keeps the last good value and
// a cancelled request changes nothing, and what the next event takes from
// the record: the version, or the install id and the reason it is missing.
func TestTheOutcomeTableDecidesWhatEachRecordStores(t *testing.T) {
	for _, row := range answerRows {
		for _, prior := range []bool{false, true} {
			name := row.name + " with no prior value"
			if prior {
				name = row.name + " after a good value"
			}
			t.Run(name, func(t *testing.T) {
				var f Facts
				if prior {
					f.SetVersion("v2.14.0", EditionCommunity, fixedNoon.Add(-48*time.Hour))
					f.SetInstallID(goodInstallID, fixedNoon.Add(-48*time.Hour))
				}
				before := f
				now := fixedNoon

				changed := f.StoreVersion(row.answer("v2.15.2"), now)
				assert.Equal(t, row.stored, changed)
				v, e, ok := f.usableVersion(now)
				switch {
				case !row.stored:
					assert.Equal(t, before.Version, f.Version, "a cancelled request changes nothing")
				case row.kind == "":
					assert.True(t, ok)
					assert.Equal(t, "2.15.2", v)
					assert.Equal(t, EditionPrime, e)
					assert.Nil(t, f.Version.Failure, "a success replaces the failure")
				default:
					require.NotNil(t, f.Version.Failure)
					assert.Equal(t, row.kind, f.Version.Failure.Kind)
					assert.Equal(t, now, f.Version.Failure.At)
					assert.Equal(t, prior, ok, "the last good version stays")
					if prior {
						assert.Equal(t, "2.14.0", v)
					}
				}

				changed = f.StoreInstallID(row.answer(goodInstallID), now)
				assert.Equal(t, row.stored, changed)
				id, reason := f.usableInstallID(now)
				switch {
				case !row.stored:
					assert.Equal(t, before.InstallID, f.InstallID)
				case row.kind == "":
					assert.Equal(t, goodInstallID, id)
					assert.Empty(t, reason)
				case prior:
					assert.Equal(t, goodInstallID, id, "the last good install id stays")
				default:
					assert.Empty(t, id)
					assert.Equal(t, map[FailureKind]MissingReason{
						FailureInvalid:   ReasonInvalid,
						FailureForbidden: ReasonNotPermitted,
						FailureNetwork:   ReasonNotFetched,
					}[row.kind], reason)
				}
				if row.stored && !prior {
					assert.Equal(t, now, f.UpdatedAt)
				}
			})
		}
	}
}

// TestAnAnswerOfAnotherKindIsHandledByTheValueRules asserts a version the
// version rule turns into _OTHER is a success that the next event reports as
// _OTHER, and an install id that is not a UUID is an invalid failure whose
// reason the next event reports.
func TestAnAnswerOfAnotherKindIsHandledByTheValueRules(t *testing.T) {
	var f Facts
	assert.True(t, f.StoreVersion(Answer{Status: 200, Value: "v2.15.2-acme-corp", Edition: EditionCommunity}, fixedNoon))
	v, e, ok := f.usableVersion(fixedNoon)
	require.True(t, ok)
	assert.Equal(t, Other, v)
	assert.Equal(t, EditionCommunity, e)
	assert.Nil(t, f.Version.Failure)

	assert.True(t, f.StoreInstallID(Answer{Status: 200, Value: "not-a-uuid"}, fixedNoon))
	id, reason := f.usableInstallID(fixedNoon)
	assert.Empty(t, id)
	assert.Equal(t, ReasonInvalid, reason)
	require.NotNil(t, f.InstallID.Failure)
	assert.Equal(t, FailureInvalid, f.InstallID.Failure.Kind)
}

// TestARecordIsDueAtEachBoundary asserts both records: nothing stored is due,
// a stored time after now is due, and a record waits 24 hours after a success
// or an answer it cannot use, 6 hours after forbidden and 1 hour after a
// network failure, from the time of that answer.
func TestARecordIsDueAtEachBoundary(t *testing.T) {
	rows := []struct {
		name  string
		set   func(f *Facts, at time.Time)
		after time.Duration
	}{
		{"success", func(f *Facts, at time.Time) {
			f.SetVersion("v2.15.2", EditionPrime, at)
			f.SetInstallID(goodInstallID, at)
		}, 24 * time.Hour},
		{"invalid", func(f *Facts, at time.Time) {
			f.FailVersion(FailureInvalid, at)
			f.FailInstallID(FailureInvalid, at)
		}, 24 * time.Hour},
		{"forbidden", func(f *Facts, at time.Time) {
			f.FailVersion(FailureForbidden, at)
			f.FailInstallID(FailureForbidden, at)
		}, 6 * time.Hour},
		{"network", func(f *Facts, at time.Time) {
			f.FailVersion(FailureNetwork, at)
			f.FailInstallID(FailureNetwork, at)
		}, time.Hour},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var f Facts
			row.set(&f, fixedNoon)
			for _, due := range []func(Facts, time.Time) bool{Facts.VersionDue, Facts.InstallIDDue} {
				assert.False(t, due(f, fixedNoon.Add(row.after-time.Minute)), "one minute short")
				assert.True(t, due(f, fixedNoon.Add(row.after)), "at the boundary")
				assert.True(t, due(f, fixedNoon.Add(row.after+time.Minute)))
				assert.True(t, due(f, fixedNoon.Add(-time.Minute)), "a stored time after now: a clock set back")
			}
		})
	}
	var empty Facts
	assert.True(t, empty.VersionDue(fixedNoon))
	assert.True(t, empty.InstallIDDue(fixedNoon))
}

// TestAFailureWaitsFromItsOwnTimeBesideAGoodValue asserts a failure stored
// beside a good value starts its own wait, and a success after it starts the
// 24 hours again.
func TestAFailureWaitsFromItsOwnTimeBesideAGoodValue(t *testing.T) {
	var f Facts
	f.SetVersion("v2.15.2", EditionPrime, fixedNoon.Add(-30*time.Hour))
	require.True(t, f.VersionDue(fixedNoon))
	f.FailVersion(FailureNetwork, fixedNoon.Add(-30*time.Minute))
	assert.False(t, f.VersionDue(fixedNoon), "the failure 30 minutes ago waits an hour")
	assert.True(t, f.VersionDue(fixedNoon.Add(30*time.Minute)))
	f.SetVersion("v2.15.2", EditionPrime, fixedNoon)
	assert.False(t, f.VersionDue(fixedNoon.Add(23*time.Hour)))
}

// TestSaveFactsWritesUnderTheRecordingGates asserts the write function
// writes the facts file of a server when recording is on and the directory
// existed, and writes and creates nothing when a control is off, when the
// caller saw no directory, when the directory no longer exists, or when the
// telemetry path is a symlink to a directory outside the config directory.
func TestSaveFactsWritesUnderTheRecordingGates(t *testing.T) {
	enable(t)
	const server = "https://rancher.example.com"
	var f Facts
	f.SetVersion("v2.15.2", EditionPrime, fixedNoon)
	file := filepath.Join("telemetry", "servers", FactsFileName(server))

	t.Run("on and the directory existed", func(t *testing.T) {
		dir := t.TempDir()
		assert.True(t, SaveFacts(server, f, Options{ConfigDir: dir, DirExisted: true}))
		s, err := OpenStore(dir)
		require.NoError(t, err)
		defer func() { _ = s.Close() }()
		got, ok := s.ReadFacts(server)
		require.True(t, ok)
		assert.Equal(t, "2.15.2", got.Version.Value)
		assert.FileExists(t, filepath.Join(dir, file))
	})
	t.Run("a variable turns recording off", func(t *testing.T) {
		dir := t.TempDir()
		assert.False(t, SaveFacts(server, f, Options{ConfigDir: dir, DirExisted: true, Getenv: envOf(map[string]string{EnvTelemetry: "false"})}))
		assert.Empty(t, readNames(t, dir))
	})
	t.Run("the config key turns recording off", func(t *testing.T) {
		dir := t.TempDir()
		assert.False(t, SaveFacts(server, f, Options{ConfigDir: dir, DirExisted: true, Config: ConfigOff}))
		assert.Empty(t, readNames(t, dir))
	})
	t.Run("the directory did not exist at the start", func(t *testing.T) {
		dir := t.TempDir()
		assert.False(t, SaveFacts(server, f, Options{ConfigDir: dir}))
		assert.Empty(t, readNames(t, dir))
	})
	t.Run("the directory no longer exists", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "gone")
		assert.False(t, SaveFacts(server, f, Options{ConfigDir: missing, DirExisted: true}))
		_, err := os.Stat(missing)
		assert.ErrorIs(t, err, os.ErrNotExist, "the config directory is never created")
	})
	t.Run("the telemetry path is a symlink", func(t *testing.T) {
		dir, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "telemetry")); err != nil {
			t.Skipf("symlinks are not available: %v", err)
		}
		assert.False(t, SaveFacts(server, f, Options{ConfigDir: dir, DirExisted: true}))
		assert.Empty(t, readNames(t, outside))
	})
}

// TestRecordHandsBackWhatItsEventRead asserts Record reports the facts its
// event read and whether they can start a write: a file that was read, a file
// that is missing and one that holds no facts are usable; a read that failed
// for another reason, a run with no server and a run that records nothing
// are not.
func TestRecordHandsBackWhatItsEventRead(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	const server = "https://rancher.example.com"
	var stored Facts
	stored.SetVersion("v2.15.2", EditionPrime, fixedNoon)
	servers := func(dir string) string { return filepath.Join(dir, "telemetry", "servers") }
	factsPath := func(dir string) string { return filepath.Join(servers(dir), FactsFileName(server)) }

	t.Run("a file that was read", func(t *testing.T) {
		dir := t.TempDir()
		require.True(t, SaveFacts(server, stored, Options{ConfigDir: dir, DirExisted: true}))
		SetServer(server, "")
		st, read := Record(runEvent(fixedNoon), Options{ConfigDir: dir, DirExisted: true})
		assert.Equal(t, StatusRecorded, st)
		assert.True(t, read.Usable)
		assert.Equal(t, "2.15.2", read.Facts.Version.Value)
	})
	t.Run("a missing file", func(t *testing.T) {
		SetServer(server, "")
		_, read := Record(runEvent(fixedNoon), Options{ConfigDir: t.TempDir(), DirExisted: true})
		assert.True(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
	})
	t.Run("a file that holds no facts", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(servers(dir), 0o700))
		require.NoError(t, os.WriteFile(factsPath(dir), []byte("{not json"), 0o600))
		SetServer(server, "")
		_, read := Record(runEvent(fixedNoon), Options{ConfigDir: dir, DirExisted: true})
		assert.True(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
	})
	t.Run("a read that failed", func(t *testing.T) {
		// A directory where the facts file belongs fails the read with an
		// error other than not-exist on every platform; a plain file in place
		// of the servers directory reads as not-exist on Windows.
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(factsPath(dir), 0o700))
		SetServer(server, "")
		st, read := Record(runEvent(fixedNoon), Options{ConfigDir: dir, DirExisted: true})
		assert.Equal(t, StatusRecorded, st)
		assert.False(t, read.Usable)
	})
	t.Run("no server", func(t *testing.T) {
		ResetServerForTest()
		_, read := Record(runEvent(fixedNoon), Options{ConfigDir: t.TempDir(), DirExisted: true})
		assert.False(t, read.Usable)
	})
	t.Run("recording off", func(t *testing.T) {
		SetServer(server, "")
		st, read := Record(runEvent(fixedNoon), Options{ConfigDir: t.TempDir(), DirExisted: true, Getenv: envOf(map[string]string{EnvTelemetry: "false"})})
		assert.Equal(t, StatusOff, st)
		assert.False(t, read.Usable)
	})
}

// TestTheInstallIDRecordSkipsAStoredFailureWaitOnly asserts the due rule of a
// command that stored the token the request carries: a stored failure of any
// kind is due at once, a time after now is due, and the 24 hours after a
// good value, a value 23 hours 59 minutes old included, and nothing stored
// keep their rule. The version record follows its own wait in every row.
func TestTheInstallIDRecordSkipsAStoredFailureWaitOnly(t *testing.T) {
	failureAgo := func(kind FailureKind, ago time.Duration) func(*Facts) {
		return func(f *Facts) {
			f.FailInstallID(kind, fixedNoon.Add(-ago))
			f.FailVersion(kind, fixedNoon.Add(-ago))
		}
	}
	valueAgo := func(ago time.Duration) func(*Facts) {
		return func(f *Facts) {
			f.SetInstallID(goodInstallID, fixedNoon.Add(-ago))
			f.SetVersion("v2.15.2", EditionPrime, fixedNoon.Add(-ago))
		}
	}
	rows := []struct {
		name                  string
		set                   func(*Facts)
		withWait, withoutWait bool
		versionDue            bool
	}{
		{"forbidden 1 hour ago", failureAgo(FailureForbidden, time.Hour), false, true, false},
		{"network 10 minutes ago", failureAgo(FailureNetwork, 10*time.Minute), false, true, false},
		{"invalid 1 hour ago", failureAgo(FailureInvalid, time.Hour), false, true, false},
		{"a failure with a time after now", failureAgo(FailureForbidden, -time.Hour), true, true, true},
		{"a value 1 hour ago", valueAgo(time.Hour), false, false, false},
		{"a value 23 hours 59 minutes ago", valueAgo(24*time.Hour - time.Minute), false, false, false},
		{"a value 24 hours ago", valueAgo(24 * time.Hour), true, true, true},
		{"a value 25 hours ago", valueAgo(25 * time.Hour), true, true, true},
		{"nothing stored", func(*Facts) {}, true, true, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var f Facts
			row.set(&f)
			assert.Equal(t, row.withWait, f.InstallIDDue(fixedNoon), "the root client's rule")
			assert.Equal(t, row.withoutWait, f.InstallIDDueSkippingFailureWait(fixedNoon), "the rule of a command that stored the token")
			assert.Equal(t, row.versionDue, f.VersionDue(fixedNoon), "the version record keeps its schedule")
		})
	}
}

// TestReadServerFactsReportsWhatItCouldRead asserts one row per case of the
// read: no telemetry directory, no facts file and a file that holds no facts
// are usable and empty, a good file is usable with its facts; the facts file
// path made a directory, off controls, no directory seen, a config directory
// that does not exist, a telemetry symlink, a telemetry plain file and a
// panic in the read are not usable, and a read that is not allowed touches
// no file.
func TestReadServerFactsReportsWhatItCouldRead(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	const server = "https://rancher.example.com"
	var stored Facts
	stored.SetVersion("v2.15.2", EditionPrime, fixedNoon)
	stored.SetInstallID(goodInstallID, fixedNoon)
	factsPath := func(dir string) string {
		return filepath.Join(dir, "telemetry", "servers", FactsFileName(server))
	}
	seed := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		require.True(t, SaveFacts(server, stored, Options{ConfigDir: dir, DirExisted: true}))
		return dir
	}
	existed := func(dir string) Options { return Options{ConfigDir: dir, DirExisted: true} }

	t.Run("no telemetry directory", func(t *testing.T) {
		dir := t.TempDir()
		ops := observeOps(t)
		read := ReadServerFacts(server, existed(dir))
		assert.True(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
		assert.Equal(t, []string{"openroot .", "lstat telemetry"}, ops.take())
		assert.Empty(t, readNames(t, dir), "nothing is created")
	})
	t.Run("no facts file", func(t *testing.T) {
		dir := seed(t)
		require.NoError(t, os.Remove(factsPath(dir)))
		read := ReadServerFacts(server, existed(dir))
		assert.True(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
	})
	t.Run("an unparsable file", func(t *testing.T) {
		dir := seed(t)
		require.NoError(t, os.WriteFile(factsPath(dir), []byte("{not json"), 0o600))
		read := ReadServerFacts(server, existed(dir))
		assert.True(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
	})
	t.Run("a good file", func(t *testing.T) {
		read := ReadServerFacts(server, existed(seed(t)))
		assert.True(t, read.Usable)
		assert.Equal(t, "2.15.2", read.Facts.Version.Value)
		assert.Equal(t, goodInstallID, read.Facts.InstallID.Value)
	})
	t.Run("the facts file path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(factsPath(dir), 0o700))
		assert.False(t, ReadServerFacts(server, existed(dir)).Usable)
	})
	t.Run("controls off", func(t *testing.T) {
		dir := seed(t)
		ops := observeOps(t)
		opts := existed(dir)
		opts.Getenv = envOf(map[string]string{EnvTelemetry: "false"})
		read := ReadServerFacts(server, opts)
		assert.False(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
		assert.Empty(t, ops.take())
	})
	t.Run("the directory was not seen", func(t *testing.T) {
		dir := seed(t)
		ops := observeOps(t)
		read := ReadServerFacts(server, Options{ConfigDir: dir})
		assert.False(t, read.Usable)
		assert.Empty(t, ops.take())
	})
	t.Run("a config directory that does not exist", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "gone")
		assert.False(t, ReadServerFacts(server, existed(missing)).Usable)
		_, err := os.Stat(missing)
		assert.ErrorIs(t, err, os.ErrNotExist, "the config directory is never created")
	})
	t.Run("the telemetry path is a symlink", func(t *testing.T) {
		dir, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "telemetry")); err != nil {
			t.Skipf("symlinks are not available: %v", err)
		}
		assert.False(t, ReadServerFacts(server, existed(dir)).Usable)
	})
	t.Run("the telemetry path is a plain file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry"), []byte("x"), 0o600))
		assert.False(t, ReadServerFacts(server, existed(dir)).Usable)
	})
	t.Run("a panic in the read", func(t *testing.T) {
		dir := seed(t)
		readHook = func(string) error { panic("read failed") }
		t.Cleanup(func() { readHook = nil })
		var read FactsRead
		require.NotPanics(t, func() { read = ReadServerFacts(server, existed(dir)) })
		assert.False(t, read.Usable)
		assert.Equal(t, Facts{}, read.Facts)
	})
}

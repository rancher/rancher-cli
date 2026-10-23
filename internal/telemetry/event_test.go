package telemetry

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/invocation"
)

var testCodes = []string{"RBAC", "NOT_FOUND", "GENERIC", "INTERRUPTED"}

func baseInput() RunInput {
	return RunInput{
		Time:       time.Date(2026, 10, 23, 14, 37, 12, 0, time.UTC),
		Invocation: invocation.Context{Version: "0.1.0-alpha.8", OS: "linux", Arch: "amd64"},
		Command:    "get project",
		Outcome:    OutcomeOK,
		ErrorCodes: testCodes,
	}
}

func decodeEvent(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func schemaValidator(t *testing.T) validator {
	t.Helper()
	return loadSchema(t, versionDir+"/"+SchemaFile)
}

// TestFlatEventsPassTheSchema asserts every event the constructors write,
// across the cases that change which keys appear, is valid against the
// committed schema.
func TestFlatEventsPassTheSchema(t *testing.T) {
	v := schemaValidator(t)
	withInstall := baseInput()
	withInstall.InstallID = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
	withInstall.ManagerVersion = "v2.15.2"
	withInstall.Edition = EditionPrime
	failed := baseInput()
	failed.Outcome = OutcomeError
	failed.ErrorType = "RBAC"
	failed.ExitCode, failed.ExitCodeSet = 6, true
	agent := baseInput()
	agent.Invocation.Agent = "claude-code"
	agent.Invocation.CI = true
	plugin := baseInput()
	plugin.Invocation.ExecPlugin = true
	plugin.Command = "auth get-token"
	plugin.Flags = []string{"server", "user-id"}
	hostile := baseInput()
	hostile.Command = "get Customer-Prod"
	hostile.Flags = []string{"output=json", "x"}
	hostile.InstallID = "not-a-uuid"
	hostile.ManagerVersion = "2.13.4-hotfix-acme"
	hostile.Invocation.Version = "0.1.0+dirty"
	hostile.Outcome = OutcomeError
	hostile.ErrorType = "customer text"
	hostile.ExitCode, hostile.ExitCodeSet = 999, true
	for name, in := range map[string]RunInput{"bare": baseInput(), "install id": withInstall, "failed": failed, "agent": agent, "plugin": plugin, "hostile": hostile} {
		ev := NewCommandRun(in).withDirectoryID(testUUID())
		assert.Empty(t, v.errors(decodeJSON(t, ev.Flat())), name)
		assert.Empty(t, v.errors(decodeJSON(t, ev.stored())), name+" stored")
	}
	dropped := NewEventsDropped(time.Date(2026, 10, 23, 13, 0, 0, 0, time.UTC), 3, baseInput().Invocation)
	assert.Empty(t, v.errors(decodeJSON(t, dropped.Flat())))
}

// listedKeys returns the keys the schema lists for an event.
func listedKeys(t *testing.T, event string) []string {
	t.Helper()
	v := schemaValidator(t)
	defs := v.root["$defs"].(map[string]any)
	props := defs[event].(map[string]any)["properties"].(map[string]any)
	var keys []string
	for k := range props {
		keys = append(keys, k)
	}
	return keys
}

// strictFindings returns the keys of an encoded event that the schema does
// not list for the event, after removing local_format, which only the
// stored form carries and the schema never lists. An extra key still fails.
func strictFindings(t *testing.T, event string, data []byte) []string {
	t.Helper()
	listed := listedKeys(t, event)
	got := decodeEvent(t, data)
	delete(got, keyLocalFormat)
	var out []string
	for k := range got {
		if !slices.Contains(listed, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// TestEncoderWritesOnlyListedFields asserts every key of every encoded
// event, in the contract form and the stored form, is one the schema lists,
// and that the schema does not list local_format.
func TestEncoderWritesOnlyListedFields(t *testing.T) {
	for _, event := range []string{EventCommandRun, EventEventsDropped} {
		keys := listedKeys(t, event)
		assert.NotContains(t, keys, keyLocalFormat, "the storage-only key must not be in the schema")
		assert.NotContains(t, SortedNames(event), keyLocalFormat, "the storage-only key must not be an attribute")
		assert.ElementsMatch(t, append([]string{keyEventName, keyTime, keySchemaVersion}, SortedNames(event)...), keys, event)
	}
	in := baseInput()
	in.Invocation.Agent = "codex"
	in.InstallID = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
	in.ManagerVersion = "2.15.2"
	in.Edition = EditionCommunity
	in.Outcome = OutcomeError
	in.ErrorType = "RBAC"
	in.ExitCode, in.ExitCodeSet = 6, true
	in.Flags = []string{"output"}
	runs := []Event{NewCommandRun(in).withDirectoryID(testUUID()), NewCommandRun(baseInput())}
	dropped := NewEventsDropped(in.Time, 2, in.Invocation)
	for _, ev := range append(runs, dropped) {
		assert.Empty(t, strictFindings(t, ev.Name(), ev.Flat()), "%s writes a key the schema does not list", ev.Name())
		assert.Empty(t, strictFindings(t, ev.Name(), ev.stored()), "%s stored", ev.Name())
		flat := decodeEvent(t, ev.Flat())
		stored := decodeEvent(t, ev.stored())
		assert.NotContains(t, flat, keyLocalFormat)
		assert.EqualValues(t, LocalFormat, stored[keyLocalFormat])
		delete(stored, keyLocalFormat)
		assert.Equal(t, flat, stored, "the stored form is the contract form plus local_format")
	}
}

// TestStrictCheckRejectsAnUnlistedKey asserts the strict check removes only
// local_format and reports any other key.
func TestStrictCheckRejectsAnUnlistedKey(t *testing.T) {
	ev := NewCommandRun(baseInput())
	with := func(key string) []byte {
		m := decodeEvent(t, ev.stored())
		m[key] = "x"
		data, err := json.Marshal(m)
		require.NoError(t, err)
		return data
	}
	assert.Empty(t, strictFindings(t, EventCommandRun, with(keyLocalFormat)))
	assert.Equal(t, []string{"customer.name"}, strictFindings(t, EventCommandRun, with("customer.name")))
	assert.Equal(t, []string{attrDroppedCount}, strictFindings(t, EventCommandRun, with(attrDroppedCount)), "an attribute of another event is not listed here")
}

// TestRequiredCoreIsInEveryEvent asserts the required core is present in
// every event, and that the metadata marks exactly the attributes the
// schema requires.
func TestRequiredCoreIsInEveryEvent(t *testing.T) {
	core := []string{keyEventName, keyTime, keySchemaVersion, attrServiceName, attrServiceVersion, attrInstanceID, attrRecordUID}
	for _, ev := range []Event{NewCommandRun(RunInput{}), NewEventsDropped(time.Time{}, 0, invocation.Context{})} {
		got := decodeEvent(t, ev.Flat())
		for _, k := range core {
			assert.Contains(t, got, k, "%s lacks %s", ev.Name(), k)
		}
	}
	v := schemaValidator(t)
	for _, event := range []string{EventCommandRun, EventEventsDropped} {
		req := v.root["$defs"].(map[string]any)[event].(map[string]any)["required"].([]any)
		var want []string
		want = append(want, keyEventName, keyTime, keySchemaVersion)
		for _, a := range AttributesOf(event) {
			if a.Required {
				want = append(want, a.Name)
			}
		}
		var got []string
		for _, r := range req {
			got = append(got, r.(string))
		}
		assert.Equal(t, want, got, event)
		for _, k := range core {
			assert.Contains(t, got, k)
		}
	}
}

// TestEveryAttributeHasAStabilityMarkerAndMeaning asserts each attribute
// names a stability marker, a standard, a placement, a reason and what
// absent means, and that every attribute is in the encoder's output for
// some input.
func TestEveryAttributeHasAStabilityMarkerAndMeaning(t *testing.T) {
	names := map[string]bool{}
	for _, a := range Attributes() {
		assert.Contains(t, []Stability{StabilityDevelopment, StabilityStable}, a.Stability, a.Name)
		assert.Equal(t, StabilityDevelopment, a.Stability, "%s: every attribute starts as development", a.Name)
		assert.NotEmpty(t, a.Why, a.Name)
		assert.NotEmpty(t, a.Absent, a.Name)
		assert.NotEmpty(t, a.Standard, a.Name)
		assert.Contains(t, []string{PlaceResource, PlaceRecord}, a.Placement, a.Name)
		assert.NotEmpty(t, a.Events, a.Name)
		assert.False(t, names[a.Name], "%s is listed twice", a.Name)
		names[a.Name] = true
	}
	in := baseInput()
	in.Invocation.Agent = "codex"
	in.InstallID = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
	in.ManagerVersion = "2.15.2"
	in.Edition = EditionCommunity
	in.Outcome = OutcomeError
	in.ErrorType = "RBAC"
	in.ExitCode, in.ExitCodeSet = 6, true
	in.Flags = []string{"output"}
	seen := map[string]bool{}
	for _, ev := range []Event{
		NewCommandRun(in).withDirectoryID(testUUID()), NewCommandRun(baseInput()),
		NewEventsDropped(in.Time, 2, in.Invocation).withDirectoryID(testUUID()),
	} {
		for k := range decodeEvent(t, ev.Flat()) {
			seen[k] = true
		}
	}
	for n := range names {
		assert.True(t, seen[n], "%s is never written", n)
	}
}

// TestAbsentMeaningBooleansAndFlags asserts the booleans and the flag list
// are always written, and an unknown value is absent rather than empty.
func TestAbsentMeaningBooleansAndFlags(t *testing.T) {
	got := decodeEvent(t, NewCommandRun(RunInput{}).Flat())
	assert.Equal(t, false, got[attrClientCI])
	assert.Equal(t, false, got[attrExecPlugin])
	assert.Equal(t, []any{}, got[attrFlags])
	for _, k := range []string{attrCommand, attrManagerVersion, attrManagerEdition, attrInstallID, attrClientAgent, attrErrorType, attrConfigDirID, attrOutcome} {
		assert.NotContains(t, got, k, k)
	}
	assert.Equal(t, string(ReasonNotFetched), got[attrMissingReason])
	assert.NotContains(t, got, attrExitCode, "no status given: absent, not 0")
}

// TestExitCodeCarriesPresence asserts zero is a recorded status only when the
// caller says it is set, an unset status is absent whatever the value, and a
// set status outside 0-255 is absent.
func TestExitCodeCarriesPresence(t *testing.T) {
	cases := []struct {
		name string
		in   RunInput
		want any
	}{
		{"zero, set", RunInput{ExitCode: 0, ExitCodeSet: true}, float64(0)},
		{"zero, not set", RunInput{ExitCode: 0}, nil},
		{"value, not set", RunInput{ExitCode: 6}, nil},
		{"value, set", RunInput{ExitCode: 6, ExitCodeSet: true}, float64(6)},
		{"top of the range", RunInput{ExitCode: 255, ExitCodeSet: true}, float64(255)},
		{"above the range", RunInput{ExitCode: 256, ExitCodeSet: true}, nil},
		{"below the range", RunInput{ExitCode: -1, ExitCodeSet: true}, nil},
	}
	for _, c := range cases {
		got := decodeEvent(t, NewCommandRun(c.in).Flat())
		assert.Equal(t, c.want, got[attrExitCode], c.name)
		if c.want == nil {
			assert.NotContains(t, got, attrExitCode, c.name)
		}
	}
}

// TestEventTimeIsAWholeUTCHour asserts the written time is the start of
// the UTC hour whatever the input zone and minutes, in RFC 3339, and that
// the dropped event carries the start of the hour it summarizes.
func TestEventTimeIsAWholeUTCHour(t *testing.T) {
	zone := time.FixedZone("x", 5*3600+1800)
	in := baseInput()
	in.Time = time.Date(2026, 10, 23, 19, 59, 59, 999, zone)
	got := decodeEvent(t, NewCommandRun(in).Flat())
	assert.Equal(t, "2026-10-23T14:00:00Z", got[keyTime])
	for _, ts := range []time.Time{time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), time.Date(2026, 12, 31, 23, 59, 59, 0, zone)} {
		parsed, err := time.Parse(time.RFC3339, decodeEvent(t, NewEventsDropped(ts, 1, in.Invocation).Flat())[keyTime].(string))
		require.NoError(t, err)
		assert.Equal(t, parsed, parsed.Truncate(time.Hour))
		assert.Equal(t, ts.UTC().Truncate(time.Hour), parsed)
	}
	assert.Equal(t, in.Time.UTC().Truncate(time.Hour), NewCommandRun(in).Hour())
}

// TestMissingReasonIsPresentExactlyWhenTheInstallIDIsAbsent asserts the
// four reasons and the present case.
func TestMissingReasonIsPresentExactlyWhenTheInstallIDIsAbsent(t *testing.T) {
	const good = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
	cases := []struct {
		name   string
		id     string
		reason MissingReason
		want   string
		hasID  bool
	}{
		{"present", good, "", "", true},
		{"present with a stale reason", good, ReasonNoServer, "", true},
		{"no server", "", ReasonNoServer, "no_server", false},
		{"not permitted", "", ReasonNotPermitted, "not_permitted", false},
		{"not fetched", "", ReasonNotFetched, "not_fetched", false},
		{"nothing said", "", "", "not_fetched", false},
		{"an unknown reason", "", "because", "not_fetched", false},
		{"invalid id", "cluster-prod-1", ReasonNoServer, "invalid", false},
	}
	for _, c := range cases {
		in := baseInput()
		in.InstallID, in.MissingReason = c.id, c.reason
		got := decodeEvent(t, NewCommandRun(in).Flat())
		if c.hasID {
			assert.Equal(t, good, got[attrInstallID], c.name)
			assert.NotContains(t, got, attrMissingReason, c.name)
			continue
		}
		assert.NotContains(t, got, attrInstallID, c.name)
		assert.Equal(t, c.want, got[attrMissingReason], c.name)
	}
}

// TestNameRuleInTheEncoder asserts the encoder keeps at most 32 flags, the
// first 32 sorted, drops a flag that breaks the rule, and leaves out a
// command name that breaks it.
func TestNameRuleInTheEncoder(t *testing.T) {
	in := baseInput()
	for i := range 40 {
		in.Flags = append(in.Flags, "flag-"+strings.Repeat(string(rune('a'+i%26)), 1+i/26))
	}
	in.Flags = append(in.Flags, "Bad", "also bad", strings.Repeat("z", 33))
	flags := decodeEvent(t, NewCommandRun(in).Flat())[attrFlags].([]any)
	require.Len(t, flags, 32)
	var names []string
	for _, f := range flags {
		names = append(names, f.(string))
	}
	assert.True(t, slices.IsSorted(names))
	in.Command = "Get Project"
	assert.NotContains(t, decodeEvent(t, NewCommandRun(in).Flat()), attrCommand)
}

// TestHostileInputsNeverReachAnEvent asserts text a customer could control
// does not appear anywhere in an encoded event, whichever input carries it.
func TestHostileInputsNeverReachAnEvent(t *testing.T) {
	const secret = "acme-corp-secret"
	in := baseInput()
	in.Invocation.Version = "1.2.3-" + secret
	in.Invocation.OS = secret
	in.Invocation.Arch = secret
	in.Invocation.Agent = secret
	in.InstanceID = secret
	in.Command = "get " + secret + " Name"
	in.Flags = []string{"output=" + secret, secret + "=x", "Output"}
	in.ErrorType = secret
	in.Outcome = OutcomeError
	in.ManagerVersion = "2.13.4-hotfix-" + secret + " x"
	in.InstallID = secret
	in.MissingReason = MissingReason(secret)
	in.Edition = Edition(secret)
	ev := NewCommandRun(in).withDirectoryID(secret)
	assert.NotContains(t, string(ev.Flat()), secret)
	assert.NotContains(t, string(ev.stored()), secret)
	assert.NotContains(t, string(NewEventsDropped(in.Time, 1, in.Invocation).Flat()), secret)
	assert.Empty(t, schemaValidator(t).errors(decodeJSON(t, ev.Flat())))
}

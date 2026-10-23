package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// Event is one event of the contract. Only this package implements it.
// Every value an event holds went through a normalizer or is a closed
// type; an event can be encoded as is.
type Event interface {
	// Name is the event name.
	Name() string
	// UID is the event's log.record.uid.
	UID() string
	// Hour is the event time: the start of the UTC hour.
	Hour() time.Time
	// Flat is the flat contract event, one JSON object, as the schema
	// describes it.
	Flat() []byte

	stored() []byte
	// failed reports that the system random source failed while the event
	// was built; Record drops such an event.
	failed() bool
	producerInfo() producer
	withDirectoryID(id string) Event
	withFacts(known bool, f Facts, now time.Time) Event
}

// producer is who wrote an event: the values of the resource attributes.
type producer struct {
	version  string
	osType   string
	hostArch string
	instance string
}

// producerFor describes the program that writes an event, without a process
// instance id: version, operating system and architecture.
func producerFor(ctx invocation.Context) producer {
	return producer{
		version:  NormalizeVersion(ctx.Version),
		osType:   NormalizeOS(ctx.OS),
		hostArch: NormalizeArch(ctx.Arch),
	}
}

// producerOf describes the process that writes an event. The instance id
// is the one given when it is a version 4 UUID, else the process's own; it
// returns false when the random source failed and no id exists.
func producerOf(ctx invocation.Context, instance string) (producer, bool) {
	p := producerFor(ctx)
	if id, ok := NormalizeRandomID(instance); ok {
		p.instance = id
		return p, true
	}
	id, err := processInstanceID()
	p.instance = id
	return p, err == nil
}

// RunInput is the raw description of one finished command run. NewCommandRun
// puts every field through a normalizer; a value that fails one is left out
// of the event.
type RunInput struct {
	// Time is when the command ended; zero means now. It is truncated to
	// the UTC hour.
	Time time.Time
	// Invocation says how the process was started.
	Invocation invocation.Context
	// InstanceID overrides the process's own id when it is a version 4
	// UUID; anything else uses the process's own.
	InstanceID string
	// Command is the registered command name.
	Command string
	// Flags are the long names of the flags typed on the command line.
	Flags   []string
	Outcome Outcome
	// ErrorType is the CLI error code of a failed command.
	ErrorType string
	// ErrorCodes is every CLI error code; ErrorType not in it is _OTHER.
	ErrorCodes []string
	// ExitCode is the process exit status. It is recorded only when
	// ExitCodeSet is true and the value is from 0 to 255.
	ExitCode    int
	ExitCodeSet bool
	Duration    time.Duration
	// StdoutTTY is true when standard output is a terminal.
	StdoutTTY bool
	// ManagerVersion is the targeted server's version, empty when unknown.
	ManagerVersion string
	Edition        Edition
	// InstallID is the targeted server's install id, empty when unknown.
	InstallID string
	// MissingReason says why InstallID is empty; empty means not_fetched.
	MissingReason MissingReason
}

// CommandRun is the io.rancher.cli.command.run event.
type CommandRun struct {
	broken     bool
	hour       time.Time
	producer   producer
	uid        string
	dirID      string
	clientType ClientType
	agent      string
	ci         bool
	execPlugin bool
	command    string
	flags      []string
	outcome    Outcome
	errorType  string
	exitCode   int
	hasExit    bool
	bucket     Bucket
	version    string
	edition    Edition
	installID  string
	reason     MissingReason
}

// NewCommandRun builds a command run event from raw input.
func NewCommandRun(in RunInput) CommandRun {
	t := in.Time
	if t.IsZero() {
		t = nowFunc()
	}
	var ok bool
	c := CommandRun{
		hour:       t.UTC().Truncate(time.Hour),
		clientType: ClientTypeFor(in.Invocation, in.StdoutTTY),
		ci:         in.Invocation.CI,
		execPlugin: in.Invocation.ExecPlugin,
		flags:      NormalizeFlags(in.Flags),
		bucket:     BucketFor(in.Duration),
		edition:    parseEdition(string(in.Edition)),
	}
	var uidErr error
	c.producer, ok = producerOf(in.Invocation, in.InstanceID)
	c.uid, uidErr = newUUID()
	c.broken = !ok || uidErr != nil
	c.agent, _ = NormalizeAgent(in.Invocation.Agent)
	c.command, _ = NormalizeCommand(in.Command)
	if in.ExitCodeSet {
		c.exitCode, c.hasExit = NormalizeExitCode(in.ExitCode)
	}
	switch in.Outcome {
	case OutcomeOK, OutcomeError:
		c.outcome = in.Outcome
	}
	if c.outcome == OutcomeError {
		c.errorType = NormalizeErrorType(in.ErrorType, in.ErrorCodes)
	}
	if in.ManagerVersion != "" {
		c.version = NormalizeVersion(in.ManagerVersion)
	}
	if in.InstallID != "" {
		if id, ok := NormalizeUUID(in.InstallID); ok {
			c.installID = id
		} else {
			c.reason = ReasonInvalid
		}
	}
	if c.installID == "" && c.reason == "" && in.MissingReason.valid() {
		c.reason = in.MissingReason
	}
	return c
}

// Name implements Event.
func (c CommandRun) Name() string { return EventCommandRun }

// UID implements Event.
func (c CommandRun) UID() string { return c.uid }

// Hour implements Event.
func (c CommandRun) Hour() time.Time { return c.hour }

func (c CommandRun) producerInfo() producer { return c.producer }

func (c CommandRun) failed() bool { return c.broken }

func (c CommandRun) withDirectoryID(id string) Event {
	c.dirID, _ = NormalizeRandomID(id)
	return c
}

// withFacts fills what the event does not know from the server's facts
// record. known is false when the process had no server; an absent install
// id then says no_server.
func (c CommandRun) withFacts(known bool, f Facts, now time.Time) Event {
	if !known {
		if c.installID == "" && c.reason == "" {
			c.reason = ReasonNoServer
		}
		return c
	}
	if c.version == "" {
		if v, e, ok := f.usableVersion(now); ok {
			c.version, c.edition = v, e
		}
	}
	if c.installID == "" {
		id, reason := f.usableInstallID(now)
		switch {
		case id != "":
			c.installID, c.reason = id, ""
		case c.reason == "":
			c.reason = reason
		}
	}
	return c
}

// Flat implements Event.
func (c CommandRun) Flat() []byte { return c.encode(false) }

func (c CommandRun) stored() []byte { return c.encode(true) }

func (c CommandRun) encode(withLocal bool) []byte {
	w := newWriter(EventCommandRun, c.hour)
	w.str(attrServiceNamespace, "io.rancher")
	w.str(attrServiceName, "io.rancher.cli")
	w.str(attrServiceVersion, c.producer.version)
	w.str(attrInstanceID, c.producer.instance)
	w.optStr(attrConfigDirID, c.dirID)
	w.optStr(attrOSType, c.producer.osType)
	w.optStr(attrHostArch, c.producer.hostArch)
	w.str(attrClientType, string(c.clientType))
	w.optStr(attrClientAgent, c.agent)
	w.boolean(attrClientCI, c.ci)
	w.boolean(attrExecPlugin, c.execPlugin)
	w.str(attrRecordUID, c.uid)
	w.optStr(attrCommand, c.command)
	w.strings(attrFlags, c.flags)
	w.optStr(attrOutcome, string(c.outcome))
	w.optStr(attrErrorType, c.errorType)
	if c.hasExit {
		w.integer(attrExitCode, c.exitCode)
	}
	w.str(attrDuration, string(c.bucket))
	w.optStr(attrManagerVersion, c.version)
	w.optStr(attrManagerEdition, string(c.edition))
	w.optStr(attrInstallID, c.installID)
	if c.installID == "" {
		reason := c.reason
		if !reason.valid() {
			reason = ReasonNotFetched
		}
		w.str(attrMissingReason, string(reason))
	}
	return w.finish(withLocal)
}

// EventsDropped is the io.rancher.cli.events.dropped event.
type EventsDropped struct {
	broken   bool
	hour     time.Time
	producer producer
	uid      string
	dirID    string
	count    int
}

// NewEventsDropped builds the event for the hour that starts at hour and
// lost count events. A count below one is raised to one.
func NewEventsDropped(hour time.Time, count int, ctx invocation.Context) EventsDropped {
	return newEventsDropped(hour, count, producerFor(ctx))
}

// newEventsDropped builds the event with its own random instance id: the
// event is written by the process that compacts the hour, yet it describes
// drops by other processes; it joins to no run. A failed random source
// marks the event broken; no id is invented and the process's own is not
// reused.
func newEventsDropped(hour time.Time, count int, p producer) EventsDropped {
	uid, err := newUUID()
	instance, ierr := newUUID()
	p.instance = instance
	return EventsDropped{
		broken:   err != nil || ierr != nil,
		hour:     hour.UTC().Truncate(time.Hour),
		producer: p,
		uid:      uid,
		count:    max(count, 1),
	}
}

// Name implements Event.
func (d EventsDropped) Name() string { return EventEventsDropped }

// UID implements Event.
func (d EventsDropped) UID() string { return d.uid }

// Hour implements Event.
func (d EventsDropped) Hour() time.Time { return d.hour }

func (d EventsDropped) producerInfo() producer { return d.producer }

func (d EventsDropped) failed() bool { return d.broken }

func (d EventsDropped) withDirectoryID(id string) Event {
	d.dirID, _ = NormalizeRandomID(id)
	return d
}

func (d EventsDropped) withFacts(bool, Facts, time.Time) Event { return d }

// Flat implements Event.
func (d EventsDropped) Flat() []byte { return d.encode(false) }

func (d EventsDropped) stored() []byte { return d.encode(true) }

func (d EventsDropped) encode(withLocal bool) []byte {
	w := newWriter(EventEventsDropped, d.hour)
	w.str(attrServiceNamespace, "io.rancher")
	w.str(attrServiceName, "io.rancher.cli")
	w.str(attrServiceVersion, d.producer.version)
	w.str(attrInstanceID, d.producer.instance)
	w.optStr(attrConfigDirID, d.dirID)
	w.optStr(attrOSType, d.producer.osType)
	w.optStr(attrHostArch, d.producer.hostArch)
	w.str(attrRecordUID, d.uid)
	w.integer(attrDroppedCount, d.count)
	return w.finish(withLocal)
}

// writer builds one flat JSON object with its keys in the order written.
type writer struct {
	buf bytes.Buffer
}

func newWriter(event string, hour time.Time) *writer {
	w := &writer{}
	w.buf.WriteByte('{')
	w.str(keyEventName, event)
	w.str(keyTime, hour.UTC().Truncate(time.Hour).Format(time.RFC3339))
	w.str(keySchemaVersion, ContractVersion)
	return w
}

func (w *writer) key(k string) {
	if w.buf.Len() > 1 {
		w.buf.WriteByte(',')
	}
	b, _ := json.Marshal(k)
	w.buf.Write(b)
	w.buf.WriteByte(':')
}

func (w *writer) str(k, v string) {
	w.key(k)
	b, _ := json.Marshal(v)
	w.buf.Write(b)
}

func (w *writer) optStr(k, v string) {
	if v != "" {
		w.str(k, v)
	}
}

func (w *writer) boolean(k string, v bool) {
	w.key(k)
	w.buf.WriteString(strconv.FormatBool(v))
}

func (w *writer) integer(k string, v int) {
	w.key(k)
	w.buf.WriteString(strconv.Itoa(v))
}

func (w *writer) strings(k string, v []string) {
	w.key(k)
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	w.buf.Write(b)
}

func (w *writer) finish(withLocal bool) []byte {
	if withLocal {
		w.key(keyLocalFormat)
		w.buf.WriteString(strconv.Itoa(LocalFormat))
	}
	w.buf.WriteByte('}')
	return w.buf.Bytes()
}

var (
	instanceMu  sync.Mutex
	instanceVal string
)

// processInstanceID is this process's random service.instance.id, created on
// first use. A failure is not remembered: the next call tries again.
func processInstanceID() (string, error) {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	if instanceVal != "" {
		return instanceVal, nil
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	instanceVal = id
	return id, nil
}

// newUUID returns a random version 4 UUID, or an error when the system
// random source fails; no id is ever invented.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:]), nil
}

var randRead = rand.Read

package telemetry

import (
	"slices"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// ContractVersion is the version of the event contract this package
// writes. It is the schema_version of every event.
const ContractVersion = "1.0.0"

// LocalFormat is the version of the stored file layout. It is written as
// local_format beside the contract fields, is never part of the contract,
// and changes only when the stored layout changes.
const LocalFormat = 1

// Event names.
const (
	EventCommandRun    = "io.rancher.cli.command.run"
	EventEventsDropped = "io.rancher.cli.events.dropped"
)

// Fixed keys of the flat event. Every other key is an attribute name.
const (
	keyEventName     = "event.name"
	keyTime          = "time"
	keySchemaVersion = "schema_version"
	keyLocalFormat   = "local_format"
)

// Attribute names.
const (
	attrServiceNamespace = "service.namespace"
	attrServiceName      = "service.name"
	attrServiceVersion   = "service.version"
	attrInstanceID       = "service.instance.id"
	attrConfigDirID      = "io.rancher.cli.config_directory.id"
	attrOSType           = "os.type"
	attrHostArch         = "host.arch"
	attrClientType       = "io.rancher.client.type"
	attrClientAgent      = "io.rancher.client.agent"
	attrClientCI         = "io.rancher.client.ci"
	attrExecPlugin       = "io.rancher.cli.exec_plugin"
	attrRecordUID        = "log.record.uid"
	attrCommand          = "io.rancher.cli.command"
	attrFlags            = "io.rancher.cli.command.flags"
	attrOutcome          = "io.rancher.cli.command.outcome"
	attrErrorType        = "error.type"
	attrExitCode         = "process.exit.code"
	attrDuration         = "io.rancher.cli.command.duration.bucket"
	attrManagerVersion   = "io.rancher.manager.version"
	attrManagerEdition   = "io.rancher.manager.edition"
	attrInstallID        = "io.rancher.manager.install.id"
	attrMissingReason    = "io.rancher.cli.install_id.missing_reason"
	attrDroppedCount     = "io.rancher.cli.events.dropped.count"
)

// Stability is the stability marker every attribute carries.
type Stability string

// The stability markers. Every attribute starts as development and becomes
// stable once an ingestion consumer depends on it.
const (
	StabilityDevelopment Stability = "development"
	StabilityStable      Stability = "stable"
)

// Kind is the JSON type of an attribute.
type Kind string

// The attribute kinds.
const (
	KindString  Kind = "string"
	KindBoolean Kind = "boolean"
	KindInteger Kind = "integer"
	KindStrings Kind = "array of string"
)

// Where an attribute sits in OTLP.
const (
	PlaceResource = "resource"
	PlaceRecord   = "log record"
)

// ContractInputs holds the closed value lists and the text that live outside
// this package; the package imports no error code package and no package that
// encodes events.
type ContractInputs struct {
	// ErrorCodes is every CLI error code, in a stable order.
	ErrorCodes []string
	// OTLPEncoding is the text of the JSON encoding section of the OTLP
	// mapping, written by the package that encodes OTLP; empty adds nothing.
	OTLPEncoding string
}

// Attribute is the metadata of one attribute: the one place the encoder's
// tests, the schema, the field table and the OTLP mapping are generated
// from.
type Attribute struct {
	Name      string
	Kind      Kind
	Stability Stability
	// Standard says where the name comes from: an OpenTelemetry level or new.
	Standard string
	// Events lists the event names that carry the attribute.
	Events []string
	// Required marks the attributes every event of those names carries.
	Required bool
	// Placement is where the attribute sits in OTLP.
	Placement string
	// Pattern, MaxLength, MaxItems, Minimum and Maximum are the schema
	// limits; zero means none. Maximum is only used with HasMaximum.
	Pattern    string
	MaxLength  int
	MaxItems   int
	Minimum    int
	Maximum    int
	HasMinimum bool
	HasMaximum bool
	// Const is the one allowed value of a constant attribute.
	Const string
	// Values returns the closed value list, or nil for none.
	Values func(ContractInputs) []string
	// Why says why the attribute is recorded.
	Why string
	// Absent says what a missing attribute means.
	Absent string
}

var bothEvents = []string{EventCommandRun, EventEventsDropped}
var runOnly = []string{EventCommandRun}

const (
	uuidPattern = "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
	// randomIDPattern is a random UUID, version 4, as the three ids this
	// producer creates are.
	randomIDPattern = "^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
	// versionPattern is the output of the version rule.
	versionPattern = "^(?:[0-9]+\\.[0-9]+\\.[0-9]+(?:-(?:alpha|beta|rc)\\.?[0-9]+|-hotfix)?|dev|dev-[0-9a-f]{1,40}|dev-unknown|unknown|_OTHER)$"
	// commandPattern is the name rule for a command: one to three words.
	commandPattern = "^[a-z][a-z0-9-]{0,31}( [a-z][a-z0-9-]{0,31}){0,2}$"
	// flagPattern is the name rule for a flag's long name.
	flagPattern = "^[a-z][a-z0-9-]{0,31}$"
	timePattern = "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:00:00Z$"
)

// maxFlags is the most flag names one event carries.
const maxFlags = 32

func fixed(v ...string) func(ContractInputs) []string {
	return func(ContractInputs) []string { return slices.Clone(v) }
}

var attributes = []Attribute{
	{
		Name: attrServiceNamespace, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry stable",
		Events: bothEvents, Placement: PlaceResource, Const: "io.rancher",
		Why:    "Groups every Rancher product's events under one namespace.",
		Absent: "The producer did not state a namespace; every producer of this contract is in io.rancher.",
	},
	{
		Name: attrServiceName, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry stable",
		Events: bothEvents, Required: true, Placement: PlaceResource, Const: "io.rancher.cli",
		Why:    "Names the producer; the producer is identified by this attribute alone.",
		Absent: "Never absent; the event is invalid without it.",
	},
	{
		Name: attrServiceVersion, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry stable",
		Events: bothEvents, Required: true, Placement: PlaceResource, Pattern: versionPattern, MaxLength: 64,
		Why:    "Tells which release of the producer wrote the event. A customer-specific hotfix id is removed; a version that matches none of the forms this contract lists becomes _OTHER.",
		Absent: "Never absent; the event is invalid without it.",
	},
	{
		Name: attrInstanceID, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry stable",
		Events: bothEvents, Required: true, Placement: PlaceResource, Pattern: randomIDPattern,
		Why:    "A random id for one process, the same on every event that describes that process. It names no person and no machine.",
		Absent: "Never absent; the event is invalid without it.",
	},
	{
		Name: attrConfigDirID, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: bothEvents, Placement: PlaceResource, Pattern: randomIDPattern,
		Why:    "A random id per CLI config directory. It is not a user and not a machine: coding agents use several directories at once, and CI and test runs use new ones.",
		Absent: "The config directory id did not exist yet, or another process was creating it; treat the event as from an unknown config directory.",
	},
	{
		Name: attrOSType, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry development",
		Events: bothEvents, Placement: PlaceResource,
		Values: func(ContractInputs) []string { return OSValues() },
		Why:    "Tells which operating systems the producer runs on.",
		Absent: "The producer did not report an operating system.",
	},
	{
		Name: attrHostArch, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry development",
		Events: bothEvents, Placement: PlaceResource,
		Values: func(ContractInputs) []string { return ArchValues() },
		Why:    "Tells which processor architectures the producer runs on.",
		Absent: "The producer did not report an architecture.",
	},
	{
		Name: attrClientType, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord,
		Values: fixed(string(ClientPerson), string(ClientAgent), string(ClientAutomation)),
		Why:    "A hint, never proof, of who ran the command: a coding agent, a person at a terminal, or automation (CI, scripts, kubectl calling the exec plugin, output piped to another program).",
		Absent: "The producer did not report who ran the command.",
	},
	{
		Name: attrClientAgent, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord,
		Values: func(ContractInputs) []string { return AgentValues() },
		Why:    "Tells which coding-agent harness ran the command, from a fixed list; agent stands for a harness not on the list.",
		Absent: "No coding-agent harness was detected.",
	},
	{
		Name: attrClientCI, Kind: KindBoolean, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord,
		Why:    "True when an environment variable that a CI system sets was present; recorded beside the client type and never changes the client type.",
		Absent: "Always written; never absent.",
	},
	{
		Name: attrExecPlugin, Kind: KindBoolean, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord,
		Why:    "True when kubectl ran the command as its exec plugin.",
		Absent: "Always written; never absent.",
	},
	{
		Name: attrRecordUID, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry development",
		Events: bothEvents, Required: true, Placement: PlaceRecord, Pattern: randomIDPattern,
		Why:    "A random id per event, never time-ordered; lets a consumer remove duplicates.",
		Absent: "Never absent; the event is invalid without it.",
	},
	{
		Name: attrCommand, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Pattern: commandPattern, MaxLength: 98,
		Why:    "The command name (an alias is replaced by the name it stands for); never an argument value. Valid names are in the `rancher schema` manifest of the event's service.version.",
		Absent: "The name did not follow the name rule and was left out.",
	},
	{
		Name: attrFlags, Kind: KindStrings, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Pattern: flagPattern, MaxLength: 32, MaxItems: maxFlags,
		Why:    "The long names, without dashes and sorted, of the flags typed on the command line; never values and never flags set from the environment. At most 32 are kept (the first 32 sorted).",
		Absent: "Always written; an empty list means no flag was typed.",
	},
	{
		Name: attrOutcome, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Values: fixed(string(OutcomeOK), string(OutcomeError)),
		Why:    "Whether the command succeeded. An exit code that the command lists as success in the `rancher schema` manifest (`successExitCodes`) counts as ok.",
		Absent: "The producer did not report an outcome.",
	},
	{
		Name: attrErrorType, Kind: KindString, Stability: StabilityDevelopment, Standard: "OpenTelemetry stable",
		Events: runOnly, Placement: PlaceRecord,
		Values: func(in ContractInputs) []string { return append(slices.Clone(in.ErrorCodes), Other) },
		Why:    "The CLI error code of a failed command, or _OTHER when it has none.",
		Absent: "The command did not fail.",
	},
	{
		Name: attrExitCode, Kind: KindInteger, Stability: StabilityDevelopment, Standard: "OpenTelemetry release candidate",
		Events: runOnly, Placement: PlaceRecord, Minimum: 0, Maximum: 255, HasMinimum: true, HasMaximum: true,
		Why:    "The exit code the process returns to its caller.",
		Absent: "The producer did not report an exit code.",
	},
	{
		Name: attrDuration, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Values: fixed(string(BucketUnder1s), string(Bucket1to5s), string(BucketOver5s)),
		Why:    "How long the command took, in three buckets; never an exact time.",
		Absent: "The producer did not report a duration.",
	},
	{
		Name: attrManagerVersion, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Pattern: versionPattern, MaxLength: 64,
		Why:    "The version of the Rancher server the command targeted, by the same version rule.",
		Absent: "The server version is unknown.",
	},
	{
		Name: attrManagerEdition, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Values: fixed(string(EditionCommunity), string(EditionPrime)),
		Why:    "The edition of the Rancher server the command targeted.",
		Absent: "The server edition is unknown.",
	},
	{
		Name: attrInstallID, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord, Pattern: uuidPattern,
		Why:    "The install id of the Rancher server the command targeted (its install-uuid setting), when the CLI could read it. It joins CLI use to a server installation.",
		Absent: "The CLI did not have the id; io.rancher.cli.install_id.missing_reason says why.",
	},
	{
		Name: attrMissingReason, Kind: KindString, Stability: StabilityDevelopment, Standard: "new",
		Events: runOnly, Placement: PlaceRecord,
		Values: fixed(string(ReasonNoServer), string(ReasonNotPermitted), string(ReasonNotFetched), string(ReasonInvalid)),
		Why:    "Why the install id is absent: no_server (the run had no server), not_permitted (the server's response was 403 or 404), not_fetched (no fresh response yet), invalid (the value was not a UUID). Present exactly when the install id is absent.",
		Absent: "The install id is present.",
	},
	{
		Name: attrDroppedCount, Kind: KindInteger, Stability: StabilityDevelopment, Standard: "new",
		Events: []string{EventEventsDropped}, Required: true, Placement: PlaceRecord, Minimum: 1, HasMinimum: true,
		Why:    "How many events were dropped in one finished hour because every slot (a numbered file) the producer tried was taken. Consumers report usage as lower bounds when this event exists.",
		Absent: "Never absent; the event is invalid without it.",
	},
}

// Attributes returns the metadata of every attribute, in the order the
// encoder writes them.
func Attributes() []Attribute { return slices.Clone(attributes) }

// EventSpec describes one event name.
type EventSpec struct {
	Name        string
	Description string
}

// Events returns the event names the contract defines.
func Events() []EventSpec {
	return []EventSpec{
		{EventCommandRun, "One finished command run."},
		{EventEventsDropped, "One finished hour in which events were dropped: every slot (a numbered file) the producer tried was taken."},
	}
}

// AttributesOf returns the attributes an event name carries, in order.
func AttributesOf(event string) []Attribute {
	var out []Attribute
	for _, a := range attributes {
		if slices.Contains(a.Events, event) {
			out = append(out, a)
		}
	}
	return out
}

// AgentValues is the closed list of agent ids: the invocation context's
// ids and the generic agent.
func AgentValues() []string {
	return append(invocation.AgentIDs(), invocation.AgentGeneric)
}

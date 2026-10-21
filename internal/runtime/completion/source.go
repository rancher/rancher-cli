package completion

import "context"

// Request is the completion site as the surface parsed it. Flag is empty
// for a positional; Index is the positional's index. Partial is the value
// fragment as typed, composite prefix included. Cluster and Project are
// the --cluster and --project flags as typed on the line and stay empty
// when the flags are absent: the active context is never read at TAB
// time, so a context cannot make a site complete against a scope the user
// did not type. ClusterID is Cluster's resolution, filled only when the
// claim said ClusterResolved. Tail is the raw arg tail; every component a
// source takes from it into a URL goes through ValidSegment via the Path
// builders. Positionals are the tail's positional tokens in order, the
// value-taking flags consumed by their arity, which only the surface
// knows; a site that depends on an earlier positional (a cluster's pool,
// a client's secret, a log target's pods) reads it here and never
// re-parses the tail. The word being completed is not among them.
type Request struct {
	Flag        string
	Index       int
	Partial     string
	Cluster     string
	ClusterID   string
	Project     string
	Tail        []string
	Positionals []string
}

// ClusterNeed says whether a source needs a resolved cluster id.
type ClusterNeed int

const (
	// ClusterNone means no cluster: an unrelated or unresolvable typed
	// cluster neither suppresses the source nor costs a request.
	ClusterNone ClusterNeed = iota
	// ClusterResolved means Request.ClusterID must hold an id: the surface
	// resolves a typed display name first (one request, counted apart from
	// the allowance) and skips the source when it cannot.
	ClusterResolved
)

// CredentialNeed says which credential a source can work with.
type CredentialNeed int

const (
	// CredentialAny accepts an exec-credential-only token (downstream
	// listings through the cluster proxy).
	CredentialAny CredentialNeed = iota
	// CredentialManagement needs a management token: the surface skips the
	// source under an id-only token.
	CredentialManagement
	// CredentialNone needs no credential: the source reads an
	// unauthenticated route, the surface attaches no token to its requests,
	// and the command it completes for supplies the server and CA.
	CredentialNone
)

// Allowance is the closed class of request budgets the surface grants. The
// surface maps each member to a number; a claim outside the class is
// rejected before a connection exists. The zero value is not a member.
type Allowance int

const (
	// AllowanceOne is one request: a collection listed, or one object read.
	AllowanceOne Allowance = iota + 1
	// AllowanceDiscovery is the two discovery roots of a cluster and, for
	// a root that does not serve the aggregated document, its plain
	// document and one document per group, at most DiscoveryGroupLimit.
	AllowanceDiscovery
	// AllowanceSteveClusterPages is the Steve cluster collection followed
	// through its continuation tokens, bounded like the access commands'
	// own page walk.
	AllowanceSteveClusterPages
	// AllowanceSteveProjectPages is one cluster's steve project collection
	// followed page by page, bounded like the command's own read.
	AllowanceSteveProjectPages
)

// DiscoveryGroupLimit bounds the group documents a source reads on a
// discovery root that does not serve the aggregated document: the
// legacy walk is one request per group, and a cluster with more groups
// than this offers its first DiscoveryGroupLimit groups. The surface's
// count for AllowanceDiscovery is derived from it.
const DiscoveryGroupLimit = 100

// Requirements is a source's declaration for one request, made without
// I/O.
type Requirements struct {
	Cluster    ClusterNeed
	Credential CredentialNeed
	Allowance  Allowance
}

// ManagementListing is the declaration of a source that makes one request
// to the management cluster - a collection listed, or one object read:
// no cluster need, a management token, one request.
func ManagementListing() Requirements {
	return Requirements{Cluster: ClusterNone, Credential: CredentialManagement, Allowance: AllowanceOne}
}

// PublicListing is the declaration of a source that makes one request to
// an unauthenticated route of the server: no cluster need, no credential,
// one request.
func PublicListing() Requirements {
	return Requirements{Cluster: ClusterNone, Credential: CredentialNone, Allowance: AllowanceOne}
}

// ClusterListing is the declaration of a source that makes one request to
// the management cluster scoped by the typed cluster - a collection in
// the cluster's namespace, or one object there: the cluster resolved, a
// management token, one request.
func ClusterListing() Requirements {
	return Requirements{Cluster: ClusterResolved, Credential: CredentialManagement, Allowance: AllowanceOne}
}

// Result is what a source answers. Names are bare candidates the surface
// sanitizes, dedupes and sorts. Descriptions maps a bare name to the raw
// text a shell shows beside it; a name with no entry has none, and the
// surface sanitizes every text and looks it up by the bare name, before
// any prefix. Prefix, when set, is a compiled-in literal the source chose
// ("<kind>/" for a composite value) that the surface validates and puts in
// front of every sanitized name.
type Result struct {
	Names        []string
	Descriptions map[string]string
	Prefix       string
}

// Source is a live completion source. Claim runs first, without I/O:
// false means the source does not own the site and the surface falls
// through; true owns the site even when Complete answers no names. Both
// functions are declared in an owner's restricted complete package; the
// surface verifies their declaring package when it builds the command
// tree.
type Source struct {
	Claim    func(req Request) (Requirements, bool)
	Complete func(ctx context.Context, conn *Connection, req Request) Result
}

// Completer is the optional interface a command implements to own live
// completion for some of its sites. It returns the Source its owner's
// complete package declares; the functions that answer a site are the
// Source's, never a method of the command. A source that needs no
// credential gets its server and CA from the command's own read-only
// resolution, which the surface asks for through a method of the command
// (CompletionTarget in internal/cli).
type Completer interface {
	LiveSource() Source
}

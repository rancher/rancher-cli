package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// FactsMaxAge is how long a stored fact stays usable, and how long a facts
// file is kept before the hourly pass removes it.
const FactsMaxAge = 30 * 24 * time.Hour

// FailureKind says why a request for a fact failed.
type FailureKind string

// The failure kinds.
const (
	// FailureForbidden is a 403 or 404 answer.
	FailureForbidden FailureKind = "forbidden"
	// FailureNetwork is a timeout, a network error, or any answer that is not
	// a 2xx, a 403 or a 404: a 401, another 4xx, a redirect or a 5xx.
	FailureNetwork FailureKind = "network"
	// FailureInvalid is an answer that failed the pattern for the value.
	FailureInvalid FailureKind = "invalid"
)

func (k FailureKind) valid() bool {
	switch k {
	case FailureForbidden, FailureNetwork, FailureInvalid:
		return true
	}
	return false
}

// Failure is the last failed request for a fact. It is kept beside the last
// good value and never replaces it.
type Failure struct {
	Kind FailureKind `json:"kind"`
	At   time.Time   `json:"at"`
}

// Fact is one record of a server's facts file: the last good value and
// when it was read, and the last failure.
type Fact struct {
	Value   string    `json:"value,omitempty"`
	Edition string    `json:"edition,omitempty"`
	At      time.Time `json:"at,omitzero"`
	Failure *Failure  `json:"failure,omitempty"`
}

// Facts is the content of one server's facts file: the version record and
// the install id record.
type Facts struct {
	Version   Fact `json:"version"`
	InstallID Fact `json:"install_id"`
	// UpdatedAt is the latest time any record changed; the hourly pass
	// removes a file whose UpdatedAt is past FactsMaxAge.
	UpdatedAt time.Time `json:"updated_at"`
}

// SetVersion stores a successful answer for the server's version and
// edition. The version goes through the version rule.
func (f *Facts) SetVersion(version string, edition Edition, now time.Time) {
	f.Version = Fact{Value: NormalizeVersion(version), Edition: string(parseEdition(string(edition))), At: now.UTC()}
	f.UpdatedAt = now.UTC()
}

// FailVersion stores a failed request beside the last good version.
func (f *Facts) FailVersion(kind FailureKind, now time.Time) {
	f.Version.Failure = failure(kind, now)
	f.UpdatedAt = now.UTC()
}

// SetInstallID stores the server's install id. A value that is not a UUID
// is stored as an invalid failure, and the last good value stays.
func (f *Facts) SetInstallID(raw string, now time.Time) {
	id, ok := NormalizeUUID(raw)
	if !ok {
		f.FailInstallID(FailureInvalid, now)
		return
	}
	f.InstallID = Fact{Value: id, At: now.UTC()}
	f.UpdatedAt = now.UTC()
}

// FailInstallID stores a failed request beside the last good install id.
func (f *Facts) FailInstallID(kind FailureKind, now time.Time) {
	f.InstallID.Failure = failure(kind, now)
	f.UpdatedAt = now.UTC()
}

func failure(kind FailureKind, now time.Time) *Failure {
	if !kind.valid() {
		kind = FailureNetwork
	}
	return &Failure{Kind: kind, At: now.UTC()}
}

// usableVersion returns the stored version and edition when they were read
// no more than FactsMaxAge ago.
func (f Facts) usableVersion(now time.Time) (string, Edition, bool) {
	v := f.Version
	if v.Value == "" || NormalizeVersion(v.Value) != v.Value || now.Sub(v.At) > FactsMaxAge {
		return "", EditionUnknown, false
	}
	return v.Value, parseEdition(v.Edition), true
}

// usableInstallID returns the stored install id when it was read no more
// than FactsMaxAge ago. Otherwise it returns the reason there is none.
func (f Facts) usableInstallID(now time.Time) (string, MissingReason) {
	r := f.InstallID
	if id, ok := NormalizeUUID(r.Value); ok && now.Sub(r.At) <= FactsMaxAge {
		return id, ""
	}
	if r.Failure != nil {
		switch r.Failure.Kind {
		case FailureForbidden:
			return "", ReasonNotPermitted
		case FailureInvalid:
			return "", ReasonInvalid
		}
	}
	return "", ReasonNotFetched
}

// FactsFileName is the name of the facts file of a server: the first 32 hex
// characters of the SHA-256 of its canonical URL, then .json. The name
// holds nothing but the hash; it is the same on every path that reaches
// the server and never carries a server entry's name.
func FactsFileName(canonicalURL string) string {
	sum := sha256.Sum256([]byte(canonicalURL))
	return hex.EncodeToString(sum[:])[:32] + ".json"
}

func parseFacts(data []byte) (Facts, bool) {
	var f Facts
	if err := json.Unmarshal(data, &f); err != nil {
		return Facts{}, false
	}
	return f, true
}

func marshalFacts(f Facts) ([]byte, error) {
	if f.UpdatedAt.IsZero() {
		for _, t := range []time.Time{f.Version.At, f.InstallID.At} {
			if t.After(f.UpdatedAt) {
				f.UpdatedAt = t
			}
		}
	}
	return json.Marshal(f)
}

// Package rbac implements the grant/revoke role-binding surface: subject
// parsing and resolution, scope resolution, and the two commands.
package rbac

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

type SubjectKind string

const (
	SubjectUser  SubjectKind = "user"
	SubjectGroup SubjectKind = "group"
)

// Subject is the parsed <subject> argument. Exactly one of Principal
// (fully qualified, contains "://") and Bare is set.
type Subject struct {
	Kind      SubjectKind
	Principal string
	Bare      string
}

// ParseSubject parses the positional subject plus --type. A fully
// qualified principal infers its kind by a CLOSED rule: "local" and
// "_user"-suffixed prefixes are users; "_group"-suffixed prefixes plus
// Rancher's documented group aliases (github_org, github_team) are
// groups; any other prefix is not inferred — an explicit --type decides
// it, and without one it is a validation error. An explicit --type
// contradicting an inferred kind is a validation error, mirroring the
// spec's inference-conflict rule. Principals with the "system:" prefix
// (Rancher's IsSystem predicate) are rejected outright.
func ParseSubject(arg, typeFlag string) (Subject, error) {
	if arg == "" {
		return Subject{}, clierr.New(clierr.CodeValidation,
			"argument <subject> is required; provide a user/group name, id, or fully qualified principal")
	}
	var flagKind SubjectKind
	switch typeFlag {
	case "", "user":
		flagKind = SubjectUser
	case "group":
		flagKind = SubjectGroup
	default:
		return Subject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid --type %q; pass --type user or --type group", typeFlag))
	}

	// Principals with the "system:" prefix (Rancher's IsSystem predicate,
	// shared via engine.IsSystemPrincipal) are rejected outright.
	if engine.IsSystemPrincipal(arg) {
		return Subject{}, clierr.New(clierr.CodeValidation,
			"system principals cannot be granted roles; pass a user or group principal")
	}

	if !strings.Contains(arg, "://") {
		if flagKind == SubjectGroup {
			return Subject{}, clierr.New(clierr.CodeValidation,
				"group subjects require a fully qualified principal id; pass the full group principal id, e.g. okta_group://<id>")
		}
		return Subject{Kind: SubjectUser, Bare: arg}, nil
	}

	if arg[strings.Index(arg, "://")+3:] == "" {
		return Subject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("principal %q has empty remainder after ://; provide the full principal id, e.g. local://<user-id>", arg))
	}

	// Closed inference rule: only recognized principal forms infer a kind.
	// "_user" → user, "_group" → group, plus Rancher's documented group
	// aliases (github_org, github_team) and "local" (users only). Unknown
	// prefixes never default silently: an explicit --type decides them, and
	// without one they are a validation error.
	prefix := arg[:strings.Index(arg, "://")]
	var inferred SubjectKind
	switch {
	case prefix == "local" || strings.HasSuffix(prefix, "_user"):
		inferred = SubjectUser
	case strings.HasSuffix(prefix, "_group") || prefix == "github_org" || prefix == "github_team":
		inferred = SubjectGroup
	default:
		if typeFlag == "" {
			return Subject{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("cannot infer subject type from principal %q; pass --type user or --type group", arg))
		}
		return Subject{Kind: flagKind, Principal: arg}, nil
	}
	if typeFlag != "" && flagKind != inferred {
		return Subject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--type %s contradicts principal %q (inferred %s); drop --type or pass the matching principal", typeFlag, arg, inferred))
	}
	return Subject{Kind: inferred, Principal: arg}, nil
}

// ProvisionedUserID derives the deterministic user id Rancher's EnsureUser
// creates for a principal (pkg/auth/providers/common/usermanager.go:281-283):
// "u-" + lowercase(base32-no-padding(sha256(principal))[:10]).
func ProvisionedUserID(principal string) string {
	h := sha256.New()
	h.Write([]byte(principal))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h.Sum(nil))
	return "u-" + strings.ToLower(enc[:10])
}

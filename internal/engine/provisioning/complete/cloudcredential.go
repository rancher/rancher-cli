package complete

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

const (
	nodeDriverGroup      = "management.cattle.io"
	nodeDriverVersion    = "v3"
	nodeDriverResource   = "nodedrivers"
	settingResource      = "settings"
	kev2OperatorsSetting = "kev2-operators"
	alibabaOperator      = "alibaba"
	flagSet              = "set"
	flagFromFile         = "from-file"
)

// CreateCloudCredentialSource completes create cloud-credential: the <driver>
// positional with the drivers the command accepts on this server, and --set and
// --from-file with the fields of the driver typed as the first positional that
// the flag accepts, each with the trailing "=" the surface adds. A <driver> needs
// a management token and two reads, the node drivers and the kev2-operators
// setting; a field flag needs one read, none for s3. A field flag whose value
// already has "=" has nothing to offer: the value is the user's, and a secret
// never comes from a suggestion. Every other site of the command, the display
// name included, is declined.
func CreateCloudCredentialSource() completion.Source {
	return completion.Source{Claim: claimCreateCloudCredential, Complete: completeCreateCloudCredential}
}

// GetCloudCredentialSource completes the <credential> argument of get
// cloud-credential with the values it accepts: every credential id and each
// display name that names one credential. It needs a management token and one
// listing; every other site is declined.
func GetCloudCredentialSource() completion.Source {
	return completion.Source{Claim: claimGetCloudCredential, Complete: completeGetCloudCredential}
}

// PatchCloudCredentialSource completes patch cloud-credential: the <credential>
// argument with the credential ids the command accepts, and --set and --from-file with the fields of the driver the typed credential
// stores, each with the trailing "=" the surface adds, under the rule of create
// cloud-credential. A field flag needs one listing of the credentials and then,
// for a driver other than s3, the node drivers or the kev2-operators setting; a
// credential that does not resolve to exactly one, or a refused listing, offers
// nothing. Every other site of the command, the display name and the description
// included, is declined.
func PatchCloudCredentialSource() completion.Source {
	return completion.Source{Claim: claimPatchCloudCredential, Complete: completePatchCloudCredential}
}

// DeleteCloudCredentialSource completes the <credential> argument of delete
// cloud-credential with the credential ids it accepts; every other site is
// declined (--wait and --timeout complete from the flags' own values).
func DeleteCloudCredentialSource() completion.Source {
	return completion.Source{Claim: claimDeleteCloudCredential, Complete: completeDeleteCloudCredential}
}

func claimDeleteCloudCredential(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "" || req.Index != 0 {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

func completeDeleteCloudCredential(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	return completeCredentialIDs(ctx, conn)
}

func claimGetCloudCredential(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "" || req.Index != 0 {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

func completeGetCloudCredential(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	return completeCloudCredentials(ctx, conn)
}

func claimCreateCloudCredential(req completion.Request) (completion.Requirements, bool) {
	switch {
	case req.Flag == "" && req.Index == 0:
		return completion.ManagementTwoListings(), true
	case req.Flag == flagSet || req.Flag == flagFromFile:
		if len(req.Positionals) == 0 || strings.Contains(req.Partial, "=") {
			return completion.Requirements{}, false
		}
		return completion.ManagementListing(), true
	}
	return completion.Requirements{}, false
}

func completeCreateCloudCredential(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	if req.Flag == "" {
		return completeDrivers(ctx, conn)
	}
	driver, ok := credentialDriver(ctx, conn, req.Positionals[0])
	if !ok {
		return completion.Result{}
	}
	return fieldNames(driver, req)
}

func claimPatchCloudCredential(req completion.Request) (completion.Requirements, bool) {
	switch {
	case req.Flag == "" && req.Index == 0:
		return completion.ManagementListing(), true
	case req.Flag == flagSet || req.Flag == flagFromFile:
		if len(req.Positionals) == 0 || strings.Contains(req.Partial, "=") {
			return completion.Requirements{}, false
		}
		return completion.ManagementTwoListings(), true
	}
	return completion.Requirements{}, false
}

func completePatchCloudCredential(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	if req.Flag == "" {
		return completeCredentialIDs(ctx, conn)
	}
	prefix, ok := storedDriverPrefix(ctx, conn, req.Positionals[0])
	if !ok {
		return completion.Result{}
	}
	driver, ok := credentialDriver(ctx, conn, prefix)
	if !ok {
		return completion.Result{}
	}
	return fieldNames(driver, req)
}

// fieldNames returns the fields of driver the flag of req accepts and the line
// has not typed yet: the public fields for --set, the secret and multi-line
// fields for --from-file.
func fieldNames(driver cloudcredfields.Driver, req completion.Request) completion.Result {
	taken := slices.Concat(typedFields(req.Tail, flagSet), typedFields(req.Tail, flagFromFile))
	keep := func(f cloudcredfields.Field) bool {
		if slices.Contains(taken, f.Name) {
			return false
		}
		if req.Flag == flagSet {
			return !f.Secret
		}
		return f.Secret || f.Multiline
	}
	return completion.Result{Names: driver.Names(keep)}
}

// storedDriverPrefix lists the Secrets of cattle-global-data and returns the
// driver prefix of the one credential value names, by id (with or without
// "cattle-global-data:"), as patch cloud-credential selects it: a value that
// is only a display name selects nothing, and a bare value that is also another
// credential's display name is ambiguous (the prefixed form is not). It reads
// metadata and data key names only. In each of these cases, and when a listing
// fails, a value selects no credential, or a credential's keys name no driver,
// it returns false.
func storedDriverPrefix(ctx context.Context, conn *completion.Connection, value string) (string, bool) {
	p, err := completion.ManagementNamespacedPath("", "v1", cloudCredentialNamespace, "secrets")
	if err != nil {
		return "", false
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return "", false
	}
	name := strings.TrimPrefix(value, cloudCredentialNamespace+":")
	var byID map[string]any
	ambiguous := false
	for _, item := range list.Items {
		id := completion.ItemName(item)
		if id == "" || !credentialShaped(item, id) {
			continue
		}
		switch {
		case id == name:
			byID = item
		case storedDisplayName(item) == name:
			ambiguous = true
		}
	}
	if byID == nil || (ambiguous && name == value) {
		return "", false
	}
	return keyDriverPrefix(byID)
}

// storedDisplayName reads the field.cattle.io/name annotation of a Secret.
func storedDisplayName(item map[string]any) string {
	meta, _ := item["metadata"].(map[string]any)
	anns, _ := meta["annotations"].(map[string]any)
	display, _ := anns[cloudCredentialDisplay].(string)
	return display
}

// keyDriverPrefix returns the prefix before "credentialConfig-" of the first
// data key, in sorted order, that has it. Key names only.
func keyDriverPrefix(item map[string]any) (string, bool) {
	data, _ := item["data"].(map[string]any)
	for _, key := range slices.Sorted(maps.Keys(data)) {
		if before, _, ok := strings.Cut(key, credentialConfigMarker); ok && before != "" {
			return before, true
		}
	}
	return "", false
}

// completeDrivers returns the display names of the node drivers with a
// credential, s3, and alibaba while its operator is active: what the command
// accepts as <driver>.
func completeDrivers(ctx context.Context, conn *completion.Connection) completion.Result {
	drivers, ok := listNodeDrivers(ctx, conn)
	if !ok {
		return completion.Result{}
	}
	return completion.Result{Names: cloudcredfields.OfferedPrefixes(drivers, alibabaActive(ctx, conn))}
}

// credentialDriver resolves the credential config of the driver typed as the
// first positional, the way the command does: s3 is static, alibaba needs its
// operator active, and any other name is the first node driver with that
// display name, which needs a credential schema and credential fields.
func credentialDriver(ctx context.Context, conn *completion.Connection, name string) (cloudcredfields.Driver, bool) {
	switch name {
	case "s3":
		return cloudcredfields.Static(name)
	case alibabaOperator:
		if !alibabaActive(ctx, conn) {
			return cloudcredfields.Driver{}, false
		}
		return cloudcredfields.Static(name)
	}
	drivers, ok := listNodeDrivers(ctx, conn)
	if !ok {
		return cloudcredfields.Driver{}, false
	}
	for _, n := range drivers {
		if n.DisplayName != name {
			continue
		}
		if !n.HasSchema() {
			return cloudcredfields.Driver{}, false
		}
		return n.Driver()
	}
	return cloudcredfields.Driver{}, false
}

func listNodeDrivers(ctx context.Context, conn *completion.Connection) ([]cloudcredfields.NodeDriver, bool) {
	p, err := completion.ManagementPath(nodeDriverGroup, nodeDriverVersion, nodeDriverResource)
	if err != nil {
		return nil, false
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return nil, false
	}
	out := make([]cloudcredfields.NodeDriver, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, cloudcredfields.NodeDriverOf(item))
	}
	return out, true
}

// alibabaActive reads the kev2-operators setting; a setting that cannot be read
// means the operator is not active.
func alibabaActive(ctx context.Context, conn *completion.Connection) bool {
	p, err := completion.ManagementNamedResourcePath(nodeDriverGroup, nodeDriverVersion, "", settingResource, kev2OperatorsSetting)
	if err != nil {
		return false
	}
	var setting map[string]any
	if err := conn.GetJSON(ctx, p, "", &setting); err != nil {
		return false
	}
	return cloudcredfields.OperatorActive(setting, alibabaOperator)
}

// typedFields returns the field names already typed after the flag, in the
// space form (--set field=value) and the equals form (--set=field=value).
func typedFields(tail []string, flag string) []string {
	var out []string
	for i, word := range tail {
		value, joined := strings.CutPrefix(word, "--"+flag+"=")
		if !joined {
			if word != "--"+flag || i+1 >= len(tail) {
				continue
			}
			value = tail[i+1]
		}
		if field, _, ok := strings.Cut(value, "="); ok && field != "" {
			out = append(out, field)
		}
	}
	return out
}

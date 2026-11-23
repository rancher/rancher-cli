package complete

import (
	"context"
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

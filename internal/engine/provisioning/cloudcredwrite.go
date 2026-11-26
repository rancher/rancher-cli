// cloudcredwrite.go implements `create cloud-credential <driver>
// <display-name>`: the one command that writes a cloud credential. Rancher
// serves no API but Norman that runs its credential checks, so the write goes
// through engine.CloudCredentialClient. Every --set and --from-file argument
// and every value the command reads is a secret.Value, which prints <hidden> in
// every format; no preview, result, warning or error shows one. Only the
// functions that call secret.Reveal get the text of a Value: cleanValue,
// decodesAsKubeconfig, shownValue, fileWordPath and typedFieldName here, and the
// request body and the server-text filter in internal/engine.

package provisioning

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/secret"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// nodeDriverGVR and settingGVR are the Rancher kinds the command reads.
var (
	nodeDriverGVR = kindGVR("nodedriver")
	settingGVR    = kindGVR("setting")

	managementClusterGVR = kindGVR("managementcluster")
)

func kindGVR(name string) schema.GroupVersionResource {
	k, _ := kinds.Lookup(name)
	return k.GVR
}

const (
	flagSet         = "set"
	flagFromFile    = "from-file"
	flagDescription = "description"
	flagDisplayName = "display-name"
	flagWait        = "wait"
	flagTimeout     = "timeout"

	// driverAnnotation is the annotation Rancher's UI reads to list a credential
	// in its cluster forms.
	driverAnnotation = "provisioning.cattle.io/driver"

	// harvesterRefusalPrefix starts the message of a 500 Rancher responds with when
	// its check of a Harvester kubeconfig fails; nothing was written.
	harvesterRefusalPrefix = "failed to process harvester cloud credential: "
	// harvesterParserMarker is the text in the tail of that message that comes
	// before the parser's error, which can quote a fragment of the kubeconfig.
	harvesterParserMarker = "tokens from kubeconfig:"
	// harvesterTokenInUse ends the message of a token that another credential,
	// or the one an update changes, already has.
	harvesterTokenInUse = "is already in use by secret"

	// kev2OperatorsSetting has the state of Rancher's KEv2 operators.
	kev2OperatorsSetting = "kev2-operators"
	// alibabaOperator is the operator that serves the alibaba credential.
	alibabaOperator = "alibaba"

	// cloudCredentialWaitDefaultInterval and cloudCredentialWaitDefaultTimeout are
	// the default time between reads and the default time limit of the wait until
	// the caller can read the new Secret.
	cloudCredentialWaitDefaultInterval = time.Second
	cloudCredentialWaitDefaultTimeout  = 30 * time.Second
)

// cloudCredentialWait is the time between two reads of the new Secret and the
// time limit of the wait. A package variable, set once by a test package's TestMain
// through SetCloudCredentialWaitForTest, so waits that run in parallel read
// the same value.
var cloudCredentialWait = struct {
	mu                sync.Mutex
	interval, timeout time.Duration
}{interval: cloudCredentialWaitDefaultInterval, timeout: cloudCredentialWaitDefaultTimeout}

// SetCloudCredentialWaitForTest sets the interval and the timeout of the wait
// after a create and returns a function that puts the old values back. A test
// package calls it once, in its TestMain, before its tests run.
func SetCloudCredentialWaitForTest(interval, timeout time.Duration) (restore func()) {
	cloudCredentialWait.mu.Lock()
	defer cloudCredentialWait.mu.Unlock()
	oldInterval, oldTimeout := cloudCredentialWait.interval, cloudCredentialWait.timeout
	cloudCredentialWait.interval, cloudCredentialWait.timeout = interval, timeout
	return func() {
		cloudCredentialWait.mu.Lock()
		defer cloudCredentialWait.mu.Unlock()
		cloudCredentialWait.interval, cloudCredentialWait.timeout = oldInterval, oldTimeout
	}
}

func cloudCredentialWaitValues() (interval, timeout time.Duration) {
	cloudCredentialWait.mu.Lock()
	defer cloudCredentialWait.mu.Unlock()
	return cloudCredentialWait.interval, cloudCredentialWait.timeout
}

type createCloudCredentialCmd struct {
	clients engine.ClusterClientFactory
	creds   engine.CloudCredentialClientFactory
	src     engine.FileSource
}

// NewCreateCloudCredential builds `create cloud-credential <driver>
// <display-name>` (mutating).
func NewCreateCloudCredential(clients engine.ClusterClientFactory, creds engine.CloudCredentialClientFactory, src engine.FileSource) runtime.Command {
	return &createCloudCredentialCmd{clients: clients, creds: creds, src: src}
}

func (createCloudCredentialCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create cloud-credential",
		Usage: "Create a cloud credential for a node driver or for etcd snapshots in S3",
		Desc: "Creates a cloud credential through Rancher's Norman API, the one API that runs " +
			"Rancher's credential checks: the driver must be active, the required fields " +
			"must be given and, for harvester, the tokens of the kubeconfig are checked. " +
			"<driver> is the node driver's display name, as `rancher get nodedrivers` shows it " +
			"(amazonec2, never aws), or s3, or alibaba while its operator is active. " +
			"A field Rancher marks as a password is given only with --from-file <field>=<path>, " +
			"where a <path> of - reads stdin, once; a field Rancher marks public is given with " +
			"--set <field>=<value> and is shown in the preview; never type a secret there. " +
			"The command is previewed from server reads: Rancher checks the request only when it " +
			"is sent, and it does not check the values against the provider. " +
			"The command waits, up to 30 seconds, until you can read the new " +
			"credential, which is when `rancher create cluster --cloud-credential` finds it; " +
			"--wait=false skips the wait.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "driver", Type: "string", Required: true},
			{Name: "display-name", Type: "string", Required: true},
		},
		Flags: []runtime.ArgSpec{
			{Name: flagSet, Type: "stringSlice", Desc: setFlagDesc, Secret: true},
			{Name: flagFromFile, Type: "stringSlice", Desc: fromFileFlagDesc, Secret: true},
			{Name: flagDescription, Type: "string", Desc: "the credential's `description`"},
			{Name: flagWait, Type: "bool", Desc: "wait until you can read the new credential (default true; --wait=false returns as soon as Rancher accepts the request)"},
		},
		Examples: []string{
			"rancher create cloud-credential amazonec2 prod --set accessKey=AKIAEXAMPLE --from-file secretKey=./secret-key.txt --dry-run",
			"rancher create cloud-credential digitalocean ci --from-file accessToken=- --apply < token.txt",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeTabular,
		SecretInput: true,
	}
}

// serverReadsWarning is the first warning of every cloud credential write.
const serverReadsWarning = "previewed from server reads: Rancher checks the request only when it is sent; it does not check the values against the provider"

// The descriptions of --set and --from-file, which create and patch share.
const (
	setFlagDesc      = "a public field of the driver, as `field=value`; repeat the flag for more fields"
	fromFileFlagDesc = "a secret or multi-line field of the driver and the file that has its value, as `field=path`; a path of - reads stdin once; repeat the flag for more fields"
)

// createdCredentialRows is the result: the row `get cloud-credential` shows
// for the credential, without its age.
type createdCredentialRows []CloudCredRef

func (r createdCredentialRows) Columns() []string { return []string{"NAME", "DISPLAY NAME", "DRIVER"} }

func (r createdCredentialRows) Rows() [][]string {
	out := make([][]string, 0, len(r))
	for _, row := range r {
		out = append(out, []string{row.SecretName, row.DisplayName, row.Driver})
	}
	return out
}

// fieldWord is one --set or --from-file argument split at its first "=".
type fieldWord struct {
	flag         string
	field, value secret.Value
	position     int
}

// named builds the text that names the argument by its flag and position,
// never by what the user typed: a pasted secret that has "=" splits into a
// field part and a value part.
func (w fieldWord) named() string {
	return fmt.Sprintf("the name in the %s --%s", ordinal(w.position), w.flag)
}

// ordinal returns n with its English suffix: 1st, 2nd, 3rd, 4th, 11th, 12th,
// 13th, 21st. It takes the numbers of the arguments of a flag, from 1.
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// splitFieldWords splits the occurrences of a flag into field and value. A
// refusal names the flag and the occurrence's position, never the argument: it
// may be a pasted secret.
func splitFieldWords(flag string, words []secret.Value) ([]fieldWord, error) {
	out := make([]fieldWord, 0, len(words))
	valuePart := "value"
	if flag == flagFromFile {
		valuePart = "path"
	}
	for i, w := range words {
		field, value, ok := w.Cut("=")
		switch {
		case !ok:
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s --%s must be <field>=<%s>", ordinal(i+1), flag, valuePart))
		case field.IsZero():
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s --%s has an empty field name", ordinal(i+1), flag))
		case value.IsZero():
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s --%s has an empty %s", ordinal(i+1), flag, valuePart))
		}
		out = append(out, fieldWord{flag: flag, field: field, value: value, position: i + 1})
	}
	return out, nil
}

// quoteField quotes a field name the user typed that is already known to be a
// field of the driver, or to equal one without regard to case.
func quoteField(field string) string { return strconv.Quote(field) }

// fieldNamed matches the field whose name is exactly name.
func fieldNamed(name secret.Value) func(cloudcredfields.Field) bool {
	return func(f cloudcredfields.Field) bool { return name.EqualString(f.Name) }
}

// typedFieldName returns a field name as the user typed it, when it equals a
// field of the driver without regard to case; a refusal can quote it. Any other
// name may be a pasted secret and is never revealed.
func typedFieldName(field secret.Value, d cloudcredfields.Driver) (string, bool) {
	if _, ok := d.Match(func(f cloudcredfields.Field) bool { return field.EqualFoldString(f.Name) }); !ok {
		return "", false
	}
	return secret.Reveal(field), true
}

// fileWordPath returns the path of a --from-file argument. A path is not
// secret: refusals name it as typed. The argument of any other flag may be a
// secret value, and the function returns "" for it.
func fileWordPath(w fieldWord) string {
	if w.flag != flagFromFile {
		return ""
	}
	return secret.Reveal(w.value)
}

func stdinIsTerminal(in runtime.Input) bool {
	si, ok := in.(runtime.StdinInput)
	return ok && si.StdinTerminal()
}

// credentialValue is one config field to send.
type credentialValue struct {
	field  cloudcredfields.Field
	value  secret.Value
	secret bool
}

func (c *createCloudCredentialCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	driverName, displayName := args[0], args[1]

	sets, files, err := readFieldFlags(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if displayName == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "the display name is empty; give the name the credential shows in Rancher")
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	driver, err := resolveCredentialDriver(ctx, client, driverName, unknownDriverError)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := checkFieldFlags(driver, sets, files); err != nil {
		return safety.Effect{}, nil, err
	}

	if err := checkRequired(driver, sets, files); err != nil {
		return safety.Effect{}, nil, err
	}

	values, warnings, err := readWords(c.src, driver, sets, files)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	nameWarning, err := checkDisplayName(ctx, client, displayName, "")
	if err != nil {
		return safety.Effect{}, nil, err
	}
	warnings = append(warnings, nameWarning...)

	if allowed, reviewed := reviewCredentialSecrets(ctx, client, "create", ""); reviewed && !allowed {
		return safety.Effect{}, nil, clierr.New(clierr.CodeRBAC,
			"you cannot create cloud credentials: Rancher lets the User and Create Clusters roles create secrets in cattle-global-data")
	} else if !reviewed {
		warnings = append(warnings, "your access to create a cloud credential was not checked; Rancher checks it when the request is sent")
	}

	description := in.String(flagDescription)
	doc, diff := requestDocument(driver, displayName, description, values)
	body := engine.NewCloudCredentialBody(doc)
	filter := responseFilter(values)
	label := cloudcredfields.Label(driver.Prefix)

	effectWarnings := []string{
		serverReadsWarning,
	}
	if driver.Prefix == "harvester" {
		effectWarnings = append(effectWarnings, "Rancher checks the tokens of the kubeconfig when the request is sent")
	}
	effectWarnings = append(effectWarnings, warnings...)

	eff := safety.Effect{
		Action:        safety.ActionCreate,
		Target:        "cloudcredentials",
		DisplayKind:   "cloud-credential",
		DisplayName:   displayName,
		PreviewSource: safety.PreviewServerReads,
		Cluster:       engine.LocalCluster,
		Diff:          diff,
		Reversible:    true,
		Warnings:      effectWarnings,
	}
	wait := !in.IsSet(flagWait) || in.Bool(flagWait)
	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, body, filter, driver.Prefix, label, displayName, wait)
	}
	return eff, exec, nil
}

// readFieldFlags reads --set and --from-file and refuses the shapes the command
// line cannot carry, before any request and before stdin or a file is read.
func readFieldFlags(in runtime.Input) (sets, files []fieldWord, err error) {
	sets, err = splitFieldWords(flagSet, in.Secrets(flagSet))
	if err != nil {
		return nil, nil, err
	}
	files, err = splitFieldWords(flagFromFile, in.Secrets(flagFromFile))
	if err != nil {
		return nil, nil, err
	}
	stdinWords := 0
	for _, f := range files {
		if f.value.EqualString("-") {
			stdinWords++
		}
	}
	switch {
	case stdinWords > 1:
		return nil, nil, clierr.New(clierr.CodeValidation, "--from-file - is given more than once; stdin can be read once")
	case stdinWords == 1 && stdinIsTerminal(in):
		return nil, nil, clierr.New(clierr.CodeValidation, "--from-file - reads stdin, which is a terminal; pipe the value on stdin or give a file")
	}
	words := slices.Concat(sets, files)
	for i, w := range words {
		for _, first := range words[:i] {
			if first.field.Equal(w.field) {
				return nil, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
					"the %s --%s and the %s --%s give the same field; give each field once",
					ordinal(first.position), first.flag, ordinal(w.position), w.flag))
			}
		}
	}
	return sets, files, nil
}

// resolveCredentialDriver finds the credential config of driverName: a static
// row for s3, alibaba while its operator is active, or the node driver whose
// spec.displayName it is. gone returns the refusal when the server has no such
// driver; it gets the drivers the server has and whether alibaba is active.
func resolveCredentialDriver(ctx context.Context, client engine.ClusterClient, driverName string, gone func(driverName string, drivers []cloudcredfields.NodeDriver, alibabaActive bool) error) (cloudcredfields.Driver, error) {
	if driverName == "s3" {
		d, _ := cloudcredfields.Static("s3")
		return d, nil
	}
	if driverName == "alibaba" {
		alibabaActive, err := alibabaOperatorActive(ctx, client)
		if err != nil {
			return cloudcredfields.Driver{}, err
		}
		if !alibabaActive {
			return cloudcredfields.Driver{}, clierr.New(clierr.CodeValidation,
				"the alibaba operator is not active: the setting "+kev2OperatorsSetting+" marks it inactive; an administrator activates it first")
		}
		d, _ := cloudcredfields.Static("alibaba")
		return d, nil
	}
	list, err := client.List(ctx, nodeDriverGVR, "", "")
	if err != nil {
		return cloudcredfields.Driver{}, err
	}
	drivers := make([]cloudcredfields.NodeDriver, 0, len(list.Items))
	for i := range list.Items {
		drivers = append(drivers, cloudcredfields.NodeDriverOf(list.Items[i].Object))
	}
	for _, n := range drivers {
		if n.DisplayName != driverName {
			continue
		}
		if !n.HasSchema() {
			return cloudcredfields.Driver{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("node driver %s is not active; an administrator activates it", strconv.Quote(driverName))).
				WithHintCommand(clierr.NewCommand("patch nodedriver").Arg(n.ID).FlagText(` --patch '{"spec":{"active":true}}'`).Flag("--dry-run"))
		}
		d, ok := n.Driver()
		if !ok {
			return cloudcredfields.Driver{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("node driver %s has no annotations that name credential fields; Rancher has no cloud credential for it", strconv.Quote(driverName)))
		}
		return d, nil
	}
	alibabaActive, err := alibabaOperatorActive(ctx, client)
	if err != nil {
		alibabaActive = false
	}
	return cloudcredfields.Driver{}, gone(driverName, drivers, alibabaActive)
}

// unknownDriverError is the refusal of a create for a driver the server does
// not have.
func unknownDriverError(driverName string, drivers []cloudcredfields.NodeDriver, alibabaActive bool) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("unknown cloud credential driver %s; drivers with a credential on this server: %s",
			strconv.Quote(driverName), strings.Join(cloudcredfields.OfferedPrefixes(drivers, alibabaActive), ", ")))
}

// alibabaOperatorActive reads the setting that says whether the alibaba
// operator is active. A missing setting means it is not.
func alibabaOperatorActive(ctx context.Context, client engine.ClusterClient) (bool, error) {
	obj, err := client.Get(ctx, settingGVR, "", kev2OperatorsSetting)
	switch {
	case clierr.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return cloudcredfields.OperatorActive(obj.Object, alibabaOperator), nil
}

// checkFieldFlags refuses a field the driver does not have, a secret field in
// --set, and a public field in --from-file unless it has several lines.
func checkFieldFlags(d cloudcredfields.Driver, sets, files []fieldWord) error {
	for _, w := range sets {
		f, ok := d.Match(fieldNamed(w.field))
		switch {
		case !ok:
			if folded, ok := d.Match(func(f cloudcredfields.Field) bool { return f.Secret && w.field.EqualFoldString(f.Name) }); ok {
				typed, _ := typedFieldName(w.field, d)
				return secretInSetError(typed, folded.Name)
			}
			return unknownFieldError(d, w)
		case f.Secret:
			return secretInSetError(f.Name, f.Name)
		}
	}
	for _, w := range files {
		f, ok := d.Match(fieldNamed(w.field))
		switch {
		case !ok:
			return unknownFieldError(d, w)
		case !f.Secret && !f.Multiline:
			return clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"%s is a public field of %s; give it with --set %s=<value>", quoteField(f.Name), d.Prefix, f.Name))
		}
	}
	return nil
}

func secretInSetError(typed, canonical string) error {
	msg := fmt.Sprintf("%s is a secret field and cannot be given with --set; read it from a file with --from-file %s=<path>", quoteField(typed), canonical)
	if typed != canonical {
		msg = fmt.Sprintf("%s is the secret field %s and cannot be given with --set; read it from a file with --from-file %s=<path>", quoteField(typed), canonical, canonical)
	}
	return clierr.New(clierr.CodeValidation, msg)
}

// unknownFieldError refuses an argument whose field name is not a field of the
// driver. It quotes the field name only when it equals a field of the driver
// without regard to case; any other name may be a pasted secret and is named by
// its flag and position.
func unknownFieldError(d cloudcredfields.Driver, w fieldWord) error {
	subject := w.named()
	if typed, ok := typedFieldName(w.field, d); ok {
		subject = quoteField(typed) + " after --" + w.flag
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf("%s is not a field of %s; its fields are %s",
		subject, d.Prefix, strings.Join(d.Names(func(cloudcredfields.Field) bool { return true }), ", ")))
}

// readWords reads each --from-file value and checks every value against the
// white space rules. warnings names a value the command trimmed.
func readWords(src engine.FileSource, d cloudcredfields.Driver, sets, files []fieldWord) ([]credentialValue, []string, error) {
	var out []credentialValue
	var warnings []string
	for _, w := range sets {
		f, _ := d.Match(fieldNamed(w.field))
		cleaned, problem, trimmed := cleanValue(f, w.value, false)
		if err := valueProblemError(problem, f, "--"+flagSet, ""); err != nil {
			return nil, nil, err
		}
		if trimmed {
			warnings = append(warnings, fmt.Sprintf("the value of %s was trimmed of white space at its ends, as Rancher trims it", f.Name))
		}
		out = append(out, credentialValue{field: f, value: cleaned})
	}
	for _, w := range files {
		f, _ := d.Match(fieldNamed(w.field))
		path := fileWordPath(w)
		value, err := secret.Read(src.Read, path)
		if err != nil {
			return nil, nil, clierr.New(clierr.CodeValidation, "cannot read the --from-file file "+path)
		}
		cleaned, problem, trimmed := cleanValue(f, value, true)
		if err := valueProblemError(problem, f, "--"+flagFromFile, path); err != nil {
			return nil, nil, err
		}
		if trimmed {
			warnings = append(warnings, fmt.Sprintf("the value of %s was trimmed of white space at its ends, as Rancher trims it", f.Name))
		}
		if d.Prefix == "harvester" && f.Name == "kubeconfigContent" && !decodesAsKubeconfig(cleaned) {
			return nil, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the --from-file file %s for kubeconfigContent is not a kubeconfig", path))
		}
		out = append(out, credentialValue{field: f, value: cleaned, secret: f.Secret})
	}
	slices.SortFunc(out, func(a, b credentialValue) int { return strings.Compare(a.field.Name, b.field.Name) })
	return out, warnings, nil
}

// cleanValue checks a value against the white space rules of its field and
// returns the cleaned value, the problem found and whether it trimmed the ends.
// A value read from a file or stdin first gets the encoding check and loses the
// byte order marks at its start; a typed --set value gets the rules only.
func cleanValue(f cloudcredfields.Field, v secret.Value, fromFile bool) (secret.Value, cloudcredfields.Problem, bool) {
	text := secret.Reveal(v)
	if fromFile {
		if cloudcredfields.RawEncodingProblem([]byte(text)) != cloudcredfields.ProblemNone {
			return secret.Value{}, cloudcredfields.ProblemEncoding, false
		}
		text = string(engine.DropLeadingByteOrderMarks([]byte(text)))
	}
	cleaned, problem, trimmed := cloudcredfields.Clean(f, text, fromFile)
	return secret.New(cleaned), problem, trimmed
}

// valueProblemError writes the refusal of a value: it names the field, the flag
// and, for a file, its path as typed, never a byte of the value.
func valueProblemError(p cloudcredfields.Problem, f cloudcredfields.Field, flag, path string) error {
	where := flag
	switch path {
	case "":
	case "-":
		where = "stdin"
	default:
		where = path
	}
	switch p {
	case cloudcredfields.ProblemEncoding:
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"the value of %s read from %s is not UTF-8 text: it starts with a UTF-16 mark or has a NUL byte; save it as UTF-8 without a byte order mark and try again", f.Name, where))
	case cloudcredfields.ProblemLineBreak:
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("the value of %s holds a line break; %s takes one line", f.Name, f.Name))
	case cloudcredfields.ProblemEdgeSpace:
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"Rancher removes leading and trailing white space from %s; remove it from the value", f.Name))
	}
	return nil
}

// decodesAsKubeconfig reports whether v is a YAML or JSON document with a
// cluster, a user or a context, the shape of a kubeconfig.
func decodesAsKubeconfig(v secret.Value) bool {
	var doc struct {
		Clusters []any `json:"clusters"`
		Users    []any `json:"users"`
		Contexts []any `json:"contexts"`
	}
	if err := yaml.Unmarshal([]byte(secret.Reveal(v)), &doc); err != nil {
		return false
	}
	return len(doc.Clusters)+len(doc.Users)+len(doc.Contexts) > 0
}

// checkRequired refuses a missing required field, from the names the command
// line gives, before stdin or any file is read.
func checkRequired(d cloudcredfields.Driver, sets, files []fieldWord) error {
	var missing []string
	for _, f := range d.Fields {
		if f.Required && !slices.ContainsFunc(slices.Concat(sets, files), func(w fieldWord) bool { return w.field.EqualString(f.Name) }) {
			flag := "--set " + f.Name + "=<value>"
			if f.Secret {
				flag = "--from-file " + f.Name + "=<path>"
			}
			missing = append(missing, f.Name+" ("+flag+")")
		}
	}
	if len(missing) > 0 {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("%s needs these fields: %s", d.Prefix, strings.Join(missing, ", ")))
	}
	return nil
}

// requestDocument builds the JSON the create sends and the preview's diff.
func requestDocument(d cloudcredfields.Driver, displayName, description string, values []credentialValue) (map[string]any, []safety.DiffEntry) {
	config := map[string]any{}
	diff := []safety.DiffEntry{{Path: "/name", To: displayName}}
	if description != "" {
		diff = append(diff, safety.DiffEntry{Path: "/description", To: description})
	}
	label := cloudcredfields.Label(d.Prefix)
	diff = append(diff, safety.DiffEntry{Path: engine.PointerFor([]string{"annotations", driverAnnotation}), To: label})
	configKey := d.Prefix + "credentialConfig"
	for _, v := range values {
		config[v.field.Name] = v.value
		diff = append(diff, safety.DiffEntry{Path: engine.PointerFor([]string{configKey, v.field.Name}), To: shownValue(v)})
	}
	doc := map[string]any{
		"type":        "cloudCredential",
		"name":        displayName,
		"annotations": map[string]any{driverAnnotation: label},
		configKey:     config,
	}
	if description != "" {
		doc["description"] = description
	}
	return doc, diff
}

// shownValue is what the preview's diff shows for a value: a public field's
// value as typed, and engine.Redacted for a secret field's.
func shownValue(v credentialValue) any {
	if v.field.Secret {
		return engine.Redacted
	}
	return secret.Reveal(v.value)
}

// responseFilter returns the function that hides the secret values the request
// carried, and the extra texts it sent along, in a text of the server.
func responseFilter(values []credentialValue, extra ...string) func(string) string {
	var secrets []secret.Value
	for _, v := range values {
		if v.field.Secret {
			secrets = append(secrets, v.value)
		}
	}
	return engine.NewSecretFilter(secrets, extra...)
}

// CredentialName is the id and display name of one stored cloud credential.
type CredentialName struct{ ID, DisplayName string }

// CredentialNames lists the cloud credentials as id and display name pairs, from
// the same List and the same credential rule `get cloud-credential` uses. refused
// reports that the List was refused with RBAC, as it is for a standard user,
// because Rancher grants a credential's creator access by name only. It keeps
// key names and metadata only.
func CredentialNames(ctx context.Context, client engine.ClusterClient) (names []CredentialName, refused bool, err error) {
	list, err := client.List(ctx, secretsGVR, cloudCredNamespace, "")
	if err != nil {
		if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeRBAC {
			return nil, true, nil
		}
		return nil, false, err
	}
	for _, obj := range credentialObjects(list) {
		names = append(names, CredentialName{ID: obj.GetName(), DisplayName: displayNameOf(obj)})
	}
	return names, false, nil
}

// checkDisplayName refuses a display name another credential has, and returns
// the warning for a List that was refused. exceptID is the Secret name of a
// credential that may keep the name, or "".
func checkDisplayName(ctx context.Context, client engine.ClusterClient, displayName, exceptID string) ([]string, error) {
	names, listRefused, err := CredentialNames(ctx, client)
	if err != nil {
		return nil, err
	}
	if listRefused {
		return []string{"the display name was not checked against the existing cloud credentials because you cannot list them; Rancher allows two credentials with one display name"}, nil
	}
	names = slices.DeleteFunc(names, func(n CredentialName) bool { return exceptID != "" && n.ID == exceptID })
	if other, ok := nameInUse(names, displayName); ok {
		return nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("a cloud credential with the display name %s exists (%s); choose another display name, because two credentials with one display name are hard to tell apart",
				strconv.Quote(displayName), other)).WithHintCommand(clierr.NewCommand("get cloud-credential"))
	}
	return nil, nil
}

// nameInUse finds a credential with this display name and returns the text that
// names it, "id <id>".
func nameInUse(names []CredentialName, displayName string) (string, bool) {
	for _, n := range names {
		if n.DisplayName == displayName {
			return "id " + n.ID, true
		}
	}
	return "", false
}

// reviewCredentialSecrets asks the cluster whether the caller may do verb on
// Secrets in cattle-global-data: on the Secret name when it is not empty, else
// on any. A failed request has no response: reviewed is false.
func reviewCredentialSecrets(ctx context.Context, client engine.ClusterClient, verb, name string) (allowed, reviewed bool) {
	gvk, ok := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	if !ok {
		return false, false
	}
	attributes := map[string]any{
		"verb": verb, "group": "", "resource": secretsGVR.Resource, "namespace": cloudCredNamespace,
	}
	if name != "" {
		attributes["name"] = name
	}
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
		"spec": map[string]any{"resourceAttributes": attributes},
	}}
	resp, err := client.PostReview(ctx, engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: review})
	if err != nil || resp.Status < 200 || resp.Status > 299 || resp.Object == nil {
		return false, false
	}
	allowed, _, _ = unstructured.NestedBool(resp.Object.Object, "status", "allowed")
	return allowed, true
}

// execute sends the create once and, with wait, waits until the caller can
// read the new Secret. Every error that prints server text runs the filter.
func (c *createCloudCredentialCmd) execute(ctx context.Context, body engine.CloudCredentialBody, filter func(string) string, prefix, label, displayName string, wait bool) (runtime.Result, error) {
	client, err := c.creds.CloudCredentials()
	if err != nil {
		return runtime.Result{}, err
	}
	id, err := client.Create(ctx, body)
	if err != nil {
		return runtime.Result{}, credentialError(err, filter, credentialRequest{
			method: http.MethodPost, subject: "cloud credentials", done: "created", checkArg: displayName})
	}
	name := strings.TrimPrefix(id, cloudCredNamespace+":")
	ref := CloudCredRef{ID: cloudCredNamespace + ":" + name, SecretName: name, DisplayName: displayName, Driver: prefix}
	res := runtime.Result{Kind: "CloudCredential", Data: createdCredentialRows{ref}}
	if wait {
		if warning := c.waitReadable(ctx, name); warning != "" {
			res.Warnings = append(res.Warnings, warning)
		}
	}
	return res, nil
}

// errNotReadable ends the wait for the new Secret without reading it.
var errNotReadable = errors.New("the new cloud credential is not readable yet")

// waitReadable waits until the caller can read the new Secret, for at most the
// wait's time limit. Rancher gives the creator access shortly after it creates
// the credential, so a missing Secret and a refused read mean not yet. It returns a warning
// when the wait ended without reading it: the credential exists.
func (c *createCloudCredentialCmd) waitReadable(ctx context.Context, name string) string {
	interval, timeout := cloudCredentialWaitValues()
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return notReadableWarning(name)
	}
	err = engine.WaitObject(ctx, client, engine.WaitParams{
		GVR:         secretsGVR,
		Namespace:   cloudCredNamespace,
		Name:        name,
		Timeout:     timeout,
		Interval:    interval,
		SpacingFrom: engine.SpacingFromReturn,
		WhileAbsent: true,
		Observe: func(context.Context, *unstructured.Unstructured, engine.WaitSource) (engine.WaitOutcome, error) {
			return engine.WaitDone, nil
		},
		RecordErr: func(context.Context, error) bool { return false },
		OnAbort:   func(error) error { return errNotReadable },
		OnCancel:  func(context.Context, *unstructured.Unstructured, *clierr.Error) error { return errNotReadable },
		OnTimeout: func(*unstructured.Unstructured, *clierr.Error) error { return errNotReadable },
	})
	if err != nil {
		return notReadableWarning(name)
	}
	return ""
}

func notReadableWarning(name string) string {
	return clierr.Text("the cloud credential was created, but you cannot read it yet; Rancher grants the creator access shortly after it creates the credential",
		clierr.Lead("; read it with: "), clierr.NewCommand("get cloud-credential").Arg(name))
}

// credentialRequest names the Norman request an error is about: its method, how
// the refusal names it, the verb of the outcome that is not known and the
// argument of the hint that checks the credential.
type credentialRequest struct {
	method  string
	subject string
	// done is the verb of the unknown outcome: "created", "changed" or
	// "deleted".
	done     string
	checkArg string
	// existing is true for a request about a credential that exists: its 404
	// points to the list of credentials.
	existing bool
	// resendsKubeconfig is true when the body carries the kubeconfigContent
	// of a Harvester credential.
	resendsKubeconfig bool
}

// credentialError turns an error of the Norman client into the command's
// error, with the values the request carried hidden in every text of the
// server.
func credentialError(err error, filter func(string) string, req credentialRequest) error {
	if r, ok := errors.AsType[*engine.ActionRefused](err); ok {
		refused := engine.RefusedError(req.method, req.subject, r, filter)
		if ce, ok := errors.AsType[*clierr.Error](refused); ok && req.existing && ce.Code == clierr.CodeNotFound {
			return ce.WithHintCommand(clierr.NewCommand("get cloud-credential"))
		}
		return refused
	}
	if u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err); ok {
		said := clierr.FilteredServerMessage(u.Body, filter)
		if tail, ok := strings.CutPrefix(said, harvesterRefusalPrefix); ok {
			return harvesterRefusal(tail, req.resendsKubeconfig)
		}
		cause := "no response arrived"
		if status, ok := clierr.HTTPStatus(u.Err); ok {
			cause = "Rancher responded with " + strconv.Itoa(status)
		}
		if said != "" {
			cause += ": " + said
		}
		return clierr.New(clierr.CodeGeneric, clierr.Text(
			"the request was sent, but the response did not say whether Rancher "+req.done+" the cloud credential ("+cause+")",
			clierr.Lead("; before you try again, check with: "),
			clierr.NewCommand("get cloud-credential").Arg(req.checkArg))).WithRetryable(false)
	}
	return engine.FilterError(err, filter)
}

// harvesterRefusal is the error for a 500 whose message starts with the
// prefix Rancher wraps its Harvester kubeconfig checks in: nothing was
// written. tail is Rancher's message after that prefix, already filtered. The
// parser's own error can quote a fragment of the kubeconfig, so a fixed text
// replaces it. A kubeconfig that a request sends again is refused when its
// token is in use; the text then gives the remedy.
func harvesterRefusal(tail string, resendsKubeconfig bool) error {
	if strings.Contains(tail, harvesterParserMarker) {
		return clierr.New(clierr.CodeValidation, "Rancher could not read the tokens in the kubeconfig given for kubeconfigContent; check that it is a kubeconfig of the Harvester cluster")
	}
	msg := "Rancher refused the kubeconfig: " + tail
	if resendsKubeconfig && strings.Contains(tail, harvesterTokenInUse) {
		msg += "; remove --from-file kubeconfigContent to keep the stored kubeconfig, or give a newly generated one"
	}
	return clierr.New(clierr.CodeValidation, msg)
}

// patchDocumentText is what the refusal of a patch document flag says after
// the flag as typed.
const patchDocumentText = "this command takes no patch document; give fields with --set <field>=<value>, secret values with --from-file <field>=<path>, and --display-name or --description"

type patchCloudCredentialCmd struct {
	clients engine.ClusterClientFactory
	creds   engine.CloudCredentialClientFactory
	src     engine.FileSource
}

// NewPatchCloudCredential builds `patch cloud-credential <credential>`
// (mutating).
func NewPatchCloudCredential(clients engine.ClusterClientFactory, creds engine.CloudCredentialClientFactory, src engine.FileSource) runtime.Command {
	return &patchCloudCredentialCmd{clients: clients, creds: creds, src: src}
}

func (patchCloudCredentialCmd) Describe() runtime.Descriptor {
	refused := make([]runtime.RefusedFlag, 0, 4)
	for _, name := range []string{"patch", "p", "patch-file", "type"} {
		refused = append(refused, runtime.RefusedFlag{Name: name, Text: patchDocumentText})
	}
	return runtime.Descriptor{
		Name:  "patch cloud-credential",
		Usage: "Change the fields, display name or description of a cloud credential",
		Desc: "Changes a cloud credential through Rancher's Norman API, the one API that runs " +
			"Rancher's credential checks. <credential> is the credential's id, as `rancher get cloud-credential` " +
			"shows it (with or without cattle-global-data:), not its display name. A value that is " +
			"the id of one credential and the display name of another is refused: give the id as " +
			"cattle-global-data:<id>. Give at least one of --set <field>=<value> (a public field), " +
			"--from-file <field>=<path> (a secret or multi-line field; a <path> of - reads stdin, once), " +
			"--display-name and --description; fields not given keep their values. " +
			"The command takes no patch document: --patch, -p, --patch-file and --type are refused. " +
			"Replacing a secret value needs --yes. The command is previewed from server reads: " +
			"--apply reads the cloud credential again and stops if it changed, and a change after " +
			"that read is not detected, because Rancher's Norman API takes no precondition. " +
			"In the diff of a preview, `from` is left out when the CLI did not read the stored " +
			"value, and is null when the field had no value.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "credential", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: flagSet, Type: "stringSlice", Desc: setFlagDesc, Secret: true},
			{Name: flagFromFile, Type: "stringSlice", Desc: fromFileFlagDesc, Secret: true},
			{Name: flagDisplayName, Type: "string", Desc: "the credential's new display `name`"},
			{Name: flagDescription, Type: "string", Desc: "the credential's new `description`"},
		},
		Examples: []string{
			"rancher patch cloud-credential cc-abc12 --from-file secretKey=./secret-key.txt --dry-run",
			"rancher patch cloud-credential cc-abc12 --description 'production account' --dry-run",
		},
		Destructive:  "dependsOnInput",
		Auth:         true,
		Shape:        runtime.ShapeTabular,
		SecretInput:  true,
		RefusedFlags: refused,
	}
}

// patchRun is what a patch's Execute needs from its Plan.
type patchRun struct {
	typed             string
	secretName        string
	resourceVersion   string
	displayName       string
	body              engine.CloudCredentialBody
	filter            func(string) string
	resendsKubeconfig bool
}

func (c *patchCloudCredentialCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	typed := in.Args()[0]

	sets, files, err := readFieldFlags(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	displayGiven, descriptionGiven := in.IsSet(flagDisplayName), in.IsSet(flagDescription)
	displayName, description := in.String(flagDisplayName), in.String(flagDescription)
	if len(sets)+len(files) == 0 && !displayGiven && !descriptionGiven {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "give at least one of --set, --from-file, --display-name or --description")
	}

	stored, err := readStoredCredential(ctx, c.clients, typed, "update", lookupForChange)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if stored.ref.Driver == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("the cloud credential %s has no fields of a node driver; the CLI cannot tell which driver it belongs to", stored.ref.SecretName))
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	driver, err := resolveCredentialDriver(ctx, client, stored.ref.Driver, func(driverName string, _ []cloudcredfields.NodeDriver, _ bool) error {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"the node driver %s of cloud credential %s is not on this server; Rancher cannot change a credential of a driver it does not have",
			driverName, stored.ref.SecretName))
	})
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := checkFieldFlags(driver, sets, files); err != nil {
		return safety.Effect{}, nil, err
	}

	values, valueWarnings, err := readWords(c.src, driver, sets, files)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if displayGiven {
		nameWarning, err := checkDisplayName(ctx, client, displayName, stored.ref.SecretName)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		valueWarnings = append(valueWarnings, nameWarning...)
	}

	var reviewWarnings []string
	if allowed, reviewed := reviewCredentialSecrets(ctx, client, "update", stored.ref.SecretName); reviewed && !allowed {
		return safety.Effect{}, nil, clierr.New(clierr.CodeRBAC, fmt.Sprintf(
			"you do not have permission to update cloud credential %s: Rancher gives that permission to the user who created it", stored.ref.SecretName))
	} else if !reviewed {
		reviewWarnings = append(reviewWarnings, "whether you have permission to update the cloud credential was not checked; Rancher checks it when the request is sent")
	}

	doc, diff := updateDocument(driver, stored, values, displayName, displayGiven, description, descriptionGiven)
	var annotationTexts []string
	if annotations, ok := doc["annotations"].(map[string]any); ok {
		for _, v := range annotations {
			if text, ok := v.(string); ok {
				annotationTexts = append(annotationTexts, text)
			}
		}
	}
	replacesSecret := slices.ContainsFunc(files, func(w fieldWord) bool {
		f, _ := driver.Match(fieldNamed(w.field))
		return f.Secret
	})
	resends := slices.ContainsFunc(files, func(w fieldWord) bool {
		return driver.Prefix == "harvester" && w.field.EqualString("kubeconfigContent")
	})

	warnings := []string{serverReadsWarning, "fields not given keep their values", readsAgainWarning}
	warnings = append(warnings, valueWarnings...)
	warnings = append(warnings, reviewWarnings...)

	eff := safety.Effect{
		Action:          safety.ActionUpdate,
		Target:          "cloudcredentials",
		DisplayKind:     "cloud-credential",
		DisplayName:     stored.ref.SecretName,
		PreviewSource:   safety.PreviewServerReads,
		Cluster:         engine.LocalCluster,
		Diff:            diff,
		ResourceVersion: stored.resourceVersion,
		Destructive:     replacesSecret,
		Warnings:        warnings,
	}
	run := patchRun{
		typed:             typed,
		secretName:        stored.ref.SecretName,
		resourceVersion:   stored.resourceVersion,
		displayName:       displayName,
		body:              engine.NewCloudCredentialBody(doc),
		filter:            responseFilter(values, annotationTexts...),
		resendsKubeconfig: resends,
	}
	return eff, func(ctx context.Context) (runtime.Result, error) { return c.execute(ctx, run) }, nil
}

// readsAgainWarning says what a patch or a delete checks before it writes and
// what it cannot check.
const readsAgainWarning = "--apply reads the cloud credential again and stops if it changed; a change after that read is not detected"

// updateDocument builds the body of an update and the preview's diff. The
// config object is always present, empty when no config field is given.
// With a new name or description the body also carries the stored annotations
// Rancher would remove when the body leaves them out, except the ones whose
// key has cattle.io/, which it protects.
func updateDocument(d cloudcredfields.Driver, stored storedCredential, values []credentialValue, displayName string, displayGiven bool, description string, descriptionGiven bool) (map[string]any, []safety.DiffEntry) {
	configKey := d.Prefix + "credentialConfig"
	config := map[string]any{}
	doc := map[string]any{configKey: config}
	var diff []safety.DiffEntry
	if displayGiven {
		doc["name"] = displayName
		diff = append(diff, safety.DiffEntry{Path: "/name", From: stored.ref.DisplayName, To: displayName})
	}
	if descriptionGiven {
		doc["description"] = description
		var from any
		if old, ok := stored.annotations[descriptionAnnotation]; ok {
			from = old
		}
		diff = append(diff, safety.DiffEntry{Path: "/description", From: from, To: description})
	}
	if displayGiven || descriptionGiven {
		kept := map[string]any{}
		for key, value := range stored.annotations {
			if !strings.Contains(key, "cattle.io/") {
				kept[key] = value
			}
		}
		doc["annotations"] = kept
	}
	for _, v := range values {
		config[v.field.Name] = v.value
		diff = append(diff, safety.DiffEntry{Path: engine.PointerFor([]string{configKey, v.field.Name}), To: shownValue(v), FromUnread: true})
	}
	return doc, diff
}

// execute reads the Secret again, stops when it changed since the preview, and
// sends the update once. A change after that read is not detected: Norman's
// update takes no precondition.
func (c *patchCloudCredentialCmd) execute(ctx context.Context, run patchRun) (runtime.Result, error) {
	ref, err := readAgain(ctx, c.clients, run.typed, run.secretName, "update", run.resourceVersion)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.creds.CloudCredentials()
	if err != nil {
		return runtime.Result{}, err
	}
	if err := client.Update(ctx, cloudCredNamespace+":"+run.secretName, run.body); err != nil {
		return runtime.Result{}, credentialError(err, run.filter, credentialRequest{
			method: http.MethodPut, subject: "cloud credentials", done: "changed", checkArg: run.secretName,
			existing: true, resendsKubeconfig: run.resendsKubeconfig})
	}
	if run.displayName != "" {
		ref.DisplayName = run.displayName
	}
	return runtime.Result{Kind: "CloudCredential", Data: createdCredentialRows{ref}}, nil
}

// readAgain reads the credential's Secret by name and refuses when its
// resourceVersion differs from the one the preview read: a patch or a delete
// stops, and nothing is sent. typed is the credential as the user typed it. verb
// is the verb of the write, in the hint of a refused read. It returns the
// credential's row.
func readAgain(ctx context.Context, clients engine.ClusterClientFactory, typed, name, verb, resourceVersion string) (CloudCredRef, error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return CloudCredRef{}, err
	}
	obj, err := client.Get(ctx, secretsGVR, cloudCredNamespace, name)
	switch {
	case clierr.IsNotFound(err):
		return CloudCredRef{}, clierr.New(clierr.CodeNotFound, "cloud credential not found: "+typed).WithHintCommand(clierr.NewCommand("get cloud-credential"))
	case err != nil:
		return CloudCredRef{}, engine.WithMissingGetHint(err, "command", "cloud-credential", secretsGVR, cloudCredNamespace, name, verb)
	case obj.GetResourceVersion() != resourceVersion:
		return CloudCredRef{}, clierr.New(clierr.CodeConflict, "the cloud credential changed since the preview; try again").WithRetryable(true)
	}
	return refFrom(obj), nil
}

var _ runtime.MutatingCommand = (*patchCloudCredentialCmd)(nil)

var _ runtime.MutatingCommand = (*createCloudCredentialCmd)(nil)

// deleteServerReadsWarning is the preview warning of a delete: no provider
// check applies to it.
const deleteServerReadsWarning = "previewed from server reads: Rancher checks the request only when it is sent"

// hostedClusterFields are the paths of the fields of a management cluster that
// name a cloud credential for a hosted cluster. Rancher does not check them
// when it deletes a credential.
var hostedClusterFields = [][]string{
	{"spec", "eksConfig", "amazonCredentialSecret"},
	{"spec", "aksConfig", "azureCredentialSecret"},
	{"spec", "gkeConfig", "googleCredentialSecret"},
	{"spec", "aliConfig", "alibabaCredentialSecret"},
}

const etcdS3Field = "spec.rkeConfig.etcd.s3.cloudCredentialName"

type deleteCloudCredentialCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
	creds   engine.CloudCredentialClientFactory
}

// NewDeleteCloudCredential builds `delete cloud-credential <credential>`
// (mutating). roots may be nil: a caller who cannot list the clusters through
// the Kubernetes API then gets the warnings for clusters that were not
// checked.
func NewDeleteCloudCredential(clients engine.ClusterClientFactory, roots engine.RootClientFactory, creds engine.CloudCredentialClientFactory) runtime.Command {
	return &deleteCloudCredentialCmd{clients: clients, roots: roots, creds: creds}
}

func (deleteCloudCredentialCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "delete cloud-credential",
		Usage: "Delete a cloud credential",
		Desc: "Deletes a cloud credential through Rancher's Norman API, which refuses the delete while a " +
			"provisioning cluster uses the credential. <credential> is the credential's id, as " +
			"`rancher get cloud-credential` shows it (with or without cattle-global-data:), not its " +
			"display name. A value that is the id of one credential and the display name of another is " +
			"refused: give the id as cattle-global-data:<id>. The preview refuses when a provisioning " +
			"cluster uses the credential. It warns for each hosted cluster that uses it and for each " +
			"provisioning cluster that uses it only for etcd snapshots in S3; Rancher does not check " +
			"these references. The command is previewed from server reads: " +
			"--apply reads the cloud credential again and stops if it changed, and a change after that " +
			"read is not detected. Without --wait=false the command waits until the credential is gone.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "credential", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: flagWait, Type: "bool", Desc: "wait until the cloud credential is gone before returning (default true; --wait=false returns once the server accepts the request)"},
			{Name: flagTimeout, Type: "string", Desc: "wait `timeout`, a positive duration, e.g. 10m (default 5m)"},
		},
		Examples:    []string{"rancher delete cloud-credential cc-abc12 --dry-run"},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeTabular,
	}
}

// deleteRun is what a delete's Execute needs from its Plan.
type deleteRun struct {
	typed           string
	secretName      string
	resourceVersion string
	uid             string
	wait            resources.DeleteWait
}

func (c *deleteCloudCredentialCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	typed := in.Args()[0]
	wait, err := resources.ParseDeleteWait(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	stored, err := readStoredCredential(ctx, c.clients, typed, "delete", lookupForChange)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	name := stored.ref.SecretName

	provisioning, provisioningRefused, provisioningSteve, err := c.listClusters(ctx, client, provisioningClusterGVR, resources.ProvisioningClustersAfterForbidden)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	slices.SortFunc(provisioning, func(a, b unstructured.Unstructured) int {
		return cmp.Or(strings.Compare(a.GetNamespace(), b.GetNamespace()), strings.Compare(a.GetName(), b.GetName()))
	})
	var etcdWarnings []string
	for _, cluster := range provisioning {
		if usesCredential(cluster, name) {
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"Cloud credential is currently referenced by provisioning cluster %s (namespace %s)",
				cluster.GetName(), cluster.GetNamespace()))
		}
		if value, _, _ := unstructured.NestedString(cluster.Object, "spec", "rkeConfig", "etcd", "s3", "cloudCredentialName"); namesCredential(value, name) {
			etcdWarnings = append(etcdWarnings, fmt.Sprintf(
				"provisioning cluster %s in namespace %s uses this cloud credential for etcd snapshots in S3 (%s); Rancher does not check that reference and deletes the credential",
				cluster.GetName(), cluster.GetNamespace(), etcdS3Field))
		}
	}

	management, managementRefused, managementSteve, err := c.listClusters(ctx, client, managementClusterGVR, resources.ManagementClustersAfterForbidden)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	slices.SortFunc(management, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	var hostedWarnings []string
	for _, cluster := range management {
		shown, _, _ := unstructured.NestedString(cluster.Object, "spec", "displayName")
		if shown == "" {
			shown = cluster.GetName()
		}
		for _, field := range hostedClusterFields {
			if value, _, _ := unstructured.NestedString(cluster.Object, field...); namesCredential(value, name) {
				hostedWarnings = append(hostedWarnings, fmt.Sprintf(
					"cluster %s (%s) uses this cloud credential in %s; Rancher does not check that reference and deletes the credential",
					shown, cluster.GetName(), strings.Join(field, ".")))
			}
		}
	}

	warnings := []string{deleteServerReadsWarning, readsAgainWarning}
	warnings = append(warnings, hostedWarnings...)
	warnings = append(warnings, etcdWarnings...)
	if provisioningRefused {
		warnings = append(warnings, "the provisioning clusters were not checked for references to this cloud credential because you do not have permission to list them; Rancher refuses the delete when a provisioning cluster uses it")
	}
	if managementRefused {
		warnings = append(warnings, "the hosted clusters (EKS, AKS, GKE and Alibaba ACK) were not checked for references to this cloud credential because you do not have permission to list the management clusters")
	}
	if provisioningSteve {
		warnings = append(warnings, fmt.Sprintf(readableOnlyWarning, "provisioning clusters"))
	}
	if managementSteve {
		warnings = append(warnings, fmt.Sprintf(readableOnlyWarning, "management clusters"))
	}

	eff := safety.Effect{
		Action:          safety.ActionDelete,
		Target:          "cloudcredentials",
		DisplayKind:     "cloud-credential",
		DisplayName:     name,
		PreviewSource:   safety.PreviewServerReads,
		Cluster:         engine.LocalCluster,
		ResourceVersion: stored.resourceVersion,
		Destructive:     true,
		Warnings:        warnings,
	}
	run := deleteRun{typed: typed, secretName: name, resourceVersion: stored.resourceVersion, uid: stored.uid, wait: wait}
	return eff, func(ctx context.Context) (runtime.Result, error) { return c.execute(ctx, run) }, nil
}

// readableOnlyWarning says that a list the Steve API returns has only what the
// caller has permission to get; its argument is the plain name of the list.
const readableOnlyWarning = "only the %s you have permission to get were checked for references to this cloud credential"

// listClusters lists the clusters of gvr in every namespace through the
// Kubernetes API and, when that responds with 403, through the Steve read given. A
// List that stays refused is no error: refused is true and the caller says so.
// steve is true when Steve responded, so the items are the clusters the
// caller has permission to get.
func (c *deleteCloudCredentialCmd) listClusters(ctx context.Context, client engine.ClusterClient, gvr schema.GroupVersionResource,
	steveRead func(context.Context, engine.RootClientFactory, error) ([]unstructured.Unstructured, error)) (items []unstructured.Unstructured, refused, steve bool, err error) {
	list, err := client.List(ctx, gvr, "", "")
	if err == nil {
		return list.Items, false, false, nil
	}
	if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeRBAC {
		return nil, false, false, err
	}
	if c.roots == nil {
		return nil, true, false, nil
	}
	items, err = steveRead(ctx, c.roots, err)
	if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeRBAC {
		return nil, true, false, nil
	}
	return items, false, err == nil, err
}

// usesCredential reports whether the provisioning cluster names the credential,
// for the whole cluster or a machine pool, as cattle-global-data:<name>: the
// exact form Rancher's check compares.
func usesCredential(cluster unstructured.Unstructured, name string) bool {
	want := cloudCredNamespace + ":" + name
	if value, _, _ := unstructured.NestedString(cluster.Object, "spec", "cloudCredentialSecretName"); value == want {
		return true
	}
	pools, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
	for _, pool := range pools {
		if m, ok := pool.(map[string]any); ok {
			if value, _ := m["cloudCredentialSecretName"].(string); value == want {
				return true
			}
		}
	}
	return false
}

// namesCredential reports whether a reference Rancher does not check names the
// credential, with its namespace or without it.
func namesCredential(value, name string) bool {
	return value != "" && (value == cloudCredNamespace+":"+name || value == name)
}

// execute reads the Secret again, stops when it changed since the preview,
// sends the delete once and, unless --wait=false, waits until the Secret is
// gone. A change after the read is not detected: Norman's delete takes no
// precondition.
func (c *deleteCloudCredentialCmd) execute(ctx context.Context, run deleteRun) (runtime.Result, error) {
	ref, err := readAgain(ctx, c.clients, run.typed, run.secretName, "delete", run.resourceVersion)
	if err != nil {
		return runtime.Result{}, err
	}
	creds, err := c.creds.CloudCredentials()
	if err != nil {
		return runtime.Result{}, err
	}
	if err := creds.Delete(ctx, cloudCredNamespace+":"+run.secretName); err != nil {
		return runtime.Result{}, credentialError(err, engine.NewTextFilter(nil, true), credentialRequest{
			method: http.MethodDelete, subject: "cloud credentials", done: "deleted", checkArg: run.secretName, existing: true})
	}
	res := runtime.Result{Kind: "CloudCredential", Data: createdCredentialRows{ref}}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	res.Warnings, err = resources.WaitCloudCredentialDeleted(ctx, client, run.wait, run.secretName, run.uid)
	if err != nil {
		return runtime.Result{}, err
	}
	return res, nil
}

var _ runtime.MutatingCommand = (*deleteCloudCredentialCmd)(nil)

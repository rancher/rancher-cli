// cloudcredwrite.go implements `create cloud-credential <driver>
// <display-name>`: the one command that writes a cloud credential. Rancher
// serves no API but Norman that runs its credential checks, so the write goes
// through engine.CloudCredentialClient. The command keeps the secret values it
// sends only in engine.CloudCredentialBody, in the list the error filter
// builds and, while Plan reads them, in credentialValue; no preview, result,
// warning or error shows one.

package provisioning

import (
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
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// nodeDriverGVR and settingGVR are the Rancher kinds the command reads.
var (
	nodeDriverGVR = kindGVR("nodedriver")
	settingGVR    = kindGVR("setting")
)

func kindGVR(name string) schema.GroupVersionResource {
	k, _ := kinds.Lookup(name)
	return k.GVR
}

const (
	flagSet         = "set"
	flagFromFile    = "from-file"
	flagDescription = "description"
	flagWait        = "wait"

	// driverAnnotation is the annotation Rancher's UI reads to list a credential
	// in its cluster forms.
	driverAnnotation = "provisioning.cattle.io/driver"

	// harvesterRefusalPrefix starts the message of a 500 Rancher responds with when
	// its check of a Harvester kubeconfig fails; nothing was written.
	harvesterRefusalPrefix = "failed to process harvester cloud credential: "
	// harvesterParserMarker is the text in the tail of that message that comes
	// before the parser's error, which can quote a fragment of the kubeconfig.
	harvesterParserMarker = "tokens from kubeconfig:"

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
			{Name: flagSet, Type: "stringSlice", Desc: "a public field of the driver, as `field=value`; repeat the flag for more fields"},
			{Name: flagFromFile, Type: "stringSlice", Desc: "a secret or multi-line field of the driver and the file that has its value, as `field=path`; a path of - reads stdin once; repeat the flag for more fields"},
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
	flag, field, value string
	position           int
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
func splitFieldWords(flag string, words []string) ([]fieldWord, error) {
	out := make([]fieldWord, 0, len(words))
	valuePart := "value"
	if flag == flagFromFile {
		valuePart = "path"
	}
	for i, w := range words {
		field, value, ok := strings.Cut(w, "=")
		switch {
		case !ok:
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s --%s must be <field>=<%s>", ordinal(i+1), flag, valuePart))
		case field == "":
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s --%s has an empty field name", ordinal(i+1), flag))
		case value == "":
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s --%s has an empty %s", ordinal(i+1), flag, valuePart))
		}
		out = append(out, fieldWord{flag: flag, field: field, value: value, position: i + 1})
	}
	return out, nil
}

// quoteField quotes a field name the user typed that is already known to be a
// field of the driver, or to equal one without regard to case.
func quoteField(field string) string { return strconv.Quote(field) }

func stdinIsTerminal(in runtime.Input) bool {
	si, ok := in.(runtime.StdinInput)
	return ok && si.StdinTerminal()
}

// credentialValue is one config field to send.
type credentialValue struct {
	field  cloudcredfields.Field
	value  string
	secret bool
}

func (c *createCloudCredentialCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	driverName, displayName := args[0], args[1]

	// Every error of readFieldFlags, checkFieldFlags and readWords leaves
	// through read.refuse, which hides the --set values and the values read so
	// far; until readWords runs, only the --set values are known.
	var read readTexts
	read.recordSetWords(in.Strings(flagSet))
	sets, files, err := readFieldFlags(in)
	if err != nil {
		return safety.Effect{}, nil, read.refuse(err)
	}
	if displayName == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "the display name is empty; give the name the credential shows in Rancher")
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	driver, err := resolveCredentialDriver(ctx, client, driverName)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := checkFieldFlags(driver, sets, files); err != nil {
		return safety.Effect{}, nil, read.refuse(err)
	}

	if err := checkRequired(driver, sets, files); err != nil {
		return safety.Effect{}, nil, err
	}

	values, warnings, err := c.readWords(driver, sets, files, read.record)
	if err != nil {
		return safety.Effect{}, nil, read.refuse(err)
	}

	names, listRefused, err := CredentialNames(ctx, client)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if listRefused {
		warnings = append(warnings, "the display name was not checked against the existing cloud credentials because you cannot list them; Rancher allows two credentials with one display name")
	} else if other, ok := nameInUse(names, displayName); ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("a cloud credential with the display name %s exists (%s); choose another display name, because --cloud-credential of create cluster could not tell the two apart",
				strconv.Quote(displayName), other)).WithHintCommand(clierr.NewCommand("get cloud-credential"))
	}

	if allowed, reviewed := canCreateCredentialSecrets(ctx, client); reviewed && !allowed {
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
		"previewed from server reads: Rancher checks the request only when it is sent; it does not check the values against the provider",
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
	sets, err = splitFieldWords(flagSet, in.Strings(flagSet))
	if err != nil {
		return nil, nil, err
	}
	files, err = splitFieldWords(flagFromFile, in.Strings(flagFromFile))
	if err != nil {
		return nil, nil, err
	}
	stdinWords := 0
	for _, f := range files {
		if f.value == "-" {
			stdinWords++
		}
	}
	switch {
	case stdinWords > 1:
		return nil, nil, clierr.New(clierr.CodeValidation, "--from-file - is given more than once; stdin can be read once")
	case stdinWords == 1 && stdinIsTerminal(in):
		return nil, nil, clierr.New(clierr.CodeValidation, "--from-file - reads stdin, which is a terminal; pipe the value on stdin or give a file")
	}
	seen := map[string]fieldWord{}
	for _, w := range slices.Concat(sets, files) {
		if first, ok := seen[w.field]; ok {
			return nil, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"the %s --%s and the %s --%s give the same field; give each field once",
				ordinal(first.position), first.flag, ordinal(w.position), w.flag))
		}
		seen[w.field] = w
	}
	return sets, files, nil
}

// resolveCredentialDriver finds the credential config of driverName: a static
// row for s3, alibaba while its operator is active, or the node driver whose
// spec.displayName it is.
func resolveCredentialDriver(ctx context.Context, client engine.ClusterClient, driverName string) (cloudcredfields.Driver, error) {
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
	return cloudcredfields.Driver{}, clierr.New(clierr.CodeValidation,
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
		f, ok := d.Field(w.field)
		switch {
		case !ok:
			if secret, folded := d.SecretFolded(w.field); folded {
				return secretInSetError(w.field, secret.Name)
			}
			return unknownFieldError(d, w)
		case f.Secret:
			return secretInSetError(w.field, f.Name)
		}
	}
	for _, w := range files {
		f, ok := d.Field(w.field)
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
	if _, ok := d.FieldFolded(w.field); ok {
		subject = quoteField(w.field) + " after --" + w.flag
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf("%s is not a field of %s; its fields are %s",
		subject, d.Prefix, strings.Join(d.Names(func(cloudcredfields.Field) bool { return true }), ", ")))
}

// readTexts has the texts of the command line that no refusal may show: the
// value part of each --set argument as typed, and every form of the values
// readWords has read.
type readTexts struct{ forms []string }

// record adds the redaction forms of each text.
func (r *readTexts) record(texts ...string) { r.forms = append(r.forms, redactionTexts(texts)...) }

// recordSetWords records the part after the first "=" of each --set argument as
// typed. The part before it is not recorded: it can equal a field name, which
// the refusals list.
func (r *readTexts) recordSetWords(words []string) {
	for _, w := range words {
		if _, value, ok := strings.Cut(w, "="); ok {
			r.record(value)
		}
	}
}

// refuse returns err with every recorded text hidden in its message and hint,
// in the forms the filter covers: the text as recorded, each of its lines, its
// JSON-escaped form, its base64 form and its Go-quoted forms (the text between
// the quotes of strconv.Quote, QuoteToASCII and QuoteToGraphic). A form under 4
// characters is not hidden, and a text shown in any other form is not covered.
func (r *readTexts) refuse(err error) error { return engine.FilterError(err, r.filter()) }

// filter returns the function that replaces every recorded form with <hidden>.
func (r *readTexts) filter() func(string) string { return engine.NewTextFilter(r.forms, true) }

// redactionTexts is redactionForms of every value.
func redactionTexts(values []string) []string {
	var out []string
	for _, v := range values {
		out = append(out, redactionForms(v)...)
	}
	return out
}

// redactionForms lists the texts a filter hides for one value: the value, each
// of its lines (cut at a line feed or a carriage return, empty lines dropped)
// and, for the value and for each line, the text between the quotes of
// strconv.Quote, QuoteToASCII and QuoteToGraphic. Both filters of the command
// take their forms from it: the one that hides a value in a refusal of the
// command and the one that hides it in a response of the server.
func redactionForms(value string) []string {
	forms := []string{value}
	texts := []string{value}
	for line := range strings.FieldsFuncSeq(value, func(r rune) bool { return r == '\n' || r == '\r' }) {
		forms = append(forms, line)
		texts = append(texts, line)
	}
	for _, text := range texts {
		for _, quoted := range []string{strconv.Quote(text), strconv.QuoteToASCII(text), strconv.QuoteToGraphic(text)} {
			inner := strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
			if !slices.Contains(forms, inner) {
				forms = append(forms, inner)
			}
		}
	}
	return forms
}

// readWords reads each --from-file value and checks every value against the
// white space rules. warnings names a value the command trimmed. It calls
// record with each form of a value of a secret or multi-line field as soon as
// the form exists, before any check that can refuse the value: the bytes as
// read, the text after the byte order marks are dropped, and the cleaned
// value.
func (c *createCloudCredentialCmd) readWords(d cloudcredfields.Driver, sets, files []fieldWord, record func(texts ...string)) ([]credentialValue, []string, error) {
	var out []credentialValue
	var warnings []string
	for _, w := range sets {
		f, _ := d.Field(w.field)
		hidden := f.Secret || f.Multiline
		if hidden {
			record(w.value)
		}
		cleaned, problem, trimmed := cloudcredfields.Clean(f, w.value, false)
		if hidden {
			record(cleaned)
		}
		if err := valueProblemError(problem, f, "--"+flagSet, ""); err != nil {
			return nil, nil, err
		}
		if trimmed {
			warnings = append(warnings, fmt.Sprintf("the value of %s was trimmed of white space at its ends, as Rancher trims it", f.Name))
		}
		out = append(out, credentialValue{field: f, value: cleaned})
	}
	for _, w := range files {
		f, _ := d.Field(w.field)
		hidden := f.Secret || f.Multiline
		path := w.value
		raw, err := c.src.Read(path)
		if err != nil {
			return nil, nil, clierr.New(clierr.CodeValidation, "cannot read the --from-file file "+path)
		}
		if hidden {
			record(string(raw))
		}
		if cloudcredfields.RawEncodingProblem(raw) != cloudcredfields.ProblemNone {
			return nil, nil, valueProblemError(cloudcredfields.ProblemEncoding, f, "--"+flagFromFile, path)
		}
		text := string(engine.DropLeadingByteOrderMarks(raw))
		if hidden {
			record(text)
		}
		cleaned, problem, trimmed := cloudcredfields.Clean(f, text, true)
		if hidden {
			record(cleaned)
		}
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

// decodesAsKubeconfig reports whether text is a YAML or JSON document with a
// cluster, a user or a context, the shape of a kubeconfig.
func decodesAsKubeconfig(text string) bool {
	var doc struct {
		Clusters []any `json:"clusters"`
		Users    []any `json:"users"`
		Contexts []any `json:"contexts"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return false
	}
	return len(doc.Clusters)+len(doc.Users)+len(doc.Contexts) > 0
}

// checkRequired refuses a missing required field, from the names the command
// line gives, before stdin or any file is read.
func checkRequired(d cloudcredfields.Driver, sets, files []fieldWord) error {
	var missing []string
	for _, f := range d.Fields {
		if f.Required && !slices.ContainsFunc(slices.Concat(sets, files), func(w fieldWord) bool { return w.field == f.Name }) {
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
		shown := any(v.value)
		if v.field.Secret {
			shown = engine.Redacted
		}
		diff = append(diff, safety.DiffEntry{Path: engine.PointerFor([]string{configKey, v.field.Name}), To: shown})
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

// responseFilter returns the function that hides the secret values the request
// carried in a text of the server.
func responseFilter(values []credentialValue) func(string) string {
	return engine.NewTextFilter(redactionTexts(secretTexts(values)), true)
}

// secretTexts lists the values the error filter hides: every secret field's.
func secretTexts(values []credentialValue) []string {
	var out []string
	for _, v := range values {
		if v.field.Secret {
			out = append(out, v.value)
		}
	}
	return out
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

// canCreateCredentialSecrets asks the cluster whether the caller may create
// Secrets in cattle-global-data. A failed request has no response: reviewed is
// false.
func canCreateCredentialSecrets(ctx context.Context, client engine.ClusterClient) (allowed, reviewed bool) {
	gvk, ok := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	if !ok {
		return false, false
	}
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"verb": "create", "group": "", "resource": secretsGVR.Resource, "namespace": cloudCredNamespace,
		}},
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
		return runtime.Result{}, createError(err, filter, displayName)
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

// createError turns an error of the Norman client into the command's error,
// with the values the request carried hidden in every text of the server.
func createError(err error, filter func(string) string, displayName string) error {
	if r, ok := errors.AsType[*engine.ActionRefused](err); ok {
		return engine.RefusedError(http.MethodPost, "cloud credentials", r, filter)
	}
	if u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err); ok {
		said := clierr.FilteredServerMessage(u.Body, filter)
		if tail, ok := strings.CutPrefix(said, harvesterRefusalPrefix); ok {
			return harvesterRefusal(tail)
		}
		cause := "no response arrived"
		if status, ok := clierr.HTTPStatus(u.Err); ok {
			cause = "Rancher responded with " + strconv.Itoa(status)
		}
		if said != "" {
			cause += ": " + said
		}
		return clierr.New(clierr.CodeGeneric, clierr.Text(
			"the request was sent, but the response did not say whether Rancher created the cloud credential ("+cause+")",
			clierr.Lead("; before you try again, check with: "),
			clierr.NewCommand("get cloud-credential").Arg(displayName))).WithRetryable(false)
	}
	return engine.FilterError(err, filter)
}

// harvesterRefusal is the error for a 500 whose message starts with the
// prefix Rancher wraps its Harvester kubeconfig checks in: nothing was
// written. tail is Rancher's message after that prefix, already filtered. The
// parser's own error can quote a fragment of the kubeconfig, so a fixed text
// replaces it.
func harvesterRefusal(tail string) error {
	if strings.Contains(tail, harvesterParserMarker) {
		return clierr.New(clierr.CodeValidation, "Rancher could not read the tokens in the kubeconfig given for kubeconfigContent; check that it is a kubeconfig of the Harvester cluster")
	}
	return clierr.New(clierr.CodeValidation, "Rancher refused the kubeconfig: "+tail)
}

var _ runtime.MutatingCommand = (*createCloudCredentialCmd)(nil)

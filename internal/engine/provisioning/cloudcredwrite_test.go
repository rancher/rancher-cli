package provisioning

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const ccwSecret = "fake-credential-secret-0001"

// ccwInput is the runtime.Input of a create cloud-credential run.
type ccwInput struct {
	args     []string
	set      []string
	fromFile []string
	desc     string
	wait     *bool
	terminal bool
	// display and description are set for a patch: a flag typed with a value,
	// the empty value included.
	display, description *string
	// timeout is --timeout of a delete.
	timeout string
}

func (i ccwInput) String(k string) string {
	switch k {
	case "description":
		if i.description != nil {
			return *i.description
		}
		return i.desc
	case "display-name":
		if i.display != nil {
			return *i.display
		}
	case "timeout":
		return i.timeout
	}
	return ""
}
func (i ccwInput) Bool(k string) bool { return k == "wait" && i.wait != nil && *i.wait }
func (i ccwInput) Strings(k string) []string {
	switch k {
	case "set":
		return i.set
	case "from-file":
		return i.fromFile
	}
	return nil
}
func (i ccwInput) IsSet(k string) bool {
	switch k {
	case "wait":
		return i.wait != nil
	case "display-name":
		return i.display != nil
	case "timeout":
		return i.timeout != ""
	case "description":
		return i.description != nil
	}
	return false
}
func (i ccwInput) Args() []string      { return i.args }
func (i ccwInput) StdinTerminal() bool { return i.terminal }

var _ runtime.StdinInput = ccwInput{}

// ccwFiles is a FileSource over a map; "-" is stdin. A path with no entry is
// unreadable. It counts the reads.
type ccwFiles struct {
	mu    sync.Mutex
	files map[string][]byte
	reads []string
}

func (f *ccwFiles) Read(path string) ([]byte, error) {
	f.mu.Lock()
	f.reads = append(f.reads, path)
	f.mu.Unlock()
	if b, ok := f.files[path]; ok {
		return b, nil
	}
	return nil, errors.New("no such file: OS text that must never be shown")
}

// ccwClient is the cluster the command reads.
type ccwClient struct {
	*fakeClient
	mu         sync.Mutex
	drivers    []*unstructured.Unstructured
	setting    string
	settingErr error
	secrets    *unstructured.UnstructuredList
	secretsErr error
	review     func() (engine.ReviewResponse, error)
	getsLeft   int // reads of the new Secret that respond with not found first
	// getSecret, when set, responds to the read of a Secret by name.
	getSecret func(name string) (*unstructured.Unstructured, error)
	gets      int
	settings  int // reads of the kev2-operators setting
	// clusterLists returns the List of the clusters of an API group; a group
	// with no entry has none. clusterListGroups records every such List.
	clusterLists      map[string]func() (*unstructured.UnstructuredList, error)
	clusterListGroups []string
}

func (c *ccwClient) List(_ context.Context, gvr schema.GroupVersionResource, _ string, _ string) (*unstructured.UnstructuredList, error) {
	if gvr.Resource == "clusters" {
		c.mu.Lock()
		c.clusterListGroups = append(c.clusterListGroups, gvr.Group)
		c.mu.Unlock()
		if read := c.clusterLists[gvr.Group]; read != nil {
			return read()
		}
		return &unstructured.UnstructuredList{}, nil
	}
	if gvr.Resource == "nodedrivers" {
		l := &unstructured.UnstructuredList{}
		for _, d := range c.drivers {
			l.Items = append(l.Items, *d)
		}
		return l, nil
	}
	if c.secretsErr != nil {
		return nil, c.secretsErr
	}
	if c.secrets == nil {
		return &unstructured.UnstructuredList{}, nil
	}
	return c.secrets, nil
}

func (c *ccwClient) Get(_ context.Context, gvr schema.GroupVersionResource, _ string, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource == "settings" {
		c.mu.Lock()
		c.settings++
		c.mu.Unlock()
		if c.settingErr != nil {
			return nil, c.settingErr
		}
		return &unstructured.Unstructured{Object: map[string]any{"value": c.setting}}, nil
	}
	if c.getSecret != nil {
		c.mu.Lock()
		c.gets++
		c.mu.Unlock()
		return c.getSecret(name)
	}
	c.mu.Lock()
	c.gets++
	left := c.getsLeft
	if left > 0 {
		c.getsLeft--
	}
	c.mu.Unlock()
	if left > 0 {
		return nil, clierr.New(clierr.CodeNotFound, "secret not found")
	}
	return secretObj(name, nil, map[string]any{"amazonec2credentialConfig-accessKey": "x"}), nil
}

func (c *ccwClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	if c.review != nil {
		return c.review()
	}
	return reviewAnswer(true), nil
}

func reviewAnswer(allowed bool) engine.ReviewResponse {
	return engine.ReviewResponse{Status: http.StatusCreated, Object: &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"allowed": allowed}}}}
}

func nodeDriver(id, display string, active, add bool, public, private, password, optional, defaults string) *unstructured.Unstructured {
	ann := map[string]any{}
	for k, v := range map[string]string{
		"publicCredentialFields": public, "privateCredentialFields": private, "passwordFields": password,
		"optionalCredentialFields": optional, "defaults": defaults,
	} {
		if v != "" {
			ann[k] = v
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id, "annotations": ann},
		"spec":     map[string]any{"displayName": display, "active": active, "addCloudCredential": add},
	}}
}

func defaultDrivers() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		nodeDriver("amazonec2", "amazonec2", true, false, "accessKey", "secretKey", "", "", ""),
		nodeDriver("digitalocean", "digitalocean", true, false, "", "accessToken", "", "", ""),
		nodeDriver("harvester", "harvester", true, false, "clusterType,clusterId", "kubeconfigContent", "", "clusterId", "clusterType:imported"),
		nodeDriver("linode", "linode", false, false, "", "token", "", "", ""),
		nodeDriver("pnap", "pnap", true, false, "clientIdentifier,clientSecret", "", "clientSecret", "", ""),
		nodeDriver("twosecrets", "twosecrets", true, false, "user", "keyOne,keyTwo", "", "", ""),
	}
}

// ccwNorman is the Norman client of a test.
type ccwNorman struct {
	mu       sync.Mutex
	bodies   []string
	creates  int
	id       string
	err      error
	sentBody engine.CloudCredentialBody
	// updates and deletes record the id of every Update and Delete; updateBodies
	// has the body of each Update.
	updates      []string
	updateBodies []string
	deletes      []string
	updateErr    error
	deleteErr    error
	// onDelete runs after a Delete is recorded, as the server deletes or
	// replaces the Secret.
	onDelete func()
}

func (n *ccwNorman) CloudCredentials() (engine.CloudCredentialClient, error) { return n, nil }

func (n *ccwNorman) Create(_ context.Context, body engine.CloudCredentialBody) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.creates++
	raw, _ := body.RequestJSON()
	n.bodies = append(n.bodies, string(raw))
	n.sentBody = body
	if n.err != nil {
		return "", n.err
	}
	if n.id == "" {
		return "cattle-global-data:cc-abcde", nil
	}
	return n.id, nil
}

func (n *ccwNorman) Update(_ context.Context, id string, body engine.CloudCredentialBody) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	raw, _ := body.RequestJSON()
	n.updates = append(n.updates, id)
	n.updateBodies = append(n.updateBodies, string(raw))
	return n.updateErr
}

func (n *ccwNorman) Delete(_ context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deletes = append(n.deletes, id)
	if n.onDelete != nil {
		n.onDelete()
	}
	return n.deleteErr
}

func (n *ccwNorman) body(t *testing.T) map[string]any {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	require.Len(t, n.bodies, 1)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(n.bodies[0]), &out))
	return out
}

type ccwRig struct {
	client *ccwClient
	norman *ccwNorman
	files  *ccwFiles
	cmd    *createCloudCredentialCmd
}

func newCCWRig(files map[string]string) *ccwRig {
	r := &ccwRig{
		client: &ccwClient{fakeClient: &fakeClient{}, drivers: defaultDrivers(), setting: `[{"name":"alibaba","active":false}]`},
		norman: &ccwNorman{},
		files:  &ccwFiles{files: map[string][]byte{}},
	}
	for p, c := range files {
		r.files.files[p] = []byte(c)
	}
	r.cmd = NewCreateCloudCredential(fakeFactory{c: r.client}, r.norman, r.files).(*createCloudCredentialCmd)
	return r
}

func (r *ccwRig) plan(t *testing.T, in ccwInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return r.cmd.Plan(t.Context(), in)
}

// apply runs the command through the pipeline with --apply and returns the
// decoded stdout and stderr and the error.
func (r *ccwRig) apply(in ccwInput) (stdout, stderr string, ce *clierr.Error) {
	var out, errOut bytes.Buffer
	_, ce = runtime.Run(context.Background(), r.cmd, in, runtime.IO{Out: &out, Err: &errOut, Format: "json", Apply: true}, safety.Binding{})
	return out.String(), errOut.String(), ce
}

func ccwAmazon(extra ...string) ccwInput {
	return ccwInput{
		args:     []string{"amazonec2", "prod"},
		set:      append([]string{"accessKey=AKIAFAKEEXAMPLE"}, extra...),
		fromFile: []string{"secretKey=secret.txt"},
	}
}

func ccwFilesFor(secret string) map[string]string { return map[string]string{"secret.txt": secret} }

// TestCloudCredentialPlanDisplayIdentity asserts the Effect names the kind
// and the display name the preview and the result show.
func TestOrdinal(t *testing.T) {
	for n, want := range map[int]string{
		1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 10: "10th",
		11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd",
		23: "23rd", 101: "101st", 111: "111th", 112: "112th", 113: "113th",
	} {
		assert.Equal(t, want, ordinal(n), "n=%d", n)
	}
}

// TestCloudCredentialPlanDisplayIdentity asserts the Effect of a create and of a
// patch names the kind and the display name the preview and the result show.
func TestCloudCredentialPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret + "\n"))
	eff, _, err := r.plan(t, ccwAmazon())
	require.NoError(t, err)
	assert.Equal(t, "cloud-credential", eff.DisplayKind)
	assert.Equal(t, "prod", eff.DisplayName)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.False(t, eff.ConflictDetection)
	assert.Empty(t, eff.ResourceVersion)
	assert.True(t, eff.Reversible)
	assert.False(t, eff.Destructive)

	// A patch names the same kind and the Secret name, an update of the server
	// reads it came from, with the resourceVersion it read.
	p := newCCPRig(newCCWRig(nil), "amazonec2")
	in := ccpInputOf()
	in.description = new("new")
	peff, _, err := p.plan(t, in)
	require.NoError(t, err)
	assert.Equal(t, "cloud-credential", peff.DisplayKind)
	assert.Equal(t, ccpID, peff.DisplayName)
	assert.Equal(t, safety.ActionUpdate, peff.Action)
	assert.Equal(t, "cloudcredentials", peff.Target)
	assert.Equal(t, safety.PreviewServerReads, peff.PreviewSource)
	assert.Equal(t, "7", peff.ResourceVersion)
	assert.False(t, peff.ConflictDetection)
	assert.False(t, peff.Reversible)
	assert.False(t, peff.Destructive)

	// A delete names the same kind and the Secret name.
	d := newCCDRig(newCCWRig(nil), "amazonec2")
	deff, _, err := d.plan(t, ccpInputOf())
	require.NoError(t, err)
	assert.Equal(t, "cloud-credential", deff.DisplayKind)
	assert.Equal(t, ccpID, deff.DisplayName)
	assert.Equal(t, safety.ActionDelete, deff.Action)
}

// TestCloudCredentialEffectDiffHidesSecrets asserts the diff has the name,
// the driver annotation, the public values as typed and <redacted> for a
// secret field, and the warnings say the preview comes from server reads.
func TestCloudCredentialEffectDiffHidesSecrets(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret + "\n"))
	in := ccwAmazon("defaultRegion=us-west-2")
	in.desc = "ci credential"
	eff, _, err := r.plan(t, in)
	require.NoError(t, err)
	got := map[string]any{}
	for _, d := range eff.Diff {
		got[d.Path] = d.To
	}
	assert.Equal(t, map[string]any{
		"/name":        "prod",
		"/description": "ci credential",
		engine.PointerFor([]string{"annotations", driverAnnotation}): "aws",
		"/amazonec2credentialConfig/accessKey":                       "AKIAFAKEEXAMPLE",
		"/amazonec2credentialConfig/defaultRegion":                   "us-west-2",
		"/amazonec2credentialConfig/secretKey":                       engine.Redacted,
	}, got)
	require.NotEmpty(t, eff.Warnings)
	assert.Contains(t, eff.Warnings[0], "previewed from server reads")
	assert.NotContains(t, fmt.Sprint(eff), ccwSecret)
}

// TestCloudCredentialDriverAnnotation asserts the POST body carries the UI's
// label in the annotation: aws, gcp and oracle for the three renamed drivers,
// the prefix in lower case for every other, a mixed-case display name of a
// driver added by URL included.
func TestCloudCredentialDriverAnnotation(t *testing.T) {
	t.Parallel()
	for prefix, label := range map[string]string{
		"amazonec2": "aws", "google": "gcp", "oci": "oracle", "azure": "azure", "digitalocean": "digitalocean",
		"MyDriver": "mydriver", "s3": "s3",
	} {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(map[string]string{"f": ccwSecret})
			r.client.drivers = append(r.client.drivers,
				nodeDriver("google", "google", true, false, "", "authEncodedJson", "", "", ""),
				nodeDriver("oci", "oci", true, false, "", "privateKeyContents", "", "", ""),
				nodeDriver("azure", "azure", true, false, "", "clientSecret", "", "", ""),
				nodeDriver("mydriver", "MyDriver", true, false, "", "key", "", "", ""))
			in := ccwInput{args: []string{prefix, "n"}}
			switch prefix {
			case "amazonec2":
				in.set, in.fromFile = []string{"accessKey=AKIAFAKE"}, []string{"secretKey=f"}
			case "google":
				in.fromFile = []string{"authEncodedJson=f"}
			case "oci":
				in.fromFile = []string{"privateKeyContents=f"}
			case "azure":
				in.fromFile = []string{"clientSecret=f"}
			case "digitalocean":
				in.fromFile = []string{"accessToken=f"}
			case "MyDriver":
				in.fromFile = []string{"key=f"}
			case "s3":
				in.set, in.fromFile = []string{"accessKey=AKIAFAKE"}, []string{"secretKey=f"}
			}
			_, _, ce := r.apply(in)
			require.Nil(t, ce)
			body := r.norman.body(t)
			assert.Equal(t, label, body["annotations"].(map[string]any)[driverAnnotation])
			assert.Equal(t, "cloudCredential", body["type"])
			assert.Contains(t, body, prefix+"credentialConfig")
		})
	}
}

// TestCloudCredentialPostBody asserts the body has exactly the fields given,
// the description only when given, and the secret value without its final
// line ending.
func TestCloudCredentialPostBody(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret + "\r\n"))
	in := ccwAmazon()
	in.desc = "d"
	_, _, ce := r.apply(in)
	require.Nil(t, ce)
	assert.Equal(t, map[string]any{
		"type": "cloudCredential", "name": "prod", "description": "d",
		"annotations":               map[string]any{driverAnnotation: "aws"},
		"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKEEXAMPLE", "secretKey": ccwSecret},
	}, r.norman.body(t))

	r = newCCWRig(ccwFilesFor(ccwSecret))
	_, _, ce = r.apply(ccwAmazon())
	require.Nil(t, ce)
	assert.NotContains(t, r.norman.body(t), "description")
}

// TestCloudCredentialCommandGates asserts the gates: --apply is the only one,
// a create without it is a preview and sends nothing.
func TestCloudCredentialCommandGates(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret))
	d := r.cmd.Describe()
	assert.Equal(t, "false", d.Destructive)
	var out, errOut bytes.Buffer
	_, ce := runtime.Run(t.Context(), r.cmd, ccwAmazon(), runtime.IO{Out: &out, Err: &errOut, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
	assert.NotContains(t, ce.Hint, "--yes")
	assert.Zero(t, r.norman.creates)
	_, _, ce = r.apply(ccwAmazon())
	assert.Nil(t, ce, "--apply alone is enough: the create is not destructive")
	assert.Equal(t, 1, r.norman.creates)
}

// TestCloudCredentialCommandsTakeSecretInput asserts the command's descriptor
// sets SecretInput, so no typed argument reaches a refusal of the command line.
func TestCloudCredentialCommandsTakeSecretInput(t *testing.T) {
	t.Parallel()
	for _, c := range []runtime.Command{
		NewCreateCloudCredential(fakeFactory{}, &ccwNorman{}, &ccwFiles{}),
		NewPatchCloudCredential(fakeFactory{}, &ccwNorman{}, &ccwFiles{}),
	} {
		d := c.Describe()
		assert.True(t, d.SecretInput, d.Name)
	}
}

// TestCloudCredentialDryRunSendsNoWrite asserts a preview sends no write on
// any path, a refused display name list and a failed access review included.
func TestCloudCredentialDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*ccwRig){
		"plain":                func(*ccwRig) {},
		"display name refused": func(r *ccwRig) { r.client.secretsErr = clierr.New(clierr.CodeRBAC, "forbidden") },
		"review request failed": func(r *ccwRig) {
			r.client.review = func() (engine.ReviewResponse, error) { return engine.ReviewResponse{}, errors.New("down") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(ccwFilesFor(ccwSecret))
			mutate(r)
			var out, errOut bytes.Buffer
			_, ce := runtime.Run(t.Context(), r.cmd, ccwAmazon(), runtime.IO{Out: &out, Err: &errOut, Format: "json", DryRun: true}, safety.Binding{})
			require.Nil(t, ce)
			assert.Zero(t, r.norman.creates)
			if name != "plain" {
				var env struct {
					Warnings []string `json:"warnings"`
				}
				require.NoError(t, json.Unmarshal(out.Bytes(), &env))
				assert.NotEmpty(t, env.Warnings)
			}
		})
	}
}

// TestCloudCredentialAccessReviewDenied asserts a denied review is RBAC and
// names the roles that grant it, with nothing sent.
func TestCloudCredentialAccessReviewDenied(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret))
	r.client.review = func() (engine.ReviewResponse, error) { return reviewAnswer(false), nil }
	_, _, err := r.plan(t, ccwAmazon())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Contains(t, ce.Message, "User")
	assert.Contains(t, ce.Message, "Create Clusters")
	assert.Zero(t, r.norman.creates)
}

// TestCloudCredentialPlanRefusals asserts each refusal of the plan: its code,
// that its text names the field or the flag and never a typed value, and that
// nothing was sent. A refusal of the command line or of the driver reads no
// file and no stdin.
func TestCloudCredentialPlanRefusals(t *testing.T) {
	t.Parallel()
	type row struct {
		name      string
		in        ccwInput
		prep      func(*ccwRig)
		want      string
		readsFile bool
	}
	secretWord := ccwSecret
	for _, tc := range []row{
		{name: "empty display name", in: ccwInput{args: []string{"amazonec2", ""}}, want: "the display name is empty"},
		{name: "a --set argument with no equals sign", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{secretWord}, fromFile: []string{"secretKey=-"}},
			want: "the 1st --set must be <field>=<value>"},
		{name: "a --from-file argument with no equals sign", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{"secretKey=secret.txt", secretWord}},
			want: "the 2nd --from-file must be <field>=<path>"},
		{name: "empty field name", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"=" + secretWord}}, want: "the 1st --set has an empty field name"},
		{name: "empty value", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey="}}, want: "the 1st --set has an empty value"},
		{name: "stdin twice", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{"secretKey=-", "other=-"}}, want: "--from-file - is given more than once"},
		{name: "stdin is a terminal", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{"secretKey=-"}, terminal: true}, want: "stdin, which is a terminal"},
		{name: "a field twice", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a", "accessKey=b"}}, want: "the 1st --set and the 2nd --set give the same field; give each field once"},
		{name: "a field in both flags", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"secretKey=a"}, fromFile: []string{"accessKey=x.txt", "secretKey=secret.txt"}},
			want: "the 1st --set and the 2nd --from-file give the same field; give each field once"},
		{name: "unknown driver", in: ccwInput{args: []string{"nope", "n"}}, want: "unknown cloud credential driver \"nope\"; drivers with a credential on this server: amazonec2, digitalocean, harvester, pnap, s3, twosecrets"},
		{name: "inactive driver", in: ccwInput{args: []string{"linode", "n"}}, want: "node driver \"linode\" is not active"},
		{name: "alibaba while its operator is inactive", in: ccwInput{args: []string{"alibaba", "n"}}, want: "the alibaba operator is not active: the setting kev2-operators marks it inactive"},
		{name: "unknown field", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"nope=1"}}, want: "the name in the 1st --set is not a field of amazonec2; its fields are accessKey, defaultRegion, secretKey"},
		{name: "a public field in another case with --set", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"ACCESSKEY=x"}}, want: "\"ACCESSKEY\" after --set is not a field of amazonec2"},
		{name: "a public field in another case with --from-file", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{"accesskey=secret.txt"}}, want: "\"accesskey\" after --from-file is not a field of amazonec2"},
		{name: "secret field in --set", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"secretKey=" + secretWord}}, want: "\"secretKey\" is a secret field and cannot be given with --set"},
		{name: "secret field in --set in other case", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"SECRETKEY=" + secretWord}}, want: "is the secret field secretKey"},
		{name: "a public field of a driver marked secret by passwordFields", in: ccwInput{args: []string{"pnap", "n"}, set: []string{"clientSecret=" + secretWord}}, want: "is a secret field"},
		{name: "public field in --from-file", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{"accessKey=secret.txt"}}, want: "\"accessKey\" is a public field of amazonec2"},
		{name: "missing required field with stdin that fails", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"defaultRegion=us-east-1"}, fromFile: []string{"secretKey=-"}},
			want: "amazonec2 needs these fields: accessKey (--set accessKey=<value>)"},
		{name: "a pasted secret that has equals signs as a --set argument", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{secretWord + "=="}},
			want: "the name in the 1st --set is not a field of amazonec2"},
		{name: "a pasted secret that has equals signs as a --from-file argument", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{secretWord + "=="}},
			want: "the name in the 1st --from-file is not a field of amazonec2"},
		{name: "a pasted secret that has equals signs given twice", in: ccwInput{args: []string{"amazonec2", "n"}, fromFile: []string{secretWord + "==", secretWord + "=="}},
			want: "the 1st --from-file and the 2nd --from-file give the same field; give each field once"},
		{name: "missing required field", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a"}}, want: "amazonec2 needs these fields: secretKey (--from-file secretKey=<path>)", readsFile: false},
		{name: "unreadable file", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a"}, fromFile: []string{"secretKey=missing.txt"}},
			want: "cannot read the --from-file file missing.txt", readsFile: true},
		{name: "a line break in a single-line value", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a"}, fromFile: []string{"secretKey=two.txt"}},
			prep: func(r *ccwRig) { r.files.files["two.txt"] = []byte("one\ntwo\n") }, want: "the value of secretKey holds a line break", readsFile: true},
		{name: "white space at the end of a value", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a "}, fromFile: []string{"secretKey=secret.txt"}},
			want: "Rancher removes leading and trailing white space from accessKey; remove it from the value"},
		{name: "a UTF-16 file", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a"}, fromFile: []string{"secretKey=utf16.txt"}},
			prep: func(r *ccwRig) { r.files.files["utf16.txt"] = []byte{0xFF, 0xFE, 'a', 0} }, want: "the value of secretKey read from utf16.txt is not UTF-8 text", readsFile: true},
		{name: "UTF-16 text on stdin", in: ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a"}, fromFile: []string{"secretKey=-"}},
			prep: func(r *ccwRig) { r.files.files["-"] = []byte{0xFF, 0xFE, 'a', 0} }, want: "the value of secretKey read from stdin is not UTF-8 text", readsFile: true},
		{name: "a harvester file that is not a kubeconfig", in: ccwInput{args: []string{"harvester", "n"}, fromFile: []string{"kubeconfigContent=secret.txt"}},
			want: "the --from-file file secret.txt for kubeconfigContent is not a kubeconfig", readsFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(ccwFilesFor(ccwSecret + "\n"))
			if tc.prep != nil {
				tc.prep(r)
			}
			_, _, err := r.plan(t, tc.in)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code, ce.Message)
			assert.Contains(t, ce.Message, tc.want)
			assert.NotContains(t, ce.Message, secretWord)
			assert.NotContains(t, ce.Message, "OS text")
			assert.Zero(t, r.norman.creates)
			if !tc.readsFile {
				assert.Empty(t, r.files.reads, "a refusal before the values are read reads no file and no stdin")
			}
		})
	}
}

// TestCloudCredentialDisplayNameInUse asserts a display name another
// credential has is refused with its id, and that a refused List skips the
// check with a warning.
func TestCloudCredentialDisplayNameInUse(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret))
	r.client.secrets = listOf(secretObj("cc-other", map[string]string{"field.cattle.io/name": "prod"}, map[string]any{"amazonec2credentialConfig-accessKey": "x"}))
	_, _, err := r.plan(t, ccwAmazon())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "cc-other")
	assert.Zero(t, r.norman.creates)

	r = newCCWRig(ccwFilesFor(ccwSecret))
	r.client.secretsErr = clierr.New(clierr.CodeRBAC, "forbidden")
	eff, _, err := r.plan(t, ccwAmazon())
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(eff.Warnings, func(w string) bool {
		return strings.Contains(w, "was not checked against the existing cloud credentials")
	}))
}

// TestCloudCredentialValueWhiteSpace asserts the values a create reads: a file
// value loses one final line ending and byte order marks at its start, a
// multi-line field keeps its interior CRLF and is trimmed at both ends with a
// warning, and a value with white space left is refused.
func TestCloudCredentialValueWhiteSpace(t *testing.T) {
	t.Parallel()
	kubeconfig := "apiVersion: v1\r\nclusters:\r\n- name: c\r\nusers:\r\n- name: u\r\n  user:\r\n    token: kubeconfig-user-x:abc\r\n"
	for name, tc := range map[string]struct {
		in      ccwInput
		file    string
		field   string
		config  string
		want    string
		warning bool
	}{
		"one mark":                    {ccwAmazon(), "\xef\xbb\xbf" + ccwSecret + "\r\n", "secretKey", "amazonec2credentialConfig", ccwSecret, false},
		"two marks":                   {ccwAmazon(), "\xef\xbb\xbf\xef\xbb\xbf" + ccwSecret + "\n", "secretKey", "amazonec2credentialConfig", ccwSecret, false},
		"a mark after one byte stays": {ccwAmazon(), "a\xef\xbb\xbf" + ccwSecret, "secretKey", "amazonec2credentialConfig", "a\xef\xbb\xbf" + ccwSecret, false},
		"multi-line kubeconfig": {ccwInput{args: []string{"harvester", "h"}, fromFile: []string{"kubeconfigContent=secret.txt"}}, kubeconfig,
			"kubeconfigContent", "harvestercredentialConfig", strings.TrimSpace(kubeconfig), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(map[string]string{"secret.txt": tc.file})
			_, _, ce := r.apply(tc.in)
			require.Nil(t, ce)
			got := r.norman.body(t)[tc.config].(map[string]any)[tc.field]
			assert.Equal(t, tc.want, got)
			eff, _, err := newCCWRig(map[string]string{"secret.txt": tc.file}).plan(t, tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.warning, slices.ContainsFunc(eff.Warnings, func(w string) bool { return strings.Contains(w, "was trimmed") }))
		})
	}
}

// TestCloudCredentialSetValueWhiteSpace asserts a --set value with white space
// at either end, of ASCII, U+00A0 or U+0085, or with a line break, is refused
// before any request and never trimmed, because Rancher stores the value
// trimmed and the preview would show something else.
func TestCloudCredentialSetValueWhiteSpace(t *testing.T) {
	t.Parallel()
	edge := "Rancher removes leading and trailing white space from accessKey; remove it from the value"
	lineBreak := "the value of accessKey holds a line break"
	for _, tc := range []struct{ name, value, want string }{
		{"a trailing space", "a ", edge},
		{"a leading space", " a", edge},
		{"a trailing tab", "a\t", edge},
		{"a trailing no-break space", "a\u00a0", edge},
		{"a leading no-break space", "\u00a0a", edge},
		{"a trailing next line", "a\u0085", edge},
		{"a leading next line", "\u0085a", edge},
		{"a line break inside", "a\nb", lineBreak},
		{"a carriage return inside", "a\rb", lineBreak},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(ccwFilesFor(ccwSecret))
			_, _, err := r.plan(t, ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=" + tc.value}, fromFile: []string{"secretKey=secret.txt"}})
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code, ce.Message)
			assert.Contains(t, ce.Message, tc.want)
			assert.Zero(t, r.norman.creates)
		})
	}
}

// TestCloudCredentialReadsTheOperatorSettingOnlyWhenNeeded asserts the
// kev2-operators setting is read for alibaba and for the list an unknown
// driver names, and for no other driver. A failed read fails an alibaba create,
// leaves alibaba out of an unknown driver's list, and fails no other create.
func TestCloudCredentialReadsTheOperatorSettingOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	amazon := ccwInput{args: []string{"amazonec2", "n"}, set: []string{"accessKey=a"}, fromFile: []string{"secretKey=secret.txt"}}
	for _, tc := range []struct {
		name   string
		in     ccwInput
		reads  int
		failed bool
		want   string
	}{
		{"an ordinary driver", amazon, 0, true, ""},
		{"s3", ccwInput{args: []string{"s3", "n"}, set: []string{"accessKey=a"}, fromFile: []string{"secretKey=secret.txt"}}, 0, true, ""},
		{"alibaba", ccwInput{args: []string{"alibaba", "n"}, fromFile: []string{"accessKeySecret=secret.txt"}}, 1, false, ""},
		{"alibaba and a failed read", ccwInput{args: []string{"alibaba", "n"}, fromFile: []string{"accessKeySecret=secret.txt"}}, 1, true, "the setting cannot be read"},
		{"an unknown driver", ccwInput{args: []string{"nope", "n"}}, 1, false, "unknown cloud credential driver \"nope\"; drivers with a credential on this server: alibaba, amazonec2"},
		{"an unknown driver and a failed read", ccwInput{args: []string{"nope", "n"}}, 1, true, "unknown cloud credential driver \"nope\"; drivers with a credential on this server: amazonec2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(ccwFilesFor(ccwSecret))
			r.client.setting = `[{"name":"alibaba","active":true}]`
			if tc.failed {
				r.client.settingErr = clierr.New(clierr.CodeGeneric, "the setting cannot be read")
			}
			_, _, err := r.plan(t, tc.in)
			assert.Equal(t, tc.reads, r.client.settings)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestCloudCredentialResponseClasses asserts the code, the retry advice and the
// text of each response the create can get, with no value in any text.
func TestCloudCredentialResponseClasses(t *testing.T) {
	t.Parallel()
	request := &clierr.Request{Server: "https://rancher.test"}
	refused := func(status int, body string) error {
		return &engine.ActionRefused{Err: clierr.FromResponse("POST x on the Rancher server", http.MethodPost, status, nil, request), Body: []byte(body), Request: request}
	}
	unknown := func(status int, body string) error {
		return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, status, nil), Body: []byte(body)}
	}
	echo := `{"type":"error","code":"X","message":"bad value ` + ccwSecret + ` here"}`
	harvester := func(tail string) string {
		return `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: ` + tail + `"}`
	}
	for _, tc := range []struct {
		name      string
		err       error
		code      string
		retryable bool
		contains  string
	}{
		{"401", refused(http.StatusUnauthorized, echo), clierr.CodeAuth, false, ""},
		{"403", refused(http.StatusForbidden, echo), clierr.CodeRBAC, false, "bad value <hidden> here"},
		{"409", refused(http.StatusConflict, echo), clierr.CodeConflict, false, "bad value <hidden> here"},
		{"422", refused(http.StatusUnprocessableEntity, echo), clierr.CodeValidation, false, "bad value <hidden> here"},
		{"harvester parser error", unknown(http.StatusInternalServerError, harvester("failed to get tokens from kubeconfig: yaml: line 3: "+ccwSecret)), clierr.CodeValidation, false, "Rancher could not read the tokens"},
		{"harvester expired token", unknown(http.StatusInternalServerError, harvester("token kubeconfig-user-x is expired")), clierr.CodeValidation, false, "token kubeconfig-user-x is expired"},
		{"other 500", unknown(http.StatusInternalServerError, echo), clierr.CodeGeneric, false, "check with"},
		{"502", unknown(http.StatusBadGateway, ""), clierr.CodeGeneric, false, "check with"},
		{"504", unknown(http.StatusGatewayTimeout, ""), clierr.CodeGeneric, false, "check with"},
		{"a lost response", &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric, "the connection failed")}, clierr.CodeGeneric, false, "check with"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCWRig(ccwFilesFor(ccwSecret))
			r.norman.err = tc.err
			_, stderr, ce := r.apply(ccwAmazon())
			require.NotNil(t, ce)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Contains(t, ce.Message, tc.contains)
			assert.NotContains(t, ce.Message+ce.Hint+stderr, ccwSecret)
			assert.NotContains(t, ce.Message+ce.Hint+stderr, base64.StdEncoding.EncodeToString([]byte(ccwSecret)))
		})
	}
}

// TestCloudCredentialCreateWait asserts the create waits until the Secret is
// readable, ends with a warning and exit 0 past the timeout, and sends no read
// with --wait=false.
func TestCloudCredentialCreateWait(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	t.Run("waits for the Secret", func(t *testing.T) {
		t.Parallel()
		r := newCCWRig(ccwFilesFor(ccwSecret))
		r.client.getsLeft = 3
		in := ccwAmazon()
		in.wait = &yes
		out, _, ce := r.apply(in)
		require.Nil(t, ce)
		assert.Equal(t, 4, r.client.gets)
		assert.NotContains(t, out, "cannot read it yet")
	})
	t.Run("the default is to wait", func(t *testing.T) {
		t.Parallel()
		r := newCCWRig(ccwFilesFor(ccwSecret))
		_, _, ce := r.apply(ccwAmazon())
		require.Nil(t, ce)
		assert.Equal(t, 1, r.client.gets)
	})
	t.Run("a timeout is a warning", func(t *testing.T) {
		t.Parallel()
		r := newCCWRig(ccwFilesFor(ccwSecret))
		r.client.getsLeft = 1 << 20
		var out, errOut bytes.Buffer
		_, ce := runtime.Run(t.Context(), r.cmd, ccwAmazon(), runtime.IO{Out: &out, Err: &errOut, Format: "json", Apply: true}, safety.Binding{})
		require.Nil(t, ce)
		var env struct {
			Warnings []string `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &env))
		assert.True(t, slices.ContainsFunc(env.Warnings, func(w string) bool { return strings.Contains(w, "you cannot read it yet") }), env.Warnings)
	})
	t.Run("--wait=false sends no read", func(t *testing.T) {
		t.Parallel()
		r := newCCWRig(ccwFilesFor(ccwSecret))
		in := ccwAmazon()
		in.wait = &no
		_, _, ce := r.apply(in)
		require.Nil(t, ce)
		assert.Zero(t, r.client.gets)
	})
}

// TestCloudCredentialResultRow asserts the result is the row of
// get cloud-credential: id, secretName, displayName and driver.
func TestCloudCredentialResultRow(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret))
	out, _, ce := r.apply(ccwAmazon())
	require.Nil(t, ce)
	var env struct {
		Data []CloudCredRef `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.Equal(t, []CloudCredRef{{ID: "cattle-global-data:cc-abcde", SecretName: "cc-abcde", DisplayName: "prod", Driver: "amazonec2"}}, env.Data)
	assert.NotContains(t, out, ccwSecret)
}

// TestCloudCredentialAlibabaNeedsItsOperator asserts alibaba is accepted once
// the kev2-operators setting marks its operator active, and its fields are
// optional.
func TestCloudCredentialAlibabaNeedsItsOperator(t *testing.T) {
	t.Parallel()
	r := newCCWRig(ccwFilesFor(ccwSecret))
	r.client.setting = `[{"name":"alibaba","active":true}]`
	_, _, ce := r.apply(ccwInput{args: []string{"alibaba", "a"}, fromFile: []string{"accessKeySecret=secret.txt"}})
	require.Nil(t, ce)
	assert.Equal(t, map[string]any{"accessKeySecret": ccwSecret}, r.norman.body(t)["alibabacredentialConfig"])
}

// TestCloudCredentialCompletionOffersOnlyWhatPlanAccepts asserts every driver
// and every field name the completion source offers for the node drivers of the
// fixture passes the command's own checks: Plan never refuses an offered driver
// as unknown or inactive, nor an offered field as unknown, secret in --set or
// public in --from-file; the only refusal left is a missing required field.
func TestCloudCredentialCompletionOffersOnlyWhatPlanAccepts(t *testing.T) {
	t.Parallel()
	items := make([]string, 0, len(defaultDrivers()))
	for _, d := range defaultDrivers() {
		raw, err := json.Marshal(d.Object)
		require.NoError(t, err)
		items = append(items, string(raw))
	}
	server := completiontest.New(t, map[string]string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers":             `{"items":[` + strings.Join(items, ",") + `]}`,
		"/k8s/clusters/local/apis/management.cattle.io/v3/settings/kev2-operators": `{"value":"[{\"name\":\"alibaba\",\"active\":false}]"}`,
	})
	src := complete.CreateCloudCredentialSource()
	refusedWords := []string{"unknown cloud credential driver", "is not active", "has no annotations that name credential fields", "is not a field of", "cannot be given with --set", "is a public field of"}
	check := func(t *testing.T, in ccwInput) {
		t.Helper()
		r := newCCWRig(ccwFilesFor(ccwSecret))
		_, _, err := r.plan(t, in)
		if err == nil {
			return
		}
		for _, word := range refusedWords {
			assert.NotContains(t, err.Error(), word)
		}
	}

	drivers := src.Complete(t.Context(), server.Conn(2), completion.Request{Index: 0})
	require.NotEmpty(t, drivers.Names)
	for _, driver := range drivers.Names {
		check(t, ccwInput{args: []string{driver, "prod"}})
		for _, flag := range []string{"set", "from-file"} {
			fields := src.Complete(t.Context(), server.Conn(1), completion.Request{Flag: flag, Positionals: []string{driver}})
			for _, field := range fields.Names {
				in := ccwInput{args: []string{driver, "prod"}}
				if flag == "set" {
					in.set = []string{field + "=value"}
				} else {
					in.fromFile = []string{field + "=secret.txt"}
				}
				check(t, in)
			}
		}
	}
}

// TestPatchCloudCredentialCompletionOffersOnlyWhatPlanAccepts asserts every
// field name the patch completion source offers for a stored credential of each
// driver of the fixture passes the command's own checks: Plan never refuses an
// offered field as unknown, secret in --set or public in --from-file, and the
// credential value the source resolved is the one Plan resolves.
func TestPatchCloudCredentialCompletionOffersOnlyWhatPlanAccepts(t *testing.T) {
	t.Parallel()
	items := make([]string, 0, len(defaultDrivers()))
	for _, d := range defaultDrivers() {
		raw, err := json.Marshal(d.Object)
		require.NoError(t, err)
		items = append(items, string(raw))
	}
	src := complete.PatchCloudCredentialSource()
	refusedWords := []string{"is not a field of", "cannot be given with --set", "is a public field of", "has no fields of a node driver"}
	offered := 0
	for _, driver := range []string{"amazonec2", "digitalocean", "harvester", "pnap", "twosecrets", "s3"} {
		stored, err := json.Marshal(ccpStored(driver, "7").Object)
		require.NoError(t, err)
		server := completiontest.New(t, map[string]string{
			"/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets":         `{"items":[` + string(stored) + `]}`,
			"/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers":             `{"items":[` + strings.Join(items, ",") + `]}`,
			"/k8s/clusters/local/apis/management.cattle.io/v3/settings/kev2-operators": `{"value":"[{\"name\":\"alibaba\",\"active\":false}]"}`,
		})
		for _, value := range []string{ccpID, "prod"} {
			for _, flag := range []string{"set", "from-file"} {
				fields := src.Complete(t.Context(), server.Conn(2), completion.Request{Flag: flag, Positionals: []string{value}})
				offered += len(fields.Names)
				for _, field := range fields.Names {
					r := newCCPRig(newCCWRig(ccwFilesFor(ccwSecret)), driver)
					in := ccwInput{args: []string{ccpID}}
					if flag == "set" {
						in.set = []string{field + "=value"}
					} else {
						in.fromFile = []string{field + "=secret.txt"}
					}
					_, _, err := r.plan(t, in)
					if err == nil {
						continue
					}
					for _, word := range refusedWords {
						assert.NotContains(t, err.Error(), word, "%s %s %s=...", driver, flag, field)
					}
				}
			}
		}
	}
	require.Positive(t, offered)
}
